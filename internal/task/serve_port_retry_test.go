package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- helpers for serve port-retry D2 tests ---

// alwaysUnhealthyOC：Health 永不就绪；Probe 可注入。
type alwaysUnhealthyOC struct {
	probeErr error
}

func (c *alwaysUnhealthyOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return opencode.HealthResponse{}, opencode.ErrServeNotReady
}
func (c *alwaysUnhealthyOC) Probe(ctx context.Context) (string, error) {
	if c.probeErr != nil {
		return "", c.probeErr
	}
	return opencode.ContractBaseline, nil
}
func (c *alwaysUnhealthyOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return nil, nil
}
func (c *alwaysUnhealthyOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return opencode.Session{}, opencode.ErrSessionNotFound
}
func (c *alwaysUnhealthyOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 1, Updated: 1}}, nil
}
func (c *alwaysUnhealthyOC) DeleteSession(ctx context.Context, dir, id string) error { return nil }
func (c *alwaysUnhealthyOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return nil, nil
}
func (c *alwaysUnhealthyOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	<-ctx.Done()
	return ctx.Err()
}
func (c *alwaysUnhealthyOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}
func (c *alwaysUnhealthyOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}

// healthThenProbeOC：Health 就绪；Probe 返回注入错误。
type healthThenProbeOC struct {
	probeErr error
}

func (c *healthThenProbeOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return opencode.HealthResponse{Healthy: true, Version: opencode.ContractBaseline}, nil
}
func (c *healthThenProbeOC) Probe(ctx context.Context) (string, error) {
	if c.probeErr != nil {
		return "", c.probeErr
	}
	return opencode.ContractBaseline, nil
}
func (c *healthThenProbeOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return nil, nil
}
func (c *healthThenProbeOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return opencode.Session{}, opencode.ErrSessionNotFound
}
func (c *healthThenProbeOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 1, Updated: 1}}, nil
}
func (c *healthThenProbeOC) DeleteSession(ctx context.Context, dir, id string) error { return nil }
func (c *healthThenProbeOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return nil, nil
}
func (c *healthThenProbeOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	<-ctx.Done()
	return ctx.Err()
}
func (c *healthThenProbeOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}
func (c *healthThenProbeOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}

func newServeRetryManager(t *testing.T, store TaskStore, proc ProcessBackend, oc OCClient, pr config.PortRange) *Manager {
	t.Helper()
	if pr.Min == 0 && pr.Max == 0 {
		pr = config.PortRange{Min: 50000, Max: 50999}
	}
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: pr,
		ShutdownPolicy: config.ShutdownPersist,
	}
	factory := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOCWrap{inner: oc, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: factory,
	})
	// 测试注入短健康轮询：生产默认 10s/500ms 不变；整套 D2 用例应在数十秒内完成。
	m.serveReadyTimeout = 30 * time.Millisecond
	m.serveReadyPollInterval = time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond, time.Millisecond} }
	return m
}

// withFastServeReady 为任意 Manager 注入短健康轮询（供 newTestManager 路径复用）。
func withFastServeReady(m *Manager) *Manager {
	m.serveReadyTimeout = 30 * time.Millisecond
	m.serveReadyPollInterval = time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond, time.Millisecond} }
	return m
}

// noticeFailNStore：前 failN 次 UpdateTaskNoticeCAS 失败（返回 replaced=false），之后透传。
type noticeFailNStore struct {
	*mockStore
	mu    sync.Mutex
	failN int
	calls int
}

func (s *noticeFailNStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n <= s.failN {
		return application.MutationResult{}, nil
	}
	return s.mockStore.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// noticeAlwaysFailStore：notice CAS 永远不收敛。
type noticeAlwaysFailStore struct {
	*mockStore
}

func (s *noticeAlwaysFailStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	return application.MutationResult{}, nil
}

// killSeqProc：按调用序号返回 KillResult / error 序列；耗尽后 clean。
type killSeqProc struct {
	*mockProc
	mu  sync.Mutex
	seq []killSeqStep
	idx int
	// autoDieAfterKill：SessionKilled=true 后从 sessions 移除（mockProc 已做）。
}

type killSeqStep struct {
	res process.KillResult
	err error
}

func (p *killSeqProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	var step killSeqStep
	if p.idx < len(p.seq) {
		step = p.seq[p.idx]
		p.idx++
	} else {
		step = killSeqStep{res: process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}}
	}
	p.mu.Unlock()
	// 记录顺序。
	p.mockProc.mu.Lock()
	p.mockProc.killOrder = append(p.mockProc.killOrder, name)
	if step.err != nil {
		p.mockProc.mu.Unlock()
		return process.KillResult{}, step.err
	}
	if step.res.SessionKilled {
		delete(p.mockProc.sessions, name)
	}
	p.mockProc.mu.Unlock()
	return step.res, nil
}

func residualNotices(row TaskRow) []noticeEntry {
	entries, _ := parseNotices(row.Notice)
	var out []noticeEntry
	for _, e := range entries {
		if e.Code == noticeCodeResidual {
			out = append(out, e)
		}
	}
	return out
}

func noticeTicketsOf(e noticeEntry) []string {
	return noticeTickets(e)
}

func countNewServe(proc *mockProc, taskID string) int {
	name := runtimeSessionName(taskID)
	n := 0
	for _, s := range proc.newSessionNamesSnapshot() {
		if s == name {
			n++
		}
	}
	return n
}

func countKillServe(proc *mockProc, taskID string) int {
	name := runtimeSessionName(taskID)
	n := 0
	for _, s := range proc.killOrderSnapshot() {
		if s == name {
			n++
		}
	}
	return n
}

// --- 2.1 allocatePort exclude ---

func TestAllocatePort_ExcludeSkipsLastPortAndScan(t *testing.T) {
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	m.cfg.ServePortRange = config.PortRange{Min: 50000, Max: 50002}

	// lastPort 空闲但被 exclude → 不得返回 exclude。
	p, err := m.allocatePort(sql.NullInt64{Int64: 50000, Valid: true}, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if p == 50000 {
		t.Fatalf("exclude=50000 仍返回 50000")
	}

	// 单端口范围且被排除 → exhausted。
	m.cfg.ServePortRange = config.PortRange{Min: 50010, Max: 50010}
	_, err = m.allocatePort(sql.NullInt64{}, 50010)
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("want exhausted, got %v", err)
	}
}

// --- 2.2 sentinel ---

func TestWaitServeReadyOrDead_SessionDiedIsSentinel(t *testing.T) {
	store := newMockStore()
	proc := newMockProc()
	oc := newMockOC(false)
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	err := m.waitServeReadyOrDead(context.Background(), oc, runtimeSessionName("t1"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errServeSessionDied) {
		t.Fatalf("errors.Is(errServeSessionDied)=false, err=%v", err)
	}
}

// --- 2.3/2.4 port retry scenarios ---

func TestPortRetry_AdjacentPortsDifferOnHealthTimeoutClean(t *testing.T) {
	// 健康检查持续失败 + Kill clean → 相邻 NewSession 端口不同；末次无 allocate 幽灵端口。
	// 仅前两轮 health 超时（会话存活 + clean kill），第三轮 health 成功。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	// 记录每次 NewSession 的 --port。
	var ports []int
	var portsMu sync.Mutex
	proc := &portRecordingProc{mockProc: base, ports: &ports, mu: &portsMu}
	// 前 2 次 New 对应的 client 永不健康；第 3 次健康。
	var healthClients atomic.Int32
	factory := func(port int, password string, opts opencode.Options) OCClient {
		n := healthClients.Add(1)
		var inner OCClient
		if n <= 2 {
			inner = &alwaysUnhealthyOC{}
		} else {
			inner = newMockOC(true)
		}
		return &readyOCWrap{inner: inner, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg:   &config.Config{DataDir: t.TempDir(), ServePortRange: config.PortRange{Min: 50000, Max: 50010}, ShutdownPolicy: config.ShutdownPersist},
		Store: store, Proc: proc, Worktree: newMockWorktree(), OCFactory: factory,
	})
	withFastServeReady(m)
	env, err := m.mergeEnvSnapshot(context.Background(), row, 50000)
	if err != nil {
		t.Fatal(err)
	}
	final, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err != nil {
		t.Fatalf("startServeWithPortRetry: %v", err)
	}
	portsMu.Lock()
	got := append([]int(nil), ports...)
	portsMu.Unlock()
	if len(got) != 3 {
		t.Fatalf("NewSession ports=%v want 3", got)
	}
	if got[0] == got[1] || got[1] == got[2] {
		t.Fatalf("adjacent ports must differ: %v", got)
	}
	if final != got[2] {
		t.Fatalf("final=%d want last port %d", final, got[2])
	}
	// clean kill 无 residual notice。
	if n := residualNotices(mustGet(store, "t1")); len(n) != 0 {
		t.Fatalf("clean should not record notice: %+v", n)
	}
}

// portRecordingProc 记录每次 NewSession 的 --port 参数。
type portRecordingProc struct {
	*mockProc
	ports *[]int
	mu    *sync.Mutex
}

func (p *portRecordingProc) NewSession(spec process.SessionSpec) error {
	port := 0
	for i := 0; i+1 < len(spec.CmdArgv); i++ {
		if spec.CmdArgv[i] == "--port" {
			port, _ = strconv.Atoi(spec.CmdArgv[i+1])
			break
		}
	}
	p.mu.Lock()
	*p.ports = append(*p.ports, port)
	p.mu.Unlock()
	return p.mockProc.NewSession(spec)
}

func TestPortRetry_SessionDiedRotatesWithoutKill(t *testing.T) {
	// 进程已死亡：KillSession=0，成功轮换，无 notice。
	// 仅前 1 次 NewSession 后死亡；第二次存活后 health 成功。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	var newCount atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 1, newCount: &newCount}
	oc := newMockOC(true)
	m := newServeRetryManager(t, store, proc, oc, config.PortRange{Min: 50000, Max: 50010})
	env, err := m.mergeEnvSnapshot(context.Background(), row, 50000)
	if err != nil {
		t.Fatal(err)
	}
	final, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err != nil {
		t.Fatalf("startServeWithPortRetry: %v", err)
	}
	if final == 50000 {
		t.Fatalf("expected port rotate away from 50000, got %d", final)
	}
	if countKillServe(base, "t1") != 0 {
		t.Fatalf("KillSession calls=%d want 0", countKillServe(base, "t1"))
	}
	if n := residualNotices(mustGet(store, "t1")); len(n) != 0 {
		t.Fatalf("notice should be empty, got %+v", n)
	}
	if countNewServe(base, "t1") != 2 {
		t.Fatalf("NewSession count=%d want 2", countNewServe(base, "t1"))
	}
}

// selectiveDieProc：前 dieUntil 次 NewSession 后立即删会话。
type selectiveDieProc struct {
	*mockProc
	dieUntil int32
	newCount *atomic.Int32
}

func (p *selectiveDieProc) NewSession(spec process.SessionSpec) error {
	if err := p.mockProc.NewSession(spec); err != nil {
		return err
	}
	n := p.newCount.Add(1)
	if n <= p.dieUntil {
		p.mockProc.mu.Lock()
		delete(p.mockProc.sessions, spec.Name)
		p.mockProc.mu.Unlock()
	}
	return nil
}

func mustGet(store *mockStore, id string) TaskRow {
	row, _ := store.GetTask(context.Background(), id)
	return row
}

func TestPortRetry_KillInfraErrorTerminalNotice(t *testing.T) {
	// KillSession err != nil → 终态 kill_failed notice；分支后零 NewSession/allocate。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	proc := wrapProcKill(base, map[string]error{runtimeSessionName("t1"): errors.New("tmux kill infra")})
	oc := &alwaysUnhealthyOC{}
	m := newServeRetryManager(t, store, proc, oc, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected terminal error")
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession after branch should be 0 extra; total=%d want 1", countNewServe(base, "t1"))
	}
	notices := residualNotices(mustGet(store, "t1"))
	if len(notices) != 1 {
		t.Fatalf("notices=%d want 1, %+v", len(notices), notices)
	}
	if notices[0].Data["reason"] != noticeReasonKillFailed {
		t.Errorf("reason=%v want kill_failed", notices[0].Data["reason"])
	}
	if !strings.Contains(err.Error(), "kill:") {
		t.Errorf("error should aggregate kill context: %v", err)
	}
}

func TestPortRetry_ReapFailedTerminalWithTickets(t *testing.T) {
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled:  true,
		Disposition:    process.DispositionReapFailed,
		CleanupTickets: []string{"tk-reap-1"},
	}
	oc := &alwaysUnhealthyOC{}
	m := newServeRetryManager(t, store, base, oc, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected terminal")
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
	}
	notices := residualNotices(mustGet(store, "t1"))
	if len(notices) != 1 {
		t.Fatalf("notices=%d %+v", len(notices), notices)
	}
	if notices[0].Data["reason"] != noticeReasonReapFailed {
		t.Errorf("reason=%v", notices[0].Data["reason"])
	}
	tks := noticeTicketsOf(notices[0])
	if len(tks) != 1 || tks[0] != "tk-reap-1" {
		t.Errorf("tickets=%v", tks)
	}
}

func TestPortRetry_ReapFailedNoticeWriteFail_PendingHandoff(t *testing.T) {
	// 首次 notice 写失败 → pendingCleanupError；外层补偿回放成功（会话已 reap，无二次 kill）。
	inner := newMockStore()
	store := &noticeFailNStore{mockStore: inner, failN: 8} // 首次 record 的 8 次 CAS 全失败
	row := seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) { tr.Status = StatusActivating })
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled:  true,
		Disposition:    process.DispositionReapFailed,
		CleanupTickets: []string{"tk-handoff"},
	}
	oc := &alwaysUnhealthyOC{}
	m := newServeRetryManager(t, store, base, oc, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected error")
	}
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("want pendingCleanupError, got %T %v", err, err)
	}
	if pce.pending.reason != noticeReasonReapFailed {
		t.Errorf("pending reason=%s", pce.pending.reason)
	}
	if len(pce.pending.cleanupTickets) != 1 || pce.pending.cleanupTickets[0] != "tk-handoff" {
		t.Errorf("pending tickets=%v", pce.pending.cleanupTickets)
	}
	// reap 后会话已不存在 → 外层 compensation 无需二次 kill；failN 已耗尽，回放 CAS 成功。
	killsBefore := countKillServe(base, "t1")
	m.runActivateFailureCompensation(context.Background(), "t1", err)
	if countKillServe(base, "t1") != killsBefore {
		t.Fatalf("outer must not re-kill reaped session: before=%d after=%d", killsBefore, countKillServe(base, "t1"))
	}
	row2 := mustGet(inner, "t1")
	if row2.Status != StatusSuspended {
		t.Errorf("status=%s want suspended", row2.Status)
	}
	notices := residualNotices(row2)
	if len(notices) == 0 {
		t.Fatal("expected notice after replay")
	}
	found := false
	for _, n := range notices {
		for _, tk := range noticeTicketsOf(n) {
			if tk == "tk-handoff" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tickets not in notice: %+v", notices)
	}
}

func TestPortRetry_SnapshotFailedAndKillFailedTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  process.KillResult
		want string
	}{
		{"snapshot_failed", process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotFailed}, noticeReasonSnapshotFailed},
		{"kill_failed", process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed, CleanupTickets: []string{"tk"}}, noticeReasonKillFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			row := seedSuspendedTask(store, "t1", "p1")
			base := newMockProc()
			base.killResults[runtimeSessionName("t1")] = tc.res
			m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
			env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
			_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
			if err == nil {
				t.Fatal("expected terminal")
			}
			if countNewServe(base, "t1") != 1 {
				t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
			}
			notices := residualNotices(mustGet(store, "t1"))
			if len(notices) != 1 || notices[0].Data["reason"] != tc.want {
				t.Fatalf("notices=%+v want reason %s", notices, tc.want)
			}
		})
	}
}

func TestPortRetry_SnapshotMissingDegradedContinuesRotate(t *testing.T) {
	// 首次 kill → snapshot_missing_degraded + notice；第二次 health 成功。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	proc := &killSeqProc{mockProc: base, seq: []killSeqStep{
		{res: process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}},
	}}
	failPort := 50000
	ocFactory := func(port int, password string, opts opencode.Options) OCClient {
		return &readyOCWrap{inner: &portHealthOC{port: port, failPort: failPort}, onReady: opts.OnReady}
	}
	m := New(Options{
		Cfg:   &config.Config{DataDir: t.TempDir(), ServePortRange: config.PortRange{Min: 50000, Max: 50005}, ShutdownPolicy: config.ShutdownPersist},
		Store: store, Proc: proc, Worktree: newMockWorktree(), OCFactory: ocFactory,
	})
	withFastServeReady(m)
	env, _ := m.mergeEnvSnapshot(context.Background(), row, failPort)
	final, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), failPort, "pw", env, "")
	if err != nil {
		t.Fatalf("expected rotate success: %v", err)
	}
	if final == failPort {
		t.Fatal("expected different port")
	}
	notices := residualNotices(mustGet(store, "t1"))
	if len(notices) != 1 || notices[0].Data["reason"] != noticeReasonSnapshotMissing {
		t.Fatalf("notices=%+v", notices)
	}
	if rb, _ := notices[0].Data["retryable"].(bool); rb {
		t.Error("snapshot_missing_degraded retryable should be false")
	}
	if countNewServe(base, "t1") != 2 {
		t.Fatalf("NewSession=%d want 2", countNewServe(base, "t1"))
	}
}

func TestPortRetry_SnapshotMissingNoticeWriteFailTerminal(t *testing.T) {
	inner := newMockStore()
	store := &noticeAlwaysFailStore{mockStore: inner}
	row := seedSuspendedTask(inner, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded,
	}
	m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected terminal on notice write fail")
	}
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("want pendingCleanupError, got %v", err)
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("must not rotate: NewSession=%d", countNewServe(base, "t1"))
	}
}

func TestPortRetry_UnknownAndContradictoryKillResultTerminal(t *testing.T) {
	cases := []struct {
		name string
		res  process.KillResult
	}{
		{"unknown_disp", process.KillResult{SessionKilled: true, Disposition: process.CleanupDisposition("weird")}},
		{"contradict_clean", process.KillResult{SessionKilled: false, Disposition: process.DispositionClean}},
		{"contradict_reap", process.KillResult{SessionKilled: false, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"t"}}},
		{"contradict_snap", process.KillResult{SessionKilled: true, Disposition: process.DispositionSnapshotFailed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			row := seedSuspendedTask(store, "t1", "p1")
			base := newMockProc()
			base.killResults[runtimeSessionName("t1")] = tc.res
			m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
			env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
			_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
			if err == nil {
				t.Fatal("expected terminal")
			}
			notices := residualNotices(mustGet(store, "t1"))
			if len(notices) != 1 || notices[0].Data["reason"] != noticeReasonKillFailed {
				t.Fatalf("must explicit kill_failed notice, got %+v", notices)
			}
			if countNewServe(base, "t1") != 1 {
				t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
			}
		})
	}
}

func TestPortRetry_PersistEnvSnapshotFailNoNewSession(t *testing.T) {
	// 进程死亡换端口后 persist 失败 → NewSession 总次数=1。
	store := &envSnapshotFailStore{mockStore: newMockStore()}
	row := seedSuspendedTask(store.mockStore, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 1, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected persist failure")
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Errorf("err=%v", err)
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d want 1 (no next attempt)", countNewServe(base, "t1"))
	}
}

type envSnapshotFailStore struct {
	*mockStore
}

func (s *envSnapshotFailStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if envSnapshot.Valid {
		return application.MutationResult{}, errors.New("db write failed")
	}
	return s.mockStore.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

func TestPortRetry_NoticeWriteFailNoAllocate(t *testing.T) {
	inner := newMockStore()
	store := &noticeAlwaysFailStore{mockStore: inner}
	row := seedSuspendedTask(inner, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"},
	}
	m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected error")
	}
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("want pendingCleanupError: %v", err)
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d", countNewServe(base, "t1"))
	}
}

func TestPortRetry_AllocateExhaustedWrapsAerr(t *testing.T) {
	// 进程死亡 + 单端口范围 exclude → exhausted 包装 aerr。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 99, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50000})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected exhausted")
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%s want conflict", OpErrorCode(err))
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("must wrap allocate error: %v", err)
	}
	// 不得被健康检查文案单独覆盖：应含 no free port / exhausted。
	if !strings.Contains(err.Error(), "no free port") && !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("err=%v", err)
	}
}

// countingEnvStore 统计 Valid env snapshot 写入次数与最后一次写入的端口，用于末次预算零副作用断言。
type countingEnvStore struct {
	*mockStore
	mu       sync.Mutex
	writes   int
	lastPort string
}

func (s *countingEnvStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, snapSQL sql.NullString) (application.MutationResult, error) {
	if snapSQL.Valid {
		s.mu.Lock()
		s.writes++
		var snap envSnapshot
		if err := json.Unmarshal([]byte(snapSQL.String), &snap); err == nil && snap.Vars != nil {
			s.lastPort = snap.Vars["OCDECK_SERVE_PORT"]
		}
		s.mu.Unlock()
	}
	return s.mockStore.UpdateTaskEnvSnapshot(ctx, id, snapSQL)
}

func (s *countingEnvStore) snapshot() (writes int, lastPort string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes, s.lastPort
}

func TestPortRetry_LastAttemptNoAllocate(t *testing.T) {
	// 3 次进程死亡 → 末次不再 allocate/persist：
	// - NewSession=3（无第 4 次）
	// - Valid env 写入仅 2 次（attempt0→1、attempt1→2；末次不得再 persist）
	// - 最终快照端口 = 返回端口 = 第 3 次会话端口（无幽灵第 4 端口）
	// 若回归为「末次额外 allocate+persist 后不 NewSession」，writes 会变成 3 或 lastSnapPort 偏离。
	inner := newMockStore()
	store := &countingEnvStore{mockStore: inner}
	row := seedSuspendedTask(inner, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 99, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	// 仅统计 startServeWithPortRetry 期间的 Valid persist（排除 mergeEnvSnapshot 可能的初始写）。
	writesBefore, _ := store.snapshot()
	finalPort, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected fail after retries")
	}
	if countNewServe(base, "t1") != servePortRetries {
		t.Fatalf("NewSession=%d want %d", countNewServe(base, "t1"), servePortRetries)
	}
	writesAfter, lastSnapPort := store.snapshot()
	writes := writesAfter - writesBefore
	if writes != servePortRetries-1 {
		t.Fatalf("env snapshot Valid writes during retry=%d want %d (no persist on last attempt; before=%d after=%d)", writes, servePortRetries-1, writesBefore, writesAfter)
	}
	if lastSnapPort == "" {
		t.Fatal("last snapshot port empty")
	}
	if lastSnapPort != strconv.Itoa(finalPort) {
		t.Fatalf("last snapshot port=%s finalPort=%d (ghost allocate would diverge)", lastSnapPort, finalPort)
	}
	if env["OCDECK_SERVE_PORT"] != strconv.Itoa(finalPort) {
		t.Errorf("env port=%s final=%d", env["OCDECK_SERVE_PORT"], finalPort)
	}
	if !strings.Contains(err.Error(), "serve not ready") {
		t.Errorf("err=%v", err)
	}
}

func TestPortRetry_CtxCancelZeroLocalSideEffects(t *testing.T) {
	// wait 期间 ctx 取消 → 零本地 kill/allocate/persist。
	// 注意：cancel 必须发生在健康墙钟超时之前，否则会先走 kill 门禁（与契约无关的竞态）。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	oc := &alwaysUnhealthyOC{}
	m := newServeRetryManager(t, store, base, oc, config.PortRange{Min: 50000, Max: 50005})
	// 拉长健康超时，保证 cancel 落在轮询窗口内而非墙钟 timeout 之后。
	m.serveReadyTimeout = 2 * time.Second
	m.serveReadyPollInterval = 5 * time.Millisecond
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := m.startServeWithPortRetry(ctx, row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v want canceled", err)
	}
	if countKillServe(base, "t1") != 0 {
		t.Fatalf("KillSession=%d want 0", countKillServe(base, "t1"))
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
	}
}

func TestPortRetry_ProbeFailuresNoLocalKillNoRotate(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"not_ready", opencode.ErrServeNotReady, codeProcessError},
		{"unauthorized", opencode.ErrUnauthorized, codeInternal},
		{"mismatch", opencode.ErrCapabilityMismatch, codeOCIncompatible},
		{"unknown", errors.New("weird probe"), codeProcessError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			row := seedSuspendedTask(store, "t1", "p1")
			base := newMockProc()
			oc := &healthThenProbeOC{probeErr: tc.err}
			m := newServeRetryManager(t, store, base, oc, config.PortRange{Min: 50000, Max: 50005})
			// 冷启动 backoff 缩短
			m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond, time.Millisecond} }
			env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
			_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
			if err == nil {
				t.Fatal("expected probe error")
			}
			if OpErrorCode(err) != tc.code {
				t.Errorf("code=%s want %s err=%v", OpErrorCode(err), tc.code, err)
			}
			if countKillServe(base, "t1") != 0 {
				t.Fatalf("local KillSession=%d want 0", countKillServe(base, "t1"))
			}
			if countNewServe(base, "t1") != 1 {
				t.Fatalf("NewSession=%d want 1 (no rotate)", countNewServe(base, "t1"))
			}
		})
	}
}

func TestPortRetry_ProbeErrorImmutableOnActivateCompensation(t *testing.T) {
	// 负向合同：Activate 返回错误保持 Probe 分类/文案，不含 cleanup/disposition/notice。
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk-comp"},
	}
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrCapabilityMismatch
	m := withFastServeReady(newTestManager(t, store, base, newMockWorktree(), oc))
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected error")
	}
	if OpErrorCode(err) != codeOCIncompatible {
		t.Errorf("code=%s want oc_incompatible", OpErrorCode(err))
	}
	msg := err.Error()
	if strings.Contains(msg, "disposition") || strings.Contains(msg, "cleanup notice") || strings.Contains(msg, "reap_failed") {
		t.Errorf("Activate return must not include cleanup context: %s", msg)
	}
	if !strings.Contains(msg, "capability") && !errors.Is(err, opencode.ErrCapabilityMismatch) {
		// probeErrToOpCode wraps capability probe
		if !strings.Contains(msg, "capability probe") {
			t.Errorf("want probe classification text: %s", msg)
		}
	}
	row := mustGet(store, "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s", row.Status)
	}
	// last_error 可含 cleanup；返回错误不含。
	if !row.LastError.Valid {
		t.Error("last_error should be set")
	}
}

func TestPortRetry_ProbeCompensation_CleanNoNotice(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc() // kill → clean
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrServeNotReady
	m := withFastServeReady(newTestManager(t, store, base, newMockWorktree(), oc))
	_ = m.Activate(context.Background(), "t1")
	if n := residualNotices(mustGet(store, "t1")); len(n) != 0 {
		t.Fatalf("clean should not record notice: %+v", n)
	}
	if countKillServe(base, "t1") == 0 {
		t.Error("outer compensation should kill serve")
	}
}

func TestPortRetry_ProbeCompensation_ReapFailedTickets(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk-probe"},
	}
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrServeNotReady
	m := withFastServeReady(newTestManager(t, store, base, newMockWorktree(), oc))
	_ = m.Activate(context.Background(), "t1")
	notices := residualNotices(mustGet(store, "t1"))
	found := false
	for _, n := range notices {
		for _, tk := range noticeTicketsOf(n) {
			if tk == "tk-probe" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tickets lost: %+v", notices)
	}
}

func TestPortRetry_ProbeCompensation_UnknownDispositionFailClosed(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.CleanupDisposition("totally_unknown"),
	}
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrServeNotReady
	m := withFastServeReady(newTestManager(t, store, base, newMockWorktree(), oc))
	_ = m.Activate(context.Background(), "t1")
	notices := residualNotices(mustGet(store, "t1"))
	if len(notices) == 0 {
		t.Fatal("unknown disposition must record kill_failed notice (not silent via dispositionToNotice)")
	}
	if notices[0].Data["reason"] != noticeReasonKillFailed {
		t.Errorf("reason=%v", notices[0].Data["reason"])
	}
}

// --- fold / replay ---

func TestFoldPendingCleanups_MergeLatestReasonUnionTickets(t *testing.T) {
	cause := &pendingCleanupError{
		pending: pendingCleanup{
			sessionName: "ocdeck-t1-serve", cleanupTickets: []string{"tk-old"},
			reason: noticeReasonKillFailed, retryable: true,
		},
		noticeErr: errors.New("cas"),
		cause:     errors.New("serve not ready"),
	}
	obs := []cleanupObservation{
		{sessionName: "ocdeck-t1-serve", reason: noticeReasonSnapshotMissing, retryable: false, cleanupTickets: []string{"tk-new"}, persisted: false},
	}
	out := foldPendingCleanups(cause, obs)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	if out[0].reason != noticeReasonSnapshotMissing || out[0].retryable {
		t.Errorf("got reason=%s retryable=%v", out[0].reason, out[0].retryable)
	}
	if len(out[0].cleanupTickets) != 2 {
		t.Errorf("union tickets=%v", out[0].cleanupTickets)
	}
}

func TestFoldPendingCleanups_PersistedTrueUpdatesReasonNoReplayIfAllPersisted(t *testing.T) {
	// 旧 pending 未落库 + 新 observation 写成功：回放项 reason 取新值，tickets 含旧。
	cause := &pendingCleanupError{
		pending: pendingCleanup{
			sessionName: "s1", cleanupTickets: []string{"tk1"},
			reason: noticeReasonKillFailed, retryable: true,
		},
		noticeErr: errors.New("fail"),
		cause:     errors.New("x"),
	}
	obs := []cleanupObservation{
		{sessionName: "s1", reason: noticeReasonSnapshotMissing, retryable: false, cleanupTickets: nil, persisted: true},
	}
	out := foldPendingCleanups(cause, obs)
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].reason != noticeReasonSnapshotMissing {
		t.Errorf("reason=%s want snapshot_missing (not covered by old pending)", out[0].reason)
	}
	if len(out[0].cleanupTickets) != 1 || out[0].cleanupTickets[0] != "tk1" {
		t.Errorf("tickets=%v", out[0].cleanupTickets)
	}
}

func TestFoldPendingCleanups_CleanDoesNotCancelPending(t *testing.T) {
	cause := &pendingCleanupError{
		pending: pendingCleanup{
			sessionName: "s1", cleanupTickets: []string{"tk1"},
			reason: noticeReasonKillFailed, retryable: true,
		},
		noticeErr: errors.New("fail"),
		cause:     errors.New("x"),
	}
	// clean 不形成 observation → 仅 cause pending 回放。
	out := foldPendingCleanups(cause, nil)
	if len(out) != 1 || out[0].reason != noticeReasonKillFailed {
		t.Fatalf("got %+v", out)
	}
}

func TestReplayPendingCleanups_FailureAggregatesLastError(t *testing.T) {
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) { tr.Status = StatusActivating })
	store := &noticeAlwaysFailStore{mockStore: inner}
	base := newMockProc()
	m := newTestManager(t, store, base, newMockWorktree(), newMockOC(true))
	cause := &pendingCleanupError{
		pending: pendingCleanup{
			sessionName: runtimeSessionName("t1"), cleanupTickets: []string{"tk-r"},
			reason: noticeReasonReapFailed, retryable: true,
		},
		noticeErr: errors.New("first write"),
		cause:     errors.New("serve not ready: health check timeout"),
	}
	// 无会话 → cleanup 跳过；回放应失败并进 last_error。
	m.runActivateFailureCompensation(context.Background(), "t1", cause)
	row := mustGet(inner, "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s", row.Status)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "replay notice") {
		t.Errorf("last_error should aggregate replay: %v", row.LastError)
	}
}

func TestReplay_UpdatedObservationNotCoveredByOldPending(t *testing.T) {
	// 轮换 kill_failed 写失败 pending + 外层 kill 得 snapshot_missing 写成功。
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	inner.mutTask("t1", func(tr *TaskRow) { tr.Status = StatusActivating })
	// 会话存在以便外层 kill。
	base := newMockProc()
	_ = base.NewSession(process.SessionSpec{Name: runtimeSessionName("t1"), Dir: "/tmp"})
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded,
	}
	m := newTestManager(t, inner, base, newMockWorktree(), newMockOC(true))
	cause := &pendingCleanupError{
		pending: pendingCleanup{
			sessionName: runtimeSessionName("t1"), cleanupTickets: []string{"tk-first"},
			reason: noticeReasonKillFailed, retryable: true,
		},
		noticeErr: errors.New("cas"),
		cause:     errors.New("serve not ready"),
	}
	m.runActivateFailureCompensation(context.Background(), "t1", cause)
	notices := residualNotices(mustGet(inner, "t1"))
	if len(notices) != 1 {
		t.Fatalf("notices=%+v", notices)
	}
	if notices[0].Data["reason"] != noticeReasonSnapshotMissing {
		t.Errorf("reason=%v want snapshot_missing_degraded", notices[0].Data["reason"])
	}
	tks := noticeTicketsOf(notices[0])
	found := false
	for _, tk := range tks {
		if tk == "tk-first" {
			found = true
		}
	}
	if !found {
		t.Errorf("must union first tickets: %v", tks)
	}
}

func TestCleanupNonActivatePath_NoPendingReplay(t *testing.T) {
	// 非 Activate 调用点：notice 写失败按现状首写聚合，无 pending 回放。
	inner := newMockStore()
	seedSuspendedTask(inner, "t1", "p1")
	store := &noticeAlwaysFailStore{mockStore: inner}
	base := newMockProc()
	_ = base.NewSession(process.SessionSpec{Name: runtimeSessionName("t1"), Dir: "/tmp"})
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"},
	}
	m := newTestManager(t, store, base, newMockWorktree(), newMockOC(true))
	err := m.cleanupActivationRuntime(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected notice error aggregated")
	}
	// 无回放：notice 仍空（写失败且无 replay）。
	if n := residualNotices(mustGet(inner, "t1")); len(n) != 0 {
		t.Fatalf("non-activate path must not replay: %+v", n)
	}
}

func TestCleanupCollect_HasSessionInfraFormsObservation(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.hasSessionErr = errors.New("tmux list fail")
	m := newTestManager(t, store, base, newMockWorktree(), newMockOC(true))
	err, obs := m.cleanupActivationRuntimeCollect(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected has-session error")
	}
	if len(obs) == 0 {
		t.Fatal("infra has-session must form observations")
	}
	for _, o := range obs {
		if o.reason != noticeReasonKillFailed || !o.retryable {
			t.Errorf("obs=%+v", o)
		}
	}
}

func TestClassifyKillResult_Table(t *testing.T) {
	cases := []struct {
		res    process.KillResult
		action string
		reason string
	}{
		{process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}, "none", ""},
		{process.KillResult{SessionKilled: true, Disposition: process.DispositionReapFailed}, "terminal", noticeReasonReapFailed},
		{process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotFailed}, "terminal", noticeReasonSnapshotFailed},
		{process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed}, "terminal", noticeReasonKillFailed},
		{process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}, "continue", noticeReasonSnapshotMissing},
		{process.KillResult{SessionKilled: true, Disposition: "x"}, "terminal", noticeReasonKillFailed},
		{process.KillResult{SessionKilled: false, Disposition: process.DispositionClean}, "terminal", noticeReasonKillFailed},
	}
	for i, tc := range cases {
		got := classifyKillResult(tc.res)
		if got.action != tc.action || got.reason != tc.reason {
			t.Errorf("#%d got action=%s reason=%s want %s/%s", i, got.action, got.reason, tc.action, tc.reason)
		}
	}
}

func TestPortRetry_AdjacentPortsDiffer_ViaSessionDied(t *testing.T) {
	// 相邻两次 NewSession 端口不同（A→B）。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 1, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	final, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err != nil {
		t.Fatal(err)
	}
	if final == 50000 {
		t.Fatal("ports must differ")
	}
	// 从 envValues 取两次端口：第二次覆盖第一次；从 newSession 顺序+CmdArgv。
	// mock 只保留最后 env。用 CmdArgv --port。
	// 检查 exclude 生效：final != 50000 且 env 更新。
	if env["OCDECK_SERVE_PORT"] != strconv.Itoa(final) {
		t.Errorf("env port=%s final=%d", env["OCDECK_SERVE_PORT"], final)
	}
}

func TestPortRetry_ActivateCtxCancelCompensationKills(t *testing.T) {
	// 完整 Activate：ctx 取消后本地零 kill，外层 compensation 清理 kill。
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	oc := &alwaysUnhealthyOC{}
	m := newServeRetryManager(t, store, base, oc, config.PortRange{Min: 50000, Max: 50005})
	// cancel 必须落在健康墙钟超时之前，否则会先走本地 kill 门禁。
	m.serveReadyTimeout = 2 * time.Second
	m.serveReadyPollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := m.Activate(ctx, "t1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v want canceled", err)
	}
	row := mustGet(store, "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended", row.Status)
	}
	// 外层 compensation 应 kill 已建 serve。
	if countKillServe(base, "t1") == 0 {
		t.Error("outer compensation should kill serve")
	}
	if !row.LastError.Valid {
		t.Error("last_error required")
	}
}

func TestPortRetry_NoticeWriteUsesDetachedBoundedCtx(t *testing.T) {
	// withResidualNoticeCtx：请求 ctx 已取消时，notice 写仍用 detached + 有界 deadline。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	nctx, ncancel := withResidualNoticeCtx(ctx)
	defer ncancel()
	if nctx.Err() != nil {
		t.Error("detached notice ctx must not be canceled")
	}
	dl, ok := nctx.Deadline()
	if !ok {
		t.Fatal("must have deadline")
	}
	remain := time.Until(dl)
	if remain > residualNoticeWriteTimeout || remain < residualNoticeWriteTimeout-time.Second {
		t.Errorf("deadline remain=%v want ~%v", remain, residualNoticeWriteTimeout)
	}
}

func TestPortRetry_NilEnvOnRotate_NoPanicPersistFail(t *testing.T) {
	// nil env 轮换写入 OCDECK_SERVE_PORT 不得 panic；persist 失败仍终态、不 NewSession 第二次。
	store := &envSnapshotFailStore{mockStore: newMockStore()}
	row := seedSuspendedTask(store.mockStore, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 1, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", nil, "")
	if err == nil {
		t.Fatal("expected persist failure")
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Errorf("err=%v", err)
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
	}
}

func TestPortRetry_ProbeCompensation_NoticeWriteFailThenReplay(t *testing.T) {
	// F3：Probe 失败 → 外层 compensation kill serve（reap_failed）→ cleanup 首次 recordResidualNotice
	// 的 8 次 CAS 全失败（persisted=false）→ fold 产生 pending → replay 成功 → tickets 落 notice。
	// probe 在 tui 建立前失败，故 cleanup 只枚举 serve（tui 不存在 → 跳过），
	// 整次 cleanup 只对 serve 调一次 recordResidualNotice（最多 8 CAS），failN=8 使其耗尽失败。
	inner := newMockStore()
	store := &noticeFailNStore{mockStore: inner, failN: 8}
	seedSuspendedTask(inner, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk-outer"},
	}
	oc := newMockOC(true)
	oc.probeErr = opencode.ErrServeNotReady
	m := withFastServeReady(newTestManager(t, store, base, newMockWorktree(), oc))
	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("expected probe error")
	}
	// F3 契约 1：Activate 返回错误保持 probe 分类，不含 cleanup/disposition/notice/replay 文案。
	if OpErrorCode(err) != codeProcessError {
		t.Errorf("code=%s want process_error", OpErrorCode(err))
	}
	msg := err.Error()
	if strings.Contains(msg, "disposition") || strings.Contains(msg, "replay notice") || strings.Contains(msg, "reap_failed") {
		t.Errorf("Activate err must stay probe-only: %s", msg)
	}
	if !strings.Contains(msg, "capability probe") {
		t.Errorf("want probe classification text: %s", msg)
	}
	// F3 契约 2：tickets 最终经 replay 落 notice。
	row := mustGet(inner, "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status=%s want suspended", row.Status)
	}
	found := false
	for _, n := range residualNotices(row) {
		for _, tk := range noticeTicketsOf(n) {
			if tk == "tk-outer" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tickets not persisted via replay: notices=%+v last_error=%v", residualNotices(row), row.LastError)
	}
}

// --- F1: ErrNoTmuxServer = session 不存在 ---

// noTmuxServerOnceProc：首次 HasSession 调用返回 ErrNoTmuxServer（模拟 server 消失），
// 之后委托 mockProc（会话存活）。
type noTmuxServerOnceProc struct {
	*mockProc
	checked atomic.Bool
}

func (p *noTmuxServerOnceProc) HasSession(name string) (bool, error) {
	if !p.checked.Swap(true) {
		return false, process.ErrNoTmuxServer
	}
	return p.mockProc.HasSession(name)
}

func TestPortRetry_NoTmuxServerIsSessionDied(t *testing.T) {
	// HasSession 返回 ErrNoTmuxServer → 视为进程死亡：零 KillSession、零 notice、异端口轮换。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	proc := &noTmuxServerOnceProc{mockProc: base}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	final, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err != nil {
		t.Fatalf("expected rotate success: %v", err)
	}
	if final == 50000 {
		t.Fatal("expected port rotate away from 50000")
	}
	if countKillServe(base, "t1") != 0 {
		t.Fatalf("KillSession=%d want 0 (ErrNoTmuxServer = died, no kill)", countKillServe(base, "t1"))
	}
	if n := residualNotices(mustGet(store, "t1")); len(n) != 0 {
		t.Fatalf("notice should be empty (died path): %+v", n)
	}
	if countNewServe(base, "t1") != 2 {
		t.Fatalf("NewSession=%d want 2", countNewServe(base, "t1"))
	}
}

func TestPortRetry_HasSessionInfraErrorNotDied(t *testing.T) {
	// HasSession 返回非 ErrNoTmuxServer 的 infra 错误 → 不得伪装死亡；走墙钟超时→kill 门禁。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.hasSessionErr = errors.New("tmux infra weird")
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"},
	}
	m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected terminal")
	}
	// infra 错误非死亡 → 走 kill 门禁 → 记 reap_failed notice。
	notices := residualNotices(mustGet(store, "t1"))
	if len(notices) != 1 || notices[0].Data["reason"] != noticeReasonReapFailed {
		t.Fatalf("notices=%+v want reap_failed", notices)
	}
}

// --- F2: 终态错误聚合 wait + disposition ---

func TestPortRetry_TerminalErrorAggregatesWaitKillNotice(t *testing.T) {
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	base.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"},
	}
	m := newServeRetryManager(t, store, base, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "serve not ready") {
		t.Errorf("missing wait context: %s", msg)
	}
	if !strings.Contains(msg, "disposition") {
		t.Errorf("missing disposition: %s", msg)
	}
	if msg == "health check timeout" || msg == "serve not ready: health check timeout" {
		t.Errorf("must aggregate more than health timeout: %s", msg)
	}
}

func TestPortRetry_LastAttemptAggregatesDispositionFromEarlierRotate(t *testing.T) {
	// 每轮 snapshot_missing_degraded 记 notice + continue；末次终态应聚合 wait + disposition。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	// 每轮 kill → snapshot_missing_degraded（continue），会话存活（mockProc 不删），持续到末次。
	missingProc := &killSeqProc{mockProc: base, seq: []killSeqStep{
		{res: process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}},
		{res: process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}},
		{res: process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}},
	}}
	m := newServeRetryManager(t, store, missingProc, &alwaysUnhealthyOC{}, config.PortRange{Min: 50000, Max: 50005})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected terminal after retries")
	}
	msg := err.Error()
	if !strings.Contains(msg, "serve not ready") {
		t.Errorf("missing wait context: %s", msg)
	}
	if !strings.Contains(msg, "disposition") {
		t.Errorf("last attempt must aggregate earlier disposition: %s", msg)
	}
}

func TestPortRetry_AllocateFailAggregatesWaitContext(t *testing.T) {
	// 进程死亡 + 单端口范围 exclude → exhausted；终态错误必须含 wait（session died）上下文，
	// 且 aerr 经 %w 保留在 error chain 中（errors.Is 可达底层 exhausted 错误）。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 99, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50000})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected exhausted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exhausted") && !strings.Contains(msg, "allocate") {
		t.Errorf("must include allocate error: %s", msg)
	}
	// wait 上下文是 session died（进程死亡路径），非仅字面 "serve not ready"。
	if !strings.Contains(msg, "died") && !strings.Contains(msg, "health check timeout") {
		t.Errorf("must aggregate wait cause (died/timeout): %s", msg)
	}
	// error chain：底层 allocate 错误必须可 errors.Is（非仅字符串）。
	// allocatePort 在范围耗尽时返回 fmt.Errorf("serve port range %d-%d exhausted", ...)。
	if !strings.Contains(msg, "serve port range") {
		t.Errorf("must retain allocate error text in chain: %s", msg)
	}
	// 通过 sentinel 注入验证 %w 保留：见 TestPortRetry_AllocateFailPreservesErrorChain。
}

// errAllocSentinel 用于断言 wrapServeWaitCause 以 %w 保留底层 aerr。
var errAllocSentinel = errors.New("alloc sentinel")

// TestPortRetry_AllocateFailPreservesErrorChain 直接测 wrapServeWaitCause + allocate 路径的 %w 语义。
func TestPortRetry_AllocateFailPreservesErrorChain(t *testing.T) {
	// helper：wrapServeWaitCause(wait, errAllocSentinel) 可 errors.Is 到 sentinel，并保留 wait/disposition 文本。
	wait := fmt.Errorf("%w: ocdeck-t1-serve", errServeSessionDied)
	wrapped := wrapServeWaitCause(wait, errAllocSentinel, "disposition: snapshot_missing_degraded")
	if !errors.Is(wrapped, errAllocSentinel) {
		t.Fatalf("wrapServeWaitCause must preserve finalErr via %%w: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "died") {
		t.Errorf("must keep wait text: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "snapshot_missing_degraded") {
		t.Errorf("must keep disposition parts: %v", wrapped)
	}

	// 集成：allocate exhausted 经 newOpErr 后错误文本含 range exhausted（底层 aerr 文本）。
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 99, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50000})
	env, _ := m.mergeEnvSnapshot(context.Background(), row, 50000)
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", env, "")
	if err == nil {
		t.Fatal("expected exhausted")
	}
	if !strings.Contains(err.Error(), "50000-50000 exhausted") {
		t.Errorf("allocate aerr must remain in error chain text: %v", err)
	}
	if OpErrorCode(err) != codeConflict {
		t.Errorf("code=%s want conflict", OpErrorCode(err))
	}
}

// errPersistSentinel 用于断言 persist 路径 %w 保留。
var errPersistSentinel = errors.New("persist sentinel")

type envSnapshotSentinelFailStore struct {
	*mockStore
}

func (s *envSnapshotSentinelFailStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if envSnapshot.Valid {
		return application.MutationResult{}, errPersistSentinel
	}
	return s.mockStore.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

func TestPortRetry_PersistFailAggregatesWaitContext(t *testing.T) {
	// 进程死亡换端口后 persist 失败 → 终态错误含 wait（session died）上下文，
	// 且 perr 经 %w 保留（errors.Is 可达 errPersistSentinel）。
	store := &envSnapshotSentinelFailStore{mockStore: newMockStore()}
	row := seedSuspendedTask(store.mockStore, "t1", "p1")
	base := newMockProc()
	var nc atomic.Int32
	proc := &selectiveDieProc{mockProc: base, dieUntil: 1, newCount: &nc}
	m := newServeRetryManager(t, store, proc, newMockOC(true), config.PortRange{Min: 50000, Max: 50010})
	_, err := m.startServeWithPortRetry(context.Background(), row, runtimeSessionName("t1"), 50000, "pw", nil, "")
	if err == nil {
		t.Fatal("expected persist failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "serve not ready") {
		t.Errorf("must aggregate wait context: %s", msg)
	}
	if !errors.Is(err, errPersistSentinel) {
		t.Fatalf("persist perr must remain reachable via errors.Is: %v", err)
	}
	if OpErrorCode(err) != codeInternal {
		t.Errorf("code=%s want internal", OpErrorCode(err))
	}
	if countNewServe(base, "t1") != 1 {
		t.Fatalf("NewSession=%d want 1", countNewServe(base, "t1"))
	}
}
