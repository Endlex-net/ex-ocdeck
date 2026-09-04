package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
)

// --- P1-F1：Reconcile 有效模式预检 fail-closed（add-local-path-task-mode D2/D11） ---

// TestReconcile_IllegalModeCombo_FailClosedZeroSideEffects 验证非法 kind/mode 组合下
// Reconcile 在一切 converge/debt/进程操作之前 fail-closed：返回 internal、零状态变化、
// 零进程副作用（无 kill、无新建会话），persist 与 kill 两种 policy 均成立。
func TestReconcile_IllegalModeCombo_FailClosedZeroSideEffects(t *testing.T) {
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"unknown kind", "weird", TaskModeWorktree},
		{"unknown mode", ProjectKindRepo, "bogus"},
	}
	policies := []struct {
		name   string
		policy config.ShutdownPolicy
	}{
		{"persist", config.ShutdownPersist},
		{"kill_on_start", config.ShutdownKillOnStart},
	}
	for _, pol := range policies {
		for _, tc := range cases {
			t.Run(pol.name+"/"+tc.name, func(t *testing.T) {
				store := newMockStore()
				store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: tc.kind})
				// active 任务 + 已注册 runtime 会话：若预检未拦截，persist/kill 矩阵必产生
				// kill/状态写入副作用（pre-condition 供对照）。
				store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/a",
					Status: StatusActive, WorktreePath: "/wt", Mode: tc.mode}
				proc := newMockProc()
				proc.sessions[runtimeSessionName("t1")] = true
				m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
				m.cfg.ShutdownPolicy = pol.policy

				err := m.Reconcile(context.Background())
				if err == nil {
					t.Fatal("Reconcile with illegal kind/mode combo: want error, got nil")
				}
				if OpErrorCode(err) != codeInternal {
					t.Errorf("error code = %q, want internal (fail-closed)", OpErrorCode(err))
				}

				// 零状态变化。
				after := store.tasks["t1"]
				if after.Status != StatusActive {
					t.Errorf("status = %q, want unchanged active（零状态副作用）", after.Status)
				}
				if after.LastError.Valid {
					t.Errorf("last_error = %q, want unset（零状态副作用）", after.LastError.String)
				}
				// 零进程副作用：预检先于 ListSessions/矩阵，不得有任何 kill/新建。
				if kills := proc.killOrderSnapshot(); len(kills) != 0 {
					t.Errorf("kill calls = %v, want empty（零进程副作用）", kills)
				}
				if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
					t.Errorf("new session calls = %v, want empty（零进程副作用）", created)
				}
			})
		}
	}
}

// --- P1-F2：deleteResume 解析失败落账可靠（design.md D2 重入路径分层） ---

// finalizeFailStore 包装 mockStore：可注入 UpdateTaskStatus/UpdateTaskStatusConditional
// 写失败 / CAS 未命中。
type finalizeFailStore struct {
	*mockStore
	updateStatusErr error
	matchedOverride *bool
}

func (s *finalizeFailStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	if s.updateStatusErr != nil {
		return application.TransitionResult{}, s.updateStatusErr
	}
	if s.matchedOverride != nil {
		return application.TransitionResult{MutationResult: application.MutationResult{Matched: *s.matchedOverride, Changed: *s.matchedOverride}}, nil
	}
	return s.mockStore.UpdateTaskStatus(ctx, id, status, lastError)
}

func (s *finalizeFailStore) UpdateTaskStatusConditional(ctx context.Context, id, from, to string, lastError sql.NullString) (application.TransitionResult, error) {
	if s.updateStatusErr != nil {
		return application.TransitionResult{}, s.updateStatusErr
	}
	if s.matchedOverride != nil {
		return application.TransitionResult{MutationResult: application.MutationResult{Matched: *s.matchedOverride, Changed: *s.matchedOverride}}, nil
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, from, to, lastError)
}

// newIllegalDeleteResumeManager 构造 dir+worktree 非法组合的 deleting 任务（已提交删除意图）。
func newIllegalDeleteResumeManager(t *testing.T) (*Manager, *finalizeFailStore) {
	t.Helper()
	store := &finalizeFailStore{mockStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/proj", DefaultBranch: "", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusDeleting, WorktreePath: "/proj", Mode: TaskModeWorktree}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	return m, store
}

// TestDeleteResume_IllegalCombo_CanceledCtxStillFinalizes 验证解析失败落账用非取消有界 ctx：
// 调用方 request ctx 已取消时行仍落 deletion_failed + last_error，返回 internal（解析错误）。
func TestDeleteResume_IllegalCombo_CanceledCtxStillFinalizes(t *testing.T) {
	m, store := newIllegalDeleteResumeManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	row := store.tasks["t1"]
	err := m.deleteResume(ctx, row, DeleteNormal, nil)
	if err == nil {
		t.Fatal("deleteResume with illegal combo: want error, got nil")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("error code = %q, want internal", OpErrorCode(err))
	}
	after := store.tasks["t1"]
	if after.Status != StatusDeletionFailed {
		t.Errorf("status = %q, want deletion_failed（取消 ctx 下落账仍须成功）", after.Status)
	}
	if !after.LastError.Valid || after.LastError.String == "" {
		t.Error("last_error must be persisted with resolution failure")
	}
}

// TestDeleteResume_IllegalCombo_WriteFailure_PropagatesBoth 验证落账写失败时返回错误
// 同时包含原始解析错误与落账错误（MUST NOT 吞错），行停留 deleting。
func TestDeleteResume_IllegalCombo_WriteFailure_PropagatesBoth(t *testing.T) {
	m, store := newIllegalDeleteResumeManager(t)
	writeSentinel := errors.New("db write failed")
	store.updateStatusErr = writeSentinel

	row := store.tasks["t1"]
	err := m.deleteResume(context.Background(), row, DeleteNormal, nil)
	if err == nil {
		t.Fatal("deleteResume with finalize write failure: want error, got nil")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("error code = %q, want internal", OpErrorCode(err))
	}
	if !errors.Is(err, writeSentinel) {
		t.Errorf("error chain missing finalize write error (errors.Is sentinel): %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "must not use mode") || !strings.Contains(msg, "finalize deletion_failed") || !strings.Contains(msg, "db write failed") {
		t.Errorf("error = %q, want both resolution and finalize errors", msg)
	}
	if after := store.tasks["t1"]; after.Status != StatusDeleting {
		t.Errorf("status = %q, want unchanged deleting（落账失败不得伪造收敛）", after.Status)
	}
}

// TestDeleteResume_IllegalCombo_CASMiss_ReportsFailure 验证落账 CAS 未命中（行已不存在/
// 并发变更）时返回落账失败错误，MUST NOT 伪装落账成功。
func TestDeleteResume_IllegalCombo_CASMiss_ReportsFailure(t *testing.T) {
	m, store := newIllegalDeleteResumeManager(t)
	no := false
	store.matchedOverride = &no

	row := store.tasks["t1"]
	err := m.deleteResume(context.Background(), row, DeleteNormal, nil)
	if err == nil {
		t.Fatal("deleteResume with finalize CAS miss: want error, got nil")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("error code = %q, want internal", OpErrorCode(err))
	}
	if !strings.Contains(err.Error(), "CAS not matched") {
		t.Errorf("error = %q, want CAS-not-matched finalize failure", err.Error())
	}
}

// TestFinalizeDeletionFailed_MovedOutDeleting_NoOverwrite 验证落账为 CAS 条件写
// （deleting → deletion_failed，评审 C-F1）：行状态已迁出 deleting（并发收敛）时落账
// 返回未命中错误，且 MUST NOT 无条件覆盖已收敛状态与 last_error。
func TestFinalizeDeletionFailed_MovedOutDeleting_NoOverwrite(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/proj", DefaultBranch: "", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusDeleting, WorktreePath: "/proj", Mode: TaskModeWorktree}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 并发收敛：行状态已离开 deleting（模拟其他 actor 已完成收敛）。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })

	if err := m.finalizeDeletionFailed("t1", "boom"); err == nil {
		t.Fatal("finalizeDeletionFailed with moved-out status: want CAS miss error, got nil")
	}
	// 已收敛状态不得被覆盖，last_error 不得写入。
	assertStatus(t, store, "t1", StatusSuspended)
	if row := store.tasks["t1"]; row.LastError.Valid {
		t.Errorf("last_error = %q, want unset（CAS 未命中不得写列）", row.LastError.String)
	}
}

// getProjectErrStore 在 finalizeFailStore 之上注入 GetProject 失败（真实实现经
// QueryRowContext 会响应 ctx 取消；mock 忽略 ctx，故必须显式包装才能走到该路径）。
type getProjectErrStore struct {
	*finalizeFailStore
	getProjectErr error
}

func (s *getProjectErrStore) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	return ProjectRow{}, s.getProjectErr
}

// TestDeleteResume_GetProjectCanceled_FinalizesDeletionFailed 验证 GetProject 失败
// （含 ctx 取消）路径：落账用非取消有界 ctx，行落 deletion_failed + last_error，
// 返回 not_found 且错误含取消与解析上下文（P1-F2 剩余项）。
func TestDeleteResume_GetProjectCanceled_FinalizesDeletionFailed(t *testing.T) {
	store := &finalizeFailStore{mockStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/proj", DefaultBranch: "", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusDeleting, WorktreePath: "/proj", Mode: TaskModeWorktree}
	gpStore := &getProjectErrStore{finalizeFailStore: store, getProjectErr: context.Canceled}
	m := newTestManager(t, gpStore, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.deleteResume(context.Background(), store.tasks["t1"], DeleteNormal, nil)
	if err == nil {
		t.Fatal("deleteResume with GetProject failure: want error, got nil")
	}
	if OpErrorCode(err) != codeNotFound {
		t.Errorf("error code = %q, want not_found（现状映射保留）", OpErrorCode(err))
	}
	if !strings.Contains(err.Error(), "project not found for delete resume") || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %q, want cancellation + resolution context", err.Error())
	}
	after := store.tasks["t1"]
	if after.Status != StatusDeletionFailed {
		t.Errorf("status = %q, want deletion_failed（取消 ctx 下落账仍须成功）", after.Status)
	}
	if !after.LastError.Valid || !strings.Contains(after.LastError.String, "context canceled") {
		t.Errorf("last_error = %v, want persisted cancellation context", after.LastError)
	}
}

// TestDeleteResume_GetProjectFail_FinalizeWriteFail_PropagatesBoth 验证 GetProject 失败
// 且落账写失败时返回错误同时含读取错误与落账错误（errors.Join），行停留 deleting。
func TestDeleteResume_GetProjectFail_FinalizeWriteFail_PropagatesBoth(t *testing.T) {
	store := &finalizeFailStore{mockStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/proj", DefaultBranch: "", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusDeleting, WorktreePath: "/proj", Mode: TaskModeWorktree}
	store.updateStatusErr = errors.New("db write failed")
	gpStore := &getProjectErrStore{finalizeFailStore: store, getProjectErr: context.Canceled}
	m := newTestManager(t, gpStore, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.deleteResume(context.Background(), store.tasks["t1"], DeleteNormal, nil)
	if err == nil {
		t.Fatal("deleteResume with GetProject + finalize failure: want error, got nil")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("error code = %q, want internal（落账失败升级）", OpErrorCode(err))
	}
	msg := err.Error()
	for _, want := range []string{"project not found for delete resume", "context canceled", "finalize deletion_failed", "db write failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want contain %q", msg, want)
		}
	}
	if after := store.tasks["t1"]; after.Status != StatusDeleting {
		t.Errorf("status = %q, want unchanged deleting（落账失败不得伪造收敛）", after.Status)
	}
}

// --- P1-F4：Reconcile 业务快照在 converge/restore/replay 之后刷新 ---

// TestReconcile_ActivatingWithCompleteDebt_FreshSnapshot 验证 activating 任务 + complete
// recovery debt：debt 重放把任务收敛为 suspended + last_error=cause 后，后续矩阵使用
// 刷新后的快照（P1-F4）——Reconcile 成功返回（无 stale activating 的 CAS miss 噪音）、
// 最终状态 suspended、原 recovery cause 的 last_error 不被覆盖。
func TestReconcile_ActivatingWithCompleteDebt_FreshSnapshot(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/a",
		Status: StatusActivating, WorktreePath: "/wt", BaseRef: "refs/heads/main", Mode: TaskModeWorktree}
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "recovery budget exhausted", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed recovery debt: %v", err)
	}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v, want nil（stale 快照会对已收敛的 activating 走清理分支产生 CAS miss 噪音）", err)
	}
	after := store.tasks["t1"]
	if after.Status != StatusSuspended {
		t.Errorf("status = %q, want suspended（complete debt 重放收敛）", after.Status)
	}
	if !after.LastError.Valid || after.LastError.String != "recovery budget exhausted" {
		t.Errorf("last_error = %v, want preserved recovery cause（不得被矩阵覆盖）", after.LastError)
	}
	// 零进程副作用：任务已收敛为 suspended 且无会话，矩阵不得新建会话。
	if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
		t.Errorf("new session calls = %v, want empty", created)
	}
}
