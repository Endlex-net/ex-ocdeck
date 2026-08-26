// events.go P1.6.5 bus wiring 适配层：composition root 把单例 *eventbus.Bus 同时接入
// 生产侧（application.Publisher，经 apptask.Options.Publish 注入 LifecycleService）与
// 消费侧（api.EventSubscriber）。api 与 eventbus 互不 import（design.md D0:55 依赖方向），
// 返回类型精确匹配由本适配器完成。
package main

import (
	"ocdeck/internal/api"
	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/eventbus"
)

// 编译期断言：*eventbus.Bus 结构性满足 application.Publisher（Publish 非阻塞，指针接收者）。
var _ application.Publisher = (*eventbus.Bus)(nil)

// 编译期断言：适配器满足 api.EventSubscriber。
var _ api.EventSubscriber = eventSubscriberAdapter{}

// eventSubscriberAdapter 把 *eventbus.Bus 适配为 api.EventSubscriber。
// bus.Subscribe 返回的 *eventbus.Sub 结构性满足 api.EventSubscription。
type eventSubscriberAdapter struct {
	bus *eventbus.Bus
}

func (a eventSubscriberAdapter) Subscribe(topic ocdeckevent.Topic) api.EventSubscription {
	return a.bus.Subscribe(topic)
}
