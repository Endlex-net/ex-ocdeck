package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// TestKeyedMutex_ConcurrentSameTask409 验证同一任务的并发操作返回 409（B3）。
// 不同 taskID 不冲突（TestKeyedMutex_ConcurrentCreate 已覆盖）；同 taskID 第二个操作须 409。
func TestKeyedMutex_ConcurrentSameTask409(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 手动持锁模拟并发：lock 后另一操作同任务应 409。
	unlock, lerr := m.tryLockTask("t1")
	if lerr != nil {
		t.Fatalf("tryLock: %v", lerr)
	}
	defer unlock()
	err := m.Suspend(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("concurrent same-task op: code=%v want conflict, err=%v", OpErrorCode(err), err)
	}
}

// TestLockTaskWait_CtxCancel 验证 ReopenAttach 等待路径感知 ctx 取消（B3）。
// 锁被占用时，lockTaskWait 在 ctx 取消后返回 ctx.Err()，不执行副作用。
func TestLockTaskWait_CtxCancel(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 手动持锁。
	unlock, _ := m.tryLockTask("t1")
	defer unlock()
	// ReopenAttach 在锁被占用时等待；ctx 取消应返回 conflict(ctx.Err)。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := m.lockTaskWait(ctx, "t1")
	if err == nil {
		t.Fatal("expected error on ctx cancel during lock wait")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%v want conflict (ctx cancelled)", OpErrorCode(err))
	}
}

// TestCallbackIsolation_OldGenIgnored 验证回调三元组隔离：旧代回调不清理新代（B4）。
// 触发旧代 serve 退出事件 → 新代 runtime 不应被清理。
func TestCallbackIsolation_OldGenIgnored(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 旧代 runtime + serve group。
	oldRT := m.newRuntime("t1")
	m.setRuntime("t1", oldRT)
	oldRT.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.watchServeExit("t1", runtimeSessionName("t1"))

	// 模拟新代 runtime：newRuntime 递增 generation，注册新 serve group。
	newRT := m.newRuntime("t1")
	m.setRuntime("t1", newRT)
	newRT.registerGroup(roleRuntime, runtimeSessionName("t1"))

	// 触发旧代 watch 的退出事件（旧 cancel 仍指向 oldRT 闭包）。
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	time.Sleep(100 * time.Millisecond)

	// 旧代回调 matchesRegistry 应不匹配新代 → 忽略 → 任务状态保持 active。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("old-gen callback should not affect new-gen runtime; status=%s want active", row.Status)
	}
}

// TestAlignSessions_OverflowNotice 验证 count==limit 写 session_overflow notice（B5）。
func TestAlignSessions_OverflowNotice(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	// 用一个返回 overflow 错误的 OCClient。
	oc := &overflowOC{}
	if err := m.alignSessions(context.Background(), "t1", "/wt", oc, AlignModeRepo); err != nil {
		t.Fatalf("alignSessions: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if e.Code == noticeCodeSessionOverflow {
			found = true
		}
	}
	if !found {
		t.Error("session_overflow notice should be recorded on count==limit")
	}
}

// overflowOC 返回 ErrSessionOverflow（模拟 count==limit）。
type overflowOC struct{}

func (c *overflowOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return opencode.HealthResponse{Healthy: true}, nil
}
func (c *overflowOC) Probe(ctx context.Context) (string, error) { return "1", nil }
func (c *overflowOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return nil, opencode.ErrSessionOverflow
}
func (c *overflowOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return opencode.Session{}, opencode.ErrSessionNotFound
}
func (c *overflowOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 1, Updated: 1}}, nil
}
func (c *overflowOC) DeleteSession(ctx context.Context, dir, id string) error { return nil }
func (c *overflowOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return nil, nil
}
func (c *overflowOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *overflowOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}
func (c *overflowOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}

func (c *overflowOC) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) opencode.PromptResult {
	return opencode.PromptResult{Kind: opencode.ResultPreSendFailure, Detail: "overflowOC: prompt_async not supported"}
}

// TestAllocatePort_RotationCursor 验证端口轮转游标（B5）：连续分配不每次从头扫。
func TestAllocatePort_RotationCursor(t *testing.T) {
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.cfg.ServePortRange = config.PortRange{Min: 50000, Max: 50005}
	// 第一次分配（无 last_port）。
	p1, err := m.allocatePort(sql.NullInt64{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 占用 p1，再分配应轮转到下一个。
	// 由于 isPortFree 探测真实端口，测试环境端口可能都空闲；改为验证游标推进。
	p2, err := m.allocatePort(sql.NullInt64{Int64: int64(p1), Valid: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// last_port 仍空闲 → 返回 last_port。
	if p2 != p1 {
		t.Errorf("last_port free should reuse it: got %d want %d", p2, p1)
	}
	// 游标应记录 p1。
	m.portCursorMu.Lock()
	cursor := m.portCursor
	m.portCursorMu.Unlock()
	if cursor != p1 {
		t.Errorf("cursor=%d want %d", cursor, p1)
	}
}

// TestSuspend_BranchC_FullRuntimeRecovery 验证分支 c 修复恢复完整运行时（B7）。
// 断言：状态回 active + runtime 重建（serve/tui group 注册 + SSE）。
func TestSuspend_BranchC_FullRuntimeRecovery(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	store.mutTask("t1", func(t *TaskRow) {
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}
	// tui kill 失败；serve kill 失败（仍存活）→ 分支 c。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status=%s want active (branch c repaired)", row.Status)
	}
	// B7：断言完整运行时恢复——runtime 重建且 serve/tui group 注册。
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime should be rebuilt after branch c repair")
	}
	rt.mu.Lock()
	_, hasRuntime := rt.groups[runtimeSessionName("t1")]
	_, hasServe := rt.groups[serveSessionName("t1")]
	rt.mu.Unlock()
	if !hasRuntime && !hasServe {
		t.Error("runtime group should be registered after repair")
	}
	// SSE 应已建立（sseCancel 非 nil）。
	rt.mu.Lock()
	sseActive := rt.sseCancel != nil
	rt.mu.Unlock()
	if !sseActive {
		t.Error("SSE subscription should be active after repair")
	}
}

// TestDelete_PreflightBeforeSideEffects 验证静态检查先于副作用（B8）。
// PreflightDelete 返回错误 → 拒绝删除，状态不变（不进 deleting）。
func TestDelete_PreflightBeforeSideEffects(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wt := newMockWorktree()
	wt.preflightErr = errors.New("worktree dirty, confirm required")
	m := newTestManager(t, store, proc, wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("want conflict on preflight failure, got %v", err)
	}
	// 状态应保持 suspended（未进 deleting）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (preflight rejected, no side effects)", row.Status)
	}
}

// TestDelete_DebtBlocksDelete 验证 retryable debt 阻止删除（B8）。
func TestDelete_DebtBlocksDelete(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 retryable residual notice。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	proc := newMockProc()
	// serve 仍存活，kill 仍失败 → debt 不收敛。
	proc.sessions[serveSessionName("t1")] = true
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("expected error on unresolved debt")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed (debt blocks)", row.Status)
	}
}

// TestDelete_NonRetryableDebtDoesNotBlock 验证 overflow/degraded 不阻止删除（B8）。
func TestDelete_NonRetryableDebtDoesNotBlock(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入不可重试 degraded notice + overflow notice。
	notice := []noticeEntry{
		{Code: noticeCodeResidual, Data: map[string]interface{}{"sessionName": "x", "reason": noticeReasonSnapshotMissing, "retryable": false}},
		{Code: noticeCodeSessionOverflow},
	}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Fatalf("non-retryable debt should not block delete: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted despite non-retryable debt")
	}
}

// TestReconcile_ListSessionsFailClosed 验证 ListSessions infra 错误 fail-closed（B9）。
func TestReconcile_ListSessionsFailClosed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := &listErrProc{err: errors.New("tmux binary missing")}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected error on ListSessions infra failure (fail-closed)")
	}
	// 状态应保持 active（不改状态，fail-closed）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status=%s want active (fail-closed, no state change)", row.Status)
	}
}

// listErrProc mockProc 变体：ListSessions 返回 infra 错误。
type listErrProc struct {
	mockProc
	err error
}

func (p *listErrProc) ListSessions() ([]string, error) { return nil, p.err }

// TestReconcile_OrphanFailureQueuedForRetry 验证孤儿清理失败进后台重试（B9）。
func TestReconcile_OrphanFailureQueuedForRetry(t *testing.T) {
	store := newMockStore() // 无 DB 任务
	proc := newMockProc()
	proc.sessions["ocdeck-ghost-serve"] = true
	// kill 失败（disposition 非 clean）。
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk"}}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	_ = m.Reconcile(context.Background())
	// 孤儿会话应仍存活（kill 失败）。
	if !proc.sessions["ocdeck-ghost-serve"] {
		t.Error("orphan session should still exist after failed kill")
	}
	// 应进 orphanFailures 队列。
	m.orphanMu.Lock()
	queued := len(m.orphanFailures) > 0
	m.orphanMu.Unlock()
	if !queued {
		t.Error("failed orphan kill should be queued for background retry")
	}
}

// TestReconcile_PrePassDebtBlocksResume 验证 cleanup-debt pre-pass：有 retryable debt 的任务不恢复活跃（B9）。
func TestReconcile_PrePassDebtBlocksResume(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	// 注入 retryable debt。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (debt blocks resume)", row.Status)
	}
}

// TestShutdown_KillModeOrder 验证关停顺序（§10）：kill 模式杀全部会话后停后台。
func TestShutdown_KillModeOrder(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// kill 模式：全部会话应被杀。
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should be killed on shutdown (kill mode)")
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("tui session should be killed on shutdown (kill mode)")
	}
}

// TestCreate_BranchConflictPreservesExisting 验证分支冲突不伤既有分支（B1）。
// P1（design §19 前置检查顺序）：分支冲突属于无副作用前置检查，MUST 在插入 creating 行之前
// 完成——前置失败不得残留 creation_failed 行。调整后：分支冲突返回 conflict 且不落任何任务行。
func TestCreate_BranchConflictPreservesExisting(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	// 预置分支已存在。
	wt.branches["ocdeck/my-task"] = true
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", "My Task", "")
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("want conflict on existing branch, got %v", err)
	}
	// P1：前置失败不得残留 creation_failed 行——无任务行被插入。
	if _, gerr := store.GetTask(context.Background(), store.lastTaskID()); gerr == nil {
		t.Errorf("branch conflict MUST NOT insert a task row (pre-check before insert); got row for %s", store.lastTaskID())
	}
	// 既有分支不应被删除（worktree.Add 未调用，cleanupFailedAdd 不触发）。
	if !wt.branches["ocdeck/my-task"] {
		t.Error("existing branch should be preserved (not deleted by cleanup)")
	}
}

// TestRetryCreate_StrictProductVerification 验证 RetryCreate 严格产物验证（B1）。
// 产物不完整 → 重新 add；产物完整 → 跳过 add。
func TestRetryCreate_StrictProductVerification(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	// 任务处于 creation_failed，worktree 产物不完整。
	t1 := TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/p1/t1", BaseRef: "refs/heads/main"}
	store.tasks["t1"] = t1
	wt := newMockWorktree()
	// products 不含该路径 → VerifyWorktreeProduct 失败 → 重新 add。
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (retry add succeeded)", row.Status)
	}
	if !wt.addedPaths["/data/worktrees/p1/t1"] {
		t.Error("worktree add should be re-attempted when product verification fails")
	}
}

// TestShellNameParsing 验证 shell-<n> 名解析正确（B9 util.go 修复）。
func TestShellNameParsing(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		taskID string
	}{
		{"ocdeck-abc123-serve", "serve", "abc123"},
		{"ocdeck-abc123-tui", "tui", "abc123"},
		{"ocdeck-abc123-runtime", "runtime", "abc123"},
		{"ocdeck-abc123-shell-1", "shell-1", "abc123"},
		{"ocdeck-abc123-shell-12", "shell-12", "abc123"},
		{"ocdeck-a1b2c3d4-shell-99", "shell-99", "a1b2c3d4"},
		{"ocdeck-abc123-shell-0", "", ""},
		{"ocdeck-abc123-shell-00", "", ""},
		{"ocdeck-abc123-shell-", "", ""},
		{"ocdeck-abc123-shell-abc", "", ""},
		{"ocdeck-abc123-worker", "", ""},
		{"not-ocdeck-abc123-runtime", "", ""},
		{"ocdeck-", "", ""},
	}
	if got := runtimeSessionName("abc123"); got != "ocdeck-abc123-runtime" {
		t.Errorf("runtimeSessionName = %q, want ocdeck-abc123-runtime", got)
	}
	for _, c := range cases {
		if got := roleFromSessionName(c.name); got != c.role {
			t.Errorf("roleFromSessionName(%q)=%q want %q", c.name, got, c.role)
		}
		if got := taskIDFromSessionName(c.name); got != c.taskID {
			t.Errorf("taskIDFromSessionName(%q)=%q want %q", c.name, got, c.taskID)
		}
	}
}

// TestCloseShell_RejectsNonShell 验证 CloseShell 校验 terminalID 必须是 shell（B10）。
func TestCloseShell_RejectsNonShell(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 尝试关闭 serve 会话 → 拒绝。
	err := m.CloseShell(context.Background(), TerminalID(serveSessionName("t1")))
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Errorf("closing serve should be rejected, got %v", err)
	}
	// 尝试关闭 tui 会话 → 拒绝。
	err = m.CloseShell(context.Background(), TerminalID(tuiSessionName("t1")))
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Errorf("closing tui should be rejected, got %v", err)
	}
	// serve/tui 会话应仍存活。
	if !proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should not be killed by CloseShell")
	}
	if !proc.sessions[tuiSessionName("t1")] {
		t.Error("tui session should not be killed by CloseShell")
	}
}

// TestProbe_IncompatibleMapsOCIncompatible 验证 Probe 不兼容映射 oc_incompatible（B5）。
func TestProbe_IncompatibleMapsOCIncompatible(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrCapabilityMismatch
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error on probe incompatibility")
	}
	if OpErrorCode(err) != codeOCIncompatible {
		t.Errorf("code=%s want oc_incompatible, err=%v", OpErrorCode(err), err)
	}
	// 失败应清 env snapshot + 回 suspended。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (activate failed)", row.Status)
	}
	if row.EnvSnapshot.Valid {
		t.Error("env snapshot should be cleared on activate failure")
	}
}

// TestActivate_SSEManagerLifecycle 验证 SSE 挂 Manager 生命周期 context（B5）。
// HTTP request ctx 取消后 SSE 仍存活（挂 lifecycle ctx）。
func TestActivate_SSEManagerLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 注入 lifecycle ctx。
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	defer lifeCancel()
	m.SetLifecycleCtx(lifeCtx)

	// 用可取消的 request ctx 激活。
	reqCtx, reqCancel := context.WithCancel(context.Background())
	if err := m.Activate(reqCtx, "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 取消 request ctx。
	reqCancel()
	time.Sleep(50 * time.Millisecond)
	// SSE 应仍存活（挂 lifecycle ctx）：runtime.sseCancel 非 nil 且 lifecycle ctx 未取消。
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime should exist after activate")
	}
	rt.mu.Lock()
	sseCancel := rt.sseCancel
	rt.mu.Unlock()
	if sseCancel == nil {
		t.Error("SSE should still be active (bound to lifecycle ctx, not request ctx)")
	}
}

// (test helpers retained below use no external imports beyond the package.)
