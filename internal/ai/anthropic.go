// Anthropic provider 实现（design.md D2）。
//
// 端点：{base}/v1/messages（默认 https://api.anthropic.com）。
// 认证头：x-api-key: <api_key> + anthropic-version: 2023-06-01。
// 请求体：{model, system, messages:[{role:user, content}], max_tokens}（max_tokens 协议必填）。
// 响应取值：拼接 content[*] 中 type=="text" 的 text。
// 失败语义：非 2xx / 超时 / 解析失败 / 结构缺失 / 1MB 超限 → error，不重试。

package ai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// anthropicVersion Anthropic API 版本头（design.md D2 契约表）。
const anthropicVersion = "2023-06-01"

// anthropicCompleter 实现 Completer。
type anthropicCompleter struct {
	cfg    ProviderConfig
	client *http.Client
}

func newAnthropicCompleter(cfg ProviderConfig) *anthropicCompleter {
	return &anthropicCompleter{cfg: cfg, client: newHTTPClient()}
}

// anthropicMessage 单条消息（Anthropic 仅 user/assistant；system 单独字段）。
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicRequest 请求体。
// Thinking 用指针 + omitempty：nil（thinking=""）时完全不下发该字段；
// 非 nil 时为 {"type":"disabled"}(off) 或 {"type":"enabled","budget_tokens":N}。
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
}

// anthropicThinking Anthropic 思考参数体（design.md D2 映射表）。
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"` // 仅 enabled 有意义
}

// anthropicThinkingParam 返回 thinking 映射的 anthropicThinking 值。
// "" → nil（不下发）；off → {"type":"disabled"}；
// low/medium/high → {"type":"enabled","budget_tokens":1024/4096/16384}。
func anthropicThinkingParam(thinking string) *anthropicThinking {
	switch thinking {
	case ThinkingOff:
		return &anthropicThinking{Type: "disabled"}
	case ThinkingLow, ThinkingMedium, ThinkingHigh:
		budget, _ := AnthropicThinkingBudget(thinking)
		return &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	default:
		return nil
	}
}

// anthropicAdjustedMaxTokens 按协议约束调整 max_tokens：enabled 时 MUST max_tokens > budget_tokens，
// 不足自动提升为 budget_tokens+512（若请求值更大则保持请求值）（design.md D2）。
func anthropicAdjustedMaxTokens(reqMaxTokens, budgetTokens int) int {
	minRequired := budgetTokens + 512
	if reqMaxTokens > minRequired {
		return reqMaxTokens
	}
	return minRequired
}

// anthropicContentBlock 响应 content 元素。Text 用 *string 区分「字段缺失/null」与「显式空串」。
type anthropicContentBlock struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

// anthropicResponse 响应体。
type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
}

func (c *anthropicCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	endpoint := c.cfg.EffectiveBaseURL() + "/v1/messages"

	buildRequest := func(stripThinking bool) (*http.Request, error) {
		maxTokens := req.MaxTokens
		var thinking *anthropicThinking
		if !stripThinking {
			thinking = anthropicThinkingParam(c.cfg.Thinking)
			if thinking != nil && thinking.Type == "enabled" {
				// 协议约束：max_tokens > budget_tokens，不足自动提升（design.md D2）。
				maxTokens = anthropicAdjustedMaxTokens(req.MaxTokens, thinking.BudgetTokens)
			}
		}
		body := anthropicRequest{
			Model:     c.cfg.Model,
			System:    req.System,
			MaxTokens: maxTokens,
			Messages:  []anthropicMessage{{Role: "user", Content: req.User}},
			Thinking:  thinking,
		}
		httpReq, err := newJSONRequest(endpoint, body)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-api-key", c.cfg.APIKey)
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		return httpReq, nil
	}

	return doJSON(ctx, c.client, c.cfg.Provider, c.cfg.APIKey, buildRequest, parseAnthropic, c.cfg.Thinking != "")
}

func parseAnthropic(data []byte) (string, error) {
	var r anthropicResponse
	if err := jsonUnmarshal(data, &r); err != nil {
		return "", err
	}
	if len(r.Content) == 0 {
		return "", fmt.Errorf("ai: anthropic response missing content")
	}
	var sb strings.Builder
	hasText := false
	for _, b := range r.Content {
		if b.Type != "text" {
			continue
		}
		if b.Text == nil {
			// text 字段缺失或 JSON null → 结构缺失，error。
			return "", fmt.Errorf("ai: anthropic response missing text")
		}
		hasText = true
		sb.WriteString(*b.Text)
	}
	if !hasText {
		// content 数组存在但无 type=="text" block → 结构缺失，error。
		return "", fmt.Errorf("ai: anthropic response missing text block")
	}
	return sb.String(), nil
}