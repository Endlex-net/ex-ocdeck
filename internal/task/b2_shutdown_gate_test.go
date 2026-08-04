package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/config"
)

// blockingProbeOC 包装 mockOC，使 Probe 阻塞直到 release 信号，用于 B2 测试
// 观察自动激活 goroutine 在途时 Shutdown 的 gate+join 行为。
// probeEntered 在 Probe 进入阻塞前自增（断言 in-flight 已到达）。
type blockingProbeOC struct {
	*mockOC
	probeCh      chan struct{}
	probeEntered atomic.Int32
}

func newBlockingProbeOC() *blockingProbeOC {
	return &blockingProbeOC{mockOC: newMockOC(true), probeCh: make(chan struct{})}
}

func (c *blockingProbeOC) Probe(ctx context.Context) (string, error) {
	c.probeEntered.Add(1)
	select {
	case <-c.probeCh:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return c.mockOC.Probe(ctx)
}

// TestB2_ShutdownGate_JoinsInFlightAutoActivate 验证 B2：
// - Shutdown 开始后 admission gate 拒绝新自动激活触发；
// - 已登记的 triggerActivate goroutine 被 join（有界超时等待收尾）后再执行清理。
// 消灭窗口：kill 模式 shutdown 枚举后再建 tmux 会话。
func TestB2_ShutdownGate_JoinsInFlightAutoActivate(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newBlockingProbeOC()
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	// Create 返回 suspended 后立即触发自动激活 goroutine（阻塞在 Probe）。
	row, err := m.Create(context.Background(), "p1", "Gate Task")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended", row.Status)
	}

	// 等待自动激活 goroutine 进入 Probe 阻塞（in-flight，已登记 autoActivateWG）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if oc.probeEntered.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if oc.probeEntered.Load() == 0 {
		t.Fatal("auto-activate goroutine did not enter Probe in time")
	}

	// 此时自动激活 in-flight（Probe 阻塞）。调用 Shutdown。
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- m.Shutdown(ctx)
	}()

	// 给 Shutdown 一点时间设置 gate 并开始等待 WG。
	time.Sleep(50 * time.Millisecond)

	// 验证 gate 已开始：触发新的自动激活 MUST 被拒绝。
	// 被拒绝时 triggerActivate 仅记日志、不派发 goroutine；通过 CreateSession 计数间接验证。
	createBefore := oc.createSessionCountLoad()
	m.triggerActivate(row.ID)
	// 给可能（错误）派发的 goroutine 一点时间；gate 拒绝时不应有新 Probe/CreateSession。
	time.Sleep(30 * time.Millisecond)
	if got := oc.createSessionCountLoad(); got != createBefore {
		t.Errorf("gate did not reject new auto-activate after shutdown start: createSession %d -> %d",
			createBefore, got)
	}

	// 释放 Probe 让 in-flight 自动激活完成 → Activate 推进 → WG Done → Shutdown join 成功。
	close(oc.probeCh)

	select {
	case <-shutdownDone:
		// Shutdown join 成功（in-flight 自动激活收尾后继续清理）。
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after releasing in-flight auto-activate (join failed)")
	}
}

// TestB2_TriggerActivate_AfterShutdownStarted_Rejected 验证 B2：
// shutdownStarted=true 后 triggerActivate MUST 不派发 goroutine（任务保持 suspended）。
func TestB2_TriggerActivate_AfterShutdownStarted_Rejected(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	oc := newBlockingProbeOC()
	m, _ := newTestManagerWithLifecycle(t, store, newMockProc(), newMockWorktree(), oc)

	// 模拟 Shutdown 已开始（仅设 gate，不跑完整 Shutdown 以隔离断言）。
	m.shutdownGateMu.Lock()
	m.shutdownStarted = true
	m.shutdownGateMu.Unlock()

	createBefore := oc.probeEntered.Load()
	m.triggerActivate("t-skip")
	// 给可能（错误）派发的 goroutine 一点时间；gate 拒绝时不应进入 Probe。
	time.Sleep(30 * time.Millisecond)
	if got := oc.probeEntered.Load(); got != createBefore {
		t.Errorf("triggerActivate after shutdown started dispatched goroutine: probeEntered %d -> %d",
			createBefore, got)
	}
}

// TestB2_ShutdownTimeoutOnStuckAutoActivate 验证 B2：Shutdown 有界超时——
// in-flight 自动激活不收尾时，Shutdown 超时后仍继续清理（不无限阻塞）。
func TestB2_ShutdownTimeoutOnStuckAutoActivate(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newBlockingProbeOC()
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if _, err := m.Create(context.Background(), "p1", "Stuck Task"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 等待自动激活 in-flight（阻塞在 Probe）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if oc.probeEntered.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if oc.probeEntered.Load() == 0 {
		t.Fatal("auto-activate did not enter in-flight")
	}

	// Shutdown 用极短超时：in-flight 未收尾 → MUST 超时但不无限阻塞。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = m.Shutdown(ctx)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Shutdown blocked %v, expected bounded timeout", elapsed)
	}
	// 释放 Probe 清理 in-flight，避免 goroutine 泄漏（lifecycle ctx 取消也会收尾）。
	close(oc.probeCh)
}
