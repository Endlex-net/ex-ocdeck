package task

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ocdeck/internal/application"
	apptask "ocdeck/internal/application/task"
	ocdecktask "ocdeck/internal/domain/task"
)

// --- P1.4.6 strangler 双路径写 helper ---
//
// Create/Retry/Activate 的任务行写入经此处分流：注入 LifecycleService 时走 persist+commit
// 封装（commit helper 经 NoopPublisher，调用位就绪无实际发布），未注入时回退 legacy 直连
// store 路径（单测路径行为不变）。外部副作用（worktree/tmux/opencode）与调度仍留在 Manager。

// writeCreateTask 插入任务行（creating 意图落库，crud.go 调用点）。
func (m *Manager) writeCreateTask(ctx context.Context, row TaskRow) error {
	if m.lifecycle != nil {
		return m.lifecycle.CreateTask(ctx, taskRowToSnapshot(row))
	}
	return m.store.CreateTask(ctx, row)
}

// writeStatus 无条件状态写入（含 last_error）。
func (m *Manager) writeStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.UpdateStatus(ctx, id, ocdecktask.Status(status), nullStringToPtr(lastError))
	}
	return m.store.UpdateTaskStatus(ctx, id, status, lastError)
}

// writeStatusConditional CAS 状态写入（fromStatus 失配返回 !Matched）。
func (m *Manager) writeStatusConditional(ctx context.Context, id, from, to string, lastError sql.NullString) (application.TransitionResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.UpdateStatusConditional(ctx, id, ocdecktask.Status(from), ocdecktask.Status(to), nullStringToPtr(lastError))
	}
	return m.store.UpdateTaskStatusConditional(ctx, id, from, to, lastError)
}

// writeCommitCreated 创建提交点（expectedStatus CAS → suspended + init_status）。
func (m *Manager) writeCommitCreated(ctx context.Context, id, expectedStatus, initStatus string) (application.TransitionResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.CommitCreated(ctx, id, ocdecktask.Status(expectedStatus), ocdecktask.InitStatus(initStatus))
	}
	return m.store.CommitCreated(ctx, id, expectedStatus, initStatus)
}

// writeDeleteMode 写入 delete_mode（Retry deletion_failed 重入）。
func (m *Manager) writeDeleteMode(ctx context.Context, id, mode string) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.SetDeleteMode(ctx, id, ocdecktask.DeleteMode(mode))
	}
	return m.store.SetTaskDeleteMode(ctx, id, mode)
}

// writeEnvSnapshot 写入 env_snapshot（Activate 合并快照持久化与补偿清空）。
func (m *Manager) writeEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.UpdateEnvSnapshot(ctx, id, nullStringToPtr(envSnapshot))
	}
	return m.store.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

// writeLastPort 写入 last_port（Activate 端口写回）。
func (m *Manager) writeLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.UpdateLastPort(ctx, id, port)
	}
	return m.store.UpdateTaskLastPort(ctx, id, port)
}

// writeBeginDeleteIntent 写入删除意图（deleting 迁移，fromStatuses 守卫）。
func (m *Manager) writeBeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (application.TransitionResult, error) {
	if m.lifecycle != nil {
		statuses := make([]ocdecktask.Status, len(fromStatuses))
		for i, s := range fromStatuses {
			statuses[i] = ocdecktask.Status(s)
		}
		return m.lifecycle.BeginDeleteIntent(ctx, id, ocdecktask.DeleteMode(mode), statuses)
	}
	return m.store.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

// writeDeleteTask 删除任务行（级联剩余会话；commit 先发 session.deleted 再 task.deleted）。
func (m *Manager) writeDeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.DeleteTask(ctx, id)
	}
	return m.store.DeleteTask(ctx, id)
}

// writeNoticeCAS 后台通知 CAS 重写（Changed=true 发布 activity_changed）。
func (m *Manager) writeNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.UpdateNoticeCAS(ctx, id, nullStringToPtr(expected), nullStringToPtr(newNotice))
	}
	return m.store.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// writeConvergeInterruptedInitRuns 启动期收敛中断 init run（HTTP 开放前零订阅者，不发布）。
func (m *Manager) writeConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	if m.lifecycle != nil {
		return m.lifecycle.ConvergeInterruptedInitRuns(ctx)
	}
	return m.store.ConvergeInterruptedInitRuns(ctx)
}

// writeClaimInitRun 认领 init run（Changed=true 发布 activity_changed）。
func (m *Manager) writeClaimInitRun(ctx context.Context, id string) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.ClaimInitRun(ctx, id)
	}
	return m.store.ClaimInitRun(ctx, id)
}

// writeClaimInitRerun 认领 init 重跑（Changed=true 发布 activity_changed）。
func (m *Manager) writeClaimInitRerun(ctx context.Context, id string) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.ClaimInitRerun(ctx, id)
	}
	return m.store.ClaimInitRerun(ctx, id)
}

// writeFinishInitRun 写入 init run 终态（Changed=true 发布 activity_changed）。
func (m *Manager) writeFinishInitRun(ctx context.Context, id, initStatus string, initError sql.NullString) (application.MutationResult, error) {
	if m.lifecycle != nil {
		return m.lifecycle.FinishInitRun(ctx, id, ocdecktask.InitStatus(initStatus), nullStringToPtr(initError))
	}
	return m.store.FinishInitRun(ctx, id, initStatus, initError)
}

// Create 在项目下创建任务：按 proj.Kind 分叉（add-plain-dir-project D2/D10）。
//   - repo：生成 worktree + 分支（分支 ocdeck/<task-name-slug>，worktree 路径
//     <dataDir>/worktrees/<projectNameSlug>/<branchPathSlug>-<rand4>）；接受可选 base_ref
//     短名，解析为全限定 ref 随任务落库，wt.Add 用解析后 baseRef 替代 proj.DefaultBranch。
//   - dir：无 worktree/分支/inherit 复制；worktree_path=canonical 项目路径，Branch=""；
//     落库前做无副作用目录预检（os.Stat 存在且为目录，否则 invalid_state 且不落 creating 行）。
//
// D5 主流程顺序：项目存在 → kind 分叉 → 无副作用前置检查（repo：slug/branch/分支校验/冲突检查/
// base_ref 解析；dir：目录预检）→ 落库 creating → 副作用（repo：wt.Add+inherit 复制；dir：仅读配置）
// → CommitCreated → InitRunner/triggerActivate。
// 未知 kind fail-closed 报错零副作用（MUST NOT 落 creating 行）。
func (m *Manager) Create(ctx context.Context, projectID, taskName, baseRef string) (TaskRow, error) {
	if strings.TrimSpace(taskName) == "" {
		return TaskRow{}, newOpErr(codeInvalidInput, errors.New("task name is required"))
	}
	// 项目存在性检查。
	proj, err := m.store.GetProject(ctx, projectID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("project not found: %w", err))
	}

	// 按 kind 分叉（显式 repo/dir，未知 fail-closed 零副作用）。
	switch proj.Kind {
	case ProjectKindRepo:
		return m.createRepo(ctx, projectID, taskName, proj, baseRef)
	case ProjectKindDir:
		return m.createDir(ctx, projectID, taskName, proj, baseRef)
	default:
		// 未知持久化 kind（DB 损坏值）→ internal（D1：区别于用户请求非法 kind 的 invalid_input）。
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("unknown project kind %q", proj.Kind))
	}
}

// createRepo 实现 repo 项目任务创建（ai-worktree-naming D5 + add-plain-dir-project D10）。
// base_ref 缺省落库 refs/heads/<proj.DefaultBranch>；提供短名则解析为全限定 ref。
func (m *Manager) createRepo(ctx context.Context, projectID, taskName string, proj ProjectRow, baseRef string) (TaskRow, error) {
	taskID := newTaskID()
	// 分支 slug 经 Namer 提炼（ai-worktree-naming D3/D4）：nil 时回退到本包 Slugify
	//（构造期或测试未注入时的防御，杜绝 panic）。
	slug := Slugify(taskName)
	if m.namer != nil {
		slug = m.namer.Slug(ctx, taskName)
	}
	branch := "ocdeck/" + slug
	repoPath := proj.Path

	// P1：Create 前置检查（design.md §19：项目存在、分支名 check-ref-format、分支名不冲突）
	// MUST 全部完成于插入 creating 行之前——先插行后查分支冲突时，前置失败会残留
	// creation_failed 行。D5 顺序：分支校验/冲突检查先于 dest 生成循环，保证冲突 409
	// 不被 stat/rand 异常覆盖（无副作用阶段全部前置完成）。
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

	// base_ref 解析（add-plain-dir-project D10）：无副作用前置检查，落库前完成。
	// 缺省 → refs/heads/<proj.DefaultBranch>；提供短名 → 规范校验 + heads/remotes 探测。
	resolvedBaseRef, err := m.resolveRepoBaseRef(ctx, repoPath, baseRef, proj.DefaultBranch)
	if err != nil {
		return TaskRow{}, newOpErr(codeInvalidInput, err)
	}

	// dest 生成循环（D5：分支检查通过后再生成路径）：rand4+os.Stat≤3 次碰撞重试。
	// 碰撞/rand/stat 异常 → 直接返回错误，零副作用（无落库、无 Add）。
	wtPath, err := m.newWorktreePath(proj, branch)
	if err != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("compute worktree path: %w", err))
	}

	// ① 意图落库：插入任务行（status=creating）。
	if err := m.writeCreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: projectID, Name: taskName,
		Branch: branch, Status: StatusCreating, WorktreePath: wtPath, BaseRef: resolvedBaseRef,
	}); err != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("create task row: %w", err))
	}

	// ② worktree add（仓库级写锁在 worktree.Add 内）。
	if err := m.wt.Add(ctx, repoPath, wtPath, branch, resolvedBaseRef); err != nil {
		// worktree add 失败 → creation_failed（前置检查已通过，失败发生在副作用阶段）。
		le := sql.NullString{String: fmt.Errorf("worktree add: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeGitError, fmt.Errorf("worktree add: %w", err))
	}

	// ③ runInherit 编排（design.md §4，tasks 3.2-3.3）：读配置(唯一阻断点) → 枚举 → 复制 → 重写 inherit.log。
	// 读配置失败 → creation_failed；枚举/复制失败 → 警告（非阻断）。
	// 单配置快照：读到的 cfg 供后续 init_status 决策，避免二次读取把读错误静默当"无 init 脚本"
	//（违反"配置读取失败=唯一阻断点"，design.md §4/不变量 5）。
	cfg, warnings, inheritErr := m.runInherit(ctx, repoPath, wtPath, projectID)
	if inheritErr != nil {
		le := sql.NullString{String: fmt.Errorf("inherit: %w", inheritErr).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeInternal, fmt.Errorf("inherit: %w", inheritErr))
	}
	// task 层重写 inherit.log（每次重写/无警告删除/1MB 截断/写失败仅服务端日志/0600+0700）。
	m.writeInheritLog(m.inheritLogPath(taskID), warnings)

	// ④ 提交点：CommitCreated 原子提交（design.md §4，tasks 3.3）。
	// 配置有 init 脚本 → init_status=pending（待 InitRunner）；否则 none（直接激活）。
	initStatus := InitStatusNone
	if cfg.InitScript != "" {
		initStatus = InitStatusPending
	}
	committed, err := m.writeCommitCreated(ctx, taskID, StatusCreating, initStatus)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("commit created: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeInternal, fmt.Errorf("commit created: %w", err))
	}
	if !committed.Matched {
		// 状态已被并发改变（预期 creating → 已变），落 creation_failed。
		le := sql.NullString{String: "commit created: status changed concurrently", Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeConflict, fmt.Errorf("commit created: status changed concurrently"))
	}

	// 调度不依赖 request-bound GetTask：用已持有数据（taskID）启动异步链。
	// request ctx 取消/瞬时读失败不得留无人处理的 suspended+pending/none（design.md §4）。
	// ④ 锁外异步链（design.md §4）：none → 直接 triggerActivate（现状）；pending → 启动 InitRunner。
	if initStatus == InitStatusPending {
		m.startInitRunner(taskID)
	} else {
		m.triggerActivate(taskID)
	}
	// 返回前读最新行（用 request ctx，失败仅影响返回值，不影响已提交状态与已调度异步链）。
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{ID: taskID, ProjectID: projectID, Name: taskName, Branch: branch,
			Status: StatusSuspended, WorktreePath: wtPath, BaseRef: resolvedBaseRef}, newOpErr(codeInternal, err)
	}
	return row, nil
}

// createDir 实现 dir 项目任务创建（add-plain-dir-project D2）。
// 无 worktree/分支/inherit 复制；worktree_path=canonical 项目路径，Branch=""。
// 落库前做无副作用目录预检（os.Stat 存在且为目录，否则 invalid_state 不落 creating 行）。
// 提供 base_ref → invalid_input 零副作用。
func (m *Manager) createDir(ctx context.Context, projectID, taskName string, proj ProjectRow, baseRef string) (TaskRow, error) {
	if baseRef != "" {
		// dir 项目不接受 base_ref（add-plain-dir-project D10/spec：提供即 invalid_input）。
		return TaskRow{}, newOpErr(codeInvalidInput, errors.New("base_ref is not allowed for dir project"))
	}
	// 目录存在性预检（无副作用，落库前完成）：存在且为目录，否则 invalid_state。
	canonicalPath, err := filepath.EvalSymlinks(proj.Path)
	if err != nil {
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("dir project path not accessible: %w", err))
	}
	info, err := os.Stat(canonicalPath)
	if err != nil {
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("dir project path not accessible: %w", err))
	}
	if !info.IsDir() {
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("dir project path is not a directory: %s", canonicalPath))
	}

	taskID := newTaskID()
	// ① 意图落库：插入任务行（status=creating，Branch=""，WorktreePath=canonical 项目路径）。
	if err := m.writeCreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: projectID, Name: taskName,
		Branch: "", Status: StatusCreating, WorktreePath: canonicalPath, BaseRef: "",
	}); err != nil {
		return TaskRow{}, newOpErr(codeInternal, fmt.Errorf("create task row: %w", err))
	}

	// ② 仅读 lifecycle 配置（不枚举/复制 gitignored 文件，D2）。
	// 读配置失败 → creation_failed（与 repo 同一阻断点语义）。
	cfg, err := m.store.GetLifecycleConfig(ctx, projectID)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("read lifecycle config: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeInternal, fmt.Errorf("read lifecycle config: %w", err))
	}

	// ④ 提交点：CommitCreated 原子提交。init_status 决策同 repo（配置有 init 脚本 → pending）。
	initStatus := InitStatusNone
	if cfg.InitScript != "" {
		initStatus = InitStatusPending
	}
	committed, err := m.writeCommitCreated(ctx, taskID, StatusCreating, initStatus)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("commit created: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeInternal, fmt.Errorf("commit created: %w", err))
	}
	if !committed.Matched {
		le := sql.NullString{String: "commit created: status changed concurrently", Valid: true}
		_, _ = m.writeStatus(ctx, taskID, StatusCreationFailed, le)
		row, _ := m.store.GetTask(ctx, taskID)
		return row, newOpErr(codeConflict, fmt.Errorf("commit created: status changed concurrently"))
	}

	// ④ 锁外异步链：同 repo（init_status=pending → InitRunner；none → triggerActivate）。
	// dir 激活只锚定目录，天然兼容（D2）。
	if initStatus == InitStatusPending {
		m.startInitRunner(taskID)
	} else {
		m.triggerActivate(taskID)
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{ID: taskID, ProjectID: projectID, Name: taskName, Branch: "",
			Status: StatusSuspended, WorktreePath: canonicalPath}, newOpErr(codeInternal, err)
	}
	return row, nil
}

// resolveRepoBaseRef 解析 repo 任务 base_ref（add-plain-dir-project D10）。
// baseRef 为空 → 返回 refs/heads/<proj.DefaultBranch>（缺省落库全限定 ref）。
// baseRef 非空 → 先 ValidateBranchName（check-ref-format --branch）规范校验，
// 再 ResolveBaseRef 按 refs/heads → refs/remotes 顺序探测（heads 优先）。
// 任一环节失败返回错误（调用方映射 invalid_input）。无副作用，前置完成于落库前。
func (m *Manager) resolveRepoBaseRef(ctx context.Context, repoPath, baseRef, defaultBranch string) (string, error) {
	if baseRef == "" {
		if defaultBranch == "" {
			// repo 项目 default_branch 必须非空（注册时经 ResolveDefaultBranch 探测）；空为异常。
			return "", errors.New("repo project has no default branch")
		}
		return "refs/heads/" + defaultBranch, nil
	}
	// 规范校验（check-ref-format --branch）：拒绝 .. /控制字符等非法输入。
	if err := m.wt.ValidateBranchName(ctx, repoPath, baseRef); err != nil {
		return "", fmt.Errorf("invalid base_ref %q: %w", baseRef, err)
	}
	// 解析为全限定 ref（heads 优先，仅接受这两个命名空间）。
	resolved, err := m.wt.ResolveBaseRef(ctx, repoPath, baseRef)
	if err != nil {
		return "", fmt.Errorf("resolve base_ref %q: %w", baseRef, err)
	}
	return resolved, nil
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
		outcome, rerr := m.retryCreate(ctx, row)
		if rerr != nil {
			return rerr
		}
		// 先显式解锁再调度异步链，保证 Activate/InitRunner 的 tryLockTask 不自锁。
		unlockOnce()
		switch outcome {
		case createDirectActivate:
			m.triggerActivate(row.ID)
		case createStartInit:
			m.startInitRunner(row.ID)
		}
		return nil
	case StatusDeleting, StatusDeletionFailed:
		mode := DeleteNormal
		if row.DeleteMode.Valid && row.DeleteMode.String == string(DeleteForce) {
			mode = DeleteForce
		}
		// add-plain-dir-project D3：删除重入按项目 kind 分叉（与 Delete 首次一致，delete.go）。
		// dir 跳过 DirtyFiles 快照与 confirmDirty 门禁（git 静态检查不适用，confirmDirty 接受但忽略），
		// 直接以 nil 快照进入 deleteResume（deleteResumeDir 忽略 preflightDirty）。
		// repo 保持现状：preflight DirtyFiles 快照 + confirmDirty 门禁 + deleteResume。
		// 未知 kind fail-closed：在 deletion_failed → deleting 状态转换与 DirtyFiles 前返回，零副作用。
		proj, perr := m.store.GetProject(ctx, row.ProjectID)
		if perr != nil {
			return newOpErr(codeNotFound, fmt.Errorf("project not found: %w", perr))
		}
		if proj.Kind != ProjectKindRepo && proj.Kind != ProjectKindDir {
			// 未知持久化 kind（DB 损坏值）→ internal（D1）。
			return newOpErr(codeInternal, fmt.Errorf("task %s unknown project kind %q", taskID, proj.Kind))
		}
		// B8：delete_mode 不得被 Normal 重试覆盖；按持久化 delete_mode 重入。
		// 先置 deleting 再执行（deletion_failed → deleting，design.md §19/§8）。
		// 在 kind 校验通过后转换，保证未知 kind 零副作用（状态不变）。
		if row.Status == StatusDeletionFailed {
			if _, err := m.writeDeleteMode(ctx, row.ID, string(mode)); err != nil {
				return newOpErr(codeInternal, err)
			}
			if _, err := m.writeStatus(ctx, row.ID, StatusDeleting, sql.NullString{}); err != nil {
				return newOpErr(codeInternal, err)
			}
		}
		var preflightDirty map[string]struct{}
		switch proj.Kind {
		case ProjectKindRepo:
			// P0/B1：Retry 重入删除序列 MUST NOT 传 nil 跳过二次 dirty 门禁，也不得以新快照
			// 为基线把首次未确认文件纳入"已确认"随后 ForceDirty 强删。正确语义与首次 Delete
			// 一致：取当前 dirty 集合，非空则要求调用方显式 confirmDirty=true；confirmDirty=false
			// 且非空 → 拒绝（409，提示需确认），不进入 deleteResume。
			// 快照探测失败 MUST fail-closed（与首次 Delete 一致）：DirtyFiles 错误意味着无法
			// 判定当前 dirty 集合，不得当空集强删用户数据。
			snap, derr := m.wt.DirtyFiles(ctx, row.WorktreePath)
			if derr != nil {
				le := sql.NullString{String: fmt.Errorf("retry: preflight dirty snapshot: %w", derr).Error(), Valid: true}
				_, _ = m.writeStatus(ctx, row.ID, StatusDeletionFailed, le)
				return newOpErr(codeGitError, fmt.Errorf("retry: preflight dirty snapshot: %w", derr))
			}
			if len(snap) > 0 && !confirmDirty {
				le := sql.NullString{String: "retry: worktree has dirty files; confirm deletion again", Valid: true}
				_, _ = m.writeStatus(ctx, row.ID, StatusDeletionFailed, le)
				return newOpErr(codeConflict, errors.New("worktree: retry delete has dirty files; confirm deletion again with confirmDirty=true"))
			}
			preflightDirty = snap
		case ProjectKindDir:
			// dir：跳过 DirtyFiles 快照与 confirmDirty 门禁（preflightDirty 保持 nil）。
		}
		return m.deleteResume(ctx, row, mode, preflightDirty)
	default:
		return newOpErr(codeInvalidState, fmt.Errorf("task %s not in retryable state (status=%s)", taskID, row.Status))
	}
}

// retryCreate 重试创建（design.md §19 Create Retry 行 + §3.1，tasks 3.2-3.3 + D2/D10）。
// 按 proj.Kind 分叉：
//   - repo：严格产物验证——通过则跳过 add，否则重新 add（用落库 BaseRef，MUST NOT 重读
//     proj.DefaultBranch；repo 落库值为空 fail-closed 报错）。无论产物复用还是重建都重新
//     幂等执行 inherit（§3.1）。
//   - dir：跳过 VerifyWorktreeProduct/分支检查/wt.Add，仅校验项目目录仍存在且为目录
//     （不存在/非目录 → 保持 creation_failed 并报错，零副作用），然后读配置提交。
//
// CommitCreated(expectedStatus='creation_failed')：从 creation_failed 原子提交到 suspended。
// 读配置失败 → creation_failed（保留状态，调用方据 error 传播）。
// 返回二态结果：createDirectActivate（none，调用方 triggerActivate）或 createStartInit（pending，调用方启动 InitRunner）。
// 未知 kind fail-closed 报错零副作用。
func (m *Manager) retryCreate(ctx context.Context, row TaskRow) (createOutcome, error) {
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return 0, newOpErr(codeInternal, fmt.Errorf("project gone during retry: %w", err))
	}
	switch proj.Kind {
	case ProjectKindRepo:
		return m.retryCreateRepo(ctx, row, proj)
	case ProjectKindDir:
		return m.retryCreateDir(ctx, row, proj)
	default:
		// 未知持久化 kind（DB 损坏值）→ internal（D1）。
		return 0, newOpErr(codeInternal, fmt.Errorf("unknown project kind %q", proj.Kind))
	}
}

// retryCreateRepo 实现 repo 任务创建重试（design.md §3.1 + add-plain-dir-project D10）。
// 重建 worktree 用落库 BaseRef（空值 fail-closed），MUST NOT 重读 proj.DefaultBranch。
func (m *Manager) retryCreateRepo(ctx context.Context, row TaskRow, proj ProjectRow) (createOutcome, error) {
	// repo 任务落库 BaseRef MUST 非空（空值仅 dir 使用，D10 fail-closed）。
	if row.BaseRef == "" {
		le := sql.NullString{String: "retry: repo task has empty base_ref (fail-closed)", Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInvalidState, errors.New("retry: repo task has empty base_ref (fail-closed)"))
	}
	// 严格产物验证：通过则跳过 add，否则重新 add。
	if err := m.wt.VerifyWorktreeProduct(ctx, proj.Path, row.WorktreePath, row.Branch); err != nil {
		// 产物不完整或不存在 → 重新 add。先检查分支冲突（B1）。
		if exists, berr := m.wt.BranchExists(ctx, proj.Path, row.Branch); berr != nil {
			le := sql.NullString{String: fmt.Errorf("branch existence check: %w", berr).Error(), Valid: true}
			_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
			return 0, newOpErr(codeInternal, fmt.Errorf("branch existence check: %w", berr))
		} else if exists {
			le := sql.NullString{String: fmt.Errorf("branch %s already exists", row.Branch).Error(), Valid: true}
			_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
			return 0, newOpErr(codeConflict, fmt.Errorf("branch %s already exists", row.Branch))
		}
		// D10：重建用落库 BaseRef，MUST NOT 重读 proj.DefaultBranch。
		if err := m.wt.Add(ctx, proj.Path, row.WorktreePath, row.Branch, row.BaseRef); err != nil {
			le := sql.NullString{String: fmt.Errorf("retry worktree add: %w", err).Error(), Valid: true}
			_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
			return 0, newOpErr(codeGitError, err)
		}
	}
	// runInherit 编排（§3.1：总是幂等重跑）。读配置失败 → creation_failed。
	// 单配置快照：cfg 供 init_status 决策（与 Create 一致，避免二次读取）。
	cfg, warnings, inheritErr := m.runInherit(ctx, proj.Path, row.WorktreePath, row.ProjectID)
	if inheritErr != nil {
		le := sql.NullString{String: fmt.Errorf("inherit: %w", inheritErr).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInternal, fmt.Errorf("inherit: %w", inheritErr))
	}
	m.writeInheritLog(m.inheritLogPath(row.ID), warnings)

	// CommitCreated 原子提交（expectedStatus='creation_failed'，§3.1）。
	initStatus := InitStatusNone
	if cfg.InitScript != "" {
		initStatus = InitStatusPending
	}
	committed, err := m.writeCommitCreated(ctx, row.ID, StatusCreationFailed, initStatus)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("commit created: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInternal, err)
	}
	if !committed.Matched {
		// 状态已被并发改变，保留原状态。
		return 0, newOpErr(codeConflict, fmt.Errorf("commit created: status changed concurrently"))
	}
	if initStatus == InitStatusPending {
		return createStartInit, nil
	}
	return createDirectActivate, nil
}

// retryCreateDir 实现 dir 任务创建重试（add-plain-dir-project D2）。
// 跳过 VerifyWorktreeProduct/分支检查/wt.Add，仅校验项目目录仍存在且为目录
// （不存在/非目录 → 保持 creation_failed 并报错，零副作用），然后读配置提交。
func (m *Manager) retryCreateDir(ctx context.Context, row TaskRow, proj ProjectRow) (createOutcome, error) {
	// 目录存在性校验（与 createDir 预检一致，零副作用）：不存在/非目录 → 保持 creation_failed。
	canonicalPath, err := filepath.EvalSymlinks(proj.Path)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("retry: dir project path not accessible: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInvalidState, fmt.Errorf("retry: dir project path not accessible: %w", err))
	}
	info, err := os.Stat(canonicalPath)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("retry: dir project path not accessible: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInvalidState, fmt.Errorf("retry: dir project path not accessible: %w", err))
	}
	if !info.IsDir() {
		le := sql.NullString{String: fmt.Errorf("retry: dir project path is not a directory: %s", canonicalPath).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInvalidState, fmt.Errorf("retry: dir project path is not a directory: %s", canonicalPath))
	}

	// 仅读 lifecycle 配置（不枚举/复制 gitignored 文件，D2）。读配置失败 → creation_failed。
	cfg, err := m.store.GetLifecycleConfig(ctx, row.ProjectID)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("read lifecycle config: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInternal, fmt.Errorf("read lifecycle config: %w", err))
	}

	// CommitCreated 原子提交（expectedStatus='creation_failed'）。init_status 决策同 repo。
	initStatus := InitStatusNone
	if cfg.InitScript != "" {
		initStatus = InitStatusPending
	}
	committed, err := m.writeCommitCreated(ctx, row.ID, StatusCreationFailed, initStatus)
	if err != nil {
		le := sql.NullString{String: fmt.Errorf("commit created: %w", err).Error(), Valid: true}
		_, _ = m.writeStatus(ctx, row.ID, StatusCreationFailed, le)
		return 0, newOpErr(codeInternal, err)
	}
	if !committed.Matched {
		return 0, newOpErr(codeConflict, fmt.Errorf("commit created: status changed concurrently"))
	}
	if initStatus == InitStatusPending {
		return createStartInit, nil
	}
	return createDirectActivate, nil
}

// Archive 归档（design.md §19 Archive 行：纯 DB，状态须为 suspended）。
//
// P1.4.4：注入 LifecycleService 时委托（guard 经 domain CanArchive、CAS 经 ports、
// commit helper 经 NoopPublisher），未注入时回退 legacy 直连 store 路径。
// OpError 映射逐字不变（api.TaskBackend 契约冻结不变量）。
func (m *Manager) Archive(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	if m.lifecycle != nil {
		return m.mapLifecycleArchiveErr(ctx, taskID)
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// guard 委托 domain/task.CanArchive（design D0 P1.4.2 strangler 第二步）。
	// 委托前后行为 byte-equivalent：guard 拒绝时按现状维度顺序生成错误（status 优先，init 次之）。
	if !rehydrateGuardView(row).CanArchive() {
		if row.Status != StatusSuspended {
			return newOpErr(codeInvalidState, fmt.Errorf("archive requires suspended, got %s", row.Status))
		}
		// init_status 门禁（design.md tasks 3.7）：init 进行中拒绝归档。
		return newOpErr(codeInvalidState, fmt.Errorf("archive: task %s init in progress (init_status=%s)", taskID, row.InitStatus))
	}
	if _, err := m.store.ArchiveTask(ctx, taskID); err != nil {
		return newOpErr(codeInternal, err)
	}
	return nil
}

// mapLifecycleArchiveErr 委托 LifecycleService.Archive 并映射 typed error 为 OpError（逐字不变）。
func (m *Manager) mapLifecycleArchiveErr(ctx context.Context, taskID string) error {
	err := m.lifecycle.Archive(ctx, taskID)
	if err == nil {
		return nil
	}
	var ae *apptask.ArchiveError
	if errors.As(err, &ae) {
		return newOpErr(codeInvalidState, errors.New(ae.Error()))
	}
	if errors.Is(err, application.ErrTaskNotFound) {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	return newOpErr(codeInternal, err)
}

// Restore 从归档恢复挂起（design.md §19 Restore 行：纯 DB，状态须为 archived）。
//
// P1.4.4：注入 LifecycleService 时委托，未注入时回退 legacy 直连 store 路径。
func (m *Manager) Restore(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	if m.lifecycle != nil {
		return m.mapLifecycleRestoreErr(ctx, taskID)
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// guard 委托 domain/task.CanRestore（design D0 P1.4.2 strangler 第二步），byte-equivalent。
	if !rehydrateGuardView(row).CanRestore() {
		return newOpErr(codeInvalidState, fmt.Errorf("restore requires archived, got %s", row.Status))
	}
	if _, err := m.store.RestoreTask(ctx, taskID); err != nil {
		return newOpErr(codeInternal, err)
	}
	return nil
}

// mapLifecycleRestoreErr 委托 LifecycleService.Restore 并映射 typed error 为 OpError（逐字不变）。
func (m *Manager) mapLifecycleRestoreErr(ctx context.Context, taskID string) error {
	err := m.lifecycle.Restore(ctx, taskID)
	if err == nil {
		return nil
	}
	var re *apptask.RestoreError
	if errors.As(err, &re) {
		return newOpErr(codeInvalidState, errors.New(re.Error()))
	}
	if errors.Is(err, application.ErrTaskNotFound) {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	return newOpErr(codeInternal, err)
}

// Get 返回任务详情（design.md §18 Get）。
//
// P1.4.4：注入 LifecycleService 时委托（经 TaskReadRepository 读全行快照，还原为 TaskRow），
// 未注入时回退 legacy 直连 store 路径。OpError 映射逐字不变。
func (m *Manager) Get(ctx context.Context, taskID string) (TaskRow, error) {
	if m.lifecycle != nil {
		snap, err := m.lifecycle.Get(ctx, taskID)
		if err != nil {
			return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
		}
		return taskSnapshotToTaskRow(snap), nil
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	return row, nil
}

// List 返回项目下任务列表（design.md §18 List）。
//
// P1.4.4：注入 LifecycleService 时委托，未注入时回退 legacy 直连 store 路径。
func (m *Manager) List(ctx context.Context, projectID string) ([]TaskRow, error) {
	if m.lifecycle != nil {
		snaps, err := m.lifecycle.List(ctx, projectID)
		if err != nil {
			return nil, newOpErr(codeInternal, err)
		}
		out := make([]TaskRow, 0, len(snaps))
		for _, s := range snaps {
			out = append(out, taskSnapshotToTaskRow(s))
		}
		return out, nil
	}
	rows, err := m.store.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, newOpErr(codeInternal, err)
	}
	return rows, nil
}

// RerunInit 手动重跑 init 脚本（design.md §4/§6，tasks 3.6）。
// 持任务 keyed mutex；先 admission（gate 已关闭 → 返回错误且 init_status 不变）；
// 再门禁（status=suspended 且 init_status∈{failed, succeeded}，其余 invalid_state）
// + ClaimInitRerun CAS（竞争 conflict）；admission 后所有同步退出恰好一次释放。
// 成功 claim 后异步执行（不持锁）；成功不自动激活；覆盖 init.log。
// 返回 claim 后的任务行（init_status=running），供 API 层 200+DTO（Phase C）。
func (m *Manager) RerunInit(ctx context.Context, taskID string) (TaskRow, error) {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return TaskRow{}, err
	}

	// admission：gate 已关闭 → 返回错误且 init_status 不变。
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		unlock()
		return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("rerun init: shutdown in progress"))
	}
	m.runnerWG.Add(1)
	m.shutdownGateMu.Unlock()

	// admission 后所有同步退出路径恰好一次释放登记 + 解锁。
	released := false
	wgRelease := func() {
		if !released {
			released = true
			m.runnerWG.Done()
		}
	}
	unlocked := false
	unlockOnce := func() {
		if !unlocked {
			unlocked = true
			unlock()
		}
	}

	// 同步退出：任务不存在/门禁失败/store error/CAS 失败。
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		unlockOnce()
		wgRelease()
		return TaskRow{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	// 门禁：status=suspended 且 init_status∈{failed, succeeded}。
	if row.Status != StatusSuspended {
		unlockOnce()
		wgRelease()
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("rerun init requires suspended, got %s", row.Status))
	}
	if row.InitStatus != InitStatusFailed && row.InitStatus != InitStatusSucceeded {
		unlockOnce()
		wgRelease()
		return TaskRow{}, newOpErr(codeInvalidState, fmt.Errorf("rerun init requires init_status failed or succeeded, got %s", row.InitStatus))
	}
	// ClaimInitRerun CAS：竞争 conflict。
	claimed, cerr := m.writeClaimInitRerun(ctx, taskID)
	if cerr != nil {
		unlockOnce()
		wgRelease()
		return TaskRow{}, newOpErr(codeInternal, cerr)
	}
	if !claimed.Matched {
		// 并发下已被 claim 或状态已变。
		unlockOnce()
		wgRelease()
		return TaskRow{}, newOpErr(codeConflict, fmt.Errorf("rerun init: task %s state changed concurrently", taskID))
	}

	// 异步执行（不持锁）：释放 keyed mutex，脚本在 goroutine 内执行，wg 持有到落账完成。
	// 返回 claim 后的任务行（init_status=running，init_error 已由 ClaimInitRerun 清空），供调用方展示。
	// 用独立非取消 ctx 重试一次读取：request ctx 取消时仍能返回 claim 后的准确行；
	// 读取仍失败时 fallback 构造行（MUST 与 ClaimInitRerun 后置一致：InitStatus=running、InitError 清空、
	// UpdatedAt 取当前时间），避免 Phase C 返回 running+旧错误信息（design.md §3 ClaimInitRerun 后置）。
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	claimedRow, readErr := m.store.GetTask(readCtx, taskID)
	readCancel()
	unlockOnce()
	go m.runRerunInitAttempt(taskID, wgRelease)
	if readErr == nil && claimedRow.ID != "" {
		return claimedRow, nil
	}
	// fallback：构造 claim 后行（与 ClaimInitRerun 后置一致：running、init_error 清空）。
	fallback := row
	fallback.InitStatus = InitStatusRunning
	fallback.InitError = sql.NullString{}
	fallback.UpdatedAt = nowUnixI()
	return fallback, nil
}

// runRerunInitAttempt 移至 init_run.go（与 runInitAttempt 共享落账语义）。

// slugify 已迁移为导出版 Slugify（util.go），行为不变。

// rand4 生成 4 位 [a-z0-9] 随机串。
// Go 1.24 起 crypto/rand.Read 保证填充成功，失败时进程 fatal，故返回的 error 永远为 nil。
// 此处保留 error 返回值仅为可注入熵源（Manager.rand4Fn）的测试路径服务：生产路径不可达，
// 属防御性分支。
func rand4() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

// projectDirSlug 由项目名派生目录段：normalizeSlug(proj.Name)，空 → project-<id 前 8>，
// 截断 ≤50 后去尾部 -。
func projectDirSlug(proj ProjectRow) string {
	slug := normalizeSlug(proj.Name)
	if slug == "" {
		if len(proj.ID) >= 8 {
			slug = "project-" + proj.ID[:8]
		} else {
			slug = "project-" + proj.ID
		}
	}
	if len(slug) > 50 {
		slug = slug[:50]
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

// branchDirSlug 由分支名派生目录段：去 ocdeck/ 前缀，normalizeSlug 截断 ≤50，
// 去尾部 -，截空兜底 task。分支名本身不变——目录段只是派生展示。
func branchDirSlug(branch string) string {
	seg := branch
	if strings.HasPrefix(seg, "ocdeck/") {
		seg = strings.TrimPrefix(seg, "ocdeck/")
	}
	seg = normalizeSlug(seg)
	if len(seg) > 50 {
		seg = seg[:50]
		seg = strings.TrimRight(seg, "-")
	}
	if seg == "" {
		seg = "task"
	}
	return seg
}

// newWorktreePath 计算并校验新 worktree 路径（task 包唯一计算点，ai-worktree-naming）：
// <dataDir>/worktrees/<projectNameSlug>/<branchPathSlug>-<rand4>。
// 碰撞预检（落库前、无副作用）：rand4 → dest → os.Stat(dest) 已存在则重生（≤3 次）；
// 3 次均碰撞 → 返回错误；os.Stat 返回 IsNotExist 以外错误 → 直接返回错误。
func (m *Manager) newWorktreePath(proj ProjectRow, branch string) (string, error) {
	projSlug := projectDirSlug(proj)
	branchSlug := branchDirSlug(branch)
	base := filepath.Join(m.cfg.DataDir, "worktrees", projSlug)
	// rand4Fn 默认由 New 注入为 crypto/rand 实现；直接构造 &Manager{} 的测试需经
	// newManagerWithDataDir 等助手或显式赋值，此处 nil 防御回退包级 rand4 杜绝 panic。
	rand4Fn := m.rand4Fn
	if rand4Fn == nil {
		rand4Fn = rand4
	}
	for attempt := 0; attempt < 3; attempt++ {
		r, err := rand4Fn()
		if err != nil {
			return "", fmt.Errorf("rand4: %w", err)
		}
		dest := filepath.Join(base, branchSlug+"-"+r)
		info, err := os.Stat(dest)
		if err == nil {
			_ = info
			continue // 碰撞，重试
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat worktree dest: %w", err)
		}
		return dest, nil
	}
	return "", errors.New("worktree: path collision after 3 attempts")
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
