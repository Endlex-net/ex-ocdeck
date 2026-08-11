package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ocdeck/internal/opencode"
)

// --- 三层类型模型：task 层（含 Since，本地首次观察时间） ---

// PendingPermission task 层 pending 权限请求。Since 为本地首次观察 Unix 秒
// （SSE asked 到达时刻；REST 对账同 ID 保留原 since，新 ID 取对账时刻，design.md D6）。
type PendingPermission struct {
	opencode.PermissionRequest
	Since int64
}

// PendingQuestion task 层 pending 问题请求。Since 同 PendingPermission。
type PendingQuestion struct {
	opencode.QuestionRequest
	Since int64
}

// Attention 是任务注意力信号的只读快照（design.md D6 API 透出）。
// 拷贝语义：attentionSnapshot 返回深拷贝，调用方可安全持有。
// 无 pending 时为非 nil 空切片（空数组非 null，spec）。
type Attention struct {
	Permissions []PendingPermission
	Questions   []PendingQuestion
}

// --- 能力状态机（per 任务 × per 类型，独立） ---

type capabilityState int

const (
	capUnknown     capabilityState = iota // 初始态，无旧值透出空数组
	capAvailable                          // 200，正常对账
	capUnsupported                        // 404，停止对账、忽略 SSE、透出空数组
	capDegraded                           // 非 404 错误，保留旧快照+SSE 增量、继续重试
)

// bufferedEvent 增量缓冲项：携带本地首次观察时间（design.md D6：since 为本地首次观察时间，
// 缓冲与 align 共享缓冲只存原始事件 + observedAt，重放时用 observedAt 而非重放时刻）。
type bufferedEvent struct {
	ev         opencode.AttentionEvent
	observedAt int64
}

// permTypeState permission 类型独立状态。
type permTypeState struct {
	cap   capabilityState
	order []string // requestID 有序（登记序）
	perms map[string]PendingPermission

	// reconciling owner（增量缓冲模型，design.md D6）。
	ownerEpoch uint64
	buffer     []bufferedEvent
	// reconcileEpoch 单调递增，推进 MUST NOT 依赖 mu（atomic）。align 与 background 路径
	// 均推进此 epoch 成为合法 owner；写回校验自身 epoch 仍是当前 owner。
	reconcileEpoch atomic.Uint64
}

func newPermTypeState() *permTypeState {
	return &permTypeState{perms: map[string]PendingPermission{}}
}

// questTypeState question 类型独立状态。结构对称 permTypeState。
type questTypeState struct {
	cap    capabilityState
	order  []string
	quests map[string]PendingQuestion

	ownerEpoch     uint64
	buffer         []bufferedEvent
	reconcileEpoch atomic.Uint64
}

func newQuestTypeState() *questTypeState {
	return &questTypeState{quests: map[string]PendingQuestion{}}
}

// attentionState 挂在 taskRuntime 上。mu 覆盖"SSE 事件应用 vs 集合替换"，
// 不覆盖 REST 往返（design.md D6）。attentionEpoch 推进不依赖 mu。
type attentionState struct {
	mu             sync.Mutex
	perm           *permTypeState
	quest          *questTypeState
	attentionEpoch atomic.Uint64
}

func newAttentionState() *attentionState {
	return &attentionState{perm: newPermTypeState(), quest: newQuestTypeState()}
}

// applyAttentionEvent 应用一个注意力 SSE 事件（design.md D6）。
// unsupported 类型忽略；replied/rejected 枚举值不校验一律了结；未知 ID 忽略。
// 后台对账在途时该类型事件追加到增量缓冲而非直接写集合，缓冲项携带 observedAt。
func (a *attentionState) applyAttentionEvent(ev opencode.AttentionEvent) {
	now := nowUnixI()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case opencode.AttentionPermission:
		if a.perm.cap == capUnsupported {
			return
		}
		if a.perm.ownerEpoch != 0 {
			a.perm.buffer = append(a.perm.buffer, bufferedEvent{ev: ev, observedAt: now})
			return
		}
		a.applyPermLocked(ev, now)
	case opencode.AttentionQuestion:
		if a.quest.cap == capUnsupported {
			return
		}
		if a.quest.ownerEpoch != 0 {
			a.quest.buffer = append(a.quest.buffer, bufferedEvent{ev: ev, observedAt: now})
			return
		}
		a.applyQuestLocked(ev, now)
	}
}

func (a *attentionState) applyPermLocked(ev opencode.AttentionEvent, observedAt int64) {
	switch ev.Kind {
	case opencode.AttentionAsked:
		a.upsertPermLocked(ev, observedAt)
	case opencode.AttentionReplied, opencode.AttentionRejected:
		a.removePermLocked(ev.RequestID)
	}
}

func (a *attentionState) applyQuestLocked(ev opencode.AttentionEvent, observedAt int64) {
	switch ev.Kind {
	case opencode.AttentionAsked:
		a.upsertQuestLocked(ev, observedAt)
	case opencode.AttentionReplied, opencode.AttentionRejected:
		a.removeQuestLocked(ev.RequestID)
	}
}

// upsertPermLocked 登记一条 asked。已存在则更新内容、保留原 since（首次观察时间）；
// 新增则用 observedAt（缓冲项携带的首次观察时刻，或直接应用时的 now）。
func (a *attentionState) upsertPermLocked(ev opencode.AttentionEvent, observedAt int64) {
	id := ev.RequestID
	if id == "" {
		return
	}
	since := observedAt
	if old, ok := a.perm.perms[id]; ok {
		since = old.Since
	}
	a.perm.perms[id] = PendingPermission{
		PermissionRequest: opencode.PermissionRequest{
			ID: id, SessionID: ev.SessionID, Permission: ev.Permission, Patterns: ev.Patterns,
		},
		Since: since,
	}
	if !containsString(a.perm.order, id) {
		a.perm.order = append(a.perm.order, id)
	}
}

func (a *attentionState) upsertQuestLocked(ev opencode.AttentionEvent, observedAt int64) {
	id := ev.RequestID
	if id == "" {
		return
	}
	since := observedAt
	if old, ok := a.quest.quests[id]; ok {
		since = old.Since
	}
	a.quest.quests[id] = PendingQuestion{
		QuestionRequest: opencode.QuestionRequest{
			ID: id, SessionID: ev.SessionID, Questions: ev.Questions,
		},
		Since: since,
	}
	if !containsString(a.quest.order, id) {
		a.quest.order = append(a.quest.order, id)
	}
}

func (a *attentionState) removePermLocked(id string) {
	if id == "" {
		return
	}
	if _, ok := a.perm.perms[id]; !ok {
		return
	}
	delete(a.perm.perms, id)
	a.perm.order = removeString(a.perm.order, id)
}

func (a *attentionState) removeQuestLocked(id string) {
	if id == "" {
		return
	}
	if _, ok := a.quest.quests[id]; !ok {
		return
	}
	delete(a.quest.quests, id)
	a.quest.order = removeString(a.quest.order, id)
}

// --- 快照 ---

// attentionSnapshot 返回 pending 集合深拷贝（含 Patterns/Questions 嵌套 slice）。
// 无 pending 时空切片（空数组非 null，spec）。
func (a *attentionState) attentionSnapshot() Attention {
	a.mu.Lock()
	defer a.mu.Unlock()
	perms := make([]PendingPermission, 0, len(a.perm.order))
	for _, id := range a.perm.order {
		p := a.perm.perms[id]
		p.Patterns = copyStrings(p.Patterns)
		perms = append(perms, p)
	}
	quests := make([]PendingQuestion, 0, len(a.quest.order))
	for _, id := range a.quest.order {
		q := a.quest.quests[id]
		q.Questions = copyQuestionItems(q.Questions)
		quests = append(quests, q)
	}
	return Attention{Permissions: perms, Questions: quests}
}

// clearAttention 推进 attentionEpoch（atomic，不依赖 mu）并清空全部集合（短暂持 mu）。
func (a *attentionState) clearAttention() {
	a.attentionEpoch.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.perm = newPermTypeState()
	a.quest = newQuestTypeState()
}

// --- 对账：两条路径 ---

type reconcileMode int

const (
	reconcileAlign reconcileMode = iota
	reconcileBackground
)

// reconcileAttention 对单类型对账。unsupported 停止。context.Canceled 中性。
func (a *attentionState) reconcileAttention(ctx context.Context, oc OCClient, dir string, typ opencode.AttentionType, mode reconcileMode) {
	switch mode {
	case reconcileAlign:
		switch typ {
		case opencode.AttentionPermission:
			a.reconcilePermAlign(ctx, oc, dir)
		case opencode.AttentionQuestion:
			a.reconcileQuestAlign(ctx, oc, dir)
		}
	case reconcileBackground:
		switch typ {
		case opencode.AttentionPermission:
			a.reconcilePermBackground(ctx, oc, dir)
		case opencode.AttentionQuestion:
			a.reconcileQuestBackground(ctx, oc, dir)
		}
	}
}

// --- permission 对账 ---

// reconcilePermAlign align 路径对账：成为合法 owner（ownerEpoch=自身 epoch），
// 归并旧缓冲到旧集合并清空，REST 锁外，写回时校验 reconcile epoch + attentionEpoch + 替换集合。
// 不使用增量缓冲（SSE 事件在 startSSE buffered 中，drainAndRelease 统一重放）。
func (a *attentionState) reconcilePermAlign(ctx context.Context, oc OCClient, dir string) {
	a.mu.Lock()
	if a.perm.cap == capUnsupported {
		a.mu.Unlock()
		return
	}
	// 接管 owner：推进 epoch 使后台在途对账写回被拒，归并旧缓冲到旧集合并清空（design.md D6）。
	ownerEpoch := a.perm.reconcileEpoch.Add(1)
	if a.perm.ownerEpoch != 0 {
		for _, be := range a.perm.buffer {
			a.applyPermLocked(be.ev, be.observedAt)
		}
		a.perm.buffer = nil
	}
	a.perm.ownerEpoch = ownerEpoch
	startEpoch := a.attentionEpoch.Load()
	a.mu.Unlock()

	result, err := oc.ListPermissions(ctx, dir)
	if neutralCancel(ctx, err) {
		a.mu.Lock()
		if a.perm.ownerEpoch == ownerEpoch {
			a.perm.ownerEpoch = 0
			a.perm.buffer = nil
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// 写回校验：既是当前 owner 且 attentionEpoch 未变（design.md D6 双触发仲裁 + 生命周期仲裁）。
	if a.perm.ownerEpoch != ownerEpoch || a.attentionEpoch.Load() != startEpoch {
		return // 被抢占或挂起/删除：不写回
	}
	a.perm.ownerEpoch = 0
	a.perm.buffer = nil
	if err != nil {
		a.transitionPermCapLocked(err)
		return
	}
	a.replacePermLocked(result, nowUnixI())
	a.perm.cap = capAvailable
}

func (a *attentionState) reconcilePermBackground(ctx context.Context, oc OCClient, dir string) {
	a.mu.Lock()
	if a.perm.cap == capUnsupported {
		a.mu.Unlock()
		return
	}
	ownerEpoch := a.perm.reconcileEpoch.Add(1)
	// 接管：归并旧 owner 缓冲到旧集合并清空（design.md D6）。
	if a.perm.ownerEpoch != 0 {
		for _, be := range a.perm.buffer {
			a.applyPermLocked(be.ev, be.observedAt)
		}
		a.perm.buffer = nil
	}
	a.perm.ownerEpoch = ownerEpoch
	startEpoch := a.attentionEpoch.Load()
	a.mu.Unlock()

	result, err := oc.ListPermissions(ctx, dir)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perm.ownerEpoch != ownerEpoch {
		// 被抢占：MUST NOT 触碰 reconciling 标记与缓冲（design.md D6）。
		return
	}
	// 仅 owner 可清标记与动缓冲。
	a.perm.ownerEpoch = 0
	if neutralCancel(ctx, err) {
		a.perm.buffer = nil
		return // 中性：不写回、不迁移
	}
	if a.attentionEpoch.Load() != startEpoch {
		a.perm.buffer = nil
		return // 挂起/删除：不写回
	}
	if err != nil {
		if errors.Is(err, opencode.ErrCapabilityUnsupported) {
			// 404→unsupported：清空集合 + 丢弃缓冲（design.md D6 增量缓冲结果表）。
			a.perm.cap = capUnsupported
			a.perm.perms = map[string]PendingPermission{}
			a.perm.order = nil
			a.perm.buffer = nil
		} else {
			// 非 404→degraded：保留旧集合并按序重放增量缓冲到保留集合（事件真实发生过，MUST NOT 丢）。
			a.perm.cap = capDegraded
			for _, be := range a.perm.buffer {
				a.applyPermLocked(be.ev, be.observedAt)
			}
			a.perm.buffer = nil
		}
		return
	}
	// 200：替换集合后按序重放增量缓冲到新集合。
	a.replacePermLocked(result, nowUnixI())
	for _, be := range a.perm.buffer {
		a.applyPermLocked(be.ev, be.observedAt)
	}
	a.perm.buffer = nil
	a.perm.cap = capAvailable
}

func (a *attentionState) replacePermLocked(perms []opencode.PermissionRequest, givenSince int64) {
	newMap := map[string]PendingPermission{}
	newOrder := make([]string, 0, len(perms))
	for _, p := range perms {
		since := givenSince
		if old, ok := a.perm.perms[p.ID]; ok {
			since = old.Since
		}
		newMap[p.ID] = PendingPermission{PermissionRequest: p, Since: since}
		newOrder = append(newOrder, p.ID)
	}
	a.perm.perms = newMap
	a.perm.order = newOrder
}

// transitionPermCapLocked 按 REST 错误迁移能力状态机（align 路径用，后台 404 单独处理缓冲丢弃）。
// nil err（200）→ available；404 → unsupported + 清空；非 404 → degraded（保留旧集合）。
func (a *attentionState) transitionPermCapLocked(err error) {
	if err == nil {
		a.perm.cap = capAvailable
		return
	}
	if errors.Is(err, opencode.ErrCapabilityUnsupported) {
		a.perm.cap = capUnsupported
		a.perm.perms = map[string]PendingPermission{}
		a.perm.order = nil
		return
	}
	a.perm.cap = capDegraded
}

// --- question 对账 ---

func (a *attentionState) reconcileQuestAlign(ctx context.Context, oc OCClient, dir string) {
	a.mu.Lock()
	if a.quest.cap == capUnsupported {
		a.mu.Unlock()
		return
	}
	ownerEpoch := a.quest.reconcileEpoch.Add(1)
	if a.quest.ownerEpoch != 0 {
		for _, be := range a.quest.buffer {
			a.applyQuestLocked(be.ev, be.observedAt)
		}
		a.quest.buffer = nil
	}
	a.quest.ownerEpoch = ownerEpoch
	startEpoch := a.attentionEpoch.Load()
	a.mu.Unlock()

	result, err := oc.ListQuestions(ctx, dir)
	if neutralCancel(ctx, err) {
		a.mu.Lock()
		if a.quest.ownerEpoch == ownerEpoch {
			a.quest.ownerEpoch = 0
			a.quest.buffer = nil
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quest.ownerEpoch != ownerEpoch || a.attentionEpoch.Load() != startEpoch {
		return
	}
	a.quest.ownerEpoch = 0
	a.quest.buffer = nil
	if err != nil {
		a.transitionQuestCapLocked(err)
		return
	}
	a.replaceQuestLocked(result, nowUnixI())
	a.quest.cap = capAvailable
}

func (a *attentionState) reconcileQuestBackground(ctx context.Context, oc OCClient, dir string) {
	a.mu.Lock()
	if a.quest.cap == capUnsupported {
		a.mu.Unlock()
		return
	}
	ownerEpoch := a.quest.reconcileEpoch.Add(1)
	if a.quest.ownerEpoch != 0 {
		for _, be := range a.quest.buffer {
			a.applyQuestLocked(be.ev, be.observedAt)
		}
		a.quest.buffer = nil
	}
	a.quest.ownerEpoch = ownerEpoch
	startEpoch := a.attentionEpoch.Load()
	a.mu.Unlock()

	result, err := oc.ListQuestions(ctx, dir)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quest.ownerEpoch != ownerEpoch {
		return // 被抢占：MUST NOT 触碰标记与缓冲
	}
	a.quest.ownerEpoch = 0
	if neutralCancel(ctx, err) {
		a.quest.buffer = nil
		return
	}
	if a.attentionEpoch.Load() != startEpoch {
		a.quest.buffer = nil
		return
	}
	if err != nil {
		if errors.Is(err, opencode.ErrCapabilityUnsupported) {
			a.quest.cap = capUnsupported
			a.quest.quests = map[string]PendingQuestion{}
			a.quest.order = nil
			a.quest.buffer = nil
		} else {
			a.quest.cap = capDegraded
			for _, be := range a.quest.buffer {
				a.applyQuestLocked(be.ev, be.observedAt)
			}
			a.quest.buffer = nil
		}
		return
	}
	a.replaceQuestLocked(result, nowUnixI())
	for _, be := range a.quest.buffer {
		a.applyQuestLocked(be.ev, be.observedAt)
	}
	a.quest.cap = capAvailable
}

func (a *attentionState) replaceQuestLocked(quests []opencode.QuestionRequest, givenSince int64) {
	newMap := map[string]PendingQuestion{}
	newOrder := make([]string, 0, len(quests))
	for _, q := range quests {
		since := givenSince
		if old, ok := a.quest.quests[q.ID]; ok {
			since = old.Since
		}
		newMap[q.ID] = PendingQuestion{QuestionRequest: q, Since: since}
		newOrder = append(newOrder, q.ID)
	}
	a.quest.quests = newMap
	a.quest.order = newOrder
}

func (a *attentionState) transitionQuestCapLocked(err error) {
	if err == nil {
		a.quest.cap = capAvailable
		return
	}
	if errors.Is(err, opencode.ErrCapabilityUnsupported) {
		a.quest.cap = capUnsupported
		a.quest.quests = map[string]PendingQuestion{}
		a.quest.order = nil
		return
	}
	a.quest.cap = capDegraded
}

// isDegradedLocked 状态查询（后台重试筛选，调用方持 attentionState.mu）。
func (p *permTypeState) isDegradedLocked() bool  { return p.cap == capDegraded }
func (q *questTypeState) isDegradedLocked() bool { return q.cap == capDegraded }

// --- taskRuntime 接入 ---

func (rt *taskRuntime) ensureAttentionState() *attentionState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.attention == nil {
		rt.attention = newAttentionState()
	}
	return rt.attention
}

func (rt *taskRuntime) applyAttentionEvent(ev opencode.AttentionEvent) {
	rt.mu.Lock()
	a := rt.attention
	rt.mu.Unlock()
	if a == nil {
		return
	}
	a.applyAttentionEvent(ev)
}

func (rt *taskRuntime) attentionSnapshot() Attention {
	rt.mu.Lock()
	a := rt.attention
	rt.mu.Unlock()
	if a == nil {
		return Attention{Permissions: []PendingPermission{}, Questions: []PendingQuestion{}}
	}
	return a.attentionSnapshot()
}

func (rt *taskRuntime) clearAttention() {
	rt.mu.Lock()
	a := rt.attention
	rt.mu.Unlock()
	if a == nil {
		return
	}
	a.clearAttention()
}

// --- Manager: Attention 快照 API ---

// Attention 返回任务注意力快照。非 active/无 runtime 返回空快照 + false。
func (m *Manager) Attention(taskID string) (Attention, bool) {
	rt := m.getRuntime(taskID)
	if rt == nil {
		return Attention{Permissions: []PendingPermission{}, Questions: []PendingQuestion{}}, false
	}
	return rt.attentionSnapshot(), true
}

// reconcileTaskAttention 激活/SSE 重连 align 路径对账（两类型，REST 并发）。
// 在 alignMu 内、session align 成功后、drainAndRelease 前调用。失败不影响任务状态机。
func (m *Manager) reconcileTaskAttention(ctx context.Context, rt *taskRuntime, oc OCClient, dir string) {
	a := rt.ensureAttentionState()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a.reconcileAttention(ctx, oc, dir, opencode.AttentionPermission, reconcileAlign)
	}()
	go func() {
		defer wg.Done()
		a.reconcileAttention(ctx, oc, dir, opencode.AttentionQuestion, reconcileAlign)
	}()
	wg.Wait()
}

// --- 后台 30s 周期重试（degraded） ---

// retryAttentionDegraded 扫描活跃 runtime，对 degraded 类型触发后台路径对账（design.md D6）。
// 挂入既有 backgroundLoop，MUST NOT 新增 goroutine/定时器。
func (m *Manager) retryAttentionDegraded(ctx context.Context) {
	m.rtMu.Lock()
	ids := make([]string, 0, len(m.runtimes))
	for id := range m.runtimes {
		ids = append(ids, id)
	}
	m.rtMu.Unlock()

	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		rt := m.getRuntime(id)
		if rt == nil {
			continue
		}
		rt.mu.Lock()
		a := rt.attention
		rt.mu.Unlock()
		if a == nil {
			continue
		}
		// 扫描 cap 需持 attentionState.mu（与集合替换互斥，data race 安全）。
		a.mu.Lock()
		permDegraded := a.perm.isDegradedLocked()
		questDegraded := a.quest.isDegradedLocked()
		a.mu.Unlock()
		if !permDegraded && !questDegraded {
			continue
		}
		oc, dir, ok := m.taskOcClient(ctx, id)
		if !ok {
			continue
		}
		// 两类型并发启动并等待（与 align 路径同形；类型独立、不串行拖慢 30s 周期）
		var wg sync.WaitGroup
		if permDegraded {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.reconcileAttention(ctx, oc, dir, opencode.AttentionPermission, reconcileBackground)
			}()
		}
		if questDegraded {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.reconcileAttention(ctx, oc, dir, opencode.AttentionQuestion, reconcileBackground)
			}()
		}
		wg.Wait()
	}
}

// taskOcClient 为活跃任务构造一次性 OCClient 与 directory（后台对账用）。
func (m *Manager) taskOcClient(ctx context.Context, taskID string) (OCClient, string, bool) {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil || row.Status != StatusActive {
		return nil, "", false
	}
	serveName := serveSessionName(taskID)
	password, perr := m.proc.ShowSessionEnvContext(ctx, serveName, "OPENCODE_SERVER_PASSWORD")
	if perr != nil || password == "" {
		return nil, "", false
	}
	portStr, perr := m.proc.ShowSessionEnvContext(ctx, serveName, "OCDECK_SERVE_PORT")
	if perr != nil || portStr == "" {
		return nil, "", false
	}
	port, ok := parsePort(portStr)
	if !ok {
		return nil, "", false
	}
	oc := m.ocFactory(port, password, opencode.Options{
		HealthTimeout: 2 * time.Second,
		OpTimeout:     5 * time.Second,
	})
	return oc, row.WorktreePath, true
}

// --- ProjectTaskSummary（GET /projects tasks 摘要，design.md D4 11 字段） ---

// ProjectTaskSummary 项目任务摘要（design.md D4：10 存储字段 + attention_count）。
type ProjectTaskSummary struct {
	TaskID         string
	Name           string
	ProjectID      string
	Status         string
	InitStatus     string
	Branch         string
	WorktreePath   string
	LastError      string
	Notice         string
	UpdatedAt      int64
	AttentionCount int
}

// ListProjectTaskSummaries 聚合全部任务摘要。纯读聚合；store 失败返回错误（API 层 500 不水合）。
func (m *Manager) ListProjectTaskSummaries(ctx context.Context) ([]ProjectTaskSummary, error) {
	rows, err := m.store.ListAllTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all tasks for summaries: %w", err)
	}
	out := make([]ProjectTaskSummary, 0, len(rows))
	for _, t := range rows {
		summary := ProjectTaskSummary{
			TaskID: t.ID, Name: t.Name, ProjectID: t.ProjectID, Status: t.Status,
			InitStatus: t.InitStatus, Branch: t.Branch, WorktreePath: t.WorktreePath,
			UpdatedAt: t.UpdatedAt,
		}
		if t.LastError.Valid {
			summary.LastError = t.LastError.String
		}
		if t.Notice.Valid {
			summary.Notice = t.Notice.String
		}
		att, _ := m.Attention(t.ID)
		summary.AttentionCount = len(att.Permissions) + len(att.Questions)
		out = append(out, summary)
	}
	return out, nil
}

// --- 辅助 ---

// neutralCancel 判断对账请求是否因 context 取消而需中性处理（design.md D6）。
// 仅 context.Canceled 为中性（不迁移、不写回）；context.DeadlineExceeded 属超时 → degraded。
// ctx 已取消但 err==nil（REST 成功返回但 ctx 已被取消）也视为中性，避免用已取消结果写回。
func neutralCancel(ctx context.Context, err error) bool {
	if ctxErr := ctx.Err(); ctxErr != nil {
		// ctx 已取消（含 Canceled 与 DeadlineExceeded）：err 可能是 nil 或 wrapped ctx.Err。
		// Canceled 中性；DeadlineExceeded 在 err 非 nil 时按超时→degraded 处理（非中性），
		// 但 err==nil 时无法区分，保守视为中性（不写回已取消的结果）。
		if err == nil {
			return true // ctx 取消且无错误结果：不写回（避免用已取消 ctx 的结果）
		}
		return errors.Is(err, context.Canceled)
	}
	// ctx 未取消但 err 是 Canceled（上游传入 canceled ctx 给 mock 的场景）：中性。
	return err != nil && errors.Is(err, context.Canceled)
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeString(s []string, v string) []string {
	for i, x := range s {
		if x == v {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func copyQuestionItems(qs []opencode.QuestionItem) []opencode.QuestionItem {
	if qs == nil {
		return nil
	}
	out := make([]opencode.QuestionItem, len(qs))
	copy(out, qs)
	return out
}
