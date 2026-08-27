package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"ocdeck/internal/application/runtime"
	"ocdeck/internal/infrastructure/process"
)

const (
	recoveryDebtPhaseCleanupNotice = "cleanup_notice"
	recoveryDebtPhaseComplete      = "complete"
	errRecoveryBudgetExhausted     = "recovery budget exhausted"
)

var (
	errRecoveryTokenInvalid = errors.New("recovery token invalid")
	errRecoveryAbandon      = errors.New("recovery abandoned")
)

type recoveryTaggedDebt struct {
	phase          string
	taskID         string
	sessionName    string
	cleanupTickets []string
	reason         string
	retryable      bool
	cause          string
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
// 持任务锁后校验 token + 状态；仅首个 incident CAS active→activating；
// activating 下重复调用 no-op。
func (m *Manager) ensureRecovery(taskID string, tok runtime.InstVersion) {
	unlock, err := m.lockTaskForConverge(taskID)
	if err != nil {
		log.Printf("ensureRecovery: lock task %s: %v", taskID, err)
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

	ctx := m.lifecycleCtx()
	cas, cerr := m.writeStatusConditional(ctx, taskID, StatusActive, StatusActivating, sql.NullString{})
	if cerr != nil {
		log.Printf("ensureRecovery: CAS active→activating task %s: %v", taskID, cerr)
		unlock()
		return
	}
	if !cas.Matched {
		unlock()
		return
	}
	// CAS 完成后释放任务锁，attempt/退避不得阻塞 Suspend（D3：Suspend 先拿锁则恢复放弃）。
	unlock()
	m.runRecoveryIncident(ctx, taskID, tok)
}

func (m *Manager) shutdownStartedLocked() bool {
	m.shutdownGateMu.Lock()
	defer m.shutdownGateMu.Unlock()
	return m.shutdownStarted
}

func (m *Manager) runRecoveryIncident(ctx context.Context, taskID string, trigger runtime.InstVersion) {
	if err := m.recoveryPrelude(ctx, taskID, trigger); err != nil {
		if errors.Is(err, errRecoveryTokenInvalid) || errors.Is(err, errRecoveryAbandon) {
			return
		}
		m.completeRecoveryFailure(ctx, taskID, trigger, err)
		return
	}

	var lastErr error
	for {
		if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
			if errors.Is(err, errRecoveryTokenInvalid) || errors.Is(err, errRecoveryAbandon) {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				m.conditionalCancelCleanup(ctx, taskID, trigger)
				return
			}
			m.completeRecoveryFailure(ctx, taskID, trigger, err)
			return
		}
		attemptErr := m.runRecoveryAttempt(ctx, taskID, trigger)
		if attemptErr == nil {
			return
		}
		if errors.Is(attemptErr, errRecoveryTokenInvalid) || errors.Is(attemptErr, errRecoveryAbandon) {
			return
		}
		var handled *errActivateCommitHandled
		if errors.As(attemptErr, &handled) {
			m.replayHandledCleanup(ctx, taskID, attemptErr)
			return
		}
		if errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) {
			m.conditionalCancelCleanup(ctx, taskID, trigger)
			return
		}
		if isRecoveryTerminal(attemptErr) {
			if lastErr != nil {
				attemptErr = fmt.Errorf("%w; previous: %v", attemptErr, lastErr)
			}
			m.completeRecoveryFailure(ctx, taskID, trigger, attemptErr)
			return
		}
		_ = m.confirmRuntimeTerminated(ctx, taskID, runtimeSessionName(taskID))
		lastErr = attemptErr
	}
}

func isRecoveryTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRecoveryBudgetExhaustedSentinel) {
		return true
	}
	msg := err.Error()
	if contains(msg, errRecoveryBudgetExhausted) {
		return true
	}
	code := OpErrorCode(err)
	switch code {
	case codeOCIncompatible, codeInternal:
		if contains(msg, "capability probe") || contains(msg, "persist anchor") || contains(msg, "anchor session") {
			return true
		}
		if contains(msg, "clear stale anchor") || contains(msg, "store") {
			return true
		}
	}
	if contains(msg, "anchor session") && contains(msg, "conflict") {
		return true
	}
	if contains(msg, "create anchor session") {
		return true
	}
	if contains(msg, "capability probe") {
		return true
	}
	if contains(msg, "allocate") || contains(msg, "no available port") || contains(msg, "acquire recovery permit") {
		return true
	}
	var pce *pendingCleanupError
	if errors.As(err, &pce) && pce != nil && pce.pending.retryable {
		return true
	}
	return false
}

var errRecoveryBudgetExhaustedSentinel = errors.New(errRecoveryBudgetExhausted)

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

func (m *Manager) recoveryPrelude(ctx context.Context, taskID string, trigger runtime.InstVersion) error {
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err
	}
	obsErr, observations := m.cleanupActivationRuntimeCollect(ctx, taskID)
	if obsErr != nil {
		if pendings := foldPendingCleanups(obsErr, observations); len(pendings) > 0 {
			return obsErr
		}
		return obsErr
	}
	for _, o := range observations {
		if o.retryable {
			return fmt.Errorf("recovery prelude retryable cleanup debt: %s", o.reason)
		}
	}
	return nil
}

func (m *Manager) runRecoveryAttempt(ctx context.Context, taskID string, trigger runtime.InstVersion) error {
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		return err
	}
	permit, err := m.store.AcquireRecoveryPermit(ctx, taskID, m.nowUnix())
	if err != nil {
		return newOpErr(codeInternal, fmt.Errorf("acquire recovery permit: %w", err))
	}
	if !permit.Acquired {
		return fmt.Errorf("%w", errRecoveryBudgetExhaustedSentinel)
	}
	if err := m.waitRecoveryBackoff(ctx, taskID, trigger, permit.Ordinal); err != nil {
		return err
	}

	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeInternal, err)
	}
	proj, perr := m.store.GetProject(ctx, row.ProjectID)
	if perr != nil {
		return newOpErr(codeNotFound, fmt.Errorf("project gone: %w", perr))
	}
	mode, kerr := alignModeForKind(proj.Kind)
	if kerr != nil {
		return newOpErr(codeInternal, kerr)
	}

	port, aerr := m.allocatePort(row.LastPort, 0)
	if aerr != nil {
		return newOpErr(codeConflict, aerr)
	}
	env, merr := m.mergeEnvSnapshot(ctx, row, port)
	if merr != nil {
		return newOpErr(codeInternal, merr)
	}

	runtimeName := runtimeSessionName(taskID)
	password := newRandomPassword()
	consumedFirst := true
	beforeCreate := func() error {
		if consumedFirst {
			consumedFirst = false
			return nil
		}
		if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
			return err
		}
		p, perr := m.store.AcquireRecoveryPermit(ctx, taskID, m.nowUnix())
		if perr != nil {
			return newOpErr(codeInternal, fmt.Errorf("acquire recovery permit: %w", perr))
		}
		if !p.Acquired {
			return fmt.Errorf("%w", errRecoveryBudgetExhaustedSentinel)
		}
		return m.waitRecoveryBackoff(ctx, taskID, trigger, p.Ordinal)
	}
	port, password, berr := m.bootstrapRuntime(ctx, row, runtimeName, port, password, env, beforeCreate)
	if berr != nil {
		return berr
	}
	if err := m.checkRecoveryContinuable(ctx, taskID, trigger); err != nil {
		_ = m.confirmRuntimeTerminated(ctx, taskID, runtimeName)
		return err
	}
	cerr := m.commitRuntimeReady(ctx, taskID, row.WorktreePath, runtimeName, port, password, mode)
	if cerr == nil {
		return nil
	}
	var handled *errActivateCommitHandled
	if errors.As(cerr, &handled) {
		return cerr
	}
	if rt := m.getRuntime(taskID); rt != nil {
		_ = m.rollbackAttemptRuntime(ctx, taskID, runtimeName, rt.instVersion)
	} else {
		_ = m.confirmRuntimeTerminated(ctx, taskID, runtimeName)
	}
	return cerr
}

func (m *Manager) waitRecoveryBackoff(ctx context.Context, taskID string, trigger runtime.InstVersion, ordinal int) error {
	d := m.recoveryBackoff(ordinal)
	if d <= 0 {
		return m.checkRecoveryContinuable(ctx, taskID, trigger)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return m.checkRecoveryContinuable(ctx, taskID, trigger)
	}
}

func (m *Manager) conditionalCancelCleanup(ctx context.Context, taskID string, trigger runtime.InstVersion) {
	rt := m.getRuntime(taskID)
	if rt == nil || rt.instVersion != trigger {
		return
	}
	_ = m.rollbackAttemptRuntime(ctx, taskID, runtimeSessionName(taskID), trigger)
}

func (m *Manager) completeRecoveryFailure(ctx context.Context, taskID string, trigger runtime.InstVersion, cause error) {
	compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationTimeout)
	defer cancel()

	rt := m.getRuntime(taskID)
	if rt != nil && rt.instVersion == trigger {
		rt.stopAllJoin()
		m.rtMu.Lock()
		if m.runtimes[taskID] == rt {
			delete(m.runtimes, taskID)
		}
		m.rtMu.Unlock()
	}

	runtimeName := runtimeSessionName(taskID)
	exists, herr := m.proc.HasSession(runtimeName)
	var obs []cleanupObservation
	var cleanupErr error
	nctx, ncancel := withResidualNoticeCtx(compCtx)
	defer ncancel()
	switch {
	case herr != nil && !errors.Is(herr, process.ErrNoTmuxServer):
		nerr := m.recordResidualNotice(nctx, taskID, runtimeName, nil, noticeReasonKillFailed, true)
		obs = append(obs, cleanupObservation{
			sessionName: runtimeName, reason: noticeReasonKillFailed, retryable: true, persisted: nerr == nil,
		})
		cleanupErr = nerr
		if nerr != nil {
			m.persistRecoveryCleanupDebt(taskID, runtimeName, nil, noticeReasonKillFailed, true, cause.Error())
			return
		}
	case exists:
		res, kerr := m.proc.KillSession(runtimeName)
		if kerr != nil {
			nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true)
			obs = append(obs, cleanupObservation{
				sessionName: runtimeName, reason: noticeReasonKillFailed, retryable: true,
				cleanupTickets: res.CleanupTickets, persisted: nerr == nil,
			})
			if nerr != nil {
				m.persistRecoveryCleanupDebt(taskID, runtimeName, res.CleanupTickets, noticeReasonKillFailed, true, cause.Error())
				return
			}
		} else {
			cls := classifyKillResult(res)
			if cls.action != "none" {
				nerr := m.recordResidualNotice(nctx, taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable)
				obs = append(obs, cleanupObservation{
					sessionName: runtimeName, reason: cls.reason, retryable: cls.retryable,
					cleanupTickets: res.CleanupTickets, persisted: nerr == nil,
				})
				if nerr != nil {
					m.persistRecoveryCleanupDebt(taskID, runtimeName, res.CleanupTickets, cls.reason, cls.retryable, cause.Error())
					return
				}
			}
		}
	}

	_ = cleanupErr
	_ = obs
	le := sql.NullString{String: cause.Error(), Valid: true}
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), activateCompensationFinalizeTimeout)
	defer finalCancel()
	res, err := m.writeCompleteRecoveryFailure(finalCtx, taskID, le)
	if err != nil {
		m.persistRecoveryCompleteDebt(taskID, cause.Error())
		log.Printf("ensureRecovery: CompleteRecoveryFailure task %s: %v", taskID, err)
		return
	}
	if !res.Matched {
		m.clearRecoveryDebt(taskID)
	}
}

func (m *Manager) persistRecoveryCleanupDebt(taskID, sessionName string, tickets []string, reason string, retryable bool, cause string) {
	m.recoveryDebtMu.Lock()
	defer m.recoveryDebtMu.Unlock()
	if m.recoveryDebts == nil {
		m.recoveryDebts = map[string]recoveryTaggedDebt{}
	}
	m.recoveryDebts[taskID] = recoveryTaggedDebt{
		phase: recoveryDebtPhaseCleanupNotice, taskID: taskID, sessionName: sessionName,
		cleanupTickets: append([]string(nil), tickets...), reason: reason, retryable: retryable, cause: cause,
	}
	if perr := m.persistOrphanDebt(context.Background(), sessionName, tickets); perr != nil {
		log.Printf("ensureRecovery: persist orphan debt %s: %v", sessionName, perr)
	}
}

func (m *Manager) persistRecoveryCompleteDebt(taskID, cause string) {
	m.recoveryDebtMu.Lock()
	defer m.recoveryDebtMu.Unlock()
	if m.recoveryDebts == nil {
		m.recoveryDebts = map[string]recoveryTaggedDebt{}
	}
	m.recoveryDebts[taskID] = recoveryTaggedDebt{phase: recoveryDebtPhaseComplete, taskID: taskID, cause: cause}
}

func (m *Manager) clearRecoveryDebt(taskID string) {
	m.recoveryDebtMu.Lock()
	defer m.recoveryDebtMu.Unlock()
	delete(m.recoveryDebts, taskID)
}

func (m *Manager) replayRecoveryDebts(ctx context.Context) {
	m.recoveryDebtMu.Lock()
	debts := make([]recoveryTaggedDebt, 0, len(m.recoveryDebts))
	for _, d := range m.recoveryDebts {
		debts = append(debts, d)
	}
	m.recoveryDebtMu.Unlock()
	for _, d := range debts {
		switch d.phase {
		case recoveryDebtPhaseCleanupNotice:
			if err := m.recordResidualNotice(ctx, d.taskID, d.sessionName, d.cleanupTickets, d.reason, d.retryable); err != nil {
				log.Printf("recovery debt replay notice %s: %v", d.taskID, err)
				continue
			}
			le := sql.NullString{String: d.cause, Valid: true}
			res, err := m.writeCompleteRecoveryFailure(ctx, d.taskID, le)
			if err != nil {
				m.persistRecoveryCompleteDebt(d.taskID, d.cause)
				continue
			}
			if !res.Matched {
				m.clearRecoveryDebt(d.taskID)
				continue
			}
			m.clearRecoveryDebt(d.taskID)
		case recoveryDebtPhaseComplete:
			le := sql.NullString{String: d.cause, Valid: true}
			res, err := m.writeCompleteRecoveryFailure(ctx, d.taskID, le)
			if err != nil {
				continue
			}
			m.clearRecoveryDebt(d.taskID)
			_ = res
		}
	}
}
