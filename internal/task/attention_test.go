package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// --- 辅助 ---

func permReq(id, sid, perm string, patterns ...string) opencode.PermissionRequest {
	return opencode.PermissionRequest{ID: id, SessionID: sid, Permission: perm, Patterns: patterns}
}

func questReq(id, sid, header, question string) opencode.QuestionRequest {
	return opencode.QuestionRequest{ID: id, SessionID: sid, Questions: []opencode.QuestionItem{{Header: header, Question: question}}}
}

func permAsked(id, sid, perm string, patterns ...string) opencode.AttentionEvent {
	return opencode.AttentionEvent{Kind: opencode.AttentionAsked, Type: opencode.AttentionPermission, RequestID: id, SessionID: sid, Permission: perm, Patterns: patterns}
}

func permReplied(sid, rid string) opencode.AttentionEvent {
	return opencode.AttentionEvent{Kind: opencode.AttentionReplied, Type: opencode.AttentionPermission, RequestID: rid, SessionID: sid}
}

func questAsked(id, sid, header, question string) opencode.AttentionEvent {
	return opencode.AttentionEvent{Kind: opencode.AttentionAsked, Type: opencode.AttentionQuestion, RequestID: id, SessionID: sid, Questions: []opencode.QuestionItem{{Header: header, Question: question}}}
}

func questReplied(sid, rid string) opencode.AttentionEvent {
	return opencode.AttentionEvent{Kind: opencode.AttentionReplied, Type: opencode.AttentionQuestion, RequestID: rid, SessionID: sid}
}

// blockingPermOC 阻塞 ListPermissions 直到 block 通道关闭。
// entered 在进入 ListPermissions（REST 开始）时关闭，供测试确定性同步。
type blockingPermOC struct {
	inner   *mockOC
	block   <-chan struct{}
	entered chan struct{}
}

func (c *blockingPermOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	if c.entered != nil {
		close(c.entered)
	}
	select {
	case <-c.block:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.inner.ListPermissions(ctx, dir)
}
func (c *blockingPermOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	return c.inner.ListQuestions(ctx, dir)
}
func (c *blockingPermOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *blockingPermOC) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *blockingPermOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *blockingPermOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *blockingPermOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *blockingPermOC) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *blockingPermOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *blockingPermOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *blockingPermOC) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) opencode.PromptResult {
	return c.inner.PromptAsync(ctx, dir, sessionID, messageID, text)
}
func (c *blockingPermOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	return c.inner.ProbePromptAsyncCapability(ctx)
}

// blockingBothOC 同时阻塞 ListPermissions 与 ListQuestions，各自 entered 信号独立。
// 用于验证 degraded 后台重试两类型并发启动（任一释放前两者均已进入 REST）。
type blockingBothOC struct {
	inner        *mockOC
	block        <-chan struct{}
	permEntered  chan struct{}
	questEntered chan struct{}
}

func (c *blockingBothOC) ListPermissions(ctx context.Context, dir string) ([]opencode.PermissionRequest, error) {
	if c.permEntered != nil {
		close(c.permEntered)
	}
	select {
	case <-c.block:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.inner.ListPermissions(ctx, dir)
}
func (c *blockingBothOC) ListQuestions(ctx context.Context, dir string) ([]opencode.QuestionRequest, error) {
	if c.questEntered != nil {
		close(c.questEntered)
	}
	select {
	case <-c.block:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return c.inner.ListQuestions(ctx, dir)
}
func (c *blockingBothOC) Health(ctx context.Context) (opencode.HealthResponse, error) {
	return c.inner.Health(ctx)
}
func (c *blockingBothOC) Probe(ctx context.Context) (string, error) { return c.inner.Probe(ctx) }
func (c *blockingBothOC) ListSessions(ctx context.Context, dir string, limit int) ([]opencode.Session, error) {
	return c.inner.ListSessions(ctx, dir, limit)
}
func (c *blockingBothOC) GetSession(ctx context.Context, dir, id string) (opencode.Session, error) {
	return c.inner.GetSession(ctx, dir, id)
}
func (c *blockingBothOC) CreateSession(ctx context.Context, dir, title string) (opencode.Session, error) {
	return c.inner.CreateSession(ctx, dir, title)
}
func (c *blockingBothOC) DeleteSession(ctx context.Context, dir, id string) error {
	return c.inner.DeleteSession(ctx, dir, id)
}
func (c *blockingBothOC) SessionStatus(ctx context.Context, dir string) (map[string]opencode.SessionStatus, error) {
	return c.inner.SessionStatus(ctx, dir)
}
func (c *blockingBothOC) SubscribeEvents(ctx context.Context, dir string, onEvent func(opencode.Event), onReconnect func()) error {
	return c.inner.SubscribeEvents(ctx, dir, onEvent, onReconnect)
}
func (c *blockingBothOC) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string) opencode.PromptResult {
	return c.inner.PromptAsync(ctx, dir, sessionID, messageID, text)
}
func (c *blockingBothOC) ProbePromptAsyncCapability(ctx context.Context) opencode.CapabilityState {
	return c.inner.ProbePromptAsyncCapability(ctx)
}

// --- pending 生命周期 ---

func TestAttention_Lifecycle(t *testing.T) {
	a := newAttentionState()

	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 1 || snap.Permissions[0].ID != "r1" {
		t.Fatalf("expected 1 permission, got %+v", snap.Permissions)
	}
	if snap.Permissions[0].Since == 0 {
		t.Error("Since should be set to local observation time")
	}
	if len(snap.Permissions[0].Patterns) != 1 || snap.Permissions[0].Patterns[0] != "rm" {
		t.Errorf("patterns should be copied, got %+v", snap.Permissions[0].Patterns)
	}

	a.applyAttentionEvent(questAsked("q1", "s1", "h", "what?"))
	snap = a.attentionSnapshot()
	if len(snap.Questions) != 1 || snap.Questions[0].ID != "q1" {
		t.Fatalf("expected 1 question, got %+v", snap.Questions)
	}

	a.applyAttentionEvent(permReplied("s1", "r1"))
	snap = a.attentionSnapshot()
	if len(snap.Permissions) != 0 {
		t.Fatalf("expected 0 permissions after replied, got %+v", snap.Permissions)
	}

	a.applyAttentionEvent(permReplied("s1", "unknown-id"))
	snap = a.attentionSnapshot()
	if len(snap.Questions) != 1 {
		t.Fatalf("unknown replied should not affect questions, got %+v", snap.Questions)
	}

	// since 保留：已存在 ID 不覆盖 since
	firstSince := snap.Questions[0].Since
	a.applyAttentionEvent(questAsked("q1", "s1", "h", "what?"))
	snap = a.attentionSnapshot()
	if snap.Questions[0].Since != firstSince {
		t.Fatalf("existing ID since should be preserved: got %d, want %d", snap.Questions[0].Since, firstSince)
	}

	a.clearAttention()
	snap = a.attentionSnapshot()
	if len(snap.Permissions) != 0 || len(snap.Questions) != 0 {
		t.Fatalf("expected empty after clear, got %+v", snap)
	}
}

// --- 快照深拷贝：修改快照不影响内部状态 ---

func TestAttention_SnapshotDeepCopy(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm", "ls"))
	a.applyAttentionEvent(questAsked("q1", "s1", "h", "what?"))
	snap := a.attentionSnapshot()
	// 破坏快照
	snap.Permissions[0].Patterns[0] = "MUTATED"
	snap.Questions[0].Questions[0].Header = "MUTATED"
	// 内部状态不受影响
	snap2 := a.attentionSnapshot()
	if snap2.Permissions[0].Patterns[0] == "MUTATED" {
		t.Error("snapshot patterns not deep-copied")
	}
	if snap2.Questions[0].Questions[0].Header == "MUTATED" {
		t.Error("snapshot questions not deep-copied")
	}
}

// --- 原子替换（align 路径）：REST 快照替换保留同 ID since ---

func TestAttention_AlignReplace_PreserveSince(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
	origSince := a.attentionSnapshot().Permissions[0].Since

	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{
		permReq("r1", "s1", "bash", "rm"),
		permReq("r2", "s2", "edit", "file"),
	}
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)

	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 2 {
		t.Fatalf("expected 2 permissions, got %+v", snap.Permissions)
	}
	for _, p := range snap.Permissions {
		if p.ID == "r1" && p.Since != origSince {
			t.Errorf("r1 since should be preserved: got %d, want %d", p.Since, origSince)
		}
	}
}

// --- 能力状态机全迁移 ---

func TestAttention_CapabilityStateMachine(t *testing.T) {
	t.Run("unknown→available on 200", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsResult = []opencode.PermissionRequest{}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capAvailable {
			t.Fatalf("cap = %v, want available", a.perm.cap)
		}
		a.mu.Unlock()
	})

	t.Run("unknown→unsupported on 404", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsErr = opencode.ErrCapabilityUnsupported
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		cap := a.perm.cap
		a.mu.Unlock()
		if cap != capUnsupported {
			t.Fatalf("cap = %v, want unsupported", cap)
		}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		snap := a.attentionSnapshot()
		if len(snap.Permissions) != 0 {
			t.Fatalf("unsupported should expose empty array, got %+v", snap.Permissions)
		}
	})

	t.Run("unknown→degraded on non-404 error", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsErr = errors.New("500 internal error")
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capDegraded {
			t.Fatalf("cap = %v, want degraded", a.perm.cap)
		}
		a.mu.Unlock()
	})

	t.Run("available→degraded then degraded→available", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash")}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capAvailable {
			t.Fatalf("cap = %v, want available", a.perm.cap)
		}
		a.mu.Unlock()

		oc.listPermissionsResult = nil
		oc.listPermissionsErr = errors.New("500")
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capDegraded {
			t.Fatalf("cap = %v, want degraded", a.perm.cap)
		}
		a.mu.Unlock()
		snap := a.attentionSnapshot()
		if len(snap.Permissions) != 1 {
			t.Fatalf("degraded should retain old set, got %+v", snap.Permissions)
		}

		oc.listPermissionsErr = nil
		oc.listPermissionsResult = []opencode.PermissionRequest{}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capAvailable {
			t.Fatalf("cap = %v, want available", a.perm.cap)
		}
		a.mu.Unlock()
	})

	t.Run("available→unsupported on runtime 404", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash")}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		oc.listPermissionsResult = nil
		oc.listPermissionsErr = opencode.ErrCapabilityUnsupported
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capUnsupported {
			t.Fatalf("cap = %v, want unsupported", a.perm.cap)
		}
		a.mu.Unlock()
		snap := a.attentionSnapshot()
		if len(snap.Permissions) != 0 {
			t.Fatalf("unsupported should clear set, got %+v", snap.Permissions)
		}
	})

	t.Run("two types independent", func(t *testing.T) {
		a := newAttentionState()
		oc := newMockOC(true)
		oc.listPermissionsErr = opencode.ErrCapabilityUnsupported
		oc.listQuestionsResult = []opencode.QuestionRequest{questReq("q1", "s1", "h", "q")}
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionQuestion, reconcileAlign, nil)
		a.mu.Lock()
		if a.perm.cap != capUnsupported {
			t.Fatalf("perm cap = %v, want unsupported", a.perm.cap)
		}
		if a.quest.cap != capAvailable {
			t.Fatalf("quest cap = %v, want available", a.quest.cap)
		}
		a.mu.Unlock()
	})
}

// --- context.Canceled 中性 ---

func TestAttention_CanceledNeutral(t *testing.T) {
	a := newAttentionState()
	oc := newMockOC(true)
	oc.listPermissionsErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.reconcileAttention(ctx, oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
	a.mu.Lock()
	if a.perm.cap != capUnknown {
		t.Fatalf("canceled should not transition: cap = %v, want unknown", a.perm.cap)
	}
	a.mu.Unlock()
}

// context.DeadlineExceeded → degraded（非中性，属超时）
func TestAttention_DeadlineExceededIsDegraded(t *testing.T) {
	a := newAttentionState()
	oc := newMockOC(true)
	oc.listPermissionsErr = context.DeadlineExceeded
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
	a.mu.Lock()
	if a.perm.cap != capDegraded {
		t.Fatalf("deadline exceeded should be degraded: cap = %v, want degraded", a.perm.cap)
	}
	a.mu.Unlock()
}

// --- unsupported 忽略 SSE ---

func TestAttention_UnsupportedIgnoresSSE(t *testing.T) {
	a := newAttentionState()
	oc := newMockOC(true)
	oc.listPermissionsErr = opencode.ErrCapabilityUnsupported
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 0 {
		t.Fatalf("unsupported should ignore SSE, got %+v", snap.Permissions)
	}
}

// --- 挂起-对账竞态：在途对账遇挂起代际推进 → 写回被拒（channel barrier） ---

func TestAttention_SuspendReconcileRace(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))

	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{permReq("r1", "s1", "bash")}
	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	done := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(done)
	}()
	// 确定性同步：等 REST 进入后再挂起
	<-entered
	a.clearAttention()
	close(blockCh)
	<-done

	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 0 {
		t.Fatalf("after suspend race, set should be empty, got %+v", snap.Permissions)
	}
}

// --- SSE 写入 × API 快照读取并发 ---

func TestAttention_ConcurrentWriteRead(t *testing.T) {
	a := newAttentionState()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				a.applyAttentionEvent(permAsked("r1", "s1", "bash", "rm"))
			} else {
				a.applyAttentionEvent(permReplied("s1", "r1"))
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = a.attentionSnapshot()
		}
	}()
	wg.Wait()
}

// --- 后台路径：REST 在途期间 SSE asked 不被旧快照覆盖（channel barrier） ---

func TestAttention_BackgroundREST_InFlightSSE(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{}

	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	done := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(done)
	}()
	<-entered
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	close(blockCh)
	<-done

	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 1 || snap.Permissions[0].ID != "r1" {
		t.Fatalf("expected r1 retained from buffer, got %+v", snap.Permissions)
	}
}

// --- since 为缓冲首次观察时间（非重放时刻） ---

func TestAttention_BufferSinceIsObservationTime(t *testing.T) {
	a := newAttentionState()
	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{} // REST 空快照

	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	done := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(done)
	}()
	<-entered

	// 记录 SSE 到达时刻
	obsTS := nowUnixI()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	close(blockCh)
	<-done

	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 1 {
		t.Fatalf("expected 1 permission, got %+v", snap.Permissions)
	}
	// since 应为 SSE 到达时刻（observedAt），非重放时刻
	if snap.Permissions[0].Since != obsTS {
		t.Errorf("since should be observation time %d, got %d", obsTS, snap.Permissions[0].Since)
	}
}

// --- 接管时旧缓冲归并至旧集合并清空（channel barrier 版本） ---
// design.md D6：断连期间已回复的请求不得复活——旧缓冲事件若不在 REST 快照中即视为已回复，替换后丢弃。

func TestAttention_TakeoverMergesOldBuffer(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	// 手动置旧 owner 状态 + 缓冲
	a.mu.Lock()
	a.perm.ownerEpoch = 1
	a.perm.reconcileEpoch.Store(1)
	a.perm.buffer = []bufferedEvent{{ev: permAsked("r2", "s1", "edit"), observedAt: nowUnixI()}}
	a.mu.Unlock()

	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{
		permReq("r1", "s1", "bash"),
		permReq("r3", "s2", "run"),
	}
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileBackground, nil)

	snap := a.attentionSnapshot()
	ids := make(map[string]bool)
	for _, p := range snap.Permissions {
		ids[p.ID] = true
	}
	if !ids["r1"] || !ids["r3"] {
		t.Fatalf("expected r1(REST)+r3(REST), got %+v", snap.Permissions)
	}
	if ids["r2"] {
		t.Fatalf("r2 should be dropped (not in REST = replied), got %+v", snap.Permissions)
	}
}

// --- 后台 404 时缓冲被丢弃不重放（增量缓冲结果表） ---

func TestAttention_Background404DiscardsBuffer(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	oc := newMockOC(true)
	oc.listPermissionsErr = opencode.ErrCapabilityUnsupported

	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	done := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(done)
	}()
	<-entered
	// REST 在途期间收到 r2 asked（进缓冲）
	a.applyAttentionEvent(permAsked("r2", "s1", "edit"))
	close(blockCh)
	<-done

	snap := a.attentionSnapshot()
	// 404→unsupported：清空集合 + 丢弃缓冲（r1 和 r2 都不在）
	if len(snap.Permissions) != 0 {
		t.Fatalf("404 should clear set + discard buffer, got %+v", snap.Permissions)
	}
	a.mu.Lock()
	if a.perm.cap != capUnsupported {
		t.Errorf("cap = %v, want unsupported", a.perm.cap)
	}
	a.mu.Unlock()
}

// --- align 被后台抢占后旧 align 结果被拒（反向时序） ---

func TestAttention_AlignPreemptedByBackground(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))

	// align 路径阻塞在 REST
	ocAlign := newMockOC(true)
	ocAlign.listPermissionsResult = []opencode.PermissionRequest{
		permReq("r1", "s1", "bash"),
		permReq("r2", "s2", "edit"),
	}
	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrapAlign := &blockingPermOC{inner: ocAlign, block: blockCh, entered: entered}
	alignDone := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrapAlign, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
		close(alignDone)
	}()
	<-entered

	// 后台路径抢占：REST 返回空快照
	ocBg := newMockOC(true)
	ocBg.listPermissionsResult = []opencode.PermissionRequest{}
	a.reconcileAttention(context.Background(), ocBg, "/wt", opencode.AttentionPermission, reconcileBackground, nil)

	// 放行 align REST
	close(blockCh)
	<-alignDone

	// align 结果应被拒（epoch 失配），以后台空快照为准
	snap := a.attentionSnapshot()
	if len(snap.Permissions) != 0 {
		t.Fatalf("align result should be rejected, expected empty, got %+v", snap.Permissions)
	}
}

// --- 后台 REST 在途时发生 SSE 重连 → align 路径新对账抢占、旧结果被拒（channel barrier） ---

func TestAttention_BackgroundPreemptedByAlign(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	oc := newMockOC(true)

	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	oc.listPermissionsResult = []opencode.PermissionRequest{}
	bgDone := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(bgDone)
	}()
	<-entered

	oc2 := newMockOC(true)
	oc2.listPermissionsResult = []opencode.PermissionRequest{
		permReq("r1", "s1", "bash"),
		permReq("r2", "s2", "edit"),
	}
	a.reconcileAttention(context.Background(), oc2, "/wt", opencode.AttentionPermission, reconcileAlign, nil)

	close(blockCh)
	<-bgDone

	snap := a.attentionSnapshot()
	ids := make(map[string]bool)
	for _, p := range snap.Permissions {
		ids[p.ID] = true
	}
	if !ids["r1"] || !ids["r2"] {
		t.Fatalf("expected align snapshot r1+r2, got %+v", snap.Permissions)
	}
}

// --- 非404失败时增量缓冲重放到保留集合（channel barrier） ---

func TestAttention_BackgroundNon404ReplayBuffer(t *testing.T) {
	a := newAttentionState()
	a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
	oc := newMockOC(true)
	oc.listPermissionsErr = errors.New("500")

	blockCh := make(chan struct{})
	entered := make(chan struct{})
	wrap := &blockingPermOC{inner: oc, block: blockCh, entered: entered}
	done := make(chan struct{})
	go func() {
		a.reconcileAttention(context.Background(), wrap, "/wt", opencode.AttentionPermission, reconcileBackground, nil)
		close(done)
	}()
	<-entered
	a.applyAttentionEvent(permAsked("r2", "s1", "edit"))
	close(blockCh)
	<-done

	snap := a.attentionSnapshot()
	ids := make(map[string]bool)
	for _, p := range snap.Permissions {
		ids[p.ID] = true
	}
	if !ids["r1"] || !ids["r2"] {
		t.Fatalf("expected r1(retained)+r2(buffer replay), got %+v", snap.Permissions)
	}
	a.mu.Lock()
	if a.perm.cap != capDegraded {
		t.Errorf("cap = %v, want degraded", a.perm.cap)
	}
	a.mu.Unlock()
}

// --- API 透出：空数组非 null ---

func TestAttention_EmptyArrayNotNull(t *testing.T) {
	a := newAttentionState()
	snap := a.attentionSnapshot()
	if snap.Permissions == nil {
		t.Error("Permissions should be non-nil empty slice, not nil")
	}
	if snap.Questions == nil {
		t.Error("Questions should be non-nil empty slice, not nil")
	}
	if len(snap.Permissions) != 0 || len(snap.Questions) != 0 {
		t.Errorf("expected empty, got %+v", snap)
	}
}

// --- Manager.Attention 与 ListProjectTaskSummaries ---

func TestManager_AttentionSnapshot(t *testing.T) {
	m := newTestManager(t, newMockStore(), newMockProc(), newMockWorktree(), newMockOC(true))
	att, ok := m.Attention("no-such-task")
	if ok {
		t.Error("expected ok=false for no runtime")
	}
	if len(att.Permissions) != 0 || len(att.Questions) != 0 {
		t.Errorf("expected empty, got %+v", att)
	}
}

func TestManager_ListProjectTaskSummaries_Empty(t *testing.T) {
	store := newMockStore()
	m := newTestManager(t, store, newMockProc(), newMockWorktree(), newMockOC(true))
	summaries, err := m.ListProjectTaskSummaries(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summaries == nil {
		t.Fatal("expected non-nil empty slice")
	}
}

// --- degraded 后台重试：经生产入口 Manager.retryAttentionDegraded 并发启动 ---
// 若生产退化为串行，quest 在 perm 释放前不会进入 ListQuestions；
// 并发则两 entered 在 close(block) 前均就绪。MUST 驱动真实入口，禁止复制 goroutine 逻辑。

func TestAttention_BackgroundRetry_ConcurrentTypes(t *testing.T) {
	store := newMockStore()
	seedActiveTask(store, "t1", "p1")
	proc := newMockProc()
	proc.envValues[runtimeSessionName("t1")] = map[string]string{
		"OPENCODE_SERVER_PASSWORD": "pw", "OCDECK_SERVE_PORT": "50001",
	}

	// 先把两类型推到 degraded（对齐种子），再挂到 runtime
	seed := newMockOC(true)
	seed.listPermissionsErr = errors.New("500")
	seed.listQuestionsErr = errors.New("500")
	a := newAttentionState()
	a.reconcileAttention(context.Background(), seed, "/wt", opencode.AttentionPermission, reconcileAlign, nil)
	a.reconcileAttention(context.Background(), seed, "/wt", opencode.AttentionQuestion, reconcileAlign, nil)
	a.mu.Lock()
	if !a.perm.isDegradedLocked() || !a.quest.isDegradedLocked() {
		a.mu.Unlock()
		t.Fatalf("setup: want both degraded, perm=%v quest=%v", a.perm.cap, a.quest.cap)
	}
	a.mu.Unlock()

	oc := newMockOC(true)
	oc.listPermissionsResult = []opencode.PermissionRequest{}
	oc.listQuestionsResult = []opencode.QuestionRequest{}
	blockCh := make(chan struct{})
	permEntered := make(chan struct{})
	questEntered := make(chan struct{})
	wrap := &blockingBothOC{inner: oc, block: blockCh, permEntered: permEntered, questEntered: questEntered}

	// OCFactory 返回 blockingBothOC，经 taskOcClient → retryAttentionDegraded 生产路径
	factory := func(port int, password string, opts opencode.Options) OCClient { return wrap }
	m := newTestManagerWithFactory(t, store, proc, newMockWorktree(), factory)
	rt := m.newRuntime("t1")
	rt.attention = a
	m.setRuntime("t1", rt)

	done := make(chan struct{})
	go func() {
		m.retryAttentionDegraded(context.Background())
		close(done)
	}()

	// 在释放任一 REST 前，两请求均须已进入（串行则 quest 卡在 perm 之后）
	waitEntered := func(name string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not enter REST before release (likely serial, want concurrent via retryAttentionDegraded)", name)
		}
	}
	waitEntered("ListPermissions", permEntered)
	waitEntered("ListQuestions", questEntered)
	close(blockCh)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retryAttentionDegraded concurrent background retry did not finish")
	}
}

// --- degraded 扫描持锁正确（无 data race） ---

func TestAttention_DegradedScanNoRace(t *testing.T) {
	a := newAttentionState()
	oc := newMockOC(true)
	oc.listPermissionsErr = errors.New("500")
	a.reconcileAttention(context.Background(), oc, "/wt", opencode.AttentionPermission, reconcileAlign, nil)

	// 并发：后台扫描 cap + SSE 写入 + 快照读取
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			a.mu.Lock()
			_ = a.perm.isDegradedLocked()
			a.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			a.applyAttentionEvent(permAsked("r1", "s1", "bash"))
			a.applyAttentionEvent(permReplied("s1", "r1"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = a.attentionSnapshot()
		}
	}()
	wg.Wait()
}

// 等待变量使用以避免 unused（time 用于 context.WithTimeout 在其他测试中）
var _ = time.Second
