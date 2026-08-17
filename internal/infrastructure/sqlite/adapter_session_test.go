// adapter_session_test.go 验证 P1.4.5 sqlite adapter 的 SessionRepository 实现
//（design.md D0:78-86）。
//
// 覆盖：
//   - Claim：成功插入（Changed=true）/ 幂等同值（Claimed+!Changed）/ 他主冲突（Claimed=false+OwnerTaskID）；
//   - TouchOwned：命中推进（Matched+Changed）/ 同值 no-op（Matched+!Changed）/ 未命中（!Matched）；
//   - DeleteOwned：命中（affected=1）/ 未命中（affected=0）；
//   - Align complete：session 行变更 + notice 清除同事务提交（结构化结果）；expected 失配
//     → application.AlignConflict 且整事务回滚（session 行不变、notice 不变）；
//   - Align overflow（complete=false）：不删缺席行、不触碰 notice；
//   - OwnerOf：命中 / 未命中 / 历史双归属 fail-closed 返回 session.AmbiguousOwnerError。
package sqlite

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/application"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/store"
)

func seedTaskWithProject(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	ctx := context.Background()
	// project path 唯一：同库重复 seed 时幂等跳过。
	if _, err := db.GetProjectByPath(ctx, "/repo"); err != nil {
		if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	if err := db.CreateTask(ctx, store.TaskRow{ID: taskID, ProjectID: "p1", Name: "task-" + taskID, Branch: "b",
		Status: "suspended", WorktreePath: "/wt-" + taskID}); err != nil {
		t.Fatalf("create task: %v", err)
	}
}

// TestP145_Adapter_Claim 覆盖 claim 三态：新插入、同值幂等、他主冲突。
func TestP145_Adapter_Claim(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	seedTaskWithProject(t, db, "t2")
	adapter := New(db)
	ctx := context.Background()

	// 新插入：Claimed+Changed。
	res, err := adapter.Claim(ctx, "t1", ocdecksess.Observation{ID: "s1", ParentID: "p", CreatedAt: 1, UpdatedAt: 10, FirstSeenAt: 5})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !res.Claimed || !res.Changed {
		t.Fatalf("res = %+v, want Claimed+Changed (new insert)", res)
	}
	// 同值幂等：Claimed+!Changed。
	res, err = adapter.Claim(ctx, "t1", ocdecksess.Observation{ID: "s1", ParentID: "p", CreatedAt: 1, UpdatedAt: 10, FirstSeenAt: 5})
	if err != nil {
		t.Fatalf("claim repeat: %v", err)
	}
	if !res.Claimed || res.Changed {
		t.Fatalf("res = %+v, want Claimed+!Changed (same value)", res)
	}
	// 值推进：Claimed+Changed。
	res, err = adapter.Claim(ctx, "t1", ocdecksess.Observation{ID: "s1", ParentID: "p2", CreatedAt: 1, UpdatedAt: 20, FirstSeenAt: 5})
	if err != nil {
		t.Fatalf("claim advance: %v", err)
	}
	if !res.Claimed || !res.Changed {
		t.Fatalf("res = %+v, want Claimed+Changed (advanced)", res)
	}
	// 他主冲突：Claimed=false + OwnerTaskID。
	res, err = adapter.Claim(ctx, "t2", ocdecksess.Observation{ID: "s1", ParentID: "", CreatedAt: 1, UpdatedAt: 30, FirstSeenAt: 5})
	if err != nil {
		t.Fatalf("claim conflict: %v", err)
	}
	if res.Claimed || res.OwnerTaskID != "t1" {
		t.Fatalf("res = %+v, want conflict with owner t1", res)
	}
}

// TestP145_Adapter_TouchOwned 覆盖 touch 三态：命中推进、同值 no-op、未命中。
func TestP145_Adapter_TouchOwned(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	if _, err := adapter.Claim(ctx, "t1", ocdecksess.Observation{ID: "s1", CreatedAt: 1, UpdatedAt: 10, FirstSeenAt: 5}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// 同值：Matched+!Changed。
	res, err := adapter.TouchOwned(ctx, "t1", "s1", 10)
	if err != nil || !res.Matched || res.Changed {
		t.Fatalf("same value: res=%+v err=%v, want Matched+!Changed", res, err)
	}
	// 旧值（last_seen_at < ? 排除）：Matched+!Changed。
	res, err = adapter.TouchOwned(ctx, "t1", "s1", 5)
	if err != nil || !res.Matched || res.Changed {
		t.Fatalf("older value: res=%+v err=%v, want Matched+!Changed", res, err)
	}
	// 推进：Matched+Changed。
	res, err = adapter.TouchOwned(ctx, "t1", "s1", 99)
	if err != nil || !res.Matched || !res.Changed {
		t.Fatalf("advance: res=%+v err=%v, want Matched+Changed", res, err)
	}
	// 未命中（他任务归属/不存在）：!Matched。
	seedTaskWithProject(t, db, "t2")
	if _, err := db.ClaimTaskSession(ctx, "t2", "s-other", 1, 1, 1, ""); err != nil {
		t.Fatalf("claim other: %v", err)
	}
	res, err = adapter.TouchOwned(ctx, "t1", "s-other", 99)
	if err != nil || res.Matched || res.Changed {
		t.Fatalf("unowned: res=%+v err=%v, want !Matched", res, err)
	}
	res, err = adapter.TouchOwned(ctx, "t1", "s-missing", 99)
	if err != nil || res.Matched || res.Changed {
		t.Fatalf("missing: res=%+v err=%v, want !Matched", res, err)
	}
}

// TestP145_Adapter_DeleteOwned 覆盖 delete：命中 affected=1；未命中 affected=0。
func TestP145_Adapter_DeleteOwned(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	if _, err := adapter.Claim(ctx, "t1", ocdecksess.Observation{ID: "s1", CreatedAt: 1, UpdatedAt: 10, FirstSeenAt: 5}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := adapter.DeleteOwned(ctx, "t1", "s-missing")
	if err != nil || n != 0 {
		t.Fatalf("delete missing: n=%d err=%v, want 0", n, err)
	}
	n, err = adapter.DeleteOwned(ctx, "t1", "s1")
	if err != nil || n != 1 {
		t.Fatalf("delete owned: n=%d err=%v, want 1", n, err)
	}
	n, err = adapter.DeleteOwned(ctx, "t1", "s1")
	if err != nil || n != 0 {
		t.Fatalf("delete repeat: n=%d err=%v, want 0 (idempotent)", n, err)
	}
}

// TestP145_Adapter_AlignCompleteSingleTx 验证 complete 对齐单事务：插入+删除+notice 清除
// 一并提交，结构化结果携带计数/AffectedSessionIDs/OwnedSessionIDs。
func TestP145_Adapter_AlignCompleteSingleTx(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	// 预置 owned：s-stay、s-stale；预置 overflow notice。
	if _, err := db.ClaimTaskSession(ctx, "t1", "s-stay", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimTaskSession(ctx, "t1", "s-stale", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	overflowJSON := `[{"code":"session_overflow","message":"m","ts":1}]`
	if _, err := db.UpdateTaskNotice(ctx, "t1", &overflowJSON); err != nil {
		t.Fatal(err)
	}

	observed := []ocdecksess.Observation{{ID: "s-stay", CreatedAt: 1, UpdatedAt: 20}}
	res, err := adapter.Align(ctx, "t1", ocdecksess.AlignModeRepo, observed, true,
		application.NoticeMutation{Expected: &overflowJSON, New: nil})
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	// s-stay 时间推进（Touched=1）+ s-stale 删除（Deleted=1）。
	if res.Touched != 1 || res.Deleted != 1 || res.Inserted != 0 {
		t.Fatalf("res = %+v, want Touched=1 Deleted=1", res)
	}
	if len(res.AffectedSessionIDs) != 2 || len(res.OwnedSessionIDs) != 1 || res.OwnedSessionIDs[0] != "s-stay" {
		t.Fatalf("res ids = affected:%v owned:%v, want [s-stay s-stale]/[s-stay]", res.AffectedSessionIDs, res.OwnedSessionIDs)
	}
	// notice 清除随同事务提交。
	row, err := db.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Notice.Valid && row.Notice.String != "" {
		t.Fatalf("notice = %q, want cleared", row.Notice.String)
	}
	if !res.TaskMutation.Changed {
		t.Fatalf("TaskMutation = %+v, want Changed=true (notice cleared)", res.TaskMutation)
	}
}

// TestP145_Adapter_AlignConflictRollsBack 验证 complete 路径 notice expected 失配：
// 返回 application.AlignConflict 且整事务回滚（session 行不变、notice 不变）。
func TestP145_Adapter_AlignConflictRollsBack(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	if _, err := db.ClaimTaskSession(ctx, "t1", "s-stay", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	curJSON := `[{"code":"session_overflow","message":"m","ts":1}]`
	if _, err := db.UpdateTaskNotice(ctx, "t1", &curJSON); err != nil {
		t.Fatal(err)
	}

	// expected 传入失配值（实际 notice 为 curJSON）。
	wrongExpected := `[{"code":"residual_processes","message":"x","ts":2}]`
	observed := []ocdecksess.Observation{{ID: "s-new", CreatedAt: 1, UpdatedAt: 20}}
	_, err := adapter.Align(ctx, "t1", ocdecksess.AlignModeRepo, observed, true,
		application.NoticeMutation{Expected: &wrongExpected, New: nil})
	if !application.IsAlignConflict(err) {
		t.Fatalf("err = %v, want AlignConflict", err)
	}
	// 整事务回滚：s-new 未插入、s-stay 未删除。
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 1 || sessions[0].SessionID != "s-stay" {
		t.Fatalf("sessions = %+v, want [s-stay] only (rollback, no partial commit)", sessions)
	}
	// notice 未变。
	row, _ := db.GetTask(ctx, "t1")
	if row.Notice.String != curJSON || !row.Notice.Valid {
		t.Fatalf("notice = %+v, want unchanged %q", row.Notice, curJSON)
	}
}

// TestP145_Adapter_AlignOverflowKeepsAbsentAndNotice 验证 overflow（complete=false）：
// 不删缺席行、不触碰 notice（空 NoticeMutation 分支跳过）。
func TestP145_Adapter_AlignOverflowKeepsAbsentAndNotice(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	if _, err := db.ClaimTaskSession(ctx, "t1", "s-stale", 1, 1, 10, ""); err != nil {
		t.Fatal(err)
	}
	overflowJSON := `[{"code":"session_overflow","message":"m","ts":1}]`
	if _, err := db.UpdateTaskNotice(ctx, "t1", &overflowJSON); err != nil {
		t.Fatal(err)
	}

	observed := []ocdecksess.Observation{{ID: "s-stale", CreatedAt: 1, UpdatedAt: 20}}
	res, err := adapter.Align(ctx, "t1", ocdecksess.AlignModeRepo, observed, false, application.NoticeMutation{})
	if err != nil {
		t.Fatalf("align overflow: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("Deleted = %d, want 0 (overflow must not delete absent rows)", res.Deleted)
	}
	if res.TaskMutation.Changed {
		t.Fatalf("TaskMutation = %+v, want unchanged (overflow must not touch notice)", res.TaskMutation)
	}
	sessions, _ := db.ListTaskSessions(ctx, "t1")
	if len(sessions) != 1 || sessions[0].SessionID != "s-stale" {
		t.Fatalf("sessions = %+v, want [s-stale] kept", sessions)
	}
}

// TestP145_Adapter_OwnerOf 覆盖 OwnerOf 三态：命中、未命中、历史双归属 fail-closed。
func TestP145_Adapter_OwnerOf(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	seedTaskWithProject(t, db, "t2")
	adapter := New(db)
	ctx := context.Background()

	if _, err := db.ClaimTaskSession(ctx, "t1", "s1", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	owner, found, err := adapter.OwnerOf(ctx, "s1")
	if err != nil || !found || owner != "t1" {
		t.Fatalf("OwnerOf(s1) = %q/%v/%v, want t1/true/nil", owner, found, err)
	}

	_, found, err = adapter.OwnerOf(ctx, "s-missing")
	if err != nil || found {
		t.Fatalf("OwnerOf(missing) = %v/%v, want false/nil", found, err)
	}

	// 直接构造历史脏数据：同一 session_id 归属两个 task（绕过 claim 事务的物理行）。
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t2", SessionID: "s-dup", LastSeenAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t1", SessionID: "s-dup", LastSeenAt: 1}); err != nil {
		t.Fatal(err)
	}
	_, _, err = adapter.OwnerOf(ctx, "s-dup")
	if err == nil {
		t.Fatal("OwnerOf with duplicate ownership must fail (fail-closed)")
	}
	var amb *ocdecksess.AmbiguousOwnerError
	if !errors.As(err, &amb) {
		t.Fatalf("err type = %T, want *session.AmbiguousOwnerError", err)
	}
}

// TestP145_Adapter_OwnedSessions 验证 owned 集合查询（对账交集用）。
func TestP145_Adapter_OwnedSessions(t *testing.T) {
	db := openTestDB(t)
	seedTaskWithProject(t, db, "t1")
	adapter := New(db)
	ctx := context.Background()
	if _, err := db.ClaimTaskSession(ctx, "t1", "s1", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimTaskSession(ctx, "t1", "s2", 1, 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	ids, err := adapter.OwnedSessions(ctx, "t1")
	if err != nil {
		t.Fatalf("OwnedSessions: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2", ids)
	}
}
