package task

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
)

// --- task-base-branch-context 1.1：baseBranchShortName（D5 纯函数） ---

func TestBaseBranchShortName(t *testing.T) {
	cases := []struct {
		fullRef string
		want    string
		ok      bool
	}{
		{"refs/heads/main", "main", true},
		{"refs/heads/feature/x", "feature/x", true},
		{"refs/remotes/origin/main", "origin/main", true},
		{"refs/heads/", "", false},   // 前缀匹配但名字为空
		{"refs/remotes/", "", false}, // 前缀匹配但名字为空
		{"", "", false},
		{"main", "", false},            // 裸短名不是全限定引用
		{"refs/tags/v1", "", false},    // 其它 refs 前缀
		{"refs/heads", "", false},      // 缺尾斜杠
		{"REFS/heads/main", "", false}, // 大小写敏感
	}
	for _, tc := range cases {
		got, ok := baseBranchShortName(tc.fullRef)
		if ok != tc.ok || got != tc.want {
			t.Errorf("baseBranchShortName(%q) = (%q, %v), want (%q, %v)", tc.fullRef, got, ok, tc.want, tc.ok)
		}
	}
}

// --- task-base-branch-context 1.2/2.2：layerEnvSnapshot kind 分支（repo=6 / dir=4） ---

// ocdeckLifecycleCount 统计快照中 OCDECK_* 键个数（layerEnvSnapshot 结果只含生命周期
// OCDECK_* 键：reserved 前缀规则拒绝用户/全局 env 注入任何 OCDECK_*）。
func ocdeckLifecycleCount(merged map[string]string) int {
	n := 0
	for k := range merged {
		if strings.HasPrefix(k, "OCDECK_") {
			n++
		}
	}
	return n
}

// TestLayerEnvSnapshot_BaseBranchInjection（tasks 1.2/2.2）：repo 注入 BASE/HEAD 短名；
// dir 脏数据（有 base_ref/branch）也 MUST 缺两键；repo 异常行与未知 kind fail-closed
// 返回 error 且不持久化快照；OCDECK_SERVE_PORT 永不出现（调用方按场景注入）。
func TestLayerEnvSnapshot_BaseBranchInjection(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		baseRef  string
		branch   string
		wantBase string // "" 表示键必须不存在
		wantHead string
		wantErr  bool
	}{
		{name: "repo heads prefix", kind: ProjectKindRepo, baseRef: "refs/heads/main", branch: "ocdeck/my-task", wantBase: "main", wantHead: "ocdeck/my-task"},
		{name: "repo remotes prefix", kind: ProjectKindRepo, baseRef: "refs/remotes/origin/main", branch: "ocdeck/my-task", wantBase: "origin/main", wantHead: "ocdeck/my-task"},
		{name: "dir dirty base_ref/branch no keys", kind: ProjectKindDir, baseRef: "refs/heads/main", branch: "ocdeck/my-task"},
		{name: "repo empty base_ref", kind: ProjectKindRepo, baseRef: "", branch: "ocdeck/my-task", wantErr: true},
		{name: "repo illegal prefix", kind: ProjectKindRepo, baseRef: "refs/tags/v1", branch: "ocdeck/my-task", wantErr: true},
		{name: "repo bare short name", kind: ProjectKindRepo, baseRef: "main", branch: "ocdeck/my-task", wantErr: true},
		{name: "repo heads empty name", kind: ProjectKindRepo, baseRef: "refs/heads/", branch: "ocdeck/my-task", wantErr: true},
		{name: "repo empty branch", kind: ProjectKindRepo, baseRef: "refs/heads/main", branch: "", wantErr: true},
		{name: "unknown kind fail-closed", kind: "weird", baseRef: "refs/heads/main", branch: "ocdeck/my-task", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task", Branch: tc.branch, BaseRef: tc.baseRef,
				Status: StatusSuspended, WorktreePath: "/data/worktrees/p1/t1"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			merged, err := m.layerEnvSnapshot(context.Background(), store.tasks["t1"])
			if tc.wantErr {
				if err == nil {
					t.Fatalf("layerEnvSnapshot MUST fail on repo bad base_ref/branch or unknown kind")
				}
				if store.tasks["t1"].EnvSnapshot.Valid {
					t.Errorf("MUST NOT persist snapshot on error; got %q", store.tasks["t1"].EnvSnapshot.String)
				}
				return
			}
			if err != nil {
				t.Fatalf("layerEnvSnapshot: %v", err)
			}
			if _, hasPort := merged["OCDECK_SERVE_PORT"]; hasPort {
				t.Error("layerEnvSnapshot MUST NOT include OCDECK_SERVE_PORT")
			}
			gotBase, hasBase := merged["OCDECK_TASK_BASE_BRANCH"]
			gotHead, hasHead := merged["OCDECK_TASK_HEAD_BRANCH"]
			if tc.wantBase == "" {
				if hasBase {
					t.Errorf("OCDECK_TASK_BASE_BRANCH=%q, want absent (dir MUST NOT inject keys)", gotBase)
				}
				if hasHead {
					t.Errorf("OCDECK_TASK_HEAD_BRANCH=%q, want absent (dir MUST NOT inject keys)", gotHead)
				}
			} else {
				if gotBase != tc.wantBase {
					t.Errorf("OCDECK_TASK_BASE_BRANCH=%q, want %q", gotBase, tc.wantBase)
				}
				if gotHead != tc.wantHead {
					t.Errorf("OCDECK_TASK_HEAD_BRANCH=%q, want %q", gotHead, tc.wantHead)
				}
			}
			wantN := 4 // dir：4 个生命周期键不变
			if tc.wantBase != "" {
				wantN = 6 // repo：旧 4 + 两新键
			}
			if n := ocdeckLifecycleCount(merged); n != wantN {
				t.Errorf("OCDECK_* lifecycle count = %d, want %d (merged=%v)", n, wantN, merged)
			}
			if store.tasks["t1"].EnvSnapshot.Valid {
				t.Errorf("layerEnvSnapshot MUST NOT persist snapshot; got %q", store.tasks["t1"].EnvSnapshot.String)
			}
		})
	}
}

// TestMergeEnvSnapshot_BaseBranchLifecycleCounts（tasks 2.2）：merge repo=7（6+PORT）、
// dir=5（4+PORT，仅 merge 调用方注入 PORT）。
func TestMergeEnvSnapshot_BaseBranchLifecycleCounts(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		baseRef  string
		branch   string
		wantN    int
		wantBase string // "" 表示键必须不存在
	}{
		{name: "repo merge adds port over six lifecycle keys", kind: ProjectKindRepo, baseRef: "refs/heads/main", branch: "ocdeck/my-task", wantN: 7, wantBase: "main"},
		{name: "dir merge keeps five lifecycle keys", kind: ProjectKindDir, wantN: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: "main", Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "my task", Branch: tc.branch, BaseRef: tc.baseRef,
				Status: StatusSuspended, WorktreePath: "/data/worktrees/p1/t1"}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			merged, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001)
			if err != nil {
				t.Fatalf("mergeEnvSnapshot: %v", err)
			}
			if n := ocdeckLifecycleCount(merged); n != tc.wantN {
				t.Errorf("OCDECK_* lifecycle count = %d, want %d (merged=%v)", n, tc.wantN, merged)
			}
			if got := merged["OCDECK_SERVE_PORT"]; got != "50001" {
				t.Errorf("OCDECK_SERVE_PORT=%q, want 50001", got)
			}
			gotBase, hasBase := merged["OCDECK_TASK_BASE_BRANCH"]
			if tc.wantBase == "" {
				if hasBase {
					t.Errorf("OCDECK_TASK_BASE_BRANCH=%q, want absent (dir)", gotBase)
				}
				if _, has := merged["OCDECK_TASK_HEAD_BRANCH"]; has {
					t.Error("OCDECK_TASK_HEAD_BRANCH must be absent (dir)")
				}
			} else if gotBase != tc.wantBase {
				t.Errorf("OCDECK_TASK_BASE_BRANCH=%q, want %q", gotBase, tc.wantBase)
			}
		})
	}
}

// --- task-base-branch-context 1.3：loadEnvSnapshot 坏快照拒绝（普通 error，非 nil map） ---

func TestLoadEnvSnapshot_RejectsBadSnapshot(t *testing.T) {
	cases := []struct {
		name     string
		snapshot sql.NullString
	}{
		{name: "missing snapshot", snapshot: sql.NullString{}},
		{name: "empty string snapshot", snapshot: sql.NullString{String: "", Valid: true}},
		{name: "illegal json", snapshot: sql.NullString{String: "{not-json", Valid: true}},
		{name: "vars null", snapshot: sql.NullString{String: `{"vars":null}`, Valid: true}},
		{name: "vars missing", snapshot: sql.NullString{String: `{"foo":1}`, Valid: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = tc.snapshot })
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			row, _ := store.GetTask(context.Background(), "t1")
			vars, err := m.loadEnvSnapshot(row)
			if err == nil {
				t.Fatalf("loadEnvSnapshot(%s) MUST return ordinary error", tc.name)
			}
			if vars != nil {
				t.Errorf("loadEnvSnapshot MUST NOT return nil-typed success map on error; got %v", vars)
			}
			if isRecoveryTerminal(err) {
				t.Error("loadEnvSnapshot MUST NOT return recoveryTerminalError (D8: ordinary error only)")
			}
		})
	}
	// 合法空对象 vars：err==nil 且返回非 nil map。
	t.Run("valid empty vars returns non-nil map", func(t *testing.T) {
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		b, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{}})
		store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = b })
		m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
		row, _ := store.GetTask(context.Background(), "t1")
		vars, err := m.loadEnvSnapshot(row)
		if err != nil {
			t.Fatalf("loadEnvSnapshot: %v", err)
		}
		if vars == nil {
			t.Error("loadEnvSnapshot MUST NOT return nil map for valid snapshot")
		}
	})
}

// --- task-base-branch-context 2.3：Activate 持久化快照 + serve/CreateShell env 均含两键 ---

func TestActivate_InjectsBaseBranchKeys(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.BaseRef = "refs/remotes/origin/main" })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	wantBase, wantHead := "origin/main", "ocdeck/my-task"
	// 持久化快照。
	row, _ := store.GetTask(context.Background(), "t1")
	snap, err := m.loadEnvSnapshot(row)
	if err != nil {
		t.Fatalf("loadEnvSnapshot after Activate: %v", err)
	}
	// serve env。
	serveEnv := proc.envValues[runtimeSessionName("t1")]
	if serveEnv == nil {
		t.Fatal("serve session env missing after Activate")
	}
	// CreateShell env（读同一持久化快照）。
	shellID, err := m.CreateShell(context.Background(), "t1")
	if err != nil {
		t.Fatalf("CreateShell: %v", err)
	}
	shellEnv := proc.envValues[string(shellID)]
	if shellEnv == nil {
		t.Fatalf("shell session env missing for %s", shellID)
	}
	for label, env := range map[string]map[string]string{"snapshot": snap, "serve env": serveEnv, "shell env": shellEnv} {
		if got := env["OCDECK_TASK_BASE_BRANCH"]; got != wantBase {
			t.Errorf("%s: OCDECK_TASK_BASE_BRANCH=%q, want %q", label, got, wantBase)
		}
		if got := env["OCDECK_TASK_HEAD_BRANCH"]; got != wantHead {
			t.Errorf("%s: OCDECK_TASK_HEAD_BRANCH=%q, want %q", label, got, wantHead)
		}
	}
}

// --- task-base-branch-context 2.4：Activate 遇 repo 异常行 fail-closed ---

func TestActivate_RepoBadBaseRefFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		baseRef string
		branch  string
	}{
		{"empty base_ref", "", "ocdeck/my-task"},
		{"illegal base_ref prefix", "refs/tags/v1", "ocdeck/my-task"},
		{"empty branch", "refs/heads/main", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			store.mutTask("t1", func(r *TaskRow) { r.BaseRef = tc.baseRef; r.Branch = tc.branch })
			proc := newMockProc()
			m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
			m.SetLifecycleCtx(context.Background())
			err := m.Activate(context.Background(), "t1")
			if err == nil {
				t.Fatal("Activate MUST fail on repo bad base_ref/branch")
			}
			if OpErrorCode(err) != codeInternal {
				t.Errorf("error code = %s, want internal", OpErrorCode(err))
			}
			assertStatus(t, store, "t1", StatusSuspended)
			if n := len(proc.newSessionNamesSnapshot()); n != 0 {
				t.Errorf("NewSession calls = %d, want 0 (no process on env error)", n)
			}
			row, _ := store.GetTask(context.Background(), "t1")
			if row.EnvSnapshot.Valid {
				t.Errorf("env snapshot MUST NOT be persisted; got %q", row.EnvSnapshot.String)
			}
		})
	}
}

// --- task-base-branch-context 2.5：init 路径遇 repo 异常行 fail-closed ---

func TestRerunInit_BadBaseRefFailsBeforeScript(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	seedLifecycleConfig(store, "p1", "", "", "echo init")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// repo 项目 + 空白 BaseRef：若误先跑脚本/持久化则本测试失败。
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", BaseRef: "", Branch: "ocdeck/my-task", InitStatus: InitStatusFailed}
	if _, err := m.RerunInit(context.Background(), tid); err != nil {
		t.Fatalf("RerunInit claim: %v", err)
	}
	waitInitStatus(t, store, tid, InitStatusFailed, 3*time.Second)
	row, _ := store.GetTask(context.Background(), "t1")
	if !contains(row.InitError.String, "layer env snapshot") {
		t.Errorf("init_error=%q want to mention layer env snapshot", row.InitError.String)
	}
	if n := runner.runScriptCallCount(); n != 0 {
		t.Errorf("RunScript calls = %d, want 0 (must fail before script)", n)
	}
	assertStatus(t, store, tid, StatusSuspended)
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession calls = %d, want 0 (init failure must not activate)", n)
	}
}

// TestInitAttempt_BadBaseRefFailsBeforeScript（tasks 2.5 首次 init 编排）：与 rerun 用例
// 成对——runInitAttempt 在 CAS succeeded 后会 triggerActivate，是首次 init 特有副作用
// 边界；repo 行空白 BaseRef 时 MUST 在脚本执行前失败落 init_status=failed 且不激活。
func TestInitAttempt_BadBaseRefFailsBeforeScript(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// suspended + pending + 空白 BaseRef：首次 init 入口（非 RerunInit）。
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", BaseRef: "", Branch: "ocdeck/my-task", InitStatus: InitStatusPending}
	m.startInitRunner(tid)
	waitInitStatus(t, store, tid, InitStatusFailed, 3*time.Second)
	row, _ := store.GetTask(context.Background(), "t1")
	if !contains(row.InitError.String, "layer env snapshot") {
		t.Errorf("init_error=%q want to mention layer env snapshot", row.InitError.String)
	}
	if n := runner.runScriptCallCount(); n != 0 {
		t.Errorf("RunScript calls = %d, want 0 (must fail before script)", n)
	}
	// 仅 CAS succeeded 才 triggerActivate：失败路径零 NewSession（未进入自动激活）。
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession calls = %d, want 0 (init failure must not triggerActivate)", n)
	}
	assertStatus(t, store, tid, StatusSuspended)
}

// --- task-base-branch-context 2.6：pre-delete 路径 ---

// TestPreDelete_BadBaseRefFailsBeforeScript：repo 异常行 + 目录存在 + 脚本配置 →
// deletion_failed（pre-delete: 前缀 + layer env snapshot 原因），脚本零执行、绝不 wt.Remove。
func TestPreDelete_BadBaseRefFailsBeforeScript(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	seedLifecycleConfig(store, "p1", "", "", "echo cleanup")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	wtPath := t.TempDir()
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, BaseRef: "", Branch: "ocdeck/my-task", InitStatus: InitStatusNone}
	if err := m.Delete(context.Background(), tid, DeleteNormal, false); err == nil {
		t.Fatal("Delete MUST fail (deletion_failed) when layerEnvSnapshot rejects repo row")
	}
	assertStatus(t, store, tid, StatusDeletionFailed)
	lastErrorContains(t, store, tid, "pre-delete:")
	lastErrorContains(t, store, tid, "layer env snapshot")
	if n := runner.runScriptCallCount(); n != 0 {
		t.Errorf("RunScript calls = %d, want 0", n)
	}
	// 绝不越过失败点删除 worktree（pre-delete 失败后零 wt.Remove）。
	if got := wt.removeCalls(); got != 0 {
		t.Errorf("wt.Remove calls = %d, want 0", got)
	}
	assertTaskExists(t, store, tid)
}

// TestPreDelete_SkipPathsNeverLayerEnv（tasks 2.6）：Force / worktree 不存在 / 空 pre_delete
// 三条跳过路径不得因本变更开始调用 layerEnvSnapshot——repo 行故意带非法（空）BaseRef，
// 任何误调 layer 都会导致删除失败，使断言失败。
func TestPreDelete_SkipPathsNeverLayerEnv(t *testing.T) {
	cases := []struct {
		name           string
		mode           DeleteMode
		preDelete      string
		worktreeExists bool
	}{
		{name: "force skips pre-delete", mode: DeleteForce, preDelete: "echo cleanup", worktreeExists: true},
		{name: "missing worktree skips pre-delete", mode: DeleteNormal, preDelete: "echo cleanup", worktreeExists: false},
		{name: "empty script skips pre-delete", mode: DeleteNormal, preDelete: "", worktreeExists: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
			seedLifecycleConfig(store, "p1", "", "", tc.preDelete)
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
			tid := "t1"
			row := TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, BaseRef: "", Branch: "", InitStatus: InitStatusNone}
			if tc.worktreeExists {
				row.WorktreePath = t.TempDir()
			} else {
				row.WorktreePath = filepath.Join(t.TempDir(), "missing")
			}
			store.tasks[tid] = row
			if err := m.Delete(context.Background(), tid, tc.mode, false); err != nil {
				t.Fatalf("Delete(%s) MUST succeed on skip path: %v", tc.name, err)
			}
			if n := runner.runScriptCallCount(); n != 0 {
				t.Errorf("RunScript calls = %d, want 0 (skip path must not run script)", n)
			}
		})
	}
}

// --- task-base-branch-context 2.7：Recovery 复用同代快照，不重分层 ---

// getProjectCountingStore 统计 GetProject 调用（突变证据）：layerEnvSnapshot 内部必须
// GetProject；合法 Recovery 仅 ensureRecovery kind 门禁调 1 次，若回归为 mergeEnvSnapshot
// 重分层则会再 +1。
type getProjectCountingStore struct {
	*mockStore
	getProjectCalls atomic.Int64
}

func (s *getProjectCountingStore) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	s.getProjectCalls.Add(1)
	return s.mockStore.GetProject(ctx, id)
}

func TestRecovery_ReuseSameGenerationSnapshot(t *testing.T) {
	cases := []struct {
		name     string
		snapVars map[string]string
		wantBase string // "" 表示两键必须仍不存在
		wantHead string
	}{
		{
			name:     "legacy snapshot without branch keys stays keyless",
			snapVars: map[string]string{"OCDECK_TASK_ID": "t1", "OCDECK_SERVE_PORT": "50001"},
		},
		{
			name: "existing branch keys preserved verbatim",
			snapVars: map[string]string{
				"OCDECK_TASK_ID":          "t1",
				"OCDECK_TASK_BASE_BRANCH": "origin/main",
				"OCDECK_TASK_HEAD_BRANCH": "ocdeck/my-task",
				"OCDECK_SERVE_PORT":       "50001",
			},
			wantBase: "origin/main",
			wantHead: "ocdeck/my-task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			spy := &getProjectCountingStore{mockStore: store}
			seedSuspendedTask(store, "t1", "p1")
			// 行值故意与快照值不同：若 Recovery 重分层，BASE 会被重算为 main、
			// HEAD 重算为 ocdeck/rotated —— 下方断言即失败（证明未重分层）。
			store.mutTask("t1", func(r *TaskRow) {
				r.Status = StatusActive
				r.BaseRef = "refs/heads/main"
				r.Branch = "ocdeck/rotated"
				b, _ := encodeEnvSnapshot(envSnapshot{Vars: tc.snapVars})
				r.EnvSnapshot = b
			})
			proc := newMockProc()
			m := newTestManager(t, spy, proc, newMockWorktree(), newMockOC(true))
			m.SetLifecycleCtx(context.Background())
			rt := m.newRuntime("t1")
			m.setRuntime("t1", rt)
			rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

			m.ensureRecovery("t1", rt.instVersion)
			waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

			serveEnv := proc.envValues[runtimeSessionName("t1")]
			if serveEnv == nil {
				t.Fatal("serve session env missing after recovery")
			}
			row, _ := store.GetTask(context.Background(), "t1")
			snap, err := m.loadEnvSnapshot(row)
			if err != nil {
				t.Fatalf("loadEnvSnapshot after recovery: %v", err)
			}
			for label, env := range map[string]map[string]string{"serve env": serveEnv, "persisted snapshot": snap} {
				gotBase, hasBase := env["OCDECK_TASK_BASE_BRANCH"]
				gotHead, hasHead := env["OCDECK_TASK_HEAD_BRANCH"]
				if tc.wantBase == "" {
					if hasBase {
						t.Errorf("%s: OCDECK_TASK_BASE_BRANCH=%q, want absent (same-generation snapshot must not gain keys)", label, gotBase)
					}
					if hasHead {
						t.Errorf("%s: OCDECK_TASK_HEAD_BRANCH=%q, want absent", label, gotHead)
					}
				} else {
					if gotBase != tc.wantBase {
						t.Errorf("%s: OCDECK_TASK_BASE_BRANCH=%q, want %q (MUST NOT re-layer)", label, gotBase, tc.wantBase)
					}
					if gotHead != tc.wantHead {
						t.Errorf("%s: OCDECK_TASK_HEAD_BRANCH=%q, want %q (MUST NOT re-layer)", label, gotHead, tc.wantHead)
					}
				}
			}
			// 仅 OCDECK_SERVE_PORT 允许更新：持久化快照端口与最新 serve env 一致。
			if snap["OCDECK_SERVE_PORT"] != serveEnv["OCDECK_SERVE_PORT"] {
				t.Errorf("snapshot port=%q, serve env port=%q (PORT must be the only updated var)", snap["OCDECK_SERVE_PORT"], serveEnv["OCDECK_SERVE_PORT"])
			}
			// 突变证据：合法实现 Recovery 期间仅 kind 门禁调 GetProject 一次。
			if n := spy.getProjectCalls.Load(); n != 1 {
				t.Errorf("GetProject calls during recovery = %d, want 1 (layerEnvSnapshot MUST NOT be called)", n)
			}
		})
	}
}

// --- task-base-branch-context 2.8：坏快照 → 终态补偿，零 permit/backoff/进程副作用 ---

// badSnapshotSpyStore 记录 UpdateTaskEnvSnapshot 的 Valid=true 写入与
// CompleteRecoveryFailureAndClearDebts 调用次数（R1-01）：坏快照路径 MUST NOT 走
// persist 路径（Valid=true 写入=0，防止"先违规 persist 再被终态补偿清空"漏检），
// 终态补偿恰好走单事务一次。
type badSnapshotSpyStore struct {
	*mockStore
	envWrites     atomic.Int64
	completeCalls atomic.Int64
}

func (s *badSnapshotSpyStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if envSnapshot.Valid {
		s.envWrites.Add(1)
	}
	return s.mockStore.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

func (s *badSnapshotSpyStore) CompleteRecoveryFailureAndClearDebts(ctx context.Context, id string, lastError sql.NullString) (application.TransitionResult, error) {
	s.completeCalls.Add(1)
	return s.mockStore.CompleteRecoveryFailureAndClearDebts(ctx, id, lastError)
}

func TestRecovery_BadSnapshotTerminalBeforePermit(t *testing.T) {
	cases := []struct {
		name     string
		snapshot sql.NullString
	}{
		{"missing snapshot", sql.NullString{}},
		{"illegal json", sql.NullString{String: "{not-json", Valid: true}},
		{"vars null", sql.NullString{String: `{"vars":null}`, Valid: true}},
		{"vars missing", sql.NullString{String: `{"foo":1}`, Valid: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			spy := &badSnapshotSpyStore{mockStore: store}
			seedSuspendedTask(store, "t1", "p1")
			store.mutTask("t1", func(r *TaskRow) {
				r.Status = StatusActive
				r.EnvSnapshot = tc.snapshot
			})
			proc := newMockProc()
			m := newTestManager(t, spy, proc, newMockWorktree(), newMockOC(true))
			var backoffs []int
			m.recoveryBackoffFn = func(ordinal int) time.Duration {
				backoffs = append(backoffs, ordinal)
				return 0
			}
			rt := m.newRuntime("t1")
			m.setRuntime("t1", rt)
			rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

			m.ensureRecovery("t1", rt.instVersion)

			// 终态补偿事务照常落账且恰好执行一次：suspended + last_error + env_snapshot=NULL
			// 全部走 CompleteRecoveryFailureAndClearDebts 单事务，而非普通 env 写入。
			if n := spy.completeCalls.Load(); n != 1 {
				t.Errorf("CompleteRecoveryFailureAndClearDebts calls = %d, want 1", n)
			}
			row, _ := store.GetTask(context.Background(), "t1")
			if row.Status != StatusSuspended {
				t.Fatalf("status=%s want suspended (terminal compensation)", row.Status)
			}
			if !row.LastError.Valid || !contains(row.LastError.String, "env snapshot") {
				t.Errorf("last_error=%v want to mention env snapshot", row.LastError)
			}
			if row.EnvSnapshot.Valid {
				t.Errorf("env_snapshot=%q want NULL (terminal transaction)", row.EnvSnapshot.String)
			}
			// 突变证据：坏快照路径零 persist。若 attempt 违规先 persist 更新后的 env map
			// 再被终态补偿清空，Valid=true 写入次数必 > 0，此断言即失败。
			if n := spy.envWrites.Load(); n != 0 {
				t.Errorf("UpdateTaskEnvSnapshot(Valid=true) writes = %d, want 0 (bad snapshot path MUST NOT persistEnvSnapshot)", n)
			}
			// 快照加载在 permit/backoff/attempt 之前：三者零消耗。
			if n := store.recoveryPermitCount("t1"); n != 0 {
				t.Errorf("permits=%d want 0 (snapshot loaded before permit)", n)
			}
			if len(backoffs) != 0 {
				t.Errorf("backoff ordinals=%v want none", backoffs)
			}
			if n := len(proc.newSessionNamesSnapshot()); n != 0 {
				t.Errorf("NewSession calls=%d want 0", n)
			}
		})
	}
}
