// Package sqlite 是 application ports 的 SQLite 持久化适配器（design.md D0）。
//
// 包装既有 *store.DB，实现 application 层 TaskRepository/SessionRepository/ProjectReader
// /EnvReader/CleanupDebtRepository/ProcessPort/OpenCodePort/WorktreePort。
//
// P1.2 仅提供骨架 + 结构化结果映射：store 写方法已直接返回 application 结果类型，
// 适配器对 reformed 方法做 1:1 透传；尚未接入的 LifecycleService 在 P1.4 落地。
// 本阶段无 fx wiring（main 仍在 legacy lane），骨架仅保证编译与类型闭合。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/infrastructure/store"
)

// Adapter 包装 *store.DB 实现 application ports 子集（design.md D0）。
//
// P1.2 阶段仅实现 TaskRepository/SessionRepository 等已 reformed 的方法骨架；
// ProcessPort/OpenCodePort/WorktreePort 的完整实现留待 P1.4 LifecycleService 接入。
type Adapter struct {
	db *store.DB
}

// New 构造 Adapter。
func New(db *store.DB) *Adapter { return &Adapter{db: db} }

// --- TaskRepository（design.md D0） ---

// CreateTask 委托 store.CreateTask（仅消费 row 的 ID/ProjectID/Name/Branch/Status/
// WorktreePath/BaseRef，status 由调用方提供不再校验）。
func (a *Adapter) CreateTask(ctx context.Context, row application.TaskSnapshot) error {
	return a.db.CreateTask(ctx, store.TaskRow{
		ID: row.ID, ProjectID: row.ProjectID, Name: row.Name, Branch: row.Branch,
		Status: row.Status, WorktreePath: row.WorktreePath, BaseRef: row.BaseRef,
	})
}

// UpdateTaskStatus 委托 store.UpdateTaskStatus，透传结构化结果。
func (a *Adapter) UpdateTaskStatus(ctx context.Context, id string, status ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatus(ctx, id, status, lastError)
}

// UpdateTaskStatusConditional 委托 store.UpdateTaskStatusConditional。
func (a *Adapter) UpdateTaskStatusConditional(ctx context.Context, id string, fromStatus, toStatus ocdecktask.Status, lastError *string) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

// UpdateTaskEnvSnapshot 委托 store.UpdateTaskEnvSnapshot。
func (a *Adapter) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot *string) (application.MutationResult, error) {
	return a.db.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

// UpdateTaskLastPort 委托 store.UpdateTaskLastPort。
func (a *Adapter) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return a.db.UpdateTaskLastPort(ctx, id, port)
}

// UpdateTaskNotice 委托 store.UpdateTaskNotice。
func (a *Adapter) UpdateTaskNotice(ctx context.Context, id string, notice *string) (application.MutationResult, error) {
	return a.db.UpdateTaskNotice(ctx, id, notice)
}

// UpdateTaskNoticeCAS 委托 store.UpdateTaskNoticeCAS。
func (a *Adapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	return a.db.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// SetTaskDeleteMode 委托 store.SetTaskDeleteMode。
func (a *Adapter) SetTaskDeleteMode(ctx context.Context, id string, mode ocdecktask.DeleteMode) (application.MutationResult, error) {
	return a.db.SetTaskDeleteMode(ctx, id, mode)
}

// BeginDeleteIntent 委托 store.BeginDeleteIntent。
func (a *Adapter) BeginDeleteIntent(ctx context.Context, id string, mode ocdecktask.DeleteMode, fromStatuses []ocdecktask.Status) (application.TransitionResult, error) {
	return a.db.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

// ArchiveTask 委托 store.ArchiveTask。
func (a *Adapter) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.ArchiveTask(ctx, id)
}

// RestoreTask 委托 store.RestoreTask。
func (a *Adapter) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.RestoreTask(ctx, id)
}

// DeleteTask 委托 store.DeleteTask。
func (a *Adapter) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	return a.db.DeleteTask(ctx, id)
}

// CommitCreated 委托 store.CommitCreated。
func (a *Adapter) CommitCreated(ctx context.Context, taskID string, expectedStatus ocdecktask.Status, initStatus ocdecktask.InitStatus) (application.TransitionResult, error) {
	return a.db.CommitCreated(ctx, taskID, expectedStatus, initStatus)
}

// ClaimInitRun 委托 store.ClaimInitRun。
func (a *Adapter) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRun(ctx, taskID)
}

// ClaimInitRerun 委托 store.ClaimInitRerun。
func (a *Adapter) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRerun(ctx, taskID)
}

// FinishInitRun 委托 store.FinishInitRun。
func (a *Adapter) FinishInitRun(ctx context.Context, taskID string, status ocdecktask.InitStatus, initError *string) (application.MutationResult, error) {
	return a.db.FinishInitRun(ctx, taskID, status, initError)
}

// ConvergeInterruptedInitRuns 委托 store.ConvergeInterruptedInitRuns。
func (a *Adapter) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return a.db.ConvergeInterruptedInitRuns(ctx)
}

// GetTask 读侧：返回领域 Task 聚合的 guard 视图（design.md D0 P1.4.2）。
//
// 从 store.TaskRow 行值经 domain/task.Rehydrate 重建，填入 guard 所需字段
// （status/init_status/delete_mode/notices）。notice JSON 解析为 domain typed Notice 集合；
// 损坏 JSON 返回 error（fail-closed，对齐 legacy parseNotices 语义）。
//
// 本步仅实现读侧 guard 视图；创建期可变信息（name/branch/worktreePath/baseRef）与
// 持久化元数据（last_port/last_error/env_snapshot/timestamps）不在 guard 视图中，
// 调用方需要完整行时仍走 legacy store.DB.GetTask。
func (a *Adapter) GetTask(ctx context.Context, id string) (*ocdecktask.Task, error) {
	row, err := a.db.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, application.ErrTaskNotFound
		}
		return nil, err
	}
	notices, err := parseNoticeJSON(row.Notice)
	if err != nil {
		return nil, fmt.Errorf("sqlite GetTask: %w", err)
	}
	return ocdecktask.Rehydrate(ocdecktask.GuardView{
		Status:     ocdecktask.Status(row.Status),
		InitStatus: ocdecktask.InitStatus(row.InitStatus),
		DeleteMode: ocdecktask.DeleteMode(nullStringValue(row.DeleteMode)),
		Notices:    notices,
	}), nil
}

// --- TaskReadRepository（design.md D0:71 consumer-owned，P1.4.4 Get/List 读侧） ---

// GetTaskRow 读侧全行快照（design.md D0 P1.4.4）。映射 store.TaskRow → application.TaskSnapshot，
// nullable 字段转 *string/*int64（MUST NOT 泄漏 sql.NullString）。未命中返回 store error
// （sql.ErrNoRows，由 Manager facade 映射为 codeNotFound）。
func (a *Adapter) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	row, err := a.db.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return application.TaskSnapshot{}, application.ErrTaskNotFound
		}
		return application.TaskSnapshot{}, err
	}
	return toTaskSnapshot(row), nil
}

// ListTasksByProject 读侧全行快照列表（design.md D0 P1.4.4）。映射 store.TaskRow → application.TaskSnapshot。
func (a *Adapter) ListTasksByProject(ctx context.Context, projectID string) ([]application.TaskSnapshot, error) {
	rows, err := a.db.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]application.TaskSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, toTaskSnapshot(r))
	}
	return out, nil
}

// toTaskSnapshot 把 store.TaskRow 映射为 application.TaskSnapshot（design.md D0:71）。
//
// nullable sql.NullString/sql.NullInt64 → *string/*int64（Invalid → nil），
// 保持与 legacy toTaskRow 1:1 字段对应，供 Manager facade 还原 task.TaskRow。
func toTaskSnapshot(r store.TaskRow) application.TaskSnapshot {
	return application.TaskSnapshot{
		ID:           r.ID,
		ProjectID:    r.ProjectID,
		Name:         r.Name,
		Branch:       r.Branch,
		Status:       r.Status,
		WorktreePath: r.WorktreePath,
		LastPort:     nullInt64ToPtr(r.LastPort),
		LastError:    nullStringToPtrSnapshot(r.LastError),
		Notice:       nullStringToPtrSnapshot(r.Notice),
		DeleteMode:   nullStringToPtrSnapshot(r.DeleteMode),
		EnvSnapshot:  nullStringToPtrSnapshot(r.EnvSnapshot),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		ArchivedAt:   nullInt64ToPtr(r.ArchivedAt),
		InitStatus:   r.InitStatus,
		InitError:    nullStringToPtrSnapshot(r.InitError),
		BaseRef:      r.BaseRef,
	}
}

// nullInt64ToPtr 把 sql.NullInt64 映射为 *int64：Invalid → nil。
func nullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// nullStringToPtrSnapshot 把 sql.NullString 映射为 *string：Invalid → nil。
// （与 task 包 nullStringToPtr 同义，此处为 sqlite adapter 层独立 helper，避免跨包依赖。）
func nullStringToPtrSnapshot(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// parseNoticeJSON 把 tasks.notice JSON 数组解析为 domain typed Notice 集合。
// JSON 形态的单一映射在 domain/task（ParseNoticesJSON）；此处仅做 sql.NullString →
// string 的取值转换。损坏 JSON 返回 error（fail-closed，对齐 legacy parseNotices）。
func parseNoticeJSON(raw sql.NullString) ([]ocdecktask.Notice, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	return ocdecktask.ParseNoticesJSON(raw.String)
}

// nullStringValue 返回 sql.NullString 的 String 值，Invalid 时空串。
func nullStringValue(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// --- SessionRepository（design.md D0:78-86，P1.4.5 实现） ---
//
// 委托 store 的结构化 session 写方法（claim/touch/delete/align 均为 store 层单事务
// 原子操作），adapter 负责 domain 类型（ocdecksess.ID/Observation/AlignMode）与 store
// 类型的映射。Align 的 notice CAS/回滚语义由 store.alignNoticeInTx 在事务内保证
//（expected 失配返回 application.AlignConflict）。

// Claim 实现 SessionRepository.Claim：单事务先查他主再 upsert（design.md D0:77）。
func (a *Adapter) Claim(ctx context.Context, taskID string, obs ocdecksess.Observation) (application.ClaimResult, error) {
	return a.db.ClaimTaskSession(ctx, taskID, string(obs.ID), obs.CreatedAt, obs.FirstSeenAt, obs.UpdatedAt, obs.ParentID)
}

// TouchOwned 实现 SessionRepository.TouchOwned：值变化条件推进已归属行 last_seen_at。
func (a *Adapter) TouchOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID, lastSeenAt int64) (application.MutationResult, error) {
	return a.db.TouchOwnedTaskSession(ctx, taskID, string(sessionID), lastSeenAt)
}

// DeleteOwned 实现 SessionRepository.DeleteOwned：返回受影响行数。
func (a *Adapter) DeleteOwned(ctx context.Context, taskID string, sessionID ocdecksess.ID) (int, error) {
	return a.db.DeleteTaskSession(ctx, taskID, string(sessionID))
}

// Align 实现 SessionRepository.Align：单事务批处理对齐（design.md D0:80/86）。
// mode 映射 domain → store；observed 映射 Observation → store.SessionObservation；
// complete=false 时 notice 分支由 store 层跳过（overflow 前置 CAS 已由 application 完成）。
func (a *Adapter) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	obs := make([]store.SessionObservation, 0, len(observed))
	for _, o := range observed {
		obs = append(obs, store.SessionObservation{
			SessionID: string(o.ID),
			CreatedAt: o.CreatedAt,
			UpdatedAt: o.UpdatedAt,
			ParentID:  o.ParentID,
		})
	}
	return a.db.AlignTaskSessions(ctx, taskID, toStoreAlignMode(mode), obs, complete, notice)
}

// toStoreAlignMode 映射 domain/session.AlignMode → store.AlignMode（未知值由 store 层
// AlignTaskSessions fail-closed 拒绝）。
func toStoreAlignMode(mode ocdecksess.AlignMode) store.AlignMode {
	switch mode {
	case ocdecksess.AlignModeRepo:
		return store.AlignModeRepo
	case ocdecksess.AlignModeOwnedOnly:
		return store.AlignModeOwnedOnly
	default:
		return 0
	}
}

// OwnedSessions 实现 SessionRepository.OwnedSessions：返回全量 owned session ID。
func (a *Adapter) OwnedSessions(ctx context.Context, taskID string) ([]ocdecksess.ID, error) {
	ids, err := a.db.ListOwnedSessionIDs(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]ocdecksess.ID, 0, len(ids))
	for _, sid := range ids {
		out = append(out, ocdecksess.ID(sid))
	}
	return out, nil
}

// OwnerOf 实现 SessionRepository.OwnerOf：fail-closed on ambiguous ownership（design.md D0:82）。
// 同一 session_id 归属多个 task 的历史脏数据返回 ocdecksess.AmbiguousOwnerError；
// found=false 表示无归属行。
func (a *Adapter) OwnerOf(ctx context.Context, sessionID ocdecksess.ID) (string, bool, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT DISTINCT task_id FROM task_sessions WHERE session_id = ?`, string(sessionID))
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return "", false, err
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	switch len(owners) {
	case 0:
		return "", false, nil
	case 1:
		return owners[0], true, nil
	default:
		// 历史重复归属 fail-closed：typed ambiguity，对应 status/diff 事件不 apply。
		return "", false, ocdecksess.NewAmbiguousOwnerError(sessionID, owners)
	}
}

// --- ProjectReader / EnvReader / CleanupDebtRepository（P1.4 完整实现） ---

// GetProject 读侧项目（P1.4 行→application.Project 映射）。
func (a *Adapter) GetProject(ctx context.Context, id string) (application.Project, error) {
	return application.Project{}, ErrNotImplemented
}

// ListGlobalEnvVars 读全局 env（P1.4）。
func (a *Adapter) ListGlobalEnvVars(ctx context.Context) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// ListProjectEnvVars 读项目级 env（P1.4）。
func (a *Adapter) ListProjectEnvVars(ctx context.Context, projectID string) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// ListTaskEnvVars 读任务级 env（P1.4）。
func (a *Adapter) ListTaskEnvVars(ctx context.Context, taskID string) ([]application.EnvVar, error) {
	return nil, ErrNotImplemented
}

// UpsertCleanupDebt 委托 store.UpsertCleanupDebt（P1.4 完整接入）。
func (a *Adapter) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	return a.db.UpsertCleanupDebt(ctx, sessionName, ticketsJSON, createdAt)
}

// DeleteCleanupDebt 委托 store.DeleteCleanupDebt。
func (a *Adapter) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	return a.db.DeleteCleanupDebt(ctx, sessionName)
}

// ListCleanupDebts 读侧 cleanup debt（P1.4 行→application.CleanupDebt 映射）。
func (a *Adapter) ListCleanupDebts(ctx context.Context) ([]application.CleanupDebt, error) {
	return nil, ErrNotImplemented
}

// ErrNotImplemented 表示该方法在 P1.2 骨架阶段尚未接入（留待 P1.4 LifecycleService）。
// 本阶段仅用于保证 adapter 实现全部 application ports 并编译闭合；不应在 P1.2 被调用。
var ErrNotImplemented = errNotImplemented{}

type errNotImplemented struct{}

func (errNotImplemented) Error() string { return "sqlite adapter: method not implemented in P1.2 skeleton" }

// --- ProcessPort / OpenCodePort / WorktreePort（P1.4 完整接入） ---
//
// 这三个 port 背后是 process/opencode/worktree 能力，非 store。P1.2 骨架返回
// ErrNotImplemented 保证接口闭合；P1.4 注入真实 client 实现并替换 Adapter 字段。

// ShowSessionEnvContext 实现 ProcessPort（P1.4 接入 process.Manager）。
func (a *Adapter) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	return "", ErrNotImplemented
}

// Health 实现 OpenCodePort（P1.4 接入 opencode.Client）。
func (a *Adapter) Health(ctx context.Context) error { return ErrNotImplemented }

// ListSessions 实现 OpenCodePort（P1.4）。
func (a *Adapter) ListSessions(ctx context.Context, dir string, limit int) ([]application.OpenCodeSession, error) {
	return nil, ErrNotImplemented
}

// BranchExists 实现 WorktreePort（P1.4 接入 worktree.Manager）。
func (a *Adapter) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	return false, ErrNotImplemented
}

// ValidateBranchName 实现 WorktreePort（P1.4）。
func (a *Adapter) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return ErrNotImplemented
}