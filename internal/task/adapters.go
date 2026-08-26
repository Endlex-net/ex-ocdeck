package task

import (
	"context"
	"database/sql"

	"ocdeck/internal/application"
	ocdecktask "ocdeck/internal/domain/task"
	ocdecksess "ocdeck/internal/domain/session"
	"ocdeck/internal/infrastructure/git"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/pty"
	"ocdeck/internal/infrastructure/store"
	"ocdeck/internal/infrastructure/worktree"
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

func (a *StoreAdapter) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatus(ctx, id, ocdecktask.Status(status), nullStringToPtr(lastError))
}
func (a *StoreAdapter) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	return a.db.UpdateTaskStatusConditional(ctx, id, ocdecktask.Status(fromStatus), ocdecktask.Status(toStatus), nullStringToPtr(lastError))
}
func (a *StoreAdapter) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	return a.db.UpdateTaskEnvSnapshot(ctx, id, nullStringToPtr(envSnapshot))
}
func (a *StoreAdapter) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	return a.db.UpdateTaskLastPort(ctx, id, port)
}
func (a *StoreAdapter) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) (application.MutationResult, error) {
	return a.db.UpdateTaskNotice(ctx, id, nullStringToPtr(notice))
}
func (a *StoreAdapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	return a.db.UpdateTaskNoticeCAS(ctx, id, nullStringToPtr(expected), nullStringToPtr(newNotice))
}
func (a *StoreAdapter) SetTaskDeleteMode(ctx context.Context, id, mode string) (application.MutationResult, error) {
	return a.db.SetTaskDeleteMode(ctx, id, ocdecktask.DeleteMode(mode))
}
func (a *StoreAdapter) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (application.TransitionResult, error) {
	fs := make([]ocdecktask.Status, len(fromStatuses))
	for i, s := range fromStatuses {
		fs[i] = ocdecktask.Status(s)
	}
	return a.db.BeginDeleteIntent(ctx, id, ocdecktask.DeleteMode(mode), fs)
}
func (a *StoreAdapter) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.ArchiveTask(ctx, id)
}
func (a *StoreAdapter) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	return a.db.RestoreTask(ctx, id)
}
func (a *StoreAdapter) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
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

func (a *StoreAdapter) CommitCreated(ctx context.Context, taskID, expectedStatus, initStatus string) (application.TransitionResult, error) {
	return a.db.CommitCreated(ctx, taskID, ocdecktask.Status(expectedStatus), ocdecktask.InitStatus(initStatus))
}

func (a *StoreAdapter) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRun(ctx, taskID)
}

func (a *StoreAdapter) ClaimInitRerun(ctx context.Context, taskID string) (application.MutationResult, error) {
	return a.db.ClaimInitRerun(ctx, taskID)
}

func (a *StoreAdapter) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (application.MutationResult, error) {
	return a.db.FinishInitRun(ctx, taskID, ocdecktask.InitStatus(status), nullStringToPtr(initError))
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
func (a *StoreAdapter) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	return a.db.DeleteTaskSession(ctx, taskID, sessionID)
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
// 返回结构化 ClaimResult（P1.4.5：Claimed/Changed/OwnerTaskID）。
func (a *StoreAdapter) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	return a.db.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}

// TouchOwnedTaskSession 条件 UPDATE 仅本任务已归属行的 last_seen_at（D8）。
// 返回结构化 MutationResult（P1.4.5：Matched=命中归属行，Changed=值真实推进）。
func (a *StoreAdapter) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	return a.db.TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt)
}

// AlignTaskSessions 单事务原子对齐（D8 + design.md D0:80/86）：task 层 AlignMode/
// SessionObservation 映射到 store 层类型；notice 以 NoticeMutation 事务内 CAS 提交。
func (a *StoreAdapter) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	obs := make([]store.SessionObservation, 0, len(listed))
	for _, s := range listed {
		obs = append(obs, store.SessionObservation{
			SessionID: s.SessionID, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, ParentID: s.ParentID,
		})
	}
	return a.db.AlignTaskSessions(ctx, taskID, toStoreAlignMode(mode), obs, complete, notice)
}

// storeAlignPortsAdapter 把 legacy TaskStore 适配为 apptask.AlignPorts（P1.4.5 迁移期
// legacy 直连路径共用 RunAlign 编排，避免 align 决策/事务边界逻辑双写）。
type storeAlignPortsAdapter struct {
	store TaskStore
}

// GetTaskRow 读全行快照：TaskStore.GetTask → application.TaskSnapshot（nullable 转指针）。
func (a storeAlignPortsAdapter) GetTaskRow(ctx context.Context, id string) (application.TaskSnapshot, error) {
	row, err := a.store.GetTask(ctx, id)
	if err != nil {
		return application.TaskSnapshot{}, err
	}
	return taskRowToSnapshot(row), nil
}

// UpdateTaskNoticeCAS 乐观更新 notice（sql.NullString ↔ *string 转换）。
func (a storeAlignPortsAdapter) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice *string) (application.MutationResult, error) {
	return a.store.UpdateTaskNoticeCAS(ctx, id, ptrToNullString(expected), ptrToNullString(newNotice))
}

// Align 委托 TaskStore.AlignTaskSessions（Observation/AlignMode 映射）。
func (a storeAlignPortsAdapter) Align(ctx context.Context, taskID string, mode ocdecksess.AlignMode, observed []ocdecksess.Observation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	listed := make([]SessionObservation, 0, len(observed))
	for _, o := range observed {
		listed = append(listed, SessionObservation{
			SessionID: string(o.ID), CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, ParentID: o.ParentID,
		})
	}
	return a.store.AlignTaskSessions(ctx, taskID, fromDomainAlignMode(mode), listed, complete, notice)
}

// taskRowToSnapshot 把 task.TaskRow 映射为 application.TaskSnapshot（P1.4.5 供 legacy
// align 端口适配读取 notice 原文；与 taskSnapshotToTaskRow 互逆）。
func taskRowToSnapshot(r TaskRow) application.TaskSnapshot {
	return application.TaskSnapshot{
		ID:           r.ID,
		ProjectID:    r.ProjectID,
		Name:         r.Name,
		Branch:       r.Branch,
		Status:       r.Status,
		WorktreePath: r.WorktreePath,
		LastPort:     ptrToNullInt64ToPtr(r.LastPort),
		LastError:    nullStringToPtr(r.LastError),
		Notice:       nullStringToPtr(r.Notice),
		DeleteMode:   nullStringToPtr(r.DeleteMode),
		EnvSnapshot:  nullStringToPtr(r.EnvSnapshot),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		ArchivedAt:   ptrToNullInt64ToPtr(r.ArchivedAt),
		InitStatus:      r.InitStatus,
		InitError:       nullStringToPtr(r.InitError),
		BaseRef:         r.BaseRef,
		AnchorSessionID: nullStringToPtr(r.AnchorSessionID),
	}
}

// ptrToNullInt64ToPtr 把 sql.NullInt64 映射为 *int64（Invalid → nil）。
func ptrToNullInt64ToPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// fromDomainAlignMode 映射 domain/session.AlignMode → task 层 AlignMode。
// 未知值映射为零值，由 store 层 AlignTaskSessions fail-closed 拒绝。
func fromDomainAlignMode(mode ocdecksess.AlignMode) AlignMode {
	switch mode {
	case ocdecksess.AlignModeRepo:
		return AlignModeRepo
	case ocdecksess.AlignModeOwnedOnly:
		return AlignModeOwnedOnly
	default:
		return 0
	}
}

// toDomainAlignMode 映射 task 层 AlignMode → domain/session.AlignMode。
// 未知值映射为零值，由 store 层 fail-closed 拒绝。
func toDomainAlignMode(mode AlignMode) ocdecksess.AlignMode {
	switch mode {
	case AlignModeRepo:
		return ocdecksess.AlignModeRepo
	case AlignModeOwnedOnly:
		return ocdecksess.AlignModeOwnedOnly
	default:
		return 0
	}
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

// nullStringToPtr 把 sql.NullString 映射为 *string：Invalid → nil。
// 供 StoreAdapter 把 legacy TaskStore 的 sql.NullString 参数转换为 store 层 *string。
func nullStringToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

// rehydrateGuardView 把 legacy TaskRow 重建为 domain Task guard 视图（design D0 P1.4.2）。
//
// strangler 第二步：现状 guard 判断（Archive/Restore/Delete/Activate init_status/Suspend）
// 委托 domain/task 的 CanArchive/CanRestore/CanDelete/CanActivate/CanSuspend。本 helper
// 从 legacy TaskRow 行值构造 domain guard 所需的最小字段子集（status/init_status），
// notices 不填（notice 维度的判断仍由 legacy hasRetryableNotice 完成，保证 notice JSON
// 损坏 fail-closed 语义与错误码映射 byte-equivalent）。
//
// 调用方在拿到 TaskRow（GetTask 已将 init_status 归一化为 none）后调用本 helper，
// 再调用 domain Can* guard；guard 拒绝时调用方按现状维度顺序与错误模板生成 OpError，
// 保持委托前后行为字节级等价。
func rehydrateGuardView(row TaskRow) *ocdecktask.Task {
	return ocdecktask.Rehydrate(ocdecktask.GuardView{
		Status:     ocdecktask.Status(row.Status),
		InitStatus: ocdecktask.InitStatus(row.InitStatus),
	})
}

func toTaskRow(t store.TaskRow) TaskRow {
	return TaskRow{
		ID: t.ID, ProjectID: t.ProjectID, Name: t.Name, Branch: t.Branch, Status: t.Status,
		WorktreePath: t.WorktreePath, LastPort: t.LastPort, LastError: t.LastError, Notice: t.Notice,
		DeleteMode: t.DeleteMode, EnvSnapshot: t.EnvSnapshot, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ArchivedAt: t.ArchivedAt, InitStatus: t.InitStatus, InitError: t.InitError, BaseRef: t.BaseRef,
		AnchorSessionID: t.AnchorSessionID,
	}
}

// taskSnapshotToTaskRow 把 application.TaskSnapshot 还原为 task.TaskRow（design D0 P1.4.4）。
//
// 迁移期 api.TaskBackend 契约返回 task.TaskRow（冻结不变量），LifecycleService.Get/List
// 返回 application.TaskSnapshot，经本 helper 还原为 task.TaskRow，保持逐字段等价。
// *string/*int64 还原为 sql.NullString/sql.NullInt64（nil → Invalid）。
func taskSnapshotToTaskRow(s application.TaskSnapshot) TaskRow {
	return TaskRow{
		ID:           s.ID,
		ProjectID:    s.ProjectID,
		Name:         s.Name,
		Branch:       s.Branch,
		Status:       s.Status,
		WorktreePath: s.WorktreePath,
		LastPort:     ptrToNullInt64(s.LastPort),
		LastError:    ptrToNullString(s.LastError),
		Notice:       ptrToNullString(s.Notice),
		DeleteMode:   ptrToNullString(s.DeleteMode),
		EnvSnapshot:  ptrToNullString(s.EnvSnapshot),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		ArchivedAt:   ptrToNullInt64(s.ArchivedAt),
		InitStatus:      s.InitStatus,
		InitError:       ptrToNullString(s.InitError),
		BaseRef:         s.BaseRef,
		AnchorSessionID: ptrToNullString(s.AnchorSessionID),
	}
}

// ptrToNullString 把 *string 还原为 sql.NullString（nil → Invalid）。
func ptrToNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

// ptrToNullInt64 把 *int64 还原为 sql.NullInt64（nil → Invalid）。
func ptrToNullInt64(n *int64) sql.NullInt64 {
	if n == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *n, Valid: true}
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
// design.md §19）。委托 internal/infrastructure/git.ValidateBranchName，保持 worktree 包不引入新公开方法。
func (a *WorktreeAdapter) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return git.ValidateBranchName(ctx, repoPath, branch)
}

// ResolveBaseRef 将 base_ref 短名解析为全限定 ref（add-plain-dir-project D10）。
// 委托 internal/infrastructure/git.ResolveBaseRef；task 包不直接依赖 internal/infrastructure/git，经此端口解析。
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
