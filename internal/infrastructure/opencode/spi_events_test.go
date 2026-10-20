package opencode

// spi_events_test.go 覆盖 D8 SSE → typed events 字段级映射（tasks 3.3 验证）：
// 六家族 v1/v2、session.created/updated/deleted（directory 同源防线、title、
// parentID、time 毫秒不换算）、session.status（retry 明细 + SSE 路径未知枚举
// 忽略不应用）、session.error（字段路径 + 可空降级 + 无 info.id 回退）、未知
// type 静默忽略、EventStream Close/Err 生命周期。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// sseScriptServer 起一个 /event 端点：连接后按序投递 events（每条一行 JSON
// payload），随后挂起直至客户端断开。
func sseScriptServer(t *testing.T, events []string) *spiTestServer {
	t.Helper()
	return newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		fl.Flush()
		for _, ev := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		}
		<-r.Context().Done()
	}))
}

// collectEvents 订阅并收满 n 条（或超时）。
func collectEvents(t *testing.T, ts *spiTestServer, n int) ([]domain.AgentEvent, appagent.EventStream) {
	t.Helper()
	p := NewSPIProvider()
	stream, err := p.Events().Subscribe(context.Background(), ts.testBinding())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var got []domain.AgentEvent
	timeout := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case ev := <-stream.Events():
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("收满 %d 条超时，已收 %d", n, len(got))
		}
	}
	return got, stream
}

func TestSPI_Events_D8Mapping(t *testing.T) {
	dir := "/wt/task"
	events := []string{
		// ① created：完整形状（title/parent/time 毫秒）。
		`{"type":"session.created","properties":{"info":{"id":"ses_main","title":"任务 A","directory":"` + dir + `","time":{"created":1700000000.5,"updated":1700000010.5}}}}`,
		// ② created 子会话：parentID 同命名空间。
		`{"type":"session.created","properties":{"info":{"id":"ses_sub","parentID":"ses_main","directory":"` + dir + `","time":{"created":1,"updated":2}}}}`,
		// ③ directory 不匹配 → 丢弃（同源防线）。
		`{"type":"session.created","properties":{"info":{"id":"ses_other_dir","directory":"/other","time":{"created":1,"updated":2}}}}`,
		// ④ directory 缺失 → 丢弃。
		`{"type":"session.created","properties":{"info":{"id":"ses_no_dir","time":{"created":1,"updated":2}}}}`,
		// ⑤ updated：刷新 time.updated。
		`{"type":"session.updated","properties":{"info":{"id":"ses_main","directory":"` + dir + `","time":{"created":1700000000.5,"updated":1700000099.5}}}}`,
		// ⑥ status busy → StatusChanged(idle/busy 均合法)。
		`{"type":"session.status","properties":{"sessionID":"ses_main","status":{"type":"busy"}}}`,
		// ⑦ status retry 明细（attempt/message/next 毫秒不换算）。
		`{"type":"session.status","properties":{"sessionID":"ses_main","status":{"type":"retry","attempt":2,"message":"rate limit","next":1787710728671}}}`,
		// ⑧ status 未知枚举 → SSE 路径忽略不应用（不产生事件）。
		`{"type":"session.status","properties":{"sessionID":"ses_main","status":{"type":"future-state"}}}`,
		// ⑨ session.error 完整形状（可空字段保留）。
		`{"type":"session.error","properties":{"sessionID":"ses_main","error":{"name":"APIError","data":{"message":"boom","statusCode":429,"isRetryable":true}}}}`,
		// ⑩ session.error 缺 sessionID（即使 info.id 在场）→ 丢弃（MUST NOT 回退）。
		`{"type":"session.error","properties":{"info":{"id":"ses_main"},"error":{"name":"APIError","data":{"message":"boom"}}}}`,
		// ⑪ session.error statusCode 非整数 → 仅该字段降级 nil，事件保留。
		`{"type":"session.error","properties":{"sessionID":"ses_main","error":{"name":"E","data":{"message":"m","statusCode":429.5}}}}`,
		// ⑫ permission.asked（缺 patterns 合法 → nil）。
		`{"type":"permission.asked","properties":{"id":"perm-1","sessionID":"ses_main","permission":"bash"}}`,
		// ⑬ permission.asked 缺 permission → 丢弃。
		`{"type":"permission.asked","properties":{"id":"perm-bad","sessionID":"ses_main"}}`,
		// ⑭ permission.v2.replied → AttentionResolved。
		`{"type":"permission.v2.replied","properties":{"sessionID":"ses_main","requestID":"perm-1"}}`,
		// ⑮ question.v2.asked。
		`{"type":"question.v2.asked","properties":{"id":"q-1","sessionID":"ses_main","questions":[{"header":"方案","question":"选哪个？"}]}}`,
		// ⑯ question.v2.rejected → AttentionResolved。
		`{"type":"question.v2.rejected","properties":{"sessionID":"ses_main","requestID":"q-1"}}`,
		// ⑰ 未知 type → 静默忽略。
		`{"type":"future.unknown","properties":{"sessionID":"ses_main"}}`,
		// ⑱ deleted：仅要求 id+directory（time 可缺）。
		`{"type":"session.deleted","properties":{"info":{"id":"ses_sub","directory":"` + dir + `"}}}`,
	}
	// 期望产生事件的条目：①②⑤⑥⑦⑨⑪⑫⑭⑮⑯⑱ = 12 条。
	got, stream := collectEvents(t, sseScriptServer(t, events), 12)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantKinds := []domain.EventKind{
		domain.EventSessionCreated, domain.EventSessionCreated,
		domain.EventSessionUpdated,
		domain.EventStatusChanged, domain.EventStatusChanged,
		domain.EventSessionError, domain.EventSessionError,
		domain.EventAttentionRaised, domain.EventAttentionResolved,
		domain.EventAttentionRaised, domain.EventAttentionResolved,
		domain.EventSessionDeleted,
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("event[%d].Kind = %s, want %s", i, got[i].Kind, want)
		}
		if err := got[i].Validate(); err != nil {
			t.Errorf("event[%d] 不变量: %v", i, err)
		}
	}

	// 逐项字段断言（D8 字段级）。
	main := v1Key("ses_main")
	// ① created。
	c := got[0].Session
	if c.Key != main || c.Title != "任务 A" || c.CreatedAt != 1700000000 || c.UpdatedAt != 1700000010 {
		// 注：0.5ms 截断为整毫秒（int64 截断，与现状 int64(float64) 一致）。
		t.Errorf("created 形状 = %+v", c)
	}
	if c.Parent != nil {
		t.Errorf("created parent = %+v, want nil", c.Parent)
	}
	// ② 子会话 parent 同命名空间。
	sub := got[1].Session
	if sub.Parent == nil || *sub.Parent != main {
		t.Errorf("sub parent = %+v", sub.Parent)
	}
	// ⑤ updated。
	if got[2].Session.UpdatedAt != 1700000099 {
		t.Errorf("updated.UpdatedAt = %d", got[2].Session.UpdatedAt)
	}
	// ⑥ status busy。
	if got[3].Status.State != domain.StatusBusy {
		t.Errorf("busy state = %v", got[3].Status.State)
	}
	// ⑦ retry 明细（NextAt 毫秒不换算）。
	rd := got[4].Status
	if rd.State != domain.StatusRetry || rd.Attempt != 2 || rd.Message != "rate limit" || rd.NextAt != 1787710728671 {
		t.Errorf("retry detail = %+v", rd)
	}
	// ⑨ error 完整。
	e1 := got[5].Error
	if got[5].Key != main || e1.Name != "APIError" || e1.Message != "boom" ||
		e1.StatusCode == nil || *e1.StatusCode != 429 || e1.IsRetryable == nil || !*e1.IsRetryable {
		t.Errorf("error 完整形状 = %+v", e1)
	}
	// ⑪ statusCode 非整数 → 仅降级该字段。
	e2 := got[6].Error
	if e2.StatusCode != nil {
		t.Errorf("非整数 statusCode 必须降级 nil: %+v", e2)
	}
	// ⑫ permission asked（patterns nil 合法）。
	a1 := got[7].Attention
	if a1.ID != "perm-1" || a1.Kind != domain.AttentionPermission || a1.Session != main ||
		a1.Permission == nil || a1.Permission.Permission != "bash" || a1.Permission.Patterns != nil {
		t.Errorf("permission asked = %+v", a1)
	}
	// ⑭ resolved（v2 家族）。
	if got[8].Attention.ID != "perm-1" || got[8].Kind != domain.EventAttentionResolved {
		t.Errorf("permission resolved = %+v", got[8].Attention)
	}
	// ⑮ question asked（独立字段保留）。
	q1 := got[9].Attention
	if q1.Kind != domain.AttentionQuestion || q1.Question == nil ||
		len(q1.Question.Questions) != 1 || q1.Question.Questions[0].Header != "方案" || q1.Question.Questions[0].Question != "选哪个？" {
		t.Errorf("question asked = %+v", q1)
	}
	// ⑯ question rejected → resolved。
	if got[10].Attention.ID != "q-1" || got[10].Kind != domain.EventAttentionResolved {
		t.Errorf("question rejected = %+v", got[10].Attention)
	}
	// ⑱ deleted（time 缺失合法，键身份承载语义）。
	if got[11].Session.Key != v1Key("ses_sub") || got[11].Session.CreatedAt != 0 {
		t.Errorf("deleted = %+v", got[11].Session)
	}
}

func TestSPI_Events_BindingInvalidAndClose(t *testing.T) {
	p := NewSPIProvider()
	// binding 无效 → 进入 provider 前失败（EventSource 准入）。
	if _, err := p.Events().Subscribe(context.Background(), domain.TaskBinding{}); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Fatalf("invalid binding = %v, want binding_invalid", err)
	}
	// Close 终止订阅（server 端连接随 ctx 取消结束），Err 可读。
	ts := sseScriptServer(t, []string{
		`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"idle"}}}`,
	})
	got, stream := collectEvents(t, ts, 1)
	if got[0].Kind != domain.EventStatusChanged {
		t.Fatalf("first = %+v", got[0])
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stream.Err() == nil {
		t.Log("Close 后 Err 允许为 ctx.Canceled 或 nil（终止原因非错误）")
	}
}

// TestSPI_Events_TimeFieldsOptionalPerKind C8 门禁修复（design D8 created/updated
// 字段必填规则还原——逐事件沿用 activate.go 现状语义）：
//   - created 仅要求 ID+directory：time 全缺仍产生 SessionCreated（CreatedAt/
//     UpdatedAt 零值——activate.go 现状：时间缺失仍 claim，createdAt/lastSeen=0）；
//   - updated 仅要求 time.updated：缺失整条丢弃（activate.go 现状 return）；
//   - deleted 仅要求 ID+directory（既有断言见 D8Mapping ⑱）。
//
// mutation 证据（已验证 → 红 → 还原）：恢复整体时间门禁
// （`ev.Type != "session.deleted" && (CreatedAt==0 || UpdatedAt==0) → 丢`）
// → timeMissingCreated 分支红（SessionCreated 被丢弃，收 1 条超时）。
func TestSPI_Events_TimeFieldsOptionalPerKind(t *testing.T) {
	dir := "/wt/task"
	events := []string{
		// created：time 全缺 → 仍捕获（时间字段可选携带——激活归属捕获不依赖时间）。
		`{"type":"session.created","properties":{"info":{"id":"ses_notime","directory":"` + dir + `"}}}`,
		// created：仅 time.created（无 updated）→ 仍捕获（仅缺的字段为零值）。
		`{"type":"session.created","properties":{"info":{"id":"ses_onlyc","directory":"` + dir + `","time":{"created":1700000001}}}}`,
		// updated：time.updated 缺失 → 整条丢弃。
		`{"type":"session.updated","properties":{"info":{"id":"ses_notime","directory":"` + dir + `"}}}`,
		// updated：仅 time.updated（无 created）→ 合法（updated 不查 created）。
		`{"type":"session.updated","properties":{"info":{"id":"ses_notime","directory":"` + dir + `","time":{"updated":1700000099}}}}`,
	}
	got, stream := collectEvents(t, sseScriptServer(t, events), 3)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got[0].Kind != domain.EventSessionCreated {
		t.Fatalf("event[0] = %s, want SessionCreated（time 缺失不丢弃）", got[0].Kind)
	}
	if got[0].Session.CreatedAt != 0 || got[0].Session.UpdatedAt != 0 {
		t.Errorf("event[0] times = %+v, want zero（可选字段零值）", got[0].Session)
	}
	if got[1].Session.CreatedAt != 1700000001 || got[1].Session.UpdatedAt != 0 {
		t.Errorf("event[1] times = %d/%d, want created only", got[1].Session.CreatedAt, got[1].Session.UpdatedAt)
	}
	if got[2].Session.UpdatedAt != 1700000099 {
		t.Errorf("event[2].UpdatedAt = %d, want 1700000099（updated 仅要求 time.updated）", got[2].Session.UpdatedAt)
	}
}
