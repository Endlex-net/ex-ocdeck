// Package diffreview 实现 diff-review-workbench 的用例协调器（design.md D9）。
//
// 本包承载批注/提交用例、队列调度、编辑读写等全部 diff review 相关 application 逻辑。
// 分层约束（与 internal/application/task 一致，design.md D9）：本包 MUST NOT 反向依赖
// internal/task 或 internal/infrastructure 包；仅依赖标准库与同层 application/domain 类型。
// 外部能力经 consumer-owned ports（本包定义、由外部 adapter 注入）获取：
//
//   - DiffReviewRepository：批注/提交持久化（SQLite adapter 在 store 包）
//   - PromptPort：异步投递 prompt 到任务 agent 会话（adapter 在 task 层，opencode.PromptResult→PromptOutcome 映射）
//   - DiffSourcePort：diff 来源内容读取（GitDiff 核心 helper 的能力面，adapter 在 task 层）
//   - RuntimePort：任务 runtime 快照（instVersion、锚定会话、能力缓存、SessionStatus），adapter 在 task 层
//   - TaskScopePort：任务作用域准入（任务存在性 + 项目 kind），SQLite adapter 在 store 包
//
// 本阶段（tasks 3.2）为骨架：定义 ports、domain 类型、service 构造器与字段，编译通过。
// 用例逻辑（3.4-3.11）由后续子任务填充。
package diffreview

import (
	"context"
	"errors"
)

// --- domain 类型：Prompt outcome（design.md D1 类型归属条） ---
//
// PromptOutcome/PromptOutcomeKind 为本包拥有的 domain 类型（consumer-owned）。
// 与低层 opencode.PromptResult（transport DTO，字段形状相同但归属 opencode 包）区分：
// task 层 adapter 做显式逐字段映射，MUST NOT 类型别名跨层共享。

// PromptOutcomeKind 分类 PromptPort.PromptAsync 的投递结果（design.md D1）。
// 四值与 opencode.PromptResultKind 一一对应（同形常量集），但为本包独立定义。
type PromptOutcomeKind string

const (
	// PromptOutcomeAccepted 服务端返回 204（请求已被接受、异步执行）。
	PromptOutcomeAccepted PromptOutcomeKind = "accepted"
	// PromptOutcomeHTTPResponse 收到任何状态码 != 204 的响应（含意外 2xx、400/401/404 等）。
	// StatusCode/Body 携带实际值，供上层按错误矩阵分流。
	PromptOutcomeHTTPResponse PromptOutcomeKind = "http_response"
	// PromptOutcomeTransportUnknown 请求已发出（httpClient.Do 返回错误：dial/超时/断连/ctx 取消），
	// 结果未知。MUST NOT 尝试区分"是否已发出"，一律按已发出处理。
	PromptOutcomeTransportUnknown PromptOutcomeKind = "transport_unknown"
	// PromptOutcomePreSendFailure httpClient.Do 之前的本地失败（marshal/NewRequest 或 adapter 获取 client 失败）。
	PromptOutcomePreSendFailure PromptOutcomeKind = "pre_send_failure"
)

// PromptOutcome 是 PromptPort.PromptAsync 的 domain 返回类型（design.md D1）。
//
// 字段填充规则（与 Kind 绑定，唯一，与 opencode.PromptResult 同形）：
//   - accepted:          StatusCode=204，Body/Detail 为空
//   - http_response:     StatusCode=实际状态码，Body=有界截断响应体，Detail 为空
//   - transport_unknown: StatusCode=0，Body 为空，Detail=底层错误文本
//   - pre_send_failure:  StatusCode=0，Body 为空，Detail=底层错误文本
type PromptOutcome struct {
	Kind       PromptOutcomeKind
	StatusCode int
	Body       string
	Detail     string
}

// --- consumer-owned ports（design.md D9） ---
//
// 五个端口均在本包定义、由外部实现注入。签名以 design.md D1/D9 为准。

// DiffAnnotationRecord 为 DiffReviewRepository 返回的活动批注行（domain 拥有，与 store 行同形）。
// ref/untracked 为来源元组；revision 为版本比对唯一依据；created_at/updated_at 秒级 Unix。
type DiffAnnotationRecord struct {
	ID                string
	TaskID            string
	Path              string
	Side              string
	Ref               string
	Untracked         bool
	StartLine         int
	EndLine           int
	SnapshotStartLine int
	Snapshot          string
	SnapshotLineCount int
	Comment           string
	Revision          int
	CreatedAt         int64
	UpdatedAt         int64
}

// DiffReviewSubmissionRecord 为 DiffReviewRepository 返回的提交行（domain 拥有，与 store 行同形）。
type DiffReviewSubmissionRecord struct {
	Seq             int64
	ID              string
	TaskID          string
	Status          string
	TargetSessionID string
	MessageID       string
	Note            string
	Payload         string
	Truncated       bool
	Error           string
	CreatedAt       int64
	SentAt          int64 // 0 表示未发送（store 侧 sql.NullInt64.Valid=false）
}

// DiffReviewSubmissionItemRecord 为提交快照条目（domain 拥有，与 store 行同形）。
type DiffReviewSubmissionItemRecord struct {
	SubmissionID       string
	AnnotationID       string
	AnnotationRevision int
	Path               string
	Side               string
	Ref                string
	Untracked          bool
	StartLine          int
	EndLine            int
	SnapshotStartLine  int
	Snapshot           string
	Comment            string
}

// CreateDiffAnnotationInput 为创建批注的入参（domain 拥有）。revision 由 store 原语初始化为 1。
type CreateDiffAnnotationInput struct {
	ID                string
	TaskID            string
	Path              string
	Side              string
	Ref               string
	Untracked         bool
	StartLine         int
	EndLine           int
	SnapshotStartLine int
	Snapshot          string
	SnapshotLineCount int
	Comment           string
}

// CreateDiffReviewSubmissionInput 为创建提交的入参（domain 拥有，与 store 原语同形）。
// 单事务写 submission（status=queued）+ items 快照 + 事务内逐条 revision 复核。
type CreateDiffReviewSubmissionInput struct {
	Submission DiffReviewSubmissionRecord
	Items      []DiffReviewSubmissionItemRecord
}

// CommentUpdateResult 表达编辑评论写入的三态结果（domain 拥有，与 store 三态同义）。
//   - Matched: WHERE 命中行（id 存在）。
//   - Changed: 命中行且评论发生真实变更（同值命中时 Changed=false，revision 不递增）。
//   - Revision: 真实变更后的新 revision（>0）；同值命中时为命中行当前 revision；未命中时为 0。
type CommentUpdateResult struct {
	Matched  bool
	Changed  bool
	Revision int
	// Record 为命中行的完整记录（F12：RETURNING/同事务 SELECT 取得；Matched=false 时为零值）。
	Record DiffAnnotationRecord
}

// ErrRevisionConflict 复核失败（事务内批注 revision 已变化或被删除），
// 调用方统一映射 conflict(409)（不区分「从未存在」与「预览后删除」，D2）。
var ErrRevisionConflict = errors.New("diffreview: submission revision conflict")

// DiffReviewRepository 表达批注/提交的持久化端口（design.md D9，SQLite adapter 在 store 包）。
// 方法签名闭合为 diff review 用例所需，MUST NOT 暴露通用 Save/Upsert（避免绕过领域不变量）。
//
// 方法集对应 store 原语（diff_review_queries.go），adapter 逐字透传不改语义：
//   - 批注 CRUD：CreateDiffAnnotation / UpdateDiffAnnotationComment / DeleteDiffAnnotation /
//     ListDiffAnnotationsByTask / GetDiffAnnotation
//   - 提交创建：CreateDiffReviewSubmission（单事务 + items 快照 + revision 复核）
//   - 队列/分区：ListDiffReviewQueue / ListDiffReviewHistory / ListDiffReviewFailures /
//     ListDiffReviewSubmissionItems / ListDiffReviewSubmissionPartitions / GetDiffReviewSubmission
//   - CAS/清理：CASDiffReviewSubmission / CompleteDiffReviewSentCleanup /
//     CancelDiffReviewSubmission / DeleteDiffReviewSubmission
//   - 启动收敛：ConvergeDiffReviewOnStartup
type DiffReviewRepository interface {
	// CreateDiffAnnotation 插入批注并返回完整行（F12：INSERT...RETURNING 同一语句，
	// 调用方不得在写入提交后再做必需的二次读取）。
	CreateDiffAnnotation(ctx context.Context, in CreateDiffAnnotationInput) (DiffAnnotationRecord, error)
	UpdateDiffAnnotationComment(ctx context.Context, id, comment string) (CommentUpdateResult, error)
	DeleteDiffAnnotation(ctx context.Context, id string) (int, error)
	ListDiffAnnotationsByTask(ctx context.Context, taskID string) ([]DiffAnnotationRecord, error)
	GetDiffAnnotation(ctx context.Context, id string) (DiffAnnotationRecord, error)

	// CreateDiffReviewSubmission 单事务创建提交并返回完整记录（G1：INSERT...RETURNING
	// seq/created_at，调用方不得在事务提交后再做必需的二次读取——避免写成功但响应失败
	// 致客户端重试产生第二条 submission/重复投递）。
	CreateDiffReviewSubmission(ctx context.Context, in CreateDiffReviewSubmissionInput) (DiffReviewSubmissionRecord, error)
	ListDiffReviewQueue(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error)
	ListDiffReviewHistory(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error)
	ListDiffReviewFailures(ctx context.Context, taskID string) ([]DiffReviewSubmissionRecord, error)
	ListDiffReviewSubmissionItems(ctx context.Context, submissionID string) ([]DiffReviewSubmissionItemRecord, error)
	ListDiffReviewSubmissionPartitions(ctx context.Context, taskID string) (SubmissionPartitions, error)
	GetDiffReviewSubmission(ctx context.Context, id string) (DiffReviewSubmissionRecord, error)

	CASDiffReviewSubmission(ctx context.Context, id, from, to, errorText string) (bool, error)
	CompleteDiffReviewSentCleanup(ctx context.Context, submissionID string) (bool, error)
	CancelDiffReviewSubmission(ctx context.Context, id string) (bool, error)
	DeleteDiffReviewSubmission(ctx context.Context, id string) (bool, error)

	ConvergeDiffReviewOnStartup(ctx context.Context) (int64, error)
}

// PromptPort 表达异步投递 prompt 到任务 agent 会话的能力（design.md D1/D9，adapter 在 task 层）。
//
// taskID 为路由上下文：adapter 经 Manager.taskOcClient(taskID) 获取当前 client+directory，
// 保证多任务投递路由隔离。adapter 获取失败（ok=false）→ PromptOutcome{Kind: pre_send_failure,
// Detail: "runtime client unavailable"}（design.md D1 adapter 获取失败唯一规则）。
type PromptPort interface {
	PromptAsync(ctx context.Context, taskID, sessionID, messageID, text string) PromptOutcome
}

// DiffSource 表达单个 diff 来源的内容读取请求（design.md D9，GitDiff 核心 helper 的能力面）。
// 字段对齐 GitDiffDTO 八字段契约（specs/git-operations「文件 diff 查看」）。
// adapter 在 task 层，经 gitDiffLocked 已持锁核心 helper 实现（禁止递归加锁）。
type DiffSource struct {
	Ref       string
	Path      string
	Untracked bool
}

// DiffSourceResult 为 DiffSourcePort.Read 的返回（GitDiffDTO 八字段投影，domain 拥有）。
// 字段与 application.GitDiffDTO 同形，但为本包独立定义以保持分层边界（service MUST NOT
// 反向依赖 task 包；application.GitDiffDTO 位于 application 包，可被本包引用——此处直接复用）。
// 错误经 err-first 返回（OpError 风格由 adapter 映射；本端口返回 (result, error)）。
type DiffSourceResult struct {
	OldContent string
	NewContent string
	OldExists  bool
	NewExists  bool
	OldMode    string
	NewMode    string
	IsBinary   bool
	Truncated  bool
	// OldTruncated/NewTruncated 为单侧截断标志（F9 stale 逐侧判定用）。
	OldTruncated bool
	NewTruncated bool
}

// DiffSourcePort 表达 diff 来源内容读取能力（design.md D9，adapter 在 task 层）。
// 实现为 Manager.gitDiffLocked 的能力面：调用方 MUST 已持任务锁（adapter 负责锁协调）。
type DiffSourcePort interface {
	// Read 读取单个 diff 来源两侧版本内容。taskID 标识任务（adapter 负责加锁与 task 校验）。
	// 错误映射与 GitDiff 公共入口一致（invalid_input/invalid_state/git_error/internal）。
	Read(ctx context.Context, taskID string, src DiffSource) (DiffSourceResult, error)
	// ReadLocked 在单个任务锁作用域内批量读取多个 diff 来源（F5/D7 组装全程持锁）。
	// adapter 一次 tryLockTask + TaskRow 校验，回调内完成全部来源读取与组装，
	// 每个来源只调核心 helper gitDiffLocked（禁止递归加锁）。
	// fn 为组装回调：对每个来源调用 fn 返回结果，adapter 在锁内逐来源读取并喂给 fn。
	// 任一来源读取失败立即返回首个错误（多源失败返回排序最前，D7）。
	ReadLocked(ctx context.Context, taskID string, srcs []DiffSource, fn DiffReadCallback) error
}

// DiffReadCallback 为 ReadLocked 的组装回调：接收单来源结果，返回是否继续。
// 回调内完成格式化与有界 builder 写入（D7 有界单遍算法），不持锁阻塞其他任务。
type DiffReadCallback func(src DiffSource, result DiffSourceResult, err error) error

// RuntimeSnapshot 为 RuntimePort.Snapshot 的返回：任务 runtime 快照（design.md D9）。
// instVersion 标识当前 runtime 实例（能力缓存绑定键，Suspend/重启/实例替换失效）。
// AnchorSessionID 为任务锚定会话（提交目标会话）；found=false 表示无锚定。
// CapabilityState 为 prompt_async 能力探测缓存（supported/unsupported/unknown/absent）。
type RuntimeSnapshot struct {
	InstVersion      string
	HasRuntime       bool
	AnchorSessionID  string
	HasAnchorSession bool
	CapabilityState  CapabilityState
}

// CapabilityState 表达 prompt_async 能力探测三值 + absent（未探测）（design.md D1/D9）。
type CapabilityState string

const (
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
	CapabilityUnknown     CapabilityState = "unknown"
	CapabilityAbsent      CapabilityState = "absent"
)

// RuntimePort 表达任务 runtime 快照读取能力（design.md D9，adapter 在 task 层）。
//
// 方法集闭合 3.4 能力协调所需：Snapshot（含能力缓存）+ SessionStatus（调度器投递门禁）+
// ProbeCapability（主动探测/复探，singleflight 合并、instVersion fencing、404→GetSession 分流、
// 缓存写回）。adapter 在 task 层经 *opencode.Client 实现。
type RuntimePort interface {
	// Snapshot 返回任务当前 runtime 快照（instVersion/锚定会话/能力缓存）。
	// 任务无运行时（未激活/已 Suspend）→ HasRuntime=false，能力为 absent。
	Snapshot(ctx context.Context, taskID string) (RuntimeSnapshot, error)
	// SessionStatus 返回目标会话的 busy/idle/retry 状态（调度器投递门禁用）。
	// 会话不存在或查询失败 → 返回 error。
	SessionStatus(ctx context.Context, taskID, sessionID string) (SessionStatus, error)
	// ProbeCapability 探测/复探 prompt_async 能力并更新 instVersion-bound 缓存
	//（design.md D1 事件模型②复探、③恢复门禁）。并发请求经 singleflight 合并为一次探测。
	// 任务无运行时 → CapabilityAbsent；探测底层失败 → CapabilityUnknown。
	// 404 分流由 adapter 内部经 GetSession 穷尽判定（design.md D1 错误矩阵 404 行）。
	// 返回探测后的缓存状态（supported/unsupported/unknown；absent 仅无运行时）。
	ProbeCapability(ctx context.Context, taskID string) (CapabilityState, error)
	// InvalidateCapability 将 instVersion-bound 能力缓存置 unknown（design.md D1 事件模型④：
	// PromptAsync 400 或意外 2xx 后置 unknown 触发复探）。
	// 任务无运行时 → no-op。instVersion fencing：仅当当前 instVersion 与 snapshot 一致时失效。
	InvalidateCapability(ctx context.Context, taskID, instVersion string)
	// GetSession 查询目标会话是否存在（design.md D1 404 分流穷尽）。
	// 返回结构化 GetSessionResult：found/missing/unknown（仅 unknown 携带 Detail）。
	// 用于 POST prompt_async 返回 404 后判定「端点不支持」（会话存在）vs「会话缺失」vs「未知」。
	GetSession(ctx context.Context, taskID, sessionID string) (GetSessionResult, error)
	// SetCapabilityUnsupported 将 instVersion-bound 能力缓存直接置 unsupported
	//（design.md D1 404 分流端点不支持分支：能力转 unsupported + 零重投，MUST NOT 复探）。
	// instVersion fencing：仅当当前 instVersion 与 snapshot 一致时写入。
	SetCapabilityUnsupported(ctx context.Context, taskID, instVersion string)
}

// SessionStatus 表达会话运行状态（与 opencode.SessionStatusType 同形，domain 拥有）。
type SessionStatus string

const (
	SessionStatusIdle  SessionStatus = "idle"
	SessionStatusBusy  SessionStatus = "busy"
	SessionStatusRetry SessionStatus = "retry"
)

// GetSessionStatus 表达 GetSession 的结构化结果状态（design.md D1 404 分流穷尽）。
type GetSessionStatus string

const (
	// GetSessionFound 会话存在（GET 200）→ 端点不支持 prompt_async（能力转 unsupported）。
	GetSessionFound GetSessionStatus = "found"
	// GetSessionMissing 会话明确不存在（GET 404）→ failed invalid_state。
	GetSessionMissing GetSessionStatus = "missing"
	// GetSessionUnknown 查询结果未知（其他状态码/网络错误/解码失败）→ failed + capability unknown。
	GetSessionUnknown GetSessionStatus = "unknown"
)

// GetSessionResult 为 RuntimePort.GetSession 的返回（design.md D1 404 分流穷尽三分支）。
type GetSessionResult struct {
	Status GetSessionStatus
	// Detail 仅 GetSessionUnknown 非空（底层错误/状态码摘要，供 error 文案）。
	Detail string
}

// TaskScopeResult 为 TaskScopePort.Lookup 的返回：任务作用域准入结构化结果（design.md D9）。
// diffreview service 自行执行准入校验，错误映射（design.md D9）：
// 任务不存在 → not_found；dir → invalid_input；未知 kind → internal。
type TaskScopeResult struct {
	Found bool
	Kind  string // repo | dir | ""（未找到时）
}

// TaskScopePort 表达任务作用域准入能力（design.md D9，SQLite adapter 在 store 包）。
// 返回结构化结果（非 error），由 service 自行映射错误码。
type TaskScopePort interface {
	// Lookup 查询任务存在性与项目 kind。任务不存在 → Found=false。
	// 底层 store 错误（非 sql.ErrNoRow）→ 返回 error。
	Lookup(ctx context.Context, taskID string) (TaskScopeResult, error)
}

// --- service 骨架（design.md D9） ---
//
// 本阶段为骨架：构造器 + ports 字段 + 编译通过。用例方法（批注/提交/调度/编辑）由 3.4-3.10 填充。

// Service 是 diff review 用例的单一协调器（design.md D9）。
// 持五个 consumer-owned ports 与 FileEditPort（文件编辑读写，可空——文件编辑为独立用例面，
// 部署可不接线；ReadFile/WriteFile 在 fileEdit=nil 时返回 ErrFileEditPortMissing）。
// MUST NOT 反向依赖 task/infrastructure。
type Service struct {
	repo     DiffReviewRepository
	prompt   PromptPort
	diff     DiffSourcePort
	rt       RuntimePort
	scope    TaskScopePort
	fileEdit FileEditPort
}

// Options 构造 Service 的依赖注入。FileEdit 为可空端口（文件编辑用例）。
type Options struct {
	Repo     DiffReviewRepository
	Prompt   PromptPort
	Diff     DiffSourcePort
	Runtime  RuntimePort
	Scope    TaskScopePort
	FileEdit FileEditPort
}

// New 构造 Service。Repo/Prompt/Diff/Runtime/Scope 为构造期合同（与 application/task.LifecycleService 一致）；
// FileEdit 可空（文件编辑用例为独立能力面，部署可不接线，ReadFile/WriteFile 返回 ErrFileEditPortMissing）。
func New(opts Options) *Service {
	return &Service{
		repo:     opts.Repo,
		prompt:   opts.Prompt,
		diff:     opts.Diff,
		rt:       opts.Runtime,
		scope:    opts.Scope,
		fileEdit: opts.FileEdit,
	}
}

// --- err-first 错误（domain 层，供 service 准入校验用） ---
//
// 本包内部流转用 err-first；adapter 层再映射为 code/msg。本阶段先定义准入相关错误，
// 3.5-3.10 用例按需扩展。

// ErrTaskNotFound 任务不存在（准入校验，design.md D9：任务不存在 → not_found）。
var ErrTaskNotFound = errors.New("diffreview: task not found")

// ErrDirProject 任务为 dir 项目（准入校验，design.md D9：dir → invalid_input）。
var ErrDirProject = errors.New("diffreview: project kind is dir (not a git repository)")

// ErrUnknownProjectKind 未知项目 kind（准入校验，design.md D9：未知 kind → internal）。
var ErrUnknownProjectKind = errors.New("diffreview: unknown project kind")

// ErrCapabilityNotReady 能力探测非 supported（准入校验，design.md D2：能力非 supported → invalid_state）。
var ErrCapabilityNotReady = errors.New("diffreview: prompt_async capability not supported")

// ErrTaskNotRunning 任务未运行（无 runtime，准入校验，design.md D2：任务未运行 → invalid_state）。
var ErrTaskNotRunning = errors.New("diffreview: task runtime not active")

// ErrNoAnchorSession 无锚定会话（准入校验，design.md D2：AnchorSessionID 空 → invalid_state）。
var ErrNoAnchorSession = errors.New("diffreview: task has no anchor session")

// ErrEmptySubmission 提交批注列表为空（准入校验，design.md D2/D8：annotations 空 → invalid_input）。
var ErrEmptySubmission = errors.New("diffreview: submission annotations empty")

// ErrDuplicateAnnotationID 提交批次内 id 重复（准入校验，design.md D8：重复 id → invalid_input）。
var ErrDuplicateAnnotationID = errors.New("diffreview: duplicate annotation id in submission")

// ErrCrossTaskAnnotation 提交批次内 id 属于其他任务（准入校验，design.md D2/D8：跨任务 → invalid_input）。
var ErrCrossTaskAnnotation = errors.New("diffreview: annotation belongs to another task")

// ErrPayloadTooLarge 核心区超阈值（准入校验，design.md D7：core+markerSuffix > 65536 → invalid_input）。
var ErrPayloadTooLarge = errors.New("diffreview: payload core exceeds size limit")

// ErrInvalidAnnotationRevision 提交批次内 revision 非法（准入校验，design.md D8：revision 非 1..MaxInt64 → invalid_input）。
var ErrInvalidAnnotationRevision = errors.New("diffreview: invalid annotation revision")
