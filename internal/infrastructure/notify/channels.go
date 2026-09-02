package notify

import "ocdeck/internal/domain/notification"

// BuildChannels 构造三渠道实例（design D11）：web 持 WebPublisher（与路由共享
// WebHub）、bark 静态 http.Client、macos 按 goos 探测。渠道只持静态依赖，
// endpoint/token 经 DispatchPlan 下发。顺序稳定：web、bark、macos（测试通知
// 逐渠道报告保序）。
func BuildChannels(pub WebPublisher, goos string) []notification.Channel {
	return []notification.Channel{
		NewWebChannel(pub),
		NewBarkChannel(),
		NewMacosChannel(goos),
	}
}
