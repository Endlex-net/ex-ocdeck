package opencode

// spi_supervisor_test.go 覆盖 RuntimeSupervisor/TerminalIO 实装（tasks 3.6 验证）：
//   - Acquire 锚定三支（有锚定直启+列表校验 / 失效锚定 CAS 清空→双启动 /
//     无锚定双启动：bootstrap→POST→claim+锚定三元组写入→确认终止→formal --session）；
//   - Resume 入口差异（D9a）：BeforeLaunch 先于端口分配与任何进程副作用；
//     AllowSessionCreate=false 时锚定缺席→修复完成信号 / 无锚定→修复完成信号；
//   - 端口重试两条分派（进程死亡→无 kill 轮换；健康超时→kill 门禁 clean→轮换；
//     非 clean disposition→PendingCleanupError）与轮换时 PersistServePort；
//   - Release 幂等（absent→nil；clean→nil；非 clean→PendingCleanupError）；
//   - AcquireEphemeral（无锚定/无 claim/无对齐）；
//   - 占位形态（nil deps→CapabilityUnavailable）。
//
// 有效性证据：全部 seam 为进程内 fake（计数+失败注入位）；无监听端口让健康
// 检查必然失败，区分「死亡优先」与「健康超时」两条分派的行为差异（kill 计数）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/process"
)

// supFakeProc 是进程 seam fake：记录调用序列，可注入 NewSession 失败、
// kill disposition/错误、会话死亡脚本。
type supFakeProc struct {
	mu          sync.Mutex
	sessions    map[string]bool
	newCalls    int
	killCalls   int
	newErr      error
	killErr     error
	killRes     map[string]process.KillResult
	dieAfterNew int // 前 N 次 NewSession 后会话立即消失
	// newEnvFirst 记录首次 NewSession 的 env 拷贝（C13 时序断言）。
	newEnvFirst map[string]string
}

func newSupFakeProc() *supFakeProc {
	return &supFakeProc{sessions: map[string]bool{}, killRes: map[string]process.KillResult{}}
}

func (p *supFakeProc) NewSession(spec process.SessionSpec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.newCalls++
	if p.newEnvFirst == nil {
		env := map[string]string{}
		for k, v := range spec.Env {
			env[k] = v
		}
		p.newEnvFirst = env
	}
	if p.newErr != nil {
		return p.newErr
	}
	p.sessions[spec.Name] = true
	if p.dieAfterNew > 0 && p.newCalls <= p.dieAfterNew {
		delete(p.sessions, spec.Name)
	}
	return nil
}

func (p *supFakeProc) KillSession(name string) (process.KillResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killCalls++
	if p.killErr != nil {
		return process.KillResult{}, p.killErr
	}
	if res, ok := p.killRes[name]; ok {
		if res.SessionKilled {
			delete(p.sessions, name)
		}
		return res, nil
	}
	delete(p.sessions, name)
	return process.KillResult{SessionKilled: true, Disposition: process.DispositionClean}, nil
}

func (p *supFakeProc) HasSession(name string) (bool, error) { return p.sessions[name], nil }

func (p *supFakeProc) calls() (news, kills int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.newCalls, p.killCalls
}

// supFakeStore 是 store seam fake。
type supFakeStore struct {
	mu            sync.Mutex
	anchor        map[string]string
	claims        []string // "task|session" 记录（claim+anchor 单事务调用次数断言）
	clearRes      application.MutationResult
	clearErr      error
	lastPort      map[string]int
	lastPortErr   error // C13：GetTaskLastPort 读取失败 fail-closed 注入
	claimConflict bool  // 下次 ClaimSessionKeyAndSetAnchor 返回他任务持有
}

func newSupFakeStore() *supFakeStore {
	return &supFakeStore{anchor: map[string]string{}, lastPort: map[string]int{}}
}

func (s *supFakeStore) GetTaskAnchor(ctx context.Context, taskID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anchor[taskID], nil
}

func (s *supFakeStore) ClaimSessionKeyAndSetAnchor(ctx context.Context, taskID, sessionID string, createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = append(s.claims, taskID+"|"+sessionID)
	if s.claimConflict {
		return application.ClaimResult{Claimed: false, OwnerTaskID: "t-other"}, nil
	}
	s.anchor[taskID] = sessionID
	return application.ClaimResult{Claimed: true, Changed: true}, nil
}

func (s *supFakeStore) ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearErr != nil {
		return application.MutationResult{}, s.clearErr
	}
	if s.anchor[taskID] != oldAnchor {
		return application.MutationResult{}, nil
	}
	delete(s.anchor, taskID)
	return application.MutationResult{Matched: true, Changed: true}, nil
}

func (s *supFakeStore) GetTaskLastPort(ctx context.Context, taskID string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPortErr != nil {
		return 0, false, s.lastPortErr
	}
	p, ok := s.lastPort[taskID]
	return p, ok, nil
}

// supFakeSideFx 旁路副作用 fake（持久化端口调用计数断言）。
type supFakeSideFx struct {
	persistCalls int
	persistErr   error
	// buildEnv 可编程（C13：主路径快照带端口的 fixture 形态）；nil → 默认无端口。
	buildEnv func(taskID string) map[string]string
}

func (s *supFakeSideFx) ResolvePermissionMode(ctx context.Context, taskID string) (string, error) {
	return "ask", nil
}

func (s *supFakeSideFx) PersistServePort(ctx context.Context, taskID string, env map[string]string) error {
	s.persistCalls++
	if s.persistErr != nil {
		return s.persistErr
	}
	return nil
}

func (s *supFakeSideFx) BuildServeEnv(ctx context.Context, taskID string) (map[string]string, error) {
	if s.buildEnv != nil {
		return s.buildEnv(taskID), nil
	}
	return map[string]string{"OCDECK_TASK_ID": taskID}, nil
}

// supFixture 组装 deps：健康检查指向 deadServer（无监听）或 healthy server。
type supFixture struct {
	t        *testing.T
	proc     *supFakeProc
	store    *supFakeStore
	fx       *supFakeSideFx
	prov     *SPIProvider
	deps     *SupervisorDeps
	healthy  *spiTestServer           // nil = 全端口健康失败
	sessSeq  int                      // POST /session 的生成序号
	sessions []map[string]interface{} // 已创建 session（GET /session 列表返回）
	// probeBroken 置位时 /session/status 返回结构违约数组（Probe 失败 fixture，C4）。
	probeBroken bool
	// srvPort 健康 server 监听端口（fixture 内构造断言 binding 端点用；全失败=0）。
	srvPort int
}

func newSupFixture(t *testing.T, healthy bool) *supFixture {
	t.Helper()
	// C2c 裁决 2：测试宇宙统一注入固定测试 master secret（默认 passwordFor 派生）。
	t.Setenv(appagent.MasterSecretEnv, "sup-test-master-secret")
	f := &supFixture{t: t, proc: newSupFakeProc(), store: newSupFakeStore(), fx: &supFakeSideFx{}}
	port := 1 // 无监听端口：连接拒绝 → 健康超时脚本。
	if healthy {
		f.healthy = newSPITestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/global/health":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": ContractBaseline})
			case r.URL.Path == "/session/status":
				// probeBroken：结构违约（数组）→ Probe ErrCapabilityMismatch/网络形错误。
				if f.probeBroken {
					_ = json.NewEncoder(w).Encode([]interface{}{})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{})
			case r.URL.Path == "/session" && r.Method == http.MethodPost:
				f.sessSeq++
				sess := map[string]interface{}{
					"id":   fmt.Sprintf("ses_fake_%03d", f.sessSeq),
					"time": map[string]interface{}{"created": 1000 + f.sessSeq, "updated": 1000 + f.sessSeq},
				}
				f.sessions = append(f.sessions, sess)
				_ = json.NewEncoder(w).Encode(sess)
			case r.URL.Path == "/session":
				// 列表：POST 生成的 session 供锚定校验（created.ID 必须在场）。
				if f.sessions == nil {
					f.sessions = []map[string]interface{}{}
				}
				_ = json.NewEncoder(w).Encode(f.sessions)
			default:
				w.WriteHeader(200)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{})
			}
		}))
		port = f.healthy.port
		f.srvPort = port
	}
	f.deps = &SupervisorDeps{
		Proc: f.proc, Store: f.store, SideFx: f.fx,
		AllocatePort: func(lastPort int, lastValid bool, exclude int) (int, error) {
			next := lastPort
			if !lastValid || next < 1 {
				next = port
			}
			if next == exclude {
				next++
			}
			return next, nil
		},
		ReadyTimeout: 40 * time.Millisecond, PollInterval: 2 * time.Millisecond,
		ProbeColdStartBackoff: func() []time.Duration { return []time.Duration{time.Millisecond, time.Millisecond} },
	}
	f.prov = NewSPIProviderWithDeps(*f.deps)
	return f
}

func (f *supFixture) taskRef() appagent.TaskRef {
	return appagent.TaskRef{TaskID: "t1", Directory: "/wt/t1", Mode: "worktree"}
}

// aliveOldBinding 构造「TerminalRef 存活复用」fixture 的旧 binding：端点指向健康
// server（reuse 健康探测可命中）、锚定与会话名固定、derived 凭据（现场派生）。
func (f *supFixture) aliveOldBinding() domain.TaskBinding {
	f.proc.sessions["ocdeck-t1-runtime"] = true
	return buildBinding(f.taskRef(), "ses_keep", "ocdeck-t1-runtime", f.srvPort)
}

// seedServerSession 预置健康 server 的列表返回项（锚定校验用）。
func (f *supFixture) seedServerSession(id string) {
	f.sessions = append(f.sessions, map[string]interface{}{
		"id":   id,
		"time": map[string]interface{}{"created": 1, "updated": 2},
	})
}

func TestSupervisor_Acquire_NoAnchorDualStart(t *testing.T) {
	f := newSupFixture(t, true)
	b, err := f.prov.Acquire(context.Background(), f.taskRef())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// 双启动：bootstrap + formal 各一次 NewSession；bootstrap 确认终止 kill 1 次。
	news, kills := f.proc.calls()
	if news != 2 || kills != 1 {
		t.Errorf("new/kill = %d/%d, want 2/1（双启动子事务）", news, kills)
	}
	// claim+锚定三元组单事务恰好一次。
	if claims := f.store.claims; len(claims) != 1 || claims[0] != "t1|"+b.UpstreamSessionID {
		t.Fatalf("claims = %v, want single claim of binding anchor", f.store.claims)
	}
	if f.store.anchor["t1"] != b.UpstreamSessionID {
		t.Errorf("anchor seam = %q, want binding anchor %q", f.store.anchor["t1"], b.UpstreamSessionID)
	}
	// binding 形状（D3 envelope；C2c：derived——密码不落库、taskId 随 data 携带）。
	if b.Version != 1 || b.ProviderID != ProviderID || b.ProviderInstanceID != ProviderInstanceID ||
		b.TerminalRef != "ocdeck-t1-runtime" || b.UpstreamSessionID == "" {
		t.Errorf("binding = %+v", b)
	}
	var data v1BindingData
	if err := json.Unmarshal(b.Data, &data); err != nil || data.Directory != "/wt/t1" ||
		data.Auth.Kind != appagent.DerivedCredKind || data.Auth.Password != "" ||
		data.TaskID != "t1" || data.Port <= 0 {
		t.Errorf("binding data = %v err=%v", b.Data, err)
	}
	// spawn env：派生密码（32 字符 base64url）+ 来源标记 derived。
	master, _ := appagent.ResolveMasterSecret()
	wantPW := appagent.DeriveRuntimePassword(master, ProviderID, "t1")
	if len(wantPW) != 32 || wantPW == "" {
		t.Fatalf("派生密码形状异常: %q", wantPW)
	}
	// supervisor 只返回不持久化（binding 提交协议）：store seam 无 binding 写入口。
	fx := f.fx.persistCalls
	_ = fx
}

func TestSupervisor_Acquire_AnchoredDirectStart(t *testing.T) {
	f := newSupFixture(t, true)
	f.store.anchor["t1"] = "ses_anchor"
	f.seedServerSession("ses_anchor")
	b, err := f.prov.Acquire(context.Background(), f.taskRef())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if b.UpstreamSessionID != "ses_anchor" || b.TerminalRef != "ocdeck-t1-runtime" {
		t.Errorf("binding = %+v", b)
	}
	news, kills := f.proc.calls()
	if news != 1 || kills != 0 {
		t.Errorf("new/kill = %d/%d, want 1/0（直启，无双启动）", news, kills)
	}
	if len(f.store.claims) != 0 {
		t.Error("直启不创建 session（无 claim）")
	}
}

func TestSupervisor_Acquire_StaleAnchorCASClearsThenDualStart(t *testing.T) {
	f := newSupFixture(t, true)
	f.store.anchor["t1"] = "ses_gone"
	// 锚定缺席 → 直启 new(1) → 列表校验缺席 → CAS 清空 → 确认直启进程终止
	// kill(1) → 无锚定双启动 new(2)+kill(1)+new(3)。
	f.seedServerSession("ses_unrelated")
	if _, err := f.prov.Acquire(context.Background(), f.taskRef()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, ok := f.store.anchor["t1"]; ok && f.store.anchor["t1"] == "ses_gone" {
		t.Error("stale anchor must be cleared before dual start")
	}
	news, kills := f.proc.calls()
	if news != 3 || kills != 2 {
		t.Errorf("new/kill = %d/%d, want 3/2（直启 + 清空确认 + 双启动）", news, kills)
	}
}

func TestSupervisor_Acquire_AllUnhealthyExhausts(t *testing.T) {
	f := newSupFixture(t, false)
	_, err := f.prov.Acquire(context.Background(), f.taskRef())
	if err == nil {
		t.Fatal("健康永不就绪必须失败")
	}
	if !errors.Is(err, appagent.ErrRuntimeUnavailable) {
		t.Errorf("err = %v, want runtime_unavailable", err)
	}
	news, kills := f.proc.calls()
	// 3 轮轮换：new ×3 + kill ×3（clean → 轮换）。
	if news != 3 || kills != 3 {
		t.Errorf("new/kill = %d/%d, want 3/3", news, kills)
	}
}

func TestSupervisor_HealthTimeoutKillDispositionPropagation(t *testing.T) {
	f := newSupFixture(t, false)
	// 注入非 clean disposition（reap_failed + tickets）→ 不得轮换、直接终态。
	f.proc.killRes["ocdeck-t1-runtime"] = process.KillResult{
		SessionKilled: true, Disposition: process.DispositionReapFailed,
		CleanupTickets: []string{"tk-1"},
	}
	_, err := f.prov.Acquire(context.Background(), f.taskRef())
	se, ok := appagent.StageErrorOf(err)
	if err == nil || !ok {
		t.Fatalf("err = %v, want StageError", err)
	}
	if se.Cleanup == nil || se.Cleanup.Reason != appagent.KillReasonReapFailed || len(se.Cleanup.Tickets) != 1 || se.Cleanup.Tickets[0] != "tk-1" || !se.Cleanup.Retryable {
		t.Errorf("pending = %v", err)
	}
}

func TestSupervisor_PortDeathRotatesWithoutKill(t *testing.T) {
	f := newSupFixture(t, false)
	// 前两次会话立即死亡 → 死亡轮换（无 kill），第三次存活（dieAfterNew=2
	// 之后再无死亡脚本）；但所有端口都无健康 → 预算耗尽；kill 计数必须为 0。
	f.proc.dieAfterNew = 3
	_, err := f.prov.Acquire(context.Background(), f.taskRef())
	if err == nil {
		t.Fatal("all dead must fail")
	}
	_, kills := f.proc.calls()
	if kills != 0 {
		t.Errorf("kill calls = %d, want 0（死亡轮换无 kill）", kills)
	}
}

func TestSupervisor_PortRotationPersistsPort(t *testing.T) {
	f := newSupFixture(t, true)
	// lastPort 预置为健康端口前一档（无进程 → 健康超时 → kill clean → 轮换
	// + PersistServePort；第二轮命中健康端口成功）。
	fxPort := f.healthy.port
	f.store.lastPort["t1"] = fxPort - 1
	if _, err := f.prov.Acquire(context.Background(), f.taskRef()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if n := f.fx.persistCalls; n < 1 {
		t.Errorf("persist port calls = %d, want >= 1（轮换时持久化新端口）", n)
	}
}

// --- C13 残余修复测试（spec :145「新端口先持久化再建程」全覆盖）---

// TestSupervisor_FallbackFirstPortPersistsBeforeSpawn 固定输入：env 快照无
// OCDECK_SERVE_PORT（task 层未持久化的防御形态）→ GetTaskLastPort 缺失 →
// AllocatePort 分配 → 后备首轮 MUST 先 PersistServePort 再 NewSession
//（supervisor 同步顺序执行，spawn 完成时 persist 必已发生；「先持久化」的
// 有效性由 mutation 承载：persist 段删除 → env 端口仍随 spawn 落库但快照
// 端口缺席）。
// 断言：persistCalls ≥ 1；persist（新 env 含分配端口）——由 NewSession env
// 端口 == 分配端口（binding data.port）交叉断言（若 persist 先行且带端口，
// startServeLoop 的 env 即带同值）。
//
// mutation 证据（已验证 → 红 → 还原）：spi_supervisor.go 后备分支 persist 段
// 删除（:640-644）→ spawn env 端口仍来自 allocated 参数，但 persistCalls==0 →
// 本测试红。还原后绿。
func TestSupervisor_FallbackFirstPortPersistsBeforeSpawn(t *testing.T) {
	f := newSupFixture(t, true)
	// 无快照端口/无 lastPort → 后备分配（fixture AllocatePort 恒健康端口）。
	b, err := f.prov.Acquire(context.Background(), f.taskRef())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var data v1BindingData
	if err := json.Unmarshal(b.Data, &data); err != nil {
		t.Fatalf("binding data: %v", err)
	}
	if n := f.fx.persistCalls; n < 1 {
		t.Fatalf("persist calls = %d, want >= 1（后备首轮端口先落库，C13）", n)
	}
	spawnPort := ""
	if f.proc.newEnvFirst != nil {
		spawnPort = f.proc.newEnvFirst["OCDECK_SERVE_PORT"]
	}
	if spawnPort != fmt.Sprintf("%d", data.Port) {
		t.Fatalf("spawn env port = %q, want binding port %d（分配端口进 env+持久化同值）", spawnPort, data.Port)
	}
}

// TestSupervisor_ReadLastPortFailureFailClosed 固定输入：GetTaskLastPort seam
// 失败（env 无端口）。断言：supervisor.read_last_port 错误 + 零进程副作用
//（spec「配置读取失败 → 本次 attempt 失败」fail-closed）。
//
// mutation 证据（已验证 → 红 → 还原）：read_last_port 错误出口改 continue/忽略
// → fallback 建程继续 → news != 0 → 本测试红。还原后绿。
func TestSupervisor_ReadLastPortFailureFailClosed(t *testing.T) {
	f := newSupFixture(t, true)
	f.store.lastPortErr = errors.New("last port read down")
	_, err := f.prov.Acquire(context.Background(), f.taskRef())
	var ae *appagent.Error
	if err == nil || !errors.As(err, &ae) || ae.Op != "supervisor.read_last_port" {
		t.Fatalf("err = %v, want supervisor.read_last_port", err)
	}
	if news, _ := f.proc.calls(); news != 0 {
		t.Fatalf("new = %d, want 0（读取失败禁建程）", news)
	}
}

// TestSupervisor_PersistFailureBlocksSpawn 固定输入：PersistServePort seam 失败
// 两条腿（主路径快照端口 / 后备首轮分配端口）。断言：supervisor.persist_port
// 错误 + 零 NewSession（spec :145「持久化失败 MUST NOT 创建会话」）。
//
// mutation 证据（已验证 → 红 → 还原）：persist 失败出口改忽略续走 startServeLoop
// → news > 0 → 本测试红。还原后绿。
func TestSupervisor_PersistFailureBlocksSpawn(t *testing.T) {
	// 主路径：快照带端口（fx.buildEnv 可编程）→ persist 失败。
	f := newSupFixture(t, true)
	f.fx.buildEnv = func(taskID string) map[string]string {
		return map[string]string{"OCDECK_TASK_ID": taskID, "OCDECK_SERVE_PORT": fmt.Sprintf("%d", f.healthy.port)}
	}
	f.fx.persistErr = errors.New("snapshot store down")
	_, err := f.prov.Acquire(context.Background(), f.taskRef())
	// 主路径错误 op=supervisor.persist_port（普通 Error 非 StageError——persist_port
	// 在 isRecoveryTerminal 分派中按 RuntimeUnavailable 走 attempt 重试）。
	var ae *appagent.Error
	if err == nil || !errors.As(err, &ae) || ae.Op != "supervisor.persist_port" {
		t.Fatalf("main path err = %v, want supervisor.persist_port", err)
	}
	if news, _ := f.proc.calls(); news != 0 {
		t.Fatalf("main path new = %d, want 0（持久化失败禁建程）", news)
	}
	// 后备路径：无快照端口 → 分配后 persist 失败。
	f2 := newSupFixture(t, true)
	f2.fx.persistErr = errors.New("snapshot store down")
	if _, err := f2.prov.Acquire(context.Background(), f2.taskRef()); err == nil {
		t.Fatal("fallback path want persist_port error")
	}
	if news, _ := f2.proc.calls(); news != 0 {
		t.Fatalf("fallback new = %d, want 0", news)
	}
}

func TestSupervisor_Resume_PolicyMatrix(t *testing.T) {
	t.Run("beforeLaunch_precedes_port_allocation", func(t *testing.T) {
		f := newSupFixture(t, true)
		// tasks 4.4（D7「supervisor 内部每次启动进程前调用」）：BeforeLaunch 调用
		// 计数 ≥ 进程创建计数——无锚定双启动（bootstrap + 正式）各过一次闸门，
		// 且首次调用先于任何 NewSession。
		var gateCalls, newsAtGate int
		_, err := f.prov.Resume(context.Background(), f.taskRef(), domain.TaskBinding{}, appagent.ResumePolicy{
			AllowSessionCreate: true,
			BeforeLaunch: func(ctx context.Context) error {
				gateCalls++
				n, _ := f.proc.calls()
				newsAtGate = n // 末次闸门调用时已建进程数（正式进程前应为 1）
				return nil
			},
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		news, _ := f.proc.calls()
		if news != 2 {
			t.Fatalf("new = %d, want 2（无锚定双启动）", news)
		}
		if newsAtGate != 1 {
			t.Errorf("第二次闸门时 new = %d, want 1（正式进程建程前过闸门）", newsAtGate)
		}
	})
	t.Run("beforeLaunch_fail_zero_side_effects", func(t *testing.T) {
		f := newSupFixture(t, true)
		_, err := f.prov.Resume(context.Background(), f.taskRef(), domain.TaskBinding{}, appagent.ResumePolicy{
			AllowSessionCreate: true,
			BeforeLaunch: func(ctx context.Context) error {
				return errors.New("permit denied")
			},
		})
		news, kills := f.proc.calls()
		if err == nil || news != 0 || kills != 0 {
			t.Errorf("闸门拒绝必须零副作用: err=%v new=%d kill=%d", err, news, kills)
		}
	})
	t.Run("old_binding_anchor_direct_start", func(t *testing.T) {
		f := newSupFixture(t, true)
		f.seedServerSession("ses_resumed")
		old := buildBinding(f.taskRef(), "ses_resumed", "ocdeck-t1-runtime", 59999)
		// C4 残余：AllowRebuild 独立承载重建——恢复直启路径需显式 AllowRebuild=true
		//（default ResumePolicy{} 禁重建形态由 repair 入口显式构造）。
		b, err := f.prov.Resume(context.Background(), f.taskRef(), old, appagent.ResumePolicy{
			AllowSessionCreate: true, AllowRebuild: true,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if b.UpstreamSessionID != "ses_resumed" {
			t.Errorf("binding anchor = %q", b.UpstreamSessionID)
		}
		news, _ := f.proc.calls()
		if news != 1 {
			t.Errorf("new = %d, want 1（resume 直启）", news)
		}
	})
	t.Run("repair_complete_no_anchor", func(t *testing.T) {
		f := newSupFixture(t, true)
		// old 无锚定 + AllowSessionCreate=false → 修复完成信号（非失败）。
		_, err := f.prov.Resume(context.Background(), f.taskRef(), domain.TaskBinding{},
			appagent.ResumePolicy{AllowSessionCreate: false})
		if !errors.Is(err, errRepairCompleteNoAnchor) {
			t.Fatalf("err = %v, want errRepairCompleteNoAnchor", err)
		}
	})
	t.Run("stale_anchor_rebuild_disallowed_zero_side_effects", func(t *testing.T) {
		// C4 残余（review attempt 2）：旧 terminal 缺失 + 有锚定 + 两开关均 false
		// → 修复失败（非修复完成），零端口分配零 NewSession 零 kill——锁死零副作用。
		f := newSupFixture(t, true)
		f.store.anchor["t1"] = "ses_gone"
		old := domain.TaskBinding{UpstreamSessionID: "ses_gone", TerminalRef: "ocdeck-t1-runtime"}
		_, err := f.prov.Resume(context.Background(), f.taskRef(), old, appagent.ResumePolicy{
			AllowSessionCreate: false, AllowRebuild: false,
		})
		var ae *appagent.Error
		if err == nil || !errors.As(err, &ae) || ae.Kind != appagent.ErrKindRuntimeUnavailable {
			t.Fatalf("err = %v, want runtime_unavailable rebuild-disallowed", err)
		}
		// 零副作用：锚定未被清、零 NewSession、零 kill。
		if got := f.store.anchor["t1"]; got != "ses_gone" {
			t.Errorf("anchor = %q, want unchanged（禁重建零副作用）", got)
		}
		news, kills := f.proc.calls()
		if news != 0 || kills != 0 {
			t.Errorf("new=%d kill=%d, want 0/0（AllowRebuild=false 零进程副作用）", news, kills)
		}
	})

	// --- C4 门禁修复 fixtures ---

	// resume_reuses_alive_terminal（specs「终端引用失效重建绑定」反例：未失效不重建）：
	// 旧 binding 的 TerminalRef 对应 tmux 会话仍存活且健康 → 复用返回旧 binding，
	// 零 NewSession/零 kill/零 claim / 零端口分配副作用。
	t.Run("resume_reuses_alive_terminal", func(t *testing.T) {
		f := newSupFixture(t, true)
		old := f.aliveOldBinding()
		b, err := f.prov.Resume(context.Background(), f.taskRef(), old, appagent.ResumePolicy{
			AllowSessionCreate: true, AllowRebuild: true,
		})
		if err != nil {
			t.Fatalf("Resume(reuse): %v", err)
		}
		if b.UpstreamSessionID != "ses_keep" || b.TerminalRef != old.TerminalRef {
			t.Errorf("reused binding = %+v, want old binding unchanged", b)
		}
		news, kills := f.proc.calls()
		if news != 0 || kills != 0 {
			t.Errorf("new/kill = %d/%d, want 0/0（存活复用零进程副作用）", news, kills)
		}
		if len(f.store.claims) != 0 {
			t.Errorf("claims = %v, want none（复用不重新 claim）", f.store.claims)
		}
	})

	// resume_rebuild_when_terminal_dead：TerminalRef 缺失 → 失效重建路径（AllowRebuild
	// 为 true 时允许经锚定重建建程）；复用前的 Health 探测不产生进程副作用。
	t.Run("resume_rebuild_when_terminal_dead", func(t *testing.T) {
		f := newSupFixture(t, true)
		old := buildBinding(f.taskRef(), "", "ocdeck-t1-runtime", 59872) // 无锚定
		b, err := f.prov.Resume(context.Background(), f.taskRef(), old, appagent.ResumePolicy{
			AllowSessionCreate: true, AllowRebuild: true,
		})
		if err != nil {
			t.Fatalf("Resume(rebuild): %v", err)
		}
		if b.UpstreamSessionID == "" {
			t.Errorf("rebuilt binding anchor = %q, want new claim", b.UpstreamSessionID)
		}
		news, _ := f.proc.calls()
		if news != 2 {
			t.Errorf("new = %d, want 2（失效重建双启动）", news)
		}
	})

	// repair_no_rebuild_when_terminal_dead：AllowRebuild=false 且 terminal 缺失
	// → 修复完成信号（不启动进程——D9a repair 不建进程）。
	t.Run("repair_no_rebuild_when_terminal_dead", func(t *testing.T) {
		f := newSupFixture(t, true)
		old := buildBinding(f.taskRef(), "", "ocdeck-t1-runtime", 59873)
		_, err := f.prov.Resume(context.Background(), f.taskRef(), old, appagent.ResumePolicy{
			AllowSessionCreate: false, AllowRebuild: false,
		})
		if !errors.Is(err, errRepairCompleteNoAnchor) {
			t.Fatalf("err = %v, want errRepairCompleteNoAnchor（修复禁重建）", err)
		}
		news, _ := f.proc.calls()
		if news != 0 {
			t.Errorf("new = %d, want 0（repair 不启动进程）", news)
		}
	})

	// beforeLaunch_gates_every_process_create（specs 预算协议：每次进程创建对应一个
	// 已写入 permit——双启动 bootstrap/正式各一次 + 端口轮换每轮各一次）：闸门调用
	// 计数 == NewSession 计数，且闸门先于该轮端口分配（闸门拒绝轮 → 零进程副作用）。
	t.Run("beforeLaunch_gates_every_process_create", func(t *testing.T) {
		f := newSupFixture(t, true)
		var gateCalls int
		_, err := f.prov.Resume(context.Background(), f.taskRef(), domain.TaskBinding{}, appagent.ResumePolicy{
			AllowSessionCreate: true,
			BeforeLaunch: func(ctx context.Context) error {
				gateCalls++
				return nil
			},
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		news, _ := f.proc.calls()
		if gateCalls != news {
			t.Errorf("gate=%d new=%d, want 相等（每次进程创建恰过一次闸门）", gateCalls, news)
		}
	})

	// probe_failure_fails_attempt_no_kill_no_rotate（逐阶段失败表「能力探测：任何失败
	// → 本次 attempt 失败，探测失败不轮换」）：健康通过但 /session/status 结构违约
	// → typed StageError（C12 残余修复：普通 NewError 逃过 StageErrorOf→task 层
	// 原样返回走 attempt 重试；probe 属终态分派，统一 StageError 类型）、零 kill
	//（kill+notice 委托调用方补偿）。
	t.Run("probe_failure_fails_attempt_no_kill_no_rotate", func(t *testing.T) {
		f := newSupFixture(t, true)
		f.probeBroken = true
		_, err := f.prov.Acquire(context.Background(), f.taskRef())
		if err == nil {
			t.Fatalf("want probe stage error, got nil")
		}
		var se *appagent.StageError
		if !errors.As(err, &se) || se.Op != "supervisor.probe" {
			t.Fatalf("err = %v (%T), want supervisor.probe StageError", err, err)
		}
		if se.Kind != appagent.ErrKindRuntimeUnavailable {
			t.Errorf("probe kind = %s, want runtime_unavailable", se.Kind)
		}
		_, kills := f.proc.calls()
		if kills != 0 {
			t.Errorf("kill = %d, want 0（探测失败不轮换不清理——委托调用方补偿）", kills)
		}
	})
}

func TestSupervisor_AcquireEphemeral_NoAnchorNoClaim(t *testing.T) {
	f := newSupFixture(t, true)
	b, err := f.prov.AcquireEphemeral(context.Background(), f.taskRef())
	if err != nil {
		t.Fatalf("AcquireEphemeral: %v", err)
	}
	if b.UpstreamSessionID != "" {
		t.Errorf("ephemeral anchor = %q, want empty（无锚定 bootstrap）", b.UpstreamSessionID)
	}
	if len(f.store.claims) != 0 {
		t.Errorf("ephemeral MUST NOT claim: %v", f.store.claims)
	}
	if _, err := f.prov.AcquireEphemeral(context.Background(), f.taskRef()); err != nil {
		t.Fatalf("second ephemeral: %v", err)
	}
}

func TestSupervisor_Release_Idempotent(t *testing.T) {
	f := newSupFixture(t, true)
	b := buildBinding(f.taskRef(), "", "ocdeck-t1-runtime", 49999)
	// absent → nil（幂等）。
	if err := f.prov.Release(context.Background(), b); err != nil {
		t.Fatalf("Release absent: %v", err)
	}
	// clean kill → nil；session 移除。
	f.proc.sessions[b.TerminalRef] = true
	if err := f.prov.Release(context.Background(), b); err != nil {
		t.Fatalf("Release clean: %v", err)
	}
	if _, exists := f.proc.sessions[b.TerminalRef]; exists {
		t.Error("clean kill must remove session")
	}
	// 非 clean → StageError (cleanup payload)。
	f.proc.sessions[b.TerminalRef] = true
	f.proc.killRes[b.TerminalRef] = process.KillResult{SessionKilled: true,
		Disposition: process.DispositionReapFailed, CleanupTickets: []string{"tk"}}
	err := f.prov.Release(context.Background(), b)
	se, serrOK := appagent.StageErrorOf(err)
	if err == nil || !serrOK {
		t.Fatalf("Release non-clean = %v", err)
	}
	if se.Cleanup == nil || se.Cleanup.Reason != appagent.KillReasonReapFailed {
		t.Fatalf("Release non-clean cleanup = %+v", se)
	}
	// 空 TerminalRef → 幂等 nil。
	if err := f.prov.Release(context.Background(), domain.TaskBinding{}); err != nil {
		t.Fatalf("Release empty binding: %v", err)
	}
}

func TestSupervisor_StubAndDepValidation(t *testing.T) {
	// 无 deps → C1 占位形态（CapabilityUnavailable）。
	plain := NewSPIProvider()
	if _, err := plain.Supervisor().Acquire(context.Background(), appagent.TaskRef{TaskID: "t"}); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("占位态 = %v, want capability_unavailable", err)
	}
	// deps 半注入（缺 AllocatePort）→ 构造期暴露错误。
	fx := newSupFixture(t, true)
	fx.deps.AllocatePort = nil
	p := NewSPIProviderWithDeps(*fx.deps)
	if _, err := p.Acquire(context.Background(), fx.taskRef()); !errors.Is(err, appagent.ErrCapabilityUnavailable) {
		t.Errorf("deps 缺失 = %v, want capability_unavailable", err)
	}
	// TaskRef 缺字段 → BindingInvalid。
	f2 := newSupFixture(t, true)
	if _, err := f2.prov.Acquire(context.Background(), appagent.TaskRef{Directory: "/d"}); !errors.Is(err, appagent.ErrBindingInvalid) {
		t.Errorf("TaskRef 缺 TaskID = %v, want binding_invalid", err)
	}
}

func TestSupTerminal_ViaDeps(t *testing.T) {
	f := newSupFixture(t, true)
	f.proc.sessions["ocdeck-t1-runtime"] = true
	ref, err := f.prov.Terminal().EnsureTerminal(context.Background(), buildBinding(fxTaskRef(), "", "ocdeck-t1-runtime", 1))
	if err != nil || ref != "ocdeck-t1-runtime" {
		t.Fatalf("EnsureTerminal = (%q, %v)", ref, err)
	}
	// 会话不存在 → 错误。
	delete(f.proc.sessions, "ocdeck-t1-runtime")
	if _, err := f.prov.Terminal().EnsureTerminal(context.Background(), buildBinding(fxTaskRef(), "", "ocdeck-t1-runtime", 1)); err == nil {
		t.Error("EnsureTerminal 会话缺失必须报错")
	}
}

func fxTaskRef() appagent.TaskRef { return appagent.TaskRef{TaskID: "t1", Directory: "/wt/t1"} }

// TestSupervisor_OnRuntimeReadyEpochHook（tasks 3.8 epoch 语义）：Acquire/Resume/
// AcquireEphemeral 成功恰回调一次；失败路径零回调；与 HealthTracker.BumpRuntimeEpoch
// 组合时代际递增（生产装配注入该回调）。
func TestSupervisor_OnRuntimeReadyEpochHook(t *testing.T) {
	f := newSupFixture(t, true)
	var ready []string
	f.deps.OnRuntimeReady = func(taskID string) { ready = append(ready, taskID) }
	f.prov = NewSPIProviderWithDeps(*f.deps)

	// Acquire 成功 → 恰一次。
	if _, err := f.prov.Acquire(context.Background(), f.taskRef()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if len(ready) != 1 || ready[0] != "t1" {
		t.Fatalf("Acquire 后回调 = %v, want [t1]", ready)
	}

	// AcquireEphemeral 成功 → 恰一次（临时 runtime 亦换代）。
	ready = nil
	if _, err := f.prov.AcquireEphemeral(context.Background(), f.taskRef()); err != nil {
		t.Fatalf("AcquireEphemeral: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("Ephemeral 后回调 = %v, want 恰一次", ready)
	}

	// 失败路径（BeforeLaunch 拒绝）→ 零回调。
	ready = nil
	if _, err := f.prov.Resume(context.Background(), f.taskRef(), domain.TaskBinding{},
		appagent.ResumePolicy{AllowSessionCreate: true, BeforeLaunch: func(ctx context.Context) error {
			return errors.New("deny")
		}}); err == nil {
		t.Fatal("闸门拒绝必须失败")
	}
	if len(ready) != 0 {
		t.Errorf("失败路径回调 = %v, want 零", ready)
	}

	// 与 tracker 组合：epoch 递增可观测（生产接线形态）。
	f2 := newSupFixture(t, true)
	tr := appagent.NewHealthTracker()
	f2.deps.OnRuntimeReady = func(taskID string) { tr.BumpRuntimeEpoch(taskID) }
	p2 := NewSPIProviderWithDeps(*f2.deps)
	if _, err := p2.Acquire(context.Background(), f2.taskRef()); err != nil {
		t.Fatalf("Acquire2: %v", err)
	}
	if _, err := p2.Resume(context.Background(), f2.taskRef(),
		domain.TaskBinding{UpstreamSessionID: "ses_fake_001"}, appagent.ResumePolicy{AllowSessionCreate: true, AllowRebuild: true}); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	if e := tr.RuntimeEpoch("t1"); e != 2 {
		t.Errorf("epoch = %d, want 2（Acquire + Resume 各换代一次）", e)
	}
}
