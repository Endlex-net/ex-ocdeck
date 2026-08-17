package task

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/process"
)

// TestRecordResidualNotice_ReadbackConverge 验证 recordResidualNotice CAS 写回后读回校验收敛（A）。
// 正常路径：写入后 notice 落库且可读回。
func TestRecordResidualNotice_ReadbackConverge(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if err := m.recordResidualNotice(context.Background(), "t1", "ocdeck-t1-serve", []string{"tk"}, noticeReasonKillFailed, true); err != nil {
		t.Fatalf("recordResidualNotice: %v", err)
	}

	row, _ := store.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notices: %v", perr)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d notices, want 1", len(entries))
	}
	if entries[0].Code != noticeCodeResidual {
		t.Errorf("code=%s want %s", entries[0].Code, noticeCodeResidual)
	}
	if got, _ := entries[0].Data["retryable"].(bool); !got {
		t.Error("retryable should be true")
	}
}

// TestRecordSessionOverflowNotice_IdempotentReadback 验证 overflow notice 幂等写入并读回收敛（A）。
func TestRecordSessionOverflowNotice_IdempotentReadback(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	m.recordSessionOverflowNotice(context.Background(), "t1")
	m.recordSessionOverflowNotice(context.Background(), "t1") // 幂等：不应重复追加

	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	count := 0
	for _, e := range entries {
		if e.Code == noticeCodeSessionOverflow {
			count++
		}
	}
	if count != 1 {
		t.Errorf("overflow notice count=%d want 1 (idempotent)", count)
	}
}

// casFailingStore 包装 mockStore，使 UpdateTaskNoticeCAS 永远返回 replaced=false，
// 模拟持续 CAS 竞争。其他方法透传。
type casFailingStore struct {
	*mockStore
	casCalls int
	mu       sync.Mutex
}

func (s *casFailingStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	s.casCalls++
	s.mu.Unlock()
	return application.MutationResult{}, nil
}

// TestRetryTaskNotices_CASExhaustionAggregatesError 验证 CAS 循环耗尽时 retryTaskNotices
// 聚合返回错误，不静默 return（A：末尾 CAS 失败 MUST 聚合返回）。
func TestRetryTaskNotices_CASExhaustionAggregatesError(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 retryable residual notice。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })

	// 用 wrapper 让 CAS 永远失败。
	inner := store
	failing := &casFailingStore{mockStore: inner}
	// 重新构造 manager 指向 failing store（通过直接替换字段不可行，构造新 manager）。
	m := newTestManager(t, failing, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.retryTaskNotices(context.Background(), TaskRow{ID: "t1", Notice: encodeNotices(notice)}, notice)
	if err == nil {
		t.Fatal("expected aggregated error when CAS never converges, got nil")
	}
	if !strings.Contains(err.Error(), "CAS") && !strings.Contains(err.Error(), "converge") {
		t.Errorf("error should mention CAS/converge, got: %v", err)
	}
}

// TestProcessRetryableNotices_AggregatesError 验证 processRetryableNotices 聚合多任务错误（A）。
// 构造一个 CAS 永远失败的任务 → processRetryableNotices 返回非空 error。
func TestProcessRetryableNotices_AggregatesError(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	seedSuspendedTask(store, "t2", "p1")
	for _, id := range []string{"t1", "t2"} {
		notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
			"sessionName": "ocdeck-" + id + "-serve", "reason": "kill_failed", "retryable": true,
			"cleanupTickets": []interface{}{"tk"},
		}}}
		sid := id
		store.mutTask(sid, func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	}
	failing := &casFailingStore{mockStore: store}
	m := newTestManager(t, failing, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.processRetryableNotices(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error from processRetryableNotices, got nil")
	}
	// 两个任务的 CAS 失败都应聚合。
	if !strings.Contains(err.Error(), "t1") || !strings.Contains(err.Error(), "t2") {
		t.Errorf("aggregated error should mention both tasks, got: %v", err)
	}
}

// TestRetryTaskNotices_SuccessReturnsNil 验证正常收敛返回 nil（A，反向用例）。
func TestRetryTaskNotices_SuccessReturnsNil(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 注入 retryable residual notice，无会话存活 → kill 跳过 → reap tickets 空成功 → CAS 清除。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{},
	}}}
	store.mutTask("t1", func(t *TaskRow) { t.Notice = encodeNotices(notice) })
	proc := newMockProc()
	proc.sessions["ocdeck-t1-serve"] = true
	proc.killResults["ocdeck-t1-serve"] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.retryTaskNotices(context.Background(), TaskRow{ID: "t1", Notice: encodeNotices(notice)}, notice)
	if err != nil {
		t.Errorf("expected nil on successful converge, got: %v", err)
	}
}

// errorsJoin helper 兜底（保证 errors.Join 可用；若编译报重复定义则移除）。
var _ = strings.Contains