package task

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/opencode"
)

func runtimeCmdArgvOf(p *mockProc, taskID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmdArgvValues[runtimeSessionName(taskID)]
}

func assertRuntimeCmd(t *testing.T, argv []string, port int, sessionID string) {
	t.Helper()
	if len(argv) < 5 {
		t.Fatalf("runtime argv too short: %v", argv)
	}
	if argv[0] != "opencode" || argv[1] != "--port" {
		t.Fatalf("runtime argv prefix = %v, want opencode --port", argv[:2])
	}
	if argv[3] != "--hostname" || argv[4] != "127.0.0.1" {
		t.Fatalf("runtime argv hostname = %v", argv)
	}
	hasSession := false
	for i, a := range argv {
		if a == "--session" {
			hasSession = true
			if sessionID == "" {
				t.Fatalf("unanchored argv MUST NOT contain --session: %v", argv)
			}
			if i+1 >= len(argv) || argv[i+1] != sessionID {
				t.Fatalf("argv --session = %v, want %s", argv, sessionID)
			}
		}
		if a == "serve" || a == "attach" || a == "--continue" {
			t.Fatalf("forbidden arg %q in %v", a, argv)
		}
	}
	if sessionID != "" && !hasSession {
		t.Fatalf("anchored argv missing --session %s: %v", sessionID, argv)
	}
	_ = port
}

func TestActivate_UnanchoredCreatesClaimAndDualStarts(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	if got := oc.createSessionCountLoad(); got != 1 {
		t.Errorf("CreateSession called %d times, want 1", got)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-fresh" {
		t.Errorf("anchor = %+v, want sess-fresh", row.AnchorSessionID)
	}
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("must not create TUI session")
	}
	if !proc.sessions[runtimeSessionName("t1")] {
		t.Fatal("runtime session missing")
	}
	argv := runtimeCmdArgvOf(proc, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-fresh")
	if n := countNewServe(proc, "t1"); n < 2 {
		t.Errorf("NewSession(runtime) count=%d, want >=2 (bootstrap + formal)", n)
	}
	if n := countKillServe(proc, "t1"); n < 1 {
		t.Errorf("KillSession(runtime) count=%d, want >=1 (confirm bootstrap terminated)", n)
	}
}

func TestActivate_AnchoredSessionPresentNoCreate(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	proc := newMockProc()
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Errorf("CreateSession called %d times, want 0", got)
	}
	argv := runtimeCmdArgvOf(proc, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-existing")
	if n := countNewServe(proc, "t1"); n != 1 {
		t.Errorf("NewSession count=%d, want 1 (no dual-start)", n)
	}
}

func TestActivate_StaleAnchorClearsAndCreates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-gone", Valid: true}
	})
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 100, Updated: 200}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-new" {
		t.Errorf("anchor = %+v, want sess-new", row.AnchorSessionID)
	}
	if got := oc.createSessionCountLoad(); got != 1 {
		t.Errorf("CreateSession called %d, want 1", got)
	}
}

func TestActivate_ClaimConflictFails(t *testing.T) {
	store := &conflictClaimStore{mockStore: newMockStore()}
	seedSuspendedTask(store.mockStore, "t1", "p1")
	proc := newMockProc()
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate must fail on claim conflict")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("error = %v, want conflict", err)
	}
	assertStatus(t, store.mockStore, "t1", StatusSuspended)
	row, _ := store.GetTask(context.Background(), "t1")
	if row.AnchorSessionID.Valid {
		t.Errorf("conflict must not set anchor: %+v", row.AnchorSessionID)
	}
}

type statusOnProbeOC struct {
	*mockOC
	store     *mockStore
	probeSeen string
}

func (c *statusOnProbeOC) Probe(ctx context.Context) (string, error) {
	row, _ := c.store.GetTask(ctx, "t1")
	c.probeSeen = row.Status
	return c.mockOC.Probe(ctx)
}

func TestActivate_ProbeReadyNotActiveUntilCommit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	proc := newMockProc()
	oc := &statusOnProbeOC{mockOC: newMockOC(true), store: store}
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if oc.probeSeen != StatusActivating {
		t.Errorf("status at Probe = %q, want activating (probe 仅 ready)", oc.probeSeen)
	}
	assertStatus(t, store, "t1", StatusActive)
	if !store.tasks["t1"].LastPort.Valid {
		t.Error("last_port must be written in success commit")
	}
}

func TestReopenAttach_ActivatingReturnsRecovering(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) { tr.Status = StatusActivating })
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected recovering")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Errorf("code=%s want recovering", OpErrorCode(err))
	}
}
