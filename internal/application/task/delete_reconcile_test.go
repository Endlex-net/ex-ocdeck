// delete_reconcile_test.go 验证 P1.4.8 Delete/Reconcile/notice-CAS/init_status
// 写入薄封装的 persist+commit 行为（Phase B strangler 最后一步，design.md D0:146-156）。
//
// 覆盖（recordingPublisher 断言 commit helper 调用位）：
//   - BeginDeleteIntent：StatusChanged=true 发布 task.status_changed（无 task.deleted）；
//   - DeleteTask：Affected=0 零发布；Affected>0 先逐个发布 session.deleted（CascadedSessionIDs
//     顺序），最后 task.deleted（P1.6.1 发布顺序契约）；
//   - UpdateNoticeCAS：仅 Changed && UpdatedAtAdvanced 发布 task.activity_changed；
//   - ConvergeInterruptedInitRuns/ClaimInitRun/FinishInitRun：init_status 写入零发布
//     （P1.6.1：init_status 写入不发事件，即使 updated_at 前进）。
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

func TestP148_UpdateNoticeCAS_ChangedPublishesActivityOnlyWhenAdvanced(t *testing.T) {
	advanced := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true}}
	pubAdvanced := &recordingPublisher{}
	if _, err := newSvc(advanced, &fakeReadRepo{}, pubAdvanced).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err != nil {
		t.Fatalf("UpdateNoticeCAS err: %v", err)
	}
	if len(pubAdvanced.events) != 1 || pubAdvanced.events[0] != string(ocdeckevent.TypeTaskActivityChanged) {
		t.Fatalf("events = %v, want [task.activity_changed]", pubAdvanced.events)
	}

	// CAS 未命中（!Changed）零发布。
	unmatched := &fakeTaskRepo{mutationRes: application.MutationResult{Matched: false}}
	pubUnmatched := &recordingPublisher{}
	if _, err := newSvc(unmatched, &fakeReadRepo{}, pubUnmatched).UpdateNoticeCAS(
		context.Background(), "t1", nil, strPtr("n1")); err != nil {
		t.Fatalf("UpdateNoticeCAS err: %v", err)
	}
	if len(pubUnmatched.events) != 0 {
		t.Fatalf("!Changed should not publish, got %v", pubUnmatched.events)
	}
}

func TestP148_InitStatusWritesNeverPublish(t *testing.T) {
	// ConvergeInterruptedInitRuns n>0 零发布。
	conv := &fakeTaskRepo{convergeN: 3}
	pubConv := &recordingPublisher{}
	if n, err := newSvc(conv, &fakeReadRepo{}, pubConv).ConvergeInterruptedInitRuns(context.Background()); err != nil || n != 3 {
		t.Fatalf("ConvergeInterruptedInitRuns = (%d, %v), want (3, nil)", n, err)
	}
	if len(pubConv.events) != 0 {
		t.Fatalf("converge should not publish, got %v", pubConv.events)
	}

	// ClaimInitRun / FinishInitRun Changed=true（含 updated_at 前进）零发布（P1.6.1）。
	changed := application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true}
	pubClaim := &recordingPublisher{}
	if _, err := newSvc(&fakeTaskRepo{mutationRes: changed}, &fakeReadRepo{}, pubClaim).ClaimInitRun(
		context.Background(), "t1"); err != nil {
		t.Fatalf("ClaimInitRun err: %v", err)
	}
	if len(pubClaim.events) != 0 {
		t.Fatalf("ClaimInitRun should not publish, got %v", pubClaim.events)
	}

	pubFinish := &recordingPublisher{}
	if _, err := newSvc(&fakeTaskRepo{mutationRes: changed}, &fakeReadRepo{}, pubFinish).FinishInitRun(
		context.Background(), "t1", ocdecktask.InitStatusFailed, strPtr("boom")); err != nil {
		t.Fatalf("FinishInitRun err: %v", err)
	}
	if len(pubFinish.events) != 0 {
		t.Fatalf("FinishInitRun should not publish, got %v", pubFinish.events)
	}
}
