// webhub_test.go WebHub 与通知 SSE 端点测试（task-notifications Lane D 4.1/4.7；
// spec「网页通知渠道」、design D7）：帧格式（event: notification + 单行 snake_case
// 七字段 JSON）、多连接广播、慢客户端断开、零连接 accepted=false、断线不重放。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/domain/notification"
)

// newNotificationTestServer 构造带 webHub 的测试 Server（通知路由始终注册）。
func newNotificationTestServer() *Server {
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		webHub:    newWebHub(),
	}
	s.registerRoutes()
	return s
}

// openNotificationsStream 发起已认证的通知 SSE 请求。
func openNotificationsStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("GET", url+"/api/v1/notifications/stream", ""))
	if err != nil {
		t.Fatalf("open notification stream: %v", err)
	}
	return resp
}

// notificationIntentFixture 标准测试意图（字段值用于帧断言）。
func notificationIntentFixture() notification.Intent {
	return notification.Intent{
		TaskID:   "task-42",
		TaskName: "构建服务",
		Category: notification.CategoryQuestion,
		Level:    notification.LevelTimeSensitive,
		Title:    "[构建服务] 等待你的回答",
		Body:     "构建服务\n用哪个分支？",
		URL:      "http://127.0.0.1:18080/#/task/task-42",
	}
}

// TestWebHub_FrameFormat 首个注释帧（建连确认）后 Publish 一条意图：帧为
// `event: notification` + 单行 data，JSON 为 snake_case 七字段（spec 唯一形状）。
func TestWebHub_FrameFormat(t *testing.T) {
	s := newNotificationTestServer()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openNotificationsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "connect comment") // 建连注释帧

	accepted := s.webHub.Publish(notificationIntentFixture())
	if !accepted {
		t.Fatal("publish with one connected frontend must be accepted")
	}
	f := nextFrame(t, frames, "notification frame")
	if f.event != "notification" {
		t.Fatalf("event = %q, want notification", f.event)
	}
	if strings.Contains(f.data, "\n") {
		t.Errorf("data must be single line, got %q", f.data)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatalf("data not valid JSON: %v", err)
	}
	want := map[string]string{
		"task_id":   "task-42",
		"task_name": "构建服务",
		"category":  "question",
		"level":     "timeSensitive",
		"title":     "[构建服务] 等待你的回答",
		"body":      "构建服务\n用哪个分支？",
		"url":       "http://127.0.0.1:18080/#/task/task-42",
	}
	if len(got) != len(want) {
		t.Fatalf("frame fields = %v, want exactly %d fields", got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("frame[%q] = %v, want %q", k, got[k], v)
		}
	}
}

// TestWebHub_MultiConnectionBroadcast 多连接广播：两个连接各收到同一意图帧。
func TestWebHub_MultiConnectionBroadcast(t *testing.T) {
	s := newNotificationTestServer()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp1 := openNotificationsStream(t, ts.URL)
	defer resp1.Body.Close()
	resp2 := openNotificationsStream(t, ts.URL)
	defer resp2.Body.Close()

	frames1 := startSSEFrameReader(resp1.Body)
	frames2 := startSSEFrameReader(resp2.Body)
	nextFrame(t, frames1, "connect 1")
	nextFrame(t, frames2, "connect 2")

	if !s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish must be accepted with two connected frontends")
	}
	f1 := nextFrame(t, frames1, "notification on conn 1")
	f2 := nextFrame(t, frames2, "notification on conn 2")
	if f1.data != f2.data {
		t.Errorf("both connections must receive identical frame, got %q vs %q", f1.data, f2.data)
	}
}

// TestWebHub_NoConnectionNotAccepted 零连接 → accepted=false（web 渠道投递失败
// 判定依据，spec「无连接前端计为该渠道失败」）。
func TestWebHub_NoConnectionNotAccepted(t *testing.T) {
	s := newNotificationTestServer()
	if s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish with zero connections must not be accepted")
	}
}

// TestWebHub_SlowClientDisconnectedAndNotAccepted 慢客户端（hub 层确定性测试）：
// 不消费的连接缓冲填满（16）后判定断开并从注册表移除，本次 Publish 不得计入
// accepted；移除后零连接态。
func TestWebHub_SlowClientDisconnectedAndNotAccepted(t *testing.T) {
	s := newNotificationTestServer()
	c := s.webHub.register()
	for i := 0; i < webHubBufferPerConn; i++ {
		if !s.webHub.Publish(notificationIntentFixture()) {
			t.Fatalf("publish %d must be accepted while buffer not full", i)
		}
	}
	// 第 17 条：缓冲满 → 慢客户端断开、accepted=false。
	if s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish on full buffer must not be accepted")
	}
	// 注册表已清理：零连接态。
	if s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish after slow client removal must not be accepted (registry empty)")
	}
	// 独立取消信号关闭：handler 必须被唤醒退出（不得依赖排空缓冲）。
	select {
	case <-c.done:
	default:
		t.Fatal("done must be closed after slow client disconnection")
	}
}

// deadlineSSEWriter 支持 SetWriteDeadline：首帧立即成功，后续 Write 阻塞到
// deadline 后返回。用于断言同步写路径在 handler 返回前结束（不另起 goroutine、
// 不依赖 cleanup 解阻塞）。
type deadlineSSEWriter struct {
	header   http.Header
	mu       sync.Mutex
	n        int
	inFlight int
	deadline time.Time
}

func (w *deadlineSSEWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *deadlineSSEWriter) WriteHeader(int) {}
func (w *deadlineSSEWriter) Flush()          {}
func (w *deadlineSSEWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.mu.Unlock()
	return nil
}
func (w *deadlineSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.n++
	n := w.n
	w.inFlight++
	dl := w.deadline
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight--
		w.mu.Unlock()
	}()
	if n == 1 {
		return len(p), nil
	}
	d := time.Until(dl)
	if d < 0 {
		return 0, os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	<-timer.C
	return 0, os.ErrDeadlineExceeded
}

func (w *deadlineSSEWriter) inflight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

func (w *deadlineSSEWriter) writes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// noDeadlineWriter 不支持 SetWriteDeadline：写路径必须立即失败并关连接，
// 不得进入阻塞 Write。
type noDeadlineWriter struct {
	header http.Header
	n      int
}

func (w *noDeadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *noDeadlineWriter) WriteHeader(int) {}
func (w *noDeadlineWriter) Flush()          {}
func (w *noDeadlineWriter) Write(p []byte) (int, error) {
	w.n++
	return len(p), nil
}

func TestWebHub_WriteDeadlineUnsupportedFailsClosed(t *testing.T) {
	s := newNotificationTestServer()
	w := &noDeadlineWriter{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/stream", nil)
	s.handleNotificationsStream(w, req)
	if w.n != 0 {
		t.Fatalf("Write called %d times, want 0 when SetWriteDeadline unsupported", w.n)
	}
}

func TestWebHub_SlowClientBlockedWriteTerminatesHandler(t *testing.T) {
	s := newNotificationTestServer()
	s.webHubWriteTimeout = 40 * time.Millisecond
	bw := &deadlineSSEWriter{}

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/stream", nil)
		s.handleNotificationsStream(bw, req)
	}()
	waitFor(t, time.Second, "conn registered", func() bool {
		s.webHub.mu.Lock()
		defer s.webHub.mu.Unlock()
		return len(s.webHub.conns) == 1
	})
	waitFor(t, time.Second, "heartbeat written", func() bool { return bw.writes() >= 1 })
	if !s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish must be accepted")
	}
	waitFor(t, time.Second, "write blocked", func() bool { return bw.writes() >= 2 })

	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler must exit on write deadline (must not hang on blocked Write)")
	}
	if n := bw.inflight(); n != 0 {
		t.Fatalf("Write still in flight after handler return (%d); must not leak a write goroutine", n)
	}
}

// TestWebHub_SlowClientDoesNotAffectOthers 慢客户端断开不影响其他连接：慢连接
// 缓冲满被移除后，活跃 HTTP 连接继续接收后续投递（accepted 以活跃连接为准）。
func TestWebHub_SlowClientDoesNotAffectOthers(t *testing.T) {
	s := newNotificationTestServer()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openNotificationsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "connect comment")

	// 手动注册一个不消费的慢连接（不经 HTTP，确定性填满其缓冲）。
	_ = s.webHub.register()
	for i := 0; i < webHubBufferPerConn; i++ {
		if !s.webHub.Publish(notificationIntentFixture()) {
			t.Fatalf("publish %d must be accepted (active frontend draining)", i)
		}
	}
	// 等待活跃连接 handler 排空本轮缓冲（写入快于排空时避免活跃连接被误判慢）。
	for i := 0; i < webHubBufferPerConn; i++ {
		nextFrame(t, frames, "notification")
	}
	// 第 17 条：慢连接缓冲满被断开；活跃连接 enqueue 成功 → accepted=true。
	if !s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish must still be accepted by active frontend after slow client dropped")
	}
	// 活跃连接继续接收后续投递。
	if !s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish after slow client dropped must be accepted")
	}
	nextFrame(t, frames, "notification after slow client dropped")
}

// TestWebHub_NoReplayAfterReconnect 断线不重放：断开期间 Publish 的意图不出现在
// 重连后的新流（新连接只收到注册之后投递的意图）。
func TestWebHub_NoReplayAfterReconnect(t *testing.T) {
	s := newNotificationTestServer()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp1 := openNotificationsStream(t, ts.URL)
	frames1 := startSSEFrameReader(resp1.Body)
	nextFrame(t, frames1, "connect 1")
	resp1.Body.Close() // 断线（客户端关闭）
	waitFor(t, 2*time.Second, "hub registry drain", func() bool {
		s.webHub.mu.Lock()
		defer s.webHub.mu.Unlock()
		return len(s.webHub.conns) == 0
	})

	// 断线期间投递（零连接 → 失败）。
	if s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish while disconnected must not be accepted")
	}

	resp2 := openNotificationsStream(t, ts.URL)
	defer resp2.Body.Close()
	frames2 := startSSEFrameReader(resp2.Body)
	nextFrame(t, frames2, "connect 2")
	// 重连后不补发断线期间的通知。
	assertNoFrame(t, frames2, 300*time.Millisecond, "replayed notification")
	// 仅新投递到达。
	if !s.webHub.Publish(notificationIntentFixture()) {
		t.Fatal("publish after reconnect must be accepted")
	}
	if f := nextFrame(t, frames2, "new notification"); f.event != "notification" {
		t.Fatalf("event = %q, want notification", f.event)
	}
}

// TestWebHub_ConcurrentPublishNonBlocking 并发 Publish 与连接建立/断开无死锁、
// 无 panic（注册表锁与 channel 操作的竞态回归）。
func TestWebHub_ConcurrentPublishNonBlocking(t *testing.T) {
	s := newNotificationTestServer()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			s.webHub.Publish(notificationIntentFixture())
		}
	}()
	resp := openNotificationsStream(t, ts.URL)
	defer resp.Body.Close()
	<-done // Publish 全程非阻塞：500ms 内完成（不因慢连接卡死）
}
