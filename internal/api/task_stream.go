// task_stream.go GET /api/v1/tasks/{id}/stream SSE 端点（task-detail-stream D1/D4）。
//
// 薄绑定：组装器 assembleTaskDetail + 过滤表 eventDirtiesTaskDetail；循环纪律由
// read_model_stream.go 共享核心承载。路由始终注册；eventSubscriber 未注入时返回
// 500（使该路径 404 唯一表示任务不存在）。
package api

import (
	"context"
	"errors"
	"net/http"

	"ocdeck/internal/application"
)

// assembleTaskDetail 组装任务详情快照：Get 与 ListTaskSessions 失败均作为组装错误
// 返回（not-found 由 assembleGone 处理，其余保持 dirty 重试）。AgentStatus 读内存快照。
func (s *Server) assembleTaskDetail(ctx context.Context, taskID string) (taskRowDTO, error) {
	t, err := s.tasks.Get(ctx, taskID)
	if err != nil {
		return taskRowDTO{}, err
	}
	sessions, err := s.tasks.ListTaskSessions(ctx, taskID)
	if err != nil {
		return taskRowDTO{}, err
	}
	dto, ae := s.buildTaskDetailDTOBase(ctx, t, sessions)
	if ae != nil {
		return taskRowDTO{}, ae
	}
	dto.AgentStatus = s.tasks.AgentStatusSnapshot(taskID)
	return dto, nil
}

// handleTaskStream GET /api/v1/tasks/{id}/stream。
func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	if s.eventSubscriber == nil {
		writeError(w, CodeInternal, "event stream not configured")
		return
	}
	taskID := r.PathValue("id")
	s.runReadModelStream(w, r, readModelStreamConfig{
		assemble: func(ctx context.Context) (any, error) {
			return s.assembleTaskDetail(ctx, taskID)
		},
		eventDirty: eventDirtiesTaskDetail(taskID),
		assembleGone: func(err error) bool {
			return errors.Is(err, application.ErrTaskNotFound)
		},
		logPrefix: "task detail stream",
		errCopy:   "get task failed",
	})
}
