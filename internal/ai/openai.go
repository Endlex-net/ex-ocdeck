// OpenAI provider 实现（design.md D2）。
//
// 端点：{base}/v1/chat/completions（默认 https://api.openai.com）。
// 认证：Authorization: Bearer <api_key>。
// 请求体：{model, messages:[{role:system|user, content}], max_tokens}。
// 响应取值：choices[0].message.content。
// 失败语义：非 2xx / 超时 / 解析失败 / 结构缺失 / 1MB 超限 → error，不重试。

package ai

import (
	"context"
	"fmt"
	"net/http"
)

// openaiCompleter 实现 Completer。
type openaiCompleter struct {
	cfg    ProviderConfig
	client *http.Client
}

func newOpenAICompleter(cfg ProviderConfig) *openaiCompleter {
	return &openaiCompleter{cfg: cfg, client: newHTTPClient()}
}

// openaiMessage 单条消息（role: system|user）。
type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiRequest 请求体。
// ReasoningEffort 用指针 + omitempty：nil（thinking=""）时完全不下发该字段；
// 非 nil 时下发 "minimal"(off) 或 "low"/"medium"/"high"。
type openaiRequest struct {
	Model           string          `json:"model"`
	Messages        []openaiMessage `json:"messages"`
	MaxTokens       int             `json:"max_tokens"`
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
}

// openaiResponse 响应体（仅取必要字段）。Message 用指针区分「字段缺失」与「存在」；
// Content 用 *string 区分「字段缺失/null」与「显式空串」。
type openaiResponse struct {
	Choices []struct {
		Message *struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// openaiReasoningEffort 返回 thinking 映射的 reasoning_effort 值（design.md D2 映射表）。
// "" → nil（不下发）；off → "minimal"；low/medium/high → 同名。
func openaiReasoningEffort(thinking string) *string {
	switch thinking {
	case ThinkingOff:
		s := "minimal"
		return &s
	case ThinkingLow, ThinkingMedium, ThinkingHigh:
		s := thinking
		return &s
	default:
		return nil
	}
}

func (c *openaiCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	endpoint := c.cfg.EffectiveBaseURL() + "/v1/chat/completions"

	buildRequest := func(stripThinking bool) (*http.Request, error) {
		body := openaiRequest{
			Model:     c.cfg.Model,
			MaxTokens: req.MaxTokens,
			Messages:  make([]openaiMessage, 0, 2),
		}
		if req.System != "" {
			body.Messages = append(body.Messages, openaiMessage{Role: "system", Content: req.System})
		}
		body.Messages = append(body.Messages, openaiMessage{Role: "user", Content: req.User})
		if !stripThinking {
			body.ReasoningEffort = openaiReasoningEffort(c.cfg.Thinking)
		}
		httpReq, err := newJSONRequest(endpoint, body)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		return httpReq, nil
	}

	return doJSON(ctx, c.client, c.cfg.Provider, c.cfg.APIKey, buildRequest, parseOpenAI, c.cfg.Thinking != "")
}

func parseOpenAI(data []byte) (string, error) {
	var r openaiResponse
	if err := jsonUnmarshal(data, &r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("ai: openai response missing choices")
	}
	if r.Choices[0].Message == nil {
		// choices 元素存在但 message 缺失 → 结构缺失，error。
		return "", fmt.Errorf("ai: openai response missing message")
	}
	if r.Choices[0].Message.Content == nil {
		// content 字段缺失或 JSON null → 结构缺失，error。
		return "", fmt.Errorf("ai: openai response missing content")
	}
	// content 显式空串可放行（SlugNamer 门禁兜底）。
	return *r.Choices[0].Message.Content, nil
}