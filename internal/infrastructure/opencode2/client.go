// Package opencode2 实现 opencode v2 的 provider 适配层（opencode-v2-provider
// design D1）：v2 wire 差异全部封闭在包内——/api 前缀、{data} envelope、
// data.* 事件载荷、permission 双端点聚合、cursor 分页。wire 契约唯一来源 =
// 本包 CONTRACT.md（钉 v2.0.28 / 源码锚点 commit 026b106b）。
//
// 包结构与 v1（internal/infrastructure/opencode）镜像：client/sse/events/
// attention/prompt/session/status/terminal/titles/provider。v1 包保持冻结，
// 本包不 import v1（D1 双包并存，接受的重复仅限 transport 小件）。
//
// 仅使用标准库（net/http + bufio 手写 SSE 解析），不引入新依赖。
package opencode2

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

// v2 传输错误 sentinel（分类语义与 v1 client 同构；包内私有，SPI 分类在
// provider.go classifyHTTPError 完成）。
var (
	// ErrUnauthorized 401：凭据错误（派生凭据不应失配，内部 bug 语义）。
	ErrUnauthorized = errors.New("opencode2: unauthorized (internal bug)")
	// ErrSessionNotFound 404：session 不存在（SessionControl.Get/Delete、
	// Terminal 预验证消费）。
	ErrSessionNotFound = errors.New("opencode2: session not found")
	// ErrPermissionGone 404 且 _tag=PermissionNotFoundError：重复回复/幽灵
	// requestID（已了结竞态）。
	ErrPermissionGone = errors.New("opencode2: permission request not found")
	// ErrFormNotFound 404 且 _tag=FormNotFoundError。
	ErrFormNotFound = errors.New("opencode2: form not found")
	// ErrFormSettled 409 且 _tag=FormAlreadySettledError：form 已了结。
	ErrFormSettled = errors.New("opencode2: form already settled")
	// ErrContractUnavailable 运行时契约发现不可用（获取/解析失败；MUST NOT
	// 猜默认 pattern，对应能力保持待验证）。
	ErrContractUnavailable = errors.New("opencode2: runtime contract discovery unavailable")
)

// httpStatusError 携带 HTTP 状态码与原始 body，便于分类（404 vs 401 vs 5xx）。
type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("opencode2: http %d: %s", e.status, e.body)
}

// Client 封装对 v2 共享 serve（A+ 全局单 server）的 REST/SSE 访问。
// 凭据：用户名固定 "opencode"，password 经派生原语按 runtime 实例标识计算
// （同一 server 全部任务共用；见 provider.go）。
type Client struct {
	baseURL  string
	username string
	password string

	httpClient *http.Client
	sseClient  *http.Client // SSE 专用：无读超时，靠 heartbeat/重连

	reconnectBase     time.Duration
	reconnectMax      time.Duration
	reconnectMaxTries int

	// heartbeatTimeout SSE 流空闲超时（0 = 禁用）。
	heartbeatTimeout time.Duration

	// onReady 首个 SSE 事件到达后、任何业务事件派发前同步触发（握手判定：
	// 进程存活不算成功，收到首帧才算连接建立）。
	onReady func()

	// onDisconnect 已建立连接终止、退避前同步触发（主动 ctx 取消不触发）。
	onDisconnect func()

	// malformedCount 累计解析失败/未知事件数（保守丢弃+记录，诊断用）。
	malformedCount int64
}

// Options 构造 Client 的可调参数（零值可用，默认见 NewClient）。
type Options struct {
	OpTimeout         time.Duration
	ReconnectBase     time.Duration
	ReconnectMax      time.Duration
	ReconnectMaxTries int
	HeartbeatTimeout  time.Duration
	OnReady           func()
	OnDisconnect      func()
}

const (
	defaultOpTimeout         = 10 * time.Second
	defaultReconnectBase     = 500 * time.Millisecond
	defaultReconnectMax      = 30 * time.Second
	defaultReconnectMaxTries = 0
	defaultHeartbeatTimeout  = 60 * time.Second
)

// loopbackTransport 进程内共享 Transport（v1 同款语义：继承 DefaultTransport
// 有界回收配置 + 显式禁代理——v2 serve 亦仅 loopback 可达）。
var loopbackTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}()

// NewClient 构造 Client。port 为共享 serve 端口，password 为派生凭据。
func NewClient(port int, password string, opts Options) *Client {
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
	// debug 开启时包装传输层（[debug] 每请求 method/URI/status + 错误 body 前
	// 200 字符；凭据在 Authorization header，不经过传输层日志——见 debug.go）。
	var transport http.RoundTripper = loopbackTransport
	if debugEnabled() {
		transport = debugTransport{base: loopbackTransport}
	}
	return &Client{
		baseURL:           baseURL,
		username:          "opencode",
		password:          password,
		httpClient:        &http.Client{Timeout: opts.OpTimeout, Transport: transport},
		sseClient:         &http.Client{Timeout: 0, Transport: transport},
		reconnectBase:     opts.ReconnectBase,
		reconnectMax:      opts.ReconnectMax,
		reconnectMaxTries: opts.ReconnectMaxTries,
		heartbeatTimeout:  opts.HeartbeatTimeout,
		onReady:           opts.OnReady,
		onDisconnect:      opts.OnDisconnect,
	}
}

// MalformedCount 返回累计解析失败/未知事件数（诊断用；含保守丢弃的未知事件
// 与识别的 stream 级 failure）。
func (c *Client) MalformedCount() int64 { return c.malformedCount }

// apiURL 拼装 v2 API 地址：baseURL + /api 前缀 + 路径（CONTRACT §1 传输形态；
// 前缀拼装是本包唯一入口，禁止调用方手工拼 /api）。
func (c *Client) apiURL(path string, q url.Values) string {
	u := c.baseURL + "/api" + path
	if q != nil {
		u += "?" + q.Encode()
	}
	return u
}

// dataEnvelope 是 v2 成功响应的 {data} 包裹（CONTRACT §1）；错误响应非
// envelope（{_tag, message, kind}），不经本类型。
type dataEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// getJSONData GET 端点共享实现：Basic Auth + query + {data} envelope 解包。
// v 接收 data 载荷（指针）。fail-closed：非 2xx → httpStatusError（body 保留
// 原文供 _tag 分类）；envelope 缺 data / data 为 null / 根非 object → 协议违约。
func (c *Client) getJSONData(ctx context.Context, hc *http.Client, path string, q url.Values, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL(path, q), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp); err != nil {
		return err
	}
	return decodeEnvelope(resp.Body, v)
}

// classifyStatus 按 v2 语义分类非 2xx（CONTRACT §1：错误响应非 envelope，
// {_tag,...} 形态；body 原文保留供调用方按 _tag 细分）。
func classifyStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	case resp.StatusCode/100 != 2:
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	return nil
}

// decodeEnvelope 解包 {data} envelope 到 v。畸形 envelope（根非 object、缺
// data 键、data=null）→ 错误（fail-closed，不猜空值语义）。
func decodeEnvelope(r io.Reader, v interface{}) error {
	var env dataEnvelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return fmt.Errorf("opencode2: decode envelope: %w", err)
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return errors.New("opencode2: envelope 缺少 data 载荷")
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return fmt.Errorf("opencode2: decode data payload: %w", err)
	}
	return nil
}

// readBodyForError 读取 body 供错误信息使用（有界，防 OOM）。
func readBodyForError(r io.Reader) string {
	const max = 4 * 1024
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// maxReadBodyBytes 完整读取响应体的上限（分类/校验 MUST 基于完整 body——
// readBodyForError 单次 Read 不保证读全，不能用于 envelope 校验）。
const maxReadBodyBytes = 1 << 20

// readBodyComplete 读取有界完整正文（校验用；G2-F6）：上限内完整字节返回；
// 读取错误与超限显式报错（截断正文不得进入校验——合法大响应在 JSON 中间截断
// 会被误判协议违约）。
func readBodyComplete(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxReadBodyBytes+1))
	if err != nil {
		return data, fmt.Errorf("opencode2: read response body: %w", err)
	}
	if len(data) > maxReadBodyBytes {
		return data[:maxReadBodyBytes], fmt.Errorf("opencode2: response body exceeds %d bytes", maxReadBodyBytes)
	}
	return data, nil
}

// truncateForDiagnostics 把正文截断到诊断长度（4KB）——仅用于填充诊断字段
// （PromptResult.Body 等），MUST NOT 进入任何校验路径。
func truncateForDiagnostics(data []byte) string {
	const keep = 4 * 1024
	if len(data) > keep {
		return string(data[:keep])
	}
	return string(data)
}

// errorTag 解析错误响应体的 _tag（CONTRACT §1 错误形态）；空体/非法 JSON → 空串。
func errorTag(body string) string {
	var payload struct {
		Tag string `json:"_tag"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}
	return payload.Tag
}

// --- wire DTO（v2 形状；字段名以 CONTRACT.md/R1 OpenAPI 提取为准） ---

// wireSessionTime 是 Session.Info.time（required created/updated，epoch 毫秒）。
type wireSessionTime struct {
	Created *float64 `json:"created"`
	Updated *float64 `json:"updated"`
}

// wireSessionLocation 是 Session.Info.location（Location.PublicRef，required
// directory——v2 session 与目录的绑定在 Session.Info 上显式携带）。
type wireSessionLocation struct {
	Directory string `json:"directory"`
}

// wireSession 是 v2 Session.Info 的最小消费形状（其余字段不消费）。
type wireSession struct {
	ID       string              `json:"id"`
	Title    string              `json:"title"`
	ParentID string              `json:"parentID"`
	Time     wireSessionTime     `json:"time"`
	Location wireSessionLocation `json:"location"`
}

// parseWireSession 校验 v2 Session.Info 最小形状：id 非空 + time.created/
// updated 存在且为数字 + location.directory 非空（schema required 集合；
// 缺失 = 协议违约 fail-closed）。
func parseWireSession(raw []byte) (wireSession, error) {
	var s wireSession
	if err := json.Unmarshal(raw, &s); err != nil {
		return wireSession{}, fmt.Errorf("opencode2: parse session: %w", err)
	}
	switch {
	case s.ID == "":
		return wireSession{}, errors.New("opencode2: parse session: missing id")
	case s.Time.Created == nil || s.Time.Updated == nil:
		return wireSession{}, fmt.Errorf("opencode2: parse session %s: missing time.created/updated", s.ID)
	case s.Location.Directory == "":
		return wireSession{}, fmt.Errorf("opencode2: parse session %s: missing location.directory", s.ID)
	}
	return s, nil
}

// GetSession GET /api/session/{id}（CONTRACT §2）。404 → ErrSessionNotFound。
func (c *Client) GetSession(ctx context.Context, id string) (wireSession, error) {
	var raw json.RawMessage
	if err := c.getJSONData(ctx, c.httpClient, "/session/"+url.PathEscape(id), nil, &raw); err != nil {
		if isNotFound(err) {
			return wireSession{}, ErrSessionNotFound
		}
		return wireSession{}, err
	}
	return parseWireSession(raw)
}

// sessionListResponse 是 GET /api/session 的 cursor 分页包裹（CONTRACT §2：
// {data, cursor{previous,next}}，均 required）。
type sessionListResponse struct {
	Data   []json.RawMessage `json:"data"`
	Cursor struct {
		Previous *string `json:"previous"`
		Next     *string `json:"next"`
	} `json:"cursor"`
}

// decodeSessionListPage 严格解码列表页（CONTRACT §2 required 集合）：根必须是
// object；data 必须存在且为非 null 数组；cursor 必须是 object 且 previous/next
// 两个键都存在（值可为 null 或 string）。协议违约 → 错误，MUST NOT 退化为
// 「空完整快照」——畸形页在就绪探测里会伪造成 ready，在任务对账里会当完整空
// 快照删除已有归属行。
func decodeSessionListPage(r io.Reader) (sessionListResponse, error) {
	var page sessionListResponse
	var root map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&root); err != nil {
		return page, fmt.Errorf("opencode2: list sessions: decode root: %w", err)
	}
	if root == nil {
		return page, errors.New("opencode2: list sessions: root 非 object（null）")
	}
	rawData, ok := root["data"]
	if !ok {
		return page, errors.New("opencode2: list sessions: 缺少 data")
	}
	trimmed := bytes.TrimSpace(rawData)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return page, errors.New("opencode2: list sessions: data 非数组（null）")
	}
	if trimmed[0] != '[' {
		return page, fmt.Errorf("opencode2: list sessions: data 非数组: %s", truncateForError(trimmed))
	}
	if err := json.Unmarshal(rawData, &page.Data); err != nil {
		return page, fmt.Errorf("opencode2: list sessions: decode data: %w", err)
	}
	rawCursor, ok := root["cursor"]
	if !ok {
		return page, errors.New("opencode2: list sessions: 缺少 cursor")
	}
	trimmedCursor := bytes.TrimSpace(rawCursor)
	if len(trimmedCursor) == 0 || bytes.Equal(trimmedCursor, []byte("null")) || trimmedCursor[0] != '{' {
		return page, fmt.Errorf("opencode2: list sessions: cursor 非 object: %s", truncateForError(trimmedCursor))
	}
	var cursorFields map[string]json.RawMessage
	if err := json.Unmarshal(rawCursor, &cursorFields); err != nil {
		return page, fmt.Errorf("opencode2: list sessions: decode cursor: %w", err)
	}
	for _, key := range []string{"previous", "next"} {
		raw, ok := cursorFields[key]
		if !ok {
			return page, fmt.Errorf("opencode2: list sessions: cursor 缺少 %s", key)
		}
		t := bytes.TrimSpace(raw)
		if bytes.Equal(t, []byte("null")) {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return page, fmt.Errorf("opencode2: list sessions: cursor.%s 非 string/null: %s", key, truncateForError(t))
		}
		if key == "previous" {
			page.Cursor.Previous = &s
		} else {
			page.Cursor.Next = &s
		}
	}
	return page, nil
}

// truncateForError 截断原始 JSON 片段用于错误信息（有界）。
func truncateForError(raw []byte) string {
	const max = 128
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}

// SessionPage 是一页 session 列表；HasMore = cursor.next 非 null（不完整快照
// 信号，SPI ListResult.Overflow 的 v2 映射）。
type SessionPage struct {
	Sessions []wireSession
	HasMore  bool
}

// ListSessions GET /api/session?directory=<d>&limit=<N>（CONTRACT §2 cursor
// 分页；limit 精确生效已实证 R1）。适配层取单页，HasMore 由调用方按溢出语义
// 处置（MUST NOT 当作错误中止）。cursor 非对象或缺 cursor 键 = 协议违约。
func (c *Client) ListSessions(ctx context.Context, dir string, limit int) (SessionPage, error) {
	q := url.Values{}
	q.Set("directory", dir)
	q.Set("limit", strconv.Itoa(limit))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("/session", q), nil)
	if err != nil {
		return SessionPage{}, err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SessionPage{}, err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp); err != nil {
		return SessionPage{}, err
	}
	body, derr := decodeSessionListPage(resp.Body)
	if derr != nil {
		return SessionPage{}, derr
	}
	page := SessionPage{HasMore: body.Cursor.Next != nil}
	for _, raw := range body.Data {
		s, perr := parseWireSession(raw)
		if perr != nil {
			return SessionPage{}, perr
		}
		page.Sessions = append(page.Sessions, s)
	}
	return page, nil
}

// CreateSession POST /api/session，body {location: {directory}}（CONTRACT §2：
// v2 无 directory query 参数，目录定位在 body——v1→v2 create-directory 语义
// 变化）。响应 {data: Session.Info}，ID pattern 校验由调用方按 2.5 运行时
// 契约执行。
func (c *Client) CreateSession(ctx context.Context, dir string) (wireSession, error) {
	s, _, err := c.createSession(ctx, dir)
	return s, err
}

// createSession 是 CreateSession 的三分实现（收尾 K1）：返回（session, idHint, err）。
// idHint 非空表示响应里已可靠取得 session id——即使后续解码/形状校验失败，调用方
// 仍可对该 ID 执行受限回收（MUST NOT 因「未知」而误判未创建）。
// err 分类：
//   - 请求未发出/发送失败/连接建立失败 → 确定未创建（idHint 空，普通错误）；
//   - HTTP 非 2xx（classifyStatus）→ 上游明确拒绝，确定未创建；
//   - 连接在响应前断开/读体截断/响应不可解码 → 结果未知（idHint 可能非空）。
func (c *Client) createSession(ctx context.Context, dir string) (wireSession, string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"location": map[string]string{"directory": dir},
	})
	if err != nil {
		return wireSession{}, "", fmt.Errorf("opencode2: create session: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL("/session", nil), bytes.NewReader(body))
	if err != nil {
		return wireSession{}, "", err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return wireSession{}, "", &createResultUnknownError{err: err}
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp); err != nil {
		return wireSession{}, "", err
	}
	// 响应体先整体读取（有界），再解包——读取失败/截断 = 交付结果未知。
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return wireSession{}, "", &createResultUnknownError{err: rerr}
	}
	idHint := sniffSessionID(raw)
	var payload json.RawMessage
	if err := decodeEnvelopeBytes(raw, &payload); err != nil {
		return wireSession{}, idHint, &createResultUnknownError{err: err}
	}
	s, perr := parseWireSession(payload)
	if perr != nil {
		return wireSession{}, idHint, &createResultUnknownError{err: perr}
	}
	return s, s.ID, nil
}

// createResultUnknownError 标记 CreateSession 的交付结果未知（客户端内部类型，
// supervisor 转为 application 层 CandidateCreateUnknown）。
type createResultUnknownError struct{ err error }

func (e *createResultUnknownError) Error() string { return e.err.Error() }
func (e *createResultUnknownError) Unwrap() error { return e.err }

// sniffSessionID 在响应体里尽力提取 session id（`{"data":{"id":"ses_..."}}`）：
// 成功即让上层对已产生的 candidate 走受限回收/债务链，而不是误判「未创建」。
func sniffSessionID(raw []byte) string {
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	if env.Data.ID == "" {
		return ""
	}
	return env.Data.ID
}

// decodeEnvelopeBytes 与 decodeEnvelope 同语义（字节入口，供需要先读体的调用方复用）。
func decodeEnvelopeBytes(raw []byte, v interface{}) error {
	var env dataEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("opencode2: decode envelope: %w", err)
	}
	if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return errors.New("opencode2: envelope 缺少 data 载荷")
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return fmt.Errorf("opencode2: decode data payload: %w", err)
	}
	return nil
}

// DeleteSession DELETE /api/session/{id} → 204（CONTRACT §2；404 按 D7 映射
// ErrKindNotFound——v2 不做 v1 式「404 幂等成功」）。
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiURL("/session/"+url.PathEscape(id), nil), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return classifyStatus(resp)
}

// UpdateTitle PATCH /api/session/{id} body {title} → 204 空体（CONTRACT §2
// 实证：204 + GET 读回精确匹配）。
func (c *Client) UpdateTitle(ctx context.Context, id, title string) error {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return fmt.Errorf("opencode2: update session title: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.apiURL("/session/"+url.PathEscape(id), nil), bytes.NewReader(body))
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
	if err := classifyStatus(resp); err != nil {
		return err
	}
	// 204 空体；意外带体不消费（成功判定仅看状态码——已冻结契约）。
	return nil
}

// PutSessionEnvironment PUT /api/session/{id}/environment body {variables} →
// 204 替换式（CONTRACT §2；R2 p1b_environment 实证 session 间隔离）。tasks 5.4
// 配置应用链的应用点。
func (c *Client) PutSessionEnvironment(ctx context.Context, sessionID string, vars map[string]string) error {
	if vars == nil {
		vars = map[string]string{}
	}
	body, err := json.Marshal(map[string]interface{}{"variables": vars})
	if err != nil {
		return fmt.Errorf("opencode2: put session environment: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.apiURL("/session/"+url.PathEscape(sessionID)+"/environment", nil), bytes.NewReader(body))
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
	return classifyStatus(resp)
}

// InterruptSession POST /api/session/{id}/interrupt → 200 {interrupted:bool}
// （CONTRACT §4：/abort 不存在；false = idle no-op 成功）。挂起门禁的停止 wire。
func (c *Client) InterruptSession(ctx context.Context, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL("/session/"+url.PathEscape(sessionID)+"/interrupt", nil), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp); err != nil {
		return err
	}
	var out struct {
		Interrupted bool `json:"interrupted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("opencode2: interrupt: decode body: %w", err)
	}
	// true=实际中断活跃执行；false=idle no-op——两者均为停止确认成功。
	return nil
}

// isNotFound 判断是否 404。
func isNotFound(err error) bool {
	var hse *httpStatusError
	if errors.As(err, &hse) {
		return hse.status == http.StatusNotFound
	}
	return false
}
