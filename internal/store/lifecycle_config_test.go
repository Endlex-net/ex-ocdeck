package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitNextSecond 等待到下一秒边界，确保 nowUnix()（秒级精度）在调用前后产生不同的时间戳。
// 用于严格断言 updated_at 被刷新：记录 old 后调用本 helper，再执行写操作，new 必然严格大于 old。
func waitNextSecond(t *testing.T, old int64) {
	t.Helper()
	deadline := time.Unix(old, 0).Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// taskUpdatedAt 读取任务当前 updated_at，供 CAS 刷新断言使用。
func taskUpdatedAt(t *testing.T, db *DB, id string) int64 {
	t.Helper()
	task, err := db.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("get task %s for updated_at: %v", id, err)
	}
	return task.UpdatedAt
}

// applyMigrationsUpto 在裸 DB 上按序应用编号 <= maxVer 的 migration 文件，并在 schema_version 记录版本。
// 用于测试"0007 前已有任务"等分步迁移场景：先 upto 某版本、插入数据、再调用 db.Migrate() 应用剩余版本。
// 复用生产 migrationsFS embed，不引入额外 DDL 副本。
func applyMigrationsUpto(t *testing.T, db *DB, maxVer int) {
	t.Helper()
	names, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var ordered []string
	for _, e := range names {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			ordered = append(ordered, e.Name())
		}
	}
	sort.Strings(ordered)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin migration tx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("ensure schema_version: %v", err)
	}
	for _, name := range ordered {
		ver, err := strconv.Atoi(strings.TrimSuffix(name, ".sql")[:4])
		if err != nil {
			t.Fatalf("parse version %s: %v", name, err)
		}
		if ver > maxVer {
			continue
		}
		content, err := migrationsFS.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", ver); err != nil {
			t.Fatalf("record migration %s: %v", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migrations upto %d: %v", maxVer, err)
	}
	committed = true
}

// openTestDBRaw 打开裸 SQLite 连接（不应用任何 migration），供分步迁移测试使用。
func openTestDBRaw(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ocdeck.db")
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return &DB{DB: sqlDB, Queries: New(sqlDB)}
}

// TestMigration0007_LifecycleConfigTableAndColumns 验证 migration 0007 应用后：
// project_lifecycle_configs 表存在且可写，tasks 增列 init_status（默认 'none'）/ init_error（nullable）。
func TestMigration0007_LifecycleConfigTableAndColumns(t *testing.T) {
	db := openTestDB(t)
	// project_lifecycle_configs 表存在且可写（migration 0007 已在 Open 内应用）。
	var n int
	if err := db.QueryRow("SELECT count(*) FROM project_lifecycle_configs").Scan(&n); err != nil {
		t.Fatalf("query project_lifecycle_configs: %v", err)
	}
	if n != 0 {
		t.Errorf("project_lifecycle_configs count = %d, want 0", n)
	}
	// tasks.init_status / init_error 列存在（直接 SELECT 不报错）。
	seedProjectTask(t, db, "t1")
	var initStatus string
	var initError sql.NullString
	if err := db.QueryRow("SELECT init_status, init_error FROM tasks WHERE id = 't1'").Scan(&initStatus, &initError); err != nil {
		t.Fatalf("select init_status/init_error: %v", err)
	}
	if initStatus != "none" {
		t.Errorf("init_status = %q, want 'none' (column default)", initStatus)
	}
	if initError.Valid {
		t.Errorf("init_error = %v, want NULL (nullable, unset)", initError)
	}
}

// TestMigration0007_ExistingTasksInitStatusDefaultNone 验证存量任务迁移后 init_status 默认 'none'。
// 此处模拟"迁移前已有任务"：CreateTask 不显式写 init_status，列默认值生效。
func TestMigration0007_ExistingTasksInitStatusDefaultNone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// CreateTask 未写 init_status，读取应得列默认 'none'。
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.InitStatus != "none" {
		t.Errorf("InitStatus = %q, want 'none' (column default)", task.InitStatus)
	}
	if task.InitError.Valid {
		t.Errorf("InitError = %v, want NULL", task.InitError)
	}
}

// TestMigration0007_PreExistingTaskBackfilledDefaultNone 验证真实"0007 前已有任务"场景：
// 应用 0001-0004 → 插入任务（此时 tasks 无 init_status 列）→ 应用 0007（ALTER TABLE ADD COLUMN）
// → 断言存量任务 init_status='none'（列 DEFAULT 生效）且 init_error 为 NULL。
func TestMigration0007_PreExistingTaskBackfilledDefaultNone(t *testing.T) {
	db := openTestDBRaw(t)
	ctx := context.Background()
	// 阶段 1：仅应用 0001-0004，tasks 此时无 init_status/init_error 列。
	applyMigrationsUpto(t, db, 4)
	// 阶段 2：插入任务（迁移前已有数据）。
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 确认此时查询 init_status 列不存在（验证 0007 尚未应用）。
	var dummy string
	if err := db.QueryRow("SELECT init_status FROM tasks WHERE id = 't1'").Scan(&dummy); err == nil {
		t.Fatal("init_status column should NOT exist before migration 0007")
	}
	// 阶段 3：应用剩余 migration（0007），ALTER TABLE ADD COLUMN init_status/init_error。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate applies 0007: %v", err)
	}
	// 阶段 4：存量任务被回填列默认值。
	task, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get after migration 0007: %v", err)
	}
	if task.InitStatus != "none" {
		t.Errorf("InitStatus = %q, want 'none' (backfilled column default for pre-existing task)", task.InitStatus)
	}
	if task.InitError.Valid {
		t.Errorf("InitError = %v, want NULL (nullable column default)", task.InitError)
	}
}
func TestLifecycleConfig_GetMissingRowReturnsEmpty(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
	cfg, err := db.GetLifecycleConfig(ctx, "p1")
	if err != nil {
		t.Fatalf("GetLifecycleConfig missing row: %v", err)
	}
	if cfg.ProjectID != "p1" {
		t.Errorf("ProjectID = %q, want p1", cfg.ProjectID)
	}
	if cfg.InheritPatterns != "" || cfg.InitScript != "" || cfg.PreDeleteScript != "" {
		t.Errorf("missing row cfg = %+v, want all empty scripts", cfg)
	}
}

// TestLifecycleConfig_UpsertReplaceAndGet 验证 Upsert 整体替换并刷新 updated_at，Get 读回一致。
func TestLifecycleConfig_UpsertReplaceAndGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
	if err := db.UpsertLifecycleConfig(ctx, "p1", "pat-a", "init-a", "predel-a"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	cfg, err := db.GetLifecycleConfig(ctx, "p1")
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	if cfg.InheritPatterns != "pat-a" || cfg.InitScript != "init-a" || cfg.PreDeleteScript != "predel-a" {
		t.Errorf("cfg 1 = %+v, want pat-a/init-a/predel-a", cfg)
	}
	firstUpdatedAt := cfg.UpdatedAt
	// 整体替换：新值覆盖旧值。
	if err := db.UpsertLifecycleConfig(ctx, "p1", "pat-b", "init-b", "predel-b"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	cfg2, err := db.GetLifecycleConfig(ctx, "p1")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if cfg2.InheritPatterns != "pat-b" || cfg2.InitScript != "init-b" || cfg2.PreDeleteScript != "predel-b" {
		t.Errorf("cfg 2 = %+v, want pat-b/init-b/predel-b", cfg2)
	}
	if cfg2.UpdatedAt < firstUpdatedAt {
		t.Errorf("updated_at = %d, want >= %d (refreshed on upsert)", cfg2.UpdatedAt, firstUpdatedAt)
	}
}

// TestLifecycleConfig_CascadeOnDeleteProject 验证删除项目时 lifecycle_configs 被 CASCADE 删除。
func TestLifecycleConfig_CascadeOnDeleteProject(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main")
	_ = db.UpsertLifecycleConfig(ctx, "p1", "pat", "init", "predel")
	if err := db.DeleteProject(ctx, "p1"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM project_lifecycle_configs WHERE project_id = 'p1'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("project_lifecycle_configs not cascade-deleted, count=%d", n)
	}
}

// TestCommitCreated_FromCreating 验证 CommitCreated 从 creating → suspended，置 init_status 并清 last_error。
func TestCommitCreated_FromCreating(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 先把任务置为 creating 并带 last_error，模拟创建中状态。
	_, _ = db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "creating", sql.NullString{String: "old err", Valid: true})
	updated, err := db.CommitCreated(ctx, "t1", "creating", "pending")
	if err != nil || !updated {
		t.Fatalf("CommitCreated from creating: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Status != "suspended" {
		t.Errorf("status = %s, want suspended", task.Status)
	}
	if task.InitStatus != "pending" {
		t.Errorf("init_status = %s, want pending", task.InitStatus)
	}
	if task.LastError.Valid {
		t.Errorf("last_error = %v, want NULL (cleared)", task.LastError)
	}
}

// TestCommitCreated_FromCreationFailed 验证 CommitCreated 从 creation_failed → suspended，清旧 creation_failed 错误。
func TestCommitCreated_FromCreationFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_, _ = db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "creation_failed", sql.NullString{String: "create err", Valid: true})
	updated, err := db.CommitCreated(ctx, "t1", "creation_failed", "none")
	if err != nil || !updated {
		t.Fatalf("CommitCreated from creation_failed: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.Status != "suspended" || task.InitStatus != "none" {
		t.Errorf("status=%s init_status=%s, want suspended/none", task.Status, task.InitStatus)
	}
	if task.LastError.Valid {
		t.Errorf("last_error = %v, want NULL (cleared)", task.LastError)
	}
}

// TestCommitCreated_StatusMismatchNoUpdate 验证 expectedStatus 不匹配时 rows=0（updated=false），不报错。
func TestCommitCreated_StatusMismatchNoUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 当前 status=suspended，期望 creating → 不匹配。
	updated, err := db.CommitCreated(ctx, "t1", "creating", "pending")
	if err != nil {
		t.Fatalf("CommitCreated mismatch err: %v", err)
	}
	if updated {
		t.Error("CommitCreated updated despite status mismatch (expected creating, was suspended)")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "none" {
		t.Errorf("init_status = %s, want none (unchanged on mismatch)", task.InitStatus)
	}
}

// TestClaimInitRun_PendingToRunning 验证 ClaimInitRun：suspended+pending → running。
func TestClaimInitRun_PendingToRunning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	updated, err := db.ClaimInitRun(ctx, "t1")
	if err != nil || !updated {
		t.Fatalf("ClaimInitRun: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "running" {
		t.Errorf("init_status = %s, want running", task.InitStatus)
	}
}

// TestClaimInitRun_ConditionNotMetRowsZero 验证 ClaimInitRun 条件不满足时 rows=0。
func TestClaimInitRun_ConditionNotMetRowsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 当前 init_status=none（非 pending），claim 应不更新。
	updated, err := db.ClaimInitRun(ctx, "t1")
	if err != nil {
		t.Fatalf("ClaimInitRun err: %v", err)
	}
	if updated {
		t.Error("ClaimInitRun updated despite init_status != pending")
	}
}

// TestClaimInitRerun_FailedOrSucceededToRunning 验证 ClaimInitRerun：suspended + failed|succeeded → running（清 init_error）。
func TestClaimInitRerun_FailedOrSucceededToRunning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// failed → running。
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	_, _ = db.ClaimInitRun(ctx, "t1")
	_, _ = db.FinishInitRun(ctx, "t1", "failed", sql.NullString{String: "boom", Valid: true})
	updated, err := db.ClaimInitRerun(ctx, "t1")
	if err != nil || !updated {
		t.Fatalf("ClaimInitRerun from failed: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "running" {
		t.Errorf("init_status = %s, want running", task.InitStatus)
	}
	if task.InitError.Valid {
		t.Errorf("init_error = %v, want NULL (cleared on rerun claim)", task.InitError)
	}
	// succeeded → running。
	_, _ = db.FinishInitRun(ctx, "t1", "succeeded", sql.NullString{})
	updated2, err := db.ClaimInitRerun(ctx, "t1")
	if err != nil || !updated2 {
		t.Fatalf("ClaimInitRerun from succeeded: updated=%v err=%v", updated2, err)
	}
	task2, _ := db.GetTask(ctx, "t1")
	if task2.InitStatus != "running" {
		t.Errorf("init_status = %s, want running (rerun from succeeded)", task2.InitStatus)
	}
}

// TestClaimInitRerun_ConditionNotMetRowsZero 验证 ClaimInitRerun 条件不满足时 rows=0。
func TestClaimInitRerun_ConditionNotMetRowsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// init_status=none（非 failed/succeeded），rerun claim 应不更新。
	updated, err := db.ClaimInitRerun(ctx, "t1")
	if err != nil {
		t.Fatalf("ClaimInitRerun err: %v", err)
	}
	if updated {
		t.Error("ClaimInitRerun updated despite init_status not in {failed,succeeded}")
	}
}

// TestFinishInitRun_RunningToSucceededOrFailed 验证 FinishInitRun：running → succeeded|failed，成功清 init_error，失败写 init_error。
func TestFinishInitRun_RunningToSucceededOrFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	_, _ = db.ClaimInitRun(ctx, "t1")
	// 成功：init_error 清空。
	updated, err := db.FinishInitRun(ctx, "t1", "succeeded", sql.NullString{})
	if err != nil || !updated {
		t.Fatalf("FinishInitRun succeeded: updated=%v err=%v", updated, err)
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "succeeded" || task.InitError.Valid {
		t.Errorf("after succeeded: init_status=%s init_error=%v, want succeeded/NULL", task.InitStatus, task.InitError)
	}
	// 再次 claim → failed：写 init_error。
	_, _ = db.ClaimInitRerun(ctx, "t1")
	updated2, err := db.FinishInitRun(ctx, "t1", "failed", sql.NullString{String: "init failed", Valid: true})
	if err != nil || !updated2 {
		t.Fatalf("FinishInitRun failed: updated=%v err=%v", updated2, err)
	}
	task2, _ := db.GetTask(ctx, "t1")
	if task2.InitStatus != "failed" || task2.InitError.String != "init failed" {
		t.Errorf("after failed: init_status=%s init_error=%v, want failed/'init failed'", task2.InitStatus, task2.InitError)
	}
}

// TestFinishInitRun_ConditionNotMetRowsZero 验证 FinishInitRun 非 running 时 rows=0。
func TestFinishInitRun_ConditionNotMetRowsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// init_status=none（非 running），finish 应不更新。
	updated, err := db.FinishInitRun(ctx, "t1", "succeeded", sql.NullString{})
	if err != nil {
		t.Fatalf("FinishInitRun err: %v", err)
	}
	if updated {
		t.Error("FinishInitRun updated despite init_status != running")
	}
}

// TestClaimInitRun_StatusNotSuspendedRowsZero 验证 ClaimInitRun 在 status 非 suspended（如 active）时 CAS 失败。
// 证明 WHERE 条件含 status='suspended'，即便 init_status=pending 也不应被 claim。
func TestClaimInitRun_StatusNotSuspendedRowsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 置为 suspended+pending，再切到 active（保留 init_status=pending）。
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	_, _ = db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "active", sql.NullString{})
	updated, err := db.ClaimInitRun(ctx, "t1")
	if err != nil {
		t.Fatalf("ClaimInitRun err: %v", err)
	}
	if updated {
		t.Error("ClaimInitRun updated despite status=active (WHERE status='suspended' not satisfied)")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "pending" {
		t.Errorf("init_status = %s, want pending (unchanged when status != suspended)", task.InitStatus)
	}
}

// TestClaimInitRerun_StatusNotSuspendedRowsZero 验证 ClaimInitRerun 在 status 非 suspended（如 archived）时 CAS 失败。
// 证明 WHERE 条件含 status='suspended'，即便 init_status=failed 也不应被 rerun claim。
func TestClaimInitRerun_StatusNotSuspendedRowsZero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	// 走完整流程到 failed，再切到 archived（保留 init_status=failed）。
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	_, _ = db.ClaimInitRun(ctx, "t1")
	_, _ = db.FinishInitRun(ctx, "t1", "failed", sql.NullString{String: "boom", Valid: true})
	_, _ = db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "archived", sql.NullString{})
	updated, err := db.ClaimInitRerun(ctx, "t1")
	if err != nil {
		t.Fatalf("ClaimInitRerun err: %v", err)
	}
	if updated {
		t.Error("ClaimInitRerun updated despite status=archived (WHERE status='suspended' not satisfied)")
	}
	task, _ := db.GetTask(ctx, "t1")
	if task.InitStatus != "failed" {
		t.Errorf("init_status = %s, want failed (unchanged when status != suspended)", task.InitStatus)
	}
}

// TestCAS_UpdatedAtRefreshed 验证每个 CAS 方法成功执行后严格刷新 updated_at。
// nowUnix() 为秒级精度，故记录 old 后用 waitNextSecond 跨越秒边界，再执行 CAS，断言 new > old。
func TestCAS_UpdatedAtRefreshed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")

	// CommitCreated：creating → suspended。
	_, _ = db.UpdateTaskStatusConditional(ctx, "t1", "suspended", "creating", sql.NullString{})
	old := taskUpdatedAt(t, db, "t1")
	waitNextSecond(t, old)
	_, _ = db.CommitCreated(ctx, "t1", "creating", "pending")
	new := taskUpdatedAt(t, db, "t1")
	if new <= old {
		t.Errorf("CommitCreated updated_at = %d, want > %d (refreshed)", new, old)
	}

	// ClaimInitRun：pending → running。
	old = new
	waitNextSecond(t, old)
	_, _ = db.ClaimInitRun(ctx, "t1")
	new = taskUpdatedAt(t, db, "t1")
	if new <= old {
		t.Errorf("ClaimInitRun updated_at = %d, want > %d (refreshed)", new, old)
	}

	// FinishInitRun：running → failed。
	old = new
	waitNextSecond(t, old)
	_, _ = db.FinishInitRun(ctx, "t1", "failed", sql.NullString{String: "boom", Valid: true})
	new = taskUpdatedAt(t, db, "t1")
	if new <= old {
		t.Errorf("FinishInitRun updated_at = %d, want > %d (refreshed)", new, old)
	}

	// ClaimInitRerun：failed → running。
	old = new
	waitNextSecond(t, old)
	_, _ = db.ClaimInitRerun(ctx, "t1")
	new = taskUpdatedAt(t, db, "t1")
	if new <= old {
		t.Errorf("ClaimInitRerun updated_at = %d, want > %d (refreshed)", new, old)
	}
}

// TestConvergeInterruptedInitRuns_UpdatedAtRefreshed 验证 Converge 也刷新受影响行的 updated_at。
func TestConvergeInterruptedInitRuns_UpdatedAtRefreshed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	seedProjectTask(t, db, "t1")
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	old := taskUpdatedAt(t, db, "t1")
	waitNextSecond(t, old)
	affected, err := db.ConvergeInterruptedInitRuns(ctx)
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	new := taskUpdatedAt(t, db, "t1")
	if new <= old {
		t.Errorf("Converge updated_at = %d, want > %d (refreshed)", new, old)
	}
}
func TestConvergeInterruptedInitRuns_PendingOrRunningToFailed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// 三个任务同属一个项目，seedProjectTask 每次都建同一项目会冲突，故手动 seed。
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := db.CreateTask(ctx, TaskRow{
			ID: id, ProjectID: "p1", Name: "task-" + id, Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// t1: pending；t2: running；t3: none（不应被收敛）。
	_, _ = db.CommitCreated(ctx, "t1", "suspended", "pending")
	_, _ = db.CommitCreated(ctx, "t2", "suspended", "pending")
	_, _ = db.ClaimInitRun(ctx, "t2")
	affected, err := db.ConvergeInterruptedInitRuns(ctx)
	if err != nil {
		t.Fatalf("ConvergeInterruptedInitRuns: %v", err)
	}
	if affected != 2 {
		t.Errorf("affected = %d, want 2 (pending + running)", affected)
	}
	t1, _ := db.GetTask(ctx, "t1")
	if t1.InitStatus != "failed" || t1.InitError.String != "interrupted by server restart" {
		t.Errorf("t1 init_status=%s init_error=%v, want failed/'interrupted by server restart'", t1.InitStatus, t1.InitError)
	}
	t2, _ := db.GetTask(ctx, "t2")
	if t2.InitStatus != "failed" || t2.InitError.String != "interrupted by server restart" {
		t.Errorf("t2 init_status=%s init_error=%v, want failed/'interrupted by server restart'", t2.InitStatus, t2.InitError)
	}
	t3, _ := db.GetTask(ctx, "t3")
	if t3.InitStatus != "none" {
		t.Errorf("t3 init_status=%s, want none (not converged)", t3.InitStatus)
	}
	// 幂等：再次收敛应返回 0（已无 pending/running）。
	affected2, _ := db.ConvergeInterruptedInitRuns(ctx)
	if affected2 != 0 {
		t.Errorf("second converge affected = %d, want 0 (idempotent)", affected2)
	}
}
