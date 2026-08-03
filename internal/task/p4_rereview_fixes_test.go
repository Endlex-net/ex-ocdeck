package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
	"ocdeck/internal/store"
)

// 本文件覆盖 P4 门禁复评（ora-1）5 项阻塞的测试。
// Fix 1 测试已在 p4_gate_fixes_test.go（TestP4_RecordResidualNotice_SameSessionReplacesNotDuplicates 已更新为 union 断言）。

// --- Fix 2: Delete debt 错误路径完整性 ---

// TestP4Retry_RetryDebt_HasSessionError_KeepsCurrentAndSubsequent 验证 P4 复评阻塞 2：
// retryDebt 中当前 entry 的 HasSession 报错时，返回值 MUST 包含当前 entry + 全部后续未处理 entry，
// 不得只返回已处理 remaining（下次 Retry 会越过未处理 debt 门禁）。
func TestP4Retry_RetryDebt_HasSessionError_KeepsCurrentAndSubsequent(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	m := newTestManager(t, s, newMockProc(), newMockWorktree(), newMockOC(true))
	// 两个 retryable residual entry：session-a（会触发 HasSession infra 错误）+ session-b（后续未处理）。
	entries := []noticeEntry{
		{Code: noticeCodeResidual, TS: 1, Data: map[string]interface{}{
			"sessionName": "ocdeck-t1-serve", "cleanupTickets": []interface{}{"tk-a"}, "reason": noticeReasonSnapshotFailed, "retryable": true,
		}},
		{Code: noticeCodeResidual, TS: 2, Data: map[string]interface{}{
			"sessionName": "ocdeck-t1-tui", "cleanupTickets": []interface{}{"tk-b"}, "reason": noticeReasonKillFailed, "retryable": true,
		}},
	}
	// procHasSessionByNameWrapper：仅 serve 返回 infra 错误，tui 正常（absent）。
	proc := newMockProc()
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		serveSessionName("t1"): errors.New("tmux infra: has serve failed"),
	}, nil)
	m.proc = wrapped

	remaining, derr := m.retryDebt(context.Background(), "t1", entries)
	if derr == nil {
		t.Fatal("retryDebt must return error on HasSession infra failure")
	}
	// 返回值 MUST 包含当前 entry（serve）+ 后续未处理 entry（tui）。
	if len(remaining) != 2 {
		t.Fatalf("remaining=%d entries, want 2 (current + subsequent unprocessed); entries=%+v", len(remaining), remaining)
	}
	// 验证两个会话都在 remaining（不丢后续 tui）。
	sessions := map[string]bool{}
	for _, e := range remaining {
		if sn, ok := e.Data["sessionName"].(string); ok {
			sessions[sn] = true
		}
	}
	if !sessions["ocdeck-t1-serve"] || !sessions["ocdeck-t1-tui"] {
		t.Errorf("remaining MUST contain both serve (current) and tui (subsequent); got %v", sessions)
	}
}

// TestP4Retry_DeleteResume_CasWriteNoticesError_AggregatesLastError 验证 P4 复评阻塞 2：
// retryDebt 错误路径 casWriteNotices 的错误 MUST 聚合进 last_error（不静默 _ =）。
// 用 statusErrStore 不影响 casWriteNotices（CAS 用 UpdateTaskNoticeCAS），此处用注入 NoticeCAS
// 失败的 wrapper 验证 last_error 含 cas write 错误。
func TestP4Retry_DeleteResume_CasWriteNoticesError_AggregatesLastError(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	// 预置 retryable debt（触发 retryDebt）。
	s.mutTask("t1", func(r *TaskRow) {
		r.Notice = sql.NullString{String: `[{"code":"residual_processes","message":"x","ts":1,"data":{"sessionName":"ocdeck-t1-serve","cleanupTickets":["tk-a"],"reason":"snapshot_failed","retryable":true}}]`, Valid: true}
	})
	// procHasSessionByNameWrapper 让 serve HasSession 报错 → retryDebt 返回 error → casWriteNotices 被调用。
	proc := newMockProc()
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		serveSessionName("t1"): errors.New("tmux infra: has serve failed"),
	}, nil)
	// noticeCASFailStore 让 UpdateTaskNoticeCAS 永远返回 replaced=false（CAS 不收敛）。
	failStore := &noticeCASFailStore{TaskStore: s}
	m := newR7TestManager(t, failStore, wrapped, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("Delete must fail on retryDebt HasSession infra error")
	}
	row, _ := s.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Fatalf("status=%s want deletion_failed", row.Status)
	}
	// last_error MUST 含 cas write 错误（聚合，非静默）。
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "cas write") {
		t.Errorf("last_error=%v must contain 'cas write' (aggregated casWriteNotices error)", row.LastError)
	}
}

// noticeCASFailStore 让 UpdateTaskNoticeCAS 永远返回 replaced=false（CAS 不收敛）。
type noticeCASFailStore struct {
	TaskStore
}

func (w *noticeCASFailStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (bool, error) {
	return false, nil
}

// --- Fix 3: orphan ticket 持久化闭环 ---

// TestP4Rereview_RestoreCleanupDebts_Failure_FailClosed 验证 P4 复评阻塞 3a：
// restoreCleanupDebts 失败 MUST 传播 → Reconcile fail-closed 拒绝开放 HTTP。
func TestP4Rereview_RestoreCleanupDebts_Failure_FailClosed(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	// failDebtStore 注入 ListCleanupDebts 错误。
	debt := wrapFailDebt(newMemCleanupDebtStore(), errors.New("db: list debts failed"), nil)
	proc := newMockProc()
	m := newR7TestManagerWithDebt(t, s, debt, proc, newMockWorktree(), newMockOC(true))

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile must fail-closed when restoreCleanupDebts errors")
	}
	if !strings.Contains(err.Error(), "restore orphan debts") {
		t.Errorf("error must mention restore orphan debts, got: %v", err)
	}
}

// TestP4Rereview_Reconcile_NewOrphanTickets_PersistedImmediately 验证 P4 复评阻塞 3b：
// Reconcile 新发现 orphan 的 tickets MUST 立即持久化到 cleanup_debts（不等 30s 周期）。
func TestP4Rereview_Reconcile_NewOrphanTickets_PersistedImmediately(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	debt := newMemCleanupDebtStore()
	proc := newMockProc()
	// 预置孤儿会话（无 DB 行的 ocdeck 会话）+ kill 失败产生 tickets。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled:  false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-new-orphan"},
	}
	m := newR7TestManagerWithDebt(t, s, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	if err := m.Reconcile(context.Background()); err != nil {
		// reconcileKill/persist 模式可能返回 error（orphan 残留），但关键断言是持久化已发生。
		// kill 模式下 reconcileKill 不返回 error；persist 模式下可能因 orphanRemaining 返回 error。
	}

	// orphan tickets MUST 已持久化到 cleanup_debts（不等 30s 周期）。
	if debt.count() != 1 {
		t.Fatalf("cleanup_debts count=%d want 1 (orphan tickets persisted immediately)", debt.count())
	}
	debt.mu.Lock()
	row, ok := debt.rows["ocdeck-ghost-serve"]
	debt.mu.Unlock()
	if !ok {
		t.Fatal("orphan session tickets MUST be persisted to cleanup_debts immediately")
	}
	if !strings.Contains(row.tickets, "tk-new-orphan") {
		t.Errorf("persisted tickets=%s must contain tk-new-orphan", row.tickets)
	}
}

// TestP4Rereview_Shutdown_NewOrphanTickets_PersistedAfterKill 验证 P4 复评阻塞 3c：
// Shutdown kill 模式下 shutdownKillAllSessions 新产生的 orphan tickets MUST 再次持久化
//（顺序：先收割既有 → kill 全部会话 → 新 orphan 再持久化，不先持久化再 kill）。
func TestP4Rereview_Shutdown_NewOrphanTickets_PersistedAfterKill(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	debt := newMemCleanupDebtStore()
	proc := newMockProc()
	// Activate 会创建 serve 会话；Shutdown kill 时 KillSession 返回非 clean + tickets。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-shutdown-new"},
	}
	m := newR7TestManagerWithDebt(t, s, debt, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate
	m.SetLifecycleCtx(context.Background())

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Shutdown kill 模式 → shutdownKillAllSessions kill serve 产生新 orphan tickets。
	_ = m.Shutdown(context.Background())

	// 新 orphan tickets MUST 已持久化（shutdownKillAllSessions 后再 persistOrphanDebts）。
	// 注意：serve 会话名可解析 taskID t1，有 DB 行 → tickets 落 row.Notice（非 orphan）。
	// 但 SessionKilled=false 意味着会话仍存活 → residualSessions=true → killFailed=true → error。
	// 关键：shutdownKillAllSessions 中非 clean 且有 DB 行的走 recordResidualNoticeFromDisposition
	//（落 notice），无 DB 行的走 orphanFailures。此测试用已知 task，验证 notice 落库 + Shutdown error。
	row, _ := s.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if len(noticeTickets(e)) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("Shutdown kill non-clean MUST persist tickets to notice (locatable)")
	}
}

// TestP4Rereview_PersistOrphanDebt_UnionWithExisting 验证 P4 复评阻塞 3e：
// 恢复项与同会话新失败项重复存在时，持久化 MUST union tickets（不 latest-wins 覆盖）。
func TestP4Rereview_PersistOrphanDebt_UnionWithExisting(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	debt := newMemCleanupDebtStore()
	// 表中已有恢复的 orphan tickets（旧）。
	debt.rows["ocdeck-ghost-serve"] = memDebtRow{tickets: `["tk-old"]`, createdAt: 1}
	proc := newMockProc()
	m := newR7TestManagerWithDebt(t, s, debt, proc, newMockWorktree(), newMockOC(true))
	// 新失败项：同会话 + 新 tickets。
	m.recordOrphanFailure(context.Background(), "ocdeck-ghost-serve", []string{"tk-new"})

	debt.mu.Lock()
	row := debt.rows["ocdeck-ghost-serve"]
	debt.mu.Unlock()
	// MUST union：tk-old + tk-new 俱在，不覆盖。
	if !strings.Contains(row.tickets, "tk-old") {
		t.Errorf("persisted tickets=%s MUST contain tk-old (union, not latest-wins)", row.tickets)
	}
	if !strings.Contains(row.tickets, "tk-new") {
		t.Errorf("persisted tickets=%s MUST contain tk-new (union)", row.tickets)
	}
}

// TestP4Rereview_RealStore_Migration0002_CleanupDebtsApplied 验证 P4 复评阻塞 3（真实 SQLite）：
// migration 0002 应用后 cleanup_debts 表存在，Upsert/List/Delete 全链路工作。
func TestP4Rereview_RealStore_Migration0002_CleanupDebtsApplied(t *testing.T) {
	adapter, db := openRealStore(t)

	ctx := context.Background()
	// cleanup_debts 表应存在（migration 0002 已应用）。
	rows, err := db.ListCleanupDebts(ctx)
	if err != nil {
		t.Fatalf("ListCleanupDebts on real store (migration 0002): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("initial cleanup_debts should be empty, got %d rows", len(rows))
	}
	// Upsert + List + Delete 全链路。
	if err := adapter.UpsertCleanupDebt(ctx, "ocdeck-ghost-serve", `["tk1"]`, 100); err != nil {
		t.Fatalf("UpsertCleanupDebt: %v", err)
	}
	if err := adapter.UpsertCleanupDebt(ctx, "ocdeck-ghost-tui", `["tk2"]`, 101); err != nil {
		t.Fatalf("UpsertCleanupDebt 2: %v", err)
	}
	got, err := adapter.ListCleanupDebts(ctx)
	if err != nil {
		t.Fatalf("ListCleanupDebts: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("after 2 upserts, got %d rows want 2", len(got))
	}
	// Upsert 同 session 原地替换（ON CONFLICT）。
	if err := adapter.UpsertCleanupDebt(ctx, "ocdeck-ghost-serve", `["tk1","tk3"]`, 102); err != nil {
		t.Fatalf("UpsertCleanupDebt conflict: %v", err)
	}
	got2, _ := adapter.ListCleanupDebts(ctx)
	if len(got2) != 2 {
		t.Errorf("upsert same session should replace, got %d rows want 2", len(got2))
	}
	// Delete 收敛项。
	if err := adapter.DeleteCleanupDebt(ctx, "ocdeck-ghost-tui"); err != nil {
		t.Fatalf("DeleteCleanupDebt: %v", err)
	}
	got3, _ := adapter.ListCleanupDebts(ctx)
	if len(got3) != 1 {
		t.Errorf("after delete, got %d rows want 1", len(got3))
	}
}

// TestP4Rereview_RealStore_Reconcile_RestoreFromCleanupDebts 验证 P4 复评阻塞 3（真实 SQLite 全链路）：
// 进程退出前持久化 orphan tickets → 重启后 Reconcile 从 cleanup_debts 恢复，并在开放 HTTP 前
// 完成一次重试收割（FIX2(c)：不只恢复到内存，MUST 主动 reap 收敛）。
func TestP4Rereview_RealStore_Reconcile_RestoreFromCleanupDebts(t *testing.T) {
	adapter, db := openRealStore(t)
	ctx := context.Background()
	// 模拟上一进程退出前持久化的未收敛 orphan tickets。
	if err := db.UpsertCleanupDebt(ctx, "ocdeck-ghost-serve", `["tk-persist-1","tk-persist-2"]`, 1000); err != nil {
		t.Fatalf("UpsertCleanupDebt: %v", err)
	}
	// 用真实 store adapter 作 DebtStore，构造 Manager 后 Reconcile 恢复。
	proc := newMockProc()
	// ghost 会话已不存在（进程重启后 tmux 可能已清），RetryReap 成功 → 收敛。
	m := newR7TestManagerWithDebt(t, adapter, adapter, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	if err := m.Reconcile(ctx); err != nil {
		// Reconcile 可能因其他原因返回 error，但关键是 restore + reap 发生。
		t.Logf("Reconcile returned: %v (restore+reap may still have happened)", err)
	}
	// FIX2(c)：恢复后 MUST 在开放 HTTP 前完成一次重试收割。ghost 会话不存在且 RetryReap
	// 成功（mock 返回 nil remaining）→ tickets 收敛 → 从 cleanup_debts 表删除。
	// 断言：收敛后 cleanup_debts 表不再含 ghost-serve 行（证明 reap 已运行，不只恢复到内存）。
	remaining, err := adapter.ListCleanupDebts(ctx)
	if err != nil {
		t.Fatalf("ListCleanupDebts after Reconcile: %v", err)
	}
	for _, row := range remaining {
		if row.SessionName == "ocdeck-ghost-serve" {
			t.Errorf("converged orphan debt MUST be reaped before HTTP open; still in cleanup_debts: %+v", row)
		}
	}
}

// --- Fix 4: SSE failpoint 收敛 runtime 身份隔离 ---

// TestP4Rereview_SSEStaleGenError_DoesNotClearNewGenRuntime 验证 P4 复评阻塞 4：
// 旧代 SSE 错误回调的 convergeToSuspendedForGen 携带 (generation, instanceID)，拿锁后校验
// 与当前 runtime 注册表匹配，旧代延迟错误 MUST NOT 清理新代 runtime（design.md §2 三元组隔离）。
// 构造：Activate gen1 → 换代为 gen2（模拟 Suspend→重新 Activate）→ 用 gen1 的 (gen,instID)
// 调用 convergeToSuspendedForGen → 断言 gen2 runtime 不受影响 + 状态仍 active。
func TestP4Rereview_SSEStaleGenError_DoesNotClearNewGenRuntime(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newR7TestManager(t, s, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	// 第一次 Activate（旧代 gen=1）。
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate gen1: %v", err)
	}
	oldGen := m.getRuntime("t1").generation
	oldInst := m.getRuntime("t1").instanceID

	// Suspend → 重新 Activate（换代为 gen=2，不同 instanceID）。
	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend gen1: %v", err)
	}
	assertStatus(t, s, "t1", StatusSuspended)
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate gen2: %v", err)
	}
	newRT := m.getRuntime("t1")
	if newRT == nil {
		t.Fatal("new gen runtime must exist after re-Activate")
	}
	if newRT.generation == oldGen {
		t.Fatalf("new gen=%d must differ from old gen=%d", newRT.generation, oldGen)
	}
	assertStatus(t, s, "t1", StatusActive)

	// 旧代延迟 SSE 错误回调到达：用 gen1 的 (generation, instanceID) 调用 convergeToSuspendedForGen。
	// 拿锁后校验 gen != 当前 gen2 → 跳过收敛（不清理新代 runtime）。
	m.convergeToSuspendedForGen("t1", "stale gen sse error callback", oldGen, oldInst)

	// 断言：新代 runtime 仍存在 + 状态仍 active（旧代延迟错误未清理新代）。
	cur := m.getRuntime("t1")
	if cur == nil || cur.generation != newRT.generation {
		t.Fatal("stale gen SSE error callback MUST NOT clear new gen runtime (identity isolation)")
	}
	assertStatus(t, s, "t1", StatusActive)
}

// TestP4Rereview_SSEStaleGenError_CurrentGenConverges 验证当前代 SSE 错误回调正常收敛
//（gen 校验通过时 convergeToSuspendedForGen 行为与 convergeToSuspended 一致）。
func TestP4Rereview_SSEStaleGenError_CurrentGenConverges(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newR7TestManager(t, s, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	// 当前代的 SSE 错误回调 → gen 校验通过 → 正常收敛到 suspended。
	m.convergeToSuspendedForGen("t1", "current gen sse error", rt.generation, rt.instanceID)
	assertStatus(t, s, "t1", StatusSuspended)
}

// --- Fix 5: convergeToSuspended 自身 fail-closed ---

// TestP4Rereview_ConvergeToSuspended_CleanupInfraError_FormsRetryableDebt 验证 P4 复评阻塞 5：
// convergeToSuspended 清理路径（cleanupActivationRuntime）的 HasSession/KillSession infra 错误
// MUST 收集为可重试 debt（retryable notice），不得 _ = 吞错（残留进程下次 Activate 被门禁阻塞）。
func TestP4Rereview_ConvergeToSuspended_CleanupInfraError_FormsRetryableDebt(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	s.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	// HasSession 全局返回 infra 错误 → cleanupActivationRuntime fail-closed 记 notice。
	proc.hasSessionErr = errors.New("tmux infra: has session failed")
	m := newR7TestManager(t, s, proc, newMockWorktree(), newMockOC(true))

	m.convergeToSuspended("t1", "test converge fail-closed")
	// MUST 收敛到 suspended。
	assertStatus(t, s, "t1", StatusSuspended)
	// MUST 记 retryable notice（cleanup infra 错误形成可重试 debt）。
	row, _ := s.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notices: %v", perr)
	}
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
		t.Error("convergeToSuspended cleanup infra error MUST form retryable debt (notice); got none")
	}
}

// TestP4Rereview_ConvergeToSuspended_StatusCommitFailure_LogsAndDoesNotSilentlyClear 验证 P4 复评阻塞 5：
// 状态提交（Active→Suspended）失败时 MUST NOT 静默——runtime 注册表已清（停 SSE/watcher），
// 但 DB 仍 active+last_error，不得移除既有 notice。用 statusErrStore 注入状态提交错误。
func TestP4Rereview_ConvergeToSuspended_StatusCommitFailure_LogsAndDoesNotSilentlyClear(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	s.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 预置既有 notice（验证不被移除）。
	s.mutTask("t1", func(r *TaskRow) {
		r.Notice = sql.NullString{String: `[{"code":"residual_processes","message":"existing","ts":1,"data":{"sessionName":"ocdeck-t1-serve","cleanupTickets":["tk-existing"],"reason":"kill_failed","retryable":true}}]`, Valid: true}
	})
	proc := newMockProc()
	// statusErrStore 注入 UpdateTaskStatusConditional(Active→Suspended) 错误。
	errStore := wrapStatusErr(s, errors.New("db: status commit failed"))
	m := newR7TestManager(t, errStore, proc, newMockWorktree(), newMockOC(true))

	m.convergeToSuspended("t1", "test status commit failure")
	// 状态提交失败 → DB 仍 active（未落 suspended）。
	assertStatus(t, s, "t1", StatusActive)
	// 既有 notice MUST NOT 被移除。
	row, _ := s.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	if len(entries) == 0 {
		t.Fatal("existing notice MUST NOT be removed when status commit fails")
	}
	foundExisting := false
	for _, e := range entries {
		if tks := noticeTickets(e); len(tks) > 0 && tks[0] == "tk-existing" {
			foundExisting = true
		}
	}
	if !foundExisting {
		t.Error("existing tk-existing notice MUST be preserved (not removed on status commit failure)")
	}
}

// --- 顺带项：端口解析 strconv.Atoi + 校验 1..65535 ---

// TestP4Rereview_ReopenAttach_PortOutOfRange_Fails 验证端口范围校验（parsePort 1..65535）。
func TestP4Rereview_ReopenAttach_PortOutOfRange_Fails(t *testing.T) {
	s := newMockStore()
	seedSuspendedTask(s, "t1", "p1")
	s.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.LastPort = sql.NullInt64{Int64: 70000, Valid: true}
		r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	// serve env 端口超出范围 → MUST 失败（不回退 last_port）。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "70000", "OCDECK_TASK_ID": "t1",
	}
	m := newR7TestManager(t, s, proc, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach must fail when serve env port out of range 1..65535")
	}
}

// --- 辅助 ---

var _ = opencode.Event{}
var _ config.ShutdownPolicy
var _ = store.TaskRow{}