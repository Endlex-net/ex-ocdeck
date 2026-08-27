package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWatchdog_KillsServerOnParentDeath 验证 watchdog FSM：fork 一个短命父进程
// 启动 ocdeck-server watchdog 子进程 + 创建会话，父进程退出后 watchdog 应
// kill-server 清理全部会话（design.md §10/§15 假设 c）。
//
// 实现方式：编译 ocdeck-server 二进制 → fork 短命父进程跑 watchdog + new-session →
// 父退出后 watchdog 轮询 ppid 发现父亡 → kill-server → 会话消失。
func TestWatchdog_KillsServerOnParentDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watchdog fork integration test in -short mode")
	}
	if os.Getenv("OCDECK_WATCHDOG_FORK_TEST") == "1" {
		// 子进程模式：作为父进程跑 watchdog + 创建会话然后退出，
		// 让真实 watchdog 子进程接管清理。
		runWatchdogForkChild(t)
		return
	}

	// 编译 ocdeck-server 二进制到临时目录。
	// exec.Command 继承测试进程 cwd（internal/infrastructure/process），需切到模块根目录才能解析 ./cmd/ocdeck-server。
	modRoot, err := findModuleRoot()
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	binPath := filepath.Join(t.TempDir(), "ocdeck-server-test")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/ocdeck-server")
	buildCmd.Dir = modRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build ocdeck-server: %v\n%s", err, out)
	}

	// 隔离 socket/tmpdir。tmpdir MUST 短（macOS tmux socket 路径上限 ~104 字符）。
	socket := "wd-" + pidSuffix()
	tmpdir := filepath.Join("/tmp", "ocdeck-wd-"+pidSuffix())
	if err := os.MkdirAll(tmpdir, 0o700); err != nil {
		t.Fatalf("create short tmpdir: %v", err)
	}
	defer os.RemoveAll(tmpdir)

	// 启动子进程（父进程角色）：创建会话 + spawn watchdog + 立即退出。
	// 子进程退出后 watchdog 应 kill-server。
	cmd := exec.Command(os.Args[0], "-test.run=TestWatchdog_KillsServerOnParentDeath", "-test.v")
	cmd.Env = append(os.Environ(),
		"OCDECK_WATCHDOG_FORK_TEST=1",
		"OCDECK_TEST_BIN="+binPath,
		"OCDECK_TEST_SOCKET="+socket,
		"OCDECK_TEST_TMPDIR="+tmpdir,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("fork child output: %s", out)
	if err != nil {
		t.Fatalf("fork child: %v", err)
	}

	// 等待 watchdog 轮询 ppid + kill-server 完成（父进程退出后 watchdog 1s 轮询 + 清理）。
	time.Sleep(4 * time.Second)

	// 验证：socket 下无 ocdeck-* 会话。
	// 直接构造 Manager 探测（注意：watchdog 已 kill-server，但残留 socket 文件可能仍在）。
	m := &Manager{
		socketName: socket,
		tmpdir:     tmpdir,
		baseEnv:    []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmpdir, "TMUX_TMPDIR=" + tmpdir, "HOME=" + os.Getenv("HOME"), "TERM=xterm"},
		psProvider: darwinPSProvider{},
	}
	sessions, listErr := m.ListSessions()
	if listErr != nil {
		t.Fatalf("ListSessions after watchdog cleanup: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Errorf("watchdog should have killed all sessions; got %v", sessions)
	}
	// 确保清理 socket（kill-server 后 socket 文件可能残留，但 tmux server 已停）。
	_, _, _ = m.execTmux(context.Background(), "kill-server")
}

// runWatchdogForkChild 是 fork 子进程模式：spawn watchdog + 创建会话 + 退出。
func runWatchdogForkChild(t *testing.T) {
	binPath := os.Getenv("OCDECK_TEST_BIN")
	socket := os.Getenv("OCDECK_TEST_SOCKET")
	tmpdir := os.Getenv("OCDECK_TEST_TMPDIR")

	// spawn watchdog（用编译好的二进制自调）。
	ppid := os.Getpid()
	wdCmd := exec.Command(binPath, "watchdog", ppidStr(ppid), socket, tmpdir)
	wdCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmpdir, "TMUX_TMPDIR=" + tmpdir, "HOME=" + os.Getenv("HOME"), "TERM=xterm"}
	wdCmd.Stdout = os.Stdout
	wdCmd.Stderr = os.Stderr
	if err := wdCmd.Start(); err != nil {
		t.Fatalf("spawn watchdog: %v", err)
	}
	// 等 watchdog 就绪。
	time.Sleep(500 * time.Millisecond)

	// 创建一个会话（确保 watchdog 存活期间会话存在，父死后应被清）。
	m := &Manager{
		socketName: socket,
		tmpdir:     tmpdir,
		baseEnv:    []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmpdir, "TMUX_TMPDIR=" + tmpdir, "HOME=" + os.Getenv("HOME"), "TERM=xterm"},
		psProvider: darwinPSProvider{},
	}
	if err := m.NewSession(SessionSpec{
		Name:    "ocdeck-wdtask-runtime",
		Dir:     tmpdir,
		Env:     map[string]string{"K": "v"},
		CmdArgv: []string{"sleep", "600"},
	}); err != nil {
		t.Fatalf("NewSession in fork child: %v", err)
	}
	// 确认会话存在。
	ok, _ := m.HasSession("ocdeck-wdtask-runtime")
	if !ok {
		t.Fatalf("session not created in fork child")
	}
	t.Logf("fork child: session created, exiting; watchdog should kill-server")
	// 子进程退出 → watchdog 轮询 ppid 发现父亡 → kill 路径。
	// （不主动 kill-server，依赖 watchdog）
}

func pidSuffix() string {
	return strconv.Itoa(os.Getpid())
}

func ppidStr(pid int) string { return strconv.Itoa(pid) }

// findModuleRoot 返回当前 go module 根目录（go env GOMOD 去掉尾部 go.mod）。
// 测试进程 cwd 在 internal/infrastructure/process 下，直接用 ./cmd/ocdeck-server 无法解析。
func findModuleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		return "", errors.New("not in a go module")
	}
	return filepath.Dir(gomod), nil
}

// TestWatchdog_SpawnRejectsEmptyBinary 验证 Spawn 在 binaryPath 为空时拒绝启动
// 且 state 不变 running（design.md §10：spawn 失败 MUST 拒绝启动）。
func TestWatchdog_SpawnRejectsEmptyBinary(t *testing.T) {
	w := NewWatchdogManager("", "", "ocdeck", "/tmp", nil)
	err := w.Spawn()
	if err == nil {
		t.Fatal("Spawn with empty binary should fail")
	}
	if w.StateString() == WatchdogStateRunning {
		t.Error("state should not be running after spawn failure")
	}
}

// TestWatchdog_StopIdempotent verifies Stop is idempotent on an unstarted manager.
func TestWatchdog_StopIdempotent(t *testing.T) {
	w := NewWatchdogManager("/nonexistent", "", "ocdeck", "/tmp", nil)
	if err := w.Stop(); err != nil {
		t.Errorf("Stop on unstarted watchdog should be no-op, got %v", err)
	}
	if w.StateString() != WatchdogStateOff {
		t.Errorf("state after Stop on unstarted = %q, want off", w.StateString())
	}
}