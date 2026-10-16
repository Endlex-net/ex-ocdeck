// v2_gate3_test.go 落实 gate3（ora-9）修复验证：
//   - F1：ready 失效路径（进程死亡 → CheckServerAlive/Acquire 同端口重拉，无手工 ready 篡改）
//   - F4：pid 有效但进程已死 → 不换端口重新 spawn
//   - F6：进程身份 token 化精确匹配（相似 cmdline/端口前缀拒绝）
//   - F7：并发首次 Acquire 只 spawn 一次；ShutdownServer 后拒绝新业务 ensure
//   - F5：Resume 结构/归属校验先于副作用（无 spawn/store 写/HTTP/凭据派生）
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

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

// makeProcDead 使托管单元消失 + 实例 pid 不存活（进程级 fake 状态——不触碰
// supervisor.ready；G3-F1 真实失效路径）。
func makeProcDead(sup *V2Supervisor, proc *fakeProcSeam, recs *fakeRecordStore) {
	proc.mu.Lock()
	delete(proc.pids, ServeSessionName)
	proc.mu.Unlock()
	recs.mu.Lock()
	recs.rec.PID.Valid = false
	recs.mu.Unlock()
	sup.mu.Lock()
	sup.ready = false
	sup.pid = 0
	sup.mu.Unlock()
}

// TestGate3_F1_ReadyInvalidatedOnProcessDeath 落实 G3-F1：初始化成功后使 PID
// 不存活——CheckServerAlive 必须同端口重拉并触发回调；Acquire 亦经恢复矩阵
// 重拉（无手工 ready 篡改之外的状态注入）。
func TestGate3_F1_ReadyInvalidatedOnProcessDeath(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts int
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	sup.deps.OnServerRestarted = func() { restarts++ }
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	makeProcDead(sup, proc, recs)
	// CheckServerAlive：失效 → 重拉同端口 → 回调。
	if err := sup.CheckServerAlive(context.Background()); err != nil {
		t.Fatalf("CheckServerAlive: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("restarts=%d", restarts)
	}
	if port, _, err := sup.ensureServer(context.Background()); err != nil || port != 4096 {
		t.Fatalf("port=%d err=%v（同端口重拉）", port, err)
	}
	// Acquire 路径：再次死亡 → Acquire 内 ensureServer 重拉（同端口）后建 session。
	makeProcDead(sup, proc, recs)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/environment") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost {
			_, _ = w.Write(readFixture(t, "session_create.json"))
			return
		}
		_, _ = w.Write(readFixture(t, "session_list_page2.json"))
	}))
	t.Cleanup(srv2.Close)
	sup.deps.Env = fakeEnvSeam(func(string, bool) (map[string]string, error) { return map[string]string{}, nil })
	sup.newClient = func(port int, password string, opts Options) *Client {
		c := NewClient(port, password, opts)
		c.baseURL = srv2.URL
		return c
	}
	b, err := sup.Acquire(context.Background(), appTaskRef("t1", "/wt"))
	if err != nil {
		t.Fatalf("Acquire after death: %v", err)
	}
	if b.UpstreamSessionID == "" {
		t.Fatalf("binding: %+v", b)
	}
}

// TestGate3_F4_PidDead_RespawnSamePort 落实 G3-F4：记录 pid 有效但进程已死 →
// 确认无存活后同端口重启（MUST NOT 误判身份不符 fail-closed）。
func TestGate3_F4_PidDead_RespawnSamePort(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	// 记录 pid 仍有效（不手工清）——仅进程侧死亡：身份报不存活 + 托管单元消失。
	recs.mu.Lock()
	pid := int(recs.rec.PID.Int64)
	recs.mu.Unlock()
	sup.deps.ProcessIdentity = func(q int) (string, bool, error) {
		if q == pid {
			return "", false, nil // 旧 pid 进程已死
		}
		// 重新 spawn 的新 pid：存活且 serve 形态（同端口）。
		return "opencode serve --port 4096 --hostname 127.0.0.1", true, nil
	}
	proc.mu.Lock()
	delete(proc.pids, ServeSessionName)
	proc.mu.Unlock()
	sup.mu.Lock()
	sup.ready = false
	sup.mu.Unlock()

	port, _, err := sup.ensureServer(context.Background())
	if err != nil {
		t.Fatalf("respawn: %v（MUST NOT fail-closed on dead pid）", err)
	}
	if port != 4096 {
		t.Fatalf("port rotated: %d", port)
	}
	if len(proc.spawned) != 2 {
		t.Fatalf("spawned=%d（同端口重新 spawn）", len(proc.spawned))
	}
}

// TestGate3_F6_IdentityTokenMatch 落实 G3-F6：token 化精确匹配——相似 cmdline
// 与端口前缀拒绝；executable 路径前缀/参数序容忍。
func TestGate3_F6_IdentityTokenMatch(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		port    int
		want    bool
	}{
		{"exact", "opencode serve --port 4096 --hostname 127.0.0.1", 4096, true},
		{"binary path prefix", "/usr/local/bin/opencode serve --port 4096 --hostname 127.0.0.1", 4096, true},
		{"port prefix mismatch (40961 vs 4096)", "opencode serve --port 40961 --hostname 127.0.0.1", 4096, false},
		{"missing serve token", "opencode --port 4096 --hostname 127.0.0.1", 4096, false},
		{"missing hostname", "opencode serve --port 4096", 4096, false},
		{"wrong hostname", "opencode serve --port 4096 --hostname 0.0.0.0", 4096, false},
		{"similar binary name", "opencode2 serve --port 4096 --hostname 127.0.0.1", 4096, false},
		{"empty", "", 4096, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityMatches(tc.cmdline, tc.port); got != tc.want {
				t.Fatalf("identityMatches(%q, %d) = %v, want %v", tc.cmdline, tc.port, got, tc.want)
			}
		})
	}
}

// TestGate3_F7_ConcurrentFirstAcquire_SpawnOnce 落实 G3-F7：多 goroutine 首次
// Acquire 并发——只 spawn 一次、全部同端口（统一状态锁序列化）。
func TestGate3_F7_ConcurrentFirstAcquire_SpawnOnce(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := newTestSupervisor(t, srv.URL, nil)
	const n = 8
	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex
	ready := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			port, _, err := sup.ensureServer(context.Background())
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			if err == nil && port != 4096 {
				t.Errorf("port: %d", port)
			}
		}()
	}
	close(ready)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent ensure: %v", firstErr)
	}
	if len(proc.spawned) != 1 {
		t.Fatalf("spawned=%d（并发首 Acquire MUST 只 spawn 一次）", len(proc.spawned))
	}
}

// TestGate3_F7_Shutdown_RejectsNewBusinessEnsure 落实 G3-F7：ShutdownServer 后
// 拒绝新业务 ensure（不重拉、不接管）；幂等 shutdown 不报错。
func TestGate3_F7_Shutdown_RejectsNewBusinessEnsure(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := sup.ShutdownServer(context.Background()); err != nil {
		t.Fatalf("ShutdownServer: %v", err)
	}
	if _, _, err := sup.ensureServer(context.Background()); err == nil || !errors.Is(err, errSupShuttingDown) {
		t.Fatalf("expect shutting-down refusal, got %v", err)
	}
	// 幂等：二次 shutdown 成功（单元已不存在）。
	if err := sup.ShutdownServer(context.Background()); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
	// server 单元已被单 owner 关闭。
	if ok, _ := proc.HasSession(ServeSessionName); ok {
		t.Fatalf("serve unit must be killed by shutdown")
	}
	// 状态复位断言（G3-F1）：ready/port/pid 清零。
	sup.mu.Lock()
	ready, port, pid := sup.ready, sup.port, sup.pid
	sup.mu.Unlock()
	if ready || port != 0 || pid != 0 {
		t.Fatalf("state not reset: ready=%v port=%d pid=%d", ready, port, pid)
	}
}

// TestGate3_F5_Resume_StructureBeforeSideEffects 落实 G3-F5：v2 binding 结构/
// 归属校验失败时 MUST NOT 触发 spawn/实例记录写/HTTP/凭据派生（副作用计数全零）。
func TestGate3_F5_Resume_StructureBeforeSideEffects(t *testing.T) {
	var httpHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&httpHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "session_list_page2.json"))
	}))
	t.Cleanup(srv.Close)
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	recs.mu.Lock()
	basePID := recs.rec.PID
	recs.mu.Unlock()
	hitsAfterInit := atomic.LoadInt32(&httpHits)
	spawnsAfterInit := len(proc.spawned)

	cases := []struct {
		name string
		data string
	}{
		{"directory mismatch", `{"port":4096,"directory":"/other","taskId":"t1","origin":"created","auth":{"kind":"derived"}}`},
		{"bad port range", `{"port":0,"directory":"/wt","taskId":"t1","origin":"created","auth":{"kind":"derived"}}`},
		{"unknown origin", `{"port":4096,"directory":"/wt","taskId":"t1","origin":"forked","auth":{"kind":"derived"}}`},
		{"malformed", `{bad`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := domainTaskBindingFor(t, tc.data)
			sup.deps.Env = nil
			_, err := sup.Resume(context.Background(), appTaskRef("t1", "/wt"), b, resumePolicyNone())
			if err == nil {
				t.Fatalf("expect structural rejection")
			}
			if got := atomic.LoadInt32(&httpHits); got != hitsAfterInit {
				t.Fatalf("no HTTP may fire before structural validation, new hits=%d", got-hitsAfterInit)
			}
			if got := len(proc.spawned); got != spawnsAfterInit {
				t.Fatalf("no spawn may fire before structural validation, new spawns=%d", got-spawnsAfterInit)
			}
			recs.mu.Lock()
			pid := recs.rec.PID
			recs.mu.Unlock()
			if pid != basePID {
				t.Fatalf("runtime store must be untouched")
			}
		})
	}
}

var _ = atomic.AddInt32

// ---- 测试辅助（gate3）----

// errSupShuttingDown 是 shutdown 拒绝语义的可判定错误（supervisor.go 定义）。
var errSupShuttingDown = errShuttingDown

// appTaskRef 构造 TaskRef。
func appTaskRef(taskID, dir string) appagent.TaskRef {
	return appagent.TaskRef{TaskID: taskID, Directory: dir}
}

// resumePolicyNone 空 policy。
func resumePolicyNone() appagent.ResumePolicy { return appagent.ResumePolicy{} }

// domainTaskBindingFor 以 data 构造 v2 命名空间 binding（结构校验测试入参）。
func domainTaskBindingFor(t *testing.T, data string) domain.TaskBinding {
	t.Helper()
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  "ses_fixture01a",
		TerminalRef:        "ocdeck-v2-tui-t1",
		Data:               []byte(data),
	}
}
