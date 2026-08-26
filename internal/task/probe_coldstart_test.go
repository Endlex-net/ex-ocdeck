package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// probeSequenceOC 装饰 mockOC，使 Probe 按预设序列返回，逐次消费。
// 用于 D8 冷启动重试测试：首次 ErrServeNotReady、第二次成功 等。
type probeSequenceOC struct {
	*mockOC
	mu    sync.Mutex
	seq   []error // 每次 Probe 返回的错误（nil 表示成功）
	calls int
}

func newProbeSequenceOC(healthOK bool, seq ...error) *probeSequenceOC {
	return &probeSequenceOC{mockOC: newMockOC(healthOK), seq: seq}
}

func (c *probeSequenceOC) Probe(ctx context.Context) (string, error) {
	c.mu.Lock()
	idx := c.calls
	c.calls++
	if idx >= len(c.seq) {
		// 超出序列：默认成功（避免无限失败拖慢断言）。
		c.mu.Unlock()
		return opencode.ContractBaseline, nil
	}
	err := c.seq[idx]
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	return opencode.ContractBaseline, nil
}

func (c *probeSequenceOC) probeCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// shortBackoff 返回 ms 级退避序列，避免拖慢测试。
func shortBackoff() []time.Duration {
	return []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
}

// newTestManagerWithShortProbeBackoff 构造注入短退避的 Manager（Activate 级测试用）。
func newTestManagerWithShortProbeBackoff(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) *Manager {
	t.Helper()
	m := newTestManager(t, store, proc, wt, oc)
	m.probeColdStartBackoffFn = shortBackoff
	return m
}

// TestActivate_ProbeColdStartRetryThenSuccess 验证 D8：Probe 首次返回 ErrServeNotReady、
// 第二次成功 → 激活继续推进，serve 会话未被 kill。
func TestActivate_ProbeColdStartRetryThenSuccess(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newProbeSequenceOC(true, opencode.ErrServeNotReady, nil)
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := oc.probeCalls(); got != 2 {
		t.Errorf("Probe calls = %d, want 2", got)
	}
	if !proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should NOT be killed during cold-start retry")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active (probe succeeded on retry)", row.Status)
	}
}

// TestActivate_ProbeColdStartExhaustedFailsAndKillsSession 验证 D8：3 次均 ErrServeNotReady
// → kill 会话 + 落 suspended + last_error。
func TestActivate_ProbeColdStartExhaustedFailsAndKillsSession(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newProbeSequenceOC(true,
		opencode.ErrServeNotReady,
		opencode.ErrServeNotReady,
		opencode.ErrServeNotReady,
	)
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error after 3 cold-start probe failures")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code = %s, want process_error", OpErrorCode(err))
	}
	if got := oc.probeCalls(); got != probeColdStartAttempts {
		t.Errorf("Probe calls = %d, want %d", got, probeColdStartAttempts)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should be killed after cold-start retry exhausted")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended", row.Status)
	}
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Error("last_error should be recorded on cold-start retry exhaustion")
	}
}

// TestActivate_ProbeCapabilityMismatchNoRetry 验证非 ErrServeNotReady 立即失败、不重试：
// Probe 仅被调用 1 次。
func TestActivate_ProbeCapabilityMismatchNoRetry(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newProbeSequenceOC(true, opencode.ErrCapabilityMismatch)
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error on capability mismatch")
	}
	if OpErrorCode(err) != codeOCIncompatible {
		t.Errorf("code = %s, want oc_incompatible", OpErrorCode(err))
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1 (no retry on mismatch)", got)
	}
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should be killed on capability mismatch")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended", row.Status)
	}
}

// TestActivate_ProbeUnauthorizedNoRetry 验证 ErrUnauthorized 立即失败、不重试。
func TestActivate_ProbeUnauthorizedNoRetry(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newProbeSequenceOC(true, opencode.ErrUnauthorized)
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error on unauthorized")
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("code = %s, want internal", OpErrorCode(err))
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1 (no retry on unauthorized)", got)
	}
}

// TestActivate_ProbeUnknownErrorNoRetry 验证非 sentinel 的未知错误立即失败、不重试。
func TestActivate_ProbeUnknownErrorNoRetry(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	oc := newProbeSequenceOC(true, errors.New("totally unknown boom"))
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error on unknown probe error")
	}
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code = %s, want process_error (unknown defaults to process_error)", OpErrorCode(err))
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1 (no retry on unknown)", got)
	}
}

// TestActivate_ProbeWrappedErrServeNotReadyRetries 验证 fmt.Errorf %w 包一层的
// ErrServeNotReady 仍触发重试（errors.Is 兼容 wrap）。
func TestActivate_ProbeWrappedErrServeNotReadyRetries(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wrapped := fmt.Errorf("upstream: %w", opencode.ErrServeNotReady)
	oc := newProbeSequenceOC(true, wrapped, nil)
	m := newTestManagerWithShortProbeBackoff(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := oc.probeCalls(); got != 2 {
		t.Errorf("Probe calls = %d, want 2 (wrapped ErrServeNotReady should retry)", got)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active", row.Status)
	}
}

// TestRunProbeColdStartRetry_RespectsContextCancel 验证退避期间 ctx 取消立即返回。
func TestRunProbeColdStartRetry_RespectsContextCancel(t *testing.T) {
	oc := newProbeSequenceOC(true, opencode.ErrServeNotReady)
	// 用较长退避确保 cancel 在 sleep 期间触发。
	backoff := []time.Duration{1 * time.Second, 1 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := runProbeColdStartRetry(ctx, oc, backoff)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1 (cancel before retry)", got)
	}
}

// TestRunProbeColdStartRetry_SuccessOnFirstCall 验证首次成功不退避。
func TestRunProbeColdStartRetry_SuccessOnFirstCall(t *testing.T) {
	oc := newProbeSequenceOC(true, nil)
	start := time.Now()
	if err := runProbeColdStartRetry(context.Background(), oc, shortBackoff()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("should not sleep on first-call success, elapsed = %v", elapsed)
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1", got)
	}
}

// TestRunProbeColdStartRetry_UnknownErrorNoRetry 验证 runProbeColdStartRetry 对未知错误不重试。
func TestRunProbeColdStartRetry_UnknownErrorNoRetry(t *testing.T) {
	oc := newProbeSequenceOC(true, errors.New("boom"))
	err := runProbeColdStartRetry(context.Background(), oc, shortBackoff())
	if err == nil {
		t.Fatal("expected error")
	}
	if got := oc.probeCalls(); got != 1 {
		t.Errorf("Probe calls = %d, want 1", got)
	}
}

// TestRunProbeColdStartRetry_ExhaustedMsgIncludesAttempts 验证 3 次耗尽后错误信息包含尝试次数。
func TestRunProbeColdStartRetry_ExhaustedMsgIncludesAttempts(t *testing.T) {
	oc := newProbeSequenceOC(true,
		opencode.ErrServeNotReady,
		opencode.ErrServeNotReady,
		opencode.ErrServeNotReady,
	)
	err := runProbeColdStartRetry(context.Background(), oc, shortBackoff())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, opencode.ErrServeNotReady) {
		t.Errorf("err should wrap ErrServeNotReady, got %v", err)
	}
	want := fmt.Sprintf("after %d attempts", probeColdStartAttempts)
	if !contains(err.Error(), want) {
		t.Errorf("err = %q, want contains %q", err.Error(), want)
	}
	if got := oc.probeCalls(); got != probeColdStartAttempts {
		t.Errorf("Probe calls = %d, want %d", got, probeColdStartAttempts)
	}
}

// TestDefaultProbeColdStartBackoffSequence 验证默认退避序列为 2s、4s（精确断言）。
func TestDefaultProbeColdStartBackoffSequence(t *testing.T) {
	got := defaultProbeColdStartBackoff()
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if len(got) != probeColdStartAttempts-1 {
		t.Errorf("backoff len = %d, want %d (attempts-1)", len(got), probeColdStartAttempts-1)
	}
}