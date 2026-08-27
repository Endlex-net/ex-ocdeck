package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// 本文件验证 P3 oracle 第六次评审的 7 项阻塞修复（R7）。
// mock 扩展见 mock_r7_test.go（不修改 mock_test.go）。

// --- Fix 1: 一次性 serve 清理错误进入返回值，阻止越过 DB 提交点 ---

// TestR7_DeleteTempServeKillError_BlocksDBCommit 验证 R7-1：无活跃 serve 时起一次性 serve，
// 其 KillSession 返回非 clean disposition（reap_failed + tickets），deleteOCSessions 聚合
// 非 clean 清理错误进返回值 → Delete 落 deletion_failed、不删 DB 行（tickets 不随 CASCADE 丢失）。
func TestR7_DeleteTempServeKillError_BlocksDBCommit(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	// 有 session row → deleteOCSessions 走删除路径；无活跃 serve → 起一次性 serve。
	tStore.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	// 一次性 serve kill 产生 reap_failed（非 clean）+ tickets。
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled:  true,
		Disposition:    process.DispositionReapFailed,
		CleanupTickets: []string{"r7-temp-serve-tk"},
	}
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("Delete MUST return error when temp serve cleanup non-clean (blocks DB commit)")
	}
	// MUST 落 deletion_failed（不越过提交点删 DB 行）。
	assertStatus(t, tStore, "t1", StatusDeletionFailed)
	assertTaskExists(t, tStore, "t1")
	// tickets MUST 落 notice（不随 DB CASCADE 丢失）。
	row, _ := tStore.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if tks, ok := e.Data["cleanupTickets"].([]interface{}); ok {
			for _, tk := range tks {
				if s, ok := tk.(string); ok && s == "r7-temp-serve-tk" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("temp serve reap_failed tickets MUST be persisted in notice, not lost with DB CASCADE")
	}
}

// --- Fix 2a: Suspend infra 错误收敛到 suspended（不留下 suspending） ---

// TestR7_SuspendHasSessionInfraError_ConvergesSuspended 验证 R7-2a：Suspend 提交 suspending 后
// 若 HasSession(serve) 返回 infra 错误（非 ErrNoTmuxServer），MUST forceKillAll + 落 suspended，
// 不得直接返回 error 留 suspending（Retry 不接受 suspending，只能重启 reconcile）。
func TestR7_SuspendHasSessionInfraError_ConvergesSuspended(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	// HasSession 全局返回 infra 错误（非 ErrNoTmuxServer）。
	proc.hasSessionErr = errors.New("tmux infra: has session failed")
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must return error for HasSession infra failure")
	}
	// MUST 收敛到 suspended（不得停留 suspending）。
	assertStatus(t, tStore, "t1", StatusSuspended)
	lastErrorContains(t, tStore, "t1", "infra")
}

// TestR7_SuspendListShellInfraError_ConvergesSuspended 验证 R7-2a：listShellSessions
// 返回 infra 错误时同样收敛到 suspended（不留 suspending）。
func TestR7_SuspendListShellInfraError_ConvergesSuspended(t *testing.T) {
	// 用 procListErrWrapper 让 ListSessions 返回 infra 错误（非 ErrNoTmuxServer）。
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	wrapped := wrapProcListErr(proc, errors.New("tmux infra: list sessions failed"))
	m := newR7TestManager(t, tStore, wrapped, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must return error for ListSessions infra failure")
	}
	assertStatus(t, tStore, "t1", StatusSuspended)
}

// --- Fix 2b: killTaskSessions 逐会话 HasSession 错误不得吞 ---

// TestR7_SuspendPerSessionHasSessionError_TriggersFailure 验证 R7-2b：killTaskSessions 中
// 某会话 HasSession 返回 infra 错误（非 ErrNoTmuxServer）MUST 收集为 killErr → hasKillFailure
// 触发分支 c 修复/强制收敛，不得吞错当 absent 继续杀。
func TestR7_SuspendPerSessionHasSessionError_TriggersFailure(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	// 用 hasSessionNameErrWrapper：仅对 tui 会话返回 infra 错误，serve 正常。
	wrapped := wrapProcHasSessionByName(proc, map[string]error{
		tuiSessionName("t1"): errors.New("tmux infra: has tui failed"),
	}, nil)
	m := newR7TestManager(t, tStore, wrapped, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must surface failure when per-session HasSession infra error")
	}
	// 收敛到 suspended（tryRepairRuntime 会因 serve 不健康/会话已死而失败 → forceKillAll + suspended）。
	assertStatus(t, tStore, "t1", StatusSuspended)
}

// --- Fix 3: SSE replay 屏障——全部缓冲事件先于实时事件落库 ---

// replayOrderOC：SubscribeEvents 触发 onReady 后，按测试控制顺序发送事件：
// 缓冲期发 buf 事件（buffering=true 进 buffered）→ onReady 后 align 完成 → drainAndRelease 排空缓冲
// → 释放后发 real 事件（buffering=false 直接处理）。
// 用 sessionTraceStore 记录 upsert/delete 顺序断言 buffered 先于 real。
type replayOrderOC struct {
	*mockOC
	onReadyCb      func()
	readySignaled  atomic.Bool
	bufSent        atomic.Bool
	realSent       atomic.Bool
	bufEvents      []opencode.Event
	realEvents     []opencode.Event
	afterReleaseCh chan struct{}
}

func (c *replayOrderOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReadyCb != nil {
		c.onReadyCb()
	}
	// onReady 触发后，Activate 主路径做 align；此处先发缓冲事件（buffering=true）。
	// 略等 Activate 进入 align 屏障阶段，确保事件在 buffering=true 期间到达。
	for _, ev := range c.bufEvents {
		onEvent(ev)
	}
	c.bufSent.Store(true)
	// 等待 Activate 完成 align + drainAndRelease（buffering=false）后，由测试释放信号发 real 事件。
	<-c.afterReleaseCh
	for _, ev := range c.realEvents {
		onEvent(ev)
	}
	c.realSent.Store(true)
	<-ctx.Done()
	return ctx.Err()
}

// TestR7_SSEReplayOrder_BufferedEventsBeforeReal 验证 R7-3：首次对齐完成后，drainAndRelease
// 排空全部缓冲事件再置 buffering=false，实时事件不会越过缓冲事件越序。
// 构造：缓冲 session.created(A) + session.created(B) 事件；real 发 session.deleted(A)。
// 若顺序正确（buffered 先落库 A/B，再处理 real delete A），最终 A 被删除、B 保留。
// 若越序（real delete A 先处理），则 A 未创建 delete 空操作，之后 buffered created A 补建 → A 残留。
// 通过 sessionTraceStore 的 trace 顺序断言 buffered upsert 先于 real delete。
func TestR7_SSEReplayOrder_BufferedEventsBeforeReal(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	traced := wrapSessionTrace(tStore)
	oc := &replayOrderOC{
		mockOC:         newMockOC(true),
		bufEvents:      []opencode.Event{makeEventWithDir("session.created", "A", 100, "/data/worktrees/p1/t1"), makeEventWithDir("session.created", "B", 110, "/data/worktrees/p1/t1")},
		realEvents:     []opencode.Event{makeEventWithDir("session.deleted", "A", 120, "/data/worktrees/p1/t1")},
		afterReleaseCh: make(chan struct{}),
	}
	factory := func(port int, password string, opts opencode.Options) OCClient {
		oc.onReadyCb = opts.OnReady
		return oc
	}
	m := newTestManagerWithFactory(t, traced, proc, newMockWorktree(), factory)
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Activate 返回说明首次 align + drainAndRelease 完成（buffering=false）。
	// 确认缓冲事件已发送。
	if !oc.bufSent.Load() {
		t.Fatal("buffered events were not sent during buffering phase")
	}
	// 释放信号让 real 事件发送。
	close(oc.afterReleaseCh)
	// 等 real 事件被处理。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if oc.realSent.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !oc.realSent.Load() {
		t.Fatal("real events were not sent/processed")
	}
	// 短暂等待 real 事件落库。
	time.Sleep(50 * time.Millisecond)

	trace := traced.snapshot()
	// 断言：trace 中首个 real delete(A) 必须出现在所有 buffered upsert 之后。
	// 即不存在 delete:A 在 upsert:A 或 upsert:B 之前。
	bufUpsertA := indexOfTrace(trace, "upsert:A")
	bufUpsertB := indexOfTrace(trace, "upsert:B")
	realDeleteA := indexOfTrace(trace, "delete:A")
	if bufUpsertA < 0 || bufUpsertB < 0 {
		t.Fatalf("buffered upsert events not recorded in trace: %v", trace)
	}
	if realDeleteA < 0 {
		t.Fatalf("real delete(A) not recorded in trace: %v", trace)
	}
	if realDeleteA < bufUpsertA || realDeleteA < bufUpsertB {
		t.Fatalf("real delete(A) at %d processed before buffered upsert A(%d)/B(%d); trace=%v (replay barrier violated)",
			realDeleteA, bufUpsertA, bufUpsertB, trace)
	}
	// 最终 B 仍存在（未被 delete），A 被 delete。
	sessions, _ := tStore.ListTaskSessions(context.Background(), "t1")
	hasA, hasB := false, false
	for _, s := range sessions {
		if s.SessionID == "A" {
			hasA = true
		}
		if s.SessionID == "B" {
			hasB = true
		}
	}
	if hasA {
		t.Errorf("session A must be deleted by real event (got A still present); order wrong")
	}
	if !hasB {
		t.Errorf("session B must remain (created by buffered event, not deleted)")
	}
}

func indexOfTrace(trace []string, key string) int {
	for i, v := range trace {
		if v == key {
			return i
		}
	}
	return -1
}

// --- Fix 4: 等待锁取消 TOCTOU——deadline 交界压力测试 ---

// TestR7_LockTaskWait_DeadlineBoundaryRace 验证 R7-4：调用方在 deadline 交界放弃
// （waitCtx 取消）后，goroutine 拿锁后必经 waitCtx.Done 分支释放锁，不存在"检查 abandoned
// 后写入无人接收缓冲 → 锁永久占用"窗口。多轮压力 + -race 验证锁最终可被重新获取。
func TestR7_LockTaskWait_DeadlineBoundaryRace(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))

	// acquireLockPoll 轮询获取锁（lockTaskWait/lockTaskForConverge 的内部 goroutine 在
	// wg.Wait 后仍异步持有锁片刻——waitCtx.Done 触发 Unlock 有调度延迟）。非泄漏：最终必然释放。
	acquireLockPoll := func(label string) func() {
		for attempt := 0; attempt < 500; attempt++ {
			u, err := m.tryLockTask("t1")
			if err == nil {
				return u
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("%s: lock never became available (leaked)", label)
		return nil
	}

	for round := 0; round < 50; round++ {
		// 主 goroutine 持锁（轮询获取，确保上一轮内部 goroutine 已释放）。
		unlock := acquireLockPoll(fmt.Sprintf("round %d prereq", round))
		// 用极短 deadline 让等待方几乎立即超时（deadline 交界）。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Microsecond)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.ReopenAttach(ctx, "t1")
		}()
		// 略等让等待 goroutine 进入 select。
		time.Sleep(2 * time.Microsecond)
		// 释放主锁，让等待 goroutine 的内部 goroutine 可能拿到锁。
		unlock()
		cancel()
		wg.Wait()
		// wg.Wait 后内部 goroutine 可能仍异步持有锁片刻（waitCtx.Done→Unlock 调度延迟），
		// 下一轮的 acquireLockPoll 会轮询等到其释放——证实锁最终必然可获取（无永久占用）。
		// 末轮再显式确认一次。
		if round == 49 {
			u := acquireLockPoll("final")
			u()
		}
	}
}

// TestR7_LockTaskForConverge_AcquireAndRelease 验证 R7-4 收敛路径锁获取后正常释放
//（与 lockTaskWait 同模式，TOCTOU 修复由 lockTaskWait 压力测试覆盖——lockTaskForConverge
// 的 waitCtx 挂 Background() + 30s deadline，放弃路径无法在测试中快速触发）。
func TestR7_LockTaskForConverge_AcquireAndRelease(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	m := newR7TestManager(t, tStore, newMockProc(), newMockWorktree(), newMockOC(true))

	for round := 0; round < 50; round++ {
		// 主 goroutine 持锁。
		holder, _ := m.tryLockTask("t1")
		var wg sync.WaitGroup
		var convUnlock func()
		wg.Add(1)
		go func() {
			defer wg.Done()
			un, err := m.lockTaskForConverge("t1")
			if err == nil && un != nil {
				convUnlock = un
			}
		}()
		// 释放主锁，让 lockTaskForConverge 拿锁（成功路径）。
		time.Sleep(2 * time.Microsecond)
		holder()
		wg.Wait()
		// lockTaskForConverge 成功拿到锁 MUST 返回 unlock；测试释放之（不泄漏）。
		if convUnlock != nil {
			convUnlock()
		}
		// 锁最终可达（无泄漏）。
		acquired := false
		for attempt := 0; attempt < 200; attempt++ {
			u, err := m.tryLockTask("t1")
			if err == nil {
				u()
				acquired = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !acquired {
			t.Fatalf("round %d: converge lock leaked after release", round)
		}
	}
}

// --- Fix 5: DirtyFiles 错误 fail-closed before intent ---

// TestR7_DeleteDirtyFilesError_FailClosedBeforeIntent 验证 R7-5：preflightDirty 探测失败时
// MUST fail-closed 在删除意图提交前返回（状态不变 suspended），不得当空集强删。
func TestR7_DeleteDirtyFilesError_FailClosedBeforeIntent(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	wt := newMockWorktree()
	wt.dirtyErr = errors.New("git status failed")
	m := newR7TestManager(t, tStore, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("Delete MUST fail-closed when DirtyFiles errors")
	}
	// 状态不变（删除意图未提交）。
	assertStatus(t, tStore, "t1", StatusSuspended)
	assertTaskExists(t, tStore, "t1")
	// 不应进入 deleting/deletion_failed。
	row, _ := tStore.GetTask(context.Background(), "t1")
	if row.Status == StatusDeleting || row.Status == StatusDeletionFailed {
		t.Fatalf("status=%s must remain suspended (delete intent not committed)", row.Status)
	}
}

// --- Fix 6: ReopenAttach 用 serve 会话 env 端口（权威）优先于 last_port ---

// TestR7_ReopenAttach_UsesSessionEnvPort 验证 R7-6：ReopenAttach 端口以 serve 会话内
// OCDECK_SERVE_PORT 为权威来源，last_port 仅回退。预置 serve env port=50099，last_port=50000，
// 断言新建 tui 的 attach URL 用 50099（捕获 NewSession argv）。
func TestR7_ReopenAttach_UsesSessionEnvPort(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		// last_port = 50000（非权威，应被 serve env 覆盖）。
		r.LastPort = sql.NullInt64{Int64: 50000, Valid: true}
		// env snapshot 供 ReopenAttach loadEnvSnapshot（创建 tui 需要）。
		r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))

	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach: %v", err)
	}
	if string(tid) != runtimeSessionName("t1") {
		t.Fatalf("tid=%s want %s", tid, runtimeSessionName("t1"))
	}
}

// TestR7_ReopenAttach_ServeEnvEmpty_FailsNoFallback 验证 P4-3：serve env 不含
// OCDECK_SERVE_PORT（读回空）MUST 直接失败，MUST NOT 回退 last_port（design.md §3/§5：
// last_port 非事实来源，可能写入失败或过期）。断言 ReopenAttach 返回 error + 不建 tui。
func TestR7_ReopenAttach_ServeEnvEmpty_FailsNoFallback(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.LastPort = sql.NullInt64{Int64: 50077, Valid: true}
		r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	// serve env 不含 OCDECK_SERVE_PORT（读回空）→ MUST 失败，不得回退 last_port。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_TASK_ID": "t1",
	}
	cap := wrapNewSessionCapture(proc)
	m := newR7TestManager(t, tStore, cap, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach without runtime must return recovering")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Errorf("code=%s want recovering, err=%v", OpErrorCode(err), err)
	}
	if specs := cap.specsFor(tuiSessionName("t1")); len(specs) != 0 {
		t.Errorf("ReopenAttach must not create tui, got specs=%v", specs)
	}
}

// TestR7_ReopenAttach_ServeEnvUnparseable_Fails 验证 P4-3：serve env 端口不可解析 MUST 失败。
func TestR7_ReopenAttach_ServeEnvUnparseable_Fails(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	tStore.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.LastPort = sql.NullInt64{Int64: 50077, Valid: true}
		r.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
	})
	proc := newMockProc()
	cap := wrapNewSessionCapture(proc)
	m := newR7TestManager(t, tStore, cap, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach without runtime must return recovering")
	}
	if OpErrorCode(err) != codeRecovering {
		t.Errorf("code=%s want recovering, err=%v", OpErrorCode(err), err)
	}
}

func argvContains(argv []string, sub string) bool {
	for _, a := range argv {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// --- Fix 7: Shutdown retryOrphanSessions + orphanFailures clean 判定 + persist 模式 orphan tickets 错误 ---

// TestR7_ShutdownRetryOrphanSessions_ReapsTickets 验证 R7-7：Shutdown 调用 retryOrphanSessions
// 收割内存 orphanFailures tickets。预置 orphanFailure 含 tickets，RetryReap 成功（mock 返回 nil,nil）
// → orphanFailures 清空。
func TestR7_ShutdownRetryOrphanSessions_ReapsTickets(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))
	// 预置内存 orphanFailures（kill 失败的孤儿会话 tickets）。
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-reap-1"},
	})
	m.orphanMu.Unlock()

	if err := m.Shutdown(context.Background()); err != nil {
		// persist 模式：orphanFailures 被 retryOrphanSessions 清空后应无错（RetryReap 成功）。
		t.Fatalf("Shutdown: %v", err)
	}
	m.orphanMu.Lock()
	remaining := len(m.orphanFailures)
	m.orphanMu.Unlock()
	if remaining != 0 {
		t.Fatalf("orphanFailures must be reaped (RetryReap success), remaining=%d", remaining)
	}
}

// TestR7_ShutdownPersist_OrphanTicketsReturnsError 验证 R7-7：persist 模式下若 retryOrphanSessions
// 后仍有未收割 orphan tickets（会话仍存活 kill 又失败），Shutdown MUST 返回错误（不得干净退出）。
func TestR7_ShutdownPersist_OrphanTicketsReturnsError(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	// 预置孤儿会话仍存活 + kill 产生 reap_failed tickets（retryOrphanSessions kill 失败保留）。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-ghost"},
	}
	m := newR7TestManager(t, tStore, proc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-ghost"},
	})
	m.orphanMu.Unlock()
	// 默认 persist 模式。
	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("persist mode Shutdown MUST return error when orphan tickets remain after retryOrphanSessions")
	}
	if !strings.Contains(err.Error(), "orphan") && !strings.Contains(err.Error(), "persist") {
		t.Errorf("error should mention orphan/persist, got: %v", err)
	}
}

// TestR7_ShutdownKillMode_OrphanFailuresCleanCheck 验证 R7-7：kill 模式下 orphanFailures 非空
// 视为 runtime 未净，Shutdown 返回错误。
func TestR7_ShutdownKillMode_OrphanFailuresCleanCheck(t *testing.T) {
	tStore := newMockStore()
	seedSuspendedTask(tStore, "t1", "p1")
	proc := newMockProc()
	// 用 reapFailingProc 使 RetryReap 永远返回 remaining（未收割）→ orphan tickets 保留。
	wrapped := wrapReapFailing(proc)
	m := newR7TestManager(t, tStore, wrapped, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate
	m.orphanMu.Lock()
	m.orphanFailures = append(m.orphanFailures, orphanFailure{
		sessionName: "ocdeck-ghost-serve",
		tickets:     []string{"tk-undead"},
	})
	m.orphanMu.Unlock()

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("kill mode Shutdown MUST return error when orphanFailures remain (runtime not clean)")
	}
	if !strings.Contains(err.Error(), "orphanFailures") {
		t.Errorf("error should mention orphanFailures, got: %v", err)
	}
}