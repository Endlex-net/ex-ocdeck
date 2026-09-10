package task

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- ai-auto 判定消费者测试基建（task-permission-mode D4/D9） ---

// fakeJudge 记录 Judge 输入并返回脚本化结果；block 非 nil 时阻塞直到 release 或 ctx 取消
//（ctx 取消返回 uncertain + err，模拟真实 Judge 的 10s 预算中断；onCtxDone/drain 供
// 并发停止测试观测 cancel 与模拟收尾耗时）。
type fakeJudge struct {
	mu        sync.Mutex
	calls     []PermissionJudgeInput
	verdict   PermissionVerdict
	err       error
	block     <-chan struct{}
	entered   chan struct{}
	returns   chan struct{} // 每次 Judge 返回前信号（容量足够，非阻塞发送）
	onCtxDone func()        // ctx.Done 分支回调（观测 stop 已 cancel）
	drain     time.Duration // ctx.Done 分支返回前的模拟收尾耗时
}

func newFakeJudge(verdict PermissionVerdict) *fakeJudge {
	return &fakeJudge{
		verdict: verdict,
		entered: make(chan struct{}, 16),
		returns: make(chan struct{}, 16),
	}
}

func (f *fakeJudge) Judge(ctx context.Context, in PermissionJudgeInput) (PermissionVerdict, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			if f.onCtxDone != nil {
				f.onCtxDone()
			}
			if f.drain > 0 {
				time.Sleep(f.drain)
			}
			f.signalReturn()
			return PermissionVerdictUncertain, ctx.Err()
		}
	}
	f.signalReturn()
	return f.verdict, f.err
}

func (f *fakeJudge) signalReturn() {
	select {
	case f.returns <- struct{}{}:
	default:
	}
}

func (f *fakeJudge) judgeCalls() []PermissionJudgeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PermissionJudgeInput(nil), f.calls...)
}

// gatedPermOC 匿名内嵌 *mockOC，仅覆写 ListPermissions：进入时发 entered 信号并阻塞
// 直到 release（模拟后台对账 GET 在途），放行后转回 inner 逻辑。
type gatedPermOC struct {
	*mockOC
	entered chan struct{}
	release chan struct{}
}

func (g *gatedPermOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return g.mockOC.ListPermissions(ctx, dir)
}

// reconnectPermOC 匿名内嵌 *mockOC，覆写 SubscribeEvents：进入即返回（readyOC 包装层
// 已触发 opts.OnReady），并在 proceed 关闭后调用一次 onReconnect（模拟 SSE 断流重连）。
type reconnectPermOC struct {
	*mockOC
	proceed chan struct{}
	calls   atomic.Int32
}

func (r *reconnectPermOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	r.calls.Add(1)
	go func() {
		<-r.proceed
		onReconnect()
	}()
	<-ctx.Done()
	return ctx.Err()
}

// eventually 轮询等待异步判定/回复收敛（判定/回复 goroutine 与触发点解耦）。
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// permAskedEvent 构造 permission.asked SSE 事件（handleSSEEvent 触发点 (a) 用）。
func permAskedEvent(id, sid, perm string, patterns ...string) opencode.Event {
	props := map[string]interface{}{"id": id, "sessionID": sid, "permission": perm}
	if len(patterns) > 0 {
		arr := make([]interface{}, 0, len(patterns))
		for _, p := range patterns {
			arr = append(arr, p)
		}
		props["patterns"] = arr
	}
	return opencode.Event{Type: "permission.asked", Properties: props}
}

// permRepliedEvent 构造 permission.replied SSE 事件（人工抢先了结模拟用）。
func permRepliedEvent(sid, rid string) opencode.Event {
	return opencode.Event{Type: "permission.replied", Properties: map[string]interface{}{
		"sessionID": sid, "requestID": rid,
	}}
}

// newAutoTaskEnv 构造单元级 ai-auto 消费者环境：active + ai-auto 任务的就绪 runtime
//（serve 会话 env 已就位，taskOcClient 可用；attention 已懒初始化）。
// 返回 (manager, store, mockOC, runtime, 释放函数)。
func newAutoTaskEnv(t *testing.T, judge PermissionJudge) (*Manager, *mockStore, *mockOC, *taskRuntime, func()) {
	t.Helper()
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}
	oc := newMockOC(true)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.ensureAttentionState()
	rt.setPermJudgeReady()
	cleanup := func() { rt.stopAll() }
	t.Cleanup(cleanup)
	return m, store, oc, rt, cleanup
}

// --- 准入矩阵与计数契约（D4/D9） ---

// TestJudgeScan_AdmissionMatrix 准入矩阵：nil judge / 非 ai-auto 模式 / 非 active /
// 未知持久化模式 → 零判定零回复；未就绪 → 暂缓（不登记 judged-set、零判定），
// 置就绪后同一 pending 恰好判定一次（证明此前确未登记）。
func TestJudgeScan_AdmissionMatrix(t *testing.T) {
	newEnv := func(t *testing.T) (*Manager, *mockStore, *mockOC, *taskRuntime, *fakeJudge) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, store, oc, rt, _ := newAutoTaskEnv(t, judge)
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		return m, store, oc, rt, judge
	}

	t.Run("ready+ai-auto 判定一次并回复 once", func(t *testing.T) {
		m, _, oc, rt, judge := newEnv(t)
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool {
			return len(judge.judgeCalls()) == 1 && len(oc.replyPermissionCallsSnapshot()) == 1
		})
		call := oc.replyPermissionCallsSnapshot()[0]
		if call.RequestID != "r1" || call.Reply != "once" || call.Dir != "/data/worktrees/p1/t1" {
			t.Errorf("reply call = %+v, want {r1 once dir=/data/worktrees/p1/t1}", call)
		}
	})

	t.Run("nil judge 零判定零回复", func(t *testing.T) {
		m, _, oc, rt, judge := newEnv(t)
		m.judge = nil
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 || len(oc.replyPermissionCallsSnapshot()) != 0 {
			t.Errorf("nil judge must not initiate: judge=%d reply=%d",
				len(judge.judgeCalls()), len(oc.replyPermissionCallsSnapshot()))
		}
		if len(rt.judgedPerms) != 0 {
			t.Errorf("judged-set must stay empty, got %v", rt.judgedPerms)
		}
	})

	t.Run("ask 模式零发起", func(t *testing.T) {
		m, store, oc, rt, judge := newEnv(t)
		store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAsk })
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 || len(oc.replyPermissionCallsSnapshot()) != 0 {
			t.Errorf("ask mode must not initiate: judge=%d reply=%d",
				len(judge.judgeCalls()), len(oc.replyPermissionCallsSnapshot()))
		}
		if len(rt.judgedPerms) != 0 {
			t.Errorf("judged-set must stay empty (未准入不登记), got %v", rt.judgedPerms)
		}
	})

	t.Run("all-approve 模式零发起", func(t *testing.T) {
		m, store, oc, rt, judge := newEnv(t)
		store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAllApprove })
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 || len(oc.replyPermissionCallsSnapshot()) != 0 {
			t.Errorf("all-approve mode must not initiate")
		}
	})

	t.Run("非 active 零发起", func(t *testing.T) {
		m, store, oc, rt, judge := newEnv(t)
		store.mutTask("t1", func(r *TaskRow) { r.Status = StatusSuspended })
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 || len(oc.replyPermissionCallsSnapshot()) != 0 {
			t.Errorf("non-active task must not initiate")
		}
		if len(rt.judgedPerms) != 0 {
			t.Errorf("judged-set must stay empty, got %v", rt.judgedPerms)
		}
	})

	t.Run("未知持久化模式 fail-closed 零发起", func(t *testing.T) {
		m, store, oc, rt, judge := newEnv(t)
		store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = "bogus" })
		m.judgeScan(rt) // MUST NOT panic、MUST NOT 发起
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 || len(oc.replyPermissionCallsSnapshot()) != 0 {
			t.Errorf("corrupt permission_mode must not initiate")
		}
	})

	t.Run("未就绪暂缓不登记_置就绪后判定一次", func(t *testing.T) {
		m, _, oc, rt, judge := newEnv(t)
		rt.mu.Lock()
		rt.permJudgeReady = false
		rt.mu.Unlock()
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if len(judge.judgeCalls()) != 0 {
			t.Fatalf("not-ready must defer (zero judge), got %d", len(judge.judgeCalls()))
		}
		if len(rt.judgedPerms) != 0 {
			t.Fatalf("not-ready MUST NOT register judged-set, got %v", rt.judgedPerms)
		}
		// 就绪提交后（触发点 (b1) 同序）：先置就绪再扫描 → 该 pending 恰好判定一次。
		rt.setPermJudgeReady()
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool {
			return len(judge.judgeCalls()) == 1 && len(oc.replyPermissionCallsSnapshot()) == 1
		})
	})
}

// TestJudgeScan_JudgedSetDedup judged-set 去重（D4 计数契约）：同 ID SSE 重放只发起一次；
// 新 ID 各判定一次；判定失败不清除（后续扫描不重复发起）。
func TestJudgeScan_JudgedSetDedup(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	a := rt.ensureAttentionState()

	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })

	// SSE 重放同 ID（同值 asked）：不重复判定。
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	m.judgeScan(rt)
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 1 {
		t.Fatalf("SSE replay must not re-judge, judge calls = %d", got)
	}

	// 新 ID r2：纳入判定并回复。
	a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))
	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 2 })
	if got := len(judge.judgeCalls()); got != 2 {
		t.Errorf("judge calls = %d, want 2 (r1+r2)", got)
	}
}

// TestJudgeScan_VerdictContract 判定值契约（D5/D6）：approve→once、reject→reject、
// uncertain 与判定失败 → 不回复；失败后 judged-set 保留（后续扫描不重复发起）。
func TestJudgeScan_VerdictContract(t *testing.T) {
	cases := []struct {
		name      string
		verdict   PermissionVerdict
		judgeErr  error
		wantReply string // "" = 不回复
	}{
		{name: "approve→once", verdict: PermissionVerdictApprove, wantReply: "once"},
		{name: "reject→reject", verdict: PermissionVerdictReject, wantReply: "reject"},
		{name: "uncertain 不回复", verdict: PermissionVerdictUncertain},
		{name: "judge 失败不回复", verdict: PermissionVerdictApprove, judgeErr: errors.New("llm down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			judge := newFakeJudge(tc.verdict)
			judge.err = tc.judgeErr
			m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
			rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
			m.judgeScan(rt)
			eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
			time.Sleep(50 * time.Millisecond)
			replies := oc.replyPermissionCallsSnapshot()
			if tc.wantReply == "" {
				if len(replies) != 0 {
					t.Fatalf("must not reply, got %+v", replies)
				}
				// 判定失败/uncertain：judged-set 不清除——后续扫描不重复发起（不反复烧 LLM）。
				m.judgeScan(rt)
				time.Sleep(50 * time.Millisecond)
				if got := len(judge.judgeCalls()); got != 1 {
					t.Errorf("judged-set must persist after failure: judge calls = %d, want 1", got)
				}
				return
			}
			if len(replies) != 1 || replies[0].Reply != tc.wantReply {
				t.Fatalf("replies = %+v, want exactly one %q", replies, tc.wantReply)
			}
			if replies[0].RequestID != "r1" {
				t.Errorf("reply requestID = %q, want r1", replies[0].RequestID)
			}
		})
	}
}

// --- 回复错误分类（D6 失败三分类） ---

// TestJudgeAndReply_ReplyErrorClassification 回复结果分类：竞态 404 忽略且不降级能力；
// 路由 404 标记 reply-unsupported 并停止后续判定；结果未知（5xx/超时）不重试、
// 不本地改状态、后续扫描不重复判定。
func TestJudgeAndReply_ReplyErrorClassification(t *testing.T) {
	t.Run("ErrPermissionRequestGone 竞态忽略", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		oc.replyPermissionErrByID = map[string]error{"r1": opencode.ErrPermissionRequestGone}
		a := rt.ensureAttentionState()
		a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
		rt.mu.Lock()
		unsupported := rt.replyUnsupported
		rt.mu.Unlock()
		if unsupported {
			t.Fatal("竞态 404 MUST NOT 标记 reply-unsupported")
		}
		// 不重试：调用数保持 1。
		time.Sleep(50 * time.Millisecond)
		if got := len(oc.replyPermissionCallsSnapshot()); got != 1 {
			t.Errorf("must not retry, reply calls = %d", got)
		}
	})

	t.Run("路由 404 标记 unsupported 停止后续判定", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		oc.replyPermissionErrByID = map[string]error{"r1": opencode.ErrCapabilityUnsupported}
		a := rt.ensureAttentionState()
		a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		m.judgeScan(rt)
		// 结果处理完成的屏障：等消费者置位 replyUnsupported（mock 先记录调用再返回
		// 错误，仅凭调用记录断言标记存在合法调度顺序下的偶发窗口）。
		eventually(t, 2*time.Second, func() bool {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return rt.replyUnsupported
		})
		// 后续判定被终止：新 pending 不再发起（准入拒绝，judged-set 不新增）。
		a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 1 {
			t.Errorf("unsupported 后必须停止后续判定, judge calls = %d, want 1", got)
		}
		if _, ok := rt.judgedPerms["r2"]; ok {
			t.Error("unsupported 后 MUST NOT 登记新 ID")
		}
	})

	t.Run("结果未知不重试不本地改状态", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		oc.replyPermissionErr = errors.New("post http://127.0.0.1:50001: connection reset")
		a := rt.ensureAttentionState()
		a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		snapBefore := rt.attentionSnapshot()
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
		time.Sleep(50 * time.Millisecond)
		// 不重试。
		if got := len(oc.replyPermissionCallsSnapshot()); got != 1 {
			t.Fatalf("unknown result MUST NOT retry, reply calls = %d", got)
		}
		// 不本地断言状态：完整 pending 快照逐字段不变（含 Since/Patterns/SessionID）。
		snapAfter := rt.attentionSnapshot()
		if !reflect.DeepEqual(snapBefore.Permissions, snapAfter.Permissions) {
			t.Errorf("local pending MUST NOT change on unknown result:\nbefore=%+v\nafter=%+v",
				snapBefore.Permissions, snapAfter.Permissions)
		}
		// 不标记 unsupported。
		rt.mu.Lock()
		unsupported := rt.replyUnsupported
		rt.mu.Unlock()
		if unsupported {
			t.Error("unknown result MUST NOT mark reply-unsupported")
		}
		// judged-set 保留：重复触发扫描不重复判定/回复。
		m.judgeScan(rt)
		time.Sleep(50 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 1 {
			t.Errorf("judged-set must persist, judge calls = %d", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 1 {
			t.Errorf("must not re-reply, reply calls = %d", got)
		}
	})
}

// --- 停止路径与实例绑定（D4） ---

// TestJudgeScan_StopRuntime_NoSend 停止中的 runtime 不发送：在途判定 ctx 被 cancel，
// stopAll join 判定 goroutine 后零回复；再次扫描准入拒绝（judgeStopping）。
func TestJudgeScan_StopRuntime_NoSend(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	release := make(chan struct{})
	judge.block = release
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-judge.entered // 判定已在途
	rt.stopAll()    // 置 judgeStopping + cancel + join（阻塞直至 goroutine 退出）
	close(release)  // 此时即便判定完成，发送前复核也必然失败
	time.Sleep(50 * time.Millisecond)

	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("stopping/stopped runtime MUST NOT reply, got %d", got)
	}
	// 停止后扫描：准入拒绝，无新判定。
	m.judgeScan(rt)
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 1 {
		t.Errorf("stopped runtime must reject admission, judge calls = %d, want 1", got)
	}
}

// TestJudgeScan_RuntimeRebuild_SameID_RejudgedOnce runtime 重建后同 ID 仍 pending →
// 新实例重新判定一次（judged-set 随 runtime 销毁释放，计数契约 per-runtime）。
func TestJudgeScan_RuntimeRebuild_SameID_RejudgedOnce(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt1, _ := newAutoTaskEnv(t, judge)
	rt1.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	m.judgeScan(rt1)
	// 等第一代「判定+回复」完整收敛再替换（Judge 进入不代表回复已发出——生产在
	// Judge 返回后仍有 permGate 与 HTTP，F3）；旧实例在途结果被抑制由
	// FinalAdmit_ReplacedDuringConstruction 专门覆盖，本用例只证「两代各完成一次」。
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })

	// runtime 销毁（挂起/重建）→ 重建后同 ID 仍 pending → 恰好再判定一次。
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	rt2.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	rt2.setPermJudgeReady()
	m.judgeScan(rt2)
	eventually(t, 2*time.Second, func() bool {
		return len(judge.judgeCalls()) == 2 && len(oc.replyPermissionCallsSnapshot()) == 2
	})
	if got := len(oc.replyPermissionCallsSnapshot()); got != 2 {
		t.Errorf("reply calls = %d, want 2 (one per runtime instance)", got)
	}
}

// --- 触发点接线（D4 (a)/(b1)/(b2)/(c)） ---

// TestSSEAsked_AIAuto_JudgesOnce 触发点 (a)：SSE asked 应用后扫描；重放只判定一次。
func TestSSEAsked_AIAuto_JudgesOnce(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, _, _ := newAutoTaskEnv(t, judge)

	ev := permAskedEvent("r1", "s1", "bash", "rm")
	if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", ev); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })

	// 重放同 asked：judged-set 去重。
	if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", ev); err != nil {
		t.Fatalf("handleSSEEvent replay: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 1 {
		t.Errorf("replayed asked must not re-judge, judge calls = %d", got)
	}
}

// TestActivate_AIAuto_JudgesPendingAfterCommit 触发点 (b1) 首次激活：activating 期经
// 首次 align 登记的 pending 在 CAS 提交后恰好判定一次并回复 once（提交前零判定由
// 未就绪准入矩阵覆盖）。
func TestActivate_AIAuto_JudgesPendingAfterCommit(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
	// 首次 align（就绪提交前）发现的 pending：由提交后扫描兜底纳入。
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	judge := newFakeJudge(PermissionVerdictApprove)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
	if in := judge.judgeCalls()[0]; in.Permission != "bash" || in.TaskName != "my task" {
		t.Errorf("judge input = %+v, want permission=bash task=my task", in)
	}
	replies := oc.replyPermissionCallsSnapshot()
	if replies[0].RequestID != "r1" || replies[0].Reply != "once" {
		t.Errorf("reply = %+v, want {r1 once}", replies[0])
	}
	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime missing after activate")
	}
	if _, ok := rt.judgedPerms["r1"]; !ok {
		t.Error("judged-set must contain r1 after commit scan")
	}
}

// TestResumeActive_AIAuto_CommitGating 触发点 (b1) 启动恢复：提交前登记的 pending
// 在 writeStatus(active) 后恰好判定一次；恢复失败（watcher 恢复失败）则零判定零回复。
func TestResumeActive_AIAuto_CommitGating(t *testing.T) {
	newResumeEnv := func(t *testing.T) (*Manager, *mockStore, *mockOC, *fakeJudge, *ptrListErrProc) {
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		store.mutTask("t1", func(r *TaskRow) {
			r.Status = StatusActive
			r.PermissionMode = PermissionModeAIAuto
		})
		snap := envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}}
		snapBytes, _ := encodeEnvSnapshot(snap)
		store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
		proc := newMockProc()
		// resumeActive 读 runtime 会话名（runtimeSessionName）下的密码/端口/任务 ID。
		proc.sessions[runtimeSessionName("t1")] = true
		proc.envValues[runtimeSessionName("t1")] = map[string]string{
			"OPENCODE_SERVER_PASSWORD": "pw",
			"OCDECK_SERVE_PORT":        "50001",
			"OCDECK_TASK_ID":           "t1",
		}
		oc := newMockOC(true)
		oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
		judge := newFakeJudge(PermissionVerdictApprove)
		m := newTestManager(t, store, proc, newMockWorktree(), oc)
		m.judge = judge
		m.SetLifecycleCtx(context.Background())
		return m, store, oc, judge, &ptrListErrProc{mockProc: proc, err: errors.New("tmux list failed")}
	}

	t.Run("提交成功后恰好一次", func(t *testing.T) {
		m, store, oc, judge, _ := newResumeEnv(t)
		row, _ := store.GetTask(context.Background(), "t1")
		if err := m.resumeActive(context.Background(), row, AlignModeRepo); err != nil {
			t.Fatalf("resumeActive: %v", err)
		}
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
	})

	t.Run("恢复失败零判定零回复", func(t *testing.T) {
		m, store, oc, judge, badList := newResumeEnv(t)
		// 换上 watcher 恢复必败的 proc：pending 已由 align 登记（提交前），但提交不发生。
		m.proc = badList
		row, _ := store.GetTask(context.Background(), "t1")
		if err := m.resumeActive(context.Background(), row, AlignModeRepo); err == nil {
			t.Fatal("resumeActive must fail with watcher restore error")
		}
		time.Sleep(100 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 0 {
			t.Errorf("resume failure MUST NOT judge, judge calls = %d", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("resume failure MUST NOT reply, reply calls = %d", got)
		}
		// 恢复失败 → clearRuntime：runtime 不留就绪状态。
		if rt := m.getRuntime("t1"); rt != nil {
			rt.mu.Lock()
			ready := rt.permJudgeReady
			rt.mu.Unlock()
			if ready {
				t.Error("failed resume must not leave runtime ready")
			}
		}
	})
}

// TestSuspendRepair_AIAuto_JudgesAfterRepair 触发点 (b1) 挂起修复：修复重建期间经
// align 登记的 pending 在 suspending→active 提交后恰好判定一次。
func TestSuspendRepair_AIAuto_JudgesAfterRepair(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) {
		t.Status = StatusActive
		t.PermissionMode = PermissionModeAIAuto
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	// reply 路径 taskOcClient 读 runtime 会话名下的密码/端口（修复路径不重写 env）。
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
	}
	// tui/serve kill 失败 + serve 存活 → 分支 c 修复（tryRepairRuntime → startSSE align）。
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	judge := newFakeJudge(PermissionVerdictApprove)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActive {
		t.Fatalf("status = %s, want active (branch c repaired)", row.Status)
	}
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
}

// TestReconnectAlign_AIAuto_JudgesAfterAlign 触发点 (b2)：已 active 实例重连 align
// 对账写回发现的 pending 在对账完成后判定一次（startSSE onReconnect 接缝）。
func TestReconnectAlign_AIAuto_JudgesAfterAlign(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	roc := &reconnectPermOC{mockOC: oc, proceed: make(chan struct{})}
	// 替换 ocFactory 产物：taskOcClient/reply 仍走原 mockOC（roc 内嵌），SSE 走 roc。
	m.ocFactory = func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: roc, onReady: opts.OnReady}
	}
	oc.listPermissionsResult = nil // 首次 align：无 pending

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		started <- m.startSSE(ctx, rt, "t1", "/data/worktrees/p1/t1", 50001, "pw", AlignModeRepo)
	}()
	// startSSE 等待 ready 后完成首次 align 才返回。
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("startSSE: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startSSE did not complete first align in time")
	}
	if got := roc.calls.Load(); got != 1 {
		t.Fatalf("SubscribeEvents calls = %d, want 1", got)
	}
	// 重连期间对账将发现的新 pending（REST 结果在重连前就位）。
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	close(roc.proceed) // 触发 onReconnect → align → reconcileTaskAttention → judgeScan
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
}

// TestDegradedReplay_AIAuto_JudgesOnce 触发点 (c)：后台 GET 在途时到达的 asked 入缓冲，
// GET 失败重放写入 pending 后由 (c) 扫描判定一次；重复失败不重复判定。
func TestDegradedReplay_AIAuto_JudgesOnce(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	oc.listPermissionsErr = errors.New("flux") // 后台对账持续失败（degraded）
	a := rt.ensureAttentionState()

	// 预置 degraded：一次失败的后台对账（此时无 pending，不触发判定）。
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
	a.mu.Lock()
	degraded := a.perm.cap == capDegraded
	a.mu.Unlock()
	if !degraded {
		t.Fatal("precondition: perm cap should be degraded after failed background reconcile")
	}
	// 预置 pending r1（degraded 直接写路径）。
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	// 轮 1：GET 在途期间 asked r2 入缓冲 → 失败重放写入 → (c) 扫描判定 r1+r2。
	gated := &gatedPermOC{mockOC: oc, entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.ocFactory = func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: gated, onReady: opts.OnReady}
	}
	done := make(chan struct{})
	go func() {
		m.retryAttentionDegraded(context.Background())
		close(done)
	}()
	<-gated.entered // GET 在途
	// GET 在途期间 asked 只入缓冲（attention.go 增量缓冲模型）。
	a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))
	// 放行 → GET 失败 → degraded 重放写入 r2 → (c) 扫描判定 r1+r2。
	close(gated.release)
	<-done
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 2 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 2 })

	// 轮 2：再次失败重试 → 无新 ID → 零新判定（重复失败不重复判定）。
	m.retryAttentionDegraded(context.Background())
	time.Sleep(100 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 2 {
		t.Errorf("repeated failure must not re-judge, judge calls = %d, want 2", got)
	}
}

// TestHandleSSEAsked_PreCommitResolved_NoJudge 提交前观察、提交前人工了结：activating
// 期 asked 暂缓不登记；人工回复后 pending 移除；就绪提交后扫描零判定。
func TestHandleSSEAsked_PreCommitResolved_NoJudge(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	rt.mu.Lock()
	rt.permJudgeReady = false // 模拟 activating 期（未就绪提交）
	rt.mu.Unlock()

	if err := m.handleSSEEvent(context.Background(), "t1", "/wt", permAskedEvent("r1", "s1", "bash", "rm")); err != nil {
		t.Fatalf("handleSSEEvent asked: %v", err)
	}
	if got := len(judge.judgeCalls()); got != 0 {
		t.Fatalf("pre-commit asked must defer, judge calls = %d", got)
	}
	// 人工抢先了结。
	if err := m.handleSSEEvent(context.Background(), "t1", "/wt", permRepliedEvent("s1", "r1")); err != nil {
		t.Fatalf("handleSSEEvent replied: %v", err)
	}
	// 就绪提交后扫描：ID 已不在 pending → 不发起。
	rt.setPermJudgeReady()
	m.judgeScan(rt)
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 0 {
		t.Errorf("resolved-before-commit must not judge, judge calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("resolved-before-commit must not reply, reply calls = %d", got)
	}
}

// --- F1 回归：最终发送准入复核（客户端构造后、ReplyPermission 前） ---

// blockedFactory 返回一个在 ocFactory 调用处阻塞的工厂（taskOcClient 构造的
// DB 读 + env 读之后、返回客户端之前），用于确定性注入「构造期间发生 stop /
// unsupported / 实例替换」。测试负责在断言前 close release（避免 cleanup 卡死）。
func blockedFactory(oc *mockOC) (entered chan struct{}, release chan struct{}, factory OCClientFactory) {
	entered = make(chan struct{}, 1)
	release = make(chan struct{})
	factory = func(port int, password string, opts opencode.Options) OCClient {
		entered <- struct{}{}
		<-release
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	return entered, release, factory
}

// TestJudgeAndReply_FinalAdmit_StopDuringConstruction 客户端构造阻塞期间发生 stop →
// 最终发送准入复核拒绝，零 Reply（旧实现在构造前一次性复核，无此防御）。
func TestJudgeAndReply_FinalAdmit_StopDuringConstruction(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	entered, release, factory := blockedFactory(oc)
	m.ocFactory = factory
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-entered // 发送前复核已过，客户端构造阻塞中
	stopped := make(chan struct{})
	go func() { rt.stopAll(); close(stopped) }()
	// 确定性交接：等 stopAll 锁内段完成（judgeStopping 已置位）再放行构造，
	// 杜绝「release 先于 stop 拿锁」的调度竞态。
	eventually(t, 2*time.Second, func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.judgeStopping
	})
	close(release)
	select {
	case <-stopped: // stopAll 已 join 判定 goroutine
	case <-time.After(3 * time.Second):
		t.Fatal("stopAll must join judge goroutine after release")
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("stop during client construction MUST NOT send, replies = %d", got)
	}
	if got := len(judge.judgeCalls()); got != 1 {
		t.Errorf("judge should have happened before construction, calls = %d", got)
	}
}

// TestJudgeAndReply_FinalAdmit_UnsupportedDuringConstruction 客户端构造阻塞期间
// 标记 reply-unsupported → 最终发送准入拒绝，零 Reply。
func TestJudgeAndReply_FinalAdmit_UnsupportedDuringConstruction(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	entered, release, factory := blockedFactory(oc)
	m.ocFactory = factory
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-entered
	rt.mu.Lock()
	rt.replyUnsupported = true
	rt.mu.Unlock()
	close(release)
	<-judge.returns
	time.Sleep(50 * time.Millisecond)
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("unsupported during construction MUST NOT send, replies = %d", got)
	}
}

// TestJudgeAndReply_FinalAdmit_ReplacedDuringConstruction 旧实例延迟返回零回复
//（F1/F4.2）：Manager 替换 runtime 后，旧 runtime 的判定结果 MUST NOT 发送——
// 旧对象令牌自比较恒真，只有 Manager 当前实例校验能拦住（旧实现此处会发送）。
// 审计（7.2 补强）：最终发送准入拒绝仍是「调用 Judge 后」的终结路径 → 恰好一条
// APPROVE + not_applicable（留痕不受 permGate 拦截）。
func TestJudgeAndReply_FinalAdmit_ReplacedDuringConstruction(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt1, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{}
	m.permAudit = fake
	entered, release, factory := blockedFactory(oc)
	m.ocFactory = factory
	rt1.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt1)
	<-entered
	// Manager 换上新 runtime（生产路径随后会清理旧 rt；此处取「替换后、清理前」
	// 的竞态窗口——最终发送复核必须兜底）。
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	close(release)
	<-judge.returns
	time.Sleep(50 * time.Millisecond)
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("stale instance late verdict MUST NOT send, replies = %d", got)
	}
	// 审计留痕：恰好一条 APPROVE + not_applicable、不含 reply。
	eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
	recs := fake.snapshot()
	if recs[0].Verdict != "APPROVE" || recs[0].ReplyResult != "not_applicable" || recs[0].Reply != "" {
		t.Errorf("record = %+v, want APPROVE + not_applicable without reply", recs[0])
	}
	// 新 runtime 不受牵连：无 pending、无判定发起。
	if got := len(judge.judgeCalls()); got != 1 {
		t.Errorf("replacement must not trigger extra judge, calls = %d", got)
	}
}

// --- F2 回归：批内条间复核 ---

// TestJudgeScan_BatchStopsOnUnsupported 一次扫描含多条请求，首条回复返回路由 404
// 标记 unsupported 后，同批剩余条目 MUST NOT 进入 Judge。
func TestJudgeScan_BatchStopsOnUnsupported(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	oc.replyPermissionErrByID = map[string]error{"r1": opencode.ErrCapabilityUnsupported}
	a := rt.ensureAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))

	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 1 {
		t.Fatalf("after unsupported, remaining batch items MUST NOT judge, judge calls = %d, want 1", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 1 {
		t.Errorf("replies = %d, want 1", got)
	}
}

// TestJudgeScan_BatchStopsOnStop 首条 Judge 阻塞期间 stop → 剩余条目 MUST NOT 进入
// Judge（stopAll join 保证断言时 goroutine 已退出）。
func TestJudgeScan_BatchStopsOnStop(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	release := make(chan struct{})
	judge.block = release
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	a := rt.ensureAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))

	m.judgeScan(rt)
	<-judge.entered // r1 判定在途
	rt.stopAll()    // cancel + join（r1 经 ctx.Done 收敛，r2 条间复核拒绝）
	if got := len(judge.judgeCalls()); got != 1 {
		t.Fatalf("after stop, remaining batch items MUST NOT judge, judge calls = %d, want 1", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("replies = %d, want 0", got)
	}
	close(release) // 清理（goroutine 已退出，无读方）
}

// --- F3 回归：并发停止的 join 与 cancel 所有权解耦 ---

// TestStop_ConcurrentStops_BothJoin 第一个 stop 正在等待 Judge 收尾时，第二个 stop
// 也必须等待（无条件 judgeWG.Wait）：第二个调用返回 MUST 发生在判定 goroutine
// 退出之后（旧实现仅取得 cancel 的调用方 join，第二个调用提前返回）。
func TestStop_ConcurrentStops_BothJoin(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	release := make(chan struct{})
	judge.block = release
	judge.drain = 200 * time.Millisecond // ctx.Done 后仍收尾一段时间（模拟在途请求）
	ctxDoneObserved := make(chan struct{})
	judge.onCtxDone = func() { close(ctxDoneObserved) }
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-judge.entered

	aDone := make(chan struct{})
	go func() { rt.stopAllJoin(); close(aDone) }()
	<-ctxDoneObserved // A 已 cancel 并进入 Wait（fake 仍在 drain）

	bDone := make(chan struct{})
	go func() { rt.stopAll(); close(bDone) }()

	<-bDone // B 返回
	select {
	case <-judge.returns:
		// B 返回时 Judge 已完成（returns 信号先于 goroutine Done）→ B 确实 join 了。
	default:
		t.Fatal("second stop returned before judge goroutine finished (join skipped)")
	}
	select {
	case <-aDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first stop must also complete")
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("replies = %d, want 0", got)
	}
}

// --- F4 补齐：401 不影响后续判定 / degraded 成功替换 / resumeActive 阻塞注入 ---

// TestJudgeAndReply_401DoesNotAffectSubsequent 401 等普通回复错误不影响后续新请求
// 的判定与回复（D6：MUST NOT 标记 unsupported）。
func TestJudgeAndReply_401DoesNotAffectSubsequent(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	oc.replyPermissionErrByID = map[string]error{"r1": opencode.ErrUnauthorized}
	a := rt.ensureAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	a.applyAttentionEvent(permAsked("r2", "s1", "edit", "file"))

	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 2 })
	replies := oc.replyPermissionCallsSnapshot()
	if replies[0].RequestID != "r1" || replies[1].RequestID != "r2" {
		t.Errorf("replies = %+v, want r1 then r2 both attempted", replies)
	}
	rt.mu.Lock()
	unsupported := rt.replyUnsupported
	rt.mu.Unlock()
	if unsupported {
		t.Error("401 MUST NOT mark reply-unsupported")
	}
}

// countingPermOC 匿名内嵌 *mockOC，仅计数 ListPermissions 调用次数（F6：证明
// degraded 恢复后第二次成功 GET 真实发生）。
type countingPermOC struct {
	*mockOC
	calls *int32
}

func (c *countingPermOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	atomic.AddInt32(c.calls, 1)
	return c.mockOC.ListPermissions(ctx, dir)
}

// TestDegradedReplay_AIAuto_SuccessReplace_JudgesOnce (c) 成功替换路径（F6 修订）：
// 初始 pending 为空，成功 REST 首次发现新 ID → (c) 扫描判定/回复恰好一次；
// 再经失败对账恢复 degraded，下一次扫描实际执行第二次成功 GET、返回相同快照 →
// GET 次数增加但 Judge/Reply 不增加（judged-set 去重）。
func TestDegradedReplay_AIAuto_SuccessReplace_JudgesOnce(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	var getCalls int32
	counting := &countingPermOC{mockOC: oc, calls: &getCalls}
	m.ocFactory = func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: counting, onReady: opts.OnReady}
	}
	a := rt.ensureAttentionState()
	ctx := context.Background()

	// 前置：制造 degraded（失败后台对账）；初始 pending 为空。
	oc.listPermissionsErr = errors.New("flux")
	a.reconcileAttention(ctx, counting, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
	if snap := a.attentionSnapshot(); len(snap.Permissions) != 0 {
		t.Fatalf("precondition: pending must be empty, got %+v", snap.Permissions)
	}

	// 成功替换：REST 首次发现新 ID r1 → 恰好一次 GET → (c) 扫描 → 判定/回复恰好一次。
	oc.listPermissionsErr = nil
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	getsBefore := atomic.LoadInt32(&getCalls)
	m.retryAttentionDegraded(ctx)
	if got := atomic.LoadInt32(&getCalls); got != getsBefore+1 {
		t.Fatalf("degraded reconcile must issue exactly one GET, delta = %d", got-getsBefore)
	}
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })

	// 再经失败对账恢复 degraded（cap=available 时 retryAttentionDegraded 跳过 degraded
	// 类型，故用同一条失败 REST 路径直接恢复；degraded 保留旧集合 r1）。
	oc.listPermissionsErr = errors.New("flux")
	a.reconcileAttention(ctx, counting, "/wt", opencode.AttentionPermission, reconcileBackground, nil)

	// 第二次成功 GET 返回相同快照：(c) 扫描真实执行（GET 增加）但 judged-set 去重，
	// Judge/Reply 不增加。
	oc.listPermissionsErr = nil
	getsBefore2 := atomic.LoadInt32(&getCalls)
	m.retryAttentionDegraded(ctx)
	if got := atomic.LoadInt32(&getCalls); got != getsBefore2+1 {
		t.Fatalf("second degraded reconcile must issue exactly one GET, delta = %d", got-getsBefore2)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 1 {
		t.Errorf("duplicate snapshot must not re-judge, judge calls = %d, want 1", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 1 {
		t.Errorf("duplicate snapshot must not re-reply, replies = %d, want 1", got)
	}
}

// gatedListProc 匿名内嵌 *mockProc，覆写 ListSessions：进入时发 entered 信号并阻塞
// 直到 release（定位 resumeActive 的 watcher 恢复阶段），release 后按 err 返回。
type gatedListProc struct {
	*mockProc
	entered chan struct{}
	release chan struct{}
	err     error
}

func (p *gatedListProc) ListSessions() ([]string, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
	if p.err != nil {
		return nil, p.err
	}
	return p.mockProc.ListSessions()
}

// TestResumeActive_AIAuto_PreCommitAskedGating（F4.1）resumeActive 阻塞后续恢复步骤
//（watcher 阶段）注入 asked + 立即 APPROVE fake Judge：提交前零 Judge/零 Reply 且
// 暂缓不登记；提交成功后恰好一次判定；恢复失败始终零回复。
func TestResumeActive_AIAuto_PreCommitAskedGating(t *testing.T) {
	newEnv := func(t *testing.T, listErr error) (*Manager, *mockStore, *mockOC, *fakeJudge, *gatedListProc, chan error) {
		store := newMockStore()
		seedSuspendedTask(store, "t1", "p1")
		store.mutTask("t1", func(r *TaskRow) {
			r.Status = StatusActive
			r.PermissionMode = PermissionModeAIAuto
		})
		snap := envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t1"}}
		snapBytes, _ := encodeEnvSnapshot(snap)
		store.mutTask("t1", func(r *TaskRow) { r.EnvSnapshot = snapBytes })
		proc := newMockProc()
		proc.sessions[runtimeSessionName("t1")] = true
		proc.envValues[runtimeSessionName("t1")] = map[string]string{
			"OPENCODE_SERVER_PASSWORD": "pw",
			"OCDECK_SERVE_PORT":        "50001",
			"OCDECK_TASK_ID":           "t1",
		}
		gated := &gatedListProc{
			mockProc: proc,
			entered:  make(chan struct{}, 1),
			release:  make(chan struct{}),
			err:      listErr,
		}
		oc := newMockOC(true)
		oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
		judge := newFakeJudge(PermissionVerdictApprove)
		m := newTestManager(t, store, proc, newMockWorktree(), oc)
		m.judge = judge
		m.proc = gated
		m.SetLifecycleCtx(context.Background())
		done := make(chan error, 1)
		return m, store, oc, judge, gated, done
	}

	t.Run("提交前零判定_提交后恰好一次", func(t *testing.T) {
		m, store, oc, judge, gated, done := newEnv(t, nil)
		row, _ := store.GetTask(context.Background(), "t1")
		go func() { done <- m.resumeActive(context.Background(), row, AlignModeRepo) }()
		<-gated.entered // watcher 恢复阶段（align 已完成、active 未提交）
		if got := len(judge.judgeCalls()); got != 0 {
			t.Fatalf("pre-commit MUST NOT judge, calls = %d", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Fatalf("pre-commit MUST NOT reply, replies = %d", got)
		}
		// env/watcher 阶段注入 asked：(a) 扫描因未就绪暂缓，不登记 judged-set。
		if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", permAskedEvent("r9", "s1", "edit", "file")); err != nil {
			t.Fatalf("handleSSEEvent: %v", err)
		}
		if rt := m.getRuntime("t1"); rt != nil && len(rt.judgedPerms) != 0 {
			t.Errorf("pre-commit asked MUST NOT register judged-set, got %v", rt.judgedPerms)
		}
		close(gated.release) // 放行 → 提交 active → (b1) 扫描（r1 align + r9 asked）
		if err := <-done; err != nil {
			t.Fatalf("resumeActive: %v", err)
		}
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 2 })
		eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 2 })
	})

	t.Run("恢复失败始终零回复", func(t *testing.T) {
		m, store, oc, judge, gated, done := newEnv(t, errors.New("tmux list failed"))
		row, _ := store.GetTask(context.Background(), "t1")
		go func() { done <- m.resumeActive(context.Background(), row, AlignModeRepo) }()
		<-gated.entered
		if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", permAskedEvent("r9", "s1", "edit", "file")); err != nil {
			t.Fatalf("handleSSEEvent: %v", err)
		}
		close(gated.release)
		if err := <-done; err == nil {
			t.Fatal("resumeActive must fail (watcher restore error)")
		}
		time.Sleep(100 * time.Millisecond)
		if got := len(judge.judgeCalls()); got != 0 {
			t.Errorf("failed resume MUST NOT judge, calls = %d", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("failed resume MUST NOT reply, replies = %d", got)
		}
	})
}

// --- F4 残余：真实激活路径的提交前暂缓/提交失败、挂起修复提交失败 ---

// errCommitInject 目标提交点的注入失败 sentinel（F8）：测试断言操作最终返回的
// 错误链保留该原因（errors.Is），证明失败确实来自目标 CAS 而非任意更早步骤。
var errCommitInject = errors.New("injected commit-point failure")

// failCommitCASStore 只在目标提交点的 CAS 注入失败（其余状态迁移透传）：
// failFrom/failTo 指定要拦截的迁移（activating→active 或 suspending→active）。
// onCommitInject 在拦截分支内、返回错误前调用（mutation 验证用确定性屏障：
// 等「错误时点发起的判定」已进入 Judge 再放行失败，避免 rollback cancel+join
// 在 goroutine 调度前掩盖错误点扫描）。
// 拦截分支内记录命中次数与拦截时刻的 runtime 状态（F8）：pending 已被观察登记、
// 实例尚未就绪——证明失败发生在「已观察 pending 后的目标提交点」。
type failCommitCASStore struct {
	*mockStore
	failFrom       string
	failTo         string
	onCommitInject func()

	// m 在 Manager 构造后由测试注入（拦截分支读取 runtime 状态用）。
	m *Manager
	// mu 保护以下拦截时刻记录（拦截发生在被测流程 goroutine，断言在测试 goroutine）。
	mu           sync.Mutex
	hits         int32
	injectSnap   []PendingPermission
	injectReady  bool
	injectRtGone bool
}

func (s *failCommitCASStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if fromStatus == s.failFrom && toStatus == s.failTo {
		rt := s.m.getRuntime(id)
		s.mu.Lock()
		s.hits++
		if rt == nil {
			s.injectRtGone = true
		} else {
			s.injectSnap = rt.attentionSnapshot().Permissions
			rt.mu.Lock()
			s.injectReady = rt.permJudgeReady
			rt.mu.Unlock()
		}
		s.mu.Unlock()
		if s.onCommitInject != nil {
			s.onCommitInject()
		}
		return application.TransitionResult{}, errCommitInject
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

// interceptSnapshot 返回拦截时刻记录：命中次数、pending 快照、就绪状态、runtime 缺失标记。
func (s *failCommitCASStore) interceptSnapshot() (hits int32, snap []PendingPermission, ready bool, rtGone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.injectSnap, s.injectReady, s.injectRtGone
}

// TestActivate_AIAuto_PreCommitAskedGating 真实激活流程提交前暂缓（F4 残余 1）：
// 在目标提交点（CAS active）之前的屏障（commitRuntimeReady 内 lastPort 写入，
// 首次 align 之后）注入 asked——证明 pending 已被观察登记、提交前零 Judge/零 Reply
// 且不登记 judged-set；提交成功后恰好一次判定（align r1 + 注入 r9）。
func TestActivate_AIAuto_PreCommitAskedGating(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	judge := newFakeJudge(PermissionVerdictApprove)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge

	// CAS 前屏障：lastPort 写入（mockStore 在取锁前回调，屏障期间 store 可读）。
	release := make(chan struct{})
	gated := make(chan struct{}, 1)
	store.onLastPort = func(string) {
		gated <- struct{}{}
		<-release
	}

	done := make(chan error, 1)
	go func() { done <- m.Activate(context.Background(), "t1") }()
	<-gated // 已到提交点屏障（align 已完成）

	rt := m.getRuntime("t1")
	if rt == nil {
		t.Fatal("runtime should be registered before commit barrier")
	}
	// 证明 pending 已被观察登记（align r1 已写入 attention）。
	if snap := rt.attentionSnapshot(); len(snap.Permissions) != 1 || snap.Permissions[0].ID != "r1" {
		t.Fatalf("align-registered pending must be observed, got %+v", snap.Permissions)
	}
	// 提交点前注入 asked：(a) 扫描因 DB 非 active（activating）暂缓，不登记 judged-set。
	if err := m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", permAskedEvent("r9", "s1", "edit", "file")); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	if snap := rt.attentionSnapshot(); len(snap.Permissions) != 2 {
		t.Fatalf("injected asked must be observed, got %+v", snap.Permissions)
	}
	if got := len(judge.judgeCalls()); got != 0 {
		t.Fatalf("pre-commit MUST NOT judge, calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("pre-commit MUST NOT reply, replies = %d", got)
	}
	if len(rt.judgedPerms) != 0 {
		t.Fatalf("pre-commit MUST NOT register judged-set, got %v", rt.judgedPerms)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Activate: %v", err)
	}
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 2 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 2 })
}

// TestActivate_AIAuto_CommitFailure_NoReply 激活提交失败零回复（F4 残余 2 / F8）：
// 失败注入精确发生在目标提交点（activating→active CAS）且拦截时刻 pending（align r1）
// 已被观察登记、runtime 尚未就绪——证明「失败确实发生在已观察 pending 后的目标提交点」，
// 任意更早的失败无法满足命中/快照/sentinel 断言。align 期间登记的 pending 不触发任何
// 判定/回复。
func TestActivate_AIAuto_CommitFailure_NoReply(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(tr *TaskRow) { tr.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	oc := newMockOC(true)
	oc.createSessionResult = opencode.Session{ID: "sess-fresh", Time: opencode.SessionTime{Created: 10, Updated: 20}}
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	judge := newFakeJudge(PermissionVerdictApprove)
	// block 使「错误时点发起的判定」一旦进入 Judge 即被记录（calls==1）并 park 至
	// ctx 取消——rollback 的 cancel+join 不会掩盖 mutation 的错误点扫描。
	judge.block = make(chan struct{})
	fstore := &failCommitCASStore{mockStore: store, failFrom: StatusActivating, failTo: StatusActive}
	fstore.onCommitInject = func() {
		// 等 mutation 的错误点扫描 goroutine 已进入 Judge（真实实现下无扫描，超时放行）。
		select {
		case <-judge.entered:
		case <-time.After(500 * time.Millisecond):
		}
	}
	m := newTestManager(t, fstore, proc, newMockWorktree(), oc)
	fstore.m = m
	m.judge = judge

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate must fail at injected CAS commit point")
	}
	if !errors.Is(err, errCommitInject) {
		t.Fatalf("err = %v, want sentinel errCommitInject（失败必须来自目标 CAS 注入点）", err)
	}
	// 目标提交点恰好命中一次（任意更早失败 → 命中 0，重复提交 → 命中 >1）。
	hits, snap, ready, rtGone := fstore.interceptSnapshot()
	if hits != 1 {
		t.Fatalf("target CAS intercept hits = %d, want 1（失败必须恰好发生在目标提交点）", hits)
	}
	if rtGone {
		t.Fatal("runtime must exist at target commit point")
	}
	// 拦截时刻：align r1 已被观察登记，runtime 尚未就绪（提交点在就绪置位之前）。
	if len(snap) != 1 || snap[0].ID != "r1" {
		t.Fatalf("pending must be observed at target commit point, got %+v", snap)
	}
	if ready {
		t.Error("runtime must NOT be ready at target commit point (提交点先于就绪置位)")
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 0 {
		t.Errorf("commit failure MUST NOT judge, calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("commit failure MUST NOT reply, replies = %d", got)
	}
}

// TestSuspendRepair_AIAuto_CommitFailure_NoReply 挂起修复提交失败零回复（F4 残余 3 / F8）：
// 失败注入精确发生在目标提交点（suspending→active CAS）且拦截时刻 pending（修复 align
// 的 r1）已被观察登记、runtime 尚未就绪；修复 align 期间登记的 pending 不触发任何
// 判定/回复，任务收敛 suspended。
func TestSuspendRepair_AIAuto_CommitFailure_NoReply(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	store.mutTask("t1", func(t *TaskRow) {
		t.Status = StatusActive
		t.PermissionMode = PermissionModeAIAuto
		t.EnvSnapshot = sql.NullString{String: `{"vars":{"PATH":"/usr/bin"}}`, Valid: true}
		t.LastPort = sql.NullInt64{Int64: 50001, Valid: true}
	})
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true
	proc.envValues[serveSessionName("t1")] = map[string]string{"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001"}
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
	}
	proc.killResults[tuiSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk1"}}
	proc.killResults[serveSessionName("t1")] = process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk2"}}
	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash", "rm")}
	judge := newFakeJudge(PermissionVerdictApprove)
	// 同 TestActivate_AIAuto_CommitFailure_NoReply：进入 Judge 即记录，防 cancel+join 掩盖。
	judge.block = make(chan struct{})
	fstore := &failCommitCASStore{mockStore: store, failFrom: StatusSuspending, failTo: StatusActive}
	fstore.onCommitInject = func() {
		select {
		case <-judge.entered:
		case <-time.After(500 * time.Millisecond):
		}
	}
	m := newTestManager(t, fstore, proc, newMockWorktree(), oc)
	fstore.m = m
	m.judge = judge

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("Suspend must surface repair commit failure")
	}
	if !errors.Is(err, errCommitInject) {
		t.Fatalf("err = %v, want sentinel errCommitInject（失败必须来自目标 CAS 注入点）", err)
	}
	// 目标提交点恰好命中一次；拦截时刻 pending 已被观察登记、runtime 尚未就绪。
	hits, snap, ready, rtGone := fstore.interceptSnapshot()
	if hits != 1 {
		t.Fatalf("target CAS intercept hits = %d, want 1（失败必须恰好发生在目标提交点）", hits)
	}
	if rtGone {
		t.Fatal("runtime must exist at target commit point")
	}
	if len(snap) != 1 || snap[0].ID != "r1" {
		t.Fatalf("pending must be observed at target commit point, got %+v", snap)
	}
	if ready {
		t.Error("runtime must NOT be ready at target commit point (提交点先于就绪置位)")
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended (detached compensation)", row.Status)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 0 {
		t.Errorf("repair commit failure MUST NOT judge, calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("repair commit failure MUST NOT reply, replies = %d", got)
	}
}

// --- F1：judgeScan 入口零 DB/零阻塞；扫描工作绑定 runtime cancel+join ---

// gatedGetTaskStore 匿名内嵌 *mockStore：第 armAfter 次 GetTask 阻塞在屏障
//（entered 信号、release 放行或 ctx 取消），用于确定性验证扫描 DB 读的可取消性。
type gatedGetTaskStore struct {
	*mockStore
	armAfter int32
	calls    atomic.Int32
	canceled atomic.Bool
	entered  chan struct{}
	release  chan struct{}
}

func (s *gatedGetTaskStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	if s.calls.Add(1) == s.armAfter {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			s.canceled.Store(true)
			return TaskRow{}, ctx.Err()
		}
	}
	return s.mockStore.GetTask(ctx, id)
}

// TestJudgeScan_SSEPathNotBlockedByDBPrecheck SSE asked 处理路径（触发点 (a)）不被
// 判定扫描的 DB 预检阻塞：judgeScan 入口零 DB/零阻塞，DB 读在扫描 goroutine 内
//（judgeCtx 绑定）。阻塞 DB 读期间 handleSSEEvent MUST 立即返回，放行后判定照常。
func TestJudgeScan_SSEPathNotBlockedByDBPrecheck(t *testing.T) {
	store := &gatedGetTaskStore{
		mockStore: newMockStore(),
		armAfter:  1,
		entered:   make(chan struct{}, 1),
		release:   make(chan struct{}),
	}
	seedActiveTask(store.mockStore, "t1", "p1")
	store.mockStore.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}
	oc := newMockOC(true)
	judge := newFakeJudge(PermissionVerdictApprove)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.ensureAttentionState()
	rt.setPermJudgeReady()

	done := make(chan error, 1)
	go func() {
		done <- m.handleSSEEvent(context.Background(), "t1", "/data/worktrees/p1/t1", permAskedEvent("r1", "s1", "bash", "rm"))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleSSEEvent: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("SSE asked path MUST NOT block on judgeScan DB pre-check")
	}
	<-store.entered // 扫描 goroutine 确已阻塞在 DB 读（非已通过）
	close(store.release)
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
}

// TestJudgeScan_StopDuringBlockedDBRead 阻塞 DB 读期间停止 runtime：judgeCtx 先
// cancel → DB 读收取消 → 扫描退出 → stopAll join 完成（不被无法取消的 DB 读拖住）；
// 全程零 Judge/零 Reply 且 judged-set 未登记（登记以完整准入通过为前提）。
func TestJudgeScan_StopDuringBlockedDBRead(t *testing.T) {
	store := &gatedGetTaskStore{
		mockStore: newMockStore(),
		armAfter:  1,
		entered:   make(chan struct{}, 1),
		release:   make(chan struct{}),
	}
	seedActiveTask(store.mockStore, "t1", "p1")
	store.mockStore.mutTask("t1", func(r *TaskRow) { r.PermissionMode = PermissionModeAIAuto })
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw",
		"OCDECK_SERVE_PORT":        "50001",
		"OCDECK_TASK_ID":           "t1",
	}
	oc := newMockOC(true)
	judge := newFakeJudge(PermissionVerdictApprove)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	m.judge = judge
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.ensureAttentionState()
	rt.setPermJudgeReady()
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	<-store.entered // 扫描 goroutine 阻塞在 DB 读（judgeCtx 未取消）

	stopped := make(chan struct{})
	go func() { rt.stopAll(); close(stopped) }() // 先 cancel judgeCtx，再 SSE join，再 judgeWG.Wait
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stopAll must not be blocked by in-flight DB read (judgeCtx must cancel it)")
	}
	if !store.canceled.Load() {
		t.Error("blocked DB read must observe judgeCtx cancellation")
	}
	close(store.release)
	time.Sleep(50 * time.Millisecond)
	if got := len(judge.judgeCalls()); got != 0 {
		t.Errorf("scan cancelled before admission MUST NOT judge, calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("scan cancelled before admission MUST NOT reply, replies = %d", got)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.judgedPerms) != 0 {
		t.Errorf("judged-set MUST NOT register before full admission, got %v", rt.judgedPerms)
	}
}

// --- F11/7.2：ai-auto 判定审计（旁路，记录计数从调用判定器起算） ---

// fakePermAudit 记录审计记录并可注入写入错误（旁路失败注入用）。
type fakePermAudit struct {
	mu   sync.Mutex
	recs []PermAuditRecord
	err  error
}

func (f *fakePermAudit) Record(r PermAuditRecord) error {
	f.mu.Lock()
	f.recs = append(f.recs, r)
	f.mu.Unlock()
	return f.err
}

func (f *fakePermAudit) snapshot() []PermAuditRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PermAuditRecord(nil), f.recs...)
}

// TestJudgeAndReply_AuditMatrix 四 verdict × reply_result 全分支恰好一条记录
//（tasks 7.2）：ok 含 reply 字段；gone/unknown/unsupported/not_applicable 不含；
// FAILED/UNCERTAIN 判定终结即写 not_applicable。
func TestJudgeAndReply_AuditMatrix(t *testing.T) {
	newEnv := func(t *testing.T, verdict PermissionVerdict, judgeErr error) (*Manager, *mockOC, *taskRuntime, *fakePermAudit) {
		judge := newFakeJudge(verdict)
		judge.err = judgeErr
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm", "ls"))
		return m, oc, rt, fake
	}

	cases := []struct {
		name             string
		verdict          PermissionVerdict
		judgeErr         error
		replyErr         error
		wantVerdict      string
		wantReplyResult  string
		wantReply        string
		wantReplyPresent bool
	}{
		{name: "APPROVE+ok 含 reply=once", verdict: PermissionVerdictApprove, wantVerdict: "APPROVE", wantReplyResult: "ok", wantReply: "once", wantReplyPresent: true},
		{name: "REJECT+ok 含 reply=reject", verdict: PermissionVerdictReject, wantVerdict: "REJECT", wantReplyResult: "ok", wantReply: "reject", wantReplyPresent: true},
		{name: "APPROVE+gone", verdict: PermissionVerdictApprove, replyErr: opencode.ErrPermissionRequestGone, wantVerdict: "APPROVE", wantReplyResult: "gone"},
		{name: "APPROVE+unknown", verdict: PermissionVerdictApprove, replyErr: errors.New("conn reset"), wantVerdict: "APPROVE", wantReplyResult: "unknown"},
		{name: "APPROVE+unsupported", verdict: PermissionVerdictApprove, replyErr: opencode.ErrCapabilityUnsupported, wantVerdict: "APPROVE", wantReplyResult: "unsupported"},
		{name: "UNCERTAIN 合法输出 not_applicable", verdict: PermissionVerdictUncertain, wantVerdict: "UNCERTAIN", wantReplyResult: "not_applicable"},
		{name: "FAILED judge 失败 not_applicable", verdict: PermissionVerdictApprove, judgeErr: errors.New("llm down"), wantVerdict: "FAILED", wantReplyResult: "not_applicable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, oc, _, fake := newEnv(t, tc.verdict, tc.judgeErr)
			if tc.replyErr != nil {
				oc.replyPermissionErrByID = map[string]error{"r1": tc.replyErr}
			}
			m.judgeScan(m.getRuntime("t1"))
			eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
			time.Sleep(50 * time.Millisecond)
			recs := fake.snapshot()
			if len(recs) != 1 {
				t.Fatalf("audit records = %d, want exactly 1", len(recs))
			}
			rec := recs[0]
			if rec.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", rec.Verdict, tc.wantVerdict)
			}
			if rec.ReplyResult != tc.wantReplyResult {
				t.Errorf("reply_result = %q, want %q", rec.ReplyResult, tc.wantReplyResult)
			}
			if rec.TaskID != "t1" || rec.TaskName != "my task" || rec.RequestID != "r1" || rec.Permission != "bash" {
				t.Errorf("identity fields = %+v", rec)
			}
			if len(rec.Patterns) != 2 || rec.Patterns[0] != "rm" || rec.Patterns[1] != "ls" {
				t.Errorf("patterns = %v, want [rm ls]", rec.Patterns)
			}
			if _, err := time.Parse(time.RFC3339, rec.Time); err != nil {
				t.Errorf("time %q not RFC3339: %v", rec.Time, err)
			}
			if tc.wantReplyPresent {
				if rec.Reply != tc.wantReply {
					t.Errorf("reply = %q, want %q", rec.Reply, tc.wantReply)
				}
			} else if rec.Reply != "" {
				t.Errorf("reply = %q, want empty (非 ok 记录不含 reply)", rec.Reply)
			}
			// 不重复：稳定后仍恰好一条。
			time.Sleep(50 * time.Millisecond)
			if got := len(fake.snapshot()); got != 1 {
				t.Errorf("audit records after settle = %d, want 1", got)
			}
		})
	}
}

// TestJudgeAndReply_AuditNotApplicable_ClientConstructFail 判定后未发送分支：
// 回复客户端构造失败（serve env 缺失）→ 恰好一条 APPROVE + not_applicable 且不含 reply。
func TestJudgeAndReply_AuditNotApplicable_ClientConstructFail(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{}
	m.permAudit = fake
	// 摘除 serve 会话 env：taskOcClient 构造失败（active 门槛后）。
	proc := m.proc.(*mockProc)
	proc.mu.Lock()
	delete(proc.envValues, runtimeSessionName("t1"))
	proc.mu.Unlock()
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
	recs := fake.snapshot()
	if recs[0].Verdict != "APPROVE" || recs[0].ReplyResult != "not_applicable" || recs[0].Reply != "" {
		t.Errorf("record = %+v, want APPROVE + not_applicable without reply", recs[0])
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("replies = %d, want 0 (构造失败不发送)", got)
	}
}

// TestJudgeAndReply_AuditNotApplicable_OnInstanceReplaced 判定后未发送分支：
// Judge 阻塞期间实例被替换 → 发送前复核拒绝 → 恰好一条 APPROVE + not_applicable。
func TestJudgeAndReply_AuditNotApplicable_OnInstanceReplaced(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	release := make(chan struct{})
	judge.block = release
	m, _, oc, rt1, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{}
	m.permAudit = fake
	rt1.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt1)
	<-judge.entered
	// Manager 换上新 runtime（旧实例延迟结果不发送）。
	rt2 := m.newRuntime("t1")
	m.setRuntime("t1", rt2)
	close(release)
	<-judge.returns
	eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
	recs := fake.snapshot()
	if recs[0].Verdict != "APPROVE" || recs[0].ReplyResult != "not_applicable" || recs[0].Reply != "" {
		t.Errorf("record = %+v, want APPROVE + not_applicable without reply", recs[0])
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("replies = %d, want 0 (旧实例延迟结果不发送)", got)
	}
}

// TestJudgeScan_PreGateReject_ZeroAudit 调用判定器之前的门禁拒绝零审计记录
//（记录计数从调用判定器起算，tasks 7.2）。
func TestJudgeScan_PreGateReject_ZeroAudit(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{}
	m.permAudit = fake
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	// 未就绪（提交前）+ 停止两种门禁拒绝：均零记录。
	rt.mu.Lock()
	rt.permJudgeReady = false
	rt.mu.Unlock()
	m.judgeScan(rt)
	rt.mu.Lock()
	rt.permJudgeReady = true
	rt.judgeStopping = true
	rt.mu.Unlock()
	m.judgeScan(rt)
	time.Sleep(100 * time.Millisecond)
	if got := len(fake.snapshot()); got != 0 {
		t.Errorf("pre-gate rejection MUST NOT audit, records = %d", got)
	}
	if got := len(judge.judgeCalls()); got != 0 {
		t.Errorf("pre-gate rejection MUST NOT judge, calls = %d", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Errorf("pre-gate rejection MUST NOT reply, replies = %d", got)
	}
}

// TestJudgeAndReply_AuditWriteFailureBypass 审计写入失败旁路（spec 行为契约）：
// 注入写入错误后，判定与回复行为与无审计完全一致（恰好一次判定、一次回复）。
func TestJudgeAndReply_AuditWriteFailureBypass(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	fake := &fakePermAudit{err: errors.New("disk full")}
	m.permAudit = fake
	rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

	m.judgeScan(rt)
	eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
	eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })
	// 审计写入发生在 Reply 返回之后：等审计 fake 收敛再断言（防调度窗口）。
	eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
	replies := oc.replyPermissionCallsSnapshot()
	if replies[0].Reply != "once" || replies[0].RequestID != "r1" {
		t.Errorf("reply = %+v, want {r1 once}（写入失败不影响行为）", replies[0])
	}
	if got := len(fake.snapshot()); got != 1 {
		t.Errorf("audit attempts = %d, want 1（写入失败仍恰好尝试一条）", got)
	}
}

// TestJudgeAndReply_AuditDetailAndDegraded 判定详情与降级端到端（tasks 8.2/8.3）：
// ① metadata 缺失的 bash 请求：提取产物（Detail.Degraded=missing_critical）到达
// Judge 输入；审计 detail 以原因前缀开头（verdict 仍按 fake 判定值）。
// ② metadata 携带 command：Detail.Command 到达 Judge；审计 detail 含同源摘要。
// ③ 全部记录 detail 字段必有。
func TestJudgeAndReply_AuditDetailAndDegraded(t *testing.T) {
	t.Run("metadata 缺失 → Judge 收到降级标记 + UNCERTAIN 审计（真实短路输出组合）", func(t *testing.T) {
		// 真实 ai.PermJudge 对 Degraded 非空短路返回 uncertain + nil（ai 层
		// DegradedShortCircuit 测试保证）；此处 fake 以相同输出特征组合断言
		// task 侧行为：UNCERTAIN 审计 + not_applicable + 零回复 + 降级前缀 detail。
		judge := newFakeJudge(PermissionVerdictUncertain)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		// 无 metadata 的 asked（观察层归 nil → 提取记 missing_critical）。
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		if got := judge.judgeCalls()[0].Detail.Degraded; got != DegradedMissing {
			t.Errorf("Judge input Detail.Degraded = %q, want missing_critical", got)
		}
		rec := fake.snapshot()[0]
		if rec.Verdict != "UNCERTAIN" || rec.ReplyResult != "not_applicable" {
			t.Errorf("verdict/reply_result = %v/%v, want UNCERTAIN/not_applicable（短路）", rec.Verdict, rec.ReplyResult)
		}
		if !strings.HasPrefix(rec.Detail, DegradedMissing+": ") {
			t.Errorf("audit detail = %q, want prefix %q", rec.Detail, DegradedMissing+": ")
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("degraded MUST NOT reply, replies = %d", got)
		}
	})
	t.Run("总量阶段超限（无字段级截断）→ 零回复 + UNCERTAIN 审计", func(t *testing.T) {
		// F1 端到端：20×500B patch 无字段级截断、纯总量超限 → Judge 输入
		// Degraded=truncated_critical（真实实现短路）→ 零 Reply、恰好一条
		// UNCERTAIN 审计且 detail 带 truncated 前缀。
		judge := newFakeJudge(PermissionVerdictUncertain)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		var sb strings.Builder
		sb.WriteString(`{"files":[`)
		for i := 0; i < 20; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"type":"update","relativePath":"f` + strconv.Itoa(i) + `","patch":"` + strings.Repeat("p", 500) + `"}`)
		}
		sb.WriteString(`]}`)
		ev := permAsked("r1", "s1", "apply_patch", "src")
		ev.Metadata = metaRaw(t, sb.String())
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		if got := judge.judgeCalls()[0].Detail.Degraded; got != DegradedTruncated {
			t.Errorf("Judge input Detail.Degraded = %q, want truncated_critical（总量裁剪回写，F1）", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("degraded MUST NOT reply, replies = %d", got)
		}
		rec := fake.snapshot()[0]
		if rec.Verdict != "UNCERTAIN" || !strings.HasPrefix(rec.Detail, DegradedTruncated+": ") {
			t.Errorf("record = %+v, want UNCERTAIN with truncated prefix", rec)
		}
	})
	t.Run("metadata 携带 command → 同源摘要进审计", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, _, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		ev := permAsked("r1", "s1", "bash", "rm")
		ev.Metadata = metaRaw(t, `{"command":"rm -rf build"}`)
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		if got := judge.judgeCalls()[0].Detail.Command; got != "rm -rf build" {
			t.Errorf("Judge input Detail.Command = %q, want rm -rf build", got)
		}
		if got := fake.snapshot()[0].Detail; !strings.Contains(got, "command=rm -rf build") {
			t.Errorf("audit detail = %q, want contain command=rm -rf build", got)
		}
	})
	t.Run("F10 move 成员巨大 movePath → 关键槽总量裁剪 → UNCERTAIN 审计", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictUncertain)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		ev := permAsked("r1", "s1", "apply_patch", "src")
		ev.Metadata = metaRaw(t, `{"files":[{"type":"move","relativePath":"b.go","movePath":"` + strings.Repeat("m", 9000) + `"}]}`)
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		if got := judge.judgeCalls()[0].Detail.Degraded; got != DegradedTruncated {
			t.Errorf("Judge input Detail.Degraded = %q, want truncated_critical（movePath 关键槽被裁，F10）", got)
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("degraded MUST NOT reply, replies = %d", got)
		}
		rec := fake.snapshot()[0]
		if rec.Verdict != "UNCERTAIN" || !strings.HasPrefix(rec.Detail, DegradedTruncated+": ") {
			t.Errorf("record = %+v, want UNCERTAIN with truncated prefix", rec)
		}
	})
	t.Run("R1-F1 合法 command 与 [null] 并存 → malformed 审计", func(t *testing.T) {
		// directories 字面 null 成员（R1-F1）→ malformed_critical（真实短路输出特征：
		// uncertain + nil）→ UNCERTAIN 审计 + malformed 前缀 + 零回复。
		judge := newFakeJudge(PermissionVerdictUncertain)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		ev := permAsked("r1", "s1", "external_directory", "/outside")
		ev.Metadata = metaRaw(t, `{"command":"cat /outside/a","directories":[null]}`)
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		if got := judge.judgeCalls()[0].Detail.Degraded; got != DegradedMalformed {
			t.Errorf("Judge input Detail.Degraded = %q, want malformed_critical", got)
		}
		rec := fake.snapshot()[0]
		if rec.Verdict != "UNCERTAIN" || rec.ReplyResult != "not_applicable" {
			t.Errorf("verdict/reply_result = %v/%v, want UNCERTAIN/not_applicable（短路）", rec.Verdict, rec.ReplyResult)
		}
		if !strings.HasPrefix(rec.Detail, DegradedMalformed+": ") {
			t.Errorf("audit detail = %q, want prefix %q", rec.Detail, DegradedMalformed+": ")
		}
		if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
			t.Errorf("degraded MUST NOT reply, replies = %d", got)
		}
	})
	t.Run("R1-F2 正常 move → 审计 detail 含源与目标路径", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		ev := permAsked("r1", "s1", "apply_patch", "src")
		ev.Metadata = metaRaw(t, `{"files":[{"type":"move","relativePath":"a.go","movePath":"../outside/b.go"}]}`)
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(judge.judgeCalls()) == 1 })
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		got := fake.snapshot()[0].Detail
		// 源与目标都在摘要中（不同移动目标 MUST NOT 产生相同摘要，R1-F2）。
		if !strings.Contains(got, "a.go") || !strings.Contains(got, "../outside/b.go") {
			t.Errorf("audit detail = %q, want contain source a.go and target ../outside/b.go", got)
		}
		replies := oc.replyPermissionCallsSnapshot()
		if len(replies) != 1 || replies[0].Reply != "once" {
			t.Errorf("replies = %+v, want once", replies)
		}
	})
	t.Run("FAILED 分支也含 detail", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictReject)
		judge.err = errors.New("llm down")
		m, _, _, rt, _ := newAutoTaskEnv(t, judge)
		fake := &fakePermAudit{}
		m.permAudit = fake
		ev := permAsked("r1", "s1", "bash", "rm")
		ev.Metadata = metaRaw(t, `{"command":"ls"}`)
		rt.ensureAttentionState().applyAttentionEvent(ev)

		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(fake.snapshot()) == 1 })
		rec := fake.snapshot()[0]
		if rec.Verdict != "FAILED" || rec.ReplyResult != "not_applicable" {
			t.Errorf("verdict/reply_result = %v/%v, want FAILED/not_applicable", rec.Verdict, rec.ReplyResult)
		}
		if !strings.Contains(rec.Detail, "command=ls") {
			t.Errorf("FAILED record detail = %q, want same-source summary", rec.Detail)
		}
	})
}
