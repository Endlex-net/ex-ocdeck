// archive_restore.go 实现 Archive/Restore 用例（design.md D0:140 迁移第 4 步）。
//
// 两个用例的统一形态（design.md D0:124-125 状态机矩阵）：
//  1. 决策先于副作用（design.md D0:156）：先经 TaskRepository.GetTask 读 guard 视图，
//     再经 domain CanArchive/CanRestore guard 判定；guard 拒绝叶节点零副作用。
//  2. guard 通过后经 TaskRepository.ArchiveTask/RestoreTask CAS 提交（返回 TransitionResult）。
//  3. commit helper：仅 TransitionResult.StatusChanged=true 时发布 task.status_changed
//     （design.md D0:133）。本阶段 NoopPublisher，调用位就绪无实际发布。
//
// guard 决策与错误消息生成在 application 层完成，Manager facade 映射为 OpError
// （codeInvalidState，逐字不变）。Archive guard 拒绝时按现状维度顺序生成错误
// （status 优先，init 次之），保持委托前后行为字节级等价。
package task

import (
	"context"
	"fmt"

	ocdecktask "ocdeck/internal/domain/task"
)

// ArchiveError 表达 Archive 用例 guard 拒绝的 typed error（design.md D0:148 OpError 映射冻结）。
//
// application 层 err-first：guard 拒绝返回 typed error，由 Manager facade 映射为
// task.OpError{codeInvalidState}。两种拒绝维度（status 非 suspended / init 进行中）
// 用 typed 标记区分，Manager 按维度生成逐字一致的消息。
type ArchiveError struct {
	Reason     ArchiveRejectReason
	Status     string
	InitStatus string
	TaskID     string
}

// ArchiveRejectReason 表达 Archive guard 拒绝的维度（status 优先于 init，byte-equivalent）。
type ArchiveRejectReason int

const (
	// ArchiveRejectStatus 非 suspended 状态拒绝。
	ArchiveRejectStatus ArchiveRejectReason = iota + 1
	// ArchiveRejectInit init 进行中拒绝（status 已是 suspended）。
	ArchiveRejectInit
)

func (e *ArchiveError) Error() string {
	switch e.Reason {
	case ArchiveRejectStatus:
		return fmt.Sprintf("archive requires suspended, got %s", e.Status)
	case ArchiveRejectInit:
		return fmt.Sprintf("archive: task %s init in progress (init_status=%s)", e.TaskID, e.InitStatus)
	default:
		return "archive rejected"
	}
}

// Archive 归档任务（design.md D0:124 状态机矩阵行 suspended→archived）。
//
// 决策先于副作用：先读 guard 视图 → domain CanArchive guard → CAS 提交 → commit helper。
// guard 拒绝返回 *ArchiveError（typed，零副作用）；CAS 返回 TransitionResult，
// StatusChanged=true 时经 commitTransition 发布（本阶段 NoopPublisher）。
// 同值 no-op（已 archived）Matched=true / Changed=false → 不发布，对齐同值原子语义。
func (s *LifecycleService) Archive(ctx context.Context, taskID string) error {
	t, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !t.CanArchive() {
		return &ArchiveError{
			Reason:     archiveRejectReason(t),
			Status:     string(t.Status()),
			InitStatus: string(t.InitStatus()),
			TaskID:     taskID,
		}
	}
	res, err := s.tasks.ArchiveTask(ctx, taskID)
	if err != nil {
		return err
	}
	s.commitTransition(ctx, taskID, res)
	return nil
}

// archiveRejectReason 按 domain guard 决策结果推断拒绝维度（status 优先，byte-equivalent）。
//
// domain CanArchive=false 有两种维度：status 非 suspended，或 status=suspended 但 init 进行中。
// 调用方已确认 CanArchive=false，此处按 status 优先顺序判定维度，生成与 legacy 逐字一致的消息。
func archiveRejectReason(t *ocdecktask.Task) ArchiveRejectReason {
	if t.Status() != ocdecktask.StatusSuspended {
		return ArchiveRejectStatus
	}
	return ArchiveRejectInit
}

// RestoreError 表达 Restore 用例 guard 拒绝的 typed error（design.md D0:148 OpError 映射冻结）。
type RestoreError struct {
	Status string
}

func (e *RestoreError) Error() string {
	return fmt.Sprintf("restore requires archived, got %s", e.Status)
}

// Restore 从归档恢复挂起（design.md D0:125 状态机矩阵行 archived→suspended）。
//
// 决策先于副作用：先读 guard 视图 → domain CanRestore guard → CAS 提交 → commit helper。
// guard 拒绝返回 *RestoreError（typed，零副作用）；CAS 返回 TransitionResult，
// StatusChanged=true 时经 commitTransition 发布（本阶段 NoopPublisher）。
func (s *LifecycleService) Restore(ctx context.Context, taskID string) error {
	t, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !t.CanRestore() {
		return &RestoreError{Status: string(t.Status())}
	}
	res, err := s.tasks.RestoreTask(ctx, taskID)
	if err != nil {
		return err
	}
	s.commitTransition(ctx, taskID, res)
	return nil
}