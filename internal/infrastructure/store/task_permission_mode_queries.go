// task_permission_mode_queries.go 定义任务启动事实列 permission_mode_at_start 的
// 窄端口读写（task-permission-mode D5/D8，tasks 2.2）。
//
// 启动事实由 startRuntimeWithPortRetry 建 serve 进程前写入一次（写失败不建进程），
// 由 resume/修复路径读校验后重建 RuntimePermissionState。事实写入/回填均不推进
// updated_at、不发事件（与 rename_pending 同哲学：非业务列真实变更）。
package store

import (
	"context"
	"database/sql"
)

// BackfillPermissionModeAtStart 启动回填：仅 status='active' 且启动事实为 NULL 的行
// 按持久化 permission_mode 回填（task-permission-mode tasks 2.2 / 组合根启动顺序）。
// 幂等（仅 NULL 行受影响）；不推进 updated_at、不发事件；DB 错误原样返回。
func (q *Queries) BackfillPermissionModeAtStart(ctx context.Context) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET permission_mode_at_start = permission_mode
		 WHERE status = 'active' AND permission_mode_at_start IS NULL`)
	return err
}

// SetPermissionModeAtStart 写入启动事实（本次 argv 实际模式值）。
// 不推进 updated_at、不发事件；行不存在返回 sql.ErrNoRows。
func (q *Queries) SetPermissionModeAtStart(ctx context.Context, taskID string, mode string) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET permission_mode_at_start = ? WHERE id = ?`, mode, taskID)
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

// GetPermissionModeAtStart 读取启动事实（NULL → nil）。
// 行不存在返回 sql.ErrNoRows（由 adapter 归一化 application.ErrTaskNotFound）。
func (q *Queries) GetPermissionModeAtStart(ctx context.Context, taskID string) (*string, error) {
	row := q.db.QueryRowContext(ctx, `SELECT permission_mode_at_start FROM tasks WHERE id = ?`, taskID)
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
