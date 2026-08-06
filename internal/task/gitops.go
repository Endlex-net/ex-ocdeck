package task

import (
	"context"
	"errors"
	"fmt"

	"ocdeck/internal/git"
)

// GitFileDTO 单文件状态（design.md §21 git/status，与 internal/api/git.go gitFileDTO 字段一致）。
// lane B 将按 internal/task.GitStatus/GitDiff/GitCommit/GitPush 签名重接 API，DTO 定义在 task 包。
type GitFileDTO struct {
	Path      string `json:"path"`
	X         string `json:"x"`
	Y         string `json:"y"`
	Staged    bool   `json:"staged"`
	Unstaged  bool   `json:"unstaged"`
	Untracked bool   `json:"untracked"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsBinary  bool   `json:"isBinary"`
}

// GitStatusDTO status 响应（含当前分支，design.md §21 git/status）。
type GitStatusDTO struct {
	Branch string       `json:"branch"`
	Files  []GitFileDTO `json:"files"`
}

// GitDiffDTO diff 响应（unified diff 文本 + 截断标记，design.md §21 git/diff）。
type GitDiffDTO struct {
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

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
// 复用 internal/git.Status + git.CurrentBranch；只读，不进 repo 写锁。
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

// GitDiff 返回任务 worktree 中 path 相对 ref 的 unified diff（design.md §9/§21）。
// 持任务锁（与生命周期操作互斥，冲突 409）。ref 可选（空=工作区 vs 索引/HEAD），
// path 可选（空=全仓 diff，受 git.DiffMaxFiles 限制）。
// untracked=true 时调用 git.DiffUntracked 合成新文件 diff；此时 path 必填、ref 必空，
// 否则返回 invalid_input（在任何 git 命令前校验）。
func (m *Manager) GitDiff(ctx context.Context, taskID, ref, path string, untracked bool) (GitDiffDTO, error) {
	unlock, err := m.tryLockTask(taskID)
	if err != nil {
		return GitDiffDTO{}, err
	}
	defer unlock()

	// 用例不变量（在任何 git 命令前校验）：untracked 模式下 path 必填、ref 必空。
	if untracked {
		if path == "" {
			return GitDiffDTO{}, newOpErr(codeInvalidInput, errors.New("untracked diff requires a path"))
		}
		if ref != "" {
			return GitDiffDTO{}, newOpErr(codeInvalidInput, errors.New("untracked diff does not accept a ref"))
		}
	}

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return GitDiffDTO{}, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.WorktreePath == "" {
		return GitDiffDTO{}, newOpErr(codeInvalidState, fmt.Errorf("task %s has no worktree", taskID))
	}
	// add-plain-dir-project D5：dir 项目 git 操作降级——MUST 在任何 git 命令前拒绝。
	if _, err := m.assertGitRepoTask(ctx, row); err != nil {
		return GitDiffDTO{}, err
	}

	if untracked {
		diff, truncated, derr := git.DiffUntracked(ctx, row.WorktreePath, path)
		if derr != nil {
			if errors.Is(derr, git.ErrInvalidDiffPath) {
				return GitDiffDTO{}, newOpErr(codeInvalidInput, derr)
			}
			return GitDiffDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(derr)))
		}
		return GitDiffDTO{Diff: diff, Truncated: truncated}, nil
	}

	diff, truncated, derr := git.Diff(ctx, row.WorktreePath, ref, path)
	if derr != nil {
		return GitDiffDTO{}, newOpErr(codeGitError, fmt.Errorf("%s", git.StderrOf(derr)))
	}
	return GitDiffDTO{Diff: diff, Truncated: truncated}, nil
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
