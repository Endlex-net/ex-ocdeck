// occlient.go 承载 OCClient 传输契约的中性（provider-neutral）DTO 集合
// （opencode-v1-spi-provider C2a / tasks 3.6：task 层与 application 层不再直接
// 依赖具体 client 类型；opencode 包内保留同名别名作过渡兼容，
// 生产装配在 cmd 经 registry 工厂注入）。
//
// 内容自 opencode 包整体迁移（逐字搬运，含 JSON tag 与解析语义——行为保持）；
// PermissionRequest/QuestionRequest/QuestionItem 直接复用 domain payload 别名
// （字段与 tag 与 v1 契约一致，C1 已锁定）。所有错误 sentinel 同步迁移，opencode
// 侧保留指向本包的 var 别名（errors.Is 语义不变）。
package agentprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	domain "ocdeck/internal/domain/agentprovider"
)

// --- 契约区间常量（CONTRACT.md；opencode 侧保留 const 副本） ---

const ContractBaseline = "1.18.26"
const ContractMinVersion = "1.18.14"

// --- 传输 DTO（原 opencode 包逐字迁移） ---

// HealthResponse GET /global/health 响应。
type HealthResponse struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

// SessionTime session.time 子对象（顶层 time.updated）。
type SessionTime struct {
	Created float64 `json:"created,omitempty"`
	Updated float64 `json:"updated,omitempty"`
}

// Session 表示上游 session 对象：顶层 id、time.updated；ParentID 非空表示
// background subagent 子会话；额外字段以 Raw 保留原文。
type Session struct {
	ID       string                 `json:"id"`
	Time     SessionTime            `json:"time"`
	Title    string                 `json:"title,omitempty"`
	ParentID string                 `json:"parentID,omitempty"`
	Raw      map[string]interface{} `json:"-"`
}

// SessionStatusType /session/status 的三值枚举。
type SessionStatusType string

const (
	StatusIdle  SessionStatusType = "idle"
	StatusBusy  SessionStatusType = "busy"
	StatusRetry SessionStatusType = "retry"
)

// SessionStatus 单个 session 的状态对象，其余字段保留在 Raw。
type SessionStatus struct {
	Type SessionStatusType      `json:"type"`
	Raw  map[string]interface{} `json:"-"`
}

// Event SSE 事件 envelope：{type, properties}。
type Event struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
}

// infoMap 取出 properties.info 并断言为 object；非 object 返回 nil。
func (e Event) infoMap() map[string]interface{} {
	if e.Properties == nil {
		return nil
	}
	if info, ok := e.Properties["info"].(map[string]interface{}); ok {
		return info
	}
	return nil
}

// Info 返回 properties.info（session.created/updated/deleted 的负载对象）；
// 缺失或非 object 返回 nil。供 provider 适配层读取 title/time 等非固定字段。
func (e Event) Info() map[string]interface{} { return e.infoMap() }

// SessionID 从 Event 提取 sessionID（session.created/updated/deleted 均取
// properties.info.id）。
func (e Event) SessionID() string {
	info := e.infoMap()
	if info == nil {
		return ""
	}
	if id, ok := info["id"].(string); ok {
		return id
	}
	return ""
}

// SessionIDProp 提取 sessionID：优先 properties.sessionID（status/diff 事件真实
// 结构），回退 properties.info.id（created/updated/deleted 携带 info.id，冗余
// properties.sessionID 与之同值）。
func (e Event) SessionIDProp() string {
	if e.Properties != nil {
		if id, ok := e.Properties["sessionID"].(string); ok && id != "" {
			return id
		}
	}
	return e.SessionID()
}

// Directory 提取 properties.info.directory（created/updated/deleted 同源校验防线：
// directory MUST 明确等于本任务目录才落库）。无 directory 字段或非 string 返回
// ("", false)。
func (e Event) Directory() (string, bool) {
	info := e.infoMap()
	if info == nil {
		return "", false
	}
	d, ok := info["directory"].(string)
	if !ok || d == "" {
		return "", false
	}
	return d, true
}

// TimeUpdated 提取 info.time.updated（刷新 last_seen_at）。缺失或非数字返回
// (0, false)。
func (e Event) TimeUpdated() (float64, bool) {
	info := e.infoMap()
	if info == nil {
		return 0, false
	}
	t, ok := info["time"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	return FloatNumber(t["updated"])
}

// ParentID 提取 properties.info.parentID（子 session 非空；空为顶层会话）。
func (e Event) ParentID() string {
	info := e.infoMap()
	if info == nil {
		return ""
	}
	if id, ok := info["parentID"].(string); ok {
		return id
	}
	return ""
}

// FloatNumber 判断 v 是否为 JSON number（float64/json.Number 等），返回数值与
// 是否为数字（opencode client 内部消费同款语义，导出供其复用）。
func FloatNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// --- 错误集合（occlient 边界 err-first；sentinel 由 opencode 侧 var 别名承接） ---

var (
	// ErrServeNotReady health 超时或非 2xx：serve 未就绪。
	ErrServeNotReady = errors.New("opencode: serve not ready")
	// ErrUnauthorized 401：内部 bug（Basic Auth 凭据错误）。
	ErrUnauthorized = errors.New("opencode: unauthorized (internal bug)")
	// ErrSessionNotFound 404：会话不存在（attach 前预检、孤儿清理）。
	ErrSessionNotFound = errors.New("opencode: session not found")
	// ErrCapabilityMismatch 能力探测不兼容（结构漂移，激活门禁拒绝）。
	ErrCapabilityMismatch = errors.New("opencode: capability mismatch (incompatible with contract baseline)")
	// ErrSessionOverflow GET /session 返回数==limit（溢出信号，非错误中止）。
	ErrSessionOverflow = errors.New("opencode: session list overflow (returned count equals limit)")
	// ErrCapabilityUnsupported serve 不支持该能力端点（404，能力状态机迁移 unsupported）。
	ErrCapabilityUnsupported = errors.New("opencode: capability unsupported (endpoint not available)")
	// ErrSessionTitleUnsupported serve 不提供会话标题更新端点（PATCH 404 能力降级）。
	ErrSessionTitleUnsupported = errors.New("opencode: session title update unsupported")
	// ErrPermissionRequestGone 权限请求已了结竞态（_tag=PermissionNotFoundError，正常忽略）。
	ErrPermissionRequestGone = errors.New("opencode: permission request not found")
)

// --- attention 家族（原 opencode/attention.go 逐字迁移） ---

// PermissionRequest 是 GET /permission 返回的单条 pending 权限请求（v1 契约）。
// 与 domain.PermissionPayload 字段/tag 完全一致，直接别名（dto 嵌入字段名与
// 序列化形状保持不变）。
type PermissionRequest = domain.PermissionPayload

// QuestionItem 单条提问的最小稳定字段。
type QuestionItem = domain.QuestionItem

// QuestionRequest GET /question 返回的单条 pending 问题请求。
type QuestionRequest = domain.QuestionPayload

// NormalizePermissionMetadata 归一化请求 metadata：有效形状仅为 JSON object——
// 缺失/null/非法 JSON/合法非 object 一律归 nil（MUST NOT 丢弃请求）。
func NormalizePermissionMetadata(raw json.RawMessage) json.RawMessage {
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

// AttentionKind 注意力事件语义：asked（新增 pending）/ replied|rejected（了结）。
type AttentionKind string

const (
	AttentionAsked    AttentionKind = "asked"
	AttentionReplied  AttentionKind = "replied"
	AttentionRejected AttentionKind = "rejected"
)

// AttentionType 注意力事件类型：permission 或 question。
type AttentionType string

const (
	AttentionPermission AttentionType = "permission"
	AttentionQuestion   AttentionType = "question"
)

// AttentionEvent 是从 SSE Event 解析出的规范化注意力事件。Title/Fields 为
// 方案 B（design D10）结构化 question 视图：provider 侧携带时填充（v2 form
// 事件路径；v1 wire 无字段级信息，事件路径留空——消费方经 EffectiveFields
// 回退）。
type AttentionEvent struct {
	Kind       AttentionKind
	Type       AttentionType
	RequestID  string
	SessionID  string
	Permission string                 // permission.asked only
	Patterns   []string               // permission.asked only
	Title      string                 // question asks only（结构化，可空）
	Fields     []domain.QuestionField // question asks only（结构化主视图）
	Questions  []QuestionItem         // question asks only（兼容视图）
	Metadata   json.RawMessage
}

// ParseAttentionEvent 将 SSE Event 按其 type 分派解析为 AttentionEvent。
// 支持 v1 与 v2 同形家族。字段级校验：id+sessionID 必填；permission asks 还需
// permission；question asks 还需非空 Questions 且每条 header+question 非空；
// patterns 必须为 string 数组；replied/rejected 需 sessionID+requestID。
// 未知 type 或缺字段 → (zero, false)，调用方静默忽略（不中断流）。
func ParseAttentionEvent(ev Event) (AttentionEvent, bool) {
	props := ev.Properties
	if props == nil {
		return AttentionEvent{}, false
	}
	switch ev.Type {
	case "permission.asked", "permission.v2.asked":
		return parsePermissionAsked(props)
	case "permission.replied", "permission.v2.replied":
		return parsePermissionReplied(props)
	case "question.asked", "question.v2.asked":
		return parseQuestionAsked(props)
	case "question.replied", "question.v2.replied":
		return parseQuestionReplied(props)
	case "question.rejected", "question.v2.rejected":
		return parseQuestionRejected(props)
	default:
		return AttentionEvent{}, false
	}
}

func parsePermissionAsked(props map[string]interface{}) (AttentionEvent, bool) {
	id, ok := stringField(props, "id")
	if !ok {
		return AttentionEvent{}, false
	}
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	perm, ok := stringField(props, "permission")
	if !ok {
		return AttentionEvent{}, false
	}
	// patterns 缺省（缺失或 null）合法 → nil；存在但非字符串数组 → 非法。
	patterns, ok := optionalStringSliceField(props, "patterns")
	if !ok {
		return AttentionEvent{}, false
	}
	// metadata：缺省合法 → nil；存在则序列化后归一化（非 object 一律归 nil，不丢请求）。
	var metadata json.RawMessage
	if mv, present := props["metadata"]; present && mv != nil {
		if b, merr := json.Marshal(mv); merr == nil {
			metadata = NormalizePermissionMetadata(b)
		}
	}
	return AttentionEvent{
		Kind:       AttentionAsked,
		Type:       AttentionPermission,
		RequestID:  id,
		SessionID:  sid,
		Permission: perm,
		Patterns:   patterns,
		Metadata:   metadata,
	}, true
}

func parsePermissionReplied(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionReplied,
		Type:      AttentionPermission,
		RequestID: rid,
		SessionID: sid,
	}, true
}

func parseQuestionAsked(props map[string]interface{}) (AttentionEvent, bool) {
	id, ok := stringField(props, "id")
	if !ok {
		return AttentionEvent{}, false
	}
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	questions, ok := parseQuestionItems(props["questions"])
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionAsked,
		Type:      AttentionQuestion,
		RequestID: id,
		SessionID: sid,
		Questions: questions,
	}, true
}

func parseQuestionReplied(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionReplied,
		Type:      AttentionQuestion,
		RequestID: rid,
		SessionID: sid,
	}, true
}

func parseQuestionRejected(props map[string]interface{}) (AttentionEvent, bool) {
	sid, ok := stringField(props, "sessionID")
	if !ok {
		return AttentionEvent{}, false
	}
	rid, ok := stringField(props, "requestID")
	if !ok {
		return AttentionEvent{}, false
	}
	return AttentionEvent{
		Kind:      AttentionRejected,
		Type:      AttentionQuestion,
		RequestID: rid,
		SessionID: sid,
	}, true
}

// parseQuestionItems 解析 question.asked 的 questions 数组，逐条校验
// header+question 非空。非数组、空数组、任一条目缺字段 → false。
func parseQuestionItems(v interface{}) ([]QuestionItem, bool) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, false
	}
	out := make([]QuestionItem, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, false
		}
		header, ok := stringField(m, "header")
		if !ok {
			return nil, false
		}
		question, ok := stringField(m, "question")
		if !ok {
			return nil, false
		}
		out = append(out, QuestionItem{Header: header, Question: question})
	}
	return out, true
}

// stringField 取 props[key] 并断言为非空 string。
func stringField(props map[string]interface{}, key string) (string, bool) {
	v, ok := props[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// optionalStringSliceField 取 props[key]：缺省（缺失或 null）合法返回 (nil, true)；
// 存在但非数组或含非 string 元素 → (nil, false)。空数组 [] 合法。
func optionalStringSliceField(props map[string]interface{}, key string) ([]string, bool) {
	v, ok := props[key]
	if !ok || v == nil {
		return nil, true // 缺省/null 合法
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false // 存在但非数组 → 非法
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// --- session.status 事件（原 opencode/session_status.go 逐字迁移） ---

// SessionStatusEvent 从 SSE session.status 事件解析出的结构化状态：sessionID 在
// properties.sessionID，状态在 properties.status.type ∈ {idle,busy,retry}；retry
// 额外携带 properties.status.{attempt, message, next(epoch ms)}。
type SessionStatusEvent struct {
	SessionID string
	Status    SessionStatusType
	Attempt   int    // retry：当前重试序号
	Message   string // retry：失败摘要
	Next      int64  // retry：下次尝试时刻（epoch ms）
}

// ParseSessionStatusEvent 识别并解析 session.status 事件。sessionID 提取沿用
// SessionIDProp 语义（优先 properties.sessionID，回退 properties.info.id）。
// 非该 type、缺 sessionID、缺/非法 status.type → (zero, false)。
func ParseSessionStatusEvent(ev Event) (SessionStatusEvent, bool) {
	if ev.Type != "session.status" {
		return SessionStatusEvent{}, false
	}
	sid := ev.SessionIDProp()
	if sid == "" {
		return SessionStatusEvent{}, false
	}
	st, ok := ev.Properties["status"].(map[string]interface{})
	if !ok {
		return SessionStatusEvent{}, false
	}
	typ, ok := stringField(st, "type")
	if !ok {
		return SessionStatusEvent{}, false
	}
	var status SessionStatusType
	switch SessionStatusType(typ) {
	case StatusIdle, StatusBusy, StatusRetry:
		status = SessionStatusType(typ)
	default:
		// 枚举外取值视为契约漂移，fail-closed 忽略。
		return SessionStatusEvent{}, false
	}
	out := SessionStatusEvent{SessionID: sid, Status: status}
	if status == StatusRetry {
		if a, ok := FloatNumber(st["attempt"]); ok {
			out.Attempt = int(a)
		}
		if m, ok := st["message"].(string); ok {
			out.Message = m
		}
		if n, ok := FloatNumber(st["next"]); ok {
			out.Next = int64(n)
		}
	}
	return out, true
}

// --- session.error 事件（原 opencode/session_error.go 逐字迁移） ---

// SessionErrorEvent 从 SSE session.error 事件解析出的结构化错误观察：
// sessionID 在 properties.sessionID，错误体在 properties.error.{name,
// data.{message, statusCode, isRetryable}}。
type SessionErrorEvent struct {
	SessionID   string
	Name        string
	Message     string
	StatusCode  *int  // 可空：仅可无损表示为 Go int 的 JSON 整数
	IsRetryable *bool // 可空：仅 JSON 布尔
}

// ParseSessionErrorEvent 识别并解析 session.error 事件。必填 sessionID/
// error.name/error.data.message 三者 MUST 为 TrimSpace 后非空字符串（详情保留
// 原文）；缺失、空白或类型非法 → (zero, false) 忽略整个事件（fail-closed）。
// sessionID 直读 properties["sessionID"]，MUST NOT 走 info.id 回退。
func ParseSessionErrorEvent(ev Event) (SessionErrorEvent, bool) {
	if ev.Type != "session.error" {
		return SessionErrorEvent{}, false
	}
	sid, ok := ev.Properties["sessionID"].(string)
	if !ok || strings.TrimSpace(sid) == "" {
		return SessionErrorEvent{}, false
	}
	errObj, ok := ev.Properties["error"].(map[string]interface{})
	if !ok {
		return SessionErrorEvent{}, false
	}
	name, ok := errObj["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return SessionErrorEvent{}, false
	}
	data, ok := errObj["data"].(map[string]interface{})
	if !ok {
		return SessionErrorEvent{}, false
	}
	msg, ok := data["message"].(string)
	if !ok || strings.TrimSpace(msg) == "" {
		return SessionErrorEvent{}, false
	}
	out := SessionErrorEvent{SessionID: sid, Name: name, Message: msg}
	if v := data["statusCode"]; v != nil {
		if code, ok := losslessJSONInt(v); ok {
			out.StatusCode = &code
		}
	}
	if v := data["isRetryable"]; v != nil {
		if b, ok := v.(bool); ok {
			out.IsRetryable = &b
		}
	}
	return out, true
}

// losslessJSONInt 判断 v 是否可无损表示为 Go int 的 JSON 整数：仅接受生产
// SSE 解码路径（UseNumber）保留的 json.Number 词法值，经 ParseInt 精确判定。
func losslessJSONInt(v interface{}) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, strconv.IntSize)
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// --- prompt_async 传输 DTO（原 opencode/prompt_async.go 逐字迁移） ---
// 命名区分：D7 的 SPI PromptResult（Outcome 绑定）见 agentprovider.go；此处为
// 传输层四分类 DTO，前缀 PromptTransport。

// PromptTransportResultKind 四类发送结果（至多一次发送决策依赖该分类）。
type PromptTransportResultKind string

const (
	// ResultAccepted 服务端返回 204（请求已被接受、异步执行）。
	ResultAccepted PromptTransportResultKind = "accepted"
	// ResultHTTPResponse 收到任何状态码 != 204 的响应。StatusCode/Body 携带实际值。
	ResultHTTPResponse PromptTransportResultKind = "http_response"
	// ResultTransportUnknown httpClient.Do 返回错误，结果未知；MUST NOT 推断是否已发出。
	ResultTransportUnknown PromptTransportResultKind = "transport_unknown"
	// ResultPreSendFailure httpClient.Do 之前的本地失败。
	ResultPreSendFailure PromptTransportResultKind = "pre_send_failure"
)

// PromptTransportResult 是 PromptAsync 的传输 DTO。字段填充规则与 Kind 绑定（唯一）。
type PromptTransportResult struct {
	Kind       PromptTransportResultKind
	StatusCode int    // accepted=204；http_response=实际状态码；其余=0
	Body       string // 仅 http_response 非空（有界截断）；其余为空
	Detail     string // 仅 transport_unknown/pre_send_failure 非空
}

// PromptFilePart 投递附件（file:// URI）。
type PromptFilePart struct {
	URL      string
	Mime     string
	Filename string
}

// --- 能力探测三值 ---

// ProbeCapabilityState 能力探测三值结果（传输层；命名区分 D7 的四值
// CapabilityHealth 状态 CapabilityState，见 agentprovider.go）。
type ProbeCapabilityState string

const (
	// CapabilitySupported 200 且契约形状匹配。
	CapabilitySupported ProbeCapabilityState = "supported"
	// CapabilityUnsupported 200 但路径缺失或 operationId 不符（fail-closed）。
	CapabilityUnsupported ProbeCapabilityState = "unsupported"
	// CapabilityUnknown 401/404/5xx/网络错误/畸形 JSON。
	CapabilityUnknown ProbeCapabilityState = "unknown"
)

// --- 消息列表（原 opencode/messages.go 逐字迁移） ---

// Message /session/:id/message 列表元素：id/role/parts。parts 内非文本 part
// 保留 Type、无 text 字段；形状漂移由解码错误承担（fail-closed）。
type Message struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
}

// messageInfo live 形状的 info 嵌套块，仅取 id/role 兜底用。
type messageInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

// UnmarshalJSON 兼容 flat {id,role,parts} 与 live {info,parts} 两种形状。
func (m *Message) UnmarshalJSON(data []byte) error {
	type flat struct {
		ID    string        `json:"id"`
		Role  string        `json:"role"`
		Parts []MessagePart `json:"parts"`
		Info  *messageInfo  `json:"info"`
	}
	var f flat
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	m.ID = f.ID
	m.Role = f.Role
	m.Parts = f.Parts
	if f.Info != nil {
		if m.ID == "" {
			m.ID = f.Info.ID
		}
		if m.Role == "" {
			m.Role = f.Info.Role
		}
	}
	return nil
}

// MessagePart 消息 part：type=text 时携带 text，其余类型仅保留 type。
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// --- Options（OCClient 工厂的中性参数；OnMalformed 为 opencode 内部回调，
// 不在中性面暴露——cmd 工厂转换时置 nil） ---

// Options 构造 Client 的可调参数（零值可用，默认见 opencode.NewClient）。
type Options struct {
	HealthTimeout     time.Duration
	OpTimeout         time.Duration
	ReconnectBase     time.Duration
	ReconnectMax      time.Duration
	ReconnectMaxTries int
	HeartbeatTimeout  time.Duration
	OnReady           func()
	OnDisconnect      func()
}
