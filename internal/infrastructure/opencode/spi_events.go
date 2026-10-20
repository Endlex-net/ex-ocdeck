// spi_events.go 实现 EventSource 端口与 D8 SSE → typed events 的字段级映射
// （opencode-v1-spi-provider tasks 3.3）：逐字继承现有解析语义——六家族校验经
// ParseAttentionEvent、session.error 字段路径经 ParseSessionErrorEvent、
// session.status 经 ParseSessionStatusEvent、created/updated/deleted 的 directory
// 同源校验与 parentID/time 形状沿用 activate.go 现状。畸形/未知/越域事件静默
// 丢弃（不中断流）；未知 type 静默忽略。
package opencode

import (
	"context"
	"sync"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// v1Namespace 构造 v1 命名空间下的 SessionKey。
func v1Namespace(upstreamID string) domain.SessionKey {
	return domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: upstreamID}
}

// translateEvent 按 D8 表把一条上游事件映射为 domain.AgentEvent。
// ok=false 表示「静默丢弃」：畸形必填、未知 type、未知枚举（SSE 路径保持投影，
// 不应用）、directory 不匹配（created/updated/deleted 同源校验防线）。
func translateEvent(ev Event, bindingDir string) (domain.AgentEvent, bool) {
	switch ev.Type {
	case "session.created", "session.updated", "session.deleted":
		sid := ev.SessionID()
		if sid == "" {
			return domain.AgentEvent{}, false
		}
		// 同源校验（D8/activate.go 现状）：directory 缺失或与本任务目录不同 → 丢弃。
		dir, ok := ev.Directory()
		if !ok || dir != bindingDir {
			return domain.AgentEvent{}, false
		}
		info := ev.Info()
		if info == nil {
			return domain.AgentEvent{}, false
		}
		sess := domain.AgentSession{Key: v1Namespace(sid)}
		// C8 门禁修复（design D8 created/updated 字段必填规则还原）：逐事件沿用
		// activate.go 现状必填语义——created 仅要求 ID+directory（时间缺失仍捕获/
		// claim，时间保持零值）；updated 仅要求 time.updated（缺失 → 整条丢弃，
		// 与现状 return 语义一致）；deleted 仅要求 ID+directory（time 零值）。
		// 不设整体时间门禁（禁止丢弃 created 事件——时间字段为可选携带）。
		created, okC := eventTimeField(info, "created")
		updated, okU := eventTimeField(info, "updated")
		if okC {
			sess.CreatedAt = int64(created)
		}
		if okU {
			sess.UpdatedAt = int64(updated)
		}
		// 整体时间门禁已移除（C8 门禁修复，design D8）：created/deleted 不要求时间
		// 字段（时间缺失仍捕获——activate.go 现状 claim 不依赖时间）；updated 仅
		// 要求 time.updated。mutation 证据（已验证 → 红）：恢复门禁
		//（`ev.Type != "session.deleted" && (sess.CreatedAt==0 || sess.UpdatedAt==0)`）
		// → TestSPI_Events_TimeFieldsOptionalPerKind 的 timeMissingCreated 分支红
		//（SessionCreated 被丢弃，收 3 条超时）。
		if ev.Type == "session.updated" && !okU {
			return domain.AgentEvent{}, false
		}
		if title, ok := info["title"].(string); ok {
			sess.Title = title
		}
		if parent := ev.ParentID(); parent != "" {
			pk := v1Namespace(parent)
			sess.Parent = &pk
		}
		kind := domain.EventSessionCreated
		switch ev.Type {
		case "session.updated":
			kind = domain.EventSessionUpdated
		case "session.deleted":
			kind = domain.EventSessionDeleted
		}
		out, err := domain.NewSessionEvent(kind, sess)
		return out, err == nil

	case "session.status":
		se, ok := ParseSessionStatusEvent(ev)
		if !ok {
			// 缺 sessionID / 缺非法 status.type / 未知枚举：SSE 路径忽略不应用
			//（保持投影，agent_status_state 现状）。
			return domain.AgentEvent{}, false
		}
		var state domain.AgentStatus
		switch se.Status {
		case StatusIdle:
			state = domain.StatusIdle
		case StatusBusy:
			state = domain.StatusBusy
		case StatusRetry:
			state = domain.StatusRetry
		}
		out, err := domain.NewStatusEvent(v1Namespace(se.SessionID), domain.StatusDetail{
			State: state, Attempt: se.Attempt, Message: se.Message, NextAt: se.Next,
		})
		return out, err == nil

	case "session.error":
		// 字段路径唯一（session_error.go 现状）：sessionID 直读（MUST NOT 回退
		// info.id）；必填 name/message TrimSpace 非空；可空字段类型非法仅降级该字段。
		se, ok := ParseSessionErrorEvent(ev)
		if !ok {
			return domain.AgentEvent{}, false
		}
		out, err := domain.NewSessionErrorEvent(v1Namespace(se.SessionID), domain.SessionErrorDetail{
			Name: se.Name, Message: se.Message, StatusCode: se.StatusCode, IsRetryable: se.IsRetryable,
		})
		return out, err == nil

	default:
		// 六家族注意力事件（v1/v2 同形家族）：缺字段 → (zero,false) 静默忽略。
		aev, ok := ParseAttentionEvent(ev)
		if !ok {
			return domain.AgentEvent{}, false
		}
		kind := domain.EventAttentionRaised
		if aev.Kind == AttentionReplied || aev.Kind == AttentionRejected {
			kind = domain.EventAttentionResolved
		}
		att := domain.Attention{
			ID:      aev.RequestID,
			Kind:    domain.AttentionKind(aev.Type),
			Session: v1Namespace(aev.SessionID),
		}
		if aev.Type == AttentionPermission {
			att.Permission = &domain.PermissionPayload{
				ID: aev.RequestID, SessionID: aev.SessionID, Permission: aev.Permission,
				Patterns: aev.Patterns, Metadata: aev.Metadata,
			}
		} else {
			items := make([]domain.QuestionItem, 0, len(aev.Questions))
			for _, q := range aev.Questions {
				items = append(items, domain.QuestionItem{Header: q.Header, Question: q.Question})
			}
			att.Question = &domain.QuestionPayload{ID: aev.RequestID, SessionID: aev.SessionID, Questions: items}
		}
		out, err := domain.NewAttentionEvent(kind, att)
		return out, err == nil
	}
}

// eventTimeField 读取 info.time.<field> 数字（epoch 毫秒，不换算）。
func eventTimeField(info map[string]interface{}, field string) (float64, bool) {
	t, ok := info["time"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	return floatNumber(t[field])
}

// --- EventSource / EventStream ---

type spiEvents struct{ p *SPIProvider }

func (a *spiEvents) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	h := appagent.CapabilityHealth{ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
		Primitive: appagent.PrimitiveEventSource, Scope: appagent.ScopeRuntime}
	data, password, err := decodeV1Binding(b)
	if err != nil {
		a.p.reportConversion(h, "events.subscribe", err)
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	es := &spiEventStream{
		events: make(chan domain.AgentEvent, 64),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	a.p.reportConversion(h, "events.subscribe", nil)
	// C10 门禁修复：ready 信号 = 实际 SSE 连接建立（client onReady 同步回调，收到首
	// 事件后触发）——Subscribe 返回仅代表订阅受理，不得冒充连接建立；重连回调桥接
	// 调用方既有串行对齐屏障（onReconnect 先于新连接事件派发，fireReconnect 供订阅
	// 方经 SetCallbacks 挂接对齐域）。
	// volatile 语义：断流/溢出即终止（Err 携带终止原因）；客户端内部重连保留——重连
	// 建立经 onReconnect 触发对齐屏障，与原始通道 SubscribeEvents 语义一致。
	// onReady 经 Client Options 注入（构造期回调）；onReconnect 是 SubscribeEvents
	// 的调用参数。
	c2 := a.p.newClient(data.Port, password, Options{OnReady: es.markConnected, OnDisconnect: es.markDisconnected})
	// C10 门禁修复：streamErr 保存 SubscribeEvents 实际返回错误（终止原因），先于
	// events/done 关闭发布（Err 在 !ok 消费分支可达）；正常 ctx 取消保留 ctx.Canceled
	// 原因语义（与原始通道一致）。
	var streamErr error
	go func() {
		// C10 门禁修复：流退出顺序「先存 Err 再关 events」——Err 在 Close 观察前
		// 可见，调用方 `ev, ok := <-events` 的 !ok 分支可达（流终止可判定）。
		defer func() {
			es.setErr(streamErr)
			close(es.events)
			close(es.done)
		}()
		streamErr = c2.SubscribeEvents(sctx, data.Directory, func(ev Event) {
			typed, ok := translateEvent(ev, data.Directory)
			if !ok {
				return
			}
			select {
			case es.events <- typed:
			case <-sctx.Done():
			}
		}, es.fireReconnect)
		if streamErr != nil && sctx.Err() == nil {
			a.p.reportConversion(h, "events.stream", streamErr)
		}
	}()
	return es, nil
}

// spiEventStream 是 EventStream 的 v1 实现：Events 通道在 Close 后关闭；
// Err 在流终止后可读（终止原因；正常 Close 为 ctx.Canceled）。
// C10：connected/reconnect/disconnect 由实际 SSE 连接生命周期驱动（client onReady/
// onReconnect/onDisconnect 同步回调桥接），供调用方兑现「实际连接建立才 ready /
// 重连触发对齐屏障 / 断流立即失效 agent status」契约。
// C15（严格恰一次 + 全序）：连接生命周期事件（connected/reconnect/disconnected）
// 统一入 FIFO 队列（connMu 内原子），由 dispMu 串行分派器按序消费——注册与补发
// 共享同一分派域，杜绝「补发窗口内实时信号直调」的交错/重复；回调未注册时事件
// 留队（可重放），注入后按事件序恰一次补发。reconnect 只在有注册回调时派发
// （fireReconnect 无补发语义——既有契约：must 在 Subscribe 返回后注册，激活路径
// SetCallbacks 先于消费循环）。
type spiEventStream struct {
	events chan domain.AgentEvent
	done   chan struct{}
	cancel context.CancelFunc

	errMu sync.Mutex
	err   error

	connMu         sync.Mutex
	onConnected    func()
	onReconnect    func()
	onDisconnected func()
	// lifecycleEvents 是连接生命周期事件 FIFO（connMu 下原子入队/领取）：
	// mark* 在回调侧入队，SetCallbacks/SetDisconnectCallback 注入后由统一
	// 分派器按序恰一次消费。队内元素为事件序（C15：布尔 pending 会合并多个
	// 未消费信号，队列保留完整序）。
	lifecycleEvents []string
	// dispMu 串行化全部生命周期回调执行（实时触发与补发共享同一分派域——
	// C15：补发窗口内新的实时事件会在队首等待，不与本批补发交错）。
	dispMu sync.Mutex
}

func (s *spiEventStream) Events() <-chan domain.AgentEvent { return s.events }

// lifecycleEventKind 是连接生命周期事件名（FIFO 队列元素；事件序即真实序）。
const (
	lifecycleConnected    = "connected"
	lifecycleReconnect    = "reconnect"
	lifecycleDisconnected = "disconnected"
)

// markConnected 实际连接建立信号（client onReady）：入队后经统一分派器按序消费
//（C15：注册前入队可重放，注入后补发恰一次；与实时触发共享同一串行分派域）。
func (s *spiEventStream) markConnected() {
	s.enqueueLifecycle(lifecycleConnected)
}

// markDisconnected 断流信号（client onDisconnect：已建立连接终止、退避前同步一次；
// 主动 ctx 取消不触发）：入队后经统一分派器按序消费（C16：注册前入队可重放）。
func (s *spiEventStream) markDisconnected() {
	s.enqueueLifecycle(lifecycleDisconnected)
}

// enqueueLifecycle 生命周期事件原子入队（connMu），随后在锁外交给统一分派器
// 按序消费（分派器持有 dispMu，与入队锁分离——回调执行不占 connMu，防死锁）。
func (s *spiEventStream) enqueueLifecycle(kind string) {
	s.connMu.Lock()
	s.lifecycleEvents = append(s.lifecycleEvents, kind)
	s.connMu.Unlock()
	s.dispatchLifecycle()
}

// fireReconnect 重连建立信号（client onReconnect，先于新连接事件派发）：入队同域
// 分派；回调未注册时留存队列（可重放），与既有契约（must 在 Subscribe 返回后注册）
// 一致——激活路径 SetCallbacks 先于消费循环，重连不会先于注册发生。
func (s *spiEventStream) fireReconnect() {
	s.enqueueLifecycle(lifecycleReconnect)
}

// dispatchLifecycle 统一分派器：按 FIFO 序消费生命周期事件，回调未注册的队首
// 事件阻塞后续消费（保序），已注册则锁内领取、dispMu 临界区内恰一次执行。
// 实时信号与补发共享此分派器：补发执行期间到达的新事件在队首等待，不交错
//（C15：杜绝「补发窗口内 markConnected 重读已注册回调直调」的重复/交错）。
func (s *spiEventStream) dispatchLifecycle() {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	for {
		s.connMu.Lock()
		if len(s.lifecycleEvents) == 0 {
			s.connMu.Unlock()
			return
		}
		// 队首未就绪（回调未注册）→ 停止消费（事件留队待注入后补发）。
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
			// 未知事件不消费（防御形态，理论不可达）。
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

// SetCallbacks 注入连接生命周期回调（must 在 Subscribe 返回后、消费循环前调用；
// 回调统一经 dispatchLifecycle 串行分派）。注入后立即按事件序补发队列中已发生
// 的 connected/reconnect/disconnected（恰一次；队首未就绪语义使次序与真实连接
// 事件序一致）。
func (s *spiEventStream) SetCallbacks(onConnected, onReconnect func()) {
	s.connMu.Lock()
	s.onConnected = onConnected
	s.onReconnect = onReconnect
	s.connMu.Unlock()
	s.dispatchLifecycle()
}

// SetDisconnectCallback 注入断流回调（C16）；注入后按事件序补发（恰一次）——
// 与 SetCallbacks 共享同一队列与分派器。
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

// Close 终止订阅并等待 goroutine 收尾（幂等）。正常关闭返回 nil（调用方主动
// 取消属预期终止——非故障）；流异常终止的原因经 Err() 观测（C10：先存 Err 再关
// events，!ok 消费分支可达）。
func (s *spiEventStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}

var _ appagent.EventStream = (*spiEventStream)(nil)
