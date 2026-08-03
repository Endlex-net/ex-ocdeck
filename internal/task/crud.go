package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// Create 在项目下创建任务：生成 worktree + 分支，落库（design.md §19 Create 行）。
// 分支名 ocdeck/<task-name-slug>，worktree 路径 <dataDir>/worktrees/<projectID>/<taskID>/。
func (m *Manager) Create(ctx context.Context, projectID, taskName string) (TaskRow, error) {
	if strings.TrimSpace(taskName) == "" {
		return TaskRow{}, newOpErr(codeInvalidInput, errors.New("task name is required"))
	}
	// 项目存在性检查。
	proj, err := m.store.GetProject(ctx, projectID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("project not found: %w", err))
	}

	taskID := newTaskID()
	branch := "ocdeck/" + slugify(taskName)
	wtPath := m.worktreePath(projectID, taskID)

	// P1：Create 前置检查（design.md §19：项目存在、分支名 check-ref-format、分支名不冲突）
	// MUST 全部完成于插入 creating 行之前——先插行后查分支冲突时，前置失败会残留
	// creation_failed 行。调整顺序：全部前置检查（无副作用）→ 插入 creating → worktree add。
	repoPath := proj.Path
	// 分支名合法性校验（check-ref-format）：无副作用，前置完成于落库前。
	if err := m.wt.ValidateBranchName(ctx, repoPath, branch); err != nil {
		return TaskRow{}, newOpErr(codeInvalidInput, fmt.Errorf("invalid branch name %q: %w", branch, err))
	}
	// 分支冲突检查（B1）：分支已存在 → 拒绝，不残留 creation_failed 行。
	if exists, err := m.wt.BranchExists(ctx, repoPath, branch); err != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("branch existence check: %w", err))
	} else if exists {
		return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("branch %s already exists", branch))
	}

	// ① 意图落库：插入任务行（status=creating）。
	if err := m.store.CreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: projectID, Name: taskName,
		Branch: branch, Status: StatusCreating, WorktreePath: wtPath,
	}); err != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("create task row: %w", err))
	}

	// ② worktree add（仓库级写锁在 worktree.Add 内）。
	if _, err := m.wt.Add(ctx, repoPath, projectID, taskID, branch, proj.DefaultBranch); err != nil {
		// worktree add 失败 → creation_failed（前置检查已通过，失败发生在副作用阶段）。
		le := sql.NullString{String: fmt.Errorf("worktree add: %w", err).Error(), Valid: true}
		_ = m.store.UpdateTaskStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeGitError, fmt.Errorf("worktree add: %w", err))
	}

	// ④ 提交点：status=suspended。
	if err := m.store.UpdateTaskStatus(ctx, taskID, StatusSuspended, sql.NullString{}); err != nil {
		// worktree 已建但 DB 写失败 → creation_failed（design.md §19）。
		le := sql.NullString{String: fmt.Errorf("update status: %w", err).Error(), Valid: true}
		_ = m.store.UpdateTaskStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeInternal, fmt.Errorf("commit status: %w", err))
	}

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{}, newOpErr(codeInternal, err)
	}
	// ④ 自动触发激活（异步，design.md §19 Create 行）：等价于用户手动 Activate，
	// 走同一入口（全部门禁 + session 锚定），挂 Manager 生命周期 ctx（不随 Create 请求 ctx 结束）。
	// Create 立即返回 suspended，前端轮询看到激活推进；失败落 suspended+last_error 可手动重试。
	m.triggerActivate(taskID)
	return row, nil
}

// Retry 对 creation_failed/deleting/deletion_failed 重入（design.md §18/§19，幂等）。
// creation_failed → 重新 worktree add（已存在则跳过）→ suspended；
// deleting/deletion_failed → 按持久化 delete_mode 重入删除序列。
//
// confirmDirty 仅作用于删除重试（B1）：Retry 的 dirty 门禁 MUST 与首次 Delete 一致——
// 取当前 dirty 集合，非空则要求调用方显式 confirmDirty=true（与首次相同的用户确认），
// 不得静默以新快照为基线随后 ForceDirty 强删未确认文件。confirmDirty=true 才继续
// "从失败步骤继续"的剩余步骤。
func (m *Manager) Retry(ctx context.Context, taskID string, confirmDirty bool) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	// B1：retryCreate 成功后需在释放 keyed mutex 后触发自动激活（杜绝 goroutine 自锁竞态），
	// 故用 unlocked flag 防止 defer 与显式 unlock 重复解锁。
	unlocked := false
	unlockOnce := func() {
		if !unlocked {
			unlocked = true
			unlock()
		}
	}
	defer unlockOnce()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	switch row.Status {
	case StatusCreationFailed:
		// B1：自动激活 MUST 在 keyed mutex 释放后启动（retryCreate 内不触发），
		// 否则 goroutine 在 Retry 持锁期间调 Activate → tryLockTask 409 busy →
		// 永久 suspended 无 last_error。
		shouldActivate, rerr := m.retryCreate(ctx, row)
		if rerr != nil {
			return rerr
		}
		// 先显式解锁再调度自动激活 goroutine，保证 Activate 的 tryLockTask 不自锁。
		unlockOnce()
		if shouldActivate {
			m.triggerActivate(row.ID)
		}
		return nil
	case StatusDeleting, StatusDeletionFailed:
		mode := DeleteNormal
		if row.DeleteMode.Valid && row.DeleteMode.String == string(DeleteForce) {
			mode = DeleteForce
		}
		// B8：delete_mode 不得被 Normal 重试覆盖；按持久化 delete_mode 重入。
		// 先置 deleting 再执行（deletion_failed → deleting，design.md §19/§8）。
		if row.Status == StatusDeletionFailed {
			if err := m.store.SetTaskDeleteMode(ctx, row.ID, string(mode)); err != nil {
				return newOpErr(codeInternal, err)
			}
			if err := m.store.UpdateTaskStatus(ctx, row.ID, StatusDeleting, sql.NullString{}); err != nil {
				return newOpErr(codeInternal, err)
			}
		}
		// P0/B1：Retry 重入删除序列 MUST NOT 传 nil 跳过二次 dirty 门禁，也不得以新快照
		// 为基线把首次未确认文件纳入"已确认"随后 ForceDirty 强删。正确语义与首次 Delete
		// 一致：取当前 dirty 集合，非空则要求调用方显式 confirmDirty=true；confirmDirty=false
		// 且非空 → 拒绝（409，提示需确认），不进入 deleteResume。
		// 快照探测失败 MUST fail-closed（与首次 Delete 一致）：DirtyFiles 错误意味着无法
		// 判定当前 dirty 集合，不得当空集强删用户数据。
		preflightDirty, derr := m.wt.DirtyFiles(ctx, row.WorktreePath)
		if derr != nil {
			le := sql.NullString{String: fmt.Errorf("retry: preflight dirty snapshot: %w", derr).Error(), Valid: true}
			_ = m.store.UpdateTaskStatus(ctx, row.ID, StatusDeletionFailed, le)
			return newOpErr(codeGitError, fmt.Errorf("retry: preflight dirty snapshot: %w", derr))
		}
		if len(preflightDirty) > 0 && !confirmDirty {
			le := sql.NullString{String: "retry: worktree has dirty files; confirm deletion again", Valid: true}
			_ = m.store.UpdateTaskStatus(ctx, row.ID, StatusDeletionFailed, le)
			return newOpErr(codeConflict, errors.New("worktree: retry delete has dirty files; confirm deletion again with confirmDirty=true"))
		}
		return m.deleteResume(ctx, row, mode, preflightDirty)
	default:
		return newOpErr(codeInvalidState, fmt.Errorf("task %s not in retryable state (status=%s)", taskID, row.Status))
	}
}

// retryCreate 重试创建：严格产物验证（design.md §19 Create Retry 行原文）——
// 路径存在 + .git 文件 + rev-parse --is-inside-work-tree + 检出分支匹配 + 属预期 repo，
// 全部通过才跳过 add；否则重新 add。
// 返回 shouldActivate=true 表示创建已提交 suspended、调用方应在释放 keyed mutex 后触发自动激活
//（B1：retryCreate 自身不触发，避免在 Retry 持锁期间 goroutine 调 Activate 自锁）。
func (m *Manager) retryCreate(ctx context.Context, row TaskRow) (bool, error) {
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return false, newOpErr(codeInternal, fmt.Errorf("project gone during retry: %w", err))
	}
	// 严格产物验证：通过则跳过 add，否则重新 add。
	if err := m.wt.VerifyWorktreeProduct(ctx, proj.Path, row.WorktreePath, row.Branch); err != nil {
		// 产物不完整或不存在 → 重新 add。先检查分支冲突（B1）。
		if exists, berr := m.wt.BranchExists(ctx, proj.Path, row.Branch); berr != nil {
			le := sql.NullString{String: fmt.Errorf("branch existence check: %w", berr).Error(), Valid: true}
			_ = m.store.UpdateTaskStatus(ctx, row.ID, StatusCreationFailed, le)
			return false, newOpErr(codeInternal, fmt.Errorf("branch existence check: %w", berr))
		} else if exists {
			le := sql.NullString{String: fmt.Errorf("branch %s already exists", row.Branch).Error(), Valid: true}
			_ = m.store.UpdateTaskStatus(ctx, row.ID, StatusCreationFailed, le)
			return false, newOpErr(codeConflict, fmt.Errorf("branch %s already exists", row.Branch))
		}
		if _, err := m.wt.Add(ctx, proj.Path, row.ProjectID, row.ID, row.Branch, proj.DefaultBranch); err != nil {
			le := sql.NullString{String: fmt.Errorf("retry worktree add: %w", err).Error(), Valid: true}
			_ = m.store.UpdateTaskStatus(ctx, row.ID, StatusCreationFailed, le)
			return false, newOpErr(codeGitError, err)
		}
	}
	// 提交点：suspended。
	if err := m.store.UpdateTaskStatus(ctx, row.ID, StatusSuspended, sql.NullString{}); err != nil {
		return false, newOpErr(codeInternal, err)
	}
	// 创建完成即激活（语义与 Create 一致，design.md §19）：由调用方在解锁后触发。
	return true, nil
}

// Archive 归档（design.md §19 Archive 行：纯 DB，状态须为 suspended）。
func (m *Manager) Archive(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.Status != StatusSuspended {
		return newOpErr(codeInvalidState, fmt.Errorf("archive requires suspended, got %s", row.Status))
	}
	if err := m.store.ArchiveTask(ctx, taskID); err != nil {
		return newOpErr(codeInternal, err)
	}
	return nil
}

// Restore 从归档恢复挂起（design.md §19 Restore 行：纯 DB，状态须为 archived）。
func (m *Manager) Restore(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.Status != StatusArchived {
		return newOpErr(codeInvalidState, fmt.Errorf("restore requires archived, got %s", row.Status))
	}
	if err := m.store.RestoreTask(ctx, taskID); err != nil {
		return newOpErr(codeInternal, err)
	}
	return nil
}

// Get 返回任务详情（design.md §18 Get）。
func (m *Manager) Get(ctx context.Context, taskID string) (TaskRow, error) {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	return row, nil
}

// List 返回项目下任务列表（design.md §18 List）。
func (m *Manager) List(ctx context.Context, projectID string) ([]TaskRow, error) {
	rows, err := m.store.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, newOpErr(codeInternal, err)
	}
	return rows, nil
}

// slugify 将任务名转为分支 slug（小写、非 [a-z0-9-] 替换为 -、折叠连续 -、去首尾 -）。
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := true // 首字符也禁止 -，等价于首字符前视为 -
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}

// worktreePath 返回 <dataDir>/worktrees/<projectID>/<taskID>（与 worktree.Manager 约定一致）。
func (m *Manager) worktreePath(projectID, taskID string) string {
	return m.cfg.DataDir + "/worktrees/" + projectID + "/" + taskID
}

// triggerActivate 异步触发自动激活（design.md §19 Create/Retry 行 ④）。
// 挂 Manager 生命周期 ctx（非调用方请求 ctx），保证 Create/Retry 请求返回后激活仍推进。
// 走与手动 Activate 完全相同的入口：同一 keyed mutex、全部门禁、session 锚定；
// 失败已由 Activate 落 suspended+last_error（可手动重试），此处仅记日志，不向调用方传播错误。
// 并发手动 Activate 与本触发经 keyed mutex 互斥：一方获胜推进，另一方返回 409（符合 design.md §1）。
//
// B2：Shutdown 准入 gate——Shutdown 开始后拒绝新自动激活触发（autoActivateWG 登记本 goroutine，
// Shutdown 等待已登记触发收尾后再清理，消灭 kill 模式 shutdown 枚举后再建 tmux 会话 / persist 模式
// Shutdown 返回后继续注册 runtime/访问 store 的窗口）。
// B1：409 busy 仅记日志（表示并发手动 Activate 正在推进，非"卡 suspended 无错误"）；
// 其他失败已由 Activate 落 suspended+last_error。
func (m *Manager) triggerActivate(taskID string) {
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		// Shutdown 已开始：不再触发自动激活，任务保持 suspended，用户可手动激活。
		log.Printf("auto-activate after create: task %s skipped (shutdown in progress)", taskID)
		return
	}
	m.autoActivateWG.Add(1)
	m.shutdownGateMu.Unlock()
	go func() {
		defer m.autoActivateWG.Done()
		if err := m.Activate(m.lifecycleCtx(), taskID); err != nil {
			// Activate 失败已落 suspended+last_error；记录日志供运维感知自动激活未成功。
			// 409 busy 表示并发手动 Activate 正在推进（良性，非"卡住"）。
			log.Printf("auto-activate after create: task %s: %v", taskID, err)
		}
	}()
}
