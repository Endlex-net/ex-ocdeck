// task_info_test.go 验证 Adapter 任务信息修改端口委托与 rename_pending 内部隔离
// （task-info-editable tasks 1.2 验证项：意图写清 roundtrip + TaskSnapshot/公共 DTO 不外泄）。
package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/store"
)

// TestAdapter_TaskInfoUpdate_RenamePendingRoundTrip 验证 adapter 层意图写清与业务提交：
// Set → Get → Commit（业务列 + 清意图）→ Clear 幂等。
func TestAdapter_TaskInfoUpdate_RenamePendingRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/a", Status: "suspended",
		WorktreePath: "/wt", Mode: "worktree", PermissionMode: "ask",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	adapter := New(db)

	// Set → Get roundtrip。
	const intent = `{"name":"n2","branch_old":"ocdeck/a","branch_new":"ocdeck/b","env_snapshot":null}`
	if _, err := adapter.SetTaskRenamePending(ctx, "t1", intent); err != nil {
		t.Fatalf("SetTaskRenamePending: %v", err)
	}
	got, err := adapter.GetTaskRenamePending(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskRenamePending: %v", err)
	}
	if got == nil || *got != intent {
		t.Fatalf("pending = %v, want %q", got, intent)
	}

	// Commit：业务列 + 同事务清除意图。
	name, branch := "n2", "ocdeck/b"
	res, err := adapter.CommitTaskInfoUpdate(ctx, "t1", application.TaskInfoUpdate{Name: &name, Branch: &branch})
	if err != nil {
		t.Fatalf("CommitTaskInfoUpdate: %v", err)
	}
	if !res.Matched || !res.Changed {
		t.Fatalf("res = %+v, want Matched+Changed（业务列真实变化）", res)
	}
	if pending, err := adapter.GetTaskRenamePending(ctx, "t1"); err != nil || pending != nil {
		t.Fatalf("pending after commit = %v, err = %v, want nil（同事务清除）", pending, err)
	}
	snap, err := adapter.GetTaskRow(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskRow: %v", err)
	}
	if snap.Name != "n2" || snap.Branch != "ocdeck/b" {
		t.Errorf("name/branch = %q/%q, want n2/ocdeck/b", snap.Name, snap.Branch)
	}

	// Clear 幂等（无意图时清除仍成功）。
	if _, err := adapter.ClearTaskRenamePending(ctx, "t1"); err != nil {
		t.Fatalf("ClearTaskRenamePending idempotent: %v", err)
	}

	// 未命中行 → ErrTaskNotFound sentinel（与 GetTask 同型归一化）。
	if _, err := adapter.GetTaskRenamePending(ctx, "missing"); !errors.Is(err, application.ErrTaskNotFound) {
		t.Fatalf("GetTaskRenamePending(missing) err = %v, want ErrTaskNotFound", err)
	}
}

// TestAdapter_TaskSnapshot_DoesNotLeakRenamePending 回归断言（tasks 1.2 验证项）：
// rename_pending 仅由内部 store row / service 映射携带，MUST NOT 进入公共 TaskSnapshot
// / facade 转换——TaskSnapshot 经 JSON 序列化后 MUST NOT 出现 rename_pending 键，
// 且写有意图的任务读出的快照字段保持既有形态（无新增字段）。
func TestAdapter_TaskSnapshot_DoesNotLeakRenamePending(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/a", Status: "suspended",
		WorktreePath: "/wt", Mode: "worktree", PermissionMode: "ask",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	const intent = `{"name":"n2"}`
	if _, err := db.SetTaskRenamePending(ctx, "t1", intent); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	adapter := New(db)

	snap, err := adapter.GetTaskRow(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskRow: %v", err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "rename_pending") {
		t.Fatalf("TaskSnapshot JSON 外泄 rename_pending：%s", raw)
	}
	// List 读侧同样不外泄。
	snaps, err := adapter.ListTasksByProject(ctx, "p1")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListTasksByProject: n=%d err=%v", len(snaps), err)
	}
	raw, err = json.Marshal(snaps)
	if err != nil {
		t.Fatalf("marshal snapshots: %v", err)
	}
	if strings.Contains(string(raw), "rename_pending") {
		t.Fatalf("TaskSnapshot list JSON 外泄 rename_pending：%s", raw)
	}
}
