// Completer 抽象 + 通用 HTTP 客户端辅助（design.md D2）。
//
// 失败语义统一为「任何错误 → 调用方回退」：非 2xx / 超时 / 解析失败 / 结构缺失 /
// 请求或响应 body 超 1MB → 返回 error，MUST NOT 重试。api_key 不写入任何日志。

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// jsonUnmarshal 仅作为薄包装，便于统一 mock（当前直接转调 stdlib）。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// Request 是 LLM 补全请求的统一形态（design.md D2）。
type Request struct {
	System    string // 可选 system prompt
	User      string // user 消息
	MaxTokens int    // 必填上限（Anthropic 协议必填，OpenAI 同设）
}

// Response 是 LLM 补全响应的统一形态。
type Response struct {
	Text string // 首个文本块的全部内容
}

// Completer 抽象 provider 调用。未来 LLM 功能直接复用此接口。
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// NewCompleter 按 provider 分派（design.md D2）。返回薄 client 实现。
// 未知 provider 返回错误（调用方应已在配置层拒绝）。
func NewCompleter(cfg ProviderConfig) (Completer, error) {
	switch cfg.Provider {
	case ProviderOpenAI:
		return newOpenAICompleter(cfg), nil
	case ProviderAnthropic:
		return newAnthropicCompleter(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}
}

// httpTimeout Completer HTTP 超时上限（design.md D2：≤10s）。
const httpTimeout = 10 * time.Second

// maxBodyBytes 请求/响应 body 上限 1MB（design.md D2）。
const maxBodyBytes = 1 << 20

// errBodyTooLarge body 超 1MB 时返回（不截断后解析）。
var errBodyTooLarge = errors.New("ai: response body exceeds 1MB limit")

// errRequestBodyTooLarge 请求序列化后超 1MB（design.md D2：请求 body 上限 1MB）。
var errRequestBodyTooLarge = errors.New("ai: request body exceeds 1MB limit")

// doJSON 发送 JSON 请求并读取上限 1MB 的 JSON 响应。
// 统一处理：超时（由 http.Client + ctx）、非 2xx/3xx、读超限、解析失败、结构缺失。
// 调用方通过 parseFn 从已读 body 提取结果。
//
// 能力协商（design.md D2）：当 thinkingEnabled 且响应为 4xx 且错误体表明不支持
// thinking/reasoning 参数时，MUST 剥离思考参数原样重试一次（确定性能力协商，
// 非瞬时重试）。重试仍失败才返回 error。重试仅发生一次。
//
// 安全：
//   - http.Client 禁止自动跟随 redirect（CheckRedirect 返回 ErrUseLastResponse），
//     防止攻击者控制的 base_url 用 302 Location: ...?<key> 泄露 api_key 到日志。
//   - 错误信息 MUST NOT 携带 provider 响应原文（自定义 base_url 时响应可能回显
//     请求头/体，含 api_key），仅保留 provider 名与 status code。
//   - 纵深防御：transport error 仍可能包含 api_key（如恶意 URL 嵌入 error），
//     返回前若 error 字符串含 apiKey 子串则替换为 ***。
func doJSON(ctx context.Context, client *http.Client, provider Provider, apiKey string, buildRequest func(stripThinking bool) (*http.Request, error), parse func([]byte) (string, error), thinkingEnabled bool) (Response, error) {
	req, err := buildRequest(false)
	if err != nil {
		return Response{}, err
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s http request: %s", provider, redactAPIKey(err.Error(), apiKey))
	}
	body, parseErr := handleResponse(resp, provider, apiKey, parse)
	if parseErr == nil {
		return body, nil
	}
	// 能力协商：thinking 启用且 4xx 表明不支持 → 剥离思考参数重试一次。
	if thinkingEnabled && isUnsupportedThinkingError(parseErr) {
		req2, err2 := buildRequest(true)
		if err2 != nil {
			return Response{}, err2
		}
		resp2, err2 := client.Do(req2.WithContext(ctx))
		if err2 != nil {
			return Response{}, fmt.Errorf("ai: %s http request: %s", provider, redactAPIKey(err2.Error(), apiKey))
		}
		body2, parseErr2 := handleResponse(resp2, provider, apiKey, parse)
		if parseErr2 == nil {
			return body2, nil
		}
		return Response{}, parseErr2
	}
	return Response{}, parseErr
}

// handleResponse 处理单次 HTTP 响应：关闭 body、非 2xx → error（含 4xx 响应体供
// 能力协商判定，但错误信息本身仅含 provider+status 不含响应原文）、读 body、parse。
func handleResponse(resp *http.Response, provider Provider, apiKey string, parse func([]byte) (string, error)) (Response, error) {
	defer func() { _ = resp.Body.Close() }()

	// 非 2xx → error。读 body 用于能力协商判定（isUnsupportedThinkingError），
	// 但返回的错误信息只含 provider + status，不含响应原文。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readLimited(resp.Body)
		return Response{}, &providerError{
			provider: provider,
			status:   resp.StatusCode,
			body:     body,
		}
	}

	body, err := readLimited(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s read response: %w", provider, err)
	}

	text, err := parse(body)
	if err != nil {
		return Response{}, fmt.Errorf("ai: %s parse response: %w", provider, err)
	}
	return Response{Text: text}, nil
}

// providerError 携带 status 与 body 供能力协商判定，但 Error() 不含响应原文。
type providerError struct {
	provider Provider
	status   int
	body     []byte
}

func (e *providerError) Error() string {
	return fmt.Sprintf("ai: %s provider returned status %d", e.provider, e.status)
}

// isUnsupportedThinkingError 判定 error 是否为「不支持思考参数」的参数校验类错误
// （启发式，design.md D2 能力协商）。
//
// 两道收窄：
//  1. 状态码：仅 400 与 422（参数校验类）可触发协商；401/404/429 等即使错误体强匹配
//     也 MUST NOT 重试。
//  2. 判定改为结构化提取 + 语义词补全：优先将响应体按 JSON 解析并提取错误对象字段
//     （OpenAI 风格 {"error":{"message","param","code"}}、Anthropic 风格
//     {"error":{"message"}} 或顶层 {"message"}），只在错误对象字段内做参数标识 +
//     不支持语义判定；仅当 body 非 JSON 时才用全文兜底。避免跨字段误判（如
//     "thinking" 在 request 字段、"invalid" 在 message 字段分处不同对象）。
//
// 触发条件：思考参数证据 + 不支持语义 **在同一个判定文本内** 共同出现。
//   - 思考参数证据：结构化 error.param == reasoning_effort|thinking；或文本含
//     reasoning_effort 子串、thinking 与 parameter 共现、thinking is not supported、
//     "thinking" JSON 键形态、not support thinking（见 containsThinkingParamMarker）。
//   - 不支持语义：unsupported/unknown/not supported/not support/invalid/unrecognized。
//
// 单独出现的语义词（无思考参数标识）MUST NOT 触发；
// 单独出现的 thinking（无不支持语义，如 429 rate limit for thinking-model）MUST NOT 触发。
func isUnsupportedThinkingError(err error) bool {
	pe, ok := err.(*providerError)
	if !ok {
		return false
	}
	// 仅参数校验类状态码可触发协商。
	if pe.status != http.StatusBadRequest && pe.status != http.StatusUnprocessableEntity {
		return false
	}
	raw := pe.body

	// 1) 优先结构化提取：在错误对象字段内判定。
	if matchStructuredUnsupportedThinking(raw) {
		return true
	}

	// 2) 仅当 body 非 JSON 时用全文兜底。
	if !jsonLooksLikeObject(raw) {
		body := strings.ToLower(string(raw))
		return containsThinkingParamMarker(body) && containsUnsupportedSemantic(body)
	}
	return false
}

// jsonLooksLikeObject 粗判 body 是否为 JSON 对象（以 `{` 起始，忽略前导空白）。
// 用于决定是否走结构化提取路径 vs 全文兜底。
func jsonLooksLikeObject(body []byte) bool {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// matchStructuredUnsupportedThinking 解析 JSON 错误响应，仅在错误对象字段内判定
// 思考参数证据 + 不支持语义是否共同出现。
//
// 覆盖三种常见形态：
//   - OpenAI 风格：{"error":{"message":"...","param":"reasoning_effort","code":"..."}}
//   - Anthropic 风格：{"error":{"message":"thinking is not supported ..."}}
//   - 顶层 message：{"message":"...","param":"thinking"} 或 {"message":"unknown parameter thinking"}
//
// 解析成功后在提取的错误字段文本（message + param + code，同处 error 对象）内判定；
// body 内其他字段（如 request.thinking）不计入，避免跨字段误判。
func matchStructuredUnsupportedThinking(body []byte) bool {
	// 提取错误对象文本：error 子对象字段 + 顶层 message/param/code（兜底）。
	text := extractErrorObjectText(body)
	if text == "" {
		return false
	}
	// 结构化 param 优先：error.param == reasoning_effort|thinking 即触发。
	param := extractErrorParam(body)
	if param == "reasoning_effort" || param == "thinking" {
		return true
	}
	return containsThinkingParamMarker(text) && containsUnsupportedSemantic(text)
}

// extractErrorParam 从 JSON 提取 error.param 字段（OpenAI 风格），lowercase + trim。
// 提取失败或字段缺失返回空串。
func extractErrorParam(body []byte) string {
	var structured struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
		Param string `json:"param"`
	}
	if err := json.Unmarshal(body, &structured); err != nil {
		return ""
	}
	if structured.Error.Param != "" {
		return strings.ToLower(strings.TrimSpace(structured.Error.Param))
	}
	if structured.Param != "" {
		return strings.ToLower(strings.TrimSpace(structured.Param))
	}
	return ""
}

// extractErrorObjectText 从 JSON 提取错误对象相关字段的拼接文本，用于语义词判定。
// 仅提取 error 子对象的 string 字段值与顶层 message/param/code，避免纳入 request 等
// 其他字段（防止跨字段误判）。
func extractErrorObjectText(body []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	var parts []string

	// error 子对象的 string 值。
	if errObj, ok := raw["error"]; ok {
		var errMap map[string]json.RawMessage
		if err := json.Unmarshal(errObj, &errMap); err == nil {
			for _, v := range errMap {
				if s, ok := rawJSONStringValue(v); ok {
					parts = append(parts, s)
				}
			}
		} else {
			// error 字段本身是 string。
			if s, ok := rawJSONStringValue(errObj); ok {
				parts = append(parts, s)
			}
		}
	}

	// 顶层 message/param/code（兜底 Anthropic 平铺或简化结构）。
	for _, key := range []string{"message", "param", "code"} {
		if v, ok := raw[key]; ok {
			if s, ok := rawJSONStringValue(v); ok {
				parts = append(parts, s)
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// rawJSONStringValue 若 rawMessage 为 JSON 字符串则返回其值，否则 ok=false。
func rawJSONStringValue(v json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s, true
	}
	return "", false
}

// containsThinkingParamMarker 判定 body（错误对象字段拼接文本或全文兜底文本）是否含思考参数标识。
//
// `reasoning_effort` 含下划线，天然是参数名形态，保持子串匹配即可。
// `thinking` 作为单词可能在 model 名（如 thinking-v1）等非参数上下文出现，
// 故仅当与参数上下文绑定才算标识，匹配以下任一形态：
//   - `reasoning_effort`（参数名形态）
//   - `thinking` 与 `parameter` 同时出现（如 "unknown parameter thinking"、
//     "thinking is an unknown parameter"；model 错误信息一般不含 parameter 一词）
//   - `not support thinking` / `not support reasoning_effort`（自然表述拒绝语）
//   - `thinking is not supported`（Anthropic 风格拒绝语）
//   - `"thinking"`（带引号的 JSON 键/字段形态）
//
// 裸子串 `thinking`（如 `model thinking-v1`）MUST NOT 命中。
func containsThinkingParamMarker(body string) bool {
	if strings.Contains(body, "reasoning_effort") {
		return true
	}
	if strings.Contains(body, "thinking") && strings.Contains(body, "parameter") {
		return true
	}
	if strings.Contains(body, "not support thinking") || strings.Contains(body, "not support reasoning_effort") {
		return true
	}
	if strings.Contains(body, "thinking is not supported") {
		return true
	}
	if strings.Contains(body, `"thinking"`) {
		return true
	}
	return false
}

// containsUnsupportedSemantic 判定 body 是否含「不支持/未知」语义词。
// 含 `not support` 形态以覆盖自然表述如 "This model does not support thinking."
func containsUnsupportedSemantic(body string) bool {
	for _, marker := range []string{"unsupported", "unknown", "not supported", "not support", "invalid", "unrecognized"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// redactAPIKey 将 s 中出现的 key 子串替换为 ***，作为 transport error 去敏的纵深防御。
// key 为空时原样返回。
func redactAPIKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// readLimited 最多读取 maxBodyBytes+1 字节；超过则返回 errBodyTooLarge（不截断解析）。
func readLimited(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return data, nil
}

// newJSONRequest 构造 POST JSON 请求，强制 Content-Type: application/json。
// 序列化后 MUST 不超过 maxBodyBytes（design.md D2 请求 body 上限 1MB），超限显式 error。
func newJSONRequest(url string, body any) (*http.Request, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if len(data) > maxBodyBytes {
		return nil, errRequestBodyTooLarge
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// newHTTPClient 构造统一超时的 http.Client。
//
// 禁止自动跟随 redirect（返回 ErrUseLastResponse）：攻击者控制的自定义 base_url
// 可用 302 Location: ...?<key> 泄露 api_key。禁止跟随让 3xx 走既有「非 2xx 静态
// 错误」路径（仅 provider + status，无 body），配合 redactAPIKey 形成纵深防御。
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}