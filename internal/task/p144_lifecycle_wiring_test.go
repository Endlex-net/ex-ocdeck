// p144_lifecycle_wiring_test.go 验证 P1.4.4 Manager facade 注入 LifecycleService 后
// Get/List/Archive/Restore 行为与 legacy 直连 store 路径 byte-equivalent
// （design.md D0:140 迁移第 4 步 + D0:148 api.TaskBackend 契约/OpError 映射冻结不变量）。
//
// 通过 mockAppAdapter 把 internal/task 测试用 mockStore 适配为 application ports
// （TaskRepository + TaskReadRepository），注入 LifecycleService，断言：
//   - Get/List 字段逐字还原（TaskRow ← TaskSnapshot）；
//   - Archive/Restore guard 放行/拒绝与 legacy 一致（codeInvalidState + 错误消息）；
//   - 同值 no-op 不推进 updated_at（对齐 task-lifecycle delta）；
//   - commit helper NoopPublisher 阶段无实际发布（调用位就绪）。
package task

import (
	"context"
	"database/sql"
	"testing"

	"ocdeck/internal/application"
	apptask "ocdeck/internal/application/task"
	ocdecksess "ocdeck/internal/domain/session"
	ocdecktask "ocdeck/internal/domain/task"
)

// mockAppAdapter 把测试用 mockStore（可经 traceStore 包装）适配为 application ports
// （TaskRepository + TaskReadRepository + SessionRepository）。
//
// GetTask 读 TaskStore.GetTask 返回 TaskRow，重建 domain guard 视图（status/init_status）。
// P1.4.6：Create/Retry/Activate 写方法（CreateTask/UpdateTask*/CommitCreated/SetTaskDeleteMode）
// 委托 TaskStore（sql.NullString ↔ *string / string ↔ domain 枚举转换）。
// 其余 TaskRepository 方法 panic（本测试包仅用列出的方法）。
type mockAppAdapter struct {
	s TaskStore
}

func (a *mockAppAdapter) GetTask(ctx context.Context, id string) (*ocdecktask.Task, error) {
	row, err := a.s.GetTask(ctx, id)
	if err != nil {
		// mockStore 未命中返回 fmt.Errorf("not found")，归一化为 application.ErrTaskNotFound
		// （对齐 sqlite adapter 把 sql.ErrNoRows 映射为 sentinel）。
		if err.Error() == "not found" {
			return nil, application.ErrTaskNotFound
		}
		return nil, err
	}
	return rehydrateGuardView(row), nil
}

func (a *mockAppAdapter) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.s.ArchiveTask(ctx, id)
}

func (a *mockAppAdapter) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.s.RestoreTask(ctx, id)
}

func (a *mockAppAdapter) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	row, err := a.s.GetTask(ctx, id)
	if err != nil {
		if err.Error() == "not found" {
			return application.TaskSnapshot{}, application.ErrTaskNotFound
		}
		return application.TaskSnapshot{}, err
	}
	return taskRowToSnapshot(row), nil
}

func (a *mockAppAdapter) ListTasksByProject(ctx context.Context, projectID string) ([]application.TaskSnapshot, error) {
	rows, err := a.s.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]application.TaskSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, taskRowToSnapshot(r))
	}
	return out, nil
}

// --- TaskRepository 写方法（P1.4.6 Create/Retry/Activate wiring 测试用） ---

func (a *mockAppAdapter) CreateTask(ctx context.Context, row application.TaskSnapshot) error {
	return a.s.CreateTask(ctx, taskSnapshotToTaskRow(row))
}

func (a *mockAppAdapter) UpdateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.s.UpdateTaskStatus(ctx, id, string(status), ptrToNullString(lastError))
}

func (a *mockAppAdapter) UpdateTaskStatusConditional(ctx context.Context, id string, fromStatus, toStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.s.UpdateTaskStatusConditional(ctx, id, string(fromStatus), string(toStatus), ptrToNullString(lastError))
}

func (a *mockAppAdapter) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (application.MutationResult, error) {
	return a.s.UpdateTaskEnvSnapshot(ctx, id, ptrToNullString(envSnapshot))
}

func (a *mockAppAdapter) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return a.s.UpdateTaskLastPort(ctx, id, port)
}

func (a *mockAppAdapter) SetTaskDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (application.MutationResult, error) {
	return a.s.SetTaskDeleteMode(ctx, id, string(mode))
}

func (a *mockAppAdapter) CommitCreated(ctx context.Context, taskID string, expectedStatus ocdecktask.Status, initStatus ocdecktask.InitStatus) (application.TransitionResult, error) {
	return a.s.CommitCreated(ctx, taskID, string(expectedStatus), string(initStatus))
}

// --- SessionRepository（P1.4.6 Activate wiring：SSE align / 锚定 claim 用） ---

func (a *mockAppAdapter) Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (application.ClaimResult, error) {
	return a.s.ClaimTaskSession(ctx, taskID, string(obs.ID), obs.CreatedAt, obs.FirstSeenAt, obs.UpdatedAt, obs.ParentID)
}

func (a *mockAppAdapter) TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (application.MutationResult, error) {
	return a.s.TouchOwnedTaskSession(ctx, taskID, string(sessionID), lastSeenAt)
}

func (a *mockAppAdapter) DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error) {
	return a.s.DeleteTaskSession(ctx, taskID, string(sessionID))
}

func (a *mockAppAdapter) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	listed := make([]SessionObservation, 0, len(observed))
	for _, o := range observed {
		listed = append(listed, SessionObservation{
			SessionID: string(o.ID), CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, ParentID: o.ParentID,
		})
	}
	return a.s.AlignTaskSessions(ctx, taskID, fromDomainAlignMode(mode), listed, complete, notice)
}

func (a *mockAppAdapter) OwnedSessions(ctx context.Context, taskID string) ([]ocdecksess.ID, error) {
	panic("not used")
}

func (a *mockAppAdapter) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	panic("not used")
}

// 以下 TaskRepository 方法本测试包不调用（panic 占位，签名须匹配 application.TaskRepository）。
func (a *mockAppAdapter) UpdateTaskNotice(context.Context, string, *string) (application.MutationResult, error) {
	panic("not used")
}

// --- P1.4.8 Delete/Reconcile/notice-CAS/init_status wiring 测试用（委托 TaskStore） ---

func (a *mockAppAdapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	return a.s.UpdateTaskNoticeCAS(ctx, id, ptrToNullString(expected), ptrToNullString(newNotice))
}

func (a *mockAppAdapter) BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (application.TransitionResult, error) {
	statuses := make([]string, len(fromStatuses))
	for i, s := range fromStatuses {
		statuses[i] = string(s)
	}
	return a.s.BeginDeleteIntent(ctx, id, string(mode), statuses)
}

func (a *mockAppAdapter) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	return a.s.DeleteTask(ctx, id)
}

func (a *mockAppAdapter) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.s.ClaimInitRun(ctx, taskID)
}

func (a *mockAppAdapter) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.s.ClaimInitRerun(ctx, taskID)
}

func (a *mockAppAdapter) FinishInitRun(ctx context.Context, taskID string, status ocdecktask.InitStatus, initError *string) (application.MutationResult, error) {
	return a.s.FinishInitRun(ctx, taskID, string(status), ptrToNullString(initError))
}

func (a *mockAppAdapter) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return a.s.ConvergeInterruptedInitRuns(ctx)
}

// taskRowToSnapshot / nullable ptr 映射复用生产 adapters.go 中的同名实现
//（P1.4.5 storeAlignPortsAdapter 引入），此处不再维护平行拷贝。

// newTestManagerWithLifecycleService 构造注入 LifecycleService 的 Manager（复用 newTestManager builder）。
func newTestManagerWithLifecycleService(t *testing.T, store TaskStore) *Manager {
	t.Helper()
	adapter := &mockAppAdapter{s: store.(*mockStore)}
	svc := apptask.New(apptask.Options{
		Tasks:   adapter,
		Read:    adapter,
		Publish: apptask.NoopPublisher{},
	})
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.lifecycle = svc
	return m
}

func TestP144_GetList_EquivalentWithLifecycle(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{
		ID: "t1", ProjectID: "p1", Name: "n1", Branch: "b1", Status: StatusSuspended,
		WorktreePath: "/wt", BaseRef: "refs/heads/main",
		LastPort:   sql.NullInt64{Int64: 12345, Valid: true},
		LastError:  sql.NullString{String: "err", Valid: true},
		InitStatus: InitStatusNone,
	}
	store.tasks["t2"] = TaskRow{ID: "t2", ProjectID: "p1", Name: "n2", Status: StatusActive}

	m := newTestManagerWithLifecycleService(t, store)

	// Get：字段逐字还原
	got, err := m.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get err: %v", err)
	}
	if got.ID != "t1" || got.Name != "n1" || got.Status != StatusSuspended ||
		got.LastPort.Int64 != 12345 || !got.LastPort.Valid ||
		got.LastError.String != "err" || !got.LastError.Valid ||
		got.BaseRef != "refs/heads/main" || got.InitStatus != InitStatusNone {
		t.Fatalf("Get row = %+v, want fields restored from snapshot", got)
	}

	// Get 未命中 → codeNotFound
	_, err = m.Get(context.Background(), "missing")
	if err == nil || OpErrorCode(err) != codeNotFound {
		t.Fatalf("Get missing: code=%v want not_found, err=%v", OpErrorCode(err), err)
	}

	// List：返回项目下全部任务
	list, err := m.List(context.Background(), "p1")
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
}

func TestP144_Archive_EquivalentWithLifecycle(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		initStatus string
		wantCode   string
		errSub     string
	}{
		{"suspended/none allowed", StatusSuspended, InitStatusNone, "", ""},
		{"suspended/pending rejected init", StatusSuspended, InitStatusPending, codeInvalidState, "init in progress (init_status=pending)"},
		{"active rejected status", StatusActive, InitStatusNone, codeInvalidState, "archive requires suspended, got active"},
		{"active/pending status-first", StatusActive, InitStatusPending, codeInvalidState, "archive requires suspended, got active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, InitStatus: c.initStatus, WorktreePath: "/wt"}
			m := newTestManagerWithLifecycleService(t, store)

			err := m.Archive(context.Background(), "t1")
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("%s: expected allow, got %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

func TestP144_Restore_EquivalentWithLifecycle(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantCode string
		errSub   string
	}{
		{"archived allowed", StatusArchived, "", ""},
		{"suspended rejected", StatusSuspended, codeInvalidState, "restore requires archived, got suspended"},
		{"active rejected", StatusActive, codeInvalidState, "restore requires archived, got active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: c.status, WorktreePath: "/wt"}
			m := newTestManagerWithLifecycleService(t, store)

			err := m.Restore(context.Background(), "t1")
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("%s: expected allow, got %v", c.name, err)
				}
				return
			}
			assertCodeEq(t, c.name, err, c.wantCode)
			assertErrContains(t, c.name, c.errSub, err)
		})
	}
}

// TestP144_ArchiveRestore_NotFound_Equivalent 断言注入 LifecycleService 后
// Archive/Restore 对不存在任务返回 codeNotFound（与 legacy byte-equivalent）。
func TestP144_ArchiveRestore_NotFound_Equivalent(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main"})
	m := newTestManagerWithLifecycleService(t, store)

	if err := m.Archive(context.Background(), "missing"); OpErrorCode(err) != codeNotFound {
		t.Fatalf("Archive missing: code=%v want not_found, err=%v", OpErrorCode(err), err)
	}
	if err := m.Restore(context.Background(), "missing"); OpErrorCode(err) != codeNotFound {
		t.Fatalf("Restore missing: code=%v want not_found, err=%v", OpErrorCode(err), err)
	}
}
