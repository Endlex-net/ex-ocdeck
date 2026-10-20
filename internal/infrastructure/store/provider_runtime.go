// provider_runtime.go 实现 v2 runtime 实例记录的持久化（opencode-v2-provider
// tasks 5.1 / design D3）：跨进程恢复来源与记录生命周期——pid 允许 NULL「启动中」
// 态先于 server 启动写入（唯一合法写序①），步骤⑤才落实际 pid；启动失败保留
// 记录 + 记 last_error。全部方法独立短事务（记录级原子性；与承接原子事务不同域）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProviderRuntimeRecord 是 provider_runtime 单行（D3 实例记录）。
type ProviderRuntimeRecord struct {
	ProviderID string
	InstanceID string
	Port       int
	PID        sql.NullInt64 // NULL = 启动中（未验证/崩溃窗口）
	LastError  sql.NullString
	UpdatedAt  int64
}

// GetProviderRuntime 读实例记录；无记录 → (zero, false, nil)。
func (q *Queries) GetProviderRuntime(ctx context.Context, providerID, instanceID string) (ProviderRuntimeRecord, bool, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT provider_id, instance_id, port, pid, last_error, updated_at
		 FROM provider_runtime WHERE provider_id = ? AND instance_id = ?`,
		providerID, instanceID)
	var r ProviderRuntimeRecord
	var pid sql.NullInt64
	err := row.Scan(&r.ProviderID, &r.InstanceID, &r.Port, &pid, &r.LastError, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRuntimeRecord{}, false, nil
	}
	if err != nil {
		return ProviderRuntimeRecord{}, false, err
	}
	r.PID = pid
	return r, true, nil
}

// PutProviderRuntimeStart 写入/重置实例记录为「启动中」态（写序①：port 先行、
// pid=NULL）。已存在记录保留 port（端口固定不轮换——传入 port 不一致时拒绝，
// 调用方 fail-closed）；不存在则插入。
func (q *Queries) PutProviderRuntimeStart(ctx context.Context, providerID, instanceID string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("store: provider_runtime start: port %d 越界", port)
	}
	var stored int
	err := q.db.QueryRowContext(ctx,
		`SELECT port FROM provider_runtime WHERE provider_id = ? AND instance_id = ?`,
		providerID, instanceID).Scan(&stored)
	switch {
	case err == nil:
		if stored != port {
			return fmt.Errorf("store: provider_runtime: 端口固定不轮换（记录 %d != 请求 %d）", stored, port)
		}
		_, err = q.db.ExecContext(ctx,
			`UPDATE provider_runtime SET pid = NULL, last_error = NULL, updated_at = ?
			 WHERE provider_id = ? AND instance_id = ?`,
			time.Now().UnixMilli(), providerID, instanceID)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO provider_runtime (provider_id, instance_id, port, pid, updated_at)
		 VALUES (?, ?, ?, NULL, ?)`,
		providerID, instanceID, port, time.Now().UnixMilli())
	return err
}

// SetProviderRuntimePID 写序⑤：验证通过后落实际 pid（「可用」态）；同时清除
// last_error。行不存在返回 sql.ErrNoRows。
func (q *Queries) SetProviderRuntimePID(ctx context.Context, providerID, instanceID string, pid int) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE provider_runtime SET pid = ?, last_error = NULL, updated_at = ?
		 WHERE provider_id = ? AND instance_id = ?`,
		pid, time.Now().UnixMilli(), providerID, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetProviderRuntimeError 记启动失败诊断（D3 恢复矩阵「启动失败」分支：保留
// 记录 pid=NULL + last_error）。行不存在时忽略（记录尚未来得及写入的窗口——
// 调用方先走 PutProviderRuntimeStart）。
func (q *Queries) SetProviderRuntimeError(ctx context.Context, providerID, instanceID string, cause string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE provider_runtime SET last_error = ?, updated_at = ?
		 WHERE provider_id = ? AND instance_id = ?`,
		cause, time.Now().UnixMilli(), providerID, instanceID)
	return err
}
