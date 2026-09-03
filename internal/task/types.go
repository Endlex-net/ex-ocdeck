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
	"fmt"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/pty"
)

// --- 迁移别名（sse-active-sessions P1.9a：定义已迁至 internal/application，
// 锁定 import 方向 api → application（design.md D0:55）；本包及既有引用零改动） ---

// DeleteMode 删除模式（design.md §19）。
type DeleteMode = application.DeleteMode

const (
	DeleteNormal = application.DeleteNormal
	DeleteForce  = application.DeleteForce
)

// TerminalID 标识一个 shell 终端（design.md §18 CreateShell/CloseShell）。
type TerminalID = application.TerminalID

// Status 用户态 + 内部过渡态（design.md §5）。
const (
	StatusSuspended      = application.StatusSuspended
	StatusActive         = application.StatusActive
	StatusArchived       = application.StatusArchived
	StatusCreating       = application.StatusCreating
	StatusCreationFailed = application.StatusCreationFailed
	StatusActivating     = application.StatusActivating
	StatusSuspending     = application.StatusSuspending
	StatusDeleting       = application.StatusDeleting
	StatusDeletionFailed = application.StatusDeletionFailed
)

// InitStatus init_status 域（design.md §3：none | pending | running | succeeded | failed）。
// Create 链按是否配置 init 脚本落 pending（待 InitRunner 执行）或 none（无脚本直接激活）。
// 既有任务迁移为 none。Activate 门禁按 §5 五分支放行/拒绝。
const (
	InitStatusNone      = application.InitStatusNone
	InitStatusPending   = application.InitStatusPending
	InitStatusRunning   = application.InitStatusRunning
	InitStatusSucceeded = application.InitStatusSucceeded
	InitStatusFailed    = application.InitStatusFailed
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
	// ShowSessionEnvContext 同 ShowSessionEnv 但使用调用方 ctx（cross-project-active-sessions D0）。
	ShowSessionEnvContext(ctx context.Context, name, key string) (string, error)
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
	// ListPermissions GET /permission → pending 权限请求快照（design.md D6 注意力信号）。
	// 404 → opencode.ErrCapabilityUnsupported（能力状态机迁移 unsupported）。
	ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error)
	// ListQuestions GET /question → pending 问题请求快照（design.md D6）。
	ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error)
	// PromptAsync 投递一条 text prompt 到目标 session 的异步队列（design.md D1）。
	// 返回 transport DTO（不返回 error）；签名与 *opencode.Client 逐字一致。
	// adapter 获取失败（taskOcClient ok=false）由 PromptPort 返回 pre_send_failure（D1），
	// 不属于本接口的职责。
	PromptAsync(ctx context.Context, dir, sessionID, messageID, text string, files []opencode.PromptFilePart) opencode.PromptResult
	// ProbePromptAsyncCapability 探测目标 serve 是否支持 prompt_async（design.md D1）。
	// GET /doc 结构化解析，返回 supported/unsupported/unknown 三值。
	// 签名与 *opencode.Client 逐字一致；adapter 在 RuntimePort.ProbeCapability 复用。
	ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState
}

// 编译期断言：*opencode.Client 实现 OCClient（签名 MUST 与 *Client 逐字一致，
// design.md D1）。factory 直接返回 *opencode.Client（manager.go:448）。
var _ OCClient = (*opencode.Client)(nil)

// OCClientFactory 构造指向某 serve（port+password）的 OCClient。
type OCClientFactory func(port int, password string, opts opencode.Options) OCClient

// WorktreeBackend 抽象 worktree.Manager。
type WorktreeBackend interface {
	// Add 在 dest 创建 worktree（dest 由调用方计算，MUST 位于 worktrees 根之下；
	// 副作用前 worktree.Manager 会先做 checkContainment）。失败时由 Manager 走 cleanupFailedAdd。
	Add(ctx context.Context, repoPath, dest, branch, baseRef string) error
	Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error
	// BranchExists 判断分支是否已存在（B1：Create 落库前检查分支冲突）。
	BranchExists(ctx context.Context, repoPath, branch string) (bool, error)
	// ValidateBranchName 用 git check-ref-format 校验分支名合法性（P1：Create 前置检查，
	// design.md §19）。无副作用，前置完成于落库前。
	ValidateBranchName(ctx context.Context, repoPath, branch string) error
	// ResolveBaseRef 将 base_ref 短名解析为全限定 ref（add-plain-dir-project D10）。
	// 解析顺序 refs/heads/<name> → refs/remotes/<name>（heads 优先），仅接受这两个命名空间；
	// 不存在返回错误（调用方映射 invalid_input）。调用方 MUST 先经 ValidateBranchName 做规范校验。
	// 无副作用只读，前置完成于落库前。task 包不直接依赖 internal/infrastructure/git，经此端口解析。
	ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error)
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

// BranchNamer 将任务名提炼为分支 slug（design.md ai-worktree-naming D3/D4）。
//
// Slug 永不返回 error：由实现内部决定 AI 提炼或回退到机械 slugify（fallback 由 wiring 注入，
// 通常是 task.Slugify）。返回的 slug 已经过清洗或 fallback，调用方直接拼 `ocdeck/` 前缀即可。
// 本接口只定义抽象，避免 task 包 import internal/infrastructure/ai；具体实现（ai.SlugNamer）在 main.go
// wiring 阶段注入（tasks 3.3）。Manager 持有 namer 为 nil 时回退到本包 Slugify（防 panic）。
type BranchNamer interface {
	Slug(ctx context.Context, taskName string) string
}

// PreflightDeleteOpts 删除前置检查选项（B8）。
type PreflightDeleteOpts struct {
	RepoPath     string
	Branch       string
	ConfirmDirty bool // 用户已确认 dirty（API 层 confirmDirty=true）
}

// --- session 归属隔离（add-plain-dir-project D8） ---

// AlignMode 对齐模式（task 层常量，StoreAdapter 映射到 store.AlignMode；
// 合法值仅两种，未知值 MUST 在任何写入前返回错误——fail-closed）。
type AlignMode int

const (
	// AlignModeRepo 目录私有：listed 逐个原子 claim，冲突 ID 上报；complete 删 owned 缺席行。
	// 单任务场景 claim guard 永不命中，与既有 upsert 行为逐点一致。
	AlignModeRepo AlignMode = iota + 1
	// AlignModeOwnedOnly 目录可共享（dir）：仅对 listed∩owned 刷新 last_seen_at，绝不 claim；
	// complete 仅删 owned 缺席行。
	AlignModeOwnedOnly
)

// SessionObservation 持久化中立的会话观测（application 层从 opencode DTO 转换，
// 仅含归属写回所需字段：SessionID/CreatedAt/UpdatedAt/ParentID）。StoreAdapter 映射到 store 层类型。
type SessionObservation struct {
	SessionID string
	CreatedAt int64
	UpdatedAt int64
	ParentID  string
}

// ProjectKind 项目类型枚举（add-plain-dir-project D1）。未知值 fail-closed。
const (
	ProjectKindRepo = "repo"
	ProjectKindDir  = "dir"
)

// alignModeForKind 按项目 kind 解析对齐模式（add-plain-dir-project D8）。
// repo → AlignModeRepo；dir → AlignModeOwnedOnly；未知值 fail-closed 返回错误。
// 调用方 MUST 在任何副作用前调用本函数取得 mode（design.md D8：MUST NOT 在 serve 启动后才发现未知 kind）。
func alignModeForKind(kind string) (AlignMode, error) {
	switch kind {
	case ProjectKindRepo:
		return AlignModeRepo, nil
	case ProjectKindDir:
		return AlignModeOwnedOnly, nil
	default:
		return 0, fmt.Errorf("task: unknown project kind %q", kind)
	}
}

// worktreeRemoveOpts 解耦 worktree.RemoveOpts（避免 task 直接依赖 worktree 包结构）。
type worktreeRemoveOpts struct {
	RepoPath   string
	Branch     string
	ForceDirty bool
}

// --- 错误语义（design.md §21 code 枚举，task 层用 typed error，边界映射在 api） ---
// OpError 定义已迁至 internal/application（operror.go）；此处保留别名与薄包装。

type OpError = application.OpError

// OpErrorCode 返回 OpError 的 code（若 err 为 *OpError，否则空串）。
func OpErrorCode(err error) string { return application.OpErrorCode(err) }

// newOpErr 构造 OpError。
func newOpErr(code string, err error) *OpError { return application.NewOpErr(code, err) }

// 错误码常量（与 api.ErrorCode 字面量一致，避免循环引用）。
const (
	codeConflict       = application.CodeConflict
	codeNotFound       = application.CodeNotFound
	codeInvalidState   = application.CodeInvalidState
	codeInvalidInput   = application.CodeInvalidInput
	codeInternal       = application.CodeInternal
	codeProcessError   = application.CodeProcessError
	codeGitError       = application.CodeGitError
	codeOCIncompatible = application.CodeOCIncompatible
	codeRecovering     = application.CodeRecovering
)
