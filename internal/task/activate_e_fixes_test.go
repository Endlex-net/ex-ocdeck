package task

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// recordingProc 包装 mockProc，按调用顺序记录 NewSession（含端口）/KillSession 事件，
// 供 E1 断言 "kill 旧 serve → 以新端口重建 serve" 的顺序。
type recordingProc struct {
	*mockProc
	mu    sync.Mutex
	order []string // 形如 "new:ocdeck-t1-serve:50001" / "kill:ocdeck-t1-serve"
}

func newRecordingProc() *recordingProc {
	return &recordingProc{mockProc: newMockProc()}
}

func (r *recordingProc) NewSession(spec process.SessionSpec) error {
	err := r.mockProc.NewSession(spec)
	if err == nil {
		r.mu.Lock()
		r.order = append(r.order, "new:"+spec.Name+":"+spec.Env["OCDECK_SERVE_PORT"])
		r.mu.Unlock()
	}
	return err
}

func (r *recordingProc) KillSession(name string) (process.KillResult, error) {
	res, err := r.mockProc.KillSession(name)
	r.mu.Lock()
	r.order = append(r.order, "kill:"+name)
	r.mu.Unlock()
	return res, err
}

func (r *recordingProc) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// portHealthOC 携带自身端口，在 failPort 上 Health 失败，其他端口成功。
type portHealthOC struct {
	port     int
	failPort int
	probeErr error
}

func (c *portHealthOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	if c.port == c.failPort {
		return opencode.HealthResponse{}, opencode.ErrServeNotReady
	}
	return opencode.HealthResponse{Healthy: true, Version: opencode.ContractBaseline}, nil
}

func (c *portHealthOC) Probe(ctx context.Context) (string, error) {
	if c.probeErr != nil {
		return "", c.probeErr
	}
	return opencode.ContractBaseline, nil
}

func (c *portHealthOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return nil, nil
}

func (c *portHealthOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return opencode.Session{}, opencode.ErrSessionNotFound
}

func (c *portHealthOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return opencode.Session{ID: "sess-new", Time: opencode.SessionTime{Created: 1, Updated: 1}}, nil
}

func (c *portHealthOC) DeleteSession(ctx context.Context, dir, id string) error { return nil }

func (c *portHealthOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return nil, nil
}

func (c *portHealthOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	// Activate 的 startSSE 需要 OnReady：在 readyOC wrapper 中触发，这里仅阻塞。
	if onReconnect != nil {
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *portHealthOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}
func (c *portHealthOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return nil, opencode.ErrCapabilityUnsupported
}

// newPortRetryManager 构造一个 Manager，其 ocFactory 在 failPort 上 Health 失败、其他端口成功。
// 用于 E1 端口重试测试。probeErr 传入 Probe 返回错误（默认 nil=成功）。
func newPortRetryManager(t *testing.T, store TaskStore, proc ProcessBackend, failPort int, probeErr error) *Manager {
	t.Helper()
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownPersist,
	}
	factory := func(port int, password string, opts opencode.Options) OCClient {
		oc := &portHealthOC{port: port, failPort: failPort, probeErr: probeErr}
		// startSSE 依赖 OnReady 触发 readyCh，包裹以在 SubscribeEvents 时调用 onReady。
		return &readyOCWrap{inner: oc, onReady: opts.OnReady}
	}
	return New(Options{
		Cfg: cfg, Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: factory,
	})
}

// readyOCWrap 复用 readyOC 的 OnReady 触发语义，但 inner 为 portHealthOC。
type readyOCWrap struct {
	inner   OCClient
	onReady func()
}

func (c *readyOCWrap) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *readyOCWrap) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *readyOCWrap) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *readyOCWrap) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *readyOCWrap) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *readyOCWrap) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *readyOCWrap) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *readyOCWrap) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	if c.onReady != nil {
		c.onReady()
	}
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *readyOCWrap) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	return c.inner.ListPermissions(ctx, dir)
}
func (c *readyOCWrap) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return c.inner.ListQuestions(ctx, dir)
}

// --- E1: 端口重写三处一致 ---

// TestStartServeWithPortRetry_PortRewriteConsistency 验证换端口重试时三处 OCDECK_SERVE_PORT
// 同步更新（design.md §3 E1，§2 line 68）：
//   - 内存 env map（传给 startTUI）
//   - 持久化 tasks.env_snapshot
//   - 新建 serve 会话 env（serveEnv）
//
// 且旧 serve 会话 MUST 在新 serve 会话创建之前被 kill（不允许 serve 旧端口 / TUI 新端口）。
func TestStartServeWithPortRetry_PortRewriteConsistency(t *testing.T) {
	store := newMockStore()
	row := seedSuspendedTask(store, "t1", "p1")

	// 占用初始端口 P，迫使重试时的 allocatePort(P) 旋转到另一个空闲端口 Q。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	initialPort := ln.Addr().(*net.TCPAddr).Port

	proc := newRecordingProc()
	// ocFactory：在 initialPort 上 Health 失败（serve 未就绪），其他端口成功。
	m := newPortRetryManager(t, store, proc, initialPort, nil)

	// 初始 env 快照（含初始端口 P），模拟 activateRun 第 206 行。
	env, err := m.mergeEnvSnapshot(context.Background(), row, initialPort)
	if err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	if got := env["OCDECK_SERVE_PORT"]; got != strconv.Itoa(initialPort) {
		t.Fatalf("初始 env 端口=%s want %d", got, initialPort)
	}

	serveName := serveSessionName("t1")
	finalPort, sErr := m.startServeWithPortRetry(context.Background(), row, serveName, initialPort, "pw", env)
	if sErr != nil {
		t.Fatalf("startServeWithPortRetry: %v", sErr)
	}
	if finalPort == initialPort {
		t.Fatalf("期望换端口，但 finalPort=%d == initial=%d", finalPort, initialPort)
	}

	// 断言 1：内存 env map 的 OCDECK_SERVE_PORT 已更新为 finalPort。
	if got := env["OCDECK_SERVE_PORT"]; got != strconv.Itoa(finalPort) {
		t.Errorf("内存 env OCDECK_SERVE_PORT=%s want %d (E1 内存同步)", got, finalPort)
	}

	// 断言 2：持久化 tasks.env_snapshot 的 OCDECK_SERVE_PORT 已更新为 finalPort。
	row2, _ := store.GetTask(context.Background(), "t1")
	if !row2.EnvSnapshot.Valid {
		t.Fatal("env snapshot should be persisted")
	}
	snap, perr := m.loadEnvSnapshot(row2)
	if perr != nil {
		t.Fatalf("loadEnvSnapshot: %v", perr)
	}
	if got := snap["OCDECK_SERVE_PORT"]; got != strconv.Itoa(finalPort) {
		t.Errorf("持久化快照 OCDECK_SERVE_PORT=%s want %d (E1 持久化同步)", got, finalPort)
	}

	// 断言 3：新建 serve 会话 env 的 OCDECK_SERVE_PORT 为 finalPort。
	serveEnv, _ := proc.envValues[serveName]
	if got := serveEnv["OCDECK_SERVE_PORT"]; got != strconv.Itoa(finalPort) {
		t.Errorf("serve 会话 env OCDECK_SERVE_PORT=%s want %d (E1 serve 同步)", got, finalPort)
	}

	// 断言 4：旧 serve 会话 MUST 在新 serve 会话创建之前被 kill（顺序：new(P) → kill → new(Q)）。
	order := proc.snapshot()
	if len(order) != 3 {
		t.Fatalf("事件顺序长度=%d want 3, order=%v", len(order), order)
	}
	if order[0] != "new:"+serveName+":"+strconv.Itoa(initialPort) {
		t.Errorf("事件[0]=%q want new 初始端口", order[0])
	}
	if order[1] != "kill:"+serveName {
		t.Errorf("事件[1]=%q want kill 旧 serve", order[1])
	}
	if order[2] != "new:"+serveName+":"+strconv.Itoa(finalPort) {
		t.Errorf("事件[2]=%q want new 新端口", order[2])
	}
}

// TestStartServeWithPortRetry_TUIEnvUsesFinalPort 验证端口变更后 startTUI 用的是最终端口的 env
// （E1 第 3 处：新建 TUI 会话 env）。端到端通过 Activate 触发。
func TestStartServeWithPortRetry_TUIEnvUsesFinalPort(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")

	// 占用初始端口 firstPort，迫使 Activate 中 startServeWithPortRetry 重试旋转到另一端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	firstPort := ln.Addr().(*net.TCPAddr).Port
	store.mutTask("t1", func(tr *TaskRow) {
		tr.LastPort = sql.NullInt64{Int64: int64(firstPort), Valid: true}
	})

	proc := newRecordingProc()
	// ocFactory：在 firstPort 上 Health 失败，其他端口成功。
	m := newPortRetryManager(t, store, proc, firstPort, nil)

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	row, _ := store.GetTask(context.Background(), "t1")
	if row.LastPort.Int64 == int64(firstPort) {
		t.Fatalf("last_port 应已写回为新端口，仍是初始 %d", firstPort)
	}
	finalPort := int(row.LastPort.Int64)

	// 断言 TUI 会话 env 的 OCDECK_SERVE_PORT == finalPort（E1 第 3 处）。
	tuiEnv, ok := proc.envValues[tuiSessionName("t1")]
	if !ok {
		t.Fatal("TUI 会话未创建")
	}
	if got := tuiEnv["OCDECK_SERVE_PORT"]; got != strconv.Itoa(finalPort) {
		t.Errorf("TUI env OCDECK_SERVE_PORT=%s want %d (E1 TUI env 同步)", got, finalPort)
	}
	// serve 会话 env 也应是 finalPort。
	serveEnv := proc.envValues[serveSessionName("t1")]
	if got := serveEnv["OCDECK_SERVE_PORT"]; got != strconv.Itoa(finalPort) {
		t.Errorf("serve env OCDECK_SERVE_PORT=%s want %d", got, finalPort)
	}
}

// --- E2: Probe 错误三类映射 ---

// TestProbe_Classification 验证 Probe 错误按 design.md §11/§21 分类映射（非全部 oc_incompatible）：
//   - ErrServeNotReady → process_error
//   - ErrCapabilityMismatch → oc_incompatible
//   - ErrUnauthorized → internal
func TestProbe_Classification(t *testing.T) {
	cases := []struct {
		name     string
		probeErr error
		wantCode string
	}{
		{"ServeNotReadyMapsProcessError", opencode.ErrServeNotReady, codeProcessError},
		{"CapabilityMismatchMapsOCIncompatible", opencode.ErrCapabilityMismatch, codeOCIncompatible},
		{"UnauthorizedMapsInternal", opencode.ErrUnauthorized, codeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			seedSuspendedTask(store, "t1", "p1")
			oc := newMockOC(true) // health 就绪
			oc.probeErr = tc.probeErr
			m := newTestManager(t, store, newMockProc(), newMockWorktree(), oc)

			err := m.Activate(context.Background(), "t1")
			if err == nil {
				t.Fatal("期望 Probe 失败导致 Activate 失败")
			}
			if got := OpErrorCode(err); got != tc.wantCode {
				t.Errorf("code=%s want %s, err=%v", got, tc.wantCode, err)
			}
			// 失败应回 suspended + 清 env snapshot。
			row, _ := store.GetTask(context.Background(), "t1")
			if row.Status != StatusSuspended {
				t.Errorf("status=%s want suspended", row.Status)
			}
			if row.EnvSnapshot.Valid {
				t.Error("env snapshot 应在 Activate 失败时清空")
			}
		})
	}
}

// TestProbeErrToOpCode_UnknownDefaultsProcessError 验证未知 Probe 错误保守映射为 process_error（可重试）。
func TestProbeErrToOpCode_UnknownDefaultsProcessError(t *testing.T) {
	unknown := errors.New("some transient network glitch")
	code, ferr := probeErrToOpCode(unknown)
	if code != codeProcessError {
		t.Errorf("code=%s want %s", code, codeProcessError)
	}
	if !errors.Is(ferr, unknown) {
		t.Errorf("返回错误应 wrap 原始错误，got=%v", ferr)
	}
}

// --- E3: 健康轮询前判进程死亡 ---

// TestWaitServeReadyOrDead_DeadProcessFastFails 验证 serve 会话已死时 waitServeReadyOrDead
// 立即返回错误，不等满 10s 超时（design.md §3/§19 E3）。
func TestWaitServeReadyOrDead_DeadProcessFastFails(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	// 不创建 serve 会话 → HasSession 返回 false（进程已死）。
	oc := newMockOC(false) // health 也不就绪
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	serveName := serveSessionName("t1")
	start := time.Now()
	err := m.waitServeReadyOrDead(context.Background(), oc, serveName)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("期望进程死亡返回错误，got nil")
	}
	if !contains(err.Error(), "died before ready") {
		t.Errorf("错误应明确报进程死亡，got=%v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("进程死亡应快速失败（<2s），实际耗时=%v", elapsed)
	}
}

// TestWaitServeReadyOrDead_AliveProcessWaitsForHealth 验证进程存活但 health 未就绪时
// waitServeReadyOrDead 会正常轮询（不因存活误判为死亡），最终超时。
func TestWaitServeReadyOrDead_AliveProcessWaitsForHealth(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	proc := newMockProc()
	proc.sessions[serveSessionName("t1")] = true // 进程存活
	oc := newMockOC(false)                       // health 永不就绪
	m := newTestManager(t, store, proc, newMockWorktree(), oc)

	err := m.waitServeReadyOrDead(context.Background(), oc, serveSessionName("t1"))
	if err == nil {
		t.Fatal("期望 health 超时错误")
	}
	// 进程存活应走 health 超时路径，而非 "died before ready" 路径。
	if msg := err.Error(); contains(msg, "died before ready") {
		t.Errorf("进程存活不应报 died before ready，err=%v", err)
	}
}
