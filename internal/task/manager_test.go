package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// seedProjectTask 在 mockStore 中创建项目与挂起任务。
// repo fixture 默认带 Branch 与 BaseRef=refs/heads/main（task-base-branch-context 2.1：
// 会到达 layerEnvSnapshot 的正常 repo fixture 必须满足 D5 校验；异常路径再显式覆盖）。
func seedSuspendedTask(s *mockStore, taskID, projectID string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: "/repo", DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task", Branch: "ocdeck/my-task",
		BaseRef: "refs/heads/main",
		Status:  StatusSuspended, WorktreePath: "/data/worktrees/" + projectID + "/" + taskID}
	s.tasks[taskID] = t
	return t
}

// seedActiveTask 在 mockStore 中创建项目与活跃任务（2.8 agentStatus 测试用）。
// repo fixture 默认带 Branch 与 BaseRef=refs/heads/main（同 seedSuspendedTask）。
func seedActiveTask(s *mockStore, taskID, projectID string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: "/repo", DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task", Branch: "ocdeck/my-task",
		BaseRef: "refs/heads/main",
		Status:  StatusActive, WorktreePath: "/data/worktrees/" + projectID + "/" + taskID}
	s.tasks[taskID] = t
	return t
}

func TestCreate_Success(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended", row.Status)
	}
	if !strings.HasPrefix(row.Branch, "ocdeck/") {
		t.Errorf("branch = %s, want ocdeck/ prefix", row.Branch)
	}
	if row.WorktreePath == "" {
		t.Error("empty worktree path")
	}
}

func TestCreate_WorktreeFail_CreationFailed(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	wt.addErr = errors.New("branch exists")
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", "task", "")
	if err == nil {
		t.Fatal("expected error on worktree add failure")
	}
	row, _ := store.GetTask(context.Background(), store.lastTaskID())
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed", row.Status)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Task":  "my-task",
		"  a  b  ": "a-b",
		"foo__bar": "foo-bar",
		"---x---":  "x",
		"":         "task",
		"UPPER":    "upper",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArchiveRestore(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if err := m.Archive(context.Background(), "t1"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusArchived {
		t.Fatalf("status = %s, want archived", row.Status)
	}
	if err := m.Restore(context.Background(), "t1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended", row.Status)
	}
}

func TestArchive_RequiresSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.Archive(context.Background(), "t1")
	if err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Errorf("expected invalid_state error, got %v", err)
	}
}

func TestActivate_RetryableNoticeRejected(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 retryable residual notice（B6：retryable 在 data 内）。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true}}}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected conflict on retryable notice")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code = %s, want conflict", OpErrorCode(err))
	}
}

func TestActivate_ResidualSessionRejected(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true // 残留 serve 会话
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected conflict on residual session")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code = %s, want conflict", OpErrorCode(err))
	}
}

func TestSuspend_BranchA_ServeDead(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	// serve 不存在（已死），tui 存在（将被 kill）。
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (branch a)", row.Status)
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("tui session should be killed")
	}
}

func TestSuspend_BranchB_AllClean(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (branch b)", row.Status)
	}
}

func TestSuspend_BranchC_PartialFailRepair(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	store.mutTask("t1", func(t *TaskRow) {
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// tui kill 失败（kill_failed 带 tickets）；serve kill 失败（仍存活）→ 分支 c。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	// serve 存活 → tryRepairRuntime 应重开 tui。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	// 分支 c 修复成功 → 回 active。
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active (branch c repaired)", row.Status)
	}
}

// cancelOnKillProc 包装 mockProc：首次 KillSession 时触发 cancel（模拟请求中途取消）。
// 用于验证 suspending 已提交后的补偿路径使用 detached ctx 而非已取消的请求 ctx（F3）。
type cancelOnKillProc struct {
	*mockProc
	cancel context.CancelFunc
	once   sync.Once
}

func (p *cancelOnKillProc) KillSession(name string) (process.KillResult, error) {
	p.once.Do(p.cancel)
	return p.mockProc.KillSession(name)
}

// TestSuspend_BranchC_RestoreCommitCtxCancel_ConvergesSuspended（F3）：
// 分支 c 修复成功，但恢复 active 的状态提交时请求 ctx 已取消（werr）——
// 补偿必须使用 detached ctx 完成清理与 suspended 收敛，werr 经 cause 并入 last_error，
// 不得伪造 killResultEntry 产生 retryable notice。
func TestSuspend_BranchC_RestoreCommitCtxCancel_ConvergesSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	store.mutTask("t1", func(t *TaskRow) {
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// tui/serve kill 失败 → 分支 c；serve 存活 → tryRepairRuntime 成功。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	aware := newCtxAwareStore(store)
	m := newTestManager(t, aware, proc, newMockWorktree(), newMockOC(true))

	// 首次 KillSession（tui）时取消请求 ctx：其后的 restore active 提交必因 ctx 取消失败。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.proc = &cancelOnKillProc{mockProc: proc, cancel: cancel}

	err := m.Suspend(ctx, "t1")
	if err == nil {
		t.Fatal("Suspend should surface restore-commit failure")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	// detached ctx 补偿：最终必须收敛到 suspended（不得卡 suspending）。
	if row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended (detached compensation)", row.Status)
	}
	// werr 经 cause 并入 last_error（含 restore active commit 语义）。
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "restore active commit") {
		t.Errorf("last_error = %v, want contains 'restore active commit'", row.LastError.String)
	}
	// finishSuspend 的写路径 MUST NOT 使用已取消的请求 ctx：
	// deadCalls 只允许出现 restore active 提交那一次 UpdateTaskStatusConditional，
	// 不得出现 finishSuspend 的 UpdateTaskStatus / UpdateTaskEnvSnapshot。
	for _, m := range aware.deadCalls() {
		if m == "UpdateTaskStatus" || m == "UpdateTaskEnvSnapshot" {
			t.Errorf("finishSuspend write %q used canceled request ctx", m)
		}
	}
	// 无 werr 被伪造进残留 notice（F16：tui 真实 kill 失败可合法产生 kill_failed notice，
	// 但 restore-commit 错误 MUST NOT 经 killResultEntry 混入 notice/债务）。
	if row.Notice.Valid && strings.Contains(row.Notice.String, "restore active commit") {
		t.Errorf("restore-commit error must not leak into residual notice: %s", row.Notice.String)
	}
}

// TestFinishSuspend_CauseOnlyNoFabricatedNotice（F21/F16）：
// 直接调用 finishSuspend（空 results + 仅 cause）——cause 并入 last_error 与返回值，
// 但 MUST NOT 产生任何残留 notice（notice 不含 killErr 文本，旧断言无法识别伪造
// killResultEntry；此直测验证 cause 不经 killRes 通道）。
func TestFinishSuspend_CauseOnlyNoFabricatedNotice(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusSuspending })
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	cause := errors.New("restore active commit: db down")
	err := m.finishSuspend(context.Background(), "t1", nil, cause)
	if err == nil || !strings.Contains(err.Error(), "restore active commit") {
		t.Fatalf("finishSuspend should return cause, got %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended", row.Status)
	}
	// cause 进 last_error。
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "restore active commit") {
		t.Errorf("last_error = %v, want contains cause", row.LastError.String)
	}
	// 空 results + 仅 cause → 零残留 notice。
	if row.Notice.Valid && row.Notice.String != "" {
		t.Errorf("no residual notice expected for cause-only finish, got %s", row.Notice.String)
	}
}

// TestFinishSuspend_CausePlusWriteFaults_AllReasonsInReturn（F22）：
// cause + env 清理失败 + 状态提交失败并发——返回错误必须聚合全部原因（F20：
// 状态写失败不得被 cause 掩盖）；last_error 只含提交前已知错误（cause + env 清理失败）。
func TestFinishSuspend_CausePlusWriteFaults_AllReasonsInReturn(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusSuspending })
	// 用已取消的 ctx 调用 finishSuspend：ctxAwareStore 让 env 清理与状态写双双失败。
	aware := newCtxAwareStore(store)
	m := newTestManager(t, aware, newMockProc(), newMockWorktree(), newMockOC(true))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("restore active commit: db down")
	err := m.finishSuspend(ctx, "t1", nil, cause)
	if err == nil {
		t.Fatal("finishSuspend should aggregate errors")
	}
	// 返回聚合必须包含：cause + env 清理失败 + 状态提交失败（F20 核心）。
	for _, want := range []string{"restore active commit", "clear env snapshot", "commit suspended"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("return error missing %q: %v", want, err)
		}
	}
	// 行仍为 suspending（状态写失败），last_error 只含提交前已知错误（cause+env），
	// 不含状态提交错误本身（无法写进该次失败的持久化操作）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspending {
		t.Errorf("status = %s, want suspending (commit failed)", row.Status)
	}
	if row.LastError.Valid && strings.Contains(row.LastError.String, "commit suspended") {
		t.Errorf("last_error must not contain the commit failure itself: %s", row.LastError.String)
	}
}

func TestDelete_Empty_Success(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wt := newMockWorktree()
	m := newTestManager(t, store, proc, wt, newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task row should be deleted")
	}
}

func TestDelete_404_SessionIdempotent(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入一条 session 记录。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 100}}
	proc := newMockProc()
	// serve 存活以复用删除 session。
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{} // 空
	oc.getSessionErr = opencode.ErrSessionNotFound
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task row should be deleted")
	}
}

func TestDeletionFailed_RetryByPersistedMode(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 置 deletion_failed + delete_mode=force。
	store.mutTask("t1", func(t *TaskRow) {
		t.Status = StatusDeletionFailed
		t.DeleteMode = sql.NullString{String: string(DeleteForce), Valid: true}
	})
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted after retry")
	}
}

func TestReconcile_KillMode_AllToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) {
		t.Status = StatusActive
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillOnStart

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (kill mode)", row.Status)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should be killed in kill mode")
	}
}

func TestReconcile_Persist_ServeGone_Suspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc() // 无 serve 会话
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (serve gone)", row.Status)
	}
}

func TestReconcile_OrphanSessionKilled(t *testing.T) {
	store := newMockStore() // 无 DB 任务
	proc := newMockProc()
	proc.sessions["ocdeck-ghost-serve"] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	_ = m.Reconcile(context.Background())
	if proc.sessions["ocdeck-ghost-serve"] {
		t.Error("orphan session should be killed")
	}
}

func TestKeyedMutex_ConcurrentCreate(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	m, _ := newTestManagerWithLifecycle(t, store, newMockProc(), wt, newMockOC(true))

	// 并发创建两个任务：各自独立 taskID，不冲突（keyed mutex per task）。
	done := make(chan error, 2)
	go func() { _, err := m.Create(context.Background(), "p1", "A", ""); done <- err }()
	go func() { _, err := m.Create(context.Background(), "p1", "B", ""); done <- err }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent create %d: %v", i, err)
		}
	}
	// 等待自动激活 goroutine 收尾，避免与 store 断言读竞态（自动激活写 store.tasks）。
	m.autoActivateWG.Wait()
	store.mu.Lock()
	n := len(store.tasks)
	store.mu.Unlock()
	if n != 2 {
		t.Errorf("expected 2 tasks, got %d", n)
	}
}

func TestServeExit_HandlerSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) {
		t.Status = StatusActive
		// D8：Recovery attempt 前加载持久化快照——手动 seed 的 active 行需带合法快照。
		b, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}})
		t.EnvSnapshot = b
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 注入运行时以使 watchServeExit 回调生效（B4：需注册 serve group，回调三元组校验）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.watchServeExit("t1", runtimeSessionName("t1"))

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), "t1")
		if row.Status == StatusActive || row.Status == StatusActivating {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	t.Errorf("status = %s, want activating|active after serve exit recovery", row.Status)
}
