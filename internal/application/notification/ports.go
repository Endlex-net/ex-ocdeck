// Package notification 实现通知触发编排（task-notifications design D1/D3/D4）：
// 单 goroutine run loop 消费 serve_runtime/task 领域事件，维护五类触发状态机、
// episode 仲裁、启动基线/overflow 对账、发送前门禁与 DispatchPlan 投递。
//
// 本包仅依赖 domain 与 application 共享 DTO + stdlib，MUST NOT import
// infrastructure 具体类型（任务侧与 bus 依赖全部经窄端口注入，组合根适配——
// import 断言见 internal/api/import_graph_test.go，Lane D）。
package notification

import (
	"context"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// EventSubscription bus 订阅窄接口（design D1：组合根包装 *eventbus.Sub——
// Bus.Subscribe 返回具体类型与端口不同，必须经适配，本包不感知）。
// Overflow 为「至少一次可观察」信号：接收即消费，下次溢出重新置位。
type EventSubscription interface {
	C() <-chan ocdeckevent.Event
	Overflow() <-chan struct{}
	Close()
}

// EventSubscriber bus 订阅窄接口。
type EventSubscriber interface {
	Subscribe(topic ocdeckevent.Topic) EventSubscription
}

// TaskRef 任务行最小引用（组合快照内嵌）。
type TaskRef struct {
	ID     string
	Name   string
	Status string
}

// RetryDetail 每 session 最近一次 retry 详情（design D3 类型定义；缺失以
// HasRetryDetail=false 显式表达，不用零值歧义）。
type RetryDetail struct {
	Attempt int    // 重试序号，>=1 为有效；0 表示缺失
	Message string // 失败摘要；空串表示缺失
	Next    int64  // epoch ms；0 表示事件未携带 next
}

// TaskSnapshot 单次组合快照（design D1：任务行、attention、run_status、retry
// 详情一次读取，MUST NOT 分次独立读取——spec「通知抑制、启动基线与对账」）。
// Attention 为 application 共享 DTO（深拷贝只读）；InstVersion 为快照捕获时任务
// 当前 runtime 的实例令牌（design D3 runtime fencing：serve 事件 RID 与之一致
// 才处理，旧 runtime 迟到事件丢弃；无 runtime 为空串）。
type TaskSnapshot struct {
	Task           TaskRef
	Attention      application.Attention
	RunStatus      string // idle|busy|retry 聚合值；"" 表不可用
	RetryDetail    RetryDetail
	HasRetryDetail bool
	InstVersion    string
}

// TaskSnapshotReader 任务侧组合快照端口（task.Manager 实现）。
type TaskSnapshotReader interface {
	TaskNotificationSnapshot(ctx context.Context, taskID string) (TaskSnapshot, error)
}

// ActiveTaskLister active 任务枚举端口（启动基线与 overflow 对账；task.Manager 实现）。
type ActiveTaskLister interface {
	ListAllActiveTaskIDs(ctx context.Context) ([]string, error)
}

// ConfigStore 通知配置快照端口（infrastructure/notify.Store 实现：load_error 态
// 已由 Store 降级为默认配置，本端口只见最终生效配置）。
type ConfigStore interface {
	Config() notification.Config
}

// LastAgentOutputReader agent 最后一轮输出窄端口（task-notifications design D9：
// task.Manager 实现——锚会话优先、无锚取最近更新 owned session，limit 10 拉取，
// 最后一条 assistant 消息的文本 part 拼接、截 2000 字符；实现 MUST 尊重 ctx
// 取消，不可得返回 ok=false，fail-closed 由调用方降级为「（不可得）」）。
type LastAgentOutputReader interface {
	LastAgentOutput(ctx context.Context, taskID string) (string, bool)
}

// BaseURLResolver 跳转 base 推导窄端口（design D8/D11：组合根闭包只读
// srv.BoundAddr()；配置 base_url 覆盖值由调用方传入，resolver MUST NOT 自行读
// 配置存储）。
type BaseURLResolver func(configuredBaseURL string) (string, error)
