// attention.go 实现 AttentionIO 全接口（opencode-v2-provider tasks 4.2，冻结
// 形态 CONTRACT §5/§6）：ListPermissions = session 级 GET /api/session/{id}/
// permission?directory=<d>（pending 仅存于此）；ListQuestions = form 族
// GET /api/session/{id}/form（pending = Form.Detail.state.status="pending"）；
// Reply = permission reply body {decision} / form reply body {answer}（均
// 204）。端点 4xx/5xx 或不可解析时如实报错透出能力健康（不得当作无 pending）。
package opencode2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// wirePermissionRequest 是 v2 Permission.Request 最小消费形状（required
// id/sessionID/action/resources；R2 pending-observation 实证同形）。
type wirePermissionRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Action    string          `json:"action"`
	Resources []string        `json:"resources"`
	Metadata  json.RawMessage `json:"metadata"`
}

// parseWirePermissionRequest 校验必填形状（缺失 = 协议违约，整体失败——不得
// 当作无 pending）。
func parseWirePermissionRequest(raw []byte) (wirePermissionRequest, error) {
	var r wirePermissionRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return wirePermissionRequest{}, fmt.Errorf("opencode2: parse permission request: %w", err)
	}
	if r.ID == "" || r.SessionID == "" || r.Action == "" {
		return wirePermissionRequest{}, errors.New("opencode2: permission request: missing id/sessionID/action")
	}
	r.Metadata = normalizeMetadata(r.Metadata)
	return r, nil
}

// normalizeMetadata 归一化 metadata：有效形状仅为 JSON object——缺失/null/
// 非法/非 object 一律归 nil（MUST NOT 丢弃请求；v1 同语义）。
func normalizeMetadata(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil
	}
	return trimmed
}

// ListPermissions GET /api/session/{id}/permission?directory=<d> →
// {data: [Permission.Request]}。404/其他 4xx/5xx → 错误透出（能力健康降级），
// MUST NOT 折算为空列表。
func (c *Client) ListPermissions(ctx context.Context, dir, sessionID string) ([]wirePermissionRequest, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raws []json.RawMessage
	if err := c.getJSONData(ctx, c.httpClient, "/session/"+url.PathEscape(sessionID)+"/permission", q, &raws); err != nil {
		return nil, err
	}
	out := make([]wirePermissionRequest, 0, len(raws))
	for _, raw := range raws {
		req, err := parseWirePermissionRequest(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, nil
}

// --- form 族（question 等价） ---

// wireFormOption 是 Form.Option（value/label required）。
type wireFormOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// normalizeFormType 是 v2 wire 字段类型 → SPI QuestionFieldType 的有限映射
// （design D10 冻结）：string/text→text、boolean→boolean、number→number、
// enum→select、multiselect（array enum）→multiselect；未知类型（integer/
// external 等）保留原值，MUST NOT 静默降级为 text。
func normalizeFormType(t string) string {
	switch t {
	case "string", "text":
		return string(domain.QuestionTypeText)
	case "boolean":
		return string(domain.QuestionTypeBoolean)
	case "number":
		return string(domain.QuestionTypeNumber)
	case "enum":
		return string(domain.QuestionTypeSelect)
	case "multiselect":
		return string(domain.QuestionTypeMultiselect)
	default:
		return t
	}
}

// mapFormFields 把 v2 form 字段按原生 key 结构化映射（design D10）：
// key→Key、title→Label、description→Prompt、type→normalizeFormType；
// 不再拼接文本、不再按位置 zip。G2-F7：字段保留原始对象——已建模属性之外的
// 属性（hidden/when/format/default/custom 等）原样归入 QuestionField.Metadata
// （仅保留未入模型的字段，不得成为消费方主路径）；无未建模属性时 Metadata 为
// nil。key/type 缺失（schema required）= 协议违约。
func mapFormFields(raws []json.RawMessage) ([]domain.QuestionField, error) {
	out := make([]domain.QuestionField, 0, len(raws))
	for _, raw := range raws {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("opencode2: form field: %w", err)
		}
		var f domain.QuestionField
		if err := json.Unmarshal(obj["key"], &f.Key); err != nil || f.Key == "" {
			return nil, errors.New("opencode2: form field: missing key")
		}
		if err := json.Unmarshal(obj["type"], &f.Type); err != nil || f.Type == "" {
			return nil, fmt.Errorf("opencode2: form field %s: missing type", f.Key)
		}
		f.Type = normalizeFormType(f.Type)
		_ = json.Unmarshal(obj["title"], &f.Label)
		_ = json.Unmarshal(obj["description"], &f.Prompt)
		_ = json.Unmarshal(obj["required"], &f.Required)
		if opts, ok := obj["options"]; ok {
			var wireOpts []wireFormOption
			if err := json.Unmarshal(opts, &wireOpts); err != nil {
				return nil, fmt.Errorf("opencode2: form field %s: options: %w", f.Key, err)
			}
			f.Options = make([]domain.QuestionOption, 0, len(wireOpts))
			for _, o := range wireOpts {
				f.Options = append(f.Options, domain.QuestionOption{Value: o.Value, Label: o.Label})
			}
		}
		// 未建模属性 → Metadata（保留原文 bytes；键排序保证确定性输出）。
		modeled := map[string]bool{"key": true, "type": true, "title": true,
			"description": true, "required": true, "options": true}
		var leftover []string
		for k := range obj {
			if !modeled[k] {
				leftover = append(leftover, k)
			}
		}
		if len(leftover) > 0 {
			sort.Strings(leftover)
			meta := make(map[string]json.RawMessage, len(leftover))
			for _, k := range leftover {
				meta[k] = obj[k]
			}
			m, err := json.Marshal(meta)
			if err != nil {
				return nil, fmt.Errorf("opencode2: form field %s: metadata: %w", f.Key, err)
			}
			f.Metadata = m
		}
		out = append(out, f)
	}
	return out, nil
}

// wireFormInfo 是 Form.Info（required id/sessionID/title/fields；list 响应
// 元素无 state——pending 判定需逐条取 detail）。Fields 保留原始对象
// （G2-F7：未建模属性归入 QuestionField.Metadata——design D10）。
type wireFormInfo struct {
	ID        string            `json:"id"`
	SessionID string            `json:"sessionID"`
	Title     string            `json:"title"`
	Fields    []json.RawMessage `json:"fields"`
}

// wireFormDetail 是 Form.Detail（Form.Info + state；state.status ∈
// pending/answered/cancelled）。
type wireFormDetail struct {
	wireFormInfo
	State struct {
		Status string `json:"status"`
	} `json:"state"`
}

// ListSessionForms GET /api/session/{id}/form → {data: [Form.Info]}。
func (c *Client) ListSessionForms(ctx context.Context, sessionID string) ([]wireFormInfo, error) {
	var raws []json.RawMessage
	if err := c.getJSONData(ctx, c.httpClient, "/session/"+url.PathEscape(sessionID)+"/form", nil, &raws); err != nil {
		return nil, err
	}
	out := make([]wireFormInfo, 0, len(raws))
	for _, raw := range raws {
		var f wireFormInfo
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("opencode2: parse form info: %w", err)
		}
		if f.ID == "" || f.SessionID == "" {
			return nil, errors.New("opencode2: form info: missing id/sessionID")
		}
		out = append(out, f)
	}
	return out, nil
}

// GetForm GET /api/session/{id}/form/{formID} → {data: Form.Detail}。
func (c *Client) GetForm(ctx context.Context, sessionID, formID string) (wireFormDetail, error) {
	var raw json.RawMessage
	path := "/session/" + url.PathEscape(sessionID) + "/form/" + url.PathEscape(formID)
	if err := c.getJSONData(ctx, c.httpClient, path, nil, &raw); err != nil {
		return wireFormDetail{}, err
	}
	var d wireFormDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return wireFormDetail{}, fmt.Errorf("opencode2: parse form detail: %w", err)
	}
	if d.ID == "" || d.SessionID == "" {
		return wireFormDetail{}, errors.New("opencode2: form detail: missing id/sessionID")
	}
	return d, nil
}

// ReplyPermission POST /api/session/{id}/permission/{requestID}/reply?directory=<d>
// body {decision} → 204（D7 冻结形态：带 directory query——R2 once 204 实证）。
// decision 值域 once/always/reject（v2 冻结枚举，较 v1 放开 always）。404 且
// _tag=PermissionNotFoundError → ErrPermissionGone（重复回复/幽灵 ID）。
func (c *Client) ReplyPermission(ctx context.Context, dir, sessionID, requestID, decision string) error {
	q := url.Values{}
	q.Set("directory", dir)
	body, err := json.Marshal(map[string]string{"decision": decision})
	if err != nil {
		return fmt.Errorf("opencode2: reply permission: marshal body: %w", err)
	}
	path := "/session/" + url.PathEscape(sessionID) + "/permission/" + url.PathEscape(requestID) + "/reply"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL(path, q), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if errorTag(readBodyForError(resp.Body)) == "PermissionNotFoundError" {
			return ErrPermissionGone
		}
		return &httpStatusError{status: resp.StatusCode}
	}
	return classifyStatus(resp)
}

// ReplyForm POST /api/session/{id}/form/{formID}/reply body {answer} → 204
// （D7 冻结形态：无 directory query——OpenAPI session.form.reply 仅 sessionID/
// formID 路径参数）。409 _tag=FormAlreadySettledError → ErrFormSettled；404
// _tag=FormNotFoundError → ErrFormNotFound。
func (c *Client) ReplyForm(ctx context.Context, sessionID, formID string, answer map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"answer": answer})
	if err != nil {
		return fmt.Errorf("opencode2: reply form: marshal body: %w", err)
	}
	path := "/session/" + url.PathEscape(sessionID) + "/form/" + url.PathEscape(formID) + "/reply"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL(path, nil), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if errorTag(readBodyForError(resp.Body)) == "FormNotFoundError" {
			return ErrFormNotFound
		}
		return &httpStatusError{status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusConflict {
		if errorTag(readBodyForError(resp.Body)) == "FormAlreadySettledError" {
			return ErrFormSettled
		}
		return &httpStatusError{status: resp.StatusCode}
	}
	return classifyStatus(resp)
}

// --- AttentionIO（SPI） ---

type spiAttention struct{ p *SPIProvider }

func (a *spiAttention) ListPermissions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.PermissionPayload, error) {
	h := a.p.healthBase(b, appagent.PrimitiveAttentionIO, appagent.ScopeSession, &s)
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.list_permissions", err)
		return nil, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "attention.list_permissions", err)
		return nil, err
	}
	list, lerr := c.ListPermissions(ctx, data.Directory, s.UpstreamID)
	if lerr != nil {
		lerr = classifyHTTPError("attention.list_permissions", lerr)
		a.p.reportConversion(h, "attention.list_permissions", lerr)
		return nil, lerr
	}
	out := make([]domain.PermissionPayload, 0, len(list))
	for _, pr := range list {
		out = append(out, domain.PermissionPayload{
			ID: pr.ID, SessionID: pr.SessionID, Permission: pr.Action,
			Patterns: pr.Resources, Metadata: pr.Metadata,
		})
	}
	a.p.reportConversion(h, "attention.list_permissions", nil)
	return out, nil
}

// ListQuestions 经 form 族：list → 逐条取 detail → state.status=="pending"
// 过滤（list 元素无 state——CONTRACT §6）。结构化映射已冻结（design D10）：
// QuestionPayload 按原生 key 映射 Fields（mapFormFields）+ Title，不再填充
// Questions 兼容视图（拼接文本已移除）。
func (a *spiAttention) ListQuestions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.QuestionPayload, error) {
	h := a.p.healthBase(b, appagent.PrimitiveAttentionIO, appagent.ScopeSession, &s)
	c, _, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.list_questions", err)
		return nil, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "attention.list_questions", err)
		return nil, err
	}
	forms, lerr := c.ListSessionForms(ctx, s.UpstreamID)
	if lerr != nil {
		lerr = classifyHTTPError("attention.list_questions", lerr)
		a.p.reportConversion(h, "attention.list_questions", lerr)
		return nil, lerr
	}
	out := make([]domain.QuestionPayload, 0, len(forms))
	for _, f := range forms {
		detail, derr := c.GetForm(ctx, s.UpstreamID, f.ID)
		if derr != nil {
			derr = classifyHTTPError("attention.list_questions", derr)
			a.p.reportConversion(h, "attention.list_questions", derr)
			return nil, derr
		}
		if detail.State.Status != "pending" {
			continue
		}
		fields, ferr := mapFormFields(detail.Fields)
		if ferr != nil {
			// 字段形状违约（缺 key/type 等）→ 协议违约透出，不得当作无 pending。
			ferr = appagent.NewError("attention.list_questions", appagent.ErrKindRuntimeUnavailable, ferr)
			a.p.reportConversion(h, "attention.list_questions", ferr)
			return nil, ferr
		}
		out = append(out, domain.QuestionPayload{
			ID:        detail.ID,
			SessionID: detail.SessionID,
			Title:     detail.Title,
			Fields:    fields,
		})
	}
	a.p.reportConversion(h, "attention.list_questions", nil)
	return out, nil
}

// Reply 经对应 reply 端点。permission：decision 值域校验（越界 = 参数错误）；
// form：answer 对象按键构造——取 detail 的 fields[i].key 与 reply.Answers[i]
// 逐位 zip（Answers 为 string 数组；Form.Value 的非 string 值域不开放——
// 仅开放 string 答案）。404 → NotFound、409 FormAlreadySettled → Conflict
// （D7 错误映射）。
func (a *spiAttention) Reply(ctx context.Context, b domain.TaskBinding, at domain.Attention, reply appagent.AttentionReply) error {
	h := a.p.healthBase(b, appagent.PrimitiveAttentionIO, appagent.ScopeSession, &at.Session)
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.reply", err)
		return err
	}
	if at.Kind == domain.AttentionPermission {
		switch reply.Action {
		case "once", "always", "reject":
		default:
			err := fmt.Errorf("attention reply: permission Action %q 越界（契约值域 once/always/reject）", reply.Action)
			a.p.reportConversion(h, "attention.reply", err)
			return err
		}
		err = classifyHTTPError("attention.reply",
			c.ReplyPermission(ctx, data.Directory, at.Session.UpstreamID, at.ID, reply.Action))
		a.p.reportConversion(h, "attention.reply", err)
		return err
	}
	// form reply（design D10）：Values 优先（key-based），Answers 兼容回退；
	// 校验 fail-closed（未知/重复 key、必填缺失、数量不匹配）且失败不触达
	// reply 端点（零 wire 副作用）。不再按位置 zip。
	detail, derr := c.GetForm(ctx, at.Session.UpstreamID, at.ID)
	if derr != nil {
		derr = classifyHTTPError("attention.reply", derr)
		a.p.reportConversion(h, "attention.reply", derr)
		return derr
	}
	fields, ferr := mapFormFields(detail.Fields)
	if ferr != nil {
		// 字段形状违约 → 协议违约透出；不触达 reply 端点（零 wire 副作用）。
		ferr = appagent.NewError("attention.reply", appagent.ErrKindRuntimeUnavailable, ferr)
		a.p.reportConversion(h, "attention.reply", ferr)
		return ferr
	}
	payload := domain.QuestionPayload{ID: detail.ID, SessionID: detail.SessionID, Fields: fields}
	answers, aerr := reply.EffectiveAnswers(payload)
	if aerr != nil {
		a.p.reportConversion(h, "attention.reply", aerr)
		return aerr
	}
	answer := make(map[string]interface{}, len(answers))
	for _, ans := range answers {
		answer[ans.Key] = ans.Value
	}
	err = classifyHTTPError("attention.reply",
		c.ReplyForm(ctx, at.Session.UpstreamID, at.ID, answer))
	a.p.reportConversion(h, "attention.reply", err)
	return err
}
