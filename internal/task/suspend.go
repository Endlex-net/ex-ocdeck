package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

// Suspend 挂起任务（design.md §19 Suspend 行 + §5 互斥决策树三分支）。
// 前置：状态须为 active。置 suspending → 停 SSE → KillSession 全部会话 → 按决策树收敛。
//
// add-plain-dir-project D8：入口在状态转换/运行时副作用前解析并校验项目 kind（未知值零副作用报错），
// 解析后的对齐模式传入 repair 路径（tryRepairRuntime 复用，不重复查询）。
func (m *Manager) Suspend(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// guard 委托 domain/task.CanSuspend（design D0 P1.4.2 strangler 第二步）。
	// 委托前后行为 byte-equivalent：guard 拒绝时仍按现状错误模板生成 invalid_state。
	if !rehydrateGuardView(row).CanSuspend() {
		return newOpErr(codeInvalidState, fmt.Errorf("suspend requires active, got %s", row.Status))
	}
	// D8 + add-local-path-task-mode D2/D5：状态转换前解析任务有效模式（任何副作用前 fail-closed）。
	// 非法持久化 kind/mode 组合 → internal（D1）。
	proj, perr := m.store.GetProject(ctx, row.ProjectID)
	if perr != nil {
		return newOpErr(codeNotFound, fmt.Errorf("project gone: %w", perr))
	}
	mode, kerr := resolveAlignMode(row, proj.Kind)
	if kerr != nil {
		return newOpErr(codeInternal, kerr)
	}
	// C7 残余（review attempt 2）：挂起准入——首次状态/进程副作用前按任务 provider
	// 做 registry 准入（specs「Provider registry 分派」：未注册 fail-closed 零副作用，
	// 诊断性 last_error 允许）。准入失败：任务保持 active，零 kill 零 suspending。
	// mutation 证据（已验证 → 红）：准入旁路（`if false &&`）→ TestC7_Suspend
	// UnknownProviderZeroSideEffects 红（suspending 提交逃逸——status 偏离 active）。
	if aerr := m.registryAdmitTask(ctx, taskID); aerr != nil {
		if isProviderNotFound(aerr) {
			m.recordAdmissionError(ctx, taskID, aerr)
			return newOpErr(codeInternal, aerr)
		}
		return newOpErr(codeInternal, aerr)
	}
	// task-info-editable D2 仲裁表：Suspend 在 active→suspending 状态流转前先执行 R1 收敛
	//（防止清快照后恢复补提交复活快照）。不可收敛 → conflict 拒绝挂起（状态与快照不变）。
	if cerr := m.convergePendingBeforeLifecycle(ctx, row, proj); cerr != nil {
		return newOpErr(codeConflict, cerr)
	}
	// 评审 I2（D4 挂起行）：v2 挂起门禁——active→suspending CAS 前完成归属校验与
	// 停止（interrupt 确认；停止确认前 MUST 保留运行关联）。失败 → 任务保持 active
	// + last_error（零状态写入；禁入 v1 强杀补偿/serve 修复树）。
	sb, hasV2, gerr := m.suspendGateV2(ctx, taskID, row)
	if gerr != nil {
		return gerr
	}
	// P1.4.7：DB 写入经 write* helper 路由（注入 LifecycleService 时走 persist+commit 封装）。
	// terminal-file-paste-drop 2.5：active→suspending 提交（离开 active）在 per-task
	// 协调锁临界区内执行（本方法已持任务锁，锁顺序：任务锁 → 协调锁），与上传
	// finalize/投递临界区互斥——挂起提交先行时 finalize 复查必见非 active、不发布。
	var updated application.TransitionResult
	if err := m.inUploadCoordination(ctx, taskID, func() error {
		var werr error
		updated, werr = m.writeStatusConditional(ctx, taskID, StatusActive, StatusSuspending, sql.NullString{})
		return werr
	}); err != nil {
		return newOpErr(codeInternal, err)
	}
	if !updated.Matched {
		return newOpErr(codeConflict, fmt.Errorf("task %s state changed before suspend", taskID))
	}
	return m.suspendRun(ctx, taskID, mode, sb, hasV2)
}

// suspendGateV2 是 v2 挂起门禁（评审 I2，D4 挂起行）：active→suspending CAS 前
// 读实际 binding → 归属校验（supervisorForTaskConsumption 统一入口）→ 按实际
// binding 分派 supervisor 停止执行（Release=interrupt；停止确认成功才放行后续
// 退订/清理）。非 v2 binding（无 binding/v1）返回 hasV2=false 走既有流程。
// 停止失败：任务保持 active、保留运行关联、last_error，零状态转换（禁止进 v1
// forceKillAll+finishSuspend 补偿——对共享 server 禁用）。
func (m *Manager) suspendGateV2(ctx context.Context, taskID string, row TaskRow) (domain.TaskBinding, bool, error) {
	b, has, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return domain.TaskBinding{}, false, newOpErr(codeInternal, fmt.Errorf("suspend gate: get binding: %w", err))
	}
	if !has || b.ProviderID != store.ProviderIDV2 {
		return domain.TaskBinding{}, false, nil
	}
	sup, serr := m.supervisorForTaskConsumption(ctx, taskID, b, row.WorktreePath)
	if serr != nil {
		m.recordAdmissionError(ctx, taskID, serr)
		return domain.TaskBinding{}, false, newOpErr(codeConflict, serr)
	}
	if rerr := sup.Release(ctx, b); rerr != nil {
		// 停止失败如实报错：状态/运行关联/快照全部不动（active 保留）。
		le := sql.NullString{String: fmt.Sprintf("suspend stop failed (kept active): %v", rerr), Valid: true}
		if _, werr := m.writeStatus(ctx, taskID, m.readStatusForDiagnostic(ctx, taskID), le); werr != nil {
			log.Printf("suspend gate: task %s record last_error: %v", taskID, werr)
		}
		return domain.TaskBinding{}, false, newOpErr(codeProcessError, fmt.Errorf("suspend stop: %w", rerr))
	}
	return b, true, nil
}

// suspendRun 执行挂起的外部副作用并按互斥决策树收敛（design.md §5）。
// 决策树按序判定取首个命中；分支判定以 kill 前 serve 是否存活为准（B7 时机）：
//
//	a) kill 前 serve 已死 → 继续完成剩余清理 → suspended（个别失败记 notice + 后台重试）；
//	b) kill 前 serve 存活且全部 kill 成功 → suspended；
//	c) kill 前 serve 存活但有 kill 失败 → 尝试修复运行时（恢复完整运行时才算成功）→
//	   修复成功回 active + last_error；修复失败或期间 serve 死亡 → 转分支 a。
//
// 不变量：Suspend 已提交 suspending（Suspend 入口），本函数任意失败路径 MUST 收敛到
// active 或 suspended，不得停留 suspending（Retry 不接受 suspending，只能重启 reconcile，
// design.md §5）。infra 错误（HasSession/ListSessions）MUST 经 forceKillAll + finishSuspend
// 落 suspended + last_error，不得直接返回 error 留 suspending。
//
// mode 为 Suspend 入口已解析的对齐模式，传入 repair 路径复用（D8：不重复查询项目 kind）。
func (m *Manager) suspendRun(ctx context.Context, taskID string, mode AlignMode, sb domain.TaskBinding, hasV2 bool) error {
	// 停 SSE 订阅 + 退出监视。
	m.clearRuntime(taskID)

	// 评审 I2：v2 挂起收敛（停止已在 Suspend 门禁确认——先停止后退订，D3 顺序）。
	// 本地清理仅限任务自有 tmux 单元（v2 TUI/shell；无 v1 serve/runtime 单元判定，
	// 禁入 forceKillAll 强杀树与 tryRepairRuntime 修复树——共享 server 与 v1 单元
	// 语义均不适用）。清理失败仍收敛 suspended + notice（D4：执行已停止属实，
	// 清理遗留按 cleanup notice 后台重试；MUST NOT 停留 suspending）。
	if hasV2 {
		killRes := m.killTaskSessions(ctx, taskID, []string{sb.TerminalRef})
		shellNames, serr := m.listShellSessions(taskID)
		if serr != nil {
			killRes = append(killRes, killResultEntry{name: sb.TerminalRef, killErr: serr})
			return m.finalizeSuspendDetached(ctx, taskID, killRes, nil)
		}
		killRes = append(killRes, m.killTaskSessions(ctx, taskID, shellNames)...)
		return m.finalizeSuspendDetached(ctx, taskID, killRes, nil)
	}

	// 分支判定时机：kill 前查进程会话是否已死。Phase 2 进程名为 -runtime，
	// leftover -serve 仍参与判定与清理。
	processName := runtimeSessionName(taskID)
	legacyServe := serveSessionName(taskID)
	tuiName := tuiSessionName(taskID)
	serveAliveBeforeKill, herr := m.proc.HasSession(processName)
	if herr == nil && !serveAliveBeforeKill {
		if alive, lerr := m.proc.HasSession(legacyServe); lerr == nil && alive {
			serveAliveBeforeKill = true
			processName = legacyServe
		} else if lerr != nil && !errors.Is(lerr, process.ErrNoTmuxServer) {
			herr = lerr
		}
	}
	// B5e：HasSession 基础设施错误（非 ErrNoTmuxServer）MUST 收敛到 suspended，不得直接返回留 suspending。
	// 保守视为 serve 状态不可判定 → 强制 kill 残余 + finishSuspend 落 suspended + last_error。
	// infra 错误作为 killResultEntry（killErr）传入 finishSuspend，使 last_error 含 infra 上下文
	//（finishSund 会提交 suspended + 记 notice + 以 firstErr 作 last_error）。
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		base := []killResultEntry{{name: processName, killErr: herr}}
		return newOpErr(codeProcessError, m.cleanupAndFinalizeSuspendDetached(ctx, taskID, base, []string{tuiName, processName, legacyServe}, nil))
	}

	// KillSession 全部会话：tui → shells → serve（design.md §19；serve 最后杀，
	// 清理期间继续捕获 session，各自先 reaper 快照）。
	killRes := m.killTaskSessions(ctx, taskID, []string{tuiName})
	// shell 会话：枚举存在的 shell-<n>。B7b：ListSessions infra 错误 MUST 收敛到 suspended
	//（infra 错误作为 killResultEntry 传入 finishSund，使 last_error 含 infra 上下文）。
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		killRes = append(killRes, killResultEntry{name: processName, killErr: err})
		return newOpErr(codeProcessError, m.cleanupAndFinalizeSuspendDetached(ctx, taskID, killRes, []string{tuiName, processName, legacyServe}, nil))
	}
	killRes = append(killRes, m.killTaskSessions(ctx, taskID, shellNames)...)
	killRes = append(killRes, m.killTaskSessions(ctx, taskID, []string{processName, legacyServe})...)

	// 分支 a：kill 前 serve 已死 → 继续完成剩余清理 → suspended（detached finalize，F3）。
	if !serveAliveBeforeKill {
		return m.finalizeSuspendDetached(ctx, taskID, killRes, nil)
	}

	// 分支 b/c：kill 前 serve 存活。
	// 判定是否有 kill 失败（B7：以 kill 结果为准，非 kill 后 HasSession）。
	hasFailure := hasKillFailure(killRes)
	if !hasFailure {
		// 分支 b：全部成功 → suspended（detached finalize，F3）。
		return m.finalizeSuspendDetached(ctx, taskID, killRes, nil)
	}

	// 分支 c：serve 存活但有 kill 失败 → 尝试修复运行时（恢复完整运行时，B7）。
	fixed, ferr := m.tryRepairRuntime(ctx, taskID, mode)
	if ferr == nil && fixed {
		// 修复成功 → 回 active + last_error（design.md §5）。
		le := sql.NullString{String: "suspend partially failed, runtime repaired", Valid: true}
		cas, werr := m.writeStatusConditional(ctx, taskID, StatusSuspending, StatusActive, le)
		if werr != nil {
			// F3：写失败必须走补偿清理（本文件收敛约束：任意失败路径收敛到 active 或
			// suspended，不得留修复出的 runtime 悬挂在 suspending）。集中 helper：
			// detached cleanup（独立预算）+ detached finalize（独立预算），不被请求取消打断。
			// F16/F20：werr 经 cause 并入 last_error 与返回值（MUST NOT 伪造 killResultEntry）。
			return newOpErr(codeInternal, m.cleanupAndFinalizeSuspendDetached(ctx, taskID, killRes,
				[]string{tuiName, processName, legacyServe}, fmt.Errorf("restore active commit: %w", werr)))
		}
		if !cas.Matched {
			// F3：复读状态——已是 active（另一路径已提交，runtime 可用）→ 幂等成功并启动
			// 调度器；否则清理本次修复的 runtime 并收敛挂起（不得静默返回成功）。
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			fresh, rerr := m.store.GetTask(cctx, taskID)
			ccancel()
			if rerr == nil && fresh.Status == StatusActive && m.getRuntime(taskID) != nil {
				m.StartDiffReviewSchedulerForTask(m.lifeCtx, taskID)
				// task-permission-mode D4 (b1)：幂等成功分支同置就绪并扫描。
				if rt := m.getRuntime(taskID); rt != nil {
					rt.setPermJudgeReady()
					m.judgeScan(rt)
				}
				return nil
			}
			m.clearRuntime(taskID)
			return m.cleanupAndFinalizeSuspendDetached(ctx, taskID, killRes, []string{tuiName, processName, legacyServe}, nil)
		}
		// F3：恢复 active 提交成功后启动 diff review 调度器（tryRepairRuntime 经
		// setRuntime 注册运行时，但 setRuntime 不再承担启动点）。幂等。
		m.StartDiffReviewSchedulerForTask(m.lifeCtx, taskID)
		// task-permission-mode D4 (b1)：挂起修复回 active CAS matched 分支置就绪并扫描
		//（覆盖修复重建期间经 align/SSE 登记的 pending）。
		if rt := m.getRuntime(taskID); rt != nil {
			rt.setPermJudgeReady()
			m.judgeScan(rt)
		}
		return nil
	}
	// 修复失败或期间 serve 死亡 → 转分支 a：强制 kill 残余 → suspended。
	// F3：已提交 suspending 后的补偿统一走集中 helper（detached cleanup/finalize 独立预算，
	// tryRepairRuntime 因请求取消失败时也不被打断）。
	return m.cleanupAndFinalizeSuspendDetached(ctx, taskID, killRes, []string{tuiName, processName, legacyServe}, nil)
}

// finalizeSuspendDetached 以独立 detached finalize ctx 提交 finishSuspend（F3：
// suspending 已提交后的收敛统一入口——不被请求取消打断；无强制清理需求的分支 a/b 用）。
func (m *Manager) finalizeSuspendDetached(ctx context.Context, taskID string, killRes []killResultEntry, cause error) error {
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer fcancel()
	return m.finishSuspend(fctx, taskID, killRes, cause)
}

// cleanupAndFinalizeSuspendDetached 先强制清理（独立 detached cleanup 预算，90s 覆盖
// forceKillAll 串行 KillSession 的多次 30s 预算），cleanup 完成后另起独立 detached
// finalize ctx（30s）提交 finishSuspend（F3：cleanup 耗尽不会影响 finalize 预算）。
// killRes 为 kill 阶段已收集结果；forceNames 为需强制 kill 的会话名；cause 为生命周期
// 原因错误（经 finishSuspend 并入 last_error/返回值，不产生 notice）。
func (m *Manager) cleanupAndFinalizeSuspendDetached(ctx context.Context, taskID string, killRes []killResultEntry, forceNames []string, cause error) error {
	m.clearRuntime(taskID)
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	forceRes := m.forceKillAll(cctx, forceNames)
	ccancel()
	killRes = append(killRes, forceRes...)
	return m.finalizeSuspendDetached(ctx, taskID, killRes, cause)
}

// killResultEntry 记录一次 KillSession 结果。
type killResultEntry struct {
	name     string
	alive    bool // kill 前 serve 是否存活
	result   process.KillResult
	occupied bool  // 会话不存在（absent）
	killErr  error // KillSession 本身的基础设施错误（非 nil 时 disposition 不可信）
}

// minimalReleaseBinding 构造 supervisor.Release 的最小 envelope：Release 只消费
// TerminalRef（终止目标）与 data.taskId（provider 侧换代观测身份）；终止目标是
// 任务会话名事实，非 binding 事实来源切换（不读写持久化 binding）。
func minimalReleaseBinding(taskID, name string) domain.TaskBinding {
	data, _ := json.Marshal(map[string]string{"taskId": taskID})
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         store.ProviderIDV1,
		ProviderInstanceID: store.ProviderInstanceV1Default,
		TerminalRef:        name,
		Data:               data,
	}
}

// releaseStageToKillEntry 把 supervisor.Release 的 CleanupStageError 映射回既有
// killResultEntry 通道（阶段 4 Release 接线）：disposition 决策树与 notice 语义
// 不变——Reason 反向映射 process.Disposition 并按 ClassifyKill 语义保真
// SessionKilled（reap_failed=已杀收割失败；snapshot_missing=未净终止 degraded；
// snapshot_failed/kill_failed=会话仍在），tickets 透传；kill_failed 映射为
// killErr 形态（finishSuspend 的 infra 分派：notice kill_failed + tickets +
// "kill session" 错误聚合，与内联 KillSession 基础设施错误同通道）。
func releaseStageToKillEntry(name string, err error) killResultEntry {
	se, ok := appagent.StageErrorOf(err)
	if !ok || se.Cleanup == nil {
		// 非 cleanup 形态（理论不可达——Release 只产 CleanupStageError/nil）：
		// 保守按基础设施错误聚合，不伪造 disposition。
		return killResultEntry{name: name, killErr: err}
	}
	tickets := se.Cleanup.Tickets
	mk := func(killed bool, disp process.CleanupDisposition) killResultEntry {
		return killResultEntry{name: name, result: process.KillResult{
			SessionKilled: killed, Disposition: disp, CleanupTickets: tickets}}
	}
	switch se.Cleanup.Reason {
	case appagent.KillReasonReapFailed:
		return mk(true, process.DispositionReapFailed)
	case appagent.KillReasonSnapshotFailed:
		return mk(false, process.DispositionSnapshotFailed)
	case appagent.KillReasonSnapshotMissing:
		return mk(false, process.DispositionSnapshotMissingDegraded)
	default: // kill_failed：HasSession/KillSession 基础设施错误。
		return killResultEntry{name: name, killErr: err,
			result: process.KillResult{Disposition: process.DispositionKillFailed, CleanupTickets: tickets}}
	}
}

// killTaskSessions 对 names 中存在的会话执行 KillSession，返回结果集。
// 仅收集，不记 notice：逐项 notice 由调用方（finishSuspend）统一聚合记录，
// 避免在此处记录后被 finishSund 再次记录导致同一清理失败双写两条 notice。
// B5e：逐会话 HasSession 基础设施错误（非 ErrNoTmuxServer）MUST 收集为 killErr，
// 不得吞错当 absent（掩盖 infra 故障，design.md §8）。ErrNoTmuxServer 视为 absent。
func (m *Manager) killTaskSessions(ctx context.Context, taskID string, names []string) []killResultEntry {
	var out []killResultEntry
	for _, name := range names {
		// 阶段 4 Release 接线（opencode-v1-spi-provider）：agent runtime（serve，
		// binding.TerminalRef 对应会话）的终止经 supervisor.Release（registry 分派
		// 到 provider——Release 成为生产路径）；C7 provider 准入已在 Suspend 入口
		// 完成。CleanupStageError 映射回 killResultEntry，disposition/notice 通道
		// 不变。TUI/shell 与 legacy -serve（迁移导入桥接特许域，binding 未覆盖）
		// 仍由 task 层直接管理；runtimeSupervisor 未注入（内联测试接缝/golden
		// 矩阵）保持内联实现——与 resumeActive 的回退形态同一模式。
		if name == runtimeSessionName(taskID) && m.runtimeSupervisor != nil {
			if rerr := m.runtimeSupervisor.Release(ctx, minimalReleaseBinding(taskID, name)); rerr != nil {
				out = append(out, releaseStageToKillEntry(name, rerr))
			}
			continue
		}
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			// infra 错误收集为 killErr，由 hasKillFailure 判定失败 → 触发分支 c 修复或强制收敛。
			out = append(out, killResultEntry{name: name, killErr: herr})
			continue
		}
		if !exists {
			// absent-at-entry：上层直接视为该步骤幂等成功（design.md §18）。
			continue
		}
		res, kerr := m.proc.KillSession(name)
		out = append(out, killResultEntry{name: name, result: res, killErr: kerr})
	}
	return out
}

// hasKillFailure 判断是否有非 clean 结果（含 shell 会话，逐项聚合，design.md §19）。
// killErr 非 nil 同样视为失败（基础设施错误不得忽略）。
func hasKillFailure(results []killResultEntry) bool {
	for _, r := range results {
		if r.killErr != nil {
			return true
		}
		if classifyKillResult(r.result).action != "none" {
			return true
		}
	}
	return false
}

// finishSund 落为 suspended：逐项聚合记录残留 notice（killRes + 强制清理中的失败项），
// 清除 env 快照，置 suspended。
// cause 为生命周期原因错误（如恢复 active 提交失败），仅并入 last_error 与返回值——
// F16：MUST NOT 经 killResultEntry 伪造进程清理失败（那会错误产生 retryable kill_failed
// notice，后续 Activate 被债务拒绝）；只有真实进程清理失败（killRes 内的 killErr）
// 才产生 residual notice。
// F20 错误聚合：cause 与全部操作错误（notice 记录失败/env 清理失败/状态提交失败）经
// errors.Join 聚合返回——状态写失败 MUST 无条件进入返回值（不得被 cause 预先占据 firstErr
// 而丢失）；last_error 保存提交前已知错误（cause + notice/env 错误），状态提交自身的错误
// 仅进入返回聚合（无法写进该次失败的持久化操作）。调用方不得用包含 cause 的返回值判断
// 补偿本身是否失败。
// suspended 状态提交失败 MUST 处理：不得静默置为无效流转（design.md §19 Suspend 行）。
func (m *Manager) finishSuspend(ctx context.Context, taskID string, results []killResultEntry, cause error) error {
	var errs []error
	if cause != nil {
		errs = append(errs, cause)
	}
	for _, r := range results {
		if r.killErr != nil {
			// 基础设施错误：记 notice（reason=kill_failed, retryable=true，保留 tickets）。
			if nerr := m.recordResidualNotice(ctx, taskID, r.name, r.result.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
				errs = append(errs, fmt.Errorf("record notice %s: %w", r.name, nerr))
			}
			errs = append(errs, fmt.Errorf("kill session %s: %w", r.name, r.killErr))
			continue
		}
		if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, r.name, r.result); nerr != nil {
			errs = append(errs, fmt.Errorf("record notice %s: %w", r.name, nerr))
		}
	}
	// 清除 env 快照（design.md §2：Suspend 成功清快照）。
	if _, err := m.writeEnvSnapshot(ctx, taskID, sql.NullString{}); err != nil {
		errs = append(errs, fmt.Errorf("clear env snapshot: %w", err))
	}
	le := sql.NullString{}
	if joined := errors.Join(errs...); joined != nil {
		le = sql.NullString{String: joined.Error(), Valid: true}
	}
	if _, err := m.writeStatus(ctx, taskID, StatusSuspended, le); err != nil {
		// F20：状态提交失败无条件进入返回值（不得被 cause 掩盖）。
		errs = append(errs, fmt.Errorf("commit suspended: %w", err))
	} else {
		// 任务离开 active：撤销收敛债务登记（best-effort compare-and-delete，design.md D2）。
		m.deleteConvergeDebt(taskID)
	}
	return errors.Join(errs...)
}

// tryRepairRuntime 尝试修复运行时（design.md §5 分支 c，B7）。
// 修复 MUST 恢复完整运行时（SSE 订阅 + 全量对齐 + watchers + RuntimeGroup 重建）才算成功回 active。
// 修复期间产生的 retryable notice 不得被后台拿去杀刚修复的 serve（后台重试须校验 generation/注册表，
// 由 sessionOwnedByRuntime 保证）。
//
// mode 为 Suspend 入口已解析的对齐模式（D8：复用传入值，不重复查询项目 kind）。
// 返回 (fixed, err)：serve 存活且完整运行时重建成功 → (true, nil)；否则 (false, err)。
func (m *Manager) tryRepairRuntime(ctx context.Context, taskID string, mode AlignMode) (bool, error) {
	serveName := runtimeSessionName(taskID)
	alive, err := m.proc.HasSession(serveName)
	if err != nil || !alive {
		legacy := serveSessionName(taskID)
		if la, lerr := m.proc.HasSession(legacy); lerr == nil && la {
			serveName = legacy
			alive = true
			err = nil
		}
	}
	if err != nil || !alive {
		return false, fmt.Errorf("runtime gone before repair: %w", err)
	}
	// C2 门禁修复（design D9a 入口映射：tryRepairRuntime → Resume 仅修复（不建程）
	// {AllowSessionCreate=false, AllowRebuild=false, BeforeLaunch=nil}）：runtimeSupervisor
	// 注入时，存活终端健康验证经 supervisor.Resume 复用分支——会话仍在为前置
	//（上方 HasSession 已校验；复用零 NewSession），策略矩阵与 D9 一致。
	if m.runtimeSupervisor != nil {
		oldBinding, _, berr := m.store.GetTaskBinding(ctx, taskID)
		if berr != nil || oldBinding.TerminalRef == "" {
			return false, fmt.Errorf("cannot recover binding for repair")
		}
		binding, serr := m.runtimeSupervisor.Resume(ctx,
			appagent.TaskRef{TaskID: taskID, Directory: m.worktreePathForRepair(taskID, serveName)},
			oldBinding, appagent.ResumePolicy{AllowSessionCreate: false, AllowRebuild: false})
		if serr != nil {
			return false, fmt.Errorf("supervisor resume reuse: %w", serr)
		}
		port, password, _, eerr := m.bindingEndpoint(binding)
		if eerr != nil {
			return false, fmt.Errorf("provider binding endpoint: %w", eerr)
		}
		return m.repairRuntimeRegistered(ctx, taskID, mode, serveName, port, password)
	}
	// 健康检查凭据/端点从持久化 binding 解析（tasks 4.3 事实来源切换）；任务行由
	// repairRuntimeRegistered 读取（initPermState 需要）。
	oc, _, ok := m.taskOcBindingClient(ctx, taskID, appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	if !ok {
		return false, fmt.Errorf("cannot recover binding for repair")
	}
	if _, err := oc.Health(ctx); err != nil {
		return false, fmt.Errorf("health check: %w", err)
	}
	return m.repairRuntimeRegistered(ctx, taskID, mode, serveName, -1, "")
}

// worktreePathForRepair 返回修复路径的 supervisor TaskRef 目录（任务行 worktree；
// 读取失败回退空串——supervisor checkDeps 对空目录 fail-closed，等价既有失败语义）。
func (m *Manager) worktreePathForRepair(taskID, serveName string) string {
	if row, err := m.store.GetTask(context.Background(), taskID); err == nil {
		return row.WorktreePath
	}
	_ = serveName
	return ""
}

// repairRuntimeRegistered 是 tryRepairRuntime 的注册段（共享）：启动事实校验 →
// runtime 注册 → SSE（C9 finalBinding 语义：supervisor 路径显式传入 binding）→
// watchServeExit。port<0 表示内联回退路径（SSE 端点经持久化 binding 自解析）。
func (m *Manager) repairRuntimeRegistered(ctx context.Context, taskID string, mode AlignMode, serveName string, port int, password string) (bool, error) {
	// 重建运行时 → SSE 订阅 + 全量对齐（B7：恢复完整运行时）。mode 由 Suspend 入口传入。
	// 启动事实读校验（task-permission-mode D5 三路径矩阵②）：有存活进程但启动事实
	// NULL/非法 MUST 失败不注册（fail-closed）。
	row, grerr := m.store.GetTask(ctx, taskID)
	if grerr != nil {
		return false, fmt.Errorf("get task: %w", grerr)
	}
	startMode, serr := m.readStartFactForRegister(ctx, taskID)
	if serr != nil {
		return false, serr
	}
	rt := m.newRuntime(taskID)
	if err := initPermState(rt, row, startMode); err != nil {
		return false, err
	}
	m.setRuntime(taskID, rt)
	rt.registerGroup(roleRuntime, serveName)
	if port > 0 {
		binding, berr := m.currentTaskBinding(ctx, taskID)
		if berr != nil {
			m.clearRuntime(taskID)
			return false, fmt.Errorf("resolve binding for sse: %w", berr)
		}
		if err := m.startSSE(ctx, rt, taskID, row.WorktreePath, port, password, mode, &binding); err != nil {
			m.clearRuntime(taskID)
			return false, fmt.Errorf("sse resubscribe: %w", err)
		}
	} else if err := m.startSSEFromBinding(ctx, rt, taskID, mode); err != nil {
		m.clearRuntime(taskID)
		return false, fmt.Errorf("sse resubscribe: %w", err)
	}
	m.watchServeExit(taskID, serveName)
	return true, nil
}

// forceKillAll 对已死路径强制 kill 全部会话（尽力清理）。
// R7 fail-closed：HasSession/KillSession infra 错误 MUST 收集为 killErr entry，
// 不得吞错——无法清理的会话经 finishSund 记 retryable notice 形成可重试 debt
// （否则残留会话下次 Activate 被 residual 门禁永久阻塞，design.md §5/§8）。
// ErrNoTmuxServer 视为 absent（无 server 即无会话可清）。
// 阶段 4 Release 接线说明：本函数是挂起补偿的强制清场兜底（tui/shell/serve 混合
// 集合、「尽力清理」语义），保持 task 层直接管理不经 Release——agent runtime 的
// Release 接线覆盖主清理路径（killTaskSessions），补偿兜底不改变。
func (m *Manager) forceKillAll(ctx context.Context, names []string) []killResultEntry {
	var out []killResultEntry
	for _, name := range names {
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			// infra 错误：收集为 killErr，finishSuspend 记 retryable notice。
			out = append(out, killResultEntry{name: name, killErr: herr})
			continue
		}
		if !exists {
			continue
		}
		res, kerr := m.proc.KillSession(name)
		if kerr != nil {
			// kill infra 错误：收集为 killErr（result 零值，finishSuspend 按 kill_err 记 notice）。
			out = append(out, killResultEntry{name: name, killErr: kerr})
			continue
		}
		out = append(out, killResultEntry{name: name, result: res})
	}
	return out
}

// listShellSessions 枚举该任务存在的 shell-<n> 会话名（通过 tmux ls）。
// B7b：ListSessions 基础设施错误 MUST 传播（不得静默吞错，design.md §8）。
func (m *Manager) listShellSessions(taskID string) ([]string, error) {
	sessions, err := m.proc.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("list sessions for shell enumeration (task %s): %w", taskID, err)
	}
	var out []string
	prefix := "ocdeck-" + taskID + "-shell-"
	for _, s := range sessions {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			out = append(out, s)
		}
	}
	return out, nil
}

// newSessionSpec 构造 SessionSpec 辅助。
func newSessionSpec(name, dir string, env map[string]string, cmd []string) process.SessionSpec {
	return process.SessionSpec{Name: name, Dir: dir, Env: env, CmdArgv: cmd}
}

// intFromNullInt sql.NullInt64 → int（无效返回 0）。
func intFromNullInt(n sql.NullInt64) int {
	if n.Valid {
		return int(n.Int64)
	}
	return 0
}
