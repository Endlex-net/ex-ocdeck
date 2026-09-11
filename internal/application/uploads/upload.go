package uploads

import (
	"context"
	"io"
	"sync"
	"time"
)

// ActiveUpload 一次活跃上传的登记（内存态；lastProgressAt 不落盘）。
// 生命周期：RegisterActiveUpload 登记（handler 通过认证/Origin/任务准入后、开始读
// 请求体时；此时 connId 尚未解析，允许空串）→ BindConnID（首 part 解析出 connId 后、
// 阶段③准入前绑定）→ TouchProgress/WriteUploadBody（非零进度刷新停滞计时器）→
// FinalizeUpload（提交发布）或 AbortUpload（异常收尾/取消收尾）。停滞超时由编排器
// 直接触发取消收尾，不等周期扫描。
type ActiveUpload struct {
	ID     string
	TaskID string

	// stallTimeout 停滞阈值（构造时从编排器拷贝，进度重置计时器用）。
	stallTimeout time.Duration
	timer        *time.Timer

	mu             sync.Mutex
	connID         string    // 绑定的当前 TUI 连接 ID（注册时可为空，首 part 解析后绑定）；未绑定为空
	filename       string    // 绑定后的受管落盘名（首 part 解析后）；未绑定为空
	canceled       bool      // 停滞取消已触发（不可提交标记）
	lastProgressAt time.Time // 最近非零请求体进度时刻（登记起点起算；停滞复核用）
	cancelCh       chan struct{}
	done           chan struct{}
	doneOnce       sync.Once
	committed      bool // FinalizeUpload 已成功提交（Abort 不再删文件）
}

// --- 上传初始准入导出口（design D1 分阶段：任务准入先于请求体解析、
// connId 准入先于文件落盘；最终裁决仍以 FinalizeUpload 协调锁内复查为准）---

// AdmitUpload 上传初始准入的任务状态判定（design D1 阶段①，先于请求体解析）：
// 任务不存在 → ErrUploadTaskNotFound（404 not_found）；非 active →
// ErrUploadTaskInactive（409 invalid_state）；任务查询 I/O 故障 → errTaskQuery
//（500 internal，不归业务码）。
func (o *Orchestrator) AdmitUpload(ctx context.Context, taskID string) error {
	return o.checkTaskActive(ctx, taskID)
}

// AdmitUploadConn 上传初始准入的 connId 归属判定（design D1 阶段③，先于文件落盘）：
// connId 非该任务当前 TUI 连接（含无当前 TUI 连接）→ ErrConnNotCurrent（403
// forbidden）。Conns 端口未注入（nil）时跳过判定（与 FinalizeUpload 既有跳过语义一致）。
func (o *Orchestrator) AdmitUploadConn(ctx context.Context, taskID, connID string) error {
	if o.conns == nil {
		return nil
	}
	current, found, err := o.conns.CurrentTUIConn(ctx, taskID)
	if err != nil {
		return err
	}
	if !found || current != connID {
		return ErrConnNotCurrent
	}
	return nil
}

// RegisterActiveUpload 登记活跃上传：预留 uploadID 并启动停滞计时器。
// connID 允许为空串（认证/任务准入后、开始读请求体时登记，此时 connId 尚未解析，
// 待首 part 解析后经 BindConnID 绑定）。
func (o *Orchestrator) RegisterActiveUpload(taskID, connID string) (*ActiveUpload, error) {
	id, err := o.store.NewUploadID()
	if err != nil {
		return nil, err
	}
	u := &ActiveUpload{
		ID:             id,
		TaskID:         taskID,
		connID:         connID,
		stallTimeout:   o.stallTimeout,
		lastProgressAt: time.Now(), // 登记起点即首个进度基准（停滞计时器同步启动）
		cancelCh:       make(chan struct{}),
		done:           make(chan struct{}),
	}
	u.timer = time.AfterFunc(o.stallTimeout, func() { o.stallCancel(u) })
	o.mu.Lock()
	o.active[u.ID] = u
	o.mu.Unlock()
	return u, nil
}

// BindConnID 绑定当前 TUI 连接 ID（首 part 解析出 connId 后、阶段③准入前调用）。
// 幂等：仅允许从空值设置一次；已绑定同值返回 nil，已绑定不同值返回 ErrConnAlreadyBound。
func (o *Orchestrator) BindConnID(u *ActiveUpload, connID string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.connID == connID {
		return nil
	}
	if u.connID != "" {
		return ErrConnAlreadyBound
	}
	u.connID = connID
	return nil
}

// ConnID 返回绑定的当前 TUI 连接 ID（未绑定为空）。
func (u *ActiveUpload) ConnID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.connID
}

// BindOriginalName 绑定原始文件名并计算受管落盘名（首 part 解析后调用；
// 客户端原始名仅扩展名提示参与命名，不进入落盘路径）。
func (o *Orchestrator) BindOriginalName(u *ActiveUpload, originalName string) string {
	name := o.store.ManagedName(u.ID, originalName)
	u.mu.Lock()
	u.filename = name
	u.mu.Unlock()
	return name
}

// Filename 返回绑定的受管落盘名（未绑定为空）。
func (u *ActiveUpload) Filename() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.filename
}

// CancelCh 返回停滞取消信号通道：停滞超时取消收尾第一步发出（close）。
// Lane B MUST 将请求体读取 ctx 挂接此通道，取消后中断读取、经 done 等待收尾。
func (u *ActiveUpload) CancelCh() <-chan struct{} { return u.cancelCh }

// DoneCh 返回上传执行结束信号通道（FinalizeUpload 返回或 AbortUpload 调用后关闭）。
func (u *ActiveUpload) DoneCh() <-chan struct{} { return u.done }

// TouchProgress 报告一次请求体读取进度（任何非零字节读取即调用；api 扫描器在
// 读取边界上报，覆盖首 part header、connId、file payload、尾部 epilogue 全阶段）。
func (u *ActiveUpload) TouchProgress() { u.touchProgress() }

// touchProgress 进度刷新：记录最近进度时刻并重置停滞计时器。已取消后不再重置。
func (u *ActiveUpload) touchProgress() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.canceled || u.timer == nil {
		return
	}
	u.lastProgressAt = time.Now()
	u.timer.Reset(u.stallTimeout)
}

// markDone 结束信号（恰好一次）；同时停止停滞计时器（计时器已触发的场合
// stallCancel 侧的登记复核使收尾为 no-op）。
func (u *ActiveUpload) markDone() {
	u.doneOnce.Do(func() {
		u.mu.Lock()
		if u.timer != nil {
			u.timer.Stop()
		}
		u.mu.Unlock()
		close(u.done)
	})
}

// isCanceled 报告停滞取消是否已触发（不可提交标记）。
func (u *ActiveUpload) isCanceled() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.canceled
}

// WriteUploadBody 流式写请求体到 .partial（上限 M+1 字节探测 → ErrUploadTooLarge）。
// 每个非零字节块重置停滞计时器。写失败不自动清理：由调用方经 AbortUpload 收尾
//（spec：任一失败不 rename、不发布、清理 .partial）。
func (o *Orchestrator) WriteUploadBody(ctx context.Context, u *ActiveUpload, r io.Reader) (int64, error) {
	name := u.Filename()
	if name == "" {
		return 0, ErrUploadNotBound
	}
	limited := io.LimitReader(r, o.cfg.MaxBytes+1)
	n, err := o.store.WritePartial(ctx, u.TaskID, name, limited, func(int64) {
		u.touchProgress()
	})
	if err != nil {
		return n, err
	}
	if n > o.cfg.MaxBytes {
		return n, ErrUploadTooLarge
	}
	return n, nil
}

// FinalizeUpload 提交上传（per-task 协调锁临界区内复查与提交）：
// 停滞取消已触发 → ErrUploadStalled；随后复查顺序固定为任务存在（404）→ active（409）
// → 当前 connId（403）。复查通过后执行提交（sidecar 失败不发布）；成功才入可投递索引。
// 返回错误时调用方 MUST 以 AbortUpload 收尾（清理 .partial）。
func (o *Orchestrator) FinalizeUpload(ctx context.Context, u *ActiveUpload) error {
	defer u.markDone()
	release, err := o.coord.Acquire(ctx, u.TaskID)
	if err != nil {
		return err
	}
	defer release()

	if u.isCanceled() {
		return ErrUploadStalled
	}
	// 复查（顺序钉死）：任务存在 → active → connId 归属。
	connID := u.ConnID()
	exists, active, err := o.tasks.TaskActive(ctx, u.TaskID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUploadTaskNotFound
	}
	if !active {
		return ErrUploadTaskInactive
	}
	if o.conns != nil {
		current, found, err := o.conns.CurrentTUIConn(ctx, u.TaskID)
		if err != nil {
			return err
		}
		if !found || current != connID {
			return ErrConnNotCurrent
		}
	}
	name := u.Filename()
	if name == "" {
		return ErrUploadNotBound
	}
	meta := Meta{
		UploadID:   u.ID,
		TaskID:     u.TaskID,
		ConnID:     connID,
		Filename:   name,
		UploadedAt: o.clock().UTC(),
	}
	if err := o.store.CommitUpload(u.TaskID, meta); err != nil {
		return err
	}
	u.mu.Lock()
	u.committed = true
	u.mu.Unlock()
	o.mu.Lock()
	o.index[indexKey{taskID: u.TaskID, uploadID: u.ID}] = meta
	o.mu.Unlock()
	o.unregister(u)
	return nil
}

// AbortUpload 上传异常收尾（幂等）：注销登记并删除 .partial。已提交（FinalizeUpload
// 成功）后调用不再删文件。同时发出 done 信号（停滞取消收尾的等待点）。
func (o *Orchestrator) AbortUpload(ctx context.Context, u *ActiveUpload) {
	u.markDone()
	release, err := o.coord.Acquire(ctx, u.TaskID)
	if err != nil {
		o.logf("uploads: abort acquire coordination lock (task %s): %v", u.TaskID, err)
		return
	}
	defer release()
	if cur := o.lookupActive(u.ID); cur != u {
		return // 已注销（finalize 成功或取消收尾已清理）
	}
	u.mu.Lock()
	committed := u.committed
	name := u.filename
	u.mu.Unlock()
	if !committed && name != "" {
		if err := o.store.RemovePartial(u.TaskID, name); err != nil {
			o.logf("uploads: abort remove partial %s/%s: %v", u.TaskID, name, err)
		}
	}
	o.unregister(u)
}

// stallCancel 停滞取消收尾三步（design D4/D5）：
//  ①持协调锁标记不可提交 + 发取消信号 → 释放锁；
//  ②中断读取并等上传执行结束（不得持协调锁等待）；
//  ③重新持锁确认身份及非活跃后删 .partial（注销登记）。
func (o *Orchestrator) stallCancel(u *ActiveUpload) {
	// ①
	release, err := o.coord.Acquire(context.Background(), u.TaskID)
	if err != nil {
		o.logf("uploads: stall cancel acquire coordination lock (task %s): %v", u.TaskID, err)
		return
	}
	var trigger bool
	func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.active[u.ID] != u {
			return // 已 finalize/abort 注销，无需收尾
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.canceled {
			return
		}
		// 复核停滞时长：Timer.Reset 无法撤回已触发、正等协调锁的回调，等锁期间
		// 到达的新进度（lastProgressAt 后移）说明未停滞，重新安排计时、不取消。
		if stalled := time.Since(u.lastProgressAt); stalled < u.stallTimeout {
			u.timer.Reset(u.stallTimeout - stalled)
			return
		}
		u.canceled = true
		trigger = true
	}()
	if trigger {
		close(u.cancelCh)
	}
	release() // 释放锁：不得持锁等 handler 退出
	if !trigger {
		return // 已 finalize/abort，无需收尾
	}
	o.logf("uploads: upload %s (task %s) stalled for %s; cancelling", u.ID, u.TaskID, u.stallTimeout)
	// ② 中断读取由 Lane B 经 CancelCh 完成；此处等上传执行结束。
	<-u.done
	// ③
	release2, err := o.coord.Acquire(context.Background(), u.TaskID)
	if err != nil {
		o.logf("uploads: stall cancel re-acquire (task %s): %v", u.TaskID, err)
		return
	}
	defer release2()
	if cur := o.lookupActive(u.ID); cur != u {
		return
	}
	if name := u.Filename(); name != "" {
		if err := o.store.RemovePartial(u.TaskID, name); err != nil {
			o.logf("uploads: stall cancel remove partial %s/%s: %v", u.TaskID, name, err)
		}
	}
	o.unregister(u)
}

// lookupActive 查活跃登记（未注册返回 nil）。
func (o *Orchestrator) lookupActive(uploadID string) *ActiveUpload {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.active[uploadID]
}

// unregister 注销登记（登记项为同一指针时删除）。
func (o *Orchestrator) unregister(u *ActiveUpload) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cur, ok := o.active[u.ID]; ok && cur == u {
		delete(o.active, u.ID)
	}
}
