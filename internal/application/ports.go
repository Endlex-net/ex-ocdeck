// ports.go 定义应用层 repository 与外部能力端口（design.md D0:71-86）。
//
// 端口为 consumer-owned：由 application 层声明所需能力签名，infrastructure/sqlite
// adapter 实现。SessionRepository 方法闭合为 Claim/TouchOwned/DeleteOwned/Align/
// OwnedSessions/OwnerOf，MUST NOT 暴露通用 Save/Upsert（避免绕过领域不变量）。
package application

import (
	"context"
	"errors"

	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
)

// ErrTaskNotFound 为读侧端口返回任务未命中的 sentinel error（design.md D0 consumer-owned）。
//
// sqlite adapter 把底层 sql.ErrNoRows 归一化为本 sentinel，application 经
// LifecycleService 透传，Manager facade 映射为 task.OpError{codeNotFound}
// （保持 api.TaskBackend 契约与 OpError 映射逐字不变）。
var ErrTaskNotFound = errors.New("task not found")

// Publisher 为 application commit helper 发布事件的窄接口（design.md D0:133）。
//
// P1.4.4 阶段注入 NoopPublisher（不发布任何事件）；真实事件生产挂接在 Phase C/P1.6。
// Publish 非阻塞：订阅方缓冲满时丢弃该事件并置位溢出信号，MUST NOT 回滚业务提交。
type Publisher interface {
	Publish(ev ocdeckevent.Event)
}

// TaskRepository 表达任务聚合的持久化端口（design.md D0）。
//
// 状态写入返回 TransitionResult（区分 status 是否真实迁移）；其余单列写入返回
// MutationResult。DeleteTask 返回 DeleteResult（含被删行原状态与级联 session ID）。
type TaskRepository interface {
	// CreateTask 插入任务行（status 由调用方提供，creating 意图落库）。
	// 仅消费 ID/ProjectID/Name/Branch/Status/WorktreePath/BaseRef；MUST NOT 再校验 status。
	CreateTask(ctx context.Context, row TaskSnapshot) error
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

// TaskReadRepository 为任务读侧端口（design.md D0:71 consumer-owned）。
//
// Get/List 用例需要全行快照（含创建期可变信息与持久化元数据），而 TaskRepository.GetTask
// 仅返回 guard 视图（status/init_status/delete_mode/notices）。本端口闭合为 Get/List 读侧，
// 返回 application 层 TaskSnapshot（普通 Go 类型，MUST NOT 泄漏 sql.NullString），
// 供 LifecycleService 编排与 Manager facade 转换为 task.TaskRow（迁移期 api.TaskBackend 契约冻结）。
type TaskReadRepository interface {
	GetTaskRow(ctx context.Context, id string) (TaskSnapshot, error)
	ListTasksByProject(ctx context.Context, projectID string) ([]TaskSnapshot, error)
}

// TaskSnapshot 为 application 层读出的任务全行快照（design.md D0:71 consumer-owned）。
//
// 字段与 store.TaskRow / task.TaskRow 一一对应，但用普通 Go 类型表达 nullable
// （*string / *int64），MUST NOT 泄漏 sql.NullString。Manager facade 转换为 task.TaskRow
// 时还原 sql.NullString（迁移期 api.TaskBackend 契约返回 task.TaskRow，逐字不变）。
type TaskSnapshot struct {
	ID           string
	ProjectID    string
	Name         string
	Branch       string
	Status       string
	WorktreePath string
	LastPort     *int64
	LastError    *string
	Notice       *string
	DeleteMode   *string
	EnvSnapshot  *string
	CreatedAt    int64
	UpdatedAt    int64
	ArchivedAt   *int64
	InitStatus   string
	InitError    *string
	BaseRef      string
}

// SessionRepository 表达会话归属隔离的持久化端口（design.md D0:78-86）。
//
// 方法闭合为 Claim/TouchOwned/DeleteOwned/Align/OwnedSessions/OwnerOf，MUST NOT 暴露
// 通用 Save/Upsert。OwnerOf 读到历史重复归属时 fail-closed 返回
// session.AmbiguousOwnerError。
type SessionRepository interface {
	// Claim 原子认领归属：单事务先查他主再 upsert；changed=新插入或 last_seen_at/parent_id
	// 实际推进（design.md D0:77）。obs.FirstSeenAt 为 ocdeck 首次观测时间。
	Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (ClaimResult, error)
	// TouchOwned 条件推进本任务已归属行的 last_seen_at（绝不插入）；Matched=命中归属行，
	// Changed=值真实推进（值不变为 Matched+!Changed 同值幂等）。
	TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (MutationResult, error)
	// DeleteOwned 删除归属行，返回受影响行数（0=行不存在，同值幂等成功）。
	DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error)
	// Align 单事务批处理一致性对齐（design.md D0:80/86）：按 mode 处理 observed（repo 逐个
	// 原子 claim / ownedOnly 仅刷新 listed∩owned），complete=true 时删 owned 缺席行并同事务
	// 提交 notice 变更（expected 失配整事务回滚返回 AlignConflict）；complete=false 不删缺席行、
	// 不触碰 notice（overflow 的 session_overflow 由 application 在 Align 之前经事务外 CAS 写入，
	// Align 失败 MUST NOT 回滚该 notice）。
	Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice NoticeMutation) (AlignResult, error)
	// OwnedSessions 返回本任务拥有的全部 session ID（对账交集用）。
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