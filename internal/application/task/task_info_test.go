// task_info_test.go 验证 LifecycleService 任务信息修改端口的事件链接线
// （task-info-editable tasks 1.4）。
//
// 覆盖 D6 四态表在 application 层的接线语义：
//   - 业务提交经 commitTaskMutation：仅 Changed=true（业务列真实变化）时发布一次
//     task.activity_changed；同值 no-op（!Changed）不发布；repo 错误透传不发布；
//   - pending-only 操作（仅设置/仅清除意图）：不发事件，结果透传；
//   - GetTaskRenamePending 透传（内部收敛编排消费，不出 DTO）。
//
// updated_at 语义由 store 层单测覆盖（task_info_queries_test.go，真实 DB + nowUnix 注入）。
// 使用 fakeTaskRepo（扩展于 lifecycle_test.go 的既有 fake，方法补于本文件）。
package task

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
)

// fakeTaskRepo 任务信息修改端口扩展（task-info-editable Phase 1）：
// infoRes/infoErr 控制 CommitTaskInfoUpdate；pendingRes/pendingErr 控制
// SetTaskRenamePending/ClearTaskRenamePending；pendingValue/getPendingErr 控制
// GetTaskRenamePending。commitUpdates 记录 CommitTaskInfoUpdate 入参。
func (r *fakeTaskRepo) CommitTaskInfoUpdate(_ context.Context, _ string, update application.TaskInfoUpdate) (application.MutationResult, error) {
	r.commitUpdates = append(r.commitUpdates, update)
	return r.infoRes, r.infoErr
}

func (r *fakeTaskRepo) SetTaskRenamePending(context.Context, string, string) (application.MutationResult, error) {
	return r.pendingRes, r.pendingErr
}

func (r *fakeTaskRepo) ClearTaskRenamePending(context.Context, string) (application.MutationResult, error) {
	return r.pendingRes, r.pendingErr
}

func (r *fakeTaskRepo) GetTaskRenamePending(context.Context, string) (*string, error) {
	return r.pendingValue, r.getPendingErr
}

// TestCommitTaskInfoUpdate_EventWiring 验证业务提交事件链接线：仅 Changed=true 发布一次
// task.activity_changed；同值 no-op 与 repo 错误均不发布。
func TestCommitTaskInfoUpdate_EventWiring(t *testing.T) {
	name := "n2"
	cases := []struct {
		name       string
		infoRes    application.MutationResult
		infoErr    error
		wantEvents int
		wantErr    bool
	}{
		{
			name:       "business change publishes once",
			infoRes:    application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: true},
			wantEvents: 1,
		},
		{
			name:       "same value no-op publishes none",
			infoRes:    application.MutationResult{Matched: true, Changed: false},
			wantEvents: 0,
		},
		{
			name:       "repo error propagates and publishes none",
			infoErr:    errors.New("db down"),
			wantEvents: 0,
			wantErr:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeTaskRepo{infoRes: c.infoRes, infoErr: c.infoErr}
			pub := &recordingPublisher{}
			svc := newSvc(repo, &fakeReadRepo{}, pub)

			res, err := svc.CommitTaskInfoUpdate(context.Background(), "t1", application.TaskInfoUpdate{Name: &name})
			if c.wantErr {
				if err == nil {
					t.Fatalf("CommitTaskInfoUpdate: want err, got nil")
				}
				if err.Error() != "db down" {
					t.Fatalf("err = %v, want db down (err-first 透传)", err)
				}
			} else if err != nil {
				t.Fatalf("CommitTaskInfoUpdate err: %v", err)
			}
			if res != c.infoRes {
				t.Fatalf("res = %+v, want %+v (结构化结果透传)", res, c.infoRes)
			}
			if len(pub.events) != c.wantEvents {
				t.Fatalf("publish events = %v, want %d event(s)", pub.events, c.wantEvents)
			}
			for _, ev := range pub.raw {
				if ev.Type != ocdeckevent.TypeTaskActivityChanged {
					t.Fatalf("published event type = %q, want %q", ev.Type, ocdeckevent.TypeTaskActivityChanged)
				}
			}
			// 入参原样透传（presence 语义由调用方构造，service 不改写）。
			if len(repo.commitUpdates) != 1 || repo.commitUpdates[0].Name != &name || repo.commitUpdates[0].Branch != nil {
				t.Fatalf("commitUpdates = %+v, want 原样透传的 TaskInfoUpdate", repo.commitUpdates)
			}
		})
	}
}

// TestPendingOnly_NoEvents 验证 pending-only 操作不发事件（design D6 四态表第 1/3 行）。
func TestPendingOnly_NoEvents(t *testing.T) {
	cases := []struct {
		name string
		run  func(svc *LifecycleService) error
	}{
		{
			name: "SetTaskRenamePending (仅设置 pending)",
			run: func(svc *LifecycleService) error {
				_, err := svc.SetTaskRenamePending(context.Background(), "t1", `{"name":"n2"}`)
				return err
			},
		},
		{
			name: "ClearTaskRenamePending (仅清除 pending)",
			run: func(svc *LifecycleService) error {
				_, err := svc.ClearTaskRenamePending(context.Background(), "t1")
				return err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeTaskRepo{pendingRes: application.MutationResult{Matched: true}}
			pub := &recordingPublisher{}
			svc := newSvc(repo, &fakeReadRepo{}, pub)

			if err := c.run(svc); err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(pub.events) != 0 {
				t.Fatalf("pending-only 操作不应发布事件，got %v", pub.events)
			}
		})
	}
}

// TestPendingOnly_ErrorPropagates 验证 pending-only 操作 repo 错误 err-first 透传且不发布。
func TestPendingOnly_ErrorPropagates(t *testing.T) {
	repo := &fakeTaskRepo{pendingErr: errors.New("db down")}
	pub := &recordingPublisher{}
	svc := newSvc(repo, &fakeReadRepo{}, pub)

	if _, err := svc.SetTaskRenamePending(context.Background(), "t1", "{}"); err == nil {
		t.Fatal("SetTaskRenamePending: want err, got nil")
	}
	if _, err := svc.ClearTaskRenamePending(context.Background(), "t1"); err == nil {
		t.Fatal("ClearTaskRenamePending: want err, got nil")
	}
	if len(pub.events) != 0 {
		t.Fatalf("错误路径不应发布事件，got %v", pub.events)
	}
}

// TestGetTaskRenamePending_Passthrough 验证意图读取透传（值/nil/错误）。
func TestGetTaskRenamePending_Passthrough(t *testing.T) {
	pending := `{"name":"n2"}`
	cases := []struct {
		name         string
		pendingValue *string
		getErr       error
		want         *string
		wantErr      bool
	}{
		{name: "value passthrough", pendingValue: &pending, want: &pending},
		{name: "nil passthrough", want: nil},
		{name: "error passthrough", getErr: application.ErrTaskNotFound, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeTaskRepo{pendingValue: c.pendingValue, getPendingErr: c.getErr}
			svc := newSvc(repo, &fakeReadRepo{}, NoopPublisher{})

			got, err := svc.GetTaskRenamePending(context.Background(), "t1")
			if c.wantErr {
				if !errors.Is(err, application.ErrTaskNotFound) {
					t.Fatalf("err = %v, want ErrTaskNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("got = %v, want %v", got, c.want)
			}
		})
	}
}
