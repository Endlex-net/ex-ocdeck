package api

import (
	"testing"

	ocdeckevent "ocdeck/internal/domain/event"

	"ocdeck/internal/application"
)

// TestEventDirtiesActiveSessions 按消费过滤表逐行表驱动（sse-active-sessions P2.1；
// design.md 消费过滤表）。事件经 typed 构造器组装，断言过滤函数从 typed payload
// 读取 From/To。
func TestEventDirtiesActiveSessions(t *testing.T) {
	cases := []struct {
		name string
		ev   ocdeckevent.Event
		want bool
	}{
		// session.* 全部标脏。
		{"session.claimed", ocdeckevent.NewSessionClaimed("s1", "t1"), true},
		{"session.touched", ocdeckevent.NewSessionTouched("s1", "t1"), true},
		{"session.deleted", ocdeckevent.NewSessionDeleted("s1", "t1"), true},
		{"sessions.aligned", ocdeckevent.NewSessionsAligned("t1", 1, 2, 0, []string{"s1"}), true},
		// serve_runtime.* 标脏。
		{"serve_runtime.attention_changed", ocdeckevent.NewServeRuntimeAttentionChanged("iv1", "t1"), true},
		{"serve_runtime.run_status_changed", ocdeckevent.NewServeRuntimeRunStatusChanged("iv1", "t1", "idle", "busy", true), true},
		// serve_runtime.session_error：一次性通知输入，快照（attention/run_status 投影）
		// 不受影响 → 不标脏（task-notifications D2）；payload 非 typed 按本表惯例保守标脏。
		{"serve_runtime.session_error", ocdeckevent.NewServeRuntimeSessionError("iv1", "t1", "s1", "APIError", "boom", nil, nil), false},
		{"serve_runtime.session_error malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicServeRuntime, Type: ocdeckevent.TypeServeRuntimeSessionError, RID: "iv1", Payload: "x"}, true},
		// resync.requested 标脏（强制重拉全量）。
		{"resync.requested", ocdeckevent.NewResyncRequested(), true},
		// task.activity_changed 标脏。
		{"task.activity_changed", ocdeckevent.NewTaskActivityChanged("t1"), true},
		// task.created 不标脏（只改 projects 树）。
		{"task.created", ocdeckevent.NewTaskCreated("t1"), false},
		// task.status_changed：仅进出 active 集合标脏。
		{"status active→suspending（离开 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActive, application.StatusSuspending), true},
		{"status suspended→active（进入 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspended, application.StatusActive), true},
		{"status activating→active（进入 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActivating, application.StatusActive), true},
		{"status active→active（同值不迁移，防御行）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActive, application.StatusActive), false},
		{"status suspended→activating（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspended, application.StatusActivating), false},
		{"status activating→suspended（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusActivating, application.StatusSuspended), false},
		{"status suspending→suspended（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusSuspending, application.StatusSuspended), false},
		{"status creating→activating（CommitCreated，两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusCreating, application.StatusActivating), false},
		{"status archived→deleting（两端非 active）", ocdeckevent.NewTaskStatusChanged("t1", application.StatusArchived, application.StatusDeleting), false},
		// task.deleted：仅 from==active 标脏。
		{"deleted from=active", ocdeckevent.NewTaskDeleted("t1", application.StatusActive), true},
		{"deleted from=suspended", ocdeckevent.NewTaskDeleted("t1", application.StatusSuspended), false},
		{"deleted from=creating", ocdeckevent.NewTaskDeleted("t1", application.StatusCreating), false},
		{"deleted from=deletion_failed", ocdeckevent.NewTaskDeleted("t1", application.StatusDeletionFailed), false},
		// 未知 Type 保守标脏。
		{"unknown type task.*", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: "task.unknown_future", RID: "t1", Payload: struct{}{}}, true},
		{"unknown type 其他域", ocdeckevent.Event{Topic: "future.topic", Type: "future.thing", RID: "x", Payload: struct{}{}}, true},
		{"empty type", ocdeckevent.Event{}, true},
		// 已知 Type 但 payload 非 typed（bus 不做 schema 校验）→ 保守标脏，
		// 避免零值 From/To 判非 active 漏推。
		{"status_changed malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskStatusChanged, RID: "t1", Payload: map[string]any{"from": "active"}}, true},
		{"deleted malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskDeleted, RID: "t1", Payload: nil}, true},
		// typed payload 但 From/To 语义畸形（空串/未知 status）→ 保守标脏：
		// 按零值判非 active 会把畸形迁移漏成"无变化"。
		{"status_changed empty from", typedStatusEvent("", application.StatusActive), true},
		{"status_changed empty to", typedStatusEvent(application.StatusActive, ""), true},
		{"status_changed empty both", typedStatusEvent("", ""), true},
		{"status_changed invalid from", typedStatusEvent("ACTIVE", application.StatusSuspending), true},
		{"status_changed invalid to", typedStatusEvent(application.StatusSuspended, "resumed"), true},
		{"status_changed invalid both", typedStatusEvent("from-x", "to-y"), true},
		{"deleted empty from", typedDeletedEvent(""), true},
		{"deleted invalid from", typedDeletedEvent("running"), true},
	}
	for _, c := range cases {
		if got := eventDirtiesActiveSessions(c.ev); got != c.want {
			t.Errorf("%s: eventDirtiesActiveSessions = %v, want %v", c.name, got, c.want)
		}
	}
}

// typedStatusEvent 组装携带任意 From/To 字符串的 typed status_changed payload 事件
// （绕过构造器的语义畸形注入；payload 类型仍为 typed struct）。
func typedStatusEvent(from, to string) ocdeckevent.Event {
	return ocdeckevent.Event{
		Topic:   ocdeckevent.TopicTask,
		Type:    ocdeckevent.TypeTaskStatusChanged,
		RID:     "t1",
		Payload: ocdeckevent.TaskStatusChangedPayload{From: from, To: to},
	}
}

// typedDeletedEvent 组装携带任意 From 字符串的 typed deleted payload 事件。
func typedDeletedEvent(from string) ocdeckevent.Event {
	return ocdeckevent.Event{
		Topic:   ocdeckevent.TopicTask,
		Type:    ocdeckevent.TypeTaskDeleted,
		RID:     "t1",
		Payload: ocdeckevent.TaskDeletedPayload{From: from},
	}
}
