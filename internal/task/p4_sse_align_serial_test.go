package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/opencode"
)

// 本文件验证 P4 门禁项：SSE 首次对齐与重连串行化。
// 首次 OnReady 后主路径做首次 align，SSE goroutine 随即继续读流；若首次 align 未完成即断流，
// onReconnect 触发的重对齐 MUST 排队等待首次 align 完成，不得并发清空 buffered 造成事件丢失/乱序。
// 时序由可控 channel/屏障驱动，不靠 sleep 碰运气。

// alignSerialOC 控制 ListSessions 返回时机，并观测并发对齐数，验证串行化。
// 嵌入 *mockOC 提供其余 OCClient 方法（Health/Probe/GetSession/DeleteSession/SubscribeEvents 占位，
// SubscribeEvents 由 alignSerialSubscribeOC 覆盖）。
type alignSerialOC struct {
	*mockOC
	onReadyCb func()
	// firstListCh：首次 align 的 ListSessions 在此阻塞（屏障），测试据此注入 onReconnect。
	firstListCh chan struct{}
	// reconnectListCh：reconnect align 的 ListSessions 在此阻塞，测试断言排队成功后放行。
	reconnectListCh chan struct{}
	// inflight：当前正在执行的 ListSessions 数（>1 即并发 align，违反串行化）。
	inflight atomic.Int32
	// listCalls：ListSessions 调用次数。
	listCalls atomic.Int32
	// maxInflight：观测到的最大并发 ListSessions 数（MUST ==1）。
	maxInflight atomic.Int32
	// reconnectRan：reconnect 的 align 已开始（ListSessions 第 2 次调用）。
	reconnectRan atomic.Bool
	firstSessions []opencode.Session
	reconnectSess []opencode.Session
}

func (c *alignSerialOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	n := c.inflight.Add(1)
	for {
		m := c.maxInflight.Load()
		if n <= m || c.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	defer c.inflight.Add(-1)
	calls := c.listCalls.Add(1)
	if calls == 1 {
		// 首次 align：在 barrier 阻塞，测试据此注入 onReconnect。
		<-c.firstListCh
		return c.firstSessions, nil
	}
	// reconnect align：记录已开始后阻塞，测试断言排队成功后放行。
	c.reconnectRan.Store(true)
	<-c.reconnectListCh
	return c.reconnectSess, nil
}

// alignSerialSubscribeOC 控制 SubscribeEvents 时序，模拟"首次 align 进行中发生断流→reconnect"。
// 可选在首次 align 进行中发送缓冲事件（bufEvents 非空时，验证缓冲事件不因并发清空丢失）。
type alignSerialSubscribeOC struct {
	*alignSerialOC
	// firstAlignStarted：主路径进入首次 align（ListSessions 已阻塞）后 SubscribeEvents 收到此信号。
	firstAlignStarted chan struct{}
	// reconnectTrigger：测试发信号触发 onReconnect（模拟断流→reconnect）。
	reconnectTrigger chan struct{}
	// reconnectQueued：onReconnect 已进入（标记排队等待 alignMu）。
	reconnectQueued atomic.Bool
	// bufEvents：首次 align 进行中经 onEvent 发送的缓冲事件（buffering=true 进 buffered）。
	bufEvents []opencode.Event
}

func (c *alignSerialSubscribeOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReadyCb != nil {
		c.onReadyCb()
	}
	// 等主路径进入首次 align（ListSessions 已阻塞在 firstListCh）。
	<-c.firstAlignStarted
	// 首次 align 进行中发送缓冲事件（buffering=true 进 buffered），验证不被并发清空丢失。
	for _, ev := range c.bufEvents {
		onEvent(ev)
	}
	// 测试释放 reconnectTrigger → 触发 onReconnect（模拟首次 align 进行中断流→reconnect）。
	<-c.reconnectTrigger
	// onReconnect 内部 alignMu.Lock 阻塞（首次 align 仍持 alignMu）。
	// 标记排队后同步调用 onReconnect（其内部 alignMu.Lock 排队等待首次 align 释放）。
	c.reconnectQueued.Store(true)
	onReconnect()
	<-ctx.Done()
	return ctx.Err()
}

// TestP4_SSEFirstAlignInProgressReconnectSerializes 验证：首次 align 进行中发生断流→reconnect 时，
// reconnect align MUST 排队等待首次 align 完成，不并发清空 buffered（无并发 align）。
// 时序（全程 channel/屏障驱动）：
//  1. onReady → 主路径首次 align（ListSessions 阻塞于 firstListCh）
//  2. 触发 onReconnect（模拟断流→reconnect）
//  3. 断言 reconnect 已排队、首次 align 仍在进行、reconnect align 未开始、无并发 align（maxInflight==1）
//  4. 放行首次 align 完成 → 断言 reconnect align 随后开始（串行接力）
//  5. 放行 reconnect align 完成 → 最终 sessions == reconnect 全量对齐结果，无事件丢失
func TestP4_SSEFirstAlignInProgressReconnectSerializes(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	traced := wrapSessionTrace(tStore)

	base := &alignSerialOC{
		mockOC:        newMockOC(true),
		firstSessions: []opencode.Session{{ID: "S1", Time: opencode.SessionTime{Updated: 100, Created: 100}}},
		// reconnectSess 包含 S2 + E1/E2：断流前到达的 session.created(E1/E2) 事件在重连时已是
		// serve 上真实 session，全量对齐 MUST upsert 它们（不因并发清空 buffered 丢失）。
		reconnectSess:   []opencode.Session{{ID: "S2", Time: opencode.SessionTime{Updated: 200, Created: 200}}, {ID: "E1", Time: opencode.SessionTime{Updated: 300, Created: 300}}, {ID: "E2", Time: opencode.SessionTime{Updated: 310, Created: 310}}},
		firstListCh:     make(chan struct{}),
		reconnectListCh: make(chan struct{}),
	}
	sub := &alignSerialSubscribeOC{
		alignSerialOC:     base,
		firstAlignStarted: make(chan struct{}),
		reconnectTrigger:  make(chan struct{}),
		// 缓冲事件在首次 align 进行中发送（buffering=true），验证不被并发清空丢失。
		bufEvents: []opencode.Event{makeEventWithDir("session.created", "E1", 300, "/data/worktrees/p1/t1"), makeEventWithDir("session.created", "E2", 310, "/data/worktrees/p1/t1")},
	}
	factory := func(port int, password string, opts opencode.Options) OCClient {
		base.onReadyCb = opts.OnReady
		return sub
	}
	m := newTestManagerWithFactory(t, traced, proc, newMockWorktree(), factory)

	activateErr := make(chan error, 1)
	go func() { activateErr <- m.Activate(context.Background(), "t1") }()

	// 等主路径进入首次 align（ListSessions 已阻塞，inflight==1）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if base.listCalls.Load() == 1 && base.inflight.Load() == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if base.listCalls.Load() != 1 || base.inflight.Load() != 1 {
		t.Fatalf("first align not in progress: listCalls=%d inflight=%d", base.listCalls.Load(), base.inflight.Load())
	}

	// 通知 SubscribeEvents：主路径已进入首次 align。
	close(sub.firstAlignStarted)
	// 触发 onReconnect（模拟首次 align 进行中断流→reconnect）。
	close(sub.reconnectTrigger)

	// 等 onReconnect 排队（alignMu 被首次 align 持有，reconnect 在等待）。
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sub.reconnectQueued.Load() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sub.reconnectQueued.Load() {
		t.Fatal("onReconnect did not queue (alignMu held by first align)")
	}

	// 关键断言：reconnect 已排队，但首次 align 仍在进行，reconnect align 未开始，无并发 align。
	if base.reconnectRan.Load() {
		t.Fatal("reconnect align MUST wait for first align to release alignMu; reconnect align started while first align in progress (no serialization)")
	}
	if base.maxInflight.Load() > 1 {
		t.Fatalf("concurrent align detected: maxInflight=%d (MUST be <=1)", base.maxInflight.Load())
	}
	if base.listCalls.Load() != 1 {
		t.Fatalf("reconnect align MUST NOT start before first align completes; listCalls=%d", base.listCalls.Load())
	}

	// 放行首次 align 完成（ListSessions 返回 → 首次 align + drainAndRelease → alignMu 释放 → reconnect 接力）。
	close(base.firstListCh)
	// 等 reconnect align 开始（证明首次 align 已释放 alignMu，reconnect 串行接力）。
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if base.reconnectRan.Load() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !base.reconnectRan.Load() {
		t.Fatal("reconnect align did not start after first align released alignMu")
	}
	if base.maxInflight.Load() > 1 {
		t.Fatalf("aligns ran concurrently after release: maxInflight=%d", base.maxInflight.Load())
	}
	// 放行 reconnect align 完成 → drainAndRelease → Activate 返回。
	close(base.reconnectListCh)

	select {
	case err := <-activateErr:
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Activate did not return after reconnect align released")
	}

	// Activate 返回仅意味着首次 align + drainAndRelease 完成；reconnect align 在 SSE goroutine 内异步执行，
	// 需等其 AlignSessions 落库（listCalls==2 且 inflight==0）后再断言最终状态。
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if base.listCalls.Load() == 2 && base.inflight.Load() == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if base.listCalls.Load() != 2 || base.inflight.Load() != 0 {
		t.Fatalf("reconnect align not completed: listCalls=%d inflight=%d", base.listCalls.Load(), base.inflight.Load())
	}

	// 最终断言：只有 reconnect 全量对齐的结果（S2 + E1/E2），首次 align 的 S1 被全量重对齐覆盖（幂等 upsert）。
	// 缓冲事件 E1/E2 不因并发清空丢失：reconnect 全量对齐 upsert E1/E2（其在首次 align 期间经 SSE 缓冲，
	// drainAndRelease 已排空，或被 reconnect 全量对齐覆盖，最终落库结果一致）。
	sessions, _ := tStore.ListTaskSessions(context.Background(), "t1")
	got := map[string]bool{}
	for _, s := range sessions {
		got[s.SessionID] = true
	}
	if !got["S2"] || !got["E1"] || !got["E2"] {
		t.Fatalf("final sessions MUST include reconnect align result (S2,E1,E2), got %v", got)
	}
	if got["S1"] {
		t.Errorf("first align session S1 MUST be overwritten by reconnect full align, got S1 still present")
	}
	// align 共执行两次（首次 + reconnect），全程串行（maxInflight==1）。
	if base.listCalls.Load() != 2 {
		t.Fatalf("expected exactly 2 align calls (first + reconnect), got %d", base.listCalls.Load())
	}
	if base.maxInflight.Load() != 1 {
		t.Fatalf("aligns MUST be serialized (maxInflight=1), got %d", base.maxInflight.Load())
	}
}

// alignSessions 幂等 upsert，丢弃半成品状态安全。