package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// allow-duplicate-project-path 迁移测试（design D3 + Test Strategy）：
// 0014 重建 projects 表移除 path UNIQUE，runner 以 no-FK 方式执行。

// seedProjectsPathMigrationFixture 在 0013 库上构造存量数据：repo/dir 两个项目 +
// repo 任务 + project_env_vars + project_lifecycle_configs，供升级后逐行比对。
func seedProjectsPathMigrationFixture(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	seed := []string{
		`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pr', 'r', '/tmp/r', 'main', 'repo', 111)`,
		`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pd', 'd', '/tmp/d', '', 'dir', 222)`,
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
		 VALUES ('tr', 'pr', 'repo task', 'ocdeck/a', 'suspended', '/tmp/wt', 'refs/heads/main', 1, 1)`,
		`INSERT INTO project_env_vars (project_id, key, value) VALUES ('pr', 'FOO', 'bar')`,
		`INSERT INTO project_lifecycle_configs (project_id, inherit_patterns, init_script, pre_delete_script, updated_at)
		 VALUES ('pr', '*.go', 'echo init', 'echo bye', 333)`,
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

// foreignKeysOn 读回连接级 PRAGMA foreign_keys。
func foreignKeysOn(t *testing.T, db *DB) int {
	t.Helper()
	var on int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatalf("read PRAGMA foreign_keys: %v", err)
	}
	return on
}

// TestMigration0014_ProjectsPathNotUnique_UpgradeFrom0013 验证 0013 → 0014 升级路径
// （tasks 1.4，走真实 runner Migrate）：存量项目/任务/子表数据逐行保留、子表 FK 目标
// 仍为 projects、迁移后子表可写入、path 可插重复值、foreign_key_check 无违规、FK 恢复 ON。
func TestMigration0014_ProjectsPathNotUnique_UpgradeFrom0013(t *testing.T) {
	ctx := context.Background()
	db := openTestDBRaw(t)
	applyMigrationsUpto(t, db, 13)
	seedProjectsPathMigrationFixture(t, db)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from 0013: %v", err)
	}

	// 存量项目逐行保留。
	p, err := db.GetProject(ctx, "pr")
	if err != nil {
		t.Fatalf("get project pr: %v", err)
	}
	if p.Name != "r" || p.Path != "/tmp/r" || p.DefaultBranch != "main" || p.Kind != "repo" || p.CreatedAt != 111 {
		t.Errorf("pr = %+v, want r /tmp/r main repo 111", p)
	}
	pd, err := db.GetProject(ctx, "pd")
	if err != nil {
		t.Fatalf("get project pd: %v", err)
	}
	if pd.Name != "d" || pd.Path != "/tmp/d" || pd.DefaultBranch != "" || pd.Kind != "dir" || pd.CreatedAt != 222 {
		t.Errorf("pd = %+v, want d /tmp/d '' dir 222", pd)
	}

	// 存量任务保留（0013 列 DEFAULT 'worktree' 覆盖 mode）。
	tr, err := db.GetTask(ctx, "tr")
	if err != nil {
		t.Fatalf("get task tr: %v", err)
	}
	if tr.ProjectID != "pr" || tr.Name != "repo task" || tr.Branch != "ocdeck/a" ||
		tr.Status != "suspended" || tr.WorktreePath != "/tmp/wt" || tr.BaseRef != "refs/heads/main" || tr.Mode != "worktree" {
		t.Errorf("tr = %+v, want seeded repo task", tr)
	}

	// 存量 project_env_vars / project_lifecycle_configs 保留。
	vars, err := db.ListProjectEnvVars(ctx, "pr")
	if err != nil {
		t.Fatalf("list env vars: %v", err)
	}
	if len(vars) != 1 || vars[0].Key != "FOO" || vars[0].Value != "bar" {
		t.Errorf("env vars = %+v, want [FOO=bar]", vars)
	}
	cfg, err := db.GetLifecycleConfig(ctx, "pr")
	if err != nil {
		t.Fatalf("get lifecycle config: %v", err)
	}
	if cfg.InheritPatterns != "*.go" || cfg.InitScript != "echo init" || cfg.PreDeleteScript != "echo bye" || cfg.UpdatedAt != 333 {
		t.Errorf("lifecycle config = %+v, want seeded values", cfg)
	}

	// 子表 FK 目标仍为 projects（重建未改写子表引用）。
	for _, table := range []string{"tasks", "project_env_vars", "project_lifecycle_configs"} {
		var ddl string
		if err := db.QueryRow(
			`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&ddl); err != nil {
			t.Fatalf("read ddl of %s: %v", table, err)
		}
		if !strings.Contains(ddl, "REFERENCES projects(id)") {
			t.Errorf("%s FK target drifted: %s", table, ddl)
		}
	}

	// 迁移后子表可正常写入。
	if err := db.CreateTask(ctx, TaskRow{ID: "tr2", ProjectID: "pr", Name: "new task", Branch: "b",
		Status: "suspended", WorktreePath: "/tmp/wt2"}); err != nil {
		t.Fatalf("create task on migrated projects: %v", err)
	}

	// path 可插重复值（UNIQUE 约束已移除）。
	if err := db.CreateProject(ctx, "pr2", "r2", "/tmp/r", "main", "repo"); err != nil {
		t.Fatalf("insert duplicate path: %v", err)
	}

	// foreign_key_check 无违规。
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	violated := rows.Next()
	rows.Close()
	if violated {
		t.Error("foreign_key_check reported violations after migration")
	}

	// 连接 FK 恢复 ON。
	if on := foreignKeysOn(t, db); on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1 (restored)", on)
	}

	// 最新版本已记录（0015 起；migration 文件号有跳号，不以文件数断言总数）。
	var maxVer int
	if err := db.QueryRow("SELECT max(version) FROM schema_version").Scan(&maxVer); err != nil {
		t.Fatal(err)
	}
	if maxVer != 15 {
		t.Errorf("max schema_version = %d, want 15", maxVer)
	}
}

// TestMigration0014_Failure_RollbackAndFkRestored 失败路径（tasks 1.5）：
// 0014 执行失败 / 完整性违规 → 整体回滚（schema_version 未记录 14、旧表数据不变）且 FK 恢复 ON。
func TestMigration0014_Failure_RollbackAndFkRestored(t *testing.T) {
	ctx := context.Background()

	t.Run("exec_fail", func(t *testing.T) {
		// 预建 projects_new 使 0014 的 CREATE TABLE 失败。
		db := openTestDBRaw(t)
		applyMigrationsUpto(t, db, 13)
		seedProjectsPathMigrationFixture(t, db)
		if _, err := db.Exec(`CREATE TABLE projects_new (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}

		if err := db.Migrate(ctx); err == nil {
			t.Fatal("Migrate should fail when projects_new already exists")
		}
		assertRolledBack(t, db)
	})

	t.Run("fk_violation", func(t *testing.T) {
		// 预埋孤儿任务行（FK 临时关闭下插入），0014 的 commit 前 foreign_key_check 必须拦截。
		db := openTestDBRaw(t)
		applyMigrationsUpto(t, db, 13)
		seedProjectsPathMigrationFixture(t, db)
		if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
			 VALUES ('torphan', 'ghost', 'orphan', 'b', 'suspended', '/tmp/wt-o', '', 1, 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
			t.Fatal(err)
		}

		if err := db.Migrate(ctx); err == nil {
			t.Fatal("Migrate should fail when foreign_key_check reports violations")
		}
		assertRolledBack(t, db)
		// 孤儿行仍在（回滚未误删）。
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE id = 'torphan'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("orphan task rows = %d, want 1 (rollback preserved)", n)
		}
	})
}

// assertRolledBack 断言失败迁移已整体回滚：版本 14 未记录、存量数据不变、FK 恢复 ON。
func assertRolledBack(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM schema_version WHERE version = 14").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("version 14 recorded, want absent (rolled back)")
	}
	// 旧 projects 表数据不变。
	p, err := db.GetProject(ctx, "pr")
	if err != nil {
		t.Fatalf("project pr should survive rollback: %v", err)
	}
	if p.Path != "/tmp/r" {
		t.Errorf("pr path = %q, want /tmp/r (old table intact)", p.Path)
	}
	if _, err := db.GetProject(ctx, "pd"); err != nil {
		t.Errorf("project pd should survive rollback: %v", err)
	}
	// FK 恢复 ON。
	if on := foreignKeysOn(t, db); on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1 (restored after failure)", on)
	}
}

// TestMigration0014_FreshDB 验证全新空库从零建库成功（tasks 1.6）：全量 migration
// 应用后同 path 可注册多个项目、FK 为 ON。
func TestMigration0014_FreshDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create p1: %v", err)
	}
	// 同 path 第二个项目：UNIQUE 约束已移除。
	if err := db.CreateProject(ctx, "p2", "proj2", "/tmp/repo", "main", "dir"); err != nil {
		t.Fatalf("create p2 with duplicate path: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE path = '/tmp/repo'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("projects with same path = %d, want 2", n)
	}
	if on := foreignKeysOn(t, db); on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", on)
	}
}

// TestMigration0014_FreshDB_MidwayFailure_NoResidue 验证空库迁移中途失败（tasks 1.6）：
// 回滚后 schema_version 与业务 DDL 均不残留，FK 恢复 ON。
func TestMigration0014_FreshDB_MidwayFailure_NoResidue(t *testing.T) {
	db := openTestDBRaw(t)
	// 预建 0007 才创建的表，使迁移链在 0007 处失败（此前 0001-0006 已在本事务内执行）。
	if _, err := db.Exec(`CREATE TABLE project_lifecycle_configs (project_id TEXT PRIMARY KEY, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(context.Background()); err == nil {
		t.Fatal("Migrate should fail when project_lifecycle_configs pre-exists")
	}

	// schema_version 与业务 DDL 均不残留。
	for _, table := range []string{
		"schema_version", "projects", "tasks", "project_env_vars", "task_env_vars", "task_sessions", "projects_new",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s exists after failed fresh migrate, want absent", table)
		}
	}
	if on := foreignKeysOn(t, db); on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1 (restored after failure)", on)
	}
}

// TestMigrationRunner_RestoreFailureObservability 注入 FK 恢复阶段失败（评审 I1），
// 验证返回错误对恢复失败的可观察性：(a) 恢复失败单独发生时可从返回错误观察到，
// 且已提交的迁移不回滚（design Migration Plan）；(b) 恢复失败与迁移失败同时发生时
// 两者经 errors.Join 均可观察。注入方式为恢复前关闭 DB，仍走真实 restoreForeignKeysOn。
func TestMigrationRunner_RestoreFailureObservability(t *testing.T) {
	ctx := context.Background()
	// open 打开全新 DB 文件（每个子用例独立路径，避免共享已迁移状态；
	// hook 关闭后可经 reopen 用新连接读回已提交状态）。
	open := func(t *testing.T) (*DB, string) {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "ocdeck.db")
		sqlDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
		return &DB{DB: sqlDB, Queries: New(sqlDB)}, dbPath
	}
	reopen := func(t *testing.T, dbPath string) *DB {
		t.Helper()
		sqlDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatalf("reopen sqlite: %v", err)
		}
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
		return &DB{DB: sqlDB, Queries: New(sqlDB)}
	}

	t.Run("restore_fail_alone", func(t *testing.T) {
		db, dbPath := open(t)
		restoreForeignKeysHook = func() { _ = db.Close() }
		t.Cleanup(func() { restoreForeignKeysHook = nil })

		// 空库全量迁移会成功提交，随后恢复阶段失败 → Migrate 返回恢复错误。
		err := db.Migrate(ctx)
		if err == nil {
			t.Fatal("Migrate should fail when FK restore fails")
		}
		if !strings.Contains(err.Error(), "restore foreign_keys=ON") {
			t.Errorf("err = %v, want restore failure observable", err)
		}
		// 已提交的 schema 与版本记录保留（拒绝启动但不回滚已提交迁移）。
		db2 := reopen(t, dbPath)
		var v14 int
		if err := db2.QueryRow("SELECT count(*) FROM schema_version WHERE version = 14").Scan(&v14); err != nil {
			t.Fatal(err)
		}
		if v14 != 1 {
			t.Errorf("version 14 records = %d, want 1 (committed migration survives restore failure)", v14)
		}
	})

	t.Run("restore_fail_with_migration_fail", func(t *testing.T) {
		db, _ := open(t)
		// 预建 projects_new 使 0014 执行失败；hook 在恢复阶段关闭 DB 使恢复同时失败。
		if _, err := db.Exec(`CREATE TABLE projects_new (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		restoreForeignKeysHook = func() { _ = db.Close() }
		t.Cleanup(func() { restoreForeignKeysHook = nil })

		err := db.Migrate(ctx)
		if err == nil {
			t.Fatal("Migrate should fail (migration failure + restore failure)")
		}
		// errors.Join：迁移失败与恢复失败在返回错误中均可观察。
		if !strings.Contains(err.Error(), "apply migration 0014") {
			t.Errorf("err = %v, want migration failure observable", err)
		}
		if !strings.Contains(err.Error(), "restore foreign_keys=ON") {
			t.Errorf("err = %v, want restore failure observable", err)
		}
	})
}

// TestMigration0014_AlreadyAt14_Idempotent 正常路径回归（tasks 1.7）：
// 已是版本 14 的库重启/重复 Migrate 幂等跳过，行为与既有 runner 一致（无 no-FK 待执行迁移）。
func TestMigration0014_AlreadyAt14_Idempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t) // Open 已应用全量 migration（含 0014）
	seedProjectsPathMigrationFixture(t, db)

	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("Migrate #%d on v14 db: %v", i+2, err)
		}
	}
	var dupes int
	if err := db.QueryRow("SELECT count(*) - count(DISTINCT version) FROM schema_version").Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Errorf("duplicate version records = %d, want 0", dupes)
	}
	var v14 int
	if err := db.QueryRow("SELECT count(*) FROM schema_version WHERE version = 14").Scan(&v14); err != nil {
		t.Fatal(err)
	}
	if v14 != 1 {
		t.Errorf("version 14 records = %d, want 1 (recorded exactly once)", v14)
	}
	// 数据完好且可继续写入（FK 语义正常）。
	if err := db.CreateTask(ctx, TaskRow{ID: "tr9", ProjectID: "pr", Name: "after restart", Branch: "b",
		Status: "suspended", WorktreePath: "/tmp/wt9"}); err != nil {
		t.Fatalf("create task after idempotent migrate: %v", err)
	}
	if on := foreignKeysOn(t, db); on != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", on)
	}
}
