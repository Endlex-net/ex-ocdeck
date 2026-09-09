package api

import (
	"testing"

	ocdeckevent "ocdeck/internal/domain/event"

	"ocdeck/internal/application"
)

// TestEventDirtiesProjectsTaskTree 按消费过滤表逐行表驱动（projects-stream design D4）。
// projects 场景为全任务树视图：除 task.user_activity（一次性用户输入事实，
// 见 TestEventDirtiesProjectsTaskTree_UserActivityException）外，全部已知 Type
// 与未知 Type 均标脏，不解读其余 Payload。
// 与 eventDirtiesActiveSessions 的差异对照见
// TestEventDirtiesProjectsTaskTree_ContrastWithActiveSessions。
func TestEventDirtiesProjectsTaskTree(t *testing.T) {
	cases := []struct {
		name string
		ev   ocdeckevent.Event
	}{
		// task.* 全部标脏（含 active-only 过滤不标脏的差异行）。
		{"task.created", ocdeckevent.NewTaskCreated("t1")},
		{"task.activity_changed", ocdeckevent.NewTaskActivityChanged("t1")},
		{"status active→suspending（跨 active 边界）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActive, application.StatusSuspending)},
		{"status suspended→active（跨 active 边界）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspended, application.StatusActive)},
		{"status active→active（同值防御行）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActive, application.StatusActive)},
		{"status suspended→activating（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspended, application.StatusActivating)},
		{"status creating→creation_failed（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusCreating, application.StatusCreationFailed)},
		{"status suspending→suspended（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspending, application.StatusSuspended)},
		{"status archived→deleting（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusArchived, application.StatusDeleting)},
		{"deleted from=active", ocdeckevent.NewTaskDeleted("t1", application.StatusActive)},
		{"deleted from=suspended", ocdeckevent.NewTaskDeleted("t1", application.StatusSuspended)},
		{"deleted from=archived", ocdeckevent.NewTaskDeleted("t1", application.StatusArchived)},
		{"deleted from=creating", ocdeckevent.NewTaskDeleted("t1", application.StatusCreating)},
		// session.* 全部标脏。
		{"session.claimed", ocdeckevent.NewSessionClaimed("s1", "t1")},
		{"session.touched", ocdeckevent.NewSessionTouched("s1", "t1")},
		{"session.deleted", ocdeckevent.NewSessionDeleted("s1", "t1")},
		{"sessions.aligned", ocdeckevent.NewSessionsAligned("t1", 1, 2, 0, []string{"s1"})},
		// serve_runtime.* 标脏。
		{"serve_runtime.attention_changed", ocdeckevent.NewServeRuntimeAttentionChanged("iv1", "t1")},
		{"serve_runtime.run_status_changed", ocdeckevent.NewServeRuntimeRunStatusChanged("iv1", "t1", "idle", "busy", true)},
		// resync.requested 标脏（强制重拉全量）。
		{"resync.requested", ocdeckevent.NewResyncRequested()},
		// 未知 Type 保守标脏。
		{"unknown type task.*", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: "task.unknown_future", RID: "t1", Payload: struct{}{}}},
		{"unknown type 其他域", ocdeckevent.Event{Topic: "future.topic", Type: "future.thing", RID: "x", Payload: struct{}{}}},
		{"empty type", ocdeckevent.Event{}},
		// 已知 Type 但 payload 非 typed / 语义畸形：本场景不解读 Payload，一律标脏。
		{"status_changed malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskStatusChanged, RID: "t1", Payload: map[string]any{"from": "active"}}},
		{"deleted malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskDeleted, RID: "t1", Payload: nil}},
		{"status_changed empty from", typedStatusEvent("", application.StatusActive)},
		{"status_changed invalid both", typedStatusEvent("from-x", "to-y")},
		{"deleted empty from", typedDeletedEvent("")},
		{"deleted invalid from", typedDeletedEvent("running")},
	}
	for _, c := range cases {
		if got := eventDirtiesProjectsTaskTree(c.ev); !got {
			t.Errorf("%s: eventDirtiesProjectsTaskTree = false, want true (全任务树任一事件标脏)", c.name)
		}
	}
}

// TestEventDirtiesProjectsTaskTree_ContrastWithActiveSessions 差异对照断言（tasks 2.3）：
// active-only 过滤不标脏的三类行（task.created、两端均非 active 的 status_changed、
// from 非 active 的 deleted）在 projects 场景必须标脏。
func TestEventDirtiesProjectsTaskTree_ContrastWithActiveSessions(t *testing.T) {
	diffRows := []struct {
		name string
		ev   ocdeckevent.Event
	}{
		{"task.created", ocdeckevent.NewTaskCreated("t1")},
		{"status 两端均非 active", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspended, application.StatusArchived)},
		{"deleted from 非 active", ocdeckevent.NewTaskDeleted("t1", application.StatusSuspended)},
	}
	for _, c := range diffRows {
		if eventDirtiesActiveSessions(c.ev) {
			t.Errorf("%s: eventDirtiesActiveSessions = true, want false（对照前提失效）", c.name)
		}
		if !eventDirtiesProjectsTaskTree(c.ev) {
			t.Errorf("%s: eventDirtiesProjectsTaskTree = false, want true", c.name)
		}
	}
}

// TestEventDirtiesProjectsTaskTree_UserActivityException 用户活动例外行
//（idle-reminder-user-activity D1；projects-stream spec「用户活动事件不触发更新」）：
// 合法 task.user_activity（Topic task 且 Payload struct{}{}）不标脏，畸形标脏，
// 其余事件维持标脏。
func TestEventDirtiesProjectsTaskTree_UserActivityException(t *testing.T) {
	cases := []struct {
		name string
		ev   ocdeckevent.Event
		want bool
	}{
		{"legal self RID", ocdeckevent.NewTaskUserActivity("t1"), false},
		{"legal other RID", ocdeckevent.NewTaskUserActivity("t2"), false},
		{"malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskUserActivity, RID: "t1", Payload: nil}, true},
		{"wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeTaskUserActivity, RID: "t1", Payload: struct{}{}}, true},
		{"unknown type still dirty", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: "task.unknown_future", RID: "t1", Payload: struct{}{}}, true},
		{"task.activity_changed still dirty", ocdeckevent.NewTaskActivityChanged("t1"), true},
	}
	for _, c := range cases {
		if got := eventDirtiesProjectsTaskTree(c.ev); got != c.want {
			t.Errorf("%s: eventDirtiesProjectsTaskTree = %v, want %v", c.name, got, c.want)
		}
	}
}
