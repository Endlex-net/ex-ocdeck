// client_test.go 落实 tasks 2.3：v2 client 传输层——/api 前缀拼装、{data}
// envelope 解包（含畸形 envelope 报错）、Basic auth、错误响应非 envelope 形态。
package opencode2

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_APIPrefixAssembly(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(readFixture(t, "session_get.json"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if _, err := c.GetSession(context.Background(), "ses_fixture01a"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// 前缀拼装唯一入口：/api + /session/{id}（CONTRACT §1 传输形态）。
	if gotPath != "/api/session/ses_fixture01a" {
		t.Fatalf("path: got %q want /api/session/ses_fixture01a", gotPath)
	}
}

func TestClient_EnvelopeUnwrap(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 200, body: string(readFixture(t, "session_get.json"))})
	c := newTestClient(t, srv, "pw")
	s, err := c.GetSession(context.Background(), "ses_fixture01a")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.ID != "ses_fixture01a" || s.Title != "renamed-title" || s.Location.Directory != "/wt" {
		t.Fatalf("session: %+v", s)
	}
	if s.Time.Created == nil || *s.Time.Created != 1792083316435 {
		t.Fatalf("time.created: %v", s.Time.Created)
	}
}

func TestClient_MalformedEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing data key", `{"foo": 1}`},
		{"null data", `{"data": null}`},
		{"array root", `[1,2,3]`},
		{"not json", `oops`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 200, body: tc.body})
			c := newTestClient(t, srv, "pw")
			if _, err := c.GetSession(context.Background(), "ses_fixture01a"); err == nil {
				t.Fatalf("expected error for malformed envelope %q", tc.body)
			}
		})
	}
}

func TestClient_ErrorBodyNonEnvelope(t *testing.T) {
	// CONTRACT §1：错误响应非 envelope，{_tag, message, kind} 形态；body 原文
	// 保留供 _tag 分类。
	errBody := string(readFixture(t, "error_permission_gone.json"))
	srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 400, body: errBody})
	c := newTestClient(t, srv, "pw")
	_, err := c.GetSession(context.Background(), "ses_fixture01a")
	var hse *httpStatusError
	if !errors.As(err, &hse) {
		t.Fatalf("expect httpStatusError, got %v", err)
	}
	if hse.status != 400 || !strings.Contains(hse.body, "PermissionNotFoundError") {
		t.Fatalf("status/body: %d %q", hse.status, hse.body)
	}
	if errorTag(hse.body) != "PermissionNotFoundError" {
		t.Fatalf("errorTag: %q", errorTag(hse.body))
	}
}

func TestClient_UnauthorizedSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "wrong-password")
	if _, err := c.GetSession(context.Background(), "ses_fixture01a"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expect ErrUnauthorized, got %v", err)
	}
}

func TestClient_BasicAuthHeader(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = io.WriteString(w, `{"data":{"id":"ses_fixture01a","time":{"created":1,"updated":2},"location":{"directory":"/wt"}}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "derived-password")
	if _, err := c.GetSession(context.Background(), "ses_fixture01a"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotUser != "opencode" || gotPass != "derived-password" {
		t.Fatalf("basic auth: user=%q pass=%q", gotUser, gotPass)
	}
}

func TestClient_ListSessionsCursor(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page1.json"))})
	c := newTestClient(t, srv, "pw")
	page, err := c.ListSessions(context.Background(), "/wt", 1000)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page.Sessions) != 2 || !page.HasMore {
		t.Fatalf("page: len=%d hasMore=%v", len(page.Sessions), page.HasMore)
	}
}

func TestClient_SessionShapeViolation(t *testing.T) {
	// Session.Info required 形状缺失 = 协议违约（fail-closed）。
	cases := []struct {
		name string
		body string
	}{
		{"missing time", `{"data":{"id":"ses_fixture01a","location":{"directory":"/wt"}}}`},
		{"missing location", `{"data":{"id":"ses_fixture01a","time":{"created":1,"updated":2}}}`},
		{"missing id", `{"data":{"time":{"created":1,"updated":2},"location":{"directory":"/wt"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtureServer(t, route{path: "/api/session/ses_fixture01a", status: 200, body: tc.body})
			c := newTestClient(t, srv, "pw")
			if _, err := c.GetSession(context.Background(), "ses_fixture01a"); err == nil {
				t.Fatalf("expected shape violation error")
			}
		})
	}
}
