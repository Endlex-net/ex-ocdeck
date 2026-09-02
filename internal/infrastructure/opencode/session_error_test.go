package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// errorProps 构造 session.error 的 properties（真实契约形状：sessionID +
// error.{name, data}，fixture 第 6 行同形）。sid/name/data 取 interface{} 以便
// 构造类型非法负例。
func errorProps(sid interface{}, name interface{}, data interface{}) map[string]interface{} {
	return map[string]interface{}{
		"sessionID": sid,
		"error": map[string]interface{}{
			"name": name,
			"data": data,
		},
	}
}

func errorData(mutate func(map[string]interface{})) map[string]interface{} {
	d := map[string]interface{}{"message": "boom"}
	if mutate != nil {
		mutate(d)
	}
	return d
}

// TestParseSessionErrorEvent_FixtureRealSample 真实样本（testdata/
// session_status_events.jsonl 第 6 行，P1.7.3 门禁抓包固化）：完整字段解析，
// statusCode/isRetryable 提取自 error.data。
func TestParseSessionErrorEvent_FixtureRealSample(t *testing.T) {
	events := loadSessionStatusFixture(t)
	got, ok := ParseSessionErrorEvent(events[5])
	if !ok {
		t.Fatal("real session.error sample must parse")
	}
	want := SessionErrorEvent{
		SessionID:   "ses_fc4359334ffe59lfDo3O5vKb1d",
		Name:        "APIError",
		Message:     "p17 stub rate limit",
		StatusCode:  intPtr(429),
		IsRetryable: boolPtr(true),
	}
	if got.SessionID != want.SessionID || got.Name != want.Name || got.Message != want.Message {
		t.Fatalf("required fields: got %+v want %+v", got, want)
	}
	if got.StatusCode == nil || *got.StatusCode != 429 {
		t.Fatalf("statusCode = %v, want *429", got.StatusCode)
	}
	if got.IsRetryable == nil || *got.IsRetryable != true {
		t.Fatalf("isRetryable = %v, want *true", got.IsRetryable)
	}
}

// TestParseSessionErrorEvent_FixtureNegatives fixture 非该 type 的事件一律不解析。
func TestParseSessionErrorEvent_FixtureNegatives(t *testing.T) {
	events := loadSessionStatusFixture(t)
	for _, line := range []int{1, 2, 7} { // server.connected / session.status / 未知 type
		if got, ok := ParseSessionErrorEvent(events[line-1]); ok {
			t.Fatalf("fixture line %d must not parse as session.error, got %+v", line, got)
		}
	}
}

// TestParseSessionErrorEvent_RequiredFields 必填字段规则（spec「通知触发——错误未恢复」）：
// sessionID/error.name/error.data.message 缺失、TrimSpace 后空白或类型非法 →
// (zero, false) 忽略整个事件。
func TestParseSessionErrorEvent_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"missing sessionID", Event{Type: "session.error", Properties: errorProps("", "APIError", errorData(nil))}},
		{"blank sessionID", Event{Type: "session.error", Properties: errorProps("   ", "APIError", errorData(nil))}},
		{"non-string sessionID", Event{Type: "session.error", Properties: errorProps(42, "APIError", errorData(nil))}},
		{"missing error object", Event{Type: "session.error", Properties: map[string]interface{}{"sessionID": "s1"}}},
		{"error non-object", Event{Type: "session.error", Properties: map[string]interface{}{"sessionID": "s1", "error": "nope"}}},
		{"missing error.name", Event{Type: "session.error", Properties: errorProps("s1", nil, errorData(nil))}},
		{"blank error.name", Event{Type: "session.error", Properties: errorProps("s1", "  ", errorData(nil))}},
		{"non-string error.name", Event{Type: "session.error", Properties: errorProps("s1", 123, errorData(nil))}},
		{"missing error.data", Event{Type: "session.error", Properties: map[string]interface{}{
			"sessionID": "s1",
			"error":     map[string]interface{}{"name": "APIError"},
		}}},
		{"error.data non-object", Event{Type: "session.error", Properties: errorProps("s1", "APIError", "str")}},
		{"missing data.message", Event{Type: "session.error", Properties: errorProps("s1", "APIError", map[string]interface{}{})}},
		{"blank data.message", Event{Type: "session.error", Properties: errorProps("s1", "APIError", errorData(func(d map[string]interface{}) { d["message"] = "  \t " }))}},
		{"non-string data.message", Event{Type: "session.error", Properties: errorProps("s1", "APIError", errorData(func(d map[string]interface{}) { d["message"] = 7 }))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSessionErrorEvent(tc.ev)
			if ok {
				t.Fatalf("invalid required field must reject whole event, got %+v", got)
			}
			if got != (SessionErrorEvent{}) {
				t.Fatalf("rejected event must be zero value, got %+v", got)
			}
		})
	}
}

// TestParseSessionErrorEvent_MinimalValid 最小合法事件：可空字段缺席 → nil 指针；
// message 周边空白保留原文（TrimSpace 仅用于有效性判定）。
func TestParseSessionErrorEvent_MinimalValid(t *testing.T) {
	ev := Event{Type: "session.error", Properties: errorProps("s1", "APIError", map[string]interface{}{
		"message": "  wrapped boom  ",
	})}
	got, ok := ParseSessionErrorEvent(ev)
	if !ok {
		t.Fatal("minimal valid event must parse")
	}
	if got.SessionID != "s1" || got.Name != "APIError" || got.Message != "  wrapped boom  " {
		t.Fatalf("minimal event fields: %+v", got)
	}
	if got.StatusCode != nil || got.IsRetryable != nil {
		t.Fatalf("absent nullable fields must stay nil: %+v", got)
	}
}

// TestParseSessionErrorEvent_StatusCode 可空 statusCode 规则（经原始 JSON 文本
// 构造，镜像生产 UseNumber 解码）：仅可无损表示为 Go int 的 JSON 整数接受；
// 小数/指数/非数字/超范围/缺失/null 一律降级为缺失（不拒绝事件）。
func TestParseSessionErrorEvent_StatusCode(t *testing.T) {
	cases := []struct {
		name     string
		rawValue string
		want     *int
	}{
		{"integer", "429", intPtr(429)},
		{"zero", "0", intPtr(0)},
		{"negative", "-1", intPtr(-1)},
		{"decimal", "429.5", nil},
		{"string", `"429"`, nil},
		{"bool", "true", nil},
		{"null", "null", nil},
		{"missing", "##absent##", nil},
		{"overflow 2^63", "9223372036854775808", nil},
		{"huge 1e300", "1e300", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := eventFromJSONText(t, statusCodeRawEvent(tc.rawValue))
			got, ok := ParseSessionErrorEvent(ev)
			if !ok {
				t.Fatalf("nullable-field cases must not reject the event: %+v", got)
			}
			if (got.StatusCode == nil) != (tc.want == nil) {
				t.Fatalf("statusCode presence = %v, want %v (got %+v)", got.StatusCode, tc.want, got)
			}
			if tc.want != nil && *got.StatusCode != *tc.want {
				t.Fatalf("statusCode = %v, want %v", *got.StatusCode, *tc.want)
			}
		})
	}
}

// TestParseSessionErrorEvent_IsRetryable 可空 isRetryable 规则：仅 JSON 布尔接受；
// 其他类型/null/缺失降级为缺失。
func TestParseSessionErrorEvent_IsRetryable(t *testing.T) {
	cases := []struct {
		name string
		raw  interface{}
		want *bool
	}{
		{"true", true, boolPtr(true)},
		{"false", false, boolPtr(false)},
		{"string", "yes", nil},
		{"number", 1.0, nil},
		{"null", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := errorData(func(d map[string]interface{}) { d["isRetryable"] = tc.raw })
			got, ok := ParseSessionErrorEvent(Event{Type: "session.error", Properties: errorProps("s1", "APIError", data)})
			if !ok {
				t.Fatalf("nullable-field cases must not reject the event: %+v", got)
			}
			if (got.IsRetryable == nil) != (tc.want == nil) {
				t.Fatalf("isRetryable presence = %v, want %v (got %+v)", got.IsRetryable, tc.want, got)
			}
			if tc.want != nil && *got.IsRetryable != *tc.want {
				t.Fatalf("isRetryable = %v, want %v", *got.IsRetryable, *tc.want)
			}
		})
	}
}

// TestParseSessionErrorEvent_BothNullableDegraded 两个可空字段同时类型非法：
// 仅各自降级，事件仍被接受（spec：不拒绝整个事件）。
func TestParseSessionErrorEvent_BothNullableDegraded(t *testing.T) {
	data := errorData(func(d map[string]interface{}) {
		d["statusCode"] = "abc"
		d["isRetryable"] = 1.0
	})
	got, ok := ParseSessionErrorEvent(Event{Type: "session.error", Properties: errorProps("s1", "APIError", data)})
	if !ok {
		t.Fatal("degraded nullable fields must not reject the event")
	}
	if got.StatusCode != nil || got.IsRetryable != nil {
		t.Fatalf("both nullable fields must degrade to nil: %+v", got)
	}
	if got.Name != "APIError" || got.Message != "boom" {
		t.Fatalf("required fields must survive: %+v", got)
	}
}

// TestParseSessionErrorEvent_NoSessionIDFallbackToInfoID session.error 的 sessionID
// 必填键是 properties.sessionID 本身，MUST NOT 回退 properties.info.id（与
// session.status 的 SessionIDProp 语义不同）：缺/错 sessionID + 合法 info.id →
// 忽略整个事件（task-notifications Phase 1 A3）。
func TestParseSessionErrorEvent_NoSessionIDFallbackToInfoID(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{
			name: "missing sessionID with valid info.id",
			ev: Event{Type: "session.error", Properties: map[string]interface{}{
				"info":  map[string]interface{}{"id": "ses_fallback"},
				"error": map[string]interface{}{"name": "APIError", "data": map[string]interface{}{"message": "boom"}},
			}},
		},
		{
			name: "non-string sessionID with valid info.id",
			ev: Event{Type: "session.error", Properties: map[string]interface{}{
				"sessionID": 42,
				"info":      map[string]interface{}{"id": "ses_fallback"},
				"error":     map[string]interface{}{"name": "APIError", "data": map[string]interface{}{"message": "boom"}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSessionErrorEvent(tc.ev)
			if ok {
				t.Fatalf("session.error must not fall back to info.id, got %+v", got)
			}
			if got != (SessionErrorEvent{}) {
				t.Fatalf("rejected event must be zero value, got %+v", got)
			}
		})
	}
}

// eventFromJSONText 以词法保真（UseNumber）方式从原始 JSON 文本构造 Event，
// 镜像生产 parseEvent 解码路径（不经 float64）。A4：statusCode 的无损 int 判定
// 必须基于 JSON 词法值而非浮点往返。
func eventFromJSONText(t *testing.T, raw string) Event {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var ev Event
	if err := dec.Decode(&ev); err != nil {
		t.Fatalf("decode raw event %s: %v", raw, err)
	}
	return ev
}

// statusCodeRawEvent 构造携带指定 statusCode JSON 词法的 session.error 事件原文；
// "##absent##" 哨兵表示省略该键。
func statusCodeRawEvent(rawValue string) string {
	statusCodeField := `"statusCode":` + rawValue + `,`
	if rawValue == "##absent##" {
		statusCodeField = ""
	}
	return `{"type":"session.error","properties":{"sessionID":"s1","error":{"name":"APIError","data":{` + statusCodeField + `"message":"boom"}}}}`
}

// TestParseSessionErrorEvent_StatusCodeLexical 词法保真判定（A4）：整数按
// strconv.ParseInt 精确接受（含 2^53+1 奇数——float64 路径会舍入破坏）；小数、
// 指数记法、越界一律按缺失处理。
func TestParseSessionErrorEvent_StatusCodeLexical(t *testing.T) {
	cases := []struct {
		name      string
		rawNumber string
		want      *int
	}{
		{"plain integer", "429", intPtr(429)},
		{"zero", "0", intPtr(0)},
		{"negative", "-1", intPtr(-1)},
		{"decimal 429.0 rejected", "429.0", nil},
		{"decimal 429.5 rejected", "429.5", nil},
		{"exponent 1e3 rejected", "1e3", nil},
		{"exponent decimal 1.5e2 rejected", "1.5e2", nil},
		{"2^53 accepted exact", "9007199254740992", intPtr(9007199254740992)},
		{"2^53+1 accepted exact (odd, float64 会舍入)", "9007199254740993", intPtr(9007199254740993)},
		{"int64 max accepted", "9223372036854775807", intPtr(9223372036854775807)},
		{"int64 min accepted", "-9223372036854775808", intPtr(-9223372036854775808)},
		{"2^63 overflow rejected", "9223372036854775808", nil},
		{"huge rejected", "99999999999999999999999", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := eventFromJSONText(t, statusCodeRawEvent(tc.rawNumber))
			got, ok := ParseSessionErrorEvent(ev)
			if !ok {
				t.Fatalf("statusCode lexical cases must not reject the event: %+v", got)
			}
			if (got.StatusCode == nil) != (tc.want == nil) {
				t.Fatalf("statusCode = %v, want %v", got.StatusCode, tc.want)
			}
			if tc.want != nil && *got.StatusCode != *tc.want {
				t.Fatalf("statusCode = %d, want exact %d", *got.StatusCode, *tc.want)
			}
		})
	}
}

// TestParseEvent_PreservesJSONNumbers 生产 SSE 解码路径（parseEvent）以 UseNumber
// 保留词法数值：session.error 的 2^53+1 精确经全链路接受；session.status 的
// attempt/next 数值语义不变（floatNumber 已适配 json.Number）。
func TestParseEvent_PreservesJSONNumbers(t *testing.T) {
	rawErr := `{"type":"session.error","properties":{"sessionID":"s1","error":{"name":"APIError","data":{"message":"boom","statusCode":9007199254740993}}}}`
	ev, err := parseEvent(sseRawEvent{Type: "session.error", data: []byte(rawErr)})
	if err != nil {
		t.Fatalf("parseEvent: %v", err)
	}
	got, ok := ParseSessionErrorEvent(ev)
	if !ok || got.StatusCode == nil || *got.StatusCode != 9007199254740993 {
		t.Fatalf("production decode path must preserve lexical integers: ok=%v code=%v", ok, got.StatusCode)
	}

	rawStatus := `{"type":"session.status","properties":{"sessionID":"s1","status":{"type":"retry","attempt":2,"message":"m","next":1787710728671}}}`
	sev, err := parseEvent(sseRawEvent{Type: "session.status", data: []byte(rawStatus)})
	if err != nil {
		t.Fatalf("parseEvent status: %v", err)
	}
	st, ok := ParseSessionStatusEvent(sev)
	if !ok || st.Attempt != 2 || st.Next != 1787710728671 {
		t.Fatalf("session.status numeric semantics must be unchanged: ok=%v got %+v", ok, st)
	}
}
