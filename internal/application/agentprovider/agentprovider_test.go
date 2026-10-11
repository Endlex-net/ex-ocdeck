package agentprovider

// agentprovider_test.go 覆盖 tasks 3.2 验证项：registry fail-closed、typed error
// sentinel 匹配（errors.Is 按 Kind）、能力健康内存维护；stubProvider 同时充当
// Provider 接口的编译期断言。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
)

// stubProvider 满足 Provider 全部窄接口（编译期断言：接口形状变更即编译失败）。
type stubProvider struct{ meta ProviderMeta }

var _ Provider = stubProvider{}

func (p stubProvider) Meta() ProviderMeta          { return p.meta }
func (stubProvider) Supervisor() RuntimeSupervisor { return stubSupervisor{} }
func (stubProvider) Sessions() SessionControl      { return nil }
func (stubProvider) Events() EventSource           { return nil }
func (stubProvider) Attention() AttentionIO        { return nil }
func (stubProvider) Prompts() AsyncPrompt          { return nil }
func (stubProvider) Status() StatusReader          { return nil }
func (stubProvider) Terminal() TerminalIO          { return nil }
func (stubProvider) Titles() TitleUpdater          { return nil }
func (stubProvider) BindingRuntimeEndpoint(domain.TaskBinding) (int, string, string, error) {
	return 0, "", "", NewError("endpoint", ErrKindCapabilityUnavailable, errors.New("stub provider"))
}

type stubSupervisor struct{}

var _ RuntimeSupervisor = stubSupervisor{}

func (stubSupervisor) Acquire(ctx context.Context, task TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, nil
}
func (stubSupervisor) Resume(ctx context.Context, task TaskRef, old domain.TaskBinding, policy ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, nil
}
func (stubSupervisor) AcquireEphemeral(ctx context.Context, task TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, nil
}
func (stubSupervisor) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

func TestRegistry_FailClosedOnUnregistered(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{meta: ProviderMeta{ProviderID: "opencode-v1"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 重复注册 → 错误（启动期装配错误）。
	if err := r.Register(stubProvider{meta: ProviderMeta{ProviderID: "opencode-v1"}}); err == nil {
		t.Fatal("重复注册必须报错")
	}
	// 空 providerID → 拒绝。
	if err := r.Register(stubProvider{meta: ProviderMeta{ProviderID: ""}}); err == nil {
		t.Fatal("空 providerID 必须拒绝")
	}
	p, err := r.Get("opencode-v1")
	if err != nil || p == nil {
		t.Fatalf("Get 已注册: %v", err)
	}
	// 未注册 → fail-closed：Kind=provider_not_found 且 errors.Is 命中 sentinel。
	got, gerr := r.Get("codex-9")
	if got != nil {
		t.Fatal("未注册 Get 必须返回 nil provider")
	}
	if gerr == nil {
		t.Fatal("未注册 Get 必须返回错误（fail-closed）")
	}
	if !errors.Is(gerr, ErrProviderNotFound) {
		t.Errorf("errors.Is(ErrProviderNotFound) = false, err=%v", gerr)
	}
	if KindOf(gerr) != ErrKindProviderNotFound {
		t.Errorf("KindOf = %q, want provider_not_found", KindOf(gerr))
	}
	if metas := r.List(); len(metas) != 1 || metas[0].ProviderID != "opencode-v1" {
		t.Errorf("List = %+v", metas)
	}
}

func TestError_SentinelMatchingByKind(t *testing.T) {
	sentinels := map[ErrorKind]error{
		ErrKindProviderNotFound:      ErrProviderNotFound,
		ErrKindBindingInvalid:        ErrBindingInvalid,
		ErrKindRuntimeUnavailable:    ErrRuntimeUnavailable,
		ErrKindNotFound:              ErrNotFound,
		ErrKindConflict:              ErrConflict,
		ErrKindCapabilityUnavailable: ErrCapabilityUnavailable,
	}
	for kind, sentinel := range sentinels {
		inner := fmt.Errorf("inner-%s", kind)
		err := NewError("op.x", kind, inner)
		if !errors.Is(err, sentinel) {
			t.Errorf("Kind %s 未命中 sentinel（errors.Is）", kind)
		}
		// 其他 sentinel 不误命中。
		for otherKind, other := range sentinels {
			if otherKind == kind {
				continue
			}
			if errors.Is(err, other) {
				t.Errorf("Kind %s 误命中 %s 的 sentinel", kind, otherKind)
			}
		}
		// 原始链可经 errors.As 取出。
		var ae *Error
		if !errors.As(err, &ae) || ae.Op != "op.x" {
			t.Errorf("errors.As 取原始 *Error 失败: %v", err)
		}
		if !errors.Is(err, inner) {
			t.Errorf("原始错误链丢失: %v", err)
		}
	}
	// 未知 Kind 不匹配任何 sentinel，但仍可按自身比较。
	weird := NewError("op.y", ErrorKind("bogus"), nil)
	if errors.Is(weird, ErrNotFound) {
		t.Error("未知 Kind 不得命中 sentinel")
	}
	if KindOf(weird) != ErrorKind("bogus") {
		t.Errorf("KindOf(未知) = %q", KindOf(weird))
	}
	// 非 *Error 输入。
	if KindOf(errors.New("plain")) != "" {
		t.Error("非 *Error 输入 KindOf 必须为空")
	}
}

func TestHealthTracker_UpsertAndReset(t *testing.T) {
	tr := NewHealthTracker()
	// tasks 3.8 预填语义：provider 作用域 7 原语初始 unknown epoch 0
	//（server 重启归零 unknown 的透出基础）。
	if n := len(tr.Snapshot()); n != 7 {
		t.Fatalf("初始 Snapshot len = %d, want 7（provider 原语预填 unknown）", n)
	}
	for _, h := range tr.Snapshot() {
		if h.Scope != ScopeProvider || h.State != CapUnknown || h.Epoch != 0 {
			t.Fatalf("预填记录 = %+v, want provider/unknown/epoch0", h)
		}
	}
	key := domain.SessionKey{ProviderID: "opencode-v1", ProviderInstanceID: "default", UpstreamID: "ses_1"}
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveTitleUpdater, Scope: ScopeRuntime, TaskID: "t1", State: CapUnknown, Epoch: 1})
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveTitleUpdater, Scope: ScopeRuntime, TaskID: "t1", State: CapDegraded, Reason: "404", Epoch: 2})
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveAsyncPrompt, Scope: ScopeSession, TaskID: "t1", Session: &key, State: CapAvailable})
	tr.Report(CapabilityHealth{ProviderID: "opencode-v1", Primitive: PrimitiveEventSource, Scope: ScopeProvider, State: CapAvailable})

	// 7 预填 + runtime 1（upsert 去重）+ session 1 + provider 显式 1 = 10。
	snap := tr.Snapshot()
	if len(snap) != 10 {
		t.Fatalf("Snapshot len = %d, want 10（同 identity upsert 去重）", len(snap))
	}
	find := func(primitive CapabilityPrimitive, scope CapabilityScope, upstream string) (CapabilityHealth, bool) {
		for _, h := range snap {
			if h.Primitive != primitive || h.Scope != scope {
				continue
			}
			if upstream == "" && h.Session == nil {
				return h, true
			}
			if upstream != "" && h.Session != nil && h.Session.UpstreamID == upstream {
				return h, true
			}
		}
		return CapabilityHealth{}, false
	}
	if got, ok := find(PrimitiveTitleUpdater, ScopeRuntime, ""); !ok || got.State != CapDegraded || got.Epoch != 2 {
		t.Errorf("upsert 未生效: (%+v, %v)", got, ok)
	}
	if got, ok := find(PrimitiveAsyncPrompt, ScopeSession, "ses_1"); !ok || got.Session == nil || got.Session.UpstreamID != "ses_1" {
		t.Errorf("session 作用域记录丢失: (%+v, %v)", got, ok)
	}

	// ResetRuntime：runtime/session 作用域清除，provider 作用域保留
	//（预填 + 显式 provider 记录均保留）。
	tr.ResetRuntime("t1")
	snap = tr.Snapshot()
	if len(snap) != 8 {
		t.Fatalf("ResetRuntime 后 len = %d, want 8（7 预填 + 1 显式 provider）", len(snap))
	}
	for _, h := range snap {
		if h.Scope != ScopeProvider {
			t.Errorf("ResetRuntime 后残留非 provider 记录: %+v", h)
		}
	}
}
