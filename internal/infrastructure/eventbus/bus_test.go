package eventbus

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ocdeckevent "ocdeck/internal/domain/event"
)

func newTestBus() *Bus { return New() }

func drainTimeout(ch <-chan ocdeckevent.Event, timeout time.Duration) (ocdeckevent.Event, bool) {
	select {
	case e := <-ch:
		return e, true
	case <-time.After(timeout):
		return ocdeckevent.Event{}, false
	}
}

// 发布/订阅语义：单订阅者收到对应 topic 的事件。
func TestPublishSubscribeSingle(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	ev := ocdeckevent.NewTaskCreated("tsk_1")
	bus.Publish(ev)

	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("timed out waiting for event")
	}
	if got.Type != ocdeckevent.TypeTaskCreated || got.RID != "tsk_1" {
		t.Fatalf("got = %+v", got)
	}
}

// 按 topic 路由：只投递给订阅该 topic 的订阅者，其他 topic 订阅者不收到。
func TestTopicRouting(t *testing.T) {
	bus := newTestBus()
	taskSub := bus.Subscribe(ocdeckevent.TopicTask)
	defer taskSub.Close()
	sessSub := bus.Subscribe(ocdeckevent.TopicSession)
	defer sessSub.Close()

	bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))

	if _, ok := drainTimeout(taskSub.C(), time.Second); !ok {
		t.Fatalf("task subscriber did not receive task event")
	}
	// session 订阅者不应收到
	if _, ok := drainTimeout(sessSub.C(), 50*time.Millisecond); ok {
		t.Fatalf("session subscriber should not receive task event")
	}

	bus.Publish(ocdeckevent.NewSessionClaimed("sess_1", "tsk_1"))
	if _, ok := drainTimeout(sessSub.C(), time.Second); !ok {
		t.Fatalf("session subscriber did not receive session event")
	}
	if _, ok := drainTimeout(taskSub.C(), 50*time.Millisecond); ok {
		t.Fatalf("task subscriber should not receive session event")
	}
}

// 同一 topic 多订阅者都收到事件（fan-out）。
func TestMultipleSubscribersSameTopic(t *testing.T) {
	bus := newTestBus()
	s1 := bus.Subscribe(ocdeckevent.TopicTask)
	defer s1.Close()
	s2 := bus.Subscribe(ocdeckevent.TopicTask)
	defer s2.Close()

	bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))

	if _, ok := drainTimeout(s1.C(), time.Second); !ok {
		t.Fatalf("s1 did not receive")
	}
	if _, ok := drainTimeout(s2.C(), time.Second); !ok {
		t.Fatalf("s2 did not receive")
	}
}

// Close 后订阅者不再收到事件；Close 后 C() 被关闭可读到零值。
func TestCloseStopsDelivery(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)

	sub.Close()

	// Close 后 channel 应被关闭。
	if _, ok := <-sub.C(); ok {
		t.Fatalf("channel should be closed after Close")
	}

	// 发布不应 panic，且不应向已关闭 channel 投递。
	bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))

	// 重复 Close 安全。
	sub.Close()
}

// 退订后后续 Publish 不再投递给该订阅者，且订阅从路由表实际移除。
func TestUnsubscribeRemovesFromRouting(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	if got := bus.subscriberCount(ocdeckevent.TopicTask); got != 1 {
		t.Fatalf("subscriberCount before Close = %d, want 1", got)
	}

	sub.Close()

	// 路由表订阅数实际减少（验证 Close 真正退订，而非仅关闭 channel）。
	if got := bus.subscriberCount(ocdeckevent.TopicTask); got != 0 {
		t.Fatalf("subscriberCount after Close = %d, want 0 (Close must unsubscribe)", got)
	}

	bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))

	// channel 已关闭，读到零值即表示未投递
	got, ok := <-sub.C()
	if ok {
		t.Fatalf("unexpected event after unsubscribe: %+v", got)
	}
}

// 退订只移除被关闭的订阅者，其他订阅者保留在路由表。
func TestUnsubscribeOnlyRemovesTarget(t *testing.T) {
	bus := newTestBus()
	keep := bus.Subscribe(ocdeckevent.TopicTask)
	defer keep.Close()
	drop := bus.Subscribe(ocdeckevent.TopicTask)

	if got := bus.subscriberCount(ocdeckevent.TopicTask); got != 2 {
		t.Fatalf("subscriberCount = %d, want 2", got)
	}

	drop.Close()

	if got := bus.subscriberCount(ocdeckevent.TopicTask); got != 1 {
		t.Fatalf("subscriberCount after dropping one = %d, want 1", got)
	}

	// 保留的订阅者仍能收到事件
	bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))
	if _, ok := drainTimeout(keep.C(), time.Second); !ok {
		t.Fatalf("kept subscriber did not receive after another was closed")
	}
	// 已关闭的订阅者不收到（channel 已关闭）
	if ev, ok := <-drop.C(); ok {
		t.Fatalf("closed subscriber received: %+v", ev)
	}
}

// 重复 Close 安全且不重复修改路由表。
func TestCloseIdempotent(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	sub.Close()
	sub.Close() // 重复 Close 不 panic

	if got := bus.subscriberCount(ocdeckevent.TopicTask); got != 0 {
		t.Fatalf("subscriberCount = %d, want 0 after repeated Close", got)
	}
}

// 缓冲溢出时发布不阻塞、溢出信号至少一次可见、其余订阅者不受影响。
func TestOverflowNonBlockingAndSignal(t *testing.T) {
	bus := newTestBus()
	overflowSub := bus.Subscribe(ocdeckevent.TopicTask)
	defer overflowSub.Close()
	healthySub := bus.Subscribe(ocdeckevent.TopicTask)
	defer healthySub.Close()

	// 填满 overflowSub 的缓冲（不消费），再多发一条以触发溢出。
	fill := SubscriberBufferCapacity + 5
	for i := 0; i < fill; i++ {
		// Publish 必须不阻塞：用很短超时 wrapper 验证
		done := make(chan struct{})
		go func() {
			bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("Publish blocked on full subscriber at i=%d", i)
		}
	}

	// overflowSub 的 Overflow 信号至少一次可见
	select {
	case <-overflowSub.Overflow():
	case <-time.After(time.Second):
		t.Fatalf("overflow signal not observed")
	}

	// healthySub 仍应收到全部事件（缓冲容量足够容纳 fill <= 64+5 = 69，但 healthySub
	// 缓冲也是 64，故 healthySub 自身也会溢出）。为避免健康订阅者自身溢出干扰断言，
	// 改用持续消费的健康订阅者验证 fan-out 未受 overflowSub 影响：
	bus2 := newTestBus()
	blocker := bus2.Subscribe(ocdeckevent.TopicTask)
	_ = blocker // 不消费，制造溢出
	consumer := bus2.Subscribe(ocdeckevent.TopicTask)
	defer consumer.Close()

	// 填满 blocker 缓冲并溢出
	for i := 0; i < SubscriberBufferCapacity+3; i++ {
		bus2.Publish(ocdeckevent.NewTaskCreated("tsk_x"))
	}
	// consumer 应能收到事件（证明 blocker 的溢出未影响 consumer）
	if _, ok := drainTimeout(consumer.C(), time.Second); !ok {
		t.Fatalf("healthy consumer did not receive while another sub overflowed")
	}
	blocker.Close()
}

// 溢出信号可清零重挂（ClearOverflow 后再次溢出可再次观察）。
func TestOverflowClearAndRearm(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	// 填满并溢出
	for i := 0; i < SubscriberBufferCapacity+2; i++ {
		bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))
	}
	select {
	case <-sub.Overflow():
	case <-time.After(time.Second):
		t.Fatalf("first overflow not observed")
	}

	// 排空 channel 与溢出信号，重挂
	sub.ClearOverflow()
	for len(sub.C()) > 0 {
		<-sub.C()
	}

	// 再次溢出应再次观察到
	for i := 0; i < SubscriberBufferCapacity+1; i++ {
		bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))
	}
	select {
	case <-sub.Overflow():
	case <-time.After(time.Second):
		t.Fatalf("re-armed overflow not observed")
	}
}

// 未知 Type 仍投递给订阅者且 Publish 不失败。
func TestUnknownTypeStillDelivered(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	unknown := ocdeckevent.Event{
		Topic:   ocdeckevent.TopicTask,
		Type:    "totally.unknown.type.v2",
		RID:     "rid_1",
		Payload: nil,
	}
	// Publish 不应 panic / 不应返回错误（签名无返回值，故只验证不 panic）
	bus.Publish(unknown)

	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("unknown type event not delivered")
	}
	if got.Type != unknown.Type || got.RID != "rid_1" {
		t.Fatalf("got = %+v", got)
	}
}

// task.status_changed Payload 含 from/to。
func TestTaskStatusChangedPayloadFromTo(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicTask)
	defer sub.Close()

	bus.Publish(ocdeckevent.NewTaskStatusChanged("tsk_1", "active", "suspended"))

	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("not received")
	}
	p, ok := got.Payload.(ocdeckevent.TaskStatusChangedPayload)
	if !ok {
		t.Fatalf("payload type = %T", got.Payload)
	}
	if p.From != "active" || p.To != "suspended" {
		t.Fatalf("payload = %+v", p)
	}
}

// session.claimed RID 为 session 主键且 Payload 含 task_id。
func TestSessionClaimedRIDAndPayload(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicSession)
	defer sub.Close()

	bus.Publish(ocdeckevent.NewSessionClaimed("sess_42", "tsk_1"))

	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("not received")
	}
	if got.RID != "sess_42" {
		t.Fatalf("RID = %q, want sess_42 (session primary key)", got.RID)
	}
	p, ok := got.Payload.(ocdeckevent.SessionOwnerPayload)
	if !ok || p.TaskID != "tsk_1" {
		t.Fatalf("payload = %+v", got.Payload)
	}
}

// serve_runtime.* RID 为 instanceID 且 Payload 含 task_id。
func TestServeRuntimeRIDAndPayload(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicServeRuntime)
	defer sub.Close()

	// attention_changed
	bus.Publish(ocdeckevent.NewServeRuntimeAttentionChanged("inst_99", "tsk_1"))
	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("attention_changed not received")
	}
	if got.RID != "inst_99" {
		t.Fatalf("RID = %q, want instanceID", got.RID)
	}
	if p, ok := got.Payload.(ocdeckevent.ServeRuntimeTaskPayload); !ok || p.TaskID != "tsk_1" {
		t.Fatalf("attention payload = %+v", got.Payload)
	}

	// run_status_changed
	bus.Publish(ocdeckevent.NewServeRuntimeRunStatusChanged("inst_99", "tsk_1", "idle", "busy", true))
	got, ok = drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("run_status_changed not received")
	}
	if got.RID != "inst_99" {
		t.Fatalf("RID = %q, want instanceID", got.RID)
	}
	p2, ok := got.Payload.(ocdeckevent.ServeRuntimeRunStatusChangedPayload)
	if !ok || p2.TaskID != "tsk_1" || p2.From != "idle" || p2.To != "busy" || !p2.Available {
		t.Fatalf("run_status payload = %+v", got.Payload)
	}
}

// resync.requested 控制事件投递给 control topic。
func TestResyncRequestedControlTopic(t *testing.T) {
	bus := newTestBus()
	sub := bus.Subscribe(ocdeckevent.TopicControl)
	defer sub.Close()

	bus.Publish(ocdeckevent.NewResyncRequested())
	got, ok := drainTimeout(sub.C(), time.Second)
	if !ok {
		t.Fatalf("resync not received")
	}
	if got.Topic != ocdeckevent.TopicControl || got.Type != ocdeckevent.TypeResyncRequested {
		t.Fatalf("got = %+v", got)
	}
	// resync.requested 无主体，RID 固定为空（design 事件目录规定）。
	if got.RID != "" {
		t.Fatalf("resync.requested RID = %q, want empty", got.RID)
	}
}

// Publish 与 Subscribe/Close 并发 race 测试（-race 验证）。
func TestConcurrentPublishSubscribeClose(t *testing.T) {
	bus := newTestBus()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 并发发布者：多个 topic
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				bus.Publish(ocdeckevent.NewTaskCreated("tsk_1"))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				bus.Publish(ocdeckevent.NewSessionClaimed("sess_1", "tsk_1"))
			}
		}
	}()

	// 并发订阅/退订循环
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s := bus.Subscribe(ocdeckevent.TopicTask)
				// 立即消费一些再关闭，模拟真实订阅者生命周期
				select {
				case <-s.C():
				case <-time.After(time.Millisecond):
				}
				s.Close()
			}
		}
	}()

	// 让 race detector 有足够窗口捕获冲突
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 持续发布 + 持续消费 + 持续退订并发，race 验证 channel 关闭与投递不冲突。
func TestConcurrentProducerConsumerClose(t *testing.T) {
	bus := newTestBus()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var delivered int64

	// 持续创建订阅者、消费、关闭
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s := bus.Subscribe(ocdeckevent.TopicServeRuntime)
					// 消费若干
					drained := 0
					timer := time.NewTimer(time.Millisecond)
					for {
						select {
						case _, ok := <-s.C():
							if !ok {
								timer.Stop()
								goto done
							}
							atomic.AddInt64(&delivered, 1)
							drained++
							if drained > 5 {
								timer.Stop()
								goto done
							}
						case <-timer.C:
							goto done
						}
					}
				done:
					s.Close()
				}
			}
		}()
	}

	// 持续发布
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				bus.Publish(ocdeckevent.NewServeRuntimeAttentionChanged("inst_1", "tsk_1"))
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}