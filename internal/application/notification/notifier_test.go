// notifier_test.go Lane B 测试基建：fake 端口（bus/快照源/配置/渠道/枚举/时钟）
// 与 run loop 集成测试（design D1/D3/D4；spec「通知抑制、启动基线与对账」等）。
package notification

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// errNotFound fakeTasks 未登记任务的读取错误。
var errNotFound = errors.New("fakeTasks: task not found")

// --- fakes ---

// fakeTasks 可编程组合快照源（errFn 非 nil 时按任务注入读取失败）。
type fakeTasks struct {
	mu    sync.Mutex
	snaps map[string]TaskSnapshot
	errFn func(taskID string) error
	reads int
}

func newFakeTasks(snaps ...TaskSnapshot) *fakeTasks {
	ft := &fakeTasks{snaps: map[string]TaskSnapshot{}}
	for _, s := range snaps {
		ft.snaps[s.Task.ID] = s
	}
	return ft
}

func (f *fakeTasks) set(snap TaskSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[snap.Task.ID] = snap
}

func (f *fakeTasks) setErr(fn func(taskID string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errFn = fn
}

func (f *fakeTasks) TaskNotificationSnapshot(_ context.Context, taskID string) (TaskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.errFn != nil {
		if err := f.errFn(taskID); err != nil {
			return TaskSnapshot{}, err
		}
	}
	snap, ok := f.snaps[taskID]
	if !ok {
		return TaskSnapshot{}, errNotFound
	}
	return snap, nil
}

// fakeCfgStore 可变配置快照（模拟并发 PUT 热更新）；seq 非空时按调用次序弹出
// （末值重复），验证按候选逐次读取（B6）。
type fakeCfgStore struct {
	mu  sync.Mutex
	cfg notification.Config
	seq []notification.Config
}

func (f *fakeCfgStore) Config() notification.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seq) > 0 {
		c := f.seq[0]
		if len(f.seq) > 1 {
			f.seq = f.seq[1:]
		}
		return c
	}
	return f.cfg
}

func (f *fakeCfgStore) set(c notification.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = c
}

// fakeChannel 记录型渠道（可脚本化失败/阻塞，验证并行投递与 DispatchPlan 固化）。
type fakeChannel struct {
	name string
	caps notification.Capability

	mu      sync.Mutex
	intents []notification.Intent
	configs []notification.ChannelConfig
	calls   int
	fail    bool
	block   chan struct{} // 非 nil 时 Send 阻塞直到关闭
	unavail bool          // true → Available()=false（macos skipped 矩阵）
}

func (c *fakeChannel) Name() string                  { return c.name }
func (c *fakeChannel) Caps() notification.Capability { return c.caps }
func (c *fakeChannel) Available() bool               { return !c.unavail }
func (c *fakeChannel) Send(_ context.Context, in notification.Intent, cfg notification.ChannelConfig) notification.Result {
	c.mu.Lock()
	if c.block != nil {
		c.mu.Unlock()
		<-c.block
		c.mu.Lock()
	}
	c.calls++
	c.intents = append(c.intents, in)
	c.configs = append(c.configs, cfg)
	failed := c.fail
	c.mu.Unlock()
	if failed {
		return notification.Result{OK: false, Err: "scripted failure"}
	}
	return notification.Result{OK: true}
}

func (c *fakeChannel) sent() []notification.Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notification.Intent(nil), c.intents...)
}

func (c *fakeChannel) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fakeLister active 任务枚举（可注入错误；记录调用）。
type fakeLister struct {
	mu    sync.Mutex
	ids   []string
	err   error
	calls int
}

func (f *fakeLister) ListAllActiveTaskIDs(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.ids, f.err
}

func (f *fakeLister) set(ids []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids, f.err = ids, err
}

// fakeSub / fakeBus 订阅窄接口 fake（记录 Subscribe 顺序与 topic）。
type fakeSub struct {
	events   chan ocdeckevent.Event
	overflow chan struct{}
	mu       sync.Mutex
	closed   bool
}

func (s *fakeSub) C() <-chan ocdeckevent.Event { return s.events }
func (s *fakeSub) Overflow() <-chan struct{}   { return s.overflow }
func (s *fakeSub) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
}
func (s *fakeSub) signalOverflow() {
	select {
	case s.overflow <- struct{}{}:
	default:
	}
}

type fakeBus struct {
	mu    sync.Mutex
	subs  []*fakeSub
	order []string // "subscribe:<topic>" 调用序列（验证 Subscribe 先于基线）
}

func (b *fakeBus) Subscribe(topic ocdeckevent.Topic) EventSubscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &fakeSub{events: make(chan ocdeckevent.Event, 64), overflow: make(chan struct{}, 1)}
	b.subs = append(b.subs, s)
	b.order = append(b.order, "subscribe:"+string(topic))
	return s
}

// fakeClock 可手动推进的时钟（测试不睡真实时间）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// listerFunc 函数形态的 ActiveTaskLister（测试包装用）。
type listerFunc func(context.Context) ([]string, error)

func (f listerFunc) ListAllActiveTaskIDs(ctx context.Context) ([]string, error) {
	return f(ctx)
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// --- 测试装置 ---

// newTestNotifier 构造引擎直测 Notifier（不经 run loop；handleEvent/scan 直接调用）。
func newTestNotifier(ft *fakeTasks, fl *fakeLister, fc *fakeCfgStore, chs []notification.Channel, resolver BaseURLResolver, clk *fakeClock) *Notifier {
	return New(Options{
		Tasks: ft, ListActive: fl, Cfg: fc,
		Channels: chs, ResolveBaseURL: resolver, Now: clk.now,
	})
}

// testConfig 默认全开配置：总开关开、五类别全开、阈值 60、web+bark 启用已配置。
func testConfig() notification.Config {
	c := notification.DefaultConfig()
	c.Enabled = true
	c.Channels.Web.Enabled = true
	c.Channels.Bark.Enabled = true
	c.Channels.Bark.Token = "bark-token-123456"
	return c
}

// activeSnap 构造 active 任务快照（RunStatus 默认 idle，无 pending；instVersion
// 为 "iv-<taskID>"，与事件构造 helper 的 RID 对齐——B3 fencing 一致实例）。
func activeSnap(taskID, name, runStatus string) TaskSnapshot {
	return TaskSnapshot{
		Task:        TaskRef{ID: taskID, Name: name, Status: "active"},
		RunStatus:   runStatus,
		InstVersion: "iv-" + taskID,
	}
}

// runStatusEvent 构造 serve_runtime.run_status_changed 事件。
func runStatusEvent(taskID, from, to string, available bool) ocdeckevent.Event {
	return ocdeckevent.NewServeRuntimeRunStatusChanged("iv-"+taskID, taskID, from, to, available)
}

// sessionErrorEvent 构造 serve_runtime.session_error 事件。
func sessionErrorEvent(taskID, sessionID, message string, statusCode *int, isRetryable *bool) ocdeckevent.Event {
	return ocdeckevent.NewServeRuntimeSessionError("iv-"+taskID, taskID, sessionID, "APIError", message, statusCode, isRetryable)
}

// attentionEvent 构造 serve_runtime.attention_changed 事件。
func attentionEvent(taskID string) ocdeckevent.Event {
	return ocdeckevent.NewServeRuntimeAttentionChanged("iv-"+taskID, taskID)
}

// waitDispatch 等待在途投递 goroutine 结束（engine 直测的同步点）。
func waitDispatch(n *Notifier) { n.dispatchWG.Wait() }

// --- run loop 集成 ---

// TestRunLoop_SubscribeBeforeBaselineThenDrain 验证启动顺序（design D3：先
// Subscribe 再基线快照再 drain）与基线后事件的串行处理、tick 扫描触发。
func TestRunLoop_SubscribeBeforeBaselineThenDrain(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "任务一", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	bus := &fakeBus{}
	n := New(Options{
		Bus: bus, Tasks: ft, ListActive: fl, Cfg: fc,
		Channels:       []notification.Channel{ch},
		ResolveBaseURL: func(string) (string, error) { return "http://127.0.0.1:7777", nil },
		Now:            clk.now, TickEvery: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)
	defer n.Stop()
	<-n.baselineDone

	// Subscribe 两次（serve_runtime + task）先于基线枚举。
	bus.mu.Lock()
	order := append([]string(nil), bus.order...)
	bus.mu.Unlock()
	fl.mu.Lock()
	listerCalls := fl.calls
	fl.mu.Unlock()
	if len(order) != 2 || order[0] != "subscribe:serve_runtime" || order[1] != "subscribe:task" {
		t.Fatalf("subscribe order = %v", order)
	}
	if listerCalls != 1 {
		t.Fatalf("baseline lister calls = %d, want 1", listerCalls)
	}

	// 基线后事件：pending question 事件驱动即时投递（不经 tick，处理完成即可观测）。
	snap := activeSnap("t1", "任务一", "idle")
	snap.Attention.Questions = []application.PendingQuestion{pendingQuestion("q1", "循环内的问题")}
	ft.set(snap)
	bus.mu.Lock()
	sr := bus.subs[0]
	bus.mu.Unlock()
	sr.events <- attentionEvent("t1")
	waitFor(t, func() bool {
		waitDispatch(n)
		return len(ch.sent()) == 1
	}, "question notification should fire from run loop event")
	if got := ch.sent()[0]; got.Category != notification.CategoryQuestion || got.URL != "http://127.0.0.1:7777/#/task/t1" {
		t.Fatalf("intent = %+v", got)
	}

	// 溢出信号 → 对账（枚举被再次调用；已通知的 pending 不补发——对账只播种，
	// 且该 request ID 已在去重集合中）。
	sr.signalOverflow()
	waitFor(t, func() bool {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		return fl.calls >= 2
	}, "overflow should trigger reconcile enumeration")
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 {
		t.Fatalf("sends after overflow reconcile = %d, want 1 (notified pending not re-sent)", got)
	}
}

// waitFor 轮询等待条件成立（不睡定长；上限内失败即报错）。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", msg)
}

// TestRunNormal_OverflowBeforeExecution B2 补齐：普通分支（事件/tick）被选中
// 后、执行前二次检查溢出——已选普通工作被放弃（事件丢弃，属污染队列范畴），
// 先进入对账。确定性直测 runNormal。
func TestRunNormal_OverflowBeforeExecution(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	subServe := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subTask := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subs := []EventSubscription{subServe, subTask}
	n.subs = subs

	fl.mu.Lock()
	callsBefore := fl.calls
	fl.mu.Unlock()

	// 已选中的普通工作 = busy→idle 武装事件（若被执行会武装 idle，远期扫描即
	// 投递）；执行前注入溢出 token → 普通工作被放弃，先对账（对账本身不武装
	// idle——design D3 基线规则）。
	subServe.signalOverflow()
	ev := runStatusEvent("t1", "busy", "idle", true)
	n.runNormal(ctx, subs, &ev, false)

	fl.mu.Lock()
	callsAfter := fl.calls
	fl.mu.Unlock()
	if callsAfter != callsBefore+1 {
		t.Fatalf("overflow must win over selected normal work (reconcile calls %d→%d)", callsBefore, callsAfter)
	}
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("normal work must be abandoned on overflow, sends = %d", got)
	}
	// 事件被丢弃而非延迟执行：远期扫描无 idle 投递（若被执行则已武装）。
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("selected event must be dropped, not executed after reconcile, sends = %d", got)
	}
}

// TestOnOverflow_DrainsPollutedQueue B10：检测到溢出后先排空受污染订阅的既有
// 事件队列——gap 前事件（如旧 busy→idle）不得在对账重建后继续解释（否则重新
// 武装 idle 误发）。
func TestOnOverflow_DrainsPollutedQueue(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	subServe := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subTask := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subs := []EventSubscription{subServe, subTask}
	n.subs = subs

	// 污染队列：两条 gap 前事件（若对账后回放，busy→idle 会重新武装 idle）。
	subServe.events <- runStatusEvent("t1", "idle", "busy", true)
	subServe.events <- runStatusEvent("t1", "busy", "idle", true)
	subServe.signalOverflow()

	n.onOverflow(ctx)
	if got := len(subServe.events); got != 0 {
		t.Fatalf("polluted queue must be drained on overflow, %d events left", got)
	}

	// 排空后即便事件本应武装，也无计时：远期扫描无投递。
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("drained pre-gap events must not re-arm after reconcile, sends = %d", got)
	}
}

// TestDrainOverflow_SingleTokenNoLivelock B11：溢出信号为容量 1 的合并信号，
// 每订阅每轮至多消费一个 token（不循环清空）——持续补 token 的 producer 下
// runOnce 仍逐轮返回（可继续响应取消/tick），不活锁。
func TestDrainOverflow_SingleTokenNoLivelock(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	subServe := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subTask := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subs := []EventSubscription{subServe, subTask}
	n.subs = subs

	// producer 持续补 token（overflow 通道几乎恒满）。
	stopRefill := make(chan struct{})
	refillDone := make(chan struct{})
	go func() {
		defer close(refillDone)
		for {
			select {
			case <-stopRefill:
				return
			case subServe.overflow <- struct{}{}:
			}
		}
	}()

	// 20 轮 runOnce 全部返回且每轮至少一次对账（progress 可观测；单轮至多
	// 前置 + select 内各一次）。
	fl.mu.Lock()
	callsBefore := fl.calls
	fl.mu.Unlock()
	for i := 0; i < 20; i++ {
		if !n.runOnce(ctx, subs, make(chan time.Time)) {
			t.Fatal("runOnce must not exit on live ctx")
		}
	}
	fl.mu.Lock()
	callsAfter := fl.calls
	fl.mu.Unlock()
	close(stopRefill)
	<-refillDone
	if d := callsAfter - callsBefore; d < 20 || d > 40 {
		t.Fatalf("each round must make progress (one or two reconciles), reconciles = %d", d)
	}
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("reconcile rounds must not deliver, sends = %d", got)
	}
}

// TestReconcile_PreservesEpisodeActive B1 补齐（Round 3）：error→overflow→快照
// 仍 idle（非 busy、同实例）时 episodeActive 保留——error episode 持续到 busy，
// idle 门禁依赖 episodeActive 抑制；且 busy 后 episode 可正常关闭。
func TestReconcile_PreservesEpisodeActive(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "idle")
	fl := &fakeLister{ids: []string{"t1"}}
	n.opts.ListActive = fl
	ctx := context.Background()

	// error 开启 episode（idle 计时被取消）。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true)) // 先武装 idle
	n.handleEvent(ctx, sessionErrorEvent("t1", "s1", "boom", nil, nil))
	if st := n.states["t1"]; st.idleSince != nil || !st.episodeActive {
		t.Fatal("prereq: error opens episode and cancels idle")
	}

	// overflow 对账（快照仍 idle、同实例）：episode 存续语义保留。
	n.onOverflow(ctx)
	st := n.states["t1"]
	if st == nil || !st.episodeActive {
		t.Fatalf("surviving error episode must keep episodeActive across reconcile: %+v", st)
	}
	if st.errorDeadline != nil {
		t.Fatal("error timer must not be restored by reconcile")
	}
	// idle 被抑制：episode 存续期间远期扫描无 idle 投递。
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	for _, in := range ch.sent() {
		if in.Category == notification.CategoryIdle {
			t.Fatal("idle must stay suppressed while error episode survives reconcile")
		}
	}

	// busy 关闭 episode → 新 busy→idle 可武装并正常触发（保留不等于卡死）。
	busySnap := activeSnap("t1", "构建服务", "busy")
	ft.set(busySnap)
	n.handleEvent(ctx, runStatusEvent("t1", "idle", "busy", true))
	if st := n.states["t1"]; st.episodeActive {
		t.Fatal("busy must close episode")
	}
	ft.set(activeSnap("t1", "构建服务", "idle"))
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(60 * time.Second)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 || ch.sent()[0].Category != notification.CategoryIdle {
		t.Fatalf("idle must fire after episode closed, got %+v", ch.sent())
	}
}

// TestRunLoop_CancelExitsUnderPerpetualOverflow B11 补齐（Round 3）：溢出信号
// 恒可消费（closed channel——每次非阻塞接收立即成功，是「producer 持续补
// token」的确定性同步等价）下，取消 ctx 后 run loop 必须退出——runOnce 先检查
// 取消再处理 overflow，Stop 不得永久等待。
func TestRunLoop_CancelExitsUnderPerpetualOverflow(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "任务一", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	bus := &fakeBus{}
	n := New(Options{
		Bus: bus, Tasks: ft, ListActive: fl, Cfg: fc,
		Channels:       []notification.Channel{ch},
		ResolveBaseURL: func(string) (string, error) { return "http://127.0.0.1:7777", nil },
		Now:            clk.now, TickEvery: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	<-n.baselineDone

	// serve_runtime 订阅的溢出通道关闭：恒有 token 可消费（确定性恒满）。
	bus.mu.Lock()
	sr := bus.subs[0]
	bus.mu.Unlock()
	close(sr.overflow)

	cancel() // 取消优先：loop 必须退出（修复前 pre-drain 恒命中，永不进 select）
	exited := make(chan struct{})
	go func() {
		n.Stop()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("run loop must exit after cancel under perpetual overflow")
	}
}

// TestDrainQueued_BoundedUnderPerpetualEvents B12：事件队列恒可消费（closed
// channel——持续 refill 的确定性同步等价）时 drainQueued 有界返回（每通道消费
// 上限=订阅缓冲容量，循环内检查取消），不阻塞对账。
func TestDrainQueued_BoundedUnderPerpetualEvents(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "任务一", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	subServe := &fakeSub{events: make(chan ocdeckevent.Event, 64), overflow: make(chan struct{}, 1)}
	subTask := &fakeSub{events: make(chan ocdeckevent.Event, 64), overflow: make(chan struct{}, 1)}
	n.subs = []EventSubscription{subServe, subTask}

	// 事件通道关闭：drainQueued 的每次非阻塞接收立即成功（修复前无限循环）。
	close(subServe.events)
	subServe.signalOverflow()

	fl.mu.Lock()
	callsBefore := fl.calls
	fl.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.onOverflow(ctx) // 内部 drainQueued 必须有界返回
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainQueued must be bounded under perpetual events")
	}
	fl.mu.Lock()
	callsAfter := fl.calls
	fl.mu.Unlock()
	if callsAfter != callsBefore+1 {
		t.Fatalf("reconcile must run after bounded drain (calls %d→%d)", callsBefore, callsAfter)
	}
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("drained/reconciled rounds must not deliver, sends = %d", got)
	}
}

// TestReconcile_PruneResolvedDedup B13：成功对账的去重 map 仅由当前快照 pending
// 构建——了结的 request ID（了结事件在溢出缺口中丢失、未经事件路径剪枝）在对账
// 时剪除（spec「去重集合以当前 pending 集合为上界」）；失败对账不得修改旧 map。
func TestReconcile_PruneResolvedDedup(t *testing.T) {
	n, ft, _, ch, clk := triggerFixture(t, "idle")
	fl := &fakeLister{ids: []string{"t1"}}
	n.opts.ListActive = fl
	ctx := context.Background()

	// q1 通知（仍在 pending）。
	snapQ1 := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "旧问题")}, nil)
	ft.set(snapQ1)
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 1 {
		t.Fatalf("prereq q1 notified, sends = %d", got)
	}

	// q1 了结但了结事件丢失在溢出缺口中（不经事件路径剪枝）：overflow 对账
	//（当前 pending 为空）→ 新 map 为空，q1 剪除。
	ft.set(attentionSnapWith("idle", nil, nil))
	n.onOverflow(ctx)
	if _, kept := n.states["t1"].notifiedQuestions["q1"]; kept {
		t.Fatal("resolved request id must be pruned at reconcile (pending-bound)")
	}

	// 剪枝后 q1 以同 ID 复现（新的一轮 pending）→ 重新通知（不被残留去重抑制）。
	snapAgain := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "重新出现")}, nil)
	ft.set(snapAgain)
	n.handleEvent(ctx, attentionEvent("t1"))
	waitDispatch(n)
	if got := len(ch.sent()); got != 2 || !containsBody(ch.sent(), "重新出现") {
		t.Fatalf("re-appearing resolved id must notify again after prune, sends = %d", got)
	}

	// 失败对账不得修改旧 map：枚举失败 → reconciling，q1（当前 pending）留存。
	fl.set(nil, errNotFound)
	ft.set(attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "重新出现")}, nil))
	n.onOverflow(ctx)
	if _, kept := n.states["t1"].notifiedQuestions["q1"]; !kept {
		t.Fatal("failed reconcile must not modify old dedup maps")
	}
	clk.add(time.Hour) // reconciling 期间重试仍失败：无投递
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 2 {
		t.Fatalf("reconciling must suppress sends, sends = %d", got)
	}
}

// TestStop_Idempotent Stop 幂等且可重复调用；未 Start 的 Notifier Stop 安全。
func TestStop_Idempotent(t *testing.T) {
	n := New(Options{Tasks: newFakeTasks(), ListActive: &fakeLister{}, Cfg: &fakeCfgStore{cfg: testConfig()}, Now: newFakeClock().now})
	n.Stop() // 未 Start：安全
	n.Stop()

	n2 := New(Options{
		Bus: &fakeBus{}, Tasks: newFakeTasks(), ListActive: &fakeLister{},
		Cfg: &fakeCfgStore{cfg: testConfig()}, Now: newFakeClock().now,
	})
	n2.Start(context.Background())
	n2.Stop()
	n2.Stop() // 幂等
}

// TestLifecycle_StopBeforeStartB8 B8：Stop-before-Start 后 Start 为 no-op——
// 不得启动不可停止的 run loop（终态生命周期状态机）。
func TestLifecycle_StopBeforeStartB8(t *testing.T) {
	n := New(Options{
		Bus: &fakeBus{}, Tasks: newFakeTasks(), ListActive: &fakeLister{ids: []string{"t1"}},
		Cfg: &fakeCfgStore{cfg: testConfig()}, Now: newFakeClock().now,
	})
	n.Stop() // 消耗终态
	n.Start(context.Background())
	if n.lifecycleState() != lcDead {
		t.Fatalf("Start after Stop-before-Start must be no-op, lifecycle = %v", n.lifecycleState())
	}
	select {
	case <-n.baselineDone:
		t.Fatal("no-op Start must not run the loop (baselineDone must stay open)")
	case <-time.After(50 * time.Millisecond):
	}
	n.Stop() // 幂等且安全
}

// TestLifecycle_ConcurrentStartStop B8：并发 Start/Stop 交叉不 panic、不双启、
// 最终全部停止（-race 下验证同步正确性）。
func TestLifecycle_ConcurrentStartStop(t *testing.T) {
	n := New(Options{
		Bus: &fakeBus{}, Tasks: newFakeTasks(), ListActive: &fakeLister{ids: []string{"t1"}},
		Cfg: &fakeCfgStore{cfg: testConfig()}, Now: newFakeClock().now, TickEvery: time.Millisecond,
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(start bool) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if start {
					n.Start(context.Background())
				} else {
					n.Stop()
				}
			}
		}(i%2 == 0)
	}
	wg.Wait()
	n.Stop()
	if n.lifecycleState() != lcDead {
		t.Fatalf("final Stop must reach dead, got %v", n.lifecycleState())
	}
}

// TestRunLoop_OverflowDrainedBeforeSelect B2：overflow 与到期 tick/事件同时
// ready 时，溢出先被消费（runOnce 前置 drain），对账取消计时后 tick/事件不再
// 产生投递——不受 select 随机性影响（确定性直测 runOnce）。
func TestRunLoop_OverflowDrainedBeforeSelect(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	// 武装 idle（iv-t1）并推进到届满；同时备好：溢出 token + 到期 tick + 事件。
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(70 * time.Second)

	subServe := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subTask := &fakeSub{events: make(chan ocdeckevent.Event, 8), overflow: make(chan struct{}, 1)}
	subServe.signalOverflow()
	subServe.events <- runStatusEvent("t1", "idle", "busy", true) // 同轮 ready 的事件
	tick := make(chan time.Time, 1)
	tick <- clk.now()

	fl.mu.Lock()
	callsBefore := fl.calls
	fl.mu.Unlock()
	if !n.runOnce(ctx, []EventSubscription{subServe, subTask}, tick) {
		t.Fatal("runOnce must continue on live ctx")
	}
	fl.mu.Lock()
	callsAfter := fl.calls
	fl.mu.Unlock()
	if callsAfter != callsBefore+1 {
		t.Fatalf("overflow must be consumed before select and trigger reconcile (calls %d→%d)", callsBefore, callsAfter)
	}
	// 对账已取消计时：本轮 select 无论消费到 tick 还是事件，都不得产生 idle 投递。
	waitDispatch(n)
	for _, in := range ch.sent() {
		if in.Category == notification.CategoryIdle {
			t.Fatal("overflow-reconciled timers must not fire idle delivery")
		}
	}
	if st := n.states["t1"]; st == nil || st.idleSince != nil {
		t.Fatalf("reconcile must cancel armed timers, state = %+v", st)
	}
}

// TestDisabled_IgnoresOverflowAndStaysTerminal B7：disabled 为终态——overflow
// 信号被消费但忽略，枚举恢复后也不得迁回 running（待进程重启恢复）。
func TestDisabled_IgnoresOverflowAndStaysTerminal(t *testing.T) {
	ft := newFakeTasks(activeSnap("t1", "构建服务", "idle"))
	fl := &fakeLister{err: errNotFound} // 基线枚举失败
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	n := newTestNotifier(ft, fl, fc, []notification.Channel{ch},
		func(string) (string, error) { return "http://127.0.0.1:7777", nil }, clk)
	ctx := context.Background()

	n.initBaseline(ctx) // 枚举失败 → disabled
	if n.mode != modeDisabled {
		t.Fatalf("mode = %v, want disabled", n.mode)
	}

	n.onOverflow(ctx) // 直接调用：disabled 下必须忽略
	if n.mode != modeDisabled {
		t.Fatalf("disabled must ignore overflow, mode = %v", n.mode)
	}

	// 枚举恢复（lister 修复）：任何路径不得迁回 running。
	fl.set([]string{"t1"}, nil)
	n.scan(ctx)
	if n.mode != modeDisabled {
		t.Fatalf("disabled is terminal until process restart, mode = %v", n.mode)
	}
	n.handleEvent(ctx, runStatusEvent("t1", "busy", "idle", true))
	clk.add(time.Hour)
	n.scan(ctx)
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("disabled notifier must never deliver, sends = %d", got)
	}
}

// TestRunLoop_DrainEventsQueuedDuringBaseline B9：基线期间排队的事件在基线后
// drain：与基线快照一致的 pending 只播种不补发（若事件先于基线处理则会误发）。
func TestRunLoop_DrainEventsQueuedDuringBaseline(t *testing.T) {
	ft := newFakeTasks()
	snap := attentionSnapWith("idle", []application.PendingQuestion{pendingQuestion("q1", "基线期间排队")}, nil)
	snap.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	ft.set(snap)
	fl := &fakeLister{ids: []string{"t1"}}
	fc := &fakeCfgStore{cfg: testConfig()}
	ch := &fakeChannel{name: "web", caps: notification.CapReplace}
	clk := newFakeClock()
	bus := &fakeBus{}
	n := New(Options{
		Bus: bus, Tasks: ft, ListActive: fl, Cfg: fc,
		Channels:       []notification.Channel{ch},
		ResolveBaseURL: func(string) (string, error) { return "http://127.0.0.1:7777", nil },
		Now:            clk.now, TickEvery: time.Millisecond,
	})

	// 基线阻塞钩子：lister 第一次调用阻塞，期间向订阅缓冲排队事件（先于基线
	// 完成到达——drain 语义的确定性构造）。
	gate := make(chan struct{})
	baselineStarted := make(chan struct{})
	var listOnce sync.Once
	n.opts.ListActive = listerFunc(func(ctx context.Context) ([]string, error) {
		listOnce.Do(func() { close(baselineStarted) })
		<-gate
		return fl.ListAllActiveTaskIDs(ctx)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)
	defer n.Stop()
	<-baselineStarted

	bus.mu.Lock()
	sr := bus.subs[0]
	bus.mu.Unlock()
	sr.events <- attentionEvent("t1") // 基线期间排队
	close(gate)
	<-n.baselineDone

	// drain 的可观测同步点：排队事件被处理时必读一次组合快照（基线已读 1 次），
	// 读数达到 2 即证明事件已消费——不靠 Sleep 推断。
	waitFor(t, func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.reads >= 2
	}, "queued event must be drained after baseline")
	waitDispatch(n)
	if got := len(ch.sent()); got != 0 {
		t.Fatalf("event queued during baseline must be seeded (not backfilled) after drain, sends = %d", got)
	}

	// drain 后新事件正常通知。
	snap2 := attentionSnapWith("idle", []application.PendingQuestion{
		pendingQuestion("q1", "基线期间排队"), pendingQuestion("q2", "基线后新增"),
	}, nil)
	snap2.Task = TaskRef{ID: "t1", Name: "任务一", Status: "active"}
	ft.set(snap2)
	sr.events <- attentionEvent("t1")
	waitFor(t, func() bool {
		waitDispatch(n)
		return len(ch.sent()) == 1
	}, "post-drain event must notify")
}
