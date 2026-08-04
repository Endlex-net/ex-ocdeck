// Package task 实现 TaskManager：任务状态转换、进程、worktree 操作的唯一入口
// （design.md §1/§5/§18/§19）。
//
// 设计要点：
//   - 每任务 keyed mutex（冲突操作返回 409，个人单用户场景不做 in-flight 结果共享）
//   - 副作用边界（§19）：前置检查 → 意图落库 → 外部副作用 → 提交点 → 补偿/重试
//   - 启动 reconciliation（§5/§10）按 shutdownPolicy 收敛
//   - 后台周期重试（30s）消化 retryable notice
//
// TaskManager 通过依赖接口注入 process/opencode/store/worktree/git/pty，
// 便于测试 mock 边界（design.md §18 依赖方向 task→{process,pty,worktree,git,opencode,store}）。
package task

import (
	"context"
	"errors"

	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
	"ocdeck/internal/pty"
)

// DeleteMode 删除模式（design.md §19）。
type DeleteMode string

const (
	DeleteNormal DeleteMode = "normal"
	DeleteForce  DeleteMode = "force"
)

// TerminalID 标识一个 shell 终端（design.md §18 CreateShell/CloseShell）。
type TerminalID string

// Status 用户态 + 内部过渡态（design.md §5）。
const (
	StatusSuspended      = "suspended"
	StatusActive         = "active"
	StatusArchived       = "archived"
	StatusCreating       = "creating"
	StatusCreationFailed = "creation_failed"
	StatusActivating     = "activating"
	StatusSuspending     = "suspending"
	StatusDeleting       = "deleting"
	StatusDeletionFailed = "deletion_failed"
)

// InitStatus init_status 域（design.md §3：none | pending | running | succeeded | failed）。
// Create 链按是否配置 init 脚本落 pending（待 InitRunner 执行）或 none（无脚本直接激活）。
// 既有任务迁移为 none。Activate 门禁按 §5 五分支放行/拒绝。
const (
	InitStatusNone      = "none"
	InitStatusPending   = "pending"
	InitStatusRunning   = "running"
	InitStatusSucceeded = "succeeded"
	InitStatusFailed    = "failed"
)

// --- 依赖接口（design.md §18，供 mock 边界） ---

// ProcessBackend 抽象 process.Manager 的会话生命周期方法。
type ProcessBackend interface {
	NewSession(spec process.SessionSpec) error
	KillSession(name string) (process.KillResult, error)
	RetryReap(tickets []string) ([]string, error)
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	ShowSessionEnv(name, key string) (string, error)
	WatchExit(name string, callback func(process.WatchEvent)) (cancel func(), done <-chan struct{})
	AttachPty(name string, cols, rows int) (*pty.Pty, error)
}

// OCClient 抽象 opencode.Client 的 REST/SSE 能力。
type OCClient interface {
	Health(ctx context.Context) (opencode.HealthResponse, error)
	Probe(ctx context.Context) (version string, err error)
	ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error)
	GetSession(ctx context.Context, dir, id string) (opencode.Session, error)
	CreateSession(ctx context.Context, dir, title string) (opencode.Session, error)
	DeleteSession(ctx context.Context, dir, id string) error
	SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error)
	SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error
}

// OCClientFactory 构造指向某 serve（port+password）的 OCClient。
type OCClientFactory func(port int, password string, opts opencode.Options) OCClient

// WorktreeBackend 抽象 worktree.Manager。
type WorktreeBackend interface {
	Add(ctx context.Context, repoPath, projectID, taskID, branch, baseRef string) (string, error)
	Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error
	// BranchExists 判断分支是否已存在（B1：Create 落库前检查分支冲突）。
	BranchExists(ctx context.Context, repoPath, branch string) (bool, error)
	// ValidateBranchName 用 git check-ref-format 校验分支名合法性（P1：Create 前置检查，
	// design.md §19）。无副作用，前置完成于落库前。
	ValidateBranchName(ctx context.Context, repoPath, branch string) error
	// VerifyWorktreeProduct 严格产物验证（B1：RetryCreate 幂等跳过 add 的判定依据，
	// design.md §19 Create Retry 行）。校验：路径存在 + .git 文件 + rev-parse --is-inside-work-tree
	// + 检出分支匹配 + 属预期 repo。全部通过返回 nil，否则返回明确错误。
	VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error
	// PreflightDelete 在删除副作用前做静态安全检查（B8：包含性/dirty/分支占用先于 oc session 清理）。
	// ConfirmDirty=true 表示调用方已确认 dirty（API 层 confirmDirty=true 或 task 层 force 删除不再自动确认）。
	PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error
	// DirtyFiles 返回 worktree 中 dirty/untracked 文件路径集合（B7c：删除二次门禁用）。
	// worktree 不存在返回空集 + nil。
	DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error)
}

// PreflightDeleteOpts 删除前置检查选项（B8）。
type PreflightDeleteOpts struct {
	RepoPath     string
	Branch       string
	ConfirmDirty bool // 用户已确认 dirty（API 层 confirmDirty=true）
}

// worktreeRemoveOpts 解耦 worktree.RemoveOpts（避免 task 直接依赖 worktree 包结构）。
type worktreeRemoveOpts struct {
	RepoPath   string
	Branch     string
	ForceDirty bool
}

// --- 错误语义（design.md §21 code 枚举，task 层用 typed error，边界映射在 api） ---

// OpError 携带语义化错误码，供 api 层映射 HTTP code/msg（design.md §21）。
// task 层内部流转 err-first；仅在跨边界返回时附带 code。
type OpError struct {
	Code string // 对应 api.ErrorCode 枚举（conflict/not_found/invalid_state/...）
	Err  error
}

func (e *OpError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *OpError) Unwrap() error { return e.Err }

// CodeOf 返回错误码（供 api 边界映射）。
func (e *OpError) CodeOf() string { return e.Code }

// OpErrorCode 返回 OpError 的 code（若 err 为 *OpError，否则空串）。
func OpErrorCode(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

// newOpErr 构造 OpError。
func newOpErr(code string, err error) *OpError { return &OpError{Code: code, Err: err} }

// 错误码常量（与 api.ErrorCode 字面量一致，避免循环引用）。
const (
	codeConflict       = "conflict"
	codeNotFound       = "not_found"
	codeInvalidState   = "invalid_state"
	codeInvalidInput   = "invalid_input"
	codeInternal       = "internal"
	codeProcessError   = "process_error"
	codeGitError       = "git_error"
	codeOCIncompatible = "oc_incompatible"
)
