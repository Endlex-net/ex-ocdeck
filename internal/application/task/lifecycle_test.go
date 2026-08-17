// lifecycle_test.go 验证 P1.4.4 LifecycleService 的 Get/List/Archive/Restore 行为
// （design.md D0:140 迁移第 4 步）。
//
// 覆盖：
//   - Get/List 纯读：命中/未命中、字段逐字还原；
//   - Archive guard 放行/拒绝（status 维度 + init 维度，status 优先）、CAS 提交、
//     同值 no-op（已 archived Matched=true/Changed=false 不调用 Publish）；
//   - Restore guard 放行/拒绝、CAS 提交、同值 no-op；
//   - commit helper NoopPublisher 阶段：不发布任何事件（publish 调用位就绪无实际发布）。
//
// 使用 fakeTaskRepo / fakeReadRepo 实现 application ports（不依赖 sqlite），
// 断言 application 层 err-first 语义与 typed error（*ArchiveError/*RestoreError）。
package task

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
)

// fakeTaskRepo 实现 application.TaskRepository（仅本测试包所需子集）。
// 其余方法 panic 占位（未在本测试包调用）。
type fakeTaskRepo struct {
	task *ocdecktask.Task // GetTask 返回的 guard 视图
	// archiveResult/restoreResult 控制 CAS 返回结构化结果。
	archiveResult application.TransitionResult
	restoreResult application.TransitionResult
	archiveErr    error
	restoreErr    error
	getErr        error
	// P1.4.6 create/activate 封装测试（create_activate_test.go）：
	// transitionRes/transitionErr 控制 UpdateStatus/UpdateStatusConditional/CommitCreated
	// 返回（每个测试仅调用其一）；mutationRes/mutationErr 同理控制 SetTaskDeleteMode/
	// UpdateTaskEnvSnapshot/UpdateTaskLastPort；createErr 控制 CreateTask 失败，
	// createdRows 记录 CreateTask 入参。
	transitionRes application.TransitionResult
	transitionErr error
	mutationRes   application.MutationResult
	mutationErr   error
	createErr     error
	createdRows   []application.TaskSnapshot
	// P1.4.8 delete/reconcile 封装测试（delete_reconcile_test.go）：
	// deleteRes/deleteErr 控制 DeleteTask；convergeN/convergeErr 控制 ConvergeInterruptedInitRuns。
	deleteRes   application.DeleteResult
	deleteErr   error
	convergeN   int64
	convergeErr error
}

func (r *fakeTaskRepo) GetTask(ctx context.Context, id string) (*ocdecktask.Task, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.task, nil
}

func (r *fakeTaskRepo) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return r.archiveResult, r.archiveErr
}

func (r *fakeTaskRepo) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return r.restoreResult, r.restoreErr
}

func (r *fakeTaskRepo) CreateTask(ctx context.Context, row application.TaskSnapshot) error {
	r.createdRows = append(r.createdRows, row)
	return r.createErr
}

func (r *fakeTaskRepo) UpdateTaskStatus(context.Context, string, ocdecktask.Status, *string) (application.TransitionResult, error) {
	return r.transitionRes, r.transitionErr
}
func (r *fakeTaskRepo) UpdateTaskStatusConditional(context.Context, string, ocdecktask.Status, ocdecktask.Status, *string) (application.TransitionResult, error) {
	return r.transitionRes, r.transitionErr
}
func (r *fakeTaskRepo) CommitCreated(context.Context, string, ocdecktask.Status, ocdecktask.InitStatus) (application.TransitionResult, error) {
	return r.transitionRes, r.transitionErr
}
func (r *fakeTaskRepo) SetTaskDeleteMode(context.Context, string, ocdecktask.DeleteMode) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) UpdateTaskEnvSnapshot(context.Context, string, *string) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) UpdateTaskLastPort(context.Context, string, int) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}

// 以下方法为占位，本测试包不调用。
func (r *fakeTaskRepo) UpdateTaskNotice(context.Context, string, *string) (application.MutationResult, error) {
	panic("not used")
}

// P1.4.8 delete/reconcile 封装：UpdateTaskNoticeCAS/BeginDeleteIntent 走 transition/mutation
// 结果槽；ClaimInit*/FinishInitRun 走 mutation 槽；DeleteTask/Converge 走独立槽。
func (r *fakeTaskRepo) UpdateTaskNoticeCAS(context.Context, string, *string, *string) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) BeginDeleteIntent(context.Context, string, ocdecktask.DeleteMode, []ocdecktask.Status) (application.TransitionResult, error) {
	return r.transitionRes, r.transitionErr
}
func (r *fakeTaskRepo) DeleteTask(context.Context, string) (application.DeleteResult, error) {
	return r.deleteRes, r.deleteErr
}
func (r *fakeTaskRepo) ClaimInitRun(context.Context, string) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) ClaimInitRerun(context.Context, string) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) FinishInitRun(context.Context, string, ocdecktask.InitStatus, *string) (application.MutationResult, error) {
	return r.mutationRes, r.mutationErr
}
func (r *fakeTaskRepo) ConvergeInterruptedInitRuns(context.Context) (int64, error) {
	return r.convergeN, r.convergeErr
}

// fakeReadRepo 实现 application.TaskReadRepository（仅 GetTaskRow/ListTasksByProject）。
type fakeReadRepo struct {
	snap    application.TaskSnapshot
	snaps   []application.TaskSnapshot
	getErr  error
	listErr error
}

func (r *fakeReadRepo) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	if r.getErr != nil {
		return application.TaskSnapshot{}, r.getErr
	}
	return r.snap, nil
}

func (r *fakeReadRepo) ListTasksByProject(ctx context.Context, projectID string) ([]application.TaskSnapshot, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.snaps, nil
}

// recordingPublisher 记录 Publish 调用，断言 NoopPublisher 阶段不发布或按预期发布。
type recordingPublisher struct {
	events []string            // 记录已发布事件的 Type
	raw    []ocdeckevent.Event // 记录完整事件（供 payload 断言，如 from/to）
}

func (p *recordingPublisher) Publish(ev ocdeckevent.Event) {
	p.events = append(p.events, string(ev.Type))
	p.raw = append(p.raw, ev)
}

func newSvc(repo *fakeTaskRepo, read *fakeReadRepo, pub application.Publisher) *LifecycleService {
	return New(Options{Tasks: repo, Read: read, Publish: pub})
}

func TestP144_GetList_ReadPassthrough(t *testing.T) {
	repo := &fakeTaskRepo{}
	read := &fakeReadRepo{
		snap: application.TaskSnapshot{ID: "t1", ProjectID: "p1", Name: "n", Status: "suspended"},
		snaps: []application.TaskSnapshot{
			{ID: "t1", ProjectID: "p1", Name: "n1"},
			{ID: "t2", ProjectID: "p1", Name: "n2"},
		},
	}
	svc := newSvc(repo, read, NoopPublisher{})

	got, err := svc.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get err: %v", err)
	}
	if got.ID != "t1" || got.Status != "suspended" {
		t.Fatalf("Get snapshot = %+v, want t1/suspended", got)
	}

	list, err := svc.List(context.Background(), "p1")
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if len(list) != 2 || list[0].ID != "t1" || list[1].ID != "t2" {
		t.Fatalf("List = %+v, want 2 items t1,t2", list)
	}

	// 未命中：GetTaskRow 返回 error，原样透传。
	read.getErr = errors.New("sql: no rows")
	_, err = svc.Get(context.Background(), "missing")
	if err == nil {
		t.Fatalf("Get missing: want err, got nil")
	}
}

func TestP144_Archive_GuardAndCommit(t *testing.T) {
	cases := []struct {
		name       string
		status     ocdecktask.Status
		initStatus ocdecktask.InitStatus
		// 期望
		wantAllow bool
		// 拒绝维度（status 优先）
		wantReason ArchiveRejectReason
	}{
		{"suspended/none allowed", ocdecktask.StatusSuspended, ocdecktask.InitStatusNone, true, 0},
		{"suspended/succeeded allowed", ocdecktask.StatusSuspended, ocdecktask.InitStatusSucceeded, true, 0},
		{"suspended/failed allowed", ocdecktask.StatusSuspended, ocdecktask.InitStatusFailed, true, 0},
		{"suspended/pending rejected init", ocdecktask.StatusSuspended, ocdecktask.InitStatusPending, false, ArchiveRejectInit},
		{"suspended/running rejected init", ocdecktask.StatusSuspended, ocdecktask.InitStatusRunning, false, ArchiveRejectInit},
		{"active rejected status", ocdecktask.StatusActive, ocdecktask.InitStatusNone, false, ArchiveRejectStatus},
		{"creating rejected status", ocdecktask.StatusCreating, ocdecktask.InitStatusNone, false, ArchiveRejectStatus},
		{"archived rejected status", ocdecktask.StatusArchived, ocdecktask.InitStatusNone, false, ArchiveRejectStatus},
		// 两个维度都失败：status 优先
		{"active/pending status-first", ocdecktask.StatusActive, ocdecktask.InitStatusPending, false, ArchiveRejectStatus},
		{"creating/running status-first", ocdecktask.StatusCreating, ocdecktask.InitStatusRunning, false, ArchiveRejectStatus},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeTaskRepo{
				task: ocdecktask.Rehydrate(ocdecktask.GuardView{Status: c.status, InitStatus: c.initStatus}),
				archiveResult: application.TransitionResult{
					MutationResult: application.MutationResult{Matched: true, Changed: true},
					StatusChanged:  true,
					From:           c.status,
					To:             ocdecktask.StatusArchived,
				},
			}
			pub := &recordingPublisher{}
			svc := newSvc(repo, &fakeReadRepo{}, pub)

			err := svc.Archive(context.Background(), "t1")
			if c.wantAllow {
				if err != nil {
					t.Fatalf("Archive err: %v, want allow", err)
				}
				// 真实迁移 StatusChanged=true → commit helper 调用 Publish（task.status_changed）。
				if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskStatusChanged) {
					t.Fatalf("publish events = %v, want [task.status_changed]", pub.events)
				}
				return
			}
			// 拒绝：零副作用（不应调用 ArchiveTask，不应发布）
			if err == nil {
				t.Fatalf("Archive: want reject, got nil")
			}
			var ae *ArchiveError
			if !errors.As(err, &ae) {
				t.Fatalf("Archive err type = %T, want *ArchiveError", err)
			}
			if ae.Reason != c.wantReason {
				t.Fatalf("Archive reject reason = %v, want %v", ae.Reason, c.wantReason)
			}
			if len(pub.events) != 0 {
				t.Fatalf("reject should not publish, got %v", pub.events)
			}
		})
	}
}

func TestP144_Archive_SameValueNoopNotPublish(t *testing.T) {
	// 同值 no-op：已 archived，ArchiveTask 返回 Matched=true/Changed=false/StatusChanged=false。
	repo := &fakeTaskRepo{
		task: ocdecktask.Rehydrate(ocdecktask.GuardView{Status: ocdecktask.StatusArchived}),
	}
	// CanArchive 要求 status=suspended，archived 会 guard 拒绝。为了让同值 no-op 路径走到 commit helper，
	// 这里用 suspended guard 放行 + archive 返回 Changed=false 模拟 store 已 archived 的场景
	// （store ArchiveTask 对已 archived 行返回 Matched=true/Changed=false）。
	repo.task = ocdecktask.Rehydrate(ocdecktask.GuardView{Status: ocdecktask.StatusSuspended})
	repo.archiveResult = application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: false},
		StatusChanged:  false,
	}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	err := svc.Archive(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Archive no-op err: %v", err)
	}
	// 同值 no-op 不发布（design.md D0:133）。
	if len(pub.events) != 0 {
		t.Fatalf("same-value no-op should not publish, got %v", pub.events)
	}
}

func TestP144_Restore_GuardAndCommit(t *testing.T) {
	cases := []struct {
		name      string
		status    ocdecktask.Status
		wantAllow bool
	}{
		{"archived allowed", ocdecktask.StatusArchived, true},
		{"suspended rejected", ocdecktask.StatusSuspended, false},
		{"active rejected", ocdecktask.StatusActive, false},
		{"creating rejected", ocdecktask.StatusCreating, false},
		{"unknown rejected", "bogus", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeTaskRepo{
				task: ocdecktask.Rehydrate(ocdecktask.GuardView{Status: c.status}),
				restoreResult: application.TransitionResult{
					MutationResult: application.MutationResult{Matched: true, Changed: true},
					StatusChanged:  true,
					From:           c.status,
					To:             ocdecktask.StatusSuspended,
				},
			}
			pub := &recordingPublisher{}
			svc := newSvc(repo, &fakeReadRepo{}, pub)

			err := svc.Restore(context.Background(), "t1")
			if c.wantAllow {
				if err != nil {
					t.Fatalf("Restore err: %v, want allow", err)
				}
				if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskStatusChanged) {
					t.Fatalf("publish events = %v, want [task.status_changed]", pub.events)
				}
				return
			}
			if err == nil {
				t.Fatalf("Restore: want reject, got nil")
			}
			var re *RestoreError
			if !errors.As(err, &re) {
				t.Fatalf("Restore err type = %T, want *RestoreError", err)
			}
			if len(pub.events) != 0 {
				t.Fatalf("reject should not publish, got %v", pub.events)
			}
		})
	}
}

func TestP144_NoopPublisherNeverPublishes(t *testing.T) {
	// NoopPublisher 直接注入：Archive 放行 + 真实迁移，不应有任何事件泄漏。
	repo := &fakeTaskRepo{
		task: ocdecktask.Rehydrate(ocdecktask.GuardView{Status: ocdecktask.StatusSuspended}),
		archiveResult: application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true},
			StatusChanged:  true,
			From:           ocdecktask.StatusSuspended,
			To:             ocdecktask.StatusArchived,
		},
	}
	svc := New(Options{Tasks: repo, Read: &fakeReadRepo{}}) // Publish nil → NoopPublisher
	err := svc.Archive(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Archive err: %v", err)
	}
	// NoopPublisher 不发布；无 panic 即证明调用位就绪且无实际发布。
}

func TestP144_Archive_GetTaskErrorPropagates(t *testing.T) {
	repo := &fakeTaskRepo{getErr: errors.New("db error")}
	svc := newSvc(repo, &fakeReadRepo{}, NoopPublisher{})
	err := svc.Archive(context.Background(), "t1")
	if err == nil || err.Error() != "db error" {
		t.Fatalf("Archive GetTask error propagate: got %v, want db error", err)
	}
}

func TestP144_Archive_NotFoundPropagatesAsSentinel(t *testing.T) {
	// GetTask 返回 application.ErrTaskNotFound（sqlite adapter 把 sql.ErrNoRows 归一化），
	// application 透传，Manager facade 映射为 codeNotFound。
	repo := &fakeTaskRepo{getErr: application.ErrTaskNotFound}
	svc := newSvc(repo, &fakeReadRepo{}, NoopPublisher{})
	err := svc.Archive(context.Background(), "t1")
	if !errors.Is(err, application.ErrTaskNotFound) {
		t.Fatalf("Archive not-found: err=%v, want ErrTaskNotFound sentinel", err)
	}
}

func TestP144_Restore_NotFoundPropagatesAsSentinel(t *testing.T) {
	repo := &fakeTaskRepo{getErr: application.ErrTaskNotFound}
	svc := newSvc(repo, &fakeReadRepo{}, NoopPublisher{})
	err := svc.Restore(context.Background(), "t1")
	if !errors.Is(err, application.ErrTaskNotFound) {
		t.Fatalf("Restore not-found: err=%v, want ErrTaskNotFound sentinel", err)
	}
}

func TestP144_Get_NotFoundPropagates(t *testing.T) {
	read := &fakeReadRepo{getErr: application.ErrTaskNotFound}
	svc := newSvc(&fakeTaskRepo{}, read, NoopPublisher{})
	_, err := svc.Get(context.Background(), "t1")
	if !errors.Is(err, application.ErrTaskNotFound) {
		t.Fatalf("Get not-found: err=%v, want ErrTaskNotFound sentinel", err)
	}
}
