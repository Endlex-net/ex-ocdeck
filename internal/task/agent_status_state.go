package task

import (
	"context"
	"log"
	"sync"

	"ocdeck/internal/infrastructure/opencode"
)

// agentStatusModeA 是 agentStatus 维护模式的编译期常量（design.md D4 模式执行矩阵，
// MUST NOT 做成运行时配置、MUST NOT 混搭两模式）。P1.7 门禁（2026-08-26，opencode
// 1.18.18）裁定 MODE A（事件驱动）：解析并 apply 状态事件；valid 阶段不做周期探测，
// 仅 reconcilePending 由后台重试对账。模式依赖分支以参数形态暴露（probeAgentStatus /
// retryAgentStatusReconcile / applySessionStatusEvent 的 modeA 参数）：生产调用点恒传
// 本常量，测试直接以 false 调用验证模式 B 等价分支——无全局翻转、无运行时切换。
const agentStatusModeA = true

// agentStatusPhase 连接代阶段机（design D4）：aligning → reconcilePending → valid；
// reconcileBlocked 为 reconcilePending 的 fail-closed 受阻形态（owned 重建失败，后台
// tick 须重跑完整对账而非带陈旧成员探测）。断流即终止当前连接代（connected=false，
// 阶段冻结）。
type agentStatusPhase int

const (
	agentPhaseAligning         agentStatusPhase = iota // 连接已建立，全量对齐在途
	agentPhaseReconcilePending                         // align 成功，可发起对账/探测
	agentPhaseValid                                    // 对账成功，快照可用
	agentPhaseReconcileBlocked                         // owned 重建失败：不可探测，须重跑完整对账（re-list）
)

// agentStatusDelta 唯一 apply 在锁内捕获的外部投影 typed delta（design D4）。From/To 为
// 聚合三态（idle/busy/retry）或 "" 表不可用；Available 为变化后外部可用性。发布方在
// 锁外直接使用本 delta 组装事件，MUST NOT 重新读取 runtime。Epoch 为写入后的当前连接代
//（Connect 写入即新分配的代）：SSE 断流回调依赖它捕获所属连接代（apply 的 Disconnect
// 按代匹配失效，旧连接延迟回调不得误伤新代）。FactID 为失效事实号：仅
// agentOpInvalidate 实际落态时取该次写入分配的写代号（writeGen，单调），供发布点
// per-fact claim（同一令牌上失效→恢复→再失效为不同事实，各自恰好发布一次）；其余
// op 恒 0。
type agentStatusDelta struct {
	From      string
	To        string
	Available bool
	Changed   bool
	Epoch     uint64
	FactID    uint64
}

// agentStatusOpKind 唯一 apply 的写入种类（design D4：状态事件、owned 成员变更、
// 对账结果、断流全部收敛到同一 apply）。
type agentStatusOpKind int

const (
	agentOpStatusEvent      agentStatusOpKind = iota // 状态事件更新单 session（模式 A）
	agentOpOwnedAdd                                  // claim 新增 owned 成员（默认 idle）
	agentOpOwnedRemove                               // delete 移除 owned 成员
	agentOpOwnedSet                                  // align 后按 store 重建 owned 集合
	agentOpConnect                                   // SSE 连接建立：新 epoch + aligning
	agentOpAlignSuccess                              // align 成功：进入 reconcilePending
	agentOpReconcileSuccess                          // 对账成功：写状态 + 进入 valid
	agentOpProbeFailure                              // 探测失败：valid 退回 reconcilePending（仅模式 B 会从 valid 探测）
	agentOpDisconnect                                // 断流：当前 epoch 失效
	agentOpInvalidate                                // 异常收敛强制不可用（lock-timeout/worker/持锁矩阵，design.md:426）
	agentOpReconcileBlocked                          // owned 重建失败：进入 reconcileBlocked（fail-closed）
)

// agentStatusOp 唯一 apply 的输入。字段按 kind 取用。
type agentStatusOp struct {
	kind      agentStatusOpKind
	sessionID string                                // StatusEvent/OwnedAdd/OwnedRemove
	status    opencode.SessionStatusType            // StatusEvent
	owned     []string                              // OwnedSet
	epoch     uint64                                // Disconnect/Invalidate/AlignSuccess/ReconcileBlocked/OwnedSet/ReconcileSuccess/ProbeFailure 的代匹配校验
	seq       uint64                                // ReconcileSuccess/ProbeFailure 的探测序号校验（发放时最新）
	gen       uint64                                // ReconcileSuccess/ProbeFailure 的写代校验（beginProbe 时快照）
	phase     agentStatusPhase                      // OwnedSet 的租约阶段校验（完整对账发起时捕获）
	statuses  map[string]opencode.SessionStatusType // ReconcileSuccess（REST 全目录状态 map，apply 内与 owned 取交集）
}

// agentStatusLease 完整对账租约（round-4 BLOCKER 修复）：发起完整对账时在状态锁域内
// 捕获的 (epoch, phase)。租约贯穿 re-list 阻塞窗口，在 OwnedSet/AlignSuccess 落态点由
// apply 守卫原子复验——失配（阻塞的 OC client 获取期间换代/阶段推进）即陈旧对账，
// 其全部写入 MUST 被拒绝（无成员写、无屏障开放、无探测、无发布），新代走自己的
// align 路径。
type agentStatusLease struct {
	epoch uint64
	phase agentStatusPhase
}

// agentStatusState 挂在 taskRuntime 上的 agentStatus 内存态（design D4，attention 懒初始化
// 先例）。sessions 即 owned session 集合（claim/delete/align 成员变更经同一 apply 维护），
// 值为 session 级状态；epoch 为独立于激活代的单调连接代（每次 SSE 连接建立 +1）；
// probeSeq 为已发放探测的单调序号（同一 epoch 内重叠探测按新者胜）；writeGen 为投影
// 写代计数（状态事件/成员变更/连接/断流/强制失效每次实际写入 +1）——beginProbe 记录
// 发放时写代，探测结果仅在该代未被打断时写回（探测在途期间到达的更新事件不被
// 迟到的 REST 结果覆写）；invalidated 为异常收敛的强制不可用标记（design.md:426：
// 失效经唯一 apply 落态后发布；不冻结连接代/阶段，后续状态事件/对账是新事实可合法恢复）。
type agentStatusState struct {
	mu          sync.Mutex
	sessions    map[string]opencode.SessionStatusType
	connected   bool
	epoch       uint64
	phase       agentStatusPhase
	probeSeq    uint64
	writeGen    uint64
	invalidated bool
}

func newAgentStatusState() *agentStatusState {
	return &agentStatusState{sessions: map[string]opencode.SessionStatusType{}}
}

// apply 是 agentStatus 内存态的唯一写入口（design D4）。锁内执行变更并按外部投影
// before/after 计算 typed delta；守卫不满足的写入为 no-op（返回未变化的 delta）。
// 陈旧写回防护：对账/探测结果仅在 (epoch, seq, 写代) 均匹配、仍 connected 且阶段允许
// 时写入（重叠探测的新者胜；探测在途期间有任何投影写入即作废旧结果；断流/换代的
// 旧结果一律拒绝）；断流仅使匹配连接代失效。
func (a *agentStatusState) apply(op agentStatusOp) agentStatusDelta {
	a.mu.Lock()
	defer a.mu.Unlock()
	from, fromAvail := a.projectionLocked()
	var factID uint64 // 仅 agentOpInvalidate 实际落态时分配（见该 case）
	switch op.kind {
	case agentOpStatusEvent:
		// 归属反查已在调用方完成；断流后防御性忽略。状态事件是新事实：清除强制
		// 失效标记（lock-timeout 失效 ≠ 断流，不冻结连接代，恢复可发布）。
		if a.connected {
			a.sessions[op.sessionID] = op.status
			a.invalidated = false
			a.writeGen++
		}
	case agentOpOwnedAdd:
		if _, ok := a.sessions[op.sessionID]; !ok {
			a.sessions[op.sessionID] = opencode.StatusIdle
			a.writeGen++
		}
	case agentOpOwnedRemove:
		if _, ok := a.sessions[op.sessionID]; ok {
			delete(a.sessions, op.sessionID)
			a.writeGen++
		}
	case agentOpOwnedSet:
		// 完整对账租约复验：仅在签发时的 (epoch, phase) 仍未被取代且连接存活时落态。
		// 阻塞的 OC client 获取期间换代（重连 Connect 新代 aligning）后，陈旧对账
		// 携带的预对齐成员集 MUST NOT 覆写新代（round-4 BLOCKER）。
		if a.connected && a.epoch == op.epoch && a.phase == op.phase {
			next := make(map[string]opencode.SessionStatusType, len(op.owned))
			for _, sid := range op.owned {
				if st, ok := a.sessions[sid]; ok {
					next[sid] = st // 保留既有状态
				} else {
					next[sid] = opencode.StatusIdle
				}
			}
			a.sessions = next
			a.writeGen++
		}
	case agentOpConnect:
		a.epoch++
		a.connected = true
		a.phase = agentPhaseAligning
		a.invalidated = false // 新连接代：新事实集接管
		a.writeGen++
	case agentOpAlignSuccess:
		// 屏障开放按租约代精确匹配（round-4 BLOCKER）：陈旧对账的 AlignSuccess
		// MUST NOT 把新代从 aligning 提前开放到 reconcilePending——新代的屏障只能
		// 由它自己的 align + owned 重建完成后开放。
		if a.connected && a.epoch == op.epoch && (a.phase == agentPhaseAligning || a.phase == agentPhaseReconcileBlocked) {
			a.phase = agentPhaseReconcilePending
		}
	case agentOpReconcileBlocked:
		// owned 重建失败（fail-closed）：置受阻阶段——不可探测（防陈旧成员集探测），
		// 后台 tick 须重跑完整对账（re-list）后才能进入探测。按租约代匹配（陈旧
		// 对账的失败 MUST NOT 阻塞新代）；阶段实际变化即作废在途探测结果。
		if a.connected && a.epoch == op.epoch && a.phase != agentPhaseReconcileBlocked {
			a.phase = agentPhaseReconcileBlocked
			a.writeGen++
		}
	case agentOpReconcileSuccess:
		if a.epoch != op.epoch || a.probeSeq != op.seq || a.writeGen != op.gen || !a.connected {
			break // 陈旧对账（断流/换代、被更新的重叠探测取代，或发放后有过更新的投影写入）：MUST NOT 写回
		}
		if a.phase != agentPhaseReconcilePending && a.phase != agentPhaseValid {
			break // aligning/受阻/断流冻结：不得写回
		}
		// 与 owned 集合取交集：REST 是目录级 map，共享目录下他任务 session 状态
		// 忽略；owned 但状态缺失按 idle（既有契约，agent_status.go）。完全栅栏的
		// 新鲜事实：清除强制失效标记。
		for sid := range a.sessions {
			st := opencode.StatusIdle
			if v, ok := op.statuses[sid]; ok {
				st = v
			}
			a.sessions[sid] = st
		}
		a.invalidated = false
		a.phase = agentPhaseValid
	case agentOpProbeFailure:
		// 仅模式 B 从 valid 周期探测，失败退回 reconcilePending（同 connected epoch，
		// 恢复路径唯一）；reconcilePending 失败保持不可用（投影无变化，不发布）。
		// 同按 (epoch, seq, 写代) 验收：发放后有更新写入即作废（连接存活时不因
		// 陈旧失败降级）。
		if a.epoch == op.epoch && a.probeSeq == op.seq && a.writeGen == op.gen && a.connected && a.phase == agentPhaseValid {
			a.phase = agentPhaseReconcilePending
		}
	case agentOpDisconnect:
		// 仅使回调捕获的匹配连接代失效：旧连接的延迟回调（晚于新代分配）epoch 失配
		// 为 no-op，MUST NOT 失效新代。
		if a.epoch == op.epoch {
			a.connected = false
			a.writeGen++
		}
	case agentOpInvalidate:
		// 异常收敛强制不可用（design.md:426：失效捕获即 apply——本 case 在同一锁域
		// 内读取失效前投影（delta.From 即事件 from 的唯一来源，杜绝「先捕获后 apply」
		// 的交错陈旧值）、置失效标记并分配该事实的写代号（delta.FactID，供发布点
		// per-fact claim）。不冻结连接代/阶段/成员——与断流不同，runtime 可能仍健康，
		// 后续状态事件/对账是新事实可合法恢复；epoch 栅栏防旧代延迟回调误伤新代。
		if a.epoch == op.epoch && !a.invalidated {
			a.invalidated = true
			a.writeGen++
			factID = a.writeGen
		}
	}
	to, toAvail := a.projectionLocked()
	return agentStatusDelta{From: from, To: to, Available: toAvail, Changed: from != to || fromAvail != toAvail, Epoch: a.epoch, FactID: factID}
}

// projectionLocked 返回外部可见投影：不可用（未连接/强制失效/未 valid/零 owned）→
// ("", false)；否则 (busy>retry>idle 聚合值, true)。零 owned 省略字段（空串，不是
// idle）。调用方持 a.mu。
func (a *agentStatusState) projectionLocked() (string, bool) {
	if !a.connected || a.invalidated || a.phase != agentPhaseValid || len(a.sessions) == 0 {
		return "", false
	}
	return aggregateAgentStatus(a.sessions), true
}

// aggregateAgentStatus 聚合 session 级状态：busy > retry > idle；空集合 → ""。
func aggregateAgentStatus(sessions map[string]opencode.SessionStatusType) string {
	agg := opencode.StatusIdle
	any := false
	for _, st := range sessions {
		any = true
		switch st {
		case opencode.StatusBusy:
			return string(opencode.StatusBusy)
		case opencode.StatusRetry:
			agg = opencode.StatusRetry
		}
	}
	if !any {
		return ""
	}
	return string(agg)
}

// probeCandidate 返回当前是否可发起对账/探测（只读判定；align 后首次对账与后台 30s
// 重试共用前置）。modeA 为实现期模式常量的参数化形态：模式 A 仅 reconcilePending；
// 模式 B 周期探测 valid 与 reconcilePending（design D4 模式矩阵）。aligning 一律跳过
//（align 屏障）。
func (a *agentStatusState) probeCandidate(modeA bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probeCandidateLocked(modeA)
}

// probeCandidateLocked 同 probeCandidate（调用方持 a.mu）。
func (a *agentStatusState) probeCandidateLocked(modeA bool) bool {
	if !a.connected {
		return false
	}
	switch a.phase {
	case agentPhaseReconcilePending:
		return true
	case agentPhaseValid:
		return !modeA
	default:
		return false
	}
}

// beginProbe 认领一次对账/探测：可探测判定通过后在同一锁域内分配单调递增探测序号
// 并快照当前写代。leaseEpoch 为调用方租约的连接代（完整对账租约的 epoch，或直连/
// 后台 fresh 探测的自签当前代）：认领原子要求租约代仍等于当前代——陈旧对账（写入
// 虽被 apply 守卫拒绝）MUST NOT 用先前获取的 client 在更新的代上认领探测（round-5
// BLOCKER）。结果写回（ReconcileSuccess/ProbeFailure）按 (epoch, seq, gen) 匹配验收
// ——同 epoch 重叠探测按新者胜；发放到写回之间发生过任何投影写入（状态事件/成员
// 变更/断流/强制失效）即作废（探测在途的更新事件不被迟到的 REST 结果覆写）。
func (a *agentStatusState) beginProbe(modeA bool, leaseEpoch uint64) (epoch, seq, gen uint64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.epoch != leaseEpoch || !a.probeCandidateLocked(modeA) {
		return 0, 0, 0, false
	}
	a.probeSeq++
	return a.epoch, a.probeSeq, a.writeGen, true
}

// probeLease 后台 fresh 探测分支的租约捕获（round-5）：在同一状态锁域内完成可探测
// 判定 + 当前连接代快照（与 beginFullReconcile 的判定+捕获同模式）。后续阻塞的
// taskOcClient 获取期间换代 → beginProbe 的租约代校验拒绝认领。
func (a *agentStatusState) probeLease(modeA bool) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.probeCandidateLocked(modeA) {
		return 0, false
	}
	return a.epoch, true
}

// beginFullReconcile 后台重跑完整对账的判定 + 租约捕获（同一状态锁域内原子完成，
// round-4 BLOCKER 修复）：connected 且 reconcileBlocked 才获准，返回签发时的
// (epoch, phase) 租约。租约贯穿后续阻塞的 OC client 获取与 re-list，落态点由
// apply 守卫复验；失配即陈旧重跑静默中止。
func (a *agentStatusState) beginFullReconcile() (agentStatusLease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connected && a.phase == agentPhaseReconcileBlocked {
		return agentStatusLease{epoch: a.epoch, phase: a.phase}, true
	}
	return agentStatusLease{}, false
}

// currentLease 以当前 (epoch, phase) 签发租约（直连路径：初始/重连 align 钩子在
// 自身的 align 串行域内调用——该域内 Connect 不会交错，租约即自身连接代，语义不变）。
func (a *agentStatusState) currentLease() agentStatusLease {
	a.mu.Lock()
	defer a.mu.Unlock()
	return agentStatusLease{epoch: a.epoch, phase: a.phase}
}

// currentEpoch 返回当前连接代（断流回调捕获与测试构造 Disconnect op 用）。
func (a *agentStatusState) currentEpoch() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.epoch
}

// snapshotValue 返回外部可见快照（不可用 → ""）。
func (a *agentStatusState) snapshotValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, _ := a.projectionLocked()
	return v
}

// clear 清空内存态（clearRuntime 钩子，attention clearAttention 先例）：owned 集合清空、
// 断流、阶段回 aligning、清除强制失效标记。被清理 runtime 的在途对账写回被
// connected=false 守卫拒绝。epoch/writeGen/probeSeq 保持单调（防御性，无语义影响）。
func (a *agentStatusState) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = map[string]opencode.SessionStatusType{}
	a.connected = false
	a.phase = agentPhaseAligning
	a.invalidated = false
}

// --- taskRuntime 接入（attention 先例：懒初始化 + nil 安全读） ---

func (rt *taskRuntime) ensureAgentStatusState() *agentStatusState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.agentStatus == nil {
		rt.agentStatus = newAgentStatusState()
	}
	return rt.agentStatus
}

// agentStatusStateOrNil nil 安全读：未懒初始化时钩子（状态事件/claim/delete）为 no-op。
func (rt *taskRuntime) agentStatusStateOrNil() *agentStatusState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.agentStatus
}

// applyAgentStatus nil 安全 apply：状态未初始化时返回未变化 delta。
func (rt *taskRuntime) applyAgentStatus(op agentStatusOp) agentStatusDelta {
	a := rt.agentStatusStateOrNil()
	if a == nil {
		return agentStatusDelta{}
	}
	return a.apply(op)
}

func (rt *taskRuntime) clearAgentStatus() {
	a := rt.agentStatusStateOrNil()
	if a != nil {
		a.clear()
	}
}

// invalidateAgentStatus 失效捕获即 apply（design.md:426，round-4 复评）：单次唯一
// apply 在同一锁域内读取当前投影、置强制不可用并返回 typed delta——事件的 from 与
// per-fact claim 的事实号（FactID）均只来自本 delta，MUST NOT 另行预读可见性再事后
// apply（交错下产生陈旧 from）。幂等：已失效/快照本不可见时返回未变化 delta
//（From==""，发布侧据此跳过）；发布不在本方法——由 publishRunStatusInvalidation
// 在发布点 claim 后执行。
func (rt *taskRuntime) invalidateAgentStatus() agentStatusDelta {
	a := rt.agentStatusStateOrNil()
	if a == nil {
		return agentStatusDelta{}
	}
	return rt.applyAgentStatus(agentStatusOp{kind: agentOpInvalidate, epoch: a.currentEpoch()})
}

// --- Manager：对账/探测/发布（design D4，P1.8.3/P1.8.5） ---

// reconcileAgentStatus 直连完整对账入口（SSE 首次/重连 align 成功后，drainAndRelease
// 前调用）：以当前 (epoch, phase) 自签租约（align 串行域内即自身连接代），语义与
// round-4 之前的直连路径一致。
func (m *Manager) reconcileAgentStatus(ctx context.Context, rt *taskRuntime, taskID, wtPath string, oc OCClient) {
	m.reconcileAgentStatusLeased(ctx, rt, taskID, wtPath, oc, rt.ensureAgentStatusState().currentLease())
}

// reconcileAgentStatusLeased 租约制完整对账：① 按 store 重建 owned 成员（align 插入/
// 删除经同一 apply 维护）；② 重建成功并将 owned 集合 apply 落态后才进入
// reconcilePending（align 屏障延伸覆盖 re-list 窗口：期间保持 aligning/blocked，
// 后台 tick 的 probeCandidate 恒 false，MUST NOT 带陈旧成员集探测）；③ REST
// /session/status 对账（模式 A 与模式 B 的首次探测同路径）。租约复验在 ①② 落态点
// 由 apply 守卫原子完成（OwnedSet 按 (epoch, phase)、AlignSuccess 按 epoch）：阻塞的
// client 获取/re-list 期间换代或阶段推进 → 陈旧对账的全部写入被拒（无成员写、无
// 屏障开放、探测经 beginProbe 的 aligning 判定自动跳过、无发布），静默中止——新代
// 走自己的 align 路径。① 失败 fail-closed：按租约代置 reconcileBlocked（陈旧代失败
// 不误伤新代），后台 30s tick 对受阻 runtime 重跑本完整对账。失败不影响任务生命周期。
func (m *Manager) reconcileAgentStatusLeased(ctx context.Context, rt *taskRuntime, taskID, wtPath string, oc OCClient, lease agentStatusLease) {
	a := rt.ensureAgentStatusState()
	sessions, err := m.store.ListTaskSessions(ctx, taskID)
	if err != nil {
		a.apply(agentStatusOp{kind: agentOpReconcileBlocked, epoch: lease.epoch})
		log.Printf("task %s: agent status owned rebuild after align: %v (fail-closed, blocked until full reconcile)", taskID, err)
		return
	}
	owned := make([]string, 0, len(sessions))
	for _, s := range sessions {
		owned = append(owned, s.SessionID)
	}
	m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{kind: agentOpOwnedSet, owned: owned, epoch: lease.epoch, phase: lease.phase})
	// 租约仍匹配（owned 已落态），align 屏障方可开放（aligning/blocked → reconcilePending）。
	a.apply(agentStatusOp{kind: agentOpAlignSuccess, epoch: lease.epoch})
	// 探测认领携带完整对账租约的代（round-5 BLOCKER）：写入被守卫拒绝的陈旧对账
	// 不得用先前获取的 client 在更新的代上认领探测——beginProbe 的租约代校验拒绝
	// （无探测、无发布）。
	m.probeAgentStatus(ctx, rt, taskID, wtPath, oc, agentStatusModeA, lease.epoch)
}

// probeAgentStatus 执行一次对账/探测并写回（align 后首次对账与后台 30s 重试共用）。
// modeA 为实现期模式常量的参数化形态（valid 阶段仅模式 B 周期探测）；leaseEpoch 为
// 调用方租约的连接代（完整对账租约 epoch / 直连与后台 fresh 分支的自签当前代）。
// 调用链全程携带调用方 ctx。经 beginProbe 原子认领 (epoch, seq, 写代)——含租约代
// 校验（陈旧租约无法为更新的代获得探测序号），结果按三元组验收（重叠探测新者胜；
// 探测在途的投影写入作废旧结果）。探测失败仅记日志（valid 退回经 apply 守卫，仅
// 模式 B 触达）。
func (m *Manager) probeAgentStatus(ctx context.Context, rt *taskRuntime, taskID, wtPath string, oc OCClient, modeA bool, leaseEpoch uint64) {
	a := rt.agentStatusStateOrNil()
	if a == nil {
		return
	}
	epoch, seq, gen, ok := a.beginProbe(modeA, leaseEpoch)
	if !ok {
		return
	}
	statuses, err := oc.SessionStatus(ctx, wtPath)
	if err != nil {
		log.Printf("task %s: agent status reconcile: %v", taskID, err)
		m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{kind: agentOpProbeFailure, epoch: epoch, seq: seq, gen: gen})
		return
	}
	filtered := make(map[string]opencode.SessionStatusType, len(statuses))
	for sid, st := range statuses {
		filtered[sid] = st.Type
	}
	m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{kind: agentOpReconcileSuccess, epoch: epoch, seq: seq, gen: gen, statuses: filtered})
}

// applyAgentStatusAndCommit 经唯一 apply 写入并按 delta 发布（锁外，不重读 runtime 状态）。
// 未注入 LifecycleService（迁移期 legacy 路径）或 runtime 已被替换/清理时不发布。
func (m *Manager) applyAgentStatusAndCommit(taskID string, rt *taskRuntime, op agentStatusOp) {
	delta := rt.applyAgentStatus(op)
	if !delta.Changed || m.lifecycle == nil {
		return
	}
	if cur := m.getRuntime(taskID); cur != rt {
		return
	}
	m.lifecycle.CommitRunStatusChange(taskID, string(rt.instVersion), delta.From, delta.To, delta.Available)
}

// noteAgentSessionClaimed claim 成功后的 owned 成员钩子（handleSSEEvent 与
// resolveAnchorSession 的全部 claim 生产点，P1.8.2）。
func (m *Manager) noteAgentSessionClaimed(taskID, sid string) {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return
	}
	m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{kind: agentOpOwnedAdd, sessionID: sid})
}

// noteAgentSessionDeleted session 归属行删除成功后的 owned 成员钩子（P1.8.2）。
func (m *Manager) noteAgentSessionDeleted(taskID, sid string) {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return
	}
	m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{kind: agentOpOwnedRemove, sessionID: sid})
}

// applySessionStatusEvent 将归属已确认 session 的 session.status 事件写入 agentStatus
// 内存态（P1.8.2）。modeA 为实现期模式常量的参数化形态：模式 A 解析并 apply，模式 B
// 不解析不 apply（design D4 模式执行矩阵；生产调用点恒传 agentStatusModeA，测试传
// false 验证模式 B 等价分支，MUST NOT 运行时切换）。解析失败/未知枚举静默忽略
//（fail-closed，不中断流）。
func (m *Manager) applySessionStatusEvent(taskID string, ev opencode.Event, modeA bool) {
	if !modeA {
		return
	}
	sev, ok := opencode.ParseSessionStatusEvent(ev)
	if !ok {
		return
	}
	if rt := m.getRuntime(taskID); rt != nil {
		m.applyAgentStatusAndCommit(taskID, rt, agentStatusOp{
			kind: agentOpStatusEvent, sessionID: sev.SessionID, status: sev.Status,
		})
	}
}

// handleAgentStatusDisconnect SSE 断流回调（client OnDisconnect，仅已建立连接终止触发，
// 主动 ctx 取消不触发）。校验 runtime 激活代身份（matchesRegistry，B4：旧实例回调不得
// 触碰新实例）后经唯一 apply 使回调捕获的连接代失效（epoch 匹配防旧连接延迟回调误伤
// 新代）；仅投影真实变化时发布（design D4）。
func (m *Manager) handleAgentStatusDisconnect(taskID string, rt *taskRuntime, epoch uint64) {
	cur := m.getRuntime(taskID)
	if cur == nil || !cur.matchesRegistry(rt.instVersion, runtimeSessionName(taskID)) {
		return
	}
	m.applyAgentStatusAndCommit(taskID, cur, agentStatusOp{kind: agentOpDisconnect, epoch: epoch})
}

// retryAgentStatusReconcile 后台 30s tick 分支（P1.8.3）：对 reconcilePending（模式 B
// 另含 valid）阶段的 runtime 重试对账/探测；对 reconcileBlocked（owned 重建失败）的
// runtime 重跑完整对账（re-list 重建成员后才探测，MUST NOT 带陈旧成员集探测）。
// modeA 为实现期模式常量的参数化形态（生产恒传 agentStatusModeA，测试传 false 验证
// 模式 B 等价分支）。aligning 跳过；断流/失效 epoch 由 probeCandidate 拒绝。挂入既有
// backgroundLoop，不新增 goroutine/定时器。
func (m *Manager) retryAgentStatusReconcile(ctx context.Context, modeA bool) {
	m.rtMu.Lock()
	ids := make([]string, 0, len(m.runtimes))
	for id := range m.runtimes {
		ids = append(ids, id)
	}
	m.rtMu.Unlock()

	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		rt := m.getRuntime(id)
		if rt == nil {
			continue
		}
		a := rt.agentStatusStateOrNil()
		if a == nil {
			continue
		}
		// 完整对账租约与判定在同一锁域内捕获（round-4 BLOCKER）：后续阻塞的
		// taskOcClient 获取期间换代/阶段推进时，陈旧租约的全部写入被 apply 守卫拒绝。
		// fresh 探测分支同模式捕获租约代（round-5）：阻塞的 client 获取期间换代 →
		// beginProbe 的租约代校验拒绝认领。
		lease, fullReconcile := a.beginFullReconcile()
		probeEpoch := uint64(0)
		if !fullReconcile {
			epoch, ok := a.probeLease(modeA)
			if !ok {
				continue
			}
			probeEpoch = epoch
		}
		// 对账调用链携带本 tick ctx（ctx-aware tmux 环境读取 → opencode HTTP）。
		oc, dir, ok := m.taskOcClient(ctx, id)
		if !ok {
			continue
		}
		if fullReconcile {
			m.reconcileAgentStatusLeased(ctx, rt, id, dir, oc, lease)
			continue
		}
		m.probeAgentStatus(ctx, rt, id, dir, oc, modeA, probeEpoch)
	}
}

// AgentStatusSnapshot 读 agentStatus 内存快照（design D4，P2 消费侧专用；REST
// active sessions 与 SSE 组装 helper 使用）。非 active（含 activating 窗口：状态已置
// activating 而 runtime 可能已完成对账）返回空串——与实时探测 AgentStatus 的降级语义
// 一致（agent_status.go）。不可用（无 runtime、连接代无效或零 owned）同样返回空串
//（omitempty 省略，降级语义不变）。既有 Manager.AgentStatus 实时探测语义不变
//（/projects、/tasks/{id} 消费者不在 P1.8 范围，P1.8.5）。
func (m *Manager) AgentStatusSnapshot(taskID string) string {
	row, err := m.store.GetTask(context.Background(), taskID)
	if err != nil || row.Status != StatusActive {
		return ""
	}
	rt := m.getRuntime(taskID)
	if rt == nil {
		return ""
	}
	a := rt.agentStatusStateOrNil()
	if a == nil {
		return ""
	}
	return a.snapshotValue()
}
