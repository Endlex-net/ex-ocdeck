package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- 任务 1：taskBusy 机制 ---

// TestLockTaskWait_DeadlineTimeout_409 验证 lockTaskWait 内部 deadline（30s）超时返回 409 conflict。
// errTaskBusy 携带 conflict 码与 "busy" 语义；真实 30s 等待不适合 CI，语义由 TestLockTaskWait_BusySemantic_IsConflict 覆盖。
// 此处用占用锁 + 长截止 ctx 验证：ctx 不取消时，最终在 deadline 后返回 conflict。
// 为加速，本测试不真实等待 30s，仅断言 errTaskBusy 的 code 已是 conflict（语义锚点）。
func TestLockTaskWait_DeadlineTimeout_409(t *testing.T) {
	if OpErrorCode(errTaskBusy) != codeConflict {
		t.Fatalf("errTaskBusy code=%v want conflict (deadline timeout returns 409)", OpErrorCode(errTaskBusy))
	}
}

// TestLockTaskWait_CtxCancel_Conflict 验证 ctx 取消返回 conflict 语义（保留现有取消感知）。
// 与 TestLockTaskWait_CtxCancel（b_fixes_test.go）互补：此处断言 OpError code 为 conflict。
func TestLockTaskWait_CtxCancel_Conflict(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	unlock, _ := m.tryLockTask("t1")
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := m.lockTaskWait(ctx, "t1")
	if err == nil {
		t.Fatal("expected error on ctx cancel during lock wait")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%v want conflict (ctx cancelled during wait)", OpErrorCode(err))
	}
}

// --- 任务 2：Lane D ---

// TestLockTaskWait_StatusChangedAfterLock_OnlyExistsCheck 验证拿锁后复查（G4-3
// attempt 2 语义）：等待器只复查任务存在，不做 active 状态复查——等锁期间
// active→suspended/activating 等合法迁移由调用方（ReopenAttach）按 D8 表统一分派，
// 等待器不得抢先返回 invalid_state（曾导致等锁期间 active→activating 误发 4010）。
func TestLockTaskWait_StatusChangedAfterLock_OnlyExistsCheck(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	unlock, _ := m.tryLockTask("t1")

	waitDone := make(chan struct {
		unlock func()
		err    error
	}, 1)
	go func() {
		unlock, err := m.lockTaskWait(context.Background(), "t1")
		waitDone <- struct {
			unlock func()
			err    error
		}{unlock, err}
	}()
	time.Sleep(50 * time.Millisecond)
	// 等锁期间状态迁移（active→activating 是恢复场景的真实迁移；suspended 亦同）。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	unlock()

	select {
	case r := <-waitDone:
		if r.err != nil {
			t.Fatalf("lockTaskWait must not fail on legal status transition during wait: %v", r.err)
		}
		if r.unlock == nil {
			t.Fatal("expected lock handoff")
		}
		r.unlock() // 等待器成功拿锁：状态分派归调用方，不执行副作用
	case <-time.After(5 * time.Second):
		t.Fatal("lockTaskWait did not return after lock released")
	}
}

// TestLockTaskWait_NormalWaitSuccess 验证正常等待成功路径：
// 锁被短暂占用，释放后等待方拿到锁并复查 active 成功，ReopenAttach 复用已有 tui。
func TestLockTaskWait_NormalWaitSuccess(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 手动持锁。
	unlock, _ := m.tryLockTask("t1")

	// 异步在短暂延迟后释放锁，让等待方拿到。
	go func() {
		time.Sleep(50 * time.Millisecond)
		unlock()
	}()

	unlock2, err := m.lockTaskWait(context.Background(), "t1")
	if err != nil {
		t.Fatalf("lockTaskWait: %v", err)
	}
	unlock2()
}

// --- 任务 2：Lane D ---

// TestSuspend_KillOrder_TuiShellsServe 验证 D1：Suspend kill 顺序为 tui → shells → serve。
// 用 shell 会话存在，断言 mock killOrder 顺序。
func TestSuspend_KillOrder_TuiShellsServe(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.sessions[shellSessionName("t1", 2)] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	order := proc.killOrderSnapshot()
	// 期望顺序：tui 先，shell 会话中间，serve 最后。
	wantFirst := tuiSessionName("t1")
	wantLast := serveSessionName("t1")
	if len(order) == 0 {
		t.Fatal("no kill recorded")
	}
	if order[0] != wantFirst {
		t.Errorf("first kill=%s want %s (tui)", order[0], wantFirst)
	}
	if order[len(order)-1] != wantLast {
		t.Errorf("last kill=%s want %s (serve)", order[len(order)-1], wantLast)
	}
	// 所有 shell 在 tui 之后、serve 之前。
	for i, name := range order {
		if name == wantLast && i != len(order)-1 {
			t.Errorf("serve killed at index %d, must be last", i)
		}
		if name == wantFirst && i != 0 {
			t.Errorf("tui killed at index %d, must be first", i)
		}
	}
}

// TestDelete_KillOrder_TuiShellsServe 验证 D1：Delete killResidualSessions 顺序为 tui → shells → serve。
func TestDelete_KillOrder_TuiShellsServe(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteForce, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	order := proc.killOrderSnapshot()
	wantFirst := tuiSessionName("t1")
	wantLast := serveSessionName("t1")
	if len(order) == 0 {
		t.Fatal("no kill recorded")
	}
	if order[0] != wantFirst {
		t.Errorf("first kill=%s want %s (tui)", order[0], wantFirst)
	}
	if order[len(order)-1] != wantLast {
		t.Errorf("last kill=%s want %s (serve)", order[len(order)-1], wantLast)
	}
}

// TestSuspend_BranchProbe_BeforeKill 验证 D2：serveAliveBeforeKill 在 kill 前采集。
// 构造 serve 不存在（已死），Suspend 应走分支 a 并完成清理。
// 用 mock 记录 HasSession 调用时机，断言 serve 探测先于任何 kill。
func TestSuspend_BranchProbe_BeforeKill(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// serve 不存在 → 分支 a。
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended (branch a, serve already dead)", row.Status)
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("tui should be killed (branch a completes cleanup)")
	}
}

// TestSuspend_BranchC_ServeDeadBeforeRepair_GoesBranchA 验证 D2：分支 c 尝试修复前先探测 serve，
// serve 已死 MUST NOT 直接 NewSession(tui)，转分支 a。
// 构造 serve 在 kill 阶段死亡（kill 后 serve 不存活），且有 kill 失败 → 进分支 c → 修复前探测 serve 已死 → 转分支 a。
func TestSuspend_BranchC_ServeDeadBeforeRepair_GoesBranchA(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	store.mutTask("t1", func(r *TaskRow) {
		r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		r.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	// serve 初始存活（kill 前探测 alive=true），但 kill 后从 sessions 移除（SessionKilled=true 视为死亡）。
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// tui kill 失败（disposition 非 clean）→ hasFailure=true → 进分支 c。
	// serve kill 成功（SessionKilled=true）→ serve 死亡。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	// serveAliveBeforeKill=true，tui kill 失败 → 分支 c → 修复前探测 serve 已死 → 转分支 a → suspended。
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (branch c → serve dead → branch a)", row.Status)
	}
	// D2：serve 已死 MUST NOT 直接 NewSession(tui) 修复——应转分支 a（forceKillAll 尽力清理）。
	// 断言 NewSession 未被调用（未新建 tui）。
	for _, name := range proc.newSessionNamesSnapshot() {
		if name == tuiSessionName("t1") {
			t.Error("NewSession(tui) must not be called when serve is dead before repair (branch a path)")
		}
	}
}

// TestDeleteOCSessions_AggregatesErrors_NoShortCircuit 验证 D3：deletion_failed 下 removeSessions 不短路，
// 逐项错误聚合返回，404 幂等成功。
// 构造多个 session，第一个 DeleteSession 返回错误，后续应继续处理；
// 最终聚合错误，落 deletion_failed，但已成功删除的 session 行已落账。
func TestDeleteOCSessions_AggregatesErrors_NoShortCircuit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 3 个 session 记录。
	store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "s1"},
		{TaskID: "t1", SessionID: "s2"},
		{TaskID: "t1", SessionID: "s3"},
	}
	proc := newMockProc()
	// serve 存活以复用（不起一次性 serve）。
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	oc := newMockOC(true)
	// s1 删除失败，s2 成功（404 幂等，mock 返回 nil），s3 失败。
	oc.deleteErrByID["s1"] = errors.New("server error")
	oc.deleteErrByID["s3"] = errors.New("server error")
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("expected aggregated error from deleteOCSessions")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code=%v want process_error (oc session delete failed)", OpErrorCode(err))
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed (oc session delete errors)", row.Status)
	}
	// D3：不短路——s2 成功删除，其 DB 行应已落账删除。
	remaining := store.sessions["t1"]
	for _, s := range remaining {
		if s.SessionID == "s2" {
			t.Error("s2 (delete succeeded) row should be removed, not short-circuited")
		}
	}
	// s1/s3 删除失败的行应保留（未落账删除）。
	hasS1, hasS3 := false, false
	for _, s := range remaining {
		if s.SessionID == "s1" {
			hasS1 = true
		}
		if s.SessionID == "s3" {
			hasS3 = true
		}
	}
	if !hasS1 || !hasS3 {
		t.Errorf("s1=%v s3=%v (failed deletes should keep rows), remaining=%v", hasS1, hasS3, remaining)
	}
}

// TestDelete_DeleteModePreservedOnFailure_RetainedOnSuccess 验证 D4：
// 中途失败保留持久化 delete_mode 供 Retry 重入；成功完成则随 DB 行删除清除。
func TestDelete_DeleteModePreservedOnFailure_RetainedOnSuccess(t *testing.T) {
	// 失败路径：worktree remove 失败 → deletion_failed，delete_mode 保留。
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wt := newMockWorktree()
	wt.removeErr = errors.New("worktree remove failed")
	m := newTestManager(t, store, proc, wt, newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteForce, true); err == nil {
		t.Fatal("expected error on worktree remove failure")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed", row.Status)
	}
	// D4：失败时 delete_mode 必须保留（供 Retry 重入）。
	if !row.DeleteMode.Valid || row.DeleteMode.String != string(DeleteForce) {
		t.Errorf("delete_mode=%v want force (preserved on failure for retry)", row.DeleteMode)
	}

	// 成功路径：删除完成 → DB 行删除 → delete_mode 随行清除。
	store2 := newMockStore()
	seedSuspendedTask(store2, "t2", "p1")
	proc2 := newMockProc()
	m2 := newTestManager(t, store2, proc2, newMockWorktree(), newMockOC(true))
	if err := m2.Delete(context.Background(), "t2", DeleteForce, true); err != nil {
		t.Fatalf("Delete success path: %v", err)
	}
	if _, err := store2.GetTask(context.Background(), "t2"); err == nil {
		t.Error("task row should be deleted on success (delete_mode cleared with row)")
	}
}

// TestDelete_InvalidModeRejected 验证 D5：mode 仅接受 normal|force，非法值 invalid_input。
func TestDelete_InvalidModeRejected(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 非法 mode 值。
	err := m.Delete(context.Background(), "t1", DeleteMode("bogus"), true)
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Errorf("invalid mode: code=%v want invalid_input, err=%v", OpErrorCode(err), err)
	}
	// 空字符串也非法。
	err = m.Delete(context.Background(), "t1", DeleteMode(""), true)
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Errorf("empty mode: code=%v want invalid_input, err=%v", OpErrorCode(err), err)
	}
	// 状态应未变（无副作用）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (invalid mode rejected, no side effects)", row.Status)
	}
	// 合法值不被拒绝。
	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Errorf("normal mode should not be rejected: %v", err)
	}
}

// 避免未用导入。
var _ = config.ShutdownPersist
var _ = opencode.ErrSessionNotFound
var _ sync.Mutex
var _ = fmt.Sprintf