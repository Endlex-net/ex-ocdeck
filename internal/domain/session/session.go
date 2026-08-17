// Package session 实现 Session 独立聚合：领域 ID=session_id、Ownership（owner task）、
// claim/touch/delete 规则。
//
// 本包只依赖 stdlib。不依赖持久化细节（task_sessions 表复合主键、Claim 事务）。
//
// session_id 唯一性口径（design D0）：
//   - 领域层声明 session_id 全局唯一
//   - 物理层无全局唯一索引，跨 task 唯一由 Claim 事务保证（先查其他 owner 再 upsert）
//   - 读到历史重复归属时 fail-closed（OwnerOf 返回 typed ambiguity）
//
// Session 不包含 opencode session 内容、run status、Task 指针。
// Kill/RetryReap 等编排不在 domain。
package session

import (
	"errors"
	"fmt"
	"time"
)

// ID 为 Session 的领域 ID（即 opencode session_id，全局唯一）。
type ID string

// Session 为独立聚合根。
//
// 身份（主键）= ID（session_id）。
// Ownership 字段表达归属：OwnerTaskID 为 owning task 主键，空串表示未归属。
//
// 不包含 opencode session 内容、run status、Task 指针。
type Session struct {
	id          ID
	ownerTaskID string
	parentID    string
	createdAt   int64
	firstSeenAt int64
	lastSeenAt  int64
}

// NewInput 为 session.New 的输入。
type NewInput struct {
	ID          ID
	OwnerTaskID string
	ParentID    string
	// CreatedAt opencode 侧创建时间（Unix 秒）。
	CreatedAt int64
	// FirstSeenAt ocdeck 首次观测时间（Unix 秒），可等于 CreatedAt。
	FirstSeenAt int64
	// LastSeenAt opencode 侧最近活动时间（Unix 秒）。
	LastSeenAt int64
}

// New 构造一个归属明确的 Session。
//
// 不变式（design D0 Session 行 + session_id 唯一性口径）：
//   - ID 非空
//   - OwnerTaskID 非空（Session 须有归属，未归属的 session 不进入 domain）
//   - LastSeenAt >= FirstSeenAt（最近活动不早于首次观测；同值合法）
func New(in NewInput) (*Session, error) {
	if in.ID == "" {
		return nil, errors.New("session.New: ID must not be empty")
	}
	if in.OwnerTaskID == "" {
		return nil, errors.New("session.New: OwnerTaskID must not be empty")
	}
	if in.LastSeenAt < in.FirstSeenAt {
		return nil, fmt.Errorf("session.New: LastSeenAt (%d) < FirstSeenAt (%d)", in.LastSeenAt, in.FirstSeenAt)
	}
	return &Session{
		id:          in.ID,
		ownerTaskID: in.OwnerTaskID,
		parentID:    in.ParentID,
		createdAt:   in.CreatedAt,
		firstSeenAt: in.FirstSeenAt,
		lastSeenAt:  in.LastSeenAt,
	}, nil
}

// --- 只读访问器 ---

func (s *Session) ID() ID           { return s.id }
func (s *Session) OwnerTaskID() string { return s.ownerTaskID }
func (s *Session) ParentID() string  { return s.parentID }
func (s *Session) CreatedAt() int64  { return s.createdAt }
func (s *Session) FirstSeenAt() int64 { return s.firstSeenAt }
func (s *Session) LastSeenAt() int64  { return s.lastSeenAt }

// --- claim/touch/delete 规则（domain 表达合法性，不做 IO/事务） ---
//
// Claim 事务（先查他主再 upsert）由 SessionRepository 在 application/infrastructure 层完成；
// domain 只表达「在已有归属的前提下，claim 是否合法」与「touch 是否推进」的领域判定。

// ClaimDecision 为 Claim 在 domain 层的判定结果，供 application 编排。
//
// 注意：他主冲突（当前归属属于其他 task）的 fail-closed 由 Repository 事务保证，
// domain 不模拟并发；这里表达的是「已知当前归属时，本次 claim 是否构成变更」。
type ClaimDecision struct {
	// Changed 表示本次 claim 是否真实变更（新插入或 last_seen_at/parent_id 推进）。
	Changed bool
	// ConflictOwner 非空表示当前归属属于其他 task（domain 层判定），
	// application 收到后 MUST 不发布 session.claimed、由 Repository 事务保证不覆盖。
	ConflictOwner string
}

// ClaimBy 在 domain 层判定「taskID 认领本 session」的合法性，并返回 ClaimDecision。
//
// 规则（design D0 Session 行 + session_id 唯一性口径）：
//   - 若 OwnerTaskID 为空（不应发生，Session 须有归属）：返回 Changed=true，归属写入 taskID。
//   - 若 OwnerTaskID == taskID（幂等 upsert）：
//     - lastSeenAt/parentID 推进 → Changed=true
//     - 全列同值 → Changed=false（幂等无变化，不发布 session.claimed）
//   - 若 OwnerTaskID != taskID（他主持有）→ ConflictOwner=OwnerTaskID，Changed=false，
//     application MUST 不覆盖（Repository 事务 fail-closed）。
func (s *Session) ClaimBy(taskID string, lastSeenAt int64, parentID string) ClaimDecision {
	if taskID == "" {
		return ClaimDecision{}
	}
	if s.ownerTaskID == "" {
		// 不应发生（Session 须有归属）；视为新归属。
		return ClaimDecision{Changed: true}
	}
	if s.ownerTaskID != taskID {
		return ClaimDecision{ConflictOwner: s.ownerTaskID}
	}
	// 幂等 upsert：仅当 last_seen_at 推进或 parent_id 变化才视为真实变更。
	if lastSeenAt > s.lastSeenAt || parentID != s.parentID {
		return ClaimDecision{Changed: true}
	}
	return ClaimDecision{Changed: false}
}

// ApplyClaim 在 domain 层将 claim 结果落到 Session 上。
// 调用方必须先经 ClaimBy 判定，且 Changed=true 且 ConflictOwner 为空时才调用本方法。
//
// parentID 无条件写入（含从非空清空为空），与持久层 ClaimTaskSession 的
// `parent_id = excluded.parent_id` 无条件覆盖语义一致。
//
// 注意：本方法不重新校验他主冲突（domain 不模拟并发）；Repository 事务负责 fail-closed。
func (s *Session) ApplyClaim(taskID string, lastSeenAt int64, parentID string) {
	s.ownerTaskID = taskID
	s.parentID = parentID
	if lastSeenAt > s.lastSeenAt {
		s.lastSeenAt = lastSeenAt
	}
}

// TouchDecision 为 Touch 在 domain 层的判定结果。
type TouchDecision struct {
	// Changed 表示 last_seen_at 是否真实推进。
	Changed bool
	// NotOwned 表示 taskID 不是当前 owner（domain 判定，application 不发布）。
	NotOwned bool
}

// TouchBy 在 domain 层判定「taskID 推进本 session 最近活跃时间」的合法性。
//
// 规则（design D0 session touch 提交点）：
//   - taskID != OwnerTaskID → NotOwned=true，Changed=false（不发布）
//   - lastSeenAt <= 当前 lastSeenAt → Changed=false（值不变不发布）
//   - lastSeenAt > 当前 lastSeenAt → Changed=true
func (s *Session) TouchBy(taskID string, lastSeenAt int64) TouchDecision {
	if taskID != s.ownerTaskID {
		return TouchDecision{NotOwned: true}
	}
	if lastSeenAt <= s.lastSeenAt {
		return TouchDecision{}
	}
	return TouchDecision{Changed: true}
}

// ApplyTouch 在 domain 层将 touch 结果落到 Session 上。
// 调用方必须先经 TouchBy 判定，且 Changed=true 时才调用。
func (s *Session) ApplyTouch(lastSeenAt int64) {
	if lastSeenAt > s.lastSeenAt {
		s.lastSeenAt = lastSeenAt
	}
}

// DeleteOwnership 表达「taskID 移除本 session 归属」的 domain 判定。
//
// 规则（design D0 session delete 提交点）：
//   - taskID == OwnerTaskID → affected=1（真实删除归属行）
//   - taskID != OwnerTaskID → affected=0（未命中归属，不发布 session.deleted）
//
// 返回 affected int；application 仅在 affected>0 时发布。
// domain 不删除 Session 对象本身（归属移除后对象不再有意义，由 Repository 删行）。
func (s *Session) DeleteOwnership(taskID string) int {
	if taskID == s.ownerTaskID && s.ownerTaskID != "" {
		return 1
	}
	return 0
}

// --- 历史重复归属 fail-closed ---
//
// OwnerOf 的歧义判定在 application/Repository 层（需要查询物理行）；
// domain 提供 typed ambiguity error 供 Repository 返回，避免裸 error。

// AmbiguousOwnerError 表示读到同一 session_id 归属多个 task 的历史脏数据。
// design D0：读到历史重复归属时 fail-closed，对应 status/diff 事件不 apply、不发布。
type AmbiguousOwnerError struct {
	SessionID ID
	Owners    []string
}

func (e *AmbiguousOwnerError) Error() string {
	return fmt.Sprintf("session: ambiguous owner for %s: %v", e.SessionID, e.Owners)
}

// NewAmbiguousOwnerError 构造历史重复归属的 typed error。
func NewAmbiguousOwnerError(id ID, owners []string) *AmbiguousOwnerError {
	return &AmbiguousOwnerError{SessionID: id, Owners: owners}
}

// --- 辅助：构造观测用的 Session（application 从 opencode DTO 转换） ---

// Observation 为持久化中立的会话观测（application 从 opencode DTO 转换后传入 domain）。
// 仅含归属写回所需字段，不含 opencode session 内容。
type Observation struct {
	ID         ID
	ParentID   string
	CreatedAt  int64
	UpdatedAt  int64
}

// FromObservation 在 align 路径从观测构造归属明确的新 Session（待 Claim 事务写入）。
// firstSeenAt/lastSeenAt 都用观测的 UpdatedAt（opencode 侧最近活动时间）。
func FromObservation(obs Observation, ownerTaskID string, firstSeenAt int64) (*Session, error) {
	return New(NewInput{
		ID:          obs.ID,
		OwnerTaskID: ownerTaskID,
		ParentID:    obs.ParentID,
		CreatedAt:   obs.CreatedAt,
		FirstSeenAt: firstSeenAt,
		LastSeenAt:  obs.UpdatedAt,
	})
}

// nowUnix 仅用于内部默认值（避免直接调 time.Now 散落）。
func nowUnix() int64 { return time.Now().Unix() }