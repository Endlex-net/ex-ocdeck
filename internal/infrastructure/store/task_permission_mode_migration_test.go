package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0015_TaskPermissionMode_UpgradeFrom0014 验证从 0014 升级路径
// （task-permission-mode tasks 1.1，design D1）：手动应用 0015 之前的全部 migration
// 构造升级前状态（存量任务行无 permission_mode 列），随后 Migrate 仅应用 0015——
// 存量任务由列 DEFAULT 覆盖为 ask（行为等价现状），无需回填（比 0013 更简单，无 kind 维度）。
func TestMigration0015_TaskPermissionMode_UpgradeFrom0014(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ocdeck.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// 手动应用 0015 之前的全部 migration，构造「0014 最新」升级前状态。
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
			if ver >= 15 {
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
		// seed：repo 项目 + 存量任务行（0015 之前无 permission_mode 列，插入语句不写该列）。
		seed := []string{
			`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pr', 'r', '/tmp/r', 'main', 'repo', 1)`,
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, mode, created_at, updated_at)
			 VALUES ('tr', 'pr', 'legacy task', 'ocdeck/a', 'suspended', '/tmp/wt', 'refs/heads/main', 'worktree', 1, 1)`,
		}
		for _, s := range seed {
			if _, err := tx.Exec(s); err != nil {
				t.Fatalf("seed %q: %v", s, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit pre-0015 state: %v", err)
		}
	}

	db := &DB{DB: sqlDB, Queries: New(sqlDB)}
	// Migrate 从 0014 升级到最新版本，验证 0015 的 DEFAULT 覆盖。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from 0014: %v", err)
	}

	tr, err := db.GetTask(ctx, "tr")
	if err != nil {
		t.Fatalf("get legacy task: %v", err)
	}
	if tr.PermissionMode != "ask" {
		t.Errorf("legacy task permission_mode = %q, want ask (存量任务由 DEFAULT 覆盖，行为等价现状)", tr.PermissionMode)
	}
}

// TestMigration0015_TaskPermissionMode_FreshDB 验证全新建库路径（tasks 1.3）：
// 全量 migration 后 CreateTask 显式写 permission_mode 三值 roundtrip 逐字持久化；
// 列 DEFAULT 'ask' 覆盖未显式写入的插入（与存量语义一致）。
func TestMigration0015_TaskPermissionMode_FreshDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.CreateProject(ctx, "pr", "r", "/tmp/r", "main", "repo"); err != nil {
		t.Fatalf("create repo project: %v", err)
	}
	// 显式写三值 roundtrip（CreateTask INSERT 显式写入，tasks 1.3）。
	for i, mode := range []string{"ask", "all-approve", "ai-auto"} {
		id := "t" + mode
		if err := db.CreateTask(ctx, TaskRow{ID: id, ProjectID: "pr", Name: "task " + mode, Branch: "ocdeck/a" + mode,
			Status: "suspended", WorktreePath: "/tmp/wt" + mode, BaseRef: "refs/heads/main",
			Mode: "worktree", PermissionMode: mode}); err != nil {
			t.Fatalf("create task %s: %v", id, err)
		}
		got, err := db.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("get task %s: %v", id, err)
		}
		if got.PermissionMode != mode {
			t.Errorf("task %s permission_mode = %q, want %q (roundtrip #%d)", id, got.PermissionMode, mode, i+1)
		}
		// ListAllTasks / ListTasksByProject 同源透出（查询 SELECT 补齐）。
		all, err := db.ListAllTasks(ctx)
		if err != nil {
			t.Fatalf("list all: %v", err)
		}
		byProject, err := db.ListTasksByProject(ctx, "pr")
		if err != nil {
			t.Fatalf("list by project: %v", err)
		}
		for _, rows := range [][]TaskRow{all, byProject} {
			found := false
			for _, r := range rows {
				if r.ID == id {
					found = true
					if r.PermissionMode != mode {
						t.Errorf("list task %s permission_mode = %q, want %q", id, r.PermissionMode, mode)
					}
				}
			}
			if !found {
				t.Errorf("list (len=%d) missing task %s", len(rows), id)
			}
		}
	}

	// 列 DEFAULT：原始插入省略 permission_mode 列 → ask（与存量语义一致）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, mode, created_at, updated_at)
		 VALUES ('tdef', 'pr', 'default task', 'b', 'suspended', '/tmp/wt2', 'refs/heads/main', 'worktree', 1, 1)`); err != nil {
		t.Fatalf("raw insert without permission_mode: %v", err)
	}
	if tdef, err := db.GetTask(ctx, "tdef"); err != nil {
		t.Fatalf("get default-permission task: %v", err)
	} else if tdef.PermissionMode != "ask" {
		t.Errorf("default permission_mode = %q, want ask", tdef.PermissionMode)
	}

	// CreateTask 空串按列 DEFAULT 语义显式写 'ask'（与 mode 空串兜底同型，杜绝持久化空值）。
	if err := db.CreateTask(ctx, TaskRow{ID: "tempty", ProjectID: "pr", Name: "empty pm", Branch: "ocdeck/e",
		Status: "suspended", WorktreePath: "/tmp/wt3", BaseRef: "refs/heads/main", Mode: "worktree"}); err != nil {
		t.Fatalf("create task with empty permission_mode: %v", err)
	}
	if got, err := db.GetTask(ctx, "tempty"); err != nil {
		t.Fatalf("get empty-pm task: %v", err)
	} else if got.PermissionMode != "ask" {
		t.Errorf("empty permission_mode written = %q, want ask", got.PermissionMode)
	}
}
