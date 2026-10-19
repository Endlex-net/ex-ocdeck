package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"ocdeck/internal/application/runtime"
	"ocdeck/internal/infrastructure/process"
)

// hostEnv 读取宿主环境变量（design.md §2：基础集从宿主取，不继承全部）。
// D1 收窄：仅进程环境，不做 login shell 兜底（基础集/locale/userShell 来源不变）。
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
	// noticeCodeSuspendStop 是 v2 停止收敛意图（评审 Round3 I2）：停止已确认但
	// 状态收敛写失败时落库；后台重放持任务锁复核后完成清关联+active→suspended
	// 再删意图。带执行代际（token）——新一代运行时旧意图失效清除，不影响新一代。
	noticeCodeSuspendStop = "suspend_stop_confirmed"
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

// newRuntime 构造任务运行时索引，instVersion 为本实例唯一令牌（design.md:70，B4）。
//
// P1.4.3 起令牌责任迁移至 application/runtime.Registry（单一锁域）；P1.4.9 收敛为单字符串
// instVersion：Registry.NewInstVersion 生成（ms 时间戳 + 随机后缀，构造上同任务不撞），
// tombstone 语义保持不变（分配即更新、清理后保留）。回调校验用等值比对，
// MUST NOT 数值比大小。
func (m *Manager) newRuntime(taskID string) *taskRuntime {
	return &taskRuntime{
		taskID:       taskID,
		instVersion:  m.runtimeRegistry.NewInstVersion(taskID),
		groups:       map[string]*runtimeGroup{},
		watchCancels: map[string]func(){},
		watchDones:   map[string]<-chan struct{}{},
		judgedPerms:  map[string]struct{}{},
		permVerdicts: map[string]permVerdictRecord{},
	}
}

// setPermJudgeReady 置实例就绪状态（task-permission-mode D4 触发点 (b1)）：
// 仅三条就绪提交路径成功且确认当前实例后调用（与 StartDiffReviewSchedulerForTask
// 同接缝）；幂等。调用方随后先置就绪再调用 judgeScan。
func (rt *taskRuntime) setPermJudgeReady() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.permJudgeReady = true
}

// matchesRegistry 校验回调身份 (instVersion, sessionName) 是否仍匹配当前运行时注册表的
// 对应 group（design.md §2 回调隔离，B4；P1.4.9 双字段收敛为单令牌等值比对）。
// 不匹配即忽略回调（旧实例回调不得清理新实例）。
func (rt *taskRuntime) matchesRegistry(tok runtime.InstVersion, key string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.instVersion != tok {
		return false
	}
	g, ok := rt.groups[key]
	if !ok || g == nil {
		return false
	}
	return g.InstVersion == tok
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
		// Round4 I2：停止收敛意图同为激活门禁债务（可重试收敛工作在位——激活
		// MUST NOT 绕过未完成的挂起收敛）。
		if e.Code == noticeCodeSuspendStop {
			return true, nil
		}
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

// recordSuspendStopIntent 落 v2 停止收敛意图（评审 Round3 I2）：停止已确认
// （Release interrupt 成功）但状态收敛写失败——durable retryable 意图，后台重放
// 持任务锁复核后完成清关联+active→suspended 再删意图。token 为停止确认时的
// runtime 执行代际（可校验：重放时与注册表当前代比对，新一代运行即意图过时
// 清除，不影响新一代）。CAS 同键替换不膨胀（形态同 recordResidualNotice）。
func (m *Manager) recordSuspendStopIntent(ctx context.Context, taskID, terminalRef, token string) error {
	entry := noticeEntry{
		Code:    noticeCodeSuspendStop,
		Message: fmt.Sprintf("suspend stop confirmed; convergence pending for %s", terminalRef),
		TS:      nowUnixI(),
		Data: map[string]interface{}{
			"sessionName": terminalRef,
			"token":       token,
			"retryable":   true,
		},
	}
	key := noticeKey(entry)
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("record suspend stop intent: get task: %w", err)
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			// JSON 损坏 fail-closed（同 residual——不得覆盖已落库 notice）。
			return fmt.Errorf("record suspend stop intent: notice json corrupted (task %s): %w", taskID, perr)
		}
		dup := false
		for i, e := range entries {
			if noticeKey(e) == key {
				// Round6 I19：同键替换保留未完成持久化义务（pending 随新条目
				// 延续——重新 kill 同名单元不保证重建原有 cleanup tickets）。
				nc := entry
				if old := decodeSuspendFinishObligations(e); len(old) > 0 {
					if nc.Data == nil {
						nc.Data = map[string]interface{}{}
					}
					nc.Data[suspendFinishPendingKey] = encodeSuspendFinishObligations(old)
				}
				entries[i] = nc
				dup = true
				break
			}
		}
		if !dup {
			entries = append(entries, entry)
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, encodeNotices(entries))
		if !r.Matched {
			continue
		}
		if m.noticeReadbackContains(ctx, taskID, entry) {
			return nil
		}
	}
	return fmt.Errorf("record suspend stop intent: CAS did not converge (task %s, session %s)", taskID, terminalRef)
}

// clearSuspendStopIntents 清除任务的全部停止收敛意图（Round4 I2：后续 Suspend
// 取得收敛权后，历史意图的前提已被取代——CAS 过滤式移除；无意图幂等成功）。
// Round6 I19：删除前必须补全意图携带的未完成持久化义务（notice/tickets、env）——
// 补全失败则保留意图（含 pending），不得丢弃 tickets 重放能力。
func (m *Manager) clearSuspendStopIntents(ctx context.Context, taskID string) error {
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("clear suspend stop intent: get task: %w", err)
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			// JSON 损坏 fail-closed：不得覆盖已落库 notice。
			return fmt.Errorf("clear suspend stop intent: notice json corrupted (task %s): %w", taskID, perr)
		}
		var remaining []noticeEntry
		changed := false
		for _, e := range entries {
			if e.Code != noticeCodeSuspendStop {
				remaining = append(remaining, e)
				continue
			}
			changed = true
			if pending := decodeSuspendFinishObligations(e); len(pending) > 0 {
				ref, _ := e.Data["sessionName"].(string)
				if rem := m.persistSuspendFinishObligations(ctx, taskID, ref, pending); len(rem) > 0 {
					// 义务仍未持久化：保留意图（含 pending 标注），不得删除。
					remaining = append(remaining, annotateSuspendStopPending(e, rem))
					continue
				}
			}
			// 无义务或义务已补全：意图可安全移除。
		}
		if !changed {
			return nil
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, encodeNotices(remaining))
		if r.Matched {
			return nil
		}
		// 未收敛：被并发覆盖，继续重试。
	}
	return fmt.Errorf("clear suspend stop intent: CAS did not converge (task %s)", taskID)
}

// suspendFinishObligation 是挂起收尾未完成的持久化义务（Round5 I17）：清理操作
// 失败可随收敛固化为债务，但债务/快照「持久化」失败 MUST 保留为未完成义务——
// 意图不清除、完整 tickets 随意图保留，下轮重放优先补全。Round8 I20：TaskID
// 为登记与落账的权威归属（ledger 按任务分组，drain 只取本任务条目）；Token 为
// env 义务的记录时代际（新一代运行不受旧 env 义务影响）。
type suspendFinishObligation struct {
	Kind      string   `json:"kind"` // "notice"（残留 notice 未落库）| "env"（env 快照清除失败）
	TaskID    string   `json:"taskId,omitempty"`
	Name      string   `json:"name,omitempty"`
	Tickets   []string `json:"tickets,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Retryable bool     `json:"retryable"`
	Token     string   `json:"token,omitempty"` // env 义务：记录时的 runtime 执行代际
}

const suspendFinishPendingKey = "pendingJSON"

func encodeSuspendFinishObligations(obs []suspendFinishObligation) string {
	if len(obs) == 0 {
		return ""
	}
	b, err := json.Marshal(obs)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeSuspendFinishObligations(e noticeEntry) []suspendFinishObligation {
	raw, _ := e.Data[suspendFinishPendingKey].(string)
	if raw == "" {
		return nil
	}
	var obs []suspendFinishObligation
	if err := json.Unmarshal([]byte(raw), &obs); err != nil {
		return nil
	}
	return obs
}

// persistSuspendFinishObligations 补全持久化义务，返回仍未完成的义务。
func (m *Manager) persistSuspendFinishObligations(ctx context.Context, taskID, terminalRef string, obs []suspendFinishObligation) []suspendFinishObligation {
	var remaining []suspendFinishObligation
	for _, o := range obs {
		switch o.Kind {
		case "notice":
			if nerr := m.recordResidualNotice(ctx, taskID, o.Name, o.Tickets, o.Reason, o.Retryable); nerr != nil {
				remaining = append(remaining, o)
			}
		case "env":
			// Round8 I20：env 义务携带记录时执行代际——新一代运行在位即视为
			// 义务过时（旧义务不得清除新一代的 env 快照；对齐 I2 token 机制）。
			if o.Token != "" {
				if rt := m.getRuntime(taskID); rt != nil && string(rt.instVersion) != o.Token {
					continue
				}
			}
			if _, eerr := m.writeEnvSnapshot(ctx, taskID, sql.NullString{}); eerr != nil {
				remaining = append(remaining, o)
			}
		}
	}
	_ = terminalRef
	return remaining
}

// recordResidualNoticeFromDisposition 按 KillResult 完整处置表记 notice（B6/G4-4
// 同款语义，Phase 5 技术债 a）：复用 classifyKillResult——仅一致 clean 不记；
// snapshot_missing_degraded 记 non-retryable；kill/disposition 失败、未知 disposition
// 或 SessionKilled 矛盾一律 retryable kill_failed 显式记录（MUST NOT 静默返回 nil，
// 否则调用方可能留下无 notice/debt 索引的存活进程）。返回 recordResidualNotice 的
// error（不静默）。
func (m *Manager) recordResidualNoticeFromDisposition(ctx context.Context, taskID, sessionName string, res process.KillResult) error {
	cls := classifyKillResult(res)
	if cls.action == "none" {
		return nil
	}
	return m.recordResidualNotice(ctx, taskID, sessionName, res.CleanupTickets, cls.reason, cls.retryable)
}

// reapLedgerSnapshot 返回任务的受管登记快照（测试观测面）。
func (m *Manager) reapLedgerSnapshot(taskID string) []suspendFinishObligation {
	m.reapLedgerMu.Lock()
	defer m.reapLedgerMu.Unlock()
	return append([]suspendFinishObligation(nil), m.reapLedger[taskID]...)
}

// reapLedgerHasUnfinished 报告任务是否仍有未完成义务（Round8 I17：登记非空
// 期间意图是唯一重试入口，不得清除）。
func (m *Manager) reapLedgerHasUnfinished(taskID string) bool {
	m.reapLedgerMu.Lock()
	defer m.reapLedgerMu.Unlock()
	return len(m.reapLedger[taskID]) > 0
}

// reapLedgerTaskIDs 返回登记中的全部任务 ID（Round8 I17：后台调度独立覆盖——
// 即使任务 notice 已为空，ledger 义务仍获得调度）。
func (m *Manager) reapLedgerTaskIDs() []string {
	m.reapLedgerMu.Lock()
	defer m.reapLedgerMu.Unlock()
	ids := make([]string, 0, len(m.reapLedger))
	for id := range m.reapLedger {
		ids = append(ids, id)
	}
	return ids
}

// registerReapObligations 登记任务的未完成持久化义务（Round7 I17：所有持久化
// 出口失败时的受管重试登记——义务含本次 kill 返回的完整 tickets，单元可能已删，
// 后续落账/收割 MUST 使用原 tickets，不依赖再次 kill。Round8 I20：按任务分组，
// drain 只取当前任务条目）。
func (m *Manager) registerReapObligations(taskID string, obs []suspendFinishObligation) {
	if len(obs) == 0 {
		return
	}
	m.reapLedgerMu.Lock()
	m.reapLedger[taskID] = append(m.reapLedger[taskID], obs...)
	m.reapLedgerMu.Unlock()
}

// drainReapLedger 尝试落账「当前任务」的受管登记义务，返回仍未完成的（保留在
// 登记中）。Round8 I20：只取该任务条目，调用方在该任务锁内执行——杜绝跨任务
// 污染（A 的义务不得按 B 身份执行）。
func (m *Manager) drainReapLedger(ctx context.Context, taskID string) {
	m.reapLedgerMu.Lock()
	pending := m.reapLedger[taskID]
	delete(m.reapLedger, taskID)
	m.reapLedgerMu.Unlock()
	if len(pending) == 0 {
		return
	}
	remaining := m.persistSuspendFinishObligations(ctx, taskID, "", pending)
	if len(remaining) > 0 {
		m.reapLedgerMu.Lock()
		m.reapLedger[taskID] = append(remaining, m.reapLedger[taskID]...)
		m.reapLedgerMu.Unlock()
	}
}

// flushReapLedger 关停收尾：逐任务 best-effort 落账受管登记，返回仍失败的
// 任务 ID（Round8 I17：关停尝试 flush 并报告剩余失败）。
func (m *Manager) flushReapLedger(ctx context.Context) []string {
	var failed []string
	for _, id := range m.reapLedgerTaskIDs() {
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		m.drainReapLedger(fctx, id)
		fcancel()
		if m.reapLedgerHasUnfinished(id) {
			failed = append(failed, id)
		}
	}
	return failed
}

// retrySuspendStopIntent 消费停止收敛意图（评审 Round3 I2；调用方持任务锁）：
// 复核（suspended=已收敛/非 active=意图过时/新一代 runtime 在位=旧意图失效——
// 三者清除意图且不影响现状）→ 复用正常挂起收尾语义完成清关联+残留 notice/tickets
// 持久化+env 快照清除（Round4 I17）→ active→suspended 条件收敛。Round5 I17：
// 未完成的持久化义务（notice/env 写失败）以 pending 形态随条目保留（updated
// 返回给外层 CAS 持久化）——done=false 不清意图、完整 tickets 不丢；下轮重放
// 优先补全义务（与状态无关——suspended 已收敛也不得丢弃 tickets 重放能力）。
// 返回 done（true=意图已了结可清除）、更新后的条目与错误。
func (m *Manager) retrySuspendStopIntent(ctx context.Context, taskID string, e noticeEntry) (bool, noticeEntry, error) {
	terminalRef, _ := e.Data["sessionName"].(string)
	// Round7 I17：先落账受管登记的义务（存储恢复后优先落账原 tickets——单元
	// 可能已删，不能依赖再次 kill 重新派生）。
	m.drainReapLedger(ctx, taskID)
	// Round8 I17：登记仍有未完成义务（本轮 drain 失败）——意图是唯一重试入口，
	// 不得清除（下轮继续调度落账）。
	if m.reapLedgerHasUnfinished(taskID) {
		return false, e, fmt.Errorf("suspend stop intent: ledger obligations unfinished")
	}
	// Round5 I17：上一轮遗留的未完成持久化义务优先补全（与状态无关——suspended
	// 已收敛也不得丢弃 tickets 重放能力）。
	if pending := decodeSuspendFinishObligations(e); len(pending) > 0 {
		remaining := m.persistSuspendFinishObligations(ctx, taskID, terminalRef, pending)
		e = annotateSuspendStopPending(e, remaining)
		if len(remaining) > 0 {
			return false, e, fmt.Errorf("suspend stop intent: %d obligation(s) still unpersisted", len(remaining))
		}
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return false, e, fmt.Errorf("suspend stop intent: reread task: %w", err)
	}
	if row.Status != StatusActive {
		// suspended=已收敛；activating/其他=用户新一代动作已接管——意图过时清除
		//（守卫语义：MUST NOT 覆盖其他代的状态事实；未完成义务已在上方补全）。
		return true, e, nil
	}
	token, _ := e.Data["token"].(string)
	if rt := m.getRuntime(taskID); rt != nil {
		// Round4 I2：注册表存在 runtime 即与记录代际严格比对——记录时空 token
		//（记录时无 runtime）不得授权清理后来出现的 runtime（无可验证身份→视为
		// 新代，意图过时清除）。
		if token == "" || string(rt.instVersion) != token {
			return true, e, nil
		}
	}
	// 完成挂起的本地半程（上游停止已确认属实）：清运行时关联 + 幂等回收 v2 本地
	// 单元（TUI/shell）——收集完整结果（含 shell 枚举错误），复用正常挂起收尾
	// 语义持久化残留 notice/tickets + 清 env 快照（Round4 I17：MUST NOT 丢弃
	// 清理债务、MUST NOT 残留 env 快照）。
	m.clearRuntime(taskID)
	var results []killResultEntry
	if terminalRef != "" {
		results = append(results, m.killTaskSessions(ctx, taskID, []string{terminalRef})...)
	}
	shells, serr := m.listShellSessions(taskID)
	if serr != nil {
		// 枚举失败如实入结果集（finishSuspendCommon 记 retryable kill_failed）。
		results = append(results, killResultEntry{name: terminalRef,
			killErr: fmt.Errorf("list shells: %w", serr)})
	} else {
		results = append(results, m.killTaskSessions(ctx, taskID, shells)...)
	}
	le, ferrs, obligations := m.finishSuspendCommon(ctx, taskID, results, nil)
	if joined := errors.Join(ferrs...); joined != nil && !le.Valid {
		le = sql.NullString{String: joined.Error(), Valid: true}
	}
	if len(obligations) > 0 {
		// Round7 I17：义务先交受管登记（进程内有生命周期的重试载荷——任何持久化
		// 出口失败都不至于让完整 tickets 只存在于本次调用返回值）。Round8 I20：
		// 义务携带权威 taskID；env 义务携带记录时执行代际（新一代运行不受旧
		// env 义务影响——对齐 I2 token 机制）。
		token, _ := e.Data["token"].(string)
		for i := range obligations {
			obligations[i].TaskID = taskID
			if obligations[i].Kind == "env" {
				obligations[i].Token = token
			}
		}
		m.registerReapObligations(taskID, obligations)
	}
	// Round6 I17：状态收敛与完整 pending 载荷同一持久化提交边界——义务标注
	// 先于状态收敛落库。标注失败 → 不收敛（保持 active：tickets 由 kill 结果
	// 下一轮重新派生，零丢失）；标注成功 → 收敛；收敛写失败 → 意图已携带
	// pending（active 态），下轮先补义务再收敛。消除「已 suspended 而义务载荷
	// 仅存在于本次调用返回值」的丢失窗口。
	if len(obligations) > 0 {
		e = annotateSuspendStopPending(e, obligations)
		if aerr := m.persistAnnotatedIntent(ctx, taskID, e); aerr != nil {
			return false, e, fmt.Errorf("suspend stop intent: pending annotate: %w", aerr)
		}
	}
	if _, werr := m.writeStatusConditional(ctx, taskID, StatusActive, StatusSuspended, le); werr != nil {
		// 意图已携带 pending（若收敛前段有义务）：active 态下轮先补义务再收敛。
		return false, e, fmt.Errorf("suspend stop intent: converge: %w", werr)
	}
	if len(obligations) > 0 {
		// Round6 I17：收敛完成但义务（已随意图 pending 持久化）未完成——意图
		// 保留，下轮 pending 分支补全后才清除（不得随 done 丢弃 pending）。
		return false, e, errors.Join(ferrs...)
	}
	// Matched=收敛完成且无未完成义务；!Matched=状态已被其他代改写（守卫不
	// 覆盖）——意图均了结。
	return true, e, nil
}

// persistAnnotatedIntent 把标注后的停止收敛意图 CAS 落库（Round6 I17：义务标注
// 与状态收敛的提交边界——标注未落库时调用方 MUST NOT 收敛状态）。
func (m *Manager) persistAnnotatedIntent(ctx context.Context, taskID string, annotated noticeEntry) error {
	key := noticeKey(annotated)
	for attempt := 0; attempt < 8; attempt++ {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("persist annotated intent: get task: %w", err)
		}
		entries, perr := parseNotices(row.Notice)
		if perr != nil {
			return fmt.Errorf("persist annotated intent: notice json corrupted (task %s): %w", taskID, perr)
		}
		found := false
		for i, cur := range entries {
			if noticeKey(cur) != key {
				continue
			}
			entries[i] = annotated
			found = true
			break
		}
		if !found {
			return fmt.Errorf("persist annotated intent: intent vanished (task %s)", taskID)
		}
		r, _ := m.writeNoticeCAS(ctx, taskID, row.Notice, encodeNotices(entries))
		if r.Matched {
			return nil
		}
	}
	return fmt.Errorf("persist annotated intent: CAS did not converge (task %s)", taskID)
}

// annotateSuspendStopPending 把未完成持久化义务以 pendingJSON 形态写入条目副本
// （obs 空=清除标记）。条目的持久化由外层 retryTaskNotices 的 CAS 承担。
func annotateSuspendStopPending(e noticeEntry, obs []suspendFinishObligation) noticeEntry {
	data := map[string]interface{}{}
	for k, v := range e.Data {
		if k != suspendFinishPendingKey {
			data[k] = v
		}
	}
	if raw := encodeSuspendFinishObligations(obs); raw != "" {
		data[suspendFinishPendingKey] = raw
	}
	c := e
	c.Data = data
	return c
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
			// Round8 I17：受管义务登记的任务即使 notice 已为空也保有调度入口
			//（retryTaskNotices 头部在该任务锁内 drain——不无锁跨任务全局取走）。
			if !m.reapLedgerHasUnfinished(t.ID) {
				continue
			}
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

	// Round7 I17：受管义务登记优先落账（含意图已清但义务尚在登记的边角）。
	m.drainReapLedger(ctx, t.ID)

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
		if e.Code == noticeCodeSuspendStop {
			// 停止收敛意图（评审 Round3 I2）：上游已停+收敛写曾失败——持任务锁
			// 复核后完成清关联+状态收敛（suspended）再删意图；旧意图不得影响
			// 新一代运行（执行代际守卫）。done=false（DB 仍不可用/义务未完成）
			// 保留重试——updated 条目（含 Round5 I17 pending 义务标注）随外层
			// CAS 持久化。
			done, updated, herr := m.retrySuspendStopIntent(ctx, t.ID, e)
			if herr != nil {
				remaining = append(remaining, updated)
				errs = append(errs, herr)
				continue
			}
			if !done {
				remaining = append(remaining, updated)
				continue
			}
			clearedKeys[noticeKey(updated)] = true
			continue
		}
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
				cls := classifyKillResult(res)
				if cls.action != "none" {
					// 仍失败：合并新 tickets，按 classifyKillResult 完整表更新
					// reason/retryable（未知/矛盾一律 retryable kill_failed，不得经
					// dispositionToNotice 吞掉 ok=false 当成功清债）。
					tickets = append(tickets, res.CleanupTickets...)
					e.Data["cleanupTickets"] = tickets
					e.Data["reason"] = cls.reason
					e.Data["retryable"] = cls.retryable
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
