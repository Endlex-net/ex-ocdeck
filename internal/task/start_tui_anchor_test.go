package task

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

// tuiCmdArgv 返回 mockProc 记录的 TUI 会话 CmdArgv（§4 锚定测试断言 --session <id>）。
func tuiCmdArgv(p *mockProc, taskID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmdArgvValues[tuiSessionName(taskID)]
}

// assertTUIAttachSession 断言 TUI CmdArgv 为 opencode attach <url> --session <id>（无 --continue）。
func assertTUIAttachSession(t *testing.T, argv []string, wantSessionID string) {
	t.Helper()
	if len(argv) < 5 {
		t.Fatalf("tui argv too short: %v", argv)
	}
	if argv[0] != "opencode" || argv[1] != "attach" {
		t.Fatalf("tui argv prefix = %v, want opencode attach", argv[:2])
	}
	if argv[3] != "--session" {
		t.Fatalf("tui argv[3] = %q, want --session (no --continue)", argv[3])
	}
	if argv[4] != wantSessionID {
		t.Fatalf("tui session id = %q, want %q", argv[4], wantSessionID)
	}
	for _, a := range argv {
		if a == "--continue" {
			t.Fatalf("tui argv MUST NOT contain --continue: %v", argv)
		}
	}
}

// anchorTestOC 可配置 GetSession/CreateSession 行为，独立测试 resolveAnchorSession。
// 嵌入 *mockOC 以复用 CreateSession/其他方法，仅覆盖 GetSession。
type anchorTestOC struct {
	*mockOC
	getSessionResult opencode.Session
	getSessionErr    error
}

func (c *anchorTestOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	if c.getSessionErr != nil {
		return opencode.Session{}, c.getSessionErr
	}
	return c.getSessionResult, nil
}

func newAnchorTestOC() *anchorTestOC {
	return &anchorTestOC{mockOC: newMockOC(true)}
}

// TestResolveAnchorSession_HasRecordExistsReuseSession 验证 §4 路径 1：
// 有记录且 GetSession 预检存在 → 复用旧 id，不创建新会话。
func TestResolveAnchorSession_HasRecordExistsReuseSession(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-existing", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1,
	})
	oc := newAnchorTestOC()
	oc.getSessionResult = opencode.Session{ID: "sess-existing", Time: opencode.SessionTime{Created: 1, Updated: 1}}
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	id, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err != nil {
		t.Fatalf("resolveAnchorSession: %v", err)
	}
	if id != "sess-existing" {
		t.Errorf("id = %q, want sess-existing (reuse)", id)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0 (record exists)", got)
	}
}

// TestResolveAnchorSession_Record404CreatesAndPersists 验证 §4 路径 2：
// 有记录但 GetSession 404 → CreateSession → 持久化 task_sessions → 返回新 id。
func TestResolveAnchorSession_Record404CreatesAndPersists(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-gone", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1,
	})
	oc := newAnchorTestOC()
	oc.getSessionErr = opencode.ErrSessionNotFound
	oc.createSessionResult = opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 100, Updated: 200}}
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	id, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err != nil {
		t.Fatalf("resolveAnchorSession: %v", err)
	}
	if id != "sess-new" {
		t.Errorf("id = %q, want sess-new (created)", id)
	}
	if got := oc.createSessionCountLoad(); got != 1 {
		t.Errorf("CreateSession called %d times, want 1 (404 → create)", got)
	}
	// 新 session MUST 已持久化（created/updated 取自 CreateSession 响应）。
	rows, _ := tStore.ListTaskSessions(context.Background(), "t1")
	var found *SessionRow
	for i := range rows {
		if rows[i].SessionID == "sess-new" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("sess-new not persisted in task_sessions")
	}
	if found.SessionCreatedAt != 100 || found.FirstSeenAt != 200 || found.LastSeenAt != 200 {
		t.Errorf("persisted row = %+v, want created=100 first/last=200", *found)
	}
}

// TestResolveAnchorSession_NoRecordCreatesAndPersists 验证 §4 路径 3：
// 无记录 → CreateSession → 持久化 task_sessions → 返回新 id。
func TestResolveAnchorSession_NoRecordCreatesAndPersists(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	oc := newAnchorTestOC()
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	id, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err != nil {
		t.Fatalf("resolveAnchorSession: %v", err)
	}
	if id != "sess-fresh" {
		t.Errorf("id = %q, want sess-fresh (created)", id)
	}
	if got := oc.createSessionCountLoad(); got != 1 {
		t.Errorf("CreateSession called %d times, want 1 (no record → create)", got)
	}
	rows, _ := tStore.ListTaskSessions(context.Background(), "t1")
	if len(rows) != 1 || rows[0].SessionID != "sess-fresh" {
		t.Errorf("task_sessions = %+v, want single sess-fresh", rows)
	}
}

// TestResolveAnchorSession_CreateSessionFails 验证 §4：
// CreateSession 失败 → 返回错误（不回退）。
func TestResolveAnchorSession_CreateSessionFails(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	oc := newAnchorTestOC()
	oc.createSessionErr = errors.New("opencode: create session: http 500")
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	_, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err == nil {
		t.Fatal("expected error when CreateSession fails")
	}
	if !strings.Contains(err.Error(), "create anchor session") {
		t.Errorf("error must mention create anchor session, got: %v", err)
	}
}

// TestResolveAnchorSession_PersistFails 验证 §4：
// CreateSession 成功但 task_sessions 写入失败 → 返回错误（不留 session 已建但无归属记录的不一致）。
func TestResolveAnchorSession_PersistFails(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	oc := newAnchorTestOC()
	oc.createSessionResult = opencode.Session{ID: "sess-orphan", Time: opencode.SessionTime{Created: 1, Updated: 1}}
	errStore := wrapSessionStoreErr(tStore, errors.New("db: upsert failed"), nil, nil)
	m := newTestManager(t, errStore, newMockProc(), newMockWorktree(), oc)

	_, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err == nil {
		t.Fatal("expected error when persisting anchor session fails")
	}
	if !strings.Contains(err.Error(), "persist anchor session") {
		t.Errorf("error must mention persist anchor session, got: %v", err)
	}
}

// TestResolveAnchorSession_GetSessionOtherErrorFails 验证 §4：
// GetSession 预检返回非 404 错误 → 返回错误（不回退到创建）。
func TestResolveAnchorSession_GetSessionOtherErrorFails(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	_ = tStore.UpsertTaskSession(context.Background(), SessionRow{
		TaskID: "t1", SessionID: "sess-precheck", SessionCreatedAt: 1, FirstSeenAt: 1, LastSeenAt: 1,
	})
	oc := newAnchorTestOC()
	oc.getSessionErr = errors.New("opencode: http 500: boom")
	m := newTestManager(t, tStore, newMockProc(), newMockWorktree(), oc)

	_, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err == nil {
		t.Fatal("expected error when GetSession returns non-404 error")
	}
	if !strings.Contains(err.Error(), "session precheck") {
		t.Errorf("error must mention session precheck, got: %v", err)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0 (precheck non-404 must not fall back)", got)
	}
}

// TestResolveAnchorSession_ListTaskSessionsErrorPropagates 验证 §4：
// ListTopLevelTaskSessions 错误 MUST 传播（不得当空集继续创建致 session 归属丢失，
// design.md §19）。B3：resolveAnchorSession 走顶层会话查询。
func TestResolveAnchorSession_ListTaskSessionsErrorPropagates(t *testing.T) {
	tStore := newMockStore()
	row := seedSuspendedTask(tStore, "t1", "p1")
	oc := newAnchorTestOC()
	errStore := wrapSessionStoreErr(tStore, nil, nil, nil)
	errStore.listTopErr = errors.New("db: list top-level sessions failed")
	m := newTestManager(t, errStore, newMockProc(), newMockWorktree(), oc)

	_, err := m.resolveAnchorSession(context.Background(), oc, row)
	if err == nil {
		t.Fatal("expected error when ListTopLevelTaskSessions fails")
	}
	if !strings.Contains(err.Error(), "list top-level sessions") {
		t.Errorf("error must mention list top-level sessions, got: %v", err)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0 (list error must not fall back to create)", got)
	}
}

// TestStartTUI_ActivateAttachSessionNoContinue 验证端到端 Activate 后 TUI 会话 argv
// 为 opencode attach <url> --session <id>（无 --continue，design.md §4）。
func TestStartTUI_ActivateAttachSessionNoContinue(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-e2e", Time: opencode.SessionTime{Created: 1, Updated: 1}}
	m := newTestManager(t, tStore, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	argv := tuiCmdArgv(proc, "t1")
	if argv == nil {
		t.Fatal("TUI session not created")
	}
	assertTUIAttachSession(t, argv, "sess-e2e")
}
