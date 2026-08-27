package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- Fix 1: notice 身份/去重按会话（snapshot_failed 转 degraded 时旧项消失、不膨胀） ---

// TestP4_RetryTaskNotices_SnapshotFailedConvert_ReplacesNotDuplicates 验证 P4 阻塞 1：
// snapshot_failed 历史会话在重试前自行消失 → 转 snapshot_missing_degraded 时，旧 retryable 项
// MUST 消失（按会话去重替换，非追加），仅剩一条 degraded（retryable=false）。连续两轮重试
// 幂等不变（不每轮追加新 degraded 膨胀）。
func TestP4_RetryTaskNotices_SnapshotFailedConvert_ReplacesNotDuplicates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": serveSessionName("t1"), "reason": noticeReasonSnapshotFailed, "retryable": true,
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc() // serve 不存在（已消失）
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	runOnce := func() {
		row, _ := store.GetTask(context.Background(), "t1")
		entries, _ := parseNotices(row.Notice)
		if err := m.retryTaskNotices(context.Background(), row, entries); err != nil {
			t.Fatalf("retryTaskNotices: %v", err)
		}
	}

	runOnce()
	// 第一轮：仅剩一条 degraded，旧 retryable snapshot_failed 消失。
	finalRow, _ := store.GetTask(context.Background(), "t1")
	finalEntries, _ := parseNotices(finalRow.Notice)
	if n := countResidualBySession(finalEntries, serveSessionName("t1")); n != 1 {
		t.Fatalf("round1: got %d residual entries for serve session, want exactly 1 (replacement not duplicate); entries=%+v", n, finalEntries)
	}
	if hasRetryableSnapshotFailed(finalEntries) {
		t.Errorf("round1: old retryable snapshot_failed must be gone after conversion; entries=%+v", finalEntries)
	}
	if !hasDegradedForSession(finalEntries, serveSessionName("t1")) {
		t.Errorf("round1: want one snapshot_missing_degraded (retryable=false); entries=%+v", finalEntries)
	}

	// 第二轮：degraded 不可重试 → 跳过，状态幂等不变（不追加新 degraded）。
	runOnce()
	row2, _ := store.GetTask(context.Background(), "t1")
	entries2, _ := parseNotices(row2.Notice)
	if n := countResidualBySession(entries2, serveSessionName("t1")); n != 1 {
		t.Errorf("round2: got %d residual entries for serve session, want still 1 (idempotent, no inflation); entries=%+v", n, entries2)
	}
	if hasRetryableSnapshotFailed(entries2) {
		t.Errorf("round2: retryable snapshot_failed must not reappear; entries=%+v", entries2)
	}
}

// countResidualBySession 统计指定会话的 residual notice 项数（断言唯一性）。
func countResidualBySession(entries []noticeEntry, sessionName string) int {
	n := 0
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if sn, _ := e.Data["sessionName"].(string); sn == sessionName {
			n++
		}
	}
	return n
}

func hasRetryableSnapshotFailed(entries []noticeEntry) bool {
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if r, _ := e.Data["reason"].(string); r == noticeReasonSnapshotFailed {
			if retry, _ := e.Data["retryable"].(bool); retry {
				return true
			}
		}
	}
	return false
}

func hasDegradedForSession(entries []noticeEntry, sessionName string) bool {
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if sn, _ := e.Data["sessionName"].(string); sn != sessionName {
			continue
		}
		if r, _ := e.Data["reason"].(string); r == noticeReasonSnapshotMissing {
			if retry, _ := e.Data["retryable"].(bool); !retry {
				return true
			}
		}
	}
	return false
}

// TestP4_RecordResidualNotice_SameSessionReplacesNotDuplicates 验证 P4 阻塞 1：
// 同一会话多次 recordResidualNotice MUST 按会话替换（保留最新 reason），但 tickets MUST union
//（旧 + 新去重合并，不 latest-wins 丢失未收割旧 tickets，design.md §5）。
func TestP4_RecordResidualNotice_SameSessionReplacesNotDuplicates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	ctx := context.Background()
	_ = m.recordResidualNotice(ctx, "t1", serveSessionName("t1"), []string{"tk1"}, noticeReasonSnapshotFailed, true)
	_ = m.recordResidualNotice(ctx, "t1", serveSessionName("t1"), []string{"tk2"}, noticeReasonKillFailed, true)

	row, _ := store.GetTask(ctx, "t1")
	entries, _ := parseNotices(row.Notice)
	if n := countResidualBySession(entries, serveSessionName("t1")); n != 1 {
		t.Fatalf("got %d residual entries for same session, want 1 (replace not duplicate); entries=%+v", n, entries)
	}
	// 最新 reason 应保留（kill_failed），但 tickets MUST union（tk1 + tk2 俱在）。
	var got noticeEntry
	for _, e := range entries {
		if e.Code == noticeCodeResidual {
			if sn, _ := e.Data["sessionName"].(string); sn == serveSessionName("t1") {
				got = e
				break
			}
		}
	}
	if r, _ := got.Data["reason"].(string); r != noticeReasonKillFailed {
		t.Errorf("reason=%s want %s (latest reason wins)", r, noticeReasonKillFailed)
	}
	tks := noticeTickets(got)
	if !containsTicket(tks, "tk1") || !containsTicket(tks, "tk2") {
		t.Errorf("tickets=%v MUST contain both tk1 and tk2 (union, not latest-wins)", tks)
	}
	if len(tks) != 2 {
		t.Errorf("tickets=%v want exactly 2 (tk1+tk2 deduped union)", tks)
	}
}

// containsTicket 判断 tickets 切片是否含指定 ticket。
func containsTicket(tks []string, want string) bool {
	for _, tk := range tks {
		if tk == want {
			return true
		}
	}
	return false
}

// TestP4_RecordResidualNotice_DifferentSessionsCoexist 验证不同会话的 notice 互不替换（各自保留）。
func TestP4_RecordResidualNotice_DifferentSessionsCoexist(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	ctx := context.Background()
	_ = m.recordResidualNotice(ctx, "t1", serveSessionName("t1"), []string{"tk1"}, noticeReasonKillFailed, true)
	_ = m.recordResidualNotice(ctx, "t1", tuiSessionName("t1"), []string{"tk2"}, noticeReasonKillFailed, true)

	row, _ := store.GetTask(ctx, "t1")
	entries, _ := parseNotices(row.Notice)
	if countResidualBySession(entries, serveSessionName("t1")) != 1 {
		t.Error("serve session notice missing")
	}
	if countResidualBySession(entries, tuiSessionName("t1")) != 1 {
		t.Error("tui session notice missing")
	}
}

// suppress unused import in incremental file growth
var _ = process.DispositionClean
var _ = strings.Contains
var _ = errors.New

// --- Fix 2: forceKillAll/killResidualSessions/retryOrphanSessions fail-closed ---

// TestP4_SuspendForceKillAll_HasSessionInfraError_FormsRetryableDebt 验证 P4-2：Suspend
// infra 错误路径 forceKillAll 中 HasSession infra 错误 MUST 收集为 killErr entry → finishSuspend
// 记 retryable notice（形成可重试 debt），不得吞错（design.md §5/§8）。
// 构造：serve HasSession 返回 infra 错误 → suspendRun 走 infra 路径 forceKillAll；
// forceKillAll 对 tui/serve 的 HasSession 同样 infra 错误 → MUST 收集 killErr → notice 记录。
func TestP4_SuspendForceKillAll_HasSessionInfraError_FormsRetryableDebt(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	// 全局 HasSession 返回 infra 错误：suspendRun serve 探测 + forceKillAll 逐会话均命中。
	proc.hasSessionErr = errors.New("tmux infra: has session failed")
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must return error for infra failure")
	}
	assertStatus(t, tStore, "t1", StatusSuspended)
	// MUST 记 retryable notice（forceKillAll killErr entries → finishSuspend 记 notice）。
	row, _ := tStore.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notices: %v", perr)
	}
	if len(entries) == 0 {
		t.Fatal("forceKillAll HasSession infra error MUST form retryable debt (notice); got empty")
	}
	// 至少一项 retryable=true 的 residual notice。
	foundRetryable := false
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if r, ok := e.Data["retryable"].(bool); ok && r {
			foundRetryable = true
			break
		}
	}
	if !foundRetryable {
		t.Error("notice MUST contain retryable=true residual entry for forceKillAll infra failure")
	}
}

// TestP4_DeleteKillResidualSessions_HasSessionInfraError_FailsClosed 验证 P4-2：Delete
// killResidualSessions 中 HasSession infra 错误 MUST 返回错误落 deletion_failed，不得吞错
// 当 absent 继续删 worktree/DB（design.md §5/§8/§19）。
func TestP4_DeleteKillResidualSessions_HasSessionInfraError_FailsClosed(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[serveSessionName("t1")] = true
	// 仅对 serve 会话的 HasSession 返回 infra 错误（killResidualSessions 最后杀 serve 时命中）。
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		serveSessionName("t1"): errors.New("tmux infra: has serve failed"),
	}, nil)
	m := newR7TestManager(t, tStore, wrapped, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("Delete must fail when killResidualSessions HasSession infra error")
	}
	// MUST 落 deletion_failed（不得越过 DB 提交点删行）。
	assertStatus(t, tStore, "t1", StatusDeletionFailed)
	assertTaskExists(t, tStore, "t1")
}

// TestP4_RetryOrphanSessions_HasSessionInfraError_ConservativeUnconverged 验证 P4-2：
// retryOrphanSessions 中 HasSession infra 错误 MUST 保守视为存活（未收敛），不得当 absent
// 跳过 kill 致 tickets 误判干净（design.md §5/§10）。
func TestP4_RetryOrphanSessions_HasSessionInfraError_ConservativeUnconverged(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	// 预置内存 orphanFailure（会话仍存活 + 有 tickets）。
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-1"},
	})
	m.orphanMu.Unlock()
	// RetryReap 成功（返回空），但 HasSession 对该会话返回 infra 错误 → MUST 保守视为存活。
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		"ocdeck-ghost-serve": errors.New("tmux infra: has session failed"),
	}, nil)
	m.proc = wrapped

	m.retryOrphanSessions(context.Background())
	// HasSession infra 错误保守视为存活 → 尝试 kill（mock 成功 clean）→ 但 hasSessionErr 阻断
	// 路径：infra 错误时 alive=true → kill 成功 → reapRemaining 空 → 应收敛清空。
	// 关键：不得因 HasSession infra 错误当 absent 跳过 kill 致逃逸进程误判干净。
	// 此处 mock kill 成功所以收敛；若 kill 也失败则保留——下面单独验证 HasSession infra 且 kill 失败。
	m.orphanMu.Lock()
	remaining := len(m.orphanFailures)
	m.orphanMu.Unlock()
	if remaining != 0 {
		// mock kill 成功所以应清空；若未清空说明 HasSession infra 错误路径未正确处理存活判定。
		// 实际：HasSession infra 错误→alive=true→KillSession mock 成功 clean→reapRemaining 空→收敛。
		t.Errorf("orphan should converge after mock kill; remaining=%d", remaining)
	}
}

// TestP4_RetryOrphanSessions_HasSessionInfraAndKillFails_PreservesTickets 验证 P4-2：
// HasSession infra 错误保守视为存活 + kill 失败时 tickets MUST 保留进下轮重试（不丢弃）。
func TestP4_RetryOrphanSessions_HasSessionInfraAndKillFails_PreservesTickets(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-keep"},
	})
	m.orphanMu.Unlock()
	// HasSession infra 错误（保守存活）+ KillSession infra 错误 → tickets 保留。
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		"ocdeck-ghost-serve": errors.New("tmux infra: has session failed"),
	}, nil)
	m.proc = wrapProcKill(wrapped, map[string]error{
		"ocdeck-ghost-serve": errors.New("tmux infra: kill failed"),
	})

	m.retryOrphanSessions(context.Background())
	m.orphanMu.Lock()
	remaining := m.orphanFailures
	m.orphanMu.Unlock()
	if len(remaining) != 1 {
		t.Fatalf("orphan MUST be preserved when HasSession infra + kill fails; got %d", len(remaining))
	}
	if len(remaining[0].tickets) == 0 {
		t.Error("preserved orphan MUST retain tickets for next-round reap")
	}
}

// --- Fix 4: orphan ticket 持久化跨重启恢复 ---

// TestP4_RestoreCleanupDebts_ReconcileRestoresOrphanTickets 验证 P4-4：进程退出再启动后
// Reconcile 从 cleanup_debts 表恢复未收敛 orphan tickets 到内存 orphanFailures（design.md §10）。
func TestP4_RestoreCleanupDebts_ReconcileRestoresOrphanTickets(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	debt := newMemCleanupDebtStore()
	// 模拟上一进程退出前持久化的未收敛 orphan tickets。
	debt.rows["ocdeck-ghost-serve"] = memDebtRow{tickets: `["tk-persist-1","tk-persist-2"]`, createdAt: 1000}
	proc := newMockProc()
	m := newR7TestManagerWithDebt(t, tStore, debt, proc, newMockWorktree(), newMockOC(true))

	// Reconcile 首步骤 restoreCleanupDebts → 内存 orphanFailures 应含恢复的 tickets。
	m.restoreCleanupDebts(context.Background())
	m.orphanMu.Lock()
	restored := m.orphanFailures
	m.orphanMu.Unlock()
	if len(restored) != 1 {
		t.Fatalf("restore orphan debts: got %d failures, want 1", len(restored))
	}
	if restored[0].sessionName != "ocdeck-ghost-serve" {
		t.Errorf("sessionName=%s want ocdeck-ghost-serve", restored[0].sessionName)
	}
	if len(restored[0].tickets) != 2 {
		t.Errorf("restored tickets=%v want 2", restored[0].tickets)
	}
}

// TestP4_PersistOrphanDebts_UpsertsUnconvergedDeletesConverged 验证 P4-4：retryOrphanSessions
// 后未收敛 tickets 持久化到 cleanup_debts，收敛的从表删除（design.md §10）。
func TestP4_PersistOrphanDebts_UpsertsUnconvergedDeletesConverged(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	debt := newMemCleanupDebtStore()
	// 表中已有已收敛的旧 debt（内存已无）→ 应删除。
	debt.rows["ocdeck-converged-serve"] = memDebtRow{tickets: `["old"]`, createdAt: 1}
	proc := newMockProc()
	m := newR7TestManagerWithDebt(t, tStore, debt, proc, newMockWorktree(), newMockOC(true))
	// 内存中未收敛的 orphan（kill 持续失败，tickets 保留）。
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-unconverged"},
	})
	m.orphanMu.Unlock()
	// 用 reapFailingWrapper 让 RetryReap 返回 remaining + HasSession 存活 + kill 失败 → 保留。
	m.proc = wrapProcKill(wrapReapFailing(proc), map[string]error{
		"ocdeck-ghost-serve": errors.New("kill failed"),
	})

	m.retryOrphanSessions(context.Background())
	// 持久化：未收敛的 upsert，已收敛的删除。
	if debt.count() != 1 {
		t.Fatalf("cleanup_debts count=%d want 1 (unconverged kept, converged deleted)", debt.count())
	}
	debt.mu.Lock()
	row, ok := debt.rows["ocdeck-ghost-serve"]
	debt.mu.Unlock()
	if !ok {
		t.Fatal("unconverged orphan debt MUST be persisted")
	}
	if !strings.Contains(row.tickets, "tk-unconverged") {
		t.Errorf("persisted tickets=%s must contain tk-unconverged", row.tickets)
	}
	if _, ok := debt.rows["ocdeck-converged-serve"]; ok {
		t.Error("converged orphan debt MUST be deleted from cleanup_debts")
	}
}

// --- Fix 6: handleSSEEvent/startTUI 错误传播 ---

// TestP4_HandleSSEEvent_StoreError_ConvergesToSuspended 验证 P4-6：handleSSEEvent 中
// session 落库（UpsertTaskSession/DeleteTaskSession）错误 MUST 收敛运行时（convergeToSuspended），
// 不得 _ = 丢弃致会话归属丢失（design.md §4/§19 failpoint 表）。
func TestP4_HandleSSEEvent_StoreError_ConvergesToSuspended(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	// 预置一条已归属 session（startTUI 走 GetSession 预检路径，不触发 upsert），
	// 使 upsert 错误仅作用于 SSE 实时事件落库路径。
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-anchor", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1,
	})
	// 注入 UpsertTaskSession 错误（session 落库失败）。
	errStore := wrapSessionStoreErr(tStore, errors.New("db: upsert failed"), nil, nil)
	// 用自定义 OC 发送 session.created 事件后阻塞，使 onEvent 实时路径命中 upsert 错误。
	oc := &eventSendOC{mockOC: newMockOC(true), events: []opencode.Event{makeEventWithDir("session.created", "sess-err", 1, "/data/worktrees/p1/t1")}}
	oc.sessions = []opencode.Session{{ID: "sess-anchor", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	m := newR7TestManager(t, errStore, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Activate 后 SSE 已起；实时事件 session.created 触发 upsert 错误 → convergeToSuspended。
	// 等待收敛（异步 goroutine）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, _ := tStore.GetTask(context.Background(), "t1")
		if row.Status == StatusSuspended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertStatus(t, tStore, "t1", StatusSuspended)
	lastErrorContains(t, tStore, "t1", "session store error")
}

// TestP4_StartTUI_ListTaskSessionsError_Propagates 验证 P4-6：startTUI 中
// ListTopLevelTaskSessions 错误 MUST 传播（Activate 失败），不得当空集继续创建/锚定
// 致 session 归属丢失（design.md §19）。B3：resolveAnchorSession 走顶层会话查询。
// eventSendOC 发送预置事件后阻塞（用于 handleSSEEvent 错误传播测试）。
type eventSendOC struct {
	*mockOC
	events []opencode.Event
}

func (c *eventSendOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.mockOC.onReadyCh != nil {
		// 触发 onReady（startSSE 首次对齐屏障）。
		select {
		case c.mockOC.onReadyCh <- struct{}{}:
		default:
		}
	}
	// onReady 后短暂延迟（让首次 align 完成、buffering=false），再发实时事件触发 onEvent。
	go func() {
		time.Sleep(100 * time.Millisecond)
		for _, ev := range c.events {
			onEvent(ev)
		}
	}()
	<-ctx.Done()
	return ctx.Err()
}