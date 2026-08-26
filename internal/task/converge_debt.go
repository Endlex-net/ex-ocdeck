package task

// converge_debt.go 收敛债务编排（OpenSpec change sse-active-sessions P1.4.7，
// design.md D0:151 替换行为 + D2 债务两阶段矩阵）。
//
// 锁等待超时路径 MUST NOT 无锁 cleanupActivationRuntime / 无锁 CAS（旧行为已删除）：
// 仅按触发令牌（TRIGGER，非等锁结束时刻的当前令牌）做登记前过期判定并登记两阶段债务，
// 由 backgroundLoop worker 持任务锁消化（preCleanup 先清理再 CAS；postCleanup 仅重试 CAS）。
// 债务表与过期判定在 application/runtime.Registry 的 genMu 锁域内（与 runtime 安装/
// tombstone 更新串行化）；移除一律 compare-and-delete，防旧 worker 误删新代登记。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"ocdeck/internal/application"
	"ocdeck/internal/application/runtime"
)

// convergeDebtReasons 是 worker 消化债务时的 last_error 原因（债务项不携带原始触发 reason，
// design.md D2 注册项仅含令牌+阶段）。
const (
	convergeDebtPreCleanupReason  = "converge debt: deferred runtime cleanup"
	convergeDebtPostCleanupReason = "converge debt: post-cleanup CAS retry"
)

// requestResync 发布 resync.requested 控制事件（design.md D2：提交结果不确定时保守发布，
// 订阅方全量重建兜底）。经 LifecycleService 发布（NoopPublisher 阶段调用位就绪无实际发布）。
func (m *Manager) requestResync() {
	if m.lifecycle != nil {
		m.lifecycle.RequestResync()
	}
}

// attentionVisible 判断 runtime 的 attention 快照是否外部可见：存在 pending
// permission/question 即可见（对账在途缓冲不落集合、不改变外部快照，不算可见）。
// 供异常收敛在 cleanup 前捕获可见性（design.md D2「清理前捕获外部可见状态」；
// cleanup 后快照已被 clearAttention 清空，MUST NOT 事后读取）。
func (m *Manager) attentionVisible(rt *taskRuntime) bool {
	if rt == nil {
		return false
	}
	snap := rt.attentionSnapshot()
	return len(snap.Permissions) > 0 || len(snap.Questions) > 0
}

// runStatusVisible 已删除（round-4 复评）：run_status 失效的「捕获」即唯一 apply
//（rt.invalidateAgentStatus 返回 typed delta，From/FactID 取自 op 时状态），MUST NOT
// 预读可见性再事后 apply（交错下产生陈旧 from）。

// publishAttentionInvalidation 发布一次 attention 可见失效（design.md D2 异常收敛行：
// converge 的 cleanup 经 clearRuntime→clearAttention 清空了外部可见注意力快照，MUST
// 发布 serve_runtime.attention_changed 告知订阅方失效）。仅当清理前快照可见时发布
//（「无可见字段则不发领域事件」）；RID 经触发令牌定位（该令牌实例的快照才是被失效
// 的主体）。
//
// 发布所有权在发布点原子认领（ClaimAttentionInvalidation，唯一权威）：持锁回调在
// 长清理期间可能被无需任务锁的超时回调并发认领发布——捕获时刻的查询快照此时已
// 过期（TOCTOU），claim 失败即该事实已发布，MUST NOT 二次发布。NoopPublisher 阶段
// 调用位就绪无实际发布。
func (m *Manager) publishAttentionInvalidation(taskID string, tok runtime.InstVersion, visible bool) {
	if !visible || m.lifecycle == nil {
		return
	}
	if !m.runtimeRegistry.ClaimAttentionInvalidation(taskID, tok) {
		return
	}
	m.lifecycle.CommitAttentionChange(taskID, string(tok))
}

// publishRunStatusInvalidation 发布一次 run_status 可见失效（design.md D2 异常收敛矩阵
// ②/③a/③c 与锁超时路径）。d 为调用方经失效捕获即 apply（rt.invalidateAgentStatus，
// 单锁域内读取投影并落态）得到的 typed delta：事件 from 与事实号只来自本 delta，
// MUST NOT 另行预读可见性（交错下陈旧）。From 为空即无可失效快照（已失效/不可见/
// runtime 已清理）→ 不发布不认领。发布所有权在发布点按事实原子认领
//（ClaimRunStatusInvalidation(taskID, tok, d.FactID)）：同一令牌上同一事实恰好发布
// 一次；失效→恢复→再失效为更高事实号的新事实，MUST 获准发布；claim 失败即该事实
// 已发布，跳过（落态保持幂等）。attention 的 claim 语义不受影响。
func (m *Manager) publishRunStatusInvalidation(taskID string, tok runtime.InstVersion, d agentStatusDelta) {
	if d.From == "" || m.lifecycle == nil {
		return
	}
	if !m.runtimeRegistry.ClaimRunStatusInvalidation(taskID, tok, d.FactID) {
		return
	}
	m.lifecycle.CommitRunStatusChange(taskID, string(tok), d.From, "", false)
}

// onConvergeLockTimeout 是收敛等锁超时分支（convergeToSuspendedChecked 等锁失败时调用）。
// design.md D0:151：本分支 MUST NOT cleanup / CAS，仅登记债务后返回。
func (m *Manager) onConvergeLockTimeout(taskID, reason string, tok runtime.InstVersion) {
	m.registerDebtIfCurrent(taskID, tok, reason)
}

// registerDebtIfCurrent 以触发令牌做登记前过期判定并登记收敛债务（design.md D2）：
//   - 当前 runtime 令牌 == 触发令牌 → runtime 仍在触发代 → 登记 preCleanup（worker
//     持锁清理），登记成功后发布 attention 失效 + run_status 失效 + resync；
//   - runtime 为 nil 且 tombstone == 触发令牌 → cleanup 已在等锁期间完成（快照
//     已随之清空，无可失效快照）→ 登记 postCleanup（仅剩 CAS），登记成功后仅发布 resync；
//   - 其余 → 旧代 stale callback → 记日志返回（不登记、不清理、不发布）。
//
// run_status 失效在登记成功后经「捕获即 apply」落态（invalidateAgentStatus 的 delta
// 携带 from/事实号，快照立即 "" 与事件一致；登记被拒时不失效不发布，避免
// publish-then-reject）。本方法不持任务锁（等锁已失败）。
func (m *Manager) registerDebtIfCurrent(taskID string, trigger runtime.InstVersion, reason string) {
	var current *runtime.InstVersion
	var invalidRT *taskRuntime
	attentionVisible := false
	if rt := m.getRuntime(taskID); rt != nil {
		cur := rt.instVersion
		current = &cur
		if cur == trigger {
			// 触发令牌仍为当前代：cleanup 尚未发生，runtime 待 worker 清理。
			invalidRT = rt
			attentionVisible = m.attentionVisible(rt)
		}
	}
	var phase runtime.DebtPhase
	switch {
	case current != nil && *current == trigger:
		phase = runtime.DebtPhasePreCleanup
	case current == nil && m.tombstoneIs(taskID, trigger):
		phase = runtime.DebtPhasePostCleanup
	default:
		log.Printf("convergeToSuspended: lock wait timed out (task %s instVersion=%s): stale trigger token, no debt registered; reason=%s",
			taskID, trigger, reason)
		return
	}
	// 登记时记录失效发布状态（供 worker 捕获预判与测试断言）；发布决策走发布点的
	// 原子 claim（publishAttentionInvalidation → ClaimAttentionInvalidation）。
	registered, actual := m.runtimeRegistry.RegisterIfCurrent(taskID, trigger, phase, current, attentionVisible)
	if !registered {
		log.Printf("converge debt: register skipped (task %s instVersion=%s phase=%d): stale trigger; existing debt phase=%d",
			taskID, trigger, phase, actual)
		return
	}
	var runStatusInvalidation agentStatusDelta
	if invalidRT != nil {
		runStatusInvalidation = invalidRT.invalidateAgentStatus()
	}
	m.publishAttentionInvalidation(taskID, trigger, attentionVisible)
	m.publishRunStatusInvalidation(taskID, trigger, runStatusInvalidation)
	m.requestResync()
}

// registerConvergeDebt 登记收敛债务（CAS 矩阵 ②a/②c/③a/③c 与推进缺失重登记路径）。
// 调用时机均为 cleanup 之后：runtime 已清，currentRuntime=nil，过期判定由 tombstone
// 匹配承载（design.md D2 postCleanup 语义）。矩阵分支在登记前已过发布点（本轮 claim
// 成功已发布，或 claim 失败即他人已发布），attentionInvalidated 置 true。
func (m *Manager) registerConvergeDebt(taskID string, tok runtime.InstVersion, phase runtime.DebtPhase) {
	registered, actual := m.runtimeRegistry.RegisterIfCurrent(taskID, tok, phase, nil, true)
	if !registered {
		log.Printf("converge debt: register skipped (task %s instVersion=%s phase=%d): stale trigger; existing debt phase=%d",
			taskID, tok, phase, actual)
	}
}

// tombstoneIs 判断任务 tombstone 是否等于给定令牌（无 tombstone 视为不等）。
func (m *Manager) tombstoneIs(taskID string, tok runtime.InstVersion) bool {
	tomb, ok := m.runtimeRegistry.Tombstone(taskID)
	return ok && tomb == tok
}

// deleteConvergeDebt 撤销任务的收敛债务登记（任务离开 active，design.md D2）。
// best-effort：按当前登记项令牌 compare-and-delete（不盲删，防误删新代登记）。
func (m *Manager) deleteConvergeDebt(taskID string) {
	if entry, ok := m.runtimeRegistry.Get(taskID); ok {
		m.runtimeRegistry.CompareAndDelete(taskID, entry.Token)
	}
}

// convergeCommitCAS 持锁收敛的提交段：清 env 快照 + last_error 聚合 + active→suspended CAS
// + D2 嵌套决策表。converge 持锁主路径与债务 worker 共用；调用方持任务锁且清理已完成
//（或 postCleanup 无需清理）。attentionVisible 为调用方在 cleanup 前捕获的外部可见
// attention 快照存在性；runStatusInvalidation 为 cleanup 前「捕获即 apply」得到的
// run_status 失效 delta（From/FactID 取自 apply 时状态；无可失效快照时为零值）。
// 两者在 cleanup 后快照已清空，不可事后读取；worker postCleanup/nil-runtime 分支传
// 零值（该令牌的失效已在超时登记或前轮 preCleanup 发布过）。
func (m *Manager) convergeCommitCAS(ctx context.Context, taskID, reason string, tok runtime.InstVersion, attentionVisible bool, runStatusInvalidation agentStatusDelta, cleanupErr error) {
	_, envErr := m.writeEnvSnapshot(ctx, taskID, sql.NullString{})
	le := sql.NullString{String: reason, Valid: true}
	if cleanupErr != nil {
		le = sql.NullString{String: fmt.Sprintf("%s; cleanup notice: %v", reason, cleanupErr), Valid: true}
	}
	if envErr != nil {
		le = sql.NullString{String: fmt.Sprintf("%s; clear env snapshot: %v", le.String, envErr), Valid: true}
	}
	committed, statusErr := m.writeStatusConditional(ctx, taskID, StatusActive, StatusSuspended, le)
	if statusErr != nil {
		log.Printf("convergeToSuspended: commit suspended failed (task %s): %v; last_error=%s", taskID, statusErr, le.String)
	}
	m.applyConvergeCASMatrix(taskID, tok, attentionVisible, runStatusInvalidation, committed.Matched, statusErr)
}

// applyConvergeCASMatrix 落实异常收敛 CAS 后的 D2 嵌套决策表（design.md D2 持锁主路径）。
// 本方法运行时清理已完成（runtime 已清、tombstone 仍为触发令牌），仅按 CAS 结果分叉：
//   ① committed（Matched，active→suspended 真实迁移）→ task.status_changed 已由
//      writeStatusConditional 的 commit helper 发布；任务离开 active → compare-and-delete
//      同令牌债务（若存在）；不发 attention/run_status 失效（迁移事件承载任务级失效）；
//   ② CAS error → 保守发布一次 attention/run_status 可见失效 + resync.requested（发布
//      先于重读分叉，覆盖 ②a/②b/②c，design.md D2「②CAS error → 保守发布实际已发生的
//      可见失效…然后按状态重读结果分叉」）；重读仍 active 或读错误（②a/②c）→ 登记
//      postCleanup；重读非 active/缺失（②b）→ compare-and-delete，不登记；
//   ③ !Matched（并发已转走，无错误）→ 重读分叉：③a 仍 active → 发布一次 attention/
//      run_status 可见失效 + 登记 postCleanup；③b 非 active/缺失 → 不发布，
//      compare-and-delete；③c 读错误 → 同 ②c（可见失效 + resync + 登记 postCleanup）。
func (m *Manager) applyConvergeCASMatrix(taskID string, tok runtime.InstVersion, attentionVisible bool, runStatusInvalidation agentStatusDelta, committed bool, statusErr error) {
	switch {
	case statusErr == nil && committed:
		// ① 任务已离开 active。
		m.runtimeRegistry.CompareAndDelete(taskID, tok)
	case statusErr != nil:
		// ② 提交结果不确定：保守发布 attention/run_status 可见失效 + resync.requested。
		m.publishAttentionInvalidation(taskID, tok, attentionVisible)
		m.publishRunStatusInvalidation(taskID, tok, runStatusInvalidation)
		m.requestResync()
		if active, known := m.convergeRereadTask(taskID); !known || active {
			// ②a/②c：重读仍 active 或读错误 → 登记 postCleanup 由 worker 重试。
			m.registerConvergeDebt(taskID, tok, runtime.DebtPhasePostCleanup)
		} else {
			// ②b：重读非 active/缺失 → 任务已离开 active（并发转换已收敛），
			// compare-and-delete 债务，不登记。
			m.runtimeRegistry.CompareAndDelete(taskID, tok)
		}
	default:
		active, known := m.convergeRereadTask(taskID)
		switch {
		case !known:
			// ③c 重读错误：保守发布可见失效 + 登记 + resync（同 ②c）。
			m.publishAttentionInvalidation(taskID, tok, attentionVisible)
			m.publishRunStatusInvalidation(taskID, tok, runStatusInvalidation)
			m.requestResync()
			m.registerConvergeDebt(taskID, tok, runtime.DebtPhasePostCleanup)
		case active:
			// ③a 仍 active（并发窗口）：发布 attention/run_status 可见失效 + 登记
			// postCleanup 由 worker 重试。
			m.publishAttentionInvalidation(taskID, tok, attentionVisible)
			m.publishRunStatusInvalidation(taskID, tok, runStatusInvalidation)
			m.registerConvergeDebt(taskID, tok, runtime.DebtPhasePostCleanup)
		default:
			// ③b 非 active/缺失：该并发转换由其对应提交点发布，不发布不登记；
			// 任务已离开 active → compare-and-delete 债务（W③b）。
			m.runtimeRegistry.CompareAndDelete(taskID, tok)
			log.Printf("convergeToSuspended: task %s no longer active (concurrent transition)", taskID)
		}
	}
}

// convergeRereadTask 重读任务状态（CAS 后 ②/③ 分叉的重读判定）。
// 返回 (active, known)：known=false 表示读取失败（②c/③c 保守处理）；任务不存在视为已知
// 非 active（已删除，由其对应提交点负责发布）。优先经 lifecycle.Get（ErrTaskNotFound
// sentinel 可区分缺失与读错误）；未注入时经 store 直读（sql.ErrNoRows 视为缺失）。
func (m *Manager) convergeRereadTask(taskID string) (active bool, known bool) {
	if m.lifecycle != nil {
		snap, err := m.lifecycle.Get(context.Background(), taskID)
		if err != nil {
			if errors.Is(err, application.ErrTaskNotFound) {
				return false, true
			}
			return false, false
		}
		return taskSnapshotToTaskRow(snap).Status == StatusActive, true
	}
	row, err := m.store.GetTask(context.Background(), taskID)
	if err != nil {
		// 缺失（sql.ErrNoRows / mockStore "not found"）视为已知非 active；其余读错误 unknown。
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "not found") {
			return false, true
		}
		return false, false
	}
	return row.Status == StatusActive, true
}

// processConvergeDebts 消化收敛债务（backgroundLoop tick 分支，design.md D2 债务两阶段）。
// 逐项 tryLockTask（非阻塞）：锁忙跳过本轮（不得让单任务阻塞 30s 周期）；
// 持锁后重新校验令牌/状态/tombstone，preCleanup 先清理再 CAS，postCleanup 仅重试 CAS。
func (m *Manager) processConvergeDebts(ctx context.Context) {
	for _, entry := range m.runtimeRegistry.Snapshot() {
		unlock, err := m.tryLockTask(entry.TaskID)
		if err != nil {
			// 任务忙（用户操作在执行）：跳过本轮，下个周期重试。
			continue
		}
		m.processConvergeDebtLocked(ctx, entry)
		unlock()
	}
}

// processConvergeDebtLocked 处理单条债务（调用方持任务锁；design.md D2 债务两阶段）。
// 前置校验失败 → compare-and-delete（仅令牌仍匹配时）撤销登记，不清理。
func (m *Manager) processConvergeDebtLocked(ctx context.Context, entry runtime.DebtEntry) {
	// ① 注册表令牌仍等于快照令牌（等锁期间登记被改动则放弃本次快照）。
	cur, ok := m.runtimeRegistry.Get(entry.TaskID)
	if !ok || cur != entry {
		return
	}
	// ② 任务仍 active；读失败无法判定 → 保守跳过本轮（不删不清理）。
	row, rerr := m.store.GetTask(ctx, entry.TaskID)
	if rerr != nil {
		return
	}
	if row.Status != StatusActive {
		// 任务已离开 active：债务已过时（登记侧撤销错过），compare-and-delete 撤销。
		m.runtimeRegistry.CompareAndDelete(entry.TaskID, entry.Token)
		return
	}
	// ④ tombstone 等于债务令牌：已换代 → 旧代债务 compare-and-delete。
	if !m.tombstoneIs(entry.TaskID, entry.Token) {
		m.runtimeRegistry.CompareAndDelete(entry.TaskID, entry.Token)
		return
	}

	switch cur.Phase {
	case runtime.DebtPhasePreCleanup:
		if rt := m.getRuntime(entry.TaskID); rt != nil {
			// ③ 当前 runtime 令牌 == 债务令牌（runtime 允许非空：worker 负责清理）。
			if rt.instVersion != entry.Token {
				m.runtimeRegistry.CompareAndDelete(entry.TaskID, entry.Token)
				return
			}
			// 清理前捕获 attention 可见性（worker 持锁执行同一矩阵，design.md D2）；
			// attention 登记已标记失效发布过（超时路径已发布）则不再计可见，防同一
			// 事实二次发布。run_status 失效为「捕获即 apply」（design.md:426）：
			// delta 携带 op 时投影与事实号——超时路径已失效且未恢复时为幂等零
			// delta（不发布）；超时失效后经状态事件恢复的快照在此产生更高事实号的
			// 新事实（claim 获准，不被旧 marker 抑制）。持锁清理（worker 唯一
			// cleanup 点）→ 原子推进 postCleanup → 持锁 CAS 矩阵。
			attentionVisible := m.attentionVisible(rt) && !entry.AttentionInvalidated
			runStatusInvalidation := rt.invalidateAgentStatus()
			cleanupErr := m.cleanupActivationRuntime(ctx, entry.TaskID)
			m.advanceDebtAndRunCAS(ctx, entry, attentionVisible, runStatusInvalidation, cleanupErr)
			return
		}
		// runtime 已清且 tombstone 仍为债务令牌：design.md D2「runtime 允许非空」指
		// preCleanup 允许存活 runtime（worker 负责清理），nil 并非禁止——nil + tombstone
		// 匹配即 cleanup 已在等锁期间被他人完成（与超时登记 nil→postCleanup 同语义，
		// 该令牌的失效已在超时登记时发布）→ 推进 postCleanup 仅重试 CAS。
		m.advanceDebtAndRunCAS(ctx, entry, false, agentStatusDelta{}, nil)
	case runtime.DebtPhasePostCleanup:
		// ③ runtime 必须已清。tombstone 已校验等于债务令牌，runtime 非空意味着同令牌
		// runtime 被重装（不可达：每令牌只分配一次）——保守跳过本轮，不清理不删除。
		if m.getRuntime(entry.TaskID) != nil {
			log.Printf("converge debt: postCleanup with live runtime (task %s); skip this tick", entry.TaskID)
			return
		}
		// 仅重试 CAS（env 清 + writeStatusConditional + ①/②/③）。可见失效已在该
		// 令牌的超时登记或前轮 preCleanup 发布过，本轮无可失效快照（零 delta）。
		m.convergeCommitCAS(ctx, entry.TaskID, convergeDebtPostCleanupReason, entry.Token, false, agentStatusDelta{}, nil)
	}
}

// advanceDebtAndRunCAS 将债务原子推进 postCleanup 后执行持锁 CAS 收敛（preCleanup 清理
// 完成后调用）。attentionVisible 为清理前捕获的外部可见 attention 快照存在性，
// runStatusInvalidation 为清理前「捕获即 apply」的 run_status 失效 delta，随矩阵穿透。
// 推进失败：令牌已换（新代）→ 直接退出不删除；记录缺失 → 任务仍 active 且 runtime 已空
// → 重新登记 postCleanup（design.md D2）。
func (m *Manager) advanceDebtAndRunCAS(ctx context.Context, entry runtime.DebtEntry, attentionVisible bool, runStatusInvalidation agentStatusDelta, cleanupErr error) {
	ok, _, tokenMoved := m.runtimeRegistry.AdvanceToPostCleanup(entry.TaskID, entry.Token)
	if !ok {
		if tokenMoved {
			log.Printf("converge debt: advance skipped (task %s): token moved to newer instance", entry.TaskID)
			return
		}
		m.registerConvergeDebt(entry.TaskID, entry.Token, runtime.DebtPhasePostCleanup)
		return
	}
	m.convergeCommitCAS(ctx, entry.TaskID, convergeDebtPreCleanupReason, entry.Token, attentionVisible, runStatusInvalidation, cleanupErr)
}
