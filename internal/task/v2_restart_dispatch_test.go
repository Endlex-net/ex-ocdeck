// v2_restart_dispatch_test.go 落实 Manager.RestartV2Server 透传验证（操作者发起
// 重启的全局 v2 共享 serve）：v2 supervisor 实现 ServerRestarter → 透传成功；
// registry 未注入 / v2 未注册 / supervisor 未实现窄接口 → capability_unavailable
// fail-closed（用户可见能力，不得 no-op）。
package task

import (
	"context"
	"errors"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

// restartStubSupervisor 内嵌 failSup 满足 RuntimeSupervisor，并实现 ServerRestarter。
type restartStubSupervisor struct {
	failSup
	restarted int
	err       error
}

func (s *restartStubSupervisor) RestartServer(ctx context.Context) error {
	s.restarted++
	return s.err
}

// restartProvider 是 v2 形态 Provider，Supervisor 返回可注入的 stub。
type restartProvider struct {
	v2Stub
	sup *restartStubSupervisor
}

func (p *restartProvider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

// TestRestartV2Server_PassthroughSuccess 落实：v2 provider 注册且 supervisor 实现
// ServerRestarter → RestartV2Server 透传调用一次成功。
func TestRestartV2Server_PassthroughSuccess(t *testing.T) {
	ms := newMockStore()
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)

	stub := &restartStubSupervisor{}
	reg := appagent.NewRegistry()
	if err := reg.Register(&restartProvider{sup: stub}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if err := m.RestartV2Server(context.Background()); err != nil {
		t.Fatalf("RestartV2Server: %v", err)
	}
	if stub.restarted != 1 {
		t.Fatalf("RestartServer calls=%d, want 1", stub.restarted)
	}
}

// TestRestartV2Server_RegistryNil_FailClosed 落实：registry 未注入 →
// capability_unavailable（用户可见能力，不得 no-op）。
func TestRestartV2Server_RegistryNil_FailClosed(t *testing.T) {
	ms := newMockStore()
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.providerRegistry = nil

	err := m.RestartV2Server(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("nil registry kind=%v, want capability_unavailable (err=%v)", appagent.KindOf(err), err)
	}
}

// TestRestartV2Server_V2Unregistered_FailClosed 落实：registry 仅注册 v1（v2 未
// 注册，开关关形态）→ capability_unavailable（重启能力不可用，不得 no-op）。
func TestRestartV2Server_V2Unregistered_FailClosed(t *testing.T) {
	ms := newMockStore()
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	// newTestManagerFull 的 registry 仅注册 v1——v2 未注册。

	err := m.RestartV2Server(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("v2 unregistered kind=%v, want capability_unavailable (err=%v)", appagent.KindOf(err), err)
	}
}

// TestRestartV2Server_SupervisorNotRestarter_FailClosed 落实：v2 provider 注册但
// Supervisor 未实现 ServerRestarter 窄接口 → capability_unavailable。
func TestRestartV2Server_SupervisorNotRestarter_FailClosed(t *testing.T) {
	ms := newMockStore()
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)

	// v2Stub.Supervisor 返回 failSup（未实现 ServerRestarter）。
	reg := appagent.NewRegistry()
	if err := reg.Register(&v2Stub{}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	err := m.RestartV2Server(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("not-restarter kind=%v, want capability_unavailable (err=%v)", appagent.KindOf(err), err)
	}
}

// TestRestartV2Server_PropagatesSupervisorError 落实：supervisor 返回错误时
// 原样透传（不吞错）。
func TestRestartV2Server_PropagatesSupervisorError(t *testing.T) {
	ms := newMockStore()
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)

	stub := &restartStubSupervisor{err: appagent.NewError("supervisor.restart_server",
		appagent.ErrKindRuntimeUnavailable, errors.New("shutting down"))}
	reg := appagent.NewRegistry()
	if err := reg.Register(&restartProvider{sup: stub}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	err := m.RestartV2Server(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("propagated kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(err), err)
	}
}