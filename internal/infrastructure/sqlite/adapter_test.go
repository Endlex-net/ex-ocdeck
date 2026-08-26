// adapter_test.go 验证 Adapter 实现 application ports 全部方法（编译期接口闭合，P1.2）。
package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/store"
)

// TestAdapter_ImplementsApplicationPorts 确保 Adapter 满足所有 application ports 接口。
// 编译期断言：方法缺失或签名不匹配即编译失败。
func TestAdapter_ImplementsApplicationPorts(t *testing.T) {
	var _ application.TaskRepository = (*Adapter)(nil)
	var _ application.SessionRepository = (*Adapter)(nil)
	var _ application.ProjectReader = (*Adapter)(nil)
	var _ application.EnvReader = (*Adapter)(nil)
	var _ application.CleanupDebtRepository = (*Adapter)(nil)
	var _ application.ProcessPort = (*Adapter)(nil)
	var _ application.OpenCodePort = (*Adapter)(nil)
	var _ application.WorktreePort = (*Adapter)(nil)
}

// openTestDB 打开临时目录 SQLite 并 migration，供 adapter 读侧测试使用。
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestAdapter_GetTask_GuardView 验证 GetTask 把 store 行重建为 domain guard 视图，
// status/init_status/delete_mode/notices 字段映射正确（design D0 P1.4.2）。
func TestAdapter_GetTask_GuardView(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// 残留进程 notice JSON（对齐 internal/task.noticeEntry 结构）。
	noticeJSON := `[{"code":"residual_processes","message":"cleanup failed","ts":99,"data":{"sessionName":"t1-serve","cleanupTickets":["k1","k2"],"reason":"kill_failed","retryable":true}}]`
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := db.UpdateTaskNotice(ctx, "t1", &noticeJSON); err != nil {
		t.Fatalf("update notice: %v", err)
	}
	adapter := New(db)

	got, err := adapter.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status() != ocdecktask.StatusSuspended {
		t.Errorf("Status = %q, want suspended", got.Status())
	}
	if got.InitStatus() != ocdecktask.InitStatusNone {
		t.Errorf("InitStatus = %q, want none (column default)", got.InitStatus())
	}
	if !got.HasRetryableResidual() {
		t.Error("HasRetryableResidual = false, want true (notice has retryable residual_processes)")
	}
	if !got.CanArchive() {
		t.Error("CanArchive = false, want true (suspended + none init)")
	}
	// retryable notice blocks Activate（domain CanActivate: HasRetryableResidual→false）。
	if got.CanActivate() {
		t.Error("CanActivate = true, want false (blocked by retryable residual notice)")
	}
	// 验证 delete_mode NULL 映射为空串。
	if got.DeleteMode() != "" {
		t.Errorf("DeleteMode = %q, want empty (NULL)", got.DeleteMode())
	}
}

// TestAdapter_GetTask_NotFound 验证 GetTask 不存在行返回 error。
func TestAdapter_GetTask_NotFound(t *testing.T) {
	db := openTestDB(t)
	adapter := New(db)
	if _, err := adapter.GetTask(context.Background(), "missing"); err == nil {
		t.Fatal("GetTask(missing) should return error")
	}
}

// TestAdapter_GetTask_CorruptNotice 验证损坏 notice JSON 返回 error（fail-closed）。
func TestAdapter_GetTask_CorruptNotice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_ = db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo")
	_ = db.CreateTask(ctx, store.TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/wt"})
	corrupt := `{not valid json`
	_, _ = db.UpdateTaskNotice(ctx, "t1", &corrupt)
	adapter := New(db)
	if _, err := adapter.GetTask(ctx, "t1"); err == nil {
		t.Fatal("GetTask with corrupt notice JSON should return error (fail-closed)")
	}
}

// TestParseNoticeJSON 验证 notice JSON 到 domain typed Notice 的映射（两 code 覆盖）。
func TestParseNoticeJSON(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := parseNoticeJSON(sql.NullString{})
		if err != nil || len(got) != 0 {
			t.Fatalf("empty: got=%v err=%v", got, err)
		}
	})
	t.Run("residual_processes", func(t *testing.T) {
		raw := `[{"code":"residual_processes","message":"m","ts":1,"data":{"sessionName":"s","cleanupTickets":["a","b"],"reason":"kill_failed","retryable":true}}]`
		got, err := parseNoticeJSON(sql.NullString{String: raw, Valid: true})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		n := got[0]
		if n.Code != ocdecktask.NoticeCodeResidualProcesses {
			t.Errorf("Code = %q", n.Code)
		}
		if n.Data.SessionName != "s" {
			t.Errorf("SessionName = %q", n.Data.SessionName)
		}
		if len(n.Data.CleanupTickets) != 2 || n.Data.CleanupTickets[0] != "a" {
			t.Errorf("CleanupTickets = %v", n.Data.CleanupTickets)
		}
		if n.Data.Reason != "kill_failed" {
			t.Errorf("Reason = %q", n.Data.Reason)
		}
		if !n.Data.Retryable {
			t.Error("Retryable = false")
		}
	})
	t.Run("session_overflow", func(t *testing.T) {
		raw := `[{"code":"session_overflow","message":"overflow","ts":2}]`
		got, err := parseNoticeJSON(sql.NullString{String: raw, Valid: true})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != 1 || got[0].Code != ocdecktask.NoticeCodeSessionOverflow {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		raw := `not json`
		if _, err := parseNoticeJSON(sql.NullString{String: raw, Valid: true}); err == nil {
			t.Fatal("corrupt JSON should return error")
		}
	})
}

func TestAdapter_GetTaskRow_AnchorSessionIDRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: "suspended", WorktreePath: "/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	adapter := New(db)

	empty, err := adapter.GetTaskRow(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskRow empty: %v", err)
	}
	if empty.AnchorSessionID != nil {
		t.Errorf("empty AnchorSessionID = %v, want nil", empty.AnchorSessionID)
	}

	if _, err := db.ClaimTaskSessionAndSetAnchor(ctx, "t1", "sess-anchor", 1, 1, 1, ""); err != nil {
		t.Fatalf("claim+anchor: %v", err)
	}
	got, err := adapter.GetTaskRow(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskRow: %v", err)
	}
	if got.AnchorSessionID == nil || *got.AnchorSessionID != "sess-anchor" {
		t.Errorf("AnchorSessionID = %v, want sess-anchor", got.AnchorSessionID)
	}
}