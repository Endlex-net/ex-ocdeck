// sessions_stream_test.go SSE 端点测试（sse-active-sessions P2.6；design.md D3）：
// 建连状态机（先订阅再组装、初始组装失败 500、组装期间变更补 update）、推送语义
// （500ms 合并窗口、组装失败保持 dirty 重试、Overflow 窗口外重推、心跳注释行）、
// 统一写路径（任何 Write/Flush 失败立即退订退出）、401/断连/进程 ctx 取消与
// 推送路径纯读断言。
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/opencode"
)

// --- fake EventSubscriber：内存可控订阅器（per-topic 事件/溢出 channel） ---

// fakeStreamSubscriber 内存 EventSubscriber：Subscribe 按 topic 建独立事件/溢出
// channel，测试随时 publish 事件或触发溢出；liveSubs 观测退订（断连/退出后归零）。
type fakeStreamSubscriber struct {
	mu   sync.Mutex
	subs []*fakeStreamSub
}

func (f *fakeStreamSubscriber) Subscribe(topic ocdeckevent.Topic) EventSubscription {
	s := &fakeStreamSub{
		topic:    topic,
		events:   make(chan ocdeckevent.Event, 64),
		overflow: make(chan struct{}, 1),
	}
	f.mu.Lock()
	f.subs = append(f.subs, s)
	f.mu.Unlock()
	return s
}

// liveSubs 返回未 Close 的订阅数。
func (f *fakeStreamSubscriber) liveSubs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.subs {
		if !s.isClosed() {
			n++
		}
	}
	return n
}

// publish 向该 topic 的全部存活订阅非阻塞投递事件（缓冲满丢弃，模仿 bus）。
func (f *fakeStreamSubscriber) publish(ev ocdeckevent.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		if s.topic != ev.Topic || s.isClosed() {
			continue
		}
		select {
		case s.events <- ev:
		default:
		}
	}
}

// triggerOverflow 置位该 topic 全部存活订阅的溢出信号。
func (f *fakeStreamSubscriber) triggerOverflow(topic ocdeckevent.Topic) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		if s.topic != topic || s.isClosed() {
			continue
		}
		select {
		case s.overflow <- struct{}{}:
		default:
		}
	}
}

type fakeStreamSub struct {
	topic    ocdeckevent.Topic
	events   chan ocdeckevent.Event
	overflow chan struct{}
	closeMu  sync.Mutex
	closed   bool
}

func (s *fakeStreamSub) C() <-chan ocdeckevent.Event { return s.events }
func (s *fakeStreamSub) Overflow() <-chan struct{}   { return s.overflow }

func (s *fakeStreamSub) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

func (s *fakeStreamSub) isClosed() bool {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closed
}

// --- 测试 backend：可编程 overview + 组装计数 + 写方法调用记录 ---

// streamBackend SSE 测试 backend：overview 行可经 overviewFn 动态返回、failNext
// 控制接下来 N 次组装失败；记录组装次数、全部写方法调用与实时探测调用（断言推送
// 路径无 opencode 调用、无写副作用）。
type streamBackend struct {
	*fakeTaskBackend
	rows                []application.ActiveTaskOverviewRow
	overviewFn          func(ctx context.Context) []application.ActiveTaskOverviewRow
	agentStatusSnapshot map[string]string
	attentions          map[string]application.Attention

	mu               sync.Mutex
	assemblies       int
	failNext         int
	writes           []string
	agentStatusCalls []string
}

func newStreamBackend(rows ...application.ActiveTaskOverviewRow) *streamBackend {
	return &streamBackend{fakeTaskBackend: &fakeTaskBackend{}, rows: rows}
}

func (b *streamBackend) ListActiveTaskOverview(ctx context.Context) ([]application.ActiveTaskOverviewRow, error) {
	b.mu.Lock()
	b.assemblies++
	fail := b.failNext > 0
	if fail {
		b.failNext--
	}
	fn, rows := b.overviewFn, b.rows
	b.mu.Unlock()
	if fail {
		return nil, errStoreFailure
	}
	if fn != nil {
		return fn(ctx), nil
	}
	return rows, nil
}

// setFailNext 让接下来 N 次组装返回 store 错误（模拟重组装失败，dirty 保持）。
func (b *streamBackend) setFailNext(n int) {
	b.mu.Lock()
	b.failNext = n
	b.mu.Unlock()
}

func (b *streamBackend) assemblyCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.assemblies
}

// AgentStatus 实时探测：推送路径 MUST NOT 调用（opencode 探测代理断言点）。
func (b *streamBackend) AgentStatus(ctx context.Context, taskID string) string {
	b.mu.Lock()
	b.agentStatusCalls = append(b.agentStatusCalls, taskID)
	b.mu.Unlock()
	return ""
}

func (b *streamBackend) AgentStatusSnapshot(taskID string) string {
	return b.agentStatusSnapshot[taskID]
}

func (b *streamBackend) Attention(taskID string) (application.Attention, bool) {
	if att, ok := b.attentions[taskID]; ok {
		return att, true
	}
	return application.Attention{Permissions: []application.PendingPermission{}, Questions: []application.PendingQuestion{}}, false
}

// 写方法全量记录（推送路径无写断言）。
func (b *streamBackend) recordWrite(name string) {
	b.mu.Lock()
	b.writes = append(b.writes, name)
	b.mu.Unlock()
}

func (b *streamBackend) writeCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.writes...)
}

func (b *streamBackend) agentStatusCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.agentStatusCalls)
}

func (b *streamBackend) Create(ctx context.Context, projectID, taskName, baseRef string) (application.TaskRow, error) {
	b.recordWrite("Create")
	return b.fakeTaskBackend.Create(ctx, projectID, taskName, baseRef)
}
func (b *streamBackend) Activate(ctx context.Context, taskID string) error {
	b.recordWrite("Activate")
	return b.fakeTaskBackend.Activate(ctx, taskID)
}
func (b *streamBackend) Suspend(ctx context.Context, taskID string) error {
	b.recordWrite("Suspend")
	return b.fakeTaskBackend.Suspend(ctx, taskID)
}
func (b *streamBackend) Archive(ctx context.Context, taskID string) error {
	b.recordWrite("Archive")
	return b.fakeTaskBackend.Archive(ctx, taskID)
}
func (b *streamBackend) Restore(ctx context.Context, taskID string) error {
	b.recordWrite("Restore")
	return b.fakeTaskBackend.Restore(ctx, taskID)
}
func (b *streamBackend) Delete(ctx context.Context, taskID string, mode application.DeleteMode, confirmDirty bool) error {
	b.recordWrite("Delete")
	return b.fakeTaskBackend.Delete(ctx, taskID, mode, confirmDirty)
}
func (b *streamBackend) Retry(ctx context.Context, taskID string, confirmDirty bool) error {
	b.recordWrite("Retry")
	return b.fakeTaskBackend.Retry(ctx, taskID, confirmDirty)
}
func (b *streamBackend) ReopenAttach(ctx context.Context, taskID string) (application.TerminalID, error) {
	b.recordWrite("ReopenAttach")
	return b.fakeTaskBackend.ReopenAttach(ctx, taskID)
}
func (b *streamBackend) CreateShell(ctx context.Context, taskID string) (application.TerminalID, error) {
	b.recordWrite("CreateShell")
	return b.fakeTaskBackend.CreateShell(ctx, taskID)
}
func (b *streamBackend) CloseShell(ctx context.Context, terminalID application.TerminalID) error {
	b.recordWrite("CloseShell")
	return b.fakeTaskBackend.CloseShell(ctx, terminalID)
}
func (b *streamBackend) RerunInit(ctx context.Context, taskID string) (application.TaskRow, error) {
	b.recordWrite("RerunInit")
	return b.fakeTaskBackend.RerunInit(ctx, taskID)
}
func (b *streamBackend) GitCommit(ctx context.Context, taskID, message string, paths []string) error {
	b.recordWrite("GitCommit")
	return b.fakeTaskBackend.GitCommit(ctx, taskID, message, paths)
}
func (b *streamBackend) GitPush(ctx context.Context, taskID string) error {
	b.recordWrite("GitPush")
	return b.fakeTaskBackend.GitPush(ctx, taskID)
}

// --- SSE 帧读取与同步辅助 ---

// sseFrame 解析后的 SSE 帧：event 为空表示注释行（心跳）。
type sseFrame struct {
	event string
	data  string
}

// startSSEFrameReader 后台按空行分帧读取 SSE 流（bufio 缓冲吸收跨 chunk 粘包），
// 帧推送到返回 channel；流结束（EOF/错误）时关闭 channel。
func startSSEFrameReader(r io.Reader) <-chan sseFrame {
	frames := make(chan sseFrame, 16)
	go func() {
		defer close(frames)
		br := bufio.NewReader(r)
		var cur sseFrame
		for {
			line, err := br.ReadString('\n')
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				if cur.event != "" || cur.data != "" {
					frames <- cur
				}
				cur = sseFrame{}
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ":"):
				cur.data = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

// nextFrame 限时等待下一帧（默认 2s，可覆盖），超时/流关闭 fatal。
func nextFrame(t *testing.T, frames <-chan sseFrame, what string) sseFrame {
	t.Helper()
	return nextFrameWithin(t, frames, 2*time.Second, what)
}

func nextFrameWithin(t *testing.T, frames <-chan sseFrame, timeout time.Duration, what string) sseFrame {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("stream closed while waiting for %s frame", what)
		}
		return f
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %s frame", what)
	}
	return sseFrame{}
}

// assertNoFrame 断言 timeout 内没有新帧（流意外关闭同样失败）。
func assertNoFrame(t *testing.T, frames <-chan sseFrame, timeout time.Duration, what string) {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("stream closed unexpectedly while expecting no %s", what)
		}
		t.Fatalf("unexpected frame while expecting no %s: %+v", what, f)
	case <-time.After(timeout):
	}
}

// waitFor 轮询等待条件成立，超时 fatal。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// newStreamTestServer 构造注入 fake 订阅器与短间隔的测试 Server；SetEventSubscriber
// 后 RebuildRoutes 挂载 stream 路由（与生产 wiring 顺序一致）。
func newStreamTestServer(t *testing.T, tb TaskBackend, sub *fakeStreamSubscriber, coalesce, heartbeat time.Duration) *Server {
	t.Helper()
	s := newAPITestServer(t, tb)
	s.SetEventSubscriber(sub)
	s.sseCoalesce = coalesce
	s.sseHeartbeat = heartbeat
	s.RebuildRoutes()
	return s
}

// openActiveSessionsStream 发起已认证的 SSE 请求并返回响应。
func openActiveSessionsStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(authedReq("GET", url+"/api/v1/tasks/active/stream", ""))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	return resp
}

// --- 建连状态机 ---

// TestActiveSessionsStream_FirstFrameBareArraySnapshot 首帧为裸数组 snapshot，
// 无需等心跳立即可读；SSE headers 正确；帧 data 与 REST /tasks/active 响应
// 完全同构（attention.permissions/questions 子结构、agentStatus omitempty）。
func TestActiveSessionsStream_FirstFrameBareArraySnapshot(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 300),
		activeRow("t2", "p2", "projB", "taskB", "bB", "/wtB", 200),
	}
	tb := newStreamBackend(rows...)
	tb.agentStatusSnapshot = map[string]string{"t1": "busy"} // t2 无快照 → 省略
	tb.attentions = map[string]application.Attention{
		"t1": {
			Permissions: []application.PendingPermission{{
				PermissionRequest: opencode.PermissionRequest{ID: "perm1", Permission: "bash", Patterns: []string{"git *"}},
				Since:             100,
			}},
			Questions: []application.PendingQuestion{{
				QuestionRequest: opencode.QuestionRequest{ID: "q1", Questions: []opencode.QuestionItem{{Header: "h", Question: "go?"}}},
				Since:           101,
			}},
		},
	}
	sub := &fakeStreamSubscriber{}
	// 心跳 5s：断言首帧到达不依赖任何心跳/窗口触发。
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache, no-transform" {
		t.Errorf("cache-control = %q, want no-cache, no-transform", cc)
	}
	if xa := resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("x-accel-buffering = %q, want no", xa)
	}
	if conn := resp.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("connection = %q, want keep-alive", conn)
	}

	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame event = %q, want snapshot", snap.event)
	}
	if !strings.HasPrefix(snap.data, "[") {
		t.Fatalf("snapshot data not bare array: %s", snap.data)
	}
	// agentStatus：t1 出现、t2 omitempty 省略；attention 子结构透出。
	if !strings.Contains(snap.data, `"agentStatus":"busy"`) || strings.Count(snap.data, "agentStatus") != 1 {
		t.Errorf("snapshot agentStatus omitempty wrong: %s", snap.data)
	}
	if !strings.Contains(snap.data, `"permissions":[`) || !strings.Contains(snap.data, `"questions":[`) {
		t.Errorf("snapshot attention sub-structure missing: %s", snap.data)
	}

	// 与 REST 响应逐字段同构。
	restResp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer restResp.Body.Close()
	_, rest := readAndDecode(t, restResp.Body)
	sse := decodeActiveSessions(t, snap.data)
	if !reflect.DeepEqual(rest, sse) {
		t.Errorf("SSE snapshot %+v != REST %+v", sse, rest)
	}
	if n := sub.liveSubs(); n != 4 {
		t.Errorf("live subs = %d, want 4 while streaming", n)
	}
}

// TestActiveSessionsStream_SubscribeBeforeAssembleCatchUpUpdate 订阅先于组装：
// 组装期间（overviewFn 回调内）发布过滤表命中事件 → snapshot 后立即补一帧 update
// （不等合并窗口），消除"查询与订阅之间的变更永久漏掉"。
func TestActiveSessionsStream_SubscribeBeforeAssembleCatchUpUpdate(t *testing.T) {
	var subsAtAssembly atomic.Int32
	sub := &fakeStreamSubscriber{}
	tb := newStreamBackend()
	gen := 0
	tb.overviewFn = func(ctx context.Context) []application.ActiveTaskOverviewRow {
		gen++
		if gen == 1 {
			// 组装时刻断言四路订阅已建立（subscribe-before-assemble）。
			subsAtAssembly.Store(int32(sub.liveSubs()))
			// 组装期间发布 session.touched（过滤表命中）。
			sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
			return []application.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 200)}
		}
		return []application.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA-v2", "bA", "/wtA", 300)}
	}
	// 窗口 1s：catch-up update 必须远早于首个窗口 tick 到达。
	s := newStreamTestServer(t, tb, sub, 1*time.Second, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)

	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame event = %q, want snapshot", snap.event)
	}
	if !strings.Contains(snap.data, `"taskA"`) {
		t.Fatalf("snapshot should carry v1 row: %s", snap.data)
	}
	if n := subsAtAssembly.Load(); n != 4 {
		t.Errorf("subscriptions at initial assembly = %d, want 4 (subscribe before assemble)", n)
	}
	// catch-up update 立即到达（600ms 内，远小于 1s 窗口），携带重组装后的 v2 行。
	var upd sseFrame
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("stream closed waiting for catch-up update")
		}
		upd = f
	case <-time.After(600 * time.Millisecond):
		t.Fatal("catch-up update did not arrive before first coalesce window")
	}
	if upd.event != "update" {
		t.Fatalf("second frame event = %q, want update", upd.event)
	}
	if !strings.Contains(upd.data, `"taskA-v2"`) {
		t.Errorf("catch-up update should carry reassembled row: %s", upd.data)
	}
}

// TestActiveSessionsStream_InitialAssemblyFailureReturns500 初始组装失败：写 SSE
// headers 前退订全部订阅并返回 500 JSON 错误信封（无 text/event-stream、无悬挂连接）。
func TestActiveSessionsStream_InitialAssemblyFailureReturns500(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	tb.setFailNext(1)
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (no SSE headers)", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeInternal)
	}
	// 四路订阅全部关闭。
	waitFor(t, 2*time.Second, "subscriptions closed after 500", func() bool {
		return sub.liveSubs() == 0
	})
}

// --- 推送语义 ---

// TestActiveSessionsStream_DirtyEventProducesUpdate 过滤表命中事件（跨 topic：
// session.touched / serve_runtime.run_status_changed）→ 下一合并窗口内 update。
func TestActiveSessionsStream_DirtyEventProducesUpdate(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100)}
	tb := newStreamBackend(rows...)
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	upd := nextFrame(t, frames, "update after session.touched")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if got := decodeActiveSessions(t, upd.data); len(got) != 1 || got[0].TaskID != "t1" {
		t.Errorf("update data = %s, want 1-row bare array", upd.data)
	}

	// serve_runtime topic 同样经 fan-in 推送。
	sub.publish(ocdeckevent.NewServeRuntimeRunStatusChanged("iv-1", "t1", "idle", "busy", true))
	upd2 := nextFrame(t, frames, "update after serve_runtime.run_status_changed")
	if upd2.event != "update" {
		t.Fatalf("event = %q, want update", upd2.event)
	}
}

// TestActiveSessionsStream_NonDirtyEventsNeverProduceUpdate task.created 与两端
// 均非 active 的 task.status_changed（含中间态迁移）不产生任何 update 帧。
// （跨秒场景下生产侧保证此类迁移不伴随 task.activity_changed；此处直接注入事件
// 验证消费侧语义。）
func TestActiveSessionsStream_NonDirtyEventsNeverProduceUpdate(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 30*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewTaskCreated("t2"))
	sub.publish(ocdeckevent.NewTaskStatusChanged("t3", application.StatusSuspended, application.StatusArchived))
	sub.publish(ocdeckevent.NewTaskStatusChanged("t3", application.StatusArchived, application.StatusSuspended))
	// 200ms ≈ 6 个窗口：非命中事件后不得出现任何帧。
	assertNoFrame(t, frames, 200*time.Millisecond, "update after non-dirty events")
}

// TestActiveSessionsStream_CoalesceWindowSingleUpdate 合并窗口：一个窗口内多个
// 命中事件合并为恰好一帧 update。
func TestActiveSessionsStream_CoalesceWindowSingleUpdate(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 120*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	sub.publish(ocdeckevent.NewSessionClaimed("sess-2", "t1"))
	sub.publish(ocdeckevent.NewSessionDeleted("sess-3", "t1"))
	upd := nextFrame(t, frames, "coalesced update")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	// 后续约 3 个窗口内无第二帧 update。
	assertNoFrame(t, frames, 350*time.Millisecond, "second update after coalesce window")
	// 组装次数：初始 1 次 + 重推 1 次。
	waitFor(t, 2*time.Second, "assembly count 2", func() bool { return tb.assemblyCount() == 2 })
}

// TestActiveSessionsStream_UpdateAssemblyFailureKeepsDirtyRetries update 组装
// 失败：连接保留、dirty 保持，下一窗口 tick 重试成功后送达 update（无新事件）。
func TestActiveSessionsStream_UpdateAssemblyFailureKeepsDirtyRetries(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 60*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	// 首次重推组装失败，下一 tick 成功。
	tb.setFailNext(1)
	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	upd := nextFrame(t, frames, "update after failed-then-retried assembly")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	// 初始 1 + 失败 1 + 成功 1：证明 dirty 保持并经窗口 tick 自动重试。
	if n := tb.assemblyCount(); n != 3 {
		t.Errorf("assembly count = %d, want 3 (initial + failed retry + success)", n)
	}
}

// TestActiveSessionsStream_OverflowImmediateRepushOutsideWindow 任一 Overflow()
// 置位：先置 dirty 再窗口外立即重推（不等合并窗口）。
func TestActiveSessionsStream_OverflowImmediateRepushOutsideWindow(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	// 窗口 1s：update 若到达必然来自窗口外溢出重推路径。
	s := newStreamTestServer(t, tb, sub, 1*time.Second, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.triggerOverflow(ocdeckevent.TopicSession)
	var upd sseFrame
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("stream closed waiting for overflow repush")
		}
		upd = f
	case <-time.After(600 * time.Millisecond):
		t.Fatal("overflow repush did not arrive before coalesce window")
	}
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if n := tb.assemblyCount(); n != 2 {
		t.Errorf("assembly count = %d, want 2 (initial + overflow repush)", n)
	}
}

// TestActiveSessionsStream_OverflowAssemblyFailureHeartbeatRetry 溢出重推组装
// 失败 → dirty 保持 → 心跳 tick 重试成功送达 update，此后心跳恢复 `: ping`。
func TestActiveSessionsStream_OverflowAssemblyFailureHeartbeatRetry(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	// 窗口 1s（排除窗口重试路径），心跳 40ms 承担重试。
	s := newStreamTestServer(t, tb, sub, 1*time.Second, 40*time.Millisecond)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	tb.setFailNext(1) // 溢出立即重推组装失败
	sub.triggerOverflow(ocdeckevent.TopicTask)
	upd := nextFrame(t, frames, "update via heartbeat retry")
	if upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	// 连接仍存活：后续心跳照常发出注释行。
	ping := nextFrame(t, frames, "heartbeat ping")
	if ping.event != "" || ping.data != "ping" {
		t.Errorf("heartbeat frame = %+v, want comment `: ping`", ping)
	}
	// 初始 1 + 溢出失败 1 + 心跳重试成功 1。
	if n := tb.assemblyCount(); n != 3 {
		t.Errorf("assembly count = %d, want 3 (initial + failed overflow repush + heartbeat retry)", n)
	}
}

// TestActiveSessionsStream_HeartbeatCommentFrames 心跳按节奏出现且为 `: ping`
// 注释行（不是 event 帧）；无事件期间无任何其他帧。
func TestActiveSessionsStream_HeartbeatCommentFrames(t *testing.T) {
	tb := newStreamBackend()
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 1*time.Second, 50*time.Millisecond)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	// TeeReader 捕获原始 wire 字节，锁定注释行格式。
	var raw bytes.Buffer
	frames := startSSEFrameReader(io.TeeReader(resp.Body, &raw))

	snap := nextFrame(t, frames, "snapshot")
	if snap.event != "snapshot" {
		t.Fatalf("first frame = %+v, want snapshot", snap)
	}
	for i := 0; i < 2; i++ {
		ping := nextFrame(t, frames, "heartbeat ping")
		if ping.event != "" || ping.data != "ping" {
			t.Fatalf("frame %d after snapshot = %+v, want `: ping` comment", i+1, ping)
		}
	}
	wire := raw.String()
	if !strings.Contains(wire, ": ping\n\n") {
		t.Errorf("wire format missing `: ping` comment frame: %q", wire)
	}
	if strings.Contains(wire, "event: ping") {
		t.Errorf("heartbeat must be a comment line, not an event frame: %q", wire)
	}
}

// --- 认证 / 退出路径 ---

// TestActiveSessionsStream_Unauthorized401 无 token：中间件返回 JSON 401，
// 不进入 SSE（无 text/event-stream）。
func TestActiveSessionsStream_Unauthorized401(t *testing.T) {
	tb := newStreamBackend()
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/tasks/active/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatal(err)
	}
	if eb.Error.Code != CodeUnauthorized {
		t.Errorf("error code = %q, want %q", eb.Error.Code, CodeUnauthorized)
	}
}

// TestActiveSessionsStream_LegacyPathJSON404 projects-stream 改名后旧流路径不留别名：
// /api/v1/sessions/active/stream 返回 JSON 404（非 SSE）。
func TestActiveSessionsStream_LegacyPathJSON404(t *testing.T) {
	tb := newStreamBackend()
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active/stream", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("body = %q, want JSON error body with not_found code", string(body))
	}
	if sub.liveSubs() != 0 {
		t.Errorf("legacy path must not subscribe, liveSubs = %d", sub.liveSubs())
	}
}

// TestActiveSessionsStream_ClientDisconnectClosesSubscriptions 客户端断开
// （请求 ctx 取消）：handler 退出、四路订阅归零。
func TestActiveSessionsStream_ClientDisconnectClosesSubscriptions(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/tasks/active/stream", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			respCh <- resp
		}
	}()
	var resp *http.Response
	select {
	case resp = <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request did not start")
	}
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	cancel()
	waitFor(t, 2*time.Second, "subscriptions closed after client disconnect", func() bool {
		return sub.liveSubs() == 0
	})
}

// TestActiveSessionsStream_ProcessContextCancelExits 进程 ctx 取消（P2.4
// BaseContext）：SSE handler 观测 r.Context() 取消退出，订阅归零（Shutdown 5s
// 预算内释放）。
func TestActiveSessionsStream_ProcessContextCancelExits(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 5*time.Second)

	procCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := httptest.NewUnstartedServer(s.mux)
	ts.Config.BaseContext = func(net.Listener) context.Context { return procCtx }
	ts.Start()
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	// 进程取消（先取消 stream，再 Shutdown —— Start 的关停顺序）。
	cancel()
	waitFor(t, 4*time.Second, "subscriptions closed after process ctx cancel", func() bool {
		return sub.liveSubs() == 0
	})
}

// TestActiveSessionsStream_PushPathReadOnly 推送路径纯读：完整流程（snapshot +
// 事件 update + 心跳）不触发任何实时探测（opencode 调用代理）与写方法。
func TestActiveSessionsStream_PushPathReadOnly(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 50*time.Millisecond, 40*time.Millisecond)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	if upd := nextFrame(t, frames, "update"); upd.event != "update" {
		t.Fatalf("event = %q, want update", upd.event)
	}
	if ping := nextFrame(t, frames, "heartbeat ping"); ping.data != "ping" {
		t.Fatalf("heartbeat = %+v, want ping", ping)
	}

	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times on push path, want 0 (no opencode calls)", calls)
	}
	if writes := tb.writeCalls(); len(writes) != 0 {
		t.Errorf("push path performed writes %v, want none", writes)
	}
	if n := tb.assemblyCount(); n < 2 {
		t.Errorf("assembly count = %d, want >= 2 (push reads only)", n)
	}
}

// --- 统一写路径：Write/Flush 失败立即退订退出 ---

var errStreamWriteFailed = errors.New("sse stream write failed")

// flakyFlushWriter 前 failAfter 次 flush 成功、之后失败的底层 ResponseWriter；
// 经 statusRecorder 的 FlushError 路径注入写失败（P2.5 路径）。
type flakyFlushWriter struct {
	header    http.Header
	mu        sync.Mutex
	failAfter int
	flushes   int
}

func (f *flakyFlushWriter) Header() http.Header         { return f.header }
func (f *flakyFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *flakyFlushWriter) WriteHeader(int)             {}

func (f *flakyFlushWriter) FlushError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	if f.flushes > f.failAfter {
		return errStreamWriteFailed
	}
	return nil
}

func (f *flakyFlushWriter) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// TestActiveSessionsStream_WriteFailureUnsubscribesAndExits snapshot/update/
// overflow 重推/心跳任一帧写（flush）失败：立即退订退出。
func TestActiveSessionsStream_WriteFailureUnsubscribesAndExits(t *testing.T) {
	cases := []struct {
		name      string
		coalesce  time.Duration
		heartbeat time.Duration
		failAfter int
		trigger   func(sub *fakeStreamSubscriber)
	}{
		// 首帧 snapshot flush 即失败。
		{name: "snapshot-flush-failure", coalesce: 20 * time.Millisecond, heartbeat: time.Hour, failAfter: 0},
		// snapshot 成功后，窗口 tick 的 update flush 失败。
		{name: "update-flush-failure", coalesce: 20 * time.Millisecond, heartbeat: time.Hour, failAfter: 1,
			trigger: func(sub *fakeStreamSubscriber) {
				sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
			}},
		// snapshot 成功后，溢出窗口外重推 flush 失败。
		{name: "overflow-repush-flush-failure", coalesce: time.Hour, heartbeat: time.Hour, failAfter: 1,
			trigger: func(sub *fakeStreamSubscriber) {
				sub.triggerOverflow(ocdeckevent.TopicServeRuntime)
			}},
		// snapshot 成功后，心跳注释帧 flush 失败。
		{name: "heartbeat-flush-failure", coalesce: time.Hour, heartbeat: 25 * time.Millisecond, failAfter: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
			sub := &fakeStreamSubscriber{}
			s := newStreamTestServer(t, tb, sub, c.coalesce, c.heartbeat)

			fw := &flakyFlushWriter{header: http.Header{}, failAfter: c.failAfter}
			rec := &statusRecorder{ResponseWriter: fw, status: http.StatusOK}
			req := httptest.NewRequest("GET", "/api/v1/tasks/active/stream", nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.handleActiveSessionsStream(rec, req)
			}()

			// snapshot 帧成功用例：等待首帧 flush 完成再注入触发。
			if c.failAfter > 0 {
				waitFor(t, 2*time.Second, "snapshot frame flushed", func() bool { return fw.flushCount() >= 1 })
				if c.trigger != nil {
					c.trigger(sub)
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not exit after write failure")
			}
			if n := sub.liveSubs(); n != 0 {
				t.Errorf("live subs = %d, want 0 after write-failure exit", n)
			}
		})
	}
}

// failWriteWriter Write 一律失败的底层 ResponseWriter：经 statusRecorder 注入
// Write 错误（区别于 flakyFlushWriter 的 flush 错误路径），验证统一写路径的
// Write 错误同样立即退订退出。
type failWriteWriter struct{ header http.Header }

func (f *failWriteWriter) Header() http.Header         { return f.header }
func (f *failWriteWriter) Write(b []byte) (int, error) { return 0, errStreamWriteFailed }
func (f *failWriteWriter) WriteHeader(int)             {}
func (f *failWriteWriter) FlushError() error           { return nil }

// TestActiveSessionsStream_WriteErrorUnsubscribesAndExits 首帧 snapshot 的
// ResponseWriter.Write 错误（非 Flush 错误）：统一写路径立即退订退出。
func TestActiveSessionsStream_WriteErrorUnsubscribesAndExits(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 20*time.Millisecond, time.Hour)

	rec := &statusRecorder{ResponseWriter: &failWriteWriter{header: http.Header{}}, status: http.StatusOK}
	req := httptest.NewRequest("GET", "/api/v1/tasks/active/stream", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleActiveSessionsStream(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after Write error")
	}
	if n := sub.liveSubs(); n != 0 {
		t.Errorf("live subs = %d, want 0 after Write-error exit", n)
	}
}

// TestActiveSessionsStream_BusinessFrameResetsHeartbeat 业务帧重置心跳：
// 心跳语义为「连续 heartbeat 间隔无业务帧」（design D3）。t0 发 snapshot（心跳
// 起算），t≈120ms 触发 update——若 update 未重置心跳，ping 将在 t≈200ms 到达；
// 断言 reset 后 ping 推迟到完整间隔之外（t≈320ms 前无帧），随后按新起算点到达。
func TestActiveSessionsStream_BusinessFrameResetsHeartbeat(t *testing.T) {
	tb := newStreamBackend(activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100))
	sub := &fakeStreamSubscriber{}
	s := newStreamTestServer(t, tb, sub, 30*time.Millisecond, 200*time.Millisecond)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	nextFrame(t, frames, "snapshot")

	// t≈120ms 触发事件 → 窗口 30ms 内写出 update（远早于 200ms 心跳原起算点）。
	time.Sleep(120 * time.Millisecond)
	sub.publish(ocdeckevent.NewSessionTouched("sess-1", "t1"))
	upd := nextFrame(t, frames, "update before original heartbeat deadline")
	if upd.event != "update" {
		t.Fatalf("frame = %+v, want update (a ping here means heartbeat fired before reset)", upd)
	}

	// 覆盖原起算点 t≈200ms（观察至 t≈320ms）：未重置的 ticker 会在此窗口发 ping。
	assertNoFrame(t, frames, 150*time.Millisecond, "ping before a full heartbeat interval after business frame")

	// 重置后 ping 在 update+200ms ≈ t≈350ms 到达。
	ping := nextFrameWithin(t, frames, 300*time.Millisecond, "ping after reset heartbeat interval")
	if ping.event != "" || ping.data != "ping" {
		t.Errorf("frame = %+v, want `: ping` comment", ping)
	}
}

// TestActiveSessionsStream_EventDuringReassemblyNotLost 重组装进行中到达的事件
// 不丢失：update#1 组装期间（overviewFn 调用内）发布的事件不在 update#1 的数据里，
// 但必须留在订阅缓冲中被事件循环接收（重新置脏），其变更经后续 update 收敛——
// 断言该事件的行最终出现在稍后的 update 帧中（最终一致性）。
func TestActiveSessionsStream_EventDuringReassemblyNotLost(t *testing.T) {
	rowA := activeRow("tA", "p1", "projA", "taskA", "bA", "/wtA", 100)
	rowB := activeRow("tB", "p2", "projB", "taskB", "bB", "/wtB", 200)
	sub := &fakeStreamSubscriber{}
	tb := newStreamBackend()
	gen := 0
	tb.overviewFn = func(ctx context.Context) []application.ActiveTaskOverviewRow {
		gen++
		switch gen {
		case 1:
			return []application.ActiveTaskOverviewRow{rowA}
		case 2:
			// update#1 重组装进行中发布事件 B：此刻 update#1 的数据已固定为 [A]，
			// B 落入订阅缓冲，必须由事件循环随后接收置脏。
			sub.publish(ocdeckevent.NewSessionClaimed("sess-B", "tB"))
			return []application.ActiveTaskOverviewRow{rowA}
		default:
			return []application.ActiveTaskOverviewRow{rowA, rowB}
		}
	}
	s := newStreamTestServer(t, tb, sub, 40*time.Millisecond, 10*time.Second)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := openActiveSessionsStream(t, ts.URL)
	defer resp.Body.Close()
	frames := startSSEFrameReader(resp.Body)
	snap := nextFrame(t, frames, "snapshot")
	if strings.Contains(snap.data, "taskB") {
		t.Fatalf("snapshot unexpectedly contains taskB: %s", snap.data)
	}

	// 触发 update#1（其组装期间发布事件 B）。
	sub.publish(ocdeckevent.NewSessionTouched("sess-A", "tA"))
	upd1 := nextFrame(t, frames, "update #1")
	if upd1.event != "update" || strings.Contains(upd1.data, "taskB") {
		t.Fatalf("update #1 = %+v, want update without taskB (assembled before event B)", upd1)
	}

	// 事件 B 不丢失：其变更最终出现在后续 update（若 B 被丢，dirty 已清且无后续
	// 事件，不会再有任何帧，下述等待将超时失败）。
	var upd2 sseFrame
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed waiting for update carrying event B's change")
			}
			if strings.Contains(f.data, "taskB") {
				upd2 = f
				break loop
			}
			t.Fatalf("unexpected intermediate frame before taskB update: %+v", f)
		case <-deadline:
			t.Fatal("event arriving during reassembly was lost: no update carries its change")
		}
	}
	if upd2.event != "update" {
		t.Errorf("frame carrying taskB = %+v, want update", upd2)
	}
}
