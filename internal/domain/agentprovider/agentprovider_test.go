package agentprovider

// agentprovider_test.go 覆盖 D7 不变量（tasks 3.1 验证：AgentEvent 每 Kind 恰好
// 一个非 nil 负载）与 TaskBinding envelope 版本化编解码（未知版本 fail-closed）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func testKey() SessionKey {
	return SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "ses_1"}
}

func TestAgentEvent_InvariantPerKind(t *testing.T) {
	key := testKey()
	// mustEvent 接单参多值调用（构造错误直接 Fatal）。
	mustEvent := func(ev AgentEvent, err error) AgentEvent {
		t.Helper()
		if err != nil {
			t.Fatalf("construct event: %v", err)
		}
		return ev
	}
	okCases := map[string]AgentEvent{
		"created":  mustEvent(NewSessionEvent(EventSessionCreated, AgentSession{Key: key, Title: "t"})),
		"updated":  mustEvent(NewSessionEvent(EventSessionUpdated, AgentSession{Key: key})),
		"deleted":  mustEvent(NewSessionEvent(EventSessionDeleted, AgentSession{Key: key})),
		"status":   mustEvent(NewStatusEvent(key, StatusDetail{State: StatusBusy})),
		"raised":   mustEvent(NewAttentionEvent(EventAttentionRaised, Attention{ID: "r1", Kind: AttentionPermission, Session: key, Permission: &PermissionPayload{ID: "r1"}})),
		"resolved": mustEvent(NewAttentionEvent(EventAttentionResolved, Attention{ID: "q1", Kind: AttentionQuestion, Session: key, Question: &QuestionPayload{ID: "q1"}})),
		"error":    mustEvent(NewSessionErrorEvent(key, SessionErrorDetail{Name: "APIError", Message: "boom"})),
	}
	for name, ev := range okCases {
		if err := ev.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if ev.Key != key {
			t.Errorf("%s: Key 漂移", name)
		}
	}

	// 违例：双负载 / 缺负载 / 零键 / Kind 与负载错配——Validate 与工厂都必须拒绝。
	ev := mustEvent(NewStatusEvent(key, StatusDetail{State: StatusIdle}))
	ev.Session = &AgentSession{Key: key}
	if err := ev.Validate(); err == nil || !strings.Contains(err.Error(), "必须且仅携带") {
		t.Errorf("双负载未拒绝: %v", err)
	}
	ev2 := mustEvent(NewStatusEvent(key, StatusDetail{State: StatusIdle}))
	ev2.Status = nil
	if err := ev2.Validate(); err == nil {
		t.Error("缺负载未拒绝")
	}
	if _, err := NewStatusEvent(SessionKey{}, StatusDetail{}); err == nil {
		t.Error("零键未拒绝")
	}
	// Kind 与负载错配：permission Kind 携带 Question 负载。
	if _, err := NewAttentionEvent(EventAttentionRaised, Attention{ID: "x", Kind: AttentionPermission, Session: key, Question: &QuestionPayload{ID: "x"}}); err == nil {
		t.Error("Kind/负载错配未拒绝")
	}
	// 必填负载缺失：错误事件缺 message。
	if _, err := NewSessionErrorEvent(key, SessionErrorDetail{Name: "n"}); err == nil {
		t.Error("错误事件缺 message 未拒绝")
	}
	// 工厂 Kind 误用：非会话生命周期 Kind。
	if _, err := NewSessionEvent(EventStatusChanged, AgentSession{Key: key}); err == nil {
		t.Error("生命周期工厂误用 Kind 未拒绝")
	}
}

func TestTaskBinding_EnvelopeRoundtrip(t *testing.T) {
	data := json.RawMessage(`{"port":50001,"directory":"/wt"}`)
	b := TaskBinding{
		Version: TaskBindingVersion, ProviderID: "opencode-v1", ProviderInstanceID: "default",
		UpstreamSessionID: "ses_a", TerminalRef: "ocdeck-t1-runtime", Data: data,
	}
	raw, err := b.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeTaskBinding(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ProviderID != b.ProviderID || got.ProviderInstanceID != b.ProviderInstanceID ||
		got.UpstreamSessionID != b.UpstreamSessionID || got.TerminalRef != b.TerminalRef || got.Version != b.Version {
		t.Errorf("roundtrip 漂移: %+v", got)
	}
	if string(got.Data) != string(data) {
		t.Errorf("Data 漂移: %s", got.Data)
	}
	// envelope 字段名稳定（持久化列格式）。
	for _, field := range []string{`"version":1`, `"provider_id":"opencode-v1"`, `"provider_instance_id":"default"`, `"upstream_session_id":"ses_a"`, `"terminal_ref":"ocdeck-t1-runtime"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("envelope 缺少稳定字段 %s: %s", field, raw)
		}
	}
}

func TestTaskBinding_FailClosed(t *testing.T) {
	// 未知版本。
	if _, err := DecodeTaskBinding(`{"version":2,"provider_id":"p","provider_instance_id":"d","upstream_session_id":"s","terminal_ref":"t"}`); err == nil || !strings.Contains(err.Error(), "未知") {
		t.Errorf("未知版本未 fail-closed: %v", err)
	}
	// 畸形 JSON。
	if _, err := DecodeTaskBinding(`{not-json`); err == nil {
		t.Error("畸形 JSON 未 fail-closed")
	}
	// 必填字段缺失（terminal_ref）。
	if _, err := DecodeTaskBinding(`{"version":1,"provider_id":"p","provider_instance_id":"d","upstream_session_id":"s"}`); err == nil {
		t.Error("字段缺失未 fail-closed")
	}
	// Encode 侧对偶：不完整绑定不得落库。
	if _, err := (TaskBinding{Version: 1, ProviderID: "p", ProviderInstanceID: "d", UpstreamSessionID: "s"}).Encode(); err == nil {
		t.Error("不完整绑定 Encode 未拒绝")
	}
}

func TestSessionKey_Zero(t *testing.T) {
	if !(SessionKey{}).IsZero() {
		t.Error("零值键必须 IsZero")
	}
	full := testKey()
	for _, mutate := range []func(SessionKey) SessionKey{
		func(k SessionKey) SessionKey { k.ProviderID = ""; return k },
		func(k SessionKey) SessionKey { k.ProviderInstanceID = ""; return k },
		func(k SessionKey) SessionKey { k.UpstreamID = ""; return k },
	} {
		if !mutate(full).IsZero() {
			t.Error("任一字段为空必须视为不完整键")
		}
	}
}
