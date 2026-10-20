// spi_bridge_wiring_test.go 验证 C2 门禁修复的生产接线：注册真实 EventSource 的
// provider 后，task 层 startSSE 经 Provider.Events() 消费 typed domain.AgentEvent
// （recording provider 断言实际调用对应 port——Subscribe 请求携带持久化 binding）。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/config"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"
)

// recordingEventsProvider 记录 Subscribe 调用的 binding（断言生产桥按持久化
// binding 分派），并回放预置 typed 事件。subscribeErr 非 nil → Subscribe 拒绝
// （C9 fixture：适配层拒绝直接进失败补偿，不静默回退）。
type recordingEventsProvider struct {
	minimalProvider
	subscribed   []domain.TaskBinding
	stream       *stubEventStream
	subscribeErr error
}

func (p *recordingEventsProvider) Events() appagent.EventSource {
	return &recordingEventSource{p: p}
}

type recordingEventSource struct{ p *recordingEventsProvider }

func (s *recordingEventSource) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	s.p.subscribed = append(s.p.subscribed, b)
	if s.p.subscribeErr != nil {
		return nil, s.p.subscribeErr
	}
	return s.p.stream, nil
}

// stubEventStream 预置事件回放（SetCallbacks 触发 onConnected——C10 契约形态）。
type stubEventStream struct {
	events chan domain.AgentEvent
	done   chan struct{}
	errV   error
	close  func()
	// C10：连接生命周期回调（订阅方注入；stub 在首事件消费后触发 onConnected）。
	onConnected, onReconnect func()
	// C16：断流回调（stub 记录注册，供消费面断言）。
	onDisconnected func()
}

func newStubEventStream(closeFn func()) *stubEventStream {
	return &stubEventStream{events: make(chan domain.AgentEvent, 8), done: make(chan struct{}), close: closeFn}
}

func (s *stubEventStream) Events() <-chan domain.AgentEvent { return s.events }
func (s *stubEventStream) Err() error                       { return s.errV }
func (s *stubEventStream) SetCallbacks(onConnected, onReconnect func()) {
	s.onConnected, s.onReconnect = onConnected, onReconnect
	// stub 形态：订阅受理回调即视为连接建立（模拟适配层首事件就绪信号——C10 契约
	// 的测试等价；真实 v1 流由 client onReady 驱动）。
	if onConnected != nil {
		go onConnected()
	}
}
func (s *stubEventStream) SetDisconnectCallback(cb func()) { s.onDisconnected = cb }
func (s *stubEventStream) Close() error {
	s.close()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

// TestSSE_TypedBridgeConsumesProviderEvents 固定输入：active 任务、registry 注入
// recording provider（Events 非空）。断言：startSFEFromBinding 经 provider.Events()
// Subscribe（binding 传递至 provider——recording 断言），typed SessionCreated 经
// handleAgentEvent 落归属行（session claim 等价语义），流终止（Close 由 sseCancel
// 触发）后 runtime 清理正常。
//
// mutation 证据（已验证 → 红 → 还原）：将 startSSE 的 typed 桥订阅判定删除
// （stream 构造恒 nil 回退原始通道）→ provider.Subscribe 计数 0 → 本测试
// subscribed==0 红（生产未接 Provider.Events()，仍走原始事件通道）。
func TestSSE_TypedBridgeConsumesProviderEvents(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	seedDerivedBinding(store, "t1", 50001, "/data/worktrees/p1/t1")
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true
	proc.envValues[runtimeSessionName("t1")] = map[string]string{"OCDECK_TASK_ID": "t1"}

	reg := appagent.NewRegistry()
	provider := &recordingEventsProvider{stream: newStubEventStream(func() {})}
	if err := reg.Register(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return &readyOC{inner: newMockOC(true), onReady: opts.OnReady}
		},
		ProviderRegistry: reg,
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	// typed 事件预置：SessionCreated（归属落库断言面）。
	binding, err := m.currentTaskBinding(context.Background(), "t1")
	if err != nil {
		t.Fatalf("current binding: %v", err)
	}
	created, cerr := domain.NewSessionEvent(domain.EventSessionCreated, domain.AgentSession{
		Key:       bindingKeyFor("t1", "ses_new"),
		CreatedAt: 100, UpdatedAt: 150,
	})
	if cerr != nil {
		t.Fatalf("event: %v", cerr)
	}
	provider.stream.events <- created

	if err := m.startSSEFromBinding(context.Background(), rt, "t1", AlignModeRepo); err != nil {
		t.Fatalf("startSSEFromBinding: %v", err)
	}
	defer m.clearRuntime("t1")

	// Subscribe 断言：生产桥经 Provider.Events() 且携带持久化 binding。
	deadline := time.Now().Add(3 * time.Second)
	for len(provider.subscribed) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(provider.subscribed) == 0 {
		t.Fatal("provider.Events().Subscribe MUST be called with persisted binding (C2 typed bridge)")
	}
	if provider.subscribed[0].UpstreamSessionID != binding.UpstreamSessionID ||
		provider.subscribed[0].ProviderID != binding.ProviderID {
		t.Fatalf("subscribed binding = %+v, want %+v", provider.subscribed[0], binding)
	}

	// typed SessionCreated → 归属行落库（claim 等价语义）。
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := store.ListTaskSessions(context.Background(), "t1")
		found := false
		for _, r := range rows {
			if r.SessionID == "ses_new" {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	rows, _ := store.ListTaskSessions(context.Background(), "t1")
	found := false
	for _, r := range rows {
		if r.SessionID == "ses_new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("typed SessionCreated MUST claim ownership, rows=%v", rows)
	}
}

// TestSSE_SubscribeRejectFailsNotFallback（C9 fixture）：适配层 Subscribe 拒绝
// （provider 对非法 binding 的 typed 错误）→ startSSEFromBinding 返回该错误（进失败
// 补偿——零 SSE 假 ready），MUST NOT 静默回退原始通道吞掉拒绝。
//
// mutation 证据（已验证 → 红 → 还原）：typed 桥判定回退吞错（Subscribe err 忽略
// `s, _ :=`）→ 本测试红（startSSEFromBinding 返回 nil——拒绝被吞，回退原始通道
// 假 ready）。
func TestSSE_SubscribeRejectFailsNotFallback(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	markActiveWithSnapshot(store, "t1")
	seedDerivedBinding(store, "t1", 50001, "/data/worktrees/p1/t1")
	proc := newMockProc()
	proc.sessions[runtimeSessionName("t1")] = true

	reg := appagent.NewRegistry()
	provider := &recordingEventsProvider{
		stream:       newStubEventStream(func() {}),
		subscribeErr: appagent.NewError("events.subscribe", appagent.ErrKindBindingInvalid, errors.New("adapter reject")),
	}
	if err := reg.Register(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return &readyOC{inner: newMockOC(true), onReady: opts.OnReady}
		},
		ProviderRegistry: reg,
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }

	rt := m.newRuntime("t1")
	m.setRuntime("t1", rt)
	rt.registerGroup(roleRuntime, runtimeSessionName("t1"))

	err := m.startSSEFromBinding(context.Background(), rt, "t1", AlignModeRepo)
	if err == nil {
		t.Fatal("Subscribe reject MUST fail startSSE (C9: 零 SSE 假 ready，拒绝直接传播)")
	}
	if !strings.Contains(err.Error(), "adapter reject") {
		t.Fatalf("err = %v, want adapter reject 原样传播", err)
	}
}

// TestResolveBinding_AdapterRejectPropagatesNoMirrorFallback（C2 残余 fixture）：
// 真实 provider 的 BindingRuntimeEndpoint 对非法 binding 的拒绝（非法 provider
// instance——镜像不查 instance）直接传播；MUST NOT 回退 v1 镜像重新接受（复审
// C2 点名）。测试接缝形态（CapabilityUnavailable）仍允许镜像回退（过渡裁决 B）。
//
// mutation 证据（已验证 → 红 → 还原）：isTestSeamBindingErr 判定删除（endpoint
// err 一律回退镜像）→ 本测试红（镜像重新接受非法 instance，resolveRuntimeBinding
// 成功返回）。
func TestResolveBinding_AdapterRejectPropagatesNoMirrorFallback(t *testing.T) {
	store := newMockStore()
	seedSuspendedTask(store, "t1", "p1")
	// envelope 合法（provider/instance/upstream/terminal 非空 + data 含 port），
	// 但 provider instance 非法（spi decodeV1Binding 拒绝非 default 实例；镜像不设
	// 该谓词——正是 C2 指出的「镜像重新接受 adapter 拒绝」面）。
	data, _ := json.Marshal(struct {
		Port      int    `json:"port"`
		Directory string `json:"directory"`
		TaskID    string `json:"taskId"`
	}{Port: 50001, Directory: "/data/worktrees/p1/t1", TaskID: "t1"})
	bad := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         "opencode-v1",
		ProviderInstanceID: "rogue-instance",
		UpstreamSessionID:  "ses-anchor-t1",
		TerminalRef:        runtimeSessionName("t1"),
		Data:               data,
	}
	if err := store.SetTaskBinding(context.Background(), "t1", bad); err != nil {
		t.Fatalf("seed rogue binding: %v", err)
	}

	reg := appagent.NewRegistry()
	// 真实 v1 provider（无 supDeps——BindingRuntimeEndpoint 为适配层解码实现；
	// 非法 instance 在 decodeV1Binding 拒绝）。task 包不 import infrastructure/
	// opencode 的既有约定在此不受影响：registry 分派的 provider 实现属于生产装配
	// 面，fixture 经 application Provider 接口消费。
	if err := reg.Register(opencode.NewSPIProvider()); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: 50000, Max: 50999},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: newMockProc(), Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return newMockOC(true)
		},
		ProviderRegistry: reg,
	})
	_, _, err := m.resolveRuntimeBinding(context.Background(), "t1")
	if err == nil {
		t.Fatal("adapter 端点拒绝 MUST 直接传播（零镜像回退）")
	}
	if !strings.Contains(err.Error(), "instance") {
		t.Fatalf("err = %v, want provider instance 拒绝形态", err)
	}
}

// TestRecovery_ResumeViaRuntimeSupervisor 固定输入：runtimeSupervisor 注入（同
// Acquire fixture 形态），active 任务会话异常消失 → ensureRecovery attempt。断言：
// (1) runRecoveryAttempt 经 supervisor.Resume（recording 捕获 TaskRef + old binding
// =持久化值）；(2) 恢复成功经 commitRuntimeReady 原子替换 binding（C3——旧端口被
// supervisor 轮换后新 binding 落库）；(3) 状态收敛 active。
//
// mutation 证据（已验证 → 红 → 还原）：recovery 的 supervisor 判定删除（恒走
// 内联 bootstrapRuntime）→ Resume 计数 0 → 本测试红（C2 恢复建程未接 supervisor）。
func TestRecovery_ResumeViaRuntimeSupervisor(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	// 复活场景预备：锚定/归属由首次激活的 supervisor 建立后再触发恢复——这里直接
	// 注入 supervisor 并先跑一次 Acquire 激活（复用同套 deps）。
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: srv.port + 1, Max: srv.port + 3},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(opts))
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }

	deps := opencode.SupervisorDeps{
		Proc:         proc,
		Store:        &mockSupStoreSeam{store: store},
		SideFx:       &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	recording := &recordingSupervisor{inner: opencode.NewSPIProviderWithDeps(deps).Supervisor()}
	m.runtimeSupervisor = recording

	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	srv.expectDirectory("/data/worktrees/p1/t1")

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	beforeRow, _ := store.GetTask(context.Background(), "t1")
	if beforeRow.Status != StatusActive || !beforeRow.AnchorSessionID.Valid {
		t.Fatalf("precondition: status=%s anchor=%v", beforeRow.Status, beforeRow.AnchorSessionID)
	}
	anchorBefore := beforeRow.AnchorSessionID.String

	// 恢复触发：会话异常消失（watcher 事件）→ ensureRecovery → attempt → Resume。
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := store.GetTask(context.Background(), "t1"); r.Status == StatusActive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	dbgRow, _ := store.GetTask(context.Background(), "t1")
	if dbgRow.Status != StatusActive {
		t.Fatalf("recovery converged to %s, last_error=%q resumed=%d acquired=%d newSessions=%v",
			dbgRow.Status, dbgRow.LastError.String, len(recording.resumed), len(recording.acquired), proc.newSessionNamesSnapshot())
	}

	// Resume recording 断言（第 1 次 Acquire + 第 2 次 Resume）。
	if len(recording.resumed) == 0 {
		t.Fatal("supervisor.Resume MUST be called by runRecoveryAttempt (C2)")
	}
	last := recording.resumed[len(recording.resumed)-1]
	if last.task.TaskID != "t1" {
		t.Fatalf("Resume TaskRef = %+v, want t1", last.task)
	}
	if last.old.UpstreamSessionID != anchorBefore {
		t.Fatalf("Resume old binding UpstreamSessionID = %q, want pre-recovery anchor %q",
			last.old.UpstreamSessionID, anchorBefore)
	}

	// C3：恢复后 binding 原子替换（端口/锚定为 supervisor 重建后的最终值）。
	row, _ := store.GetTask(context.Background(), "t1")
	binding, has, gerr := store.GetTaskBinding(context.Background(), "t1")
	if gerr != nil || !has {
		t.Fatalf("binding after recovery: has=%v err=%v", has, gerr)
	}
	if binding.UpstreamSessionID != row.AnchorSessionID.String {
		t.Fatalf("binding anchor = %q, want task anchor %q (replaced atomically)",
			binding.UpstreamSessionID, row.AnchorSessionID.String)
	}
	var data struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(binding.Data, &data); err != nil {
		t.Fatalf("binding data: %v", err)
	}
	if data.Port != srv.port {
		t.Fatalf("binding port = %d, want fake server port %d (supervisor final runtime)", data.Port, srv.port)
	}
	// 恢复后的 runtime/SSE 归本测试统一清理（clearRuntime join SSE goroutine——
	// 防 fake server Close 等待泄漏的 SSE 连接）。
	t.Cleanup(func() { m.clearRuntime("t1") })
}

// --- C2 门禁修复：激活建程经 Provider.Supervisor() ---

// recordingSupervisor 记录 Acquire/Resume 调用并委托真实 supervisor（其余方法透传）。
type recordingSupervisor struct {
	inner    appagent.RuntimeSupervisor
	acquired []appagent.TaskRef
	resumed  []resumeCallRecord
}

type resumeCallRecord struct {
	task appagent.TaskRef
	old  domain.TaskBinding
}

func (s *recordingSupervisor) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	s.acquired = append(s.acquired, task)
	return s.inner.Acquire(ctx, task)
}

func (s *recordingSupervisor) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	s.resumed = append(s.resumed, resumeCallRecord{task: task, old: old})
	return s.inner.Resume(ctx, task, old, policy)
}

func (s *recordingSupervisor) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	return s.inner.AcquireEphemeral(ctx, task)
}

func (s *recordingSupervisor) Release(ctx context.Context, b domain.TaskBinding) error {
	return s.inner.Release(ctx, b)
}

// bindingKeyFor 构造 v1 命名空间 SessionKey（fixture 助手）。
func bindingKeyFor(taskID, upstream string) domain.SessionKey {
	return domain.SessionKey{ProviderID: store.ProviderIDV1, ProviderInstanceID: store.ProviderInstanceV1Default, UpstreamID: upstream}
}

// recordingSupervisorProvider 是 Provider 形态的 fixture（Supervisor 返回 recording
// 包装；其余能力沿用 minimalProvider fail-closed 形态）。
type recordingSupervisorProvider struct {
	minimalProvider
	sup appagent.RuntimeSupervisor
}

func (p *recordingSupervisorProvider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

// TestActivate_AcquireViaRuntimeSupervisor 固定输入：runtimeSupervisor 注入（真实
// opencode supervisor，AllocatePort 恒指 fake server 端口；proc 会话启动即被 fake
// server 健康检查接纳）。断言：(1) activateRun 经 supervisor.Acquire（recording
// 捕获 TaskRef=任务 ID + worktree 目录）；(2) 激活成功收敛 active（D5 锚定 bootstrap
// 在 supervisor 内完成——claim+set anchor 后 commitRuntimeReady 写 binding+CAS）；
// (3) 持久化 binding 为 supervisor buildBinding 形状（UpstreamSessionID=新建锚定、
// TerminalRef=runtime 会话名、data.port=fake 端口）。
//
// mutation 证据（已验证 → 红 → 还原）：activateRun 的 supervisor 判定删除（恒走
// 内联 bootstrapRuntime 回退）→ Acquire 计数 0 → 本测试红（C2 建程未接 supervisor）。
func TestActivate_AcquireViaRuntimeSupervisor(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: srv.port + 1, Max: srv.port + 3},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(opts))
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }

	// 真实 supervisor：AllocatePort 恒指 fake server 端口（supervisor 内部 NewClient
	// 直连健康检查——不经 ocFactory 映射，与生产装配同形）。
	deps := opencode.SupervisorDeps{
		Proc:         proc,
		Store:        &mockSupStoreSeam{store: store},
		SideFx:       &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	recording := &recordingSupervisor{inner: opencode.NewSPIProviderWithDeps(deps).Supervisor()}
	provider := &recordingSupervisorProvider{sup: recording}
	m.runtimeSupervisor = provider.sup

	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	srv.expectDirectory("/data/worktrees/p1/t1")

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate via supervisor: %v", err)
	}
	defer m.clearRuntime("t1")

	// Acquire recording 断言：建程经 Provider.Supervisor().Acquire。
	if n := len(recording.acquired); n != 1 {
		t.Fatalf("Acquire calls = %d, want 1 (C2: 建程必须经 supervisor 入口)", n)
	}
	if recording.acquired[0].TaskID != "t1" || recording.acquired[0].Directory != "/data/worktrees/p1/t1" {
		t.Fatalf("Acquire TaskRef = %+v, want t1 @ /data/worktrees/p1/t1", recording.acquired[0])
	}

	row, gerr2 := store.GetTask(context.Background(), "t1")
	if gerr2 != nil {
		t.Fatalf("get task: %v", gerr2)
	}
	if row.Status != StatusActive {
		t.Fatalf("status = %s, want active after supervisor Acquire + commit", row.Status)
	}
	// 锚定在 supervisor 内 claim+set（noAnchorDualStart → ClaimSessionKeyAndSetAnchor）。
	if !row.AnchorSessionID.Valid || row.AnchorSessionID.String == "" {
		t.Fatal("anchor MUST be claimed by supervisor dual-start")
	}
	binding, has, gerr := store.GetTaskBinding(context.Background(), "t1")
	if gerr != nil || !has {
		t.Fatalf("binding persisted: has=%v err=%v", has, gerr)
	}
	if binding.UpstreamSessionID != row.AnchorSessionID.String {
		t.Fatalf("binding UpstreamSessionID = %q, want anchor %q", binding.UpstreamSessionID, row.AnchorSessionID.String)
	}
	if binding.TerminalRef != runtimeSessionName("t1") {
		t.Fatalf("binding TerminalRef = %q, want %q", binding.TerminalRef, runtimeSessionName("t1"))
	}
	var data struct {
		Port      int    `json:"port"`
		Directory string `json:"directory"`
		TaskID    string `json:"taskId"`
		Auth      struct {
			Kind string `json:"kind"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(binding.Data, &data); err != nil {
		t.Fatalf("binding data: %v", err)
	}
	if data.Port != srv.port || data.Directory != "/data/worktrees/p1/t1" || data.TaskID != "t1" ||
		data.Auth.Kind != appagent.DerivedCredKind {
		t.Fatalf("binding data = %+v, want fake-port derived shape", data)
	}
}

// TestC13_RecoveryPortThreeWayConsistent 固定输入：supervisor 路径激活成功（端口 =
// fake server 端口）→ watcher 感知异常消失触发自动重拉 → task 层 attempt 顶部
// allocatePort+persistEnvSnapshot（端口分配 owner = task 层；FirstPort=srv+2 死亡 →
// supervisor 健康超时轮换 → attempt>0 轮换分配实际端口=fake 端口并轮换前持久化）→
// 恢复成功收敛 active。断言三方一致（spec「先持久化实际新端口再建程」）：
// (1) 恢复后 env_snapshot.OCDECK_SERVE_PORT == 轮换实际端口（NewSession env 同值）；
// (2) 恢复后 binding data.port == 同值。
//
// mutation 证据（已验证 → 红 → 还原）：①BuildServeEnv seam 删快照叠加（仅
// layerEnvSnapshot）→ attempt 轮换持久化的端口被 supervisor 重算 env 丢弃 →
// binding/持久化值失配 → 本测试红「binding port == 新建 env 端口」断言抓到（或
// snapshot==env 端口断言红）。锚注释在 spi_seams.go BuildServeEnv。
// ②startServeLoop attempt>0 的 PersistServePort 删除 → NewSession 端口先于持久化
// 落库的顺序失去——实际端口不再进持久化 → 「snapshot==env」断言红。
func TestC13_RecoveryPortThreeWayConsistent(t *testing.T) {
	srv := newFakeOCServer(t)
	store := newMockStore()
	charxSeedRepoWorktreeTask(store, "t1", "p1")
	proc := newCharxProc()
	m := New(Options{
		Cfg: &config.Config{
			DataDir:        t.TempDir(),
			ServePortRange: config.PortRange{Min: srv.port + 1, Max: srv.port + 3},
			ShutdownPolicy: config.ShutdownPersist,
		},
		Store: store, Proc: proc, Worktree: newMockWorktree(),
		OCFactory: func(_ string, port int, password string, opts appagent.Options) OCClient {
			return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(opts))
		},
		ProviderRegistry: newTestRegistry(),
	})
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	deps := opencode.SupervisorDeps{
		Proc:         proc,
		Store:        &mockSupStoreSeam{store: store},
		SideFx:       &mockSupSideFx{m: m},
		AllocatePort: func(int, bool, int) (int, error) { return srv.port, nil },
		ReadyTimeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	}
	m.runtimeSupervisor = opencode.NewSPIProviderWithDeps(deps).Supervisor()

	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	srv.expectDirectory("/data/worktrees/p1/t1")

	if err := m.Activate(context.Background(), "t1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { m.clearRuntime("t1") })
	// task 层 attempt 顶部分配的端口必然是死端口（fake server 只在 srv.port 上
	// 健康可达）→ supervisor 健康超时轮换 → attempt>0 轮换分配（AllocatePort seam
	// 指向 fake server 端口）+ 轮换前持久化 → 恢复成功收敛 active。
	// 收敛证据：状态回到 active 且恢复确实执行了 attempt（permit ≥1）。

	// watcher 感知异常消失 → 自动重拉（supervisor.Resume；快照端口采用；轮换收敛）。
	proc.triggerExit(runtimeSessionName("t1"), process.WatchEvent{Type: process.WatchEventSessionExit})
	deadline := time.Now().Add(3 * time.Second)
	for {
		row, err := store.GetTask(context.Background(), "t1")
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if row.Status == StatusActive && store.recoveryPermitCount("t1") >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery did not converge to active: status=%s permits=%d last_error=%v",
				row.Status, store.recoveryPermitCount("t1"), row.LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 三方一致断言（spec「先持久化实际新端口再建程」）：恢复的 NewSession env 端口
	// == 恢复后持久化快照端口 == binding data.port。
	proc.mu.Lock()
	env := map[string]string{}
	for k, v := range proc.envValues[runtimeSessionName("t1")] {
		env[k] = v
	}
	proc.mu.Unlock()
	newSessionPort := env["OCDECK_SERVE_PORT"]
	if newSessionPort == "" {
		t.Fatal("NewSession env OCDECK_SERVE_PORT missing")
	}

	row, err := store.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	var snap envSnapshot
	if err := json.Unmarshal([]byte(row.EnvSnapshot.String), &snap); err != nil {
		t.Fatalf("env snapshot: %v", err)
	}
	if snap.Vars["OCDECK_SERVE_PORT"] != newSessionPort {
		t.Fatalf("persisted snapshot port = %q, want NewSession env port %q（实际端口先持久化，C13）", snap.Vars["OCDECK_SERVE_PORT"], newSessionPort)
	}

	binding, has, gerr := store.GetTaskBinding(context.Background(), "t1")
	if gerr != nil || !has {
		t.Fatalf("binding after recovery: has=%v err=%v", has, gerr)
	}
	var data struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(binding.Data, &data); err != nil {
		t.Fatalf("binding data: %v", err)
	}
	if strconv.Itoa(data.Port) != newSessionPort {
		t.Fatalf("binding data.port = %d, want NewSession env port %q（binding=事实来源与建程端口一致）", data.Port, newSessionPort)
	}
}
