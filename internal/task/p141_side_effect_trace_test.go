package task

// 本文件为 OpenSpec change sse-active-sessions P1.4.1 的关键流程副作用 trace 回归测试。
//
// 目标：冻结 Create / Activate / Suspend / Delete 四流程的当前副作用调用顺序与
// 「不得发生」项（guard 拒绝时零 store 写、零外部副作用），作为后续 strangler 迁移步骤
// 的回归 oracle。测试在当前行为下 MUST 通过——它们不改任何生产行为。
//
// 实现说明：
//   - 复用既有 mock 基建（mockStore/mockProc/mockWorktree/mockOC + R7 wrapper 模式）。
//   - traceStore 包装 TaskStore，按调用顺序记录 store 写方法（含参数要点）。
//   - traceProc 包装 ProcessBackend，记录 NewSession/KillSession/HasSession 调用顺序。
//   - traceWorktree 包装 WorktreeBackend，记录 Add/Remove/PreflightDelete/DirtyFiles 调用。
//   - traceOC 包装 OCClient，记录 Health/ListSessions/GetSession/CreateSession/DeleteSession/
//     SubscribeEvents/Probe 调用顺序。
//   - 每个流程断言：副作用调用顺序快照（[]traceEvent{op, key...}）以及 Times(0) 级
//     「不得发生」断言（guard 拒绝路径零 store 写、零外部副作用）。
//
// 这些测试与现有 mock_test.go / mock_r7_test.go 的 wrapper 风格一致，不引入新依赖。

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- trace event 记录 ---

// traceOp 标识一次副作用调用的来源与操作。
type traceOp struct {
	src string // "store" | "proc" | "wt" | "oc"
	op  string // 方法名（如 "UpdateTaskStatus"）
	// key 为参数要点摘要（如 status 值、sessionID、taskID），用于顺序断言。
	key string
}

// tracer 收集所有副作用调用顺序。
type tracer struct {
	mu     sync.Mutex
	events []traceOp
}

func (t *tracer) record(src, op, key string) {
	t.mu.Lock()
	t.events = append(t.events, traceOp{src: src, op: op, key: key})
	t.mu.Unlock()
}

// snapshot 返回调用顺序拷贝。
func (t *tracer) snapshot() []traceOp {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]traceOp, len(t.events))
	copy(out, t.events)
	return out
}

// srcCount 返回指定 src 的调用次数（用于 Times(0) 级「不得发生」断言）。
func (t *tracer) srcCount(src string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, e := range t.events {
		if e.src == src {
			n++
		}
	}
	return n
}

// countOp 返回指定 src+op 的调用次数。
func (t *tracer) countOp(src, op string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, e := range t.events {
		if e.src == src && e.op == op {
			n++
		}
	}
	return n
}

// hasOpKey 返回是否存在 src+op 且 key 含 substr 的调用。
func (t *tracer) hasOpKey(src, op, substr string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.events {
		if e.src == src && e.op == op && strings.Contains(e.key, substr) {
			return true
		}
	}
	return false
}

// --- traceStore：包装 TaskStore 记录 store 写 ---

type traceStore struct {
	TaskStore
	tr *tracer
}

func wrapTraceStore(inner TaskStore, tr *tracer) *traceStore {
	return &traceStore{TaskStore: inner, tr: tr}
}

// 写方法（改变任务/session/notice 状态或结构化结果）记录 trace；只读方法（GetProject/GetTask/
// List*/env 列表）不记录——它们是决策输入而非副作用。AlignTaskSessions 为事务写，记录。

func (s *traceStore) CreateTask(ctx context.Context, t TaskRow) error {
	s.tr.record("store", "CreateTask", fmt.Sprintf("id=%s status=%s", t.ID, t.Status))
	return s.TaskStore.CreateTask(ctx, t)
}

func (s *traceStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	s.tr.record("store", "UpdateTaskStatus", fmt.Sprintf("id=%s status=%s", id, status))
	return s.TaskStore.UpdateTaskStatus(ctx, id, status, lastError)
}

func (s *traceStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	s.tr.record("store", "UpdateTaskStatusConditional", fmt.Sprintf("id=%s %s->%s", id, fromStatus, toStatus))
	return s.TaskStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func (s *traceStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	s.tr.record("store", "UpdateTaskEnvSnapshot", fmt.Sprintf("id=%s valid=%v", id, envSnapshot.Valid))
	return s.TaskStore.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

func (s *traceStore) UpdateTaskLastPort(ctx context.Context, id string, port int) (application.MutationResult, error) {
	s.tr.record("store", "UpdateTaskLastPort", fmt.Sprintf("id=%s port=%d", id, port))
	return s.TaskStore.UpdateTaskLastPort(ctx, id, port)
}

func (s *traceStore) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) (application.MutationResult, error) {
	s.tr.record("store", "UpdateTaskNotice", fmt.Sprintf("id=%s valid=%v", id, notice.Valid))
	return s.TaskStore.UpdateTaskNotice(ctx, id, notice)
}

func (s *traceStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	s.tr.record("store", "UpdateTaskNoticeCAS", fmt.Sprintf("id=%s", id))
	return s.TaskStore.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

func (s *traceStore) SetTaskDeleteMode(ctx context.Context, id, mode string) (application.MutationResult, error) {
	s.tr.record("store", "SetTaskDeleteMode", fmt.Sprintf("id=%s mode=%s", id, mode))
	return s.TaskStore.SetTaskDeleteMode(ctx, id, mode)
}

func (s *traceStore) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (application.TransitionResult, error) {
	s.tr.record("store", "BeginDeleteIntent", fmt.Sprintf("id=%s mode=%s", id, mode))
	return s.TaskStore.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

func (s *traceStore) ArchiveTask(ctx context.Context, id string) (application.TransitionResult, error) {
	s.tr.record("store", "ArchiveTask", fmt.Sprintf("id=%s", id))
	return s.TaskStore.ArchiveTask(ctx, id)
}

func (s *traceStore) RestoreTask(ctx context.Context, id string) (application.TransitionResult, error) {
	s.tr.record("store", "RestoreTask", fmt.Sprintf("id=%s", id))
	return s.TaskStore.RestoreTask(ctx, id)
}

func (s *traceStore) DeleteTask(ctx context.Context, id string) (application.DeleteResult, error) {
	s.tr.record("store", "DeleteTask", fmt.Sprintf("id=%s", id))
	return s.TaskStore.DeleteTask(ctx, id)
}

func (s *traceStore) CommitCreated(ctx context.Context, taskID, expectedStatus, initStatus string) (application.TransitionResult, error) {
	s.tr.record("store", "CommitCreated", fmt.Sprintf("id=%s exp=%s init=%s", taskID, expectedStatus, initStatus))
	return s.TaskStore.CommitCreated(ctx, taskID, expectedStatus, initStatus)
}

func (s *traceStore) ClaimInitRun(ctx context.Context, taskID string) (application.MutationResult, error) {
	s.tr.record("store", "ClaimInitRun", fmt.Sprintf("id=%s", taskID))
	return s.TaskStore.ClaimInitRun(ctx, taskID)
}

func (s *traceStore) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (application.MutationResult, error) {
	s.tr.record("store", "FinishInitRun", fmt.Sprintf("id=%s status=%s", taskID, status))
	return s.TaskStore.FinishInitRun(ctx, taskID, status, initError)
}

func (s *traceStore) ClaimTaskSession(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.tr.record("store", "ClaimTaskSession", fmt.Sprintf("id=%s sid=%s", taskID, sessionID))
	return s.TaskStore.ClaimTaskSession(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}

func (s *traceStore) ClaimTaskSessionAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.tr.record("store", "ClaimTaskSessionAndSetAnchor", fmt.Sprintf("id=%s sid=%s", taskID, sessionID))
	return s.TaskStore.ClaimTaskSessionAndSetAnchor(ctx, taskID, sessionID, createdAt, firstSeen, lastSeen, parentID)
}

func (s *traceStore) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	s.tr.record("store", "ClearTaskAnchorConditional", fmt.Sprintf("id=%s old=%s", taskID, oldAnchor))
	return s.TaskStore.ClearTaskAnchorConditional(ctx, taskID, oldAnchor)
}

func (s *traceStore) TouchOwnedTaskSession(ctx context.Context, taskID, sessionID string, lastSeenAt int64) (application.MutationResult, error) {
	s.tr.record("store", "TouchOwnedTaskSession", fmt.Sprintf("id=%s sid=%s", taskID, sessionID))
	return s.TaskStore.TouchOwnedTaskSession(ctx, taskID, sessionID, lastSeenAt)
}

func (s *traceStore) DeleteTaskSession(ctx context.Context, taskID, sessionID string) (int, error) {
	s.tr.record("store", "DeleteTaskSession", fmt.Sprintf("id=%s sid=%s", taskID, sessionID))
	return s.TaskStore.DeleteTaskSession(ctx, taskID, sessionID)
}

func (s *traceStore) AlignTaskSessions(ctx context.Context, taskID string, mode AlignMode, listed []SessionObservation, complete bool, notice application.NoticeMutation) (application.AlignResult, error) {
	s.tr.record("store", "AlignTaskSessions", fmt.Sprintf("id=%s mode=%d complete=%v n=%d", taskID, mode, complete, len(listed)))
	return s.TaskStore.AlignTaskSessions(ctx, taskID, mode, listed, complete, notice)
}

// --- traceProc：包装 ProcessBackend ---

type traceProc struct {
	ProcessBackend
	tr *tracer
}

func wrapTraceProc(inner ProcessBackend, tr *tracer) *traceProc {
	return &traceProc{ProcessBackend: inner, tr: tr}
}

func (p *traceProc) NewSession(spec process.SessionSpec) error {
	p.tr.record("proc", "NewSession", spec.Name)
	return p.ProcessBackend.NewSession(spec)
}

func (p *traceProc) KillSession(name string) (process.KillResult, error) {
	p.tr.record("proc", "KillSession", name)
	return p.ProcessBackend.KillSession(name)
}

func (p *traceProc) HasSession(name string) (bool, error) {
	// HasSession 为查询，但 Suspend 决策树用其做分支判定，记录以断言决策点顺序。
	p.tr.record("proc", "HasSession", name)
	return p.ProcessBackend.HasSession(name)
}

func (p *traceProc) ListSessions() ([]string, error) {
	p.tr.record("proc", "ListSessions", "")
	return p.ProcessBackend.ListSessions()
}

func (p *traceProc) ShowSessionEnv(name, key string) (string, error) {
	p.tr.record("proc", "ShowSessionEnv", fmt.Sprintf("%s/%s", name, key))
	return p.ProcessBackend.ShowSessionEnv(name, key)
}

func (p *traceProc) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	p.tr.record("proc", "ShowSessionEnvContext", fmt.Sprintf("%s/%s", name, key))
	return p.ProcessBackend.ShowSessionEnvContext(ctx, name, key)
}

func (p *traceProc) WatchExit(name string, callback func(process.WatchEvent)) (func(), <-chan struct{}) {
	p.tr.record("proc", "WatchExit", name)
	return p.ProcessBackend.WatchExit(name, callback)
}

// --- traceWorktree：包装 WorktreeBackend ---

type traceWorktree struct {
	WorktreeBackend
	tr *tracer
}

func wrapTraceWorktree(inner WorktreeBackend, tr *tracer) *traceWorktree {
	return &traceWorktree{WorktreeBackend: inner, tr: tr}
}

func (w *traceWorktree) Add(ctx context.Context, repoPath, dest, branch, baseRef string) error {
	w.tr.record("wt", "Add", fmt.Sprintf("branch=%s dest=%s", branch, dest))
	return w.WorktreeBackend.Add(ctx, repoPath, dest, branch, baseRef)
}

func (w *traceWorktree) Remove(ctx context.Context, wtPath string, opts worktreeRemoveOpts) error {
	w.tr.record("wt", "Remove", fmt.Sprintf("wt=%s branch=%s", wtPath, opts.Branch))
	return w.WorktreeBackend.Remove(ctx, wtPath, opts)
}

func (w *traceWorktree) BranchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	w.tr.record("wt", "BranchExists", branch)
	return w.WorktreeBackend.BranchExists(ctx, repoPath, branch)
}

func (w *traceWorktree) ValidateBranchName(ctx context.Context, repoPath, branch string) error {
	w.tr.record("wt", "ValidateBranchName", branch)
	return w.WorktreeBackend.ValidateBranchName(ctx, repoPath, branch)
}

func (w *traceWorktree) ResolveBaseRef(ctx context.Context, repoPath, shortName string) (string, error) {
	w.tr.record("wt", "ResolveBaseRef", shortName)
	return w.WorktreeBackend.ResolveBaseRef(ctx, repoPath, shortName)
}

func (w *traceWorktree) VerifyWorktreeProduct(ctx context.Context, repoPath, wtPath, branch string) error {
	w.tr.record("wt", "VerifyWorktreeProduct", fmt.Sprintf("branch=%s", branch))
	return w.WorktreeBackend.VerifyWorktreeProduct(ctx, repoPath, wtPath, branch)
}

func (w *traceWorktree) PreflightDelete(ctx context.Context, wtPath string, opts PreflightDeleteOpts) error {
	w.tr.record("wt", "PreflightDelete", fmt.Sprintf("branch=%s confirm=%v", opts.Branch, opts.ConfirmDirty))
	return w.WorktreeBackend.PreflightDelete(ctx, wtPath, opts)
}

func (w *traceWorktree) DirtyFiles(ctx context.Context, wtPath string) (map[string]struct{}, error) {
	w.tr.record("wt", "DirtyFiles", wtPath)
	return w.WorktreeBackend.DirtyFiles(ctx, wtPath)
}

// --- traceOC：包装 OCClient ---

type traceOC struct {
	OCClient
	tr *tracer
}

func wrapTraceOC(inner OCClient, tr *tracer) *traceOC {
	return &traceOC{OCClient: inner, tr: tr}
}

func (c *traceOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	c.tr.record("oc", "Health", "")
	return c.OCClient.Health(ctx)
}

func (c *traceOC) Probe(ctx context.Context) (string, error) {
	c.tr.record("oc", "Probe", "")
	return c.OCClient.Probe(ctx)
}

func (c *traceOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	c.tr.record("oc", "ListSessions", "")
	return c.OCClient.ListSessions(ctx, dir, limit)
}

func (c *traceOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	c.tr.record("oc", "GetSession", id)
	return c.OCClient.GetSession(ctx, dir, id)
}

func (c *traceOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	c.tr.record("oc", "CreateSession", title)
	return c.OCClient.CreateSession(ctx, dir, title)
}

func (c *traceOC) DeleteSession(ctx context.Context, dir, id string) error {
	c.tr.record("oc", "DeleteSession", id)
	return c.OCClient.DeleteSession(ctx, dir, id)
}

func (c *traceOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	c.tr.record("oc", "SessionStatus", "")
	return c.OCClient.SessionStatus(ctx, dir)
}

func (c *traceOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	c.tr.record("oc", "SubscribeEvents", "")
	return c.OCClient.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}

// --- trace 测试 Manager 构造 ---

// newTraceTestManager 构造带 tracer 的 Manager：store/proc/wt/oc 全部经 trace wrapper，
// readyOC 包装保留（SSE onReady 触发）；OCFactory 返回 traceOC 包装 readyOC。
func newTraceTestManager(t *testing.T, store *mockStore, proc *mockProc, wt *mockWorktree, oc *mockOC, tr *tracer) *Manager {
	t.Helper()
	tStore := wrapTraceStore(store, tr)
	tProc := wrapTraceProc(proc, tr)
	tWt := wrapTraceWorktree(wt, tr)
	wrap := func(port int, password string, opts opencode.Options) OCClient {
		inner := &readyOC{inner: oc, onReady: opts.OnReady}
		return wrapTraceOC(inner, tr)
	}
	cfg := testCfg(t)
	return New(Options{
		Cfg:       cfg,
		Store:     tStore,
		Proc:      tProc,
		Worktree:  tWt,
		OCFactory: wrap,
	})
}

// testCfg 构造测试用 config（复用 newTestManager 的默认值）。
func testCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
}

// --- 辅助断言 ---

// mutatingProcOps / mutatingWtOps / mutatingOcOps 定义各 src 下的「修改类」副作用方法
// （与只读检查区分：只读检查如 wt.PreflightDelete/DirtyFiles/BranchExists/ValidateBranchName、
// proc.HasSession/ListSessions/ShowSessionEnv、oc.Health/Probe/ListSessions/GetSession/
// SessionStatus 是 guard 决策输入，不是不可逆副作用）。
// guard 拒绝路径断言「零外部副作用」仅指修改类方法 Times(0)，只读检查允许作为 guard 的一部分发生。
var (
	mutatingStoreOps = map[string]bool{
		"CreateTask": true, "UpdateTaskStatus": true, "UpdateTaskStatusConditional": true,
		"UpdateTaskEnvSnapshot": true, "UpdateTaskLastPort": true, "UpdateTaskNotice": true,
		"UpdateTaskNoticeCAS": true, "SetTaskDeleteMode": true, "BeginDeleteIntent": true,
		"ArchiveTask": true, "RestoreTask": true, "DeleteTask": true, "CommitCreated": true,
		"ClaimInitRun": true, "ClaimInitRerun": true, "FinishInitRun": true,
		"ClaimTaskSession": true, "TouchOwnedTaskSession": true, "DeleteTaskSession": true,
		"AlignTaskSessions": true,
	}
	mutatingProcOps = map[string]bool{
		"NewSession": true, "KillSession": true,
	}
	mutatingWtOps = map[string]bool{
		"Add": true, "Remove": true,
	}
	mutatingOcOps = map[string]bool{
		"CreateSession": true, "DeleteSession": true, "SubscribeEvents": true,
	}
)

// srcMutatingCount 返回指定 src 下修改类副作用调用次数。
func (t *tracer) srcMutatingCount(src string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var set map[string]bool
	switch src {
	case "store":
		set = mutatingStoreOps
	case "proc":
		set = mutatingProcOps
	case "wt":
		set = mutatingWtOps
	case "oc":
		set = mutatingOcOps
	default:
		return 0
	}
	n := 0
	for _, e := range t.events {
		if e.src == src && set[e.op] {
			n++
		}
	}
	return n
}

// assertNoSideEffects 断言 guard 拒绝路径零修改类副作用（store 写、proc 修改、wt 修改、oc 修改）。
// 只读检查（wt.PreflightDelete/DirtyFiles、proc.HasSession 等 guard 决策输入）不计入副作用。
func assertNoSideEffects(t *testing.T, tr *tracer, flow string) {
	t.Helper()
	if n := tr.srcMutatingCount("store"); n != 0 {
		t.Errorf("%s: guard 拒绝路径 MUST 零 store 写，got %d: %v", flow, n, filterEvents(tr, "store"))
	}
	if n := tr.srcMutatingCount("proc"); n != 0 {
		t.Errorf("%s: guard 拒绝路径 MUST 零 proc 修改副作用，got %d: %v", flow, n, filterEvents(tr, "proc"))
	}
	if n := tr.srcMutatingCount("wt"); n != 0 {
		t.Errorf("%s: guard 拒绝路径 MUST 零 wt 修改副作用，got %d: %v", flow, n, filterEvents(tr, "wt"))
	}
	if n := tr.srcMutatingCount("oc"); n != 0 {
		t.Errorf("%s: guard 拒绝路径 MUST 零 oc 修改副作用，got %d: %v", flow, n, filterEvents(tr, "oc"))
	}
}

// filterEvents 返回指定 src 的事件快照（用于错误信息）。
func filterEvents(tr *tracer, src string) []traceOp {
	all := tr.snapshot()
	var out []traceOp
	for _, e := range all {
		if e.src == src {
			out = append(out, e)
		}
	}
	return out
}

// assertOrdered 断言 tr 中存在指定的 src+op 序列（按出现顺序，允许中间穿插其他调用）。
// 用于断言关键副作用的相对顺序（如 CreateTask 在 wt.Add 之前，CommitCreated 在其后）。
func assertOrdered(t *testing.T, tr *tracer, want []traceOp, flow string) {
	t.Helper()
	got := tr.snapshot()
	i := 0
	for _, e := range got {
		if i < len(want) && e.src == want[i].src && e.op == want[i].op && strings.Contains(e.key, want[i].key) {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("%s: 副作用顺序断言失败：期望按序出现 %v，仅匹配前 %d 个\n完整 trace:\n%s",
			flow, want, i, formatTrace(got))
	}
}

// formatTrace 格式化 trace 供错误信息输出。
func formatTrace(events []traceOp) string {
	var b strings.Builder
	for i, e := range events {
		fmt.Fprintf(&b, "  [%d] %s.%s %s\n", i, e.src, e.op, e.key)
	}
	return b.String()
}

// assertOpCount 断言指定 src+op 调用次数 == want。
func assertOpCount(t *testing.T, tr *tracer, src, op string, want int, flow string) {
	t.Helper()
	if n := tr.countOp(src, op); n != want {
		t.Errorf("%s: %s.%s 调用次数 = %d, want %d\ntrace:\n%s", flow, src, op, n, want, formatTrace(tr.snapshot()))
	}
}

// assertOpNever 断言指定 src+op 从未被调用（Times(0)）。
func assertOpNever(t *testing.T, tr *tracer, src, op string, flow string) {
	t.Helper()
	if n := tr.countOp(src, op); n != 0 {
		t.Errorf("%s: %s.%s MUST 零调用（Times(0)），got %d\ntrace:\n%s", flow, src, op, n, formatTrace(tr.snapshot()))
	}
}

// assertSrcNeverMutating 断言指定 src 的修改类副作用从未被调用（guard 拒绝时整类修改副作用不得发生）。
// 只读检查（guard 决策输入）不计入。
func assertSrcNeverMutating(t *testing.T, tr *tracer, src string, flow string) {
	t.Helper()
	if n := tr.srcMutatingCount(src); n != 0 {
		t.Errorf("%s: %s 类修改副作用 MUST 零调用（Times(0)），got %d\ntrace:\n%s", flow, src, n, formatTrace(tr.snapshot()))
	}
}

// assertSrcNeverAll 断言指定 src 从未被调用（含只读检查）——用于 dir 路径断言零 wt 调用、
// guard 在任何 wt 检查前就拒绝的路径。
func assertSrcNeverAll(t *testing.T, tr *tracer, src string, flow string) {
	t.Helper()
	if n := tr.srcCount(src); n != 0 {
		t.Errorf("%s: %s 类全部调用 MUST 零（Times(0)），got %d\ntrace:\n%s", flow, src, n, formatTrace(tr.snapshot()))
	}
}

// ============================================================
// Create 流程 trace 回归
// ============================================================

// TestP141_Create_Repo_Success_Trace 冻结 repo Create 成功路径的副作用顺序：
// 前置检查（wt.ValidateBranchName/BranchExists/ResolveBaseRef，无副作用落库前完成）
// → store.CreateTask(creating) → wt.Add → runInherit（GetLifecycleConfig）
// → store.CommitCreated(creating→suspended) → 锁外调度（triggerActivate，异步）。
func TestP141_Create_Repo_Success_Trace(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	// repo Create 无 init 脚本 → triggerActivate 异步；用 lifecycle ctx 让其推进到 active。
	m.SetLifecycleCtx(context.Background())

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("status=%s want suspended|activating|active (auto-activate race)", row.Status)
	}

	// 关键副作用顺序：CreateTask(creating) → wt.Add → CommitCreated(creating→suspended)。
	// 前置 wt 检查在 CreateTask 之前；CommitCreated 在 wt.Add 之后（副作用先于提交点）。
	assertOrdered(t, tr, []traceOp{
		{src: "wt", op: "ValidateBranchName", key: "ocdeck/"},
		{src: "wt", op: "BranchExists", key: "ocdeck/"},
		{src: "store", op: "CreateTask", key: "status=creating"},
		{src: "wt", op: "Add", key: "branch=ocdeck/"},
		{src: "store", op: "CommitCreated", key: "init=none"},
	}, "Create.repo.success")

	// CreateTask 恰好一次；CommitCreated 恰好一次；无 BeginDeleteIntent/ArchiveTask/RestoreTask/DeleteTask。
	assertOpCount(t, tr, "store", "CreateTask", 1, "Create.repo.success")
	assertOpCount(t, tr, "store", "CommitCreated", 1, "Create.repo.success")
	assertOpNever(t, tr, "store", "BeginDeleteIntent", "Create.repo.success")
	assertOpNever(t, tr, "store", "DeleteTask", "Create.repo.success")
	assertOpNever(t, tr, "store", "ArchiveTask", "Create.repo.success")
	assertOpNever(t, tr, "store", "RestoreTask", "Create.repo.success")
	// wt.Add 恰好一次；无 wt.Remove。
	assertOpCount(t, tr, "wt", "Add", 1, "Create.repo.success")
	assertOpNever(t, tr, "wt", "Remove", "Create.repo.success")
}

// TestP141_Create_Repo_GuardReject_Trace 冻结 Create guard 拒绝路径零副作用：
// 分支冲突（wt.BranchExists=true）→ 返回 conflict，不得 CreateTask、不得 wt.Add、不得 CommitCreated。
func TestP141_Create_Repo_GuardReject_Trace(t *testing.T) {
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	// 预置分支已存在 → Create 前置检查拒绝。
	wt.branches["ocdeck/my-task"] = true
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)

	_, err := m.Create(context.Background(), "p1", "My Task", "")
	if err == nil {
		t.Fatal("expected conflict on branch exists")
	}
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("code = %s, want conflict", OpErrorCode(err))
	}

	// guard 拒绝：零 store 写、零 wt.Add、零 proc、零 oc。
	assertNoSideEffects(t, tr, "Create.repo.guardReject")
	// 显式断言关键写方法 Times(0)。
	assertOpNever(t, tr, "store", "CreateTask", "Create.repo.guardReject")
	assertOpNever(t, tr, "store", "CommitCreated", "Create.repo.guardReject")
	assertOpNever(t, tr, "wt", "Add", "Create.repo.guardReject")
}

// TestP141_Create_Dir_Success_Trace 冻结 dir Create 成功路径的副作用顺序：
// 目录预检（os.Stat，无 wt 调用）→ store.CreateTask(creating) → GetLifecycleConfig
// → store.CommitCreated(creating→suspended) → 锁外调度。
// dir 路径 MUST 零 wt 调用（无 worktree/分支）。
func TestP141_Create_Dir_Success_Trace(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: dir, DefaultBranch: "", Kind: ProjectKindDir})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	m.SetLifecycleCtx(context.Background())

	row, err := m.Create(context.Background(), "p1", "My Task", "")
	if err != nil {
		t.Fatalf("Create dir: %v", err)
	}
	if row.Status != StatusSuspended && row.Status != StatusActivating && row.Status != StatusActive {
		t.Fatalf("status=%s want suspended|activating|active", row.Status)
	}
	if row.Branch != "" {
		t.Errorf("dir task branch MUST empty, got %q", row.Branch)
	}
	// macOS /var 是 /private/var 符号链接，EvalSymlinks 会解析为物理路径；
	// 比较规范化后的路径而非字面相等。
	wantPath, _ := filepath.EvalSymlinks(dir)
	gotPath, _ := filepath.EvalSymlinks(row.WorktreePath)
	if gotPath != wantPath {
		t.Errorf("dir task worktree_path = %q (norm %q), want %q (norm %q)", row.WorktreePath, gotPath, dir, wantPath)
	}

	// 顺序：CreateTask(creating) → CommitCreated(creating→suspended)。无 wt 调用。
	assertOrdered(t, tr, []traceOp{
		{src: "store", op: "CreateTask", key: "status=creating"},
		{src: "store", op: "CommitCreated", key: "init=none"},
	}, "Create.dir.success")

	assertOpCount(t, tr, "store", "CreateTask", 1, "Create.dir.success")
	assertOpCount(t, tr, "store", "CommitCreated", 1, "Create.dir.success")
	// dir 路径零 wt 调用（无 Add/ValidateBranchName/BranchExists/Remove）。
	assertSrcNeverAll(t, tr, "wt", "Create.dir.success")
}

// TestP141_Create_Dir_GuardReject_Trace 冻结 dir Create guard 拒绝：
// base_ref 非空 → invalid_input，零副作用（不得 CreateTask、零 wt、零 proc、零 oc）。
func TestP141_Create_Dir_GuardReject_Trace(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: dir, DefaultBranch: "", Kind: ProjectKindDir})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)

	_, err := m.Create(context.Background(), "p1", "My Task", "feat/x")
	if err == nil {
		t.Fatal("expected invalid_input on base_ref for dir project")
	}
	if OpErrorCode(err) != codeInvalidInput {
		t.Fatalf("code = %s, want invalid_input", OpErrorCode(err))
	}

	// guard 拒绝：零 store 写、零 wt、零 proc、零 oc。
	assertNoSideEffects(t, tr, "Create.dir.guardReject")
	assertOpNever(t, tr, "store", "CreateTask", "Create.dir.guardReject")
	assertSrcNeverAll(t, tr, "wt", "Create.dir.guardReject")
}

// ============================================================
// Activate 流程 trace 回归
// ============================================================

// TestP141_Activate_Success_Trace 冻结 Activate 成功路径副作用顺序：
// 前置 guard（GetTask status=suspended、GetProject kind、residual session 检查、retryable notice 检查）
// → CAS suspended→activating → 分配端口+合并 env（store.UpdateTaskEnvSnapshot）
// → proc.NewSession(serve) → oc.Health → oc.Probe → store.UpdateTaskLastPort
// → oc.SubscribeEvents（onReady→alignSessions: oc.ListSessions + store.AlignTaskSessions）
// → oc.GetSession/CreateSession（锚定）→ proc.NewSession(tui) → store.UpdateTaskStatus(active)。
func TestP141_Activate_Success_Trace(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// serve 会话预置存活，让 Activate 内 HasSession(serveName) 检查通过。
	// mockProc.HasSession 仅在 map 中存在时返回 true；Activate 在 NewSession(serve) 后才检查。
	wt := newMockWorktree()
	oc := newMockOC(true)
	// 预置一个 anchor session 候选（resolveAnchorSession 走 GetSession 预检路径）。
	oc.sessions = []opencode.Session{{ID: "sess-anchor", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	store.mutTask("t1", func(tr *TaskRow) {
		tr.AnchorSessionID = sql.NullString{String: "sess-anchor", Valid: true}
	})
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	m.SetLifecycleCtx(context.Background())

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 等待异步 SSE align goroutine 收敛（mock oc.SubscribeEvents 立即 onReady 后阻塞，对齐在主路径内完成）。
	waitStatus(t, store, "t1", StatusActive, 2*time.Second)

	// 关键顺序：CAS suspended→activating → NewSession(serve) → UpdateTaskLastPort
	// → SubscribeEvents → AlignTaskSessions → UpdateTaskStatus(active)。
	assertOrdered(t, tr, []traceOp{
		{src: "store", op: "UpdateTaskStatusConditional", key: "suspended->activating"},
		{src: "proc", op: "NewSession", key: runtimeSessionName("t1")},
		{src: "proc", op: "WatchExit", key: runtimeSessionName("t1")},
		{src: "oc", op: "SubscribeEvents", key: ""},
		{src: "store", op: "AlignTaskSessions", key: "mode=1"},
		{src: "oc", op: "Health", key: ""},
		{src: "store", op: "UpdateTaskLastPort", key: "t1"},
		{src: "store", op: "UpdateTaskStatusConditional", key: "activating->active"},
	}, "Activate.success")

	assertOpCount(t, tr, "store", "UpdateTaskStatusConditional", 2, "Activate.success")
	assertOpNever(t, tr, "store", "UpdateTaskStatus", "Activate.success")
	// 无 BeginDeleteIntent/ArchiveTask/RestoreTask/DeleteTask（非删除/归档流程）。
	assertOpNever(t, tr, "store", "BeginDeleteIntent", "Activate.success")
	assertOpNever(t, tr, "store", "DeleteTask", "Activate.success")
	assertOpNever(t, tr, "store", "ArchiveTask", "Activate.success")
	// 无 wt.Remove（Activate 不删 worktree）。
	assertOpNever(t, tr, "wt", "Remove", "Activate.success")
}

// TestP141_Activate_GuardReject_Trace 冻结 Activate guard 拒绝路径零副作用：
// 状态非 suspended（active）→ invalid_state，不得 CAS、不得 NewSession、不得 UpdateTaskLastPort、不得 SubscribeEvents。
func TestP141_Activate_GuardReject_Trace(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1") // status=active，非 suspended
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected invalid_state on non-suspended activate")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("code = %s, want invalid_state", OpErrorCode(err))
	}

	// guard 拒绝：零 store 写、零 proc、零 wt、零 oc。
	// 注意：GetTask 为读不记录；状态 guard 在 CAS 之前，故 UpdateTaskStatusConditional MUST 零调用。
	assertNoSideEffects(t, tr, "Activate.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskStatusConditional", "Activate.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskEnvSnapshot", "Activate.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskLastPort", "Activate.guardReject")
	assertOpNever(t, tr, "proc", "NewSession", "Activate.guardReject")
	assertOpNever(t, tr, "oc", "SubscribeEvents", "Activate.guardReject")
}

// ============================================================
// Suspend 流程 trace 回归
// ============================================================

// TestP141_Suspend_Success_Trace 冻结 Suspend 成功路径副作用顺序：
// 前置 guard（GetTask status=active、GetProject kind）→ CAS active→suspending
// → clearRuntime（停 SSE/watch，内部 cancel 不记 trace）→ proc.HasSession(serve) 判分支
// → proc.KillSession(tui) → proc.KillSession(shells...) → proc.KillSession(serve)
// → finishSuspend：store.UpdateTaskEnvSnapshot(清空) → store.UpdateTaskStatus(suspended)。
func TestP141_Suspend_Success_Trace(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	// 预置 tui/serve 存活，让 Suspend kill 路径触发。
	proc.sessions[serveSessionName("t1")] = true
	proc.sessions[tuiSessionName("t1")] = true
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	// 构造 runtime（Suspend 入口 clearRuntime 需 runtime 存在以停 SSE/watch）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	if err := m.Suspend(context.Background(), "t1"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)

	// 关键顺序：CAS active→suspending → HasSession(serve) → KillSession → UpdateTaskEnvSnapshot → UpdateTaskStatus(suspended)。
	assertOrdered(t, tr, []traceOp{
		{src: "store", op: "UpdateTaskStatusConditional", key: "active->suspending"},
		{src: "proc", op: "KillSession", key: serveSessionName("t1")},
		{src: "store", op: "UpdateTaskEnvSnapshot", key: "valid=false"},
		{src: "store", op: "UpdateTaskStatus", key: "status=suspended"},
	}, "Suspend.success")

	// CAS active→suspending 恰好一次；UpdateTaskStatus(suspended) 恰好一次（finishSuspend 落账）。
	assertOpCount(t, tr, "store", "UpdateTaskStatusConditional", 1, "Suspend.success")
	if n := tr.countOp("store", "UpdateTaskStatus"); n != 1 || !tr.hasOpKey("store", "UpdateTaskStatus", "status=suspended") {
		t.Errorf("Suspend.success: UpdateTaskStatus(suspended) MUST 恰好一次, got %d\ntrace:\n%s", n, formatTrace(tr.snapshot()))
	}
	// 无 BeginDeleteIntent/DeleteTask/ArchiveTask/RestoreTask。
	assertOpNever(t, tr, "store", "BeginDeleteIntent", "Suspend.success")
	assertOpNever(t, tr, "store", "DeleteTask", "Suspend.success")
	assertOpNever(t, tr, "wt", "Remove", "Suspend.success")
	// Suspend 不起 serve/tui（无 NewSession）。
	assertOpNever(t, tr, "proc", "NewSession", "Suspend.success")
}

// TestP141_Suspend_GuardReject_Trace 冻结 Suspend guard 拒绝路径零副作用：
// 状态非 active（suspended）→ invalid_state，不得 CAS、不得 KillSession、不得清 env、不得 UpdateTaskStatus。
func TestP141_Suspend_GuardReject_Trace(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // 已 suspended
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)

	err := m.Suspend(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected invalid_state on non-active suspend")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("code = %s, want invalid_state", OpErrorCode(err))
	}

	// guard 拒绝：零 store 写、零 proc、零 wt、零 oc。
	assertNoSideEffects(t, tr, "Suspend.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskStatusConditional", "Suspend.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskEnvSnapshot", "Suspend.guardReject")
	assertOpNever(t, tr, "store", "UpdateTaskStatus", "Suspend.guardReject")
	assertOpNever(t, tr, "proc", "KillSession", "Suspend.guardReject")
}

// ============================================================
// Delete 流程 trace 回归
// ============================================================

// TestP141_Delete_Normal_Success_Trace 冻结 Delete Normal 成功路径副作用顺序：
// 静态检查（GetTask status、GetProject kind、init_status 门禁、wt.PreflightDelete、wt.DirtyFiles）
// → store.BeginDeleteIntent(deleting) → deleteResume：
//
//	retryDebtGate（row.Notice 空→通过）→ deleteOCSessions（ListTaskSessions 空→跳过）
//
// → killResidualSessions（proc.HasSession/ListSessions→无残余）→ pre-delete（无脚本→跳过）
// → wt.Remove → store.DeleteTask → clearRuntime。
//
// 关键不变量（design D0）：静态检查先于 BeginDeleteIntent（删除意图）与破坏性副作用
// （wt.Remove/DeleteTask）。本测试断言 PreflightDelete/DirtyFiles 在 BeginDeleteIntent 之前。
func TestP141_Delete_Normal_Success_Trace(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// 确保 worktree 路径存在以通过 runPreDeleteHook os.Stat 检查。
	wt := newMockWorktree()
	oc := newMockOC(true)
	proc := newMockProc()
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	// 构造 runtime（Delete 成功后 clearRuntime 需 runtime 存在）。
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	// 创建 worktree 目录让 pre-delete os.Stat 通过（非 IsNotExist）。
	wtDir := t.TempDir()
	store.mutTask("t1", func(t *TaskRow) { t.WorktreePath = wtDir })

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete normal: %v", err)
	}

	// 关键顺序：wt.PreflightDelete → wt.DirtyFiles → BeginDeleteIntent → wt.Remove → DeleteTask。
	// 静态检查（PreflightDelete/DirtyFiles）MUST 先于删除意图（BeginDeleteIntent）。
	assertOrdered(t, tr, []traceOp{
		{src: "wt", op: "PreflightDelete", key: "branch=ocdeck/my-task"},
		{src: "wt", op: "DirtyFiles", key: ""},
		{src: "store", op: "BeginDeleteIntent", key: "mode=normal"},
		{src: "wt", op: "Remove", key: "branch=ocdeck/my-task"},
		{src: "store", op: "DeleteTask", key: "id=t1"},
	}, "Delete.normal.success")

	// BeginDeleteIntent 恰好一次；DeleteTask 恰好一次。
	assertOpCount(t, tr, "store", "BeginDeleteIntent", 1, "Delete.normal.success")
	assertOpCount(t, tr, "store", "DeleteTask", 1, "Delete.normal.success")
	// 无 ArchiveTask/RestoreTask/CommitCreated（非归档/恢复/创建流程）。
	assertOpNever(t, tr, "store", "ArchiveTask", "Delete.normal.success")
	assertOpNever(t, tr, "store", "RestoreTask", "Delete.normal.success")
	assertOpNever(t, tr, "store", "CommitCreated", "Delete.normal.success")
	// wt.Remove 恰好一次。
	assertOpCount(t, tr, "wt", "Remove", 1, "Delete.normal.success")
}

// TestP141_Delete_Force_Success_Trace 冻结 Delete Force 成功路径副作用顺序：
// Force 跳过 oc session 删除（deleteOCSessions）与 pre-delete 脚本，但仍做静态检查
// （PreflightDelete/DirtyFiles，repo）与 BeginDeleteIntent、wt.Remove、DeleteTask。
func TestP141_Delete_Force_Success_Trace(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	wt := newMockWorktree()
	oc := newMockOC(true)
	proc := newMockProc()
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)

	wtDir := t.TempDir()
	store.mutTask("t1", func(t *TaskRow) { t.WorktreePath = wtDir })

	if err := m.Delete(context.Background(), "t1", DeleteForce, false); err != nil {
		t.Fatalf("Delete force: %v", err)
	}

	// Force 仍先静态检查（repo）→ BeginDeleteIntent(force) → wt.Remove → DeleteTask。
	// Force 跳过 oc session 删除（deleteOCSessions），但仍做 wt.Remove（repo）。
	assertOrdered(t, tr, []traceOp{
		{src: "wt", op: "PreflightDelete", key: "confirm=false"},
		{src: "wt", op: "DirtyFiles", key: ""},
		{src: "store", op: "BeginDeleteIntent", key: "mode=force"},
		{src: "wt", op: "Remove", key: "branch=ocdeck/my-task"},
		{src: "store", op: "DeleteTask", key: "id=t1"},
	}, "Delete.force.success")

	assertOpCount(t, tr, "store", "BeginDeleteIntent", 1, "Delete.force.success")
	assertOpCount(t, tr, "store", "DeleteTask", 1, "Delete.force.success")
	// Force 跳过 oc session 删除：无 oc.DeleteSession（无 session 归属，且 Force 分支跳过 deleteOCSessions）。
	assertOpNever(t, tr, "oc", "DeleteSession", "Delete.force.success")
}

// TestP141_Delete_GuardReject_Trace 冻结 Delete guard 拒绝路径零副作用：
// 状态 active（非 deletable）→ invalid_state，不得 BeginDeleteIntent、不得 PreflightDelete（active 不在 deletable 集合，门禁先于 PreflightDelete）、不得 wt.Remove、不得 DeleteTask。
func TestP141_Delete_GuardReject_Trace(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1") // active 不可删除
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil {
		t.Fatal("expected invalid_state on delete from active")
	}
	if OpErrorCode(err) != codeInvalidState {
		t.Fatalf("code = %s, want invalid_state", OpErrorCode(err))
	}

	// guard 拒绝：零 store 写、零 wt、零 proc、零 oc。
	// deleteAllowedStatus(active, Normal)=false，在 PreflightDelete 之前返回。
	assertNoSideEffects(t, tr, "Delete.guardReject")
	assertOpNever(t, tr, "store", "BeginDeleteIntent", "Delete.guardReject")
	assertOpNever(t, tr, "store", "DeleteTask", "Delete.guardReject")
	assertOpNever(t, tr, "wt", "PreflightDelete", "Delete.guardReject")
	assertOpNever(t, tr, "wt", "DirtyFiles", "Delete.guardReject")
	assertOpNever(t, tr, "wt", "Remove", "Delete.guardReject")
}

// TestP141_Delete_StaticCheckBeforeIntent_Trace 显式断言 design D0 不变量：
// Delete 静态检查（PreflightDelete/DirtyFiles）先于删除意图 BeginDeleteIntent 与破坏性副作用（wt.Remove）。
// 这是 P1.4.1 锁定的关键不变量，后续迁移 MUST 保持。
func TestP141_Delete_StaticCheckBeforeIntent_Trace(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	tr := &tracer{}
	m := newTraceTestManager(t, store, proc, wt, oc, tr)
	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	wtDir := t.TempDir()
	store.mutTask("t1", func(t *TaskRow) { t.WorktreePath = wtDir })

	if err := m.Delete(context.Background(), "t1", DeleteNormal, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	events := tr.snapshot()
	idxPreflight := indexOfOp(events, "wt", "PreflightDelete")
	idxDirty := indexOfOp(events, "wt", "DirtyFiles")
	idxIntent := indexOfOp(events, "store", "BeginDeleteIntent")
	idxRemove := indexOfOp(events, "wt", "Remove")
	idxDelete := indexOfOp(events, "store", "DeleteTask")

	if idxPreflight < 0 || idxDirty < 0 || idxIntent < 0 || idxRemove < 0 || idxDelete < 0 {
		t.Fatalf("Delete.staticCheckBeforeIntent: 关键副作用缺失\ntrace:\n%s", formatTrace(events))
	}
	if !(idxPreflight < idxIntent && idxDirty < idxIntent) {
		t.Errorf("Delete.staticCheckBeforeIntent: 静态检查 MUST 先于删除意图，got preflight=%d dirty=%d intent=%d\ntrace:\n%s",
			idxPreflight, idxDirty, idxIntent, formatTrace(events))
	}
	if !(idxIntent < idxRemove && idxIntent < idxDelete) {
		t.Errorf("Delete.staticCheckBeforeIntent: 删除意图 MUST 先于破坏性副作用，got intent=%d remove=%d delete=%d\ntrace:\n%s",
			idxIntent, idxRemove, idxDelete, formatTrace(events))
	}
}

// indexOfOp 返回 events 中首个 src+op 的索引，不存在返回 -1。
func indexOfOp(events []traceOp, src, op string) int {
	for i, e := range events {
		if e.src == src && e.op == op {
			return i
		}
	}
	return -1
}

// waitStatus 轮询等待任务状态到达 want（异步 Activate/InitRunner 测试用）。
func waitStatus(t *testing.T, store TaskStore, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, _ := store.GetTask(context.Background(), taskID)
		if row.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := store.GetTask(context.Background(), taskID)
	t.Fatalf("wait status %s timeout: got %s (task %s)", want, row.Status, taskID)
}

// errSentinel 仅为编译期保留（占位，避免未使用 import 警告）；当前断言用 OpErrorCode。
var _ = func() bool { return true }
