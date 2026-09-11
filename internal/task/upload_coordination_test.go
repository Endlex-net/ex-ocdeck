package task

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// recordingCoordination 记录 Acquire 调用的协调锁 fake（upload_coordination 2.5）。
type recordingCoordination struct {
	mu        sync.Mutex
	acquired  []string
	onAcquire func(taskID string)
}

func (c *recordingCoordination) Acquire(ctx context.Context, taskID string) (func(), error) {
	c.mu.Lock()
	c.acquired = append(c.acquired, taskID)
	c.mu.Unlock()
	if c.onAcquire != nil {
		c.onAcquire(taskID)
	}
	return func() {}, nil
}

func (c *recordingCoordination) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.acquired...)
}

// failingCoordination Acquire 恒失败（验证 Acquire 失败时 commit 不执行）。
type failingCoordination struct{ err error }

func (f failingCoordination) Acquire(ctx context.Context, taskID string) (func(), error) {
	return nil, f.err
}

// TestSuspend_UsesUploadCoordination_LockOrder（terminal-file-paste-drop 2.5）：
// Suspend 的 active→suspending 提交经 per-task 协调锁，且协调锁临界区内 Manager
// 任务锁已被持有（锁顺序：任务锁 → 协调锁）。
func TestSuspend_UsesUploadCoordination_LockOrder(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	// branch a：serve 已死，tui 存在（将被 kill）。
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	rec := &recordingCoordination{}
	// 协调锁临界区内同步 tryLockTask：必须 409（任务锁先于协调锁持有）。
	// 同步调用（TryLock 不阻塞）保证判定发生在 Acquire 返回前，与 Suspend 持锁
	// 窗口无竞态——另起 goroutine 可能在 Suspend 释放任务锁后才执行 TryLock。
	lockHeld := make(chan bool, 1)
	rec.onAcquire = func(taskID string) {
		unlock, err := m.tryLockTask(taskID)
		if err == nil {
			unlock()
		}
		lockHeld <- err != nil
	}
	m.uploadCoordination = rec

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if got := rec.calls(); len(got) != 1 || got[0] != "t1" {
		t.Fatalf("coordination acquired = %v, want [t1]", got)
	}
	if held := <-lockHeld; !held {
		t.Fatal("task lock must be held during coordination critical section (got TryLock success)")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (commit executed inside coordination)", row.Status)
	}
}

// TestDelete_UsesUploadCoordination（2.5）：Delete 的意图提交与删行提交均经协调锁。
func TestDelete_UsesUploadCoordination(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	rec := &recordingCoordination{}
	m.uploadCoordination = rec

	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row should be deleted")
	}
	// writeBeginDeleteIntent + writeDeleteTask 两次提交均入协调锁。
	if got := rec.calls(); len(got) != 2 {
		t.Fatalf("coordination acquired = %v, want 2 calls (intent + delete row)", got)
	}
}

// TestSuspend_CoordinationAcquireFails_CommitNotExecuted（2.5）：
// 协调锁 Acquire 失败 → 提交不执行、Suspend 以 internal 报错返回。
func TestSuspend_CoordinationAcquireFails_CommitNotExecuted(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) { t.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.uploadCoordination = failingCoordination{err: errors.New("coordination unavailable")}

	err := m.Suspend(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeInternal {
		t.Fatalf("err = %v, want internal code", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Errorf("status = %s, want active (commit must not execute when Acquire fails)", row.Status)
	}
}
