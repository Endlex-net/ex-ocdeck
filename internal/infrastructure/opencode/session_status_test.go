package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// fixture：testdata/session_status_events.jsonl，每行 = SSE data payload 原文（envelope JSON）。
//
// 溯源（P1.7.1 固化）：前 6 行为真实样本，逐字摘自 P1.7.3 门禁（2026-08-26，opencode 1.18.18）
// 的原始 SSE 抓包（本机临时归档 /var/folders/yk/5bvypw1121s2h7mf08c1x0pr0000gn/T/opencode/p17/logs/sse_capture.txt，
// probe 会话 ses_fc4359334ffe59lfDo3O5vKb1d；判定 run 的 events.jsonl 是预登记协议定义的归一化
// 台账 {ts_ms,type,sessionID,status}，不含 envelope/evt id/retry extras，故逐字样本取自抓包）。
// evt id / sessionID / 时间戳均为原值。后 3 行为合成负例（evt_synthetic_*），置尾、仍为合法 JSONL。
//
// 行布局（下标从 0）：
//	0 server.connected			1 session.status busy
//	2 session.status retry		3 session.status idle
//	4 session.idle				5 session.error
//	6 未知 type					7 session.status 缺 properties.sessionID
//	8 session.status 缺 status.type
//
// 注：真实 session.error 的 statusCode/isRetryable 嵌套在 error.data 下（error.data.statusCode），
// 本测试不解析该事件，仅保留原文供后续消费方参考。
// 注：REST GET /session/status 的 idle 会话缺席（map 无显式 idle 条目）是 REST map 侧关注点，
// 与事件解析无关，此处不断言。
func loadSessionStatusFixture(t *testing.T) []Event {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(readFixture(t, "session_status_events.jsonl"))), "\n")
	events := make([]Event, 0, len(lines))
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("fixture line %d: %v", i+1, err)
		}
		events = append(events, ev)
	}
	if len(events) != 9 {
		t.Fatalf("fixture lines = %d want 9", len(events))
	}
	return events
}

func TestParseSessionStatusEvent(t *testing.T) {
	const sid = "ses_fc4359334ffe59lfDo3O5vKb1d"
	tests := []struct {
		name    string
		line    int // fixture 行号（1-based）
		want    SessionStatusEvent
		wantOK  bool
	}{
		{name: "server.connected: not a status event", line: 1, wantOK: false},
		{name: "busy", line: 2, want: SessionStatusEvent{SessionID: sid, Status: StatusBusy}, wantOK: true},
		{
			name: "retry with attempt/message/next",
			line: 3,
			want: SessionStatusEvent{
				SessionID: sid,
				Status:    StatusRetry,
				Attempt:   1,
				Message:   "p17 stub rate limit",
				Next:      1787710728671,
			},
			wantOK: true,
		},
		{name: "idle", line: 4, want: SessionStatusEvent{SessionID: sid, Status: StatusIdle}, wantOK: true},
		{name: "session.idle: not a status event", line: 5, wantOK: false},
		{name: "session.error: not a status event", line: 6, wantOK: false},
		{name: "unknown event type: ignored", line: 7, wantOK: false},
		{name: "missing properties.sessionID: ignored", line: 8, wantOK: false},
		{name: "missing status.type: ignored", line: 9, wantOK: false},
	}
	events := loadSessionStatusFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseSessionStatusEvent(events[tt.line-1])
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseSessionStatusEvent_EnumDrift 枚举外 status.type（契约漂移）→ fail-closed 忽略。
func TestParseSessionStatusEvent_EnumDrift(t *testing.T) {
	ev := Event{Type: "session.status", Properties: map[string]interface{}{
		"sessionID": "ses_x",
		"status":    map[string]interface{}{"type": "paused"},
	}}
	if got, ok := ParseSessionStatusEvent(ev); ok {
		t.Errorf("non-enum status.type: got (%+v, true), want not-ok", got)
	}
}
