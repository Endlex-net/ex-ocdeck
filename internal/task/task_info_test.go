// task_info_test.go 覆盖任务信息修改用例与 R1 收敛/仲裁接线（task-info-editable 2.2/2.3/2.4）：
// D1 字段矩阵、D2 git 序列与失败矩阵逐行映射、R1 三分支 HEAD 判定、五处生命周期入口仲裁、
// 启动四路径与 kill 模式 Shutdown。断言基于 mockStore 镜像语义（意图不推进 updated_at、
// Changed 仅由业务列决定）。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
)

// --- fixtures ---

const (
	renameOldBranch = "ocdeck/old"
	renameNewBranch = "ocdeck/new"
	renameWtPath    = "/wt/t1"
)

// seedRenameable 建 repo 项目 + worktree 任务（分支 ocdeck/old，HEAD 指向该分支）。
func seedRenameable(t *testing.T, store *mockStore, wt *mockWorktree, status string) TaskRow {
	t.Helper()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main", Kind: ProjectKindRepo})
	row := TaskRow{
		ID: "t1", ProjectID: "p1", Name: "Task", Branch: renameOldBranch,
		Status: status, WorktreePath: renameWtPath, BaseRef: "refs/heads/main",
		Mode: TaskModeWorktree, PermissionMode: PermissionModeAsk,
		CreatedAt: 1, UpdatedAt: 1,
	}
	store.tasks[row.ID] = row
	wt.headBranch[renameWtPath] = renameOldBranch
	return row
}

func intentJSON(t *testing.T, intent renamePendingIntent) string {
	t.Helper()
	b, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	return string(b)
}

func strPtr(s string) *string { return &s }

// setEnvSnapshot 预置任务 env 快照（Go map 值字段不可直接赋值）。
func setEnvSnapshot(store *mockStore, id, raw string) {
	tk := store.tasks[id]
	tk.EnvSnapshot = sqlNullString(raw)
	store.tasks[id] = tk
}

func snapshotJSON(t *testing.T, vars map[string]string) string {
	t.Helper()
	b, err := json.Marshal(envSnapshot{Vars: vars})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(b)
}

func readSnapshotVars(t *testing.T, raw string) map[string]string {
	t.Helper()
	var snap envSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal snapshot %q: %v", raw, err)
	}
	return snap.Vars
}

// pendingTraceStore 包装 mockStore 记录 R1/CAS 调用顺序（仲裁位置断言）。
type pendingTraceStore struct {
	*mockStore
	mu    sync.Mutex
	order []string
}

func (s *pendingTraceStore) record(ev string) {
	s.mu.Lock()
	s.order = append(s.order, ev)
	s.mu.Unlock()
}

func (s *pendingTraceStore) GetTaskRenamePending(ctx context.Context, id string) (*string, error) {
	s.record("readPending")
	return s.mockStore.GetTaskRenamePending(ctx, id)
}

func (s *pendingTraceStore) SetTaskRenamePending(ctx context.Context, id, pendingJSON string) (application.MutationResult, error) {
	s.record("setPending")
	return s.mockStore.SetTaskRenamePending(ctx, id, pendingJSON)
}

func (s *pendingTraceStore) ClearTaskRenamePending(ctx context.Context, id string) (application.MutationResult, error) {
	s.record("clearPending")
	return s.mockStore.ClearTaskRenamePending(ctx, id)
}

func (s *pendingTraceStore) CommitTaskInfoUpdate(ctx context.Context, id string, update application.TaskInfoUpdate) (application.MutationResult, error) {
	s.record("commitInfo")
	return s.mockStore.CommitTaskInfoUpdate(ctx, id, update)
}

func (s *pendingTraceStore) CasActivationIfNoRecoveryDebt(ctx context.Context, id, fromStatus, toStatus string) (application.TransitionResult, error) {
	s.record("casActivate")
	return s.mockStore.CasActivationIfNoRecoveryDebt(ctx, id, fromStatus, toStatus)
}

func (s *pendingTraceStore) ListAllTasks(ctx context.Context) ([]TaskRow, error) {
	s.record("listAll")
	return s.mockStore.ListAllTasks(ctx)
}

func (s *pendingTraceStore) traceSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// indexOf 返回事件首次出现下标（不存在返回 -1）。
func indexOf(order []string, ev string) int {
	for i, e := range order {
		if e == ev {
			return i
		}
	}
	return -1
}

func lastIndexOf(order []string, ev string) int {
	idx := -1
	for i, e := range order {
		if e == ev {
			idx = i
		}
	}
	return idx
}

// --- 2.2 UpdateTaskInfo：D1 字段矩阵 ---

func TestUpdateTaskInfo_PureNameChange_NoGitNoIntent(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("New Name")})
	if err != nil {
		t.Fatalf("UpdateTaskInfo: %v", err)
	}
	if row.Name != "New Name" {
		t.Errorf("name = %q, want New Name", row.Name)
	}
	if row.Branch != renameOldBranch {
		t.Errorf("branch = %q, want unchanged %q", row.Branch, renameOldBranch)
	}
	if row.UpdatedAt == 1 {
		t.Errorf("updated_at not advanced on real change (got %d)", row.UpdatedAt)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("pure name save must not write rename pending")
	}
	if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
		t.Errorf("pure name save must not rename branch, got %v", calls)
	}
}

func TestUpdateTaskInfo_BlankName_InvalidInputZeroSideEffect(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("   ")})
	if OpErrorCode(err) != codeInvalidInput {
		t.Fatalf("err code = %v, want invalid_input", err)
	}
	if row := store.tasks["t1"]; row.Name != "Task" || row.UpdatedAt != 1 {
		t.Errorf("zero side effect violated: row=%+v", row)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("no intent expected")
	}
}

func TestUpdateTaskInfo_AllSameValues_IdempotentNoOp(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{
		Name: strPtr("Task"), BranchSlug: strPtr("old"),
	})
	if err != nil {
		t.Fatalf("same-value save: %v", err)
	}
	if row.UpdatedAt != 1 {
		t.Errorf("same-value save must not advance updated_at (got %d)", row.UpdatedAt)
	}
	if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
		t.Errorf("same-value slug must skip branch path, got %v", calls)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("no intent expected")
	}
}

func TestUpdateTaskInfo_BranchRename_Suspended_HappyPath(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
	if err != nil {
		t.Fatalf("branch rename: %v", err)
	}
	if row.Branch != renameNewBranch {
		t.Errorf("branch = %q, want %q", row.Branch, renameNewBranch)
	}
	calls := wt.renameCallsSnapshot()
	if len(calls) != 1 || calls[0] != renameOldBranch+"->"+renameNewBranch {
		t.Errorf("rename calls = %v, want [%s->%s]", calls, renameOldBranch, renameNewBranch)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("intent must be cleared by the same commit")
	}
	// HEAD 跟随（mock 模拟 git 行为）。
	if head := wt.headBranch[renameWtPath]; head != renameNewBranch {
		t.Errorf("mock HEAD = %q, want %q", head, renameNewBranch)
	}
}

func TestUpdateTaskInfo_SlugSameValue_SkipsGateAndGitPath(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	// 过渡状态：slug 同值时按纯保存处理，不触发状态门禁与 git 路径（D1 矩阵）。
	seedRenameable(t, store, wt, StatusCreating)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("old")})
	if err != nil {
		t.Fatalf("same-value slug on creating task: %v", err)
	}
	if row.Branch != renameOldBranch {
		t.Errorf("branch = %q, want unchanged", row.Branch)
	}
	if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
		t.Errorf("same-value slug must not enter git path, got %v", calls)
	}
}

func TestUpdateTaskInfo_BranchRename_GateMatrix(t *testing.T) {
	cases := []struct {
		name   string
		status string
		branch string // "" = gitless
		want   string
	}{
		{"creating", StatusCreating, renameOldBranch, codeInvalidState},
		{"creation_failed", StatusCreationFailed, renameOldBranch, codeInvalidState},
		{"deleting", StatusDeleting, renameOldBranch, codeInvalidState},
		{"gitless dir task", StatusArchived, "", codeInvalidState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			wt := newMockWorktree()
			row := seedRenameable(t, store, wt, tc.status)
			row.Branch = tc.branch
			store.tasks["t1"] = row
			wt.headBranch[renameWtPath] = tc.branch
			m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

			_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
			if OpErrorCode(err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if row := store.tasks["t1"]; row.Branch != tc.branch || row.UpdatedAt != 1 {
				t.Errorf("zero side effect violated: row=%+v", row)
			}
			if _, ok := store.pendingOf("t1"); ok {
				t.Errorf("gate rejection must not write intent")
			}
			if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
				t.Errorf("gate rejection must not rename, got %v", calls)
			}
		})
	}
}

func TestUpdateTaskInfo_SlugInvalidName_InvalidInput(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	wt.validateErr = errors.New("check-ref-format rejected")
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("bad..name")})
	if OpErrorCode(err) != codeInvalidInput {
		t.Fatalf("err = %v, want invalid_input", err)
	}
	if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
		t.Errorf("zero side effect violated: row=%+v", row)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("no intent expected before git path")
	}
}

func TestUpdateTaskInfo_SlugConflict_Conflict(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	wt.branches[renameNewBranch] = true
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
		t.Errorf("zero side effect violated: row=%+v", row)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("no intent expected")
	}
}

func TestUpdateTaskInfo_HeadMismatch_InvalidState_BeforeIntent(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	wt.headBranch[renameWtPath] = "other-branch" // HEAD 身份不匹配
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("err = %v, want invalid_state", err)
	}
	if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
		t.Errorf("zero side effect violated: row=%+v", row)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("identity mismatch must reject before intent write")
	}
	if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
		t.Errorf("identity mismatch must not rename, got %v", calls)
	}
}

// TestUpdateTaskInfo_ActiveSnapshotMatrix 覆盖 D3 快照矩阵的 active 分支（改写/缺失/损坏/仅名称不补键）。
func TestUpdateTaskInfo_ActiveSnapshotMatrix(t *testing.T) {
	t.Run("active valid snapshot rewritten with business commit", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusActive)
		setEnvSnapshot(store, "t1", snapshotJSON(t, map[string]string{
			"OCDECK_TASK_NAME":        "Task",
			"OCDECK_TASK_HEAD_BRANCH": renameOldBranch,
			"OTHER":                   "keep",
		}))
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		if _, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{
			Name: strPtr("Renamed"), BranchSlug: strPtr("new"),
		}); err != nil {
			t.Fatalf("UpdateTaskInfo: %v", err)
		}
		vars := readSnapshotVars(t, store.tasks["t1"].EnvSnapshot.String)
		if vars["OCDECK_TASK_NAME"] != "Renamed" || vars["OCDECK_TASK_HEAD_BRANCH"] != renameNewBranch {
			t.Errorf("snapshot lifecycle keys = %v", vars)
		}
		if vars["OTHER"] != "keep" {
			t.Errorf("other snapshot keys must be preserved, got %v", vars)
		}
	})

	t.Run("active missing snapshot internal zero side effect", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusActive)
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("Renamed")})
		if OpErrorCode(err) != codeInternal {
			t.Fatalf("err = %v, want internal", err)
		}
		if row := store.tasks["t1"]; row.Name != "Task" || row.EnvSnapshot.Valid {
			t.Errorf("zero side effect violated: row=%+v", row)
		}
	})

	t.Run("active corrupted snapshot internal zero side effect", func(t *testing.T) {
		for name, raw := range map[string]string{
			"invalid json": "not-json",
			"vars missing": `{"vars":null}`,
		} {
			t.Run(name, func(t *testing.T) {
				store := newMockStore()
				wt := newMockWorktree()
				seedRenameable(t, store, wt, StatusActive)
				setEnvSnapshot(store, "t1", raw)
				m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

				if _, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("Renamed")}); OpErrorCode(err) != codeInternal {
					t.Fatalf("err = %v, want internal", err)
				}
				if row := store.tasks["t1"]; row.Name != "Task" {
					t.Errorf("zero side effect violated: name=%q", row.Name)
				}
				if _, ok := store.pendingOf("t1"); ok {
					t.Errorf("corrupted snapshot must reject before intent write")
				}
			})
		}
	})

	t.Run("active name-only change does not add branch key", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusActive)
		setEnvSnapshot(store, "t1", snapshotJSON(t, map[string]string{
			"OCDECK_TASK_NAME": "Task",
		}))
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		if _, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("Renamed")}); err != nil {
			t.Fatalf("UpdateTaskInfo: %v", err)
		}
		vars := readSnapshotVars(t, store.tasks["t1"].EnvSnapshot.String)
		if vars["OCDECK_TASK_NAME"] != "Renamed" {
			t.Errorf("OCDECK_TASK_NAME = %q, want Renamed", vars["OCDECK_TASK_NAME"])
		}
		if _, ok := vars["OCDECK_TASK_HEAD_BRANCH"]; ok {
			t.Errorf("name-only change must not add branch key, got %v", vars)
		}
	})

	t.Run("suspended snapshot untouched business only", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		setEnvSnapshot(store, "t1", snapshotJSON(t, map[string]string{
			"OCDECK_TASK_NAME": "Task",
		}))
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		if _, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("Renamed")}); err != nil {
			t.Fatalf("UpdateTaskInfo: %v", err)
		}
		vars := readSnapshotVars(t, store.tasks["t1"].EnvSnapshot.String)
		if vars["OCDECK_TASK_NAME"] != "Task" {
			t.Errorf("non-active snapshot must stay untouched, got %v", vars)
		}
	})
}

// TestUpdateTaskInfo_PendingConvergedBeforeThisSave 覆盖两阶段语义：历史意图收敛成功后
// 重读当前值，本次修改以收敛后状态为基准。
func TestUpdateTaskInfo_PendingConvergedBeforeThisSave(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	// 中断现场：git 已改名（HEAD/分支均新名），DB 未提交，意图在案（含名称目标 B）。
	wt.headBranch[renameWtPath] = renameNewBranch
	wt.branches[renameNewBranch] = true
	delete(wt.branches, renameOldBranch)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		Name: strPtr("B"), BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("C")})
	if err != nil {
		t.Fatalf("UpdateTaskInfo with pending: %v", err)
	}
	// 阶段一收敛：B + ocdeck/new 落账、意图清除；阶段二以收敛后状态为基准应用 C。
	if row.Name != "C" {
		t.Errorf("name = %q, want C (this save applied after converge)", row.Name)
	}
	if row.Branch != renameNewBranch {
		t.Errorf("branch = %q, want %q (converged value preserved)", row.Branch, renameNewBranch)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("intent must be cleared by R1 replay commit")
	}
}

func TestUpdateTaskInfo_PendingUnconvergeable_ConflictNotExecuted(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	delete(wt.headBranch, renameWtPath) // worktree 缺失 → 无法判定
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{Name: strPtr("C")})
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if row := store.tasks["t1"]; row.Name != "Task" {
		t.Errorf("this save must not execute, name=%q", row.Name)
	}
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("intent must be preserved")
	}
}

// TestUpdateTaskInfo_NestedSlugReplayNotIdempotent 记录已知限制：slug 含 / 时原样重发
// 同一请求按当前分支前缀重新构造目标（嵌套），不被视为同值保存（服务端无幂等特判）。
func TestUpdateTaskInfo_NestedSlugReplayNotIdempotent(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	// 第一次：ocdeck/old + feature/X → ocdeck/feature/X。
	row, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("feature/X")})
	if err != nil {
		t.Fatalf("first rename: %v", err)
	}
	if row.Branch != "ocdeck/feature/X" {
		t.Fatalf("branch = %q, want ocdeck/feature/X", row.Branch)
	}
	// 原样重放：前缀取当前分支最后一个 / 之前部分 → ocdeck/feature/feature/X（非同值，再次改名）。
	row, err = m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("feature/X")})
	if err != nil {
		t.Fatalf("replay rename: %v", err)
	}
	if row.Branch != "ocdeck/feature/feature/X" {
		t.Errorf("replay branch = %q, want ocdeck/feature/feature/X (known limitation: replay not idempotent)", row.Branch)
	}
}

// --- 2.4 失败矩阵（持久化 + git 侧逐行映射） ---

func TestUpdateTaskInfo_FailureMatrix(t *testing.T) {
	t.Run("intent write fails: internal, zero git/db change", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.setRenamePendingErr = errors.New("disk full")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeInternal {
			t.Fatalf("err = %v, want internal", err)
		}
		if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
			t.Errorf("business columns must stay unchanged: row=%+v", row)
		}
		if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
			t.Errorf("git must not run before intent write, got %v", calls)
		}
		if head := wt.headBranch[renameWtPath]; head != renameOldBranch {
			t.Errorf("git side must stay unchanged, head=%q", head)
		}
	})

	t.Run("repo lock unavailable before intent: zero side effect", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 取消：AcquireRepoLock 在意图写入前失败
		_, err := m.UpdateTaskInfo(ctx, "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("no intent expected")
		}
		if calls := wt.renameCallsSnapshot(); len(calls) != 0 {
			t.Errorf("git must not run, got %v", calls)
		}
		if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
			t.Errorf("zero side effect violated: row=%+v", row)
		}
	})

	t.Run("git explicit failure head still old: clears intent (确定未生效)", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		wt.renameErr = errors.New("git branch -m failed")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("intent must be cleared when git definitively not applied")
		}
		if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
			t.Errorf("business must stay unchanged: row=%+v", row)
		}
	})

	t.Run("git explicit failure head old but clear fails: intent preserved (待恢复)", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		wt.renameErr = errors.New("git branch -m failed")
		store.clearRenamePendingErr = errors.New("db busy")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved when clear fails")
		}
	})

	t.Run("git outcome unknown (head already new): intent preserved (待恢复)", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		// 模拟「git 实际已生效但返回错误」：RenameBranch 报错且效果已应用（HEAD 已在新名）。
		wt.renameAppliedErrFor = map[string]error{renameOldBranch + "->" + renameNewBranch: errors.New("killed mid-command")}
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved on unknown outcome")
		}
		if row := store.tasks["t1"]; row.Branch != renameOldBranch {
			t.Errorf("db branch must stay old until R1 converges, got %q", row.Branch)
		}
	})

	t.Run("db commit fails, compensate success: intent cleared (确定未生效)", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.commitInfoErr = errors.New("db locked")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		calls := wt.renameCallsSnapshot()
		if len(calls) != 2 || calls[1] != renameNewBranch+"->"+renameOldBranch {
			t.Fatalf("rename calls = %v, want rename + compensate back", calls)
		}
		if head := wt.headBranch[renameWtPath]; head != renameOldBranch {
			t.Errorf("compensate must revert git side, head=%q", head)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("intent must be cleared after successful compensation")
		}
		if row := store.tasks["t1"]; row.Branch != renameOldBranch || row.UpdatedAt != 1 {
			t.Errorf("business must stay unchanged: row=%+v", row)
		}
	})

	t.Run("db commit fails, compensate fails: intent preserved (待恢复)", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.commitInfoErr = errors.New("db locked")
		wt.renameErrFor = map[string]error{renameNewBranch + "->" + renameOldBranch: errors.New("compete rename-back failed")}
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		_, err := m.UpdateTaskInfo(context.Background(), "t1", UpdateTaskInfoOptions{BranchSlug: strPtr("new")})
		if OpErrorCode(err) != codeGitError {
			t.Fatalf("err = %v, want git_error", err)
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved when compensation fails")
		}
		if head := wt.headBranch[renameWtPath]; head != renameNewBranch {
			t.Errorf("git side stays new (pending recovery), head=%q", head)
		}
	})
}

// --- 2.3 R1 三分支判定 + 稳定 ID ---

func TestConvergeRenamePending_ThreeHeadBranches(t *testing.T) {
	intent := renamePendingIntent{
		Name: strPtr("B"), BranchOld: renameOldBranch, BranchNew: renameNewBranch,
		EnvSnapshot: strPtr(`{"vars":{"OCDECK_TASK_NAME":"B"}}`),
	}

	t.Run("head already new: replay whole commit via intent and clear", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intentJSON(t, intent))
		wt.headBranch[renameWtPath] = renameNewBranch
		wt.branches[renameNewBranch] = true
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		row, _ := store.GetTask(context.Background(), "t1")
		proj, _ := store.GetProject(context.Background(), "p1")
		pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
		if err := m.convergeRenamePending(context.Background(), row, proj, *pending); err != nil {
			t.Fatalf("converge: %v", err)
		}
		got := store.tasks["t1"]
		if got.Name != "B" || got.Branch != renameNewBranch {
			t.Errorf("replayed commit name=%q branch=%q, want B/%s", got.Name, got.Branch, renameNewBranch)
		}
		if !got.EnvSnapshot.Valid || got.EnvSnapshot.String != *intent.EnvSnapshot {
			t.Errorf("env snapshot replay = %+v, want %s", got.EnvSnapshot, *intent.EnvSnapshot)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("intent must be cleared by replay commit")
		}
	})

	t.Run("head still old: clear intent, save treated as not applied", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intentJSON(t, intent))
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		row, _ := store.GetTask(context.Background(), "t1")
		proj, _ := store.GetProject(context.Background(), "p1")
		pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
		if err := m.convergeRenamePending(context.Background(), row, proj, *pending); err != nil {
			t.Fatalf("converge: %v", err)
		}
		got := store.tasks["t1"]
		if got.Name != "Task" || got.Branch != renameOldBranch {
			t.Errorf("business must stay unchanged: name=%q branch=%q", got.Name, got.Branch)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("intent must be cleared")
		}
	})

	t.Run("head indeterminable: keep intent with explicit error", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intentJSON(t, intent))
		delete(wt.headBranch, renameWtPath)
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		row, _ := store.GetTask(context.Background(), "t1")
		proj, _ := store.GetProject(context.Background(), "p1")
		pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
		err := m.convergeRenamePending(context.Background(), row, proj, *pending)
		if err == nil {
			t.Fatal("converge with missing worktree: want error, got nil")
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved")
		}
	})

	t.Run("replay commit fails: intent preserved", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intentJSON(t, intent))
		wt.headBranch[renameWtPath] = renameNewBranch
		store.commitInfoErr = errors.New("db locked")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		row, _ := store.GetTask(context.Background(), "t1")
		proj, _ := store.GetProject(context.Background(), "p1")
		pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
		if err := m.convergeRenamePending(context.Background(), row, proj, *pending); err == nil {
			t.Fatal("replay commit failure: want error, got nil")
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved when replay commit fails")
		}
	})

	t.Run("clear intent fails: intent preserved", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intentJSON(t, intent))
		store.clearRenamePendingErr = errors.New("db busy")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		row, _ := store.GetTask(context.Background(), "t1")
		proj, _ := store.GetProject(context.Background(), "p1")
		pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
		if err := m.convergeRenamePending(context.Background(), row, proj, *pending); err == nil {
			t.Fatal("clear intent failure: want error, got nil")
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved when clear fails")
		}
	})
}

// TestConvergeRenamePending_ReplayCommitTitleSyncSeam 验证恢复补提交（branch_new 路径）与
// 正常路径同规进入 D3 统一后处理：仅名称实际变更（intent.Name != nil）时调用标题同步 seam，
// 且传入重读后的最新任务名称；仅分支补提交（intent.Name == nil）不调用。
func TestConvergeRenamePending_ReplayCommitTitleSyncSeam(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withName bool
		wantCall bool
	}{
		{"intent carries name: seam called with re-read name", true, true},
		{"branch-only intent: seam not called", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			wt := newMockWorktree()
			seedRenameable(t, store, wt, StatusSuspended)
			intent := renamePendingIntent{BranchOld: renameOldBranch, BranchNew: renameNewBranch}
			if tc.withName {
				intent.Name = strPtr("B")
			}
			store.seedRenamePending("t1", intentJSON(t, intent))
			wt.headBranch[renameWtPath] = renameNewBranch // git 已是新名（补提交路径）
			wt.branches[renameNewBranch] = true
			m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
			var calls []string
			m.titleSync = func(ctx context.Context, taskID, title string) {
				calls = append(calls, taskID+"|"+title)
			}

			row, _ := store.GetTask(context.Background(), "t1")
			proj, _ := store.GetProject(context.Background(), "p1")
			pending, _ := store.GetTaskRenamePending(context.Background(), "t1")
			if err := m.convergeRenamePending(context.Background(), row, proj, *pending); err != nil {
				t.Fatalf("converge: %v", err)
			}
			if tc.wantCall {
				if len(calls) != 1 || calls[0] != "t1|B" {
					t.Fatalf("title sync calls = %v, want [t1|B] (re-read latest name after replay commit)", calls)
				}
			} else if len(calls) != 0 {
				t.Fatalf("title sync calls = %v, want none (branch-only replay)", calls)
			}
		})
	}
}

// TestConvergeRenamePending_StableIDPerTask 验证意图按稳定任务 ID 定位：两个任务各自独立收敛。
func TestConvergeRenamePending_StableIDPerTask(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	// t2：另一任务，意图 old2→new2，HEAD 已是 new2（补提交路径）。
	store.tasks["t2"] = TaskRow{
		ID: "t2", ProjectID: "p1", Name: "Task2", Branch: "ocdeck/old2",
		Status: StatusSuspended, WorktreePath: "/wt/t2", BaseRef: "refs/heads/main",
		Mode: TaskModeWorktree, PermissionMode: PermissionModeAsk,
	}
	wt.headBranch["/wt/t2"] = "ocdeck/new2"
	store.seedRenamePending("t2", intentJSON(t, renamePendingIntent{
		BranchOld: "ocdeck/old2", BranchNew: "ocdeck/new2",
	}))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("t1 intent must be converged (head still old → cleared)")
	}
	if _, ok := store.pendingOf("t2"); ok {
		t.Errorf("t2 intent must be converged (head already new → replay commit)")
	}
	if got := store.tasks["t2"]; got.Branch != "ocdeck/new2" {
		t.Errorf("t2 branch = %q, want ocdeck/new2 (replayed)", got.Branch)
	}
	if got := store.tasks["t1"]; got.Branch != renameOldBranch {
		t.Errorf("t1 branch must stay old (git never happened), got %q", got.Branch)
	}
}

// --- 2.3 仲裁表：五处入口 ---

func TestArbitration_Suspend_PendingConvergesThenProceeds(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusActive)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend with convergable pending: %v", err)
	}
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("pending must be converged before suspending")
	}
	if got := store.tasks["t1"]; got.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (suspend proceeded after converge)", got.Status)
	}
}

func TestArbitration_Suspend_PendingUnconvergeable_ConflictStateAndSnapshotKept(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusActive)
	snap := snapshotJSON(t, map[string]string{"OCDECK_TASK_NAME": "Task"})
	setEnvSnapshot(store, "t1", snap)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	delete(wt.headBranch, renameWtPath) // 无法判定
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	got := store.tasks["t1"]
	if got.Status != StatusActive {
		t.Errorf("status = %s, want active (refused before active→suspending)", got.Status)
	}
	if !got.EnvSnapshot.Valid || got.EnvSnapshot.String != snap {
		t.Errorf("env snapshot must be preserved (not cleared), got %+v", got.EnvSnapshot)
	}
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("intent must be preserved")
	}
}

func TestArbitration_Activate_Unconvergeable_StaysSuspendedLastErrorNoCompensation(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	snap := snapshotJSON(t, map[string]string{"OCDECK_TASK_NAME": "Task"})
	setEnvSnapshot(store, "t1", snap)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	delete(wt.headBranch, renameWtPath)
	trace := &pendingTraceStore{mockStore: store}
	m := newTestManager(t, trace, newMockProc(), wt, newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	got := store.tasks["t1"]
	if got.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (kept)", got.Status)
	}
	if !got.LastError.Valid || !strings.Contains(got.LastError.String, "rename pending") {
		t.Errorf("last_error must record converge failure, got %+v", got.LastError)
	}
	if !got.EnvSnapshot.Valid || got.EnvSnapshot.String != snap {
		t.Errorf("env snapshot must be preserved (MUST NOT enter snapshot-clearing compensation), got %+v", got.EnvSnapshot)
	}
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("intent must be preserved")
	}
	// R1 位于 CAS 之前：不可收敛时 MUST NOT 到达状态 CAS。
	if indexOf(trace.traceSnapshot(), "casActivate") >= 0 {
		t.Errorf("CAS must not run when pending not converged, order=%v", trace.traceSnapshot())
	}
}

func TestArbitration_Activate_PendingConvergesBeforeCAS(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	trace := &pendingTraceStore{mockStore: store}
	m := newTestManager(t, trace, newMockProc(), wt, newMockOC(true))

	_ = m.Activate(context.Background(), "t1") // 激活后续结果不作为断言对象（mock 环境），只断言 R1 位置
	if _, ok := store.pendingOf("t1"); ok {
		t.Errorf("pending must be converged (head still old → cleared) before CAS")
	}
	order := trace.traceSnapshot()
	clearIdx := indexOf(order, "clearPending")
	casIdx := indexOf(order, "casActivate")
	if clearIdx < 0 || casIdx < 0 || clearIdx > casIdx {
		t.Errorf("R1 must run before activation CAS, order=%v", order)
	}
}

func TestArbitration_Recovery_Unconvergeable_SkipWithLastErrorBeforeCAS(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusActive)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	delete(wt.headBranch, renameWtPath)
	trace := &pendingTraceStore{mockStore: store}
	m := newTestManager(t, trace, newMockProc(), wt, newMockOC(true))

	// 注册匹配 token 的 runtime 后同步触发运行期恢复。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	m.ensureRecovery("t1", rt.instVersion)

	got := store.tasks["t1"]
	if got.Status != StatusActive {
		t.Errorf("status = %s, want active (recovery skipped, no state write)", got.Status)
	}
	if !got.LastError.Valid || !strings.Contains(got.LastError.String, "rename pending") {
		t.Errorf("last_error must record skip reason, got %+v", got.LastError)
	}
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("intent must be preserved")
	}
	if indexOf(trace.traceSnapshot(), "casActivate") >= 0 {
		t.Errorf("CAS must not run when pending not converged, order=%v", trace.traceSnapshot())
	}
}

// --- 启动 reconcile：四种路径 ---

func TestStartupReconcile_ConvergePaths(t *testing.T) {
	intent := intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	})
	t.Run("success head old: cleared, task list re-read before lifecycle recovery", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intent)
		trace := &pendingTraceStore{mockStore: store}
		m := newTestManager(t, trace, newMockProc(), wt, newMockOC(true))

		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if _, ok := store.pendingOf("t1"); ok {
			t.Errorf("intent must be converged at startup")
		}
		order := trace.traceSnapshot()
		clearIdx := lastIndexOf(order, "clearPending")
		secondList := -1
		listCount := 0
		for i, e := range order {
			if e == "listAll" {
				listCount++
				if listCount == 2 {
					secondList = i
				}
			}
		}
		if clearIdx < 0 || secondList < 0 || clearIdx > secondList {
			t.Errorf("task list must be re-read after converge commits (clear at %d, second listAll at %d), order=%v", clearIdx, secondList, order)
		}
	})

	t.Run("head indeterminable: fail-closed, no lifecycle recovery", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusActive)
		store.seedRenamePending("t1", intent)
		delete(wt.headBranch, renameWtPath)
		trace := &pendingTraceStore{mockStore: store}
		m := newTestManager(t, trace, newMockProc(), wt, newMockOC(true))

		err := m.Reconcile(context.Background())
		if err == nil {
			t.Fatal("Reconcile with indeterminable HEAD: want error (fail-closed), got nil")
		}
		if !strings.Contains(err.Error(), "rename pending") {
			t.Errorf("error must point at rename pending: %v", err)
		}
		// 不进入后续生命周期恢复/清理：active 任务未发生任何状态写入。
		if len(trace.statusCalls) != 0 {
			t.Errorf("no status writes allowed after converge failure, got %v", trace.statusCalls)
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved")
		}
	})

	t.Run("replay commit fails: fail-closed, intent preserved", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intent)
		wt.headBranch[renameWtPath] = renameNewBranch
		store.commitInfoErr = errors.New("db locked")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		if err := m.Reconcile(context.Background()); err == nil {
			t.Fatal("Reconcile with replay commit failure: want error, got nil")
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved")
		}
	})

	t.Run("clear intent fails: fail-closed, intent preserved", func(t *testing.T) {
		store := newMockStore()
		wt := newMockWorktree()
		seedRenameable(t, store, wt, StatusSuspended)
		store.seedRenamePending("t1", intent)
		store.clearRenamePendingErr = errors.New("db busy")
		m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

		if err := m.Reconcile(context.Background()); err == nil {
			t.Fatal("Reconcile with clear intent failure: want error, got nil")
		}
		if _, ok := store.pendingOf("t1"); !ok {
			t.Errorf("intent must be preserved")
		}
	})
}

// --- kill 模式 Shutdown：R1 失败保留现场，进程终止与 join 照常，返回非 nil ---

func TestShutdownKillMode_PendingConvergeFail_ProcessesStillKilledJoinStillRuns(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusActive)
	snap := snapshotJSON(t, map[string]string{"OCDECK_TASK_NAME": "Task"})
	setEnvSnapshot(store, "t1", snap)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	delete(wt.headBranch, renameWtPath) // R1 不可收敛
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	// 进程 fixture：存活 runtime 会话；join fixture：watch cancel 标志（stopAndJoinAllRuntimes 触发）。
	serveName := runtimeSessionName("t1")
	if err := m.proc.NewSession(newSessionSpec(serveName, renameWtPath, nil, []string{"sleep"})); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, serveName)
	var joinRan atomic.Bool
	rt.mu.Lock()
	rt.watchCancels[serveName] = func() { joinRan.Store(true) }
	rt.mu.Unlock()

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown must return non-nil when R1 not converged (watchdog contract), got nil")
	}
	if !strings.Contains(err.Error(), "rename pending") {
		t.Errorf("Shutdown error must aggregate R1 failure, got %v", err)
	}
	// 进程终止照常执行。
	if alive, _ := m.proc.HasSession(serveName); alive {
		t.Errorf("process must still be killed despite R1 failure")
	}
	// goroutine join 照常执行。
	if !joinRan.Load() {
		t.Errorf("runtime join (watch cancel) must still run")
	}
	// pending / 状态 / 快照保留不变。
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("intent must be preserved")
	}
	got := store.tasks["t1"]
	if got.Status != StatusActive {
		t.Errorf("status = %s, want active (preserved)", got.Status)
	}
	if !got.EnvSnapshot.Valid || got.EnvSnapshot.String != snap {
		t.Errorf("env snapshot must be preserved, got %+v", got.EnvSnapshot)
	}
}

// --- 仲裁表：任务删除 conflict 拒绝 ---

func TestArbitration_Delete_PendingConflict(t *testing.T) {
	store := newMockStore()
	wt := newMockWorktree()
	seedRenameable(t, store, wt, StatusSuspended)
	store.seedRenamePending("t1", intentJSON(t, renamePendingIntent{
		BranchOld: renameOldBranch, BranchNew: renameNewBranch,
	}))
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if store.deleteTaskCount != 0 {
		t.Errorf("delete must be refused (row intact)")
	}
	if got := store.tasks["t1"]; got.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (no delete intent committed)", got.Status)
	}
	if _, ok := store.pendingOf("t1"); !ok {
		t.Errorf("delete must not converge the intent (no auto convergence)")
	}
}

// sqlNullString 是测试内 sql.NullString 的简写。
func sqlNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}
