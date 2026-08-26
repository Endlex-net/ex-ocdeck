// events.go 定义事件消费侧窄端口与注入（P1.6.5 bus wiring / design.md D3、D0:55）。
//
// P1.6 仅定义端口并存储注入值；SSE 端点消费在 Phase 2（P2.1+）建立。
// api MUST NOT import infrastructure/eventbus：*eventbus.Bus.Subscribe 返回具体 *Sub
// 而非本包 EventSubscription（Go 接口要求返回类型精确匹配），由 cmd 组合根的薄适配器
// 完成适配；*Sub 自身结构性满足 EventSubscription（C/Overflow/Close，bus.go:132/141/157）。
package api

import (
	ocdeckevent "ocdeck/internal/domain/event"
)

// EventSubscriber 为 Phase 2 SSE 的消费侧端口（P2.1+）。
// Subscribe 按单个 topic 订阅；多 topic 由调用方多次 Subscribe 后自行 fan-in。
type EventSubscriber interface {
	Subscribe(topic ocdeckevent.Topic) EventSubscription
}

// EventSubscription 为单 topic 订阅句柄（消费面方法集）。
type EventSubscription interface {
	// C 返回事件 channel；Close 后该 channel 被关闭。
	C() <-chan ocdeckevent.Event
	// Overflow 返回溢出信号 channel（至少一次可观察，消费者读取后可清零重挂）。
	Overflow() <-chan struct{}
	// Close 退订并关闭事件 channel。
	Close()
}

// SetEventSubscriber 注入事件订阅端口（P1.6.5）。本阶段仅存储，无路由依赖；
// 与 SetTaskBackend 等注入项一致，须在 RebuildRoutes 前调用。
func (s *Server) SetEventSubscriber(sub EventSubscriber) {
	s.eventSubscriber = sub
}
