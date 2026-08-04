package task

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// initScriptTimeout 是 InitRunner 执行 init 脚本的超时上限（design.md §3：10min）。
const initScriptTimeout = 10 * time.Minute

// finishInitRunCtxTimeout 是 runnerCtx 取消后最终落账用的独立非取消 ctx 超时
// （design.md §6.1：5s Background）。
const finishInitRunCtxTimeout = 5 * time.Second

// createOutcome 是 Create/retryCreate 的二态内部结果（design.md §4）。
type createOutcome int

const (
	createDirectActivate createOutcome = iota // init_status=none → 锁外直接 triggerActivate
	createStartInit                           // init_status=pending → 锁外启动 InitRunner
)

// startInitRunner 锁外启动 InitRunner 异步执行（design.md §4 + §6.1）。
// 调用方 MUST 已释放 keyed mutex（避免 InitRunner 内 triggerActivate 自锁）。
func (m *Manager) startInitRunner(taskID string) {
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		// Shutdown 已开始：不启动新 InitRunner，任务保持 suspended+pending，
		// 下次启动 Reconcile 经 ConvergeInterruptedInitRuns 收敛为 failed（design.md §6.1）。
		log.Printf("init runner: task %s skipped (shutdown in progress)", taskID)
		return
	}
	m.runnerWG.Add(1)
	m.shutdownGateMu.Unlock()
	go m.runInitAttempt(taskID)
}

// runInitAttempt 执行一次 init 脚本尝试（design.md §4 + §6.1）。
// 流程：admission（已由 startInitRunner 登记 WG + gate 检查）→ ClaimInitRun CAS →
// 读配置快照执行 → FinishInitRun CAS 落账 → 仅 CAS rows=1 置 succeeded 后锁外 triggerActivate。
// runnerCtx 取消后的最终落账用独立短超时非取消 ctx（5s Background），仍在 WG 内。
func (m *Manager) runInitAttempt(taskID string) {
	defer m.runnerWG.Done()

	// ClaimInitRun CAS（§4：admission 后第一个 DB 操作）。
	claimed, err := m.store.ClaimInitRun(m.runnerCtx, taskID)
	if err != nil {
		log.Printf("init runner: claim task %s: %v", taskID, err)
		return
	}
	if !claimed {
		// 并发下另一执行者已 claim 或状态已变，不重复执行。
		return
	}

	// 读配置快照执行（§4：配置读取/env 合并/日志创建/脚本执行任一失败 → FinishInitRun failed）。
	initErr := m.executeInitScript(m.runnerCtx, taskID)

	// 最终落账 MUST 无条件使用独立短超时非取消 ctx（design.md §6.1）。
	// 不预检 runnerCtx.Err()——检查后取消仍漏（TOCTOU）；FinishInitRun/状态写一律走 Background+5s。
	finishCtx, finishCancel := context.WithTimeout(context.Background(), finishInitRunCtxTimeout)
	defer finishCancel()

	status := InitStatusSucceeded
	var initError sql.NullString
	if initErr != nil {
		status = InitStatusFailed
		initError = sql.NullString{String: initErr.Error(), Valid: true}
	}
	updated, ferr := m.store.FinishInitRun(finishCtx, taskID, status, initError)
	if ferr != nil {
		// DB error MUST NOT 激活（§4）。
		log.Printf("init runner: finish task %s: %v", taskID, ferr)
		return
	}
	if !updated {
		// rows=0：任务已被外部收敛（如服务重启），不激活（§4）。
		log.Printf("init runner: task %s init_status no longer running (externally converged)", taskID)
		return
	}
	// 仅 CAS rows=1 置 succeeded 后才锁外 triggerActivate（§4，避免 crud.go:93 同类自锁）。
	if status == InitStatusSucceeded {
		m.triggerActivate(taskID)
	}
}

// executeInitScript 读配置快照并执行 init 脚本（design.md §4）。
// 配置读取/env 合并/日志创建/脚本执行任一失败 → 返回 error（调用方 FinishInitRun(failed)）。
func (m *Manager) executeInitScript(ctx context.Context, taskID string) error {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	cfg, err := m.store.GetLifecycleConfig(ctx, row.ProjectID)
	if err != nil {
		return fmt.Errorf("read lifecycle config: %w", err)
	}
	// 空脚本仍经 RunScript 执行（`/bin/sh -c ""` 立即 exit 0）：保证"每次执行覆盖 init.log"
	// 的单一写入者契约——用户清空脚本 Re-run（escape hatch）时必须清除旧日志（tasks.md 3.6）。
	env, err := m.layerEnvSnapshot(ctx, row)
	if err != nil {
		return fmt.Errorf("layer env snapshot: %w", err)
	}
	if m.lifecycleRunner == nil {
		return fmt.Errorf("lifecycle runner not configured")
	}
	logPath := m.initLogPath(taskID)
	// 以 runnerCtx 执行脚本（§6.1：不复用 signal ctx，仅 Shutdown 关 gate 后取消）。
	if err := m.lifecycleRunner.RunScript(ctx, row.WorktreePath, env, cfg.InitScript, logPath, initScriptTimeout); err != nil {
		return fmt.Errorf("init script: %w", err)
	}
	return nil
}

// admitPreDelete 执行 pre-delete 脚本执行的 admission（design.md §6，tasks 3.9）。
// gate 已关闭 → 返回错误（调用方停止删除序列，绝不 wt.Remove，返回错误供 Retry）；
// 成功 → 登记 runnerWG，返回 release 函数（调用方在最终落账后调用恰好一次）。
func (m *Manager) admitPreDelete() (func(), error) {
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		return nil, fmt.Errorf("pre-delete: shutdown in progress")
	}
	m.runnerWG.Add(1)
	m.shutdownGateMu.Unlock()
	released := false
	return func() {
		if !released {
			released = true
			m.runnerWG.Done()
		}
	}, nil
}

// preDeleteScriptTimeout 是 pre-delete 脚本超时上限（design.md §6，2min）。
const preDeleteScriptTimeout = 2 * time.Minute

// finishDeletionCtxTimeout 是 pre-delete token 覆盖范围内落账（deletion_failed 写库）用的
// 独立短超时非取消 ctx（design.md §6.1，与 init 落账同一模式：5s Background）。
const finishDeletionCtxTimeout = 5 * time.Second

// runRerunInitAttempt 执行 rerun init 脚本尝试（design.md §4/§6，tasks 3.6）。
// 与 runInitAttempt 的区别：claim 条件不同（已由 RerunInit 调用 ClaimInitRerun），
// 成功不自动激活（§6）。wgRelease 由调用方传入，在最终状态写库之后调用（WG 覆盖完整 attempt）。
func (m *Manager) runRerunInitAttempt(taskID string, wgRelease func()) {
	defer wgRelease()

	initErr := m.executeInitScript(m.runnerCtx, taskID)

	// 最终落账 MUST 无条件使用独立短超时非取消 ctx（design.md §6.1）。
	finishCtx, finishCancel := context.WithTimeout(context.Background(), finishInitRunCtxTimeout)
	defer finishCancel()

	status := InitStatusSucceeded
	var initError sql.NullString
	if initErr != nil {
		status = InitStatusFailed
		initError = sql.NullString{String: initErr.Error(), Valid: true}
	}
	// 成功不自动激活（§6）：仅落账，不 triggerActivate。
	updated, ferr := m.store.FinishInitRun(finishCtx, taskID, status, initError)
	if ferr != nil {
		log.Printf("rerun init: finish task %s: %v", taskID, ferr)
		return
	}
	if !updated {
		log.Printf("rerun init: task %s init_status no longer running (externally converged)", taskID)
		return
	}
	// 成功不自动激活。
}

// runPreDeleteScript 读配置并同步执行 pre-delete 脚本（design.md §6）。
// 调用方（deleteResume）在二次 dirty 门禁后、wt.Remove 前调用。
// 配置读取/env 合并/脚本执行均用 m.runnerCtx（Manager 持有的非取消执行 ctx，design.md §6.1），
// 不用 HTTP request ctx——避免 request 取消时脚本被终止且落账用已取消 ctx 失败。
// 无 pre_delete_script → 返回 nil（跳过），调用方仍需释放 wg。
// env 合并/日志创建/脚本执行任一失败 → 返回 "pre-delete:" 前缀 error，调用方落 deletion_failed
// 且 MUST NOT 执行 wt.Remove。
func (m *Manager) runPreDeleteScript(row TaskRow) error {
	cfg, err := m.store.GetLifecycleConfig(m.runnerCtx, row.ProjectID)
	if err != nil {
		return fmt.Errorf("pre-delete: read lifecycle config: %w", err)
	}
	if cfg.PreDeleteScript == "" {
		return nil
	}
	env, err := m.layerEnvSnapshot(m.runnerCtx, row)
	if err != nil {
		return fmt.Errorf("pre-delete: layer env snapshot: %w", err)
	}
	if m.lifecycleRunner == nil {
		return fmt.Errorf("pre-delete: lifecycle runner not configured")
	}
	logPath := m.preDeleteLogPath(row.ID)
	// 以 runnerCtx 执行脚本（§6.1），wg 登记已由 admitPreDelete 持有到调用方最终落账后释放。
	if err := m.lifecycleRunner.RunScript(m.runnerCtx, row.WorktreePath, env, cfg.PreDeleteScript, logPath, preDeleteScriptTimeout); err != nil {
		return fmt.Errorf("pre-delete: script: %w", err)
	}
	return nil
}
