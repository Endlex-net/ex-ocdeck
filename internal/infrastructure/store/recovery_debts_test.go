package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestRecoveryDebts_MultiRowCRUDAndReopen 验证 G3-10/G3-8：真实 SQLite 下
// recovery_debts 复合主键 (task_id, session_name) 支持同任务多条 cleanup debt，
// 载荷（phase/tickets/reason/retryable/cause）跨 reopen 完整往返，按 task_id 整组删除。
func TestRecoveryDebts_MultiRowCRUDAndReopen(t *testing.T) {
	ctx := context.Background()
	// 自建 dir 以便跨 reopen 复用同一数据库文件（openTestDB 每次 TempDir）。
	dir := t.TempDir()
	db1, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedProjectTask(t, db1, "t1")

	upsert := func(db *DB, row RecoveryDebtRow) {
		t.Helper()
		if err := db.UpsertRecoveryDebt(ctx, row); err != nil {
			t.Fatalf("upsert %+v: %v", row, err)
		}
	}
	upsert(db1, RecoveryDebtRow{TaskID: "t1", SessionName: "ocdeck-t1-runtime", Phase: "cleanup_notice",
		Tickets: `["tk1"]`, Reason: "kill_failed", Retryable: true, Cause: "recovery budget exhausted", CreatedAt: 100})
	upsert(db1, RecoveryDebtRow{TaskID: "t1", SessionName: "ocdeck-t1-shell-1", Phase: "cleanup_notice",
		Tickets: `["tk2","tk3"]`, Reason: "reap_failed", Retryable: true, Cause: "recovery budget exhausted", CreatedAt: 101})
	upsert(db1, RecoveryDebtRow{TaskID: "t1", SessionName: "ocdeck-t1-runtime", Phase: "cleanup_notice",
		Tickets: `["tk1","tk1b"]`, Reason: "kill_failed", Retryable: true, Cause: "latest wins", CreatedAt: 102})
	// 同键 upsert 覆盖：runtime 行仍只有一条，tickets 为最新值。
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rows, err := db2.ListRecoveryDebts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2 (runtime + shell-1)", len(rows))
	}
	bySession := map[string]RecoveryDebtRow{}
	for _, r := range rows {
		bySession[r.SessionName] = r
	}
	rt, ok := bySession["ocdeck-t1-runtime"]
	if !ok {
		t.Fatal("runtime debt row missing")
	}
	if rt.Tickets != `["tk1","tk1b"]` || rt.Cause != "latest wins" || !rt.Retryable || rt.Reason != "kill_failed" {
		t.Errorf("runtime row payload drifted: %+v", rt)
	}
	sh, ok := bySession["ocdeck-t1-shell-1"]
	if !ok {
		t.Fatal("shell debt row missing")
	}
	if sh.Tickets != `["tk2","tk3"]` || sh.Reason != "reap_failed" {
		t.Errorf("shell row payload drifted: %+v", sh)
	}

	// complete 行占空串位，与 cleanup 行共存。
	upsert(db2, RecoveryDebtRow{TaskID: "t1", SessionName: "", Phase: "complete",
		Tickets: "[]", Cause: "recovery budget exhausted", CreatedAt: 103})
	if rows, _ = db2.ListRecoveryDebts(ctx); len(rows) != 3 {
		t.Fatalf("rows=%d want 3 after complete row", len(rows))
	}
	// 按 task_id 整组删除。
	if err := db2.DeleteRecoveryDebt(ctx, "t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows, _ = db2.ListRecoveryDebts(ctx); len(rows) != 0 {
		t.Fatalf("rows=%d want 0 after delete", len(rows))
	}

	// 非法 phase / complete 行带 session_name 拒绝。
	if err := db2.UpsertRecoveryDebt(ctx, RecoveryDebtRow{TaskID: "t1", SessionName: "s", Phase: "bogus", CreatedAt: 1}); err == nil {
		t.Error("invalid phase must be rejected")
	}
	if err := db2.UpsertRecoveryDebt(ctx, RecoveryDebtRow{TaskID: "t1", SessionName: "s", Phase: "complete", CreatedAt: 1}); err == nil {
		t.Error("complete row with session_name must be rejected")
	}
}

// TestRecoveryDebts_GetSingleRow 验证 GetRecoveryDebt 单行读取与缺行错误。
func TestRecoveryDebts_GetSingleRow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedProjectTask(t, db, "t1")
	if _, err := db.GetRecoveryDebt(ctx, "t1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing row err=%v want sql.ErrNoRows", err)
	}
	if err := db.UpsertRecoveryDebt(ctx, RecoveryDebtRow{TaskID: "t1", SessionName: "", Phase: "complete",
		Tickets: "[]", Cause: "c", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetRecoveryDebt(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Phase != "complete" || row.Cause != "c" {
		t.Errorf("row=%+v", row)
	}
}

// TestCasActivationIfNoRecoveryDebt 验证 G3-18 准入原子事务：存在 debt 行 →
// ErrRecoveryDebtPresent 且状态零修改；无 debt → CAS 迁移；fromStatus 失配零修改。
func TestCasActivationIfNoRecoveryDebt(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedProjectTask(t, db, "t1")

	// 无 debt：suspended → activating 成功。
	res, err := db.CasActivationIfNoRecoveryDebt(ctx, "t1", "suspended", "activating")
	if err != nil || !res.Matched {
		t.Fatalf("res=%+v err=%v want matched", res, err)
	}

	// 有 debt：拒绝 + 状态零修改。
	if err := db.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
		TaskID: "t1", Phase: "complete", Tickets: "[]", Cause: "c", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateTaskStatus(ctx, "t1", "suspended", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CasActivationIfNoRecoveryDebt(ctx, "t1", "suspended", "activating"); !errors.Is(err, ErrRecoveryDebtPresent) {
		t.Fatalf("err=%v want ErrRecoveryDebtPresent", err)
	}
	if row, _ := db.GetTask(ctx, "t1"); row.Status != "suspended" {
		t.Fatalf("status=%s want suspended (zero-modify on reject)", row.Status)
	}

	// fromStatus 失配（无 debt，实际 suspended、传 active）：零修改返回 !Matched。
	if err := db.DeleteRecoveryDebt(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	res, err = db.CasActivationIfNoRecoveryDebt(ctx, "t1", "active", "activating")
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := db.GetTask(ctx, "t1"); row.Status != "suspended" || res.Matched {
		t.Fatalf("res=%+v status=%s want zero-modify on fromStatus mismatch", res, row.Status)
	}
}

// TestCompleteRecoveryFailureAndClearDebts_SingleTx 验证 G3-18 单事务：activating
// 任务 Complete（suspended+last_error+清 env_snapshot）与 debt 删除同事务；CAS
// 失配（非 activating）也删 debt。
func TestCompleteRecoveryFailureAndClearDebts_SingleTx(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	seedProjectTask(t, db, "t1")
	if _, err := db.UpdateTaskStatus(ctx, "t1", "activating", nil); err != nil {
		t.Fatal(err)
	}
	for _, sn := range []string{"ocdeck-t1-runtime", "ocdeck-t1-shell-1"} {
		if err := db.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
			TaskID: "t1", SessionName: sn, Phase: "cleanup_notice",
			Tickets: "[]", Reason: "kill_failed", Retryable: true, Cause: "c", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cause := "recovery budget exhausted"
	res, err := db.CompleteRecoveryFailureAndClearDebts(ctx, "t1", strPtr(cause))
	if err != nil || !res.Matched {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.Status != "suspended" || row.LastError.String != cause || row.EnvSnapshot.Valid {
		t.Fatalf("row=%+v want suspended+cause+cleared snapshot", row)
	}
	if rows, _ := db.ListRecoveryDebts(ctx); len(rows) != 0 {
		t.Fatalf("debts=%d want 0 (deleted in same tx)", len(rows))
	}

	// CAS 失配（已 suspended）也删 debt（服从 DB 最新状态）。
	if err := db.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
		TaskID: "t1", Phase: "complete", Tickets: "[]", Cause: "stale", CreatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	res, err = db.CompleteRecoveryFailureAndClearDebts(ctx, "t1", strPtr(cause))
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched {
		t.Fatal("want !Matched on CAS mismatch")
	}
	if rows, _ := db.ListRecoveryDebts(ctx); len(rows) != 0 {
		t.Fatalf("debts=%d want 0 (deleted even on CAS mismatch)", len(rows))
	}
}
