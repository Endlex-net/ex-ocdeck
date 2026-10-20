// attention.go 实现 GET /permission、GET /question 的 REST 快照端点
// （design.md D6 注意力对账）。DTO 类型与 SSE 家族解析器已迁移至
// application/agentprovider/occlient.go（C2a 中性化），本包经 neutral_compat.go
// 别名引用；本文件仅保留 client 方法与响应形状校验。
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

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
	// metadata 归一化（task-permission-mode D12）：缺失/null/非法 JSON/非 object → nil。
	r.Metadata = normalizePermissionMetadata(r.Metadata)
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
	// 方案 B 双写（design D10）：v1 每个 QuestionItem 一对一映射为结构化字段
	//（合成稳定 key question-N，非上游 ID）；Fields 与 Questions 双视图同源
	//（EffectiveFields 为合成规则唯一来源）。v1 reply 上游契约未冻结，维持
	// spi_provider 既有 ErrKindCapabilityUnavailable，不猜测。
	r.Fields = r.EffectiveFields()
	return r, nil
}
