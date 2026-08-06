package task

// fix-git-diff-new-file-and-linenum 任务 1.5：Manager.GitDiff untracked 用例校验。
// 覆盖：
//   - untracked=true && path 空 → invalid_input（在任何 git 命令前）
//   - untracked=true && ref 非空 → invalid_input（在任何 git 命令前）
//   - 上述两种均未执行 git 命令（WorktreePath 指向不存在路径，若 git 被执行会返回 git_error 而非 invalid_input）
//   - git.ErrInvalidDiffPath 映射 invalid_input（oracle 非阻塞建议）

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGitDiff_Untracked_RequiresPath 验证 untracked=true && path 空 → invalid_input，
// 且未执行 git 命令（WorktreePath 指向不存在路径，若 git 被执行会返回 git_error 而非 invalid_input）。
func TestGitDiff_Untracked_RequiresPath(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // WorktreePath=/data/worktrees/...（不存在）
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "", "", true)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("untracked+empty path: err = %v, want codeInvalidInput", err)
	}
}

// TestGitDiff_Untracked_RejectsRef 验证 untracked=true && ref 非空 → invalid_input，未执行 git 命令。
func TestGitDiff_Untracked_RejectsRef(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	_, err := m.GitDiff(context.Background(), "t1", "HEAD", "a.txt", true)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("untracked+non-empty ref: err = %v, want codeInvalidInput", err)
	}
}

// TestGitDiff_Untracked_InvalidPathMapsInvalidInput 验证 git.ErrInvalidDiffPath 映射 invalid_input。
// 用真实 worktree + 绝对路径触发 git.ErrInvalidDiffPath（绝对路径被 git 层防御性拒绝）。
func TestGitDiff_Untracked_InvalidPathMapsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)

	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: dir, DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task",
		Status: StatusSuspended, WorktreePath: dir, InitStatus: InitStatusNone}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 绝对路径触发 git.ErrInvalidDiffPath → invalid_input。
	absPath := filepath.Join(dir, "x.txt")
	_, err := m.GitDiff(context.Background(), "t1", "", absPath, true)
	if !isOpErrCode(err, codeInvalidInput) {
		t.Fatalf("untracked+absolute path: err = %v, want codeInvalidInput (mapped from ErrInvalidDiffPath)", err)
	}
}

// initTestGitRepo 在 dir 初始化一个 git 仓库供测试使用（建立 HEAD）。
func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGitInit(t, dir, "init", "-q")
	runGitInit(t, dir, "config", "user.email", "t@t.com")
	runGitInit(t, dir, "config", "user.name", "tester")
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitInit(t, dir, "add", "README.md")
	runGitInit(t, dir, "commit", "-qm", "init")
}

func runGitInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
