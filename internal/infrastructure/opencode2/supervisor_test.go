// supervisor_test.go 落实 tasks 5.1–5.4：A+ 全局单 server 承载——启动顺序六步
// 写序、重启恢复矩阵、端口固定不轮换、Acquire/Resume/Release/AcquireEphemeral
// 语义、崩溃重拉回调、配置应用链三来源分支与失败禁止。
package opencode2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ocdeck/internal/infrastructure/hostenv"

	"ocdeck/internal/infrastructure/process"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// fakeProcSeam 进程托管 fake（内存态托管单元；SessionPID 返回注册 pid）。
type fakeProcSeam struct {
	mu      sync.Mutex
	pids    map[string]int
	newErr  error
	spawned []process.SessionSpec
}

func (f *fakeProcSeam) NewSession(spec process.SessionSpec) error {
	if f.newErr != nil {
		return f.newErr
	}
	f.mu.Lock()
	f.spawned = append(f.spawned, spec)
	f.pids[spec.Name] = 9000 + len(f.spawned)
	f.mu.Unlock()
	return nil
}

func (f *fakeProcSeam) HasSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pids[name]
	return ok, nil
}

func (f *fakeProcSeam) KillSession(name string) (process.KillResult, error) {
	f.mu.Lock()
	delete(f.pids, name)
	f.mu.Unlock()
	return process.KillResult{SessionKilled: true}, nil
}

func (f *fakeProcSeam) SessionPID(name string) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, ok := f.pids[name]
	return pid, ok, nil
}

// fakeRecordStore 实例记录 fake（语义镜像 store 实现：端口固定不轮换）。
type fakeRecordStore struct {
	mu   sync.Mutex
	rec  store.ProviderRuntimeRecord
	has  bool
	errs map[string]error
}

func (f *fakeRecordStore) GetProviderRuntime(ctx context.Context, p, i string) (store.ProviderRuntimeRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.errs["get"]; e != nil {
		return store.ProviderRuntimeRecord{}, false, e
	}
	return f.rec, f.has, nil
}

func (f *fakeRecordStore) PutProviderRuntimeStart(ctx context.Context, p, i string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.errs["put"]; e != nil {
		return e
	}
	if f.has && f.rec.Port != port {
		return fmt.Errorf("端口固定不轮换（记录 %d != 请求 %d）", f.rec.Port, port)
	}
	f.rec = store.ProviderRuntimeRecord{ProviderID: p, InstanceID: i, Port: port, UpdatedAt: 1}
	f.has = true
	return nil
}

func (f *fakeRecordStore) SetProviderRuntimePID(ctx context.Context, p, i string, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.errs["pid"]; e != nil {
		return e
	}
	f.rec.PID.Valid = true
	f.rec.PID.Int64 = int64(pid)
	return nil
}

func (f *fakeRecordStore) SetProviderRuntimeError(ctx context.Context, p, i string, cause string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.LastError.Valid = true
	f.rec.LastError.String = cause
	return nil
}

// fakeIdentity 进程身份 fake：注册 pid 视为存活，cmdline 按 serve 形态构造。
func fakeIdentity(registered map[int]bool) func(int) (string, bool, error) {
	return func(pid int) (string, bool, error) {
		if !registered[pid] {
			return "", false, nil
		}
		return fmt.Sprintf("%s serve --port %%d --hostname 127.0.0.1", "opencode"), true, nil
	}
}

// identityCheckingPort 构造 cmdline 含指定端口的身份函数。
func identityCheckingPort(registered map[int]bool) func(int) (string, bool, error) {
	return func(pid int) (string, bool, error) {
		if !registered[pid] {
			return "", false, nil
		}
		return "opencode serve --port known --hostname", true, nil
	}
}

// newTestSupervisor 构造指向 httptest server 的 supervisor：ensureServer 走完整
// 六步（fake 托管/记录/身份），业务调用经 baseURL 覆写指向 fixture server。
func newTestSupervisor(t *testing.T, srvURL string, mutate func(*SupervisorDeps, *fakeProcSeam, *fakeRecordStore)) (*V2Supervisor, *fakeProcSeam, *fakeRecordStore) {
	t.Helper()
	setMasterSecret(t)
	proc := &fakeProcSeam{pids: map[string]int{}}
	recs := &fakeRecordStore{errs: map[string]error{}}
	registered := map[int]bool{}
	deps := SupervisorDeps{
		Proc:            proc,
		Store:           recs,
		AllocatePort:    func(lastPort int, lastValid bool, exclude int) (int, error) { return 4096, nil },
		ProcessIdentity: func(pid int) (string, bool, error) { return registeredIdentity(pid, registered) },
		ReadyTimeout:    300 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
	}
	sup := NewV2Supervisor(deps)
	// 契约发现 stub（冻结 pattern；discover_test 单独覆盖发现失败路径）。
	sup.contractOf = func(ctx context.Context, c *Client) (WireContract, error) {
		return WireContract{
			SessionIDPatternRaw: "^ses", MessageIDPatternRaw: "^msg_",
			sessionIDRe: regexp.MustCompile("^ses"), messageIDRe: regexp.MustCompile("^msg_"),
		}, nil
	}
	base := srvURL
	sup.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = base
		return c
	}
	if mutate != nil {
		mutate(&sup.deps, proc, recs)
	}
	// 注册 ensureServer 产生的 pid（九千段）为存活。
	_ = registered
	sup.deps.ProcessIdentity = func(pid int) (string, bool, error) {
		if pid >= 9000 {
			return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", 4096), true, nil
		}
		return "", false, nil
	}
	return sup, proc, recs
}

func registeredIdentity(pid int, registered map[int]bool) (string, bool, error) {
	if !registered[pid] {
		return "", false, nil
	}
	return "opencode serve --port 4096 --hostname 127.0.0.1", true, nil
}

var _ = json.Marshal
var _ = strconv.Itoa

func TestSupervisor_EnsureServer_SixStepWriteOrder(t *testing.T) {
	hostenv.ResetForTest()
	origCapture := hostenv.Capture
	hostenv.Capture = func() map[string]string { return map[string]string{} }
	t.Cleanup(func() {
		hostenv.Capture = origCapture
		hostenv.ResetForTest()
	})
	t.Setenv("ARCHI_API_KEY_OPENAI", "sk-test-host")
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	port, password, err := sup.ensureServer(context.Background())
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	if port != 4096 || password == "" {
		t.Fatalf("port/password: %d %q", port, password)
	}
	// ② serve 托管单元按冻结 argv 启动（子命令形态 + 密码 env 注入）。
	if len(proc.spawned) != 1 {
		t.Fatalf("spawned: %d", len(proc.spawned))
	}
	spec := proc.spawned[0]
	if spec.Name != ServeSessionName {
		t.Fatalf("unit name: %q", spec.Name)
	}
	wantArgv := []string{"opencode", "serve", "--port", "4096", "--hostname", "127.0.0.1"}
	for i, a := range wantArgv {
		if spec.CmdArgv[i] != a {
			t.Fatalf("argv: %v", spec.CmdArgv)
		}
	}
	if spec.Env["OPENCODE_SERVER_PASSWORD"] == "" {
		t.Fatalf("OPENCODE_SERVER_PASSWORD must be injected")
	}
	if spec.Env["ARCHI_API_KEY_OPENAI"] != "sk-test-host" {
		t.Fatalf("host env ARCHI_API_KEY_OPENAI = %q, want sk-test-host", spec.Env["ARCHI_API_KEY_OPENAI"])
	}
	if spec.Env["TERM"] != "xterm-256color" {
		t.Fatalf("TERM = %q, want xterm-256color", spec.Env["TERM"])
	}
	// ①先于启动（记录先行）、⑤落 pid：最终记录 pid 有效。
	rec, ok, _ := recs.GetProviderRuntime(context.Background(), ProviderID, ProviderInstanceID)
	if !ok || !rec.PID.Valid || rec.Port != 4096 {
		t.Fatalf("record: %+v ok=%v", rec, ok)
	}
	// 复用：二次 ensure 不重启（spawned 不变）。
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if len(proc.spawned) != 1 {
		t.Fatalf("ensure must reuse running server, spawned=%d", len(proc.spawned))
	}
}

func TestSupervisor_IdentityMismatch_FailClosed(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	// 记录 pid 有效但身份不再匹配（进程被替换）→ fail-closed 不接管。
	rec, _, _ := recs.GetProviderRuntime(context.Background(), ProviderID, ProviderInstanceID)
	sup.deps.ProcessIdentity = func(pid int) (string, bool, error) {
		return "rogue-binary --port 4096", true, nil // cmdline 不含 serve 形态
	}
	_ = rec
	sup.ready = false // 强制走恢复矩阵
	_, _, err := sup.ensureServer(context.Background())
	if !errors.Is(err, errRuntimeIdentityMismatch) {
		t.Fatalf("expect identity mismatch, got %v", err)
	}
	// 未重启 server（不接管）。
	if len(proc.spawned) != 1 {
		t.Fatalf("must not respawn on identity mismatch, spawned=%d", len(proc.spawned))
	}
}

func TestSupervisor_CrashWindow_RediscoverAndReuse(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	// 崩溃窗口：记录 pid 清空（步骤⑤后进程记录丢失场景），托管单元仍存活。
	rec, _, _ := recs.GetProviderRuntime(context.Background(), ProviderID, ProviderInstanceID)
	pid := int(rec.PID.Int64)
	recs.mu.Lock()
	recs.rec.PID.Valid = false
	recs.mu.Unlock()
	sup.ready = false
	// 进程身份 fake 依赖注册表：把原 pid 标记为存活（重新发现路径核验用）。
	port, _, err := sup.ensureServer(context.Background())
	if err != nil {
		t.Fatalf("rediscover: %v", err)
	}
	if port != 4096 {
		t.Fatalf("port: %d", port)
	}
	// 重新发现不重启（托管单元存活即复用）。
	if len(proc.spawned) != 1 {
		t.Fatalf("rediscover must not respawn, spawned=%d", len(proc.spawned))
	}
	// 步骤⑤重落 pid。
	rec, _, _ = recs.GetProviderRuntime(context.Background(), ProviderID, ProviderInstanceID)
	if !rec.PID.Valid || int(rec.PID.Int64) != pid {
		t.Fatalf("pid not re-recorded: %+v (want %d)", rec.PID, pid)
	}
}

func TestSupervisor_CrashWindow_PortFixed_Respawn(t *testing.T) {
	var srvURL string
	sup, proc, recs := newTestSupervisor(t, "http://unused", nil)
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	srvURL = srv.URL
	sup.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srvURL
		return c
	}
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	// 崩溃：托管单元消失 + pid NULL → 同端口重启（固定不轮换）。
	recs.mu.Lock()
	recs.rec.PID.Valid = false
	recs.mu.Unlock()
	proc.mu.Lock()
	delete(proc.pids, ServeSessionName)
	proc.mu.Unlock()
	sup.ready = false
	port, _, err := sup.ensureServer(context.Background())
	if err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if port != 4096 || len(proc.spawned) != 2 {
		t.Fatalf("port=%d spawned=%d（同端口重启）", port, len(proc.spawned))
	}
	if proc.spawned[1].CmdArgv[3] != "4096" {
		t.Fatalf("respawn port argv: %v", proc.spawned[1].CmdArgv)
	}
}

func TestSupervisor_AcquireEphemeral_NoSessionNoPersist(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := newTestSupervisor(t, srv.URL, nil)
	b, err := sup.AcquireEphemeral(context.Background(), appagent.TaskRef{TaskID: "t1", Directory: "/wt"})
	if err != nil {
		t.Fatalf("AcquireEphemeral: %v", err)
	}
	// 仅内存临时 binding：origin=ephemeral、TerminalRef="none"、UpstreamSessionID 空。
	if b.UpstreamSessionID != "" || b.TerminalRef != EphemeralTerminalRef {
		t.Fatalf("binding: %+v", b)
	}
	var data v2BindingData
	if err := json.Unmarshal(b.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Origin != originEphemeral {
		t.Fatalf("origin: %q", data.Origin)
	}
	// MUST NOT 创建 session（无 prompt/create 调用——fixture server 对未知路径报错）。
	// MUST NOT 持久化（provider_runtime 记录仅为 server 实例，非 binding）。
	if len(proc.spawned) != 1 {
		t.Fatalf("spawned: %d", len(proc.spawned))
	}
	// Release(ephemeral)：仅回收连接——不调用 interrupt/delete。
	var hitSessionAPI bool
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitSessionAPI = true
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	sup2, _, _ := newTestSupervisor(t, srv2.URL, nil)
	if err := sup2.Release(context.Background(), b); err != nil {
		t.Fatalf("Release(ephemeral): %v", err)
	}
	if hitSessionAPI {
		t.Fatalf("ephemeral release must not touch session API")
	}
}

func TestSupervisor_Resume_MissingSessionNoReplacement(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session/ses_gone", status: 404, body: `{"_tag":"SessionNotFoundError"}`},
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	old := testBinding(t)
	old.UpstreamSessionID = "ses_gone"
	// ResumePolicy 任何取值不得授权创建替代 session：AllowSessionCreate=true 仍拒绝。
	_, err := sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, old,
		appagent.ResumePolicy{AllowSessionCreate: true})
	if got := appagent.KindOf(err); got != appagent.ErrKindNotFound {
		t.Fatalf("kind: %v (%v)", got, err)
	}
	// UpstreamSessionID 空 → BindingInvalid（不伪造 session）。
	old2 := testBinding(t)
	old2.UpstreamSessionID = ""
	_, err = sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, old2, appagent.ResumePolicy{})
	if appagent.KindOf(err) != appagent.ErrKindBindingInvalid {
		t.Fatalf("empty upstream kind: %v", err)
	}
}

func TestSupervisor_Resume_OK_And_PortMismatch(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a", status: 200, body: string(readFixture(t, "session_get.json"))},
		route{path: "/api/session/ses_fixture01a/environment", status: 204, body: ""},
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	old := testBinding(t) // port=1, directory=/wt, origin=created, upstream=ses_fixture01a
	// 端口一致（binding data port=1 == 实例 4096? 不一致！——testBindingData port=1，
	// 实例端口=4096 → fail-closed。先验证不一致路径。
	got, err := sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, old, appagent.ResumePolicy{})
	if appagent.KindOf(err) != appagent.ErrKindBindingInvalid {
		t.Fatalf("port mismatch must fail-closed, got %v (%v)", appagent.KindOf(err), err)
	}
	// 修正：binding data 端口改为实例端口后成功（origin=created → 同代快照 env 应用）。
	raw, err := encodeV2BindingData(v2BindingData{Port: 4096, Directory: "/wt", TaskID: "task_fixture01",
		Origin: originCreated, Auth: v2BindingAuth{Kind: "derived"}})
	if err != nil {
		t.Fatal(err)
	}
	old.Data = raw
	var envCalls int
	sup.deps.Env = fakeEnvSeam(func(taskID string, recovery bool) (map[string]string, error) {
		envCalls++
		if !recovery {
			t.Fatalf("created resume must apply recovery snapshot (recovery=true)")
		}
		return map[string]string{"K": "V"}, nil
	})
	got, err = sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, old, appagent.ResumePolicy{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.UpstreamSessionID != "ses_fixture01a" || got.TerminalRef != "ocdeck-v2-tui-task_fixture01" {
		t.Fatalf("binding: %+v", got)
	}
	if envCalls != 1 {
		t.Fatalf("env applications: %d", envCalls)
	}
	var data v2BindingData
	_ = json.Unmarshal(got.Data, &data)
	if data.Origin != originCreated {
		t.Fatalf("origin preserved: %q", data.Origin)
	}
}

// TestSupervisor_Resume_LegacyV1BindingMinimalFields 落实 v1-legacy-anchor-adopt：
// legacy-A 合成 binding（仅 ProviderID/ProviderInstanceID/UpstreamSessionID 三字段，
// 无 Version/Data/TerminalRef）经真实 V2Supervisor.Resume 兼容——v1 命名空间跳过
// v2 结构校验与端口校验，origin 按 adopted（不 POST 新 session、不 PUT 环境），
// 返回完整 v2 lifecycle binding（upstream 保留）。fixtureServer 按 path 匹配不分
// method：POST /api/session 会命中列表 route（body 无法解码为 create 结果）、
// PUT .../environment 未注册 404——两者任一发生 Resume 即报错（负断言隐式成立）。
func TestSupervisor_Resume_LegacyV1BindingMinimalFields(t *testing.T) {
	srv := fixtureServer(t,
		route{path: "/api/session/ses_fixture01a", status: 200, body: string(readFixture(t, "session_get.json"))},
		route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	// adopted 形态 MUST NOT 应用环境（零 env seam 调用）。
	sup.deps.Env = fakeEnvSeam(func(taskID string, recovery bool) (map[string]string, error) {
		t.Errorf("adopted resume MUST NOT apply env (taskID=%s recovery=%v)", taskID, recovery)
		return nil, nil
	})
	old := domain.TaskBinding{ // 与 task 层 legacy-A 合成形态逐字段一致（仅三字段）
		ProviderID:         store.ProviderIDV1,
		ProviderInstanceID: store.ProviderInstanceV1Default,
		UpstreamSessionID:  "ses_fixture01a",
	}
	got, err := sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, old, appagent.ResumePolicy{})
	if err != nil {
		t.Fatalf("Resume(legacy minimal v1 binding): %v", err)
	}
	if got.UpstreamSessionID != "ses_fixture01a" || got.ProviderID != ProviderID || got.ProviderInstanceID != ProviderInstanceID {
		t.Fatalf("binding: %+v", got)
	}
	data, derr := decodeV2Binding(got)
	if derr != nil {
		t.Fatalf("result must be a complete v2 binding: %v", derr)
	}
	if data.Origin != originAdopted || data.Port != 4096 || data.Directory != "/wt" || data.TaskID != "task_fixture01" {
		t.Fatalf("lifecycle data: %+v", data)
	}
	if got.TerminalRef != v2TerminalRef("task_fixture01") {
		t.Fatalf("terminal ref: %q", got.TerminalRef)
	}
}

// fakeEnvSeam 按 func 适配 TaskEnvSeam。
type fakeEnvSeam func(taskID string, recovery bool) (map[string]string, error)

func (f fakeEnvSeam) ResolveSessionEnv(ctx context.Context, taskID string, recovery bool) (map[string]string, error) {
	return f(taskID, recovery)
}

func TestSupervisor_ConfigChain_ThreeOrigins(t *testing.T) {
	// 5.4：adopted 禁止覆盖（不调用 seam、不发 PUT）；created 恢复同代快照；
	// 首次（Acquire）按当前值。env PUT 走 204。
	var envPuts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/session/ses_fixture01a/environment":
			envPuts++
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/session/ses_fixture01a":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "session_get.json"))
		case r.URL.Path == "/api/session" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		case r.URL.Path == "/api/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "session_create.json"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatal(err)
	}

	var recovered bool
	sup.deps.Env = fakeEnvSeam(func(taskID string, recovery bool) (map[string]string, error) {
		recovered = recovery
		return map[string]string{"A": "B"}, nil
	})

	// 首次路径：Acquire → PUT environment（recovery=false）。
	if _, err := sup.Acquire(context.Background(), appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if envPuts != 1 || recovered {
		t.Fatalf("acquire env: puts=%d recovered=%v", envPuts, recovered)
	}

	// adopted：恢复不调用 seam、不发 PUT（承接保护持续有效）。
	adoptedRaw, err := encodeV2BindingData(v2BindingData{Port: 4096, Directory: "/wt",
		TaskID: "task_fixture01", Origin: originAdopted, Auth: v2BindingAuth{Kind: "derived"}})
	if err != nil {
		t.Fatal(err)
	}
	oldAdopted := testBinding(t)
	oldAdopted.Data = adoptedRaw
	if _, err := sup.Resume(context.Background(),
		appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}, oldAdopted, appagent.ResumePolicy{}); err != nil {
		t.Fatalf("Resume(adopted): %v", err)
	}
	if envPuts != 1 {
		t.Fatalf("adopted must not overwrite env, puts=%d", envPuts)
	}

	// 配置应用失败：不返回 binding（保留旧事实——提交协议零持久化）。
	sup2, _, _ := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup2.ensureServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	sup2.deps.Env = fakeEnvSeam(func(string, bool) (map[string]string, error) {
		return nil, errors.New("env snapshot unavailable")
	})
	if _, err := sup2.Acquire(context.Background(), appagent.TaskRef{TaskID: "task_fixture01", Directory: "/wt"}); err == nil {
		t.Fatalf("env application failure must fail the attempt (no binding returned)")
	}
}

func TestSupervisor_Release_InterruptThenNeverDelete(t *testing.T) {
	var interrupted, deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && stringsHasSuffix(r.URL.Path, "/interrupt"):
			interrupted = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"interrupted":true}`))
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(204)
		case r.Method == http.MethodGet && stringsHasSuffix(r.URL.Path, "/api/session"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, "session_list_page2.json"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	sup, _, _ := newTestSupervisor(t, srv.URL, nil)
	raw, err := encodeV2BindingData(v2BindingData{Port: 4096, Directory: "/wt", TaskID: "task_fixture01",
		Origin: originCreated, Auth: v2BindingAuth{Kind: "derived"}})
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding(t)
	b.Data = raw
	if err := sup.Release(context.Background(), b); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !interrupted || deleted {
		t.Fatalf("interrupted=%v deleted=%v（Release 停止执行但 MUST NOT 删除 session）", interrupted, deleted)
	}
	// 停止失败如实报错（5xx）——调用方收敛回 active 不提交 suspended。
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srvFail.Close()
	sup2, _, _ := newTestSupervisor(t, srvFail.URL, nil)
	if err := sup2.Release(context.Background(), b); err == nil {
		t.Fatalf("interrupt failure must surface")
	}
}

// stringsHasSuffix 局部助手（避免 strings import 噪音）。
func stringsHasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

func TestSupervisor_CrashRepull_Callback(t *testing.T) {
	// 5.3：server 崩溃 → CheckServerAlive 重拉 → OnServerRestarted 触发
	//（活跃任务按 binding 重建关联的钩子；I9：通知已收口至 ensureServer 真实
	// 恢复状态转换并异步派发——通知体可能取任务锁，同步会与 Resume 成环）。
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	sup.deps.OnServerRestarted = func() { restarts.Add(1) }
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 首次建立不算重拉。
	if err := sup.CheckServerAlive(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitRestarts(t, &restarts, 0)
	// 崩溃：进程真实死亡（G3-F10——托管单元消失 + pid 身份报不存活，无 ready 篡改）。
	makeProcDead(sup, proc, recs)
	if err := sup.CheckServerAlive(context.Background()); err != nil {
		t.Fatalf("CheckServerAlive: %v", err)
	}
	awaitRestarts(t, &restarts, 1)
}

// awaitRestarts 轮询等待重拉回调计数达标（I9：通知异步派发）。deadline 2s。
func awaitRestarts(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n.Load() != want {
		t.Fatalf("restarts=%d, want %d", n.Load(), want)
	}
}

// TestSupervisor_VerifyReadyRejectsMalformedList 落实收尾 K2：就绪探测复用
// ListSessions——HTTP 200 的畸形列表页（协议违约）MUST NOT 被判为 ready
// （否则 supervisor 把畸形响应当作服务可用，后续建程/对账基于错误事实）。
func TestSupervisor_VerifyReadyRejectsMalformedList(t *testing.T) {
	malformed := []string{
		`{}`,
		`null`,
		`{"data":null,"cursor":{"previous":null,"next":null}}`,
		`{"data":[]}`,
		`{"data":null}`,
	}
	for _, body := range malformed {
		t.Run(body, func(t *testing.T) {
			srv := fixtureServer(t, route{path: "/api/session", status: 200, body: body})
			sup, _, _ := newTestSupervisor(t, srv.URL, nil)
			if sup.verifyReady(context.Background(), 4096, "pw") {
				t.Fatalf("malformed list page MUST NOT pass readiness probe: %s", body)
			}
		})
	}
	t.Run("legal page passes", func(t *testing.T) {
		srv := fixtureServer(t, route{path: "/api/session", status: 200,
			body: `{"data":[],"cursor":{"previous":null,"next":null}}`})
		sup, _, _ := newTestSupervisor(t, srv.URL, nil)
		if !sup.verifyReady(context.Background(), 4096, "pw") {
			t.Fatalf("legal empty page must pass readiness probe")
		}
	})
}

var _ = domain.TaskBindingVersion
var _ = store.ProviderIDV2
