// task_info_queries.go 定义任务信息业务列单事务提交与改名意图元数据写清
// （task-info-editable D6：CommitTaskInfoUpdate 单事务原子提交 + rename_pending 列）。
//
// 事务结果四态（design D6 表）：
//
//	| 事务内容                      | Changed | updated_at |
//	| 仅设置 pending                | false   | 不推进     |
//	| 业务列真实变化 + 清除 pending  | true    | 推进       |
//	| 仅清除 pending                | false   | 不推进     |
//	| 业务列同值 + 清除 pending      | false   | 不推进     |
//
// Changed 只由业务列 name/branch/env_snapshot 的真实变化决定，rename_pending 永不参与
// Changed 计算。updated_at 仅真实业务变更且跨秒推进（buildUpdateOnAdvance，同值原子 no-op）。
package store

import (
	"context"
	"database/sql"
	"strings"

	"ocdeck/internal/application"
)

// taskInfoColUpdate 表达 CommitTaskInfoUpdate 事务内单个提供字段的目标写入
// （col = 目标值 cur → newVal，NULL 安全同值判定）。
type taskInfoColUpdate struct {
	col    string
	cur    sql.NullString
	newVal sql.NullString
}

// CommitTaskInfoUpdate 单事务原子提交任务信息业务列（name/branch/env_snapshot，presence
// 语义 nil = 不修改该字段）并清除 rename_pending 意图（task-info-editable D6）。
//
// 业务列真实变化 → Changed=true、updated_at 跨秒推进、同事务清除意图；业务列全同值 →
// Changed=false、不推进 updated_at，仅意图存在时同事务清除意图（意图清除本身不动业务列）。
// 同值排除下推到 UPDATE WHERE（NULL 安全 IS/IS NOT，F-01 原子）；RowsAffected=0 分类
// 对齐 updateSingleNullableCol 先例（行存在同值 → Matched+!Changed）。
func (q *Queries) CommitTaskInfoUpdate(ctx context.Context, id string, update application.TaskInfoUpdate) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT name, branch, env_snapshot, rename_pending, updated_at FROM tasks WHERE id = ?`, id)
		var curName, curBranch string
		var curEnv, curPending sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curName, &curBranch, &curEnv, &curPending, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}

		// 收集调用方提供的字段（presence 语义 nil = 不修改），同值判定 NULL 安全。
		var ups []taskInfoColUpdate
		if update.Name != nil {
			ups = append(ups, taskInfoColUpdate{col: "name",
				cur:    sql.NullString{String: curName, Valid: true},
				newVal: sql.NullString{String: *update.Name, Valid: true}})
		}
		if update.Branch != nil {
			ups = append(ups, taskInfoColUpdate{col: "branch",
				cur:    sql.NullString{String: curBranch, Valid: true},
				newVal: sql.NullString{String: *update.Branch, Valid: true}})
		}
		if update.EnvSnapshot != nil {
			ups = append(ups, taskInfoColUpdate{col: "env_snapshot", cur: curEnv, newVal: nullableString(update.EnvSnapshot)})
		}

		// 判定业务列是否真实变化（任一提供列与当前值不同）。
		bizChanged := false
		for _, u := range ups {
			if !nullStringEqual(u.cur, u.newVal) {
				bizChanged = true
				break
			}
		}

		// 业务列同值：仅意图存在时清除意图（不动业务列、不推进 updated_at），否则纯 no-op。
		if !bizChanged {
			if !curPending.Valid {
				return application.MutationResult{Matched: true}, nil
			}
			if _, err := qx.db.ExecContext(ctx,
				`UPDATE tasks SET rename_pending = NULL WHERE id = ? AND rename_pending IS NOT NULL`, id); err != nil {
				return application.MutationResult{}, err
			}
			return application.MutationResult{Matched: true}, nil
		}

		// 业务列真实变化：单事务写业务列 + 清除意图；updated_at 仅跨秒推进。
		setFrags := make([]string, 0, len(ups)+1)
		cols := make([]string, 0, len(ups))
		newVals := make([]sql.NullString, 0, len(ups))
		var args []any
		for _, u := range ups {
			setFrags = append(setFrags, u.col+" = ?")
			cols = append(cols, u.col)
			newVals = append(newVals, u.newVal)
			args = append(args, u.newVal)
		}
		setFrags = append(setFrags, "rename_pending = NULL")
		diffPred, diffArgs := anyColDiffersPredicate(cols, newVals)
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET " + strings.Join(setFrags, ", ") + ", " + updClause +
			" WHERE id = ? AND (" + diffPred + ")"
		args = append(args, updArgs...)
		args = append(args, id)
		args = append(args, diffArgs...)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.MutationResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.MutationResult{}, err
		}
		if n == 0 {
			// 同值（并发下被写为同值）。
			return application.MutationResult{Matched: true}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// SetTaskRenamePending 写入改名恢复意图 JSON（task-info-editable D2 步骤 5）。
//
// 意图元数据写入 MUST NOT 推进 updated_at（design D6 表：仅设置 pending → 不推进）；
// rename_pending 永不参与 Changed 计算（恒 Changed=false）。同值重复写幂等 no-op。
func (q *Queries) SetTaskRenamePending(ctx context.Context, id string, pendingJSON string) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT rename_pending FROM tasks WHERE id = ?`, id)
		var cur sql.NullString
		if err := row.Scan(&cur); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		newNS := sql.NullString{String: pendingJSON, Valid: true}
		if nullStringEqual(cur, newNS) {
			return application.MutationResult{Matched: true}, nil
		}
		pred, predArg := colNotEqualPredicate("rename_pending", newNS)
		qry := "UPDATE tasks SET rename_pending = ? WHERE id = ? AND " + pred
		args := []any{newNS, id}
		if predArg != nil {
			args = append(args, predArg)
		}
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.MutationResult{}, err
		}
		if _, err := res.RowsAffected(); err != nil {
			return application.MutationResult{}, err
		}
		// 意图写入不推进 updated_at、不参与 Changed（Matched=行存在，事务内已确认）。
		return application.MutationResult{Matched: true}, nil
	})
}

// ClearTaskRenamePending 清除改名恢复意图（task-info-editable D2 失败矩阵「确定未生效」）。
//
// 意图清除 MUST NOT 推进 updated_at（design D6 表：仅清除 pending → 不推进）；
// rename_pending 永不参与 Changed 计算（恒 Changed=false）。已无意图时幂等成功。
func (q *Queries) ClearTaskRenamePending(ctx context.Context, id string) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT rename_pending FROM tasks WHERE id = ?`, id)
		var cur sql.NullString
		if err := row.Scan(&cur); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		if !cur.Valid {
			return application.MutationResult{Matched: true}, nil
		}
		if _, err := qx.db.ExecContext(ctx,
			`UPDATE tasks SET rename_pending = NULL WHERE id = ? AND rename_pending IS NOT NULL`, id); err != nil {
			return application.MutationResult{}, err
		}
		// 意图清除不推进 updated_at、不参与 Changed（Matched=行存在，事务内已确认）。
		return application.MutationResult{Matched: true}, nil
	})
}

// GetTaskRenamePending 读取未收敛改名意图 JSON（nil = 无意图）。
//
// 仅由内部收敛编排（R1）消费；行不存在返回 sql.ErrNoRows（由 adapter 归一化
// application.ErrTaskNotFound）。MUST NOT 进入公共 Task DTO / TaskSnapshot。
func (q *Queries) GetTaskRenamePending(ctx context.Context, id string) (*string, error) {
	row := q.db.QueryRowContext(ctx, `SELECT rename_pending FROM tasks WHERE id = ?`, id)
	var cur sql.NullString
	if err := row.Scan(&cur); err != nil {
		return nil, err
	}
	if !cur.Valid {
		return nil, nil
	}
	v := cur.String
	return &v, nil
}
