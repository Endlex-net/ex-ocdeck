package task

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/opencode"
)

// ctxAwareStore 装饰 *mockStore，在调用方 ctx 已取消（ctx.Err()!=nil）时对补偿路径
// 涉及的 store 方法返回错误，模拟真实 store 用 ExecContext 的行为。
// 覆盖的方法：UpdateTaskEnvSnapshot、UpdateTaskStatusConditional、recordResidualNotice
// 路径用的 UpdateTaskNotice/UpdateTaskNoticeCAS。
// 其余方法直接委托嵌入的 *mockStore。
type ctxAwareStore struct {
	*mockStore
	mu            sync.Mutex
	callsWhenDead []string // 记录 ctx 取消时被调用的方法名（断言补偿未走已取消 ctx）
}

func newCtxAwareStore(inner *mockStore) *ctxAwareStore {
	return &ctxAwareStore{mockStore: inner}
}

// deadCtxErr 在 ctx 已取消时返回模拟 ExecContext 的错误。
func deadCtxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.New("context canceled: " + err.Error())
	}
	return nil
}

func (s *ctxAwareStore) noteDeadCall(method string) {
	s.mu.Lock()
	s.callsWhenDead = append(s.callsWhenDead, method)
	s.mu.Unlock()
}

func (s *ctxAwareStore) deadCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.callsWhenDead))
	copy(out, s.callsWhenDead)
	return out
}

func (s *ctxAwareStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if err := deadCtxErr(ctx); err != nil {
		s.noteDeadCall("UpdateTaskEnvSnapshot")
		return application.MutationResult{}, err
	}
	return s.mockStore.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

func (s *ctxAwareStore) UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (application.TransitionResult, error) {
	if err := deadCtxErr(ctx); err != nil {
		s.noteDeadCall("UpdateTaskStatusConditional")
		return application.TransitionResult{}, err
	}
	return s.mockStore.UpdateTaskStatusConditional(ctx, id, fromStatus, toStatus, lastError)
}

func (s *ctxAwareStore) UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) (application.TransitionResult, error) {
	if err := deadCtxErr(ctx); err != nil {
		s.noteDeadCall("UpdateTaskStatus")
		return application.TransitionResult{}, err
	}
	return s.mockStore.UpdateTaskStatus(ctx, id, status, lastError)
}

func (s *ctxAwareStore) UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) (application.MutationResult, error) {
	if err := deadCtxErr(ctx); err != nil {
		s.noteDeadCall("UpdateTaskNotice")
		return application.MutationResult{}, err
	}
	return s.mockStore.UpdateTaskNotice(ctx, id, notice)
}

func (s *ctxAwareStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	if err := deadCtxErr(ctx); err != nil {
		s.noteDeadCall("UpdateTaskNoticeCAS")
		return application.MutationResult{}, err
	}
	return s.mockStore.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// --- BLOCKED 修复测试：probe 退避期间取消 ctx，补偿仍落 suspended + last_error ---

// TestActivate_ProbeCancelCompensationUsesUncanceledCtx 验证 BLOCKED 修复：
// probe 退避期间取消调用方 ctx → 补偿用脱离取消的 compCtx →
// serve 会话已 kill、任务回 suspended、last_error 已写入。
// 用 ctxAwareStore 模拟真实 store：若补偿误用已取消 ctx，UpdateTaskStatusConditional
// 会返回错误导致任务卡 activating。
//
// 确定性同步（无 wall-clock sleep 时序假设）：probeSequenceOC 首次 Probe 进入时 close 信号 channel；
// 退避注入 5s（永不真实等待——取消立即打断 timer）。测试侧等首 Probe 信号 → 取消 ctx → Activate 返回。
func TestActivate_ProbeCancelCompensationUsesUncanceledCtx(t *testing.T) {
	store := newCtxAwareStore(newMockStore())
	seedSuspendedTask(store.mockStore, "t1", "p1")
	proc := newMockProc()
	// Probe 首次返回 ErrServeNotReady → 进入退避；首 Probe 进入时 close 信号。
	firstProbeStarted := make(chan struct{})
	oc := &probeSignalOC{
		probeSequenceOC: newProbeSequenceOC(true, opencode.ErrServeNotReady),
		firstProbeCh:    firstProbeStarted,
	}
	m := newTestManager(t, store, proc, newMockWorktree(), oc)
	// 退避注入 5s：退避期间取消 ctx，timer 立即被打断，永不真实等待 5s。
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{5 * time.Second, 5 * time.Second} }

	reqCtx, cancel := context.WithCancel(context.Background())
	// 等首次 Probe 进入（确定性信号），然后取消 ctx——此时 Activate 必在退避 sleep 中。
	go func() {
		<-firstProbeStarted
		cancel()
	}()

	err := m.Activate(reqCtx, "t1")
	if err == nil {
		t.Fatal("expected error on ctx cancel during probe backoff")
	}
	// 补偿 MUST 用脱离取消的 compCtx：ctxAwareStore 不应在 ctx 取消时被调用补偿方法。
	if dead := store.deadCalls(); len(dead) != 0 {
		t.Errorf("compensation must use uncanceled ctx; dead-ctx calls observed: %v", dead)
	}
	// serve 会话已 kill（补偿执行）。
	if proc.sessions[serveSessionName("t1")] {
		t.Error("serve session should be killed by compensation after ctx cancel")
	}
	// 任务回 suspended（补偿落库成功，未卡 activating）。
	row, _ := store.GetTask(context.Background(), "t1")
	if row.Status != StatusSuspended {
		t.Errorf("status = %s, want suspended (compensation must rollback)", row.Status)
	}
	// last_error 已写入。
	if !row.LastError.Valid || row.LastError.String == "" {
		t.Error("last_error should be recorded by compensation")
	}
}

// probeSignalOC 装饰 probeSequenceOC，首次 Probe 进入时 close firstProbeCh（确定性信号，
// 供测试精确等待「首次 Probe 已开始」而非 wall-clock 轮询）。
type probeSignalOC struct {
	*probeSequenceOC
	firstProbeCh chan struct{}
	firstOnce    sync.Once
}

func (c *probeSignalOC) Probe(ctx context.Context) (string, error) {
	c.firstOnce.Do(func() { close(c.firstProbeCh) })
	return c.probeSequenceOC.Probe(ctx)
}