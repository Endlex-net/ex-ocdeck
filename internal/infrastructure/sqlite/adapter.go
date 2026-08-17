// Package sqlite 是 application ports 的 SQLite 持久化适配器（design.md D0）。
//
// 包装既有 *store.DB，实现 application 层 TaskRepository/SessionRepository/ProjectReader
// /EnvReader/CleanupDebtRepository/ProcessPort/OpenCodePort/WorktreePort。
//
// P1.2 仅提供骨架 + 结构化结果映射：store 写方法已直接返回 application 结果类型，
// 适配器对 reformed 方法做 1:1 透传；尚未接入的 LifecycleService 在 P1.4 落地。
// 本阶段无 fx wiring（main 仍在 legacy lane），骨架仅保证编译与类型闭合。
package sqlite

import (
	"context"

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/store"
)

// Adapter 包装 *store.DB 实现 application ports 子集（design.md D0）。
//
// P1.2 阶段仅实现 TaskRepository/SessionRepository 等已 reformed 的方法骨架；
// ProcessPort/OpenCodePort/WorktreePort 的完整实现留待 P1.4 LifecycleService 接入。
type Adapter struct {
	db *store.DB
}

// New 构造 Adapter。
func New(db *store.DB) *Adapter { return &Adapter{db: db} }

// --- TaskRepository（design.md D0） ---

// UpdateTaskStatus 委托 store.UpdateTaskStatus，透传结构化结果。
func (a *Adapter) UpdateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatus(ctx, id, status, lastError)
}

// UpdateTaskStatusConditional 委托 store.UpdateTaskStatusConditional。
func (a *Adapter) UpdateTaskStatusConditional(ctx context.Context, id string, fromStatus, toStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

// UpdateTaskEnvSnapshot 委托 store.UpdateTaskEnvSnapshot。
func (a *Adapter) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (application.MutationResult, error) {
	return a.db.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

// UpdateTaskLastPort 委托 store.UpdateTaskLastPort。
func (a *Adapter) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return a.db.UpdateTaskLastPort(ctx, id, port)
}

// UpdateTaskNotice 委托 store.UpdateTaskNotice。
func (a *Adapter) UpdateTaskNotice(ctx context.Context, id string, notice *string) (application.MutationResult, error) {
	return a.db.UpdateTaskNotice(ctx, id, notice)
}

// UpdateTaskNoticeCAS 委托 store.UpdateTaskNoticeCAS。
func (a *Adapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	return a.db.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// SetTaskDeleteMode 委托 store.SetTaskDeleteMode。
func (a *Adapter) SetTaskDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (application.MutationResult, error) {
	return a.db.SetTaskDeleteMode(ctx, id, mode)
}

// BeginDeleteIntent 委托 store.BeginDeleteIntent。
func (a *Adapter) BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (application.TransitionResult, error) {
	return a.db.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

// ArchiveTask 委托 store.ArchiveTask。
func (a *Adapter) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.ArchiveTask(ctx, id)
}

// RestoreTask 委托 store.RestoreTask。
func (a *Adapter) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.RestoreTask(ctx, id)
}

// DeleteTask 委托 store.DeleteTask。
func (a *Adapter) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	return a.db.DeleteTask(ctx, id)
}

// CommitCreated 委托 store.CommitCreated。
func (a *Adapter) CommitCreated(ctx context.Context, taskID string, expectedStatus ocdecktask.Status, initStatus ocdecktask.InitStatus) (application.TransitionResult, error) {
	return a.db.CommitCreated(ctx, taskID, expectedStatus, initStatus)
}

// ClaimInitRun 委托 store.ClaimInitRun。
func (a *Adapter) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRun(ctx, taskID)
}

// ClaimInitRerun 委托 store.ClaimInitRerun。
func (a *Adapter) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRerun(ctx, taskID)
}

// FinishInitRun 委托 store.FinishInitRun。
func (a *Adapter) FinishInitRun(ctx context.Context, taskID string, status ocdecktask.InitStatus, initError *string) (application.MutationResult, error) {
	return a.db.FinishInitRun(ctx, taskID, status, initError)
}

// ConvergeInterruptedInitRuns 委托 store.ConvergeInterruptedInitRuns。
func (a *Adapter) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return a.db.ConvergeInterruptedInitRuns(ctx)
}

// GetTask 读侧：返回领域 Task 聚合（P1.2 骨架，完整映射留 P1.4）。
// A1 lane 已落地 domain/task，但 store 行→领域聚合映射属 LifecycleService（P1.4）。
// 本阶段返回 ErrNotImplemented 保持编译闭合，不阻塞 store 改造。
func (a *Adapter) GetTask(ctx context.Context, id string) (*ocdecktask.Task, error) {
	return nil, ErrNotImplemented
}

// --- SessionRepository（design.md D0:78-86） ---
//
// P1.2 骨架：Claim/TouchOwned/DeleteOwned/OwnedSessions/OwnerOf/Align 的领域映射
// 与事务边界编排属 LifecycleService（P1.4）。本阶段返回 ErrNotImplemented 保持编译。

// Claim 实现 SessionRepository.Claim（P1.4 LifecycleService 接入）。
func (a *Adapter) Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (application.ClaimResult, error) {
	return application.ClaimResult{}, ErrNotImplemented
}

// TouchOwned 实现 SessionRepository.TouchOwned（P1.4）。
func (a *Adapter) TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (application.MutationResult, error) {
	return application.MutationResult{}, ErrNotImplemented
}

// DeleteOwned 实现 SessionRepository.DeleteOwned（P1.4）。
func (a *Adapter) DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error) {
	return 0, ErrNotImplemented
}

// Align 实现 SessionRepository.Align（P1.4：notice 决策链 + 单事务边界）。
// notice expected 失配返回 application.AlignConflict（typed conflict），application 重试。
func (a *Adapter) Align(ctx context.Context, taskID string, observed []ocdecksess.Session, notice application.NoticeMutation) (application.AlignResult, error) {
	return application.AlignResult{}, ErrNotImplemented
}

// OwnedSessions 实现 SessionRepository.OwnedSessions（P1.4）。
func (a *Adapter) OwnedSessions(ctx context.Context, taskID string) ([]ocdecksess.ID, error) {
	return nil, ErrNotImplemented
}

// OwnerOf 实现 SessionRepository.OwnerOf（P1.4：fail-closed on ambiguous ownership）。
// 读到历史重复归属返回 ocdecksess.AmbiguousOwnerError；found=false 表示无归属行。
func (a *Adapter) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	return "", false, ErrNotImplemented
}

// --- ProjectReader / EnvReader / CleanupDebtRepository（P1.4 完整实现） ---

// GetProject 读侧项目（P1.4 行→application.Project 映射）。
func (a *Adapter) GetProject(ctx context.Context, id string) (application.Project, error) {
	return application.Project{}, ErrNotImplemented
}

// ListGlobalEnvVars 读全局 env（P1.4）。
func (a *Adapter) ListGlobalEnvVars(ctx context.Context) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// ListProjectEnvVars 读项目级 env（P1.4）。
func (a *Adapter) ListProjectEnvVars(ctx context.Context, projectID string) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// ListTaskEnvVars 读任务级 env（P1.4）。
func (a *Adapter) ListTaskEnvVars(ctx context.Context, taskID string) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// UpsertCleanupDebt 委托 store.UpsertCleanupDebt（P1.4 完整接入）。
func (a *Adapter) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	return a.db.UpsertCleanupDebt(ctx, sessionName, ticketsJSON, createdAt)
}

// DeleteCleanupDebt 委托 store.DeleteCleanupDebt。
func (a *Adapter) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	return a.db.DeleteCleanupDebt(ctx, sessionName)
}

// ListCleanupDebts 读侧 cleanup debt（P1.4 行→application.CleanupDebt 映射）。
func (a *Adapter) ListCleanupDebts(ctx context.Context) ([]application.CleanupDebt, error) {
	return nil, ErrNotImplemented
}

// ErrNotImplemented 表示该方法在 P1.2 骨架阶段尚未接入（留待 P1.4 LifecycleService）。
// 本阶段仅用于保证 adapter 实现全部 application ports 并编译闭合；不应在 P1.2 被调用。
var ErrNotImplemented = errNotImplemented{}

type errNotImplemented struct{}

func (errNotImplemented) Error() string { return "sqlite adapter: method not implemented in P1.2 skeleton" }

// --- ProcessPort / OpenCodePort / WorktreePort（P1.4 完整接入） ---
//
// 这三个 port 背后是 process/opencode/worktree 能力，非 store。P1.2 骨架返回
// ErrNotImplemented 保证接口闭合；P1.4 注入真实 client 实现并替换 Adapter 字段。

// ShowSessionEnvContext 实现 ProcessPort（P1.4 接入 process.Manager）。
func (a *Adapter) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	return "", ErrNotImplemented
}

// Health 实现 OpenCodePort（P1.4 接入 opencode.Client）。
func (a *Adapter) Health(ctx context.Context) error { return ErrNotImplemented }

// ListSessions 实现 OpenCodePort（P1.4）。
func (a *Adapter) ListSessions(ctx context.Context, dir string, limit int) ([]application.OpenCodeSession, error) {
	return nil, ErrNotImplemented
}

// BranchExists 实现 WorktreePort（P1.4 接入 worktree.Manager）。
func (a *Adapter) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	return false, ErrNotImplemented
}

// ValidateBranchName 实现 WorktreePort（P1.4）。
func (a *Adapter) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return ErrNotImplemented
}