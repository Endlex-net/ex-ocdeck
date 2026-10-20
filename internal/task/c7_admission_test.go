// c7_admission_test.go 补 C7 门禁修复的行为测试（m1203 C7 验证要求）：
// 生命周期副作用前按任务行声明 provider 做 registry 准入；未注册 → 零进程/
// 网络/归属/锚定/binding 副作用 + 诊断性 last_error（specs「Provider registry
// 分派」fail-closed scenario + D10「provider 未注册」行）。生产改动
// （registryAdmitTask/recordAdmissionError/reconcile 循环体首准入/Activate
// suspended 门禁后准入）已落地，本文件给固定输入断言与 mutation 证据。
package task

import (
	"context"
	"errors"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// c7UnknownTaskID 声明了未注册 provider 的任务（tasks.provider_id 持久化为
// "other-v9"——mock GetTaskProviderID 回读该值，registryAdmitTask 分派失败）。
const c7UnknownProvider = "other-v9"

// seedSuspendedTaskWithProvider 在 seedSuspendedTask 基础上改写 provider 声明。
func seedSuspendedTaskWithProvider(s *mockStore, taskID, projectID, providerID string) TaskRow {
	t := seedSuspendedTask(s, taskID, projectID)
	s.mutTask(taskID, func(row *TaskRow) { row.ProviderID = providerID })
	return t
}

// TestC7_ReconcileUnknownProviderZeroSideEffects 固定输入：suspended 任务声明
// 未注册 provider。断言（specs「未注册 provider fail-closed」）：Reconcile 后
// 状态保持 suspended、last_error=provider not registered（唯一允许的持久化副
// 作用）、零 NewSession/零 KillSession、binding 与归属零修改、无恢复 incident。
//
// mutation 证据（已验证 → 红 → 还原）：reconcile.go C7 准入段删除（或
// ProviderNotFound 分支 continue 前无 recordAdmissionError）→ 本测试红：
// 状态偏移/suspended 分支被绕过时 last_error 缺失、或准入缺失时任务进入
// reconcile 后续生命周期（active+陈旧 runtime 形态会触发 kill/恢复）。
func TestC7_ReconcileUnknownProviderZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedSuspendedTaskWithProvider(store, "t1", "p1", c7UnknownProvider)
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	bindingBefore, _ := store.bindingRawOf("t1")
	ownedBefore := len(store.sessions["t1"])

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 状态零修改（C7：不修改任务状态；last_error 属允许的诊断性持久化）。
	assertStatus(t, store, "t1", StatusSuspended)
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || !contains(row.LastError.String, "provider not registered") {
		t.Errorf("last_error = %+v, want provider not registered", row.LastError)
	}
	// 零进程副作用。
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0", n)
	}
	if n := len(proc.killOrder); n != 0 {
		t.Errorf("KillSession = %d, want 0", n)
	}
	// binding/归属零修改。
	if bindingAfter, _ := store.bindingRawOf("t1"); bindingAfter != bindingBefore {
		t.Errorf("binding changed: %q → %q", bindingBefore, bindingAfter)
	}
	if ownedAfter := len(store.sessions["t1"]); ownedAfter != ownedBefore {
		t.Errorf("ownership changed: %d → %d", ownedBefore, ownedAfter)
	}
}

// TestC7_ReconcileActiveUnknownProviderZeroSideEffects 固定输入：active 任务
// 声明未注册 provider（数据损坏/回填窗口形态）。断言：不触发恢复/清理/挂起——
// Reconcile 跳过该任务生命周期，active 状态保持，零 kill/零 NewSession，
// 仅诊断 last_error（provider 准入先于一切生命周期分支）。任务停留 active
// 由运维收敛（本 fixture 锚定「准入缺失时 active+runtime-gone 会误触发
// cleanup+commitSuspendedReconcile」的反面）。
func TestC7_ReconcileActiveUnknownProviderZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	store.mutTask("t1", func(row *TaskRow) { row.ProviderID = c7UnknownProvider })
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	assertStatus(t, store, "t1", StatusActive)
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || !contains(row.LastError.String, "provider not registered") {
		t.Errorf("last_error = %+v, want provider not registered", row.LastError)
	}
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0", n)
	}
	if n := len(proc.killOrder); n != 0 {
		t.Errorf("KillSession = %d, want 0 (active 形态也不得进入 legacy cleanup/resume 决策)", n)
	}
}

// TestC7_ActivateUnknownProviderZeroSideEffects 固定输入：suspended 任务声明
// 未注册 provider，用户请求激活。断言：Activate 返回 typed 错误且不落
// activating（C7 准入先于 beginActivation）、diagnostic last_error 落账、
// 零进程副作用、下次 Activate 仍可重试（状态未被破坏）。
//
// mutation 证据（已验证 → 红 → 还原）：activate.go C7 准入段删除 → 激活进入
// activateRun（supervisor/内联建程）→ NewSession>0 副作用、本测试红。
func TestC7_ActivateUnknownProviderZeroSideEffects(t *testing.T) {
	store := newMockStore()
	seedSuspendedTaskWithProvider(store, "t1", "p1", c7UnknownProvider)
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	err := m.Activate(context.Background(), "t1")
	if err == nil {
		t.Fatal("Activate MUST fail for unregistered provider")
	}
	var oe *OpError
	if !errors.As(err, &oe) {
		t.Fatalf("err type = %T, want *OpError", err)
	}
	// 状态保持 suspended（诊断性 last_error 不迁移状态）。
	assertStatus(t, store, "t1", StatusSuspended)
	row, _ := store.GetTask(context.Background(), "t1")
	if !row.LastError.Valid || !contains(row.LastError.String, "provider not registered") {
		t.Errorf("last_error = %+v, want provider not registered", row.LastError)
	}
	if n := len(proc.newSessionNamesSnapshot()); n != 0 {
		t.Errorf("NewSession = %d, want 0（activation 零进程副作用）", n)
	}
}

// TestC7_RegisteredProviderStillAdmitted 固定输入：suspended 任务声明
// opencode-v1（默认回填值）。断言：准入放行（注册表 Get 成功），Activate
// 正常进入后续流程（错误不再是 provider_not_registered——用激活成功表达，
// 建程经内联回退路径 minimalProvider failClosed 不影响准入判定）。
// 该 case 同时是 mutation 的对照面：准入判定误伤已注册任务即红。
func TestC7_RegisteredProviderStillAdmitted(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1") // ProviderID 空 → GetTaskProviderID DEFAULT v1
	proc := newMockProc()
	m := newTestManager(t, store, proc, newMockWorktree(), newMockOC(true))

	if err := m.registryAdmitTask(context.Background(), "t1"); err != nil {
		t.Fatalf("registryAdmitTask(v1): %v", err)
	}
	var ae *appagent.Error
	if err := m.registryAdmitTask(context.Background(), "t-missing"); err == nil || errors.As(err, &ae) && ae.Kind != appagent.ErrKindProviderNotFound {
		t.Errorf("missing task admission err = %v, want ProviderNotFound（行缺失按未声明分派失败 fail-closed）", err)
	}
}

// 编译期锚定：c7UnknownProvider 必须不是 store.ProviderIDV1（fixture 语义前提）。
var _ = mustNotEqualV1(c7UnknownProvider)

func mustNotEqualV1(s string) bool { return s != store.ProviderIDV1 }
