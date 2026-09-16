// session_title_test.go 覆盖 UpdateSessionTitle 客户端契约（task-info-editable 3.2）：
// PATCH 方法、PathEscape/QueryEscape、请求体 title、成功响应 ID 校验、404/401/其他错误矩阵。
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateSessionTitle_SuccessAndRequestShape(t *testing.T) {
	const (
		wantID    = "ses_abc"
		wantDir   = "/tmp/wt dir"
		wantTitle = "新任务名"
	)
	var (
		gotMethod string
		gotPath   string
		gotRawDir string
		gotBody   map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotRawDir = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + wantID + `","time":{"created":1,"updated":2}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if err := c.UpdateSessionTitle(context.Background(), wantDir, wantID, wantTitle); err != nil {
		t.Fatalf("UpdateSessionTitle: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	// id 经 url.PathEscape（路径段不能出现原始 /）。
	if gotPath != "/session/"+wantID {
		t.Errorf("path = %s, want /session/%s", gotPath, wantID)
	}
	// directory 经 QueryEscape（空格不破坏 query 结构）。
	if !strings.Contains(gotRawDir, "directory=%2Ftmp%2Fwt+dir") {
		t.Errorf("raw query = %s, want escaped directory", gotRawDir)
	}
	if gotBody["title"] != wantTitle {
		t.Errorf("body title = %q, want %q", gotBody["title"], wantTitle)
	}
}

func TestUpdateSessionTitle_PathEscapeSpecialID(t *testing.T) {
	var gotEscapedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"id":"ses/a","time":{"created":1,"updated":2}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	// 含 / 的 id（契约上不会出现，但 PathEscape 行为需稳定：/ 转义为 %2F）。
	if err := c.UpdateSessionTitle(context.Background(), "/wt", "ses/a", "t"); err != nil {
		t.Fatalf("UpdateSessionTitle: %v", err)
	}
	if gotEscapedPath != "/session/ses%2Fa" {
		t.Errorf("escaped path = %s, want /session/ses%%2Fa", gotEscapedPath)
	}
}

func TestUpdateSessionTitle_ResponseIDMustMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_other","time":{"created":1,"updated":2}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	err := c.UpdateSessionTitle(context.Background(), "/wt", "ses_1", "t")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want id mismatch error", err)
	}
}

func TestUpdateSessionTitle_ErrorMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantErr    error
		wantSentin bool // errors.Is 目标 sentinel
	}{
		{"404 → unsupported", http.StatusNotFound, ErrSessionTitleUnsupported, true},
		{"401 → unauthorized", http.StatusUnauthorized, ErrUnauthorized, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "pw")
			err := c.UpdateSessionTitle(context.Background(), "/wt", "ses_1", "t")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	t.Run("500 → plain status error (not unsupported)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := newTestClient(t, srv, "pw")
		err := c.UpdateSessionTitle(context.Background(), "/wt", "ses_1", "t")
		if err == nil || errors.Is(err, ErrSessionTitleUnsupported) {
			t.Fatalf("err = %v, want non-unsupported status error", err)
		}
	})

	t.Run("network error → plain error", func(t *testing.T) {
		c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})), "pw")
		c.baseURL = "http://127.0.0.1:1" // 未监听端口：连接失败
		err := c.UpdateSessionTitle(context.Background(), "/wt", "ses_1", "t")
		if err == nil || errors.Is(err, ErrSessionTitleUnsupported) {
			t.Fatalf("err = %v, want network error (not sentinel)", err)
		}
	})
}
