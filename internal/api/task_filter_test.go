package api

import (
	"testing"

	ocdeckevent "ocdeck/internal/domain/event"
)

func TestEventDirtiesTaskDetail(t *testing.T) {
	dirty := eventDirtiesTaskDetail("t1")
	cases := []struct {
		name string
		ev   ocdeckevent.Event
		want bool
	}{
		{"task.created self", ocdeckevent.NewTaskCreated("t1"), true},
		{"task.created other", ocdeckevent.NewTaskCreated("t2"), false},
		{"task.status_changed self", ocdeckevent.NewTaskStatusChanged("t1", "active", "suspended"), true},
		{"task.status_changed other", ocdeckevent.NewTaskStatusChanged("t2", "active", "suspended"), false},
		{"task.deleted self", ocdeckevent.NewTaskDeleted("t1", "active"), true},
		{"task.deleted other", ocdeckevent.NewTaskDeleted("t2", "active"), false},
		{"task.activity_changed self", ocdeckevent.NewTaskActivityChanged("t1"), true},
		{"task.activity_changed other", ocdeckevent.NewTaskActivityChanged("t2"), false},
		{"sessions.aligned self", ocdeckevent.NewSessionsAligned("t1", 1, 0, 0, []string{"s1"}), true},
		{"sessions.aligned other", ocdeckevent.NewSessionsAligned("t2", 1, 0, 0, []string{"s1"}), false},
		{"session.claimed self", ocdeckevent.NewSessionClaimed("s1", "t1"), true},
		{"session.claimed other", ocdeckevent.NewSessionClaimed("s1", "t2"), false},
		{"session.touched self", ocdeckevent.NewSessionTouched("s1", "t1"), true},
		{"session.touched other", ocdeckevent.NewSessionTouched("s1", "t2"), false},
		{"session.deleted self", ocdeckevent.NewSessionDeleted("s1", "t1"), true},
		{"session.deleted other", ocdeckevent.NewSessionDeleted("s1", "t2"), false},
		{"serve_runtime.attention self", ocdeckevent.NewServeRuntimeAttentionChanged("iv1", "t1"), true},
		{"serve_runtime.attention other", ocdeckevent.NewServeRuntimeAttentionChanged("iv1", "t2"), false},
		{"serve_runtime.run_status self", ocdeckevent.NewServeRuntimeRunStatusChanged("iv1", "t1", "idle", "busy", true), true},
		{"serve_runtime.run_status other", ocdeckevent.NewServeRuntimeRunStatusChanged("iv1", "t2", "idle", "busy", true), false},
		{"resync.requested", ocdeckevent.NewResyncRequested(), true},
		{"unknown type", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: "task.unknown_future", RID: "t2"}, true},
		{"unknown type other topic", ocdeckevent.Event{Topic: "future.topic", Type: "future.thing", RID: "x"}, true},
		{"empty type", ocdeckevent.Event{}, true},
		{"task.status malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskStatusChanged, RID: "t2", Payload: map[string]any{"from": "active"}}, true},
		{"task.deleted malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskDeleted, RID: "t2", Payload: nil}, true},
		{"task.created wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeTaskCreated, RID: "t2", Payload: struct{}{}}, true},
		{"session.claimed malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeSessionClaimed, RID: "s1", Payload: "x"}, true},
		{"session.claimed wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeSessionClaimed, RID: "s1", Payload: ocdeckevent.SessionOwnerPayload{TaskID: "t2"}}, true},
		{"serve_runtime.attention malformed", ocdeckevent.Event{Topic: ocdeckevent.TopicServeRuntime, Type: ocdeckevent.TypeServeRuntimeAttentionChanged, RID: "iv", Payload: nil}, true},
		{"serve_runtime.run_status malformed", ocdeckevent.Event{Topic: ocdeckevent.TopicServeRuntime, Type: ocdeckevent.TypeServeRuntimeRunStatusChanged, RID: "iv", Payload: ocdeckevent.ServeRuntimeTaskPayload{TaskID: "t2"}}, true},
		{"sessions.aligned malformed", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeSessionsAligned, RID: "t2", Payload: nil}, true},
		{"resync malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicControl, Type: ocdeckevent.TypeResyncRequested, Payload: "x"}, true},
		{"resync wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeResyncRequested, Payload: struct{}{}}, true},
		{"task.created malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskCreated, RID: "t2", Payload: nil}, true},
		{"task.activity_changed malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeTaskActivityChanged, RID: "t2", Payload: "x"}, true},
		{"task.activity_changed wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicControl, Type: ocdeckevent.TypeTaskActivityChanged, RID: "t2", Payload: struct{}{}}, true},
		{"task.status_changed wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeTaskStatusChanged, RID: "t2", Payload: ocdeckevent.TaskStatusChangedPayload{From: "a", To: "b"}}, true},
		{"task.deleted wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicServeRuntime, Type: ocdeckevent.TypeTaskDeleted, RID: "t2", Payload: ocdeckevent.TaskDeletedPayload{From: "active"}}, true},
		{"sessions.aligned wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeSessionsAligned, RID: "t2", Payload: ocdeckevent.SessionsAlignedPayload{}}, true},
		{"session.touched malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeSessionTouched, RID: "s1", Payload: nil}, true},
		{"session.touched wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeSessionTouched, RID: "s1", Payload: ocdeckevent.SessionOwnerPayload{TaskID: "t2"}}, true},
		{"session.deleted malformed payload", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeSessionDeleted, RID: "s1", Payload: map[string]any{}}, true},
		{"session.deleted wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicControl, Type: ocdeckevent.TypeSessionDeleted, RID: "s1", Payload: ocdeckevent.SessionOwnerPayload{TaskID: "t2"}}, true},
		{"serve_runtime.attention wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicSession, Type: ocdeckevent.TypeServeRuntimeAttentionChanged, RID: "iv", Payload: ocdeckevent.ServeRuntimeTaskPayload{TaskID: "t2"}}, true},
		{"serve_runtime.run_status wrong topic", ocdeckevent.Event{Topic: ocdeckevent.TopicTask, Type: ocdeckevent.TypeServeRuntimeRunStatusChanged, RID: "iv", Payload: ocdeckevent.ServeRuntimeRunStatusChangedPayload{TaskID: "t2"}}, true},
	}
	for _, c := range cases {
		if got := dirty(c.ev); got != c.want {
			t.Errorf("%s: eventDirtiesTaskDetail = %v, want %v", c.name, got, c.want)
		}
	}
}
