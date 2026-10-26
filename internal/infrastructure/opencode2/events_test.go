// events_test.go 落实 tasks 3.2/3.3：EventSource——首帧 server.connected 握手
// 判定（进程存活不算成功）、事件归一化（fixture sse_frames.txt 驱动）、data.
// sessionID 归属校验拒绝、未知事件保守处理（不猜测 SessionError）、断线重连
// 与重连后快照自补。
package opencode2

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// sseHandler 构造 SSE 端点 handler：写出 frames 后保持连接（或按 keep 控制）。
type sseHandler struct {
	frames string
	keep   bool // true：写完后保持连接直到 ctx 取消（false：写完即关闭触发重连）
}

func writeSSE(t *testing.T, w http.ResponseWriter, frames string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(frames))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestEvents_HandshakeFirstFrameNotProcessAlive(t *testing.T) {
	gate := make(chan struct{})
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			t.Errorf("path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("directory") != "/wt" {
			t.Errorf("directory query: %q", r.URL.Query().Get("directory"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-gate // 连接已建立但未发任何帧（进程存活态）
		writeSSE(t, w, string(readFixture(t, "sse_frames.txt")))
		<-r.Context().Done()
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{})
	var connected atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, testBinding(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es.SetCallbacks(func() { connected.Store(true) }, nil)

	// 订阅受理 + 进程存活 ≠ 连接建立：无首帧期间不得触发握手、不得有事件。
	time.Sleep(150 * time.Millisecond)
	if connected.Load() {
		t.Fatalf("handshake must not fire before first frame")
	}
	select {
	case ev, ok := <-es.Events():
		t.Fatalf("no event expected before first frame, got %v (ok=%v)", ev, ok)
	default:
	}

	// 首帧（server.connected）到达 → 握手触发，server.connected 不透传。
	close(gate)
	expectEvent(t, es, domain.EventStatusChanged) // 第一条透传事件 = execution.started
	if !connected.Load() {
		t.Fatalf("handshake must fire on first frame")
	}
	cancel()
	_ = es.Close()
}

func TestEvents_FixtureStreamNormalization(t *testing.T) {
	var clientRef *Client
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			t.Errorf("path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		writeSSE(t, w, string(readFixture(t, "sse_frames.txt")))
		<-r.Context().Done()
	})
	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{})
	p.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srv.URL
		clientRef = c
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, testBinding(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es.SetCallbacks(func() {}, nil)

	// 归一化序列（server.connected/未知事件/无归属/stream failure 均不透传）：
	// busy → renamed → idle → permission.asked → created(带 parent)。
	expectStatus(t, es, domain.StatusBusy)
	upd := expectEvent(t, es, domain.EventSessionUpdated)
	if upd.Session.Title != "renamed-title" {
		t.Fatalf("renamed title: %q", upd.Session.Title)
	}
	expectStatus(t, es, domain.StatusIdle)
	att := expectEvent(t, es, domain.EventAttentionRaised)
	if att.Attention.ID != "per_fixture01" || att.Attention.Permission.Permission != "shell" {
		t.Fatalf("attention: %+v", att.Attention)
	}
	if len(att.Attention.Permission.Patterns) != 1 || att.Attention.Permission.Patterns[0] != "touch /tmp/fixture-marker" {
		t.Fatalf("permission patterns: %+v", att.Attention.Permission)
	}
	created := expectEvent(t, es, domain.EventSessionCreated)
	// created 映射（D7）：Key={opencode-v2, default, data.sessionID}；parent 同
	// 命名空间；时间 epoch 毫秒不换算。
	if created.Key.UpstreamID != "ses_fixture02b" {
		t.Fatalf("created key: %v", created.Key)
	}
	if created.Session.Parent == nil || created.Session.Parent.UpstreamID != "ses_fixture01a" {
		t.Fatalf("parent: %+v", created.Session.Parent)
	}
	if created.Session.CreatedAt != 1792089041106 {
		t.Fatalf("created at: %d", created.Session.CreatedAt)
	}

	// 保守丢弃+记录：unknown.probe、无归属 renamed、stream failure。
	if clientRef == nil || clientRef.MalformedCount() != 3 {
		t.Fatalf("conservative drop count: %v (want 3)", clientRef)
	}
	cancel()
	_ = es.Close()
}

// TestEvents_TerminalExecutionEventsMapIdle 回归：真实停止/失败的 v2 会话只发
// session.execution.interrupted / session.execution.failed、不跟随 succeeded
//（实证 evidence/20261016T090000Z-0bb6ac6c live/lifecycle/p1b-frames-1.jsonl:20
// interrupted reason=user、evidence/20261014T040930Z-1bc764c7 live/event/
// stream.ndjson:43 failed data.error）——两者均为执行终态，MUST 映射回 idle，
// 否则 UI 永久 busy。error/reason 载荷仅诊断，不参与映射。
func TestEvents_TerminalExecutionEventsMapIdle(t *testing.T) {
	var clientRef *Client
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			t.Errorf("path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		writeSSE(t, w, "data: {\"id\":\"e0\",\"type\":\"server.connected\",\"data\":{}}\n\n"+
			"data: {\"id\":\"e1\",\"type\":\"session.execution.started\",\"data\":{\"sessionID\":\"ses_fixture01a\"}}\n\n"+
			// 实证形态：failed 附 data.error（诊断载荷，不参与映射）。
			"data: {\"id\":\"e2\",\"type\":\"session.execution.failed\",\"data\":{\"sessionID\":\"ses_fixture01a\",\"error\":{\"type\":\"provider.invalid-request\",\"message\":\"Model exo-free has been deprecated.\"}}}\n\n"+
			"data: {\"id\":\"e3\",\"type\":\"session.execution.started\",\"data\":{\"sessionID\":\"ses_fixture01a\"}}\n\n"+
			// 实证形态：interrupted 附 reason=user。
			"data: {\"id\":\"e4\",\"type\":\"session.execution.interrupted\",\"data\":{\"sessionID\":\"ses_fixture01a\",\"reason\":\"user\"}}\n\n")
		<-r.Context().Done()
	})
	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{StatusVerified: true})
	p.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srv.URL
		clientRef = c
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, testBinding(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es.SetCallbacks(func() {}, nil)

	// busy →（failed 终态）idle → busy →（interrupted 终态）idle。
	expectStatus(t, es, domain.StatusBusy)
	expectStatus(t, es, domain.StatusIdle)
	expectStatus(t, es, domain.StatusBusy)
	idle := expectStatus(t, es, domain.StatusIdle)
	if idle.Key != testSessionKey() {
		t.Fatalf("terminal idle key: %v", idle.Key)
	}

	// provider 可见当前状态（StatusVerified 冻结映射）= idle，不残留 busy。
	out, err := p.Status().Current(ctx, testBinding(t))
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if out[testSessionKey()] != domain.StatusIdle {
		t.Fatalf("provider-visible status: %v", out)
	}
	// 终态事件是已知事件，MUST NOT 走保守丢弃。
	if clientRef == nil || clientRef.MalformedCount() != 0 {
		t.Fatalf("terminal events must not count as conservative drops: %v", clientRef)
	}
	cancel()
	_ = es.Close()
}

func TestEvents_Reconnect_SnapshotSelfHeal(t *testing.T) {
	var conns int32
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/event":
			if n := atomic.AddInt32(&conns, 1); n == 1 {
				// 首连接：握手帧后立即断开（触发断流重连）。
				writeSSE(t, w, "data: {\"id\":\"e0\",\"type\":\"server.connected\",\"data\":{}}\n\n")
				return
			}
			// 重连：握手帧 → post-reconnect 业务事件 → 保持连接。
			writeSSE(t, w, "data: {\"id\":\"e1\",\"type\":\"server.connected\",\"data\":{}}\n\n"+
				"data: {\"id\":\"e2\",\"type\":\"session.renamed\",\"data\":{\"sessionID\":\"ses_fixture01a\",\"title\":\"post-reconnect\"}}\n\n")
			<-r.Context().Done()
		case "/api/session/ses_fixture01a":
			// 重连快照自补的 GET /api/session/{id}（标题区别于事件帧以验证来源）。
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"ses_fixture01a","time":{"created":1,"updated":2},"title":"snapshot-title","location":{"directory":"/wt"}}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	setMasterSecret(t)
	p := NewSPIProvider(ProviderOptions{})
	p.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, Options{
			OpTimeout:     time.Second,
			ReconnectBase: 5 * time.Millisecond,
			ReconnectMax:  20 * time.Millisecond,
		})
		c.baseURL = srv.URL
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, testBinding(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var lifecycle []string
	es.SetCallbacks(
		func() { lifecycle = append(lifecycle, "connected") },
		func() { lifecycle = append(lifecycle, "reconnect") },
	)

	// 重连后快照自补（CONTRACT §9）：先收快照 SessionUpdated（snapshot-title），
	// 再收新连接事件（post-reconnect）；不依赖事件回放。
	snap := expectEvent(t, es, domain.EventSessionUpdated)
	if snap.Session.Title != "snapshot-title" {
		t.Fatalf("snapshot event title: %q", snap.Session.Title)
	}
	post := expectEvent(t, es, domain.EventSessionUpdated)
	if post.Session.Title != "post-reconnect" {
		t.Fatalf("post-reconnect title: %q", post.Session.Title)
	}
	if atomic.LoadInt32(&conns) < 2 {
		t.Fatalf("reconnect must have happened, conns=%d", conns)
	}
	cancel()
	_ = es.Close()
}

// TestEvents_HandshakeRejectsBadFirstFrame 落实 G2-F1：首个可解析事件必须严格
// 为 server.connected——首帧为业务事件/畸形帧/stream failure 均为握手失败：
// 不触发 ready、不透传业务事件，按重连语义退避重试（重试耗尽后返回错误）。
func TestEvents_HandshakeRejectsBadFirstFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
	}{
		{"wrong-first-frame business event",
			"data: {\"id\":\"e1\",\"type\":\"session.created\",\"data\":{\"sessionID\":\"ses_fixture01a\",\"title\":\"early\"}}\n\n"},
		{"malformed-first-frame",
			"data: {not-json\n\n"},
		{"stream-failure-first-frame",
			"data: {\"id\":\"e1\",\"type\":\"effect/http-api/stream/failure\",\"data\":{}}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var conns, readyCalls, events int32
			srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&conns, 1)
				writeSSE(t, w, tc.frame)
				<-r.Context().Done() // 保持连接：失败必须由握手判定主动发现
			})
			c := NewClient(1, "pw", Options{
				OpTimeout:         time.Second,
				ReconnectBase:     2 * time.Millisecond,
				ReconnectMax:      5 * time.Millisecond,
				ReconnectMaxTries: 2,
				OnReady:           func() { atomic.AddInt32(&readyCalls, 1) },
			})
			c.baseURL = srv.URL
			err := c.SubscribeEvents(context.Background(), "/wt",
				func(wireEvent) { atomic.AddInt32(&events, 1) },
				func() {})
			// 重试耗尽返回错误，链上可辨握手失败原因。
			if err == nil || !errors.Is(err, errHandshakeFailed) {
				t.Fatalf("expect errHandshakeFailed, got %v", err)
			}
			if !strings.Contains(err.Error(), "attempts exhausted") {
				t.Fatalf("expected bounded retries, got %v", err)
			}
			if n := atomic.LoadInt32(&conns); n != 3 { // 首次 + 2 次重试
				t.Fatalf("connections: %d", n)
			}
			if atomic.LoadInt32(&readyCalls) != 0 {
				t.Fatalf("ready must not fire on failed handshake")
			}
			if atomic.LoadInt32(&events) != 0 {
				t.Fatalf("no event may be delivered on failed handshake")
			}
		})
	}
}

// expectEvent 消费下一条事件并断言 Kind。
func expectEvent(t *testing.T, es appagent.EventStream, kind domain.EventKind) domain.AgentEvent {
	t.Helper()
	select {
	case ev, ok := <-es.Events():
		if !ok {
			t.Fatalf("stream closed while waiting for %s (err=%v)", kind, es.Err())
		}
		if ev.Kind != kind {
			t.Fatalf("event kind: got %s want %s", ev.Kind, kind)
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("event invalid: %v", err)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s (err=%v)", kind, es.Err())
		return domain.AgentEvent{}
	}
}

// expectStatus 消费下一条状态事件并断言状态值。
func expectStatus(t *testing.T, es appagent.EventStream, state domain.AgentStatus) domain.AgentEvent {
	t.Helper()
	ev := expectEvent(t, es, domain.EventStatusChanged)
	if ev.Status.State != state {
		t.Fatalf("status: got %s want %s", ev.Status.State, state)
	}
	return ev
}

// TestTranslateV2Event_CreatedEnvelopeMatrix 记录翻译层事实（跨任务偷
// candidate 事故的输入面）：session.created 的 envelope location 三形态
// （无 location=容器事故形态 / location 缺 directory / location 等于任意目录）
// 都产出同一 EventSessionCreated——归属仅凭 data.sessionID（spec 冻结），
// directory 不参与翻译。因此「他任务 candidate 广播不被认领」的防线在 task 层
// （handleAgentEvent 对 v2 typed created 一律丢弃——归属唯一来源=原子提交
// 协议+对齐），不在本层。
func TestTranslateV2Event_CreatedEnvelopeMatrix(t *testing.T) {
	frames := []string{
		`{"id":"evt1","type":"session.created","data":{"sessionID":"ses_ea8bee95","time":{"created":100,"updated":100}}}`,
		`{"id":"evt2","type":"session.created","location":{},"data":{"sessionID":"ses_ea8bee95","time":{"created":100,"updated":100}}}`,
		`{"id":"evt3","type":"session.created","location":{"directory":"/home/ocdeck"},"data":{"sessionID":"ses_ea8bee95","time":{"created":100,"updated":100}}}`,
	}
	for _, frame := range frames {
		ev, err := parseWireEvent([]byte(frame))
		if err != nil {
			t.Fatalf("parse %s: %v", frame, err)
		}
		out, ok := translateV2Event(ev, nil)
		if !ok || out.Kind != domain.EventSessionCreated {
			t.Fatalf("created must translate regardless of envelope location, frame=%s ok=%v", frame, ok)
		}
		if out.Session == nil || out.Session.Key.UpstreamID != "ses_ea8bee95" ||
			out.Session.Key.ProviderID != ProviderID || out.Session.Key.ProviderInstanceID != ProviderInstanceID {
			t.Fatalf("key mismatch: %+v", out.Session)
		}
	}
}
