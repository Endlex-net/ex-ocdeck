// sessions.go 实现 Session claim/touch/delete/align 用例与 Attention apply 提交位
//（design.md D0:141 迁移第 5 步）。
//
// 统一形态：
//  1. 决策先于副作用：claim/touch/delete 的领域判定由 Repository 事务原子完成并返回
//     结构化事实（ClaimResult/MutationResult/affected）；align 的 notice 变更先经
//     Task domain 决策（typed notice 规则）算出 NoticeMutation，再由 adapter 同事务提交。
//  2. 提交成功后经集中 commit helper 调用非阻塞 Publisher（本阶段 NoopPublisher，
//     调用位就绪无实际发布；真实事件生产在 P1.6）。
//
// Align 事务边界（design.md D0:86 + canonical opencode-orchestration spec）：
//   - overflow（complete=false）：session_overflow notice 在 Align 之前经事务外 CAS 写入
//     （失败返回错误且不执行 Align；Align 失败不回滚该 notice）；
//   - complete：notice 清除随 Align 单事务提交，expected 失配整事务回滚返回 AlignConflict，
//     由本层重读 Task、重新经 domain 决策后有界重试（8 次上限，沿用 notice CAS 语义）。
package task

import (
	"context"
	"fmt"
	"log"

	"ocdeck/internal/application"
	ocdeckevent "ocdeck/internal/domain/event"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
)

// AlignPorts 聚合 align 用例所需的窄端口（session Align + notice CAS + 任务行读取）。
//
// 以窄接口为参数使 Legacy facade（未注入 LifecycleService 的测试路径）能以 TaskStore
// 支撑的适配器共用同一编排，避免 align 编排逻辑双写。LifecycleService 经 svcAlignPorts
// 满足本接口。
type AlignPorts interface {
	Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error)
	UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error)
	GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error)
}

// svcAlignPorts 把 LifecycleService 持有的端口聚合为 AlignPorts。
type svcAlignPorts struct {
	s *LifecycleService
}

func (p svcAlignPorts) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	return p.s.sessions.Align(ctx, taskID, mode, observed, complete, notice)
}

// UpdateTaskNoticeCAS 经 LifecycleService.UpdateNoticeCAS 提交：overflow notice CAS
// 真实变更（Changed && UpdatedAtAdvanced）发布 task.activity_changed（P1.6.2）。
// 事务外 CAS 先于 Align 提交与发布，Align 失败不回滚（design.md D0:86）。
func (p svcAlignPorts) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	return p.s.UpdateNoticeCAS(ctx, id, expected, newNotice)
}

func (p svcAlignPorts) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	return p.s.read.GetTaskRow(ctx, id)
}

// alignNoticeCASLimit 为 notice CAS（overflow 写入与 Align 冲突重试）的有界重试上限，
// 沿用既有 notice CAS 重读重试语义（notice.go 8 次上限）。
const alignNoticeCASLimit = 8

// ClaimSession 认领会话归属（design.md D0:77）。
//
// Repository 单事务先查他主再 upsert，返回 ClaimResult（Claimed/Changed/OwnerTaskID）；
// 仅 Changed=true 经 commit helper 发布 session.claimed（本阶段 NoopPublisher）。
func (s *LifecycleService) ClaimSession(ctx context.Context, taskID string, obs ocdecksess.Observation) (application.ClaimResult, error) {
	res, err := s.sessions.Claim(ctx, taskID, obs)
	if err != nil {
		return application.ClaimResult{}, err
	}
	s.commitSessionClaim(taskID, obs.ID, res)
	return res, nil
}

// TouchOwnedSession 推进本任务已归属会话的最近活跃时间（design.md D0:78）。
//
// 绝不创建归属（Matched=false 为未命中正常路径）；仅 Changed=true 发布 session.touched。
func (s *LifecycleService) TouchOwnedSession(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (application.MutationResult, error) {
	res, err := s.sessions.TouchOwned(ctx, taskID, sessionID, lastSeenAt)
	if err != nil {
		return application.MutationResult{}, err
	}
	s.commitSessionTouch(taskID, sessionID, res)
	return res, nil
}

// DeleteOwnedSession 删除会话归属行（design.md D0:79）。
//
// 返回受影响行数；仅 affected>0 发布 session.deleted。
func (s *LifecycleService) DeleteOwnedSession(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error) {
	affected, err := s.sessions.DeleteOwned(ctx, taskID, sessionID)
	if err != nil {
		return 0, err
	}
	s.commitSessionDelete(taskID, sessionID, affected)
	return affected, nil
}

// OwnerOf 反查会话归属（status/diff 事件 fail-closed 反查用，design.md D0:82）。
// 读到历史重复归属返回 session.AmbiguousOwnerError。
func (s *LifecycleService) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	return s.sessions.OwnerOf(ctx, sessionID)
}

// AlignSessions 全量对齐用例（design.md D0:80/86）。
//
// 编排经 RunAlign（与 legacy facade 共用）：overflow 前置 CAS + complete notice 决策 +
// AlignConflict 有界重试；提交成功后经 commitSessionsAligned 发布（本阶段 NoopPublisher）。
func (s *LifecycleService) AlignSessions(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool) (application.AlignResult, error) {
	res, err := RunAlign(ctx, svcAlignPorts{s}, taskID, mode, observed, complete)
	if err != nil {
		return application.AlignResult{}, err
	}
	s.commitSessionsAligned(taskID, res)
	return res, nil
}

// RunAlign 编排单次全量对齐（供 LifecycleService 与迁移期 legacy facade 共用，避免双写）。
//
// 事务边界（design.md D0:86）：
//   - complete=false（overflow）：先经事务外 CAS 写入 session_overflow notice（domain 决策
//     AddSessionOverflowNotice 幂等追加），CAS 失败返回错误且不执行 Align；Align 失败不回滚
//     该 notice（两者为独立提交）。
//   - complete=true：notice 清除决策（domain ClearSessionOverflowNotice）经 NoticeMutation
//     随 Align 单事务提交；expected 失配返回 AlignConflict → 重读 Task 重新决策后有界重试。
func RunAlign(ctx context.Context, p AlignPorts, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool) (application.AlignResult, error) {
	if !complete {
		if err := recordSessionOverflowNotice(ctx, p, taskID); err != nil {
			return application.AlignResult{}, fmt.Errorf("session overflow notice: %w", err)
		}
	}
	for attempt := 0; attempt < alignNoticeCASLimit; attempt++ {
		mut, err := decideAlignNotice(ctx, p, taskID, complete)
		if err != nil {
			return application.AlignResult{}, err
		}
		res, aerr := p.Align(ctx, taskID, mode, observed, complete, mut)
		if aerr == nil {
			// repo/dir 对齐路径冲突 → 忽略 + 记诊断日志（不阻断，D8）。
			for _, sid := range res.Conflicts {
				log.Printf("task %s: align session %s conflict (owned by other task); skipping", taskID, sid)
			}
			return res, nil
		}
		if application.IsAlignConflict(aerr) {
			// notice expected 失配：重读 Task、重新经 domain 决策后重试。
			continue
		}
		return application.AlignResult{}, aerr
	}
	return application.AlignResult{}, fmt.Errorf("align sessions: notice CAS did not converge (task %s)", taskID)
}

// recordSessionOverflowNotice 经事务外 CAS 写入 session_overflow notice（overflow 前置，
// design.md D0:86）。domain AddSessionOverflowNotice 幂等决策；CAS 8 次有界重试。
// 错误文本对齐 legacy recordSessionOverflowNotice（Manager alignSessions 聚合语义）。
func recordSessionOverflowNotice(ctx context.Context, p AlignPorts, taskID string) error {
	for attempt := 0; attempt < alignNoticeCASLimit; attempt++ {
		snap, err := p.GetTaskRow(ctx, taskID)
		if err != nil {
			return fmt.Errorf("record session overflow notice: get task: %w", err)
		}
		notices, perr := ocdecktask.ParseNoticesJSON(noticeRawString(snap.Notice))
		if perr != nil {
			// JSON 损坏 MUST fail-closed，不得当空数组覆盖（design.md §8）。
			return fmt.Errorf("record session overflow notice: notice json corrupted (task %s): %w", taskID, perr)
		}
		t := ocdecktask.Rehydrate(ocdecktask.GuardView{Notices: notices})
		if !t.AddSessionOverflowNotice() {
			// 已存在：幂等成功，无需写入。
			return nil
		}
		res, cerr := p.UpdateTaskNoticeCAS(ctx, taskID, snap.Notice, ocdecktask.EncodeNoticesJSON(t.Notices()))
		if cerr != nil {
			return fmt.Errorf("record session overflow notice: %w", cerr)
		}
		if res.Matched {
			// Matched 含 Changed 与同值幂等两种收敛形态，均视为已落库。
			return nil
		}
		// CAS 失配：并发 notice 写入，重读重试。
	}
	return fmt.Errorf("record session overflow notice: CAS did not converge (task %s)", taskID)
}

// decideAlignNotice 计算 Align 事务内的 notice 决策输入（design.md D0:83）。
//
// complete=true：读 Task 全行 → domain ClearSessionOverflowNotice → NoticeMutation
// {Expected=读取的当前 notice, New=清除后的编码}。notice JSON 损坏时返回同值 no-op 决策
//（对齐 legacy noticeFn parse 失败保持当前值不动的语义）。complete=false 返回零值
//（Align 不触碰 notice，overflow 已由事务外 CAS 写入）。
func decideAlignNotice(ctx context.Context, p AlignPorts, taskID string, complete bool) (application.NoticeMutation, error) {
	if !complete {
		return application.NoticeMutation{}, nil
	}
	snap, err := p.GetTaskRow(ctx, taskID)
	if err != nil {
		return application.NoticeMutation{}, err
	}
	notices, perr := ocdecktask.ParseNoticesJSON(noticeRawString(snap.Notice))
	if perr != nil {
		return application.NoticeMutation{Expected: snap.Notice, New: snap.Notice}, nil
	}
	t := ocdecktask.Rehydrate(ocdecktask.GuardView{Notices: notices})
	t.ClearSessionOverflowNotice()
	return application.NoticeMutation{Expected: snap.Notice, New: ocdecktask.EncodeNoticesJSON(t.Notices())}, nil
}

// noticeRawString 把 TaskSnapshot.Notice（*string）还原为原始字符串（nil → 空）。
func noticeRawString(n *string) string {
	if n == nil {
		return ""
	}
	return *n
}

// CommitAttentionChange 提交外部可见注意力快照变化（design.md D2 attention 行）。
//
// 两个独立 accepted apply（接管归并 / REST 写回）各自在 apply 前后快照 diff 判定 changed，
// changed 时调用本方法发布 serve_runtime.attention_changed（本阶段 NoopPublisher，
// 无实际发布）。RID 为 ServeRuntime 主键 instVersion，Payload 携带 task_id。
func (s *LifecycleService) CommitAttentionChange(taskID, instVersion string) {
	s.publish.Publish(ocdeckevent.NewServeRuntimeAttentionChanged(instVersion, taskID))
}

// CommitRunStatusChange 提交 agentStatus 聚合/可用性真实变化（design.md D2 agent 状态
// 变更行）。调用方持 runtime 唯一 apply 返回的 typed delta（锁内捕获），解锁后直接用
// delta 组装事件、MUST NOT 重读 runtime。RID 为 ServeRuntime 主键 instVersion，Payload
// 携带 task_id/from/to/available（from/to 为聚合三态或 "" 表不可用）。
func (s *LifecycleService) CommitRunStatusChange(taskID, instVersion, from, to string, available bool) {
	s.publish.Publish(ocdeckevent.NewServeRuntimeRunStatusChanged(instVersion, taskID, from, to, available))
}

// --- commit helper（design.md D0:133，NoopPublisher 阶段调用位就绪） ---

// commitSessionClaim 提交 claim 结果：仅真实变更（新插入或 last_seen_at/parent_id 推进）
// 发布 session.claimed（design.md D2 session claim 行）；冲突持有不发布。
func (s *LifecycleService) commitSessionClaim(taskID string, sessionID ocdecksess.ID, res application.ClaimResult) {
	if !res.Changed {
		return
	}
	s.publish.Publish(ocdeckevent.NewSessionClaimed(string(sessionID), taskID))
}

// commitSessionTouch 提交 touch 结果：仅值真实推进发布 session.touched（design.md D2）。
func (s *LifecycleService) commitSessionTouch(taskID string, sessionID ocdecksess.ID, res application.MutationResult) {
	if !res.Changed {
		return
	}
	s.publish.Publish(ocdeckevent.NewSessionTouched(string(sessionID), taskID))
}

// commitSessionDelete 提交 delete 结果：仅 affected>0 发布 session.deleted（design.md D2）。
func (s *LifecycleService) commitSessionDelete(taskID string, sessionID ocdecksess.ID, affected int) {
	if affected <= 0 {
		return
	}
	s.publish.Publish(ocdeckevent.NewSessionDeleted(string(sessionID), taskID))
}

// commitSessionsAligned 提交 align 结果：session 行计数（inserted+touched+deleted）总和 >0
// 发布一次 sessions.aligned；事务内 notice 真实推进另按 updated_at 规则发布
// task.activity_changed（design.md D2 align 行；本阶段 NoopPublisher）。
func (s *LifecycleService) commitSessionsAligned(taskID string, res application.AlignResult) {
	if res.Inserted+res.Touched+res.Deleted > 0 {
		ids := make([]string, 0, len(res.AffectedSessionIDs))
		for _, sid := range res.AffectedSessionIDs {
			ids = append(ids, string(sid))
		}
		s.publish.Publish(ocdeckevent.NewSessionsAligned(taskID, res.Inserted, res.Touched, res.Deleted, ids))
	}
	if res.TaskMutation.Changed && res.TaskMutation.UpdatedAtAdvanced {
		s.publish.Publish(ocdeckevent.NewTaskActivityChanged(taskID))
	}
}
