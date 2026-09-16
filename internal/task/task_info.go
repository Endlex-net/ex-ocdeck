// task_info.go 实现任务信息修改用例与改名恢复协议（openspec change task-info-editable
// Phase 2：design D1/D2/D3 + tasks 2.2/2.3）。
//
// 核心结构：
//   - UpdateTaskInfo：两阶段保存——阶段一「历史意图收敛」（R1），阶段二「本次修改」
//     （零副作用前置校验 → 快照预计算 → git 序列（意图写入 → branch -m → 单事务提交）
//     或纯名称路径（无 git 副作用、无意图记录））。
//   - convergeRenamePending（R1）：按 worktree 实际 HEAD 三分支判定，启动 reconcile 与
//     各生命周期入口共用同一收敛函数。
//   - 仲裁接线（D2 仲裁表）：Suspend/Activate/运行期 Recovery 在首个状态写入前、启动
//     reconcile 在生命周期恢复/清理前、kill 模式 Shutdown 在进程终止前先执行 R1。
//
// rename_pending 仅由内部 store 行/本文件意图结构消费，MUST NOT 进入公共 Task DTO。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/git"
)

// UpdateTaskInfoOptions 任务信息修改请求选项（task-info-editable D1）——定义迁至
// application（dto.go，锁定 api → application import 方向，与 CreateTaskOptions 同型）；
// 此处保留别名，本包及既有引用零改动。字段 presence 语义见该类型文档。
type UpdateTaskInfoOptions = application.UpdateTaskInfoOptions

// renamePendingIntent 是 tasks.rename_pending 的 JSON 载荷（design D2 步骤 5）：
// 同次保存的完整目标值——恢复单元 = 提交单元，R1 仅凭意图即可经 CommitTaskInfoUpdate
// 重建整次本地提交。Name/EnvSnapshot 为 JSON null 时表示该字段不在本次保存目标内
// （对应 application.TaskInfoUpdate presence 语义：不修改）。
type renamePendingIntent struct {
	Name        *string `json:"name"`
	BranchOld   string  `json:"branch_old"`
	BranchNew   string  `json:"branch_new"`
	EnvSnapshot *string `json:"env_snapshot"`
}

// branchWithOriginalPrefix 沿用当前分支的原有前缀构造新分支名（task-info-editable D2）：
// 前缀 = 当前分支名最后一个 / 之前的部分；当前分支无 / 时新分支名即 slug 本身。
// MUST NOT 使用全局前缀配置的当前值。
func branchWithOriginalPrefix(currentBranch, slug string) string {
	if i := strings.LastIndex(currentBranch, "/"); i >= 0 {
		return currentBranch[:i] + "/" + slug
	}
	return slug
}

// UpdateTaskInfo 修改任务名称/分支 slug（task-info-editable D1 请求处理顺序，严格）：
// 任务锁内读取 → 阶段一 R1 收敛（存在意图时；成功后重读当前值）→ presence/字段矩阵校验 →
// 含分支变更路径：repo 写锁（proj.Path）→ 锁内 HEAD 身份验证 + 目标复查（任一失败在意图
// 写入前拒绝，零副作用）→ 意图写入 → git branch -m → CommitTaskInfoUpdate 单事务提交 →
// 失败补偿；纯名称路径：快照矩阵预计算 → 单事务提交 → 标题同步后处理。
// 全部同值/未提供 → 幂等成功零变更。名称修改不受状态门禁；分支改名仅稳定状态且分支非空。
func (m *Manager) UpdateTaskInfo(ctx context.Context, taskID string, opts UpdateTaskInfoOptions) (TaskRow, error) {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return TaskRow{}, err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("project not found: %w", err))
	}

	// 阶段一：历史意图收敛（D1/D2 两阶段语义）。不可收敛 → conflict，本次修改不执行；
	// 收敛成功 → 重读任务当前值，本阶段二全部"原值"断言以收敛后状态为基准。
	pending, err := m.readRenamePending(ctx, taskID)
	if err != nil {
		return TaskRow{}, newOpErr(codeInternal, err)
	}
	if pending != nil {
		if cerr := m.convergeRenamePending(ctx, row, proj, *pending); cerr != nil {
			return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("task %s rename pending not converged: %w", taskID, cerr))
		}
		if row, err = m.store.GetTask(ctx, taskID); err != nil {
			return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("reread task after converge: %w", err))
		}
	}

	// 阶段二：presence/字段矩阵校验（D1 步骤 3-5，全部零副作用前置）。
	nameChanged := false
	var newName string
	if opts.Name != nil {
		if strings.TrimSpace(*opts.Name) == "" {
			return TaskRow{}, newOpErr(codeInvalidInput, errors.New("task name must not be blank"))
		}
		newName = *opts.Name // 原值存储（保留首尾空白）
		nameChanged = newName != row.Name
	}
	branchChanged := false
	var newBranch string
	if opts.BranchSlug != nil {
		// trim 后为空视为未提供（不进入分支改名路径）。
		if slug := strings.TrimSpace(*opts.BranchSlug); slug != "" {
			newBranch = branchWithOriginalPrefix(row.Branch, slug)
			// slug 换算出的新分支名等于当前分支 → 同值跳过（不进 git 路径，不触发状态门禁）。
			branchChanged = newBranch != row.Branch
		}
	}
	if branchChanged {
		// 状态门禁：仅稳定状态（active/suspended/archived）可改名。
		switch row.Status {
		case StatusActive, StatusSuspended, StatusArchived:
		default:
			return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("branch rename not allowed from %s", row.Status))
		}
		// gitless：分支记录为空（dir/local-path 任务）拒绝。
		if row.Branch == "" {
			return TaskRow{}, newOpErr(codeInvalidState, errors.New("task has no branch (dir/local-path task)"))
		}
		if verr := m.wt.ValidateBranchName(ctx, proj.Path, newBranch); verr != nil {
			return TaskRow{}, newOpErr(codeInvalidInput, fmt.Errorf("invalid branch name %q: %w", newBranch, verr))
		}
		if exists, eerr := m.wt.BranchExists(ctx, proj.Path, newBranch); eerr != nil {
			return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("branch existence check: %w", eerr))
		} else if exists {
			return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("branch %s already exists", newBranch))
		}
	}
	if !nameChanged && !branchChanged {
		// 全部同值/未提供：幂等成功，零变更（跳过快照校验、提交与标题后处理）。
		return row, nil
	}

	// 预计算 env 快照目标值（D3 快照矩阵）：快照缺失/损坏在任何写入前返回 internal 零副作用。
	envTarget, err := m.precomputeEnvSnapshotTarget(row, nameChanged, newName, branchChanged, newBranch)
	if err != nil {
		return TaskRow{}, err
	}

	if branchChanged {
		return m.renameBranchAndCommit(ctx, row, proj, nameChanged, newName, newBranch, envTarget)
	}

	// 纯名称变更：无 git 副作用、无意图记录（D2）。
	update := application.TaskInfoUpdate{Name: &newName, EnvSnapshot: envTarget}
	if _, cerr := m.writeCommitTaskInfo(ctx, taskID, update); cerr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("commit task info: %w", cerr))
	}
	m.postCommitTitleSync(ctx, taskID)
	fresh, gerr := m.store.GetTask(ctx, taskID)
	if gerr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("reread task: %w", gerr))
	}
	return fresh, nil
}

// precomputeEnvSnapshotTarget 按 D3 快照矩阵预计算 env 快照目标值（task-info-editable）：
//   - active 且快照缺失（NULL）→ internal（不可自愈，不生成快照，任何写入前拒绝）；
//   - active 且损坏（JSON 非法 / vars 缺失）→ internal（任何写入前拒绝）；
//   - active 且有效 → 仅改写本次变更对应键（name → OCDECK_TASK_NAME；分支实际改名 →
//     OCDECK_TASK_HEAD_BRANCH，历史快照缺键时补写），其余键原样保留；
//   - 非 active → 只提交业务字段（快照保持原样，下次激活按新值重生成）。
//
// 返回 nil 表示不改写快照列。
func (m *Manager) precomputeEnvSnapshotTarget(row TaskRow, nameChanged bool, newName string, branchChanged bool, newBranch string) (*string, error) {
	if row.Status != StatusActive {
		return nil, nil
	}
	if !row.EnvSnapshot.Valid || row.EnvSnapshot.String == "" {
		return nil, newOpErr(codeInternal, fmt.Errorf("task %s: active task env snapshot missing", row.ID))
	}
	var snap envSnapshot
	if err := json.Unmarshal([]byte(row.EnvSnapshot.String), &snap); err != nil || snap.Vars == nil {
		return nil, newOpErr(codeInternal, fmt.Errorf("task %s: env snapshot corrupted", row.ID))
	}
	if nameChanged {
		snap.Vars["OCDECK_TASK_NAME"] = newName
	}
	if branchChanged {
		snap.Vars["OCDECK_TASK_HEAD_BRANCH"] = newBranch
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, newOpErr(codeInternal, fmt.Errorf("task %s: marshal env snapshot: %w", row.ID, err))
	}
	s := string(b)
	return &s, nil
}

// renameBranchAndCommit 执行分支改名的 git 序列与提交（task-info-editable D2 步骤 2-8）。
// 调用方已持任务锁；本函数以 proj.Path（项目公共仓库路径）获取 repo 写锁——锁键不会自动
// 把 worktree 路径折算为公共仓库，编排层必须显式以公共仓库路径取锁。
func (m *Manager) renameBranchAndCommit(ctx context.Context, row TaskRow, proj ProjectRow, nameChanged bool, newName, newBranch string, envTarget *string) (TaskRow, error) {
	repoUnlock, lerr := git.AcquireRepoLock(ctx, proj.Path)
	if lerr != nil {
		// repo 锁获取失败（意图写入前）：确定未生效，零副作用。
		return TaskRow{}, newOpErr(codeGitError, fmt.Errorf("acquire repo lock: %w", lerr))
	}
	defer repoUnlock()

	// 锁内 HEAD 身份验证（零副作用）：symbolic HEAD 须指向任务记录的当前分支；
	// 不匹配（含 detached HEAD、路径缺失、不归属）即 invalid_state，意图写入前拒绝。
	head, herr := m.wt.WorktreeHeadBranch(ctx, proj.Path, row.WorktreePath)
	if herr != nil {
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("verify worktree HEAD: %w", herr))
	}
	if head != row.Branch {
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("worktree HEAD is %q, want %q", head, row.Branch))
	}
	// 锁内复查目标本地 ref 不存在（防锁外竞态）。
	if exists, eerr := m.wt.BranchExists(ctx, proj.Path, newBranch); eerr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("branch existence check: %w", eerr))
	} else if exists {
		return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("branch %s already exists", newBranch))
	}

	// 写恢复意图：同次保存完整目标值 JSON（design D2 步骤 5）。失败 → internal 零 git/DB 业务变更。
	intent := renamePendingIntent{
		BranchOld:   row.Branch,
		BranchNew:   newBranch,
		EnvSnapshot: envTarget,
	}
	if nameChanged {
		intent.Name = &newName
	}
	b, jerr := json.Marshal(intent)
	if jerr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("marshal rename pending: %w", jerr))
	}
	if _, werr := m.writeSetRenamePending(ctx, row.ID, string(b)); werr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("write rename pending: %w", werr))
	}

	// git branch -m（锁内单命令原子：HEAD 跟随、reflog 保留、路径不变）。
	if rerr := m.wt.RenameBranch(ctx, row.WorktreePath, row.Branch, newBranch); rerr != nil {
		return TaskRow{}, m.classifyRenameGitFailure(ctx, row, proj, newBranch, rerr)
	}

	// 提交点：单事务业务列提交 + 清除意图（task-info-editable）。
	update := application.TaskInfoUpdate{Branch: &newBranch, EnvSnapshot: envTarget}
	if nameChanged {
		update.Name = &newName
	}
	if _, cerr := m.writeCommitTaskInfo(ctx, row.ID, update); cerr != nil {
		return TaskRow{}, m.compensateRenameAfterCommitFailure(ctx, row, proj, newBranch, cerr)
	}
	m.postCommitTitleSync(ctx, row.ID)
	fresh, gerr := m.store.GetTask(ctx, row.ID)
	if gerr != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("reread task: %w", gerr))
	}
	return fresh, nil
}

// classifyRenameGitFailure 处置 git 改名报错（D2 失败矩阵）：按 worktree 实际 HEAD 分类，
// 不盲信返回值——HEAD 仍是旧名 → git 明确未生效：清除意图（清除失败则意图残留，聚合报告），
// git_error「确定未生效」；HEAD 已是新名或无法判定 → 结果未知（待恢复）：保留意图，git_error。
func (m *Manager) classifyRenameGitFailure(ctx context.Context, row TaskRow, proj ProjectRow, newBranch string, gitErr error) error {
	head, herr := m.wt.WorktreeHeadBranch(ctx, proj.Path, row.WorktreePath)
	if herr == nil && head == row.Branch {
		if _, cerr := m.writeClearRenamePending(ctx, row.ID); cerr != nil {
			return newOpErr(codeGitError, fmt.Errorf("rename not applied; clear rename pending: %v (git: %v)", cerr, gitErr))
		}
		return newOpErr(codeGitError, fmt.Errorf("rename branch: %w", gitErr))
	}
	return newOpErr(codeGitError, fmt.Errorf("rename branch outcome unknown (rename pending kept): %w", gitErr))
}

// compensateRenameAfterCommitFailure 处置 DB 提交失败（D2 失败矩阵）：git 改回旧名后按 HEAD
// 复核归类——改回成功 → 清除意图（「确定未生效」，git_error）；改回失败或结果未知 →
// 保留意图（「待恢复」，git_error）。
func (m *Manager) compensateRenameAfterCommitFailure(ctx context.Context, row TaskRow, proj ProjectRow, newBranch string, commitErr error) error {
	rerr := m.wt.RenameBranch(ctx, row.WorktreePath, newBranch, row.Branch)
	head, herr := m.wt.WorktreeHeadBranch(ctx, proj.Path, row.WorktreePath)
	if rerr == nil && herr == nil && head == row.Branch {
		if _, cerr := m.writeClearRenamePending(ctx, row.ID); cerr != nil {
			return newOpErr(codeGitError, fmt.Errorf("db commit failed and reverted; clear rename pending: %v (commit: %v)", cerr, commitErr))
		}
		return newOpErr(codeGitError, fmt.Errorf("db commit failed (branch reverted): %w", commitErr))
	}
	return newOpErr(codeGitError, fmt.Errorf("db commit failed and compensate failed (rename pending kept): commit=%v rename-back=%v", commitErr, rerr))
}

// convergeRenamePending 执行 R1 收敛（task-info-editable D2；调用方 MUST 已持任务锁）。
// 在 repo 写锁内按 worktree 实际 HEAD 三分支判定（稳定 ID：意图携带于任务行 rename_pending，
// 按任务 ID 定位；MUST NOT 先要求 HEAD=row.Branch）：
//   - HEAD 已是 branch_new → 按意图经 CommitTaskInfoUpdate 原子补做整次提交（含名称与快照），
//     同事务清除意图（补提交失败 = 意图保留）；
//   - HEAD 仍是 branch_old → 清除意图（git 未发生或已回滚，整次保存视为未生效）；
//   - HEAD 指向其他分支 / detached / worktree 缺失 / 读取失败 → 保留意图，返回明确错误。
//
// 返回 nil 表示意图已收敛；非 nil 表示不可收敛（意图保留，调用方按入口仲裁处置）。
func (m *Manager) convergeRenamePending(ctx context.Context, row TaskRow, proj ProjectRow, intentJSON string) error {
	var intent renamePendingIntent
	if err := json.Unmarshal([]byte(intentJSON), &intent); err != nil {
		// 意图 JSON 损坏（持久化损坏）：无法重建提交，保留现场按不可收敛处置。
		return fmt.Errorf("corrupt rename pending: %w", err)
	}
	repoUnlock, lerr := git.AcquireRepoLock(ctx, proj.Path)
	if lerr != nil {
		return fmt.Errorf("acquire repo lock: %w", lerr)
	}
	defer repoUnlock()

	head, herr := m.wt.WorktreeHeadBranch(ctx, proj.Path, row.WorktreePath)
	if herr != nil {
		return fmt.Errorf("determine worktree HEAD: %w", herr)
	}
	switch head {
	case intent.BranchNew:
		// git 已是新名：按意图原子补做整次提交并清除意图。
		update := application.TaskInfoUpdate{Branch: &intent.BranchNew, EnvSnapshot: intent.EnvSnapshot}
		if intent.Name != nil {
			update.Name = intent.Name
		}
		if _, cerr := m.writeCommitTaskInfo(ctx, row.ID, update); cerr != nil {
			return fmt.Errorf("replay commit: %w", cerr)
		}
		// D3 统一后处理：恢复补提交与正常提交同规——仅名称实际变更时同步会话标题
		//（内部重读任务当前 name，不直接使用 intent 旧值）。
		if intent.Name != nil {
			m.postCommitTitleSync(ctx, row.ID)
		}
		return nil
	case intent.BranchOld:
		// git 仍是旧名：清除意图。
		if _, cerr := m.writeClearRenamePending(ctx, row.ID); cerr != nil {
			return fmt.Errorf("clear rename pending: %w", cerr)
		}
		return nil
	default:
		return fmt.Errorf("worktree HEAD %q matches neither %q nor %q", head, intent.BranchOld, intent.BranchNew)
	}
}

// convergePendingBeforeLifecycle 生命周期入口共用的 R1 前置（D2 仲裁表）：存在未收敛改名
// 意图时先收敛；不可收敛返回明确错误（调用方按入口映射 conflict / 跳过+last_error）。
// 无意图时零 git 副作用直接放行。调用方 MUST 已持任务锁（Suspend/Activate/Recovery 入口均持有）。
func (m *Manager) convergePendingBeforeLifecycle(ctx context.Context, row TaskRow, proj ProjectRow) error {
	pending, err := m.readRenamePending(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("read rename pending: %w", err)
	}
	if pending == nil {
		return nil
	}
	if cerr := m.convergeRenamePending(ctx, row, proj, *pending); cerr != nil {
		return fmt.Errorf("task %s rename pending not converged: %w", row.ID, cerr)
	}
	return nil
}

// convergeAllRenamePendings 对 tasks 中存在未收敛改名意图的任务逐个执行 R1（启动 reconcile
// 与 kill 模式 Shutdown 共用）。逐任务 tryLockTask（拿不到锁视为不可收敛，启动期无并发不应发生）；
// 项目路径按 ProjectID 缓存。错误聚合返回（调用方：Reconcile fail-closed 拒开 HTTP；
// Shutdown 聚合后仍执行既有进程终止）。
func (m *Manager) convergeAllRenamePendings(ctx context.Context, tasks []TaskRow) error {
	var errs []error
	projByID := make(map[string]ProjectRow)
	for _, t := range tasks {
		pending, err := m.readRenamePending(ctx, t.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: read rename pending: %w", t.ID, err))
			continue
		}
		if pending == nil {
			continue
		}
		proj, ok := projByID[t.ProjectID]
		if !ok {
			p, perr := m.store.GetProject(ctx, t.ProjectID)
			if perr != nil {
				errs = append(errs, fmt.Errorf("task %s: get project: %w", t.ID, perr))
				continue
			}
			proj = p
			projByID[t.ProjectID] = proj
		}
		unlock, lerr := m.tryLockTask(t.ID)
		if lerr != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, lerr))
			continue
		}
		if cerr := m.convergeRenamePending(ctx, t, proj, *pending); cerr != nil {
			errs = append(errs, fmt.Errorf("task %s converge rename pending: %w", t.ID, cerr))
		}
		unlock()
	}
	return errors.Join(errs...)
}

// postCommitTitleSync 统一后处理入口（task-info-editable D3）：名称实际变更的本地提交成功后
// best-effort 同步已有会话标题。Phase 2 仅预留 seam（m.titleSync 未注入时 no-op，零进程/HTTP
// 副作用）；Phase 3 注入基于 UpdateSessionTitle client 的实现（capability 降级与 notice 语义
// 随该任务落地）。防旧值覆盖：执行时重新读取任务当前 name 再传入——保存由任务锁串行，
// 后处理读到的总是最新名称。
func (m *Manager) postCommitTitleSync(ctx context.Context, taskID string) {
	if m.titleSync == nil {
		return
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	m.titleSync(ctx, taskID, row.Name)
}

// --- 写 helper（P1.4.7 write* 路由先例：注入 LifecycleService 时走 persist+commit 封装，
// 事件链接线在 LifecycleService 层；未注入走 legacy 直连 store 路径） ---

func (m *Manager) writeCommitTaskInfo(ctx context.Context, id string, update application.TaskInfoUpdate) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.CommitTaskInfoUpdate(ctx, id, update)
	}
	return m.store.CommitTaskInfoUpdate(ctx, id, update)
}

func (m *Manager) writeSetRenamePending(ctx context.Context, id, pendingJSON string) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.SetTaskRenamePending(ctx, id, pendingJSON)
	}
	return m.store.SetTaskRenamePending(ctx, id, pendingJSON)
}

func (m *Manager) writeClearRenamePending(ctx context.Context, id string) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.ClearTaskRenamePending(ctx, id)
	}
	return m.store.ClearTaskRenamePending(ctx, id)
}

func (m *Manager) readRenamePending(ctx context.Context, id string) (*string, error) {
	if m.lifecycle != nil {
		return m.lifecycle.GetTaskRenamePending(ctx, id)
	}
	return m.store.GetTaskRenamePending(ctx, id)
}
