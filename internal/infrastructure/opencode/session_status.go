// session_status.go 实现 GET /session/status 严格版（能力探测）与宽容版
// （StatusReader.Current 的 unknown 语义）。事件解析器 SessionStatusEvent/
// ParseSessionStatusEvent 已迁移至 application/agentprovider/occlient.go
// （C2a 中性化），本包经 neutral_compat.go 别名引用。
package opencode

import (
	"context"
	"net/url"
)

// SessionStatusTolerant 是 /session/status 的宽容解析变体（agent-provider-spi
// design D8：REST 路径 StatusReader.Current 的「未知枚举 → unknown」语义）。
// 与 SessionStatus（严格版，能力探测沿用——结构漂移必须使探测失败）的差异
// 仅在条目级：传输/HTTP 错误照常返回；条目 type 缺失/非三值枚举时该条目
// Type 为空串（调用方映射 unknown），不使整个调用失败。
func (c *Client) SessionStatusTolerant(ctx context.Context, dir string) (map[string]SessionStatus, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raw map[string]jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/session/status", q, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]SessionStatus, len(raw))
	for sid, r := range raw {
		st, err := parseSessionStatus(r)
		if err != nil {
			out[sid] = SessionStatus{}
			continue
		}
		out[sid] = st
	}
	return out, nil
}
