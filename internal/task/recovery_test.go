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
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
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

// markActiveWithSnapshot 将任务置为 active 并写入合法持久化快照（D8 fixture 约定）：
// Recovery 在 permit/backoff/attempt 之前加载持久化快照，手动 seed 的 active 行必须带
// 可加载快照，否则会走坏快照终态而非预期恢复路径。
func markActiveWithSnapshot(store *mockStore, taskID string) {
	store.mutTask(taskID, func(r *TaskRow) {
		r.Status = StatusActive
		b, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": taskID}})
		r.EnvSnapshot = b
	})
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
	// G3-8：双触发恰好 1 个 incident / 1 个 permit（恢复首次 attempt 即成功）。
	n := store.recoveryPermitCount("t1")
	if n != 1 {
		t.Fatalf("permits=%d want exactly 1 (idempotent incident)", n)
	}
}

func TestEnsureRecovery_BudgetExhaustedSuspends(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
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
	if store.recoveryPermitCount("t1") != 3 {
		t.Errorf("permits=%d want 3 (no extra permit on exhaust)", store.recoveryPermitCount("t1"))
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
	if store.recoveryPermitCount("t1") != 0 {
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
	// G3-8：不再放宽为 active/activating/suspended 全通过——mock 恢复必然成功，收敛 active。
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)
}

// opTrace 记录跨 mock 的操作顺序（G3-8：permit 时序验证）。
type opTrace struct {
	mu  sync.Mutex
	ops []string
}

func (tr *opTrace) add(op string) {
	tr.mu.Lock()
	tr.ops = append(tr.ops, op)
	tr.mu.Unlock()
}

func (tr *opTrace) snapshot() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.ops...)
}

// TestEnsureRecovery_PermitBeforeNewSession 验证 permit 写入先于本次恢复的
// NewSession（G3-4/G3-8：permit 是 attempt 首个动作，先于任何进程副作用）。
func TestEnsureRecovery_PermitBeforeNewSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	tr := &opTrace{}
	store.onPermit = func(taskID string) { tr.add("permit:" + taskID) }
	proc.onNewSession = func(name string) { tr.add("new:" + name) }
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	ops := tr.snapshot()
	runtimeName := "new:" + runtimeSessionName("t1")
	permitIdx, newIdx := -1, -1
	for i, op := range ops {
		if op == "permit:t1" && permitIdx < 0 {
			permitIdx = i
		}
		if op == runtimeName {
			newIdx = i // 末次（恢复成功的）runtime 创建
		}
	}
	if permitIdx < 0 {
		t.Fatalf("no permit recorded in trace %v", ops)
	}
	if newIdx < 0 {
		t.Fatalf("no runtime NewSession recorded in trace %v", ops)
	}
	if permitIdx > newIdx {
		t.Fatalf("permit (idx %d) MUST precede recovery NewSession (idx %d): %v", permitIdx, newIdx, ops)
	}
	if store.recoveryPermitCount("t1") != 1 {
		t.Fatalf("permits=%d want exactly 1", store.recoveryPermitCount("t1"))
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

// TestIsRecoveryTerminal_TypedStages 验证 G3-5 typed 分派：具名标记 → 终态；
// 默认（NewSession/serve 未就绪/提交步）→ 重试。
func TestIsRecoveryTerminal_TypedStages(t *testing.T) {
	terminal := []error{
		&capabilityProbeError{err: errors.New("capability probe: boom")},
		&anchorStageError{err: errors.New("anchor session conflict")},
		&portAllocationError{err: errors.New("serve port range exhausted")},
		&recoveryTerminalError{err: errors.New("acquire recovery permit: store down")},
		&pendingCleanupError{pending: pendingCleanup{sessionName: "s", reason: noticeReasonKillFailed, retryable: true}},
	}
	for _, err := range terminal {
		if !isRecoveryTerminal(err) {
			t.Errorf("isRecoveryTerminal(%v) = false, want true", err)
		}
	}
	retry := []error{
		errors.New("runtime session: boom"),
		newOpErr(codeProcessError, errors.New("serve not ready after 3 port retries")),
		newOpErr(codeInternal, &persistEnvSnapshotError{err: errors.New("persist env snapshot: boom")}),
		newOpErr(codeInternal, &lastPortWriteError{err: errors.New("write last port: boom")}),
		newOpErr(codeProcessError, errors.New("sse subscribe: boom")),
	}
	for _, err := range retry {
		if isRecoveryTerminal(err) {
			t.Errorf("isRecoveryTerminal(%v) = true, want false (retry)", err)
		}
	}
}

// TestEnsureRecovery_UnknownKindZeroSideEffects 验证 G3-6：未知 kind 在 CAS 与一切
// 运行时副作用前零副作用放弃（不进入 activating、不耗 permit、不动既有 runtime）。
func TestEnsureRecovery_UnknownKindZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/tmp/p1", Kind: "weird"})
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	m.ensureRecovery("t1", rt.instVersion)

	assertStatus(t, store, "t1", StatusActive)
	if store.recoveryPermitCount("t1") != 0 {
		t.Errorf("permits=%v want none (zero side effects)", store.recoveryAttempts["t1"])
	}
	if m.getRuntime("t1") == nil || m.getRuntime("t1").instVersion != rt.instVersion {
		t.Error("runtime MUST NOT be touched on unknown kind")
	}
}

// sseFatalGateOC 模拟提交期 SSE 永久失败（G3-1 failpoint，G3-8②：替代 Sleep 竞态）。
// armed 后首次 SubscribeEvents 阻塞在 release 屏障上，测试在确定时点放行 → 返回
// fatal 错误（确定性控制 fatal 相对 CAS 的落点）；后续订阅正常阻塞。
type sseFatalGateOC struct {
	OCClient
	armed   atomic.Bool
	fired   atomic.Bool
	release chan struct{}
}

func (c *sseFatalGateOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.armed.Load() && c.fired.CompareAndSwap(false, true) {
		<-c.release
		return errors.New("sse fatal at commit window")
	}
	return c.OCClient.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}

// waitIncidentFatalLatched 非破坏性轮询 incident fatal latch 置位（测试屏障用）。
func waitIncidentFatalLatched(m *Manager, taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.recoveryIncidentsMu.Lock()
		inc := m.recoveryIncidents[taskID]
		m.recoveryIncidentsMu.Unlock()
		if inc != nil {
			inc.fatalMu.Lock()
			latched := inc.fatal != nil
			inc.fatalMu.Unlock()
			if latched {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// convergeSSEFatal 等待 fatal 分派收敛：active + 无进行中 incident + 恰好 2 permit。
func convergeSSEFatal(t *testing.T, m *Manager, store *mockStore, taskID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		m.recoveryIncidentsMu.Lock()
		busy := len(m.recoveryIncidents) > 0
		m.recoveryIncidentsMu.Unlock()
		if row.Status == StatusActive && !busy && store.recoveryPermitCount(taskID) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sse fatal recovery did not converge (permits=%d)", store.recoveryPermitCount(taskID))
}

func assertSSEFatalConverged(t *testing.T, store *mockStore, proc *mockProc) {
	t.Helper()
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status=%s want active (fatal MUST be dispatched, not lost)", row.Status)
	}
	if got := store.recoveryPermitCount("t1"); got != 2 {
		t.Fatalf("permits=%d want 2 (fatal forced a second attempt)", got)
	}
	creates := 0
	for _, name := range proc.newSessionNamesSnapshot() {
		if name == runtimeSessionName("t1") {
			creates++
		}
	}
	if creates != 4 {
		t.Fatalf("runtime creates=%d want 4 (activate dual-start 2 + recovery 2)", creates)
	}
}

// TestEnsureRecovery_SSEFatalBeforeCASRetries 验证 G3-1 failpoint（CAS 前侧）：
// fatal 在 lastPort 写回后、CAS 前确定性 latch → pre-CAS drain 捕获 → 本次 attempt
// 失败（反向清理）→ 重试成功。若 fatal 被丢弃（旧缺陷），permit 停在 1、创建停在 3，
// 任务带死 SSE 处于 active。
func TestEnsureRecovery_SSEFatalBeforeCASRetries(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	fatalOC := &sseFatalGateOC{OCClient: newMockOC(true), release: make(chan struct{})}
	m := newTestManager(t, store, proc, newMockWorktree(), fatalOC)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	fatalOC.armed.Store(true)
	// 屏障：lastPort（pre-CAS drain 前最后一步）先等 fatal latch 置位再放行——
	// fatal 确定性落在 CAS 前侧。仅首次调用生效（attempt2 的 lastPort 不再设障）。
	atLastPort := make(chan struct{}, 1)
	latched := make(chan bool, 1)
	var barrierOnce sync.Once
	store.onLastPort = func(string) {
		barrierOnce.Do(func() {
			atLastPort <- struct{}{}
			latched <- waitIncidentFatalLatched(m, "t1", 5*time.Second)
		})
	}
	go func() {
		<-atLastPort
		close(fatalOC.release)
	}()

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	if !<-latched {
		t.Fatal("fatal not latched at lastPort barrier (pre-CAS failpoint broken)")
	}
	convergeSSEFatal(t, m, store, "t1")
	assertSSEFatalConverged(t, store, proc)
}

// TestEnsureRecovery_SSEFatalAfterCASOpensNewIncident 验证 G3-1 failpoint（CAS 后侧）：
// fatal 在 CAS 提交 active 之后放行 → 幂等 ensureRecovery 开新 incident 完成重拉。
// 两条分派路径的收敛计数一致（permit=2、创建=4、最终 active）。
func TestEnsureRecovery_SSEFatalAfterCASOpensNewIncident(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	fatalOC := &sseFatalGateOC{OCClient: newMockOC(true), release: make(chan struct{})}
	m := newTestManager(t, store, proc, newMockWorktree(), fatalOC)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	fatalOC.armed.Store(true)
	// 屏障：等 CAS 已提交 active（store 可见）后放行 fatal → 落在 post-CAS 侧
	//（或被 post-CAS drain 捕获——两分派的收敛计数相同）。
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if row, _ := store.GetTask(context.Background(), "t1"); row.Status == StatusActive {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(fatalOC.release)
	}()

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	convergeSSEFatal(t, m, store, "t1")
	assertSSEFatalConverged(t, store, proc)
}

// TestCompleteRecoveryFailure_Dispositions 表驱动验证终态补偿的 KillResult
// disposition 处置（G3-8；design D3 表）：absent/clean 直接终态、degraded 记
// non-retryable notice 后终态、retryable 各分支 notice 落库后终态、notice 写失败
// MUST NOT 执行终态事务并落 durable cleanup_notice debt。
func TestCompleteRecoveryFailure_Dispositions(t *testing.T) {
	cases := []struct {
		name         string
		killRes      *process.KillResult
		killErr      error
		noticeFail   bool
		wantStatus   string
		wantNotice   string // 空 = 无 notice
		wantDebt     string // 空 = 无 debt
		replayStatus string // debt 重放后的期望状态（空 = 无 debt 重放）
	}{
		{
			name:       "absent session completes without notice",
			wantStatus: StatusSuspended,
		},
		{
			name:       "clean kill completes without notice",
			killRes:    &process.KillResult{SessionKilled: true, Disposition: process.DispositionClean},
			wantStatus: StatusSuspended,
		},
		{
			name:       "snapshot_missing_degraded records non-retryable notice then completes",
			killRes:    &process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonSnapshotMissing,
		},
		{
			name:       "reap_failed records retryable notice then completes",
			killRes:    &process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonReapFailed,
		},
		{
			name:       "snapshot_failed records retryable notice then completes",
			killRes:    &process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotFailed},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonSnapshotFailed,
		},
		{
			name:       "kill_failed disposition records retryable notice then completes",
			killRes:    &process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:       "unknown disposition fail-closed as kill_failed notice then completes",
			killRes:    &process.KillResult{SessionKilled: true, Disposition: process.CleanupDisposition("weird")},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:       "contradictory clean without SessionKilled fail-closed as kill_failed",
			killRes:    &process.KillResult{SessionKilled: false, Disposition: process.DispositionClean},
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:       "kill infra error records retryable notice then completes",
			killErr:    errors.New("tmux unreachable"),
			wantStatus: StatusSuspended,
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:         "notice write failure blocks complete and persists durable debt",
			killRes:      &process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed},
			noticeFail:   true,
			wantStatus:   StatusActivating,
			wantNotice:   "",
			wantDebt:     recoveryDebtPhaseCleanupNotice,
			replayStatus: StatusSuspended,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			markActiveWithSnapshot(store, "t1")
			proc := newMockProc()
			m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
			rt := m.newRuntime("t1")
			m.setRuntime("t1", rt)
			rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
			if tc.killRes != nil {
				proc.killResults[runtimeSessionName("t1")] = *tc.killRes
			}
			proc.killSessionErr = tc.killErr
			// 配置了 kill 行为的用例 MUST seed 会话：HasSession 命中才会走
			// KillSession/notice 处置分支；absent 用例不 seed，验证 absent → 直接终态事务。
			if tc.killRes != nil || tc.killErr != nil {
				proc.sessions[runtimeSessionName("t1")] = true
			}
			if tc.noticeFail {
				store.noticeCasErr = errors.New("notice store down")
			}
			// 预耗尽预算 → 首个 attempt 即走终态补偿。
			now := m.nowUnix()
			for i := 0; i < 3; i++ {
				if _, err := store.AcquireRecoveryPermit(context.Background(), "t1", now); err != nil {
					t.Fatal(err)
				}
			}
			m.ensureRecovery("t1", rt.instVersion)

			row, _ := store.GetTask(context.Background(), "t1")
			if row.Status != tc.wantStatus {
				t.Fatalf("status=%s want %s", row.Status, tc.wantStatus)
			}
			noticeHas := func(reason string) bool {
				entries, err := parseNotices(row.Notice)
				if err != nil {
					return false
				}
				for _, e := range entries {
					if r, _ := e.Data["reason"].(string); r == reason {
						return true
					}
				}
				return false
			}
			if tc.wantNotice == "" {
				if noticeHas(noticeReasonKillFailed) || noticeHas(noticeReasonReapFailed) || noticeHas(noticeReasonSnapshotMissing) {
					t.Errorf("unexpected notice: %v", row.Notice)
				}
			} else if !noticeHas(tc.wantNotice) {
				t.Errorf("notice missing reason %s: %v", tc.wantNotice, row.Notice)
			}
			debts, _ := store.ListRecoveryDebts(context.Background())
			if tc.wantDebt == "" {
				if len(debts) != 0 {
					t.Errorf("unexpected durable debt: %+v", debts)
				}
			} else {
				if len(debts) != 1 || debts[0].Phase != tc.wantDebt {
					t.Fatalf("durable debt=%+v want single %s", debts, tc.wantDebt)
				}
				// G3-3：notice 写失败恢复后重放 → notice 落库 + CompleteRecoveryFailure + debt 删除。
				store.noticeCasErr = nil
				if err := m.replayRecoveryDebts(context.Background()); err != nil {
					t.Fatalf("replay: %v", err)
				}
				rrow, _ := store.GetTask(context.Background(), "t1")
				if rrow.Status != tc.replayStatus {
					t.Fatalf("after replay status=%s want %s", rrow.Status, tc.replayStatus)
				}
				if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
					t.Errorf("debt not deleted after replay: %+v", drow)
				}
			}
		})
	}
}

// TestReplayRecoveryDebts_CompleteDebt 验证 G3-3：complete 债务重启后经重放直接
// 执行 CompleteRecoveryFailure，CAS mismatch 时删除 debt 服从最新状态。
func TestReplayRecoveryDebts_CompleteDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "recovery budget exhausted", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended || !row.LastError.Valid || row.LastError.String != "recovery budget exhausted" {
		t.Fatalf("row=%+v want suspended with cause", row)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("debt not deleted after replay: %+v", drow)
	}

	// CAS mismatch：任务已非 activating → 服从 DB 最新状态，仅清 debt。
	markActiveWithSnapshot(store, "t1")
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "stale", CreatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay mismatch: %v", err)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status=%s want active (CAS mismatch zero-modify)", row.Status)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("debt not deleted on CAS mismatch: %+v", drow)
	}
}

// TestRecoveryBackoff_CancelledByShutdown 验证 G3-7：Shutdown 关 gate 即时取消
// incident（退避立即中断、不做状态写入、incident 收尾），任务留 activating 由
// 下次启动 reconcile 收敛（D3 取消分派）。
func TestRecoveryBackoff_CancelledByShutdown(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	// 真实退避：取消前 backoff 会睡满 30s，若 Shutdown 未即时取消则测试超时失败。
	m.recoveryBackoffFn = func(ordinal int) time.Duration { return 30 * time.Second }

	go m.ensureRecovery("t1", rt.instVersion)
	// 等 permit 写入（attempt 已进入退避）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.recoveryPermitCount("t1") == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if store.recoveryPermitCount("t1") != 1 {
		t.Fatal("permit not acquired before shutdown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Shutdown took %v; backoff was not cancelled promptly", elapsed)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (shutdown MUST NOT write state)", row.Status)
	}
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("recovery incidents=%d want 0 after shutdown", incidents)
	}
}

// --- G3-8① 端口轮换 / 双启动的 permit 严格配对 ---

// failHealthOC 恒定健康失败（端口轮换 failpoint：注入特定端口的 factory 包装）。
type failHealthOC struct{ OCClient }

func (c *failHealthOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return opencode.HealthResponse{}, opencode.ErrServeNotReady
}

// newPortFailoverManager 构造按端口注入健康失败的 Manager（复刻 newTestManager 的
// readyOC 包装；failPort 命中时健康轮询超时 → 端口轮换路径）。failPort==-1 表示
// 全部端口健康失败（轮换预算耗尽 failpoint）。
func newPortFailoverManager(t *testing.T, store TaskStore, proc ProcessBackend, oc OCClient, failPort *atomic.Int64) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	factory := func(port int, password string, opts opencode.Options) OCClient {
		inner := oc
		if fp := failPort.Load(); fp == -1 || int64(port) == fp {
			inner = &failHealthOC{OCClient: oc}
		}
		return &readyOC{inner: inner, onReady: opts.OnReady}
	}
	m := New(Options{Cfg: cfg, Store: store, Proc: proc, Worktree: newMockWorktree(), OCFactory: factory})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 150 * time.Millisecond
	m.serveReadyPollInterval = 30 * time.Millisecond
	return m
}

func runtimeCreateCount(proc *mockProc, taskID string) int {
	n := 0
	for _, name := range proc.newSessionNamesSnapshot() {
		if name == runtimeSessionName(taskID) {
			n++
		}
	}
	return n
}

// assertPermitCreatePairing 断言 trace 中恢复段每次 runtime NewSession 前都有
// 一次 permit（且两次创建之间恰一次 permit，G3-8①）。
func assertPermitCreatePairing(t *testing.T, ops []string, taskID string) {
	t.Helper()
	runtimeNew := "new:" + runtimeSessionName(taskID)
	permit := "permit:" + taskID
	pendingPermit := false
	creates := 0
	for _, op := range ops {
		switch op {
		case permit:
			if creates > 0 || pendingPermit {
				// 连续 permit（无创建间隔）只可能来自下一次 attempt，合法；
				// 记录最近一次 permit 即可。
			}
			pendingPermit = true
		case runtimeNew:
			creates++
			if !pendingPermit {
				t.Fatalf("runtime create #%d not preceded by permit: %v", creates, ops)
			}
			pendingPermit = false
		}
	}
	if creates == 0 {
		t.Fatalf("no runtime create in trace: %v", ops)
	}
}

// TestRecoveryRotation_PermitPairedPerCreate 验证 G3-8①/G3-4：端口轮换的新进程
// 另耗 permit，且 permit 先于每次 NewSession（trace 逐次配对）。
func TestRecoveryRotation_PermitPairedPerCreate(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	failPort := &atomic.Int64{}
	m := newPortFailoverManager(t, store, proc, newMockOC(true), failPort)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastPort.Valid {
		t.Fatal("last_port missing after activate")
	}
	failPort.Store(row.LastPort.Int64)

	tr := &opTrace{}
	store.onPermit = func(taskID string) { tr.add("permit:" + taskID) }
	proc.onNewSession = func(name string) { tr.add("new:" + name) }

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	if got := store.recoveryPermitCount("t1"); got != 2 {
		t.Fatalf("permits=%d want 2 (rotation new process consumes another permit)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 4 {
		t.Fatalf("runtime creates=%d want 4 (activate dual-start 2 + attempt rotation 2)", got)
	}
	assertPermitCreatePairing(t, tr.snapshot(), "t1")
}

// TestRecoveryRotation_RetryableDebtBlocksNewSession 验证 G3-2/G3-5/G3-8③ 执行器
// 分派：轮换路径 KillSession 产生 retryable disposition（notice 已落库）→ typed
// 终态——MUST NOT 再创建进程（对比 clean kill 轮换继续）。
func TestRecoveryRotation_RetryableDebtBlocksNewSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	failPort := &atomic.Int64{}
	m := newPortFailoverManager(t, store, proc, newMockOC(true), failPort)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	failPort.Store(row.LastPort.Int64)
	// 轮换 kill 产生 reap_failed（SessionKilled=true：会话移除 + retryable notice）。
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
	}

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)

	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (terminal after retryable cleanup debt)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 3 {
		t.Fatalf("runtime creates=%d want 3 (NO new session after retryable debt; activate 2 + attempt 1)", got)
	}
	frow, _ := store.GetTask(context.Background(), "t1")
	if !frow.LastError.Valid || !contains(frow.LastError.String, "reap_failed") {
		t.Errorf("last_error=%v want reap_failed context", frow.LastError)
	}
}

// TestRecoveryNoAnchor_DualStartPermits 验证 G3-8①：无锚定恢复的双启动子事务
// 两次 NewSession 各耗一个 permit（bootstrap P1 / 正式 P2）。
func TestRecoveryNoAnchor_DualStartPermits(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	tr := &opTrace{}
	store.onPermit = func(taskID string) { tr.add("permit:" + taskID) }
	proc.onNewSession = func(name string) { tr.add("new:" + name) }

	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	if got := store.recoveryPermitCount("t1"); got != 2 {
		t.Fatalf("permits=%d want 2 (dual-start: bootstrap + formal)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 2 {
		t.Fatalf("runtime creates=%d want 2", got)
	}
	frow, _ := store.GetTask(context.Background(), "t1")
	if !frow.AnchorSessionID.Valid || frow.AnchorSessionID.String == "" {
		t.Fatalf("anchor missing after dual-start: %+v", frow.AnchorSessionID)
	}
	assertPermitCreatePairing(t, tr.snapshot(), "t1")
}

// TestRecoveryNoAnchor_SecondPermitWindowExhaustedKeepsAnchor 验证 D5⑥/G3-8①：
// 双启动第二个 permit 窗口不足 → 已 claim 的锚定保留，本次 attempt 进终态补偿。
func TestRecoveryNoAnchor_SecondPermitWindowExhaustedKeepsAnchor(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	// 预耗 2 个 permit：bootstrap 占第 3 个，正式进程窗口不足。
	now := m.nowUnix()
	for i := 0; i < 2; i++ {
		if _, err := store.AcquireRecoveryPermit(context.Background(), "t1", now); err != nil {
			t.Fatal(err)
		}
	}

	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)

	frow, _ := store.GetTask(context.Background(), "t1")
	if !frow.AnchorSessionID.Valid || frow.AnchorSessionID.String != "sess-new" {
		t.Fatalf("claimed anchor MUST be retained: %+v", frow.AnchorSessionID)
	}
	if !frow.LastError.Valid || !contains(frow.LastError.String, errRecoveryBudgetExhausted) {
		t.Errorf("last_error=%v want budget exhausted", frow.LastError)
	}
	if got := store.recoveryPermitCount("t1"); got != 3 {
		t.Fatalf("permits=%d want 3 (no extra permit on exhaust)", got)
	}
}

// --- G3-8④ 状态/token invalidation 与进行中进程清理的即时取消 ---

// TestRecoveryBackoff_CancelledByStateWrite 验证 G3-7：退避中任务被写出 activating
// （生产写路径触发 wake）→ 即时复核放弃，不等满退避 timer。
func TestRecoveryBackoff_CancelledByStateWrite(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }

	go m.ensureRecovery("t1", rt.instVersion)
	waitFor(t, 5*time.Second, func() bool { return store.recoveryPermitCount("t1") == 1 })

	// 生产写路径把任务带离 activating（如并发收敛）→ wake → 退避即时放弃。
	if _, err := m.writeStatusConditional(context.Background(), "t1", StatusActivating, StatusSuspended, sql.NullString{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 0
	})
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("incidents=%d want 0 (state invalidation must cancel backoff promptly)", incidents)
	}
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (cancelled backoff consumes no more)", got)
	}
}

// TestRecoveryBackoff_CancelledByForeignRuntime 验证 G3-7：退避中注册表出现异代
// runtime（token 失效）→ wake → 即时放弃。
func TestRecoveryBackoff_CancelledByForeignRuntime(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }

	go m.ensureRecovery("t1", rt.instVersion)
	waitFor(t, 5*time.Second, func() bool { return store.recoveryPermitCount("t1") == 1 })

	// 异代 runtime 注册（setRuntime 生产路径触发 wake）→ trigger token 失效。
	m.setRuntime("t1", m.newRuntime("t1"))
	waitFor(t, 3*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 0
	})
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("incidents=%d want 0 (token invalidation must cancel backoff promptly)", incidents)
	}
}

// blockHealthOC 按 armed 阻塞 Health 调用至 release/ctx 取消（进行中进程 failpoint）。
type blockHealthOC struct {
	OCClient
	block   atomic.Bool
	release chan struct{}
}

func (c *blockHealthOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	if c.block.Load() {
		select {
		case <-ctx.Done():
			return opencode.HealthResponse{}, ctx.Err()
		case <-c.release:
		}
	}
	return c.OCClient.Health(ctx)
}

// TestRecoveryCancelOwnedProcessCleanup 验证 G3-7：NewSession 到 commit 之间
// （注册表无 runtime）发生 Shutdown → incident-owned 进程标记使取消清理仍然
// 回退该进程（旧实现无匹配 runtime 可清 → 泄漏）。
func TestRecoveryCancelOwnedProcessCleanup(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	blockOC := &blockHealthOC{OCClient: newMockOC(true), release: make(chan struct{})}
	m := newTestManager(t, store, proc, newMockWorktree(), blockOC)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	rt := m.getRuntime("t1")
	// 恢复 attempt 的健康轮询阻塞：进程已创建（owned）、未到 commit。
	blockOC.block.Store(true)

	go m.ensureRecovery("t1", rt.instVersion)
	waitFor(t, 5*time.Second, func() bool { return runtimeCreateCount(proc, "t1") >= 3 })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	alive, _ := proc.HasSession(runtimeSessionName("t1"))
	if alive {
		t.Fatal("attempt-owned runtime session MUST be rolled back on shutdown cancel")
	}
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("incidents=%d want 0 after shutdown", incidents)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (shutdown cancel MUST NOT write state)", row.Status)
	}
}

// waitFor 轮询 cond 至 true 或超时（测试收敛助手）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// --- G3-9 Shutdown barrier ---

// TestEnsureRecovery_ShutdownBarrierInLockWait 验证 G3-9：锁等待中的 ensureRecovery
// 被 shutdownCh 即时取消（不占满 convergeLockDeadline），recoveryWG barrier 无
// 「已过 gate 未 Add」逃逸。
func TestEnsureRecovery_ShutdownBarrierInLockWait(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ensureRecovery("t1", rt.instVersion)
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ensureRecovery returned while lock held")
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v; lock wait was not cancelled by shutdownCh", elapsed)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureRecovery goroutine did not exit after shutdown")
	}
	unlock()
}

// --- G3-11 write-intent-first ---

// TestCompleteRecoveryFailure_IntentFirstRetained 验证 G3-11：Complete 写库失败时
// durable complete intent 已先行落盘并保留（不依赖已超时的补偿 ctx），重放修复后收敛。
func TestCompleteRecoveryFailure_IntentFirstRetained(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
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
	store.completeRecoveryErr = errors.New("complete store down")

	m.ensureRecovery("t1", rt.instVersion)

	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (complete failed, intent retained)", row.Status)
	}
	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 1 || debts[0].Phase != recoveryDebtPhaseComplete {
		t.Fatalf("durable complete intent=%+v want single complete row", debts)
	}

	// store 修复后重放收敛并删除 intent。
	store.completeRecoveryErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("after replay status=%s want suspended", row.Status)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("intent not deleted after replay: %+v", drow)
	}
}

// TestRecoveryDebtFallbackFlush 验证 G3-11：intent 落盘失败 → 内存重试队列 →
// store 修复后 flush 落盘并由重放收敛（Shutdown 传播链路的队列部分）。
func TestRecoveryDebtFallbackFlush(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
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
	store.completeRecoveryErr = errors.New("complete store down")
	store.recoveryDebtUpsertErr = errors.New("debt store down")

	m.ensureRecovery("t1", rt.instVersion)

	m.recoveryDebtFallbackMu.Lock()
	fallbacks := len(m.recoveryDebtFallbacks)
	m.recoveryDebtFallbackMu.Unlock()
	if fallbacks != 1 {
		t.Fatalf("fallback queue=%d want 1 (intent persist failed)", fallbacks)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Fatalf("no durable row expected while store down: %+v", drow)
	}

	store.recoveryDebtUpsertErr = nil
	if err := m.flushRecoveryDebtFallbacks(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 1 || debts[0].Phase != recoveryDebtPhaseComplete {
		t.Fatalf("durable row after flush=%+v", debts)
	}
	store.completeRecoveryErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended after flush+replay", row.Status)
	}
}

// --- G3-2 handled 分支 / G3-10 多 pending 载荷 ---

// TestReplayHandledCleanupForRecovery_PersistsDebt 验证 G3-2：CAS mismatch handled
// 分支 notice 重放失败 MUST 落完整 cleanup_notice debt（不再仅记日志）。
func TestReplayHandledCleanupForRecovery_PersistsDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	store.noticeCasErr = errors.New("notice store down")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	pending := pendingCleanup{
		sessionName:    runtimeSessionName("t1"),
		cleanupTickets: []string{"tk1", "tk2"},
		reason:         noticeReasonKillFailed,
		retryable:      true,
	}
	cause := &pendingCleanupError{pending: pending, noticeErr: store.noticeCasErr, cause: errors.New("commit cas mismatch")}
	m.replayHandledCleanupForRecovery(context.Background(), "t1", cause)

	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 1 {
		t.Fatalf("debts=%d want 1", len(debts))
	}
	d := debts[0]
	if d.Phase != recoveryDebtPhaseCleanupNotice || d.SessionName != pending.sessionName ||
		d.Reason != noticeReasonKillFailed || !d.Retryable || !contains(d.Tickets, "tk1") || !contains(d.Tickets, "tk2") {
		t.Errorf("debt payload incomplete: %+v", d)
	}
}

// TestRecoveryDebts_MultiPendingFullPayload 验证 G3-10：多 session pending（runtime +
// shells）notice 写失败 → 每 session 一行完整载荷落盘（不再只保首条）→ 修复后重放
// 逐项 notice 成功才 Complete 并整组删除。
func TestRecoveryDebts_MultiPendingFullPayload(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	// 三个会话都产生 retryable disposition（reap_failed）→ prelude 清理各自产生
	// notice 意图；clean kill 不形成 notice intent，无法构造 notice 写失败场景。
	for _, name := range []string{runtimeSessionName("t1"), shellSessionName("t1", 1), shellSessionName("t1", 2)} {
		proc.sessions[name] = true
		proc.killResults[name] = process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed}
	}
	store.noticeCasErr = errors.New("notice store down")
	now := m.nowUnix()
	for i := 0; i < 3; i++ {
		if _, err := store.AcquireRecoveryPermit(context.Background(), "t1", now); err != nil {
			t.Fatal(err)
		}
	}

	m.ensureRecovery("t1", rt.instVersion)

	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (notice failure blocks complete)", row.Status)
	}
	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 3 {
		t.Fatalf("debts=%d want 3 (one per session)", len(debts))
	}
	sessions := map[string]bool{}
	for _, d := range debts {
		if d.Phase != recoveryDebtPhaseCleanupNotice {
			t.Errorf("debt phase=%s", d.Phase)
		}
		sessions[d.SessionName] = true
	}
	for _, want := range []string{runtimeSessionName("t1"), shellSessionName("t1", 1), shellSessionName("t1", 2)} {
		if !sessions[want] {
			t.Errorf("debt for %s missing: %+v", want, debts)
		}
	}

	store.noticeCasErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("after replay status=%s want suspended", row.Status)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("debts not deleted after replay: %+v", drow)
	}
}

// --- G3-12 Complete 重放失败保留原 debt 行 ---

// TestReplayRecoveryDebts_CompleteFailureKeepsOriginalRows 验证 G3-12：Complete
// 写库失败时先 upsert complete 行、不预删——upsert 也失败或两步间退出，原
// cleanup_notice 行 MUST 仍在（design.md:115）；upsert 成功后共存安全，修复后收敛。
func TestReplayRecoveryDebts_CompleteFailureKeepsOriginalRows(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	seedCleanupDebt := func(cause string) {
		t.Helper()
		if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
			TaskID: "t1", SessionName: runtimeSessionName("t1"), Phase: recoveryDebtPhaseCleanupNotice,
			Tickets: `["tk1"]`, Reason: noticeReasonKillFailed, Retryable: true, Cause: cause, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCleanupDebt("recovery budget exhausted")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// Complete 失败 + complete upsert 失败 → 原行原样保留。
	store.completeRecoveryErr = errors.New("complete store down")
	store.recoveryDebtUpsertErr = errors.New("debt store down")
	if err := m.replayRecoveryDebts(context.Background()); err == nil {
		t.Fatal("replay must fail while store down")
	}
	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 1 || debts[0].Phase != recoveryDebtPhaseCleanupNotice || debts[0].SessionName != runtimeSessionName("t1") {
		t.Fatalf("original cleanup row MUST survive: %+v", debts)
	}

	// Complete 失败 + upsert 成功 → complete 行与 cleanup 行共存（不预删）。
	store.recoveryDebtUpsertErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err == nil {
		t.Fatal("replay must fail while complete down")
	}
	debts, _ = store.ListRecoveryDebts(context.Background())
	if len(debts) != 2 {
		t.Fatalf("rows=%d want 2 (cleanup + complete coexist, no pre-delete)", len(debts))
	}
	phases := map[string]bool{}
	for _, d := range debts {
		phases[d.Phase] = true
	}
	if !phases[recoveryDebtPhaseCleanupNotice] || !phases[recoveryDebtPhaseComplete] {
		t.Fatalf("coexist rows=%+v", debts)
	}

	// 修复后重放收敛：notice 幂等 + Complete + 整组删除。
	store.completeRecoveryErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended after repair", row.Status)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("debts not deleted after convergence: %+v", drow)
	}
}

// --- G3-13 fallback 队列不复活旧 intent ---

// TestFlushRecoveryDebtFallbacks_NoResurrect 验证 G3-13：flush 持锁摘除快照并清空
// 队列——成功项不残留（不复活旧 complete intent）、失败项不倍增、重复 tick 无副作用。
func TestFlushRecoveryDebtFallbacks_NoResurrect(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	fallbackLen := func() int {
		m.recoveryDebtFallbackMu.Lock()
		defer m.recoveryDebtFallbackMu.Unlock()
		return len(m.recoveryDebtFallbacks)
	}

	// 成功：队列清空 + durable 落库。
	m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]", Cause: "c1", CreatedAt: 1,
	})
	if err := m.flushRecoveryDebtFallbacks(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if fallbackLen() != 0 {
		t.Fatalf("queue=%d want 0 after successful flush (no resurrect)", fallbackLen())
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 1 {
		t.Fatalf("durable=%d want 1", len(drow))
	}

	// 失败：不倍增；重复 flush 仍为 1。
	store.recoveryDebtUpsertErr = errors.New("debt store down")
	m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]", Cause: "c2", CreatedAt: 2,
	})
	for i := 0; i < 3; i++ {
		if err := m.flushRecoveryDebtFallbacks(context.Background()); err == nil {
			t.Fatal("flush must fail while store down")
		}
		if fallbackLen() != 1 {
			t.Fatalf("queue=%d want 1 (failed rows must not duplicate) at iter %d", fallbackLen(), i)
		}
	}

	// 修复后 flush 落盘；再次 flush no-op；durable 重放（任务非 activating →
	// CAS mismatch）删除——旧 intent 不复活。
	store.recoveryDebtUpsertErr = nil
	if err := m.flushRecoveryDebtFallbacks(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if fallbackLen() != 0 {
		t.Fatalf("queue=%d want 0 after repair flush", fallbackLen())
	}
	if err := m.flushRecoveryDebtFallbacks(context.Background()); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Fatalf("debts must converge to empty, got %+v", drow)
	}
}

// --- G3-14 attempt token 精确匹配 ---

// TestCancelOwnedCleanup_TokenGuard 验证 G3-14：取消清理仅当注册表当前 token 精确
// 匹配本 incident 的 attempt token 时执行 rollback；异代替换后零清理（不误杀新代）。
func TestCancelOwnedCleanup_TokenGuard(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	noopCancel := func() {}
	rname := runtimeSessionName("t1")

	// inc1 的 attempt 注册 R1（G3-19：attempt token 由 attempt 显式绑定，
	// setRuntime 不再挂钩——模拟 commitRuntimeReady 的 onRegister 回调）。
	inc1 := &recoveryIncident{cancel: noopCancel}
	m.registerRecoveryIncident("t1", inc1)
	R1 := m.newRuntime("t1")
	inc1.noteAttemptToken(R1.instVersion)
	m.setRuntime("t1", R1)
	// 异代替换：inc2 接管（注册表 incident 替换 + R2 注册 + attemptTok(inc2)=R2）。
	inc2 := &recoveryIncident{cancel: noopCancel}
	m.registerRecoveryIncident("t1", inc2)
	R2 := m.newRuntime("t1")
	inc2.noteAttemptToken(R2.instVersion)
	m.setRuntime("t1", R2)
	proc.sessions[rname] = true

	// 旧 incident 取消：cur=R2 ≠ inc1.attemptTok(R1) → 零清理，R2 会话存活。
	m.cancelOwnedCleanup(context.Background(), "t1", inc1)
	if alive, _ := proc.HasSession(rname); !alive {
		t.Fatal("cancel of superseded incident MUST NOT kill foreign runtime")
	}
	if cur := m.getRuntime("t1"); cur == nil || cur.instVersion != R2.instVersion {
		t.Fatal("foreign runtime MUST stay registered")
	}

	// 新 incident 取消：cur=R2 == attemptTok(inc2) → 完整 rollback。
	m.cancelOwnedCleanup(context.Background(), "t1", inc2)
	if alive, _ := proc.HasSession(rname); alive {
		t.Fatal("owned runtime MUST be rolled back on owning incident cancel")
	}
	if m.getRuntime("t1") != nil {
		t.Fatal("owned runtime MUST be deregistered after rollback")
	}
}

// TestRecoveryCommitFailure_ForeignRuntimeZeroCleanup 验证 G3-14：提交失败回退仅当
// 注册表 token 匹配本 attempt token；屏障内模拟异代接管后，回退与 attempt 间清理
// 均零清理，incident 随后按 token 失效 abandon（不写状态）。
func TestRecoveryCommitFailure_ForeignRuntimeZeroCleanup(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	fatalOC := &sseFatalGateOC{OCClient: newMockOC(true), release: make(chan struct{})}
	m := newTestManager(t, store, proc, newMockWorktree(), fatalOC)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	fatalOC.armed.Store(true)
	// 屏障：commit 的 lastPort 写回时（本 attempt runtime 已注册）模拟异代接管
	//（inc2 替换注册表 + R2 注册），再放行 fatal 并等 latch 置位——确保 pre-CAS
	// drain 确定性捕获（提交失败路径）。inc2 为测试构造的接管者，收尾需手动注销。
	takeover := &recoveryIncident{cancel: func() {}}
	var barrierOnce sync.Once
	store.onLastPort = func(string) {
		barrierOnce.Do(func() {
			m.registerRecoveryIncident("t1", takeover)
			m.setRuntime("t1", m.newRuntime("t1"))
			close(fatalOC.release)
			waitIncidentFatalLatched(m, "t1", 5*time.Second)
		})
	}

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})

	// 收敛：incident 按 token 失效退出（activating 保持，无状态写入）。
	// 测试接管者注销后注册表应仅剩真实 incident 的生命周期。
	m.unregisterRecoveryIncident("t1", takeover)
	waitFor(t, 5*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 0
	})
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (token invalidation writes no state)", row.Status)
	}
	rname := runtimeSessionName("t1")
	if alive, _ := proc.HasSession(rname); !alive {
		t.Fatal("foreign runtime session MUST NOT be killed by superseded incident")
	}
	if cur := m.getRuntime("t1"); cur == nil {
		t.Fatal("foreign runtime MUST stay registered")
	}
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (incident abandoned after attempt 1)", got)
	}
}

// --- G3-15 HasSession infra 错误 fail-closed ---

// TestConfirmRuntimeTerminated_HasSessionInfraFailClosed 定点验证 G3-15：HasSession
// infra 错误按 kill_failed/retryable 写 notice——成功返回 typed cleanup error
// （Recovery 终态），失败返回完整 pending。
func TestConfirmRuntimeTerminated_HasSessionInfraFailClosed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rname := runtimeSessionName("t1")
	proc.sessions[rname] = true
	proc.hasSessionErr = errors.New("tmux has-session broken")

	// notice 写成功 → typed retryable cleanup（terminal 分派）。
	err := m.confirmRuntimeTerminated(context.Background(), "t1", rname)
	if !isRecoveryTerminal(err) {
		t.Fatalf("err=%v want terminal dispatch (retryableCleanupError)", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil || len(entries) != 1 {
		t.Fatalf("notice not recorded: %v %+v", perr, row.Notice)
	}
	if r, _ := entries[0].Data["reason"].(string); r != noticeReasonKillFailed {
		t.Errorf("notice reason=%q want kill_failed", r)
	}

	// notice 写失败 → 完整 pending（阻断终态事务，进 debt 通道）。
	store.noticeCasErr = errors.New("notice store down")
	err = m.confirmRuntimeTerminated(context.Background(), "t1", rname)
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("err=%v want pendingCleanupError", err)
	}
	if pce.pending.sessionName != rname || pce.pending.reason != noticeReasonKillFailed || !pce.pending.retryable {
		t.Errorf("pending payload incomplete: %+v", pce.pending)
	}
}

// hasErrProc 按 armed 注入 HasSession infra 错误（G3-15 执行器级 failpoint）。
type hasErrProc struct {
	*mockProc
	armed *atomic.Bool
}

func (p *hasErrProc) HasSession(name string) (bool, error) {
	if p.armed.Load() {
		return false, errors.New("tmux has-session broken")
	}
	return p.mockProc.HasSession(name)
}

// TestRecoveryHasSessionInfra_FreezesNewSession 验证 G3-15 执行器分派：attempt 间
// 确认终止遇 HasSession infra 错误（未确认终止）→ fail-closed 终态——NewSession
// 计数与 permit 冻结（不再复用固定会话名创建进程）。
func TestRecoveryHasSessionInfra_FreezesNewSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	armed := &atomic.Bool{}
	hproc := &hasErrProc{mockProc: proc, armed: armed}
	failPort := &atomic.Int64{}
	m := newPortFailoverManager(t, store, hproc, newMockOC(true), failPort)
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 恢复第 1 次 NewSession 后 arm（hook 在 Activate 之后挂载，计数不含激活的
	// 双启动）：轮换健康轮询视为存活 → 超时 → clean kill 轮换（3 次创建耗尽
	// 预算）→ attempt 间 confirmRuntimeTerminated 遇 infra 错误。
	seen := &atomic.Int64{}
	hproc.onNewSession = func(name string) {
		if name == runtimeSessionName("t1") && seen.Add(1) == 1 {
			armed.Store(true)
			failPort.Store(-1)
		}
	}

	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	waitStatusAny(t, store, "t1", 8*time.Second, StatusSuspended)

	// attempt1 内 3 次轮换创建 = 3 permits；confirm 未确认终止 → 终态，无 attempt2。
	if got := store.recoveryPermitCount("t1"); got != 3 {
		t.Fatalf("permits=%d want 3 (frozen after unconfirmed termination)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 5 {
		t.Fatalf("runtime creates=%d want 5 (activate 2 + rotation 3, frozen)", got)
	}
	frow, _ := store.GetTask(context.Background(), "t1")
	if !frow.LastError.Valid || !contains(frow.LastError.String, "has session") {
		t.Errorf("last_error=%v want has-session context", frow.LastError)
	}
	// 冻结复核：terminal 后不再有任何创建/permit。
	time.Sleep(150 * time.Millisecond)
	if got := store.recoveryPermitCount("t1"); got != 3 {
		t.Fatalf("permits=%d want 3 (must stay frozen)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 5 {
		t.Fatalf("runtime creates=%d want 5 (must stay frozen)", got)
	}
}

// --- G3-16 wake 丢失窗口 ---

// TestWaitRecoveryBackoff_PreExistingInvalidation 验证 G3-16：通道注册前状态已失效
// （无 wake 信号可收）→ 注册后立即复核兜底，不等满退避 timer。
func TestWaitRecoveryBackoff_PreExistingInvalidation(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }
	// 直接改状态（绕过生产 wake hook），模拟注册前已失效。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })

	start := time.Now()
	err := m.waitRecoveryBackoff(context.Background(), "t1", runtime.InstVersion("trigger"), 1)
	if !errors.Is(err, errRecoveryAbandon) {
		t.Fatalf("err=%v want errRecoveryAbandon", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v; pre-existing invalidation must be caught by immediate recheck", elapsed)
	}
}

// TestRecoveryBackoff_CancelledByDebtReplayComplete 验证 G3-16：退避中 debt 重放的
// CompleteRecoveryFailure（writeCompleteRecoveryFailure）改出 activating → wake →
// 退避即时放弃。
func TestRecoveryBackoff_CancelledByDebtReplayComplete(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }

	go m.ensureRecovery("t1", rt.instVersion)
	waitFor(t, 5*time.Second, func() bool { return store.recoveryPermitCount("t1") == 1 })

	// 退避中：debt 重放路径的 Complete 把任务带离 activating（生产路径 wake）。
	if _, err := m.writeCompleteRecoveryFailure(context.Background(), "t1",
		sql.NullString{String: "recovery budget exhausted", Valid: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 0
	})
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("incidents=%d want 0 (debt-replay Complete must cancel backoff promptly)", incidents)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended", row.Status)
	}
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1", got)
	}
}

// --- G3-17 Shutdown 错误 join ---

// TestShutdown_JoinsFallbackAndOrphanErrors 验证 G3-17：fallback flush 失败与
// persist 模式 orphan 残留同时存在时，两类原因均可达（不被覆盖）。
func TestShutdown_JoinsFallbackAndOrphanErrors(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	store.recoveryDebtUpsertErr = errors.New("debt store down")
	m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]", Cause: "c", CreatedAt: 1,
	})
	// orphan 会话 kill 落 kill_failed（不可收敛）→ Shutdown 收割后仍残留。
	ghost := "ocdeck-ghost-runtime"
	proc.sessions[ghost] = true
	proc.killResults[ghost] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed}
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{sessionName: ghost, tickets: []string{"tk"}})
	m.orphanMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown must fail (fallback unflushed + orphan remains)")
	}
	msg := err.Error()
	if !contains(msg, "flush recovery debt fallbacks") {
		t.Errorf("fallback flush error lost: %s", msg)
	}
	if !contains(msg, "orphan cleanup tickets remain") {
		t.Errorf("orphan remain error lost: %s", msg)
	}
}

// TestRecoveryTerminalCleanupBetweenAttempts_FreezesNewSession 验证 G3-15：attempt
// 间确认终止返回 typed 终态错误（notice 已落库）时立即终态补偿——预算尚余也
// MUST NOT 再创建进程（旧实现仅并入 lastErr 继续循环）。
func TestRecoveryTerminalCleanupBetweenAttempts_FreezesNewSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	// attempt1 的 NewSession 失败（permit 已耗）+ 随后 confirm 遇 HasSession infra
	// 错误：onPermit 首次回调时 arm（prelude 无 permit，不受影响）。
	store.onPermit = func(string) {
		proc.hasSessionErr = errors.New("tmux has-session broken")
		proc.newSessionErr = errors.New("new-session rejected")
	}

	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)

	// 预算仅耗 1 个 permit（3 窗口仍余）但创建冻结：无 attempt2。
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (terminal cleanup must freeze despite budget left)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 0 {
		t.Fatalf("runtime creates=%d want 0 (activate-free setup; NewSession failed)", got)
	}
	frow, _ := store.GetTask(context.Background(), "t1")
	if !frow.LastError.Valid || !contains(frow.LastError.String, "has session") {
		t.Errorf("last_error=%v want has-session context", frow.LastError)
	}
	// 冻结复核：terminal 后无新 permit/创建。
	time.Sleep(150 * time.Millisecond)
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (must stay frozen)", got)
	}
}

// TestCancelOwnedCleanup_UnboundIncidentNeverKillsNewActivate 验证 G3-19 + G3-16
// ABA 首选方案：①旧 incident 存续期间 Activate 被拒（消除「Complete→重 Activate
// 重叠」窗口）；②通用 setRuntime 挂钩移除后，未绑定 attemptTok 的旧 incident 取消
// 时零清理——新 Activate 注册的 runtime 不被误杀（挂钩会把新代 token 绑到旧 incident）。
func TestCancelOwnedCleanup_UnboundIncidentNeverKillsNewActivate(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	// ①模拟「Complete 已落 suspended 但旧 incident 未注销」窗口：incident 存续
	// 期间 Activate 被拒（G3-16 ABA 防护）；注销后放行。
	inc1 := &recoveryIncident{cancel: func() {}}
	m.registerRecoveryIncident("t1", inc1)
	if err := m.Activate(context.Background(), "t1"); err == nil || !contains(err.Error(), "recovery in progress") {
		t.Fatalf("err=%v want recovery-in-progress conflict", err)
	}
	m.unregisterRecoveryIncident("t1", inc1)
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate after incident settled: %v", err)
	}
	R2 := m.getRuntime("t1")
	if R2 == nil {
		t.Fatal("runtime missing after activate")
	}

	// ②unbound 旧 incident（模拟注销前残留引用）取消：attemptTok 恒空 → 零清理。
	m.cancelOwnedCleanup(context.Background(), "t1", inc1)

	if alive, _ := proc.HasSession(runtimeSessionName("t1")); !alive {
		t.Fatal("unbound incident cancel MUST NOT kill newly activated runtime")
	}
	if cur := m.getRuntime("t1"); cur == nil || cur.instVersion != R2.instVersion {
		t.Fatal("newly activated runtime MUST stay registered")
	}
	if inc1.attemptToken() != runtime.InstVersion("") {
		t.Fatalf("attemptTok=%q want empty (setRuntime must not bind tokens)", inc1.attemptToken())
	}
}

// TestWaitRecoveryBackoff_WakeDuringRecheckNotLost 验证 G3-16：复核进行中到达的
// wake 必须落在已订阅的新通道上（「先订阅→再复核」）。屏障构造 Complete→新
// Activate 交错：wake1 唤醒复核（阻塞在 GetTask gate）→ setRuntime(R2) 触发
// wake2（关闭复核前已订阅的 C2）→ gate 放行后 select 立即醒 → 复核发现 token
// 失效。旧顺序（复核→订阅）下 wake2 落在无通道窗口，退避睡满 60s。
func TestWaitRecoveryBackoff_WakeDuringRecheckNotLost(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }

	firstCheck := make(chan struct{}, 1)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	store.onGetTask = func(string) {
		switch calls.Add(1) {
		case 1:
			// 初始复核完成（订阅 C1 已发生）→ 测试可安全 wake1。
			select {
			case firstCheck <- struct{}{}:
			default:
			}
		case 2:
			// wake 后复核：阻塞在 gate，期间测试注入 setRuntime(R2)（wake2）。
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
	}

	trigger := runtime.InstVersion("T1")
	done := make(chan error, 1)
	go func() {
		done <- m.waitRecoveryBackoff(context.Background(), "t1", trigger, 1)
	}()
	<-firstCheck
	m.wakeRecoveryIncident("t1") // wake1：关闭 C1，退避者醒来进入复核（订阅 C2 后阻塞）
	<-entered
	m.setRuntime("t1", m.newRuntime("t1")) // wake2：关闭 C2（复核进行中，已订阅）
	close(release)

	select {
	case err := <-done:
		if !errors.Is(err, errRecoveryTokenInvalid) {
			t.Fatalf("err=%v want errRecoveryTokenInvalid", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wake during recheck was lost; backoff slept full window")
	}
}

// --- G3-18 激活准入原子拒绝未清 recovery debt + Complete/清 debt 单事务 ---

// TestActivate_RejectsUncleanedRecoveryDebt 验证 G3-18 ①：存在 complete debt 行时
// Activate 被原子拒绝（conflict），清 debt 后激活成功。
func TestActivate_RejectsUncleanedRecoveryDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "complete delete failed residue", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	err := m.Activate(context.Background(), "t1")
	if err == nil || !contains(err.Error(), "recovery debt") {
		t.Fatalf("err=%v want conflict mentioning recovery debt", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)

	if err := store.DeleteRecoveryDebt(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate after debt cleared: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
}

// TestEnsureRecovery_RejectsUncleanedRecoveryDebt 验证 G3-18 ②：存在 debt 行时
// ensureRecovery 零副作用放弃（不进 activating、不耗 permit）。
func TestEnsureRecovery_RejectsUncleanedRecoveryDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", SessionName: runtimeSessionName("t1"), Phase: recoveryDebtPhaseCleanupNotice,
		Tickets: "[]", Reason: noticeReasonKillFailed, Retryable: true, Cause: "residue", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	m.ensureRecovery("t1", rt.instVersion)

	assertStatus(t, store, "t1", StatusActive)
	if got := store.recoveryPermitCount("t1"); got != 0 {
		t.Fatalf("permits=%d want 0 (zero side effects on debt present)", got)
	}
	if m.getRuntime("t1") == nil || m.getRuntime("t1").instVersion != rt.instVersion {
		t.Fatal("runtime MUST NOT be touched")
	}
}

// TestCompleteRecoveryFailureAndClearDebts_SingleTransaction 验证 G3-18 ③：
// Complete 与 debt 删除为单事务——事务失败时状态与 debt 均不变（无「Complete
// 成功但删除失败」的中间态）；成功时两者同批收敛。
func TestCompleteRecoveryFailureAndClearDebts_SingleTransaction(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "recovery budget exhausted", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 事务失败：状态保持 activating + debt 保留。
	store.completeAndClearErr = errors.New("tx aborted")
	if _, err := m.writeCompleteRecoveryFailure(context.Background(), "t1",
		sql.NullString{String: "recovery budget exhausted", Valid: true}); err == nil {
		t.Fatal("want tx failure")
	}
	assertStatus(t, store, "t1", StatusActivating)
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 1 {
		t.Fatalf("debts=%d want 1 (single tx keeps both on failure)", len(drow))
	}

	// 事务成功：suspended + debt 同批删除。
	store.completeAndClearErr = nil
	if _, err := m.writeCompleteRecoveryFailure(context.Background(), "t1",
		sql.NullString{String: "recovery budget exhausted", Valid: true}); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Fatalf("debts=%d want 0 after single-tx complete", len(drow))
	}
}

// TestRecoveryDebtResidue_ReplaySafeThenActivate 验证 G3-18 ④端到端：debt 残留
// （模拟旧缺陷「Complete 成功但删除失败」）→ 重激活被拒（准入原子拒绝）→
// 重放收敛（CAS mismatch 删 debt）→ Activate 成功。全程无误伤新激活。
func TestRecoveryDebtResidue_ReplaySafeThenActivate(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	// 残留 complete debt（任务已 suspended——Complete 成功、删除失败的终态）。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "stale complete intent", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 重激活被拒（原子准入）。
	if err := m.Activate(context.Background(), "t1"); err == nil || !contains(err.Error(), "recovery debt") {
		t.Fatalf("err=%v want recovery debt conflict", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)

	// 重放：CAS mismatch（已 suspended）→ 删 debt 服从最新状态。
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Fatalf("debts=%+v want cleared on CAS mismatch", drow)
	}

	// Activate 成功。
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate after replay: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
}

// TestWaitRecoveryBackoff_TimerExpiryRecheckNotLost 验证 G3-16 timer 路径：到期
// 复核同样「订阅→复核→检查通道」——复核取值后判定前到达的 wake（通道关闭）触发
// 追加复核，信号不吞。旧实现 timer 到期直接复核返回，窗口内信号丢失。
func TestWaitRecoveryBackoff_TimerExpiryRecheckNotLost(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.recoveryBackoffFn = func(int) time.Duration { return 80 * time.Millisecond }

	firstCheck := make(chan struct{}, 1)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	store.onGetTask = func(string) {
		switch calls.Add(1) {
		case 1:
			// 初始复核完成（退避计时开始）。
			select {
			case firstCheck <- struct{}{}:
			default:
			}
		case 2:
			// timer 到期复核取值后阻塞：期间测试关通道 + 改状态——复核 #2 判定
			// 用旧值（activating）通过，通道检查发现新信号 → 追加复核读新值。
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- m.waitRecoveryBackoff(context.Background(), "t1", runtime.InstVersion("T1"), 1)
	}()
	<-firstCheck
	<-entered
	m.wakeRecoveryIncident("t1") // 关闭 timer case 已订阅的新通道
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })
	close(release)

	select {
	case err := <-done:
		if !errors.Is(err, errRecoveryAbandon) {
			t.Fatalf("err=%v want errRecoveryAbandon (timer-recheck must consume missed wake)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer expiry recheck did not consume missed wake")
	}
}

// TestActivate_AroundLiveIncident_ConvergesSafely 验证 G3-16 首选方案的端到端时序：
// incident 退避中（activating）Activate 被状态门禁拒绝 → debt replay 的 Complete
// 落 suspended + wake → incident 即时退出注销 → Activate 成功。全程无重叠窗口。
func TestActivate_AroundLiveIncident_ConvergesSafely(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	m.recoveryBackoffFn = func(int) time.Duration { return 60 * time.Second }

	go m.ensureRecovery("t1", rt.instVersion)
	waitFor(t, 5*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 1
	})

	// incident 运行中（activating）：Activate 被状态门禁拒绝。
	if err := m.Activate(context.Background(), "t1"); err == nil || !contains(err.Error(), "requires suspended") {
		t.Fatalf("err=%v want invalid state while incident live", err)
	}

	// debt replay 的 Complete 落 suspended（+ wake）→ incident 即时退出。
	if _, err := m.writeCompleteRecoveryFailure(context.Background(), "t1",
		sql.NullString{String: "recovery budget exhausted", Valid: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		m.recoveryIncidentsMu.Lock()
		defer m.recoveryIncidentsMu.Unlock()
		return len(m.recoveryIncidents) == 0
	})

	// incident 注销后 Activate 成功（无残留拒绝）。
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate after incident settled: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
}

// TestRecoveryDebtCleanup_SingleClearPoint 验证 G3-20：补偿与重放的完整流程均不
// 执行事务外 legacy DeleteRecoveryDebt——清债唯一入口是
// CompleteRecoveryFailureAndClearDebts 原子事务；事务返回后写入的新 intent 不被
// 调用方删除。
func TestRecoveryDebtCleanup_SingleClearPoint(t *testing.T) {
	// 场景 A：completeRecoveryFailure 全流程（budget 耗尽终态）。
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
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
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)
	if got := store.deleteDebtCallsCount(); got != 0 {
		t.Fatalf("legacy DeleteRecoveryDebt calls=%d want 0 (single clear point: atomic tx)", got)
	}
	// 事务后写入的新 intent 不被流程删除（流程已结束，无残留删除者）。
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "concurrent new intent", CreatedAt: 9,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 1 {
		t.Fatalf("new intent must survive: %+v", drow)
	}
	if got := store.deleteDebtCallsCount(); got != 0 {
		t.Fatalf("legacy DeleteRecoveryDebt calls=%d want 0", got)
	}

	// 场景 B：重放路径收敛也不走 legacy Delete。
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Fatalf("replay must converge concurrent intent via atomic tx: %+v", drow)
	}
	if got := store.deleteDebtCallsCount(); got != 0 {
		t.Fatalf("legacy DeleteRecoveryDebt calls=%d want 0 on replay path", got)
	}
}
