// notify.go 通知组合根适配（task-notifications design D11）：把单例 bus / aiStore
// 适配为 application/notification 窄端口。application/notification MUST NOT
// import infrastructure 具体类型（Lane D import_graph 断言）。
package main

import (
	"context"
	"fmt"

	"ocdeck/internal/api"
	appnotification "ocdeck/internal/application/notification"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/ai"
	"ocdeck/internal/infrastructure/eventbus"
	"ocdeck/internal/infrastructure/notify"
	"ocdeck/internal/task"
)

var (
	_ appnotification.EventSubscriber    = notifyEventSubscriberAdapter{}
	_ appnotification.SummaryCompleter   = summaryCompleterAdapter{}
	_ appnotification.TaskSnapshotReader = (*task.Manager)(nil)
	_ appnotification.ActiveTaskLister   = (*task.Manager)(nil)
	_ appnotification.ConfigStore        = (*notify.Store)(nil)
	_ api.NotificationTester             = (*appnotification.Notifier)(nil)
)

// notifyEventSubscriberAdapter 把 *eventbus.Bus 适配为通知触发器的 EventSubscriber。
// 与 api 侧 eventSubscriberAdapter 并列：Subscribe 返回类型必须精确匹配各自端口
// （Go 接口不变性）；*eventbus.Sub 结构性满足两边的 EventSubscription。
type notifyEventSubscriberAdapter struct {
	bus *eventbus.Bus
}

func (a notifyEventSubscriberAdapter) Subscribe(topic ocdeckevent.Topic) appnotification.EventSubscription {
	return a.bus.Subscribe(topic)
}

// summaryCompleterAdapter 把 ai.Store + Completer 适配为 SummaryCompleter
// （design D9：复用 infrastructure/ai completer 与 aiStore；未配置/构造失败
// 返回 error，Notifier 降级为确定性摘要）。单次 Complete 使用同一 State() 快照。
type summaryCompleterAdapter struct {
	store *ai.Store
}

func (a summaryCompleterAdapter) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	st := a.store.State()
	if !st.Configured {
		return "", fmt.Errorf("ai not configured")
	}
	c, err := ai.NewCompleter(st.CFG)
	if err != nil {
		return "", err
	}
	resp, err := c.Complete(ctx, ai.Request{System: system, User: user, MaxTokens: maxTokens})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}
