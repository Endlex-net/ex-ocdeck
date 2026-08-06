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
	status    string
	getTaskID string
}

func (a *agentStatusTaskBackend) AgentStatus(ctx context.Context, taskID string) string {
	return a.status
}

// Get 返回带 ProjectID 的零值任务行，供 handleGetTask 在 fail-closed kind 校验下通过。
func (a *agentStatusTaskBackend) Get(ctx context.Context, taskID string) (task.TaskRow, error) {
	return task.TaskRow{ID: taskID, ProjectID: a.getTaskID}, nil
}

// newAgentStatusServer 构造带 ProjectStore + TaskBackend 的 Server（agentStatus 测试用）。
func newAgentStatusServer(t *testing.T, tb TaskBackend, projectID, kind string) *Server {
	t.Helper()
	cfg := testConfig()
	projs := newFakeProjectStore()
	projs.projects[projectID] = storeProjectRow{ID: projectID, Name: "p", Path: "/x", DefaultBranch: "main", Kind: kind}
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
		projs:     projs,
		tasks:     tb,
	}
	s.registerRoutes()
	return s
}

func TestTaskDTO_AgentStatus_Populated(t *testing.T) {
	tb := &agentStatusTaskBackend{
		fakeTaskBackend: &fakeTaskBackend{},
		status:           "busy",
		getTaskID:        "p1",
	}
	s := newAgentStatusServer(t, tb, "p1", "repo")
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
		getTaskID:       "p1",
	}
	s := newAgentStatusServer(t, tb, "p1", "repo")
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