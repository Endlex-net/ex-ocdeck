// Package application 定义应用层端口与结构化提交结果类型（design.md D0）。
//
// 结果类型为 application commit helper 的输入，表达 store 同值原子 no-op 改造后的
// 四态判定（Matched / Changed / UpdatedAtAdvanced / StatusChanged），不在 domain
// 也不在 sqlite 包定义。Port 接口为 consumer-owned：由 application 层声明所需能力，
// infrastructure/sqlite adapter 实现。
package application

import (
	"errors"
	"fmt"

	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
)

// MutationResult 表达单列或多列写入的结构化结果（design.md D0:88）。
//
// 字段语义：
//   - Matched: WHERE 命中行（id 存在且满足 CAS/同值比较条件）。
//   - Changed: 命中行且业务列发生真实变更（同值命中时 Changed=false）。
//   - UpdatedAtAdvanced: 真实变更且 updated_at 跨秒推进（秒精度，nowUnix）。
//
// 同值原子 no-op：SQL WHERE 排除同值（NULL 安全 IS/IS NOT），仅真实变更才写业务列
// 与 updated_at；updated_at 仅在跨秒时推进（同秒实变不推进，避免秒精度抖动）。
type MutationResult struct {
	Matched           bool
	Changed           bool
	UpdatedAtAdvanced bool
}

// TransitionResult 表达状态写入的结构化结果（design.md D0:92）。
//
// StatusChanged=true 才填 From/To；同值命中（status+last_error 均未变）时
// StatusChanged=false 且 From/To 保持零值。
type TransitionResult struct {
	MutationResult
	StatusChanged bool
	From          ocdecktask.Status
	To            ocdecktask.Status
}

// ClaimResult 表达会话 claim 的结构化结果（design.md D0:96）。
type ClaimResult struct {
	Claimed     bool
	Changed     bool
	OwnerTaskID string
}

// AlignResult 表达单事务会话对齐的结构化结果（design.md D0:98 / 事件目录 sessions.aligned）。
//
// AffectedSessionIDs 为本次对账真实受影响的 session ID（inserted+touched+deleted 的并集），
// 供 commit helper 发布 sessions.aligned（D2：session_ids 为受影响会话 ID，由对账事务统计得出）。
// OwnedSessionIDs 为对齐后本任务拥有的全部 session ID（全量快照，供 application 判定后续动作）。
// Conflicts 为 repo 模式下被他任务拥有的 session ID；TaskMutation 为事务内 notice 分支的结构化结果。
type AlignResult struct {
	Inserted          int
	Touched           int
	Deleted           int
	AffectedSessionIDs []ocdecksess.ID
	OwnedSessionIDs   []ocdecksess.ID
	TaskMutation      MutationResult
	Conflicts         []ocdecksess.ID
}

// DeleteResult 表达任务删除的结构化结果（design D2:337）。
//
// Affected 为删除行数；From 为被删行原状态；CascadedSessionIDs 为同事务删除前捕获的
// 剩余 session ID（级联删除范围）。
type DeleteResult struct {
	Affected           int
	From               ocdecktask.Status
	CascadedSessionIDs []string
}

// NoticeMutation 表达对齐事务内的 notice 决策输入（design.md D0:83）。
//
// 由 application 层经 Task domain 决策后传入；sqlite adapter 在同一事务内读取当前
// notice、比较 expected、写入 new。expected 不匹配即整事务回滚并返回 AlignConflict。
// nil 表示 NULL/空 notice（与存储层 sql.NullString{Valid:false} 对应）。
type NoticeMutation struct {
	Expected *string
	New      *string
}

// AlignConflict 表达 Align 事务内 notice expected 失配的 typed conflict（design.md D0:80/86）。
//
// expected 与事务内最新 notice 不匹配时 MUST 回滚整个 Align 事务（不提交任何 session 行变更）
// 并返回此错误；application MUST 重读 Task、重新经 Task domain 决策 notice、有界重试
// （沿用既有 notice CAS 重读重试语义，上限 8 次）。
type AlignConflict struct {
	TaskID  string
	Expected *string
	Actual   *string
}

// Error 实现 error。
func (e *AlignConflict) Error() string {
	exp, act := "<null>", "<null>"
	if e.Expected != nil {
		exp = *e.Expected
	}
	if e.Actual != nil {
		act = *e.Actual
	}
	return fmt.Sprintf("align: notice expected mismatch (task %s): expected=%s actual=%s", e.TaskID, exp, act)
}

// IsAlignConflict 判断 err 是否为 *AlignConflict（供 application 重试决策）。
func IsAlignConflict(err error) bool {
	var c *AlignConflict
	return errors.As(err, &c)
}