// messages.go GET /session/:id/message 消息列表（task-notifications design D9：
// LLM 停止原因总结的 agent 最后一轮输出数据源）。
package opencode

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// Message /session/:id/message 列表元素：id/role/parts。parts 内非文本 part
// （tool 等）保留 Type、无 text 字段；形状漂移由解码错误承担（fail-closed）。
//
// 实测 live OpenCode 响应为 {info:{id,role,sessionID}, parts:[...]}，顶层无
// id/role（旧 flat {id,role,parts} 仍可能出现）。UnmarshalJSON 在顶层缺值时
// 从 info 兜底回填 id/role，使 LastAgentOutput 能命中 assistant；info 缺失时
// 保持 flat 形状语义不变。
type Message struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
}

// messageInfo live 形状的 info 嵌套块，仅取 id/role 兜底用。
type messageInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

// UnmarshalJSON 兼容 flat {id,role,parts} 与 live {info,parts} 两种形状：
// 顶层 Role/ID 为空时从 info 兜底；parts 始终取顶层。info 缺失不报错，flat 仍有效。
func (m *Message) UnmarshalJSON(data []byte) error {
	type flat struct {
		ID    string        `json:"id"`
		Role  string        `json:"role"`
		Parts []MessagePart `json:"parts"`
		Info  *messageInfo  `json:"info"`
	}
	var f flat
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	m.ID = f.ID
	m.Role = f.Role
	m.Parts = f.Parts
	if f.Info != nil {
		if m.ID == "" {
			m.ID = f.Info.ID
		}
		if m.Role == "" {
			m.Role = f.Info.Role
		}
	}
	return nil
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
