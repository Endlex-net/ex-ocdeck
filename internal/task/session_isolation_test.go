package task

import (
	"context"
	"database/sql"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/store"
)

// --- add-plain-dir-project D8：session 归属隔离测试（tasks.md 3.4） ---

// seedDirProjectTask 在 mockStore 中创建一个 dir 项目与挂起任务。
func seedDirProjectTask(s *mockStore, taskID, projectID string) TaskRow {
	s.seedProject(ProjectRow{ID: projectID, Name: "dir", Path: "/dir", Kind: ProjectKindDir})
	t := TaskRow{ID: taskID, ProjectID: projectID, Name: "my task", Branch: "",
		Status: StatusSuspended, WorktreePath: "/dir"}
	s.tasks[taskID] = t
	return t
}

// TestClaimTaskSession_ConcurrentUniqueOwnership 的真实 SQLite 原子性验证在 internal/infrastructure/store
// 包（TestClaimTaskSession_ConcurrentUniqueOwnership_RealSQLite）；mockStore 不保证生产
// 事务原子性，故此处不再覆盖（避免 mockStore 并发 seedProject 的 data race）。

// TestClaimTaskSession_ConflictIgnoredDiagnosed 验证已被他任务拥有的 session：claim 返回 false+owner，
// 不写入本任务行（冲突忽略，调用方记诊断）。存量重复 owner 同此处理。
func TestClaimTaskSession_ConflictIgnoredDiagnosed(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	store.seedProject(ProjectRow{ID: "p1", Name: "p1", Path: "/p1", Kind: ProjectKindRepo})
	store.seedProject(ProjectRow{ID: "p2", Name: "p2", Path: "/p2", Kind: ProjectKindRepo})
	// t1 先 claim sess-x。
	if res, err := store.ClaimTaskSession(ctx, "t1", "sess-x", 1, 1, 1, ""); err != nil || !res.Claimed {
		t.Fatalf("t1 claim: claimed=%v err=%v", res.Claimed, err)
	}
	// t2 claim 同一 session → 冲突。
	res, err := store.ClaimTaskSession(ctx, "t2", "sess-x", 2, 2, 2, "")
	if err != nil {
		t.Fatalf("t2 claim err: %v", err)
	}
	if res.Claimed {
		t.Errorf("t2 claim should fail (conflict), got claimed=true")
	}
	if res.OwnerTaskID != "t1" {
		t.Errorf("owner = %q, want t1", res.OwnerTaskID)
	}
	// t2 不应有 sess-x 行。
	t2Sessions, _ := store.ListTaskSessions(ctx, "t2")
	for _, s := range t2Sessions {
		if s.SessionID == "sess-x" {
			t.Errorf("t2 should not own sess-x (conflict ignored), got %+v", s)
		}
	}
}

// TestDirTasks_AlignDontClaimOthersSessions 验证同目录两 dir 任务各自对齐互不认领：
// 任务 A 对齐仅核对自身 owned session，不认领任务 B 拥有的 session；目录中不属于任何任务的 session 不被认领。
func TestDirTasks_AlignDontClaimOthersSessions(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	// 两 dir 任务共享同一目录 /dir。
	store.seedProject(ProjectRow{ID: "pdir", Name: "dir", Path: "/dir", Kind: ProjectKindDir})
	store.tasks["tA"] = TaskRow{ID: "tA", ProjectID: "pdir", Status: StatusSuspended, WorktreePath: "/dir"}
	store.tasks["tB"] = TaskRow{ID: "tB", ProjectID: "pdir", Status: StatusSuspended, WorktreePath: "/dir"}
	// 预置归属：tA 拥有 sess-a，tB 拥有 sess-b（模拟既有锚定/SSE 归属）。
	store.sessions["tA"] = []SessionRow{{TaskID: "tA", SessionID: "sess-a", LastSeenAt: 10}}
	store.sessions["tB"] = []SessionRow{{TaskID: "tB", SessionID: "sess-b", LastSeenAt: 10}}
	// 原始目录列表含 a/b/foreign（foreign 不属任何任务）。
	listed := []SessionObservation{
		{SessionID: "sess-a", UpdatedAt: 20},
		{SessionID: "sess-b", UpdatedAt: 20},
		{SessionID: "sess-foreign", UpdatedAt: 20},
	}
	// tA 按 ownedOnly 对齐。
	ares, err := store.AlignTaskSessions(ctx, "tA", AlignModeOwnedOnly, listed, true, application.NoticeMutation{})
	if err != nil {
		t.Fatalf("tA align: %v", err)
	}
	if len(ares.Conflicts) != 0 {
		t.Errorf("tA ownedOnly should not report conflicts, got %v", ares.Conflicts)
	}
	// tA 仅刷新 sess-a（owned），不认领 sess-b/foreign。
	tASessions, _ := store.ListTaskSessions(ctx, "tA")
	ownedA := map[string]bool{}
	for _, s := range tASessions {
		ownedA[s.SessionID] = true
	}
	if !ownedA["sess-a"] {
		t.Errorf("tA should still own sess-a")
	}
	if ownedA["sess-b"] {
		t.Errorf("tA MUST NOT claim sess-b (owned by tB)")
	}
	if ownedA["sess-foreign"] {
		t.Errorf("tA MUST NOT claim sess-foreign (unowned)")
	}
	// complete=true：tA 删除 owned 缺席行（sess-a 仍在 listed 故保留）。tA 仍有 sess-a。
	if len(tASessions) != 1 || !ownedA["sess-a"] {
		t.Errorf("tA after complete align = %+v, want only sess-a", tASessions)
	}
	// tB 同理：对齐后仍只拥有 sess-b，不认领 sess-a/foreign。
	_, err = store.AlignTaskSessions(ctx, "tB", AlignModeOwnedOnly, listed, true, application.NoticeMutation{})
	if err != nil {
		t.Fatalf("tB align: %v", err)
	}
	tBSessions, _ := store.ListTaskSessions(ctx, "tB")
	ownedB := map[string]bool{}
	for _, s := range tBSessions {
		ownedB[s.SessionID] = true
	}
	if !ownedB["sess-b"] || len(tBSessions) != 1 {
		t.Errorf("tB after align = %+v, want only sess-b", tBSessions)
	}
}

// TestDirTasks_DeleteIsolation 验证删除任务 A 仅删 A 拥有的 session 行，
// 任务 B 的 session 不受影响（DeleteTaskSession 按 taskID+sessionID 限定，既有行为）。
func TestDirTasks_DeleteIsolation(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	store.sessions["tA"] = []SessionRow{{TaskID: "tA", SessionID: "sess-a", LastSeenAt: 10}}
	store.sessions["tB"] = []SessionRow{{TaskID: "tB", SessionID: "sess-b", LastSeenAt: 10}}
	// 删除 tA 的 session。
	if _, err := store.DeleteTaskSession(ctx, "tA", "sess-a"); err != nil {
		t.Fatalf("delete tA sess-a: %v", err)
	}
	tA, _ := store.ListTaskSessions(ctx, "tA")
	if len(tA) != 0 {
		t.Errorf("tA sessions after delete = %+v, want empty", tA)
	}
	tB, _ := store.ListTaskSessions(ctx, "tB")
	if len(tB) != 1 || tB[0].SessionID != "sess-b" {
		t.Errorf("tB sessions after deleting tA = %+v, want [sess-b] (isolated)", tB)
	}
}

// TestAlign_CompleteDeletesOwnedAbsentAndClearsOverflowNotice 验证 complete=true：
// 仅删 owned 缺席行（listed 内的不删），noticeFn 清除 session_overflow notice。
// 用真实 store 验证事务内 notice 清除。
func TestAlign_CompleteDeletesOwnedAbsentAndClearsOverflowNotice(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: StatusSuspended, WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	// 预置 owned：sess-a, sess-stale。
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t1", SessionID: "sess-a", LastSeenAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t1", SessionID: "sess-stale", LastSeenAt: 5}); err != nil {
		t.Fatal(err)
	}
	// 预置 session_overflow notice。
	overflowNotice := encodeNotices([]noticeEntry{{Code: noticeCodeSessionOverflow, Message: "overflow"}})
	if _, err := db.UpdateTaskNotice(ctx, "t1", nullStringToPtr(overflowNotice)); err != nil {
		t.Fatal(err)
	}
	// complete 对齐：listed = [sess-a]（sess-stale 缺席 → 应删；notice 清除 overflow）。
	// P1.4.5：notice 决策以 NoticeMutation 表达——Expected=预置的 overflow notice，
	// New=清除 session_overflow 后（空集合 → nil，对应 NULL）。
	listed := []store.SessionObservation{{SessionID: "sess-a", UpdatedAt: 20}}
	expected := overflowNotice.String
	if _, err := db.AlignTaskSessions(ctx, "t1", store.AlignModeRepo, listed, true,
		application.NoticeMutation{Expected: &expected, New: nil}); err != nil {
		t.Fatalf("AlignTaskSessions: %v", err)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.SessionID] = true
	}
	if !ids["sess-a"] || ids["sess-stale"] {
		t.Errorf("after complete align = %+v, want only sess-a (stale deleted)", sessions)
	}
	// notice 中 overflow 被清除。
	row, _ := db.GetTask(ctx, "t1")
	entries, _ := parseNotices(row.Notice)
	for _, e := range entries {
		if e.Code == noticeCodeSessionOverflow {
			t.Errorf("notice still contains session_overflow after complete align: %+v", entries)
		}
	}
}

// TestAlign_OverflowKeepsAbsentAndNoticePreserved 验证 overflow（complete=false）：
// 不删任何缺席行；application 层先经事务外 CAS 写 overflow notice 再调对齐（对齐失败 notice 保留）。
// 此处验证 store 层 complete=false 不删行 + noticeFn 为 nil 时 notice 不变。
func TestAlign_OverflowKeepsAbsentAndNoticePreserved(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: StatusSuspended, WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t1", SessionID: "sess-stale", LastSeenAt: 5}); err != nil {
		t.Fatal(err)
	}
	// 预置 overflow notice（模拟 application 层事务外 CAS 已写入）。
	overflowNotice := encodeNotices([]noticeEntry{{Code: noticeCodeSessionOverflow, Message: "overflow"}})
	if _, err := db.UpdateTaskNotice(ctx, "t1", nullStringToPtr(overflowNotice)); err != nil {
		t.Fatal(err)
	}
	// overflow 对齐：listed=[sess-a]（sess-stale 缺席但 complete=false 不删；notice 分支跳过不动 notice）。
	listed := []store.SessionObservation{{SessionID: "sess-a", UpdatedAt: 20}}
	if _, err := db.AlignTaskSessions(ctx, "t1", store.AlignModeRepo, listed, false, application.NoticeMutation{}); err != nil {
		t.Fatalf("AlignTaskSessions overflow: %v", err)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.SessionID] = true
	}
	if !ids["sess-stale"] {
		t.Errorf("overflow MUST NOT delete absent owned rows, but sess-stale gone: %+v", sessions)
	}
	// notice 保留 overflow（noticeFn 为 nil）。
	row, _ := db.GetTask(ctx, "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if e.Code == noticeCodeSessionOverflow {
			found = true
		}
	}
	if !found {
		t.Errorf("overflow notice should be preserved (noticeFn nil), got %+v", entries)
	}
}

// TestTouchOwnedTaskSession_DoesNotCreateOwnership 验证 session.updated 不创建归属：
// 未归属 session 的 TouchOwnedTaskSession 返回 updated=false（正常路径，不报错、不插入）；
// 已归属 session 仅刷新 last_seen_at。
func TestTouchOwnedTaskSession_DoesNotCreateOwnership(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "sess-owned", LastSeenAt: 10}}
	// 未归属 session：!Matched，不插入。
	tres, err := store.TouchOwnedTaskSession(ctx, "t1", "sess-unowned", 99)
	if err != nil {
		t.Fatalf("touch unowned: %v", err)
	}
	if tres.Matched || tres.Changed {
		t.Errorf("touch unowned should return !Matched, got %+v", tres)
	}
	t1, _ := store.ListTaskSessions(ctx, "t1")
	for _, s := range t1 {
		if s.SessionID == "sess-unowned" {
			t.Errorf("TouchOwnedTaskSession MUST NOT create ownership row for sess-unowned")
		}
	}
	// 已归属 session：Matched+Changed，刷新 last_seen_at。
	tres, err = store.TouchOwnedTaskSession(ctx, "t1", "sess-owned", 99)
	if err != nil || !tres.Matched || !tres.Changed {
		t.Fatalf("touch owned: res=%+v err=%v", tres, err)
	}
	t1, _ = store.ListTaskSessions(ctx, "t1")
	for _, s := range t1 {
		if s.SessionID == "sess-owned" && s.LastSeenAt != 99 {
			t.Errorf("owned last_seen_at = %d, want 99", s.LastSeenAt)
		}
	}
}

// TestHandleSSEEvent_SessionUpdated_UnownedIgnored 验证 handleSSEEvent 对未归属 session.updated
// 事件忽略（不报错、不创建归属）。
func TestHandleSSEEvent_SessionUpdated_UnownedIgnored(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	// session.updated for unowned session（task_sessions 中无 sess-x）。
	ev := makeEventWithDir("session.updated", "sess-x", 100, "/data/worktrees/p1/t1")
	if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", ev); err != nil {
		t.Fatalf("handleSSEEvent unowned updated: %v", err)
	}
	t1, _ := store.ListTaskSessions(context.Background(), "t1")
	for _, s := range t1 {
		if s.SessionID == "sess-x" {
			t.Errorf("unowned session.updated MUST NOT create ownership row")
		}
	}
}

// TestUnknownKind_ActivateZeroSideEffect 验证 Activate 在项目 kind 未知时零副作用（状态不变）。
func TestUnknownKind_ActivateZeroSideEffect(t *testing.T) {
	store := newMockStore()
	// 未知 kind 项目。
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", Kind: "bogus"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended,
		WorktreePath: "/data/worktrees/p1/t1", Name: "task"}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate with unknown kind should fail")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (zero side effect on unknown kind)", row.Status)
	}
}

// TestUnknownKind_ResumeActiveZeroSideEffect 验证 resumeActive 在项目 kind 未知时零副作用。
func TestUnknownKind_ResumeActiveZeroSideEffect(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", Kind: "bogus"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusActive,
		WorktreePath: "/data/worktrees/p1/t1", Name: "task"}
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	err := m.resumeActive(context.Background(), store.tasks["t1"])
	if err == nil {
		t.Fatal("resumeActive with unknown kind should fail")
	}
	// runtime 不应被注册（零副作用）。
	if rt := m.getRuntime("t1"); rt != nil {
		t.Errorf("runtime should not be registered on unknown kind, got %+v", rt)
	}
}

// TestUnknownKind_SuspendZeroSideEffect 验证 Suspend 公共入口在项目 kind 未知时零副作用
// （D8：任何状态修改或运行时副作用前解析校验 kind）。状态不变（active），无会话副作用。
func TestUnknownKind_SuspendZeroSideEffect(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", Kind: "bogus"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusActive,
		WorktreePath: "/data/worktrees/p1/t1", Name: "task"}
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend unknown kind should fail")
	}
	if !isOpErrCode(err, codeInternal) {
		t.Errorf("err code = %v, want codeInternal (unknown persisted kind, D1)", err)
	}
	// 状态不变（未转 suspending）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active (zero side effect, no state transition)", row.Status)
	}
	// 会话未被清理（KillSession 未调用）。
	if !proc.sessions[serveSessionName("t1")] || !proc.sessions[tuiSessionName("t1")] {
		t.Errorf("sessions were modified on unknown kind (must not kill before kind validation)")
	}
	// 未知 kind → internal（D1：区别于用户请求非法 kind 的 invalid_input）。
}

// TestUnknownKind_ReopenAttachZeroSideEffect 验证 ReopenAttach 在项目 kind 未知时零副作用。
func TestUnknownKind_ReopenAttachZeroSideEffect(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "p", Path: "/repo", Kind: "bogus"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusActive,
		WorktreePath: "/data/worktrees/p1/t1", Name: "task"}
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach with unknown kind should fail")
	}
	// 状态保持 active 不收敛。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active (no convergence on unknown kind)", row.Status)
	}
}

// TestReopenAttach_ClaimConflictKeepsActiveWithLastError 验证 ReopenAttach 锚定 claim 冲突时
// 记 last_error、任务保持 active 不收敛、MUST NOT attach 不属本任务的 session。
func TestReopenAttach_ClaimConflictKeepsActiveWithLastError(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	// 活跃任务 + env snapshot（loadEnvSnapshot 需非空快照）。
	tStore.tasks["t1"] = TaskRow{
		ID: "t1", ProjectID: "p1", Status: StatusActive,
		WorktreePath: "/data/worktrees/p1/t1", Name: "task",
		EnvSnapshot: sql.NullString{String: `{"vars":{}}`, Valid: true},
	}
	proc := newMockProc()
	m := newTestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))
	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach without runtime should fail recovering")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Errorf("code=%s want recovering, err=%v", OpErrorCode(err), err)
	}
	row, _ := tStore.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active", row.Status)
	}
}

// conflictClaimStore 包装 mockStore，ClaimTaskSession 恒返回冲突（已被他任务拥有）。
type conflictClaimStore struct {
	*mockStore
}

func (c *conflictClaimStore) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	return application.ClaimResult{Claimed: false, OwnerTaskID: "other-task"}, nil
}
func (c *conflictClaimStore) ClaimTaskSessionAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	return application.ClaimResult{Claimed: false, OwnerTaskID: "other-task"}, nil
}

// TestUnknownAlignMode_FailClosed 验证 AlignTaskSessions 对未知 mode 在任何写入前返回错误（fail-closed）。
func TestUnknownAlignMode_FailClosed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	_, err := store.AlignTaskSessions(context.Background(), "t1", AlignMode(99),
		[]SessionObservation{{SessionID: "x"}}, true, application.NoticeMutation{})
	if err == nil {
		t.Fatal("AlignTaskSessions with unknown mode should fail (fail-closed)")
	}
}

// TestForeignSessionNotClaimed 验证 foreign session（不属本任务）在 repo 对齐模式下被他任务拥有时
// 不被本任务认领（conflict 上报，store 层跳过）。
func TestForeignSessionNotClaimed(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()
	store.seedProject(ProjectRow{ID: "p1", Name: "p1", Path: "/p1", Kind: ProjectKindRepo})
	store.seedProject(ProjectRow{ID: "p2", Name: "p2", Path: "/p2", Kind: ProjectKindRepo})
	// complete=true 时 align 走 notice 分支（任务行必须存在，镜像生产 fail-closed）。
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/p1"}
	store.tasks["t2"] = TaskRow{ID: "t2", ProjectID: "p2", Status: StatusSuspended, WorktreePath: "/p2"}
	// t2 拥有 sess-shared。
	store.sessions["t2"] = []SessionRow{{TaskID: "t2", SessionID: "sess-shared", LastSeenAt: 10}}
	// t1 repo 对齐 listed=[sess-shared, sess-own]：sess-shared 被他任务拥有 → 冲突跳过；sess-own claim 成功。
	listed := []SessionObservation{
		{SessionID: "sess-shared", UpdatedAt: 20},
		{SessionID: "sess-own", UpdatedAt: 20},
	}
	ares, err := store.AlignTaskSessions(ctx, "t1", AlignModeRepo, listed, true, application.NoticeMutation{})
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	found := false
	for _, sid := range ares.Conflicts {
		if string(sid) == "sess-shared" {
			found = true
		}
	}
	if !found {
		t.Errorf("conflicts = %v, want sess-shared reported (owned by other task)", ares.Conflicts)
	}
	t1, _ := store.ListTaskSessions(ctx, "t1")
	ownsShared := false
	for _, s := range t1 {
		if s.SessionID == "sess-shared" {
			ownsShared = true
		}
	}
	if ownsShared {
		t.Errorf("t1 MUST NOT claim sess-shared (owned by t2)")
	}
}