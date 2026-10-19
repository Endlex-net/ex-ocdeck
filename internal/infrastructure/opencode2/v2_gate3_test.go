// v2_gate3_test.go 落实 gate3（ora-9）修复验证：
//   - F10：ready 快速路径进程死亡自发现（无手工 ready 篡改——verifyAliveLocked
//     验活失败 → 清 ready → 恢复矩阵；CheckServerAlive/Acquire 同路径）
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

// makeProcDead 使进程真实死亡（G3-F10：不触碰 supervisor.ready/pid）——托管
// 单元消失 + 记录/in 内存 pid 经身份 seam 报不存活；ready 快速路径须经
// verifyAliveLocked 自行发现失效 → 清 ready → 命中恢复矩阵。
func makeProcDead(sup *V2Supervisor, proc *fakeProcSeam, recs *fakeRecordStore) {
	proc.mu.Lock()
	delete(proc.pids, ServeSessionName)
	proc.mu.Unlock()
	sup.mu.Lock()
	deadPID := sup.pid
	sup.mu.Unlock()
	if deadPID <= 0 {
		recs.mu.Lock()
		deadPID = int(recs.rec.PID.Int64)
		recs.mu.Unlock()
	}
	if deadPID <= 0 {
		return
	}
	base := sup.deps.ProcessIdentity
	sup.deps.ProcessIdentity = func(q int) (string, bool, error) {
		if q == deadPID {
			return "", false, nil // 旧 pid 进程已死
		}
		return base(q)
	}
}

// supervisorReadyLocked 在持锁下读取快速路径验活结果（测试观测面）。
func supervisorReadyLocked(sup *V2Supervisor) bool {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.verifyAliveLocked()
}

// TestGate3_F10_ReadyFastPathDiscoversProcessDeath 落实 G3-F10（增强 F1）：
// 进程真实死亡（无任何 ready 手工篡改）——ready 快速路径必须经
// verifyAliveLocked 自行发现（验活失败）→ 清 ready → 命中恢复矩阵重拉；
// CheckServerAlive 触发回调恰一次、Acquire 亦经同一快速路径恢复后建 session。
func TestGate3_F10_ReadyFastPathDiscoversProcessDeath(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts int
	sup, proc, recs := newTestSupervisor(t, srv.URL, nil)
	sup.deps.OnServerRestarted = func() { restarts++ }
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	// 快速路径验活初始通过。
	if !supervisorReadyLocked(sup) {
		t.Fatalf("fast path must be alive after init")
	}
	makeProcDead(sup, proc, recs)
	// 快速路径验活失败（自行发现进程死亡——非测试注入 ready）。
	if supervisorReadyLocked(sup) {
		t.Fatalf("fast path liveness check must fail after real process death")
	}
	// CheckServerAlive：快速路径失效 → 重拉同端口 → 回调恰一次。
	if err := sup.CheckServerAlive(context.Background()); err != nil {
		t.Fatalf("CheckServerAlive: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("restarts=%d", restarts)
	}
	if !supervisorReadyLocked(sup) {
		t.Fatalf("fast path must be alive after repull")
	}
	// 复用：快速路径验活通过 → 不再重拉（回调不增）。
	if port, _, err := sup.ensureServer(context.Background()); err != nil || port != 4096 {
		t.Fatalf("port=%d err=%v（同端口重拉）", port, err)
	}
	if err := sup.CheckServerAlive(context.Background()); err != nil {
		t.Fatalf("CheckServerAlive(reuse): %v", err)
	}
	if restarts != 1 {
		t.Fatalf("reuse must not re-callback, restarts=%d", restarts)
	}
	// Acquire 路径：再次死亡 → Acquire 内 ensureServer 快速路径自行发现 → 恢复
	// 矩阵重拉（同端口）后建 session。
	makeProcDead(sup, proc, recs)
	if supervisorReadyLocked(sup) {
		t.Fatalf("fast path must fail after second death")
	}
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
	// 记录 pid 仍有效（不手工清）——仅进程侧真实死亡（G3-F10：无 ready 篡改，
	// 快速路径自发现 → 记录 pid 已死 → 托管单元消失 → 同端口重启）。
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
	makeProcDead(sup, proc, recs)

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
