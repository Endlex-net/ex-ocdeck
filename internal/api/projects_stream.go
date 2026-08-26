// projects_stream.go GET /api/v1/projects/stream SSE 端点（projects-stream
// design D3；design D5 薄绑定）。
//
// 帧与 REST /projects 响应体完全同构（buildProjectsSnapshot 共享组装，data 为
// projectDTO 裸数组）；消费过滤为 projects 场景全任务树表
// eventDirtiesProjectsTaskTree（projects_filter.go）。SSE 循环纪律（四 topic 订阅
// fan-in、合并窗口、溢出重推、心跳、断连/关停退出）全部由 read_model_stream.go
// 共享核心承载（MUST NOT 平行复制），本端点仅做场景注入。
package api

import (
	"context"
	"net/http"
)

// handleProjectsStream GET /api/v1/projects/stream（design D3；design D5 薄封装：
// 循环核心 + 场景组装器/过滤表注入）。
func (s *Server) handleProjectsStream(w http.ResponseWriter, r *http.Request) {
	s.runReadModelStream(w, r, readModelStreamConfig{
		assemble: func(ctx context.Context) (any, error) {
			return s.buildProjectsSnapshot(ctx)
		},
		eventDirty: eventDirtiesProjectsTaskTree,
		logPrefix:  "projects stream",
		errCopy:    "list projects failed",
	})
}
