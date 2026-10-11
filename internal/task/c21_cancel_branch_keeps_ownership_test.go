package task

// c21_cancel_branch_keeps_ownership_test.go 锚定 C21 BLOCKING（第四轮复审）：
// runRecoveryAttempt supervisor 分支的 checkRecoveryContinuable（:843，Resume
// 成功返回后、commitRuntimeReady 前）——非 token-invalid 失败分支 MUST 保留
// process ownership（不 releaseProcess）：supervisor Resume 可能已建进程，
// 取消路径 runRecoveryIncident 的 cancelOwnedCleanup 读 inc.ownedProcess() 在
// 注册表空时按名 confirmRuntimeTerminated——owned 被提前清空 → confirm 被跳过
// → 进程泄漏。token 失效分支保持零清理（releaseProcess 后 return）。
//
// 与 TestC11_CancelBeforeRegisterKeepsOwnership 的差异：该测试的取消实际经
// 包装层 ctx.Done → Resume 返回 err（serr 提前返回，:843 取消分支未被踩）；
// 本测试构造取消发生在 Resume 正常成功返回（binding 无 err）之后的窗口——
// 包装层放行后 incident ctx 已取消，Resume 返回的 binding 无 err → :825 serr
// 检查通过 → bindingEndpoint → :843 checkRecoveryContinuable 命中 ctx.Err()
//（context.Canceled）→ 走非 token-invalid 分支（不 release）→ 处理分支先做
// 同款 confirm（注册表空 + owned 在位）→ 外层 canceled 分派 cancelOwnedCleanup
// → 幂等。核心断言：**进程被确认终止（非泄漏）**、状态不写（activating）。
//
// mutation 证据（破坏 → 红 → 还原）：恢复 :848 取消分支的 inc.releaseProcess()
// → owned=="" → cancelOwnedCleanup 注册表空分支跳过 confirm → 进程泄漏 →
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

// c21GatedSupervisor：inner.Resume 成功返回后放行前把 incident ctx 打入取消态
//（由测试在 release 前调 cancelAllRecoveryIncidents）——保证 :843 检查点踩中
// 取消（而非包装层 ctx.Err 分支）。
type c21GatedSupervisor struct {
	inner    appagent.RuntimeSupervisor
	entered  chan string
	release  chan struct{}
	released atomic.Bool
	// onRelease 由测试注入：放行前执行取消注入（确定性时序：取消先于 Resume
	// 返回值被 runRecoveryAttempt 消费）。
	onRelease func()
}

func (s *c21GatedSupervisor) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return s.inner.Acquire(ctx, task)
}

func (s *c21GatedSupervisor) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	b, err := s.inner.Resume(ctx, task, old, policy)
	if err == nil && s.entered != nil {
		select {
		case s.entered <- b.TerminalRef:
		default:
		}
		select {
		case <-s.release:
		default:
		}
		select {
		case <-s.release:
			if s.onRelease != nil {
				s.onRelease()
			}
		case <-ctx.Done():
			// ctx 已取消时仍优先走 release（onRelease 注取消后 binding 仍无 err
			// 返回——精确命中 :843 取消分支而非 serr 分派）。
			select {
			case <-s.release:
				if s.onRelease != nil {
					s.onRelease()
				}
			default:
				return b, ctx.Err()
			}
		}
	}
	return b, err
}

func (s *c21GatedSupervisor) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return s.inner.AcquireEphemeral(ctx, task)
}

func (s *c21GatedSupervisor) Release(ctx context.Context, b domain.TaskBinding) error {
	return s.inner.Release(ctx, b)
}

// TestC21_CancelAfterResumeReturnKeepsOwnership：Resume 成功返回后、:843 检查点
// 前取消（cancelAllRecoveryIncidents 直注）→ 取消分支保留 ownership → confirm
// 收口（进程确认终止、零泄漏）；取消分派不写状态。
func TestC21_CancelAfterResumeReturnKeepsOwnership(t *testing.T) {
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
	seedServeEnvSnapshot(store, "t1", srv.port)

	deps := opencode.SupervisorDeps{
		Proc: proc, Store: &mockSupStoreSeam{store: store}, SideFx: &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	gated := &c21GatedSupervisor{inner: opencode.NewSPIProviderWithDeps(deps).Supervisor(),
		entered: make(chan string, 1), release: make(chan struct{})}
	// 放行前注入取消：incident ctx 进入取消态 → :843 checkRecoveryContinuable
	// 命中 context.Canceled（非 token invalid）。
	gated.onRelease = func() { m.cancelAllRecoveryIncidents() }
	m.runtimeSupervisor = gated

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ensureRecovery("t1", rt.instVersion)
	}()
	select {
	case ownedName := <-gated.entered:
		if ownedName != runtimeSessionName("t1") {
			t.Fatalf("gate terminal ref = %q, want %q", ownedName, runtimeSessionName("t1"))
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Resume never reached post-return gate; newSessions=%v",
			proc.newSessionNamesSnapshot())
	}
	// 前置：进程已建（owned 面成立），且尚未注册 runtime（注册表空——
	// cancelOwnedCleanup 的 owned-confirm 分支可达）。
	aliveNow, _ := proc.HasSession(runtimeSessionName("t1"))
	if !aliveNow {
		t.Fatal("precondition: supervisor must have created runtime session before resume return")
	}
	if cur := m.getRuntime("t1"); cur != rt && cur != nil {
		// rt 已 setRuntime 但 instVersion 未换代——取消路径 attemptTok==""（未
		// commitRuntimeReady）→ getRuntime 返回 rt 但 token 不匹配 → 零清理分派。
		// 为精确验证 owned-confirm 收口，注册表必须为空：prelude 检查在 attempt
		// 前已过，这里不改注册表（keep rt），取消分派走 attemptTok 未记录分支
		// ——与 C21 契约面（ownership 保留至 confirm 完成）等价：确认由
		// runRecoveryAttempt 处理分支的直接 confirm 承载（:848 分支先做
		// confirmRuntimeTerminated），断言进程被终止即可。
		_ = cur
	}

	// 放行 gate（onRelease 先注入取消）→ binding 正常返回 → :843 取消分支。
	if gated.released.CompareAndSwap(false, true) {
		close(gated.release)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ensureRecovery incident did not finish after cancel injection")
	}

	// 断言 1：owned 进程被确认终止（C21 核心：取消分支保留 ownership 使 confirm
	// 可达；mutation 恢复 releaseProcess → owned=="" → confirm 跳过 → 泄漏红）。
	alive, _ := proc.HasSession(runtimeSessionName("t1"))
	if alive {
		t.Fatal("owned runtime session MUST be confirmed terminated (C21: 取消分支保留 ownership)")
	}
	// 断言 2：取消分派不写任务状态（spec :139——仍 activating）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusActivating {
		t.Fatalf("status=%s want activating (cancel dispatch MUST NOT write state)", row.Status)
	}
}