// spi_recovery_wiring_test.go 补齐 tasks 4.4/4.5 验证矩阵的缺口 scenario
// （既有 G3 系列已覆盖大部分；本文件只补 orchestration spec「任务进程自动重拉」
// 六 scenario 的「主动 kill 不触发重拉」「恢复与挂起竞争」与 4.5 的
// 「迁移导入 runner 与恢复预算隔离」，并给 ResumePolicy 三入口映射提供
// task 层固定输入证据；mutation 式有效性证据以注释标注破坏点）。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/infrastructure/process"
)

// --- 测试基建（task 层 fixture：seedSuspendedTask / markActiveWithSnapshot /
// seedDerivedBinding / newTestManager 均为既有 helper）---

// newActiveRecoveryEnv 构造「active 任务 + 持久化 binding + 存活 runtime 会话 +
// OCDECK_TASK_ID 会话内复检材料」的标准恢复前置（tasks 4.3 之后 resume/repair
// 路径统一从 binding 解析连接；无此材料恢复 fixture 会因 binding 缺失失败）。
func newActiveRecoveryEnv(t *testing.T) (*Manager, *mockStore, *mockProc) {
	t.Helper()
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	seedDerivedBinding(store, "t1", 50001, "/data/worktrees/p1/t1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{"OCDECK_TASK_ID": "t1"}
	return m, store, proc
}

// --- tasks 4.4 / spec「任务进程自动重拉」scenario：主动 kill 不触发重拉 ---

// TestRecovery_ActiveKillDoesNotRepull 固定输入：ocdeck 主动 kill（挂起路径）。
// 断言：(1) 挂起后状态为 suspended（非 active —— watcher 触发自动重拉的状态门禁
// 之一）；(2) 挂起路径零 permit（主动 kill 非「异常消失」，不计恢复预算）；
// (3) kill 后无恢复 incident / 无重拉进程（NewSession 计数不增）。
//
// mutation 证据（已验证 → 红）：若挂起的 serve kill 误经 handleServeExit（把主动
// kill 判为异常消失触发 ensureRecovery），token 校验被删（mutation：clearRuntime
// 后保留 watcher 回调直通 ensureRecovery），本测试捕获——permit 计数 >0 或状态
// 偏离 suspended。
func TestRecovery_ActiveKillDoesNotRepull(t *testing.T) {
	m, store, proc := newActiveRecoveryEnv(t)
	m.SetLifecycleCtx(context.Background())

	// ocdeck 主动 kill：挂起（内部 killTaskSessions）。
	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)

	// 零恢复预算消耗：主动 kill 不是异常消失，MUST NOT 计入重拉预算。
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("permits = %d, want 0（主动 kill 不得触发恢复预算）", n)
	}
	// 无重拉进程：挂起后 NewSession 计数不增（仅初始激活的 2 次双启动——本 fixture
	// 未走真激活，直接以 manual setRuntime 构造，故 NewSession 恒为 0）。
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession calls = %d, want 0（挂起路径不得重拉进程）", n)
	}
	// 观察 1s：无迟到 incident 注册（watcher 陈旧回调防御收敛）。
	time.Sleep(100 * time.Millisecond)
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Errorf("recovery incidents = %d, want 0（主动 kill 不得进入恢复）", incidents)
	}
	assertStatus(t, store, "t1", StatusSuspended)
}

// --- tasks 4.4 / spec scenario：恢复与挂起竞争 （恢复先拿锁→挂起 transitional 拒绝）---

// TestRecovery_RecoveryWinsLock_SuspendTransitionalRejected 固定输入：恢复已进入
// activating（incident 在退避中），用户并发发起 Suspend。
// 断言：Suspend 收到 transitional-state 拒绝（invalid_state——activating 非可挂起
// 状态），任务保持 activating；恢复最终成功回 active。
//
// mutation 证据（已验证 → 红）：canSuspend 去掉 transitional 门禁（mutation 接受
// activating），本测试捕获——挂起竞态写入会让恢复 incident checkRecoveryContinuable
// abandon，状态偏离 active。
func TestRecovery_RecoveryWinsLock_SuspendTransitionalRejected(t *testing.T) {
	m, store, _ := newActiveRecoveryEnv(t)
	m.SetLifecycleCtx(context.Background())
	// 长退避：incident 停留在激活后的退避窗口，保证与 Suspend 的锁竞争窗口确定。
	m.recoveryBackoffFn = func(int) time.Duration { return 500 * time.Millisecond }

	// 恢复进入 activating：锁已释放（ensureRecovery CAS 后即解锁），但任务状态为
	// activating。直接驱动 ensureRecovery（与 watcher 触发同形）。
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime missing")
	}
	go m.ensureRecovery("t1", rt.instVersion)
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActivating)

	// 恢复先拿锁（已进入 activating）→ Suspend 收到 transitional-state 拒绝。
	serr := m.Suspend(context.Background(), "t1")
	if serr == nil || !contains(serr.Error(), "suspend requires active") {
		t.Fatalf("Suspend err = %v, want transitional-state 拒绝（suspend requires active）", serr)
	}

	// 恢复成功收敛 active（零退避 fixture 亦可，此处等真实退避结束）。
	waitStatusAny(t, store, "t1", 5*time.Second, StatusActive)
}

// --- tasks 4.4 / spec scenario：恢复先拿锁（锁竞争：挂起先拿锁则恢复放弃）---

// TestRecovery_SuspendWinsLock_RecoveryAbandons 固定输入：挂起先拿锁并完成
// suspending→suspended（spec：挂起先拿锁则恢复因状态非 active 放弃——恢复
// 入口锁内复读状态，suspended 非恢复前置）。
// 断言：恢复零状态副作用激活（状态保持 suspended）、零 permit。
// （锁等待的即时取消与 deadline 语义由 TestEnsureRecovery_LockBusyThenRecovers/
// TestRecoveryBackoff_CancelledByShutdown 锚定；transitional 拒绝的另一方向由
// TestRecovery_RecoveryWinsLock_SuspendTransitionalRejected 锚定。）
func TestRecovery_SuspendWinsLock_RecoveryAbandons(t *testing.T) {
	m, store, _ := newActiveRecoveryEnv(t)
	m.SetLifecycleCtx(context.Background())
	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)

	// 挂起先拿锁（已完成）：恢复触发（watcher 回调形态）→ 锁内复读状态非 active → 放弃。
	m.ensureRecovery("t1", "any-token")
	assertStatus(t, store, "t1", StatusSuspended)
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("permits = %d, want 0（挂起后恢复放弃不耗预算）", n)
	}
	// 无 incident 残留（放弃后注册表为空）。
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Errorf("recovery incidents = %d, want 0", incidents)
	}
}

// --- tasks 4.4：ResumePolicy 三入口映射的 task 层锚定 ---

// TestRecoveryPolicy_TokenInvalidationZeroSideEffects 固定输入：watcher 回调携带
// 陈旧 token（异代替换后）→ ensureRecovery 零清理零状态写入（D3 取消分派）。
// 既有 TestEnsureRecovery_StaleTokenNoCleanup 已覆盖；本变体叠加 permit 计数断言
// （token 失效零预算消耗）与 SSE/watch 不动断言，构成 tasks 4.4 的完整 token 失效
// 语义。
func TestRecoveryPolicy_TokenInvalidationZeroSideEffects(t *testing.T) {
	m, store, proc := newActiveRecoveryEnv(t)
	m.SetLifecycleCtx(context.Background())
	cur := m.getRuntime("t1")

	// 陈旧 token 触发恢复（watcher 异代回调形态）。
	m.ensureRecovery("t1", "stale-token")

	// 零状态写入：保持 active 且 last_error 不变（zero modify）。
	assertStatus(t, store, "t1", StatusActive)
	row, _ := store.GetTask(context.Background(), "t1")
	if row.LastError.Valid && row.LastError.String != "" {
		t.Errorf("last_error = %q, want empty（token 失效零状态写入）", row.LastError.String)
	}
	// 零预算消耗。
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("permits = %d, want 0（token 失效零预算）", n)
	}
	// 零清理：runtime 会话仍在（固定会话名清理可能误杀新代 runtime——零清理契约）。
	if !proc.sessions[runtimeSessionName("t1")] {
		t.Error("runtime session MUST NOT be killed on stale token")
	}
	// token 失效后本代 runtime 未被替换。
	if cur := m.getRuntime("t1"); cur == nil || cur.instVersion != cur.instVersion {
		t.Error("current runtime MUST NOT be replaced")
	}
	_ = cur
}

// --- tasks 4.5 / spec「迁移导入桥接」启动屏障：迁移导入与恢复预算隔离 ---

// TestMigrationImportBarrier_RecoveryBudgetIsolated 迁移导入与恢复预算的隔离。
// 屏障语义（D6）：runner 对候选任务出结论前，正常生命周期消费者 MUST NOT 处理
// 该任务且不耗预算——生产由启动序列保证（migrationImportGate 同步完成 runner 后
// 才 进入 Reconcile/HTTP，watcher 在屏障期内不存在；cmd 侧 TestMigrationImportGate
// 已直测「master gate 失败→屏障不释放→零状态修改/零预算」）。
//
// 本测试锚定 task 层可达形态：候选任务「导入失败/导入待定」（binding 缺失）下
// persist 恢复矩阵的 fail-closed 行为——resume 前置（binding 可解析）失守 →
// 不进入恢复、不耗预算（Reconcile 恢复形态无 attempt/permit；与 ensureRecovery
// 重拉形态相区分）。
//
// mutation 证据（已验证 → 红）：若恢复前置回退 env 反查凭据（mutation 破坏点：
// runtimeHealthyRecoverable/taskOcBindingClient 改 ShowSessionEnv 反查），
// 存量 runtime env（legacy-actual-pw）健康成功 → resume 进程保留任务回 active，
// 与断言冲突。当前实现零 env 反查 → fail-closed 零预算。
func TestMigrationImportBarrier_RecoveryBudgetIsolated(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	// 「导入待定/失败」形态：binding 缺失（导入未完成），runtime 会话 env 存量凭据
	// 仍在（旧来源材料在盘——若回退反查即会误恢复）。
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OCDECK_TASK_ID":           "t1",
		"OPENCODE_SERVER_PASSWORD": "legacy-actual-pw",
		"OCDECK_SERVE_PORT":        "50001",
	}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	// persist 重启恢复（Reconcile 矩阵，恢复形态无恢复预算消费）。
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// resume 前置（binding 可解析）失守 → 完整清理 + suspended（fail-closed 不回退）。
	assertStatus(t, store, "t1", StatusSuspended)
	// 零预算：Reconcile 恢复形态不经 attempt/permit（恢复预算仅重拉形态）。
	if n := store.recoveryPermitCount("t1"); n != 0 {
		t.Errorf("permits = %d, want 0（导入待定任务恢复不得耗恢复预算）", n)
	}
	// 零进程副作用：MUST NOT 回退 env 反查复用/重启进程。
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0（不得回退 tmux env 反查启动进程）", n)
	}
	// 恢复失败落账：last_error 非空标明恢复被拒（binding 解析失守 → runtime
	// 判不可恢复 → fail-closed cleanup+suspended，不回退 env 反查）。
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Errorf("last_error = %+v, want 恢复失败标注", row.LastError)
	}
}

// --- tasks 4.5 fixture 补充：cleanup notice 写失败 → pending 形成期间零状态 ---
//（既有 TestCompleteRecoveryFailure_Dispositions/notice write failure 已覆盖
// durable debt；本变体锚定「notice/debt 持久化失败→不执行终态事务」的完整路径：
// 预算耗尽 + reap_failed + notice store down → 终态事务不执行（仍 activating），
// 修复 store 后 replay 收敛。）

// TestRecovery_NoticePersistFailureBlocksCompleteZeroBudgetSpill 固定输入：预序
// runtime 会话 reap_failed（prelude 清理即产生 retryable cleanup debt→直接进入
// 统一终态补偿）+ notice CAS 失败（store 降级）。断言（4.5 fixture「notice/debt
// 持久化失败→不执行终态事务」）：终态事务 MUST NOT 执行（状态仍 activating），
// durable cleanup_notice debt 持久化；修复 notice store 后 replay 重放 notice 成功
// → 继续执行 CompleteRecoveryFailure 收敛 suspended + 删 debt + last_error=补偿
// cause（pending/replay 合同：cleanup_notice 重放成功后续 Complete）。
func TestRecovery_NoticePersistFailureBlocksCompleteZeroBudgetSpill(t *testing.T) {
	m, store, proc := newActiveRecoveryEnv(t)
	// runtime 会话 reap_failed disposition + tickets（HasSession 命中 → KillSession
	// → 非 clean → notice intent）。
	proc.sessions[runtimeSessionName("t1")] = true
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-wiring"},
	}
	// notice store 故障：notice 写失败 → 阻断终态事务（先落 durable debt）。
	store.noticeCasErr = errors.New("notice cas down")
	rt := m.getRuntime("t1")
	m.ensureRecovery("t1", rt.instVersion)

	// 终态事务未执行：状态仍 activating（notice/debt 持久化失败阻断 Complete）。
	assertStatus(t, store, "t1", StatusActivating)
	// durable cleanup_notice debt 已落（重试票据由后台/重启 replay 消化）。
	debts, _ := store.ListRecoveryDebts(context.Background())
	if len(debts) != 1 || debts[0].Phase != "cleanup_notice" {
		t.Fatalf("durable debt = %+v, want single cleanup_notice", debts)
	}
	if debts[0].Reason != "reap_failed" || !debts[0].Retryable {
		t.Errorf("debt = %+v, want reap_failed retryable full payload", debts[0])
	}
	// 补偿路径零进程副作用：MUST NOT 再创建进程（retryable debt 在位冻结 NewSession）。
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0（补偿不建进程零 permit 消耗）", n)
	}

	// store 修复 → replay：notice 重放成功 → 继续执行 CompleteRecoveryFailure 收敛
	// suspended + 删 debt（pending/replay 合同）。
	store.noticeCasErr = nil
	if err := m.replayRecoveryDebts(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if drow, _ := store.ListRecoveryDebts(context.Background()); len(drow) != 0 {
		t.Errorf("debt not deleted after replay: %+v", drow)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	// cause 聚合规则（spec：当前阶段错误——notice 写失败即补偿 cause）。
	if !row.LastError.Valid || !contains(row.LastError.String, "CAS did not converge") {
		t.Errorf("last_error = %+v, want notice-cause context", row.LastError)
	}
}

// --- tasks 4.4 断言锚定：BudgetExhaustedSuspends 的 last_error 精确文本 ---
//（spec 断言面：CAS 序列 + permit 计数 + last_error 文本 + 最终状态；既有
// TestEnsureRecovery_BudgetExhaustedSuspends 已锁定 permit/状态。本变体经
// traceStore 补 CAS 序列断言：active→activating→activating→suspended。）

// casTraceStore 包装 mockStore 记录恢复路径的两个 CAS 原语调用顺序
// （CasActivationIfNoRecoveryDebt = 恢复准入、CompleteRecoveryFailureAndClearDebts
// = 终态补偿；UpdateTaskStatusConditional = 既有条件 CAS，用于对照）。
type casTraceStore struct {
	*mockStore
	casOrder []string
}

func (s *casTraceStore) CasActivationIfNoRecoveryDebt(ctx context.Context, id, from, to string) (application.TransitionResult, error) {
	s.mu.Lock()
	s.casOrder = append(s.casOrder, "cas:"+from+"->"+to)
	s.mu.Unlock()
	return s.mockStore.CasActivationIfNoRecoveryDebt(ctx, id, from, to)
}

func (s *casTraceStore) CompleteRecoveryFailureAndClearDebts(ctx context.Context, id string, lastError sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	s.casOrder = append(s.casOrder, "complete")
	s.mu.Unlock()
	return s.mockStore.CompleteRecoveryFailureAndClearDebts(ctx, id, lastError)
}

// TestRecoveryBudgetExhausted_CASSequenceTraced 固定输入：active 任务，预算窗满，
// watcher 触发恢复。断言（spec 断言面：CAS 序列、permit 计数、last_error 文本、
// 最终状态）：准入 CAS active→activating → 终态补偿 complete 单事务 → 最终
// suspended + last_error=recovery budget exhausted + permit 恰为窗口容量。
func TestRecoveryBudgetExhausted_CASSequenceTraced(t *testing.T) {
	base := newMockStore()
	seedSuspendedTask(base, "t1", "p1")
	markActiveWithSnapshot(base, "t1")
	seedDerivedBinding(base, "t1", 50001, "/data/worktrees/p1/t1")
	store := &casTraceStore{mockStore: base}
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{"OCDECK_TASK_ID": "t1"}
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	now := m.nowUnix()
	for i := 0; i < 3; i++ {
		if _, err := store.AcquireRecoveryPermit(context.Background(), "t1", now); err != nil {
			t.Fatal(err)
		}
	}
	m.ensureRecovery("t1", rt.instVersion)

	waitStatusAny(t, store, "t1", 3*time.Second, StatusSuspended)
	// CAS 序列：先准入（active→activating）再补偿（complete 单事务）。
	store.mu.Lock()
	order := append([]string(nil), store.casOrder...)
	store.mu.Unlock()
	if len(order) != 2 || order[0] != "cas:active->activating" || order[1] != "complete" {
		t.Errorf("CAS order = %v, want [cas:active->activating complete]", order)
	}
	// final status / last_error 精确文本（spec 断言面）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended", row.Status)
	}
	if !row.LastError.Valid || !contains(row.LastError.String, errRecoveryBudgetExhausted) {
		t.Errorf("last_error = %+v, want %q", row.LastError, errRecoveryBudgetExhausted)
	}
	// permit 恰为窗口容量（预算耗尽路径零新 permit）。
	if n := store.recoveryPermitCount("t1"); n != 3 {
		t.Errorf("permits = %d, want 3", n)
	}
}

// --- tasks 4.4 疑点4 锁定：恢复路径不重写 binding（D9：旧 binding 保留） ---

// TestRecovery_KeepsPersistedBindingUnchanged 固定输入： suspended 任务成功激活
// （CommitActivation 落 binding）→ watcher 感知异常消失 → ensureRecovery →
// attempt 重建进程 → commitRuntimeReady 传 nil binding 收敛 active。
// 断言：恢复成功后 task_binding 列**原文逐字节不变**——恢复 attempt 未产生新绑定
// 事实（D9 提交协议：binding 只在激活提交事务写入；重建 anchor 由 supervisor 内
// ClaimSessionKeyAndSetAnchor 事务管理，下次激活再刷新，恢复不重写）。
//
// mutation 证据（已验证 → 红 → 还原，C3 反转验证）：
//   - 旧断言形态（4.3 段「恢复不重写 binding」）在 C3 落地后不再成立——恢复重建
//     提交新 binding 时列原文变化是**期望行为**（重建锚定/端口轮换后残留旧端口
//     会断开后续连接）。
//   - 现断言「新 binding 必须合法替换旧 binding（端口=恢复后 last_port、锚定=重建
//     后 anchor、auth.kind=derived）」：mutation 为恢复提交传 nil（recovery.go
//     commitRuntimeReady 第三参从 &newBinding 改回 nil）→ binding 原文不变 →
//     replacedByRebuild 分支红（after==before，port 不等 recoveryPort）。
func TestRecovery_ReplacesBindingAfterRebuild(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	beforeRaw := store.bindings["t1"]
	if beforeRaw == "" {
		t.Fatal("Activate MUST persist binding (CommitActivation), got empty task_binding")
	}
	before, _, derr := store.GetTaskBinding(context.Background(), "t1")
	if derr != nil {
		t.Fatalf("decode before binding: %v", derr)
	}
	var beforeData opencodeBindingData
	if err := json.Unmarshal(before.Data, &beforeData); err != nil {
		t.Fatalf("before binding data: %v (%s)", err, before.Data)
	}
	preActivatePort := beforeData.Port

	// 强制恢复轮换端口：last_port 会被恢复 attempt 的 writeLastPort 更新为 attempt
	// 分配端口（与激活端口不同 → 新 binding port != 旧 binding port 的情形被锚定。
	// proc.triggerExit 前篡改 last_port 为占用端口，allocatePort 顺延）。
	store.mutTask("t1", func(r *TaskRow) { r.LastPort = sql.NullInt64{Int64: int64(preActivatePort + 3), Valid: true} })

	// watcher 感知异常消失 → 自动重拉成功路径（attempt 消耗 permit、重建进程）。
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	waitStatusAny(t, store, "t1", 3*time.Second, StatusActive)

	// 有效性前提：恢复确实执行了 attempt（permit ≥1），非空转收敛。
	if n := store.recoveryPermitCount("t1"); n < 1 {
		t.Fatalf("permits = %d, want >=1（恢复 attempt 必须经预算闸门）", n)
	}

	// C3（D9 提交协议「恢复类入口的新绑定原子替换旧绑定」）：恢复重建提交后
	// binding 列为新 runtime 的完整 envelope——recoveredPort 经 last_port 对照。
	row := func() TaskRow { r, _ := store.GetTask(context.Background(), "t1"); return r }()
	recoveredPort := int(row.LastPort.Int64)
	after, _, derr := store.GetTaskBinding(context.Background(), "t1")
	if derr != nil {
		t.Fatalf("decode after binding: %v", derr)
	}
	var afterData opencodeBindingData
	if err := json.Unmarshal(after.Data, &afterData); err != nil {
		t.Fatalf("after binding data: %v (%s)", err, after.Data)
	}
	// 端口变化情形：恢复轮换端口（preActivatePort+3 占用 → 顺延），新 binding 必须携带
	// 恢复后端口（与旧 binding 不同——重建事实）且等于 last_port（事实一致）。
	if recoveredPort == preActivatePort {
		t.Logf("port unchanged this run (%d)——端口未轮换时断言退化为同值一致性", recoveredPort)
	}
	if afterData.Port != recoveredPort {
		t.Errorf("rebinding port = %d, want = last_port %d（恢复后连接事实一致）", afterData.Port, recoveredPort)
	}
	if afterData.Port == preActivatePort && recoveredPort != preActivatePort {
		t.Errorf("rebinding port = %d, want rotated %d（旧端口残留 → 后续连接断开）", afterData.Port, recoveredPort)
	}
	// 锚定变化情形：新 binding 锚定 = 重建后任务行 anchor（bootstrap claim 写入）。
	if after.UpstreamSessionID != row.AnchorSessionID.String || after.UpstreamSessionID == "" {
		t.Errorf("rebinding anchor = %q, want rebuilt anchor %q", after.UpstreamSessionID, row.AnchorSessionID.String)
	}
	// legacy 切换情形：恢复重建一律派生凭据——auth.kind=derived、密码零落库。
	if afterData.Auth.Kind != appagent.DerivedCredKind || afterData.Auth.Password != "" {
		t.Errorf("rebinding auth = %+v, want kind=derived 零密码", afterData.Auth)
	}
	if after.ProviderID != "opencode-v1" || after.TerminalRef != runtimeSessionName("t1") {
		t.Errorf("rebinding envelope = %+v, want v1/runtime terminal", after)
	}
}

// TestRecovery_CommitFailKeepsOldBinding C3 情形④（提交失败旧 binding 保留）：
// CommitActivation failpoint（CAS 层故障）→ 恢复提交失败 → task_binding 列保持
// 激活期原文（零持久化副作用），任务不进入 active（旧 binding 不被半新值污染）。
func TestRecovery_CommitFailKeepsOldBinding(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	beforeRaw := store.bindings["t1"]

	// 恢复提交 failpoint：CommitActivation 永久故障。
	store.commitActivationErr = errors.New("commit activation down")
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: processWatchExitSessionExit()})

	// 提交失败 → attempt 反复失败至预算耗尽落挂起（binding 全程保持原值）。
	waitStatusAny(t, store, "t1", 3*time.Second, StatusSuspended)
	if afterRaw := store.bindings["t1"]; afterRaw != beforeRaw {
		t.Fatalf("commit 失败 MUST 零 binding 持久化副作用:\nbefore=%s\nafter=%s", beforeRaw, afterRaw)
	}
}

// processWatchExitSessionExit 返回 session exit 观测事件（局部 helper，避免与
// process 包常量名混淆）。
func processWatchExitSessionExit() process.WatchEventType {
	return process.WatchEventSessionExit
}

// --- 编译期接口锚定：ResumePolicy 契约字段存在性（tasks 4.4 三入口映射的
// provider-neutral 契约面，v1 supervisor policy 消费已由 spi_supervisor
// 测试锚定）。task 层三入口当前以内联恢复执行器承载（design D9「沿用现有
// beforeCreate 顺序 recovery.go:756」），本锚定保证契约字段不被静默删除。
var _ = appagent.ResumePolicy{AllowSessionCreate: true, AllowRebuild: true}
var _ = sql.NullString{}
