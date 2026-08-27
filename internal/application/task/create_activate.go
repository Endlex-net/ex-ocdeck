// create_activate.go 实现 Create/Retry/Activate 用例的 DB 事实写入薄封装
// （design.md D0:142 迁移第 6 步，P1.4.6）。
//
// 统一形态：persist + commit helper。Manager facade 保持流程管理器角色（worktree/tmux/
// opencode/锁与调度不动），仅任务行写入经本层路由：
//   - 迁移类写入（UpdateStatus/Conditional/CommitCreated）：StatusChanged=true 走
//     commitTransition（发布 task.status_changed）；否则走 commitTaskMutation
//     （activity_changed 仅 Changed=true）。
//   - 变更类写入（SetDeleteMode/UpdateEnvSnapshot/UpdateLastPort）：commitTaskMutation。
//   - CreateTask：成功后 commitTaskCreated（发布 task.created）。
//
// 本层不持 guard 决策（调用方已按现状完成前置检查与门禁），仅透传结构化结果。
// NoopPublisher 阶段调用位就绪无实际发布，真实事件生产挂接在 Phase C/P1.6。
package task

import (
	"context"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
)

// CreateTask 插入任务行并在成功后提交（publish task.created）。
func (s *LifecycleService) CreateTask(ctx context.Context, row application.TaskSnapshot) error {
	if err := s.tasks.CreateTask(ctx, row); err != nil {
		return err
	}
	s.commitTaskCreated(row.ID)
	return nil
}

// UpdateStatus 无条件状态写入（含 last_error），按 StatusChanged 分流提交。
func (s *LifecycleService) UpdateStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	res, err := s.tasks.UpdateTaskStatus(ctx, id, status, lastError)
	if err != nil {
		return application.TransitionResult{}, err
	}
	s.commitTransitionResult(ctx, id, res)
	return res, nil
}

// UpdateStatusConditional CAS 状态写入，按 StatusChanged 分流提交。
func (s *LifecycleService) UpdateStatusConditional(ctx context.Context, id string, from, to ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	res, err := s.tasks.UpdateTaskStatusConditional(ctx, id, from, to, lastError)
	if err != nil {
		return application.TransitionResult{}, err
	}
	s.commitTransitionResult(ctx, id, res)
	return res, nil
}

// CommitCreated 创建提交点（expectedStatus CAS → suspended + init_status）。
func (s *LifecycleService) CommitCreated(ctx context.Context, id string, expected ocdecktask.Status, init ocdecktask.InitStatus) (application.TransitionResult, error) {
	res, err := s.tasks.CommitCreated(ctx, id, expected, init)
	if err != nil {
		return application.TransitionResult{}, err
	}
	s.commitTransitionResult(ctx, id, res)
	return res, nil
}

// SetDeleteMode 写入 delete_mode（非迁移列写入）。
func (s *LifecycleService) SetDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (application.MutationResult, error) {
	res, err := s.tasks.SetTaskDeleteMode(ctx, id, mode)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// UpdateEnvSnapshot 写入 env_snapshot（非迁移列写入）。
func (s *LifecycleService) UpdateEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (application.MutationResult, error) {
	res, err := s.tasks.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// UpdateLastPort 写入 last_port（非迁移列写入）。
func (s *LifecycleService) UpdateLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	res, err := s.tasks.UpdateTaskLastPort(ctx, id, port)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// commitTransitionResult 提交迁移类写入：真实状态迁移发布 task.status_changed，
// 否则（同值 no-op / 同值 last_error 变更）按 Changed=true 发布 task.activity_changed。
func (s *LifecycleService) commitTransitionResult(ctx context.Context, taskID string, res application.TransitionResult) {
	if res.StatusChanged {
		s.commitTransition(ctx, taskID, res)
		return
	}
	s.commitTaskMutation(ctx, taskID, res.MutationResult)
}

// commitTaskCreated 提交任务行创建：发布 task.created（NoopPublisher 阶段无实际发布）。
func (s *LifecycleService) commitTaskCreated(taskID string) {
	s.publish.Publish(ocdeckevent.NewTaskCreated(taskID))
}
