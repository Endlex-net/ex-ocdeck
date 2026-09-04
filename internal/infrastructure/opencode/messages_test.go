// messages_test.go ListMessages（task-notifications design D9：GET
// /session/:id/message?directory=&limit=）：路径/query/鉴权、404/401/畸形
// 响应 fail-closed。
package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListMessages_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/session/ses_1/message" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Fatalf("limit query: %s", got)
		}
		requireDirectory(t, r, "/wt")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"m1","role":"user","parts":[{"type":"text","text":"继续"}]},
			{"id":"m2","role":"assistant","parts":[{"type":"text","text":"已完成"},{"type":"tool","tool":"bash"}]}
		]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	msgs, err := c.ListMessages(context.Background(), "/wt", "ses_1", 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != "m1" || msgs[0].Role != "user" || len(msgs[0].Parts) != 1 ||
		msgs[0].Parts[0].Type != "text" || msgs[0].Parts[0].Text != "继续" {
		t.Fatalf("message[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].Parts) != 2 || msgs[1].Parts[1].Type != "tool" {
		t.Fatalf("message[1] = %+v", msgs[1])
	}
}

// TestListMessages_NestedInfoRole live OpenCode 响应为 {info:{id,role},parts}
// （顶层无 id/role），解码后 ID/Role 应从 info 兜底回填，使 LastAgentOutput
// 能命中 assistant；旧 flat 形状仍由 TestListMessages_OK 覆盖。
func TestListMessages_NestedInfoRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDirectory(t, r, "/wt")
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Fatalf("limit query: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"info":{"id":"m1","role":"user"},"parts":[{"type":"text","text":"继续"}]},
			{"info":{"id":"m2","role":"assistant"},"parts":[{"type":"step-start"},{"type":"text","text":"已完成登录"},{"type":"tool"}]}
		]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	msgs, err := c.ListMessages(context.Background(), "/wt", "ses_1", 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != "m1" || msgs[0].Role != "user" || len(msgs[0].Parts) != 1 ||
		msgs[0].Parts[0].Type != "text" || msgs[0].Parts[0].Text != "继续" {
		t.Fatalf("message[0] = %+v", msgs[0])
	}
	if msgs[1].ID != "m2" || msgs[1].Role != "assistant" {
		t.Fatalf("message[1] = %+v", msgs[1])
	}
	var hasText bool
	for _, p := range msgs[1].Parts {
		if p.Type == "text" && p.Text == "已完成登录" {
			hasText = true
		}
	}
	if !hasText {
		t.Fatalf("message[1] missing text part: %+v", msgs[1].Parts)
	}
}

// TestListMessages_MixedFlatAndNested 同一列表混用 flat 与 nested 形状时
// 两种都应正确解码。
func TestListMessages_MixedFlatAndNested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"f1","role":"user","parts":[{"type":"text","text":"flat"}]},
			{"info":{"id":"n2","role":"assistant"},"parts":[{"type":"text","text":"nested"}]}
		]`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	msgs, err := c.ListMessages(context.Background(), "/wt", "s1", 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != "f1" || msgs[0].Role != "user" || msgs[0].Parts[0].Text != "flat" {
		t.Fatalf("message[0] = %+v", msgs[0])
	}
	if msgs[1].ID != "n2" || msgs[1].Role != "assistant" || msgs[1].Parts[0].Text != "nested" {
		t.Fatalf("message[1] = %+v", msgs[1])
	}
}

func TestListMessages_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if _, err := c.ListMessages(context.Background(), "/wt", "gone", 10); err == nil {
		t.Fatal("404 must fail-closed")
	}
}

func TestListMessages_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "wrong")
	if _, err := c.ListMessages(context.Background(), "/wt", "s1", 10); err == nil {
		t.Fatal("401 must fail-closed")
	}
}

// TestListMessages_Malformed 响应非消息数组（对象/字段类型漂移）→ 解析失败
// fail-closed（design D9：解析失败 → 不可得）。
func TestListMessages_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"unexpected object shape"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if _, err := c.ListMessages(context.Background(), "/wt", "s1", 10); err == nil {
		t.Fatal("non-array body must fail-closed")
	}
}
