package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/process"
)

// --- Lane C：运行时事件与注册竞态 ---

// TestTypedRuntimeEvent_ServeInfraErrorSuspended 验证 C1：serve watcher 收到
// WatchEventInfraError（tmux 持续故障）→ 完整清理运行时 + last_error + suspended（不静默）。
func TestTypedRuntimeEvent_ServeInfraErrorSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup("serve", serveSessionName("t1"))
	rt.registerGroup("tui", tuiSessionName("t1"))
	m.watchServeExit("t1", serveSessionName("t1"))

	proc.triggerExit(serveSessionName("t1"), process.WatchEvent{
		Type: process.WatchEventInfraError,
		Err:  errors.New("tmux protocol error"),
	})
	time.Sleep(150 * time.Millisecond)

	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("infra_error should suspend task; status=%s want suspended", row.Status)
	}
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Error("infra_error must record last_error (not silent)")
	}
}

// TestTypedRuntimeEvent_TuiExitKeepsActive 验证 C1：tui watcher session_exit 保持 active（tui_exit 语义）。
func TestTypedRuntimeEvent_TuiExitKeepsActive(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup("serve", serveSessionName("t1"))
	rt.registerGroup("tui", tuiSessionName("t1"))
	m.watchTUIExit("t1", tuiSessionName("t1"))

	proc.triggerExit(tuiSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	time.Sleep(100 * time.Millisecond)

	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("tui_exit should keep active; status=%s want active", row.Status)
	}
	// tui group 应已从注册表移除。
	if _, ok := rt.groups[tuiSessionName("t1")]; ok {
		t.Error("tui group should be removed from registry after tui_exit")
	}
}

// TestTypedRuntimeEvent_OldGenIgnored_ViaCurrentRegistry 验证 C1 三元组隔离：
// 旧代回调不清理新代（回调校验当前 runtime 注册表，非捕获快照）。
// 旧代 serve watcher 触发 infra_error → 新代 runtime 不应受影响。
func TestTypedRuntimeEvent_OldGenIgnored_ViaCurrentRegistry(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// 旧代 runtime + serve group + watcher。
	oldRT := m.newRuntime("t1")
	m.setRuntime("t1", oldRT)
	oldRT.registerGroup("serve", serveSessionName("t1"))
	m.watchServeExit("t1", serveSessionName("t1"))

	// 新代 runtime（generation 递增）。
	newRT := m.newRuntime("t1")
	m.setRuntime("t1", newRT)
	newRT.registerGroup("serve", serveSessionName("t1"))

	// 旧代 watcher 触发 infra_error → 应不匹配新代注册表 → 忽略。
	proc.triggerExit(serveSessionName("t1"), process.WatchEvent{
		Type: process.WatchEventInfraError,
		Err:  errors.New("old gen infra"),
	})
	time.Sleep(100 * time.Millisecond)

	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("old-gen infra callback should not affect new-gen; status=%s want active", row.Status)
	}
}

// TestRegisterRuntime_ServeDeadBeforeRegister 验证 C2：activate 路径注册前校验 serve 存活，
// serve 已死则不注册 runtime，清已建会话回 suspended。
func TestRegisterRuntime_ServeDeadBeforeRegister(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// serve 在 waitServeReadyOrDead（HasSession 返回 true）+ Probe 通过后死亡：
	// 前 1 次 HasSession 返回 true（覆盖 health 轮询），第 2 次 false（注册前校验失败）。
	proc := &lateDieProc{mockProc: newMockProc(), aliveFor: 1}
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	err := m.activateRun(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error when serve dies before register")
	}
	// runtime 不应被注册。
	if m.getRuntime("t1") != nil {
		t.Error("runtime should not be registered when serve died before register")
	}
}

// lateDieProc：前 aliveFor 次 HasSession 返回 true，之后返回 false（模拟 serve 在
// health/Probe 通过后、注册前崩溃）。
type lateDieProc struct {
	*mockProc
	aliveFor    int
	hasCalls    int
}

func (p *lateDieProc) HasSession(name string) (bool, error) {
	p.mu.Lock()
	p.hasCalls++
	alive := p.hasCalls <= p.aliveFor
	p.mu.Unlock()
	return alive, nil
}

// TestRegisterRuntime_KillImmediate_OnlyAfterWatchdog 验证 C2：kill_immediate 模式下
// SpawnWatchdog 成功前 MUST NOT 注册 runtime。此为启动顺序契约，由 cmd/main.go 保证
// （spawnWatchdog 失败则 run() 返回，tm 不构造）。此处断言语义：仅当 watchdog 已 spawn
// 时 reconcile 才可注册 runtime。通过断言 Manager 在无 watchdog 注入下仍能正常工作，
// 但 kill_immediate 模式 reconcile 走 kill 路径（不 resume，不注册 runtime）。
func TestRegisterRuntime_KillImmediate_KillPathNoRuntime(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// kill 模式不注册 runtime（不 resume active）。
	if m.getRuntime("t1") != nil {
		t.Error("kill_immediate reconcile should not register runtime (no resume)")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (kill mode)", row.Status)
	}
}

// --- Lane F：reconcile 闭合 ---

// TestPrePass_NoSelfDeadlock 验证 F1：reconcileDebtPrePass 调用 retryTaskNotices 不自死锁。
// pre-pass 不持锁调用 retryTaskNotices（后者自取锁），debt 可被消化。
func TestPrePass_NoSelfDeadlock(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 注入 retryable debt：sessionName 指向已不存在的会话 + cleanupTickets。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc()
	// serve 会话不存在 → pre-pass 的 retryTaskNotices 走 reap 路径。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	// 直接调用 pre-pass（验证不自死锁：若自死锁，retryTaskNotices 拿不到锁 → 返回 nil 不消化）。
	m.reconcileDebtPrePass(context.Background(), store.tasks["t1"])
	// pre-pass 后 notice 应被消化（tk1 reap 成功，会话不存在 → 项清除）。
	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	if len(entries) != 0 {
		t.Errorf("pre-pass should digest retryable debt without self-deadlock; remaining=%d", len(entries))
	}
}

// TestPrePass_HasDebtDoesNotResumeActive 验证 F1：pre-pass 后仍有 debt 的任务 MUST NOT 恢复 active。
func TestPrePass_HasDebtDoesNotResumeActive(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 注入不可消化的 debt：tickets reap 返回非空（remaining）。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	// reap 永远失败（remaining 非空）。
	proc2 := &reapFailProc{mockProc: proc, left: []string{"tk1"}}
	m := newTestManager(t, store, proc2, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (debt blocks resume after pre-pass)", row.Status)
	}
}

// reapFailProc mockProc 变体：RetryReap 总返回 remaining 非空（debt 不可消化）。
type reapFailProc struct {
	*mockProc
	left []string
}

func (p *reapFailProc) RetryReap(tickets []string) ([]string, error) { return p.left, nil }

// TestPersistResume_RestoresShellWatchersGroups 验证 F2：persist 恢复时补注册 TUI + shell
// RuntimeGroup 与 watchers（此前仅注册 serve）。
func TestPersistResume_RestoresShellWatchersGroups(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	// 设置 env snapshot（resumeActive 需 loadEnvSnapshot 成功）。
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })

	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status=%s want active (persist resume)", row.Status)
	}
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime should be registered after persist resume")
	}
	// serve/tui/shell group 均应注册。
	if _, ok := rt.groups[serveSessionName("t1")]; !ok {
		t.Error("serve group should be registered")
	}
	if _, ok := rt.groups[tuiSessionName("t1")]; !ok {
		t.Error("tui group should be registered (F2: persist restores TUI watchers+groups)")
	}
	if _, ok := rt.groups[shellSessionName("t1", 1)]; !ok {
		t.Error("shell group should be registered (F2: persist restores shell watchers+groups)")
	}
}

// TestOrphanTicketsAggregated 验证 F3：孤儿会话 kill 失败时聚合 cleanupTickets（不止 session names）
// 进后台重试队列。
func TestOrphanTicketsAggregated(t *testing.T) {
	store := newMockStore() // 无 DB 任务 → 孤儿
	proc := newMockProc()
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
		CleanupTickets: []string{"ticket-A", "ticket-B"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	_ = m.Reconcile(context.Background())
	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanMu.Unlock()
	if len(failures) != 1 {
		t.Fatalf("expected 1 orphan failure queued, got %d", len(failures))
	}
	if failures[0].sessionName != "ocdeck-ghost-serve" {
		t.Errorf("sessionName=%s want ocdeck-ghost-serve", failures[0].sessionName)
	}
	// tickets 应被聚合（F3：不止 session names）。
	if len(failures[0].tickets) != 2 {
		t.Errorf("expected 2 aggregated tickets, got %d", len(failures[0].tickets))
	}
}

// TestActivating_ServeResidualKilled 验证 F4：activating 状态 serve 已死时
// MUST kill 残留 serve 会话（此前仅落 suspended 不清进程）。
func TestActivating_ServeResidualKilled(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	// serve 不存在，但 tui 残留（activating 崩溃窗口）。
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (activating serve dead)", row.Status)
	}
	// 残留 tui/shell 会话 MUST 被 kill。
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("residual tui session should be killed (F4: full runtime cleanup)")
	}
	if proc.sessions[shellSessionName("t1", 1)] {
		t.Error("residual shell session should be killed (F4: full runtime cleanup)")
	}
}

// TestActiveServeDead_FullRuntimeCleanup 验证 F4：active serve 已死 → 完整运行时清理
//（watcher/SSE 收敛）后落 suspended。
func TestActiveServeDead_FullRuntimeCleanup(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// serve 不存在，tui 残留。
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended", row.Status)
	}
	// 运行时应已收敛（无残留 runtime）。
	if m.getRuntime("t1") != nil {
		t.Error("runtime should be cleared (full runtime cleanup, watcher/SSE converged)")
	}
	// tui 会话应被 kill。
	if proc.sessions[tuiSessionName("t1")] {
		t.Error("residual tui session should be killed")
	}
}

// encodeEnvSnapshot 编码 env snapshot 为 sql.NullString（测试辅助）。
func encodeEnvSnapshot(snap envSnapshot) (sql.NullString, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// _ 保持 sync 引用（部分测试用 goroutine + sleep）。
var _ = sync.Mutex{}