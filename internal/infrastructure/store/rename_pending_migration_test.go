// rename_pending_migration_test.go 验证 migration 0016：tasks.rename_pending 可空 TEXT 列
// （task-info-editable tasks 1.1）。
//
// 覆盖：
//   - 全新建库：PRAGMA table_info(tasks) 含 rename_pending（TEXT、可空、默认 NULL）；
//   - 存量库升级（0015 → 0016）：手动应用 0016 之前的全部 migration 构造升级前状态，
//     随后 Migrate 仅应用 0016，存量任务行 rename_pending 为 NULL、读写正常。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// assertRenamePendingColumn 校验 PRAGMA table_info(tasks) 中 rename_pending 列的形态：
// type=TEXT、notnull=0（可空）、dflt_value=NULL（默认 NULL）。
func assertRenamePendingColumn(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(tasks)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(tasks): %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info row: %v", err)
		}
		if name != "rename_pending" {
			continue
		}
		found = true
		if colType != "TEXT" {
			t.Errorf("rename_pending type = %q, want TEXT", colType)
		}
		if notNull != 0 {
			t.Errorf("rename_pending notnull = %d, want 0 (可空)", notNull)
		}
		if dflt.Valid {
			t.Errorf("rename_pending dflt_value = %q, want NULL (默认 NULL)", dflt.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if !found {
		t.Fatal("PRAGMA table_info(tasks) 未包含 rename_pending 列（tasks 1.1）")
	}
}

// TestMigration0016_TaskRenamePending_FreshDB 验证全新建库路径。
func TestMigration0016_TaskRenamePending_FreshDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	assertRenamePendingColumn(t, db)

	// 新任务行 rename_pending 默认 NULL，写入后可读回（JSON 内容 roundtrip）。
	if err := db.CreateProject(ctx, "pr", "r", "/tmp/r", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "pr", Name: "task", Branch: "ocdeck/a",
		Status: "suspended", WorktreePath: "/tmp/wt", Mode: "worktree", PermissionMode: "ask"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	row, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.RenamePending.Valid {
		t.Errorf("fresh task rename_pending = %+v, want NULL", row.RenamePending)
	}
	const intent = `{"name":"n2","branch_old":"ocdeck/a","branch_new":"ocdeck/b","env_snapshot":null}`
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET rename_pending = ? WHERE id = 't1'`, intent); err != nil {
		t.Fatalf("seed rename_pending: %v", err)
	}
	row, err = db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("re-get task: %v", err)
	}
	if !row.RenamePending.Valid || row.RenamePending.String != intent {
		t.Errorf("rename_pending = %+v, want %q（JSON 内容 roundtrip）", row.RenamePending, intent)
	}
	// List 读侧同源透出（scanTaskRow 列序一致）。
	all, err := db.ListAllTasks(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list all: n=%d err=%v", len(all), err)
	}
	if !all[0].RenamePending.Valid || all[0].RenamePending.String != intent {
		t.Errorf("list rename_pending = %+v, want %q", all[0].RenamePending, intent)
	}
}

// TestMigration0016_TaskRenamePending_UpgradeFrom0015 验证从 0015 升级路径：
// 手动应用 0016 之前的全部 migration 构造「0015 最新」升级前状态（存量任务行无
// rename_pending 列），随后 Migrate 仅应用 0016——存量行 rename_pending 为 NULL。
func TestMigration0016_TaskRenamePending_UpgradeFrom0015(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ocdeck.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// 手动应用 0016 之前的全部 migration，构造「0015 最新」升级前状态。
	{
		tx, err := sqlDB.Begin()
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)"); err != nil {
			t.Fatalf("ensure schema_version: %v", err)
		}
		names, err := migrationNames()
		if err != nil {
			t.Fatalf("migration names: %v", err)
		}
		for _, name := range names {
			ver, err := migrationVersion(name)
			if err != nil {
				t.Fatalf("migration version %s: %v", name, err)
			}
			if ver >= 16 {
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
		// seed：repo 项目 + 存量任务行（0016 之前无 rename_pending 列，插入语句不写该列）。
		seed := []string{
			`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pr', 'r', '/tmp/r', 'main', 'repo', 1)`,
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, mode, permission_mode, created_at, updated_at)
			 VALUES ('tr', 'pr', 'legacy task', 'ocdeck/a', 'suspended', '/tmp/wt', 'refs/heads/main', 'worktree', 'ask', 1, 1)`,
		}
		for _, s := range seed {
			if _, err := tx.Exec(s); err != nil {
				t.Fatalf("seed %q: %v", s, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit pre-0016 state: %v", err)
		}
	}

	db := &DB{DB: sqlDB, Queries: New(sqlDB)}
	// Migrate 从 0015 升级到最新版本（存量库启动成功，tasks 1.1）。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from 0015: %v", err)
	}
	assertRenamePendingColumn(t, db)

	tr, err := db.GetTask(ctx, "tr")
	if err != nil {
		t.Fatalf("get legacy task: %v", err)
	}
	if tr.RenamePending.Valid {
		t.Errorf("legacy task rename_pending = %+v, want NULL（存量行默认 NULL）", tr.RenamePending)
	}
}
