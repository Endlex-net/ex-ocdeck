package task

import (
	"context"
	"errors"
	"testing"

	"ocdeck/internal/config"
)

// optRecordingProc 记录 EnsureServerOptions 调用次数与返回错误
// （嵌入 *mockProc 避免复制 sync.Mutex，模式同 ptrListErrProc）。
type optRecordingProc struct {
	*mockProc
	calls int
	err   error
}

func (p *optRecordingProc) EnsureServerOptions() error {
	p.calls++
	return p.err
}

func newOptRecordingManager(t *testing.T, proc *optRecordingProc) *Manager {
	t.Helper()
	m := newTestManager(t, newMockStore(), proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist
	return m
}

// TestReconcile_EnsureServerOptions_Gate 验证对账路径的剪贴板选项门控：
// 无会话（无 server）不调用，避免 set-option 拉起空 server；有会话时重设；
// 失败仅记日志，不阻断 reconciliation。
func TestReconcile_EnsureServerOptions_Gate(t *testing.T) {
	t.Run("empty sessions not called", func(t *testing.T) {
		proc := &optRecordingProc{mockProc: newMockProc()}
		m := newOptRecordingManager(t, proc)
		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if proc.calls != 0 {
			t.Errorf("EnsureServerOptions calls = %d, want 0 (no sessions → no server)", proc.calls)
		}
	})

	t.Run("called when sessions exist", func(t *testing.T) {
		proc := &optRecordingProc{mockProc: newMockProc()}
		proc.sessions["ocdeck-ghost-serve"] = true
		m := newOptRecordingManager(t, proc)
		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if proc.calls != 1 {
			t.Errorf("EnsureServerOptions calls = %d, want 1", proc.calls)
		}
	})

	t.Run("failure does not block reconcile", func(t *testing.T) {
		proc := &optRecordingProc{mockProc: newMockProc(), err: errors.New("set-option boom")}
		proc.sessions["ocdeck-ghost-serve"] = true
		m := newOptRecordingManager(t, proc)
		if err := m.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile must not fail on EnsureServerOptions error: %v", err)
		}
		if proc.calls != 1 {
			t.Errorf("EnsureServerOptions calls = %d, want 1", proc.calls)
		}
	})
}
