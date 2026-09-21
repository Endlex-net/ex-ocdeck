// task_permission_mode_at_start_test.go 验证启动权限事实窄端口（task-permission-mode
// tasks 2.1/2.2/2.3，design D5/D8）：0017 加列（存量 NULL）、回填幂等/仅 active+NULL/
// 不推进 updated_at、Set/Get roundtrip（NULL→nil、写失败零副作用）、
// UpdateTaskPermissionMode 单列条件更新四态（跨秒推进/同秒实变/同值幂等/未命中）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// startFactOf 直接读列（TaskRow 不扩展该列，D8）。
func startFactOf(t *testing.T, db *DB, taskID string) *string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT permission_mode_at_start FROM tasks WHERE id = ?`, taskID).Scan(&v); err != nil {
		t.Fatalf("read permission_mode_at_start %s: %v", taskID, err)
	}
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// tasksColumnExists 断言 tasks 表含指定列（0017 加列验证）。
func tasksColumnExists(t *testing.T, db *DB, col string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(tasks)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// TestMigration0017_PermissionModeAtStart_AddColumnNullable 验证 0017：新列存在且
// 存量行（迁移时插入）为 NULL。
func TestMigration0017_PermissionModeAtStart_AddColumnNullable(t *testing.T) {
	db := openTestDB(t)
	if !tasksColumnExists(t, db, "permission_mode_at_start") {
		t.Fatal("tasks.permission_mode_at_start column missing after migrations")
	}
	restore := withNowUnix(t, 100)
	defer restore()
	if ua := seedSuspendedTaskForSV(t, db); ua != 100 {
		t.Fatalf("seed updated_at = %d, want 100", ua)
	}
	if got := startFactOf(t, db, "t1"); got != nil {
		t.Errorf("permission_mode_at_start = %v, want NULL（仅加列，不回填）", *got)
	}
}

// TestBackfillPermissionModeAtStart 验证回填矩阵：仅 active 且 NULL 行按持久化
// permission_mode 回填；幂等；非 active/已有值行不动；全程不推进 updated_at。
func TestBackfillPermissionModeAtStart(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	restore := withNowUnix(t, 300)
	defer restore()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	seed := []TaskRow{
		{ID: "t-active-null", ProjectID: "p1", Name: "a", Branch: "b", Status: "active", WorktreePath: "/w1", PermissionMode: "ai-auto"},
		{ID: "t-active-kept", ProjectID: "p1", Name: "b", Branch: "b", Status: "active", WorktreePath: "/w2", PermissionMode: "ask"},
		{ID: "t-suspended-null", ProjectID: "p1", Name: "c", Branch: "b", Status: "suspended", WorktreePath: "/w3", PermissionMode: "all-approve"},
	}
	for _, r := range seed {
		if err := db.CreateTask(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.ID, err)
		}
	}
	// 预置 t-active-kept 已有启动事实（ 非 NULL 行 MUST NOT 被覆盖）。
	if err := db.SetPermissionModeAtStart(ctx, "t-active-kept", "all-approve"); err != nil {
		t.Fatalf("seed start fact: %v", err)
	}

	if err := db.BackfillPermissionModeAtStart(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := startFactOf(t, db, "t-active-null"); got == nil || *got != "ai-auto" {
		t.Errorf("t-active-null = %v, want ai-auto（active+NULL 按持久化值回填）", got)
	}
	if got := startFactOf(t, db, "t-active-kept"); got == nil || *got != "all-approve" {
		t.Errorf("t-active-kept = %v, want all-approve（非 NULL 不覆盖）", got)
	}
	if got := startFactOf(t, db, "t-suspended-null"); got != nil {
		t.Errorf("t-suspended-null = %v, want NULL（非 active 不回填）", *got)
	}
	// 幂等：二次回填零变化。
	if err := db.BackfillPermissionModeAtStart(ctx); err != nil {
		t.Fatalf("backfill #2: %v", err)
	}
	if got := startFactOf(t, db, "t-active-null"); got == nil || *got != "ai-auto" {
		t.Errorf("after idempotent backfill t-active-null = %v, want ai-auto", got)
	}
	// 不推进 updated_at。
	for _, r := range seed {
		row, err := db.GetTask(ctx, r.ID)
		if err != nil {
			t.Fatalf("get %s: %v", r.ID, err)
		}
		if row.UpdatedAt != 300 {
			t.Errorf("%s updated_at = %d, want 300（回填不推进）", r.ID, row.UpdatedAt)
		}
	}
}

// TestBackfillPermissionModeAtStart_DBError 原样返回 DB 错误（取消 ctx）。
func TestBackfillPermissionModeAtStart_DBError(t *testing.T) {
	db := openTestDB(t)
	failCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.BackfillPermissionModeAtStart(failCtx); err == nil {
		t.Fatal("want error on failed backfill, got nil")
	}
}

// TestSetGetPermissionModeAtStart_RoundTrip 验证写入/读回/NULL→nil/未命中/写失败零副作用
// 与全程不推进 updated_at。
func TestSetGetPermissionModeAtStart_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	var ua int64
	{
		restore := withNowUnix(t, 400)
		defer restore()
		ua = seedSuspendedTaskForSV(t, db)
	}
	// 初始 NULL → nil。
	if got, err := db.GetPermissionModeAtStart(ctx, "t1"); err != nil || got != nil {
		t.Fatalf("initial = (%v, %v), want (nil, nil)", got, err)
	}
	{
		restore := withNowUnix(t, 500) // 跨秒 now，写入 MUST NOT 推进
		defer restore()
		if err := db.SetPermissionModeAtStart(ctx, "t1", "ai-auto"); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	if got, err := db.GetPermissionModeAtStart(ctx, "t1"); err != nil || got == nil || *got != "ai-auto" {
		t.Fatalf("after set = (%v, %v), want ai-auto", got, err)
	}
	if row, _ := db.GetTask(ctx, "t1"); row.UpdatedAt != ua {
		t.Errorf("updated_at = %d, want %d（启动事实写入不推进）", row.UpdatedAt, ua)
	}
	// 未命中行：Set/Get 均报错。
	if err := db.SetPermissionModeAtStart(ctx, "missing", "ask"); err == nil {
		t.Error("set missing: want error, got nil")
	}
	if got, err := db.GetPermissionModeAtStart(ctx, "missing"); err == nil || got != nil {
		t.Errorf("get missing = (%v, %v), want (nil, err)", got, err)
	}
	// 写失败（取消 ctx）零副作用：列值保持原值。
	failCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.SetPermissionModeAtStart(failCtx, "t1", "ask"); err == nil {
		t.Fatal("set failure: want error, got nil")
	}
	if got := startFactOf(t, db, "t1"); got == nil || *got != "ai-auto" {
		t.Errorf("after failed set = %v, want ai-auto（写失败零副作用）", got)
	}
}

// TestUpdateTaskPermissionMode_FourStates 验证单列条件更新四态（tasks 2.3）：
// 跨秒推进 / 同秒实变（数值不变）/ 同值幂等 / 未命中。
func TestUpdateTaskPermissionMode_FourStates(t *testing.T) {
	t.Run("跨秒真实变更推进", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		{
			restore := withNowUnix(t, 600)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 601)
			defer restore()
			res, err := db.UpdateTaskPermissionMode(ctx, "t1", "ai-auto")
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if !res.Matched || !res.Changed || !res.UpdatedAtAdvanced {
				t.Fatalf("res = %+v, want Matched+Changed+UpdatedAtAdvanced", res)
			}
		}
		row, _ := db.GetTask(ctx, "t1")
		if row.PermissionMode != "ai-auto" || row.UpdatedAt != 601 {
			t.Errorf("mode/updated_at = %q/%d, want ai-auto/601", row.PermissionMode, row.UpdatedAt)
		}
	})
	t.Run("同秒真实变更提交但数值不变", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		{
			restore := withNowUnix(t, 700)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 700)
			defer restore()
			res, err := db.UpdateTaskPermissionMode(ctx, "t1", "ai-auto")
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if !res.Matched || !res.Changed || res.UpdatedAtAdvanced {
				t.Fatalf("res = %+v, want Matched+Changed+!UpdatedAtAdvanced（同秒实变）", res)
			}
		}
		row, _ := db.GetTask(ctx, "t1")
		if row.PermissionMode != "ai-auto" || row.UpdatedAt != 700 {
			t.Errorf("mode/updated_at = %q/%d, want ai-auto/700", row.PermissionMode, row.UpdatedAt)
		}
	})
	t.Run("同值保存零副作用", func(t *testing.T) {
		db := openTestDB(t)
		ctx := context.Background()
		var ua int64
		{
			restore := withNowUnix(t, 800)
			defer restore()
			ua = seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 801)
			defer restore()
			res, err := db.UpdateTaskPermissionMode(ctx, "t1", "ask")
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if !res.Matched || res.Changed || res.UpdatedAtAdvanced {
				t.Fatalf("res = %+v, want Matched+!Changed+!UpdatedAtAdvanced（同值幂等）", res)
			}
		}
		if row, _ := db.GetTask(ctx, "t1"); row.UpdatedAt != ua {
			t.Errorf("updated_at = %d, want %d（同值不推进）", row.UpdatedAt, ua)
		}
	})
	t.Run("未命中 Matched=false", func(t *testing.T) {
		db := openTestDB(t)
		res, err := db.UpdateTaskPermissionMode(context.Background(), "missing", "ask")
		if err != nil {
			t.Fatalf("update missing: %v", err)
		}
		if res.Matched || res.Changed {
			t.Errorf("res = %+v, want zero（行不存在）", res)
		}
	})
	t.Run("DB 错误原样返回", func(t *testing.T) {
		db := openTestDB(t)
		failCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := db.UpdateTaskPermissionMode(failCtx, "t1", "ask"); err == nil {
			t.Fatal("want error, got nil")
		} else if errors.Is(err, sql.ErrNoRows) {
			t.Errorf("err = %v, want infra error（非 ErrNoRows）", err)
		}
	})
}
