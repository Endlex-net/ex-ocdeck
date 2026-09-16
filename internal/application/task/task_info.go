// task_info.go 实现任务信息修改的持久化用例（task-info-editable Phase 1：契约与事件链接线）。
//
// 业务提交经 commitTaskMutation 先例：仅 Changed=true（业务列 name/branch/env_snapshot
// 真实变化）时发布一次 task.activity_changed；pending-only 操作（仅设置/仅清除 rename_pending
// 意图）不发事件、不推进 updated_at（store 契约保证，design D6 四态表）。
// rename_pending 仅由内部 store row / service 映射携带，MUST NOT 进入公共 Task DTO、
// TaskSnapshot 或 facade 转换（design D6 读映射约束）。
package task

import (
	"context"

	"ocdeck/internal/application"
)

// CommitTaskInfoUpdate 单事务提交任务信息业务列（presence 语义 nil=不修改）并清除改名
// 意图，真实业务变更（Changed=true）时经 commitTaskMutation 发布一次 task.activity_changed
// （publish-after-commit，Publish 溢出不回滚业务提交）。同值 no-op 不发布。
func (s *LifecycleService) CommitTaskInfoUpdate(ctx context.Context, taskID string, update application.TaskInfoUpdate) (application.MutationResult, error) {
	res, err := s.tasks.CommitTaskInfoUpdate(ctx, taskID, update)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, taskID, res)
	return res, nil
}

// SetTaskRenamePending 写入改名恢复意图（pending-only：不发事件；updated_at 由 store
// 契约保证不推进）。
func (s *LifecycleService) SetTaskRenamePending(ctx context.Context, taskID string, pendingJSON string) (application.MutationResult, error) {
	return s.tasks.SetTaskRenamePending(ctx, taskID, pendingJSON)
}

// ClearTaskRenamePending 清除改名恢复意图（pending-only：不发事件；updated_at 由 store
// 契约保证不推进）。
func (s *LifecycleService) ClearTaskRenamePending(ctx context.Context, taskID string) (application.MutationResult, error) {
	return s.tasks.ClearTaskRenamePending(ctx, taskID)
}

// GetTaskRenamePending 读取未收敛改名意图（nil = 无）。仅供内部收敛编排（R1）消费，
// 不进入公共 DTO。
func (s *LifecycleService) GetTaskRenamePending(ctx context.Context, taskID string) (*string, error) {
	return s.tasks.GetTaskRenamePending(ctx, taskID)
}
