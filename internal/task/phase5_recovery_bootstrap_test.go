package task

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// countingOCG5 计数 Health/Probe/ListSessions（G5-3④ 正式进程第二轮校验）。
type countingOCG5 struct {
	*mockOC
	tr     *opTrace
	mu     sync.Mutex
	health int
	probe  int
	list   int
}

type newTraceProc struct {
	*mockProc
	tr *opTrace
}

func (p *newTraceProc) NewSession(spec process.SessionSpec) error {
	if p.tr != nil {
		p.tr.add("new:" + spec.Name)
	}
	return p.mockProc.NewSession(spec)
}

func (c *countingOCG5) Health(ctx context.Context) (opencode.HealthResponse, error) {
	c.mu.Lock()
	c.health++
	c.mu.Unlock()
	if c.tr != nil {
		c.tr.add("health")
	}
	return c.mockOC.Health(ctx)
}

func (c *countingOCG5) Probe(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.probe++
	c.mu.Unlock()
	if c.tr != nil {
		c.tr.add("probe")
	}
	return c.mockOC.Probe(ctx)
}

func (c *countingOCG5) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	c.mu.Lock()
	c.list++
	c.mu.Unlock()
	if c.tr != nil {
		c.tr.add("list")
	}
	return c.mockOC.ListSessions(ctx, dir, limit)
}

func (c *countingOCG5) counts() (health, probe, list int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health, c.probe, c.list
}

// strictReuseProc 同名会话仍在 sessions 时拒绝 NewSession（G5-3③：漏 kill 会暴露覆盖）。
type strictReuseProc struct {
	*mockProc
	tr *opTrace
}

func (p *strictReuseProc) NewSession(spec process.SessionSpec) error {
	p.mockProc.mu.Lock()
	exists := p.mockProc.sessions[spec.Name]
	p.mockProc.mu.Unlock()
	if exists {
		if p.tr != nil {
			p.tr.add("overlap:" + spec.Name)
		}
		return fmt.Errorf("session %s still exists (confirm terminated before reuse)", spec.Name)
	}
	if p.tr != nil {
		p.tr.add("new:" + spec.Name)
	}
	return p.mockProc.NewSession(spec)
}

func (p *strictReuseProc) KillSession(name string) (process.KillResult, error) {
	if p.tr != nil {
		p.tr.add("kill:" + name)
	}
	return p.mockProc.KillSession(name)
}

// nthCreateFailProc 在第 failAt 次 runtime NewSession 失败（G5-3⑤ 第二次启动补偿）。
type nthCreateFailProc struct {
	*mockProc
	failAt int
	n      atomic.Int64
}

func (p *nthCreateFailProc) NewSession(spec process.SessionSpec) error {
	if spec.Name == runtimeSessionName("t1") {
		n := int(p.n.Add(1))
		if p.failAt > 0 && n >= p.failAt {
			return fmt.Errorf("injected second-start failure")
		}
	}
	return p.mockProc.NewSession(spec)
}

func startRecoveryFromActive(t *testing.T, store TaskStore, proc ProcessBackend, oc OCClient) *Manager {
	t.Helper()
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.SetLifecycleCtx(context.Background())
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.ensureRecovery("t1", rt.instVersion)
	return m
}

func seedActiveForRecovery(store *mockStore) {
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
}

// TestRecovery_AnchoredSessionArgvAndListCheck 验证 G5-3①：有锚定时启动 argv 带
// `--session <id>`，就绪后列表校验通过，不走 POST/双启动。
func TestRecovery_AnchoredSessionArgvAndListCheck(t *testing.T) {
	store := newMockStore()
	seedActiveForRecovery(store)
	store.mutTask("t1", func(r *TaskRow) {
		r.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	proc := newMockProc()
	oc := &countingOCG5{mockOC: newMockOC(true)}
	oc.sessions = []opencode.Session{{ID: "sess-existing", Time: opencode.SessionTime{Created: 1, Updated: 1}}}

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	argv := runtimeCmdArgvOf(proc, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-existing")
	if got := runtimeCreateCount(proc, "t1"); got != 1 {
		t.Fatalf("creates=%d want 1 (anchored, no dual-start)", got)
	}
	if got := oc.createSessionCountLoad(); got != 0 {
		t.Fatalf("CreateSession=%d want 0", got)
	}
	_, _, lists := oc.counts()
	if lists < 1 {
		t.Fatal("anchor list check must run (ListSessions)")
	}
}

// TestRecovery_StaleAnchorRebuilds 验证 G5-3②：锚定不在列表 → 条件清空转无锚定，
// POST+claim 后双启动落到新锚定。
func TestRecovery_StaleAnchorRebuilds(t *testing.T) {
	store := newMockStore()
	seedActiveForRecovery(store)
	store.mutTask("t1", func(r *TaskRow) {
		r.AnchorSessionID = sql.NullString{String: "sess-gone", Valid: true}
	})
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-rebuilt", Time: opencode.SessionTime{Created: 10, Updated: 20}}

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 5*time.Second, StatusActive)

	row, _ := store.GetTask(context.Background(), "t1")
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-rebuilt" {
		t.Fatalf("anchor=%+v want sess-rebuilt", row.AnchorSessionID)
	}
	if got := oc.createSessionCountLoad(); got != 1 {
		t.Fatalf("CreateSession=%d want 1", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got < 2 {
		t.Fatalf("creates=%d want >=2 (stale start + dual-start)", got)
	}
	argv := runtimeCmdArgvOf(proc, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-rebuilt")
}

// TestRecovery_ClaimConflictFails 验证 G5-3②：claim 冲突 → 恢复失败，不写锚定。
func TestRecovery_ClaimConflictFails(t *testing.T) {
	inner := newMockStore()
	store := &conflictClaimStore{mockStore: inner}
	seedActiveForRecovery(inner)
	proc := newMockProc()
	oc := newMockOC(true)

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, inner, "t1", 5*time.Second, StatusSuspended)

	row, _ := inner.GetTask(context.Background(), "t1")
	if row.AnchorSessionID.Valid {
		t.Fatalf("conflict must not set anchor: %+v", row.AnchorSessionID)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "conflict") {
		t.Errorf("last_error=%v want conflict", row.LastError)
	}
}

// TestRecovery_ConfirmTerminatedBeforeRuntimeReuse 验证 G5-3③：bootstrap 确认终止
// 后才复用 -runtime。顺序 trace 为 new → kill → new；同名仍存活时 NewSession 拒绝，
// 漏 kill 会变红。
func TestRecovery_ConfirmTerminatedBeforeRuntimeReuse(t *testing.T) {
	store := newMockStore()
	seedActiveForRecovery(store)
	tr := &opTrace{}
	proc := &strictReuseProc{mockProc: newMockProc(), tr: tr}
	oc := newMockOC(true)

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	ops := tr.snapshot()
	runtimeName := runtimeSessionName("t1")
	wantNew := "new:" + runtimeName
	wantKill := "kill:" + runtimeName
	wantOverlap := "overlap:" + runtimeName
	var filtered []string
	for _, op := range ops {
		if op == wantNew || op == wantKill || op == wantOverlap {
			filtered = append(filtered, op)
		}
	}
	if len(filtered) < 3 || filtered[0] != wantNew || filtered[1] != wantKill || filtered[2] != wantNew {
		t.Fatalf("order=%v want %s, %s, %s", filtered, wantNew, wantKill, wantNew)
	}
	for _, op := range filtered {
		if op == wantOverlap {
			t.Fatal("NewSession overlapped live -runtime (missed confirm kill)")
		}
	}
}

// TestRecovery_FormalProcessSecondRoundHealthProbeAnchor 验证 G5-3④：双启动正式
// 进程重新健康+探测+锚定列表校验。
func TestRecovery_FormalProcessSecondRoundHealthProbeAnchor(t *testing.T) {
	store := newMockStore()
	seedActiveForRecovery(store)
	tr := &opTrace{}
	inner := newMockProc()
	proc := &newTraceProc{mockProc: inner, tr: tr}
	oc := &countingOCG5{mockOC: newMockOC(true), tr: tr}

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	if got := runtimeCreateCount(inner, "t1"); got != 2 {
		t.Fatalf("creates=%d want 2", got)
	}
	health, probe, list := oc.counts()
	if health < 3 {
		t.Fatalf("Health=%d want >=3 (bootstrap wait + formal wait + commit final)", health)
	}
	if probe < 2 {
		t.Fatalf("Probe=%d want >=2 (bootstrap + formal)", probe)
	}
	if list < 1 {
		t.Fatalf("ListSessions=%d want >=1 (formal anchor check)", list)
	}
	runtimeNew := "new:" + runtimeSessionName("t1")
	ops := tr.snapshot()
	news := 0
	var window []string
	for _, op := range ops {
		if op == runtimeNew {
			news++
			if news == 2 {
				window = nil
				continue
			}
		}
		if news == 2 {
			window = append(window, op)
			if op == "probe" || op == "list" {
				break
			}
		}
	}
	if news != 2 {
		t.Fatalf("runtime NewSession count=%d want 2; ops=%v", news, ops)
	}
	hasHealth := false
	for _, op := range window {
		if op == "health" {
			hasHealth = true
			break
		}
	}
	if !hasHealth {
		t.Fatalf("formal wait health missing after 2nd NewSession before probe/list; window=%v ops=%v", window, ops)
	}
	argv := runtimeCmdArgvOf(inner, "t1")
	assertRuntimeCmd(t, argv, 0, "sess-new")
}

// TestRecovery_SecondStartFailureCompensates 验证 G5-3⑤：正式进程第二次启动失败
// 走终态补偿，不留 active 无托管 runtime。
func TestRecovery_SecondStartFailureCompensates(t *testing.T) {
	store := newMockStore()
	seedActiveForRecovery(store)
	proc := &nthCreateFailProc{mockProc: newMockProc(), failAt: 2}
	oc := newMockOC(true)

	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)

	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "second-start") {
		t.Errorf("last_error=%v want second-start failure", row.LastError)
	}
	if proc.sessions[runtimeSessionName("t1")] {
		t.Error("failed second start must not leave a live runtime session")
	}
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String != "sess-new" {
		t.Errorf("claimed anchor MUST be retained after second-start failure: %+v", row.AnchorSessionID)
	}
}
