package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeDir 返回 worktree 所属的主仓库目录。
// 在 worktree 内运行 git rev-parse --git-common-dir 得到主仓库 .git 路径，
// 其父目录即主仓库工作区根。
func WorktreeDir(ctx context.Context, wtPath string) (string, error) {
	out, _, err := run(ctx, wtPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git worktree: resolve common dir in %s: %w", wtPath, err)
	}
	commonDir := strings.TrimSpace(out)
	if commonDir == "" {
		return "", errors.New("git worktree: empty git-common-dir")
	}
	abs := commonDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(wtPath, commonDir)
	}
	return filepath.Dir(abs), nil
}

// WorktreeAdd 在 repoPath 下创建 worktree 到 destPath，基于 baseRef 新建 branch。
// 调用方 MUST 先持有 RepoLock(repoPath) 并执行 prune。branch 须已通过 check-ref-format 校验。
// baseRef 先经 rev-parse --verify --end-of-options 解析为 OID 再使用，防止 option 注入
// 并提前校验 ref 存在（design.md §9 安全边界，对齐 Diff 的 ref 处理）。
func WorktreeAdd(ctx context.Context, repoPath, destPath, branch, baseRef string) error {
	if branch == "" {
		return errors.New("git worktree: empty branch")
	}
	if baseRef == "" {
		return errors.New("git worktree: empty base ref")
	}
	oid, err := ResolveRef(ctx, repoPath, baseRef)
	if err != nil {
		return err
	}
	_, _, err = run(ctx, repoPath, "worktree", "add", destPath, "-b", branch, oid)
	if err != nil {
		return err
	}
	return nil
}

// ResolveRef 将 ref 解析为完整 OID（git rev-parse --verify --end-of-options）。
// --end-of-options 防止 ref 形如 "--output=..." 的 option 注入（design.md §9）。
// ref 不存在或为 option 时返回错误。
func ResolveRef(ctx context.Context, repoPath, ref string) (string, error) {
	out, _, err := run(ctx, repoPath, "rev-parse", "--verify", "--end-of-options", ref)
	if err != nil {
		return "", fmt.Errorf("git: invalid ref %q: %w", ref, err)
	}
	oid := strings.TrimSpace(out)
	if oid == "" {
		return "", fmt.Errorf("git: rev-parse returned empty for ref %q", ref)
	}
	return oid, nil
}

// WorktreeRemove 删除 worktree。force=true 时忽略 dirty/untracked。
// worktree 不存在视为已成功（幂等，design.md §19 资源不存在视为已成功）。
func WorktreeRemove(ctx context.Context, repoPath, wtPath string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wtPath)
	_, _, err := run(ctx, repoPath, args...)
	if err != nil {
		if isWorktreeNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// isWorktreeNotExist 判断 worktree remove 错误是否为"worktree 不存在"（幂等成功）。
// git 输出形如 "fatal: '<path>' is not a working tree" 或 "'<path>' does not exist"。
func isWorktreeNotExist(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not a working tree") || strings.Contains(msg, "does not exist")
}

// WorktreePrune 清理已失效的 worktree 元数据（design.md §17：add 前、remove 后）。
func WorktreePrune(ctx context.Context, repoPath string) error {
	_, _, err := run(ctx, repoPath, "worktree", "prune")
	return err
}

// DeleteBranch 删除本地分支（-D 强制）。需在主仓库或非该分支检出的 worktree 中执行。
// 分支不存在视为已成功（幂等，design.md §19 资源不存在视为已成功）。
// `--` 终止选项，防止 branch 名被解释为选项。
func DeleteBranch(ctx context.Context, repoPath, branch string) error {
	if branch == "" {
		return errors.New("git: empty branch name")
	}
	_, _, err := run(ctx, repoPath, "branch", "-D", "--", branch)
	if err != nil {
		if isBranchNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// isBranchNotFound 判断 branch -D 错误是否为"分支不存在"（幂等成功）。
// git 输出形如 "error: branch 'foo' not found"。
func isBranchNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// IsWorktreeDirty 返回 worktree 是否存在 dirty 或 untracked 文件。
// porcelain v2 -z 输出非空即视为 dirty（含 untracked ? 条目）。
func IsWorktreeDirty(ctx context.Context, wtPath string) (bool, error) {
	out, _, err := run(ctx, wtPath, "status", "--porcelain=v2", "-z")
	if err != nil {
		return false, err
	}
	return strings.TrimRight(out, "\x00") != "", nil
}

// BranchCheckedOutByOther 返回 branch 是否被除 excludeWtPath 之外的某个 worktree 检出。
// 用于删除前置检查：正在被删除的 worktree 自身检出该分支不算"占用"。
// 枚举主仓库的 worktree list，匹配 branch refs/heads/<branch> 且路径不等于 excludeWtPath。
func BranchCheckedOutByOther(ctx context.Context, repoPath, branch, excludeWtPath string) (bool, error) {
	if branch == "" {
		return false, errors.New("git: empty branch name")
	}
	out, _, err := run(ctx, repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := "branch refs/heads/" + branch
	excludeCanon := ""
	if excludeWtPath != "" {
		if r, e := filepath.EvalSymlinks(excludeWtPath); e == nil {
			excludeCanon = r
		} else {
			excludeCanon = excludeWtPath
		}
	}
	var curWt string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			curWt = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			continue
		}
		if line == want {
			if excludeCanon != "" {
				if curCanon, e := filepath.EvalSymlinks(curWt); e == nil && curCanon == excludeCanon {
					continue
				}
				if curWt == excludeWtPath {
					continue
				}
			}
			return true, nil
		}
	}
	return false, nil
}

// ResolveDefaultBranch 探测 repoPath 的默认分支（HEAD 指向）。
// 优先 git symbolic-ref --short HEAD；失败回退 rev-parse --abbrev-ref HEAD。
func ResolveDefaultBranch(ctx context.Context, repoPath string) (string, error) {
	out, _, err := run(ctx, repoPath, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b, nil
		}
	}
	out, _, err = run(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: detect default branch: %w", err)
	}
	b := strings.TrimSpace(out)
	if b == "" || b == "HEAD" {
		return "", fmt.Errorf("git: cannot resolve default branch (detached HEAD) in %s", repoPath)
	}
	return b, nil
}

// IsGitRepo 校验 path 存在且为 git 仓库：含 .git 或 git rev-parse 成功。
func IsGitRepo(ctx context.Context, path string) (bool, error) {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return true, nil
	}
	// .git 文件（worktree）或无 .git 目录时，用 rev-parse 兜底。
	_, _, err := run(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err == nil {
		return true, nil
	}
	return false, nil
}

// CurrentBranch 返回 wtPath 当前检出的分支名（git rev-parse --abbrev-ref HEAD）。
// detached HEAD 返回 "HEAD" 与错误，便于调用方判定。
func CurrentBranch(ctx context.Context, wtPath string) (string, error) {
	out, _, err := run(ctx, wtPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: current branch in %s: %w", wtPath, err)
	}
	b := strings.TrimSpace(out)
	if b == "" || b == "HEAD" {
		return "", fmt.Errorf("git: detached HEAD in %s", wtPath)
	}
	return b, nil
}

// VerifyWorktreeProduct 严格校验 worktree 产物（B1：RetryCreate 幂等跳过 add 的判定依据，
// design.md §19 Create Retry 行）。校验项：路径存在 + .git 文件/目录 + rev-parse --is-inside-work-tree
// + 检出分支匹配 + 属预期 repo（git-common-dir 父目录匹配 repoPath）。
func VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		return fmt.Errorf("worktree product: path missing: %s", wtPath)
	}
	if _, err := os.Stat(filepath.Join(wtPath, ".git")); err != nil {
		return fmt.Errorf("worktree product: .git missing in %s: %w", wtPath, err)
	}
	if _, _, err := run(ctx, wtPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return fmt.Errorf("worktree product: not inside work tree: %w", err)
	}
	cur, err := CurrentBranch(ctx, wtPath)
	if err != nil {
		return fmt.Errorf("worktree product: %w", err)
	}
	if cur != branch {
		return fmt.Errorf("worktree product: branch mismatch (got %s, want %s)", cur, branch)
	}
	// 属预期 repo：git-common-dir 父目录应匹配 repoPath。
	repoDir, err := WorktreeDir(ctx, wtPath)
	if err != nil {
		return fmt.Errorf("worktree product: %w", err)
	}
	repoCanon, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		repoCanon = repoPath
	}
	gotCanon, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		gotCanon = repoDir
	}
	if repoCanon != gotCanon {
		return fmt.Errorf("worktree product: repo mismatch (worktree belongs to %s, expected %s)", gotCanon, repoCanon)
	}
	return nil
}