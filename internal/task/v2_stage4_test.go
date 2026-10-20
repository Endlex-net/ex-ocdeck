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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/application"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/opencode2"
	"ocdeck/internal/infrastructure/process"
	"ocdeck/internal/infrastructure/store"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// seedV2ActiveTask 种子 active v2 任务（真实 store 恢复路径）：v2 binding
// （upstream=ses_keep、origin=created）+ 合法 env 快照。
func seedV2ActiveTask(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	seedV2ActiveTaskUpstream(t, db, taskID, "ses_keep")
}

// seedV2ActiveTaskUpstream 同 seedV2ActiveTask，upstream 可变（隔离/挂起测试：
// 同 provider 下多任务各自独立 session 身份）。
func seedV2ActiveTaskUpstream(t *testing.T, db *store.DB, taskID, upstream string) {
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
		UpstreamSessionID:  upstream,
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
		 base_ref, provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES (?, ?, 'task', 'b', 'active', '/wt', 'refs/heads/main', ?, ?, ?, ?, ?)`,
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
		m.ensureRecovery("t-v2rec", rt.instVersion, false)
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
	oc := newMockOC(true)
	m := newTestManagerFull(t, adapter, nil, proc, newMockWorktree(), oc)
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	// mock 上游列出原 session 集（提交后对齐保留归属行——恢复归属转换断言）。
	oc.mu.Lock()
	oc.sessions = []opencode.Session{
		{ID: "sess-keep", Time: opencode.SessionTime{Created: 1, Updated: 1}},
		{ID: "sess-child", Time: opencode.SessionTime{Created: 1, Updated: 1}},
	}
	oc.mu.Unlock()
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
	// 归属转换（评审 I5 断言恢复）：全部行 v1→v2、session_id 不变、含 parent 链。
	var nV2After, nV1After int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-ad' AND provider_id=?`, ocdeckstore.ProviderIDV2).Scan(&nV2After)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-ad' AND provider_id=?`, ocdeckstore.ProviderIDV1).Scan(&nV1After)
	if nV2After != 2 || nV1After != 0 {
		t.Fatalf("ownership conversion: v2=%d v1=%d（session_id 不变的命名空间转换）", nV2After, nV1After)
	}
	// pending 丢失如实告知：last_error 指明重新触发（非「已解决」）。
	if !strings.Contains(row.LastError.String, "pending") || !strings.Contains(row.LastError.String, "重新触发") {
		t.Fatalf("pending loss must be surfaced in last_error: %q", row.LastError.String)
	}
}

// ---- B 首建提交组合（D4 B 行 / D9 首建补偿三场景②③ / tasks 6.5）----

// branchBV2Sup 是 B 首建路径的 v2 supervisor fake：Acquire 返回本次 candidate
// （POST 语义）；Resume 恒拒（B 路径 MUST NOT Resume——不建替代）；实现
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

// seedAdoptAFormTask 种子承接 A 形态任务（真实 store）：v1 binding(upstream)@v1 +
// anchor@v1 + 归属行（父+子）+ env 快照事实（承接失败保留断言用）。pinned 可指定
// （承接 A 为 v1；R5 用 v2）；upstream 为原 session id（真实流 harness 的 GET
// 路由按 ^ses_ 前缀匹配，传 ses_keep 形态）。
func seedAdoptAFormTask(t *testing.T, db *store.DB, taskID, pinned, upstream string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p1','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	v1Raw, err := domain.TaskBinding{
		Version: domain.TaskBindingVersion, ProviderID: ocdeckstore.ProviderIDV1,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV1Default,
		UpstreamSessionID:  upstream, TerminalRef: "tui",
		Data: []byte(`{"port":4000,"directory":"/wt","taskId":"` + taskID + `","auth":{"kind":"derived"}}`),
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, mode, worktree_path,
		 anchor_session_id, anchor_provider_id, anchor_provider_instance_id, provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES (?,'p1','task','b','suspended','worktree','/wt',?,'opencode-v1','default',?,?,?, ?, ?)`,
		taskID, upstream, pinned, v1Raw, `{"OCDECK_TASK_ID":"`+taskID+`"}`, now, now); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{upstream, "sess-child"} {
		if _, err := db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
			 VALUES (?, ?, 1, 1, 1, ?)`, taskID, sid, map[bool]string{true: "", false: upstream}[sid == upstream]); err != nil {
			t.Fatal(err)
		}
	}
}

// failResumeV2Sup 是 Resume 恒败（NotFound——原 session 不可读）的 v2 supervisor
// fake（承接 A 失败出口注入）。
type failResumeV2Sup struct {
	resumeCalls int32
}

func (s *failResumeV2Sup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected Acquire")
}

func (s *failResumeV2Sup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	atomic.AddInt32(&s.resumeCalls, 1)
	return domain.TaskBinding{}, appagent.NewError("supervisor.resume", appagent.ErrKindNotFound,
		errors.New("original session unreadable"))
}

func (s *failResumeV2Sup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (s *failResumeV2Sup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

// handshakeFailV2Provider 包装 recordingV2Sup：Resume 正常、Events 订阅即失败
// （评审 I10：提交前候选握手失败注入——提交 MUST NOT 发生）。
type handshakeFailV2Provider struct {
	v2Stub
	sup *recordingV2Sup
}

func (p *handshakeFailV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *handshakeFailV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *handshakeFailV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return p.sup.port, "derived", "/wt", nil
}

func (p *handshakeFailV2Provider) Events() appagent.EventSource { return failEventsSrc{} }

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
// 候选握手（仅缓冲）→ CommitFirstBuild（claim+anchor+binding+activating→active
// CAS 单事务，评审 I5）→ 提交后正式接管（对齐/事件消费启用）。断言：只 Acquire
// 不 Resume、binding v2 提交（upstream=本次 candidate）、任务 active、钉扎
// provider_id 不变（D5 #1）、claim/anchor 落 v2 命名空间、runtime 注册、零回收。
func TestStage4_AdoptBranchB_E2E_CommitAndTakeover(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-b1")
	sup := &branchBV2Sup{upstream: "ses_new_1", port: 4096}
	adapter := NewStoreAdapter(db)
	proc := newMockProc()
	oc := newMockOC(true)
	m := newTestManagerFull(t, adapter, nil, proc, newMockWorktree(), oc)
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	// 钉扎 v1 准入（registryAdmitTask 按 tasks.provider_id 查表）+ v2 fake。
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&branchBV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	// mock 上游列出本次 candidate（提交后对齐保留归属行——断言 claim/anchor）。
	oc.mu.Lock()
	oc.sessions = []opencode.Session{{ID: "ses_new_1", Time: opencode.SessionTime{Created: 100, Updated: 100}}}
	oc.mu.Unlock()

	if err := m.Activate(ctx, "t-b1"); err != nil {
		t.Fatalf("Activate(B first-build): %v", err)
	}
	if atomic.LoadInt32(&sup.acquireCalls) != 1 {
		t.Fatalf("acquire=%d, want 1", atomic.LoadInt32(&sup.acquireCalls))
	}
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
	// 评审 I5：claim+anchor 单事务落库——归属行 v2 命名空间、anchor 三元组 v2。
	var nV2, nV1 int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-b1' AND provider_id=?`, ocdeckstore.ProviderIDV2).Scan(&nV2)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-b1' AND provider_id=?`, ocdeckstore.ProviderIDV1).Scan(&nV1)
	if nV2 != 1 || nV1 != 0 {
		t.Fatalf("claim rows: v2=%d v1=%d（首建归属 MUST 落 v2 命名空间）", nV2, nV1)
	}
	var anchorSID, anchorPID string
	if err := db.QueryRow(`SELECT anchor_session_id, anchor_provider_id FROM tasks WHERE id='t-b1'`).Scan(&anchorSID, &anchorPID); err != nil || anchorSID != "ses_new_1" || anchorPID != ocdeckstore.ProviderIDV2 {
		t.Fatalf("anchor: %s/%s err=%v", anchorSID, anchorPID, err)
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
// （恢复入口异步触发——计数原子读）。
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

// CheckServerAlive 满足 ServerAvailabilityWarmer（I8 v2 恢复预算序①——fake 恒可用）。
func (s *atomicResumeSup) CheckServerAlive(ctx context.Context) error { return nil }

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

// ---- I9 全链确定性：server 死亡 → supervisor 监控自动重拉 → OnServerRestarted
//（生产接线 = Manager.RebuildV2Associations）→ 活跃任务按 binding 重建关联 ----

// monitorProcSeam 是全链测试的 SupervisorProcSeam fake：serve 托管单元真实监听
// 端口（生产形态——端口归 serve 进程所有，startServer 的 portOccupied fail-closed
// 才能如实工作）。
type monitorProcSeam struct {
	mu      sync.Mutex
	unitUp  bool
	pid     int
	port    int
	ln      net.Listener
	handler http.Handler
	spawns  int
	// onSpawn 在 serve 单元（重新）绑定时回调——模拟「新进程存活」（身份核验
	// 依据由测试侧持有，测试经此复活 ProcessIdentity 事实）。
	onSpawn func()
	// onDeath 在 serve 单元被测试主动杀死时回调——对称 onSpawn，模拟「身份探测
	// 同步感知进程死亡」（Round2 J6：死亡接缝必须改变身份探测事实，否则监控
	// 永远判活、关停断言假绿）。
	onDeath func()
}

// spawnCount 返回 serve 单元启动次数（Round收尾 J6：共享 serve 断言）。
func (p *monitorProcSeam) spawnCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

// newMonitorProcSeam 构造并预占一个空闲端口（构造即关闭——端口在 NewSession 时
// 由 serve 单元绑定，startServer 的占用检查在此前必须看到端口空闲）。
func newMonitorProcSeam(handler http.Handler) *monitorProcSeam {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return &monitorProcSeam{pid: 4242, port: port, handler: handler}
}

func (p *monitorProcSeam) NewSession(spec process.SessionSpec) error {
	port := 0
	for i, tok := range spec.CmdArgv {
		if tok == "--port" && i+1 < len(spec.CmdArgv) {
			port, _ = strconv.Atoi(spec.CmdArgv[i+1])
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	p.mu.Lock()
	p.spawns++
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.unitUp = true
	onSpawn := p.onSpawn
	srv := &http.Server{Handler: p.handler}
	p.mu.Unlock()
	if onSpawn != nil {
		onSpawn()
	}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (p *monitorProcSeam) HasSession(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unitUp, nil
}

func (p *monitorProcSeam) KillSession(name string) (process.KillResult, error) {
	p.killUnit()
	return process.KillResult{}, nil
}

func (p *monitorProcSeam) SessionPID(name string) (int, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.unitUp {
		return 0, false, nil
	}
	return p.pid, true, nil
}

func (p *monitorProcSeam) killUnit() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.unitUp = false
	onDeath := p.onDeath
	p.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if onDeath != nil {
		onDeath()
	}
}

// monitorRecordSeam 是全链测试的 RuntimeRecordSeam fake（内存实例记录）。
type monitorRecordSeam struct {
	mu  sync.Mutex
	rec store.ProviderRuntimeRecord
	has bool
}

func (s *monitorRecordSeam) GetProviderRuntime(ctx context.Context, providerID, instanceID string) (store.ProviderRuntimeRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.has, nil
}

func (s *monitorRecordSeam) PutProviderRuntimeStart(ctx context.Context, providerID, instanceID string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec = store.ProviderRuntimeRecord{ProviderID: providerID, InstanceID: instanceID, Port: port}
	s.has = true
	return nil
}

func (s *monitorRecordSeam) SetProviderRuntimePID(ctx context.Context, providerID, instanceID string, pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.PID = sql.NullInt64{Int64: int64(pid), Valid: true}
	return nil
}

func (s *monitorRecordSeam) SetProviderRuntimeError(ctx context.Context, providerID, instanceID string, cause string) error {
	return nil
}

// supRealV2Provider 用真实 opencode2.V2Supervisor 参与 registry 分派：归属校验
// 放行、端点从 binding data 解码、Events nil（握手回退 ocFactory——全链测试聚焦
// supervisor/重建链，SSE 由 mock 承载）。
type supRealV2Provider struct {
	v2Stub
	sup *opencode2.V2Supervisor
}

func (p *supRealV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *supRealV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *supRealV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	var ed struct {
		Port      int    `json:"port"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(b.Data, &ed); err != nil {
		return 0, "", "", err
	}
	return ed.Port, "derived", ed.Directory, nil
}

func (p *supRealV2Provider) Events() appagent.EventSource { return nil }

// TestStage4_ServerMonitor_FullChain_DeathToRebuild 落实 I9 生产触发闭环的确定性
// 全链（不直调 RebuildV2Associations）：真实 V2Supervisor 监控循环发现 server
// 死亡 → 独立重拉（同端口）→ 「确认重拉成功」异步通知（生产接线 OnServerRestarted
// = Manager.RebuildV2Associations）→ 活跃 v2 任务经既有恢复入口重建关联（Resume
// 延续原 session，零 POST——不重复建 session）；首次启动不通知。
func TestStage4_ServerMonitor_FullChain_DeathToRebuild(t *testing.T) {
	ctx := context.Background()
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// ① 上游 serve 端口（proc fake 承载真实监听；固定端口跨重启不变）。
	var getSessionHits, postHits atomic.Int32
	var identityAlive atomic.Bool
	procSeam := newMonitorProcSeam(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			postHits.Add(1) // 重建链 MUST NOT 建 session——任何 POST 即违规信号
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/session/ses_keep":
			getSessionHits.Add(1)
			_, _ = w.Write([]byte(`{"data":{"id":"ses_keep","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
		}
	}))
	procSeam.onSpawn = func() { identityAlive.Store(true) } // 重绑 = 新进程存活
	ocPort := procSeam.port

	// ② 种子 active v2 任务（binding data 端口 = 实际上游端口——Resume 端口一致性
	// fail-closed 校验要求）。
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p-fc','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	raw, err := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  "ses_keep",
		TerminalRef:        "ocdeck-v2-tui-t-fc",
		Data:               []byte(fmt.Sprintf(`{"port":%d,"directory":"/wt","taskId":"t-fc","origin":"created","auth":{"kind":"derived"}}`, ocPort)),
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	snap, serr := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t-fc"}})
	if serr != nil {
		t.Fatal(serr)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES ('t-fc','p-fc','task','b','active','/wt',?,?,?,?,?)`,
		ocdeckstore.ProviderIDV2, raw, snap, now, now); err != nil {
		t.Fatal(err)
	}

	// ③ Manager + 真实 V2Supervisor（OnServerRestarted = 生产接线 Rebuild）。
	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	proc := procSeam
	recs := &monitorRecordSeam{}
	identityAlive.Store(true)
	sup := opencode2.NewV2Supervisor(opencode2.SupervisorDeps{
		Proc:  proc,
		Store: recs,
		Env:   NewV2TaskEnvSeam(m), // 生产装配同源：created 恢复应用同代 env 快照
		AllocatePort: func(int, bool, int) (int, error) {
			return ocPort, nil
		},
		ProcessIdentity: func(pid int) (string, bool, error) {
			if identityAlive.Load() && pid == 4242 {
				return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", ocPort), true, nil
			}
			return "", false, nil
		},
		OnServerRestarted: m.RebuildV2Associations,
		MonitorInterval:   5 * time.Millisecond,
		PollInterval:      5 * time.Millisecond,
		ReadyTimeout:      500 * time.Millisecond,
		StartAttempts:     2,
	})
	t.Cleanup(func() { _ = sup.ShutdownServer(ctx) })
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&supRealV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	m.setRuntime("t-fc", m.newRuntime("t-fc"))

	// ④ 业务入口预热（首次启动：监控后置启动；首启不通知——无重建请求面）。
	if err := sup.CheckServerAlive(ctx); err != nil {
		t.Fatalf("warmup CheckServerAlive: %v", err)
	}
	time.Sleep(30 * time.Millisecond) // 数个监控周期
	hitsAfterWarm := getSessionHits.Load()
	if postHits.Load() != 0 {
		t.Fatalf("first start must not trigger rebuild, POSTs=%d", postHits.Load())
	}

	// ⑤ server 真实死亡：监听关闭 + 托管单元消失 + pid 身份报死（无任何显式
	// CheckServerAlive 调用——生产触发 owner 是监控循环）。
	proc.killUnit()
	identityAlive.Store(false)

	// ⑦ 轮询：监控自动发现 → 独立重拉（NewSession 重绑同端口）→ 通知 → Rebuild
	// → Resume 验证原 session（GET 命中）。
	deadline := time.Now().Add(5 * time.Second)
	for getSessionHits.Load() <= hitsAfterWarm && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if getSessionHits.Load() <= hitsAfterWarm {
		t.Fatalf("monitor did not auto-repull and rebuild (GET hits=%d, warm=%d)", getSessionHits.Load(), hitsAfterWarm)
	}
	time.Sleep(100 * time.Millisecond) // 重建链收尾（提交/接管）
	if postHits.Load() != 0 {
		t.Fatalf("rebuild MUST NOT create sessions, POSTs=%d", postHits.Load())
	}
	got, has, gerr := adapter.GetTaskBinding(ctx, "t-fc")
	if gerr != nil || !has || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding continuity: %+v has=%v err=%v", got, has, gerr)
	}
	frow, ferr := adapter.GetTask(ctx, "t-fc")
	if ferr != nil || frow.Status != "active" {
		t.Fatalf("status: %s %v lastError=%q", frow.Status, ferr, frow.LastError.String)
	}
}

// ---- I8 确定性：server 可用性失败 = 停止报告（保留现状 + 阶段结果），不进
// 预算循环、不终态补偿 ----

// stopReportV2Provider 包装 supervisor 并可注入 warmer 故障（CheckServerAlive
// 恒败 = supervisor 独立重试耗尽的确定性注入）。
type stopReportV2Provider struct {
	v2Stub
	sup appagent.RuntimeSupervisor
}

func (p *stopReportV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *stopReportV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *stopReportV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 4096, "derived", "/wt", nil
}

func (p *stopReportV2Provider) Events() appagent.EventSource { return nil }

// TestStage4_V2Recovery_ServerUnavailable_StopReport 落实 D4/D5：共享 server
// 可用性失败（supervisor 独立重试耗尽）→ 停止、保留现状（activating）、last_error
// 提交用户决策的明确阶段结果——MUST NOT 终态补偿（非 suspended）、任务 permit
// 零消耗（permit 恰在 server 可用后才扣）。
func TestStage4_V2Recovery_ServerUnavailable_StopReport(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-sr")

	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	sup := &recordingV2Sup{upstream: "ses_keep", port: 4096}
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&stopReportV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	m.setRuntime("t-sr", m.newRuntime("t-sr"))
	// warmer 故障注入（recordingV2Sup 恒可用——此处覆写为恒败）。
	warmFail := &warmFailSup{recordingV2Sup: sup}
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&stopReportV2Provider{sup: warmFail}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg2

	m.RebuildV2Associations() // 通知体：内部 go ensureRecovery（异步——下方轮询收口）
	// 轮询至 stop-report 落定（activating 保持 + 阶段结果 last_error）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := adapter.GetTask(ctx, "t-sr")
		if err == nil && row.LastError.Valid && strings.Contains(row.LastError.String, "保留现状") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	row, err := adapter.GetTask(ctx, "t-sr")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "activating" {
		t.Fatalf("stop-report must keep current status (activating), got %s (lastError=%q)", row.Status, row.LastError.String)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "保留现状") {
		t.Fatalf("stage result must be distinguishable in last_error: %q", row.LastError.String)
	}
	if sup.resumeCalls != 0 {
		t.Fatalf("session verification MUST NOT run after server availability failure, resumes=%d", sup.resumeCalls)
	}
}

// warmFailSup 覆写 CheckServerAlive 为恒败（其余行为委托 recordingV2Sup）。
type warmFailSup struct {
	*recordingV2Sup
}

func (s *warmFailSup) CheckServerAlive(ctx context.Context) error {
	return errors.New("server 启动重试耗尽（注入）")
}

// ---- 评审 Round 2 ----

// TestStage4_AdoptA_PartialHistory_ViaActivate_StopBeforeStateWrite 落实 I1：
// binding/anchor/归属「部分缺失」（判定不一致）→ 公开 Activate 在任何状态写入前
// 直接停止报告——任务保持 suspended、env 快照原文保留、last_error 含停止承接
// 语义（MUST NOT 经 activating/激活补偿清 env）。
func TestStage4_AdoptA_PartialHistory_ViaActivate_StopBeforeStateWrite(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-stop", ocdeckstore.ProviderIDV1, "sess-keep")
	// 部分缺失注入：binding 摘除（anchor+归属行仍在——判定矩阵「其余」→ stop）。
	if _, err := db.Exec(`UPDATE tasks SET task_binding=NULL WHERE id='t-stop'`); err != nil {
		t.Fatal(err)
	}
	m := newAdoptATestManager(t, db, &supOnlyV2Provider{sup: &recordingV2Sup{upstream: "sess-keep", port: 4096}})

	aerr := m.Activate(ctx, "t-stop")
	if aerr == nil || !strings.Contains(aerr.Error(), "停止承接") {
		t.Fatalf("expect stop-report error, got %v", aerr)
	}
	status, upstream, nV1, env := adoptAFactsSnapshot(t, db, "t-stop")
	if status != "suspended" {
		t.Fatalf("status=%s（任何状态写入前停止——MUST NOT 写 activating）", status)
	}
	if upstream != "sess-keep" || nV1 != 2 || !strings.Contains(env, "OCDECK_TASK_ID") {
		t.Fatalf("old facts must be fully preserved: upstream=%s nV1=%d env=%q", upstream, nV1, env)
	}
	row, err := adapterGetTask(t, db, "t-stop")
	if err != nil || !row.LastError.Valid || !strings.Contains(row.LastError.String, "判定不一致") {
		t.Fatalf("stop must be user-visible in last_error: %v %q", err, row.LastError.String)
	}
}

// adapterGetTask 局部取任务行（store adapter 直读）。
func adapterGetTask(t *testing.T, db *store.DB, taskID string) (TaskRow, error) {
	t.Helper()
	return NewStoreAdapter(db).GetTask(context.Background(), taskID)
}

// failCondWriteStore 包装真实 store：CAS 状态写入可注入 DB 失败并取消父 ctx
// （I2 持续写库失败的确定性注入——失败发生在 Release 确认后）。
type failCondWriteStore struct {
	*StoreAdapter
	failCond     atomic.Bool
	cancelParent context.CancelFunc
}

func (s *failCondWriteStore) UpdateTaskStatusConditional(ctx context.Context, taskID, from, to string, le sql.NullString) (application.TransitionResult, error) {
	if s.failCond.Load() {
		if s.cancelParent != nil {
			s.cancelParent() // 模拟「调用方 ctx 在停止确认后取消」
		}
		return application.TransitionResult{}, errors.New("db down（注入）")
	}
	return s.StoreAdapter.UpdateTaskStatusConditional(ctx, taskID, from, to, le)
}

// TestStage4_I8_RealSupervisor404_StopReport_NoBudgetLoop 落实 I8 生产错误分类：
// 真实 V2Supervisor + HTTP 404（原 session 缺失）经恢复编排命中停止报告——
// 恰一次尝试（零预算循环）、保留现状 activating、last_error 可区分离普通
// attempt 错误。
func TestStage4_I8_RealSupervisor404_StopReport_NoBudgetLoop(t *testing.T) {
	ctx := context.Background()
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var notFoundHits atomic.Int32
	procSeam := newMonitorProcSeam(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/session/ses_keep":
			notFoundHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"NotFound"}`))
		default:
			_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
		}
	}))
	ocPort := procSeam.port

	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p-404','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	raw, err := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  "ses_keep",
		TerminalRef:        "ocdeck-v2-tui-t-404",
		Data:               []byte(fmt.Sprintf(`{"port":%d,"directory":"/wt","taskId":"t-404","origin":"created","auth":{"kind":"derived"}}`, ocPort)),
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	snap, serr := encodeEnvSnapshot(envSnapshot{Vars: map[string]string{"OCDECK_TASK_ID": "t-404"}})
	if serr != nil {
		t.Fatal(serr)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES ('t-404','p-404','task','b','active','/wt',?,?,?,?,?)`,
		ocdeckstore.ProviderIDV2, raw, snap, now, now); err != nil {
		t.Fatal(err)
	}

	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	var identityAlive atomic.Bool
	identityAlive.Store(true)
	sup := opencode2.NewV2Supervisor(opencode2.SupervisorDeps{
		Proc:  procSeam,
		Store: &monitorRecordSeam{},
		Env:   NewV2TaskEnvSeam(m),
		AllocatePort: func(int, bool, int) (int, error) {
			return ocPort, nil
		},
		ProcessIdentity: func(pid int) (string, bool, error) {
			if identityAlive.Load() && pid == 4242 {
				return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", ocPort), true, nil
			}
			return "", false, nil
		},
		MonitorInterval: 50 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		ReadyTimeout:    500 * time.Millisecond,
		StartAttempts:   2,
	})
	t.Cleanup(func() { _ = sup.ShutdownServer(ctx) })
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&supRealV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	m.setRuntime("t-404", m.newRuntime("t-404"))

	if err := sup.CheckServerAlive(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	m.RebuildV2Associations()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, gerr := adapter.GetTask(ctx, "t-404")
		if gerr == nil && row.LastError.Valid && strings.Contains(row.LastError.String, "保留现状") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	row, gerr := adapter.GetTask(ctx, "t-404")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "保留现状") {
		t.Fatalf("404 must hit stop-report stage result, lastError=%q", row.LastError.String)
	}
	if row.Status != "activating" {
		t.Fatalf("status=%s（保留现状——MUST NOT 终态补偿/预算耗尽）", row.Status)
	}
	if n := notFoundHits.Load(); n != 1 {
		t.Fatalf("session GET hits=%d, want exactly 1（零预算循环——404 不重试）", n)
	}
}

// TestStage4_Suspend_V2_StopConfirmed_ConvergenceIntentOnDBFail 落实 I2：停止
// 已确认（Release 成功）后 CAS 因 DB 错误失败 → 「停止已确认」独立收敛出口：
// WithoutCancel 有界 context 完成意图落库（durable retryable notice）+ 任务保持
// active（守卫防覆盖其他代）+ 原错误如实上抛。
func TestStage4_Suspend_V2_StopConfirmed_ConvergenceIntentOnDBFail(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTaskUpstream(t, db, "t-i2", "ses_keep")

	wstore := &failCondWriteStore{StoreAdapter: NewStoreAdapter(db)}
	wstore.failCond.Store(true)
	sctx, scancel := context.WithCancel(ctx)
	wstore.cancelParent = scancel
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, wstore, prov)

	// DB 注入失败发生在 Release 确认后并取消调用方 ctx：停止确认后的收敛 MUST
	// 经 WithoutCancel 预算完成意图落库（不被取消拖垮）。
	serr := m.Suspend(sctx, "t-i2")
	if serr == nil || !strings.Contains(serr.Error(), "db down") {
		t.Fatalf("original error must surface, got %v", serr)
	}
	if atomic.LoadInt32(&sup.releaseN) != 1 {
		t.Fatalf("stop must have been confirmed once, releases=%d", atomic.LoadInt32(&sup.releaseN))
	}
	row, err := wstore.GetTask(ctx, "t-i2")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v（守卫——不覆盖其他代）", row.Status, err)
	}
	if !strings.Contains(row.Notice.String, "suspend_stop_confirmed") {
		t.Fatalf("replayable intent must be durable, notice=%q", row.Notice.String)
	}
}

// newSuspendManagerStore 同 newSuspendManager，但 store 可注入包装（I2 故障面）。
func newSuspendManagerStore(t *testing.T, db *store.DB, tstore TaskStore, prov *suspendV2Provider) *Manager {
	t.Helper()
	m := newTestManagerFull(t, tstore, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	return m
}

// TestStage4_I15_AdoptCommitSuspendInterleave_NoResurrectedAssociation 落实 I15：
// 屏障固定交错「承接提交完成 → Suspend 完成 → 继续接管」——接管守卫重验任务
// 状态（已收敛 suspended）→ 放弃接管：不注册 runtime、不启动 SSE（MUST NOT
// 重建「suspended + 运行关联」矛盾事实）。
func TestStage4_I15_AdoptCommitSuspendInterleave_NoResurrectedAssociation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-race", ocdeckstore.ProviderIDV1, "sess-keep")
	if _, err := db.Exec(`UPDATE tasks SET status='active' WHERE id='t-race'`); err != nil {
		t.Fatal(err)
	}

	// 提交屏障：CommitAdoption 落库后停下，等测试完成 Suspend 再放行。
	committed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	base := NewStoreAdapter(db)
	bstore := &commitBarrierStore{StoreAdapter: base, committed: committed,
		release: release, once: &once}
	sup := &recordingV2Sup{upstream: "sess-keep", port: 4096}
	m := newTestManagerFull(t, bstore, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&supOnlyV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	row, err := bstore.GetTask(ctx, "t-race")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.adoptTaskV2(ctx, "t-race", row, AlignModeRepo, false) }() // 恢复路径形态（未持锁）
	<-committed                                                                     // 承接提交完成（active + v2 binding）

	// Suspend 完整跑完（锁空闲——恢复路径未持锁）：v2 gate Release → suspending
	// → 本地清理 → suspended。
	if serr := m.Suspend(ctx, "t-race"); serr != nil {
		t.Fatalf("Suspend during interleave: %v", serr)
	}

	close(release) // 放行接管
	aerr := <-done
	if aerr == nil || !strings.Contains(aerr.Error(), "放弃接管") {
		t.Fatalf("takeover must abort on converged status, got %v", aerr)
	}
	if m.getRuntime("t-race") != nil {
		t.Fatalf("runtime must NOT be registered for suspended task（不重建运行关联）")
	}
	frow, ferr := bstore.GetTask(ctx, "t-race")
	if ferr != nil || frow.Status != "suspended" {
		t.Fatalf("status: %s %v", frow.Status, ferr)
	}
}

// commitBarrierStore 包装真实 store：CommitAdoption 落库后阻塞直至放行
// （I15 交错屏障——生产语义不变，仅测试同步）。
type commitBarrierStore struct {
	*StoreAdapter
	committed chan struct{}
	release   chan struct{}
	once      *sync.Once
}

func (s *commitBarrierStore) CommitAdoption(ctx context.Context, taskID string, b, oldBinding domain.TaskBinding, fromStatus string, lastError *string) (application.TransitionResult, error) {
	res, err := s.StoreAdapter.CommitAdoption(ctx, taskID, b, oldBinding, fromStatus, lastError)
	if err == nil && res.Matched {
		s.once.Do(func() {
			close(s.committed)
			<-s.release
		})
	}
	return res, err
}

// ---- I10：候选流复用——「第二次订阅必失败」注入下公开 Activate 仍成功 ----

// onceEventsSrc 订阅计数：第 2 次订阅即失败（回归信号——接管 MUST 复用候选流，
// 不重订阅）。就绪流 SetCallbacks 同步触发 onConnected（候选握手立即满足）。
type onceEventsSrc struct {
	n int32
}

func (s *onceEventsSrc) Subscribe(ctx context.Context, b domain.TaskBinding) (appagent.EventStream, error) {
	if atomic.AddInt32(&s.n, 1) >= 2 {
		return nil, errors.New("second subscribe must not happen（接管应复用候选流）")
	}
	return readyStubStream{}, nil
}

// readyStubStream 握手即就绪的最小 EventStream stub。
type readyStubStream struct {
	closeN int32
}

func (s readyStubStream) Events() <-chan domain.AgentEvent { return nil }
func (s readyStubStream) Err() error                       { return nil }
func (s readyStubStream) Close() error {
	atomic.AddInt32(&s.closeN, 1)
	return nil
}

func (s readyStubStream) SetCallbacks(onConnected func(), onReconnect func()) {
	if onConnected != nil {
		onConnected()
	}
}

func (s readyStubStream) SetDisconnectCallback(cb func()) {}

// countEventsProvider 包装 branchBV2Sup + 订阅计数 Events（端点/归属同 branchB）。
type countEventsProvider struct {
	v2Stub
	sup *branchBV2Sup
	src *onceEventsSrc
}

func (p *countEventsProvider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *countEventsProvider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *countEventsProvider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return p.sup.port, "derived", "/wt", nil
}

func (p *countEventsProvider) Events() appagent.EventSource { return p.src }

// TestStage4_I10_TakeoverReusesCandidateStream_SecondSubscribeFails 落实 I10：
// 首建「第一次订阅成功、第二次订阅必失败」——公开 Activate 仍成功（接管复用
// 候选流，不重订阅）：状态 active、binding 提交、订阅恰一次。
func TestStage4_I10_TakeoverReusesCandidateStream_SecondSubscribeFails(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-i10")
	sup := &branchBV2Sup{upstream: "ses_new_1", port: 4096}
	src := &onceEventsSrc{}
	m := newBranchBManager(t, db, sup, false)
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&countEventsProvider{sup: sup, src: src}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if aerr := m.Activate(ctx, "t-i10"); aerr != nil {
		t.Fatalf("Activate must succeed via candidate-stream reuse, got %v", aerr)
	}
	if n := atomic.LoadInt32(&src.n); n != 1 {
		t.Fatalf("subscribes=%d, want exactly 1（候选流复用——无第二次订阅）", n)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-i10")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	got, has, gerr := adapter.GetTaskBinding(ctx, "t-i10")
	if gerr != nil || !has || got.UpstreamSessionID != "ses_new_1" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, gerr)
	}
}

// ---- 批次 2：I2（Suspend v2 分派）/ I3（终端 attach）/ I6（attention 桥）/ I7（ownership 门禁）----

// v2BindingFor 构造测试 v2 binding（port 4096/dir /wt/origin created；隔离与
// 挂起测试的多任务身份用）。
func v2BindingFor(taskID, upstream string) domain.TaskBinding {
	raw, _ := json.Marshal(map[string]any{
		"port": 4096, "directory": "/wt", "taskId": taskID,
		"origin": "created", "auth": map[string]string{"kind": "derived"},
	})
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  upstream,
		TerminalRef:        "ocdeck-v2-tui-" + taskID,
		Data:               raw,
	}
}

// suspendV2Sup 记录 Release 停止路径的 binding upstream（评审 I2）。
type suspendV2Sup struct {
	mu         sync.Mutex
	released   []string
	releaseN   int32
	releaseErr error
}

func (s *suspendV2Sup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected Acquire")
}

func (s *suspendV2Sup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected Resume")
}

func (s *suspendV2Sup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected AcquireEphemeral")
}

func (s *suspendV2Sup) Release(ctx context.Context, b domain.TaskBinding) error {
	atomic.AddInt32(&s.releaseN, 1)
	s.mu.Lock()
	s.released = append(s.released, b.UpstreamSessionID)
	s.mu.Unlock()
	return s.releaseErr
}

func (s *suspendV2Sup) releasedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.released...)
}

// fakeTerminal 记录 EnsureTerminal 入参并按注入结果应答（评审 I3：预验证先于
// 创建的顺序断言）。
type fakeTerminal struct {
	ensureGot domain.TaskBinding
	ensureErr error
	argvCalls int32
}

func (f *fakeTerminal) EnsureTerminal(ctx context.Context, b domain.TaskBinding) (string, error) {
	f.ensureGot = b
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return b.TerminalRef, nil
}

func (f *fakeTerminal) AttachCommand(ctx context.Context, b domain.TaskBinding) ([]string, error) {
	atomic.AddInt32(&f.argvCalls, 1)
	return []string{"opencode", "--server", "http://127.0.0.1:4096", "--session", b.UpstreamSessionID}, nil
}

// suspendV2Provider：supervisor/归属校验/endpoint 计数/Terminal 可配置
// （评审 I2/I3/I7 共用 fake）。
type suspendV2Provider struct {
	v2Stub
	sup           *suspendV2Sup
	ownershipErr  error
	endpointCalls int32
	terminal      appagent.TerminalIO
	sessions      appagent.SessionControl
}

func (p *suspendV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *suspendV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return p.ownershipErr
}

func (p *suspendV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	atomic.AddInt32(&p.endpointCalls, 1)
	return 4096, "derived", "/wt", nil
}

func (p *suspendV2Provider) Terminal() appagent.TerminalIO {
	if p.terminal != nil {
		return p.terminal
	}
	return v2Stub{}.Terminal()
}

func (p *suspendV2Provider) Sessions() appagent.SessionControl { return p.sessions }

// newSuspendManager 构造挂起/终端测试 Manager（真实 store + v2 fake registry）。
func newSuspendManager(t *testing.T, db *store.DB, prov *suspendV2Provider) *Manager {
	t.Helper()
	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	return m
}

// TestStage4_Suspend_V2_StopOK_SuspendsAndReclaimsLocalUnits 落实评审 I2（停止
// 成功侧）：Manager.Suspend 经实际 binding 分派 v2 supervisor（Release=停止，
// 上游=真实 upstream）→ 收敛 suspended；本地自有单元（v2 TUI/shell）回收；
// v1 serve/runtime 单元判定不参与（无强杀、无修复树）。
func TestStage4_Suspend_V2_StopOK_SuspendsAndReclaimsLocalUnits(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-s1")
	prov := &suspendV2Provider{sup: &suspendV2Sup{}}
	m := newSuspendManager(t, db, prov)
	proc := m.proc.(*mockProc)
	proc.mu.Lock()
	proc.sessions["ocdeck-v2-tui-t-s1"] = true
	proc.sessions["ocdeck-t-s1-shell-1"] = true
	proc.mu.Unlock()

	if err := m.Suspend(ctx, "t-s1"); err != nil {
		t.Fatalf("Suspend(v2): %v", err)
	}
	if got := prov.sup.releasedSnapshot(); len(got) != 1 || got[0] != "ses_keep" {
		t.Fatalf("stop must target real upstream session: %v", got)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-s1")
	if err != nil || row.Status != "suspended" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	proc.mu.Lock()
	_, tuiLeft := proc.sessions["ocdeck-v2-tui-t-s1"]
	_, shellLeft := proc.sessions["ocdeck-t-s1-shell-1"]
	proc.mu.Unlock()
	if tuiLeft || shellLeft {
		t.Fatalf("local units must be reclaimed (tui=%v shell=%v)", tuiLeft, shellLeft)
	}
	for _, name := range proc.killOrder {
		if name == runtimeSessionName("t-s1") || name == serveSessionName("t-s1") {
			t.Fatalf("v1 unit %s must not participate in v2 suspend", name)
		}
	}
}

// TestStage4_Suspend_V2_StopFail_KeepsActive 落实 D4 挂起行失败侧：停止失败 →
// 保持 active + last_error、保留运行关联（不 clearRuntime、不进 v1 强杀补偿、
// 不清 env 快照）。
func TestStage4_Suspend_V2_StopFail_KeepsActive(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-s2")
	prov := &suspendV2Provider{sup: &suspendV2Sup{releaseErr: appagent.NewError("supervisor.release",
		appagent.ErrKindRuntimeUnavailable, errors.New("interrupt failed"))}}
	m := newSuspendManager(t, db, prov)
	rt := m.newRuntime("t-s2")
	m.setRuntime("t-s2", rt)

	if err := m.Suspend(ctx, "t-s2"); err == nil || !strings.Contains(err.Error(), "suspend stop") {
		t.Fatalf("expect suspend stop failure, got %v", err)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-s2")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v（停止失败保持 active）", row.Status, err)
	}
	if !strings.Contains(row.LastError.String, "suspend stop failed") {
		t.Fatalf("last_error: %q", row.LastError.String)
	}
	if m.getRuntime("t-s2") == nil {
		t.Fatalf("running association must be kept（不 clearRuntime）")
	}
	if atomic.LoadInt32(&prov.sup.releaseN) != 1 {
		t.Fatalf("release attempts=%d, want 1", atomic.LoadInt32(&prov.sup.releaseN))
	}
}

// TestStage4_Suspend_V2_OwnershipFail_BeforeEndpoint 落实评审 I7：归属校验失败
// 先于凭据派生与停止副作用——Release 与 endpoint 解析计数均为零。
func TestStage4_Suspend_V2_OwnershipFail_BeforeEndpoint(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-s3")
	prov := &suspendV2Provider{
		sup:          &suspendV2Sup{},
		ownershipErr: appagent.NewError("ownership", appagent.ErrKindBindingInvalid, errors.New("not owner")),
	}
	m := newSuspendManager(t, db, prov)

	if err := m.Suspend(ctx, "t-s3"); err == nil {
		t.Fatalf("ownership failure must reject suspend")
	}
	if atomic.LoadInt32(&prov.sup.releaseN) != 0 {
		t.Fatalf("release must not run before ownership check")
	}
	if atomic.LoadInt32(&prov.endpointCalls) != 0 {
		t.Fatalf("endpoint/credential derivation must not run before ownership check, calls=%d", prov.endpointCalls)
	}
	adapter := NewStoreAdapter(db)
	row, err := adapter.GetTask(ctx, "t-s3")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v（零副作用准入）", row.Status, err)
	}
}

// TestStage4_Suspend_V2_Isolation_ANotB 落实任务隔离（D9）：同 server 双任务
// ——A 挂起仅停止 A 的 upstream session；B 零触达、保持 active。
func TestStage4_Suspend_V2_Isolation_ANotB(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTaskUpstream(t, db, "t-a", "ses_a")
	seedV2ActiveTaskUpstream(t, db, "t-b", "ses_b")
	prov := &suspendV2Provider{sup: &suspendV2Sup{}}
	m := newSuspendManager(t, db, prov)

	if err := m.Suspend(ctx, "t-a"); err != nil {
		t.Fatalf("Suspend(A): %v", err)
	}
	got := prov.sup.releasedSnapshot()
	if len(got) != 1 || got[0] != "ses_a" {
		t.Fatalf("only A upstream must be stopped: %v", got)
	}
	adapter := NewStoreAdapter(db)
	rowB, err := adapter.GetTask(ctx, "t-b")
	if err != nil || rowB.Status != "active" {
		t.Fatalf("B status: %s %v（A 挂起不影响 B）", rowB.Status, err)
	}
}

// TestStage4_ReopenAttach_V2_TUIUnit 落实评审 I3：v2 终端 attach——EnsureTerminal
// 预验证先于创建；幂等创建 ocdeck-v2-tui-<taskID>（AttachCommand argv + 派生
// 凭据 env OPENCODE_SERVER_PASSWORD + worktree dir）；预验证失败零创建。
func TestStage4_ReopenAttach_V2_TUIUnit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-at")
	term := &fakeTerminal{}
	prov := &suspendV2Provider{sup: &suspendV2Sup{}, terminal: term}
	m := newSuspendManager(t, db, prov)
	proc := m.proc.(*mockProc)

	// ① 预验证失败（NotFound）→ typed not_found 且零创建（顺序断言）。
	term.ensureErr = appagent.NewError("terminal.ensure", appagent.ErrKindNotFound, errors.New("session gone"))
	if _, err := m.ReopenAttach(ctx, "t-at"); err == nil || OpErrorCode(err) != codeNotFound {
		t.Fatalf("expect not_found, got %v", err)
	}
	if n := len(proc.newSessionNames); n != 0 {
		t.Fatalf("prevalidation failure must not create unit, created=%v", proc.newSessionNames)
	}

	// ② 预验证通过 → 幂等创建：argv/env/目录断言。
	term.ensureErr = nil
	tid, err := m.ReopenAttach(ctx, "t-at")
	if err != nil {
		t.Fatalf("ReopenAttach(v2): %v", err)
	}
	ref := "ocdeck-v2-tui-t-at"
	if string(tid) != ref {
		t.Fatalf("terminal id: %q want %q", tid, ref)
	}
	proc.mu.Lock()
	argv := proc.cmdArgvValues[ref]
	env := proc.envValues[ref]
	proc.mu.Unlock()
	if len(argv) != 5 || argv[2] != "http://127.0.0.1:4096" || argv[4] != "ses_keep" {
		t.Fatalf("attach argv: %v（自动选中机制 D8-7）", argv)
	}
	if env["OPENCODE_SERVER_PASSWORD"] == "" {
		t.Fatalf("derived credential env missing")
	}
	if atomic.LoadInt32(&term.argvCalls) != 1 {
		t.Fatalf("AttachCommand calls=%d, want 1", term.argvCalls)
	}

	// ③ 幂等：单元已在 → 不重复创建，同一 ID。
	tid2, err := m.ReopenAttach(ctx, "t-at")
	if err != nil || tid2 != tid {
		t.Fatalf("idempotent reopen: %q %v", tid2, err)
	}
	proc.mu.Lock()
	count := 0
	for _, n := range proc.newSessionNames {
		if n == ref {
			count++
		}
	}
	proc.mu.Unlock()
	if count != 1 {
		t.Fatalf("unit must be created exactly once, created %d times", count)
	}
}

// TestStage4_I14_ReopenAttachThenDelete_V2TUIUnitChain 落实 I14 清理链全链：
// ReopenAttach 创建的 v2 TUI 托管单元（canonical parser 已识别归属）经 Manager.Delete
// 纳入清理（kill 命中 ocdeck-v2-tui-<taskID>）；kill 失败 → deletion_failed +
// notice 持久化含该单元名（非净清理 tickets 归属不丢失）。
func TestStage4_I14_ReopenAttachThenDelete_V2TUIUnitChain(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-del")
	prov := &suspendV2Provider{sup: &suspendV2Sup{}, terminal: &fakeTerminal{}}
	m := newSuspendManager(t, db, prov)
	proc := m.proc.(*mockProc)

	// canonical parser 归属解析（util/reconcile/shutdown 分组同源）。
	tid, suffix, perr := process.ParseSessionName("ocdeck-v2-tui-t-del")
	if perr != nil || tid != "t-del" || suffix != "v2-tui" {
		t.Fatalf("parser: %q %q %v", tid, suffix, perr)
	}

	// ReopenAttach 创建 TUI 单元。
	proc.mu.Lock()
	proc.sessions["ocdeck-v2-tui-t-del"] = true
	proc.mu.Unlock()
	if _, err := m.ReopenAttach(ctx, "t-del"); err != nil {
		t.Fatalf("ReopenAttach: %v", err)
	}
	// active 不可直删（状态机守卫）——先经 v2 挂起（停止确认 → suspended）。
	if serr := m.Suspend(ctx, "t-del"); serr != nil {
		t.Fatalf("Suspend before delete: %v", serr)
	}

	// Delete 全链：v2 TUI 单元被纳入清理（killOrder 命中）。
	if derr := m.Delete(ctx, "t-del", DeleteForce, true); derr != nil {
		t.Fatalf("Delete: %v", derr)
	}
	proc.mu.Lock()
	killed := false
	for _, n := range proc.killOrder {
		if n == "ocdeck-v2-tui-t-del" {
			killed = true
		}
	}
	proc.mu.Unlock()
	if !killed {
		t.Fatalf("delete must reclaim v2 TUI unit, killOrder=%v", proc.killOrder)
	}
}

// TestStage4_I14_V2TUIKillFail_NoticeAttribution 落实 I14 非净清理归属：
// v2 TUI 单元 kill 失败 → deletion_failed + notice 持久化（sessionName=该单元名
// ——parser 归属修复保证 tickets/notice 不丢任务身份）。
func TestStage4_I14_V2TUIKillFail_NoticeAttribution(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-del2")
	prov := &suspendV2Provider{sup: &suspendV2Sup{}, terminal: &fakeTerminal{}}
	m := newSuspendManager(t, db, prov)
	proc := m.proc.(*mockProc)
	proc.mu.Lock()
	proc.sessions["ocdeck-v2-tui-t-del2"] = true
	proc.killResults["ocdeck-v2-tui-t-del2"] = process.KillResult{
		SessionKilled:  false,
		Disposition:    process.DispositionKillFailed,
		CleanupTickets: []string{"ocdeck-v2-tui-t-del2"},
	}
	proc.mu.Unlock()
	// active 不可直删——先经 v2 挂起（kill failpoint 在 Delete 重入清理时命中）。
	if serr := m.Suspend(ctx, "t-del2"); serr != nil {
		t.Fatalf("Suspend before delete: %v", serr)
	}

	if derr := m.Delete(ctx, "t-del2", DeleteForce, true); derr == nil {
		t.Fatalf("kill failure must surface deletion_failed")
	}
	row, gerr := NewStoreAdapter(db).GetTask(ctx, "t-del2")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !strings.Contains(row.Notice.String, "ocdeck-v2-tui-t-del2") {
		t.Fatalf("non-clean cleanup notice must attribute v2 TUI unit, notice=%q", row.Notice.String)
	}
}

// TestStage4_ReopenAttach_V2_OwnershipFail_BeforeEndpoint 落实评审 I7（终端
// 入口同门禁）：归属失败先于凭据派生（endpoint 计数为零）。
func TestStage4_ReopenAttach_V2_OwnershipFail_BeforeEndpoint(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-at2")
	prov := &suspendV2Provider{
		sup:          &suspendV2Sup{},
		ownershipErr: appagent.NewError("ownership", appagent.ErrKindBindingInvalid, errors.New("not owner")),
		terminal:     &fakeTerminal{},
	}
	m := newSuspendManager(t, db, prov)
	if _, err := m.ReopenAttach(ctx, "t-at2"); err == nil || OpErrorCode(err) != codeConflict {
		t.Fatalf("expect conflict, got %v", err)
	}
	if atomic.LoadInt32(&prov.endpointCalls) != 0 {
		t.Fatalf("endpoint must not run before ownership check")
	}
}

// ---- I6：attention 桥真实 session 身份 ----

// recordingAttention 记录 AttentionIO 入参（bridge 消费断言）。
type recordingAttention struct {
	mu        sync.Mutex
	listKeys  []domain.SessionKey
	perms     []domain.PermissionPayload
	replySess []string
	replyIDs  []string
}

func (a *recordingAttention) ListPermissions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.PermissionPayload, error) {
	a.mu.Lock()
	a.listKeys = append(a.listKeys, s)
	a.mu.Unlock()
	return []domain.PermissionPayload{{ID: "per_" + s.UpstreamID, SessionID: s.UpstreamID, Permission: "bash"}}, nil
}

func (a *recordingAttention) ListQuestions(ctx context.Context, b domain.TaskBinding, s domain.SessionKey) ([]domain.QuestionPayload, error) {
	a.mu.Lock()
	a.listKeys = append(a.listKeys, s)
	a.mu.Unlock()
	return nil, nil
}

func (a *recordingAttention) Reply(ctx context.Context, b domain.TaskBinding, at domain.Attention, reply appagent.AttentionReply) error {
	a.mu.Lock()
	a.replySess = append(a.replySess, at.Session.UpstreamID)
	a.replyIDs = append(a.replyIDs, at.ID)
	a.mu.Unlock()
	return nil
}

// stubSessions 非 nil 占位（bridgeClientFor 的能力出口门禁）。
type stubSessions struct{}

func (stubSessions) Create(ctx context.Context, b domain.TaskBinding) (domain.AgentSession, error) {
	return domain.AgentSession{}, errors.New("unexpected")
}
func (stubSessions) Get(ctx context.Context, b domain.TaskBinding, k domain.SessionKey) (domain.AgentSession, error) {
	return domain.AgentSession{}, errors.New("unexpected")
}
func (stubSessions) Delete(ctx context.Context, b domain.TaskBinding, k domain.SessionKey) error {
	return errors.New("unexpected")
}
func (stubSessions) List(ctx context.Context, b domain.TaskBinding) (appagent.ListResult, error) {
	return appagent.ListResult{}, errors.New("unexpected")
}

// TestStage4_Bridge_V2_Attention_CarriesRealSession 落实评审 I6：v2 桥——列表
// 按归属 session 集合逐 session 聚合；回复从 attention 事实携带真实 sessionID
// （MUST NOT 用 requestID 冒充 session）。
func TestStage4_Bridge_V2_Attention_CarriesRealSession(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTaskUpstream(t, db, "t-br", "ses_main")
	adapter := NewStoreAdapter(db)
	now := time.Now().Unix()
	for _, up := range []string{"ses_a", "ses_b"} {
		if _, err := adapter.ClaimTaskSessionKey(ctx, "t-br",
			domain.SessionKey{ProviderID: ocdeckstore.ProviderIDV2, ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default, UpstreamID: up},
			now, now, now, ""); err != nil {
			t.Fatal(err)
		}
	}
	att := &recordingAttention{}
	prov := &suspendV2Provider{sup: &suspendV2Sup{}, sessions: stubSessions{}}
	m := newSuspendManager(t, db, prov)
	// 注入 recording attention：bridgeClientFor 经 providerFor 取 Provider——
	// suspendV2Provider 的 Attention 走 v2Stub（nil）会令桥构造后退回 fallback？
	// 不——桥构造只查 Sessions() 非 nil；Attention 经 p.Attention() 调用。
	// 用包装 provider 覆盖 Attention。
	provWithAtt := &attentionV2Provider{suspendV2Provider: *prov, att: att}
	reg := appagent.NewRegistry()
	if err := reg.Register(provWithAtt); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	rt := m.newRuntime("t-br")
	rt.attention = newAttentionState()
	rt.attention.applyAttentionEvent(appagent.AttentionEvent{
		Kind: appagent.AttentionAsked, Type: appagent.AttentionPermission,
		RequestID: "per_1", SessionID: "ses_a", Permission: "bash",
	})
	m.setRuntime("t-br", rt)

	binding := v2BindingFor("t-br", "ses_main")
	data := &opencodeBindingData{ProviderID: ocdeckstore.ProviderIDV2, Port: 4096, Directory: "/wt", TaskID: "t-br"}
	bridge := m.bridgeClientFor(data, binding, m.ocFactory(ocdeckstore.ProviderIDV2, 4096, "pw", appagent.Options{}))
	if bridge == nil {
		t.Fatalf("bridge must be constructed for provider with capability exits")
	}

	// 列表：按归属 session 集合聚合（ses_a/ses_b 各一次——main session 未 claim
	// 不参与；上游返回按 session 标识拼接可验证聚合）。
	perms, err := bridge.ListPermissions(ctx, "/wt")
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("aggregated permissions=%d, want 2 (per_ses_a + per_ses_b)", len(perms))
	}
	gotKeys := map[string]bool{}
	for _, k := range att.listKeys {
		gotKeys[k.UpstreamID] = true
	}
	if !gotKeys["ses_a"] || !gotKeys["ses_b"] {
		t.Fatalf("list must query owned sessions: %v", gotKeys)
	}

	// 回复：真实 sessionID（ses_a——attention 事实），非 requestID。
	if err := bridge.ReplyPermission(ctx, "/wt", "per_1", "once"); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	if len(att.replySess) != 1 || att.replySess[0] != "ses_a" || att.replyIDs[0] != "per_1" {
		t.Fatalf("reply must carry real session id: sess=%v ids=%v", att.replySess, att.replyIDs)
	}
}

// attentionV2Provider 在 suspendV2Provider 上覆盖 Attention 出口（桥测试注入）。
type attentionV2Provider struct {
	suspendV2Provider
	att *recordingAttention
}

func (p *attentionV2Provider) Attention() appagent.AttentionIO { return p.att }

// ---- 评审 I1：承接 A 经公开入口可达 + 失败保留全部旧事实 ----

// supOnlyV2Provider 通用 v2 provider 测试 fake：任意 RuntimeSupervisor + 可选
// EventSource（nil = 无 EventSource 测试接缝形态）；归属校验通过、端点同端口。
type supOnlyV2Provider struct {
	v2Stub
	sup    appagent.RuntimeSupervisor
	events appagent.EventSource
}

func (p *supOnlyV2Provider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *supOnlyV2Provider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *supOnlyV2Provider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 4096, "derived", "/wt", nil
}

func (p *supOnlyV2Provider) Events() appagent.EventSource { return p.events }

// newAdoptATestManager 构造承接 A 测试 Manager（真实 store + machine v2 开 + v1
// 钉扎准入 + 指定 v2 fake provider）。
func newAdoptATestManager(t *testing.T, db *store.DB, v2prov appagent.Provider) *Manager {
	t.Helper()
	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(context.Background())
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(v2prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	return m
}

// adoptAFactsSnapshot 读取承接 A 的旧事实（status/anchor upstream/归属行 v1 数/
// env 快照原文），供失败保留断言。
func adoptAFactsSnapshot(t *testing.T, db *store.DB, taskID string) (status, upstream string, nV1 int, env string) {
	t.Helper()
	_ = db.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&status)
	_ = db.QueryRow(`SELECT anchor_session_id FROM tasks WHERE id=?`, taskID).Scan(&upstream)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id=? AND provider_id=?`,
		taskID, ocdeckstore.ProviderIDV1).Scan(&nV1)
	_ = db.QueryRow(`SELECT COALESCE(env_snapshot,'') FROM tasks WHERE id=?`, taskID).Scan(&env)
	return
}

// TestStage4_AdoptA_ViaActivate_FailureKeepsAllOldFacts 落实评审 I1：承接 A 经公开
// Activate 可达（suspended→active 守卫，不经 activating），Resume 失败（原
// session 不可读）→ 零状态副作用、保留全部旧事实（suspended/v1 binding/归属行/
// env 快照），MUST NOT 走普通激活补偿（不清 env 快照）。
func TestStage4_AdoptA_ViaActivate_FailureKeepsAllOldFacts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-a1", ocdeckstore.ProviderIDV1, "sess-keep")
	failSup := &failResumeV2Sup{}
	m := newAdoptATestManager(t, db, &supOnlyV2Provider{sup: failSup})

	aerr := m.Activate(ctx, "t-a1")
	if aerr == nil || !strings.Contains(aerr.Error(), "original session unreadable") {
		t.Fatalf("expect adoption failure, got %v", aerr)
	}
	if atomic.LoadInt32(&failSup.resumeCalls) != 1 {
		t.Fatalf("resume=%d（承接 A 经 Activate 分派）", atomic.LoadInt32(&failSup.resumeCalls))
	}
	status, upstream, nV1, env := adoptAFactsSnapshot(t, db, "t-a1")
	if status != "suspended" {
		t.Fatalf("status=%s（零状态副作用——不经 activating/补偿回退）", status)
	}
	if upstream != "sess-keep" || nV1 != 2 {
		t.Fatalf("old facts must be intact: upstream=%s nV1=%d", upstream, nV1)
	}
	if !strings.Contains(env, "OCDECK_TASK_ID") {
		t.Fatalf("env snapshot must be preserved（承接失败不走普通激活补偿）: %q", env)
	}
}

// TestStage4_AdoptA_ViaActivate_SuccessKeepsSameSession 落实评审 I1 成功面：承接 A
// 经公开 Activate → 单事务承接提交（binding v2、upstream=原 session、active），
// 只 Resume 不 Acquire。
func TestStage4_AdoptA_ViaActivate_SuccessKeepsSameSession(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-a2", ocdeckstore.ProviderIDV1, "sess-keep")
	rec := &recordingV2Sup{upstream: "sess-keep", port: 4096}
	m := newAdoptATestManager(t, db, &supOnlyV2Provider{sup: rec})

	if err := m.Activate(ctx, "t-a2"); err != nil {
		t.Fatalf("Activate(adopt A): %v", err)
	}
	if rec.resumeCalls != 1 || rec.acquireCalls != 0 {
		t.Fatalf("resume=%d acquire=%d（承接 A 只 Resume——延续原 session）", rec.resumeCalls, rec.acquireCalls)
	}
	adapter := NewStoreAdapter(db)
	got, has, err := adapter.GetTaskBinding(ctx, "t-a2")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "sess-keep" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	row, err := adapter.GetTask(ctx, "t-a2")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
}

// TestStage4_AdoptA_ViaRecovery_KeepsFactsOnFailure 落实评审 I1 恢复面：active 存量
// 任务的恢复入口（ensureRecovery）先解析路径——承接 A 走 active→active 守卫专用
// 出口（MUST NOT 写 activating）；Resume 失败保留全部旧事实（active/v1 binding/
// 归属行/env 快照）。
func TestStage4_AdoptA_ViaRecovery_KeepsFactsOnFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-a3", ocdeckstore.ProviderIDV1, "sess-keep")
	if _, err := db.Exec(`UPDATE tasks SET status='active' WHERE id='t-a3'`); err != nil {
		t.Fatal(err)
	}
	failSup := &failResumeV2Sup{}
	m := newAdoptATestManager(t, db, &supOnlyV2Provider{sup: failSup})
	// ensureRecovery 前置校验要求注册表 runtime + token 匹配（watcher 语义）。
	rt := m.newRuntime("t-a3")
	m.setRuntime("t-a3", rt)

	m.ensureRecovery("t-a3", rt.instVersion, false)
	if atomic.LoadInt32(&failSup.resumeCalls) != 1 {
		t.Fatalf("resume=%d（恢复入口承接 A 分派）", atomic.LoadInt32(&failSup.resumeCalls))
	}
	status, upstream, nV1, env := adoptAFactsSnapshot(t, db, "t-a3")
	if status != "active" {
		t.Fatalf("status=%s（active→active 守卫失败零状态副作用——MUST NOT 写 activating/abandon）", status)
	}
	if upstream != "sess-keep" || nV1 != 2 || !strings.Contains(env, "OCDECK_TASK_ID") {
		t.Fatalf("old facts must be intact: upstream=%s nV1=%d env=%q", upstream, nV1, env)
	}
}

// ---- 评审 I4：R5（v2 钉扎新任务）首建经公开 Activate 可达 ----

// TestStage4_R5_V2PinnedFirstBuild_ViaActivate 落实评审 I4：v2 钉扎、无 binding 的
// suspended 任务激活 → firstBuildV2（与 R8-B 共用同一首建流程）——Acquire 首建、
// claim/anchor/binding/active 原子落库、钉扎值不变。
func TestStage4_R5_V2PinnedFirstBuild_ViaActivate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-r5")
	if _, err := db.Exec(`UPDATE tasks SET provider_id=? WHERE id='t-r5'`, ocdeckstore.ProviderIDV2); err != nil {
		t.Fatal(err)
	}
	sup := &branchBV2Sup{upstream: "ses_r5_1", port: 4096}
	adapter := NewStoreAdapter(db)
	oc := newMockOC(true)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), oc)
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(&branchBV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	// mock 上游列出本次 candidate（提交后对齐保留归属行——断言 claim）。
	oc.mu.Lock()
	oc.sessions = []opencode.Session{{ID: "ses_r5_1", Time: opencode.SessionTime{Created: 100, Updated: 100}}}
	oc.mu.Unlock()

	if err := m.Activate(ctx, "t-r5"); err != nil {
		t.Fatalf("Activate(R5 first-build): %v", err)
	}
	if atomic.LoadInt32(&sup.acquireCalls) != 1 {
		t.Fatalf("acquire=%d, want 1", atomic.LoadInt32(&sup.acquireCalls))
	}
	got, has, err := adapter.GetTaskBinding(ctx, "t-r5")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "ses_r5_1" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	row, err := adapter.GetTask(ctx, "t-r5")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	var nV2 int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-r5' AND provider_id=?`, ocdeckstore.ProviderIDV2).Scan(&nV2)
	if nV2 != 1 {
		t.Fatalf("claim rows=%d（首建 claim 原子落库）", nV2)
	}
}

// ---- 评审 I10：v2 激活提交前候选握手——失败保留旧 binding ----

// TestStage4_V2Resume_HandshakeFail_KeepsOldBinding 落实评审 I10：已有 v2 binding
// 激活的候选握手（订阅）失败 → 提交未发生（binding 保持旧值/旧 upstream）、任务
// 经既有激活补偿收敛 suspended；MUST NOT 出现「先提交 active 再建 SSE」的顺序。
func TestStage4_V2Resume_HandshakeFail_KeepsOldBinding(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdoptAFormTask(t, db, "t-h1", ocdeckstore.ProviderIDV2, "sess-keep")
	// v2 钉扎任务持既有 v2 binding（R1 形态：binding 决定执行身份）。
	old := domain.TaskBinding{
		Version: domain.TaskBindingVersion, ProviderID: ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  "ses_old_1", TerminalRef: "ocdeck-v2-tui-t-h1",
		Data: []byte(`{"port":4096,"directory":"/wt","taskId":"t-h1","origin":"created","auth":{"kind":"derived"}}`),
	}
	if err := NewStoreAdapter(db).SetTaskBinding(ctx, "t-h1", old); err != nil {
		t.Fatal(err)
	}
	rec := &recordingV2Sup{upstream: "ses_old_1", port: 4096}
	m := newAdoptATestManager(t, db, &handshakeFailV2Provider{sup: rec})

	aerr := m.Activate(ctx, "t-h1")
	if aerr == nil || !strings.Contains(aerr.Error(), "sse subscribe") {
		t.Fatalf("expect handshake subscribe failure, got %v", aerr)
	}
	if rec.resumeCalls != 1 {
		t.Fatalf("resume=%d", rec.resumeCalls)
	}
	// 提交未发生：binding 保持旧 envelope（代际一致——CommitV2Resume 未执行）。
	got, has, err := NewStoreAdapter(db).GetTaskBinding(ctx, "t-h1")
	if err != nil || !has || got.UpstreamSessionID != "ses_old_1" {
		t.Fatalf("binding must stay old: %+v has=%v err=%v", got, has, err)
	}
	// 候选握手失败属提交前失败 → 既有激活补偿收敛 suspended（D4 已有 binding 激活
	// 失败行）。
	row, err := NewStoreAdapter(db).GetTask(ctx, "t-h1")
	if err != nil || row.Status != "suspended" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
}

// ---- Round 3 批次 4：I2 重放 / I15 锁域 / I16 候选流所有权 / I10 真实流移交 ----

// barrierStore 是接管守卫读屏障（I15 锁域测试）：armed 后首次 GetTask 阻塞至
// release——固定「接管守卫已入锁域」与「Suspend 发起」的交错顺序。
type barrierStore struct {
	*StoreAdapter
	armed   atomic.Bool
	enter   chan struct{}
	release chan struct{}
	tripped atomic.Bool
}

func (s *barrierStore) GetTask(ctx context.Context, taskID string) (TaskRow, error) {
	if s.armed.Load() && s.tripped.CompareAndSwap(false, true) {
		close(s.enter)
		<-s.release
	}
	return s.StoreAdapter.GetTask(ctx, taskID)
}

// TestStage4_I15_RecoveryTakeoverLockDomain_SuspendWaitsReadyPublish 落实 I15：
// 恢复路径接管（taskLocked=false）的注册→SSE→ready 发布全程持有任务锁——
// 「接管前段成功→Suspend 完成→旧接管发布 ready」交错不可能：Suspend 在锁外
// 等待，接管完成后才执行清理（无复活关联/无旧代 ready 发布）。
func TestStage4_I15_RecoveryTakeoverLockDomain_SuspendWaitsReadyPublish(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-ix")
	adapter := NewStoreAdapter(db)
	tstore := &barrierStore{StoreAdapter: adapter, enter: make(chan struct{}), release: make(chan struct{})}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, tstore, prov)
	m.setRuntime("t-ix", m.newRuntime("t-ix"))

	row, err := tstore.GetTask(ctx, "t-ix")
	if err != nil {
		t.Fatal(err)
	}
	binding, has, err := tstore.GetTaskBinding(ctx, "t-ix")
	if err != nil || !has {
		t.Fatalf("binding: %v", err)
	}

	// 接管 goroutine（恢复路径形态——taskLocked=false，守卫自取任务锁）。
	tstore.armed.Store(true)
	takeoverDone := make(chan error, 1)
	go func() {
		takeoverDone <- m.commitV2RuntimeReady(ctx, "t-ix", row, 4096, "pw", binding, AlignModeRepo, nil, false)
	}()
	<-tstore.enter // 守卫已持锁、阻塞在重验读——锁域内

	// 锁域期间 Suspend 不得推进（无 interrupt、无状态转换）。
	suspendDone := make(chan error, 1)
	go func() { suspendDone <- m.Suspend(ctx, "t-ix") }()
	time.Sleep(120 * time.Millisecond)
	if atomic.LoadInt32(&sup.releaseN) != 0 {
		t.Fatalf("suspend must block on task lock during takeover, releases=%d", atomic.LoadInt32(&sup.releaseN))
	}
	if r, _ := tstore.GetTask(ctx, "t-ix"); r.Status != "active" {
		t.Fatalf("status must stay active while takeover holds the lock, got %s", r.Status)
	}

	// 放行接管 → 完成（active+runtime）→ Suspend 获锁执行 → suspended+清理。
	close(tstore.release)
	if err := <-takeoverDone; err != nil {
		t.Fatalf("takeover: %v", err)
	}
	// Suspend 对并发占用快失败（busy——v1 既有语义：并发操作不排队）；锁域内
	// 零副作用已由上方断言。重试即成功。
	if serr := <-suspendDone; serr == nil {
		t.Fatalf("concurrent suspend must fail fast busy")
	} else if !strings.Contains(serr.Error(), "busy") {
		t.Fatalf("expect busy, got %v", serr)
	}
	if serr := m.Suspend(ctx, "t-ix"); serr != nil {
		t.Fatalf("sequential suspend after takeover: %v", serr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, rerr := tstore.GetTask(ctx, "t-ix")
		if rerr == nil && r.Status == "suspended" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final status=%s err=%v（Suspend 完成后收敛 suspended）", r.Status, rerr)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if m.getRuntime("t-ix") != nil {
		t.Fatalf("runtime must be cleaned by suspend（无复活关联）")
	}
	if atomic.LoadInt32(&sup.releaseN) != 1 {
		t.Fatalf("releases=%d, want 1", atomic.LoadInt32(&sup.releaseN))
	}
}

// TestStage4_I2_SuspendStopIntent_ReplayConverges 落实 I2 停止收敛意图重放：
// 停止已确认+CAS 写失败（父 ctx 已取消）→ 意图落库；DB 恢复后后台重放持任务
// 锁复核 → 清关联+active→suspended → 意图清除；旧意图在新一代运行下仅清除
// 不触达。
func TestStage4_I2_SuspendStopIntent_ReplayConverges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-si")
	adapter := NewStoreAdapter(db)
	tstore := &failCondWriteStore{StoreAdapter: adapter, cancelParent: cancel}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, tstore, prov)
	rt := m.newRuntime("t-si")
	m.setRuntime("t-si", rt)
	tstore.failCond.Store(true) // 注入收敛写失败（并取消父 ctx）
	// TUI 托管单元在位（重放清理的可观测对象——killTaskSessions 仅清存活单元）。
	mproc := m.proc.(*mockProc)
	mproc.mu.Lock()
	mproc.sessions["ocdeck-v2-tui-t-si"] = true
	mproc.mu.Unlock()

	// 停止确认成功 + 收敛写失败（failCond 取消父 ctx）。
	if serr := m.Suspend(ctx, "t-si"); serr == nil {
		t.Fatalf("CAS failure must surface（停止已确认、收敛失败）")
	}
	row, err := adapter.GetTask(context.Background(), "t-si")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "active" {
		t.Fatalf("status=%s（收敛失败保持 active）", row.Status)
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatal(perr)
	}
	found := false
	for _, e := range entries {
		if e.Code == noticeCodeSuspendStop {
			found = true
		}
	}
	if !found {
		t.Fatalf("suspend stop intent must be durable: %q", row.Notice.String)
	}

	// DB 恢复 → 后台重放：持锁复核（token 匹配）→ 清关联+收敛 suspended → 删意图。
	tstore.failCond.Store(false)
	row2, err := adapter.GetTask(context.Background(), "t-si")
	if err != nil {
		t.Fatal(err)
	}
	entries2, _ := parseNotices(row2.Notice)
	if rerr := m.retryTaskNotices(context.Background(), row2, entries2); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		row3, rerr := adapter.GetTask(context.Background(), "t-si")
		if rerr == nil && row3.Status == "suspended" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status=%s err=%v（重放收敛 suspended）", row3.Status, rerr)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if m.getRuntime("t-si") != nil {
		t.Fatalf("runtime must be cleared by replay")
	}
	proc := m.proc.(*mockProc)
	proc.mu.Lock()
	killed := append([]string(nil), proc.killOrder...)
	proc.mu.Unlock()
	ref := "ocdeck-v2-tui-t-si"
	hit := false
	for _, k := range killed {
		if k == ref {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("replay must reclaim v2 TUI unit %s, killed=%v", ref, killed)
	}
	row4, _ := adapter.GetTask(context.Background(), "t-si")
	if strings.Contains(row4.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after convergence: %q", row4.Notice.String)
	}
}

// TestStage4_I2_SuspendStopIntent_StaleClearedOnNewGeneration 落实旧意图不影响
// 新一代运行：intent 记录旧执行代际 → 新一代 runtime 注册 → 重放仅清除意图，
// 不触达新代（状态保持 active、runtime 保留）。
func TestStage4_I2_SuspendStopIntent_StaleClearedOnNewGeneration(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-si2")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	ref := "ocdeck-v2-tui-t-si2"
	if ierr := m.recordSuspendStopIntent(ctx, "t-si2", ref, "stale-token"); ierr != nil {
		t.Fatal(ierr)
	}
	m.setRuntime("t-si2", m.newRuntime("t-si2")) // 新一代执行体
	row, _ := adapter.GetTask(ctx, "t-si2")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr != nil {
		t.Fatalf("replay(new gen): %v", rerr)
	}
	row2, _ := adapter.GetTask(ctx, "t-si2")
	if row2.Status != "active" {
		t.Fatalf("stale intent must not converge new generation, status=%s", row2.Status)
	}
	if strings.Contains(row2.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("stale intent must be cleared: %q", row2.Notice.String)
	}
	if m.getRuntime("t-si2") == nil {
		t.Fatalf("new generation runtime must be untouched")
	}
	if atomic.LoadInt32(&sup.releaseN) != 0 {
		t.Fatalf("stale intent must not touch new generation, releases=%d", atomic.LoadInt32(&sup.releaseN))
	}
}

// realStreamHarness 组装真实 opencode2 provider（真实 spiEventStream/SSE 订阅、
// 真实归属校验/端点解析）+ 端口钉扎上游（proc fake 真实监听，与生产一致——端口
// 归 serve 进程所有）。
type realStreamHarness struct {
	db      *store.DB
	adapter *StoreAdapter
	m       *Manager
	proc    *monitorProcSeam
	prov    *opencode2.SPIProvider
	conns   atomic.Int32
	evtDone chan struct{}
	port    int
	// allowConns 允许的 /api/event 连接数（默认 1——重订阅即 500 红灯）。
	allowConns int32
	// created 记录首建 POST 创建的 session 数（Round收尾 J6）。
	created atomic.Int32
	// dropConn 非 nil 时：首条连接写出 hello 后等待该 chan 关闭再返回（模拟
	// 服务端主动断连——触发客户端重连）。
	dropConn chan struct{}
	// conn2Hello 非 nil 时：第二条连接等待该 chan 关闭后才写 hello（模拟「断线
	// 期间接管消费者已安装、上游尚未恢复」的可控时序——Round5 I10）。
	conn2Hello chan struct{}
	// oc 是 Manager OCFactory 实际返回的内存 mock 上游（startSSE 对齐经
	// readyOC{inner: oc} 读 ListSessions——真实流 harness 的 HTTP 面只服务
	// supervisor/事件通道，会话列表事实在此注入）。
	oc *mockOC
	// listPageBody 非空时 GET /api/session 返回该原文（K2：畸形列表页注入）。
	listPageBody string
	// createFault 非 nil 时接管 POST /api/session 的响应（K1：服务端已创建
	// session 但连接在响应前断开/响应截断）。返回 true = 已自行写响应。
	createFault func(w http.ResponseWriter, r *http.Request, createdID string) bool
	// v1Hits 记录 v1 wire 形态路径命中数（生产装配形态测试：v2 全链路 MUST 零
	// 命中——命中即被 SPA HTML 回退吞掉的形态，与容器实测一致）。
	v1Hits atomic.Int32
	// apiMu/apiPaths 记录 /api/* 命中（method+path——生产装配形态测试的正断言
	// 面：桥接 SPI 窄接口必须真实打到预期端点，而非仅「零 v1 命中」负断言）。
	apiMu    sync.Mutex
	apiPaths []string
	// deleteStatus 按 id 覆写 DELETE /api/session/{id} 的响应码（0=默认 204；
	// 删除幂等混合测试注入 404/500）。
	deleteStatus map[string]int
}

// apiHit 报告是否发生过 method+pathPrefix 形态的 /api 命中。
func (h *realStreamHarness) apiHit(method, pathPrefix string) bool {
	h.apiMu.Lock()
	defer h.apiMu.Unlock()
	for _, p := range h.apiPaths {
		if p == method+" "+pathPrefix || strings.HasPrefix(p, method+" "+pathPrefix) {
			return true
		}
	}
	return false
}

// v1ShapedPath 判定 v1 wire client 的 HTTP 路径形态（v1 client GET/POST /session、
// GET /event、GET /session/status、GET /global/health——均无 /api 前缀）。
func v1ShapedPath(p string) bool {
	switch p {
	case "/session", "/event", "/session/status", "/global/health":
		return true
	}
	return strings.HasPrefix(p, "/session/")
}

// harnessOpt 在 harness 构造期调整装配（必须在 Manager 构造前生效的注入点）。
type harnessOpt func(*realStreamHarness)

func newRealStreamHarness(t *testing.T, taskID, upstream, status string, opts ...harnessOpt) *realStreamHarness {
	t.Helper()
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &realStreamHarness{db: db, evtDone: make(chan struct{}), allowConns: 1}
	for _, opt := range opts {
		opt(h)
	}
	var evtOnce sync.Once
	h.proc = newMonitorProcSeam(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// v1 wire 形态路径（无 /api 前缀）：计数 + 回 SPA HTML（模拟真实 v2 serve
		// 对未匹配路径的 SPA 回退——v1 client 解码必失败，与容器实测一致）。
		if v1ShapedPath(r.URL.Path) {
			h.v1Hits.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><body>opencode</body></html>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.apiMu.Lock()
			h.apiPaths = append(h.apiPaths, r.Method+" "+r.URL.Path)
			h.apiMu.Unlock()
		}
		switch {
		case r.URL.Path == "/openapi.json":
			_, _ = w.Write([]byte(`{"components":{"schemas":{"Session.ID":{"type":"string","pattern":"^ses"},"Session.Message.ID":{"type":"string","pattern":"^msg_"}}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			// 首建（Round收尾 J6）：递增 id 的 session 创建。
			n := h.created.Add(1)
			id := fmt.Sprintf("ses_c%d", n)
			if h.createFault != nil && h.createFault(w, r, id) {
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + id + `","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/"):
			// 删除端点（CONTRACT §2）：默认 204；deleteStatus 按 id 覆写
			// （404=已不存在 / 500=服务端故障——幂等混合矩阵）。
			id := strings.TrimPrefix(r.URL.Path, "/api/session/")
			switch h.deleteStatus[id] {
			case 0:
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(h.deleteStatus[id])
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			// 首建（Round收尾 J6）：递增 id 的 session 创建。
			n := h.created.Add(1)
			id := fmt.Sprintf("ses_c%d", n)
			if h.createFault != nil && h.createFault(w, r, id) {
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + id + `","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/environment"):
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/event":
			if h.conns.Add(1) > h.allowConns {
				// 接管 MUST 复用候选流——超限订阅即失败信号（重订阅即红）。
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if h.conns.Load() == 2 && h.conn2Hello != nil {
				<-h.conn2Hello // Round5 I10：conn2 建立但尚未握手（断线期间窗口）
			}
			_, _ = w.Write([]byte("data: {\"id\":\"h\",\"created\":1,\"type\":\"server.connected\",\"data\":{\"clientID\":\"cli_x\"}}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if h.conns.Load() == 1 && h.dropConn != nil {
				<-h.dropConn // 服务端主动断连（触发客户端重连）
			} else {
				<-r.Context().Done() // 客户端断开（Close）→ return 产生 EOF
			}
			evtOnce.Do(func() { close(h.evtDone) })
		case strings.HasPrefix(r.URL.Path, "/api/session/ses_"):
			id := strings.TrimPrefix(r.URL.Path, "/api/session/")
			_, _ = w.Write([]byte(`{"data":{"id":"` + id + `","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
		case h.listPageBody != "":
			_, _ = w.Write([]byte(h.listPageBody))
		default:
			_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
		}
	}))
	h.port = h.proc.port

	// 种子 v2 binding 任务（binding data 端口=实际上游端口）。
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES (?, 'proj', '/repo', 'main', ?, 'repo')`, "p-"+taskID, now); err != nil {
		t.Fatal(err)
	}
	raw, err := domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  upstream,
		TerminalRef:        "ocdeck-v2-tui-" + taskID,
		Data:               []byte(fmt.Sprintf(`{"port":%d,"directory":"/wt","taskId":"%s","origin":"created","auth":{"kind":"derived"}}`, h.port, taskID)),
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
		 VALUES (?,?,'task','b',?,'/wt',?,?,?,?,?)`,
		taskID, "p-"+taskID, status, ocdeckstore.ProviderIDV2, raw, snap, now, now); err != nil {
		t.Fatal(err)
	}

	h.adapter = NewStoreAdapter(db)
	h.oc = newMockOC(true)
	h.m = newTestManagerFull(t, h.adapter, nil, newMockProc(), newMockWorktree(), h.oc)
	h.m.SetLifecycleCtx(ctx)
	h.m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	var identityAlive atomic.Bool
	identityAlive.Store(true)
	deps := &opencode2.SupervisorDeps{
		Proc:         h.proc,
		Store:        &monitorRecordSeam{},
		Env:          NewV2TaskEnvSeam(h.m),
		AllocatePort: func(int, bool, int) (int, error) { return h.port, nil },
		ProcessIdentity: func(pid int) (string, bool, error) {
			if identityAlive.Load() && pid == 4242 {
				return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", h.port), true, nil
			}
			return "", false, nil
		},
		MonitorInterval: 50 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		ReadyTimeout:    500 * time.Millisecond,
		StartAttempts:   2,
	}
	h.proc.onSpawn = func() { identityAlive.Store(true) }
	h.proc.onDeath = func() { identityAlive.Store(false) }
	h.prov = opencode2.NewSPIProviderWithDeps(opencode2.ProviderOptions{StatusVerified: true}, deps)
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(h.prov); err != nil {
		t.Fatal(err)
	}
	h.m.providerRegistry = reg
	t.Cleanup(func() {
		sup := h.prov.Supervisor()
		if ctl, ok := sup.(appagent.ServerMonitorController); ok {
			ctl.StopServerMonitoring()
		}
		if ss, ok := sup.(interface{ ShutdownServer(context.Context) error }); ok {
			_ = ss.ShutdownServer(ctx)
		}
	})
	return h
}

// TestStage4_I10_RealStream_HandoverReplaysReady_ExistingBindingActivate 落实
// I10 真实流握手移交：真实 spiEventStream 握手消费 connected 后，正式接管经
// SetCallbacks 重放立即获得 ready（不二次等 15s 预算、不重订阅——第二次 /api/event
// 连接即 500 红灯）。公开 Activate 入口（已有 binding→resumeActivateV2）。
func TestStage4_I10_RealStream_HandoverReplaysReady_ExistingBindingActivate(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-hs", "ses_keep", "suspended")

	start := time.Now()
	if err := h.m.Activate(ctx, "t-hs"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("ready handover must not wait full ready timeout, elapsed=%v", elapsed)
	}
	if n := h.conns.Load(); n != 1 {
		t.Fatalf("takeover MUST reuse candidate stream, event connections=%d", n)
	}
	got, has, err := h.adapter.GetTaskBinding(ctx, "t-hs")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding continuity: %+v has=%v err=%v", got, has, err)
	}
	row, err := h.adapter.GetTask(ctx, "t-hs")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
	if h.m.getRuntime("t-hs") == nil {
		t.Fatalf("runtime must be registered（正式接管完成）")
	}
}

// TestStage4_I10_RealStream_HandoverReplaysReady_RecoveryEntry 落实 I10 恢复
// 入口（v2Path）同一移交语义：ensureRecovery→Resume→候选握手（真实流消费
// connected）→CommitV2Resume→接管复用候选流重放 ready——不重订阅、不二次等
// ready 预算。
func TestStage4_I10_RealStream_HandoverReplaysReady_RecoveryEntry(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-hr", "ses_keep", "active")
	h.m.setRuntime("t-hr", h.m.newRuntime("t-hr"))

	h.m.RebuildV2Associations() // 通知体：异步 ensureRecovery

	// 完成信号=候选握手连接（真实流）出现；随后收敛收尾（提交+接管）。
	deadline := time.Now().Add(10 * time.Second)
	for h.conns.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("recovery handshake never reached the real event stream")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // 提交+接管收尾
	if n := h.conns.Load(); n != 1 {
		t.Fatalf("recovery takeover MUST reuse candidate stream, event connections=%d", n)
	}
	got, has, err := h.adapter.GetTaskBinding(ctx, "t-hr")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding continuity: %+v has=%v err=%v", got, has, err)
	}
	row, err := h.adapter.GetTask(ctx, "t-hr")
	if err != nil || row.Status != "active" {
		t.Fatalf("status: %s %v", row.Status, err)
	}
}

// TestStage4_I16_RejectTakeover_ClosesCandidateStream 落实 I16 候选流所有权：
// 接管守卫拒绝（任务已离开 active）→ 候选流立即关闭（事件通道闭合+服务端观测
// 连接取消）——不留存续到 Manager 关停的订阅面。
func TestStage4_I16_RejectTakeover_ClosesCandidateStream(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-cg", "ses_keep", "suspended")

	binding, has, err := h.adapter.GetTaskBinding(ctx, "t-cg")
	if err != nil || !has {
		t.Fatalf("binding: %v", err)
	}
	warmRealStreamHarness(t, h)
	es, serr := h.prov.Events().Subscribe(ctx, binding)
	if serr != nil {
		t.Fatalf("Subscribe: %v", serr)
	}
	connected := make(chan struct{})
	es.SetCallbacks(func() { close(connected) }, nil)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("handshake connected never fired (event conns=%d)", h.conns.Load())
	}

	row, err := h.adapter.GetTask(ctx, "t-cg")
	if err != nil {
		t.Fatal(err)
	}
	// 守卫拒绝（suspended ≠ active）→ postCommit 分类 + 候选流立即关闭。
	terr := h.m.commitV2RuntimeReady(ctx, "t-cg", row, h.port, "pw", binding, AlignModeRepo, es, true)
	var pce *postCommitTakeoverError
	if !errors.As(terr, &pce) {
		t.Fatalf("guard rejection must be post-commit classified, got %v", terr)
	}
	select {
	case _, ok := <-es.Events():
		if ok {
			t.Fatalf("candidate stream must be closed (no further events)")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("candidate stream was not closed after rejection")
	}
	select {
	case <-h.evtDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("server must observe candidate stream disconnect")
	}
}

// failReplayPersistStore 是 Round5 I17 的持久化失败注入：分别独立失败
// UpdateTaskNoticeCAS（债务持久化）与 UpdateTaskEnvSnapshot（env 快照清除）。
type failReplayPersistStore struct {
	*StoreAdapter
	failNoticeCAS atomic.Bool
	// failNextNoticeCAS 计数失败：仅接下来 N 次 notice CAS 写失败（让携带
	// pending 标注的意图写回成功、仅债务落库失败的精确注入）。
	failNextNoticeCAS atomic.Int32
	failEnvClear      atomic.Bool
}

func (s *failReplayPersistStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	if s.failNoticeCAS.Load() {
		return application.MutationResult{}, errors.New("notice cas injected failure")
	}
	if n := s.failNextNoticeCAS.Load(); n > 0 {
		s.failNextNoticeCAS.Add(-1)
		return application.MutationResult{}, errors.New("notice cas injected failure (counted)")
	}
	return s.StoreAdapter.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

func (s *failReplayPersistStore) UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) (application.MutationResult, error) {
	if s.failEnvClear.Load() {
		return application.MutationResult{}, errors.New("env clear injected failure")
	}
	return s.StoreAdapter.UpdateTaskEnvSnapshot(ctx, id, envSnapshot)
}

// TestStage4_R5_I17_ReplayKeepsIntentOnDebtPersistFail 落实 Round5 I17（债务
// 持久化失败）：收敛写成功但残留 notice（含完整 tickets）落库失败 → 意图不清除、
// 改写为 pending 形态（tickets 随意图保留）；下一轮补全后意图才清除。
func TestStage4_R5_I17_ReplayKeepsIntentOnDebtPersistFail(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-dp")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	// 义务持久化走 writeNoticeCAS 的 lifecycle 出口——测试置空回退 store 注入
	//（failReplayPersistStore）。
	m.lifecycle = nil
	rt := m.newRuntime("t-dp")
	m.setRuntime("t-dp", rt)
	mproc := m.proc.(*mockProc)
	mproc.mu.Lock()
	mproc.sessions["ocdeck-v2-tui-t-dp"] = true
	mproc.killResults["ocdeck-v2-tui-t-dp"] = process.KillResult{
		SessionKilled:  false,
		Disposition:    process.DispositionKillFailed,
		CleanupTickets: []string{"tk-r5-1"},
	}
	mproc.mu.Unlock()

	ref := "ocdeck-v2-tui-t-dp"
	if ierr := m.recordSuspendStopIntent(ctx, "t-dp", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	// Round6 I17（持续失败注入）：覆盖 recordResidualNotice 全部 CAS 重试（8 次）
	// + pending 标注写回（第 9 次）——债务落库与义务标注均失败 → MUST NOT 收敛
	// 状态（保持 active：tickets 由 kill 结果下一轮重新派生，零丢失）。
	fstore.failNoticeCAS.Store(true)
	row, _ := fstore.GetTask(ctx, "t-dp")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr == nil {
		t.Fatalf("debt persistence failure must surface (done=false)")
	}
	row2, _ := fstore.GetTask(ctx, "t-dp")
	if row2.Status != "active" {
		t.Fatalf("status=%s（义务标注未落库 MUST NOT 收敛状态）", row2.Status)
	}
	if !strings.Contains(row2.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be kept: %q", row2.Notice.String)
	}
	if strings.Contains(row2.Notice.String, `"code":"residual_processes"`) {
		t.Fatalf("debt notice must NOT be persisted while persistence fails: %q", row2.Notice.String)
	}

	// 恢复 → 重放：债务 notice 落库 → 收敛 suspended → 意图清除（完整 tickets
	// 可继续重放——not 仅断言错误返回）。
	fstore.failNoticeCAS.Store(false)
	row3, _ := fstore.GetTask(ctx, "t-dp")
	entries3, _ := parseNotices(row3.Notice)
	if rerr := m.retryTaskNotices(ctx, row3, entries3); rerr != nil {
		t.Fatalf("replay(restore): %v", rerr)
	}
	row4, _ := fstore.GetTask(ctx, "t-dp")
	if row4.Status != "suspended" {
		t.Fatalf("status=%s（恢复后收敛 suspended）", row4.Status)
	}
	if !strings.Contains(row4.Notice.String, "tk-r5-1") {
		t.Fatalf("tickets debt must be persisted after recovery: %q", row4.Notice.String)
	}
	if strings.Contains(row4.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after converged replay: %q", row4.Notice.String)
	}
}

// TestStage4_R5_I17_ReplayKeepsIntentOnEnvClearFail 落实 Round5 I17（env 快照
// 清除失败）：收敛写成功但 env 清除失败 → 意图不清除（pending 形态保留）、快照
// 不得残留被当作已完成；下一轮补全后意图清除。
func TestStage4_R5_I17_ReplayKeepsIntentOnEnvClearFail(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-ev")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	rt := m.newRuntime("t-ev")
	m.setRuntime("t-ev", rt)

	ref := "ocdeck-v2-tui-t-ev"
	if ierr := m.recordSuspendStopIntent(ctx, "t-ev", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	fstore.failEnvClear.Store(true)
	row, _ := fstore.GetTask(ctx, "t-ev")
	entries, _ := parseNotices(row.Notice)
	// Round6 I17：义务标注先于收敛落库（notice 通道健康——标注成功）→ 收敛
	// suspended + 意图携带 env 义务。重放返回值携带义务错误（允许）——以持久化
	// 状态断言为准。
	_ = m.retryTaskNotices(ctx, row, entries)
	row2, _ := fstore.GetTask(ctx, "t-ev")
	if row2.Status != "suspended" {
		t.Fatalf("status=%s", row2.Status)
	}
	if !strings.Contains(row2.Notice.String, suspendFinishPendingKey) {
		t.Fatalf("intent must be kept with env obligation: %q", row2.Notice.String)
	}
	var snap sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-ev'`).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || snap.String == "" {
		t.Fatalf("env snapshot must remain while clear fails (not treated as done)")
	}

	fstore.failEnvClear.Store(false)
	row3, _ := fstore.GetTask(ctx, "t-ev")
	entries3, _ := parseNotices(row3.Notice)
	if rerr := m.retryTaskNotices(ctx, row3, entries3); rerr != nil {
		t.Fatalf("replay(restore): %v", rerr)
	}
	row4, _ := fstore.GetTask(ctx, "t-ev")
	if strings.Contains(row4.Notice.String, suspendFinishPendingKey) {
		t.Fatalf("pending marker must be cleared after env obligation completes: %q", row4.Notice.String)
	}
	var snap2 sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-ev'`).Scan(&snap2); err != nil {
		t.Fatal(err)
	}
	if snap2.Valid && snap2.String != "" {
		t.Fatalf("env snapshot must be cleared after obligation completion, got %q", snap2.String)
	}
}

// TestStage4_R7_I17_ReapTicketsSurviveViaLedger 落实 Round7 I17：kill 返回
// SessionKilled=true + ReapFailed + 原始 tickets（tmux 单元已删、逃逸进程待收割）
// 且残留 notice/pending 标注/外层写回持续失败 → 义务进受管登记（单元消失后无法
// 重新派生 tickets）→ 存储恢复后重放：按原 tickets 内容落账（收割目标=原
// tickets，非重新派生）→ 收敛 suspended + 意图清除。
func TestStage4_R7_I17_ReapTicketsSurviveViaLedger(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-r7")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	rt := m.newRuntime("t-r7")
	m.setRuntime("t-r7", rt)
	mproc := m.proc.(*mockProc)
	const ref = "ocdeck-v2-tui-t-r7"
	mproc.mu.Lock()
	mproc.sessions[ref] = true
	// SessionKilled=true + ReapFailed：单元已删、逃逸进程待收割——tickets 无法
	// 通过再次 kill 重新派生（单元消失后 HasSession 直接跳过）。
	mproc.killResults[ref] = process.KillResult{
		SessionKilled:  true,
		Disposition:    process.DispositionReapFailed,
		CleanupTickets: []string{"tk-orig-1", "tk-orig-2"},
	}
	mproc.mu.Unlock()

	ref2 := ref
	if ierr := m.recordSuspendStopIntent(ctx, "t-r7", ref2, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	// ①持续写失败：残留 notice / pending 标注 / 外层写回全部失败。
	fstore.failNoticeCAS.Store(true)
	row, _ := fstore.GetTask(ctx, "t-r7")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr == nil {
		t.Fatalf("persistent persistence failure must surface")
	}
	// 单元消失（不可重新派生 tickets）。
	mproc.mu.Lock()
	delete(mproc.sessions, ref)
	mproc.mu.Unlock()
	// 本轮义务必须已进受管登记（进程内有生命周期的重试载荷）。
	if len(m.reapLedgerSnapshot("t-r7")) == 0 {
		t.Fatalf("obligations must be registered in the managed ledger")
	}

	// ②存储恢复 → 重放：义务经受管登记落账并被收割（收割目标=原 tickets 内容，
	// 非重新派生——单元已删、无可再次 kill）→ 收敛 suspended → 意图清除。
	rproc := &reapRecordingProc{mockProc: mproc}
	m.proc = rproc
	fstore.failNoticeCAS.Store(false)
	row2, _ := fstore.GetTask(ctx, "t-r7")
	entries2, _ := parseNotices(row2.Notice)
	if rerr := m.retryTaskNotices(ctx, row2, entries2); rerr != nil {
		t.Fatalf("replay(restore): %v", rerr)
	}
	row3, _ := fstore.GetTask(ctx, "t-r7")
	if row3.Status != "suspended" {
		t.Fatalf("status=%s", row3.Status)
	}
	rproc.mu.Lock()
	reaped := append([][]string(nil), rproc.reaped...)
	rproc.mu.Unlock()
	if len(reaped) == 0 {
		t.Fatalf("original tickets must be reaped via ledger-drain")
	}
	for _, tk := range reaped {
		if len(tk) != 2 || tk[0] != "tk-orig-1" || tk[1] != "tk-orig-2" {
			t.Fatalf("reap target must be the ORIGINAL tickets, got %v", tk)
		}
	}
	if strings.Contains(row3.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after converged replay: %q", row3.Notice.String)
	}
	if len(m.reapLedgerSnapshot("t-r7")) != 0 {
		t.Fatalf("ledger must be drained after persistence recovers")
	}
}

// reapRecordingProc 包装 mockProc 录制 RetryReap 的收割目标（Round7 I17：断言
// 收割目标=原 tickets 内容）。
type reapRecordingProc struct {
	*mockProc
	mu     sync.Mutex
	reaped [][]string
}

func (p *reapRecordingProc) RetryReap(tickets []string) ([]string, error) {
	p.mu.Lock()
	p.reaped = append(p.reaped, append([]string(nil), tickets...))
	p.mu.Unlock()
	return nil, nil
}

// TestStage4_R6_I19_SuspendCompletesPendingOrKeepsIt 落实 Round6 I19：后续
// Suspend 取得收敛权清除意图前 MUST 补全意图携带的未完成持久化义务——补全成功
// → tickets 债务落库 + 意图清除；补全失败 → 意图保留（pending 不丢），下轮
// 重放补全。
func TestStage4_R6_I19_SuspendCompletesPendingOrKeepsIt(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-pk")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	rt := m.newRuntime("t-pk")
	m.setRuntime("t-pk", rt)
	ref := "ocdeck-v2-tui-t-pk"

	// 意图携带未完成 notice 义务（tk-r6-1）。
	if ierr := m.recordSuspendStopIntent(ctx, "t-pk", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row, _ := fstore.GetTask(ctx, "t-pk")
	entries, _ := parseNotices(row.Notice)
	annotated := annotateSuspendStopPending(entries[0], []suspendFinishObligation{{
		Kind: "notice", Name: ref, Tickets: []string{"tk-r6-1"}, Reason: "kill_failed", Retryable: true,
	}})
	if _, werr := m.writeNoticeCAS(ctx, "t-pk", row.Notice, encodeNotices([]noticeEntry{annotated})); werr != nil {
		t.Fatal(werr)
	}

	// 成功路径：Suspend 清除意图前补全义务（tickets 债务落库）。
	if serr := m.Suspend(ctx, "t-pk"); serr != nil {
		t.Fatalf("suspend: %v", serr)
	}
	row2, _ := fstore.GetTask(ctx, "t-pk")
	if row2.Status != "suspended" {
		t.Fatalf("status=%s", row2.Status)
	}
	if !strings.Contains(row2.Notice.String, "tk-r6-1") {
		t.Fatalf("pending obligation must be completed before intent removal: %q", row2.Notice.String)
	}
	if strings.Contains(row2.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after obligations complete: %q", row2.Notice.String)
	}

	// 失败路径：义务补全持续写失败（failNoticeCAS——覆盖义务补全与意图写回的
	// 全部 notice 写）→ 意图保留（pending+tickets 不丢）；Suspend 本身完成。
	seedV2ActiveTask(t, db, "t-pk2")
	rt2 := m.newRuntime("t-pk2")
	m.setRuntime("t-pk2", rt2)
	ref2 := "ocdeck-v2-tui-t-pk2"
	if ierr := m.recordSuspendStopIntent(ctx, "t-pk2", ref2, string(rt2.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row3, _ := fstore.GetTask(ctx, "t-pk2")
	entries3, _ := parseNotices(row3.Notice)
	annotated2 := annotateSuspendStopPending(entries3[0], []suspendFinishObligation{{
		Kind: "notice", Name: ref2, Tickets: []string{"tk-r6-2"}, Reason: "kill_failed", Retryable: true,
	}})
	if _, werr := m.writeNoticeCAS(ctx, "t-pk2", row3.Notice, encodeNotices([]noticeEntry{annotated2})); werr != nil {
		t.Fatal(werr)
	}
	fstore.failNoticeCAS.Store(true)
	if serr := m.Suspend(ctx, "t-pk2"); serr != nil {
		t.Fatalf("suspend(fail path): %v", serr)
	}
	row4, _ := fstore.GetTask(ctx, "t-pk2")
	if row4.Status != "suspended" {
		t.Fatalf("status=%s", row4.Status)
	}
	// 意图（含 pending+完整 tickets）MUST 保留——补全失败不得删除或降级为文本。
	if !strings.Contains(row4.Notice.String, noticeCodeSuspendStop) ||
		!strings.Contains(row4.Notice.String, suspendFinishPendingKey) ||
		!strings.Contains(row4.Notice.String, "tk-r6-2") {
		t.Fatalf("intent with pending obligations must be preserved on persist failure: %q", row4.Notice.String)
	}
	if strings.Contains(row4.Notice.String, `"code":"residual_processes"`) {
		t.Fatalf("obligation must not masquerade as persisted debt: %q", row4.Notice.String)
	}
	// 恢复 → 重放：义务补全（tickets 债务落库）→ 意图清除。
	fstore.failNoticeCAS.Store(false)
	row5, _ := fstore.GetTask(ctx, "t-pk2")
	entries5, _ := parseNotices(row5.Notice)
	if rerr := m.retryTaskNotices(ctx, row5, entries5); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	row6, _ := fstore.GetTask(ctx, "t-pk2")
	if strings.Contains(row6.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after replay completes obligations: %q", row6.Notice.String)
	}
	if !strings.Contains(row6.Notice.String, "tk-r6-2") {
		t.Fatalf("tickets must be persisted after replay completion: %q", row6.Notice.String)
	}
}

// ---- 收尾 Round1：J3 candidate 清理义务按 task 持久化 + 首建准入门禁 ----

// debtCandidateSup 是 J3 测试的 fake supervisor：Acquire 恒失败且错误链携带
// CandidateCleanupDebt（配置应用失败+DELETE 失败的编排后形态）；实现
// CandidateDebtReaper 录制收割目标。ReapCandidateSession 仅对真实 candidate id
// 返回成功——任意 ID 均成功的 fake 会掩盖 J8「占位符被当作可收割身份」缺陷。
type debtCandidateSup struct {
	mu           sync.Mutex
	acquireCalls int32
	reaped       []string
	// realCandidate 非空时：收割目标不匹配即返回 NotFound（模拟真实上游 DELETE
	// 只对真实 session id 生效，未知 id 404）。
	realCandidate string
	// acquireHook 非 nil 时在 Acquire 进入后调用（取消/故障注入等时序控制）。
	acquireHook func()
}

func (s *debtCandidateSup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	atomic.AddInt32(&s.acquireCalls, 1)
	if h := s.acquireHook; h != nil {
		h()
	}
	cfgErr := appagent.NewError("supervisor.env", appagent.ErrKindRuntimeUnavailable,
		errors.New("env snapshot unavailable"))
	debt := &appagent.CandidateCleanupDebt{SessionID: "ses_debt_1",
		Err: errors.New("delete ses_debt_1: 500")}
	return domain.TaskBinding{}, errors.Join(cfgErr, debt)
}

func (s *debtCandidateSup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.resume", appagent.ErrKindNotFound,
		errors.New("no upstream session (B first-build path)"))
}

func (s *debtCandidateSup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (s *debtCandidateSup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

func (s *debtCandidateSup) ReapCandidateSession(ctx context.Context, taskID, sessionID string) error {
	s.mu.Lock()
	s.reaped = append(s.reaped, sessionID)
	real := s.realCandidate
	s.mu.Unlock()
	// J8：只有真实 candidate id 可被收割；占位/未知 id 模拟真实上游 404
	//（J7 收敛只对真实 ID 的清理窄路径成立，测试 fake 不得对任意 ID 返回成功）。
	if real != "" && sessionID != real {
		return fmt.Errorf("reap candidate %s: session not found", sessionID)
	}
	return nil
}

func (s *debtCandidateSup) reapedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reaped...)
}

// debtProvider 包装 debtCandidateSup（归属校验/端点/Events nil——首建握手回退
// ocFactory 形态）。
type debtProvider struct {
	v2Stub
	sup *debtCandidateSup
}

func (p *debtProvider) Supervisor() appagent.RuntimeSupervisor { return p.sup }

func (p *debtProvider) CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error {
	return nil
}

func (p *debtProvider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 4096, "derived", "/wt", nil
}

// TestR1_J3_CandidateDebt_GatesFirstBuild 落实收尾 Round1 J3：首建 Acquire 失败
// 且错误链携带 CandidateCleanupDebt → 按 task 落 retryable 残债（reason=
// candidate_cleanup_failed）→ 再次 Activate 被首建准入门禁拒绝（零 POST）→
// reaper 可用后重放收割原 session → 残债清除 → Activate 可再次首建。
func TestR1_J3_CandidateDebt_GatesFirstBuild(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-cd")
	adapter := NewStoreAdapter(db)
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// 首次 Activate：Acquire 失败 + 清理义务落残债。
	if aerr := m.Activate(ctx, "t-cd"); aerr == nil {
		t.Fatalf("first build must fail on injected config+delete errors")
	}
	row, err := adapter.GetTask(ctx, "t-cd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Notice.String, "candidate_cleanup_failed") ||
		!strings.Contains(row.Notice.String, "ses_debt_1") {
		t.Fatalf("cleanup debt must be persisted per task: %q", row.Notice.String)
	}
	// 残债在位：再次 Activate 被首建准入门禁拒绝（零 POST——不误判零历史）。
	if aerr := m.Activate(ctx, "t-cd"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("second Activate must be gated by cleanup debt, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("gated Activate must not POST again, acquires=%d", calls)
	}

	// reaper 就绪 → 后台重放收割原 session → 残债清除 → Activate 可再次首建。
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	reaped := sup.reapedSnapshot()
	if len(reaped) != 1 || reaped[0] != "ses_debt_1" {
		t.Fatalf("reaper target must be the original candidate session, got %v", reaped)
	}
	row2, _ := adapter.GetTask(ctx, "t-cd")
	if strings.Contains(row2.Notice.String, "candidate_cleanup_failed") {
		t.Fatalf("residual debt must be cleared after reap: %q", row2.Notice.String)
	}
	if aerr := m.Activate(ctx, "t-cd"); aerr == nil {
		t.Fatalf("Activate after reap expected to fail later in the chain (fake sup), but gate must be open; aerr=%v", aerr)
	} else if strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("gate must be open after reap, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 2 {
		t.Fatalf("ungated Activate must attempt Acquire once more, acquires=%d", calls)
	}
}

// rollbackFailProvider：Acquire 成功返回 candidate、端点解析失败（回滚触发）、
// 回收（CandidateSessionRecycler）恒失败——J3 逃逸路径 2 注入。
type rollbackFailProvider struct {
	v2Stub
}

func (p *rollbackFailProvider) Supervisor() appagent.RuntimeSupervisor {
	return rollbackFailSup{}
}

type rollbackFailSup struct{}

func (rollbackFailSup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	data, _ := json.Marshal(map[string]any{
		"port": 4096, "directory": "/wt", "taskId": t.TaskID,
		"origin": "created", "auth": map[string]string{"kind": "derived"},
	})
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ocdeckstore.ProviderIDV2,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default,
		UpstreamSessionID:  "ses_rb_1",
		TerminalRef:        "ocdeck-v2-tui-" + t.TaskID,
		Data:               data,
	}, nil
}

func (rollbackFailSup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, appagent.NewError("test.resume", appagent.ErrKindNotFound,
		errors.New("no upstream session"))
}

func (rollbackFailSup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (rollbackFailSup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

// RecycleCandidateSession 注入回收失败（J3 逃逸路径 2：回滚触发但 DELETE 失败）。
func (rollbackFailSup) RecycleCandidateSession(ctx context.Context, b domain.TaskBinding) error {
	return errors.New("delete injected failure (rollback recycle)")
}

func (rollbackFailProvider) BindingRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	return 0, "", "", errors.New("endpoint resolution injected failure (rollback trigger)")
}

// TestR1_J3_RollbackFail_GatesFirstBuild 落实收尾 Round2 J3 逃逸路径 2：Acquire
// 成功后端点解析失败触发回滚、回收（DELETE）失败 → 清理义务按 task 持久化
// （candidate_cleanup_failed）→ 门禁拒绝再次首建；reaper 收割原 session 后清除。
func TestR1_J3_RollbackFail_GatesFirstBuild(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-rb")
	adapter := NewStoreAdapter(db)
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&rollbackFailProvider{}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if aerr := m.Activate(ctx, "t-rb"); aerr == nil {
		t.Fatalf("rollback must fail on injected endpoint+delete errors")
	}
	row, err := adapter.GetTask(ctx, "t-rb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Notice.String, "candidate_cleanup_failed") ||
		!strings.Contains(row.Notice.String, "ses_rb_1") {
		t.Fatalf("cleanup debt must be persisted on rollback recycle failure: %q", row.Notice.String)
	}
	if aerr := m.Activate(ctx, "t-rb"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("second Activate must be gated, got %v", aerr)
	}
}

// TestR1_J3_ArmBlocksBeforePOST 落实 Round3 J3：首建阻断意图在 POST 之前持久
// 化——notice 持久化失败 → 零 POST（无 candidate 产生 → 无残留事实可丢失）；
// 恢复后重放正常首建。
func TestR1_J3_ArmBlocksBeforePOST(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-am")
	adapter := NewStoreAdapter(db)
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// notice 持久化失败（阻断意图写不进去）→ Activate 失败且零 POST。
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	m.store = fstore
	fstore.failNoticeCAS.Store(true)
	if aerr := m.Activate(ctx, "t-am"); aerr == nil {
		t.Fatalf("arm persistence failure must block first-build")
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 0 {
		t.Fatalf("blocked first-build must not POST, acquires=%d", calls)
	}
	// 恢复（notice 通道健康）→ 正常首建（门禁解除；改用成功形态 sup 验证
	// arm 意图在首建成功后被清除）。
	fstore.failNoticeCAS.Store(false)
	okSup := &okAcquireSup{upstream: "ses_am_1", port: 4096}
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&supOnlyV2Provider{sup: okSup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg2
	if aerr := m.Activate(ctx, "t-am"); aerr != nil {
		t.Fatalf("Activate(restore): %v", aerr)
	}
	row, _ := adapter.GetTask(ctx, "t-am")
	if row.Status != "active" {
		t.Fatalf("status=%s", row.Status)
	}
	if strings.Contains(row.Notice.String, noticeCodeSuspendStop) ||
		strings.Contains(row.Notice.String, "candidate_cleanup_failed") {
		t.Fatalf("armed intent must be cleared after successful first-build: %q", row.Notice.String)
	}
}

// okAcquireSup 是首建成功形态 fake：Acquire 返回 candidate binding（一次）。
type okAcquireSup struct {
	upstream string
	port     int
	mu       sync.Mutex
	reaped   []string
}

func (s *okAcquireSup) Acquire(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
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

func (s *okAcquireSup) Resume(ctx context.Context, t appagent.TaskRef, b domain.TaskBinding, p appagent.ResumePolicy) (domain.TaskBinding, error) {
	return b, nil
}

func (s *okAcquireSup) AcquireEphemeral(ctx context.Context, t appagent.TaskRef) (domain.TaskBinding, error) {
	return domain.TaskBinding{}, errors.New("unexpected")
}

func (s *okAcquireSup) Release(ctx context.Context, b domain.TaskBinding) error { return nil }

// ReapCandidateSession 记录收割目标：J9 断言「已提交的 session 不作为 candidate
// 收割」（提交证明路径 MUST 零 DELETE）。
func (s *okAcquireSup) ReapCandidateSession(ctx context.Context, taskID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaped = append(s.reaped, sessionID)
	return nil
}

func (s *okAcquireSup) reapedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reaped...)
}

// TestR1_J3_CrossRestartZeroPOST 落实 Round3 J3 跨重启逃逸闭合：debt 持久化
// （notice 含 candidate sessionID）→ 重建 Manager（模拟进程重启，内存 ledger
// 清空）→ Activate 仍被门禁拒绝（零 POST——残留事实不被误判为从未建立）；
// reaper 收割后门禁开放。
func TestR1_J3_CrossRestartZeroPOST(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-xr")
	adapter := NewStoreAdapter(db)
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	if aerr := m.Activate(ctx, "t-xr"); aerr == nil {
		t.Fatalf("first build must fail (debt injected)")
	}

	// 进程重启形态：全新 Manager（内存 ledger/登记全空），同一 DB。
	m2 := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m2.SetLifecycleCtx(ctx)
	m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(prov); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg2
	if aerr := m2.Activate(ctx, "t-xr"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("cross-restart Activate must be gated by persisted debt, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("cross-restart must be zero-POST, acquires=%d", calls)
	}
	// reaper 重放清债 → 门禁开放。
	if berr := m2.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	if aerr := m2.Activate(ctx, "t-xr"); aerr == nil ||
		strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("gate must open after reap, aerr=%v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 2 {
		t.Fatalf("post-reap Activate must attempt Acquire, acquires=%d", calls)
	}
}

// TestR1_J3_CancelAndDebtPersist 落实收尾 Round2/3 J3 逃逸路径 1：Acquire 已
// 进入后请求 ctx 取消且 DELETE 失败 → 占位意图升级为真实残债仍落库（settle 用
// 独立有界 ctx，不随请求取消）→ 状态写成功回退 suspended → 门禁拒绝再次首建；
// reaper 收割后清除开放。
func TestR1_J3_CancelAndDebtPersist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-cn")
	adapter := NewStoreAdapter(db)
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	sup.acquireHook = func() { cancel() } // Acquire 进入后取消（POST 在途形态）
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	if aerr := m.Activate(ctx, "t-cn"); aerr == nil {
		t.Fatalf("cancelled activate must fail")
	}
	// 取消后的 DB 读取一律用 bg（驱动尊重取消 ctx）。
	bg := context.Background()
	row, err := adapter.GetTask(bg, "t-cn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Notice.String, "candidate_cleanup_failed") ||
		!strings.Contains(row.Notice.String, "ses_debt_1") {
		t.Fatalf("debt must persist despite cancellation: %q (full=%v)", row.Notice.String, row.Notice)
	}
	if strings.Contains(row.Notice.String, `"pending"`) {
		t.Fatalf("armed intent must be upgraded to real session id, got %q", row.Notice.String)
	}
	// 状态写成功回退 suspended（补偿 compCtx WithoutCancel）。
	if row.Status != "suspended" {
		t.Fatalf("status must converge suspended despite cancellation, status=%s", row.Status)
	}
	if m.reapLedgerHasUnfinished("t-cn") {
		t.Fatalf("settled debt must not leave managed ledger obligations")
	}
	// 门禁拒绝（健康 ctx——拒绝语义来自残债而非取消态）且零 POST。
	if aerr := m.Activate(bg, "t-cn"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("gated, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("gated retry must not POST, acquires=%d", calls)
	}
	// 重放收割原 session → 清除 → 门禁开放。
	if berr := m.processRetryableNotices(bg); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) == 0 || reaped[0] != "ses_debt_1" {
		t.Fatalf("reaper target=%v", reaped)
	}
	row2, _ := adapter.GetTask(bg, "t-cn")
	if strings.Contains(row2.Notice.String, "candidate_cleanup_failed") {
		t.Fatalf("debt must be cleared after reap: %q", row2.Notice.String)
	}
}

// TestR1_J3_SettleFail_StillGatedAfterRestart 落实 Round3 J3 确定性故障链收口：
// arm 成功（POST 前）→ Acquire 产生 debt → 仅 settle（notice 写）持续失败 →
// 受管 ledger 兜底 → 状态写成功回退 suspended → 重建 Manager（模拟进程重启，
// 内存 ledger 清空）→ DB 仍有 retryable 占位意图 → Activate 门禁拒绝（零 POST）；
// 通道恢复 → 重放清债 → 门禁开放完成首建。
func TestR1_J3_SettleFail_StillGatedAfterRestart(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-sf")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &debtCandidateSup{}
	// Acquire 进入后打开 notice CAS 失败——arm 已成功落库（POST 前），仅 settle 失败。
	sup.acquireHook = func() { fstore.failNoticeCAS.Store(true) }
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.store = fstore
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// arm 成功 → Acquire 失败（debt）→ settle 失败（ledger 兜底）→ 状态写成功回退 suspended。
	if aerr := m.Activate(ctx, "t-sf"); aerr == nil {
		t.Fatalf("first build must fail (debt + settle persist failure)")
	}
	if !m.reapLedgerHasUnfinished("t-sf") {
		t.Fatalf("settle failure must register managed ledger obligation")
	}
	row, err := fstore.GetTask(ctx, "t-sf")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "suspended" {
		t.Fatalf("status write must succeed (fallback suspended), status=%s", row.Status)
	}
	if !strings.Contains(row.Notice.String, `"pending"`) ||
		!strings.Contains(row.Notice.String, "candidate_cleanup_failed") {
		t.Fatalf("armed pre-POST intent must persist across settle failure: %q", row.Notice.String)
	}

	// 进程重启形态：全新 Manager（ledger 清空），同一 DB——占位意图持续阻断，零 POST。
	m2 := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m2.store = fstore
	m2.SetLifecycleCtx(ctx)
	m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(prov); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg2
	if aerr := m2.Activate(ctx, "t-sf"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("cross-restart Activate must stay gated by persisted intent, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("cross-restart must be zero-POST, acquires=%d", calls)
	}

	// notice 通道恢复 → 后台重放 + 启动 Reconcile（debt pre-pass）后 MUST 仍零
	// POST、占位不被当作收割目标——真实 candidate ses_debt_1 的身份只存在于已随
	// 重启丢失的内存 ledger，占位符 DELETE 成功/404 不能证明其已回收（Round4 J8）。
	fstore.failNoticeCAS.Store(false)
	sup.realCandidate = "ses_debt_1" // 只有真实 candidate id 可被收割（未知 id 404）
	if berr := m2.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	if rerr := m2.Reconcile(ctx); rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) != 0 {
		t.Fatalf("identity-unknown placeholder MUST NOT be reaped, reaped=%v", reaped)
	}
	for _, id := range sup.reapedSnapshot() {
		if id == candidateIntentSessionPending {
			t.Fatalf("MUST NOT DELETE placeholder %q (proves nothing about real candidate)", id)
		}
	}
	rowGate, _ := fstore.GetTask(ctx, "t-sf")
	if rowGate.Status != "suspended" {
		t.Fatalf("replay must not open first-build: status=%s", rowGate.Status)
	}
	if !strings.Contains(rowGate.Notice.String, `"pending"`) {
		t.Fatalf("identity-unknown intent must stay persisted (gated): %q", rowGate.Notice.String)
	}
	if aerr := m2.Activate(ctx, "t-sf"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") ||
		!strings.Contains(aerr.Error(), "身份未知") {
		t.Fatalf("gate must persist with manual-resolution guidance, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("post-replay/reconcile Activate must stay zero-POST, acquires=%d", calls)
	}
	row2, _ := fstore.GetTask(ctx, "t-sf")
	if !strings.Contains(row2.LastError.String, "身份未知") {
		t.Fatalf("gated last_error must carry manual-resolution guidance, got %q", row2.LastError.String)
	}

	// 人工处置（清除占位 notice）后门禁才开放——换健康 sup 完成首建。
	clearPendingIntentNotice(t, fstore, "t-sf")
	okSup := &okAcquireSup{upstream: "ses_sf_1", port: 4096}
	reg3 := appagent.NewRegistry()
	if err := reg3.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg3.Register(&supOnlyV2Provider{sup: okSup}); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg3
	if aerr := m2.Activate(ctx, "t-sf"); aerr != nil {
		t.Fatalf("Activate(restore after manual resolution): %v", aerr)
	}
	row3, _ := fstore.GetTask(ctx, "t-sf")
	if row3.Status != "active" {
		t.Fatalf("status=%s", row3.Status)
	}
}

// clearPendingIntentNotice 模拟人工处置：从任务 notice 中移除身份未知的首建
// 意图（candidate_cleanup_failed + sessionName=pending），其余条目保留。
func clearPendingIntentNotice(t *testing.T, db TaskStore, taskID string) {
	t.Helper()
	ctx := context.Background()
	row, err := db.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	entries, perr := parseNotices(row.Notice)
	if perr != nil {
		t.Fatal(perr)
	}
	var remaining []noticeEntry
	for _, e := range entries {
		if e.Code == noticeCodeResidual {
			if sn, _ := e.Data["sessionName"].(string); sn == candidateIntentSessionPending {
				continue
			}
		}
		remaining = append(remaining, e)
	}
	if len(remaining) == len(entries) {
		t.Fatalf("no pending intent notice to clear: %q", row.Notice.String)
	}
	if _, err := db.UpdateTaskNoticeCAS(ctx, taskID, row.Notice, encodeNotices(remaining)); err != nil {
		t.Fatal(err)
	}
}

// ---- Round 4：J8 身份未知的首建意图不是可收割 session ----

// TestR1_J8_PendingIntentNotReaped_KnownIdentityResolves 落实 Round4 J8 正向面：
// 同进程 settle 失败后真实 candidate 身份仍在受管 ledger → 通道恢复重放先落账
// 真实 ID 并收割成功（回收证明）→ 占位意图随之解除、门禁开放。
func TestR1_J8_PendingIntentNotReaped_KnownIdentityResolves(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-j8k")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	sup.acquireHook = func() { fstore.failNoticeCAS.Store(true) }
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.store = fstore
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// arm 成功 → debt 产生 → settle 失败（占位仍在，真实 ID 只在 ledger）。
	if aerr := m.Activate(ctx, "t-j8k"); aerr == nil {
		t.Fatalf("first build must fail")
	}
	if !m.reapLedgerHasUnfinished("t-j8k") {
		t.Fatalf("settle failure must register real candidate identity in ledger")
	}
	row, _ := fstore.GetTask(ctx, "t-j8k")
	if !strings.Contains(row.Notice.String, `"pending"`) {
		t.Fatalf("pre-POST placeholder must persist: %q", row.Notice.String)
	}

	// 通道恢复 → 重放：真实 ID 经 ledger 落账并收割（占位不得被直接当作收割目标）。
	fstore.failNoticeCAS.Store(false)
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	reaped := sup.reapedSnapshot()
	if len(reaped) != 1 || reaped[0] != "ses_debt_1" {
		t.Fatalf("replay must reap the real candidate identity only, reaped=%v", reaped)
	}
	row2, _ := fstore.GetTask(ctx, "t-j8k")
	if strings.Contains(row2.Notice.String, "candidate_cleanup_failed") ||
		strings.Contains(row2.Notice.String, `"pending"`) {
		t.Fatalf("recovered identity + proven recycle must clear the intent: %q", row2.Notice.String)
	}
	okSup := &okAcquireSup{upstream: "ses_j8k_1", port: 4096}
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&supOnlyV2Provider{sup: okSup}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg2
	if aerr := m.Activate(ctx, "t-j8k"); aerr != nil {
		t.Fatalf("gate must open after real recycle, got %v", aerr)
	}
}

// TestR1_J8_UnknownIdentity_ManualResolutionOnly 落实 Round4 J8 跨重启面：真实
// candidate 身份只在内存 ledger（重启丢失）→ 重放与启动 Reconcile 均 MUST NOT
// 把占位 `pending` 交给 reaper（DELETE 占位 404 不能证明真实 candidate 已回收）→
// 门禁持续阻断、零新增 POST、last_error 给出人工处置指引；只有人工清除意图后
// 才允许再次首建。
func TestR1_J8_UnknownIdentity_ManualResolutionOnly(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-j8u")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	sup.acquireHook = func() { fstore.failNoticeCAS.Store(true) }
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.store = fstore
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	if aerr := m.Activate(ctx, "t-j8u"); aerr == nil {
		t.Fatalf("first build must fail")
	}
	if aerr := m.Activate(ctx, "t-j8u"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("same-process gate must hold before replay, got %v", aerr)
	}

	// 进程重启：全新 Manager（ledger 清空），同一 DB——真实身份不可找回。
	m2 := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m2.store = fstore
	m2.SetLifecycleCtx(ctx)
	m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	m2.providerRegistry = reg

	// 通道恢复 → 后台重放 + 启动 Reconcile（debt pre-pass）：零 DELETE、零 POST。
	fstore.failNoticeCAS.Store(false)
	if berr := m2.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	if rerr := m2.Reconcile(ctx); rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) != 0 {
		t.Fatalf("unknown identity MUST NOT reach reaper, reaped=%v", reaped)
	}
	rowGate, _ := fstore.GetTask(ctx, "t-j8u")
	if !strings.Contains(rowGate.Notice.String, `"pending"`) {
		t.Fatalf("unknown identity intent must stay persisted: %q", rowGate.Notice.String)
	}
	if aerr := m2.Activate(ctx, "t-j8u"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") ||
		!strings.Contains(aerr.Error(), "身份未知") {
		t.Fatalf("gate must persist with manual guidance, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 1 {
		t.Fatalf("unknown identity must stay zero-POST across replay/reconcile, acquires=%d", calls)
	}
	row2, _ := fstore.GetTask(ctx, "t-j8u")
	if !strings.Contains(row2.LastError.String, "身份未知") ||
		!strings.Contains(row2.LastError.String, "人工确认") {
		t.Fatalf("last_error must carry manual-resolution guidance, got %q", row2.LastError.String)
	}

	// 人工处置后才开放：清除意图 → 健康 sup 完成首建。
	clearPendingIntentNotice(t, fstore, "t-j8u")
	okSup := &okAcquireSup{upstream: "ses_j8_1", port: 4096}
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&supOnlyV2Provider{sup: okSup}); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg2
	if aerr := m2.Activate(ctx, "t-j8u"); aerr != nil {
		t.Fatalf("Activate after manual resolution: %v", aerr)
	}
}

// ---- Round 5：J9 已正式提交的首建占位重放收敛 ----

// commitThenFailClearStore 在任务已 active（CommitFirstBuild 已提交）时让 notice
// CAS 失败——精确制造「首建提交成功 → 单独清占位失败」窗口（v2_adopt.go 提交后
// clearCandidateIntent）。active 前的 CAS（arm/settle/状态收敛）不受影响。
type commitThenFailClearStore struct {
	*StoreAdapter
	failActiveNoticeCAS atomic.Bool
}

func (s *commitThenFailClearStore) UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (application.MutationResult, error) {
	if s.failActiveNoticeCAS.Load() {
		if row, err := s.StoreAdapter.GetTask(ctx, id); err == nil && row.Status == StatusActive {
			return application.MutationResult{}, errors.New("notice cas injected failure (post-commit clear)")
		}
	}
	return s.StoreAdapter.UpdateTaskNoticeCAS(ctx, id, expected, newNotice)
}

// TestR1_J9_CommittedThenClearFail_ReplayConverges 落实 Round5 J9：首建
// CommitFirstBuild 成功 → 清占位 CAS 失败（窗口内进程重启）→ 持久化 v2
// binding/anchor/归属行证明已正式提交 → 后台重放与启动 Reconcile 清除占位
// （零 candidate DELETE——已提交 session 不是残留 candidate）→ 任务保留 active
// 正式 session，占位不再阻断后续 Activate。
func TestR1_J9_CommittedThenClearFail_ReplayConverges(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-j9")
	adapter := NewStoreAdapter(db)
	fstore := &commitThenFailClearStore{StoreAdapter: adapter}
	fstore.failActiveNoticeCAS.Store(true)
	sup := &okAcquireSup{upstream: "ses_j9_1", port: 4096}
	prov := &supOnlyV2Provider{sup: sup}
	oc := newMockOC(true)
	// 提交后接管对齐按上游列表保留归属行（空列表会删掉刚 claim 的行——与 J6
	// 同源的 mock 列表注入模式）；J9 的提交证明依赖该归属行持久化事实。
	oc.mu.Lock()
	oc.sessions = []opencode.Session{{ID: "ses_j9_1", Time: opencode.SessionTime{Created: 1, Updated: 1}}}
	oc.mu.Unlock()
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), oc)
	m.store = fstore
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// 首建提交成功；清占位 CAS 失败仅记日志（提交事实已落库）。
	if aerr := m.Activate(ctx, "t-j9"); aerr != nil {
		t.Fatalf("first build must commit despite clear failure: %v", aerr)
	}
	row, err := adapter.GetTask(ctx, "t-j9")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != StatusActive {
		t.Fatalf("status=%s（首建已提交）", row.Status)
	}
	if !strings.Contains(row.Notice.String, `"pending"`) {
		t.Fatalf("window must leave the placeholder behind: %q", row.Notice.String)
	}
	// 提交事实（判定依据）已持久化。
	binding, has, berr := adapter.GetTaskBinding(ctx, "t-j9")
	if berr != nil || !has || binding.ProviderID != ocdeckstore.ProviderIDV2 ||
		binding.UpstreamSessionID != "ses_j9_1" {
		t.Fatalf("binding: %+v has=%v err=%v", binding, has, berr)
	}

	// 进程重启形态：全新 Manager（无内存态），同一 DB。
	m2 := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m2.store = adapter
	m2.SetLifecycleCtx(ctx)
	m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&supOnlyV2Provider{sup: sup}); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg2

	// 存储恢复 → 后台重放 + 启动 Reconcile：占位按「已正式提交」事实清除。
	fstore.failActiveNoticeCAS.Store(false)
	if berr := m2.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	rowAfterReplay, _ := adapter.GetTask(ctx, "t-j9")
	if strings.Contains(rowAfterReplay.Notice.String, `"pending"`) ||
		strings.Contains(rowAfterReplay.Notice.String, "candidate_cleanup_failed") {
		t.Fatalf("committed placeholder must be cleared by replay: %q", rowAfterReplay.Notice.String)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) != 0 {
		t.Fatalf("committed session MUST NOT be reaped as candidate, reaped=%v", reaped)
	}
	// 正式 session 保留（binding/anchor/归属行未被删除）。
	if b2, has2, err := adapter.GetTaskBinding(ctx, "t-j9"); err != nil || !has2 || b2.UpstreamSessionID != "ses_j9_1" {
		t.Fatalf("committed binding must be preserved: %+v has=%v err=%v", b2, has2, err)
	}

	// 启动 Reconcile（debt pre-pass）同样不复活占位、不杀正式 session。
	if rerr := m2.Reconcile(ctx); rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}
	rowFinal, _ := adapter.GetTask(ctx, "t-j9")
	if strings.Contains(rowFinal.Notice.String, `"pending"`) {
		t.Fatalf("reconcile must not resurrect the placeholder: %q", rowFinal.Notice.String)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) != 0 {
		t.Fatalf("reconcile MUST NOT reap the committed session, reaped=%v", reaped)
	}
	if b3, has3, err := adapter.GetTaskBinding(ctx, "t-j9"); err != nil || !has3 || b3.UpstreamSessionID != "ses_j9_1" {
		t.Fatalf("reconcile must preserve the committed session: %+v has=%v err=%v", b3, has3, err)
	}
	// 旧占位不再阻断：重启后 Reconcile 按既有语义把无 runtime 的任务收敛
	// suspended（正式 binding/session 保留）——随后 Activate 不得被旧占位门禁拦截。
	if serr := m2.Suspend(ctx, "t-j9"); serr != nil &&
		!strings.Contains(serr.Error(), "suspend requires active") {
		t.Fatalf("Suspend: %v", serr)
	}
	if aerr := m2.Activate(ctx, "t-j9"); aerr != nil {
		if strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
			t.Fatalf("stale placeholder must not gate Activate, got %v", aerr)
		}
	}
}

// TestR1_J9_NoCommitProof_StaysGated 落实 Round5 J9 反面对：占位存在但持久化
// 事实不构成提交证明（v1 钉扎、无 v2 binding）→ 重放 MUST 继续按 J8 阻断，
// 不得把「任务行状态」或历史事实当作该占位的提交证明。
func TestR1_J9_NoCommitProof_StaysGated(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedBranchBTask(t, db, "t-j9n")
	adapter := NewStoreAdapter(db)
	sup := &debtCandidateSup{realCandidate: "ses_debt_1"}
	prov := &debtProvider{sup: sup}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	if aerr := m.armCandidateIntent(ctx, "t-j9n"); aerr != nil {
		t.Fatalf("arm placeholder: %v", aerr)
	}

	// 无 v2 binding/anchor/归属行 → 无提交证明 → 占位保持（不调 reaper）。
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("replay: %v", berr)
	}
	row, _ := adapter.GetTask(ctx, "t-j9n")
	if !strings.Contains(row.Notice.String, `"pending"`) {
		t.Fatalf("no commit proof MUST keep the placeholder: %q", row.Notice.String)
	}
	if reaped := sup.reapedSnapshot(); len(reaped) != 0 {
		t.Fatalf("unknown identity MUST NOT reach reaper, reaped=%v", reaped)
	}
	if aerr := m.Activate(ctx, "t-j9n"); aerr == nil ||
		!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("gate must hold without commit proof, got %v", aerr)
	}
	if calls := atomic.LoadInt32(&sup.acquireCalls); calls != 0 {
		t.Fatalf("gated Activate must be zero-POST, acquires=%d", calls)
	}
}

// ---- Round 8：I20 ledger 按任务隔离 · I17 重试生命周期闭环 ----

// TestStage4_R8_I20_LedgerPerTaskIsolation 落实 Round8 I20：A 挂起失败留下
// notice+env 义务（受管登记按任务分组）→ 先触发 B 的重试调度——B 的 notice/
// env 快照/运行关联零污染（A 的义务不得按 B 身份执行）；随后 A 重试——原
// tickets 落账收割（收割目标=原 tickets）+ A env 快照清除。
func TestStage4_R8_I20_LedgerPerTaskIsolation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-ia")
	seedV2ActiveTask(t, db, "t-ib")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	rtA := m.newRuntime("t-ia")
	m.setRuntime("t-ia", rtA)
	rtB := m.newRuntime("t-ib")
	m.setRuntime("t-ib", rtB)
	mproc := m.proc.(*mockProc)
	mproc.mu.Lock()
	mproc.sessions["ocdeck-v2-tui-t-ia"] = true
	mproc.killResults["ocdeck-v2-tui-t-ia"] = process.KillResult{
		SessionKilled:  false,
		Disposition:    process.DispositionKillFailed,
		CleanupTickets: []string{"tk-iso-1", "tk-iso-2"},
	}
	mproc.mu.Unlock()

	// A：停止确认 + 持续失败（notice CAS + env 清除）→ 义务进 ledger[A]，
	// 标注失败 → A 保持 active。
	refA := "ocdeck-v2-tui-t-ia"
	if ierr := m.recordSuspendStopIntent(ctx, "t-ia", refA, string(rtA.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	fstore.failNoticeCAS.Store(true)
	fstore.failEnvClear.Store(true)
	rowA, _ := fstore.GetTask(ctx, "t-ia")
	entriesA, _ := parseNotices(rowA.Notice)
	if rerr := m.retryTaskNotices(ctx, rowA, entriesA); rerr == nil {
		t.Fatalf("A persistence failure must surface")
	}
	if !m.reapLedgerHasUnfinished("t-ia") {
		t.Fatalf("A obligations must be registered in ledger[t-ia]")
	}
	// Round8 I20 污染检测点（A 重放后、B 收敛前）：ledger[A] 持有 env 义务——
	// 若被按 B 身份误执行，此处 B 的快照已被清。快照必须原样保留。
	var snapB0 sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-ib'`).Scan(&snapB0); err != nil {
		t.Fatal(err)
	}
	if !snapB0.Valid || snapB0.String == "" {
		t.Fatalf("B env snapshot must be untouched by A's ledger before B converges")
	}
	// B：正常停止收敛（成功）。注入先解除（B 的意图记录与收敛走健康通道）。
	fstore.failNoticeCAS.Store(false)
	fstore.failEnvClear.Store(false)
	refB := "ocdeck-v2-tui-t-ib"
	if ierr := m.recordSuspendStopIntent(ctx, "t-ib", refB, string(rtB.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	if serr := m.Suspend(ctx, "t-ib"); serr != nil {
		t.Fatalf("suspend B: %v", serr)
	}

	// 先触发 B 的重试调度（B notice 已空但 Suspend 成功后无意图——直取 B 行重放，
	// 隔离性断言不受 A 残留影响）。
	rowB, _ := fstore.GetTask(ctx, "t-ib")
	entriesB, _ := parseNotices(rowB.Notice)
	if rerr := m.retryTaskNotices(ctx, rowB, entriesB); rerr != nil {
		t.Fatalf("replay B: %v", rerr)
	}
	// B 零污染断言：env 快照原样保留（A 的 env 义务不得清 B 的快照）、无 A 的
	// tickets 债务、B 保持 suspended。
	rowB2, _ := fstore.GetTask(ctx, "t-ib")
	if rowB2.Status != "suspended" {
		t.Fatalf("B status=%s", rowB2.Status)
	}
	if strings.Contains(rowB2.Notice.String, "tk-iso-1") || strings.Contains(rowB2.Notice.String, "tk-iso-2") {
		t.Fatalf("A tickets must not leak into B: %q", rowB2.Notice.String)
	}
	// B 自身收敛成功后快照清除属正常语义（污染检测见上方 snapB0 断言）。

	// A 恢复 → 重放：原 tickets 落账收割（收割目标=原 tickets）+ A env 清除 +
	// 收敛 suspended + 意图清除。
	fstore.failNoticeCAS.Store(false)
	fstore.failEnvClear.Store(false)
	rowA2, _ := fstore.GetTask(ctx, "t-ia")
	entriesA2, _ := parseNotices(rowA2.Notice)
	if rerr := m.retryTaskNotices(ctx, rowA2, entriesA2); rerr != nil {
		t.Fatalf("replay A(restore): %v", rerr)
	}
	rowA3, _ := fstore.GetTask(ctx, "t-ia")
	if rowA3.Status != "suspended" {
		t.Fatalf("A status=%s", rowA3.Status)
	}
	if !strings.Contains(rowA3.Notice.String, "tk-iso-1") || !strings.Contains(rowA3.Notice.String, "tk-iso-2") {
		t.Fatalf("A original tickets must be persisted after recovery: %q", rowA3.Notice.String)
	}
	if strings.Contains(rowA3.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("A intent must be cleared after converged replay: %q", rowA3.Notice.String)
	}
	var snapA sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-ia'`).Scan(&snapA); err != nil {
		t.Fatal(err)
	}
	if snapA.Valid && snapA.String != "" {
		t.Fatalf("A env snapshot must be cleared by A's own obligation, got %q", snapA.String)
	}
	if m.reapLedgerHasUnfinished("t-ia") || m.reapLedgerHasUnfinished("t-ib") {
		t.Fatalf("ledgers must be drained")
	}
}

// TestStage4_R8_I17_LedgerGradualRecovery 落实 Round8 I17 重试生命周期闭环：
// drain 持续失败（逐步恢复而非一次性）→ 意图作为唯一重试入口持续保有（不得
// 清除）→ 恢复后按原 tickets 落账收割（union 不膨胀）→ 收敛 suspended + 意图
// 清除。
func TestStage4_R8_I17_LedgerGradualRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-gr")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	rt := m.newRuntime("t-gr")
	m.setRuntime("t-gr", rt)
	mproc := m.proc.(*mockProc)
	mproc.mu.Lock()
	mproc.sessions["ocdeck-v2-tui-t-gr"] = true
	mproc.killResults["ocdeck-v2-tui-t-gr"] = process.KillResult{
		SessionKilled:  false,
		Disposition:    process.DispositionKillFailed,
		CleanupTickets: []string{"tk-gr-1"},
	}
	mproc.mu.Unlock()
	ref := "ocdeck-v2-tui-t-gr"
	if ierr := m.recordSuspendStopIntent(ctx, "t-gr", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}

	// 第 1/2 轮：notice CAS 持续失败 → 义务留 ledger、意图保留（唯一重试入口）、
	// 状态保持 active（Round6 排序：标注未落库不收敛）。
	fstore.failNoticeCAS.Store(true)
	for round := 0; round < 2; round++ {
		row, _ := fstore.GetTask(ctx, "t-gr")
		entries, _ := parseNotices(row.Notice)
		if rerr := m.retryTaskNotices(ctx, row, entries); rerr == nil {
			t.Fatalf("round %d: persistence failure must surface", round)
		}
		if !m.reapLedgerHasUnfinished("t-gr") {
			t.Fatalf("round %d: ledger obligations must be retained", round)
		}
		if !strings.Contains(row.Notice.String, noticeCodeSuspendStop) {
			t.Fatalf("round %d: intent is the sole retry entry and must be kept", round)
		}
		if r, _ := fstore.GetTask(ctx, "t-gr"); r.Status != "active" {
			t.Fatalf("round %d: status must stay active (annotate not persisted), got %s", round, r.Status)
		}
	}

	// 恢复 → 后台调度（Round8 I17：processRetryableNotices 独立覆盖 ledger
	// 任务）：债务落账（tickets union 不膨胀=原 tickets）→ 收敛 suspended →
	// 意图清除。
	fstore.failNoticeCAS.Store(false)
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("background scheduling: %v", berr)
	}
	row4, _ := fstore.GetTask(ctx, "t-gr")
	if row4.Status != "suspended" {
		t.Fatalf("status=%s", row4.Status)
	}
	if !strings.Contains(row4.Notice.String, "tk-gr-1") {
		t.Fatalf("original tickets must be persisted: %q", row4.Notice.String)
	}
	if strings.Contains(row4.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared: %q", row4.Notice.String)
	}
	if m.reapLedgerHasUnfinished("t-gr") {
		t.Fatalf("ledger must be drained")
	}
	// Round8 I17：notice 真正清空后，ledger 任务仍被后台独立调度（drain 不依赖
	// 意图存在——无意图则不收敛，仅落账义务）。
	if _, werr := m.writeNoticeCAS(ctx, "t-gr", row4.Notice, sql.NullString{}); werr != nil {
		t.Fatal(werr)
	}
	m.registerReapObligations("t-gr", []suspendFinishObligation{{
		Kind: "env", TaskID: "t-gr", Token: string(rt.instVersion),
	}})
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("background scheduling (empty notice): %v", berr)
	}
	if m.reapLedgerHasUnfinished("t-gr") {
		t.Fatalf("empty-notice ledger task must still be scheduled and drained")
	}
	if row5, _ := fstore.GetTask(ctx, "t-gr"); strings.Contains(row5.Notice.String, "tk-gr-1") {
		t.Fatalf("empty notice must not resurrect tickets: %q", row5.Notice.String)
	}
}

// ---- Round 9：I21 空 token env 义务的代际隔离 + flush 锁尊重 ----

// TestStage4_R9_I21_EmptyTokenEnvObligation_NewGenerationProtected 落实
// Round9 I21：空 token 的 env 义务（记录时无 runtime）不得清除后来出现的
// 新一代 env 快照。序列：无 runtime 停止意图（token=""）→ env 清除失败（义务
// 进 pending+ledger 副本、收敛 suspended）→ 义务补全（rt 为空=执行清除）→
// 意图清除+ledger 副本移除 → 重新激活建新 rt+新快照 → 后台 drain 残留的空
// token/旧 token 义务副本：新快照与 runtime 均原样保留（代际守卫 skip）。
func TestStage4_R9_I21_EmptyTokenEnvObligation_NewGenerationProtected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-e9")
	adapter := NewStoreAdapter(db)
	fstore := &failReplayPersistStore{StoreAdapter: adapter}
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManagerStore(t, db, fstore, prov)
	m.lifecycle = nil
	ref := "ocdeck-v2-tui-t-e9"

	// ①无 runtime 记录停止意图（空 token）；env 清除注入失败 → 义务进
	// pending+ledger 副本、收敛 suspended、快照残留。
	if ierr := m.recordSuspendStopIntent(ctx, "t-e9", ref, ""); ierr != nil {
		t.Fatal(ierr)
	}
	fstore.failEnvClear.Store(true)
	row, _ := fstore.GetTask(ctx, "t-e9")
	entries, _ := parseNotices(row.Notice)
	_ = m.retryTaskNotices(ctx, row, entries)
	row2, _ := fstore.GetTask(ctx, "t-e9")
	if row2.Status != "suspended" {
		t.Fatalf("status=%s（标注先于收敛——Round6 排序）", row2.Status)
	}
	if !strings.Contains(row2.Notice.String, suspendFinishPendingKey) {
		t.Fatalf("env obligation must be kept as pending: %q", row2.Notice.String)
	}
	if !m.reapLedgerHasUnfinished("t-e9") {
		t.Fatalf("env obligation copy must be in ledger[t-e9]")
	}

	// ②恢复 → 重放：pending 补全（rt 为空=执行清除）→ 快照清空+意图清除+
	// ledger 副本移除（Round8 I21② removeReapLedgerObligations 接线）。
	fstore.failEnvClear.Store(false)
	row3, _ := fstore.GetTask(ctx, "t-e9")
	entries3, _ := parseNotices(row3.Notice)
	if rerr := m.retryTaskNotices(ctx, row3, entries3); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	if m.reapLedgerHasUnfinished("t-e9") {
		t.Fatalf("ledger copy must be removed after pending obligations complete")
	}
	row4, _ := fstore.GetTask(ctx, "t-e9")
	if strings.Contains(row4.Notice.String, suspendFinishPendingKey) {
		t.Fatalf("pending marker must be cleared: %q", row4.Notice.String)
	}

	// ③重新激活（公开 Activate；fake sup Resume 延续原 session）→ 新 rt 注册 +
	// 新 env 快照写回。
	rec := &recordingV2Sup{upstream: "ses_keep", port: 4096}
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&recordingV2Provider{sup: rec}); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg
	if aerr := m.Activate(ctx, "t-e9"); aerr != nil {
		t.Fatalf("re-activate: %v", aerr)
	}
	rtNew := m.getRuntime("t-e9")
	if rtNew == nil {
		t.Fatalf("new generation runtime must be registered")
	}
	newSnap := sql.NullString{String: `{"vars":{"OCDECK_TASK_ID":"t-e9","GEN":"2"}}`, Valid: true}
	if _, werr := m.writeEnvSnapshot(ctx, "t-e9", newSnap); werr != nil {
		t.Fatal(werr)
	}

	// ④残留的空 token env 义务副本 + 旧代 token 义务副本先后登记 → 后台调度
	// drain：新快照与 runtime 均原样保留（空 token 视为新代；旧代失配 skip）。
	m.registerReapObligations("t-e9", []suspendFinishObligation{
		{Kind: "env", TaskID: "t-e9", Token: ""},
		{Kind: "env", TaskID: "t-e9", Token: "stale-gen"},
	})
	if berr := m.processRetryableNotices(ctx); berr != nil {
		t.Fatalf("background drain: %v", berr)
	}
	var snapAfter sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-e9'`).Scan(&snapAfter); err != nil {
		t.Fatal(err)
	}
	if !snapAfter.Valid || snapAfter.String != newSnap.String {
		t.Fatalf("new generation env snapshot must be preserved, got %q (want %q)", snapAfter.String, newSnap.String)
	}
	if rtCur := m.getRuntime("t-e9"); rtCur == nil || rtCur.instVersion != rtNew.instVersion {
		t.Fatalf("new generation runtime must be untouched")
	}
	if m.reapLedgerHasUnfinished("t-e9") {
		t.Fatalf("stale obligations must be dropped (skipped), not retained")
	}
}

// TestStage4_R9_I21_FlushRespectsTaskLock 落实 Round9 I21③：flushReapLedger
// 不绕任务锁——busy 任务的登记保留并进入 failed 报告，不无锁执行 drain。
func TestStage4_R9_I21_FlushRespectsTaskLock(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-fl")
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	m.registerReapObligations("t-fl", []suspendFinishObligation{{
		Kind: "env", TaskID: "t-fl", Token: "", Name: "ocdeck-v2-tui-t-fl",
	}})
	unlock, lerr := m.tryLockTask("t-fl")
	if lerr != nil {
		t.Fatal(lerr)
	}
	failed := m.flushReapLedger(ctx)
	unlock()
	found := false
	for _, id := range failed {
		if id == "t-fl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("locked task must be reported as flush-failed, failed=%v", failed)
	}
	if !m.reapLedgerHasUnfinished("t-fl") {
		t.Fatalf("locked task ledger must be retained (not drained without lock)")
	}
}

// ---- 收尾 Round1：J6 真实 provider 承接 A 连续性 ----

// TestR1_J6_RealProvider_AdoptA_Continuity 落实收尾 Round1 J6：经真实
// NewSPIProviderWithDeps 注册的 provider 从 v1 历史事实（v1 binding/anchor/
// 归属行）承接 A——公开 Activate → 真实 Resume（GET 延续原 session，零创建
// POST）→ binding/归属/anchor 原子转换（v1→v2 单事务）→ 钉扎不变 → active。
func TestR1_J6_RealProvider_AdoptA_Continuity(t *testing.T) {
	ctx := context.Background()
	// harness 上游 session=ses_keep（GET 路由按 ^ses_ 前缀回显）；其自种行仅为
	// provider 装配，本测试消费承接 A 历史事实行——移除避免混淆。
	h := newRealStreamHarness(t, "t-j6x", "ses_keep", "active")
	if _, err := h.db.Exec(`DELETE FROM tasks WHERE id='t-j6x'`); err != nil {
		t.Fatal(err)
	}
	// v1 历史事实：binding(ses_keep)@v1 + anchor@v1 + 归属行（父+子）。
	seedAdoptAFormTask(t, h.db, "t-ja", ocdeckstore.ProviderIDV1, "ses_keep")
	// 上游列表种子走 HTTP 面（listPageBody——生产装配保真：startSSE 对齐客户端
	// 经桥接优先打真实 provider 的 GET /api/session，mock ocFactory 面不再承载
	// v2 对齐事实），列出承接历史（父+子）保住承接行。
	h.listPageBody = `{"data":[` +
		`{"id":"ses_keep","time":{"created":1,"updated":1},"location":{"directory":"/wt"}},` +
		`{"id":"sess-child","parentID":"ses_keep","time":{"created":1,"updated":1},"location":{"directory":"/wt"}}` +
		`],"cursor":{"previous":null,"next":null}}`

	// 公开 Activate 入口（v1 钉扎 + 机器 v2 开关开 → R8 承接 A）。
	if aerr := h.m.Activate(ctx, "t-ja"); aerr != nil {
		t.Fatalf("Activate(adopt A via real provider): %v", aerr)
	}
	// 零创建 POST：原 session 经真实 Resume（GET）延续。
	if n := h.created.Load(); n != 0 {
		t.Fatalf("adopt A MUST NOT create sessions, POSTs=%d", n)
	}
	// 原子转换断言：binding v2（upstream 不变）+ 归属行 v1→v2（父+子，session_id
	// 不变）+ anchor 转 v2（id 不变）+ 钉扎不变 + active 收敛。
	got, has, err := h.adapter.GetTaskBinding(ctx, "t-ja")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding: %+v has=%v err=%v", got, has, err)
	}
	var v2Rows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-ja' AND provider_id=? AND provider_instance_id=?`,
		ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default).Scan(&v2Rows); err != nil {
		t.Fatal(err)
	}
	if v2Rows != 2 {
		t.Fatalf("ownership rows must convert v1→v2 (parent+child), v2 rows=%d", v2Rows)
	}
	var childParent string
	if err := h.db.QueryRow(`SELECT parent_id FROM task_sessions WHERE task_id='t-ja' AND session_id='sess-child'`).Scan(&childParent); err != nil || childParent != "ses_keep" {
		t.Fatalf("child row must keep parent chain after align, parent=%q err=%v", childParent, err)
	}
	var anchorProv, anchorID string
	if err := h.db.QueryRow(`SELECT anchor_provider_id, anchor_session_id FROM tasks WHERE id='t-ja'`).Scan(&anchorProv, &anchorID); err != nil {
		t.Fatal(err)
	}
	if anchorProv != ocdeckstore.ProviderIDV2 || anchorID != "ses_keep" {
		t.Fatalf("anchor must convert to v2 with id unchanged, got %s/%s", anchorProv, anchorID)
	}
	var pinned string
	if err := h.db.QueryRow(`SELECT provider_id FROM tasks WHERE id='t-ja'`).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != ocdeckstore.ProviderIDV1 {
		t.Fatalf("pinned provider must stay v1, got %q", pinned)
	}
	row, err := h.adapter.GetTask(ctx, "t-ja")
	if err != nil || row.Status != "active" {
		t.Fatalf("status=%s err=%v", row.Status, err)
	}
}

// TestStage4_R6_I19_ReRecordPreservesPending 落实 Round6 I19：同键替换保留未
// 完成义务——重新记录停止意图不得覆盖 pending（重新 kill 同名单元不保证重建
// 原 cleanup tickets）。
func TestStage4_R6_I19_ReRecordPreservesPending(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-rr")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	rt := m.newRuntime("t-rr")
	m.setRuntime("t-rr", rt)
	ref := "ocdeck-v2-tui-t-rr"

	if ierr := m.recordSuspendStopIntent(ctx, "t-rr", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row, _ := adapter.GetTask(ctx, "t-rr")
	entries, _ := parseNotices(row.Notice)
	annotated := annotateSuspendStopPending(entries[0], []suspendFinishObligation{{
		Kind: "notice", Name: ref, Tickets: []string{"tk-r6-keep"}, Reason: "kill_failed", Retryable: true,
	}})
	if _, werr := m.writeNoticeCAS(ctx, "t-rr", row.Notice, encodeNotices([]noticeEntry{annotated})); werr != nil {
		t.Fatal(werr)
	}

	// 同键重录（新停止确认失败）→ pending 必须随新条目延续。
	if ierr := m.recordSuspendStopIntent(ctx, "t-rr", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row2, _ := adapter.GetTask(ctx, "t-rr")
	if !strings.Contains(row2.Notice.String, "tk-r6-keep") {
		t.Fatalf("same-key re-record must preserve pending tickets: %q", row2.Notice.String)
	}
	if !strings.Contains(row2.Notice.String, suspendFinishPendingKey) {
		t.Fatalf("pending marker must survive re-record: %q", row2.Notice.String)
	}
}

// ---- 收尾 Round1：J6 真实 provider 唯一实例/生命周期 ----

// TestR1_J6_RealProvider_SharedInstanceAndLifecycle 落实收尾 Round1 J1/J6：
// 经真实 NewSPIProviderWithDeps 注册的 provider——Supervisor() 唯一实例；并发
// 两任务首建共享同一 serve（spawn==1，全局互斥）、各自独立 upstream session、
// SSE 订阅各恰一次；Manager 关停（StopServerMonitoring）后 serve 死亡不再重拉
// （监控已停，spawn 不再增长）。
func TestR1_J6_RealProvider_SharedInstanceAndLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-j6", "ses_keep", "active")
	h.allowConns = 2
	// 额外两个 v2 钉扎 suspended 首建任务（无 binding；v1 钉扎走 R8-B 同型分支）。
	now := time.Now().Unix()
	if _, err := h.db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p-j6', 'proj', '/repo', 'main', ?, 'repo')`, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t-f1", "t-f2"} {
		if _, err := h.db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
			 provider_id, base_ref, created_at, updated_at)
			 VALUES (?, 'p-j6', 'task', 'b', 'suspended', '/wt', 'opencode-v1', 'refs/heads/main', ?, ?)`,
			id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	sA := h.prov.Supervisor()
	sB := h.prov.Supervisor()
	if sA != sB {
		t.Fatalf("Supervisor() must return the single instance")
	}

	var wg sync.WaitGroup
	aerrs := [2]error{}
	ids := []string{"t-f1", "t-f2"}
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			aerrs[i] = h.m.Activate(ctx, ids[i])
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if aerrs[i] != nil {
			t.Fatalf("Activate(%s): %v", ids[i], aerrs[i])
		}
	}
	if spawns := h.proc.spawnCount(); spawns != 1 {
		t.Fatalf("concurrent first-builds must share one serve, spawns=%d", spawns)
	}
	if n := h.conns.Load(); n != 2 {
		t.Fatalf("two tasks must hold exactly two event subscriptions, conns=%d", n)
	}
	for _, id := range ids {
		row, err := h.adapter.GetTask(ctx, id)
		if err != nil || row.Status != "active" {
			t.Fatalf("%s status=%s err=%v", id, row.Status, err)
		}
	}
	b1, _, _ := h.adapter.GetTaskBinding(ctx, "t-f1")
	b2, _, _ := h.adapter.GetTaskBinding(ctx, "t-f2")
	if b1.UpstreamSessionID == "" || b2.UpstreamSessionID == "" || b1.UpstreamSessionID == b2.UpstreamSessionID {
		t.Fatalf("distinct upstream sessions required: %q vs %q", b1.UpstreamSessionID, b2.UpstreamSessionID)
	}

	// ①监控开启时的死亡 MUST 触发重拉（Round2 J6：死亡接缝同步身份探测——
	// 否则此处 spawn 不增长、关停断言假绿）。
	h.proc.killUnit()
	deadline := time.Now().Add(5 * time.Second)
	for h.proc.spawnCount() == 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if spawns := h.proc.spawnCount(); spawns != 2 {
		t.Fatalf("visible death while monitoring must trigger re-pull, spawns=%d", spawns)
	}

	// ②经 Manager 停止分派入口停止监控 → serve 死亡零重拉（Round2 J6：不走
	// 直调 controller——与生产 Manager 停止分派同一入口）。
	h.m.stopV2ServerMonitoring()
	h.proc.killUnit()
	time.Sleep(60 * time.Millisecond) // 数个监控周期：停止后不得重拉
	if spawns := h.proc.spawnCount(); spawns != 2 {
		t.Fatalf("monitor must stay stopped after manager shutdown, spawns=%d", spawns)
	}
}

// 连接断开 → 正式消费方在断线期间接手（安装回调、进入 ready 等待）→ 上游恢复
// TestStage4_R5_I10_HandoverDuringDisconnect 落实 Round5 I10：候选握手完成 →
// 连接断开 → 正式消费方在断线期间接手（安装回调、进入 ready 等待）→ 上游恢复
// 重连成功 → 首次 ready 由重连满足（对齐执行、接管成功）——不再依赖
// 「期间从未断线」。
func TestStage4_R5_I10_HandoverDuringDisconnect(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-hd", "ses_keep", "active")
	h.allowConns = 2
	h.dropConn = make(chan struct{})
	h.conn2Hello = make(chan struct{})

	binding, has, err := h.adapter.GetTaskBinding(ctx, "t-hd")
	if err != nil || !has {
		t.Fatalf("binding: %v", err)
	}
	warmRealStreamHarness(t, h)
	es, serr := h.prov.Events().Subscribe(ctx, binding)
	if serr != nil {
		t.Fatalf("Subscribe: %v", serr)
	}
	connected := make(chan struct{}, 4)
	es.SetCallbacks(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	}, nil)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("handshake connected never fired")
	}

	// 断开首条连接 → 客户端发起 conn2（建立 TCP 但尚未握手——hello 门控挡住）。
	close(h.dropConn)
	deadline := time.Now().Add(5 * time.Second)
	for h.conns.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("client never reconnected after server drop")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 断线期间安装正式消费方（接管进入 ready 等待）。
	row, err := h.adapter.GetTask(ctx, "t-hd")
	if err != nil {
		t.Fatal(err)
	}
	takeoverDone := make(chan error, 1)
	start := time.Now()
	go func() {
		takeoverDone <- h.m.commitV2RuntimeReady(ctx, "t-hd", row, h.port, "pw", binding, AlignModeRepo, es, true)
	}()
	time.Sleep(150 * time.Millisecond) // 消费方已安装、阻塞在 ready 等待

	// 上游恢复：conn2 握手完成 → fireReconnect → 交接期首次 ready 满足。
	close(h.conn2Hello)
	select {
	case terr := <-takeoverDone:
		if terr != nil {
			t.Fatalf("takeover after during-disconnect handover: %v", terr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("takeover did not converge (first ready not satisfied by reconnect)")
	}
	if elapsed := time.Since(start); elapsed > 14*time.Second {
		t.Fatalf("ready must be satisfied by reconnect, elapsed=%v", elapsed)
	}
	if n := h.conns.Load(); n != 2 {
		t.Fatalf("event connections=%d（无重订阅）", n)
	}
}

// ---- Round 4：I2 空 token/门禁/清除 · I17 重放收尾 · I10 minted token/断线重连 · I16 锁失败关流 ----

// TestStage4_R4_I2_EmptyTokenIntent_GatesActivate 落实 Round4 I2（门禁方向）：
// 停止收敛意图纳入激活债务门禁——意图在位时 Activate 拒绝（uncleaned cleanup
// debt），不得绕过未完成的挂起收敛。
func TestStage4_R4_I2_EmptyTokenIntent_GatesActivate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-et")
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	if ierr := m.recordSuspendStopIntent(ctx, "t-et", "ocdeck-v2-tui-t-et", ""); ierr != nil {
		t.Fatal(ierr)
	}
	// 债务门禁在 suspended 前置校验之后——种子置 suspended 使 Activate 走到门禁。
	if _, err := db.Exec(`UPDATE tasks SET status='suspended' WHERE id='t-et'`); err != nil {
		t.Fatal(err)
	}
	if aerr := m.Activate(ctx, "t-et"); aerr == nil || !strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
		t.Fatalf("pending suspend-stop intent must gate Activate, got %v", aerr)
	}
}

// TestStage4_R4_I2_EmptyTokenIntent_StaleWhenRuntimeAppears 落实 Round4 I2
// （守卫方向）：记录时无 runtime（空 token——无可验证生命周期身份）→ 其后出现
// 的 runtime 一律视为新代——重放仅清除意图、零触达（空 token 不得授权清理后来
// 出现的运行关联）。
func TestStage4_R4_I2_EmptyTokenIntent_StaleWhenRuntimeAppears(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-et")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)

	if ierr := m.recordSuspendStopIntent(ctx, "t-et", "ocdeck-v2-tui-t-et", ""); ierr != nil {
		t.Fatal(ierr)
	}
	rt := m.newRuntime("t-et")
	m.setRuntime("t-et", rt)
	before := string(rt.instVersion)
	row, _ := adapter.GetTask(ctx, "t-et")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	row2, _ := adapter.GetTask(ctx, "t-et")
	if row2.Status != "active" {
		t.Fatalf("empty-token intent must not converge a later runtime, status=%s", row2.Status)
	}
	if strings.Contains(row2.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("stale intent must be cleared: %q", row2.Notice.String)
	}
	if rt2 := m.getRuntime("t-et"); rt2 == nil || string(rt2.instVersion) != before {
		t.Fatalf("new generation runtime must be untouched")
	}
	if atomic.LoadInt32(&sup.releaseN) != 0 {
		t.Fatalf("empty-token intent must not release later runtime, releases=%d", atomic.LoadInt32(&sup.releaseN))
	}
}

// TestStage4_R4_I2_SecondSuspendClearsStaleIntent 落实 Round4 I2：停止确认失败
// 留下的旧意图，被后续成功的 Suspend 取得收敛权时清除（不得在重放中触达本次
// 收敛后的状态）。
func TestStage4_R4_I2_SecondSuspendClearsStaleIntent(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-sc")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	if ierr := m.recordSuspendStopIntent(ctx, "t-sc", "ocdeck-v2-tui-t-sc", ""); ierr != nil {
		t.Fatal(ierr)
	}
	if serr := m.Suspend(ctx, "t-sc"); serr != nil {
		t.Fatalf("suspend: %v", serr)
	}
	row, err := adapter.GetTask(ctx, "t-sc")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "suspended" {
		t.Fatalf("status=%s", row.Status)
	}
	if strings.Contains(row.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("stale intent must be cleared by successful suspend: %q", row.Notice.String)
	}
}

// TestStage4_R4_I17_ReplayPersistsCleanupDebtAndClearsEnv 落实 Round4 I17：重放
// 收敛复用正常挂起收尾语义——kill 失败固化为 retryable 残留 notice（含 reap
// tickets）、env 快照清除，随后才清除停止意图（suspended 收敛不丢清理债务、
// 不残留 env 快照）。
func TestStage4_R4_I17_ReplayPersistsCleanupDebtAndClearsEnv(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-rp")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	rt := m.newRuntime("t-rp")
	m.setRuntime("t-rp", rt)
	mproc := m.proc.(*mockProc)
	mproc.mu.Lock()
	mproc.sessions["ocdeck-v2-tui-t-rp"] = true
	// kill 失败 + reap tickets（非净 disposition——残留债务信号）。
	mproc.killResults["ocdeck-v2-tui-t-rp"] = process.KillResult{
		SessionKilled:  false,
		Disposition:    process.DispositionKillFailed,
		CleanupTickets: []string{"tk-reap-1"},
	}
	mproc.mu.Unlock()

	ref := "ocdeck-v2-tui-t-rp"
	if ierr := m.recordSuspendStopIntent(ctx, "t-rp", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row, _ := adapter.GetTask(ctx, "t-rp")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	row2, err := adapter.GetTask(ctx, "t-rp")
	if err != nil {
		t.Fatal(err)
	}
	if row2.Status != "suspended" {
		t.Fatalf("status=%s（收敛完成）", row2.Status)
	}
	// 清理债务已持久化（kill_failed + tickets），停止意图已清除。
	if !strings.Contains(row2.Notice.String, "tk-reap-1") {
		t.Fatalf("cleanup tickets must be persisted as debt: %q", row2.Notice.String)
	}
	if strings.Contains(row2.Notice.String, noticeCodeSuspendStop) {
		t.Fatalf("intent must be cleared after converged replay: %q", row2.Notice.String)
	}
	// env 快照清除（design.md §2：挂起成功清快照——重放同语义）。
	var snap sql.NullString
	if err := db.QueryRow(`SELECT env_snapshot FROM tasks WHERE id='t-rp'`).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Valid && snap.String != "" {
		t.Fatalf("env snapshot must be cleared by replay, got %q", snap.String)
	}
}

// TestStage4_R4_I17_ReplayShellEnumFail_PersistsDebt 落实枚举失败如实入债务：
// shell 枚举错误不再被吞——固化为 retryable 残留 notice 后收敛。
func TestStage4_R4_I17_ReplayShellEnumFail_PersistsDebt(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-re")
	adapter := NewStoreAdapter(db)
	sup := &suspendV2Sup{}
	prov := &suspendV2Provider{sup: sup}
	m := newSuspendManager(t, db, prov)
	lfp := &listFailProc{mockProc: m.proc.(*mockProc)}
	m.proc = lfp
	lfp.fail.Store(true)
	rt := m.newRuntime("t-re")
	m.setRuntime("t-re", rt)

	ref := "ocdeck-v2-tui-t-re"
	if ierr := m.recordSuspendStopIntent(ctx, "t-re", ref, string(rt.instVersion)); ierr != nil {
		t.Fatal(ierr)
	}
	row, _ := adapter.GetTask(ctx, "t-re")
	entries, _ := parseNotices(row.Notice)
	if rerr := m.retryTaskNotices(ctx, row, entries); rerr != nil {
		t.Fatalf("replay: %v", rerr)
	}
	row2, _ := adapter.GetTask(ctx, "t-re")
	if row2.Status != "suspended" {
		t.Fatalf("status=%s", row2.Status)
	}
	// 枚举失败固化为 retryable 残留债务（kill_failed 形态）；细节文本入 last_error。
	if !strings.Contains(row2.Notice.String, "kill_failed") || !strings.Contains(row2.Notice.String, `"retryable":true`) {
		t.Fatalf("shell enumeration failure must be persisted as retryable debt: %q", row2.Notice.String)
	}
	if !strings.Contains(row2.LastError.String, "list shells") {
		t.Fatalf("enumeration detail must surface in last_error: %q", row2.LastError.String)
	}
}

// listFailProc 包装 mockProc 注入 ListSessions 失败（shell 枚举故障注入接缝）。
type listFailProc struct {
	*mockProc
	fail atomic.Bool
}

func (p *listFailProc) ListSessions() ([]string, error) {
	if p.fail.Load() {
		return nil, errors.New("list sessions injected failure")
	}
	return p.mockProc.ListSessions()
}

// TestStage4_R4_I10_MintedTokenRecovery_ExecutesResume 落实 Round5 I18：无
// runtime（接管前段失败登记）的恢复入口受注册表 tombstone 代际权威约束——
// 登记 T1→推进至 T2→runtime 为空→T1 零副作用拒绝、T2 才能恢复；任意字面量
// token 拒绝；v1 回调形态（allowNoRuntime=false）保持原拒绝语义。
func TestStage4_R4_I10_MintedTokenRecovery_ExecutesResume(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTask(t, db, "t-mt")
	adapter := NewStoreAdapter(db)
	rec := &recordingV2Sup{upstream: "ses_keep", port: 4096}
	prov := &recordingV2Provider{sup: rec}
	m := newTestManagerFull(t, adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
	m.SetLifecycleCtx(ctx)
	m.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	m.providerRegistry = reg

	// 登记 T1 → 推进至 T2（登记后发生过代际推进）→ runtime 为空：
	// T1 零副作用拒绝（stale——当前 tombstone 已是 T2）。
	t1 := m.runtimeRegistry.NewInstVersion("t-mt")
	t2 := m.runtimeRegistry.NewInstVersion("t-mt")
	m.ensureRecovery("t-mt", t1, true)
	if rec.resumeCalls != 0 {
		t.Fatalf("stale registered token T1 must be rejected, resumes=%d", rec.resumeCalls)
	}
	// 任意字面量（未由注册表签发）：拒绝。
	m.ensureRecovery("t-mt", "literal-not-issued", true)
	if rec.resumeCalls != 0 {
		t.Fatalf("literal token must be rejected, resumes=%d", rec.resumeCalls)
	}
	// 当前 tombstone T2（allowNoRuntime）：实际执行 Resume（恢复闭环）。
	m.ensureRecovery("t-mt", t2, true)
	if rec.resumeCalls != 1 {
		t.Fatalf("current tombstone T2 must execute Resume, resumes=%d", rec.resumeCalls)
	}
	got, has, err := adapter.GetTaskBinding(ctx, "t-mt")
	if err != nil || !has || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding continuity: %+v has=%v err=%v", got, has, err)
	}

	// v1 回调形态（allowNoRuntime=false，rt==nil）：保持原拒绝语义。
	rec.resumeCalls = 0
	m.ensureRecovery("t-mt", t2, false)
	if rec.resumeCalls != 0 {
		t.Fatalf("v1 callback form (rt==nil) must keep rejection semantics, resumes=%d", rec.resumeCalls)
	}

	// 已注册新代且代际失配：skip（stale 守卫方向不回归）。
	m.setRuntime("t-mt", m.newRuntime("t-mt")) // 新代 token ≠ t2
	m.ensureRecovery("t-mt", t2, true)
	if rec.resumeCalls != 0 {
		t.Fatalf("stale token against registered newer generation must skip, resumes=%d", rec.resumeCalls)
	}
}

// TestStage4_R4_I10_HandoverAfterDisconnectReconnect 落实 Round4 I10②：候选
// 握手后、正式接管前发生断线+重连——流移交按当前连接代整理待派发信号，首次
// ready 不依赖「期间从未断线」（接管成功、不二次等 ready 预算）。
func TestStage4_R4_I10_HandoverAfterDisconnectReconnect(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-dr", "ses_keep", "active")
	h.allowConns = 2
	h.dropConn = make(chan struct{})

	binding, has, err := h.adapter.GetTaskBinding(ctx, "t-dr")
	if err != nil || !has {
		t.Fatalf("binding: %v", err)
	}
	warmRealStreamHarness(t, h)
	es, serr := h.prov.Events().Subscribe(ctx, binding)
	if serr != nil {
		t.Fatalf("Subscribe: %v", serr)
	}
	connected := make(chan struct{}, 4)
	reconnected := make(chan struct{}, 4)
	es.SetCallbacks(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	},
		func() {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		})
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("handshake connected never fired (conns=%d)", h.conns.Load())
	}

	// 服务端主动断连 → 客户端重连（conn2 建立；旧消费方队列可能因无
	// onDisconnected 回调而滞留——移交对账由 SetCallbacks 承担）。
	close(h.dropConn)
	deadline := time.Now().Add(5 * time.Second)
	for h.conns.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("client never reconnected after server drop")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 正式接管（复用候选流）：移交按连接代前置 connected——ready 即达。
	row, err := h.adapter.GetTask(ctx, "t-dr")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if terr := h.m.commitV2RuntimeReady(ctx, "t-dr", row, h.port, "pw", binding, AlignModeRepo, es, true); terr != nil {
		t.Fatalf("takeover after reconnect: %v", terr)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("ready must be delivered by handover replay, elapsed=%v", elapsed)
	}
	if n := h.conns.Load(); n != 2 {
		t.Fatalf("event connections=%d（断线重连恰一次重连，无重订阅）", n)
	}
}

// TestStage4_R4_I16_LockFailClosesCandidateStream 落实 Round4 I16：取锁失败出口
// 同样未移交候选流——调用链默认持有关闭责任（流关闭+服务端观测断连）。
func TestStage4_R4_I16_LockFailClosesCandidateStream(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-lk", "ses_keep", "active")
	warmRealStreamHarness(t, h)

	binding, has, err := h.adapter.GetTaskBinding(ctx, "t-lk")
	if err != nil || !has {
		t.Fatalf("binding: %v", err)
	}
	es, serr := h.prov.Events().Subscribe(ctx, binding)
	if serr != nil {
		t.Fatalf("Subscribe: %v", serr)
	}
	connected := make(chan struct{}, 4)
	es.SetCallbacks(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	}, nil)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatalf("handshake connected never fired")
	}

	// 占住任务锁（tryLockTask 快失败语义——避免 lockTaskForRecovery 30s 等待）
	// → 接管守卫取锁失败。
	unlock, lerr := h.m.tryLockTask("t-lk")
	if lerr != nil {
		t.Fatal(lerr)
	}
	row, err := h.adapter.GetTask(ctx, "t-lk")
	if err != nil {
		t.Fatal(err)
	}
	terr := h.m.commitV2RuntimeReady(ctx, "t-lk", row, h.port, "pw", binding, AlignModeRepo, es, false)
	unlock()
	var pce *postCommitTakeoverError
	if !errors.As(terr, &pce) {
		t.Fatalf("lock failure must be post-commit classified, got %v", terr)
	}
	select {
	case _, ok := <-es.Events():
		if ok {
			t.Fatalf("candidate stream must be closed on lock failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("candidate stream was not closed after lock failure")
	}
	select {
	case <-h.evtDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("server must observe candidate stream disconnect")
	}
}

// TestStage4_I16 前置：上游 server 冷启动（proc fake 真实监听——未预热时候选
// 订阅只会打到未绑定端口）。
func warmRealStreamHarness(t *testing.T, h *realStreamHarness) {
	t.Helper()
	sup := h.prov.Supervisor()
	warmer, ok := sup.(appagent.ServerAvailabilityWarmer)
	if !ok {
		t.Fatalf("supervisor must implement ServerAvailabilityWarmer")
	}
	if err := warmer.CheckServerAlive(context.Background()); err != nil {
		t.Fatalf("warmup: %v", err)
	}
}

// ---- 收尾 Round 1：K2 畸形列表页不放行任务对账 ----

// TestR1_K2_MalformedListKeepsOwnership_RealBridge 落实收尾 K2：生产桥
// （真实 SPIProvider → spiBridgeOCClient → opencode2.Client.ListSessions）在
// 上游返回 HTTP 200 畸形列表页时报错——任务对账 MUST 保留既有归属行
// （MUST NOT 把畸形响应当完整空快照进 DeleteAbsentSessions）。
func TestR1_K2_MalformedListKeepsOwnership_RealBridge(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-k2", "ses_keep", "active")
	// 先用合法页完成 serve warmup（畸形页会让就绪验证本身失败——见 K2 语义），
	// 再切换为畸形页，隔离「对账路径」的断言。
	warmRealStreamHarness(t, h)
	h.listPageBody = `{}`
	// 既有 v2 归属行（对齐若误删即红）。
	if _, err := h.db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
		 VALUES ('t-k2','ses_keep',1,1,1,?,?)`,
		ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
		t.Fatal(err)
	}
	// 生产桥：buildTaskOCClient 经 bridgeClientFor 返回真实 provider 的 SPI 桥。
	oc, err := h.m.buildTaskOCClient(ctx, "t-k2", appagent.Options{HealthTimeout: time.Second})
	if err != nil {
		t.Fatalf("build bridge client: %v", err)
	}
	if err := h.m.alignSessions(ctx, "t-k2", "/wt", oc, AlignModeRepo); err == nil {
		t.Fatalf("malformed list page MUST surface as align error (protocol violation)")
	} else if !strings.Contains(err.Error(), "list sessions") || strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected protocol violation error, got %v", err)
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-k2' AND session_id='ses_keep'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ownership row must survive protocol error, n=%d", n)
	}
}

// TestR1_K2_LegalEmptySnapshotDeletesAbsent 落实收尾 K2 对照面：同一生产桥
// 链路上「合法空快照」仍按既有语义删除缺席归属行（协议校验不得把合法页也拒掉，
// 否则对账失去删除能力）。
func TestR1_K2_LegalEmptySnapshotDeletesAbsent(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-k2b", "ses_keep", "active")
	h.listPageBody = `{"data":[],"cursor":{"previous":null,"next":null}}`
	warmRealStreamHarness(t, h)
	if _, err := h.db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, provider_id, provider_instance_id)
		 VALUES ('t-k2b','ses_keep',1,1,1,?,?)`,
		ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
		t.Fatal(err)
	}
	oc, err := h.m.buildTaskOCClient(ctx, "t-k2b", appagent.Options{HealthTimeout: time.Second})
	if err != nil {
		t.Fatalf("build bridge client: %v", err)
	}
	if err := h.m.alignSessions(ctx, "t-k2b", "/wt", oc, AlignModeRepo); err != nil {
		t.Fatalf("legal empty page align: %v", err)
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-k2b'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("legal empty snapshot must delete absent rows, n=%d", n)
	}
}

// ---- 收尾 Round 1：K1 首建创建结果未知不误清占位 ----

// TestR1_K1_UnknownCreate_KeepsGate_RealProvider 落实收尾 Round1 K1：真实
// provider + 公开 Activate 入口——上游已创建 session 但连接在响应前断开 /
// 响应截断不可解码（CreateSession 结果未知）时，MUST NOT 把「无 typed debt」
// 当作「candidate 未产生」清占位：首次失败后重试与重建 Manager 后均保持占位、
// 零新增 POST、无替代 session。
func TestR1_K1_UnknownCreate_KeepsGate_RealProvider(t *testing.T) {
	cases := []struct {
		name  string
		fault func(w http.ResponseWriter, r *http.Request, createdID string) bool
	}{
		{
			name: "connection closed before response",
			fault: func(w http.ResponseWriter, r *http.Request, _ string) bool {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("hijacker unavailable")
					return false
				}
				conn, _, herr := hj.Hijack()
				if herr != nil {
					t.Errorf("hijack: %v", herr)
					return false
				}
				_ = conn.Close() // 已创建 session，但响应前断开
				return true
			},
		},
		{
			name: "truncated undecodable response",
			fault: func(w http.ResponseWriter, _ *http.Request, createdID string) bool {
				w.Header().Set("Content-Type", "application/json")
				// 已写一半响应体（JSON 不完整——无法解码，也无可靠 ID）。
				_, _ = w.Write([]byte(`{"data":{"id":"` + createdID + `"`))
				return true
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newRealStreamHarness(t, "t-k1", "ses_keep", "active")
			// 清掉 harness 自种的 v2 任务行，改为 v2 钉扎 suspended 首建形态。
			if _, err := h.db.Exec(`DELETE FROM tasks WHERE id='t-k1'`); err != nil {
				t.Fatal(err)
			}
			seedBranchBTask(t, h.db, "t-k1")
			if _, err := h.db.Exec(`UPDATE tasks SET provider_id=? WHERE id='t-k1'`, ocdeckstore.ProviderIDV2); err != nil {
				t.Fatal(err)
			}
			warmRealStreamHarness(t, h)
			h.createFault = tc.fault

			berr := h.m.Activate(ctx, "t-k1")
			if berr == nil {
				t.Fatalf("unknown create result must fail the activation")
			}
			if n := h.created.Load(); n != 1 {
				t.Fatalf("expected exactly one POST attempt, got %d", n)
			}
			row, err := h.adapter.GetTask(ctx, "t-k1")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(row.Notice.String, candidateIntentSessionPending) {
				t.Fatalf("placeholder must be retained on unknown create result: %q", row.Notice.String)
			}
			if strings.Contains(row.Notice.String, `"sessionName":"ses_`) {
				t.Fatalf("no candidate identity may be fabricated: %q", row.Notice.String)
			}
			// 重试：门禁拒绝、零新增 POST。
			if aerr := h.m.Activate(ctx, "t-k1"); aerr == nil ||
				!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
				t.Fatalf("retry must stay gated, got %v", aerr)
			}
			if n := h.created.Load(); n != 1 {
				t.Fatalf("gated retry must not POST, POSTs=%d", n)
			}
			// 重建 Manager（模拟进程重启，内存态清空）后同样零 POST。
			m2 := newTestManagerFull(t, h.adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
			m2.SetLifecycleCtx(ctx)
			m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
			reg2 := appagent.NewRegistry()
			if err := reg2.Register(newTestRegistryProvider()); err != nil {
				t.Fatal(err)
			}
			if err := reg2.Register(h.prov); err != nil {
				t.Fatal(err)
			}
			m2.providerRegistry = reg2
			if aerr := m2.Activate(ctx, "t-k1"); aerr == nil ||
				!strings.Contains(aerr.Error(), "uncleaned cleanup debt") {
				t.Fatalf("cross-restart gate must hold, got %v", aerr)
			}
			if n := h.created.Load(); n != 1 {
				t.Fatalf("cross-restart must be zero-POST, POSTs=%d", n)
			}
			// 无替代 session：任务无 binding / 无归属行。
			if _, has, berr := h.adapter.GetTaskBinding(ctx, "t-k1"); berr != nil || has {
				t.Fatalf("no binding may be committed: has=%v err=%v", has, berr)
			}
			var nsess int
			if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-k1'`).Scan(&nsess); err != nil {
				t.Fatal(err)
			}
			if nsess != 0 {
				t.Fatalf("no ownership row may be claimed, n=%d", nsess)
			}
		})
	}
}

// ---- 收尾：v2 重激活同代快照持久化（env snapshot missing 修复） ----

// TestR1_ResumeV2_RepersistsEnvSnapshot 落实生产 bug 回归（Docker 实测：全新
// 任务重激活死于「resolve session env: env snapshot missing」）：Suspend 清
// env 快照而 binding 保留（R4 I17）→ 重激活 resumeActivateV2 MUST 在
// Resume(origin=created, recovery=true) 之前重建同代快照——不预置 env_snapshot
// 的真实 store + 真实 provider harness，两轮 Activate 均成功且快照落盘。
func TestR1_ResumeV2_RepersistsEnvSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-env9", "ses_keep", "suspended")
	// 模拟 suspend 后形态：env 快照缺失（binding origin=created 保留）。
	if _, err := h.db.Exec(`UPDATE tasks SET env_snapshot='', base_ref='refs/heads/main' WHERE id='t-env9'`); err != nil {
		t.Fatal(err)
	}
	warmRealStreamHarness(t, h)

	activate := func(round int) {
		if aerr := h.m.Activate(ctx, "t-env9"); aerr != nil {
			if strings.Contains(aerr.Error(), "env snapshot missing") {
				t.Fatalf("round %d: resume must repersist env snapshot before Resume, got %v", round, aerr)
			}
			t.Fatalf("round %d Activate: %v", round, aerr)
		}
		row, err := h.adapter.GetTask(ctx, "t-env9")
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != "active" {
			t.Fatalf("round %d status=%s", round, row.Status)
		}
		if !row.EnvSnapshot.Valid || row.EnvSnapshot.String == "" {
			t.Fatalf("round %d env snapshot must be persisted", round)
		}
		binding, has, berr := h.adapter.GetTaskBinding(ctx, "t-env9")
		if berr != nil || !has || binding.UpstreamSessionID != "ses_keep" || binding.ProviderID != ocdeckstore.ProviderIDV2 {
			t.Fatalf("round %d binding continuity: %+v has=%v err=%v", round, binding, has, berr)
		}
	}

	// 第 1 轮：快照缺失 → 重激活重建（修复面）。
	activate(1)
	// 收敛第 1 轮运行面（释放 /api/event 连接），模拟进程重启后以全新 Manager
	// 走第 2 轮（连接计数随之放开 1→2）。
	h.m.clearRuntime("t-env9")
	h.allowConns = 2
	m2 := newTestManagerFull(t, h.adapter, nil, newMockProc(), newMockWorktree(), h.oc)
	m2.SetLifecycleCtx(ctx)
	m2.SetMachineFacts(machineFacts{MachineMajor: 2, V2Enabled: true})
	reg2 := appagent.NewRegistry()
	if err := reg2.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(h.prov); err != nil {
		t.Fatal(err)
	}
	m2.providerRegistry = reg2
	// 第 2 轮：再次清空（模拟又一轮 suspend 清除）→ 重激活再次重建（周期形态）。
	if _, err := h.db.Exec(`UPDATE tasks SET env_snapshot='', status='suspended', base_ref='refs/heads/main' WHERE id='t-env9'`); err != nil {
		t.Fatal(err)
	}
	h.m = m2
	activate(2)
}

// ---- 生产装配形态（本批次 A/B）：v2 全链路零 v1 wire 路径命中 ----

// useProdDispatchFactory 把 harness 装配切换为生产分派工厂形态（与
// cmd/ocdeck-server 装配同形）：registry 注册 v1 minimal + 真实 v2 SPI
// provider，ocFactory = NewRegistryDispatchFactory（对已注册 provider 恒返回
// v1 wire client、未注册 fail-closed）——消除「mock factory 恒零 v1 命中」
// 的验证盲区：v2 全链路必须经桥 SPI 窄接口真实命中 /api/* 端点。
func (h *realStreamHarness) useProdDispatchFactory(t *testing.T) {
	t.Helper()
	reg := appagent.NewRegistry()
	if err := reg.Register(newTestRegistryProvider()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(h.prov); err != nil {
		t.Fatal(err)
	}
	h.m.providerRegistry = reg
	h.m.ocFactory = NewRegistryDispatchFactory(reg)
}

// TestProdAssembly_V2ResumeZeroV1PathHits 落实生产装配形态回归（A）：生产
// ocFactory = NewRegistryDispatchFactory（对已注册 provider 恒返回 v1 wire
// client）+ 真实 opencode2 SPI provider + startSSE 桥接优先。fake server 对
// v1 形态路径（/session、/event、/session/status、/global/health——无 /api
// 前缀）一律回 SPA HTML（模拟真实 v2 serve 未匹配路径回退，容器实测形态）。
// 断言：v2 任务经公开 Activate（已有 v2 binding → resumeActivateV2）激活后
// binding/状态连续，v1 形态路径零命中，且对齐/SSE 正命中预期 /api/* 端点
// （正断言：桥接真实承载，非仅「零 v1 命中」；变异——移除 startSSE 桥接 →
// v1 wire client 打 /session 被 SPA 回退吞掉 → 对齐失败/命中计数红）。
func TestProdAssembly_V2ResumeZeroV1PathHits(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-prod", "ses_keep", "suspended")
	h.useProdDispatchFactory(t)
	if aerr := h.m.Activate(ctx, "t-prod"); aerr != nil {
		t.Fatalf("Activate: %v", aerr)
	}
	if n := h.v1Hits.Load(); n != 0 {
		t.Fatalf("v2 full activation must not hit v1 wire paths, hits=%d", n)
	}
	// 正断言：对齐列表与 SSE 事件流经桥真实命中 /api/* 端点。
	if !h.apiHit("GET", "/api/session") {
		t.Fatalf("align must hit GET /api/session via bridge, api=%v", h.apiPaths)
	}
	if !h.apiHit("GET", "/api/event") {
		t.Fatalf("SSE takeover must hit GET /api/event via bridge, api=%v", h.apiPaths)
	}
	got, has, err := h.adapter.GetTaskBinding(ctx, "t-prod")
	if err != nil || !has || got.ProviderID != ocdeckstore.ProviderIDV2 || got.UpstreamSessionID != "ses_keep" {
		t.Fatalf("binding continuity: %+v has=%v err=%v", got, has, err)
	}
	row, err := h.adapter.GetTask(ctx, "t-prod")
	if err != nil || row.Status != "active" {
		t.Fatalf("status=%s err=%v", row.Status, err)
	}
}

// TestProdAssembly_V2DeleteViaSharedServe 落实生产装配形态回归（B）：v2 任务
// 删除经共享 serve bridge（provider Sessions().Delete → DELETE /api/session/{id}），
// MUST NOT 请求临时 runtime（生产实测 acquire_ephemeral capability_unavailable
// 事故面），v1 wire 路径零命中且 DELETE 端点正命中；oc session 删除成功后归属
// 行落账、任务删除收敛。任务行 suspended + binding/归属行已提交（挂起后删除的
// 主形态；删除只消费持久化事实，不需要 Activate）。
func TestProdAssembly_V2DeleteViaSharedServe(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-prod-del", "ses_keep", "suspended")
	h.useProdDispatchFactory(t)
	if _, err := h.db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id, provider_id, provider_instance_id)
		 VALUES ('t-prod-del','ses_keep',1,1,1,'',?,?)`, ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Delete(ctx, "t-prod-del", DeleteNormal, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := h.v1Hits.Load(); n != 0 {
		t.Fatalf("v2 delete must not hit v1 wire paths (no ephemeral runtime), hits=%d", n)
	}
	if !h.apiHit("DELETE", "/api/session/ses_keep") {
		t.Fatalf("delete must hit DELETE /api/session/ses_keep via shared serve, api=%v", h.apiPaths)
	}
	if _, err := h.adapter.GetTask(ctx, "t-prod-del"); err == nil {
		t.Fatalf("task row must be gone after delete")
	}
}

// TestProdAssembly_V2DeleteMixedIdempotent 落实删除幂等混合矩阵（评审 P1-2）：
// v2 SPI 把 404 映射 ErrKindNotFound（D7），删除编排把 NotFound 与成功同语义
// 落账（已不存在=幂等成功）；服务端故障项（500）聚合报错并保留归属行。
// 断言：204/404 项删除落账、500 项归属行保留、Delete 报错含故障项。
func TestProdAssembly_V2DeleteMixedIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newRealStreamHarness(t, "t-prod-mix", "ses_keep", "suspended")
	h.useProdDispatchFactory(t)
	h.deleteStatus = map[string]int{"ses_gone": http.StatusNotFound, "ses_bad": http.StatusInternalServerError}
	for _, id := range []string{"ses_keep", "ses_gone", "ses_bad"} {
		if _, err := h.db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id, provider_id, provider_instance_id)
			 VALUES ('t-prod-mix',?,1,1,1,'',?,?)`, id, ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
			t.Fatal(err)
		}
	}
	derr := h.m.Delete(ctx, "t-prod-mix", DeleteNormal, true)
	if derr == nil || !strings.Contains(derr.Error(), "ses_bad") {
		t.Fatalf("server-fault item must fail the delete with per-item error, got: %v", derr)
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-prod-mix' AND session_id IN ('ses_keep','ses_gone')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deleted items must be settled (204+404 idempotent), rows=%d err=%v", n, err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-prod-mix' AND session_id='ses_bad'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("server-fault item must keep its ownership row, rows=%d err=%v", n, err)
	}
}

// TestProdAssembly_V2Delete_OwnershipGateBlocksForeignBinding 落实删除归属门禁
// （评审 P1-3）：损坏/串任务 binding（错 taskId/错 directory/错 TerminalRef）
// 在任何 provider 调用与 warmer 之前被 BindingOwnershipChecker 拒绝——fake
// server 零请求（无进程/凭据/网络副作用），归属行保留。
func TestProdAssembly_V2Delete_OwnershipGateBlocksForeignBinding(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(taskID string, b *domain.TaskBinding)
	}{
		{"foreign taskId", func(taskID string, b *domain.TaskBinding) {
			b.Data = []byte(fmt.Sprintf(`{"port":%d,"directory":"/wt","taskId":"t-evil","origin":"created","auth":{"kind":"derived"}}`, 1))
		}},
		{"foreign directory", func(taskID string, b *domain.TaskBinding) {
			b.Data = []byte(fmt.Sprintf(`{"port":%d,"directory":"/elsewhere","taskId":"%s","origin":"created","auth":{"kind":"derived"}}`, 1, taskID))
		}},
		{"foreign TerminalRef", func(taskID string, b *domain.TaskBinding) {
			b.TerminalRef = "ocdeck-v2-tui-t-evil"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRealStreamHarness(t, "t-gate-del", "ses_keep", "suspended")
			h.useProdDispatchFactory(t)
			if _, err := h.db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id, provider_id, provider_instance_id)
				 VALUES ('t-gate-del','ses_keep',1,1,1,'',?,?)`, ocdeckstore.ProviderIDV2, ocdeckstore.ProviderInstanceV2Default); err != nil {
				t.Fatal(err)
			}
			got, has, err := h.adapter.GetTaskBinding(ctx, "t-gate-del")
			if err != nil || !has {
				t.Fatalf("seed binding: %+v has=%v err=%v", got, has, err)
			}
			tc.mutate("t-gate-del", &got)
			raw, err := got.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Exec(`UPDATE tasks SET task_binding=? WHERE id='t-gate-del'`, raw); err != nil {
				t.Fatal(err)
			}
			if derr := h.m.Delete(ctx, "t-gate-del", DeleteNormal, true); derr == nil {
				t.Fatalf("tampered binding must be rejected by ownership gate")
			}
			h.apiMu.Lock()
			if len(h.apiPaths) != 0 {
				h.apiMu.Unlock()
				t.Fatalf("ownership gate must run before any provider call, api=%v", h.apiPaths)
			}
			h.apiMu.Unlock()
			if n := h.v1Hits.Load(); n != 0 {
				t.Fatalf("no v1 wire side effects allowed, hits=%d", n)
			}
			var rows int
			if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-gate-del'`).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("ownership row must be kept on gate rejection, rows=%d err=%v", rows, err)
			}
		})
	}
}

// TestStage4_Bridge_V2_RawFacesFailClosed 落实桥内 v2 fail-closed（生产装配
// 修复）：v2 binding 的桥 SubscribeEvents/Health/Probe MUST 拒绝（typed
// EventSource 是 v2 唯一事件面；fallback 的 v1 wire 形态对 v2 serve 均被 SPA
// 回退吞掉——静默委托会产生假 ready/假健康/假事件面）。
func TestStage4_Bridge_V2_RawFacesFailClosed(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedV2ActiveTaskUpstream(t, db, "t-brc", "ses_main")
	prov := &suspendV2Provider{sup: &suspendV2Sup{}, sessions: stubSessions{}}
	m := newSuspendManager(t, db, prov)
	binding := v2BindingFor("t-brc", "ses_main")
	data := &opencodeBindingData{ProviderID: ocdeckstore.ProviderIDV2, Port: 4096, Directory: "/wt", TaskID: "t-brc"}
	bridge := m.bridgeClientFor(data, binding, m.ocFactory(ocdeckstore.ProviderIDV2, 4096, "pw", appagent.Options{}))
	if bridge == nil {
		t.Fatalf("bridge must be constructed for provider with capability exits")
	}
	err = bridge.SubscribeEvents(context.Background(), "/wt", func(appagent.Event) {}, func() {})
	if appagent.KindOf(err) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("v2 bridge SubscribeEvents must fail closed (capability_unavailable), got: %v", err)
	}
	if _, herr := bridge.Health(context.Background()); appagent.KindOf(herr) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("v2 bridge Health must fail closed, got: %v", herr)
	}
	if _, perr := bridge.Probe(context.Background()); appagent.KindOf(perr) != appagent.ErrKindCapabilityUnavailable {
		t.Fatalf("v2 bridge Probe must fail closed, got: %v", perr)
	}
}

// ---- 跨任务偷 candidate 事故回归（typed created 不认领） ----

// TestV2_TypedCreated_NeverClaims 落实跨任务偷 candidate 事故修复：v2 SSE 是
// 共享 serve 的全局事件流（归属键仅 data.sessionID，envelope location 可选且
// 未实证），他任务首建 candidate 的 session.created 广播会到达任何活跃任务的
// typed 流——MUST NOT claim（v2 归属唯一合法来源 = 原子提交协议 + 对齐）。
// 三形态（容器事故无 location / location 缺 directory / 同目录他任务 session）
// 均零归属新增；随后 B 任务对同一 candidate 的原子首建提交成功（事故闭环：
// 修复前 A 已偷 → CommitFirstBuild 守卫 ErrAdoptionConflict → 候选被误回收、
// 激活失败，容器实测）。丢弃判定位于 handleAgentEvent EventSessionCreated
// 公共入口（lifecycle/legacy 两条落库分支分叉之前）——真实 harness 即事故链
// 形态（legacy 落库分支）。
func TestV2_TypedCreated_NeverClaims(t *testing.T) {
	const candidate = "ses_ea8bee95"
	ctx := context.Background()
	// 任务 A：v2 binding 挂起（公开 Activate 激活 → active，typed 流在位）。
	h := newRealStreamHarness(t, "t-vsteal", "ses_a", "suspended")
	if aerr := h.m.Activate(ctx, "t-vsteal"); aerr != nil {
		t.Fatalf("Activate A: %v", aerr)
	}
	// 与适配层翻译产物同构的 typed created 事件（translateV2Event 只看
	// data.sessionID——envelope 三形态差异不影响 AgentEvent 形状，翻译层事实
	// 由 opencode2 events_test 记录；此处断言 task 层不认领）。
	stealEvent := func() domain.AgentEvent {
		sess := domain.AgentSession{
			Key:       domain.SessionKey{ProviderID: ocdeckstore.ProviderIDV2, ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default, UpstreamID: candidate},
			CreatedAt: 100, UpdatedAt: 100,
		}
		ev, err := domain.NewSessionEvent(domain.EventSessionCreated, sess)
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	for i := 0; i < 3; i++ {
		if err := h.m.handleAgentEvent(ctx, "t-vsteal", stealEvent()); err != nil {
			t.Fatalf("typed created must be dropped without error, round %d: %v", i, err)
		}
	}
	var stolen int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM task_sessions WHERE task_id='t-vsteal' AND session_id=?`, candidate).Scan(&stolen); err != nil {
		t.Fatal(err)
	}
	if stolen != 0 {
		t.Fatalf("A must not claim cross-task candidate via typed created, rows=%d", stolen)
	}
	// 事故闭环：B 对同一 candidate 的原子首建提交成功（修复前被 A 偷走
	// → 守卫 ErrAdoptionConflict → candidate 回收、激活失败）。
	now := time.Now().Unix()
	if _, err := h.db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p-t-b', 'proj', '/repo', 'main', ?, 'repo')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, worktree_path,
		 provider_id, created_at, updated_at)
		 VALUES ('t-b','p-t-b','task-b','b','activating','/data/worktrees/other-b',?,?,?)`,
		ocdeckstore.ProviderIDV2, now, now); err != nil {
		t.Fatal(err)
	}
	bBinding := v2BindingFor("t-b", candidate)
	bKey := domain.SessionKey{ProviderID: ocdeckstore.ProviderIDV2, ProviderInstanceID: ocdeckstore.ProviderInstanceV2Default, UpstreamID: candidate}
	if _, err := h.db.CommitFirstBuild(ctx, "t-b", bBinding, bKey, 100, 100, 100, ""); err != nil {
		t.Fatalf("B first build commit must succeed (candidate not stolen by A): %v", err)
	}
}
