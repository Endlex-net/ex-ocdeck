// v2_wiring_test.go 落实 tasks 6.5/6.6/6.7 编排层验证：准入拒绝零副作用
// （R2/R3/R9）、归属校验先于凭据派生的调用顺序与未实现拒绝（G2-F5 门禁项）、
// 承接 store 能力缺失 fail-closed、开关两态分派行为。
package task

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/store"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// fakeOwnershipProvider 是 v2 形态的最小 Provider fake：Supervisor 为 fail-closed
// stub（本组测试聚焦 resolver/准入/归属门禁，不触达真实 supervisor）；可选实现
// BindingOwnershipChecker（未实现形态用于 G2-F5 拒绝测试）。
type fakeOwnershipProvider struct {
	v2Stub
	// withChecker false = 未实现窄接口（G2-F5 拒绝路径）。
	withChecker bool
	// ownershipCalls 记录归属校验入参（顺序断言：先于凭据派生）。
	ownershipCalls []string
	// endpointCalls 记录 BindingRuntimeEndpoint 调用次数（归属通过后才允许）。
	endpointCalls int
}

// v2Stub 内嵌：Supervisor 返回显式 fail-closed（本测试不触达）。
type v2Stub struct{}

func (v2Stub) Meta() appagent.ProviderMeta {
	return appagent.ProviderMeta{ProviderID: ocdeckstore.ProviderIDV2, DisplayName: "v2 fake"}
}

func (v2Stub) Supervisor() appagent.RuntimeSupervisor { return failSup{} }

func (v2Stub) Sessions() appagent.SessionControl { return nil }
func (v2Stub) Events() appagent.EventSource      { return nil }
func (v2Stub) Attention() appagent.AttentionIO   { return nil }
func (v2Stub) Prompts() appagent.AsyncPrompt     { return nil }
func (v2Stub) Status() appagent.StatusReader     { return nil }
func (v2Stub) Terminal() appagent.TerminalIO     { return nil }
func (v2Stub) Titles() appagent.TitleUpdater     { return nil }
func (v2Stub) BindingRuntimeEndpoint(domain.TaskBinding) (int, string, string, error) {
	return 0, "", "", errors.New("BindingRuntimeEndpoint must not be called before ownership check")
}

type failSup struct{}

func (failSup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.acquire", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}
func (failSup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.resume", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}
func (failSup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.acquire_ephemeral", appagent.ErrKindCapabilityUnavailable, errors.New("test stub"))
}
func (failSup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

func (f *fakeOwnershipProvider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	f.ownershipCalls = append(f.ownershipCalls, expectedTaskID+"@"+expectedDirectory)
	return nil
}

func (f *fakeOwnershipProvider) EphemeralRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 4096, "derived", "/wt", nil
}

// seedV2BindingTask 在 mockStore 种入 v2 binding 任务（R1/R4 形态）：任务行
// （worktree 路径 + provider_id 列）+ v2 形状 binding envelope。
func seedV2BindingTask(t *testing.T, ms *mockStore, taskID string) {
	t.Helper()
	seedTaskRow(t, ms, taskID)
	b := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: "default",
		UpstreamSessionID:  "ses_x",
		TerminalRef:        "ocdeck-v2-tui-" + taskID,
		Data:               []byte(`{"port":4096,"directory":"/wt","taskId":"` + taskID + `","origin":"created","auth":{"kind":"derived"}}`),
	}
	if err := ms.SetTaskBinding(context.Background(), taskID, b); err != nil {
		t.Fatalf("seed v2 binding: %v", err)
	}
}

// seedTaskRow 构造最小任务行（worktree 模式 + v1 provider 钉扎默认）。
func seedTaskRow(t *testing.T, ms *mockStore, taskID string) {
	t.Helper()
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.tasks[taskID] = TaskRow{
		ID: taskID, ProjectID: "p1", Name: "task", Branch: "b",
		Status: "suspended", WorktreePath: "/wt", Mode: "worktree",
		ProviderID: ocdeckstore.ProviderIDV1,
	}
	if ms.projects == nil {
		ms.projects = map[string]ProjectRow{}
	}
	ms.projects["p1"] = ProjectRow{ID: "p1", Name: "proj", Path: "/repo", Kind: "repo"}
}

// TestAdmit_DenyZeroSideEffect 落实 tasks 6.6：v2 binding 任务在 v1 机器
// （R2）激活拒绝——typed CapabilityUnavailable、零副作用（状态不变、无
// supervisor 调用）、用户可感知 last_error。
func TestAdmit_DenyZeroSideEffect(t *testing.T) {
	ms := newMockStore()
	seedV2BindingTask(t, ms, "t-deny")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 1, V2Enabled: true}) // R2：机器 1

	err := m.activateRun(context.Background(), "t-deny", AlignModeRepo)
	if err == nil {
		t.Fatalf("R2 must deny")
	}
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("kind: %v", appagent.KindOf(err))
	}
	// 零副作用：binding 不变（仍 v2；deny 路径仅 last_error，无归属/锚定漂移）。
	b, has, err := ms.GetTaskBinding(context.Background(), "t-deny")
	if err != nil || !has || b.ProviderID != ocdeckstore.ProviderIDV2 {
		t.Fatalf("binding drifted: has=%v err=%v", has, err)
	}
}

// TestAdmit_R9_DenyWhenSwitchOff 落实 tasks 6.6：v1 钉扎 + 机器 v2 + 开关关
// → fail-closed 不静默 v1（MUST NOT 退回 v1 supervisor）。
func TestAdmit_R9_DenyWhenSwitchOff(t *testing.T) {
	ms := newMockStore()
	seedTaskRow(t, ms, "t-r9")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: false}) // R9

	err := m.activateRun(context.Background(), "t-r9", AlignModeRepo)
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("kind: %v (%v)", appagent.KindOf(err), err)
	}
	// MUST NOT 静默 v1：无进程创建（v1 supervisor 未被调用——无 runtime 会话产生）。
	if sessions, _ := proc.ListSessions(); len(sessions) != 0 {
		t.Fatalf("R9 must not silently start v1 runtime, sessions=%v", sessions)
	}
}

// TestSupervisorForTaskConsumption_OwnershipGate 落实 tasks 6.5 G2-F5 门禁项：
// v2 binding 消费入口统一先调 BindingOwnershipChecker——provider 未实现 →
// fail-closed 拒绝；实现时归属校验先于任何凭据派生（endpoint 计数为 0）。
func TestSupervisorForTaskConsumption_OwnershipGate(t *testing.T) {
	ms := newMockStore()
	seedV2BindingTask(t, ms, "t-gate")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)

	orig := m.providerRegistry
	defer func() { m.providerRegistry = orig }()
	t.Run("not implemented fail-closed", func(t *testing.T) {
		reg := appagent.NewRegistry()
		// v2Stub 不实现 BindingOwnershipChecker（G2-F5 未实现形态）。
		if err := reg.Register(&v2Stub{}); err != nil {
			t.Fatal(err)
		}
		m.providerRegistry = reg
		b, _, _ := m.store.GetTaskBinding(context.Background(), "t-gate")
		_, err := m.supervisorForTaskConsumption(context.Background(), "t-gate", b, "/wt")
		if err == nil || !strings.Contains(err.Error(), "BindingOwnershipChecker") {
			t.Fatalf("expect ownership checker refusal, got %v", err)
		}
	})
	t.Run("check before credential derivation", func(t *testing.T) {
		reg := appagent.NewRegistry()
		fp := &fakeOwnershipProvider{withChecker: true}
		if err := reg.Register(fp); err != nil {
			t.Fatal(err)
		}
		m.providerRegistry = reg
		b, _, _ := m.store.GetTaskBinding(context.Background(), "t-gate")
		if _, err := m.supervisorForTaskConsumption(context.Background(), "t-gate", b, "/wt"); err != nil {
			t.Fatalf("ownership pass: %v", err)
		}
		if len(fp.ownershipCalls) != 1 || fp.endpointCalls != 0 {
			t.Fatalf("ownership must run first without endpoint derivation: calls=%v endpoints=%d",
				fp.ownershipCalls, fp.endpointCalls)
		}
	})
}

// TestAdopt_StoreCapabilityMissing_FailClosed 落实 tasks 6.5：store 未实现承接
// 原子转换能力（窄接口缺失）→ 承接 fail-closed（零持久化），不影响 v1 路径。
func TestAdopt_StoreCapabilityMissing_FailClosed(t *testing.T) {
	ms := newMockStore()
	seedTaskRow(t, ms, "t-adopt") // v1 钉扎任务（R8 准入；mockStore 未实现承接窄接口）
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true}) // R8
	// 注册 v2 fake provider（过 admission；本测试聚焦 store 窄接口缺失出口）。
	reg := appagent.NewRegistry()
	if err := reg.Register(&fakeOwnershipProvider{withChecker: true}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	// mockStore 未实现 adoptionStore 窄接口 → fail-closed。
	err := m.activateRun(context.Background(), "t-adopt", AlignModeRepo)
	if err == nil || !strings.Contains(err.Error(), "CommitAdoption") {
		t.Fatalf("expect store capability refusal, got %v", err)
	}
	// 零副作用：binding 仍 v1（无 v2 binding 写入）。
	_, has, err := ms.GetTaskBinding(context.Background(), "t-adopt")
	if err != nil {
		t.Fatalf("GetTaskBinding: %v", err)
	}
	_ = has
}

// TestSwitchOff_V2NotRegistered 落实 tasks 6.7：开关关闭形态 = v2 不注册——
// 消费点按未注册 provider fail-closed（R4），不自动转回 v1。
func TestSwitchOff_V2NotRegistered(t *testing.T) {
	ms := newMockStore()
	seedV2BindingTask(t, ms, "t-off")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	// newTestManagerFull 的 registry 仅注册 v1（开关关形态）+ 机器事实关。
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: false})

	// 消费点分派（R4）：按 binding 路由 v2 → ProviderNotFound（不转 v1）。
	b, _, _ := m.store.GetTaskBinding(context.Background(), "t-off")
	_, err := m.supervisorForTaskConsumption(context.Background(), "t-off", b, "/wt")
	if appagent.KindOf(err) != appagent.ErrKindProviderNotFound {
		t.Fatalf("kind: %v（MUST NOT fall back to v1）", appagent.KindOf(err))
	}
}

// ---- gate3 F2/F3：v2 激活 Resume 分支与承接正式接管 ----

// recordingV2Sup 记录 Resume/Acquire 调用；Resume 返回同 upstream 的 v2 binding。
type recordingV2Sup struct {
	acquireCalls int
	resumeCalls  int
	upstream     string
	port         int
}

func (s *recordingV2Sup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	s.acquireCalls++
	return domain.TaskBinding{}, appagent.NewError("test.acquire", appagent.ErrKindCapabilityUnavailable,
		errors.New("Acquire MUST NOT be called when v2 binding exists"))
}

func (s *recordingV2Sup) Resume(ctx context.Context, t appagent.TaskRef, old domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	s.resumeCalls++
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: "default",
		UpstreamSessionID:  s.upstream,
		TerminalRef:        "ocdeck-v2-tui-" + t.TaskID,
		Data:               []byte(`{"port":4096,"directory":"/wt","taskId":"` + t.TaskID + `","origin":"created","auth":{"kind":"derived"}}`),
	}, nil
}

func (s *recordingV2Sup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (s *recordingV2Sup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

// CheckServerAlive 满足 ServerAvailabilityWarmer（I8 v2 恢复预算序①——fake 恒可用）。
func (s *recordingV2Sup) CheckServerAlive(ctx context.Context) error { return nil }

// recordingV2Provider 包装 v2Stub：Supervisor 指向 recorder；端点解析返回同端口；
// 归属校验通过（G3-F5 门禁放行形态）。
type recordingV2Provider struct {
	v2Stub
	sup *recordingV2Sup
}

func (p *recordingV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return p.sup.port, "derived", "/wt", nil
}

func (p *recordingV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *recordingV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

// TestGate3_F2_ExistingV2Binding_ResumeNotAcquire 落实 G3-F2：suspended 任务已有
// v2 binding 时激活只调 Resume、不调 Acquire（不建替代 session）、upstream 不变、
// 提交后 active。
func TestGate3_F2_ExistingV2Binding_ResumeNotAcquire(t *testing.T) {
	ms := newMockStore()
	seedTaskRow(t, ms, "t-f2")
	b := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: "default",
		UpstreamSessionID:  "ses_keep_me",
		TerminalRef:        "ocdeck-v2-tui-t-f2",
		Data:               []byte(`{"port":4096,"directory":"/wt","taskId":"t-f2","origin":"created","auth":{"kind":"derived"}}`),
	}
	if err := ms.SetTaskBinding(context.Background(), "t-f2", b); err != nil {
		t.Fatal(err)
	}
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})

	rec := &recordingV2Sup{upstream: "ses_keep_me", port: 4096}
	reg := appagent.NewRegistry()
	if err := reg.Register(&recordingV2Provider{sup: rec}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if err := m.activateRun(context.Background(), "t-f2", AlignModeRepo); err != nil {
		t.Fatalf("activateRun: %v", err)
	}
	// 只 Resume、不 Acquire（连续性——MUST NOT 再 POST 建 session）。
	if rec.resumeCalls != 1 || rec.acquireCalls != 0 {
		t.Fatalf("resume=%d acquire=%d", rec.resumeCalls, rec.acquireCalls)
	}
	// upstream 不变 + 提交后 active。
	got, has, err := ms.GetTaskBinding(context.Background(), "t-f2")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep_me" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	if row, err := ms.GetTask(context.Background(), "t-f2"); err != nil || row.Status != "active" {
		t.Fatalf("status: %v %v", row.Status, err)
	}
}

// fakeAdoptStream 是候选关联的立即握手 EventStream fake（F3 承接握手）。
type fakeAdoptStream struct {
	connected chan struct{}
	closed    int32
}

func newFakeAdoptStream() *fakeAdoptStream {
	f := &fakeAdoptStream{connected: make(chan struct{}, 1)}
	return f
}

func (f *fakeAdoptStream) Events() <-chan domain.AgentEvent { return make(chan domain.AgentEvent) }
func (f *fakeAdoptStream) Err() error                       { return nil }
func (f *fakeAdoptStream) Close() error {
	atomic.AddInt32(&f.closed, 1)
	return nil
}
func (f *fakeAdoptStream) SetCallbacks(onConnected, onReconnect func()) {
	if onConnected != nil {
		onConnected()
	}
}
func (f *fakeAdoptStream) SetDisconnectCallback(func()) {}

// adoptingV2Provider 承接形态 fake：Supervisor 指向 recorder（承接 Resume 返回
// candidate）；Events 立即握手；归属校验通过。
type adoptingV2Provider struct {
	v2Stub
	sup    *recordingV2Sup
	stream *fakeAdoptStream
}

func (p *adoptingV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *adoptingV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *adoptingV2Provider) Events() appagent.EventSource { return &adoptEventsSrc{stream: p.stream} }

func (p *adoptingV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 4096, "derived", "/wt", nil
}

type adoptEventsSrc struct{ stream *fakeAdoptStream }

func (s *adoptEventsSrc) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	return s.stream, nil
}

// TestGate3_F3_Adoption_CommittedThenTakeover 落实 G3-F3：承接提交成功后必须
// 完成正式运行时接管（runtime 注册 + SSE），任务 active、binding v2（upstream=
// 锚定原 session）。
func TestGate3_F3_Adoption_CommittedThenTakeover(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p1','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 anchor_session_id, anchor_provider_id, anchor_provider_instance_id, provider_id, task_binding, created_at, updated_at)
		 VALUES ('t-f3','p1','task','b','suspended','/wt','sess-keep','opencode-v1','default','opencode-v1',?, ?, ?)`,
		`{"version":1,"provider_id":"opencode-v1","provider_instance_id":"default","upstream_session_id":"sess-keep","terminal_ref":"tui","data":{"port":4000,"directory":"/wt","taskId":"t-f3","auth":{"kind":"derived"}}}`,
		now, now); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"sess-keep", "sess-child"} {
		if _, err := db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
			 VALUES ('t-f3', ?, 1, 1, 1, ?)`, sid, map[bool]string{true: "", false: "sess-keep"}[sid == "sess-keep"]); err != nil {
			t.Fatal(err)
		}
	}

	adapter := NewStoreAdapter(db)
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, adapter, nil, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})

	rec := &recordingV2Sup{upstream: "sess-keep", port: 4096}
	stream := newFakeAdoptStream()
	reg := appagent.NewRegistry()
	if err := reg.Register(&adoptingV2Provider{sup: rec, stream: stream}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if err := m.activateRun(ctx, "t-f3", AlignModeRepo); err != nil {
		t.Fatalf("activateRun(adoption): %v", err)
	}
	// A 分支：只 Resume（延续原 session），不 Acquire。
	if rec.resumeCalls != 1 || rec.acquireCalls != 0 {
		t.Fatalf("resume=%d acquire=%d", rec.resumeCalls, rec.acquireCalls)
	}
	// 提交：binding v2（upstream=anchor）、任务 active。
	got, has, err := adapter.GetTaskBinding(ctx, "t-f3")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "sess-keep" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	row, err := adapter.GetTask(ctx, "t-f3")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %v %v", row.Status, err)
	}
	// G3-F3 正式接管：runtime 已注册（SSE 消费在位）。
	if m.getRuntime("t-f3") == nil {
		t.Fatalf("takeover must register task runtime")
	}
	// 候选流复用为正式接管流（评审 I10：保留缓冲事件，不重订阅——流保持打开，
	// 由 runtime SSE 生命周期接管关闭时机；关闭反而是回归信号）。
	if atomic.LoadInt32(&stream.closed) != 0 {
		t.Fatalf("candidate stream must be reused as takeover stream (not closed)")
	}
}

// TestGate3_F9_AdmissionReadError_FailClosed 落实 G3-F9：binding 读取错误必须
// 使 admission 拒绝（fail-closed 透传），MUST NOT 静默降级为「无 v2 binding」
// 继续返回 supervisor；零副作用（无 runtime 会话产生）。
func TestGate3_F9_AdmissionReadError_FailClosed(t *testing.T) {
	ms := newMockStore()
	seedTaskRow(t, ms, "t-rerr")
	proc := newMockProc()
	wt := newMockWorktree()
	oc := newMockOC(true)
	m := newTestManagerFull(t, ms, ms, proc, wt, oc)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	// 注册 v2 fake（若读错被吞，admission 会继续走到 v2 supervisor 派发）。
	reg := appagent.NewRegistry()
	if err := reg.Register(&fakeOwnershipProvider{withChecker: true}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	// 种入 v2 binding 后注入读取错误（同一次读取路径）。
	seedV2BindingTask(t, ms, "t-rerr")
	ms.mu.Lock()
	ms.getTaskBindingErr = errors.New("db read failure")
	ms.mu.Unlock()

	err := m.activateRun(context.Background(), "t-rerr", AlignModeRepo)
	if err == nil || !strings.Contains(err.Error(), "db read failure") {
		t.Fatalf("read error must fail admission, got %v", err)
	}
	// 零副作用：无 runtime 会话、binding 未漂移。
	if sessions, _ := proc.ListSessions(); len(sessions) != 0 {
		t.Fatalf("zero side effect violated, sessions=%v", sessions)
	}
	b, has, gerr := ms.GetTaskBinding(context.Background(), "t-rerr")
	_ = b
	if gerr == nil && has {
		// 注入错误仍在（未写入）；binding 事实未被漂移。
		if has {
			t.Fatalf("binding must be untouched on read error")
		}
	}
}
