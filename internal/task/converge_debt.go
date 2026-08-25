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

// onConvergeLockTimeout 是收敛等锁超时分支（convergeToSuspendedChecked 等锁失败时调用）。
// design.md D0:151：本分支 MUST NOT cleanup / CAS，仅登记债务后返回。
func (m *Manager) onConvergeLockTimeout(taskID, reason string, tok runtime.InstVersion) {
	m.registerDebtIfCurrent(taskID, tok, reason)
}

// registerDebtIfCurrent 以触发令牌做登记前过期判定并登记收敛债务（design.md D2）：
//   - 当前 runtime 令牌 == 触发令牌 → runtime 仍在触发代 → 登记 preCleanup（worker 持锁清理）；
//   - runtime 为 nil 且 tombstone == 触发令牌 → cleanup 已在等锁期间完成 → 登记 postCleanup（仅剩 CAS）；
//   - 其余 → 旧代 stale callback → 记日志返回（不登记、不清理、不 CAS）。
//
// 锁外粗判只选阶段；权威的过期重校验 + 登记是 RegisterIfCurrent 在 genMu 内的一个原子
// 事务（tombstone 为代际权威；新代触发令牌原子替换旧债），MUST 传入分类所用的同一
// runtime 快照指针，不得丢弃存活校验。登记成功才发布 resync.requested（避免
// publish-then-reject；NoopPublisher 阶段调用位就绪无实际发布）。本方法不持任务锁
//（等锁已失败）。
func (m *Manager) registerDebtIfCurrent(taskID string, trigger runtime.InstVersion, reason string) {
	var current *runtime.InstVersion
	if rt := m.getRuntime(taskID); rt != nil {
		cur := rt.instVersion
		current = &cur
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
	registered, actual := m.runtimeRegistry.RegisterIfCurrent(taskID, trigger, phase, current)
	if !registered {
		log.Printf("converge debt: register skipped (task %s instVersion=%s phase=%d): stale trigger; existing debt phase=%d",
			taskID, trigger, phase, actual)
		return
	}
	m.requestResync()
}

// registerConvergeDebt 登记收敛债务（CAS 矩阵 ②a/②c/③a/③c 与推进缺失重登记路径）。
// 调用时机均为 cleanup 之后：runtime 已清，currentRuntime=nil，过期判定由 tombstone
// 匹配承载（design.md D2 postCleanup 语义）。
func (m *Manager) registerConvergeDebt(taskID string, tok runtime.InstVersion, phase runtime.DebtPhase) {
	registered, actual := m.runtimeRegistry.RegisterIfCurrent(taskID, tok, phase, nil)
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
//（或 postCleanup 无需清理）。
func (m *Manager) convergeCommitCAS(ctx context.Context, taskID, reason string, tok runtime.InstVersion, cleanupErr error) {
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
	m.applyConvergeCASMatrix(taskID, tok, committed.Matched, statusErr)
}

// applyConvergeCASMatrix 落实异常收敛 CAS 后的 D2 嵌套决策表（design.md D2 持锁主路径）。
// 本方法运行时清理已完成（runtime 已清、tombstone 仍为触发令牌），仅按 CAS 结果分叉：
//   ① committed（Matched，active→suspended 真实迁移）→ task.status_changed 已由
//      writeStatusConditional 的 commit helper 发布；任务离开 active → compare-and-delete
//      同令牌债务（若存在）；
//   ② CAS error → 发布一次 resync.requested；重读仍 active 或读错误（②a/②c）→ 登记
//      postCleanup；重读非 active/缺失（②b）→ compare-and-delete，不登记；
//   ③ !Matched（并发已转走，无错误）→ 重读分叉：③a 仍 active → 登记 postCleanup；
//      ③b 非 active/缺失 → 不发布，compare-and-delete；③c 读错误 → 同 ②c（登记
//      postCleanup + resync）。
func (m *Manager) applyConvergeCASMatrix(taskID string, tok runtime.InstVersion, committed bool, statusErr error) {
	switch {
	case statusErr == nil && committed:
		// ① 任务已离开 active。
		m.runtimeRegistry.CompareAndDelete(taskID, tok)
	case statusErr != nil:
		// ② 提交结果不确定：保守发布 resync.requested。
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
			// ③c 重读错误：保守登记 + resync（同 ②c）。
			m.requestResync()
			m.registerConvergeDebt(taskID, tok, runtime.DebtPhasePostCleanup)
		case active:
			// ③a 仍 active（并发窗口）：登记 postCleanup 由 worker 重试。
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
			// 持锁清理（worker 唯一 cleanup 点）→ 原子推进 postCleanup → 持锁 CAS 矩阵。
			cleanupErr := m.cleanupActivationRuntime(ctx, entry.TaskID)
			m.advanceDebtAndRunCAS(ctx, entry, cleanupErr)
			return
		}
		// runtime 已清且 tombstone 仍为债务令牌：design.md D2「runtime 允许非空」指
		// preCleanup 允许存活 runtime（worker 负责清理），nil 并非禁止——nil + tombstone
		// 匹配即 cleanup 已在等锁期间被他人完成（与超时登记 nil→postCleanup 同语义）
		// → 推进 postCleanup 仅重试 CAS。
		m.advanceDebtAndRunCAS(ctx, entry, nil)
	case runtime.DebtPhasePostCleanup:
		// ③ runtime 必须已清。tombstone 已校验等于债务令牌，runtime 非空意味着同令牌
		// runtime 被重装（不可达：每令牌只分配一次）——保守跳过本轮，不清理不删除。
		if m.getRuntime(entry.TaskID) != nil {
			log.Printf("converge debt: postCleanup with live runtime (task %s); skip this tick", entry.TaskID)
			return
		}
		// 仅重试 CAS（env 清 + writeStatusConditional + ①/②/③）。
		m.convergeCommitCAS(ctx, entry.TaskID, convergeDebtPostCleanupReason, entry.Token, nil)
	}
}

// advanceDebtAndRunCAS 将债务原子推进 postCleanup 后执行持锁 CAS 收敛（preCleanup 清理
// 完成后调用）。推进失败：令牌已换（新代）→ 直接退出不删除；记录缺失 → 任务仍 active 且
// runtime 已空 → 重新登记 postCleanup（design.md D2）。
func (m *Manager) advanceDebtAndRunCAS(ctx context.Context, entry runtime.DebtEntry, cleanupErr error) {
	ok, _, tokenMoved := m.runtimeRegistry.AdvanceToPostCleanup(entry.TaskID, entry.Token)
	if !ok {
		if tokenMoved {
			log.Printf("converge debt: advance skipped (task %s): token moved to newer instance", entry.TaskID)
			return
		}
		m.registerConvergeDebt(entry.TaskID, entry.Token, runtime.DebtPhasePostCleanup)
		return
	}
	m.convergeCommitCAS(ctx, entry.TaskID, convergeDebtPreCleanupReason, entry.Token, cleanupErr)
}
