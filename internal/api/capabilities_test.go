package api

// capabilities_test.go 覆盖 opencode-v1-spi-provider tasks 3.8 的状态端点透出：
//   - capabilities 为唯一增量：未装配 provider 时响应不含该字段（既有形状不变）；
//   - 装配后 JSON 精确形状（tasks 3.7 T9 决议，逐字段断言）：primitive/scope/
//     epoch/state/reason/taskId 顶层小驼峰 + session 可空对象（null |
//     {providerId, providerInstanceId, upstreamId}）；
//   - 既有七字段不受装配影响。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

func getStatusJSON(t *testing.T, srv *Server) map[string]json.RawMessage {
	t.Helper()
	ts := httptest.NewServer(srv.mux)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/server/status", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestServerStatus_CapabilitiesAbsentWithoutProvider(t *testing.T) {
	// 行为保持：未装配时响应不含 capabilities 字段，既有七字段齐全。
	srv := New(testConfig(), nil)
	body := getStatusJSON(t, srv)
	if _, ok := body["capabilities"]; ok {
		t.Error("未装配 provider 时不得出现 capabilities 字段")
	}
	for _, field := range []string{"opencodeVersion", "tmuxVersion", "shutdownPolicy", "watchdogState", "contractBaseline", "contractMinVersion", "versionVerified"} {
		if _, ok := body[field]; !ok {
			t.Errorf("既有字段 %s 缺失", field)
		}
	}
}

func TestServerStatus_CapabilitiesExactShape(t *testing.T) {
	srv := New(testConfig(), nil)
	key := domain.SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "ses_1"}
	srv.SetCapabilitiesProvider(func() []appagent.CapabilityHealth {
		return []appagent.CapabilityHealth{
			// 顶层字段 + session 对象形态。
			{ProviderID: "opencode-v1", Primitive: appagent.PrimitiveAsyncPrompt, Scope: appagent.ScopeSession,
				TaskID: "t1", Session: &key, Epoch: 2, State: appagent.CapDegraded, Reason: "contract violation"},
			// session null 形态。
			{ProviderID: "opencode-v1", Primitive: appagent.PrimitiveTitleUpdater, Scope: appagent.ScopeRuntime,
				TaskID: "t9", Epoch: 1, State: appagent.CapUnsupported},
		}
	})
	body := getStatusJSON(t, srv)

	raw, ok := body["capabilities"]
	if !ok {
		t.Fatal("装配后必须出现 capabilities 字段")
	}
	var caps []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("capabilities 非数组: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("len = %d, want 2", len(caps))
	}

	strField := func(m map[string]json.RawMessage, k string) string {
		t.Helper()
		var v string
		if err := json.Unmarshal(m[k], &v); err != nil {
			t.Fatalf("字段 %s 非字符串: %v", k, err)
		}
		return v
	}
	// ① session 对象形态：顶层字段名/类型逐字断言。
	c0 := caps[0]
	if got := strField(c0, "primitive"); got != "async_prompt" {
		t.Errorf("primitive = %q", got)
	}
	if got := strField(c0, "scope"); got != "session" {
		t.Errorf("scope = %q", got)
	}
	var epoch int64
	if err := json.Unmarshal(c0["epoch"], &epoch); err != nil || epoch != 2 {
		t.Errorf("epoch = (%v, %v), want number 2", epoch, err)
	}
	if got := strField(c0, "state"); got != "degraded" {
		t.Errorf("state = %q", got)
	}
	if got := strField(c0, "reason"); got != "contract violation" {
		t.Errorf("reason = %q", got)
	}
	if got := strField(c0, "taskId"); got != "t1" {
		t.Errorf("taskId = %q", got)
	}
	var sess map[string]json.RawMessage
	if err := json.Unmarshal(c0["session"], &sess); err != nil {
		t.Fatalf("session 非对象: %v", err)
	}
	if strField(sess, "providerId") != "opencode-v1" || strField(sess, "providerInstanceId") != "default" || strField(sess, "upstreamId") != "ses_1" {
		t.Errorf("session 对象字段 = %v", sess)
	}
	// session 对象仅三字段（无多余键）。
	if len(sess) != 3 {
		t.Errorf("session 对象键数 = %d, want 3", len(sess))
	}
	// 顶层仅七字段（T9 形状；无 providerId 顶层键）。
	if len(c0) != 7 {
		t.Errorf("顶层键数 = %d, want 7（%v）", len(c0), c0)
	}
	if _, has := c0["providerId"]; has {
		t.Error("顶层不得有 providerId（T9 形状无此顶层字段）")
	}

	// ② session null 形态。
	c1 := caps[1]
	var isNull *json.RawMessage
	if err := json.Unmarshal(c1["session"], &isNull); err != nil {
		t.Fatalf("session 反序列化失败: %v", err)
	}
	var nullCheck any
	if err := json.Unmarshal(c1["session"], &nullCheck); err != nil || nullCheck != nil {
		t.Errorf("session = %v, want null", string(c1["session"]))
	}

	// 既有字段不受装配影响（七字段仍齐全）。
	for _, field := range []string{"opencodeVersion", "tmuxVersion", "shutdownPolicy", "watchdogState", "contractBaseline", "contractMinVersion", "versionVerified"} {
		if _, ok := body[field]; !ok {
			t.Errorf("既有字段 %s 缺失", field)
		}
	}
}

func TestServerStatus_CapabilitiesPrefilledUnknown(t *testing.T) {
	// 生产装配形态：NewHealthTracker 预填 7 原语 unknown epoch 0（重启归零
	// unknown 的端点透出验证——经 tracker.Snapshot 直连）。
	tr := appagent.NewHealthTracker()
	srv := New(testConfig(), nil)
	srv.SetCapabilitiesProvider(tr.Snapshot)
	body := getStatusJSON(t, srv)
	var caps []map[string]any
	if err := json.Unmarshal(body["capabilities"], &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(caps) != 7 {
		t.Fatalf("len = %d, want 7", len(caps))
	}
	for _, c := range caps {
		if c["state"] != "unknown" || c["scope"] != "provider" || c["epoch"] != float64(0) {
			t.Errorf("预填记录 = %v", c)
		}
	}
}
