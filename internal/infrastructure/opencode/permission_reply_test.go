package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReplyPermission_ResultMapping 回复结果映射表驱动（task-permission-mode D6/D9）：
// 200 true 成功；200 false/空体/非法 JSON → 结果未知普通错误；401 → 普通错误
//（ErrUnauthorized，调用方 MUST NOT 标记 unsupported）；404 _tag 双语义；
// 其他 4xx/5xx → 结果未知普通错误。
func TestReplyPermission_ResultMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    error // nil = 成功；errors.Is 匹配；sentinelErrNot = 必须为普通错误（非两个 sentinel）
		plainError bool
	}{
		{name: "200 true 成功", status: 200, body: `true`},
		{name: "200 false 结果未知", status: 200, body: `false`, plainError: true},
		{name: "200 空体 结果未知", status: 200, body: ``, plainError: true},
		{name: "200 非法 JSON 结果未知", status: 200, body: `{"reply":`, plainError: true},
		{name: "401 普通错误不标记 unsupported", status: 401, body: `unauthorized`, wantErr: ErrUnauthorized},
		{name: "404 _tag=PermissionNotFoundError 竞态忽略", status: 404, body: `{"_tag":"PermissionNotFoundError"}`, wantErr: ErrPermissionRequestGone},
		{name: "404 无 _tag 路由不存在", status: 404, body: `404 page not found`, wantErr: ErrCapabilityUnsupported},
		{name: "404 其他 tag", status: 404, body: `{"_tag":"SomethingElse"}`, wantErr: ErrCapabilityUnsupported},
		{name: "500 结果未知", status: 500, body: `boom`, plainError: true},
		{name: "400 结果未知", status: 400, body: `bad request`, plainError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != "/permission/perm-1/reply" {
					t.Errorf("path = %s, want /permission/perm-1/reply", r.URL.Path)
				}
				if r.URL.Query().Get("directory") != "/wt" {
					t.Errorf("directory query = %q, want /wt", r.URL.Query().Get("directory"))
				}
				// body MUST be {"reply": <reply>}；MUST NOT 出现 always（spec 契约）。
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				if body["reply"] != "once" {
					t.Errorf("reply = %q, want once", body["reply"])
				}
				if body["reply"] == "always" {
					t.Errorf("reply MUST NOT be always")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, "pw")
			err := c.ReplyPermission(context.Background(), "/wt", "perm-1", "once")
			switch {
			case tc.plainError:
				if err == nil {
					t.Fatal("expected plain error (结果未知), got nil")
				}
				if errors.Is(err, ErrCapabilityUnsupported) || errors.Is(err, ErrPermissionRequestGone) {
					t.Fatalf("结果未知错误 MUST NOT 映射为能力/竞态 sentinel，got %v", err)
				}
			case tc.wantErr == nil:
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
			default:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected %v, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

// TestReplyPermission_GoneVsUnsupported_互斥 ErrPermissionRequestGone 与
// ErrCapabilityUnsupported 互斥（D6：仅路由 404 允许标记 reply-unsupported；
// 竞态 404 MUST NOT 触发能力降级）。
func TestReplyPermission_GoneVsUnsupported_互斥(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"_tag":"PermissionNotFoundError"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	err := c.ReplyPermission(context.Background(), "/wt", "perm-1", "reject")
	if !errors.Is(err, ErrPermissionRequestGone) {
		t.Fatalf("expected ErrPermissionRequestGone, got %v", err)
	}
	if errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatal("竞态 404 MUST NOT 同时是 ErrCapabilityUnsupported")
	}
}

// TestReplyPermission_Timeout_结果未知 客户端读响应超时（服务端已受理）→
// 返回错误（结果未知），调用方据此不重试不本地断言（D6 分类 ②）。
func TestReplyPermission_Timeout_结果未知(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `true`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.ReplyPermission(ctx, "/wt", "perm-1", "once")
	if err == nil {
		t.Fatal("expected timeout error (结果未知), got nil")
	}
	if errors.Is(err, ErrCapabilityUnsupported) || errors.Is(err, ErrPermissionRequestGone) {
		t.Fatalf("超时 MUST NOT 映射为能力/竞态 sentinel，got %v", err)
	}
}

// TestReplyPermission_404ChunkedGone 分块到达的合法 PermissionNotFoundError 404
//（task-permission-mode F1）：404 分类 MUST 基于完整读取的响应体——单次 Read 会把
// 第一段 `{"_tag":` 当完整 body，json 解析失败误判为路由 404（错误标记 unsupported，
// 永久停止后续自动判定）。分块下 MUST 仍返回 ErrPermissionRequestGone。
func TestReplyPermission_404ChunkedGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"_tag":`)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `"PermissionNotFoundError"}`)
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	err := c.ReplyPermission(context.Background(), "/wt", "perm-1", "once")
	if !errors.Is(err, ErrPermissionRequestGone) {
		t.Fatalf("chunked PermissionNotFoundError MUST classify as Gone, got %v", err)
	}
	if errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatal("chunked Gone MUST NOT be misclassified as route-404 (unsupported)")
	}
}

// twoPartBody 确定性两段式 Body：第一次 Read 恰好返回第一段，下一次 Read 返回第二段，
// 之后 EOF（不受 client bufio 合并影响——回退为单次读取的旧实现必然只见到第一段）。
type twoPartBody struct {
	parts [][]byte
	idx   int
}

func (b *twoPartBody) Read(p []byte) (int, error) {
	if b.idx >= len(b.parts) {
		return 0, io.EOF
	}
	part := b.parts[b.idx]
	b.idx++
	n := copy(p, part)
	return n, nil
}

// stubNotFoundTripper 恒返回 404 + 指定 Body 的 RoundTripper（绕过真实网络，
// 确定性控制读取边界）。
type stubNotFoundTripper struct{ body io.ReadCloser }

func (t *stubNotFoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       t.body,
	}, nil
}

// TestReplyPermission_404TwoPartBody_DeterministicGone 确定性分块 404 回归（F2）：
// Body 第一次 Read 只给 `{"_tag":`、后续 Read 给剩余+EOF。完整读取实现 →
// ErrPermissionRequestGone；回退为单次读取（readBodyForError）的旧实现必然只见第一段、
// 解析失败误判路由 404 → 本测试变红（mutation 验证过）。
func TestReplyPermission_404TwoPartBody_DeterministicGone(t *testing.T) {
	c := &Client{
		baseURL:  "http://127.0.0.1:1", // 不发真实网络请求（RoundTripper 拦截）
		username: "opencode",
		password: "pw",
		httpClient: &http.Client{Transport: &stubNotFoundTripper{body: io.NopCloser(&twoPartBody{
			parts: [][]byte{[]byte(`{"_tag":`), []byte(`"PermissionNotFoundError"}`)},
		})}},
	}
	err := c.ReplyPermission(context.Background(), "/wt", "perm-1", "once")
	if !errors.Is(err, ErrPermissionRequestGone) {
		t.Fatalf("two-part 404 body MUST classify as Gone, got %v", err)
	}
	if errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatal("two-part Gone MUST NOT be misclassified as route-404 (unsupported)")
	}
}

// TestReplyPermission_404BodyTimeout 404 headers 已达但 body 超时/截断 →
// 普通错误（结果未知，D6 分类 ②），MUST NOT 标记 unsupported。
func TestReplyPermission_404BodyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond) // body 迟迟不到
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.ReplyPermission(ctx, "/wt", "perm-1", "once")
	if err == nil {
		t.Fatal("expected error on truncated/timeout 404 body, got nil")
	}
	if errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatal("truncated 404 body MUST NOT classify as route-404 (unsupported)")
	}
	if errors.Is(err, ErrPermissionRequestGone) {
		t.Fatal("truncated 404 body MUST NOT classify as Gone")
	}
}

// TestReplyPermission_SuccessTrailingContent 成功响应含尾随第二 JSON 值或尾随垃圾 →
// 非法 JSON（结果未知普通错误），MUST NOT 返回成功（F2）。
func TestReplyPermission_SuccessTrailingContent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "尾随第二 JSON 值", body: `true false`},
		{name: "尾随垃圾", body: `true garbage`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, "pw")
			err := c.ReplyPermission(context.Background(), "/wt", "perm-1", "once")
			if err == nil {
				t.Fatal("trailing content after true MUST NOT be treated as success")
			}
			if errors.Is(err, ErrCapabilityUnsupported) || errors.Is(err, ErrPermissionRequestGone) {
				t.Fatalf("非法 JSON（结果未知）MUST NOT 映射为 sentinel，got %v", err)
			}
		})
	}
}
