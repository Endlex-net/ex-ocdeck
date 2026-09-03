package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/process"
)

// --- Phase 4 / tasks 5.1：ReopenAttach 接 ensureRecovery（D8 表） ---

// TestReopenAttach_LiveRuntimeReturnsTerminal 验证 D8 分支一：runtime 存活且
// active → 返回 -runtime 会话名 terminal ID。
func TestReopenAttach_LiveRuntimeReturnsTerminal(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach: %v", err)
	}
	if string(tid) != runtimeSessionName("t1") {
		t.Fatalf("tid=%s want %s", tid, runtimeSessionName("t1"))
	}
}

// TestReopenAttach_MissingRuntimeTriggersRecovery 验证 D8 分支二：active 但 runtime
// 缺失 → 触发幂等 ensureRecovery（异步）+ 返回 typed recovering；恢复完成后任务
// 回到 active。
func TestReopenAttach_MissingRuntimeTriggersRecovery(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 模拟进程异常消失（tmux 会话不在），注册表 runtime 仍在 → ReopenAttach 兜底触发恢复。
	proc.mu.Lock()
	delete(proc.sessions, runtimeSessionName("t1"))
	proc.mu.Unlock()

	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeRecovering {
		t.Fatalf("err=%v want typed recovering", err)
	}
	if tid != "" {
		t.Fatalf("tid=%q want empty on recovering", tid)
	}
	// 恢复被触发（异步）并收敛回 active（watcher 未达场景下由终端入口兜底）。
	// 先等恢复确实启动（permit 写入），再等收敛。
	waitFor(t, 5*time.Second, func() bool { return store.recoveryPermitCount("t1") >= 1 })
	waitStatusAny(t, store, "t1", 5*time.Second, StatusActive)
	if got := store.recoveryPermitCount("t1"); got != 1 {
		t.Fatalf("permits=%d want 1 (recovery triggered exactly once)", got)
	}
}

// TestReopenAttach_ActivatingReturnsRecoveringNoRestart 验证 D8 分支三：activating
// → 同一 typed recovering，不重复启动恢复（无新 permit/进程创建）。
func TestReopenAttach_ActivatingReturnsRecoveringNoRestart(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeRecovering {
		t.Fatalf("err=%v want typed recovering", err)
	}
	if got := store.recoveryPermitCount("t1"); got != 0 {
		t.Fatalf("permits=%d want 0 (activating MUST NOT start a second recovery)", got)
	}
	if got := len(proc.newSessionNamesSnapshot()); got != 0 {
		t.Fatalf("new sessions=%d want 0", got)
	}
}

// TestReopenAttach_NonActiveInvalidState 验证 D8 分支四：suspended 等非活跃状态 →
// invalid_state 错误，不触发恢复。
func TestReopenAttach_NonActiveInvalidState(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeInvalidState {
		t.Fatalf("err=%v want invalid_state", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
}

// --- Phase 4 / tasks 6.1：reconcile 单进程迁移（D7） ---

// seedPersistRuntime 构造 persist resume 前置（active 任务 + env 快照 + 健康的
// runtime 会话）。
func seedPersistRuntime(t *testing.T, store *mockStore, proc *mockProc) {
	t.Helper()
	seedSuspendedTask(store, "t1", "p1")
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
}

// TestReconcilePersist_ActiveRuntimeAliveResumes 验证 D7：persist 模式下仅 active
// 任务的存活健康 runtime 会话可原地 resume（判定已从 -serve 迁移到 -runtime）。
func TestReconcilePersist_ActiveRuntimeAliveResumes(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	if m.getRuntime("t1") == nil {
		t.Fatal("runtime must be registered after persist resume")
	}
}

// TestReconcilePersist_ActiveRuntimeUnhealthyCleansToSuspended 验证 D7：active 但
// runtime 会话不健康（健康检查失败）→ 完整清理 + 落挂起，不原地 resume。
func TestReconcilePersist_ActiveRuntimeUnhealthyCleansToSuspended(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 健康检查失败：oc 恒不健康。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(false))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); alive {
		t.Fatal("unhealthy runtime session must be cleaned")
	}
}

// TestReconcilePersist_ActivatingAlwaysCleansToSuspended 验证 D7：activating 一律
// 视为被中断的激活/恢复——清理并落挂起（不续跑，不原地 resume），健康 runtime
// 也不复活。
func TestReconcilePersist_ActivatingAlwaysCleansToSuspended(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); alive {
		t.Fatal("interrupted activation runtime must be cleaned (no resume)")
	}
	if m.getRuntime("t1") != nil {
		t.Fatal("no runtime must be registered for interrupted activation")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || row.LastError.String != "interrupted activation during server restart" {
		t.Errorf("last_error=%v want interrupted activation cause", row.LastError)
	}
}

// TestReconcilePersist_LegacyServeTuiCleaned 验证 D7：旧版 -serve/-tui 会话一律按
// 异常会话清理（不支持热迁移）——active 任务即便残留 legacy 会话也不恢复，全部清理。
func TestReconcilePersist_LegacyServeTuiCleaned(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 残留旧双进程布局会话（无 -runtime 会话：hasRuntime=false → 不 resume）。
	delete(proc.sessions, runtimeSessionName("t1"))
	delete(proc.envValues, runtimeSessionName("t1"))
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	for _, name := range []string{serveSessionName("t1"), tuiSessionName("t1")} {
		if alive, _ := proc.HasSession(name); alive {
			t.Errorf("legacy session %s must be cleaned", name)
		}
	}
}

// TestReconcilePersist_OrphanLegacySessionsCleaned 验证 D7：taskID 无 DB 行的旧版
// 会话按孤儿清理。
func TestReconcilePersist_OrphanLegacySessionsCleaned(t *testing.T) {
	store := newMockStore() // 空 DB → 全部孤儿
	proc := newMockProc()
	proc.sessions["ocdeck-ghost-t1-serve"] = true
	proc.sessions["ocdeck-ghost-t1-tui"] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, name := range []string{"ocdeck-ghost-t1-serve", "ocdeck-ghost-t1-tui"} {
		if alive, _ := proc.HasSession(name); alive {
			t.Errorf("orphan legacy session %s must be cleaned", name)
		}
	}
}

// TestReconcilePersist_ActivatingShellAlsoCleaned 验证 D7：activating 清理覆盖
// shell 会话（残余进程不遗留）。
func TestReconcilePersist_ActivatingShellAlsoCleaned(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc.sessions[shellSessionName("t1", 1)] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if alive, _ := proc.HasSession(shellSessionName("t1", 1)); alive {
		t.Fatal("shell session of interrupted activation must be cleaned")
	}
	assertStatus(t, store, "t1", StatusSuspended)
}

// 编译期引用守卫（process 包仅在注释语境使用时避免未用导入）。
var _ = process.ErrNoTmuxServer

// --- Gate 4（G4-1/2/3/5）---

// TestReopenAttach_NoRegistryTriggersRecovery 验证 G4-1：active + runtime 会话缺失 +
// 注册表无 runtime（watcher/SSE 均无法触发的窗口）→ attach 入口以新分配 trigger
// 创建 incident，任务不永久卡 active。
func TestReopenAttach_NoRegistryTriggersRecovery(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 直接构造：active、无 runtime 注册、无任何会话（rt==nil 的 G4-1 窗口）。
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeRecovering {
		t.Fatalf("err=%v want typed recovering", err)
	}
	waitFor(t, 5*time.Second, func() bool { return store.recoveryPermitCount("t1") >= 1 })
	waitStatusAny(t, store, "t1", 5*time.Second, StatusActive)
	// 无锚定任务恢复走 D5 双启动（bootstrap + 正式进程各一 permit）= 恰好 2；
	// 双 incident 才会 ≥3。
	if got := store.recoveryPermitCount("t1"); got != 2 {
		t.Fatalf("permits=%d want 2 (single attach-created incident, dual-start)", got)
	}
}

// TestRecovery_WatcherAndAttachConcurrent_SingleIncident 验证 G4-5②：watcher 来源
// （ensureRecovery，token 匹配）与 attach 来源（ensureRecoveryFromAttach）并发触发
// 同一任务 → 恰好一个 incident / 一个 permit（CAS 幂等收敛）。
func TestRecovery_WatcherAndAttachConcurrent_SingleIncident(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	// 删除 runtime 会话（进程消失），注册表 runtime 保留（watcher token 可用）。
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	proc.mu.Lock()
	delete(proc.sessions, runtimeSessionName("t1"))
	proc.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.ensureRecovery("t1", rt.instVersion) }()
	go func() { defer wg.Done(); m.ensureRecoveryFromAttach("t1") }()
	wg.Wait()

	waitStatusAny(t, store, "t1", 5*time.Second, StatusActive)
	// 无锚定恢复为单 incident 双启动 = 恰好 2 permit；两个 incident 并发会 ≥3。
	if got := store.recoveryPermitCount("t1"); got != 2 {
		t.Fatalf("permits=%d want 2 (concurrent sources converge to single dual-start incident)", got)
	}
}

// TestReconcilePersist_LegacyCoexistCleanedBeforeResume 验证 G4-2/G4-5③：健康
// -runtime 与 legacy -serve/-tui 共存 → resume 决策前独立清理 legacy；clean 清理
// 不阻断 resume（legacy 清空、runtime 保留、任务 active）。
func TestReconcilePersist_LegacyCoexistCleanedBeforeResume(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 共存：runtime 健康 + legacy serve/tui 同在。
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	for _, name := range []string{serveSessionName("t1"), tuiSessionName("t1")} {
		if alive, _ := proc.HasSession(name); alive {
			t.Errorf("legacy session %s must be cleaned before resume", name)
		}
	}
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); !alive {
		t.Fatal("healthy runtime must survive legacy cleanup and resume")
	}
}

// TestReconcilePersist_LegacyCleanupDebtBlocksResume 验证 G4-2：legacy 清理产生
// retryable debt → 不得恢复 active（连同 runtime 一并清理落挂起）。
func TestReconcilePersist_LegacyCleanupDebtBlocksResume(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc.sessions[serveSessionName("t1")] = true
	// legacy serve 清理落 reap_failed（retryable notice）。
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); alive {
		t.Fatal("runtime must not resume when legacy cleanup leaves retryable debt")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if r, _ := e.Data["reason"].(string); r == noticeReasonReapFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("legacy cleanup debt notice missing: %v", row.Notice)
	}
}

// TestReconcileActivating_KillDispositionTable 验证 G4-4/G4-5④：activating 清理的
// KillResult 处置按 classifyKillResult 完整表——仅一致 clean 无 notice；矛盾
// clean+!SessionKilled / 未知 disposition / kill infra 错误 / retryable disposition
// 均显式记录（未知/矛盾按 retryable kill_failed），不静默遗留无 notice 的失败。
func TestReconcileActivating_KillDispositionTable(t *testing.T) {
	cases := []struct {
		name       string
		killRes    *process.KillResult
		killErr    error
		wantNotice string // 空 = 无 notice
	}{
		{
			name:    "consistent clean no notice",
			killRes: &process.KillResult{SessionKilled: true, Disposition: process.DispositionClean},
		},
		{
			name:       "reap_failed records retryable notice",
			killRes:    &process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed},
			wantNotice: noticeReasonReapFailed,
		},
		{
			name:       "contradictory clean without SessionKilled fails closed as kill_failed",
			killRes:    &process.KillResult{SessionKilled: false, Disposition: process.DispositionClean},
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:       "unknown disposition fails closed as kill_failed",
			killRes:    &process.KillResult{SessionKilled: true, Disposition: process.CleanupDisposition("weird")},
			wantNotice: noticeReasonKillFailed,
		},
		{
			name:       "kill infra error records retryable notice",
			killErr:    errors.New("tmux unreachable"),
			wantNotice: noticeReasonKillFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			proc := newMockProc()
			seedPersistRuntime(t, store, proc)
			store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
			if tc.killRes != nil {
				proc.killResults[runtimeSessionName("t1")] = *tc.killRes
			}
			proc.killSessionErr = tc.killErr
			m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
			m.cfg.ShutdownPolicy = "persist"

			_ = m.Reconcile(context.Background())
			assertStatus(t, store, "t1", StatusSuspended)
			row, _ := store.GetTask(context.Background(), "t1")
			entries, perr := parseNotices(row.Notice)
			if perr != nil {
				t.Fatalf("notice parse: %v", perr)
			}
			if tc.wantNotice == "" {
				if len(entries) != 0 {
					t.Errorf("clean kill must not record notice: %v", row.Notice)
				}
				return
			}
			found := false
			for _, e := range entries {
				if r, _ := e.Data["reason"].(string); r == tc.wantNotice {
					found = true
				}
			}
			if !found {
				t.Errorf("notice missing reason %s: %v", tc.wantNotice, row.Notice)
			}
		})
	}
}

// TestReopenAttach_BusyWaitsLockThenDispatches 验证 G4-3/G4-5⑤：锁竞争不直接抛
// conflict——等锁后按 D8 表重新分派（active + 存活 runtime → 返回 terminal）。
func TestReopenAttach_BusyWaitsLockThenDispatches(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct {
		tid TerminalID
		err error
	}, 1)
	go func() {
		tid, err := m.ReopenAttach(context.Background(), "t1")
		done <- struct {
			tid TerminalID
			err error
		}{tid, err}
	}()
	time.Sleep(80 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ReopenAttach returned while lock held (must wait and re-dispatch)")
	default:
	}
	unlock()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ReopenAttach after lock released: %v", r.err)
		}
		if string(r.tid) != runtimeSessionName("t1") {
			t.Fatalf("tid=%s want %s", r.tid, runtimeSessionName("t1"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReopenAttach did not dispatch after lock released")
	}
}

// TestReopenAttach_LockWaitActivatingTransitionRecovers 验证 G4-3（attempt 2）：
// 等锁期间 active→activating（并发恢复 CAS）是合法迁移——等待器只拿锁不复查状态，
// ReopenAttach 主流程按 D8 表落 activating 分支 → typed recovering（而非
// invalid_state → WS 4010 让前端停止重连）。
func TestReopenAttach_LockWaitActivatingTransitionRecovers(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	unlock, err := m.tryLockTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.ReopenAttach(context.Background(), "t1")
		done <- err
	}()
	time.Sleep(80 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ReopenAttach returned while lock held (must wait)")
	default:
	}
	// 持锁方释放前把任务迁入 activating（模拟并发恢复已 CAS）。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	unlock()

	select {
	case err := <-done:
		if err == nil || OpErrorCode(err) != codeRecovering {
			t.Fatalf("err=%v want typed recovering (not invalid_state/4010)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReopenAttach did not dispatch after lock released")
	}
}

// TestEnsureRecoveryFromAttach_ZeroSideEffectsOnReject 验证 G4-6：attach 入口的
// 新 trigger 分配（NewInstVersion 会改写权威 tombstone）延迟到 CAS matched 后——
// 未知 kind 与未清 recovery debt 两个拒绝路径 tombstone 保持原值、零副作用。
func TestEnsureRecoveryFromAttach_ZeroSideEffectsOnReject(t *testing.T) {
	newSetup := func(kind string) (*mockStore, *Manager) {
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
		if kind != "repo" {
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/tmp/p1", Kind: kind})
		}
		proc := newMockProc()
		m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
		return store, m
	}

	// 未知 kind：tombstone 不变、状态保持 active、零 permit。
	store, m := newSetup("weird")
	before, hadBefore := m.runtimeRegistry.Tombstone("t1")
	m.ensureRecoveryFromAttach("t1")
	after, _ := m.runtimeRegistry.Tombstone("t1")
	if hadBefore != false || after != before {
		t.Errorf("tombstone changed on unknown-kind reject: before=%q(%v) after=%q", before, hadBefore, after)
	}
	assertStatus(t, store, "t1", StatusActive)
	if got := store.recoveryPermitCount("t1"); got != 0 {
		t.Errorf("permits=%d want 0", got)
	}

	// 未清 recovery debt：同样零 tombstone 副作用。
	store, m = newSetup("repo")
	if err := store.UpsertRecoveryDebt(context.Background(), RecoveryDebtRow{
		TaskID: "t1", SessionName: "", Phase: recoveryDebtPhaseComplete, Tickets: "[]",
		Cause: "stale", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	before, _ = m.runtimeRegistry.Tombstone("t1")
	m.ensureRecoveryFromAttach("t1")
	after, _ = m.runtimeRegistry.Tombstone("t1")
	if after != before {
		t.Errorf("tombstone changed on debt reject: before=%q after=%q", before, after)
	}
	assertStatus(t, store, "t1", StatusActive)
	if got := store.recoveryPermitCount("t1"); got != 0 {
		t.Errorf("permits=%d want 0", got)
	}
}

// TestReconcilePersist_LegacyCleanupErrBlocksResume 验证 G4-7：legacy 清理
// kill/disposition 失败且 notice 写失败（重读看不到 retryable debt）时，kerr 本身
// 阻断 resume——任务连同 runtime 清理落挂起，不得重建 SSE 后遗留存活 legacy。
func TestReconcilePersist_LegacyCleanupErrBlocksResume(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	seedPersistRuntime(t, store, proc)
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc.sessions[serveSessionName("t1")] = true
	// kill 落 kill_failed（retryable）且 notice 写失败：重读 notice 看不到 debt，
	// 只有 kerr 非空能阻断。
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
	}
	store.noticeCasErr = errors.New("notice store down")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = "persist"

	if err := m.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile must report legacy cleanup error")
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); alive {
		t.Fatal("runtime must not resume when legacy cleanup kill fails")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || !contains(row.LastError.String, "legacy cleanup") {
		t.Errorf("last_error=%v want legacy cleanup context", row.LastError)
	}
}
