// perm_verdict_event_test.go verdict 事件发布与快照出口（task-permission-mode
// tasks 3.1/3.2，design D1）：settlePermVerdict 先提交后发布的时序断言、epoch 失配
// 不写状态不发事件、judgeAndReply D1 分支表全枚举、TaskNotificationSnapshot 的
// EffectivePermissionMode（与 PermissionModeView 同源）与当前 epoch 判定状态映射
// （旧 epoch 残余不输出）。
package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	appnotif "ocdeck/internal/application/notification"
	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/opencode"
)

// recordingPublisher 记录发布事件并在发布时刻执行回调（时序断言）。
type recordingPublisher struct {
	mu        sync.Mutex
	events    []ocdeckevent.Event
	onPublish func(ev ocdeckevent.Event)
}

func (p *recordingPublisher) Publish(ev ocdeckevent.Event) {
	if p.onPublish != nil {
		p.onPublish(ev)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPublisher) snapshot() []ocdeckevent.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ocdeckevent.Event(nil), p.events...)
}

// permVerdictState 读取判定状态记录（id → {state, epoch}）。
func permVerdictState(rt *taskRuntime, id string) (permVerdictRecord, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rec, ok := rt.permVerdicts[id]
	return rec, ok
}

// TestSettlePermVerdict_CommitBeforePublish 先（rt.mu 内）提交判定状态、后发布
// 唤醒事件；发布时刻状态必须已可读（D1 不变量）；epoch 失配不写状态不发事件。
func TestSettlePermVerdict_CommitBeforePublish(t *testing.T) {
	m, _, rt := newPermModeEnv(t, "ai-auto", "ai-auto")
	rec := &recordingPublisher{}
	m.publish = rec

	committedDuringPublish := false
	rec.onPublish = func(ocdeckevent.Event) {
		recState, ok := permVerdictState(rt, "r1")
		committedDuringPublish = ok && recState.state == appnotif.VerdictManualRequired
	}
	m.settlePermVerdict(rt, 0, "r1", permVerdictManualRequired)
	if !committedDuringPublish {
		t.Fatal("verdict state must be committed before the wake event is published")
	}
	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Topic != ocdeckevent.TopicServeRuntime || ev.Type != ocdeckevent.TypeServeRuntimePermissionVerdict ||
		ev.RID != string(rt.instVersion) {
		t.Fatalf("event = %+v, want serve_runtime/permission_verdict RID=%s", ev, rt.instVersion)
	}
	if p, ok := ev.Payload.(ocdeckevent.ServeRuntimeTaskPayload); !ok || p.TaskID != rt.taskID {
		t.Fatalf("payload = %+v", ev.Payload)
	}

	// epoch 失配：不写状态、不发事件。
	m.settlePermVerdict(rt, 7, "r2", permVerdictManualRequired)
	if _, ok := permVerdictState(rt, "r2"); ok {
		t.Fatal("epoch mismatch MUST NOT write verdict state")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("events after mismatch = %d, want 1（不发事件）", got)
	}
}

// TestJudgeAndReply_VerdictBranchTable D1 分支表全枚举：各终结分支的判定状态
// 映射与事件发布（状态提交先于发布）；REJECT/UNCERTAIN/FAILED/unknown/unsupported/
// 未发送均不产生成功回复。
func TestJudgeAndReply_VerdictBranchTable(t *testing.T) {
	cases := []struct {
		name       string
		verdict    PermissionVerdict
		judgeErr   error
		replyErr   error
		dropEnv    bool // true：摘除 serve env 使 taskOcClient 构造失败（发送前未发送）
		wantState  string
		wantReply  bool // 是否期望一次回复调用（ok/unknown/unsupported 均发起过调用）
		wantEvents int
	}{
		{name: "APPROVE+ok→settled", verdict: PermissionVerdictApprove, wantState: permVerdictSettledNoNotify, wantReply: true, wantEvents: 1},
		{name: "APPROVE+gone→settled", verdict: PermissionVerdictApprove, replyErr: opencode.ErrPermissionRequestGone, wantState: permVerdictSettledNoNotify, wantReply: true, wantEvents: 1},
		{name: "REJECT→manual", verdict: PermissionVerdictReject, wantState: permVerdictManualRequired, wantEvents: 1},
		{name: "UNCERTAIN→manual", verdict: PermissionVerdictUncertain, wantState: permVerdictManualRequired, wantEvents: 1},
		{name: "FAILED→manual", verdict: PermissionVerdictApprove, judgeErr: errors.New("llm down"), wantState: permVerdictManualRequired, wantEvents: 1},
		{name: "APPROVE+unknown→manual", verdict: PermissionVerdictApprove, replyErr: errors.New("conn reset"), wantState: permVerdictManualRequired, wantReply: true, wantEvents: 1},
		{name: "APPROVE+unsupported→manual", verdict: PermissionVerdictApprove, replyErr: opencode.ErrCapabilityUnsupported, wantState: permVerdictManualRequired, wantReply: true, wantEvents: 1},
		{name: "构造失败未发送→manual", verdict: PermissionVerdictApprove, dropEnv: true, wantState: permVerdictManualRequired, wantEvents: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			judge := newFakeJudge(tc.verdict)
			judge.err = tc.judgeErr
			m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
			rec := &recordingPublisher{}
			m.publish = rec
			if tc.replyErr != nil {
				oc.replyPermissionErrByID = map[string]error{"r1": tc.replyErr}
			}
			if tc.dropEnv {
				proc := m.proc.(*mockProc)
				proc.mu.Lock()
				delete(proc.envValues, runtimeSessionName("t1"))
				proc.mu.Unlock()
			}
			rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
			m.judgeScan(rt)
			eventually(t, 2*time.Second, func() bool {
				recState, ok := permVerdictState(rt, "r1")
				return ok && recState.state != permVerdictInflight
			})
			recState, _ := permVerdictState(rt, "r1")
			if recState.state != tc.wantState || recState.epoch != 0 {
				t.Fatalf("verdict record = %+v, want {epoch 0, %s}", recState, tc.wantState)
			}
			if got := len(rec.snapshot()); got != tc.wantEvents {
				t.Fatalf("events = %d, want %d", got, tc.wantEvents)
			}
			replies := oc.replyPermissionCallsSnapshot()
			if tc.wantReply && len(replies) != 1 {
				t.Fatalf("reply calls = %d, want 1", len(replies))
			}
			if !tc.wantReply && len(replies) != 0 {
				t.Fatalf("reply calls = %d, want 0", len(replies))
			}
		})
	}
}

// TestJudgeAndReply_GateFailBeforeJudge 无终态：调用判定器前门禁拒绝（ctx 取消）
// 零事件，判定状态保持 inflight（未定论，deadline 兜底语义）。
func TestJudgeAndReply_GateFailBeforeJudge(t *testing.T) {
	judge := newFakeJudge(PermissionVerdictApprove)
	m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
	rec := &recordingPublisher{}
	m.publish = rec
	rt.mu.Lock()
	rt.judgedPerms["r1"] = struct{}{}
	rt.permVerdicts["r1"] = permVerdictRecord{epoch: 0, state: permVerdictInflight}
	rt.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.judgeAndReply(ctx, rt, rt.instVersion, 0, PermissionJudgeInput{}, PendingPermission{PermissionRequest: permReq("r1", "s1", "bash", "rm")})
	rec2, ok := permVerdictState(rt, "r1")
	if !ok || rec2.state != permVerdictInflight {
		t.Fatalf("verdict record = %+v ok=%v, want inflight preserved", rec2, ok)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
	if got := len(judge.judgeCalls()); got != 0 {
		t.Fatalf("judge calls = %d, want 0", got)
	}
	if got := len(oc.replyPermissionCallsSnapshot()); got != 0 {
		t.Fatalf("reply calls = %d, want 0", got)
	}
}

// TestTaskNotificationSnapshot_PermissionState 快照出口（tasks 3.1）：判定状态映射
// 仅含当前 epoch；EffectivePermissionMode 为 D6 推导值且与 PermissionModeView 同源；
// 无 runtime 时读 DB 行、Verdicts 为 nil；持久化值损坏 fail-closed 返回错误。
func TestTaskNotificationSnapshot_PermissionState(t *testing.T) {
	t.Run("runtime 在位：当前 epoch 状态与同源 effective", func(t *testing.T) {
		judge := newFakeJudge(PermissionVerdictApprove)
		m, _, oc, rt, _ := newAutoTaskEnv(t, judge)
		rt.ensureAttentionState().applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
		m.judgeScan(rt)
		eventually(t, 2*time.Second, func() bool { return len(oc.replyPermissionCallsSnapshot()) == 1 })

		snap, err := m.TaskNotificationSnapshot(context.Background(), "t1")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		rec, ok := snap.Verdicts["r1"]
		if !ok || rec.State != appnotif.VerdictSettledNoNotify || rec.Epoch != 0 {
			t.Fatalf("verdicts[p1 r1] = %+v ok=%v, want {0, settled_no_notify}", rec, ok)
		}
		view, err := m.PermissionModeView(context.Background(), "t1")
		if err != nil {
			t.Fatalf("PermissionModeView: %v", err)
		}
		if snap.Task.EffectivePermissionMode != view.EffectivePermissionMode {
			t.Fatalf("snapshot effective %q != view %q（必须同源）",
				snap.Task.EffectivePermissionMode, view.EffectivePermissionMode)
		}

		// 切出+切入（epoch 推进到 2）后：r1 的旧 epoch 残余 MUST NOT 出现在快照。
		m.convergePermissionModeSave("t1", PermissionModeAsk)
		m.convergePermissionModeSave("t1", PermissionModeAIAuto)
		snap, err = m.TaskNotificationSnapshot(context.Background(), "t1")
		if err != nil {
			t.Fatalf("snapshot after epoch bump: %v", err)
		}
		if _, stale := snap.Verdicts["r1"]; stale {
			t.Fatal("stale-epoch verdict state MUST NOT be exported")
		}
		if snap.Task.EffectivePermissionMode != PermissionModeAIAuto {
			t.Fatalf("effective after switch-in = %q, want ai-auto", snap.Task.EffectivePermissionMode)
		}
	})
	t.Run("无 runtime：读 DB 行推导且无判定状态", func(t *testing.T) {
		m, _, _ := newPermModeEnv(t, "ai-auto", "ai-auto")
		m.clearRuntime("t1")
		snap, err := m.TaskNotificationSnapshot(context.Background(), "t1")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if snap.Task.EffectivePermissionMode != PermissionModeAIAuto {
			t.Fatalf("effective = %q, want ai-auto", snap.Task.EffectivePermissionMode)
		}
		if snap.Verdicts != nil {
			t.Fatalf("verdicts = %v, want nil（无 runtime）", snap.Verdicts)
		}
	})
	t.Run("持久化损坏 fail-closed", func(t *testing.T) {
		m, store, _ := newPermModeEnv(t, "ask", "ask")
		m.clearRuntime("t1")
		store.mutTask("t1", func(r *TaskRow) { r.PermissionMode = "bogus" })
		if _, err := m.TaskNotificationSnapshot(context.Background(), "t1"); err == nil {
			t.Fatal("corrupted persisted mode must fail closed")
		}
	})
}
