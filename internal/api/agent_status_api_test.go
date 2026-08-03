package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ocdeck/internal/task"
)

// agentStatusTaskBackend 返回固定 agentStatus 供 DTO 测试。
type agentStatusTaskBackend struct {
	*fakeTaskBackend
	status string
}

func (a *agentStatusTaskBackend) AgentStatus(ctx context.Context, taskID string) string {
	return a.status
}

func TestTaskDTO_AgentStatus_Populated(t *testing.T) {
	tb := &agentStatusTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		status:          "busy",
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// fakeTaskBackend.Get 返回零值 TaskRow；仅验证 agentStatus 字段存在且被填充。
	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dto taskRowDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.AgentStatus != "busy" {
		t.Errorf("agentStatus = %q, want busy", dto.AgentStatus)
	}
}

func TestTaskDTO_AgentStatus_EmptyWhenDegraded(t *testing.T) {
	tb := &agentStatusTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		status:          "", // 降级返回空串
	}
	s := newAPITestServer(t, tb)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(authedReq("GET", ts.URL+"/api/v1/tasks/t1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dto taskRowDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.AgentStatus != "" {
		t.Errorf("agentStatus = %q, want empty", dto.AgentStatus)
	}
}

// 编译期断言：task 仅用于 OpError 等类型引用（防止重构误删 import）。
var _ task.DeleteMode