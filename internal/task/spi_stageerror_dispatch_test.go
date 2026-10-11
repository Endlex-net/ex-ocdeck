// spi_stageerror_dispatch_test.go 锚定 C12 门禁修复：supervisor 阶段错误（application
// StageError/CleanupPayload 契约）到恢复失败矩阵的分派（orchestration spec「逐阶段
// 失败分派表」）——probe→终态补偿、claim 冲突→终态补偿、cleanup debt→notice 落账
// 成功后终态阻断继续建程（持久化失败→pendingCleanupError 保留 debt）、spawn/env/
// wait_ready 类 RuntimeUnavailable→普通 attempt 重试。mutation 证据在各测试注释标注。
package task

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appagent "ocdeck/internal/application/agentprovider"
)

// TestStageErrorMapping_ProbeFailsTerminal 固定输入：supervisor Resume 返回
// op="supervisor.probe" 阶段错误（能力探测失败形态）。断言：映射为
// capabilityProbeError→isRecoveryTerminal 真（终态补偿分派——spec「能力探测任何
// 失败→终态补偿」，MUST NOT 按 attempt 重试烧预算）。
//
// mutation 证据（已验证 → 红 → 还原）：supervisorStageErrorForTyped 的 probe 分派
// 删除（default 原样返回）→ isRecoveryTerminal 假 → 普通 attempt 重试（预算被烧
// 完才终态，且 last_error 形态偏离）→ 本测试红「want terminal」。
func TestStageErrorMapping_ProbeFailsTerminal(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.supervisorStageErrorForTyped(
		appagent.NewStageError("supervisor.probe", appagent.ErrKindRuntimeUnavailable,
			errors.New("after 3 attempts: capability mismatch")), "t1")
	if !isRecoveryTerminal(err) {
		t.Fatalf("probe stage error MUST be terminal (终态补偿), got non-terminal: %v", err)
	}
	var probe *capabilityProbeError
	if !errors.As(err, &probe) {
		t.Fatalf("want capabilityProbeError, got %T", err)
	}
}

// TestStageErrorMapping_ClaimConflictTerminal 固定输入：supervisor 双启动锚定 claim
// 冲突（op="supervisor.claim_anchor"、Kind conflict）。断言：映射 anchorStageError→
// isRecoveryTerminal 真（spec「bootstrap claim 冲突/写失败→终态补偿」）。
func TestStageErrorMapping_ClaimConflictTerminal(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.supervisorStageErrorForTyped(
		appagent.NewStageError("supervisor.claim_anchor", appagent.ErrKindConflict,
			fmt.Errorf("anchor session ses_x conflict (owned by task t2); MUST NOT attach")), "t1")
	if !isRecoveryTerminal(err) {
		t.Fatalf("claim conflict MUST be terminal, got non-terminal: %v", err)
	}
	var anchor *anchorStageError
	if !errors.As(err, &anchor) {
		t.Fatalf("want anchorStageError, got %T", err)
	}
}

// TestStageErrorMapping_CleanupTicketsNoticeOrDebt 固定输入：supervisor 轮换 kill
// 非净 disposition（cleanup payload：reap_failed + tickets）。断言两条腿：
// (1) notice 写成功 → retryable cleanup debt 已落账 → 终态补偿阻断继续建程
//（orchestration spec「统一终态补偿」：reap_failed 等 retryable debt/notice
// **持久化成功后**执行终态事务——落账的 debt MUST 直接进终态，MUST NOT 回到
// attempt 重试继续创建进程）；
// (2) notice 写失败（noticeCasErr failpoint）→ pendingCleanupError（durable debt 重放，
// spec「notice/debt 持久化失败 MUST NOT 执行终态事务」——isRecoveryTerminal 真，debt
// 保留 pending）。
//
// mutation 证据（已验证 → 红 → 还原）：Cleanup 分派删除（payload 仅丢失，原样返回
// StageError）→ notice 不落账/last_notice 空 + 错误链无 pendingCleanupError → 本
// 测试红（tickets 逃逸，残余进程无收割索引）。
func TestStageErrorMapping_CleanupTicketsNoticeOrDebt(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	payload := appagent.CleanupPayload{SessionName: runtimeSessionName("t1"),
		Reason: "reap_failed", Tickets: []string{"tk-c12"}, Retryable: true}

	// (1) notice 写成功 → retryable debt 已落账 → 终态阻断继续建程。
	err := m.supervisorStageErrorForTyped(appagent.CleanupStageError("supervisor.serve_rotate", payload, errors.New("rotate: disposition reap_failed")), "t1")
	if !isRecoveryTerminal(err) {
		t.Fatalf("notice 落账成功的 retryable cleanup debt MUST 判终态阻断继续建程（spec 统一终态补偿），got non-terminal: %v", err)
	}
	var rc *retryableCleanupError
	if !errors.As(err, &rc) {
		t.Fatalf("want retryableCleanupError, got %T", err)
	}
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Notice.String == "" || !contains(row.Notice.String, "tk-c12") {
		t.Fatalf("residual notice MUST carry tickets, got %q", row.Notice.String)
	}

	// (2) notice 写失败 → durable debt 形态（pendingCleanupError）。
	store.noticeCasErr = errors.New("notice cas down")
	err = m.supervisorStageErrorForTyped(appagent.CleanupStageError("supervisor.serve_rotate", payload, errors.New("rotate: disposition reap_failed")), "t1")
	if !isRecoveryTerminal(err) {
		t.Fatalf("notice 写失败 MUST 判终态阻断（debt 优先于重试），got non-terminal: %v", err)
	}
	var pce *pendingCleanupError
	if !errors.As(err, &pce) {
		t.Fatalf("want pendingCleanupError, got %T", err)
	}
}

// TestStageErrorMapping_RetryableStageRetries 固定输入：spawn 失败（op="supervisor.spawn"，
// RuntimeUnavailable，无 cleanup）。断言：原样返回（非 terminal）——普通 attempt 重试
// （attempt 重试语义）+ task 层 typed 检出（isRecoveryTerminal 假）。
// （C12 残余口径：仅 spawn/env/wait_ready 类「本次 attempt 失败，退避重试」保留重试；
// probe/claim/allocate/cleanup 按分派表进终态，见同文件各终态测试。）
func TestStageErrorMapping_RetryableStageRetries(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.supervisorStageErrorForTyped(
		appagent.NewStageError("supervisor.spawn", appagent.ErrKindRuntimeUnavailable,
			errors.New("runtime session: spawn exited")), "t1")
	if isRecoveryTerminal(err) {
		t.Fatalf("spawn 失败 MUST 走 attempt 重试（非终态），got terminal: %v", err)
	}
}

// TestStageErrorMapping_BindingInvalidTerminal 固定输入：binding 无效类 typed
// （ErrBindingInvalid——endpoint 派生失败/凭据缺失的 fail-closed 面）。断言：
// recoveryTerminalError（终态——恢复前置校验失败不可重试）。
func TestStageErrorMapping_BindingInvalidTerminal(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))
	err := m.supervisorStageErrorForTyped(
		appagent.NewStageError("supervisor.binding_endpoint", appagent.ErrKindBindingInvalid,
			errors.New("binding data: port missing")), "t1")
	if !isRecoveryTerminal(err) {
		t.Fatalf("binding invalid MUST be terminal, got non-terminal: %v", err)
	}
}
