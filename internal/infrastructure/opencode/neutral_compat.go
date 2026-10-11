// neutral_compat.go 提供 OCClient 传输契约的过渡兼容别名（opencode-v1-spi-provider
// C2a / tasks 3.6）：类型的唯一实现已迁移至 application/agentprovider/occlient.go
// （provider-neutral，task 层经中性类型消费）；本文件保留 opencode 包内的同名
// 引用（type alias / const 副本 / sentinel var 别名 / 解析器委托），使既有实现与
// 测试零改动编译。行为等价：alias 即同一类型，errors.Is 语义不变。
package opencode

import (
	"encoding/json"

	appagent "ocdeck/internal/application/agentprovider"
)

// --- 类型别名（与中性类型为同一类型） ---

type (
	HealthResponse     = appagent.HealthResponse
	Session            = appagent.Session
	SessionTime        = appagent.SessionTime
	SessionStatus      = appagent.SessionStatus
	SessionStatusType  = appagent.SessionStatusType
	Event              = appagent.Event
	AttentionEvent     = appagent.AttentionEvent
	AttentionKind      = appagent.AttentionKind
	AttentionType      = appagent.AttentionType
	SessionStatusEvent = appagent.SessionStatusEvent
	SessionErrorEvent  = appagent.SessionErrorEvent
	PromptResult       = appagent.PromptTransportResult
	PromptResultKind   = appagent.PromptTransportResultKind
	PromptFilePart     = appagent.PromptFilePart
	CapabilityState    = appagent.ProbeCapabilityState
	PermissionRequest  = appagent.PermissionRequest
	QuestionRequest    = appagent.QuestionRequest
	QuestionItem       = appagent.QuestionItem
	Message            = appagent.Message
	MessagePart        = appagent.MessagePart
)

// --- 常量副本 ---

const (
	StatusIdle             = appagent.StatusIdle
	StatusBusy             = appagent.StatusBusy
	StatusRetry            = appagent.StatusRetry
	ContractBaseline       = appagent.ContractBaseline
	ContractMinVersion     = appagent.ContractMinVersion
	AttentionAsked         = appagent.AttentionAsked
	AttentionReplied       = appagent.AttentionReplied
	AttentionRejected      = appagent.AttentionRejected
	AttentionPermission    = appagent.AttentionPermission
	AttentionQuestion      = appagent.AttentionQuestion
	ResultAccepted         = appagent.ResultAccepted
	ResultHTTPResponse     = appagent.ResultHTTPResponse
	ResultTransportUnknown = appagent.ResultTransportUnknown
	ResultPreSendFailure   = appagent.ResultPreSendFailure
	CapabilitySupported    = appagent.CapabilitySupported
	CapabilityUnsupported  = appagent.CapabilityUnsupported
	CapabilityUnknown      = appagent.CapabilityUnknown
)

// --- 错误 sentinel 别名（同一实例，errors.Is 语义不变） ---

var (
	ErrServeNotReady           = appagent.ErrServeNotReady
	ErrUnauthorized            = appagent.ErrUnauthorized
	ErrSessionNotFound         = appagent.ErrSessionNotFound
	ErrCapabilityMismatch      = appagent.ErrCapabilityMismatch
	ErrSessionOverflow         = appagent.ErrSessionOverflow
	ErrCapabilityUnsupported   = appagent.ErrCapabilityUnsupported
	ErrSessionTitleUnsupported = appagent.ErrSessionTitleUnsupported
	ErrPermissionRequestGone   = appagent.ErrPermissionRequestGone
)

// --- 解析器/助手委托 ---

var (
	ParseAttentionEvent     = appagent.ParseAttentionEvent
	ParseSessionStatusEvent = appagent.ParseSessionStatusEvent
	ParseSessionErrorEvent  = appagent.ParseSessionErrorEvent
)

// normalizePermissionMetadata 委托中性实现（opencode 内部/测试兼容）。
func normalizePermissionMetadata(raw json.RawMessage) json.RawMessage {
	return appagent.NormalizePermissionMetadata(raw)
}

// floatNumber 委托中性实现（client.go 内部消费）。
func floatNumber(v interface{}) (float64, bool) {
	return appagent.FloatNumber(v)
}

// OptionsFromNeutral 把中性 Options 转换为本包完整 Options（OnMalformed 为
// opencode 内部回调，中性面不暴露，转换置 nil）。
func OptionsFromNeutral(o appagent.Options) Options {
	return Options{
		HealthTimeout:     o.HealthTimeout,
		OpTimeout:         o.OpTimeout,
		ReconnectBase:     o.ReconnectBase,
		ReconnectMax:      o.ReconnectMax,
		ReconnectMaxTries: o.ReconnectMaxTries,
		HeartbeatTimeout:  o.HeartbeatTimeout,
		OnReady:           o.OnReady,
		OnDisconnect:      o.OnDisconnect,
	}
}

// NewWireClient 是 OCClient 传输实现的 provider 内部构造出口（C2 残余修复）：
// 「从已解析的端点三元组构造 v1 传输 client」的唯一调用点收敛进 provider 包——
// 装配方（cmd/ocdeck-server）的 registry 分派工厂不再直构传输实现（ora-4
// 裁决 ②：生产 OCClient 固定查 v1+NewClient 不可接受——经 registry.Get 分派
// 后的传输构造属于 provider 自有面，与「绕过分派固定 v1」语义区分）。行为与
// NewClient 同一实现（中性 Options 转换在此收敛）。
func NewWireClient(port int, password string, opts appagent.Options) *Client {
	return NewClient(port, password, OptionsFromNeutral(opts))
}
