package task

// binding_endpoint_gate_test.go 阶段 4 一致性修复（Section 7 评审 F1）：
// bindingEndpoint 与 resolveRuntimeBinding 统一镜像回退闸门——provider 拒绝
// （BindingInvalid/instance 不匹配/凭据派生失败）原样传播，仅
// isTestSeamBindingErr（CapabilityUnavailable 接缝形态）允许 v1 镜像回退
//（过渡裁决 B）。修复前 bindingEndpoint 无闸门无条件回退，两条路径不一致。
//
// mutation 证据（红→还原，闸门旁路）：isTestSeamBindingErr 判定改 `false &&`
//（endpoint err 一律回退镜像）→ 两测试红——镜像重新接受 adapter 拒绝的
// rogue-instance binding（data 含 port/auth.kind=derived，镜像可解成功返回；
// 生产串联形态 Activate 成功返回 nil）→ 还原。
import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
)

// rogueBinding 构造「adapter 拒绝、镜像可解」的 binding：provider instance 非法
// （spi decodeV1Binding 拒绝非 default 实例），data 含 port/auth.kind=derived
// （master secret 在位时镜像 decodeV1BindingData+password() 可成功派生——正是
// 「镜像重新接受 adapter 拒绝」的攻击面形态）。
func rogueBinding() domain.TaskBinding {
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         "opencode-v1",
		ProviderInstanceID: "rogue-instance",
		UpstreamSessionID:  "ses-anchor-t1",
		TerminalRef:        runtimeSessionName("t1"),
		Data:               []byte(`{"port":50001,"directory":"/data/worktrees/p1/t1","taskId":"t1","auth":{"kind":"derived"}}`),
	}
}

// TestBindingEndpoint_AdapterRejectPropagatesSameGate 单元：真实 provider 的
// BindingRuntimeEndpoint 对非法 instance 的拒绝经 bindingEndpoint 原样传播
// （与 resolveRuntimeBinding 同一闸门）；接缝形态（minimalProvider =
// CapabilityUnavailable）仍允许镜像回退——过渡裁决 B 不受闸门影响。
func TestBindingEndpoint_AdapterRejectPropagatesSameGate(t *testing.T) {
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	bad := rogueBinding()

	// ① 真实 provider：拒绝传播（文本含 adapter 命名空间断言——镜像重解的
	// 错误文本无此形态）。
	reg := appagent.NewRegistry()
	if err := reg.Register(opencode.NewSPIProvider()); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: newMockStore(), Proc: newMockProc(), Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return newMockOC(true)
		},
		ProviderRegistry: reg,
	})
	_, _, _, err := m.bindingEndpoint(bad)
	if err == nil {
		t.Fatal("rogue-instance binding MUST be rejected（闸门修复后 provider 拒绝传播；回退镜像则成功返回）")
	}
	if !strings.Contains(err.Error(), "不属于 opencode-v1/default") {
		t.Fatalf("err = %v, want adapter 命名空间拒绝文本（镜像重解文本错位）", err)
	}

	// ② 接缝对照：minimalProvider（CapabilityUnavailable）→ 镜像回退成功。
	legal := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         "opencode-v1",
		ProviderInstanceID: "default",
		UpstreamSessionID:  "ses-anchor-t1",
		TerminalRef:        runtimeSessionName("t1"),
		Data:               []byte(`{"port":50001,"directory":"/data/worktrees/p1/t1","taskId":"t1","auth":{"kind":"legacy","password":"legacy-pw"}}`),
	}
	m2 := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: newMockStore(), Proc: newMockProc(), Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return newMockOC(true)
		},
		ProviderRegistry: newTestRegistry(),
	})
	port, pw, dir, err2 := m2.bindingEndpoint(legal)
	if err2 != nil {
		t.Fatalf("接缝形态镜像回退必须保持（过渡裁决 B）: %v", err2)
	}
	if port != 50001 || pw != "legacy-pw" || dir != "/data/worktrees/p1/t1" {
		t.Fatalf("镜像回退三元组 = (%d,%q,%q), want (50001,legacy-pw,/data/worktrees/p1/t1)", port, pw, dir)
	}
}

// rogueBindingSupervisor 是 Acquire 恒返 rogue binding 的 stub（Activate supervisor
// 分支生产串联——binding 来自 supervisor 返回值而非 store，闸门修复前该 binding
// 经 bindingEndpoint 被镜像静默接受；registry 侧注册真实 provider 提供端点解码）。
type rogueBindingSupervisor struct{ bad domain.TaskBinding }

func (s rogueBindingSupervisor) Acquire(context.Context, appagent.TaskRef) (domain.TaskBinding, error) {
	return s.bad, nil
}
func (s rogueBindingSupervisor) Resume(context.Context, appagent.TaskRef, domain.TaskBinding, appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.resume", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}
func (s rogueBindingSupervisor) AcquireEphemeral(context.Context, appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.acquire_ephemeral", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}
func (s rogueBindingSupervisor) Release(context.Context, domain.TaskBinding) error {
	return appagent.NewError("test.release", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}

// TestActivate_SupervisorBindingRejectPropagates 生产串联（activateRun supervisor
// 分支）：Acquire 成功返回 rogue binding → bindingEndpoint provider 拒绝传播 →
// Activate 失败（错误含 adapter 命名空间文本）+ 失败补偿落 suspended。
func TestActivate_SupervisorBindingRejectPropagates(t *testing.T) {
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	srv := newFakeOCServer(t)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	bad := rogueBinding()
	// registry 注册真实 provider（BindingRuntimeEndpoint 对 rogue instance 真实
	// 拒绝）；runtimeSupervisor 用独立 stub（Acquire 返回 rogue binding）——生产
	// 装配中两者本就分离（supervisor 返回 binding，registry 分派端点解码）。
	reg := appagent.NewRegistry()
	if err := reg.Register(opencode.NewSPIProvider()); err != nil {
		t.Fatalf("register: %v", err)
	}
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
		ProviderRegistry: reg,
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	m.runtimeSupervisor = rogueBindingSupervisor{bad: bad}

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate MUST fail（rogue binding 经 bindingEndpoint 被拒绝并传播；闸门旁路则镜像接受成功返回）")
	}
	if !strings.Contains(err.Error(), "不属于 opencode-v1/default") {
		t.Fatalf("err = %v, want adapter 命名空间拒绝文本", err)
	}
	// 失败补偿落 suspended（既有统一补偿路径）。
	if row := mustGet(store, "t1"); row.Status != StatusSuspended {
		t.Fatalf("status = %s, want suspended（失败补偿）", row.Status)
	}
}
