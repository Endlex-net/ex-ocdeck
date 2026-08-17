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
