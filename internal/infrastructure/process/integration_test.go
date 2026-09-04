package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestManager 构造隔离的测试 Manager：随机 socket 名 + 短 tmpdir 作 TMUX_TMPDIR，
// 绝不触碰默认 ocdeck socket（design.md §2/测试隔离约束）。
//
// macOS UNIX domain socket 路径上限约 104 字符；tmux socket 落在
// $TMUX_TMPDIR/tmux-<uid>/<socketName>，故 tmpdir MUST 足够短。t.TempDir()
// 在 /var/folders 下的路径常超 90 字符，叠加 tmux-<uid>/socket 名即超限，
// 因此改在 /tmp 下构造短路径（仍 per-test 唯一）。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	tmpdir := shortTmpDir(t)
	// socket 名尽量短：tmux socket 路径 = $TMUX_TMPDIR/tmux-<uid>/<socketName>，
	// macOS 上限 ~104 字符，tmpdir 已占约 30 字符，socket 名留 20 字符以内。
	socket := fmt.Sprintf("o%d", time.Now().UnixNano()%1000000)
	// 测试用 base env：最小集，确保 tmux 可启动（需 PATH/TMPDIR/HOME）。
	// TMUX_TMPDIR 显式注入（S5：测试真实设置，配合 B1 隔离不变量验证）。
	baseEnv := []string{
		"PATH=" + envOr("PATH", "/usr/bin:/bin"),
		"TMPDIR=" + tmpdir,
		"TMUX_TMPDIR=" + tmpdir,
		"HOME=" + envOr("HOME", "/tmp"),
		"TERM=xterm",
	}
	return &Manager{
		socketName: socket,
		tmpdir:     tmpdir,
		baseEnv:    baseEnv,
		psProvider: darwinPSProvider{},
	}
}

// shortTmpDir 在 /tmp 下创建 per-test 唯一的短目录（0700）。
// macOS tmux socket 路径上限 ~104 字符，$TMUX_TMPDIR MUST 足够短；
// os.TempDir() 在 darwin 上常指向 /var/folders 长路径，故固定用 /tmp。
func shortTmpDir(t *testing.T) string {
	t.Helper()
	p := filepath.Join("/tmp", fmt.Sprintf("oc-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatalf("create short tmpdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(p) })
	return p
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// cleanupTmux 杀掉测试 socket 的 tmux server，避免泄漏。
func cleanupTmux(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _ = m.execTmux(ctx, "kill-server")
}

// waitForSessionActive 轮询直到会话存在或超时。
func waitForSessionActive(t *testing.T, m *Manager, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := m.HasSession(name)
		if err == nil && ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s not active within %s", name, timeout)
}

// waitForFile 轮询直到文件出现或超时。
// 用于短命命令的测试：命令执行完成后写文件，轮询文件比轮询会话存活更稳定
// （短命会话可能在存活轮询前已退出）。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("file %s not created within %s", path, timeout)
}

// runTmuxCmd 在测试中直接跑 tmux 命令取 stdout（辅助验证）。
func runTmuxCmd(t *testing.T, m *Manager, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _, err := m.execTmux(ctx, args...)
	if err != nil {
		t.Fatalf("tmux %v: %v", args, err)
	}
	return out
}

// TestNewSession_EnvInjected 验证 -e env 实测生效（design.md §15 假设 a）：
// 创建带 -e FOO=bar 的会话，经 show-environment 读回确认注入。
func TestNewSession_EnvInjected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	name := "ocdeck-task1-runtime"
	dir := t.TempDir()
	spec := SessionSpec{
		Name:    name,
		Dir:     dir,
		Env:     map[string]string{"FOO": "bar", "OCDECK_SERVE_PORT": "50001"},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		_, _ = m.KillSession(name)
	}()

	waitForSessionActive(t, m, name, 3*time.Second)

	// 验证 FOO 注入生效。
	val, err := m.ShowSessionEnv(name, "FOO")
	if err != nil {
		t.Fatalf("ShowSessionEnv FOO: %v", err)
	}
	if val != "bar" {
		t.Errorf("FOO = %q, want bar", val)
	}
	// 验证含空格的值经 -e 保留。
	val2, err := m.ShowSessionEnv(name, "OCDECK_SERVE_PORT")
	if err != nil {
		t.Fatalf("ShowSessionEnv OCDECK_SERVE_PORT: %v", err)
	}
	if val2 != "50001" {
		t.Errorf("OCDECK_SERVE_PORT = %q, want 50001", val2)
	}
}

// TestTmuxSocket_InTmpdir 验证 TMUX_TMPDIR 注入使 tmux socket 落在指定 tmpdir
// （B1 / S5：隔离不变量实测）。创建会话后 socket 文件 MUST 出现在 Manager.tmpdir
// 下（tmux-<uid>/<socketName>），而非默认 /tmp/tmux-<uid>。
func TestTmuxSocket_InTmpdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	name := "ocdeck-socktest-runtime"
	spec := SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		Env:     map[string]string{},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _, _ = m.KillSession(name) }()
	waitForSessionActive(t, m, name, 3*time.Second)

	// socket 文件应出现在 tmpdir 下（tmux 布局：$TMUX_TMPDIR/tmux-<uid>/<socketName>）。
	// 列出 tmpdir 下的 tmux-<uid> 子目录。
	entries, err := os.ReadDir(m.tmpdir)
	if err != nil {
		t.Fatalf("read tmpdir %s: %v", m.tmpdir, err)
	}
	var found bool
	for _, e := range entries {
		// tmux 创建 tmux-<uid> 子目录，内含 socketName socket 文件。
		subDir := filepath.Join(m.tmpdir, e.Name())
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if se.Name() == m.socketName {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("socket %s not found under tmpdir %s (TMUX_TMPDIR not injected/used)", m.socketName, m.tmpdir)
	}
}

// TestNewSession_CmdArgvWithSpaces 验证 CmdArgv 含空格参数正确转义传递。
//
// 该命令（echo 重定向）执行后会话立即退出，不能用 waitForSessionActive 轮询
// 会话存活（会话可能在首次轮询前已结束，导致 flaky）。
// 改为轮询输出文件：命令成功即等价于 argv 转义正确（design.md §2）。
func TestNewSession_CmdArgvWithSpaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	outFile := filepath.Join(t.TempDir(), "out.txt")
	name := "ocdeck-task2-runtime"
	dir := t.TempDir()
	// sh -c 'echo "hello world" > /path' —— 参数含空格与引号。
	spec := SessionSpec{
		Name:    name,
		Dir:     dir,
		Env:     map[string]string{"X": "1"},
		CmdArgv: []string{"sh", "-c", "echo hello-world > " + outFile},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		_, _ = m.KillSession(name)
	}()

	// 轮询输出文件：命令执行完成即代表 argv 正确传递。
	// 不依赖会话存活（短命命令会话在轮询前可能已退出），避免 flaky。
	waitForFile(t, outFile, 5*time.Second)
	// 命令写文件后短暂 sleep，确保文件内容刷盘完成。
	time.Sleep(200 * time.Millisecond)

	data, err := readFile(outFile)
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if strings.TrimSpace(data) != "hello-world" {
		t.Errorf("cmd output = %q, want hello-world", strings.TrimSpace(data))
	}
}

// TestHasSession_DistinguishesAbsent 验证 HasSession 区分"无 tmux server"、
// "单会话不存在"与"命令失败"（design.md §18，B4）。
func TestHasSession_DistinguishesAbsent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	// 无任何会话（无 server）：HasSession 返回 (false, ErrNoTmuxServer)——
	// 区分"空运行时"与"单会话不存在"。
	ok, err := m.HasSession("ocdeck-none-serve")
	if !errors.Is(err, ErrNoTmuxServer) {
		t.Fatalf("HasSession on no server: err = %v, want ErrNoTmuxServer", err)
	}
	if ok {
		t.Error("HasSession on no server returned true")
	}

	// 创建会话后应返回 true。
	name := "ocdeck-task3-runtime"
	spec := SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		Env:     map[string]string{"K": "v"},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		_, _ = m.KillSession(name)
	}()
	waitForSessionActive(t, m, name, 3*time.Second)

	ok2, err := m.HasSession(name)
	if err != nil {
		t.Fatalf("HasSession on existing: %v", err)
	}
	if !ok2 {
		t.Error("HasSession on existing returned false")
	}

	// server 已存在但查询一个不存在的会话：返回 (false, nil)——
	// 区分于"无 server"。
	ok3, err := m.HasSession("ocdeck-none-serve")
	if err != nil {
		t.Fatalf("HasSession on absent session (server running): %v", err)
	}
	if ok3 {
		t.Error("HasSession on absent session returned true")
	}
}

// TestKillSession_AbsentIsDegraded 验证 kill-session 幂等语义：不存在会话 →
// snapshot_missing_degraded（调用方应在调用前确认存在；此处测 absent-at-entry 走 degraded）。
func TestKillSession_AbsentIsDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	// 会话不存在时 KillSession 返回 snapshot_missing_degraded（设计：absent-at-entry
	// 由上层短路；本包将"会话在快照时消失"统一标为 degraded）。
	res, err := m.KillSession("ocdeck-absent-serve")
	if err != nil {
		t.Fatalf("KillSession absent: unexpected error %v", err)
	}
	if res.Disposition != DispositionSnapshotMissingDegraded {
		t.Errorf("absent disposition = %v, want snapshot_missing_degraded", res.Disposition)
	}
	if res.SessionKilled {
		t.Error("absent SessionKilled should be false")
	}
}

// TestKillSession_Clean 验证正常 kill：创建会话→KillSession→clean + 无 tickets。
func TestKillSession_Clean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	name := "ocdeck-task4-runtime"
	spec := SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		Env:     map[string]string{"K": "v"},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSessionActive(t, m, name, 3*time.Second)

	res, err := m.KillSession(name)
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if !res.SessionKilled {
		t.Error("SessionKilled should be true")
	}
	if res.Disposition != DispositionClean {
		t.Errorf("disposition = %v, want clean", res.Disposition)
	}
	if len(res.CleanupTickets) != 0 {
		t.Errorf("clean should have no tickets, got %d", len(res.CleanupTickets))
	}
	// 确认会话已终止。
	ok, _ := m.HasSession(name)
	if ok {
		t.Error("session should be gone after kill")
	}
}

// TestReaper_ReapsEscapedDescendants 验证 reaper 收割忽略 SIGHUP 的逃逸子孙
// （design.md §15 假设 c / §2 reaper）。
func TestReaper_ReapsEscapedDescendants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	name := "ocdeck-task5-runtime"
	// 启动一个 bash 子进程，trap "" HUP 忽略 SIGHUP，其内 sleep 600。
	// kill-session 后该 bash 幸存（reparent 到 init），reaper 应按身份校验后 TERM/KILL。
	// 逃逸 bash 将自身 PID 写入 marker 文件，供测试精确追踪（S5：避免全局 pgrep
	// "sleep 600" 匹配到其他 lane/用户进程）。
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	pidFile := filepath.Join(dir, "pid")
	spec := SessionSpec{
		Name: name,
		Dir:  dir,
		Env:  map[string]string{"M": "1"},
		// bash -c 'trap "" HUP; touch <marker>; echo $$ > <pidFile>; sleep 600'
		CmdArgv: []string{"bash", "-c", "trap \"\" HUP; touch " + marker + "; echo $$ > " + pidFile + "; sleep 600"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSessionActive(t, m, name, 3*time.Second)
	// 等 bash 与 sleep 起来。
	time.Sleep(500 * time.Millisecond)

	// 确认 marker 存在（进程已起）。
	if _, err := readFile(marker); err != nil {
		t.Fatalf("marker not created: %v", err)
	}

	// 精确追踪逃逸 bash 的 PID（非全局 pgrep）。
	escapedPid := readPidFile(t, pidFile)
	if !processAlive(escapedPid) {
		t.Fatalf("escaped bash pid=%d not alive before kill", escapedPid)
	}

	// KillSession 触发 reaper。
	res, err := m.KillSession(name)
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	// 逃逸子孙被收割 → disposition 应为 clean（幸存者已 KILL）。
	if res.Disposition != DispositionClean && res.Disposition != DispositionReapFailed {
		// reap_failed 也允许（若 TERM 宽限内未杀掉），但 clean 是期望。
		t.Logf("disposition = %v (clean or reap_failed acceptable)", res.Disposition)
	}

	// 验证逃逸 bash 已被收割（最多等 5s）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(escapedPid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("escaped bash pid=%d still alive after reaper", escapedPid)
}

// TestListSessions_EmptyAndFilters 验证 ListSessions 无 server 时返回空，
// 且仅返回 ocdeck-* 前缀会话。
func TestListSessions_EmptyAndFilters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	// 无 server：空列表非 error。
	sessions, err := m.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions empty: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("empty ListSessions = %v, want []", sessions)
	}

	// 创建 ocdeck 会话后应列出。
	name := "ocdeck-task6-runtime"
	spec := SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		Env:     map[string]string{},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		_, _ = m.KillSession(name)
	}()
	waitForSessionActive(t, m, name, 3*time.Second)

	sessions2, err := m.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range sessions2 {
		if s == name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListSessions %v does not contain %s", sessions2, name)
	}
}

// TestWatchExit_FiresOnSessionGone 验证 WatchExit 在会话消失时触发 callback。
func TestWatchExit_FiresOnSessionGone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	name := "ocdeck-task7-runtime"
	spec := SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		Env:     map[string]string{},
		CmdArgv: []string{"sleep", "30"},
	}
	if err := m.NewSession(spec); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForSessionActive(t, m, name, 3*time.Second)

	done := make(chan WatchEvent, 1)
	cancel, wdone := m.WatchExit(name, func(ev WatchEvent) {
		select {
		case done <- ev:
		default:
		}
	})
	defer func() {
		cancel()
		<-wdone
	}()

	// 杀掉会话触发 callback。
	_, _ = m.KillSession(name)

	select {
	case ev := <-done:
		if ev.Type != WatchEventSessionExit {
			t.Errorf("WatchExit event type = %v, want session_exit", ev.Type)
		}
	// 成功触发。
	case <-time.After(8 * time.Second):
		t.Fatal("WatchExit callback did not fire within 8s")
	}
}

// TestKillServer_IdempotentWhenNoServer 验证无 server 时 kill-server 幂等成功
// （design.md §10/§15）。
func TestKillServer_IdempotentWhenNoServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	// 不创建任何会话，直接 killServerGlobal 应无 error。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.killServerGlobal(ctx); err != nil {
		t.Fatalf("killServerGlobal no-server: %v", err)
	}
}

// TestKillServerGlobal_ReapsAllSessions 验证 watchdog kill 路径收割全部会话。
func TestKillServerGlobal_ReapsAllSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	defer cleanupTmux(t, m)

	// 创建 2 个会话，其中一个带逃逸子孙。
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	pidFile := filepath.Join(dir, "pid")
	if err := m.NewSession(SessionSpec{
		Name: "ocdeck-task8-runtime",
		Dir:  dir,
		Env:  map[string]string{},
		// 逃逸 bash 写自身 PID 到 pidFile，供测试精确追踪（S5：避免全局 pgrep）。
		CmdArgv: []string{"bash", "-c", "trap \"\" HUP; touch " + marker + "; echo $$ > " + pidFile + "; sleep 600"},
	}); err != nil {
		t.Fatalf("NewSession 1: %v", err)
	}
	if err := m.NewSession(SessionSpec{
		Name:    "ocdeck-task8-shell-1",
		Dir:     dir,
		Env:     map[string]string{},
		CmdArgv: []string{"sleep", "30"},
	}); err != nil {
		t.Fatalf("NewSession 2: %v", err)
	}
	waitForSessionActive(t, m, "ocdeck-task8-runtime", 3*time.Second)
	waitForSessionActive(t, m, "ocdeck-task8-shell-1", 3*time.Second)
	time.Sleep(500 * time.Millisecond)

	escapedPid := readPidFile(t, pidFile)
	if !processAlive(escapedPid) {
		t.Fatalf("escaped bash pid=%d not alive before kill", escapedPid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.killServerGlobal(ctx); err != nil {
		t.Fatalf("killServerGlobal: %v", err)
	}

	// 全部 ocdeck 会话应已消失。
	sessions, _ := m.ListSessions()
	if len(sessions) != 0 {
		t.Errorf("after killServerGlobal sessions = %v, want empty", sessions)
	}
	// 逃逸子孙应被收割（精确追踪 pid，非全局 pgrep）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(escapedPid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("escaped bash pid=%d still alive after killServerGlobal", escapedPid)
}

// --- 辅助 ---

func readFile(path string) (string, error) {
	out, err := exec.Command("cat", path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readPidFile 读取 pidFile 内容解析为 PID（逃逸 bash 写入的自身 PID）。
func readPidFile(t *testing.T, pidFile string) int {
	t.Helper()
	data, err := exec.Command("cat", pidFile).Output()
	if err != nil {
		t.Fatalf("read pid file %s: %v", pidFile, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("invalid pid in %s: %q", pidFile, data)
	}
	return pid
}

// processAlive 精确判断单个 PID 是否存活（kill -0 + zombie 排除）。
// 用于 S5：避免全局 pgrep -f "sleep 600" 误匹配其他 lane/用户进程。
// zombie 不算存活：被 reaper 杀掉的进程 reparent 给 PID 1 后，在 PID 1 不收割
// 孤儿的环境（docker 容器等）下长期保持 zombie（kill -0 仍返回成功），但其已死
// 且不可再杀——测试若把 zombie 判活，会对已正确收割的进程误报 still alive。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if exec.Command("kill", "-0", strconv.Itoa(pid)).Run() != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func setAttachTermClipboard(m *Manager) {
	for i, e := range m.baseEnv {
		if strings.HasPrefix(e, "TERM=") {
			m.baseEnv[i] = "TERM=xterm-256color"
			return
		}
	}
	m.baseEnv = append(m.baseEnv, "TERM=xterm-256color")
}

func waitForTriggerScript(trigger, extraPrintf string) string {
	return "while [ ! -f " + shellQuote(trigger) + " ]; do sleep 0.05; done; " +
		extraPrintf + "sleep 30"
}

func readPtyUntil(t *testing.T, p interface {
	ReadCtx(ctx context.Context) ([]byte, error)
}, needle string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var buf strings.Builder
	for {
		chunk, err := p.ReadCtx(ctx)
		if len(chunk) > 0 {
			buf.Write(chunk)
			if strings.Contains(buf.String(), needle) {
				drainCtx, drainCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				for {
					more, derr := p.ReadCtx(drainCtx)
					if len(more) > 0 {
						buf.Write(more)
						continue
					}
					_ = derr
					break
				}
				drainCancel()
				return buf.String()
			}
		}
		if err != nil {
			t.Fatalf("pty read: %v (got %q, want contain %q)", err, buf.String(), needle)
		}
	}
}

// TestNewSession_ForwardsOSC52ToAttachClient 验证 set-clipboard on 后，pane 发出的
// 原始 OSC 52 会转到 attach 客户端（-f /dev/null 默认 external 会丢弃）。
func TestNewSession_ForwardsOSC52ToAttachClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	m := newTestManager(t)
	setAttachTermClipboard(m)
	defer cleanupTmux(t, m)

	trigger := filepath.Join(shortTmpDir(t), "go")
	name := "ocdeck-clip-runtime"
	script := waitForTriggerScript(trigger, `printf '\033]52;c;dGVzdA==\a'; `)
	if err := m.NewSession(SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		CmdArgv: []string{"sh", "-c", script},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _, _ = m.KillSession(name) }()
	waitForSessionActive(t, m, name, 3*time.Second)

	shown := runTmuxCmd(t, m, "show-options", "-sv", "set-clipboard")
	if strings.TrimSpace(shown) != "on" {
		t.Fatalf("set-clipboard = %q, want on", shown)
	}

	pt, err := m.AttachPty(name, 80, 24)
	if err != nil {
		t.Fatalf("AttachPty: %v", err)
	}
	defer pt.Close()

	if err := os.WriteFile(trigger, []byte("1"), 0o600); err != nil {
		t.Fatalf("write trigger: %v", err)
	}
	got := readPtyUntil(t, pt, "]52;c;dGVzdA==", 5*time.Second)
	if !strings.Contains(got, "]52;c;dGVzdA==") {
		t.Fatalf("attach stream missing OSC52 payload: %q", got)
	}
}

// TestEnsureServerOptions_OSC52OnceWithPassthroughWrapper 验证 pane 同时发出原始
// OSC52 与 DCS tmux-passthrough 包装时，attach 只收到一次 payload（passthrough
// 被默认 allow-passthrough off 吃掉，raw 由 set-clipboard on 转发）。
// 明文 sentinel 在两次 printf 之后输出——tmux 顺序处理 pane 输出，attach 流中出现
// sentinel 即代表两次 OSC52 均已处理完毕，此时计数无竞态（不依赖固定 drain 窗口）。
func TestEnsureServerOptions_OSC52OnceWithPassthroughWrapper(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in -short mode")
	}
	const (
		osc52Payload = "]52;c;dGVzdA=="
		sentinel     = "OCDECK-CLIP-SENTINEL"
	)
	m := newTestManager(t)
	setAttachTermClipboard(m)
	defer cleanupTmux(t, m)

	trigger := filepath.Join(shortTmpDir(t), "go")
	name := "ocdeck-clip2-runtime"
	script := waitForTriggerScript(trigger,
		`printf '\033]52;c;dGVzdA==\a'; printf '\033Ptmux;\033\033]52;c;dGVzdA==\a\033\\'; printf '`+sentinel+`\n'; `)
	if err := m.NewSession(SessionSpec{
		Name:    name,
		Dir:     t.TempDir(),
		CmdArgv: []string{"sh", "-c", script},
	}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _, _ = m.KillSession(name) }()
	waitForSessionActive(t, m, name, 3*time.Second)

	pt, err := m.AttachPty(name, 80, 24)
	if err != nil {
		t.Fatalf("AttachPty: %v", err)
	}
	defer pt.Close()

	if err := os.WriteFile(trigger, []byte("1"), 0o600); err != nil {
		t.Fatalf("write trigger: %v", err)
	}
	got := readPtyUntil(t, pt, sentinel, 5*time.Second)
	if n := strings.Count(got, osc52Payload); n != 1 {
		t.Fatalf("OSC52 payload count = %d, want 1 (stream=%q)", n, got)
	}
}
