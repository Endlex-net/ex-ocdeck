package task

import (
	"context"
	"errors"
	"fmt"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/git"
)

// GitFileDTO 单文件状态（design.md §21 git/status，与 internal/api/git.go gitFileDTO 字段一致）。
// lane B 将按 internal/task.GitStatus/GitDiff/GitCommit/GitPush 签名重接 API，DTO 定义在 task 包。
type GitFileDTO = application.GitFileDTO

// GitStatusDTO status 响应（含当前分支，design.md §21 git/status）。
type GitStatusDTO = application.GitStatusDTO

// GitDiffDTO diff 响应（单文件两侧版本内容八字段契约，design.md §21 git/diff）。
type GitDiffDTO = application.GitDiffDTO

// assertGitRepoTask 解析任务所属项目 kind，校验该任务可作为 git 操作目标（add-plain-dir-project D5）。
// 在执行任何 git 命令前调用：dir 项目 → codeInvalidInput（明确"project kind is dir (not a git repository)"），
// 未知 kind → codeInvalidInput fail-closed。repo 项目通过，返回 ProjectRow 供后续 git 命令使用。
func (m *Manager) assertGitRepoTask(ctx context.Context, row TaskRow) (ProjectRow, error) {
	proj, err := m.store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return ProjectRow{}, newOpErr(codeNotFound, fmt.Errorf("project not found: %w", err))
	}
	switch proj.Kind {
	case ProjectKindRepo:
		return proj, nil
	case ProjectKindDir:
		return ProjectRow{}, newOpErr(codeInvalidInput, errors.New("project kind is dir (not a git repository)"))
	default:
		// 未知持久化 kind（DB 损坏值）→ internal（D1：区别于用户请求非法 kind 的 invalid_input）。
		return ProjectRow{}, newOpErr(codeInternal, fmt.Errorf("unknown project kind %q", proj.Kind))
	}
}

// GitStatus 返回任务 worktree 的 git 状态（design.md §9/§21）。
// 持任务锁（与 Suspend/Delete 等生命周期操作互斥，冲突返回 409）防并发。
// 复用 internal/infrastructure/git.Status + git.CurrentBranch；只读，不进 repo 写锁。
// not_found 语义与现有一致：任务不存在返回 codeNotFound。
func (m *Manager) GitStatus(ctx context.Context, taskID string) (GitStatusDTO, error) {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return GitStatusDTO{}, err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return GitStatusDTO{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return GitStatusDTO{}, newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	// add-plain-dir-project D5：dir 项目 git 操作降级——MUST 在任何 git 命令前拒绝。
	if _, err := m.assertGitRepoTask(ctx, row); err != nil {
		return GitStatusDTO{}, err
	}
	// detached HEAD 或 git 不可用：分支留空，不阻断 status（与 api/git.go 一致）。
	branch, berr := git.CurrentBranch(ctx, row.WorktreePath)
	if berr != nil {
		branch = ""
	}
	entries, serr := git.Status(ctx, row.WorktreePath)
	if serr != nil {
		return GitStatusDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(serr)))
	}
	files := make([]GitFileDTO, 0, len(entries))
	for _, e := range entries {
		files = append(files, GitFileDTO{
			Path: e.Path, X: string(e.X), Y: string(e.Y),
			Staged: e.Staged, Unstaged: e.Unstaged, Untracked: e.Untracked,
			Additions: e.Additions, Deletions: e.Deletions, IsBinary: e.IsBinary,
		})
	}
	return GitStatusDTO{Branch: branch, Files: files}, nil
}

// GitDiff 返回任务 worktree 中单文件 path 的两侧版本内容（design.md §9/§21、
// codemirror-git-diff design D10）。持任务锁（与生命周期操作互斥，冲突 409）。
// 固定六阶段，多源失败返回首个：①纯词法校验（untracked 组合约束、path 非空、
// 绝对路径/`..`/NUL，先于任务锁与任何 git 命令/文件读取）→ ②task/worktree/repo kind 校验 →
// ③ref rev-parse 解析 → ④旧侧探测+读取 → ⑤新侧读取 → ⑥DTO 组装。
// 内容来源：untracked=1（调用方声明模式，不二次探测）→ 旧侧不存在、新侧为工作区文件；
// ref 非空 → ref OID 下 blob；ref 空 → index stage-0（无 stage-0 有其他 stage → invalid_state）。
// 错误矩阵：词法非法/新侧真实路径逃逸 → invalid_input；未解决冲突 → invalid_state；
// ref 解析与旧侧 git 失败、子模块 dirty 探测失败 → git_error（透传 stderr）；
// 新侧非 ENOENT IO 错误 → internal。
func (m *Manager) GitDiff(ctx context.Context, taskID, ref, path string, untracked bool) (GitDiffDTO, error) {
	// 阶段①：纯词法校验。untracked 组合约束沿用既有消息；path 校验与工作区读取同源
	//（git.ValidateDiffPath）。MUST 在任务锁与任何 git 命令/文件读取之前完成。
	if untracked {
		if path == "" {
			return GitDiffDTO{}, newOpErr(codeInvalidInput, errors.New("untracked diff requires a path"))
		}
		if ref != "" {
			return GitDiffDTO{}, newOpErr(codeInvalidInput, errors.New("untracked diff does not accept a ref"))
		}
	}
	if err := git.ValidateDiffPath(path); err != nil {
		return GitDiffDTO{}, newOpErr(codeInvalidInput, err)
	}

	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return GitDiffDTO{}, err
	}
	defer unlock()

	// 阶段②：task 存在性与 worktree/repo kind 校验（在任何 git 命令前完成）。
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return GitDiffDTO{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return GitDiffDTO{}, newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	if _, err := m.assertGitRepoTask(ctx, row); err != nil {
		return GitDiffDTO{}, err
	}

	// 阶段③：ref 解析（词法校验全部通过后执行；untracked 模式已保证 ref 为空）。
	oid := ""
	if ref != "" {
		oid, err = git.ResolveRefOID(ctx, row.WorktreePath, ref)
		if err != nil {
			return GitDiffDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(err)))
		}
	}

	// 阶段④：旧侧探测+读取。untracked=1 为调用方声明的展示模式，旧侧不存在、零 git 探测。
	var oldSide git.SideContent
	if untracked {
		oldSide = git.SideContent{}
	} else if oid != "" {
		oldSide, err = git.ReadRefSideContent(ctx, row.WorktreePath, oid, path)
	} else {
		oldSide, err = git.ReadIndexSideContent(ctx, row.WorktreePath, path)
	}
	if err != nil {
		if errors.Is(err, git.ErrUnmergedPath) {
			return GitDiffDTO{}, newOpErr(codeInvalidState, err)
		}
		return GitDiffDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(err)))
	}

	// 阶段⑤：新侧（工作区）读取。ENOENT 竞态/fifo 等非 regular 类型在读取函数内归为
	// 新侧不存在；symlink/directory 分别按链接文本/gitlink（rev-parse HEAD）处理；
	// 子模块 dirty 探测（status --porcelain）失败 → git_error（透传 stderr）。
	newSide, err := git.ReadWorktreeSideContent(ctx, row.WorktreePath, path)
	if err != nil {
		if errors.Is(err, git.ErrInvalidDiffPath) || errors.Is(err, git.ErrWorktreeEscape) {
			return GitDiffDTO{}, newOpErr(codeInvalidInput, err)
		}
		if errors.Is(err, git.ErrSubmoduleDirtyProbe) {
			return GitDiffDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(err)))
		}
		return GitDiffDTO{}, newOpErr(codeInternal, err)
	}

	// 阶段⑥：DTO 组装。isBinary=任一侧二进制，置位后清空两侧内容但不改变 truncated。
	dto := GitDiffDTO{
		OldContent: oldSide.Content,
		NewContent: newSide.Content,
		OldExists:  oldSide.Exists,
		NewExists:  newSide.Exists,
		OldMode:    oldSide.Mode,
		NewMode:    newSide.Mode,
		IsBinary:   oldSide.IsBinary || newSide.IsBinary,
		Truncated:  oldSide.Truncated || newSide.Truncated,
	}
	if dto.IsBinary {
		dto.OldContent = ""
		dto.NewContent = ""
	}
	return dto, nil
}

// GitCommit 在任务 worktree 中暂存 paths（非空时）并以 message 提交（design.md §9/§21）。
// 持任务锁（与生命周期操作互斥，冲突 409）。保留 hooks/签名行为，错误原样透传 git stderr。
// message 为空返回 invalid_input；paths 为空表示提交全部改动（git.Commit 语义）。
func (m *Manager) GitCommit(ctx context.Context, taskID, message string, paths []string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	if message == "" {
		return newOpErr(codeInvalidInput, fmt.Errorf("commit message is required"))
	}
	// add-plain-dir-project D5：dir 项目 git 操作降级——MUST 在任何 git 命令前拒绝。
	if _, err := m.assertGitRepoTask(ctx, row); err != nil {
		return err
	}
	if cerr := git.Commit(ctx, row.WorktreePath, message, paths); cerr != nil {
		return newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(cerr)))
	}
	return nil
}

// GitPush 推送任务 worktree 所在分支到 origin（design.md §9/§21）。
// 持任务锁（与生命周期操作互斥，冲突 409）。git push -u origin <branch>，MUST NOT force-push。
func (m *Manager) GitPush(ctx context.Context, taskID string) error {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	// add-plain-dir-project D5：dir 项目 git 操作降级——MUST 在任何 git 命令前拒绝。
	if _, err := m.assertGitRepoTask(ctx, row); err != nil {
		return err
	}
	branch, berr := git.CurrentBranch(ctx, row.WorktreePath)
	if berr != nil {
		return newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(berr)))
	}
	if perr := git.Push(ctx, row.WorktreePath, branch); perr != nil {
		return newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(perr)))
	}
	return nil
}
