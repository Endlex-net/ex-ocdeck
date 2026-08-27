package task

import (
	"context"
	"database/sql"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// TestB3_AnchorIsolatesSubsession_LateLastSeen 验证 B3 锚定隔离：
// 子 session（background subagent）last_seen 更晚时，resolveAnchorSession/Activate
// 仍锚定顶层主会话，不锚定到子会话。
func TestB3_AnchorIsolatesSubsession_LateLastSeen(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-top", Valid: true}
	})
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-top", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 10,
		ParentID: "",
	})
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-sub", SessionCreatedAt: 100, FirstSeenAt: 100, LastSeenAt: 1000,
		ParentID: "sess-top",
	})

	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-top", Time: opencode.SessionTime{Created: 1, Updated: 10}}}
	proc := newMockProc()
	m := newTestManager(t, tStore, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0", got)
	}
	argv := runtimeCmdArgvOf(proc, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-top")
}

// TestB3_AnchorIsolatesSubsession_ReopenAttach 验证 B3：ReopenAttach 路径
// 经 resolveAnchorSession 同样锚定顶层会话（子会话 last_seen 更晚时不被选中）。
func TestB3_AnchorIsolatesSubsession_ReopenAttach(t *testing.T) {
	tStore := newMockStore()
	seedActiveTask(tStore, "t1", "p1")
	store := tStore
	store.mutTask("t1", func(t *TaskRow) {
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
	})
	// 顶层会话 + 子会话（子会话 last_seen 更晚）。
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-top", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 10,
		ParentID: "",
	})
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-sub", SessionCreatedAt: 100, FirstSeenAt: 100, LastSeenAt: 1000,
		ParentID: "sess-top",
	})

	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach: %v", err)
	}
	if string(tid) != runtimeSessionName("t1") {
		t.Errorf("terminal = %q, want runtime session", tid)
	}
}

// TestB3_AnchorIsolatesSubsession_SuspendRepair 验证 B3：Suspend 分支 c 修复路径
// 经 resolveAnchorSession 同样锚定顶层会话（子会话 last_seen 更晚时不被选中）。
func TestB3_AnchorIsolatesSubsession_SuspendRepair(t *testing.T) {
	tStore := newMockStore()
	seedActiveTask(tStore, "t1", "p1")
	store := tStore
	store.mutTask("t1", func(t *TaskRow) {
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-top", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 10,
		ParentID: "",
	})
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-sub", SessionCreatedAt: 100, FirstSeenAt: 100, LastSeenAt: 1000,
		ParentID: "sess-top",
	})

	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	// tui kill 成功（会话移除，修复时需重开）+ serve kill 失败（仍存活）→ 分支 c 修复重开 tui，锚定。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-top", Time: opencode.SessionTime{Created: 1, Updated: 10}}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("repair must not recreate TUI session")
	}
}

// TestB3_AlignSessions_PersistsParentID 验证 B3：全量对齐持久化 parent_id
// （子 session 非空），后续 ListTopLevelTaskSessions 正确过滤。
func TestB3_AlignSessions_PersistsParentID(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	oc := newMockOC(true)
	// ListSessions 返回顶层 + 子会话。
	oc.sessions = []opencode.Session{
		{ID: "sess-top", Time: opencode.SessionTime{Created: 1, Updated: 10}},
		{ID: "sess-sub", Time: opencode.SessionTime{Created: 100, Updated: 1000}, ParentID: "sess-top"},
	}
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	if err := m.alignSessions(context.Background(), "t1", "/wt", oc, AlignModeRepo); err != nil {
		t.Fatalf("alignSessions: %v", err)
	}
	all, _ := tStore.ListTaskSessions(context.Background(), "t1")
	if len(all) != 2 {
		t.Fatalf("ListTaskSessions = %d rows, want 2", len(all))
	}
	var sub *SessionRow
	for i := range all {
		if all[i].SessionID == "sess-sub" {
			sub = &all[i]
		}
	}
	if sub == nil {
		t.Fatal("sess-sub not persisted")
	}
	if sub.ParentID != "sess-top" {
		t.Errorf("sess-sub ParentID = %q, want sess-top", sub.ParentID)
	}
	// ListTopLevelTaskSessions MUST 仅返回顶层会话。
	top, _ := tStore.ListTopLevelTaskSessions(context.Background(), "t1")
	if len(top) != 1 || top[0].SessionID != "sess-top" {
		t.Errorf("ListTopLevelTaskSessions = %+v, want single sess-top", top)
	}
}

// TestB3_SSECapture_PersistsParentID 验证 B3：SSE 捕获 session.created 事件
// 持久化 parent_id（子 session 非空），后续 ListTopLevelTaskSessions 正确过滤。
func TestB3_SSECapture_PersistsParentID(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), newMockOC(true))

	// 构造 session.created 事件携带 parentID（子会话）。
	ev := opencode.Event{Type: "session.created", Properties: map[string]interface{}{
		"info": map[string]interface{}{
			"id":        "sess-sub-ev",
			"directory": "/wt/t1",
			"time":      map[string]interface{}{"updated": float64(500)},
			"parentID":  "sess-top-ev",
		},
	}}
	if err := m.handleSSEEvent(context.Background(), "t1", "/wt/t1", ev); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	all, _ := tStore.ListTaskSessions(context.Background(), "t1")
	if len(all) != 1 {
		t.Fatalf("ListTaskSessions = %d rows, want 1", len(all))
	}
	if all[0].ParentID != "sess-top-ev" {
		t.Errorf("captured ParentID = %q, want sess-top-ev", all[0].ParentID)
	}
	top, _ := tStore.ListTopLevelTaskSessions(context.Background(), "t1")
	if len(top) != 0 {
		t.Errorf("ListTopLevelTaskSessions = %d rows, want 0 (sub-session filtered)", len(top))
	}
}
