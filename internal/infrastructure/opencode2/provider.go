// provider.go 聚合 v2 Provider 适配器（opencode-v2-provider design D1/D2）：
// 身份常量 opencode-v2/default、binding 解码与凭据派生（同一全局 server 全部
// 任务共用同一凭据——派生标识 = runtime 实例标识 default）、归属校验端口、
// 运行时契约发现缓存与能力健康上报。RuntimeSupervisor 为阶段 3 显式
// fail-closed 占位（tasks 5.x 落位）。
package opencode2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// v2 provider 身份常量（design D2；docs/design/multi-agent-spi.md 稳定值表）。
const (
	ProviderID         = "opencode-v2"
	ProviderInstanceID = "default"
	// ContractVersion v2 契约冻结基线（CONTRACT.md 锚点）。
	ContractVersion = "2.0.28"
)

// SPIProvider 是 opencode v2 的 Provider 实现。
type SPIProvider struct {
	meta appagent.ProviderMeta
	// newClient 构造指向共享 serve 的 Client；默认 NewClient（测试可注入）。
	newClient func(port int, password string, opts Options) *Client
	// opencodeBin 是 AttachCommand argv[0]（交互 TUI 二进制；构造期注入）。
	opencodeBin string
	// statusVerified 是 StatusReader 冻结映射切换开关（D8-1：实证已 PASS，
	// 切换为显式装配决策；默认 false = 保守态）。
	statusVerified bool
	// health 是能力健康上报端口（可选；nil → no-op，测试态）。
	health *spiHealthPort
	// supDeps 非 nil 时 Supervisor() 为真实 A+ 承载（NewSPIProviderWithDeps）；
	// nil 保持阶段 2 的 fail-closed 占位。
	supDeps *SupervisorDeps

	contractMu sync.Mutex
	contract   *WireContract

	statusMu sync.Mutex
	status   map[string]domain.AgentStatus
}

// spiHealthPort 聚合健康上报的两个注入函数（v1 同构：report 收到已填充的
// 观测，由装配方补 ProviderID 并路由）。
type spiHealthPort struct {
	report func(appagent.CapabilityHealth)
	epoch  func(taskID string) int64
}

// ProviderOptions 是 NewSPIProvider 的显式依赖注入参数（无 Fx；零值可用）。
type ProviderOptions struct {
	// OpencodeBin 交互 TUI 二进制路径/名称（AttachCommand argv[0]；空 =
	// "opencode"）。
	OpencodeBin string
	// StatusVerified 切换 StatusReader 到冻结映射（D8-1；默认保守态）。
	StatusVerified bool
}

// NewSPIProvider 构造 v2 Provider。meta 契约区间 = 冻结基线 2.0.28
// （CONTRACT.md；升级 SOP 见该文件）。
func NewSPIProvider(opts ProviderOptions) *SPIProvider {
	bin := opts.OpencodeBin
	if bin == "" {
		bin = "opencode"
	}
	return &SPIProvider{
		meta: appagent.ProviderMeta{
			ProviderID:       ProviderID,
			DisplayName:      "OpenCode v2",
			ContractMin:      ContractVersion,
			ContractBaseline: ContractVersion,
		},
		newClient:      NewClient,
		opencodeBin:    bin,
		statusVerified: opts.StatusVerified,
	}
}

// NewSPIProviderWithDeps 构造带真实 A+ supervisor 承载的 v2 Provider
// （tasks 5.1；deps 完整性由装配方保证）。
func NewSPIProviderWithDeps(opts ProviderOptions, supDeps *SupervisorDeps) *SPIProvider {
	p := NewSPIProvider(opts)
	if supDeps != nil {
		supDeps.OpencodeBin = p.opencodeBin
		p.supDeps = supDeps
	}
	return p
}

// 编译期断言：*SPIProvider 实现 Provider 聚合与归属校验窄接口。
var (
	_ appagent.Provider                = (*SPIProvider)(nil)
	_ appagent.BindingOwnershipChecker = (*SPIProvider)(nil)
	_ appagent.SessionControl          = (*spiSessions)(nil)
	_ appagent.EventSource             = (*spiEvents)(nil)
	_ appagent.AttentionIO             = (*spiAttention)(nil)
	_ appagent.AsyncPrompt             = (*spiPrompts)(nil)
	_ appagent.StatusReader            = (*spiStatus)(nil)
	_ appagent.TerminalIO              = (*spiTerminal)(nil)
	_ appagent.TitleUpdater            = (*spiTitles)(nil)
	_ appagent.RuntimeSupervisor       = (*V2Supervisor)(nil)
)

func (p *SPIProvider) Meta() appagent.ProviderMeta { return p.meta }

// Supervisor 返回 runtime supervisor：deps 已注入（NewSPIProviderWithDeps）时
// 为真实 A+ 承载；否则保持阶段 2 的 fail-closed 占位（tasks 5.x 前测试依赖）。
func (p *SPIProvider) Supervisor() appagent.RuntimeSupervisor {
	if p.supDeps == nil {
		return spiSupervisorStub{}
	}
	sup := NewV2Supervisor(*p.supDeps)
	sup.contractOf = p.contractFor
	return sup
}

func (p *SPIProvider) Sessions() appagent.SessionControl { return &spiSessions{p: p} }
func (p *SPIProvider) Events() appagent.EventSource      { return &spiEvents{p: p} }
func (p *SPIProvider) Attention() appagent.AttentionIO   { return &spiAttention{p: p} }
func (p *SPIProvider) Prompts() appagent.AsyncPrompt     { return &spiPrompts{p: p} }
func (p *SPIProvider) Status() appagent.StatusReader     { return &spiStatus{p: p} }
func (p *SPIProvider) Terminal() appagent.TerminalIO     { return spiTerminal{p: p} }
func (p *SPIProvider) Titles() appagent.TitleUpdater     { return &spiTitles{p: p} }

// WithHealthReporter 注入健康上报端口（生产装配；nil 参数移除上报）。
func (p *SPIProvider) WithHealthReporter(
	report func(appagent.CapabilityHealth), epoch func(taskID string) int64) {
	if report == nil || epoch == nil {
		p.health = nil
		return
	}
	p.health = &spiHealthPort{report: report, epoch: epoch}
}

// healthBase 构造能力观测基底（task 身份键 = 私有 data.taskId；C6 同语义）。
func (p *SPIProvider) healthBase(b domain.TaskBinding, primitive appagent.CapabilityPrimitive,
	scope appagent.CapabilityScope, session *domain.SessionKey) appagent.CapabilityHealth {
	return appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: primitive, Scope: scope, Session: session}
}

// bindingHealthTaskID 是能力观测的 task 身份键（预解析阶段仅能从 data 取
// taskId——解码失败留空，观测以 provider 作用域落账）。
func bindingHealthTaskID(b domain.TaskBinding) string {
	var data v2BindingData
	if len(b.Data) > 0 && json.Unmarshal(b.Data, &data) == nil {
		return data.TaskID
	}
	return ""
}

// reportConversion 把一次能力调用的结果转换为健康观测并上报（nil port no-op）。
// 状态映射与 v1 同构：nil → available；CapabilityUnavailable → unsupported；
// 其余 typed 错误 → degraded。
func (p *SPIProvider) reportConversion(h appagent.CapabilityHealth, op string, err error) {
	if p.health == nil {
		return
	}
	state := appagent.CapAvailable
	var reason string
	if err != nil {
		state = appagent.CapDegraded
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

// derivePassword 按冻结公式派生共享 serve 凭据（D2）：第三参 = runtime 实例
// 标识 default——同一 server 全部任务共用同一凭据（注意与 v1 的按 taskId 派生
// 不同）；任务身份与目录归属仍按 binding 独立校验。
func derivePassword() (string, error) {
	master, err := appagent.EnsureMasterSecret()
	if err != nil {
		return "", fmt.Errorf("opencode2: resolve master secret: %w", err)
	}
	return appagent.DeriveRuntimePassword(master, ProviderID, ProviderInstanceID), nil
}

// decodeV2Binding 校验 binding 归属 v2 并解码私有 data（结构层，不派生凭据——
// D2 阶段一/阶段二分立；凭据仅在 resolveBinding/BindingRuntimeEndpoint 派生）。
func decodeV2Binding(b domain.TaskBinding) (v2BindingData, error) {
	if b.ProviderID != ProviderID || b.ProviderInstanceID != ProviderInstanceID {
		return v2BindingData{}, appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("binding %s/%s 不属于 %s/%s", b.ProviderID, b.ProviderInstanceID, ProviderID, ProviderInstanceID))
	}
	data, err := decodeV2BindingData(b.Data)
	if err != nil {
		return v2BindingData{}, appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid, err)
	}
	return data, nil
}

// resolveBinding 校验 binding、解码私有 data 并派生凭据，构造对应 Client。
// 校验顺序（D2）：envelope 结构（task 层 DecodeTaskBinding 已承载）→ data
// 解码校验 → 凭据派生 → 网络。
func (p *SPIProvider) resolveBinding(b domain.TaskBinding) (*Client, v2BindingData, error) {
	data, err := decodeV2Binding(b)
	if err != nil {
		return nil, v2BindingData{}, err
	}
	password, err := derivePassword()
	if err != nil {
		return nil, v2BindingData{}, appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid, err)
	}
	return p.newClient(data.Port, password, Options{}), data, nil
}

// contractFor 返回运行时契约发现结果（2.5：单一来源缓存；获取/解析失败 →
// 错误透出，缓存保持 nil，MUST NOT 猜默认 pattern）。
func (p *SPIProvider) contractFor(ctx context.Context, c *Client) (WireContract, error) {
	p.contractMu.Lock()
	defer p.contractMu.Unlock()
	if p.contract != nil {
		return *p.contract, nil
	}
	w, err := c.DiscoverContract(ctx)
	if err != nil {
		return WireContract{}, err
	}
	p.contract = &w
	return w, nil
}

// CheckBindingOwnership 实现 BindingOwnershipChecker（D2 阶段一）：不派生
// 凭据，解私有字段逐字段比对 taskId/directory；不符/解码失败 →
// ErrKindBindingInvalid。
func (p *SPIProvider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	data, err := decodeV2Binding(b)
	if err != nil {
		return err
	}
	if data.TaskID != expectedTaskID {
		return appagent.NewError("check binding ownership", appagent.ErrKindBindingInvalid,
			fmt.Errorf("binding taskId %q 与任务事实 %q 不一致", data.TaskID, expectedTaskID))
	}
	if data.Directory != expectedDirectory {
		return appagent.NewError("check binding ownership", appagent.ErrKindBindingInvalid,
			fmt.Errorf("binding directory %q 与任务 worktree %q 不一致", data.Directory, expectedDirectory))
	}
	return nil
}

// BindingRuntimeEndpoint 从持久化 binding 解出 runtime 端点（阶段二端口——
// 返回值含凭据，MUST NOT 用于阶段一归属比对）：v2 命名空间校验 + 私有 data
// 解码 + 凭据确定性派生（共享凭据，按实例标识 default）。
func (p *SPIProvider) BindingRuntimeEndpoint(b domain.TaskBinding) (port int, password string, directory string, err error) {
	data, err := decodeV2Binding(b)
	if err != nil {
		return 0, "", "", err
	}
	password, err = derivePassword()
	if err != nil {
		return 0, "", "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid, err)
	}
	return data.Port, password, data.Directory, nil
}

// classifyHTTPError 是 D7 默认错误映射：网络/超时/5xx/契约外形状 →
// RuntimeUnavailable；404 → NotFound；401/403 → BindingInvalid；操作级
// sentinel（PermissionGone/FormNotFound → NotFound、FormSettled → Conflict、
// 契约不可用 → CapabilityUnavailable）。原始错误链保留在 *appagent.Error 内。
func classifyHTTPError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrUnauthorized):
		return appagent.NewError(op, appagent.ErrKindBindingInvalid, err)
	case errors.Is(err, ErrSessionNotFound):
		return appagent.NewError(op, appagent.ErrKindNotFound, err)
	case errors.Is(err, ErrPermissionGone), errors.Is(err, ErrFormNotFound):
		return appagent.NewError(op, appagent.ErrKindNotFound, err)
	case errors.Is(err, ErrFormSettled):
		return appagent.NewError(op, appagent.ErrKindConflict, err)
	case errors.Is(err, ErrContractUnavailable):
		return appagent.NewError(op, appagent.ErrKindCapabilityUnavailable, err)
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

// --- 阶段 3 占位（RuntimeSupervisor；tasks 5.x 落位） ---

// spiSupervisorStub 是阶段 2 的显式 fail-closed 占位（v1 阶段 0 同构形态）。
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

func supervisorPendingError(op string) error {
	return appagent.NewError(op, appagent.ErrKindCapabilityUnavailable,
		errors.New("v2 supervisor 阶段 3 落位（tasks 5.x；A+ 全局单 server 生命周期）"))
}
