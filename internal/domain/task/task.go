// Package task 实现 Task 聚合根：status/init_status 状态机 guard、DeleteMode、
// typed Notice 集合，以及 `task.New(...)` 构造不变量。
//
// 本包只依赖 stdlib。它不执行 IO、不持有 context、不发布事件、不持有持久化细节。
// guard 与构造器只表达领域规则；具体提交与编排责任在 application 层。
//
// 反模式约束（design D0）：不使用泛型 Repository[T]/BaseEntity/反射 mapper/Factory 类/
// Specification 模式；构造不变量用普通构造函数 `task.New(...)` 保证。
package task

import (
	"errors"
	"fmt"
	"time"
)

// Status 为 tasks.status 域枚举（九值，逐字对齐 internal/task/types.go:37-45）。
type Status string

const (
	StatusSuspended      Status = "suspended"
	StatusActive         Status = "active"
	StatusArchived       Status = "archived"
	StatusCreating       Status = "creating"
	StatusCreationFailed Status = "creation_failed"
	StatusActivating     Status = "activating"
	StatusSuspending     Status = "suspending"
	StatusDeleting       Status = "deleting"
	StatusDeletionFailed Status = "deletion_failed"
)

// InitStatus 为 init_status 域枚举（五值，逐字对齐 internal/task/types.go:51-57）。
type InitStatus string

const (
	InitStatusNone      InitStatus = "none"
	InitStatusPending   InitStatus = "pending"
	InitStatusRunning   InitStatus = "running"
	InitStatusSucceeded InitStatus = "succeeded"
	InitStatusFailed    InitStatus = "failed"
)

// DeleteMode 删除模式（design §19，逐字对齐 internal/task/types.go:28-30）。
type DeleteMode string

const (
	DeleteModeNormal DeleteMode = "normal"
	DeleteModeForce  DeleteMode = "force"
)

// NoticeCode 为 typed Notice 的分类码（对齐 internal/task/notice.go:29-32 的 code 枚举）。
type NoticeCode string

const (
	// NoticeCodeResidualProcesses 残留进程通知（cleanup 失败记录）。
	NoticeCodeResidualProcesses NoticeCode = "residual_processes"
	// NoticeCodeSessionOverflow 会话对齐达到上限，缺席行未清理。
	NoticeCodeSessionOverflow NoticeCode = "session_overflow"
)

// Notice 为 Task 的 typed notice 项。Notice 在 domain 层是类型化集合组件，
// 其增删决策在 domain；Kill/RetryReap 等编排不在 domain。
//
// 字段语义对齐 internal/task/notice.go:noticeEntry：
//   - Code 分类码（NoticeCodeResidualProcesses / NoticeCodeSessionOverflow）
//   - Message 人类可读描述
//   - TS Unix 秒
//   - Data 仅对 residual_processes 出现，含 sessionName/cleanupTickets/reason/retryable
//
// retryable 必须在 Data 内（不在顶层）；sessionOverflow 无 Data。
type Notice struct {
	Code    NoticeCode
	Message string
	TS      int64
	Data    NoticeData
}

// NoticeData 为 Notice 的载荷。仅 residual_processes 使用 Residual 字段。
// session_overflow 不使用任何字段（语义：单一幂等标记，不携带明细）。
type NoticeData struct {
	// SessionName 仅 residual_processes 使用。
	SessionName string
	// CleanupTickets 仅 residual_processes 使用，逃逸进程 tickets 集合。
	CleanupTickets []string
	// Reason 仅 residual_processes 使用，为 notice reason 枚举原值（snapshot_failed 等）。
	Reason string
	// Retryable 仅 residual_processes 使用。
	Retryable bool
}

// ResidualReason 枚举（对齐 internal/task/notice.go:37-41 的 reason 枚举原值）。
const (
	ResidualReasonSnapshotFailed       = "snapshot_failed"
	ResidualReasonKillFailed            = "kill_failed"
	ResidualReasonReapFailed            = "reap_failed"
	ResidualReasonSnapshotMissingDegraded = "snapshot_missing_degraded"
)

// Task 为聚合根：完整 status/init_status 状态机 guard、delete intent、typed notice 集合、
// 创建期不可变信息。
//
// 不包含 []Session、ServeRuntime、tmux/opencode handle、env 合并算法。
// updated_at/last_port 为持久化元数据，domain 对象只读暴露，不由 Task 方法任意修改。
type Task struct {
	id          string
	projectID   string
	name        string
	branch      string
	worktreePath string
	baseRef     string
	status      Status
	initStatus  InitStatus
	deleteMode  DeleteMode
	notices     []Notice
	lastError   string
	initError   string
	createdAt   int64
	updatedAt   int64
}

// NewInput 为 `task.New` 的输入（仅创建期可变信息；状态由构造函数固定为 creating）。
type NewInput struct {
	ID           string
	ProjectID    string
	Name         string
	Branch       string
	WorktreePath string
	BaseRef      string
	// CreatedAt 可选；0 表示由调用方在持久化时填入。
	CreatedAt int64
}

// New 构造一个处于 creating 状态的 Task（design D0 状态机矩阵第一行：—→creating）。
// 调用方保证前置检查已通过（项目存在、名称/分支/baseRef 合法）。
//
// 不变式：
//   - ID/ProjectID/Name/WorktreePath 非空
//   - status=creating
//   - initStatus=none（init 配置在 application 层决定是否落 pending；domain 默认 none）
//   - notices 为空集合
//   - deleteMode 为空串（未发起删除意图）
func New(in NewInput) (*Task, error) {
	if in.ID == "" {
		return nil, errors.New("task.New: ID must not be empty")
	}
	if in.ProjectID == "" {
		return nil, errors.New("task.New: ProjectID must not be empty")
	}
	if in.Name == "" {
		return nil, errors.New("task.New: Name must not be empty")
	}
	if in.WorktreePath == "" {
		return nil, errors.New("task.New: WorktreePath must not be empty")
	}
		return &Task{
		id:           in.ID,
		projectID:    in.ProjectID,
		name:         in.Name,
		branch:       in.Branch,
		worktreePath: in.WorktreePath,
		baseRef:      in.BaseRef,
		status:       StatusCreating,
		initStatus:   InitStatusNone,
		createdAt:    in.CreatedAt,
		updatedAt:    in.CreatedAt,
	}, nil
}

// GuardView 为持久化行重建 guard 视图所需的最小字段子集（design D0 P1.4.2）。
// 仅包含 status/init_status/delete_mode/notices——guard 判断输入；其余创建期可变信息
// （name/branch/worktreePath/baseRef）与持久化元数据（lastError/initError/timestamps）
// 不参与 guard 决策，故不在重建输入中。调用方按需填充：notice 维度已在上游判断完成时
// 传 nil；delete_mode 仅在 guard 需要读持久化 mode 时填充（CanDelete 的 mode 由参数传入，
// 不读此字段）。
type GuardView struct {
	Status     Status
	InitStatus InitStatus
	DeleteMode DeleteMode
	Notices    []Notice
}

// Rehydrate 从持久化行值构造 Task 的 guard 视图（design D0 P1.4.2）。
//
// 这是持久化重建的合法形态：直接按行值构造，不走 New 的创建不变量（New 仅用于首次创建，
// 固定 status=creating）。Rehydrate 供 application/legacy facade 调用 domain guard
// （CanArchive/CanRestore/CanDelete/CanActivate/CanSuspend）使用，纯 stdlib、无 IO、
// 不校验业务不变量。
//
// 与 New 的区别：New 校验创建期必填字段并固定 status=creating；Rehydrate 接受任意
// 持久化值（含 creation_failed/deletion_failed 等终态），仅用于读取 guard 判定，
// 不用于写入或状态迁移。
func Rehydrate(v GuardView) *Task {
	t := &Task{
		status:     v.Status,
		initStatus: v.InitStatus,
		deleteMode: v.DeleteMode,
	}
	if v.Notices != nil {
		t.notices = make([]Notice, len(v.Notices))
		copy(t.notices, v.Notices)
	}
	return t
}

// --- 只读访问器 ---

func (t *Task) ID() string           { return t.id }
func (t *Task) ProjectID() string    { return t.projectID }
func (t *Task) Name() string         { return t.name }
func (t *Task) Branch() string       { return t.branch }
func (t *Task) WorktreePath() string { return t.worktreePath }
func (t *Task) BaseRef() string      { return t.baseRef }
func (t *Task) Status() Status       { return t.status }
func (t *Task) InitStatus() InitStatus { return t.initStatus }
func (t *Task) DeleteMode() DeleteMode { return t.deleteMode }
func (t *Task) LastError() string    { return t.lastError }
func (t *Task) InitError() string    { return t.initError }
func (t *Task) CreatedAt() int64     { return t.createdAt }
func (t *Task) UpdatedAt() int64     { return t.updatedAt }

// Notices 返回 notice 集合的拷贝（防止外部突变）。顺序与内部一致。
func (t *Task) Notices() []Notice {
	if len(t.notices) == 0 {
		return nil
	}
	out := make([]Notice, len(t.notices))
	copy(out, t.notices)
	return out
}

// HasRetryableResidual 判断是否存在任意 retryable=true 的 residual_processes notice。
// 用于 Activate 门禁（design D0：CanActivate 的无阻断 notice 维度）。
func (t *Task) HasRetryableResidual() bool {
	for _, n := range t.notices {
		if n.Code == NoticeCodeResidualProcesses && n.Data.Retryable {
			return true
		}
	}
	return false
}

// HasSessionOverflow 判断是否存在 session_overflow notice（幂等标记）。
func (t *Task) HasSessionOverflow() bool {
	for _, n := range t.notices {
		if n.Code == NoticeCodeSessionOverflow {
			return true
		}
	}
	return false
}

// --- 状态机 guard（design D0 `tasks.status` 状态机矩阵逐行对应） ---

// InitInProgress 判断 init 是否进行中（init_status ∈ {pending, running}）。
// 归档/删除门禁共享此判定（design §19/§3.7）。
func (t *Task) InitInProgress() bool {
	return t.initStatus == InitStatusPending || t.initStatus == InitStatusRunning
}

// CanActivate 判断是否允许 suspended→activating 的迁移。
// guard 维度（design D0 matrix 行 suspended→activating + canonical task-lifecycle spec 五分支）：
//   - status=suspended
//   - 无阻断 notice（无 retryable residual_processes）
//   - init_status 门禁五分支：
//     none|succeeded → 放行；pending|running → 拒绝（init 进行中）；
//     failed → 拒绝（需 Re-run）；未知值 → fail-closed 拒绝
func (t *Task) CanActivate() bool {
	if t.status != StatusSuspended {
		return false
	}
	if t.HasRetryableResidual() {
		return false
	}
	switch t.initStatus {
	case InitStatusNone, InitStatusSucceeded:
		return true
	case InitStatusPending, InitStatusRunning, InitStatusFailed:
		return false
	default:
		// 未知/空值 fail-closed。
		return false
	}
}

// CanSuspend 判断是否允许 active→suspending 的迁移（design D0 matrix 行 active→suspending）。
func (t *Task) CanSuspend() bool {
	return t.status == StatusActive
}

// CanArchive 判断是否允许 suspended→archived 的迁移。
// guard 维度（design D0 matrix 行 suspended→archived + crud.go:515-521）：
//   - status=suspended
//   - init_status ∉ {pending, running}（init 进行中拒绝归档）
func (t *Task) CanArchive() bool {
	if t.status != StatusSuspended {
		return false
	}
	if t.InitInProgress() {
		return false
	}
	return true
}

// CanRestore 判断是否允许 archived→suspended 的迁移（design D0 matrix 行 archived→suspended）。
func (t *Task) CanRestore() bool {
	return t.status == StatusArchived
}

// CanDelete 判断是否允许进入 deleting 状态（BeginDeleteIntent）。
// guard 维度（design D0 matrix 行 suspended|archived|creation_failed|deletion_failed → deleting
// + delete.go:46-49/654-662）：
//   - Normal 模式仅允许 suspended|archived|creation_failed
//   - Force 模式额外允许 deletion_failed
//   - 两者均拒绝 init 进行中（init_status ∈ {pending, running}）
//   - 未知 mode 或未知 status fail-closed 拒绝
func (t *Task) CanDelete(mode DeleteMode) bool {
	if t.InitInProgress() {
		return false
	}
	if mode != DeleteModeNormal && mode != DeleteModeForce {
		return false
	}
	switch t.status {
	case StatusSuspended, StatusArchived, StatusCreationFailed:
		return true
	case StatusDeletionFailed:
		// Force 额外允许 deletion_failed；Normal 拒绝（deletion_failed 必须经 Retry 按持久化 force mode 重入）。
		return mode == DeleteModeForce
	default:
		return false
	}
}

// --- 状态机应用方法（仅迁移，不做 IO/CAS；application 负责 CAS 提交） ---
//
// 这些方法表达「在 guard 通过的前提下，将状态迁移到目标值」。
// 调用方（application）必须先调用对应 Can* guard，再调用 Apply*。
// 迁移失败（guard 不通过）返回 err-first 的 typed error，供 application 映射为 invalid_state/conflict。

// TransitionError 表示状态迁移被 guard 拒绝，供 application 映射为 invalid_state。
type TransitionError struct {
	From Status
	To   Status
	Msg  string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("task: transition %s→%s rejected: %s", e.From, e.To, e.Msg)
}

// applyStatus 迁移 status；guard 不通过返回 *TransitionError。
// 调用方应使用具名 Apply* 方法，不直接调用本方法。
func (t *Task) applyStatus(to Status, guard bool, msg string) error {
	if !guard {
		return &TransitionError{From: t.status, To: to, Msg: msg}
	}
	t.status = to
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyActivate 迁移 suspended→activating。guard=CanActivate。
func (t *Task) ApplyActivate() error {
	return t.applyStatus(StatusActivating, t.CanActivate(), "activate requires suspended + no blocking notice + init_status gate")
}

// ApplyActivateCommit 迁移 activating→active（design D0 matrix 行 activating→active，编排内部提交，无额外 guard）。
func (t *Task) ApplyActivateCommit() error {
	if t.status != StatusActivating {
		return &TransitionError{From: t.status, To: StatusActive, Msg: "activate commit requires activating"}
	}
	t.status = StatusActive
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyActivateCompensate 迁移 activating→suspended（补偿路径，design D0 matrix 行 activating→suspended）。
func (t *Task) ApplyActivateCompensate() error {
	if t.status != StatusActivating {
		return &TransitionError{From: t.status, To: StatusSuspended, Msg: "activate compensation requires activating"}
	}
	t.status = StatusSuspended
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyRecoveryStart 迁移 active→activating（D3 恢复前序）。
// 只表达任务领域 guard（status=active）；runtime token 校验留在 Manager。
func (t *Task) ApplyRecoveryStart() error {
	return t.applyStatus(StatusActivating, t.status == StatusActive, "recovery start requires active")
}

// ApplyRecoveryCommit 迁移 activating→active（D3 恢复成功提交，与 ApplyActivateCommit 同矩阵行）。
func (t *Task) ApplyRecoveryCommit() error {
	return t.ApplyActivateCommit()
}

// ApplyRecoveryFailure 迁移 activating→suspended（D3 终态补偿，与 ApplyActivateCompensate 同矩阵行）。
func (t *Task) ApplyRecoveryFailure() error {
	return t.ApplyActivateCompensate()
}

// ApplySuspend 迁移 active→suspending。guard=CanSuspend。
func (t *Task) ApplySuspend() error {
	return t.applyStatus(StatusSuspending, t.CanSuspend(), "suspend requires active")
}

// ApplySuspendRepair 迁移 suspending→active（Suspend 修复回迁，design D0 matrix 行 suspending→active）。
func (t *Task) ApplySuspendRepair() error {
	if t.status != StatusSuspending {
		return &TransitionError{From: t.status, To: StatusActive, Msg: "suspend repair requires suspending"}
	}
	t.status = StatusActive
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplySuspendComplete 迁移 suspending→suspended（Suspend 完成，design D0 matrix 行 suspending→suspended）。
func (t *Task) ApplySuspendComplete() error {
	if t.status != StatusSuspending {
		return &TransitionError{From: t.status, To: StatusSuspended, Msg: "suspend complete requires suspending"}
	}
	t.status = StatusSuspended
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyConvergeSuspended 迁移 active→suspended（异常收敛 CAS / reconcile kill 分支，
// design D0 matrix 行 active→suspended）。仅持锁主路径使用；调用方负责清理先于提交。
func (t *Task) ApplyConvergeSuspended() error {
	if t.status != StatusActive {
		return &TransitionError{From: t.status, To: StatusSuspended, Msg: "converge requires active"}
	}
	t.status = StatusSuspended
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyArchive 迁移 suspended→archived。guard=CanArchive。
func (t *Task) ApplyArchive() error {
	return t.applyStatus(StatusArchived, t.CanArchive(), "archive requires suspended + init not in progress")
}

// ApplyRestore 迁移 archived→suspended。guard=CanRestore。
func (t *Task) ApplyRestore() error {
	return t.applyStatus(StatusSuspended, t.CanRestore(), "restore requires archived")
}

// ApplyCommitCreated 迁移 creating|creation_failed→suspended（CommitCreated 提交点，
// design D0 matrix 行 creating→suspended / creation_failed→suspended）。
// 允许的 from：creating、creation_failed（Retry 的 CommitCreated）。
func (t *Task) ApplyCommitCreated(initStatus InitStatus) error {
	if t.status != StatusCreating && t.status != StatusCreationFailed {
		return &TransitionError{From: t.status, To: StatusSuspended, Msg: "commit created requires creating|creation_failed"}
	}
	if err := validateInitStatusForCommit(initStatus); err != nil {
		return err
	}
	t.status = StatusSuspended
	t.initStatus = initStatus
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyCreationFailed 迁移任意状态→creation_failed（补偿性落账总是允许，
// design D0 matrix 行 creating→creation_failed / 任意并发状态→creation_failed）。
// lastError 可为空串表示不更新；非空表示覆盖。
func (t *Task) ApplyCreationFailed(from Status, lastError string) {
	// 补偿性落账总是允许：domain 不校验 from 是否匹配当前 status（并发可能已变化），
	// application 在 CAS 未命中路径保留并发状态不调用本方法。
	t.status = StatusCreationFailed
	if lastError != "" {
		t.lastError = lastError
	}
	t.updatedAt = time.Now().Unix()
}

// ApplyBeginDeleteIntent 迁移到 deleting 并记录 deleteMode（BeginDeleteIntent，
// design D0 matrix 行 suspended|archived|creation_failed|deletion_failed → deleting）。
// guard=CanDelete(mode)。调用方在通过后持久化 delete_mode + status + updated_at。
func (t *Task) ApplyBeginDeleteIntent(mode DeleteMode) error {
	if !t.CanDelete(mode) {
		return &TransitionError{From: t.status, To: StatusDeleting, Msg: "delete intent guard rejected (mode=" + string(mode) + ")"}
	}
	t.status = StatusDeleting
	t.deleteMode = mode
	t.updatedAt = time.Now().Unix()
	return nil
}

// ApplyDeletionFailed 迁移 deleting→deletion_failed（删除副作用失败落账，
// design D0 matrix 行 deleting→deletion_failed，补偿性落账总是允许）。
func (t *Task) ApplyDeletionFailed(lastError string) {
	t.status = StatusDeletionFailed
	if lastError != "" {
		t.lastError = lastError
	}
	t.updatedAt = time.Now().Unix()
}

// ApplyRetryReenterDelete 迁移 deletion_failed→deleting（Retry 重入删除，
// design D0 matrix 行 deletion_failed→deleting）。deleteMode 保持已持久化的 force。
func (t *Task) ApplyRetryReenterDelete() error {
	if t.status != StatusDeletionFailed {
		return &TransitionError{From: t.status, To: StatusDeleting, Msg: "retry reenter delete requires deletion_failed"}
	}
	t.status = StatusDeleting
	t.updatedAt = time.Now().Unix()
	return nil
}

// --- init_status 状态机（design D0 init_status 状态机段） ---
//
// init_status 改写 MUST 走同值原子 no-op 与结构化结果（application 侧）；domain 表达合法流转 guard。
// 本变更 MUST NOT 为 init_status 发明领域事件（design D0）。

// ApplyCommitInitPending 在 CommitCreated 路径将 init_status 落为 pending（配置了 init 脚本）。
// 仅在 status 为 creating|creation_failed 时合法（与 ApplyCommitCreated 同事务）。
func (t *Task) ApplyCommitInitPending() error {
	if t.status != StatusCreating && t.status != StatusCreationFailed {
		return &TransitionError{From: t.status, To: t.status, Msg: "init pending requires creating|creation_failed"}
	}
	if t.initStatus != InitStatusNone {
		return &TransitionError{From: t.status, To: t.status, Msg: "init pending requires init_status=none"}
	}
	t.initStatus = InitStatusPending
	return nil
}

// ApplyClaimInitRun 迁移 init_status pending→running（ClaimInitRun，design D0 init_status 状态机段）。
func (t *Task) ApplyClaimInitRun() error {
	if t.initStatus != InitStatusPending {
		return &TransitionError{From: t.status, To: t.status, Msg: "claim init run requires init_status=pending"}
	}
	t.initStatus = InitStatusRunning
	return nil
}

// ApplyFinishInitRun 迁移 init_status running→succeeded|failed（FinishInitRun）。
// failed 时记录 initError。
func (t *Task) ApplyFinishInitRun(succeeded bool, initError string) error {
	if t.initStatus != InitStatusRunning {
		return &TransitionError{From: t.status, To: t.status, Msg: "finish init run requires init_status=running"}
	}
	if succeeded {
		t.initStatus = InitStatusSucceeded
		t.initError = ""
	} else {
		t.initStatus = InitStatusFailed
		t.initError = initError
	}
	return nil
}

// ApplyClaimInitRerun 迁移 init_status failed|succeeded→running（ClaimInitRerun，
// design D0 init_status 状态机段，要求 status=suspended）。
func (t *Task) ApplyClaimInitRerun() error {
	if t.status != StatusSuspended {
		return &TransitionError{From: t.status, To: t.status, Msg: "claim init rerun requires status=suspended"}
	}
	if t.initStatus != InitStatusFailed && t.initStatus != InitStatusSucceeded {
		return &TransitionError{From: t.status, To: t.status, Msg: "claim init rerun requires init_status=failed|succeeded"}
	}
	t.initStatus = InitStatusRunning
	return nil
}

// ApplyConvergeInterruptedInit 迁移 init_status pending|running→failed
// （ConvergeInterruptedInitRuns，design D0 init_status 状态机段，启动收敛）。
func (t *Task) ApplyConvergeInterruptedInit(initError string) error {
	if t.initStatus != InitStatusPending && t.initStatus != InitStatusRunning {
		return &TransitionError{From: t.status, To: t.status, Msg: "converge interrupted init requires init_status=pending|running"}
	}
	t.initStatus = InitStatusFailed
	t.initError = initError
	return nil
}

// validateInitStatusForCommit 校验 CommitCreated 时设置的 init_status 仅允许 none|pending
// （配置 init 脚本落 pending，否则 none）。
func validateInitStatusForCommit(s InitStatus) error {
	switch s {
	case InitStatusNone, InitStatusPending:
		return nil
	default:
		return &TransitionError{Msg: "commit created init_status must be none|pending, got " + string(s)}
	}
}

// --- typed Notice 增删决策（domain 负责；Kill/RetryReap 编排不在 domain） ---

// noticeIdentity 为 notice 项的去重身份（对齐 internal/task/notice.go:518-521：code + sessionName）。
// residual_processes 按 sessionName 去重；session_overflow 无 sessionName，按 code 唯一。
func noticeIdentity(n Notice) string {
	if n.Code == NoticeCodeResidualProcesses {
		return string(n.Code) + "|" + n.Data.SessionName
	}
	return string(n.Code)
}

// AddResidualNotice 增/替换一条 residual_processes notice。
// 按 sessionName 去重：已存在同 sessionName 项则替换（tickets union 合并、reason/retryable 以新为准）。
// 返回的 added bool 表示是否真实新增（用于 application 判定是否产生 updated_at 推进）。
func (t *Task) AddResidualNotice(entry Notice) (added bool) {
	if entry.Code != NoticeCodeResidualProcesses {
		// AddResidualNotice 仅处理 residual_processes；其他 code 由专门方法处理。
		return false
	}
	for i, n := range t.notices {
		if n.Code == NoticeCodeResidualProcesses && n.Data.SessionName == entry.Data.SessionName {
			// 替换：tickets union 合并（旧 + 新去重，旧在前新在后），reason/retryable 以新为准。
			merged := unionTickets(n.Data.CleanupTickets, entry.Data.CleanupTickets)
			entry.Data.CleanupTickets = merged
			t.notices[i] = entry
			return false
		}
	}
	t.notices = append(t.notices, entry)
	return true
}

// AddSessionOverflowNotice 增加一条 session_overflow notice（幂等标记）。
// 已存在则不重复追加。返回 added bool 表示是否真实新增。
func (t *Task) AddSessionOverflowNotice() bool {
	for _, n := range t.notices {
		if n.Code == NoticeCodeSessionOverflow {
			return false
		}
	}
	t.notices = append(t.notices, Notice{
		Code:    NoticeCodeSessionOverflow,
		Message: "session alignment reached limit; absent rows not pruned",
		TS:      time.Now().Unix(),
	})
	return true
}

// ClearResidualNotice 按 sessionName 清除一条 residual_processes notice。
// 返回 cleared bool 表示是否真实清除（用于 application 判定）。
func (t *Task) ClearResidualNotice(sessionName string) bool {
	for i, n := range t.notices {
		if n.Code == NoticeCodeResidualProcesses && n.Data.SessionName == sessionName {
			t.notices = append(t.notices[:i], t.notices[i+1:]...)
			return true
		}
	}
	return false
}

// ClearSessionOverflowNotice 清除 session_overflow notice。返回是否真实清除。
func (t *Task) ClearSessionOverflowNotice() bool {
	for i, n := range t.notices {
		if n.Code == NoticeCodeSessionOverflow {
			t.notices = append(t.notices[:i], t.notices[i+1:]...)
			return true
		}
	}
	return false
}

// SetNotices 用外部读回的 notice 集合替换内部集合（application 从持久化重建 Task 时使用）。
// 传入 nil 等价于清空。调用方负责保持项的身份与字段语义。
func (t *Task) SetNotices(notices []Notice) {
	if notices == nil {
		t.notices = nil
		return
	}
	t.notices = make([]Notice, len(notices))
	copy(t.notices, notices)
}

// unionTickets 合并两批 tickets 去重（保序，旧在前新在后）。
// 对齐 internal/task/notice.go:unionStringSlices 语义。
func unionTickets(old, neu []string) []string {
	seen := make(map[string]struct{}, len(old)+len(neu))
	out := make([]string, 0, len(old)+len(neu))
	for _, tk := range old {
		if _, ok := seen[tk]; ok {
			continue
		}
		seen[tk] = struct{}{}
		out = append(out, tk)
	}
	for _, tk := range neu {
		if _, ok := seen[tk]; ok {
			continue
		}
		seen[tk] = struct{}{}
		out = append(out, tk)
	}
	return out
}