package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ocdeck/internal/domain/notification"
)

var _ notification.Channel = (*BarkChannel)(nil)

// barkTestIntent wire 契约测试用意图（字段值含泄露检查标记，供 Err 禁泄露断言）。
func barkTestIntent() notification.Intent {
	return notification.Intent{
		TaskID:   "task-42",
		TaskName: "demo-task",
		Category: notification.CategoryQuestion,
		Level:    notification.LevelTimeSensitive,
		Title:    "等待你的回答",
		Body:     "demo-task\nleak-check-body-marker",
		URL:      "http://127.0.0.1:18080/#/task/task-42",
	}
}

const barkTestToken = "leak-check-token-000123"

func barkTestConfig(endpoint string) notification.ChannelConfig {
	return notification.ChannelConfig{Endpoint: endpoint, Token: barkTestToken}
}

// capturedBarkRequest handler 侧捕获的单次请求。
type capturedBarkRequest struct {
	method      string
	path        string
	contentType string
	rawBody     []byte
}

// barkCaptureServer 起 fake Bark server，respond 决定响应内容，返回捕获器。
func barkCaptureServer(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *capturedBarkRequest) {
	t.Helper()
	captured := &capturedBarkRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.contentType = r.Header.Get("Content-Type")
		captured.rawBody = body
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// barkSizedJSON 构造总长恰为 total 字节、code=200 的合法 JSON（用于 64KiB 边界测试）。
func barkSizedJSON(t *testing.T, total int) string {
	t.Helper()
	const prefix = `{"code":200,"pad":"`
	const suffix = `"}`
	pad := total - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("total %d too small", total)
	}
	return prefix + strings.Repeat("a", pad) + suffix
}

// TestBarkChannel_SendWireContract 覆盖 wire 契约全项：POST <endpoint>/push
// （尾部 '/' 拼接前剔除）、Content-Type: application/json、请求体六字段与取值。
func TestBarkChannel_SendWireContract(t *testing.T) {
	srv, captured := barkCaptureServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"code":200}`))
	})
	// endpoint 带尾部 '/'，验证拼接前剔除。
	res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL+"/"))
	if !res.OK || res.Err != "" {
		t.Fatalf("expect success, got OK=%v Err=%q", res.OK, res.Err)
	}
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", captured.method)
	}
	if captured.path != "/push" {
		t.Fatalf("path = %q, want /push (trailing slash must be trimmed)", captured.path)
	}
	if captured.contentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", captured.contentType)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(captured.rawBody, &keys); err != nil {
		t.Fatalf("request body not a JSON object: %v", err)
	}
	wantKeys := []string{"device_key", "title", "body", "level", "group", "url"}
	if len(keys) != len(wantKeys) {
		t.Fatalf("request body keys = %v, want exactly %v", keys, wantKeys)
	}
	var got barkRequest
	if err := json.Unmarshal(captured.rawBody, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	in := barkTestIntent()
	want := barkRequest{
		DeviceKey: barkTestToken,
		Title:     in.Title,
		Body:      in.Body,
		Level:     string(in.Level),
		Group:     in.TaskID,
		URL:       in.URL,
	}
	if got != want {
		t.Fatalf("request body = %+v, want %+v", got, want)
	}
}

func TestBarkChannel_Non2xxFails(t *testing.T) {
	srv, _ := barkCaptureServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
	if res.OK {
		t.Fatal("non-2xx must fail")
	}
}

// TestBarkChannel_RedirectNotFollowed 禁跟随重定向：302 不发起第二次请求，
// 按非 2xx 失败处理。
func TestBarkChannel_RedirectNotFollowed(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/push" {
			http.Redirect(w, r, "/leak-target", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
	if res.OK {
		t.Fatal("redirect must be treated as failure")
	}
	if hits != 1 {
		t.Fatalf("redirect target hit %d times, want 0 (must not follow)", hits-1)
	}
}

// TestBarkChannel_TimeoutFails 请求超时判定失败（缩短 client 超时避免慢测试；
// 生产 10s 由 TestBarkChannel_DefaultClientContract 固定）。
func TestBarkChannel_TimeoutFails(t *testing.T) {
	srv, _ := barkCaptureServer(t, func(w http.ResponseWriter) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":200}`))
	})
	start := time.Now()
	res := newBarkChannel(150*time.Millisecond).Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
	if res.OK {
		t.Fatal("timeout must fail")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout enforced too late: %v", el)
	}
}

// TestBarkChannel_ResponseSizeLimit 64KiB 上界：恰 64KiB 合法响应成功，
// 超过 1 字节判定失败。
func TestBarkChannel_ResponseSizeLimit(t *testing.T) {
	t.Run("exactly 64KiB succeeds", func(t *testing.T) {
		srv, _ := barkCaptureServer(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(barkSizedJSON(t, 64*1024)))
		})
		res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
		if !res.OK {
			t.Fatalf("64KiB exactly must succeed, got Err=%q", res.Err)
		}
	})
	t.Run("over 64KiB fails", func(t *testing.T) {
		srv, _ := barkCaptureServer(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(barkSizedJSON(t, 64*1024+1)))
		})
		res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
		if res.OK {
			t.Fatal("response over 64KiB must fail")
		}
	})
}

// TestBarkChannel_ResponseJudgement 成功判定 = HTTP 2xx 且 JSON code==200；
// code 非 200、非法 JSON、缺 code 字段均失败。
func TestBarkChannel_ResponseJudgement(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"code 200", `{"code":200}`, true},
		{"code 400", `{"code":400}`, false},
		{"malformed json", `not-json`, false},
		{"missing code", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := barkCaptureServer(t, func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(tc.body))
			})
			res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (Err=%q)", res.OK, tc.wantOK, res.Err)
			}
		})
	}
}

// TestBarkChannel_ErrNeverLeaksSecrets 失败路径的 Err（将进日志）MUST NOT 携带
// token、请求体内容与 Bark 响应原文。
func TestBarkChannel_ErrNeverLeaksSecrets(t *testing.T) {
	scenarios := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"code":400,"message":"raw-response-marker-xyz"}`))
		},
		func(w http.ResponseWriter) { _, _ = w.Write([]byte(`raw-response-marker-xyz not json`)) },
	}
	for i, respond := range scenarios {
		srv, _ := barkCaptureServer(t, respond)
		res := NewBarkChannel().Send(context.Background(), barkTestIntent(), barkTestConfig(srv.URL))
		if res.OK {
			t.Fatalf("scenario %d must fail", i)
		}
		for _, secret := range []string{barkTestToken, "leak-check-body-marker", "raw-response-marker-xyz", "等待你的回答"} {
			if strings.Contains(res.Err, secret) {
				t.Fatalf("scenario %d Err leaks %q: %q", i, secret, res.Err)
			}
		}
	}
}

// TestBarkChannel_DefaultClientContract 生产构造的 wire 参数：10s 超时、
// CheckRedirect 返回 ErrUseLastResponse（禁跟随）。
func TestBarkChannel_DefaultClientContract(t *testing.T) {
	ch := NewBarkChannel()
	if ch.client.Timeout != 10*time.Second {
		t.Fatalf("client timeout = %v, want 10s", ch.client.Timeout)
	}
	if ch.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := ch.client.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect error = %v, want http.ErrUseLastResponse", err)
	}
}

func TestBarkChannel_NameAndCaps(t *testing.T) {
	ch := NewBarkChannel()
	if ch.Name() != "bark" {
		t.Fatalf("name = %q, want bark", ch.Name())
	}
	if ch.Caps() != notification.CapGroup {
		t.Fatalf("caps = %v, want CapGroup", ch.Caps())
	}
}
