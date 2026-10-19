// v2_stage4_test.go 落实阶段 4 验收（opencode-v2-provider tasks 7.1 / design D9
// 验收矩阵）在 task 层的两项承接连续性补测（真实 store 路径）：
//   - server 重启后恢复（承接四路径之三）：active v2 任务经恢复入口 runRecoveryAttempt
//     v2 分支——Resume 延续原 session + CommitV2Resume activating→active 收敛，
//     upstream 不变（连续性）、不卡 activating（D4 生命周期矩阵）
//   - 承接中途失败重试（承接四路径之四）：A 分支提交冲突 → 整体回滚零副作用 →
//     冲突解除后重试成功——全部 attempt 保持同一锚定原 session；pending 丢失如实
//     告知（last_error，非「已解决」）
package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/store"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// seedV2ActiveTask 种子 active v2 任务（真实 store 恢复路径）：v2 binding
// （upstream=ses_keep、origin=created）+ 合法 env 快照。
func seedV2ActiveTask(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES (?, 'proj', '/repo', 'main', ?, 'repo')`, "p-"+taskID, now); err != nil {
		t.Fatal(err)
	}
	raw, err := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  "ses_keep",
		TerminalRef:        "ocdeck-v2-tui-" + taskID,
		Data:               []byte(`{"port":4096,"directory":"/wt","taskId":"` + taskID + `","origin":"created","auth":{"kind":"derived"}}`),
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	snap, serr := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": taskID}})
	if serr != nil {
		t.Fatal(serr)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES (?, ?, 'task', 'b', 'active', '/wt', ?, ?, ?, ?, ?)`,
		taskID, "p-"+taskID, ocdeckstore.ProviderIDV2, raw, snap, now, now); err != nil {
		t.Fatal(err)
	}
}

// TestStage4_V2Recovery_ServerRestart_ResumeKeepsSameSession 落实承接四路径之
// 「server 重启后恢复」：active v2 任务（server 层重拉语义由 supervisor 侧
// F10/5.3 覆盖）经恢复入口——只 Resume 不 Acquire（不建替代 session）、upstream
// 保持同一原 session、CommitV2Resume 将 activating 收敛 active（D4：MUST NOT 卡
// activating）。
func TestStage4_V2Recovery_ServerRestart_ResumeKeepsSameSession(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-v2rec")

	adapter := NewStoreAdapter(db)
	proc := newMockProc()
	m := newTestManagerFull(t, adapter, nil, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	rec := &recordingV2Sup{upstream: "ses_keep", port: 4096}
	reg := appagent.NewRegistry()
	if err := reg.Register(&recordingV2Provider{sup: rec}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	rt := m.newRuntime("t-v2rec")
	m.setRuntime("t-v2rec", rt)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ensureRecovery("t-v2rec", rt.instVersion)
	}()

	// ensureRecovery 完成后（done happens-before）再读 recorder 计数。
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		row, _ := adapter.GetTask(ctx, "t-v2rec")
		t.Fatalf("recovery did not finish: status=%s lastError=%v", row.Status, row.LastError)
	}
	if rec.resumeCalls != 1 || rec.acquireCalls != 0 {
		t.Fatalf("resume=%d acquire=%d（恢复只 Resume、不建替代）", rec.resumeCalls, rec.acquireCalls)
	}
	// 连续性：binding 仍锚定同一原 session（v2 命名空间、origin 保留）。
	got, has, err := adapter.GetTaskBinding(ctx, "t-v2rec")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep" || got.ProviderID != ocdeckstore.ProviderIDV2 {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	var data struct {
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(got.Data, &data); err != nil || data.Origin != "created" {
		t.Fatalf("origin: %q err=%v", data.Origin, err)
	}
	// D4 生命周期矩阵：恢复提交后必须收敛 active（MUST NOT 卡 activating）。
	row, err := adapter.GetTask(ctx, "t-v2rec")
	if err != nil || row.Status != "active" {
		t.Fatalf("status=%s err=%v（activating 必须收敛 active）", row.Status, err)
	}
	// 正式接管：runtime 已注册（provider-aware SSE 接线在位）。
	if m.getRuntime("t-v2rec") == nil {
		t.Fatalf("recovery takeover must register task runtime")
	}
}

// TestStage4_AdoptionCommitConflict_RetryKeepsSameSession 落实承接四路径之
// 「承接中途失败重试」（D5 步骤 5 失败出口）：新键被他任务持有 → CommitAdoption
// 整体拒绝——零持久化副作用（状态/binding/anchor/归属行全原样、候选流退订）；
// 冲突解除后重试成功——两次 attempt 均延续同一锚定原 session（连续性），归属行
// session_id 不变，pending 丢失如实告知（last_error 指引重新触发）。
func TestStage4_AdoptionCommitConflict_RetryKeepsSameSession(t *testing.T) {
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
		 VALUES ('t-ad','p1','task','b','suspended','/wt','sess-keep','opencode-v1','default','opencode-v1',?, ?, ?)`,
		`{"version":1,"provider_id":"opencode-v1","provider_instance_id":"default","upstream_session_id":"sess-keep","terminal_ref":"tui","data":{"port":4000,"directory":"/wt","taskId":"t-ad","auth":{"kind":"derived"}}}`,
		now, now); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"sess-keep", "sess-child"} {
		if _, err := db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
			 VALUES ('t-ad', ?, 1, 1, 1, ?)`, sid, map[bool]string{true: "", false: "sess-keep"}[sid == "sess-keep"]); err != nil {
			t.Fatal(err)
		}
	}
	// 冲突种子：他任务以 v2 命名空间持有 sess-keep（D5 新键三列谓词冲突）。
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, created_at, updated_at)
		 VALUES ('t-other','p1','other','b','suspended','/wt2','opencode-v1',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
		 VALUES ('t-other','sess-keep',1,1,1,?,?)`, ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
		t.Fatal(err)
	}

	adapter := NewStoreAdapter(db)
	proc := newMockProc()
	m := newTestManagerFull(t, adapter, nil, proc, newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	rec := &recordingV2Sup{upstream: "sess-keep", port: 4096}
	stream := newFakeAdoptStream()
	reg := appagent.NewRegistry()
	if err := reg.Register(&adoptingV2Provider{sup: rec, stream: stream}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// Attempt 1：提交冲突 → 整体拒绝，零持久化副作用。
	aerr := m.activateRun(ctx, "t-ad", AlignModeRepo)
	if aerr == nil || !strings.Contains(aerr.Error(), "被他任务持有") {
		t.Fatalf("expect adoption conflict, got %v", aerr)
	}
	row, err := adapter.GetTask(ctx, "t-ad")
	if err != nil || row.Status != "suspended" {
		t.Fatalf("conflict must keep task suspended: %s %v", row.Status, err)
	}
	got, has, err := adapter.GetTaskBinding(ctx, "t-ad")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV1 {
		t.Fatalf("conflict must keep v1 binding: %+v has=%v err=%v", got, has, err)
	}
	var nV1, nV2 int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-ad' AND provider_id=?`, ocdeckstore.ProviderIDV1).Scan(&nV1)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-ad' AND provider_id=?`, ocdeckstore.ProviderIDV2).Scan(&nV2)
	if nV1 != 2 || nV2 != 0 {
		t.Fatalf("ownership rows must stay v1: v1=%d v2=%d", nV1, nV2)
	}
	if atomic.LoadInt32(&stream.closed) == 0 {
		t.Fatalf("candidate stream must be closed on conflict（退订回收）")
	}

	// Attempt 2：冲突解除 → 重试成功，同一原 session 连续性。
	if _, err := db.Exec(`DELETE FROM task_sessions WHERE task_id='t-other'`); err != nil {
		t.Fatal(err)
	}
	if err := m.activateRun(ctx, "t-ad", AlignModeRepo); err != nil {
		t.Fatalf("retry activateRun: %v", err)
	}
	if rec.resumeCalls != 2 || rec.acquireCalls != 0 {
		t.Fatalf("resume=%d acquire=%d（重试仍只 Resume 同一 anchor，不建替代）", rec.resumeCalls, rec.acquireCalls)
	}
	got, has, err = adapter.GetTaskBinding(ctx, "t-ad")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "sess-keep" {
		t.Fatalf("binding after retry: %+v has=%v err=%v", got, has, err)
	}
	row, err = adapter.GetTask(ctx, "t-ad")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	// 归属转换（全部行 v1→v2、session_id 不变、含 parent 链）由 store 层
	// TestCommitAdoption_Success 覆盖；此处不断言归属行——fake 环境接管后的
	// SSE 对齐（mock 上游为空）会回收 absent 归属行，属 fake 语义而非承接事务。
	// pending 丢失如实告知：last_error 指明重新触发（非「已解决」）。
	if !strings.Contains(row.LastError.String, "pending") || !strings.Contains(row.LastError.String, "重新触发") {
		t.Fatalf("pending loss must be surfaced in last_error: %q", row.LastError.String)
	}
}

// ---- B 首建提交组合（D4 B 行 / D9 首建补偿三场景②③ / tasks 6.5）----

// branchBV2Sup 是 B 首建路径的 v2 supervisor fake：Acquire 返回本次 candidate
//（POST 语义）；Resume 恒拒（B 路径 MUST NOT Resume——不建替代）；实现
// CandidateSessionRecycler 记录回收事实（真实 DELETE 由 V2Supervisor 承载，
// supervisor 侧见 opencode2 RecycleCandidateSession 测试）。
type branchBV2Sup struct {
	mu           sync.Mutex
	acquireCalls int32
	recycled     []string
	upstream     string
	port         int
}

func (s *branchBV2Sup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	atomic.AddInt32(&s.acquireCalls, 1)
	data, derr := json.Marshal(map[string]any{
		"port": s.port, "directory": "/wt", "taskId": t.TaskID,
		"origin": "created", "auth": map[string]string{"kind": "derived"},
	})
	if derr != nil {
		return domain.TaskBinding{}, derr
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  s.upstream,
		TerminalRef:        "ocdeck-v2-tui-" + t.TaskID,
		Data:               data,
	}, nil
}

func (s *branchBV2Sup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.resume", appagent.ErrKindCapabilityUnavailable,
		errors.New("Resume MUST NOT be called on B first-build"))
}

func (s *branchBV2Sup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected AcquireEphemeral")
}

func (s *branchBV2Sup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

func (s *branchBV2Sup) RecycleCandidateSession(ctx context.Context, b domain.TaskBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recycled = append(s.recycled, b.UpstreamSessionID)
	return nil
}

func (s *branchBV2Sup) recycledSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.recycled...)
}

// failEventsSrc 是订阅即失败的 EventSource（D9 场景② SSE 失败注入）。
type failEventsSrc struct{}

func (failEventsSrc) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	return nil, errors.New("injected sse subscribe failure")
}

// branchBV2Provider 包装 branchBV2Sup：归属校验通过、端点同端口解析；
// sseErr=true 时 Events 返回订阅即失败源（场景②）。
type branchBV2Provider struct {
	v2Stub
	sup    *branchBV2Sup
	sseErr bool
}

func (p *branchBV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *branchBV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *branchBV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return p.sup.port, "derived", "/wt", nil
}

func (p *branchBV2Provider) Events() appagent.EventSource {
	if p.sseErr {
		return failEventsSrc{}
	}
	return nil
}

// seedBranchBTask 种子 B 首建形态任务（D5 判定矩阵「三者一致为空」）：suspended、
// v1 钉扎、无 binding/anchor/归属行（init_status 默认 none——Activate 门禁放行）。
func seedBranchBTask(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p1','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, created_at, updated_at)
		 VALUES (?,'p1','task','b','suspended','/wt','opencode-v1',?,?)`, taskID, now, now); err != nil {
		t.Fatal(err)
	}
}

// newBranchBManager 构造 B 首建测试 Manager（真实 store + machine v2 开关开 +
// 注入 v2 fake registry）。
func newBranchBManager(t *testing.T, db *store.DB, sup *branchBV2Sup, sseErr bool) *Manager {
	t.Helper()
	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	// 钉扎 v1 准入（registryAdmitTask 按 tasks.provider_id 查表）+ v2 fake。
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&branchBV2Provider{sup: sup, sseErr: sseErr}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	return m
}

// TestStage4_AdoptBranchB_E2E_CommitAndTakeover 落实 B 首建端到端（R8→branchB→
// 提交→接管）：Activate（suspended→activating 既有入口）→ v2 Acquire 建 session →
// 接管前段 SSE → commitActivationBinding（activating→active CAS + binding）→
// 提交后段就绪。断言：只 Acquire 不 Resume、binding v2 提交（upstream=本次
// candidate）、任务 active、钉扎 provider_id 不变（D5 #1）、runtime 注册、零回收。
func TestStage4_AdoptBranchB_E2E_CommitAndTakeover(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-b1")
	sup := &branchBV2Sup{upstream: "ses_new_1", port: 4096}
	m := newBranchBManager(t, db, sup, false)

	if err := m.Activate(ctx, "t-b1"); err != nil {
		t.Fatalf("Activate(B first-build): %v", err)
	}
	if atomic.LoadInt32(&sup.acquireCalls) != 1 {
		t.Fatalf("acquire=%d, want 1", atomic.LoadInt32(&sup.acquireCalls))
	}
	adapter := NewStoreAdapter(db)
	got, has, err := adapter.GetTaskBinding(ctx, "t-b1")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "ses_new_1" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	row, err := adapter.GetTask(ctx, "t-b1")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	// 钉扎值不变（D5 #1）：tasks.provider_id 仍 opencode-v1。
	var pinned string
	if err := db.QueryRow(`SELECT provider_id FROM tasks WHERE id='t-b1'`).Scan(&pinned); err != nil || pinned != ocdeckstore.ProviderIDV1 {
		t.Fatalf("pinned provider: %q err=%v（钉扎 MUST NOT 漂移）", pinned, err)
	}
	if m.getRuntime("t-b1") == nil {
		t.Fatalf("B first-build must register task runtime（正式接管）")
	}
	if len(sup.recycledSnapshot()) != 0 {
		t.Fatalf("success path must not recycle candidate: %v", sup.recycledSnapshot())
	}
}

// TestStage4_AdoptBranchB_SSEFail_RecyclesCandidate 落实 D9 首建补偿场景②：
// POST 成功后 SSE 失败（提交前段）→ candidate 未提交：本地接管回退（clearRuntime）
// + candidate 回收（D3 受限回收例外）+ 既有激活补偿收敛 suspended + last_error，
// binding 零持久化。
func TestStage4_AdoptBranchB_SSEFail_RecyclesCandidate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-b2")
	sup := &branchBV2Sup{upstream: "ses_new_2", port: 4096}
	m := newBranchBManager(t, db, sup, true)

	aerr := m.Activate(ctx, "t-b2")
	if aerr == nil || !strings.Contains(aerr.Error(), "sse subscribe") {
		t.Fatalf("expect sse subscribe failure, got %v", aerr)
	}
	if got := sup.recycledSnapshot(); len(got) != 1 || got[0] != "ses_new_2" {
		t.Fatalf("candidate must be recycled on SSE failure: %v", got)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-b2")
	if err != nil || row.Status != "suspended" {
		t.Fatalf("status: %s %v（补偿收敛 suspended）", row.Status, err)
	}
	if _, has, gerr := adapter.GetTaskBinding(ctx, "t-b2"); gerr != nil || has {
		t.Fatalf("binding must not persist on failed attempt: has=%v err=%v", has, gerr)
	}
	if m.getRuntime("t-b2") != nil {
		t.Fatalf("local takeover must be reverted（clearRuntime）")
	}
}

// TestStage4_AdoptBranchB_CommitConflict_RecyclesCandidate 落实 D9 首建补偿
// 场景③：提交失败（状态守卫失配——未经理接入口 CAS，suspended 行遇
// activating→active CAS 必失配的确定性注入）→ candidate 未提交：本地接管回退 +
// candidate 回收 + 冲突错误；任务保持 suspended、binding 零持久化。
func TestStage4_AdoptBranchB_CommitConflict_RecyclesCandidate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-b3")
	sup := &branchBV2Sup{upstream: "ses_new_3", port: 4096}
	m := newBranchBManager(t, db, sup, false)

	// 直调 activateRun（绕过 Activate 的 suspended→activating CAS）→ 提交 CAS
	// activating→active 对 suspended 行必失配（确定性提交失败注入）。
	if err := m.activateRun(ctx, "t-b3", AlignModeRepo); err == nil {
		t.Fatalf("commit must fail on status guard mismatch")
	}
	if got := sup.recycledSnapshot(); len(got) != 1 || got[0] != "ses_new_3" {
		t.Fatalf("candidate must be recycled on commit failure: %v", got)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-b3")
	if err != nil || row.Status != "suspended" {
		t.Fatalf("status: %s %v（提交失败不得改状态）", row.Status, err)
	}
	if _, has, gerr := adapter.GetTaskBinding(ctx, "t-b3"); gerr != nil || has {
		t.Fatalf("binding must not persist: has=%v err=%v", has, gerr)
	}
	if m.getRuntime("t-b3") != nil {
		t.Fatalf("local takeover must be reverted")
	}
}

// ---- OnServerRestarted 重建编排（tasks 5.3）----

// atomicResumeSup 是 RebuildV2Associations 测试的线程安全 v2 supervisor fake
//（恢复入口异步触发——计数原子读）。
type atomicResumeSup struct {
	resumeCalls  int32
	acquireCalls int32
	upstream     string
	port         int
}

func (s *atomicResumeSup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	atomic.AddInt32(&s.acquireCalls, 1)
	return domain.TaskBinding{}, appagent.NewError("test.acquire", appagent.ErrKindCapabilityUnavailable,
		errors.New("rebuild MUST NOT create sessions"))
}

func (s *atomicResumeSup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	atomic.AddInt32(&s.resumeCalls, 1)
	data, _ := json.Marshal(map[string]any{
		"port": s.port, "directory": "/wt", "taskId": t.TaskID,
		"origin": "created", "auth": map[string]string{"kind": "derived"},
	})
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  s.upstream,
		TerminalRef:        "ocdeck-v2-tui-" + t.TaskID,
		Data:               data,
	}, nil
}

func (s *atomicResumeSup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (s *atomicResumeSup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

// atomicV2Provider 包装 atomicResumeSup（归属校验通过 + 端点解析）。
type atomicV2Provider struct {
	v2Stub
	sup *atomicResumeSup
}

func (p *atomicV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *atomicV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *atomicV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return p.sup.port, "derived", "/wt", nil
}

// TestStage4_OnServerRestarted_RebuildResumesActiveV2Only 落实 OnServerRestarted
// 接线语义（tasks 5.3）：重拉后仅 active v2 任务经既有恢复入口重建关联——只
// Resume（延续原 session，MUST NOT 重复建 session：Acquire 恒拒即红）；suspended
// v2 任务与 v1 任务不触碰。
func TestStage4_OnServerRestarted_RebuildResumesActiveV2Only(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-v2a") // active v2：应重建
	seedV2ActiveTask(t, db, "t-v2s") // 种子后置 suspended：不应触碰
	if _, err := db.Exec(`UPDATE tasks SET status='suspended' WHERE id='t-v2s'`); err != nil {
		t.Fatal(err)
	}
	seedBranchBTask(t, db, "t-v1")
	if _, err := db.Exec(`UPDATE tasks SET status='active' WHERE id='t-v1'`); err != nil {
		t.Fatal(err)
	}

	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	sup := &atomicResumeSup{upstream: "ses_keep", port: 4096}
	reg := appagent.NewRegistry()
	if err := reg.Register(&atomicV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	rt := m.newRuntime("t-v2a")
	m.setRuntime("t-v2a", rt)

	m.RebuildV2Associations()

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&sup.resumeCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // 恢复 incident 收尾
	if got := atomic.LoadInt32(&sup.resumeCalls); got != 1 {
		t.Fatalf("resume=%d, want exactly 1（仅 active v2 任务）", got)
	}
	if got := atomic.LoadInt32(&sup.acquireCalls); got != 0 {
		t.Fatalf("acquire=%d, want 0（重建 MUST NOT 重复建 session）", got)
	}
	// 连续性：upstream 不变、收敛 active。
	got, has, err := adapter.GetTaskBinding(ctx, "t-v2a")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	row, err := adapter.GetTask(ctx, "t-v2a")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
}
