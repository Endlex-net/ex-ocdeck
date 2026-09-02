package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ocdeck/internal/infrastructure/git"
)

// helper: 在 t.TempDir() 创建真实 git 仓库，返回仓库路径。
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@t.com")
	runGit(t, dir, "config", "user.name", "tester")
	writeFile(t, dir, "README.md", "init\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-qm", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdd_Success(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()

	dest := filepath.Join(mgr.dataDir, "worktrees", "proj1", "task1")
	if err := mgr.Add(ctx, repo, dest, "ocdeck/task-1", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("worktree .git missing: %v", err)
	}
}

func TestAdd_EmptyBranchRejected(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	dest := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	for _, br := range []string{"", "  "} {
		if err := mgr.Add(ctx, repo, dest, br, "main"); err == nil {
			t.Errorf("branch %q should be rejected", br)
		}
	}
}

func TestAdd_BranchConflict(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	d1 := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, d1, "ocdeck/same", "main"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	// 同分支名再次 add 应失败。
	d2 := filepath.Join(mgr.dataDir, "worktrees", "p1", "t2")
	if err := mgr.Add(ctx, repo, d2, "ocdeck/same", "main"); err == nil {
		t.Fatal("second Add with same branch should fail")
	}
}

func TestAdd_FailureCleansHalfBaked(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	dest := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	// 无效分支名让 add 失败。
	if err := mgr.Add(ctx, repo, dest, "ocdeck/bad..name", "main"); err == nil {
		t.Fatal("expected error for invalid branch name")
	}
	// 目标目录不应残留。
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("half-baked dir should be cleaned: %s", dest)
	}
}

func TestAdd_TargetExistsRejected(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	// 预创建目标目录。
	dest := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(ctx, repo, dest, "ocdeck/b", "main"); err == nil {
		t.Fatal("Add should reject existing target")
	}
}

// TestAdd_ContainmentBeforeSideEffect 验证 dest 逃逸 worktrees 根时在无任何副作用前报错（ai-worktree-naming 2.5）。
func TestAdd_ContainmentBeforeSideEffect(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	escaped := filepath.Join(t.TempDir(), "evil", "task1")
	if err := mgr.Add(ctx, repo, escaped, "ocdeck/b", "main"); err == nil {
		t.Fatal("Add should reject dest escaping worktrees root")
	}
	// 逃逸目录不应被创建。
	if _, err := os.Stat(escaped); err == nil {
		t.Errorf("escaped dest should not be created: %s", escaped)
	}
}

func TestRemove_CleanClosedLoop(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	wt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, wt, "ocdeck/task-1", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/task-1"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Errorf("worktree dir should be removed")
	}
	// 分支应已删除。
	if _, _, err := execGit(repo, "rev-parse", "--verify", "refs/heads/ocdeck/task-1"); err == nil {
		t.Errorf("branch should be deleted")
	}
}

func execGit(dir string, args ...string) (string, string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), string(ee.Stderr), err
		}
		return string(out), "", err
	}
	return string(out), "", nil
}

func TestRemove_DirtyRejectedWithoutForce(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	wt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, wt, "ocdeck/task-1", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	writeFile(t, wt, "dirty.txt", "x\n")
	err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/task-1"})
	if err == nil {
		t.Fatal("Remove should reject dirty worktree without ForceDirty")
	}
	// ForceDirty 应成功。
	if err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/task-1", ForceDirty: true}); err != nil {
		t.Fatalf("Remove with ForceDirty: %v", err)
	}
}

func TestRemove_PathEscapeRejected(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	// 传一个 worktrees root 之外的路径。
	escaped := filepath.Join(t.TempDir(), "evil")
	if err := os.MkdirAll(escaped, 0o755); err != nil {
		t.Fatal(err)
	}
	err := mgr.Remove(ctx, escaped, RemoveOpts{RepoPath: repo, Branch: "ocdeck/x"})
	if err == nil {
		t.Fatal("Remove should reject path outside worktrees root")
	}
}

func TestRemove_BranchOccupiedRejected(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	// 第一个 worktree 检出 ocdeck/shared。
	wt1 := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, wt1, "ocdeck/shared", "main"); err != nil {
		t.Fatalf("Add wt1: %v", err)
	}
	// 直接尝试 Remove 一个不存在的 worktree 路径但分支被 wt1 占用：
	// 构造一个在 worktrees root 下但实际未创建的路径。
	fakeWt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t-fake")
	if err := mgr.Remove(ctx, fakeWt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/shared"}); err == nil {
		// 分支被 wt1 占用，应拒绝。但 fakeWt 不存在导致 dirty 检查跳过；
		// BranchCheckedOut 仍应返回 true 并拒绝。
		t.Fatal("Remove should reject branch occupied by another worktree")
	}
	// 清理 wt1。
	if err := mgr.Remove(ctx, wt1, RemoveOpts{RepoPath: repo, Branch: "ocdeck/shared", ForceDirty: false}); err != nil {
		t.Fatalf("cleanup wt1: %v", err)
	}
}

func TestRemove_NonexistentWorktreeIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	wt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t-gone")
	// 不存在的 worktree + 不存在的分支：均视为已成功（B8 幂等），Remove 应返回 nil。
	err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/nope"})
	if err != nil {
		t.Fatalf("Remove should be idempotent for nonexistent worktree+branch, got: %v", err)
	}
}

// TestAdd_FailureFullCompensation 验证 Add 失败后全量回收 worktree 目录、分支、metadata。
func TestAdd_FailureFullCompensation(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	dest := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")

	// 用一个会让 worktree add 失败的 baseRef（不存在），但 check-ref-format 通过的分支。
	if err := mgr.Add(ctx, repo, dest, "ocdeck/goodname", "refs/heads/nonexistent-base"); err == nil {
		t.Fatal("Add with nonexistent baseRef should fail")
	}
	// 目录应被清理。
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("half-baked dir should be cleaned: %s", dest)
	}
	// 分支不应残留（rev-parse --verify 失败）。
	if _, _, err := execGit(repo, "rev-parse", "--verify", "refs/heads/ocdeck/goodname"); err == nil {
		t.Errorf("branch ocdeck/goodname should be cleaned after failed Add")
	}
	// worktree metadata 不应残留该 worktree。
	out, _, _ := execGit(repo, "worktree", "list", "--porcelain")
	if strings.Contains(out, dest) {
		t.Errorf("worktree metadata should be pruned after failed Add: %s", out)
	}
}

// TestRemove_RetryAfterPartialFailure 验证 Remove 各步幂等：worktree 已通过 git 移除
// （元数据已清），但分支仍残留，Retry 应删除分支并成功。
func TestRemove_RetryAfterPartialFailure(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	wt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, wt, "ocdeck/retry-1", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 模拟删除中途 worktree 已被 git 正确移除但分支未删（元数据已清，分支残留）。
	if err := gitWorktreeRemove(t, repo, wt); err != nil {
		t.Fatalf("git worktree remove: %v", err)
	}
	// 此时分支仍在。
	if _, _, err := execGit(repo, "rev-parse", "--verify", "refs/heads/ocdeck/retry-1"); err != nil {
		t.Fatalf("branch should still exist: %v", err)
	}
	// Remove 应成功：worktree 不存在（幂等），分支删除成功。
	if err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/retry-1"}); err != nil {
		t.Fatalf("Remove retry should succeed (idempotent worktree + delete branch): %v", err)
	}
	// 分支应已删除。
	if _, _, err := execGit(repo, "rev-parse", "--verify", "refs/heads/ocdeck/retry-1"); err == nil {
		t.Errorf("branch should be deleted")
	}
}

// gitWorktreeRemove 用 git worktree remove 移除 worktree（不经 Manager，模拟外部/部分清理）。
func gitWorktreeRemove(t *testing.T, repo, wt string) error {
	t.Helper()
	cmd := exec.Command("git", "worktree", "remove", wt)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", out)
	}
	return nil
}

// TestRemove_BranchAlreadyGoneIdempotent 验证 branch 已不存在时 Remove 仍成功。
func TestRemove_BranchAlreadyGoneIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	mgr := New(t.TempDir())
	ctx := context.Background()
	wt := filepath.Join(mgr.dataDir, "worktrees", "p1", "t1")
	if err := mgr.Add(ctx, repo, wt, "ocdeck/gone", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 先删除分支（worktree 仍存在但分支关联解除），再用不存在的分支 Remove。
	// 注：worktree 仍检出该分支，手动 branch -D 会失败；改为先 remove worktree 再 retry branch。
	if err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/gone"}); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	// 二次 Remove：worktree 与分支均已不存在，应幂等成功。
	if err := mgr.Remove(ctx, wt, RemoveOpts{RepoPath: repo, Branch: "ocdeck/gone"}); err != nil {
		t.Fatalf("second Remove should be idempotent (already gone): %v", err)
	}
}

// TestFetchVsWorktreeAdd_CriticalSectionNonOverlap 验证 git.RefreshBranches（fetch + ListBranches）
// 与 worktree.Add 使用同一 canonical repo 写锁，临界区不重叠（add-plain-dir-project D10）。
// 用 git.repoLockAcquiredHook 观察锁获取/释放，断言并发期间同时持锁数永不超过 1。
func TestFetchVsWorktreeAdd_CriticalSectionNonOverlap(t *testing.T) {
	repo := newTestRepo(t)
	// 建一个 bare remote 供 RefreshBranches fetch。
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "-q")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "origin", "main")

	// 安装锁观察 hook：统计并发持锁数（应永不超过 1）。
	var active int32
	var maxOverlap int32
	origHook := git.RepoLockAcquiredHookForTest
	git.RepoLockAcquiredHookForTest = func(canonPath string, acquired bool) {
		if acquired {
			cur := atomic.AddInt32(&active, 1)
			for {
				m := atomic.LoadInt32(&maxOverlap)
				if cur <= m || atomic.CompareAndSwapInt32(&maxOverlap, m, cur) {
					break
				}
			}
		} else {
			atomic.AddInt32(&active, -1)
		}
	}
	defer func() { git.RepoLockAcquiredHookForTest = origHook }()

	mgr := New(t.TempDir())
	ctx := context.Background()
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			if idx%2 == 0 {
				// 并发 RefreshBranches（fetch+枚举，持锁）。
				_, errs[idx] = git.RefreshBranches(ctx, repo)
			} else {
				// 并发 worktree.Add（持锁）。
				wt := filepath.Join(mgr.dataDir, "worktrees", "p1", fmt.Sprintf("t%d", idx))
				errs[idx] = mgr.Add(ctx, repo, wt, fmt.Sprintf("ocdeck/t%d", idx), "main")
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if maxOverlap > 1 {
		t.Errorf("max concurrent repo-lock holders = %d, want <= 1 (critical section MUST not overlap)", maxOverlap)
	}
	// 错误检查（Add 可能因分支冲突失败，但 RefreshBranches 应成功；核心是锁串行不重叠）。
	for i, e := range errs {
		if e != nil && i%2 == 0 { // RefreshBranches 不应失败
			t.Errorf("RefreshBranches %d: %v", i, e)
		}
	}
}