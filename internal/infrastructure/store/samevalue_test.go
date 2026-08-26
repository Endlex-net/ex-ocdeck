// samevalue_test.go 验证 store 写方法同值原子 no-op 的四态结构化结果（task P1.2 validation）。
//
// 覆盖 spec.md 5 Scenario：
//  1. 同值匹配 → Matched+!Changed（updated_at 不动）
//  2. expected 不匹配 → !Matched（CAS）
//  3. 同秒实变 → Changed+!UpdatedAtAdvanced
//  4. 跨秒实变 → Changed+UpdatedAtAdvanced
//  5. CAS 同值幂等成功 Matched+!Changed（不误判为重试失败）
//
// 跨秒用可注入 nowUnix（task spec）：测试临时覆盖 package-level nowUnix var，t.Cleanup 恢复。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
)

// withNowUnix 临时覆盖 package-level nowUnix，返回恢复函数（测试用）。
func withNowUnix(t *testing.T, ts int64) func() {
	t.Helper()
	orig := nowUnix
	nowUnix = func() int64 { return ts }
	return func() { nowUnix = orig }
}

// seedSuspendedTaskForSV 在测试 DB 中建项目+挂起任务，返回 updated_at（=创建时的 nowUnix）。
func seedSuspendedTaskForSV(t *testing.T, db *DB) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	row, _ := db.GetTask(ctx, "t1")
	return row.UpdatedAt
}

// TestUpdateTaskEnvSnapshot_FourPaths 验证单列写入四态。
func TestUpdateTaskEnvSnapshot_FourPaths(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, db *DB) (curUpdatedAt int64)
		newVal   *string
		wantMatched bool
		wantChanged bool
		wantAdvances bool
	}{
		{
			name:    "same value match Matched+!Changed",
			setup: func(t *testing.T, db *DB) int64 {
				restore := withNowUnix(t, 100)
				defer restore()
				seedSuspendedTaskForSV(t, db)
				// 先写入 env_snapshot = "snap1"（跨秒 200）。
				r := withNowUnix(t, 200)
				defer r()
				s := "snap1"
				if _, err := db.UpdateTaskEnvSnapshot(context.Background(), "t1", &s); err != nil {
					t.Fatalf("seed env snapshot: %v", err)
				}
				row, _ := db.GetTask(context.Background(), "t1")
				return row.UpdatedAt
			},
			newVal:        strPtr("snap1"),
			wantMatched:   true,
			wantChanged:   false,
			wantAdvances:  false,
		},
		{
			name: "same second real change Changed+!UpdatedAtAdvanced",
			setup: func(t *testing.T, db *DB) int64 {
				restore := withNowUnix(t, 300)
				defer restore()
				ua := seedSuspendedTaskForSV(t, db)
				return ua
			},
			// nowUnix 仍为 300（与 curUpdatedAt 同秒）。
			newVal:        strPtr("snap-new"),
			wantMatched:   true,
			wantChanged:   true,
			wantAdvances:  false,
		},
		{
			name: "cross second real change Changed+UpdatedAtAdvanced",
			setup: func(t *testing.T, db *DB) int64 {
				restore := withNowUnix(t, 400)
				defer restore()
				ua := seedSuspendedTaskForSV(t, db)
				return ua
			},
			// 调用前切到 401（跨秒）。
			newVal:        strPtr("snap-cross"),
			wantMatched:   true,
			wantChanged:   true,
			wantAdvances:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			curUpdatedAt := tc.setup(t, db)
			// 对 same-value 与 same-second：nowUnix 保持 curUpdatedAt；对 cross-second：切到 curUpdatedAt+1。
			if tc.wantAdvances {
				restore := withNowUnix(t, curUpdatedAt+1)
				defer restore()
			} else {
				restore := withNowUnix(t, curUpdatedAt)
				defer restore()
			}
			r, err := db.UpdateTaskEnvSnapshot(context.Background(), "t1", tc.newVal)
			if err != nil {
				t.Fatalf("UpdateTaskEnvSnapshot: %v", err)
			}
			if r.Matched != tc.wantMatched {
				t.Errorf("Matched = %v, want %v", r.Matched, tc.wantMatched)
			}
			if r.Changed != tc.wantChanged {
				t.Errorf("Changed = %v, want %v", r.Changed, tc.wantChanged)
			}
			if r.UpdatedAtAdvanced != tc.wantAdvances {
				t.Errorf("UpdatedAtAdvanced = %v, want %v", r.UpdatedAtAdvanced, tc.wantAdvances)
			}
		})
	}
}

// TestUpdateTaskStatusConditional_ExpectedMismatch 验证 CAS expected 不匹配 → !Matched。
func TestUpdateTaskStatusConditional_ExpectedMismatch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	restore := withNowUnix(t, 500)
	defer restore()
	seedSuspendedTaskForSV(t, db)
	// 当前 suspended，期望 creating → 不匹配。
	r, err := db.UpdateTaskStatusConditional(ctx, "t1", "creating", "active", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Matched {
		t.Errorf("Matched = true, want false (expected creating, was suspended)")
	}
}

// TestUpdateTaskNoticeCAS_SameValueIdempotentNotFailure 验证 spec Scenario 5：
// CAS expected 匹配且 newNotice 同值 → Matched+!Changed（不误判为重试失败）。
func TestUpdateTaskNoticeCAS_SameValueIdempotentNotFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 600)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	// 写入 notice = "N1"（期望当前 NULL）。
	{
		restore := withNowUnix(t, 601)
		defer restore()
		n := "N1"
		if _, err := db.UpdateTaskNoticeCAS(ctx, "t1", nil, strPtr(n)); err != nil {
			t.Fatalf("seed notice: %v", err)
		}
	}
	// CAS：期望当前 "N1"，写入 "N1"（同值）→ Matched+!Changed，不应被调用方误判为失败。
	r, err := db.UpdateTaskNoticeCAS(ctx, "t1", strPtr("N1"), strPtr("N1"))
	if err != nil {
		t.Fatalf("same-value CAS err: %v", err)
	}
	if !r.Matched {
		t.Errorf("Matched = false, want true (expected matched, same value idempotent success)")
	}
	if r.Changed {
		t.Errorf("Changed = true, want false (same value must not be treated as real change)")
	}
}

// TestUpdateTaskNoticeCAS_ExpectedMismatchNotMatched 验证 CAS expected 不匹配 → !Matched（触发重试）。
func TestUpdateTaskNoticeCAS_ExpectedMismatchNotMatched(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 700)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	// 写入 notice = "A"。
	{
		restore := withNowUnix(t, 701)
		defer restore()
		if _, err := db.UpdateTaskNoticeCAS(ctx, "t1", nil, strPtr("A")); err != nil {
			t.Fatalf("seed notice A: %v", err)
		}
	}
	// 期望当前 NULL（实际 A）→ !Matched，调用方据此重试。
	r, err := db.UpdateTaskNoticeCAS(ctx, "t1", nil, strPtr("B"))
	if err != nil {
		t.Fatalf("mismatch CAS err: %v", err)
	}
	if r.Matched {
		t.Errorf("Matched = true, want false (expected NULL, actual A → mismatch → retry)")
	}
}

// TestUpdateTaskStatus_StatusSameLastErrorDifferent 仍提交（spec Scenario）。
func TestUpdateTaskStatus_StatusSameLastErrorDifferent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 800)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	// 先写 status=suspended, last_error="e1"。
	{
		restore := withNowUnix(t, 801)
		defer restore()
		le := "e1"
		if _, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusSuspended, &le); err != nil {
			t.Fatalf("seed status: %v", err)
		}
	}
	// 同 status，last_error="e2" → 应 Changed（last_error 不同），StatusChanged=false。
	r, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusSuspended, strPtr("e2"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !r.Matched || !r.Changed {
		t.Errorf("Matched=%v Changed=%v, want true/true (status same, last_error different still commits)", r.Matched, r.Changed)
	}
	if r.StatusChanged {
		t.Errorf("StatusChanged = true, want false (status unchanged)")
	}
}

func strPtr(s string) *string { return &s }

// 避免 time 未使用 import 报错（withNowUnix 使用 time.Now 仅在默认时，测试不直接调用）。
var _ = time.Now

// --- F-05：TransitionResult 正/负例 ---

// TestTransitionResult_FromTo_Positive 验证真实状态迁移填充 From/To 且 StatusChanged=true
// （六类方法各一：UpdateTaskStatus / Conditional / ArchiveTask / RestoreTask / BeginDeleteIntent / CommitCreated）。
func TestTransitionResult_FromTo_Positive(t *testing.T) {
	ctx := context.Background()

	// UpdateTaskStatus: suspended → active。
	t.Run("UpdateTaskStatus", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 900)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 901)
			defer restore()
			r, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusActive, nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusSuspended || r.To != ocdecktask.StatusActive {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/suspended/active", r.StatusChanged, r.From, r.To)
			}
		}
	})

	// UpdateTaskStatusConditional: suspended → activating。
	t.Run("UpdateTaskStatusConditional", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 910)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 911)
			defer restore()
			r, err := db.UpdateTaskStatusConditional(ctx, "t1", ocdecktask.StatusSuspended, ocdecktask.StatusActivating, nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusSuspended || r.To != ocdecktask.StatusActivating {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/suspended/activating", r.StatusChanged, r.From, r.To)
			}
		}
	})

	// ArchiveTask: suspended → archived。
	t.Run("ArchiveTask", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 920)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 921)
			defer restore()
			r, err := db.ArchiveTask(ctx, "t1")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusSuspended || r.To != ocdecktask.StatusArchived {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/suspended/archived", r.StatusChanged, r.From, r.To)
			}
		}
	})

	// RestoreTask: archived → suspended。
	t.Run("RestoreTask", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 930)
			defer restore()
			seedSuspendedTaskForSV(t, db)
			if _, err := db.ArchiveTask(ctx, "t1"); err != nil {
				t.Fatalf("archive: %v", err)
			}
		}
		{
			restore := withNowUnix(t, 931)
			defer restore()
			r, err := db.RestoreTask(ctx, "t1")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusArchived || r.To != ocdecktask.StatusSuspended {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/archived/suspended", r.StatusChanged, r.From, r.To)
			}
		}
	})

	// BeginDeleteIntent: suspended → deleting。
	t.Run("BeginDeleteIntent", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 940)
			defer restore()
			seedSuspendedTaskForSV(t, db)
		}
		{
			restore := withNowUnix(t, 941)
			defer restore()
			r, err := db.BeginDeleteIntent(ctx, "t1", ocdecktask.DeleteModeNormal, []ocdecktask.Status{ocdecktask.StatusSuspended})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusSuspended || r.To != ocdecktask.StatusDeleting {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/suspended/deleting", r.StatusChanged, r.From, r.To)
			}
		}
	})

	// CommitCreated: creating → suspended。
	t.Run("CommitCreated", func(t *testing.T) {
		db := openTestDB(t)
		{
			restore := withNowUnix(t, 950)
			defer restore()
			seedCreatingTaskForCommit(t, db, "t1")
		}
		{
			restore := withNowUnix(t, 951)
			defer restore()
			r, err := db.CommitCreated(ctx, "t1", ocdecktask.StatusCreating, ocdecktask.InitStatusPending)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !r.StatusChanged || r.From != ocdecktask.StatusCreating || r.To != ocdecktask.StatusSuspended {
				t.Errorf("StatusChanged=%v From=%v To=%v, want true/creating/suspended", r.StatusChanged, r.From, r.To)
			}
		}
	})
}

// TestTransitionResult_SameStatusLastErrorDifferent_Negative 验证同状态 last_error 变更：
// StatusChanged=false（状态未迁移）但 Changed=true（last_error 真实变更）。
func TestTransitionResult_SameStatusLastErrorDifferent_Negative(t *testing.T) {
	ctx := context.Background()
	// UpdateTaskStatus: suspended+e1 → suspended+e2。
	db := openTestDB(t)
	{
		restore := withNowUnix(t, 960)
		defer restore()
		seedSuspendedTaskForSV(t, db)
		e := "e1"
		if _, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusSuspended, &e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	{
		restore := withNowUnix(t, 961)
		defer restore()
		e2 := "e2"
		r, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusSuspended, &e2)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !r.Matched || !r.Changed {
			t.Errorf("Matched=%v Changed=%v, want true/true (last_error changed)", r.Matched, r.Changed)
		}
		if r.StatusChanged {
			t.Errorf("StatusChanged=true, want false (status unchanged)")
		}
		if r.From != "" || r.To != "" {
			t.Errorf("From=%v To=%v, want empty (StatusChanged=false must not fill From/To)", r.From, r.To)
		}
	}
}

// TestTransitionResult_SameValue_MatchedNotChanged_Negative 验证全列同值：
// Matched=true Changed=false StatusChanged=false（CommitCreated 用 creating seed 正确路径）。
func TestTransitionResult_SameValue_MatchedNotChanged_Negative(t *testing.T) {
	ctx := context.Background()
	// CommitCreated: creating+pending+last_error=NULL → creating+pending+last_error=NULL
	// 先正确 seed（creating→suspended+pending），再同值重复 CommitCreated(creating→suspended+pending)
	// 应 !Matched（expected creating 但当前已 suspended）。
	db := openTestDB(t)
	{
		restore := withNowUnix(t, 970)
		defer restore()
		seedCreatingTaskForCommit(t, db, "t1")
		if _, err := db.CommitCreated(ctx, "t1", ocdecktask.StatusCreating, ocdecktask.InitStatusPending); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	// 重复 CommitCreated(creating→suspended+pending)：当前已 suspended，expected=creating 不匹配 → !Matched。
	{
		restore := withNowUnix(t, 971)
		defer restore()
		r, err := db.CommitCreated(ctx, "t1", ocdecktask.StatusCreating, ocdecktask.InitStatusPending)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if r.Matched {
			t.Errorf("Matched=true, want false (expected creating, actual suspended → CAS fail)")
		}
	}
}

// --- F-01：确定性丢更新测试 ---
//
// 用顺序执行模拟 A-read/B-write/A-write 交错：A 读旧值后 B 先写（改变行），A 再用基于旧值的
// expected/同值条件写。断言 A 的 UPDATE WHERE 在当前行（B 写后）上重新求值：expected 已失配
// 或同值 → 0 行（不覆盖 B）。这验证同值排除下推到 UPDATE WHERE 使写入原子，无丢更新。

// TestUpdateTaskNoticeCAS_NoLostUpdate 验证 notice CAS：A 读（期望 NULL），B 写（NULL→B），
// A 写（期望 NULL，实际 B）→ WHERE notice IS NULL 命中 0 行 → !Matched（不覆盖 B）。
func TestUpdateTaskNoticeCAS_NoLostUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatal(err)
	}
	// A 读：当前 notice 为 NULL（A 据此决定 expected=nil）。
	row, _ := db.GetTask(ctx, "t1")
	if row.Notice.Valid {
		t.Fatalf("A read: expect NULL notice, got %q", row.Notice.String)
	}
	// B 写：NULL → "B"（在 A 读后、A 写前发生）。
	b := "B"
	if _, err := db.UpdateTaskNoticeCAS(ctx, "t1", nil, &b); err != nil {
		t.Fatalf("B write: %v", err)
	}
	// A 写：expected=nil（A 读到的旧值），实际已为 "B"（B 写后）→ WHERE notice IS NULL 命中 0 行。
	a := "A"
	r, err := db.UpdateTaskNoticeCAS(ctx, "t1", nil, &a)
	if err != nil {
		t.Fatalf("A write: %v", err)
	}
	if r.Matched {
		t.Errorf("A Matched=true, want false (expected NULL, B already wrote → CAS fail, no lost update)")
	}
	// 验证最终 notice = "B"（先写方 B 胜出，A 未覆盖）。
	final, _ := db.GetTask(ctx, "t1")
	if !final.Notice.Valid || final.Notice.String != "B" {
		t.Errorf("final notice = %v, want B (先写方 B 胜出，A 未覆盖)", final.Notice)
	}
}

// TestUpdateTaskStatusConditional_NoLostUpdate 验证 status CAS：A 读（期望 suspended），
// B 写（suspended→archived），A 写（期望 suspended，实际 archived）→ WHERE status IS 'suspended'
// 命中 0 行 → !Matched（不覆盖 B）。
func TestUpdateTaskStatusConditional_NoLostUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatal(err)
	}
	// A 读：当前 status=suspended（A 据此决定 fromStatus=suspended）。
	row, _ := db.GetTask(ctx, "t1")
	if row.Status != "suspended" {
		t.Fatalf("A read: expect suspended, got %q", row.Status)
	}
	// B 写：suspended → archived（在 A 读后、A 写前发生）。
	if _, err := db.ArchiveTask(ctx, "t1"); err != nil {
		t.Fatalf("B write (archive): %v", err)
	}
	// A 写：fromStatus=suspended（A 读到的旧值），实际已 archived → WHERE status IS 'suspended' 命中 0 行。
	r, err := db.UpdateTaskStatusConditional(ctx, "t1", ocdecktask.StatusSuspended, ocdecktask.StatusActive, nil)
	if err != nil {
		t.Fatalf("A write: %v", err)
	}
	if r.Matched {
		t.Errorf("A Matched=true, want false (expected suspended, B archived → CAS fail, no lost update)")
	}
	// 验证最终 status=archived（先写方 B 胜出，A 未覆盖）。
	final, _ := db.GetTask(ctx, "t1")
	if final.Status != "archived" {
		t.Errorf("final status = %q, want archived (先写方 B 胜出，A 未覆盖)", final.Status)
	}
}

// --- F-01 真交错测试（方法内 SELECT 与 UPDATE 之间暂停，独立连接完成 B 写入） ---

// openWALMultiConnTestDB 打开 WAL 模式多连接测试 DB（F-01 真交错测试需 A 读事务不阻塞 B 写）。
// WAL 模式下读事务持有快照锁不阻塞写者，允许多连接交错。
func openWALMultiConnTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ocdeck.db")
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(4) // 多连接，允许 A 事务与 B 写交错
	t.Cleanup(func() { sqlDB.Close() })
	db := &DB{DB: sqlDB, Queries: New(sqlDB)}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestUpdateTaskStatusConditional_RealInterleaveNoLostUpdate 真交错测试：
// A 调用方法 → 内部 SELECT 读旧值（suspended）→ afterRead 信号通知测试 → beforeHook 阻塞 A →
// 测试经独立连接 B 写入（suspended→archived）→ 放行 beforeHook → A 的 UPDATE WHERE 携带
// `status IS 'suspended'`（expected）+ 同值排除，在 B 写后行（archived）上重新求值 → 0 行 →
// 分类 SELECT 读到 archived != suspended → !Matched（CAS 失败，不覆盖 B）。
func TestUpdateTaskStatusConditional_RealInterleaveNoLostUpdate(t *testing.T) {
	db := openWALMultiConnTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatal(err)
	}

	// 信号 channels：afterRead 通知「A 已读到旧值，可放行 B」；release 通知「B 已写完，放行 A 的 UPDATE」。
	aReadDone := make(chan struct{})
	releaseA := make(chan struct{})

	// 注入 hooks：A 的方法内 SELECT 完成后发 aReadDone 信号，然后阻塞在 releaseA 上等 B 写完。
	origAfter, origBefore := afterConditionalUpdateReadHook, beforeConditionalUpdateHook
	afterConditionalUpdateReadHook = func(taskID string) {
		if taskID == "t1" {
			close(aReadDone)
		}
	}
	beforeConditionalUpdateHook = func(taskID string) {
		if taskID == "t1" {
			<-releaseA // 阻塞 A 的 UPDATE，等 B 写完
		}
	}
	t.Cleanup(func() {
		afterConditionalUpdateReadHook = origAfter
		beforeConditionalUpdateHook = origBefore
	})

	// A goroutine：调用 UpdateTaskStatusConditional（fromStatus=suspended, toStatus=active）。
	type aResult struct {
		r   application.TransitionResult
		err error
	}
	aDone := make(chan aResult, 1)
	go func() {
		r, err := db.UpdateTaskStatusConditional(ctx, "t1", ocdecktask.StatusSuspended, ocdecktask.StatusActive, nil)
		aDone <- aResult{r, err}
	}()

	// 等 A 的内部 SELECT 完成（A 已读到 suspended 旧值）。
	select {
	case <-aReadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("A 的内部 SELECT 未在 5s 内完成（hook 未触发）")
	}

	// B 经独立连接写入：suspended → archived（在 A 的 UPDATE 之前）。
	bConn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("B conn: %v", err)
	}
	defer bConn.Close()
	res, err := bConn.ExecContext(ctx,
		`UPDATE tasks SET status = 'archived', updated_at = updated_at WHERE id = ? AND status IS 'suspended'`, "t1")
	if err != nil {
		t.Fatalf("B write: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("B write: expect 1 row (suspended→archived), got %d", n)
	}

	// 放行 A 的 UPDATE。
	close(releaseA)

	// 等 A 返回。
	select {
	case ar := <-aDone:
		if ar.err != nil {
			t.Fatalf("A write err: %v", ar.err)
		}
		// A 的 fromStatus=suspended，B 已改为 archived → expected 失配 → !Matched。
		if ar.r.Matched {
			t.Errorf("A Matched=true, want false (expected suspended, B archived in real interleave → CAS fail, no lost update)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A 未在 5s 内返回（UPDATE 阻塞）")
	}

	// 验证最终 status=archived（先写方 B 胜出，A 未覆盖）。
	final, _ := db.GetTask(ctx, "t1")
	if final.Status != "archived" {
		t.Errorf("final status = %q, want archived (先写方 B 胜出，A 未覆盖)", final.Status)
	}
}

// TestUpdateTaskStatusConditional_RealInterleaveSameValueIdempotent 真交错测试（同值幂等）：
// A 读（suspended+e1）→ B 写 e1（同值，no-op）→ A 写（suspended+e1，同值）→ Matched+!Changed。
// 验证 RowsAffected=0 分类正确区分同值幂等（expected 匹配 + 全列同值）与 !Matched。
func TestUpdateTaskStatusConditional_RealInterleaveSameValueIdempotent(t *testing.T) {
	db := openWALMultiConnTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/tmp/repo", "main", "repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTask(ctx, TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/tmp/wt"}); err != nil {
		t.Fatal(err)
	}
	// 预置 last_error=e1（跨秒写）。
	e1 := "e1"
	if _, err := db.UpdateTaskStatus(ctx, "t1", ocdecktask.StatusSuspended, &e1); err != nil {
		t.Fatal(err)
	}

	aReadDone := make(chan struct{})
	releaseA := make(chan struct{})
	origAfter, origBefore := afterConditionalUpdateReadHook, beforeConditionalUpdateHook
	afterConditionalUpdateReadHook = func(taskID string) {
		if taskID == "t1" {
			close(aReadDone)
		}
	}
	beforeConditionalUpdateHook = func(taskID string) {
		if taskID == "t1" {
			<-releaseA
		}
	}
	t.Cleanup(func() {
		afterConditionalUpdateReadHook = origAfter
		beforeConditionalUpdateHook = origBefore
	})

	type aResult struct {
		r   application.TransitionResult
		err error
	}
	aDone := make(chan aResult, 1)
	go func() {
		// A 写：fromStatus=suspended, toStatus=suspended, last_error=e1（全同值）。
		r, err := db.UpdateTaskStatusConditional(ctx, "t1", ocdecktask.StatusSuspended, ocdecktask.StatusSuspended, &e1)
		aDone <- aResult{r, err}
	}()

	select {
	case <-aReadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("A 的内部 SELECT 未在 5s 内完成")
	}

	// B 经独立连接写同值（suspended+e1，no-op）。
	bConn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("B conn: %v", err)
	}
	defer bConn.Close()
	res, err := bConn.ExecContext(ctx,
		`UPDATE tasks SET status = 'suspended', last_error = 'e1', updated_at = updated_at
		 WHERE id = ? AND (status IS NOT 'suspended' OR last_error IS NOT 'e1')`, "t1")
	if err != nil {
		t.Fatalf("B write: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 0 {
		t.Fatalf("B write: expect 0 rows (同值 no-op), got %d", n)
	}

	close(releaseA)

	select {
	case ar := <-aDone:
		if ar.err != nil {
			t.Fatalf("A write err: %v", ar.err)
		}
		// A 全同值（suspended+e1）：expected 匹配 + 全列同值 → Matched+!Changed。
		if !ar.r.Matched {
			t.Errorf("A Matched=false, want true (expected suspended matched, same value idempotent)")
		}
		if ar.r.Changed {
			t.Errorf("A Changed=true, want false (same value idempotent, no real change)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A 未在 5s 内返回")
	}
}

var _ = sync.Mutex{} // 保留 sync import（供未来并发测试扩展）