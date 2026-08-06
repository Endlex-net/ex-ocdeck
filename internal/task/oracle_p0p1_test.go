package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
)

// programmableDirtyWorktree 允许测试按调用序号返回不同的 DirtyFiles 结果，
// 覆盖 Delete 首次 + Retry 重入的二次门禁序列。
type programmableDirtyWorktree struct {
	*mockWorktree
	mu      sync.Mutex
	results []map[string]struct{} // 按调用序号返回的 dirty 集（1-based 通过 len 判定）
	calls   int
}

func (w *programmableDirtyWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	if n <= len(w.results) {
		return w.results[n-1], nil
	}
	// 默认返回空集（干净）。
	return map[string]struct{}{}, nil
}

// TestP0_DeleteRetry_SecondDirtyGate_ReRejectsNewDirty（FIX1 回归）：
// 首次 Delete 二次门禁失败（preflight 后新增 dirty）→ deletion_failed。
// Retry 重入 MUST 重新取 DirtyFiles 快照并执行二次门禁——不得传 nil 跳过。
// 重入时若仍有新增 dirty（未经确认），MUST 再次拒绝删除（落 deletion_failed）。
func TestP0_DeleteRetry_SecondDirtyGate_ReRejectsNewDirty(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	// 调用序列：Delete 调用 DirtyFiles 2 次（preflight 快照 + 二次门禁），
	// Retry 调用 DirtyFiles 2 次（重入 preflight 快照 + 重入二次门禁）。
	// 全程 preflight 快照为空，二次门禁时出现 new.txt（新增 dirty）。
	wt.results = []map[string]struct{}{
		{},              // Delete #1 preflight 快照：干净
		{"new.txt": {}}, // Delete #2 二次门禁：新增 dirty → 拒绝
		{},              // Retry #1 preflight 快照：干净
		{"new.txt": {}}, // Retry #2 二次门禁：仍新增 dirty → MUST 再次拒绝
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	// 首次 Delete：二次门禁拒绝。
	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("first Delete must be rejected (new dirty after preflight); err=%v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Fatalf("after first Delete reject, status=%s want deletion_failed", row.Status)
	}

	// Retry（confirmDirty=false）：MUST 重新执行 dirty 门禁，不得传 nil 跳过。
	err = m.Retry(context.Background(), "t1", false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("Retry with new dirty MUST be rejected again (not skip gate); err=%v", err)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Fatalf("after Retry reject, status=%s want deletion_failed", row.Status)
	}
}

// TestP0_DeleteRetry_SecondDirtyGate_AllowsWhenClean（FIX1 正向）：
// 首次 Delete 二次门禁失败 → deletion_failed；Retry 重入时 dirty 已清理（二次门禁通过）→ 删除成功。
// 证明 Retry 重新执行门禁且不误拒干净状态。
func TestP0_DeleteRetry_SecondDirtyGate_AllowsWhenClean(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	wt.results = []map[string]struct{}{
		{},              // Delete preflight 快照：干净
		{"new.txt": {}}, // Delete 二次门禁：新增 dirty → 拒绝
		{},              // Retry preflight 快照：干净
		{},              // Retry 二次门禁：干净 → 通过 → 删除成功
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("first Delete must be rejected (new dirty); err=%v", err)
	}

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry with clean dirty gate must succeed; got %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted after Retry with clean gate")
	}
}

// TestP0_ReconcileKill_PropagatesCleanupErrors（FIX2a）：
// kill 模式 Reconcile 中 kill 会话 infra 错误 MUST 传播返回。
// 用 HasSession infra 错误：Reconcile 孤儿循环忽略（已落持久化），但 reconcileKill
// 对全部会话再次 killOrphanSession 时返回 infra 错误 → 聚合传播。
func TestP0_ReconcileKill_PropagatesCleanupErrors(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// 孤儿会话（无 DB 行）。
	proc.sessions["ocdeck-ghost-serve"] = true
	// HasSession infra 错误（非 ErrNoTmuxServer）：killOrphanSession 返回错误，
	// reconcileKill 聚合传播。
	proc.hasSessionErr = errors.New("tmux protocol error")
	// kill 模式。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate
	// Reconcile kill 模式应返回非 nil（kill infra 错误传播）。
	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("reconcileKill MUST propagate kill cleanup errors (non-nil)")
	}
	if !strings.Contains(err.Error(), "orphan session") {
		t.Errorf("error should mention orphan session failure; got %v", err)
	}
}

// TestP0_ResumeActive_RestoresWatchersBeforeActive（FIX2b）：
// resumeActive MUST 先恢复 SSE/watcher/RuntimeGroup 再写 active。
// 构造 resumeRuntimeWatchers 失败（shell 枚举 ListSessions infra 错误）→ MUST 不写 active。
// 预置状态为 activating（非 active）：若 resumeActive 提交 active 则状态变 active；
// 若 watcher 失败不提交则保持 activating，证明 watchers 在 active 提交前恢复。
func TestP0_ResumeActive_RestoresWatchersBeforeActive(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	// env snapshot 供 loadEnvSnapshot。
	snap := envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })

	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}

	// 用 ptrListErrProc（嵌入 *mockProc，避免复制锁）让 ListSessions 返回 infra 错误
	// → resumeRuntimeWatchers 失败。
	badListProc := &ptrListErrProc{mockProc: proc, err: errors.New("tmux list failed")}

	m := newTestManager(t, store, badListProc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	err := m.resumeActive(context.Background(), store.tasks["t1"])
	if err == nil {
		t.Fatal("resumeActive with watcher restore failure MUST return error")
	}
	// 关键：MUST NOT 写 active（watchers 未恢复不应提交 active）→ 保持 activating。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status == StatusActive {
		t.Error("resumeActive MUST NOT commit active when resumeRuntimeWatchers fails (keep prior state, not active)")
	}
}

// ptrListErrProc 嵌入 *mockProc（避免复制 sync.Mutex），ListSessions 返回 infra 错误。
type ptrListErrProc struct {
	*mockProc
	err error
}

func (p *ptrListErrProc) ListSessions() ([]string, error) { return nil, p.err }

// TestP1_CloseShell_PropagatesInfraError（FIX3）：
// CloseShell 的 HasSession/KillSession infra 错误 MUST 返回错误（不得一律成功）。
func TestP1_CloseShell_PropagatesInfraError(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.hasSessionErr = errors.New("tmux protocol error")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.CloseShell(context.Background(), TerminalID(shellSessionName("t1", 1)))
	if err == nil {
		t.Fatal("CloseShell MUST return error on HasSession infra error (not silently succeed)")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code=%v want process_error; err=%v", OpErrorCode(err), err)
	}
}

// TestB5_ListShells_InfraErrorReturnsError（B5-backend）：
// ListShells 的 tmux 基础设施故障 MUST 返回错误（process_error），不得映射为空列表成功。
// 区分 ErrNoTmuxServer/会话不存在（空列表合理）vs 其他 infra 故障（返回 error）。
func TestB5_ListShells_InfraErrorReturnsError(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.hasSessionErr = errors.New("tmux protocol error")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup("shell", shellSessionName("t1", 1))

	out, err := m.ListShells("t1")
	if err == nil {
		t.Fatal("ListShells with infra error MUST return error (not map to empty list success)")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code=%v want process_error; err=%v", OpErrorCode(err), err)
	}
	if len(out) != 0 {
		t.Errorf("ListShells with infra error should return empty list; got %v", out)
	}
}

// TestB5_ListShells_ErrNoTmuxServerReturnsEmpty（B5-backend）：
// ErrNoTmuxServer（无 server）下空列表合理，不返回错误。
func TestB5_ListShells_ErrNoTmuxServerReturnsEmpty(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.hasSessionErr = process.ErrNoTmuxServer
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup("shell", shellSessionName("t1", 1))

	out, err := m.ListShells("t1")
	if err != nil {
		t.Fatalf("ListShells with ErrNoTmuxServer should return empty list no error; got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ListShells with no server should return empty; got %v", out)
	}
}

// TestP1_ValidateShellTerminal_InfraErrorNotMappedToAbsent（FIX3）：
// ValidateShellTerminal 区分 ErrNoTmuxServer/会话不存在 vs 其他 infra 错误——infra 错误返回 process_error。
func TestP1_ValidateShellTerminal_InfraErrorNotMappedToAbsent(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.hasSessionErr = errors.New("tmux protocol error")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup("shell", shellSessionName("t1", 1))

	err := m.ValidateShellTerminal(shellSessionName("t1", 1))
	if err == nil {
		t.Fatal("ValidateShellTerminal MUST return error on infra error (not map to absent)")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code=%v want process_error (infra); err=%v", OpErrorCode(err), err)
	}
}

// TestP1_DeleteOCSessions_UsesAuthoritativePort（FIX4）：
// 删除活跃 serve 删 oc session 时端口 MUST 取 ShowSessionEnv(OCDECK_SERVE_PORT)，
// 读回失败 MUST fail（不得用 last_port 兜底）。
func TestP1_DeleteOCSessions_UsesAuthoritativePort(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 给任务一个 oc session 行，触发 deleteOCSessions 走活跃 serve 分支。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	// 有密码但 OCDECK_SERVE_PORT 缺失/空 → 读回失败 MUST fail。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		// OCDECK_SERVE_PORT 故意缺失
	}
	store.mutTask("t1", func(r *TaskRow) { r.LastPort = sql.NullInt64{Int64: 50111, Valid: true} })

	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatal("Delete with missing OCDECK_SERVE_PORT MUST fail (not fall back to last_port)")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed (port read failure)", row.Status)
	}
}

// TestP1_DeleteOCSessions_AuthoritativePortSucceeds（FIX4 正向）：
// OCDECK_SERVE_PORT 存在时用权威端口删 oc session 成功。
func TestP1_DeleteOCSessions_AuthoritativePortSucceeds(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50077",
	}
	// LastPort 故意不同（证明不用 last_port）。
	store.mutTask("t1", func(r *TaskRow) { r.LastPort = sql.NullInt64{Int64: 50111, Valid: true} })

	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete with valid OCDECK_SERVE_PORT must succeed; got %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted")
	}
}

// TestP1_Create_PreChecksBeforeInsert（FIX5）：
// Create 前置检查（分支冲突）MUST 在插入 creating 行之前完成——前置失败不得残留任务行。
func TestP1_Create_PreChecksBeforeInsert(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	wt.branches["ocdeck/pre-check-task"] = true // 分支已存在
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", "Pre Check Task", "")
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("want conflict on branch conflict; got %v", err)
	}
	// P1：前置失败不得残留任务行。
	if _, gerr := store.GetTask(context.Background(), store.lastTaskID()); gerr == nil {
		t.Error("branch conflict pre-check MUST NOT insert a task row (pre-check before insert)")
	}
}

// TestP1_Create_InvalidBranchNameBeforeInsert（FIX5）：
// 分支名 check-ref-format 失败 MUST 在插入前拒绝（mock ValidateBranchName 返回错误）。
func TestP1_Create_InvalidBranchNameBeforeInsert(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := &invalidBranchWT{mockWorktree: newMockWorktree()}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", "My Task", "")
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Fatalf("want invalid_input on invalid branch name; got %v", err)
	}
	if _, gerr := store.GetTask(context.Background(), store.lastTaskID()); gerr == nil {
		t.Error("invalid branch name pre-check MUST NOT insert a task row")
	}
}

type invalidBranchWT struct {
	*mockWorktree
}

func (w *invalidBranchWT) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return errors.New("invalid branch name: bad ref")
}

// TestP1_SSE_DirectoryFilter_DropsForeignWorktree（FIX6）：
// created/updated/deleted 事件 directory != 本任务 worktree → 丢弃并告警，不落库。
func TestP1_SSE_DirectoryFilter_DropsForeignWorktree(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	wtPath := "/data/worktrees/p1/t1"
	// foreign directory 事件：created with directory != wtPath → 丢弃。
	ev := opencode.Event{
		Type: "session.created",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{
				"id":        "s-foreign",
				"directory": "/other/worktree",
			},
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, ev); err != nil {
		t.Fatalf("foreign directory event should be dropped not error; got %v", err)
	}
	// 不应落库 foreign session。
	rows, _ := store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 0 {
		t.Errorf("foreign directory session MUST NOT be stored; got %d rows", len(rows))
	}

	// matching directory 事件：created with directory == wtPath → 落库。
	evMatch := opencode.Event{
		Type: "session.created",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{
				"id":        "s-match",
				"directory": wtPath,
			},
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evMatch); err != nil {
		t.Fatalf("matching directory event should succeed; got %v", err)
	}
	rows, _ = store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 1 || rows[0].SessionID != "s-match" {
		t.Errorf("matching directory session should be stored; got %+v", rows)
	}
}

// TestP1_SSE_StatusEvent_OwnershipFilter（FIX6）：
// status/diff 事件（无 directory）用 sessionID 反查 task_sessions 归属，命中本任务才处理。
// 此处验证：不属于本任务的 session 事件不产生副作用（不 panic、不落库）。
func TestP1_SSE_StatusEvent_OwnershipFilter(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 预置本任务已有 session s-owned。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s-owned"}}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	wtPath := "/data/worktrees/p1/t1"

	// 无 directory 的 status 事件，sessionID 属于本任务 → 处理（无副作用，不落库变更）。
	evOwned := opencode.Event{
		Type: "session.status",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{"id": "s-owned"},
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evOwned); err != nil {
		t.Fatalf("owned status event should succeed; got %v", err)
	}

	// 无 directory 的 status 事件，sessionID 不属于本任务 → 丢弃（不处理）。
	evForeign := opencode.Event{
		Type: "session.status",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{"id": "s-foreign"},
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evForeign); err != nil {
		t.Fatalf("foreign status event should be dropped not error; got %v", err)
	}
	// 本任务 session 数不变（status 不增删）。
	rows, _ := store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 1 || rows[0].SessionID != "s-owned" {
		t.Errorf("status event must not change session rows; got %+v", rows)
	}
}

// TestP1_EnvReservedNamespace_CoversAllOCDECK（FIX7）：
// 保留命名空间 MUST 覆盖全部 OCDECK_* 前缀（用户 env 注入任何 OCDECK_* 均被忽略）。
func TestP1_EnvReservedNamespace_CoversAllOCDECK(t *testing.T) {
	if !isReservedEnvKey("OCDECK_CUSTOM_ANYTHING") {
		t.Error("isReservedEnvKey MUST cover all OCDECK_* prefix, not just enumerated keys")
	}
	if !isReservedEnvKey("OCDECK_FOO_BAR") {
		t.Error("OCDECK_FOO_BAR should be reserved")
	}
	if isReservedEnvKey("USER_PATH_LEAK") {
		t.Error("non-OCDECK env should not be reserved by prefix rule")
	}
	// 显式枚举的 5 个生命周期变量 + secret。
	for _, k := range []string{
		"OPENCODE_SERVER_PASSWORD",
		"OCDECK_SERVE_PORT", "OCDECK_TASK_ID", "OCDECK_TASK_NAME",
		"OCDECK_TASK_PATH", "OCDECK_PROJECT_PATH",
	} {
		if !isReservedEnvKey(k) {
			t.Errorf("lifecycle env %s MUST be reserved", k)
		}
	}
}

// TestP1_EnvSnapshot_InjectsAllLifecycleVars（FIX7）：
// mergeEnvSnapshot MUST 注入五个生命周期变量。
func TestP1_EnvSnapshot_InjectsAllLifecycleVars(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	row, _ := store.GetTask(context.Background(), "t1")
	merged, err := m.mergeEnvSnapshot(context.Background(), row, 50001)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	want := map[string]string{
		"OCDECK_SERVE_PORT":   "50001",
		"OCDECK_TASK_ID":      "t1",
		"OCDECK_TASK_NAME":    "my task",
		"OCDECK_TASK_PATH":    "/data/worktrees/p1/t1",
		"OCDECK_PROJECT_PATH": "/repo",
	}
	for k, v := range want {
		if got := merged[k]; got != v {
			t.Errorf("lifecycle var %s = %q want %q", k, got, v)
		}
	}
}

// TestP1_GitOps_LockMutexWithLifecycle（FIX8 锁互斥）：
// GitStatus/GitDiff/GitCommit/GitPush 持任务锁，与生命周期操作（Delete/Suspend）互斥，冲突返回 409。
func TestP1_GitOps_LockMutexWithLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 先占住任务锁（模拟生命周期操作持有锁）。
	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatalf("tryLockTask: %v", err)
	}
	defer unlock()

	// GitStatus 持同一任务锁 → MUST 返回 409 conflict。
	_, err = m.GitStatus(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("GitStatus with held lock MUST return conflict; got %v", err)
	}
	_, err = m.GitDiff(context.Background(), "t1", "", "", false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("GitDiff with held lock MUST return conflict; got %v", err)
	}
	err = m.GitCommit(context.Background(), "t1", "msg", nil)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("GitCommit with held lock MUST return conflict; got %v", err)
	}
	err = m.GitPush(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Errorf("GitPush with held lock MUST return conflict; got %v", err)
	}
}

// TestP1_GitOps_NotFoundSemantics（FIX8）：
// 不存在的任务返回 not_found（与现有一致）。
func TestP1_GitOps_NotFoundSemantics(t *testing.T) {
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if _, err := m.GitStatus(context.Background(), "nope"); err == nil || OpErrorCode(err) != codeNotFound {
		t.Errorf("GitStatus not_found; got %v", err)
	}
	if _, err := m.GitDiff(context.Background(), "nope", "", "", false); err == nil || OpErrorCode(err) != codeNotFound {
		t.Errorf("GitDiff not_found; got %v", err)
	}
	if err := m.GitCommit(context.Background(), "nope", "msg", nil); err == nil || OpErrorCode(err) != codeNotFound {
		t.Errorf("GitCommit not_found; got %v", err)
	}
	if err := m.GitPush(context.Background(), "nope"); err == nil || OpErrorCode(err) != codeNotFound {
		t.Errorf("GitPush not_found; got %v", err)
	}
}

// 防止未使用 import（fmt 仅在部分 helper 用到）。
var _ = fmt.Sprintf

// --- B1：Retry dirty 门禁与首次一致 ---

// TestB1_RetryDirty_NeedsConfirmDirty（B1 回归）：
// 首次 Delete dirty 门禁失败 → deletion_failed。Retry 时 worktree 仍有 dirty 文件，
// 无 confirmDirty → MUST 拒绝（409，提示需确认），不得以新快照为基线随后 ForceDirty 强删。
func TestB1_RetryDirty_NeedsConfirmDirty(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	// Delete: preflight 快照干净 → 二次门禁出现 new.txt → 拒绝（confirmDirty=false）。
	// Retry: preflight 快照（DirtyFiles #3）含 new.txt → 非空且 !confirmDirty → 拒绝。
	wt.results = []map[string]struct{}{
		{},              // Delete #1 preflight 快照：干净
		{"new.txt": {}}, // Delete #2 二次门禁：新增 dirty → 拒绝
		{"new.txt": {}}, // Retry #1 preflight 快照：dirty 非空 → !confirmDirty → 拒绝
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("first Delete must be rejected (new dirty); err=%v", err)
	}

	// Retry 无确认 → MUST 拒绝（不得以新快照为基线强删）。
	err = m.Retry(context.Background(), "t1", false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("Retry with dirty and no confirmDirty MUST be rejected; err=%v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed", row.Status)
	}
}

// TestB1_RetryDirty_WithConfirmDeletes（B1 正向）：
// Retry 时 worktree 有 dirty 文件，confirmDirty=true → 继续从失败步骤删除 → 成功。
func TestB1_RetryDirty_WithConfirmDeletes(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	wt.results = []map[string]struct{}{
		{},              // Delete #1 preflight 快照：干净
		{"new.txt": {}}, // Delete #2 二次门禁：新增 dirty → 拒绝
		{"new.txt": {}}, // Retry #1 preflight 快照：dirty 非空（confirmDirty=true 通过）
		{"new.txt": {}}, // Retry #2 二次门禁：dirty 与快照一致（无新增）→ 通过 → 删除成功
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("first Delete must be rejected; err=%v", err)
	}

	// Retry 带确认 → 继续删除（dirty 已纳入确认基线，二次门禁无新增 → 删除成功）。
	if err := m.Retry(context.Background(), "t1", true); err != nil {
		t.Fatalf("Retry with confirmDirty must succeed; got %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted after Retry with confirmDirty")
	}
}

// TestB1_RetryDirty_NoDirtyContinues（B1 边界）：
// Retry 时 worktree 干净（无 dirty）→ 无需 confirmDirty，继续从失败步骤删除 → 成功。
func TestB1_RetryDirty_NoDirtyContinues(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &programmableDirtyWorktree{mockWorktree: newMockWorktree()}
	wt.results = []map[string]struct{}{
		{},              // Delete #1 preflight 快照：干净
		{"new.txt": {}}, // Delete #2 二次门禁：新增 dirty → 拒绝
		{},              // Retry #1 preflight 快照：干净（无需确认）
		{},              // Retry #2 二次门禁：干净 → 删除成功
	}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("first Delete must be rejected; err=%v", err)
	}

	// Retry 无确认但 dirty 已清理 → 继续 → 删除成功。
	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry with clean worktree must succeed; got %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted after Retry (clean worktree)")
	}
}

// --- B2：Reconcile cleanup_debt 错误传播 ---

// failingDebtStore 注入 cleanup_debt 持久化错误（List/Upsert/Delete）。
// listErrAfterN 让 ListCleanupDebts 在第 N 次调用（1-based）后才返回错误，
// 以区分 restoreCleanupDebts（第 1 次）与 persistOrphanDebts（后续）的 List 调用。
type failingDebtStore struct {
	listErr       error
	listErrAfterN int
	listCalls     int
	upsertErr     error
	deleteErr     error
	upsertd       map[string]string
}

func newFailingDebtStore(listErr error, upsertErr, deleteErr error) *failingDebtStore {
	return &failingDebtStore{listErr: listErr, upsertErr: upsertErr, deleteErr: deleteErr, upsertd: map[string]string{}}
}

func newFailingDebtStoreAfterN(n int, listErr error, upsertErr, deleteErr error) *failingDebtStore {
	return &failingDebtStore{listErrAfterN: n, listErr: listErr, upsertErr: upsertErr, deleteErr: deleteErr, upsertd: map[string]string{}}
}

func (s *failingDebtStore) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upsertd[sessionName] = ticketsJSON
	return nil
}
func (s *failingDebtStore) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.upsertd, sessionName)
	return nil
}
func (s *failingDebtStore) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	s.listCalls++
	if s.listErrAfterN > 0 {
		if s.listCalls >= s.listErrAfterN {
			return nil, s.listErr
		}
	} else if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]CleanupDebtRow, 0, len(s.upsertd))
	for name, t := range s.upsertd {
		out = append(out, CleanupDebtRow{SessionName: name, Tickets: t})
	}
	return out, nil
}

// TestB2_Reconcile_RetryOrphanSessions_ListErrPropagates（B2）：
// cleanup_debt 恢复后 retryOrphanSessions 调 persistOrphanDebts→ListCleanupDebts 失败，
// MUST 传播给 Reconcile 返回值（main 据此拒开 HTTP）。
func TestB2_Reconcile_RetryOrphanSessions_ListErrPropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// persistOrphanDebts 末尾 ListCleanupDebts（删除已收敛项）失败 → 返回错误。
	// 用 listErrAfterN=2：restoreCleanupDebts 的 List（第 1 次）成功，persistOrphanDebts 的 List（第 2 次）失败。
	debt := newFailingDebtStoreAfterN(2, errors.New("cleanup_debt list boom"), nil, nil)
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	// 注入内存 orphan failure（模拟恢复后的逃逸 tickets）。
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{sessionName: "ocdeck-ghost-serve", tickets: []string{"tk1"}})
	m.orphanMu.Unlock()

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile MUST propagate cleanup_debt List error (non-nil)")
	}
	if !strings.Contains(err.Error(), "retry orphan sessions") {
		t.Errorf("error should mention retry orphan sessions; got %v", err)
	}
}

// TestB2_Reconcile_RetryOrphanSessions_UpsertErrPropagates（B2）：
// retryOrphanSessions 中 orphan 未收敛（kill 非 clean）→ persistOrphanDebts upsert 失败 → 传播。
func TestB2_Reconcile_RetryOrphanSessions_UpsertErrPropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// ghost 会话存活且 kill 非 clean → stillFailing → upsert。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"},
	}
	debt := newFailingDebtStore(nil, errors.New("cleanup_debt upsert boom"), nil)
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{sessionName: "ocdeck-ghost-serve", tickets: []string{"tk1"}})
	m.orphanMu.Unlock()

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile MUST propagate cleanup_debt Upsert error (non-nil)")
	}
	if !strings.Contains(err.Error(), "retry orphan sessions") {
		t.Errorf("error should mention retry orphan sessions; got %v", err)
	}
}

// TestB2_Reconcile_EnumerateNewOrphan_UpsertErrPropagates（第三轮 fix1）：
// Reconcile 枚举阶段发现新 orphan（无 DB 行的 tmux 会话），killOrphanSession → recordOrphanFailure
// → persistOrphanDebt Upsert 失败 → MUST 传播给 Reconcile 返回值（main 据此拒开 HTTP）。
// 与既有 B2 测试区别：既有测试预置内存 orphan（启动前已恢复），此测试经枚举路径新发现 orphan。
func TestB2_Reconcile_EnumerateNewOrphan_UpsertErrPropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// 新发现 orphan 会话：taskID=ghost 无 DB 行，存活且 kill 非 clean → recordOrphanFailure → persist 失败。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk-new"},
	}
	// persistOrphanDebt Upsert 失败（killOrphanSession → recordOrphanFailure → persistOrphanDebt）。
	debt := newFailingDebtStore(nil, errors.New("cleanup_debt upsert boom (enumerate)"), nil)
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile MUST propagate new-orphan Upsert error from enumerate phase (non-nil)")
	}
	if !strings.Contains(err.Error(), "orphan session cleanup") && !strings.Contains(err.Error(), "persist orphan debt") {
		t.Errorf("error should mention orphan session cleanup / persist orphan debt; got %v", err)
	}
}

// TestB2_Reconcile_EnumerateNewOrphan_KillErrPropagates（第三轮 fix1）：
// Reconcile 枚举阶段 killOrphanSession 的 kill infra 错误 MUST 检查并传播（当前忽略返回值）。
func TestB2_Reconcile_EnumerateNewOrphan_KillErrPropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// 新发现 orphan：HasSession 返回 infra 错误（非 ErrNoTmuxServer）→ killOrphanSession 返回错误。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.hasSessionErr = errors.New("tmux protocol error")
	debt := newFailingDebtStore(nil, nil, nil) // 持久化成功，仅验证 kill/has 错误传播
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile MUST propagate killOrphanSession HasSession infra error (non-nil)")
	}
	if !strings.Contains(err.Error(), "orphan session") {
		t.Errorf("error should mention orphan session; got %v", err)
	}
}

// TestB2_Reconcile_PersistFlushBeforeHTTPOpen（第三轮 fix1）：
// persist 模式枚举/恢复后 MUST 再次 flush persistOrphanDebts 才开放 HTTP。
// 构造：枚举无新 orphan，但内存有未 flush 的 orphan（restore 后 retryOrphanSessions 收敛留空），
// 注入 persistOrphanDebts 的 List 错误验证 flush 被调用且错误传播。
func TestB2_Reconcile_PersistFlushBeforeHTTPOpen(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// restore 的 List（第 1 次）成功；retryOrphanSessions 的 persistOrphanDebts List（第 2 次）成功；
	// 枚举无新 orphan；reconcilePersist 后的 flush persistOrphanDebts List（第 3 次）失败。
	debt := newFailingDebtStoreAfterN(3, errors.New("cleanup_debt flush list boom"), nil, nil)
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile MUST propagate persist-mode flush error before HTTP open (non-nil)")
	}
	if !strings.Contains(err.Error(), "flush orphan debts before http open") {
		t.Errorf("error should mention flush orphan debts before http open; got %v", err)
	}
}

// TestB2_ReconcilePersist_CASCommittedFalsePropagates（B2）：
// persist 分支状态 CAS committed=false（状态被并发改动）MUST 传播。
// 直接验证 commitSuspendedReconcile：状态已非 fromStatus 时 CAS 返回 committed=false → 返回错误。
func TestB2_ReconcilePersist_CASCommittedFalsePropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// CAS from=active，但当前状态已是 suspended（模拟并发 Suspend）→ committed=false → 返回错误。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })
	err := m.commitSuspendedReconcile(context.Background(), "t1", StatusActive, "test reason")
	if err == nil {
		t.Fatal("commitSuspendedReconcile with CAS not matched MUST return error (committed=false)")
	}
	if !strings.Contains(err.Error(), "CAS not matched") {
		t.Errorf("error should mention CAS not matched; got %v", err)
	}
}

// TestB2_ReconcilePersist_CASErrPropagates（B2）：
// persist 分支状态 CAS 返回 store error MUST 传播。注入 store 在 UpdateTaskStatusConditional 报错。
func TestB2_ReconcilePersist_CASErrPropagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// wrapper 让 UpdateTaskStatusConditional 返回错误。
	errStore := &casErrStore{mockStore: store, err: errors.New("db: cas boom")}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	err := m.commitSuspendedReconcile(context.Background(), "t1", StatusActive, "test reason")
	// commitSuspendedReconcile 走原 store（非 errStore）——验证 helper 直接路径：
	// 此处改用 errStore 构造 Manager 走 reconcilePersist 全链路更真实，但 helper 内部用 m.store，
	// 故需 Manager 持 errStore。重新构造。
	m2 := newTestManager(t, errStore, proc, newMockWorktree(), newMockOC(true))
	m2.SetLifecycleCtx(context.Background())
	err2 := m2.commitSuspendedReconcile(context.Background(), "t1", StatusActive, "test reason")
	if err2 == nil {
		t.Fatal("commitSuspendedReconcile with CAS store error MUST return error")
	}
	if !strings.Contains(err2.Error(), "cas boom") {
		t.Errorf("error should propagate store error; got %v", err2)
	}
	_ = err
	_ = m
}

// casErrStore 包装 mockStore，让 UpdateTaskStatusConditional 返回固定错误。
type casErrStore struct {
	*mockStore
	err error
}

func (s *casErrStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (bool, error) {
	return false, s.err
}

// --- B3：SSE directory fail-closed + status/diff sessionIDProp 归属 ---

// TestB3_SSECreatedMissingDirectory_Dropped（B3a）：
// created 事件 directory 缺失 → MUST 丢弃并告警，不落库（fail-closed：directory 明确等于 worktree 才落库）。
func TestB3_SSECreatedMissingDirectory_Dropped(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	wtPath := "/data/worktrees/p1/t1"

	// created 无 directory 字段 → 丢弃。
	ev := opencode.Event{
		Type: "session.created",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{
				"id":   "s-missing-dir",
				"time": map[string]interface{}{"updated": 100.0},
			},
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, ev); err != nil {
		t.Fatalf("missing-directory event should be dropped not error; got %v", err)
	}
	rows, _ := store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 0 {
		t.Errorf("created with missing directory MUST be dropped (fail-closed); got %d rows", len(rows))
	}

	// deleted 无 directory 字段 → 丢弃（不误删）。
	evDel := opencode.Event{
		Type: "session.deleted",
		Properties: map[string]interface{}{
			"info": map[string]interface{}{"id": "s-missing-dir"},
		},
	}
	// 预置一行，验证 deleted 缺 directory 不会误删。
	_ = store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s-missing-dir"})
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evDel); err != nil {
		t.Fatalf("missing-directory deleted should be dropped not error; got %v", err)
	}
	rows, _ = store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 1 {
		t.Errorf("deleted with missing directory MUST be dropped (no row deleted); got %d rows", len(rows))
	}
}

// TestB3_StatusEvent_SessionIDPropOwnership（B3b）：
// status/diff 事件无 properties.info，仅 properties.sessionID。
// 用 properties.sessionID 反查 task_sessions 归属：命中本任务→处理（无副作用）；
// 未命中→忽略。验证 SessionIDProp 优先取 properties.sessionID。
func TestB3_StatusEvent_SessionIDPropOwnership(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 预置本任务已有 session s-owned。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s-owned"}}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	wtPath := "/data/worktrees/p1/t1"

	// status 事件：无 info，仅 properties.sessionID=s-owned → SessionIDProp 返回 s-owned → 命中本任务 → 处理（无副作用）。
	evOwned := opencode.Event{
		Type: "session.status",
		Properties: map[string]interface{}{
			"sessionID": "s-owned",
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evOwned); err != nil {
		t.Fatalf("owned status event should succeed; got %v", err)
	}

	// status 事件：properties.sessionID=s-foreign → 未命中本任务 → 忽略（不落库变更）。
	evForeign := opencode.Event{
		Type: "session.status",
		Properties: map[string]interface{}{
			"sessionID": "s-foreign",
		},
	}
	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, evForeign); err != nil {
		t.Fatalf("foreign status event should be ignored not error; got %v", err)
	}
	// 本任务 session 数不变（status 不增删）。
	rows, _ := store.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 1 || rows[0].SessionID != "s-owned" {
		t.Errorf("status event must not change session rows; got %+v", rows)
	}
}

// TestB3_SessionIDProp_PrefersPropSessionID（B3b 单元）：
// SessionIDProp 优先 properties.sessionID，回退 properties.info.id。
func TestB3_SessionIDProp_PrefersPropSessionID(t *testing.T) {
	// 仅有 properties.sessionID（status/diff 形态）。
	ev := opencode.Event{Type: "session.status", Properties: map[string]interface{}{
		"sessionID": "sid-prop",
	}}
	if got := ev.SessionIDProp(); got != "sid-prop" {
		t.Errorf("SessionIDProp with only properties.sessionID = %q want sid-prop", got)
	}
	// 同时有 properties.sessionID 与 properties.info.id → 优先 properties.sessionID。
	ev2 := opencode.Event{Type: "session.created", Properties: map[string]interface{}{
		"sessionID": "sid-prop",
		"info":      map[string]interface{}{"id": "sid-info"},
	}}
	if got := ev2.SessionIDProp(); got != "sid-prop" {
		t.Errorf("SessionIDProp should prefer properties.sessionID; got %q want sid-prop", got)
	}
	// 仅 properties.info.id → 回退。
	ev3 := opencode.Event{Type: "session.created", Properties: map[string]interface{}{
		"info": map[string]interface{}{"id": "sid-info"},
	}}
	if got := ev3.SessionIDProp(); got != "sid-info" {
		t.Errorf("SessionIDProp fallback to info.id = %q want sid-info", got)
	}
	// 都无 → 空串。
	ev4 := opencode.Event{Type: "session.status", Properties: map[string]interface{}{}}
	if got := ev4.SessionIDProp(); got != "" {
		t.Errorf("SessionIDProp with nothing = %q want empty", got)
	}
}
