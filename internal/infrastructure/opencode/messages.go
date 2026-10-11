// messages.go GET /session/:id/message 消息列表（task-notifications design D9：
// LLM 停止原因总结的 agent 最后一轮输出数据源）。
// Message/MessagePart 类型已迁移至 application/agentprovider/occlient.go
// （C2a 中性化），本包经 neutral_compat.go 别名引用。
package opencode

import (
	"context"
	"net/url"
	"strconv"
)

// ListMessages GET /session/:id/message?directory=<dir>&limit=<n>（design D9）。
// 复用 getJSON 既有鉴权与状态码分类；404/401/非 2xx/响应非消息数组 → error
// （调用方 fail-closed 降级，不重试）。
func (c *Client) ListMessages(ctx context.Context, dir, sessionID string, limit int) ([]Message, error) {
	q := url.Values{}
	q.Set("directory", dir)
	q.Set("limit", strconv.Itoa(limit))
	var msgs []Message
	if err := c.getJSON(ctx, c.httpClient, "/session/"+url.PathEscape(sessionID)+"/message", q, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}
