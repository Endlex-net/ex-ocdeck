// v2_stage4_test.go 落实阶段 4 验收（opencode-v2-provider tasks 7.1 / design D9
// 验收矩阵）在 opencode2 侧的三项补测：
//   - 首建补偿场景①（POST 成功后配置失败）：candidate session 受限回收
//     （SessionControl.Delete）；DELETE 失败记清理债务且不误判「从未建立」（D3）
//   - ephemeral 完整链：AcquireEphemeral → EphemeralRuntimeEndpoint → Release
//     （零 session 副作用触达，D3/D9 完整链行）
//   - 任务隔离：同 server 两 session——A 挂起（Release）仅 interrupt A，
//     B 零触达、全程无 DELETE（D3 挂起停止只作用于目标任务 session）
package opencode2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// TestStage4_FirstBuildCompensation_ConfigFailAfterPOST 落实首建补偿场景①：
// POST 建 session 成功后配置应用失败 → attempt 失败（不返回 binding）+ candidate
// 经 DELETE 受限回收（仅本次新建 session id）；DELETE 失败 → 清理债务落实例记录
// last_error（含残留 session id——非「从未建立」表述），原错误照常返回。
func TestStage4_FirstBuildCompensation_ConfigFailAfterPOST(t *testing.T) {
	run := func(t *testing.T, deleteStatus int) (debt string) {
		var deletedID string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/environment"):
				w.WriteHeader(http.StatusInternalServerError) // 配置应用失败（场景①触发点）
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write(readFixture(t, "session_create.json"))
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_"):
				deletedID = strings.TrimPrefix(r.URL.Path, "/api/session/")
				w.WriteHeader(deleteStatus)
				if deleteStatus != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"_tag":"InternalError"}`))
				}
			default:
				_, _ = w.Write(readFixture(t, "session_list_page2.json"))
			}
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		if _, _, err := sup.ensureServer(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
		sup.deps.Env = fakeEnvSeam(func(string, bool) (map[string]string, error) {
			return nil, errors.New("env snapshot unavailable")
		})
		b, err := sup.Acquire(context.Background(), appTaskRef("t1", "/wt"))
		if err == nil {
			t.Fatalf("config failure must fail the attempt (no binding), got %+v", b)
		}
		if b.UpstreamSessionID != "" {
			t.Fatalf("binding must not be returned on failed attempt: %+v", b)
		}
		if deletedID != "ses_fixture01a" {
			t.Fatalf("candidate must be recycled via DELETE, deleted=%q", deletedID)
		}
		recs.mu.Lock()
		if recs.rec.LastError.Valid {
			debt = recs.rec.LastError.String
		}
		recs.mu.Unlock()
		return debt
	}

	t.Run("delete ok", func(t *testing.T) {
		if debt := run(t, http.StatusNoContent); debt != "" {
			t.Fatalf("clean recycle must not record debt: %q", debt)
		}
	})
	t.Run("delete fail records cleanup debt", func(t *testing.T) {
		debt := run(t, http.StatusInternalServerError)
		if debt == "" {
			t.Fatalf("DELETE failure must record cleanup debt")
		}
		if !strings.Contains(debt, "ses_fixture01a") || !strings.Contains(debt, "残留") {
			t.Fatalf("debt must name leftover session (非「从未建立」): %q", debt)
		}
	})
}

// TestStage4_RecycleCandidateSession_Supervisor 落实任务层首建失败补偿的
// supervisor 回收入口（D3 受限回收例外；CandidateSessionRecycler 窄接口）：v2
// binding upstream → DELETE 回收；DELETE 失败记清理债务（含残留 session id，
// 非「从未建立」）并向调用方返回错误；无 upstream 幂等成功零触达。
func TestStage4_RecycleCandidateSession_Supervisor(t *testing.T) {
	run := func(t *testing.T, deleteStatus int) (deletedID, debt string, rerr error) {
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_") {
				deletedID = strings.TrimPrefix(r.URL.Path, "/api/session/")
				w.WriteHeader(deleteStatus)
				if deleteStatus != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"_tag":"InternalError"}`))
				}
				return
			}
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		b := stage4LifecycleBinding(t, "t-rc", "ses_fixture01a")
		rerr = sup.RecycleCandidateSession(context.Background(), b)
		recs.mu.Lock()
		if recs.rec.LastError.Valid {
			debt = recs.rec.LastError.String
		}
		recs.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		return deletedID, debt, rerr
	}

	t.Run("delete ok", func(t *testing.T) {
		id, debt, err := run(t, http.StatusNoContent)
		if err != nil {
			t.Fatalf("clean recycle must succeed: %v", err)
		}
		if debt != "" {
			t.Fatalf("clean recycle must not record debt: %q", debt)
		}
		if id != "ses_fixture01a" {
			t.Fatalf("DELETE must target candidate upstream, deleted=%q", id)
		}
	})
	t.Run("delete fail debt and error", func(t *testing.T) {
		_, debt, err := run(t, http.StatusInternalServerError)
		if err == nil {
			t.Fatalf("DELETE failure must surface to caller for compensation error chain")
		}
		if !strings.Contains(debt, "ses_fixture01a") || !strings.Contains(debt, "残留") {
			t.Fatalf("debt must name leftover session (非「从未建立」): %q", debt)
		}
	})
	t.Run("empty upstream idempotent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("no session side effects expected, got %s %s", r.Method, r.URL.Path)
		}))
		t.Cleanup(srv.Close)
		sup, _, recs := newTestSupervisor(t, srv.URL, nil)
		b := stage4LifecycleBinding(t, "t-rc", "")
		if err := sup.RecycleCandidateSession(context.Background(), b); err != nil {
			t.Fatalf("empty upstream must be idempotent success: %v", err)
		}
		if recs.rec.LastError.Valid {
			t.Fatalf("no debt expected: %q", recs.rec.LastError.String)
		}
	})
}

// TestStage4_ServerMonitor_StopsOnShutdown 落实 I9 关停语义：监控循环在
// ShutdownServer 后停止——此后进程死亡不再自动重拉（ready 保持清除、零通知）。
func TestStage4_ServerMonitor_StopsOnShutdown(t *testing.T) {
	var restarts atomic.Int32
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	sup.deps.OnServerRestarted = func() { restarts.Add(1) }
	sup.deps.MonitorInterval = 5 * time.Millisecond
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := sup.ShutdownServer(context.Background()); err != nil {
		t.Fatalf("ShutdownServer: %v", err)
	}
	makeProcDead(sup, proc, recs)
	time.Sleep(30 * time.Millisecond) // 数个监控周期：关停后监控不得自愈
	if supervisorReadyLocked(sup) {
		t.Fatalf("monitor must be stopped after shutdown (ready must stay cleared)")
	}
	if n := restarts.Load(); n != 0 {
		t.Fatalf("no restart notifications after shutdown, got %d", n)
	}
}

// TestStage4_Status_ConnectionGenerationInvalidation 落实 I12：断流即旧代
// execution 状态作废（Current 缺项——不得重发过期 busy）；重连新代由快照自补
// （SessionUpdated 不映射状态——不得 outcome→idle 猜测）；新代冻结映射事件
// （succeeded）到达才重建 idle。
func TestStage4_Status_ConnectionGenerationInvalidation(t *testing.T) {
	setMasterSecret(t)
	var conns atomic.Int32
	gate1 := make(chan struct{}) // conn#1 持流（已发 started）；关闭即断流
	gate2 := make(chan struct{}) // conn#2 持流（重连自补后）；关闭触发 conn#3
	var snapGets atomic.Int32
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/event":
			n := conns.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// 每连接首帧必须 server.connected（握手严格契约），随后业务帧。
			hello := "data: {\"id\":\"h\",\"created\":1,\"type\":\"server.connected\",\"data\":{\"clientID\":\"cli_x\"}}\n\n"
			switch n {
			case 1:
				writeSSE(t, w, hello+"data: {\"id\":\"e1\",\"created\":1,\"type\":\"session.execution.started\",\"data\":{\"sessionID\":\"ses_fixture01a\"}}\n\n")
				<-gate1 // 持流至测试释放（模拟保持连接）
			case 2:
				writeSSE(t, w, hello+"data: {\"id\":\"e2\",\"created\":2,\"type\":\"session.updated\",\"data\":{\"sessionID\":\"ses_fixture01a\",\"title\":\"healed\"}}\n\n")
				<-gate2
			default:
				writeSSE(t, w, hello+"data: {\"id\":\"e3\",\"created\":3,\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_fixture01a\"}}\n\n")
				<-r.Context().Done()
			}
		case "/api/session/ses_fixture01a":
			snapGets.Add(1) // selfHeal 快照（含 outcome——MUST NOT 映射状态）
			_, _ = w.Write([]byte(`{"data":{"id":"ses_fixture01a","location":{"directory":"/wt"},"time":{"created":1,"updated":2},"outcome":"idle"}}`))
		default:
			w.WriteHeader(404)
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{StatusVerified: true})
	b := testBinding(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, b)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es.SetCallbacks(func() {}, func() {})
	key := v2Namespace(b.UpstreamSessionID)
	statusOf := func() (domain.AgentStatus, bool) {
		snap, cerr := p.Status().Current(ctx, b)
		if cerr != nil {
			t.Fatalf("Current: %v", cerr)
		}
		st, ok := snap[key]
		return st, ok
	}
	// 旧代：started 冻结映射 → busy。
	expectEvent(t, es, domain.EventStatusChanged)
	if st, ok := statusOf(); !ok || st != domain.StatusBusy {
		t.Fatalf("pre-disconnect status=%v ok=%v, want busy", st, ok)
	}
	// 断流：旧代状态作废（Current 缺项——不重发过期 busy）。
	close(gate1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, ok := statusOf()
		if !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale busy must be invalidated on disconnect")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// 重连新代：selfHeal 快照（SessionUpdated，含 outcome）不重建状态——缺项
	// 保持，不得 outcome→idle 猜测。
	deadline = time.Now().Add(2 * time.Second)
	for snapGets.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if st, ok := statusOf(); ok {
		t.Fatalf("self-heal snapshot must not guess status, got %v", st)
	}
	// 新代冻结证据：succeeded 事件到达 → idle 重建（跳过自补 SessionUpdated 等
	// 非状态事件——只认冻结映射的 StatusChanged）。
	close(gate2)
	statusSettled := false
	deadline = time.Now().Add(5 * time.Second)
	for !statusSettled {
		select {
		case ev, ok := <-es.Events():
			if !ok {
				t.Fatalf("stream closed while waiting for new-generation status event")
			}
			if ev.Kind == domain.EventStatusChanged {
				statusSettled = true
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("new-generation status event never arrived")
		}
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		st, ok := statusOf()
		if ok && st == domain.StatusIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new-generation succeeded must rebuild idle, got %v ok=%v", st, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	_ = es.Close()
}

// TestStage4_Status_ChildSession_Invalidation 落实 I12 失效范围（主+子）：同一流
// 携带的子 session 状态随断线一并按流覆盖范围失效——不残留旧代 busy。
func TestStage4_Status_ChildSession_Invalidation(t *testing.T) {
	setMasterSecret(t)
	child := "ses_child01"
	var conns atomic.Int32
	gate := make(chan struct{})
	srv := httptestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/event":
			n := conns.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if n == 1 {
				writeSSE(t, w, "data: {\"id\":\"h1\",\"created\":1,\"type\":\"server.connected\",\"data\":{\"clientID\":\"c\"}}\n\n"+
					"data: {\"id\":\"e1\",\"created\":1,\"type\":\"session.execution.started\",\"data\":{\"sessionID\":\""+child+"\"}}\n\n")
				<-gate // 持流至测试释放；返回即断流（EOF）
				return
			}
			<-r.Context().Done()
		case "/api/session/" + child:
			_, _ = w.Write([]byte(`{"data":{"id":"` + child + `","location":{"directory":"/wt"},"time":{"created":1,"updated":1}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[],"cursor":{"previous":null,"next":null}}`))
		}
	})
	p := newTestProvider(t, srv.URL, ProviderOptions{StatusVerified: true})
	b := testBinding(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	es, err := p.Events().Subscribe(ctx, b)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es.SetCallbacks(func() {}, func() {})
	expectEvent(t, es, domain.EventStatusChanged) // 子 session started → busy
	if st, ok := p.status[child]; !ok || st != domain.StatusBusy {
		t.Fatalf("child busy expected, got %v ok=%v", st, ok)
	}
	close(gate) // 断流
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.statusMu.Lock()
		_, alive := p.status[child]
		p.statusMu.Unlock()
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child stale busy must be invalidated on disconnect")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	_ = es.Close()
}

// TestStage4_Status_MultiSub_GenerationGuard 落实 I12 并行订阅互不污染：旧流的
// 断线清扫按代守卫——只清除「本流写入且其后无更新写入」的事实，新流刚写入的
// 状态不被旧流清除。
func TestStage4_Status_MultiSub_GenerationGuard(t *testing.T) {
	p := NewSPIProvider(ProviderOptions{StatusVerified: true})
	key := domain.SessionKey{ProviderID: ProviderID, ProviderInstanceID: ProviderInstanceID, UpstreamID: "ses_x"}
	// 旧流写入 busy（gen1——recordStatus 同临界区返回本次写入代，Round3 I12
	// 生产记账路径）。
	gen1 := p.recordStatus(key, domain.StatusBusy)
	// 新流随后重写（gen2——事实已被新流接管）。
	_ = p.recordStatus(key, domain.StatusBusy)
	// 旧流断线清扫（按 gen1）：MUST NOT 清除新流的 gen2 事实。
	p.invalidateStatusGen("ses_x", gen1)
	if st, ok := p.status["ses_x"]; !ok || st != domain.StatusBusy {
		t.Fatalf("newer stream fact must survive old-stream sweep, got %v ok=%v", st, ok)
	}
	// 新流自身断线（按 gen2）：事实清除（缺项=未知）。
	gen2 := p.recordStatus(key, domain.StatusIdle)
	p.invalidateStatusGen("ses_x", gen2)
	p.statusMu.Lock()
	_, alive := p.status["ses_x"]
	p.statusMu.Unlock()
	if alive {
		t.Fatalf("own-generation sweep must invalidate")
	}
}

// TestStage4_TerminalIO_EnsureAndAttach 落实 TerminalIO 真实 wire 行为（D3
// 契约；评审 I3 的 provider 侧证据）：EnsureTerminal REST 预验证（GET
// /api/session/{id}——存在且 location.directory==binding directory，归属不符 →
// BindingInvalid、缺失 → NotFound）；AttachCommand argv 自动选中（--server +
// --session，D8-7）。
func TestStage4_TerminalIO_EnsureAndAttach(t *testing.T) {
	var getSessionHits int32
	var lastGetPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/session/ses_") {
			id := strings.TrimPrefix(r.URL.Path, "/api/session/")
			if id == "ses_gone" {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"_tag":"NotFound"}`))
				return
			}
			atomic.AddInt32(&getSessionHits, 1)
			lastGetPath = r.URL.Path
			dir := "/wt"
			if id == "ses_foreign" {
				dir = "/other"
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + id + `","time":{"created":1,"updated":1},"location":{"directory":"` + dir + `"}}}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"_tag":"NotFound"}`))
	}))
	t.Cleanup(srv.Close)

	p := NewSPIProviderWithDeps(ProviderOptions{StatusVerified: true}, &SupervisorDeps{
		Proc:         &fakeProcSeam{pids: map[string]int{}},
		Store:        &fakeRecordStore{errs: map[string]error{}},
		AllocatePort: func(lastPort int, lastValid bool, exclude int) (int, error) { return 4096, nil },
	})
	p.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srv.URL
		return c
	}
	term := p.Terminal()
	b := stage4LifecycleBinding(t, "t-term", "ses_keep")

	// 存在且归属本任务 → 返回托管单元引用（TerminalRef 稳定标识）。
	ref, err := term.EnsureTerminal(context.Background(), b)
	if err != nil || ref != b.TerminalRef {
		t.Fatalf("EnsureTerminal: ref=%q err=%v", ref, err)
	}
	if atomic.LoadInt32(&getSessionHits) != 1 || lastGetPath != "/api/session/ses_keep" {
		t.Fatalf("REST prevalidation expected on binding upstream, hits=%d path=%q", getSessionHits, lastGetPath)
	}

	// 归属不符 → BindingInvalid（MUST NOT 建替代 session）。
	foreign := stage4LifecycleBinding(t, "t-term", "ses_foreign")
	if _, err := term.EnsureTerminal(context.Background(), foreign); err == nil || !errorKindIs(err, appagent.ErrKindBindingInvalid) {
		t.Fatalf("directory mismatch must be BindingInvalid, got %v", err)
	}

	// 缺失 → NotFound。
	missing := stage4LifecycleBinding(t, "t-term", "ses_gone")
	if _, err := term.EnsureTerminal(context.Background(), missing); err == nil || !errorKindIs(err, appagent.ErrKindNotFound) {
		t.Fatalf("missing session must be NotFound, got %v", err)
	}

	// AttachCommand：argv 自动选中（--server + --session；凭据走 env 不入 argv）。
	argv, aerr := term.AttachCommand(context.Background(), b)
	if aerr != nil {
		t.Fatalf("AttachCommand: %v", aerr)
	}
	want := []string{"opencode", "--server", "http://127.0.0.1:4096", "--session", "ses_keep"}
	if len(argv) != len(want) {
		t.Fatalf("argv: %v want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d]: %q want %q", i, argv[i], want[i])
		}
	}
}

// errorKindIs 判定 typed provider 错误的 ErrorKind（errors.As 解包）。
func errorKindIs(err error, kind appagent.ErrorKind) bool {
	var ae *appagent.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Kind == kind
}

// TestStage4_Ephemeral_FullChain 落实 ephemeral 完整链（D9）：AcquireEphemeral →
// EphemeralRuntimeEndpoint（端点/派生凭据/目录解析）→ Release 仅回收连接——
// 全链零 session 副作用（除就绪判定外无任何 session API 触达）。
func TestStage4_Ephemeral_FullChain(t *testing.T) {
	var sessionSideEffects int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/api/session") {
			mu.Lock()
			sessionSideEffects++ // 非就绪判定的 session 侧副作用都算违规
			mu.Unlock()
		}
		_, _ = w.Write(readFixture(t, "session_list_page2.json"))
	}))
	t.Cleanup(srv.Close)
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	b, err := sup.AcquireEphemeral(context.Background(), appTaskRef("t-eph", "/wt"))
	if err != nil {
		t.Fatalf("AcquireEphemeral: %v", err)
	}
	// 端点解析（provider 侧下沉；删除清理消费）。
	port, password, dir, err := sup.EphemeralRuntimeEndpoint(b)
	if err != nil {
		t.Fatalf("EphemeralRuntimeEndpoint: %v", err)
	}
	if port != 4096 || password == "" || dir != "/wt" {
		t.Fatalf("endpoint: port=%d dir=%q password-empty=%v", port, dir, password == "")
	}
	if err := sup.Release(context.Background(), b); err != nil {
		t.Fatalf("Release(ephemeral): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sessionSideEffects != 0 {
		t.Fatalf("ephemeral chain must not touch session side effects, hits=%d", sessionSideEffects)
	}
}

// TestStage4_Isolation_ReleaseA_LeavesB_Untouched 落实任务隔离（D9）：同 server
// 两任务 session（A/B）——A 挂起 Release 仅 interrupt A 恰一次；B 的 session API
// 零触达，全程无任何 DELETE（Release 保留上游 session）。
func TestStage4_Isolation_ReleaseA_LeavesB_Untouched(t *testing.T) {
	var interruptA, interruptsB, deletes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_a/interrupt"):
			interruptA++
			_, _ = w.Write([]byte(`{"interrupted":true}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_b"):
			interruptsB++
			w.WriteHeader(500)
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(204)
		default:
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		}
	}))
	t.Cleanup(srv.Close)
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	bindingA := stage4LifecycleBinding(t, "t-a", "ses_a")
	if err := sup.Release(context.Background(), bindingA); err != nil {
		t.Fatalf("Release(A): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if interruptA != 1 {
		t.Fatalf("A interrupt hits=%d, want exactly 1", interruptA)
	}
	if interruptsB != 0 {
		t.Fatalf("task B must be untouched by A release, B hits=%d", interruptsB)
	}
	if deletes != 0 {
		t.Fatalf("release must never DELETE sessions, deletes=%d", deletes)
	}
}

// stage4LifecycleBinding 构造 port/directory 与实例一致的 v2 生命周期 binding。
func stage4LifecycleBinding(t *testing.T, taskID, upstream string) domain.TaskBinding {
	t.Helper()
	raw, err := encodeV2BindingData(v2BindingData{Port: 4096, Directory: "/wt", TaskID: taskID,
		Origin: originCreated, Auth: v2BindingAuth{Kind: "derived"}})
	if err != nil {
		t.Fatal(err)
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  upstream,
		TerminalRef:        v2TerminalRef(taskID),
		Data:               raw,
	}
}
