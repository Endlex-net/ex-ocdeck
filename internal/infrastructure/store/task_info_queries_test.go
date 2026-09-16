// task_info_queries_test.go 验证任务信息业务列单事务提交与改名意图元数据写清
// （task-info-editable tasks 1.2/1.3）。
//
// 覆盖 design D6 四态表（store 侧真实 DB + nowUnix 注入）：
//   - 业务列真实变化 + 清除 pending → Changed=true、updated_at 跨秒推进、意图同事务清除；
//   - 业务列同值 + 清除 pending → Changed=false、updated_at 不推进、意图同事务清除；
//   - 仅清除 pending / 仅设置 pending → Changed=false、updated_at 不推进；
//   - 同值 no-op、presence 语义（nil=不修改）、NULL 安全比较、跨秒/同秒 updated_at；
//   - 失败矩阵持久化侧（tasks 1.3）：提交失败意图保留、意图清除失败语义（取消 ctx 使
//     整个事务失败，断言零部分写入）。
package store

import (
	"context"
	"testing"

	"ocdeck/internal/application"
)

// seedTaskWithPending 建项目+任务并写入改名意图，返回写入意图后的 updated_at。
func seedTaskWithPending(t *testing.T, db *DB, taskID, pendingJSON string) int64 {
	t.Helper()
	ua := seedSuspendedTaskForSV(t, db)
	if _, err := db.SetTaskRenamePending(context.Background(), taskID, pendingJSON); err != nil {
		t.Fatalf("set rename pending: %v", err)
	}
	row, err := db.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !row.RenamePending.Valid || row.RenamePending.String != pendingJSON {
		t.Fatalf("seed pending = %+v, want %q", row.RenamePending, pendingJSON)
	}
	if row.UpdatedAt != ua {
		t.Fatalf("意图写入推进了 updated_at：got %d, want %d（D6：pending-only 不推进）", row.UpdatedAt, ua)
	}
	return row.UpdatedAt
}

// TestCommitTaskInfoUpdate_BusinessChangeCommitsAndClearsIntent 验证四态表第 2 行：
// 业务列真实变化 + 清除 pending → Changed=true、跨秒推进 updated_at、意图同事务清除、
// 业务列原子落账。
func TestCommitTaskInfoUpdate_BusinessChangeCommitsAndClearsIntent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1000)
		defer restore()
		seedTaskWithPending(t, db, "t1", `{"name":"n2","branch_old":"b","branch_new":"b2","env_snapshot":null}`)
	}
	{
		restore := withNowUnix(t, 1001) // 跨秒
		defer restore()
		name, branch := "n2", "b2"
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{Name: &name, Branch: &branch})
		if err != nil {
			t.Fatalf("CommitTaskInfoUpdate: %v", err)
		}
		if !res.Matched || !res.Changed || !res.UpdatedAtAdvanced {
			t.Fatalf("res = %+v, want Matched+Changed+UpdatedAtAdvanced", res)
		}
	}
	row, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.Name != "n2" || row.Branch != "b2" {
		t.Errorf("name/branch = %q/%q, want n2/b2（业务列落账）", row.Name, row.Branch)
	}
	if row.RenamePending.Valid {
		t.Errorf("rename_pending = %+v, want NULL（意图同事务清除）", row.RenamePending)
	}
	if row.UpdatedAt != 1001 {
		t.Errorf("updated_at = %d, want 1001（跨秒推进）", row.UpdatedAt)
	}
}

// TestCommitTaskInfoUpdate_SameSecondChangeNotAdvanced 验证同秒实变：Changed=true 且
// updated_at 不推进（秒精度语义）。
func TestCommitTaskInfoUpdate_SameSecondChangeNotAdvanced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1100)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	{
		restore := withNowUnix(t, 1100) // 与 updated_at 同秒
		defer restore()
		name := "n2"
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{Name: &name})
		if err != nil {
			t.Fatalf("CommitTaskInfoUpdate: %v", err)
		}
		if !res.Matched || !res.Changed || res.UpdatedAtAdvanced {
			t.Fatalf("res = %+v, want Matched+Changed+!UpdatedAtAdvanced（同秒实变不推进）", res)
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.Name != "n2" || row.UpdatedAt != 1100 {
		t.Errorf("name/updated_at = %q/%d, want n2/1100", row.Name, row.UpdatedAt)
	}
}

// TestCommitTaskInfoUpdate_SameValueClearsIntentOnly 验证四态表第 4 行：
// 业务列同值 + 清除 pending → Changed=false、updated_at 不推进、意图清除。
func TestCommitTaskInfoUpdate_SameValueClearsIntentOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1200)
		defer restore()
		if ua := seedTaskWithPending(t, db, "t1", `{"branch_old":"b","branch_new":"b"}`); ua != 1200 {
			t.Fatalf("seed updated_at = %d, want 1200", ua)
		}
	}
	{
		restore := withNowUnix(t, 1250) // 跨秒 now，但同值写 MUST NOT 推进
		defer restore()
		name := "task" // 与当前 name 同值
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{Name: &name})
		if err != nil {
			t.Fatalf("CommitTaskInfoUpdate: %v", err)
		}
		if !res.Matched || res.Changed || res.UpdatedAtAdvanced {
			t.Fatalf("res = %+v, want Matched+!Changed+!UpdatedAtAdvanced（同值幂等）", res)
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.Name != "task" || row.UpdatedAt != 1200 {
		t.Errorf("name/updated_at = %q/%d, want task/1200（同值不推进）", row.Name, row.UpdatedAt)
	}
	if row.RenamePending.Valid {
		t.Errorf("rename_pending = %+v, want NULL（同事务清除意图）", row.RenamePending)
	}
}

// TestCommitTaskInfoUpdate_IntentOnlyClear 验证四态表第 3 行：全部字段 nil + pending 存在
// → 仅清除意图，业务列与 updated_at 均不动。
func TestCommitTaskInfoUpdate_IntentOnlyClear(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1300)
		defer restore()
		seedTaskWithPending(t, db, "t1", `{"branch_old":"b","branch_new":"b2"}`)
	}
	{
		restore := withNowUnix(t, 1301)
		defer restore()
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{})
		if err != nil {
			t.Fatalf("CommitTaskInfoUpdate: %v", err)
		}
		if !res.Matched || res.Changed || res.UpdatedAtAdvanced {
			t.Fatalf("res = %+v, want Matched+!Changed+!UpdatedAtAdvanced（仅清除意图）", res)
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.Name != "task" || row.Branch != "b" {
		t.Errorf("name/branch = %q/%q, want task/b（业务列不动）", row.Name, row.Branch)
	}
	if row.RenamePending.Valid {
		t.Errorf("rename_pending = %+v, want NULL", row.RenamePending)
	}
	if row.UpdatedAt != 1300 {
		t.Errorf("updated_at = %d, want 1300（意图清除不推进）", row.UpdatedAt)
	}
}

// TestCommitTaskInfoUpdate_PureNoop 验证全部字段 nil + 无 pending → 纯 no-op。
func TestCommitTaskInfoUpdate_PureNoop(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1400)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	{
		restore := withNowUnix(t, 1401)
		defer restore()
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{})
		if err != nil {
			t.Fatalf("CommitTaskInfoUpdate: %v", err)
		}
		if !res.Matched || res.Changed || res.UpdatedAtAdvanced {
			t.Fatalf("res = %+v, want Matched+!Changed+!UpdatedAtAdvanced", res)
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.UpdatedAt != 1400 || row.RenamePending.Valid {
		t.Errorf("updated_at/rename_pending = %d/%+v, want 1400/NULL", row.UpdatedAt, row.RenamePending)
	}
}

// TestCommitTaskInfoUpdate_PresenceAndNullSafety 验证 presence 语义与 NULL 安全同值判定：
// nil 字段不动；NULL env_snapshot 与非 NULL 目标按真实变化处理；同值 NULL 比较不误判。
func TestCommitTaskInfoUpdate_PresenceAndNullSafety(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1500)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	// 仅 Name 提供：Branch/EnvSnapshot 不动（NULL 保持 NULL）。
	{
		restore := withNowUnix(t, 1501)
		defer restore()
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{Name: strPtr("n2")})
		if err != nil {
			t.Fatalf("commit name only: %v", err)
		}
		if !res.Changed {
			t.Fatalf("res = %+v, want Changed（name 变化）", res)
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.Branch != "b" || row.EnvSnapshot.Valid {
		t.Errorf("branch/env_snapshot = %q/%+v, want b/NULL（nil 字段不动）", row.Branch, row.EnvSnapshot)
	}
	// env_snapshot：NULL → 值为真实变化；值 → 同值为幂等。
	{
		restore := withNowUnix(t, 1502)
		defer restore()
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{EnvSnapshot: strPtr(`{"vars":{}}`)})
		if err != nil {
			t.Fatalf("commit env: %v", err)
		}
		if !res.Changed {
			t.Fatalf("env NULL→值 res = %+v, want Changed", res)
		}
	}
	{
		restore := withNowUnix(t, 1503)
		defer restore()
		res, err := db.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{EnvSnapshot: strPtr(`{"vars":{}}`)})
		if err != nil {
			t.Fatalf("commit env same: %v", err)
		}
		if res.Changed || res.UpdatedAtAdvanced {
			t.Fatalf("env 同值 res = %+v, want !Changed+!UpdatedAtAdvanced（NULL 安全同值判定）", res)
		}
	}
}

// TestCommitTaskInfoUpdate_MissingTask 验证行不存在 → !Matched。
func TestCommitTaskInfoUpdate_MissingTask(t *testing.T) {
	db := openTestDB(t)
	name := "n"
	res, err := db.CommitTaskInfoUpdate(context.Background(), "missing", application.TaskInfoUpdate{Name: &name})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Matched {
		t.Errorf("Matched = true, want false（行不存在）")
	}
}

// TestSetAndClearTaskRenamePending_RoundTrip 验证意图写清 roundtrip：写入/覆盖/清除/幂等，
// 全程 updated_at 不推进、Changed 恒 false（意图元数据永不参与 Changed 计算）。
func TestSetAndClearTaskRenamePending_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	var ua int64
	{
		restore := withNowUnix(t, 1600)
		defer restore()
		ua = seedSuspendedTaskForSV(t, db)
	}
	assertUA := func(stage string) {
		t.Helper()
		row, _ := db.GetTask(ctx, "t1")
		if row.UpdatedAt != ua {
			t.Errorf("%s: updated_at = %d, want %d（意图元数据不推进）", stage, row.UpdatedAt, ua)
		}
	}
	{
		restore := withNowUnix(t, 1601)
		defer restore()
		res, err := db.SetTaskRenamePending(ctx, "t1", `{"branch_old":"b","branch_new":"b2"}`)
		if err != nil {
			t.Fatalf("set: %v", err)
		}
		if !res.Matched || res.Changed {
			t.Fatalf("set res = %+v, want Matched+!Changed（意图不参与 Changed）", res)
		}
	}
	assertUA("after set")
	// 覆盖写入（不同 JSON）。
	{
		restore := withNowUnix(t, 1602)
		defer restore()
		if _, err := db.SetTaskRenamePending(ctx, "t1", `{"name":"n2"}`); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
	}
	pending, err := db.GetTaskRenamePending(ctx, "t1")
	if err != nil || pending == nil || *pending != `{"name":"n2"}` {
		t.Fatalf("pending = %v, err = %v, want {\"name\":\"n2\"}", pending, err)
	}
	assertUA("after overwrite")
	// 清除。
	{
		restore := withNowUnix(t, 1603)
		defer restore()
		res, err := db.ClearTaskRenamePending(ctx, "t1")
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if !res.Matched || res.Changed {
			t.Fatalf("clear res = %+v, want Matched+!Changed", res)
		}
	}
	if pending, err := db.GetTaskRenamePending(ctx, "t1"); err != nil || pending != nil {
		t.Fatalf("pending after clear = %v, err = %v, want nil", pending, err)
	}
	assertUA("after clear")
	// 幂等清除（无意图时再清除仍成功）。
	{
		restore := withNowUnix(t, 1604)
		defer restore()
		res, err := db.ClearTaskRenamePending(ctx, "t1")
		if err != nil {
			t.Fatalf("idempotent clear: %v", err)
		}
		if !res.Matched || res.Changed {
			t.Fatalf("idempotent clear res = %+v, want Matched+!Changed", res)
		}
	}
	assertUA("after idempotent clear")
	// 未命中行。
	if res, err := db.SetTaskRenamePending(ctx, "missing", "{}"); err != nil || res.Matched {
		t.Errorf("set missing: res=%+v err=%v, want !Matched", res, err)
	}
	if res, err := db.ClearTaskRenamePending(ctx, "missing"); err != nil || res.Matched {
		t.Errorf("clear missing: res=%+v err=%v, want !Matched", res, err)
	}
	if pending, err := db.GetTaskRenamePending(ctx, "missing"); err == nil {
		t.Errorf("get missing: err = nil, want sql.ErrNoRows")
	} else if pending != nil {
		t.Errorf("get missing: pending = %v, want nil", pending)
	}
}

// --- 失败矩阵持久化侧（tasks 1.3） ---
//
// 以取消 ctx 使整个事务失败（begin tx 即失败，确定性、无注入钩子），断言：
// 提交失败零部分写入——意图保留、业务列与 updated_at 原状（「待恢复」语义的持久化前提：
// 提交未成功时意图 MUST 保留供恢复路径收敛）。

// TestCommitTaskInfoUpdate_CommitFailurePreservesIntent 验证提交失败意图保留。
func TestCommitTaskInfoUpdate_CommitFailurePreservesIntent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1700)
		defer restore()
		seedTaskWithPending(t, db, "t1", `{"name":"n2","branch_old":"b","branch_new":"b2"}`)
	}
	{
		restore := withNowUnix(t, 1701)
		defer restore()
		failCtx, cancel := context.WithCancel(ctx)
		cancel()
		name, branch := "n2", "b2"
		if _, err := db.CommitTaskInfoUpdate(failCtx, "t1", application.TaskInfoUpdate{Name: &name, Branch: &branch}); err == nil {
			t.Fatal("提交失败：want err, got nil")
		}
	}
	row, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !row.RenamePending.Valid || row.RenamePending.String != `{"name":"n2","branch_old":"b","branch_new":"b2"}` {
		t.Errorf("rename_pending = %+v, want 保留（提交失败意图保留）", row.RenamePending)
	}
	if row.Name != "task" || row.Branch != "b" {
		t.Errorf("name/branch = %q/%q, want task/b（零部分写入）", row.Name, row.Branch)
	}
	if row.UpdatedAt != 1700 {
		t.Errorf("updated_at = %d, want 1700（失败不推进）", row.UpdatedAt)
	}
}

// TestClearTaskRenamePending_FailurePreservesIntent 验证意图清除失败语义：清除未成功时
// 意图保留（D2 失败矩阵「清除意图失败 → 待恢复，由下次收敛清理」）。
func TestClearTaskRenamePending_FailurePreservesIntent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1800)
		defer restore()
		seedTaskWithPending(t, db, "t1", `{"branch_old":"b","branch_new":"b2"}`)
	}
	{
		restore := withNowUnix(t, 1801)
		defer restore()
		failCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := db.ClearTaskRenamePending(failCtx, "t1"); err == nil {
			t.Fatal("清除失败：want err, got nil")
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if !row.RenamePending.Valid || row.RenamePending.String != `{"branch_old":"b","branch_new":"b2"}` {
		t.Errorf("rename_pending = %+v, want 保留（清除失败语义）", row.RenamePending)
	}
	if row.UpdatedAt != 1800 {
		t.Errorf("updated_at = %d, want 1800", row.UpdatedAt)
	}
}

// TestSetTaskRenamePending_FailureKeepsCleanState 验证意图写入失败零副作用：无部分写入。
func TestSetTaskRenamePending_FailureKeepsCleanState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	{
		restore := withNowUnix(t, 1900)
		defer restore()
		seedSuspendedTaskForSV(t, db)
	}
	{
		restore := withNowUnix(t, 1901)
		defer restore()
		failCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := db.SetTaskRenamePending(failCtx, "t1", `{"branch_old":"b"}`); err == nil {
			t.Fatal("写入失败：want err, got nil")
		}
	}
	row, _ := db.GetTask(ctx, "t1")
	if row.RenamePending.Valid {
		t.Errorf("rename_pending = %+v, want NULL（写入失败零副作用）", row.RenamePending)
	}
}
