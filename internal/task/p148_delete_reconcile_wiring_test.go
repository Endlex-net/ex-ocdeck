// p148_delete_reconcile_wiring_test.go 验证 P1.4.8 Delete/Reconcile/notice-CAS 的
// store 写入经 LifecycleService persist+commit 封装后行为等价（Phase B strangler 收尾，
// design.md D0:146-156 不变量冻结：Delete 静态检查先于删除意图、Reconcile Converge 先行、
// init_status 写入零发布）。
//
// 注入路径：mockAppAdapter 把 mockStore 适配为 application ports，Manager 写 helper 分流
// 至 LifecycleService（NoopPublisher 阶段无实际发布）。断言与 legacy 路径（p141/p142）
// 的关键不变量一致：guard 拒绝零副作用、preflight 失败不落 deleting、Delete 成功无残留行。
package task

import (
	"context"
	"errors"
	"sync"
	"testing"

	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
)

// TestP148_Delete_Normal_ViaLifecycle：注入 LifecycleService 后 Delete Normal 成功，
// 行删除无残留（writeBeginDeleteIntent → writeDeleteTask 全程经 lifecycle 路径）。
func TestP148_Delete_Normal_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManagerWithLifecycleService(t, store)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete normal via lifecycle: %v", err)
	}

	// 行已删除（Get not found），DeleteTask 恰好一次。
	if _, err := store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be gone after successful delete")
	}
	if n := store.deleteTaskCountVal(); n != 1 {
		t.Fatalf("DeleteTask count = %d, want 1", n)
	}
}

// TestP148_Delete_StaticCheckBeforeIntent_ViaLifecycle：PreflightDelete 失败 →
// codeConflict 且不落删除意图（行保持 suspended，未被 BeginDeleteIntent 置 deleting）。
// 冻结 design.md D0:144 不变量：静态检查先于删除意图。
func TestP148_Delete_StaticCheckBeforeIntent_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := newMockWorktree()
	wt.preflightErr = errors.New("worktree has dirty changes, confirm required")

	adapter := &mockAppAdapter{s: store}
	m := newTestManager(t, store, newMockProc(), wt, newMockOC(true))
	m.lifecycle = apptask.New(apptask.Options{Tasks: adapter, Read: adapter, Publish: apptask.NoopPublisher{}})

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatal("expected conflict on preflight failure")
	}
	assertCodeEq(t, "P148.staticCheck", err, codeConflict)

	// 未落删除意图：状态保持 suspended，DeleteMode 未写入。
	row, gerr := store.GetTask(context.Background(), "t1")
	if gerr != nil {
		t.Fatalf("task row must remain after preflight rejection: %v", gerr)
	}
	if row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended (BeginDeleteIntent must not run)", row.Status)
	}
	if row.DeleteMode.Valid {
		t.Fatalf("delete_mode = %+v, want empty (no intent persisted)", row.DeleteMode)
	}
	if n := store.deleteTaskCountVal(); n != 0 {
		t.Fatalf("DeleteTask count = %d, want 0", n)
	}
}

// TestP148_Delete_GuardReject_ViaLifecycle：active 任务 → invalid_state，零副作用
// （与 p141 guard 拒绝路径等价：不 BeginDeleteIntent、不 DeleteTask）。
func TestP148_Delete_GuardReject_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m := newTestManagerWithLifecycleService(t, store)

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatal("expected invalid_state on delete from active")
	}
	assertCodeEq(t, "P148.guardReject", err, codeInvalidState)

	row, gerr := store.GetTask(context.Background(), "t1")
	if gerr != nil {
		t.Fatalf("task row must remain: %v", gerr)
	}
	if row.Status != StatusActive {
		t.Fatalf("status = %s, want active (guard reject must not mutate)", row.Status)
	}
	if n := store.deleteTaskCountVal(); n != 0 {
		t.Fatalf("DeleteTask count = %d, want 0", n)
	}
}

// TestP148_Reconcile_ConvergeFirst_ViaLifecycle：注入 LifecycleService 后 Reconcile 仍
// 先执行 ConvergeInterruptedInitRuns（init running → failed），且先于 restoreCleanupDebts
// （复用 TestReconcile_ConvergeBeforeRestoreCleanupDebts 的 trace 手法）。
func TestP148_Reconcile_ConvergeFirst_ViaLifecycle(t *testing.T) {
	resetLifecycleCfgMock()
	orderMu := &sync.Mutex{}
	var order []string
	store := &convergeTraceStore{mockStore: newMockStore(), orderMu: orderMu, order: &order}
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt1", InitStatus: InitStatusRunning}

	oc := newMockOC(true)
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	debt := &restoreTraceDebtStore{inner: newMemCleanupDebtStore(), orderMu: orderMu, order: &order}
	adapter := &mockAppAdapter{s: store}
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: newMockProc(), Worktree: newMockWorktree(), OCFactory: wrap,
		DebtStore: debt, LifecycleRunner: &mockLifecycleRunner{},
	})
	m.lifecycle = apptask.New(apptask.Options{Tasks: adapter, Read: adapter, Publish: apptask.NoopPublisher{}})

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile via lifecycle: %v", err)
	}

	// Converge 先于 restoreCleanupDebts（共享 order slice 断言先后）。
	orderMu.Lock()
	snapshot := make([]string, len(order))
	copy(snapshot, order)
	orderMu.Unlock()
	convergeIdx, restoreIdx := -1, -1
	for i, name := range snapshot {
		if name == "converge" {
			convergeIdx = i
		}
		if name == "restoreDebts" {
			restoreIdx = i
		}
	}
	if convergeIdx < 0 {
		t.Fatal("ConvergeInterruptedInitRuns not called via lifecycle")
	}
	if restoreIdx < 0 {
		t.Fatal("restoreCleanupDebts not called (DebtStore injected)")
	}
	if convergeIdx >= restoreIdx {
		t.Fatalf("Converge (idx %d) must precede restoreCleanupDebts (idx %d)", convergeIdx, restoreIdx)
	}

	// init run 已收敛：running → failed（interrupted by server restart）。
	row, gerr := store.GetTask(context.Background(), "t1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if row.InitStatus != InitStatusFailed {
		t.Fatalf("init_status = %s, want failed (converged)", row.InitStatus)
	}
}

// TestP148_NoticeCAS_ViaLifecycle：注入 LifecycleService 后 recordResidualNotice 的
// notice CAS 写回经 writeNoticeCAS 路径不 panic 且收敛（entry 落库含 tickets）。
func TestP148_NoticeCAS_ViaLifecycle(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManagerWithLifecycleService(t, store)

	if err := m.recordResidualNotice(context.Background(), "t1", "ocdeck-serve-t1",
		[]string{"tk1"}, noticeReasonKillFailed, true); err != nil {
		t.Fatalf("recordResidualNotice via lifecycle: %v", err)
	}

	row, gerr := store.GetTask(context.Background(), "t1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notices: %v", perr)
	}
	found := false
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		if e.Data["sessionName"] == "ocdeck-serve-t1" {
			found = true
			// JSON 落库后 tickets 为 []interface{}（round-trip 后类型信息丢失）。
			raw, _ := e.Data["cleanupTickets"].([]interface{})
			if len(raw) != 1 || raw[0] != "tk1" {
				t.Fatalf("cleanupTickets = %v, want [tk1]", e.Data["cleanupTickets"])
			}
		}
	}
	if !found {
		t.Fatalf("residual notice for ocdeck-serve-t1 not persisted, notice=%+v", row.Notice)
	}
}
