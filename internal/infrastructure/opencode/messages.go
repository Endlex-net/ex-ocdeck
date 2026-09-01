// messages.go GET /session/:id/message 消息列表（task-notifications design D9：
// LLM 停止原因总结的 agent 最后一轮输出数据源）。
package opencode

import (
	"context"
	"net/url"
	"strconv"
)

// Message /session/:id/message 列表元素：id/role/parts。parts 内非文本 part
// （tool 等）保留 Type、无 text 字段；形状漂移由解码错误承担（fail-closed）。
type Message struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
}

// MessagePart 消息 part：type=text 时携带 text，其余类型仅保留 type。
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

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
