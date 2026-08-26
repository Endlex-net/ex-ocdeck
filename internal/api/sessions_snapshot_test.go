package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ocdeck/internal/application"
)

// TestBuildActiveSessionsSnapshot_AssemblesOverviewAttentionAgentStatus 验证共享
// 快照 helper 组装三路数据（overview 读模型 + attention 快照 + agentStatus 内存快照）
// 到与 REST 响应同构的 DTO 裸数组（sse-active-sessions P2.2；design.md D3）。
func TestBuildActiveSessionsSnapshot_AssemblesOverviewAttentionAgentStatus(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{
		activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 300),
		activeRow("t2", "p2", "projB", "taskB", "bB", "/wtB", 200),
	}
	tb := newActiveSessionsBackend(rows...)
	tb.agentStatusSnapshot = map[string]string{"t1": "busy"} // t2 无快照 → 空串降级
	tb.attentions = map[string]application.Attention{
		"t1": {
			Permissions: []application.PendingPermission{{Since: 100}},
			Questions:   []application.PendingQuestion{},
		},
	}
	s := newAPITestServer(t, tb)

	out, err := s.buildActiveSessionsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildActiveSessionsSnapshot: %v", err)
	}
	want := []activeSessionDTO{
		{
			TaskID: "t1", ProjectID: "p1", ProjectName: "projA", Name: "taskA",
			Branch: "bA", WorktreePath: "/wtA", LastActiveAt: 300, AgentStatus: "busy",
			Attention: attentionDTO{
				Permissions: []permissionDTO{{Since: 100}},
				Questions:   []questionDTO{},
			},
		},
		{
			TaskID: "t2", ProjectID: "p2", ProjectName: "projB", Name: "taskB",
			Branch: "bB", WorktreePath: "/wtB", LastActiveAt: 200,
			// 无 attention 注入 → fake 默认空快照 → 空数组非 null。
			Attention: attentionDTO{Permissions: []permissionDTO{}, Questions: []questionDTO{}},
		},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("snapshot = %+v, want %+v", out, want)
	}
	// 组装读内存快照，MUST NOT 实时探测。
	if calls := tb.agentStatusCallCount(); calls != 0 {
		t.Errorf("AgentStatus realtime probe called %d times, want 0", calls)
	}
}

// TestBuildActiveSessionsSnapshot_EmptyNonNil 验证空结果返回非 nil 空切片
// （JSON `[]` 非 null，spec）。
func TestBuildActiveSessionsSnapshot_EmptyNonNil(t *testing.T) {
	tb := newActiveSessionsBackend()
	s := newAPITestServer(t, tb)

	out, err := s.buildActiveSessionsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildActiveSessionsSnapshot: %v", err)
	}
	if out == nil {
		t.Fatal("snapshot = nil, want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Errorf("snapshot len = %d, want 0", len(out))
	}
	b, _ := json.Marshal(out)
	if string(b) != "[]" {
		t.Errorf("empty snapshot JSON = %s, want []", b)
	}
}

// TestBuildActiveSessionsSnapshot_StoreErrorPassthrough 验证 overview 查询失败
// 原样返回 error（REST 500 / SSE 保留上次快照的决策留在调用方）。
func TestBuildActiveSessionsSnapshot_StoreErrorPassthrough(t *testing.T) {
	tb := newActiveSessionsBackend()
	tb.overviewErr = errStoreFailure
	s := newAPITestServer(t, tb)

	out, err := s.buildActiveSessionsSnapshot(context.Background())
	if err == nil {
		t.Fatalf("want error passthrough, got snapshot %+v", out)
	}
	if out != nil {
		t.Errorf("snapshot = %+v, want nil on error", out)
	}
}

// TestBuildActiveSessionsSnapshot_HandlerShapeUnchanged 验证 REST handler 经共享
// helper 后响应字段与降级语义不变：agentStatus omitempty、attention 空数组非 null、
// Content-Type application/json。
func TestBuildActiveSessionsSnapshot_HandlerShapeUnchanged(t *testing.T) {
	rows := []application.ActiveTaskOverviewRow{activeRow("t1", "p1", "projA", "taskA", "bA", "/wtA", 100)}
	tb := newActiveSessionsBackend(rows...)
	// 无 agentStatus 快照（降级省略）。
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/sessions/active", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	raw, got := readAndDecode(t, resp.Body)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].AgentStatus != "" {
		t.Errorf("agentStatus = %q, want empty (omitted)", got[0].AgentStatus)
	}
	if strings.Contains(raw, "agentStatus") {
		t.Errorf("degraded row must omit agentStatus field: %s", raw)
	}
	if got[0].Attention.Permissions == nil || got[0].Attention.Questions == nil {
		t.Error("attention arrays should be [] not null")
	}
}
