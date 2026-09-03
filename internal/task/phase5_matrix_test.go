package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// TestRecordResidualNoticeFromDisposition_FailClosedTable 验证 Phase 5 技术债 a：
// recordResidualNoticeFromDisposition 与 reconcile 同款 classifyKillResult 完整表——
// 未知 disposition 与 clean+!SessionKilled 矛盾不再静默返回 nil，一律 retryable
// kill_failed 显式记录（旧实现的 dispositionToNotice 对这两类返回 !ok 被吞掉，
// 调用方可能留下无 notice/debt 索引的存活进程）。
func TestRecordResidualNoticeFromDisposition_FailClosedTable(t *testing.T) {
	cases := []struct {
		name       string
		res        process.KillResult
		wantNotice string // 空 = 不记
		wantRetry  bool
	}{
		{
			name:       "consistent clean records nothing",
			res:        process.KillResult{SessionKilled: true, Disposition: process.DispositionClean},
			wantNotice: "",
		},
		{
			name:       "degraded records non-retryable",
			res:        process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded},
			wantNotice: noticeReasonSnapshotMissing,
		},
		{
			name:       "reap_failed records retryable",
			res:        process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed},
			wantNotice: noticeReasonReapFailed, wantRetry: true,
		},
		{
			name:       "unknown disposition fail-closed as retryable kill_failed",
			res:        process.KillResult{SessionKilled: true, Disposition: process.CleanupDisposition("weird")},
			wantNotice: noticeReasonKillFailed, wantRetry: true,
		},
		{
			name:       "contradictory clean without SessionKilled fail-closed as retryable kill_failed",
			res:        process.KillResult{SessionKilled: false, Disposition: process.DispositionClean},
			wantNotice: noticeReasonKillFailed, wantRetry: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))

			err := m.recordResidualNoticeFromDisposition(context.Background(), "t1", "ocdeck-t1-shell-1", tc.res)
			if err != nil {
				t.Fatalf("record: %v", err)
			}
			row, _ := store.GetTask(context.Background(), "t1")
			entries, perr := parseNotices(row.Notice)
			if tc.wantNotice == "" {
				if perr == nil && len(entries) > 0 {
					t.Fatalf("unexpected notice: %v", row.Notice)
				}
				return
			}
			if perr != nil || len(entries) != 1 {
				t.Fatalf("notice missing: %v %+v", perr, row.Notice)
			}
			reason, _ := entries[0].Data["reason"].(string)
			retry, _ := entries[0].Data["retryable"].(bool)
			if reason != tc.wantNotice || retry != tc.wantRetry {
				t.Errorf("notice reason=%q retryable=%v want %q/%v", reason, retry, tc.wantNotice, tc.wantRetry)
			}
		})
	}
}

// --- 8.x 测试矩阵缺口收尾（Phase 5 审计补充） ---

// TestActivate_DualStartConsumesNoRecoveryPermit 验证 8.1/预算协议：首次 Activate
// （含无锚定双启动的两次 NewSession）MUST NOT 消耗恢复 permit——permit 子协议仅
// 适用 Recovery 路径（Activate 沿其既有端口轮换重试预算）。
func TestActivate_DualStartConsumesNoRecoveryPermit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := store.recoveryPermitCount("t1"); got != 0 {
		t.Fatalf("permits=%d want 0 (Activate dual-start MUST NOT consume recovery permits)", got)
	}
	if got := runtimeCreateCount(proc, "t1"); got != 2 {
		t.Fatalf("runtime creates=%d want 2 (dual-start)", got)
	}
}

// TestSuspendDuringActivating_TransitionalReject 验证 8.2 双向拿锁的恢复先拿锁侧：
// 任务已 activating（恢复进行中）时 Suspend 收 transitional-state 拒绝
// （invalid_state，经 domain CanSuspend guard），状态保持 activating。
func TestSuspendDuringActivating_TransitionalReject(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActivating })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil || OpErrorCode(err) != codeInvalidState {
		t.Fatalf("err=%v want invalid_state (transitional reject while activating)", err)
	}
	assertStatus(t, store, "t1", StatusActivating)
}

// envCaptureProc 捕获每次 NewSession 的 env（8.4 每次新密码断言用；
// mockProc.envValues 按会话名覆盖，无法比对同名双启动的历史值）。
type envCaptureProc struct {
	*mockProc
	envs []map[string]string
}

func (p *envCaptureProc) NewSession(spec process.SessionSpec) error {
	if err := p.mockProc.NewSession(spec); err != nil {
		return err
	}
	p.envs = append(p.envs, spec.Env)
	return nil
}

// TestRecovery_FreshPasswordPerCreate 验证 8.4/预算协议：Recovery 双启动子事务的
// 两次进程创建各自生成新随机密码（端口复用已持久化值，密码 MUST NOT 复用）。
func TestRecovery_FreshPasswordPerCreate(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := &envCaptureProc{mockProc: newMockProc()}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	if len(proc.envs) != 2 {
		t.Fatalf("captured creates=%d want 2 (dual-start)", len(proc.envs))
	}
	pw1 := proc.envs[0]["OPENCODE_SERVER_PASSWORD"]
	pw2 := proc.envs[1]["OPENCODE_SERVER_PASSWORD"]
	if pw1 == "" || pw2 == "" || pw1 == pw2 {
		t.Fatalf("password must be regenerated per create (pw1==pw2: %v)", pw1 == pw2)
	}
	if proc.envs[0]["OCDECK_SERVE_PORT"] != proc.envs[1]["OCDECK_SERVE_PORT"] {
		t.Errorf("dual-start MUST reuse the persisted port: %v vs %v",
			proc.envs[0]["OCDECK_SERVE_PORT"], proc.envs[1]["OCDECK_SERVE_PORT"])
	}
}

// --- G5-2 调用链级反例：nil-error KillResult 必须经 classifyKillResult ---

func residualReasonRetryable(t *testing.T, store *mockStore, taskID string) (reason string, retryable bool, n int) {
	t.Helper()
	row, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatalf("parse notice: %v", perr)
	}
	for _, e := range entries {
		if e.Code != noticeCodeResidual {
			continue
		}
		n++
		reason, _ = e.Data["reason"].(string)
		retryable, _ = e.Data["retryable"].(bool)
	}
	return reason, retryable, n
}

// TestDelete_ContradictoryCleanDoesNotCommit 验证 Delete 路径：KillResult 为
// DispositionClean 但 SessionKilled=false（矛盾值）不得越过 DB 提交点。旧 raw
// `Disposition != clean` 分支把该值当成功，Delete 会删行致 tickets 随 CASCADE 丢失。
func TestDelete_ContradictoryCleanDoesNotCommit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	name := runtimeSessionName("t1")
	proc.sessions[name] = true
	proc.killResults[name] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionClean,
		CleanupTickets: []string{"tk-contradict"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteForce, true)
	if err == nil {
		t.Fatal("Delete must fail on contradictory clean KillResult")
	}
	if store.deleteTaskCountVal() != 0 {
		t.Fatal("Delete MUST NOT cross DB commit (DeleteTask) on non-none classification")
	}
	assertStatus(t, store, "t1", StatusDeletionFailed)
	reason, retry, n := residualReasonRetryable(t, store, "t1")
	if n != 1 || reason != noticeReasonKillFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want kill_failed/true/1", reason, retry, n)
	}
}

// TestShutdown_UnknownDispositionRetainsRetryableDebt 验证 shutdown 路径：未知
// disposition 不得当 clean 跳过；必须记 retryable notice 且 Shutdown 返回未净。
func TestShutdown_UnknownDispositionRetainsRetryableDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	name := runtimeSessionName("t1")
	proc.sessions[name] = true
	proc.killResults[name] = process.KillResult{
		SessionKilled: true, Disposition: process.CleanupDisposition("weird"),
		CleanupTickets: []string{"tk-unknown"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown must return error on unknown disposition (runtime not clean)")
	}
	reason, retry, n := residualReasonRetryable(t, store, "t1")
	if n != 1 || reason != noticeReasonKillFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want kill_failed/true/1", reason, retry, n)
	}
}

// TestShutdown_ContradictoryCleanRetainsRetryableDebt 验证 G5-6：Shutdown 调用链
// 对 DispositionClean+SessionKilled=false 不得走旧 raw `Disposition != clean` 旁路
// （该条件把矛盾 clean 当成功）；必须返回未净并保留 retryable kill_failed debt。
func TestShutdown_ContradictoryCleanRetainsRetryableDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.Status = StatusActive })
	proc := newMockProc()
	name := runtimeSessionName("t1")
	proc.sessions[name] = true
	proc.killResults[name] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionClean,
		CleanupTickets: []string{"tk-contradict-sd"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.cfg.ShutdownPolicy = config.ShutdownKillImmediate

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown must return error on contradictory clean (runtime not clean)")
	}
	reason, retry, n := residualReasonRetryable(t, store, "t1")
	if n != 1 || reason != noticeReasonKillFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want kill_failed/true/1", reason, retry, n)
	}
}

// TestRetryTaskNotices_UnknownDispositionKeepsRetryable 验证后台 notice 重试：
// 未知 disposition 不得经 dispositionToNotice 吞 ok=false 变成空 reason / 非重试，
// 必须保留 retryable debt。
func TestRetryTaskNotices_UnknownDispositionKeepsRetryable(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	name := runtimeSessionName("t1")
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": name, "reason": noticeReasonKillFailed, "retryable": true,
		"cleanupTickets": []interface{}{"tk-old"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc()
	proc.sessions[name] = true
	proc.killResults[name] = process.KillResult{
		SessionKilled: true, Disposition: process.CleanupDisposition("weird"),
		CleanupTickets: []string{"tk-new"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	if err := m.retryTaskNotices(context.Background(), row, entries); err != nil {
		t.Fatalf("retryTaskNotices: %v", err)
	}
	reason, retry, n := residualReasonRetryable(t, store, "t1")
	if n != 1 || reason != noticeReasonKillFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want kill_failed/true/1 (unknown must stay retryable)", reason, retry, n)
	}
}

// TestRetryTaskNotices_ContradictoryCleanKeepsRetryable 验证矛盾 clean（!SessionKilled）
// 在后台重试中不得当成功清债。
func TestRetryTaskNotices_ContradictoryCleanKeepsRetryable(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	name := runtimeSessionName("t1")
	notice := []noticeEntry{{Code: noticeCodeResidual, Data: map[string]interface{}{
		"sessionName": name, "reason": noticeReasonKillFailed, "retryable": true,
		"cleanupTickets": []interface{}{"tk-old"},
	}}}
	store.mutTask("t1", func(r *TaskRow) { r.Notice = encodeNotices(notice) })
	proc := newMockProc()
	proc.sessions[name] = true
	proc.killResults[name] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionClean,
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	row, _ := store.GetTask(context.Background(), "t1")
	entries, _ := parseNotices(row.Notice)
	if err := m.retryTaskNotices(context.Background(), row, entries); err != nil {
		t.Fatalf("retryTaskNotices: %v", err)
	}
	reason, retry, n := residualReasonRetryable(t, store, "t1")
	if n != 1 || reason != noticeReasonKillFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want kill_failed/true/1", reason, retry, n)
	}
	if !proc.sessions[name] {
		t.Fatal("contradictory clean must not drop the live session from retry")
	}
}

// TestRetryOrphanSessions_ContradictoryCleanRetainsDebt 验证孤儿重试：矛盾 clean
// 不得当收敛，tickets 必须保留进下轮。
func TestRetryOrphanSessions_ContradictoryCleanRetainsDebt(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	ghost := "ocdeck-ghost-serve"
	proc.sessions[ghost] = true
	proc.killResults[ghost] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionClean,
		CleanupTickets: []string{"tk-ghost"},
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.orphanMu.Lock()
	m.orphanFailures = []orphanFailure{{sessionName: ghost, tickets: []string{"tk-old"}}}
	m.orphanMu.Unlock()

	if err := m.retryOrphanSessions(context.Background()); err != nil {
		t.Fatalf("retryOrphanSessions: %v", err)
	}
	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanMu.Unlock()
	if len(failures) != 1 {
		t.Fatalf("orphan failures=%d want 1 (contradictory clean is not converged)", len(failures))
	}
}

// commitHasErrProc：runtime NewSession 之后第 2 次 HasSession(runtime) 注入 infra
// 错误（G5-5：命中 commitRuntimeReady 注册前探活，不伤 waitServeReady 的第 1 次）。
type commitHasErrProc struct {
	*mockProc
	created       bool
	postCreateHas int
}

func (p *commitHasErrProc) NewSession(spec process.SessionSpec) error {
	err := p.mockProc.NewSession(spec)
	if err == nil && spec.Name == runtimeSessionName("t1") {
		p.mu.Lock()
		p.created = true
		p.postCreateHas = 0
		p.mu.Unlock()
	}
	return err
}

func (p *commitHasErrProc) HasSession(name string) (bool, error) {
	p.mu.Lock()
	created := p.created
	if created && name == runtimeSessionName("t1") {
		p.postCreateHas++
		n := p.postCreateHas
		p.mu.Unlock()
		if n == 2 {
			return false, errors.New("tmux has-session broken")
		}
	} else {
		p.mu.Unlock()
	}
	return p.mockProc.HasSession(name)
}

func assertReapFailedNotice(t *testing.T, store *mockStore, taskID, ticket string) {
	t.Helper()
	reason, retry, n := residualReasonRetryable(t, store, taskID)
	if n != 1 || reason != noticeReasonReapFailed || !retry {
		t.Fatalf("notice reason=%q retryable=%v n=%d want reap_failed/true/1", reason, retry, n)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	entries, _ := parseNotices(row.Notice)
	found := false
	for _, e := range entries {
		for _, tk := range noticeTickets(e) {
			if tk == ticket {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("reap_failed tickets missing %q in %v", ticket, row.Notice)
	}
}

// TestActivate_CommitProbeHasSessionError_ReapFailedTicketsPersisted 验证 G5-5：
// 注册前 HasSession infra 错误不得本地 kill（否则 reap_failed tickets 被丢）；
// Activate 外层补偿 KillSession 的 reap_failed 必须落库。
func TestActivate_CommitProbeHasSessionError_ReapFailedTicketsPersisted(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
	})
	inner := newMockProc()
	name := runtimeSessionName("t1")
	inner.killResults[name] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-reap-act"},
	}
	proc := &commitHasErrProc{mockProc: inner}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	if err := m.Activate(context.Background(), "t1"); err == nil {
		t.Fatal("Activate must fail when commit probe HasSession errors")
	}
	assertStatus(t, store, "t1", StatusSuspended)
	assertReapFailedNotice(t, store, "t1", "tk-reap-act")
}

// TestRecovery_CommitProbeHasSessionError_ReapFailedTicketsPersisted 验证 G5-5
// Recovery 路径：commit 探活 infra 错误后，外层 confirm/compensation 持久化 reap_failed tickets。
func TestRecovery_CommitProbeHasSessionError_ReapFailedTicketsPersisted(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) {
		r.Status = StatusActive
		r.AnchorSessionID = sql.NullString{String: "sess-existing", Valid: true}
		// D8：Recovery attempt 前加载持久化快照——active fixture 需带合法快照。
		b, _ := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}})
		r.EnvSnapshot = b
	})
	inner := newMockProc()
	name := runtimeSessionName("t1")
	inner.killResults[name] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-reap-rec"},
	}
	proc := &commitHasErrProc{mockProc: inner}
	oc := newMockOC(true)
	oc.sessions = []opencode.Session{{ID: "sess-existing"}}
	startRecoveryFromActive(t, store, proc, oc)
	waitStatusAny(t, store, "t1", 5*time.Second, StatusSuspended)
	assertReapFailedNotice(t, store, "t1", "tk-reap-rec")
}
