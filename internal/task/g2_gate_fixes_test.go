package task

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

type clearAnchorStore struct {
	*mockStore
	clearErr   error
	mismatchTo sql.NullString
}

func (s *clearAnchorStore) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	if s.clearErr != nil {
		return application.MutationResult{}, s.clearErr
	}
	s.mu.Lock()
	t := s.tasks[taskID]
	t.AnchorSessionID = s.mismatchTo
	s.tasks[taskID] = t
	s.mu.Unlock()
	return application.MutationResult{}, nil
}

func TestActivate_ClearAnchorStoreError(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-gone", Valid: true}
	})
	store := &clearAnchorStore{mockStore: inner, clearErr: errors.New("db: clear anchor")}
	oc := newMockOC(true)
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), oc)
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate must fail on clear-anchor store error")
	}
	if !strings.Contains(err.Error(), "clear stale anchor") {
		t.Errorf("err=%v, want clear stale anchor", err)
	}
	assertStatus(t, inner, "t1", StatusSuspended)
}

func TestActivate_ClearAnchorCASMismatchToNULL(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-gone", Valid: true}
	})
	store := &clearAnchorStore{mockStore: inner, mismatchTo: sql.NullString{}}
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 1, Updated: 1}}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	assertStatus(t, inner, "t1", StatusActive)
	row, _ := inner.GetTask(context.Background(), "t1")
	if row.AnchorSessionID.String != "sess-fresh" {
		t.Errorf("anchor=%+v want sess-fresh (NULL 后走无锚定)", row.AnchorSessionID)
	}
}

func TestActivate_ClearAnchorCASMismatchNewAnchorMissing(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-old", Valid: true}
	})
	store := &clearAnchorStore{
		mockStore:  inner,
		mismatchTo: sql.NullString{String: "sess-new", Valid: true},
	}
	oc := newMockOC(true)
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("new anchor missing from list must not confirm into active")
	}
	assertStatus(t, inner, "t1", StatusSuspended)
	_ = proc
}

func TestKillResidualSessions_RuntimeClean(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	if err := proc.NewSession(process.SessionSpec{
		Name: runtimeSessionName("t1"), Dir: "/tmp", CmdArgv: []string{"sleep", "1"},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	if err := m.killResidualSessions(context.Background(), "t1"); err != nil {
		t.Fatalf("killResidualSessions: %v", err)
	}
	if proc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime session must be killed")
	}
}

func TestKillResidualSessions_RuntimeNonClean(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	err := m.killResidualSessions(context.Background(), "t1")
	if err == nil {
		t.Fatal("non-clean runtime kill must fail")
	}
}

func TestKillResidualSessions_HasSessionInfraError(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.hasSessionErr = errors.New("tmux infra")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	err := m.killResidualSessions(context.Background(), "t1")
	if err == nil {
		t.Fatal("HasSession infra error must fail")
	}
	if !strings.Contains(err.Error(), "has session") {
		t.Errorf("err=%v want has session", err)
	}
}

type holdHealthOC struct {
	*mockOC
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *holdHealthOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return opencode.HealthResponse{}, ctx.Err()
	}
	return c.mockOC.Health(ctx)
}

func TestReopenAttach_ActivateHoldsLockReturnsRecovering(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	oc := &holdHealthOC{mockOC: newMockOC(true), started: make(chan struct{}), release: make(chan struct{})}
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), oc)

	done := make(chan error, 1)
	go func() { done <- m.Activate(context.Background(), "t1") }()
	select {
	case <-oc.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Activate did not reach Health")
	}
	_, err := m.ReopenAttach(context.Background(), "t1")
	close(oc.release)
	actErr := <-done
	if actErr != nil {
		t.Fatalf("Activate: %v", actErr)
	}
	if err == nil {
		t.Fatal("ReopenAttach during Activate must fail")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Fatalf("code=%s want recovering (not generic conflict); err=%v", OpErrorCode(err), err)
	}
}

type mismatchActiveCASStore struct{ *mockStore }

func (s *mismatchActiveCASStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if fromStatus == StatusActivating && toStatus == StatusActive {
		return application.TransitionResult{}, nil
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func TestActivate_CASMismatch_NoGenericCompensation(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	store := &mismatchActiveCASStore{mockStore: inner}
	proc := newMockProc()
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"cas-tk"},
	}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("CAS mismatch must fail")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%s want conflict", OpErrorCode(err))
	}
	row, _ := inner.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Errorf("status=%s want activating (禁写 status)", row.Status)
	}
	if row.LastError.Valid {
		t.Errorf("last_error must stay empty, got %q", row.LastError.String)
	}
	if !row.EnvSnapshot.Valid {
		t.Error("env_snapshot must not be cleared")
	}
	if row.AnchorSessionID.String != "sess-existing" {
		t.Errorf("anchor mutated: %+v", row.AnchorSessionID)
	}
	entries, _ := parseNotices(row.Notice)
	if n := countResidualBySession(entries, runtimeSessionName("t1")); n != 1 {
		t.Fatalf("notice count=%d want 1 (generic compensation would duplicate)", n)
	}
	if m.getRuntime("t1") != nil {
		t.Error("attempt runtime must be cleared")
	}
}

func TestRollbackAttemptRuntime_SkipsNewToken(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	oldRT := m.newRuntime("t1")
	m.setRuntime("t1", oldRT)
	oldRT.registerGroup(roleRuntime, runtimeSessionName("t1"))
	newRT := m.newRuntime("t1")
	m.setRuntime("t1", newRT)
	newRT.registerGroup(roleRuntime, runtimeSessionName("t1"))

	if err := m.rollbackAttemptRuntime(context.Background(), "t1", runtimeSessionName("t1"), oldRT.instVersion); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if cur := m.getRuntime("t1"); cur == nil || cur.instVersion != newRT.instVersion {
		t.Fatal("new token runtime must be untouched")
	}
	if !proc.sessions[runtimeSessionName("t1")] {
		t.Error("must not kill session owned by new token")
	}
}

type rereadFailCASStore struct {
	*mockStore
	arm bool
}

func (s *rereadFailCASStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if fromStatus == StatusActivating && toStatus == StatusActive {
		s.arm = true
		return application.TransitionResult{}, nil
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func (s *rereadFailCASStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	if s.arm {
		return TaskRow{}, errors.New("reread failed")
	}
	return s.mockStore.GetTask(ctx, id)
}

func TestActivate_CASMismatchRereadFail_NoGenericCompensation(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	store := &rereadFailCASStore{mockStore: inner}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("reread failure must fail Activate")
	}
	row, _ := inner.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Errorf("status=%s want activating", row.Status)
	}
	if row.LastError.Valid {
		t.Errorf("last_error must stay empty, got %q", row.LastError.String)
	}
	if !row.EnvSnapshot.Valid {
		t.Error("env_snapshot must not be cleared")
	}
	if row.AnchorSessionID.String != "sess-existing" {
		t.Errorf("anchor mutated: %+v", row.AnchorSessionID)
	}
}

type casMismatchNoticeFailStore struct {
	*noConvergeStore
}

func (s *casMismatchNoticeFailStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if fromStatus == StatusActivating && toStatus == StatusActive {
		return application.TransitionResult{}, nil
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func TestActivate_CASMismatch_NoticeFailCreatesDebt(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	store := &casMismatchNoticeFailStore{noConvergeStore: &noConvergeStore{mockStore: inner}}
	debt := newMemCleanupDebtStore()
	proc := newMockProc()
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"debt-tk"},
	}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newR7TestManagerWithDebt(t, store, debt, proc, newMockWorktree(), oc)
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("CAS mismatch must fail")
	}
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("want pendingCleanupError debt, got %T %v", err, err)
	}
	if len(pce.pending.cleanupTickets) == 0 || pce.pending.cleanupTickets[0] != "debt-tk" {
		t.Errorf("pending tickets=%v want debt-tk", pce.pending.cleanupTickets)
	}
	row, _ := inner.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Errorf("status=%s want activating", row.Status)
	}
	debt.mu.Lock()
	_, ok := debt.rows[runtimeSessionName("t1")]
	debt.mu.Unlock()
	if !ok {
		t.Error("cleanup debt must be persisted when notice write fails")
	}
}

type joinWatchProc struct {
	*mockProc
	done <-chan struct{}
}

func (p *joinWatchProc) WatchExit(name string, callback func(process.WatchEvent)) (func(), <-chan struct{}) {
	c, d := p.mockProc.WatchExit(name, callback)
	p.done = d
	return c, d
}

func TestActivate_CASMismatch_WatchJoinedBeforeReturn(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	store := &mismatchActiveCASStore{mockStore: inner}
	proc := &joinWatchProc{mockProc: newMockProc()}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	if err := m.Activate(context.Background(), "t1"); err == nil {
		t.Fatal("CAS mismatch must fail")
	}
	if proc.done == nil {
		t.Fatal("WatchExit was not registered")
	}
	select {
	case <-proc.done:
	default:
		t.Fatal("watch done must be closed before Activate returns (join)")
	}
}

type failLastPortStore struct{ *mockStore }

func (s *failLastPortStore) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return application.MutationResult{}, errors.New("last port write failed")
}

func TestActivate_LastPortFail_ReverseCleanup(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	store := &failLastPortStore{mockStore: inner}
	proc := newMockProc()
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"lp-tk"},
	}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	if err := m.Activate(context.Background(), "t1"); err == nil {
		t.Fatal("last_port failure must fail Activate")
	}
	assertStatus(t, inner, "t1", StatusSuspended)
	if proc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime must be cleaned after last_port failure")
	}
}

type failAfterSubscribeOC struct {
	*mockOC
	entered sync.Once
	subbed  bool
}

func (c *failAfterSubscribeOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	c.entered.Do(func() { c.subbed = true })
	return c.mockOC.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}

func (c *failAfterSubscribeOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	if c.subbed {
		return opencode.HealthResponse{}, errors.New("final health failed")
	}
	return c.mockOC.Health(ctx)
}

func TestActivate_FinalHealthFail_ReverseCleanup(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	oc := &failAfterSubscribeOC{mockOC: newMockOC(true)}
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	if err := m.Activate(context.Background(), "t1"); err == nil {
		t.Fatal("final health failure must fail Activate")
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if proc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime must be cleaned after final health failure")
	}
}

func TestKillResidualSessions_RuntimeNonCleanStillClearsOthers(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	tui := tuiSessionName("t1")
	shell := shellSessionName("t1", 1)
	rt := runtimeSessionName("t1")
	proc.sessions[tui] = true
	proc.sessions[shell] = true
	proc.sessions[rt] = true
	proc.killResults[rt] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"rt-tk"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	err := m.killResidualSessions(context.Background(), "t1")
	if err == nil {
		t.Fatal("runtime non-clean must still return error")
	}
	if proc.sessions[tui] {
		t.Error("tui must still be killed")
	}
	if proc.sessions[shell] {
		t.Error("shell must still be killed")
	}
	order := proc.killOrderSnapshot()
	if len(order) < 3 {
		t.Fatalf("kill order=%v, want tui/shell before runtime", order)
	}
	if order[0] != tui {
		t.Errorf("first kill=%s want tui", order[0])
	}
}
