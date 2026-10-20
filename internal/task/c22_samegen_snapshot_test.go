package task

// c22_samegen_snapshot_test.go 锚定 C22 BLOCKING 修复（第四轮复审）：
//   - 修复点 A：supervisor BuildServeEnv seam（task 层 spi_seams.go）——
//     旧实现先 layerEnvSnapshot 重算再叠加 DB 快照 → 恢复 attempt 会把快照
//     持久化之后新增的全局/项目/任务级 env 配置混入本代 runtime 进程（代际
//     mid-flight 漂移，违反 orchestration spec :145「恢复 attempt 用已持久化
//     同代快照」）。修复 = 只消费已持久化快照（激活入口 mergeEnvSnapshot /
//     恢复入口 runRecoveryAttempt 都先持久化，快照是 attempt 层唯一 env 事实源；
//     新配置生效点 = 下一次快照持久化）。
//   - 修复点 B（前序已实施，本测试覆盖回归）：supervisor 全部分配路径
//     （主路径/后备 last_port/轮换）统一「PersistServePort 成功后才 NewSession」；
//     快照损坏 = fail-closed（supervisor.env 错误，零建程）。
//
// mutation 证据（破坏 → 红 → 还原）见各测试注释。

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
)

// c22EnvFixture 驱动真实 supervisor Acquire（激活形态——运行时 supervisor 路径
// 与恢复 Resume 共用 startServeWithRetry→BuildServeEnv seam；激活入口已先经
// mergeEnvSnapshot 持久化快照，seam 语义同代一致）。
type c22EnvFixture struct {
	t     *testing.T
	srv   *fakeOCServer
	store *mockStore
	proc  *mockProc
	m     *Manager
}

func newC22EnvFixture(t *testing.T) *c22EnvFixture {
	t.Helper()
	srv := newFakeOCServer(t)
	srv.expectDirectory("/data/worktrees/p1/t1")
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	proc := newMockProc()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: srv.port + 1, Max: srv.port + 3},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(opts))
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	m.runtimeSupervisor = opencode.NewSPIProviderWithDeps(deps).Supervisor()
	return &c22EnvFixture{t: t, srv: srv, store: store, proc: proc, m: m}
}

// newConfLayerCatcherSideFx 记录 supervisor 在建程时刻观察到的 env（BuildServeEnv
// seam 返回值经 PersistServePort 原样透传——直接在 seam 层捕获）。
type c22EnvCatcher struct {
	mockSupSideFx
	mu   chan struct{} // 序列化观察通道（容量 1 携带最新 env）
	envs chan map[string]string
}

func newC22EnvCatcher(m *Manager) *c22EnvCatcher {
	return &c22EnvCatcher{mockSupSideFx: mockSupSideFx{m: m}, envs: make(chan map[string]string, 8)}
}

func (c *c22EnvCatcher) BuildServeEnv(ctx context.Context, taskID string) (map[string]string, error) {
	env, err := c.mockSupSideFx.BuildServeEnv(ctx, taskID)
	if err == nil {
		select {
		case c.envs <- env:
		default:
		}
	}
	return env, err
}

// TestC22_NewConfigKeyNotMixedIntoSameGenRuntime（修复点 A）：快照持久化后新增
// 项目级 env 配置，supervisor 建程 env MUST NOT 包含新键（只消费同代快照）。
//
// mutation 证据（破坏 → 红 → 还原）：BuildServeEnv 恢复旧实现（layerEnvSnapshot
// 重算 + 叠加快照）→ 新键 C22_NEW_AFTER_PERSIST 混入建程 env → 本测试红
// 「new config key leaked into same-generation runtime env」；还原后绿。
func TestC22_NewConfigKeyNotMixedIntoSameGenRuntime(t *testing.T) {
	fx := newC22EnvFixture(t)
	// 激活入口持久化同代快照（端口 = fake server 真端口，主路径直连）。
	if _, err := fx.m.mergeEnvSnapshot(context.Background(),
		mustTaskRow(t, fx.store, "t1"), fx.srv.port); err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	// 快照持久化后新增配置键（用户在代际 mid-flight 加配置——恢复 attempt 的
	// env 来源是快照，不是重算分层）。
	fx.store.mu.Lock()
	fx.store.projEnvVars = map[string][]EnvVarRow{
		"p1": {{Key: "C22_NEW_AFTER_PERSIST", Value: "leaked-if-regressed"}},
	}
	fx.store.mu.Unlock()

	rt := fx.m.newRuntime("t1")
	fx.m.setRuntime("t1", rt)

	// 直接驱动真实 supervisor Acquire（绕过 Activate 编排——seam 层验证；
	// 这正是 BuildServeEnv 的生产消费形态：supervisor 建程前读 env）。
	sup := fx.m.runtimeSupervisor
	binding, err := sup.Acquire(context.Background(), appagent.TaskRef{TaskID: "t1",
		Directory: "/data/worktrees/p1/t1"})
	if err != nil {
		t.Fatalf("supervisor Acquire: %v", err)
	}
	if binding.TerminalRef == "" {
		t.Fatal("binding terminal ref empty (acquire unexpectedly no-op)")
	}
	// 观察建程 env（c22EnvCatcher 捕获 BuildServeEnv 返回值）。
	// supervisor 内部直接 NewSession→env；这里经 proc.envValues 快照核对。
	env := fx.proc.envValues[runtimeSessionName("t1")]
	if env == nil {
		t.Fatal("runtime session env not recorded")
	}
	if v, ok := env["C22_NEW_AFTER_PERSIST"]; ok {
		t.Fatalf("new config key leaked into same-generation runtime env: %s=%s（恢复/激活 attempt 必须只消费同代快照）", "C22_NEW_AFTER_PERSIST", v)
	}
	if env["OCDECK_SERVE_PORT"] != strconv.Itoa(fx.srv.port) {
		t.Fatalf("serve port drifted: %s, want %d（同代 ownership 端口必经快照）",
			env["OCDECK_SERVE_PORT"], fx.srv.port)
	}
	// 快照本体含端口（同代事实源）。
	row, _ := fx.store.GetTask(context.Background(), "t1")
	snap, err := fx.m.loadEnvSnapshot(row)
	if err != nil {
		t.Fatalf("loadEnvSnapshot after acquire: %v", err)
	}
	if snap["OCDECK_SERVE_PORT"] != strconv.Itoa(fx.srv.port) {
		t.Fatalf("snapshot serve port = %s, want %d", snap["OCDECK_SERVE_PORT"], fx.srv.port)
	}
	_ = binding
}

// TestC22_CorruptSnapshotFailClosed（修复点 B 回归）：快照损坏（坏 JSON）→
// supervisor.env fail-closed → 零 NewSession、Acquire 返回错误。
func TestC22_CorruptSnapshotFailClosed(t *testing.T) {
	fx := newC22EnvFixture(t)
	// 注入坏快照（JSON 非法）。
	fx.store.mutTask("t1", func(r *TaskRow) {
		r.EnvSnapshot = sql.NullString{String: `{"vars": `, Valid: true} // 截断 JSON
	})
	rt := fx.m.newRuntime("t1")
	fx.m.setRuntime("t1", rt)
	sup := fx.m.runtimeSupervisor
	_, err := sup.Acquire(context.Background(), appagent.TaskRef{TaskID: "t1",
		Directory: "/data/worktrees/p1/t1"})
	if err == nil {
		t.Fatal("corrupt env snapshot must fail closed (supervisor.env), got nil error")
	}
	if !strings.Contains(err.Error(), "supervisor.env") && !strings.Contains(err.Error(), "env snapshot") {
		t.Fatalf("error = %v, want supervisor.env/env snapshot fail-closed shape", err)
	}
	if n := len(fx.proc.newSessionNamesSnapshot()); n != 0 {
		t.Fatalf("NewSession calls = %d, want 0（fail-closed：坏快照零建程）", n)
	}
}

// TestC22_PersistFailureBlocksSpawnBaseline（修复点 B 回归基线）：快照在位且
// persist 正常时 Acquire 成功建程（对照面；persist 失败阻塞建程由
// TestSupervisor_PersistFailureBlocksSpawn 双路径覆盖，此处锁定 seam 正常路径
// 不被同代快照语义改变）。
func TestC22_PersistFailureBlocksSpawnBaseline(t *testing.T) {
	fx := newC22EnvFixture(t)
	if _, err := fx.m.mergeEnvSnapshot(context.Background(),
		mustTaskRow(t, fx.store, "t1"), fx.srv.port); err != nil {
		t.Fatalf("mergeEnvSnapshot: %v", err)
	}
	rt := fx.m.newRuntime("t1")
	fx.m.setRuntime("t1", rt)
	sup := fx.m.runtimeSupervisor
	_, err := sup.Acquire(context.Background(), appagent.TaskRef{TaskID: "t1",
		Directory: "/data/worktrees/p1/t1"})
	if err != nil {
		t.Fatalf("acquire (healthy snapshot): %v", err)
	}
	if len(fx.proc.newSessionNamesSnapshot()) == 0 {
		t.Fatal("acquire must create runtime session")
	}
}

// mustTaskRow 读取任务行（不存在则失败）。
func mustTaskRow(t *testing.T, store *mockStore, id string) TaskRow {
	t.Helper()
	row, err := store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return row
}
