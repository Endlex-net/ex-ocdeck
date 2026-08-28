// dto.go 定义 API 契约侧的任务/会话/注意力/Git DTO 与状态常量（design.md D5/§21）。
//
// sse-active-sessions P1.9a：定义自 internal/task 迁移至此，锁定 import 方向
// api → application（design.md D0:55）；internal/task 保留同名类型/常量别名，
// 既有引用零改动。字段、JSON tag、常量值与迁移前逐字一致（零 wire/DTO/行为变更）。
package application

import (
	"database/sql"

	"ocdeck/internal/infrastructure/opencode"
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

// TaskRow tasks 表行映射（解耦 store 包结构，design.md §18）。
// BaseRef 为 repo 任务的基线分支全引用，dir 项目任务为空串（add-plain-dir-project D10）。
type TaskRow struct {
	ID           string
	ProjectID    string
	Name         string
	Branch       string
	Status       string
	WorktreePath string
	LastPort     sql.NullInt64
	LastError    sql.NullString
	Notice       sql.NullString
	DeleteMode   sql.NullString
	EnvSnapshot  sql.NullString
	CreatedAt    int64
	UpdatedAt    int64
	ArchivedAt   sql.NullInt64
	InitStatus      string
	InitError       sql.NullString
	BaseRef         string
	AnchorSessionID sql.NullString
}

// SessionRow 会话归属行（解耦 store 包结构，design.md §18）。
type SessionRow struct {
	TaskID           string
	SessionID        string
	SessionCreatedAt int64
	FirstSeenAt      int64
	LastSeenAt       int64
	// ParentID 非空表示 background subagent 子会话；空为顶层会话（design.md §4 锚定隔离）。
	ParentID string
}

// ActiveTaskOverviewRow 跨项目 active 任务概览投影行（cross-project-active-sessions D1/D2）。
// 仅供 GET /api/v1/tasks/active 读模型：字段与 store.ActiveTaskOverviewRow 一一对应，
// 不携带 agentStatus（由 API 层组装读内存快照填充到 DTO，sse-active-sessions P2.2）。
type ActiveTaskOverviewRow struct {
	ID           string
	ProjectID    string
	ProjectName  string
	Name         string
	Branch       string
	WorktreePath string
	LastActiveAt int64
}

// --- 注意力信号三层类型模型（含 Since，本地首次观察时间） ---

// PendingPermission task 层 pending 权限请求。Since 为本地首次观察 Unix 秒
// （SSE asked 到达时刻；REST 对账同 ID 保留原 since，新 ID 取对账时刻，design.md D6）。
type PendingPermission struct {
	opencode.PermissionRequest
	Since int64
}

// PendingQuestion task 层 pending 问题请求。Since 同 PendingPermission。
type PendingQuestion struct {
	opencode.QuestionRequest
	Since int64
}

// Attention 是任务注意力信号的只读快照（design.md D6 API 透出）。
// 拷贝语义：attentionSnapshot 返回深拷贝，调用方可安全持有。
// 无 pending 时为非 nil 空切片（空数组非 null，spec）。
type Attention struct {
	Permissions []PendingPermission
	Questions   []PendingQuestion
}

// ProjectTaskSummary 项目任务摘要（design.md D4：10 存储字段 + attention_count，
// GET /projects tasks 摘要）。
type ProjectTaskSummary struct {
	TaskID         string
	Name           string
	ProjectID      string
	Status         string
	InitStatus     string
	Branch         string
	WorktreePath   string
	LastError      string
	Notice         string
	UpdatedAt      int64
	AttentionCount int
}

// GitFileDTO 单文件状态（design.md §21 git/status，与 internal/api/git.go gitFileDTO 字段一致）。
type GitFileDTO struct {
	Path      string `json:"path"`
	X         string `json:"x"`
	Y         string `json:"y"`
	Staged    bool   `json:"staged"`
	Unstaged  bool   `json:"unstaged"`
	Untracked bool   `json:"untracked"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	IsBinary  bool   `json:"isBinary"`
}

// GitStatusDTO status 响应（含当前分支，design.md §21 git/status）。
type GitStatusDTO struct {
	Branch string       `json:"branch"`
	Files  []GitFileDTO `json:"files"`
}

// GitDiffDTO diff 响应（单文件两侧版本内容，design.md §21 git/diff、codemirror-git-diff design D1）。
// 八字段 MUST 始终全部返回：oldContent/newContent 为两侧内容（UTF-8 文本契约，不承诺字节级
// round-trip），任一侧不存在（oldExists/newExists=false）时该侧内容与 mode 为空串；
// oldMode/newMode 为该侧 git 八进制 mode 文本（100644/100755/120000/160000，取自 ref/index
// 探测记录或工作区类型/权限位）；isBinary=任一侧二进制（前 8000 字节含 NUL，mode 为
// 120000/160000 的侧不参与嗅探），置位时两侧内容为空；truncated=任一侧内容大小超 512KB 的
// 截断标记，MUST NOT 兼任二进制含义。派生规则以 git-operations spec「文件 diff 查看」为唯一来源。
type GitDiffDTO struct {
	OldContent string `json:"oldContent"`
	NewContent string `json:"newContent"`
	OldExists  bool   `json:"oldExists"`
	NewExists  bool   `json:"newExists"`
	OldMode    string `json:"oldMode"`
	NewMode    string `json:"newMode"`
	IsBinary   bool   `json:"isBinary"`
	Truncated  bool   `json:"truncated"`
}
