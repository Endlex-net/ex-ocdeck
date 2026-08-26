package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultBranch(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	b, err := ResolveDefaultBranch(ctx, repo)
	if err != nil {
		t.Fatalf("ResolveDefaultBranch: %v", err)
	}
	if b != "main" && b != "master" {
		t.Errorf("default branch = %q, want main or master", b)
	}
}

func TestResolveDefaultBranch_DetachedFails(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	// detach HEAD
	runGit(t, repo, "checkout", "-q", "--detach", "HEAD")
	_, err := ResolveDefaultBranch(ctx, repo)
	if err == nil {
		t.Fatal("expected error for detached HEAD")
	}
}

func TestIsGitRepo_RealRepo(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	ok, err := IsGitRepo(ctx, repo)
	if err != nil {
		t.Fatalf("IsGitRepo: %v", err)
	}
	if !ok {
		t.Error("want true for real repo")
	}
}

func TestIsGitRepo_NonRepoDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ok, err := IsGitRepo(ctx, dir)
	if err != nil {
		t.Fatalf("IsGitRepo: %v", err)
	}
	if ok {
		t.Error("want false for plain dir")
	}
}

func TestIsGitRepo_NonexistentPath(t *testing.T) {
	ctx := context.Background()
	ok, err := IsGitRepo(ctx, "/nonexistent-xyz-abc")
	if err != nil {
		t.Fatalf("IsGitRepo: %v", err)
	}
	if ok {
		t.Error("want false for nonexistent path")
	}
}

func TestWorktreeDir_ResolveMainRepo(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	wtPath := filepath.Join(t.TempDir(), "wt1")
	if err := WorktreeAdd(ctx, repo, wtPath, "ocdeck/wt-test", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	t.Cleanup(func() {
		_ = WorktreeRemove(ctx, repo, wtPath, false)
		_ = DeleteBranch(ctx, repo, "ocdeck/wt-test")
		_ = WorktreePrune(ctx, repo)
	})
	main, err := WorktreeDir(ctx, wtPath)
	if err != nil {
		t.Fatalf("WorktreeDir: %v", err)
	}
	// canonical 比较。
	repoCanon, _ := filepath.EvalSymlinks(repo)
	if main != repoCanon {
		t.Errorf("WorktreeDir = %q, want %q", main, repoCanon)
	}
}

func TestBranchCheckedOutByOther(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	wt1 := filepath.Join(t.TempDir(), "wt1")
	if err := WorktreeAdd(ctx, repo, wt1, "ocdeck/bo", "main"); err != nil {
		t.Fatalf("add wt1: %v", err)
	}
	t.Cleanup(func() {
		_ = WorktreeRemove(ctx, repo, wt1, false)
		_ = DeleteBranch(ctx, repo, "ocdeck/bo")
		_ = WorktreePrune(ctx, repo)
	})
	// 排除 wt1 自身 → 应返回 false（无其他 worktree 占用）。
	checked, err := BranchCheckedOutByOther(ctx, repo, "ocdeck/bo", wt1)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if checked {
		t.Error("want false when only self checks out branch")
	}
	// 不排除 wt1 → 应返回 true（被 wt1 占用）。
	checked2, err := BranchCheckedOutByOther(ctx, repo, "ocdeck/bo", "")
	if err != nil {
		t.Fatalf("check2: %v", err)
	}
	if !checked2 {
		t.Error("want true when branch is checked out (no exclude)")
	}
}

func TestIsWorktreeDirty_CleanAndDirty(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	wt := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, repo, wt, "ocdeck/dirty", "main"); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() {
		_ = WorktreeRemove(ctx, repo, wt, true)
		_ = DeleteBranch(ctx, repo, "ocdeck/dirty")
		_ = WorktreePrune(ctx, repo)
	})
	dirty, err := IsWorktreeDirty(ctx, wt)
	if err != nil {
		t.Fatalf("check clean: %v", err)
	}
	if dirty {
		t.Error("clean worktree reported dirty")
	}
	_ = os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644)
	dirty2, err := IsWorktreeDirty(ctx, wt)
	if err != nil {
		t.Fatalf("check dirty: %v", err)
	}
	if !dirty2 {
		t.Error("dirty worktree reported clean")
	}
}

func TestResolveRef_Valid(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	oid, err := ResolveRef(ctx, repo, "main")
	if err != nil {
		t.Fatalf("ResolveRef main: %v", err)
	}
	if oid == "" {
		t.Error("empty oid")
	}
}

func TestResolveRef_Nonexistent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	if _, err := ResolveRef(ctx, repo, "refs/heads/nonexistent"); err == nil {
		t.Error("nonexistent ref should error")
	}
}

func TestResolveRef_OptionInjectionRejected(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	evilPath := filepath.Join(repo, "evil.txt")
	// --output=... 应被 --end-of-options 拒绝（对齐 Diff 的 ref 注入防御）。
	if _, err := ResolveRef(ctx, repo, "--output="+evilPath); err == nil {
		t.Fatal("expected option injection to be rejected")
	}
	if _, err := os.Stat(evilPath); err == nil {
		t.Fatalf("evil file created by ref injection: %s", evilPath)
	}
}

func TestDeleteBranch_Idempotent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	// 不存在的分支应视为成功（幂等）。
	if err := DeleteBranch(ctx, repo, "ocdeck/never-existed"); err != nil {
		t.Errorf("DeleteBranch nonexistent should be idempotent: %v", err)
	}
}

func TestDeleteBranch_DashTerminatesOptions(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	// 验证 `--` 终止选项不破坏正常分支删除：先建分支（不经 worktree，用 git branch）再删。
	runGit(t, repo, "branch", "ocdeck/opt-test")
	if err := DeleteBranch(ctx, repo, "ocdeck/opt-test"); err != nil {
		t.Errorf("DeleteBranch with -- should succeed: %v", err)
	}
}