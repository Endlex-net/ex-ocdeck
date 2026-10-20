// prompt.go 实现 AsyncPrompt 端口（opencode-v2-provider tasks 4.1，D7 映射）：
// POST /api/session/{id}/prompt body {text, id}；成功响应 200 {data} 必须解包
// envelope 并校验 data.id 回显（协议违约按 http_response 透出，不得 accepted）；
// 附件 files:FileAttachment[] {uri(必), name?, description?, mention?}——无
// mime 字段（PromptFile.Mime 丢弃）；inline base64 未实证不开放（仅 uri 引用）。
// MessageID 按运行时发现的 ^msg_ pattern 校验（透传不重写；不匹配/契约不可用
// → pre_send_failure）。未知交付（transport_unknown）禁止重投：适配层内部零重试。
package opencode2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// fileAttachment 是 v2 FileAttachment（uri 必填；mime 字段上游不存在）。
type fileAttachment struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// promptBody 是 POST /api/session/{id}/prompt 请求体（CONTRACT §3）。
type promptBody struct {
	ID    string           `json:"id"`
	Text  string           `json:"text"`
	Files []fileAttachment `json:"files,omitempty"`
}

type spiPrompts struct{ p *SPIProvider }

// Send 通道规则（D7 唯一）：仅 SPI 准入失败（binding 无效等进入适配层前）
// 返回 error；进入适配层后严格四分类经 PromptResult 返回——accepted（上游
// 2xx，v2 为 200 {data}，归一化 accepted 且保留实际码供诊断、Body 空）、
// http_response（非 2xx：实际码 + 有界截断体）、transport_unknown（Do 失败，
// 不推断是否已发出）、pre_send_failure（进入适配层后的编码/构造失败，含
// MessageID pattern 校验不符与契约不可用）。
func (a *spiPrompts) Send(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, req appagent.PromptRequest) (appagent.PromptResult, error) {
	h := a.p.healthBase(b, appagent.PrimitiveAsyncPrompt, appagent.ScopeSession, &s)
	c, _, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "prompts.send", err)
		return appagent.PromptResult{}, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "prompts.send", err)
		return appagent.PromptResult{}, err
	}
	// MessageID pattern 校验（4.1：按运行时发现的 ^msg_ pattern；契约不可用
	// = 能力待验证，保守拒绝发送——零副作用）。
	contract, err := a.p.contractFor(ctx, c)
	if err != nil {
		a.p.reportConversion(h, "prompts.send",
			appagent.NewError("prompts.send", appagent.ErrKindCapabilityUnavailable, err))
		return appagent.PromptResult{
			Outcome: appagent.PromptPreSendFailure,
			Detail:  fmt.Sprintf("runtime contract discovery unavailable: %v", err),
		}, nil
	}
	if !contract.ValidMessageID(req.MessageID) {
		a.p.reportConversion(h, "prompts.send", nil)
		return appagent.PromptResult{
			Outcome: appagent.PromptPreSendFailure,
			Detail:  fmt.Sprintf("messageID %q 不匹配运行时发现的 pattern %q", req.MessageID, contract.MessageIDPatternRaw),
		}, nil
	}
	files := make([]fileAttachment, 0, len(req.Files))
	for _, f := range req.Files {
		// Mime 丢弃（上游无对应字段）；uri=URL、name=Filename（D7 冻结映射）。
		files = append(files, fileAttachment{URI: f.URL, Name: f.Filename})
	}
	body, err := json.Marshal(promptBody{ID: req.MessageID, Text: req.Text, Files: files})
	if err != nil {
		a.p.reportConversion(h, "prompts.send", nil)
		return appagent.PromptResult{
			Outcome: appagent.PromptPreSendFailure,
			Detail:  fmt.Sprintf("marshal body: %v", err),
		}, nil
	}
	reqURL := c.apiURL("/session/"+url.PathEscape(s.UpstreamID)+"/prompt", nil)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		a.p.reportConversion(h, "prompts.send", nil)
		return appagent.PromptResult{
			Outcome: appagent.PromptPreSendFailure,
			Detail:  fmt.Sprintf("new request: %v", err),
		}, nil
	}
	httpReq.SetBasicAuth(c.username, c.password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// transport_unknown：结果未知（MUST NOT 推断是否已发出）；禁止重投。
		a.p.reportConversion(h, "prompts.send", nil)
		return appagent.PromptResult{
			Outcome: appagent.PromptTransportUnknown,
			Detail:  err.Error(),
		}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		// 冻结契约（CONTRACT §3）：v2 成功 = 200 {data}，data.id 与请求 id
		// 原值回显。2xx 必须解包 {data} envelope 并校验回显——空体/畸形
		// envelope/缺 data/ID 不一致/超限/读取失败属协议违约或无法核验，
		// 不得报 accepted：按 http_response 透出（StatusCode=实际码、
		// Body=诊断截断原文，D7 字段绑定不变），能力健康降级。
		// G2-F6：校验基于上限内完整正文；4KB 截断仅用于诊断填充。
		respBytes, rerr := readBodyComplete(resp.Body)
		var echoed struct {
			ID string `json:"id"`
		}
		envErr := rerr
		if envErr == nil {
			envErr = decodeEnvelope(bytes.NewReader(respBytes), &echoed)
		}
		if envErr == nil && echoed.ID != req.MessageID {
			envErr = fmt.Errorf("data.id %q 与请求 id %q 不一致", echoed.ID, req.MessageID)
		}
		if envErr != nil {
			perr := fmt.Errorf("prompt response 协议违约: %v", envErr)
			a.p.reportConversion(h, "prompts.send",
				appagent.NewError("prompts.send", appagent.ErrKindRuntimeUnavailable, perr))
			return appagent.PromptResult{
				Outcome:    appagent.PromptHTTPResponse,
				StatusCode: resp.StatusCode,
				Body:       truncateForDiagnostics(respBytes),
			}, nil
		}
		// 合法回显：归一化 accepted，保留实际码供诊断，Body/Detail 空。
		a.p.reportConversion(h, "prompts.send", nil)
		return appagent.PromptResult{
			Outcome:    appagent.PromptAccepted,
			StatusCode: resp.StatusCode,
		}, nil
	}
	a.p.reportConversion(h, "prompts.send", nil)
	return appagent.PromptResult{
		Outcome:    appagent.PromptHTTPResponse,
		StatusCode: resp.StatusCode,
		Body:       readBodyForError(resp.Body),
	}, nil
}

// recordStatus 供事件路径更新状态追踪（execution 边界事件；见 status.go）。
// 同一临界区内分配并返回本次写入代（评审 Round3 I12：写入与代际归属原子——
// 交错订阅下流记录的必须是「自己这次写入」的代，重读全局当前代会把别流的新代
// 记到自己名下，断线时误删新流事实）。
func (p *SPIProvider) recordStatus(key domain.SessionKey, state domain.AgentStatus) uint64 {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	if p.status == nil {
		p.status = map[string]domain.AgentStatus{}
	}
	if p.statusGen == nil {
		p.statusGen = map[string]uint64{}
	}
	p.statusGenCtr++
	gen := p.statusGenCtr
	p.status[key.UpstreamID] = state
	p.statusGen[key.UpstreamID] = gen
	return gen
}

// currentStatusGen 返回 session 当前状态写入代（无记录 → false——无可失效
// 事实）。仅供订阅方领受既有事实责任时快照；写入方代际由 recordStatus 同临界区
// 返回（Round3 I12：写路径 MUST NOT 重读全局当前代）。
func (p *SPIProvider) currentStatusGen(upstreamID string) (uint64, bool) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	gen, ok := p.statusGen[upstreamID]
	return gen, ok
}

// invalidateStatusGen 按代失效 session 状态（评审 I12）：仅当当前代仍等于记录代
// （本流写入后无更新写入）才删除——同目录多订阅下，新流刚写入的事实不被旧流
// 的断线清除。
func (p *SPIProvider) invalidateStatusGen(upstreamID string, gen uint64) {
	p.statusMu.Lock()
	if p.statusGen[upstreamID] == gen {
		delete(p.status, upstreamID)
		delete(p.statusGen, upstreamID)
	}
	p.statusMu.Unlock()
}
