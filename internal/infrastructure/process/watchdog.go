// Package process watchdog FSM（design.md §10，仅 kill_immediate）。
//
// spawn 时机：服务端启动时、任何 tmux 会话创建之前 spawn 单个
// `ocdeck-server watchdog` 子进程；spawn 失败拒绝启动。
// 运行：轮询自身 ppid（os.Getppid() 变化判断，避免 kill(0) 的 PID 复用窗口），
// 父进程消失 → 进入 kill 路径。
// kill 路径（内置全局 reaper）：对全部 ocdeck-* 会话 list-panes 收集 pane 子孙快照 →
// tmux -L ocdeck kill-server → 对快照存活者按身份校验 TERM→宽限→KILL → 自退。
// 无 tmux server 时 kill-server 退出非零视为幂等成功；其他 kill-server 错误与
// 未收割 survivors MUST 反映在返回/日志，不得无条件成功。
package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Watchdog 子命令名（由 cmd/ocdeck-server watchdog 入口识别）。
const WatchdogSubcommand = "watchdog"

// watchdogState 暴露给 /server/status 的运行态（design.md §10/§21）。
type watchdogState string

const (
	watchdogOff      watchdogState = "off"
	watchdogRunning  watchdogState = "running"
	watchdogDegraded watchdogState = "degraded"
)

// WatchdogStateOff/Running/Degraded 是 watchdogState 的导出常量，供 api 包接线
// /server/status 的 watchdogState 字段（design.md §21）。
const (
	WatchdogStateOff      = string(watchdogOff)
	WatchdogStateRunning = string(watchdogRunning)
	WatchdogStateDegraded = string(watchdogDegraded)
)

// watchdogReadyTimeout 是 Spawn 等待子进程 READY 握手的超时（design.md §10 ack 握手）。
const watchdogReadyTimeout = 5 * time.Second

// watchdogMaxRestarts 是 watchdog 异常退出后的最大重启次数（design.md §10：
// 指数退避，上限 3 次；连续失败 → degraded）。
const watchdogMaxRestarts = 3

// watchdogBaseBackoff 是 watchdog 重启指数退避的基准间隔。
const watchdogBaseBackoff = 500 * time.Millisecond

// WatchdogManager 管理 watchdog 子进程生命周期。
//
// 字段：
//   - binaryPath ocdeck-server 可执行文件路径（自调子进程）。
//   - dataDir 数据目录（watchdog 需 TMUX_TMPDIR/socket 参数）。
//   - sub 进程句柄；state 当前运行态；mu 保护二者。
//   - waitDone 在 waitOwner goroutine 退出时关闭，供 Stop 等待 Wait 返回。
//   - stopMu 保护 Stop/重启串行化，避免多个 goroutine 同时操作 sub。
type WatchdogManager struct {
	binaryPath string
	dataDir    string
	socketName string
	tmpdir     string
	baseEnv    []string

	mu       sync.Mutex
	sub      *exec.Cmd
	state    watchdogState
	waitDone chan struct{} // 单次 Wait owner 退出信号

	stopMu sync.Mutex // 串行化 Stop / 重启，消除 Wait 并发竞争

	// restartCount 已重启次数（指数退避，上限 watchdogMaxRestarts→degraded）。
	restartCount int
	// stopCh 在 Stop 时关闭，通知 supervise 停止重启循环。
	stopCh chan struct{}
	// cancelFunc 是当前 sub 的 ctx 取消函数，Stop 时调用触发子进程退出。
	cancelFunc context.CancelFunc
}

// NewWatchdogManager 构造 watchdog 管理器。binaryPath 为 ocdeck-server 可执行路径，
// 通常取 os.Executable()。dataDir/tmpdir/socketName/baseEnv 与 Manager 一致，
// 用于 watchdog 子进程执行 kill-server 的环境。
func NewWatchdogManager(binaryPath, dataDir, socketName, tmpdir string, baseEnv []string) *WatchdogManager {
	return &WatchdogManager{
		binaryPath: binaryPath,
		dataDir:    dataDir,
		socketName: socketName,
		tmpdir:     tmpdir,
		baseEnv:    baseEnv,
		state:      watchdogOff,
	}
}

// State 返回当前 watchdog 运行态（off/running/degraded）。
func (w *WatchdogManager) State() watchdogState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// StateString 返回导出字符串形式（供 api 包接线，避免依赖未导出类型）。
func (w *WatchdogManager) StateString() string {
	return string(w.State())
}

// Spawn 启动 watchdog 子进程（design.md §10）。
// spawn 失败 MUST 拒绝启动服务端——返回 error。
// 子进程以 `ocdeck-server watchdog <ppid> <socketName> <tmpdir>` 自调，
// 通过 stdout 写 "READY\n" 完成 ack 握手确认就绪（带超时，超时视为 spawn 失败）。
//
// Wait 并发竞争消除：spawn 后启动单一 waitOwner goroutine 调 cmd.Wait，
// 监控 goroutine（reaper）不直接调 Wait；Stop 经 ctx cancel + 等待 waitDone
// （带超时→Kill）。异常退出由监控 goroutine 触发指数退避重启（上限 3 次→degraded）。
func (w *WatchdogManager) Spawn() error {
	w.stopMu.Lock()
	defer w.stopMu.Unlock()
	w.mu.Lock()
	if w.sub != nil && w.state == watchdogRunning {
		w.mu.Unlock()
		return fmt.Errorf("watchdog: already running")
	}
	if w.binaryPath == "" {
		w.mu.Unlock()
		return fmt.Errorf("watchdog: empty binary path")
	}
	w.mu.Unlock()

	if err := w.spawnOnce(); err != nil {
		return err
	}
	// 启动监控 goroutine：检测 watchdog 异常退出并指数退避重启（上限 3 次→degraded）。
	go w.supervise()
	return nil
}

// spawnOnce 执行一次 spawn + READY 握手。调用方持有 stopMu。
func (w *WatchdogManager) spawnOnce() error {
	ppid := os.Getpid()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, w.binaryPath, WatchdogSubcommand,
		fmt.Sprintf("%d", ppid), w.socketName, w.tmpdir)
	// 清洗 env 注入（与主进程一致，仅最小基础集 + TMUX_TMPDIR）。
	cmd.Env = withTmuxTmpdir(w.baseEnv, w.tmpdir)
	// READY 握手：子进程启动后写 "READY\n" 到 stdout 表示就绪。
	// 用 pipe 读取首行，完成握手后不再消费 stdout（子进程日志写 stderr）。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("watchdog: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		cancel()
		w.setState(watchdogDegraded)
		return fmt.Errorf("watchdog: spawn %s: %w", w.binaryPath, err)
	}

	// 等待 READY 握手（带超时）。
	readyCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "READY" {
				readyCh <- nil
				return
			}
			readyCh <- fmt.Errorf("watchdog: unexpected handshake %q", line)
			return
		}
		readyCh <- fmt.Errorf("watchdog: no READY line: %v", sc.Err())
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			// 握手失败：取消 ctx + 等待退出 + 清理。
			cancel()
			_ = cmd.Wait()
			w.setState(watchdogDegraded)
			return fmt.Errorf("watchdog: handshake: %w", err)
		}
	case <-time.After(watchdogReadyTimeout):
		cancel()
		_ = cmd.Wait()
		w.setState(watchdogDegraded)
		return fmt.Errorf("watchdog: READY handshake timeout after %s", watchdogReadyTimeout)
	}

	waitDone := make(chan struct{})
	w.mu.Lock()
	w.sub = cmd
	w.state = watchdogRunning
	w.waitDone = waitDone
	w.cancelFunc = cancel
	w.mu.Unlock()

	// 单一 waitOwner：唯一调用 cmd.Wait 的 goroutine，消除并发竞争。
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	return nil
}

// supervise 监控 watchdog 子进程退出，指数退避重启（上限 3 次→degraded）。
// 仅在子进程异常退出（非 Stop 主动终止）时重启。Stop 通过设置 stopping 标志
// 抑制重启。
func (w *WatchdogManager) supervise() {
	for {
		w.mu.Lock()
		waitDone := w.waitDone
		w.mu.Unlock()
		if waitDone == nil {
			return
		}
		<-waitDone
		w.mu.Lock()
		// 若 sub 已被 Stop 清空（stopping），不重启。
		if w.sub == nil {
			w.mu.Unlock()
			return
		}
		// 子进程异常退出——尝试重启。
		restarts := w.restartCount
		w.mu.Unlock()

		if restarts >= watchdogMaxRestarts {
			log.Printf("watchdog: exceeded max restarts (%d), entering degraded", watchdogMaxRestarts)
			w.setState(watchdogDegraded)
			w.mu.Lock()
			w.sub = nil
			w.waitDone = nil
			w.mu.Unlock()
			return
		}

		// 指数退避：base * 2^restarts。
		backoff := watchdogBaseBackoff << restarts
		select {
		case <-time.After(backoff):
		case <-w.stopRequested():
			return
		}

		w.stopMu.Lock()
		w.mu.Lock()
		// 再次检查是否已被 Stop。
		if w.sub == nil {
			w.mu.Unlock()
			w.stopMu.Unlock()
			return
		}
		w.restartCount++
		w.mu.Unlock()
		if err := w.spawnOnce(); err != nil {
			log.Printf("watchdog: restart %d failed: %v", restarts+1, err)
			w.stopMu.Unlock()
			// 继续循环，等待下一次 waitDone（spawnOnce 失败未启动新子进程时
			// waitDone 为旧值已关闭，需 break 避免空转）。
			continue
		}
		w.stopMu.Unlock()
	}
}

// stopRequested 返回一个在 Stop 时关闭的 channel（lazy 创建）。
func (w *WatchdogManager) stopRequested() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopCh == nil {
		w.stopCh = make(chan struct{})
	}
	return w.stopCh
}

// Stop 停止 watchdog（design.md §10，ack 握手，超时强杀）。
// 优雅关停顺序：watchdog 存活期清理会话 → 确认 runtime 空 → StopWatchdog → 退出。
// 幂等。Wait 并发竞争消除：设置 stopping + cancel ctx + 等待 waitDone（带超时→Kill）。
func (w *WatchdogManager) Stop() error {
	w.stopMu.Lock()
	defer w.stopMu.Unlock()
	w.mu.Lock()
	sub := w.sub
	waitDone := w.waitDone
	cancelFn := w.cancelFunc
	if sub == nil {
		w.mu.Unlock()
		return nil
	}
	// 标记 stopping：抑制 supervise 重启。
	if w.stopCh != nil {
		select {
		case <-w.stopCh:
		default:
			close(w.stopCh)
		}
	}
	w.sub = nil
	w.waitDone = nil
	w.cancelFunc = nil
	w.mu.Unlock()

	// 发 SIGTERM 通知子进程自退（watchdog 子进程收到 SIGTERM 即自退）。
	_ = sub.Process.Signal(syscall.SIGTERM)
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		// 超时：取消 ctx（exec.CommandContext ctx 取消触发 SIGKILL）+ Kill 兜底。
		if cancelFn != nil {
			cancelFn()
		}
		_ = sub.Process.Kill()
		<-waitDone
	}
	w.setState(watchdogOff)
	return nil
}

func (w *WatchdogManager) setState(s watchdogState) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

// --- watchdog 子进程入口 ---

// RunWatchdog 是 watchdog 子进程主循环（design.md §10 FSM）。
// ppid 为父进程 pid（轮询存活）；socketName/tmpdir 用于 kill-server。
// 返回 nil 表示正常自退（父亡后清理完成）；error 表示异常退出。
//
// 父亡检测改 os.Getppid() 变化判断（design.md §10 / B2）：kill(0) 有 PID 复用窗口
// （父退出后 PID 被新进程复用，kill(0) 仍返回成功），而 os.Getppid() 在父进程退出后
// 被 init/launchd 收养，返回值变化为 1——可靠区分父亡。
func RunWatchdog(ppid int, socketName, tmpdir string) error {
	m := &Manager{
		socketName: socketName,
		tmpdir:     tmpdir,
		baseEnv:    DefaultBaseEnv(os.LookupEnv),
		psProvider: darwinPSProvider{},
	}
	// READY 握手：写 "READY\n" 到 stdout 表示就绪（供父进程 Spawn 等待）。
	fmt.Fprintln(os.Stdout, "READY")

	// 轮询父进程存活：os.Getppid() 变化判断。
	// 初始记录父 pid，每次轮询若 getppid() != ppid 则父已退出（被 init 收养）。
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		}
		if os.Getppid() != ppid {
			break
		}
	}
	// 父亡 → kill 路径（内置全局 reaper）。
	return m.killServerGlobal(context.Background())
}

// killServerGlobal 执行 watchdog kill 路径：收集全部 ocdeck-* 会话 pane 快照 →
// kill-server → 收割幸存者（design.md §10）。
//
// 错误反映（B2）：无 server 时 kill-server 退出非零视为幂等成功；其他 kill-server
// 错误与未收割 survivors MUST 反映在返回/日志，不得无条件成功。
func (m *Manager) killServerGlobal(ctx context.Context) error {
	sessions, err := m.ListSessions()
	if err != nil {
		// 列不出会话（含无 server）——继续尝试 kill-server 保证幂等。
		sessions = nil
	}
	// 收集每个会话的 pane 子孙快照。
	var allTickets []ticketPayload
	for _, name := range sessions {
		snap, snapErr := m.snapshotSession(ctx, name)
		if snapErr != nil || snap == nil {
			// 快照失败：记录但不阻塞 kill 路径（尽力收割）。
			log.Printf("watchdog: snapshot %s failed: %v", name, snapErr)
			continue
		}
		allTickets = append(allTickets, snap.tickets...)
	}

	// kill-server（无 server 退出非零视为幂等成功；其他错误 MUST 反映）。
	_, _, killErr := m.execTmux(ctx, "kill-server")
	if killErr != nil {
		var ce *tmuxCmdError
		if !errors.As(killErr, &ce) || !isNoServerExit(ce) {
			// 非"无 server"的其他错误——记录，继续收割快照幸存者，不阻塞自退。
			log.Printf("watchdog: kill-server failed (non-idempotent): %v", killErr)
		}
	}

	// 收割幸存者：kill-server 后 SIGHUP 杀掉大多数，忽略 HUP 的逃逸子孙需 TERM/KILL。
	snap := &procSnapshot{pidSet: make(map[int]ticketPayload, len(allTickets))}
	for _, tp := range allTickets {
		snap.pidSet[tp.PID] = tp
	}
	remaining := m.reapSurvivors(ctx, snap)
	if len(remaining) > 0 {
		// 未收割 survivors MUST 反映在日志（不得无条件成功）。
		log.Printf("watchdog: %d survivors not reaped (tickets=%v)", len(remaining), encodeTickets(remaining))
		return fmt.Errorf("watchdog: %d survivors not reaped", len(remaining))
	}
	return nil
}

// withTmuxTmpdir 返回 baseEnv 叠加 TMUX_TMPDIR=tmpdir 的执行环境（与 Manager.tmuxExecEnv
// 同语义，watchdog 子进程也需 TMUX_TMPDIR 保证 socket 落在指定 tmpdir）。
func withTmuxTmpdir(baseEnv []string, tmpdir string) []string {
	if tmpdir == "" {
		return baseEnv
	}
	entry := "TMUX_TMPDIR=" + tmpdir
	out := make([]string, 0, len(baseEnv)+1)
	seen := false
	for _, e := range baseEnv {
		if strings.HasPrefix(e, "TMUX_TMPDIR=") {
			out = append(out, entry)
			seen = true
			continue
		}
		out = append(out, e)
	}
	if !seen {
		out = append(out, entry)
	}
	return out
}