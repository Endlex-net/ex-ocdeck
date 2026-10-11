package task

// charx_release_wiring_test.go 阶段 4 Release 接线验证（opencode-v1-spi-provider：
// 挂起/删除路径中 agent runtime（serve，binding.TerminalRef 对应会话）的终止经
// supervisor.Release——Release 成为生产路径）。生产装配形态（统一 fake server +
// 真实 supervisor + HealthTracker OnRuntimeReady/OnRuntimeRelease，main.go 装配
// 同形）下断言：
//  1. Suspend 的 runtime 终止走 Release：clean 释放经 OnRuntimeRelease →
//     tracker.ResetRuntime（runtime 代际归零）——内联 kill 无此回调，是 Release
//     分派被使用的可观察证据；终态行为（suspended/notice 空/会话已杀）不变；
//  2. Delete 路径的 runtime 终止（killResidualSessions）同样经 Release；
//  3. 非净 disposition 经 Release 的映射决策树保持：reap_failed + tickets →
//     finishSuspend 既有 notice 通道（reason/retryable/tickets 逐字段不变）；
//  4. ephemeral 临时 runtime 终止经 Release 且「清理失败阻断删除」契约保持
//     （deletion_failed + tickets 落账，D10 例外行）。
//
// mutation 证据（红→还原，接线分派旁路）：killTaskSessions / killResidualSessions
// 的 Release 分流条件临时 `&& false`（旁路回内联 kill）→ 场景 1/2 的 tracker
// ResetRuntime 断言红（OnRuntimeRelease 不再触发）→ 还原。
import (
	"context"
	"database/sql"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// newReleaseWiringEnv 装配生产形态环境：真实 supervisor + tracker（换代回调接线，
// main.go 同形）。harness 与 supervisor 统一同一 fake server；端口分配恒返该
// server 真端口（supervisor 内部健康检查直连）。返回 env 与 tracker。
func newReleaseWiringEnv(t *testing.T) (*charxEnv, *appagent.HealthTracker) {
	t.Helper()
	tr := appagent.NewHealthTracker()
	srv := newFakeOCServer(t)
	e := newCharxEnvOnServer(t, srv, newMockStore(), newCharxProc(), charxEnvOpts{})
	e.m.allocateServePortFn = func(_ sql.NullInt64, _ int) (int, error) { return srv.port, nil }
	seam := &mockSupStoreSeam{store: e.store}
	deps := opencode.SupervisorDeps{
		Proc:             e.proc,
		Store:            seam,
		SideFx:           &mockSupSideFx{m: e.m},
		AllocatePort:     func(int, bool, int) (int, error) { return srv.port, nil },
		OnRuntimeReady:   func(taskID string) { tr.BumpRuntimeEpoch(taskID) },
		OnRuntimeRelease: func(taskID string) { tr.ResetRuntime(taskID) },
		ReadyTimeout:     60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
		ProbeColdStartBackoff: func() []time.Duration { return []time.Duration{time.Millisecond} },
	}
	e.m.SetRuntimeSupervisor(opencode.NewSPIProviderWithDeps(deps).Supervisor())
	return e, tr
}

// activateAnchored 种子并激活锚定任务（直启单次成功形态）。
func activateAnchored(t *testing.T, e *charxEnv, taskID string) {
	t.Helper()
	charxSeedRepoWorktreeTask(e.store, taskID, "p1")
	dir := "/data/worktrees/p1/" + taskID
	e.srv.expectDirectory(dir)
	e.store.mutTask(taskID, func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "ses-" + taskID, Valid: true}
	})
	e.srv.seedSession(dir, "ses-"+taskID, 1, 1)
	if err := e.charxActivate(taskID); err != nil {
		t.Fatalf("Activate %s: %v", taskID, err)
	}
}

func TestCharx_ReleaseWiring_SuspendAndDelete(t *testing.T) {
	e, tr := newReleaseWiringEnv(t)
	ctx := context.Background()

	// ① 激活 → runtime 换代观测在位（Release 证据基线）。
	activateAnchored(t, e, "t1")
	if got := tr.RuntimeEpoch("t1"); got != 1 {
		t.Fatalf("epoch after activate = %d, want 1（前置：换代观测在位）", got)
	}

	// ② Suspend：runtime 终止经 Release（clean → OnRuntimeRelease → ResetRuntime：
	// epoch 归零 + runtime 作用域观测清空——内联 kill 无此回调，Release 分派证据）；
	// 终态行为保持：suspended、notice 空。
	if err := e.m.Suspend(ctx, "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if row := mustGet(e.store, "t1"); row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended", row.Status)
	}
	if entries := e.charxNoticesOf("t1"); len(entries) != 0 {
		t.Fatalf("notices = %v, want empty（clean 释放无残留）", entries)
	}
	if got := tr.RuntimeEpoch("t1"); got != 0 {
		t.Fatalf("epoch after suspend = %d, want 0（Release 分派证据：OnRuntimeRelease 未触发 = 接线旁路）", got)
	}
	for _, h := range tr.Snapshot() {
		if h.TaskID == "t1" && h.Scope == appagent.ScopeRuntime {
			t.Fatalf("runtime 作用域观测未清空（Release 分派证据失守）: %+v", h)
		}
	}

	// ③ Delete 路径（killResidualSessions）的 runtime 终止同样经 Release：
	// 重新激活（tracker 观测基线重建）→ killResidualSessions → epoch 归零
	//（内联 kill 不会触发 ResetRuntime）。
	activateAnchored(t, e, "t1")
	if got := tr.RuntimeEpoch("t1"); got != 1 {
		t.Fatalf("epoch after reactivate = %d, want 1", got)
	}
	if err := e.m.killResidualSessions(ctx, "t1"); err != nil {
		t.Fatalf("killResidualSessions: %v", err)
	}
	if got := tr.RuntimeEpoch("t1"); got != 0 {
		t.Fatalf("epoch after killResidualSessions = %d, want 0（Delete 路径 Release 分派证据）", got)
	}
}

// TestCharx_ReleaseWiring_DispositionMapping 非净 disposition 经 Release 的映射
// 决策树保持：killResults 注入 reap_failed + tickets → Suspend → finishSuspend
// 既有 notice 通道落账（reason=reap_failed/retryable=true/tickets 逐字段不变）。
func TestCharx_ReleaseWiring_DispositionMapping(t *testing.T) {
	e, _ := newReleaseWiringEnv(t)
	activateAnchored(t, e, "t1")
	e.proc.mockProc.mu.Lock()
	// reap_failed 真实形态：会话已杀（SessionKilled=true）但逃逸收割失败——
	//（killed=false 的矛盾注入会被 ClassifyKill fail-closed 判 kill_failed，
	// 内联与 Release 同规则）。
	e.proc.mockProc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-release-mapping"},
	}
	e.proc.mockProc.mu.Unlock()
	// 分支 c 修复路径封死（健康 503 → supervisor.Resume 复用健康失败 → 修复失败），
	// 使决策树落分支 a 收敛 suspended——reap_failed notice 经 Release 映射通道落账。
	e.srv.setHealthFailFirst(1 << 30)

	if err := e.m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if row := mustGet(e.store, "t1"); row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended（分支 a 收敛）", row.Status)
	}
	assertReapFailedNotice(t, e.store, "t1", "tk-release-mapping")
}

// TestCharx_ReleaseWiring_EphemeralBlocksDelete ephemeral 临时 runtime 终止经
// Release 且「清理失败阻断删除」契约保持（D10 例外行）：无 binding + runtime 已
// 死 + 存在归属会话 → AcquireEphemeral 临时绑定 → Release 非净（注入 reap_failed
// + tickets）→ deletion_failed + 行保留 + tickets 落 notice。
func TestCharx_ReleaseWiring_EphemeralBlocksDelete(t *testing.T) {
	e, _ := newReleaseWiringEnv(t)
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	_ = e.store.UpsertTaskSession(context.Background(), SessionRow{TaskID: "t1", SessionID: "s1"})
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-release-ephemeral"},
	}
	e.proc.mockProc.mu.Unlock()

	if err := e.m.Delete(context.Background(), "t1", DeleteNormal, false); err == nil {
		t.Fatal("Delete MUST be blocked by non-clean ephemeral cleanup（Release 路径）")
	}
	assertStatus(t, e.store, "t1", StatusDeletionFailed)
	if _, err := e.store.GetTask(context.Background(), "t1"); err != nil {
		t.Fatal("task row MUST be retained（阻断在 DB 提交点之前）")
	}
	found := false
	for _, n := range e.charxNoticesOf("t1") {
		if tks, ok := n.Data["cleanupTickets"].([]interface{}); ok {
			for _, tk := range tks {
				if s, ok := tk.(string); ok && s == "tk-release-ephemeral" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("ephemeral cleanup tickets must persist in notice（Release 路径）; notices=%+v", e.charxNoticesOf("t1"))
	}
}
