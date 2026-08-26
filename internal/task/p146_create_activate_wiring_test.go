// p146_create_activate_wiring_test.go 验证 P1.4.6 Manager facade 注入 LifecycleService 后
// Create/Retry/Activate 的行为与 legacy 直连 store 路径等价
//（design.md D0:142 迁移第 6 步：DB 事实经 application persist+commit 封装，
// worktree/tmux/opencode/锁与调度留在 Manager）。
//
// 经 mockAppAdapter（TaskRepository + TaskReadRepository + SessionRepository）注入
// LifecycleService，断言：
//   - Create repo/dir 成功路径状态收敛（自动激活推进 active）与 guard 拒绝零落库；
//   - Retry creation_failed 解锁后再调度自动激活（不自锁）；
//   - Retry deletion_failed 重入：SetDeleteMode + deleting 两笔写入先于 dirty preflight；
//   - Activate guard 拒绝零 CAS；成功路径推进 active。
package task

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/infrastructure/opencode"
)

// newP146TestManager 构造注入完整 LifecycleService（Tasks+Read+Sessions ports）的 Manager。
// store 可为 mockStore 或其 traceStore 包装（副作用顺序断言用）。
func newP146TestManager(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) *Manager {
	t.Helper()
	adapter := &mockAppAdapter{s: store}
	svc := apptask.New(apptask.Options{
		Tasks:    adapter,
		Read:     adapter,
		Sessions: adapter,
		Publish:  apptask.NoopPublisher{},
	})
	m := newTestManager(t, store, proc, wt, oc)
	m.SetLifecycleCtx(context.Background())
	m.lifecycle = svc
	return m
}

// TestP146_Create_Repo_ViaLifecycle 镜像 TestCreate_AutoActivateTriggered：
// repo Create 成功后自动激活推进 active（CreateTask/CommitCreated/CAS/env/port/active
// 全链 DB 写经 lifecycle 路径收敛），serve/tui 会话已创建。
func TestP146_Create_Repo_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	m := newP146TestManager(t, store, proc, wt, newMockOC(true))

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("status=%s want suspended|activating|active (auto-activate race)", row.Status)
	}

	// 自动激活异步推进至 active（经 lifecycle CAS suspended→activating→active）。
	waitForStatus(t, store, row.ID, StatusActive, 3*time.Second)
	proc.mu.Lock()
	hasServe := proc.sessions[serveSessionName(row.ID)]
	hasTUI := proc.sessions[tuiSessionName(row.ID)]
	proc.mu.Unlock()
	if !hasServe {
		t.Error("serve session not created by auto-activate (via lifecycle)")
	}
	if !hasTUI {
		t.Error("tui session not created by auto-activate (via lifecycle)")
	}
}

// TestP146_Create_Repo_GuardReject_ViaLifecycle 分支已存在 → conflict，
// 不得 CreateTask 落库（决策先于副作用不变量保持）。
func TestP146_Create_Repo_GuardReject_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	wt.branches["ocdeck/my-task"] = true
	m := newP146TestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", "My Task", "")
	if err == nil {
		t.Fatal("expected conflict on branch exists")
	}
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("code = %s, want conflict", OpErrorCode(err))
	}
	store.mu.Lock()
	n := len(store.tasks)
	store.mu.Unlock()
	if n != 0 {
		t.Fatalf("guard reject must not create task row, got %d rows", n)
	}
}

// TestP146_Create_Dir_ViaLifecycle 镜像 p141 dir Create：Branch 空、
// worktree_path 为 canonical 项目目录。
func TestP146_Create_Dir_ViaLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: dir, DefaultBranch: "", Kind: ProjectKindDir})
	m := newP146TestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create dir: %v", err)
	}
	if row.Branch != "" {
		t.Errorf("dir task branch MUST empty, got %q", row.Branch)
	}
	// macOS /var 是 /private/var 符号链接，比较规范化后的路径而非字面相等。
	wantPath, _ := filepath.EvalSymlinks(dir)
	gotPath, _ := filepath.EvalSymlinks(row.WorktreePath)
	if gotPath != wantPath {
		t.Errorf("dir task worktree_path = %q (norm %q), want %q", row.WorktreePath, gotPath, wantPath)
	}
}

// TestP146_Retry_CreationFailed_UnlockThenActivate 镜像 TestRetryCreationFailed_AutoActivateTriggered：
// retryCreate 成功提交后 MUST 先释放 keyed mutex 再调度自动激活——若在持锁期间触发，
// Activate 的 tryLockTask 会 409 自锁，任务永久 suspended。等待 active 即证明解锁先于调度。
func TestP146_Retry_CreationFailed_UnlockThenActivate(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/p1/t1", BaseRef: "refs/heads/main"}
	wt := newMockWorktree()
	proc := newMockProc()
	m := newP146TestManager(t, store, proc, wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	waitForStatus(t, store, "t1", StatusActive, 3*time.Second)
	proc.mu.Lock()
	hasTUI := proc.sessions[tuiSessionName("t1")]
	proc.mu.Unlock()
	if !hasTUI {
		t.Error("tui session not created by auto-activate after retry create (via lifecycle)")
	}
}

// TestP146_Retry_DeletionFailed_Reenter_ViaLifecycle deletion_failed + 持久化 force 模式重入：
// SetDeleteMode + UpdateTaskStatus(deleting) 两笔写入（经 lifecycle 路由）MUST 先于
// dirty preflight 发生；preflight 失败后落回 deletion_failed。
func TestP146_Retry_DeletionFailed_Reenter_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/task",
		Status: StatusDeletionFailed, WorktreePath: "/data/worktrees/p1/t1",
		DeleteMode: sql.NullString{String: string(DeleteForce), Valid: true}}
	wt := newMockWorktree()
	wt.dirtyErr = errors.New("git status failed")
	tr := &tracer{}
	m := newP146TestManager(t, wrapTraceStore(store, tr), newMockProc(), wrapTraceWorktree(wt, tr), newMockOC(true))

	err := m.Retry(context.Background(), "t1", false)
	if err == nil {
		t.Fatal("expected dirty snapshot error")
	}
	if OpErrorCode(err) != codeGitError {
		t.Fatalf("code = %s, want git_error", OpErrorCode(err))
	}
	// 两笔写入先于 DirtyFiles（deleteResume 未达即失败，写入顺序仍冻结）。
	assertOrdered(t, tr, []traceOp{
		{src: "store", op: "SetTaskDeleteMode", key: "mode=force"},
		{src: "store", op: "UpdateTaskStatus", key: "status=deleting"},
		{src: "wt", op: "DirtyFiles", key: "/data/worktrees/p1/t1"},
		{src: "store", op: "UpdateTaskStatus", key: "status=deletion_failed"},
	}, "Retry.deletionFailed.reenter")
	// SetDeleteMode 写入生效（force 持久化，不被 Normal 重试覆盖）。
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.DeleteMode.Valid || row.DeleteMode.String != string(DeleteForce) {
		t.Fatalf("delete_mode = %+v, want force", row.DeleteMode)
	}
}

// TestP146_Activate_GuardReject_ViaLifecycle 非 suspended → invalid_state，零 CAS 零副作用。
func TestP146_Activate_GuardReject_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1") // status=active，非 suspended
	tr := &tracer{}
	m := newP146TestManager(t, wrapTraceStore(store, tr), newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected invalid_state on non-suspended activate")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("code = %s, want invalid_state", OpErrorCode(err))
	}
	// guard 拒绝：零 store 写、零外部副作用（含零 CAS）。
	assertNoSideEffects(t, tr, "Activate.guardReject.viaLifecycle")
	assertOpNever(t, tr, "store", "UpdateTaskStatusConditional", "Activate.guardReject.viaLifecycle")
}

// TestP146_Activate_Success_ViaLifecycle 镜像 p141 Activate 成功路径（同一 mock 组合）：
// CAS suspended→activating → serve/tui → active，全链 DB 写经 lifecycle 路径收敛。
func TestP146_Activate_Success_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	// 预置 anchor session 候选（resolveAnchorSession 走 GetSession 预检路径）。
	oc.sessions = []opencode.Session{{ID: "sess-anchor", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	_ = store.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-anchor", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1,
	})
	m := newP146TestManager(t, store, proc, wt, oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	waitStatus(t, store, "t1", StatusActive, 2*time.Second)
}
