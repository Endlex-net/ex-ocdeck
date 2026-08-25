package task

// p147_converge_debt_test.go 验证 OpenSpec change sse-active-sessions P1.4.7：
// Suspend DB 写经 LifecycleService write* helper 路由、收敛入口令牌贯穿
//（design.md D0:150）、锁超时无锁 cleanup+CAS 被替换为令牌校验后的两阶段
// preCleanup/postCleanup 债务（design.md D0:151/D2）、worker 持锁消化债务。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application/runtime"
	apptask "ocdeck/internal/application/task"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/process"
)

// --- Suspend 经 LifecycleService 路由 ---

// TestP147_Suspend_ViaLifecycle 注入 LifecycleService 后 active 任务 Suspend 收敛 suspended，
// env 快照清空，且 P141 冻结的副作用顺序保持（CAS active→suspending → KillSession(serve)
// → UpdateTaskEnvSnapshot(valid=false) → UpdateTaskStatus(suspended)）。
func TestP147_Suspend_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	tr := &tracer{}
	m := newP146TestManager(t, wrapTraceStore(store, tr), wrapTraceProc(proc, tr), newMockWorktree(), newMockOC(true))
	// 构造 runtime（Suspend 入口 clearRuntime 需 runtime 存在以停 SSE/watch）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	row, _ := store.GetTask(context.Background(), "t1")
	if row.EnvSnapshot.Valid {
		t.Errorf("env snapshot must be cleared after suspend, got %v", row.EnvSnapshot)
	}
	// P141 顺序冻结（经 lifecycle 路由后副作用顺序不变）。
	assertOrdered(t, tr, []traceOp{
		{src: "store", op: "UpdateTaskStatusConditional", key: "active->suspending"},
		{src: "proc", op: "KillSession", key: serveSessionName("t1")},
		{src: "store", op: "UpdateTaskEnvSnapshot", key: "valid=false"},
		{src: "store", op: "UpdateTaskStatus", key: "status=suspended"},
	}, "Suspend.viaLifecycle")
	assertOpCount(t, tr, "store", "UpdateTaskStatusConditional", 1, "Suspend.viaLifecycle")
}

// TestP147_Suspend_GuardReject_ViaLifecycle 非 active → invalid_state 零副作用（无 CAS）。
func TestP147_Suspend_GuardReject_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 已 suspended
	tr := &tracer{}
	m := newP146TestManager(t, wrapTraceStore(store, tr), newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected invalid_state on non-active suspend")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("code = %s, want invalid_state", OpErrorCode(err))
	}
	assertNoSideEffects(t, tr, "Suspend.guardReject.viaLifecycle")
	assertOpNever(t, tr, "store", "UpdateTaskStatusConditional", "Suspend.guardReject.viaLifecycle")
	assertOpNever(t, tr, "store", "UpdateTaskStatus", "Suspend.guardReject.viaLifecycle")
}

// --- 锁超时分支：登记债务、不得无锁清理/CAS ---

// p147RuntimeWithSessions 构造带存活 serve/tui 会话与已安装 runtime 的 active 任务，
// 返回触发令牌（当前 runtime 令牌）。
func p147RuntimeWithSessions(t *testing.T, m *Manager) runtime.InstVersion {
	t.Helper()
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	return rt.instVersion
}

// TestP147_LockTimeout_RegistersPreCleanup_NoUnlockCleanup 直接单测锁超时分支
//（onConvergeLockTimeout）：当前 runtime 令牌 == 触发令牌 → 登记 preCleanup；
// MUST NOT 无锁清理（runtime 仍在、会话仍在）且 MUST NOT CAS（状态仍 active）。
func TestP147_LockTimeout_RegistersPreCleanup_NoUnlockCleanup(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	tok := p147RuntimeWithSessions(t, m)

	m.onConvergeLockTimeout("t1", "serve session exited unexpectedly; converge lock wait timed out", tok)

	entry, ok := m.runtimeRegistry.Get("t1")
	if !ok {
		t.Fatal("preCleanup debt MUST be registered on lock timeout with current token")
	}
	if entry.Phase != runtime.DebtPhasePreCleanup || entry.Token != tok {
		t.Fatalf("debt entry = %+v, want phase=preCleanup token=%+v", entry, tok)
	}
	// 无锁清理未发生：runtime 仍在、会话仍在、状态仍 active。
	if m.getRuntime("t1") == nil {
		t.Fatal("runtime MUST NOT be cleaned without holding task lock")
	}
	for _, name := range []string{serveSessionName("t1"), tuiSessionName("t1")} {
		if alive, _ := proc.HasSession(name); !alive {
			t.Fatalf("session %s MUST NOT be killed without holding task lock", name)
		}
	}
	assertStatus(t, store, "t1", StatusActive)
}

// TestP147_LockTimeout_StaleToken_NoRegister 触发令牌非当前代 → 旧代 stale callback：
// 不登记、不清理、不 CAS。
func TestP147_LockTimeout_StaleToken_NoRegister(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 安装当前 runtime（tombstone 随分配推进到当前令牌）。
	p147RuntimeWithSessions(t, m)

	// 不同于当前令牌的触发令牌（tombstone 已推进到当前，此值为旧代）。
	stale := runtime.InstVersion("01724000000123-stale0")
	m.onConvergeLockTimeout("t1", "stale callback", stale)

	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("stale trigger token MUST NOT register debt")
	}
	if m.getRuntime("t1") == nil {
		t.Fatal("runtime MUST stay (no cleanup)")
	}
	if alive, _ := proc.HasSession(serveSessionName("t1")); !alive {
		t.Fatal("serve session MUST stay (no cleanup)")
	}
	assertStatus(t, store, "t1", StatusActive)
}

// TestP147_LockTimeout_NilRuntimeTombstoneMatch_RegistersPostCleanup cleanup 已在等锁期间
// 完成（runtime 已清、tombstone == 触发令牌）→ 登记 postCleanup；不 CAS（状态仍 active）。
func TestP147_LockTimeout_NilRuntimeTombstoneMatch_RegistersPostCleanup(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	tok := p147RuntimeWithSessions(t, m)
	// 模拟等锁期间他人完成清理（Suspend/收敛已 clearRuntime；tombstone 保留令牌）。
	m.clearRuntime("t1")

	m.onConvergeLockTimeout("t1", "serve session exited unexpectedly", tok)

	entry, ok := m.runtimeRegistry.Get("t1")
	if !ok {
		t.Fatal("postCleanup debt MUST be registered when tombstone matches trigger")
	}
	if entry.Phase != runtime.DebtPhasePostCleanup || entry.Token != tok {
		t.Fatalf("debt entry = %+v, want phase=postCleanup token=%+v", entry, tok)
	}
	// 超时路径不 CAS：状态仍 active。
	assertStatus(t, store, "t1", StatusActive)
}

// --- watcher 令牌贯穿 ---

// TestP147_WatcherThreadsToken watcher 回调路径端到端：Activate 注册 watcher 后触发
// serve 退出事件，回调以注册时令牌进入 handleServeExit → 收敛 suspended。
func TestP147_WatcherThreadsToken(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	waitStatus(t, store, "t1", StatusActive, 2*time.Second)
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime must exist after Activate")
	}

	// 触发 serve 退出事件：watchServeExit 回调 matchesRegistry 校验通过后携带注册令牌收敛。
	proc.triggerExit(serveSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})

	assertStatus(t, store, "t1", StatusSuspended)
	if m.getRuntime("t1") != nil {
		t.Fatal("runtime must be cleaned after serve exit converge")
	}
}

// --- worker 消化债务 ---

// TestP147_Worker_PreCleanup_CleansThenCAS preCleanup 债务 + 存活 runtime + active 任务：
// processConvergeDebts 一个 tick 内持锁清理 → 推进 postCleanup → CAS committed → 债务删除。
func TestP147_Worker_PreCleanup_CleansThenCAS(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	tok := p147RuntimeWithSessions(t, m)

	// 经锁超时分支登记 preCleanup（等价生产路径：等锁失败 + 令牌仍当前）。
	m.onConvergeLockTimeout("t1", "serve session exited unexpectedly; converge lock wait timed out", tok)
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePreCleanup {
		t.Fatalf("prereq preCleanup debt missing: %+v (ok=%v)", entry, ok)
	}

	// worker tick：持锁清理 + CAS。
	m.processConvergeDebts(context.Background())

	assertStatus(t, store, "t1", StatusSuspended)
	if m.getRuntime("t1") != nil {
		t.Fatal("runtime must be cleaned by worker preCleanup")
	}
	for _, name := range []string{serveSessionName("t1"), tuiSessionName("t1")} {
		if alive, _ := proc.HasSession(name); alive {
			t.Fatalf("session %s must be killed by worker preCleanup", name)
		}
	}
	// ① committed → 债务 compare-and-delete。
	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("debt MUST be deleted after committed CAS (matrix ①)")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.EnvSnapshot.Valid {
		t.Error("env snapshot must be cleared by worker converge")
	}
}

// TestP147_Worker_SkipsBusyLock 锁忙任务跳过本轮 tick（不阻塞、不清理）。
func TestP147_Worker_SkipsBusyLock(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	tok := p147RuntimeWithSessions(t, m)
	m.onConvergeLockTimeout("t1", "lock timeout", tok)

	// 占用任务锁：worker 跳过本轮（不清理、不 CAS、债务保留）。
	unlock, lerr := m.tryLockTask("t1")
	if lerr != nil {
		t.Fatalf("tryLockTask: %v", lerr)
	}
	m.processConvergeDebts(context.Background())
	unlock()

	assertStatus(t, store, "t1", StatusActive)
	if m.getRuntime("t1") == nil {
		t.Fatal("runtime must stay when task lock busy")
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePreCleanup {
		t.Fatalf("debt must survive busy-lock tick: %+v (ok=%v)", entry, ok)
	}
}

// --- 新代超时登记原子替换旧债（ora-16 finding 1/2）---

// TestP147_LockTimeout_NewRuntimeReplacesOldDebt 旧债存在时新代锁超时：新触发令牌
// MUST 原子替换旧注册项（不得被旧债挡住导致任务留在 active 且无托管 SSE）。
func TestP147_LockTimeout_NewRuntimeReplacesOldDebt(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 第一代锁超时 → 登记 tok1 preCleanup。
	rt1 := m.newRuntime("t1")
	m.setRuntime("t1", rt1)
	tok1 := rt1.instVersion
	m.onConvergeLockTimeout("t1", "gen1 lock timeout", tok1)
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Token != tok1 {
		t.Fatalf("prereq: tok1 debt missing: %+v (ok=%v)", entry, ok)
	}

	// 模拟换代：旧 runtime 清理、新 runtime 安装（tombstone 推进到 tok2）。
	m.clearRuntime("t1")
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	tok2 := rt2.instVersion

	// 第二代锁超时：新令牌登记必须替换旧债。
	m.onConvergeLockTimeout("t1", "gen2 lock timeout", tok2)

	entry, ok := m.runtimeRegistry.Get("t1")
	if !ok {
		t.Fatal("new-gen lock timeout MUST register its debt (old debt must not block)")
	}
	if entry.Token != tok2 {
		t.Fatalf("debt token = %+v, want new token %+v (atomic replace)", entry.Token, tok2)
	}
	if entry.Phase != runtime.DebtPhasePreCleanup {
		t.Fatalf("phase = %d, want preCleanup (new-gen runtime is live)", entry.Phase)
	}
}

// --- W②b / W③b compare-and-delete（ora-16 finding 3）---

// p147RecordingPublisher 记录型 Publisher（断言 resync.requested 调用位；
// 生产语义由 NoopPublisher 占位，P1.6 挂接真实事件生产）。
type p147RecordingPublisher struct {
	mu     sync.Mutex
	events []ocdeckevent.Event
}

func (p *p147RecordingPublisher) Publish(e ocdeckevent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *p147RecordingPublisher) resyncCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e.Type == ocdeckevent.TypeResyncRequested {
			n++
		}
	}
	return n
}

// p147ManagerWithRecordingPublisher 构造注入 LifecycleService + 记录型 Publisher 的
// Manager（CAS 写经 lifecycle 路由，resync.requested 可观测）。
func p147ManagerWithRecordingPublisher(t *testing.T, store TaskStore) (*Manager, *p147RecordingPublisher) {
	t.Helper()
	pub := &p147RecordingPublisher{}
	adapter := &mockAppAdapter{s: store}
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.lifecycle = apptask.New(apptask.Options{Tasks: adapter, Read: adapter, Publish: pub})
	return m, pub
}

// p147SeedPostCleanupDebt 构造 runtime 并登记其令牌的 postCleanup 债务
//（runtime 已清、tombstone 保留令牌，与 worker postCleanup 前置一致）。
func p147SeedPostCleanupDebt(t *testing.T, m *Manager) runtime.InstVersion {
	t.Helper()
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	tok := rt.instVersion
	m.clearRuntime("t1")
	registered, _ := m.runtimeRegistry.RegisterIfCurrent("t1", tok, runtime.DebtPhasePostCleanup, nil)
	if !registered {
		t.Fatal("prereq: postCleanup debt register failed")
	}
	return tok
}

// TestP147_Matrix_W3b_CommittedFalseNonActive_DeletesDebtNoPublish W③b：
// CAS 未命中（!Matched 无错误）+ 重读非 active → compare-and-delete 债务，不发布。
func TestP147_Matrix_W3b_CommittedFalseNonActive_DeletesDebtNoPublish(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 已 suspended（CAS active→suspended 必不命中）
	m, pub := p147ManagerWithRecordingPublisher(t, store)
	tok := p147SeedPostCleanupDebt(t, m)

	// 持锁提交段：env 清 + CAS(active→suspended) 在 suspended 行上 !Matched → ③b。
	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, nil)

	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("W③b MUST compare-and-delete debt when reread is non-active")
	}
	if n := pub.resyncCount(); n != 0 {
		t.Fatalf("W③b MUST NOT publish, got %d resync events", n)
	}
}

// TestP147_Matrix_W2b_StatusErrNonActive_ResyncThenDeletesDebt W②b：
// CAS 写错误 + 重读非 active → resync.requested 恰好一次 + compare-and-delete，不登记。
func TestP147_Matrix_W2b_StatusErrNonActive_ResyncThenDeletesDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 已 suspended（重读非 active）
	// 注入 CAS 写错误：wrapper 仅对 UpdateTaskStatusConditional(active→suspended) 报错；
	// 读侧（lifecycle.Get 重读判定）经包装透传，仍读到 suspended。
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	tok := p147SeedPostCleanupDebt(t, m)

	// 持锁提交段：CAS 写失败（statusErr 非 nil）→ ② → resync → 重读 suspended → ②b 删除。
	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, nil)

	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("W②b MUST compare-and-delete debt when reread is non-active despite status error")
	}
	if n := pub.resyncCount(); n != 1 {
		t.Fatalf("W②b MUST publish exactly one resync.requested, got %d", n)
	}
}
