package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
)

// Reconcile 启动 reconciliation（design.md §5 + §10 shutdownPolicy 三模式 + tasks 3.8）。
// 必须在 HTTP 服务就绪前完成对账。调用方在 cmd 启动时调用。
func (m *Manager) Reconcile(ctx context.Context) error {
	policy := m.cfg.ShutdownPolicy

	// tasks 3.8：ConvergeInterruptedInitRuns MUST 先于既有启动恢复步骤执行——
	// 把 init_status∈{pending,running} 的任务收敛为 failed（interrupted by server restart）。
	// 更新失败 MUST fail-closed 阻止 HTTP 开放。
	if n, err := m.store.ConvergeInterruptedInitRuns(ctx); err != nil {
		return fmt.Errorf("reconcile: converge interrupted init runs: %w", err)
	} else if n > 0 {
		log.Printf("reconcile: converged %d interrupted init runs", n)
	}

	// R7：恢复持久化的未收敛 orphan tickets 到内存（design.md §10：进程退出再启动后
	// 从 cleanup_debts 表恢复重试，不随进程退出丢失）。先于一切处理，确保后台周期与
	// Shutdown 的 retryOrphanSessions 能收割这些跨重启的逃逸进程 debt。
	// P4 复评阻塞 3：restore 失败 MUST fail-closed 拒绝开放 HTTP（与既有 Reconcile fail-closed 一致）。
	if rerr := m.restoreCleanupDebts(ctx); rerr != nil {
		return fmt.Errorf("reconcile: restore orphan debts: %w", rerr)
	}
	// P0：持久化 cleanup_debts 恢复后 MUST 在开放 HTTP 前完成一次重试收割（不只恢复到内存，
	// design.md §10）：恢复到内存的逃逸进程 tickets 若不主动收割，下次后台周期（30s）才处理，
	// 窗口期 reconcile 误判 runtime 已净。此处同步收割一次，仍残留的留给后台周期。
	// B2：retryOrphanSessions 的 cleanup_debt 持久化错误 MUST 传播（main 据此拒开 HTTP）。
	if rerr := m.retryOrphanSessions(ctx); rerr != nil {
		return fmt.Errorf("reconcile: retry orphan sessions: %w", rerr)
	}

	// 枚举全部 DB 任务。
	tasks, err := m.store.ListAllTasks(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: list tasks: %w", err)
	}

	// cleanup-debt pre-pass（design.md §5：全局首步骤，先于枚举/处理会话）。
	// 读取全部任务 residual_processes notice → RetryReap → 原子更新 remaining。
	// 仍有 cleanup debt 的任务在后续矩阵中 MUST NOT 恢复 active。
	// pre-pass 错误聚合返回（不静默）：store/进程错误 MUST 传播，调用方据此 fail-closed。
	if perr := m.reconcileDebtPrePassAll(ctx, tasks); perr != nil {
		return fmt.Errorf("reconcile: cleanup-debt pre-pass: %w", perr)
	}

	// 枚举 tmux 会话（运行时注册表）。
	// B9：ListSessions infra 错误不得当空集合（fail-closed：不确定就不改状态）。
	sessions, err := m.proc.ListSessions()
	if err != nil {
		// ListSessions 无 server 时返回空列表（非错误）；其他错误 fail-closed。
		if !errors.Is(err, process.ErrNoTmuxServer) {
			return fmt.Errorf("reconcile: list sessions: %w", err)
		}
		sessions = nil
	}

	// 按 taskID 分组会话。
	sessionsByTask := groupSessionsByTask(sessions)

	// 孤儿会话（taskID 无 DB 行）：清理失败进后台重试（非仅日志，B9）。
	// B2/第三轮：killOrphanSession 返回错误 MUST 检查并传播（当前忽略），含 kill 失败与新发现
	// orphan 的 persistOrphanDebt 持久化失败——聚合进 errs，随 policy 分支返回传播给 main 据此拒开 HTTP。
	taskByID := make(map[string]TaskRow, len(tasks))
	for _, t := range tasks {
		taskByID[t.ID] = t
	}
	var errs []error
	for taskID, names := range sessionsByTask {
		if _, ok := taskByID[taskID]; !ok {
			for _, name := range names {
				if err := m.killOrphanSession(ctx, name); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	switch policy {
	case config.ShutdownPersist:
		if err := m.reconcilePersist(ctx, tasks, sessionsByTask); err != nil {
			errs = append(errs, err)
		}
		// 第三轮：persist 模式枚举/恢复可能产生新 orphan，开放 HTTP 前 MUST 再次 flush
		// persistOrphanDebts 保证"HTTP 开放时无仅内存 orphan tickets"（design.md §10）。
		// killOrphanSession 已逐项立即持久化，此处 flush 统一收敛（删除已收敛 + 重 upsert 全部内存 orphan）。
		if err := m.persistOrphanDebts(ctx); err != nil {
			errs = append(errs, fmt.Errorf("flush orphan debts before http open: %w", err))
		}
	case config.ShutdownKillOnStart, config.ShutdownKillImmediate:
		if err := m.reconcileKill(ctx, tasks, sessions); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// commitSuspendedReconcile 在 reconcile persist 分支把任务从 fromStatus CAS 到 suspended
// 并记录 last_error（design.md §5）。B2：状态 CAS 的 error 与 committed=false 结果 MUST
// 检查并返回（不得 _, _ 吞没）——committed=false 表示状态已被外部改动（CAS 不匹配），
// 调用方据此聚合进 Reconcile 返回值，main 据此拒开 HTTP。返回 nil 仅当 CAS 成功提交。
func (m *Manager) commitSuspendedReconcile(ctx context.Context, taskID, fromStatus, lastError string) error {
	committed, err := m.store.UpdateTaskStatusConditional(ctx, taskID, fromStatus, StatusSuspended, sql.NullString{String: lastError, Valid: lastError != ""})
	if err != nil {
		return fmt.Errorf("commit suspended (from %s): %w", fromStatus, err)
	}
	if !committed {
		return fmt.Errorf("commit suspended (from %s): CAS not matched (state changed concurrently)", fromStatus)
	}
	return nil
}

// reconcilePersist persist 模式恢复序列（design.md §5 矩阵）。
// KillSession/notice/状态提交错误聚合返回（design.md §5/§8，不静默）。
// B2：状态 CAS 的 error 与 committed=false 结果 MUST 检查并传播（不得 _, _ 吞没）。
func (m *Manager) reconcilePersist(ctx context.Context, tasks []TaskRow, sessionsByTask map[string][]string) error {
	var errs []error
	for _, t := range tasks {
		names := sessionsByTask[t.ID]
		hasServe := containsName(names, serveSessionName(t.ID))
		switch t.Status {
		case StatusActive, StatusActivating:
			// cleanup-debt pre-pass 已在 Reconcile 全局首步骤消化（design.md §5）。
			// hasDebt 判定 MUST 重读任务的当前 notice：pre-pass 可能已清债，
			// 旧 t.Notice 是 pre-pass 前快照，沿用会导致已清债任务被错误 kill+suspended。
			cur, rerr := m.store.GetTask(ctx, t.ID)
			if rerr != nil {
				// 读不回 → 状态不确定，fail-closed：kill runtime 落 suspended。
				kerr := m.cleanupTaskRuntimeReconcile(ctx, t.ID, names)
				if cerr := m.commitSuspendedReconcile(ctx, t.ID, t.Status, fmt.Sprintf("reconcile: reread task: %v", rerr)); cerr != nil {
					errs = append(errs, fmt.Errorf("task %s reread commit suspended: %w", t.ID, cerr))
				}
				errs = append(errs, fmt.Errorf("task %s reread: %w", t.ID, rerr))
				if kerr != nil {
					errs = append(errs, fmt.Errorf("task %s cleanup: %w", t.ID, kerr))
				}
				continue
			}
			if hasRetryable, _ := m.hasRetryableNotice(ctx, cur); hasRetryable {
				kerr := m.cleanupTaskRuntimeReconcile(ctx, t.ID, names)
				if cerr := m.commitSuspendedReconcile(ctx, t.ID, t.Status, "cleanup debt blocks resume"); cerr != nil {
					errs = append(errs, fmt.Errorf("task %s debt-blocks commit suspended: %w", t.ID, cerr))
				}
				if kerr != nil {
					errs = append(errs, fmt.Errorf("task %s cleanup (debt blocks): %w", t.ID, kerr))
				}
				continue
			}
			if hasServe && m.serveHealthyRecoverable(ctx, cur) {
				// 恢复活跃：读回密码与端口 → 重建运行时 → SSE 订阅 + 全量对齐。
				// 使用重读后的 cur（notice 已消化），避免基于旧快照恢复。
				if err := m.resumeActive(ctx, cur); err != nil {
					// 恢复中途失败 → kill runtime → suspended + last_error。
					cleanupErr := m.cleanupActivationRuntime(ctx, t.ID)
					le := err.Error()
					if cleanupErr != nil {
						le = fmt.Sprintf("%s; cleanup notice: %v", le, cleanupErr)
					}
					if cerr := m.commitSuspendedReconcile(ctx, t.ID, t.Status, le); cerr != nil {
						errs = append(errs, fmt.Errorf("task %s resume commit suspended: %w", t.ID, cerr))
					}
					errs = append(errs, fmt.Errorf("task %s resume: %w", t.ID, err))
				}
			} else {
				// serve 已消失/健康失败 → 完整运行时清理（残余会话 kill + watcher/SSE 收敛）→ suspended + last_error（F4）。
				// activating 状态也 MUST kill 残留 serve 会话（此前仅落 suspended 不清进程）。
				kerr := m.cleanupTaskRuntimeReconcile(ctx, t.ID, names)
				if cerr := m.commitSuspendedReconcile(ctx, t.ID, t.Status, "serve gone during reconcile"); cerr != nil {
					errs = append(errs, fmt.Errorf("task %s serve-gone commit suspended: %w", t.ID, cerr))
				}
				if kerr != nil {
					errs = append(errs, fmt.Errorf("task %s cleanup (serve gone): %w", t.ID, kerr))
				}
			}
		case StatusSuspending:
			// 以持久化意图为准：完成清理 → suspended。
			if kerr := m.killTaskSessionsReconcile(ctx, t.ID, names); kerr != nil {
				errs = append(errs, fmt.Errorf("task %s kill sessions: %w", t.ID, kerr))
			}
			if serr := m.store.UpdateTaskStatus(ctx, t.ID, StatusSuspended, sql.NullString{}); serr != nil {
				errs = append(errs, fmt.Errorf("task %s commit suspended: %w", t.ID, serr))
			}
		case StatusSuspended, StatusArchived:
			// 存在会话则 kill（状态不变，记 notice）。
			if len(names) > 0 {
				if kerr := m.killTaskSessionsReconcile(ctx, t.ID, names); kerr != nil {
					errs = append(errs, fmt.Errorf("task %s kill sessions: %w", t.ID, kerr))
				}
			}
		case StatusCreating:
			// kill 异常会话 → creation_failed。
			if kerr := m.killTaskSessionsReconcile(ctx, t.ID, names); kerr != nil {
				errs = append(errs, fmt.Errorf("task %s kill sessions: %w", t.ID, kerr))
			}
			if serr := m.store.UpdateTaskStatus(ctx, t.ID, StatusCreationFailed, sql.NullString{String: "reconcile: interrupted during creating", Valid: true}); serr != nil {
				errs = append(errs, fmt.Errorf("task %s commit creation_failed: %w", t.ID, serr))
			}
		case StatusCreationFailed:
			// 保持原状，kill 异常会话。
			if kerr := m.killTaskSessionsReconcile(ctx, t.ID, names); kerr != nil {
				errs = append(errs, fmt.Errorf("task %s kill sessions: %w", t.ID, kerr))
			}
		case StatusDeleting, StatusDeletionFailed:
			// 保持状态，kill 会话，提示用户 Retry。
			if kerr := m.killTaskSessionsReconcile(ctx, t.ID, names); kerr != nil {
				errs = append(errs, fmt.Errorf("task %s kill sessions: %w", t.ID, kerr))
			}
		}
	}
	return errors.Join(errs...)
}

// reconcileKill kill 模式：kill 全部会话，active/activating/suspending → suspended（design.md §5/§10）。
// P0：kill 清理错误与状态 CAS 错误 MUST 传播（main 对 Reconcile 失败拒开 HTTP 才有效）。
func (m *Manager) reconcileKill(ctx context.Context, tasks []TaskRow, sessions []string) error {
	var errs []error
	// kill 全部 ocdeck 会话（聚合 kill 失败错误，不静默）。
	for _, name := range sessions {
		if kerr := m.killOrphanSession(ctx, name); kerr != nil {
			errs = append(errs, kerr)
		}
	}
	for _, t := range tasks {
		switch t.Status {
		case StatusActive, StatusActivating, StatusSuspending:
			if serr := m.store.UpdateTaskStatus(ctx, t.ID, StatusSuspended, sql.NullString{String: "kill mode reconcile", Valid: true}); serr != nil {
				errs = append(errs, fmt.Errorf("task %s commit suspended: %w", t.ID, serr))
			}
		}
		// archived/creation_failed/deletion_failed 保持原状。
	}
	return errors.Join(errs...)
}

// resumeActive persist 恢复活跃运行时（design.md §5 恢复序列）。
// 端口以会话内 OCDECK_SERVE_PORT 为准（读回失败 → suspended+last_error，不得静默用 last_port，B9）。
// env snapshot 解析错误传播（B9）。
func (m *Manager) resumeActive(ctx context.Context, t TaskRow) error {
	serveName := serveSessionName(t.ID)
	pw, err := m.proc.ShowSessionEnv(serveName, "OPENCODE_SERVER_PASSWORD")
	if err != nil || pw == "" {
		return fmt.Errorf("recover password: %w", err)
	}
	// 端口以会话内 OCDECK_SERVE_PORT 为准（design.md §5）。
	portStr, err := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
	if err != nil || portStr == "" {
		// 读回失败 → 激活失败（不得静默用 last_port，B9）。last_port 仅交叉校验。
		return fmt.Errorf("recover serve port from session: %w", err)
	}
	port, ok := parsePort(portStr)
	if !ok {
		return fmt.Errorf("invalid serve port %q", portStr)
	}
	// 认证健康检查探活（校验会话内 OCDECK_TASK_ID 与任务匹配，design.md §5）。
	oc := m.ocFactory(port, pw, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	if _, err := oc.Health(ctx); err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	// 校验会话内 OCDECK_TASK_ID 与任务匹配（design.md §5）。
	taskIDInSession, terr := m.proc.ShowSessionEnv(serveName, "OCDECK_TASK_ID")
	if terr != nil || taskIDInSession != t.ID {
		return fmt.Errorf("OCDECK_TASK_ID mismatch (got %q want %q): %w", taskIDInSession, t.ID, terr)
	}
	// 重建运行时 → SSE 订阅 + 全量对齐。
	// 注册前校验 serve 会话仍存活（C2：persist 恢复路径同样校验，避免注册已消失会话）。
	if alive, _ := m.proc.HasSession(serveName); !alive {
		return fmt.Errorf("serve session gone before runtime register")
	}
	rt := m.newRuntime(t.ID)
	m.setRuntime(t.ID, rt)
	rt.registerGroup("serve", serveName)
	if err := m.startSSE(ctx, rt, t.ID, t.WorktreePath, port, pw); err != nil {
		m.clearRuntime(t.ID)
		return err
	}
	// 从 DB 恢复原 env 快照（persist 重启不是 env 生效点，design.md §2）。
	// env snapshot 解析错误传播（B9）。
	if _, err := m.loadEnvSnapshot(t); err != nil {
		m.clearRuntime(t.ID)
		return fmt.Errorf("load env snapshot: %w", err)
	}
	// P0：resumeActive MUST 先恢复 SSE/watcher/RuntimeGroup 再写 active（design.md §5）。
	// 先写 active 后恢复 watchers 失败会留"DB active、runtime 已清"假状态（watcher 失败
	// 后 clearRuntime 但 DB 仍 active）。调整：全部运行时恢复成功后再提交 active；
	// 恢复失败由调用方 cleanupActivationRuntime + UpdateTaskStatusConditional 回 suspended。
	// 补注册存活会话的 RuntimeGroup + watchers（F2：persist 恢复完整运行时）。
	// SSE 订阅+全量对齐完成后补注册 RuntimeGroup（此前仅注册 serve）。
	// serve watcher + tui/shell watchers + groups。
	m.watchServeExit(t.ID, serveName)
	if err := m.resumeRuntimeWatchers(t.ID, serveName); err != nil {
		// B7b：shell 枚举 infra 错误 fail-closed——清理已起的 serve watcher 后返回错误。
		m.clearRuntime(t.ID)
		return fmt.Errorf("resume runtime watchers: %w", err)
	}
	// 标记 active（提交点；失败 MUST 返回错误，main 的 fail-closed 才能覆盖，B9）。
	// 运行时已完整恢复（SSE + watchers + groups），此时提交 active 不会留假状态。
	if err := m.store.UpdateTaskStatus(ctx, t.ID, StatusActive, sql.NullString{}); err != nil {
		m.clearRuntime(t.ID)
		return fmt.Errorf("commit active on resume: %w", err)
	}
	return nil
}

// resumeRuntimeWatchers 为存活会话补注册 RuntimeGroup + watchers（F2：persist 恢复完整运行时）。
// 调用方已 setRuntime + registerGroup(serve) + watchServeExit。此处补 TUI 与全部 shell：
//   - tui 存活 → registerGroup(tui) + watchTUIExit；
//   - 每个 shell-<n> 存活 → registerGroup(shell) + watchShellExit。
//
// B7b：shell 枚举 ListSessions 错误 MUST 传播（返回 error，调用方据此 fail-closed）。
func (m *Manager) resumeRuntimeWatchers(taskID, serveName string) error {
	tuiName := tuiSessionName(taskID)
	if alive, _ := m.proc.HasSession(tuiName); alive {
		if rt := m.getRuntime(taskID); rt != nil {
			rt.registerGroup("tui", tuiName)
		}
		m.watchTUIExit(taskID, tuiName)
	}
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		return fmt.Errorf("enumerate shells for resume watchers (task %s): %w", taskID, err)
	}
	for _, shellName := range shellNames {
		if rt := m.getRuntime(taskID); rt != nil {
			rt.registerGroup("shell", shellName)
		}
		m.watchShellExit(taskID, shellName)
	}
	return nil
}

// listRuntimeSessions 枚举该任务当前存在的会话。
// B7b：ListSessions 基础设施错误不得忽略——记录日志后按空列表处理
// （保守：枚举仅用于诊断/恢复判断，空列表不漏杀；infra 故障通过 reconcile 其他路径传播）。
func (m *Manager) listRuntimeSessions(taskID string) []string {
	sessions, err := m.proc.ListSessions()
	if err != nil {
		log.Printf("listRuntimeSessions: list sessions for task %s: %v", taskID, err)
		return nil
	}
	var out []string
	for _, s := range sessions {
		if taskIDFromSessionName(s) == taskID {
			out = append(out, s)
		}
	}
	return out
}

// serveHealthyRecoverable 判断 serve 是否健康可恢复（design.md §5）。
// 端口以会话内 OCDECK_SERVE_PORT 为准（读回失败 → 不可恢复，B9，不得静默用 last_port）。
func (m *Manager) serveHealthyRecoverable(ctx context.Context, t TaskRow) bool {
	serveName := serveSessionName(t.ID)
	pw, err := m.proc.ShowSessionEnv(serveName, "OPENCODE_SERVER_PASSWORD")
	if err != nil || pw == "" {
		return false
	}
	portStr, err := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
	if err != nil || portStr == "" {
		// 读回失败 → 不可恢复（B9，不得静默用 last_port）。
		return false
	}
	port, ok := parsePort(portStr)
	if !ok {
		return false
	}
	oc := m.ocFactory(port, pw, opencode.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
	h, err := oc.Health(ctx)
	if err != nil || !h.Healthy {
		return false
	}
	return true
}

// killTaskSessionsReconcile 在 reconcile 中 kill 任务的全部会话（记 notice）。
// KillSession 基础设施错误与 notice 写入错误 MUST 传播返回（design.md §5/§8，不静默）。
func (m *Manager) killTaskSessionsReconcile(ctx context.Context, taskID string, names []string) error {
	var errs []error
	for _, name := range names {
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			errs = append(errs, fmt.Errorf("has-session %s: %w", name, herr))
			continue
		}
		if !exists {
			continue
		}
		res, kerr := m.proc.KillSession(name)
		if kerr != nil {
			errs = append(errs, fmt.Errorf("kill session %s: %w", name, kerr))
			continue
		}
		if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, name, res); nerr != nil {
			errs = append(errs, nerr)
		}
	}
	return errors.Join(errs...)
}

// cleanupTaskRuntimeReconcile 完整运行时清理（design.md §5/§4，F4）：停 SSE/watcher + kill 残余会话。
// 用于 reconcile 中 serve 已死/健康失败、debt 阻塞恢复等需落 suspended 的场景，
// 确保 active/activating 状态不留无人托管运行时（watcher/SSE 必须收敛，不止 kill 会话）。
// 部分清理失败登记 residual_processes notice 进后台重试。错误聚合返回（不静默，design.md §8）。
func (m *Manager) cleanupTaskRuntimeReconcile(ctx context.Context, taskID string, names []string) error {
	// 停 SSE 订阅 + 退出监视（clearRuntime 内部 stopAll），收敛 in-process 监视 goroutine。
	m.clearRuntime(taskID)
	// kill 残余会话（记 notice）。
	kerr := m.killTaskSessionsReconcile(ctx, taskID, names)
	// 清除 env 快照（design.md §2：kill 模式 reconcile 落 suspended 清快照）。
	if serr := m.store.UpdateTaskEnvSnapshot(ctx, taskID, sql.NullString{}); serr != nil {
		return errors.Join(kerr, fmt.Errorf("clear env snapshot: %w", serr))
	}
	return kerr
}

// reconcileDebtPrePass cleanup-debt 预处理（design.md §5：先消化 debt 再决定恢复/kill）。
// retryTaskNotices 自身取得 keyed mutex（notice.go），pre-pass 调用方不持锁，
// 避免重入 tryLockTask 造成自死锁（Wave 1 之前 pre-pass 在锁内调用 retryTaskNotices，
// 后者再次 tryLockTask 必失败 → pre-pass 实际未消化任何 debt）。
// 拿不到锁（用户操作在执行）则 retryTaskNotices 返回 nil 跳过，留给后台周期重试。
// retryTaskNotices 返回的聚合 error 记录日志（不阻塞恢复决策）。
// 消化后仍有 debt 的任务由调用方（reconcilePersist）判定不恢复 active（F1）。
func (m *Manager) reconcileDebtPrePass(ctx context.Context, t TaskRow) {
	entries, err := parseNotices(t.Notice)
	if err != nil || len(entries) == 0 {
		return
	}
	// 仅在有 retryable notice 时尝试（避免无谓加锁）。
	hasRetryable := false
	for _, e := range entries {
		if e.Code == noticeCodeResidual {
			if r, ok := e.Data["retryable"].(bool); ok && r {
				hasRetryable = true
				break
			}
		}
	}
	if !hasRetryable {
		return
	}
	if rerr := m.retryTaskNotices(ctx, t, entries); rerr != nil {
		log.Printf("reconcile: debt pre-pass task %s: %v", t.ID, rerr)
	}
}

// reconcileDebtPrePassAll 全局 cleanup-debt 预处理（design.md §5：先消化 debt 再枚举/处理会话）。
// 对每个有 retryable notice 的任务调用 retryTaskNotices，聚合错误返回（不静默）。
// retryTaskNotices 内部取得 keyed mutex（避免与 reconcilePersist 的 tryLock 重入），
// 拿不到锁（用户操作在执行）返回 nil 跳过，留给后台周期重试。
func (m *Manager) reconcileDebtPrePassAll(ctx context.Context, tasks []TaskRow) error {
	var errs []error
	for _, t := range tasks {
		entries, perr := parseNotices(t.Notice)
		if perr != nil {
			// JSON 损坏：聚合错误（fail-closed），不静默。
			errs = append(errs, fmt.Errorf("task %s: parse notice: %w", t.ID, perr))
			continue
		}
		if len(entries) == 0 {
			continue
		}
		hasRetryable := false
		for _, e := range entries {
			if e.Code == noticeCodeResidual {
				if r, ok := e.Data["retryable"].(bool); ok && r {
					hasRetryable = true
					break
				}
			}
		}
		if !hasRetryable {
			continue
		}
		if rerr := m.retryTaskNotices(ctx, t, entries); rerr != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, rerr))
		}
	}
	return errors.Join(errs...)
}

// recordOrphanFailure 记录 orphan 清理失败到内存 orphanFailures 并立即持久化到 cleanup_debts
// （P4 复评阻塞 3b：不等 30s 周期，崩溃窗口即丢失）。
// P4 复评阻塞 3e：恢复项与同会话新失败项重复时持久化 MUST union tickets（persistOrphanDebts
// upsert 时读回既有 tickets 合并，不 latest-wins 覆盖）。
// B2/第三轮：persistOrphanDebt 持久化失败（List/Upsert）MUST 返回错误传播——
// recordOrphanFailure 返回该错误，killOrphanSession 聚合进返回值，Reconcile 孤儿循环
// 检查并传播给 main 据此拒开 HTTP（不得仅记日志吞没）。
func (m *Manager) recordOrphanFailure(ctx context.Context, name string, tickets []string) error {
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{sessionName: name, tickets: tickets})
	m.orphanMu.Unlock()
	if err := m.persistOrphanDebt(ctx, name, tickets); err != nil {
		log.Printf("record orphan failure: persist orphan debt %s: %v", name, err)
		return err
	}
	return nil
}

// killOrphanSession kill 孤儿会话（taskID 无 DB 行，design.md §5）。
// 失败进后台周期重试（非仅日志，B9），并聚合 kill 失败产生的 cleanupTickets 供后台 RetryReap（F3）。
// P4 复评阻塞 3d：HasSession infra 错误 MUST 处理（失败项进内存队列+持久化，不得静默跳过——
// Has 错误时会话可能仍存活，误判干净会让逃逸进程脱离重试，design.md §5/§8）。
// P4 复评阻塞 3b：新发现 orphan tickets MUST 立即持久化（不等 30s 周期，崩溃窗口即丢失）。
// B2/第三轮：返回错误聚合 kill 失败 + recordOrphanFailure 持久化失败，
// 供 reconcileKill 与 Reconcile 孤儿循环检查并传播（不得忽略返回值）。
func (m *Manager) killOrphanSession(ctx context.Context, name string) error {
	// HasSession infra 错误：保守视为存活（fail-closed），记录 orphan + 持久化。
	exists, herr := m.proc.HasSession(name)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		if perr := m.recordOrphanFailure(ctx, name, nil); perr != nil {
			log.Printf("reconcile: has orphan session %s infra error: %v; persist failed: %v", name, herr, perr)
			return fmt.Errorf("has orphan session %s: %w; persist orphan debt: %w", name, herr, perr)
		}
		log.Printf("reconcile: has orphan session %s infra error: %v (recorded for retry)", name, herr)
		return fmt.Errorf("has orphan session %s: %w", name, herr)
	}
	if !exists {
		return nil
	}
	res, err := m.proc.KillSession(name)
	if err != nil || (res.Disposition != "" && res.Disposition != process.DispositionClean) {
		log.Printf("reconcile: kill orphan session %s failed (disposition=%s err=%v)", name, res.Disposition, err)
		if perr := m.recordOrphanFailure(ctx, name, res.CleanupTickets); perr != nil {
			if err != nil {
				return fmt.Errorf("kill orphan session %s: %w; persist orphan debt: %w", name, err, perr)
			}
			return fmt.Errorf("kill orphan session %s: disposition %s; persist orphan debt: %w", name, res.Disposition, perr)
		}
		if err != nil {
			return fmt.Errorf("kill orphan session %s: %w", name, err)
		}
		return fmt.Errorf("kill orphan session %s: disposition %s", name, res.Disposition)
	}
	return nil
}

// persistOrphanDebt 持久化单个会话的 orphan tickets 到 cleanup_debts。
// P4 复评阻塞 3e：既有 debt 与新 tickets MUST union（读回既有 + 合并去重），不覆盖丢失。
// B2：List/Upsert 失败 MUST 返回错误（不再仅记日志），供 persistOrphanDebts 聚合传播。
func (m *Manager) persistOrphanDebt(ctx context.Context, name string, tickets []string) error {
	if m.debtStore == nil {
		return nil
	}
	// 读回既有 tickets（同会话恢复项 + 新失败项合并）。
	merged := tickets
	existing, err := m.debtStore.ListCleanupDebts(ctx)
	if err != nil {
		// B2：List 失败 MUST 返回错误（不静默）。仍用传入 tickets 作 merged，避免丢失本轮 tickets，
		// 但无法 union 既有——返回错误让调用方感知，下次重试可重新 union。
		log.Printf("persist orphan debt %s: list existing: %v (proceeding with new tickets only)", name, err)
		b, _ := json.Marshal(merged)
		if uerr := m.debtStore.UpsertCleanupDebt(ctx, name, string(b), nowUnixI()); uerr != nil {
			return fmt.Errorf("persist orphan debt %s: list existing: %w; upsert: %w", name, err, uerr)
		}
		return fmt.Errorf("persist orphan debt %s: list existing: %w", name, err)
	}
	for _, row := range existing {
		if row.SessionName == name {
			var old []string
			if jerr := json.Unmarshal([]byte(row.Tickets), &old); jerr == nil {
				merged = unionStringSlices(old, tickets)
			}
			break
		}
	}
	b, _ := json.Marshal(merged)
	if err := m.debtStore.UpsertCleanupDebt(ctx, name, string(b), nowUnixI()); err != nil {
		return fmt.Errorf("persist orphan debt %s: upsert: %w", name, err)
	}
	return nil
}

// groupSessionsByTask 按 taskID 分组 ocdeck-* 会话。
func groupSessionsByTask(sessions []string) map[string][]string {
	out := map[string][]string{}
	for _, s := range sessions {
		tid := taskIDFromSessionName(s)
		if tid == "" {
			continue
		}
		out[tid] = append(out[tid], s)
	}
	return out
}

// containsName 判断 names 是否含 name。
func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
