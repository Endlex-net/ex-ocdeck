// lane_b_notify_test.go Lane B task 层小扩展（task-notifications design D3）：
// agentStatus 每 session retry 详情保留（apply 点扩展）、RunStatusDetail 选择规则
// （Next>0 最小→Next==0 排后→sessionID 字典序）、组合快照端口
// TaskNotificationSnapshot。不变量：busy>retry>idle 聚合与既有事件发布行为不变
// （既有 p18 套件全量回归）。
package task

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	appnotif "ocdeck/internal/application/notification"
	"ocdeck/internal/infrastructure/opencode"
)

// p18RetryStatusEvent 构造携带 retry 详情的 session.status 事件（真实契约形状）。
func p18RetryStatusEvent(sid string, attempt int, message string, next int64) opencode.Event {
	props := map[string]interface{}{
		"sessionID": sid,
		"status": map[string]interface{}{
			"type": "retry",
		},
	}
	if attempt != 0 || message != "" || next != 0 {
		st := props["status"].(map[string]interface{})
		st["attempt"] = float64(attempt)
		st["message"] = message
		if next != 0 {
			st["next"] = float64(next)
		}
	}
	return opencode.Event{Type: "session.status", Properties: props}
}

// TestRetryDetailRetention_SSEEventsOnly SSE 状态事件写入/清除每 session retry
// 详情：retry 事件写入最近详情，非 retry 状态清除。
func TestRetryDetailRetention_SSEEventsOnly(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1")
	ctx := context.Background()

	// retry 事件 → 详情保留。
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent("s1", 2, "rate limit", 1787710728671)); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	d, ok := a.retryDetail()
	if !ok || d != (appnotif.RetryDetail{Attempt: 2, Message: "rate limit", Next: 1787710728671}) {
		t.Fatalf("retry detail = %+v ok=%v", d, ok)
	}
	// 聚合仍为 retry（详情不影响投影）。
	if got := a.snapshotValue(); got != "retry" {
		t.Fatalf("aggregate = %q, want retry", got)
	}

	// busy 事件 → 详情清除（非 retry 态不保留）。
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18StatusEvent("s1", "busy")); err != nil {
		t.Fatalf("handleSSEEvent busy: %v", err)
	}
	if _, ok := a.retryDetail(); ok {
		t.Fatal("retry detail must be cleared on non-retry status")
	}
	if got := a.snapshotValue(); got != "busy" {
		t.Fatalf("aggregate after busy = %q", got)
	}
	// 发布行为不变：idle→retry→busy 各恰一次（详情扩展不影响 delta/发布）。
	if rs := pub.runStatus(); len(rs) != 2 || rs[0].To != "retry" || rs[1].From != "retry" || rs[1].To != "busy" {
		t.Fatalf("run_status publishes = %+v", rs)
	}
	_ = rt
}

// TestRetryDetailValidityRule 有效性规则（design D3 唯一成立条件）：
// Attempt>0 且 TrimSpace(Message) 非空才有效；仅 Next/仅 Attempt/空白 Message
// 均不可得。
func TestRetryDetailValidityRule(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	_, a := p18ReadyRuntime(m, "t1", "s1")
	ctx := context.Background()

	cases := []struct {
		name    string
		event   opencode.Event
		wantOK  bool
		wantDet appnotif.RetryDetail
	}{
		{
			name:    "valid detail",
			event:   p18RetryStatusEvent("s1", 1, "m", 100),
			wantOK:  true,
			wantDet: appnotif.RetryDetail{Attempt: 1, Message: "m", Next: 100},
		},
		{
			name:   "blank message invalid",
			event:  p18RetryStatusEvent("s1", 1, "   ", 100),
			wantOK: false,
		},
		{
			name:   "missing attempt invalid",
			event:  p18RetryStatusEvent("s1", 0, "m", 100),
			wantOK: false,
		},
		{
			name:    "no next still valid",
			event:   p18RetryStatusEvent("s1", 3, "m", 0),
			wantOK:  true,
			wantDet: appnotif.RetryDetail{Attempt: 3, Message: "m"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.handleSSEEvent(ctx, "t1", "/wt", tc.event); err != nil {
				t.Fatalf("handleSSEEvent: %v", err)
			}
			d, ok := a.retryDetail()
			if ok != tc.wantOK || (ok && d != tc.wantDet) {
				t.Fatalf("retryDetail = (%+v, %v), want (%+v, %v)", d, ok, tc.wantDet, tc.wantOK)
			}
			// 复位为 idle 便于下一用例。
			if err := m.handleSSEEvent(ctx, "t1", "/wt", p18StatusEvent("s1", "idle")); err != nil {
				t.Fatalf("reset: %v", err)
			}
		})
	}
}

// TestRetryDetailSelectionRule 多 session 同时 retry 的确定选择：有效详情过滤后
// Next>0 最小者优先、Next==0 排后、并列 sessionID 字典序最小；无有效 → false。
func TestRetryDetailSelectionRule(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{
		{TaskID: "t1", SessionID: "s1"}, {TaskID: "t1", SessionID: "s2"},
		{TaskID: "t1", SessionID: "s3"},
	}
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	_, a := p18ReadyRuntime(m, "t1", "s1", "s2", "s3")
	ctx := context.Background()

	send := func(sid string, attempt int, msg string, next int64) {
		t.Helper()
		if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent(sid, attempt, msg, next)); err != nil {
			t.Fatalf("retry event %s: %v", sid, err)
		}
	}

	// s1 Next=200、s2 Next=100 → 取 s2（Next 最小）。
	send("s1", 1, "a", 200)
	send("s2", 2, "b", 100)
	if d, ok := a.retryDetail(); !ok || d.Attempt != 2 || d.Message != "b" || d.Next != 100 {
		t.Fatalf("selection = %+v ok=%v, want s2 (next=100)", d, ok)
	}
	// s3 无 Next（Next=0 排最后）→ 仍取 s2。
	send("s3", 3, "c", 0)
	if d, _ := a.retryDetail(); d.Attempt != 2 {
		t.Fatalf("selection with next-less session = %+v, want s2", d)
	}
	// s2 离开 retry → 次优 s1（Next=200）。
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18StatusEvent("s2", "idle")); err != nil {
		t.Fatalf("s2 idle: %v", err)
	}
	if d, ok := a.retryDetail(); !ok || d.Message != "a" || d.Next != 200 {
		t.Fatalf("selection after s2 leaves = %+v ok=%v, want s1", d, ok)
	}
	// 并列（同为 Next=0）→ sessionID 字典序最小。
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18StatusEvent("s1", "idle")); err != nil {
		t.Fatalf("s1 idle: %v", err)
	}
	send("s3", 3, "c", 0)
	send("s1", 1, "a2", 0)
	if d, ok := a.retryDetail(); !ok || d.Message != "a2" {
		t.Fatalf("tie selection = %+v ok=%v, want s1 (lexicographic min)", d, ok)
	}
	// 全部无效（空白 message）→ false。
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent("s3", 3, "  ", 0)); err != nil {
		t.Fatalf("s3 invalid: %v", err)
	}
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18StatusEvent("s1", "idle")); err != nil {
		t.Fatalf("s1 idle: %v", err)
	}
	if _, ok := a.retryDetail(); ok {
		t.Fatal("no valid detail must return ok=false")
	}
}

// TestTaskNotificationSnapshotComposite 组合快照端口：任务行 + attention +
// run_status + retry 详情单次读取；无 runtime 时仅任务行。
func TestTaskNotificationSnapshotComposite(t *testing.T) {
	store := newMockStore()
	row := seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, _ := p18ReadyRuntime(m, "t1", "s1")
	ctx := context.Background()

	// attention pending + retry 状态 + 详情（attention 懒初始化与 ensure 先例一致）。
	rt.ensureAttentionState()
	if rt.applyAttentionEvent(opencode.AttentionEvent{
		Kind: opencode.AttentionAsked, Type: opencode.AttentionQuestion,
		RequestID: "q1", SessionID: "s1",
		Questions: []opencode.QuestionItem{{Header: "h", Question: "用哪个分支？"}},
	}) != true {
		t.Fatal("applyAttentionEvent must change snapshot")
	}
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent("s1", 2, "rate limit", 7)); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}

	snap, err := m.TaskNotificationSnapshot(ctx, "t1")
	if err != nil {
		t.Fatalf("TaskNotificationSnapshot: %v", err)
	}
	if snap.Task.ID != "t1" || snap.Task.Name != row.Name || snap.Task.Status != StatusActive {
		t.Fatalf("task ref = %+v (row %+v)", snap.Task, row)
	}
	if snap.InstVersion != string(rt.instVersion) {
		t.Fatalf("snapshot instVersion = %q, want current runtime %q", snap.InstVersion, rt.instVersion)
	}
	if len(snap.Attention.Questions) != 1 || snap.Attention.Questions[0].ID != "q1" {
		t.Fatalf("attention = %+v", snap.Attention)
	}
	if snap.RunStatus != "retry" || !snap.HasRetryDetail ||
		snap.RetryDetail != (appnotif.RetryDetail{Attempt: 2, Message: "rate limit", Next: 7}) {
		t.Fatalf("run status/detail = %q %+v ok=%v", snap.RunStatus, snap.RetryDetail, snap.HasRetryDetail)
	}

	// 无 runtime（清理后）：attention/run_status 退化为空，任务行仍在、无实例。
	m.clearRuntime("t1")
	snap, err = m.TaskNotificationSnapshot(ctx, "t1")
	if err != nil {
		t.Fatalf("snapshot after clear: %v", err)
	}
	if snap.Task.ID != "t1" || snap.RunStatus != "" || len(snap.Attention.Questions) != 0 || snap.HasRetryDetail || snap.InstVersion != "" {
		t.Fatalf("snapshot without runtime = %+v", snap)
	}
}

// TestNotifyCompositeBlockedByStateLocks B5 补齐：组合快照经 attention.mu 与
// agentStatus.mu 完成拷贝（固定锁序）——外部持有任一状态锁时快照不得完成
// （确定性锁竞争证明），释放后完成且同代完整。
func TestNotifyCompositeBlockedByStateLocks(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, _ := p18ReadyRuntime(m, "t1", "s1")
	ctx := context.Background()
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent("s1", 2, "locked", 9)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	att := rt.ensureAttentionState()
	att.applyAttentionEvent(opencode.AttentionEvent{
		Kind: opencode.AttentionAsked, Type: opencode.AttentionQuestion,
		RequestID: "q1", SessionID: "s1",
		Questions: []opencode.QuestionItem{{Header: "h", Question: "内容"}},
	})
	ag := rt.agentStatus

	runComposite := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			inst, attSnap, rs, d, ok := rt.notifyComposite()
			// 完整性：同代完整形态（retry + 详情 + pending q1）。
			if inst == "" || rs != "retry" || !ok || d.Message != "locked" ||
				len(attSnap.Questions) != 1 || attSnap.Questions[0].ID != "q1" {
				t.Errorf("composite incomplete: inst=%q rs=%q detail=%+v ok=%v att=%+v", inst, rs, d, ok, attSnap)
			}
		}()
		return done
	}

	// 持有 attention.mu：快照必须阻塞在其锁上，不得提前完成。
	att.mu.Lock()
	done := runComposite()
	select {
	case <-done:
		att.mu.Unlock()
		t.Fatal("composite must block while attention.mu held")
	case <-time.After(50 * time.Millisecond):
	}
	att.mu.Unlock()
	<-done

	// 持有 agentStatus.mu：锁序 attention → agent，同样必须阻塞。
	ag.mu.Lock()
	done = runComposite()
	select {
	case <-done:
		ag.mu.Unlock()
		t.Fatal("composite must block while agentStatus.mu held")
	case <-time.After(50 * time.Millisecond):
	}
	ag.mu.Unlock()
	<-done
}

// TestTaskNotificationSnapshotCoherentAcrossRuntimeReplace B5：并发 runtime
// 替换/清理下组合快照内部一致——快照实例与运行态同代捕获（instVersion 对应
// 该代 attention/agent 状态，或清理后的空态），不出现跨代撕裂；聚合状态与
// retry 详情单次锁内计算（-race 验证）。
func TestTaskNotificationSnapshotCoherentAcrossRuntimeReplace(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, _, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	ctx := context.Background()

	// 代 A：retry + 详情 A + pending qA。
	rtA, _ := p18ReadyRuntime(m, "t1", "s1")
	if err := m.handleSSEEvent(ctx, "t1", "/wt", p18RetryStatusEvent("s1", 1, "detail-A", 5)); err != nil {
		t.Fatalf("retry A: %v", err)
	}
	rtA.ensureAttentionState()
	rtA.applyAttentionEvent(opencode.AttentionEvent{
		Kind: opencode.AttentionAsked, Type: opencode.AttentionQuestion,
		RequestID: "qA", SessionID: "s1",
		Questions: []opencode.QuestionItem{{Header: "h", Question: "A"}},
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 持续替换 runtime：clear A → 建 B（idle、无 pending）→ 循环
		defer wg.Done()
		gen := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			gen++
			m.clearRuntime("t1")
			rt := m.newRuntime("t1")
			m.setRuntime("t1", rt)
		}
	}()
	coherent := func() (ok bool, msg string) {
		for i := 0; i < 200; i++ {
			snap, err := m.TaskNotificationSnapshot(ctx, "t1")
			if err != nil {
				return false, fmt.Sprintf("snapshot err: %v", err)
			}
			switch snap.InstVersion {
			case "":
				// 无 runtime：运行态必须为空（非 active 语义）。
				if snap.RunStatus != "" || snap.HasRetryDetail || len(snap.Attention.Questions) > 0 {
					return false, fmt.Sprintf("no-runtime snapshot has runtime state: %+v", snap)
				}
			default:
				// 有实例：必须是同一代的完整形态——retry+detailA+qA（A 代活体）
				// 或清理后的空运行态；不得出现混合（如 A 代令牌配 B 代空 attention
				// 且 RunStatus 仍 retry —— 见下方显式校验）。
				if snap.RunStatus == "retry" {
					if !snap.HasRetryDetail || snap.RetryDetail.Message != "detail-A" || len(snap.Attention.Questions) != 1 || snap.Attention.Questions[0].ID != "qA" {
						return false, fmt.Sprintf("torn snapshot: %+v", snap)
					}
				} else {
					// 清理/新代（idle 未对账）：RunStatus 空/待对账，attention 空。
					if snap.HasRetryDetail || len(snap.Attention.Questions) > 0 {
						return false, fmt.Sprintf("cleared snapshot carries stale detail: %+v", snap)
					}
				}
			}
		}
		return true, ""
	}
	ok, msg := coherent()
	close(stop)
	wg.Wait()
	if !ok {
		t.Fatalf("snapshot coherence violated: %s", msg)
	}
}
