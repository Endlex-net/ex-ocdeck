package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
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
	if row.Status != StatusActive {
		return newOpErr(codeInvalidState, fmt.Errorf("suspend requires active, got %s", row.Status))
	}
	// D8：状态转换前解析项目 kind（任何副作用前 fail-closed）。未知持久化 kind → internal（D1）。
	proj, perr := m.store.GetProject(ctx, row.ProjectID)
	if perr != nil {
		return newOpErr(codeNotFound, fmt.Errorf("project gone: %w", perr))
	}
	mode, kerr := alignModeForKind(proj.Kind)
	if kerr != nil {
		return newOpErr(codeInternal, kerr)
	}
	updated, err := m.store.UpdateTaskStatusConditional(ctx, taskID, StatusActive, StatusSuspending, sql.NullString{})
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	if !updated {
		return newOpErr(codeConflict, fmt.Errorf("task %s state changed before suspend", taskID))
	}
	return m.suspendRun(ctx, taskID, mode)
}

// suspendRun 执行挂起的外部副作用并按互斥决策树收敛（design.md §5）。
// 决策树按序判定取首个命中；分支判定以 kill 前 serve 是否存活为准（B7 时机）：
//   a) kill 前 serve 已死 → 继续完成剩余清理 → suspended（个别失败记 notice + 后台重试）；
//   b) kill 前 serve 存活且全部 kill 成功 → suspended；
//   c) kill 前 serve 存活但有 kill 失败 → 尝试修复运行时（恢复完整运行时才算成功）→
//      修复成功回 active + last_error；修复失败或期间 serve 死亡 → 转分支 a。
//
// 不变量：Suspend 已提交 suspending（Suspend 入口），本函数任意失败路径 MUST 收敛到
// active 或 suspended，不得停留 suspending（Retry 不接受 suspending，只能重启 reconcile，
// design.md §5）。infra 错误（HasSession/ListSessions）MUST 经 forceKillAll + finishSuspend
// 落 suspended + last_error，不得直接返回 error 留 suspending。
//
// mode 为 Suspend 入口已解析的对齐模式，传入 repair 路径复用（D8：不重复查询项目 kind）。
func (m *Manager) suspendRun(ctx context.Context, taskID string, mode AlignMode) error {
	// 停 SSE 订阅 + 退出监视。
	m.clearRuntime(taskID)

	// 分支判定时机：kill 前查 serve 是否已死（B7，D2：预探测而非事后推断）。
	serveName := serveSessionName(taskID)
	tuiName := tuiSessionName(taskID)
	serveAliveBeforeKill, herr := m.proc.HasSession(serveName)
	// B5e：HasSession 基础设施错误（非 ErrNoTmuxServer）MUST 收敛到 suspended，不得直接返回留 suspending。
	// 保守视为 serve 状态不可判定 → 强制 kill 残余 + finishSuspend 落 suspended + last_error。
	// infra 错误作为 killResultEntry（killErr）传入 finishSuspend，使 last_error 含 infra 上下文
	//（finishSund 会提交 suspended + 记 notice + 以 firstErr 作 last_error）。
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		forceRes := m.forceKillAll(ctx, []string{tuiName, serveName})
		forceRes = append(forceRes, killResultEntry{name: serveName, killErr: herr})
		return newOpErr(codeProcessError, m.finishSuspend(ctx, taskID, forceRes))
	}

	// KillSession 全部会话：tui → shells → serve（design.md §19；serve 最后杀，
	// 清理期间继续捕获 session，各自先 reaper 快照）。
	killRes := m.killTaskSessions(ctx, taskID, []string{tuiName})
	// shell 会话：枚举存在的 shell-<n>。B7b：ListSessions infra 错误 MUST 收敛到 suspended
	//（infra 错误作为 killResultEntry 传入 finishSund，使 last_error 含 infra 上下文）。
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		forceRes := m.forceKillAll(ctx, []string{tuiName, serveName})
		forceRes = append(forceRes, killResultEntry{name: serveName, killErr: err})
		return newOpErr(codeProcessError, m.finishSuspend(ctx, taskID, append(killRes, forceRes...)))
	}
	killRes = append(killRes, m.killTaskSessions(ctx, taskID, shellNames)...)
	killRes = append(killRes, m.killTaskSessions(ctx, taskID, []string{serveName})...)

	// 分支 a：kill 前 serve 已死 → 继续完成剩余清理 → suspended。
	if !serveAliveBeforeKill {
		return m.finishSuspend(ctx, taskID, killRes)
	}

	// 分支 b/c：kill 前 serve 存活。
	// 判定是否有 kill 失败（B7：以 kill 结果为准，非 kill 后 HasSession）。
	hasFailure := hasKillFailure(killRes)
	if !hasFailure {
		// 分支 b：全部成功 → suspended。
		return m.finishSuspend(ctx, taskID, killRes)
	}

	// 分支 c：serve 存活但有 kill 失败 → 尝试修复运行时（恢复完整运行时，B7）。
	fixed, ferr := m.tryRepairRuntime(ctx, taskID, mode)
	if ferr == nil && fixed {
		// 修复成功 → 回 active + last_error（design.md §5）。
		le := sql.NullString{String: "suspend partially failed, runtime repaired", Valid: true}
		_, _ = m.store.UpdateTaskStatusConditional(ctx, taskID, StatusSuspending, StatusActive, le)
		return nil
	}
	// 修复失败或期间 serve 死亡 → 转分支 a：强制 kill 残余 → suspended。
	forceRes := m.forceKillAll(ctx, []string{tuiName, serveName})
	killRes = append(killRes, forceRes...)
	return m.finishSuspend(ctx, taskID, killRes)
}

// killResultEntry 记录一次 KillSession 结果。
type killResultEntry struct {
	name      string
	alive     bool // kill 前 serve 是否存活
	result    process.KillResult
	occupied  bool // 会话不存在（absent）
	killErr   error // KillSession 本身的基础设施错误（非 nil 时 disposition 不可信）
}

// killTaskSessions 对 names 中存在的会话执行 KillSession，返回结果集。
// 仅收集，不记 notice：逐项 notice 由调用方（finishSuspend）统一聚合记录，
// 避免在此处记录后被 finishSund 再次记录导致同一清理失败双写两条 notice。
// B5e：逐会话 HasSession 基础设施错误（非 ErrNoTmuxServer）MUST 收集为 killErr，
// 不得吞错当 absent（掩盖 infra 故障，design.md §8）。ErrNoTmuxServer 视为 absent。
func (m *Manager) killTaskSessions(ctx context.Context, taskID string, names []string) []killResultEntry {
	var out []killResultEntry
	for _, name := range names {
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
		if r.result.Disposition != "" && r.result.Disposition != process.DispositionClean {
			return true
		}
	}
	return false
}

// finishSund 落为 suspended：逐项聚合记录残留 notice（killRes + 强制清理中的失败项），
// 清除 env 快照，置 suspended。
// suspended 状态提交失败 MUST 处理：env 快照/状态写回失败记 last_error 并返回错误，
// 不得静默置为无效流转（design.md §19 Suspend 行：kill 失败记 notice + last_error）。
func (m *Manager) finishSuspend(ctx context.Context, taskID string, results []killResultEntry) error {
	var firstErr error
	for _, r := range results {
		if r.killErr != nil {
			// 基础设施错误：记 notice（reason=kill_failed, retryable=true，保留 tickets）。
			if nerr := m.recordResidualNotice(ctx, taskID, r.name, r.result.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("record notice %s: %w", r.name, nerr)
				}
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("kill session %s: %w", r.name, r.killErr)
			}
			continue
		}
		if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, r.name, r.result); nerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("record notice %s: %w", r.name, nerr)
			}
		}
	}
	// 清除 env 快照（design.md §2：Suspend 成功清快照）。
	if err := m.store.UpdateTaskEnvSnapshot(ctx, taskID, sql.NullString{}); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("clear env snapshot: %w", err)
		}
	}
	le := sql.NullString{}
	if firstErr != nil {
		le = sql.NullString{String: firstErr.Error(), Valid: true}
	}
	if err := m.store.UpdateTaskStatus(ctx, taskID, StatusSuspended, le); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("commit suspended: %w", err)
		}
	}
	return firstErr
}

// tryRepairRuntime 尝试修复运行时（design.md §5 分支 c，B7）。
// 修复 MUST 恢复完整运行时（SSE 订阅 + 全量对齐 + watchers + RuntimeGroup 重建）才算成功回 active。
// 修复期间产生的 retryable notice 不得被后台拿去杀刚修复的 serve（后台重试须校验 generation/注册表，
// 由 sessionOwnedByRuntime 保证）。
//
// mode 为 Suspend 入口已解析的对齐模式（D8：复用传入值，不重复查询项目 kind）。
// 返回 (fixed, err)：serve 存活且完整运行时重建成功 → (true, nil)；否则 (false, err)。
func (m *Manager) tryRepairRuntime(ctx context.Context, taskID string, mode AlignMode) (bool, error) {
	serveName := serveSessionName(taskID)
	alive, err := m.proc.HasSession(serveName)
	if err != nil || !alive {
		return false, fmt.Errorf("serve gone before repair: %w", err)
	}
	row, gerr := m.store.GetTask(ctx, taskID)
	if gerr != nil {
		return false, fmt.Errorf("get task: %w", gerr)
	}
	// 读回 serve 密码与端口（端口以会话内 OCDECK_SERVE_PORT 为唯一权威来源，design.md §3/§5：
	// last_port 非事实来源，MUST NOT 回退）。读回失败/空 MUST 返回错误，不得用 last_port 兜底。
	password := m.recoverPassword(ctx, taskID)
	if password == "" {
		return false, fmt.Errorf("cannot recover serve password")
	}
	portStr, perr := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
	if perr != nil || portStr == "" {
		return false, fmt.Errorf("cannot recover serve port from session: %w", perr)
	}
	port, ok := parsePort(portStr)
	if !ok {
		return false, fmt.Errorf("invalid serve port %q", portStr)
	}
	// 认证健康检查探活（校验 serve 仍健康）。
	oc := m.ocFactory(port, password, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	if _, err := oc.Health(ctx); err != nil {
		return false, fmt.Errorf("health check: %w", err)
	}
	// 重建运行时 → SSE 订阅 + 全量对齐（B7：恢复完整运行时）。mode 由 Suspend 入口传入。
	rt := m.newRuntime(taskID)
	m.setRuntime(taskID, rt)
	rt.registerGroup("serve", serveName)
	if err := m.startSSE(ctx, rt, taskID, row.WorktreePath, port, password, mode); err != nil {
		m.clearRuntime(taskID)
		return false, fmt.Errorf("sse resubscribe: %w", err)
	}
	// 重开 tui 会话（若不存在），锚定确定 session（design.md §4：不使用 --continue）。
	tuiName := tuiSessionName(taskID)
	if tuiExists, _ := m.proc.HasSession(tuiName); !tuiExists {
		env, err := m.loadEnvSnapshot(row)
		if err != nil {
			m.clearRuntime(taskID)
			return false, fmt.Errorf("load env snapshot: %w", err)
		}
		tuiEnv := copyMap(env)
		tuiEnv["OPENCODE_SERVER_PASSWORD"] = password
		sessionID, aerr := m.resolveAnchorSession(ctx, oc, row)
		if aerr != nil {
			m.clearRuntime(taskID)
			return false, fmt.Errorf("reopen tui: %w", aerr)
		}
		if err := m.proc.NewSession(newSessionSpec(tuiName, row.WorktreePath, tuiEnv,
			[]string{"opencode", "attach", fmt.Sprintf("http://127.0.0.1:%d", port), "--session", sessionID})); err != nil {
			m.clearRuntime(taskID)
			return false, fmt.Errorf("reopen tui: %w", err)
		}
	}
	rt.registerGroup("tui", tuiName)
	// 退出监视（watchers 重建）。
	m.watchServeExit(taskID, serveName)
	m.watchTUIExit(taskID, tuiName)
	return true, nil
}

// forceKillAll 对已死路径强制 kill 全部会话（尽力清理）。
// R7 fail-closed：HasSession/KillSession infra 错误 MUST 收集为 killErr entry，
// 不得吞错——无法清理的会话经 finishSund 记 retryable notice 形成可重试 debt
//（否则残留会话下次 Activate 被 residual 门禁永久阻塞，design.md §5/§8）。
// ErrNoTmuxServer 视为 absent（无 server 即无会话可清）。
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

// recoverPassword 从 serve 会话环境恢复密码（persist 恢复 / suspend 修复用，design.md §2）。
func (m *Manager) recoverPassword(ctx context.Context, taskID string) string {
	pw, err := m.proc.ShowSessionEnv(serveSessionName(taskID), "OPENCODE_SERVER_PASSWORD")
	if err != nil {
		return ""
	}
	return pw
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