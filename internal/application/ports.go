// ports.go 定义应用层 repository 与外部能力端口（design.md D0:71-86）。
//
// 端口为 consumer-owned：由 application 层声明所需能力签名，infrastructure/sqlite
// adapter 实现。SessionRepository 方法闭合为 Claim/TouchOwned/DeleteOwned/Align/
// OwnedSessions/OwnerOf，MUST NOT 暴露通用 Save/Upsert（避免绕过领域不变量）。
package application

import (
	"context"

	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
)

// TaskRepository 表达任务聚合的持久化端口（design.md D0）。
//
// 状态写入返回 TransitionResult（区分 status 是否真实迁移）；其余单列写入返回
// MutationResult。DeleteTask 返回 DeleteResult（含被删行原状态与级联 session ID）。
type TaskRepository interface {
	UpdateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (TransitionResult, error)
	UpdateTaskStatusConditional(ctx context.Context, id string, fromStatus, toStatus ocdecktask.Status, lastError *string) (TransitionResult, error)
	UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (MutationResult, error)
	UpdateTaskLastPort(ctx context.Context, id string, port int) (MutationResult, error)
	UpdateTaskNotice(ctx context.Context, id string, notice *string) (MutationResult, error)
	UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (MutationResult, error)
	SetTaskDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (MutationResult, error)
	BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (TransitionResult, error)
	ArchiveTask(ctx context.Context, id string) (TransitionResult, error)
	RestoreTask(ctx context.Context, id string) (TransitionResult, error)
	DeleteTask(ctx context.Context, id string) (DeleteResult, error)

	// init_status CAS（design.md §2.1/§3）
	CommitCreated(ctx context.Context, taskID string, expectedStatus ocdecktask.Status, initStatus ocdecktask.InitStatus) (TransitionResult, error)
	ClaimInitRun(ctx context.Context, taskID string) (MutationResult, error)
	ClaimInitRerun(ctx context.Context, taskID string) (MutationResult, error)
	FinishInitRun(ctx context.Context, taskID string, status ocdecktask.InitStatus, initError *string) (MutationResult, error)
	ConvergeInterruptedInitRuns(ctx context.Context) (int64, error)

	// 读取（供 application 编排用）
	GetTask(ctx context.Context, id string) (*ocdecktask.Task, error)
}

// SessionRepository 表达会话归属隔离的持久化端口（design.md D0:78-86）。
//
// 方法闭合为 Claim/TouchOwned/DeleteOwned/Align/OwnedSessions/OwnerOf，MUST NOT 暴露
// 通用 Save/Upsert。OwnerOf 读到历史重复归属时 fail-closed 返回
// session.AmbiguousOwnerError。
type SessionRepository interface {
	Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (ClaimResult, error)
	TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (MutationResult, error)
	DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error)
	Align(ctx context.Context, taskID string, observed []ocdecksess.Session, notice NoticeMutation) (AlignResult, error)
	OwnedSessions(ctx context.Context, taskID string) ([]ocdecksess.ID, error)
	// OwnerOf 反查 session_id 的归属 task。读到历史重复归属时 fail-closed 返回
	// session.AmbiguousOwnerError（typed ambiguity）；found=false 表示无归属行。
	OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (taskID string, found bool, err error)
}

// ProjectReader 读侧端口（design.md D0:86）。
type ProjectReader interface {
	// GetProject 返回项目基础信息（id/name/path/default_branch/kind）。
	GetProject(ctx context.Context, id string) (Project, error)
}

// Project 为 application 层读出的项目快照（与存储行同构，避免暴露 store 包类型）。
type Project struct {
	ID            string
	Name          string
	Path          string
	DefaultBranch string
	Kind          string
	CreatedAt     int64
}

// EnvReader 读侧 env 端口（design.md D0:86）。全局/项目/任务三级 env 列表。
type EnvReader interface {
	ListGlobalEnvVars(ctx context.Context) ([]EnvVar, error)
	ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVar, error)
	ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVar, error)
}

// EnvVar 为 application 层读出的 env 变量（解耦存储包类型）。
type EnvVar struct {
	Key   string
	Value string
	Mode  string // 仅全局 env：follow_host | manual
}

// CleanupDebtRepository 持久化未收敛的 orphan cleanup tickets（design.md §10）。
type CleanupDebtRepository interface {
	UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error
	DeleteCleanupDebt(ctx context.Context, sessionName string) error
	ListCleanupDebts(ctx context.Context) ([]CleanupDebt, error)
}

// CleanupDebt 为 application 层读出的 orphan cleanup debt。
type CleanupDebt struct {
	SessionName string
	Tickets     string // JSON 编码 []string
	CreatedAt   int64
}

// ProcessPort 抽象进程会话环境读取能力（design.md D0:86）。
//
// MUST 提供 context-aware 的会话环境读取方法：签名携带 ctx，调用方取消/超时使调用
// 在预算内终止。既有非 context 接口保留且行为不变（legacy lane 仍可使用）。
type ProcessPort interface {
	// ShowSessionEnvContext 同 ShowSessionEnv 但使用调用方 ctx：调用方取消/超时
	// MUST 使该调用在预算内终止（cross-project-active-sessions D0）。
	ShowSessionEnvContext(ctx context.Context, name, key string) (string, error)
}

// OpenCodePort 抽象 opencode 客户端读侧能力（design.md D0:86）。
// 完整 REST/SSE 能力仍在 internal/task.OCClient；此处仅声明 application 编排所需子集。
type OpenCodePort interface {
	Health(ctx context.Context) error
	ListSessions(ctx context.Context, dir string, limit int) ([]OpenCodeSession, error)
}

// OpenCodeSession 为 application 层读出的 opencode session（解耦 opencode 包类型）。
type OpenCodeSession struct {
	ID        string
	Title     string
	CreatedAt int64
	UpdatedAt int64
}

// WorktreePort 抽象 worktree 读侧/前置校验能力（design.md D0:86）。
// 完整能力仍在 internal/task.WorktreeBackend；此处仅声明 application 编排所需子集。
type WorktreePort interface {
	BranchExists(ctx context.Context, repoPath, branch string) (bool, error)
	ValidateBranchName(ctx context.Context, repoPath, branch string) error
}