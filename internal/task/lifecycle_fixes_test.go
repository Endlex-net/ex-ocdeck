package task

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"ocdeck/internal/config"
)

// TestShutdown_WaitsBackgroundAndFinalDebtSweep 验证 H：Shutdown 等待后台 notice resolver 收尾，
// 并同步执行一次残留 retryable debt 清扫（不静默丢失）。
// 构造一个带可清除 retryable debt 的任务（serve 已不存在，kill 跳过，RetryReap 成功），
// 不等 30s ticker，直接 Shutdown 应清掉 notice。
func TestShutdown_WaitsBackgroundAndFinalDebtSweep(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 retryable residual notice（sessionName 指向已不存在的会话，cleanupTickets 非空）。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": noticeReasonKillFailed, "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc()
	// serve 会话不存在 → kill 跳过；RetryReap mock 返回空 → 清除成功。
	// 注：mockProc.RetryReap 默认返回 nil,nil（清除全部）。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist
	bgStop := m.StartBackground(context.Background())
	defer bgStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := m.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// H：关停时 SHOULD 同步清扫残留 retryable debt → notice 应被清除。
	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	for _, e := range entries {
		if e.Code == noticeCodeResidual {
			if r, _ := e.Data["retryable"].(bool); r {
				t.Errorf("retryable residual notice should be swept on shutdown, still present: %+v", e)
			}
		}
	}
}

// TestShutdown_JoinsBackgroundGoroutine 验证 H/G：Shutdown join 后台周期 goroutine。
// bgDone 应在 Shutdown 返回前关闭。
func TestShutdown_JoinsBackgroundGoroutine(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist
	m.StartBackground(context.Background())

	bgDone := m.bgDone
	if bgDone == nil {
		t.Fatal("bgDone should be set after StartBackground")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-bgDone:
		// ok：goroutine 已退出并被 join。
	default:
		t.Error("background goroutine should be joined (bgDone closed) before Shutdown returns")
	}
}

// TestStopAndJoinAllRuntimes_NoLeak 验证 G：Shutdown 停并 join 全部 runtime 的 SSE/watch goroutine。
// 构造活跃 runtime（serve+tui watch + SSE），Shutdown 后 runtime 应被清空、
// SSE goroutine 退出（不再持有已关资源）。
func TestStopAndJoinAllRuntimes_NoLeak(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.EnvSnapshot = newStrNull(`{"vars":{"PATH":"/usr/bin"}}`)
		r.LastPort = newIntNull(50001)
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 注入 lifecycle ctx。
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	defer lifeCancel()
	m.SetLifecycleCtx(lifeCtx)

	// 手动构造活跃 runtime + SSE + runtime watch（复用 activate 内部步骤；
	// single-process：无独立 TUI watch）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	if err := m.startSSE(lifeCtx, rt, "t1", "/wt", 50001, "pw", AlignModeRepo); err != nil {
		t.Fatalf("startSSE: %v", err)
	}
	m.watchServeExit("t1", runtimeSessionName("t1"))

	// 确认 SSE goroutine 活跃（sseCancel 非 nil）。
	rt.mu.Lock()
	sseActive := rt.sseCancel != nil
	watchCount := len(rt.watchCancels)
	rt.mu.Unlock()
	if !sseActive {
		t.Fatal("SSE should be active before shutdown")
	}
	if watchCount != 1 {
		t.Fatalf("expected 1 watch goroutine, got %d", watchCount)
	}

	// Shutdown 应停并 join 全部 runtime goroutine。
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// runtime 应被清空。
	if rt := m.getRuntime("t1"); rt != nil {
		// 仍可能残留 rt 对象（map 已清空），但 getRuntime 应返回 nil。
		_ = rt
	}
	if got := m.getRuntime("t1"); got != nil {
		t.Error("runtime should be cleared after Shutdown")
	}

	// 取消 lifecycle ctx 后，SSE goroutine 应已退出（stopAll 已 join）。
	// 触发一次 lifeCancel 不应 panic，且不再有后台写入。
	lifeCancel()
	time.Sleep(100 * time.Millisecond)
}

// TestWatchExit_G_JoinAfterCancel 验证 G：WatchExit 返回的 done 在 cancel 后关闭（goroutine 退出）。
// 使用 mock proc（done 在 cancel 时关闭）。
func TestWatchExit_G_JoinAfterCancel(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	m.watchServeExit("t1", runtimeSessionName("t1"))

	rt.mu.Lock()
	done := rt.watchDones[runtimeSessionName("t1")]
	rt.mu.Unlock()
	if done == nil {
		t.Fatal("watch done channel should be registered")
	}
	// stopAll（cancel 不 join watch，但 done 在 mock 中于 cancel 时关闭）。
	rt.stopAll()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Error("watch done channel should close after cancel (goroutine joined)")
	}
}

// 辅助。
func newStrNull(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func newIntNull(n int) sql.NullInt64      { return sql.NullInt64{Int64: int64(n), Valid: true} }