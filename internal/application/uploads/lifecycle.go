package uploads

import (
	"context"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

// StartupScan 启动恢复（design D4 分类表）：扫描根目录重建可投递索引（有效配对），
// 并按启动重扫规则清理（启动时无活跃上传，遗留 .partial 按孤儿规则）。
// 扫描失败返回错误（组合根记日志，不阻断启动）。
func (o *Orchestrator) StartupScan(ctx context.Context) error {
	entries, err := o.store.Scan()
	if err != nil {
		return err
	}
	o.replaceIndex(entries)
	o.cleanupEntries(ctx, entries)
	return nil
}

// CleanupOnce 一轮周期清理（每小时，design D4）：
//   - 有效配对：仅 TTL 启用时按 expiresAt 删除（now>=expiresAt）；
//   - 孤儿（无 sidecar 文件/单边 sidecar/损坏 sidecar 及对应文件/遗留 .tmp）：modtime>1h 删除；
//   - .partial：不属于活跃上传且 modtime>1h（非活跃停滞）删除；
//   - sidecar 读取失败：不删，记日志下轮重试。
//
// TTL 配置以清理执行时当前值为准；删除失败记日志下轮重试。
func (o *Orchestrator) CleanupOnce(ctx context.Context) error {
	entries, err := o.store.Scan()
	if err != nil {
		return err
	}
	o.cleanupEntries(ctx, entries)
	return nil
}

// replaceIndex 以扫描结果整体重建可投递索引（启动恢复）。
func (o *Orchestrator) replaceIndex(entries []Entry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.index = make(map[indexKey]Meta)
	for _, e := range entries {
		if e.Kind != EntryValidUpload || e.Meta == nil {
			continue
		}
		o.index[indexKey{taskID: e.TaskID, uploadID: e.Meta.UploadID}] = *e.Meta
	}
}

// purgeIndex 删除某任务（或任务+uploadID）的索引项。
func (o *Orchestrator) purgeIndex(taskID, uploadID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if uploadID == "" {
		for k := range o.index {
			if k.taskID == taskID {
				delete(o.index, k)
			}
		}
		return
	}
	delete(o.index, indexKey{taskID: taskID, uploadID: uploadID})
}

// cleanupEntries 按 task 分组执行清理决策。锁外扫描结果仅用于枚举候选任务：
// 等锁期间条目可能被投递续期/finalize 改写，故取得 per-task 协调锁后重新扫描、
// 分类、判断，再执行删除（消除「锁外快照旧元数据误删已续期文件」竞争）。
// 每组删除动作在协调锁内（与上传提交/投递/生命周期提交串行化）。
func (o *Orchestrator) cleanupEntries(ctx context.Context, scanned []Entry) {
	var order []string
	seen := make(map[string]bool)
	for _, e := range scanned {
		if !seen[e.TaskID] {
			seen[e.TaskID] = true
			order = append(order, e.TaskID)
		}
	}
	for _, taskID := range order {
		release, err := o.coord.Acquire(ctx, taskID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.logf("uploads: cleanup acquire coordination lock (task %s): %v", taskID, err)
			continue
		}
		o.cleanupTaskLocked(ctx, taskID)
		release()
	}
}

// cleanupTaskLocked 单任务清理（调用方已持该任务协调锁）：任务存在性兜底回收 →
// 锁内重扫分类 → 按最新元数据执行删除。
func (o *Orchestrator) cleanupTaskLocked(ctx context.Context, taskID string) {
	// R5 兜底：task.deleted 事件漏收/目录删除失败/进程崩溃会使目录残留；确认任务
	// 已不存在才整目录回收（挂起/非 active 不回收；查询失败记日志跳过，不得当
	// 不存在误删）。删除失败下轮重试。
	if o.tasks != nil {
		exists, _, err := o.tasks.TaskActive(ctx, taskID)
		switch {
		case err != nil:
			o.logf("uploads: cleanup task existence query %s: %v (skipped this round)", taskID, err)
			return
		case !exists:
			if err := o.store.RemoveTaskDir(taskID); err != nil {
				o.logf("uploads: reclaim upload dir for deleted task %s: %v", taskID, err)
				return
			}
			o.purgeIndex(taskID, "")
			return
		}
	}
	// 锁内重扫：拿最新 sidecar 元数据（lastDeliveredAt 可能已被投递续期）。
	fresh, err := o.store.Scan()
	if err != nil {
		o.logf("uploads: cleanup rescan: %v", err)
		return
	}
	now := o.clock()
	for _, e := range fresh {
		if e.TaskID != taskID {
			continue
		}
		o.cleanupEntry(ctx, e, now)
	}
}

// cleanupEntry 单条目清理决策（调用方已持该任务协调锁）。
func (o *Orchestrator) cleanupEntry(ctx context.Context, e Entry, now time.Time) {
	switch e.Kind {
	case EntryReadFailed:
		// 不归孤儿、不删，下轮重试。
		o.logf("uploads: sidecar unreadable %s/%s; skipping (retry next cycle)", e.TaskID, e.Filename)
	case EntryValidUpload:
		if e.Meta == nil || !o.expired(*e.Meta, now) {
			return
		}
		if err := o.store.RemoveUpload(e.TaskID, e.Meta.Filename); err != nil {
			o.logf("uploads: remove expired upload %s/%s: %v", e.TaskID, e.Meta.Filename, err)
			return
		}
		o.purgeIndex(e.TaskID, e.Meta.UploadID)
	case EntryStalePartial:
		// 删 .partial 前确认不属于活跃上传（活跃停滞由取消收尾负责）。
		if id := uploadIDFromPartialName(e.Filename); id != "" && o.lookupActive(id) != nil {
			return
		}
		if e.ModTime.IsZero() || now.Sub(e.ModTime) <= orphanAge {
			return // 时间未知（Info 失败）或未超龄：下轮重试
		}
		if err := o.store.RemovePartial(e.TaskID, e.Filename); err != nil {
			o.logf("uploads: remove stale partial %s/%s: %v", e.TaskID, e.Filename, err)
		}
	case EntryOrphanCorrupt:
		// 损坏 sidecar 及其对应文件成组删除（「对应文件」固定为扫描位置同名配对文件）。
		if e.ModTime.IsZero() || now.Sub(e.ModTime) <= orphanAge {
			return
		}
		if err := o.store.RemoveUpload(e.TaskID, e.Filename); err != nil {
			o.logf("uploads: remove corrupt sidecar pair %s/%s: %v", e.TaskID, e.Filename, err)
		}
	default: // EntryOrphanFile / EntryOrphanSidecar / EntryOrphanTmp
		if e.ModTime.IsZero() || now.Sub(e.ModTime) <= orphanAge {
			return
		}
		if err := o.store.RemovePath(e.TaskID, e.Filename); err != nil {
			o.logf("uploads: remove orphan %s/%s (%s): %v", e.TaskID, e.Filename, e.Kind, err)
		}
	}
}

// uploadIDFromPartialName 从受管 .partial 名提取 uploadID。仅当前 32 字符为
// 32hex（受管命名规则，IsUploadID 同一判定）才视为活跃 upload ID；非受管形态
//（如任意同名 32+ 字符文件）返回空，不得误判为活跃上传。
func uploadIDFromPartialName(name string) string {
	if len(name) < 32 {
		return ""
	}
	prefix := name[:32]
	if !IsUploadID(prefix) {
		return ""
	}
	return prefix
}

// Start 启动后台消费：task.deleted 事件回收（仅触发回收，不承担串行化）与周期清理。
// ctx 取消停止周期清理；Stop 关闭订阅并等待 goroutine 退出。
func (o *Orchestrator) Start(ctx context.Context) {
	if o.events != nil {
		sub := o.events.Subscribe(ocdeckevent.TopicTask)
		o.mu.Lock()
		o.sub = sub
		o.mu.Unlock()
		o.wg.Add(1)
		go o.runEvents(ctx, sub)
	}
	o.wg.Add(1)
	go o.runCleanup(ctx)
}

// runEvents 消费 task 事件：TypeTaskDeleted → 持协调锁整目录回收并清索引。
// 其余事件忽略；订阅溢出信号仅消费（下次溢出重新置位）。
func (o *Orchestrator) runEvents(ctx context.Context, sub EventSubscription) {
	defer o.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Overflow():
			// 非阻塞通道：溢出信号消费即复位（task.deleted 漏收由启动重扫兜底）。
		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			if ev.Type != ocdeckevent.TypeTaskDeleted {
				continue
			}
			o.reclaimTaskDir(ctx, ev.RID)
		}
	}
}

// reclaimTaskDir task.deleted 回收：持协调锁确认后删除上传目录并清索引。
//
// 发布点不变量（钉死）：task.deleted 仅在 DB 删除提交成功后发布
//（application/task/delete_reconcile.go:44 —— DeleteTask 在 store 删除成功且
// res.Affected>0 才发布），即「收到事件 ⇒ DB 已删除」。防御乱序/重复/伪事件，
// 回收前经 TaskActive 复查：查询失败跳过（残留由周期清理兜底重试，不按查询失败
// 误判）；任务仍存在（伪事件）同样跳过。删除失败记日志，下轮清理重试。
func (o *Orchestrator) reclaimTaskDir(ctx context.Context, taskID string) {
	release, err := o.coord.Acquire(ctx, taskID)
	if err != nil {
		o.logf("uploads: reclaim acquire coordination lock (task %s): %v", taskID, err)
		return
	}
	defer release()
	if o.tasks != nil {
		exists, _, err := o.tasks.TaskActive(ctx, taskID)
		if err != nil {
			o.logf("uploads: reclaim task existence query %s: %v (skipped; periodic cleanup will retry)", taskID, err)
			return
		}
		if exists {
			o.logf("uploads: reclaim skipped, task %s still exists (unexpected task.deleted)", taskID)
			return
		}
	}
	if err := o.store.RemoveTaskDir(taskID); err != nil {
		o.logf("uploads: reclaim upload dir for deleted task %s: %v", taskID, err)
	}
	o.purgeIndex(taskID, "")
}

// runCleanup 周期清理循环（ctx 取消或 Stop 关闭 stopCh 退出）。
func (o *Orchestrator) runCleanup(ctx context.Context) {
	defer o.wg.Done()
	ticker := time.NewTicker(o.cleanupIv)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stopCh:
			return
		case <-ticker.C:
			if err := o.CleanupOnce(ctx); err != nil {
				o.logf("uploads: cleanup cycle: %v", err)
			}
		}
	}
}

// Stop 关闭订阅与后台 goroutine 并等待退出（幂等；可在 Start 前调用）。
func (o *Orchestrator) Stop() {
	o.stopOnce.Do(func() {
		close(o.stopCh)
		o.mu.Lock()
		sub := o.sub
		o.mu.Unlock()
		if sub != nil {
			sub.Close()
		}
		o.wg.Wait()
	})
}
