// spi_provider.go 实现 agentprovider.Provider 的 v1 适配器（opencode-v1-spi-provider
// tasks 3.3 / design D7/D8/D11）：复用既有 *opencode.Client 的 HTTP/SSE transport
// （不重写连接层——共享 loopbackTransport 的连接池/空闲回收/代理禁用语义经
// NewClient 天然继承，见 client.go loopbackTransport），在其上实现窄接口与
// typed events 解析。
//
// 阶段 2 中间态（design D6）：RuntimeSupervisor/TerminalIO 的实现体随 task 层
// 消费点迁移（tasks 3.6/4.x）落位；当前返回显式 fail-closed 的迁移占位（携带
// 明确 Op 与说明），不做任何被猜测的行为。
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/process"
)

// v1 provider 身份常量（design D2：本期唯一 provider、统一稳定实例）。
const (
	ProviderID         = "opencode-v1"
	ProviderInstanceID = "default"
)

// v1BindingData 是 TaskBinding.Data 的 v1 私有形状（design D3 envelope 示例）。
// auth.kind: derived（派生凭据，密码不落库，resolveBinding 现场按 TaskID 派生）；
// legacy 例外仅存在于迁移导入期（runner 写入随机密码，下次激活切换 derived）。
// TaskID 为非秘密派生输入（D3 公式入参之一，随 binding 携带供消费方派生）。
type v1BindingData struct {
	Port      int           `json:"port"`
	Directory string        `json:"directory"`
	TaskID    string        `json:"taskId"`
	Auth      v1BindingAuth `json:"auth"`
}

type v1BindingAuth struct {
	Kind     string `json:"kind"`
	Password string `json:"password,omitempty"`
}

// SPIProvider 是 opencode v1 的 Provider 实现。
type SPIProvider struct {
	meta appagent.ProviderMeta
	// newClient 构造指向某 serve（port+password）的 Client；默认 NewClient
	//（测试可注入）。所有窄接口共用同一工厂，继承共享 loopback transport。
	newClient func(port int, password string, opts Options) *Client
	// supDeps 非 nil 时 supervisor/terminal 为真实实现（NewSPIProviderWithDeps）；
	// nil 保持阶段 0 的 fail-closed 占位（阶段 0 测试依赖）。
	supDeps *SupervisorDeps
	// health 是 C6 门禁修复：能力健康上报端口（可选；nil → no-op，测试态）。
	// 适配层在真实能力调用的出口（成功/协议违约/明确不支持）调 `report`——
	// SPI spec「能力故障透出」：真实故障转 unsupported/degraded 并告警，
	// 不得由测试/装配方直接制造目标状态。epoch 读取函数由装配方注入
	//（healthTracker.RuntimeEpoch——换代递增后的当前代际）。
	health *spiHealthPort
}

// spiHealthPort 聚合健康上报的两个注入函数（与 HealthTracker 解耦——
// report 收到已填充 Primitive/Scope/Session/State/Reason 的观测，由装配方
// 补 ProviderID 并路由）。
type spiHealthPort struct {
	report func(appagent.CapabilityHealth)
	epoch  func(taskID string) int64
}

// WithHealthReporter 注入健康上报端口（生产装配；nil 参数移除上报）。
func (p *SPIProvider) WithHealthReporter(
	report func(appagent.CapabilityHealth), epoch func(taskID string) int64) {
	if report == nil || epoch == nil {
		p.health = nil
		return
	}
	p.health = &spiHealthPort{report: report, epoch: epoch}
}

// bindingHealthTaskID 是能力观测的 task 身份键（C6 残余修复：禁止拿 upstream
// session ID 顶替 task ID）。预解析阶段（resolveBinding 前，失败也要上报）仅能
// 从 binding 取 data.taskId——legacy 预迁移形态缺失时留空（epoch 不挂靠、观测
// 以 provider 作用域落账）；resolve 成功后 MUST 经 withResolvedTaskID 补齐
// v1BindingData.TaskID 后再上报业务结果。
func bindingHealthTaskID(b domain.TaskBinding) string {
	var data v1BindingData
	if len(b.Data) > 0 && json.Unmarshal(b.Data, &data) == nil {
		return data.TaskID
	}
	return ""
}

// withResolvedTaskID 用解码后的 v1 data 修正观测身份键（C6：ready/report/release
// /reset 同键——真实 task ID）。bindingHealthTaskID 对可解析 data 已取 taskId，
// 本函数为显式文档化形态（供后续 need 时使用）。
var _ = withResolvedTaskID

func withResolvedTaskID(h appagent.CapabilityHealth, data v1BindingData) appagent.CapabilityHealth {
	if data.TaskID != "" {
		h.TaskID = data.TaskID
	}
	return h
}

// reportConversion 把一次能力调用的结果转换为健康观测并上报（nil port no-op）。
// 状态映射（SPI spec 139-144）：nil → available；CapabilityUnavailable →
// unsupported（明确不支持）；其余 typed 错误（凭据/运行时/协议违约）→ degraded；
// reason 取错误原文（空串表示成功）。
func (p *SPIProvider) reportConversion(h appagent.CapabilityHealth, op string, err error) {
	if p.health == nil {
		return
	}
	state := appagent.CapAvailable
	var reason string
	if err != nil {
		state = appagent.CapDegraded
		// C6 mutation 证据：CapabilityUnavailable→unsupported 映射删除
		//（`var ae *appagent.Error` 分支去除）→ 测试 spi_health_wiring 的
		// 404 title subtest 红（want unsupported, got degraded）。
		var ae *appagent.Error
		if errors.As(err, &ae) && ae.Kind == appagent.ErrKindCapabilityUnavailable {
			state = appagent.CapUnsupported
		}
		reason = fmt.Sprintf("%s: %s", op, err.Error())
	}
	if p.health.epoch != nil && h.TaskID != "" {
		h.Epoch = p.health.epoch(h.TaskID)
	}
	h.State = state
	h.Reason = reason
	p.health.report(h)
}

// NewSPIProvider 构造 v1 Provider。meta 契约区间取值由装配方维护
// （CONTRACT.md 核验记录：[1.18.14, 1.18.26]）。
func NewSPIProvider() *SPIProvider {
	return &SPIProvider{
		meta: appagent.ProviderMeta{
			ProviderID:       ProviderID,
			DisplayName:      "OpenCode v1",
			ContractMin:      ContractMinVersion,
			ContractBaseline: ContractBaseline,
		},
		newClient: NewClient,
	}
}

// 编译期断言：*SPIProvider 实现 Provider 全部窄接口（tasks 3.2/3.3）。
var (
	_ appagent.Provider          = (*SPIProvider)(nil)
	_ appagent.SessionControl    = (*spiSessions)(nil)
	_ appagent.EventSource       = (*spiEvents)(nil)
	_ appagent.AttentionIO       = (*spiAttention)(nil)
	_ appagent.AsyncPrompt       = (*spiPrompts)(nil)
	_ appagent.StatusReader      = (*spiStatus)(nil)
	_ appagent.TerminalIO        = (*spiTerminalStub)(nil)
	_ appagent.TitleUpdater      = (*spiTitles)(nil)
	_ appagent.RuntimeSupervisor = (*spiSupervisorStub)(nil)
	_ appagent.TerminalIO        = supTerminal{}
	_ appagent.RuntimeSupervisor = supDepsAdapter{}
)

func (p *SPIProvider) Meta() appagent.ProviderMeta { return p.meta }

// Supervisor 返回 runtime supervisor：deps 已注入（NewSPIProviderWithDeps）时为
// 真实实现；否则保持阶段 0 的 fail-closed 占位（阶段 0/1 测试依赖该形态）。
func (p *SPIProvider) Supervisor() appagent.RuntimeSupervisor {
	if p.supDeps == nil {
		return spiSupervisorStub{}
	}
	return supDepsAdapter{p: p}
}

func (p *SPIProvider) Sessions() appagent.SessionControl { return &spiSessions{p: p} }
func (p *SPIProvider) Events() appagent.EventSource      { return &spiEvents{p: p} }
func (p *SPIProvider) Attention() appagent.AttentionIO   { return &spiAttention{p: p} }
func (p *SPIProvider) Prompts() appagent.AsyncPrompt     { return &spiPrompts{p: p} }
func (p *SPIProvider) Status() appagent.StatusReader     { return &spiStatus{p: p} }

// Terminal 返回终端端口：deps 注入时为真实实现（EnsureTerminal 存活校验 +
// AttachCommand 经 AttachClientArgv seam），否则 fail-closed 占位。
func (p *SPIProvider) Terminal() appagent.TerminalIO {
	if p.supDeps == nil {
		return spiTerminalStub{}
	}
	return supTerminal{d: p.supDeps, p: p}
}

func (p *SPIProvider) Titles() appagent.TitleUpdater { return &spiTitles{p: p} }

// decodeV1Binding 解码 v1 私有 data 并派生凭据（resolveBinding /
// BindingRuntimeEndpoint 的共享实现；C2 门禁修复——v1 解码收回适配器后 task 层
// 经 BindingRuntimeEndpoint 消费端点三元组，不再镜像本逻辑）。
func decodeV1Binding(b domain.TaskBinding) (v1BindingData, string, error) {
	if b.ProviderID != ProviderID || b.ProviderInstanceID != ProviderInstanceID {
		return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("binding %s/%s 不属于 %s/%s", b.ProviderID, b.ProviderInstanceID, ProviderID, ProviderInstanceID))
	}
	var data v1BindingData
	if len(b.Data) > 0 {
		if err := json.Unmarshal(b.Data, &data); err != nil {
			return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
				fmt.Errorf("decode v1 binding data: %w", err))
		}
	}
	if data.Port <= 0 || data.Directory == "" || data.Auth.Kind == "" {
		return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			errors.New("v1 binding 缺少 port/directory/auth.kind"))
	}
	// 凭据解析（D3）：derived → 现场按（master secret, provider, taskId）派生
	//（密码不落库；主密钥缺失/TaskID 缺失 → BindingInvalid fail-closed）；
	// legacy → 迁移导入的存量随机密码（MUST 非空）。未知 kind fail-closed。
	password := data.Auth.Password
	switch data.Auth.Kind {
	case appagent.DerivedCredKind:
		if data.TaskID == "" {
			return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
				errors.New("derived binding 缺少 taskId（派生入参）"))
		}
		master, merr := appagent.EnsureMasterSecret()
		if merr != nil {
			return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid, merr)
		}
		password = appagent.DeriveRuntimePassword(master, ProviderID, data.TaskID)
	case appagent.LegacyCredKind:
		if password == "" {
			return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
				errors.New("legacy binding 缺少导入密码"))
		}
	default:
		return v1BindingData{}, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("未知 auth.kind %q", data.Auth.Kind))
	}
	return data, password, nil
}

// resolveBinding 校验 binding 归属 v1 并解码私有数据，构造对应 Client。
// 非 v1 binding / 畸形 Data / 缺少端口 → BindingInvalid（fail-closed）。
func (p *SPIProvider) resolveBinding(b domain.TaskBinding) (*Client, v1BindingData, error) {
	data, password, err := decodeV1Binding(b)
	if err != nil {
		return nil, v1BindingData{}, err
	}
	return p.newClient(data.Port, password, Options{}), data, nil
}

// BindingRuntimeEndpoint 从持久化 binding 解出 runtime 端点（C2 门禁修复）：
// v1 命名空间校验 + 私有 data 解码 + 凭据确定性派生/读取，与 resolveBinding
// 同一语义但不构造 Client（task 层 startSSE/ocFactory 桥接消费端点三元组）。
// 非 v1 binding / 畸形 Data / 缺 port → BindingInvalid（fail-closed）。
func (p *SPIProvider) BindingRuntimeEndpoint(b domain.TaskBinding) (port int, password string, directory string, err error) {
	data, pw, everr := decodeV1Binding(b)
	if everr != nil {
		return 0, "", "", everr
	}
	return data.Port, pw, data.Directory, nil
}

// classifyHTTPError 是 D8 默认映射：网络/连接/超时/5xx/契约外形状 →
// RuntimeUnavailable；404 → NotFound；401/403 → BindingInvalid；其余 4xx →
// RuntimeUnavailable。原始错误链保留在 *appagent.Error 内。
func classifyHTTPError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnauthorized) {
		return appagent.NewError(op, appagent.ErrKindBindingInvalid, err)
	}
	if errors.Is(err, ErrSessionNotFound) {
		return appagent.NewError(op, appagent.ErrKindNotFound, err)
	}
	if errors.Is(err, ErrCapabilityUnsupported) {
		return appagent.NewError(op, appagent.ErrKindCapabilityUnavailable, err)
	}
	// Reply 操作级例外（D8）：_tag=PermissionNotFoundError（请求已了结竞态）→
	// NotFound（正常忽略语义）。
	if errors.Is(err, ErrPermissionRequestGone) {
		return appagent.NewError(op, appagent.ErrKindNotFound, err)
	}
	var hse *httpStatusError
	if errors.As(err, &hse) {
		switch {
		case hse.status == 404:
			return appagent.NewError(op, appagent.ErrKindNotFound, err)
		case hse.status == 401 || hse.status == 403:
			return appagent.NewError(op, appagent.ErrKindBindingInvalid, err)
		default:
			return appagent.NewError(op, appagent.ErrKindRuntimeUnavailable, err)
		}
	}
	return appagent.NewError(op, appagent.ErrKindRuntimeUnavailable, err)
}

// --- SessionControl ---

type spiSessions struct{ p *SPIProvider }

func toAgentSession(s Session) domain.AgentSession {
	key := domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: s.ID}
	out := domain.AgentSession{
		Key:       key,
		Title:     s.Title,
		CreatedAt: int64(s.Time.Created),
		UpdatedAt: int64(s.Time.Updated),
	}
	if s.ParentID != "" {
		parent := key
		parent.UpstreamID = s.ParentID
		out.Parent = &parent
	}
	return out
}

// List 经 GET /session?directory=；count==limit 映射 ListResult.Overflow
// （不完整快照信号，非错误——design D7）。
func (a *spiSessions) List(ctx context.Context, b domain.TaskBinding) (appagent.ListResult, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeRuntime}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.list", err)
		return appagent.ListResult{}, err
	}
	sessions, lerr := c.ListSessions(ctx, data.Directory, 1000)
	if lerr != nil && !errors.Is(lerr, ErrSessionOverflow) {
		lerr = classifyHTTPError("sessions.list", lerr)
		a.p.reportConversion(h, "sessions.list", lerr)
		return appagent.ListResult{}, lerr
	}
	out := appagent.ListResult{Overflow: errors.Is(lerr, ErrSessionOverflow)}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, toAgentSession(s))
	}
	a.p.reportConversion(h, "sessions.list", nil)
	return out, nil
}

func (a *spiSessions) Get(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) (domain.AgentSession, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.get", err)
		return domain.AgentSession{}, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "sessions.get", err)
		return domain.AgentSession{}, err
	}
	got, gerr := c.GetSession(ctx, data.Directory, s.UpstreamID)
	if gerr != nil {
		gerr = classifyHTTPError("sessions.get", gerr)
		a.p.reportConversion(h, "sessions.get", gerr)
		return domain.AgentSession{}, gerr
	}
	a.p.reportConversion(h, "sessions.get", nil)
	return toAgentSession(got), nil
}

func (a *spiSessions) Create(ctx context.Context, b domain.TaskBinding) (domain.AgentSession, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeRuntime}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.create", err)
		return domain.AgentSession{}, err
	}
	// D7 形状无 title 参数（标题经 TitleUpdater.Sync 单独承载）。
	created, cerr := c.CreateSession(ctx, data.Directory, "")
	if cerr != nil {
		cerr = classifyHTTPError("sessions.create", cerr)
		a.p.reportConversion(h, "sessions.create", cerr)
		return domain.AgentSession{}, cerr
	}
	a.p.reportConversion(h, "sessions.create", nil)
	return toAgentSession(created), nil
}

func (a *spiSessions) Delete(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) error {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveSessionControl, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "sessions.delete", err)
		return err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "sessions.delete", err)
		return err
	}
	// client 现状：404 幂等成功；其余错误分类。
	err = classifyHTTPError("sessions.delete", c.DeleteSession(ctx, data.Directory, s.UpstreamID))
	a.p.reportConversion(h, "sessions.delete", err)
	return err
}

// validateOwnedKey 校验目标键属于 v1 命名空间（specs 归属规则：仅允许操作
// 本任务拥有的 session；跨 provider 键不得进入 v1 端点）。
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

// --- AttentionIO ---

type spiAttention struct{ p *SPIProvider }

func (a *spiAttention) ListPermissions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.PermissionPayload, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveAttentionIO, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.list_permissions", err)
		return nil, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "attention.list_permissions", err)
		return nil, err
	}
	list, lerr := c.ListPermissions(ctx, data.Directory)
	if lerr != nil {
		lerr = classifyHTTPError("attention.list_permissions", lerr)
		a.p.reportConversion(h, "attention.list_permissions", lerr)
		return nil, lerr
	}
	out := make([]domain.PermissionPayload, 0, len(list))
	for _, pr := range list {
		out = append(out, domain.PermissionPayload{
			ID: pr.ID, SessionID: pr.SessionID, Permission: pr.Permission,
			Patterns: pr.Patterns, Metadata: pr.Metadata,
		})
	}
	a.p.reportConversion(h, "attention.list_permissions", nil)
	return out, nil
}

func (a *spiAttention) ListQuestions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.QuestionPayload, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveAttentionIO, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.list_questions", err)
		return nil, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "attention.list_questions", err)
		return nil, err
	}
	list, lerr := c.ListQuestions(ctx, data.Directory)
	if lerr != nil {
		lerr = classifyHTTPError("attention.list_questions", lerr)
		a.p.reportConversion(h, "attention.list_questions", lerr)
		return nil, lerr
	}
	out := make([]domain.QuestionPayload, 0, len(list))
	for _, q := range list {
		items := make([]domain.QuestionItem, 0, len(q.Questions))
		for _, item := range q.Questions {
			items = append(items, domain.QuestionItem{Header: item.Header, Question: item.Question})
		}
		// 方案 B 双写（design D10）：结构化 Fields 随解析层填充透传，
		// Questions 兼容视图保持 v1 现状。
		out = append(out, domain.QuestionPayload{
			ID: q.ID, SessionID: q.SessionID, Title: q.Title, Fields: q.Fields, Questions: items,
		})
	}
	a.p.reportConversion(h, "attention.list_questions", nil)
	return out, nil
}

// Reply 经 POST /permission/{id}/reply。操作级例外（D8）：按完整响应体 _tag
// 分类——已了结/不存在 → NotFound；能力端点缺失 → CapabilityUnavailable；
// 401 → BindingInvalid；其余（结果未知类）按默认映射。permission reply 值域
// once/reject（task-permission-mode D6 契约，MUST NOT always）。
func (a *spiAttention) Reply(ctx context.Context, b domain.TaskBinding, at domain.Attention, reply appagent.AttentionReply) error {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveAttentionIO, Scope: appagent.ScopeSession, Session: &at.Session}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "attention.reply", err)
		return err
	}
	if at.Kind == domain.AttentionPermission {
		if reply.Action != "once" && reply.Action != "reject" {
			err := fmt.Errorf("attention reply: permission Action %q 越界（契约值域 once/reject）", reply.Action)
			a.p.reportConversion(h, "attention.reply", err)
			return err
		}
		err = classifyHTTPError("attention.reply", c.ReplyPermission(ctx, data.Directory, at.ID, reply.Action))
		a.p.reportConversion(h, "attention.reply", err)
		return err
	}
	// question reply：v1 client 现状无实现方（CONTRACT.md experimental 端点、
	// 无既有调用路径，body 契约未核验）——不猜测实现，显式 fail-closed。
	err = appagent.NewError("attention.reply", appagent.ErrKindCapabilityUnavailable,
		errors.New("v1 client 未实现 question reply（契约未核验，见 CONTRACT.md experimental 端点）"))
	a.p.reportConversion(h, "attention.reply", err)
	return err
}

// --- AsyncPrompt ---

type spiPrompts struct{ p *SPIProvider }

// Send 通道规则（D7 唯一表述）：仅 SPI 准入失败（binding 无效等进入 provider 前）
// 返回 error；进入适配层后严格四分类经 PromptResult 返回（opencode.PromptResult
// 的 Kind/字段填充规则与 D7 逐字一致，机械映射）。
func (a *spiPrompts) Send(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, req appagent.PromptRequest) (appagent.PromptResult, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveAsyncPrompt, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "prompts.send", err)
		return appagent.PromptResult{}, err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "prompts.send", err)
		return appagent.PromptResult{}, err
	}
	files := make([]PromptFilePart, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, PromptFilePart{URL: f.URL, Mime: f.Mime, Filename: f.Filename})
	}
	res := c.PromptAsync(ctx, data.Directory, s.UpstreamID, req.MessageID, req.Text, files)
	out := appagent.PromptResult{
		Outcome:    appagent.PromptOutcome(res.Kind),
		StatusCode: res.StatusCode,
		Body:       res.Body,
		Detail:     res.Detail,
	}
	// 能力级观测仅对 transport 失败形态转 degraded（pre_send_failure=准入/投递
	// 前失败，accepted/http_response/transport_unknown 均属通道在位）。
	if out.Outcome == appagent.PromptPreSendFailure {
		perr := appagent.NewError("prompts.send", appagent.ErrKindRuntimeUnavailable, errors.New(out.Detail))
		a.p.reportConversion(h, "prompts.send", perr)
	} else {
		a.p.reportConversion(h, "prompts.send", nil)
	}
	return out, nil
}

// --- StatusReader ---

type spiStatus struct{ p *SPIProvider }

// Current 经 GET /session/status：map 键为上游 session id，包装为 SessionKey；
// 未知/缺失枚举 → StatusUnknown（D8 REST 路径语义，经 SessionStatusTolerant）。
func (a *spiStatus) Current(ctx context.Context, b domain.TaskBinding) (map[domain.SessionKey]domain.AgentStatus, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveStatusReader, Scope: appagent.ScopeRuntime}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "status.current", err)
		return nil, err
	}
	entries, serr := c.SessionStatusTolerant(ctx, data.Directory)
	if serr != nil {
		serr = classifyHTTPError("status.current", serr)
		a.p.reportConversion(h, "status.current", serr)
		return nil, serr
	}
	out := make(map[domain.SessionKey]domain.AgentStatus, len(entries))
	for sid, st := range entries {
		var state domain.AgentStatus
		switch st.Type {
		case StatusIdle:
			state = domain.StatusIdle
		case StatusBusy:
			state = domain.StatusBusy
		case StatusRetry:
			state = domain.StatusRetry
		default:
			state = domain.StatusUnknown
		}
		out[domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: sid}] = state
	}
	a.p.reportConversion(h, "status.current", nil)
	return out, nil
}

// --- TitleUpdater ---

type spiTitles struct{ p *SPIProvider }

// Sync 经 PATCH /session/{id}。操作级例外（D8）：所有 404（不区分路由级/会话级）
// 一律 CapabilityUnavailable（沿用标题能力降级语义），MUST NOT 套用「对象不存在」
// 默认分类。
func (a *spiTitles) Sync(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, title string) error {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveTitleUpdater, Scope: appagent.ScopeSession, Session: &s}
	c, data, err := a.p.resolveBinding(b)
	if err != nil {
		a.p.reportConversion(h, "titles.sync", err)
		return err
	}
	if err := validateOwnedKey(s); err != nil {
		a.p.reportConversion(h, "titles.sync", err)
		return err
	}
	if err := c.UpdateSessionTitle(ctx, data.Directory, s.UpstreamID, title); err != nil {
		if errors.Is(err, ErrSessionTitleUnsupported) {
			err = appagent.NewError("titles.sync", appagent.ErrKindCapabilityUnavailable, err)
		} else {
			err = classifyHTTPError("titles.sync", err)
		}
		a.p.reportConversion(h, "titles.sync", err)
		return err
	}
	a.p.reportConversion(h, "titles.sync", nil)
	return nil
}

// --- 阶段 0/1 占位（deps 未注入时保留；NewSPIProviderWithDeps 装配真实实现）---

// spiSupervisorStub 是 deps 未注入时的显式 fail-closed 占位（C1 测试依赖该形态；
// 生产装配 MUST 经 NewSPIProviderWithDeps 注入真实 deps）。
type spiSupervisorStub struct{}

func (spiSupervisorStub) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, supervisorPendingError("supervisor.acquire")
}

func (spiSupervisorStub) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, supervisorPendingError("supervisor.resume")
}

func (spiSupervisorStub) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, supervisorPendingError("supervisor.acquire_ephemeral")
}

func (spiSupervisorStub) Release(ctx context.Context, b domain.TaskBinding) error {
	return supervisorPendingError("supervisor.release")
}

// supDepsAdapter 把 SupervisorDeps 路由为 RuntimeSupervisor（真实实现；实现体
// 在 spi_supervisor.go 的 *SPIProvider 方法上）。
type supDepsAdapter struct{ p *SPIProvider }

func (a supDepsAdapter) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return a.p.Acquire(ctx, task)
}
func (a supDepsAdapter) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	return a.p.Resume(ctx, task, old, policy)
}
func (a supDepsAdapter) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return a.p.AcquireEphemeral(ctx, task)
}
func (a supDepsAdapter) Release(ctx context.Context, b domain.TaskBinding) error {
	return a.p.supDeps.releaseBinding(ctx, b)
}

// spiTerminalStub：同 spiSupervisorStub（TerminalIO 的真实实现经 supTerminal）。
type spiTerminalStub struct{}

func (spiTerminalStub) EnsureTerminal(ctx context.Context, b domain.TaskBinding) (string, error) {
	return "", supervisorPendingError("terminal.ensure")
}

func (spiTerminalStub) AttachCommand(ctx context.Context, b domain.TaskBinding) ([]string, error) {
	return nil, supervisorPendingError("terminal.attach")
}

// supTerminal 是 TerminalIO 的真实实现（deps 注入态）：
// EnsureTerminal 校验 runtime 会话存活（HasSession；不存在 → runtime_unavailable）；
// AttachCommand 返回 attach 客户端 argv（经 AttachClientArgv seam——process.Manager
// 的 socket 绑定构造，与 AttachPty 客户端同一形态）。
type supTerminal struct {
	d *SupervisorDeps
	p *SPIProvider // C6：健康上报（nil 安全——测试态 deps 可无 owner provider）
}

func (t supTerminal) EnsureTerminal(ctx context.Context, b domain.TaskBinding) (string, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveTerminalIO, Scope: appagent.ScopeRuntime}
	report := func(err error) {
		if t.p != nil {
			t.p.reportConversion(h, "terminal.ensure", err)
		}
	}
	if b.TerminalRef == "" {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindBindingInvalid,
			errors.New("binding TerminalRef 为空"))
		report(err)
		return "", err
	}
	exists, herr := t.d.Proc.HasSession(b.TerminalRef)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindRuntimeUnavailable, herr)
		report(err)
		return "", err
	}
	if !exists {
		err := appagent.NewError("terminal.ensure", appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("runtime 会话 %s 不存在", b.TerminalRef))
		report(err)
		return "", err
	}
	report(nil)
	return b.TerminalRef, nil
}

func (t supTerminal) AttachCommand(ctx context.Context, b domain.TaskBinding) ([]string, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveTerminalIO, Scope: appagent.ScopeRuntime}
	report := func(err error) {
		if t.p != nil {
			t.p.reportConversion(h, "terminal.attach", err)
		}
	}
	if b.TerminalRef == "" {
		err := appagent.NewError("terminal.attach", appagent.ErrKindBindingInvalid,
			errors.New("binding TerminalRef 为空"))
		report(err)
		return nil, err
	}
	if t.d.AttachClientArgv == nil {
		err := appagent.NewError("terminal.attach", appagent.ErrKindCapabilityUnavailable,
			errors.New("AttachClientArgv seam 未注入"))
		report(err)
		return nil, err
	}
	argv, err := t.d.AttachClientArgv(b.TerminalRef)
	if err != nil {
		err = appagent.NewError("terminal.attach", appagent.ErrKindRuntimeUnavailable, err)
		report(err)
		return nil, err
	}
	report(nil)
	return argv, nil
}

func supervisorPendingError(op string) error {
	return appagent.NewError(op, appagent.ErrKindCapabilityUnavailable,
		errors.New("v1 supervisor/terminal 迁移中（tasks 3.6/4.x；阶段 2 中间态，runtime 生命周期仍由 task 层旧路径承载）"))
}
