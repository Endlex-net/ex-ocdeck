package task

import (
	"context"
	"database/sql"

	"ocdeck/internal/git"
	"ocdeck/internal/process"
	"ocdeck/internal/pty"
	"ocdeck/internal/store"
	"ocdeck/internal/worktree"
)

// StoreAdapter 包装 *store.DB 实现 TaskStore（row 类型转换）。
type StoreAdapter struct {
	db *store.DB
}

// NewStoreAdapter 构造 TaskStore 适配器。
func NewStoreAdapter(db *store.DB) *StoreAdapter { return &StoreAdapter{db: db} }

func (a *StoreAdapter) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	p, err := a.db.GetProject(ctx, id)
	if err != nil {
		return ProjectRow{}, err
	}
	return ProjectRow{ID: p.ID, Name: p.Name, Path: p.Path, DefaultBranch: p.DefaultBranch, Kind: p.Kind, CreatedAt: p.CreatedAt}, nil
}

func (a *StoreAdapter) CreateTask(ctx context.Context, t TaskRow) error {
	return a.db.CreateTask(ctx, store.TaskRow{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch,
		Status: t.Status, WorktreePath: t.WorktreePath, BaseRef: t.BaseRef,
	})
}

func (a *StoreAdapter) GetTask(ctx context.Context, id string) (TaskRow, error) {
	t, err := a.db.GetTask(ctx, id)
	if err != nil {
		return TaskRow{}, err
	}
	return toTaskRow(t), nil
}

func (a *StoreAdapter) ListTasksByProject(ctx context.Context, projectID string) ([]TaskRow, error) {
	rows, err := a.db.ListTasksByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRow, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTaskRow(t))
	}
	return out, nil
}

func (a *StoreAdapter) ListAllTasks(ctx context.Context) ([]TaskRow, error) {
	rows, err := a.db.ListAllTasks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRow, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTaskRow(t))
	}
	return out, nil
}

func (a *StoreAdapter) ListActiveTaskOverview(ctx context.Context) ([]ActiveTaskOverviewRow, error) {
	rows, err := a.db.ListActiveTaskOverview(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ActiveTaskOverviewRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActiveTaskOverviewRow{
			ID: r.ID, ProjectID: r.ProjectID, ProjectName: r.ProjectName, Name: r.Name,
			Branch: r.Branch, WorktreePath: r.WorktreePath, LastActiveAt: r.LastActiveAt,
		})
	}
	return out, nil
}

func (a *StoreAdapter) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error {
	return a.db.UpdateTaskStatus(ctx, id, status, lastError)
}
func (a *StoreAdapter) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (bool, error) {
	return a.db.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}
func (a *StoreAdapter) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) error {
	return a.db.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}
func (a *StoreAdapter) UpdateTaskLastPort(ctx context.Context, id string, port int) error {
	return a.db.UpdateTaskLastPort(ctx, id, port)
}
func (a *StoreAdapter) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) error {
	return a.db.UpdateTaskNotice(ctx, id, notice)
}
func (a *StoreAdapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (bool, error) {
	return a.db.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}
func (a *StoreAdapter) SetTaskDeleteMode(ctx context.Context, id, mode string) error {
	return a.db.SetTaskDeleteMode(ctx, id, mode)
}
func (a *StoreAdapter) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (bool, error) {
	return a.db.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}
func (a *StoreAdapter) ArchiveTask(ctx context.Context, id string) error {
	return a.db.ArchiveTask(ctx, id)
}
func (a *StoreAdapter) RestoreTask(ctx context.Context, id string) error {
	return a.db.RestoreTask(ctx, id)
}
func (a *StoreAdapter) DeleteTask(ctx context.Context, id string) error {
	return a.db.DeleteTask(ctx, id)
}

// --- 生命周期配置（design.md §2.1） ---

func (a *StoreAdapter) GetLifecycleConfig(ctx context.Context, projectID string) (LifecycleConfigRow, error) {
	c, err := a.db.GetLifecycleConfig(ctx, projectID)
	if err != nil {
		return LifecycleConfigRow{}, err
	}
	return LifecycleConfigRow{
		ProjectID: c.ProjectID, InheritPatterns: c.InheritPatterns,
		InitScript: c.InitScript, PreDeleteScript: c.PreDeleteScript, UpdatedAt: c.UpdatedAt,
	}, nil
}

func (a *StoreAdapter) UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error {
	return a.db.UpsertLifecycleConfig(ctx, projectID, inheritPatterns, initScript, preDeleteScript)
}

// --- init_status CAS（design.md §2.1/§3） ---

func (a *StoreAdapter) CommitCreated(ctx context.Context, taskID, expectedStatus, initStatus string) (bool, error) {
	return a.db.CommitCreated(ctx, taskID, expectedStatus, initStatus)
}

func (a *StoreAdapter) ClaimInitRun(ctx context.Context, taskID string) (bool, error) {
	return a.db.ClaimInitRun(ctx, taskID)
}

func (a *StoreAdapter) ClaimInitRerun(ctx context.Context, taskID string) (bool, error) {
	return a.db.ClaimInitRerun(ctx, taskID)
}

func (a *StoreAdapter) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (bool, error) {
	return a.db.FinishInitRun(ctx, taskID, status, initError)
}

func (a *StoreAdapter) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	return a.db.ConvergeInterruptedInitRuns(ctx)
}

func (a *StoreAdapter) ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVarRow, error) {
	rows, err := a.db.ListProjectEnvVars(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]EnvVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, EnvVarRow{Key: e.Key, Value: e.Value})
	}
	return out, nil
}
func (a *StoreAdapter) ListGlobalEnvVars(ctx context.Context) ([]GlobalEnvVarRow, error) {
	rows, err := a.db.ListGlobalEnvVars(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GlobalEnvVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, GlobalEnvVarRow{Key: e.Key, Mode: e.Mode, Value: e.Value})
	}
	return out, nil
}
func (a *StoreAdapter) ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVarRow, error) {
	rows, err := a.db.ListTaskEnvVars(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]EnvVarRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, EnvVarRow{Key: e.Key, Value: e.Value})
	}
	return out, nil
}

func (a *StoreAdapter) UpsertTaskSession(ctx context.Context, s SessionRow) error {
	return a.db.UpsertTaskSession(ctx, store.SessionRow{
		TaskID: s.TaskID, SessionID: s.SessionID, SessionCreatedAt: s.SessionCreatedAt,
		FirstSeenAt: s.FirstSeenAt, LastSeenAt: s.LastSeenAt, ParentID: s.ParentID,
	})
}
func (a *StoreAdapter) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	rows, err := a.db.ListTaskSessions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, SessionRow{
			TaskID: s.TaskID, SessionID: s.SessionID, SessionCreatedAt: s.SessionCreatedAt,
			FirstSeenAt: s.FirstSeenAt, LastSeenAt: s.LastSeenAt, ParentID: s.ParentID,
		})
	}
	return out, nil
}
func (a *StoreAdapter) ListTopLevelTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	rows, err := a.db.ListTopLevelTaskSessions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionRow, 0, len(rows))
	for _, s := range rows {
		out = append(out, SessionRow{
			TaskID: s.TaskID, SessionID: s.SessionID, SessionCreatedAt: s.SessionCreatedAt,
			FirstSeenAt: s.FirstSeenAt, LastSeenAt: s.LastSeenAt, ParentID: s.ParentID,
		})
	}
	return out, nil
}
func (a *StoreAdapter) DeleteTaskSession(ctx context.Context, taskID, sessionID string) error {
	return a.db.DeleteTaskSession(ctx, taskID, sessionID)
}
func (a *StoreAdapter) AlignSessions(ctx context.Context, taskID string, sessions []SessionRow, complete bool, noticeFn func(sql.NullString) sql.NullString) error {
	ss := make([]store.SessionRow, 0, len(sessions))
	for _, s := range sessions {
		ss = append(ss, store.SessionRow{
			TaskID: s.TaskID, SessionID: s.SessionID, SessionCreatedAt: s.SessionCreatedAt,
			FirstSeenAt: s.FirstSeenAt, LastSeenAt: s.LastSeenAt, ParentID: s.ParentID,
		})
	}
	return a.db.AlignSessions(ctx, taskID, ss, complete, noticeFn)
}

// --- session 归属隔离（add-plain-dir-project D8：原子 claim / 对齐 / 条件刷新） ---

// toStoreAlignMode 映射 task.AlignMode → store.AlignMode。
func toStoreAlignMode(mode AlignMode) store.AlignMode {
	switch mode {
	case AlignModeRepo:
		return store.AlignModeRepo
	case AlignModeOwnedOnly:
		return store.AlignModeOwnedOnly
	default:
		// 未知值由 store 层 AlignTaskSessions fail-closed 拒绝（返回错误）；此处映射为零值，
		// store 层会拒绝零值（非 AlignModeRepo/OwnedOnly）。
		return 0
	}
}

// ClaimTaskSession 原子 claim（D8）：单事务仅当 sessionID 未被他任务拥有时插入/更新本任务行。
func (a *StoreAdapter) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (bool, string, error) {
	return a.db.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}

// TouchOwnedTaskSession 条件 UPDATE 仅本任务已归属行的 last_seen_at（D8）。
func (a *StoreAdapter) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (bool, error) {
	return a.db.TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt)
}

// AlignTaskSessions 单事务原子对齐（D8）：task 层 AlignMode/SessionObservation 映射到 store 层类型。
func (a *StoreAdapter) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, noticeFn func(sql.NullString) sql.NullString) ([]string, error) {
	obs := make([]store.SessionObservation, 0, len(listed))
	for _, s := range listed {
		obs = append(obs, store.SessionObservation{
			SessionID: s.SessionID, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, ParentID: s.ParentID,
		})
	}
	return a.db.AlignTaskSessions(ctx, taskID, toStoreAlignMode(mode), obs, complete, noticeFn)
}

// --- CleanupDebtStore 适配（design.md §10：orphan tickets 跨重启持久化） ---

// UpsertCleanupDebt 插入或按 session_name 原地替换未收敛 orphan tickets。
func (a *StoreAdapter) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	return a.db.UpsertCleanupDebt(ctx, sessionName, ticketsJSON, createdAt)
}

// DeleteCleanupDebt 删除已收敛的 orphan cleanup debt。
func (a *StoreAdapter) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	return a.db.DeleteCleanupDebt(ctx, sessionName)
}

// ListCleanupDebts 枚举全部未收敛 orphan cleanup debt（Reconcile 恢复重试用）。
func (a *StoreAdapter) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	rows, err := a.db.ListCleanupDebts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CleanupDebtRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, CleanupDebtRow{SessionName: r.SessionName, Tickets: r.Tickets, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func toTaskRow(t store.TaskRow) TaskRow {
	return TaskRow{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch, Status: t.Status,
		WorktreePath: t.WorktreePath, LastPort: t.LastPort, LastError: t.LastError, Notice: t.Notice,
		DeleteMode: t.DeleteMode, EnvSnapshot: t.EnvSnapshot, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ArchivedAt: t.ArchivedAt, InitStatus: t.InitStatus, InitError: t.InitError, BaseRef: t.BaseRef,
	}
}

// WorktreeAdapter 包装 *worktree.Manager 实现 WorktreeBackend。
type WorktreeAdapter struct {
	m *worktree.Manager
}

// NewWorktreeAdapter 构造 WorktreeBackend 适配器。
func NewWorktreeAdapter(m *worktree.Manager) *WorktreeAdapter { return &WorktreeAdapter{m: m} }

func (a *WorktreeAdapter) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	return a.m.Add(ctx, repoPath, dest, branch, baseRef)
}

func (a *WorktreeAdapter) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	return a.m.Remove(ctx, wtPath, worktree.RemoveOpts{
		RepoPath:   opts.RepoPath,
		Branch:     opts.Branch,
		ForceDirty: opts.ForceDirty,
	})
}

func (a *WorktreeAdapter) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	return a.m.BranchExists(ctx, repoPath, branch)
}

// ValidateBranchName 用 git check-ref-format 校验分支名合法性（P1：Create 前置检查，
// design.md §19）。委托 internal/git.ValidateBranchName，保持 worktree 包不引入新公开方法。
func (a *WorktreeAdapter) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return git.ValidateBranchName(ctx, repoPath, branch)
}

// ResolveBaseRef 将 base_ref 短名解析为全限定 ref（add-plain-dir-project D10）。
// 委托 internal/git.ResolveBaseRef；task 包不直接依赖 internal/git，经此端口解析。
func (a *WorktreeAdapter) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	return git.ResolveBaseRef(ctx, repoPath, shortName)
}

func (a *WorktreeAdapter) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	return a.m.VerifyWorktreeProduct(ctx, repoPath, wtPath, branch)
}

func (a *WorktreeAdapter) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	return a.m.PreflightDelete(ctx, wtPath, worktree.PreflightDeleteOpts{
		RepoPath:     opts.RepoPath,
		Branch:       opts.Branch,
		ConfirmDirty: opts.ConfirmDirty,
	})
}

func (a *WorktreeAdapter) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	return a.m.DirtyFiles(ctx, wtPath)
}

// ProcessAdapter 包装 *process.Manager 实现 ProcessBackend。
type ProcessAdapter struct {
	m *process.Manager
}

// NewProcessAdapter 构造 ProcessBackend 适配器。
func NewProcessAdapter(m *process.Manager) *ProcessAdapter { return &ProcessAdapter{m: m} }

func (a *ProcessAdapter) NewSession(spec process.SessionSpec) error { return a.m.NewSession(spec) }
func (a *ProcessAdapter) KillSession(name string) (process.KillResult, error) {
	return a.m.KillSession(name)
}
func (a *ProcessAdapter) RetryReap(tickets []string) ([]string, error) { return a.m.RetryReap(tickets) }
func (a *ProcessAdapter) HasSession(name string) (bool, error)         { return a.m.HasSession(name) }
func (a *ProcessAdapter) ListSessions() ([]string, error)              { return a.m.ListSessions() }
func (a *ProcessAdapter) ShowSessionEnv(name, key string) (string, error) {
	return a.m.ShowSessionEnv(name, key)
}
func (a *ProcessAdapter) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	return a.m.ShowSessionEnvContext(ctx, name, key)
}
func (a *ProcessAdapter) WatchExit(name string, callback func(process.WatchEvent)) (func(), <-chan struct{}) {
	return a.m.WatchExit(name, callback)
}
func (a *ProcessAdapter) AttachPty(name string, cols, rows int) (*pty.Pty, error) {
	return a.m.AttachPty(name, cols, rows)
}
