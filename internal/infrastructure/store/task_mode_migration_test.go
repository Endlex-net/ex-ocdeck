package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0013_TaskMode_UpgradeFrom0012 验证从 0012 升级路径（add-local-path-task-mode
// tasks 1.2，design D1）：手动应用 0013 之前的全部 migration 构造升级前状态（repo 任务 +
// dir 项目任务），随后 Migrate 仅应用 0013——repo 存量任务由列 DEFAULT 覆盖为 worktree
// （行为等价），dir 项目存量任务被显式回填为 local-path（避免 dir+worktree 非法组合）。
func TestMigration0013_TaskMode_UpgradeFrom0012(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ocdeck.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// 手动应用 0013 之前的全部 migration，构造「0012 最新」升级前状态。
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
			if ver >= 13 {
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
		// seed：repo 项目 + repo 任务（base_ref 全引用）；dir 项目 + dir 任务（base_ref 空串）。
		seed := []string{
			`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pr', 'r', '/tmp/r', 'main', 'repo', 1)`,
			`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES ('pd', 'd', '/tmp/d', '', 'dir', 1)`,
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
			 VALUES ('tr', 'pr', 'repo task', 'ocdeck/a', 'suspended', '/tmp/wt', 'refs/heads/main', 1, 1)`,
			`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
			 VALUES ('td', 'pd', 'dir task', '', 'suspended', '/tmp/d', '', 1, 1)`,
		}
		for _, s := range seed {
			if _, err := tx.Exec(s); err != nil {
				t.Fatalf("seed %q: %v", s, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit pre-0013 state: %v", err)
		}
	}

	db := &DB{DB: sqlDB, Queries: New(sqlDB)}
	// Migrate 仅应用未记录的 0013。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from 0012: %v", err)
	}

	tr, err := db.GetTask(ctx, "tr")
	if err != nil {
		t.Fatalf("get repo task: %v", err)
	}
	if tr.Mode != "worktree" {
		t.Errorf("repo task mode = %q, want worktree (存量 repo 任务行为等价)", tr.Mode)
	}
	td, err := db.GetTask(ctx, "td")
	if err != nil {
		t.Fatalf("get dir task: %v", err)
	}
	if td.Mode != "local-path" {
		t.Errorf("dir task mode = %q, want local-path（dir 项目存量任务回填）", td.Mode)
	}
}

// TestMigration0013_TaskMode_FreshDB 验证全新建库路径（tasks 1.2）：全量 migration 后
// CreateTask 显式写 mode 逐字持久化；列 DEFAULT 'worktree' 覆盖未显式写入的插入
//（这也是 dir 任务 MUST 显式写 local-path、不得依赖 DEFAULT 的原因）。
func TestMigration0013_TaskMode_FreshDB(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if err := db.CreateProject(ctx, "pr", "r", "/tmp/r", "main", "repo"); err != nil {
		t.Fatalf("create repo project: %v", err)
	}
	if err := db.CreateProject(ctx, "pd", "d", "/tmp/d", "", "dir"); err != nil {
		t.Fatalf("create dir project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "tr", ProjectID: "pr", Name: "repo task", Branch: "ocdeck/a",
		Status: "suspended", WorktreePath: "/tmp/wt", BaseRef: "refs/heads/main", Mode: "worktree"}); err != nil {
		t.Fatalf("create repo task: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "td", ProjectID: "pd", Name: "dir task", Branch: "",
		Status: "suspended", WorktreePath: "/tmp/d", BaseRef: "", Mode: "local-path"}); err != nil {
		t.Fatalf("create dir task: %v", err)
	}

	if tr, err := db.GetTask(ctx, "tr"); err != nil {
		t.Fatalf("get repo task: %v", err)
	} else if tr.Mode != "worktree" {
		t.Errorf("repo task mode = %q, want worktree", tr.Mode)
	}
	if td, err := db.GetTask(ctx, "td"); err != nil {
		t.Fatalf("get dir task: %v", err)
	} else if td.Mode != "local-path" {
		t.Errorf("dir task mode = %q, want local-path", td.Mode)
	}

	// 列 DEFAULT：原始插入省略 mode 列 → worktree（与存量语义一致）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
		 VALUES ('tdef', 'pr', 'default task', 'b', 'suspended', '/tmp/wt2', 'refs/heads/main', 1, 1)`); err != nil {
		t.Fatalf("raw insert without mode: %v", err)
	}
	if tdef, err := db.GetTask(ctx, "tdef"); err != nil {
		t.Fatalf("get default-mode task: %v", err)
	} else if tdef.Mode != "worktree" {
		t.Errorf("default mode = %q, want worktree", tdef.Mode)
	}
}
