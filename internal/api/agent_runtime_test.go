// agent_runtime_test.go 落实 POST /api/v1/server/agent/restart 端点验证：
// 控制器未装配 → 422 invalid_state；stub 成功 → 204；capability_unavailable → 422。
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

// stubAgentRuntimeController 注入 RestartV2Server 返回值供端点验证。
type stubAgentRuntimeController struct {
	err error
}

func (s *stubAgentRuntimeController) RestartV2Server(ctx context.Context) error {
	return s.err
}

// newAgentRestartServer 构造带控制器注入的 Server（不经 TaskBackend，聚焦端点）。
func newAgentRestartServer(t *testing.T, ctrl agentRuntimeController) *Server {
	t.Helper()
	cfg := testConfig()
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		auth:      NewTokenAuthenticator(cfg.Token),
		wsClients: newWSClientRegistry(),
	}
	s.agentRuntimeController = ctrl
	s.registerRoutes()
	return s
}

func TestAPI_AgentRestart_NoController_422(t *testing.T) {
	s := newAgentRestartServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/server/agent/restart", nil)
	s.handleAgentRestart(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestAPI_AgentRestart_StubSuccess_204(t *testing.T) {
	s := newAgentRestartServer(t, &stubAgentRuntimeController{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/server/agent/restart", nil)
	s.handleAgentRestart(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 MUST have empty body, got %q", rec.Body.String())
	}
}

func TestAPI_AgentRestart_CapabilityUnavailable_422(t *testing.T) {
	s := newAgentRestartServer(t, &stubAgentRuntimeController{
		err: appagent.NewError("task.restart_v2_server", appagent.ErrKindCapabilityUnavailable,
			errors.New("v2 unregistered")),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/server/agent/restart", nil)
	s.handleAgentRestart(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestAPI_AgentRestart_RuntimeUnavailable_422(t *testing.T) {
	s := newAgentRestartServer(t, &stubAgentRuntimeController{
		err: appagent.NewError("supervisor.restart_server", appagent.ErrKindRuntimeUnavailable,
			errors.New("shutting down")),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/server/agent/restart", nil)
	s.handleAgentRestart(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422 (runtime_unavailable → invalid_state)", rec.Code)
	}
}

func TestAPI_AgentRestart_InternalError_500(t *testing.T) {
	s := newAgentRestartServer(t, &stubAgentRuntimeController{
		err: errors.New("unexpected boom"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/server/agent/restart", nil)
	s.handleAgentRestart(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}