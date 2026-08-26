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
	row := seedSuspendedTask(tStore, "t1", "p1")
	// 顶层主会话（last_seen 较早）。
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-top", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 10,
		ParentID: "",
	})
	// background subagent 子会话（last_seen 更晚，ParentID 指向顶层会话）。
	// 若锚定候选不隔离顶层，子会话会排到首项被错误锚定。
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-sub", SessionCreatedAt: 100, FirstSeenAt: 100, LastSeenAt: 1000,
		ParentID: "sess-top",
	})

	oc := newAnchorTestOC()
	// GetSession 对 sess-top 返回存在；其他 id 默认 ErrSessionNotFound（mockOC）。
	oc.getSessionResult = opencode.Session{ID: "sess-top", Time: opencode.SessionTime{Created: 1, Updated: 10}}
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	id, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err != nil {
		t.Fatalf("resolveAnchorSession: %v", err)
	}
	if id != "sess-top" {
		t.Errorf("anchored session = %q, want sess-top (top-level, not sub-session)", id)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0 (top-level session exists)", got)
	}
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
	// serve 会话需存活供 ReopenAttach 读端口。
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}
	oc := newMockOC(true)
	// 让 GetSession 对 sess-top 返回存在（mockOC.sessions 为空，默认 ErrSessionNotFound；
	// 预置 sessions 使 sess-top 命中）。
	oc.sessions = []opencode.Session{{ID: "sess-top", Time: opencode.SessionTime{Created: 1, Updated: 10}}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	// 注入 runtime（ReopenAttach 注册 group + watchTUIExit 依赖 runtime）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach: %v", err)
	}
	_ = tid
	// TUI 会话 argv MUST 使用 --session sess-top（顶层），非 sess-sub。
	argv := tuiCmdArgv(proc, "t1")
	if argv == nil {
		t.Fatal("TUI session not created")
	}
	var sessID string
	for i, a := range argv {
		if a == "--session" && i+1 < len(argv) {
			sessID = argv[i+1]
		}
	}
	if sessID != "sess-top" {
		t.Errorf("ReopenAttach anchored session = %q, want sess-top (top-level)", sessID)
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
	// 分支 c 修复成功 → 回 active；重开 tui 锚定 sess-top。
	argv := tuiCmdArgv(proc, "t1")
	if argv == nil {
		t.Fatal("TUI session not recreated by repair")
	}
	var sessID string
	for i, a := range argv {
		if a == "--session" && i+1 < len(argv) {
			sessID = argv[i+1]
		}
	}
	if sessID != "sess-top" {
		t.Errorf("Suspend repair anchored session = %q, want sess-top (top-level)", sessID)
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
