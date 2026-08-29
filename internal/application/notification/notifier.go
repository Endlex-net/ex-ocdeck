package notification

import (
	"context"
	"log"
	"sync"
	"time"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/domain/notification"
)

// mode run loop 整体状态。
type mode int

const (
	modeRunning     mode = iota
	modeDisabled         // 启动基线枚举失败：整体禁用通知触发（待进程重启恢复）
	modeReconciling      // overflow 对账中：抑制全部发送并周期重试
)

// defaultTickInterval 判定周期（design D3：10s 扫描 tick；触发延迟上界 =
// 阈值 + 10s，已接受）。
const defaultTickInterval = 10 * time.Second

// Options Notifier 依赖注入（design D1 窄端口 + D11 装配形状）。
type Options struct {
	Bus            EventSubscriber    // Start 必需（engine 直测可不注入）
	Tasks          TaskSnapshotReader // 必需
	ListActive     ActiveTaskLister   // 必需（启动基线与对账枚举）
	Cfg            ConfigStore        // 必需（配置快照）
	Channels       []notification.Channel
	ResolveBaseURL BaseURLResolver
	Summarizer     SummaryCompleter // LLM 停止原因总结（D9；nil=未装配，全降级）
	LLMBudget      time.Duration    // LLM 总结预算上界（默认 5s；测试可注入）
	Now            func() time.Time // 时钟注入（测试 fake clock；默认 time.Now）
	TickEvery      time.Duration    // 判定周期（默认 10s）
}

// Notifier 通知触发编排器。engine 状态（mode/states/subs）仅 run loop goroutine
// 触达（单串行化上下文，spec「通知抑制、启动基线与对账」）；dispatchWG 供
// Stop/测试等待在途投递；lifecycleMu 保护 Start/Stop 终态状态机（并发安全）。
type Notifier struct {
	opts Options

	mode   mode
	states map[string]*taskState
	subs   []EventSubscription // 当前订阅（onOverflow 排空污染队列用；run loop 所属）

	lifecycleMu  sync.Mutex
	lifecycle    lifecycleState
	stop         context.CancelFunc
	done         chan struct{}
	baselineDone chan struct{} // 基线完成后关闭（Subscribe→基线→drain 顺序可观测）
	dispatchWG   sync.WaitGroup
}

// New 构造 Notifier（依赖为构造期合同；Bus 仅 Start 需要）。
func New(opts Options) *Notifier {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TickEvery <= 0 {
		opts.TickEvery = defaultTickInterval
	}
	return &Notifier{
		opts:         opts,
		mode:         modeRunning,
		states:       map[string]*taskState{},
		lifecycle:    lcStopped,
		baselineDone: make(chan struct{}),
	}
}

// Start 启动 run loop（单 goroutine：Subscribe → 基线 → 事件/溢出/tick 串行
// 处理，design D3）。生命周期终态状态机（B8）：Stop-before-Start 后 Start 为
// no-op；运行中重复 Start 为 no-op。
func (n *Notifier) Start(ctx context.Context) {
	n.lifecycleMu.Lock()
	if n.lifecycle != lcStopped {
		n.lifecycleMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	n.stop = cancel
	n.done = make(chan struct{})
	n.lifecycle = lcRunning
	n.lifecycleMu.Unlock()
	go func() {
		defer close(n.done)
		n.run(runCtx)
	}()
}

// Stop 停止 run loop 并等待退出与在途投递（D11：先于 tm.Shutdown，不再发通知）。
// 幂等；未 Start 时安全（转入终态，后续 Start no-op）。
func (n *Notifier) Stop() {
	n.lifecycleMu.Lock()
	switch n.lifecycle {
	case lcRunning:
		n.stop()
		n.lifecycle = lcDead
	case lcStopped:
		n.lifecycle = lcDead
	}
	done := n.done
	n.lifecycleMu.Unlock()
	if done != nil {
		<-done
	}
	n.dispatchWG.Wait()
}

// lifecycleState Start/Stop 生命周期状态（B8）：lcDead 为终态——Stop 先于
// Start 发生（或已完成 Stop）后，Start 不再启动不可停止的 loop。
type lifecycleState int

const (
	lcStopped lifecycleState = iota
	lcRunning
	lcDead
)

func (n *Notifier) lifecycleState() lifecycleState {
	n.lifecycleMu.Lock()
	defer n.lifecycleMu.Unlock()
	return n.lifecycle
}

// run 事件循环：先 Subscribe（两 topic）再取基线快照再 drain 队列（订阅缓冲中
// 已到达的事件在基线后按序处理）。
func (n *Notifier) run(ctx context.Context) {
	if n.opts.Bus == nil {
		log.Printf("notify: bus not configured, run loop exits")
		close(n.baselineDone)
		return
	}
	subServe := n.opts.Bus.Subscribe(ocdeckevent.TopicServeRuntime)
	defer subServe.Close()
	subTask := n.opts.Bus.Subscribe(ocdeckevent.TopicTask)
	defer subTask.Close()

	n.initBaseline(ctx)
	close(n.baselineDone)

	ticker := time.NewTicker(n.opts.TickEvery)
	defer ticker.Stop()
	subs := []EventSubscription{subServe, subTask}
	n.subs = subs
	for n.runOnce(ctx, subs, ticker.C) {
	}
}

// runOnce 单轮循环体（B2）：先非阻塞检查溢出（每订阅至多一个 token，B11——
// 溢出判定优先于普通事件与 tick，不受 select 随机性影响）；再 select 等待
// 事件/溢出/tick，普通分支执行前二次检查溢出（前置检查与 select 决策之间到达
// 的溢出同样优先，已取出的普通工作丢弃）。返回 false 表示退出（ctx 取消）。
func (n *Notifier) runOnce(ctx context.Context, subs []EventSubscription, tickC <-chan time.Time) bool {
	if ctx.Err() != nil {
		return false // B11：取消优先于 overflow 处理（恒满溢出信号下仍必须可退出）
	}
	if n.drainOverflowOnce(subs) {
		n.onOverflow(ctx)
		return true
	}
	var (
		ev     ocdeckevent.Event
		hasEv  bool
		isTick bool
	)
	select {
	case <-ctx.Done():
		return false
	case <-subs[0].Overflow():
		n.onOverflow(ctx)
		return true
	case <-subs[1].Overflow():
		n.onOverflow(ctx)
		return true
	case e := <-subs[0].C():
		ev, hasEv = e, true
	case e := <-subs[1].C():
		ev, hasEv = e, true
	case <-tickC:
		isTick = true
	}
	if hasEv {
		n.runNormal(ctx, subs, &ev, false)
	} else {
		n.runNormal(ctx, subs, nil, isTick)
	}
	return true
}

// runNormal 执行一轮选中的普通工作（事件或 tick）；执行前二次检查溢出
// （B2：select 决策与执行之间的窗口期内到达的溢出优先，已选普通工作丢弃）。
func (n *Notifier) runNormal(ctx context.Context, subs []EventSubscription, ev *ocdeckevent.Event, tick bool) {
	if n.drainOverflowOnce(subs) {
		n.onOverflow(ctx)
		return
	}
	if ev != nil {
		n.handleEvent(ctx, *ev)
		return
	}
	if tick {
		n.scan(ctx)
	}
}

// drainOverflowOnce 每订阅非阻塞消费至多一个溢出 token（B11：溢出信号为容量 1
// 的合并信号，持续发布时循环清空会活锁——单 token 消费保证每轮必能进入 select
// 响应取消/tick）。
func (n *Notifier) drainOverflowOnce(subs []EventSubscription) bool {
	fired := false
	for _, s := range subs {
		select {
		case <-s.Overflow():
			fired = true
		default:
		}
	}
	return fired
}

// handleEvent 领域事件应用（仅 run loop / 测试串行调用）。payload 形状异常按
// 未知事件忽略（bus 不做 schema 校验）；disabled/reconciling 期间 serve_runtime
// 触发事件不处理（对账中状态不可信，MUST NOT 带不可信状态继续投递）。
// serve_runtime 事件以 ev.RID（instVersion）做 fencing（B3）。
func (n *Notifier) handleEvent(ctx context.Context, ev ocdeckevent.Event) {
	switch ev.Type {
	case ocdeckevent.TypeServeRuntimeAttentionChanged:
		if p, ok := ev.Payload.(ocdeckevent.ServeRuntimeTaskPayload); ok && n.mode == modeRunning {
			n.onAttentionChanged(ctx, p.TaskID, ev.RID)
		}
	case ocdeckevent.TypeServeRuntimeRunStatusChanged:
		if p, ok := ev.Payload.(ocdeckevent.ServeRuntimeRunStatusChangedPayload); ok && n.mode == modeRunning {
			n.onRunStatusChanged(ctx, p, ev.RID)
		}
	case ocdeckevent.TypeServeRuntimeSessionError:
		if p, ok := ev.Payload.(ocdeckevent.ServeRuntimeSessionErrorPayload); ok && n.mode == modeRunning {
			n.onSessionError(ctx, p, ev.RID)
		}
	case ocdeckevent.TypeTaskStatusChanged:
		// 任务离开 active：取消该任务全部待决计时并清空触发态（含去重集合——
		// 离开 active 后 pending 快照为空，上界约束要求同步清空；重新激活后
		// 全部触发器重新武装）。迟到的旧 leave 事件不得删除新实例状态（B3）。
		if p, ok := ev.Payload.(ocdeckevent.TaskStatusChangedPayload); ok && p.From == application.StatusActive {
			n.onTaskLeftActive(ctx, ev.RID)
		}
	case ocdeckevent.TypeTaskDeleted:
		if p, ok := ev.Payload.(ocdeckevent.TaskDeletedPayload); ok && p.From == application.StatusActive {
			delete(n.states, ev.RID) // 删除为终态：无条件清空
		}
	}
}

// onTaskLeftActive 任务离开 active（B3）：读组合快照判定 leave 是否生效——任务
// 当前仍 active（leave 之后已重新激活，事件迟到）时仅清旧实例状态、保留新实例
// 状态；快照不可得或当前非 active 时全部清除（保守取消方向，MUST NOT 误发）。
func (n *Notifier) onTaskLeftActive(ctx context.Context, taskID string) {
	snap, err := n.readSnapshot(ctx, taskID)
	if err != nil || snap.Task.Status != application.StatusActive {
		delete(n.states, taskID)
		return
	}
	if st, ok := n.states[taskID]; ok && st.instVersion != snap.InstVersion {
		delete(n.states, taskID)
	}
}

// stateFor 取任务触发态。实例换代（instVersion 不一致，B3）时整体重建——旧
// 实例的计时/名额/去重不延续到新实例。G2：复验调用方已读的组合快照——任务
// 当前非 active 时删除该 task 状态且不重建，返回 nil（spec「离开 active 后取消
// 全部待决计时」；跨 topic 迟到的同实例 serve 事件不得复活状态）。
func (n *Notifier) stateFor(taskID, instVersion string, snap TaskSnapshot) *taskState {
	if snap.Task.Status != application.StatusActive {
		delete(n.states, taskID)
		return nil
	}
	if st, ok := n.states[taskID]; ok && st.instVersion == instVersion {
		return st
	}
	st := newTaskState(instVersion)
	n.states[taskID] = st
	return st
}

func (n *Notifier) readSnapshot(ctx context.Context, taskID string) (TaskSnapshot, error) {
	return n.opts.Tasks.TaskNotificationSnapshot(ctx, taskID)
}

// initBaseline 启动基线（spec「通知抑制、启动基线与对账」）：attention 只播种
// 去重集合、不补发通知；已是 idle 不武装；已是 retry 自此刻重新计时 1 分钟；
// error 计时不从历史恢复。枚举失败 → 记错误日志并整体禁用通知触发（配置 API
// 与测试通知不受影响，触发器待进程重启恢复）。单任务快照失败跳过该任务
// （fail-safe：不带未知状态武装计时）。
func (n *Notifier) initBaseline(ctx context.Context) {
	ids, err := n.opts.ListActive.ListAllActiveTaskIDs(ctx)
	if err != nil {
		log.Printf("notify: baseline enumerate active tasks failed, notification triggers disabled: %v", err)
		n.mode = modeDisabled
		return
	}
	now := n.opts.Now()
	for _, id := range ids {
		snap, err := n.readSnapshot(ctx, id)
		if err != nil {
			log.Printf("notify: baseline snapshot for task %s failed, skipped: %v", id, err)
			continue
		}
		st := newTaskState(snap.InstVersion)
		for _, pq := range snap.Attention.Questions {
			st.notifiedQuestions[pq.ID] = struct{}{}
		}
		for _, pp := range snap.Attention.Permissions {
			st.notifiedPermissions[pp.ID] = struct{}{}
		}
		if snap.RunStatus == runStatusRetry {
			st.episodeActive = true
			dl := now.Add(retryErrorWindow)
			st.retryDeadline = &dl
		}
		n.states[id] = st
	}
}

// onOverflow 订阅溢出（丢事件）对账入口（design D3）：先丢弃受污染订阅的既有
// 事件队列（B10：gap 前事件在对账后继续解释会覆盖重建状态），取消全部计时并
// 进入 reconciling 后立即尝试重建；重建成功恢复 running。disabled 为终态（B7：
// 溢出信号被消费但忽略，任何路径不得迁回 running——待进程重启恢复）。
func (n *Notifier) onOverflow(ctx context.Context) {
	if n.mode == modeDisabled {
		return
	}
	n.drainQueued(ctx, n.subs)
	for _, st := range n.states {
		st.idleSince, st.retryDeadline, st.errorDeadline = nil, nil, nil
	}
	n.mode = modeReconciling
	n.attemptReconcile(ctx)
}

// drainQueueBound 每通道单次排空上限（= 事件订阅缓冲容量，bus.go Sub.ch 为
// 64：进入 drain 时已缓冲的事件至多一个缓冲量，超出部分为 drain 期间新到达的
// gap 后事件，留给后续正常处理）。
const drainQueueBound = 64

// drainQueued 非阻塞排空受污染订阅的既有事件队列（B10：溢出意味着事件缺口，
// 缺口前到达的缓冲事件在对账重建后不得继续解释——直接丢弃）。B12：有界——
// 每通道至多消费 drainQueueBound 条且逐条检查取消（事件被并发持续补入时必须
// 返回，不阻塞对账与退出）。
func (n *Notifier) drainQueued(ctx context.Context, subs []EventSubscription) {
	for _, s := range subs {
		ch := s.C()
	drain:
		for i := 0; i < drainQueueBound; i++ {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ch:
			default:
				break drain // 该通道已空
			}
		}
	}
}

// attemptReconcile 以当前快照按启动基线同规则重建（pending 仅播种不补发、idle
// 不武装、retry 重新计时、error 不恢复）；保留仍 pending 的去重条目与
// episodeConsumed（MUST NOT 重发已消费的条件）。任一枚举/快照失败 → 保持
// reconciling（抑制全部发送，每 tick 重试），返回 false。
func (n *Notifier) attemptReconcile(ctx context.Context) bool {
	ids, err := n.opts.ListActive.ListAllActiveTaskIDs(ctx)
	if err != nil {
		log.Printf("notify: reconcile enumerate failed, stay reconciling: %v", err)
		return false
	}
	now := n.opts.Now()
	next := make(map[string]*taskState, len(ids))
	for _, id := range ids {
		snap, err := n.readSnapshot(ctx, id)
		if err != nil {
			log.Printf("notify: reconcile snapshot for task %s failed, stay reconciling: %v", id, err)
			return false
		}
		st := newTaskState(snap.InstVersion)
		if prev := n.states[id]; prev != nil && prev.instVersion == snap.InstVersion && snap.RunStatus != runStatusBusy {
			// 同实例存续 episode（B1/B3/B4，design D3 overflow 字段清单）：保留
			// episodeActive（error episode 持续到 busy，idle 门禁依赖其抑制）、
			// episodeConsumed 与 errorSeen；换代或快照 busy 时全部清除（busy =
			// episode 关闭语义）。去重 map 不在此保留——见下方按 pending 重建
			//（B13 剪枝）。
			st.episodeActive = prev.episodeActive
			st.episodeConsumed = prev.episodeConsumed
			st.errorSeen = prev.errorSeen
		}
		// 去重 map 仅由当前快照 pending 构建（B13：spec「去重集合以当前 pending
		// 集合为上界」——已了结条目剪除；当前 pending 全部播种=不补发）。
		for _, pq := range snap.Attention.Questions {
			st.notifiedQuestions[pq.ID] = struct{}{}
		}
		for _, pp := range snap.Attention.Permissions {
			st.notifiedPermissions[pp.ID] = struct{}{}
		}
		if snap.RunStatus == runStatusRetry {
			st.episodeActive = true
			dl := now.Add(retryErrorWindow)
			st.retryDeadline = &dl
		}
		next[id] = st
	}
	n.states = next
	n.mode = modeRunning
	return true
}
