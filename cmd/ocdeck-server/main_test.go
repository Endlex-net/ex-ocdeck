// main_test.go 验证 P1.6.5 bus wiring 集成（P1.6.6 生产侧）：真实 *eventbus.Bus 经
// eventSubscriberAdapter 同时接入生产侧与消费侧——LifecycleService{Publish: bus} 的
// commit helper 发布的事件被 Subscribe(TopicTask) 的订阅者按序收到。
package main

import (
	"context"
	"testing"
	"time"

	"ocdeck/internal/application"
	apptask "ocdeck/internal/application/task"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/eventbus"
	"ocdeck/internal/infrastructure/store"
)

// stubTaskRepo 嵌入 application.TaskRepository，仅覆盖本测试触达的
// CreateTask/UpdateTaskStatus（其余方法测试不调用）。
type stubTaskRepo struct {
	application.TaskRepository
	transitionRes application.TransitionResult
}

func (r *stubTaskRepo) CreateTask(context.Context, application.TaskSnapshot) error { return nil }

func (r *stubTaskRepo) UpdateTaskStatus(context.Context, string, ocdecktask.Status, *string) (application.TransitionResult, error) {
	return r.transitionRes, nil
}

// stubReadRepo 嵌入 application.TaskReadRepository（本测试不触达读侧方法）。
type stubReadRepo struct {
	application.TaskReadRepository
}

func TestP165_BusWiring_LifecyclePublishesToSubscribers(t *testing.T) {
	repo := &stubTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true},
		StatusChanged:  true,
		From:           ocdecktask.StatusSuspended,
		To:             ocdecktask.StatusActivating,
	}}
	bus := eventbus.New()
	svc := apptask.New(apptask.Options{Tasks: repo, Read: &stubReadRepo{}, Publish: bus})
	sub := eventSubscriberAdapter{bus}.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	ctx := context.Background()
	if err := svc.CreateTask(ctx, application.TaskSnapshot{ID: "t1", Status: "creating"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateStatus(ctx, "t1", ocdecktask.StatusActivating, nil); err != nil {
		t.Fatal(err)
	}

	recv := func(want ocdeckevent.Type) ocdeckevent.Event {
		t.Helper()
		select {
		case ev := <-sub.C():
			if ev.Type != want {
				t.Fatalf("event type = %s, want %s", ev.Type, want)
			}
			return ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
			return ocdeckevent.Event{}
		}
	}
	if ev := recv(ocdeckevent.TypeTaskCreated); ev.RID != "t1" {
		t.Fatalf("task.created rid = %q, want t1", ev.RID)
	}
	ev := recv(ocdeckevent.TypeTaskStatusChanged)
	if ev.RID != "t1" {
		t.Fatalf("task.status_changed rid = %q, want t1", ev.RID)
	}
	if p, ok := ev.Payload.(ocdeckevent.TaskStatusChangedPayload); !ok ||
		p.From != string(ocdecktask.StatusSuspended) || p.To != string(ocdecktask.StatusActivating) {
		t.Fatalf("task.status_changed payload = %+v, want suspended→activating", ev.Payload)
	}

	// 同值 no-op（StatusChanged=false 且 Changed=false）MUST NOT 发布。
	repo.transitionRes = application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}
	if _, err := svc.UpdateStatus(ctx, "t1", ocdecktask.StatusActivating, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("same-value write should not publish, got %s", ev.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestDiffReviewStartupFailClosed 验证生产接线的启动收敛 fail-closed 语义（F1/F12）：
// 构造 composition-root 等价的 diffreview service（SQLite adapter），ConvergeOnStartup
// 在正常 DB 上成功（sending→delivery_unknown）；断言启动序列在收敛失败时 MUST 不开放 API/调度器
// （run() 中 Reconcile 前调用 ConvergeDiffReviewOnStartup，返回 error 即拒绝启动）。
func TestDiffReviewStartupFailClosed(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// composition-root 等价：构造 store adapter（与 main.go 同源）。
	repo := store.NewDiffReviewRepoAdapter(db.Queries)

	// 无 sending 行 → 收敛 0 行成功（正常启动路径）。
	n, err := repo.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge on clean db should succeed, got %v", err)
	}
	if n != 0 {
		t.Errorf("clean db converge affected = %d, want 0", n)
	}

	// 构造一个 sending 行，验证收敛成功（sending→delivery_unknown）。
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := seedSendingSubmission(ctx, db, "s1", "t1"); err != nil {
		t.Fatalf("seed sending submission: %v", err)
	}
	n, err = repo.ConvergeDiffReviewOnStartup(ctx)
	if err != nil {
		t.Fatalf("converge with sending row should succeed, got %v", err)
	}
	if n != 1 {
		t.Errorf("converge affected = %d, want 1", n)
	}
}

// TestDiffReviewStartupFailClosed_OnWriteFailure 验证 F12①：收敛写库失败时启动编排 MUST fail-closed
// （不开放 API/调度器）。经生产 gate 函数 diffReviewStartupGate（与 main.go run() 共用同一函数）
// 断言编排层在收敛失败时不调用 openAPI（而非仅断言 converge 返回 error）。
func TestDiffReviewStartupFailClosed_OnWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// 先种 FK 父行 + 一条 sending 行，再关闭 DB 使收敛写失败。
	if err := seedTaskForSubmissions(ctx, db); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := seedSendingSubmission(ctx, db, "s1", "t1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close() // 关闭 DB，后续 ConvergeOnStartup 写库失败。

	repo := store.NewDiffReviewRepoAdapter(db.Queries)
	apiOpened := false
	openAPI := func() error { apiOpened = true; return nil }
	gateErr := diffReviewStartupGate(ctx, repo, openAPI)
	if gateErr == nil {
		t.Fatal("startup gate should return error on converge write failure (fail-closed)")
	}
	if apiOpened {
		t.Fatal("fail-closed violated: openAPI called despite converge write failure")
	}
}

// TestDiffReviewStartupFailClosed_OnSuccessOpensAPI 验证 F12①：收敛成功时启动编排开放 API。
func TestDiffReviewStartupFailClosed_OnSuccessOpensAPI(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	repo := store.NewDiffReviewRepoAdapter(db.Queries)
	apiOpened := false
	openAPI := func() error { apiOpened = true; return nil }
	if err := diffReviewStartupGate(ctx, repo, openAPI); err != nil {
		t.Fatalf("startup gate on clean db should succeed, got %v", err)
	}
	if !apiOpened {
		t.Fatal("converge success should open API")
	}
}

// seedTaskForSubmissions 种 diff_review_submissions 的 FK 父行（project + task；
// 与 store 包测试同款最小行，满足 submissions.task_id → tasks.id → projects.id）。
func seedTaskForSubmissions(ctx context.Context, db *store.DB) error {
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		return err
	}
	return db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	})
}

// seedSendingSubmission 插入一条 sending 状态的 diff_review_submission 行（供启动收敛测试）。
func seedSendingSubmission(ctx context.Context, db *store.DB, id, taskID string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO diff_review_submissions
		   (id, task_id, status, target_session_id, message_id, note, payload, truncated, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, "sending", "sess1", "msg_seed", "", "", 0, "", 1)
	return err
}
