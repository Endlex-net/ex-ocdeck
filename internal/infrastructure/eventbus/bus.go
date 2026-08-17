// Package eventbus 实现进程内 topic 化 pub/sub 总线。
//
// 语义（逐字对齐 openspec/changes/sse-active-sessions design D1）：
//   - Publish 非阻塞：某订阅者缓冲满时丢弃该事件、置位该订阅者溢出信号、记日志，
//     不阻塞发布方、不影响其他订阅者；Publish 溢出不回滚任何业务。
//   - Subscribe(topic) 按**单个** topic 订阅，返回带缓冲 channel 的 *Sub。
//     多 topic 由调用方多次 Subscribe 后自行 fan-in；本包不提供 fan-in helper。
//   - Overflow() 至少一次可观察：置位后消费者读取后可清零重挂。
//   - Close() 后订阅者不再收到事件。
//   - 无持久化、无跨 topic 顺序保证、无第三方依赖；仅 stdlib。
//   - Bus 不做 schema 校验：未知 Type 的事件仍投递给订阅者且 Publish 不失败。
//
// Event / Topic / Type 常量、typed payload 构造器全部 import 自
// ocdeck/internal/domain/event（已落地，本包不重复定义）。
package eventbus

import (
	"log"
	"sync"

	ocdeckevent "ocdeck/internal/domain/event"
)

// SubscriberBufferCapacity 单个订阅者缓冲 channel 容量。
//
// 取值 64：design D1 指出「缓冲（如 64）满时丢弃该事件」为默认参考容量，
// 非阻塞优先于不丢——缓冲满才走溢出信号 + 自愈路径。容量为导出常量，
// 便于后续按实际负载观测调整（不改语义）。
const SubscriberBufferCapacity = 64

// Bus 进程内 topic 化 pub/sub 总线。
//
// 零值可用；推荐经 New 构造以便后续注入可调参数。
type Bus struct {
	mu  sync.RWMutex
	subs map[ocdeckevent.Topic]map[*Sub]struct{}
}

// New 构造一个空的总线实例。
func New() *Bus {
	return &Bus{subs: make(map[ocdeckevent.Topic]map[*Sub]struct{})}
}

// Publish 向所有订阅了 ev.Topic 的订阅者非阻塞投递事件。
//
// 语义：某订阅者缓冲满时丢弃该事件、置位其 Overflow() 信号、记日志，
// 不阻塞发布方、不影响其他订阅者；Publish 永不返回错误（Bus 不做 schema 校验，
// 未知 Type 仍投递）。ev.Topic 为零值时投递给该零值 topic 的订阅者（一般无订阅者）。
func (b *Bus) Publish(ev ocdeckevent.Event) {
	b.mu.RLock()
	subs := b.subs[ev.Topic]
	// 复制引用集合以在锁外遍历，避免投递期间持锁。引用集合不可变（Close 后从表中
	// 删除时替换为新 map），故 RLock 期间遍历副本安全。
	refs := make([]*Sub, 0, len(subs))
	for s := range subs {
		refs = append(refs, s)
	}
	b.mu.RUnlock()

	for _, s := range refs {
		s.deliver(ev)
	}
}

// Subscribe 按 topic 订阅，返回带缓冲 channel 的 *Sub。
// 多 topic 由调用方多次 Subscribe 后自行 fan-in。
//
// 返回的 Sub 必须在使用结束后调用 Close 退订，避免引用泄漏。
func (b *Bus) Subscribe(topic ocdeckevent.Topic) *Sub {
	s := newSub(SubscriberBufferCapacity, b, topic)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[ocdeckevent.Topic]map[*Sub]struct{})
	}
	set, ok := b.subs[topic]
	if !ok {
		set = make(map[*Sub]struct{})
		b.subs[topic] = set
	}
	set[s] = struct{}{}
	b.mu.Unlock()

	return s
}

// remove 在写锁下从 topic 的订阅集合移除 s。
func (b *Bus) remove(topic ocdeckevent.Topic, s *Sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[topic]
	if !ok {
		return
	}
	if _, exists := set[s]; !exists {
		return
	}
	// 复制新集合替代修改，保证并发 Publish 持有的 RLock 读到的旧集合快照稳定。
	next := make(map[*Sub]struct{}, len(set)-1)
	for k := range set {
		if k != s {
			next[k] = struct{}{}
		}
	}
	b.subs[topic] = next
}

// Sub 单 topic 订阅者。
//
// 方法集：C() 返回事件 channel；Overflow() 返回溢出信号 channel（至少一次可观察，
// 消费者读取后可清零重挂）；Close() 退订并关闭 channel，此后不再收到事件。
type Sub struct {
	topic ocdeckevent.Topic
	bus   *Bus

	mu       sync.Mutex
	ch       chan ocdeckevent.Event
	overflow chan struct{}
	closed   bool
}

func newSub(buf int, bus *Bus, topic ocdeckevent.Topic) *Sub {
	return &Sub{
		ch:       make(chan ocdeckevent.Event, buf),
		overflow: make(chan struct{}, 1),
		bus:      bus,
		topic:    topic,
	}
}

// C 返回事件 channel。Close 后该 channel 被关闭。
func (s *Sub) C() <-chan ocdeckevent.Event {
	return s.ch
}

// Overflow 返回溢出信号 channel。
//
// 语义：发布期间若该订阅者缓冲满则丢弃事件并**非阻塞**置位本信号
// （send 到带缓冲为 1 的 channel，已满则跳过，保证「至少一次可观察」）。
// 消费者读取后可清零重挂——下次溢出会再次置位。
func (s *Sub) Overflow() <-chan struct{} {
	return s.overflow
}

// ClearOverflow 非阻塞消费并清零溢出信号，便于订阅方重挂后再观察下一次溢出。
//
// 这是 Overflow 「至少一次可观察」语义的配套 helper：消费者可在重挂前调用，
// 避免旧信号长期滞留。非必须调用——消费者也可直接对 Overflow() 做 select。
func (s *Sub) ClearOverflow() {
	select {
	case <-s.overflow:
	default:
	}
}

// Close 退订并关闭事件 channel。Close 后不再收到事件。重复调用安全。
func (s *Sub) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	if s.bus != nil {
		s.bus.remove(s.topic, s)
	}
}

// deliver 非阻塞投递事件；缓冲满则置位溢出信号并记日志，不阻塞调用方。
//
// 全程持有 s.mu：Close 也持有 s.mu 后 close(s.ch)，二者互斥，保证不会
// 向已关闭 channel 发送。send 用 select+default 非阻塞，永不阻塞 Close。
func (s *Sub) deliver(ev ocdeckevent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	select {
	case s.ch <- ev:
	default:
		// 缓冲满：丢弃本事件、非阻塞置位溢出信号、记日志。
		// overflow channel 容量为 1，已满则跳过——保证至少一次可观察。
		select {
		case s.overflow <- struct{}{}:
		default:
		}
		log.Printf("eventbus: subscriber buffer full, dropping event topic=%s type=%s rid=%s",
			ev.Topic, ev.Type, ev.RID)
	}
}

// subscriberCount 返回当前订阅 topic 的存活订阅者数量（测试可观测手段）。
//
// 仅用于测试与诊断；生产代码不应依赖。写锁下读取保证一致快照。
func (b *Bus) subscriberCount(topic ocdeckevent.Topic) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[topic])
}