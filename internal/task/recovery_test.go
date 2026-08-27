package task

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application/runtime"
	"ocdeck/internal/infrastructure/process"
)

func waitStatusAny(t *testing.T, store TaskStore, taskID string, timeout time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		for _, w := range want {
			if row.Status == w {
				return row.Status
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("status=%s want one of %v", row.Status, want)
	return row.Status
}

func TestEnsureRecovery_IdempotentDualTrigger(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime missing")
	}
	tok := rt.instVersion

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.ensureRecovery("t1", tok) }()
	go func() { defer wg.Done(); m.ensureRecovery("t1", tok) }()
	wg.Wait()

	got := waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)
	if got != StatusActive {
		t.Fatalf("status=%s want active after dual ensureRecovery", got)
	}
	n := len(store.recoveryAttempts["t1"])
	if n == 0 {
		t.Fatal("expected at least one recovery permit")
	}
	if n > 3 {
		t.Fatalf("permits=%d want ≤3 (idempotent incident)", n)
	}
}

func TestEnsureRecovery_BudgetExhaustedSuspends(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	now := m.nowUnix()
	for i := 0; i < 3; i++ {
		if _, err := store.AcquireRecoveryPermit(context.Background(), "t1", now); err != nil {
			t.Fatal(err)
		}
	}
	m.ensureRecovery("t1", rt.instVersion)
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended on budget exhausted", row.Status)
	}
	if !row.LastError.Valid || !contains(row.LastError.String, errRecoveryBudgetExhausted) {
		t.Errorf("last_error=%v want %q", row.LastError, errRecoveryBudgetExhausted)
	}
	if len(store.recoveryAttempts["t1"]) != 3 {
		t.Errorf("permits=%d want 3 (no extra permit on exhaust)", len(store.recoveryAttempts["t1"]))
	}
}

func TestEnsureRecovery_BackoffOrdinal(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	var ordinals []int
	m.recoveryBackoffFn = func(ordinal int) time.Duration {
		ordinals = append(ordinals, ordinal)
		return 0
	}
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)
	if len(ordinals) == 0 {
		t.Fatal("expected backoff ordinal after recovery")
	}
	if ordinals[0] != 1 {
		t.Errorf("first ordinal=%d want 1", ordinals[0])
	}
	_ = rt
}

func TestEnsureRecovery_StaleTokenNoCleanup(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	cur := m.getRuntime("t1")
	stale := runtime.InstVersion("stale-token")
	m.ensureRecovery("t1", stale)
	assertStatus(t, store, "t1", StatusActive)
	if m.getRuntime("t1") == nil || m.getRuntime("t1").instVersion != cur.instVersion {
		t.Fatal("stale token MUST NOT clear current runtime")
	}
}

func TestEnsureRecovery_ActivatingNoReentry(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	m.ensureRecovery("t1", rt.instVersion)
	assertStatus(t, store, "t1", StatusActivating)
	if len(store.recoveryAttempts["t1"]) != 0 {
		t.Errorf("activating reentry consumed permits: %v", store.recoveryAttempts["t1"])
	}
}

func TestEnsureRecovery_SuspendWinsAbandons(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	m.ensureRecovery("t1", runtime.InstVersion("any"))
	assertStatus(t, store, "t1", StatusSuspended)
}

func TestCompleteRecoveryFailure_StoreCASMismatchZeroModify(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.LastError = sql.NullString{String: "keep", Valid: true}
		r.EnvSnapshot = sql.NullString{String: `{"vars":{}}`, Valid: true}
	})
	le := sql.NullString{String: "new", Valid: true}
	res, err := store.CompleteRecoveryFailure(context.Background(), "t1", le)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched {
		t.Fatal("want !Matched")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status=%s want active", row.Status)
	}
	if !row.LastError.Valid || row.LastError.String != "keep" {
		t.Errorf("last_error=%v want keep", row.LastError)
	}
	if !row.EnvSnapshot.Valid {
		t.Error("env_snapshot cleared on mismatch")
	}
}

func TestEnsureRecovery_LockBusyThenRecovers(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Bool
	go func() {
		m.ensureRecovery("t1", rt.instVersion)
		done.Store(true)
	}()
	time.Sleep(80 * time.Millisecond)
	if done.Load() {
		t.Fatal("ensureRecovery returned while lock held")
	}
	unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !done.Load() {
		t.Fatal("ensureRecovery did not finish after unlock")
	}
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive, StatusActivating, StatusSuspended)
}

func TestEnsureRecovery_PermitBeforeNewSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)
	if len(store.recoveryAttempts["t1"]) == 0 {
		t.Fatal("permit must be written before NewSession")
	}
}

func TestIsRecoveryTerminal_Budget(t *testing.T) {
	if !isRecoveryTerminal(errRecoveryBudgetExhaustedSentinel) {
		t.Fatal("budget exhausted must be terminal")
	}
	if isRecoveryTerminal(errors.New("runtime session: boom")) {
		t.Fatal("NewSession error should retry")
	}
}
