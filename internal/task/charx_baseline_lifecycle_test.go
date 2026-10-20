package task

// charx_baseline_lifecycle_test.go 落地阶段 0 基线组 fixture（tasks.md 1.2）：
// suspend/delete/reconcile 正常路径与失败注入。断言面：会话终止顺序（观察面 3）、
// 上游删除调用序列、归属清理终态（观察面 2/5）、deletion_failed 与 Retry 收敛、
// 失败注入的精确终态与文本（A5①）。
//
// 有效性证据思路：suspend 走生产 killTaskSessions/finishSuspend/tryRepairRuntime；
// delete 走 deleteOCSessions（suspended 任务经一次性 temp serve，delete.go:499 现状），
// 上游 DELETE 由 fake server 记录；失败注入按 sessionID/kill 结果精确命中，
// 能区分「修复回 active」「恢复失败落 suspended」等不同终态。

import (
	"context"
	"strings"
	"testing"

	"ocdeck/internal/infrastructure/process"
)

// TestCharx_Suspend_Normal 正常挂起：先建立非空 pending（permission + question，
// A5②——attention 清空必须从非空态出发验证），挂起后 runtime 会话被 kill、任务
// suspended、归属保留、attention 清空。
func TestCharx_Suspend_Normal(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	ownedBefore := len(e.charxOwnedSessions("t1"))
	e.srv.waitSubscribers(t, 1)

	// 挂起前建立非空 pending：permission + question 各一条（同步屏障确保非空）。
	e.srv.broadcastEvent(t, charxPermAsked("permission.asked", "perm-s", anchor, "bash", []string{"x"}, ""))
	e.srv.broadcastEvent(t, charxQuestAsked("question.asked", "q-s", anchor, "h", "q", false))
	e.charxAssertAttentionSet("t1", []string{"perm-s"}, []string{"q-s"})

	if err := e.m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, e.store, "t1", StatusSuspended)
	if e.proc.mockProc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime session must be killed on suspend")
	}
	if kills := countKillServe(e.proc.mockProc, "t1"); kills < 1 {
		t.Errorf("KillSession(runtime) = %d, want >= 1", kills)
	}
	// 归属保留。
	if got := len(e.charxOwnedSessions("t1")); got != ownedBefore || !e.charxOwnedSessions("t1")[anchor] {
		t.Errorf("ownership after suspend = %v, want retained (%d rows incl anchor)", e.charxOwnedSessions("t1"), ownedBefore)
	}
	// attention 清空：运行时回收后快照必为空集（从非空态出发，A5②）。
	att, ok := e.m.Attention("t1")
	if len(att.Permissions) != 0 || len(att.Questions) != 0 {
		t.Errorf("attention after suspend = %+v, want cleared (was non-empty before)", att)
	}
	if ok && (att.Permissions == nil || att.Questions == nil) {
		t.Errorf("attention after suspend must be non-nil empty slices, got %+v (ok=%v)", att, ok)
	}
	row := e.charxTaskRow("t1")
	if row.LastError.Valid && row.LastError.String != "" {
		t.Errorf("last_error = %q, want empty", row.LastError.String)
	}
}

// TestCharx_Suspend_KillFailRepaired 失败注入（A5①）：runtime kill 返回
// kill_failed disposition（带 tickets）→ 挂起分支 c：tryRepairRuntime 重建运行时
// （fake 上游健康 → 修复成功）→ 回 active + 固定文本 last_error。
// 基线实际行为锁定：分支 c 修复回 active 时不落 residual notice（killTaskSessions
// 仅收集结果，notice 落库发生在 suspended 终态的 finishSund；修复路径未到达）——
// 与 suspended 终态路径不同，注意这是既有语义（见汇报疑点）。
func TestCharx_Suspend_KillFailRepaired(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 注入 runtime kill 的 kill_failed disposition（tickets 保留供观察）。
	e.proc.mockProc.mu.Lock()
	e.proc.mockProc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk-charx"},
	}
	e.proc.mockProc.mu.Unlock()

	if err := e.m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, e.store, "t1", StatusActive)
	row := e.charxTaskRow("t1")
	if !row.LastError.Valid || row.LastError.String != "suspend partially failed, runtime repaired" {
		t.Errorf("last_error = %+v, want \"suspend partially failed, runtime repaired\"", row.LastError)
	}
	// 基线行为：分支 c 修复成功不产生 notice（finishSund 未执行）。
	if entries := e.charxNoticesOf("t1"); len(entries) != 0 {
		t.Errorf("notices = %+v, want none（分支 c 修复路径不落 residual notice）", entries)
	}
	// 修复后的 runtime 存活且 SSE 重新订阅。
	if !e.proc.mockProc.sessions[runtimeSessionName("t1")] {
		t.Error("repaired runtime session must exist (branch c)")
	}
	e.srv.waitSubscribers(t, 1)
}

// TestCharx_Reconcile_ResumeFailsOnTaskIDMismatch 失败注入（A5①）：persist 重启
// 恢复时会话 env 的 OCDECK_TASK_ID 与任务不匹配 → resumeActive 失败 → 完整运行时
// 清理 + suspended + 精确文本 last_error；Reconcile 以聚合错误返回
// （reconcilePersist 收集 resume 失败但状态已收敛 suspended）。
func TestCharx_Reconcile_ResumeFailsOnTaskIDMismatch(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	proc := newCharxProc()
	e1 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	if err := e1.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	e1.lifeCancel()

	// server 重启前篡改会话 env 的任务标识（固定故障输入）。
	name := runtimeSessionName("t1")
	proc.mockProc.mu.Lock()
	env := proc.mockProc.envValues[name]
	env["OCDECK_TASK_ID"] = "bad"
	proc.mockProc.envValues[name] = env
	proc.mockProc.mu.Unlock()

	e2 := newCharxEnvOnServer(t, srv, store, proc, charxEnvOpts{})
	rerr := e2.m.Reconcile(context.Background())
	if rerr == nil || !strings.Contains(rerr.Error(), "OCDECK_TASK_ID mismatch") {
		t.Fatalf("Reconcile error = %v, want aggregated resume failure containing mismatch", rerr)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	row := e2.charxTaskRow("t1")
	want := `OCDECK_TASK_ID mismatch (got "bad" want "t1"): %!w(<nil>)`
	if !row.LastError.Valid || row.LastError.String != want {
		t.Errorf("last_error = %+v, want %q", row.LastError, want)
	}
	// 恢复失败分类：runtime 会话被清理、恢复预算零消耗（resume 失败非重拉路径）。
	if proc.mockProc.sessions[name] {
		t.Error("runtime session must be cleaned after failed resume")
	}
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("recovery permits = %d, want 0", n)
	}
}

// TestCharx_Delete_NormalWithFailureRetry 删除：suspended 任务（runtime 已回收）
// 经一次性 temp serve 逐项删除上游 owned session → worktree 移除 → 行级联删除。
// 失败注入（锚定 DELETE 500）→ deletion_failed、行保留；Retry 后成功收敛。
func TestCharx_Delete_NormalWithFailureRetry(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	anchor := e.charxTaskRow("t1").AnchorSessionID.String
	if err := e.m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// 注入锚定 session 的 DELETE 500 → 删除失败。
	e.srv.mu.Lock()
	e.srv.deleteStatusByID[anchor] = 500
	e.srv.mu.Unlock()
	if err := e.m.Delete(context.Background(), "t1", DeleteNormal, false); err == nil {
		t.Fatal("Delete must fail when upstream session delete fails")
	}
	assertStatus(t, e.store, "t1", StatusDeletionFailed)
	// 行保留（未越过 DB 提交点）；上游 session 仍在。
	if _, err := e.store.GetTask(context.Background(), "t1"); err != nil {
		t.Fatal("task row must be retained on deletion failure")
	}
	if !e.charxOwnedSessions("t1")[anchor] {
		t.Error("ownership row must be retained on deletion failure")
	}
	if _, ok := e.srv.sessionByID(anchor); !ok {
		t.Error("upstream anchor session must be retained on deletion failure")
	}

	// 解除注入 → Retry（deletion_failed 重入）→ 删除成功。
	e.srv.mu.Lock()
	delete(e.srv.deleteStatusByID, anchor)
	e.srv.mu.Unlock()
	if err := e.m.Retry(context.Background(), "t1", false); err != nil {
		t.Fatalf("Retry delete: %v", err)
	}
	if _, err := e.store.GetTask(context.Background(), "t1"); err == nil {
		t.Fatal("task row must be deleted after successful retry")
	}
	if e.srv.sessionCount() != 0 {
		t.Errorf("upstream sessions = %d, want 0 (all deleted)", e.srv.sessionCount())
	}
	if n := e.srv.deleteCallsCount(); n < 1 {
		t.Errorf("upstream DELETE calls = %d, want >= 1", n)
	}
	// worktree 移除被调用（repo+worktree 任务）。
	if e.wt.removeCalls() < 1 {
		t.Errorf("worktree Remove calls = %d, want >= 1", e.wt.removeCalls())
	}
}

// TestCharx_Reconcile_SuspendingCompletes reconcile persist 矩阵：suspending
// 中断态在重启后完成清理并落 suspended（以持久化意图为准）。
func TestCharx_Reconcile_SuspendingCompletes(t *testing.T) {
	e := newCharxEnv(t, charxEnvOpts{})
	charxSeedRepoWorktreeTask(e.store, "t1", "p1")
	if err := e.charxActivate("t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 直接注入中断态：suspending + 会话仍在（模拟挂起中途 server 重启）。
	e.store.mutTask("t1", func(tr *TaskRow) { tr.Status = StatusSuspending })

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, e.store, "t1", StatusSuspended)
	if e.proc.mockProc.sessions[runtimeSessionName("t1")] {
		t.Error("reconcile must complete the interrupted suspend (kill runtime)")
	}
}
