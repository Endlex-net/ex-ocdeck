package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// PromptResultKind 分类 POST /session/{sessionID}/prompt_async 的传输结果（design.md D1）。
// 四值唯一规则见 PromptAsync 注释；上层（task/application）据此做至多一次状态机决策。
type PromptResultKind string

const (
	// ResultAccepted 服务端返回 204（请求已被接受、异步执行）。
	ResultAccepted PromptResultKind = "accepted"
	// ResultHTTPResponse 收到任何状态码 != 204 的响应（含意外 2xx 如 200/201/202、
	// 400/401/404 等）。StatusCode/Body 携带实际值，供上层按错误矩阵分流。
	ResultHTTPResponse PromptResultKind = "http_response"
	// ResultTransportUnknown 请求已发出（httpClient.Do 返回错误：dial/超时/断连/ctx 取消），
	// 结果未知。MUST NOT 尝试区分"是否已发出"，一律按已发出处理。
	ResultTransportUnknown PromptResultKind = "transport_unknown"
	// ResultPreSendFailure httpClient.Do 之前的本地失败（marshal/NewRequest）。
	ResultPreSendFailure PromptResultKind = "pre_send_failure"
)

// PromptResult 是 PromptAsync 的传输 DTO（opencode 包仅依赖标准库）。
//
// 字段填充规则（与 Kind 绑定，唯一）：
//   - accepted:          StatusCode=204，Body/Detail 为空
//   - http_response:     StatusCode=实际状态码，Body=有界截断响应体，Detail 为空
//   - transport_unknown: StatusCode=0，Body 为空，Detail=底层错误文本
//   - pre_send_failure:  StatusCode=0，Body 为空，Detail=底层错误文本
type PromptResult struct {
	Kind       PromptResultKind
	StatusCode int    // accepted=204；http_response=实际状态码；其余=0
	Body       string // 仅 http_response 非空（有界截断）；其余为空
	Detail     string // 仅 transport_unknown/pre_send_failure 非空（底层错误文本）
}

// promptAsyncBody 是 POST /session/{sessionID}/prompt_async 的请求体（design.md D1）。
// 仅 messageID + parts[{type:"text",text}]，无其他字段。messageID 透传不重写。
type promptAsyncBody struct {
	MessageID string             `json:"messageID"`
	Parts     []promptAsyncPart   `json:"parts"`
}

type promptAsyncPart struct {
	Type string `json:"type"` // 固定 "text"
	Text string `json:"text"`
}

// PromptAsync 投递一条 text prompt 到目标 session 的异步队列（design.md D1）。
// 端点 POST /session/{sessionID}/prompt_async?directory=<dir>，Basic Auth，
// body {"messageID":..., "parts":[{"type":"text","text":...}]}。
//
// 返回 transport DTO（不返回 error）：分类规则（唯一）：
//   - marshal/NewRequest 失败 → ResultPreSendFailure（Detail=错误文本）
//   - httpClient.Do 返回错误（dial/超时/断连/ctx 取消）→ ResultTransportUnknown
//     （Detail=错误文本）——MUST NOT 尝试区分"是否已发出"，一律按已发出处理
//   - 204 → ResultAccepted
//   - 其余一切已收到状态码（含意外 2xx、400/401/404 等）→ ResultHTTPResponse
//     （StatusCode/Body 携带实际值）
//
// messageID 编码契约由调用方负责（"msg_" + 去连字符小写 UUID hex），本方法透传不重写。
func (c *Client) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) PromptResult {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/session/" + url.PathEscape(sessionID) + "/prompt_async?" + q.Encode()

	body, err := json.Marshal(promptAsyncBody{
		MessageID: messageID,
		Parts:     []promptAsyncPart{{Type: "text", Text: text}},
	})
	if err != nil {
		return PromptResult{Kind: ResultPreSendFailure, Detail: fmt.Sprintf("opencode: prompt_async: marshal body: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return PromptResult{Kind: ResultPreSendFailure, Detail: fmt.Sprintf("opencode: prompt_async: new request: %v", err)}
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PromptResult{Kind: ResultTransportUnknown, Detail: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return PromptResult{Kind: ResultAccepted, StatusCode: resp.StatusCode}
	}
	// 其余一切已收到状态码（含意外 2xx、4xx、5xx）→ http_response。
	return PromptResult{
		Kind:       ResultHTTPResponse,
		StatusCode: resp.StatusCode,
		Body:       readBodyForError(resp.Body),
	}
}

// --- 能力探测：GET /doc 结构化解析（design.md D1 能力探测唯一实现） ---

// CapabilityState 能力探测三值结果（design.md D1）。
type CapabilityState string

const (
	// CapabilitySupported 200 且路径存在且 operationId 匹配。
	CapabilitySupported CapabilityState = "supported"
	// CapabilityUnsupported 200 但路径缺失或 operationId 不符（fail-closed）。
	CapabilityUnsupported CapabilityState = "unsupported"
	// CapabilityUnknown 401/404/5xx/网络错误/畸形 JSON。
	CapabilityUnknown CapabilityState = "unknown"
)

// promptAsyncPathKey 是 OpenAPI paths 中 prompt_async 端点的路径键（design.md D1 逐字）。
const promptAsyncPathKey = "/session/{sessionID}/prompt_async"

// promptAsyncOperationID 是该端点 POST 的期望 operationId（design.md D1 逐字）。
const promptAsyncOperationID = "session.prompt_async"

// openAPIDoc 是 GET /doc 返回的 OpenAPI 文档的最小解析结构（仅能力探测所需）。
type openAPIDoc struct {
	Paths map[string]struct {
		Post *struct {
			OperationID string `json:"operationId"`
		} `json:"post"`
	} `json:"paths"`
}

// ProbePromptAsyncCapability 请求 GET /doc 并结构化解析，判定 prompt_async 端点能力
//（design.md D1 能力探测唯一实现，本方法只管请求与解析，不缓存）。
//
// 结果语义矩阵（唯一）：
//   - 200 且 paths["/session/{sessionID}/prompt_async"].post 存在且
//     operationId == "session.prompt_async" → CapabilitySupported
//   - 200 但路径缺失或 operationId 不符 → CapabilityUnsupported（fail-closed）
//   - 401/404/5xx/网络错误/畸形 JSON（含首值后尾随非空白垃圾/第二 JSON 值）→ CapabilityUnknown
//
// 401 归 unknown（凭据/中间件问题，可重试）；404 归 unknown（端点缺失视为未知而非
// 结构漂移，与能力探测语义一致）。GET /doc 受认证中间件保护：配置
// OPENCODE_SERVER_PASSWORD 时无 Auth→401；未配置时无需认证。
func (c *Client) ProbePromptAsyncCapability(ctx context.Context) CapabilityState {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/doc", nil)
	if err != nil {
		return CapabilityUnknown
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CapabilityUnknown
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// 继续结构化解析。
	case resp.StatusCode == http.StatusUnauthorized:
		return CapabilityUnknown
	default:
		// 404/5xx/其他非 200 归 unknown。
		return CapabilityUnknown
	}

	// 畸形 JSON 归 unknown：首值解码成功后必须紧接 io.EOF（仅允许尾随空白），
	// 任何第二 JSON 值或非空白垃圾均视为畸形文档。
	dec := json.NewDecoder(resp.Body)
	var doc openAPIDoc
	if err := dec.Decode(&doc); err != nil {
		return CapabilityUnknown
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return CapabilityUnknown
	}
	entry, ok := doc.Paths[promptAsyncPathKey]
	if !ok || entry.Post == nil {
		return CapabilityUnsupported
	}
	if entry.Post.OperationID != promptAsyncOperationID {
		return CapabilityUnsupported
	}
	return CapabilitySupported
}