package task

// add-local-path-task-mode tasks.md 5.1-5.5 行为测试（repo 项目 local-path 模式专属行为
// 与 dir+worktree 非法组合在各入口的零副作用验收）。dir 同构行为的既有覆盖在
// dir_create_test.go / dir_delete_gitops_test.go / base_branch_env_test.go，
// Reconcile/deleteResume 非法组合矩阵在 reconcile_mode_gate_test.go（不重复）。
//
// 覆盖：
//   - 5.1 创建：三类新建行持久化断言；拒绝非空 base_ref；目录消失 invalid_state 零副作用；
//     dir+mode 拒绝；未知 mode 拒绝；presence 决策表全组合；createInPlace 零副作用验收
//     （panic mock + 目录快照 + worktrees/ 目录不存在）；配置读取失败阻断创建链；
//     init 以 canonical 项目路径为 cwd（成功自动激活/失败 suspended+failed）；创建重试
//     （复用项目路径跳过 git/worktree/inherit；目录不存在保持 creation_failed；非法组合写前拒绝）。
//   - 5.2 删除：repo local-path 走 dir 序列（normal/force/retry），零触碰项目目录与 git；
//     pre_delete normal 执行（cwd=项目目录）/retry 重执行/force 跳过；非法组合首次
//     Delete/Retry 状态写前拒绝零副作用。
//   - 5.3 对齐：同 repo 两个 local-path 任务 OwnedOnly 互不认领；Activate/Suspend/
//     ReopenAttach/ensureRecovery(+FromAttach) 非法组合零副作用（Reconcile 已有 P1 gate）。
//   - 5.4 env：local-path 激活不注入分支变量（快照级断言）；非法组合 layerEnvSnapshot/
//     mergeEnvSnapshot internal error 且不持久化快照。
//   - 5.5 git 门禁：local-path status/diff/commit/push 与 diff review → invalid_input，
//     零 git 命令（真实非 git 目录对照）；dir+worktree → internal。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/application/diffreview"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- 5.1 创建 ---

// recordingCreateStore 捕获传入 CreateTask 的原始 TaskRow：mockStore.CreateTask/GetTask
// 会把空 mode 归一化为 worktree（migration 0013 DEFAULT 模拟），直接从 mock 读回会掩盖
// 生产代码漏写 Mode 的回归——持久化断言 MUST 用本替身检查原始落库值。
type recordingCreateStore struct {
	*mockStore
	mu      sync.Mutex
	created []TaskRow
}

func (s *recordingCreateStore) CreateTask(ctx context.Context, t TaskRow) error {
	s.mu.Lock()
	s.created = append(s.created, t)
	s.mu.Unlock()
	return s.mockStore.CreateTask(ctx, t)
}

func (s *recordingCreateStore) createdRows() []TaskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TaskRow(nil), s.created...)
}

// alignCaptureStore 捕获 AlignTaskSessions 收到的对齐模式（5.3 运行时入口真实链路的
// OwnedOnly 观测点：alignSessions → RunAlign → storeAlignPortsAdapter → 本方法）。
type alignCaptureStore struct {
	*mockStore
	mu    sync.Mutex
	modes []AlignMode
	tids  []string
}

func (s *alignCaptureStore) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	s.mu.Lock()
	s.modes = append(s.modes, mode)
	s.tids = append(s.tids, taskID)
	s.mu.Unlock()
	return s.mockStore.AlignTaskSessions(ctx, taskID, mode, listed, complete, notice)
}

func (s *alignCaptureStore) alignCalls() (modes []AlignMode, tids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AlignMode(nil), s.modes...), append([]string(nil), s.tids...)
}

// sseSpyOC 统计 SSE 订阅建立次数（非法组合断言「不建立 SSE」的可观测点）。
type sseSpyOC struct {
	OCClient
	mu         sync.Mutex
	subscribes int
}

func (c *sseSpyOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	c.mu.Lock()
	c.subscribes++
	c.mu.Unlock()
	return c.OCClient.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}

func (c *sseSpyOC) subscribeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscribes
}

// traceDeleteStore 统计删除链路全部写路径调用（5.2 非法组合零副作用边界断言）。
type traceDeleteStore struct {
	*mockStore
	mu               sync.Mutex
	statusWrites     int // UpdateTaskStatus + UpdateTaskStatusConditional（含 deleteResume 落账）
	beginIntents     int // BeginDeleteIntent
	deleteModeWrites int // SetTaskDeleteMode
	rowDeletes       int // DeleteTask
	sessionDeletes   int // DeleteTaskSession
}

func (s *traceDeleteStore) UpdateTaskStatus(ctx context.Context, id, status string, le sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	s.statusWrites++
	s.mu.Unlock()
	return s.mockStore.UpdateTaskStatus(ctx, id, status, le)
}

func (s *traceDeleteStore) UpdateTaskStatusConditional(ctx context.Context, id, from, to string, le sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	s.statusWrites++
	s.mu.Unlock()
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, from, to, le)
}

func (s *traceDeleteStore) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (application.TransitionResult, error) {
	s.mu.Lock()
	s.beginIntents++
	s.mu.Unlock()
	return s.mockStore.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

func (s *traceDeleteStore) SetTaskDeleteMode(ctx context.Context, id, mode string) (application.MutationResult, error) {
	s.mu.Lock()
	s.deleteModeWrites++
	s.mu.Unlock()
	return s.mockStore.SetTaskDeleteMode(ctx, id, mode)
}

func (s *traceDeleteStore) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	s.mu.Lock()
	s.rowDeletes++
	s.mu.Unlock()
	return s.mockStore.DeleteTask(ctx, id)
}

func (s *traceDeleteStore) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	s.mu.Lock()
	s.sessionDeletes++
	s.mu.Unlock()
	return s.mockStore.DeleteTaskSession(ctx, taskID, sessionID)
}

// traceDeleteProc 统计进程创建/kill 调用（零进程副作用断言）。
type traceDeleteProc struct {
	*mockProc
	mu    sync.Mutex
	kills int
	news  int
}

func (p *traceDeleteProc) NewSession(spec process.SessionSpec) error {
	p.mu.Lock()
	p.news++
	p.mu.Unlock()
	return p.mockProc.NewSession(spec)
}

func (p *traceDeleteProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	return p.mockProc.KillSession(name)
}

// traceDeleteOC 统计 oc 会话创建/删除调用（零 oc 副作用断言）。
type traceDeleteOC struct {
	OCClient
	mu      sync.Mutex
	creates int
	deletes int
}

func (c *traceDeleteOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	c.mu.Lock()
	c.creates++
	c.mu.Unlock()
	return c.OCClient.CreateSession(ctx, dir, title)
}

func (c *traceDeleteOC) DeleteSession(ctx context.Context, dir, id string) error {
	c.mu.Lock()
	c.deletes++
	c.mu.Unlock()
	return c.OCClient.DeleteSession(ctx, dir, id)
}

// seedDeleteBoundary 预置删除链路的完整副作用素材，让每个被断言的零调用「若顺序错误
// 就真实可达」：owned session、残余进程会话（runtime 带有效密码/端口 env——deleteOCSessions
// 能真正走通恢复密码→oc delete 路径）、anchor、retryable cleanup debt（retryDebtGate 实际
// 读 TaskRow.Notice）、pre_delete 配置（runner 调用非平凡）。返回预置的 Notice 原文供不变断言。
func seedDeleteBoundary(t *testing.T, store *traceDeleteStore, proc *traceDeleteProc, taskID, projectID string) sql.NullString {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTaskSession(ctx, SessionRow{TaskID: taskID, SessionID: "sess-owned", LastSeenAt: 10}); err != nil {
		t.Fatalf("seed owned session: %v", err)
	}
	proc.sessions[runtimeSessionName(taskID)] = true
	proc.sessions[tuiSessionName(taskID)] = true
	// 有效密码/端口：若删除链错误地进入 deleteOCSessions，恢复密码不会提前失败，
	// oc delete / kill / temp serve 全部真实可达（零调用断言因此非平凡）。
	proc.envValues[runtimeSessionName(taskID)] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": taskID,
	}
	// retryable cleanup debt 写入 TaskRow.Notice（delete.go retryDebtGate 的真实读取位置），
	// 而非 recovery_debts 表——非法组合下门禁若被绕过，该 debt 会被 retryDebt 消费。
	seedNotice := encodeNotices([]noticeEntry{{Code: noticeCodeResidual, Message: "residual process",
		Data: map[string]any{"retryable": true, "sessionName": runtimeSessionName(taskID), "cleanupTickets": []string{}}}})
	store.mutTask(taskID, func(r *TaskRow) {
		r.AnchorSessionID = sql.NullString{String: "sess-owned", Valid: true}
		r.Notice = seedNotice
	})
	// pre_delete 配置：若顺序错误进入 pre-delete 步骤，runner 会被真实调用。
	seedLifecycleConfig(store, projectID, "", "", "echo predelete")
	if err := store.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
		TaskID: taskID, Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "seed debt", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed recovery debt: %v", err)
	}
	return seedNotice
}

// assertDeleteBoundaryUntouched 断言删除链路零写零调用且素材原样保留
//（wantStatusWrites 为允许的状态写次数：首次 Delete/Retry 为 0，deleteResume 落账为 1；
// wantNotice 为预置的 cleanup debt JSON——retryDebtGate 不得消费）。
func assertDeleteBoundaryUntouched(t *testing.T, store *traceDeleteStore, proc *traceDeleteProc, oc *traceDeleteOC, runner *mockLifecycleRunner, taskID string, wantStatusWrites int, wantStatus string, wantNotice sql.NullString) {
	t.Helper()
	ctx := context.Background()
	if store.statusWrites != wantStatusWrites {
		t.Errorf("status writes = %d, want %d（超出部分为非法组合下的非法写）", store.statusWrites, wantStatusWrites)
	}
	if store.beginIntents != 0 {
		t.Errorf("BeginDeleteIntent calls = %d, want 0", store.beginIntents)
	}
	if store.deleteModeWrites != 0 {
		t.Errorf("SetTaskDeleteMode calls = %d, want 0", store.deleteModeWrites)
	}
	if store.rowDeletes != 0 {
		t.Errorf("DeleteTask calls = %d, want 0", store.rowDeletes)
	}
	if store.sessionDeletes != 0 {
		t.Errorf("DeleteTaskSession calls = %d, want 0（owned session 不得删除）", store.sessionDeletes)
	}
	if proc.kills != 0 {
		t.Errorf("KillSession calls = %d, want 0（残余进程/retryDebt 不得 kill）", proc.kills)
	}
	if proc.news != 0 {
		t.Errorf("NewSession calls = %d, want 0（temp serve 不得启动）", proc.news)
	}
	if oc.deletes != 0 || oc.creates != 0 {
		t.Errorf("oc create/delete calls = %d/%d, want 0/0", oc.creates, oc.deletes)
	}
	if n := runner.runScriptCallCount(); n != 0 {
		t.Errorf("pre-delete script calls = %d, want 0（已配置 pre_delete，若进入该步骤即非零）", n)
	}
	row, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("task row must survive: %v", err)
	}
	if row.Status != wantStatus {
		t.Errorf("status = %s, want unchanged %s", row.Status, wantStatus)
	}
	if row.Notice != wantNotice {
		t.Errorf("notice = %v, want unchanged %q（retryDebtGate 不得消费 cleanup debt）", row.Notice, wantNotice.String)
	}
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-owned" {
		t.Errorf("anchor = %v, want unchanged sess-owned", row.AnchorSessionID)
	}
	sessions, _ := store.ListTaskSessions(ctx, taskID)
	if len(sessions) != 1 || sessions[0].SessionID != "sess-owned" {
		t.Errorf("owned sessions = %+v, want [sess-owned] preserved", sessions)
	}
	debts, _ := store.ListRecoveryDebts(ctx)
	if len(debts) != 1 {
		t.Errorf("recovery debts = %+v, want 1 preserved", debts)
	}
}

// newGitSentinel 在 PATH 前部注入 fake git：每次 git 调用追加记录到 sentinel 日志后失败。
// 返回日志路径——断言其为空即证明零 git runner 调用（5.5：status/diff/commit/push/
// diff review 在门禁处早退，任何 git 命令/子仓库探测都会在日志留痕）。
func newGitSentinel(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git-calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OCDECK_TEST_GIT_SENTINEL\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// 预创建空日志：零调用 = 文件保持空（而非不存在），断言可区分「零调用」与「未写日志」。
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCDECK_TEST_GIT_SENTINEL", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// assertGitSentinelEmpty 断言 fake git 零调用。
func assertGitSentinelEmpty(t *testing.T, logPath string) {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git sentinel log: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("git sentinel log = %q, want empty（门禁后 MUST NOT 执行任何 git 命令/探测）", b)
	}
}

// seedLocalPathRepoTask 在 mockStore 中创建 repo 项目与 local-path 模式挂起任务，
// WorktreePath 指向真实项目目录（删除/重试的逐字节比对与 pre-delete Stat 用）。
func seedLocalPathRepoTask(s *mockStore, taskID, projectID, projDir string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "p", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: TaskModeLocalPath}
	s.tasks[taskID] = t
	return t
}

// newLocalPathCreateManager 构造 local-path 创建测试 Manager（panicNamer + dirPanicWorktree，
// 证明创建链零调用 Namer/WorktreeBackend），返回 manager 与 DataDir（断言 worktrees/ 无新增）。
func newLocalPathCreateManager(t *testing.T, store TaskStore, proc ProcessBackend, oc OCClient) (*Manager, string) {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc,
		Worktree:  wrapDirPanicWorktree(newMockWorktree()),
		OCFactory: wrap, Namer: &panicNamer{},
	})
	return m, cfg.DataDir
}

// TestCreate_LocalPathRows_Persisted 三类新建行持久化断言（tasks 5.1）：
// repo 缺省/显式 worktree → mode=worktree；repo local-path → mode=local-path + 空 branch/
// base_ref + canonical 项目路径；dir 缺省 → mode=local-path（MUST NOT 落 DB DEFAULT worktree）。
func TestCreate_LocalPathRows_Persisted(t *testing.T) {
	resetLifecycleCfgMock()
	t.Run("repo_implicit_worktree", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
		rstore := &recordingCreateStore{mockStore: store}
		m := newTestManager(t, rstore, newMockProc(), newMockWorktree(), newMockOC(true))
		row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// 原始落库值断言（mock 归一化会把空 mode 变 worktree，掩盖漏写 Mode 的回归）。
		created := rstore.createdRows()
		if len(created) != 1 {
			t.Fatalf("CreateTask calls = %d, want 1", len(created))
		}
		if created[0].Mode != TaskModeWorktree {
			t.Errorf("raw persisted mode = %q, want worktree", created[0].Mode)
		}
		row, _ = store.GetTask(context.Background(), row.ID)
		if !strings.HasPrefix(row.Branch, "ocdeck/") || row.BaseRef != "refs/heads/main" {
			t.Errorf("branch/base_ref = %q/%q, want ocdeck/* + refs/heads/main", row.Branch, row.BaseRef)
		}
	})
	t.Run("repo_explicit_worktree", func(t *testing.T) {
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
		rstore := &recordingCreateStore{mockStore: store}
		m := newTestManager(t, rstore, newMockProc(), newMockWorktree(), newMockOC(true))
		row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeWorktree})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		created := rstore.createdRows()
		if len(created) != 1 || created[0].Mode != TaskModeWorktree {
			t.Errorf("raw persisted mode = %+v, want [worktree]", created)
		}
		row, _ = store.GetTask(context.Background(), row.ID)
		if row.Mode != TaskModeWorktree {
			t.Errorf("mode = %q, want worktree", row.Mode)
		}
	})
	t.Run("repo_local_path", func(t *testing.T) {
		projDir := t.TempDir()
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
		rstore := &recordingCreateStore{mockStore: store}
		m, _ := newLocalPathCreateManager(t, rstore, newMockProc(), newMockOC(true))
		row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
		if err != nil {
			t.Fatalf("Create local-path: %v", err)
		}
		canonical, _ := filepath.EvalSymlinks(projDir)
		created := rstore.createdRows()
		if len(created) != 1 {
			t.Fatalf("CreateTask calls = %d, want 1", len(created))
		}
		raw := created[0]
		if raw.Mode != TaskModeLocalPath {
			t.Errorf("raw persisted mode = %q, want local-path", raw.Mode)
		}
		if raw.Branch != "" || raw.BaseRef != "" {
			t.Errorf("raw branch/base_ref = %q/%q, want empty", raw.Branch, raw.BaseRef)
		}
		if raw.WorktreePath != canonical {
			t.Errorf("raw worktree_path = %q, want canonical project path %q", raw.WorktreePath, canonical)
		}
		row, _ = store.GetTask(context.Background(), row.ID)
		if row.Branch != "" || row.BaseRef != "" {
			t.Errorf("branch/base_ref = %q/%q, want empty (local-path 无分支/base_ref)", row.Branch, row.BaseRef)
		}
		if row.WorktreePath != canonical {
			t.Errorf("worktree_path = %q, want canonical project path %q", row.WorktreePath, canonical)
		}
	})
	t.Run("dir_default_local_path", func(t *testing.T) {
		projDir := t.TempDir()
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
		rstore := &recordingCreateStore{mockStore: store}
		m, _ := newLocalPathCreateManager(t, rstore, newMockProc(), newMockOC(true))
		row, err := m.Create(context.Background(), "pdir", CreateTaskOptions{Name: "task"})
		if err != nil {
			t.Fatalf("Create dir: %v", err)
		}
		// 原始落库值：MUST NOT 落到 DB DEFAULT 'worktree'（否则立即成为非法组合）。
		created := rstore.createdRows()
		if len(created) != 1 || created[0].Mode != TaskModeLocalPath {
			t.Errorf("raw persisted mode = %+v, want [local-path] (dir MUST NOT fall to default worktree)", created)
		}
		row, _ = store.GetTask(context.Background(), row.ID)
		if row.Mode != TaskModeLocalPath {
			t.Errorf("mode = %q, want local-path", row.Mode)
		}
	})
}

// TestCreate_LocalPath_RejectsBaseRef repo local-path 提供非空 base_ref → invalid_input 零副作用
// （dir + base_ref 同口径由 createInPlace 入口统一拒绝）。
func TestCreate_LocalPath_RejectsBaseRef(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	m, _ := newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath, BaseRef: "feature-x"})
	if err == nil {
		t.Fatal("Create local-path with base_ref: want error, got nil")
	}
	if !isOpErrCode(err, codeInvalidInput) {
		t.Errorf("err = %v, want invalid_input", err)
	}
	if !strings.Contains(err.Error(), "base_ref is not allowed for local-path task") {
		t.Errorf("err = %v, want local-path base_ref message", err)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect, MUST NOT 落 creating 行)", len(store.tasks))
	}
}

// TestCreate_LocalPath_PathGone_InvalidStateNoRow 目录消失 → invalid_state 且不落 creating 行。
func TestCreate_LocalPath_PathGone_InvalidStateNoRow(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/nonexistent-xyz-local-path-123", DefaultBranch: "main", Kind: ProjectKindRepo})
	m, _ := newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
	if err == nil {
		t.Fatal("Create local-path with gone path: want error, got nil")
	}
	if !isOpErrCode(err, codeInvalidState) {
		t.Errorf("err = %v, want invalid_state", err)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (MUST NOT 落 creating 行)", len(store.tasks))
	}
}

// TestCreate_DirProject_ExplicitModeRejected dir 项目 + 显式 mode（任意取值）→ invalid_input 零副作用。
func TestCreate_DirProject_ExplicitModeRejected(t *testing.T) {
	resetLifecycleCfgMock()
	for _, mode := range []string{TaskModeWorktree, "bogus"} {
		projDir := t.TempDir()
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "pdir", Name: "d", Path: projDir, DefaultBranch: "", Kind: ProjectKindDir})
		m, _ := newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))

		_, err := m.Create(context.Background(), "pdir", CreateTaskOptions{Name: "task", Mode: mode})
		if err == nil {
			t.Fatalf("Create dir with explicit mode %q: want error, got nil", mode)
		}
		if !isOpErrCode(err, codeInvalidInput) {
			t.Errorf("mode %q: err = %v, want invalid_input", mode, err)
		}
		if len(store.tasks) != 0 {
			t.Errorf("mode %q: store has %d tasks, want 0", mode, len(store.tasks))
		}
	}
}

// TestCreate_Repo_UnknownModeRejected repo 项目未知 mode → invalid_input 零副作用。
func TestCreate_Repo_UnknownModeRejected(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	m, _ := newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: "bogus"})
	if err == nil {
		t.Fatal("Create repo with unknown mode: want error, got nil")
	}
	if !isOpErrCode(err, codeInvalidInput) {
		t.Errorf("err = %v, want invalid_input", err)
	}
	if !strings.Contains(err.Error(), "unknown task mode") {
		t.Errorf("err = %v, want unknown task mode message", err)
	}
	if len(store.tasks) != 0 {
		t.Errorf("store has %d tasks, want 0 (zero side effect)", len(store.tasks))
	}
}

// TestCreate_PresenceDecisionTable task-lifecycle delta「请求决策表」全组合（task 层入口）。
func TestCreate_PresenceDecisionTable(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		name    string
		kind    string
		mode    string
		baseRef string
		wantErr string // "" = 成功；否则期望 code
		wantRow string // 成功时期望落库 mode
	}{
		{"repo+缺失mode", ProjectKindRepo, "", "", "", TaskModeWorktree},
		{"repo+worktree", ProjectKindRepo, TaskModeWorktree, "", "", TaskModeWorktree},
		{"repo+local-path", ProjectKindRepo, TaskModeLocalPath, "", "", TaskModeLocalPath},
		{"repo+local-path+base_ref", ProjectKindRepo, TaskModeLocalPath, "feature", codeInvalidInput, ""},
		{"repo+未知mode", ProjectKindRepo, "bogus", "", codeInvalidInput, ""},
		{"dir+缺失mode", ProjectKindDir, "", "", "", TaskModeLocalPath},
		{"dir+local-path", ProjectKindDir, TaskModeLocalPath, "", "", TaskModeLocalPath},
		{"dir+worktree", ProjectKindDir, TaskModeWorktree, "", codeInvalidInput, ""},
		{"dir+base_ref", ProjectKindDir, "", "feature", codeInvalidInput, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projDir := t.TempDir()
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: defaultBranch, Kind: tc.kind})
			// worktree 组合需要真实 WorktreeBackend（wt.Add）；local-path 组合用 panic mock。
			var m *Manager
			if tc.wantRow == TaskModeWorktree {
				m = newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
			} else {
				m, _ = newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))
			}
			row, err := m.Create(context.Background(), "p1", CreateTaskOptions{Name: "task", Mode: tc.mode, BaseRef: tc.baseRef})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error %s, got nil", tc.wantErr)
				}
				if !isOpErrCode(err, tc.wantErr) {
					t.Fatalf("err = %v, want code %s", err, tc.wantErr)
				}
				if len(store.tasks) != 0 {
					t.Errorf("store has %d tasks, want 0 (零副作用)", len(store.tasks))
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, _ := store.GetTask(context.Background(), row.ID)
			if got.Mode != tc.wantRow {
				t.Errorf("persisted mode = %q, want %q", got.Mode, tc.wantRow)
			}
		})
	}
}

// TestCreate_LocalPath_ZeroSideEffects createInPlace 零副作用验收（tasks 5.1）：
// panic Namer/WorktreeBackend 证明 slug 生成、分支校验与探测、worktree 路径生成、worktree add、
// 全部 git 调用未发生；目录快照证明项目目录内零文件创建；DataDir/worktrees/ 无新增目录。
func TestCreate_LocalPath_ZeroSideEffects(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	m, dataDir := newLocalPathCreateManager(t, store, newMockProc(), newMockOC(true))
	// 路径生成观测：newWorktreePath 是 rand4Fn 唯一消费点（crud.go），local-path 创建
	// MUST NOT 触发 worktree 路径生成（被调用即 panic fail）。
	m.rand4Fn = func() (string, error) {
		panic("local-path create MUST NOT generate worktree path (rand4Fn called)")
	}

	row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
	if err != nil {
		t.Fatalf("Create local-path: %v", err)
	}
	_ = row
	// panic mock 未触发即证明 Namer/WorktreeBackend（含 worktree 路径生成、wt.Add、
	// 分支校验 BranchExists/ValidateBranchName）零调用。
	assertDirUnchanged(t, projDir, before)
	// 数据目录 worktrees/ 下无新增目录（project-management delta：local-path MUST NOT 建 worktree）。
	if _, err := os.Stat(filepath.Join(dataDir, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("dataDir/worktrees should not exist after local-path create, stat err = %v", err)
	}
}

// TestCreate_LocalPath_ConfigReadFailureBlocks 配置读取失败仍阻断创建链（local-path 不执行
// inherit，但 lifecycle 配置读取是唯一阻断点）→ creation_failed + last_error。
func TestCreate_LocalPath_ConfigReadFailureBlocks(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	cfgErrStore := &errLifecycleConfigStore{TaskStore: store}
	m, _ := newLocalPathCreateManager(t, cfgErrStore, newMockProc(), newMockOC(true))

	_, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
	if err == nil {
		t.Fatal("Create local-path with config read failure: want error, got nil")
	}
	if !isOpErrCode(err, codeInternal) {
		t.Errorf("err = %v, want internal", err)
	}
	rowID := store.lastTaskID()
	row, _ := store.GetTask(context.Background(), rowID)
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed", row.Status)
	}
	if !strings.Contains(row.LastError.String, "read lifecycle config") {
		t.Errorf("last_error = %q, want read lifecycle config cause", row.LastError.String)
	}
	// 创建行仍为 local-path 模式（落库模式不因失败漂移）。
	if row.Mode != TaskModeLocalPath {
		t.Errorf("mode = %q, want local-path", row.Mode)
	}
}

// TestCreate_LocalPath_InitScript_ProjectDirCwd init 以 canonical 项目路径为 cwd：
// 成功 → succeeded + 自动激活；失败 → suspended + init_status=failed 不激活；
// inherit 零调用（local-path 不执行文件继承，即使项目配置了 inherit_patterns）。
func TestCreate_LocalPath_InitScript_ProjectDirCwd(t *testing.T) {
	resetLifecycleCfgMock()
	t.Run("success_auto_activates", func(t *testing.T) {
		projDir := t.TempDir()
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
		seedLifecycleConfig(store, "prep", ".env\n", "echo init", "")
		runner := &mockLifecycleRunner{}
		m := newLifecycleTestManager(t, store, newMockProc(), wrapDirPanicWorktree(newMockWorktree()), newMockOC(true), runner)

		row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		waitForScriptCalls(t, runner, 1, 2*time.Second)
		canonical, _ := filepath.EvalSymlinks(projDir)
		if got := runner.runScriptCalls[0].dir; got != canonical {
			t.Errorf("init cwd = %q, want canonical project path %q", got, canonical)
		}
		// init 直连 layerEnvSnapshot：脚本 env 不含分支两键（5.4，与 dir 同规）。
		for _, key := range []string{"OCDECK_TASK_BASE_BRANCH", "OCDECK_TASK_HEAD_BRANCH"} {
			if v, ok := runner.runScriptCalls[0].env[key]; ok {
				t.Errorf("init env contains %s=%q, want absent", key, v)
			}
		}
		waitInitStatus(t, store, row.ID, InitStatusSucceeded, 2*time.Second)
		waitStatusAny(t, store, row.ID, 3*time.Second, StatusActivating, StatusActive)
		// local-path 不执行 inherit：CopyInherited 零调用。
		if n := runner.copyCallCount(); n != 0 {
			t.Errorf("CopyInherited calls = %d, want 0 (local-path MUST NOT inherit)", n)
		}
	})
	t.Run("failure_stays_suspended", func(t *testing.T) {
		projDir := t.TempDir()
		store := newMockStore()
		store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
		seedLifecycleConfig(store, "prep", "", "exit 1", "")
		runner := &mockLifecycleRunner{runScriptErr: errors.New("init boom")}
		m := newLifecycleTestManager(t, store, newMockProc(), wrapDirPanicWorktree(newMockWorktree()), newMockOC(true), runner)

		row, err := m.Create(context.Background(), "prep", CreateTaskOptions{Name: "task", Mode: TaskModeLocalPath})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		waitInitStatus(t, store, row.ID, InitStatusFailed, 2*time.Second)
		assertStatus(t, store, row.ID, StatusSuspended)
	})
}

// TestInitRunner_LocalPath_DirtyRow_NoBranchEnvKeys init 直连 layerEnvSnapshot 路径（不经
// Activate 入口门禁）同规验收：repo+local-path 行带脏 branch/base_ref 时 InitRunner 执行的
// 脚本 env 仍 MUST NOT 含 OCDECK_TASK_BASE_BRANCH / OCDECK_TASK_HEAD_BRANCH。
func TestInitRunner_LocalPath_DirtyRow_NoBranchEnvKeys(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "prep", Name: "task",
		Branch: "ocdeck/dirty", BaseRef: "refs/heads/dirty",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusPending, Mode: TaskModeLocalPath}
	seedLifecycleConfig(store, "prep", "", "echo init", "")
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, newMockProc(), wrapDirPanicWorktree(newMockWorktree()), newMockOC(true), runner)
	m.SetLifecycleCtx(context.Background())

	m.startInitRunner("t1")
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	for _, key := range []string{"OCDECK_TASK_BASE_BRANCH", "OCDECK_TASK_HEAD_BRANCH"} {
		if v, ok := runner.runScriptCalls[0].env[key]; ok {
			t.Errorf("init env contains %s=%q, want absent (脏数据不得注入分支键)", key, v)
		}
	}
	waitInitStatus(t, store, "t1", InitStatusSucceeded, 2*time.Second)
}

// TestRetryCreate_LocalPath_ReusesProjectPathSkipsGit 创建重试：local-path creation_failed →
// Retry 复用 canonical 项目路径并跳过 git/worktree/inherit（panic mock 证明零调用）。
func TestRetryCreate_LocalPath_ReusesProjectPathSkipsGit(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	// 与生产 createInPlace 落库一致：WorktreePath 记 canonical 路径（Retry 不重算）。
	canonicalDir, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFileTree(t, projDir)
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	t1 := TaskRow{ID: "t1", ProjectID: "prep", Name: "task", Branch: "",
		Status: StatusCreationFailed, WorktreePath: canonicalDir, BaseRef: "", Mode: TaskModeLocalPath}
	store.tasks["t1"] = t1
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, newMockProc(), wrapDirPanicWorktree(newMockWorktree()), newMockOC(true), runner)

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry local-path: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.WorktreePath != canonicalDir {
		t.Errorf("worktree_path = %q, want reused canonical project path %q", row.WorktreePath, canonicalDir)
	}
	if row.Mode != TaskModeLocalPath || row.Branch != "" || row.BaseRef != "" {
		t.Errorf("mode/branch/base_ref = %q/%q/%q, want local-path/empty/empty", row.Mode, row.Branch, row.BaseRef)
	}
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Errorf("status = %s, want committed (suspended|activating|active)", row.Status)
	}
	// panic worktree/Namer 未触发即证明 git/worktree 零调用；CopyInherited 零调用证明 inherit 跳过。
	if n := runner.copyCallCount(); n != 0 {
		t.Errorf("CopyInherited calls = %d, want 0 (retry MUST skip inherit)", n)
	}
}

// TestRetryCreate_LocalPath_DirGone_KeepsCreationFailed 重试时目录不存在 → 保持 creation_failed
// 零副作用（与 dir 任务重试语义一致）。
func TestRetryCreate_LocalPath_DirGone_KeepsCreationFailed(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	t1 := TaskRow{ID: "t1", ProjectID: "prep", Name: "task", Branch: "",
		Status: StatusCreationFailed, WorktreePath: projDir, BaseRef: "", Mode: TaskModeLocalPath}
	store.tasks["t1"] = t1
	m := newDirTestManager(t, store, newMockProc(), newMockOC(true))

	if err := os.RemoveAll(projDir); err != nil {
		t.Fatal(err)
	}
	err := m.Retry(context.Background(), "t1", false)
	if err == nil {
		t.Fatal("Retry with gone path: want error, got nil")
	}
	if !isOpErrCode(err, codeInvalidState) {
		t.Errorf("err = %v, want invalid_state", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusCreationFailed {
		t.Errorf("status = %s, want creation_failed (kept)", row.Status)
	}
	// dir 语义的 last_error（区别于旧实现 repo 路径的 empty base_ref fail-closed）。
	if !strings.Contains(row.LastError.String, "project path not accessible") {
		t.Errorf("last_error = %q, want project path not accessible", row.LastError.String)
	}
}

// TestRetryCreate_IllegalCombo_RejectedBeforeStateWrite 创建重试遇非法 kind/mode 组合：
// 在状态修改与任何副作用前拒绝（internal），状态保持 creation_failed、零进程/零 git 副作用。
func TestRetryCreate_IllegalCombo_RejectedBeforeStateWrite(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
		{"unknown kind", "weird", TaskModeWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projDir := t.TempDir()
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "",
				Status: StatusCreationFailed, WorktreePath: projDir, BaseRef: "", Mode: tc.mode}
			proc := newMockProc()
			m := newDirTestManager(t, store, proc, newMockOC(true))

			err := m.Retry(context.Background(), "t1", false)
			if err == nil {
				t.Fatal("Retry with illegal combo: want error, got nil")
			}
			if !isOpErrCode(err, codeInternal) {
				t.Errorf("err = %v, want internal", err)
			}
			row, _ := store.GetTask(context.Background(), "t1")
			if row.Status != StatusCreationFailed {
				t.Errorf("status = %s, want unchanged creation_failed (写前拒绝)", row.Status)
			}
			if len(store.statusCalls) != 0 {
				t.Errorf("status transitions = %+v, want none (零状态副作用)", store.statusCalls)
			}
			if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
				t.Errorf("new sessions = %v, want none (零进程副作用)", created)
			}
		})
	}
}

// --- 5.2 删除 ---

// TestDelete_LocalPathRepo_DirSequence repo local-path 删除走 dir 序列：
// normal 执行 pre_delete（cwd=项目目录）→ 删 DB 记录，项目目录逐字节不变；
// panic mock 证明 PreflightDelete/DirtyFiles/Remove 零调用。
func TestDelete_LocalPathRepo_DirSequence(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	store := newMockStore()
	seedLocalPathRepoTask(store, "t1", "p1", projDir)
	// 行填脏 branch/base_ref：pre-delete 直连 layerEnvSnapshot，脚本 env 仍 MUST NOT 含分支两键。
	store.mutTask("t1", func(r *TaskRow) {
		r.Branch = "ocdeck/dirty"
		r.BaseRef = "refs/heads/dirty"
	})
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	proc := newMockProc()
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wrapPanicWorktree(newMockWorktree()), newMockOC(true), runner)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete local-path normal: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be deleted")
	}
	if runner.runScriptCallCount() != 1 {
		t.Fatalf("pre-delete script calls = %d, want 1", runner.runScriptCallCount())
	}
	if got := runner.runScriptCalls[0].dir; got != projDir {
		t.Errorf("pre-delete cwd = %q, want project dir %q", got, projDir)
	}
	// pre-delete 直连 layerEnvSnapshot：脚本 env 不含分支两键（脏数据不注入，5.4）。
	for _, key := range []string{"OCDECK_TASK_BASE_BRANCH", "OCDECK_TASK_HEAD_BRANCH"} {
		if v, ok := runner.runScriptCalls[0].env[key]; ok {
			t.Errorf("pre-delete env contains %s=%q, want absent", key, v)
		}
	}
	assertDirUnchanged(t, projDir, before)
}

// TestDelete_LocalPathRepo_ForceSkipsPreDelete force 跳过 pre_delete（与 repo force 契约一致）。
func TestDelete_LocalPathRepo_ForceSkipsPreDelete(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	store := newMockStore()
	seedLocalPathRepoTask(store, "t1", "p1", projDir)
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, newMockProc(), wrapPanicWorktree(newMockWorktree()), newMockOC(true), runner)

	if err := m.Delete(context.Background(), "t1", DeleteForce, false); err != nil {
		t.Fatalf("Delete local-path force: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be deleted")
	}
	if runner.runScriptCallCount() != 0 {
		t.Errorf("pre-delete script calls = %d, want 0 (force skips pre-delete)", runner.runScriptCallCount())
	}
	assertDirUnchanged(t, projDir, before)
}

// TestDelete_LocalPathRepo_RetryRerunsPreDelete deletion_failed → Retry 重入 dir 序列：
// pre_delete 重执行、DirtyFiles 零调用（panic mock）、目录逐字节不变、任务行删除。
func TestDelete_LocalPathRepo_RetryRerunsPreDelete(t *testing.T) {
	resetLifecycleCfgMock()
	projDir := t.TempDir()
	writeFileTree(t, projDir)
	before := snapshotDir(t, projDir)
	store := newMockStore()
	seedLocalPathRepoTask(store, "t1", "p1", projDir)
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	// 模拟前次删除失败落账：deletion_failed + delete_mode=normal。
	_, _ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteNormal))
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusDeletionFailed })
	runner := &mockLifecycleRunner{}
	wt := wrapPanicDirtyWorktree(wrapPanicWorktree(newMockWorktree()))
	m := newLifecycleTestManager(t, store, newMockProc(), wt, newMockOC(true), runner)

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry local-path deletion_failed: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be deleted after retry")
	}
	if runner.runScriptCallCount() != 1 {
		t.Errorf("pre-delete script calls = %d, want 1 (retry re-executes)", runner.runScriptCallCount())
	}
	assertDirUnchanged(t, projDir, before)
}

// TestDelete_IllegalCombo_FirstDeleteAndRetry_GateBeforeStateWrite 首次 Delete 与删除 Retry
// 遇非法 kind/mode 组合（dir+worktree / repo+未知 mode / unknown kind）：状态写
// （BeginDeleteIntent/delete_mode）与一切副作用前拒绝，零写零调用（owned session、
// 残余进程、anchor、recovery debt 全部原样保留）。
func TestDelete_IllegalCombo_FirstDeleteAndRetry_GateBeforeStateWrite(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
		{"unknown kind", "weird", TaskModeWorktree},
	}
	for _, tc := range cases {
		t.Run("first_delete/"+tc.name, func(t *testing.T) {
			projDir := t.TempDir()
			writeFileTree(t, projDir)
			before := snapshotDir(t, projDir)
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: tc.mode}
			tstore := &traceDeleteStore{mockStore: store}
			proc := &traceDeleteProc{mockProc: newMockProc()}
			oc := &traceDeleteOC{OCClient: newMockOC(true)}
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, tstore, proc, wrapPanicWorktree(newMockWorktree()), oc, runner)
			seedNotice := seedDeleteBoundary(t, tstore, proc, "t1", "p1")

			err := m.Delete(context.Background(), "t1", DeleteNormal, false)
			if err == nil {
				t.Fatal("Delete with illegal combo: want error, got nil")
			}
			if !isOpErrCode(err, codeInternal) {
				t.Errorf("err = %v, want internal", err)
			}
			assertDeleteBoundaryUntouched(t, tstore, proc, oc, runner, "t1", 0, StatusSuspended, seedNotice)
			assertDirUnchanged(t, projDir, before)
		})
		t.Run("retry/"+tc.name, func(t *testing.T) {
			projDir := t.TempDir()
			writeFileTree(t, projDir)
			before := snapshotDir(t, projDir)
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Status: StatusDeletionFailed, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: tc.mode}
			_, _ = store.SetTaskDeleteMode(context.Background(), "t1", string(DeleteNormal))
			tstore := &traceDeleteStore{mockStore: store}
			proc := &traceDeleteProc{mockProc: newMockProc()}
			oc := &traceDeleteOC{OCClient: newMockOC(true)}
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, tstore, proc, wrapPanicDirtyWorktree(wrapPanicWorktree(newMockWorktree())), oc, runner)
			seedNotice := seedDeleteBoundary(t, tstore, proc, "t1", "p1")

			err := m.Retry(context.Background(), "t1", false)
			if err == nil {
				t.Fatal("Retry with illegal combo: want error, got nil")
			}
			if !isOpErrCode(err, codeInternal) {
				t.Errorf("err = %v, want internal", err)
			}
			// deletion_failed → deleting 的重入转换发生在解析之后：零状态写、零副作用。
			assertDeleteBoundaryUntouched(t, tstore, proc, oc, runner, "t1", 0, StatusDeletionFailed, seedNotice)
			row, _ := store.GetTask(context.Background(), "t1")
			if !row.DeleteMode.Valid || row.DeleteMode.String != string(DeleteNormal) {
				t.Errorf("delete_mode = %v, want unchanged normal", row.DeleteMode)
			}
			assertDirUnchanged(t, projDir, before)
		})
	}
}

// TestDeleteResume_IllegalCombo_SingleFinalizeNoSideEffects deleteResume（已提交意图重入）
// 三种非法 kind/mode 组合（dir+worktree / repo+未知 mode / unknown kind）：仅允许恰好一次
// deletion_failed + last_error 落账（P1-F2 分层），随后 MUST 终止——retryDebt（消费 Notice
// cleanup debt）/oc session/kill/pre-delete/删记录全部不发生，用户目录零触碰。
func TestDeleteResume_IllegalCombo_SingleFinalizeNoSideEffects(t *testing.T) {
	resetLifecycleCfgMock()
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
		{"unknown kind", "weird", TaskModeWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projDir := t.TempDir()
			writeFileTree(t, projDir)
			before := snapshotDir(t, projDir)
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "",
				Status: StatusDeleting, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: tc.mode}
			tstore := &traceDeleteStore{mockStore: store}
			proc := &traceDeleteProc{mockProc: newMockProc()}
			oc := &traceDeleteOC{OCClient: newMockOC(true)}
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, tstore, proc, wrapPanicWorktree(newMockWorktree()), oc, runner)
			seedNotice := seedDeleteBoundary(t, tstore, proc, "t1", "p1")

			err := m.deleteResume(context.Background(), store.tasks["t1"], DeleteNormal, nil)
			if err == nil {
				t.Fatal("deleteResume with illegal combo: want error, got nil")
			}
			if !isOpErrCode(err, codeInternal) {
				t.Errorf("err = %v, want internal", err)
			}
			// 恰好一次失败落账（deletion_failed），其余全部为零。
			assertDeleteBoundaryUntouched(t, tstore, proc, oc, runner, "t1", 1, StatusDeletionFailed, seedNotice)
			row, _ := store.GetTask(context.Background(), "t1")
			if !row.LastError.Valid || row.LastError.String == "" {
				t.Error("last_error must be persisted with the resolution failure")
			}
			assertDirUnchanged(t, projDir, before)
		})
	}
}

// --- 5.3 对齐与运行时入口 ---

// TestLocalPath_SameRepoTasks_OwnedOnlyIsolation 同 repo 项目两个 local-path 任务共享同一
// 项目目录：resolveAlignMode 解析为 OwnedOnly，互相对齐不认领对方 session、不认领无主 session。
func TestLocalPath_SameRepoTasks_OwnedOnlyIsolation(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	projDir := t.TempDir()
	store.seedProject(ProjectRow{ID: "prep", Name: "r", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["tA"] = TaskRow{ID: "tA", ProjectID: "prep", Status: StatusSuspended, WorktreePath: projDir, Mode: TaskModeLocalPath}
	store.tasks["tB"] = TaskRow{ID: "tB", ProjectID: "prep", Status: StatusSuspended, WorktreePath: projDir, Mode: TaskModeLocalPath}

	// 有效模式解析：repo+local-path → OwnedOnly（D5；mode 驱动而非 kind 驱动）。
	mode, err := resolveAlignMode(store.tasks["tA"], ProjectKindRepo)
	if err != nil {
		t.Fatalf("resolveAlignMode(repo, local-path): %v", err)
	}
	if mode != AlignModeOwnedOnly {
		t.Fatalf("align mode = %v, want AlignModeOwnedOnly", mode)
	}

	store.sessions["tA"] = []SessionRow{{TaskID: "tA", SessionID: "sess-a", LastSeenAt: 10}}
	store.sessions["tB"] = []SessionRow{{TaskID: "tB", SessionID: "sess-b", LastSeenAt: 10}}
	listed := []SessionObservation{
		{SessionID: "sess-a", UpdatedAt: 20},
		{SessionID: "sess-b", UpdatedAt: 20},
		{SessionID: "sess-foreign", UpdatedAt: 20},
	}
	if _, err := store.AlignTaskSessions(ctx, "tA", mode, listed, true, application.NoticeMutation{}); err != nil {
		t.Fatalf("tA align: %v", err)
	}
	tASessions, _ := store.ListTaskSessions(ctx, "tA")
	ownedA := map[string]bool{}
	for _, s := range tASessions {
		ownedA[s.SessionID] = true
	}
	if !ownedA["sess-a"] || ownedA["sess-b"] || ownedA["sess-foreign"] {
		t.Errorf("tA ownership after align = %v, want only sess-a (互不认领)", ownedA)
	}
	if _, err := store.AlignTaskSessions(ctx, "tB", mode, listed, true, application.NoticeMutation{}); err != nil {
		t.Fatalf("tB align: %v", err)
	}
	tBSessions, _ := store.ListTaskSessions(ctx, "tB")
	ownedB := map[string]bool{}
	for _, s := range tBSessions {
		ownedB[s.SessionID] = true
	}
	if !ownedB["sess-b"] || ownedB["sess-a"] || ownedB["sess-foreign"] {
		t.Errorf("tB ownership after align = %v, want only sess-b (互不认领)", ownedB)
	}
}

// waitAlignCall 轮询等待对齐捕获到至少 n 次调用（startSSE 内对齐为异步），返回捕获值。
func waitAlignCall(t *testing.T, capStore *alignCaptureStore, n int, timeout time.Duration) ([]AlignMode, []string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if modes, tids := capStore.alignCalls(); len(modes) >= n {
			return modes, tids
		}
		time.Sleep(5 * time.Millisecond)
	}
	modes, tids := capStore.alignCalls()
	t.Fatalf("timeout waiting for %d align calls; got modes=%v tids=%v", n, modes, tids)
	return nil, nil
}

// TestLocalPath_RuntimeEntries_AlignOwnedOnly 四个运行时入口真实链路验收（合同
// opencode-orchestration「session 归属捕获」：Activate / persist 恢复 resumeActive /
// 挂起修复 tryRepairRuntime / 自动重拉 ensureRecovery，含 attach 转发）：local-path 任务
// 实际进入对齐时收到 AlignModeOwnedOnly——mode-capturing store 在 store.AlignTaskSessions
// 观测（alignSessions → RunAlign → storeAlignPortsAdapter → capture），非仅 resolver 断言。
func TestLocalPath_RuntimeEntries_AlignOwnedOnly(t *testing.T) {
	newSUT := func(t *testing.T) (*Manager, *alignCaptureStore, *mockProc) {
		t.Helper()
		capStore := &alignCaptureStore{mockStore: newMockStore()}
		projDir := t.TempDir()
		capStore.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
		capStore.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
			Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: TaskModeLocalPath}
		proc := newMockProc()
		m := newTestManager(t, capStore, proc, newMockWorktree(), newMockOC(true))
		m.SetLifecycleCtx(context.Background())
		return m, capStore, proc
	}
	// markActiveWithSnapshot 模拟 persist 重启前的活跃任务：active + env 快照 + 存活健康
	// runtime 会话（runtimeHealthyRecoverable 的读回素材）。
	markActiveWithSnapshot := func(capStore *alignCaptureStore, proc *mockProc) {
		snap := envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}}
		snapBytes, _ := encodeEnvSnapshot(snap)
		capStore.mutTask("t1", func(r *TaskRow) {
			r.Status = StatusActive
			r.EnvSnapshot = snapBytes
		})
		proc.sessions[runtimeSessionName("t1")] = true
		proc.envValues[runtimeSessionName("t1")] = map[string]string{
			"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
		}
	}
	assertAllOwnedOnly := func(t *testing.T, modes []AlignMode, tids []string, what string) {
		t.Helper()
		for i, mode := range modes {
			if mode != AlignModeOwnedOnly || tids[i] != "t1" {
				t.Fatalf("%s align[%d] = %v/%v, want OwnedOnly/t1（local-path 不得进入目录级全量对齐）", what, i, tids[i], mode)
			}
		}
	}

	t.Run("activate", func(t *testing.T) {
		m, capStore, _ := newSUT(t)
		if err := m.Activate(context.Background(), "t1"); err != nil {
			t.Fatalf("Activate local-path: %v", err)
		}
		assertStatus(t, capStore.mockStore, "t1", StatusActive)
		modes, tids := waitAlignCall(t, capStore, 1, 3*time.Second)
		assertAllOwnedOnly(t, modes, tids, "activate")
	})
	t.Run("resume_active_via_reconcile", func(t *testing.T) {
		m, capStore, proc := newSUT(t)
		markActiveWithSnapshot(capStore, proc)
		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile persist resume: %v", err)
		}
		assertStatus(t, capStore.mockStore, "t1", StatusActive)
		modes, tids := waitAlignCall(t, capStore, 1, 3*time.Second)
		assertAllOwnedOnly(t, modes, tids, "resumeActive")
	})
	t.Run("suspend_repair_tryRepairRuntime", func(t *testing.T) {
		m, capStore, proc := newSUT(t)
		markActiveWithSnapshot(capStore, proc)
		// runtime kill 失败（会话仍存活）→ Suspend 分支 c：以 Suspend 入口解析的模式
		//（OwnedOnly）经 tryRepairRuntime 重建运行时并回 active。
		proc.killResults[runtimeSessionName("t1")] = process.KillResult{
			SessionKilled: false, Disposition: process.DispositionKillFailed}
		if err := m.Suspend(context.Background(), "t1"); err != nil {
			t.Fatalf("Suspend with repair: %v", err)
		}
		assertStatus(t, capStore.mockStore, "t1", StatusActive)
		modes, tids := waitAlignCall(t, capStore, 1, 3*time.Second)
		assertAllOwnedOnly(t, modes, tids, "tryRepairRuntime")
	})
	t.Run("ensure_recovery_watcher", func(t *testing.T) {
		m, capStore, proc := newSUT(t)
		if err := m.Activate(context.Background(), "t1"); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		waitAlignCall(t, capStore, 1, 3*time.Second) // Activate 首次对齐
		// watcher 捕获 runtime 退出 → ensureRecovery：入口解析 OwnedOnly 后重拉对齐。
		proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
		waitStatusAny(t, capStore.mockStore, "t1", 5*time.Second, StatusActive)
		modes, tids := waitAlignCall(t, capStore, 2, 5*time.Second)
		assertAllOwnedOnly(t, modes, tids, "ensureRecovery")
	})
	t.Run("attach_ensureRecoveryFromAttach", func(t *testing.T) {
		m, capStore, proc := newSUT(t)
		if err := m.Activate(context.Background(), "t1"); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		waitAlignCall(t, capStore, 1, 3*time.Second)
		// active 但 runtime 会话与注册表均消失（不经 watcher）→ attach 入口触发恢复（G4-1）。
		if _, err := proc.KillSession(runtimeSessionName("t1")); err != nil {
			t.Fatalf("kill runtime session: %v", err)
		}
		m.clearRuntime("t1")
		if _, err := m.ReopenAttach(context.Background(), "t1"); err == nil || !isOpErrCode(err, codeRecovering) {
			t.Fatalf("ReopenAttach = %v, want typed recovering", err)
		}
		waitStatusAny(t, capStore.mockStore, "t1", 5*time.Second, StatusActive)
		modes, tids := waitAlignCall(t, capStore, 2, 5*time.Second)
		assertAllOwnedOnly(t, modes, tids, "ensureRecoveryFromAttach")
	})
}

// runtimeRegistrySnapshot 返回 runtime 注册表的权威状态快照：live token + tombstone
// token + found 位（Registry.Tombstone 为代际权威：newRuntime 分配即写 tombstone，
// 且清理后保留——仅观测 live map 会漏掉「先分配令牌、不安装/清理 live runtime」的回归，
// 违反四入口零 runtime 副作用合同）。
func runtimeRegistrySnapshot(m *Manager, taskID string) string {
	live := ""
	if rt := m.getRuntime(taskID); rt != nil {
		live = string(rt.instVersion)
	}
	tomb, found := m.runtimeRegistry.Tombstone(taskID)
	return fmt.Sprintf("live=%q tombstone=%q found=%v", live, string(tomb), found)
}

// TestIllegalCombo_RuntimeEntries_FailClosed 非法 kind/mode 组合（dir+worktree /
// repo+未知 mode / unknown kind）在 Activate / Suspend / ReopenAttach / ensureRecovery /
// ensureRecoveryFromAttach 各入口：internal 错误且状态/runtime/SSE/align/anchor 均未变化
// （任何状态修改或运行时副作用前拒绝；Reconcile 在 reconcile_mode_gate_test.go）。
func TestIllegalCombo_RuntimeEntries_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
		{"unknown kind", "weird", TaskModeWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: defaultBranch, Kind: tc.kind})
			// 每个子测试断言：对齐零调用 + SSE 零订阅 + anchor 不变。
			newEntrySUT := func(t *testing.T, proc *mockProc) (*Manager, *alignCaptureStore, *sseSpyOC) {
				t.Helper()
				capStore := &alignCaptureStore{mockStore: store}
				sseOC := &sseSpyOC{OCClient: newMockOC(true)}
				m := newTestManager(t, capStore, proc, newMockWorktree(), sseOC)
				m.SetLifecycleCtx(context.Background())
				return m, capStore, sseOC
			}
			seedAnchor := func() {
				store.mutTask("t1", func(r *TaskRow) {
					r.AnchorSessionID = sql.NullString{String: "sess-anchor", Valid: true}
				})
			}
			assertAnchorUnchanged := func(what string) {
				row, _ := store.GetTask(context.Background(), "t1")
				if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-anchor" {
					t.Errorf("%s: anchor = %v, want unchanged sess-anchor", what, row.AnchorSessionID)
				}
			}
			assertNoAlignNoSSE := func(what string, capStore *alignCaptureStore, sseOC *sseSpyOC) {
				t.Helper()
				if modes, tids := capStore.alignCalls(); len(modes) != 0 {
					t.Errorf("%s: align calls = %v/%v, want none（非法组合不得进入对齐）", what, tids, modes)
				}
				if n := sseOC.subscribeCount(); n != 0 {
					t.Errorf("%s: SSE subscribes = %d, want 0（非法组合不得建立 SSE）", what, n)
				}
			}

			t.Run("activate_zero_side_effect", func(t *testing.T) {
				store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
					Status: StatusSuspended, WorktreePath: "/wt", Mode: tc.mode}
				seedAnchor()
				proc := newMockProc()
				m, capStore, sseOC := newEntrySUT(t, proc)
				regBefore := runtimeRegistrySnapshot(m, "t1")
				err := m.Activate(context.Background(), "t1")
				if err == nil || !isOpErrCode(err, codeInternal) {
					t.Fatalf("err = %v, want internal", err)
				}
				assertStatus(t, store, "t1", StatusSuspended)
				// 零状态副作用含 last_error：若激活被错误放行后走补偿，last_error 会被写入。
				if row, _ := store.GetTask(context.Background(), "t1"); row.LastError.Valid {
					t.Errorf("last_error = %q, want unset（激活不得进入补偿写）", row.LastError.String)
				}
				if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
					t.Errorf("new sessions = %v, want none", created)
				}
				assertNoAlignNoSSE("activate", capStore, sseOC)
				assertAnchorUnchanged("activate")
				if regAfter := runtimeRegistrySnapshot(m, "t1"); regAfter != regBefore {
					t.Errorf("runtime registry = %q, want unchanged %q（activate 不得注册 runtime）", regAfter, regBefore)
				}
			})
			t.Run("suspend_zero_side_effect", func(t *testing.T) {
				store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
					Status: StatusActive, WorktreePath: "/wt", Mode: tc.mode}
				seedAnchor()
				proc := newMockProc()
				proc.sessions[runtimeSessionName("t1")] = true
				proc.sessions[tuiSessionName("t1")] = true
				m, capStore, sseOC := newEntrySUT(t, proc)
				regBefore := runtimeRegistrySnapshot(m, "t1")
				err := m.Suspend(context.Background(), "t1")
				if err == nil || !isOpErrCode(err, codeInternal) {
					t.Fatalf("err = %v, want internal", err)
				}
				assertStatus(t, store, "t1", StatusActive)
				if !proc.sessions[runtimeSessionName("t1")] || !proc.sessions[tuiSessionName("t1")] {
					t.Error("sessions were killed before mode validation (must not)")
				}
				assertNoAlignNoSSE("suspend", capStore, sseOC)
				assertAnchorUnchanged("suspend")
				if regAfter := runtimeRegistrySnapshot(m, "t1"); regAfter != regBefore {
					t.Errorf("runtime registry = %q, want unchanged %q（suspend 不得注册 runtime）", regAfter, regBefore)
				}
			})
			t.Run("reopen_attach_zero_side_effect", func(t *testing.T) {
				store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
					Status: StatusActive, WorktreePath: "/wt", Mode: tc.mode}
				seedAnchor()
				proc := newMockProc()
				proc.sessions[runtimeSessionName("t1")] = true
				proc.envValues[runtimeSessionName("t1")] = map[string]string{
					"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
				}
				m, capStore, sseOC := newEntrySUT(t, proc)
				regBefore := runtimeRegistrySnapshot(m, "t1")
				tid, err := m.ReopenAttach(context.Background(), "t1")
				if err == nil || !isOpErrCode(err, codeInternal) {
					t.Fatalf("err = %v, want internal", err)
				}
				if tid != "" {
					t.Errorf("terminal id = %q, want empty (MUST NOT attach)", tid)
				}
				assertStatus(t, store, "t1", StatusActive)
				assertNoAlignNoSSE("reopen_attach", capStore, sseOC)
				assertAnchorUnchanged("reopen_attach")
				if regAfter := runtimeRegistrySnapshot(m, "t1"); regAfter != regBefore {
					t.Errorf("runtime registry = %q, want unchanged %q（attach 不得注册 runtime/新分配 trigger）", regAfter, regBefore)
				}
			})
			t.Run("ensure_recovery_zero_side_effect", func(t *testing.T) {
				store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
					Status: StatusActive, WorktreePath: "/wt", Mode: tc.mode}
				seedAnchor()
				proc := newMockProc()
				m, capStore, sseOC := newEntrySUT(t, proc)
				rt := m.newRuntime("t1")
				m.setRuntime("t1", rt)
				regBefore := runtimeRegistrySnapshot(m, "t1")
				m.ensureRecovery("t1", rt.instVersion)
				m.ensureRecoveryFromAttach("t1")
				assertStatus(t, store, "t1", StatusActive)
				if n := store.recoveryPermitCount("t1"); n != 0 {
					t.Errorf("recovery permits = %d, want 0 (零恢复副作用)", n)
				}
				// 扩展快照已含 live+tombstone+found：相等比对即覆盖「不得换代/清理/分配」。
				if regAfter := runtimeRegistrySnapshot(m, "t1"); regAfter != regBefore {
					t.Errorf("runtime registry = %s, want unchanged %s（不得换代/清理/分配）", regAfter, regBefore)
				}
				if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
					t.Errorf("new sessions = %v, want none", created)
				}
				assertNoAlignNoSSE("ensure_recovery", capStore, sseOC)
				assertAnchorUnchanged("ensure_recovery")
			})
		})
	}
}

// TestReconcile_IllegalCombo_ObservabilityFailClosed 扩充非法 Reconcile 的观测面
//（reconcile_mode_gate_test.go 的 P1 矩阵只观察状态与进程）：align 零调用（capture store）、
// SSE 零订阅（spy）、预置 anchor 不变、runtime 注册表不变。active 任务 + 存活健康 runtime
// 使 persist 矩阵的 resume 分支真实可达——若非法组合被错误放行，SSE/align 探针立即非零。
func TestReconcile_IllegalCombo_ObservabilityFailClosed(t *testing.T) {
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
		{"unknown kind", "weird", TaskModeWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Status: StatusActive, WorktreePath: "/wt", InitStatus: InitStatusNone, Mode: tc.mode}
			store.mutTask("t1", func(r *TaskRow) {
				r.AnchorSessionID = sql.NullString{String: "sess-anchor", Valid: true}
				// env 快照使 persist 矩阵的 resume 分支真实可达（缺快照会在 SSE 前提前失败，
				// SSE/align 探针退化为平凡真）。
				snap, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}})
				r.EnvSnapshot = snap
			})
			capStore := &alignCaptureStore{mockStore: store}
			sseOC := &sseSpyOC{OCClient: newMockOC(true)}
			proc := newMockProc()
			proc.sessions[runtimeSessionName("t1")] = true
			proc.envValues[runtimeSessionName("t1")] = map[string]string{
				"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
			}
			m := newTestManager(t, capStore, proc, newMockWorktree(), sseOC)
			m.SetLifecycleCtx(context.Background())

			regBefore := runtimeRegistrySnapshot(m, "t1")
			err := m.Reconcile(context.Background())
			// 探针断言先于错误码断言：非法组合被错误放行时（Reconcile 可能整体成功），
			// SSE/align/registry 探针必须先暴露违规，不被 Fatalf 短路。
			if modes, tids := capStore.alignCalls(); len(modes) != 0 {
				t.Errorf("align calls = %v/%v, want none（非法组合不得进入对齐）", tids, modes)
			}
			if n := sseOC.subscribeCount(); n != 0 {
				t.Errorf("SSE subscribes = %d, want 0（非法组合不得建立 SSE）", n)
			}
			if regAfter := runtimeRegistrySnapshot(m, "t1"); regAfter != regBefore {
				t.Errorf("runtime registry = %q, want unchanged %q", regAfter, regBefore)
			}
			row, _ := store.GetTask(context.Background(), "t1")
			if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-anchor" {
				t.Errorf("anchor = %v, want unchanged sess-anchor", row.AnchorSessionID)
			}
			if n := store.recoveryPermitCount("t1"); n != 0 {
				t.Errorf("recovery permits = %d, want 0", n)
			}
			if created := proc.newSessionNamesSnapshot(); len(created) != 0 {
				t.Errorf("new sessions = %v, want none", created)
			}
			if err == nil || !isOpErrCode(err, codeInternal) {
				t.Fatalf("Reconcile = %v, want internal", err)
			}
			assertStatus(t, store, "t1", StatusActive)
		})
	}
}

// --- 5.4 env ---

// TestActivate_LocalPath_NoBranchEnvKeys local-path 激活不注入分支变量：持久化 env 快照
// 不含 OCDECK_TASK_BASE_BRANCH / OCDECK_TASK_HEAD_BRANCH（即使行带脏数据 branch/base_ref
// 也不注入；dir 同构已在 base_branch_env_test.go 覆盖，此处为 repo+local-path 缺口）。
func TestActivate_LocalPath_NoBranchEnvKeys(t *testing.T) {
	store := newMockStore()
	projDir := t.TempDir()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: "main", Kind: ProjectKindRepo})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
		Branch: "ocdeck/dirty", BaseRef: "refs/heads/dirty",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: TaskModeLocalPath}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate local-path: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	vars, err := m.loadEnvSnapshot(row)
	if err != nil {
		t.Fatalf("loadEnvSnapshot: %v", err)
	}
	for _, key := range []string{"OCDECK_TASK_BASE_BRANCH", "OCDECK_TASK_HEAD_BRANCH"} {
		if v, ok := vars[key]; ok {
			t.Errorf("env snapshot contains %s=%q, want absent (键不存在、不注入空串)", key, v)
		}
	}
	if vars["OCDECK_TASK_ID"] != "t1" {
		t.Errorf("OCDECK_TASK_ID = %q, want t1 (基础生命周期键不受影响)", vars["OCDECK_TASK_ID"])
	}
}

// TestLayerEnvSnapshot_IllegalCombo_ErrorNoPersist 非法 kind/mode 组合进 layerEnvSnapshot /
// mergeEnvSnapshot → internal error 且不持久化新快照（不建进程由 Activate 入口零副作用覆盖）。
func TestLayerEnvSnapshot_IllegalCombo_ErrorNoPersist(t *testing.T) {
	presetSnapshot := sql.NullString{String: `{"vars":{"K":"v"}}`, Valid: true}
	cases := []struct {
		name string
		kind string
		mode string
	}{
		{"dir+worktree", ProjectKindDir, TaskModeWorktree},
		{"repo+unknown mode", ProjectKindRepo, "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			defaultBranch := ""
			if tc.kind == ProjectKindRepo {
				defaultBranch = "main"
			}
			store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", DefaultBranch: defaultBranch, Kind: tc.kind})
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
				Branch: "ocdeck/a", BaseRef: "refs/heads/main",
				Status: StatusSuspended, WorktreePath: "/wt", EnvSnapshot: presetSnapshot, Mode: tc.mode}
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

			if _, err := m.layerEnvSnapshot(context.Background(), store.tasks["t1"]); err == nil {
				t.Fatal("layerEnvSnapshot with illegal combo: want error, got nil")
			}
			if _, err := m.mergeEnvSnapshot(context.Background(), store.tasks["t1"], 50001); err == nil {
				t.Fatal("mergeEnvSnapshot with illegal combo: want error, got nil")
			}
			if got := store.tasks["t1"].EnvSnapshot; got.String != presetSnapshot.String {
				t.Errorf("env snapshot = %q, want unchanged %q (MUST NOT persist new snapshot)", got.String, presetSnapshot.String)
			}
		})
	}
}

// --- 5.5 git 门禁 ---

// TestGitOps_LocalPath_GatedBeforeAnyGitWork repo local-path 任务 status/diff/commit/push 与
// diff review → invalid_input 且携带 local-path 专属文案（区别于词法校验错误），零 git 命令/
// 文件读取/子仓库探测：
//   - PATH fake-git sentinel：任何 git 调用（含子仓库探测等隐式 git）都会留痕 → 日志必须为空；
//   - 探针传相对路径 + untracked=true：绕过词法校验（绝对路径/缺 path 在门禁前就被拒，
//     会让 invalid_input 平凡真）、绕过 ref/index git 命令，直达模式门禁；门禁放行时
//     untracked 新侧直接读工作区——0000 权限文件读取失败（internal ≠ invalid_input，可观测）；
//   - 相对目录 source：untracked 目录走 gitlink/subrepo 探测分支（directory-only），
//     门禁放行时必经 git 探测（sentinel 留痕）。
func TestGitOps_LocalPath_GatedBeforeAnyGitWork(t *testing.T) {
	sentinel := newGitSentinel(t)
	projDir := t.TempDir() // 真实非 git 目录
	// 读取探针：owner 也无读权限的文件（非 root 下任何读取尝试都失败，可观测）。
	const probeRel = "probe-secret.txt"
	probePath := filepath.Join(projDir, probeRel)
	if err := os.WriteFile(probePath, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(probePath, 0o644) })
	// 自证探针有效：0000 权限文件确实不可读（否则「读取失败可观测」不成立）。
	if _, err := os.ReadFile(probePath); err == nil {
		t.Fatal("probe file must be unreadable (0000) for the read-probe assertion to be meaningful")
	}
	// 目录探针：untracked 目录走 gitlink/subrepo 探测分支（content.go directory-only 分支）。
	const subRel = "subdir"
	if err := os.MkdirAll(filepath.Join(projDir, subRel), 0o755); err != nil {
		t.Fatal(err)
	}

	store := newMockStore()
	seedLocalPathRepoTask(store, "t1", "p1", projDir)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	const wantMsg = "task runs in local-path mode (in-place, no git worktree)"
	// 各入口独立断言（非 Fatalf）：mutation 取证时单个入口违规不截断其余入口的探针证据。
	checkEntry := func(what string, err error) {
		t.Helper()
		if !isOpErrCode(err, codeInvalidInput) || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s: err = %v, want invalid_input + local-path mode message", what, err)
		}
	}

	_, err := m.GitStatus(context.Background(), "t1")
	checkEntry("GitStatus", err)
	// GitDiff untracked 文件：门禁先于新侧工作区读取。
	_, err = m.GitDiff(context.Background(), "t1", "", probeRel, true)
	checkEntry("GitDiff(untracked file)", err)
	// GitDiff untracked 目录：门禁先于 gitlink/subrepo 探测分支。
	_, err = m.GitDiff(context.Background(), "t1", "", subRel, true)
	checkEntry("GitDiff(untracked dir)", err)
	checkEntry("GitCommit", m.GitCommit(context.Background(), "t1", "msg", nil))
	checkEntry("GitPush", m.GitPush(context.Background(), "t1"))
	// diff review 复用同一门禁（diffreview_adapters.go ReadLocked）：文件 + 目录双 source，
	// callback 计数必须为零。
	adapter := NewDiffSourcePortAdapter(m)
	srcs := []diffreview.DiffSource{
		{Path: probeRel, Untracked: true},
		{Path: subRel, Untracked: true},
	}
	callbacks := 0
	err = adapter.ReadLocked(context.Background(), "t1", srcs, func(src diffreview.DiffSource, result diffreview.DiffSourceResult, err error) error {
		callbacks++
		return nil
	})
	checkEntry("diff review ReadLocked", err)
	if callbacks != 0 {
		t.Errorf("diff review callback invoked %d times for local-path task: gate MUST reject before any source read", callbacks)
	}
	// 全部入口走完：sentinel 必须为空（零 git 命令/零子仓库探测）。
	assertGitSentinelEmpty(t, sentinel)
}

// TestGitOps_IllegalCombo_FailClosed dir+worktree 非法组合 → git 门禁 internal fail-closed。
func TestGitOps_IllegalCombo_FailClosed(t *testing.T) {
	projDir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: projDir, DefaultBranch: "main", Kind: ProjectKindDir})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "task",
		Status: StatusSuspended, WorktreePath: projDir, InitStatus: InitStatusNone, Mode: TaskModeWorktree}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if _, err := m.GitStatus(context.Background(), "t1"); !isOpErrCode(err, codeInternal) {
		t.Fatalf("GitStatus dir+worktree: err = %v, want internal", err)
	}
}
