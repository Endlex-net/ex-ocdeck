// Package agentprovider 定义 provider-neutral 的 SPI 契约类型（opencode-v1-spi-provider
// design D7 上半）：会话归属键（SessionKey）、任务绑定（TaskBinding 版本化 envelope）、
// typed events（AgentEvent 逐 Kind 单负载）与注意力/状态/错误负载。
//
// 本包为 domain 层：仅承载值对象、枚举与不变量，不依赖 application/infrastructure。
// 稳定字符串值是跨进程契约（持久化列、SSE/DTO），MUST NOT 变更既有取值。
package agentprovider

import (
	"encoding/json"
	"errors"
	"fmt"
)

// --- 身份与绑定 ---

// SessionKey 会话归属唯一键：provider 标识 + provider 实例标识（后端数据域）+
// 上游 session id。本期 v1 全部归属使用统一命名空间（design D2）。
type SessionKey struct {
	ProviderID         string
	ProviderInstanceID string
	UpstreamID         string
}

// IsZero 报告键是否为零值（任一字段为空即视为不完整，不得作为归属键）。
func (k SessionKey) IsZero() bool {
	return k.ProviderID == "" || k.ProviderInstanceID == "" || k.UpstreamID == ""
}

// String 输出日志/诊断用紧凑表示（非持久化格式）。
func (k SessionKey) String() string {
	return k.ProviderID + "/" + k.ProviderInstanceID + "/" + k.UpstreamID
}

// TaskBinding 是任务级运行时绑定的版本化 envelope（design D3）：
// 非秘密连接数据（端点/目录/锚定/终端引用）的事实来源。provider 私有数据按
// Version 解码（Data 为原始 JSON，由 provider 适配层解释）；秘密凭据一律派生，
// 不入 envelope（legacy 导入期例外见 D3，属阶段 3）。
type TaskBinding struct {
	Version            int
	ProviderID         string
	ProviderInstanceID string
	UpstreamSessionID  string
	TerminalRef        string
	Data               json.RawMessage
}

// bindingEnvelope 是 TaskBinding 的持久化 JSON 形状（列内存储格式，字段名稳定）。
type bindingEnvelope struct {
	Version            int             `json:"version"`
	ProviderID         string          `json:"provider_id"`
	ProviderInstanceID string          `json:"provider_instance_id"`
	UpstreamSessionID  string          `json:"upstream_session_id"`
	TerminalRef        string          `json:"terminal_ref"`
	Data               json.RawMessage `json:"data,omitempty"`
}

// 当前 envelope 版本。
const TaskBindingVersion = 1

// Encode 序列化为列存储 JSON。结构完整性（provider/instance/锚定 session/终端引用）
// 不满足时返回错误——不完整绑定不得落库（design D3 解析规则的写侧对偶）。
func (b TaskBinding) Encode() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(bindingEnvelope{
		Version:            b.Version,
		ProviderID:         b.ProviderID,
		ProviderInstanceID: b.ProviderInstanceID,
		UpstreamSessionID:  b.UpstreamSessionID,
		TerminalRef:        b.TerminalRef,
		Data:               b.Data,
	})
	if err != nil {
		return "", fmt.Errorf("agentprovider: encode task binding: %w", err)
	}
	return string(raw), nil
}

// Validate 校验 envelope 结构完整性与版本值。
func (b TaskBinding) Validate() error {
	if b.Version != TaskBindingVersion {
		return fmt.Errorf("agentprovider: task binding version %d 未知（支持 %d）", b.Version, TaskBindingVersion)
	}
	if b.ProviderID == "" || b.ProviderInstanceID == "" || b.UpstreamSessionID == "" || b.TerminalRef == "" {
		return errors.New("agentprovider: task binding 缺少必填字段（provider/instance/upstream/terminal_ref）")
	}
	return nil
}

// DecodeTaskBinding 解析列存储 JSON 为 TaskBinding。未知版本 fail-closed；
// JSON 畸形 / 字段缺失 / 类型错误 fail-closed（design D3：任一失败即按恢复失败
// 处理，不猜测）。Data 保留原始 JSON 交由 provider 适配层按 Version 解释。
// ErrBindingEnvelopeCorrupt 标记 TaskBinding envelope 解码/校验失败（区别于
// store 读取失败的行缺失/DB 故障）——迁移导入 runner 据此把「binding 无法解析」
// 分派进来源标记判定（specs「迁移导入桥接」），而 store 读取失败 fail-closed。
var ErrBindingEnvelopeCorrupt = errors.New("agentprovider: task binding envelope corrupt")

func DecodeTaskBinding(raw string) (TaskBinding, error) {
	var env bindingEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return TaskBinding{}, fmt.Errorf("%w: %v", ErrBindingEnvelopeCorrupt, err)
	}
	b := TaskBinding{
		Version:            env.Version,
		ProviderID:         env.ProviderID,
		ProviderInstanceID: env.ProviderInstanceID,
		UpstreamSessionID:  env.UpstreamSessionID,
		TerminalRef:        env.TerminalRef,
		Data:               env.Data,
	}
	if err := b.Validate(); err != nil {
		return TaskBinding{}, fmt.Errorf("%w: %v", ErrBindingEnvelopeCorrupt, err)
	}
	return b, nil
}

// --- 事件 ---

// EventKind 是 typed event 的种类（稳定字符串值）。
type EventKind string

const (
	EventSessionCreated    EventKind = "SessionCreated"
	EventSessionUpdated    EventKind = "SessionUpdated"
	EventSessionDeleted    EventKind = "SessionDeleted"
	EventStatusChanged     EventKind = "StatusChanged"
	EventAttentionRaised   EventKind = "AttentionRaised"
	EventAttentionResolved EventKind = "AttentionResolved"
	EventSessionError      EventKind = "SessionError"
)

// AgentEvent 是 provider 归一化后的事件（design D7）：按 Kind 取用恰好一个非
// nil 负载字段；Key 恒非零。构造经 New* 工厂保证不变量，Validate 供反序列化/
// 测试复核。
type AgentEvent struct {
	Kind      EventKind
	Key       SessionKey
	Session   *AgentSession       // SessionCreated/Updated/Deleted 时非 nil
	Status    *StatusDetail       // StatusChanged 时非 nil
	Attention *Attention          // AttentionRaised/Resolved 时非 nil
	Error     *SessionErrorDetail // SessionError 时非 nil
}

// NewSessionEvent 构造会话生命周期事件（created/updated/deleted）。
func NewSessionEvent(kind EventKind, s AgentSession) (AgentEvent, error) {
	if kind != EventSessionCreated && kind != EventSessionUpdated && kind != EventSessionDeleted {
		return AgentEvent{}, fmt.Errorf("agentprovider: %s 不是会话生命周期事件 Kind", kind)
	}
	if s.Key.IsZero() {
		return AgentEvent{}, errors.New("agentprovider: 会话事件的 SessionKey 恒非零")
	}
	return AgentEvent{Kind: kind, Key: s.Key, Session: &s}, nil
}

// NewStatusEvent 构造状态变更事件。
func NewStatusEvent(key SessionKey, d StatusDetail) (AgentEvent, error) {
	if key.IsZero() {
		return AgentEvent{}, errors.New("agentprovider: 状态事件的 SessionKey 恒非零")
	}
	return AgentEvent{Kind: EventStatusChanged, Key: key, Status: &d}, nil
}

// NewAttentionEvent 构造注意力事件（raised/resolved）。
func NewAttentionEvent(kind EventKind, a Attention) (AgentEvent, error) {
	if kind != EventAttentionRaised && kind != EventAttentionResolved {
		return AgentEvent{}, fmt.Errorf("agentprovider: %s 不是注意力事件 Kind", kind)
	}
	if a.ID == "" || a.Session.IsZero() {
		return AgentEvent{}, errors.New("agentprovider: 注意力事件缺少 ID 或 SessionKey")
	}
	if (a.Kind == AttentionPermission) != (a.Permission != nil) {
		return AgentEvent{}, errors.New("agentprovider: permission 注意力事件必须且仅携带 Permission 负载")
	}
	if (a.Kind == AttentionQuestion) != (a.Question != nil) {
		return AgentEvent{}, errors.New("agentprovider: question 注意力事件必须且仅携带 Question 负载")
	}
	return AgentEvent{Kind: kind, Key: a.Session, Attention: &a}, nil
}

// NewSessionErrorEvent 构造结构化会话错误事件。
func NewSessionErrorEvent(key SessionKey, d SessionErrorDetail) (AgentEvent, error) {
	if key.IsZero() {
		return AgentEvent{}, errors.New("agentprovider: 错误事件的 SessionKey 恒非零")
	}
	if d.Name == "" || d.Message == "" {
		return AgentEvent{}, errors.New("agentprovider: 会话错误事件缺少必填 name/message")
	}
	return AgentEvent{Kind: EventSessionError, Key: key, Error: &d}, nil
}

// payloadKinds 返回本事件非 nil 的负载 Kind 集合（不变量校验用）。
func (e AgentEvent) filledPayloads() []string {
	var out []string
	if e.Session != nil {
		out = append(out, "Session")
	}
	if e.Status != nil {
		out = append(out, "Status")
	}
	if e.Attention != nil {
		out = append(out, "Attention")
	}
	if e.Error != nil {
		out = append(out, "Error")
	}
	return out
}

// Validate 复核逐 Kind 单负载不变量与必填负载形状。
func (e AgentEvent) Validate() error {
	if e.Key.IsZero() {
		return errors.New("agentprovider: 事件 SessionKey 恒非零")
	}
	want := map[EventKind]string{
		EventSessionCreated:    "Session",
		EventSessionUpdated:    "Session",
		EventSessionDeleted:    "Session",
		EventStatusChanged:     "Status",
		EventAttentionRaised:   "Attention",
		EventAttentionResolved: "Attention",
		EventSessionError:      "Error",
	}
	expect, ok := want[e.Kind]
	if !ok {
		return fmt.Errorf("agentprovider: 未知事件 Kind %q", e.Kind)
	}
	filled := e.filledPayloads()
	if len(filled) != 1 || filled[0] != expect {
		return fmt.Errorf("agentprovider: Kind %s 必须且仅携带 %s 负载，实际 %v", e.Kind, expect, filled)
	}
	return nil
}

// --- 状态 ---

// AgentStatus 是 v1 session.status 的归一化状态枚举（稳定字符串值）。
// StatusUnknown 是上游未知/缺失枚举的唯一映射，不得猜测。
type AgentStatus string

const (
	StatusUnknown AgentStatus = "unknown"
	StatusIdle    AgentStatus = "idle"
	StatusBusy    AgentStatus = "busy"
	StatusRetry   AgentStatus = "retry"
)

// StatusDetail 承载 v1 session.status 全量信息（retry 明细）。NextAt 为 epoch
// **毫秒**（与上游一致，不得换算）。
type StatusDetail struct {
	State   AgentStatus
	Attempt int    // State=retry 时有效：当前重试序号
	Message string // State=retry 时有效：失败摘要
	NextAt  int64  // State=retry 时有效：下次尝试时刻（epoch 毫秒）
}

// SessionErrorDetail 承载结构化会话错误字段（v1 现状逐字迁移）；可空字段保留
// 可空语义（缺失/null 为 nil；类型非法仅降级该字段）。
type SessionErrorDetail struct {
	Name        string // 必填：TrimSpace 后非空
	Message     string // 必填：TrimSpace 后非空（详情保留原文）
	StatusCode  *int   // 可空
	IsRetryable *bool  // 可空
}

// --- 注意力 ---

// AttentionKind 注意力事件类型（稳定字符串值）。
type AttentionKind string

const (
	AttentionPermission AttentionKind = "permission"
	AttentionQuestion   AttentionKind = "question"
)

// PermissionPayload 字段与 v1 opencode.PermissionRequest 一致（attention.go 现状）。
type PermissionPayload struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Permission string          `json:"permission"`
	Patterns   []string        `json:"patterns"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// QuestionItem 单条提问的最小稳定字段。
//
// Deprecated: 方案 B（design D10）引入结构化字段模型后，Questions 仅为
// QuestionPayload 的兼容视图（v1 路径填充）；消费方 MUST 优先读 Fields，
// 经 EffectiveFields() 回退。迁移完成后可删除。
type QuestionItem struct {
	Header   string `json:"header"`
	Question string `json:"question"`
}

// QuestionFieldType 是结构化字段的类型值域（design D10）：
// text/textarea/boolean/number/select/multiselect。未知类型值 MUST 保留原值
// （消费方按可降级文本处理），MUST NOT 静默降级为 text。
type QuestionFieldType string

const (
	QuestionTypeText        QuestionFieldType = "text"
	QuestionTypeTextarea    QuestionFieldType = "textarea"
	QuestionTypeBoolean     QuestionFieldType = "boolean"
	QuestionTypeNumber      QuestionFieldType = "number"
	QuestionTypeSelect      QuestionFieldType = "select"
	QuestionTypeMultiselect QuestionFieldType = "multiselect"
)

// QuestionOption 是 select/multiselect 字段的单个选项。
type QuestionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// QuestionField 是结构化 question 的单个字段（design D10）：Key 为回复关联键
// （provider 原生 key 或合成稳定 key），Label 为展示标题，Prompt 为提问正文，
// Metadata 仅保留未入模型的字段（不得成为消费方主路径）。
type QuestionField struct {
	Key      string           `json:"key"`
	Label    string           `json:"label,omitempty"`
	Prompt   string           `json:"prompt,omitempty"`
	Type     string           `json:"type,omitempty"`
	Required bool             `json:"required,omitempty"`
	Options  []QuestionOption `json:"options,omitempty"`
	Metadata json.RawMessage  `json:"metadata,omitempty"`
}

// QuestionAnswer 是 question 回复的单字段答案（design D10）：Key 关联
// QuestionField.Key；Value 为任意 JSON 值（v2 Form.Value 值域：
// string/number/boolean/string[]），类型校验由 provider 上游契约承担。
type QuestionAnswer struct {
	Key   string
	Value any
}

// QuestionPayload 是 provider-neutral 的 pending question 快照。主视图 =
// Fields（结构化字段，key-based 回复关联）；Questions 为兼容视图（v1 路径
// 填充；v2 form 不再填充——design D10）。消费方 MUST 优先读 Fields，
// 缺失时经 EffectiveFields() 回退合成视图。
type QuestionPayload struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Title     string          `json:"title,omitempty"`
	Fields    []QuestionField `json:"fields,omitempty"`
	// Deprecated: 兼容视图（v1 QuestionItem 路径）；见 Fields。
	Questions []QuestionItem `json:"questions,omitempty"`
}

// EffectiveFields 返回 question 的主视图字段（design D10 纯函数，不涉及
// wire）：Fields 非空时原样返回；否则由 Questions 逐条合成——
// Key=question-N（1 起，合成稳定 key，非上游 ID）、Label=Header、
// Prompt=Question、Type=text。两视图皆空返回 nil。
func (p QuestionPayload) EffectiveFields() []QuestionField {
	if len(p.Fields) > 0 {
		return p.Fields
	}
	if len(p.Questions) == 0 {
		return nil
	}
	out := make([]QuestionField, 0, len(p.Questions))
	for i, q := range p.Questions {
		out = append(out, QuestionField{
			Key:    fmt.Sprintf("question-%d", i+1),
			Label:  q.Header,
			Prompt: q.Question,
			Type:   string(QuestionTypeText),
		})
	}
	return out
}

// Attention 是归一化注意力信号：Kind 决定唯一非 nil 负载。
type Attention struct {
	ID         string
	Kind       AttentionKind
	Session    SessionKey
	Permission *PermissionPayload // Kind=permission 时非 nil
	Question   *QuestionPayload   // Kind=question 时非 nil
}

// --- 会话 ---

// AgentSession 是 provider-neutral 会话值对象（供事件负载与 SessionControl 共用）。
type AgentSession struct {
	Key       SessionKey
	Title     string
	Parent    *SessionKey // 同命名空间；无父为 nil
	CreatedAt int64       // epoch 毫秒（与上游 time.created 一致）
	UpdatedAt int64       // epoch 毫秒
}
