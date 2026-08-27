package task

import (
	"context"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
)

// BeginDeleteIntent records the delete intent transition (guarded by fromStatuses)
// and commits the resulting domain event.
func (s *LifecycleService) BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (application.TransitionResult, error) {
	res, err := s.tasks.BeginDeleteIntent(ctx, id, mode, fromStatuses)
	if err != nil {
		return application.TransitionResult{}, err
	}
	s.commitTransitionResult(ctx, id, res)
	return res, nil
}

// DeleteTask removes the task row (cascading remaining sessions) and commits
// session.deleted events for each cascaded session before the task.deleted event.
func (s *LifecycleService) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	res, err := s.tasks.DeleteTask(ctx, id)
	if err != nil {
		return application.DeleteResult{}, err
	}
	if res.Affected > 0 {
		for _, sid := range res.CascadedSessionIDs {
			s.publish.Publish(ocdeckevent.NewSessionDeleted(sid, id))
		}
		s.publish.Publish(ocdeckevent.NewTaskDeleted(id, string(res.From)))
	}
	return res, nil
}

// UpdateNoticeCAS conditionally rewrites the task notice and commits an
// activity_changed event when the mutation actually changed.
func (s *LifecycleService) UpdateNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	res, err := s.tasks.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// ConvergeInterruptedInitRuns fails stale init runs left behind by a restart.
// Startup-time converge runs before HTTP is open (zero subscribers) and MUST
// NOT publish activity_changed (task-detail-stream D0 exception).
func (s *LifecycleService) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return s.tasks.ConvergeInterruptedInitRuns(ctx)
}

// ClaimInitRun marks an initial run as started for this server instance.
// Real changes (Changed=true) publish task.activity_changed.
func (s *LifecycleService) ClaimInitRun(ctx context.Context, id string) (application.MutationResult, error) {
	res, err := s.tasks.ClaimInitRun(ctx, id)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// ClaimInitRerun marks a finished init run as re-running.
// Real changes (Changed=true) publish task.activity_changed.
func (s *LifecycleService) ClaimInitRerun(ctx context.Context, id string) (application.MutationResult, error) {
	res, err := s.tasks.ClaimInitRerun(ctx, id)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}

// FinishInitRun records the terminal init status of an init run.
// Real changes (Changed=true) publish task.activity_changed.
func (s *LifecycleService) FinishInitRun(ctx context.Context, id string, status ocdecktask.InitStatus, initError *string) (application.MutationResult, error) {
	res, err := s.tasks.FinishInitRun(ctx, id, status, initError)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitTaskMutation(ctx, id, res)
	return res, nil
}
