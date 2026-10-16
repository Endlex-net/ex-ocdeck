// discover.go 实现运行时契约发现（opencode-v2-provider tasks 2.5）：GET
// /openapi.json（注意：根路径，无 /api 前缀——CONTRACT §1）获取并解析
// Session.ID / Session.Message.ID pattern，作为 3.1/4.1 ID 校验的单一来源。
// 获取或解析失败 → 能力保持待验证，MUST NOT 猜默认 pattern（fail-closed）。
package opencode2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// WireContract 是运行时发现的 wire 契约事实（ID pattern 等）。
type WireContract struct {
	// SessionIDPatternRaw / MessageIDPatternRaw 是 OpenAPI 中原样 pattern
	//（如 "^ses" / "^msg_"），供诊断展示。
	SessionIDPatternRaw string
	MessageIDPatternRaw string
	sessionIDRe         *regexp.Regexp
	messageIDRe         *regexp.Regexp
}

// ValidSessionID 报告 id 是否匹配运行时发现的 session ID pattern。
func (w WireContract) ValidSessionID(id string) bool {
	return w.sessionIDRe != nil && w.sessionIDRe.MatchString(id)
}

// ValidMessageID 报告 id 是否匹配运行时发现的 message ID pattern。
func (w WireContract) ValidMessageID(id string) bool {
	return w.messageIDRe != nil && w.messageIDRe.MatchString(id)
}

// openAPIDoc 是 /openapi.json 的最小解析结构（仅契约发现所需）。
type openAPIDoc struct {
	Components struct {
		Schemas map[string]struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"schemas"`
	} `json:"components"`
}

// DiscoverContract 请求 GET /openapi.json 并解析 ID pattern。任一步失败
// （网络/非 200/畸形 JSON/缺 schema/pattern 为空或非法正则）→ 错误，调用方
// 保守处置（对应能力保持待验证），本函数不提供任何默认 pattern。
func (c *Client) DiscoverContract(ctx context.Context) (WireContract, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/openapi.json", nil)
	if err != nil {
		return WireContract{}, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return WireContract{}, fmt.Errorf("%w: %v", ErrContractUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := readBodyForError(resp.Body)
		return WireContract{}, fmt.Errorf("%w: http %d: %s", ErrContractUnavailable, resp.StatusCode, body)
	}
	// 畸形文档 fail-closed：仅允许单一 JSON 值（尾随垃圾视为畸形）。
	dec := json.NewDecoder(resp.Body)
	var doc openAPIDoc
	if err := dec.Decode(&doc); err != nil {
		return WireContract{}, fmt.Errorf("%w: %v", ErrContractUnavailable, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return WireContract{}, fmt.Errorf("%w: trailing data", ErrContractUnavailable)
	}

	rawSession := schemaPattern(doc, "Session.ID")
	rawMessage := schemaPattern(doc, "Session.Message.ID")
	if rawSession == "" || rawMessage == "" {
		return WireContract{}, fmt.Errorf("%w: missing Session.ID/Session.Message.ID pattern", ErrContractUnavailable)
	}
	sessionRe, err := regexp.Compile(rawSession)
	if err != nil {
		return WireContract{}, fmt.Errorf("%w: session pattern %q: %v", ErrContractUnavailable, rawSession, err)
	}
	messageRe, err := regexp.Compile(rawMessage)
	if err != nil {
		return WireContract{}, fmt.Errorf("%w: message pattern %q: %v", ErrContractUnavailable, rawMessage, err)
	}
	return WireContract{
		SessionIDPatternRaw: rawSession,
		MessageIDPatternRaw: rawMessage,
		sessionIDRe:         sessionRe,
		messageIDRe:         messageRe,
	}, nil
}

func schemaPattern(doc openAPIDoc, name string) string {
	s, ok := doc.Components.Schemas[name]
	if !ok {
		return ""
	}
	return s.Pattern
}
