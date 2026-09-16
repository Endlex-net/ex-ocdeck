// title_sync.go 实现任务名称提交后的会话标题 best-effort 同步（openspec change
// task-info-editable D3，tasks 3.2 接线）：titleSync seam 的生产实现。
//
// 协议（D3）：活跃任务 runtime + 锚定会话存在时 PATCH /session/{id}；能力判定固定为
// 首次直接 PATCH，404 → ErrSessionTitleUnsupported 按 `taskID + instVersion` 缓存降级
// （capRegistry 同构 registry；runtime 替换即 instVersion 变化缓存失效重新实测），网络
// 错误/超时不缓存下次重试；404/网络/超时统一降级为 notice（固定 code
// task_title_sync_failed，按任务去重）、MUST NOT 阻断改名。notice 清除固定为：本地事务
// 提交成功且标题同步成功后执行；清除失败仅记日志，不产生新 notice。
//
// 并发合并（D3：同 registry 先例）：同任务同实例版本的并发首次能力判定经 inflight 合并，
// 底层 client 仅收到一次 PATCH，等待者共享首测结果。
package task

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"ocdeck/internal/infrastructure/opencode"
)

// noticeCodeTitleSync 标题同步失败 notice 固定 code（D3）。
const noticeCodeTitleSync = "task_title_sync_failed"

// titleCapEntry 标题能力的 instVersion-bound 缓存条目（仅缓存稳定态 unsupported；
// 网络类失败不缓存）。
type titleCapEntry struct {
	instVersion string
	unsupported bool
}

// titleProbeOutcome 是一次标题能力实测的结构化结果（首测 leader 与等待者共享）。
type titleProbeOutcome struct {
	unsupported bool  // 实测/缓存命中得到 ErrSessionTitleUnsupported（稳定态）
	err         error // 网络等其他失败（不缓存）；nil = PATCH 成功
}

// titleFlight 是一次进行中的标题能力首测：done 关闭前 outcome 已写入（close 提供
// happens-before），全部等待者共享同一结果（广播语义，无零值误读）。
type titleFlight struct {
	done    chan struct{}
	outcome titleProbeOutcome
}

// titleCapRegistry 标题能力缓存 + inflight 合并注册表（capRegistry 同构）。
//
// 不直接复用 diffreview_capability.capRegistry：其缓存值类型与稳定态语义绑定
// diffreview.CapabilityState（prompt_async 探测），探测函数/写读 helper 均硬编码
// GET /doc 路径——两能力共用同一 registry 会在 taskID 键上互覆写缓存。故按其同构
// 模式（cache + inflight 合并 + instVersion fence）为标题能力独立实现。
//
// 生命周期：挂在 Manager 实例内（非全局 map 以 *Manager 为 key），随 Manager 回收，
// 无需显式清理。
type titleCapRegistry struct {
	cache    map[string]titleCapEntry // taskID → 缓存条目（instVersion fence 写入）
	inflight map[string]*titleFlight  // taskID@instVersion → 首测进行中（结果广播）
	mu       sync.Mutex
}

// getTitleCapRegistry 懒初始化实例内注册表（New 后通常已就绪；防御直构 Manager 的测试）。
func (m *Manager) getTitleCapRegistry() *titleCapRegistry {
	m.titleCapMu.Lock()
	defer m.titleCapMu.Unlock()
	if m.titleCaps == nil {
		m.titleCaps = &titleCapRegistry{
			cache:    map[string]titleCapEntry{},
			inflight: map[string]*titleFlight{},
		}
	}
	return m.titleCaps
}

// titleCapabilityUnsupported 读稳定缓存：仅 instVersion 匹配且已判 unsupported 时命中；
// 无 runtime / 实例替换 / 未实测 → 不命中（重新实测 PATCH）。
func (m *Manager) titleCapabilityUnsupported(taskID, instVersion string) bool {
	reg := m.getTitleCapRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	e, ok := reg.cache[taskID]
	return ok && e.instVersion == instVersion && e.unsupported
}

// probeTitleCapability 首次实测标题更新能力（并发经 inflight 合并，D3 singleflight 同
// registry 先例）。probe 为底层 PATCH 调用（仅 leader 执行恰一次）。
//
// 返回结构化结果：
//   - 稳定缓存命中（unsupported，instVersion 匹配）→ 直接返回，不实测；
//   - 有 inflight → 等待并共享 leader 结果（不重发 PATCH）；
//   - leader：实测一次 → instVersion fence 写缓存（仅 404 稳定态）→ 结果广播给等待者。
//
// 实例版本变化 → 缓存键/fence 不匹配 → 下次调用重新实测（D3「实例更换失效」）。
func (m *Manager) probeTitleCapability(ctx context.Context, taskID, instVersion string, probe func() error) titleProbeOutcome {
	reg := m.getTitleCapRegistry()
	key := taskID + "@" + instVersion

	reg.mu.Lock()
	// 稳定缓存命中（仅 unsupported 稳定；网络失败未缓存自然错过）。
	if e, ok := reg.cache[taskID]; ok && e.instVersion == instVersion && e.unsupported {
		reg.mu.Unlock()
		return titleProbeOutcome{unsupported: true, err: opencode.ErrSessionTitleUnsupported}
	}
	// 挂入既有 flight：共享 leader 实测结果，不重发 PATCH。
	if fl, inflight := reg.inflight[key]; inflight {
		reg.mu.Unlock()
		select {
		case <-fl.done:
			return fl.outcome
		case <-ctx.Done():
			return titleProbeOutcome{err: ctx.Err()}
		}
	}
	// 首个调用者：标记首测进行中，释放锁后执行 PATCH。
	fl := &titleFlight{done: make(chan struct{})}
	reg.inflight[key] = fl
	reg.mu.Unlock()

	terr := probe()
	unsupported := errors.Is(terr, opencode.ErrSessionTitleUnsupported)

	// 收敛 flight 并 fence 写缓存：仅当前实例一致时写入（探测期间可能挂起/替换）。
	// outcome 先于 close 写入（done 关闭对等待者提供 happens-before），全部等待者共享。
	reg.mu.Lock()
	delete(reg.inflight, key)
	rt := m.getRuntime(taskID)
	if unsupported && rt != nil && string(rt.instVersion) == instVersion {
		reg.cache[taskID] = titleCapEntry{instVersion: instVersion, unsupported: true}
	}
	out := titleProbeOutcome{unsupported: unsupported, err: terr}
	fl.outcome = out
	reg.mu.Unlock()

	close(fl.done)
	return out
}

// syncSessionTitle 是 titleSync seam 的生产实现（D3 协议表）。
//   - 无 runtime（非 active）/ 无锚定会话 → 不启动进程、不产生提示；
//   - 能力判定（缓存命中/共享/实测 unsupported）→ 记降级 notice，不重发 PATCH；
//   - PATCH 成功 → 清除既有失败 notice（清除失败仅记日志）；
//   - 5xx/网络/超时 → notice（不缓存，下次重试）。
//
// 全部失败 best-effort：MUST NOT 阻断改名（无返回值）。
func (m *Manager) syncSessionTitle(ctx context.Context, taskID, title string) {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil || !row.AnchorSessionID.Valid || row.AnchorSessionID.String == "" {
		return
	}
	oc, dir, ok := m.taskOcClient(ctx, taskID)
	if !ok {
		m.recordTitleSyncNotice(ctx, taskID, "runtime client unavailable")
		return
	}
	anchorID := row.AnchorSessionID.String
	out := m.probeTitleCapability(ctx, taskID, string(rt.instVersion), func() error {
		return oc.UpdateSessionTitle(ctx, dir, anchorID, title)
	})
	switch {
	case out.unsupported:
		// 404（实测/缓存/共享）：能力降级，不重发 PATCH。
		m.recordTitleSyncNotice(ctx, taskID, "session title update unsupported")
	case out.err == nil:
		// PATCH 成功：清除既有失败 notice（清除失败仅记日志）。
		if cerr := m.clearTitleSyncNotice(ctx, taskID); cerr != nil {
			log.Printf("title sync: clear notice for task %s: %v", taskID, cerr)
		}
	default:
		// 5xx/网络/超时：不缓存，下次重试。
		m.recordTitleSyncNotice(ctx, taskID, out.err.Error())
	}
}

// recordTitleSyncNotice 记录标题同步失败 notice（固定 code，按任务去重：已有同 code 项
// 原地替换，不追加膨胀）。CAS 循环镜像 recordResidualNotice 先例；notice JSON 损坏不覆盖
// （fail-closed，MUST NOT 把降级提示写进损坏数组）；CAS 不收敛仅记日志（best-effort）。
func (m *Manager) recordTitleSyncNotice(ctx context.Context, taskID, detail string) {
	entry := noticeEntry{
		Code:    noticeCodeTitleSync,
		Message: detail,
		TS:      nowUnixI(),
	}
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			log.Printf("title sync: record notice for task %s: %v", taskID, err)
			return
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			log.Printf("title sync: notice json corrupted (task %s): %v", taskID, perr)
			return
		}
		dup := false
		for i, e := range entries {
			if e.Code == noticeCodeTitleSync {
				entries[i] = entry
				dup = true
				break
			}
		}
		if !dup {
			entries = append(entries, entry)
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, encodeNotices(entries))
		if r.Matched {
			return
		}
	}
	log.Printf("title sync: record notice did not converge (task %s)", taskID)
}

// clearTitleSyncNotice 清除标题同步失败 notice（D3：提交成功且标题同步成功后执行）。
// 无同 code 项时幂等返回 nil。CAS 循环镜像 casWriteNotices 先例；错误返回给调用方
// （syncSessionTitle 仅记日志，不产生新 notice）。
func (m *Manager) clearTitleSyncNotice(ctx context.Context, taskID string) error {
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("clear title sync notice: get task: %w", err)
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			return fmt.Errorf("clear title sync notice: notice json corrupted (task %s): %w", taskID, perr)
		}
		out := make([]noticeEntry, 0, len(entries))
		removed := false
		for _, e := range entries {
			if e.Code == noticeCodeTitleSync {
				removed = true
				continue
			}
			out = append(out, e)
		}
		if !removed {
			return nil
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, encodeNotices(out))
		if r.Matched {
			return nil
		}
	}
	return fmt.Errorf("clear title sync notice: CAS did not converge (task %s)", taskID)
}
