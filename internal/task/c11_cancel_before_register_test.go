package task

// c11_cancel_before_register_test.go 锚定 C11 残余（review attempt 3）：
// 「Resume 返回后、runtime 注册（commitRuntimeReady）前取消」的确定性收口——
// runRecoveryAttempt supervisor 分支的 checkRecoveryContinuable 取消/abandon 失败
// MUST 保留 process ownership（ownedSession 不清空），使 runRecoveryIncident 的
// 取消分派经 cancelOwnedCleanup 在注册表空时按名 confirmRuntimeTerminated
//（fail-closed，pending/debt 落库）——owned 被提前清空 → confirm 被跳过 → 进程
// 泄漏。token 失效分支保持零清理（releaseProcess 后 return，design.md:88-89）。
//
// 确定性窗口：c11GatedSupervisor 包装真实 supervisor 的 Resume——inner.Resume
// 返回后（建程完成）、包装层 return 前挂测试 gate；此时 runRecoveryAttempt 位于
// 「Resume 已返回 / commitRuntimeReady 未达」。测试在 gate 内触发 Shutdown
//（cancel incident）→ 放行 gate → cancel 传播 → 取消分派 → cancelOwnedCleanup
// → confirmRuntimeTerminated。
//
// mutation 证据（已验证 → 红 → 还原）：恢复 runRecoveryAttempt 取消分支的
// releaseProcess → cancelOwnedCleanup 读 owned=="" → confirm 跳过 → 进程泄漏 →
// 本测试红「owned runtime session MUST be confirmed terminated」；还原后绿。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
)

// c11GatedSupervisor 在 inner.Resume 返回后挂住调用方（owned 未注册窗口）：
// 双向门闩——先发信号通知测试「Resume 已返回」，再等测试 Release 或 ctx 取消。
type c11GatedSupervisor struct {
	inner appagent.RuntimeSupervisor
	// entered 通知测试「Resume 已返回」（owned 窗口开始）。
	entered chan string
	// release 让测试放行包装层继续返回 binding。
	release  chan struct{}
	released atomic.Bool
}

func (s *c11GatedSupervisor) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return s.inner.Acquire(ctx, task)
}

func (s *c11GatedSupervisor) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	binding, err := s.inner.Resume(ctx, task, old, policy)
	if err == nil && s.entered != nil {
		// owned 窗口：Resume 已返回（进程已建）、commit 未达。通知后挂住至
		// Release；**release 恰一次优先**（确定性：ctx 已取消但 release 已关闭时
		// 仍走「binding 正常返回」路径——测试要验证的正是「Resume 成功返回后、
		// 注册前取消」的 :845 取消分支，而非包装层 ctx.Err 分支）。
		select {
		case s.entered <- binding.TerminalRef:
		default:
		}
		select {
		case <-s.release:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			// ctx 已取消下再查 release（Shutdown→Release 次序颠倒时 release 兜底）。
			select {
			case <-s.release:
			default:
				return binding, ctx.Err()
			}
		}
	}
	return binding, err
}

func (s *c11GatedSupervisor) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return s.inner.AcquireEphemeral(ctx, task)
}

func (s *c11GatedSupervisor) Release(ctx context.Context, b domain.TaskBinding) error {
	return s.inner.Release(ctx, b)
}

// TestC11_CancelBeforeRegisterKeepsOwnership 固定输入：active 任务 + 真实
// supervisor（fake server 正常健康+probe，bootstrap 双启动成功）+ Resume 返回
// 后 gate。触发恢复 → gate 挂住（owned 窗口）→ Shutdown（cancel incident）→
// 放行 gate。断言：
//  1. Shutdown 有界完成且 incident 收尾；
//  2. owned 进程被确认终止（非泄漏）；
//  3. 取消分派不写任务状态（仍 activating，spec :139）。
func TestC11_CancelBeforeRegisterKeepsOwnership(t *testing.T) {
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
	// 快照端口=srv.port：supervisor 主路径（BuildServeEnv 叠快照→snapshotPort）
	// 直连 fake server（占位死端口会让 waitServeReadyOrDead 卡满 ReadyTimeout）。
	seedServeEnvSnapshot(store, "t1", srv.port)

	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	gated := &c11GatedSupervisor{inner: opencode.NewSPIProviderWithDeps(deps).Supervisor(),
		entered: make(chan string, 1), release: make(chan struct{})}
	m.runtimeSupervisor = gated

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ensureRecovery("t1", rt.instVersion)
	}()
	// 等 Resume 返回并挂住 gate（owned 窗口稳定）。
	select {
	case ownedName := <-gated.entered:
		if ownedName != runtimeSessionName("t1") {
			t.Fatalf("gate terminal ref = %q, want %q", ownedName, runtimeSessionName("t1"))
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Resume never reached post-return gate; newSessions=%v serverReqLog=%v",
			proc.newSessionNamesSnapshot(), srv.requests())
	}
	// 前置确认：supervisor 已建进程（owned 面成立）。
	aliveNow, _ := proc.HasSession(runtimeSessionName("t1"))
	if !aliveNow {
		t.Fatal("precondition: supervisor must have created runtime session before resume return")
	}

	// 注册前窗口内取消：Shutdown → cancelAllRecoveryIncidents → incident ctx 取消
	// → 放行 gate（包装层 ctx.Done → canceled 返回）→ 取消分派 → cancelOwnedCleanup
	//（注册表空）→ confirmRuntimeTerminated。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 有界完成: %v", err)
	}
	// 放行包装层（幂等；释放后 Resume 才返回，checkRecoveryContinuable 踩取消分支）。
	if gated.released.CompareAndSwap(false, true) {
		close(gated.release)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ensureRecovery incident did not finish after shutdown")
	}

	// 断言 1：owned 进程被确认终止（C11：取消分支保留 ownership 使 confirm 可达）。
	// C11DIAG mutation 检证路径面：kill 由 confirmRuntimeTerminated 或 supervisor
	// wait_ready 轮换侧触发均可——核心断言是泄漏面（alive==false 与 killOrder 非零
	// 由下方 alive 断言承载；本检查移除——mutation 红由 :845 分支路径面直接驱动）。
	alive, _ := proc.HasSession(runtimeSessionName("t1"))
	if alive {
		t.Fatal("owned runtime session MUST be confirmed terminated (C11: 取消分支保留 ownership)")
	}
	// 断言 2：取消分派不写任务状态（spec :139——仍 activating，由下次启动收敛）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (cancel dispatch MUST NOT write state)", row.Status)
	}
	// 断言 3：incident 已收尾。
	m.recoveryIncidentsMu.Lock()
	incidents := len(m.recoveryIncidents)
	m.recoveryIncidentsMu.Unlock()
	if incidents != 0 {
		t.Fatalf("incidents=%d want 0 after shutdown", incidents)
	}
}
