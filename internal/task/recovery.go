package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/application/runtime"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

const (
	recoveryDebtPhaseCleanupNotice = "cleanup_notice"
	recoveryDebtPhaseComplete      = "complete"
	errRecoveryBudgetExhausted     = "recovery budget exhausted"
)

// recoveryDebtPersistTimeout 是 debt 落盘/删除用的独立短预算（G3-11：MUST NOT 复用
// 可能已超时的补偿 finalCtx；WithoutCancel + 新 bounded context）。
const recoveryDebtPersistTimeout = 10 * time.Second

var (
	errRecoveryTokenInvalid = errors.New("recovery token invalid")
	errRecoveryAbandon      = errors.New("recovery abandoned")
)

// recoveryFatal 是提交期 SSE fatal 通知（G3-1）。tok 为产生 fatal 的 runtime 令牌，
// 执行器据此过滤前序 attempt 已回滚 runtime 的陈旧 fatal。
type recoveryFatal struct {
	tok runtime.InstVersion
	err error
}

// recoveryIncident 登记一次进行中的恢复（G3-1/G3-7）：
//   - fatal 覆盖式 latch：SSE 永久失败先落入 incident 同步域，commitRuntimeReady 在
//     CAS 前后同点排空分派（CAS 前 → 本次 attempt 失败重试；CAS 后 → 幂等 ensureRecovery
//     开新 incident），杜绝「CAS 提交无 SSE 的 active」窗口；
//   - cancel 供 Shutdown 即时取消退避/attempt（D3 取消分派：不写状态，token 仍拥有
//     runtime 时反向清理，persist Shutdown 下存活进程由 reconcile 收敛）；
//   - ownedSession 标记当前 attempt 已创建、尚未提交/回退的 runtime 会话（G3-7）：
//     NewSession 到 commit 之间无注册 runtime，取消清理据此仍然可达该进程。
type recoveryIncident struct {
	fatalMu sync.Mutex
	fatal   *recoveryFatal
	cancel  context.CancelFunc

	ownedMu      sync.Mutex
	ownedSession string

	// attemptTok 记录本 incident 最近 attempt 创建的新 runtime token（经
	// commitRuntimeReady 的 onRegister 回调在 runtime 发布前显式写入，G3-14/G3-19）：
	// 回退/取消清理仅当注册表当前 token 精确匹配该值时执行——异代替换后零清理
	//（token 失效零清理契约，design.md:88-89）。
	attemptTokMu sync.Mutex
	attemptTok   runtime.InstVersion
}

func (inc *recoveryIncident) reportFatal(f recoveryFatal) {
	inc.fatalMu.Lock()
	inc.fatal = &f
	inc.fatalMu.Unlock()
}

// takeFatalMatching 取出 latch 中属于 tok 的 fatal 并清空；陈旧 token 的 fatal 直接丢弃。
func (inc *recoveryIncident) takeFatalMatching(tok runtime.InstVersion) error {
	inc.fatalMu.Lock()
	f := inc.fatal
	inc.fatal = nil
	inc.fatalMu.Unlock()
	if f == nil || f.tok != tok {
		return nil
	}
	return f.err
}

// claimProcess / releaseProcess / ownedProcess 维护 attempt 拥有的未提交进程标记
// （G3-7：取消清理在注册表无 runtime 时据此回退该进程）。
func (inc *recoveryIncident) claimProcess(sessionName string) {
	inc.ownedMu.Lock()
	inc.ownedSession = sessionName
	inc.ownedMu.Unlock()
}

func (inc *recoveryIncident) releaseProcess() {
	inc.ownedMu.Lock()
	inc.ownedSession = ""
	inc.ownedMu.Unlock()
}

func (inc *recoveryIncident) ownedProcess() string {
	inc.ownedMu.Lock()
	defer inc.ownedMu.Unlock()
	return inc.ownedSession
}

// noteAttemptToken / attemptToken 维护本 incident attempt 创建的 runtime token
// （G3-14/G3-19：由 recovery attempt 经 commitRuntimeReady 的 onRegister 回调在
// runtime 发布前显式绑定——不经 setRuntime 通用挂钩，避免误绑新代 token）。
func (inc *recoveryIncident) noteAttemptToken(tok runtime.InstVersion) {
	inc.attemptTokMu.Lock()
	inc.attemptTok = tok
	inc.attemptTokMu.Unlock()
}

func (inc *recoveryIncident) attemptToken() runtime.InstVersion {
	inc.attemptTokMu.Lock()
	defer inc.attemptTokMu.Unlock()
	return inc.attemptTok
}

// reportRuntimeFatal 是 SSE 永久失败的统一上报入口（G3-1，替代直接 go ensureRecovery）：
// 存在进行中 incident 时 fatal 先投递进其同步域（由执行器在 CAS 前后分派），随后仍异步
// ensureRecovery——activating 下 no-op（fatal 已由 incident 消费），active 下开新 incident；
// 两路径经同一 keyed mutex + CAS 幂等，不产生并发双恢复。
func (m *Manager) reportRuntimeFatal(taskID string, tok runtime.InstVersion, err error) {
	m.recoveryIncidentsMu.Lock()
	inc := m.recoveryIncidents[taskID]
	m.recoveryIncidentsMu.Unlock()
	if inc != nil {
		inc.reportFatal(recoveryFatal{tok: tok, err: err})
	}
	go m.ensureRecovery(taskID, tok)
}

// incidentFatalFor 排空任务当前 incident 的 fatal latch，返回属于 tok 的 fatal
// （commitRuntimeReady CAS 前后各调用一次，G3-1）。
func (m *Manager) incidentFatalFor(taskID string, tok runtime.InstVersion) error {
	m.recoveryIncidentsMu.Lock()
	inc := m.recoveryIncidents[taskID]
	m.recoveryIncidentsMu.Unlock()
	if inc == nil {
		return nil
	}
	return inc.takeFatalMatching(tok)
}

func (m *Manager) registerRecoveryIncident(taskID string, inc *recoveryIncident) {
	m.recoveryIncidentsMu.Lock()
	defer m.recoveryIncidentsMu.Unlock()
	if m.recoveryIncidents == nil {
		m.recoveryIncidents = map[string]*recoveryIncident{}
	}
	m.recoveryIncidents[taskID] = inc
}

// unregisterRecoveryIncident 仅在注册表仍指向本 incident 时移除（后继 incident 可能已替换）。
func (m *Manager) unregisterRecoveryIncident(taskID string, inc *recoveryIncident) {
	m.recoveryIncidentsMu.Lock()
	defer m.recoveryIncidentsMu.Unlock()
	if m.recoveryIncidents[taskID] == inc {
		delete(m.recoveryIncidents, taskID)
	}
}

// cancelAllRecoveryIncidents 即时取消全部进行中恢复（Shutdown 关 gate 后，G3-7）。
func (m *Manager) cancelAllRecoveryIncidents() {
	m.recoveryIncidentsMu.Lock()
	defer m.recoveryIncidentsMu.Unlock()
	for _, inc := range m.recoveryIncidents {
		inc.cancel()
	}
}

// hasActiveRecoveryIncident 报告该任务是否有进行中的 recovery incident
// （G3-16 ABA 防护：Activate 据此在旧 incident 注销前拒绝重入）。
func (m *Manager) hasActiveRecoveryIncident(taskID string) bool {
	m.recoveryIncidentsMu.Lock()
	defer m.recoveryIncidentsMu.Unlock()
	return m.recoveryIncidents[taskID] != nil
}

// wakeRecoveryIncident close 该任务当前唤醒通道（无等待者时 no-op，G3-7）。
// 等待者被唤醒后经 checkRecoveryContinuable 以 DB/注册表最新状态判定去留；
// 唤醒是"复核提示"，允许 spurious（incident 自身 CAS/setRuntime 也触发）。
func (m *Manager) wakeRecoveryIncident(taskID string) {
	m.recoveryWakeMu.Lock()
	defer m.recoveryWakeMu.Unlock()
	if ch, ok := m.recoveryWakes[taskID]; ok {
		close(ch)
		delete(m.recoveryWakes, taskID)
	}
}

// recoveryWakeCh 返回该任务当前唤醒通道；通道已关闭（已被消费）时换新。
func (m *Manager) recoveryWakeCh(taskID string) <-chan struct{} {
	m.recoveryWakeMu.Lock()
	defer m.recoveryWakeMu.Unlock()
	if ch, ok := m.recoveryWakes[taskID]; ok {
		select {
		case <-ch:
			// 已关闭：前一个等待者已消费，换新通道。
		default:
			return ch
		}
	}
	ch := make(chan struct{})
	m.recoveryWakes[taskID] = ch
	return ch
}

func (m *Manager) nowUnix() int64 {
	if m != nil && m.nowUnixFn != nil {
		return m.nowUnixFn()
	}
	return time.Now().Unix()
}

func recoveryBackoffForOrdinal(ordinal int) time.Duration {
	switch ordinal {
	case 1:
		return 5 * time.Second
	case 2:
		return 15 * time.Second
	default:
		return 45 * time.Second
	}
}

func (m *Manager) recoveryBackoff(ordinal int) time.Duration {
	if m != nil && m.recoveryBackoffFn != nil {
		return m.recoveryBackoffFn(ordinal)
	}
	return recoveryBackoffForOrdinal(ordinal)
}

// ensureRecovery 是 D3/D4 幂等恢复入口（watcher / SSE 共用）。
// 持任务锁后校验 token + 状态 + 项目 kind（G3-6：未知 kind / 项目不可读在 CAS 与
// 一切运行时副作用前零副作用放弃）；仅首个 incident CAS active→activating；
// activating 下重复调用 no-op。
// G3-9：准入（gate 检查 + recoveryWG.Add）在 shutdownGateMu 临界区内完成，
// Shutdown 的 WG barrier 覆盖整个 ensure 路径（含锁等待与 CAS），不存在
// 「已过 gate 检查但未 Add」的逃逸窗口；defer Done 收尾。
func (m *Manager) ensureRecovery(taskID string, tok runtime.InstVersion) {
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		return
	}
	m.recoveryWG.Add(1)
	m.shutdownGateMu.Unlock()
	defer m.recoveryWG.Done()

	unlock, err := m.lockTaskForRecovery(taskID)
	if err != nil {
		if !m.shutdownStartedLocked() {
			log.Printf("ensureRecovery: lock task %s: %v", taskID, err)
		}
		return
	}

	if m.shutdownStartedLocked() {
		unlock()
		return
	}
	rt := m.getRuntime(taskID)
	if rt == nil || rt.instVersion != tok {
		log.Printf("ensureRecovery: stale token (task %s instVersion=%s) skipped", taskID, tok)
		unlock()
		return
	}
	row, gerr := m.store.GetTask(context.Background(), taskID)
	if gerr != nil {
		log.Printf("ensureRecovery: get task %s: %v", taskID, gerr)
		unlock()
		return
	}
	switch row.Status {
	case StatusActivating:
		unlock()
		return
	case StatusActive:
		if err := rehydrateGuardView(row).ApplyRecoveryStart(); err != nil {
			unlock()
			return
		}
	default:
		unlock()
		return
	}
	// G3-6：有效模式解析在锁内、CAS 与一切副作用前（spec「session 归属捕获」：四个入口
	// 在任何状态修改或运行时副作用前 MUST 解析 kind+mode，非法组合零副作用）。
	proj, perr := m.store.GetProject(context.Background(), row.ProjectID)
	if perr != nil {
		log.Printf("ensureRecovery: get project for task %s: %v", taskID, perr)
		unlock()
		return
	}
	mode, kerr := resolveAlignMode(row, proj.Kind)
	if kerr != nil {
		log.Printf("ensureRecovery: resolve mode for task %s: %v", taskID, kerr)
		unlock()
		return
	}

	ctx := m.lifecycleCtx()
	// task-info-editable D2 仲裁表：运行期 Recovery 在状态 CAS 之前先执行 R1 收敛。
	// 不可收敛 → 跳过该任务本次恢复并记录 last_error（fail-closed 到任务级，不阻断其他任务）。
	if cerr := m.convergePendingBeforeLifecycle(ctx, row, proj); cerr != nil {
		log.Printf("ensureRecovery: task %s rename pending not converged; skip this recovery: %v", taskID, cerr)
		le := sql.NullString{String: cerr.Error(), Valid: true}
		if _, werr := m.writeStatus(ctx, taskID, StatusActive, le); werr != nil {
			log.Printf("ensureRecovery: task %s record last_error: %v", taskID, werr)
		}
		unlock()
		return
	}
	// 评审 I1：执行路径解析先于 active→activating CAS（零副作用准入）。承接 A
	//（active 存量）按 D4 生命周期矩阵 MUST NOT 写 activating：专用承接出口
	//（active→active 守卫，失败零状态副作用保留全部旧事实）；resolver 拒绝 →
	// 保持 active + last_error，不进恢复循环。
	if m.providerRegistry != nil {
		adoptA, perr := m.adoptBranchAPrecheck(ctx, taskID)
		if perr != nil {
			m.recordAdmissionError(ctx, taskID, perr)
			unlock()
			return
		}
		if adoptA {
			unlock()
			if aerr := m.adoptTaskV2(ctx, taskID, row, mode, false); aerr != nil {
				// I10：已提交但接管失败的恢复登记（提交事实不可回滚）。
				var pce *postCommitTakeoverError
				if errors.As(aerr, &pce) {
					m.registerPostCommitRecovery(taskID)
				}
				log.Printf("ensureRecovery: adopt A task %s: %v", taskID, aerr)
			}
			return
		}
	}
	// G3-18：激活准入原子拒绝未清 recovery debt（Complete 成功
	// 但删除失败遗留 / CAS mismatch 留存的旧 intent 不得误伤本次恢复）。
	cas, cerr := m.beginActivation(ctx, taskID, StatusActive)
	if cerr != nil {
		if errors.Is(cerr, store.ErrRecoveryDebtPresent) {
			log.Printf("ensureRecovery: task %s has uncleaned recovery debt; skip", taskID)
		} else {
			log.Printf("ensureRecovery: CAS active→activating task %s: %v", taskID, cerr)
		}
		unlock()
		return
	}
	if !cas.Matched {
		unlock()
		return
	}
	// CAS 完成后释放任务锁，attempt/退避不得阻塞 Suspend（D3：Suspend 先拿锁则恢复放弃）。
	unlock()
	m.runRecoveryIncident(ctx, taskID, tok, mode)
}

// ensureRecoveryFromAttach 是终端入口（ReopenAttach）触发的恢复（G4-1，source-aware）：
// callback 来源（watcher/SSE）强制 token 匹配现存 runtime；attach 来源在锁内重验
// active、会话仍缺失、kind 后，注册表无 runtime 时以**新分配** trigger 创建 incident
// （不得伪造/复用旧 runtime token 绕过校验）——否则 rt==nil 时任务永久卡 active。
// CAS 幂等（active→activating）保证与 watcher/其他 attach 并发触发不产生双恢复。
func (m *Manager) ensureRecoveryFromAttach(taskID string) {
	m.shutdownGateMu.Lock()
	if m.shutdownStarted {
		m.shutdownGateMu.Unlock()
		return
	}
	m.recoveryWG.Add(1)
	m.shutdownGateMu.Unlock()
	defer m.recoveryWG.Done()

	unlock, err := m.lockTaskForRecovery(taskID)
	if err != nil {
		if !m.shutdownStartedLocked() {
			log.Printf("ensureRecovery: attach lock task %s: %v", taskID, err)
		}
		return
	}
	if m.shutdownStartedLocked() {
		unlock()
		return
	}
	row, gerr := m.store.GetTask(context.Background(), taskID)
	if gerr != nil {
		log.Printf("ensureRecovery: attach get task %s: %v", taskID, gerr)
		unlock()
		return
	}
	switch row.Status {
	case StatusActivating:
		// 已有 incident 在恢复（watcher/先前触发）：attach 不重复介入。
		unlock()
		return
	case StatusActive:
		if err := rehydrateGuardView(row).ApplyRecoveryStart(); err != nil {
			unlock()
			return
		}
	default:
		unlock()
		return
	}
	// trigger 决策（锁内，无 TOCTOU）：注册表有 runtime → 与 callback 语义一致用其
	// token；无 runtime（G4-1 窗口：进程消失且注册表已清）→ 复核会话确实缺失后
	// 标记需新分配 trigger（G4-6：NewInstVersion 会改写权威 tombstone，MUST NOT
	// 先于 kind 校验/debt 准入/CAS——未知 kind 或未清 debt 本应零副作用）。
	// 实际分配延后到 beginActivation matched 之后、解锁之前。
	trigger := runtime.InstVersion("")
	needNewTrigger := false
	if rt := m.getRuntime(taskID); rt != nil {
		trigger = rt.instVersion
	} else {
		exists, herr := m.proc.HasSession(runtimeSessionName(taskID))
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			// infra 错误：零副作用放弃（不猜测），由 watcher/reconcile 路径兜底。
			log.Printf("ensureRecovery: attach has-session task %s: %v", taskID, herr)
			unlock()
			return
		}
		if exists {
			// 会话仍在而注册表无 runtime：状态撕裂（非本入口职责），零副作用放弃。
			unlock()
			return
		}
		needNewTrigger = true
	}
	proj, perr := m.store.GetProject(context.Background(), row.ProjectID)
	if perr != nil {
		log.Printf("ensureRecovery: attach get project for task %s: %v", taskID, perr)
		unlock()
		return
	}
	mode, kerr := resolveAlignMode(row, proj.Kind)
	if kerr != nil {
		log.Printf("ensureRecovery: attach resolve mode for task %s: %v", taskID, kerr)
		unlock()
		return
	}

	ctx := m.lifecycleCtx()
	// task-info-editable D2 仲裁表：attach 触发的运行期 Recovery 同样在状态 CAS 之前先执行
	// R1 收敛。不可收敛 → 跳过本次恢复并记录 last_error（不阻断其他任务）。
	if cerr := m.convergePendingBeforeLifecycle(ctx, row, proj); cerr != nil {
		log.Printf("ensureRecovery: attach task %s rename pending not converged; skip this recovery: %v", taskID, cerr)
		le := sql.NullString{String: cerr.Error(), Valid: true}
		if _, werr := m.writeStatus(ctx, taskID, StatusActive, le); werr != nil {
			log.Printf("ensureRecovery: attach task %s record last_error: %v", taskID, werr)
		}
		unlock()
		return
	}
	// 评审 I1：同 ensureRecovery——承接 A 先于 active→activating CAS 走专用承接
	// 出口（MUST NOT 写 activating）；resolver 拒绝零副作用（保持 active）。
	if m.providerRegistry != nil {
		adoptA, perr := m.adoptBranchAPrecheck(ctx, taskID)
		if perr != nil {
			m.recordAdmissionError(ctx, taskID, perr)
			unlock()
			return
		}
		if adoptA {
			unlock()
			if aerr := m.adoptTaskV2(ctx, taskID, row, mode, false); aerr != nil {
				// I10：已提交但接管失败的恢复登记（提交事实不可回滚）。
				var pce *postCommitTakeoverError
				if errors.As(aerr, &pce) {
					m.registerPostCommitRecovery(taskID)
				}
				log.Printf("ensureRecovery: attach adopt A task %s: %v", taskID, aerr)
			}
			return
		}
	}
	cas, cerr := m.beginActivation(ctx, taskID, StatusActive)
	if cerr != nil {
		if errors.Is(cerr, store.ErrRecoveryDebtPresent) {
			log.Printf("ensureRecovery: attach task %s has uncleaned recovery debt; skip", taskID)
		} else {
			log.Printf("ensureRecovery: attach CAS active→activating task %s: %v", taskID, cerr)
		}
		unlock()
		return
	}
	if !cas.Matched {
		unlock()
		return
	}
	// G4-6：CAS matched 后、解锁前才分配新 trigger——此前全部拒绝路径
	//（shutdown/token/状态/会话复核/kind/debt/CAS）保持零 tombstone 副作用。
	if needNewTrigger {
		trigger = m.runtimeRegistry.NewInstVersion(taskID)
	}
	unlock()
	m.runRecoveryIncident(ctx, taskID, trigger, mode)
}

// lockTaskForRecovery 为恢复路径阻塞等待任务锁：与 lockTaskForConverge 语义一致，
// 但等待可被 Shutdown 即时取消（shutdownCh，G3-9），不占满 convergeLockDeadline。
func (m *Manager) lockTaskForRecovery(taskID string) (func(), error) {
	v, _ := m.keyedMu.LoadOrStore(taskID, &keyedLock{})
	kl := v.(*keyedLock)
	if kl.mu.TryLock() {
		return func() { kl.mu.Unlock() }, nil
	}
	m.shutdownGateMu.Lock()
	shutdownCh := m.shutdownCh
	m.shutdownGateMu.Unlock()
	waitCtx, waitCancel := context.WithCancel(context.Background())
	defer waitCancel()
	deadline := time.NewTimer(convergeLockDeadline)
	defer deadline.Stop()
	lockedCh := make(chan struct{}) // unbuffered：锁所有权同步移交，无 TOCTOU 窗口
	go func() {
		kl.mu.Lock()
		select {
		case lockedCh <- struct{}{}:
			// 锁所有权已移交调用方。
		case <-waitCtx.Done():
			// 调用方放弃（Shutdown / deadline）：释放锁。
			kl.mu.Unlock()
		}
	}()
	select {
	case <-lockedCh:
		return func() { kl.mu.Unlock() }, nil
	case <-shutdownCh:
		waitCancel() // 移交 goroutine 经 waitCtx.Done 释放锁
		return nil, errTaskBusy
	case <-deadline.C:
		waitCancel()
		// 超时兜底：再试一次 tryLock（锁可能刚好释放）。
		if kl.mu.TryLock() {
			return func() { kl.mu.Unlock() }, nil
		}
		return nil, errTaskBusy
	}
}

func (m *Manager) shutdownStartedLocked() bool {
	m.shutdownGateMu.Lock()
	defer m.shutdownGateMu.Unlock()
	return m.shutdownStarted
}

// runRecoveryIncident 执行一次恢复 incident（G3-7：独立 cancel，WG 由 ensureRecovery
// 入口持有）。incident ctx 派生自生命周期 ctx，贯穿 prelude/attempt/退避/提交。
func (m *Manager) runRecoveryIncident(ctx context.Context, taskID string, trigger runtime.InstVersion, mode AlignMode) {
	incCtx, cancel := context.WithCancel(ctx)
	inc := &recoveryIncident{cancel: cancel}
	m.registerRecoveryIncident(taskID, inc)
	// 注册后复核 Shutdown：登记与 gate 关闭的竞态窗口内迟到的 incident 也必须立即让位
	//（D3 取消分派：不写状态，存活进程由 reconcile 收敛）。
	if m.shutdownStartedLocked() {
		m.unregisterRecoveryIncident(taskID, inc)
		cancel()
		return
	}
	defer func() {
		m.unregisterRecoveryIncident(taskID, inc)
		cancel()
	}()

	preludeErr, preludeObs := m.recoveryPrelude(incCtx, taskID, trigger)
	if preludeErr != nil {
		if errors.Is(preludeErr, errRecoveryTokenInvalid) || errors.Is(preludeErr, errRecoveryAbandon) {
			return
		}
		m.completeRecoveryFailure(incCtx, taskID, trigger, inc, preludeErr, foldPendingCleanups(preludeErr, preludeObs))
		return
	}

	var lastErr error
	var lastPendings []pendingCleanup
	for {
		if err := m.checkRecoveryContinuable(incCtx, taskID, trigger); err != nil {
			if errors.Is(err, errRecoveryTokenInvalid) || errors.Is(err, errRecoveryAbandon) {
				inc.releaseProcess()
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				m.cancelOwnedCleanup(context.WithoutCancel(incCtx), taskID, inc)
				return
			}
			m.completeRecoveryFailure(incCtx, taskID, trigger, inc, err, lastPendings)
			return
		}
		// D8 同代重拉：在 attempt（含 permit 与 backoff）之前从持久化快照加载同代 env。
		// 坏快照不可自愈：包装为 typed 终态走下方既有终态分派——零 permit、零 backoff、
		// 零 persist/进程副作用；ctx 取消经错误链被取消分支先行接住，语义不变。
		row, gerr := m.store.GetTask(incCtx, taskID)
		var env map[string]string
		if gerr == nil {
			env, gerr = m.loadEnvSnapshot(row)
		} else {
			gerr = fmt.Errorf("get task for env snapshot: %w", gerr)
		}
		var attemptErr error
		if gerr != nil {
			attemptErr = &recoveryTerminalError{err: newOpErr(codeInternal, gerr)}
		} else {
			attemptErr = m.runRecoveryAttempt(incCtx, taskID, trigger, mode, inc, env)
		}
		if attemptErr == nil {
			return
		}
		if pend := foldPendingCleanups(attemptErr, nil); len(pend) > 0 {
			lastPendings = append(lastPendings, pend...)
		}
		if errors.Is(attemptErr, errRecoveryTokenInvalid) || errors.Is(attemptErr, errRecoveryAbandon) {
			inc.releaseProcess()
			return
		}
		var handled *errActivateCommitHandled
		if errors.As(attemptErr, &handled) {
			// G3-2：handled 分支的 notice 重放失败 MUST 落 durable cleanup_notice debt
			//（完整载荷），不得仅记日志。
			m.replayHandledCleanupForRecovery(incCtx, taskID, attemptErr)
			return
		}
		if errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) {
			m.cancelOwnedCleanup(context.WithoutCancel(incCtx), taskID, inc)
			return
		}
		var srr *recoveryStopReportError
		if errors.As(attemptErr, &srr) {
			// I8/D5：确定性阶段失败——停止、保留现状（activating）、last_error
			// 提交用户决策；MUST NOT 终态补偿、不进预算循环。可区分于普通
			// attempt 错误（后者继续消耗预算重试）。
			le := sql.NullString{String: fmt.Sprintf("恢复阶段结果（保留现状，待用户决策）: %v", attemptErr), Valid: true}
			if _, werr := m.writeStatus(incCtx, taskID, StatusActivating, le); werr != nil {
				log.Printf("recovery stop-report task %s record last_error: %v", taskID, werr)
			}
			inc.releaseProcess()
			return
		}
		var pce *postCommitTakeoverError
		if errors.As(attemptErr, &pce) {
			// 评审 I10：提交后接管失败——任务已 active（提交不可回滚）；本轮
			// attempt 以「已完成+恢复债务」收口（禁普通补偿/禁终态），恢复工作
			// 实际登记为新 incident（调度闭环——下轮 Resume 重试接管）。
			log.Printf("recovery: task %s post-commit takeover failed, re-registering recovery: %v", taskID, attemptErr)
			inc.releaseProcess()
			m.registerPostCommitRecovery(taskID)
			return
		}
		if isRecoveryTerminal(attemptErr) {
			cause := attemptErr
			if lastErr != nil {
				cause = fmt.Errorf("%w; previous: %v", attemptErr, lastErr)
			}
			m.completeRecoveryFailure(incCtx, taskID, trigger, inc, cause, lastPendings)
			return
		}
		// G3-2：attempt 间清理结果不再 `_=` 丢弃——pending（notice 写失败）携带
		// pendings 转终态补偿；其余失败仅并入 last_error 聚合上下文。
		// G3-14：仅注册表为空（无新代 runtime）时按固定名确认终止——异代替换在位
		// 时零清理（token 失效零清理契约），下一轮 checkRecoveryContinuable 即 abandon。
		// G3-15：typed 终态清理错误（retryable/未知矛盾 disposition 或 infra 错误、
		// notice 已落库）MUST 立即终态补偿——预算尚余也不得再创建进程。
		if m.getRuntime(taskID) == nil {
			if cerr := m.confirmRuntimeTerminated(context.WithoutCancel(incCtx), taskID, runtimeSessionName(taskID)); cerr != nil {
				if pend := foldPendingCleanups(cerr, nil); len(pend) > 0 {
					cause := fmt.Errorf("%w; cleanup between attempts: %v", attemptErr, cerr)
					m.completeRecoveryFailure(incCtx, taskID, trigger, inc, cause, append(lastPendings, pend...))
					return
				}
				if isRecoveryTerminal(cerr) {
					cause := fmt.Errorf("%w; cleanup between attempts: %v", attemptErr, cerr)
					m.completeRecoveryFailure(incCtx, taskID, trigger, inc, cause, lastPendings)
					return
				}
				lastErr = errors.Join(attemptErr, cerr)
			} else {
				lastErr = attemptErr
			}
		} else {
			lastErr = attemptErr
		}
		inc.releaseProcess()
	}
}

// isRecoveryTerminal 按 typed 标记判定终态补偿分派（G3-5：替代错误文本匹配）。
// 分派四类（retry_attempt|terminal|pending|abandon）：
//   - retry_attempt：默认（NewSession 失败、serve 未就绪耗尽、提交各步失败、
//     env 快照/last_port 写失败等，D3 分派表）；
//   - terminal：本函数检出的 typed 标记 + 预算耗尽 sentinel；
//   - pending：pendingCleanupError（notice 写失败）——同为终态通道，由
//     completeRecoveryFailure 携带 pendings 处理（notice/debt 持久化成功才执行终态事务）；
//   - abandon：errRecoveryTokenInvalid / errRecoveryAbandon，调用方 errors.Is 先行分派。
func isRecoveryTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRecoveryBudgetExhaustedSentinel) {
		return true
	}
	var probe *capabilityProbeError
	if errors.As(err, &probe) {
		return true
	}
	var anchor *anchorStageError
	if errors.As(err, &anchor) {
		return true
	}
	var port *portAllocationError
	if errors.As(err, &port) {
		return true
	}
	var term *recoveryTerminalError
	if errors.As(err, &term) {
		return true
	}
	var rc *retryableCleanupError
	if errors.As(err, &rc) {
		return true
	}
	var pce *pendingCleanupError
	if errors.As(err, &pce) {
		return true
	}
	// C12 残余修复：supervisor StageError 终态分派经「阶段 + Kind」组合判定——
	// probe/claim/allocate 阶段或 Conflict/BindingInvalid Kind 均属终态补偿（specs
	// 逐阶段失败分派表）；Cleanup 载荷在 supervisorStageErrorForTyped 已转
	// retryable/pending typed。普通 RuntimeUnavailable（spawn/env/wait_ready/
	// persist_port）不在此检——按 attempt 重试分派表走重试。
	if se, ok := appagent.StageErrorOf(err); ok {
		switch {
		case se.Cleanup != nil:
			return true
		case se.Op == "supervisor.probe", se.Op == "supervisor.claim_anchor",
			se.Op == "supervisor.persist_anchor", se.Op == "supervisor.rebuild_disallowed",
			se.Op == "supervisor.allocate":
			return true
		case se.Kind == appagent.ErrKindConflict, se.Kind == appagent.ErrKindBindingInvalid:
			return true
		}
		return false
	}
	return false
}

// recoveryTerminalError 标记 recovery 专属路径的阶段失败定局为终态补偿（G3-5）。
// 共享路径的失败用各自具名标记（capabilityProbeError / anchorStageError /
// portAllocationError）， Activate 路径行为不受影响。
type recoveryTerminalError struct{ err error }

func (e *recoveryTerminalError) Error() string { return e.err.Error() }
func (e *recoveryTerminalError) Unwrap() error { return e.err }

// retryableCleanupError 标记清理（KillSession/轮换/确认终止/回退）产生了
// retryable / 未知矛盾 disposition 或 infra 错误且 notice 已落库（G3-2/G3-5）：
// Recovery 分派为终态补偿——retryable cleanup debt 在位时 MUST NOT 再创建进程；
// Activate 路径沿用既有补偿，行为不变。
type retryableCleanupError struct{ err error }

func (e *retryableCleanupError) Error() string { return e.err.Error() }
func (e *retryableCleanupError) Unwrap() error { return e.err }

var errRecoveryBudgetExhaustedSentinel = errors.New(errRecoveryBudgetExhausted)

// recoveryStopReportError 标记 v2 恢复的确定性阶段失败（D4/D5：server 可用性
// 独立重试耗尽 / 原 session 缺失——停止、保留现状、记 last_error、提交用户
// 决策；MUST NOT 终态补偿、MUST NOT 进入普通 attempt 预算循环——评审 I8）。
type recoveryStopReportError struct{ err error }

func (e *recoveryStopReportError) Error() string { return e.err.Error() }
func (e *recoveryStopReportError) Unwrap() error { return e.err }

func (m *Manager) checkRecoveryContinuable(ctx context.Context, taskID string, trigger runtime.InstVersion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.shutdownStartedLocked() {
		return context.Canceled
	}
	rt := m.getRuntime(taskID)
	if rt != nil && rt.instVersion != trigger {
		return errRecoveryTokenInvalid
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if row.Status != StatusActivating {
		return errRecoveryAbandon
	}
	return nil
}

// recoveryPrelude 一次性前序：token/状态复核 → 清理全部 shell 与残余 runtime。
// 返回 observations 供终态补偿归并未落库的 cleanup 意图（G3-2）。
func (m *Manager) recoveryPrelude(ctx context.Context, taskID string, trigger runtime.InstVersion) (error, []cleanupObservation) {
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err, nil
	}
	obsErr, observations := m.cleanupActivationRuntimeCollect(ctx, taskID)
	if obsErr != nil {
		return obsErr, observations
	}
	for _, o := range observations {
		if o.retryable {
			return fmt.Errorf("recovery prelude retryable cleanup debt: %s", o.reason), observations
		}
	}
	return nil, observations
}

// runRecoveryAttempt 可重复进程 attempt（D3/G3-4 原子序列：permit/backoff → 端口 →
// env 快照更新持久化 → 新密码 → NewSession → 健康+探测 → D5 bootstrap → 成功提交）。
// inc 承载 attempt-owned 进程标记（G3-7）：NewSession 前 claim，提交/回退确定后 release。
// env 是 runRecoveryIncident 在 permit 之前已校验的同代快照（D8）：本函数仅覆盖
// OCDECK_SERVE_PORT 后持久化，MUST NOT 调用 layerEnvSnapshot/mergeEnvSnapshot 重分层。
func (m *Manager) runRecoveryAttempt(ctx context.Context, taskID string, trigger runtime.InstVersion, mode AlignMode, inc *recoveryIncident, env map[string]string) error {
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err
	}
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	// 单一 resolver 准入先行（评审 I8：v2 决策先于 v1 permit/端口/env 解析——
	// 零副作用读取；v2 路径不产生任何 v1 attempt 副作用）。拒绝零副作用 +
	// last_error；R8（active 存量承接）经 adoptTaskV2（active→active 守卫 CAS）。
	var sup appagent.RuntimeSupervisor
	var dec execDecision
	if m.providerRegistry != nil {
		var aerr error
		sup, dec, _, _, aerr = m.admitExecution(ctx, taskID)
		if aerr != nil {
			m.recordAdmissionError(ctx, taskID, aerr)
			return aerr
		}
		if dec.Adopt {
			return m.adoptTaskV2(ctx, taskID, row, mode, false)
		}
		if dec.ExecProviderID == store.ProviderIDV2 {
			// v2 恢复（R1）：独立预算序（I8）——server 可用性（supervisor 独立
			// 有界重试）→ 任务 permit → Resume/握手/提交，见 runV2RecoveryAttempt。
			return m.runV2RecoveryAttempt(ctx, taskID, trigger, mode, inc, sup, row)
		}
	} else {
		sup = m.runtimeSupervisor
	}
	// permit 是 v1 attempt 首个消耗动作（D3：先于端口分配与任何进程副作用）；store 写失败属
	// 不可约边界 → 终态补偿（G3-5 typed）。
	permit, err := m.store.AcquireRecoveryPermit(ctx, taskID, m.nowUnix())
	if err != nil {
		return &recoveryTerminalError{err: newOpErr(codeInternal, fmt.Errorf("acquire recovery permit: %w", err))}
	}
	if !permit.Acquired {
		return fmt.Errorf("%w", errRecoveryBudgetExhaustedSentinel)
	}
	if err := m.waitRecoveryBackoff(ctx, taskID, trigger, permit.Ordinal); err != nil {
		return err
	}
	port, aerr := m.allocatePort(row.LastPort, 0)
	if aerr != nil {
		return &portAllocationError{err: newOpErr(codeConflict, aerr)}
	}
	// D8 同代快照：仅覆盖 OCDECK_SERVE_PORT 后持久化，不重算用户 env 与生命周期变量。
	env["OCDECK_SERVE_PORT"] = strconv.Itoa(port)
	if perr := m.persistEnvSnapshot(ctx, taskID, env); perr != nil {
		return newOpErr(codeInternal, &persistEnvSnapshotError{err: perr})
	}

	runtimeName := runtimeSessionName(taskID)
	// C2c（tasks 4.1）：派生凭据——同任务恒定（密码轮换随派生切换退役）；
	// 主密钥缺失 fail-closed（attempt 失败，不建进程）。
	password, derr := derivedServePassword(taskID)
	if derr != nil {
		return newOpErr(codeInternal, derr)
	}
	consumedFirst := true
	beforeCreate := func() error {
		if consumedFirst {
			consumedFirst = false
			// G3-16：首个 NewSession 前再校验——backoff 之后的端口分配/env 持久化
			// 步骤期间，状态可能已被并发收敛/改写（首轮退避仅 5s，晚于新代接管）。
			return m.checkRecoveryContinuable(ctx, taskID, trigger)
		}
		if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
			return err
		}
		p, perr := m.store.AcquireRecoveryPermit(ctx, taskID, m.nowUnix())
		if perr != nil {
			return &recoveryTerminalError{err: newOpErr(codeInternal, fmt.Errorf("acquire recovery permit: %w", perr))}
		}
		if !p.Acquired {
			return fmt.Errorf("%w", errRecoveryBudgetExhaustedSentinel)
		}
		return m.waitRecoveryBackoff(ctx, taskID, trigger, p.Ordinal)
	}
	// G3-7：进程即将创建——claim 归属本 incident（取消清理在注册表无 runtime 时
	// 仍可据此回退该进程）。
	inc.claimProcess(runtimeName)
	if sup != nil {
		// C12 门禁修复（supervisor 阶段错误桥接）：supervisor 建程失败分派到恢复
		// 失败矩阵（orchestration spec 逐阶段失败分派表）——probe→终态补偿
		//（capabilityProbeError）、claim 冲突→终态补偿（anchorStageError）、cleanup
		// 非净→pendingCleanupError（notice/debt 重放）、rebuild 拒绝→终态；
		// 其余 RuntimeUnavailable（spawn/env/allocate/wait_ready）→ 普通 attempt
		// 重试（原样返回）。映射函数 supervisorStageErrorForTyped见下方。
		// C2 门禁修复（design D9：恢复类入口经 Resume；BeforeLaunch=重拉预算闸门，
		// 闸门语义与 supPortRetries 轮换逐次对齐——每 attempt 入口已过 beforeCreate
		//，轮换/双启动正式进程前由 supervisor 经同一 BeforeLaunch 闭包再复核）。
		// 复用分支（tryReuseAliveTerminal）：会话仍在且健康 → 修复语义（不重建，
		// 零 permit 消耗在 supervisor 外已计入本 attempt）；old binding 取持久化值
		//（缺失/解析失败 → 零值 binding，复用分支被跳过，重建后新 binding 经
		// commitRuntimeReady 原子替换，与 D9 一致）。
		oldBinding, _, oldErr := m.store.GetTaskBinding(ctx, taskID)
		if oldErr != nil {
			// G3-F9 同源：binding 读取失败不得静默降级为「无 binding」。
			return oldErr
		}
		resumePolicy := appagent.ResumePolicy{AllowSessionCreate: true, AllowRebuild: true,
			BeforeLaunch: func(bctx context.Context) error {
				// supervisor 的 BeforeLaunch 契约携带 ctx；task 层闸门闭包沿用
				// attempt ctx（预算/退避不区分调用方 ctx 域）。
				_ = bctx
				return beforeCreate()
			}}
		binding, serr := sup.Resume(ctx, appagent.TaskRef{TaskID: taskID, Directory: row.WorktreePath},
			oldBinding, resumePolicy)
		if serr != nil {
			return m.supervisorStageErrorForTyped(serr, taskID)
		}
		port, password, _, eerr := m.bindingEndpoint(binding)
		if eerr != nil {
			// endpoint 解析失败 = binding 无效（BindingInvalid 契约，supervisor
			// StageError 契约同形）→ 终态补偿（isRecoveryTerminal 检出）。
			return m.supervisorStageErrorForTyped(
				appagent.NewStageError("supervisor.binding_endpoint", appagent.ErrKindBindingInvalid, eerr), taskID)
		}
		// C11 残余修复（orchestration spec :139「ctx 取消/Shutdown→仅当本 attempt
		// token 仍拥有 runtime 时执行反向清理」）：取消/abandon 分支保留 process
		// ownership（ownedSession 不清空）——上游 runRecoveryIncident 对 cancel 分支
		// 经 cancelOwnedCleanup 收口（注册表空分支读 inc.ownedProcess() 按名确认终止，
		// fail-closed；abandon 由后续 incident/prelude 处置）；token 失效保持既有
		// 零清理（releaseProcess 无碍——新接管者 prelude 负责清理）。
		// mutation 证据：取消分支 releaseProcess 保留 → cancelOwnedCleanup 检出
		// owned=="" → 确认终止被跳过 → 进程泄漏（TestC11_CancelBeforeRegisterKeepsOwnership 红）。
		if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
			if errors.Is(err, errRecoveryTokenInvalid) {
				inc.releaseProcess()
				return err
			}
			return err
		}
		cerr := m.commitRuntimeReady(ctx, taskID, row, runtimeName, port, password, &binding, mode, func(rt *taskRuntime) {
			inc.noteAttemptToken(rt.instVersion)
		})
		if cerr == nil {
			inc.releaseProcess()
			return nil
		}
		var handled *errActivateCommitHandled
		if errors.As(cerr, &handled) {
			// 反向清理已在 commitRuntimeReady 内按本 attempt token 完成。
			inc.releaseProcess()
			return cerr
		}
		// C11 门禁修复（orchestration spec「成功提交」提交各步失败=本次 attempt
		// 失败先做反向清理——与内联 bootstrapRuntime 失败收口同款）：token 匹配
		// 回退 / 注册表空+owned 进程确认终止；pending cleanup 挂回错误链。
		attemptTok := inc.attemptToken()
		if cur := m.getRuntime(taskID); cur != nil {
			if attemptTok != "" && cur.instVersion == attemptTok {
				if rbErr := m.rollbackAttemptRuntime(ctx, taskID, runtimeName, attemptTok); rbErr != nil {
					return mergeAttemptCleanupErr(cerr, rbErr)
				}
			}
		} else if inc.ownedProcess() != "" {
			if cfErr := m.confirmRuntimeTerminated(ctx, taskID, runtimeName); cfErr != nil {
				return mergeAttemptCleanupErr(cerr, cfErr)
			}
		}
		inc.releaseProcess()
		return cerr
	}
	port, password, berr := m.bootstrapRuntime(ctx, row, runtimeName, port, password, env, beforeCreate, true)
	if berr != nil {
		return berr
	}
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		if errors.Is(err, errRecoveryTokenInvalid) {
			// G3-14：token 失效零清理（固定会话名清理可能误杀新代 runtime，
			// design.md:88-89）——新接管者的 prelude 负责清理。
			inc.releaseProcess()
			return err
		}
		// abandon/ctx 取消：仅当注册表为空（无新代）且 owned 进程在位时确认终止
		//（G3-2 pending 上浮；ctx 取消路径随后由 cancelOwnedCleanup 幂等收口）。
		if cur := m.getRuntime(taskID); cur == nil && inc.ownedProcess() != "" {
			if cerr := m.confirmRuntimeTerminated(context.WithoutCancel(ctx), taskID, runtimeName); cerr != nil {
				return mergeAttemptCleanupErr(err, cerr)
			}
		}
		inc.releaseProcess()
		return err
	}
	// G3-19：runtime 发布前把本 attempt token 显式绑定到 inc（取消/回退据此
	// 精确匹配；不绑新代 token——Activate 等外部注册不经此回调）。
	// C3 门禁修复（design D9 提交协议「恢复类入口的新绑定原子替换旧绑定也在该
	// 事务内」+ specs「终端引用失效重建绑定」）：恢复重建成功后把最终 runtime 的
	// 完整 binding 传入提交事务原子替换——恢复会轮换端口/重建锚定（本次 attempt
	// 未成功前旧 binding 保持原值：提交事务 CAS 失配/失败零写入），legacy 旧 binding
	// 亦经派生凭据重建替换。mutation 证据锚定见 spi_recovery_wiring_test.go
	// TestRecovery_ReplacesBindingAfterRebuild（旧断言「恢复不重写 binding」已随
	// C3 反转——重建锚定/换代后 binding 必须替换，残留旧端口会断开后续连接）。
	newBinding := m.activationBinding(taskID, row.WorktreePath, runtimeName, port)
	cerr := m.commitRuntimeReady(ctx, taskID, row, runtimeName, port, password, &newBinding, mode, func(rt *taskRuntime) {
		inc.noteAttemptToken(rt.instVersion)
	})
	if cerr == nil {
		// 提交成功：进程由注册表 runtime 拥有，取消清理走 token 匹配回退。
		inc.releaseProcess()
		return nil
	}
	var handled *errActivateCommitHandled
	if errors.As(cerr, &handled) {
		// 反向清理已在 commitRuntimeReady 内按本 attempt token 完成。
		inc.releaseProcess()
		return cerr
	}
	// G3-2/G3-14：rollback/confirm 结果不再 `_=` 丢弃——pending cleanup 挂回错误链，
	// 由终态补偿统一重放（notice 写失败 MUST 阻断 CompleteRecoveryFailure）。
	// 回退仅当注册表当前 token 精确匹配本 attempt token（setRuntime 挂钩记录）：
	// 异代替换后零清理；注册表为空（未注册/已回退）且 owned 进程在位时按名确认终止。
	attemptTok := inc.attemptToken()
	if cur := m.getRuntime(taskID); cur != nil {
		if attemptTok != "" && cur.instVersion == attemptTok {
			if rbErr := m.rollbackAttemptRuntime(ctx, taskID, runtimeName, attemptTok); rbErr != nil {
				return mergeAttemptCleanupErr(cerr, rbErr)
			}
		}
	} else if inc.ownedProcess() != "" {
		if cfErr := m.confirmRuntimeTerminated(ctx, taskID, runtimeName); cfErr != nil {
			return mergeAttemptCleanupErr(cerr, cfErr)
		}
	}
	inc.releaseProcess()
	return cerr
}

// providerErrorKind 解析 provider 边界错误分类（评审 I8：公共 Kind 契约——
// *StageError 与 *Error 同 Kind 语义；真实 supervisor 路径多返回 NewError，
// 仅认 *StageError 会漏分类）。
func providerErrorKind(err error) (appagent.ErrorKind, bool) {
	var ae *appagent.Error
	if errors.As(err, &ae) {
		return ae.Kind, true
	}
	if se, ok := appagent.StageErrorOf(err); ok {
		return se.Kind, true
	}
	return "", false
}

// runV2RecoveryAttempt 是 v2 任务的恢复 attempt（评审 I8 预算顺序；R1 延续）：
// ①server 可用性（supervisor 独立有界重试——不消耗任务预算；耗尽 → 停止报告，
// 保留现状提交用户决策，MUST NOT 进普通 attempt 循环）→ ②任务 permit + backoff
// （恰在 server 可用后、session 验证 attempt 前消耗一次）→ ③Resume 延续原
// session（BeforeLaunch 仅取消/继续检查不扣款；原 session 缺失 = 确定性历史
// 事实——停止报告，不创建替代）→ ④候选关联握手（四就绪缺一不提交）→ ⑤
// binding 替换 + activating→active 单事务 → ⑥正式接管（失败=已提交恢复债务）。
func (m *Manager) runV2RecoveryAttempt(ctx context.Context, taskID string, trigger runtime.InstVersion, mode AlignMode, inc *recoveryIncident, sup appagent.RuntimeSupervisor, row TaskRow) error {
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err
	}
	// ① server 可用性：supervisor 独立预算承担（任务预算零消耗）。未实现该能力
	// 的 supervisor 形态 fail-closed（不猜 server 状态）。
	warmer, ok := sup.(appagent.ServerAvailabilityWarmer)
	if !ok {
		return newOpErr(codeInternal, errors.New("v2 supervisor 未实现 server 可用性检查（CheckServerAlive）"))
	}
	if serr := warmer.CheckServerAlive(ctx); serr != nil {
		return &recoveryStopReportError{err: fmt.Errorf("server 可用性失败（supervisor 独立重试耗尽，保留现状待用户决策）: %w", serr)}
	}
	// ② 任务 permit：恰在 server 可用后、session 验证 attempt 前消耗一次（D4）。
	permit, err := m.store.AcquireRecoveryPermit(ctx, taskID, m.nowUnix())
	if err != nil {
		return &recoveryTerminalError{err: newOpErr(codeInternal, fmt.Errorf("acquire recovery permit: %w", err))}
	}
	if !permit.Acquired {
		return fmt.Errorf("%w", errRecoveryBudgetExhaustedSentinel)
	}
	if err := m.waitRecoveryBackoff(ctx, taskID, trigger, permit.Ordinal); err != nil {
		return err
	}
	// ③ Resume 延续原 session（v2 attempt 不创建任务进程——不 claim v1 runtime 名）。
	oldBinding, hasOld, oldErr := m.store.GetTaskBinding(ctx, taskID)
	if oldErr != nil {
		// G3-F9 同源：binding 读取失败不得静默降级为「无 binding」。
		return oldErr
	}
	binding, serr := sup.Resume(ctx, appagent.TaskRef{TaskID: taskID, Directory: row.WorktreePath},
		oldBinding, appagent.ResumePolicy{AllowSessionCreate: true, AllowRebuild: true,
			BeforeLaunch: func(bctx context.Context) error {
				// v2 BeforeLaunch 仅取消/继续条件检查（D4 替换表：不含扣款——
				// permit 已于上一步消耗）。
				return m.checkRecoveryContinuable(bctx, taskID, trigger)
			}})
	if serr != nil {
		// 评审 I8：公共 Kind 分类（*StageError 与 *Error 同 Kind 契约——真实
		// Resume 404 返回 NewError，仅认 *StageError 会漏分类烧预算）。
		switch kind, ok := providerErrorKind(serr); {
		case ok && kind == appagent.ErrKindNotFound:
			// 原 session 缺失（D5 恢复序列行 3：不可读/不存在/不可继续使用）：
			// 停止、保留现状、提交用户决策——不创建替代 session、不进预算循环。
			return &recoveryStopReportError{err: fmt.Errorf("原 session %s 缺失（停止，提交用户决策）: %w", oldBinding.UpstreamSessionID, serr)}
		case ok && kind == appagent.ErrKindCapabilityUnavailable:
			// 配置应用阶段能力缺失（env seam 未注入等）：确定性失败——另行区分，
			// 不进预算循环（终态补偿出口）。
			return &recoveryTerminalError{err: newOpErr(codeInternal, fmt.Errorf("v2 恢复能力缺失（确定性）: %w", serr))}
		}
		return m.supervisorStageErrorForTyped(serr, taskID)
	}
	port, password, _, eerr := m.bindingEndpoint(binding)
	if eerr != nil {
		// endpoint 解析失败 = binding 无效 → 终态补偿（isRecoveryTerminal 检出）。
		return m.supervisorStageErrorForTyped(
			appagent.NewStageError("supervisor.binding_endpoint", appagent.ErrKindBindingInvalid, eerr), taskID)
	}
	// ④⑤⑥（批次 1 I10 序列）：候选握手（SSE 首帧先于原子提交；失败关候选保留
	// 旧事实，attempt 按既有矩阵重试——提交未发生，status 仍 activating 可继续）
	// → binding 替换 + activating→active 单事务 → 正式接管（失败记恢复债务）。
	astore := m.adoptionStoreOf()
	if astore == nil {
		return newOpErr(codeInternal, errors.New("store 未实现 v2 恢复/替换提交能力"))
	}
	es, herr := m.candidateHandshake(ctx, taskID, binding)
	if herr != nil {
		return m.supervisorStageErrorForTyped(
			appagent.NewStageError("recovery.v2.handshake", appagent.ErrKindRuntimeUnavailable, herr), taskID)
	}
	if _, rerr := astore.CommitV2Resume(ctx, taskID, oldBindingIf(hasOld, oldBinding), binding, row.Status); rerr != nil {
		if es != nil {
			_ = es.Close()
		}
		return newOpErr(codeConflict, fmt.Errorf("v2 恢复替换提交: %w", rerr))
	}
	// 候选流复用为正式接管流（评审 I10：保留缓冲事件，不重订阅）。接管失败 =
	// 已提交分类（postCommitTakeoverError），incident 循环登记恢复工作。
	// taskLocked=false：incident 不持任务锁——接管注册临界区由 commitV2RuntimeReady
	// 自行取锁（评审 I15 串行边界）。
	return m.commitV2RuntimeReady(ctx, taskID, row, port, password, binding, mode, es, false)
}

// supervisorStageErrorForTyped 把 supervisor 阶段错误映射为 task 层 typed 分派
// （C12 门禁修复：恢复失败矩阵逐阶段分派，task 层不 import infrastructure 错误
// 类型——契约面是 application/agentprovider.StageError + CleanupPayload）。
// 分派（orchestration spec「逐阶段失败分派表」）：
//   - probe 阶段（能力探测任何失败）→ capabilityProbeError → 终态补偿；
//   - claim_anchor 阶段（claim 冲突/写失败）→ anchorStageError → 终态补偿；
//   - 携带 CleanupPayload 的阶段（release/verify/kill 门禁/轮换/确认终止的
//     非净 disposition 或 infra 失败）→ notice 落账成功 → retryableCleanupError
//     （终态阻断继续建程）；落账失败 → pendingCleanupError（notice/debt 重放）；
//   - rebuild 拒绝（AllowRebuild=false 的修复完成语义由 supervisor 侧判定，
//     恢复入口恒 AllowRebuild=true 不可达——防御归类终态）；
//   - 其余（spwan/env/allocate/wait_ready RuntimeUnavailable）→ 原样返回，
//     普通 attempt 重试。
func (m *Manager) supervisorStageErrorForTyped(err error, taskID string) error {
	if err == nil {
		return nil
	}
	se, ok := appagent.StageErrorOf(err)
	if !ok {
		// 基础 typed（无阶段语义）：按 Kind 粗分派。
		switch {
		case errors.Is(err, appagent.ErrBindingInvalid):
			return &recoveryTerminalError{err: err}
		default:
			return err
		}
	}
	switch {
	// C12 门禁修复（mutation 证据：cleanup 分派旁路 `&& false`——tickets 不落
	// notice/debt 直接逃逸，TestStageErrorMapping_CleanupTicketsNoticeOrDebt 红
	//「residual notice MUST carry tickets」；残余进程无收割索引）。
	case se.Cleanup != nil:
		// cleanup 非净：先落 residual notice（debt 落账）。
		// C12 残余修复（spec「统一终态补偿」：reap_failed 等 retryable debt/notice
		// **持久化成功后**执行终态事务）：落账成功 → retryableCleanupError（终态，
		// 阻断继续建程；completeRecoveryFailure 的 replay 以 notice 已在位幂等）；失败 →
		// pendingCleanupError（notice/debt 持久化失败 MUST NOT 执行终态事务）。
		nctx, ncancel := withResidualNoticeCtx(context.Background())
		defer ncancel()
		noticeErr := m.recordResidualNotice(nctx, taskID, se.Cleanup.SessionName,
			se.Cleanup.Tickets, se.Cleanup.Reason, se.Cleanup.Retryable)
		if noticeErr == nil {
			return &retryableCleanupError{err: err}
		}
		return recordNoticeOrPending(se.Cleanup.SessionName, se.Cleanup.Tickets,
			se.Cleanup.Reason, se.Cleanup.Retryable, noticeErr, err)
	case se.Op == "supervisor.probe":
		// C12 门禁修复（mutation 证据：probe 分派删除→原样返回走 attempt 重试，
		// TestStageErrorMapping_ProbeFailsTerminal 红——预算被烧、spec 违约）。
		return &capabilityProbeError{err: fmt.Errorf("capability probe: %w", err)}
	case se.Op == "supervisor.claim_anchor", se.Op == "supervisor.persist_anchor":
		return &anchorStageError{err: err}
	case se.Op == "supervisor.rebuild_disallowed":
		return &recoveryTerminalError{err: err}
	case se.Kind == appagent.ErrKindBindingInvalid:
		// binding 无效类阶段失败（binding_endpoint 派生失败/凭据缺失的 fail-closed
		// 面）——恢复前置校验失败不可重试（终态补偿）。
		return &recoveryTerminalError{err: err}
	case se.Kind == appagent.ErrKindConflict:
		// conflict 类阶段失败（锚定 CAS mismatch 等）——终态补偿。
		return &recoveryTerminalError{err: err}
	default:
		return err
	}
}

// mergeAttemptCleanupErr 把 attempt 后清理失败挂回主错误（G3-2）：清理产生的
// pendingCleanupError 保留在链上（foldPendingCleanups / isRecoveryTerminal 可检出），
// 文本聚合供 last_error 诊断。
func mergeAttemptCleanupErr(main, cleanupErr error) error {
	return fmt.Errorf("%w; cleanup: %w", main, cleanupErr)
}

// waitRecoveryBackoff 按 ordinal 退避；除 ctx/timer 外监听状态/token invalidation
// 唤醒（G3-7：setRuntime/clearRuntime/状态写入触发 wakeRecoveryIncident），唤醒后
// 即时复核 continuation——状态已离 activating 或 token 失效则立即取消，不等满 timer。
// G3-16：全部退出路径（初始 d≤0、wake、timer 到期）统一「先订阅新通道→再复核→
// 检查通道」——wake 会关闭并删除通道，复核期间到达的 wake 落在已订阅的新通道上，
// 退出前未消费的关闭通道触发追加复核（信号不吞、不睡满退避）。
func (m *Manager) waitRecoveryBackoff(ctx context.Context, taskID string, trigger runtime.InstVersion, ordinal int) error {
	d := m.recoveryBackoff(ordinal)
	// 订阅→复核（初始路径）：注册前已发生的失效由复核兜底（通道收不到注册前的 close）。
	wakeCh := m.recoveryWakeCh(taskID)
	// exitRecheck：退避结束（或 d≤0）退出前的「检查通道→追加复核」——复核期间
	// 到达的信号不吞：通道未关 → 直接退出；已关 → 消费后再复核一次。
	exitRecheck := func() error {
		select {
		case <-wakeCh:
			return m.checkRecoveryContinuable(ctx, taskID, trigger)
		default:
			return nil
		}
	}
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err
	}
	if d <= 0 {
		return exitRecheck()
	}
	start := time.Now()
	remaining := d
	for {
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// 订阅→复核→检查通道（timer 到期路径，与 wake 路径同序）。
			wakeCh = m.recoveryWakeCh(taskID)
			if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
				return err
			}
			return exitRecheck()
		case <-wakeCh:
			timer.Stop()
			// 订阅→复核（唤醒路径）：先换新通道再复核——复核期间的 wake 落在
			// 新通道上，不丢失；复核失败立即取消，否则按剩余时长回到 select。
			wakeCh = m.recoveryWakeCh(taskID)
			if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
				return err
			}
			remaining = d - time.Since(start)
			if remaining <= 0 {
				return exitRecheck()
			}
		}
	}
}

// cancelOwnedCleanup 是取消路径（ctx 取消/Shutdown）的统一回退（G3-7/G3-14）：
//   - 注册表 runtime token 精确匹配本 incident 的 attemptTok → 完整 rollback；
//   - 注册表为异代 runtime（token 不匹配）→ 零清理（token 失效零清理契约，
//     固定会话名清理可能误杀新代 runtime）；
//   - 注册表为空且本 incident 仍登记、ownedSession 非空（NewSession 到 commit
//     之间）→ 按名称回退该进程（C11 残余修复：runRecoveryAttempt 的取消出口保留
//     ownership——不 releaseProcess——本收口才能按 owned 名执行同款 confirm）。
//
// 回退产生的 pending（notice 写失败）落 durable debt，不静默。
func (m *Manager) cancelOwnedCleanup(ctx context.Context, taskID string, inc *recoveryIncident) {
	attemptTok := inc.attemptToken()
	if cur := m.getRuntime(taskID); cur != nil {
		if attemptTok != "" && cur.instVersion == attemptTok {
			if err := m.rollbackAttemptRuntime(ctx, taskID, runtimeSessionName(taskID), attemptTok); err != nil {
				log.Printf("ensureRecovery: cancel rollback task %s: %v", taskID, err)
			}
		}
		// 异代替换或 attemptTok 未记录（prelude 后未创建 runtime）：零清理。
		inc.releaseProcess()
		return
	}
	m.recoveryIncidentsMu.Lock()
	stillOwned := m.recoveryIncidents[taskID] == inc
	m.recoveryIncidentsMu.Unlock()
	owned := inc.ownedProcess()
	if !stillOwned || owned == "" {
		return
	}
	if err := m.confirmRuntimeTerminated(ctx, taskID, owned); err != nil {
		log.Printf("ensureRecovery: cancel owned cleanup task %s: %v", taskID, err)
		if pend := foldPendingCleanups(err, nil); len(pend) > 0 {
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryDebtPersistTimeout)
			defer dcancel()
			if perr := m.persistRecoveryCleanupDebts(dctx, taskID, pend, err.Error()); perr != nil {
				log.Printf("ensureRecovery: persist cancel cleanup debt task %s: %v", taskID, perr)
			}
		}
	}
	inc.releaseProcess()
}

// replayHandledCleanupForRecovery 处理 CAS-mismatch handled 分支的 pending 重放
// （G3-2）：重放失败 MUST 落 durable cleanup_notice debt（完整载荷，含 tickets/
// reason/retryable/cause），不得仅记日志后丢失。
func (m *Manager) replayHandledCleanupForRecovery(ctx context.Context, taskID string, attemptErr error) {
	pendings := foldPendingCleanups(attemptErr, nil)
	if len(pendings) == 0 {
		return
	}
	replayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationFinalizeTimeout)
	defer cancel()
	if rerr := m.replayPendingCleanups(replayCtx, taskID, pendings); rerr != nil {
		log.Printf("recovery: handled pending replay task %s: %v", taskID, rerr)
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryDebtPersistTimeout)
		defer dcancel()
		if perr := m.persistRecoveryCleanupDebts(dctx, taskID, pendings, attemptErr.Error()); perr != nil {
			log.Printf("recovery: persist handled cleanup debt task %s: %v", taskID, perr)
		}
	}
}

// completeRecoveryFailure 统一终态补偿（D3/G3-2/G3-3/G3-7/G3-11）。补偿前重验
// （Shutdown / 状态已变 → 不写状态）；运行时清理按 KillResult disposition 表处置；
// pendings 为 cause 链与前序 attempt 收集的未落库 cleanup 意图，重放失败或 notice
// 写失败 MUST 阻断 CompleteRecoveryFailure 并落 durable tagged debt。
// Complete 经 write-intent-first 协议执行（G3-11）：先 durable upsert complete
// intent，成功/CAS mismatch 后删除；intent 落盘失败 → 不执行 Complete，保留内存
// 重试队列由后台/Shutdown 收敛。
func (m *Manager) completeRecoveryFailure(ctx context.Context, taskID string, trigger runtime.InstVersion, inc *recoveryIncident, cause error, pendings []pendingCleanup) {
	// G3-7 补偿前重验：Shutdown 已开始 → 按 D3 ctx 取消/Shutdown 分派，不做状态写入
	//（仅回退本 incident 拥有的 runtime；任务留 activating 由下次启动 reconcile 收敛）。
	if m.shutdownStartedLocked() {
		m.cancelOwnedCleanup(context.WithoutCancel(ctx), taskID, inc)
		return
	}
	compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationTimeout)
	defer cancel()
	// G3-7：状态重验——已非 activating（并发收敛/用户介入）时服从 DB 最新状态，清 debt 退出。
	row, rerr := m.store.GetTask(compCtx, taskID)
	if rerr != nil {
		log.Printf("ensureRecovery: revalidate task %s before compensation: %v", taskID, rerr)
		m.persistRecoveryCompleteDebtLogged(compCtx, taskID, cause.Error())
		return
	}
	if row.Status != StatusActivating {
		// G3-20：已非 activating（并发收敛/用户介入）→ 经同一原子事务收敛残留 debt
		//（CAS 失配分支事务内删除），单一清债点，不做事务外删除。
		if _, werr := m.writeCompleteRecoveryFailure(compCtx, taskID,
			sql.NullString{String: cause.Error(), Valid: true}); werr != nil {
			log.Printf("ensureRecovery: converge residual debt task %s: %v", taskID, werr)
		}
		return
	}

	runtimeName := runtimeSessionName(taskID)
	exists, herr := m.proc.HasSession(runtimeName)
	var observations []cleanupObservation
	switch {
	case herr != nil && !errors.Is(herr, process.ErrNoTmuxServer):
		nctx, ncancel := withResidualNoticeCtx(compCtx)
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, nil, noticeReasonKillFailed, true)
		ncancel()
		observations = append(observations, cleanupObservation{
			sessionName: runtimeName, reason: noticeReasonKillFailed, retryable: true, persisted: nerr == nil,
		})
		if nerr != nil {
			m.persistRecoveryCleanupDebtLogged(compCtx, taskID, runtimeName, nil, noticeReasonKillFailed, true, cause.Error())
			return
		}
	case exists:
		res, kerr := m.proc.KillSession(runtimeName)
		if kerr != nil {
			nctx, ncancel := withResidualNoticeCtx(compCtx)
			nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true)
			ncancel()
			observations = append(observations, cleanupObservation{
				sessionName: runtimeName, reason: noticeReasonKillFailed, retryable: true,
				cleanupTickets: res.CleanupTickets, persisted: nerr == nil,
			})
			if nerr != nil {
				m.persistRecoveryCleanupDebtLogged(compCtx, taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true, cause.Error())
				return
			}
		} else {
			cls := classifyKillResult(res)
			if cls.action != "none" {
				nctx, ncancel := withResidualNoticeCtx(compCtx)
				nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable)
				ncancel()
				observations = append(observations, cleanupObservation{
					sessionName: runtimeName, reason: cls.reason, retryable: cls.retryable,
					cleanupTickets: res.CleanupTickets, persisted: nerr == nil,
				})
				if nerr != nil {
					m.persistRecoveryCleanupDebtLogged(compCtx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable, cause.Error())
					return
				}
			}
		}
	}

	// G3-2：cause 链 + 前序 pendings + 本次 observations 归并后统一重放；任一 notice
	// 仍写失败 → 阻断终态事务，落 durable cleanup_notice debt 由后台/Shutdown/重启重放。
	allPendings := append(append([]pendingCleanup(nil), pendings...), foldPendingCleanups(cause, observations)...)
	if len(allPendings) > 0 {
		if rerr := m.replayPendingCleanups(compCtx, taskID, allPendings); rerr != nil {
			log.Printf("ensureRecovery: replay pending cleanups task %s: %v", taskID, rerr)
			m.persistRecoveryCleanupDebtsLogged(compCtx, taskID, allPendings, cause.Error())
			return
		}
	}

	le := sql.NullString{String: cause.Error(), Valid: true}
	// G3-11 write-intent-first：先在独立 detached bounded ctx 落 durable complete
	// intent（不得复用可能已超时的补偿 ctx），成功后才执行 Complete；Complete 与
	// 清 intent 由原子事务一次完成（成功 / CAS mismatch 均删，G3-20）。intent 落盘
	// 失败 → 不执行 Complete，入内存重试队列。
	intentCtx, intentCancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryDebtPersistTimeout)
	defer intentCancel()
	if err := m.persistRecoveryCompleteDebt(intentCtx, taskID, cause.Error()); err != nil {
		log.Printf("ensureRecovery: persist complete intent task %s: %v", taskID, err)
		m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
			TaskID: taskID, Phase: recoveryDebtPhaseComplete, Tickets: "[]",
			Cause: cause.Error(), CreatedAt: m.nowUnix(),
		})
		return
	}
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationFinalizeTimeout)
	defer finalCancel()
	if _, err := m.writeCompleteRecoveryFailure(finalCtx, taskID, le); err != nil {
		// intent 保留（durable）：后台/Shutdown/重启重放驱动收敛，MUST NOT 静默。
		log.Printf("ensureRecovery: CompleteRecoveryFailure task %s: %v (complete intent retained)", taskID, err)
		return
	}
	// G3-20：成功 / CAS mismatch 后不再执行事务外二次删除——CompleteRecoveryFailure
	// AndClearDebts 单事务内已删全部 debt（二次 Delete 短暂失败会误报 replay 错误、
	// 且可能删掉事务返回后并发写入的新 intent）。
}

// enqueueRecoveryDebtFallback 把落盘失败的 debt 行放入内存重试队列（G3-11）。
func (m *Manager) enqueueRecoveryDebtFallback(row RecoveryDebtRow) {
	m.recoveryDebtFallbackMu.Lock()
	defer m.recoveryDebtFallbackMu.Unlock()
	m.recoveryDebtFallbacks = append(m.recoveryDebtFallbacks, row)
}

// flushRecoveryDebtFallbacks 重试落盘内存 debt 队列（G3-11/G3-13；后台周期 +
// Shutdown）。持锁摘除快照并清空队列，仅失败项放回（置于并发新入队项之前，保持
// 先入先出重试顺序）——成功项不得残留（否则每轮 flush 后 replay 复活旧 complete
// intent，可把新激活错误收敛 suspended）。返回聚合错误（含未刷空提示：残留项随
// 进程退出丢失，MUST 传播）。
func (m *Manager) flushRecoveryDebtFallbacks(ctx context.Context) error {
	m.recoveryDebtFallbackMu.Lock()
	pending := m.recoveryDebtFallbacks
	m.recoveryDebtFallbacks = nil
	m.recoveryDebtFallbackMu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	var errs []error
	var remaining []RecoveryDebtRow
	for _, row := range pending {
		if err := m.store.UpsertRecoveryDebt(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", row.TaskID, err))
			remaining = append(remaining, row)
		}
	}
	if len(remaining) > 0 {
		m.recoveryDebtFallbackMu.Lock()
		m.recoveryDebtFallbacks = append(remaining, m.recoveryDebtFallbacks...)
		m.recoveryDebtFallbackMu.Unlock()
		errs = append(errs, fmt.Errorf("%d recovery debt rows unflushed (in-memory only)", len(remaining)))
	}
	return errors.Join(errs...)
}

// persistRecoveryCleanupDebt 落 durable cleanup_notice tagged debt（G3-3/G3-10）：
// 每 pending 一行（task_id + session_name 复合主键）；tickets 另经 cleanup_debts
// （orphan）持久化供 reaper 收割。返回落盘错误（G3-11）。
func (m *Manager) persistRecoveryCleanupDebt(ctx context.Context, taskID, sessionName string, tickets []string, reason string, retryable bool, cause string) error {
	b, err := json.Marshal(tickets)
	if err != nil {
		b = []byte("[]")
	}
	if uerr := m.store.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
		TaskID: taskID, Phase: recoveryDebtPhaseCleanupNotice, SessionName: sessionName,
		Tickets: string(b), Reason: reason, Retryable: retryable, Cause: cause, CreatedAt: m.nowUnix(),
	}); uerr != nil {
		return uerr
	}
	if perr := m.persistOrphanDebt(ctx, sessionName, tickets); perr != nil {
		return perr
	}
	return nil
}

// persistRecoveryCleanupDebts 落全部 pending 的 cleanup_notice debt（G3-10：每
// session 一行，不再丢弃首条外载荷）；任一失败入内存重试队列并返回错误。
func (m *Manager) persistRecoveryCleanupDebts(ctx context.Context, taskID string, pendings []pendingCleanup, cause string) error {
	var errs []error
	for _, p := range pendings {
		if err := m.persistRecoveryCleanupDebt(ctx, taskID, p.sessionName, p.cleanupTickets, p.reason, p.retryable, cause); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", p.sessionName, err))
			b, _ := json.Marshal(p.cleanupTickets)
			if b == nil {
				b = []byte("[]")
			}
			m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
				TaskID: taskID, Phase: recoveryDebtPhaseCleanupNotice, SessionName: p.sessionName,
				Tickets: string(b), Reason: p.reason, Retryable: p.retryable, Cause: cause, CreatedAt: m.nowUnix(),
			})
		}
	}
	return errors.Join(errs...)
}

// persistRecoveryCleanupDebt(s)Logged 是补偿路径的日志兜底封装（补偿流程不因 debt
// 落盘失败中断，错误 MUST 记录；内存队列由 persistRecoveryCleanupDebts 内部维护）。
func (m *Manager) persistRecoveryCleanupDebtLogged(ctx context.Context, taskID, sessionName string, tickets []string, reason string, retryable bool, cause string) {
	if err := m.persistRecoveryCleanupDebt(ctx, taskID, sessionName, tickets, reason, retryable, cause); err != nil {
		log.Printf("ensureRecovery: persist recovery debt task %s: %v", taskID, err)
		b, _ := json.Marshal(tickets)
		if b == nil {
			b = []byte("[]")
		}
		m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
			TaskID: taskID, Phase: recoveryDebtPhaseCleanupNotice, SessionName: sessionName,
			Tickets: string(b), Reason: reason, Retryable: retryable, Cause: cause, CreatedAt: m.nowUnix(),
		})
	}
}

func (m *Manager) persistRecoveryCleanupDebtsLogged(ctx context.Context, taskID string, pendings []pendingCleanup, cause string) {
	if err := m.persistRecoveryCleanupDebts(ctx, taskID, pendings, cause); err != nil {
		log.Printf("ensureRecovery: persist recovery debts task %s: %v", taskID, err)
	}
}

// persistRecoveryCompleteDebt 落 durable complete tagged debt（G3-3/G3-11）：
// 仅 taskID + cause（session_name 空串位）。返回落盘错误。
func (m *Manager) persistRecoveryCompleteDebt(ctx context.Context, taskID string, cause string) error {
	return m.store.UpsertRecoveryDebt(ctx, RecoveryDebtRow{
		TaskID: taskID, Phase: recoveryDebtPhaseComplete, Tickets: "[]", Cause: cause, CreatedAt: m.nowUnix(),
	})
}

func (m *Manager) persistRecoveryCompleteDebtLogged(ctx context.Context, taskID string, cause string) {
	if err := m.persistRecoveryCompleteDebt(ctx, taskID, cause); err != nil {
		log.Printf("ensureRecovery: persist complete debt task %s: %v", taskID, err)
		m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
			TaskID: taskID, Phase: recoveryDebtPhaseComplete, Tickets: "[]",
			Cause: cause, CreatedAt: m.nowUnix(),
		})
	}
}

// replayRecoveryDebts 重放 durable tagged debt（G3-3/G3-10，D3 pending/replay 合同）。
// 重放入口：后台周期 + Shutdown + Reconcile 启动。按任务分组：cleanup_notice 行
// 逐项重放 notice，全部成功才允许 CompleteRecoveryFailure；complete 行直接 Complete。
// Complete 写库失败 → 任务债务降级为单条 complete 行（保留，后台重试驱动收敛）；
// 清债统一由原子事务（CompleteRecoveryFailureAndClearDebts，成功 / CAS mismatch
// 均删）完成——G3-20：本层不再有事务外删除路径。
func (m *Manager) replayRecoveryDebts(ctx context.Context) error {
	rows, err := m.store.ListRecoveryDebts(ctx)
	if err != nil {
		return fmt.Errorf("replay recovery debts: list: %w", err)
	}
	byTask := map[string][]RecoveryDebtRow{}
	var order []string
	for _, d := range rows {
		if _, ok := byTask[d.TaskID]; !ok {
			order = append(order, d.TaskID)
		}
		byTask[d.TaskID] = append(byTask[d.TaskID], d)
	}
	var errs []error
	for _, taskID := range order {
		if err := m.replayRecoveryDebtsForTask(ctx, taskID, byTask[taskID]); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", taskID, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) replayRecoveryDebtsForTask(ctx context.Context, taskID string, rows []RecoveryDebtRow) error {
	var completeRow *RecoveryDebtRow
	for i := range rows {
		if rows[i].Phase == recoveryDebtPhaseComplete {
			completeRow = &rows[i]
		}
	}
	// cleanup_notice：逐项重放 notice，任一失败保留全部行等下一轮（notice 幂等）。
	for _, d := range rows {
		if d.Phase != recoveryDebtPhaseCleanupNotice {
			continue
		}
		var tickets []string
		if jerr := json.Unmarshal([]byte(d.Tickets), &tickets); jerr != nil {
			tickets = nil
		}
		if nerr := m.recordResidualNotice(ctx, taskID, d.SessionName, tickets, d.Reason, d.Retryable); nerr != nil {
			return fmt.Errorf("replay notice %s: %w", d.SessionName, nerr)
		}
		// tickets 交 reaper 收割（cleanup_debts），失败聚合但不阻断状态收敛。
		if perr := m.persistOrphanDebt(ctx, d.SessionName, tickets); perr != nil {
			log.Printf("recovery debt replay tickets %s: %v", d.SessionName, perr)
		}
	}
	cause := ""
	if completeRow != nil {
		cause = completeRow.Cause
	} else if len(rows) > 0 {
		cause = rows[0].Cause
	}
	// 状态收敛需 cause：cleanup_notice-only 行也有 cause（补偿失败原因）。
	if _, werr := m.writeCompleteRecoveryFailure(ctx, taskID, sql.NullString{String: cause, Valid: true}); werr != nil {
		// G3-12：失败时保留原行，仅 upsert complete 行、不得预删（两步非事务，
		// 中断或 upsert 失败不得丢失债务；design.md:115「Complete 写库失败必须
		// 保留 debt」）。complete + cleanup 行共存安全：notice 重放幂等，cause
		// 取 complete 行。upsert 失败 → 原行原样保留 + 内存队列补充 complete intent。
		var errs []error
		errs = append(errs, fmt.Errorf("complete: %w", werr))
		if uerr := m.persistRecoveryCompleteDebt(ctx, taskID, cause); uerr != nil {
			errs = append(errs, fmt.Errorf("demote upsert complete debt: %w", uerr))
			m.enqueueRecoveryDebtFallback(RecoveryDebtRow{
				TaskID: taskID, Phase: recoveryDebtPhaseComplete, Tickets: "[]",
				Cause: cause, CreatedAt: m.nowUnix(),
			})
		}
		return errors.Join(errs...)
	}
	// G3-20：成功 / CAS mismatch 后不再执行事务外整组删除——原子事务
	//（CompleteRecoveryFailureAndClearDebts）已清全部 debt，是唯一清债点；
	// 二次 Delete 短暂失败会误报错误使启动 Reconcile fail-closed，且可能删掉
	// 事务返回后并发写入的新 intent（跨代 debt 丢失）。
	return nil
}
