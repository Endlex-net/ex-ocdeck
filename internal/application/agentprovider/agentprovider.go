// Package agentprovider 定义 provider SPI 的 application 端口与 registry
// （opencode-v1-spi-provider design D7 下半 / D5）：按消费者拆分的窄接口集合、
// typed error（ErrorKind sentinel + errors.Is 按 Kind 匹配）、能力健康内存维护
// 与 provider registry（未注册 fail-closed）。
//
// 依赖方向：本包 import internal/domain/agentprovider；infrastructure 实现 port
// 并被 registry 注册；task 层仅消费本包（design D11）。
package agentprovider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	domain "ocdeck/internal/domain/agentprovider"
)

// TaskRef 是 provider 入口上下文：任务标识 + 运行目录 + 已解析的有效模式。
type TaskRef struct {
	TaskID    string
	Directory string
	Mode      string // worktree | local-path
}

// ProviderMeta 是 provider 注册元信息。
type ProviderMeta struct {
	ProviderID       string
	DisplayName      string
	ContractMin      string // 已验证契约区间下限
	ContractBaseline string // 已验证契约区间上限
}

// EventStream 是 volatile 事件流：断流/溢出即终止，Err() 返回终止原因；重连与
// 断流后对齐由调用方负责（specs「typed events 契约」）。
type EventStream interface {
	Events() <-chan domain.AgentEvent
	Err() error
	Close() error
	// SetCallbacks 注入连接生命周期回调（C10 门禁修复）：onConnected 在实际 SSE
	// 连接建立（首事件到达）时同步触发——订阅受理不冒充连接建立；onReconnect 在
	// 重连建立后、新连接事件派发前触发——调用方兑现重连对齐屏障。实现方 MAY 忽略
	//（stub 测试实现无连接生命周期）。
	// C16 门禁修复（既有 spec「断流降级」合同扩展至 typed EventStream）：onDisconnected
	// 在已建立连接终止、退避前同步触发（实现 MUST 桥接 Provider 内部连接层契约——
	// 主动取消不触发）。SetCallbacks MUST 在 Subscribe 返回后消费循环前调用；实现保证
	// 先到的连接信号不丢（补发/重放，C15）。onDisconnected 为 nil 时实现跳过。
	SetCallbacks(onConnected, onReconnect func())
	// SetDisconnectCallback 注入断流回调（C16）：纳入连接生命周期契约面——typed 流
	// 与原始通道同语义（已建立连接终止 → 退避前同步一次；主动 ctx 取消不触发）。
	// 可选能力（实现 MAY 不支持，默认 no-op——测试 stub）。
	SetDisconnectCallback(cb func())
}

// AttentionReply 是 permission/question 回复：permission 的 Action 取值集合以
// CONTRACT.md 锚点为准；question 为答案文本数组（封装按 v1 client 现状搬运）。
type AttentionReply struct {
	Action  string
	Answers []string
}

// ListResult 承载列表边界语义（D7）：Overflow 为不完整快照信号，不是错误——
// 对齐路径 MUST 按 complete=false 处理，MUST NOT 按普通错误中止；锚定列表校验
// 对 overflow 的既有失败语义保留。
type ListResult struct {
	Sessions []domain.AgentSession
	Overflow bool
}

// ResumePolicy 承载恢复入口差异（D9）：AllowSessionCreate/AllowRebuild 按入口
// 映射；BeforeLaunch 是 supervisor 内部每次启动进程前的准入闸门（permit/退避/
// 继续校验），MUST 先于端口分配与任何进程启动副作用（预算所有权留在 task 层）。
type ResumePolicy struct {
	AllowSessionCreate bool
	AllowRebuild       bool
	BeforeLaunch       func(ctx context.Context) error
}

// RuntimeSupervisor 是任务运行时生命周期端口（D7/D9）。
type RuntimeSupervisor interface {
	// Acquire 激活：启动 runtime 并建立绑定。只确定并返回最终 binding，
	// MUST NOT 自行持久化（binding 提交协议见 D9）。
	Acquire(ctx context.Context, task TaskRef) (domain.TaskBinding, error)
	// Resume 恢复/重建；old 为旧绑定（terminal_ref 失效时按 specs 重建语义）。
	Resume(ctx context.Context, task TaskRef, old domain.TaskBinding, policy ResumePolicy) (domain.TaskBinding, error)
	// AcquireEphemeral 删除清理专用：一次性内存临时绑定——MUST NOT 持久化、
	// 不执行锚定 bootstrap、不创建 session、不建立 SSE、不提交任务状态。
	AcquireEphemeral(ctx context.Context, task TaskRef) (domain.TaskBinding, error)
	// Release 挂起/删除：幂等；失败由调用方按 cleanup notice 处置。
	Release(ctx context.Context, binding domain.TaskBinding) error
}

// SessionControl 是会话 CRUD 端口（directory-scoped；Delete 仅允许删除本任务
// 拥有的 session——specs 归属规则）。
type SessionControl interface {
	List(ctx context.Context, b domain.TaskBinding) (ListResult, error)
	Get(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) (domain.AgentSession, error)
	Create(ctx context.Context, b domain.TaskBinding) (domain.AgentSession, error) // 返回上游 ID 供原子 claim
	Delete(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) error
}

// EventSource 是 SSE 订阅端口（订阅对象恒为最终 runtime，D9）。
type EventSource interface {
	Subscribe(ctx context.Context, b domain.TaskBinding) (EventStream, error)
}

// AttentionIO 是注意力信号端口（REST 对账快照 + 回复；Reply 目标不存在/已了结
// → ErrorKind=NotFound，能力端点缺失 → CapabilityUnavailable）。
type AttentionIO interface {
	ListPermissions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.PermissionPayload, error)
	ListQuestions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.QuestionPayload, error)
	Reply(ctx context.Context, b domain.TaskBinding, a domain.Attention, reply AttentionReply) error
}

// PromptOutcome 保留 v1 prompt_async 的四类发送结果（prompt_async.go 现状）；
// 至多一次发送决策依赖该分类，不得压平为 error。
type PromptOutcome string

const (
	PromptAccepted         PromptOutcome = "accepted"
	PromptHTTPResponse     PromptOutcome = "http_response"
	PromptTransportUnknown PromptOutcome = "transport_unknown"
	PromptPreSendFailure   PromptOutcome = "pre_send_failure"
)

// PromptFile 是附件（text 由 PromptRequest.Text 承载，不进入 Files）。
type PromptFile struct {
	URL      string // file:// 绝对路径 URI
	Mime     string
	Filename string
}

// PromptRequest 是异步投递请求；MessageID 为调用方生成的幂等标识，透传不重写。
type PromptRequest struct {
	Text      string
	MessageID string
	Files     []PromptFile
}

// PromptResult 字段填充规则与 Outcome 绑定（唯一，沿用 prompt_async.go 现状）：
// accepted=204/空；http_response=实际状态码+有界截断体；transport_unknown/
// pre_send_failure=0/Detail=底层错误文本。
type PromptResult struct {
	Outcome    PromptOutcome
	StatusCode int
	Body       string
	Detail     string
}

// AsyncPrompt 是异步投递端口（D7 通道规则唯一：仅 SPI 准入失败返回 error——
// binding 无效、provider 未注册等进入 provider 前；进入适配层后严格四分类经
// PromptResult 返回，httpClient.Do 失败一律 transport_unknown，MUST NOT 推断
// 请求是否已发出）。
type AsyncPrompt interface {
	Send(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, req PromptRequest) (PromptResult, error)
}

// StatusReader 是状态读取端口（REST 路径：未知枚举 → StatusUnknown）。
type StatusReader interface {
	Current(ctx context.Context, b domain.TaskBinding) (map[domain.SessionKey]domain.AgentStatus, error)
}

// TerminalIO 是终端端口（v1 经 tmux 兑现：输入/输出由 attach 后直连承担）。
type TerminalIO interface {
	EnsureTerminal(ctx context.Context, b domain.TaskBinding) (terminalRef string, err error)
	AttachCommand(ctx context.Context, b domain.TaskBinding) (argv []string, err error)
}

// TitleUpdater 是标题写入端口（title 为写入目标）。
type TitleUpdater interface {
	Sync(ctx context.Context, b domain.TaskBinding, s domain.SessionKey, title string) error
}

// Provider 聚合全部窄接口（D7）；实现方（v1 适配器）经 registry 注册。
type Provider interface {
	Meta() ProviderMeta
	Supervisor() RuntimeSupervisor
	Sessions() SessionControl
	Events() EventSource
	Attention() AttentionIO
	Prompts() AsyncPrompt
	Status() StatusReader
	Terminal() TerminalIO
	Titles() TitleUpdater
	// BindingRuntimeEndpoint 从持久化 binding 解出 runtime 端点（端点/凭据/运行目录，
	// opencode-v1-spi-provider C2 门禁修复：task 层禁止解码 provider 私有 data，
	// 秘密凭据由 provider 适配层确定性派生/读取）。
	BindingRuntimeEndpoint(b domain.TaskBinding) (port int, password string, directory string, err error)
}

// --- typed error（D7）---

// ErrorKind 是错误类别（稳定字符串值）。
type ErrorKind string

const (
	ErrKindProviderNotFound      ErrorKind = "provider_not_found"
	ErrKindBindingInvalid        ErrorKind = "binding_invalid"
	ErrKindRuntimeUnavailable    ErrorKind = "runtime_unavailable"
	ErrKindNotFound              ErrorKind = "not_found"
	ErrKindConflict              ErrorKind = "conflict"
	ErrKindCapabilityUnavailable ErrorKind = "capability_unavailable"
)

// 包级 sentinel：Error.Is 按 Kind 匹配对应 sentinel（errors.Is 可用）。
var (
	ErrProviderNotFound      = errors.New("agentprovider: provider not found")
	ErrBindingInvalid        = errors.New("agentprovider: binding invalid")
	ErrRuntimeUnavailable    = errors.New("agentprovider: runtime unavailable")
	ErrNotFound              = errors.New("agentprovider: not found")
	ErrConflict              = errors.New("agentprovider: conflict")
	ErrCapabilityUnavailable = errors.New("agentprovider: capability unavailable")
)

// sentinelByKind 是 Kind → sentinel 的唯一映射（Is 匹配用）。
var sentinelByKind = map[ErrorKind]error{
	ErrKindProviderNotFound:      ErrProviderNotFound,
	ErrKindBindingInvalid:        ErrBindingInvalid,
	ErrKindRuntimeUnavailable:    ErrRuntimeUnavailable,
	ErrKindNotFound:              ErrNotFound,
	ErrKindConflict:              ErrConflict,
	ErrKindCapabilityUnavailable: ErrCapabilityUnavailable,
}

// KindOf 返回 err 链上首个 *Error 的 Kind（非 *Error 返回空串）。
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// Error 包装 provider 边界错误：Kind 是分类（sentinel 匹配）；Op 是出错操作名；
// Err 保留 provider 原始错误链（errors.As 可取）。
type Error struct {
	Kind ErrorKind
	Op   string
	Err  error
}

// NewError 构造 *Error（Op/Kind 必填，Err 可为 nil）。
func NewError(op string, kind ErrorKind, err error) *Error {
	return &Error{Kind: kind, Op: op, Err: err}
}

func (e *Error) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("agentprovider: %s: %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("agentprovider: %s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Is 按 Kind 匹配包级 sentinel（未知 Kind 不匹配任何 sentinel）。
func (e *Error) Is(target error) bool {
	if s, ok := sentinelByKind[e.Kind]; ok {
		return s == target
	}
	return target == error(e)
}

// CleanupPayload 是 supervisor 建程/清理阶段失败的 cleanup 观察载荷（C12：
// application 层可识别契约——task 层据此进入 notice/debt replay，不经
// infrastructure 错误类型）。
type CleanupPayload struct {
	SessionName string
	Reason      string
	Tickets     []string
	Retryable   bool
}

// StageError 是带阶段语义与 cleanup 载荷的 provider 错误（C12：task 层把
// supervisor 建程失败分派到恢复失败矩阵——probe→终态补偿、claim→终态补偿、
// cleanup 非净→notice/debt 重放、其余按 Kind 重试/终态）。
// Op/Kind 语义与 Error 相同（如 "supervisor.probe"/"supervisor.claim_anchor"）。
type StageError struct {
	Op      string
	Kind    ErrorKind
	Err     error
	Cleanup *CleanupPayload
}

// NewStageError 构造无 cleanup 载荷的阶段错误。
func NewStageError(op string, kind ErrorKind, err error) *StageError {
	return &StageError{Op: op, Kind: kind, Err: err}
}

// CleanupStageError 构造带 cleanup 载荷的阶段错误（supervisor 建程/清理失败
// 已产生 tickets/disposition 时使用——task 层据 Cleanup 持久化 notice/debt）。
func CleanupStageError(op string, payload CleanupPayload, cause error) *StageError {
	return &StageError{Op: op, Kind: ErrKindRuntimeUnavailable, Err: cause, Cleanup: &payload}
}

// StageErrorOf 检出错误链中的 *StageError（errors.As 语义）。
func StageErrorOf(err error) (*StageError, bool) {
	var se *StageError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

func (e *StageError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("agentprovider: %s: %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("agentprovider: %s: %s: %v", e.Op, e.Kind, e.Err)
}

// Unwrap 链接底层错误（errors.Is/As 穿透到 cause）。
func (e *StageError) Unwrap() error { return e.Err }

// --- 能力健康（D7；透出通道属 tasks 3.8）---

// CapabilityState 是能力健康状态（degraded 为新增态，仅契约违约场景）。
type CapabilityState string

const (
	CapUnknown     CapabilityState = "unknown"
	CapAvailable   CapabilityState = "available"
	CapUnsupported CapabilityState = "unsupported"
	CapDegraded    CapabilityState = "degraded"
)

// CapabilityScope 是能力健康的作用域。
type CapabilityScope string

const (
	ScopeProvider CapabilityScope = "provider"
	ScopeRuntime  CapabilityScope = "runtime"
	ScopeSession  CapabilityScope = "session"
)

// CapabilityPrimitive 是能力原语名（与窄接口一一对应）。
type CapabilityPrimitive string

const (
	PrimitiveSessionControl CapabilityPrimitive = "session_control"
	PrimitiveEventSource    CapabilityPrimitive = "event_source"
	PrimitiveAttentionIO    CapabilityPrimitive = "attention_io"
	PrimitiveAsyncPrompt    CapabilityPrimitive = "async_prompt"
	PrimitiveStatusReader   CapabilityPrimitive = "status_reader"
	PrimitiveTerminalIO     CapabilityPrimitive = "terminal_io"
	PrimitiveTitleUpdater   CapabilityPrimitive = "title_updater"
)

// CapabilityHealth 是单个能力原语的健康观测值（内存态，不持久化）。
// Epoch 为 runtime 实例代际：Acquire 成功或 Resume 重建成功时递增；server 重启
// 后归零为 unknown。
type CapabilityHealth struct {
	ProviderID string
	TaskID     string
	Session    *domain.SessionKey
	Primitive  CapabilityPrimitive
	Scope      CapabilityScope
	Epoch      int64
	State      CapabilityState
	Reason     string
}

// identity 是健康记录的去重键（provider+task+session+primitive+scope）。
func (h CapabilityHealth) identity() string {
	sess := "-"
	if h.Session != nil {
		sess = h.Session.String()
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s", h.ProviderID, h.TaskID, sess, h.Primitive, h.Scope)
}

// HealthTracker 是能力健康的内存维护器（并发安全；按 identity upsert，不持久化）。
// 迁移日志（warn）、runtime 代际与 provider 预填机制见 health.go（tasks 3.8）。
type HealthTracker struct {
	mu         sync.Mutex
	entries    map[string]CapabilityHealth
	epochs     map[string]int64 // taskID → runtime 代际（Acquire/Resume 重建递增）
	providerID string           // 装配期登记的 provider 标识
}

// Report 按 identity upsert 一条健康记录（无迁移日志；观测上报的安静路径）。
func (t *HealthTracker) Report(h CapabilityHealth) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[h.identity()] = h
}

// Snapshot 返回全部健康记录（按 identity 排序的稳定拷贝）。
func (t *HealthTracker) Snapshot() []CapabilityHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]CapabilityHealth, 0, len(t.entries))
	for _, h := range t.entries {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].identity() < out[j].identity() })
	return out
}

// --- registry（D5）---

// Registry 是 provider 注册表（map[providerID]Provider）；任务 provider 未注册
// 时 fail-closed（Get 返回 ErrorKind=ErrKindProviderNotFound）。
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register 注册 provider；providerID 重复注册返回错误（启动期装配错误，
// 调用方 fail-closed 拒绝启动）。
func (r *Registry) Register(p Provider) error {
	meta := p.Meta()
	if meta.ProviderID == "" {
		return fmt.Errorf("agentprovider: register: provider ID 为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[meta.ProviderID]; dup {
		return fmt.Errorf("agentprovider: register: provider %s 已注册", meta.ProviderID)
	}
	r.providers[meta.ProviderID] = p
	return nil
}

// Get 按 providerID 查找；未注册返回 *Error{Kind: ErrKindProviderNotFound}
// （fail-closed，调用方零副作用处置）。
func (r *Registry) Get(providerID string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[providerID]
	if !ok {
		return nil, NewError("registry.get", ErrKindProviderNotFound,
			fmt.Errorf("provider %q 未注册", providerID))
	}
	return p, nil
}

// List 返回已注册 provider 的元信息（按 ProviderID 排序）。
func (r *Registry) List() []ProviderMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderMeta, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Meta())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}
