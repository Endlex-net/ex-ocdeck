// session_error_dispatch_test.go 验证 task 层 SSE 派发链接入（task-notifications
// design D2 / tasks 1.6）：归属已确认 session 的合法 session.error 经 handleSSEEvent
// （attention/status 同一消费点）发布 serve_runtime.session_error；孤儿/非法事件
// fail-closed 静默忽略；一次性事件语义（重复错误逐条发布，不并入状态投影）。
package task

import (
	"context"
	"encoding/json"
	"testing"

	ocdeckevent "ocdeck/internal/domain/event"
	"ocdeck/internal/infrastructure/opencode"
)

// sessionErrorDispatchEvents 过滤 publisher 收到的 serve_runtime.session_error 事件。
func sessionErrorDispatchEvents(t *testing.T, p *p18Publisher) []ocdeckevent.Event {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ocdeckevent.Event
	for _, ev := range p.events {
		if ev.Type == ocdeckevent.TypeServeRuntimeSessionError {
			out = append(out, ev)
		}
	}
	return out
}

// dispatchSessionErrorEvent 按真实契约形状构造 session.error 事件（fixture
// session_status_events.jsonl 第 6 行同形）；statusCode 以 json.Number 注入，
// 镜像生产 parseEvent 的 UseNumber 解码。mutateData 允许覆写 data 字段。
func dispatchSessionErrorEvent(sid string, mutateData func(map[string]interface{})) opencode.Event {
	data := map[string]interface{}{
		"message":     "p17 stub rate limit",
		"statusCode":  json.Number("429"),
		"isRetryable": true,
	}
	if mutateData != nil {
		mutateData(data)
	}
	return opencode.Event{
		Type: "session.error",
		Properties: map[string]interface{}{
			"sessionID": sid,
			"error":     map[string]interface{}{"name": "APIError", "data": data},
		},
	}
}

// TestSessionErrorDispatch_OwnedSessionPublishes 已归属 session 的合法事件：
// 发布一帧 serve_runtime.session_error，Topic/RID/payload 逐字段对齐 D2 契约；
// 重复错误逐条发布（一次性事件，与 run_status 状态投影的去重语义相反）。
func TestSessionErrorDispatch_OwnedSessionPublishes(t *testing.T) {
	store := newMockStore()
	wtPath := "/data/worktrees/p1/t1"
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	ctx := context.Background()

	if err := m.handleSSEEvent(ctx, "t1", wtPath, dispatchSessionErrorEvent("s1", nil)); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	evs := sessionErrorDispatchEvents(t, pub)
	if len(evs) != 1 {
		t.Fatalf("published session_error events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Topic != ocdeckevent.TopicServeRuntime {
		t.Fatalf("topic = %q, want %q", ev.Topic, ocdeckevent.TopicServeRuntime)
	}
	if ev.RID != string(rt.instVersion) {
		t.Fatalf("RID = %q, want instVersion %q", ev.RID, string(rt.instVersion))
	}
	pl, ok := ev.Payload.(ocdeckevent.ServeRuntimeSessionErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T", ev.Payload)
	}
	if pl.TaskID != "t1" || pl.SessionID != "s1" || pl.Name != "APIError" || pl.Message != "p17 stub rate limit" {
		t.Fatalf("payload required fields mismatch: %+v", pl)
	}
	if pl.StatusCode == nil || *pl.StatusCode != 429 {
		t.Fatalf("payload status_code = %v, want *429", pl.StatusCode)
	}
	if pl.IsRetryable == nil || *pl.IsRetryable != true {
		t.Fatalf("payload is_retryable = %v, want *true", pl.IsRetryable)
	}

	// 一次性事件：第二条 session.error 同样发布。
	second := dispatchSessionErrorEvent("s1", func(d map[string]interface{}) { d["message"] = "second failure" })
	if err := m.handleSSEEvent(ctx, "t1", wtPath, second); err != nil {
		t.Fatalf("handleSSEEvent second: %v", err)
	}
	if evs := sessionErrorDispatchEvents(t, pub); len(evs) != 2 {
		t.Fatalf("repeated session.error must publish again (one-shot semantics), events = %d", len(evs))
	}
}

// TestSessionErrorDispatch_AgentStatusUntouched session.error 是一次性事实事件，
// 不写 agentStatus 状态投影：派发前后快照与 run_status 发布均不变（design D2
// 「不并入 run_status」）。
func TestSessionErrorDispatch_AgentStatusUntouched(t *testing.T) {
	store := newMockStore()
	wtPath := "/data/worktrees/p1/t1"
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt, a := p18ReadyRuntime(m, "t1", "s1")
	m.applyAgentStatusAndCommit("t1", rt, p18StatusOp("s1", opencode.StatusBusy)) // idle→busy 发布 1 次
	before := a.snapshotValue()
	if before != "busy" {
		t.Fatalf("prereq: snapshot = %q, want busy", before)
	}
	runStatusBefore := len(pub.runStatus())

	if err := m.handleSSEEvent(context.Background(), "t1", wtPath, dispatchSessionErrorEvent("s1", nil)); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	if got := a.snapshotValue(); got != before {
		t.Fatalf("agentStatus snapshot changed by session.error: %q → %q", before, got)
	}
	if n := len(pub.runStatus()); n != runStatusBefore {
		t.Fatalf("session.error must not publish run_status: events %d → %d", runStatusBefore, n)
	}
	if evs := sessionErrorDispatchEvents(t, pub); len(evs) != 1 {
		t.Fatalf("session.error must still publish once, got %d", len(evs))
	}
}

// dispatchSessionErrorEventWithInfoID 构造带 info.id 但 sessionID 缺失/非法的事件
// （A3：sessionID 不得回退 info.id）。
func dispatchSessionErrorEventWithInfoID(infoID string, sessionID interface{}) opencode.Event {
	props := map[string]interface{}{
		"info":  map[string]interface{}{"id": infoID},
		"error": map[string]interface{}{"name": "APIError", "data": map[string]interface{}{"message": "boom"}},
	}
	if sessionID != nil {
		props["sessionID"] = sessionID
	}
	return opencode.Event{Type: "session.error", Properties: props}
}

// TestSessionErrorDispatch_NoFallbackAndNoOwnerQueryOnMalformed A3 task 层：
//   - sessionID 缺失/非法 + 合法 info.id（info.id 归属歧义，读它必错）→ 事件被
//     静默忽略，MUST NOT 触发所有权查询错误路径（handleSSEEvent 不返回 error，
//     不中断事件流）
//   - 合法事件（sessionID 歧义归属）→ 归属查询失败仅记日志丢弃，事件流不受影响
//   - 归属判定使用解析所得 sessionID 而非 info.id：sessionID 归属本任务 +
//     info.id 指向他人 session → 仍发布
func TestSessionErrorDispatch_NoFallbackAndNoOwnerQueryOnMalformed(t *testing.T) {
	store := newMockStore()
	wtPath := "/data/worktrees/p1/t1"
	seedActiveTask(store, "t1", "p1")
	// s-amb 同时挂在 t1/t2 下：OwnerOf 对其返回 typed ambiguity error（历史重复
	// 归属 fail-closed）；s1 仅归属 t1。
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}, {TaskID: "t1", SessionID: "s-amb"}}
	store.sessions["t2"] = []SessionRow{{TaskID: "t2", SessionID: "s-amb"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	ctx := context.Background()

	// malformed（缺 sessionID）+ info.id=s-amb：若回退 info.id 会撞歧义错误并中断
	// 事件流；正确行为是解析失败静默忽略（无 error、无发布）。
	ev := dispatchSessionErrorEventWithInfoID("s-amb", nil)
	if err := m.handleSSEEvent(ctx, "t1", wtPath, ev); err != nil {
		t.Fatalf("malformed session.error must be silently ignored, got error: %v", err)
	}
	// 非字符串 sessionID 同理。
	ev = dispatchSessionErrorEventWithInfoID("s-amb", 42)
	if err := m.handleSSEEvent(ctx, "t1", wtPath, ev); err != nil {
		t.Fatalf("non-string sessionID must be silently ignored, got error: %v", err)
	}
	if evs := sessionErrorDispatchEvents(t, pub); len(evs) != 0 {
		t.Fatalf("malformed events must not publish, got %d", len(evs))
	}

	// 合法事件但 sessionID 归属歧义：仅丢弃（记日志），不返回 error、不发布。
	valid := dispatchSessionErrorEvent("s-amb", nil)
	if err := m.handleSSEEvent(ctx, "t1", wtPath, valid); err != nil {
		t.Fatalf("ownership read failure must not break event stream: %v", err)
	}
	if evs := sessionErrorDispatchEvents(t, pub); len(evs) != 0 {
		t.Fatalf("ambiguous ownership must drop the event, got %d publishes", len(evs))
	}

	// 归属判定用解析所得 sessionID：sessionID=s1（归属 t1）+ info.id=s-orphan
	//（无归属）→ 仍发布。
	props := map[string]interface{}{
		"sessionID": "s1",
		"info":      map[string]interface{}{"id": "s-orphan"},
		"error":     map[string]interface{}{"name": "APIError", "data": map[string]interface{}{"message": "boom"}},
	}
	if err := m.handleSSEEvent(ctx, "t1", wtPath, opencode.Event{Type: "session.error", Properties: props}); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	evs := sessionErrorDispatchEvents(t, pub)
	if len(evs) != 1 {
		t.Fatalf("ownership must use parsed sessionID (not info.id), publishes = %d", len(evs))
	}
	if pl := evs[0].Payload.(ocdeckevent.ServeRuntimeSessionErrorPayload); pl.SessionID != "s1" {
		t.Fatalf("published sessionID = %q, want s1", pl.SessionID)
	}
}

// TestSessionErrorDispatch_OrphanAndInvalidIgnored 孤儿（未归属 session）与必填字段
// 非法的事件：静默忽略（无发布、不报错、不影响事件流）。
func TestSessionErrorDispatch_OrphanAndInvalidIgnored(t *testing.T) {
	store := newMockStore()
	wtPath := "/data/worktrees/p1/t1"
	seedActiveTask(store, "t1", "p1")
	store.sessions["t1"] = []SessionRow{{TaskID: "t1", SessionID: "s1"}}
	m, pub, _ := newP18Manager(t, store, newMockProc(), newMockOC(true))
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	ctx := context.Background()

	// 孤儿事件：归属反查（fail-closed）不通过，不进入派发。
	if err := m.handleSSEEvent(ctx, "t1", wtPath, dispatchSessionErrorEvent("s-orphan", nil)); err != nil {
		t.Fatalf("orphan session.error: %v", err)
	}
	// 必填字段缺失（data.message 空白）：解析失败静默忽略。
	blank := dispatchSessionErrorEvent("s1", func(d map[string]interface{}) { d["message"] = "   " })
	if err := m.handleSSEEvent(ctx, "t1", wtPath, blank); err != nil {
		t.Fatalf("blank-message session.error: %v", err)
	}
	if evs := sessionErrorDispatchEvents(t, pub); len(evs) != 0 {
		t.Fatalf("orphan/invalid events must not publish, got %d", len(evs))
	}

	// 可空字段类型非法仅降级该字段：事件仍发布（全链路语义）。
	degraded := dispatchSessionErrorEvent("s1", func(d map[string]interface{}) { d["statusCode"] = "429" })
	if err := m.handleSSEEvent(ctx, "t1", wtPath, degraded); err != nil {
		t.Fatalf("degraded session.error: %v", err)
	}
	evs := sessionErrorDispatchEvents(t, pub)
	if len(evs) != 1 {
		t.Fatalf("degraded nullable field must still publish, events = %d", len(evs))
	}
	pl := evs[0].Payload.(ocdeckevent.ServeRuntimeSessionErrorPayload)
	if pl.StatusCode != nil {
		t.Fatalf("degraded status_code must be nil in payload, got %v", pl.StatusCode)
	}
	if pl.IsRetryable == nil || !*pl.IsRetryable {
		t.Fatalf("is_retryable must survive: %+v", pl.IsRetryable)
	}
}
