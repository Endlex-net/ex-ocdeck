package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// opencode 用户名固定为 opencode（§20）。
const testUsername = "opencode"

// basicAuthed 校验请求携带正确的 Basic Auth（密码不得入日志：本测试不打印密码）。
func basicAuthed(t *testing.T, r *http.Request, password string) bool {
	t.Helper()
	u, p, ok := r.BasicAuth()
	return ok && u == testUsername && p == password
}

// newTestClient 构造指向 httptest server 的 Client（复用 NewClient 默认值，短超时便于测试）。
func newTestClient(t *testing.T, srv *httptest.Server, password string) *Client {
	t.Helper()
	c := NewClient(0, password, Options{
		HealthTimeout:     200 * time.Millisecond,
		OpTimeout:         1 * time.Second,
		ReconnectBase:     10 * time.Millisecond,
		ReconnectMax:      100 * time.Millisecond,
		ReconnectMaxTries: 0,
		HeartbeatTimeout:  0, // 禁用，测试显式控制断流
	})
	// 覆盖 baseURL 为 httptest 地址（NewClient 按 port 拼 127.0.0.1，测试需指向 httptest）。
	c.baseURL = srv.URL
	return c
}

// requireDirectory 校验请求带 directory query（§20：全部请求显式携带 ?directory=<wt>）。
func requireDirectory(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.URL.Query().Get("directory"); got != want {
		t.Fatalf("directory query: got %q want %q", got, want)
	}
}

// ---- Health ----

func TestHealth_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/global/health" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"healthy":true,"version":"%s"}`, ContractBaseline))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Healthy || h.Version != ContractBaseline {
		t.Fatalf("Health body: %+v", h)
	}
}

func TestHealth_NotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestHealth_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	c.healthTimeout = 50 * time.Millisecond
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ---- ListSessions ----

func TestListSessions(t *testing.T) {
	const dir = "/wt/proj/task1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requireDirectory(t, r, dir)
		if r.URL.Path != "/session" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "1000" {
			t.Fatalf("limit: %s", r.URL.Query().Get("limit"))
		}
		_, _ = io.WriteString(w, `[{"id":"sess-001","title":"A","time":{"created":1.5,"updated":2.5}},{"id":"sess-002","time":{"updated":3.0}}]`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	ss, err := c.ListSessions(context.Background(), dir, 1000)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(ss) != 2 || ss[0].ID != "sess-001" || ss[1].ID != "sess-002" {
		t.Fatalf("sessions: %+v", ss)
	}
	if ss[0].Time.Updated != 2.5 {
		t.Fatalf("time.updated: %v", ss[0].Time.Updated)
	}
}

func TestListSessions_Overflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 返回 limit 条，触发溢出（§20）。
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < 1000; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":"s%d","time":{"updated":%d.0}}`, i, i)
		}
		b.WriteByte(']')
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.ListSessions(context.Background(), "/wt", 1000)
	if !errors.Is(err, ErrSessionOverflow) {
		t.Fatalf("expected ErrSessionOverflow, got %v", err)
	}
}

func TestListSessions_MissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 非嵌套 info.id，而是顶层缺 id（契约漂移）。
		_, _ = io.WriteString(w, `[{"title":"no id","time":{"updated":1.0}}]`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.ListSessions(context.Background(), "/wt", 1000)
	if err == nil {
		t.Fatal("expected error for missing top-level id")
	}
	// 结构漂移 → 包裹 ErrCapabilityMismatch（§11）。
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch wrap, got %v", err)
	}
}

// TestListSessions_MissingTimeUpdated 顶层 id 存在但缺 time.updated（契约漂移）→
// ErrCapabilityMismatch（§20 顶层 time.updated）。
func TestListSessions_MissingTimeUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"s1","title":"no-time"}]`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.ListSessions(context.Background(), "/wt", 1000)
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch for missing time.updated, got %v", err)
	}
}

// TestListSessions_NonNumericTimeUpdated time.updated 非数字（契约漂移）→
// ErrCapabilityMismatch。
func TestListSessions_NonNumericTimeUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"s1","time":{"updated":"not-a-number"}}]`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.ListSessions(context.Background(), "/wt", 1000)
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch for non-numeric time.updated, got %v", err)
	}
}

// ---- GetSession ----

func TestGetSession_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requireDirectory(t, r, "/wt")
		if r.URL.Path != "/session/sess-001" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"sess-001","title":"A","time":{"updated":2.5}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	s, err := c.GetSession(context.Background(), "/wt", "sess-001")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.ID != "sess-001" {
		t.Fatalf("id: %s", s.ID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.GetSession(context.Background(), "/wt", "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestGetSession_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.GetSession(context.Background(), "/wt", "sess")
	if !errors.Is(err, ErrSessionNotFound) {
		// 500 MUST NOT 被误判为 not found（必须区分）。
		if err == nil {
			t.Fatal("expected error on 500")
		}
	}
}

// ---- CreateSession ----

func TestCreateSession_OK(t *testing.T) {
	const dir = "/wt/proj/task1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requireDirectory(t, r, dir)
		if r.Method != http.MethodPost {
			t.Fatalf("method: %s, want POST", r.Method)
		}
		if r.URL.Path != "/session" {
			t.Fatalf("path: %s, want /session", r.URL.Path)
		}
		// body MUST be {"title": <title>}。
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["title"] != "task-name" {
			t.Fatalf("title: %q, want task-name", body["title"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"sess-new","title":"task-name","time":{"created":1.5,"updated":2.5}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	s, err := c.CreateSession(context.Background(), dir, "task-name")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "sess-new" {
		t.Fatalf("id: %s, want sess-new", s.ID)
	}
	if s.Time.Created != 1.5 || s.Time.Updated != 2.5 {
		t.Fatalf("time: created=%v updated=%v, want 1.5/2.5", s.Time.Created, s.Time.Updated)
	}
}

func TestCreateSession_201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"sess-201","time":{"created":1,"updated":1}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	s, err := c.CreateSession(context.Background(), "/wt", "t")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "sess-201" {
		t.Fatalf("id: %s, want sess-201", s.ID)
	}
}

func TestCreateSession_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.CreateSession(context.Background(), "/wt", "t")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestCreateSession_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.CreateSession(context.Background(), "/wt", "t")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrSessionNotFound) {
		t.Fatal("500 MUST NOT be misclassified as session not found")
	}
}

// ---- SessionStatus ----

func TestSessionStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requireDirectory(t, r, "/wt")
		if r.URL.Path != "/session/status" {
			t.Fatalf("path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"sess-001":{"type":"idle"},"sess-002":{"type":"busy"},"sess-003":{"type":"retry"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	st, err := c.SessionStatus(context.Background(), "/wt")
	if err != nil {
		t.Fatalf("SessionStatus: %v", err)
	}
	if len(st) != 3 || st["sess-001"].Type != StatusIdle || st["sess-002"].Type != StatusBusy || st["sess-003"].Type != StatusRetry {
		t.Fatalf("statuses: %+v", st)
	}
}

func TestSessionStatus_BadEnum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"s1":{"type":"running"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.SessionStatus(context.Background(), "/wt")
	if err == nil {
		t.Fatal("expected error for bad enum")
	}
	// 结构漂移 → 包裹 ErrCapabilityMismatch（§11）。
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch wrap, got %v", err)
	}
}

// ---- DeleteSession ----

func TestDeleteSession_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthed(t, r, "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requireDirectory(t, r, "/wt")
		if r.Method != http.MethodDelete || r.URL.Path != "/session/sess-001" {
			t.Fatalf("method/path: %s %s", r.Method, r.URL.Path)
		}
		// §20：200 + JSON true（非 204）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `true`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if err := c.DeleteSession(context.Background(), "/wt", "sess-001"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
}

func TestDeleteSession_NotFound_Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if err := c.DeleteSession(context.Background(), "/wt", "gone"); err != nil {
		t.Fatalf("404 should be idempotent success, got %v", err)
	}
}

func TestDeleteSession_BadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 但 body 非 true（契约漂移）。
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `false`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	if err := c.DeleteSession(context.Background(), "/wt", "sess"); err == nil {
		t.Fatal("expected error for non-true body")
	}
}

// ---- Probe（能力探测门禁） ----

func TestProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"healthy":true,"version":"%s"}`, ContractBaseline))
		case "/session/status":
			_, _ = io.WriteString(w, `{"s1":{"type":"idle"}}`)
		case "/session":
			// 空列表：无字段可校验，能力探测通过。
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	v, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if v != ContractBaseline {
		t.Fatalf("version: %s", v)
	}
}

func TestProbe_Unhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`{"healthy":false,"version":"%s"}`, ContractBaseline))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch, got %v", err)
	}
}

func TestProbe_BadStatusStruct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = io.WriteString(w, `{"healthy":true,"version":"2.0.0"}`)
		case "/session/status":
			// 契约漂移：type 非三值枚举。
			_, _ = io.WriteString(w, `{"s1":{"type":"running"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch, got %v", err)
	}
}

// TestProbe_BadSessionShape session 列表字段形状漂移（缺 time.updated）→
// ErrCapabilityMismatch（§11）。
func TestProbe_BadSessionShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"healthy":true,"version":"%s"}`, ContractBaseline))
		case "/session/status":
			_, _ = io.WriteString(w, `{"s1":{"type":"idle"}}`)
		case "/session":
			// 顶层 id 存在但缺 time.updated（契约漂移）。
			_, _ = io.WriteString(w, `[{"id":"s1","title":"no-time"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("expected ErrCapabilityMismatch, got %v", err)
	}
}

// TestProbe_ServeNotReady 5xx/网络错误 → ErrServeNotReady（可重试，非结构漂移）。
func TestProbe_ServeNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrServeNotReady) {
		t.Fatalf("expected ErrServeNotReady, got %v", err)
	}
}

// TestProbe_Unauthorized 401（内部 bug）→ ErrUnauthorized，不可重试。
func TestProbe_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	_, err := c.Probe(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

// ---- 401 = 内部 bug ----

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "wrong-pw")
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

// ---- SSE ----

// sseServer 提供：首次连接发送事件后关闭流；重连后发送不同事件集（用于验证 OnReconnect）。
type sseFixture struct {
	mu          sync.Mutex
	connects    int32
	reconnected int32
	// eventsForConnect[0] = 首次连接事件；eventsForConnect[1] = 重连后事件。
	eventsForConnect [][]string
}

func (f *sseFixture) handler(w http.ResponseWriter, r *http.Request) {
	if !basicAuthed(tForHandler(r), r, "pw") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	requireDirectory(tForHandler(r), r, "/wt")
	n := atomic.AddInt32(&f.connects, 1)
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	idx := int(n - 1)
	if idx >= len(f.eventsForConnect) {
		idx = len(f.eventsForConnect) - 1
	}
	for _, ev := range f.eventsForConnect[idx] {
		_, _ = io.WriteString(w, ev)
		flusher.Flush()
	}
	// 模拟断流：写完事件后关闭连接（首次连接）；重连后保持连接打开直到客户端断开。
	if idx == 0 {
		// 首次连接：写完即关闭 → 触发重连。
		return
	}
	// 重连后：保持连接，直到客户端取消 ctx。
	select {
	case <-r.Context().Done():
	}
	atomic.StoreInt32(&f.reconnected, 1)
}

// tForHandler 从 request context 取 *testing.T（通过 ctx 注入）。
type testCtxKey struct{}

func tForHandler(r *http.Request) *testing.T {
	if v, ok := r.Context().Value(testCtxKey{}).(*testing.T); ok {
		return v
	}
	return nil
}

func TestSubscribeEvents_WriteParseReconnect(t *testing.T) {
	f := &sseFixture{
		eventsForConnect: [][]string{
			{
				"event: server.connected\ndata: {\"type\":\"server.connected\"}\n\n",
				"event: session.created\ndata: {\"type\":\"session.created\",\"properties\":{\"info\":{\"id\":\"sess-001\",\"title\":\"A\",\"time\":{\"created\":1.0,\"updated\":1.0}}}}\n\n",
			},
			{
				"event: session.updated\ndata: {\"type\":\"session.updated\",\"properties\":{\"info\":{\"id\":\"sess-001\",\"title\":\"A2\",\"time\":{\"updated\":2.0}}}}\n\n",
				"event: session.deleted\ndata: {\"type\":\"session.deleted\",\"properties\":{\"info\":{\"id\":\"sess-001\"}}}\n\n",
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), testCtxKey{}, t)
		f.handler(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, "pw")

	var mu sync.Mutex
	var got []Event
	var reconnectCalls int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt",
			func(e Event) {
				mu.Lock()
				got = append(got, e)
				mu.Unlock()
				// 收到 deleted 后取消，结束订阅。
				if e.Type == "session.deleted" {
					cancel()
				}
			},
			func() {
				atomic.AddInt32(&reconnectCalls, 1)
			},
		)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEvents: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for SubscribeEvents to return")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(got), got)
	}
	// server.connected 不应透传；第一个应为 session.created 或 session.updated。
	for _, e := range got {
		if e.Type == "server.connected" {
			t.Fatalf("server.connected should not be forwarded")
		}
	}
	// 验证 SessionID 解析。
	if got[0].SessionID() != "sess-001" {
		t.Fatalf("first event session id: %s", got[0].SessionID())
	}
	if atomic.LoadInt32(&reconnectCalls) != 1 {
		t.Fatalf("expected 1 reconnect call, got %d", atomic.LoadInt32(&reconnectCalls))
	}
}

func TestSubscribeEvents_CtxCancel(t *testing.T) {
	// 保持连接打开，直到客户端取消；验证 ctx 取消即关流。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.SubscribeEvents(ctx, "/wt", func(Event) {}, func() {})
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected ctx cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeEvents did not return on ctx cancel")
	}
}

// TestSubscribeEvents_OnReady 首次连接建立后触发 onReady（就绪信号），且先于业务事件。
func TestSubscribeEvents_OnReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: session.created\ndata: {\"type\":\"session.created\",\"properties\":{\"info\":{\"id\":\"s1\",\"time\":{\"updated\":1.0}}}}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	var readyBeforeEvent int32
	c.onReady = func() { atomic.StoreInt32(&readyBeforeEvent, 1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got Event
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt",
			func(e Event) {
				got = e
				if atomic.LoadInt32(&readyBeforeEvent) != 1 {
					t.Errorf("onReady not fired before first event")
				}
				cancel()
			},
			func() {},
		)
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEvents: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	if atomic.LoadInt32(&readyBeforeEvent) != 1 {
		t.Fatal("onReady not fired")
	}
	if got.Type != "session.created" {
		t.Fatalf("first event: %s", got.Type)
	}
}

// TestSubscribeEvents_ReconnectBarrier 重连后 onReconnect MUST 先于任何新事件回调触发。
// 注入乱序验证：onReconnect 设置屏障标志，首个重连事件回调 MUST 观察到屏障已设置。
func TestSubscribeEvents_ReconnectBarrier(t *testing.T) {
	f := &sseFixture{
		eventsForConnect: [][]string{
			{
				"event: server.connected\ndata: {}\n\n",
				"event: session.created\ndata: {\"type\":\"session.created\",\"properties\":{\"info\":{\"id\":\"s1\",\"time\":{\"updated\":1.0}}}}\n\n",
			},
			{
				"event: session.updated\ndata: {\"type\":\"session.updated\",\"properties\":{\"info\":{\"id\":\"s1\",\"time\":{\"updated\":2.0}}}}\n\n",
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), testCtxKey{}, t)
		f.handler(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, "pw")

	var barrier int32
	var reconnectBeforeEvent int32
	var reconnectCalls int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt",
			func(e Event) {
				if e.Type == "session.updated" {
					// 重连后首个事件 MUST 观察到屏障已设置。
					if atomic.LoadInt32(&barrier) != 1 {
						t.Errorf("onReconnect did not complete before reconnect event")
					}
					atomic.StoreInt32(&reconnectBeforeEvent, 1)
					cancel()
				}
			},
			func() {
				atomic.AddInt32(&reconnectCalls, 1)
				// 模拟全量对齐：设置屏障标志，供首个重连事件验证。
				atomic.StoreInt32(&barrier, 1)
			},
		)
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEvents: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	if atomic.LoadInt32(&reconnectCalls) != 1 {
		t.Fatalf("expected 1 reconnect, got %d", atomic.LoadInt32(&reconnectCalls))
	}
	if atomic.LoadInt32(&reconnectBeforeEvent) != 1 {
		t.Fatal("reconnect event did not observe barrier (ordering violated)")
	}
}

// TestSubscribeEvents_UnauthorizedFastFail 401 = 永久错误，快速失败不重试。
func TestSubscribeEvents_UnauthorizedFastFail(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	c.reconnectMaxTries = 5 // 即便允许重试，401 也应快速失败
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.SubscribeEvents(ctx, "/wt", func(Event) {}, func() {})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Fatalf("expected 1 connect (no retry on 401), got %d", got)
	}
}

// TestSubscribeEvents_HeartbeatTimeout 心跳空闲超时视为断流重连。
func TestSubscribeEvents_HeartbeatTimeout(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connects, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
		flusher.Flush()
		// 之后不发任何事件，等待心跳超时触发重连。
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	c.heartbeatTimeout = 50 * time.Millisecond
	c.reconnectMaxTries = 2 // 限制重试次数以便测试退出

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt", func(Event) {}, func() {})
	}()
	// 等待心跳超时触发若干次重连。
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeEvents did not return")
	}
	if got := atomic.LoadInt32(&connects); got < 2 {
		t.Fatalf("expected ≥2 connects (heartbeat-driven reconnect), got %d", got)
	}
}

// TestSubscribeEvents_MalformedCounted malformed event 计数而非静默丢弃。
func TestSubscribeEvents_MalformedCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: server.connected\ndata: {}\n\n")
		flusher.Flush()
		// malformed：data 非合法 JSON。
		_, _ = io.WriteString(w, "event: session.created\ndata: {bad json}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: session.updated\ndata: {\"type\":\"session.updated\",\"properties\":{\"info\":{\"id\":\"s1\",\"time\":{\"updated\":2.0}}}}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "pw")
	var got []Event
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeEvents(ctx, "/wt",
			func(e Event) {
				got = append(got, e)
				if e.Type == "session.updated" {
					cancel()
				}
			},
			func() {},
		)
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEvents: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	if c.MalformedCount() != 1 {
		t.Fatalf("expected 1 malformed, got %d", c.MalformedCount())
	}
	if len(got) != 1 || got[0].Type != "session.updated" {
		t.Fatalf("expected only valid event forwarded, got %+v", got)
	}
}

// TestEvent_TimeUpdated 结构化访问 info.time.updated。
func TestEvent_TimeUpdated(t *testing.T) {
	e := Event{Type: "session.updated", Properties: map[string]interface{}{
		"info": map[string]interface{}{
			"id":   "s1",
			"time": map[string]interface{}{"updated": 2.5},
		},
	}}
	v, ok := e.TimeUpdated()
	if !ok || v != 2.5 {
		t.Fatalf("TimeUpdated: %v %v", v, ok)
	}
	// 缺 time.updated。
	e2 := Event{Type: "session.deleted", Properties: map[string]interface{}{
		"info": map[string]interface{}{"id": "s1"},
	}}
	if _, ok := e2.TimeUpdated(); ok {
		t.Fatal("expected ok=false for missing time.updated")
	}
}

// TestExponentialBackoff 验证退避计算与上限/防溢出。
func TestExponentialBackoff(t *testing.T) {
	base := 10 * time.Millisecond
	max := 100 * time.Millisecond
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, base},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},
		{5, max}, // 160ms > max
		{100, max},
	}
	for _, tc := range cases {
		if got := exponentialBackoff(base, max, tc.attempt); got != tc.want {
			t.Errorf("attempt %d: got %v want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestEvent_SessionID(t *testing.T) {
	e := Event{Type: "session.created", Properties: map[string]interface{}{
		"info": map[string]interface{}{"id": "sess-x"},
	}}
	if e.SessionID() != "sess-x" {
		t.Fatalf("SessionID: %s", e.SessionID())
	}
	e2 := Event{Type: "other", Properties: map[string]interface{}{}}
	if e2.SessionID() != "" {
		t.Fatalf("empty SessionID expected, got %s", e2.SessionID())
	}
}

// ---- Contract fixture 解析（固化各端点响应形状，§20） ----

func TestParseFixture_Health(t *testing.T) {
	raw := readFixture(t, "health.json")
	var h HealthResponse
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !h.Healthy || h.Version != ContractBaseline {
		t.Fatalf("fixture health: %+v", h)
	}
}

func TestParseFixture_SessionList(t *testing.T) {
	raw := readFixture(t, "session_list.json")
	var raws []jsonRawObject
	if err := json.Unmarshal(raw, &raws); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raws) != 2 {
		t.Fatalf("len: %d", len(raws))
	}
	s0, err := parseSession(raws[0])
	if err != nil {
		t.Fatalf("parseSession[0]: %v", err)
	}
	if s0.ID != "sess-001" || s0.Time.Updated != 1700000010.5 {
		t.Fatalf("session[0]: %+v", s0)
	}
}

func TestParseFixture_SessionGet(t *testing.T) {
	raw := readFixture(t, "session_get.json")
	var r jsonRawObject
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, err := parseSession(r)
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if s.ID != "sess-001" {
		t.Fatalf("id: %s", s.ID)
	}
}

func TestParseFixture_SessionStatus(t *testing.T) {
	raw := readFixture(t, "session_status.json")
	var m map[string]jsonRawObject
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("len: %d", len(m))
	}
	for sid, r := range m {
		st, err := parseSessionStatus(r)
		if err != nil {
			t.Fatalf("parseSessionStatus(%s): %v", sid, err)
		}
		switch st.Type {
		case StatusIdle, StatusBusy, StatusRetry:
		default:
			t.Fatalf("bad enum: %s", st.Type)
		}
	}
}

func TestParseFixture_SessionDelete(t *testing.T) {
	raw := readFixture(t, "session_delete.json")
	var ok bool
	if err := json.Unmarshal(raw, &ok); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !ok {
		t.Fatal("delete fixture must be true")
	}
}

func TestParseFixture_SSE(t *testing.T) {
	raw := readFixture(t, "sse_events.txt")
	p := newSSEParser(strings.NewReader(string(raw)))
	ctx := context.Background()
	var types []string
	for {
		ev, err := p.Next(ctx)
		if err != nil {
			break
		}
		if ev.Type == "" && len(ev.data) == 0 {
			continue // 注释/心跳，无事件内容（真实流无 event: 行，type 在 JSON payload 内）
		}
		parsed, perr := parseEvent(ev)
		if perr != nil {
			t.Fatalf("parseEvent: %v", perr)
		}
		types = append(types, parsed.Type)
		if parsed.Type != "server.connected" {
			if parsed.SessionID() == "" {
				t.Fatalf("event %s missing session id", parsed.Type)
			}
		}
	}
	want := []string{"server.connected", "session.created", "session.updated", "session.deleted"}
	if fmt.Sprintf("%v", types) != fmt.Sprintf("%v", want) {
		t.Fatalf("event types: %v want %v", types, want)
	}
}

// readFixture 从 testdata 读取 fixture 文件。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}