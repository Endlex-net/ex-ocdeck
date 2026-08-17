// helpers.go 定义 store 包内部同值原子 no-op 改造的辅助函数（task P1.2 / F-01）。
package store

import (
	"context"
	"database/sql"

	"ocdeck/internal/application"
)

// nullableString 把 *string 映射为 sql.NullString：nil → {Valid:false}。
func nullableString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// nullStringEqual 判断两个 sql.NullString 是否同值（NULL 安全：两个 Invalid 相等）。
func nullStringEqual(a, b sql.NullString) bool {
	if !a.Valid && !b.Valid {
		return true
	}
	if a.Valid != b.Valid {
		return false
	}
	return a.String == b.String
}

// nullInt64Equal 判断两个 sql.NullInt64 是否同值（NULL 安全：两个 Invalid 相等）。
func nullInt64Equal(a, b sql.NullInt64) bool {
	if !a.Valid && !b.Valid {
		return true
	}
	if a.Valid != b.Valid {
		return false
	}
	return a.Int64 == b.Int64
}

// colNotEqualPredicate 构造单列的「新值 != 当前列」NULL-safe SQL 谓词片段。
// 用于 UPDATE 的 WHERE 把同值排除下推到语句自身（F-01：同值判定必须原子）。
//   - 新值为 NULL（colNS.Valid=false）：`col IS NOT NULL`
//   - 新值非 NULL：`col IS NOT ?`（绑定 colNS）
//
// 返回片段与需要追加到 args 的参数；arg 为 nil 时不 append。调用方按顺序拼接。
func colNotEqualPredicate(col string, newVal sql.NullString) (fragment string, arg any) {
	if !newVal.Valid {
		return col + " IS NOT NULL", nil
	}
	return col + " IS NOT ?", newVal
}

// expectedPredicate 构造单列的「当前列 == expected」NULL-safe SQL 谓词片段（CAS expected 条件）。
//   - expected 为 NULL：`col IS NULL`
//   - expected 非 NULL：`col IS ?`（绑定 expected）
//
// 返回片段与需要追加到 args 的参数；arg 为 nil 时不 append。调用方按顺序拼接。
func expectedPredicate(col string, expected sql.NullString) (fragment string, arg any) {
	if !expected.Valid {
		return col + " IS NULL", nil
	}
	return col + " IS ?", expected
}

// anyColDiffersPredicate 构造多列的「至少一列不同」NULL-safe SQL 谓词（同值排除用）。
// 用于多列写方法（status+last_error 等）把同值排除下推到 UPDATE WHERE（F-01）：
// 仅当全部写入列都等于新值时才视为同值行排除；任一列不同即真实变更行，MUST 匹配。
// 返回 `(c1 IS NOT ? OR c2 IS NOT ...)` 片段与按序的参数（NULL 用 IS NOT NULL，无参数）。
func anyColDiffersPredicate(cols []string, newVals []sql.NullString) (string, []any) {
	parts := make([]string, 0, len(cols))
	var args []any
	for i, c := range cols {
		v := newVals[i]
		if !v.Valid {
			parts = append(parts, c+" IS NOT NULL")
		} else {
			parts = append(parts, c+" IS NOT ?")
			args = append(args, v)
		}
	}
	return joinOr(parts), args
}

// joinOr 用 " OR " 连接片段（用于 anyColDiffersPredicate）。
func joinOr(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += " OR " + p
	}
	return out
}

// buildUpdateOnAdvance 构造 updated_at 子句与参数：跨秒推进时 `updated_at = ?`（带 now 参数），
// 同秒实变时 `updated_at = updated_at`（无参数）。返回子句字符串与可选 now 参数（nil 表示不追加）。
func buildUpdateOnAdvance(curUpdatedAt, now int64) (string, []any) {
	if now != curUpdatedAt {
		return "updated_at = ?", []any{now}
	}
	return "updated_at = updated_at", nil
}

// updateSingleNullableCol 是 env_snapshot/notice 等单 nullable 列写入的共享实现。
//
// 同值排除下推到 UPDATE WHERE（col IS NOT ? / col IS NOT NULL，F-01 原子）；事务内读
// updated_at 旧值以判定 UpdatedAtAdvanced。RowsAffected=0 表示同值（行存在）或行不存在。
// col 为列名（env_snapshot | notice）。
func (q *Queries) updateSingleNullableCol(ctx context.Context, id, col string, newVal *string) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT `+col+`, updated_at FROM tasks WHERE id = ?`, id)
		var cur sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&cur, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		newNS := nullableString(newVal)
		if nullStringEqual(cur, newNS) {
			// 同值匹配：不写，updated_at 不动。
			return application.MutationResult{Matched: true}, nil
		}
		colPred, colArg := colNotEqualPredicate(col, newNS)
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET " + col + " = ?, " + updClause +
			" WHERE id = ? AND " + colPred
		args := []any{newNS}
		args = append(args, updArgs...)
		args = append(args, id)
		if colArg != nil {
			args = append(args, colArg)
		}
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