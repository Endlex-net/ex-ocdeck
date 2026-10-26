// agent_runtime.go 实现操作者发起 agent runtime 控制的 HTTP 端点
// （POST /api/v1/server/agent/restart）：当前仅 v2 共享 serve 重启。错误按
// appagent.KindOf 映射——capability_unavailable / runtime_unavailable → 422
// invalid_state（用户可见能力态/运行态不可用），其余 → 500 internal。
package api

import (
	"net/http"

	appagent "ocdeck/internal/application/agentprovider"
)

// handleAgentRestart 操作者发起重启全局 v2 共享 serve。
// 认证经 /api/v1 统一 Bearer 中间件；控制器未装配 → 422 invalid_state。
// 成功 → 204 No Content（无响应体）。
func (s *Server) handleAgentRestart(w http.ResponseWriter, r *http.Request) {
	if s.agentRuntimeController == nil {
		writeError(w, CodeInvalidState, "agent runtime controller not configured")
		return
	}
	if err := s.agentRuntimeController.RestartV2Server(r.Context()); err != nil {
		switch appagent.KindOf(err) {
		case appagent.ErrKindCapabilityUnavailable, appagent.ErrKindRuntimeUnavailable:
			writeError(w, CodeInvalidState, "agent runtime restart unavailable")
		default:
			writeError(w, CodeInternal, "agent runtime restart failed")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}