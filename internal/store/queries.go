// Queries 手写的类型安全查询层。
//
// 本机无 sqlc CLI，故手写与 sqlc 生成产物同结构的 Queries（design.md §8）。
// 方法签名风格对齐 sqlc：以操作 + 实体名命名，参数用结构体承载，返回行用扫描结构体。
// 若后续接入 sqlc，可直接替换此文件而不影响调用方（方法签名保持一致）。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DBTX 是 *sql.DB 与 *sql.Tx 共同满足的查询接口，使同一组 Queries 方法
// 可在裸 DB 与事务上下文中复用（design.md §8 事务短小、session 对齐/CAS/删除意图
// 必须原子提交）。sqlc 生成的 DBTX 接口同构，后续接入 sqlc 时可平替。
//
// 注意：MaxOpenConns(1) 下事务内不得再经 *sql.DB 发查询（自锁）——
// 事务内全部查询 MUST 走绑定 *sql.Tx 的 Queries（见 DB.WithTx）。
type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Queries 持有底层 DBTX（*sql.DB 或 *sql.Tx），所有方法在其上执行。
// New 绑定 *sql.DB；DB.WithTx 在事务内构造绑定 *sql.Tx 的 Queries 供回调使用。
type Queries struct {
	db DBTX
}

func New(db DBTX) *Queries {
	return &Queries{db: db}
}

// ProjectRow projects 表行映射。
type ProjectRow struct {
	ID            string
	Name          string
	Path          string
	DefaultBranch string
	CreatedAt     int64
}

// TaskRow tasks 表行映射。env_snapshot 为激活时合并的 env JSON（design.md §2/§8）。
type TaskRow struct {
	ID           string
	ProjectID    string
	Name         string
	Branch       string
	Status       string
	WorktreePath string
	LastPort     sql.NullInt64
	LastError    sql.NullString
	Notice       sql.NullString
	DeleteMode   sql.NullString
	EnvSnapshot  sql.NullString
	CreatedAt    int64
	UpdatedAt    int64
	ArchivedAt   sql.NullInt64
}

// EnvVarRow env 变量行映射（project_env_vars / task_env_vars 共用）。
type EnvVarRow struct {
	Key   string
	Value string
}

// GlobalEnvVarRow global_env_vars 行映射（design.md §8/§2，全局级 env）。
// Mode ∈ follow_host | manual；follow_host 时 Value 忽略，激活时从服务端进程 env 解析。
type GlobalEnvVarRow struct {
	Key   string
	Mode  string
	Value string
}

// SessionRow task_sessions 表行映射。
type SessionRow struct {
	TaskID           string
	SessionID        string
	SessionCreatedAt int64
	FirstSeenAt      int64
	LastSeenAt       int64
	// ParentID 非空表示 background subagent 子会话；空为顶层会话（design.md §4 锚定隔离）。
	ParentID string
}

// nowUnix 返回当前 Unix 时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// CreateProject 插入项目行。
func (q *Queries) CreateProject(ctx context.Context, id, name, path, defaultBranch string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, path, default_branch, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, path, defaultBranch, nowUnix())
	return err
}

// GetProject 按 ID 查询项目。
func (q *Queries) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, name, path, default_branch, created_at FROM projects WHERE id = ?`, id)
	var p ProjectRow
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.CreatedAt)
	return p, err
}

// GetProjectByPath 按 path 查询项目（path 唯一）。
func (q *Queries) GetProjectByPath(ctx context.Context, path string) (ProjectRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, name, path, default_branch, created_at FROM projects WHERE path = ?`, path)
	var p ProjectRow
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.CreatedAt)
	return p, err
}

// ListProjects 返回全部项目。
func (q *Queries) ListProjects(ctx context.Context) ([]ProjectRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, name, path, default_branch, created_at FROM projects ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectTaskCountsRow 项目任务概况（design.md §21 详情含任务数量与状态分布）。
type ProjectTaskCountsRow struct {
	Total int
	// ByStatus 状态 -> 计数。
	ByStatus map[string]int
}

// CountProjectTasks 返回项目下任务总数与按状态分布。空 map 表示无任务。
func (q *Queries) CountProjectTasks(ctx context.Context, projectID string) (ProjectTaskCountsRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM tasks WHERE project_id = ? GROUP BY status`, projectID)
	if err != nil {
		return ProjectTaskCountsRow{}, err
	}
	defer rows.Close()
	out := ProjectTaskCountsRow{ByStatus: map[string]int{}}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return ProjectTaskCountsRow{}, err
		}
		out.ByStatus[status] = n
		out.Total += n
	}
	return out, rows.Err()
}

// HasProjectTasks 返回项目是否仍有任务（用于删除前置检查，design.md §21 409）。
// 比 CountProjectTasks 更轻量，仅需布尔结果。
func (q *Queries) HasProjectTasks(ctx context.Context, projectID string) (bool, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id = ?)`, projectID)
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// DeleteProject 按 ID 删除项目（CASCADE 删除其 tasks/env_vars）。
func (q *Queries) DeleteProject(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

// DeleteProjectIfEmpty 原子删除项目：仅当项目下无任务时删除，避免"先检查再删除"
// 竞态（B9/design.md §19）。返回 (deleted bool, err)：deleted=true 表示已删除，
// deleted=false 表示因有任务或项目不存在未删除；调用方需结合 GetProject 区分
// "有任务 409"与"不存在 404"（推荐：先 GetProject 判存在，再调本方法判空）。
//
// 单语句原子：NOT EXISTS 子查询与 DELETE 在同一事务/语句内，任务无法在检查与
// 删除之间插入被 CASCADE 删除。
func (q *Queries) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM projects WHERE id = ? AND NOT EXISTS(SELECT 1 FROM tasks WHERE project_id = ?)`,
		id, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetProjectEnvVar 插入或更新项目级 env 变量。
func (q *Queries) SetProjectEnvVar(ctx context.Context, projectID, key, value string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO project_env_vars (project_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(project_id, key) DO UPDATE SET value = excluded.value`,
		projectID, key, value)
	return err
}

// ListProjectEnvVars 列出项目级 env 变量。
func (q *Queries) ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVarRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT key, value FROM project_env_vars WHERE project_id = ? ORDER BY key ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvVarRow
	for rows.Next() {
		var e EnvVarRow
		if err := rows.Scan(&e.Key, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteProjectEnvVar 删除项目级 env 变量。
func (q *Queries) DeleteProjectEnvVar(ctx context.Context, projectID, key string) error {
	_, err := q.db.ExecContext(ctx,
		`DELETE FROM project_env_vars WHERE project_id = ? AND key = ?`, projectID, key)
	return err
}

// SetGlobalEnvVar 插入或更新全局级 env 变量（upsert，design.md §8/§2）。
// mode ∈ follow_host | manual；follow_host 时 value 忽略，可传空。
func (q *Queries) SetGlobalEnvVar(ctx context.Context, key, mode, value string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO global_env_vars (key, mode, value) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET mode = excluded.mode, value = excluded.value`,
		key, mode, value)
	return err
}

// ListGlobalEnvVars 列出全局级 env 变量（按 key 升序）。
func (q *Queries) ListGlobalEnvVars(ctx context.Context) ([]GlobalEnvVarRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT key, mode, value FROM global_env_vars ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GlobalEnvVarRow
	for rows.Next() {
		var e GlobalEnvVarRow
		if err := rows.Scan(&e.Key, &e.Mode, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteGlobalEnvVar 删除全局级 env 变量。
func (q *Queries) DeleteGlobalEnvVar(ctx context.Context, key string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM global_env_vars WHERE key = ?`, key)
	return err
}

// CreateTask 插入任务行。
func (q *Queries) CreateTask(ctx context.Context, t TaskRow) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Name, t.Branch, t.Status, t.WorktreePath, nowUnix(), nowUnix())
	return err
}

// GetTask 按 ID 查询任务（含 env_snapshot）。
func (q *Queries) GetTask(ctx context.Context, id string) (TaskRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, project_id, name, branch, status, worktree_path, last_port, last_error, notice,
		        delete_mode, env_snapshot, created_at, updated_at, archived_at
		 FROM tasks WHERE id = ?`, id)
	return scanTaskRow(row)
}

// ListTasksByProject 列出某项目的全部任务。
func (q *Queries) ListTasksByProject(ctx context.Context, projectID string) ([]TaskRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, project_id, name, branch, status, worktree_path, last_port, last_error, notice,
		        delete_mode, env_snapshot, created_at, updated_at, archived_at
		 FROM tasks WHERE project_id = ? ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRows(rows)
}

// ListAllTasks 列出全部任务（用于启动 reconciliation）。
func (q *Queries) ListAllTasks(ctx context.Context) ([]TaskRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, project_id, name, branch, status, worktree_path, last_port, last_error, notice,
		        delete_mode, env_snapshot, created_at, updated_at, archived_at
		 FROM tasks ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRows(rows)
}

// UpdateTaskStatus 更新任务状态与 last_error，刷新 updated_at。
func (q *Queries) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, lastError, nowUnix(), id)
	return err
}

// UpdateTaskEnvSnapshot 更新 env_snapshot（design.md §2，激活时持久化）。
func (q *Queries) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET env_snapshot = ?, updated_at = ? WHERE id = ?`,
		envSnapshot, nowUnix(), id)
	return err
}

// UpdateTaskLastPort 更新 last_port（仅记录上次成功端口，非事实来源，design.md §3）。
func (q *Queries) UpdateTaskLastPort(ctx context.Context, id string, port int) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET last_port = ?, updated_at = ? WHERE id = ?`, port, nowUnix(), id)
	return err
}

// UpdateTaskNotice 更新 notice JSON 数组（design.md §8）。
func (q *Queries) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET notice = ?, updated_at = ? WHERE id = ?`, notice, nowUnix(), id)
	return err
}

// SetTaskDeleteMode 持久化 delete_mode（design.md §8/§19）。
func (q *Queries) SetTaskDeleteMode(ctx context.Context, id, mode string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET delete_mode = ?, updated_at = ? WHERE id = ?`, mode, nowUnix(), id)
	return err
}

// ArchiveTask 置 archived 状态并记录 archived_at。
func (q *Queries) ArchiveTask(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'archived', archived_at = ?, updated_at = ? WHERE id = ?`,
		nowUnix(), nowUnix(), id)
	return err
}

// RestoreTask 从 archived 恢复到 suspended。
func (q *Queries) RestoreTask(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'suspended', updated_at = ? WHERE id = ?`, nowUnix(), id)
	return err
}

// DeleteTask 按 ID 删除任务（CASCADE 删除其 sessions/env_vars）。
func (q *Queries) DeleteTask(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	return err
}

// SetTaskEnvVar 插入或更新任务级 env 变量。
func (q *Queries) SetTaskEnvVar(ctx context.Context, taskID, key, value string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO task_env_vars (task_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(task_id, key) DO UPDATE SET value = excluded.value`,
		taskID, key, value)
	return err
}

// ListTaskEnvVars 列出任务级 env 变量。
func (q *Queries) ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVarRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT key, value FROM task_env_vars WHERE task_id = ? ORDER BY key ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvVarRow
	for rows.Next() {
		var e EnvVarRow
		if err := rows.Scan(&e.Key, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteTaskEnvVar 删除任务级 env 变量。
func (q *Queries) DeleteTaskEnvVar(ctx context.Context, taskID, key string) error {
	_, err := q.db.ExecContext(ctx,
		`DELETE FROM task_env_vars WHERE task_id = ? AND key = ?`, taskID, key)
	return err
}

// UpsertTaskSession 插入或更新会话归属行（design.md §4，upsert 用 MAX last_seen_at）。
// parent_id（design.md §4 锚定隔离）：非空为 background subagent 子会话，空为顶层会话；
// ON CONFLICT 时 parent_id 以 excluded 为准（事件更新可能补全 parent_id）。
func (q *Queries) UpsertTaskSession(ctx context.Context, s SessionRow) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id, session_id) DO UPDATE SET
		   last_seen_at = MAX(excluded.last_seen_at, task_sessions.last_seen_at),
		   parent_id = excluded.parent_id`,
		s.TaskID, s.SessionID, s.SessionCreatedAt, s.FirstSeenAt, s.LastSeenAt, s.ParentID)
	return err
}

// ListTaskSessions 列出任务的会话归属行（全量，含子会话）。
// tie-breaker（design.md §4）：last_seen_at DESC → session_created_at DESC → session_id DESC。
func (q *Queries) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id
		 FROM task_sessions WHERE task_id = ?
		 ORDER BY last_seen_at DESC, session_created_at DESC, session_id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.TaskID, &s.SessionID, &s.SessionCreatedAt, &s.FirstSeenAt, &s.LastSeenAt, &s.ParentID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListTopLevelTaskSessions 列出任务的顶层会话归属行（parent_id 为空），
// 供锚定候选（resolveAnchorSession/ReopenAttach/Suspend 修复）取最近顶层 session。
// 语义同 ListTaskSessions 的排序，仅过滤 parent_id IS NULL OR parent_id = ''。
func (q *Queries) ListTopLevelTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id
		 FROM task_sessions
		 WHERE task_id = ? AND (parent_id IS NULL OR parent_id = '')
		 ORDER BY last_seen_at DESC, session_created_at DESC, session_id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.TaskID, &s.SessionID, &s.SessionCreatedAt, &s.FirstSeenAt, &s.LastSeenAt, &s.ParentID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteTaskSession 删除会话归属行。
func (q *Queries) DeleteTaskSession(ctx context.Context, taskID, sessionID string) error {
	_, err := q.db.ExecContext(ctx,
		`DELETE FROM task_sessions WHERE task_id = ? AND session_id = ?`, taskID, sessionID)
	return err
}

// UpdateTaskNoticeCAS 乐观更新 notice（CAS）：仅当当前 notice 等于 expected 时
// 才写入 newNotice，返回是否替换成功（RowsAffected=1）。
// expected 为 sql.NullString：NULL 表示"当前为空"的期望，sql.NullString{Valid:false}。
// 设计依据 design.md §5/§8：notice 更新 MUST 为 CAS/事务，避免 Delete/Suspend/SSE 与
// 后台重试的 notice 写互相覆盖。
func (q *Queries) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (replaced bool, err error) {
	// NULL 与常量在 SQL 中不能用 = 比较（NULL = NULL → NULL），需用 IS 分支。
	var (
		res sql.Result
		qry string
	)
	if expected.Valid {
		qry = `UPDATE tasks SET notice = ?, updated_at = ?
		       WHERE id = ? AND notice = ?`
		res, err = q.db.ExecContext(ctx, qry, newNotice, nowUnix(), id, expected)
	} else {
		qry = `UPDATE tasks SET notice = ?, updated_at = ?
		       WHERE id = ? AND notice IS NULL`
		res, err = q.db.ExecContext(ctx, qry, newNotice, nowUnix(), id)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateTaskStatusConditional 条件更新任务状态：仅当当前 status == fromStatus 时
// 才更新为 toStatus（携带 lastError），返回是否更新成功（RowsAffected=1）。
// 设计依据 design.md §5/§19：状态转移前置检查 → 意图落库；并发操作通过状态条件避免覆盖。
func (q *Queries) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (updated bool, err error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, last_error = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		toStatus, lastError, nowUnix(), id, fromStatus)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// BeginDeleteIntent 原子持久化删除意图：单语句把 delete_mode 与 status=deleting 一起更新，
// 仅当当前 status 处于 fromStatus 之一时生效，返回是否更新成功。
// 设计依据 design.md §12/§19/§8：持久化 delete_mode + 置 deleting 必须原子落库，
// 按持久化 delete_mode 幂等重入 deleting。
func (q *Queries) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (updated bool, err error) {
	if len(fromStatuses) == 0 {
		return false, nil
	}
	// 单语句避免 delete_mode 与 status 之间的部分提交窗口。
	placeholders := make([]string, len(fromStatuses))
	args := make([]any, 0, len(fromStatuses)+4)
	args = append(args, mode, nowUnix(), id)
	for i, s := range fromStatuses {
		placeholders[i] = "?"
		args = append(args, s)
	}
	qry := `UPDATE tasks SET delete_mode = ?, status = 'deleting', updated_at = ?
	        WHERE id = ? AND status IN (` + joinPlaceholders(placeholders) + `)`
	res, err := q.db.ExecContext(ctx, qry, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func joinPlaceholders(p []string) string {
	out := make([]byte, 0, len(p)*2-1+2)
	out = append(out, '?')
	for range p[1:] {
		out = append(out, ',', '?')
	}
	return string(out)
}

// DeleteAbsentSessions 删除任务中不在 keepSet 内的会话归属行。
// 设计依据 design.md §4：仅完整对齐结果（count < limit）可删缺席行。
func (q *Queries) DeleteAbsentSessions(ctx context.Context, taskID string, keepSet []string) error {
	if len(keepSet) == 0 {
		_, err := q.db.ExecContext(ctx,
			`DELETE FROM task_sessions WHERE task_id = ?`, taskID)
		return err
	}
	placeholders := make([]string, len(keepSet))
	args := make([]any, 0, len(keepSet)+1)
	args = append(args, taskID)
	for i, s := range keepSet {
		placeholders[i] = "?"
		args = append(args, s)
	}
	qry := `DELETE FROM task_sessions WHERE task_id = ? AND session_id NOT IN (` + joinPlaceholders(placeholders) + `)`
	_, err := q.db.ExecContext(ctx, qry, args...)
	return err
}

// AlignSessions 全量对齐任务会话归属（design.md §4）：在单事务内 upsert 全部返回项、
// 删除缺席行、更新 notice。仅当 complete=true（count < limit，结果完整）时删除缺席行；
// complete=false（可能截断）时 MUST NOT 删除（仅 upsert + notice），由调用方写入
// session_overflow notice，complete 恢复时由调用方清除该 notice。
// noticeFn 在事务内对最新 notice 做最终写入（读取当前行后计算），保证对齐与 notice 原子提交。
//
// 若 q 已绑定 *sql.Tx（在 WithTx 回调内调用），直接复用该事务；否则自动开启事务。
func (q *Queries) AlignSessions(ctx context.Context, taskID string, sessions []SessionRow, complete bool, noticeFn func(current sql.NullString) sql.NullString) error {
	if _, isTx := q.db.(*sql.Tx); isTx {
		return q.alignSessionsInTx(ctx, taskID, sessions, complete, noticeFn)
	}
	return withTxQueries(ctx, q.db, func(qtx *Queries) error {
		return qtx.alignSessionsInTx(ctx, taskID, sessions, complete, noticeFn)
	})
}

// alignSessionsInTx 在已绑定的事务内执行对齐逻辑，供 AlignSessions 的事务与非事务两条路径复用。
func (q *Queries) alignSessionsInTx(ctx context.Context, taskID string, sessions []SessionRow, complete bool, noticeFn func(sql.NullString) sql.NullString) error {
	for _, s := range sessions {
		if err := q.UpsertTaskSession(ctx, s); err != nil {
			return err
		}
	}
	if complete {
		keep := make([]string, len(sessions))
		for i, s := range sessions {
			keep[i] = s.SessionID
		}
		if err := q.DeleteAbsentSessions(ctx, taskID, keep); err != nil {
			return err
		}
	}
	if noticeFn != nil {
		row := q.db.QueryRowContext(ctx, `SELECT notice FROM tasks WHERE id = ?`, taskID)
		var current sql.NullString
		if err := row.Scan(&current); err != nil {
			return err
		}
		next := noticeFn(current)
		// notice 在对齐事务内整体覆盖（非 CAS：对齐是单写者事务，外部并发由 keyed mutex 串行）。
		if _, err := q.db.ExecContext(ctx,
			`UPDATE tasks SET notice = ?, updated_at = ? WHERE id = ?`, next, nowUnix(), taskID); err != nil {
			return err
		}
	}
	return nil
}

// withTxQueries 使用 db（*sql.DB）开启事务并在回调中提供绑定该事务的 Queries。
// 供未持有 DB 句柄的方法（如 AlignSessions 在 *sql.DB 绑定时）自动落事务。
func withTxQueries(ctx context.Context, db DBTX, fn func(qtx *Queries) error) error {
	sqlDB, ok := db.(*sql.DB)
	if !ok {
		// 不应发生：已处理 *sql.Tx 分支；防御性返回。
		return errNoDBForTx
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmtBeginTx(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmtCommitTx(err)
	}
	committed = true
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTaskRow(row rowScanner) (TaskRow, error) {
	var t TaskRow
	err := row.Scan(&t.ID, &t.ProjectID, &t.Name, &t.Branch, &t.Status, &t.WorktreePath,
		&t.LastPort, &t.LastError, &t.Notice, &t.DeleteMode, &t.EnvSnapshot,
		&t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt)
	return t, err
}

func scanTaskRows(rows *sql.Rows) ([]TaskRow, error) {
	var out []TaskRow
	for rows.Next() {
		t, err := scanTaskRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- cleanup_debts（orphan ticket 持久化，design.md §10） ---

// CleanupDebtRow cleanup_debts 表行映射。
type CleanupDebtRow struct {
	SessionName string
	Tickets     string // JSON 编码 []string
	CreatedAt   int64
}

// UpsertCleanupDebt 插入或按 session_name 原地替换未收敛 orphan tickets（最新聚合 wins，design.md §10）。
func (q *Queries) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	_, err := q.db.ExecContext(ctx,
		"INSERT INTO cleanup_debts (session_name, tickets, created_at) VALUES (?, ?, ?) "+
			"ON CONFLICT(session_name) DO UPDATE SET tickets = excluded.tickets, created_at = excluded.created_at",
		sessionName, ticketsJSON, createdAt)
	return err
}

// DeleteCleanupDebt 删除已收敛的 orphan cleanup debt（tickets 全部 reaped 后清除，design.md §10）。
func (q *Queries) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	_, err := q.db.ExecContext(ctx, "DELETE FROM cleanup_debts WHERE session_name = ?", sessionName)
	return err
}

// ListCleanupDebts 枚举全部未收敛 orphan cleanup debt（Reconcile 启动恢复重试用，design.md §10）。
func (q *Queries) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	rows, err := q.db.QueryContext(ctx, "SELECT session_name, tickets, created_at FROM cleanup_debts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanupDebtRow
	for rows.Next() {
		var r CleanupDebtRow
		if err := rows.Scan(&r.SessionName, &r.Tickets, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// errNoDBForTx 表示 DBTX 既非 *sql.Tx 也非 *sql.DB，无法开启/复用事务。
var errNoDBForTx = fmt.Errorf("store: cannot begin transaction on non-DB/DBTX")

func fmtBeginTx(err error) error  { return fmt.Errorf("begin tx: %w", err) }
func fmtCommitTx(err error) error { return fmt.Errorf("commit tx: %w", err) }
