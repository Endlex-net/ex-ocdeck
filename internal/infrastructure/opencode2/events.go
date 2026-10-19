// events.go 实现 EventSource 端口与 v2 事件归一化（opencode-v2-provider
// tasks 3.2/3.3 + design D7 事件归一化表）：GET /api/event SSE 订阅、首帧
// server.connected 握手判定（进程存活不算成功）、data.* 载荷取值与
// data.sessionID 归属校验、session.renamed 等事件名归一化。错误事件按已冻结
// 保守形态：仅识别 effect/http-api/stream/failure stream 级 failure；未知事件
// MUST NOT 猜测为 SessionError，保守丢弃+记录。断线重连后按快照自补（拉
// binding 的 upstream session 快照对齐，不依赖事件回放——CONTRACT §9）。
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
	"sync"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// streamFailureType 是唯一识别的 stream 级失败字面量（CONTRACT §7/§9：effect
// 框架运行时产物，不存在于 opencode 源码锚点；仅作识别信号，不映射 SessionError）。
const streamFailureType = "effect/http-api/stream/failure"

// wireEvent 是 v2 SSE 事件 envelope（CONTRACT §7）：
// {id: evt_*, created, type, location: {directory}?, data: {..., sessionID},
// durable?: {aggregateID, seq, version}}；归属键 = data.sessionID。
type wireEvent struct {
	ID        string
	Type      string
	Directory string
	Data      map[string]interface{}
}

// parseWireEvent 解析 SSE data 载荷为 wireEvent。UseNumber 保留数字词法值；
// data 缺失或非 object = 协议违约（v2 事件 data 恒为 object——归属键所在）。
func parseWireEvent(raw []byte) (wireEvent, error) {
	var payload struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Location *struct {
			Directory string `json:"directory"`
		} `json:"location"`
		Data map[string]interface{} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return wireEvent{}, fmt.Errorf("opencode2: parse sse event: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return wireEvent{}, errors.New("opencode2: parse sse event: trailing data")
	}
	if payload.Type == "" {
		return wireEvent{}, errors.New("opencode2: parse sse event: missing type")
	}
	if payload.Data == nil {
		return wireEvent{}, fmt.Errorf("opencode2: parse sse event %s: missing data", payload.Type)
	}
	ev := wireEvent{ID: payload.ID, Type: payload.Type, Data: payload.Data}
	if payload.Location != nil {
		ev.Directory = payload.Location.Directory
	}
	return ev, nil
}

// eventString 取 data[key] 非空 string。
func eventString(ev wireEvent, key string) (string, bool) {
	s, ok := ev.Data[key].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// eventTimeField 读取 data.time.<field> 数字（epoch 毫秒，不换算）。
func eventTimeField(ev wireEvent, field string) (float64, bool) {
	t, ok := ev.Data["time"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	return floatNumber(t[field])
}

// floatNumber 判断 v 是否为 JSON number（float64/json.Number）。
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

// translateV2Event 按 D7 归一化表把一条 v2 事件映射为 domain.AgentEvent。
// ok=false 表示「保守丢弃」：归属缺失（data.sessionID 缺失）、必填字段不符、
// 未知事件（含识别的 stream failure——调用方另行计数）。ok=false MUST NOT
// 映射为 SessionError（保守形态冻结）。sessionByDir 一致性校验不适用 v2：
// location 为可选字段（execution.started 帧实证无 location），归属仅凭
// data.sessionID。
func translateV2Event(ev wireEvent, statusOf func(domain.SessionKey, domain.AgentStatus)) (domain.AgentEvent, bool) {
	// 归属校验（D7）：data.sessionID 缺失 → 丢弃并记录（调用方计数）。
	sessionID, ok := eventString(ev, "sessionID")
	switch ev.Type {
	case "session.created", "session.renamed", "session.deleted":
		if !ok {
			return domain.AgentEvent{}, false
		}
		sess := domain.AgentSession{Key: v2Namespace(sessionID)}
		if title, ok := eventString(ev, "title"); ok {
			sess.Title = title
		}
		if parent, ok := eventString(ev, "parentID"); ok {
			pk := v2Namespace(parent)
			sess.Parent = &pk
		}
		if created, ok := eventTimeField(ev, "created"); ok {
			sess.CreatedAt = int64(created)
		}
		if updated, ok := eventTimeField(ev, "updated"); ok {
			sess.UpdatedAt = int64(updated)
		}
		kind := domain.EventSessionCreated
		switch ev.Type {
		case "session.renamed":
			kind = domain.EventSessionUpdated
		case "session.deleted":
			kind = domain.EventSessionDeleted
		}
		out, err := domain.NewSessionEvent(kind, sess)
		return out, err == nil

	case "session.execution.started", "session.execution.succeeded":
		// 冻结映射（D8-1 实证 PASS）：execution.started→busy、succeeded→idle。
		if !ok {
			return domain.AgentEvent{}, false
		}
		state := domain.StatusBusy
		if ev.Type == "session.execution.succeeded" {
			state = domain.StatusIdle
		}
		key := v2Namespace(sessionID)
		if statusOf != nil {
			statusOf(key, state)
		}
		out, err := domain.NewStatusEvent(key, domain.StatusDetail{State: state})
		return out, err == nil

	case "permission.asked":
		// attention 补证已冻结部分：data{id, sessionID, action, resources, save}
		//（R2 perm-frames.jsonl）；resolution 事件名未实证 → 保守不映射。
		id, okID := eventString(ev, "id")
		if !okID || !ok {
			return domain.AgentEvent{}, false
		}
		action, _ := eventString(ev, "action")
		att := domain.Attention{
			ID:      id,
			Kind:    domain.AttentionPermission,
			Session: v2Namespace(sessionID),
			Permission: &domain.PermissionPayload{
				ID: id, SessionID: sessionID, Permission: action,
				Patterns: optionalStringSlice(ev, "resources"),
			},
		}
		out, err := domain.NewAttentionEvent(domain.EventAttentionRaised, att)
		return out, err == nil

	default:
		// 未知事件与错误族：MUST NOT 猜测为 SessionError，保守丢弃+记录
		//（stream failure 由调用方按 streamFailureType 先行识别计数）。
		return domain.AgentEvent{}, false
	}
}

// optionalStringSlice 取 data[key] 字符串数组（缺失/null 合法 → nil）。
func optionalStringSlice(ev wireEvent, key string) []string {
	arr, ok := ev.Data[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- SSE 订阅（wire 层；断线重连退避与 v1 同构） ---

// SubscribeEvents 订阅 GET /api/event。断流指数退避重连（有上限）；重连成功
// 后 onReconnect 先于任何新事件回调触发；ctx 取消即退订。握手判定严格：每个
// 连接的首个可解析事件必须为 server.connected 才算连接建立并触发 ready
// （首连接=onReady、重连=onReconnect）——首帧为业务事件/未知事件/stream
// failure/畸形帧按握手失败处理，退避重连。server.connected 不透传。永久错误
// （401/不可重试 4xx）快速失败。阻塞直到 ctx 取消；返回 ctx.Err()。
func (c *Client) SubscribeEvents(ctx context.Context, dir string, onEvent func(wireEvent), onReconnect func()) error {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.apiURL("/event", q)

	firstConnect := true
	var attempt int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var ready func()
		if firstConnect {
			ready = c.onReady
		} else {
			ready = onReconnect
		}
		established, err := c.sseConnect(ctx, reqURL, onEvent, ready)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if established && c.onDisconnect != nil {
			c.onDisconnect()
		}
		if isPermanentSSEError(err) {
			return err
		}
		if established {
			firstConnect = false
		}
		attempt++
		if c.reconnectMaxTries > 0 && attempt > c.reconnectMaxTries {
			return fmt.Errorf("opencode2: sse reconnect attempts exhausted: %w", err)
		}
		backoff := exponentialBackoff(c.reconnectBase, c.reconnectMax, attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// exponentialBackoff 计算指数退避：base * 2^(attempt-1)，上限 max；防溢出。
func exponentialBackoff(base, max time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	shift := attempt - 1
	if shift > 62 {
		return max
	}
	d := base << uint(shift)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// isPermanentSSEError 判断 SSE 连接错误是否永久（不重试）：401 与非 408/429
// 的 4xx。
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
			return false
		}
		if hse.status/100 == 4 {
			return true
		}
	}
	return false
}

// sseConnect 建立一次 SSE 连接并解析事件（握手语义见 SubscribeEvents）。
func (c *Client) sseConnect(ctx context.Context, reqURL string, onEvent func(wireEvent), ready func()) (bool, error) {
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

	parser := newSSEParser(resp.Body)
	type result struct {
		ev  sseRawEvent
		err error
	}
	results := make(chan result, 1)
	go func() {
		for {
			ev, err := parser.Next(connCtx)
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

	// handshaked 是本次连接的握手状态（逐连接）：首个可解析事件必须严格为
	// server.connected 才视为连接建立（冻结契约：仅在观测到可解析的首帧
	// server.connected 后才将订阅视为建立）。首帧为业务事件/未知事件/stream
	// failure/畸形帧 → 握手失败（不 ready、不透传），按重连语义退避重试。
	handshaked := false
	for {
		var heartbeatC <-chan time.Time
		if heartbeatTimer != nil {
			heartbeatC = heartbeatTimer.C
		}
		select {
		case <-ctx.Done():
			return established, ctx.Err()
		case <-heartbeatC:
			return established, errHeartbeatTimeout
		case r := <-results:
			if r.err != nil {
				return established, r.err
			}
			if heartbeatTimer != nil {
				if !heartbeatTimer.Stop() {
					select {
					case <-heartbeatTimer.C:
					default:
					}
				}
				heartbeatTimer.Reset(c.heartbeatTimeout)
			}
			if r.ev.Type == "" && len(r.ev.data) == 0 {
				continue // 注释行/heartbeat（非事件帧，不参与握手）
			}
			if !handshaked {
				parsed, perr := parseWireEvent(r.ev.data)
				if perr != nil || parsed.Type != "server.connected" {
					// 握手失败：畸形首帧 / 首帧非 server.connected。不 ready、
					// 不透传；established 保持 false（连接从未建立——不触发
					// onDisconnect），由重连循环退避重试。
					c.malformedCount++
					return established, errHandshakeFailed
				}
				handshaked = true
				established = true
				if !readyFired {
					readyFired = true
					if ready != nil {
						ready()
					}
				}
				continue // 握手帧不透传
			}
			parsed, perr := parseWireEvent(r.ev.data)
			if perr != nil {
				c.malformedCount++
				continue
			}
			if parsed.Type == "server.connected" {
				continue // 后续握手帧（不应出现），不透传
			}
			if parsed.Type == streamFailureType {
				// 识别的 stream 级 failure（保守形态冻结）：计数记录，不映射
				// SessionError、不中断流。
				c.malformedCount++
				continue
			}
			if onEvent != nil {
				onEvent(parsed)
			}
		}
	}
}

// errHeartbeatTimeout 心跳空闲超时（内部哨兵，触发重连）。
var errHeartbeatTimeout = errors.New("opencode2: sse heartbeat idle timeout")

// errHandshakeFailed SSE 握手失败（内部哨兵，可重试）：首个可解析事件非
// server.connected 或首帧畸形（G2-F1 冻结契约：首帧握手严格判定）。
var errHandshakeFailed = errors.New("opencode2: sse handshake failed: first event is not server.connected")

// --- EventSource / EventStream（SPI） ---

type spiEvents struct{ p *SPIProvider }

// Subscribe 订阅共享 server 的事件流。事件处理顺序：归属校验/归一化（保守
// 丢弃计数）→ 状态追踪（execution 边界事件）→ typed 通道。重连自补：重连
// 建立信号触发后、新连接事件派发前，主动拉 binding 的 upstream session 快照
// 并以 SessionUpdated 事件注入（快照自补语义，CONTRACT §9；快照失败仅记录，
// 不终止流）。
func (a *spiEvents) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	h := a.p.healthBase(b, appagent.PrimitiveEventSource, appagent.ScopeRuntime, nil)
	data, err := decodeV2Binding(b)
	if err != nil {
		a.p.reportConversion(h, "events.subscribe", err)
		return nil, err
	}
	password, perr := derivePassword()
	if perr != nil {
		perr = appagent.NewError("events.subscribe", appagent.ErrKindBindingInvalid, perr)
		a.p.reportConversion(h, "events.subscribe", perr)
		return nil, perr
	}
	sctx, cancel := context.WithCancel(ctx)
	es := &spiEventStream{
		events: make(chan domain.AgentEvent, 64),
		done:   make(chan struct{}),
		cancel: cancel,
		// 评审 I12：流覆盖范围+连接代的状态失效（含主 session——订阅时即记代）。
		provider:    a.p,
		statusNotes: map[string]uint64{},
	}
	// 订阅方领受主 session 既有状态事实的责任（锁内快照当前代——断线清扫守卫）。
	if gen, ok := a.p.currentStatusGen(b.UpstreamSessionID); ok {
		es.noteStatusGenValue(b.UpstreamSessionID, gen)
	}
	a.p.reportConversion(h, "events.subscribe", nil)
	// I12 连接代失效：断流即旧代 execution 状态作废（按本流覆盖范围清扫——
	// Current 缺项 = 未知，不得用旧代 busy 猜测；重连新代仅由新的冻结映射事件
	// 重建；同目录多订阅经代际守卫互不污染）。
	c2 := a.p.newClient(data.Port, password, Options{
		OnReady:      es.markConnected,
		OnDisconnect: func() { es.markDisconnected(); es.invalidateStreamStatus() },
	})
	// statusOf 经流记账后写入 provider（同一临界区返回本次写入代——清扫守卫
	// 依据，评审 Round3 I12：写入与代际归属原子）。
	statusOf := func(key domain.SessionKey, state domain.AgentStatus) {
		gen := a.p.recordStatus(key, state)
		es.noteStatusGenValue(key.UpstreamID, gen)
	}

	// onReconnect：先入队生命周期信号（调用方对齐屏障），随后同步拉快照自补
	//（Client 保证本回调先于新连接事件派发——快照事件先于新事件入通道）。
	onReconnect := func() {
		es.fireReconnect()
		// I12：重连建立新连接代——旧代 execution 状态按流覆盖范围作废（代际
		// 守卫；selfHeal 的 SessionUpdated 快照不携带状态映射，不得据 outcome
		// 猜测补齐）。
		es.invalidateStreamStatus()
		es.selfHeal(sctx, c2, data, b.UpstreamSessionID)
	}

	var streamErr error
	go func() {
		defer func() {
			es.setErr(streamErr)
			close(es.events)
			close(es.done)
		}()
		streamErr = c2.SubscribeEvents(sctx, data.Directory, func(ev wireEvent) {
			typed, ok := translateV2Event(ev, statusOf)
			if !ok {
				// 保守丢弃+记录（归属缺失/必填不符/未知事件）。
				c2.malformedCount++
				return
			}
			select {
			case es.events <- typed:
			case <-sctx.Done():
			}
		}, onReconnect)
		if streamErr != nil && sctx.Err() == nil {
			a.p.reportConversion(h, "events.stream", streamErr)
		}
	}()
	return es, nil
}

// selfHeal 重连快照自补：GET binding 的 upstream session 快照，以
// SessionUpdated 事件注入通道（消费方可据此对齐；不依赖事件回放）。快照
// 失败仅丢弃（保守语义——不自造事件、不断流）。
func (s *spiEventStream) selfHeal(ctx context.Context, c *Client, data v2BindingData, upstreamID string) {
	if upstreamID == "" {
		return
	}
	sess, err := c.GetSession(ctx, upstreamID)
	if err != nil {
		return
	}
	typed, err := domain.NewSessionEvent(domain.EventSessionUpdated, toAgentSession(sess))
	if err != nil {
		return
	}
	select {
	case s.events <- typed:
	case <-ctx.Done():
	}
}

// spiEventStream 是 EventStream 的 v2 实现（生命周期回调语义与 v1 同构：
// C10 实际连接建立才 ready / C15 FIFO 恰一次补发 / C16 断流回调）。
type spiEventStream struct {
	events chan domain.AgentEvent
	done   chan struct{}
	cancel context.CancelFunc

	errMu sync.Mutex
	err   error

	connMu          sync.Mutex
	onConnected     func()
	onReconnect     func()
	onDisconnected  func()
	lifecycleEvents []string
	// connEstablished 当前是否处于已建立连接代（connMu 下；Round3 I10：订阅者
	// 移交依据——握手消费者已取走 connected 信号后，新 SetCallbacks 依此重放
	// 当前连接代，正式接管不二次等 ready）。
	connEstablished bool
	dispMu          sync.Mutex

	// statusNotes 本流写入过的 session 状态代（评审 I12：失效范围=流覆盖范围
	// +连接代——断线仅清除本流写入且其后无更新的事实；同目录多订阅互不污染）。
	statusNotesMu sync.Mutex
	statusNotes   map[string]uint64
	provider      *SPIProvider
}

// noteStatusGenValue 记录本流对 session 的本次写入代（由 provider.recordStatus
// 同临界区返回——Round3 I12 原子性：不得重读全局当前代，交错下会把他流新代记到
// 本流名下）。
func (s *spiEventStream) noteStatusGenValue(upstreamID string, gen uint64) {
	s.statusNotesMu.Lock()
	if s.statusNotes == nil {
		s.statusNotes = map[string]uint64{}
	}
	s.statusNotes[upstreamID] = gen
	s.statusNotesMu.Unlock()
}

// invalidateStreamStatus 断线/重连新代清扫：按本流记录的（session, 代）逐项
// 代际守卫失效——旧流不得清除新流刚写入的事实。
func (s *spiEventStream) invalidateStreamStatus() {
	s.statusNotesMu.Lock()
	notes := s.statusNotes
	s.statusNotes = nil
	s.statusNotesMu.Unlock()
	for id, gen := range notes {
		s.provider.invalidateStatusGen(id, gen)
	}
}

func (s *spiEventStream) Events() <-chan domain.AgentEvent { return s.events }

const (
	lifecycleConnected    = "connected"
	lifecycleReconnect    = "reconnect"
	lifecycleDisconnected = "disconnected"
)

func (s *spiEventStream) markConnected() {
	s.connMu.Lock()
	s.connEstablished = true
	s.connMu.Unlock()
	s.enqueueLifecycle(lifecycleConnected)
}

func (s *spiEventStream) markDisconnected() {
	s.connMu.Lock()
	s.connEstablished = false
	s.connMu.Unlock()
	s.enqueueLifecycle(lifecycleDisconnected)
}
func (s *spiEventStream) fireReconnect() { s.enqueueLifecycle(lifecycleReconnect) }

// enqueueLifecycle 生命周期事件原子入队，锁外交给统一分派器按序消费。
func (s *spiEventStream) enqueueLifecycle(kind string) {
	s.connMu.Lock()
	s.lifecycleEvents = append(s.lifecycleEvents, kind)
	s.connMu.Unlock()
	s.dispatchLifecycle()
}

// dispatchLifecycle 统一分派器：FIFO 序消费，队首回调未注册则停（留队补发），
// 已注册则锁内领取、dispMu 临界区内恰一次执行。
func (s *spiEventStream) dispatchLifecycle() {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	for {
		s.connMu.Lock()
		if len(s.lifecycleEvents) == 0 {
			s.connMu.Unlock()
			return
		}
		kind := s.lifecycleEvents[0]
		var cb func()
		switch kind {
		case lifecycleConnected:
			if s.onConnected == nil {
				s.connMu.Unlock()
				return
			}
			cb = s.onConnected
		case lifecycleDisconnected:
			if s.onDisconnected == nil {
				s.connMu.Unlock()
				return
			}
			cb = s.onDisconnected
		case lifecycleReconnect:
			if s.onReconnect == nil {
				s.connMu.Unlock()
				return
			}
			cb = s.onReconnect
		default:
			s.connMu.Unlock()
			return
		}
		s.lifecycleEvents = s.lifecycleEvents[1:]
		s.connMu.Unlock()
		if cb != nil {
			cb()
		}
	}
}

// SetCallbacks 注入连接生命周期回调（must 在 Subscribe 返回后、消费循环前；
// 注入后按事件序补发已发生的信号，恰一次）。Round3 I10 移交语义：若当前连接
// 代已建立且队列已空（原 connected 已被前一订阅者——如候选握手——消费），向
// 新订阅者重放 connected（不补发历史事件本身——事件经 channel 缓冲自然连续）；
// 队列非空说明原信号仍待补发，不重放（恰一次）。
func (s *spiEventStream) SetCallbacks(onConnected, onReconnect func()) {
	s.connMu.Lock()
	s.onConnected = onConnected
	s.onReconnect = onReconnect
	replay := s.connEstablished && len(s.lifecycleEvents) == 0
	s.connMu.Unlock()
	if replay && onConnected != nil {
		s.enqueueLifecycle(lifecycleConnected)
	}
	s.dispatchLifecycle()
}

// SetDisconnectCallback 注入断流回调（与 SetCallbacks 共享队列与分派器）。
func (s *spiEventStream) SetDisconnectCallback(cb func()) {
	s.connMu.Lock()
	s.onDisconnected = cb
	s.connMu.Unlock()
	s.dispatchLifecycle()
}

func (s *spiEventStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *spiEventStream) setErr(err error) {
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

// Close 终止订阅并等待 goroutine 收尾（幂等；正常关闭返回 nil）。
func (s *spiEventStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}

var _ appagent.EventStream = (*spiEventStream)(nil)
