package event

import (
	"testing"
)

// Type 字面量与 design D0 事件类型目录逐字一致。
func TestTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		t    Type
		want string
	}{
		{"TaskCreated", TypeTaskCreated, "task.created"},
		{"TaskStatusChanged", TypeTaskStatusChanged, "task.status_changed"},
		{"TaskDeleted", TypeTaskDeleted, "task.deleted"},
		{"TaskActivityChanged", TypeTaskActivityChanged, "task.activity_changed"},
		{"SessionClaimed", TypeSessionClaimed, "session.claimed"},
		{"SessionTouched", TypeSessionTouched, "session.touched"},
		{"SessionDeleted", TypeSessionDeleted, "session.deleted"},
		{"SessionsAligned", TypeSessionsAligned, "sessions.aligned"},
		{"ServeRuntimeAttentionChanged", TypeServeRuntimeAttentionChanged, "serve_runtime.attention_changed"},
		{"ServeRuntimeRunStatusChanged", TypeServeRuntimeRunStatusChanged, "serve_runtime.run_status_changed"},
		{"ServeRuntimeSessionError", TypeServeRuntimeSessionError, "serve_runtime.session_error"},
		{"ResyncRequested", TypeResyncRequested, "resync.requested"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.t) != c.want {
				t.Fatalf("Type %s = %q, want %q", c.name, c.t, c.want)
			}
		})
	}
}

// Topic 字面量与 design D0 一致。
func TestTopicConstants(t *testing.T) {
	cases := []struct {
		name string
		topic Topic
		want  string
	}{
		{"Task", TopicTask, "task"},
		{"Session", TopicSession, "session"},
		{"ServeRuntime", TopicServeRuntime, "serve_runtime"},
		{"Control", TopicControl, "control"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.topic) != c.want {
				t.Fatalf("Topic %s = %q, want %q", c.name, c.topic, c.want)
			}
		})
	}
}

// Type/Topic 闭合常量目录：12 个 Type、4 个 Topic（task-notifications D2 增量后）。
func TestClosedCatalogSize(t *testing.T) {
	allTypes := []Type{
		TypeTaskCreated, TypeTaskStatusChanged, TypeTaskDeleted, TypeTaskActivityChanged,
		TypeSessionClaimed, TypeSessionTouched, TypeSessionDeleted, TypeSessionsAligned,
		TypeServeRuntimeAttentionChanged, TypeServeRuntimeRunStatusChanged,
		TypeServeRuntimeSessionError,
		TypeResyncRequested,
	}
	if len(allTypes) != 12 {
		t.Fatalf("expected 12 Type constants, got %d", len(allTypes))
	}
	// 确认无重复。
	seen := make(map[Type]bool, len(allTypes))
	for _, ty := range allTypes {
		if seen[ty] {
			t.Fatalf("duplicate Type constant %q", ty)
		}
		seen[ty] = true
	}

	allTopics := []Topic{TopicTask, TopicSession, TopicServeRuntime, TopicControl}
	if len(allTopics) != 4 {
		t.Fatalf("expected 4 Topic constants, got %d", len(allTopics))
	}
}

func TestNewTaskCreated(t *testing.T) {
	got := NewTaskCreated("tsk_1")
	if got.Topic != TopicTask || got.Type != TypeTaskCreated || got.RID != "tsk_1" {
		t.Fatalf("NewTaskCreated mismatch: %+v", got)
	}
}

func TestNewTaskStatusChanged(t *testing.T) {
	got := NewTaskStatusChanged("tsk_1", "suspended", "activating")
	if got.Topic != TopicTask || got.Type != TypeTaskStatusChanged || got.RID != "tsk_1" {
		t.Fatalf("mismatch: %+v", got)
	}
	p, ok := got.Payload.(TaskStatusChangedPayload)
	if !ok {
		t.Fatalf("payload type = %T, want TaskStatusChangedPayload", got.Payload)
	}
	if p.From != "suspended" || p.To != "activating" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestNewTaskDeleted(t *testing.T) {
	got := NewTaskDeleted("tsk_1", "active")
	if got.RID != "tsk_1" {
		t.Fatalf("RID = %q", got.RID)
	}
	p, ok := got.Payload.(TaskDeletedPayload)
	if !ok || p.From != "active" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestNewTaskActivityChanged(t *testing.T) {
	got := NewTaskActivityChanged("tsk_1")
	if got.Type != TypeTaskActivityChanged || got.RID != "tsk_1" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestNewSessionClaimed(t *testing.T) {
	got := NewSessionClaimed("sess_1", "tsk_1")
	if got.Topic != TopicSession || got.Type != TypeSessionClaimed || got.RID != "sess_1" {
		t.Fatalf("mismatch: %+v", got)
	}
	p, ok := got.Payload.(SessionOwnerPayload)
	if !ok || p.TaskID != "tsk_1" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestNewSessionTouched(t *testing.T) {
	got := NewSessionTouched("sess_1", "tsk_1")
	if got.RID != "sess_1" {
		t.Fatalf("RID = %q", got.RID)
	}
	p, ok := got.Payload.(SessionOwnerPayload)
	if !ok || p.TaskID != "tsk_1" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestNewSessionDeleted(t *testing.T) {
	got := NewSessionDeleted("sess_1", "tsk_1")
	if got.RID != "sess_1" {
		t.Fatalf("RID = %q", got.RID)
	}
	p, ok := got.Payload.(SessionOwnerPayload)
	if !ok || p.TaskID != "tsk_1" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestNewSessionsAligned(t *testing.T) {
	ids := []string{"sess_1", "sess_2"}
	got := NewSessionsAligned("tsk_1", 1, 2, 3, ids)
	if got.Topic != TopicSession || got.Type != TypeSessionsAligned || got.RID != "tsk_1" {
		t.Fatalf("mismatch: %+v", got)
	}
	p, ok := got.Payload.(SessionsAlignedPayload)
	if !ok {
		t.Fatalf("payload type = %T", got.Payload)
	}
	if p.Inserted != 1 || p.Touched != 2 || p.Deleted != 3 || len(p.SessionIDs) != 2 {
		t.Fatalf("payload = %+v", p)
	}
}

func TestNewServeRuntimeAttentionChanged(t *testing.T) {
	got := NewServeRuntimeAttentionChanged("inst_1", "tsk_1")
	if got.Topic != TopicServeRuntime || got.Type != TypeServeRuntimeAttentionChanged || got.RID != "inst_1" {
		t.Fatalf("mismatch: %+v", got)
	}
	p, ok := got.Payload.(ServeRuntimeTaskPayload)
	if !ok || p.TaskID != "tsk_1" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

func TestNewServeRuntimeRunStatusChanged(t *testing.T) {
	got := NewServeRuntimeRunStatusChanged("inst_1", "tsk_1", "idle", "busy", true)
	if got.RID != "inst_1" {
		t.Fatalf("RID = %q", got.RID)
	}
	p, ok := got.Payload.(ServeRuntimeRunStatusChangedPayload)
	if !ok {
		t.Fatalf("payload type = %T", got.Payload)
	}
	if p.TaskID != "tsk_1" || p.From != "idle" || p.To != "busy" || !p.Available {
		t.Fatalf("payload = %+v", p)
	}
}

func TestNewServeRuntimeSessionError(t *testing.T) {
	code, retry := 429, false
	got := NewServeRuntimeSessionError("inst_1", "tsk_1", "ses_1", "APIError", "boom", &code, &retry)
	if got.Topic != TopicServeRuntime || got.Type != TypeServeRuntimeSessionError || got.RID != "inst_1" {
		t.Fatalf("mismatch: %+v", got)
	}
	p, ok := got.Payload.(ServeRuntimeSessionErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T", got.Payload)
	}
	if p.TaskID != "tsk_1" || p.SessionID != "ses_1" || p.Name != "APIError" || p.Message != "boom" {
		t.Fatalf("payload = %+v", p)
	}
	if p.StatusCode == nil || *p.StatusCode != 429 || p.IsRetryable == nil || *p.IsRetryable {
		t.Fatalf("nullable payload fields = %+v %+v", p.StatusCode, p.IsRetryable)
	}
	// 可空字段 nil 语义：缺失时指针为 nil（不做零值歧义）。
	gotNil := NewServeRuntimeSessionError("inst_1", "tsk_1", "ses_1", "APIError", "boom", nil, nil)
	pNil := gotNil.Payload.(ServeRuntimeSessionErrorPayload)
	if pNil.StatusCode != nil || pNil.IsRetryable != nil {
		t.Fatalf("absent nullable fields must be nil: %+v", pNil)
	}
}

func TestNewResyncRequested(t *testing.T) {
	// 事件无主体，RID 固定为空（design.md:220 事件目录）。
	got := NewResyncRequested()
	if got.Topic != TopicControl || got.Type != TypeResyncRequested || got.RID != "" {
		t.Fatalf("mismatch: %+v", got)
	}
}