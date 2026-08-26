package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// seedProjectTask 在 mockStore 中创建项目与挂起任务。
func seedSuspendedTask(s *mockStore, taskID, projectID string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: "/repo", DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task", Branch: "ocdeck/my-task",
		Status: StatusSuspended, WorktreePath: "/data/worktrees/" + projectID + "/" + taskID}
	s.tasks[taskID] = t
	return t
}

// seedActiveTask 在 mockStore 中创建项目与活跃任务（2.8 agentStatus 测试用）。
func seedActiveTask(s *mockStore, taskID, projectID string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: "/repo", DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task", Branch: "ocdeck/my-task",
		Status: StatusActive, WorktreePath: "/data/worktrees/" + projectID + "/" + taskID}
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
		"My Task":   "my-task",
		"  a  b  ":  "a-b",
		"foo__bar":   "foo-bar",
		"---x---":    "x",
		"":          "task",
		"UPPER":     "upper",
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
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 注入运行时以使 watchServeExit 回调生效（B4：需注册 serve group，回调三元组校验）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleLegacyServe, serveSessionName("t1"))
	m.watchServeExit("t1", serveSessionName("t1"))

	// 触发 serve 退出事件。
	proc.triggerExit(serveSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	// 等待异步处理。
	time.Sleep(100 * time.Millisecond)
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended after serve exit", row.Status)
	}
}