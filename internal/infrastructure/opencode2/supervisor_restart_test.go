// supervisor_restart_test.go 落实可选窄接口 ServerRestarter 的 V2Supervisor 实现
// 验证（操作者发起重启）：杀掉当前 serve 并以同端口重新拉起；OnServerRestarted
// 经 ensureServer 真实恢复状态转换异步触发一次；shuttingDown 拒绝；并发重启
// 单飞（仅一次额外 spawn）。
package opencode2

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	"ocdeck/internal/infrastructure/process"
)

// restartTestSupervisor 构造重启测试 supervisor：ProcessIdentity 以 proc.pids 为
// 准（存活当且仅当托管单元仍登记该 pid）——默认 newTestSupervisor 的 pid>=9000
// 恒存活 fake 会让 ensureServer 恢复矩阵误判旧 pid 仍存活而跳过重新 spawn，故
// 重启路径必须让 KillSession 后旧 pid 真正报死。注意：newTestSupervisor 在
// mutate 之后会覆盖 ProcessIdentity，故此处必须在构造完成后重新注入。
func restartTestSupervisor(t *testing.T, srvURL string, onRestarted func()) (*V2Supervisor, *fakeProcSeam, *fakeRecordStore) {
	t.Helper()
	sup, proc, recs := newTestSupervisor(t, srvURL, nil)
	sup.deps.ProcessIdentity = func(pid int) (string, bool, error) {
		proc.mu.Lock()
		registered := false
		for _, rp := range proc.pids {
			if rp == pid {
				registered = true
				break
			}
		}
		proc.mu.Unlock()
		if !registered {
			return "", false, nil
		}
		return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", 4096), true, nil
	}
	sup.deps.OnServerRestarted = onRestarted
	return sup, proc, recs
}

// TestSupervisor_RestartServer_KillAndRespawn 落实 ServerRestarter 契约：
// ensureServer 建立后 RestartServer 杀掉当前 serve 并以同端口重新拉起；
// everReady 已置位 → ensureServer 异步触发 OnServerRestarted 恰好一次。
func TestSupervisor_RestartServer_KillAndRespawn(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() { restarts.Add(1) })

	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	if initialSpawns != 1 {
		t.Fatalf("initial spawns=%d, want 1", initialSpawns)
	}

	if err := sup.RestartServer(context.Background()); err != nil {
		t.Fatalf("RestartServer: %v", err)
	}
	// 杀掉旧 serve 并以同端口重新拉起：spawn 计数 +1（共 2）。
	if len(proc.spawned) != initialSpawns+1 {
		t.Fatalf("spawns after restart=%d, want %d", len(proc.spawned), initialSpawns+1)
	}
	// 重启后仍就绪。
	sup.mu.Lock()
	ready := sup.ready
	port := sup.port
	sup.mu.Unlock()
	if !ready || port != 4096 {
		t.Fatalf("after restart ready=%v port=%d, want ready=true port=4096", ready, port)
	}
	// everReady 已置位 → ensureServer 真实恢复状态转换异步触发 OnServerRestarted 一次。
	awaitRestarts(t, &restarts, 1)
}

// TestSupervisor_RestartServer_ShuttingDownRejects 落实关停中拒绝：
// ShutdownServer 置 shuttingDown 后 RestartServer MUST 返回 runtime_unavailable。
func TestSupervisor_RestartServer_ShuttingDownRejects(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, _, _ := restartTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	if err := sup.ShutdownServer(context.Background()); err != nil {
		t.Fatalf("ShutdownServer: %v", err)
	}
	err := sup.RestartServer(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("shuttingDown reject kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(err), err)
	}
	if !errors.Is(err, errShuttingDown) {
		t.Fatalf("expect errShuttingDown sentinel, got %v", err)
	}
}

// TestSupervisor_RestartServer_ConcurrentSingleFlight 落实并发单飞：
// 并发 RestartServer 仅产生一次额外 spawn（超出初始建立的 1 次）——经同一
// s.mu / ensureServerOnce 串行，不重复杀拉。
func TestSupervisor_RestartServer_ConcurrentSingleFlight(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() {})
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	// barrier：所有 goroutine 同时起跑，确保首个 TryLock 成 leader、其余命中
	// waiter 路径（restartMu 串行合并），验证后到者不重复杀拉。
	ready := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-ready
			_ = sup.RestartServer(context.Background())
		}()
	}
	close(ready)
	wg.Wait()

	// 并发重启：仅一次额外 spawn 超出初始（restartMu 单飞合并；waiter 命中
	// ensureServer 快速复用路径，无 spawn）。
	extra := len(proc.spawned) - initialSpawns
	if extra != 1 {
		t.Fatalf("concurrent restart extra spawns=%d, want 1 (total spawns=%d)", extra, len(proc.spawned))
	}
}

// killFailsProc 包装 fakeProcSeam，使 KillSession 返回 SessionKilled=false+nil
// error（模拟 process/reaper.go 快照失败/kill 失败形态：Disposition 非
// SnapshotMissingDegraded → fail-closed）。
type killFailsProc struct {
	SupervisorProcSeam
	inner *fakeProcSeam
}

func (k *killFailsProc) NewSession(spec process.SessionSpec) error { return k.inner.NewSession(spec) }
func (k *killFailsProc) HasSession(name string) (bool, error)      { return k.inner.HasSession(name) }
func (k *killFailsProc) SessionPID(name string) (int, bool, error) { return k.inner.SessionPID(name) }
func (k *killFailsProc) KillSession(name string) (process.KillResult, error) {
	// 仍删除托管单元登记（与生产 kill-failed 不同，但本测试聚焦 SessionKilled=false
	// 的失败判定，不依赖 HasSession）。
	_, _ = k.inner.KillSession(name)
	return process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed}, nil
}

// sessionGoneProc 模拟 KillSession 返回 SnapshotMissingDegraded（会话已消失）+
// 旧 pid 已死（ProcessIdentity !alive）——重启须跳过 kill 失败判定，经
// waitServeDead 确认死亡后重拉成功。
type sessionGoneProc struct {
	SupervisorProcSeam
	inner *fakeProcSeam
}

func (g *sessionGoneProc) NewSession(spec process.SessionSpec) error { return g.inner.NewSession(spec) }
func (g *sessionGoneProc) HasSession(name string) (bool, error)      { return g.inner.HasSession(name) }
func (g *sessionGoneProc) SessionPID(name string) (int, bool, error) { return g.inner.SessionPID(name) }
func (g *sessionGoneProc) KillSession(name string) (process.KillResult, error) {
	// 会话已消失：不删除登记（本就无），返回 degraded。
	return process.KillResult{SessionKilled: false, Disposition: process.DispositionSnapshotMissingDegraded}, nil
}

// TestSupervisor_RestartServer_KillNotDead_DoesNotSucceed 落实 Issue 1：
// KillSession 返回 SessionKilled=false → RestartServer MUST 返回
// runtime_unavailable，MUST NOT 进入 ensureServerLocked（无额外 spawn）、
// MUST NOT 触发 OnServerRestarted。
func TestSupervisor_RestartServer_KillNotDead_DoesNotSucceed(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() { restarts.Add(1) })
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	// 注入 kill 失败的 proc seam。
	sup.deps.Proc = &killFailsProc{inner: proc}

	err := sup.RestartServer(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("kill-failed kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(err), err)
	}
	if !errors.Is(err, errRestartKillFailed) {
		t.Fatalf("expect errRestartKillFailed sentinel, got %v", err)
	}
	// MUST NOT 进入 ensureServerLocked（无额外 spawn）。
	if len(proc.spawned) != initialSpawns {
		t.Fatalf("spawns=%d, want %d (kill-failed MUST NOT relaunch)", len(proc.spawned), initialSpawns)
	}
	// MUST NOT 触发 OnServerRestarted。
	awaitRestarts(t, &restarts, 0)
}

// aliveForeverProc 包装 fakeProcSeam，KillSession 正常删除登记但 ProcessIdentity
// 始终报旧 pid 存活（模拟逃逸进程 reparent 到 init 仍存活）——waitServeDead 超时。
type aliveForeverProc struct {
	SupervisorProcSeam
	inner *fakeProcSeam
}

func (a *aliveForeverProc) NewSession(spec process.SessionSpec) error { return a.inner.NewSession(spec) }
func (a *aliveForeverProc) HasSession(name string) (bool, error)      { return a.inner.HasSession(name) }
func (a *aliveForeverProc) SessionPID(name string) (int, bool, error) { return a.inner.SessionPID(name) }
func (a *aliveForeverProc) KillSession(name string) (process.KillResult, error) {
	return a.inner.KillSession(name)
}

// TestSupervisor_RestartServer_WaitTimeout_DoesNotSucceed 落实 Issue 1：
// KillSession 成功（SessionKilled=true）但旧 pid 仍存活（逃逸）→ waitServeDead
// 超时 → RestartServer MUST 返回 runtime_unavailable，MUST NOT 进入
// ensureServerLocked（无额外 spawn）、MUST NOT 触发 OnServerRestarted。
func TestSupervisor_RestartServer_WaitTimeout_DoesNotSucceed(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() { restarts.Add(1) })
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	sup.mu.Lock()
	oldPid := sup.pid
	sup.mu.Unlock()
	// ProcessIdentity 始终报旧 pid 存活（逃逸进程）——waitServeDead 必超时。
	sup.deps.ProcessIdentity = func(pid int) (string, bool, error) {
		if pid == oldPid {
			return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", 4096), true, nil
		}
		return "", false, nil
	}
	sup.deps.Proc = &aliveForeverProc{inner: proc}

	err := sup.RestartServer(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("wait-timeout kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(err), err)
	}
	if !errors.Is(err, errRestartKillFailed) {
		t.Fatalf("expect errRestartKillFailed sentinel, got %v", err)
	}
	if len(proc.spawned) != initialSpawns {
		t.Fatalf("spawns=%d, want %d (wait-timeout MUST NOT relaunch)", len(proc.spawned), initialSpawns)
	}
	awaitRestarts(t, &restarts, 0)
}

// TestSupervisor_RestartServer_SessionGone_PidDead_Proceeds 落实 Issue 1 细分：
// KillSession 返回 SnapshotMissingDegraded（会话已消失）且旧 pid 已死 → 重启
// 须跳过 kill-failed 判定，经 waitServeDead 确认死亡后重拉成功（spawn +1），
// 不得误报 kill 失败。区别于 KillFailed/SnapshotFailed（会话仍存活 kill 未成功）。
func TestSupervisor_RestartServer_SessionGone_PidDead_Proceeds(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() { restarts.Add(1) })
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	sup.mu.Lock()
	oldPid := sup.pid
	sup.mu.Unlock()
	// 会话已消失 proc：KillSession 返回 degraded。ProcessIdentity 仍以 proc.pids
	// 为准——先清掉旧 pid 登记使 waitServeDead 判死。
	proc.mu.Lock()
	delete(proc.pids, ServeSessionName)
	proc.mu.Unlock()
	sup.deps.Proc = &sessionGoneProc{inner: proc}
	_ = oldPid

	if err := sup.RestartServer(context.Background()); err != nil {
		t.Fatalf("RestartServer (session-gone/pid-dead): %v", err)
	}
	// 经 waitServeDead 确认死亡后重拉成功（spawn +1）。
	if len(proc.spawned) != initialSpawns+1 {
		t.Fatalf("spawns=%d, want %d (session-gone/pid-dead MUST relaunch)", len(proc.spawned), initialSpawns+1)
	}
	awaitRestarts(t, &restarts, 1)
}

// TestSupervisor_RestartServer_SessionGone_PidAlive_Fails 落实 Issue 1 细分对偶：
// KillSession 返回 SnapshotMissingDegraded（会话已消失）但旧 pid 仍存活（逃逸
// 进程）→ waitServeDead 超时 → fail-closed，MUST NOT 重拉/触发 OnServerRestarted。
// 即会话消失不等于进程死亡，仍须 ProcessIdentity 确认。
func TestSupervisor_RestartServer_SessionGone_PidAlive_Fails(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	var restarts atomic.Int32
	sup, proc, _ := restartTestSupervisor(t, srv.URL, func() { restarts.Add(1) })
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	sup.mu.Lock()
	oldPid := sup.pid
	sup.mu.Unlock()
	// 会话已消失但旧 pid 仍报存活（逃逸进程）。
	sup.deps.ProcessIdentity = func(pid int) (string, bool, error) {
		if pid == oldPid {
			return fmt.Sprintf("opencode serve --port %d --hostname 127.0.0.1", 4096), true, nil
		}
		return "", false, nil
	}
	sup.deps.Proc = &sessionGoneProc{inner: proc}

	err := sup.RestartServer(context.Background())
	if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("session-gone/pid-alive kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(err), err)
	}
	if !errors.Is(err, errRestartKillFailed) {
		t.Fatalf("expect errRestartKillFailed, got %v", err)
	}
	if len(proc.spawned) != initialSpawns {
		t.Fatalf("spawns=%d, want %d (pid-alive MUST NOT relaunch)", len(proc.spawned), initialSpawns)
	}
	awaitRestarts(t, &restarts, 0)
}

// TestSupervisor_RestartServer_NotReadyButPidExists_Kills 落实 Issue 2：
// ready=false 但持久化记录/托管单元仍存有 owned pid → RestartServer MUST 发现
// 并杀掉它（spawn +1），不得跳过 kill 直接复用旧 pid 报成功。
func TestSupervisor_RestartServer_NotReadyButPidExists_Kills(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, recs := restartTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	// 模拟 invalidateServer：清内存 ready+pid，但持久化记录 PID 仍有效（进程仍存活）。
	sup.mu.Lock()
	sup.ready = false
	sup.pid = 0
	sup.mu.Unlock()
	// 确认记录 PID 仍有效（discoverOwnedPidLocked 第二级来源）。
	rec, _, _ := recs.GetProviderRuntime(context.Background(), ProviderID, ProviderInstanceID)
	if !rec.PID.Valid || rec.PID.Int64 <= 0 {
		t.Fatalf("record PID must be valid for discovery, got %+v", rec.PID)
	}

	if err := sup.RestartServer(context.Background()); err != nil {
		t.Fatalf("RestartServer: %v", err)
	}
	// MUST 发现持久化 pid 并杀+重拉（spawn +1），而非跳过 kill 复用旧 pid。
	if len(proc.spawned) != initialSpawns+1 {
		t.Fatalf("spawns=%d, want %d (MUST kill persisted serve and respawn)", len(proc.spawned), initialSpawns+1)
	}
	sup.mu.Lock()
	ready := sup.ready
	port := sup.port
	sup.mu.Unlock()
	if !ready || port != 4096 {
		t.Fatalf("after restart ready=%v port=%d, want ready=true port=4096", ready, port)
	}
}

// TestSupervisor_RestartServer_ConcurrentWaitersShareLeaderFailure 落实 Issue 3：
// leader 重启失败时，waiter MUST 共享 leader 的失败结果，不得独立 ensureServer
// 报成功。注入 kill 失败使 leader 失败；并发 waiter 必须返回同一
// runtime_unavailable，且无额外 spawn（不独立重拉）。
func TestSupervisor_RestartServer_ConcurrentWaitersShareLeaderFailure(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := restartTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)
	// 注入 kill 失败：leader 必失败（runtime_unavailable）。
	sup.deps.Proc = &killFailsProc{inner: proc}

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	ready := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-ready
			errs[idx] = sup.RestartServer(context.Background())
		}(i)
	}
	close(ready)
	wg.Wait()

	// 所有调用 MUST 返回 runtime_unavailable（leader 失败，waiter 共享结果；
	// 不得有 waiter 独立 ensureServer 报成功）。
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller[%d] returned nil error, want shared leader failure", i)
		}
		if appagent.KindOf(err) != appagent.ErrKindRuntimeUnavailable {
			t.Fatalf("caller[%d] kind=%v, want runtime_unavailable (err=%v)", i, appagent.KindOf(err), err)
		}
	}
	// MUST 无额外 spawn（leader 失败在 kill 阶段，未进入 ensureServerLocked；
	// waiter 共享失败不独立重拉）。
	if len(proc.spawned) != initialSpawns {
		t.Fatalf("spawns=%d, want %d (waiters MUST NOT independently relaunch)", len(proc.spawned), initialSpawns)
	}
}

// TestSupervisor_RestartServer_WaiterBoundToFailingLeader_NotOverwrittenByNewLeader
// 落实 Oracle Issue 3 收尾（真实竞态）：leader A 失败时，waiter B 在 A 运行期间
// 捕获 A 的 per-flight 指针；A 完成后新 leader C 启动并覆盖 s.restartFlight。B
// MUST 返回 A 的失败，不受 C 覆盖影响。关键：C 在 B 返回结果前启动（不隐藏竞态）。
//
// 新设计下 B 在 A 运行期间（A 释放 restartMu 跑 restartLeader）即捕获 A 的 flight
// 指针；C 启动时若 A 仍 in-flight 则 join A（同失败），若 A 已完成则成新 leader 覆
// 盖 s.restartFlight——但 B 已持 A 的指针，覆盖不影响 B。
func TestSupervisor_RestartServer_WaiterBoundToFailingLeader_NotOverwrittenByNewLeader(t *testing.T) {
	srv := fixtureServer(t, route{path: "/api/session", status: 200, body: string(readFixture(t, "session_list_page2.json"))})
	sup, proc, _ := restartTestSupervisor(t, srv.URL, nil)
	if _, _, err := sup.ensureServer(context.Background()); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	initialSpawns := len(proc.spawned)

	// 可控 kill proc：第一次 KillSession 阻塞直到 releaseA 关闭（让 waiter B 在 A
	// 运行期间捕获 A 的 flight 指针），释放后返回 kill-failed（A 失败）；第二次起
	// 切换为正常成功 kill（C 成功）。
	releaseA := make(chan struct{})
	var killCalls atomic.Int32
	controlledProc := &controlledKillProc{
		inner: proc,
		onKill: func() (process.KillResult, error) {
			n := killCalls.Add(1)
			if n == 1 {
				// leader A：阻塞让 B 捕获 flight 指针，然后失败。
				<-releaseA
				return process.KillResult{SessionKilled: false, Disposition: process.DispositionKillFailed}, nil
			}
			// leader C 及之后：正常 kill（成功）。
			return proc.KillSession(ServeSessionName)
		},
	}
	sup.deps.Proc = controlledProc

	// A：leader，将失败（kill-failed）。A 注册 flight 后释放 restartMu 跑
	// restartLeader，在 onKill 阻塞。
	aDone := make(chan error, 1)
	go func() {
		aDone <- sup.RestartServer(context.Background())
	}()

	// 等 A 进入 kill（已注册 flight 并释放 restartMu，在 onKill 阻塞中）。
	deadline := time.Now().Add(2 * time.Second)
	for killCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if killCalls.Load() == 0 {
		t.Fatalf("leader A 未进入 kill")
	}

	// B：A 仍 in-flight（done 未关闭），B 在 restartMu 下观察到 A 的 flight 并捕获
	// 指针，释放 restartMu 后等 A 的 done。
	bDone := make(chan error, 1)
	go func() {
		bDone <- sup.RestartServer(context.Background())
	}()
	// 给 B 时间捕获 A 的 flight 指针（B 此时阻塞在 <-flight.done）。
	time.Sleep(20 * time.Millisecond)

	// 释放 A：A 的 kill 返回失败 → restartLeader 返回 runtime_unavailable →
	// A 写 flight.err + close flight.done。B 从 done 收到信号返回 A 的 err。
	close(releaseA)
	aErr := <-aDone
	if appagent.KindOf(aErr) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("leader A kind=%v, want runtime_unavailable (err=%v)", appagent.KindOf(aErr), aErr)
	}

	// C：在等 B 结果前启动。此时 A 的 flight.done 已关闭，C 观察到当前 flight 已完
	// 成 → 成新 leader，kill 正常（第二次起成功）→ 覆盖 s.restartFlight 为新对象 →
	// ensureServerLocked 重拉成功（spawn +1）。B 已持 A 的旧 flight 指针，C 覆盖不
	// 影响B。
	cDone := make(chan error, 1)
	go func() {
		cDone <- sup.RestartServer(context.Background())
	}()

	// B MUST 返回 A 的失败（非 C 的成功 nil）——这是本测试的核心断言。
	bErr := <-bDone
	if appagent.KindOf(bErr) != appagent.ErrKindRuntimeUnavailable {
		t.Fatalf("waiter B kind=%v, want runtime_unavailable (A's failure, err=%v)", appagent.KindOf(bErr), bErr)
	}
	if !errors.Is(bErr, errRestartKillFailed) {
		t.Fatalf("waiter B must share A's errRestartKillFailed, got %v", bErr)
	}

	cErr := <-cDone
	if cErr != nil {
		t.Fatalf("leader C (success) err=%v, want nil", cErr)
	}
	// 仅 leader C 重拉（spawn +1）；A 失败未重拉，B join A 未独立重拉。
	if len(proc.spawned) != initialSpawns+1 {
		t.Fatalf("spawns=%d, want %d (only leader C relaunches)", len(proc.spawned), initialSpawns+1)
	}
}

// controlledKillProc 包装 inner proc，KillSession 委托 onKill 回调（用于确定性编排
// leader A 失败 / leader C 成功的交错测试）。
type controlledKillProc struct {
	SupervisorProcSeam
	inner  *fakeProcSeam
	onKill func() (process.KillResult, error)
}

func (c *controlledKillProc) NewSession(spec process.SessionSpec) error { return c.inner.NewSession(spec) }
func (c *controlledKillProc) HasSession(name string) (bool, error)      { return c.inner.HasSession(name) }
func (c *controlledKillProc) SessionPID(name string) (int, bool, error) { return c.inner.SessionPID(name) }
func (c *controlledKillProc) KillSession(name string) (process.KillResult, error) {
	return c.onKill()
}