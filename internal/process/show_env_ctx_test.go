package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestShowSessionEnvContext_ValidationDelegation 验证 ShowSessionEnvContext
// 复用 ShowSessionEnv 的 name/key 校验（cross-project-active-sessions D0：语义一致）。
// 校验失败时 MUST 不执行 tmux 命令，直接返回校验错误。
func TestShowSessionEnvContext_ValidationDelegation(t *testing.T) {
	m := &Manager{}
	ctx := context.Background()

	cases := []struct {
		name string
		key  string
	}{
		{"bad name", "FOO"},      // 非 ocdeck-<taskID>-<role>
		{"ocdeck-t1-serve", ""},  // key 非法
		{"ocdeck-t1-serve", "1K"}, // key 数字开头
	}
	for _, c := range cases {
		_, err := m.ShowSessionEnvContext(ctx, c.name, c.key)
		if err == nil {
			t.Errorf("ShowSessionEnvContext(%q,%q) err=nil, want validation error", c.name, c.key)
		}
	}
}

// TestShowSessionEnvContext_CallerShorterDeadlineWins 验证调用方更短的 deadline
// 优先生效（cross-project-active-sessions D0：内部 5s 封顶但调用方 50ms 先到期）。
// 用 execTmuxFn 探测注入：阻塞直到 ctx 被取消（50ms 后），断言在 1s 内返回且 ctx.Err != nil。
func TestShowSessionEnvContext_CallerShorterDeadlineWins(t *testing.T) {
	m := &Manager{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	m.execTmuxFn = func(c context.Context, args ...string) (string, string, error) {
		<-c.Done() // 等待 50ms deadline 或内部 5s cap，取较短者
		return "", "", ctx.Err()
	}

	start := time.Now()
	_, err := m.ShowSessionEnvContext(ctx, "ocdeck-t1-serve", "OCDECK_SERVE_PORT")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err = nil, want ctx deadline error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, want <1s (caller 50ms deadline must win over internal 5s cap)", elapsed)
	}
}

// TestShowSessionEnvContext_Internal5sCap 验证无 deadline 的调用方 ctx
// 被内部 5s 封顶：传给 execTmux 的 ctx 必带 deadline 且 ≤5s（D0）。
// 用 execTmuxFn 探测注入：检查接收到的 ctx deadline，立即返回，避免 5s 墙钟。
func TestShowSessionEnvContext_Internal5sCap(t *testing.T) {
	m := &Manager{}
	var receivedDeadline time.Time
	m.execTmuxFn = func(c context.Context, args ...string) (string, string, error) {
		receivedDeadline, _ = c.Deadline()
		return "", "", nil
	}

	// 调用方 ctx 无 deadline → 内部 WithTimeout(5s) 必须赋予一个 ≤5s 后的 deadline。
	_, _ = m.ShowSessionEnvContext(context.Background(), "ocdeck-t1-serve", "OCDECK_SERVE_PORT")

	if receivedDeadline.IsZero() {
		t.Fatal("execTmux received ctx with no deadline; want internal 5s cap")
	}
	cap := time.Until(receivedDeadline)
	if cap > 5*time.Second {
		t.Errorf("internal cap = %v, want ≤ 5s (deadline-less caller must keep old 5s protection)", cap)
	}
	if cap <= 0 {
		t.Errorf("internal cap = %v, want positive (deadline already expired)", cap)
	}
}

// TestShowSessionEnvContext_PreCancelledCtxReturnsFast 验证 ctx 已取消时
// ShowSessionEnvContext 不阻塞（cross-project-active-sessions D0：ctx-aware 聚合，
// 取消的 ctx 必须缩短 hydration）。execTmux 在 ctx 已取消时立即失败，MUST 在 1s 内返回。
func TestShowSessionEnvContext_PreCancelledCtxReturnsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux-touching test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := m.ShowSessionEnvContext(ctx, "ocdeck-none-serve", "OCDECK_SERVE_PORT")
		done <- err
	}()
	select {
	case <-done:
		// ok: cancelled ctx shortened the call
	case <-time.After(1 * time.Second):
		t.Fatal("ShowSessionEnvContext hung >1s on cancelled ctx")
	}
}

// TestShowSessionEnvContext_NoServerMapsErrNoTmuxServer 验证无 server 时
// ShowSessionEnvContext 映射 ErrNoTmuxServer（与 ShowSessionEnv 语义一致）。
func TestShowSessionEnvContext_NoServerMapsErrNoTmuxServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	_, err := m.ShowSessionEnvContext(context.Background(), "ocdeck-none-serve", "OCDECK_SERVE_PORT")
	if !errors.Is(err, ErrNoTmuxServer) {
		t.Fatalf("err = %v, want ErrNoTmuxServer", err)
	}
}

// TestShowSessionEnv_WrapperRegression 验证 ShowSessionEnv 仍为 5s 封装，
// 无 server 时同样映射 ErrNoTmuxServer（D0：外部行为不变）。
func TestShowSessionEnv_WrapperRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	_, err := m.ShowSessionEnv("ocdeck-none-serve", "OCDECK_SERVE_PORT")
	if !errors.Is(err, ErrNoTmuxServer) {
		t.Fatalf("ShowSessionEnv err = %v, want ErrNoTmuxServer", err)
	}
}