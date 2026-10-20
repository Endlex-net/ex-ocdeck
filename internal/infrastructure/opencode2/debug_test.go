// debug_test.go 落实 OCDECK_DEBUG 传输层开关态与脱敏负断言：默认关零输出；
// 开启后 [debug] 仅白名单字段（method/URI/status）在位；响应 body 一律不落
// 日志（服务器 500 可能回显 Authorization/密码/env——截断≠脱敏）。
package opencode2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
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

// TestDebugTransport_OnLogsAndRedacts 落实白名单口径：错误响应真实回显
// Authorization 值/密码/env secret——断言日志零命中（body 一律不落日志，
// 截断≠脱敏）；method/URI/status 正断言在位。
func TestDebugTransport_OnLogsAndRedacts(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	const envSecret = "topsecret-env-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session" {
			// GetSession 命中 /api/session/{id} → 500 且 body 回显请求敏感信息
			//（实服务器事故形态：错误处理把 Authorization/env 原样回显）。
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"denied","authorization":%q,"password":"sekret-password","env":%q,"pad":%q}`,
				r.Header.Get("Authorization"), envSecret, strings.Repeat("x", 300))
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
			t.Fatalf("expected error from /api/session/ses_x")
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
	// 脱敏负断言：错误响应 body（含回显的 Authorization 值/密码/env secret）
	// 永不落日志——截断≠脱敏，body 一律不输出。
	for _, secret := range []string{"sekret-password", envSecret, "Basic ", "Authorization", "denied", strings.Repeat("x", 100)} {
		if strings.Contains(out, secret) {
			t.Fatalf("error body content %q leaked into debug log: %q", secret, out)
		}
	}
	if strings.Contains(out, "http error body") {
		t.Fatalf("body must not be logged at all, got: %q", out)
	}
}

// TestDebugSupervisor_RecycleFailNoBody 覆盖 supervisor 错误日志路径
// （recycleCandidate DELETE 失败）：debug 日志只含白名单字段
// （session/status/kind），500 响应回显的 Authorization/密码不落日志。
func TestDebugSupervisor_RecycleFailNoBody(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"boom","authorization":%q,"password":"sekret-password"}`,
			r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	sup := &V2Supervisor{deps: SupervisorDeps{Store: &fakeRecordStore{}}}
	c := NewClient(portOf(srv.URL), "sekret-password", Options{})
	out := captureDebug(t, func() {
		err := sup.recycleCandidate(context.Background(), c, "ses_bad")
		var debt *appagent.CandidateCleanupDebt
		if !errors.As(err, &debt) {
			t.Fatalf("recycleCandidate must surface CandidateCleanupDebt, got %v", err)
		}
	})
	if !strings.Contains(out, "recycle_candidate: DELETE failed session=ses_bad status=500") {
		t.Fatalf("whitelisted diagnostics missing, got: %q", out)
	}
	for _, secret := range []string{"sekret-password", "Basic ", "boom"} {
		if strings.Contains(out, secret) {
			t.Fatalf("error body content %q leaked into debug log: %q", secret, out)
		}
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
