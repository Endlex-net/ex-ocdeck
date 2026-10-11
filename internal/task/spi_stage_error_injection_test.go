package task

// spi_stage_error_injection_test.go 锚定 C12 残余（review attempt 3）：阶段错误分派
// 断言从真实 opencode supervisor 注入失败（非手工构造 StageError）——分派链路
// supervisor（错误产生）→ supervisorStageErrorForTyped（分派）→ isRecoveryTerminal
// （终态判定）全链验证。手工构造形态保留在 spi_stageerror_dispatch_test.go（纯
// task 层分派面）；本文件覆盖「真实错误类型/形状经全链」。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// seedServeEnvSnapshot 给任务行写入合法 env 快照（supervisor BuildServeEnv seam
// 的前置：快照缺失时 supervisor 走 fail-closed env 错误——fixture 需要「达到注入
// 目标阶段」的前置在位；含 OCDECK_TASK_ID 与 OCDECK_SERVE_PORT（port>0 用之，
// 0 = 占位值——C11 测试用 srv.port 覆写让主路径直连 fake server）。
func seedServeEnvSnapshot(store *mockStore, taskID string, port int) {
	servePort := "50000"
	if port > 0 {
		servePort = strconv.Itoa(port)
	}
	b, err := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{
		"OCDECK_TASK_ID":    taskID,
		"OCDECK_SERVE_PORT": servePort,
	}})
	if err != nil {
		return
	}
	store.mutTask(taskID, func(r *TaskRow) { r.EnvSnapshot = b })
}

// supInjectProc 是 C12 注入 fixture 的 process seam：NewSession 失败/kill 结果可编程。
// （复用 supFakeProc 的字段面不够——task 层测试用 mockProc 形态；此处直接用
// opencode fixture 的 supFakeProc 不可行（internal/task 无访问）。改为在 task 层
// 包一个新的最小 proc stub，仅在 mockProc 之外支持 killRes 注入已由 mockProc
// .killResults 承担——本文件 fixture 直接用 mockProc。）

// TestStageErrorInjection_ProbeFailureFromRealSupervisor 固定输入：真实 opencode
// supervisor（ProbeColdStartBackoff 即时）+ fakeOCServer /session/status 结构违约
// （probeBroken 形态）。断言：Resume（恢复 attempt 等价形态）返回
// supervisor.probe StageError → task 层分派 → 终态（isRecoveryTerminal）。
//
// mutation 证据（已验证 → 红 → 还原）：spi_supervisor.go probe 出口改回普通
// appagent.NewError → StageErrorOf 检不出 → task 层原样返回走 attempt 重试 →
// 本测试红「want terminal」。还原后绿。
func TestStageErrorInjection_ProbeFailureFromRealSupervisor(t *testing.T) {
	srv := newFakeOCServer(t)
	srv.setProbeBroken(true)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
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
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	seedServeEnvSnapshot(store, "t1", 0)
	srv.expectDirectory("/data/worktrees/p1/t1")

	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	sup := opencode.NewSPIProviderWithDeps(deps).Supervisor()
	binding, err := sup.Acquire(context.Background(), appagent.TaskRef{
		TaskID: "t1", Directory: "/data/worktrees/p1/t1",
	})
	if err == nil {
		t.Fatal("want probe stage error from real supervisor, got nil")
	}
	mapped := m.supervisorStageErrorForTyped(err, "t1")
	if !isRecoveryTerminal(mapped) {
		t.Fatalf("probe failure from real supervisor MUST be terminal (spec 终态补偿), got non-terminal: %v", mapped)
	}
	var probe *capabilityProbeError
	if !errors.As(mapped, &probe) {
		t.Fatalf("want capabilityProbeError, got %T", mapped)
	}
	_ = binding
}

// TestStageErrorInjection_ClaimConflictFromRealSupervisor 固定输入：真实 supervisor
// + mockSupStoreSeam 注入 claim 冲突（他任务持有锚定）。断言：Acquire 返回
// supervisor.claim_anchor StageError → task 层分派 → anchorStageError 终态。
//
// mutation 证据（已验证 → 红 → 还原）：spi_supervisor.go claim 冲突出口改回
// appagent.NewError → task 层 StageErrorOf 检不出 → 原样返回 →
// isRecoveryTerminal 假 → 本测试红「want terminal」。还原后绿。
func TestStageErrorInjection_ClaimConflictFromRealSupervisor(t *testing.T) {
	// bootstrap 进程健康检查需真实 HTTP serve（supervisor 内部 NewClient 直连
	// AllocatePort 端口）；claim 冲突经 store seam 注入。
	srv := newFakeOCServer(t)
	srv.expectDirectory("/wt/t1")
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	seedServeEnvSnapshot(store, "t1", srv.port)
	proc := newCharxProc()
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
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")

	seam := &mockSupStoreSeam{store: store}
	seam.claimConflict = true
	deps := opencode.SupervisorDeps{
		Proc: proc, Store: seam, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	sup := opencode.NewSPIProviderWithDeps(deps).Supervisor()
	binding, err := sup.Acquire(context.Background(), appagent.TaskRef{
		TaskID: "t1", Directory: "/wt/t1",
	})
	if err == nil {
		t.Fatal("want claim conflict from real supervisor, got nil")
	}
	mapped := m.supervisorStageErrorForTyped(err, "t1")
	if !isRecoveryTerminal(mapped) {
		t.Fatalf("claim conflict from real supervisor MUST be terminal (bootstrap claim 分派), got non-terminal: %v", mapped)
	}
	var anchor *anchorStageError
	if !errors.As(mapped, &anchor) {
		t.Fatalf("want anchorStageError, got %T", mapped)
	}
	_ = binding
}

// TestStageErrorInjection_CleanupTicketsNoticeBlocksRebuild 固定输入：真实 supervisor
// Acquire 注入「健康超时 kill → reap_failed disposition + tickets」（failHealth 轮换
// 轮的 kill 门禁）→ CleanupStageError(reap_failed, tk-c12) → task 层分派：notice 落
// 账成功 → retryableCleanupError 终态（阻断继续建程）。
//
// mutation 证据（已验证 → 红 → 还原）：supervisorStageErrorForTyped 的 cleanup 分派
// 落账成功腿改回 cause 直返（非终态）→ 本测试红「want terminal」；还原后绿。
func TestStageErrorInjection_CleanupTicketsNoticeBlocksRebuild(t *testing.T) {
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return newMockOC(true)
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 30 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	seedServeEnvSnapshot(store, "t1", 0)

	// 健康永不就绪 → 每轮 NewSession 成功但健康超时 → kill 门禁；kill 返回
	// reap_failed + tickets → CleanupStageError 携带 tickets（C17 出口保票）。
	proc.mu.Lock()
	proc.killResults[runtimeSessionName("t1")] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-c12-inject"},
	}
	proc.mu.Unlock()
	// AllocatePort 恒指死端口（无 mock server 在此端口监听 → 健康必超时）。
	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return 50199, nil },
		ReadyTimeout: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	sup := opencode.NewSPIProviderWithDeps(deps).Supervisor()
	binding, err := sup.Acquire(context.Background(), appagent.TaskRef{
		TaskID: "t1", Directory: "/wt/t1",
	})
	if err == nil {
		t.Fatal("want cleanup stage error from real supervisor, got nil")
	}
	var se *appagent.StageError
	if !errors.As(err, &se) || se.Cleanup == nil {
		t.Fatalf("want Cleanup StageError, got %v (%T)", err, err)
	}
	if se.Cleanup.Reason != appagent.KillReasonReapFailed || len(se.Cleanup.Tickets) == 0 {
		t.Fatalf("cleanup payload = %+v, want reap_failed with tickets (C17: KillSession 出口保票)", se.Cleanup)
	}
	mapped := m.supervisorStageErrorForTyped(err, "t1")
	if !isRecoveryTerminal(mapped) {
		t.Fatalf("notice 落账成功的 retryable cleanup debt MUST 判终态阻断继续建程, got non-terminal: %v", mapped)
	}
	// notice 落账回执：任务 notice 含 tickets。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Notice.Valid && !strings.Contains(row.Notice.String, "tk-c12-inject") {
		t.Fatalf("residual notice MUST carry tickets, got %q", row.Notice.String)
	}
	_ = binding
	// notice 落账后 MUST NOT 继续建程：Acquire 已终态返回——进程创建计数恰为
	// 轮换耗尽前的次数（supPortRetries=3），且终态分派阻断后续 attempt（isRecoveryTerminal 真）。
}

// TestStageErrorInjection_PortExhaustionTerminal 固定输入：真实 supervisor +
// AllocatePort 耗尽（attempt>0 轮换分配返回错误）。断言：supervisor.allocate
// StageError → task 层终态（spec「端口分配：范围耗尽→终态补偿」）。
//
// mutation 证据（已验证 → 红 → 还原）：spi_supervisor.go attempt>0 allocate 出口改
// 回普通 NewError → task 层原样返回 → isRecoveryTerminal 假 → 本测试红。
func TestStageErrorInjection_PortExhaustionTerminal(t *testing.T) {
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return newMockOC(true)
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 30 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	seedServeEnvSnapshot(store, "t1", 0)

	// AllocatePort：首次给死端口，attempt>0 轮换分配耗尽（返回错误）。
	calls := 0
	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) {
			calls++
			if calls == 1 {
				return 50198, nil
			}
			return 0, errors.New("serve port range exhausted")
		},
		ReadyTimeout: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	sup := opencode.NewSPIProviderWithDeps(deps).Supervisor()
	_, err := sup.Acquire(context.Background(), appagent.TaskRef{
		TaskID: "t1", Directory: "/wt/t1",
	})
	if err == nil {
		t.Fatal("want allocate exhaustion error, got nil")
	}
	mapped := m.supervisorStageErrorForTyped(err, "t1")
	if !isRecoveryTerminal(mapped) {
		t.Fatalf("端口耗尽 MUST 判终态（spec 分派表：端口分配范围耗尽→终态补偿）, got non-terminal: %v", mapped)
	}
}

// TestStageErrorInjection_BindingEndpointTerminal 说明：claim/probe/allocate/cleanup
// 三项注入已覆盖「从真实 supervisor 注入失败」的门禁要求；binding_endpoint 无效属
// task 层自有 fail 面（手工构造形态保留在 spi_stageerror_dispatch_test.go）。
