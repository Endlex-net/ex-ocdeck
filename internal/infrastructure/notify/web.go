// 网页通知渠道适配器（spec「网页通知渠道」/design D7）。
//
// 不经进程内 bus：持 WebPublisher 窄端口（由 api 层 WebHub 实现），Send 直接
// 转发意图并按 accepted 判定——零连接或全部连接缓冲满（零 enqueue 成功）均为
// 该渠道投递失败。Publish 为非阻塞 enqueue，无 ctx 语义。Caps=Replace
// （无 Group；Title 由内容组装统一携带任务名，分组缺失仅表现为通知中心不
// 折叠，spec 能力矩阵）。
package notify

import (
	"context"

	"ocdeck/internal/domain/notification"
)

// WebPublisher web 渠道投递窄端口（design D7）：api 层 WebHub 的最小依赖面，
// 返回 accepted 表示至少一个已连接前端接纳了本次投递。
type WebPublisher interface {
	Publish(in notification.Intent) (accepted bool)
}

// WebChannel 网页通知渠道。
type WebChannel struct {
	pub WebPublisher
}

// NewWebChannel 构造 web 渠道（pub 由组合根传入 WebHub 实例，Lane D）。
func NewWebChannel(pub WebPublisher) *WebChannel {
	return &WebChannel{pub: pub}
}

func (c *WebChannel) Name() string { return "web" }

func (c *WebChannel) Caps() notification.Capability { return notification.CapReplace }

// Send 直接转发 Intent；accepted=false 计为该渠道投递失败（不影响其他渠道，
// 由 dispatch 层隔离）。
func (c *WebChannel) Send(_ context.Context, in notification.Intent, _ notification.ChannelConfig) notification.Result {
	if c.pub.Publish(in) {
		return notification.Result{OK: true}
	}
	return notification.Result{OK: false, Err: "web: no connected frontend accepted the notification"}
}
