// session.go 实现 SessionControl 端口（opencode-v2-provider tasks 3.1）：POST
// /api/session（body 含 location.directory）、GET /api/session（cursor 分页）、
// GET /api/session/{id}、DELETE /api/session/{id}。session ID 按运行时契约
// 发现的 pattern 校验（2.5）。
package opencode2

import (
	"context"
	"errors"
	"fmt"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// v2Namespace 构造 v2 命名空间下的 SessionKey。
func v2Namespace(upstreamID string) domain.SessionKey {
	return domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: upstreamID}
}

// toAgentSession 把 wire Session.Info 映射为 domain.AgentSession（D7：Key=
// {opencode-v2, default, id}；Parent=同命名空间 parentID（无父 nil）；时间
// epoch 毫秒不换算）。
func toAgentSession(s wireSession) domain.AgentSession {
	key := v2Namespace(s.ID)
	out := domain.AgentSession{
		Key:       key,
		Title:     s.Title,
		CreatedAt: int64(*s.Time.Created),
		UpdatedAt: int64(*s.Time.Updated),
	}
	if s.ParentID != "" {
		parent := key
		parent.UpstreamID = s.ParentID
		out.Parent = &parent
	}
	return out
}

// validateOwnedKey 校验目标键属于 v2 命名空间（specs 归属规则：仅允许操作本
// 任务拥有的 session；跨 provider 键不得进入 v2 端点）。
func validateOwnedKey(s domain.SessionKey) error {
	if s.IsZero() {
		return appagent.NewError("validate key", appagent.ErrKindBindingInvalid, errors.New("SessionKey 为零值"))
	}
	if s.ProviderID != ProviderID || s.ProviderInstanceID != ProviderInstanceID {
		return appagent.NewError("validate key", appagent.ErrKindBindingInvalid,
			fmt.Errorf("SessionKey %s 不属于 %s/%s", s, ProviderID, ProviderInstanceID))
	}
	return nil
}

// --- SessionControl ---

type spiSessions struct{ p *SPIProvider }

// List 经 GET /api/session?directory=<d>：cursor 分页取单页（limit=1000）。
// 不完整快照（锚点式游标：len==limit 且 next 非 null——next 非空≠多页、短页=
// 完整，2026-10-20 v2.0.28 实测）→ ListResult.Overflow（非错误——D7）。
func (a *spiSessions) List(ctx context.Context, b domain.TaskBinding) (appagent.ListResult, error) {
	h := a.p.healthBase(b, appagent.PrimitiveSessionControl, appagent.ScopeRuntime, nil)
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.list", err)
		return appagent.ListResult{}, err
	}
	page, lerr := c.ListSessions(ctx, data.Directory, 1000)
	if lerr != nil {
		lerr = classifyHTTPError("sessions.list", lerr)
		a.p.reportConversion(h, "sessions.list", lerr)
		return appagent.ListResult{}, lerr
	}
	out := appagent.ListResult{Overflow: page.HasMore}
	for _, s := range page.Sessions {
		out.Sessions = append(out.Sessions, toAgentSession(s))
	}
	a.p.reportConversion(h, "sessions.list", nil)
	return out, nil
}

func (a *spiSessions) Get(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) (domain.AgentSession, error) {
	h := a.p.healthBase(b, appagent.PrimitiveSessionControl, appagent.ScopeSession, &s)
	c, _, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.get", err)
		return domain.AgentSession{}, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "sessions.get", err)
		return domain.AgentSession{}, err
	}
	got, gerr := c.GetSession(ctx, s.UpstreamID)
	if gerr != nil {
		gerr = classifyHTTPError("sessions.get", gerr)
		a.p.reportConversion(h, "sessions.get", gerr)
		return domain.AgentSession{}, gerr
	}
	a.p.reportConversion(h, "sessions.get", nil)
	return toAgentSession(got), nil
}

// Create 经 POST /api/session（body 含 location.directory）。D7 错误映射：
// 4xx/5xx → ErrKindRuntimeUnavailable（与默认分类不同——创建路径无 404 对象
// 语义）；契约发现失败 → 先于 POST 拒绝（零副作用）；返回 data.id 按运行时
// 发现的 ^ses pattern 校验（3.1），不符 = 协议违约。
func (a *spiSessions) Create(ctx context.Context, b domain.TaskBinding) (domain.AgentSession, error) {
	h := a.p.healthBase(b, appagent.PrimitiveSessionControl, appagent.ScopeRuntime, nil)
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.create", err)
		return domain.AgentSession{}, err
	}
	contract, err := a.p.contractFor(ctx, c)
	if err != nil {
		err = appagent.NewError("sessions.create", appagent.ErrKindCapabilityUnavailable, err)
		a.p.reportConversion(h, "sessions.create", err)
		return domain.AgentSession{}, err
	}
	created, cerr := c.CreateSession(ctx, data.Directory)
	if cerr != nil {
		cerr = appagent.NewError("sessions.create", appagent.ErrKindRuntimeUnavailable, cerr)
		a.p.reportConversion(h, "sessions.create", cerr)
		return domain.AgentSession{}, cerr
	}
	if !contract.ValidSessionID(created.ID) {
		err := appagent.NewError("sessions.create", appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("session id %q 不匹配运行时发现的 pattern %q", created.ID, contract.SessionIDPatternRaw))
		a.p.reportConversion(h, "sessions.create", err)
		return domain.AgentSession{}, err
	}
	a.p.reportConversion(h, "sessions.create", nil)
	return toAgentSession(created), nil
}

// Delete 经 DELETE /api/session/{id}：404 → ErrKindNotFound（D7；v2 不做 v1
// 式「404 幂等成功」——删除仅允许既有任务删除流程与首建补偿受限回收，语义
// 由调用方持有）。
func (a *spiSessions) Delete(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) error {
	h := a.p.healthBase(b, appagent.PrimitiveSessionControl, appagent.ScopeSession, &s)
	c, _, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.delete", err)
		return err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "sessions.delete", err)
		return err
	}
	err = classifyHTTPError("sessions.delete", c.DeleteSession(ctx, s.UpstreamID))
	a.p.reportConversion(h, "sessions.delete", err)
	return err
}
