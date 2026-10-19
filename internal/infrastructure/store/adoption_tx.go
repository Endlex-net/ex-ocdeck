// adoption_tx.go 实现 v2 承接的原子提交（opencode-v2-provider tasks 6.4 /
// design D5 步骤 5）：单事务内完成——①归属行命名空间一致性守卫（混杂拒绝）；
// ②新键三列谓词冲突检查（被他任务持有 → 拒绝）；③task_sessions 命名空间
// v1→v2 转换（含 parent_id 链子 session，session_id 不变）；④anchor 三元组
// 更新（anchor_session_id 不变，守卫原 v1 命名空间 + 与 binding 的
// UpstreamSessionID 一致）；⑤binding envelope 写入 + 状态守卫 CAS
// （suspended→active / active→active 守卫，均不经 activating；无同值排除——
// 守卫语义以行存在 + status 匹配为据）。任一步失败整体回滚零写入。
//
// MUST NOT 复用既有 upsert 的 ON CONFLICT 假定（转换不走 upsert；直接 UPDATE
// 命名空间列）。CAS 不复用 updateTaskStatus（其同值排除谓词会拒绝 active→active
// 守卫——本事务的 UPDATE 本身改变 binding/anchor 列，无需同值排除）。
package store

import (
	"context"
	"errors"
	"fmt"

	"ocdeck/internal/application"
	domain "ocdeck/internal/domain/agentprovider"
	ocdecktask "ocdeck/internal/domain/task"
)

// ErrAdoptionConflict 标记承接原子转换的拒绝类失败（来源不一致/新键被他任务
// 持有/命名空间混杂/状态守卫失配）——调用方按冲突重试语义或停止承接处置，
// 事务已整体回滚零写入。
var ErrAdoptionConflict = errors.New("store: adoption commit rejected")

// CommitAdoption 承接原子提交。binding 必须已是 v2 形状（ProviderID=opencode-v2、
// UpstreamSessionID=锚定原 session id）；fromStatus ∈ {suspended, active}
// （suspended→active 手动激活 / active→active 守卫——状态不变，仅防并发漂移）。
// lastError 随 CAS 一并写入（承接失败的如实告知位；nil 不改写）。
func (q *Queries) CommitAdoption(ctx context.Context, taskID string, binding domain.TaskBinding,
	fromStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	if binding.ProviderID != ProviderIDV2 || binding.ProviderInstanceID != ProviderInstanceV2Default {
		return application.TransitionResult{}, fmt.Errorf("%w: binding %s/%s 不属于 v2 命名空间",
			ErrAdoptionConflict, binding.ProviderID, binding.ProviderInstanceID)
	}
	if fromStatus != ocdecktask.StatusSuspended && fromStatus != ocdecktask.StatusActive {
		return application.TransitionResult{}, fmt.Errorf("%w: 承接 CAS fromStatus %s 非法（仅 suspended/active）",
			ErrAdoptionConflict, fromStatus)
	}
	raw, err := binding.Encode()
	if err != nil {
		return application.TransitionResult{}, fmt.Errorf("store: adoption commit: encode binding: %w", err)
	}
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		return qx.commitAdoptionInTx(ctx, taskID, binding, raw, fromStatus, lastError)
	})
}

func (q *Queries) commitAdoptionInTx(ctx context.Context, taskID string, binding domain.TaskBinding,
	raw string, fromStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	// ① 归属行命名空间一致性：任务存在 task_sessions 行时必须全部为 v1 命名
	// 空间（混杂/已转换 → 拒绝——D5 判定矩阵「其余」出口；重复承接被 R1 恢复
	// 路径承担，不走本事务）。
	var foreign int
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_sessions
		 WHERE task_id = ? AND (provider_id != ? OR provider_instance_id != ?)`,
		taskID, ProviderIDV1, ProviderInstanceV1Default).Scan(&foreign); err != nil {
		return application.TransitionResult{}, err
	}
	if foreign > 0 {
		return application.TransitionResult{}, fmt.Errorf("%w: task %s 存在 %d 行非 v1 命名空间归属（混杂拒绝）",
			ErrAdoptionConflict, taskID, foreign)
	}
	// ② 新键三列谓词冲突检查：任一行（v1 命名空间）的 session_id 已被他任务
	// 以 v2 命名空间持有 → 拒绝。
	var conflicts int
	if err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_sessions s
		 WHERE s.task_id = ? AND s.provider_id = ? AND s.provider_instance_id = ?
		   AND EXISTS (SELECT 1 FROM task_sessions o
		               WHERE o.session_id = s.session_id
		                 AND o.provider_id = ? AND o.provider_instance_id = ?
		                 AND o.task_id != ?)`,
		taskID, ProviderIDV1, ProviderInstanceV1Default,
		ProviderIDV2, ProviderInstanceV2Default, taskID).Scan(&conflicts); err != nil {
		return application.TransitionResult{}, err
	}
	if conflicts > 0 {
		return application.TransitionResult{}, fmt.Errorf("%w: task %s 有 %d 行新键被他任务持有（冲突拒绝）",
			ErrAdoptionConflict, taskID, conflicts)
	}
	// ③ 命名空间转换（全行含子 session；session_id 不变）。
	if _, err := q.db.ExecContext(ctx,
		`UPDATE task_sessions SET provider_id = ?, provider_instance_id = ?
		 WHERE task_id = ? AND provider_id = ? AND provider_instance_id = ?`,
		ProviderIDV2, ProviderInstanceV2Default, taskID, ProviderIDV1, ProviderInstanceV1Default); err != nil {
		return application.TransitionResult{}, err
	}
	// ④⑤ binding 写入 + anchor 三元组更新 + 状态守卫 CAS（单条 UPDATE：
	// 守卫谓词 = id + status IS fromStatus + anchor 与 binding 的 upstream 一致
	// 且 anchor 命名空间为 v1——来源一致性（D5 判定矩阵）在事务内复验）。
	newLE := nullableString(lastError)
	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET
		   task_binding = ?,
		   anchor_provider_id = ?,
		   anchor_provider_instance_id = ?,
		   status = CASE WHEN ? = 'suspended' THEN 'active' ELSE status END,
		   last_error = ?,
		   updated_at = ?
		 WHERE id = ? AND status IS ? AND task_binding IS NOT NULL
		   AND anchor_session_id IS ?
		   AND anchor_provider_id IS ? AND anchor_provider_instance_id IS ?`,
		raw, ProviderIDV2, ProviderInstanceV2Default, string(fromStatus), newLE, nowUnix(),
		taskID, string(fromStatus), binding.UpstreamSessionID,
		ProviderIDV1, ProviderInstanceV1Default)
	if err != nil {
		return application.TransitionResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return application.TransitionResult{}, err
	}
	if n == 0 {
		return application.TransitionResult{}, fmt.Errorf("%w: task %s 状态守卫失配或来源不一致（期望 status=%s、binding 已存在、anchor=%s@v1）",
			ErrAdoptionConflict, taskID, fromStatus, binding.UpstreamSessionID)
	}
	return application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true},
		StatusChanged:  fromStatus != ocdecktask.StatusActive,
		From:           fromStatus,
		To:             ocdecktask.StatusActive,
	}, nil
}

// IsAdoptionConflict 判定 err 是否承接原子转换的拒绝类失败（errors.Is 语义）。
func IsAdoptionConflict(err error) bool { return errors.Is(err, ErrAdoptionConflict) }

// CommitV2Resume 是 v2 恢复/替换提交原语（gate3 G3-F2：已有 v2 binding 的恢复
// 提交——binding 替换 + 旧 binding 代际守卫 + 状态收敛（fromStatus→active）单
// 事务，不走 activating 首次语义、无同值排除（active→active 守卫合法）。
// D4 生命周期矩阵：已有 v2 binding 的激活/恢复提交 = activating→active CAS
//（激活/恢复入口先经 beginActivation 置 activating；from=active 守卫时状态不变）。
// 仅适用于已是 v2 命名空间的 binding。
//
// G3-F8：oldBinding 为本次恢复实际读取到的旧 binding（同一次读取的事实，调用方
// 不得二次读取）——其 Encode 产物作为代际守卫精确匹配列（`AND task_binding = ?`），
// 杜绝「恢复 A 读旧 A → 恢复 B 先提交 B → A 把 B 覆盖回 A」的代际倒置。CAS 失配
// → ErrAdoptionConflict（调用方按冲突重试语义处置，回收本次候选连接）。首次安装
// （R5 无既有 binding）传零值 binding → 守卫退化为仅状态 CAS。
func (q *Queries) CommitV2Resume(ctx context.Context, taskID string, oldBinding, binding domain.TaskBinding,
	fromStatus ocdecktask.Status) (application.TransitionResult, error) {
	if binding.ProviderID != ProviderIDV2 || binding.ProviderInstanceID != ProviderInstanceV2Default {
		return application.TransitionResult{}, fmt.Errorf("%w: binding %s/%s 不属于 v2 命名空间",
			ErrAdoptionConflict, binding.ProviderID, binding.ProviderInstanceID)
	}
	raw, err := binding.Encode()
	if err != nil {
		return application.TransitionResult{}, fmt.Errorf("store: commit v2 resume: encode binding: %w", err)
	}
	var oldRaw string
	if oldBinding.ProviderID != "" || oldBinding.UpstreamSessionID != "" {
		// 既有 binding：以本次读取事实的编码产物为代际守卫（Encode 确定性——
		// 列存储写入侧同源）。
		or, oerr := oldBinding.Encode()
		if oerr != nil {
			return application.TransitionResult{}, fmt.Errorf("store: commit v2 resume: encode old binding: %w", oerr)
		}
		oldRaw = or
	}
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		// 守卫谓词：id + fromStatus + （有既有 binding 时）task_binding 精确匹配；
		// 命中即 binding 替换与 active 收敛同事务落库。
		qry := `UPDATE tasks SET task_binding = ?, status = 'active', updated_at = ?
			 WHERE id = ? AND status IS ?`
		args := []any{raw, nowUnix(), taskID, string(fromStatus)}
		if oldRaw != "" {
			qry += ` AND task_binding IS ?`
			args = append(args, oldRaw)
		}
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			return application.TransitionResult{}, fmt.Errorf("%w: task %s 守卫失配（期望 status=%s、旧 binding 代际一致）",
				ErrAdoptionConflict, taskID, fromStatus)
		}
		return application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true},
			StatusChanged:  fromStatus != ocdecktask.StatusActive,
			From:           fromStatus,
			To:             ocdecktask.StatusActive,
		}, nil
	})
}
