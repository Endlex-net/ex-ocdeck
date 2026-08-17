package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"ocdeck/internal/process"
)

// hostEnv 读取宿主环境变量（design.md §2：基础集从宿主取，不继承全部）。
func hostEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

// noticeEntry 对应 design.md §8 notice 数组项。
// data 仅对 residual_processes 出现，含 sessionName/cleanupTickets/reason/retryable。
// retryable 必须在 data 内（§8），不在顶层。
type noticeEntry struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	TS      int64                  `json:"ts"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

const (
	noticeCodeResidual        = "residual_processes"
	noticeCodeSessionOverflow = "session_overflow"
)

// notice reason 枚举（§8：residual_processes.data.reason）。
// 注意：这是 reason 枚举原值，非 disposition 名。
const (
	noticeReasonSnapshotFailed  = "snapshot_failed"
	noticeReasonKillFailed      = "kill_failed"
	noticeReasonReapFailed      = "reap_failed"
	noticeReasonSnapshotMissing = "snapshot_missing_degraded"
)

// dispositionToNotice 将 process.Disposition 唯一映射为 (reason, retryable)（B6）。
// disposition 名 → reason 枚举原值。clean 不产生 notice。
func dispositionToNotice(d process.CleanupDisposition) (reason string, retryable bool, ok bool) {
	switch d {
	case process.DispositionSnapshotFailed:
		return noticeReasonSnapshotFailed, true, true
	case process.DispositionKillFailed:
		return noticeReasonKillFailed, true, true
	case process.DispositionReapFailed:
		return noticeReasonReapFailed, true, true
	case process.DispositionSnapshotMissingDegraded:
		return noticeReasonSnapshotMissing, false, true
	default:
		return "", false, false
	}
}

// newRuntime 构造任务运行时索引，generation 单调递增（不得运行时清除后归零，B4）。
// instanceID 为本代唯一标识，供回调三元组校验（B4）。
//
// P1.4.3：generation/tombstone 责任已迁移至 application/runtime.Registry（单一锁域）。
// Manager 不再持有 genMu/lastGen，全部经 Registry.NewRuntimeToken 分配，tombstone 语义
// 保持不变（清理后从 lastGen+1 续递增，design.md D0:204-208 先例）。
func (m *Manager) newRuntime(taskID string) *taskRuntime {
	prev := m.getRuntime(taskID)
	prevGen := 0
	if prev != nil {
		prevGen = prev.generation
	}
	token := m.runtimeRegistry.NewRuntimeToken(taskID, prevGen, newTaskID())
	return &taskRuntime{
		taskID:       taskID,
		generation:   token.Generation,
		instanceID:   token.InstanceID,
		groups:       map[string]*runtimeGroup{},
		watchCancels: map[string]func(){},
		watchDones:   map[string]<-chan struct{}{},
	}
}

// matchesRegistry 校验回调三元组 (generation, role/sessionName, InstanceID) 是否仍匹配
// 当前运行时注册表的对应 group（design.md §2 回调隔离，B4）。
// 不匹配即忽略回调（旧代回调不得清理新代）。
func (rt *taskRuntime) matchesRegistry(gen int, key, instanceID string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.generation != gen {
		return false
	}
	g, ok := rt.groups[key]
	if !ok || g == nil {
		return false
	}
	return g.Generation == gen && g.InstanceID == instanceID
}

// parseNotices 解析 tasks.notice JSON 数组。
// JSON 损坏视为有 debt（fail-closed，B6）：返回 error，调用方据此拒绝门禁。
func parseNotices(raw sql.NullString) ([]noticeEntry, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var out []noticeEntry
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, fmt.Errorf("notice json corrupted: %w", err)
	}
	return out, nil
}

// encodeNotices 编码 notice 数组为 sql.NullString。
func encodeNotices(entries []noticeEntry) sql.NullString {
	if len(entries) == 0 {
		return sql.NullString{}
	}
	b, _ := json.Marshal(entries)
	return sql.NullString{String: string(b), Valid: true}
}

// hasRetryableNotice 检查任务是否存在任意 retryable=true 的 residual_processes notice（design.md §19 Activate 门禁）。
// JSON 损坏视为有 debt（fail-closed，B6）。
func (m *Manager) hasRetryableNotice(ctx context.Context, row TaskRow) (bool, error) {
	entries, err := parseNotices(row.Notice)
	if err != nil {
		return true, err
	}
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		r, ok := e.Data["retryable"]
		if !ok {
			continue
		}
		if b, ok := r.(bool); ok && b {
			return true, nil
		}
	}
	return false, nil
}

// recordResidualNotice 记录 residual_processes notice（CAS 循环，design.md §5/§8）。
// 即使无 tickets 也必须记 notice（snapshot_failed / snapshot_missing_degraded，B6）。
// CAS 写回成功后读回校验收敛：重读 notice 确认本条 entry 已落库，
// 不收敛则继续 CAS 重试（避免写回被并发覆盖后误判完成）。
// 返回 error：CAS 循环耗尽仍未收敛（store 不可达/持续被并发覆盖）时返回错误，
// 供调用方（Activate 失败路径、finishSuspend）聚合进 last_error，不静默吞错（design.md §8）。
func (m *Manager) recordResidualNotice(ctx context.Context, taskID, sessionName string, tickets []string, reason string, retryable bool) error {
	entry := noticeEntry{
		Code:    noticeCodeResidual,
		Message: fmt.Sprintf("cleanup failed for %s: %s", sessionName, reason),
		TS:      nowUnixI(),
		Data: map[string]interface{}{
			"sessionName":    sessionName,
			"cleanupTickets": tickets,
			"reason":         reason,
			"retryable":      retryable,
		},
	}
	// CAS 循环：读当前 notice → 按会话替换/追加 → CAS 写；失败重读重试，不得覆盖丢并发 notice（B6）。
	// 写回成功后读回校验收敛，确认本条已落库（避免 CAS 成功但被并发覆盖后误判完成）。
	// 按会话去重（P4 阻塞 1）：同一会话已有 residual 项时替换，不追加——避免同一会话多条 notice 膨胀。
	key := noticeKey(entry)
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("record residual notice: get task: %w", err)
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			// JSON 损坏 MUST fail-closed（视为有 debt，返回错误），不得当空数组覆盖
			// 已落库的 notice——覆盖会丢失未收割 tickets 的索引，逃逸进程再无身份可定位。
			return fmt.Errorf("record residual notice: notice json corrupted (task %s): %w", taskID, perr)
		}
		// P4 复评阻塞 1：替换时 tickets MUST union（旧 + 新去重合并），不得 latest-wins——
		// 旧 tk1 未收割时记录 tk2 会永久丢 tk1（design.md §5：新产生 tickets 合并进 notice）。
		// reason/retryable 以新 entry 为准（转换语义：snapshot_failed→degraded 时 retryable 变 false）。
		dup := false
		for i, e := range entries {
			if noticeKey(e) == key {
				merged := unionTickets(noticeTickets(e), tickets)
				entry.Data["cleanupTickets"] = merged
				entries[i] = entry
				dup = true
				break
			}
		}
		if !dup {
			entries = append(entries, entry)
		}
		newRaw := encodeNotices(entries)
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, newRaw)
		if !r.Matched {
			continue
		}
		// 读回校验收敛：确认本条 entry 已落库。
		if m.noticeReadbackContains(ctx, taskID, entry) {
			return nil
		}
		// 未收敛：被并发覆盖，继续重试。
	}
	return fmt.Errorf("record residual notice: CAS did not converge (task %s, session %s)", taskID, sessionName)
}

// recordResidualNoticeFromDisposition 唯一映射 disposition → notice（B6）。
// clean 不记；其余即使无 tickets 也记。返回 recordResidualNotice 的 error（不静默）。
func (m *Manager) recordResidualNoticeFromDisposition(ctx context.Context, taskID, sessionName string, res process.KillResult) error {
	reason, retryable, ok := dispositionToNotice(res.Disposition)
	if !ok {
		return nil
	}
	return m.recordResidualNotice(ctx, taskID, sessionName, res.CleanupTickets, reason, retryable)
}

// casWriteNotices 将 remaining notice CAS 写回（design.md §8：新 tickets 不丢失）。
// retryDebt 等处理路径可能合并新 cleanupTickets，落 deletion_failed 前 MUST CAS 写回，
// 逃逸进程下次 Retry 需 tickets 定位。返回 error：store 不可达或 CAS 不收敛。
// 损坏 JSON 视为 fail-closed：不得覆盖，返回错误（design.md §8）。
func (m *Manager) casWriteNotices(ctx context.Context, taskID string, remaining []noticeEntry) error {
	newRaw := encodeNotices(remaining)
	for attempt := 0; attempt < 8; attempt++ {
		cur, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("cas write notices: get task: %w", err)
		}
		// 损坏 JSON 不覆盖（fail-closed）。
		if _, perr := parseNotices(cur.Notice); perr != nil {
			return fmt.Errorf("cas write notices: notice json corrupted (task %s): %w", taskID, perr)
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, cur.Notice, newRaw)
		if r.Matched {
			return nil
		}
	}
	return fmt.Errorf("cas write notices: CAS did not converge (task %s)", taskID)
}

// noticeReadbackContains 读回 notice 并判断本条 entry 是否已落库（按 noticeKey 去重匹配）。
func (m *Manager) noticeReadbackContains(ctx context.Context, taskID string, entry noticeEntry) bool {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		return false
	}
	want := noticeKey(entry)
	for _, e := range entries {
		if noticeKey(e) == want {
			return true
		}
	}
	return false
}

// processRetryableNotices 后台周期处理 retryable notice（design.md §5）。
// 逐任务：取得 keyed mutex → 重读 notice → 对有 sessionName 的项先 KillSession → RetryReap tickets → CAS 清除。
// 返回聚合 error：各任务 retryTaskNotices 的错误汇总（逐项 error 用 "; " 连接），
// 便于后台循环与 Shutdown 收尾统一记录/等待，不静默吞错。
func (m *Manager) processRetryableNotices(ctx context.Context) error {
	tasks, err := m.store.ListAllTasks(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	var errs []error
	for _, t := range tasks {
		entries, perr := parseNotices(t.Notice)
		if perr != nil {
			// JSON 损坏：fail-closed，跳过（不修改 notice，留待人工/下次）。
			continue
		}
		if len(entries) == 0 {
			continue
		}
		if rerr := m.retryTaskNotices(ctx, t, entries); rerr != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, rerr))
		}
	}
	return errors.Join(errs...)
}

// retryTaskNotices 处理单任务的 notice 项（需取得 keyed mutex，design.md §5）。
// 持锁后重读 notice 再行动（不得用锁外捕获的旧值，B6）。
// 返回聚合 error：处理过程中的 store 错误与末尾 CAS 失败（CAS 循环耗尽）MUST 聚合返回，
// 不静默 return（避免后台循环丢失错误、关停残留 debt 被静默丢弃）。
func (m *Manager) retryTaskNotices(ctx context.Context, t TaskRow, entries []noticeEntry) error {
	unlock, err := m.tryLockTask(t.ID)
	if err != nil {
		// 冲突跳过本轮（用户操作在执行），下次周期重试：非错误，不聚合。
		return nil
	}
	defer unlock()

	var errs []error

	// 持锁后重读 notice（B6：拿锁后重读再行动）。
	row, err := m.store.GetTask(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("reread task: %w", err)
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		return fmt.Errorf("parse notice: %w", perr)
	}

	var remaining []noticeEntry
	// clearedKeys 记录本轮成功清除的 notice key（B6：CAS 失败重试时不得从最新读回的
	// 并发新增中复活本轮已清除项）。CAS 新值 MUST 基于同一次读取的最新版本计算。
	clearedKeys := make(map[string]bool)
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			// 非 residual（如 session_overflow）保留，后台不处理。
			remaining = append(remaining, e)
			continue
		}
		retryable, _ := e.Data["retryable"].(bool)
		if !retryable {
			// 不可重试（snapshot_missing_degraded）保留告警，不参与后台重试（§8）。
			remaining = append(remaining, e)
			continue
		}
		sessionName, _ := e.Data["sessionName"].(string)
		tickets := noticeTickets(e)

		// 后台重试须校验 generation/注册表，不得杀刚修复的 serve（B7）。
		// 若任务有活跃运行时且 sessionName 属当前代 serve/tui，则跳过（用户操作或 Suspend 修复在管）。
		if rt := m.getRuntime(t.ID); rt != nil {
			if m.sessionOwnedByRuntime(rt, sessionName) {
				// 保留该项，本轮不杀（避免杀刚修复的 serve）。
				remaining = append(remaining, e)
				continue
			}
		}

		// 有 sessionName → 先 KillSession（错误分类处理，B6）。
		if sessionName != "" {
			exists, herr := m.proc.HasSession(sessionName)
			if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
				// infra 错误（非 no-server）：保留重试，不丢。
				remaining = append(remaining, e)
				errs = append(errs, fmt.Errorf("has-session %s: %w", sessionName, herr))
				continue
			}
			if exists {
				res, kerr := m.proc.KillSession(sessionName)
				if kerr != nil {
					// kill 本身报错：保留原 notice 重试。
					remaining = append(remaining, e)
					errs = append(errs, fmt.Errorf("kill-session %s: %w", sessionName, kerr))
					continue
				}
				if res.Disposition != process.DispositionClean {
					// 仍失败：合并新 tickets，按新 disposition 更新 reason/retryable（唯一映射）。
					tickets = append(tickets, res.CleanupTickets...)
					e.Data["cleanupTickets"] = tickets
					reason, retry, _ := dispositionToNotice(res.Disposition)
					e.Data["reason"] = reason
					e.Data["retryable"] = retry
					if !retry {
						// 转为不可重试 degraded（保留告警）。
						e.Data["retryable"] = false
					}
					remaining = append(remaining, e)
					continue
				}
				// kill 成功，继续 reap。
			} else if reason, _ := e.Data["reason"].(string); reason == noticeReasonSnapshotFailed {
				// SnapshotFailed 历史会话在重试前自行消失 → 转 degraded（非成功清除，B6/§5）。
				e.Data["reason"] = noticeReasonSnapshotMissing
				e.Data["retryable"] = false
				remaining = append(remaining, e)
				continue
			}
		}
		// RetryReap tickets（错误分类处理，B6）。
		if len(tickets) > 0 {
			left, rerr := m.proc.RetryReap(tickets)
			if rerr != nil {
				remaining = append(remaining, e)
				errs = append(errs, fmt.Errorf("retry-reap: %w", rerr))
				continue
			}
			if len(left) > 0 {
				e.Data["cleanupTickets"] = left
				remaining = append(remaining, e)
				continue
			}
		}
		// 成功清除该项（不加入 remaining），记录其 key 供 CAS 合并排除。
		clearedKeys[noticeKey(e)] = true
	}
	// CAS 写回剩余 notice（失败重读重试，不得覆盖丢并发新增；已清除项不得复活）。
	for attempt := 0; attempt < 8; attempt++ {
		cur, err := m.store.GetTask(ctx, t.ID)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("reread for CAS: %w", err))...)
		}
		// CAS expected 基于本次最新读取的 cur.Notice；newRaw 基于同一次读取合并的结果。
		curEntries, _ := parseNotices(cur.Notice)
		merged := mergeNoticesExcluding(curEntries, remaining, clearedKeys)
		newRaw := encodeNotices(merged)
		r, _ := m.writeNoticeCAS(ctx, t.ID, cur.Notice, newRaw)
		if r.Matched {
			return errors.Join(errs...)
		}
		// CAS 失败：下一轮基于最新 cur.Notice 重新合并（remaining 不变，已清除项继续排除）。
	}
	// CAS 循环耗尽仍未收敛：聚合返回，不静默丢失。
	return errors.Join(append(errs, fmt.Errorf("notice CAS write did not converge after retries (task %s)", t.ID))...)
}

// sessionOwnedByRuntime 判断 sessionName 是否属于当前运行时注册表的会话（B7 后台重试保护）。
func (m *Manager) sessionOwnedByRuntime(rt *taskRuntime, sessionName string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.groups[sessionName]
	return ok
}

// mergeNoticesExcluding 合并最新读回的 src 与本轮处理后的 processed，并排除本轮已清除项
// （clearedKeys）。src 中命中 clearedKeys 的项不得复活（本轮已成功清除，即便并发写回也
// 不得恢复）。src 中不在 processed 且不在 clearedKeys 的项视为并发新增，保留（不丢）。
func mergeNoticesExcluding(src, processed []noticeEntry, clearedKeys map[string]bool) []noticeEntry {
	out := make([]noticeEntry, 0, len(src)+len(processed))
	seen := make(map[string]bool, len(processed))
	for _, p := range processed {
		seen[noticeKey(p)] = true
	}
	for _, s := range src {
		k := noticeKey(s)
		if seen[k] {
			continue
		}
		// 本轮已清除项不得从并发最新读中复活。
		if clearedKeys[k] {
			continue
		}
		// 并发新增（不在 processed、不在 cleared）：保留，不丢。
		out = append(out, s)
	}
	out = append(out, processed...)
	return out
}

// noticeKey 生成 notice 项的去重 key（identity = code + sessionName）。
// 身份 MUST 按会话（sessionName）而非含 reason/ts 的 key：reason 在 retryTaskNotices 中可
// 原地转换（snapshot_failed → snapshot_missing_degraded），ts 每次 record 不同——若 key
// 含可变字段，转换后旧项因 key 变化会被 mergeNoticesExcluding 当作"并发新增"保留，
// 导致旧 retryable 项永不消失 + 每轮追加新 degraded，后台永久重试膨胀、Activate 被永久
// cleanup-debt conflict 阻塞（P4 评审阻塞 1）。按会话去重保证转换时旧项被替换、仅剩一条。
func noticeKey(e noticeEntry) string {
	sn, _ := e.Data["sessionName"].(string)
	return fmt.Sprintf("%s|%s", e.Code, sn)
}

// toStrings 将 []interface{} 转为 []string（notice data 中 tickets 经 JSON 解码为 interface）。
func toStrings(in []interface{}) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// noticeTickets 从 notice data 提取 cleanupTickets（防御性：JSON null 或缺字段不 panic）。
// data 中 cleanupTickets 经 JSON 解码：存在为数组 → []interface{}；JSON null/缺字段 → nil interface。
// 直接 .([]interface{}) 对 nil 会 panic，故用 comma-ok 断言兜底返回空切片。
func noticeTickets(e noticeEntry) []string {
	raw, ok := e.Data["cleanupTickets"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	return toStrings(arr)
}

// unionStringSlices 合并两批字符串去重（保序，旧在前新在后）。通用工具。
func unionStringSlices(old, neu []string) []string {
	seen := make(map[string]struct{}, len(old)+len(neu))
	out := make([]string, 0, len(old)+len(neu))
	for _, tk := range old {
		if _, ok := seen[tk]; ok {
			continue
		}
		seen[tk] = struct{}{}
		out = append(out, tk)
	}
	for _, tk := range neu {
		if _, ok := seen[tk]; ok {
			continue
		}
		seen[tk] = struct{}{}
		out = append(out, tk)
	}
	return out
}

// unionTickets 合并两批 tickets 去重（保序，旧在前新在后）。
// 用于 recordResidualNotice 替换时合并旧 entry tickets 与新 tickets（design.md §5：
// 新产生 tickets 合并进 notice，不覆盖丢失未收割的旧 tickets）。
func unionTickets(old, neu []string) []string {
	return unionStringSlices(old, neu)
}
