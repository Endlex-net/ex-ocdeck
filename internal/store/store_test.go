package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{
		ID: taskID, ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
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
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
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
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
	_ = db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	})
	_ = db.SetTaskEnvVar(ctx, "t1", "FOO", "bar")
	if err := db.DeleteTask(ctx, "t1"); err != nil {
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
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
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
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
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
		return q.UpdateTaskStatus(ctx, "t1", "active", sql.NullString{})
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
		if err := q.UpdateTaskStatus(ctx, "t1", "activating", sql.NullString{}); err != nil {
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
	replaced, err := db.UpdateTaskNoticeCAS(ctx, "t1", sql.NullString{}, sql.NullString{String: "A", Valid: true})
	if err != nil || !replaced {
		t.Fatalf("first CAS: replaced=%v err=%v", replaced, err)
	}
	// 并发：仍期望 NULL，应冲突不替换。
	replaced2, err := db.UpdateTaskNoticeCAS(ctx, "t1", sql.NullString{}, sql.NullString{String: "B", Valid: true})
	if err != nil {
		t.Fatalf("second CAS err: %v", err)
	}
	if replaced2 {
		t.Error("CAS replaced despite notice != expected (lost update)")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Notice.String != "A" {
		t.Errorf("notice = %q, want A (unaffected by conflicting CAS)", task.Notice.String)
	}
	// 期望当前为 A，写入 C，应替换。
	replaced3, err := db.UpdateTaskNoticeCAS(ctx, "t1", sql.NullString{String: "A", Valid: true}, sql.NullString{String: "C", Valid: true})
	if err != nil || !replaced3 {
		t.Fatalf("third CAS: replaced=%v err=%v", replaced3, err)
	}
}

func TestUpdateTaskStatusConditional(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// from suspended → activating 成功。
	updated, err := db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "activating", sql.NullString{})
	if err != nil || !updated {
		t.Fatalf("first conditional: updated=%v err=%v", updated, err)
	}
	// 再次 from suspended → active，应不更新（当前已是 activating）。
	updated2, err := db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "active", sql.NullString{})
	if err != nil {
		t.Fatalf("second conditional err: %v", err)
	}
	if updated2 {
		t.Error("conditional updated despite status mismatch")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Status != "activating" {
		t.Errorf("status = %s, want activating", task.Status)
	}
}

func TestBeginDeleteIntent_Atomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	updated, err := db.BeginDeleteIntent(ctx, "t1", "normal", []string{"suspended", "archived", "creation_failed"})
	if err != nil || !updated {
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
	_, _ = db.UpdateTaskStatusConditional(ctx, "t2", "suspended", "active", sql.NullString{})
	updated2, err := db.BeginDeleteIntent(ctx, "t2", "force", []string{"suspended", "archived"})
	if err != nil {
		t.Fatalf("second intent err: %v", err)
	}
	if updated2 {
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
	if err := db1.CreateProject(context.Background(), "p1", "proj", "/tmp/r", "main"); err != nil {
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
	if err := db.DeleteTask(ctx, "t1"); err != nil {
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
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
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
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 无任务 → 删除。
	d1, _ := db.DeleteProjectIfEmpty(ctx, "p1")
	if !d1 {
		t.Error("first delete should succeed (empty)")
	}
	// 重建并加任务。
	if err := db.CreateProject(ctx, "p2", "proj2", "/tmp/repo2", "main"); err != nil {
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
