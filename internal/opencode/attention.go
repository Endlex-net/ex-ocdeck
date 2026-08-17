package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// ErrCapabilityUnsupported 表示 opencode serve 不支持该能力端点（404）。
// 注意力对账据此把能力状态机迁移到 unsupported：停止 REST 对账、忽略该类型 SSE、
// 透出空数组（design.md D6 能力状态机）。
var ErrCapabilityUnsupported = errors.New("opencode: capability unsupported (endpoint not available)")

// PermissionRequest 是 GET /permission 返回的单条 pending 权限请求（opencode 1.18.18 契约）。
// 字段取自 permission.asked SSE 事件 payload 的可持久子集：id/sessionID/permission/patterns。
// metadata/always/tool 等展示字段由前端经 SSE 透传，本层只保留 pending 集合所需的最小稳定字段。
type PermissionRequest struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
}

// QuestionItem 是 question.asked 事件中单条提问的最小稳定字段（design.md D6）。
// header/question 为 pending 集合展示与跳转所需；options/multiple/custom 等展示字段
// 由前端经 SSE 透传，不在 pending 集合中持久化。
type QuestionItem struct {
	Header   string `json:"header"`
	Question string `json:"question"`
}

// QuestionRequest 是 GET /question 返回的单条 pending 问题请求（opencode 1.18.18 契约）。
type QuestionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Questions []QuestionItem `json:"questions"`
}

// AttentionKind 注意力事件语义：asked（新增 pending）/ replied|rejected（了结 pending）。
type AttentionKind string

const (
	AttentionAsked    AttentionKind = "asked"
	AttentionReplied  AttentionKind = "replied"
	AttentionRejected AttentionKind = "rejected"
)

// AttentionType 注意力事件类型：permission 或 question。两类型独立对账与状态机。
type AttentionType string

const (
	AttentionPermission AttentionType = "permission"
	AttentionQuestion   AttentionType = "question"
)

// AttentionEvent 是从 SSE Event 解析出的规范化注意力事件（design.md D6 三层类型模型
// 的 opencode 层）。task 层不消费 Event.Properties 的 map，仅消费此 typed 结构。
// 不含 Since：since 由 task runtime 在本地首次观察时附加。
type AttentionEvent struct {
	Kind       AttentionKind
	Type       AttentionType
	RequestID  string
	SessionID  string
	Permission string         // permission.asked only
	Patterns   []string       // permission.asked only
	Questions  []QuestionItem // question.asked only
}

// ParseAttentionEvent 将 SSE Event 按其 type 分派解析为 AttentionEvent。
// 支持 v1 与 v2 同形家族（permission.asked/replied、permission.v2.asked/replied、
// question.asked/replied/rejected、question.v2.asked/replied/rejected）。
// 字段级校验（design.md D6 + agent-attention spec）：
//   - id + sessionID 必填；permission asks 还需 permission；question asks 还需非空 Questions
//     且每条 header+question 非空；patterns 必须为 string 数组
//   - replied/rejected 需 sessionID + requestID
//
// 未知 type 或缺字段 → 返回 (zero, false)，调用方静默忽略（不中断流）。
func ParseAttentionEvent(ev Event) (AttentionEvent, bool) {
	props := ev.Properties
	if props == nil {
		return AttentionEvent{}, false
	}
	switch ev.Type {
	case "permission.asked", "permission.v2.asked":
		return parsePermissionAsked(props)
	case "permission.replied", "permission.v2.replied":
		return parsePermissionReplied(props)
	case "question.asked", "question.v2.asked":
		return parseQuestionAsked(props)
	case "question.replied", "question.v2.replied":
		return parseQuestionReplied(props)
	case "question.rejected", "question.v2.rejected":
		return parseQuestionRejected(props)
	default:
		return AttentionEvent{}, false
	}
}

func parsePermissionAsked(props map[string]interface{}) (AttentionEvent, bool) {
	id, ok := stringField(props, "id")
	if !ok {
		return AttentionEvent{}, false
	}
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	perm, ok := stringField(props, "permission")
	if !ok {
		return AttentionEvent{}, false
	}
	// patterns 缺省（缺失或 null）合法 → nil；存在但非字符串数组 → 非法。
	patterns, ok := optionalStringSliceField(props, "patterns")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:       AttentionAsked,
		Type:       AttentionPermission,
		RequestID:  id,
		SessionID:  sid,
		Permission: perm,
		Patterns:   patterns,
	}, true
}

func parsePermissionReplied(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionReplied,
		Type:      AttentionPermission,
		RequestID: rid,
		SessionID: sid,
	}, true
}

func parseQuestionAsked(props map[string]interface{}) (AttentionEvent, bool) {
	id, ok := stringField(props, "id")
	if !ok {
		return AttentionEvent{}, false
	}
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	questions, ok := parseQuestionItems(props["questions"])
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionAsked,
		Type:      AttentionQuestion,
		RequestID: id,
		SessionID: sid,
		Questions: questions,
	}, true
}

func parseQuestionReplied(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionReplied,
		Type:      AttentionQuestion,
		RequestID: rid,
		SessionID: sid,
	}, true
}

func parseQuestionRejected(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionRejected,
		Type:      AttentionQuestion,
		RequestID: rid,
		SessionID: sid,
	}, true
}

// parseQuestionItems 解析 question.asked 的 questions 数组，逐条校验 header+question 非空。
// 非数组、空数组、任一条目缺 header/question → false。
func parseQuestionItems(v interface{}) ([]QuestionItem, bool) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, false
	}
	out := make([]QuestionItem, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, false
		}
		header, ok := stringField(m, "header")
		if !ok {
			return nil, false
		}
		question, ok := stringField(m, "question")
		if !ok {
			return nil, false
		}
		out = append(out, QuestionItem{Header: header, Question: question})
	}
	return out, true
}

// stringField 取 props[key] 并断言为非空 string。
func stringField(props map[string]interface{}, key string) (string, bool) {
	v, ok := props[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// stringSliceField 取 props[key] 并断言为 []interface{} 全为 string。
// 允许空数组（patterns: [] 为合法）。非数组或任一非 string → false。
func stringSliceField(props map[string]interface{}, key string) ([]string, bool) {
	v, ok := props[key]
	if !ok {
		return nil, false
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// optionalStringSliceField 取 props[key]：缺省（缺失或 null）合法返回 (nil, true)；
// 存在但非数组或含非 string 元素 → (nil, false)。
// 空数组 [] 合法返回 (空 slice, true)。
func optionalStringSliceField(props map[string]interface{}, key string) ([]string, bool) {
	v, ok := props[key]
	if !ok || v == nil {
		return nil, true // 缺省/null 合法
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false // 存在但非数组 → 非法
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// ListPermissions GET /permission?directory=<wt>（opencode 1.18.18 契约）→
// pending 权限请求快照。404 → ErrCapabilityUnsupported（能力状态机迁移 unsupported）。
// 响应 200 但 null/非数组/坏元素 → 整体失败（design.md D6：whole-type degraded）。
func (c *Client) ListPermissions(ctx context.Context, dir string) ([]PermissionRequest, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raws []jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/permission", q, &raws); err != nil {
		if isNotFound(err) {
			return nil, ErrCapabilityUnsupported
		}
		return nil, err
	}
	// 200 但 JSON null → 整体失败（spec：非数组形状为错误）。空数组 [] 合法（len==0）。
	if raws == nil {
		return nil, fmt.Errorf("opencode: list permissions: response is null")
	}
	out := make([]PermissionRequest, 0, len(raws))
	for _, raw := range raws {
		req, perr := parsePermissionRequest(raw)
		if perr != nil {
			return nil, fmt.Errorf("opencode: list permissions: %w", perr)
		}
		out = append(out, req)
	}
	return out, nil
}

// ListQuestions GET /question?directory=<wt>（opencode 1.18.18 契约）→
// pending 问题请求快照。404 → ErrCapabilityUnsupported。
// 响应 200 但 null/非数组/坏元素 → 整体失败。
func (c *Client) ListQuestions(ctx context.Context, dir string) ([]QuestionRequest, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raws []jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/question", q, &raws); err != nil {
		if isNotFound(err) {
			return nil, ErrCapabilityUnsupported
		}
		return nil, err
	}
	if raws == nil {
		return nil, fmt.Errorf("opencode: list questions: response is null")
	}
	out := make([]QuestionRequest, 0, len(raws))
	for _, raw := range raws {
		req, perr := parseQuestionRequest(raw)
		if perr != nil {
			return nil, fmt.Errorf("opencode: list questions: %w", perr)
		}
		out = append(out, req)
	}
	return out, nil
}

func parsePermissionRequest(raw jsonRawObject) (PermissionRequest, error) {
	var r PermissionRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return PermissionRequest{}, err
	}
	if r.ID == "" || r.SessionID == "" || r.Permission == "" {
		return PermissionRequest{}, fmt.Errorf("permission request: missing id/sessionID/permission")
	}
	return r, nil
}

func parseQuestionRequest(raw jsonRawObject) (QuestionRequest, error) {
	var r QuestionRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return QuestionRequest{}, err
	}
	if r.ID == "" || r.SessionID == "" {
		return QuestionRequest{}, fmt.Errorf("question request: missing id/sessionID")
	}
	if len(r.Questions) == 0 {
		return QuestionRequest{}, fmt.Errorf("question request: empty questions")
	}
	for _, q := range r.Questions {
		if q.Header == "" || q.Question == "" {
			return QuestionRequest{}, fmt.Errorf("question request: question missing header/question")
		}
	}
	return r, nil
}
