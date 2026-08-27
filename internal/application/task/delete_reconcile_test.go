// delete_reconcile_test.go 验证 P1.4.8 Delete/Reconcile/notice-CAS/init_status
// 写入薄封装的 persist+commit 行为（Phase B strangler 最后一步，design.md D0:146-156）。
//
// 覆盖（recordingPublisher 断言 commit helper 调用位）：
//   - BeginDeleteIntent：StatusChanged=true 发布 task.status_changed（无 task.deleted）；
//   - DeleteTask：Affected=0 零发布；Affected>0 先逐个发布 session.deleted（CascadedSessionIDs
//     顺序），最后 task.deleted（P1.6.1 发布顺序契约）；
//   - UpdateNoticeCAS：Changed=true 发布 task.activity_changed（不再要求跨秒）；
//   - ClaimInitRun/ClaimInitRerun/FinishInitRun：真实变更发布一次 activity_changed；
//     ConvergeInterruptedInitRuns 启动期例外，不发布。
package task

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
)

func TestP148_BeginDeleteIntent_StatusChangedPublishesTransition(t *testing.T) {
	repo := &fakeTaskRepo{transitionRes: application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
		StatusChanged:  true,
		From:           ocdecktask.StatusSuspended,
		To:             ocdecktask.StatusDeleting,
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.BeginDeleteIntent(context.Background(), "t1", ocdecktask.DeleteModeNormal,
		[]ocdecktask.Status{ocdecktask.StatusSuspended}); err != nil {
		t.Fatalf("BeginDeleteIntent err: %v", err)
	}
	if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskStatusChanged) {
		t.Fatalf("events = %v, want [task.status_changed] only", pub.events)
	}
	pl, ok := pub.raw[0].Payload.(ocdeckevent.TaskStatusChangedPayload)
	if !ok || pl.From != string(ocdecktask.StatusSuspended) || pl.To != string(ocdecktask.StatusDeleting) {
		t.Fatalf("payload = %#v, want from=suspended to=deleting", pub.raw[0].Payload)
	}
}

func TestP148_DeleteTask_AffectedZeroNoPublish(t *testing.T) {
	repo := &fakeTaskRepo{deleteRes: application.DeleteResult{Affected: 0}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.DeleteTask(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTask err: %v", err)
	}
	if len(pub.events) != 0 {
		t.Fatalf("Affected=0 should not publish, got %v", pub.events)
	}
}

func TestP148_DeleteTask_PublishesSessionDeletedThenTaskDeleted(t *testing.T) {
	repo := &fakeTaskRepo{deleteRes: application.DeleteResult{
		Affected:           1,
		From:               ocdecktask.StatusDeleting,
		CascadedSessionIDs: []string{"s1", "s2"},
	}}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.DeleteTask(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTask err: %v", err)
	}
	// P1.6.1 顺序契约：先 cascade session.deleted（按 CascadedSessionIDs 顺序），再 task.deleted。
	want := []string{
		string(ocdeckevent.TypeSessionDeleted),
		string(ocdeckevent.TypeSessionDeleted),
		string(ocdeckevent.TypeTaskDeleted),
	}
	if len(pub.events) != len(want) {
		t.Fatalf("events = %v, want %v", pub.events, want)
	}
	for i, ev := range want {
		if pub.events[i] != ev {
			t.Fatalf("events = %v, want %v", pub.events, want)
		}
	}
	for i, sid := range []string{"s1", "s2"} {
		if pub.raw[i].RID != sid {
			t.Fatalf("session.deleted[%d] RID = %q, want %q", i, pub.raw[i].RID, sid)
		}
		spl, ok := pub.raw[i].Payload.(ocdeckevent.SessionOwnerPayload)
		if !ok || spl.TaskID != "t1" {
			t.Fatalf("session.deleted[%d] payload = %#v, want TaskID=t1", i, pub.raw[i].Payload)
		}
	}
	if pub.raw[2].RID != "t1" {
		t.Fatalf("task.deleted RID = %q, want t1", pub.raw[2].RID)
	}
	dpl, ok := pub.raw[2].Payload.(ocdeckevent.TaskDeletedPayload)
	if !ok || dpl.From != string(ocdecktask.StatusDeleting) {
		t.Fatalf("task.deleted payload = %#v, want from=deleting", pub.raw[2].Payload)
	}
}

func TestP148_DeleteTask_ErrorNoPublish(t *testing.T) {
	repo := &fakeTaskRepo{deleteErr: errors.New("db error")}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.DeleteTask(context.Background(), "t1"); err == nil {
		t.Fatal("DeleteTask: want err, got nil")
	}
	if len(pub.events) != 0 {
		t.Fatalf("error path should not publish, got %v", pub.events)
	}
}

func TestP148_UpdateNoticeCAS_ChangedPublishesActivity(t *testing.T) {
	sameSecond := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: true, Changed: true}}
	pubSameSecond := &recordingPublisher{}
	if _, err := newSvc(sameSecond, &fakeReadRepo{}, pubSameSecond).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err != nil {
		t.Fatalf("UpdateNoticeCAS err: %v", err)
	}
	if len(pubSameSecond.events) != 1 || pubSameSecond.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
		t.Fatalf("same-second events = %v, want [task.activity_changed]", pubSameSecond.events)
	}

	advanced := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true}}
	pubAdvanced := &recordingPublisher{}
	if _, err := newSvc(advanced, &fakeReadRepo{}, pubAdvanced).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err != nil {
		t.Fatalf("UpdateNoticeCAS err: %v", err)
	}
	if len(pubAdvanced.events) != 1 || pubAdvanced.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
		t.Fatalf("events = %v, want [task.activity_changed]", pubAdvanced.events)
	}

	unmatched := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: false}}
	pubUnmatched := &recordingPublisher{}
	if _, err := newSvc(unmatched, &fakeReadRepo{}, pubUnmatched).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err != nil {
		t.Fatalf("UpdateNoticeCAS err: %v", err)
	}
	if len(pubUnmatched.events) != 0 {
		t.Fatalf("!Changed should not publish, got %v", pubUnmatched.events)
	}

	pubErr := &recordingPublisher{}
	if _, err := newSvc(&fakeTaskRepo{mutationErr: errors.New("db error")}, &fakeReadRepo{}, pubErr).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err == nil {
		t.Fatal("want err, got nil")
	}
	if len(pubErr.events) != 0 {
		t.Fatalf("error path should not publish, got %v", pubErr.events)
	}
}

func TestP148_ConvergeInterruptedInitRuns_NeverPublishes(t *testing.T) {
	conv := &fakeTaskRepo{convergeN: 3}
	pubConv := &recordingPublisher{}
	if n, err := newSvc(conv, &fakeReadRepo{}, pubConv).ConvergeInterruptedInitRuns(context.Background()); err != nil || n != 3 {
		t.Fatalf("ConvergeInterruptedInitRuns = (%d, %v), want (3, nil)", n, err)
	}
	if len(pubConv.events) != 0 {
		t.Fatalf("converge should not publish, got %v", pubConv.events)
	}
}

func TestP148_InitStatusWritesPublishActivityOnChange(t *testing.T) {
	sameSecond := application.MutationResult{Matched: true, Changed: true}
	unchanged := application.MutationResult{Matched: true, Changed: false}

	calls := []struct {
		name string
		fn   func(*LifecycleService) error
	}{
		{"ClaimInitRun", func(s *LifecycleService) error {
			_, err := s.ClaimInitRun(context.Background(), "t1")
			return err
		}},
		{"ClaimInitRerun", func(s *LifecycleService) error {
			_, err := s.ClaimInitRerun(context.Background(), "t1")
			return err
		}},
		{"FinishInitRun", func(s *LifecycleService) error {
			_, err := s.FinishInitRun(context.Background(), "t1", ocdecktask.InitStatusFailed, strPtr("boom"))
			return err
		}},
	}
	for _, c := range calls {
		pub := &recordingPublisher{}
		if err := c.fn(newSvc(&fakeTaskRepo{mutationRes: sameSecond}, &fakeReadRepo{}, pub)); err != nil {
			t.Fatalf("%s err: %v", c.name, err)
		}
		if len(pub.events) != 1 || pub.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
			t.Fatalf("%s events = %v, want [task.activity_changed]", c.name, pub.events)
		}

		pubNoop := &recordingPublisher{}
		if err := c.fn(newSvc(&fakeTaskRepo{mutationRes: unchanged}, &fakeReadRepo{}, pubNoop)); err != nil {
			t.Fatalf("%s no-op err: %v", c.name, err)
		}
		if len(pubNoop.events) != 0 {
			t.Fatalf("%s no-op should not publish, got %v", c.name, pubNoop.events)
		}

		pubErr := &recordingPublisher{}
		if err := c.fn(newSvc(&fakeTaskRepo{mutationErr: errors.New("db error")}, &fakeReadRepo{}, pubErr)); err == nil {
			t.Fatalf("%s: want err, got nil", c.name)
		}
		if len(pubErr.events) != 0 {
			t.Fatalf("%s error path should not publish, got %v", c.name, pubErr.events)
		}
	}
}
