package task

// p149_converge_attention_test.go 验证 OpenSpec change sse-active-sessions（oracle
// BLOCKED 修复）：异常收敛清理 attention（clearRuntime→clearAttention）后 MUST 按设计
// D2 矩阵发布 serve_runtime.attention_changed 可见失效（经 LifecycleService
// CommitAttentionChange，NoopPublisher 阶段用记录型 Publisher 断言调用位）。
//
// 发布位（design.md:425-426）：
//   - 持锁矩阵 ②（CAS error）：attention 失效发布在 ② 顶部（保守，先于重读分叉，
//     覆盖 ②a/②b/②c）；
//   - ③a / ③c：发布 attention 失效 + 登记 postCleanup；
//   - ① / ③b：不发布（迁移事件 / 并发转换的提交点承载）；
//   - 锁超时路径：触发令牌仍为当前代且 attention 快照可见 → 登记 preCleanup 成功后
//     发布 attention 失效 + resync（nil-runtime postCleanup 分支无可失效快照，不发布）；
//   - 不可见快照（无 pending permission/question）一律不发（「无可见字段则不发领域事件」）。

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/application/runtime"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/opencode"
)

// attentionCount 返回已发布的 serve_runtime.attention_changed 事件数。
func (p *p147RecordingPublisher) attentionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e.Type == ocdeckevent.TypeServeRuntimeAttentionChanged {
			n++
		}
	}
	return n
}

// p149SeedVisibleAttention 对当前 t1 runtime 应用一个 permission.asked 事件，使 attention
// 快照外部可见（存在 pending permission）。
func p149SeedVisibleAttention(t *testing.T, m *Manager) {
	t.Helper()
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("prereq: runtime missing")
	}
	a := rt.ensureAttentionState()
	a.applyAttentionEvent(opencode.AttentionEvent{
		Kind: opencode.AttentionAsked, Type: opencode.AttentionPermission,
		RequestID: "req-1", SessionID: "sess-1", Permission: "bash", Patterns: []string{"rm -rf"},
	})
	if snap := rt.attentionSnapshot(); len(snap.Permissions) == 0 {
		t.Fatal("prereq: attention snapshot must be visible after seeding")
	}
}

// p149CASNoMatchStore 包装 TaskStore：UpdateTaskStatusConditional 一律返回 !Matched 无错误
//（模拟并发已转走，供 ③ 分叉测试）。
type p149CASNoMatchStore struct {
	TaskStore
}

func (w *p149CASNoMatchStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	return application.TransitionResult{}, nil
}

// TestP149_Converge_CASerr_VisibleAttention_PublishesOnceAndResyncs ②（CAS error）+
// 重读仍 active（②a）+ 清理前 attention 可见 → attention_changed 恰好一次 + resync 一次
//（经全路径 convergeToSuspended 验证可见性捕获）。
func TestP149_Converge_CASerr_VisibleAttention_PublishesOnceAndResyncs(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)

	// 全路径：令牌校验通过 → 捕获可见性 → cleanup（清 attention）→ CAS error → ②。
	m.convergeToSuspended("t1", "sse stream ended", rt.instVersion)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("② with visible attention MUST publish attention_changed exactly once, got %d", n)
	}
	if n := pub.resyncCount(); n != 1 {
		t.Fatalf("② MUST publish resync.requested exactly once, got %d", n)
	}
	// ②a（重读仍 active）：登记 postCleanup。
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST register postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_Converge_CASerr_InvisibleAttention_NoAttentionEvent ② + attention 不可见
//（无 pending permission/question）→ 不发 attention_changed（无可见字段则不发领域事件），
// resync 仍一次。
func TestP149_Converge_CASerr_InvisibleAttention_NoAttentionEvent(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	// 不 seed attention：快照不可见。

	m.convergeToSuspended("t1", "sse stream ended", rt.instVersion)

	if n := pub.attentionCount(); n != 0 {
		t.Fatalf("② with invisible attention MUST NOT publish attention_changed, got %d", n)
	}
	if n := pub.resyncCount(); n != 1 {
		t.Fatalf("② MUST still publish resync once, got %d", n)
	}
}

// TestP149_Matrix_W3a_VisibleAttention_InvalidatesAndRegisters ③a（!Matched 无错误 +
// 重读仍 active）+ 可见 attention → 发布失效一次 + 登记 postCleanup，不 resync。
func TestP149_Matrix_W3a_VisibleAttention_InvalidatesAndRegisters(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub := p147ManagerWithRecordingPublisher(t, &p149CASNoMatchStore{TaskStore: store})
	// 构造触发令牌 + tombstone（runtime 已清，postCleanup 语义）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	tok := rt.instVersion
	m.clearRuntime("t1")

	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, true, agentStatusDelta{}, nil)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("③a with visible attention MUST publish attention_changed exactly once, got %d", n)
	}
	if n := pub.resyncCount(); n != 0 {
		t.Fatalf("③a MUST NOT publish resync, got %d", n)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup || entry.Token != tok {
		t.Fatalf("③a MUST register postCleanup debt for trigger token, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_Matrix_Committed_NoAttentionEvent ①（CAS 命中 committed）→ 即使清理前可见
//（visible=true）也不发 attention_changed（迁移事件承载），不 resync，债务 CAD。
func TestP149_Matrix_Committed_NoAttentionEvent(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub := p147ManagerWithRecordingPublisher(t, store)
	tok := p147SeedPostCleanupDebt(t, m)

	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, true, agentStatusDelta{}, nil)

	assertStatus(t, store, "t1", StatusSuspended)
	if n := pub.attentionCount(); n != 0 {
		t.Fatalf("① committed MUST NOT publish attention_changed, got %d", n)
	}
	if n := pub.resyncCount(); n != 0 {
		t.Fatalf("① committed MUST NOT publish resync, got %d", n)
	}
	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("① committed MUST compare-and-delete debt")
	}
}

// TestP149_Matrix_W2b_VisibleAttention_PublishesViaTopOfBranch2 ②b（CAS error + 重读
// 非 active）+ 可见 attention → ② 顶部已发布失效：attention 恰好一次 + resync 一次 +
// 债务 CAD。钉住 design.md:425「②CAS error → 保守发布…然后按重读分叉」的发布位置
//（发布先于重读，②b 属 ② 分支）。
func TestP149_Matrix_W2b_VisibleAttention_PublishesViaTopOfBranch2(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 重读非 active → ②b
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	tok := p147SeedPostCleanupDebt(t, m)

	m.convergeCommitCAS(context.Background(), "t1", convergeDebtPostCleanupReason, tok, true, agentStatusDelta{}, nil)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("②b gets the ②-top conservative attention publish (design.md:425), want exactly once, got %d", n)
	}
	if n := pub.resyncCount(); n != 1 {
		t.Fatalf("②b MUST publish resync exactly once, got %d", n)
	}
	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("②b MUST compare-and-delete debt")
	}
}

// TestP149_LockTimeout_VisibleAttention_InvalidatesAndRegistersPreCleanup 锁超时路径：
// 触发令牌仍为当前代 + attention 可见 → 登记 preCleanup 成功后发布 attention 失效一次 +
// resync 一次。
func TestP149_LockTimeout_VisibleAttention_InvalidatesAndRegistersPreCleanup(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub := p147ManagerWithRecordingPublisher(t, store)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)

	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", rt.instVersion)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("lock-timeout with visible attention MUST publish attention_changed exactly once, got %d", n)
	}
	if n := pub.resyncCount(); n != 1 {
		t.Fatalf("lock-timeout MUST publish resync exactly once, got %d", n)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePreCleanup || entry.Token != rt.instVersion {
		t.Fatalf("lock-timeout MUST register preCleanup debt for trigger token, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_LockTimeout_StaleToken_NoPublish 触发令牌非当前代（stale）→ 不登记、
// 不发布 attention、不 resync。
func TestP149_LockTimeout_StaleToken_NoPublish(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub := p147ManagerWithRecordingPublisher(t, store)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)

	m.onConvergeLockTimeout("t1", "stale callback", runtime.InstVersion("01724000000123-stale0"))

	if n := pub.attentionCount(); n != 0 {
		t.Fatalf("stale trigger MUST NOT publish attention_changed, got %d", n)
	}
	if n := pub.resyncCount(); n != 0 {
		t.Fatalf("stale trigger MUST NOT publish resync, got %d", n)
	}
	if _, ok := m.runtimeRegistry.Get("t1"); ok {
		t.Fatal("stale trigger MUST NOT register debt")
	}
}

// TestP149_LockTimeout_RepeatedSameTokenTimeout_SinglePublish 同令牌重复超时回调
// 双发防护（oracle 第三轮 BLOCKER）：两次同触发令牌超时登记、快照未变（仍可见）→
// 第二次登记 claimed=false（标记已 true）→ 全序列恰好一次 attention_changed。
// resync 按设计每次成功登记各发一次（保守，不属本测试约束）。
func TestP149_LockTimeout_RepeatedSameTokenTimeout_SinglePublish(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	m, pub := p147ManagerWithRecordingPublisher(t, store)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)
	tok := rt.instVersion

	// 第一次超时回调：插入 + claimed=true → 发布一次。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)
	// 第二次同令牌超时回调（如 watcher 与 SSE 重复触发）：合并 + 已 true → 不再发布。
	m.onConvergeLockTimeout("t1", "infra error; converge lock wait timed out", tok)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("repeated same-token timeout MUST publish attention_changed exactly once, got %d", n)
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || !entry.AttentionInvalidated || entry.Token != tok {
		t.Fatalf("debt must stay registered with flag=true for trigger token, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_HeldLockConverge_AfterTimeout_SinglePublish 持锁路径消费债务标记（oracle
// 第四轮 BLOCKER）：超时登记已发布失效（同令牌标记 true，worker 未及运行）→ 同令牌
// 持锁回调先于 worker 取得锁，矩阵命中发布叶（②，CAS error）→ 全序列仍恰好一次。
func TestP149_HeldLockConverge_AfterTimeout_SinglePublish(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)
	tok := rt.instVersion

	// 超时回调：插入 + claimed=true → 发布一次，债务标记 AttentionInvalidated=true。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)
	// 同令牌持锁回调（锁空闲，先于 worker）：捕获可见性时消费登记标记 → 矩阵②不再发布。
	m.convergeToSuspended("t1", "sse stream ended", tok)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("timeout + held-lock converge for same token MUST publish attention_changed exactly once, got %d", n)
	}
	// 持锁路径已完成 cleanup（runtime 已清）+ ②a（重读仍 active）→ 债务保留 postCleanup。
	if m.getRuntime("t1") != nil {
		t.Fatal("held-lock converge MUST have cleaned the runtime")
	}
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup || entry.Token != tok {
		t.Fatalf("②a MUST keep postCleanup debt for trigger token, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_TOCTOU_TimeoutClaimsDuringHeldLockCleanupWindow_SinglePublish TOCTOU 双发
// 防护（oracle 第五轮 BLOCKER）：持锁回调 A 清理前捕获 visible=true 且当时无标记债务；
// A 的长清理窗口内，无需任务锁的超时回调 B 认领并发布；A 到达矩阵发布点时携带的是
// 过期捕获值——发布点原子 claim（唯一权威）抑制二次发布，全序列恰好一次。
//
// 确定性注入（无 sleep/时序依赖）：用 convergeCommitCAS 直接进入「A 已完成捕获与
// 清理、正要进入矩阵」的时间点（attentionVisible 参数即 A 的捕获值）；此前先完整走
// B 的超时路径（onConvergeLockTimeout：register → claim → publish），模拟窗口内的
// 并发认领。wrapStatusErr 强制 A 的矩阵命中 ②（发布叶在 ② 顶部）。
func TestP149_TOCTOU_TimeoutClaimsDuringHeldLockCleanupWindow_SinglePublish(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)
	tok := rt.instVersion

	// B：持锁回调 A 清理窗口内的同令牌超时回调（无需任务锁）→ 认领 + 发布一次。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)

	// A：携带过期捕获值（visible=true，捕获时无标记）到达矩阵发布点 → claim 失败，
	// 不得二次发布。
	m.convergeCommitCAS(context.Background(), "t1", "sse stream ended", tok, true, agentStatusDelta{}, nil)

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("TOCTOU: timeout claim during held-lock cleanup window MUST yield exactly one attention_changed, got %d", n)
	}
	// ②a（重读仍 active）：债务保留 postCleanup 且标记为 true（B 发布过）。
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup || !entry.AttentionInvalidated {
		t.Fatalf("②a MUST keep flagged postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_Worker_PreCleanup_TimeoutInvalidationNotRepublished 双发防护（oracle BLOCKER）：
// 超时路径发布失效并在债务项标记 AttentionInvalidated → worker preCleanup 重新观察
// runtime attention 虽仍可见（cleanup 前），MUST NOT 对同一事实二次发布。
// 全序列（timeout 登记 → worker preCleanup 清理 → CAS error → ② 顶部）合计恰好一次。
func TestP149_Worker_PreCleanup_TimeoutInvalidationNotRepublished(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)
	tok := rt.instVersion

	// 超时登记：可见 → 发布失效一次 + 标记债务项。
	m.onConvergeLockTimeout("t1", "serve exit; converge lock wait timed out", tok)
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || !entry.AttentionInvalidated {
		t.Fatalf("timeout registration MUST record AttentionInvalidated=true, got %+v (ok=%v)", entry, ok)
	}

	// worker preCleanup：runtime attention 快照仍可见，但登记已标记 → 矩阵②不再发布。
	m.processConvergeDebts(context.Background())

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("full timeout→worker sequence MUST publish attention_changed exactly once, got %d", n)
	}
	// CAS error → ②a（重读仍 active）：债务保留为 postCleanup。
	if entry, ok := m.runtimeRegistry.Get("t1"); !ok || entry.Phase != runtime.DebtPhasePostCleanup {
		t.Fatalf("②a MUST keep postCleanup debt, got %+v (ok=%v)", entry, ok)
	}
}

// TestP149_Worker_PreCleanup_UnmarkedDebtStillPublishes 正向钉住：登记未标记失效
//（AttentionInvalidated=false，如超时时 attention 不可见、随后 SSE 又到达新事件使快照
// 可见——该可见事实从未被失效过）→ worker preCleanup 正常发布一次（修复不得变成
// worker 永不发布）。
func TestP149_Worker_PreCleanup_UnmarkedDebtStillPublishes(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	errStore := wrapStatusErr(store, errors.New("db: status commit failed"))
	m, pub := p147ManagerWithRecordingPublisher(t, errStore)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	p149SeedVisibleAttention(t, m)
	tok := rt.instVersion

	// 直接登记未标记的 preCleanup 债务（不经超时发布流程）。
	if registered, _ := m.runtimeRegistry.RegisterIfCurrent("t1", tok, runtime.DebtPhasePreCleanup, &tok, false); !registered {
		t.Fatal("precondition register failed")
	}

	m.processConvergeDebts(context.Background())

	if n := pub.attentionCount(); n != 1 {
		t.Fatalf("unmarked debt + visible attention MUST publish attention_changed once via matrix ②, got %d", n)
	}
}
