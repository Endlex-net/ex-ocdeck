package notify

import "ocdeck/internal/domain/notification"

// BuildChannels 构造四渠道实例（design D1）：web 持 WebPublisher（与路由共享
// WebHub）、bark 与 wecom 静态 http.Client、macos 按 goos 探测。渠道只持静态依赖，
// endpoint/token/url 经 DispatchPlan 下发。顺序稳定：web、bark、macos、wecom
// （测试通知逐渠道报告保序）。
func BuildChannels(pub WebPublisher, goos string) []notification.Channel {
	return []notification.Channel{
		NewWebChannel(pub),
		NewBarkChannel(),
		NewMacosChannel(goos),
		NewWecomChannel(),
	}
}
