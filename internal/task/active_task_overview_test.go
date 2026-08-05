package task

import (
	"context"
	"testing"

	"ocdeck/internal/store"
)

// TestManager_ListActiveTaskOverview_Delegate 验证 Manager.ListActiveTaskOverview
// 是 thin delegate，直接返回 store 结果（cross-project-active-sessions D1）。
func TestManager_ListActiveTaskOverview_Delegate(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "projA", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "taskA", Branch: "b", Status: StatusActive, WorktreePath: "/wt"}
	store.tasks["t2"] = TaskRow{ID: "t2", ProjectID: "p1", Name: "taskB", Branch: "b", Status: StatusSuspended, WorktreePath: "/wt2"}
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 100}}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	rows, err := m.ListActiveTaskOverview(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTaskOverview: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (only active)", len(rows))
	}
	if rows[0].ID != "t1" || rows[0].ProjectName != "projA" || rows[0].LastActiveAt != 100 {
		t.Errorf("row = %+v, want t1/projA/100", rows[0])
	}
}

// TestStoreAdapter_ListActiveTaskOverview_Conversion 验证 StoreAdapter 将
// store.ActiveTaskOverviewRow 逐字段转换为 task.ActiveTaskOverviewRow（D1 adapter）。
func TestStoreAdapter_ListActiveTaskOverview_Conversion(t *testing.T) {
	adapter, db := openRealStore(t)
	ctx := context.Background()
	// 第二个项目 + 跨项目 active 任务。
	if err := db.CreateProject(ctx, "p2", "projB", "/repoB", "main"); err != nil {
		t.Fatalf("create project p2: %v", err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t-active", ProjectID: "p2", Name: "task", Branch: "b", Status: StatusActive, WorktreePath: "/wtB",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.UpsertTaskSession(ctx, store.SessionRow{TaskID: "t-active", SessionID: "s1", SessionCreatedAt: 1, FirstSeenAt: 10, LastSeenAt: 300}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	// openRealStore seed 的 t1 是 suspended → 不应出现。
	rows, err := adapter.ListActiveTaskOverview(ctx)
	if err != nil {
		t.Fatalf("adapter list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (only active)", len(rows))
	}
	r := rows[0]
	if r.ID != "t-active" || r.ProjectID != "p2" || r.ProjectName != "projB" ||
		r.Name != "task" || r.Branch != "b" || r.WorktreePath != "/wtB" || r.LastActiveAt != 300 {
		t.Errorf("converted row = %+v, want field-by-field from store row", r)
	}
}

// TestMockStore_ListActiveTaskOverview_CoalesceSemantics 验证 mockStore 镜像
// store SQL 的 COALESCE 语义：有 session 行时 last_active_at 只取 sessions 的 MAX，
// 不混入 updated_at（即便 updated_at > session 值）；无 session 才回退 updated_at。
func TestMockStore_ListActiveTaskOverview_CoalesceSemantics(t *testing.T) {
	s := newMockStore()
	s.seedProject(ProjectRow{ID: "p1", Name: "projA", Path: "/repo", DefaultBranch: "main"})
	// t1 有 session（值 100），但 updated_at=9999（更大）→ COALESCE 必须取 100，不混 updated_at。
	s.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "n", Branch: "b", Status: StatusActive, WorktreePath: "/wt", UpdatedAt: 9999}
	s.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 100}}
	// t2 无 session → 回退 updated_at=200。
	s.tasks["t2"] = TaskRow{ID: "t2", ProjectID: "p1", Name: "n", Branch: "b", Status: StatusActive, WorktreePath: "/wt", UpdatedAt: 200}

	rows, err := s.ListActiveTaskOverview(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.ID] = r.LastActiveAt
	}
	if got["t1"] != 100 {
		t.Errorf("t1 = %d, want 100 (COALESCE: sessions MAX, not max(updated_at, sessions))", got["t1"])
	}
	if got["t2"] != 200 {
		t.Errorf("t2 = %d, want 200 (no session → updated_at fallback)", got["t2"])
	}
}

// TestMockStore_ListActiveTaskOverview_MsNormalization 验证 mockStore 镜像
// store 的 ms→s 归一化（≥1e11 ÷1000）。
func TestMockStore_ListActiveTaskOverview_MsNormalization(t *testing.T) {
	s := newMockStore()
	s.seedProject(ProjectRow{ID: "p1", Name: "projA", Path: "/repo", DefaultBranch: "main"})
	s.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "n", Branch: "b", Status: StatusActive, WorktreePath: "/wt"}
	// 13 位 ms 值 → 归一化为 1785797826 秒。
	s.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1", LastSeenAt: 1785797826297}}

	rows, err := s.ListActiveTaskOverview(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].LastActiveAt != 1785797826 {
		t.Errorf("row = %+v, want last_active_at=1785797826 (ms ÷1000)", rows[0])
	}
}