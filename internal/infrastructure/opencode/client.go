// Package opencode 实现 occlient：对 opencode serve REST/SSE 端点的集中封装
// 与版本/能力探测（design.md §11/§18/§20）。
//
// 漂移防护策略（§11）：版本号仅告警，激活门禁是能力探测（health + /session/status
// 结构 + session 列表字段形状；DELETE 形状不做 live 探测，首次真实删除时校验）。
// 全部端点契约以 OpenCode 1.18.18 源码核验为基线（contract fixture 固化，见 §20），
// 漂移只改本包一处。
//
// 仅使用标准库（net/http + bufio 手写 SSE 解析）：go.mod 由独立 lane 维护。
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ContractBaseline 已验证契约区间上限；仅作告警，非门禁；区间检查见 internal/infrastructure/opencode/CONTRACT.md。
const ContractBaseline = "1.18.26"

// ContractMinVersion 已验证契约区间下限；仅作告警，非门禁。
const ContractMinVersion = "1.18.14"

// Session 表示 opencode 的 session 对象。顶层字段 id、time.updated（§20，
// session.ts:191-209，非嵌套 info.id）。额外字段以 Raw 保留原文供上层按需取用。
// ParentID 非空表示 background subagent 子会话（1.18.18 契约：Session 有 parentID 字段，
// 子 session 非空，与主会话同 directory）；空为顶层会话（用户主会话）。
type Session struct {
	ID       string                 `json:"id"`
	Time     SessionTime            `json:"time"`
	Title    string                 `json:"title,omitempty"`
	ParentID string                 `json:"parentID,omitempty"`
	Raw      map[string]interface{} `json:"-"`
}

// SessionTime session.time 子对象（§20：顶层 time.updated）。
type SessionTime struct {
	Created float64 `json:"created,omitempty"`
	Updated float64 `json:"updated,omitempty"`
}

// SessionStatusType /session/status 的三值枚举（§20，session-status-event.ts）。
type SessionStatusType string

const (
	StatusIdle  SessionStatusType = "idle"
	StatusBusy  SessionStatusType = "busy"
	StatusRetry SessionStatusType = "retry"
)

// SessionStatus 单个 session 的状态对象，其余字段保留在 Raw。
type SessionStatus struct {
	Type SessionStatusType     `json:"type"`
	Raw  map[string]interface{} `json:"-"`
}

// HealthResponse GET /global/health 响应（§20）。
type HealthResponse struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

// Event SSE 事件 envelope（§20）：{type, properties}。
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

// SessionID 从 Event 提取 sessionID（§20：session.created/updated/deleted 均取
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

// SessionIDProp 从 Event 提取 sessionID，优先 properties.sessionID（status/diff 事件真实结构，
// VERIFICATION.md 实测：无 properties.info，仅有 properties.sessionID），回退 properties.info.id
//（created/updated/deleted 携带 info.id，冗余 properties.sessionID 与之同值）。
// 用于 status/diff 事件按 sessionID 反查 task_sessions 归属（design.md §4 补注）。
func (e Event) SessionIDProp() string {
	if e.Properties != nil {
		if id, ok := e.Properties["sessionID"].(string); ok && id != "" {
			return id
		}
	}
	return e.SessionID()
}

// Directory 从 Event 提取 properties.info.directory（design.md §4 补注：SSE 防线，
// created/updated/deleted 事件校验 directory == 本任务 worktree，不匹配丢弃并告警）。
// 无 directory 字段返回 ("", false)；存在但非 string 返回 ("", false)。
// status/diff 事件无 directory（由 sessionID 反查 task_sessions 归属，不依赖此方法）。
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

// TimeUpdated 从 Event 提取 info.time.updated（§4：以事件 info.time.updated 刷新
// last_seen_at）。结构化访问封装在 occlient 内，调用方不得再解析 properties.info.time.updated。
// 缺失或非数字返回 (0, false)。
func (e Event) TimeUpdated() (float64, bool) {
	info := e.infoMap()
	if info == nil {
		return 0, false
	}
	t, ok := info["time"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	return floatNumber(t["updated"])
}

// ParentID 从 Event 提取 properties.info.parentID（1.18.18 契约：Session 有 parentID 字段，
// 子 session 非空）。用于 SSE 捕获与全量对齐时持久化 parent_id，锚定候选据此仅取顶层会话。
// 无 parentID 字段或非 string 返回 ""（顶层会话）。
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

// floatNumber 判断 v 是否为 JSON number（float64 / json.Number），返回数值与是否为数字。
func floatNumber(v interface{}) (float64, bool) {
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

// 错误集合。occlient 内部流转 err-first；上层在边界映射成 code/msg。
var (
	// ErrServeNotReady health 超时或非 2xx：serve 未就绪（§20）。
	ErrServeNotReady = errors.New("opencode: serve not ready")
	// ErrUnauthorized 401：内部 bug（Basic Auth 凭据错误，§20）。
	ErrUnauthorized = errors.New("opencode: unauthorized (internal bug)")
	// ErrSessionNotFound 404：会话不存在，必须与其他错误区分（attach 前预检、孤儿清理）。
	ErrSessionNotFound = errors.New("opencode: session not found")
	// ErrCapabilityMismatch 能力探测不兼容（health 不可达 / /session/status 结构不符 /
	// session 列表字段形状不符），激活门禁拒绝（§11）。
	ErrCapabilityMismatch = errors.New("opencode: capability mismatch (incompatible with contract baseline)")
	// ErrSessionOverflow GET /session 返回数==limit，视为溢出（§20）。
	ErrSessionOverflow = errors.New("opencode: session list overflow (returned count equals limit)")
)

// httpStatusError 携带 HTTP 状态码与原始 body，便于分类（404 vs 401 vs 5xx）。
type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("opencode: http %d: %s", e.status, e.body)
}

// Client 封装对单个 opencode serve 的 REST/SSE 访问。
// 凭据：用户名固定 "opencode"（§20），password 每 serve 随机。
type Client struct {
	baseURL  string
	username string
	password string

	httpClient *http.Client
	sseClient  *http.Client // SSE 专用：无读超时，靠 heartbeat/重连

	// SSE 重连退避参数（§20：断流指数退避重连，有上限）。
	reconnectBase    time.Duration
	reconnectMax     time.Duration
	reconnectMaxTries int // 最大重试次数；0 = 无限

	// healthTimeout health 短超时（就绪轮询用，§20）。
	healthTimeout time.Duration

	// heartbeatTimeout SSE 流空闲超时：超过此时长未见任何事件/注释即视为断流（§20）。
	// 0 表示禁用（仅靠 ctx 取消或底层连接错误收尾）。
	heartbeatTimeout time.Duration

	// onReady 首次 SSE 连接建立后的就绪信号（供 Activate 等待 SSE 建立后再对齐+启动 TUI）。
	// 在收到首事件（server.connected 或任意事件）后、任何业务事件派发前同步触发一次。
	onReady func()

	// onMalformed 解析失败的事件回调（§20：malformed event 计数/记录而非静默丢弃）。
	// 为 nil 时仅内部计数；非 nil 时同步调用，调用方据此落日志/告警。
	onMalformed func(raw sseRawEvent, err error)

	// onDisconnect 断流回调（P1.8.4，design D4 断流感知）：已建立的 SSE 连接终止
	//（established 且非 ctx 主动取消）后、进入重连退避前同步调用一次。
	// 从未建立的连接与主动 ctx 取消 MUST NOT 触发（区分网络断流与正常关停）。
	onDisconnect func()

	// malformedCount 累计 malformed 事件数（供诊断）。
	malformedCount int64
}

// Options 构造 Client 的可调参数（零值可用，默认见 NewClient）。
type Options struct {
	// HealthTimeout health 短超时（就绪轮询用，§20）。
	HealthTimeout time.Duration
	// OpTimeout session 操作常规超时。
	OpTimeout time.Duration
	// ReconnectBase SSE 断流重连初始退避。
	ReconnectBase time.Duration
	// ReconnectMax SSE 断流重连退避上限。
	ReconnectMax time.Duration
	// ReconnectMaxTries SSE 最大重连尝试次数；0 表示无限重试（靠 ctx 取消终止）。
	ReconnectMaxTries int
	// HeartbeatTimeout SSE 流空闲超时：超过此时长未见任何事件/注释即视为断流重连。
	// 0 表示禁用。
	HeartbeatTimeout time.Duration
	// OnReady 首次 SSE 连接建立后的就绪信号（见 Client.onReady）。可选。
	OnReady func()
	// OnMalformed 解析失败的事件回调（见 Client.onMalformed）。可选。
	OnMalformed func(raw sseRawEvent, err error)
	// OnDisconnect 断流回调（见 Client.onDisconnect）：已建立连接终止、退避前同步一次。
	// 主动 ctx 取消与从未建立的连接不触发。可选。
	OnDisconnect func()
}

const (
	defaultHealthTimeout     = 2 * time.Second
	defaultOpTimeout         = 10 * time.Second
	defaultReconnectBase     = 500 * time.Millisecond
	defaultReconnectMax      = 30 * time.Second
	defaultReconnectMaxTries = 0
	// defaultHeartbeatTimeout SSE 默认空闲超时（§20：靠 heartbeat 判断流）。
	// opencode serve 心跳间隔较短，给充裕上限。
	defaultHeartbeatTimeout = 60 * time.Second
)

// loopbackTransport 是进程内所有 opencode serve Client 共享的 Transport。
// 从 DefaultTransport clone 继承 MaxIdleConns / IdleConnTimeout / DialContext 等有界
// 回收配置，再显式 Proxy=nil：occlient 只访问 loopback，禁止任何代理劫持。
// （Go ≥1.16 的 ProxyFromEnvironment 已豁免 loopback；Proxy: nil 仍作为显式不变量，
// 防止版本/环境语义漂移。）
var loopbackTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}()

// NewClient 构造 Client。port 为 serve 端口，password 为每 serve 随机密码。
func NewClient(port int, password string, opts Options) *Client {
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = defaultHealthTimeout
	}
	if opts.OpTimeout <= 0 {
		opts.OpTimeout = defaultOpTimeout
	}
	if opts.ReconnectBase <= 0 {
		opts.ReconnectBase = defaultReconnectBase
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = defaultReconnectMax
	}
	if opts.HeartbeatTimeout < 0 {
		opts.HeartbeatTimeout = 0
	}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	// httpClient / sseClient 各有独立 http.Client（超时语义不同），共享包级
	// loopbackTransport 以跨实例复用 loopback 连接池，消除短连接 churn。
	return &Client{
		baseURL:           baseURL,
		username:          "opencode",
		password:          password,
		httpClient:        &http.Client{Timeout: opts.OpTimeout, Transport: loopbackTransport},
		sseClient:         &http.Client{Timeout: 0, Transport: loopbackTransport}, // SSE 无读超时，靠 heartbeat 与重连（§20）
		reconnectBase:     opts.ReconnectBase,
		reconnectMax:      opts.ReconnectMax,
		reconnectMaxTries: opts.ReconnectMaxTries,
		healthTimeout:     opts.HealthTimeout,
		heartbeatTimeout:  opts.HeartbeatTimeout,
		onReady:           opts.OnReady,
		onMalformed:       opts.OnMalformed,
		onDisconnect:      opts.OnDisconnect,
	}
}

// MalformedCount 返回累计 malformed 事件数（诊断用）。
func (c *Client) MalformedCount() int64 { return c.malformedCount }

// Probe 能力探测（design.md §11/§18）：health 可达 + /session/status 响应结构校验 +
// session 列表字段形状校验。返回 version（来自 health）与 err；不兼容返回 ErrCapabilityMismatch。
// DELETE 形状不做 live 探测（§11），首次真实删除时校验。
//
// 错误分类（供 P3 决定重试或失败）：
//   - ErrUnauthorized（401）：内部 bug，MUST NOT 重试，快速失败上报。
//   - ErrServeNotReady（health 超时/非 2xx 或 5xx/网络）：serve 未就绪，可重试。
//   - ErrCapabilityMismatch（health.healthy=false、/session/status 结构不符、
//     session 列表字段形状不符）：结构漂移，阻止激活，不可重试。
//
// /session/status 结构校验（含 type 三值枚举）在 SessionStatus 内完成；session 列表
// 字段形状（顶层 id + time.updated）在 parseSession 内完成。任一解析失败映射为
// ErrCapabilityMismatch。session 列表为空时无字段可校验，视为通过（无内容即无漂移）。
func (c *Client) Probe(ctx context.Context) (version string, err error) {
	h, err := c.Health(ctx)
	if err != nil {
		return "", classifyProbeErr(err)
	}
	if !h.Healthy {
		return "", ErrCapabilityMismatch
	}
	// /session/status 结构校验（§11）。
	if _, err := c.SessionStatus(ctx, ""); err != nil {
		return "", classifyProbeErr(err)
	}
	// session 列表字段形状校验（§11/§20）：limit=1 仅取代表样本验证字段形状，
	// 避免拉全量；空列表无可校验字段，直接通过。
	if _, err := c.ListSessions(ctx, "", 1); err != nil {
		// ErrSessionOverflow 表示存在 ≥1 条且字段校验通过（parseSession 已逐条校验）。
		if errors.Is(err, ErrSessionOverflow) {
			return h.Version, nil
		}
		return "", classifyProbeErr(err)
	}
	return h.Version, nil
}

// classifyProbeErr 把各探测步的错误映射到 Probe 的三类语义（§11）：
//   - 401 → ErrUnauthorized（内部 bug，不可重试）
//   - 5xx/网络/超时 → ErrServeNotReady（可重试）
//   - 结构/枚举不符、health.healthy=false → ErrCapabilityMismatch（阻止激活）
//   - 4xx（非 401）→ ErrServeNotReady（能力探测目标不可达，归类为未就绪而非结构漂移）
func classifyProbeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnauthorized) {
		return ErrUnauthorized
	}
	if errors.Is(err, ErrCapabilityMismatch) {
		return ErrCapabilityMismatch
	}
	var hse *httpStatusError
	if errors.As(err, &hse) {
		switch {
		case hse.status == http.StatusUnauthorized:
			return ErrUnauthorized
		case hse.status == http.StatusNotFound:
			// 探测端点不存在：结构漂移（端点缺失）。
			return ErrCapabilityMismatch
		case hse.status/100 == 5:
			return ErrServeNotReady
		default:
			return ErrServeNotReady
		}
	}
	// 网络错误/超时（非 httpStatusError）：serve 未就绪，可重试。
	return ErrServeNotReady
}

// Health GET /global/health → {healthy, version}（§20）。超时/非 2xx = 未就绪。
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.healthTimeout)
	defer cancel()

	var h HealthResponse
	if err := c.getJSON(ctx, c.httpClient, "/global/health", nil, &h); err != nil {
		return HealthResponse{}, err
	}
	return h, nil
}

// ListSessions GET /session?directory=<wt>&limit=<N>（§20）。ocdeck 显式传 limit=1000。
// 返回数==limit 视为溢出（ErrSessionOverflow），调用方据此告警并依赖 SSE 增量。
// session 列表字段形状校验（顶层 id/time.updated）在 parseSession 中天然保障，结构不符即错误。
func (c *Client) ListSessions(ctx context.Context, dir string, limit int) ([]Session, error) {
	q := url.Values{}
	q.Set("directory", dir)
	q.Set("limit", strconv.Itoa(limit))
	var raws []jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/session", q, &raws); err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(raws))
	for _, raw := range raws {
		s, err := parseSession(raw)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	if len(sessions) == limit {
		return sessions, ErrSessionOverflow
	}
	return sessions, nil
}

// GetSession GET /session/:id?directory=<wt>（§20）。404 = 不存在（ErrSessionNotFound），
// 必须与其他错误区分（attach 前预检、孤儿 session 清理）。
func (c *Client) GetSession(ctx context.Context, dir, id string) (Session, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raw jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/session/"+url.PathEscape(id), q, &raw); err != nil {
		if isNotFound(err) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, err
	}
	return parseSession(raw)
}

// CreateSession POST /session?directory=<wt>（design.md §4 锚定：无记录或 404 时创建新会话）。
// body `{"title": <title>}`；响应为单个 Session 对象，解析复用 parseSession（顶层 id、time.created/updated）。
// 401 → ErrUnauthorized（内部 bug）；其他非 2xx → 明确错误（激活失败，不回退）。
func (c *Client) CreateSession(ctx context.Context, dir, title string) (Session, error) {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/session?" + q.Encode()

	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return Session{}, fmt.Errorf("opencode: create session: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Session{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return Session{}, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return Session{}, &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	var raw jsonRawObject
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Session{}, fmt.Errorf("opencode: create session: decode body: %w", err)
	}
	return parseSession(raw)
}

// SessionStatus GET /session/status?directory=<wt>（§20）→
// map[sessionID]SessionStatus，type ∈ {idle,busy,retry}。结构不符 = 能力探测失败。
func (c *Client) SessionStatus(ctx context.Context, dir string) (map[string]SessionStatus, error) {
	q := url.Values{}
	q.Set("directory", dir)
	var raw map[string]jsonRawObject
	if err := c.getJSON(ctx, c.httpClient, "/session/status", q, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]SessionStatus, len(raw))
	for sid, r := range raw {
		st, err := parseSessionStatus(r)
		if err != nil {
			return nil, err
		}
		out[sid] = st
	}
	return out, nil
}

// DeleteSession DELETE /session/:id?directory=<wt>（§20）：200 + JSON true（非 204）。
// 404 视为已删除（幂等成功）。其余失败 → 由上层落 deletion_failed。
func (c *Client) DeleteSession(ctx context.Context, dir, id string) error {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/session/" + url.PathEscape(id) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // 404 幂等成功
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	// §20：成功响应是 JSON true。
	var ok bool
	if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil {
		return fmt.Errorf("opencode: delete session: decode body: %w", err)
	}
	if !ok {
		return fmt.Errorf("opencode: delete session: unexpected body (not true)")
	}
	return nil
}

// SubscribeEvents 订阅 SSE GET /event?directory=<wt>（§20/§18）。
// 立即建立连接；断流指数退避重连（有上限），重连成功后 onReconnect MUST 先于任何新事件
// 回调触发（供 TaskManager 执行全量对齐屏障）；ctx 取消即退订。
//
// 时序保证（design.md §4/§18）：
//   - 首次连接建立（收到首事件，通常 server.connected）后，触发 client.onReady 一次
//     （供 Activate 等待 SSE 建立后再全量对齐+启动 TUI）；之后才派发业务事件。
//   - 断流后：退避 → 新连接建立成功 → onReconnect 同步触发（TaskManager 此时做全量
//     对齐屏障）→ 对齐完成（onReconnect 返回）后才派发新连接的事件。
//
// 永久错误（401 / 明确不可重试的 4xx）快速失败上报，不无限重试。
// 心跳空闲超时（heartbeatTimeout>0 时）：超过此时长未见任何事件/注释即视为断流重连。
// malformed event 计数/记录（onMalformed），不中断流。
//
// onEvent 接收已解析的 Event；onReconnect 在每次成功重连后调用一次（首连接不算重连）。
// 阻塞直到 ctx 取消；返回 ctx.Err()。
func (c *Client) SubscribeEvents(ctx context.Context, dir string, onEvent func(Event), onReconnect func()) error {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/event?" + q.Encode()

	// firstConnect 区分首连接（触发 onReady）与重连（触发 onReconnect）。
	firstConnect := true
	var attempt int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 决定本次连接的 ready 回调：首连接 → onReady；重连 → onReconnect。
		var ready func()
		if firstConnect {
			ready = c.onReady
		} else {
			ready = onReconnect
		}
		established, err := c.sseConnect(ctx, reqURL, onEvent, ready)
		if err == nil {
			return nil // sseConnect 仅在 ctx 取消时返回 nil err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// 断流回调（P1.8.4）：仅已建立连接终止（established 且非 ctx 取消）时、进入
		// 重连退避前同步调用一次；从未建立的连接不触发。
		if established && c.onDisconnect != nil {
			c.onDisconnect()
		}
		// 永久错误：不重试，快速失败上报。
		if isPermanentSSEError(err) {
			return err
		}
		// 仅当连接已建立（established）后断流才视为一次成功连接，推进重连计数。
		// 首连接一旦 established 即标记完成，后续均按重连处理。
		if established {
			firstConnect = false
		}
		attempt++
		if c.reconnectMaxTries > 0 && attempt > c.reconnectMaxTries {
			return fmt.Errorf("opencode: sse reconnect attempts exhausted: %w", err)
		}
		// 指数退避，有上限；防溢出（attempt 较大时 math.Pow 会溢出，改用位移 + cap）。
		backoff := exponentialBackoff(c.reconnectBase, c.reconnectMax, attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// exponentialBackoff 计算指数退避时长：base * 2^(attempt-1)，上限 max；防溢出。
// attempt 从 1 开始。当 2^(attempt-1) 超过 max/base 时直接返回 max。
func exponentialBackoff(base, max time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	shift := attempt - 1
	// 限制 shift 以防 int64 溢出（time.Duration 为 int64 纳秒）。
	if shift > 62 {
		return max
	}
	d := base << uint(shift)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// isPermanentSSEError 判断 SSE 连接错误是否为永久错误（不重试）。
// 401（内部 bug）与明确不可重试的 4xx（除 408/429）视为永久。
func isPermanentSSEError(err error) bool {
	if errors.Is(err, ErrUnauthorized) {
		return true
	}
	var hse *httpStatusError
	if errors.As(err, &hse) {
		switch hse.status {
		case http.StatusUnauthorized:
			return true
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return false // 可重试
		}
		if hse.status/100 == 4 {
			return true // 4xx（非 401/408/429）永久失败
		}
	}
	return false
}

// sseConnect 建立一次 SSE 连接并解析事件。返回 (established bool, err error)：
//   - established=true 表示本次连接已收到过至少一个事件（连接被视为成功建立）。
//   - err 非 nil 表示连接终止原因；ctx 取消时返回 ctx.Err()。
//
// ready 在连接被视为建立（收到首事件，server.connected 或任意事件）后、任何业务事件
// 派发前同步触发一次（首连接=onReady 就绪信号；重连=onReconnect 对齐屏障）。
// ready 为 nil 时跳过。
//
// 首事件 server.connected（§20）不透传给 onEvent（仅作为连接存活信号）。
// malformed event 计数/记录（onMalformed），不中断流。
// heartbeatTimeout>0 时：超过此时长未见任何事件/注释即视为断流返回（established 视
// 当前是否已收过事件而定）。
func (c *Client) sseConnect(ctx context.Context, reqURL string, onEvent func(Event), ready func()) (bool, error) {
	// per-connection ctx：heartbeat 超时或正常返回时取消，确保 pump goroutine 与底层
	// 连接一并释放，避免泄漏。connCtx 派生自上层 ctx，故上层取消同样传导到 pump。
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	req, err := http.NewRequestWithContext(connCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.sseClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return false, &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}

	// 事件泵：在 goroutine 中阻塞解析，通过 channel 与 heartbeat 计时器竞争。
	// connCtx 取消时 parser.Next 解除阻塞（resp.Body 关闭）→ pump 退出。
	parser := newSSEParser(resp.Body)
	type result struct {
		ev  sseRawEvent
		err error
	}
	results := make(chan result, 1)
	go func() {
		for {
			ev, err := parser.Next(connCtx)
			// connCtx 取消：不再尝试投递，直接退出（避免阻塞在无消费者的 results）。
			if connCtx.Err() != nil {
				return
			}
			select {
			case results <- result{ev: ev, err: err}:
			case <-connCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	established := false
	readyFired := false
	var heartbeatTimer *time.Timer
	if c.heartbeatTimeout > 0 {
		heartbeatTimer = time.NewTimer(c.heartbeatTimeout)
		defer heartbeatTimer.Stop()
	}

	for {
		var heartbeatC <-chan time.Time
		if heartbeatTimer != nil {
			heartbeatC = heartbeatTimer.C
		}
		select {
		case <-ctx.Done():
			// 上层 ctx 取消：connCancel 由 defer 执行，pump 释放。
			return established, ctx.Err()
		case <-heartbeatC:
			// 心跳空闲超时：视为断流。established 仅在已收过事件时为 true。
			return established, errHeartbeatTimeout
		case r := <-results:
			if r.err != nil {
				// pump 结束（EOF/连接错误/ctx）。
				return established, r.err
			}
			if heartbeatTimer != nil {
				// 任何事件/注释到达即重置心跳计时器。
				if !heartbeatTimer.Stop() {
					select {
					case <-heartbeatTimer.C:
					default:
					}
				}
				heartbeatTimer.Reset(c.heartbeatTimeout)
			}
			if r.ev.Type == "" && len(r.ev.data) == 0 {
				continue // 注释行/heartbeat，无事件内容
			}
			// 收到首个事件即标记连接已建立。
			established = true
			// 连接建立后、业务事件派发前触发 ready（就绪信号/对齐屏障）。
			if !readyFired {
				readyFired = true
				if ready != nil {
					ready()
				}
			}
			parsed, perr := parseEvent(r.ev)
			if perr != nil {
				// envelope 结构不符：计数/记录而非静默丢弃，不中断流。
				c.malformedCount++
				if c.onMalformed != nil {
					c.onMalformed(r.ev, perr)
				}
				continue
			}
			if parsed.Type == "server.connected" {
				continue // 首事件，不透传（§20）——真实 opencode 无 event: 行，type 在 JSON payload 内
			}
			if onEvent != nil {
				onEvent(parsed)
			}
		}
	}
}

// errHeartbeatTimeout 心跳空闲超时（内部哨兵，触发重连）。
var errHeartbeatTimeout = errors.New("opencode: sse heartbeat idle timeout")

// getJSON 是各 GET 端点的共享实现：携带 Basic Auth + query，按状态码分类错误。
// v 可为 *jsonRawObject / *[]jsonRawObject / *map[string]jsonRawObject / *struct。
func (c *Client) getJSON(ctx context.Context, hc *http.Client, endpoint string, q url.Values, v interface{}) error {
	reqURL := c.baseURL + endpoint
	if q != nil {
		reqURL += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return &httpStatusError{status: http.StatusNotFound, body: readBodyForError(resp.Body)}
	}
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// jsonRawObject 保留原始 bytes 以便结构化解析（保留类型与顺序，避免二次猜测）。
type jsonRawObject []byte

func (r *jsonRawObject) UnmarshalJSON(data []byte) error {
	*r = append((*r)[0:0], data...)
	return nil
}

func (r jsonRawObject) MarshalJSON() ([]byte, error) { return r, nil }

// parseSession 从 raw 解析 Session，校验顶层 id 与 time.updated 形状（§20）。
// 缺失 id、缺失 time.updated、或 time.updated 非数字 → 结构漂移（包裹
// ErrCapabilityMismatch，供 Probe 分类为能力探测失败）。
func parseSession(raw jsonRawObject) (Session, error) {
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return Session{}, shapeErr(fmt.Errorf("parse session: %w", err))
	}
	if s.ID == "" {
		return Session{}, shapeErr(errors.New("parse session: missing top-level id"))
	}
	var extra map[string]interface{}
	_ = json.Unmarshal(raw, &extra)
	s.Raw = extra
	// time.updated 必须存在且为数字（§20 顶层 time.updated）。
	if _, ok := sessionUpdatedNumber(s); !ok {
		return Session{}, shapeErr(errors.New("parse session: missing or non-numeric time.updated"))
	}
	return s, nil
}

// sessionUpdatedNumber 从 Session.Raw 取 time.updated 数字（结构校验辅助）。
func sessionUpdatedNumber(s Session) (float64, bool) {
	if s.Raw == nil {
		return 0, false
	}
	t, ok := s.Raw["time"].(map[string]interface{})
	if !ok || t == nil {
		return 0, false
	}
	v, present := t["updated"]
	if !present || v == nil {
		return 0, false
	}
	return floatNumber(v)
}

// shapeErr 把结构校验错误包裹为 ErrCapabilityMismatch，便于上层（Probe）统一分类。
func shapeErr(err error) error {
	return fmt.Errorf("opencode: %w: %v", ErrCapabilityMismatch, err)
}

// parseSessionStatus 从 raw 解析 SessionStatus，校验 type 为三值枚举（§20）。
// 结构不符（含非三值枚举）即返回错误（包裹 ErrCapabilityMismatch）——结构/枚举不匹配
// = 能力探测失败。
func parseSessionStatus(raw jsonRawObject) (SessionStatus, error) {
	var st SessionStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return SessionStatus{}, shapeErr(fmt.Errorf("parse session status: %w", err))
	}
	switch st.Type {
	case StatusIdle, StatusBusy, StatusRetry:
	default:
		return SessionStatus{}, shapeErr(fmt.Errorf("parse session status: invalid type %q", st.Type))
	}
	var extra map[string]interface{}
	_ = json.Unmarshal(raw, &extra)
	st.Raw = extra
	return st, nil
}

// parseEvent 将 sseRawEvent 解析为 Event envelope（§20：{type, properties}）。
func parseEvent(ev sseRawEvent) (Event, error) {
	var e Event
	if len(ev.data) == 0 {
		return Event{Type: ev.Type}, nil
	}
	if err := json.Unmarshal(ev.data, &e); err != nil {
		return Event{}, fmt.Errorf("opencode: parse sse event: %w", err)
	}
	if e.Type == "" {
		e.Type = ev.Type
	}
	return e, nil
}

// isNotFound 判断是否 404（ErrSessionNotFound 的前置判断）。
func isNotFound(err error) bool {
	var hse *httpStatusError
	if errors.As(err, &hse) {
		return hse.status == http.StatusNotFound
	}
	return false
}

// readBodyForError 读取 body 供错误信息使用（有界，防 OOM）。
func readBodyForError(r io.Reader) string {
	const max = 4 * 1024
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return string(buf[:n])
}