package task

// add-plain-dir-project Lane 4 测试（任务创建分叉 + repo base_ref，tasks.md 4.3）。
// 独立测试文件，不修改 mock_test.go 共享 helper（并行 lane 在动同包测试）。
//
// 覆盖：
//   - dir 创建：零文件副作用（预埋文件树前后逐字节一致）、branch 空、worktree_path=项目路径、
//     init script 仍触发 InitRunner、目录消失拒绝创建/重试、panic mock 证明 dir 路径不调用
//     Namer/WorktreeBackend。
//   - repo base_ref：本地/远端生效、同名 heads 优先、check-ref-format 非法/不存在 invalid_input
//     零副作用、缺省创建落库全限定 ref、dir 提供 base_ref 拒绝、Retry 用落库 baseRef
//     （默认分支变更不影响；空值 fail-closed）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
)

// panicNamer 实现 BranchNamer，被调用即 panic——证明 dir 路径不调用 Namer。
type panicNamer struct{}

func (n *panicNamer) Slug(ctx context.Context, taskName string) string {
	panic("dir create MUST NOT call Namer")
}

// dirPanicWorktree 包装 mockWorktree，dir 创建 MUST NOT 调用的方法被调用时 panic
// （Add/BranchExists/ValidateBranchName/ResolveBaseRef/VerifyWorktreeProduct/PreflightDelete/DirtyFiles/Remove）。
type dirPanicWorktree struct {
	WorktreeBackend
	inner *mockWorktree
}

func wrapDirPanicWorktree(inner *mockWorktree) *dirPanicWorktree {
	return &dirPanicWorktree{WorktreeBackend: inner, inner: inner}
}

func (w *dirPanicWorktree) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	panic("dir create MUST NOT call WorktreeBackend.Add")
}
func (w *dirPanicWorktree) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	panic("dir create MUST NOT call WorktreeBackend.BranchExists")
}
func (w *dirPanicWorktree) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	panic("dir create MUST NOT call WorktreeBackend.ValidateBranchName")
}
func (w *dirPanicWorktree) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	panic("dir create MUST NOT call WorktreeBackend.ResolveBaseRef")
}
func (w *dirPanicWorktree) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	panic("dir create MUST NOT call WorktreeBackend.VerifyWorktreeProduct")
}
func (w *dirPanicWorktree) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	panic("dir create MUST NOT call WorktreeBackend.PreflightDelete")
}
func (w *dirPanicWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	panic("dir create MUST NOT call WorktreeBackend.DirtyFiles")
}
func (w *dirPanicWorktree) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	panic("dir create MUST NOT call WorktreeBackend.Remove")
}

// newDirTestManager 构造 dir 创建测试专用 Manager：注入 panicNamer + dirPanicWorktree，
// 证明 dir 路径零调用 Namer/WorktreeBackend。
func newDirTestManager(t *testing.T, store TaskStore, proc ProcessBackend, oc OCClient) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	return New(Options{
		Cfg: cfg, Store: store, Proc: proc,
		Worktree:  wrapDirPanicWorktree(newMockWorktree()),
		OCFactory: wrap, Namer: &panicNamer{},
	})
}

// snapshotFileTree 生成目录文件的相对路径集合快照（仅路径，用于前后比对零副作用）。
func snapshotFileTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			out[rel+"/"] = true
			return nil
		}
		out[rel] = true
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot walk %s: %v", root, err)
	}
	return out
}

// TestCreateDir_ZeroFileSideEffects_TreeIdentical 验证 dir 创建前后项目目录文件树逐项一致
// （无 worktree add、无 inherit 复制）。
func TestCreateDir_ZeroFileSideEffects_TreeIdentical(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	// 预埋文件树。
	for _, p := range []string{"README.md", "src/main.go", "src/util.go"} {
		full := filepath.Join(projDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content-"+p+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotFileTree(t, projDir)

	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	row, err := m.Create(context.Background(), "pdir", "my task", "")
	if err != nil {
		t.Fatalf("Create dir: %v", err)
	}
	if row.Branch != "" {
		t.Errorf("Branch = %q, want '' (dir 无分支)", row.Branch)
	}
	canonical, _ := filepath.EvalSymlinks(projDir)
	if row.WorktreePath != canonical {
		t.Errorf("WorktreePath = %q, want canonical project path %q", row.WorktreePath, canonical)
	}
	if row.BaseRef != "" {
		t.Errorf("BaseRef = %q, want '' (dir 无 base_ref)", row.BaseRef)
	}

	after := snapshotFileTree(t, projDir)
	if len(before) != len(after) {
		t.Fatalf("file tree changed: before=%d entries after=%d entries", len(before), len(after))
	}
	for k := range before {
		if !after[k] {
			t.Errorf("file tree changed: %q disappeared after create", k)
		}
	}
}

// TestCreateDir_InitScript_TriggersInitRunner 验证 dir 项目配置 init 脚本时仍触发 InitRunner。
func TestCreateDir_InitScript_TriggersInitRunner(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	seedLifecycleConfig(store, "pdir", "", "echo init", "")

	// 注入 LifecycleRunner mock 以断言 InitRunner 被启动并执行脚本。
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, newMockProc(), wrapDirPanicWorktree(newMockWorktree()), newMockOC(true), runner)
	row, err := m.Create(context.Background(), "pdir", "init task", "")
	if err != nil {
		t.Fatalf("Create dir with init: %v", err)
	}
	// init 脚本 → CommitCreated 落 pending → startInitRunner。等待脚本被调用。
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	waitInitStatus(t, store, row.ID, InitStatusSucceeded, 2*time.Second)
}

// TestCreateDir_PathDoesNotExist_InvalidState_NoCreatingRow 验证 dir 路径不存在 → invalid_state 且不落 creating 行。
func TestCreateDir_PathDoesNotExist_InvalidState_NoCreatingRow(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: "/nonexistent-xyz-abc-123", DefaultBranch: "", Kind: ProjectKindDir})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "pdir", "task", "")
	if err == nil {
		t.Fatal("Create dir with nonexistent path: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidState {
		t.Errorf("error code = %q, want invalid_state", opErr)
	}
	// MUST NOT 落 creating 行。
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks after failed create, want 0 (no creating row)", len(store.tasks))
	}
}

// TestCreateDir_PathIsFile_InvalidState 验证 dir 路径指向文件（非目录）→ invalid_state。
func TestCreateDir_PathIsFile_InvalidState(t *testing.T) {
	resetLifecycleCfgMock()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: filePath, DefaultBranch: "", Kind: ProjectKindDir})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "pdir", "task", "")
	if err == nil {
		t.Fatal("Create dir with file path: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidState {
		t.Errorf("error code = %q, want invalid_state", opErr)
	}
}

// TestCreateDir_ProvidingBaseRef_InvalidInput 验证 dir 项目提供 base_ref → invalid_input 零副作用。
func TestCreateDir_ProvidingBaseRef_InvalidInput(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "pdir", "task", "feature-x")
	if err == nil {
		t.Fatal("Create dir with base_ref: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidInput {
		t.Errorf("error code = %q, want invalid_input", opErr)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}

// TestCreateDir_PanicMocksProveNoNamerOrWorktreeBackend 验证 dir 路径不调用 Namer/WorktreeBackend
// （newDirTestManager 注入 panicNamer + dirPanicWorktree，若被调用测试会 panic 失败）。
func TestCreateDir_PanicMocksProveNoNamerOrWorktreeBackend(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	// 成功即证明 Namer/WorktreeBackend 未被调用（panic mock 否则 fail）。
	if _, err := m.Create(context.Background(), "pdir", "task", ""); err != nil {
		t.Fatalf("Create dir: %v", err)
	}
}

// TestRetryCreateDir_DirGone_KeepsCreationFailed 验证 dir 重试时项目目录消失 → 保持 creation_failed。
func TestRetryCreateDir_DirGone_KeepsCreationFailed(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	t1 := TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusCreationFailed, WorktreePath: projDir, BaseRef: ""}
	store.tasks["t1"] = t1
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	// 删除项目目录模拟"目录消失"。
	if err := os.RemoveAll(projDir); err != nil {
		t.Fatal(err)
	}
	if err := m.Retry(context.Background(), "t1", false); err == nil {
		t.Fatal("Retry dir with gone path: want error, got nil")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed (kept)", row.Status)
	}
}

// TestRetryCreateDir_Success 验证 dir 重试成功（目录仍在 → 读配置 → 提交 suspended）。
func TestRetryCreateDir_Success(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
	t1 := TaskRow{ID: "t1", ProjectID: "pdir", Name: "task", Branch: "",
		Status: StatusCreationFailed, WorktreePath: projDir, BaseRef: ""}
	store.tasks["t1"] = t1
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry dir: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Errorf("status = %s, want suspended|activating|active (committed + auto-activate)", row.Status)
	}
}

// --- repo base_ref 测试 ---

// baseRefWorktree 包装 mockWorktree，ResolveBaseRef 按预设映射返回，模拟本地/远端分支解析。
type baseRefWorktree struct {
	*mockWorktree
	resolveFn func(ctx context.Context, repoPath, shortName string) (string, error)
}

func (w *baseRefWorktree) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	return w.resolveFn(ctx, repoPath, shortName)
}

// newRepoTestManager 构造 repo 创建测试 Manager（注入 baseRefWorktree，可选 Namer）。
func newRepoTestManager(t *testing.T, store TaskStore, wt WorktreeBackend) *Manager {
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
		OCFactory: wrap,
	})
}

// TestCreateRepo_DefaultBaseRef_LandsFullQualifiedRef 验证 repo 缺省 base_ref 落库 refs/heads/<默认分支>。
func TestCreateRepo_DefaultBaseRef_LandsFullQualifiedRef(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := &baseRefWorktree{mockWorktree: newMockWorktree()}
	m := newRepoTestManager(t, store, wt)

	row, err := m.Create(context.Background(), "prep", "my task", "")
	if err != nil {
		t.Fatalf("Create repo default: %v", err)
	}
	if row.BaseRef != "refs/heads/main" {
		t.Errorf("BaseRef = %q, want 'refs/heads/main' (default full-qualified)", row.BaseRef)
	}
	// 验证 wt.Add 收到 baseRef=refs/heads/main（非 proj.DefaultBranch 裸名）。
	if !wt.addedPaths[row.WorktreePath] {
		t.Errorf("wt.Add not called for %s", row.WorktreePath)
	}
}

// TestCreateRepo_BaseRefLocal_ResolvedToHeads 验证 repo base_ref 本地分支解析为 refs/heads/<name>。
func TestCreateRepo_BaseRefLocal_ResolvedToHeads(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := &baseRefWorktree{
		mockWorktree: newMockWorktree(),
		resolveFn: func(ctx context.Context, repoPath, shortName string) (string, error) {
			return "refs/heads/" + shortName, nil
		},
	}
	m := newRepoTestManager(t, store, wt)

	row, err := m.Create(context.Background(), "prep", "task", "feature-x")
	if err != nil {
		t.Fatalf("Create repo base_ref local: %v", err)
	}
	if row.BaseRef != "refs/heads/feature-x" {
		t.Errorf("BaseRef = %q, want 'refs/heads/feature-x'", row.BaseRef)
	}
}

// TestCreateRepo_BaseRefRemote_ResolvedToRemotes 验证 repo base_ref 远端分支解析为 refs/remotes/<name>。
func TestCreateRepo_BaseRefRemote_ResolvedToRemotes(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := &baseRefWorktree{
		mockWorktree: newMockWorktree(),
		resolveFn: func(ctx context.Context, repoPath, shortName string) (string, error) {
			return "refs/remotes/" + shortName, nil
		},
	}
	m := newRepoTestManager(t, store, wt)

	row, err := m.Create(context.Background(), "prep", "task", "origin/feature-x")
	if err != nil {
		t.Fatalf("Create repo base_ref remote: %v", err)
	}
	if row.BaseRef != "refs/remotes/origin/feature-x" {
		t.Errorf("BaseRef = %q, want 'refs/remotes/origin/feature-x'", row.BaseRef)
	}
}

// TestCreateRepo_HeadsPriorityOverRemotes 验证同名时 heads 优先于 remotes。
// 构造 ResolveBaseRef 第一次调用（refs/heads）返回成功，断言落库为 heads 而非 remotes。
func TestCreateRepo_HeadsPriorityOverRemotes(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	calls := 0
	wt := &baseRefWorktree{
		mockWorktree: newMockWorktree(),
		resolveFn: func(ctx context.Context, repoPath, shortName string) (string, error) {
			calls++
			// 模拟 heads 命中：返回 heads 形式（resolveRepoBaseRef 先尝试 heads 再 remotes，
			// heads 命中即返回，不会再调 remotes）。
			return "refs/heads/" + shortName, nil
		},
	}
	m := newRepoTestManager(t, store, wt)

	row, err := m.Create(context.Background(), "prep", "task", "feature-x")
	if err != nil {
		t.Fatalf("Create repo heads priority: %v", err)
	}
	if row.BaseRef != "refs/heads/feature-x" {
		t.Errorf("BaseRef = %q, want 'refs/heads/feature-x' (heads优先)", row.BaseRef)
	}
	// resolveRepoBaseRef 仅调用一次 ResolveBaseRef（heads 命中即返回）。
	if calls != 1 {
		t.Errorf("ResolveBaseRef calls = %d, want 1 (heads命中不再尝试remotes)", calls)
	}
}

// TestCreateRepo_BaseRefNotExists_InvalidInput_ZeroSideEffect 验证 base_ref 不存在 → invalid_input 零副作用。
func TestCreateRepo_BaseRefNotExists_InvalidInput_ZeroSideEffect(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := &baseRefWorktree{
		mockWorktree: newMockWorktree(),
		resolveFn: func(ctx context.Context, repoPath, shortName string) (string, error) {
			return "", fmt.Errorf("git: base_ref %q not found", shortName)
		},
	}
	m := newRepoTestManager(t, store, wt)

	_, err := m.Create(context.Background(), "prep", "task", "nonexistent-branch")
	if err == nil {
		t.Fatal("Create repo with nonexistent base_ref: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidInput {
		t.Errorf("error code = %q, want invalid_input", opErr)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}

// TestCreateRepo_InvalidBaseRef_InvalidInput 验证 check-ref-format 拒绝非法 base_ref → invalid_input。
// 用 mockWorktree 默认 ValidateBranchName=nil（不拒绝）；改用自定义 wt 覆盖 ValidateBranchName 拒绝。
func TestCreateRepo_InvalidBaseRef_InvalidInput(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := &baseRefRejectValidate{inner: newMockWorktree()}
	m := newRepoTestManager(t, store, wt)

	// base_ref 含非法字符 ".." → ValidateBranchName 拒绝（resolveRepoBaseRef 先校验）。
	_, err := m.Create(context.Background(), "prep", "task", "foo..bar")
	if err == nil {
		t.Fatal("Create repo with invalid base_ref: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidInput {
		t.Errorf("error code = %q, want invalid_input", opErr)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}

// baseRefRejectValidate 包装 mockWorktree，ValidateBranchName 对 ".." 拒绝（模拟 check-ref-format）。
type baseRefRejectValidate struct {
	inner *mockWorktree
}

func (w *baseRefRejectValidate) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	return w.inner.Add(ctx, repoPath, dest, branch, baseRef)
}
func (w *baseRefRejectValidate) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	return w.inner.Remove(ctx, wtPath, opts)
}
func (w *baseRefRejectValidate) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	return w.inner.BranchExists(ctx, repoPath, branch)
}
func (w *baseRefRejectValidate) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	// 模拟 git check-ref-format：拒绝含 ".." 的分支名。分支名前缀 ocdeck/ 也会进这里，
	// 但 ocdeck/<slug> 不含 ".."，故仅 base_ref "foo..bar" 被拒。
	if containsDotDot(branch) {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	return nil
}
func (w *baseRefRejectValidate) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	return w.inner.ResolveBaseRef(ctx, repoPath, shortName)
}
func (w *baseRefRejectValidate) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	return w.inner.VerifyWorktreeProduct(ctx, repoPath, wtPath, branch)
}
func (w *baseRefRejectValidate) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	return w.inner.PreflightDelete(ctx, wtPath, opts)
}
func (w *baseRefRejectValidate) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	return w.inner.DirtyFiles(ctx, wtPath)
}

func containsDotDot(s string) bool { return len(s) > 0 && indexDotDot(s) >= 0 }

func indexDotDot(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '.' && s[i+1] == '.' {
			return i
		}
	}
	return -1
}

// capturingWorktree 包装 mockWorktree，记录每次 Add 的 baseRef 参数（按 dest 索引）。
type capturingWorktree struct {
	*mockWorktree
	mu        sync.Mutex
	addBaseBy map[string]string
}

func (w *capturingWorktree) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	w.mu.Lock()
	w.addBaseBy[dest] = baseRef
	w.mu.Unlock()
	return w.mockWorktree.Add(ctx, repoPath, dest, branch, baseRef)
}

// TestRetryCreateRepo_UsesStoredBaseRef_NotDefaultBranch 验证 repo 重试用落库 base_ref，
// 默认分支变更不影响（MUST NOT 重读 proj.DefaultBranch）。
func TestRetryCreateRepo_UsesStoredBaseRef_NotDefaultBranch(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	// 项目默认分支当前是 "develop"（与落库 base_ref 的 main 不同），证明重试不重读默认分支。
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "develop", Kind: ProjectKindRepo})
	t1 := TaskRow{ID: "t1", ProjectID: "prep", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/prep/t1", BaseRef: "refs/heads/main"}
	store.tasks["t1"] = t1
	wt := &capturingWorktree{mockWorktree: newMockWorktree(), addBaseBy: map[string]string{}}
	m := newRepoTestManager(t, store, wt)

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry repo: %v", err)
	}
	// products 不含该路径 → 必调 Add。断言收到的 baseRef 是落库的 refs/heads/main，而非当前默认分支 develop。
	got := wt.addBaseBy["/data/worktrees/prep/t1"]
	if got != "refs/heads/main" {
		t.Errorf("Retry wt.Add baseRef = %q, want 'refs/heads/main' (落库值，不重读默认分支)", got)
	}
}

// TestRetryCreateRepo_EmptyBaseRef_FailClosed 验证 repo 重试时落库 base_ref 空 → fail-closed 报错。
func TestRetryCreateRepo_EmptyBaseRef_FailClosed(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	t1 := TaskRow{ID: "t1", ProjectID: "prep", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/prep/t1", BaseRef: ""}
	store.tasks["t1"] = t1
	wt := newMockWorktree()
	m := newRepoTestManager(t, store, wt)

	err := m.Retry(context.Background(), "t1", false)
	if err == nil {
		t.Fatal("Retry repo with empty base_ref: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInvalidState {
		t.Errorf("error code = %q, want invalid_state (fail-closed)", opErr)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed (kept on fail-closed)", row.Status)
	}
}

// TestCreate_UnknownKind_FailClosed 验证未知持久化 kind 创建零副作用报错。
// D1：未知持久化 kind（DB 损坏值）→ internal（区别于用户请求非法 kind 的 invalid_input）。
func TestCreate_UnknownKind_FailClosed(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p", Name: "p", Path: "/x", DefaultBranch: "main", Kind: "bogus"})
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "p", "task", "")
	if err == nil {
		t.Fatal("Create unknown kind: want error, got nil")
	}
	if opErr := OpErrorCode(err); opErr != codeInternal {
		t.Errorf("error code = %q, want internal (unknown persisted kind, D1)", opErr)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}