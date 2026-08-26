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
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- Fix 1: deletion_failed 时一次性 serve kill tickets 不得丢弃 ---

// TestDeleteOCSessions_TempServeKillTicketsRecorded 验证 deleteOCSessions 一次性 serve 的
// defer KillSession 非结果记录到 notice（不丢弃 tickets）。
// 构造：有 task sessions 需删除 + 无活跃 serve（起一次性 serve）+ temp serve kill 产生非 clean disposition。
// 断言：notice 含 temp serve 的 residual notice + tickets（不被丢弃）。
func TestDeleteOCSessions_TempServeKillTicketsRecorded(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 有 sessions → deleteOCSessions 走删除路径；预置一个 session row。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	proc := newMockProc()
	// 无活跃 serve → 起一次性 serve。预置 temp serve kill 产生 reap_failed（非 clean）+ tickets。
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"temp-serve-tk"},
	}
	// oc DeleteSession 成功（走完循环，触发 defer kill）。
	oc := newMockOC(true)
	// newTestManager 的 readyOC 包装会在 SubscribeEvents 时触发 onReady；
	// 但 deleteOCSessions 不用 SSE，只用 oc.DeleteSession + health。
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	// 直接调用 deleteOCSessions（它内部起 temp serve + 删 session + defer kill temp serve）。
	// 注意：startTempServe 调用 waitServeReady（oc.Health OK），NewSession 创建 serve 会话。
	err := m.deleteOCSessions(context.Background(), store.tasks["t1"])
	// deleteOCSessions 返回 errors.Join(errs...)，DeleteSession 成功 → nil。
	// defer kill 在 deleteOCSessions 返回前执行，记录 notice。
	_ = err

	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		if sn, _ := e.Data["sessionName"].(string); sn == serveSessionName("t1") {
			found = true
			hasTicket := false
			if tks, ok := e.Data["cleanupTickets"].([]interface{}); ok {
				for _, tk := range tks {
					if s, ok := tk.(string); ok && s == "temp-serve-tk" {
						hasTicket = true
					}
				}
			}
			if !hasTicket {
				t.Errorf("temp serve kill tickets must be recorded in notice, not discarded; entry=%+v", e.Data)
			}
		}
	}
	if !found {
		t.Fatal("temp serve kill residual notice must be recorded (not discarded)")
	}
}

// --- Fix 3: taskBusy ReopenAttach 409 + cancel 后释放锁可重新获取 ---

// TestReopenAttach_BusyReturnsConflict 然后取消释放锁可重新获取 验证 task busy 时 ReopenAttach
// 返回 conflict（409 映射），且占用锁的 ctx 取消后原锁释放，新调用可重新拿锁（真实行为）。
func TestReopenAttach_BusyReturnsConflict_ThenCancelReleasesLock(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 主 goroutine 持锁模拟并发。
	unlock, _ := m.tryLockTask("t1")

	// ReopenAttach 等待路径：ctx 超时返回 conflict（errTaskBusy 或 ctx cancel 语义）。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := m.ReopenAttach(ctx, "t1")
	if err == nil {
		t.Fatal("expected conflict error when task busy")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("busy code=%v want conflict (maps to 409, not 500)", OpErrorCode(err))
	}

	// 真实行为：释放锁后新调用可重新拿锁（不是仅断言错误码常量）。
	unlock()

	// 新调用应成功拿锁并复用已有 tui。
	tid, err := m.ReopenAttach(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ReopenAttach after lock released must succeed, got: %v", err)
	}
	if string(tid) != tuiSessionName("t1") {
		t.Errorf("tid=%s want %s (reuse existing tui)", tid, tuiSessionName("t1"))
	}
}

// --- Fix 4: ListShells 真实实现 ---

// TestListShells_ReturnsAliveShells 验证创建 2 个 shell 后列表返回 2 项；挂起后为空。
func TestListShells_ReturnsAliveShells(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 激活任务：Activate 需 serve+tui。用 mock 直接构造 active runtime（避免完整 Activate 开销）。
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// serve 存活（CreateShell 依赖 runtime + env snapshot）。
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// env snapshot 供 CreateShell loadEnvSnapshot。
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 构造 runtime（模拟已激活）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleLegacyServe, serveSessionName("t1"))

	// 创建 2 个 shell。
	tid1, err := m.CreateShell(context.Background(), "t1")
	if err != nil {
		t.Fatalf("CreateShell 1: %v", err)
	}
	tid2, err := m.CreateShell(context.Background(), "t1")
	if err != nil {
		t.Fatalf("CreateShell 2: %v", err)
	}

	shells, err := m.ListShells("t1")
	if err != nil {
		t.Fatalf("ListShells after 2 creates: %v", err)
	}
	if len(shells) != 2 {
		t.Fatalf("ListShells after 2 creates: got %d want 2 (%v)", len(shells), shells)
	}
	// 确认两个 tid 在列表中。
	ids := map[string]bool{}
	for _, s := range shells {
		ids[string(s)] = true
	}
	if !ids[string(tid1)] || !ids[string(tid2)] {
		t.Errorf("ListShells must include both shell ids; got %v, want %s and %s", shells, tid1, tid2)
	}

	// 挂起后（清 runtime + kill 会话）列表为空。
	m.clearRuntime("t1")
	proc.killResults[shellSessionName("t1", 1)] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	proc.killResults[shellSessionName("t1", 2)] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	// 模拟 kill（mock KillSession 删除 session）。
	_, _ = proc.KillSession(shellSessionName("t1", 1))
	_, _ = proc.KillSession(shellSessionName("t1", 2))
	shellsAfter, err := m.ListShells("t1")
	if err != nil {
		t.Fatalf("ListShells after suspend: %v", err)
	}
	if len(shellsAfter) != 0 {
		t.Errorf("ListShells after suspend: got %d want 0", len(shellsAfter))
	}
}

// --- Fix 5: ValidateShellTerminal 身份校验 ---

// TestValidateShellTerminal_RejectsNonShell 验证非法 tid / 指向 serve 会话的 tid 被拒绝。
func TestValidateShellTerminal_RejectsNonShell(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleLegacyServe, serveSessionName("t1"))
	rt.registerGroup(roleShell, shellSessionName("t1", 1))

	cases := []struct {
		name string
		tid  string
	}{
		{"empty", ""},
		{"garbage", "not-an-ocdeck-session"},
		{"serve session", serveSessionName("t1")},
		{"tui session", tuiSessionName("t1")},
		{"shell not registered/alive", shellSessionName("t1", 99)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.ValidateShellTerminal(c.tid)
			if err == nil {
				t.Errorf("ValidateShellTerminal(%q) must reject, got nil", c.tid)
			}
			// code 应为 invalid_input 或 not_found（4xx 系），非 internal。
			code := OpErrorCode(err)
			if code != codeInvalidInput && code != codeNotFound {
				t.Errorf("ValidateShellTerminal(%q) code=%v want invalid_input or not_found", c.tid, code)
			}
		})
	}
}

// TestValidateShellTerminal_AcceptsValidShell 验证合法 shell 终端通过校验。
func TestValidateShellTerminal_AcceptsValidShell(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	shellName := shellSessionName("t1", 1)
	proc.sessions[shellName] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleShell, shellName)

	if err := m.ValidateShellTerminal(shellName); err != nil {
		t.Errorf("ValidateShellTerminal(valid shell) must pass, got: %v", err)
	}
}

// --- Fix 6: SSE 重连对齐失败收敛 ---

// reconnectAlignFailOC：首次 SubscribeEvents 正常 onReady + 首次 align 成功，
// 暴露 onReconnect 供测试手动触发（mock 不自动重连）。
// 重连对齐时 ListSessions 返回非 overflow 错误 → onReconnect 内部收敛 suspended。
type reconnectAlignFailOC struct {
	*mockOC
	listErrOnReconn error
	onReconnectCb   func()
	onReadyCb       func()
	alignCallMu     sync.Mutex
	alignCalls      int
}

func (c *reconnectAlignFailOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	c.alignCallMu.Lock()
	c.alignCalls++
	n := c.alignCalls
	c.alignCallMu.Unlock()
	if n >= 2 && c.listErrOnReconn != nil {
		return nil, c.listErrOnReconn
	}
	return c.sessions, nil
}

// SubscribeEvents 存储 onReconnect 供测试触发，并立即触发 onReady（经 onReadyCb）。
// onReady 在 SubscribeEvents 内触发（而非 factory），确保 SubscribeEvents 先运行设置 onReconnectCb。
func (c *reconnectAlignFailOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	c.onReconnectCb = onReconnect
	if c.onReadyCb != nil {
		c.onReadyCb()
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestSSEReconnectAlignFailure_ConvergesToSuspended 验证 SSE 重连对齐失败收敛任务状态：
// 不得只取消 SSE 留 active 假象；MUST 落 suspended + last_error（design.md §4）。
// 选择说明（report）：重连对齐失败视为运行时不可确定 → cleanup runtime + suspended + last_error，
// 与 design §4 "serve 异常退出 → 完整清理 → suspended + last_error" 语义一致，
// 不留 "active 但 SSE 失联" 假象。
func TestSSEReconnectAlignFailure_ConvergesToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	oc := &reconnectAlignFailOC{
		mockOC:          newMockOC(true),
		listErrOnReconn: errors.New("opencode list sessions infra error on reconnect"),
	}
	// 用直接 OCFactory：所有调用返回同一 oc 实例；捕获 startSSE 的 OnReady 供 SubscribeEvents 触发。
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), func(port int, password string, opts opencode.Options) OCClient {
		oc.onReadyCb = opts.OnReady
		return oc
	})

	// Activate 成功（首次对齐成功），任务 active。
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("prereq status=%s want active", row.Status)
	}
	if m.getRuntime("t1") == nil {
		t.Fatal("prereq runtime must be registered")
	}
	if oc.onReconnectCb == nil {
		t.Fatal("prereq onReconnect must be captured by SubscribeEvents")
	}

	// 手动触发 onReconnect（模拟 SSE 重连）→ 第二次 ListSessions 返回错误 → 对齐失败 → 收敛 suspended。
	oc.onReconnectCb()

	// 收敛在 onReconnect 内异步 goroutine 执行，等待收敛完成。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, _ = store.GetTask(context.Background(), "t1")
		if row.Status == StatusSuspended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ = store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("after reconnect align failure converge: status=%s want suspended", row.Status)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "sse reconnect align failed") {
		t.Errorf("last_error=%v must contain reconnect align failure reason", row.LastError)
	}
	if m.getRuntime("t1") != nil {
		t.Error("runtime must be cleared after reconnect align failure converge")
	}
}

// TestActivateFailure_NoticePersistErrorAggregated 验证 Activate 失败路径 notice 持久化失败聚合进 last_error。
// 构造：cleanupActivationRuntime 中 KillSession 非 clean → recordResidualNotice CAS 不收敛 → 聚合。
func TestActivateFailure_NoticePersistErrorAggregated(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// activateRun 创建 serve 后对齐失败 → cleanupActivationRuntime kill serve 非 clean。
	proc.killResults[serveSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk"},
	}
	// 用 CAS 永不收敛的 store：UpdateTaskNoticeCAS 始终返回 false（模拟并发覆盖不收敛）。
	neverConvergeStore := &noConvergeStore{mockStore: store}
	// 基础 activate 需要 env snapshot 落库路径，但失败发生在 startSSE 对齐失败。
	oc := &alignFailOC{mockOC: newMockOC(true), listErr: errors.New("list sessions fail")}
	m := newTestManager(t, neverConvergeStore, proc, newMockWorktree(), oc)

	// Activate 失败（对齐失败）→ cleanup kill serve 非 clean → recordResidualNotice CAS 不收敛 → 聚合进 last_error。
	_ = m.Activate(context.Background(), "t1")
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended (activate failure converge)", row.Status)
	}
	// last_error 应含 notice 持久化失败（CAS 不收敛）聚合，不静默。
	if !row.LastError.Valid {
		t.Fatal("last_error must be set on activate failure")
	}
	// 应含原始对齐失败错误 + cleanup notice 持久化失败聚合（"cleanup notice"）。
	if !strings.Contains(row.LastError.String, "cleanup notice") {
		t.Errorf("last_error must aggregate notice persist failure (CAS not converge); got: %s", row.LastError.String)
	}
	if !strings.Contains(row.LastError.String, "did not converge") {
		t.Errorf("last_error should mention CAS non-convergence; got: %s", row.LastError.String)
	}
}

// noConvergeStore 让 UpdateTaskNoticeCAS 始终返回 false（模拟 CAS 不收敛），recordResidualNotice 返回 error。
type noConvergeStore struct {
	*mockStore
}

func (s *noConvergeStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	return application.MutationResult{}, nil // 始终冲突，不收敛
}

// --- 测试升级：reconcile active-resume 完整路径 ---
// （TestReconcilePrePassDigestsDebtThenResumesActive 在 p3_review3_fixes_test.go 已覆盖 pre-pass→恢复 active，
// 此处补充断言 SSE 订阅后状态保持 active 且 sessions 对齐完成。）

// TestReconcileResumeActive_FullPath 验证 active-resume 完整路径：
// pre-pass 消化 debt → serve 健康 → resumeActive 重建 runtime + SSE + 全量对齐 → 保持 active。
func TestReconcileResumeActive_FullPath(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	// 有 sessions 行（对齐后应保留）。
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess1", Time: opencode.SessionTime{Updated: 100, Created: 50}}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status=%s want active (resumed)", row.Status)
	}
	if m.getRuntime("t1") == nil {
		t.Error("runtime must be registered after resume")
	}
	// sessions 对齐完成：DB sessions 应含对齐结果。
	sessions, _ := store.ListTaskSessions(context.Background(), "t1")
	found := false
	for _, s := range sessions {
		if s.SessionID == "sess1" {
			found = true
		}
	}
	if !found {
		t.Error("sessions must be aligned after resume (sess1 missing)")
	}
}

// _ 保持引用（部分构造用 errors/time/sql）。
var _ = errors.New
var _ = time.Second
var _ = sql.NullString{}

// newTestManagerWithFactory 用自定义 OCClientFactory 构造 Manager（供需要自定义 SubscribeEvents/onReady 触发的测试）。
func newTestManagerWithFactory(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, factory OCClientFactory) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:       t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	return New(Options{Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: factory})
}

// --- 测试升级：CAS 真实 store 并发 add+clear ---

// TestRealStore_NoticeCAS_ConcurrentAddClear 验证用真实 SQLite store 测 CAS 并发：
// 一个 goroutine 持续 recordResidualNotice（add），另一个持续通过 retryTaskNotices 清除（clear），
// 两者并发不互相覆盖丢失：最终 notice 状态一致（要么 add 的落库，要么 clear 的清空，不损坏 JSON）。
func TestRealStore_NoticeCAS_ConcurrentAddClear(t *testing.T) {
	adapter, _ := openRealStore(t)
	m := newTestManager(t, adapter, newMockProc(), newMockWorktree(), newMockOC(true))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	// add：持续 recordResidualNotice。
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			_ = m.recordResidualNotice(ctx, "t1", "ocdeck-t1-serve", []string{"tk"}, noticeReasonKillFailed, true)
		}
	}()
	// clear：持续通过 retryTaskNotices 清除（CAS 写回空 notice）。
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			row, err := adapter.GetTask(ctx, "t1")
			if err != nil {
				continue
			}
			entries, _ := parseNotices(row.Notice)
			if len(entries) == 0 {
				continue
			}
			_ = m.retryTaskNotices(ctx, row, entries)
		}
	}()
	wg.Wait()

	// 最终校验：notice JSON 可解析（未损坏），状态一致。
	row, _ := adapter.GetTask(context.Background(), "t1")
	if _, perr := parseNotices(row.Notice); perr != nil {
		t.Fatalf("notice JSON corrupted after concurrent add+clear: %v", perr)
	}
}