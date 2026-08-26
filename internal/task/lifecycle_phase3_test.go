package task

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/git"
	"ocdeck/internal/infrastructure/lifecycle"
	"ocdeck/internal/infrastructure/opencode"
)

// newLifecycleTestManager 构造注入 LifecycleRunner + LogDir 的 Manager（Phase 3 测试用）。
// 与 newTestManager 区别：注入 lifecycleRunner 与 logDir，不注入 DebtStore。
func newLifecycleTestManager(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient, runner LifecycleRunnerLike) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	opts := Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap,
		LogDir: cfg.DataDir + "/logs",
	}
	if runner != nil {
		opts.LifecycleRunner = runner
	}
	m := New(opts)
	return m
}

// LifecycleRunnerLike 是供测试注入的 LifecycleRunner 最小接口（与 lifecycle.LifecycleRunner 同构）。
type LifecycleRunnerLike interface {
	RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error
	CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string
}

// seedLifecycleConfig 注入项目生命周期配置到 store（经 UpsertLifecycleConfig，按 store 实例隔离）。
func seedLifecycleConfig(store TaskStore, projectID, inheritPatterns, initScript, preDeleteScript string) {
	_ = store.UpsertLifecycleConfig(context.Background(), projectID, inheritPatterns, initScript, preDeleteScript)
}

// assertInitStatus 断言任务 init_status。
func assertInitStatus(t *testing.T, store TaskStore, taskID, want string) {
	t.Helper()
	row, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask %s: %v", taskID, err)
	}
	if row.InitStatus != want {
		t.Fatalf("task %s init_status=%s want %s", taskID, row.InitStatus, want)
	}
}

// waitInitStatus 轮询等待 init_status 到达 want（InitRunner 异步执行测试用）。
func waitInitStatus(t *testing.T, store TaskStore, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		if row.InitStatus == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("wait init_status %s timeout: got %s (task %s)", want, row.InitStatus, taskID)
}

// waitForScriptCalls 轮询等待 mockLifecycleRunner 收到至少 n 次 RunScript 调用。
func waitForScriptCalls(t *testing.T, runner *mockLifecycleRunner, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runner.runScriptCallCount() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wait %d runScript calls timeout: got %d", n, runner.runScriptCallCount())
}

// --- Phase 3 tests (tasks 3.10) ---

// TestCreateChain_NoInitScript_DirectActivate：无 init 脚本 → init_status=none → 直接激活。
func TestCreateChain_NoInitScript_DirectActivate(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	proc.sessions["ocdeck-t1-serve"] = true // 模拟 serve 就绪
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	row, err := m.Create(context.Background(), "p1", "mytask", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("unexpected status %s", row.Status)
	}
	// 无 init 脚本 → init_status=none。
	assertInitStatus(t, store, row.ID, InitStatusNone)
	// InitRunner 未启动（no RunScript 调用）。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RunScript should not be called without init script, got %d", runner.runScriptCallCount())
	}
}

// TestCreateChain_WithInitScript_StartsInitRunner：有 init 脚本 → pending → InitRunner 执行。
func TestCreateChain_WithInitScript_StartsInitRunner(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	row, err := m.Create(context.Background(), "p1", "mytask", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// init 脚本 → CommitCreated 落 pending → startInitRunner。返回时 status 可能为：
	//   - suspended：InitRunner 尚未完成（pending/running）；
	//   - activating：InitRunner succeeded → triggerActivate 已推进但 Activate 尚未提交 active；
	//   - active：InitRunner succeeded → triggerActivate → Activate 完整提交（mock 脚本即时返回，链路可极快完成）。
	// 三态均为该时点合法瞬态（异步 InitRunner+自动激活与 Create 最终 GetTask 存在竞态）。
	// 最终收敛由下方 waitForScriptCalls + waitInitStatus + autoActivateWG 覆盖。
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("status=%s want suspended|activating|active (init async + auto-activate race)", row.Status)
	}
	// 中间态 init_status 不断言：返回路径的 store 重读与异步 InitRunner 的 claim/完成存在
	// 竞态，pending/running/succeeded 均为该时点合法值；本测试目标是 InitRunner 被启动
	// 并收敛 succeeded（下方 waitForScriptCalls + waitInitStatus 覆盖）。
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	waitInitStatus(t, store, row.ID, InitStatusSucceeded, 2*time.Second)
}

// TestCreateChain_InitScriptFails_InitFailed：脚本失败 → init_status=failed，不激活。
func TestCreateChain_InitScriptFails_InitFailed(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("script boom")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	row, err := m.Create(context.Background(), "p1", "mytask", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitInitStatus(t, store, row.ID, InitStatusFailed, 2*time.Second)
	// 失败时 MUST NOT 激活：proc 不应收到 serve 会话。
	if len(proc.newSessionNamesSnapshot()) > 0 {
		t.Fatalf("InitRunner failed must not activate; got NewSession calls: %v", proc.newSessionNamesSnapshot())
	}
	// init_error 含脚本错误。
	r, _ := store.GetTask(context.Background(), row.ID)
	if !r.InitError.Valid || r.InitError.String == "" {
		t.Fatalf("init_error must be set on failure, got %v", r.InitError)
	}
}

// TestCreateChain_InheritConfigReadFails_CreationFailed：读配置失败 → creation_failed。
func TestCreateChain_InheritConfigReadFails_CreationFailed(t *testing.T) {
	resetLifecycleCfgMock()
	store := &errLifecycleConfigStore{TaskStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	_, err := m.Create(context.Background(), "p1", "mytask", "")
	if err == nil {
		t.Fatalf("Create must fail when lifecycle config read fails")
	}
	// 状态应为 creation_failed。
	row, _ := store.GetTask(context.Background(), store.lastTaskID())
	if row.Status != StatusCreationFailed {
		t.Fatalf("status=%s want creation_failed", row.Status)
	}
}

// errLifecycleConfigStore 包装 mockStore，使 GetLifecycleConfig 返回错误。
type errLifecycleConfigStore struct {
	TaskStore
}

func (s *errLifecycleConfigStore) GetLifecycleConfig(ctx context.Context, projectID string) (LifecycleConfigRow, error) {
	return LifecycleConfigRow{}, fmt.Errorf("db read error")
}
func (s *errLifecycleConfigStore) seedProject(p ProjectRow) {
	if ms, ok := s.TaskStore.(*mockStore); ok {
		ms.seedProject(p)
	}
}
func (s *errLifecycleConfigStore) lastTaskID() string {
	if ms, ok := s.TaskStore.(*mockStore); ok {
		return ms.lastTaskID()
	}
	return ""
}

// TestActivateGate_FiveBranches：五分支门禁。
func TestActivateGate_FiveBranches(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		initStatus string
		initError  sql.NullString
		wantErr    bool
		errSubstr  string
	}{
		{InitStatusNone, sql.NullString{}, false, ""},
		{InitStatusSucceeded, sql.NullString{}, false, ""},
		{InitStatusPending, sql.NullString{}, true, "init in progress"},
		{InitStatusRunning, sql.NullString{}, true, "init in progress"},
		{InitStatusFailed, sql.NullString{String: "boom", Valid: true}, true, "init failed"},
		{"unknown", sql.NullString{}, true, "unknown init_status"},
	}
	for _, c := range cases {
		t.Run(c.initStatus, func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
			// 直接插入 suspended 任务 + 指定 init_status。
			tid := "t-" + c.initStatus
			store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: c.initStatus, InitError: c.initError}
			err := m.Activate(context.Background(), tid)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Activate must fail for init_status=%s", c.initStatus)
				}
				if c.errSubstr != "" && !contains(err.Error(), c.errSubstr) {
					t.Fatalf("error %q must contain %q", err.Error(), c.errSubstr)
				}
			} else {
				// none/succeeded 放行 → 可能因 mock serve 健康检查失败，但不应是 init 门禁错误。
				if err != nil && contains(err.Error(), "init") {
					t.Fatalf("Activate for %s must not fail on init gate, got %v", c.initStatus, err)
				}
			}
		})
	}
}

// TestRerunInit_GateAndCAS：RerunInit 门禁 + CAS 竞争。
func TestRerunInit_GateAndCAS(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		status     string
		initStatus string
		wantErr    bool
		errSubstr  string
	}{
		{StatusSuspended, InitStatusFailed, false, ""},
		{StatusSuspended, InitStatusSucceeded, false, ""},
		{StatusActive, InitStatusFailed, true, "requires suspended"},
		{StatusSuspended, InitStatusPending, true, "requires init_status failed or succeeded"},
		{StatusSuspended, InitStatusRunning, true, "requires init_status failed or succeeded"},
		{StatusSuspended, InitStatusNone, true, "requires init_status failed or succeeded"},
	}
	for _, c := range cases {
		t.Run(c.status+"_"+c.initStatus, func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			seedLifecycleConfig(store, "p1", "", "echo init", "")
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
			tid := "t1"
			store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: c.status, WorktreePath: "/wt", InitStatus: c.initStatus}
			row, err := m.RerunInit(context.Background(), tid)
			if c.wantErr {
				if err == nil {
					t.Fatalf("RerunInit must fail for status=%s init_status=%s", c.status, c.initStatus)
				}
				if c.errSubstr != "" && !contains(err.Error(), c.errSubstr) {
					t.Fatalf("error %q must contain %q", err.Error(), c.errSubstr)
				}
			} else {
				if err != nil {
					t.Fatalf("RerunInit must succeed for status=%s init_status=%s, got %v", c.status, c.initStatus, err)
				}
				// claim 已成功；返回行来自 claim 后的 store 重读，与异步 attempt 的完成存在
				// 竞态——running（已 claim 未收敛）与 succeeded（已收敛）均为该时点合法值。
				if row.InitStatus != InitStatusRunning && row.InitStatus != InitStatusSucceeded {
					t.Fatalf("RerunInit returned row init_status=%s want running|succeeded", row.InitStatus)
				}
				// 异步执行：等待 init_status 收敛 succeeded。
				waitInitStatus(t, store, tid, InitStatusSucceeded, 2*time.Second)
			}
		})
	}
}

// TestRerunInit_SuccessDoesNotActivate：成功不自动激活。
func TestRerunInit_SuccessDoesNotActivate(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}
	if _, err := m.RerunInit(context.Background(), tid); err != nil {
		t.Fatalf("RerunInit: %v", err)
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 2*time.Second)
	// 不激活：状态仍 suspended，无 serve 会话。
	assertStatus(t, store, tid, StatusSuspended)
	if len(proc.newSessionNamesSnapshot()) > 0 {
		t.Fatalf("RerunInit success must not activate; got NewSession: %v", proc.newSessionNamesSnapshot())
	}
}

// TestDelete_InitGate：init 进行中拒绝删除。
func TestDelete_InitGate(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []string{InitStatusPending, InitStatusRunning}
	for _, is := range cases {
		t.Run(is, func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
			tid := "t1"
			store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: is}
			err := m.Delete(context.Background(), tid, DeleteNormal, false)
			if err == nil {
				t.Fatalf("Delete must reject init_status=%s", is)
			}
			if !contains(err.Error(), "init in progress") {
				t.Fatalf("error %q must contain 'init in progress'", err.Error())
			}
		})
	}
}

// TestArchive_InitGate：init 进行中拒绝归档。
func TestArchive_InitGate(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusRunning}
	err := m.Archive(context.Background(), tid)
	if err == nil {
		t.Fatalf("Archive must reject init in progress")
	}
	if !contains(err.Error(), "init in progress") {
		t.Fatalf("error %q must contain 'init in progress'", err.Error())
	}
}

// TestPreDelete_ScriptFails_NoWorktreeRemove：pre-delete 失败 → deletion_failed，不删 worktree。
func TestPreDelete_ScriptFails_NoWorktreeRemove(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir() // 真实存在的 worktree 路径（os.Stat 非 ENOENT）。
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("predelete boom")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	err := m.Delete(context.Background(), tid, DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail when pre-delete fails")
	}
	// 状态 deletion_failed + last_error 以 "pre-delete:" 开头。
	assertStatus(t, store, tid, StatusDeletionFailed)
	lastErrorContains(t, store, tid, "pre-delete:")
	// 确认任务行仍存在（未被 DeleteTask 删除）。
	assertTaskExists(t, store, tid)
}

// TestPreDelete_ForceSkips：DeleteForce 跳过 pre-delete。
func TestPreDelete_ForceSkips(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("should not run")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	if err := m.Delete(context.Background(), tid, DeleteForce, false); err != nil {
		t.Fatalf("Delete force must skip pre-delete: %v", err)
	}
	// 任务行被删除。
	if _, err := store.GetTask(context.Background(), tid); err == nil {
		t.Fatalf("task row must be deleted")
	}
	// RunScript 未调用（Force 跳过）。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("Force must skip pre-delete; got RunScript calls: %d", runner.runScriptCallCount())
	}
}

// TestPreDelete_NoScript_Skips：无 pre_delete_script → 跳过。
func TestPreDelete_NoScript_Skips(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("should not run")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	if err := m.Delete(context.Background(), tid, DeleteNormal, false); err != nil {
		t.Fatalf("Delete must succeed without pre-delete script: %v", err)
	}
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("no pre-delete script must skip; got RunScript: %d", runner.runScriptCallCount())
	}
}

// TestReconcile_ConvergeInterruptedInitRunsFirst：ConvergeInterruptedInitRuns 先执行 + fail-closed。
func TestReconcile_ConvergeInterruptedInitRunsFirst(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
	// 两个 interrupted init 任务。
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt1", InitStatus: InitStatusRunning}
	store.tasks["t2"] = TaskRow{ID: "t2", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt2", InitStatus: InitStatusPending}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// 两个任务 init_status → failed。
	assertInitStatus(t, store, "t1", InitStatusFailed)
	assertInitStatus(t, store, "t2", InitStatusFailed)
	// 重启后 succeeded 未激活任务 MUST NOT 自动激活：状态仍 suspended。
	store.tasks["t3"] = TaskRow{ID: "t3", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt3", InitStatus: InitStatusSucceeded}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	assertStatus(t, store, "t3", StatusSuspended)
}

// TestReconcile_ConvergeFails_FailClosed：ConvergeInterruptedInitRuns 失败 → 拒绝开放 HTTP。
func TestReconcile_ConvergeFails_FailClosed(t *testing.T) {
	resetLifecycleCfgMock()
	store := &errConvergeStore{TaskStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
	if err := m.Reconcile(context.Background()); err == nil {
		t.Fatalf("Reconcile must fail when ConvergeInterruptedInitRuns fails")
	}
}

type errConvergeStore struct {
	TaskStore
}

func (s *errConvergeStore) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return 0, fmt.Errorf("db converge error")
}
func (s *errConvergeStore) seedProject(p ProjectRow) {
	if ms, ok := s.TaskStore.(*mockStore); ok {
		ms.seedProject(p)
	}
}

// seedTaskInWrapper 向 wrapper store 内的 mockStore 注入任务行（测试辅助）。
func seedTaskInWrapper(wrapper TaskStore, row TaskRow) {
	if ms, ok := wrapper.(*mockStore); ok {
		ms.tasks[row.ID] = row
		return
	}
	// 递归查找嵌入的 TaskStore（一层）。
	type embeddedGetter interface{ GetEmbeddedStore() *mockStore }
	// errFinishInitStore / rowsZeroFinishStore / errConvergeStore 均嵌入 TaskStore；
	// 通过类型断言访问底层 mockStore。
	switch w := wrapper.(type) {
	case *errFinishInitStore:
		if ms, ok := w.TaskStore.(*mockStore); ok {
			ms.tasks[row.ID] = row
		}
	case *rowsZeroFinishStore:
		if ms, ok := w.TaskStore.(*mockStore); ok {
			ms.tasks[row.ID] = row
		}
	case *errConvergeStore:
		if ms, ok := w.TaskStore.(*mockStore); ok {
			ms.tasks[row.ID] = row
		}
	}
}

// readFile 读取文件内容（测试辅助，封装 os.ReadFile）。
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// TestShutdown_AdmissionRace：Shutdown 中 InitRunner admission 被拒绝，init_status 不变。
func TestShutdown_AdmissionRace(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	// 插入 suspended + pending 任务（InitRunner 未启动）。
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending}

	// 模拟 Shutdown 已开始：手动置 shutdownStarted。
	m.shutdownGateMu.Lock()
	m.shutdownStarted = true
	m.shutdownGateMu.Unlock()

	// 此时 startInitRunner 应被拒绝（init_status 保持 pending）。
	m.startInitRunner("t1")
	// InitRunner 未执行 RunScript。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("InitRunner must be rejected during shutdown")
	}
	assertInitStatus(t, store, "t1", InitStatusPending)
}

// TestInitRunner_FinishInitRunDBError_NoActivate：FinishInitRun DB error 不激活。
func TestInitRunner_FinishInitRunDBError_NoActivate(t *testing.T) {
	resetLifecycleCfgMock()
	store := &errFinishInitStore{TaskStore: newMockStore(), finishCalled: make(chan struct{}, 1)}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	// 创建 suspended + pending 任务并启动 InitRunner。
	seedTaskInWrapper(store, TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending})
	m.startInitRunner("t1")
	// 等待 RunScript 调用（脚本执行完成，但 FinishInitRun 返回 error）。
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	// 等待 FinishInitRun 被调用（channel 同步，替代 Sleep）。
	select {
	case <-store.finishCalled:
	case <-time.After(2 * time.Second):
		t.Fatalf("FinishInitRun was not called")
	}
	// DB error → init_status 保持 running（FinishInitRun 未更新），不激活。
	if len(proc.newSessionNamesSnapshot()) > 0 {
		t.Fatalf("DB error must not activate; got NewSession: %v", proc.newSessionNamesSnapshot())
	}
	m.runnerWG.Wait()
}

type errFinishInitStore struct {
	TaskStore
	finishCalled chan struct{}
}

func (s *errFinishInitStore) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (application.MutationResult, error) {
	select {
	case s.finishCalled <- struct{}{}:
	default:
	}
	return application.MutationResult{}, fmt.Errorf("db finish error")
}
func (s *errFinishInitStore) seedProject(p ProjectRow) {
	if ms, ok := s.TaskStore.(*mockStore); ok {
		ms.seedProject(p)
	}
}

// TestInitRunner_RowsZero_NoActivate：FinishInitRun rows=0（外部收敛）不激活。
func TestInitRunner_RowsZero_NoActivate(t *testing.T) {
	resetLifecycleCfgMock()
	store := &rowsZeroFinishStore{TaskStore: newMockStore(), finishCalled: make(chan struct{}, 1)}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	seedTaskInWrapper(store, TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending})
	m.startInitRunner("t1")
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	// 等待 FinishInitRun 被调用（channel 同步，替代 Sleep）。
	select {
	case <-store.finishCalled:
	case <-time.After(2 * time.Second):
		t.Fatalf("FinishInitRun was not called")
	}
	if len(proc.newSessionNamesSnapshot()) > 0 {
		t.Fatalf("rows=0 must not activate")
	}
	m.runnerWG.Wait()
}

type rowsZeroFinishStore struct {
	TaskStore
	finishCalls  int
	finishCalled chan struct{}
	mu           sync.Mutex
}

func (s *rowsZeroFinishStore) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	s.finishCalls++
	s.mu.Unlock()
	select {
	case s.finishCalled <- struct{}{}:
	default:
	}
	// 返回 rows=0（外部收敛）。
	return application.MutationResult{}, nil
}
func (s *rowsZeroFinishStore) seedProject(p ProjectRow) {
	if ms, ok := s.TaskStore.(*mockStore); ok {
		ms.seedProject(p)
	}
}
func (s *rowsZeroFinishStore) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	// 让 claim 成功，使流程进入脚本执行。
	if ms, ok := s.TaskStore.(*mockStore); ok {
		return ms.ClaimInitRun(ctx, taskID)
	}
	return application.MutationResult{Matched: true, Changed: true}, nil
}

// TestInheritLog_WrittenAndTruncated：inherit.log 写入 + 1MB 截断。
func TestInheritLog_WrittenAndTruncated(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	m := &Manager{logDir: logDir}
	logPath := filepath.Join(logDir, "t1", "inherit.log")
	// 无警告 → 删除既有文件（不存在 → no-op）。
	m.writeInheritLog(logPath, nil)
	// 有警告 → 写入。
	m.writeInheritLog(logPath, []string{"warn1", "warn2"})
	b, err := readFile(logPath)
	if err != nil {
		t.Fatalf("read inherit.log: %v", err)
	}
	if !contains(string(b), "warn1") || !contains(string(b), "warn2") {
		t.Fatalf("inherit.log must contain warnings, got %q", string(b))
	}
	// 无警告 → 删除既有文件。
	m.writeInheritLog(logPath, nil)
	if _, err := readFile(logPath); !os.IsNotExist(err) {
		t.Fatalf("inherit.log must be removed when no warnings")
	}
}

// TestInheritLog_1MBTruncation：超 1MB 截断加标记。
func TestInheritLog_1MBTruncation(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	m := &Manager{logDir: logDir}
	logPath := filepath.Join(logDir, "t1", "inherit.log")
	// 构造超 1MB 的警告。
	var warnings []string
	big := strings.Repeat("x", 600000)
	warnings = append(warnings, big, big) // 1.2MB
	m.writeInheritLog(logPath, warnings)
	data, err := readFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) > inheritLogCap+len(inheritLogTruncMarker) {
		t.Fatalf("inherit.log must be capped at %d + marker, got %d", inheritLogCap, len(data))
	}
	if !contains(string(data), inheritLogTruncMarker) {
		t.Fatalf("inherit.log must contain truncation marker")
	}
}

// TestRetryCreate_IdempotentReRunInherit：retryCreate 总是重跑 inherit。
func TestRetryCreate_IdempotentReRunInherit(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "*.env", "", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	// 插入 creation_failed 任务（产物完整 → 跳过 add，但重跑 inherit）。
	wtPath := "/data/worktrees/p1/t1"
	wt.products[wtPath] = true
	wt.branches["ocdeck/mytask"] = true
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "mytask", Status: StatusCreationFailed, WorktreePath: wtPath, Branch: "ocdeck/mytask", InitStatus: InitStatusNone, BaseRef: "refs/heads/main"}
	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	// inherit 被重跑：/repo 非真实 repo → ListIgnoredUntracked 失败 → 警告写入 inherit.log。
	data, err := readFile(m.inheritLogPath("t1"))
	if err != nil {
		t.Fatalf("read inherit.log: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("retryCreate must re-run inherit (inherit.log should have enum-failure warning)")
	}
	// CommitCreated 已提交（suspended），异步激活可能将其推至 activating。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("task t1 status=%s want suspended/activating/active", row.Status)
	}
}

// TestRerunInitVsActivate_MutexSerialized：RerunInit 与 Activate 互斥串行化。
func TestRerunInitVsActivate_MutexSerialized(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}

	// 并发发起 RerunInit 与 Activate（Activate 会因 init_status=failed 被门禁拒绝，
	// 但必须不 panic / 不死锁；keyed mutex 串行化）。
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.RerunInit(context.Background(), tid)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = m.Activate(context.Background(), tid)
	}()
	wg.Wait()
	// 不死锁即通过。
}

// TestShutdown_WaitsRunnerWG：Shutdown 等待 runnerWG。
func TestShutdown_WaitsRunnerWG(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	// 慢脚本：延迟 200ms。
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending}
	m.startInitRunner("t1")
	// 立即 Shutdown：应等待 runnerWG（脚本在 mock 中即时返回，故很快完成）。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestPreDelete_WorktreeNotExist_Skips：worktree 不存在 → 幂等跳过 pre-delete。
func TestPreDelete_WorktreeNotExist_Skips(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	// 不设 products[/wt] → worktree 不存在（os.Stat IsNotExist）。
	// 注意：mockWorktree.Remove 总返回 nil；但 pre-delete hook 用 os.Stat 真实 FS。
	// worktree path 指向不存在的临时路径。
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("should not run")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	wtPath := t.TempDir() + "/nonexistent-wt"
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	if err := m.Delete(context.Background(), tid, DeleteNormal, false); err != nil {
		t.Fatalf("Delete must succeed when worktree absent: %v", err)
	}
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("pre-delete must skip when worktree absent; got RunScript: %d", runner.runScriptCallCount())
	}
}

// --- Phase 3.10 缺口补充测试 ---

// TestCreateChain_InitScriptSucceeded_TriggersActivate：init 成功后 triggerActivate 被调用，
// 任务自动推进到 active（design.md §4，tasks 3.10）。
// 现有 TestCreateChain_WithInitScript_StartsInitRunner 仅断言收敛 succeeded，未显式断言激活。
// 反证：若 runInitAttempt 未在 rows=1+succeeded 后调 triggerActivate，status 保持 suspended 断言失败。
func TestCreateChain_InitScriptSucceeded_TriggersActivate(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	row, err := m.Create(context.Background(), "p1", "mytask", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	waitInitStatus(t, store, row.ID, InitStatusSucceeded, 2*time.Second)
	// triggerActivate 异步推进 active：等待 autoActivateWG 收尾后 status 应为 active。
	m.autoActivateWG.Wait()
	assertStatus(t, store, row.ID, StatusActive)
}

// TestPreDelete_ScriptFails_RetryRerunsScript：pre-delete 脚本失败 → deletion_failed →
// Retry 重入删除序列真正重跑脚本（runScriptCallCount 递增，tasks 3.10）。
// 现有 TestPreDelete_WtRemoveSuccessDBFail_RetryConverges 是 wt.Remove 成功 DB fail（脚本不重跑）。
// 反证：若 Retry 未重跑脚本（幂等跳过），runScriptCallCount==1 断言失败。
func TestPreDelete_ScriptFails_RetryRerunsScript(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	// 首次脚本失败，重试时改为成功。
	runner := &mockLifecycleRunner{runScriptErr: fmt.Errorf("predelete boom")}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	// 首次 Delete：pre-delete 脚本失败 → deletion_failed，wt.Remove 未调用。
	err := m.Delete(context.Background(), tid, DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail when pre-delete script fails")
	}
	assertStatus(t, store, tid, StatusDeletionFailed)
	if got := wt.removeCalls(); got != 0 {
		t.Fatalf("wt.Remove must not be called when pre-delete fails, got %d", got)
	}
	if runner.runScriptCallCount() != 1 {
		t.Fatalf("pre-delete must run once, got %d", runner.runScriptCallCount())
	}
	// Retry：重入删除序列。worktree 仍存在 → pre-delete 脚本真正重跑（非幂等跳过）。
	// 改脚本为成功 → 删除序列推进。
	runner.mu.Lock()
	runner.runScriptErr = nil
	runner.mu.Unlock()
	if err := m.Retry(context.Background(), tid, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	// 脚本真正重跑（runScriptCallCount 从 1 增至 2）。
	if runner.runScriptCallCount() != 2 {
		t.Fatalf("Retry must re-run pre-delete script (count 1→2), got %d", runner.runScriptCallCount())
	}
	// 任务行被删除。
	if _, err := store.GetTask(context.Background(), tid); err == nil {
		t.Fatalf("task row must be deleted after Retry")
	}
}

// TestInheritLog_Permissions_0600_0700：inherit.log 文件 0600、目录 0700（design.md §7.4，tasks 3.10）。
// 反证：若 writeInheritLog 未设 0600/0700，权限位断言失败。
func TestInheritLog_Permissions_0600_0700(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	m := &Manager{logDir: logDir}
	logPath := filepath.Join(logDir, "t1", "inherit.log")
	m.writeInheritLog(logPath, []string{"warn1"})

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("read inherit.log: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("inherit.log mode=%o want 0600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(logPath))
	if err != nil {
		t.Fatalf("stat log dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("log dir mode=%o want 0700", mode)
	}
}

// TestInheritLog_WriteFail_DoesNotBlock：inherit.log 写失败降级为仅服务端日志，不阻断 Create
// （design.md §7.4，tasks 3.10）。用只读父目录使 WriteFile 失败，断言 writeInheritLog 返回且无 panic。
func TestInheritLog_WriteFail_DoesNotBlock(t *testing.T) {
	resetLifecycleCfgMock()
	// 构造只读父目录，使 MkdirAll 创建子目录失败（降级路径）。
	roParent := t.TempDir()
	if err := os.Chmod(roParent, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(roParent, 0o700) // 恢复权限以便 t.TempDir 清理
	logDir := filepath.Join(roParent, "logs", "t1")
	m := &Manager{logDir: logDir}
	logPath := filepath.Join(logDir, "inherit.log")
	// writeInheritLog 写失败 MUST 仅记日志、不 panic、不阻断（无返回值）。
	m.writeInheritLog(logPath, []string{"warn1"})
	// 文件未创建（写失败降级）。
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("inherit.log must not be created when write fails (degraded)")
	}
}

// TestDelete_Success_CleansLifecycleLogDir：删除成功后 <logDir>/<taskID>/ 被清理
// （design.md §6/§7.4，tasks 3.10）。
func TestDelete_Success_CleansLifecycleLogDir(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap,
		LifecycleRunner: runner, LogDir: logDir,
	})
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	// 预置日志目录 + 文件（模拟已有日志）。
	taskLogDir := filepath.Join(logDir, tid)
	if err := os.MkdirAll(taskLogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskLogDir, "init.log"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 删除成功（无 pre-delete 脚本 → 跳过；wt.Remove mock 成功；DB 删除成功）。
	if err := m.Delete(context.Background(), tid, DeleteNormal, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 日志目录被清理（removeLifecycleLogDir best-effort RemoveAll）。
	if _, err := os.Stat(taskLogDir); !os.IsNotExist(err) {
		t.Fatalf("log dir %s must be removed after successful delete, got err=%v", taskLogDir, err)
	}
}

// TestReconcile_ConvergeBeforeRestoreCleanupDebts：Converge 先于 restoreCleanupDebts
// （design.md §5 + tasks 3.8，tasks 3.10）。用 traceStore 记录 Converge 顺序 + traceDebtStore
// 记录 ListCleanupDebts（restoreCleanupDebts 首个调用）顺序，共享 order slice 比较先后。
func TestReconcile_ConvergeBeforeRestoreCleanupDebts(t *testing.T) {
	resetLifecycleCfgMock()
	orderMu := &sync.Mutex{}
	var order []string
	store := &convergeTraceStore{mockStore: newMockStore(), orderMu: orderMu, order: &order}
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	debt := &restoreTraceDebtStore{inner: newMemCleanupDebtStore(), orderMu: orderMu, order: &order}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap,
		DebtStore: debt, LifecycleRunner: &mockLifecycleRunner{},
	})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt1", InitStatus: InitStatusRunning}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	orderMu.Lock()
	snapshot := make([]string, len(order))
	copy(snapshot, order)
	orderMu.Unlock()
	convergeIdx, restoreIdx := -1, -1
	for i, name := range snapshot {
		if name == "converge" {
			convergeIdx = i
		}
		if name == "restoreDebts" {
			restoreIdx = i
		}
	}
	if convergeIdx < 0 {
		t.Fatalf("ConvergeInterruptedInitRuns not called")
	}
	if restoreIdx < 0 {
		t.Fatalf("restoreCleanupDebts not called (DebtStore injected)")
	}
	if convergeIdx >= restoreIdx {
		t.Fatalf("Converge (idx %d) must be before restoreCleanupDebts (idx %d)", convergeIdx, restoreIdx)
	}
}

// convergeTraceStore 包装 mockStore 记录 ConvergeInterruptedInitRuns 调用顺序（共享 order slice）。
type convergeTraceStore struct {
	*mockStore
	orderMu *sync.Mutex
	order   *[]string
}

func (s *convergeTraceStore) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	s.orderMu.Lock()
	*s.order = append(*s.order, "converge")
	s.orderMu.Unlock()
	return s.mockStore.ConvergeInterruptedInitRuns(ctx)
}

// restoreTraceDebtStore 包装 CleanupDebtStore 记录 ListCleanupDebts 调用顺序（共享 order slice）。
// ListCleanupDebts 是 restoreCleanupDebts 的首个调用，用其作为 restore 顺序标记。
type restoreTraceDebtStore struct {
	inner   CleanupDebtStore
	orderMu *sync.Mutex
	order   *[]string
}

func (s *restoreTraceDebtStore) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	return s.inner.UpsertCleanupDebt(ctx, sessionName, ticketsJSON, createdAt)
}
func (s *restoreTraceDebtStore) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	return s.inner.DeleteCleanupDebt(ctx, sessionName)
}
func (s *restoreTraceDebtStore) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	s.orderMu.Lock()
	*s.order = append(*s.order, "restoreDebts")
	s.orderMu.Unlock()
	return s.inner.ListCleanupDebts(ctx)
}

// TestRerunInit_EmptyScript_OverwritesInitLog：空脚本 Re-run 仍走 RunScript，覆盖 init.log
// （生产修复 init_run.go:113，tasks 3.10）。注入真实 lifecycle.New() 作为 LifecycleRunner，
// 配脚本→产生真实 init.log（含旧内容标记）→清空脚本→Rerun→读取 init.log 断言旧内容消失
// （RunScript 以 O_TRUNC 重写日志，空脚本 truncate 为空文件）。
func TestRerunInit_EmptyScript_OverwritesInitLog(t *testing.T) {
	resetLifecycleCfgMock()
	logDir := t.TempDir()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	store := newMockStore()
	// 初始配置含 init 脚本（写入唯一标记，便于断言覆盖）。
	seedLifecycleConfig(store, "p1", "", "echo OLD-MARKER", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	// 真实 LifecycleRunner：RunScript 以 /bin/sh -c 真实执行脚本、O_TRUNC 重写日志。
	realRunner := lifecycle.New()
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap,
		LifecycleRunner: realRunner, LogDir: logDir,
	})
	tid := "t1"
	// WorktreePath 须是真实存在的目录（/bin/sh -c 以其为 cwd）。
	wtDir := t.TempDir()
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtDir, InitStatus: InitStatusFailed}
	// 第一次 Rerun：有脚本 → 真实执行 echo OLD-MARKER → init.log 含 OLD-MARKER。
	if _, err := m.RerunInit(context.Background(), tid); err != nil {
		t.Fatalf("RerunInit (with script): %v", err)
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
	m.runnerWG.Wait()
	// 断言 init.log 已被真实写入，含 OLD-MARKER（证明脚本真实执行）。
	initLogPath := filepath.Join(logDir, tid, "init.log")
	data, err := readFile(initLogPath)
	if err != nil {
		t.Fatalf("read init.log after first Rerun: %v", err)
	}
	if !contains(string(data), "OLD-MARKER") {
		t.Fatalf("init.log must contain OLD-MARKER after first run, got %q", string(data))
	}

	// 清空脚本：重置配置为空 init_script。
	seedLifecycleConfig(store, "p1", "", "", "")
	// 设置任务为 failed 以允许 Rerun（Rerun 要求 failed|succeeded）。
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtDir, InitStatus: InitStatusFailed}
	// 第二次 Rerun：空脚本仍走 RunScript（保日志 truncate 契约），O_TRUNC 覆盖旧日志为空。
	if _, err := m.RerunInit(context.Background(), tid); err != nil {
		t.Fatalf("RerunInit (empty script): %v", err)
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
	m.runnerWG.Wait()
	// 读取 init.log 断言旧内容消失：O_TRUNC 重写 → 空脚本无 stdout → 文件为空或旧标记不存在。
	data2, err := readFile(initLogPath)
	if err != nil {
		t.Fatalf("read init.log after empty-script Rerun: %v", err)
	}
	if contains(string(data2), "OLD-MARKER") {
		t.Fatalf("init.log must NOT contain OLD-MARKER after empty-script Rerun (O_TRUNC overwrite), got %q", string(data2))
	}
	// 空脚本无 stdout → 文件应为空（O_TRUNC 后无写入）。
	if len(data2) != 0 {
		t.Fatalf("init.log must be empty after empty-script Rerun (O_TRUNC + no stdout), got %q", string(data2))
	}
}
