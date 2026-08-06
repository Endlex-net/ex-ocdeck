package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/config"
)

// newManagerWithDataDir 构造仅含 dataDir 的 Manager（路径计算只依赖 cfg.DataDir）。
func newManagerWithDataDir(t *testing.T, dataDir string) *Manager {
	t.Helper()
	return &Manager{cfg: &config.Config{DataDir: dataDir}}
}

// --- projectDirSlug / branchDirSlug 单元测试 ---

func TestProjectDirSlug_Normal(t *testing.T) {
	proj := ProjectRow{ID: "abcdef1234567890", Name: "My Cool Project"}
	if got := projectDirSlug(proj); got != "my-cool-project" {
		t.Errorf("projectDirSlug = %q, want my-cool-project", got)
	}
}

func TestProjectDirSlug_EmptyNameFallsBackToProjectIDPrefix(t *testing.T) {
	proj := ProjectRow{ID: "abcdef1234567890abcdef1234567890", Name: "中文项目"}
	got := projectDirSlug(proj)
	want := "project-abcdef12"
	if got != want {
		t.Errorf("projectDirSlug(纯中文) = %q, want %q", got, want)
	}
}

func TestProjectDirSlug_TruncatesAndStripsTrailingDash(t *testing.T) {
	long := strings.Repeat("a", 60) // 60 个 a，截断至 50 无尾部 -
	proj := ProjectRow{ID: "id", Name: long}
	got := projectDirSlug(proj)
	if len(got) != 50 {
		t.Errorf("projectDirSlug len = %d, want 50", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("projectDirSlug should not end with dash: %q", got)
	}
	// 全 a 输入截断后仍全是 a，无尾部 -
	if got != strings.Repeat("a", 50) {
		t.Errorf("projectDirSlug = %q, want 50 a's", got)
	}
}

func TestProjectDirSlug_TruncatesDashedNameStripsTrailingDash(t *testing.T) {
	// 构造一个截断点正好落在 dash 边界的名字：49 个 a + "-" + "b..."
	name := strings.Repeat("a", 49) + "-bbbb"
	proj := ProjectRow{ID: "id", Name: name}
	got := projectDirSlug(proj)
	// 截断至 50：49 a + "-"；去尾部 - → 49 a
	want := strings.Repeat("a", 49)
	if got != want {
		t.Errorf("projectDirSlug = %q, want %q (strip trailing dash after truncation)", got, want)
	}
}

func TestBranchDirSlug_StripsOcdeckPrefix(t *testing.T) {
	if got := branchDirSlug("ocdeck/my-feature"); got != "my-feature" {
		t.Errorf("branchDirSlug = %q, want my-feature", got)
	}
}

func TestBranchDirSlug_NoOcdeckPrefix(t *testing.T) {
	if got := branchDirSlug("feature/x"); got != "feature-x" {
		t.Errorf("branchDirSlug = %q, want feature-x", got)
	}
}

func TestBranchDirSlug_TruncatesAndStripsTrailingDash(t *testing.T) {
	long := "ocdeck/" + strings.Repeat("a", 60)
	got := branchDirSlug(long)
	if len(got) != 50 {
		t.Errorf("branchDirSlug len = %d, want 50", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("branchDirSlug should not end with dash: %q", got)
	}
}

func TestBranchDirSlug_EmptyAfterNormalizeFallsBackToTask(t *testing.T) {
	// 纯中文分支名（去 ocdeck/ 前缀后 normalize 为空）→ 兜底 task
	if got := branchDirSlug("ocdeck/中文分支"); got != "task" {
		t.Errorf("branchDirSlug = %q, want task", got)
	}
}

// --- newWorktreePath 路径格式生成 ---

func TestNewWorktreePath_Format(t *testing.T) {
	m := newManagerWithDataDir(t, t.TempDir())
	proj := ProjectRow{ID: "abcdef1234567890", Name: "My Project", DefaultBranch: "main"}
	branch := "ocdeck/my-feature"
	dest, err := m.newWorktreePath(proj, branch)
	if err != nil {
		t.Fatalf("newWorktreePath: %v", err)
	}
	wantDir := filepath.Join(m.cfg.DataDir, "worktrees", "my-project")
	// dest = <wantDir>/my-feature-<rand4>
	dir, base := filepath.Split(dest)
	if filepath.Clean(dir) != wantDir {
		t.Errorf("dest dir = %q, want %q", dir, wantDir)
	}
	// base 形如 my-feature-xxxx（4 位随机后缀）
	prefix := "my-feature-"
	if !strings.HasPrefix(base, prefix) {
		t.Fatalf("base = %q, want prefix %q", base, prefix)
	}
	suffix := base[len(prefix):]
	if len(suffix) != 4 {
		t.Errorf("rand4 suffix len = %d, want 4 (%q)", len(suffix), suffix)
	}
	for _, c := range suffix {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			t.Errorf("rand4 suffix %q contains invalid char %q", suffix, c)
		}
	}
}

func TestNewWorktreePath_PureChineseProjectNameFallsBackToProjectIDPrefix(t *testing.T) {
	m := newManagerWithDataDir(t, t.TempDir())
	proj := ProjectRow{ID: "abcdef1234567890abcdef1234567890", Name: "纯中文项目"}
	branch := "ocdeck/feat"
	dest, err := m.newWorktreePath(proj, branch)
	if err != nil {
		t.Fatalf("newWorktreePath: %v", err)
	}
	wantDir := filepath.Join(m.cfg.DataDir, "worktrees", "project-abcdef12")
	dir, base := filepath.Split(dest)
	if filepath.Clean(dir) != wantDir {
		t.Errorf("dest dir = %q, want %q", dir, wantDir)
	}
	if !strings.HasPrefix(base, "feat-") {
		t.Errorf("base = %q, want feat-<rand4>", base)
	}
}

func TestNewWorktreePath_LongBranchNameTruncatedBranchUnchanged(t *testing.T) {
	m := newManagerWithDataDir(t, t.TempDir())
	proj := ProjectRow{ID: "id1234567890", Name: "proj"}
	longSeg := strings.Repeat("a", 80)
	branch := "ocdeck/" + longSeg
	dest, err := m.newWorktreePath(proj, branch)
	if err != nil {
		t.Fatalf("newWorktreePath: %v", err)
	}
	// 分支名本身不变（newWorktreePath 不修改分支）。
	if branch != "ocdeck/"+longSeg {
		t.Errorf("branch mutated: %q", branch)
	}
	// 目录段截断 ≤50。
	_, base := filepath.Split(dest)
	prefix := strings.TrimSuffix(base, filepath.Ext(base)) // no ext, just for clarity
	_ = prefix
	// base = <seg>-<rand4>，seg ≤50，总长度 ≤ 55
	segPart := base[:len(base)-5] // strip "-xxxx"
	if len(segPart) > 50 {
		t.Errorf("branch dir segment len = %d, want ≤50 (base=%q)", len(segPart), base)
	}
	if strings.HasSuffix(segPart, "-") {
		t.Errorf("branch dir segment should not end with dash: %q", segPart)
	}
}

// --- 碰撞重试与 3 次耗尽（rand4Fn 注入确定性构造） ---

// newManagerWithRand 构造仅含 dataDir 与可注入 rand4Fn 的 Manager。
func newManagerWithRand(t *testing.T, dataDir string, rand4Fn func() (string, error)) *Manager {
	t.Helper()
	m := newManagerWithDataDir(t, dataDir)
	m.rand4Fn = rand4Fn
	return m
}

// seqRand4 按顺序返回预设后缀；耗尽后 panic（测试用，保证调用次数受控）。
type seqRand4 struct {
	mu   sync.Mutex
	seq  []string
	idx  int
}

func (s *seqRand4) Next() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.seq) {
		panic("seqRand4 exhausted")
	}
	v := s.seq[s.idx]
	s.idx++
	return v, nil
}

func TestNewWorktreePath_CollisionRetryPicksNewSuffix(t *testing.T) {
	tmp := t.TempDir()
	proj := ProjectRow{ID: "abcdef1234567890", Name: "proj"}
	branch := "ocdeck/feat"
	// 预建第一个后缀对应目录，使首次碰撞；第二次返回新后缀，应成功。
	first := "aaaa"
	second := "bbbb"
	// 预建碰撞目录。
	projDir := filepath.Join(tmp, "worktrees", "proj")
	if err := os.MkdirAll(filepath.Join(projDir, "feat-"+first), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newManagerWithRand(t, tmp, (&seqRand4{seq: []string{first, second}}).Next)

	dest, err := m.newWorktreePath(proj, branch)
	if err != nil {
		t.Fatalf("newWorktreePath: %v", err)
	}
	want := filepath.Join(projDir, "feat-"+second)
	if dest != want {
		t.Errorf("dest = %q, want %q (retry should pick non-colliding suffix)", dest, want)
	}
}

func TestNewWorktreePath_CollisionExhaustedAfterThreeAttempts(t *testing.T) {
	tmp := t.TempDir()
	proj := ProjectRow{ID: "abcdef1234567890", Name: "proj"}
	branch := "ocdeck/feat"
	// 预建所有 3 个后缀对应目录，使 3 次均碰撞。
	projDir := filepath.Join(tmp, "worktrees", "proj")
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		if err := os.MkdirAll(filepath.Join(projDir, "feat-"+s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := newManagerWithRand(t, tmp, (&seqRand4{seq: []string{"aaaa", "bbbb", "cccc"}}).Next)

	_, err := m.newWorktreePath(proj, branch)
	if err == nil {
		t.Fatal("expected error after 3 collisions")
	}
	if !strings.Contains(err.Error(), "collision after 3 attempts") {
		t.Errorf("err = %v, want contains 'collision after 3 attempts'", err)
	}
	// 零副作用：无新目录被创建。
	if entries, derr := os.ReadDir(projDir); derr == nil && len(entries) != 3 {
		t.Errorf("expected 3 prebuilt dirs (no side effect), got %d", len(entries))
	}
}

func TestNewWorktreePath_RandFailureNoSideEffect(t *testing.T) {
	tmp := t.TempDir()
	proj := ProjectRow{ID: "abcdef1234567890", Name: "proj"}
	branch := "ocdeck/feat"
	randErr := errors.New("entropy source broken")
	m := newManagerWithRand(t, tmp, func() (string, error) { return "", randErr })

	_, err := m.newWorktreePath(proj, branch)
	if err == nil {
		t.Fatal("expected error on rand failure")
	}
	if !errors.Is(err, randErr) {
		t.Errorf("err = %v, want wrap %v", err, randErr)
	}
	// 零副作用：worktrees 目录不应被创建。
	if _, derr := os.Stat(filepath.Join(tmp, "worktrees")); !os.IsNotExist(derr) {
		t.Errorf("expected no worktrees dir created, got stat err = %v", derr)
	}
}

// TestNewWorktreePath_StatErrorNonNotExist 验证 os.Stat 返回非 IsNotExist 错误时直接返回错误且零副作用。
// 注入 rand4Fn 确定性返回固定后缀，断言 stat 错误分支被命中。
func TestNewWorktreePath_StatErrorNonNotExist(t *testing.T) {
	// 构造一个无法 stat 的路径：将 worktrees 根设为一个无权限目录的子项。
	// 在 macOS 上权限模拟不稳定，改为用文件作为父目录制造 ENOTDIR。
	tmp := t.TempDir()
	// 创建一个普通文件，作为 worktrees 根的父级，使 worktrees/<proj> stat 时遇到 ENOTDIR。
	// 实际：dataDir 指向一个文件，则 <dataDir>/worktrees 解析失败。
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var randCalls int
	m := newManagerWithRand(t, blocker, func() (string, error) {
		randCalls++
		return "aaaa", nil
	})
	proj := ProjectRow{ID: "abcdef1234567890", Name: "proj"}
	branch := "ocdeck/feat"
	_, err := m.newWorktreePath(proj, branch)
	if err == nil {
		t.Fatal("newWorktreePath should fail when os.Stat returns non-IsNotExist error")
	}
	if !strings.Contains(err.Error(), "stat worktree dest") {
		t.Errorf("err = %v, want contains 'stat worktree dest'", err)
	}
	if randCalls != 1 {
		t.Errorf("rand4Fn called %d times, want 1 (stat error short-circuits retry)", randCalls)
	}
	// 零副作用：不应创建任何目录。
	if _, statErr := os.Stat(filepath.Join(blocker, "worktrees")); statErr == nil {
		t.Errorf("no dirs should be created when stat fails: %s", blocker)
	}
}

// --- 现有 DB 路径回归：RetryCreate 不经新格式重算 ---

func TestRetryCreate_UsesDBWorktreePathNotRecomputed(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	// 既有任务的 DB worktree_path 为旧格式 /data/worktrees/p1/t1。
	t1 := TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/p1/t1", BaseRef: "refs/heads/main"}
	store.tasks["t1"] = t1
	wt := newMockWorktree()
	// products 不含该路径 → VerifyWorktreeProduct 失败 → 重新 add。
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	// 验证 Add 被调用且 dest 即 DB 中的旧格式路径（未重算为新格式）。
	if !wt.addedPaths["/data/worktrees/p1/t1"] {
		t.Errorf("Retry should Add at DB worktree_path /data/worktrees/p1/t1, got addedPaths=%v", wt.addedPaths)
	}
	// 确认未出现新格式路径。
	for p := range wt.addedPaths {
		if strings.Contains(p, "proj/") || strings.Contains(p, "task-") && !strings.HasSuffix(p, "/t1") {
			t.Errorf("Retry should not recompute new-format path, got %q", p)
		}
	}
}

// --- worktree.Add containment 前置（dest 逃逸在无副作用前报错）---
//
// 真实 worktree.Manager.Add 的 containment 已在 worktree 包 TestAdd_ContainmentBeforeSideEffect
// 覆盖。task 层调用方传入合法 dest 即可，containment 校验责任在 worktree.Manager。