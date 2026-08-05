package task

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"ocdeck/internal/config"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
	"ocdeck/internal/pty"
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
}

type statusCall struct {
	id       string
	from, to string
	updated  bool
}

func newMockStore() *mockStore {
	return &mockStore{projects: map[string]ProjectRow{}, tasks: map[string]TaskRow{}, sessions: map[string][]SessionRow{}}
}

func (s *mockStore) seedProject(p ProjectRow) { s.projects[p.ID] = p }

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
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return TaskRow{}, fmt.Errorf("not found")
	}
	// 与 store schema migration 0007 一致：init_status 缺省为 none
	//（design.md §3：既有任务迁移为 none）。测试直接构造 TaskRow 时可能未设置，
	// 读回时归一化为 none，模拟 DB schema 默认值。
	if t.InitStatus == "" {
		t.InitStatus = InitStatusNone
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

func (s *mockStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.Status = status
	t.LastError = lastError
	t.UpdatedAt = 2
	s.tasks[id] = t
	return nil
}

func (s *mockStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, fmt.Errorf("not found")
	}
	if t.Status != fromStatus {
		s.statusCalls = append(s.statusCalls, statusCall{id, fromStatus, toStatus, false})
		return false, nil
	}
	t.Status = toStatus
	t.LastError = lastError
	t.UpdatedAt = 3
	s.tasks[id] = t
	s.statusCalls = append(s.statusCalls, statusCall{id, fromStatus, toStatus, true})
	return true, nil
}

func (s *mockStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.EnvSnapshot = envSnapshot
	s.tasks[id] = t
	return nil
}

func (s *mockStore) UpdateTaskLastPort(ctx context.Context, id string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.LastPort = sql.NullInt64{Int64: int64(port), Valid: true}
	s.tasks[id] = t
	return nil
}

func (s *mockStore) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.Notice = notice
	s.tasks[id] = t
	return nil
}

func (s *mockStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, fmt.Errorf("not found")
	}
	if t.Notice != expected {
		return false, nil
	}
	t.Notice = newNotice
	s.tasks[id] = t
	return true, nil
}

func (s *mockStore) SetTaskDeleteMode(ctx context.Context, id, mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.DeleteMode = sql.NullString{String: mode, Valid: true}
	s.tasks[id] = t
	return nil
}

func (s *mockStore) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false, fmt.Errorf("not found")
	}
	for _, st := range fromStatuses {
		if t.Status == st {
			t.Status = StatusDeleting
			t.DeleteMode = sql.NullString{String: mode, Valid: true}
			s.tasks[id] = t
			return true, nil
		}
	}
	return false, nil
}

func (s *mockStore) ArchiveTask(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.Status = StatusArchived
	s.tasks[id] = t
	return nil
}

func (s *mockStore) RestoreTask(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	t.Status = StatusSuspended
	s.tasks[id] = t
	return nil
}

func (s *mockStore) DeleteTask(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteTaskCount++
	delete(s.tasks, id)
	delete(s.sessions, id)
	return nil
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
	// 返回拷贝，避免调用方在锁外迭代期间被 UpsertTaskSession/AlignSessions 原地改写
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

func (s *mockStore) DeleteTaskSession(ctx context.Context, taskID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[taskID]
	out := list[:0]
	for _, e := range list {
		if e.SessionID != sessionID {
			out = append(out, e)
		}
	}
	s.sessions[taskID] = out
	return nil
}

func (s *mockStore) AlignSessions(ctx context.Context, taskID string, sessions []SessionRow, complete bool, noticeFn func(sql.NullString) sql.NullString) error {
	s.mu.Lock()
	s.sessions[taskID] = append([]SessionRow(nil), sessions...)
	s.mu.Unlock()
	return nil
}

// --- mock process backend ---

type mockProc struct {
	mu            sync.Mutex
	sessions      map[string]bool
	killResults   map[string]process.KillResult
	newSessionErr error
	// hasSessionErr 非空时 HasSession 返回该错误（模拟基础设施错误，非 ErrNoTmuxServer）。
	hasSessionErr error
	watchCb       map[string]func(process.WatchEvent)
	envValues     map[string]map[string]string // sessionName -> env
	// cmdArgvValues 记录 NewSession 的 CmdArgv（§4 锚定测试断言 --session <id>）。
	cmdArgvValues map[string][]string
	// killOrder 记录 KillSession 调用顺序（D1 测试断言 kill 顺序 tui→shells→serve）。
	killOrder []string
	// newSessionNames 记录 NewSession 调用顺序（D2 测试断言 serve 死亡不新建 tui）。
	newSessionNames []string
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.newSessionErr != nil {
		return p.newSessionErr
	}
	p.newSessionNames = append(p.newSessionNames, spec.Name)
	p.sessions[spec.Name] = true
	p.envValues[spec.Name] = spec.Env
	p.cmdArgvValues[spec.Name] = spec.CmdArgv
	return nil
}

func (p *mockProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killOrder = append(p.killOrder, name)
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
}

func newMockOC(healthOK bool) *mockOC {
	return &mockOC{healthOK: healthOK, onReadyCh: make(chan struct{}, 1), deleteErrByID: map[string]error{}}
}

func (c *mockOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	if !c.healthOK {
		return opencode.HealthResponse{}, opencode.ErrServeNotReady
	}
	return opencode.HealthResponse{Healthy: true, Version: "1.18.9"}, nil
}

func (c *mockOC) Probe(ctx context.Context) (string, error) {
	if c.probeErr != nil {
		return "", c.probeErr
	}
	return "1.18.9", nil
}

func (c *mockOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.sessions, nil
}

func (c *mockOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	if c.getSessionErr != nil {
		return opencode.Session{}, c.getSessionErr
	}
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
	if c.createSessionResult.ID != "" {
		return c.createSessionResult, nil
	}
	return opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 100, Updated: 100}}, nil
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
	return New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: wt,
		OCFactory: wrap,
	})
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
