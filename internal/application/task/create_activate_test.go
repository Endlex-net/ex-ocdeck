// create_activate_test.go 验证 P1.4.6 Create/Retry/Activate 写入薄封装的
// persist+commit 行为（design.md D0:142 迁移第 6 步）。
//
// 覆盖（recordingPublisher 断言 commit helper 调用位）：
//   - CreateTask 成功发布一次 task.created；失败零发布；
//   - UpdateStatus/Conditional/CommitCreated：StatusChanged=true 发布 task.status_changed
//     （含 from/to）；同值 status + last_error 变更（Changed+UpdatedAtAdvanced）仅发布
//     task.activity_changed；Changed 但 updated_at 未跨秒推进零发布；!Matched 零发布；
//     error 零发布；
//   - SetDeleteMode/UpdateEnvSnapshot/UpdateLastPort：仅 Changed && UpdatedAtAdvanced
//     发布 task.activity_changed。
package task

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
)

func TestP146_CreateTask_Commits(t *testing.T) {
	repo := &fakeTaskRepo{}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	row := application.TaskSnapshot{ID: "t1", ProjectID: "p1", Name: "n", Status: string(ocdecktask.StatusCreating)}
	if err := svc.CreateTask(context.Background(), row); err != nil {
		t.Fatalf("CreateTask err: %v", err)
	}
	if len(repo.createdRows) != 1 || repo.createdRows[0].ID != "t1" {
		t.Fatalf("createdRows = %v, want single t1", repo.createdRows)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskCreated) {
		t.Fatalf("events = %v, want [task.created]", pub.events)
	}
}

func TestP146_CreateTask_ErrorNoPublish(t *testing.T) {
	repo := &fakeTaskRepo{createErr: errors.New("db error")}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if err := svc.CreateTask(context.Background(), application.TaskSnapshot{ID: "t1"}); err == nil {
		t.Fatal("CreateTask: want err, got nil")
	}
	if len(pub.events) != 0 {
		t.Fatalf("error path should not publish, got %v", pub.events)
	}
}

func TestP146_UpdateStatus_StatusChangedPublishesTransition(t *testing.T) {
	repo := &fakeTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
		StatusChanged:  true,
		From:           ocdecktask.StatusCreating,
		To:             ocdecktask.StatusCreationFailed,
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	le := "worktree add: boom"
	if _, err := svc.UpdateStatus(context.Background(), "t1", ocdecktask.StatusCreationFailed, &le); err != nil {
		t.Fatalf("UpdateStatus err: %v", err)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskStatusChanged) {
		t.Fatalf("events = %v, want [task.status_changed] only", pub.events)
	}
	pl, ok := pub.raw[0].Payload.(ocdeckevent.TaskStatusChangedPayload)
	if !ok || pl.From != string(ocdecktask.StatusCreating) || pl.To != string(ocdecktask.StatusCreationFailed) {
		t.Fatalf("payload = %#v, want from=creating to=creation_failed", pub.raw[0].Payload)
	}
}

func TestP146_UpdateStatus_SameStatusLastErrorChangePublishesActivity(t *testing.T) {
	// 同值 status + last_error 变更：StatusChanged=false、Changed=true、UpdatedAtAdvanced=true
	// → 仅发布 task.activity_changed。
	repo := &fakeTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	le := "new error"
	if _, err := svc.UpdateStatus(context.Background(), "t1", ocdecktask.StatusSuspended, &le); err != nil {
		t.Fatalf("UpdateStatus err: %v", err)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
		t.Fatalf("events = %v, want [task.activity_changed] only", pub.events)
	}
}

func TestP146_UpdateStatus_ChangedWithoutUpdatedAtAdvanceNoPublish(t *testing.T) {
	repo := &fakeTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true},
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	le := "same-second error"
	if _, err := svc.UpdateStatus(context.Background(), "t1", ocdecktask.StatusSuspended, &le); err != nil {
		t.Fatalf("UpdateStatus err: %v", err)
	}
	if len(pub.events) != 0 {
		t.Fatalf("same-second change should not publish, got %v", pub.events)
	}
}

func TestP146_UpdateStatus_ErrorNoPublish(t *testing.T) {
	repo := &fakeTaskRepo{transitionErr: errors.New("db error")}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.UpdateStatus(context.Background(), "t1", ocdecktask.StatusActive, nil); err == nil {
		t.Fatal("UpdateStatus: want err, got nil")
	}
	if len(pub.events) != 0 {
		t.Fatalf("error path should not publish, got %v", pub.events)
	}
}

func TestP146_CommitCreated_StatusChangedPublishesTransition(t *testing.T) {
	repo := &fakeTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
		StatusChanged:  true,
		From:           ocdecktask.StatusCreating,
		To:             ocdecktask.StatusSuspended,
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.CommitCreated(context.Background(), "t1", ocdecktask.StatusCreating, ocdecktask.InitStatusNone); err != nil {
		t.Fatalf("CommitCreated err: %v", err)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskStatusChanged) {
		t.Fatalf("events = %v, want [task.status_changed]", pub.events)
	}
}

func TestP146_UpdateStatusConditional_NotMatchedNoPublish(t *testing.T) {
	// BeginActivate CAS suspended→activating 失配（!Matched）：零发布。
	repo := &fakeTaskRepo{}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	res, err := svc.UpdateStatusConditional(context.Background(), "t1", ocdecktask.StatusSuspended, ocdecktask.StatusActivating, nil)
	if err != nil {
		t.Fatalf("UpdateStatusConditional err: %v", err)
	}
	if res.Matched {
		t.Fatalf("res = %+v, want !Matched", res)
	}
	if len(pub.events) != 0 {
		t.Fatalf("not-matched CAS should not publish, got %v", pub.events)
	}
}

func TestP146_Mutations_PublishActivityOnlyWhenAdvanced(t *testing.T) {
	advanced := application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true}
	sameSecond := application.MutationResult{Matched: true, Changed: true}

	t.Run("advanced publishes activity", func(t *testing.T) {
		calls := []func(s *LifecycleService) error{
			func(s *LifecycleService) error {
				_, err := s.SetDeleteMode(context.Background(), "t1", ocdecktask.DeleteModeForce)
				return err
			},
			func(s *LifecycleService) error {
				env := "snap"
				_, err := s.UpdateEnvSnapshot(context.Background(), "t1", &env)
				return err
			},
			func(s *LifecycleService) error {
				_, err := s.UpdateLastPort(context.Background(), "t1", 50001)
				return err
			},
		}
		for i, call := range calls {
			pub := &recordingPublisher{}
			svc := newSvc(&fakeTaskRepo{mutationRes: advanced}, &fakeReadRepo{}, pub)
			if err := call(svc); err != nil {
				t.Fatalf("call %d err: %v", i, err)
			}
			if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
				t.Fatalf("call %d events = %v, want [task.activity_changed]", i, pub.events)
			}
		}
	})
	t.Run("same-second change not publishes", func(t *testing.T) {
		pub := &recordingPublisher{}
		svc := newSvc(&fakeTaskRepo{mutationRes: sameSecond}, &fakeReadRepo{}, pub)
		if _, err := svc.SetDeleteMode(context.Background(), "t1", ocdecktask.DeleteModeForce); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.UpdateEnvSnapshot(context.Background(), "t1", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.UpdateLastPort(context.Background(), "t1", 50001); err != nil {
			t.Fatal(err)
		}
		if len(pub.events) != 0 {
			t.Fatalf("same-second mutations should not publish, got %v", pub.events)
		}
	})
	t.Run("error not publishes", func(t *testing.T) {
		pub := &recordingPublisher{}
		svc := newSvc(&fakeTaskRepo{mutationErr: errors.New("db error")}, &fakeReadRepo{}, pub)
		if _, err := svc.UpdateLastPort(context.Background(), "t1", 50001); err == nil {
			t.Fatal("want err, got nil")
		}
		if len(pub.events) != 0 {
			t.Fatalf("error path should not publish, got %v", pub.events)
		}
	})
}
