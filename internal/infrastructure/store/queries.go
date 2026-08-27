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

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
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
// Kind ∈ repo | dir（migration 0008，add-plain-dir-project D1）：repo 为 git 仓库，dir 为纯目录。
type ProjectRow struct {
	ID            string
	Name          string
	Path          string
	DefaultBranch string
	Kind          string
	CreatedAt     int64
}

// TaskRow tasks 表行映射。env_snapshot 为激活时合并的 env JSON（design.md §2/§8）。
// InitStatus/InitError 对应 migration 0007 新增列（design.md §2 项目生命周期配置）：
// init_status ∈ none | pending | running | succeeded | failed；init_error 仅 failed 时非空。
// BaseRef 对应 migration 0008 新增列（add-plain-dir-project D10）：repo 任务的基线分支全引用
// （如 refs/heads/main），dir 项目任务为空串。
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
	InitStatus       string
	InitError        sql.NullString
	BaseRef          string
	AnchorSessionID  sql.NullString
}

// LifecycleConfigRow project_lifecycle_configs 表行映射（design.md §2，migration 0007）。
// 缺行读取时三脚本字段为空串（无配置 = 空配置语义）。
type LifecycleConfigRow struct {
	ProjectID       string
	InheritPatterns string
	InitScript      string
	PreDeleteScript string
	UpdatedAt       int64
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

// nowUnix 返回当前 Unix 时间戳（秒精度）。包级 var，测试可临时覆盖以验证跨秒行为
// （task P1.2 validation：跨秒用可注入时间或 mock nowUnix）。
var nowUnix = func() int64 { return time.Now().Unix() }

// beforeConditionalUpdateHook 供 F-01 真交错测试在 updateTaskStatus 的内部 SELECT 完成后、
// UPDATE 执行前暂停调用方 goroutine（生产为 nil 不调用，零开销）。测试注入 channel 同步：
// A 调用方法 → SELECT 读旧值 → 触发 hook 阻塞 → 测试经独立连接完成 B 写入 → 放行 hook →
// A 的 UPDATE WHERE 在 B 写后的当前行上重新求值，断言不覆盖 B。
var beforeConditionalUpdateHook func(taskID string)

// afterConditionalUpdateReadHook 供 F-01 真交错测试在 updateTaskStatus 的内部 SELECT 完成
// 后立即通知测试「A 已读到旧值，可放行 B 写入」。配合 beforeConditionalUpdateHook 使用：
// 测试等此信号 → B 写入 → 关闭 beforeConditionalUpdateHook 的阻塞 → A 继续 UPDATE。
var afterConditionalUpdateReadHook func(taskID string)

// CreateProject 插入项目行。kind ∈ repo | dir（migration 0008）。
func (q *Queries) CreateProject(ctx context.Context, id, name, path, defaultBranch, kind string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, path, default_branch, kind, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, path, defaultBranch, kind, nowUnix())
	return err
}

// GetProject 按 ID 查询项目。
func (q *Queries) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, name, path, default_branch, kind, created_at FROM projects WHERE id = ?`, id)
	var p ProjectRow
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.Kind, &p.CreatedAt)
	return p, err
}

// GetProjectByPath 按 path 查询项目（path 唯一）。
func (q *Queries) GetProjectByPath(ctx context.Context, path string) (ProjectRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, name, path, default_branch, kind, created_at FROM projects WHERE path = ?`, path)
	var p ProjectRow
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.Kind, &p.CreatedAt)
	return p, err
}

// ListProjects 返回全部项目。
func (q *Queries) ListProjects(ctx context.Context) ([]ProjectRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, name, path, default_branch, kind, created_at FROM projects ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultBranch, &p.Kind, &p.CreatedAt); err != nil {
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

// GetLifecycleConfig 读取项目生命周期配置（design.md §2，migration 0007）。
// 缺行返回空配置（三脚本字段空串），非错误：无配置项目无行，读时缺行 = 空配置语义。
func (q *Queries) GetLifecycleConfig(ctx context.Context, projectID string) (LifecycleConfigRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT project_id, inherit_patterns, init_script, pre_delete_script, updated_at
		 FROM project_lifecycle_configs WHERE project_id = ?`, projectID)
	var c LifecycleConfigRow
	err := row.Scan(&c.ProjectID, &c.InheritPatterns, &c.InitScript, &c.PreDeleteScript, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		// 缺行 = 空配置（非错误），保留 ProjectID 供调用方定位。
		return LifecycleConfigRow{ProjectID: projectID}, nil
	}
	return c, err
}

// UpsertLifecycleConfig 整体替换项目生命周期配置（design.md §2.1，INSERT … ON CONFLICT）。
// 刷新 updated_at；调用方传入的 updatedAt 由本方法置为当前时间戳。
func (q *Queries) UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO project_lifecycle_configs (project_id, inherit_patterns, init_script, pre_delete_script, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET
		   inherit_patterns = excluded.inherit_patterns,
		   init_script = excluded.init_script,
		   pre_delete_script = excluded.pre_delete_script,
		   updated_at = excluded.updated_at`,
		projectID, inheritPatterns, initScript, preDeleteScript, nowUnix())
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

// CreateTask 插入任务行。base_ref 为 repo 任务的基线分支全引用，dir 项目任务传空串
// （migration 0008，add-plain-dir-project D10）。
func (q *Queries) CreateTask(ctx context.Context, t TaskRow) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path, base_ref, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Name, t.Branch, t.Status, t.WorktreePath, t.BaseRef, nowUnix(), nowUnix())
	return err
}

// GetTask 按 ID 查询任务（含 env_snapshot、init_status/init_error、base_ref）。
func (q *Queries) GetTask(ctx context.Context, id string) (TaskRow, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, project_id, name, branch, status, worktree_path, last_port, last_error, notice,
		        delete_mode, env_snapshot, created_at, updated_at, archived_at, init_status, init_error, base_ref,
		        anchor_session_id
		 FROM tasks WHERE id = ?`, id)
	return scanTaskRow(row)
}

// ListTasksByProject 列出某项目的全部任务。
func (q *Queries) ListTasksByProject(ctx context.Context, projectID string) ([]TaskRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, project_id, name, branch, status, worktree_path, last_port, last_error, notice,
		        delete_mode, env_snapshot, created_at, updated_at, archived_at, init_status, init_error, base_ref,
		        anchor_session_id
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
		        delete_mode, env_snapshot, created_at, updated_at, archived_at, init_status, init_error, base_ref,
		        anchor_session_id
		 FROM tasks ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRows(rows)
}

// ActiveTaskOverviewRow 跨项目 active 任务概览投影行（cross-project-active-sessions D2）。
// 仅供 GET /api/v1/tasks/active 读模型：不含 status/init 等详情字段，不携带 agentStatus。
// last_active_at 为 MAX(task_sessions.last_seen_at)，无 session 时回退 t.updated_at。
type ActiveTaskOverviewRow struct {
	ID           string
	ProjectID    string
	ProjectName  string
	Name         string
	Branch       string
	WorktreePath string
	LastActiveAt int64
}

// ListActiveTaskOverview 聚合全部 active 任务的跨项目概览（cross-project-active-sessions D2）。
// JOIN projects 取项目名；LEFT JOIN task_sessions 以 MAX(last_seen_at) 为最近活跃时间，
// 无 session 回退 t.updated_at；按 last_active_at DESC、t.id ASC 排序。
//
// 单位归一化（实测确认）：task_sessions.last_seen_at 持久化 opencode time.updated
// 为毫秒（≈1.78e12，如 1785797826297）；tasks.updated_at 为 nowUnix() 秒（≈1.78e9）。
// 逐行 CASE 归一化（阈值 1e11）：≥1e11 视为毫秒 ÷1000 取整，否则按秒原值；在 MAX 之前完成，
// 读侧兼容存量与新写入数据，不触碰写路径。
func (q *Queries) ListActiveTaskOverview(ctx context.Context) ([]ActiveTaskOverviewRow, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT t.id, t.project_id, p.name AS project_name, t.name, t.branch, t.worktree_path,
		        COALESCE(
		          MAX(CASE
		            WHEN s.last_seen_at >= 100000000000
		            THEN CAST(s.last_seen_at / 1000 AS INTEGER)
		            ELSE s.last_seen_at
		          END),
		          t.updated_at
		        ) AS last_active_at
		 FROM tasks t
		 JOIN projects p ON p.id = t.project_id
		 LEFT JOIN task_sessions s ON s.task_id = t.id
		 WHERE t.status = 'active'
		 GROUP BY t.id
		 ORDER BY last_active_at DESC, t.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveTaskOverviewRow
	for rows.Next() {
		var r ActiveTaskOverviewRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.Name, &r.Branch, &r.WorktreePath, &r.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateTaskStatus 更新任务状态与 last_error，返回结构化结果（design.md D0/§8）。
//
// 同值口径覆盖 status+last_error（spec.md Scenario：status-same-last_error-different
// 仍提交）。同值排除下推到 UPDATE WHERE（NULL-safe IS/IS NOT），RowsAffected 区分
// 同值（0 行，行存在）与不匹配（0 行，行不存在）；updated_at 仅真实变更且跨秒推进。
func (q *Queries) UpdateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return q.updateTaskStatus(ctx, id, status, lastError, false, "")
}

// UpdateTaskStatusConditional 条件更新任务状态：仅当当前 status==fromStatus 时才更新。
//
// 返回 TransitionResult：Matched=当前 status==fromStatus；同值口径 status+last_error
// （status-same-last_error-different 仍提交）。StatusChanged 仅 status 真实迁移时为 true。
// 设计依据 design.md §5/§19：状态转移前置检查 → 意图落库；并发操作通过状态条件避免覆盖。
func (q *Queries) UpdateTaskStatusConditional(ctx context.Context, id string, fromStatus, toStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return q.updateTaskStatus(ctx, id, toStatus, lastError, true, string(fromStatus))
}

// updateTaskStatus 是 UpdateTaskStatus 与 UpdateTaskStatusConditional 的共享实现。
//
// conditional=true 时施加 fromStatus CAS：UPDATE 的 WHERE 携带 `status IS fromStatus`
// （expected 谓词，NULL-safe，F-01 下推）+ status/last_error 的同值排除（任一列不同才匹配）。
// conditional=false 时无 expected 谓词，仅 id 命中 + 同值排除。
//
// RowsAffected=0 分类（同一事务内补 SELECT）：行存在且（conditional 时 status==fromStatus）
// 且全部业务列==新值 → Matched+!Changed（同值幂等）；行存在但 conditional status!=fromStatus
// 或行不存在 → !Matched（CAS 失败）。StatusChanged/From/To 由事务内读到的实际 old/new 计算（F-05）。
// updated_at 仅跨秒推进（同秒实变 SET updated_at=updated_at）。
//
// beforeConditionalUpdateHook/afterConditionalUpdateReadHook 供 F-01 真交错测试在内部 SELECT
// 与 UPDATE 之间暂停调用方（生产 nil 不调用）。
func (q *Queries) updateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string,
	conditional bool, fromStatus string,
) (application.TransitionResult, error) {
	newLE := nullableString(lastError)
	// F-01 真交错测试钩子路径：当 hooks 注入时，SELECT 与 UPDATE 之间会暂停让 B 写入。
	// SQLite WAL 下事务内 SELECT 持快照锁，B 写入后 A 的 UPDATE 会 SQLITE_LOCKED（快照过期）。
	// 测试路径改用非事务 SELECT+独立原子 UPDATE，让 B 写入不阻塞、A 的 UPDATE WHERE 在 B 写后
	// 当前行上重新求值。生产无 hooks 时仍走 runTx 事务路径（原子性保证）。
	if beforeConditionalUpdateHook != nil || afterConditionalUpdateReadHook != nil {
		return q.updateTaskStatusInterleaved(ctx, id, status, newLE, conditional, fromStatus)
	}
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		// 读旧值（status/last_error/updated_at）用于 CAS 判定与 From/To 计算。
		row := qx.db.QueryRowContext(ctx,
			`SELECT status, last_error, updated_at FROM tasks WHERE id = ?`, id)
		var curStatus string
		var curLastError sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curLastError, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		// 构造 UPDATE：WHERE 携带 id + （conditional 时 status IS fromStatus expected）+ 同值排除（任一列不同才匹配，F-01 原子）。
		diffPred, diffArgs := anyColDiffersPredicate(
			[]string{"status", "last_error"},
			[]sql.NullString{{String: string(status), Valid: true}, newLE})
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		var expFrag string
		var expArgs []any
		if conditional {
			expFrag = " AND status IS ?"
			expArgs = []any{fromStatus}
		}
		qry := "UPDATE tasks SET status = ?, last_error = ?, " + updClause +
			" WHERE id = ?" + expFrag + " AND (" + diffPred + ")"
		args := []any{string(status), newLE}
		args = append(args, updArgs...)
		args = append(args, id)
		args = append(args, expArgs...)
		args = append(args, diffArgs...)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			// RowsAffected=0 分类：在同一事务内补 SELECT 区分 expected 失配/行不存在/同值幂等。
			return qx.classifyZeroRowsStatus(ctx, id, conditional, fromStatus, status, newLE)
		}
		statusChanged := curStatus != string(status)
		out := application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  statusChanged,
		}
		if statusChanged {
			out.From = ocdecktask.Status(curStatus)
			out.To = status
		}
		return out, nil
	})
}

// updateTaskStatusInterleaved 是 F-01 真交错测试专用路径（hooks 注入时）：
// SELECT 读旧值后触发 hooks 暂停，独立连接 B 可写入（WAL 下读不阻塞写），放行后 A 执行
// 独立原子 UPDATE（非事务，避免 WAL 快照过期 SQLITE_LOCKED）。UPDATE WHERE 携带 expected
// + 同值排除，在 B 写后行上重新求值；RowsAffected=0 用 classifyZeroRowsStatus 分类。
func (q *Queries) updateTaskStatusInterleaved(ctx context.Context, id string, status ocdecktask.Status, newLE sql.NullString,
	conditional bool, fromStatus string,
) (application.TransitionResult, error) {
	// 读旧值（非事务，不持快照锁）。
	row := q.db.QueryRowContext(ctx,
		`SELECT status, last_error, updated_at FROM tasks WHERE id = ?`, id)
	var curStatus string
	var curLastError sql.NullString
	var curUpdatedAt int64
	if err := row.Scan(&curStatus, &curLastError, &curUpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return application.TransitionResult{}, nil
		}
		return application.TransitionResult{}, err
	}
	// 触发 hooks：afterRead 通知测试「A 已读旧值」，beforeHook 阻塞等 B 写完。
	if afterConditionalUpdateReadHook != nil {
		afterConditionalUpdateReadHook(id)
	}
	if beforeConditionalUpdateHook != nil {
		beforeConditionalUpdateHook(id)
	}
	// 独立原子 UPDATE（非事务）：WHERE 携带 expected + 同值排除。
	diffPred, diffArgs := anyColDiffersPredicate(
		[]string{"status", "last_error"},
		[]sql.NullString{{String: string(status), Valid: true}, newLE})
	now := nowUnix()
	updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
	var expFrag string
	var expArgs []any
	if conditional {
		expFrag = " AND status IS ?"
		expArgs = []any{fromStatus}
	}
	qry := "UPDATE tasks SET status = ?, last_error = ?, " + updClause +
		" WHERE id = ?" + expFrag + " AND (" + diffPred + ")"
	args := []any{string(status), newLE}
	args = append(args, updArgs...)
	args = append(args, id)
	args = append(args, expArgs...)
	args = append(args, diffArgs...)
	res, err := q.db.ExecContext(ctx, qry, args...)
	if err != nil {
		return application.TransitionResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return application.TransitionResult{}, err
	}
	if n == 0 {
		// RowsAffected=0 分类（非事务补 SELECT）。
		return q.classifyZeroRowsStatus(ctx, id, conditional, fromStatus, status, newLE)
	}
	statusChanged := curStatus != string(status)
	out := application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
		StatusChanged:  statusChanged,
	}
	if statusChanged {
		out.From = ocdecktask.Status(curStatus)
		out.To = status
	}
	return out, nil
}

// classifyZeroRowsStatus 在 UPDATE RowsAffected=0 后于同一事务内补 SELECT，区分：
//   - 行不存在 → !Matched（TransitionResult{}）
//   - conditional 且 status!=fromStatus → !Matched（CAS expected 失配）
//   - status==（conditional?fromStatus:当前）且 status+last_error 全等于新值 → Matched+!Changed（同值幂等）
//   - 其他（行存在、expected 匹配、但并发下业务列已被他写为非同值且 WHERE 未命中）→ !Matched
//
// 读到的 status/last_error 为 UPDATE 不命中后的当前行实际值（B 写后），据此判定。
func (q *Queries) classifyZeroRowsStatus(ctx context.Context, id string, conditional bool, fromStatus string,
	newStatus ocdecktask.Status, newLE sql.NullString,
) (application.TransitionResult, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT status, last_error FROM tasks WHERE id = ?`, id)
	var curStatus string
	var curLastError sql.NullString
	if err := row.Scan(&curStatus, &curLastError); err != nil {
		if err == sql.ErrNoRows {
			// 行不存在 → !Matched。
			return application.TransitionResult{}, nil
		}
		return application.TransitionResult{}, err
	}
	if conditional && curStatus != fromStatus {
		// expected 失配（status 已被并发改为非 fromStatus）→ !Matched。
		return application.TransitionResult{}, nil
	}
	// 行存在且 expected 匹配：判定是否同值幂等（status+last_error 全等于新值）。
	statusSame := curStatus == string(newStatus)
	leSame := nullStringEqual(curLastError, newLE)
	if statusSame && leSame {
		return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
	}
	// 行存在、expected 匹配，但业务列非全同值且 UPDATE 未命中（罕见：并发写入使 WHERE 谓词在
	// SELECT 与 UPDATE 间失配）→ 按 !Matched 上报，调用方重读决策。
	return application.TransitionResult{}, nil
}

// UpdateTaskEnvSnapshot 更新 env_snapshot（design.md §2，激活时持久化），返回结构化结果。
func (q *Queries) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (application.MutationResult, error) {
	return q.updateSingleNullableCol(ctx, id, "env_snapshot", envSnapshot)
}

// UpdateTaskLastPort 更新 last_port（仅记录上次成功端口，非事实来源，design.md §3）。
//
// 同值排除下推到 UPDATE WHERE（last_port IS NOT ?，F-01 原子）；事务内读 updated_at 旧值。
func (q *Queries) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT last_port, updated_at FROM tasks WHERE id = ?`, id)
		var curPort sql.NullInt64
		var curUpdatedAt int64
		if err := row.Scan(&curPort, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		if nullInt64Equal(curPort, sql.NullInt64{Int64: int64(port), Valid: true}) {
			return application.MutationResult{Matched: true}, nil
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		// last_port 非 NULL 列但用 IS NOT 统一（新值非 NULL 时 `last_port IS NOT ?`）。
		qry := "UPDATE tasks SET last_port = ?, " + updClause + " WHERE id = ? AND last_port IS NOT ?"
		args := []any{port}
		args = append(args, updArgs...)
		args = append(args, id, port)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.MutationResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.MutationResult{}, err
		}
		if n == 0 {
			return application.MutationResult{Matched: true}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// UpdateTaskNotice 更新 notice JSON 数组（design.md §8），返回结构化结果。
func (q *Queries) UpdateTaskNotice(ctx context.Context, id string, notice *string) (application.MutationResult, error) {
	return q.updateSingleNullableCol(ctx, id, "notice", notice)
}

// SetTaskDeleteMode 持久化 delete_mode（design.md §8/§19），返回结构化结果。
//
// 同值排除下推到 UPDATE WHERE（delete_mode IS NOT ?，F-01 原子）；事务内读 updated_at 旧值。
func (q *Queries) SetTaskDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT delete_mode, updated_at FROM tasks WHERE id = ?`, id)
		var curMode sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curMode, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		newMode := sql.NullString{String: string(mode), Valid: true}
		if nullStringEqual(curMode, newMode) {
			return application.MutationResult{Matched: true}, nil
		}
		modePred, modeArg := colNotEqualPredicate("delete_mode", newMode)
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET delete_mode = ?, " + updClause + " WHERE id = ? AND " + modePred
		args := []any{newMode}
		args = append(args, updArgs...)
		args = append(args, id)
		if modeArg != nil {
			args = append(args, modeArg)
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
			return application.MutationResult{Matched: true}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// ArchiveTask 置 archived 状态并记录 archived_at，返回结构化结果。
//
// 同值排除下推到 UPDATE WHERE（status IS NOT 'archived'，F-01 原子）；StatusChanged/From/To
// 由事务内读到的实际 old/new 计算（F-05），不得硬编码。updated_at 仅跨秒推进。
func (q *Queries) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT status, updated_at FROM tasks WHERE id = ?`, id)
		var curStatus string
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		if curStatus == string(ocdecktask.StatusArchived) {
			// 已 archived：同值，不写，updated_at 不动。
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET status = 'archived', archived_at = ?, " + updClause +
			" WHERE id = ? AND status IS NOT 'archived'"
		args := []any{now}
		args = append(args, updArgs...)
		args = append(args, id)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
		}
		return application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  true,
			From:           ocdecktask.Status(curStatus),
			To:             ocdecktask.StatusArchived,
		}, nil
	})
}

// RestoreTask 从 archived 恢复到 suspended，返回结构化结果。
//
// 同值排除下推到 UPDATE WHERE（status IS NOT 'suspended'，F-01 原子）；StatusChanged/From/To
// 由事务内读到的实际 old/new 计算（F-05）。updated_at 仅跨秒推进。
func (q *Queries) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT status, updated_at FROM tasks WHERE id = ?`, id)
		var curStatus string
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		if curStatus == string(ocdecktask.StatusSuspended) {
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET status = 'suspended', " + updClause +
			" WHERE id = ? AND status IS NOT 'suspended'"
		args := []any{}
		args = append(args, updArgs...)
		args = append(args, id)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
		}
		return application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  true,
			From:           ocdecktask.Status(curStatus),
			To:             ocdecktask.StatusSuspended,
		}, nil
	})
}

// DeleteTask 按 ID 删除任务（CASCADE 删除其 sessions/env_vars），返回结构化结果。
//
// 同事务先捕获剩余 session ID 与任务前态再删除（design D2:337）。
func (q *Queries) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	if _, isTx := q.db.(*sql.Tx); isTx {
		return q.deleteTaskInTx(ctx, id)
	}
	var r application.DeleteResult
	txErr := withTxQueries(ctx, q.db, func(qtx *Queries) error {
		var cerr error
		r, cerr = qtx.deleteTaskInTx(ctx, id)
		return cerr
	})
	return r, txErr
}

func (q *Queries) deleteTaskInTx(ctx context.Context, id string) (application.DeleteResult, error) {
	row := q.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id)
	var fromStatus string
	if err := row.Scan(&fromStatus); err != nil {
		if err == sql.ErrNoRows {
			return application.DeleteResult{}, nil
		}
		return application.DeleteResult{}, err
	}
	srows, err := q.db.QueryContext(ctx, `SELECT session_id FROM task_sessions WHERE task_id = ?`, id)
	if err != nil {
		return application.DeleteResult{}, err
	}
	var cascaded []string
	for srows.Next() {
		var sid string
		if err := srows.Scan(&sid); err != nil {
			srows.Close()
			return application.DeleteResult{}, err
		}
		cascaded = append(cascaded, sid)
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return application.DeleteResult{}, err
	}
	srows.Close()
	res, err := q.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return application.DeleteResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return application.DeleteResult{}, err
	}
	return application.DeleteResult{
		Affected:           int(n),
		From:               ocdecktask.Status(fromStatus),
		CascadedSessionIDs: cascaded,
	}, nil
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

// DeleteTaskSession 删除会话归属行，返回受影响行数（0=行不存在，幂等成功）。
func (q *Queries) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM task_sessions WHERE task_id = ? AND session_id = ?`, taskID, sessionID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// UpdateTaskNoticeCAS 乐观更新 notice（CAS）：仅当当前 notice 等于 expected 时才考虑写入。
//
// 返回 MutationResult：Matched=当前 notice==expected；Changed=newNotice!=expected
// （同值幂等成功 Matched+!Changed，不得误判为失败重试）；UpdatedAtAdvanced=真实变更且跨秒。
// expected/newNotice 为 *string：nil 表示 NULL 期望/新值。
// 设计依据 design.md §5/§8：notice 更新 MUST 为 CAS/事务，避免 Delete/Suspend/SSE 与
// 后台重试的 notice 写互相覆盖。
//
// F-01 原子性：expected 与同值排除都下推到 UPDATE 的 WHERE（notice IS <expected> AND
// notice IS NOT <new>），RowsAffected 区分异值（1 行）与不匹配/同值（0 行）；事务内读
// updated_at 旧值以判定 UpdatedAtAdvanced。
func (q *Queries) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	expNS := nullableString(expected)
	newNS := nullableString(newNotice)
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT notice, updated_at FROM tasks WHERE id = ?`, id)
		var curNotice sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curNotice, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		if !nullStringEqual(curNotice, expNS) {
			// expected 不匹配：CAS 失败，调用方重试。
			return application.MutationResult{}, nil
		}
		if nullStringEqual(curNotice, newNS) {
			// 同值幂等成功：Matched+!Changed，不写。
			return application.MutationResult{Matched: true}, nil
		}
		// UPDATE WHERE 携带 expected（notice IS <expected>）与同值排除（notice IS NOT <new>）。
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		expPred, expArg := expectedPredicate("notice", expNS)
		newPred, newArg := colNotEqualPredicate("notice", newNS)
		qry := "UPDATE tasks SET notice = ?, " + updClause +
			" WHERE id = ? AND " + expPred + " AND " + newPred
		args := []any{newNS}
		args = append(args, updArgs...)
		args = append(args, id)
		if expArg != nil {
			args = append(args, expArg)
		}
		if newArg != nil {
			args = append(args, newArg)
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
			// 并发下 expected 已失配：CAS 失败，调用方重试（不误判为同值成功）。
			return application.MutationResult{}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// BeginDeleteIntent 原子持久化删除意图：单语句把 delete_mode 与 status=deleting 一起更新，
// 仅当当前 status 处于 fromStatus 之一时生效。
//
// 返回 TransitionResult：Matched=当前 status ∈ fromStatuses；status 迁移到 deleting。
// StatusChanged/From/To 由事务内读到的实际 old/new 计算（F-05，不得硬编码：若调用方
// 误传含 deleting 的 fromStatuses，当前已 deleting 则同值 Matched+!Changed）。delete_mode
// 同值不影响 Changed（status 已变时）。设计依据 design.md §12/§19/§8。F-01：expected（status
// ∈ fromStatuses）与同值排除（status IS NOT 'deleting'）下推到 UPDATE WHERE。
func (q *Queries) BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (application.TransitionResult, error) {
	if len(fromStatuses) == 0 {
		return application.TransitionResult{}, nil
	}
	// 预构 WHERE 的 status IN(...) 与参数（expected 条件）。
	phs := make([]string, len(fromStatuses))
	expArgs := make([]any, 0, len(fromStatuses))
	for i, s := range fromStatuses {
		phs[i] = "?"
		expArgs = append(expArgs, string(s))
	}
	statusIn := "status IN (" + joinPlaceholders(phs) + ")"
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		row := qx.db.QueryRowContext(ctx, `SELECT status, delete_mode, updated_at FROM tasks WHERE id = ?`, id)
		var curStatus string
		var curMode sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curMode, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		matched := false
		for _, s := range fromStatuses {
			if curStatus == string(s) {
				matched = true
				break
			}
		}
		if !matched {
			return application.TransitionResult{}, nil
		}
		newMode := sql.NullString{String: string(mode), Valid: true}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		// 同值排除（任一列不同才匹配，F-01）：status 目标 'deleting' + delete_mode 目标 mode。
		diffPred, diffArgs := anyColDiffersPredicate(
			[]string{"status", "delete_mode"},
			[]sql.NullString{{String: string(ocdecktask.StatusDeleting), Valid: true}, newMode})
		// WHERE: id + status IN fromStatuses（expected）+ 同值排除。
		qry := "UPDATE tasks SET delete_mode = ?, status = 'deleting', " + updClause +
			" WHERE id = ? AND " + statusIn + " AND (" + diffPred + ")"
		args := []any{newMode}
		args = append(args, updArgs...)
		args = append(args, id)
		args = append(args, expArgs...)
		args = append(args, diffArgs...)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			// 并发下 status 已迁出 fromStatuses 或已 deleting：CAS 失败。
			return application.TransitionResult{}, nil
		}
		statusChanged := curStatus != string(ocdecktask.StatusDeleting)
		out := application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  statusChanged,
		}
		if statusChanged {
			out.From = ocdecktask.Status(curStatus)
			out.To = ocdecktask.StatusDeleting
		}
		return out, nil
	})
}

func joinPlaceholders(p []string) string {
	out := make([]byte, 0, len(p)*2-1+2)
	out = append(out, '?')
	for range p[1:] {
		out = append(out, ',', '?')
	}
	return string(out)
}

// --- 项目生命周期配置 CAS（design.md §2.1，migration 0007） ---
//
// 以下方法全部 CAS / 原子：条件不满足时返回 Matched=false（非 error）。
// 同值口径：init 系列 = init_status+init_error；CommitCreated 含 status 迁移。
// updated_at 仅真实变更且跨秒推进。

// CommitCreated 原子提交 Create/retryCreate 的最终状态：把 status 从 expectedStatus
// 置为 suspended 并写入 initStatus，清空 last_error（design.md §2.1/§3）。
// expectedStatus ∈ {creating, creation_failed}；initStatus ∈ {pending, none}。
//
// 返回 TransitionResult：Matched=当前 status==expectedStatus；同值口径 status+last_error+init_status
// （全部写列）。StatusChanged/From/To 由事务内读到的实际 old/new 计算（F-05，不得硬编码：
// 若调用方误传 suspended 作 expectedStatus，当前已 suspended 则同值 Matched+!Changed）。
// F-01：expected（status IS expectedStatus）与同值排除（status IS NOT 'suspended' 等）下推到 UPDATE WHERE。
func (q *Queries) CommitCreated(ctx context.Context, taskID string, expectedStatus ocdecktask.Status, initStatus ocdecktask.InitStatus) (application.TransitionResult, error) {
	expStatusNS := sql.NullString{String: string(expectedStatus), Valid: true}
	newInitNS := sql.NullString{String: string(initStatus), Valid: true}
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT status, init_status, last_error, updated_at FROM tasks WHERE id = ?`, taskID)
		var curStatus, curInitStatus string
		var curLastError sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curInitStatus, &curLastError, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		if curStatus != string(expectedStatus) {
			// expected 不匹配：CAS 失败。
			return application.TransitionResult{}, nil
		}
		statusChanged := curStatus != string(ocdecktask.StatusSuspended)
		leChanged := curLastError.Valid // 新值 NULL，旧值非 NULL 即变更
		initChanged := curInitStatus != string(initStatus)
		changed := statusChanged || leChanged || initChanged
		if !changed {
			// 全列同值：不写。
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true}}, nil
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		// WHERE: id + status IS expectedStatus（expected）+ 同值排除（任一列不同才匹配，F-01）。
		// 同值口径 status+last_error+init_status：全部等于新值才视为同值行排除。
		diffPred, diffArgs := anyColDiffersPredicate(
			[]string{"status", "last_error", "init_status"},
			[]sql.NullString{
				{String: string(ocdecktask.StatusSuspended), Valid: true},
				{}, // 新值 NULL
				newInitNS,
			})
		qry := "UPDATE tasks SET status = 'suspended', last_error = NULL, init_status = ?, " + updClause +
			" WHERE id = ? AND status IS ? AND (" + diffPred + ")"
		args := []any{initStatus}
		args = append(args, updArgs...)
		args = append(args, taskID, expStatusNS)
		args = append(args, diffArgs...)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			// 并发下 expected 已失配：CAS 失败。
			return application.TransitionResult{}, nil
		}
		out := application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  statusChanged,
		}
		if statusChanged {
			out.From = ocdecktask.Status(curStatus)
			out.To = ocdecktask.StatusSuspended
		}
		return out, nil
	})
}

// ClaimInitRun 置 init_status=running，要求 status=suspended 且 init_status=pending
// （design.md §2.1/§3：InitRunner 初次执行 claim）。
// 返回 MutationResult：Matched=满足 CAS；init_status pending→running 为真实变更。
// F-01：expected（status='suspended' AND init_status='pending'）与同值排除
// （init_status IS NOT 'running'）下推到 UPDATE WHERE；事务内读 updated_at 旧值。
func (q *Queries) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT init_status, updated_at FROM tasks WHERE id = ? AND status = 'suspended' AND init_status = 'pending'`,
			taskID)
		var curInit string
		var curUpdatedAt int64
		if err := row.Scan(&curInit, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET init_status = 'running', " + updClause +
			" WHERE id = ? AND status = 'suspended' AND init_status = 'pending' AND init_status IS NOT 'running'"
		args := []any{}
		args = append(args, updArgs...)
		args = append(args, taskID)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.MutationResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.MutationResult{}, err
		}
		if n == 0 {
			// 并发下 CAS 已失配。
			return application.MutationResult{}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// ClaimInitRerun 置 init_status=running 并清空旧 init_error，要求 status=suspended 且
// init_status ∈ {failed, succeeded}（design.md §2.1/§3：RerunInit claim）。
// 返回 MutationResult：Matched=满足 CAS；init_status 迁移到 running（非 failed/succeeded）为真实变更。
// F-01：expected（status='suspended' AND init_status IN {failed,succeeded}）与同值排除
// （init_status IS NOT 'running'）下推到 UPDATE WHERE；事务内读 updated_at 旧值。
func (q *Queries) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT init_status, init_error, updated_at FROM tasks
			 WHERE id = ? AND status = 'suspended' AND init_status IN ('failed', 'succeeded')`,
			taskID)
		var curInit string
		var curInitError sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curInit, &curInitError, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET init_status = 'running', init_error = NULL, " + updClause +
			" WHERE id = ? AND status = 'suspended' AND init_status IN ('failed', 'succeeded') AND init_status IS NOT 'running'"
		args := []any{}
		args = append(args, updArgs...)
		args = append(args, taskID)
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.MutationResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.MutationResult{}, err
		}
		if n == 0 {
			return application.MutationResult{}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// FinishInitRun 落账 InitRunner 执行结果，要求 init_status=running（design.md §2.1/§3）。
// status 为 'succeeded' 或 'failed'。返回 MutationResult：Matched=当前 init_status=running；
// 同值口径 init_status+init_error：若新 status==running 且 initError 同值则 Matched+!Changed
// （罕见，调用方按 Matched 判定）。
// F-01：expected（init_status='running'）与同值排除（init_status IS NOT ? AND init_error IS NOT ?）
// 下推到 UPDATE WHERE；事务内读 updated_at 旧值。
func (q *Queries) FinishInitRun(ctx context.Context, taskID string, status ocdecktask.InitStatus, initError *string) (application.MutationResult, error) {
	newIE := nullableString(initError)
	newInitNS := sql.NullString{String: string(status), Valid: true}
	return runTx(ctx, q, func(qx *Queries) (application.MutationResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT init_status, init_error, updated_at FROM tasks WHERE id = ? AND init_status = 'running'`,
			taskID)
		var curInit string
		var curInitError sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curInit, &curInitError, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.MutationResult{}, nil
			}
			return application.MutationResult{}, err
		}
		if curInit == string(status) && nullStringEqual(curInitError, newIE) {
			// 全列同值：不写。
			return application.MutationResult{Matched: true}, nil
		}
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		// 同值排除（任一列不同才匹配，F-01）：init_status + init_error 全部等于新值才视为同值。
		diffPred, diffArgs := anyColDiffersPredicate(
			[]string{"init_status", "init_error"},
			[]sql.NullString{newInitNS, newIE})
		qry := "UPDATE tasks SET init_status = ?, init_error = ?, " + updClause +
			" WHERE id = ? AND init_status = 'running' AND (" + diffPred + ")"
		args := []any{string(status), newIE}
		args = append(args, updArgs...)
		args = append(args, taskID)
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
			// 并发下 CAS 已失配或同值。
			return application.MutationResult{Matched: true}, nil
		}
		return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
	})
}

// ConvergeInterruptedInitRuns 启动收敛：把 init_status ∈ {pending, running} 的任务
// 置为 failed 并写 init_error='interrupted by server restart'（design.md §2.1/§3）。
// 返回受影响行数。所有命中行 init_status 均 ≠ failed → 均为真实变更，updated_at 推进 now。
// 用于 Reconcile 启动时收敛上次进程未完成的 init 执行。
// F-01：WHERE 的 init_status IN('pending','running') 已原子排除同值行（failed+interrupted
// 不在集合内），单语句原子；NOT(...) 冗余保留无害。
func (q *Queries) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET init_status = 'failed', init_error = 'interrupted by server restart', updated_at = ?
		 WHERE init_status IN ('pending', 'running') AND NOT (init_status = 'failed' AND init_error = 'interrupted by server restart')`,
		nowUnix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAbsentSessions 删除任务中不在 keepSet 内的会话归属行，返回被删除的 session ID
//（对齐计数用，design.md §4 / D2 align 行）。设计依据 design.md §4：仅完整对齐结果
//（count < limit）可删缺席行。
func (q *Queries) DeleteAbsentSessions(ctx context.Context, taskID string, keepSet []string) ([]string, error) {
	selectQry, selectArgs := absentSessionsQuery(taskID, keepSet)
	rows, err := q.db.QueryContext(ctx, selectQry, selectArgs...)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, err
		}
		deleted = append(deleted, sid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(deleted) == 0 {
		return nil, nil
	}
	deleteQry, deleteArgs := absentSessionsQuery(taskID, keepSet)
	if _, err := q.db.ExecContext(ctx, "DELETE FROM task_sessions WHERE session_id IN (SELECT session_id FROM ("+deleteQry+"))", deleteArgs...); err != nil {
		return nil, err
	}
	return deleted, nil
}

// absentSessionsQuery 构造缺席行（task 下不在 keepSet 内）的查询与参数，
// SELECT 与 DELETE 共用同一谓词保证删除范围与读取一致。
func absentSessionsQuery(taskID string, keepSet []string) (string, []any) {
	if len(keepSet) == 0 {
		return `SELECT session_id FROM task_sessions WHERE task_id = ?`, []any{taskID}
	}
	placeholders := make([]string, len(keepSet))
	args := make([]any, 0, len(keepSet)+1)
	args = append(args, taskID)
	for i, s := range keepSet {
		placeholders[i] = "?"
		args = append(args, s)
	}
	return `SELECT session_id FROM task_sessions WHERE task_id = ? AND session_id NOT IN (` + joinPlaceholders(placeholders) + `)`, args
}

// withTxQueries 使用 db（*sql.DB）开启事务并在回调中提供绑定该事务的 Queries。
// 供未持有 DB 句柄的方法（如 AlignTaskSessions 在 *sql.DB 绑定时）自动落事务。
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

// runTx 在事务内执行 fn 并返回其结果。若 q 已绑定 *sql.Tx（事务内调用）则直接复用；
// 否则经 withTxQueries 自动开事务。供同值原子写方法在事务内读旧值 + 原子 UPDATE。
func runTx[T any](ctx context.Context, q *Queries, fn func(qtx *Queries) (T, error)) (T, error) {
	var zero T
	if _, isTx := q.db.(*sql.Tx); isTx {
		return fn(q)
	}
	var r T
	txErr := withTxQueries(ctx, q.db, func(qtx *Queries) error {
		var cerr error
		r, cerr = fn(qtx)
		return cerr
	})
	if txErr != nil {
		return zero, txErr
	}
	return r, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTaskRow(row rowScanner) (TaskRow, error) {
	var t TaskRow
	err := row.Scan(&t.ID, &t.ProjectID, &t.Name, &t.Branch, &t.Status, &t.WorktreePath,
		&t.LastPort, &t.LastError, &t.Notice, &t.DeleteMode, &t.EnvSnapshot,
		&t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.InitStatus, &t.InitError, &t.BaseRef,
		&t.AnchorSessionID)
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

// --- session 归属隔离（add-plain-dir-project D8：原子 claim / 对齐 / 条件刷新） ---

// AlignMode store 层对齐模式（add-plain-dir-project D8：task 层 AlignMode 经 adapter 映射）。
// 合法值仅两种，未知值 MUST 在任何写入前返回错误（fail-closed）。
type AlignMode int

const (
	// AlignModeRepo 目录私有：listed 逐个原子 claim（未被他任务拥有才插入/更新），
	// 冲突 ID 经 AlignTaskSessions 返回值上报；complete 时删 owned 缺席行。
	AlignModeRepo AlignMode = iota + 1
	// AlignModeOwnedOnly 目录可共享（dir）：仅对 listed∩owned 刷新 last_seen_at，
	// 绝不 claim；complete 时仅删 owned 缺席行。
	AlignModeOwnedOnly
)

// SessionObservation 持久化中立的会话观测（store 层；task 层经 adapter 转换）。
// 仅含归属写回所需字段：SessionID/CreatedAt/UpdatedAt/ParentID。
type SessionObservation struct {
	SessionID string
	CreatedAt int64
	UpdatedAt int64
	ParentID  string
}

// ClaimTaskSession 原子 claim 一个 session 至本任务（add-plain-dir-project D8）。
// 单事务内"仅当该 sessionID 未被其他任务拥有时插入/更新本任务行；已被他任务拥有则
// Claimed=false + OwnerTaskID"。不加跨任务唯一索引（避免对存量数据做迁移）。
// SQLite 单写者语义下事务无竞态。
//
// 返回 application.ClaimResult：Changed=新插入或 last_seen_at/parent_id 实际推进
//（design.md D0:77，同值幂等 upsert 为 Claimed+!Changed）。
//
// 冲突判定：存在 task_id != 本任务 且 session_id == sessionID 的行即冲突（OwnerTaskID 为他任务）。
// 本任务已拥有（task_id == 本任务）→ Claimed=true，刷新 last_seen_at/parent_id（幂等 upsert）。
// 无任何归属 → 插入本任务行。
//
// created/firstSeen/lastSeen 与既有 UpsertTaskSession 语义一致。
func (q *Queries) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	if _, isTx := q.db.(*sql.Tx); isTx {
		return q.claimTaskSessionInTx(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
	}
	var res application.ClaimResult
	txErr := withTxQueries(ctx, q.db, func(qtx *Queries) error {
		var cerr error
		res, cerr = qtx.claimTaskSessionInTx(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
		return cerr
	})
	return res, txErr
}

// claimTaskSessionInTx 在已绑定事务内执行 claim 逻辑，供事务与非事务路径复用。
func (q *Queries) claimTaskSessionInTx(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	// 先查是否被他任务拥有。
	row := q.db.QueryRowContext(ctx,
		`SELECT task_id FROM task_sessions WHERE session_id = ? AND task_id != ?`, sessionID, taskID)
	var owner string
	err := row.Scan(&owner)
	if err == nil {
		// 被他任务拥有 → 冲突，不写入。
		return application.ClaimResult{Claimed: false, OwnerTaskID: owner}, nil
	}
	if err != sql.ErrNoRows {
		return application.ClaimResult{}, err
	}
	// 无冲突：读本任务既有行，判定 changed（新插入或 last_seen_at/parent_id 实际推进）。
	orow := q.db.QueryRowContext(ctx,
		`SELECT last_seen_at, parent_id FROM task_sessions WHERE task_id = ? AND session_id = ?`, taskID, sessionID)
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
	// upsert 本任务行（与 UpsertTaskSession 一致语义）。
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id, session_id) DO UPDATE SET
		   last_seen_at = MAX(excluded.last_seen_at, task_sessions.last_seen_at),
		   parent_id = excluded.parent_id`,
		taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
	if err != nil {
		return application.ClaimResult{}, err
	}
	return application.ClaimResult{Claimed: true, Changed: changed}, nil
}

// ClaimTaskSessionAndSetAnchor 单事务 claim session 并写入 tasks.anchor_session_id（D5）。
// claim 成功后 sessionID 立即成为权威锚定；冲突时归属与 anchor 均不修改。
func (q *Queries) ClaimTaskSessionAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	return runTx(ctx, q, func(qtx *Queries) (application.ClaimResult, error) {
		res, err := qtx.claimTaskSessionInTx(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
		if err != nil || !res.Claimed {
			return res, err
		}
		if _, err := qtx.db.ExecContext(ctx,
			`UPDATE tasks SET anchor_session_id = ? WHERE id = ?`, sessionID, taskID); err != nil {
			return application.ClaimResult{}, err
		}
		return res, nil
	})
}

// ClearTaskAnchorConditional 条件清空锚定（D5 CAS）：
// `anchor_session_id=NULL WHERE id=? AND anchor_session_id=<old>`。
// Matched=命中并清空；0 行 → !Matched（调用方复读后判定）。
func (q *Queries) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET anchor_session_id = NULL WHERE id = ? AND anchor_session_id = ?`,
		taskID, oldAnchor)
	if err != nil {
		return application.MutationResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return application.MutationResult{}, err
	}
	if n == 0 {
		return application.MutationResult{}, nil
	}
	return application.MutationResult{Matched: true, Changed: true}, nil
}

// CompleteRecoveryFailure 条件事务（D3）：expected=activating 时原子完成
// status=suspended + last_error=<cause> + env_snapshot=NULL。
// CAS 失配（当前 status 非 activating）时三个字段均不修改，返回 !Matched。
func (q *Queries) CompleteRecoveryFailure(ctx context.Context, id string, lastError *string) (application.TransitionResult, error) {
	return runTx(ctx, q, func(qx *Queries) (application.TransitionResult, error) {
		row := qx.db.QueryRowContext(ctx,
			`SELECT status, last_error, env_snapshot, updated_at FROM tasks WHERE id = ?`, id)
		var curStatus string
		var curLastError, curEnv sql.NullString
		var curUpdatedAt int64
		if err := row.Scan(&curStatus, &curLastError, &curEnv, &curUpdatedAt); err != nil {
			if err == sql.ErrNoRows {
				return application.TransitionResult{}, nil
			}
			return application.TransitionResult{}, err
		}
		if curStatus != string(ocdecktask.StatusActivating) {
			return application.TransitionResult{}, nil
		}
		newLE := nullableString(lastError)
		now := nowUnix()
		updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
		qry := "UPDATE tasks SET status = ?, last_error = ?, env_snapshot = NULL, " + updClause +
			" WHERE id = ? AND status = ?"
		args := []any{string(ocdecktask.StatusSuspended), newLE}
		args = append(args, updArgs...)
		args = append(args, id, string(ocdecktask.StatusActivating))
		res, err := qx.db.ExecContext(ctx, qry, args...)
		if err != nil {
			return application.TransitionResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return application.TransitionResult{}, err
		}
		if n == 0 {
			return application.TransitionResult{}, nil
		}
		return application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt},
			StatusChanged:  true,
			From:           ocdecktask.StatusActivating,
			To:             ocdecktask.StatusSuspended,
		}, nil
	})
}

// recoveryPermitWindowSec / recoveryPermitMax 为 D3 固定预算：滚动 5 分钟最多 3 个 permit。
const (
	recoveryPermitWindowSec int64 = 5 * 60
	recoveryPermitMax             = 3
)

// AcquirePermitResult 是 AcquireRecoveryPermit 的结构化结果。
type AcquirePermitResult struct {
	Acquired bool // false=窗口已满，未写入新记录
	Ordinal  int  // 窗口内第几个（1-based）；未取得时为 0
}

// AcquireRecoveryPermit 原子写入一条 attempt 记录并返回窗口内 ordinal（D3）。
// now 为调用方注入的 Unix 秒（确定性测试）；先惰性裁剪过期行，窗口已满则不写入。
func (q *Queries) AcquireRecoveryPermit(ctx context.Context, taskID string, now int64) (AcquirePermitResult, error) {
	return runTx(ctx, q, func(qtx *Queries) (AcquirePermitResult, error) {
		windowStart := now - recoveryPermitWindowSec
		if _, err := qtx.pruneRecoveryAttempts(ctx, taskID, windowStart); err != nil {
			return AcquirePermitResult{}, err
		}
		n, err := qtx.countRecoveryAttemptsSince(ctx, taskID, windowStart)
		if err != nil {
			return AcquirePermitResult{}, err
		}
		if n >= recoveryPermitMax {
			return AcquirePermitResult{}, nil
		}
		if _, err := qtx.db.ExecContext(ctx,
			`INSERT INTO task_recovery_attempts (task_id, attempted_at) VALUES (?, ?)`,
			taskID, now); err != nil {
			return AcquirePermitResult{}, err
		}
		return AcquirePermitResult{Acquired: true, Ordinal: n + 1}, nil
	})
}

// CountRecoveryAttemptsInWindow 返回滚动窗口内（now-5min..now）的 attempt 数。
func (q *Queries) CountRecoveryAttemptsInWindow(ctx context.Context, taskID string, now int64) (int, error) {
	return q.countRecoveryAttemptsSince(ctx, taskID, now-recoveryPermitWindowSec)
}

// PruneExpiredRecoveryAttempts 删除 attempted_at < before 的记录（惰性裁剪入口）。
// taskID 非空则仅裁该任务；空则全表。
func (q *Queries) PruneExpiredRecoveryAttempts(ctx context.Context, taskID string, before int64) (int64, error) {
	return q.pruneRecoveryAttempts(ctx, taskID, before)
}

func (q *Queries) countRecoveryAttemptsSince(ctx context.Context, taskID string, since int64) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_recovery_attempts WHERE task_id = ? AND attempted_at >= ?`,
		taskID, since).Scan(&n)
	return n, err
}

func (q *Queries) pruneRecoveryAttempts(ctx context.Context, taskID string, before int64) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if taskID == "" {
		res, err = q.db.ExecContext(ctx, `DELETE FROM task_recovery_attempts WHERE attempted_at < ?`, before)
	} else {
		res, err = q.db.ExecContext(ctx,
			`DELETE FROM task_recovery_attempts WHERE task_id = ? AND attempted_at < ?`, taskID, before)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchOwnedTaskSession 条件 UPDATE 仅本任务已归属行的 last_seen_at（add-plain-dir-project D8）。
// 绝不插入；返回 MutationResult：Matched=命中本任务归属行，Changed=值真实推进
//（WHERE last_seen_at < ? 值变化条件，design.md D2 session touch 行；同值为 Matched+!Changed）。
// 未命中归属行（!Matched）为正常路径，调用方记 debug 不报错。
func (q *Queries) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	res, err := q.db.ExecContext(ctx,
		`UPDATE task_sessions SET last_seen_at = ?
		 WHERE task_id = ? AND session_id = ? AND last_seen_at < ?`,
		lastSeenAt, taskID, sessionID, lastSeenAt)
	if err != nil {
		return application.MutationResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return application.MutationResult{}, err
	}
	if n == 1 {
		return application.MutationResult{Matched: true, Changed: true}, nil
	}
	// 0 行分类：同值已归属（Matched+!Changed）或未命中归属（!Matched）。
	var one int
	row := q.db.QueryRowContext(ctx,
		`SELECT 1 FROM task_sessions WHERE task_id = ? AND session_id = ?`, taskID, sessionID)
	if rerr := row.Scan(&one); rerr == nil {
		return application.MutationResult{Matched: true}, nil
	} else if rerr != sql.ErrNoRows {
		return application.MutationResult{}, rerr
	}
	return application.MutationResult{}, nil
}

// AlignTaskSessions 单事务原子对齐（add-plain-dir-project D8 + design.md D0:80/86）：
//   - 读 owned 集合 O（本任务已有归属行）；
//   - mode=repo：对 listed 逐个原子 claim（冲突 ID 经返回值上报）；
//   - mode=ownedOnly：仅对 listed∩O 刷新 last_seen_at（绝不 claim）；
//   - complete=true：仅删 O 中缺席行（不在 listed 内的 owned 行）并同事务提交 notice 变更
//     （application.NoticeMutation：expected 失配整事务回滚返回 application.AlignConflict；
//     同值原子 no-op 不推进 updated_at）；complete=false 不删缺席行、不触碰 notice；
//   - 返回 application.AlignResult：Inserted/Touched/Deleted 计数、AffectedSessionIDs
//     （受影响集合）、OwnedSessionIDs（对齐后全量 owned）、TaskMutation（notice 分支结构化结果）。
//
// owned 快照与刷新/删除/notice 同事务，杜绝"事务外 O 快照期间新 claim 行被 complete 误删"。
func (q *Queries) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	if _, isTx := q.db.(*sql.Tx); isTx {
		return q.alignTaskSessionsInTx(ctx, taskID, mode, listed, complete, notice)
	}
	var res application.AlignResult
	txErr := withTxQueries(ctx, q.db, func(qtx *Queries) error {
		var cerr error
		res, cerr = qtx.alignTaskSessionsInTx(ctx, taskID, mode, listed, complete, notice)
		return cerr
	})
	return res, txErr
}

// alignTaskSessionsInTx 在已绑定事务内执行对齐逻辑，供事务与非事务路径复用。
func (q *Queries) alignTaskSessionsInTx(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	var res application.AlignResult
	if mode != AlignModeRepo && mode != AlignModeOwnedOnly {
		// fail-closed：未知 mode MUST 在任何写入前返回错误（D8）。
		return res, fmt.Errorf("store: unknown AlignMode %d", mode)
	}
	// 读 owned 集合 O（本任务已有归属行）。
	owned, err := q.ListOwnedSessionIDs(ctx, taskID)
	if err != nil {
		return res, err
	}
	ownedSet := make(map[string]bool, len(owned))
	for _, sid := range owned {
		ownedSet[sid] = true
	}
	var affected []ocdecksess.ID
	switch mode {
	case AlignModeRepo:
		// listed 逐个原子 claim（冲突 ID 上报，不写入）。
		var conflicts []ocdecksess.ID
		for _, s := range listed {
			cres, cerr := q.claimTaskSessionInTx(ctx, taskID, s.SessionID, s.CreatedAt, nowUnix(), s.UpdatedAt, s.ParentID)
			if cerr != nil {
				return res, cerr
			}
			if !cres.Claimed {
				conflicts = append(conflicts, ocdecksess.ID(s.SessionID))
				continue
			}
			if cres.Changed {
				if ownedSet[s.SessionID] {
					res.Touched++
				} else {
					res.Inserted++
				}
				affected = append(affected, ocdecksess.ID(s.SessionID))
			}
		}
		res.Conflicts = conflicts
	case AlignModeOwnedOnly:
		// 仅对 listed∩O 刷新 last_seen_at，绝不 claim。
		for _, s := range listed {
			if !ownedSet[s.SessionID] {
				continue
			}
			tres, terr := q.TouchOwnedTaskSession(ctx, taskID, s.SessionID, s.UpdatedAt)
			if terr != nil {
				return res, terr
			}
			if tres.Changed {
				res.Touched++
				affected = append(affected, ocdecksess.ID(s.SessionID))
			}
		}
	}
	if complete {
		// 仅删 owned 缺席行（listed 内的 session_id 不删，本任务其他 owned 行删除）。
		keep := make([]string, 0, len(listed))
		for _, s := range listed {
			keep = append(keep, s.SessionID)
		}
		deleted, derr := q.DeleteAbsentSessions(ctx, taskID, keep)
		if derr != nil {
			return res, derr
		}
		res.Deleted = len(deleted)
		for _, sid := range deleted {
			affected = append(affected, ocdecksess.ID(sid))
		}
		tm, nerr := q.alignNoticeInTx(ctx, taskID, notice)
		if nerr != nil {
			return res, nerr
		}
		res.TaskMutation = tm
	}
	ownedAfter, err := q.ListOwnedSessionIDs(ctx, taskID)
	if err != nil {
		return res, err
	}
	ids := make([]ocdecksess.ID, 0, len(ownedAfter))
	for _, sid := range ownedAfter {
		ids = append(ids, ocdecksess.ID(sid))
	}
	res.OwnedSessionIDs = ids
	res.AffectedSessionIDs = affected
	return res, nil
}

// alignNoticeInTx 在对齐事务内提交 notice 变更（design.md D0:80/86，仅 complete 路径调用）。
//
// expected 与事务内最新 notice 不匹配 → 返回 application.AlignConflict（调用方事务回滚，
// 不提交任何 session 行变更）；匹配且 New 不同 → UPDATE 携带 expected 与同值排除谓词
//（仅跨秒推进 updated_at）；匹配且同值 → Matched+!Changed 原子 no-op。
func (q *Queries) alignNoticeInTx(ctx context.Context, taskID string, mut application.NoticeMutation) (application.MutationResult, error) {
	expNS := nullableString(mut.Expected)
	newNS := nullableString(mut.New)
	row := q.db.QueryRowContext(ctx, `SELECT notice, updated_at FROM tasks WHERE id = ?`, taskID)
	var current sql.NullString
	var curUpdatedAt int64
	if err := row.Scan(&current, &curUpdatedAt); err != nil {
		return application.MutationResult{}, err
	}
	if !nullStringEqual(current, expNS) {
		// expected 失配：整事务回滚，application 重读重决策后有界重试。
		return application.MutationResult{}, &application.AlignConflict{
			TaskID:   taskID,
			Expected: mut.Expected,
			Actual:   nullStringToPtr(current),
		}
	}
	if nullStringEqual(current, newNS) {
		// notice 同值：不推进 updated_at。
		return application.MutationResult{Matched: true}, nil
	}
	now := nowUnix()
	updClause, updArgs := buildUpdateOnAdvance(curUpdatedAt, now)
	expPred, expArg := expectedPredicate("notice", expNS)
	newPred, newArg := colNotEqualPredicate("notice", newNS)
	qry := "UPDATE tasks SET notice = ?, " + updClause +
		" WHERE id = ? AND " + expPred + " AND " + newPred
	args := []any{newNS}
	args = append(args, updArgs...)
	args = append(args, taskID)
	if expArg != nil {
		args = append(args, expArg)
	}
	if newArg != nil {
		args = append(args, newArg)
	}
	qres, err := q.db.ExecContext(ctx, qry, args...)
	if err != nil {
		return application.MutationResult{}, err
	}
	n, err := qres.RowsAffected()
	if err != nil {
		return application.MutationResult{}, err
	}
	if n == 0 {
		// 事务内单写者下不可达（刚读过 expected 匹配且 New 不同）；防御性按冲突回滚。
		return application.MutationResult{}, &application.AlignConflict{
			TaskID:   taskID,
			Expected: mut.Expected,
			Actual:   nullStringToPtr(current),
		}
	}
	return application.MutationResult{Matched: true, Changed: true, UpdatedAtAdvanced: now != curUpdatedAt}, nil
}

// ListOwnedSessionIDs 返回本任务已拥有的 session_id 列表（O 集合，align 事务内与
// SessionRepository.OwnedSessions 共用）。
func (q *Queries) ListOwnedSessionIDs(ctx context.Context, taskID string) ([]string, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT session_id FROM task_sessions WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// nullStringToPtr 把 sql.NullString 映射为 *string：Invalid → nil。
func nullStringToPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

// nullStringValue 返回 sql.NullString 的 String 值，Invalid 时空串（NULL 与 '' 语义等价的
// 归一比较用）。
func nullStringValue(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
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
