// get_list.go 实现 Get/List 用例（design.md D0:140 迁移第 4 步）。
//
// Get/List 为纯读用例：无 guard、无 CAS、无状态迁移。LifecycleService 经
// TaskReadRepository 读全行快照（application.TaskSnapshot），返回给调用方
// （Manager facade 转换为 task.TaskRow，保持 api.TaskBackend 契约逐字不变）。
//
// 错误映射由 Manager facade 完成（design.md D0:148 api.TaskBackend 契约与 OpError
// 映射是冻结不变量）：Get err → codeNotFound "task not found: %w"；List err → codeInternal。
package task

import (
	"context"

	"ocdeck/internal/application"
)

// Get 返回单个任务的全行快照（design.md D0:140）。
//
// 纯读：不持锁、不 guard、不 CAS。读侧端口返回 application.TaskSnapshot。
// 未命中时返回端口 error（由 Manager facade 映射为 codeNotFound）。
func (s *LifecycleService) Get(ctx context.Context, taskID string) (application.TaskSnapshot, error) {
	return s.read.GetTaskRow(ctx, taskID)
}

// List 返回项目下全部任务的全行快照（design.md D0:140）。
//
// 纯读：不持锁、不 guard、不 CAS。读侧端口返回 []application.TaskSnapshot。
func (s *LifecycleService) List(ctx context.Context, projectID string) ([]application.TaskSnapshot, error) {
	return s.read.ListTasksByProject(ctx, projectID)
}