package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAttentionEvent(t *testing.T) {
	// 10 种事件 fixture（v1+v2 家族）。
	tests := []struct {
		name string
		ev   Event
		want AttentionEvent
		ok   bool
	}{
		{
			name: "permission.asked v1",
			ev: Event{Type: "permission.asked", Properties: map[string]interface{}{
				"id": "req1", "sessionID": "s1", "permission": "bash", "patterns": []interface{}{"rm"},
			}},
			want: AttentionEvent{Kind: AttentionAsked, Type: AttentionPermission, RequestID: "req1", SessionID: "s1", Permission: "bash", Patterns: []string{"rm"}},
			ok:   true,
		},
		{
			name: "permission.v2.asked",
			ev: Event{Type: "permission.v2.asked", Properties: map[string]interface{}{
				"id": "req2", "sessionID": "s2", "permission": "edit", "patterns": []interface{}{},
			}},
			want: AttentionEvent{Kind: AttentionAsked, Type: AttentionPermission, RequestID: "req2", SessionID: "s2", Permission: "edit", Patterns: []string{}},
			ok:   true,
		},
		{
			name: "permission.replied v1",
			ev: Event{Type: "permission.replied", Properties: map[string]interface{}{
				"sessionID": "s1", "requestID": "req1", "reply": "once",
			}},
			want: AttentionEvent{Kind: AttentionReplied, Type: AttentionPermission, RequestID: "req1", SessionID: "s1"},
			ok:   true,
		},
		{
			name: "permission.v2.replied",
			ev: Event{Type: "permission.v2.replied", Properties: map[string]interface{}{
				"sessionID": "s2", "requestID": "req2", "reply": "always",
			}},
			want: AttentionEvent{Kind: AttentionReplied, Type: AttentionPermission, RequestID: "req2", SessionID: "s2"},
			ok:   true,
		},
		{
			name: "question.asked v1",
			ev: Event{Type: "question.asked", Properties: map[string]interface{}{
				"id": "q1", "sessionID": "s1", "questions": []interface{}{
					map[string]interface{}{"header": "h1", "question": "what?"},
				},
			}},
			want: AttentionEvent{Kind: AttentionAsked, Type: AttentionQuestion, RequestID: "q1", SessionID: "s1", Questions: []QuestionItem{{Header: "h1", Question: "what?"}}},
			ok:   true,
		},
		{
			name: "question.v2.asked",
			ev: Event{Type: "question.v2.asked", Properties: map[string]interface{}{
				"id": "q2", "sessionID": "s2", "questions": []interface{}{
					map[string]interface{}{"header": "h2", "question": "why?"},
				},
			}},
			want: AttentionEvent{Kind: AttentionAsked, Type: AttentionQuestion, RequestID: "q2", SessionID: "s2", Questions: []QuestionItem{{Header: "h2", Question: "why?"}}},
			ok:   true,
		},
		{
			name: "question.replied v1",
			ev: Event{Type: "question.replied", Properties: map[string]interface{}{
				"sessionID": "s1", "requestID": "q1", "answers": [][]string{{"yes"}},
			}},
			want: AttentionEvent{Kind: AttentionReplied, Type: AttentionQuestion, RequestID: "q1", SessionID: "s1"},
			ok:   true,
		},
		{
			name: "question.v2.replied",
			ev: Event{Type: "question.v2.replied", Properties: map[string]interface{}{
				"sessionID": "s2", "requestID": "q2", "answers": [][]string{{"no"}},
			}},
			want: AttentionEvent{Kind: AttentionReplied, Type: AttentionQuestion, RequestID: "q2", SessionID: "s2"},
			ok:   true,
		},
		{
			name: "question.rejected v1",
			ev: Event{Type: "question.rejected", Properties: map[string]interface{}{
				"sessionID": "s1", "requestID": "q1",
			}},
			want: AttentionEvent{Kind: AttentionRejected, Type: AttentionQuestion, RequestID: "q1", SessionID: "s1"},
			ok:   true,
		},
		{
			name: "question.v2.rejected",
			ev: Event{Type: "question.v2.rejected", Properties: map[string]interface{}{
				"sessionID": "s2", "requestID": "q2",
			}},
			want: AttentionEvent{Kind: AttentionRejected, Type: AttentionQuestion, RequestID: "q2", SessionID: "s2"},
			ok:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseAttentionEvent(tc.ev)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !attentionEventEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseAttentionEvent_Malformed(t *testing.T) {
	// 缺字段 / 未知 type → false（静默忽略）。
	tests := []struct {
		name string
		ev   Event
	}{
		{name: "unknown type", ev: Event{Type: "session.created", Properties: map[string]interface{}{}}},
		{name: "permission.asked missing id", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"sessionID": "s1", "permission": "bash"}}},
		{name: "permission.asked missing sessionID", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "permission": "bash"}}},
		{name: "permission.asked missing permission", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1"}}},
		{name: "permission.asked patterns non-string", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1", "permission": "bash", "patterns": []interface{}{1}}}},
		{name: "permission.asked patterns non-array", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1", "permission": "bash", "patterns": "rm"}}},
		{name: "question.asked empty questions", ev: Event{Type: "question.asked", Properties: map[string]interface{}{"id": "q1", "sessionID": "s1", "questions": []interface{}{}}}},
		{name: "question.asked question missing header", ev: Event{Type: "question.asked", Properties: map[string]interface{}{"id": "q1", "sessionID": "s1", "questions": []interface{}{map[string]interface{}{"question": "x"}}}}},
		{name: "question.replied missing requestID", ev: Event{Type: "question.replied", Properties: map[string]interface{}{"sessionID": "s1"}}},
		{name: "question.rejected missing sessionID", ev: Event{Type: "question.rejected", Properties: map[string]interface{}{"requestID": "q1"}}},
		{name: "nil properties", ev: Event{Type: "permission.asked"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := ParseAttentionEvent(tc.ev)
			if ok {
				t.Errorf("expected ok=false for malformed event %q", tc.name)
			}
		})
	}
}

// patterns 缺省（缺失/null）合法，验证返回 ok=true 且 Patterns 为 nil 或空。
func TestParseAttentionEvent_PatternsOptional(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
	}{
		{name: "missing patterns key", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1", "permission": "bash"}}},
		{name: "patterns null", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1", "permission": "bash", "patterns": nil}}},
		{name: "empty patterns array", ev: Event{Type: "permission.asked", Properties: map[string]interface{}{"id": "r1", "sessionID": "s1", "permission": "bash", "patterns": []interface{}{}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseAttentionEvent(tc.ev)
			if !ok {
				t.Fatalf("expected ok=true for valid event with optional patterns")
			}
			if got.RequestID != "r1" {
				t.Errorf("RequestID = %q, want r1", got.RequestID)
			}
		})
	}
}

func attentionEventEqual(a, b AttentionEvent) bool {
	return a.Kind == b.Kind && a.Type == b.Type && a.RequestID == b.RequestID && a.SessionID == b.SessionID &&
		a.Permission == b.Permission && stringSliceEq(a.Patterns, b.Patterns) && questionSliceEq(a.Questions, b.Questions)
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func questionSliceEq(a, b []QuestionItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- REST pending 响应 fixture（正常/null/非数组/坏元素 → 整体失败） ---

func TestListPermissions_RestFixtures(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		status    int
		wantErr   bool
		wantUnsup bool
	}{
		{"normal", `[{"id":"r1","sessionID":"s1","permission":"bash","patterns":["rm"]}]`, 200, false, false},
		{"patterns null valid", `[{"id":"r1","sessionID":"s1","permission":"bash","patterns":null}]`, 200, false, false},
		{"patterns missing valid", `[{"id":"r1","sessionID":"s1","permission":"bash"}]`, 200, false, false},
		{"empty array", `[]`, 200, false, false},
		{"null", `null`, 200, true, false},
		{"non-array", `{}`, 200, true, false},
		{"bad element missing id", `[{"sessionID":"s1","permission":"bash"}]`, 200, true, false},
		{"404", `{"error":"not found"}`, 404, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "pw")
			res, err := c.ListPermissions(context.Background(), "/wt")
			if tc.wantUnsup {
				if !errors.Is(err, ErrCapabilityUnsupported) {
					t.Fatalf("err = %v, want ErrCapabilityUnsupported", err)
				}
				return
			}
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && res == nil {
				t.Fatalf("expected non-nil result")
			}
		})
	}
}

func TestListQuestions_RestFixtures(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		status    int
		wantErr   bool
		wantUnsup bool
	}{
		{"normal", `[{"id":"q1","sessionID":"s1","questions":[{"header":"h","question":"what?"}]}]`, 200, false, false},
		{"empty array", `[]`, 200, false, false},
		{"null", `null`, 200, true, false},
		{"non-array", `{}`, 200, true, false},
		{"bad element empty questions", `[{"id":"q1","sessionID":"s1","questions":[]}]`, 200, true, false},
		{"404", `{"error":"not found"}`, 404, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "pw")
			res, err := c.ListQuestions(context.Background(), "/wt")
			if tc.wantUnsup {
				if !errors.Is(err, ErrCapabilityUnsupported) {
					t.Fatalf("err = %v, want ErrCapabilityUnsupported", err)
				}
				return
			}
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && res == nil {
				t.Fatalf("expected non-nil result")
			}
		})
	}
}
