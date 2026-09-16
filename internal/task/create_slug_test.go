// create_slug_test.go 覆盖创建路径 branch_slug / 前缀快照接入（task-info-editable 3.3）：
// 显式 slug 跳过 LLM 命名、前缀快照驱动命名与目录段、非法/冲突零副作用、dir/local-path 拒绝、
// 前缀变更仅影响新任务。
package task

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingNamer 记录 Slug 调用（显式 slug 时 MUST NOT 被调用的证据桩）。
type recordingNamer struct{ calls []string }

func (n *recordingNamer) Slug(ctx context.Context, name string) string {
	n.calls = append(n.calls, name)
	return "ai-slug"
}

func TestCreate_BranchSlugExplicit_SkipsNamer(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	namer := &recordingNamer{}
	m.namer = namer

	row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", BranchSlug: strPtr("my-feature")})
	if err != nil {
		t.Fatalf("Create with explicit slug: %v", err)
	}
	if row.Branch != "ocdeck/my-feature" {
		t.Fatalf("branch = %q, want ocdeck/my-feature (slug used verbatim)", row.Branch)
	}
	if len(namer.calls) != 0 {
		t.Fatalf("namer called %v, want no LLM/slugify invocation", namer.calls)
	}
	// 显式 slug 仍走既有 check-ref-format + 冲突检查（此处经 mock 成功路径）。
	if exists, _ := wt.BranchExists(context.Background(), "/repo", "ocdeck/my-feature"); !exists {
		t.Fatalf("branch not registered in mock after create")
	}
}

func TestCreate_BranchSlug_TrimmedPresence(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	// trim 后空 = 未提供 → 走 namer（stub 返回 ai-slug）。
	namer := &recordingNamer{}
	m.namer = namer
	row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", BranchSlug: strPtr("   ")})
	if err != nil {
		t.Fatalf("Create with blank slug: %v", err)
	}
	if row.Branch != "ocdeck/ai-slug" || len(namer.calls) != 1 {
		t.Fatalf("branch = %q namer calls = %d, want LLM naming path", row.Branch, len(namer.calls))
	}
}

func TestCreate_PrefixSnapshot_DrivesBranchAndDirSegment(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	m.branchPrefixFn = func() string { return "team" }

	row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", BranchSlug: strPtr("my-feature")})
	if err != nil {
		t.Fatalf("Create with prefix team: %v", err)
	}
	if row.Branch != "team/my-feature" {
		t.Fatalf("branch = %q, want team/my-feature", row.Branch)
	}
	// 目录段去 <prefix>/ 后派生：worktrees/proj/my-feature-<rand4>。
	if !strings.HasPrefix(row.WorktreePath, m.cfg.DataDir+"/worktrees/proj/my-feature-") {
		t.Fatalf("worktree path = %q, want <dataDir>/worktrees/proj/my-feature-<rand4>", row.WorktreePath)
	}
}

func TestCreate_BranchSlug_Invalid_ZeroSideEffect(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	wt.validateErr = errors.New("check-ref-format rejected")
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	namer := &recordingNamer{}
	m.namer = namer

	_, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", BranchSlug: strPtr("bad..name")})
	if err == nil || OpErrorCode(err) != codeInvalidInput {
		t.Fatalf("err = %v, want invalid_input", err)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("store has %d tasks, want 0 (zero side effect, no creating row)", len(store.tasks))
	}
	if len(namer.calls) != 0 {
		t.Fatalf("namer must not run for explicit slug, calls=%v", namer.calls)
	}
}

func TestCreate_BranchSlug_Conflict_ZeroSideEffect(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	wt.branches["ocdeck/taken"] = true
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", BranchSlug: strPtr("taken")})
	if err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}

func TestCreate_BranchSlug_DirAndLocalPath_Rejected(t *testing.T) {
	t.Run("dir project", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "pd", Name: "d", Path: t.TempDir(), DefaultBranch: "", Kind: ProjectKindDir})
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		_, err := m.Create(context.Background(), "pd", CreateTaskOptions{Name: "task", BranchSlug: strPtr("x")})
		if err == nil || OpErrorCode(err) != codeInvalidInput {
			t.Fatalf("err = %v, want invalid_input", err)
		}
		if len(store.tasks) != 0 {
			t.Fatalf("store has %d tasks, want 0", len(store.tasks))
		}
	})
	t.Run("repo local-path mode", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "r", Path: t.TempDir(), DefaultBranch: "main", Kind: ProjectKindRepo})
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

		_, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath, BranchSlug: strPtr("x")})
		if err == nil || OpErrorCode(err) != codeInvalidInput {
			t.Fatalf("err = %v, want invalid_input", err)
		}
		if len(store.tasks) != 0 {
			t.Fatalf("store has %d tasks, want 0", len(store.tasks))
		}
	})
}

func TestCreate_PrefixChange_OnlyAffectsNewTasks(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	wt := newMockWorktree()
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	prefix := "ocdeck"
	m.branchPrefixFn = func() string { return prefix }

	row1, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task1", BranchSlug: strPtr("s1")})
	if err != nil {
		t.Fatalf("create t1: %v", err)
	}
	prefix = "team" // 全局前缀变更
	row2, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task2", BranchSlug: strPtr("s2")})
	if err != nil {
		t.Fatalf("create t2: %v", err)
	}
	if row1.Branch != "ocdeck/s1" {
		t.Errorf("t1 branch = %q, want ocdeck/s1 (existing task unaffected)", row1.Branch)
	}
	if row2.Branch != "team/s2" {
		t.Errorf("t2 branch = %q, want team/s2 (new task uses new prefix)", row2.Branch)
	}
}
