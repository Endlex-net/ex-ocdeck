package task

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/application"
	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/infrastructure/opencode"
)

// task-permission-mode D9 测试：
//   - resolvePermissionMode 表驱动（三值/空串防御/未知值 fail-closed）；
//   - runtimeCmdArgv 表驱动（all-approve 含 --auto，ask/ai-auto 与旧 argv 逐字一致）；
//   - Create 缺省归一化 ask 落库、显式三值 roundtrip、非法值 invalid_input 零副作用；
//   - 激活 argv 按落库值施加（all-approve → --auto）与未知持久化值 fail-closed 不启动进程。

func TestResolvePermissionMode(t *testing.T) {
	cases := []struct {
		stored string
		want   string
		wantEr bool
	}{
		{stored: "ask", want: "ask"},
		{stored: "all-approve", want: "all-approve"},
		{stored: "ai-auto", want: "ai-auto"},
		// 空串防御（0015 列 NOT NULL DEFAULT 'ask'，存量行不会为空，仍兜底 ask）。
		{stored: "", want: "ask"},
		// 未知持久化值 = 持久化损坏 → fail-closed internal error。
		{stored: "bogus", wantEr: true},
		{stored: "always", wantEr: true},
	}
	for _, tc := range cases {
		got, err := resolvePermissionMode(TaskRow{ID: "t1", PermissionMode: tc.stored})
		if tc.wantEr {
			if err == nil {
				t.Errorf("resolvePermissionMode(%q) err = nil, want error (fail-closed)", tc.stored)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolvePermissionMode(%q) err = %v, want nil", tc.stored, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolvePermissionMode(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// legacyRuntimeArgv 旧实现 argv 基线（task-permission-mode D3：ask/ai-auto MUST 逐字一致）。
func legacyRuntimeArgv(port int, sessionID string) []string {
	argv := []string{"opencode", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}
	if sessionID != "" {
		argv = append(argv, "--session", sessionID)
	}
	return argv
}

func TestRuntimeCmdArgv_PermissionMode(t *testing.T) {
	cases := []struct {
		permissionMode string
		wantAuto       bool
	}{
		{permissionMode: PermissionModeAsk},
		{permissionMode: PermissionModeAIAuto},
		{permissionMode: PermissionModeAllApprove, wantAuto: true},
	}
	for _, tc := range cases {
		argv := runtimeCmdArgv(50505, "sess-1", tc.permissionMode)
		want := legacyRuntimeArgv(50505, "sess-1")
		if tc.wantAuto {
			want = append(want, "--auto")
		}
		if !reflect.DeepEqual(argv, want) {
			t.Errorf("runtimeCmdArgv(permissionMode=%q) = %v, want %v", tc.permissionMode, argv, want)
		}
	}
	// 行为有效性证据：all-approve 的差异仅在 --auto，且位于既有 argv 之后（不破坏锚定语义）。
	argv := runtimeCmdArgv(1, "", PermissionModeAllApprove)
	if len(argv) == 0 || argv[len(argv)-1] != "--auto" {
		t.Errorf("all-approve argv tail = %v, want --auto appended", argv)
	}
}

// TestCreate_PermissionMode_CreateChain 创建链三态（task-permission-mode tasks 1.4）：
// 缺省归一化 ask 落库、显式三值 roundtrip、非法值 invalid_input 零落库/零 worktree 副作用。
func TestCreate_PermissionMode_CreateChain(t *testing.T) {
	newManager := func(t *testing.T) (*Manager, *mockStore, *mockWorktree) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		wt := newMockWorktree()
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
		return m, store, wt
	}

	t.Run("default_normalizes_to_ask", func(t *testing.T) {
		m, store, wt := newManager(t)
		row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if row.PermissionMode != PermissionModeAsk {
			t.Errorf("row permission_mode = %q, want ask (缺省归一化)", row.PermissionMode)
		}
		stored, _ := store.GetTask(context.Background(), row.ID)
		if stored.PermissionMode != PermissionModeAsk {
			t.Errorf("stored permission_mode = %q, want ask (落库值)", stored.PermissionMode)
		}
		if len(wt.addedPaths) == 0 {
			t.Fatal("precondition: worktree add must have happened (非零副作用基线)")
		}
	})

	t.Run("explicit_values_roundtrip", func(t *testing.T) {
		for _, mode := range []string{PermissionModeAsk, PermissionModeAllApprove, PermissionModeAIAuto} {
			m, store, _ := newManager(t)
			row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", PermissionMode: mode})
			if err != nil {
				t.Fatalf("Create(permission_mode=%q): %v", mode, err)
			}
			if row.PermissionMode != mode {
				t.Errorf("row permission_mode = %q, want %q", row.PermissionMode, mode)
			}
			stored, _ := store.GetTask(context.Background(), row.ID)
			if stored.PermissionMode != mode {
				t.Errorf("stored permission_mode = %q, want %q (roundtrip)", stored.PermissionMode, mode)
			}
		}
	})

	t.Run("invalid_value_invalid_input_zero_side_effects", func(t *testing.T) {
		for _, mode := range []string{"bogus", "always", "  "} {
			m, store, wt := newManager(t)
			_, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", PermissionMode: mode})
			if err == nil {
				t.Fatalf("Create(permission_mode=%q) err = nil, want invalid_input", mode)
			}
			if OpErrorCode(err) != codeInvalidInput {
				t.Errorf("Create(permission_mode=%q) code = %s, want invalid_input", mode, OpErrorCode(err))
			}
			if len(store.tasks) != 0 {
				t.Errorf("Create(permission_mode=%q) persisted %d task rows, want 0 (零落库)", mode, len(store.tasks))
			}
			if len(wt.addedPaths) != 0 {
				t.Errorf("Create(permission_mode=%q) worktree add called: %v, want none (零副作用)", mode, wt.addedPaths)
			}
		}
	})
}

// TestActivate_PermissionModeArgvApplied 激活 argv 按落库权限模式施加（task-permission-mode
// tasks 2.1）：all-approve → 追加 --auto（旧行为下缺失该断言即失败——行为有效性证据）；
// ask/ai-auto → 与旧 argv 逐字一致。
func TestActivate_PermissionModeArgvApplied(t *testing.T) {
	cases := []struct {
		permissionMode string
		wantAuto       bool
	}{
		{permissionMode: PermissionModeAsk},
		{permissionMode: PermissionModeAIAuto},
		{permissionMode: PermissionModeAllApprove, wantAuto: true},
	}
	for _, tc := range cases {
		t.Run(tc.permissionMode, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = tc.permissionMode })
			proc := newMockProc()
			oc := newMockOC(true)
			oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
			m := newTestManager(t, store, proc, newMockWorktree(), oc)

			if err := m.Activate(context.Background(), "t1"); err != nil {
				t.Fatalf("Activate: %v", err)
			}
			argv := runtimeCmdArgvOf(proc, "t1")
			// argv[2] 为激活时实际分配的端口（50000-50999 范围内），据其构造期望基线。
			port, perr := strconv.Atoi(argv[2])
			if perr != nil {
				t.Fatalf("argv port = %q: %v", argv[2], perr)
			}
			want := legacyRuntimeArgv(port, "sess-fresh")
			if tc.wantAuto {
				want = append(want, "--auto")
			}
			if !reflect.DeepEqual(argv, want) {
				t.Errorf("activate argv (permission_mode=%q) = %v, want %v", tc.permissionMode, argv, want)
			}
			hasAuto := false
			for _, a := range argv {
				if a == "--auto" {
					hasAuto = true
				}
			}
			if hasAuto != tc.wantAuto {
				t.Errorf("argv contains --auto = %v, want %v", hasAuto, tc.wantAuto)
			}
		})
	}
}

// TestActivate_UnknownPermissionMode_FailClosedNoProcess 未知持久化权限模式 = 持久化损坏：
// 激活 fail-closed 返回 internal error，MUST NOT 启动任何进程（task-permission-mode D3）。
func TestActivate_UnknownPermissionMode_FailClosedNoProcess(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = "bogus" })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate err = nil, want internal error (fail-closed)")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("code = %s, want internal", OpErrorCode(err))
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("err = %v, want diagnostic containing persisted value", err)
	}
	if len(proc.newSessionNames) != 0 {
		t.Errorf("NewSession called for %v, want none (MUST NOT 启动进程)", proc.newSessionNames)
	}
	if proc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime session must not exist (MUST NOT 启动进程)")
	}
}

// --- F2：提交边界 spy（防 mockStore 兜底掩盖 Manager 归一化回归）与双路径透传 ---

// createTaskSpyStore 在 mockStore.CreateTask 兜底（空串→ask）之前捕获 Manager 经 legacy
// 路径实际提交的创建行；其余方法/字段提升至内嵌 mockStore。
type createTaskSpyStore struct {
	*mockStore
	mu        sync.Mutex
	submitted []TaskRow
}

func (s *createTaskSpyStore) CreateTask(ctx context.Context, t TaskRow) error {
	s.mu.Lock()
	s.submitted = append(s.submitted, t)
	s.mu.Unlock()
	return s.mockStore.CreateTask(ctx, t)
}

func (s *createTaskSpyStore) submittedRows() []TaskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TaskRow(nil), s.submitted...)
}

// createSnapshotSpyAdapter 在 mockAppAdapter（taskSnapshotToTaskRow / mockStore 兜底）之前
// 捕获 Manager 经 lifecycle 路径实际提交的创建快照；其余方法提升至内嵌 adapter。
type createSnapshotSpyAdapter struct {
	*mockAppAdapter
	mu        sync.Mutex
	snapshots []application.TaskSnapshot
}

func (a *createSnapshotSpyAdapter) CreateTask(ctx context.Context, row application.TaskSnapshot) error {
	a.mu.Lock()
	a.snapshots = append(a.snapshots, row)
	a.mu.Unlock()
	return a.mockAppAdapter.CreateTask(ctx, row)
}

func (a *createSnapshotSpyAdapter) submittedSnapshots() []application.TaskSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]application.TaskSnapshot(nil), a.snapshots...)
}

// newPermissionLifecycleManager 构造注入 LifecycleService 的 Manager，写路径经 spy adapter
//（与 p144 newTestManagerWithLifecycleService 同构，仅插入捕获层，不重构既有 mock 体系）。
// Sessions 端口同注入：Create 成功后异步 triggerActivate 的 SSE align 会经
// LifecycleService.AlignSessions 到达（缺注入会 nil panic）。
func newPermissionLifecycleManager(t *testing.T, store *mockStore, spy *createSnapshotSpyAdapter) *Manager {
	t.Helper()
	svc := apptask.New(apptask.Options{
		Tasks:    spy,
		Read:     spy.mockAppAdapter,
		Sessions: spy.mockAppAdapter,
		Publish:  apptask.NoopPublisher{},
	})
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.lifecycle = svc
	return m
}

// TestCreate_PermissionMode_SubmissionSpy 缺省创建时 Manager 实际提交 ask（而非空串穿透）：
// spy 位于 mockStore 兜底之前，若 Manager 未归一化，提交值将以 "" 暴露。
func TestCreate_PermissionMode_SubmissionSpy(t *testing.T) {
	t.Run("legacy_path_submits_ask", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		spy := &createTaskSpyStore{mockStore: store}
		m := newTestManager(t, spy, newMockProc(), newMockWorktree(), newMockOC(true))

		if _, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		submitted := spy.submittedRows()
		if len(submitted) != 1 {
			t.Fatalf("CreateTask submitted %d rows, want 1", len(submitted))
		}
		if submitted[0].PermissionMode != PermissionModeAsk {
			t.Errorf("legacy 提交 PermissionMode = %q, want ask（空串穿透即回归）", submitted[0].PermissionMode)
		}
	})
	t.Run("lifecycle_path_submits_ask", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		spy := &createSnapshotSpyAdapter{mockAppAdapter: &mockAppAdapter{s: store}}
		m := newPermissionLifecycleManager(t, store, spy)

		if _, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		submitted := spy.submittedSnapshots()
		if len(submitted) != 1 {
			t.Fatalf("lifecycle CreateTask submitted %d snapshots, want 1", len(submitted))
		}
		if submitted[0].PermissionMode != PermissionModeAsk {
			t.Errorf("lifecycle 提交 PermissionMode = %q, want ask（空串穿透即回归）", submitted[0].PermissionMode)
		}
	})
}

// TestCreate_PermissionMode_DualPathPassthrough lifecycle/legacy 双创建分支各以非默认值
//（ai-auto）透传：字段在快照/行两条路径均不丢失（丢失即被 mock 兜底为 ask，断言变红）。
func TestCreate_PermissionMode_DualPathPassthrough(t *testing.T) {
	t.Run("legacy_path", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		spy := &createTaskSpyStore{mockStore: store}
		m := newTestManager(t, spy, newMockProc(), newMockWorktree(), newMockOC(true))

		row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", PermissionMode: PermissionModeAIAuto})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		submitted := spy.submittedRows()
		if len(submitted) != 1 || submitted[0].PermissionMode != PermissionModeAIAuto {
			t.Errorf("legacy 提交 = %+v, want PermissionMode ai-auto 原样透传", submitted)
		}
		stored, _ := store.GetTask(context.Background(), row.ID)
		if stored.PermissionMode != PermissionModeAIAuto {
			t.Errorf("legacy 落库 = %q, want ai-auto", stored.PermissionMode)
		}
	})
	t.Run("lifecycle_path", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		spy := &createSnapshotSpyAdapter{mockAppAdapter: &mockAppAdapter{s: store}}
		m := newPermissionLifecycleManager(t, store, spy)

		row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", PermissionMode: PermissionModeAIAuto})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		submitted := spy.submittedSnapshots()
		if len(submitted) != 1 || submitted[0].PermissionMode != PermissionModeAIAuto {
			t.Errorf("lifecycle 提交快照 = %+v, want PermissionMode ai-auto（丢失字段即回归）", submitted)
		}
		stored, _ := store.GetTask(context.Background(), row.ID)
		if stored.PermissionMode != PermissionModeAIAuto {
			t.Errorf("lifecycle 落库 = %q, want ai-auto（快照丢字段会经兜底落为 ask）", stored.PermissionMode)
		}
	})
}
