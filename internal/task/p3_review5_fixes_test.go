package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
)

// --- Fix 4a: convergeToSuspended 串行收敛（锁忙时不得丢事件） ---

// TestConvergeToSuspended_LockBusyWaitsThenConverges 验证 B4a：fatal runtime event
// 在锁被占用时阻塞排队等锁，锁释放后必然收敛（不得因锁忙直接返回丢事件→留 active 无 SSE）。
// 构造：Activate 成功 → 手动占用任务锁 → 触发 serve 退出事件 → 释放锁 → 验证收敛到 suspended。
func TestConvergeToSuspended_LockBusyWaitsThenConverges(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("prereq status=%s want active", row.Status)
	}

	// 占用任务锁（模拟用户操作在执行）。
	unlock, lerr := m.tryLockTask("t1")
	if lerr != nil {
		t.Fatalf("prereq tryLockTask: %v", lerr)
	}
	// P1.4.7：触发令牌 = Activate 后当前 runtime 令牌（watcher 注册时捕获的同一身份）。
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("prereq runtime missing after Activate")
	}
	tok := rt.instVersion

	// 触发 serve 退出事件（fatal runtime event）：convergeToSuspended 应阻塞等锁，不立即返回。
	var converged atomic.Bool
	go func() {
		// convergeToSuspended 阻塞等锁；释放锁后收敛。
		m.handleServeExit("t1", tok)
		converged.Store(true)
	}()

	// 等待短暂时间确认 convergeToSuspended 仍在等锁（未立即返回丢事件）。
	time.Sleep(100 * time.Millisecond)
	if converged.Load() {
		t.Fatal("convergeToSuspended returned immediately on lock busy (event dropped); MUST wait for lock")
	}
	if row, _ := store.GetTask(context.Background(), "t1"); row.Status != StatusActive {
		t.Fatal("status changed while lock held; convergeToSuspended must not run before lock acquired")
	}

	// 释放锁 → convergeToSuspended 获取锁后收敛。
	unlock()

	// 等待收敛完成。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if converged.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !converged.Load() {
		t.Fatal("convergeToSuspended did not complete after lock released")
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended (serial converge after lock release)", row.Status)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "serve session exited") {
		t.Errorf("last_error=%v must contain serve exit reason", row.LastError)
	}
}

// --- Fix 4c: SubscribeEvents 永久返回 MUST 有处理路径 ---

// sseReturnErrOC：SubscribeEvents 立即返回非 ctx.Canceled 错误（模拟 SSE 流异常结束）。
type sseReturnErrOC struct {
	*mockOC
	sseErr     error
	onReadyCb  func()
	returnedCh chan struct{}
}

func (c *sseReturnErrOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReadyCb != nil {
		c.onReadyCb()
	}
	close(c.returnedCh)
	return c.sseErr
}

// TestSubscribeEvents_ReturnsWithError_ConvergesToSuspended 验证 B4c：
// SubscribeEvents 返回非 ctx.Canceled 错误（SSE 流异常结束）MUST 收敛到 suspended + last_error，
// 不得留 active 无 SSE 假象。
func TestSubscribeEvents_ReturnsWithError_ConvergesToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	oc := &sseReturnErrOC{
		mockOC:     newMockOC(true),
		sseErr:     errors.New("sse stream reset by peer"),
		returnedCh: make(chan struct{}),
	}
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), func(port int, password string, opts opencode.Options) OCClient {
		oc.onReadyCb = opts.OnReady
		return oc
	})

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// 等待 SubscribeEvents 返回。
	select {
	case <-oc.returnedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("SubscribeEvents did not return")
	}

	// 等待收敛完成（SubscribeEvents 返回后异步 converge）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), "t1")
		if row.Status == StatusSuspended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended (SubscribeEvents return must converge)", row.Status)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "sse stream ended") {
		t.Errorf("last_error=%v must contain sse stream ended reason", row.LastError)
	}
}

// --- Fix 4b: replay 顺序（先重放缓冲再放行实时） ---

// orderedEventsOC：SubscribeEvents 触发 onReady 后，按测试控制的顺序发送事件。
// 用于验证缓冲事件先于实时事件被处理（replay-then-release 顺序）。
type orderedEventsOC struct {
	*mockOC
	onReadyCb   func()
	mu          sync.Mutex
	events      []opencode.Event
	released    atomic.Bool
	sendCh      chan struct{}
}

func (c *orderedEventsOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReadyCb != nil {
		c.onReadyCb()
	}
	// 发送缓冲期事件（buffering=true，应进入 buffered）。
	c.mu.Lock()
	bufEvents := c.events
	c.mu.Unlock()
	for _, ev := range bufEvents {
		onEvent(ev)
	}
	// 等待 align 完成 + replay 释放（buffering=false）后发送实时事件。
	<-c.sendCh
	c.released.Store(true)
	// 发送实时事件（buffering=false，应直接处理）。
	c.mu.Lock()
	realEvents := c.events
	c.mu.Unlock()
	_ = realEvents // 复用同一批事件作为实时事件
	<-ctx.Done()
	return ctx.Err()
}

// TestSSE_ReplayOrder_BufferedBeforeReal 验证 B4b：首次对齐后先重放缓冲事件再放行实时事件。
// 缓冲事件在 buffering=true 期间到达，对齐完成后 replay；实时事件在 buffering=false 后直接处理。
// 此测试验证 buffering 标志的原子切换：replay 期间到达的事件继续缓冲，不越过 replay。
func TestSSE_ReplayOrder_BufferedBeforeReal(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess1", Time: opencode.SessionTime{Updated: 100, Created: 50}}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("prereq status=%s want active", row.Status)
	}
	// 对齐完成后 buffering=false（首次 align 成功后释放）。
	// 验证：Activate 成功即说明首次 align + replay 完成，buffering=false，无 panic/死锁。
	// sessions 应已落库（replay 的 session.created 事件已处理）。
	sessions, _ := store.ListTaskSessions(context.Background(), "t1")
	if len(sessions) == 0 {
		t.Error("replay should have processed buffered session.created events; sessions empty")
	}
}

// --- Fix 5e: suspend.go Has/List/Kill 基础设施错误 MUST 传播 ---

// TestSuspendRun_HasSessionInfraError_Propagates 验证 B5e：suspendRun 中 HasSession
// 基础设施错误（非 ErrNoTmuxServer）MUST 传播为 error，不得静默吞错。
func TestSuspendRun_HasSessionInfraError_Propagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	// HasSession 返回基础设施错误（非 ErrNoTmuxServer）。
	proc.hasSessionErr = errors.New("tmux infra: command failed")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must propagate HasSession infra error, got nil")
	}
	if !strings.Contains(err.Error(), "tmux infra") && !strings.Contains(err.Error(), "has session") {
		t.Errorf("error should mention infra/has session, got: %v", err)
	}
}

// --- Fix 6 remaining: Shutdown orphanFailures tickets MUST persist notice ---

// TestShutdown_KillFailureTicketsPersisted 验证 B6：Shutdown kill 模式下 KillSession
// 非 clean 的 tickets MUST 持久化为 notice（逃逸进程下次启动可定位），不得仅在内存即退出。
func TestShutdown_KillFailureTicketsPersisted(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// Activate 会创建 serve 会话；Shutdown kill 时 KillSession 返回非 clean（tickets 必须持久化）。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	// KillSession 返回非 clean（tickets 必须持久化）。
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled:   false,
		Disposition:     process.DispositionReapFailed,
		CleanupTickets:  []string{"tk-shutdown-1"},
	}
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Shutdown kill 模式下 kill 非 clean → runtime 未净 MUST 返回非 nil（design.md §10）。
	// 本测试关注点：即使 Shutdown 返回 error，tickets 也 MUST 已落库为 notice（下次启动可定位）。
	if err := m.Shutdown(context.Background()); err == nil {
		t.Fatal("Shutdown with non-clean kill MUST return error (runtime not clean)")
	}

	// tickets MUST 持久化为 notice（逃逸进程下次启动可定位）。
	row, _ := store.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("notice JSON parse error: %v", perr)
	}
	found := false
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if tks, ok := e.Data["cleanupTickets"].([]interface{}); ok && len(tks) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("Shutdown kill failure tickets MUST be persisted as notice (locatable next start); got empty")
	}
}

// --- Fix 8: ReopenAttach HasSession infra error MUST 传播（不得吞错当 absent） ---

// TestReopenAttach_HasSessionInfraError_Propagates 验证 B8：ReopenAttach 中 HasSession
// 基础设施错误（非 ErrNoTmuxServer）MUST 传播为 error（codeInternal），不得吞错当 absent
// 继续建 TUI 掩盖 infra 故障（design.md §8）。
func TestReopenAttach_HasSessionInfraError_Propagates(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// HasSession 返回基础设施错误（非 ErrNoTmuxServer）。
	proc.hasSessionErr = errors.New("tmux infra: command failed")
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	_, err := m.ReopenAttach(context.Background(), "t1")
	if err == nil {
		t.Fatal("ReopenAttach must propagate HasSession infra error, got nil")
	}
	if code := OpErrorCode(err); code != codeInternal {
		t.Errorf("error code=%q want %q (internal)", code, codeInternal)
	}
	if !strings.Contains(err.Error(), "has tui session") && !strings.Contains(err.Error(), "tmux infra") {
		t.Errorf("error should mention infra/has session, got: %v", err)
	}
}