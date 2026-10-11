package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdecksess "ocdeck/internal/domain/session"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

// Delete 删除任务（design.md §19 Delete 行 + §12）。
// 全部静态检查（包含性/dirty/分支占用）先于任何副作用（B8）；mode ∈ {normal, force}。
// confirmDirty 表示用户已确认 dirty（API 层 confirmDirty=true）；未确认且 dirty → 拒绝（前置）。
// Force 跳过 oc session 删除与 pre-delete 脚本（project-lifecycle-config 扩展），dirty 检查与确认要求同 Normal（design.md §19：Force 不得自动 confirmDirty）。
func (m *Manager) Delete(ctx context.Context, taskID string, mode DeleteMode, confirmDirty bool) error {
	// D5：mode 校验——仅接受 normal|force，非法值 invalid_input（与 API 层校验一致，
	// task 层防御性校验，避免其他调用方绕过 API）。
	switch mode {
	case DeleteNormal, DeleteForce:
	default:
		return newOpErr(codeInvalidInput, fmt.Errorf("invalid delete mode %q (must be normal or force)", mode))
	}
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// task-info-editable D2 仲裁表：任务删除在存在未收敛改名意图时 conflict 拒绝
	//（不自动收敛——删除与改名意图语义冲突，要求先经信息修改入口收敛或人工修复
	// worktree；PreflightDelete 依赖 row.Branch）。意图存在即拒绝，不区分可否收敛。
	if pending, perr := m.readRenamePending(ctx, taskID); perr != nil {
		return newOpErr(codeInternal, perr)
	} else if pending != nil {
		return newOpErr(codeConflict, fmt.Errorf("task %s has unresolved rename pending; resolve it before delete", taskID))
	}
	// guard 委托 domain/task.CanDelete(mode)（design D0 P1.4.2 strangler 第二步）。
	// gating MUST 与持久化 delete_mode 一致（design.md §19）：
	//   - Normal：状态 ∈ {suspended, archived, creation_failed}（deletion_failed 不得直接重入 Normal 流程，
	//     必须经 Retry 按 persisted force mode 强制删除，避免 Normal 跳过已失败步骤的资源清理）。
	//   - Force：状态 ∈ {suspended, archived, creation_failed, deletion_failed}（强制删除 MUST 接受 deletion_failed）。
	//   - init 进行中两者均拒绝；未知 mode/未知 status fail-closed 拒绝。
	// 委托前后行为 byte-equivalent：guard 拒绝时按现状维度顺序生成错误（status/mode 优先，init 次之）。
	if !rehydrateGuardView(row).CanDelete(ocdecktask.DeleteMode(mode)) {
		if !deleteAllowedStatus(row.Status, mode) {
			return newOpErr(codeInvalidState, fmt.Errorf("delete not allowed from %s with mode %s", row.Status, mode))
		}
		// init_status 门禁（design.md tasks 3.7）：init 进行中拒绝删除。
		return newOpErr(codeInvalidState, fmt.Errorf("task %s init in progress (init_status=%s)", taskID, row.InitStatus))
	}

	// 解析任务有效模式（add-plain-dir-project D3 + add-local-path-task-mode D4）：
	// local-path（dir 任务与 repo 项目 local-path 模式任务）跳过 PreflightDelete / dirty 快照等
	// git 静态检查（无 git worktree/branch，包含性/dirty/分支占用门禁不适用，confirmDirty 接受但忽略）。
	// 非法 kind/mode 组合 fail-closed：首次 Delete 为未提交意图入口，在 BeginDeleteIntent
	// 与一切副作用之前返回，零副作用（状态不变）。
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("project not found: %w", err))
	}
	effMode, rerr := resolveTaskMode(row, proj.Kind)
	if rerr != nil {
		return newOpErr(codeInternal, rerr)
	}

	// B8：静态安全检查（包含性/dirty/分支占用）先于任何副作用（oc session/进程清理之前）。
	// dirty 检查与确认要求同 Normal 与 Force（design.md §19：Force 只跳过 oc session 删除，
	// dirty 检查与确认要求同 Normal——Force 不得自动 confirmDirty，仍需用户显式 confirmDirty）。
	// local-path 在上面已跳过（无 git）。
	var preflightDirty map[string]struct{}
	if effMode == TaskModeWorktree {
		if perr := m.wt.PreflightDelete(ctx, row.WorktreePath, PreflightDeleteOpts{
			RepoPath:     proj.Path,
			Branch:       row.Branch,
			ConfirmDirty: confirmDirty,
		}); perr != nil {
			return newOpErr(codeConflict, perr)
		}

		// B7c：捕获 preflight 时刻的 dirty 快照，供 deleteResume 在 wt.Remove 前做二次门禁——
		// preflight 后新产生的 dirty（未经确认）不得删。快照探测失败 MUST fail-closed：
		// DirtyFiles 错误意味着无法判定当前 dirty 集合，不得当空集强删用户数据。
		// 在删除意图提交前返回，状态不变（suspended 等），用户可排查后重试。
		snap, derr := m.wt.DirtyFiles(ctx, row.WorktreePath)
		if derr != nil {
			return newOpErr(codeGitError, fmt.Errorf("delete: preflight dirty snapshot: %w", derr))
		}
		preflightDirty = snap
	}

	// ① 持久化 delete_mode + 置 deleting（原子）。
	// C7 残余（review attempt 2）：删除准入——首次状态/进程副作用（deleting 提交）
	// 前按任务 provider 做 registry 准入（specs「Provider registry 分派」）。未注册
	// 任务零副作用拒绝删除（binding/归属/锚定/状态全不变；诊断性 last_error 允许）。
	// mutation 证据（已验证 → 红）：准入旁路（`if false &&`）→ TestC7_Delete
	// UnknownProviderZeroSideEffects 红（deleting 提交逃逸 + DeleteTask 执行）。
	if aerr := m.registryAdmitTask(ctx, taskID); aerr != nil {
		if isProviderNotFound(aerr) {
			m.recordAdmissionError(ctx, taskID, aerr)
		}
		return newOpErr(codeInternal, aerr)
	}
	updated, err := m.writeBeginDeleteIntent(ctx, taskID, string(mode), []string{
		StatusSuspended, StatusArchived, StatusCreationFailed, StatusDeletionFailed,
	})
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	if !updated.Matched {
		return newOpErr(codeConflict, fmt.Errorf("task %s not in deletable state", taskID))
	}
	return m.deleteResume(ctx, row, mode, preflightDirty)
}

// deleteResume 执行删除副作用序列（design.md §19/§12，add-plain-dir-project D3
// + add-local-path-task-mode D4）。
// 幂等：资源不存在视为已成功；按持久化 delete_mode 重入。
//
// 入口按任务有效模式（resolveTaskMode）一次性分流为 worktree/local-path 两条序列。
// 已提交意图的重入路径错误分层（tasks 2.1）：非法 kind/mode 组合仅落 deletion_failed +
// last_error，零破坏性副作用（不 wt.Remove、不 DeleteTask）。
// MUST NOT 依赖 Branch=="" 等隐式信号逐步跳过——有效模式即唯一分流依据。
//
// worktree 序列：retryDebt → oc sessions → kill 残余会话 → 二次 dirty 门禁 → pre-delete → wt.Remove → DB 删除 → 日志清理。
// local-path 序列：retryDebt → oc sessions → kill 残余会话 → pre-delete（cwd=项目目录）→ DB 删除 → 日志清理。
//   - local-path 跳过 PreflightDelete（前置）、preflight dirty 快照与二次门禁（DirtyFiles）、
//     wt.Remove、DeleteBranch；confirmDirty 接受但忽略（无 git，dirty 门禁不适用）。
//   - local-path force 与 worktree force 一致：跳过 ③⑥，不跳过 ②retryDebt。
//   - local-path 重入：preflightDirty 参数被忽略（永不读 dirty 快照），按持久化 delete_mode 重入同一序列。
//
// preflightDirty 为 worktree 路径的 DirtyFiles 快照，供 wt.Remove 前做二次门禁——preflight 后
// 新产生的 dirty（快照中不存在的条目）未经确认不得删（design.md §19）。nil 表示跳过二次门禁。
func (m *Manager) deleteResume(ctx context.Context, row TaskRow, mode DeleteMode, preflightDirty map[string]struct{}) error {
	taskID := row.ID

	// 入口分流（D3/D4）：按任务有效模式一次性分叉。非法 kind/mode 组合 fail-closed 落
	// deletion_failed + last_error，不执行任何破坏性操作（无 wt.Remove / DeleteTask）。
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		// 项目读取失败（含 ctx 取消——真实实现经 QueryRowContext 响应取消）：已提交意图的
		// 重入路径，落账用非取消有界 ctx 并检查结果（P1-F2），落账后终止删除序列。
		// 落账失败时 errors.Join 同时保留原始读取错误与落账错误，MUST NOT 吞错。
		readErr := newOpErr(codeNotFound, fmt.Errorf("project not found for delete resume: %w", err))
		if ferr := m.finalizeDeletionFailed(taskID, readErr.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(readErr, ferr))
		}
		return readErr
	}
	effMode, rerr := resolveTaskMode(row, proj.Kind)
	if rerr != nil {
		// 非法 kind/mode 组合（持久化损坏）→ internal（D1/D2）。已提交意图的重入路径：
		// 仅落 deletion_failed + last_error，随后 MUST 终止删除序列（零破坏性副作用：
		// 不 retryDebt、不删 session、不 kill、不 pre-delete、不 DeleteTask）。
		// 落账用非取消有界 ctx 并检查错误与 CAS（P1-F2）：落账失败时行停留 deleting，
		// MUST 将落账错误与原始解析错误一并返回供调用方感知，MUST NOT 吞错。
		if ferr := m.finalizeDeletionFailed(taskID, rerr.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(rerr, ferr))
		}
		return newOpErr(codeInternal, rerr)
	}
	if effMode == TaskModeWorktree {
		return m.deleteResumeRepo(ctx, row, mode, proj, preflightDirty)
	}
	return m.deleteResumeDir(ctx, row, mode)
}

// finalizeDeletionFailed 在 token 覆盖范围内（pre-delete WG token 持有期间）的落账 MUST 用
// 非取消 ctx（design.md §6.1）：request ctx 取消/Shutdown 时 UpdateTaskStatus 真实响应取消，
// 会留下 deleting 无落账。使用 context.Background() + finishDeletionCtxTimeout。
//
// 落账为 CAS 条件写（deleting → deletion_failed）：行已并发迁出 deleting（被其他 actor 收敛/
// 删除）时返回未命中错误，MUST NOT 无条件覆盖已收敛状态。返回落账结果供调用方检查（P1-F2）：
// 写错误或 CAS 未命中时返回错误，调用方 MUST 检查返回错误并与原始失败原因一并传播
// （errors.Join 保留双方，I-F1）。
func (m *Manager) finalizeDeletionFailed(taskID, lastError string) error {
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), finishDeletionCtxTimeout)
	defer finalizeCancel()
	le := sql.NullString{String: lastError, Valid: true}
	updated, err := m.writeStatusConditional(finalizeCtx, taskID, StatusDeleting, StatusDeletionFailed, le)
	if err != nil {
		return fmt.Errorf("finalize deletion_failed for task %s: %w", taskID, err)
	}
	if !updated.Matched {
		return fmt.Errorf("finalize deletion_failed for task %s: CAS not matched (status changed concurrently)", taskID)
	}
	return nil
}

// retryDebtGate 执行 ② RetryReap 既有 cleanup debt 门禁（design.md §19/§8）。
// remaining 非空 → deletion_failed，不得继续。JSON 损坏视为有 debt（fail-closed，B6）。
// casWriteNotices 失败 MUST 聚合进 last_error（P4 复评阻塞 2），保留 DB 既有 notice
// （CAS 失败=未覆盖，debt 仍在，下次 Retry 仍经门禁）。返回非 nil 表示门禁未通过（已落 deletion_failed）。
func (m *Manager) retryDebtGate(ctx context.Context, taskID string, notice sql.NullString) error {
	entries, perr := parseNotices(notice)
	if perr != nil {
		// I-F1：删除意图提交后的失败出口统一可靠落账（非取消有界 ctx + 检查写错误与 CAS）；
		// 落账失败 → internal 且 errors.Join 保留原错误。下同。
		orig := newOpErr(codeConflict, fmt.Errorf("task %s notice corrupted: %w", taskID, perr))
		if ferr := m.finalizeDeletionFailed(taskID, "notice json corrupted"); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}
	if !hasDebtTickets(entries) {
		return nil
	}
	remaining, derr := m.retryDebt(ctx, taskID, entries)
	if derr != nil {
		// B8：进程错误不得忽略 → deletion_failed。
		// remaining 含本轮新产生的 tickets（KillSession 合并的 CleanupTickets），
		// MUST CAS 写回，不得随 deletion_failed 丢失（逃逸进程下次 Retry 需 tickets 定位）。
		le := derr.Error()
		if cerr := m.casWriteNotices(ctx, taskID, remaining); cerr != nil {
			le = fmt.Sprintf("%s; cas write notices: %v", le, cerr)
		}
		orig := newOpErr(codeProcessError, derr)
		if ferr := m.finalizeDeletionFailed(taskID, le); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}
	// retryable 已清但仍有 degraded/overflow 时 MUST NOT 阻止 Delete（仅 retryable 阻止，
	// design.md §8/§19）。remaining 中的 degraded/overflow 项 CAS 写回后随删除流程丢弃（非逃逸进程 debt）。
	if hasDebtTickets(remaining) {
		// remaining MUST CAS 写回（新 tickets 不丢失，design.md §8/§19）。
		// casWriteNotices 失败 MUST 聚合进 last_error（P4 复评阻塞 2）。
		le := "cleanup debt not converged"
		if cerr := m.casWriteNotices(ctx, taskID, remaining); cerr != nil {
			le = fmt.Sprintf("cleanup debt not converged; cas write notices: %v", cerr)
		}
		orig := newOpErr(codeConflict, fmt.Errorf("task %s has uncleaned cleanup debt", taskID))
		if ferr := m.finalizeDeletionFailed(taskID, le); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}
	return nil
}

// deleteResumeRepo 执行 repo 项目的删除副作用序列（design.md §19/§12）。
// 保留全部不可回归点：非取消落账 ctx、pre-delete WG token 恰好一次释放、notice CAS 失败聚合、
// oc session 逐项错误聚合不短路、tickets 不随 CASCADE 丢失（wt.Remove 失败 MUST 落 deletion_failed 保留行）。
func (m *Manager) deleteResumeRepo(ctx context.Context, row TaskRow, mode DeleteMode, proj ProjectRow, preflightDirty map[string]struct{}) error {
	taskID := row.ID

	// ② RetryReap 既有 cleanup debt 门禁。
	if err := m.retryDebtGate(ctx, taskID, row.Notice); err != nil {
		return err
	}

	// ③ 删 oc sessions（逐个，404 幂等落账）。Force 跳过 ③。
	if mode != DeleteForce {
		if err := m.deleteOCSessions(ctx, row); err != nil {
			orig := newOpErr(codeProcessError, err)
			if ferr := m.finalizeDeletionFailed(taskID, fmt.Errorf("delete oc sessions: %w", err).Error()); ferr != nil {
				return newOpErr(codeInternal, errors.Join(orig, ferr))
			}
			return orig
		}
	}

	// ④ KillSession 残余会话（若有）。
	if err := m.killResidualSessions(ctx, taskID); err != nil {
		orig := newOpErr(codeProcessError, err)
		if ferr := m.finalizeDeletionFailed(taskID, err.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}

	// ⑤ 删 worktree + ⑥ 删本地分支。
	// GetProject 第二次调用失败 MUST NOT 跳过 worktree/branch 删除直接删 DB 行
	// （否则 tickets 随 CASCADE 丢失、worktree 残留）：落 deletion_failed + last_error。
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		orig := newOpErr(codeNotFound, fmt.Errorf("project not found for worktree removal: %w", err))
		if ferr := m.finalizeDeletionFailed(taskID, fmt.Errorf("get project for worktree removal: %w", err).Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}
	// B7c：二次 dirty 门禁——preflight 通过后，oc session/kill 残余会话期间若新产生 dirty
	// （快照中不存在的条目）未经确认，不得删（design.md §19）。
	// preflightDirty == nil 表示 Retry 重入（删除意图已提交），跳过门禁。
	if preflightDirty != nil {
		currentDirty, derr := m.wt.DirtyFiles(ctx, row.WorktreePath)
		if derr != nil {
			orig := newOpErr(codeGitError, fmt.Errorf("second dirty gate: %w", derr))
			if ferr := m.finalizeDeletionFailed(taskID, orig.Error()); ferr != nil {
				return newOpErr(codeInternal, errors.Join(orig, ferr))
			}
			return orig
		}
		for f := range currentDirty {
			if _, ok := preflightDirty[f]; !ok {
				orig := newOpErr(codeConflict, errors.New("worktree: new dirty files after preflight; confirm deletion again with confirmDirty=true"))
				if ferr := m.finalizeDeletionFailed(taskID, "new dirty files after preflight; confirm deletion again"); ferr != nil {
					return newOpErr(codeInternal, errors.Join(orig, ferr))
				}
				return orig
			}
		}
	}
	// ⑤.5 pre-delete 脚本挂点（design.md §6，tasks 3.9）：二次 dirty 门禁后、wt.Remove 前。
	// DeleteForce 跳过 pre-delete。worktree os.Stat 仅 IsNotExist → 跳过（其他 Stat 错误 → deletion_failed）。
	// 配置无 pre_delete_script → 跳过。admission 失败 → 停止删除序列、绝不 wt.Remove、返回错误供 Retry。
	// 执行失败 → deletion_failed + last_error 以 "pre-delete:" 前缀开头且 MUST NOT 执行 wt.Remove。
	// WG 登记持有到删除序列成功提交（DB 行删除）或 deletion_failed 落账之后（design.md §6.1）：
	// preDeleteRelease 由调用方在最终提交点/落账点后释放，而非脚本返回即释放。
	var preDeleteRelease func()
	if mode != DeleteForce {
		rel, perr := m.runPreDeleteHook(row)
		if perr != nil {
			// I-F1：检查落账结果——落账失败时行停留 deleting，MUST 与原始失败原因一并返回。
			ferr := m.finalizeDeletionFailed(taskID, perr.Error())
			if rel != nil {
				rel()
			}
			if ferr != nil {
				return newOpErr(codeInternal, errors.Join(perr, ferr))
			}
			return newOpErr(codeInvalidState, perr)
		}
		preDeleteRelease = rel
	}

	// preDeleteRelease 非 nil 时，本路径后续落账（wt.Remove 失败 / DB 删除失败）MUST 用非取消 ctx，
	// 并在落账完成后释放 token。用 defer 结构性保证恰好一次释放。
	if preDeleteRelease != nil {
		defer preDeleteRelease()
	}
	// finalizeOnFail 可靠落账闭包（I-F1）：返回落账错误，调用方 join 后升级 internal。
	finalizeOnFail := func(lastError string) error {
		return m.finalizeDeletionFailed(taskID, lastError)
	}

	if err := m.wt.Remove(ctx, row.WorktreePath, worktreeRemoveOpts{
		RepoPath:   proj.Path,
		Branch:     row.Branch,
		ForceDirty: true, // 删除已确认（前置 + 二次门禁通过），TaskManager 层强制清理
	}); err != nil {
		orig := fmt.Errorf("worktree remove: %w", err)
		if ferr := finalizeOnFail(orig.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return newOpErr(codeGitError, err)
	}

	// ⑨ 删 DB 行（提交点）。
	if _, err := m.writeDeleteTask(ctx, taskID); err != nil {
		orig := fmt.Errorf("delete db row: %w", err)
		if ferr := finalizeOnFail(orig.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return newOpErr(codeInternal, err)
	}
	// 提交点：defer preDeleteRelease() 已保证释放（§6.1：持有到 DB 行删除成功）。
	m.clearRuntime(taskID)
	// 删除成功（DB 行删除后）→ best-effort 删除 <dataDir>/logs/<taskID>/（忽略错误，design.md §6）。
	m.removeLifecycleLogDir(taskID)
	return nil
}

// deleteResumeDir 执行就地运行任务（dir 项目任务与 repo 项目 local-path 模式任务）的
// 删除副作用序列（add-plain-dir-project D3 + add-local-path-task-mode D4）。
//
// 硬不变量：ocdeck 内建逻辑对用户目录（worktree_path = 项目路径）零写删 syscall、零 git 命令
// （pre-delete 用户脚本为唯一例外）。
// 因此跳过 PreflightDelete（前置已在 Delete 入口跳过）、preflight dirty 快照与二次门禁（DirtyFiles）、
// wt.Remove、DeleteBranch。confirmDirty 接受但忽略（无 git，dirty 门禁不适用）。
//
// 步骤序列：② retryDebt → ③ oc sessions（!force）→ ④ kill 残余会话 → ⑥ pre_delete（!force，
// cwd=项目目录即 row.WorktreePath）→ ⑨ DeleteTask → ⑩ removeLifecycleLogDir + clearRuntime。
// force 跳过 ③⑥，不跳过 ②（与 worktree force 契约一致）。
//
// 保留全部不可回归点：非取消落账 ctx（finalizeDeletionFailed）、pre-delete WG token 恰好一次释放
// （defer preDeleteRelease）、notice CAS 失败聚合（retryDebtGate）、oc session 逐项错误聚合不短路
// （deleteOCSessions）、tickets 不随 CASCADE 丢失（dir 无 wt.Remove；DeleteTask 失败落 deletion_failed 保留行）。
func (m *Manager) deleteResumeDir(ctx context.Context, row TaskRow, mode DeleteMode) error {
	taskID := row.ID

	// ② RetryReap 既有 cleanup debt 门禁（local-path 与 worktree 共享，debt 收敛语义不因模式改变）。
	if err := m.retryDebtGate(ctx, taskID, row.Notice); err != nil {
		return err
	}

	// ③ 删 oc sessions（逐个，404 幂等落账，仅删本任务 task_sessions 拥有的 session）。
	// Force 跳过 ③（与 repo force 一致）。
	if mode != DeleteForce {
		if err := m.deleteOCSessions(ctx, row); err != nil {
			orig := newOpErr(codeProcessError, err)
			if ferr := m.finalizeDeletionFailed(taskID, fmt.Errorf("delete oc sessions: %w", err).Error()); ferr != nil {
				return newOpErr(codeInternal, errors.Join(orig, ferr))
			}
			return orig
		}
	}

	// ④ KillSession 残余会话（若有）。
	if err := m.killResidualSessions(ctx, taskID); err != nil {
		orig := newOpErr(codeProcessError, err)
		if ferr := m.finalizeDeletionFailed(taskID, err.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return orig
	}

	// ⑥ pre-delete 脚本挂点（dir：cwd=项目目录即 row.WorktreePath，design.md §6.1/D3）。
	// DeleteForce 跳过 pre-delete。保留 runPreDeleteHook 语义：Stat IsNotExist → 跳过（release=nil）；
	// 其他 Stat 错误 / admission 失败 / 脚本执行失败 → "pre-delete:" 前缀 deletion_failed，绝不 DeleteTask。
	// WG token 持有到 DB 提交点/落账点后恰好一次释放。
	var preDeleteRelease func()
	if mode != DeleteForce {
		rel, perr := m.runPreDeleteHook(row)
		if perr != nil {
			// I-F1：检查落账结果——落账失败时行停留 deleting，MUST 与原始失败原因一并返回。
			ferr := m.finalizeDeletionFailed(taskID, perr.Error())
			if rel != nil {
				rel()
			}
			if ferr != nil {
				return newOpErr(codeInternal, errors.Join(perr, ferr))
			}
			return newOpErr(codeInvalidState, perr)
		}
		preDeleteRelease = rel
	}

	// preDeleteRelease 非 nil 时，本路径后续落账（DB 删除失败）MUST 用非取消 ctx，
	// 并在落账完成后释放 token。用 defer 结构性保证恰好一次释放。
	if preDeleteRelease != nil {
		defer preDeleteRelease()
	}
	// finalizeOnFail 可靠落账闭包（I-F1）：返回落账错误，调用方 join 后升级 internal。
	finalizeOnFail := func(lastError string) error {
		return m.finalizeDeletionFailed(taskID, lastError)
	}

	// ⑨ 删 DB 行（提交点）。dir 不 wt.Remove——用户目录零写删（pre-delete 脚本为唯一例外）。
	if _, err := m.writeDeleteTask(ctx, taskID); err != nil {
		orig := fmt.Errorf("delete db row: %w", err)
		if ferr := finalizeOnFail(orig.Error()); ferr != nil {
			return newOpErr(codeInternal, errors.Join(orig, ferr))
		}
		return newOpErr(codeInternal, err)
	}
	// 提交点：defer preDeleteRelease() 已保证释放（§6.1：持有到 DB 行删除成功）。
	m.clearRuntime(taskID)
	// ⑩ 删除成功（DB 行删除后）→ best-effort 删除 <dataDir>/logs/<taskID>/（忽略错误，design.md §6）。
	m.removeLifecycleLogDir(taskID)
	return nil
}

// runPreDeleteHook 执行 pre-delete 脚本前置检查与执行（design.md §6，tasks 3.9）。
// 返回 (release, error)：release 为 WG token，调用方 MUST 在删除序列成功提交或 deletion_failed
// 落账之后释放（design.md §6.1：pre-delete WG 持有到提交点/落账点，非脚本返回即释放）。
// worktree os.Stat 仅 IsNotExist → 跳过（release 为 nil）；其他 Stat 错误 → "pre-delete:" 前缀 error。
// 读配置无 pre_delete_script → 脚本视为成功（admission 已登记，release 非 nil，随提交点释放）。admission 失败 → 返回错误（停止删除序列）。
// env 合并/日志创建/脚本执行任一失败 → 返回 "pre-delete:" 前缀 error（调用方落 deletion_failed，绝不 wt.Remove）。
func (m *Manager) runPreDeleteHook(row TaskRow) (func(), error) {
	// worktree os.Stat：仅 IsNotExist → 跳过；其他错误 → deletion_failed 前缀。
	if _, err := os.Stat(row.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			// worktree 不存在：幂等跳过 pre-delete（无脚本可执行的目标）。
			return nil, nil
		}
		return nil, fmt.Errorf("pre-delete: stat worktree: %w", err)
	}
	// admission（gate 检查 + runnerWG 登记）。release 返回给调用方，在提交点/落账点后释放。
	release, aerr := m.admitPreDelete()
	if aerr != nil {
		// admission 失败（Shutdown 进行中）：停止删除序列，绝不 wt.Remove。
		return nil, aerr
	}
	// 同步执行 pre-delete 脚本；失败时 release 由调用方在落 deletion_failed 后释放。
	if scriptErr := m.runPreDeleteScript(row); scriptErr != nil {
		// 返回 release（非 nil）让调用方在落 deletion_failed 后释放。
		return release, scriptErr
	}
	// 脚本成功：release 由调用方在最终提交点后释放。
	return release, nil
}

// deleteOCSessions 删除任务的 oc session 数据（逐个，404 幂等，design.md §12/§19）。
// 无活跃 serve 时起一次性 serve 会话执行删除。
// D3：deletion_failed 下逐项结果落账与错误聚合返回——每项 oc session 删除结果落账，
// 404 幂等成功，其他错误聚合后返回（不短路，避免只删了一半就落 deletion_failed）。
// 一次性 serve 的 KillSession 结果与 notice 写入错误 MUST 聚合返回（design.md §8/§19）：
// 非 clean disposition 或 notice 写入失败时返回非 nil，使 Delete 阻止越过 DB 提交点
// （返回 deletion_failed，notice 落库后才可重试，tickets 不随 CASCADE 丢失）。
func (m *Manager) deleteOCSessions(ctx context.Context, row TaskRow) error {
	sessions, err := m.store.ListTaskSessions(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("list task sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil
	}
	// 取得连接事实（opencode-v1-spi-provider tasks 4.3 / design D9 删除清理行 +
	// D10 例外行）：
	//   分支① 有持久化 binding 且 runtime 会话存活 → 直接复用 binding 端点/凭据；
	//   分支② 无可复用 runtime（无 binding 或 binding 解析失败/会话已终止）→
	//     AcquireEphemeral 内存临时绑定（不落库、不锚定、不建 SSE、不提交 active；
	//     陈旧 binding MUST NOT 走普通 Resume），临时 runtime 清理失败阻断任务删除。
	// tmux env 反查退役（特许例外仅迁移导入 runner 与本路径的临时 runtime 形态——
	// 凭据由 supervisor 派生注入，删除侧不经 env 读回）。
	port, password, serveName, ephemeral, ephemeralBinding, setupErr := m.deleteCleanupConnect(ctx, row)
	if setupErr != nil {
		return setupErr
	}
	var errs []error
	// ephemeralStartup 标记走了 AcquireEphemeral 分支：临时 runtime MUST 在 oc session
	// 删除完成后显式终止并聚合结果进 errs（不得用 defer——defer 在返回值计算后才追加，
	// 非 clean 清理错误不会进入返回值，调用方会越过 DB 提交点继续删 worktree/DB 致
	// tickets 丢失；D10：临时 runtime 终止非净或清理落账失败 MUST 阻断任务 DB 删除）。
	ephemeralStarted := ephemeral
	deleteProviderID := row.ProviderID
	if deleteProviderID == "" {
		deleteProviderID = store.ProviderIDV1
	}
	oc := m.ocFactory(deleteProviderID, port, password, appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	// D3：逐项删除，不短路。每项失败聚合到 errs，仍继续处理剩余项，
	// 使已成功删除的 session 行同步落账，避免只删了一半就中止。
	for _, s := range sessions {
		// 404 幂等：occlient 内部已把 404 转为 nil（视作已删除）。
		if err := oc.DeleteSession(ctx, row.WorktreePath, s.SessionID); err != nil {
			errs = append(errs, fmt.Errorf("delete session %s: %w", s.SessionID, err))
			continue
		}
		// oc 删除成功（含 404 幂等）→ 落账删除 DB session 行。DB 失败也聚合，不阻断后续项。
		// P1.4.5：注入 LifecycleService 时经 SessionRepository.DeleteOwned（commit helper
		// 调用位就绪，NoopPublisher 阶段）；未注入走 legacy 直连 store 路径。
		if m.lifecycle != nil {
			if _, derr := m.lifecycle.DeleteOwnedSession(ctx, row.ID, ocdecksess.ID(s.SessionID)); derr != nil {
				errs = append(errs, fmt.Errorf("delete session row %s: %w", s.SessionID, derr))
			}
			continue
		}
		if _, derr := m.store.DeleteTaskSession(ctx, row.ID, s.SessionID); derr != nil {
			errs = append(errs, fmt.Errorf("delete session row %s: %w", s.SessionID, derr))
		}
	}
	// 临时 runtime 清理（显式步骤，非 defer）：终止结果与 notice 写入错误
	// MUST 聚合进 errs，使 Delete 落 deletion_failed，tickets 落库后才可重试，
	// 不随后续 worktree/DB 删除的 CASCADE 永久丢失（design.md §8/§19 + D10 例外行）。
	// 非 clean disposition 或 kill 错误 MUST 阻止越过 DB 提交点。
	if ephemeralStarted {
		// 阶段 4 Release 接线（约束评估：经 Release 不破坏「临时 runtime 清理失败
		// 阻断删除」契约——CleanupStageError 映射后聚合进 errs 的路径与内联
		// KillSession 失败相同，D10 例外行语义不变；临时绑定即 AcquireEphemeral
		// 返回的内存 binding，Release 幂等消费 TerminalRef）。runtimeSupervisor
		// 未注入（内联测试接缝/golden 矩阵）保持内联实现。
		if m.runtimeSupervisor != nil {
			if rerr := m.runtimeSupervisor.Release(ctx, ephemeralBinding); rerr != nil {
				entry := releaseStageToKillEntry(serveName, rerr)
				if entry.killErr != nil {
					errs = append(errs, fmt.Errorf("kill temp runtime %s: %w", serveName, entry.killErr))
					if nerr := m.recordResidualNotice(ctx, row.ID, serveName, entry.result.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
						errs = append(errs, nerr)
					}
				} else {
					if nerr := m.recordResidualNoticeFromDisposition(ctx, row.ID, serveName, entry.result); nerr != nil {
						errs = append(errs, nerr)
					}
					if cls := classifyKillResult(entry.result); cls.action != "none" {
						errs = append(errs, fmt.Errorf("temp runtime %s cleanup not clean: %s", serveName, cls.reason))
					}
				}
			}
		} else {
			res, kerr := m.proc.KillSession(serveName)
			if kerr != nil {
				// kill 基础设施错误：记 notice（reason=kill_failed, retryable=true）。
				errs = append(errs, fmt.Errorf("kill temp runtime %s: %w", serveName, kerr))
				if nerr := m.recordResidualNotice(ctx, row.ID, serveName, res.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
					errs = append(errs, nerr)
				}
			} else {
				cls := classifyKillResult(res)
				if nerr := m.recordResidualNoticeFromDisposition(ctx, row.ID, serveName, res); nerr != nil {
					errs = append(errs, nerr)
				}
				if cls.action != "none" {
					errs = append(errs, fmt.Errorf("temp runtime %s cleanup not clean: %s", serveName, cls.reason))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// deleteCleanupConnect 按删除清理分支规则建立 oc session 删除的连接事实：
// 返回 (port, password, runtime 会话名, 是否临时 runtime, 临时绑定, error)。
// 临时绑定仅分支②返回（阶段 4 Release 接线：临时 runtime 终止经 Release 消费
// 该绑定；其余分支零值）。分支语义见 deleteOCSessions 头注释（design D9 删除
// 清理行 / D10 例外行）。
func (m *Manager) deleteCleanupConnect(ctx context.Context, row TaskRow) (int, string, string, bool, domain.TaskBinding, error) {
	runtimeName := runtimeSessionName(row.ID)
	serveName := runtimeName
	alive, _ := m.proc.HasSession(runtimeName)
	if !alive {
		if legacyAlive, _ := m.proc.HasSession(serveSessionName(row.ID)); legacyAlive {
			// legacy -serve 会话仍按既有清理矩阵处理（非 binding 路径——旧二进制
			// 存量形态属迁移导入 runner 特许域）。
			serveName = serveSessionName(row.ID)
			alive = true
		}
	}
	// 分支①：persisted binding 可解析 + runtime 会话存活/健康 → 复用 binding。
	if alive && serveName == runtimeName {
		if data, pw, rerr := m.resolveRuntimeBinding(ctx, row.ID); rerr == nil {
			// 复用前健康校验（陈旧 binding 的 runtime 可能已重启且凭据失效）。
			oc := m.ocFactory(data.ProviderID, data.Port, pw, appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 5 * time.Second})
			if _, herr := oc.Health(ctx); herr == nil {
				return data.Port, pw, runtimeName, false, domain.TaskBinding{}, nil
			}
		}
		// binding 陈旧/凭据失效 → 落分支②（MUST NOT 走普通 Resume 触发恢复）。
		alive = false
	}
	// legacy 分支（旧二进制 -serve 存量，binding 未覆盖；保持既有 env 反查——
	// 语义等同迁移导入桥接例外，删除路径不做行为收敛）。
	if alive && serveName != runtimeName {
		pw, err := m.proc.ShowSessionEnv(serveName, "OPENCODE_SERVER_PASSWORD")
		if err != nil || pw == "" {
			return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("recover serve password: %w", err)
		}
		portStr, perr := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
		if perr != nil || portStr == "" {
			return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("recover serve port for oc session delete: %w", perr)
		}
		port, ok := parsePort(portStr)
		if !ok {
			return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("invalid serve port %q for oc session delete", portStr)
		}
		return port, pw, serveName, false, domain.TaskBinding{}, nil
	}
	// 分支②：AcquireEphemeral 内存临时绑定（design D9a 分支②/D10）——经 registry
	// 分派 supervisor（未注册 fail-closed），端口+进程+健康；无锚定 bootstrap、无
	// claim、无对齐、无任务状态提交、不读/不写 env 快照与启动事实；返回值仅存内存
	//（MUST NOT 持久化）。陈旧 binding 不参与（MUST NOT 走普通 Resume 触发恢复）。
	if err := m.registryDispatch(store.ProviderIDV1); err != nil {
		return 0, "", "", false, domain.TaskBinding{}, err
	}
	if m.ephemeralSupervisor == nil {
		return 0, "", "", false, domain.TaskBinding{}, appagent.NewError("supervisor.acquire_ephemeral",
			appagent.ErrKindCapabilityUnavailable, errors.New("supervisor 未注入"))
	}
	binding, aerr := m.ephemeralSupervisor.AcquireEphemeral(ctx, appagent.TaskRef{
		TaskID:    row.ID,
		Directory: row.WorktreePath,
		Mode:      taskModeForRef(row),
	})
	if aerr != nil {
		return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("acquire ephemeral runtime for oc session delete: %w", aerr)
	}
	eddata, derr := decodeV1BindingData(binding)
	if derr != nil {
		return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("decode ephemeral binding: %w", derr)
	}
	epw, err2 := eddata.password()
	if err2 != nil {
		return 0, "", "", false, domain.TaskBinding{}, fmt.Errorf("resolve ephemeral credential: %w", err2)
	}
	return eddata.Port, epw, binding.TerminalRef, true, binding, nil
}

// taskModeForRef 解析 TaskRef 的 mode 字段（provider 契约值：worktree|local-path）。
func taskModeForRef(row TaskRow) string {
	if row.Mode == TaskModeLocalPath {
		return TaskModeLocalPath
	}
	return TaskModeWorktree
}

// killResidualSessions kill 残余 tmux 会话（serve/tui/shell）。
// B8：真实返回错误——kill/snapshot/reap 失败 → deletion_failed（不得继续删 worktree/DB 致 tickets 随 CASCADE 丢失）。
func (m *Manager) killResidualSessions(ctx context.Context, taskID string) error {
	// 客户端/shell 先清，runtime 与 leftover serve 后清；任一失败聚合，不 fail-fast。
	var names []string
	names = append(names, tuiSessionName(taskID))
	shellNames, err := m.listShellSessions(taskID)
	if err != nil {
		err = fmt.Errorf("kill residual sessions: enumerate shells (task %s): %w", taskID, err)
	} else {
		names = append(names, shellNames...)
	}
	names = append(names, runtimeSessionName(taskID), serveSessionName(taskID))
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, name := range names {
		// 阶段 4 Release 接线：agent runtime（binding.TerminalRef 对应会话）终止经
		// supervisor.Release（C7 provider 准入已在 Delete 入口/清理连接完成）；
		// CleanupStageError 映射回既有 notice/errs 通道（文本形态与内联一致）。
		// legacy -serve（特许域）与 tui/shell 保持内联；runtimeSupervisor 未注入
		// （内联测试接缝/golden 矩阵）保持内联实现。
		if name == runtimeSessionName(taskID) && m.runtimeSupervisor != nil {
			if rerr := m.runtimeSupervisor.Release(ctx, minimalReleaseBinding(taskID, name)); rerr != nil {
				entry := releaseStageToKillEntry(name, rerr)
				if entry.killErr != nil {
					if nerr := m.recordResidualNotice(ctx, taskID, name, entry.result.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
						errs = append(errs, fmt.Errorf("kill residual session %s: %w; record notice: %v", name, entry.killErr, nerr))
					} else {
						errs = append(errs, fmt.Errorf("kill residual session %s: %w", name, entry.killErr))
					}
					continue
				}
				if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, name, entry.result); nerr != nil {
					errs = append(errs, fmt.Errorf("kill residual session %s: record notice: %w", name, nerr))
				} else {
					errs = append(errs, fmt.Errorf("kill residual session %s: %s", name, classifyKillResult(entry.result).reason))
				}
			}
			continue
		}
		exists, herr := m.proc.HasSession(name)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			errs = append(errs, fmt.Errorf("kill residual sessions: has session %s: %w", name, herr))
			continue
		}
		if !exists {
			continue
		}
		res, kerr := m.proc.KillSession(name)
		if kerr != nil {
			if nerr := m.recordResidualNotice(ctx, taskID, name, res.CleanupTickets, noticeReasonKillFailed, true); nerr != nil {
				errs = append(errs, fmt.Errorf("kill residual session %s: %w; record notice: %v", name, kerr, nerr))
			} else {
				errs = append(errs, fmt.Errorf("kill residual session %s: %w", name, kerr))
			}
			continue
		}
		cls := classifyKillResult(res)
		if cls.action == "none" {
			continue
		}
		if nerr := m.recordResidualNoticeFromDisposition(ctx, taskID, name, res); nerr != nil {
			errs = append(errs, fmt.Errorf("kill residual session %s: %s; record notice: %w", name, cls.reason, nerr))
		} else {
			errs = append(errs, fmt.Errorf("kill residual session %s: %s", name, cls.reason))
		}
	}
	return errors.Join(errs...)
}

// hasDebtTickets 判断 notice 是否有可重试进程 debt（B8：仅 retryable residual_processes 阻止删除；
// overflow/degraded 不阻止删除）。
func hasDebtTickets(entries []noticeEntry) bool {
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		r, ok := e.Data["retryable"]
		if !ok {
			continue
		}
		if b, ok := r.(bool); ok && b {
			return true
		}
	}
	return false
}

// retryDebt 重试 cleanup debt，返回仍剩余的 notice 项与进程错误（B8：仅 retryable 进程 debt，
// 进程错误不得忽略）。overflow/degraded 不阻止删除，保留但不重试。
func (m *Manager) retryDebt(ctx context.Context, taskID string, entries []noticeEntry) ([]noticeEntry, error) {
	var remaining []noticeEntry
	for i, e := range entries {
		if e.Code != noticeCodeResidual {
			// overflow/degraded 不阻止删除，保留但不重试。
			remaining = append(remaining, e)
			continue
		}
		retryable, _ := e.Data["retryable"].(bool)
		if !retryable {
			remaining = append(remaining, e)
			continue
		}
		sessionName, _ := e.Data["sessionName"].(string)
		tickets := noticeTickets(e)
		if sessionName != "" {
			exists, herr := m.proc.HasSession(sessionName)
			if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
				// B8：infra 错误不得忽略。返回值 MUST 包含当前 entry + 全部后续未处理 entry，
				// 不得原子清空未处理 debt 致下次 Retry 越过门禁（P4 复评阻塞 2）。
				return appendRemaining(remaining, entries[i:]), fmt.Errorf("has session %s: %w", sessionName, herr)
			}
			if exists {
				res, kerr := m.proc.KillSession(sessionName)
				if kerr != nil {
					// B8：kill 错误不得忽略。同样保留当前 + 后续未处理 entry。
					return appendRemaining(remaining, entries[i:]), fmt.Errorf("kill session %s: %w", sessionName, kerr)
				}
				cls := classifyKillResult(res)
				if cls.action != "none" {
					tickets = append(tickets, res.CleanupTickets...)
					e.Data["cleanupTickets"] = tickets
					e.Data["reason"] = cls.reason
					e.Data["retryable"] = cls.retryable
					remaining = append(remaining, e)
					continue
				}
			}
		}
		if len(tickets) > 0 {
			left, rerr := m.proc.RetryReap(tickets)
			if rerr != nil {
				// B8：reap 错误不得忽略。同样保留当前 + 后续未处理 entry。
				return appendRemaining(remaining, entries[i:]), fmt.Errorf("retry reap: %w", rerr)
			}
			if len(left) > 0 {
				e.Data["cleanupTickets"] = left
				remaining = append(remaining, e)
				continue
			}
		}
	}
	return remaining, nil
}

// appendRemaining 将 unprocessed（当前报错 entry + 其后全部 entry）追加到 already，
// 保证 retryDebt 错误返回值不丢未处理 debt（P4 复评阻塞 2：下次 Retry 仍经门禁）。
// unprocessed 项原样保留（含 retryable/reason/tickets），调用方据此 CAS 写回。
func appendRemaining(already, unprocessed []noticeEntry) []noticeEntry {
	out := make([]noticeEntry, 0, len(already)+len(unprocessed))
	out = append(out, already...)
	out = append(out, unprocessed...)
	return out
}

// deleteAllowedStatus 判断状态是否允许删除（gating 与 delete_mode 一致，design.md §19）。
// Normal：deletion_failed 不得直接重入 Normal 流程（必须经 Retry 按持久化 force mode 强删）。
// Force：接受 deletion_failed（兑现"删除失败后可强制删除"）。
//
// P1.4.2 strangler 第二步后，Delete guard 的决策权威迁至 domain/task.CanDelete(mode)
// （见 Delete 入口）。本函数仅保留用于 guard 拒绝时的错误消息维度选择——status/mode
// 维度先于 init 维度报错（byte-equivalent），因此拒绝路径仍需判定是否 status/mode
// 维度失败以生成对应消息。逻辑与 domain CanDelete 的 status/mode 子判定等价。
func deleteAllowedStatus(status string, mode DeleteMode) bool {
	switch status {
	case StatusSuspended, StatusArchived, StatusCreationFailed:
		return true
	case StatusDeletionFailed:
		return mode == DeleteForce
	}
	return false
}
