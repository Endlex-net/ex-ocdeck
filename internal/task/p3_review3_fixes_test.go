package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
	"ocdeck/internal/store"
)

// --- Fix 2：Suspend shell 错误聚合 + suspended 状态提交失败处理 ---

// failingUpdateStatusStore 包装 mockStore：UpdateTaskStatus 在置 suspended 时返回错误，
// 验证 finishSuspend 不静默吞错，MUST 记 last_error + 返回错误。
type failingUpdateStatusStore struct {
	*mockStore
	updateStatusErr error
}

func (s *failingUpdateStatusStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error {
	if s.updateStatusErr != nil && status == StatusSuspended {
		return s.updateStatusErr
	}
	return s.mockStore.UpdateTaskStatus(ctx, id, status, lastError)
}

// TestSuspend_ShellKillFailureAggregated 验证 shell 会话 kill 失败被聚合：
// finishSuspend 逐项记录 notice（含 shell），且状态落 suspended。
// 设计：design.md §19 Suspend 行 + §8 notice。B7：kill 失败记 notice。
func TestSuspend_ShellKillFailureAggregated(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	// serve 已死 → 分支 a；tui clean；shell-1 kill 失败。
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.killResults[shellSessionName("t1", 1)] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
		CleanupTickets: []string{"shell-tk"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend (branch a, shell kill failure): %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status=%s want suspended", row.Status)
	}
	// shell kill 失败 MUST 记录 residual notice（聚合）。
	entries, _ := parseNotices(row.Notice)
	foundShell := false
	for _, e := range entries {
		if sn, _ := e.Data["sessionName"].(string); sn == shellSessionName("t1", 1) {
			foundShell = true
			if r, _ := e.Data["retryable"].(bool); !r {
				t.Error("shell kill_failed notice must be retryable=true")
			}
		}
	}
	if !foundShell {
		t.Error("shell kill failure must be aggregated into residual notice")
	}
}

// TestSuspend_ShellKillFailureCountedOnce 验证 finishSuspend 逐项聚合记录，
// 不会因 killTaskSessions 与 finishSuspend 都记 notice 导致双写（同一清理失败两条 notice）。
func TestSuspend_ShellKillFailureCountedOnce(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions[tuiSessionName("t1")] = true
	proc.sessions[shellSessionName("t1", 1)] = true
	proc.killResults[shellSessionName("t1", 1)] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
		CleanupTickets: []string{"shell-tk"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	_ = m.Suspend(context.Background(), "t1")
	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	count := 0
	for _, e := range entries {
		if sn, _ := e.Data["sessionName"].(string); sn == shellSessionName("t1", 1) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shell kill failure notice count=%d want 1 (no double-write from killTaskSessions+finishSuspend)", count)
	}
}

// TestSuspend_StateCommitFailureReturnsError 验证 suspended 状态提交失败 MUST 处理：
// UpdateTaskStatus(suspended) 失败时 Suspend 返回非 nil，不静默置为无效流转。
func TestSuspend_StateCommitFailureReturnsError(t *testing.T) {
	base := newMockStore()
	seedSuspendedTask(base, "t1", "p1")
	base.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	fstore := &failingUpdateStatusStore{mockStore: base, updateStatusErr: errors.New("db write failed")}
	proc := newMockProc()
	proc.sessions[tuiSessionName("t1")] = true
	m := newTestManager(t, fstore, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must return error when suspended status commit fails, got nil")
	}
	if !strings.Contains(err.Error(), "suspended") && !strings.Contains(err.Error(), "commit") && !strings.Contains(err.Error(), "db write") {
		t.Errorf("error should describe suspended commit failure, got: %v", err)
	}
}

// --- Fix 3：Delete 四处边界 ---

// TestDelete_NormalRejectsDeletionFailed 验证 gating 与 delete_mode 一致：
// Normal 模式不得从 deletion_failed 直接重入 Normal 流程（design.md §19 Delete(Normal) 状态不含 deletion_failed）。
func TestDelete_NormalRejectsDeletionFailed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusDeletionFailed
		r.DeleteMode = sql.NullString{String: string(DeleteNormal), Valid: true}
	})
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("Delete(Normal) from deletion_failed must be rejected")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Errorf("code=%v want invalid_state (deletion_failed not allowed for Normal)", OpErrorCode(err))
	}
}

// TestDelete_ForceAcceptsDeletionFailed 验证 Force 模式接受 deletion_failed（design.md §19 Delete(Force)）。
func TestDelete_ForceAcceptsDeletionFailed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusDeletionFailed
		r.DeleteMode = sql.NullString{String: string(DeleteForce), Valid: true}
	})
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	// 走 deleteResume：无会话/无 sessions/无 debt → 删 worktree（mock remove nil）→ 删 DB 行。
	if err := m.Delete(context.Background(), "t1", DeleteForce, true); err != nil {
		t.Fatalf("Delete(Force) from deletion_failed should succeed: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task row should be deleted after Force delete from deletion_failed")
	}
}

// TestDelete_ForceDoesNotAutoConfirmDirty 验证 Force 不得自动 confirmDirty：
// dirty 未确认时 Force 删除 MUST 被 PreflightDelete 拒绝（与 Normal 同 dirty 要求，design.md §19）。
func TestDelete_ForceDoesNotAutoConfirmDirty(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := newMockWorktree()
	// PreflightDelete 在 ConfirmDirty=false 时返回 dirty 错误。
	wt.preflightErr = errors.New("worktree has dirty changes, confirm required")
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteForce, false)
	if err == nil {
		t.Fatal("Force delete without confirmDirty must be rejected when worktree dirty")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%v want conflict (dirty not confirmed)", OpErrorCode(err))
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status == StatusDeleting || row.Status == StatusDeletionFailed {
		t.Errorf("status=%s must not transition when dirty preflight rejected (no side effects before gate)", row.Status)
	}
}

// dirtyGrowWorktree 模拟 B7c：首次 DirtyFiles（preflight 快照）返回空集，
// 第二次 DirtyFiles（deleteResume 二次门禁）返回含 new.txt 的 dirty 集。
type dirtyGrowWorktree struct {
	*mockWorktree
	calls int
	mu    sync.Mutex
}

func (w *dirtyGrowWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	if n == 1 {
		// preflight 快照：干净。
		return map[string]struct{}{}, nil
	}
	// 二次门禁：产生新 dirty 文件。
	return map[string]struct{}{"new.txt": {}}, nil
}

// TestDelete_SecondDirtyGate_RejectsNewDirtyAfterPreflight 验证 B7c：preflight 通过后
// 在删除副作用期间若 worktree 新产生 dirty（preflight 快照中不存在的条目）未经确认，
// 不得删（落 deletion_failed，而非 ForceDirty:true 跳过检查直接删）。
func TestDelete_SecondDirtyGate_RejectsNewDirtyAfterPreflight(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := &dirtyGrowWorktree{mockWorktree: newMockWorktree()}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatal("Delete must be rejected when new dirty files appear after preflight")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%v want conflict (new dirty after preflight not confirmed)", OpErrorCode(err))
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Errorf("status=%s want deletion_failed (new dirty after preflight must not delete)", row.Status)
	}
}

// TestDelete_SecondDirtyGate_AllowsSameDirtyFromPreflight 验证 B7c：preflight 时已存在的
// dirty（用户已 confirmDirty=true 确认）在二次门禁时仍允许删除（非新产生，已确认）。
func TestDelete_SecondDirtyGate_AllowsSameDirtyFromPreflight(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := newMockWorktree()
	// preflight 与二次门禁时 dirty 文件集一致（同一文件，用户已 confirmDirty=true 确认）。
	wt.dirtyFiles["/data/worktrees/p1/t1"] = map[string]struct{}{"existing.txt": {}}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err != nil {
		t.Fatalf("Delete with same dirty from preflight (confirmed) must succeed, got: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted (preflight dirty confirmed, no new dirty)")
	}
}

// missingProjectStore 让 GetProject 在第二次调用（deleteResume ⑤）返回 not-found。
type missingProjectStore struct {
	*mockStore
	getProjectAfter int
	callCount       int
	mu              sync.Mutex
}

func (s *missingProjectStore) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	s.mu.Lock()
	s.callCount++
	isAfter := s.callCount > s.getProjectAfter
	s.mu.Unlock()
	if isAfter {
		return ProjectRow{}, errors.New("project gone")
	}
	return s.mockStore.GetProject(ctx, id)
}

// TestDelete_GetProjectFailureDeletionFailedNotSkipDB 验证 Delete 的 GetProject 第二次调用失败
// MUST NOT 跳过 worktree/branch 删除直接删 DB 行（改为 deletion_failed + last_error）。
func TestDelete_GetProjectFailureDeletionFailedNotSkipDB(t *testing.T) {
	base := newMockStore()
	seedSuspendedTask(base, "t1", "p1")
	// 第一次 GetProject（Delete 前置 PreflightDelete）成功；第二次（deleteResume ⑤）失败。
	fstore := &missingProjectStore{mockStore: base, getProjectAfter: 1}
	m := newTestManager(t, fstore, newMockProc(), newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, true)
	if err == nil {
		t.Fatal("expected error when second GetProject fails")
	}
	row, _ := base.GetTask(context.Background(), "t1")
	if row.Status != StatusDeletionFailed {
		t.Fatalf("status=%s want deletion_failed (must not skip to DB delete)", row.Status)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "project") {
		t.Errorf("last_error=%v should mention project lookup failure", row.LastError)
	}
	// DB 行 MUST 仍存在（未跳过 worktree 直接删 DB）。
	if _, err := base.GetTask(context.Background(), "t1"); err != nil {
		t.Error("task row must still exist (not deleted when GetProject failed)")
	}
}

// TestHasDebtTickets_OnlyRetryableBlocks 验证 Delete 债务门禁：仅 retryable=true 阻止删除；
// session_overflow 与 snapshot_missing_degraded（retryable=false）MUST NOT 阻止。
func TestHasDebtTickets_OnlyRetryableBlocks(t *testing.T) {
	cases := []struct {
		name    string
		entries []noticeEntry
		want    bool
	}{
		{"empty", nil, false},
		{"session_overflow only", []noticeEntry{{Code: noticeCodeSessionOverflow}}, false},
		{"degraded non-retryable only", []noticeEntry{{
			Code: noticeCodeResidual, Data: map[string]interface{}{
				"sessionName": "x", "reason": noticeReasonSnapshotMissing, "retryable": false,
			},
		}}, false},
		{"retryable residual", []noticeEntry{{
			Code: noticeCodeResidual, Data: map[string]interface{}{
				"sessionName": "x", "reason": noticeReasonKillFailed, "retryable": true,
			},
		}}, true},
		{"mixed overflow+retryable", []noticeEntry{
			{Code: noticeCodeSessionOverflow},
			{Code: noticeCodeResidual, Data: map[string]interface{}{"retryable": true}},
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasDebtTickets(c.entries); got != c.want {
				t.Errorf("hasDebtTickets=%v want %v", got, c.want)
			}
		})
	}
}

// TestDelete_NonRetryableNoticeDoesNotBlock 验证非 retryable notice（snapshot_missing_degraded）
// 不阻止删除：有 degraded notice 仍能完成删除。
func TestDelete_NonRetryableNoticeDoesNotBlock(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Notice = encodeNotices([]noticeEntry{{
			Code: noticeCodeResidual, Data: map[string]interface{}{
				"sessionName": "ocdeck-t1-serve", "reason": noticeReasonSnapshotMissing, "retryable": false,
			},
		}})
	})
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

	if err := m.Delete(context.Background(), "t1", DeleteNormal, true); err != nil {
		t.Fatalf("Delete with non-retryable notice must not be blocked: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Error("task should be deleted (non-retryable notice does not block)")
	}
}

// --- Fix 4：reconcile resumeActive stale notice ---

// TestReconcilePrePassDigestsDebtThenResumesActive 验证完整恢复路径（pre-pass → 恢复 active 端到端）：
// pre-pass 消化 retryable debt 后，resumeActive 的 hasDebt 判定重读当前 notice，
// 已清债任务被恢复为 active（不被旧快照误判为有 debt 而 kill+suspended）。
//
// 场景：残留 notice 指向已消失的 shell 会话（非 serve 本身，避免 pre-pass kill 误伤待恢复的 serve），
// serve 健康。pre-pass 对已消失会话跳过 kill，仅 RetryReap 清 tickets → notice 清空 → 恢复 active。
func TestReconcilePrePassDigestsDebtThenResumesActive(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	// 注入可消化的 retryable debt：sessionName 指向已不存在的 shell 会话，tickets reap 成功。
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": shellSessionName("t1", 1), "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })

	proc := newMockProc()
	// serve 健康（存在 + env + OCDECK_TASK_ID 匹配）；shell 会话不存在（pre-pass 跳过 kill）。
	proc.sessions[serveSessionName("t1")] = true
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
		t.Fatalf("status=%s want active (debt digested by pre-pass then resumed)", row.Status)
	}
	// notice 应被清空（debt 消化）。
	entries, _ := parseNotices(row.Notice)
	if len(entries) != 0 {
		t.Errorf("notice should be digested after pre-pass; remaining=%d", len(entries))
	}
	if m.getRuntime("t1") == nil {
		t.Error("runtime should be registered after successful resume")
	}
}

// TestReconcileStaleNoticeNotUsed 验证 resumeActive 的 hasDebt 判定重读当前 notice：
// 若沿用 pre-pass 前旧 t.Notice（仍有 retryable），会误判 kill+suspended。
// 此测试构造 pre-pass 能清债的场景，断言不被旧快照误导。
// （与上一测试互补：显式断言不进 suspended 分支。）
func TestReconcileStaleNoticeNotUsed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	snap := envSnapshot{Vars: map[string]string{"OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1"}}
	snapBytes, _ := encodeEnvSnapshot(snap)
	store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": shellSessionName("t1", 1), "reason": "kill_failed", "retryable": true,
		"cleanupTickets": []interface{}{"tk1"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })

	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	_ = m.Reconcile(context.Background())
	row, _ := store.GetTask(context.Background(), "t1")
	// 关键断言：若沿用旧 notice 判定，会走 suspended 分支。此处必须 active。
	if row.Status == StatusSuspended {
		t.Error("status=suspended indicates stale notice used; must reread current notice after pre-pass")
	}
}

// --- Fix 5：orphan retry tickets 不丢弃 ---

// reapPartialProc：RetryReap 返回指定 remaining（模拟部分 tickets 未收割）。
type reapPartialProc struct {
	*mockProc
	remaining []string
	reapErr   error
	reapCalls int
	mu        sync.Mutex
}

func (p *reapPartialProc) RetryReap(tickets []string) ([]string, error) {
	p.mu.Lock()
	p.reapCalls++
	p.mu.Unlock()
	if p.reapErr != nil {
		return tickets, p.reapErr
	}
	return p.remaining, nil
}

// TestRetryOrphanSessions_RetainsRemainingTickets 验证 RetryReap 返回 remaining>0 时
// 保留 orphanFailure 进下轮重试（不丢弃未收割 tickets）。
func TestRetryOrphanSessions_RetainsRemainingTickets(t *testing.T) {
	store := newMockStore() // 无 DB 任务
	proc := &reapPartialProc{mockProc: newMockProc(), remaining: []string{"tk-undead"}}
	// 会话已消失（不存活）→ 不得丢弃既有 tickets。
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	// 预置一个 orphanFailure（会话已消失 + 既有 tickets）。
	m.orphanMu.Lock()
	m.orphanFailures = []orphanFailure{{sessionName: "ocdeck-ghost-serve", tickets: []string{"tk-undead"}}}
	m.orphanMu.Unlock()

	m.retryOrphanSessions(context.Background())

	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanMu.Unlock()
	if len(failures) != 1 {
		t.Fatalf("expected 1 retained failure (tickets not reaped), got %d", len(failures))
	}
	if len(failures[0].tickets) != 1 || failures[0].tickets[0] != "tk-undead" {
		t.Errorf("retained tickets=%v want [tk-undead]", failures[0].tickets)
	}
}

// TestRetryOrphanSessions_ReapErrorRetainsTickets 验证 RetryReap 返回 error 时保留全部既有 tickets。
func TestRetryOrphanSessions_ReapErrorRetainsTickets(t *testing.T) {
	store := newMockStore()
	proc := &reapPartialProc{mockProc: newMockProc(), reapErr: errors.New("reap infra error")}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = []orphanFailure{{sessionName: "ocdeck-ghost-serve", tickets: []string{"tk1", "tk2"}}}
	m.orphanMu.Unlock()

	m.retryOrphanSessions(context.Background())

	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanMu.Unlock()
	if len(failures) != 1 {
		t.Fatalf("expected 1 retained failure on reap error, got %d", len(failures))
	}
	if len(failures[0].tickets) != 2 {
		t.Errorf("reap error must retain all existing tickets, got %d", len(failures[0].tickets))
	}
}

// TestRetryOrphanSessions_NewKillTicketsAggregated 验证后续 kill 产生的新 tickets 与既有聚合（不覆盖丢失）。
func TestRetryOrphanSessions_NewKillTicketsAggregated(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	// 会话仍存活，kill 失败产生新 tickets。
	proc.sessions["ocdeck-ghost-serve"] = true
	proc.killResults["ocdeck-ghost-serve"] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
		CleanupTickets: []string{"new-tk"},
	}
	// 既有 tickets 经 reap 全部收割（remaining=nil）。
	reapProc := &reapPartialProc{mockProc: proc, remaining: nil}
	m := newTestManager(t, store, reapProc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = []orphanFailure{{sessionName: "ocdeck-ghost-serve", tickets: []string{"old-tk"}}}
	m.orphanMu.Unlock()

	m.retryOrphanSessions(context.Background())

	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanMu.Unlock()
	if len(failures) != 1 {
		t.Fatalf("expected 1 retained failure (kill failed), got %d", len(failures))
	}
	// 新 tickets 与既有（已 reap 清空）聚合：此处既有已收割，剩 new-tk。
	hasNew := false
	for _, tk := range failures[0].tickets {
		if tk == "new-tk" {
			hasNew = true
		}
	}
	if !hasNew {
		t.Errorf("new kill tickets must be aggregated, got %v", failures[0].tickets)
	}
}

// --- Fix 6：Shutdown 严格性 ---

// TestShutdownKillAllSessions_ResidualReturnsError 验证 kill 残留（非 clean）时
// shutdownKillAllSessions 返回非 nil，tickets 持久化为 DB notice（逃逸进程下次启动可定位）。
func TestShutdownKillAllSessions_ResidualReturnsError(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions["ocdeck-t1-serve"] = true
	proc.killResults["ocdeck-t1-serve"] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed,
		CleanupTickets: []string{"res-tk"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown must return error when residual sessions remain after kill")
	}
	// tickets MUST 持久化为 DB notice（逃逸进程下次启动可定位），不得仅在内存 orphanFailures。
	row, _ := store.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("notice JSON parse error: %v", perr)
	}
	found := false
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if tks, ok := e.Data["cleanupTickets"].([]interface{}); ok {
			for _, tk := range tks {
				if s, ok := tk.(string); ok && s == "res-tk" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("residual kill tickets must be persisted as DB notice (locatable next start); got empty")
	}
}

// TestShutdownKillAllSessions_RetryableDebtReturnsError 验证 DB retryable debt 未清时
// Shutdown 返回非 nil（kill 模式下确认 runtime 已空需含 DB debt）。
func TestShutdownKillAllSessions_RetryableDebtReturnsError(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusSuspended
		r.Notice = encodeNotices([]noticeEntry{{
			Code: noticeCodeResidual, Data: map[string]interface{}{
				"sessionName": "ocdeck-t1-serve", "reason": "kill_failed", "retryable": true,
				"cleanupTickets": []interface{}{"tk1"},
			},
		}})
	})
	// 会话已全部 kill clean（无残留会话），但 DB 仍有 retryable debt。
	proc := newMockProc()
	proc.sessions["ocdeck-t1-serve"] = true
	proc.killResults["ocdeck-t1-serve"] = process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}
	// RetryReap 返回非空（debt 不可收敛）。
	reapProc := &reapPartialProc{mockProc: proc, remaining: []string{"tk1"}}
	m := newTestManager(t, store, reapProc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown must return error when DB retryable debt remains (runtime not clean)")
	}
}

// TestShutdownPersistReturnsNil 验证 persist 模式 Shutdown 不 kill 会话，返回 nil。
func TestShutdownPersistReturnsNil(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	proc.sessions["ocdeck-t1-serve"] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownPersist

	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("persist Shutdown must return nil, got %v", err)
	}
	// persist 模式会话保留。
	if !proc.sessions["ocdeck-t1-serve"] {
		t.Error("persist mode must keep sessions alive")
	}
}

// --- Fix 7：Reconcile fail-closed（在 store 包真实 DB 不可用层面，用 mock 模拟 reconcile 错误）---
// Reconcile 失败拒绝开放 HTTP 由 cmd/main.go 保证（非 task 包可测）；
// 此处验证 Reconcile 在 ListSessions 非 no-server 错误时返回 error（fail-closed 已实现）。

func TestReconcile_ListSessionsInfraErrorFailClosed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := &infraListProc{mockProc: newMockProc(), err: errors.New("tmux socket broken")}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile must return error on ListSessions infra error (fail-closed)")
	}
}

// infraListProc 让 ListSessions 返回非 no-server 错误（fail-closed）。
type infraListProc struct {
	*mockProc
	err error
}

func (p *infraListProc) ListSessions() ([]string, error) {
	return nil, p.err
}

// --- 测试升级：notice/CAS 用真实 store 测 CAS 冲突合并、损坏 JSON fail-closed ---

// openRealStore 构造真实 SQLite store.DB 并 seed 一个项目+任务，返回适配器与底层 DB。
func openRealStore(t *testing.T) (*StoreAdapter, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.CreateProject(ctx, "p1", "proj", "/repo", "main", "repo"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.CreateTask(ctx, store.TaskRow{
		ID: "t1", ProjectID: "p1", Name: "task", Branch: "b", Status: StatusSuspended, WorktreePath: "/wt",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return NewStoreAdapter(db), db
}

// TestRealStore_NoticeCAS_ConcurrentMerge 验证用真实 store 测 CAS 冲突合并：
// 两个并发 recordResidualNotice 调用，CAS 保证两者都落库（不互相覆盖丢失）。
func TestRealStore_NoticeCAS_ConcurrentMerge(t *testing.T) {
	adapter, _ := openRealStore(t)
	m := newTestManager(t, adapter, newMockProc(), newMockWorktree(), newMockOC(true))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = m.recordResidualNotice(context.Background(), "t1", "ocdeck-t1-serve", []string{"tk-a"}, noticeReasonKillFailed, true)
	}()
	go func() {
		defer wg.Done()
		_ = m.recordResidualNotice(context.Background(), "t1", "ocdeck-t1-tui", []string{"tk-b"}, noticeReasonSnapshotFailed, true)
	}()
	wg.Wait()

	row, _ := adapter.GetTask(context.Background(), "t1")
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notices: %v", perr)
	}
	// 两条 notice 都应落库（CAS 合并，不互相覆盖）。
	hasServe, hasTui := false, false
	for _, e := range entries {
		if sn, _ := e.Data["sessionName"].(string); sn == "ocdeck-t1-serve" {
			hasServe = true
		}
		if sn, _ := e.Data["sessionName"].(string); sn == "ocdeck-t1-tui" {
			hasTui = true
		}
	}
	if !hasServe || !hasTui {
		t.Errorf("CAS merge lost an entry: serve=%v tui=%v entries=%d", hasServe, hasTui, len(entries))
	}
}

// TestRealStore_CorruptedNoticeJSON_FailClosed 验证损坏 JSON 视为有 debt（fail-closed）：
// parseNotices 返回 error；hasRetryableNotice 返回 true+error（门禁拒绝）。
func TestRealStore_CorruptedNoticeJSON_FailClosed(t *testing.T) {
	adapter, _ := openRealStore(t)
	// 直接写入损坏 JSON。
	adapter.db.ExecContext(context.Background(),
		"UPDATE tasks SET notice = ? WHERE id = ?", "{broken json", "t1")

	row, err := adapter.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	_, perr := parseNotices(row.Notice)
	if perr == nil {
		t.Error("corrupted notice JSON must return error from parseNotices (fail-closed)")
	}
	m := newTestManager(t, adapter, newMockProc(), newMockWorktree(), newMockOC(true))
	has, herr := m.hasRetryableNotice(context.Background(), row)
	if !has {
		t.Error("corrupted notice must be treated as has-retryable-debt (fail-closed)")
	}
	if herr == nil {
		t.Error("corrupted notice must propagate error from hasRetryableNotice")
	}
}

// --- typed infra/SSE 生命周期测试（infra_error 收敛路径）---
// TestTypedInfraError_ConvergesToSuspended 已在 lane_cf_test.go 中覆盖（TestTypedRuntimeEvent_ServeInfraErrorSuspended）。
// 此处补充 SSE 对齐失败收敛路径（infra_error 收敛：SSE 建立后全量对齐失败 → kill runtime → suspended）。

// alignFailOC：SubscribeEvents 正常触发 onReady 后阻塞（模拟 SSE 建立成功），
// 但 ListSessions 返回非 overflow 错误 → startSSE 的首次 alignSessions 失败 → activate 失败收敛。
type alignFailOC struct {
	*mockOC
	listErr error
}

func (c *alignFailOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return nil, c.listErr
}

// TestSSEAlignFailure_ConvergesToSuspended 验证 SSE 建立后首次对齐失败 →
// activate 中途失败 → kill runtime → suspended + last_error（infra_error 收敛路径）。
func TestSSEAlignFailure_ConvergesToSuspended(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// 不预置会话：Activate 前置 checkNoResidualSessions 通过；
	// activateRun 内 startServeWithPortRetry 创建 serve 会话（mock NewSession 成功）。
	proc.envValues[serveSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001", "OCDECK_TASK_ID": "t1",
	}
	baseOC := newMockOC(true)
	oc := &alignFailOC{mockOC: baseOC, listErr: errors.New("opencode list sessions infra error")}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error on SSE align failure")
	}
	// 失败 MUST kill runtime + 落 suspended + last_error（infra_error 收敛）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended (SSE align failure must converge to suspended)", row.Status)
	}
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Error("SSE align failure must record last_error (not silent)")
	}
	if m.getRuntime("t1") != nil {
		t.Error("runtime must be cleared after SSE align failure (no orphan runtime)")
	}
}

// _ 保持引用（部分构造用 fmt）。
var _ = fmt.Sprintf
