package task

import (
	"context"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
)

// mockNamer 实现 BranchNamer，记录调用并返回预设 slug。
type mockNamer struct {
	mu       sync.Mutex
	calls    []string // 记录收到的 taskName
	slug     string   // 返回的 slug
	callCount int
}

func (n *mockNamer) Slug(ctx context.Context, taskName string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, taskName)
	n.callCount++
	return n.slug
}

func (n *mockNamer) callsSnapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.calls))
	copy(out, n.calls)
	return out
}

// newTestManagerWithNamer 构造注入 Namer 的 Manager（不注入 lifecycle ctx，
// 测试不依赖自动激活推进）。用于 Create slug 路径测试。
func newTestManagerWithNamer(t *testing.T, store TaskStore, wt WorktreeBackend, namer BranchNamer) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: newMockOC(true), onReady: opts.OnReady}
	}
	return New(Options{
		Cfg: cfg, Store: store, Proc: newMockProc(), Worktree: wt,
		OCFactory: wrap, Namer: namer,
	})
}

// TestCreate_UsesNamerForBranchSlug 验证 Create 经 Namer 生成分支 slug：
// 分支名用 Namer 返回值拼接 ocdeck/ 前缀，且 Namer 收到原始 taskName。
func TestCreate_UsesNamerForBranchSlug(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	namer := &mockNamer{slug: "ai-refined-slug"}
	m := newTestManagerWithNamer(t, store, wt, namer)

	row, err := m.Create(context.Background(), "p1", "修复登录bug", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantBranch := "ocdeck/ai-refined-slug"
	if row.Branch != wantBranch {
		t.Errorf("branch = %q, want %q", row.Branch, wantBranch)
	}
	calls := namer.callsSnapshot()
	if len(calls) != 1 || calls[0] != "修复登录bug" {
		t.Errorf("namer calls = %v, want [\"修复登录bug\"]", calls)
	}
}

// TestCreate_NamerFallbackValuePassThrough 验证 Namer 返回回退值（如 Slugify 结果）
// 时透传拼前缀，Create 不二次清洗。
func TestCreate_NamerFallbackValuePassThrough(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	// Namer 返回的就是 fallback 结果（模拟 ai.SlugNamer 走 fallback 路径）。
	namer := &mockNamer{slug: "my-task"}
	m := newTestManagerWithNamer(t, store, wt, namer)

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Branch != "ocdeck/my-task" {
		t.Errorf("branch = %q, want ocdeck/my-task", row.Branch)
	}
}

// TestCreate_NilNamerUsesSlugify 验证 Namer=nil 时 Create 回退到本包 Slugify。
func TestCreate_NilNamerUsesSlugify(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	// 不注入 Namer：newTestManager 走 nil 路径。
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Branch != "ocdeck/my-task" {
		t.Errorf("branch = %q, want ocdeck/my-task (Slugify fallback)", row.Branch)
	}
}

// TestCreate_NamerBranchConflictUnchanged 验证注入 Namer 后分支冲突语义不变：
// Namer 返回的 slug 命中既有分支 → conflict（既有 ValidateBranchName/BranchExists 预检顺序不变）。
func TestCreate_NamerBranchConflictUnchanged(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	// 预置分支已存在。
	wt.branches["ocdeck/dup-slug"] = true
	namer := &mockNamer{slug: "dup-slug"}
	m := newTestManagerWithNamer(t, store, wt, namer)

	_, err := m.Create(context.Background(), "p1", "anything", "")
	if err == nil {
		t.Fatal("expected conflict on duplicate branch")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code = %s, want conflict", OpErrorCode(err))
	}
	// 冲突时不应落库 creating 行。
	store.mu.Lock()
	n := len(store.tasks)
	store.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no task row on branch conflict, got %d", n)
	}
}

// TestRetryCreate_NamerNotInvoked 验证 RetryCreate 复用落库 Branch/WorktreePath，
// 不重算 slug（不经 Namer）。
func TestRetryCreate_NamerNotInvoked(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	t1 := TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/old-branch",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/p1/t1", BaseRef: "refs/heads/main"}
	store.tasks["t1"] = t1
	wt := newMockWorktree()
	namer := &mockNamer{slug: "should-not-be-used"}
	m := newTestManagerWithNamer(t, store, wt, namer)

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	// RetryCreate 复用落库 Branch，不调用 Namer。
	if len(namer.callsSnapshot()) != 0 {
		t.Errorf("RetryCreate should not invoke Namer, got calls %v", namer.callsSnapshot())
	}
	// 落库 Branch 保持原值。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Branch != "ocdeck/old-branch" {
		t.Errorf("branch = %q, want ocdeck/old-branch (reused from DB)", row.Branch)
	}
}

// TestCreate_NamerInvalidSlugRejectedByValidateBranchName 验证 Namer 返回非法 slug
// 时既有 ValidateBranchName 前置检查仍生效（语义不变，顺序不变）。
func TestCreate_NamerInvalidSlugRejectedByValidateBranchName(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	// 让 ValidateBranchName 返回错误。
	wtValidateErr := wtValidateErrBackend{mockWorktree: newMockWorktree(), err: errInvalidBranch}
	namer := &mockNamer{slug: "bad slug"}
	m := newTestManagerWithNamer(t, store, &wtValidateErr, namer)

	_, err := m.Create(context.Background(), "p1", "task", "")
	if err == nil {
		t.Fatal("expected invalid_input on invalid branch name")
	}
	if OpErrorCode(err) != codeInvalidInput {
		t.Errorf("code = %s, want invalid_input", OpErrorCode(err))
	}
	if !strings.Contains(err.Error(), "invalid branch name") {
		t.Errorf("err = %v, want contains 'invalid branch name'", err)
	}
}

var errInvalidBranch = strErr("invalid branch name")

type strErr string

func (e strErr) Error() string { return string(e) }

// wtValidateErrBackend 装饰 mockWorktree，让 ValidateBranchName 返回固定错误。
type wtValidateErrBackend struct {
	*mockWorktree
	err error
}

func (w *wtValidateErrBackend) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return w.err
}