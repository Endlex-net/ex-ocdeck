package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	ocdecktask "ocdeck/internal/domain/task"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedProjectTask(t *testing.T, db *DB, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
}

// seedCreatingTaskForCommit 创建一个 status=creating 的任务，供 CommitCreated 成功路径 seed
// （F-05：不得用 suspended 测 CommitCreated 成功路径）。返回后 CommitCreated(.., "creating", init)
// 把 status 迁到 suspended 并写入 init_status。
func seedCreatingTaskForCommit(t *testing.T, db *DB, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: "p1", Name: "task", Branch: "b", Status: "creating", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTestDB(t)
	// 第二次调用 Migrate 必须幂等。
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}
	// 表存在。
	var n int
	err := db.QueryRow("SELECT count(*) FROM projects").Scan(&n)
	if err != nil {
		t.Fatalf("query projects: %v", err)
	}
	if n != 0 {
		t.Errorf("projects count = %d, want 0", n)
	}
}

func TestDBFilePermissions_0600(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	info, err := os.Stat(filepath.Join(dir, "ocdeck.db"))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("db perm = %o, want 0600", info.Mode().Perm())
	}
}

func TestForeignKeys_On(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// 插入任务后删除项目，任务应被 CASCADE 删除。
	if err := db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.DeleteProject(ctx, "p1"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var taskCount int
	if err := db.QueryRow("SELECT count(*) FROM tasks WHERE id = 't1'").Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Errorf("task not cascade-deleted, count=%d", taskCount)
	}
}

func TestTaskEnvVars_CascadeOnDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo")
	_ = db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	})
	_ = db.SetTaskEnvVar(ctx, "t1", "FOO", "bar")
	if _, err := db.DeleteTask(ctx, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM task_env_vars WHERE task_id = 't1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("task_env_vars not cascade-deleted, count=%d", n)
	}
}

func TestProjectEnvVars_Upsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo")
	_ = db.SetProjectEnvVar(ctx, "p1", "K", "v1")
	_ = db.SetProjectEnvVar(ctx, "p1", "K", "v2")
	vars, err := db.ListProjectEnvVars(ctx, "p1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vars) != 1 || vars[0].Value != "v2" {
		t.Errorf("upsert result = %+v, want single v2", vars)
	}
}

func TestTaskSession_UpsertMaxLastSeen(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo")
	_ = db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100})
	// 较旧的 last_seen_at 不应回退时间戳。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 50})
	rows, err := db.ListTaskSessions(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LastSeenAt != 100 {
		t.Errorf("last_seen_at = %d, want 100 (MAX)", rows[0].LastSeenAt)
	}
}

func TestSingleConnection(t *testing.T) {
	db := openTestDB(t)
	// SetMaxOpenConns(1) 配置后 stats 反映单连接上限。
	stats := db.DB.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", stats.MaxOpenConnections)
	}
}

func TestWithTx_Commit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	err := db.WithTx(ctx, func(q *Queries) error {
		_, err := q.UpdateTaskStatus(ctx, "t1", "active", nsToPtr(sql.NullString{}))
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != "active" {
		t.Errorf("status = %s, want active (committed)", task.Status)
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	sentinel := errors.New("boom")
	err := db.WithTx(ctx, func(q *Queries) error {
		if _, err := q.UpdateTaskStatus(ctx, "t1", "activating", nsToPtr(sql.NullString{})); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx err = %v, want sentinel", err)
	}
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != "suspended" {
		t.Errorf("status = %s, want suspended (rolled back)", task.Status)
	}
}

func TestUpdateTaskNoticeCAS_ConflictNotReplaced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 期望当前为 NULL，写入 A。
	replaced, err := db.UpdateTaskNoticeCAS(ctx, "t1", nsToPtr(sql.NullString{}), nsToPtr(sql.NullString{String: "A", Valid: true}))
	if err != nil || !replaced.Matched {
		t.Fatalf("first CAS: replaced=%v err=%v", replaced, err)
	}
	// 并发：仍期望 NULL，应冲突不替换。
	replaced2, err := db.UpdateTaskNoticeCAS(ctx, "t1", nsToPtr(sql.NullString{}), nsToPtr(sql.NullString{String: "B", Valid: true}))
	if err != nil {
		t.Fatalf("second CAS err: %v", err)
	}
	if replaced2.Matched {
		t.Error("CAS replaced despite notice != expected (lost update)")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Notice.String != "A" {
		t.Errorf("notice = %q, want A (unaffected by conflicting CAS)", task.Notice.String)
	}
	// 期望当前为 A，写入 C，应替换。
	replaced3, err := db.UpdateTaskNoticeCAS(ctx, "t1", nsToPtr(sql.NullString{String: "A", Valid: true}), nsToPtr(sql.NullString{String: "C", Valid: true}))
	if err != nil || !replaced3.Matched {
		t.Fatalf("third CAS: replaced=%v err=%v", replaced3, err)
	}
}

func TestUpdateTaskStatusConditional(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// from suspended → activating 成功。
	updated, err := db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "activating", nsToPtr(sql.NullString{}))
	if err != nil || !updated.Matched {
		t.Fatalf("first conditional: updated=%v err=%v", updated, err)
	}
	// 再次 from suspended → active，应不更新（当前已是 activating）。
	updated2, err := db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "active", nsToPtr(sql.NullString{}))
	if err != nil {
		t.Fatalf("second conditional err: %v", err)
	}
	if updated2.Matched {
		t.Error("conditional updated despite status mismatch")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Status != "activating" {
		t.Errorf("status = %s, want activating", task.Status)
	}
}

// seedActiveTaskOverview 在多项目多任务场景下构造测试数据
// （cross-project-active-sessions D2：跨项目 active 概览聚合）。
// 所有 active 任务均带 session 以便用可控的 last_seen_at 验证排序（updated_at 由
// CreateTask 用 nowUnix() 写入，真实时间戳不可控，故排序测试不依赖它）。
func seedActiveTaskOverview(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "projA", "/tmp/repoA", "main", "repo"); err != nil {
		t.Fatalf("create project p1: %v", err)
	}
	if err := db.CreateProject(ctx, "p2", "projB", "/tmp/repoB", "main", "repo"); err != nil {
		t.Fatalf("create project p2: %v", err)
	}
	// active 任务跨两个项目；non-active 用于验证排除。
	tasks := []TaskRow{
		{ID: "t-active-a", ProjectID: "p1", Name: "taskA", Branch: "bA", Status: "active", WorktreePath: "/tmp/wtA"},
		{ID: "t-active-b", ProjectID: "p2", Name: "taskB", Branch: "bB", Status: "active", WorktreePath: "/tmp/wtB"},
		{ID: "t-active-c", ProjectID: "p1", Name: "taskC", Branch: "bC", Status: "active", WorktreePath: "/tmp/wtC"},
		{ID: "t-suspended", ProjectID: "p1", Name: "taskS", Branch: "bS", Status: "suspended", WorktreePath: "/tmp/wtS"},
	}
	for _, tk := range tasks {
		if err := db.CreateTask(ctx, tk); err != nil {
			t.Fatalf("create task %s: %v", tk.ID, err)
		}
	}
	// 会话：t-active-a 有两个 session（MAX last_seen_at 应取较大者），
	// t-active-c 有一个 session，t-active-b 一个 session。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-active-a", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-active-a", SessionID: "s2", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 300})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-active-b", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 200})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-active-c", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 50})
}

func TestListActiveTaskOverview_Aggregation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedActiveTaskOverview(t, db)

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 仅 3 个 active 任务；suspended 被排除。
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (active only): %+v", len(rows), rows)
	}

	got := map[string]ActiveTaskOverviewRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	// t-active-a: MAX(s1=100, s2=300) = 300。
	if r := got["t-active-a"]; r.LastActiveAt != 300 || r.ProjectName != "projA" || r.Branch != "bA" || r.WorktreePath != "/tmp/wtA" {
		t.Errorf("t-active-a = %+v, want last_active_at=300 projA bA /tmp/wtA", r)
	}
	// t-active-b: single session last_seen_at=200。
	if r := got["t-active-b"]; r.LastActiveAt != 200 || r.ProjectName != "projB" {
		t.Errorf("t-active-b = %+v, want last_active_at=200 projB", r)
	}
	// t-active-c: single session last_seen_at=50。
	if r := got["t-active-c"]; r.LastActiveAt != 50 || r.ProjectName != "projA" {
		t.Errorf("t-active-c = %+v, want last_active_at=50 projA", r)
	}
	// suspended 不应出现。
	if _, ok := got["t-suspended"]; ok {
		t.Error("t-suspended appeared in active overview (must be excluded)")
	}
}

func TestListActiveTaskOverview_SortOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedActiveTaskOverview(t, db)

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 期望 last_active_at DESC：t-active-a(300) > t-active-b(200) > t-active-c(50)。
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	want := []string{"t-active-a", "t-active-b", "t-active-c"}
	for i, w := range want {
		if rows[i].ID != w {
			t.Errorf("rows[%d].ID = %s, want %s", i, rows[i].ID, w)
		}
	}
	if !(rows[0].LastActiveAt > rows[1].LastActiveAt && rows[1].LastActiveAt > rows[2].LastActiveAt) {
		t.Errorf("not strictly DESC: %+v", rows)
	}
}

// TestListActiveTaskOverview_NoSessionFallbackToUpdatedAt 验证无 session 时
// last_active_at 回退到 t.updated_at（cross-project-active-sessions D2）。
func TestListActiveTaskOverview_NoSessionFallbackToUpdatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// seedProjectTask 默认 suspended；切到 active 以纳入概览。
	if _, err := db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "active", nsToPtr(sql.NullString{})); err != nil {
		t.Fatalf("activate t1: %v", err)
	}
	task, _ := db.GetTask(ctx, "t1")

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LastActiveAt != task.UpdatedAt {
		t.Errorf("last_active_at = %d, want updated_at fallback %d", rows[0].LastActiveAt, task.UpdatedAt)
	}
}

func TestListActiveTaskOverview_TieBreakerByID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo")
	// 两个 active 任务带相同 last_seen_at 的 session → tie-breaker 按 t.id ASC。
	_ = db.CreateTask(ctx, TaskRow{ID: "zeta", ProjectID: "p1", Name: "n", Branch: "b", Status: "active", WorktreePath: "/tmp/wt"})
	_ = db.CreateTask(ctx, TaskRow{ID: "alpha", ProjectID: "p1", Name: "n", Branch: "b", Status: "active", WorktreePath: "/tmp/wt"})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "zeta", SessionID: "s", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 500})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "alpha", SessionID: "s", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 500})
	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// 同 last_active_at → id ASC: alpha < zeta。
	if rows[0].ID != "alpha" || rows[1].ID != "zeta" {
		t.Errorf("tie-breaker order = [%s,%s], want [alpha,zeta]", rows[0].ID, rows[1].ID)
	}
}

func TestListActiveTaskOverview_EmptyReturnsEmptySlice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// 无任务 → nil slice（store 层语义；API 层保证 JSON `[]`）。
	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if rows != nil {
		t.Errorf("rows = %v, want nil (no active tasks)", rows)
	}
}

// seedMixedUnitOverview 构造 ms（13 位，opencode time.updated）与秒（updated_at）
// 混存的测试数据（cross-project-active-sessions D2 单位归一化）。
// updated_at 直接经 SQL UPDATE 设为可控秒值（CreateTask 用 nowUnix 不可控）。
func seedMixedUnitOverview(t *testing.T, db *DB, msA, msB, secC int64) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "projA", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, id := range []string{"t-ms-a", "t-ms-b", "t-sec-c"} {
		if err := db.CreateTask(ctx, TaskRow{ID: id, ProjectID: "p1", Name: id, Branch: "b", Status: "active", WorktreePath: "/tmp/wt"}); err != nil {
			t.Fatalf("create task %s: %v", id, err)
		}
	}
	// 直接控制 updated_at 为可控秒值。
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, secC, "t-sec-c"); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}
	// 两个 ms 单位 session（13 位），一个秒单位任务无 session（回退 updated_at）。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-ms-a", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: msA})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t-ms-b", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: msB})
}

// TestListActiveTaskOverview_MsNormalizedToSeconds 验证 ms 单位 last_seen_at
// 被归一化为秒（÷1000），输出始终为 Unix 秒（cross-project-active-sessions D2）。
func TestListActiveTaskOverview_MsNormalizedToSeconds(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// msA = 1785797826297 → 归一化为 1785797826 秒。
	seedMixedUnitOverview(t, db, 1785797826297, 1785797000000, 1785795000)

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(rows), rows)
	}
	got := map[string]ActiveTaskOverviewRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if r := got["t-ms-a"]; r.LastActiveAt != 1785797826 {
		t.Errorf("t-ms-a last_active_at = %d, want 1785797826 (ms ÷1000)", r.LastActiveAt)
	}
	if r := got["t-ms-b"]; r.LastActiveAt != 1785797000 {
		t.Errorf("t-ms-b last_active_at = %d, want 1785797000 (ms ÷1000)", r.LastActiveAt)
	}
	// t-sec-c 无 session → 回退 updated_at（已是秒，不归一化）。
	if r := got["t-sec-c"]; r.LastActiveAt != 1785795000 {
		t.Errorf("t-sec-c last_active_at = %d, want 1785795000 (updated_at seconds)", r.LastActiveAt)
	}
}

// TestListActiveTaskOverview_MsOlderThanSecondsFallback 验证 ms 行实际更老
// （归一化后秒值 < 回退任务的 updated_at）时，秒回退任务排在前面（单位归一化后排序正确）。
// 使用真实 13 位 ms 值（≥1e11）确保命中归一化分支——若用 <1e11 的秒值，CASE 被删测试仍会通过。
func TestListActiveTaskOverview_MsOlderThanSecondsFallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// msA/msB = 1000000000000（13 位）→ 归一化为 1000000000 秒；秒回退任务 updated_at = 1700000000（更新）。
	seedMixedUnitOverview(t, db, 1000000000000, 1000000000000, 1700000000)

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.ID] = r.LastActiveAt
	}
	// 归一化输出必须为秒值（1000000000），证明命中归一化分支。
	if got["t-ms-a"] != 1000000000 {
		t.Errorf("t-ms-a last_active_at = %d, want 1000000000 (13-digit ms ÷1000)", got["t-ms-a"])
	}
	if got["t-ms-b"] != 1000000000 {
		t.Errorf("t-ms-b last_active_at = %d, want 1000000000 (13-digit ms ÷1000)", got["t-ms-b"])
	}
	if got["t-sec-c"] != 1700000000 {
		t.Errorf("t-sec-c last_active_at = %d, want 1700000000 (updated_at seconds)", got["t-sec-c"])
	}
	// 排序：t-sec-c(1700000000) 首位，两个 ms 行(1000000000) 在后 tie-break by id。
	if rows[0].ID != "t-sec-c" {
		t.Errorf("rows[0] = %s, want t-sec-c (seconds fallback 1700000000 > normalized ms 1000000000)", rows[0].ID)
	}
}

// TestListActiveTaskOverview_SameTaskMixedUnits 验证同一任务同时存在秒值 session
// （<1e11，实际更新）与 ms 值 session（≥1e11，归一化后更老）时，last_active_at = 秒值。
// 锁定"逐行归一化在 MAX 之前完成"：若 MAX 在归一化前跑，原始 ms 值（1e12+）会恒胜。
func TestListActiveTaskOverview_SameTaskMixedUnits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "n", Branch: "b", Status: "active", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 秒值 session（<1e11，实际更新，1700000000）+ ms 值 session（≥1e11，归一化为 1000000000，更老）。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "seconds", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1700000000})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "ms", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1000000000000})

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LastActiveAt != 1700000000 {
		t.Errorf("last_active_at = %d, want 1700000000 (per-row normalize before MAX: seconds row wins over normalized ms 1000000000)", rows[0].LastActiveAt)
	}
}

// TestListActiveTaskOverview_SameTaskMixedUnitsInverse 验证反方向：ms session 归一化后
// 比秒值 session 更新 → ms（归一化后）胜。锁定归一化后 MAX 正确性。
func TestListActiveTaskOverview_SameTaskMixedUnitsInverse(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "n", Branch: "b", Status: "active", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 秒值 session（<1e11，1000000000，更老）+ ms 值 session（≥1e11，归一化为 1800000000，更新）。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "seconds", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1000000000})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "ms", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1800000000000})

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LastActiveAt != 1800000000 {
		t.Errorf("last_active_at = %d, want 1800000000 (normalized ms wins over seconds row)", rows[0].LastActiveAt)
	}
}

// TestListActiveTaskOverview_MsNewerThanSecondsFallback 验证 ms 行实际更新
// （归一化后秒值 > 回退任务 updated_at）时，ms 行排在前面。
func TestListActiveTaskOverview_MsNewerThanSecondsFallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// msA = 1800000000000 → 归一化为 1800000000 秒；秒回退任务 updated_at = 1700000000（更老）。
	seedMixedUnitOverview(t, db, 1800000000000, 1750000000000, 1700000000)

	rows, err := db.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].ID != "t-ms-a" {
		t.Errorf("rows[0] = %s, want t-ms-a (normalized ms 1800000000 > seconds fallback 1700000000)", rows[0].ID)
	}
	if rows[1].ID != "t-ms-b" {
		t.Errorf("rows[1] = %s, want t-ms-b (normalized ms 1750000000)", rows[1].ID)
	}
	if rows[2].ID != "t-sec-c" {
		t.Errorf("rows[2] = %s, want t-sec-c (seconds fallback 1700000000)", rows[2].ID)
	}
}

func TestBeginDeleteIntent_Atomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	updated, err := db.BeginDeleteIntent(ctx, "t1", "normal", []ocdecktask.Status{"suspended", "archived", "creation_failed"})
	if err != nil || !updated.Matched {
		t.Fatalf("begin delete intent: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Status != "deleting" || task.DeleteMode.String != "normal" {
		t.Errorf("status=%s delete_mode=%q, want deleting/normal (atomic)", task.Status, task.DeleteMode.String)
	}
	// 不在允许状态集合中时不应更新。
	if err := db.CreateTask(ctx, TaskRow{
		ID: "t2", ProjectID: "p1", Name: "task2", Branch: "b2", Status: "suspended", WorktreePath: "/tmp/wt2",
	}); err != nil {
		t.Fatalf("create t2: %v", err)
	}
	_, _ = db.UpdateTaskStatusConditional(ctx, "t2", "suspended", "active", nsToPtr(sql.NullString{}))
	updated2, err := db.BeginDeleteIntent(ctx, "t2", "force", []ocdecktask.Status{"suspended", "archived"})
	if err != nil {
		t.Fatalf("second intent err: %v", err)
	}
	if updated2.Matched {
		t.Error("intent updated from non-allowed status (active)")
	}
	task2, _ := db.GetTask(ctx, "t2")
	if task2.Status != "active" || task2.DeleteMode.Valid {
		t.Errorf("t2 status=%s delete_mode=%v, want active/NULL (unchanged)", task2.Status, task2.DeleteMode)
	}
}

func TestAlignSessions_CompleteDeletesAbsent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s2", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 90})
	// 全量对齐返回 [s1]，complete=true → 删除缺席的 s2。
	aligned := []SessionRow{
		{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100},
	}
	if err := db.AlignSessions(ctx, "t1", aligned, true, nil); err != nil {
		t.Fatalf("align: %v", err)
	}
	rows, err := db.ListTaskSessions(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Errorf("sessions = %+v, want only s1", rows)
	}
}

func TestAlignSessions_TruncatedKeepsAbsent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s2", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 90})
	// 截断（complete=false）：仅 upsert s1，MUST NOT 删除 s2。
	aligned := []SessionRow{
		{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100},
	}
	if err := db.AlignSessions(ctx, "t1", aligned, false, nil); err != nil {
		t.Fatalf("align: %v", err)
	}
	rows, err := db.ListTaskSessions(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("sessions count = %d, want 2 (absent kept on truncation)", len(rows))
	}
}

func TestAlignSessions_RollbackOnPartialFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 100})
	// noticeFn 读取不存在的任务行 → 返回 sql.ErrNoRows → 事务回滚，upsert 不提交。
	aligned := []SessionRow{
		{TaskID: "t1", SessionID: "s2", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 200},
	}
	noticeFn := func(sql.NullString) sql.NullString { return sql.NullString{String: "x", Valid: true} }
	err := db.AlignSessions(ctx, "nonexistent-task", aligned, true, noticeFn)
	if err == nil {
		t.Fatal("expected error for nonexistent task, got nil")
	}
	// t1 的会话集应不受影响（原子性：中途失败无部分提交）。
	rows, _ := db.ListTaskSessions(ctx, "t1")
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Errorf("sessions = %+v, want original s1 only (no partial commit)", rows)
	}
}

func TestListTaskSessions_TieBreaker(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// last_seen 相同 → session_created_at 降序。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "older", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 50})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "newer", SessionCreatedAt: 99, FirstSeenAt: 1, LastSeenAt: 50})
	// last_seen + created 都相同 → session_id 降序。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "zid", SessionCreatedAt: 5, FirstSeenAt: 1, LastSeenAt: 50})
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "aid", SessionCreatedAt: 5, FirstSeenAt: 1, LastSeenAt: 50})
	// last_seen 最大者排首位。
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "latest", SessionCreatedAt: 10, FirstSeenAt: 1, LastSeenAt: 200})
	rows, err := db.ListTaskSessions(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"latest", "newer", "zid", "aid", "older"}
	if len(rows) != len(want) {
		t.Fatalf("rows=%d want %d", len(rows), len(want))
	}
	for i, r := range rows {
		if r.SessionID != want[i] {
			t.Errorf("rows[%d].session_id = %s, want %s (tie-breaker)", i, r.SessionID, want[i])
		}
	}
}

func TestMigration_IdempotentAfterReopen(t *testing.T) {
	dir := t.TempDir()
	db1, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := db1.CreateProject(context.Background(), "p1", "proj", "/tmp/r", "main", "repo"); err != nil {
		t.Fatalf("create: %v", err)
	}
	db1.Close()
	// 关闭重开后 migration 必须幂等。
	db2, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer db2.Close()
	p, err := db2.GetProject(context.Background(), "p1")
	if err != nil || p.ID != "p1" {
		t.Fatalf("reopen: project not preserved, err=%v", err)
	}
}

func TestTaskSessions_CascadeOnDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_ = db.UpsertTaskSession(ctx, SessionRow{TaskID: "t1", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1})
	if _, err := db.DeleteTask(ctx, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM task_sessions WHERE task_id = 't1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("task_sessions not cascade-deleted, count=%d", n)
	}
}

func TestDeleteProjectIfEmpty_Empty(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	deleted, err := db.DeleteProjectIfEmpty(ctx, "p1")
	if err != nil {
		t.Fatalf("DeleteProjectIfEmpty: %v", err)
	}
	if !deleted {
		t.Fatal("empty project should be deleted")
	}
	// 确认已删除。
	if _, err := db.GetProject(ctx, "p1"); err == nil {
		t.Error("project should not exist after delete")
	}
}

func TestDeleteProjectIfEmpty_HasTasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	deleted, err := db.DeleteProjectIfEmpty(ctx, "p1")
	if err != nil {
		t.Fatalf("DeleteProjectIfEmpty: %v", err)
	}
	if deleted {
		t.Fatal("project with tasks should not be deleted")
	}
	// 项目与任务仍在。
	if p, err := db.GetProject(ctx, "p1"); err != nil || p.ID != "p1" {
		t.Errorf("project should still exist, err=%v", err)
	}
	if task, err := db.GetTask(ctx, "t1"); err != nil || task.ID != "t1" {
		t.Errorf("task should still exist, err=%v", err)
	}
}

func TestDeleteProjectIfEmpty_Nonexistent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	deleted, err := db.DeleteProjectIfEmpty(ctx, "nope")
	if err != nil {
		t.Fatalf("DeleteProjectIfEmpty: %v", err)
	}
	if deleted {
		t.Fatal("nonexistent project should not be reported as deleted")
	}
}

func TestDeleteProjectIfEmpty_Atomic(t *testing.T) {
	// 原子性：DeleteProjectIfEmpty 在单语句内完成"无任务检查+删除"，
	// 无竞态窗口。此处用单连接模型验证语义：有任务时确不删，无任务时确删。
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 无任务 → 删除。
	d1, _ := db.DeleteProjectIfEmpty(ctx, "p1")
	if !d1 {
		t.Error("first delete should succeed (empty)")
	}
	// 重建并加任务。
	if err := db.CreateProject(ctx, "p2", "proj2", "/tmp/repo2", "main", "repo"); err != nil {
		t.Fatalf("create p2: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t2", ProjectID: "p2", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	d2, _ := db.DeleteProjectIfEmpty(ctx, "p2")
	if d2 {
		t.Error("delete should fail (has tasks)")
	}
}

func TestGlobalEnvVars_UpsertAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// manual upsert。
	if err := db.SetGlobalEnvVar(ctx, "K", "manual", "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// upsert 覆盖 mode+value。
	if err := db.SetGlobalEnvVar(ctx, "K", "follow_host", ""); err != nil {
		t.Fatalf("set2: %v", err)
	}
	vars, err := db.ListGlobalEnvVars(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vars) != 1 || vars[0].Mode != "follow_host" || vars[0].Value != "" {
		t.Fatalf("upsert result = %+v, want follow_host/empty", vars)
	}
	// 多条 + key 升序。
	_ = db.SetGlobalEnvVar(ctx, "A", "manual", "a")
	_ = db.SetGlobalEnvVar(ctx, "B", "manual", "b")
	vars, _ = db.ListGlobalEnvVars(ctx)
	if len(vars) != 3 || vars[0].Key != "A" || vars[1].Key != "B" || vars[2].Key != "K" {
		t.Errorf("order = %+v, want A,B,K", vars)
	}
	// delete。
	if err := db.DeleteGlobalEnvVar(ctx, "K"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	vars, _ = db.ListGlobalEnvVars(ctx)
	if len(vars) != 2 {
		t.Errorf("after delete len = %d, want 2", len(vars))
	}
}

func TestMigrate_0003_GlobalEnvVarsTable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// 表存在且可写（migration 已在 Open 内应用）。
	var n int
	if err := db.QueryRow("SELECT count(*) FROM global_env_vars").Scan(&n); err != nil {
		t.Fatalf("query global_env_vars: %v", err)
	}
	if n != 0 {
		t.Errorf("global_env_vars count = %d, want 0", n)
	}
	// schema: key PK, mode NOT NULL, value 可空。
	if err := db.SetGlobalEnvVar(ctx, "K", "follow_host", ""); err != nil {
		t.Fatalf("set follow_host empty value: %v", err)
	}
	if err := db.SetGlobalEnvVar(ctx, "K", "manual", "v"); err != nil {
		t.Fatalf("set manual: %v", err)
	}
}

// --- session 归属隔离 store 层测试（add-plain-dir-project D8） ---

// TestClaimTaskSession_NoConflictInserts 验证无冲突时 claim 插入本任务行。
func TestClaimTaskSession_NoConflictInserts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	claimed, owner, err := db.ClaimTaskSession(ctx, "t1", "sess-1", 10, 11, 12, "parent-x")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || owner != "" {
		t.Fatalf("claimed=%v owner=%q, want true/empty", claimed, owner)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 1 || sessions[0].SessionID != "sess-1" {
		t.Fatalf("sessions = %+v, want [sess-1]", sessions)
	}
	if sessions[0].ParentID != "parent-x" {
		t.Errorf("parent_id = %q, want parent-x", sessions[0].ParentID)
	}
}

// TestClaimTaskSession_ConflictReturnsOwner 验证已被他任务拥有时 claim 返回 false+owner，不写入。
func TestClaimTaskSession_ConflictReturnsOwner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t2", ProjectID: "p1", Name: "task2", Branch: "b2",
		Status: "suspended", WorktreePath: "/wt2"}); err != nil {
		t.Fatal(err)
	}
	// t1 claim sess-shared。
	if claimed, _, err := db.ClaimTaskSession(ctx, "t1", "sess-shared", 1, 1, 1, ""); err != nil || !claimed {
		t.Fatalf("t1 claim: %v %v", claimed, err)
	}
	// t2 claim 同一 session → 冲突。
	claimed, owner, err := db.ClaimTaskSession(ctx, "t2", "sess-shared", 2, 2, 2, "")
	if err != nil {
		t.Fatalf("t2 claim: %v", err)
	}
	if claimed {
		t.Errorf("t2 claim should fail (conflict)")
	}
	if owner != "t1" {
		t.Errorf("owner = %q, want t1", owner)
	}
	// t2 不应有该行。
	t2Sessions, _ := db.ListTaskSessions(ctx, "t2")
	for _, s := range t2Sessions {
		if s.SessionID == "sess-shared" {
			t.Errorf("t2 should not own sess-shared")
		}
	}
}

// TestClaimTaskSession_IdempotentOwnTask 验证本任务已拥有时 claim 幂等刷新 last_seen/parent。
func TestClaimTaskSession_IdempotentOwnTask(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimTaskSession(ctx, "t1", "sess-1", 1, 1, 1, "p1"); err != nil {
		t.Fatal(err)
	}
	// 再次 claim 同任务同 session，last_seen 更新、parent 更新。
	if claimed, _, err := db.ClaimTaskSession(ctx, "t1", "sess-1", 5, 5, 99, "p2"); err != nil || !claimed {
		t.Fatalf("idempotent claim: %v %v", claimed, err)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 1 || sessions[0].LastSeenAt != 99 || sessions[0].ParentID != "p2" {
		t.Errorf("after idempotent claim = %+v, want last_seen=99 parent=p2", sessions)
	}
}

// TestTouchOwnedTaskSession_NoInsert 验证条件 UPDATE 仅本任务已归属行，绝不插入。
func TestTouchOwnedTaskSession_NoInsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	// 未归属 → updated=false，不插入。
	updated, err := db.TouchOwnedTaskSession(ctx, "t1", "sess-unowned", 99)
	if err != nil {
		t.Fatalf("touch unowned: %v", err)
	}
	if updated {
		t.Errorf("touch unowned should return false")
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 0 {
		t.Errorf("unowned touch MUST NOT insert, got %+v", sessions)
	}
	// 预置归属后再 touch → updated=true，刷新 last_seen。
	if _, _, err := db.ClaimTaskSession(ctx, "t1", "sess-1", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	updated, err = db.TouchOwnedTaskSession(ctx, "t1", "sess-1", 99)
	if err != nil || !updated {
		t.Fatalf("touch owned: updated=%v err=%v", updated, err)
	}
	sessions, _ = db.ListTaskSessions(ctx, "t1")
	if sessions[0].LastSeenAt != 99 {
		t.Errorf("last_seen = %d, want 99", sessions[0].LastSeenAt)
	}
}

// TestAlignTaskSessions_RepoClaimConflicts 验证 repo 模式逐个 claim，冲突 ID 上报。
func TestAlignTaskSessions_RepoClaimConflicts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t2", ProjectID: "p1", Name: "task2", Branch: "b2",
		Status: "suspended", WorktreePath: "/wt2"}); err != nil {
		t.Fatal(err)
	}
	// t2 拥有 sess-shared。
	if _, _, err := db.ClaimTaskSession(ctx, "t2", "sess-shared", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	// t1 repo 对齐：listed=[sess-shared, sess-own]。
	listed := []SessionObservation{
		{SessionID: "sess-shared", UpdatedAt: 20},
		{SessionID: "sess-own", UpdatedAt: 20},
	}
	conflicts, err := db.AlignTaskSessions(ctx, "t1", AlignModeRepo, listed, true, nil)
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0] != "sess-shared" {
		t.Errorf("conflicts = %v, want [sess-shared]", conflicts)
	}
	t1, _ := db.ListTaskSessions(ctx, "t1")
	ids := map[string]bool{}
	for _, s := range t1 {
		ids[s.SessionID] = true
	}
	if ids["sess-shared"] {
		t.Errorf("t1 MUST NOT claim sess-shared (owned by t2)")
	}
	if !ids["sess-own"] {
		t.Errorf("t1 should claim sess-own")
	}
}

// TestAlignTaskSessions_OwnedOnlyNoClaim 验证 ownedOnly 模式仅刷新 listed∩owned，绝不 claim。
func TestAlignTaskSessions_OwnedOnlyNoClaim(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	// t1 预置 owned: sess-a。
	if _, _, err := db.ClaimTaskSession(ctx, "t1", "sess-a", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	// listed=[sess-a, sess-b, sess-foreign]；ownedOnly 仅刷新 sess-a，不 claim b/foreign。
	listed := []SessionObservation{
		{SessionID: "sess-a", UpdatedAt: 20},
		{SessionID: "sess-b", UpdatedAt: 20},
		{SessionID: "sess-foreign", UpdatedAt: 20},
	}
	conflicts, err := db.AlignTaskSessions(ctx, "t1", AlignModeOwnedOnly, listed, true, nil)
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("ownedOnly should not report conflicts, got %v", conflicts)
	}
	t1, _ := db.ListTaskSessions(ctx, "t1")
	ids := map[string]bool{}
	for _, s := range t1 {
		ids[s.SessionID] = true
	}
	if !ids["sess-a"] {
		t.Errorf("sess-a should still be owned")
	}
	if ids["sess-b"] || ids["sess-foreign"] {
		t.Errorf("ownedOnly MUST NOT claim sess-b/sess-foreign")
	}
	// sess-a last_seen 被刷新到 20。
	for _, s := range t1 {
		if s.SessionID == "sess-a" && s.LastSeenAt != 20 {
			t.Errorf("sess-a last_seen = %d, want 20", s.LastSeenAt)
		}
	}
}

// TestAlignTaskSessions_UnknownModeFailClosed 验证未知 mode fail-closed 返回错误。
func TestAlignTaskSessions_UnknownModeFailClosed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimTaskSession(ctx, "t1", "sess-a", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	_, err := db.AlignTaskSessions(ctx, "t1", AlignMode(99),
		[]SessionObservation{{SessionID: "x"}}, true, nil)
	if err == nil {
		t.Fatal("unknown mode should fail (fail-closed)")
	}
	// 无写入：sess-a 仍存在。
	t1, _ := db.ListTaskSessions(ctx, "t1")
	if len(t1) != 1 || t1[0].SessionID != "sess-a" {
		t.Errorf("after unknown mode align = %+v, want [sess-a] (no writes)", t1)
	}
}

// TestClaimTaskSession_ConcurrentUniqueOwnership_RealSQLite 验证真实 SQLite 下并发 claim 同一 sessionID：
// 原子 claim 仅一个成功，失败方按路径语义返回 false + owner。验证生产 SQLite 事务原子性
// （非 mockStore）。go test -race 下并发 goroutine 经 MaxOpenConns(1) 串行化，无 data race。
func TestClaimTaskSession_ConcurrentUniqueOwnership_RealSQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	// 两任务竞争同一 sessionID。
	for _, taskID := range []string{"ta", "tb"} {
		if err := db.CreateTask(ctx, TaskRow{ID: taskID, ProjectID: "p1", Name: taskID, Branch: "b",
			Status: "suspended", WorktreePath: "/wt-" + taskID}); err != nil {
			t.Fatal(err)
		}
	}

	const sharedSess = "shared-sess"
	var wg sync.WaitGroup
	var successCount int32
	var claimOwner atomic.Value // string
	claimOwner.Store("")
	for i, taskID := range []string{"ta", "tb"} {
		wg.Add(1)
		go func(idx int, tid string) {
			defer wg.Done()
			claimed, owner, err := db.ClaimTaskSession(ctx, tid, sharedSess, 1, 1, 1, "")
			if err != nil {
				t.Errorf("claim %s: %v", tid, err)
				return
			}
			if claimed {
				atomic.AddInt32(&successCount, 1)
				claimOwner.Store(tid)
			} else if owner != "" {
				// 失败方应得到 owner（另一个任务 ID）。
				_ = idx
			}
		}(i, taskID)
	}
	wg.Wait()

	// 恰好一个 claim 成功。
	if got := atomic.LoadInt32(&successCount); got != 1 {
		t.Fatalf("expected exactly one claim success, got %d", got)
	}
	owner := claimOwner.Load().(string)
	if owner != "ta" && owner != "tb" {
		t.Fatalf("claim owner = %q, want ta or tb", owner)
	}
	// 失败方调用应返回 owner = 成功方。
	loser := "tb"
	if owner == "tb" {
		loser = "ta"
	}
	// 再次 claim（失败方重试）应返回 owner=成功方、claimed=false。
	claimed2, owner2, err := db.ClaimTaskSession(ctx, loser, sharedSess, 1, 1, 1, "")
	if err != nil {
		t.Fatalf("retry claim loser: %v", err)
	}
	if claimed2 || owner2 != owner {
		t.Errorf("loser re-claim: claimed=%v owner=%q, want false/%q", claimed2, owner2, owner)
	}
	// 最终该 session 仅归属一个任务。
	sessA, _ := db.ListTaskSessions(ctx, "ta")
	sessB, _ := db.ListTaskSessions(ctx, "tb")
	total := len(sessA) + len(sessB)
	if total != 1 {
		t.Errorf("total sessions across ta+tb = %d, want 1 (unique ownership)", total)
	}
}
