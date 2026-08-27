// p18_agent_status_test.go 验证 P1.8 agentStatus 事件驱动维护（design.md D4）：
// 唯一 apply 聚合/delta、对账交集与 idle 缺省、连接代阶段机与陈旧写回防护、
// 断流顺序与激活代身份校验、owned 成员钩子（claim/delete/align）、空 owned 省略、
// 模式 B 等价分支、对账链 ctx 收敛、clearRuntime 清理与并发安全。
package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/application/runtime"
	"ocdeck/internal/config"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/infrastructure/opencode"
)

// --- 测试基建 ---

// p18Publisher 记录 Publish 调用（断言 serve_runtime.run_status_changed 生产/不生产）。
type p18Publisher struct {
	mu     sync.Mutex
	events []ocdeckevent.Event
}

func (p *p18Publisher) Publish(ev ocdeckevent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *p18Publisher) runStatus() []ocdeckevent.ServeRuntimeRunStatusChangedPayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ocdeckevent.ServeRuntimeRunStatusChangedPayload
	for _, ev := range p.events {
		if pl, ok := ev.Payload.(ocdeckevent.ServeRuntimeRunStatusChangedPayload); ok {
			out = append(out, pl)
		}
	}
	return out
}

// p18AppAdapter 复用 mockAppAdapter，补 OwnerOf 实测实现（P1.8.2 状态事件
// 归属反查经 lifecycle.OwnerOf；mockAppAdapter 原为 panic 占位）。
type p18AppAdapter struct {
	*mockAppAdapter
	store *mockStore
}

func (a *p18AppAdapter) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	found := ""
	for tid, rows := range a.store.sessions {
		for _, r := range rows {
			if r.SessionID != string(sessionID) {
				continue
			}
			if found != "" && found != tid {
				return "", false, fmt.Errorf("ambiguous owner for session %s", sessionID)
			}
			found = tid
		}
	}
	if found == "" {
		return "", false, nil
	}
	return found, true, nil
}

// p18HookOC 捕获 Options 回调（onReady/onDisconnect）与 onReconnect，供测试手动触发
// 断流/重连（mock SubscribeEvents 不自行产生连接事件）。
type p18HookOC struct {
	OCClient
	onReady      func()
	onDisconnect func()
	onReconnect  func()
}

func (c *p18HookOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	c.onReconnect = onReconnect
	if c.onReady != nil {
		c.onReady()
	}
	return c.OCClient.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}

// newP18Manager 构造注入 LifecycleService（recording publisher + OwnerOf 可用）的 Manager。
func newP18Manager(t *testing.T, store *mockStore, proc ProcessBackend, oc OCClient) (*Manager, *p18Publisher, *p18HookOC) {
	t.Helper()
	return newP18ManagerStores(t, store, store, proc, oc)
}

// newP18ManagerStores 同 newP18Manager，但 Manager 的 store 与 lifecycle adapter 的
// store 可分离（ListTaskSessions 故障注入、CAS 错误注入等场景）。
func newP18ManagerStores(t *testing.T, adapterStore *mockStore, mngStore TaskStore, proc ProcessBackend, oc OCClient) (*Manager, *p18Publisher, *p18HookOC) {
	t.Helper()
	pub := &p18Publisher{}
	adapter := &p18AppAdapter{mockAppAdapter: &mockAppAdapter{s: mngStore}, store: adapterStore}
	svc := apptask.New(apptask.Options{
		Tasks:    adapter,
		Read:     adapter,
		Sessions: adapter,
		Publish:  pub,
	})
	hook := &p18HookOC{OCClient: oc}
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	m := New(Options{
		Cfg: cfg, Store: mngStore, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(port int, password string, opts opencode.Options) OCClient {
			hook.onReady = opts.OnReady
			hook.onDisconnect = opts.OnDisconnect
			return hook
		},
		Lifecycle: svc,
	})
	m.SetLifecycleCtx(context.Background())
	return m, pub, hook
}

// p18StatusOp 构造状态事件写入 op。
func p18StatusOp(sid string, st opencode.SessionStatusType) agentStatusOp {
	return agentStatusOp{kind: agentOpStatusEvent, sessionID: sid, status: st}
}

// p18ReadyRuntime 构造已进入 valid 的 runtime（owned=sids，全部 idle），不产生发布。
// 序列与生产对账路径一致：Connect → OwnedSet（租约=aligning）→ AlignSuccess → 探测写回。
func p18ReadyRuntime(m *Manager, taskID string, sids ...string) (*taskRuntime, *agentStatusState) {
	rt := m.newRuntime(taskID)
	m.setRuntime(taskID, rt)
	a := rt.ensureAgentStatusState()
	a.apply(agentStatusOp{kind: agentOpConnect})
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: sids, epoch: 1, phase: agentPhaseAligning})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 1})
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		panic("p18ReadyRuntime: reconcilePending must be probeable")
	}
	a.apply(agentStatusOp{kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen, statuses: map[string]opencode.SessionStatusType{}})
	return rt, a
}

// p18PendingRuntime 构造对齐后待对账（reconcilePending）的 runtime（生产顺序：
// Connect → OwnedSet（租约=aligning）→ AlignSuccess）。
func p18PendingRuntime(m *Manager, taskID string, sids ...string) (*taskRuntime, *agentStatusState) {
	rt := m.newRuntime(taskID)
	m.setRuntime(taskID, rt)
	a := rt.ensureAgentStatusState()
	a.apply(agentStatusOp{kind: agentOpConnect})
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: sids, epoch: 1, phase: agentPhaseAligning})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 1})
	return rt, a
}

// p18SetP18Env 为后台重试（taskOcClient 经 ShowSessionEnvContext 读 serve env）预置 env。
func p18SetServeEnv(proc *mockProc, taskID string) {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	proc.envValues[runtimeSessionName(taskID)] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
	}
}

// --- 唯一 apply：聚合 / delta / 无变化不发布 ---

// TestP18_Apply_AggregationAndDelta 验证 busy>retry>idle 聚合、typed delta、
// 同值不重复发布、claim 0→1 默认 idle、delete 1→0 不可用、零 owned 省略、
// 已不可用时断流不重复发布（design D4/P1.8.6）。
func TestP18_Apply_AggregationAndDelta(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1", "s2")

	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("initial aggregate = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 0 {
		t.Fatalf("direct state apply should not publish; got %v", rs)
	}

	// busy 覆盖 idle：发布一次 idle→busy。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy))
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("aggregate after s1 busy = %q, want busy", got)
	}
	rs := pub.runStatus()
	if len(rs) != 1 || rs[0].From != "idle" || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("publish after busy = %+v, want one idle→busy available", rs)
	}

	// s1 → retry：聚合降为 retry（s2 idle），发布 busy→retry。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusRetry))
	if got := a.snapshotValue(); got != "retry" {
		t.Fatalf("aggregate after s1 retry = %q, want retry", got)
	}
	if rs := pub.runStatus(); len(rs) != 2 || rs[1].From != "busy" || rs[1].To != "retry" {
		t.Fatalf("publish after retry = %+v", rs)
	}

	// s2 → busy：busy>retry，聚合回 busy，发布 retry→busy。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s2", opencode.StatusBusy))
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("aggregate after s2 busy = %q, want busy (busy>retry)", got)
	}
	if rs := pub.runStatus(); len(rs) != 3 || rs[2].From != "retry" || rs[2].To != "busy" {
		t.Fatalf("publish after s2 busy = %+v", rs)
	}

	// s1 → idle：s2 仍 busy，聚合不变，无新发布。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusIdle))
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("aggregate after s1 idle = %q, want busy (s2 busy)", got)
	}
	if n := len(pub.runStatus()); n != 3 {
		t.Fatalf("aggregate unchanged must not republish; events = %d", n)
	}

	// 同值重复：无新发布。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s2", opencode.StatusBusy))
	if n := len(pub.runStatus()); n != 3 {
		t.Fatalf("same-value apply must not republish; events = %d", n)
	}

	// delete s2：剩 s1 idle，聚合 idle，发布 busy→idle。
	m.noteAgentSessionDeleted("t1", "s2")
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("aggregate after delete s2 = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 4 || rs[3].From != "busy" || rs[3].To != "idle" {
		t.Fatalf("publish after delete = %+v", rs)
	}

	// delete s1：1→0 立即不可用，发布 idle→""（available=false）。
	m.noteAgentSessionDeleted("t1", "s1")
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("aggregate after all deleted = %q, want empty (omit)", got)
	}
	if rs := pub.runStatus(); len(rs) != 5 || rs[4].From != "idle" || rs[4].To != "" || rs[4].Available {
		t.Fatalf("publish after 1→0 = %+v, want idle→\"\" unavailable", rs)
	}

	// claim 0→1：默认 idle 变可用，发布 ""→idle。
	m.noteAgentSessionClaimed("t1", "s3")
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("aggregate after claim = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 6 || rs[5].From != "" || rs[5].To != "idle" || !rs[5].Available {
		t.Fatalf("publish after 0→1 = %+v, want \"\"→idle available", rs)
	}

	// 断流：可用→不可用发布一次（经 commit 路径；epoch 匹配当前连接代）。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpDisconnect, epoch: a.currentEpoch()})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("snapshot after disconnect = %q, want empty", got)
	}
	if n := len(pub.runStatus()); n != 7 {
		t.Fatalf("disconnect should publish once; events = %d", n)
	}
	// 已不可用再断流：不重复发布。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpDisconnect, epoch: a.currentEpoch()})
	if n := len(pub.runStatus()); n != 7 {
		t.Fatalf("already-unavailable disconnect must not republish; events = %d", n)
	}
}

// --- 对账：交集过滤 / 缺失 idle / 空 owned 省略 ---

// TestP18_Probe_IntersectAndIdleDefault 验证 REST 目录级状态 map 与 owned 取交集
//（他任务 session 忽略）、owned 缺失按 idle、对账成功进入 valid（P1.8.6）。
func TestP18_Probe_IntersectAndIdleDefault(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	ctx := context.Background()

	oc := newMockOC(true)
	// s-foreign 为共享目录他任务 session：MUST 被交集过滤；s2 缺失按 idle。
	oc.sessionStatuses = map[string]opencode.SessionStatus{
		"s1":        {Type: "busy"},
		"s-foreign": {Type: "busy"},
	}
	m, pub, _ := newP18Manager(t, store, newMockProc(), oc)
	rt, a := p18PendingRuntime(m, "t1", "s1", "s2")
	m.probeAgentStatus(ctx, rt, "t1", "/wt", oc, agentStatusModeA, a.currentEpoch())

	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("aggregate after probe = %q, want busy (foreign ignored, s2 idle)", got)
	}
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("mode A valid phase must not be probed periodically")
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "" || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("publish after probe = %+v", rs)
	}

	// 全部缺失 → idle 参与聚合（新连接代重探），发布 ""→idle。
	oc2 := newMockOC(true)
	rt2, a2 := p18PendingRuntime(m, "t1", "s1")
	m.probeAgentStatus(ctx, rt2, "t1", "/wt", oc2, agentStatusModeA, a2.currentEpoch())
	if got := a2.snapshotValue(); got != "idle" {
		t.Fatalf("aggregate with missing statuses = %q, want idle", got)
	}

	// 空 owned：对账成功仍不可用（省略字段，不是 idle）、不发布。
	rt3, a3 := p18PendingRuntime(m, "t1")
	oc2.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	m.probeAgentStatus(ctx, rt3, "t1", "/wt", oc2, agentStatusModeA, a3.currentEpoch())
	if got := a3.snapshotValue(); got != "" {
		t.Fatalf("empty owned after probe = %q, want empty (omit)", got)
	}
	// 前两次探测各发布一次（busy / idle），空 owned 不得新增。
	if rs := pub.runStatus(); len(rs) != 2 {
		t.Fatalf("empty-owned probe must not publish availability; events = %d", len(rs))
	}
}

// TestP18_ReconcileFailure_BackgroundRetry 验证对账失败保持不可用（无发布）、
// 后台 30s tick 重试（经 tmux env 读取链恢复）直至成功（P1.8.3/P1.8.6）。
func TestP18_ReconcileFailure_BackgroundRetry(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	ctx := context.Background()
	proc := newMockProc()
	p18SetServeEnv(proc, "t1")

	oc := newMockOC(true)
	oc.sessionStatusErr = errors.New("probe down")
	m, pub, _ := newP18Manager(t, store, proc, oc)
	rt, a := p18PendingRuntime(m, "t1", "s1")

	m.probeAgentStatus(ctx, rt, "t1", "/wt", oc, agentStatusModeA, a.currentEpoch())
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after failed probe = %q, want empty", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("failed probe from reconcilePending must not publish; events = %d", n)
	}
	if !a.probeCandidate(agentStatusModeA) {
		t.Fatal("reconcilePending must remain retryable after failure")
	}

	// 后台重试（tick 分支直接调用，确定性）：仍失败 → 保持不可用。
	m.retryAgentStatusReconcile(ctx, agentStatusModeA)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after retry with failure = %q, want empty", got)
	}

	// 恢复后后台重试成功 → valid + 可用。
	oc.sessionStatusErr = nil
	oc.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "retry"}}
	m.retryAgentStatusReconcile(ctx, agentStatusModeA)
	if got := a.snapshotValue(); got != "retry" {
		t.Fatalf("after successful retry = %q, want retry", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "retry" || !rs[0].Available {
		t.Fatalf("publish after retry = %+v", rs)
	}
}

// --- 连接代：断流顺序 / 陈旧写回 / 阶段门控 ---

// TestP18_DisconnectSequenceAndTokenValidation 经 startSSE 全链验证断流顺序
//（disconnect→unavailable→reconnect+reconcile→available）与 runtime 激活代身份校验
//（旧实例回调不得触碰新实例，B4）。
func TestP18_DisconnectSequenceAndTokenValidation(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "s1", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	oc.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "idle"}}
	m, pub, hook := newP18Manager(t, store, proc, oc)

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	if err := m.startSSE(context.Background(), rt, "t1", "/data/worktrees/p1/t1", 50001, "pw", AlignModeRepo); err != nil {
		t.Fatalf("startSSE: %v", err)
	}
	defer m.clearRuntime("t1")

	if got := m.AgentStatusSnapshot("t1"); got != "idle" {
		t.Fatalf("snapshot after startSSE = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "idle" || !rs[0].Available {
		t.Fatalf("publish after initial reconcile = %+v", rs)
	}

	// 断流（已建立连接终止）→ 不可用，发布 idle→""。
	hook.onDisconnect()
	if got := m.AgentStatusSnapshot("t1"); got != "" {
		t.Fatalf("snapshot after disconnect = %q, want empty", got)
	}
	if rs := pub.runStatus(); len(rs) != 2 || rs[1].From != "idle" || rs[1].To != "" || rs[1].Available {
		t.Fatalf("publish after disconnect = %+v", rs)
	}

	// 重连：新 epoch → align → 对账 → 恢复可用，发布 ""→idle。
	hook.onReconnect()
	if got := m.AgentStatusSnapshot("t1"); got != "idle" {
		t.Fatalf("snapshot after reconnect = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 3 || rs[2].From != "" || rs[2].To != "idle" || !rs[2].Available {
		t.Fatalf("publish after reconnect = %+v", rs)
	}

	// 激活代身份校验：runtime 被替换后，旧闭包的断流回调 MUST 忽略（不发布、不触碰新实例）。
	newRT := m.newRuntime("t1")
	m.setRuntime("t1", newRT)
	newRT.registerGroup(roleRuntime, runtimeSessionName("t1"))
	hook.onDisconnect()
	if got := m.AgentStatusSnapshot("t1"); got != "" {
		t.Fatalf("new runtime snapshot = %q, want empty (no state yet)", got)
	}
	if n := len(pub.runStatus()); n != 3 {
		t.Fatalf("stale-instance disconnect must not publish; events = %d", n)
	}
}

// TestP18_StaleWritebackProtection 验证陈旧写回防护：对账在途 → 断流 → 陈旧成功
// MUST NOT 恢复可用；断流期间后台重试成功 MUST NOT 恢复（design D4）。
func TestP18_StaleWritebackProtection(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	ctx := context.Background()
	proc := newMockProc()
	p18SetServeEnv(proc, "t1")

	oc := newMockOC(true)
	oc.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	m, pub, _ := newP18Manager(t, store, proc, oc)
	rt, a := p18PendingRuntime(m, "t1", "s1")

	// 对账在途：先认领探测 (epoch, seq, 写代)，再断流，随后陈旧成功返回。
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		t.Fatal("reconcilePending must be probeable")
	}
	a.apply(agentStatusOp{kind: agentOpDisconnect, epoch: epoch})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after disconnect = %q, want empty", got)
	}
	// 陈旧成功写回：epoch/seq 匹配但已断流（写代亦已推进）→ 拒绝。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{
		kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen,
		statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy},
	})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("stale success after disconnect = %q, MUST NOT restore", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("stale success must not publish; events = %d", n)
	}

	// 断流期间后台重试成功：同样 MUST NOT 恢复。
	m.retryAgentStatusReconcile(ctx, agentStatusModeA)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("background retry during disconnect = %q, MUST NOT restore", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("background retry during disconnect must not publish; events = %d", n)
	}
}

// TestP18_EpochPhaseGating 验证阶段门控：aligning 跳过后台重试（含 align 失败的
// epoch 不得被恢复）、模式 A valid 不做周期探测、仅 reconcilePending 被探测。
func TestP18_EpochPhaseGating(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	ctx := context.Background()
	proc := newMockProc()
	p18SetServeEnv(proc, "t1")
	oc := newMockOC(true)
	oc.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	m, pub, _ := newP18Manager(t, store, proc, oc)

	// aligning（连接已建立、align 未完成/失败）：tick 不对账、不恢复。
	rt, a := p18AligningRuntime(m, "t1", "s1")
	_ = rt
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("aligning epoch must not be probeable")
	}
	m.retryAgentStatusReconcile(ctx, agentStatusModeA)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("aligning epoch after tick = %q, want empty", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("aligning epoch tick must not publish; events = %d", n)
	}

	// 模式 A：valid 阶段无周期探测（probeCandidate false）。
	_, a2 := p18ReadyRuntime(m, "t1", "s1")
	if a2.probeCandidate(agentStatusModeA) {
		t.Fatal("mode A valid phase must not be periodically probed")
	}
	m.retryAgentStatusReconcile(ctx, agentStatusModeA)
	if got := a2.snapshotValue(); got != "idle" {
		t.Fatalf("mode A valid snapshot after tick = %q, want idle (no probe)", got)
	}
}

// p18AligningRuntime 构造仅完成连接建立的 runtime（align 在途/失败形态）。
func p18AligningRuntime(m *Manager, taskID string, sids ...string) (*taskRuntime, *agentStatusState) {
	rt := m.newRuntime(taskID)
	m.setRuntime(taskID, rt)
	a := rt.ensureAgentStatusState()
	a.apply(agentStatusOp{kind: agentOpConnect})
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: sids, epoch: 1, phase: agentPhaseAligning})
	return rt, a
}

// --- handleSSEEvent 接线：状态事件 / 成员钩子 / 归属过滤 / 模式 B 忽略 ---

// p18StatusEvent 构造 session.status 事件（真实契约形状：properties.sessionID + status.type）。
func p18StatusEvent(sid, typ string) opencode.Event {
	return opencode.Event{
		Type: "session.status",
		Properties: map[string]interface{}{
			"sessionID": sid,
			"status":    map[string]interface{}{"type": typ},
		},
	}
}

// TestP18_HandleSSEEvent_StatusAndMembership 验证状态事件分支归属反查（fail-closed）、
// 孤儿/缺字段忽略、delete/claim 成员钩子重聚合（P1.8.2）。
func TestP18_HandleSSEEvent_StatusAndMembership(t *testing.T) {
	store := newMockStore()
	wtPath := "/data/worktrees/p1/t1"
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1")
	_ = rt
	ctx := context.Background()

	// 孤儿（未归属）状态事件：忽略，无变化无发布。
	if err := m.handleSSEEvent(ctx, "t1", wtPath, p18StatusEvent("s-orphan", "busy")); err != nil {
		t.Fatalf("orphan status event: %v", err)
	}
	if got := a.snapshotValue(); got != "idle" || len(pub.runStatus()) != 0 {
		t.Fatalf("orphan event must be ignored; snapshot=%q events=%d", got, len(pub.runStatus()))
	}

	// 缺 sessionID 的状态事件：SessionIDProp 为空 → 提前忽略。
	evNoSid := opencode.Event{Type: "session.status", Properties: map[string]interface{}{
		"status": map[string]interface{}{"type": "busy"},
	}}
	if err := m.handleSSEEvent(ctx, "t1", wtPath, evNoSid); err != nil {
		t.Fatalf("missing sessionID status event: %v", err)
	}
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("missing sessionID must be ignored; snapshot=%q", got)
	}

	// 已归属 session 的状态事件：更新聚合并发布。
	if err := m.handleSSEEvent(ctx, "t1", wtPath, p18StatusEvent("s1", "busy")); err != nil {
		t.Fatalf("owned status event: %v", err)
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("owned status event snapshot = %q, want busy", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" {
		t.Fatalf("owned status event publish = %+v", rs)
	}

	// session.deleted：成员移除 → 1→0 不可用发布。
	evDel := opencode.Event{Type: "session.deleted", Properties: map[string]interface{}{
		"info": map[string]interface{}{"id": "s1", "directory": wtPath},
	}}
	if err := m.handleSSEEvent(ctx, "t1", wtPath, evDel); err != nil {
		t.Fatalf("session.deleted: %v", err)
	}
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after delete snapshot = %q, want empty", got)
	}
	if rs := pub.runStatus(); len(rs) != 2 || rs[1].To != "" || rs[1].Available {
		t.Fatalf("delete publish = %+v, want unavailable", rs)
	}

	// session.created（directory 匹配）：claim 0→1 → 默认 idle 可用发布。
	evCreate := opencode.Event{Type: "session.created", Properties: map[string]interface{}{
		"info": map[string]interface{}{
			"id": "s2", "directory": wtPath,
			"time": map[string]interface{}{"created": 1.0, "updated": 1.0},
		},
	}}
	if err := m.handleSSEEvent(ctx, "t1", wtPath, evCreate); err != nil {
		t.Fatalf("session.created: %v", err)
	}
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("after claim snapshot = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 3 || rs[2].From != "" || rs[2].To != "idle" || !rs[2].Available {
		t.Fatalf("claim publish = %+v, want \"\"→idle available", rs)
	}
}

// TestP18_ModeB_Equivalents 验证模式 B 等价分支（经参数化 modeA=false 直接调用，
// 无全局翻转、无运行时切换）：忽略状态事件、valid 周期探测失败退回并单次发布、
// 同 epoch 重试成功恢复 valid（design D4 模式矩阵）。
func TestP18_ModeB_Equivalents(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	p18SetServeEnv(proc, "t1")
	oc := newMockOC(true)
	m, pub, _ := newP18Manager(t, store, proc, oc)
	rt, a := p18ReadyRuntime(m, "t1", "s1")
	_ = rt
	ctx := context.Background()

	// 模式 B：状态事件不解析不 apply。
	m.applySessionStatusEvent("t1", p18StatusEvent("s1", "busy"), false)
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("mode B must ignore status events; snapshot=%q", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("mode B must not publish from status events; events=%d", n)
	}

	// 模式 B：valid 阶段周期探测（后台 tick 以 modeA=false 运行）→ 失败退回
	// reconcilePending，单次发布 available→unavailable。
	oc.sessionStatusErr = errors.New("probe down")
	m.retryAgentStatusReconcile(ctx, false)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("mode B probe failure snapshot = %q, want empty", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "idle" || rs[0].To != "" || rs[0].Available {
		t.Fatalf("mode B demotion publish = %+v", rs)
	}
	// 重复失败：不重复发布。
	m.retryAgentStatusReconcile(ctx, false)
	if n := len(pub.runStatus()); n != 1 {
		t.Fatalf("repeated failure must not republish; events = %d", n)
	}

	// 同 epoch 重试成功：恢复 valid（唯一恢复路径），发布 ""→idle。
	oc.sessionStatusErr = nil
	oc.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "idle"}}
	m.retryAgentStatusReconcile(ctx, false)
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("mode B recovery snapshot = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 2 || rs[1].To != "idle" || !rs[1].Available {
		t.Fatalf("mode B recovery publish = %+v", rs)
	}
	if !a.probeCandidate(false) {
		t.Fatal("mode B valid phase must remain periodically probeable")
	}
}

// TestP18_FirstProbeImmediateAtReconcile 验证进入 reconcilePending 后立即首次探测
//（不等待后台周期；reconcileAgentStatus→probeAgentStatus 为模式 A/B 共用的首次探测
// 路径）。
func TestP18_FirstProbeImmediateAtReconcile(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	oc := &p18CountingOC{OCClient: newMockOC(true)}
	m, pub, _ := newP18Manager(t, store, newMockProc(), oc)
	rt, a := p18AligningRuntime(m, "t1", "s1")

	// align 成功 → reconcilePending → 立即首次探测（SessionStatus 被调用一次）。
	m.reconcileAgentStatus(context.Background(), rt, "t1", "/wt", oc)
	if oc.probes != 1 {
		t.Fatalf("first probe calls = %d, want 1 (immediate)", oc.probes)
	}
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("after first probe = %q, want idle", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "idle" || !rs[0].Available {
		t.Fatalf("first probe publish = %+v", rs)
	}
}

// p18CountingOC 统计 SessionStatus 调用次数。
type p18CountingOC struct {
	OCClient
	probes int
}

func (c *p18CountingOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	c.probes++
	return c.OCClient.SessionStatus(ctx, dir)
}

// --- 并发安全 / clearRuntime / ctx 收敛 ---

// TestP18_ConcurrentApplyRace 并发 apply（状态事件 + 成员钩子）下 race 安全、
// typed delta 与最终状态一致（From != To、终态与事件序列收敛）。
func TestP18_ConcurrentApplyRace(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, _ := p18ReadyRuntime(m, "t1", "s1", "s2")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy))
			m.noteAgentSessionClaimed("t1", fmt.Sprintf("extra-%d", i))
		}(i)
	}
	wg.Wait()

	// s1 busy 恒定聚合 busy；并发 claim 只新增 idle 成员不改变聚合。
	if got := rt.agentStatusStateOrNil().snapshotValue(); got != "busy" {
		t.Fatalf("final aggregate = %q, want busy", got)
	}
	for _, pl := range pub.runStatus() {
		if pl.From == pl.To {
			t.Fatalf("published delta with From == To: %+v", pl)
		}
	}
	// busy 迁移只应发布一次（首个并发 apply）；其余并发 claim 不改变聚合值。
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" {
		t.Fatalf("concurrent publishes = %+v, want single idle→busy", rs)
	}
}

// TestP18_ClearRuntimeClearsState 验证 clearRuntime 清空内存态（挂起/删除收敛防护）。
func TestP18_ClearRuntimeClearsState(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	_, a := p18ReadyRuntime(m, "t1", "s1")
	a.apply(p18StatusOp("s1", opencode.StatusBusy))

	m.clearRuntime("t1")
	if got := m.AgentStatusSnapshot("t1"); got != "" {
		t.Fatalf("snapshot after clearRuntime = %q, want empty", got)
	}
}

// p18BlockEnvProc 阻塞 ShowSessionEnvContext 直到调用方 ctx 取消（模拟阻塞中的
// tmux 环境读取，P1.8.6 ctx 收敛断言）。
type p18BlockEnvProc struct {
	ProcessBackend
}

func (p *p18BlockEnvProc) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestP18_BlockingEnvReadReturnsOnCtxCancel 验证对账链（tmux env 读取）在调用方
// ctx 取消后迅速返回，不等待底层固定超时。
func TestP18_BlockingEnvReadReturnsOnCtxCancel(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	blockProc := &p18BlockEnvProc{ProcessBackend: newMockProc()}
	m, _, _ := newP18Manager(t, store, blockProc, newMockOC(true))
	p18PendingRuntime(m, "t1", "s1")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		m.retryAgentStatusReconcile(ctx, agentStatusModeA) // env 读取阻塞直至 ctx 超时
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("retry took %v to return after ctx cancel", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking env read did not return after ctx cancel")
	}
}

// --- P1.8 复评阻塞修复：连接代栅栏 / 探测序号 / fail-closed / 快照门禁 / 收敛失效发布 ---

// p18ListErrStore 包装 TaskStore：ListTaskSessions 可注入错误（owned 重建故障注入）。
type p18ListErrStore struct {
	TaskStore
	listErr error
}

func (w *p18ListErrStore) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	if w.listErr != nil {
		return nil, w.listErr
	}
	return w.TaskStore.ListTaskSessions(ctx, taskID)
}

// p18SeedVisibleRunStatus 对当前 t1 runtime 构造可见 busy 的 agentStatus 快照
//（valid + available；直接经 state apply，不经 commit 路径——不产生发布，与
// p149SeedVisibleAttention 同构）。
func p18SeedVisibleRunStatus(t *testing.T, m *Manager) {
	t.Helper()
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("prereq: runtime missing")
	}
	a := rt.ensureAgentStatusState()
	a.apply(agentStatusOp{kind: agentOpConnect})
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"s1"}, epoch: 1, phase: agentPhaseAligning})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 1})
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		t.Fatal("prereq: reconcilePending must be probeable")
	}
	a.apply(agentStatusOp{kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen, statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy}})
	if v := a.snapshotValue(); v != "busy" {
		t.Fatalf("prereq: run status snapshot must be visible busy, got %q", v)
	}
}

// TestP18_DisconnectEpochFence 验证断流按连接代栅栏失效：旧连接代的延迟回调
//（epoch 失配）MUST NOT 失效新连接代；仅匹配当前代的断流使快照不可用并发布。
func TestP18_DisconnectEpochFence(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1") // 连接代 1，valid idle
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	// 重连：分配新连接代 2 并重新 valid（等价 onReconnect 的 connect→align→reconcile 序列）。
	a.apply(agentStatusOp{kind: agentOpConnect})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 2})
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		t.Fatal("reconnect reconcilePending must be probeable")
	}
	a.apply(agentStatusOp{kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen, statuses: map[string]opencode.SessionStatusType{}})
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("after reconnect = %q, want idle", got)
	}

	// 旧连接代（1）的延迟断流回调：MUST 被拒（新代不受影响、无发布）。
	m.handleAgentStatusDisconnect("t1", rt, 1)
	if got := a.snapshotValue(); got != "idle" {
		t.Fatalf("old-epoch disconnect MUST NOT invalidate newer epoch; snapshot=%q", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("old-epoch disconnect must not publish; events = %d", n)
	}

	// 当前连接代（2）断流：失效并发布 idle→""（available=false）。
	m.handleAgentStatusDisconnect("t1", rt, 2)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("current-epoch disconnect snapshot = %q, want empty", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "idle" || rs[0].To != "" || rs[0].Available {
		t.Fatalf("current-epoch disconnect publish = %+v", rs)
	}
}

// TestP18_OverlappingProbes_LateResultDiscarded 验证同 epoch 重叠探测按 (epoch, seq)
// 新者胜：旧探测的迟到成功 MUST NOT 写回（含已进入 valid 后再迟到的旧结果）。
func TestP18_OverlappingProbes_LateResultDiscarded(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18PendingRuntime(m, "t1", "s1")

	busy := map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy}
	e1, s1, g1, ok1 := a.beginProbe(agentStatusModeA, a.currentEpoch())
	e2, s2, g2, ok2 := a.beginProbe(agentStatusModeA, a.currentEpoch()) // 后台 tick 与首次对账重叠
	if !ok1 || !ok2 || e1 != e2 || s2 <= s1 || g1 != g2 {
		t.Fatalf("overlapping probes must share epoch/writeGen with increasing seq: e1=%d s1=%d g1=%d e2=%d s2=%d g2=%d", e1, s1, g1, e2, s2, g2)
	}

	// 旧探测迟到成功：MUST 被拒（保持不可用、无发布）。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpReconcileSuccess, epoch: e1, seq: s1, gen: g1, statuses: busy})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("late result of superseded probe = %q, MUST NOT write back", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("superseded probe result must not publish; events = %d", n)
	}

	// 最新探测成功：写回生效。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpReconcileSuccess, epoch: e2, seq: s2, gen: g2, statuses: busy})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("latest probe result = %q, want busy", got)
	}
	// 已 valid 后旧结果再迟到：同样被拒，聚合不回退。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpReconcileSuccess, epoch: e2, seq: s1, gen: g1, statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusIdle}})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("late result after valid = %q, MUST NOT regress", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("overlapping probes must yield single busy publish, got %+v", rs)
	}
}

// TestP18_Reconcile_ListSessionsFailureFailClosed 验证 owned 重建失败 fail-closed：
// MUST NOT 带着无法确认的成员集合探测（SessionStatus 零调用），阶段置 reconcileBlocked；
// 后台 tick 对受阻 runtime 重跑完整对账（re-list）而非陈旧成员探测；list 恢复后用
// 新鲜成员集对账写回。
func TestP18_Reconcile_ListSessionsFailureFailClosed(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	mock := newMockOC(true)
	mock.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	oc := &p18CountingOC{OCClient: mock}
	listErrStore := &p18ListErrStore{TaskStore: store, listErr: errors.New("store: list task sessions failed")}
	proc := newMockProc()
	p18SetServeEnv(proc, "t1") // 后台 tick 经 taskOcClient 读 serve env 构造 client
	m, pub, _ := newP18ManagerStores(t, store, listErrStore, proc, oc)
	rt, a := p18AligningRuntime(m, "t1", "s1")

	// 初始对账：owned 重建失败 → fail-closed（零探测）+ reconcileBlocked。
	m.reconcileAgentStatus(context.Background(), rt, "t1", "/wt", oc)
	if oc.probes != 0 {
		t.Fatalf("ListTaskSessions failure MUST be fail-closed (no probe), probes = %d", oc.probes)
	}
	if _, ok := a.beginFullReconcile(); !ok {
		t.Fatal("owned rebuild failure MUST leave epoch blocked until full reconcile")
	}
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("blocked phase must not be probeable (stale membership)")
	}
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after fail-closed reconcile = %q, want empty", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("fail-closed reconcile must not publish; events = %d", n)
	}

	// 后台 tick（list 仍失败）：重跑完整对账仍受阻 → 依旧零探测（绝不带陈旧成员探测）。
	m.retryAgentStatusReconcile(context.Background(), agentStatusModeA)
	if oc.probes != 0 {
		t.Fatalf("blocked runtime tick MUST NOT probe with stale membership, probes = %d", oc.probes)
	}
	if _, ok := a.beginFullReconcile(); !ok {
		t.Fatal("still-failing list MUST keep epoch blocked")
	}

	// 恢复：list 正常 + 新鲜成员集（s2 为受阻期间新增）→ tick 重跑完整对账：
	// 按新鲜 list 重建 owned（含 s2）后探测一次，busy 写回并发布。
	listErrStore.listErr = nil
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}, {TaskID: "t1", SessionID: "s2"}}
	m.retryAgentStatusReconcile(context.Background(), agentStatusModeA)
	if oc.probes != 1 {
		t.Fatalf("after recovery probes = %d, want 1 (full reconcile with fresh list)", oc.probes)
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after recovery = %q, want busy", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("recovery publish = %+v", rs)
	}
}

// TestP18_TimeoutInvalidation_AppliedAndDisconnectDedup 锁超时失效语义（design.md:426：
// 失效经唯一 apply 落态）：快照立即 ""；同 epoch 断流无投影变化不重复发布；
// worker 消化债务不产生第二次失效发布。
func TestP18_TimeoutInvalidation_AppliedAndDisconnectDedup(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1") // valid idle
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy)) // 发布 idle→busy

	// 锁超时路径：登记成功 → 失效经唯一 apply 落态（快照立即 ""）→ claim → 发布一次。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", rt.instVersion)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after timeout invalidation snapshot = %q, want empty immediately (apply-based)", got)
	}
	rs := pub.runStatus()
	if len(rs) != 2 || rs[1].From != "busy" || rs[1].To != "" || rs[1].Available {
		t.Fatalf("timeout MUST publish busy→\"\" unavailable exactly once, got %+v", rs)
	}

	// 同 epoch 断流（延迟回调）：状态已不可用 → 无投影变化 → 不重复发布。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpDisconnect, epoch: a.currentEpoch()})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after disconnect = %q, want empty", got)
	}
	if n := len(pub.runStatus()); n != 2 {
		t.Fatalf("disconnect after timeout MUST NOT duplicate invalidation publish; events = %d", n)
	}

	// worker 消化超时债务（清理 + 持久化收敛）：不产生第二次失效发布。
	m.processConvergeDebts(context.Background())
	if n := len(pub.runStatus()); n != 2 {
		t.Fatalf("worker after timeout MUST NOT publish a second invalidation; events = %d", n)
	}
}

// TestP18_TimeoutInvalidation_LaterEventRecovers 锁超时失效不冻结连接代（与断流
// 不同，runtime 可能仍健康）：同 epoch 后续状态事件是新事实，恢复可用且恰好发布
// 一次；同值重复不再发布。接续 worker 清理（②，CAS error）：恢复后的快照在 worker
// 处产生更高事实号的新失效事实（per-fact claim 获准，不被超时轮的 marker 抑制），
// 最终事件正确报告不可用（round-4 oracle 序列）。
func TestP18_TimeoutInvalidation_LaterEventRecovers(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub, _ := newP18ManagerStores(t, store, errStore, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1") // valid idle
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy)) // 发布 idle→busy

	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", rt.instVersion)
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after timeout invalidation snapshot = %q, want empty", got)
	}

	// 同 epoch 后续状态事件：恢复可用，恰好发布一次（""→busy）。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy))
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after recovery event snapshot = %q, want busy", got)
	}
	rs := pub.runStatus()
	if len(rs) != 3 || rs[2].From != "" || rs[2].To != "busy" || !rs[2].Available {
		t.Fatalf("recovery event MUST publish \"\"→busy once, got %+v", rs)
	}
	// 同值重复事件：不再发布。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy))
	if n := len(pub.runStatus()); n != 3 {
		t.Fatalf("same-value event must not republish; events = %d", n)
	}

	// worker 消化超时债务（清理前 runtime 已恢复 busy）：捕获即 apply 产生新事实
	//（事实号高于超时轮）→ claim 获准 → ② 发布 busy→""（不被旧 marker 抑制），
	// 最终事件正确报告不可用；随后 cleanup 清空 runtime。
	m.processConvergeDebts(context.Background())
	rs = pub.runStatus()
	if len(rs) != 4 || rs[3].From != "busy" || rs[3].To != "" || rs[3].Available {
		t.Fatalf("worker after recovery MUST publish the second invalidation fact (busy→\"\" unavailable), got %+v", rs)
	}
	if m.getRuntime("t1") != nil {
		t.Fatal("worker MUST have cleaned the runtime")
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST keep postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP18_StatusEventDuringProbe_EventWins 验证探测在途期间到达的状态事件（写代 +1）
// 使迟到的探测结果作废：REST 旧值（success 为 idle / failure 降级）MUST NOT 覆写
// 更新的事件事实。
func TestP18_StatusEventDuringProbe_EventWins(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1") // valid idle

	// 以模式 B 参数认领 valid 阶段的在途探测（模式 A valid 不探测；参数化等价分支）。
	epoch, seq, gen, ok := a.beginProbe(false, a.currentEpoch())
	if !ok {
		t.Fatal("mode B valid must be probeable")
	}

	// 探测在途期间状态事件 busy 到达（新事实，写代推进，发布 idle→busy）。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy))
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after event snapshot = %q, want busy", got)
	}

	// 迟到的探测成功结果（REST 快照为旧值 idle，携带过期写代）：丢弃，事件胜出。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{
		kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen,
		statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusIdle},
	})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("late probe result MUST NOT overwrite newer event; snapshot = %q", got)
	}
	// 迟到的失败结果（同样过期）：不因陈旧失败降级 valid。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{kind: agentOpProbeFailure, epoch: epoch, seq: seq, gen: gen})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("late probe failure MUST NOT demote after newer event; snapshot = %q", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "idle" || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("only the event's transition must publish, got %+v", rs)
	}
}

// TestP18_InvalidateCaptureIsApply_DeltaFromReflectsOpTimeProjection 验证失效「捕获即
// apply」原子性（round-4）：事件 from 只来自失效 op 锁内读取的投影——「预读可见性」
// 与「apply」之间插入的状态事件不会产生陈旧 from；同一事实重复发布被 per-fact claim
// 拒绝；恢复后的再失效为更高事实号的新事实。
func TestP18_InvalidateCaptureIsApply_DeltaFromReflectsOpTimeProjection(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1") // valid idle

	// 交错缝：旧实现「预读可见性」的等价时刻投影为 idle；此后状态事件先落地（busy）。
	a.apply(p18StatusOp("s1", opencode.StatusBusy)) // 直接 state apply，不发布
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("interleaved event snapshot = %q, want busy", got)
	}

	// 失效 op：delta.From 必为 op 时投影（busy），不是预读时刻的 idle。
	d := rt.invalidateAgentStatus()
	if d.From != "busy" || d.To != "" || d.Available || !d.Changed || d.FactID == 0 {
		t.Fatalf("invalidate delta MUST reflect op-time projection, got %+v", d)
	}
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("after invalidate snapshot = %q, want empty", got)
	}

	// 幂等：再次失效为 no-op（零 delta、无事实号）。
	d2 := rt.invalidateAgentStatus()
	if d2.From != "" || d2.Changed || d2.FactID != 0 {
		t.Fatalf("idempotent invalidate MUST be a no-change zero delta, got %+v", d2)
	}

	// 同一事实重复发布：claim 只准一次。
	m.publishRunStatusInvalidation("t1", rt.instVersion, d)
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "busy" || rs[0].To != "" || rs[0].Available {
		t.Fatalf("first publish of the fact = %+v", rs)
	}
	m.publishRunStatusInvalidation("t1", rt.instVersion, d)
	if n := len(pub.runStatus()); n != 1 {
		t.Fatalf("same-fact republish MUST be rejected by claim; events = %d", n)
	}

	// 恢复 → 再失效：更高事实号的新事实，claim 获准并发布。
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy)) // 恢复，发布 ""→busy
	d3 := rt.invalidateAgentStatus()
	if d3.From != "busy" || d3.FactID <= d.FactID {
		t.Fatalf("re-invalidation MUST be a new fact with higher FactID, got %+v (first %+v)", d3, d)
	}
	m.publishRunStatusInvalidation("t1", rt.instVersion, d3)
	if rs := pub.runStatus(); len(rs) != 3 || rs[2].From != "busy" || rs[2].To != "" || rs[2].Available {
		t.Fatalf("new-fact publish after recovery = %+v", rs)
	}
}

// TestP18_AlignBarrierExtendedThroughOwnedRebuild 验证 align 屏障延伸覆盖 re-list 窗口
//（round-4）：owned 重建成功并落态前 probeCandidate 恒 false（后台 tick 绝不带陈旧
// 成员集探测）；屏障开放后（pending）在途探测与成员钩子（claim）竞争——成员写推进
// 写代，陈旧探测结果被写代护栏拒绝，新鲜探测正常写回。
func TestP18_AlignBarrierExtendedThroughOwnedRebuild(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18AligningRuntime(m, "t1", "s1") // 连接已建立，re-list 窗口（aligning）

	// re-list 窗口：aligning 不可探测、也非完整对账受阻形态。
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("aligning (re-list window) MUST NOT be probeable")
	}
	if _, blocked := a.beginFullReconcile(); blocked {
		t.Fatal("aligning is not reconcileBlocked")
	}

	// 屏障开放（owned 已由 helper 落态）：pending 可探测。
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 1})
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		t.Fatal("reconcilePending must be probeable")
	}
	// 探测在途期间成员钩子落态（claim → OwnedAdd，写代 +1）：SSE 成员事实先于
	// REST 结果到达，事件胜出。
	a.apply(agentStatusOp{kind: agentOpOwnedAdd, sessionID: "s2"})
	// 迟到的探测结果（携带过期写代）：MUST 拒绝——不写回、不发布。
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{
		kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen,
		statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy, "s2": opencode.StatusBusy},
	})
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("probe result racing membership write = %q, MUST be discarded", got)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("discarded racing probe must not publish; events = %d", n)
	}

	// 成员写之后的新鲜探测（新写代）：正常写回。
	epoch2, seq2, gen2, ok2 := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok2 {
		t.Fatal("pending must remain probeable after membership write")
	}
	m.applyAgentStatusAndCommit("t1", rt, agentStatusOp{
		kind: agentOpReconcileSuccess, epoch: epoch2, seq: seq2, gen: gen2,
		statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy},
	})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("fresh probe after membership write = %q, want busy", got)
	}
}

// TestP18_SnapshotNonActiveEmpty 验证快照对非 active 任务返回空串：activating 窗口
//（状态已置 activating 而 runtime 已完成对账）与 suspended 均不外泄快照。
func TestP18_SnapshotNonActiveEmpty(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	p18ReadyRuntime(m, "t1", "s1") // valid idle（直接 apply，不发布）

	if got := m.AgentStatusSnapshot("t1"); got != "idle" {
		t.Fatalf("active snapshot = %q, want idle", got)
	}
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActivating })
	if got := m.AgentStatusSnapshot("t1"); got != "" {
		t.Fatalf("activating snapshot = %q, want empty (MUST NOT leak reconciled state)", got)
	}
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusSuspended })
	if got := m.AgentStatusSnapshot("t1"); got != "" {
		t.Fatalf("suspended snapshot = %q, want empty", got)
	}
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	if got := m.AgentStatusSnapshot("t1"); got != "idle" {
		t.Fatalf("back-to-active snapshot = %q, want idle", got)
	}
}

// TestP18_Converge_CASerr_VisibleRunStatus_PublishesOnce ②（CAS error）全路径 + 清理前
// run_status 可见 → serve_runtime.run_status_changed 恰好一次（busy→"" 不可用）+ ②a 登记。
func TestP18_Converge_CASerr_VisibleRunStatus_PublishesOnce(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub, _ := newP18ManagerStores(t, store, errStore, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p18SeedVisibleRunStatus(t, m)

	// 全路径：令牌校验 → 捕获可见性（attention 不可见 / run_status busy）→ cleanup
	//（清 agentStatus）→ CAS error → ② 顶部保守发布。
	m.convergeToSuspended("t1", "sse stream ended", rt.instVersion)

	rs := pub.runStatus()
	if len(rs) != 1 || rs[0].TaskID != "t1" || rs[0].From != "busy" || rs[0].To != "" || rs[0].Available {
		t.Fatalf("② with visible run status MUST publish run_status_changed once (busy→\"\" unavailable), got %+v", rs)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST register postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP18_Converge_Matrix3a_VisibleRunStatus_Invalidates ③a（!Matched 无错误 + 重读仍
// active）+ 携带清理前可见值进入矩阵 → run_status 失效恰好一次 + 登记 postCleanup。
func TestP18_Converge_Matrix3a_VisibleRunStatus_Invalidates(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub, _ := newP18ManagerStores(t, store, &p149CASNoMatchStore{TaskStore: store}, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	tok := rt.instVersion
	p18SeedVisibleRunStatus(t, m)
	// 模拟矩阵入口形态（runtime 已清、携带「捕获即 apply」产出的失效 delta，
	// 与 p149 同构——From/FactID 均来自该次 apply）。
	m.clearRuntime("t1")

	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, false, agentStatusDelta{From: "busy", FactID: 1}, nil)

	rs := pub.runStatus()
	if len(rs) != 1 || rs[0].From != "busy" || rs[0].To != "" || rs[0].Available {
		t.Fatalf("③a MUST publish run_status invalidation once, got %+v", rs)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup || entry.Token != tok {
		t.Fatalf("③a MUST register postCleanup debt for trigger token, got %+v (ok=%v)", entry, ok)
	}
}

// TestP18_Converge_LockTimeout_VisibleRunStatus_PublishesOnceAcrossWorker 锁超时路径：
// 登记 preCleanup 成功后发布 run_status 失效一次；worker preCleanup 清理 + ② 顶部对
// 同一令牌同一事实不重复发布（发布点 per-aspect claim）。
func TestP18_Converge_LockTimeout_VisibleRunStatus_PublishesOnceAcrossWorker(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub, _ := newP18ManagerStores(t, store, errStore, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p18SeedVisibleRunStatus(t, m)
	tok := rt.instVersion

	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].From != "busy" {
		t.Fatalf("timeout with visible run status MUST publish once, got %+v", rs)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePreCleanup {
		t.Fatalf("timeout MUST register preCleanup debt, got %+v (ok=%v)", entry, ok)
	}

	// worker preCleanup：清理前快照仍可见（捕获），claim 已占位 → 不重复发布。
	m.processConvergeDebts(context.Background())
	if rs := pub.runStatus(); len(rs) != 1 {
		t.Fatalf("timeout→worker sequence MUST publish run_status invalidation exactly once, got %d", len(rs))
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST keep postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP18_Converge_TOCTOU_TimeoutClaimDuringHeldLockWindow_SinglePublish TOCTOU 双发
// 防护（round-4：捕获即 apply）：持锁回调 A 的清理窗口内，无需任务锁的超时回调 B
// 先行「捕获即 apply」占住失效事实并 claim 发布；A 的失效 apply 随后为幂等 no-op
//（零 delta，From==""）→ 矩阵不发布——状态层面消除「先捕获后 apply」的双发窗口。
func TestP18_Converge_TOCTOU_TimeoutClaimDuringHeldLockWindow_SinglePublish(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub, _ := newP18ManagerStores(t, store, errStore, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p18SeedVisibleRunStatus(t, m)
	tok := rt.instVersion

	// B：持锁回调清理窗口内的同令牌超时回调（无需任务锁）→ 捕获即 apply + claim + 发布一次。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)
	// A：到达矩阵发布点——其失效 apply 已是 no-op（B 已置失效），携带零 delta → 不发布。
	m.convergeCommitCAS(context.Background(), "t1", "sse stream ended", tok, false, agentStatusDelta{}, nil)

	if rs := pub.runStatus(); len(rs) != 1 {
		t.Fatalf("TOCTOU: timeout claim during held-lock window MUST yield exactly one run_status_changed, got %d", len(rs))
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST keep postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// --- 完整对账租约（round-4 BLOCKER：阻塞期后台恢复 TOCTOU） ---

// TestP18_StaleBackgroundFullReconcileAbortsOnNewEpoch 交错序列（round-4 BLOCKER）：
// 后台 tick 判定受阻并签发租约 → 阻塞的 OC client 获取期间重连（新连接代进入
// aligning，自身 align 在途）→ 陈旧租约的完整对账恢复执行 MUST 静默中止：无探测、
// 无发布、新代保持 aligning；随后新代自身的 align 路径正常推进恢复。
func TestP18_StaleBackgroundFullReconcileAbortsOnNewEpoch(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	// 预对齐成员集（陈旧 list 结果：与 runtime 内 owned 不同）。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s2"}}
	mock := newMockOC(true)
	mock.sessionStatuses = map[string]opencode.SessionStatus{"s2": {Type: "busy"}}
	oc := &p18CountingOC{OCClient: mock}
	m, pub, _ := newP18Manager(t, store, newMockProc(), oc)
	rt, a := p18AligningRuntime(m, "t1", "s1") // 连接代 1，owned {s1}
	a.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: 1}) // 受阻形态

	// 后台 tick 判定 + 租约捕获（同一锁域）。
	lease, ok := a.beginFullReconcile()
	if !ok || lease.epoch != 1 || lease.phase != agentPhaseReconcileBlocked {
		t.Fatalf("blocked epoch MUST yield full-reconcile lease, got %+v ok=%v", lease, ok)
	}
	// 交错：租约签发后、OC client 获取期间重连 → 新连接代 2 进入 aligning。
	a.apply(agentStatusOp{kind: agentOpConnect})

	// 陈旧租约的完整对账恢复执行：MUST 静默中止。
	m.reconcileAgentStatusLeased(context.Background(), rt, "t1", "/wt", oc, lease)
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("new epoch MUST stay aligning (stale reconcile MUST NOT open reconcilePending)")
	}
	if _, blocked := a.beginFullReconcile(); blocked {
		t.Fatal("new epoch aligning is not blocked")
	}
	if oc.probes != 0 {
		t.Fatalf("stale reconcile MUST NOT probe, probes = %d", oc.probes)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("stale reconcile MUST NOT publish; events = %d", n)
	}
	if got := a.snapshotValue(); got != "" {
		t.Fatalf("snapshot after aborted reconcile = %q, want empty", got)
	}

	// 新代自身的 align 路径正常推进：完整对账（自签租约 = 新代）→ busy 可用。
	m.reconcileAgentStatus(context.Background(), rt, "t1", "/wt", oc)
	if oc.probes != 1 {
		t.Fatalf("new epoch's own reconcile probes = %d, want 1", oc.probes)
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after new epoch's own reconcile = %q, want busy", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("recovery publish = %+v", rs)
	}
}

// TestP18_BackgroundFullReconcileLeasePasses 反向钉住：换代未交错时租约全程有效——
// 受阻代的后台完整对账正常落态（owned 重建 → 屏障开放 → 探测写回 → 发布）。
func TestP18_BackgroundFullReconcileLeasePasses(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	mock := newMockOC(true)
	mock.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	oc := &p18CountingOC{OCClient: mock}
	m, pub, _ := newP18Manager(t, store, newMockProc(), oc)
	rt, a := p18AligningRuntime(m, "t1", "s1")
	a.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: 1})

	lease, ok := a.beginFullReconcile()
	if !ok {
		t.Fatal("blocked epoch MUST yield full-reconcile lease")
	}
	// 无交错：租约复验通过，完整对账正常完成。
	m.reconcileAgentStatusLeased(context.Background(), rt, "t1", "/wt", oc, lease)
	if oc.probes != 1 {
		t.Fatalf("leased full reconcile probes = %d, want 1", oc.probes)
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after leased full reconcile = %q, want busy", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("recovery publish = %+v", rs)
	}
}

// TestP18_FullReconcileLeaseStateFencing 状态层钉住租约栅栏的成员写防护：陈旧租约的
// OwnedSet/AlignSuccess 在新代上 MUST 双双 no-op——新代成员不被预对齐集合覆写
//（探测目录仅含 s1 时聚合为 busy 仅当成员仍是 {s1}）、屏障不被开放；同代同阶段
// 未失配时 OwnedSet 正常落态。
func TestP18_FullReconcileLeaseStateFencing(t *testing.T) {
	a := newAgentStatusState()
	a.apply(agentStatusOp{kind: agentOpConnect})                                                  // 连接代 1
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"s1"}, epoch: 1, phase: agentPhaseAligning}) // 成员 {s1}
	a.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: 1})                               // 受阻
	lease, ok := a.beginFullReconcile()
	if !ok || lease.epoch != 1 || lease.phase != agentPhaseReconcileBlocked {
		t.Fatalf("prereq lease = %+v ok=%v", lease, ok)
	}
	a.apply(agentStatusOp{kind: agentOpConnect}) // 换代：连接代 2，aligning

	// 陈旧租约写入：成员覆写与屏障开放 MUST 双双被拒。
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"s2"}, epoch: lease.epoch, phase: lease.phase})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: lease.epoch})
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("stale AlignSuccess MUST NOT open the newer epoch")
	}

	// 证据：新代走完自身路径（探测目录仅含 s1）→ 聚合 busy 仅当成员仍是 {s1}
	//（若陈旧 OwnedSet{s2} 落态则聚合为 idle）。
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 2})
	epoch, seq, gen, ok := a.beginProbe(agentStatusModeA, a.currentEpoch())
	if !ok {
		t.Fatal("new epoch pending must be probeable")
	}
	a.apply(agentStatusOp{kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen,
		statuses: map[string]opencode.SessionStatusType{"s1": opencode.StatusBusy}})
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("owned set MUST still be {s1} (stale OwnedSet rejected); aggregate = %q", got)
	}

	// 反向：同代同阶段未失配时 OwnedSet 正常落态（受阻代上按租约重建成员）。
	b := newAgentStatusState()
	b.apply(agentStatusOp{kind: agentOpConnect})
	b.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: 1})
	leaseB, okB := b.beginFullReconcile()
	if !okB {
		t.Fatal("prereq: blocked lease")
	}
	b.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"sX"}, epoch: leaseB.epoch, phase: leaseB.phase})
	b.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: leaseB.epoch})
	if !b.probeCandidate(agentStatusModeA) {
		t.Fatal("unexpired lease MUST land OwnedSet and open the barrier")
	}
}

// TestP18_StaleLeaseCannotClaimProbeOfNewerEpoch 精确复现 round-5 oracle 窗口：
// A 租约捕获（受阻代 1）→ 重连安装代 2 → A 的写入 ops 被守卫拒绝 → B 自身对账推进到
// reconcilePending（尚未探测）→ 陈旧 A 用先前获取的 client 抢先尝试探测 → 认领被
// 租约代校验拒绝（A 的 client 零调用、无发布）；随后 B 自签探测成功。全程以直接
// state ops 确定性交错，不把任何一段对账跑完整。
func TestP18_StaleLeaseCannotClaimProbeOfNewerEpoch(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	// A/B 各自的 client（独立计数：A 的 client 零调用即证明陈旧认领在 SessionStatus
	// 之前被拒）。
	mockA := newMockOC(true)
	mockA.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	ocA := &p18CountingOC{OCClient: mockA}
	mockB := newMockOC(true)
	mockB.sessionStatuses = map[string]opencode.SessionStatus{"s1": {Type: "busy"}}
	ocB := &p18CountingOC{OCClient: mockB}
	m, pub, _ := newP18Manager(t, store, newMockProc(), ocA)
	rt, a := p18AligningRuntime(m, "t1", "s1") // 连接代 1，owned {s1}
	a.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: 1}) // 受阻形态

	// ① 后台 tick 为代 1 捕获完整对账租约。
	lease, ok := a.beginFullReconcile()
	if !ok || lease.epoch != 1 {
		t.Fatalf("prereq lease = %+v ok=%v", lease, ok)
	}
	// ② 阻塞的 client 获取期间重连：代 2 进入 aligning。
	a.apply(agentStatusOp{kind: agentOpConnect})
	// ③ A 的写入 ops（直接 state 层模拟 reconcileAgentStatusLeased 的落态序列）：
	// OwnedSet/AlignSuccess 双双被守卫拒绝。
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"s1", "s-stale"}, epoch: lease.epoch, phase: lease.phase})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: lease.epoch})
	if a.probeCandidate(agentStatusModeA) {
		t.Fatal("stale A writes MUST NOT open epoch B's barrier")
	}
	// ④ B 自身对账推进到 reconcilePending（B 的 owned 已落态、屏障已开放——尚未探测）。
	a.apply(agentStatusOp{kind: agentOpOwnedSet, owned: []string{"s1"}, epoch: 2, phase: agentPhaseAligning})
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: 2})
	if !a.probeCandidate(agentStatusModeA) {
		t.Fatal("prereq: epoch B must be probeable (its own reconcile reached pending)")
	}
	// ⑤ oracle 窗口：B 探测之前，陈旧 A 用其先前获取的 client/租约抢先认领 → 拒绝。
	m.probeAgentStatus(context.Background(), rt, "t1", "/wt", ocA, agentStatusModeA, lease.epoch)
	if ocA.probes != 0 {
		t.Fatalf("stale lease MUST NOT claim a probe of the newer epoch; A client calls = %d", ocA.probes)
	}
	if n := len(pub.runStatus()); n != 0 {
		t.Fatalf("stale lease probe must not publish; events = %d", n)
	}
	// beginProbe 层同断言：陈旧租约无法获得 (epoch, seq)。
	if _, _, _, ok := a.beginProbe(agentStatusModeA, lease.epoch); ok {
		t.Fatal("beginProbe MUST reject a lease epoch that no longer matches the current epoch")
	}
	// ⑥ B 的自签探测（当前代）成功：写回 busy 并发布一次。
	m.probeAgentStatus(context.Background(), rt, "t1", "/wt", ocB, agentStatusModeA, a.currentEpoch())
	if ocB.probes != 1 {
		t.Fatalf("B's own probe calls = %d, want 1", ocB.probes)
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("after B's own probe = %q, want busy", got)
	}
	if rs := pub.runStatus(); len(rs) != 1 || rs[0].To != "busy" || !rs[0].Available {
		t.Fatalf("B's own probe publish = %+v", rs)
	}
}
