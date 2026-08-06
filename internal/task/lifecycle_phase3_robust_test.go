package task

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/git"
)

// blockingLifecycleRunner 是可阻塞、context-aware 的 mock LifecycleRunner（Phase 3 稳定性测试用）。
// RunScript 阻塞直到 ctx 取消或 releaseCh 被关闭；记录 ctx 是否在 RunScript 执行期间被取消。
// canceledCh 在 ctx.Done 分支触发时关闭，供测试确定性等待 runnerCtx 取消传播（替代轮询 Sleep）。
// probeCtx 保存 RunScript 收到的 ctx，供测试同步断言 ctx.Err()（区分 runnerCtx / request ctx / signal ctx）。
type blockingLifecycleRunner struct {
	mu             sync.Mutex
	runScriptCalls []blockingRunCall
	startedCh      chan struct{}
	doneCh         chan struct{}
	releaseCh      chan struct{}
	canceledCh     chan struct{}
	probeCtx       context.Context
	ctxCanceled    bool
}

type blockingRunCall struct {
	dir     string
	script  string
	logPath string
	timeout time.Duration
}

func newBlockingLifecycleRunner() *blockingLifecycleRunner {
	return &blockingLifecycleRunner{
		startedCh:  make(chan struct{}, 1),
		releaseCh:  make(chan struct{}),
		canceledCh: make(chan struct{}),
	}
}

func (b *blockingLifecycleRunner) RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error {
	b.mu.Lock()
	b.runScriptCalls = append(b.runScriptCalls, blockingRunCall{dir: dir, script: script, logPath: logPath, timeout: timeout})
	b.doneCh = make(chan struct{})
	b.probeCtx = ctx
	select {
	case b.startedCh <- struct{}{}:
	default:
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if b.doneCh != nil {
			close(b.doneCh)
			b.doneCh = nil
		}
		b.mu.Unlock()
	}()

	select {
	case <-b.releaseCh:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		b.ctxCanceled = true
		b.mu.Unlock()
		close(b.canceledCh)
		return ctx.Err()
	}
}

// waitCtxCanceled 确定性等待 runnerCtx 取消传播到 RunScript（canceledCh 关闭）。
func (b *blockingLifecycleRunner) waitCtxCanceled(timeout time.Duration) bool {
	select {
	case <-b.canceledCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// currentProbeCtx 返回当前 RunScript 收到的 ctx（阻塞期间可用），供测试同步断言 ctx.Err()。
func (b *blockingLifecycleRunner) currentProbeCtx() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.probeCtx
}

// doneCh 返回当前 RunScript 的 doneCh（脚本退出时关闭）；nil 表示无活跃调用。
func (b *blockingLifecycleRunner) currentDoneCh() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.doneCh
}

func (b *blockingLifecycleRunner) CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string {
	return nil
}

func (b *blockingLifecycleRunner) waitStarted(timeout time.Duration) bool {
	select {
	case <-b.startedCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestShutdown_WaitsRunnerWG_BlockingMock：用阻塞 mock 证明 Shutdown 等待 WG、
// 取消传播、落账完成后才返回（design.md §6.1）。
func TestShutdown_WaitsRunnerWG_BlockingMock(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := newBlockingLifecycleRunner()
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending}
	m.startInitRunner("t1")
	if !runner.waitStarted(2 * time.Second) {
		t.Fatalf("RunScript did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- m.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownDone:
		// Shutdown 可能因其他清理返回错误，但必须已返回（runnerWG 已 Done）。
		t.Logf("Shutdown returned err: %v (acceptable)", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("Shutdown blocked waiting for runnerWG (script still running)")
	}
	runner.mu.Lock()
	canceled := runner.ctxCanceled
	runner.mu.Unlock()
	if !canceled {
		t.Fatalf("RunScript ctx must be canceled by Shutdown")
	}
	// 落账完成：init_status 应收敛为 failed（脚本被取消 → failed）。
	waitInitStatus(t, store, "t1", InitStatusFailed, 3*time.Second)
}

// TestShutdown_PreDeleteWG_HeldUntilFinalize：用阻塞 runner + store 闸门证明完整收敛链（design.md §6.1）。
// 正确取消序列（每步用 channel 同步，不依赖时序）：
//  1. 以可取消 request ctx 启动 Delete；
//  2. 等待 pre-delete 脚本入场（<-startedCh，RunScript 已被调用，阻塞在 runnerCtx 上）；
//  3. 此时 cancel request ctx → 断言脚本仍阻塞（ctxCanceled==false），证明入场与执行路径用的是 runnerCtx 而非 request ctx；
//  4. 触发 Shutdown（cancel runnerCtx）→ 脚本收到取消返回 ctx.Err()；
//  5. 脚本返回后 deleteResume 进入 finalizeOnFail → UpdateTaskStatus 阻塞在 finalizeGate 上；
//  6. finalize 用 Background+5s ctx（非 request ctx）落账成功 → last_error 保持 "pre-delete:" 前缀；
//  7. 落账完成后 WG 释放、Shutdown 返回。
//
// 反证：若入场/执行改用 request ctx，步骤 3 cancel request ctx 后脚本立即退出，
// ctxCanceled==true 断言失败；finalizeOnFail 会用已取消 ctx 落账失败，last_error 不会被写入，断言失败。
func TestShutdown_PreDeleteWG_HeldUntilFinalize(t *testing.T) {
	resetLifecycleCfgMock()
	store := newGateFinalizeStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := newBlockingLifecycleRunner()
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}

	// 步骤 1：以可取消 request ctx 启动 Delete（异步，pre-delete 脚本将阻塞在 runnerCtx 上）。
	reqCtx, reqCancel := context.WithCancel(context.Background())
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- m.Delete(reqCtx, tid, DeleteNormal, false)
	}()

	// 步骤 2：等待 pre-delete 脚本入场（RunScript 已被调用，阻塞在 runnerCtx 上）。
	// 必须在 cancel request ctx 之前观察到入场，否则 cancel 后 ctxCanceled 已设置，断言无法区分两种实现。
	if !runner.waitStarted(2 * time.Second) {
		t.Fatalf("pre-delete RunScript did not start")
	}

	// 步骤 3：cancel request ctx → 证明脚本用的是 runnerCtx 而非 request ctx。
	// 脚本应继续阻塞（未因 request ctx 取消而退出）。
	reqCancel()
	// 确定性同步断言：RunScript 收到的 probeCtx 必须仍有效（ctx.Err()==nil），
	// 证明脚本用 runnerCtx 而非 request ctx（request ctx 已取消但 probeCtx 未受影响）。
	// 反证：若生产代码把 request ctx 传给 runner，probeCtx.Err() != nil 断言失败。
	probeCtx := runner.currentProbeCtx()
	if probeCtx == nil {
		t.Fatalf("probeCtx must be captured after RunScript started")
	}
	if err := probeCtx.Err(); err != nil {
		t.Fatalf("pre-delete script must use runnerCtx, not request ctx (probeCtx canceled after reqCtx cancel: %v)", err)
	}
	// 脚本未退出：blockingLifecycleRunner 的 ctxCanceled 仍未设置（仅 runnerCtx 取消才设置）。
	runner.mu.Lock()
	canceledBeforeShutdown := runner.ctxCanceled
	runner.mu.Unlock()
	if canceledBeforeShutdown {
		t.Fatalf("pre-delete script must use runnerCtx, not request ctx (script exited after request ctx cancel)")
	}
	// finalize 未被触发（脚本仍阻塞）。
	if store.finalizeEntered {
		t.Fatalf("finalize must not start while script still blocking (request ctx cancel leaked into script path)")
	}

	// 步骤 4：触发 Shutdown：关 gate → cancel runnerCtx → wait runnerWG。
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- m.Shutdown(ctx)
	}()

	// 步骤 4 续：runnerCtx 取消传播到 RunScript → 脚本返回 ctx.Err()（确定性等待 canceledCh）。
	if !runner.waitCtxCanceled(2 * time.Second) {
		runner.mu.Lock()
		canceled := runner.ctxCanceled
		runner.mu.Unlock()
		if !canceled {
			t.Fatalf("runnerCtx cancel did not propagate to pre-delete RunScript")
		}
	}

	// 步骤 5：脚本返回后 deleteResume 进入 finalizeOnFail → UpdateTaskStatus 阻塞在 finalizeGate 上。
	if !store.waitFinalizeEntered(2 * time.Second) {
		t.Fatalf("finalize UpdateTaskStatus did not start (script may not have returned yet)")
	}

	// 步骤 6：Shutdown MUST NOT 返回（WG 仍持有，finalize 闸门未放行）。
	// finalize 用 Background+5s ctx（非 request ctx）落账，但闸门阻塞使其无法完成。
	select {
	case <-shutdownDone:
		t.Fatalf("Shutdown returned before finalize completed (WG released too early)")
	case <-time.After(100 * time.Millisecond):
		// 预期：Shutdown 仍阻塞。
	}

	// 步骤 6 续：放行 finalize 闸门 → UpdateTaskStatus 完成（Background ctx 落账成功）→
	// defer preDeleteRelease() 释放 WG → Shutdown 返回。
	close(store.finalizeGate)
	select {
	case <-shutdownDone:
		// Shutdown 在 finalize 完成后返回。
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown did not return after finalize released WG")
	}
	// 步骤 7：Delete 返回（落 deletion_failed，request ctx 已取消但 finalize 用 Background ctx 成功）。
	select {
	case <-deleteDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Delete did not return after finalize")
	}
	// last_error 保持 "pre-delete:" 前缀（finalize 用 Background ctx 成功写入，尽管 request ctx 已取消）。
	assertStatus(t, store, tid, StatusDeletionFailed)
	lastErrorContains(t, store, tid, "pre-delete:")
}

// gateFinalizeStore：UpdateTaskStatus 阻塞在 finalizeGate 上（模拟落账慢/阻塞），
// 用于证明 pre-delete WG 持有到落账完成（design.md §6.1）。
type gateFinalizeStore struct {
	*mockStore
	finalizeGate      chan struct{}
	finalizeEnteredMu sync.Mutex
	finalizeEntered   bool
	finalizeEnteredCh chan struct{}
}

func newGateFinalizeStore() *gateFinalizeStore {
	return &gateFinalizeStore{
		mockStore:         newMockStore(),
		finalizeGate:      make(chan struct{}),
		finalizeEnteredCh: make(chan struct{}),
	}
}

func (s *gateFinalizeStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error {
	// 仅 deletion_failed 落账走闸门（脚本失败后的 finalizeOnFail）。
	if status == StatusDeletionFailed {
		s.finalizeEnteredMu.Lock()
		if !s.finalizeEntered {
			s.finalizeEntered = true
			close(s.finalizeEnteredCh)
		}
		s.finalizeEnteredMu.Unlock()
		select {
		case <-s.finalizeGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.mockStore.UpdateTaskStatus(ctx, id, status, lastError)
}

// waitFinalizeEntered 确定性等待 finalize 落账开始（finalizeEnteredCh 关闭，替代轮询 Sleep）。
func (s *gateFinalizeStore) waitFinalizeEntered(timeout time.Duration) bool {
	select {
	case <-s.finalizeEnteredCh:
		return true
	case <-time.After(timeout):
		return false
	}
}
func (s *gateFinalizeStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// gatedLifecycleRunner 控制 RunScript 完成时机（确定性交叉竞态测试用）。
// gate != nil 时 RunScript 阻塞直到 gate 关闭或 ctx 取消；gate == nil 时立即返回 nil。
// 用于场景 A（阻塞 → 另一方看到 running 被拒）与场景 B（立即完成 → init_status 收敛 succeeded）。
type gatedLifecycleRunner struct {
	mu        sync.Mutex
	gate      chan struct{}
	calls     int
	ctxCanced bool
}

func newGatedLifecycleRunner(gate chan struct{}) *gatedLifecycleRunner {
	return &gatedLifecycleRunner{gate: gate}
}

func (g *gatedLifecycleRunner) RunScript(ctx context.Context, dir string, env map[string]string, script, logPath string, timeout time.Duration) error {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	if g.gate == nil {
		return nil // 场景 B：立即完成。
	}
	select {
	case <-g.gate:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		g.ctxCanced = true
		g.mu.Unlock()
		return ctx.Err()
	}
}

func (g *gatedLifecycleRunner) CopyInherited(ctx context.Context, repoPath, wtPath string, entries []git.FileStatus, patterns []string) []string {
	return nil
}

func (g *gatedLifecycleRunner) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestRerunInitVsActivate_MutexSerialized_Deterministic：用 channel 闸门控制脚本完成时机，
// 确定性覆盖 RerunInit vs Activate 的合法三态（design.md §3/§5）。
func TestRerunInitVsActivate_MutexSerialized_Deterministic(t *testing.T) {
	t.Run("scenarioA_rerunBlocks_activateSeesRunning", func(t *testing.T) {
		// 场景 A：RerunInit claim 后异步脚本阻塞（gate 不关闭）→ Activate 看到 running → 被拒。
		// 反证：若 keyed mutex 未串行化，Activate 可在 RerunInit claim 前获锁并读到 succeeded → 通过门禁，
		// actErr==nil 断言会失败（因我们串行调用 RerunInit 先返回，Activate 必然读到 running）。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		gate := make(chan struct{})
		runner := newGatedLifecycleRunner(gate)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		// 串行调用：RerunInit 同步返回（claim → running → 解锁 → 异步脚本阻塞在 gate 上）。
		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		// Activate 获锁后读到 running → 门禁拒绝（§5：running → invalid_state）。
		actErr := m.Activate(context.Background(), tid)
		if actErr == nil {
			t.Fatalf("Activate must be rejected when init_status=running, got nil err")
		}
		if OpErrorCode(actErr) != codeInvalidState {
			t.Fatalf("Activate must report invalid_state, got %q: %v", OpErrorCode(actErr), actErr)
		}
		// 终态合法：status=suspended, init_status=running（不变量 1：running ⇒ suspended）。
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusSuspended {
			t.Fatalf("status=%s want suspended (invariant 1: running ⇒ suspended)", row.Status)
		}
		if row.InitStatus != InitStatusRunning {
			t.Fatalf("init_status=%s want running (script still blocked)", row.InitStatus)
		}
		// 释放 gate 让异步脚本完成 → succeeded。
		close(gate)
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		m.runnerWG.Wait()
	})

	t.Run("scenarioB_rerunCompletes_activateSeesSucceeded", func(t *testing.T) {
		// 场景 B：RerunInit claim 后异步脚本立即完成（gate=nil）→ init_status 收敛 succeeded →
		// Activate 看到 succeeded → 合法通过（§5：succeeded 放行）。双 OK 合法。
		// 反证：若 init 门禁错误拒绝 succeeded（不放行），actErr != nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil) // 立即完成。
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		// 等待异步落账完成 → succeeded。
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		// Activate 看到 succeeded → 合法通过。
		actErr := m.Activate(context.Background(), tid)
		if actErr != nil {
			t.Fatalf("Activate must succeed when init_status=succeeded, got: %v", actErr)
		}
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusActive {
			t.Fatalf("status=%s want active (Activate succeeded)", row.Status)
		}
		if row.InitStatus != InitStatusSucceeded {
			t.Fatalf("init_status=%s want succeeded", row.InitStatus)
		}
		m.runnerWG.Wait()
	})

	t.Run("scenarioC_activateFirst_rerunRejected", func(t *testing.T) {
		// 场景 C：Activate 先持锁 → 推进 active → RerunInit 获锁后读到 active → invalid_state。
		// 反证：若 keyed mutex 未串行化，RerunInit 可在 Activate 提交前 claim → running，
		// 此时 rerunErr==nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		// Activate 先执行：succeeded → 放行 → active。
		actErr := m.Activate(context.Background(), tid)
		if actErr != nil {
			t.Fatalf("Activate: %v", actErr)
		}
		// RerunInit 获锁后读到 active → requires suspended → invalid_state。
		_, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr == nil {
			t.Fatalf("RerunInit must be rejected when status=active")
		}
		if OpErrorCode(rerunErr) != codeInvalidState {
			t.Fatalf("RerunInit must report invalid_state, got %q: %v", OpErrorCode(rerunErr), rerunErr)
		}
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusActive {
			t.Fatalf("status=%s want active", row.Status)
		}
	})
}

// TestRerunInitVsDelete_MutexSerialized_Deterministic：用 channel 闸门控制脚本完成时机，
// 确定性覆盖 RerunInit vs Delete 的合法终态（design.md §3/§6）。
func TestRerunInitVsDelete_MutexSerialized_Deterministic(t *testing.T) {
	t.Run("scenarioA_rerunBlocks_deleteSeesRunning", func(t *testing.T) {
		// 场景 A：RerunInit claim 后异步脚本阻塞 → Delete 看到 running → init 门禁拒绝。
		// 反证：若 Delete init 门禁不存在（不拒绝 running），delErr==nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		wtPath := t.TempDir()
		wt.products[wtPath] = true
		oc := newMockOC(true)
		gate := make(chan struct{})
		runner := newGatedLifecycleRunner(gate)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusFailed}

		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		// Delete 获锁后读到 running → init 门禁拒绝（§6：pending|running 拒绝删除）。
		delErr := m.Delete(context.Background(), tid, DeleteNormal, false)
		if delErr == nil {
			t.Fatalf("Delete must be rejected when init_status=running")
		}
		if OpErrorCode(delErr) != codeInvalidState {
			t.Fatalf("Delete must report invalid_state, got %q: %v", OpErrorCode(delErr), delErr)
		}
		// 终态合法：status=suspended, init_status=running（不变量 1）。
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusSuspended {
			t.Fatalf("status=%s want suspended (Delete rejected)", row.Status)
		}
		if row.InitStatus != InitStatusRunning {
			t.Fatalf("init_status=%s want running (script still blocked)", row.InitStatus)
		}
		// wt.Remove 未调用（Delete 在 init 门禁处返回，未进删除序列）。
		if got := wt.removeCalls(); got != 0 {
			t.Fatalf("wt.Remove must not be called, got %d", got)
		}
		close(gate)
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		m.runnerWG.Wait()
	})

	t.Run("scenarioB_rerunCompletes_deleteSeesSucceeded", func(t *testing.T) {
		// 场景 B：RerunInit claim 后异步脚本立即完成 → init_status 收敛 succeeded →
		// Delete 看到 succeeded → 合法通过（§6：succeeded 不在拒绝集）。双 OK 合法。
		// 反证：若 Delete init 门禁错误拒绝 succeeded，delErr != nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		wtPath := t.TempDir()
		wt.products[wtPath] = true
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil) // 立即完成。
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusFailed}

		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		// Delete 看到 succeeded → 合法通过。
		delErr := m.Delete(context.Background(), tid, DeleteNormal, false)
		if delErr != nil {
			t.Fatalf("Delete must succeed when init_status=succeeded, got: %v", delErr)
		}
		// 任务行被删除。
		if _, err := store.GetTask(context.Background(), tid); err == nil {
			t.Fatalf("task row must be deleted")
		}
		m.runnerWG.Wait()
	})

	t.Run("scenarioC_deleteFirst_rerunRejected", func(t *testing.T) {
		// 场景 C：Delete 先持锁 → 删除任务 → RerunInit 获锁后读到 not found。
		// 反证：若 keyed mutex 未串行化，RerunInit 可在 Delete 删除前 claim → running，
		// rerunErr==nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		wtPath := t.TempDir()
		wt.products[wtPath] = true
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusFailed}

		// Delete 先执行：succeeded/failed → 不在 init 拒绝集 → 删除成功。
		delErr := m.Delete(context.Background(), tid, DeleteNormal, false)
		if delErr != nil {
			t.Fatalf("Delete: %v", delErr)
		}
		// RerunInit 获锁后读到 not found（任务已删除）。
		_, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr == nil {
			t.Fatalf("RerunInit must fail when task is deleted")
		}
		if OpErrorCode(rerunErr) != codeNotFound {
			t.Fatalf("RerunInit must report not_found, got %q: %v", OpErrorCode(rerunErr), rerunErr)
		}
	})
}

// TestRerunInitVsArchive_MutexSerialized_Deterministic：用 channel 闸门控制脚本完成时机，
// 确定性覆盖 RerunInit vs Archive 的合法终态（design.md §3/§6）。
func TestRerunInitVsArchive_MutexSerialized_Deterministic(t *testing.T) {
	t.Run("scenarioA_rerunBlocks_archiveSeesRunning", func(t *testing.T) {
		// 场景 A：RerunInit claim 后异步脚本阻塞 → Archive 看到 running → init 门禁拒绝。
		// 反证：若 Archive init 门禁不存在（不拒绝 running），archErr==nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		gate := make(chan struct{})
		runner := newGatedLifecycleRunner(gate)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		// Archive 获锁后读到 running → init 门禁拒绝（§6：pending|running 拒绝归档）。
		archErr := m.Archive(context.Background(), tid)
		if archErr == nil {
			t.Fatalf("Archive must be rejected when init_status=running")
		}
		if OpErrorCode(archErr) != codeInvalidState {
			t.Fatalf("Archive must report invalid_state, got %q: %v", OpErrorCode(archErr), archErr)
		}
		// 终态合法：status=suspended, init_status=running（不变量 1）。
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusSuspended {
			t.Fatalf("status=%s want suspended (Archive rejected)", row.Status)
		}
		if row.InitStatus != InitStatusRunning {
			t.Fatalf("init_status=%s want running (script still blocked)", row.InitStatus)
		}
		close(gate)
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		m.runnerWG.Wait()
	})

	t.Run("scenarioB_rerunCompletes_archiveSeesSucceeded", func(t *testing.T) {
		// 场景 B：RerunInit claim 后异步脚本立即完成 → init_status 收敛 succeeded →
		// Archive 看到 succeeded → 合法通过（§6：succeeded 不在拒绝集）。双 OK 合法。
		// 反证：若 Archive init 门禁错误拒绝 succeeded，archErr != nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil) // 立即完成。
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		rerunRow, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr != nil {
			t.Fatalf("RerunInit: %v", rerunErr)
		}
		if rerunRow.InitStatus != InitStatusRunning {
			t.Fatalf("rerun row init_status=%s want running", rerunRow.InitStatus)
		}
		waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
		// Archive 看到 succeeded → 合法通过。
		archErr := m.Archive(context.Background(), tid)
		if archErr != nil {
			t.Fatalf("Archive must succeed when init_status=succeeded, got: %v", archErr)
		}
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusArchived {
			t.Fatalf("status=%s want archived", row.Status)
		}
		if row.InitStatus != InitStatusSucceeded {
			t.Fatalf("init_status=%s want succeeded", row.InitStatus)
		}
		m.runnerWG.Wait()
	})

	t.Run("scenarioC_archiveFirst_rerunRejected", func(t *testing.T) {
		// 场景 C：Archive 先持锁 → 归档 → RerunInit 获锁后读到 archived → requires suspended。
		// 反证：若 keyed mutex 未串行化，RerunInit 可在 Archive 归档前 claim → running，
		// rerunErr==nil 断言会失败。
		resetLifecycleCfgMock()
		store := newMockStore()
		seedLifecycleConfig(store, "p1", "", "echo init", "")
		store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
		proc := newMockProc()
		wt := newMockWorktree()
		oc := newMockOC(true)
		runner := newGatedLifecycleRunner(nil)
		m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
		tid := "t1"
		store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

		// Archive 先执行：succeeded → 不在 init 拒绝集 → 归档成功。
		archErr := m.Archive(context.Background(), tid)
		if archErr != nil {
			t.Fatalf("Archive: %v", archErr)
		}
		// RerunInit 获锁后读到 archived → requires suspended → invalid_state。
		_, rerunErr := m.RerunInit(context.Background(), tid)
		if rerunErr == nil {
			t.Fatalf("RerunInit must be rejected when status=archived")
		}
		if OpErrorCode(rerunErr) != codeInvalidState {
			t.Fatalf("RerunInit must report invalid_state, got %q: %v", OpErrorCode(rerunErr), rerunErr)
		}
		row, _ := store.GetTask(context.Background(), tid)
		if row.Status != StatusArchived {
			t.Fatalf("status=%s want archived", row.Status)
		}
	})
}

// --- 真实重叠竞态测试（claim-blocking store 桩 + 并发调用） ---
//
// 与上方 MutexSerialized_Deterministic 系列的区别：后者串行调用（先 RerunInit 返回再调
// Activate/Delete/Archive），不构成并发竞态证明。本组用 claim-blocking store 桩让
// RerunInit 在 ClaimInitRerun 临界区内阻塞（持锁），同时并发调用另一方，证明 keyed mutex
// 真正串行化并发请求并拒绝重叠（design.md §3/§5/§6）。

// claimBlockingStore 包装 mockStore：ClaimInitRerun 进入临界区后经 enteredCh 通知入场并
// 阻塞在 releaseCh 上（持锁状态下），用于构造 RerunInit 持锁 + 并发另一方的真实重叠。
type claimBlockingStore struct {
	*mockStore
	enteredCh chan struct{}
	releaseCh chan struct{}
}

func newClaimBlockingStore() *claimBlockingStore {
	return &claimBlockingStore{
		mockStore: newMockStore(),
		enteredCh: make(chan struct{}, 1),
		releaseCh: make(chan struct{}),
	}
}

func (s *claimBlockingStore) ClaimInitRerun(ctx context.Context, taskID string) (bool, error) {
	// 先执行真实 claim（置 running），再通知入场并阻塞——模拟"claim 已落库但 RerunInit
	// 尚未释放锁"的临界区窗口。
	claimed, err := s.mockStore.ClaimInitRerun(ctx, taskID)
	if err != nil {
		return claimed, err
	}
	select {
	case s.enteredCh <- struct{}{}:
	default:
	}
	<-s.releaseCh
	return claimed, nil
}
func (s *claimBlockingStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// commitBlockingStore 包装 mockStore：在指定状态提交方法上阻塞（Activate 的
// UpdateTaskStatusConditional / Delete 的 BeginDeleteIntent / Archive 的 ArchiveTask），
// 经 enteredCh 通知入场并阻塞在 releaseCh 上（持锁状态下），用于构造另一方持锁 +
// 并发 RerunInit 的真实重叠（反证 RerunInit 未 claim）。
type commitBlockingStore struct {
	*mockStore
	enteredCh chan struct{}
	releaseCh chan struct{}
	// blockMethod 控制阻塞哪个提交方法："conditional" | "beginDelete" | "archive"。
	blockMethod string
}

func newCommitBlockingStore(blockMethod string) *commitBlockingStore {
	return &commitBlockingStore{
		mockStore:   newMockStore(),
		enteredCh:   make(chan struct{}, 1),
		releaseCh:   make(chan struct{}),
		blockMethod: blockMethod,
	}
}

func (s *commitBlockingStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (bool, error) {
	if s.blockMethod == "conditional" && fromStatus == StatusSuspended && toStatus == StatusActivating {
		s.signalAndWait()
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func (s *commitBlockingStore) BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (bool, error) {
	if s.blockMethod == "beginDelete" {
		s.signalAndWait()
	}
	return s.mockStore.BeginDeleteIntent(ctx, id, mode, fromStatuses)
}

func (s *commitBlockingStore) ArchiveTask(ctx context.Context, id string) error {
	if s.blockMethod == "archive" {
		s.signalAndWait()
	}
	return s.mockStore.ArchiveTask(ctx, id)
}

func (s *commitBlockingStore) signalAndWait() {
	select {
	case s.enteredCh <- struct{}{}:
	default:
	}
	<-s.releaseCh
}
func (s *commitBlockingStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// rerunFinalState 断言合法终态三态集之一（design.md §3：rerunLost / otherLost / bothOK）。
//   - rerunLost：RerunInit claim 成功（init_status=succeeded），另一方被拒（status 未推进）。
//   - otherLost：另一方获胜（status 推进到 active/archived 或任务被删除），RerunInit 未 claim。
//   - bothOK：RerunInit claim 成功（init_status=succeeded）且另一方合法通过（status=active/archived）。
//
// 对 Delete 路径：otherLost/bothOK 均表现为任务行被删除（无法区分，两者都合法）；
// rerunLost 表现为任务行仍存在（init_status=succeeded, status=suspended）。
//
// 不变量：任何时刻不得存在 running+activating/deleting（调用方在闸门期间已断言）。
func assertRerunFinalState(t *testing.T, store TaskStore, tid string, other string) {
	t.Helper()
	row, err := store.GetTask(context.Background(), tid)
	if other == "deleted" {
		// 任务行被删除：otherLost 或 bothOK（两者均合法，无法从终态区分）。
		if err == nil {
			// 任务行仍存在：必须是 rerunLost（RerunInit claim 成功 succeeded，Delete 被拒）。
			if row.InitStatus != InitStatusSucceeded || row.Status != StatusSuspended {
				t.Fatalf("rerun overlap with Delete: illegal state status=%s init_status=%s (rerunLost requires suspended+succeeded, or task deleted)",
					row.Status, row.InitStatus)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("GetTask %s: %v", tid, err)
	}
	rerunOK := row.InitStatus == InitStatusSucceeded
	otherOK := row.Status == other
	switch {
	case rerunOK && !otherOK:
		// rerunLost：RerunInit claim 成功收敛 succeeded，另一方被拒（status 未推进到 other）。
	case !rerunOK && otherOK:
		// otherLost：另一方获胜推进到 other，RerunInit 未 claim（init_status 保持 failed/none）。
	case rerunOK && otherOK:
		// bothOK：RerunInit claim 成功 + 另一方合法通过（init_status=succeeded 放行）。
	default:
		t.Fatalf("rerun overlap with %s: illegal final state status=%s init_status=%s (must be one of rerunLost/otherLost/bothOK)",
			other, row.Status, row.InitStatus)
	}
}

// TestRerunInitVsActivate_Overlap_Concurrent：RerunInit 持锁（ClaimInitRerun 阻塞）时并发 Activate。
// 证明 keyed mutex 真正串行化并发请求：Activate 在 RerunInit 持锁期间被拒（409 busy），
// 无状态副作用（不置 activating），不变量 running+activating 从不同时存在。
// 反证：若 tryLockTask 未串行化，Activate 可在 RerunInit claim 后获锁并读 running→invalid_state
// 或 worse 直接置 activating（running+activating 并存），assertStatus/notActivated 断言失败。
func TestRerunInitVsActivate_Overlap_Concurrent(t *testing.T) {
	resetLifecycleCfgMock()
	store := newClaimBlockingStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := newGatedLifecycleRunner(nil) // 异步脚本立即完成（claim 释放后收敛 succeeded）
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}

	// 并发启动 RerunInit（异步）：claim 后阻塞在 ClaimInitRerun 临界区（持锁）。
	rerunDone := make(chan error, 1)
	go func() {
		_, err := m.RerunInit(context.Background(), tid)
		rerunDone <- err
	}()

	// 等待 RerunInit 进入 ClaimInitRerun 临界区（持锁，init_status 已置 running）。
	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("RerunInit did not enter ClaimInitRerun critical section")
	}

	// 不变量检查：RerunInit 持锁期间 init_status=running，status 必须仍 suspended（未 activating）。
	row, _ := store.GetTask(context.Background(), tid)
	if row.InitStatus != InitStatusRunning {
		t.Fatalf("invariant: ClaimInitRerun must set init_status=running, got %s", row.InitStatus)
	}
	if row.Status == StatusActivating {
		t.Fatalf("invariant violated: running+activating coexist (status=activating while init_status=running)")
	}

	// 并发调用 Activate：MUST 被 keyed mutex 拒绝（409 busy），无状态副作用。
	actErr := m.Activate(context.Background(), tid)
	if actErr == nil {
		t.Fatalf("Activate must be rejected (409 busy) while RerunInit holds lock")
	}
	if OpErrorCode(actErr) != codeConflict {
		t.Fatalf("Activate must report conflict (409 busy), got %q: %v", OpErrorCode(actErr), actErr)
	}
	// 无状态副作用：status 未推进到 activating。
	row, _ = store.GetTask(context.Background(), tid)
	if row.Status == StatusActivating {
		t.Fatalf("Activate must not set status=activating while RerunInit holds lock (no side effect)")
	}

	// 释放闸门：RerunInit claim 完成 → 释放锁 → 异步脚本立即完成（gate=nil）→ succeeded。
	close(store.releaseCh)
	// 等待 RerunInit 返回。
	select {
	case err := <-rerunDone:
		if err != nil {
			t.Fatalf("RerunInit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("RerunInit did not return after release")
	}
	// 等待异步脚本收敛 succeeded。
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
	m.runnerWG.Wait()
	// 合法终态：rerunLost（RerunInit claim 成功 succeeded，Activate 被拒 status=suspended）。
	assertRerunFinalState(t, store, tid, StatusActive)
	row, _ = store.GetTask(context.Background(), tid)
	if row.Status != StatusSuspended {
		t.Fatalf("rerunLost: status must be suspended (Activate rejected), got %s", row.Status)
	}
}

// TestRerunInitVsDelete_Overlap_Concurrent：RerunInit 持锁时并发 Delete。
// 证明 Delete 被 keyed mutex 拒绝（409 busy），无状态副作用（不置 deleting），
// 不变量 running+deleting 从不同时存在。
func TestRerunInitVsDelete_Overlap_Concurrent(t *testing.T) {
	resetLifecycleCfgMock()
	store := newClaimBlockingStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := newGatedLifecycleRunner(nil)
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusFailed}

	rerunDone := make(chan error, 1)
	go func() {
		_, err := m.RerunInit(context.Background(), tid)
		rerunDone <- err
	}()

	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("RerunInit did not enter ClaimInitRerun critical section")
	}

	// 不变量：running 时不得 deleting。
	row, _ := store.GetTask(context.Background(), tid)
	if row.InitStatus != InitStatusRunning {
		t.Fatalf("invariant: ClaimInitRerun must set init_status=running, got %s", row.InitStatus)
	}
	if row.Status == StatusDeleting {
		t.Fatalf("invariant violated: running+deleting coexist")
	}

	// 并发 Delete：MUST 被拒（409 busy），无状态副作用。
	delErr := m.Delete(context.Background(), tid, DeleteNormal, false)
	if delErr == nil {
		t.Fatalf("Delete must be rejected (409 busy) while RerunInit holds lock")
	}
	if OpErrorCode(delErr) != codeConflict {
		t.Fatalf("Delete must report conflict (409 busy), got %q: %v", OpErrorCode(delErr), delErr)
	}
	row, _ = store.GetTask(context.Background(), tid)
	if row.Status == StatusDeleting {
		t.Fatalf("Delete must not set status=deleting while RerunInit holds lock (no side effect)")
	}
	// wt.Remove 未调用（Delete 未进入删除序列）。
	if got := wt.removeCalls(); got != 0 {
		t.Fatalf("wt.Remove must not be called, got %d", got)
	}

	close(store.releaseCh)
	select {
	case err := <-rerunDone:
		if err != nil {
			t.Fatalf("RerunInit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("RerunInit did not return after release")
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
	m.runnerWG.Wait()
	// 合法终态：rerunLost（RerunInit claim 成功 succeeded，Delete 被拒 status=suspended）。
	assertRerunFinalState(t, store, tid, "deleted")
	row, _ = store.GetTask(context.Background(), tid)
	if row.Status != StatusSuspended {
		t.Fatalf("rerunLost: status must be suspended (Delete rejected), got %s", row.Status)
	}
}

// TestRerunInitVsArchive_Overlap_Concurrent：RerunInit 持锁时并发 Archive。
// 证明 Archive 被 keyed mutex 拒绝（409 busy），无状态副作用（不置 archived）。
func TestRerunInitVsArchive_Overlap_Concurrent(t *testing.T) {
	resetLifecycleCfgMock()
	store := newClaimBlockingStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := newGatedLifecycleRunner(nil)
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}

	rerunDone := make(chan error, 1)
	go func() {
		_, err := m.RerunInit(context.Background(), tid)
		rerunDone <- err
	}()

	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("RerunInit did not enter ClaimInitRerun critical section")
	}

	// 并发 Archive：MUST 被拒（409 busy），无状态副作用。
	archErr := m.Archive(context.Background(), tid)
	if archErr == nil {
		t.Fatalf("Archive must be rejected (409 busy) while RerunInit holds lock")
	}
	if OpErrorCode(archErr) != codeConflict {
		t.Fatalf("Archive must report conflict (409 busy), got %q: %v", OpErrorCode(archErr), archErr)
	}
	row, _ := store.GetTask(context.Background(), tid)
	if row.Status == StatusArchived {
		t.Fatalf("Archive must not set status=archived while RerunInit holds lock (no side effect)")
	}

	close(store.releaseCh)
	select {
	case err := <-rerunDone:
		if err != nil {
			t.Fatalf("RerunInit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("RerunInit did not return after release")
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)
	m.runnerWG.Wait()
	// 合法终态：rerunLost（RerunInit claim 成功 succeeded，Archive 被拒 status=suspended）。
	assertRerunFinalState(t, store, tid, StatusArchived)
	row, _ = store.GetTask(context.Background(), tid)
	if row.Status != StatusSuspended {
		t.Fatalf("rerunLost: status must be suspended (Archive rejected), got %s", row.Status)
	}
}

// TestActivateVsRerunInit_Overlap_Reverse：Activate 持锁（UpdateTaskStatusConditional 阻塞）时并发 RerunInit。
// 反向证明：另一方持锁时 RerunInit 被 keyed mutex 拒绝（409 busy），未 claim（init_status 未变 running）。
// 反证：若 tryLockTask 未串行化，RerunInit 可在 Activate 提交前 claim → init_status=running，
// 此时 rerunErr==nil 断言失败；init_status 保持 succeeded 断言也会失败。
func TestActivateVsRerunInit_Overlap_Reverse(t *testing.T) {
	resetLifecycleCfgMock()
	store := newCommitBlockingStore("conditional")
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// init_status=succeeded 让 Activate 通过 init 门禁到达 UpdateTaskStatusConditional。
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusSucceeded}

	// 并发启动 Activate（异步）：suspended→activating CAS 阻塞在 commitBlockingStore（持锁）。
	actDone := make(chan error, 1)
	go func() {
		actDone <- m.Activate(context.Background(), tid)
	}()

	// 等待 Activate 进入状态提交临界区（持锁，尚未 CAS）。
	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("Activate did not enter commit critical section")
	}

	// 并发 RerunInit：MUST 被 keyed mutex 拒绝（409 busy），未 claim。
	_, rerunErr := m.RerunInit(context.Background(), tid)
	if rerunErr == nil {
		t.Fatalf("RerunInit must be rejected (409 busy) while Activate holds lock")
	}
	if OpErrorCode(rerunErr) != codeConflict {
		t.Fatalf("RerunInit must report conflict (409 busy), got %q: %v", OpErrorCode(rerunErr), rerunErr)
	}
	// 未 claim：init_status 保持 succeeded（未被 ClaimInitRerun 置 running）。
	assertInitStatus(t, store, tid, InitStatusSucceeded)
	// 脚本未执行（RerunInit 未 claim）。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script when rejected by lock, got %d calls", runner.runScriptCallCount())
	}

	// 释放闸门：Activate CAS 完成 → 推进 activating（mock serve 可能失败回 suspended，但已提交）。
	close(store.releaseCh)
	select {
	case <-actDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Activate did not return after release")
	}
	// 合法终态：otherLost（Activate 获胜，RerunInit 未 claim init_status=succeeded）。
	assertInitStatus(t, store, tid, InitStatusSucceeded)
}

// TestDeleteVsRerunInit_Overlap_Reverse：Delete 持锁（BeginDeleteIntent 阻塞）时并发 RerunInit。
// 反向证明：Delete 持锁时 RerunInit 被拒（409 busy），未 claim。
func TestDeleteVsRerunInit_Overlap_Reverse(t *testing.T) {
	resetLifecycleCfgMock()
	store := newCommitBlockingStore("beginDelete")
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusFailed}

	delDone := make(chan error, 1)
	go func() {
		delDone <- m.Delete(context.Background(), tid, DeleteNormal, false)
	}()

	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("Delete did not enter commit critical section")
	}

	// 并发 RerunInit：MUST 被拒（409 busy），未 claim。
	_, rerunErr := m.RerunInit(context.Background(), tid)
	if rerunErr == nil {
		t.Fatalf("RerunInit must be rejected (409 busy) while Delete holds lock")
	}
	if OpErrorCode(rerunErr) != codeConflict {
		t.Fatalf("RerunInit must report conflict (409 busy), got %q: %v", OpErrorCode(rerunErr), rerunErr)
	}
	// 未 claim：init_status 保持 failed（门控期间任务仍存在，Delete 尚未提交）。
	assertInitStatus(t, store, tid, InitStatusFailed)
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script when rejected by lock, got %d calls", runner.runScriptCallCount())
	}

	close(store.releaseCh)
	select {
	case <-delDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Delete did not return after release")
	}
	// 合法终态：otherLost（Delete 获胜，任务被删除，RerunInit 未 claim）。
	if _, err := store.GetTask(context.Background(), tid); err == nil {
		t.Fatalf("otherLost: task must be deleted after Delete wins, got row exists")
	}
}

// TestArchiveVsRerunInit_Overlap_Reverse：Archive 持锁（ArchiveTask 阻塞）时并发 RerunInit。
// 反向证明：Archive 持锁时 RerunInit 被拒（409 busy），未 claim。
func TestArchiveVsRerunInit_Overlap_Reverse(t *testing.T) {
	resetLifecycleCfgMock()
	store := newCommitBlockingStore("archive")
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}

	archDone := make(chan error, 1)
	go func() {
		archDone <- m.Archive(context.Background(), tid)
	}()

	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("Archive did not enter commit critical section")
	}

	// 并发 RerunInit：MUST 被拒（409 busy），未 claim。
	_, rerunErr := m.RerunInit(context.Background(), tid)
	if rerunErr == nil {
		t.Fatalf("RerunInit must be rejected (409 busy) while Archive holds lock")
	}
	if OpErrorCode(rerunErr) != codeConflict {
		t.Fatalf("RerunInit must report conflict (409 busy), got %q: %v", OpErrorCode(rerunErr), rerunErr)
	}
	assertInitStatus(t, store, tid, InitStatusFailed)
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script when rejected by lock, got %d calls", runner.runScriptCallCount())
	}

	close(store.releaseCh)
	select {
	case <-archDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Archive did not return after release")
	}
	// 合法终态：otherLost（Archive 获胜 status=archived，RerunInit 未 claim init_status=failed）。
	assertInitStatus(t, store, tid, InitStatusFailed)
	assertStatus(t, store, tid, StatusArchived)
}

// TestRerunInitVsActivate_Overlap_BothOK：RerunInit claim 后异步脚本立即完成 → init_status=succeeded →
// Activate 获锁后看到 succeeded 合法通过（bothOK，design.md §3）。
// 确定性编排：claimBlockingStore 让 RerunInit 先 claim（持锁）→ 释放 → 脚本立即完成（gate=nil）→
// 等待 succeeded → 再调 Activate → 看到 succeeded 放行。证明 bothOK 是合法终态。
func TestRerunInitVsActivate_Overlap_BothOK(t *testing.T) {
	resetLifecycleCfgMock()
	store := newClaimBlockingStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := newGatedLifecycleRunner(nil) // 脚本立即完成
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}

	rerunDone := make(chan error, 1)
	go func() {
		_, err := m.RerunInit(context.Background(), tid)
		rerunDone <- err
	}()

	select {
	case <-store.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("RerunInit did not enter ClaimInitRerun critical section")
	}

	// 释放闸门：RerunInit claim 完成 → 释放锁 → 异步脚本立即完成 → succeeded。
	close(store.releaseCh)
	select {
	case err := <-rerunDone:
		if err != nil {
			t.Fatalf("RerunInit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("RerunInit did not return after release")
	}
	waitInitStatus(t, store, tid, InitStatusSucceeded, 3*time.Second)

	// Activate 获锁后看到 succeeded → 合法通过（bothOK）。
	actErr := m.Activate(context.Background(), tid)
	if actErr != nil {
		t.Fatalf("Activate must succeed when init_status=succeeded (bothOK), got: %v", actErr)
	}
	m.runnerWG.Wait()
	m.autoActivateWG.Wait()
	assertRerunFinalState(t, store, tid, StatusActive)
	row, _ := store.GetTask(context.Background(), tid)
	if row.Status != StatusActive {
		t.Fatalf("bothOK: status must be active, got %s", row.Status)
	}
	if row.InitStatus != InitStatusSucceeded {
		t.Fatalf("bothOK: init_status must be succeeded, got %s", row.InitStatus)
	}
}

// TestRerunInit_ClaimInitRerun_StoreError_ReleasesWG：ClaimInitRerun store error 路径
// 恰好一次释放 WG（Shutdown 不挂起）。覆盖 RerunInit 同步退出第 4 分支（crud.go:379-383）。
func TestRerunInit_ClaimInitRerun_StoreError_ReleasesWG(t *testing.T) {
	resetLifecycleCfgMock()
	store := &claimRerunErrStore{mockStore: newMockStore()}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}
	_, err := m.RerunInit(context.Background(), tid)
	if err == nil {
		t.Fatalf("RerunInit must fail when ClaimInitRerun store errors")
	}
	if OpErrorCode(err) != codeInternal {
		t.Fatalf("expected internal, got %q: %v", OpErrorCode(err), err)
	}
	// init_status 保持 failed（ClaimInitRerun 未成功）。
	assertInitStatus(t, store, tid, InitStatusFailed)
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script on ClaimInitRerun store error")
	}
	// Shutdown 不挂起（WG 已被同步退出路径释放）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown blocked: WG not released on ClaimInitRerun store error path")
	}
}

// claimRerunErrStore：ClaimInitRerun 返回 store error（模拟 DB 故障）。
type claimRerunErrStore struct {
	*mockStore
}

func (s *claimRerunErrStore) ClaimInitRerun(ctx context.Context, taskID string) (bool, error) {
	return false, fmt.Errorf("db claim error")
}
func (s *claimRerunErrStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// TestRerunInit_ClaimInitRerun_CASRowsZero_ShutdownNotHang：CAS rows=0 路径恰好一次释放 WG
// （Shutdown 不挂起）。覆盖 RerunInit 同步退出第 5 分支（crud.go:384-389）。
func TestRerunInit_ClaimInitRerun_CASRowsZero_ShutdownNotHang(t *testing.T) {
	resetLifecycleCfgMock()
	store := &claimRerunFailStore{mockStore: newMockStore()}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}
	_, err := m.RerunInit(context.Background(), tid)
	if err == nil {
		t.Fatalf("RerunInit must fail with conflict when ClaimInitRerun CAS rows=0")
	}
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("expected conflict, got %q: %v", OpErrorCode(err), err)
	}
	assertInitStatus(t, store, tid, InitStatusFailed)
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script on CAS rows=0")
	}
	// Shutdown 不挂起（WG 已被同步退出路径释放）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown blocked: WG not released on CAS rows=0 path")
	}
}

// TestInitRunner_FinishInitRun_BlockingGate_WGNotReleased：FinishInitRun 阻塞在 gate 内时
// runnerWG 计数不归零（落账完成后才释放）。证明 WG 覆盖完整 attempt，Done 在最终落账后。
func TestInitRunner_FinishInitRun_BlockingGate_WGNotReleased(t *testing.T) {
	resetLifecycleCfgMock()
	store := &finishBlockStore{
		mockStore:     newMockStore(),
		finishGate:    make(chan struct{}),
		finishEntered: make(chan struct{}, 1),
	}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending}
	m.startInitRunner(tid)
	// 等待脚本执行完成（mockLifecycleRunner 立即返回）。
	waitForScriptCalls(t, runner, 1, 2*time.Second)
	// 等待 FinishInitRun 进入闸门（阻塞在 finishGate 上，WG 仍持有）。
	select {
	case <-store.finishEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("FinishInitRun did not enter gate")
	}
	// Shutdown MUST NOT 返回（runnerWG 仍持有，FinishInitRun 阻塞在 gate 内）。
	shutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutDone <- m.Shutdown(ctx)
	}()
	select {
	case <-shutDone:
		t.Fatalf("Shutdown returned before FinishInitRun completed (WG released too early)")
	case <-time.After(100 * time.Millisecond):
		// 预期：Shutdown 仍阻塞（WG 未归零）。
	}
	// 释放闸门：FinishInitRun 完成 → WG 释放 → Shutdown 返回。
	close(store.finishGate)
	select {
	case <-shutDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown did not return after FinishInitRun released WG")
	}
}

// finishBlockStore：FinishInitRun 阻塞在 finishGate 上（模拟落账慢），证明 WG 持有到落账完成。
type finishBlockStore struct {
	*mockStore
	finishGate    chan struct{}
	finishEntered chan struct{}
}

func (s *finishBlockStore) FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (bool, error) {
	select {
	case s.finishEntered <- struct{}{}:
	default:
	}
	select {
	case <-s.finishGate:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.mockStore.FinishInitRun(ctx, taskID, status, initError)
}
func (s *finishBlockStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// TestDelete_BlockingDeleteTask_PreDeleteTokenHeld：成功删除路径 DeleteTask 阻塞在 gate 内时
// pre-delete token 未释放（持有至 DB 提交）。证明 pre-delete token 持有到 DB 提交点。
func TestDelete_BlockingDeleteTask_PreDeleteTokenHeld(t *testing.T) {
	resetLifecycleCfgMock()
	store := &deleteBlockStore{
		mockStore:     newMockStore(),
		deleteGate:    make(chan struct{}),
		deleteEntered: make(chan struct{}, 1),
	}
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}

	delDone := make(chan error, 1)
	go func() {
		delDone <- m.Delete(context.Background(), tid, DeleteNormal, false)
	}()

	// 等待 DeleteTask 进入闸门（pre-delete 脚本已完成，wt.Remove 已调用，DeleteTask 阻塞）。
	select {
	case <-store.deleteEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("DeleteTask did not enter gate")
	}
	// Shutdown MUST NOT 返回（pre-delete token 仍持有，DeleteTask 阻塞在 gate 内）。
	shutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutDone <- m.Shutdown(ctx)
	}()
	select {
	case <-shutDone:
		t.Fatalf("Shutdown returned before DeleteTask completed (pre-delete token released too early)")
	case <-time.After(100 * time.Millisecond):
		// 预期：Shutdown 仍阻塞（pre-delete token 未释放）。
	}
	// 释放闸门：DeleteTask 完成 → defer preDeleteRelease() 释放 token → Shutdown 返回。
	close(store.deleteGate)
	select {
	case <-delDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Delete did not return after DeleteTask released")
	}
	select {
	case <-shutDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown did not return after DeleteTask released WG")
	}
	// 任务行被删除（DeleteTask 成功提交）。
	if _, err := store.GetTask(context.Background(), tid); err == nil {
		t.Fatalf("task row must be deleted after Delete")
	}
}

// deleteBlockStore：DeleteTask 阻塞在 deleteGate 上（模拟 DB 删除慢），证明 pre-delete token
// 持有到 DB 提交点（defer preDeleteRelease() 在 DeleteTask 返回后才释放）。
type deleteBlockStore struct {
	*mockStore
	deleteGate    chan struct{}
	deleteEntered chan struct{}
}

func (s *deleteBlockStore) DeleteTask(ctx context.Context, id string) error {
	select {
	case s.deleteEntered <- struct{}{}:
	default:
	}
	select {
	case <-s.deleteGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.mockStore.DeleteTask(ctx, id)
}
func (s *deleteBlockStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// TestRerunInit_ClaimInitRerun_CASRowsZero：门禁通过但 ClaimInitRerun CAS 返回 rows=0
// （并发下已被 claim）→ conflict。
func TestRerunInit_ClaimInitRerun_CASRowsZero(t *testing.T) {
	resetLifecycleCfgMock()
	store := &claimRerunFailStore{mockStore: newMockStore()}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// 门禁通过（suspended+failed），但 ClaimInitRerun 返回 rows=0（模拟并发已 claim）。
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}
	_, err := m.RerunInit(context.Background(), tid)
	if err == nil {
		t.Fatalf("RerunInit must fail with conflict when ClaimInitRerun CAS rows=0")
	}
	if OpErrorCode(err) != codeConflict {
		t.Fatalf("expected conflict, got %q: %v", OpErrorCode(err), err)
	}
	// init_status 保持 failed（未被 RerunInit 修改，ClaimInitRerun 未成功）。
	assertInitStatus(t, store, tid, InitStatusFailed)
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("RerunInit must not run script on CAS rows=0")
	}
}

// claimRerunFailStore：ClaimInitRerun 总返回 false（模拟并发已 claim，CAS rows=0）。
type claimRerunFailStore struct {
	*mockStore
}

func (s *claimRerunFailStore) ClaimInitRerun(ctx context.Context, taskID string) (bool, error) {
	return false, nil // rows=0
}
func (s *claimRerunFailStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// TestRerunInit_ReturnsRunningRow：RerunInit 成功返回行 InitStatus=running（直接断言，无瞬态轮询）。
func TestRerunInit_ReturnsRunningRow(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusFailed}
	row, err := m.RerunInit(context.Background(), tid)
	if err != nil {
		t.Fatalf("RerunInit: %v", err)
	}
	if row.InitStatus != InitStatusRunning {
		t.Fatalf("returned row init_status=%s want running", row.InitStatus)
	}
	if row.InitError.Valid {
		t.Fatalf("returned row init_error must be cleared, got %v", row.InitError)
	}
	// 等待异步落账完成（runnerWG 释放），避免 running 泄漏到后续测试。
	m.runnerWG.Wait()
}

// TestRerunInit_GetTaskFailFallback：ClaimInitRerun 成功但后续 GetTask（独立 5s ctx 重试）也失败 →
// fallback 构造行 MUST InitStatus=running、InitError 清空、UpdatedAt 已刷新（=nowUnixI()，非 DB 旧值/非 ClaimInitRerun 值）。
// 反证：若 fallback 未清 InitError，row.InitError.Valid 断言失败；
// 若 fallback 未触发（第 2 次 GetTask 成功返回 ClaimInitRerun 写入的 UpdatedAt=12），row.UpdatedAt==12 断言失败；
// 若 fallback 保留旧 UpdatedAt（12345）而非刷新为 nowUnixI()，row.UpdatedAt==12345 断言失败；
// 若 fallback 用 DB 读取值而非构造，row.UpdatedAt 不在 [before,after] 窗口内断言失败。
func TestRerunInit_GetTaskFailFallback(t *testing.T) {
	resetLifecycleCfgMock()
	store := &getTaskFailAfterClaimStore{mockStore: newMockStore()}
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// 预置 failed + 旧 InitError（验证 fallback 清空旧错误）。
	// UpdatedAt=12345 是可识别的 DB 旧值——fallback MUST 刷新为 nowUnixI() 而非保留 12345。
	// ClaimInitRerun mock 会设 UpdatedAt=12——若 fallback 未触发（GetTask 成功），返回行 UpdatedAt=12。
	store.tasks[tid] = TaskRow{
		ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt",
		InitStatus: InitStatusFailed, InitError: sql.NullString{String: "old boom", Valid: true},
		UpdatedAt: 12345,
	}
	before := nowUnixI()
	row, err := m.RerunInit(context.Background(), tid)
	after := nowUnixI()
	if err != nil {
		t.Fatalf("RerunInit must succeed (fallback) even when post-claim GetTask fails: %v", err)
	}
	if row.InitStatus != InitStatusRunning {
		t.Fatalf("fallback row init_status=%s want running", row.InitStatus)
	}
	if row.InitError.Valid {
		t.Fatalf("fallback row init_error must be cleared, got %v", row.InitError)
	}
	// UpdatedAt MUST 被 nowUnixI() 刷新（≠ DB 旧值 12345，≠ ClaimInitRerun 写入的 12，且在 [before,after] 窗口内）。
	if row.UpdatedAt == 12345 {
		t.Fatalf("fallback row UpdatedAt must be refreshed (nowUnixI), not retained DB value 12345")
	}
	if row.UpdatedAt == 12 {
		t.Fatalf("fallback row UpdatedAt=12 indicates GetTask succeeded (fallback not triggered), want nowUnixI")
	}
	if row.UpdatedAt < before || row.UpdatedAt > after {
		t.Fatalf("fallback row UpdatedAt=%d must be in [before=%d, after=%d] window (nowUnixI refresh)", row.UpdatedAt, before, after)
	}
	// 等待异步落账完成（runnerWG 释放），避免 running 泄漏。
	m.runnerWG.Wait()
}

// getTaskFailAfterClaimStore：GetTask 第一次成功（门禁检查），第二次（claim 后独立 ctx 重试）失败。
// ClaimInitRerun mock 设 UpdatedAt=12——若第 2 次 GetTask 成功会返回 UpdatedAt=12，测试可据此区分 fallback 是否触发。
type getTaskFailAfterClaimStore struct {
	*mockStore
	mu         sync.Mutex
	getTaskCnt int
}

func (s *getTaskFailAfterClaimStore) GetTask(ctx context.Context, id string) (TaskRow, error) {
	s.mu.Lock()
	s.getTaskCnt++
	cnt := s.getTaskCnt
	s.mu.Unlock()
	if cnt >= 2 {
		return TaskRow{}, fmt.Errorf("simulated post-claim GetTask failure")
	}
	return s.mockStore.GetTask(ctx, id)
}
func (s *getTaskFailAfterClaimStore) seedProject(p ProjectRow) { s.mockStore.seedProject(p) }

// TestReconcile_Order_ConvergeBeforeRestore：Converge 在 ListAllTasks 之前。
func TestReconcile_Order_ConvergeBeforeRestore(t *testing.T) {
	resetLifecycleCfgMock()
	store := &orderTraceStore{mockStore: newMockStore()}
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newLifecycleTestManager(t, store, proc, wt, oc, &mockLifecycleRunner{})
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt1", InitStatus: InitStatusRunning}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	order := store.snapshot()
	convergeIdx, listIdx := -1, -1
	for i, name := range order {
		if name == "converge" {
			convergeIdx = i
		}
		if name == "listAll" {
			listIdx = i
		}
	}
	if convergeIdx < 0 {
		t.Fatalf("ConvergeInterruptedInitRuns not called")
	}
	if listIdx < 0 {
		t.Fatalf("ListAllTasks not called")
	}
	if convergeIdx >= listIdx {
		t.Fatalf("ConvergeInterruptedInitRuns (idx %d) must be before ListAllTasks (idx %d)", convergeIdx, listIdx)
	}
}

// orderTraceStore 包装 mockStore，记录方法调用顺序。
type orderTraceStore struct {
	*mockStore
	mu    sync.Mutex
	order []string
}

func (s *orderTraceStore) ConvergeInterruptedInitRuns(ctx context.Context) (int64, error) {
	s.mu.Lock()
	s.order = append(s.order, "converge")
	s.mu.Unlock()
	return s.mockStore.ConvergeInterruptedInitRuns(ctx)
}

func (s *orderTraceStore) ListAllTasks(ctx context.Context) ([]TaskRow, error) {
	s.mu.Lock()
	s.order = append(s.order, "listAll")
	s.mu.Unlock()
	return s.mockStore.ListAllTasks(ctx)
}

func (s *orderTraceStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// newLifecycleTestRepo 创建真实 git repo（参考 internal/git/git_test.go newTestRepo），
// 含 .gitignore + 一个 ignored 文件（.env）+ 一个 untracked 文件，供 ListIgnoredUntracked 枚举。
func newLifecycleTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "t@t.com")
	runGit("config", "user.name", "tester")
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("README.md", "init\n")
	writeFile(".gitignore", ".env\n")
	writeFile(".env", "SECRET=1\n")
	writeFile("untracked.txt", "x\n")
	runGit("add", "README.md", ".gitignore")
	runGit("commit", "-qm", "init")
	return dir
}

// TestRetryCreate_BothPaths_RerunInherit：worktree 复用与重建两条路径都重跑 inherit，
// 用真实 git repo 使枚举成功、CopyInherited 真正被调用，断言 copyCallCount==2。
func TestRetryCreate_BothPaths_RerunInherit(t *testing.T) {
	repoPath := newLifecycleTestRepo(t)
	for _, reuse := range []bool{true, false} {
		t.Run(fmt.Sprintf("reuse=%v", reuse), func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			seedLifecycleConfig(store, "p1", ".env", "", "")
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: repoPath, DefaultBranch: "main"})
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
			wtPath := "/data/worktrees/p1/t1"
			if reuse {
				wt.products[wtPath] = true
				wt.branches["ocdeck/mytask"] = true
			}
			store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Name: "mytask", Status: StatusCreationFailed, WorktreePath: wtPath, Branch: "ocdeck/mytask", InitStatus: InitStatusNone, BaseRef: "refs/heads/main"}
			if err := m.Retry(context.Background(), "t1", false); err != nil {
				t.Fatalf("Retry: %v", err)
			}
			// 真实 repo 枚举成功 → CopyInherited 被调用（匹配 .env）。
			if runner.copyCallCount() != 1 {
				t.Fatalf("retryCreate %v path must call CopyInherited once, got %d",
					map[bool]string{true: "reuse", false: "rebuild"}[reuse], runner.copyCallCount())
			}
		})
	}
}

// TestPreDelete_StatNonENOENT_DeletionFailed：制造真实非 ENOENT Stat 错误（ENOTDIR）→ deletion_failed。
func TestPreDelete_StatNonENOENT_DeletionFailed(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	// 制造 ENOTDIR：父路径是普通文件，worktree 路径在其下 → os.Stat 返回 ENOTDIR（非 ENOENT）。
	parentFile := filepath.Join(t.TempDir(), "regularfile")
	if err := os.WriteFile(parentFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(parentFile, "worktree") // parent 是文件 → Stat 子路径返回 ENOTDIR
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	err := m.Delete(context.Background(), tid, DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail on Stat ENOTDIR")
	}
	assertStatus(t, store, tid, StatusDeletionFailed)
	lastErrorContains(t, store, tid, "pre-delete:")
	// 脚本未执行（Stat 失败在 admission 前）。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("pre-delete must not run script on Stat ENOTDIR, got %d calls", runner.runScriptCallCount())
	}
}

// TestPreDelete_AdmissionFail_NoWorktreeRemove：触发真实 Shutdown 流程使 gate 关闭，
// 然后 Delete pre-delete admission 失败 → 停止删除序列、绝不 wt.Remove、落 deletion_failed。
// 反证：若 admission 失败后未停止删除序列（仍调用 wt.Remove），wt.removeCalls() != 0 断言失败；
// 若 admission 未真正走 admitPreDelete 路径（gate 未关），脚本会被执行，runScriptCallCount != 0 断言失败。
func TestPreDelete_AdmissionFail_NoWorktreeRemove(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir() // 真实存在（os.Stat 非 ENOENT）→ 进入 admission
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}

	// 触发真实 Shutdown 流程使 gate 关闭（shutdownStarted=true，runnerCtx 取消并置空）。
	// Shutdown 完成后（无 runner 运行，WG 立即完成），gate 保持关闭。
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = m.Shutdown(shutCtx) // 忽略清理错误（仅需 gate 关闭副作用）

	// 确认 gate 已通过真实 Shutdown 关闭。
	m.shutdownGateMu.Lock()
	gateClosed := m.shutdownStarted
	m.shutdownGateMu.Unlock()
	if !gateClosed {
		t.Fatalf("Shutdown did not close gate")
	}

	err := m.Delete(context.Background(), tid, DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail when pre-delete admission fails (gate closed)")
	}
	// wt.Remove 绝未调用（admission 失败停止删除序列）。
	if got := wt.removeCalls(); got != 0 {
		t.Fatalf("wt.Remove must not be called when pre-delete admission fails, got %d calls", got)
	}
	// 落 deletion_failed（token 覆盖范围内落账用非取消 ctx）。
	assertStatus(t, store, tid, StatusDeletionFailed)
	lastErrorContains(t, store, tid, "pre-delete:")
	// 任务行仍存在（未被删除）。
	assertTaskExists(t, store, tid)
	// 脚本未执行（admission 失败在脚本执行前）。
	if runner.runScriptCallCount() != 0 {
		t.Fatalf("pre-delete must not run script when admission fails, got %d calls", runner.runScriptCallCount())
	}
}

// TestPreDelete_WtRemoveSuccessDBFail_RetryConverges：wt.Remove 成功 DB 删除失败后 Retry 收敛（pre-delete 幂等跳过）。
func TestPreDelete_WtRemoveSuccessDBFail_RetryConverges(t *testing.T) {
	resetLifecycleCfgMock()
	store := &deleteFailStore{mockStore: newMockStore(), deleteFail: true}
	seedLifecycleConfig(store, "p1", "", "", "echo predelete")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	wtPath := t.TempDir()
	wt.products[wtPath] = true
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	tid := "t1"
	store.tasks[tid] = TaskRow{ID: tid, ProjectID: "p1", Status: StatusSuspended, WorktreePath: wtPath, InitStatus: InitStatusNone}
	// 首次 Delete：pre-delete 成功，wt.Remove 成功，DeleteTask 失败 → deletion_failed。
	err := m.Delete(context.Background(), tid, DeleteNormal, false)
	if err == nil {
		t.Fatalf("Delete must fail when DB delete fails")
	}
	assertStatus(t, store, tid, StatusDeletionFailed)
	if runner.runScriptCallCount() != 1 {
		t.Fatalf("pre-delete must run once, got %d", runner.runScriptCallCount())
	}
	// 模拟 wt.Remove 已成功：删除真实目录，使 Retry 时 os.Stat IsNotExist → pre-delete 幂等跳过。
	os.RemoveAll(wtPath)
	// Retry：删除序列重入。worktree 已不存在 → pre-delete 幂等跳过（不执行脚本）。
	store.deleteFail = false
	if err := m.Retry(context.Background(), tid, false); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if runner.runScriptCallCount() != 1 {
		t.Fatalf("pre-delete must be skipped on Retry (worktree gone), got %d calls", runner.runScriptCallCount())
	}
	if _, err := store.GetTask(context.Background(), tid); err == nil {
		t.Fatalf("task row must be deleted after Retry")
	}
}

// deleteFailStore：DeleteTask 第一次失败，第二次成功。
type deleteFailStore struct {
	*mockStore
	deleteFail bool
}

func (s *deleteFailStore) DeleteTask(ctx context.Context, id string) error {
	if s.deleteFail {
		return fmt.Errorf("db delete error")
	}
	return s.mockStore.DeleteTask(ctx, id)
}
func (s *deleteFailStore) seedProject(p ProjectRow) {
	s.mockStore.seedProject(p)
}

// TestSignalCtxEarlyCancel_DoesNotAffectRunnerCtx：signal ctx 提前取消不影响 runnerCtx。
// 用 context-aware 阻塞 runner（blockingLifecycleRunner）记录 RunScript 收到的 ctx，
// 确定性断言 runnerCtx 隔离：cancel SetLifecycleCtx 注入的 signal ctx 后，脚本收到的 ctx 仍有效
// （ctx.Err()==nil）；触发 Shutdown cancel runnerCtx 后，脚本 ctx 被取消（ctx.Err()!=nil）。
// 反证：若生产代码把 signal ctx 传给 runner，cancel signal ctx 后 probeCtx.Err() != nil 断言失败。
func TestSignalCtxEarlyCancel_DoesNotAffectRunnerCtx(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	seedLifecycleConfig(store, "p1", "", "echo init", "")
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := newBlockingLifecycleRunner()
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
	sigCtx, sigCancel := context.WithCancel(context.Background())
	m.SetLifecycleCtx(sigCtx)

	// 启动 InitRunner：脚本入场后阻塞在 runnerCtx 上（blockingLifecycleRunner 未 release）。
	store.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusPending}
	m.startInitRunner("t1")
	if !runner.waitStarted(2 * time.Second) {
		t.Fatalf("InitRunner RunScript did not start")
	}

	// cancel signal ctx → 脚本收到的 runnerCtx 不受影响（probeCtx 仍有效）。
	sigCancel()
	probeCtx := runner.currentProbeCtx()
	if probeCtx == nil {
		t.Fatalf("probeCtx must be captured after RunScript started")
	}
	if err := probeCtx.Err(); err != nil {
		t.Fatalf("runnerCtx must be isolated from signal ctx (probeCtx canceled after sigCtx cancel: %v)", err)
	}

	// 触发 Shutdown：cancel runnerCtx → 脚本 ctx 被取消（probeCtx.Err() != nil）。
	shutDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutDone <- m.Shutdown(ctx)
	}()
	// 确定性等待 runnerCtx 取消传播到 RunScript。
	if !runner.waitCtxCanceled(2 * time.Second) {
		t.Fatalf("runnerCtx cancel did not propagate to RunScript after Shutdown")
	}
	probeCtx2 := runner.currentProbeCtx()
	if probeCtx2 == nil {
		t.Fatalf("probeCtx must still be available after ctx canceled")
	}
	if err := probeCtx2.Err(); err == nil {
		t.Fatalf("runnerCtx must be canceled after Shutdown (probeCtx.Err()==nil)")
	}
	select {
	case <-shutDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Shutdown did not return after runnerCtx cancel")
	}
}

// TestAdmissionAfterSyncExit_ReleasesExactlyOnce：admission 后各同步退出恰好一次释放（Shutdown wait 不挂起）。
func TestAdmissionAfterSyncExit_ReleasesExactlyOnce(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*mockStore)
		wantErr bool
	}{
		{"taskNotFound", func(s *mockStore) {}, true},
		{"wrongStatus", func(s *mockStore) {
			s.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusActive, WorktreePath: "/wt", InitStatus: InitStatusFailed}
		}, true},
		{"wrongInitStatus", func(s *mockStore) {
			s.tasks["t1"] = TaskRow{ID: "t1", ProjectID: "p1", Status: StatusSuspended, WorktreePath: "/wt", InitStatus: InitStatusNone}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetLifecycleCfgMock()
			store := newMockStore()
			seedLifecycleConfig(store, "p1", "", "echo init", "")
			store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
			proc := newMockProc()
			wt := newMockWorktree()
			oc := newMockOC(true)
			runner := &mockLifecycleRunner{}
			m := newLifecycleTestManager(t, store, proc, wt, oc, runner)
			c.setup(store)
			_, err := m.RerunInit(context.Background(), "t1")
			if c.wantErr && err == nil {
				t.Fatalf("RerunInit must fail for %s", c.name)
			}
			// Shutdown 不应挂起（WG 已被同步退出路径释放）。
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- m.Shutdown(ctx) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("Shutdown blocked: WG not released on sync exit path %s", c.name)
			}
		})
	}
}

var _ = sql.NullString{}
var _ = strings.Contains

// TestShutdown_ClearsRunnerCtxFields：Shutdown 完成后 runnerCtx/runnerCancel 置空，
// 避免 Manager 复用时旧 cancel 与新 ctx 共存误杀复用后 runner 工作。
// 反证：若 Shutdown 未置空 runnerCancel，断言 runnerCancel != nil 失败；
// 若未置空 runnerCtx，断言 runnerCtx != nil 失败。
func TestShutdown_ClearsRunnerCtxFields(t *testing.T) {
	resetLifecycleCfgMock()
	store := newMockStore()
	store.seedProject(ProjectRow{ID: "p1", Name: "proj", Path: "/repo", DefaultBranch: "main"})
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	runner := &mockLifecycleRunner{}
	m := newLifecycleTestManager(t, store, proc, wt, oc, runner)

	// 构造时 runnerCtx/runnerCancel 非 nil（New 中初始化）。
	m.shutdownGateMu.Lock()
	ctxBefore := m.runnerCtx
	cancelBefore := m.runnerCancel
	m.shutdownGateMu.Unlock()
	if ctxBefore == nil || cancelBefore == nil {
		t.Fatalf("runnerCtx/runnerCancel must be initialized by New")
	}

	// Shutdown 完成（无 runner 运行，WG 立即完成）。
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = m.Shutdown(shutCtx)

	// Shutdown 后 runnerCtx/runnerCancel 置空。
	m.shutdownGateMu.Lock()
	ctxAfter := m.runnerCtx
	cancelAfter := m.runnerCancel
	m.shutdownGateMu.Unlock()
	if ctxAfter != nil {
		t.Fatalf("runnerCtx must be nil after Shutdown, got %v", ctxAfter)
	}
	if cancelAfter != nil {
		t.Fatalf("runnerCancel must be nil after Shutdown, got %v", cancelAfter)
	}
}
