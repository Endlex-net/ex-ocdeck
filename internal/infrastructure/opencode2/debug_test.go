// debug_test.go 落实 OCDECK_DEBUG 传输层开关态与脱敏负断言：默认关零输出；
// 开启后 [debug] method/URI/status 在位、错误 body 前 200 字符在位；凭据
// （Basic Auth/密码）永不落日志。
package opencode2

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureDebug 捕获包级 log 输出执行 fn，返回原文。
func captureDebug(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()
	fn()
	return buf.String()
}

func TestDebugTransport_OffZeroOutput(t *testing.T) {
	SetDebugLogging(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
	}))
	defer srv.Close()
	out := captureDebug(t, func() {
		c := NewClient(portOf(srv.URL), "sekret", Options{})
		if _, err := c.ListSessions(context.Background(), "/wt", 10); err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
	})
	if strings.Contains(out, "[debug]") {
		t.Fatalf("debug disabled must produce zero [debug] output, got: %q", out)
	}
}

func TestDebugTransport_OnLogsAndRedacts(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 服务端回显 Authorization 值（若实现错误地记录 header，会出现在日志里）。
		if r.URL.Path != "/api/session" {
			// GetSession 命中 /api/session/{id} → 500（错误 body tee 断言面）。
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("x", 300)))
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
	}))
	defer srv.Close()
	out := captureDebug(t, func() {
		c := NewClient(portOf(srv.URL), "sekret-password", Options{})
		if _, err := c.ListSessions(context.Background(), "/wt", 10); err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if _, err := c.GetSession(context.Background(), "ses_x"); err == nil {
			t.Fatalf("expected error from /boom")
		}
	})
	if !strings.Contains(out, "[debug] opencode2: http GET /api/session") {
		t.Fatalf("request diagnostics missing, got: %q", out)
	}
	if !strings.Contains(out, "-> 200") {
		t.Fatalf("status diagnostics missing, got: %q", out)
	}
	if !strings.Contains(out, "-> 500") {
		t.Fatalf("error status diagnostics missing, got: %q", out)
	}
	if !strings.Contains(out, "http error body") {
		t.Fatalf("error body diagnostics missing, got: %q", out)
	}
	// 脱敏负断言：密码/Authorization 值永不落日志。
	if strings.Contains(out, "sekret-password") {
		t.Fatalf("password leaked into debug log: %q", out)
	}
	if strings.Contains(out, "Authorization") || strings.Contains(out, "Basic ") {
		t.Fatalf("auth header leaked into debug log: %q", out)
	}
}

// portOf 从 httptest server URL 提取端口（NewClient 以 127.0.0.1:port 形态构造）。
func portOf(rawURL string) int {
	const scheme = "http://127.0.0.1:"
	if len(rawURL) <= len(scheme) {
		return 0
	}
	n := 0
	for i := len(scheme); i < len(rawURL); i++ {
		c := rawURL[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		panic(errors.New("cannot parse port from " + rawURL))
	}
	return n
}
