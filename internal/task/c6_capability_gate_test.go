package task

// c6_capability_gate_test.go 锚定 C6 残余（review attempt 3）：生产 OCClient
// 调用绕过 reporter → 修复后经 buildTaskOCClient 网关（生产装配形态：真实
// SPIProvider + WithHealthReporter）触发真实 HTTP 协议错误，断言 capabilities
// 观测（taskId/epoch/degraded/unsupported）经能力出口产生——非直接调 SPI adapter
//（网关桥 spiBridgeOCClient 即生产路径）。
//
// mutation 证据（已验证 → 红 → 还原）：
// 1. buildTaskOCClient 桥接判定旁路（恒回 fallback）→ 能力出口不经 provider →
//    healthTracker 无 status_reader/attention_io 观测 → 测试红「snapshot missing」。
// 2. reportConversion 的 CapabilityUnavailable→unsupported 映射删除 → titles 404
//    观测 degraded → infra 侧既有测试红（spi_health_wiring）；本文件的 unsupported
//    腿复用该映射（title sync 404 经网关触发）。

import (
	"context"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
)

func newC6GateManager(t *testing.T, srv *fakeOCServer, tracker *appagent.HealthTracker, store *mockStore) *Manager {
	t.Helper()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: srv.port + 1, Max: srv.port + 3},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: newCharxProc(), Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(opts))
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	return m
}

// waitForCapability 轮询 tracker 快照直到出现指定 primitive 的观测（provider
// 出口 reportConversion 异步语义为同步调用——轮询仅为调用链收敛缓冲）。
func waitForCapability(t *testing.T, tracker *appagent.HealthTracker, primitive appagent.CapabilityPrimitive) appagent.CapabilityHealth {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, h := range tracker.Snapshot() {
			if h.Primitive == primitive {
				return h
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("capability snapshot missing primitive %s; got %+v", primitive, tracker.Snapshot())
	return appagent.CapabilityHealth{}
}

// TestC6_GatewaySessionStatusDegradedObservation 固定输入：生产装配桥经网关
// （buildTaskOCClient）调用 StatusReader（SessionStatus）→ fake server 对
// /session/status 返回协议违约形状（probeBroken：数组）→ classifyHTTPError →
// RuntimeUnavailable typed → SPI 出口 reportConversion degraded。
// 断言：taskId=v1 data.taskId（非 upstream 假名）、epoch>0（换代递增面）、
// state=degraded、reason 带 op。
func TestC6_GatewaySessionStatusDegradedObservation(t *testing.T) {
	srv := newFakeOCServer(t)
	srv.setProbeBroken(true)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	seedDerivedBinding(store, "t1", srv.port, "/data/worktrees/p1/t1")

	tracker := appagent.NewHealthTracker()
	tracker.SetProviderID(opencode.ProviderID)

	p := opencode.NewSPIProvider() // 窄接口形态（BindingRuntimeEndpoint 可用）
	p.WithHealthReporter(func(h appagent.CapabilityHealth) { tracker.Report(h) },
		func(taskID string) int64 { return tracker.RuntimeEpoch(taskID) })
	tracker.BumpRuntimeEpoch("t1")

	sup := p.Supervisor()
	_ = sup

	m := newC6GateManager(t, srv, tracker, store)
	// 生产分派面替换为注册真实 provider 的 registry（覆盖 minimalProvider）。
	reg := appagent.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.providerRegistry = reg

	oc, err := m.buildTaskOCClient(context.Background(), "t1", appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("buildTaskOCClient: %v", err)
	}
	bridge, ok := oc.(*spiBridgeOCClient)
	if !ok {
		t.Fatalf("buildTaskOCClient MUST return capability bridge (C2/C6), got %T", oc)
	}
	if bridge.taskID != "t1" {
		t.Fatalf("bridge taskID = %q, want t1 (观测身份键 = v1 data.taskId)", bridge.taskID)
	}
	// 经网关触发真实 HTTP 协议错误（/session/status 结构违约 → classifyHTTPError）。
	if _, serr := oc.SessionStatus(context.Background(), "/data/worktrees/p1/t1"); serr == nil {
		t.Fatal("probeBroken server MUST yield protocol error via gateway bridge")
	}
	h := waitForCapability(t, tracker, appagent.PrimitiveStatusReader)
	if h.TaskID != "t1" {
		t.Fatalf("capability taskId = %q, want t1", h.TaskID)
	}
	if h.Epoch <= 0 {
		t.Fatalf("capability epoch = %d, want > 0", h.Epoch)
	}
	if h.State != appagent.CapDegraded {
		t.Fatalf("capability state = %s, want degraded (协议违约非 unsupported)", h.State)
	}
	if h.Reason == "" {
		t.Fatal("capability reason MUST carry op prefix")
	}
}

// TestC6_GatewayTitleUnsupportedObservation 覆盖 unsupported 腿：title 404 →
// ErrSessionTitleUnsupported → CapabilityUnavailable → reportConversion unsupported
// （同既有 spi_health_wiring 形态，但经网关桥而非直调 SPI adapter）。
func TestC6_GatewayTitleUnsupportedObservation(t *testing.T) {
	srv := newFakeOCServer(t)
	srv.titleStatus = 404
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	seedDerivedBinding(store, "t1", srv.port, "/data/worktrees/p1/t1")

	tracker := appagent.NewHealthTracker()
	tracker.SetProviderID(opencode.ProviderID)

	p := opencode.NewSPIProvider()
	p.WithHealthReporter(func(h appagent.CapabilityHealth) { tracker.Report(h) },
		func(taskID string) int64 { return tracker.RuntimeEpoch(taskID) })
	tracker.BumpRuntimeEpoch("t1")

	m := newC6GateManager(t, srv, tracker, store)
	reg := appagent.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.providerRegistry = reg

	oc, err := m.buildTaskOCClient(context.Background(), "t1", appagent.Options{HealthTimeout: 2 * time.Second, OpTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("buildTaskOCClient: %v", err)
	}
	if _, Terr := oc.GetSession(context.Background(), "/data/worktrees/p1/t1", "ses_missing"); Terr == nil {
		// GetSession 404 → NotFound（合法对象缺失，degraded 观测）。
		t.Fatal("want 404 error")
	}
	if terr := oc.UpdateSessionTitle(context.Background(), "/data/worktrees/p1/t1", "ses_x", "t"); terr == nil {
		t.Fatal("title 404 MUST error (CapabilityUnavailable)")
	}
	h := waitForCapability(t, tracker, appagent.PrimitiveTitleUpdater)
	if h.State != appagent.CapUnsupported {
		t.Fatalf("capability state = %s, want unsupported (title 404 → 能力降级)", h.State)
	}
	if h.TaskID != "t1" {
		t.Fatalf("capability taskId = %q, want t1", h.TaskID)
	}
}
