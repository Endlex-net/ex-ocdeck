// session_key_queries.go 实现 SessionKey 命名空间与 TaskBinding 列的类型化读写
// （opencode-v1-spi-provider tasks 3.5 / design D4/D3）：原子 claim 按完整
// （provider_id, provider_instance_id, session_id）三列谓词执行、BEGIN IMMEDIATE
// 事务；task_binding 版本化 envelope 解码未知版本 fail-closed。
//
// 既有 ClaimTaskSession/ClaimTaskSessionAndSetAnchor（task 层接缝，签名不变）
// 内部迁移为同一实现按 v1 常量命名空间执行——单命名空间下行为等价
// （0018 之后全部行携带统一命名空间列）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ocdeck/internal/application"
	domain "ocdeck/internal/domain/agentprovider"
	ocdecktask "ocdeck/internal/domain/task"
)

// v1 命名空间常量（design D2；与 provider_namespace.go 补齐目标一致）。
const (
	nsProviderIDV1              = ProviderIDV1
	nsProviderInstanceV1Default = ProviderInstanceV1Default
)

// runImmediateTx 在 BEGIN IMMEDIATE 事务中执行 fn（design D4：claim 先取写锁，
// 避免 deferred BEGIN 的锁升级窗口）。BEGIN IMMEDIATE 由连接 DSN 的
// _txlock=immediate 提供（store.Open，对全部 BeginTx 生效；单连接下连接级串行
// 不变，database/sql 连接池机制保证并发 BeginTx 互斥等待）；已在 *sql.Tx 内时
// 沿用外层事务语义。
func runImmediateTx[T any](ctx context.Context, q *Queries, fn func(qtx *Queries) (T, error)) (T, error) {
	return runTx(ctx, q, fn)
}

// claimSessionKeyInTx 按 D4 算法在已绑定事务内执行原子 claim：
//  1. 完整三列谓词查是否被他任务持有 → 冲突不写入（Claimed=false + OwnerTaskID）；
//  2. 未被持有 → 插入/幂等更新本任务行（保留旧主键 ON CONFLICT(task_id, session_id)，
//     Changed=新插入或 last_seen_at/parent_id 实际推进，与既有语义一致）。
func (q *Queries) claimSessionKeyInTx(ctx context.Context, taskID string, key domain.SessionKey, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	// 完整三列谓词冲突检查（D4 第 2 步）。
	row := q.db.QueryRowContext(ctx,
		`SELECT task_id FROM task_sessions
		  WHERE session_id = ? AND provider_id = ? AND provider_instance_id = ? AND task_id != ?`,
		key.UpstreamID, key.ProviderID, key.ProviderInstanceID, taskID)
	var owner string
	err := row.Scan(&owner)
	if err == nil {
		return application.ClaimResult{Claimed: false, OwnerTaskID: owner}, nil
	}
	if err != sql.ErrNoRows {
		return application.ClaimResult{}, err
	}
	// changed 判定（镜像既有语义）。
	orow := q.db.QueryRowContext(ctx,
		`SELECT last_seen_at, parent_id FROM task_sessions WHERE task_id = ? AND session_id = ?`,
		taskID, key.UpstreamID)
	var curLast int64
	var curParent sql.NullString
	changed := true
	if oerr := orow.Scan(&curLast, &curParent); oerr == sql.ErrNoRows {
		changed = true // 新插入。
	} else if oerr != nil {
		return application.ClaimResult{}, oerr
	} else {
		changed = lastSeen > curLast || parentID != nullStringValue(curParent)
	}
	// upsert 本任务行：命名空间列随行写入（同任务同 session 的行身份在
	// 单命名空间下与旧主键语义等价）。
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id, provider_id, provider_instance_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id, session_id) DO UPDATE SET
		   last_seen_at = MAX(excluded.last_seen_at, task_sessions.last_seen_at),
		   parent_id = excluded.parent_id`,
		taskID, key.UpstreamID, createdAt, firstSeen, lastSeen, parentID,
		key.ProviderID, key.ProviderInstanceID)
	if err != nil {
		return application.ClaimResult{}, err
	}
	return application.ClaimResult{Claimed: true, Changed: changed}, nil
}

// ClaimSessionKey 按 D4 原子 claim 一个 SessionKey 至本任务（BEGIN IMMEDIATE +
// 三列谓词）。不加跨任务唯一索引；冲突语义由调用方按入口处理。
func (q *Queries) ClaimSessionKey(ctx context.Context, taskID string, key domain.SessionKey, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	if key.IsZero() {
		return application.ClaimResult{}, errors.New("store: claim: SessionKey 不完整")
	}
	return runImmediateTx(ctx, q, func(qtx *Queries) (application.ClaimResult, error) {
		return qtx.claimSessionKeyInTx(ctx, taskID, key, createdAt, firstSeen, lastSeen, parentID)
	})
}

// ClaimSessionKeyAndSetAnchor 单事务 claim + 写入锚定三元组
// （anchor_provider_id/anchor_provider_instance_id/anchor_session_id，D4）。
// claim 成功后 key 立即成为权威锚定；冲突时归属与锚定均不修改。
func (q *Queries) ClaimSessionKeyAndSetAnchor(ctx context.Context, taskID string, key domain.SessionKey, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	if key.IsZero() {
		return application.ClaimResult{}, errors.New("store: claim+anchor: SessionKey 不完整")
	}
	return runImmediateTx(ctx, q, func(qtx *Queries) (application.ClaimResult, error) {
		res, err := qtx.claimSessionKeyInTx(ctx, taskID, key, createdAt, firstSeen, lastSeen, parentID)
		if err != nil || !res.Claimed {
			return res, err
		}
		if _, err := qtx.db.ExecContext(ctx,
			`UPDATE tasks SET anchor_session_id = ?, anchor_provider_id = ?, anchor_provider_instance_id = ? WHERE id = ?`,
			key.UpstreamID, key.ProviderID, key.ProviderInstanceID, taskID); err != nil {
			return application.ClaimResult{}, err
		}
		return res, nil
	})
}

// --- TaskBinding 列读写（D3 版本化 envelope）---

// GetTaskBinding 读取任务绑定 envelope 并解码。无绑定（NULL）→ (zero, false, nil)；
// 存在但版本未知/字段缺失/JSON 畸形 → fail-closed 错误（调用方按恢复失败处置，
// 不猜测）。行不存在返回 sql.ErrNoRows。
func (q *Queries) GetTaskBinding(ctx context.Context, taskID string) (domain.TaskBinding, bool, error) {
	var raw sql.NullString
	err := q.db.QueryRowContext(ctx, `SELECT task_binding FROM tasks WHERE id = ?`, taskID).Scan(&raw)
	if err != nil {
		return domain.TaskBinding{}, false, err
	}
	if !raw.Valid || raw.String == "" {
		return domain.TaskBinding{}, false, nil
	}
	b, derr := domain.DecodeTaskBinding(raw.String)
	if derr != nil {
		return domain.TaskBinding{}, false, fmt.Errorf("store: task %s binding: %w", taskID, derr)
	}
	return b, true, nil
}

// GetTaskProviderID 读取任务声明的 provider 标识（C7 门禁修复：生命周期副作用
// 前按任务行 provider_id 做 registry 准入）。行不存在返回 sql.ErrNoRows；
// NOT NULL 列 + 启动期回填门禁保证非空（防御性仍按空串透传由调用方 fail-closed）。
func (q *Queries) GetTaskProviderID(ctx context.Context, taskID string) (string, error) {
	var pid sql.NullString
	if err := q.db.QueryRowContext(ctx, `SELECT provider_id FROM tasks WHERE id = ?`, taskID).Scan(&pid); err != nil {
		return "", err
	}
	return pid.String, nil
}

// SetTaskBinding 写入任务绑定 envelope（先 Encode——结构不完整拒绝落库）。
// 行不存在返回 sql.ErrNoRows。
func (q *Queries) SetTaskBinding(ctx context.Context, taskID string, b domain.TaskBinding) error {
	raw, err := b.Encode()
	if err != nil {
		return fmt.Errorf("store: set task binding: %w", err)
	}
	res, err := q.db.ExecContext(ctx, `UPDATE tasks SET task_binding = ? WHERE id = ?`, raw, taskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CommitActivation 激活/恢复成功提交事务（opencode-v1-spi-provider tasks 4.3 /
// design D9 binding 提交协议）：binding 写入与任务状态 CAS 同事务原子完成——
// CAS 失配（状态已离 activating）或事务失败时零持久化副作用（binding 不落库、
// 旧 binding 保持原值）。status CAS 复用 updateTaskStatus 单点实现（Matched/
// StatusChanged 语义与其逐字一致：同值排除谓词 + fromStatus expected + 事务内
// 失配分类）。BEGIN IMMEDIATE 由连接 DSN 提供（与 claim 同一互斥域）。
func (q *Queries) CommitActivation(ctx context.Context, taskID string, binding domain.TaskBinding,
	fromStatus, toStatus ocdecktask.Status) (application.TransitionResult, error) {
	raw, err := binding.Encode()
	if err != nil {
		return application.TransitionResult{}, fmt.Errorf("store: commit activation: encode binding: %w", err)
	}
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		// 状态 CAS 先行：失配/缺行时 binding 零写入（零持久化副作用，D9 提交协议
		// ——先写 binding 会使其逃逸出失配事务）；matched 后同事务同行补写 binding。
		res, err := qx.updateTaskStatus(ctx, taskID, toStatus, nil, true, string(fromStatus))
		if err != nil || !res.Matched {
			return res, err
		}
		if _, uerr := qx.db.ExecContext(ctx,
			`UPDATE tasks SET task_binding = ? WHERE id = ?`, raw, taskID); uerr != nil {
			return application.TransitionResult{}, uerr
		}
		return res, nil
	})
}

// --- 既有接缝迁移（单命名空间行为等价）---

// claimTaskSessionInTx 是旧接缝的内部实现，迁移为完整三列谓词 + v1 常量命名空间
// （D4：单命名空间下与裸 session_id 判定行为等价——0018 后全部行携带该命名空间）。
func (q *Queries) claimTaskSessionInTx(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	return q.claimSessionKeyInTx(ctx, taskID,
		domain.SessionKey{ProviderID: nsProviderIDV1, ProviderInstanceID: nsProviderInstanceV1Default, UpstreamID: sessionID},
		createdAt, firstSeen, lastSeen, parentID)
}

// ClaimTaskSession 旧接缝（task 层现状签名）：BEGIN IMMEDIATE 事务内按 v1
// 命名空间 claim。
func (q *Queries) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	if _, isTx := q.db.(*sql.Tx); isTx {
		return q.claimTaskSessionInTx(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
	}
	return runImmediateTx(ctx, q, func(qtx *Queries) (application.ClaimResult, error) {
		return qtx.claimTaskSessionInTx(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
	})
}

// ClaimTaskSessionAndSetAnchor 旧接缝：claim + 锚定三元组写入（v1 命名空间）。
func (q *Queries) ClaimTaskSessionAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	key := domain.SessionKey{ProviderID: nsProviderIDV1, ProviderInstanceID: nsProviderInstanceV1Default, UpstreamID: sessionID}
	return runImmediateTx(ctx, q, func(qtx *Queries) (application.ClaimResult, error) {
		res, err := qtx.claimSessionKeyInTx(ctx, taskID, key, createdAt, firstSeen, lastSeen, parentID)
		if err != nil || !res.Claimed {
			return res, err
		}
		if _, err := qtx.db.ExecContext(ctx,
			`UPDATE tasks SET anchor_session_id = ?, anchor_provider_id = ?, anchor_provider_instance_id = ? WHERE id = ?`,
			key.UpstreamID, key.ProviderID, key.ProviderInstanceID, taskID); err != nil {
			return application.ClaimResult{}, err
		}
		return res, nil
	})
}

// ListTaskSessionKeys 读任务全部归属行的 SessionKey（承接判定矩阵「历史归属
// 集合」；调用方按命名空间过滤）。
func (q *Queries) ListTaskSessionKeys(ctx context.Context, taskID string) ([]domain.SessionKey, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT provider_id, provider_instance_id, session_id FROM task_sessions WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SessionKey
	for rows.Next() {
		var k domain.SessionKey
		if err := rows.Scan(&k.ProviderID, &k.ProviderInstanceID, &k.UpstreamID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetTaskAnchor 读锚定三元组（承接判定矩阵；无锚定 → 空串列）。
func (q *Queries) GetTaskAnchor(ctx context.Context, taskID string) (anchorID, providerID, instanceID string, err error) {
	var sid, pid, iid sql.NullString
	err = q.db.QueryRowContext(ctx,
		`SELECT anchor_session_id, anchor_provider_id, anchor_provider_instance_id FROM tasks WHERE id = ?`,
		taskID).Scan(&sid, &pid, &iid)
	if err != nil {
		return "", "", "", err
	}
	return sid.String, pid.String, iid.String, nil
}
