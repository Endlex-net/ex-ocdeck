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

// seedAdoptAFormTask 种子承接 A 形态任务（真实 store）：v1 binding(sess-keep)@v1 +
// anchor@v1 + 归属行（父+子）+ env 快照事实（承接失败保留断言用）。pinned 可指定
// （承接 A 为 v1；R5 用 v2）。
func seedAdoptAFormTask(t *testing.T, db *store.DB, taskID, pinned string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO projects (id, name, path, default_branch, created_at, kind)
		 VALUES ('p1','proj','/repo','main',?,'repo')`, now); err != nil {
		t.Fatal(err)
	}
	v1Raw, err := domain.TaskBinding{
		Version: domain.TaskBindingVersion, ProviderID: ocdeckstore.ProviderIDV1,
		ProviderInstanceID: ocdeckstore.ProviderInstanceV1Default,
		UpstreamSessionID:  "sess-keep", TerminalRef: "tui",
		Data: []byte(`{"port":4000,"directory":"/wt","taskId":"` + taskID + `","auth":{"kind":"derived"}}`),
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, project_id, name, branch, status, mode, worktree_path,
		 anchor_session_id, anchor_provider_id, anchor_provider_instance_id, provider_id, task_binding, env_snapshot, created_at, updated_at)
		 VALUES (?,'p1','task','b','suspended','worktree','/wt','sess-keep','opencode-v1','default',?,?,?, ?, ?)`,
		taskID, pinned, v1Raw, `{"OCDECK_TASK_ID":"`+taskID+`"}`, now, now); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"sess-keep", "sess-child"} {
		if _, err := db.Exec(`INSERT INTO task_sessions (task_id, session_id, session_created_at, first_seen_at, last_seen_at, parent_id)
			 VALUES (?, ?, 1, 1, 1, ?)`, taskID, sid, map[bool]string{true: "", false: "sess-keep"}[sid == "sess-keep"]); err != nil {
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
	// onSpawn 在 serve 单元（重新）绑定时回调——模拟「新进程存活」（身份核验
	// 依据由测试侧持有，测试经此复活 ProcessIdentity 事实）。
	onSpawn func()
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
	p.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
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
	seedAdoptAFormTask(t, db, "t-stop", ocdeckstore.ProviderIDV1)
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
	seedAdoptAFormTask(t, db, "t-race", ocdeckstore.ProviderIDV1)
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
	seedAdoptAFormTask(t, db, "t-a1", ocdeckstore.ProviderIDV1)
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
	seedAdoptAFormTask(t, db, "t-a2", ocdeckstore.ProviderIDV1)
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
	seedAdoptAFormTask(t, db, "t-a3", ocdeckstore.ProviderIDV1)
	if _, err := db.Exec(`UPDATE tasks SET status='active' WHERE id='t-a3'`); err != nil {
		t.Fatal(err)
	}
	failSup := &failResumeV2Sup{}
	m := newAdoptATestManager(t, db, &supOnlyV2Provider{sup: failSup})
	// ensureRecovery 前置校验要求注册表 runtime + token 匹配（watcher 语义）。
	rt := m.newRuntime("t-a3")
	m.setRuntime("t-a3", rt)

	m.ensureRecovery("t-a3", rt.instVersion)
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
	seedAdoptAFormTask(t, db, "t-h1", ocdeckstore.ProviderIDV2)
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
}

func newRealStreamHarness(t *testing.T, taskID, upstream, status string) *realStreamHarness {
	t.Helper()
	t.Setenv(appagent.MasterSecretEnv, "test-master-secret")
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &realStreamHarness{db: db, evtDone: make(chan struct{})}
	var evtOnce sync.Once
	h.proc = newMonitorProcSeam(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/openapi.json":
			_, _ = w.Write([]byte(`{"components":{"schemas":{"Session.ID":{"type":"string","pattern":"^ses"},"Session.Message.ID":{"type":"string","pattern":"^msg_"}}}}`))
		case r.URL.Path == "/api/event":
			if h.conns.Add(1) >= 2 {
				// 接管 MUST 复用候选流——第二次订阅即失败信号（重订阅即红）。
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("data: {\"id\":\"h\",\"created\":1,\"type\":\"server.connected\",\"data\":{\"clientID\":\"cli_x\"}}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // 客户端断开（Close）→ return 产生 EOF
			evtOnce.Do(func() { close(h.evtDone) })
		case r.URL.Path == "/api/session/"+upstream:
			_, _ = w.Write([]byte(`{"data":{"id":"` + upstream + `","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
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
	h.m = newTestManagerFull(t, h.adapter, nil, newMockProc(), newMockWorktree(), newMockOC(true))
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
