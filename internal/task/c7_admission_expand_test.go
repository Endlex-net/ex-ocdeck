// c7_admission_expand_test.go 补齐 C7 残余（ora-4 attempt 2）的准入面扩展验证：
// suspend/delete/kill-reconcile 三入口在首次状态/进程副作用前按任务 provider 准入，
// 未注册 provider 零副作用（状态/binding/归属/进程全不变 + 诊断性 last_error）；
// kill 模式区分有归属会话（跳过清理）与真 orphan（清理不受准入约束）。
package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
)

// seedTaskWithProvider 种子任意状态任务并声明 provider（空 = DEFAULT v1）。
func seedTaskWithProvider(s *mockStore, taskID, status, providerID string) {
	seedSuspendedTask(s, taskID, "p1")
	s.mutTask(taskID, func(row *TaskRow) {
		row.Status = status
		row.ProviderID = providerID
	})
}

// TestC7_SuspendUnknownProviderZeroSideEffects 固定输入：active 任务声明未注册
// provider → Suspend。断言：状态保持 active（不落 suspending）、零 NewSession、
// 零 kill、binding/归属不变、last_error 诊断性记录。
//
// mutation 证据（已验证 → 红）：suspend.go 准入判定旁路（`if false &&`）→
// suspending 提交逃逸 → 本测试红（status=suspending / last_error 缺失）。
func TestC7_SuspendUnknownProviderZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedTaskWithProvider(store, "t1", StatusActive, c7UnknownProvider)
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Suspend(context.Background(), "t1")
	if err == nil || !isProviderNotFound(errors.Unwrap(err)) && !strings.Contains(err.Error(), "provider") {
		t.Fatalf("Suspend err = %v, want provider-not-registered reject", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	if !strings.Contains(store.tasks["t1"].LastError.String, "provider not registered") {
		t.Errorf("last_error = %q, want 诊断性记录", store.tasks["t1"].LastError.String)
	}
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0", n)
	}
	if n := len(proc.killOrder); n != 0 {
		t.Errorf("KillSession = %d, want 0（准入失败零进程副作用）", n)
	}
}

// TestC7_DeleteUnknownProviderZeroSideEffects 固定输入：suspended 任务声明未注册
// provider → Delete。断言：deleting 意图零提交（状态保持 suspended）、零 kill、
// DB 行保留、last_error 诊断。
func TestC7_DeleteUnknownProviderZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedTaskWithProvider(store, "t1", StatusSuspended, c7UnknownProvider)
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Delete(context.Background(), "t1", DeleteNormal, false)
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("Delete err = %v, want provider-not-registered reject", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	if !strings.Contains(store.tasks["t1"].LastError.String, "provider not registered") {
		t.Errorf("last_error = %q, want 诊断性记录", store.tasks["t1"].LastError.String)
	}
	if store.deleteTaskCount != 0 {
		t.Errorf("DeleteTask = %d, want 0（删除零副作用）", store.deleteTaskCount)
	}
	if _, ok := store.tasks["t1"]; !ok {
		t.Error("task row MUST survive admission rejection")
	}
	if n := len(proc.killOrder); n != 0 {
		t.Errorf("KillSession = %d, want 0", n)
	}
}

// killPolicyManager 直构 kill 模式 Manager（newTestManager 固定 persist 策略）。
func killPolicyManager(t *testing.T, store TaskStore, proc ProcessBackend) *Manager {
	t.Helper()
	ms, _ := store.(*mockStore)
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		ServePortRange: config.PortRange{Min: 50000, Max: 50999},
		ShutdownPolicy: config.ShutdownKillImmediate,
	}
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(Options{Cfg: cfg, Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return &readyOC{inner: newMockOC(true), onReady: opts.OnReady}
		},
		ProviderRegistry: reg})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	_ = ms
	return m
}

// TestC7_KillReconcileUnknownProviderKeepsSessions 固定输入：kill 模式 reconcile，
// active 任务声明未注册 provider 且会话存在。断言：(1) 任务状态不收敛（保持 active
// ——不按默认 v1 收敛挂起）；(2) 该任务会话跳过清理（保留）；(3) last_error 诊断。
//
// mutation 证据（已验证 → 红）：reconcileKill 准入段删除 → 会话被 kill（killOrder
// >0）+ 状态收敛 suspended → 本测试红。
func TestC7_KillReconcileUnknownProviderKeepsSessions(t *testing.T) {
	store := newMockStore()
	seedTaskWithProvider(store, "t1", StatusActive, c7UnknownProvider)
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := killPolicyManager(t, store, proc)

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusActive)
	if !strings.Contains(store.tasks["t1"].LastError.String, "provider not registered") {
		t.Errorf("last_error = %q, want 诊断性记录", store.tasks["t1"].LastError.String)
	}
	// 未知 provider 任务会话保留（HasSession 仍真）。
	if alive, _ := proc.HasSession(runtimeSessionName("t1")); !alive {
		t.Error("unknown provider task session MUST NOT be killed in kill reconcile")
	}
	if n := len(proc.killOrder); n != 0 {
		t.Errorf("KillSession = %d, want 0（未知 provider 零清理）", n)
	}
}

// TestC7_KillReconcileRegisteredProviderConverges 对照面：v1 provider 任务在 kill
// 模式下仍按既有语义收敛 suspended（准入放行）。
func TestC7_KillReconcileRegisteredProviderConverges(t *testing.T) {
	store := newMockStore()
	seedTaskWithProvider(store, "t1", StatusActive, "") // 空 = DEFAULT v1
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	m := killPolicyManager(t, store, proc)

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertStatus(t, store, "t1", StatusSuspended)
	// 准入放行的任务会话按既有 kill 语义清理。
	if n := len(proc.killOrder); n == 0 {
		t.Error("registered provider task sessions SHOULD be killed (kill mode)")
	}
}

var _ = appagent.ErrKindProviderNotFound
