package task

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
	ocdecksess "ocdeck/internal/domain/session"
	ocdecktask "ocdeck/internal/domain/task"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/pty"
	"ocdeck/internal/infrastructure/store"
)

// --- mock store ---

type mockStore struct {
	mu       sync.Mutex
	projects map[string]ProjectRow
	tasks    map[string]TaskRow
	sessions map[string][]SessionRow // taskID -> sessions
	taskSeq  int
	// 记录状态转移历史，供断言。
	statusCalls []statusCall
	// deleteTaskCount 记录 DeleteTask 调用次数（测试经 deleteTaskCount() accessor 读取，避免直接读字段）。
	deleteTaskCount int
	// deleteDebtCalls 记录 legacy DeleteRecoveryDebt 调用次数（G3-20 单一清债点断言）。
	deleteDebtCalls int
	// recoveryAttempts 按 taskID 记录 D3 permit 时间戳（Unix 秒）。
	recoveryAttempts map[string][]int64
	// recoveryDebts 镜像 store.recovery_debts（G3-3 durable tagged debt）。
	recoveryDebts map[string]RecoveryDebtRow
	// noticeCasErr 非空时 UpdateTaskNoticeCAS 返回该错误（notice 写失败 failpoint，G3-2/G3-8）。
	noticeCasErr error
	// completeRecoveryErr 非空时 CompleteRecoveryFailure 返回该错误（G3-11 intent-first failpoint）。
	completeRecoveryErr error
	// completeAndClearErr 非空时 CompleteRecoveryFailureAndClearDebts 返回该错误
	//（G3-18 单事务 failpoint）。
	completeAndClearErr error
	// recoveryDebtUpsertErr 非空时 UpsertRecoveryDebt 返回该错误（G3-11 落盘失败→内存队列 failpoint）。
	recoveryDebtUpsertErr error
	// onPermit 在 AcquireRecoveryPermit 成功写入后回调（permit 时序 trace，G3-8）。
	onPermit func(taskID string)
	// onLastPort 在 UpdateTaskLastPort 写入前回调（CAS 前/后 failpoint 屏障，G3-8）。
	onLastPort func(taskID string)
	// onGetTask 在 GetTask 读取后回调（G3-16 复核屏障：拦截 checkRecoveryContinuable）。
	onGetTask func(taskID string)
}

type statusCall struct {
	id       string
	from, to string
	updated  bool
}

func newMockStore() *mockStore {
	return &mockStore{
		projects:         map[string]ProjectRow{},
		tasks:            map[string]TaskRow{},
		sessions:         map[string][]SessionRow{},
		recoveryAttempts: map[string][]int64{},
		recoveryDebts:    map[string]RecoveryDebtRow{},
	}
}

func (s *mockStore) seedProject(p ProjectRow) {
	// add-plain-dir-project D8/3.5：缺省回填 kind=repo，避免零值 unknown 触发 fail-closed。
	if p.Kind == "" {
		p.Kind = ProjectKindRepo
	}
	s.projects[p.ID] = p
}

// mutTask 取出任务、修改、放回（Go map 值不可直接赋字段）。
func (s *mockStore) mutTask(id string, fn func(*TaskRow)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	fn(&t)
	s.tasks[id] = t
}

// lastTaskID 返回最后创建的任务 ID（按创建顺序近似）。
func (s *mockStore) lastTaskID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last string
	for id := range s.tasks {
		last = id
	}
	return last
}

func (s *mockStore) GetProject(ctx context.Context, id string) (ProjectRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		return ProjectRow{}, fmt.Errorf("not found")
	}
	return p, nil
}

func (s *mockStore) CreateTask(ctx context.Context, t TaskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.ID]; ok {
		return fmt.Errorf("UNIQUE constraint failed: tasks.id")
	}
	t.CreatedAt = 1
	t.UpdatedAt = 1
	// 与 store schema migration 0007 一致：新建任务 init_status 默认 none
	//（design.md §3：既有任务迁移为 none；新建任务由 Create 链 CommitCreated 按配置落 pending/none）。
	if t.InitStatus == "" {
		t.InitStatus = InitStatusNone
	}
	s.tasks[t.ID] = t
	return nil
}

func (s *mockStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return TaskRow{}, fmt.Errorf("not found")
	}
	// 与 store schema migration 0007 一致：init_status 缺省为 none
	//（design.md §3：既有任务迁移为 none）。测试直接构造 TaskRow 时可能未设置，
	// 读回时归一化为 none，模拟 DB schema 默认值。
	if t.InitStatus == "" {
		t.InitStatus = InitStatusNone
	}
	s.mu.Unlock()
	// onGetTask 读后回调（锁外，G3-16 屏障：复核取值后、返回前阻塞——期间测试
	// 可改状态/关通道，模拟「复核读取与判定之间」的交错窗口）。
	if s.onGetTask != nil {
		s.onGetTask(id)
	}
	return t, nil
}

func (s *mockStore) ListTasksByProject(ctx context.Context, projectID string) ([]TaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskRow
	for _, t := range s.tasks {
		if t.ProjectID == projectID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *mockStore) ListAllTasks(ctx context.Context) ([]TaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TaskRow
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out, nil
}

// ListActiveTaskOverview 镜像 store.ListActiveTaskOverview SQL 语义：
// 仅 active；last_active_at = COALESCE(MAX(归一化后 sessions.last_seen_at), t.updated_at)
// —— 有 session 行时只用 sessions 的 MAX（不含 updated_at），无 session 回退 updated_at；
// last_seen_at ≥1e11 视为毫秒 ÷1000 归一化为秒；按 last_active_at DESC、id ASC。
func (s *mockStore) ListActiveTaskOverview(ctx context.Context) ([]ActiveTaskOverviewRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const msThreshold int64 = 100000000000
	normalize := func(v int64) int64 {
		if v >= msThreshold {
			return v / 1000
		}
		return v
	}
	var out []ActiveTaskOverviewRow
	for _, t := range s.tasks {
		if t.Status != StatusActive {
			continue
		}
		project, ok := s.projects[t.ProjectID]
		if !ok {
			// store 用 INNER JOIN：无项目行不返回。
			continue
		}
		var last int64
		sessions := s.sessions[t.ID]
		if len(sessions) > 0 {
			// COALESCE：有 session 行时取归一化后 sessions 的 MAX，不混入 updated_at。
			last = normalize(sessions[0].LastSeenAt)
			for _, sess := range sessions[1:] {
				if n := normalize(sess.LastSeenAt); n > last {
					last = n
				}
			}
		} else {
			// 无 session → 回退 updated_at（已是秒，无需归一化）。
			last = t.UpdatedAt
		}
		out = append(out, ActiveTaskOverviewRow{
			ID: t.ID, ProjectID: t.ProjectID, ProjectName: project.Name,
			Name: t.Name, Branch: t.Branch, WorktreePath: t.WorktreePath, LastActiveAt: last,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActiveAt != out[j].LastActiveAt {
			return out[i].LastActiveAt > out[j].LastActiveAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *mockStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	t.Status = status
	t.LastError = lastError
	t.UpdatedAt = 2
	s.tasks[id] = t
	return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
}

func (s *mockStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	if t.Status != fromStatus {
		s.statusCalls = append(s.statusCalls, statusCall{id, fromStatus, toStatus, false})
		return application.TransitionResult{}, nil
	}
	t.Status = toStatus
	t.LastError = lastError
	t.UpdatedAt = 3
	s.tasks[id] = t
	s.statusCalls = append(s.statusCalls, statusCall{id, fromStatus, toStatus, true})
	return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
}

func (s *mockStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	t.EnvSnapshot = envSnapshot
	s.tasks[id] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	if s.onLastPort != nil {
		s.onLastPort(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	t.LastPort = sql.NullInt64{Int64: int64(port), Valid: true}
	s.tasks[id] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	t.Notice = notice
	s.tasks[id] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noticeCasErr != nil {
		return application.MutationResult{}, s.noticeCasErr
	}
	t, ok := s.tasks[id]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	if t.Notice != expected {
		return application.MutationResult{}, nil
	}
	t.Notice = newNotice
	s.tasks[id] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

// --- recovery tagged debt mock（G3-3/G3-10：镜像 store.recovery_debts 按
// task_id+session_name 复合键 upsert，同一任务可多条 cleanup_notice 行） ---

func (s *mockStore) UpsertRecoveryDebt(ctx context.Context, row RecoveryDebtRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryDebtUpsertErr != nil {
		return s.recoveryDebtUpsertErr
	}
	if s.recoveryDebts == nil {
		s.recoveryDebts = map[string]RecoveryDebtRow{}
	}
	s.recoveryDebts[row.TaskID+"\x00"+row.SessionName] = row
	return nil
}

func (s *mockStore) DeleteRecoveryDebt(ctx context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteDebtCalls++
	for k := range s.recoveryDebts {
		if taskID == strings.SplitN(k, "\x00", 2)[0] {
			delete(s.recoveryDebts, k)
		}
	}
	return nil
}

// deleteDebtCallsCount 返回 legacy DeleteRecoveryDebt 调用计数（G3-20 断言：
// 清债唯一入口为 CompleteRecoveryFailureAndClearDebts 事务，此计数应为 0）。
func (s *mockStore) deleteDebtCallsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteDebtCalls
}

func (s *mockStore) ListRecoveryDebts(ctx context.Context) ([]RecoveryDebtRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecoveryDebtRow
	for _, r := range s.recoveryDebts {
		out = append(out, r)
	}
	return out, nil
}

func (s *mockStore) SetTaskDeleteMode(ctx context.Context, id, mode string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	t.DeleteMode = sql.NullString{String: mode, Valid: true}
	s.tasks[id] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *mockStore) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	for _, st := range fromStatuses {
		if t.Status == st {
			t.Status = StatusDeleting
			t.DeleteMode = sql.NullString{String: mode, Valid: true}
			s.tasks[id] = t
			return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
		}
	}
	return application.TransitionResult{}, nil
}

func (s *mockStore) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	t.Status = StatusArchived
	s.tasks[id] = t
	return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
}

func (s *mockStore) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	t.Status = StatusSuspended
	s.tasks[id] = t
	return application.TransitionResult{MutationResult: application.MutationResult{Matched: true, Changed: true}}, nil
}

func (s *mockStore) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteTaskCount++
	from := s.tasks[id].Status
	delete(s.tasks, id)
	delete(s.sessions, id)
	return application.DeleteResult{Affected: 1, From: ocdecktask.Status(from)}, nil
}

// deleteTaskCountVal 返回 DeleteTask 调用次数（测试经 accessor 读取，避免直接读字段与并发写竞态）。
func (s *mockStore) deleteTaskCountVal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteTaskCount
}

func (s *mockStore) ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVarRow, error) {
	return nil, nil
}
func (s *mockStore) ListGlobalEnvVars(ctx context.Context) ([]GlobalEnvVarRow, error) {
	return nil, nil
}
func (s *mockStore) ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVarRow, error) {
	return nil, nil
}

func (s *mockStore) UpsertTaskSession(ctx context.Context, sess SessionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[sess.TaskID]
	for i, e := range list {
		if e.SessionID == sess.SessionID {
			if sess.LastSeenAt > e.LastSeenAt {
				list[i].LastSeenAt = sess.LastSeenAt
			}
			s.sessions[sess.TaskID] = list
			return nil
		}
	}
	s.sessions[sess.TaskID] = append(list, sess)
	return nil
}

func (s *mockStore) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 返回拷贝，避免调用方在锁外迭代期间被 UpsertTaskSession/AlignTaskSessions 原地改写
	// 底层数组造成 data race（-race 下会报错）。
	list := s.sessions[taskID]
	out := make([]SessionRow, len(list))
	copy(out, list)
	return out, nil
}

// ListTopLevelTaskSessions 仅返回顶层会话（parent_id 为空），与 store.Queries 语义一致
// （design.md §4 锚定隔离：锚定候选仅取顶层会话）。排序同 store.Queries 的 ORDER BY。
func (s *mockStore) ListTopLevelTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[taskID]
	out := make([]SessionRow, 0, len(list))
	for _, e := range list {
		if e.ParentID == "" {
			out = append(out, e)
		}
	}
	sortSessionRows(out)
	return out, nil
}

// sortSessionRows 按 last_seen_at DESC → session_created_at DESC → session_id DESC 排序，
// 与 store.Queries.ListTopLevelTaskSessions 的 ORDER BY 一致。
func sortSessionRows(rows []SessionRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			if a.LastSeenAt < b.LastSeenAt ||
				(a.LastSeenAt == b.LastSeenAt && a.SessionCreatedAt < b.SessionCreatedAt) ||
				(a.LastSeenAt == b.LastSeenAt && a.SessionCreatedAt == b.SessionCreatedAt && a.SessionID < b.SessionID) {
				rows[j-1], rows[j] = rows[j], rows[j-1]
			} else {
				break
			}
		}
	}
}

// DeleteTaskSession 镜像 store.DeleteTaskSession：删除归属行并返回受影响行数。
func (s *mockStore) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[taskID]
	kept := list[:0]
	removed := 0
	for _, e := range list {
		if e.SessionID == sessionID {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	s.sessions[taskID] = kept
	return removed, nil
}

// --- session 归属隔离 mock（add-plain-dir-project D8，P1.4.5 结构化签名） ---

// ClaimTaskSession 镜像 store.ClaimTaskSession：仅当 sessionID 未被他任务拥有时插入/更新
// 本任务行；ClaimResult.Changed=新插入或 last_seen_at/parent_id 实际推进。
func (s *mockStore) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 查是否被他任务拥有。
	for other, list := range s.sessions {
		if other == taskID {
			continue
		}
		for _, e := range list {
			if e.SessionID == sessionID {
				return application.ClaimResult{Claimed: false, OwnerTaskID: other}, nil
			}
		}
	}
	// 无冲突：upsert 本任务行（Changed=新插入或 last_seen/parent 实际推进）。
	list := s.sessions[taskID]
	for i, e := range list {
		if e.SessionID == sessionID {
			changed := lastSeen > e.LastSeenAt || parentID != e.ParentID
			if lastSeen > e.LastSeenAt {
				list[i].LastSeenAt = lastSeen
			}
			list[i].ParentID = parentID
			s.sessions[taskID] = list
			return application.ClaimResult{Claimed: true, Changed: changed}, nil
		}
	}
	s.sessions[taskID] = append(list, SessionRow{
		TaskID: taskID, SessionID: sessionID, SessionCreatedAt: createdAt,
		FirstSeenAt: firstSeen, LastSeenAt: lastSeen, ParentID: parentID,
	})
	return application.ClaimResult{Claimed: true, Changed: true}, nil
}

func (s *mockStore) ClaimTaskSessionAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	res, err := s.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
	if err != nil || !res.Claimed {
		return res, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	t.AnchorSessionID = sql.NullString{String: sessionID, Valid: true}
	s.tasks[taskID] = t
	return res, nil
}

func (s *mockStore) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok || !t.AnchorSessionID.Valid || t.AnchorSessionID.String != oldAnchor {
		return application.MutationResult{}, nil
	}
	t.AnchorSessionID = sql.NullString{}
	s.tasks[taskID] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

const mockRecoveryPermitWindowSec int64 = 5 * 60
const mockRecoveryPermitMax = 3

func (s *mockStore) AcquireRecoveryPermit(ctx context.Context, taskID string, now int64) (AcquirePermitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryAttempts == nil {
		s.recoveryAttempts = map[string][]int64{}
	}
	windowStart := now - mockRecoveryPermitWindowSec
	kept := s.recoveryAttempts[taskID][:0]
	for _, ts := range s.recoveryAttempts[taskID] {
		if ts >= windowStart {
			kept = append(kept, ts)
		}
	}
	s.recoveryAttempts[taskID] = kept
	if len(kept) >= mockRecoveryPermitMax {
		return AcquirePermitResult{}, nil
	}
	s.recoveryAttempts[taskID] = append(s.recoveryAttempts[taskID], now)
	if s.onPermit != nil {
		s.onPermit(taskID)
	}
	return AcquirePermitResult{Acquired: true, Ordinal: len(s.recoveryAttempts[taskID])}, nil
}

// recoveryPermitCount 持锁返回该任务当前窗口内的 permit 记录数：测试断言（含与
// 进行中恢复 goroutine 并发的轮询断言）MUST 经此读取，不得无锁直读字段。
func (s *mockStore) recoveryPermitCount(taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recoveryAttempts[taskID])
}

func (s *mockStore) CompleteRecoveryFailure(ctx context.Context, id string, lastError sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completeRecoveryErr != nil {
		return application.TransitionResult{}, s.completeRecoveryErr
	}
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	if t.Status != StatusActivating {
		return application.TransitionResult{}, nil
	}
	t.Status = StatusSuspended
	t.LastError = lastError
	t.EnvSnapshot = sql.NullString{}
	t.UpdatedAt = 4
	s.tasks[id] = t
	return application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true},
		StatusChanged:  true,
	}, nil
}

// --- G3-18 mock：activating 准入拒绝未清 recovery debt + Complete/清 debt 单事务 ---

// CasActivationIfNoRecoveryDebt 镜像 store.CasActivationIfNoRecoveryDebt：存在任一
// recovery debt 行 → ErrRecoveryDebtPresent 零修改；否则按 CAS 迁移状态。
func (s *mockStore) CasActivationIfNoRecoveryDebt(ctx context.Context, id, fromStatus, toStatus string) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recoveryDebts) > 0 {
		for k := range s.recoveryDebts {
			if id == strings.SplitN(k, "\x00", 2)[0] {
				return application.TransitionResult{}, store.ErrRecoveryDebtPresent
			}
		}
	}
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	if t.Status != fromStatus {
		return application.TransitionResult{}, nil
	}
	t.Status = toStatus
	s.tasks[id] = t
	return application.TransitionResult{
		MutationResult: application.MutationResult{Matched: true, Changed: true},
		StatusChanged:  true,
	}, nil
}

// CompleteRecoveryFailureAndClearDebts 镜像单事务：completeRecoveryFailure 语义 +
// 删除该任务全部 debt 行（含 CAS 失配分支）；completeAndClearErr 注入失败。
func (s *mockStore) CompleteRecoveryFailureAndClearDebts(ctx context.Context, id string, lastError sql.NullString) (application.TransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completeAndClearErr != nil {
		return application.TransitionResult{}, s.completeAndClearErr
	}
	if s.completeRecoveryErr != nil {
		return application.TransitionResult{}, s.completeRecoveryErr
	}
	t, ok := s.tasks[id]
	if !ok {
		return application.TransitionResult{}, fmt.Errorf("not found")
	}
	var res application.TransitionResult
	if t.Status == StatusActivating {
		t.Status = StatusSuspended
		t.LastError = lastError
		t.EnvSnapshot = sql.NullString{}
		t.UpdatedAt = 4
		s.tasks[id] = t
		res = application.TransitionResult{
			MutationResult: application.MutationResult{Matched: true, Changed: true},
			StatusChanged:  true,
		}
	}
	// CAS 失配也删 debt（服从 DB 最新状态）。
	for k := range s.recoveryDebts {
		if id == strings.SplitN(k, "\x00", 2)[0] {
			delete(s.recoveryDebts, k)
		}
	}
	return res, nil
}

// TouchOwnedTaskSession 镜像 store.TouchOwnedTaskSession：Matched=命中本任务归属行，
// Changed=last_seen_at 真实推进（值变化条件），绝不插入。
func (s *mockStore) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[taskID]
	for i, e := range list {
		if e.SessionID == sessionID {
			if lastSeenAt > e.LastSeenAt {
				list[i].LastSeenAt = lastSeenAt
				s.sessions[taskID] = list
				return application.MutationResult{Matched: true, Changed: true}, nil
			}
			return application.MutationResult{Matched: true}, nil
		}
	}
	return application.MutationResult{}, nil
}

// AlignTaskSessions 镜像 store.AlignTaskSessions：按 mode 对齐（repo 逐个 claim、冲突上报、
// ownedOnly 仅刷新 listed∩owned），complete 删 owned 缺席行并对 tasks.notice 做 CAS 镜像
// （expected 失配返回 application.AlignConflict；同值 no-op），返回结构化 AlignResult。
func (s *mockStore) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res application.AlignResult
	if mode != AlignModeRepo && mode != AlignModeOwnedOnly {
		return res, fmt.Errorf("mock: unknown AlignMode %d", mode)
	}
	// owned 集合 O。
	ownedSet := map[string]bool{}
	for _, e := range s.sessions[taskID] {
		ownedSet[e.SessionID] = true
	}
	var affected []ocdecksess.ID
	switch mode {
	case AlignModeRepo:
		var conflicts []ocdecksess.ID
		for _, ob := range listed {
			// 查是否被他任务拥有。
			var owner string
			for other, list := range s.sessions {
				if other == taskID {
					continue
				}
				for _, e := range list {
					if e.SessionID == ob.SessionID {
						owner = other
						break
					}
				}
				if owner != "" {
					break
				}
			}
			if owner != "" {
				conflicts = append(conflicts, ocdecksess.ID(ob.SessionID))
				continue
			}
			// upsert 本任务行（Changed 判定镜像生产 claim）。
			list := s.sessions[taskID]
			found := false
			changed := false
			for i, e := range list {
				if e.SessionID == ob.SessionID {
					changed = ob.UpdatedAt > e.LastSeenAt || ob.ParentID != e.ParentID
					if ob.UpdatedAt > e.LastSeenAt {
						list[i].LastSeenAt = ob.UpdatedAt
					}
					list[i].ParentID = ob.ParentID
					found = true
					break
				}
			}
			if !found {
				s.sessions[taskID] = append(list, SessionRow{
					TaskID: taskID, SessionID: ob.SessionID, SessionCreatedAt: ob.CreatedAt,
					FirstSeenAt: nowUnixI(), LastSeenAt: ob.UpdatedAt, ParentID: ob.ParentID,
				})
				changed = true
			}
			if changed {
				if ownedSet[ob.SessionID] {
					res.Touched++
				} else {
					res.Inserted++
				}
				affected = append(affected, ocdecksess.ID(ob.SessionID))
			}
		}
		res.Conflicts = conflicts
	case AlignModeOwnedOnly:
		for _, ob := range listed {
			if !ownedSet[ob.SessionID] {
				continue
			}
			list := s.sessions[taskID]
			for i, e := range list {
				if e.SessionID == ob.SessionID {
					if ob.UpdatedAt > e.LastSeenAt {
						list[i].LastSeenAt = ob.UpdatedAt
						s.sessions[taskID] = list
						res.Touched++
						affected = append(affected, ocdecksess.ID(ob.SessionID))
					}
					break
				}
			}
		}
	}
	if complete {
		keep := map[string]bool{}
		for _, ob := range listed {
			keep[ob.SessionID] = true
		}
		list := s.sessions[taskID]
		out := list[:0]
		for _, e := range list {
			if keep[e.SessionID] {
				out = append(out, e)
			} else {
				res.Deleted++
				affected = append(affected, ocdecksess.ID(e.SessionID))
			}
		}
		s.sessions[taskID] = out
		tm, nerr := s.alignNoticeLocked(taskID, notice)
		if nerr != nil {
			return res, nerr
		}
		res.TaskMutation = tm
	}
	for _, e := range s.sessions[taskID] {
		res.OwnedSessionIDs = append(res.OwnedSessionIDs, ocdecksess.ID(e.SessionID))
	}
	res.AffectedSessionIDs = affected
	return res, nil
}

// alignNoticeLocked 镜像 store.alignNoticeInTx 的 CAS 语义（调用方持 s.mu）。
func (s *mockStore) alignNoticeLocked(taskID string, mut application.NoticeMutation) (application.MutationResult, error) {
	t, ok := s.tasks[taskID]
	if !ok {
		return application.MutationResult{}, fmt.Errorf("not found")
	}
	if t.Notice != ptrToNullString(mut.Expected) {
		cur := t.Notice
		return application.MutationResult{}, &application.AlignConflict{TaskID: taskID, Expected: mut.Expected, Actual: nullStringToPtr(cur)}
	}
	next := ptrToNullString(mut.New)
	if t.Notice == next {
		return application.MutationResult{Matched: true}, nil
	}
	t.Notice = next
	s.tasks[taskID] = t
	return application.MutationResult{Matched: true, Changed: true}, nil
}

// --- mock process backend ---

type mockProc struct {
	mu            sync.Mutex
	sessions      map[string]bool
	killResults   map[string]process.KillResult
	newSessionErr error
	// hasSessionErr 非空时 HasSession 返回该错误（模拟基础设施错误，非 ErrNoTmuxServer）。
	hasSessionErr error
	// killSessionErr 非空时 KillSession 返回该错误（基础设施错误 failpoint，G3-8 表驱动）。
	killSessionErr error
	watchCb        map[string]func(process.WatchEvent)
	envValues      map[string]map[string]string // sessionName -> env
	// cmdArgvValues 记录 NewSession 的 CmdArgv（§4 锚定测试断言 --session <id>）。
	cmdArgvValues map[string][]string
	// killOrder 记录 KillSession 调用顺序（D1 测试断言 kill 顺序 tui→shells→serve）。
	killOrder []string
	// newSessionNames 记录 NewSession 调用顺序（D2 测试断言 serve 死亡不新建 tui）。
	newSessionNames []string
	// onNewSession 在 NewSession 成功后回调（permit 时序 trace，G3-8）。测试注入。
	onNewSession func(name string)
}

func newMockProc() *mockProc {
	return &mockProc{
		sessions:      map[string]bool{},
		killResults:   map[string]process.KillResult{},
		watchCb:       map[string]func(process.WatchEvent){},
		envValues:     map[string]map[string]string{},
		cmdArgvValues: map[string][]string{},
	}
}

func (p *mockProc) NewSession(spec process.SessionSpec) error {
	if err := process.ValidateNewSessionName(spec.Name); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.newSessionErr != nil {
		return p.newSessionErr
	}
	p.newSessionNames = append(p.newSessionNames, spec.Name)
	p.sessions[spec.Name] = true
	p.envValues[spec.Name] = spec.Env
	p.cmdArgvValues[spec.Name] = spec.CmdArgv
	if p.onNewSession != nil {
		p.onNewSession(spec.Name)
	}
	return nil
}

func (p *mockProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killOrder = append(p.killOrder, name)
	if p.killSessionErr != nil {
		return process.KillResult{}, p.killSessionErr
	}
	if res, ok := p.killResults[name]; ok {
		// 仅当 SessionKilled=true 时才从 sessions 移除（模拟真实 kill-session 语义）。
		if res.SessionKilled {
			delete(p.sessions, name)
		}
		return res, nil
	}
	delete(p.sessions, name)
	return process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}, nil
}

func (p *mockProc) RetryReap(tickets []string) ([]string, error) { return nil, nil }

func (p *mockProc) HasSession(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hasSessionErr != nil {
		return false, p.hasSessionErr
	}
	return p.sessions[name], nil
}

func (p *mockProc) ListSessions() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for n := range p.sessions {
		out = append(out, n)
	}
	return out, nil
}

func (p *mockProc) ShowSessionEnv(name, key string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if env, ok := p.envValues[name]; ok {
		return env[key], nil
	}
	return "", nil
}

func (p *mockProc) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	return p.ShowSessionEnv(name, key)
}

func (p *mockProc) WatchExit(name string, callback func(process.WatchEvent)) (func(), <-chan struct{}) {
	p.mu.Lock()
	p.watchCb[name] = callback
	p.mu.Unlock()
	done := make(chan struct{})
	return func() {
		p.mu.Lock()
		delete(p.watchCb, name)
		p.mu.Unlock()
		// mock 无后台 goroutine，cancel 即等价于 goroutine 退出：关闭 done 供 join。
		close(done)
	}, done
}

// triggerExit 触发会话退出事件（测试用）。
func (p *mockProc) triggerExit(name string, ev process.WatchEvent) {
	p.mu.Lock()
	cb := p.watchCb[name]
	if ev.Type == process.WatchEventSessionExit {
		delete(p.sessions, name)
	}
	p.mu.Unlock()
	if cb != nil {
		cb(ev)
	}
}

func (p *mockProc) AttachPty(name string, cols, rows int) (*pty.Pty, error) {
	return nil, fmt.Errorf("attach not supported in mock")
}

// killOrderSnapshot 返回 KillSession 调用顺序的拷贝（D1 测试用）。
func (p *mockProc) killOrderSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.killOrder))
	copy(out, p.killOrder)
	return out
}

// newSessionNamesSnapshot 返回 NewSession 调用顺序的拷贝（D2 测试用）。
func (p *mockProc) newSessionNamesSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.newSessionNames))
	copy(out, p.newSessionNames)
	return out
}

// --- mock worktree backend ---

// mockWorktree 的 map 字段在并发 Create（TestKeyedMutex_ConcurrentCreate 等）下被多 goroutine
// 同时读写，MUST 以 mu 串行化所有访问，否则 -race 报 data race。
type mockWorktree struct {
	mu              sync.Mutex
	addErr          error
	removeErr       error
	addedPaths      map[string]bool
	branches        map[string]bool // branch -> exists
	products        map[string]bool // wtPath -> valid product
	preflightErr    error
	dirtyFiles      map[string]map[string]struct{} // wtPath -> dirty file set (B7c 二次门禁)
	dirtyErr        error
	removeCallCount int // Remove 调用计数（pre-delete admission 失败测试断言未调用 wt.Remove）
}

func newMockWorktree() *mockWorktree {
	return &mockWorktree{
		addedPaths: map[string]bool{},
		branches:   map[string]bool{},
		products:   map[string]bool{},
		dirtyFiles: map[string]map[string]struct{}{},
	}
}

func (w *mockWorktree) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.addErr != nil {
		return w.addErr
	}
	w.addedPaths[dest] = true
	w.branches[branch] = true
	w.products[dest] = true
	return nil
}

func (w *mockWorktree) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeCallCount++
	return w.removeErr
}

func (w *mockWorktree) removeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.removeCallCount
}

func (w *mockWorktree) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.branches[branch], nil
}

func (w *mockWorktree) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	return nil
}

// ResolveBaseRef 满足 WorktreeBackend 新增端口（add-plain-dir-project D10）。
// 默认返回 `refs/heads/<shortName>`，供 repo 创建路径默认/存在分支测试使用；
// 具体分支解析行为在 crud 测试的独立 mock 中覆盖。
func (w *mockWorktree) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	if shortName == "" {
		return "", fmt.Errorf("empty base_ref short name")
	}
	return "refs/heads/" + shortName, nil
}

func (w *mockWorktree) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.products[wtPath] {
		return fmt.Errorf("worktree product: path missing: %s", wtPath)
	}
	return nil
}

func (w *mockWorktree) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.preflightErr
}

func (w *mockWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dirtyErr != nil {
		return nil, w.dirtyErr
	}
	// 返回拷贝避免外部改 mock 内部状态。
	src := w.dirtyFiles[wtPath]
	out := make(map[string]struct{}, len(src))
	for k := range src {
		out[k] = struct{}{}
	}
	return out, nil
}

// --- mock OCClient ---

type mockOC struct {
	// mu 串行化 sessions 的并发读写（附带项：SSE 对齐 goroutine 读 ListSessions 与
	// 恢复/激活路径 CreateSession append 并发，-race 下报 data race）。测试在并发
	// 开始前的直接字段赋值经 goroutine 创建构成 happens-before，不受影响。
	mu            sync.Mutex
	probeErr      error
	healthOK      bool
	sessions      []opencode.Session
	getSessionErr error
	deleteErr     error
	// deleteErrByID 按 sessionID 返回特定错误（D3 测试：逐项错误聚合，非短路）。
	deleteErrByID map[string]error
	subscribeErr  error
	onReadyCh     chan struct{}
	// sessionStatuses 预置 /session/status 返回（2.8 agentStatus 测试）。
	sessionStatuses map[string]opencode.SessionStatus
	// sessionStatusErr 非空时 SessionStatus 返回该错误（降级测试）。
	sessionStatusErr error
	// createSessionResult / createSessionErr 控制 CreateSession 返回（§4 锚定测试）。
	// createSessionCount 用 atomic 计数（测试桩线程安全：TestKeyedMutex_ConcurrentCreate 等并发测试
	// 两个 auto-activate 同时调用 CreateSession，非 atomic 会触发 -race）。
	createSessionResult opencode.Session
	createSessionErr    error
	createSessionCount  int64
	// 注意力信号（D6）：ListPermissions/ListQuestions 返回。
	listPermissionsResult []opencode.PermissionRequest
	listPermissionsErr    error
	listQuestionsResult   []opencode.QuestionRequest
	listQuestionsErr      error
}

func newMockOC(healthOK bool) *mockOC {
	return &mockOC{healthOK: healthOK, onReadyCh: make(chan struct{}, 1), deleteErrByID: map[string]error{}}
}

func (c *mockOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	if !c.healthOK {
		return opencode.HealthResponse{}, opencode.ErrServeNotReady
	}
	return opencode.HealthResponse{Healthy: true, Version: opencode.ContractBaseline}, nil
}

func (c *mockOC) Probe(ctx context.Context) (string, error) {
	if c.probeErr != nil {
		return "", c.probeErr
	}
	return opencode.ContractBaseline, nil
}

func (c *mockOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]opencode.Session, len(c.sessions))
	copy(out, c.sessions)
	return out, nil
}

func (c *mockOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	if c.getSessionErr != nil {
		return opencode.Session{}, c.getSessionErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return opencode.Session{}, opencode.ErrSessionNotFound
}

// CreateSession 模拟 POST /session 创建会话（§4 锚定测试）。
// 默认返回新 session "sess-new"；createSessionResult/createSessionErr 可覆盖。
func (c *mockOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	atomic.AddInt64(&c.createSessionCount, 1)
	if c.createSessionErr != nil {
		return opencode.Session{}, c.createSessionErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.createSessionResult.ID != "" {
		c.sessions = append(c.sessions, c.createSessionResult)
		return c.createSessionResult, nil
	}
	s := opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 100, Updated: 100}}
	c.sessions = append(c.sessions, s)
	return s, nil
}

// createSessionCountLoad 返回 createSessionCount 的原子读（测试桩线程安全：
// atomic 写与直接读混用在 -race 下报 data race，所有测试读经此 accessor）。
func (c *mockOC) createSessionCountLoad() int64 {
	return atomic.LoadInt64(&c.createSessionCount)
}

func (c *mockOC) DeleteSession(ctx context.Context, dir, id string) error {
	if err, ok := c.deleteErrByID[id]; ok {
		return err
	}
	return c.deleteErr
}

func (c *mockOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	if c.sessionStatusErr != nil {
		return nil, c.sessionStatusErr
	}
	out := make(map[string]opencode.SessionStatus, len(c.sessionStatuses))
	for k, v := range c.sessionStatuses {
		out[k] = v
	}
	return out, nil
}

func (c *mockOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	// 立即触发 onReady（测试中由 onReadyCh 协调）。
	select {
	case c.onReadyCh <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *mockOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	if c.listPermissionsErr != nil {
		return nil, c.listPermissionsErr
	}
	out := make([]opencode.PermissionRequest, len(c.listPermissionsResult))
	copy(out, c.listPermissionsResult)
	return out, nil
}

func (c *mockOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	if c.listQuestionsErr != nil {
		return nil, c.listQuestionsErr
	}
	out := make([]opencode.QuestionRequest, len(c.listQuestionsResult))
	copy(out, c.listQuestionsResult)
	return out, nil
}

// --- test manager builder ---

func newTestManager(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	// wrapOC 包装 mock，使 Options.OnReady 在 SubscribeEvents 首次调用时同步触发。
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt,
		OCFactory: wrap,
	})
	// 测试默认跳过 D3 退避，避免 watcher/SSE 恢复把套件拖进 5s/15s/45s。
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	return m
}

// readyOC 在 SubscribeEvents 时同步触发 onReady（测试用，避免 startSSE 超时）。
type readyOC struct {
	inner   OCClient
	onReady func()
}

func (c *readyOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *readyOC) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *readyOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *readyOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *readyOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *readyOC) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *readyOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *readyOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReady != nil {
		c.onReady()
	}
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *readyOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return c.inner.ListPermissions(ctx, dir)
}
func (c *readyOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return c.inner.ListQuestions(ctx, dir)
}
