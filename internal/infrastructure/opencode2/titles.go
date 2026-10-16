// titles.go 实现 TitleUpdater 端口（opencode-v2-provider tasks 4.4）：标题
// 写入 = PATCH /api/session/{id} body {title}，已实证 204 + GET 读回精确
// 匹配（CONTRACT §2）。
package opencode2

import (
	"context"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

type spiTitles struct{ p *SPIProvider }

func (a *spiTitles) Sync(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, title string) error {
	h := a.p.healthBase(b, appagent.PrimitiveTitleUpdater, appagent.ScopeSession, &s)
	c, _, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "titles.sync", err)
		return err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "titles.sync", err)
		return err
	}
	err = classifyHTTPError("titles.sync", c.UpdateTitle(ctx, s.UpstreamID, title))
	a.p.reportConversion(h, "titles.sync", err)
	return err
}
