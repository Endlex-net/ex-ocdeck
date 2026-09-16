// rename_branch_test.go 验证 RenameBranch 原语与 WorktreeHeadBranch（task-info-editable 2.1）：
// 真实 git worktree 改名——HEAD 跟随、路径不变、reflog 保留、目标冲突/非法名零副作用。
package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestWorktree 在 repo 上创建分支并添加 worktree，返回 worktree 路径。
func newTestWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "branch", branch)
	runGit(t, repo, "worktree", "add", wt, branch)
	return wt
}

// TestRenameBranch_RealRepo_HeadFollowsReflogPreservedPathUnchanged 验证改名成功路径：
// HEAD 跟随新名、HEAD 提交不变（路径不变）、reflog 保留、旧 ref 移除。
func TestRenameBranch_RealRepo_HeadFollowsReflogPreservedPathUnchanged(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wt := newTestWorktree(t, repo, "ocdeck/old-slug")
	// 在 worktree 中提交一次（产生分支 reflog 条目，供改名后断言 reflog 保留）。
	runGit(t, wt, "commit", "--allow-empty", "-qm", "wip")
	headBefore, err := ResolveRef(ctx, wt, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD before: %v", err)
	}

	if err := RenameBranch(ctx, wt, "ocdeck/old-slug", "ocdeck/new-slug"); err != nil {
		t.Fatalf("RenameBranch: %v", err)
	}

	// HEAD 跟随：worktree 检出分支已是新名。
	if cur, err := CurrentBranch(ctx, wt); err != nil || cur != "ocdeck/new-slug" {
		t.Fatalf("CurrentBranch after rename = %q, %v; want ocdeck/new-slug", cur, err)
	}
	// HEAD 提交不变（worktree 路径与提交内容不变，仅分支引用名变化）。
	headAfter, err := ResolveRef(ctx, wt, "HEAD")
	if err != nil || headAfter != headBefore {
		t.Fatalf("HEAD moved: before=%s after=%s err=%v", headBefore, headAfter, err)
	}
	// 新分支存在、旧分支不存在（本地 ref 移动）。
	if ok, err := RefExists(ctx, repo, "refs/heads/ocdeck/new-slug"); err != nil || !ok {
		t.Fatalf("refs/heads/ocdeck/new-slug exists = %v, %v; want true", ok, err)
	}
	if ok, err := RefExists(ctx, repo, "refs/heads/ocdeck/old-slug"); err != nil || ok {
		t.Fatalf("refs/heads/ocdeck/old-slug exists = %v, %v; want false", ok, err)
	}
	// reflog 保留：新分支的 reflog 文件随改名迁移（worktree 分支 reflog 位于主仓库
	// common git dir 的 logs/refs/heads/），且含改名前的提交条目。
	reflog, err := os.ReadFile(filepath.Join(repo, ".git", "logs", "refs", "heads", "ocdeck", "new-slug"))
	if err != nil || !strings.Contains(string(reflog), "wip") {
		t.Fatalf("reflog of new branch = %q, %v; want preserved 'wip' entry", string(reflog), err)
	}
}

// TestRenameBranch_RealRepo_ConflictZeroSideEffect 验证目标分支已存在 → 报错且零副作用
// （旧分支保留、HEAD 不变、目标分支未被覆盖）。
func TestRenameBranch_RealRepo_ConflictZeroSideEffect(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wt := newTestWorktree(t, repo, "ocdeck/old-slug")
	runGit(t, repo, "branch", "ocdeck/taken")

	err := RenameBranch(ctx, wt, "ocdeck/old-slug", "ocdeck/taken")
	if err == nil {
		t.Fatal("RenameBranch to existing branch: want error, got nil")
	}
	if cur, cerr := CurrentBranch(ctx, wt); cerr != nil || cur != "ocdeck/old-slug" {
		t.Fatalf("HEAD changed after conflict: %q, %v", cur, cerr)
	}
	if ok, eerr := RefExists(ctx, repo, "refs/heads/ocdeck/old-slug"); eerr != nil || !ok {
		t.Fatalf("old branch must survive: exists=%v err=%v", ok, eerr)
	}
}

// TestRenameBranch_RealRepo_InvalidNameZeroSideEffect 验证非法分支名 → 报错且零副作用。
func TestRenameBranch_RealRepo_InvalidNameZeroSideEffect(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wt := newTestWorktree(t, repo, "ocdeck/old-slug")

	err := RenameBranch(ctx, wt, "ocdeck/old-slug", "bad..name")
	if err == nil {
		t.Fatal("RenameBranch to invalid name: want error, got nil")
	}
	if cur, cerr := CurrentBranch(ctx, wt); cerr != nil || cur != "ocdeck/old-slug" {
		t.Fatalf("HEAD changed after invalid name: %q, %v", cur, cerr)
	}
	if ok, eerr := RefExists(ctx, repo, "refs/heads/ocdeck/old-slug"); eerr != nil || !ok {
		t.Fatalf("old branch must survive: exists=%v err=%v", ok, eerr)
	}
}

// TestWorktreeHeadBranch_BelongsAndIndeterminable 验证共用检查（仓库归属 + symbolic HEAD 读取）：
// 归属 worktree 返回短分支名；不属于该 repo / detached / 路径缺失均返回错误（无法明确判定）。
func TestWorktreeHeadBranch_BelongsAndIndeterminable(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wt := newTestWorktree(t, repo, "ocdeck/feature")

	if got, err := WorktreeHeadBranch(ctx, repo, wt); err != nil || got != "ocdeck/feature" {
		t.Fatalf("WorktreeHeadBranch = %q, %v; want ocdeck/feature", got, err)
	}

	// 不属于该 repo 的 worktree：主 repo 侧查询另一个仓库的 worktree → 错误。
	other := newTestRepo(t)
	otherWt := newTestWorktree(t, other, "b2")
	if _, err := WorktreeHeadBranch(ctx, repo, otherWt); err == nil {
		t.Fatal("WorktreeHeadBranch foreign worktree: want error, got nil")
	}

	// detached HEAD：无法明确判定 → 错误。
	runGit(t, wt, "checkout", "-q", "--detach", "HEAD")
	if _, err := WorktreeHeadBranch(ctx, repo, wt); err == nil {
		t.Fatal("WorktreeHeadBranch detached HEAD: want error, got nil")
	}

	// worktree 路径缺失 → 错误。
	if _, err := WorktreeHeadBranch(ctx, repo, filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("WorktreeHeadBranch missing path: want error, got nil")
	}
}
