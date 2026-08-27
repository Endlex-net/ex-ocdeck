package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
)

// newTestManagerWithLifecycle 构造 Manager 并注入可取消 lifecycle ctx（供自动激活测试，
// 避免挂 context.Background() 致 SSE goroutine 泄漏）。返回 manager 与 cancel：
// cancel 取消 lifecycle ctx 使自动激活派生的 SSE/退出监视 goroutine 退出。
func newTestManagerWithLifecycle(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) (*Manager, context.CancelFunc) {
	t.Helper()
	lifeCtx, cancel := context.WithCancel(context.Background())
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.SetLifecycleCtx(lifeCtx)
	t.Cleanup(cancel)
	return m, cancel
}

// waitForStatus 轮询任务状态直到达到 want 或超时。
func waitForStatus(t *testing.T, store TaskStore, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		if row.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("status = %s, want %s (timed out)", row.Status, want)
}

// TestCreate_AutoActivateTriggered 验证 design.md §19 Create 行 ④：
// Create 成功提交 suspended 后异步触发 Activate，状态推进至 active，
// serve/tui NewSession 调用发生（走手动 Activate 全部步骤）。
func TestCreate_AutoActivateTriggered(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)

	row, err := m.Create(context.Background(), "p1", "Auto Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended {
		t.Fatalf("Create returned status = %s, want suspended", row.Status)
	}

	// 自动激活异步推进至 active。
	waitForStatus(t, store, row.ID, StatusActive, 3*time.Second)

	// serve 与 tui 会话 MUST 已创建（走 Activate 全部步骤）。
	proc.mu.Lock()
	hasRuntime := proc.sessions[runtimeSessionName(row.ID)]
	hasTUI := proc.sessions[tuiSessionName(row.ID)]
	proc.mu.Unlock()
	if !hasRuntime {
		t.Error("runtime session not created by auto-activate")
	}
	if hasTUI {
		t.Error("tui session must not be created by auto-activate")
	}
}

// waitForLastError 轮询任务 last_error 直到非空或超时（用于自动激活失败断言：
// Create 返回时即为 suspended，需等激活尝试失败写入 last_error 才能区分）。
func waitForLastError(t *testing.T, store TaskStore, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		if row.LastError.Valid && row.LastError.String != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("last_error still empty for %s (status=%s)", taskID, row.Status)
}

// TestCreate_AutoActivateFailureFallsToSuspended 验证 design.md §19：
// 自动激活失败 → 任务落 suspended + last_error（可手动重试）。
func TestCreate_AutoActivateFailureFallsToSuspended(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newMockOC(true)
	// Probe 失败使 Activate 在能力探测步骤失败 → 落 suspended+last_error。
	oc.probeErr = errors.New("capability mismatch boom")
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)

	row, err := m.Create(context.Background(), "p1", "Fail Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Create 返回 suspended（激活尚未完成），等待自动激活失败写入 last_error。
	waitForLastError(t, store, row.ID, 3*time.Second)
	gotRow, _ := store.GetTask(context.Background(), row.ID)
	if gotRow.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended after auto-activate failure", gotRow.Status)
	}
}

// TestRetryCreationFailed_AutoActivateTriggered 验证 design.md §19：
// Retry(creation_failed) 成功补建后同样自动触发激活（语义一致：创建完成即激活）。
func TestRetryCreationFailed_AutoActivateTriggered(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	t1 := TaskRow{ID: "t1", ProjectID: "p1", Name: "task", Branch: "ocdeck/task",
		Status: StatusCreationFailed, WorktreePath: "/data/worktrees/p1/t1", BaseRef: "refs/heads/main"}
	store.tasks["t1"] = t1
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)

	if err := m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	// retryCreate 提交 suspended 后异步触发激活 → 推进至 active。
	waitForStatus(t, store, "t1", StatusActive, 3*time.Second)
	proc.mu.Lock()
	hasRuntime := proc.sessions[runtimeSessionName("t1")]
	proc.mu.Unlock()
	if !hasRuntime {
		t.Error("runtime session not created by auto-activate after retry create")
	}
}

// TestCreate_AutoActivateUsesLifecycleCtxNotRequestCtx 验证 design.md §19：
// 自动激活挂 Manager lifecycle ctx，不随 Create 请求 ctx 取消而中断。
// Create 请求 ctx 取消后，自动激活仍应推进至 active。
func TestCreate_AutoActivateUsesLifecycleCtxNotRequestCtx(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	wt := newMockWorktree()
	proc := newMockProc()
	oc := newMockOC(true)
	m, _ := newTestManagerWithLifecycle(t, store, proc, wt, oc)

	// Create 用可取消请求 ctx，Create 返回后立即取消请求 ctx。
	reqCtx, reqCancel := context.WithCancel(context.Background())
	row, err := m.Create(reqCtx, "p1", "Ctx Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reqCancel() // 取消请求 ctx，自动激活 MUST 不受影响

	// 自动激活仍推进至 active（挂在独立的 lifecycle ctx）。
	waitForStatus(t, store, row.ID, StatusActive, 3*time.Second)
}
