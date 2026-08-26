package task

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// 本文件为 R7 评审修复测试提供 mock 扩展，不修改 mock_test.go（另一 lane 独占）。
// 这些 wrapper 仅在 R7 测试使用，用于注入 mockProc 不直接支持的行为（如 kill 错误票据）。

// procKillWrapper 包装 mockProc，为 KillSession 注入基于 sessionName 的 killErr。
// 用于一次性 serve 清理 kill 错误测试（mockProc.killResults 仅控制 disposition/tickets，
// 不直接表达 KillSession 本身的 infra 错误）。
type procKillWrapper struct {
	ProcessBackend
	killErrFor map[string]error // sessionName -> kill error
}

func wrapProcKill(inner ProcessBackend, killErrFor map[string]error) *procKillWrapper {
	return &procKillWrapper{ProcessBackend: inner, killErrFor: killErrFor}
}

func (w *procKillWrapper) KillSession(name string) (process.KillResult, error) {
	if err, ok := w.killErrFor[name]; ok {
		return process.KillResult{}, err
	}
	return w.ProcessBackend.KillSession(name)
}

// sessionTraceStore 包装 TaskStore，按调用顺序记录 UpsertTaskSession/DeleteTaskSession
// 的 sessionID（含操作类型），用于 replay 顺序测试断言缓冲事件先于实时事件落库。
type sessionTraceStore struct {
	TaskStore
	mu    sync.Mutex
	trace []string // 顺序记录 "upsert:<sid>" / "delete:<sid>"
}

func wrapSessionTrace(inner TaskStore) *sessionTraceStore {
	return &sessionTraceStore{TaskStore: inner}
}

func (s *sessionTraceStore) UpsertTaskSession(ctx context.Context, r SessionRow) error {
	s.mu.Lock()
	s.trace = append(s.trace, "upsert:"+r.SessionID)
	s.mu.Unlock()
	return s.TaskStore.UpsertTaskSession(ctx, r)
}

func (s *sessionTraceStore) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	s.mu.Lock()
	s.trace = append(s.trace, "delete:"+sessionID)
	s.mu.Unlock()
	return s.TaskStore.DeleteTaskSession(ctx, taskID, sessionID)
}

// add-plain-dir-project D8：ClaimTaskSession 取代 session.created 的 upsert，trace 记为 "upsert:<sid>"
//（归属创建语义一致，保留 replay 顺序断言不变）。
func (s *sessionTraceStore) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.mu.Lock()
	s.trace = append(s.trace, "upsert:"+sessionID)
	s.mu.Unlock()
	return s.TaskStore.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}

// TouchOwnedTaskSession 取代 session.updated 的 upsert；trace 不记录（updated 不创建归属，
// replay 顺序测试关注 created/deleted 顺序）。
func (s *sessionTraceStore) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	return s.TaskStore.TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt)
}

// AlignTaskSessions 首次对齐不记 trace（测试关注实时事件 replay 顺序，非首次对齐）。
func (s *sessionTraceStore) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	return s.TaskStore.AlignTaskSessions(ctx, taskID, mode, listed, complete, notice)
}

func (s *sessionTraceStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.trace))
	copy(out, s.trace)
	return out
}

// newR7TestManager 构造 Manager，proc 可为 wrapper（如 procKillWrapper），store 可为 wrapper。
// 与 newTestManager 区别：允许直接传入 proc wrapper（不经 readyOC 包装 oc）。
func newR7TestManager(t *testing.T, store TaskStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) *Manager {
	t.Helper()
	return newTestManager(t, store, proc, wt, oc)
}

// newR7TestManagerWithDebt 构造带 CleanupDebtStore 的 Manager（orphan 持久化测试用）。
func newR7TestManagerWithDebt(t *testing.T, store TaskStore, debt CleanupDebtStore, proc ProcessBackend, wt WorktreeBackend, oc OCClient) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOC{inner: oc, onReady: opts.OnReady}
	}
	return New(Options{Cfg: cfg, Store: store, Proc: proc, Worktree: wt, OCFactory: wrap, DebtStore: debt})
}

// assertStatus 断言任务状态，失败 fatal。
func assertStatus(t *testing.T, store TaskStore, taskID, want string) {
	t.Helper()
	row, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask %s: %v", taskID, err)
	}
	if row.Status != want {
		t.Fatalf("task %s status=%s want %s", taskID, row.Status, want)
	}
}

// assertTaskExists 断言任务行仍存在（未被 CASCADE 删除）。
func assertTaskExists(t *testing.T, store TaskStore, taskID string) {
	t.Helper()
	_, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("task %s row must still exist (not deleted), got err: %v", taskID, err)
	}
}

// lastErrorContains 断言任务 last_error 含 substr。
func lastErrorContains(t *testing.T, store TaskStore, taskID, substr string) {
	t.Helper()
	row, _ := store.GetTask(context.Background(), taskID)
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, substr) {
		t.Fatalf("task %s last_error=%v must contain %q", taskID, row.LastError, substr)
	}
}

// makeEvent 构造 SSE 事件（session.created/updated/deleted）。
func makeEvent(typ, sid string, updated float64) opencode.Event {
	return opencode.Event{Type: typ, Properties: map[string]interface{}{
		"info": map[string]interface{}{
			"id": sid,
			"time": map[string]interface{}{"updated": updated},
		},
	}}
}

// makeEventWithDir 构造带 directory 的 SSE 事件（created/updated/deleted fail-closed 防线：
// directory MUST 明确等于本任务 worktree 才落库，B3a）。
func makeEventWithDir(typ, sid string, updated float64, directory string) opencode.Event {
	return opencode.Event{Type: typ, Properties: map[string]interface{}{
		"info": map[string]interface{}{
			"id":        sid,
			"directory": directory,
			"time":      map[string]interface{}{"updated": updated},
		},
	}}
}

// procListErrWrapper 包装 ProcessBackend，使 ListSessions 返回指定错误（infra 错误模拟）。
type procListErrWrapper struct {
	ProcessBackend
	listErr error
}

func wrapProcListErr(inner ProcessBackend, err error) *procListErrWrapper {
	return &procListErrWrapper{ProcessBackend: inner, listErr: err}
}

func (w *procListErrWrapper) ListSessions() ([]string, error) {
	return nil, w.listErr
}

// procHasSessionByNameWrapper 包装 ProcessBackend，按 sessionName 返回 HasSession 错误。
// 用于 killTaskSessions 逐会话 HasSession 错误测试（mockProc.hasSessionErr 是全局的）。
type procHasSessionByNameWrapper struct {
	ProcessBackend
	errFor map[string]error
	fallback error // 对不在 errFor 的会话返回此错误（nil 表示透传 inner）
}

func wrapProcHasSessionByName(inner ProcessBackend, errFor map[string]error, fallback error) *procHasSessionByNameWrapper {
	return &procHasSessionByNameWrapper{ProcessBackend: inner, errFor: errFor, fallback: fallback}
}

func (w *procHasSessionByNameWrapper) HasSession(name string) (bool, error) {
	if err, ok := w.errFor[name]; ok {
		return false, err
	}
	return w.ProcessBackend.HasSession(name)
}

// reapFailingWrapper 包装 ProcessBackend，使 RetryReap 永远返回 remaining tickets（未收割）。
type reapFailingWrapper struct {
	ProcessBackend
}

func wrapReapFailing(inner ProcessBackend) *reapFailingWrapper {
	return &reapFailingWrapper{ProcessBackend: inner}
}

func (w *reapFailingWrapper) RetryReap(tickets []string) ([]string, error) {
	// 永远返回全部 tickets（未收割），模拟 reap 持续失败。
	return tickets, nil
}

// newSessionCapture 包装 ProcessBackend，捕获 NewSession 的 SessionSpec（含 CmdArgv），
// 供 ReopenAttach 端口断言（mockProc 仅记录 sessionName，不记 argv）。
type newSessionCapture struct {
	ProcessBackend
	mu    sync.Mutex
	specs []process.SessionSpec
}

func wrapNewSessionCapture(inner ProcessBackend) *newSessionCapture {
	return &newSessionCapture{ProcessBackend: inner}
}

func (w *newSessionCapture) NewSession(spec process.SessionSpec) error {
	w.mu.Lock()
	w.specs = append(w.specs, spec)
	w.mu.Unlock()
	return w.ProcessBackend.NewSession(spec)
}

func (w *newSessionCapture) specsFor(name string) []process.SessionSpec {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []process.SessionSpec
	for _, s := range w.specs {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// sessionStoreErrWrapper 包装 TaskStore，使 UpsertTaskSession/DeleteTaskSession/ListTaskSessions
// 返回注入错误（用于 handleSSEEvent/startTUI 错误传播测试）。
type sessionStoreErrWrapper struct {
	TaskStore
	upsertErr   error
	deleteErr   error
	listErr     error
	// listTopErr 注入 ListTopLevelTaskSessions 错误（B3：resolveAnchorSession 走顶层会话查询）。
	listTopErr error
}

func wrapSessionStoreErr(inner TaskStore, upsertErr, deleteErr, listErr error) *sessionStoreErrWrapper {
	return &sessionStoreErrWrapper{TaskStore: inner, upsertErr: upsertErr, deleteErr: deleteErr, listErr: listErr}
}

func (w *sessionStoreErrWrapper) UpsertTaskSession(ctx context.Context, r SessionRow) error {
	if w.upsertErr != nil {
		return w.upsertErr
	}
	return w.TaskStore.UpsertTaskSession(ctx, r)
}
func (w *sessionStoreErrWrapper) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	if w.deleteErr != nil {
		return 0, w.deleteErr
	}
	return w.TaskStore.DeleteTaskSession(ctx, taskID, sessionID)
}
func (w *sessionStoreErrWrapper) ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	if w.listErr != nil {
		return nil, w.listErr
	}
	return w.TaskStore.ListTaskSessions(ctx, taskID)
}
func (w *sessionStoreErrWrapper) ListTopLevelTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error) {
	if w.listTopErr != nil {
		return nil, w.listTopErr
	}
	return w.TaskStore.ListTopLevelTaskSessions(ctx, taskID)
}

// add-plain-dir-project D8：claim/touch 归属写入口经 wrapper，复用 upsertErr 触发
//（实时 session.created 走 ClaimTaskSession、session.updated 走 TouchOwnedTaskSession，
// 失败语义同既有 upsert 失败：归属写入错误传播，收敛运行时）。AlignTaskSessions 不在此拦截
//（对齐错误由专用测试覆盖，且 Activate 内首次对齐失败会导致 Activate 失败而非 SSE 实时收敛路径）。
func (w *sessionStoreErrWrapper) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	if w.upsertErr != nil {
		return application.ClaimResult{}, w.upsertErr
	}
	return w.TaskStore.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}
func (w *sessionStoreErrWrapper) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	if w.upsertErr != nil {
		return application.MutationResult{}, w.upsertErr
	}
	return w.TaskStore.TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt)
}
func (w *sessionStoreErrWrapper) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	return w.TaskStore.AlignTaskSessions(ctx, taskID, mode, listed, complete, notice)
}

// memCleanupDebtStore 内存实现 CleanupDebtStore（用于 orphan ticket 持久化测试）。
type memCleanupDebtStore struct {
	mu   sync.Mutex
	rows map[string]memDebtRow
}
type memDebtRow struct {
	tickets   string
	createdAt int64
}

func newMemCleanupDebtStore() *memCleanupDebtStore {
	return &memCleanupDebtStore{rows: map[string]memDebtRow{}}
}

func (s *memCleanupDebtStore) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[sessionName] = memDebtRow{tickets: ticketsJSON, createdAt: createdAt}
	return nil
}
func (s *memCleanupDebtStore) DeleteCleanupDebt(ctx context.Context, sessionName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, sessionName)
	return nil
}
func (s *memCleanupDebtStore) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CleanupDebtRow, 0, len(s.rows))
	for name, r := range s.rows {
		out = append(out, CleanupDebtRow{SessionName: name, Tickets: r.tickets, CreatedAt: r.createdAt})
	}
	return out, nil
}

func (s *memCleanupDebtStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// failDebtStore 包装 CleanupDebtStore，注入指定方法错误（用于 restore/persist 失败测试）。
type failDebtStore struct {
	CleanupDebtStore
	listErr   error
	upsertErr error
}

func wrapFailDebt(inner CleanupDebtStore, listErr, upsertErr error) *failDebtStore {
	return &failDebtStore{CleanupDebtStore: inner, listErr: listErr, upsertErr: upsertErr}
}

func (s *failDebtStore) ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.CleanupDebtStore.ListCleanupDebts(ctx)
}
func (s *failDebtStore) UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	return s.CleanupDebtStore.UpsertCleanupDebt(ctx, sessionName, ticketsJSON, createdAt)
}

// statusErrStore 包装 TaskStore，注入 UpdateTaskStatusConditional 错误
//（用于 convergeToSuspended 状态提交失败测试，非孤立 mock 方法注入——通过 wrapper 覆盖真实 store 方法）。
type statusErrStore struct {
	TaskStore
	condErr error
}

func wrapStatusErr(inner TaskStore, condErr error) *statusErrStore {
	return &statusErrStore{TaskStore: inner, condErr: condErr}
}

func (w *statusErrStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if w.condErr != nil && fromStatus == StatusActive && toStatus == StatusSuspended {
		return application.TransitionResult{}, w.condErr
	}
	return w.TaskStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}