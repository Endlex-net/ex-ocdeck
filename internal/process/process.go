// Package process 实现 tmux 会话后端：会话生命周期、reaper、watchdog FSM、
// 退出轮询（design.md §2/§10/§18）。
//
// 专属 tmux server：`tmux -L ocdeck -f /dev/null`（跳过用户 ~/.tmux.conf），
// `TMUX_TMPDIR=<dataDir>/tmux`（socket 隔离）。全部 tmux 命令以清洗后的 env
// 执行（仅最小基础集），会话 env 经 `-e KEY=VALUE` argv 显式注入，防止 tmux
// server 全局环境后门（design.md §2 exec env 清洗不变量）。
//
// 进程身份（pid+startTime）MUST NOT 出本包：对外 notice/接口一律使用 opaque
// cleanup ticket 字符串，包内编码 pid+startTime+pgid。
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ocdeck/internal/pty"
)

// SessionSpec 描述一次 tmux 会话创建请求（design.md §18）。
type SessionSpec struct {
	// Name 会话名，MUST 经 ValidateSessionName 校验（ocdeck-<taskID>-<role>）。
	Name string
	// Dir 会话工作目录（tmux new-session -c），MUST 为绝对路径。
	Dir string
	// Env 会话环境变量，经 -e KEY=VALUE argv 注入（MUST NOT 进命令字符串）。
	// 调用方负责合并优先级与注入内部变量（如 OPENCODE_SERVER_PASSWORD）。
	Env map[string]string
	// CmdArgv 命令 argv 白名单，逐元素单引号转义后拼成单个 shell 字符串
	// 作为 tmux new-session 的命令（design.md §2 命令构造）。
	CmdArgv []string
}

// CleanupDisposition 描述 KillSession 的清理结果分类（design.md §18）。
type CleanupDisposition string

const (
	// DispositionClean：SessionKilled=true、tickets 为空、取得过有效快照。
	DispositionClean CleanupDisposition = "clean"
	// DispositionSnapshotFailed：会话仍存活但快照失败，未执行 kill，可重试。
	DispositionSnapshotFailed CleanupDisposition = "retryable_snapshot_failed"
	// DispositionKillFailed：快照成功但 kill-session 失败会话仍在，
	// tickets MUST 携带该次有效快照的全部进程身份。
	DispositionKillFailed CleanupDisposition = "retryable_kill_failed"
	// DispositionReapFailed：会话已杀但逃逸收割失败，产生 tickets。
	DispositionReapFailed CleanupDisposition = "retryable_reap_failed"
	// DispositionSnapshotMissingDegraded：快照缺失且会话已消失，
	// 记 notice 且不可重试（已接受丢失）。
	DispositionSnapshotMissingDegraded CleanupDisposition = "snapshot_missing_degraded"
)

// KillResult 是 KillSession 的返回（design.md §18）。
type KillResult struct {
	// SessionKilled tmux kill-session 是否成功执行（会话已被 tmux 终止）。
	SessionKilled bool
	// Disposition 清理结果分类。
	Disposition CleanupDisposition
	// CleanupTickets 不透明的进程身份容器，供 RetryReap 重入。
	// clean 时为空；kill_failed/reap_failed 时携带有效快照身份。
	CleanupTickets []string
}

// Manager 是 tmux 会话后端的入口，持有 socket/tmpdir 等运行时配置。
//
// 所有方法线程安全：tmux 命令以 argv 数组调用，context 可取消，输出有界。
type Manager struct {
	// socketName tmux -L 参数（默认 ocdeck；测试可注入随机名隔离）。
	socketName string
	// tmpdir TMUX_TMPDIR 路径，tmux server socket 文件所在目录。
	tmpdir string
	// baseEnv 执行 tmux 命令时使用的清洗后基础 env（design.md §2）。
	baseEnv []string
	// psProvider ps 命令实现注入点（Darwin 默认 darwinPSProvider）。
	psProvider psProvider
	// execTmuxFn 测试注入点：非 nil 时 execTmux 委托给它（用于观测 ctx deadline 等，
	// 避免测试依赖 5s 真实墙钟）。生产留空走真实 exec.CommandContext 路径。
	execTmuxFn func(ctx context.Context, args ...string) (stdout, stderr string, err error)
}

// Options 构造 Manager 的可注入参数，便于测试隔离。
type Options struct {
	// SocketName tmux -L 参数。生产固定 "ocdeck"；测试用随机名隔离。
	SocketName string
	// Tmpdir TMUX_TMPDIR 路径。生产为 <dataDir>/tmux；测试用 t.TempDir()。
	Tmpdir string
	// BaseEnv 执行 tmux 命令的清洗 env。nil 表示用 DefaultBaseEnv。
	BaseEnv []string
	// PSProvider ps 命令注入点；nil 表示 darwinPSProvider。
	PSProvider psProvider
}

// DefaultBaseEnv 返回设计 §2 规定的最小基础 env（不含 OCDECK_*/密码等内部变量）。
// proxy 三件套仅在宿主存在时加入。
func DefaultBaseEnv(hostEnv func(string) (string, bool)) []string {
	if hostEnv == nil {
		hostEnv = os.LookupEnv
	}
	base := []string{
		"COLORTERM", "HOME", "USER", "PATH", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "SSH_AUTH_SOCK",
	}
	out := make([]string, 0, len(base)+4)
	// TERM 必须是 terminfo 库中存在的规范值（xterm.js 客户端即 xterm-256color），
	// MUST NOT 继承宿主 TERM——宿主可能是 terminfo 不认识的值（如 xterm-ghostty），
	// tmux 会报 "missing or unsuitable terminal" 拒绝启动 attach 客户端。
	out = append(out, "TERM=xterm-256color")
	for _, k := range base {
		if v, ok := hostEnv(k); ok && v != "" {
			out = append(out, k+"="+v)
		} else if k == "TMPDIR" {
			// TMPDIR 兜底：tmux 需要 tmpdir，若宿主未设则用 /tmp。
			out = append(out, "TMPDIR=/tmp")
		}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if v, ok := hostEnv(k); ok && v != "" {
			out = append(out, k+"="+v)
		}
	}
	// Locale 兜底（design.md D0）：LANG/LC_ALL/LC_CTYPE 三者均未设置或为空串时注入默认
	// LANG=en_US.UTF-8。launchd 启动的 server 常无 locale，导致 tmux attach 客户端
	// client_utf8=0，CJK 输出被转写为 `_`。任一高位变量已设置非空值则原样透传（已在
	// 上方基础集循环），此处仅兜底三者全无效的场景；空串视为未设置，不抑制注入。
	langVal, _ := hostEnv("LANG")
	lcAllVal, _ := hostEnv("LC_ALL")
	lcCtypeVal, _ := hostEnv("LC_CTYPE")
	if langVal == "" && lcAllVal == "" && lcCtypeVal == "" {
		out = append(out, "LANG=en_US.UTF-8")
	}
	return out
}

// New 构造 Manager。tmpdir MUST 在调用前已创建（0700）。
func New(opts Options) *Manager {
	m := &Manager{
		socketName: opts.SocketName,
		tmpdir:     opts.Tmpdir,
		psProvider: opts.PSProvider,
	}
	if m.socketName == "" {
		m.socketName = "ocdeck"
	}
	if m.baseEnv == nil {
		m.baseEnv = opts.BaseEnv
	}
	if m.baseEnv == nil {
		m.baseEnv = DefaultBaseEnv(os.LookupEnv)
	}
	if m.psProvider == nil {
		m.psProvider = darwinPSProvider{}
	}
	return m
}

// EnsureTmpDir 创建并校验 tmpdir（0700）。生产路径为 <dataDir>/tmux。
func EnsureTmpDir(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "tmux")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", fmt.Errorf("process: create tmux tmpdir %s: %w", p, err)
	}
	return p, nil
}

// sessionNameRe 校验 ocdeck-<taskID>-<role> 形态，taskID 字符集 [a-z0-9-]，
// role 段 MUST ∈ serve | tui | shell-<n>（design.md §2 会话命名表）。
var sessionNameRe = regexp.MustCompile(`^ocdeck-([a-z0-9]+(?:-[a-z0-9]+)*?)-(serve|tui|shell-[0-9]+)$`)

// ValidateSessionName 校验会话名格式（design.md §2：会话名 MUST 经格式校验后才用于命令）。
// 拒绝空、超长（tmux 上限 256 但此处更保守）、非法字符、不匹配 ocdeck- 前缀、
// role 段不在 {serve, tui, shell-<n>} 范围。
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("process: empty session name")
	}
	if len(name) > 128 {
		return fmt.Errorf("process: session name too long (%d)", len(name))
	}
	m := sessionNameRe.FindStringSubmatch(name)
	if m == nil {
		return fmt.Errorf("process: invalid session name %q (want ocdeck-<taskID>-<role>, role ∈ {serve,tui,shell-<n>}, charset [a-z0-9-])", name)
	}
	return nil
}

// tmuxArgs 构造 tmux 命令的完整 argv（含 -L/-f/-- 前缀），sub 为子命令及参数。
func (m *Manager) tmuxArgs(sub ...string) []string {
	return append([]string{"-L", m.socketName, "-f", "/dev/null"}, sub...)
}

// execTmux 执行 tmux 命令，以清洗后的 env 调用，返回 stdout/stderr 与错误。
// env = baseEnv + TMUX_TMPDIR=<tmpdir>（design.md §2：socket 隔离不变量 MUST 落到 cmd.Env，
// 不能依赖宿主环境变量隐式继承）。ctx 取消会杀子进程；输出有界（execOutputLimit）。
func (m *Manager) execTmux(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	if m.execTmuxFn != nil {
		return m.execTmuxFn(ctx, args...)
	}
	full := m.tmuxArgs(args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	cmd.Env = m.tmuxExecEnv()
	return runBounded(ctx, cmd, args)
}

// tmuxExecEnv 返回 baseEnv 叠加 TMUX_TMPDIR=tmpdir 的执行环境。
// TMUX_TMPDIR 是 tmux server socket 路径的决定性变量（design.md §2）——MUST 写入
// cmd.Env 以保证 socket 落在指定 tmpdir，而非宿主默认目录。若 baseEnv 已含 TMPDIR，
// TMUX_TMPDIR 与之独立（TMPDIR 影响临时文件，TMUX_TMPDIR 专指 socket 目录）。
// 同名 key 在 baseEnv 中出现时以本方法注入值为准（覆盖）。
func (m *Manager) tmuxExecEnv() []string {
	if m.tmpdir == "" {
		return m.baseEnv
	}
	tmuxTmpdirEntry := "TMUX_TMPDIR=" + m.tmpdir
	out := make([]string, 0, len(m.baseEnv)+1)
	seen := false
	for _, e := range m.baseEnv {
		if strings.HasPrefix(e, "TMUX_TMPDIR=") {
			out = append(out, tmuxTmpdirEntry)
			seen = true
			continue
		}
		out = append(out, e)
	}
	if !seen {
		out = append(out, tmuxTmpdirEntry)
	}
	return out
}

// runBounded 执行 cmd，stdout/stderr 有界读取（execOutputLimit）。
// 返回原始 stdout/stderr 字符串与执行错误（若失败，error 携带 stderr）。
// sub 为去掉全局 -L/-f 前缀的子命令（用于错误展示，避免泄露 -e env 值）。
func runBounded(ctx context.Context, cmd *exec.Cmd, sub []string) (stdout, stderr string, err error) {
	var outBuf, errBuf boundedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runErr != nil {
		return outBuf.String(), errBuf.String(), &tmuxCmdError{
			sub:    sub,
			stderr: errBuf.String(),
			err:    runErr,
		}
	}
	return outBuf.String(), errBuf.String(), nil
}

// tmuxCmdError 原样透传 tmux stderr，不做自动补救（日志红线：不含 -e 值/密码）。
// sub 仅含子命令与参数（已去除全局 -L/-f 前缀），且 MUST NOT 携带 -e 值——
// new-session 的 -e KEY=VALUE argv 会暴露 OPENCODE_SERVER_PASSWORD 与任务 env，
// 因此错误展示只保留命令名（如 "new-session"）+ exit code + 有界 stderr，
// 彻底剥离 -e 值。
type tmuxCmdError struct {
	sub    []string
	stderr string
	err    error
}

// cmdSummary 返回不含 -e 值的命令摘要（如 "tmux new-session -d -s <name>"）。
// -e KEY=VALUE argv 会暴露 env（含密码），MUST 跳过 -e 及其紧随的值，仅保留
// 其他子命令参数（命令名、-d、-s、-t 等）。
func (e *tmuxCmdError) cmdSummary() string {
	parts := []string{"tmux"}
	for i := 0; i < len(e.sub); i++ {
		if e.sub[i] == "-e" {
			// 跳过 -e 及其紧随的值（redact 全部 env 注入值）。
			i++
			continue
		}
		parts = append(parts, e.sub[i])
	}
	return strings.Join(parts, " ")
}

func (e *tmuxCmdError) Error() string {
	exitCode := exitCodeOf(e.err)
	summary := e.cmdSummary()
	boundedStderr := boundedStderr(e.stderr)
	if exitCode >= 0 {
		if boundedStderr != "" {
			return fmt.Sprintf("%s: exit %d (%s)", summary, exitCode, boundedStderr)
		}
		return fmt.Sprintf("%s: exit %d", summary, exitCode)
	}
	if boundedStderr != "" {
		return fmt.Sprintf("%s: %v (%s)", summary, e.err, boundedStderr)
	}
	return fmt.Sprintf("%s: %v", summary, e.err)
}

func (e *tmuxCmdError) Unwrap() error { return e.err }

// stderrShowLimit 错误对象中透传 stderr 的硬上限，防止巨量输出污染日志。
const stderrShowLimit = 512

func boundedStderr(s string) string {
	if len(s) <= stderrShowLimit {
		return s
	}
	return s[:stderrShowLimit] + "...(truncated)"
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// execOutputLimit 单次 tmux 命令 stdout/stderr 的硬上限（与 git 包一致，16MB）。
const execOutputLimit = 16 * 1024 * 1024

// boundedBuffer 有界写入缓冲，满足 io.Writer 契约，超限丢弃并标记 overflow。
// 借鉴 internal/git/exec.go 同款实现。
type boundedBuffer struct {
	buf      []byte
	overflow bool
	written  int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.overflow {
		return len(p), nil
	}
	remaining := execOutputLimit - b.written
	if remaining <= 0 {
		b.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf = append(b.buf, p[:remaining]...)
		b.overflow = true
		b.written += remaining
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	b.written += len(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }

// termGrace 是 reaper 对逃逸子孙的 SIGTERM 宽限期（design.md §2）。
const termGrace = 2 * time.Second

// --- 会话生命周期方法 ---

// NewSession 创建 tmux 会话（design.md §18）。
// 命令构造：从白名单 CmdArgv 逐元素单引号转义拼成单个 shell 字符串，
// env 经 -e KEY=VALUE argv 传递，精确 target -t =<name>。
func (m *Manager) NewSession(spec SessionSpec) error {
	if err := ValidateSessionName(spec.Name); err != nil {
		return err
	}
	if spec.Dir == "" {
		return fmt.Errorf("process: NewSession %s: empty dir", spec.Name)
	}
	if !filepath.IsAbs(spec.Dir) {
		return fmt.Errorf("process: NewSession %s: dir must be absolute, got %q", spec.Name, spec.Dir)
	}
	if len(spec.CmdArgv) == 0 {
		return fmt.Errorf("process: NewSession %s: empty CmdArgv", spec.Name)
	}

	// tmux new-session argv：-d -s <name> -c <dir> -e KEY=VALUE... -- <cmdString>
	args := []string{"new-session", "-d", "-s", spec.Name, "-c", spec.Dir}
	// env 经 -e argv 传递，KEY 顺序稳定（map 迭代无序，排序确保可复现测试）。
	keys := sortedKeys(spec.Env)
	for _, k := range keys {
		v := spec.Env[k]
		if err := validateEnvKey(k); err != nil {
			return fmt.Errorf("process: NewSession %s: env key %q: %w", spec.Name, k, err)
		}
		args = append(args, "-e", k+"="+v)
	}
	cmdString := buildShellCommand(spec.CmdArgv)
	args = append(args, "--", cmdString)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, stderr, err := m.execTmux(ctx, args...)
	if err != nil {
		return fmt.Errorf("process: NewSession %s: %w", spec.Name, err)
	}
	_ = stderr
	return nil
}

// ErrNoTmuxServer 表示专属 tmux server 未启动（无任何会话）。
// HasSession/ListSessions 等命令在无 server 时返回此错误，供调用方区分
// "无 server"（空运行时，正常）与"单会话不存在"（has-session can't find session）。
var ErrNoTmuxServer = errors.New("tmux server not running")

// HasSession 返回会话是否存在（design.md §18，区分"会话不存在"与"tmux 命令失败"）。
// 无 tmux server 时返回 (false, ErrNoTmuxServer)——这是正常的空运行时状态，
// 调用方据此与"单会话不存在"区分（后者返回 (false, nil)）。
func (m *Manager) HasSession(name string) (bool, error) {
	if err := ValidateSessionName(name); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := m.execTmux(ctx, "has-session", "-t", "="+name)
	if err == nil {
		return true, nil
	}
	var ce *tmuxCmdError
	if errors.As(err, &ce) {
		if isNoServerExit(ce) {
			return false, ErrNoTmuxServer
		}
		if isSessionNotFoundExit(ce) {
			return false, nil
		}
	}
	// 其他错误（ctx 超时、tmux 二进制缺失、权限/协议错误等）向上传播。
	return false, fmt.Errorf("process: HasSession %s: %w", name, err)
}

// ListSessions 列出当前 socket 下所有 ocdeck-* 会话（design.md §18）。
// 无 server 时返回空列表（非错误）。
func (m *Manager) ListSessions() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, err := m.execTmux(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		// list-sessions 无 server 时 exit 1，stderr "no server running"——视为空列表。
		var ce *tmuxCmdError
		if errors.As(err, &ce) && isNoServerExit(ce) {
			return nil, nil
		}
		return nil, fmt.Errorf("process: ListSessions: %w", err)
	}
	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ocdeck-") {
			sessions = append(sessions, line)
		}
	}
	return sessions, nil
}

// ShowSessionEnvContext 读取会话内某个环境变量值，语义与 ShowSessionEnv 一致，
// 但使用调用方 ctx（design.md §18 / cross-project-active-sessions D0：ctx-aware 聚合）。
// 内部再以 5s 封顶（context.WithTimeout）：调用方更短的 deadline 照常优先生效，
// 无 deadline 的调用方（既有项目任务列表/任务详情端点请求 ctx）获得与改造前相同的 5s 保护。
// 不存在返回 ("", nil)；tmux 命令失败返回 error。
func (m *Manager) ShowSessionEnvContext(ctx context.Context, name, key string) (string, error) {
	if err := ValidateSessionName(name); err != nil {
		return "", err
	}
	if err := validateEnvKey(key); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stdout, _, err := m.execTmux(ctx, "show-environment", "-t", "="+name, key)
	if err != nil {
		var ce *tmuxCmdError
		if errors.As(err, &ce) && isNoServerExit(ce) {
			return "", fmt.Errorf("process: ShowSessionEnv %s: %w", name, ErrNoTmuxServer)
		}
		return "", fmt.Errorf("process: ShowSessionEnv %s %s: %w", name, key, err)
	}
	// tmux show-environment 输出形如 "KEY=value"，取等号后内容（值可能含 =）。
	line := strings.TrimRight(stdout, "\n")
	idx := strings.IndexByte(line, '=')
	if idx < 0 {
		return "", nil
	}
	return line[idx+1:], nil
}

// ShowSessionEnv 读取会话内某个环境变量值（design.md §18，密码/端口恢复）。
// 兼容既有调用方：固定 5s 超时，委托 ShowSessionEnvContext。外部行为不变。
// 不存在返回 ("", nil)；tmux 命令失败返回 error。
func (m *Manager) ShowSessionEnv(name, key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.ShowSessionEnvContext(ctx, name, key)
}

// --- 退出监视 ---

// WatchEventType 退出事件类型，供 TaskManager 区分处理（design.md §18/§4）。
type WatchEventType string

const (
	// WatchEventSessionExit 会话正常退出（has-session 返回不存在）。
	WatchEventSessionExit WatchEventType = "session_exit"
	// WatchEventRuntimeLoss 全局运行时丢失（tmux server 消失，全部注册会话已丢失）。
	WatchEventRuntimeLoss WatchEventType = "runtime_loss"
	// WatchEventInfraError 基础设施错误（tmux 命令持续失败但非 server 消失）。
	WatchEventInfraError WatchEventType = "infra_error"
)

// WatchEvent 携带类型化退出事件，供 callback 区分会话退出/运行时丢失/基础设施错误。
type WatchEvent struct {
	Type WatchEventType
	Err  error // infra_error 时携带底层错误；其他类型为 nil
}

// WatchExit 以 1-2s 周期轮询 has-session，会话消失时调用 callback（design.md §18）。
// 返回 cancel 句柄与 done 通道：
//   - 调用 cancel 停止轮询（非阻塞，仅发信号）。
//   - done 在轮询 goroutine 退出后关闭，供调用方在关闭下游资源（如 store）前 join，
//     避免 cancel 后 goroutine 仍写已关资源（design.md §4 lifecycle 收敛）。
//
// callback 在单独 goroutine 调用，MUST NOT 阻塞或调用 Manager 的同步方法（避免死锁）。
// 三分结果（design.md §2）：
//   - 会话消失（has-session = 不存在）→ WatchEventSessionExit
//   - tmux server 消失（ErrNoTmuxServer）→ WatchEventSessionExit：被监视会话
//     随 server 一同消失（server 消失即该会话不存在；轮询单会话视角无法区分
//     "本会话退出导致 server 退出" 与 "server 崩溃导致会话丢失"，统一按会话退出
//     归类，P3 据此重启该会话）。runtime_loss 类型保留供后续 server 级监视使用。
//   - 临时 exec/权限/协议错误 → 退避重试；持续失败按基础设施错误 → WatchEventInfraError
func (m *Manager) WatchExit(name string, callback func(WatchEvent)) (cancel func(), done <-chan struct{}) {
	if err := ValidateSessionName(name); err != nil {
		// 无效名直接视为已消失，触发一次 callback 后返回空 cancel + 已关闭 done。
		go callback(WatchEvent{Type: WatchEventSessionExit})
		d := make(chan struct{})
		close(d)
		return func() {}, d
	}
	stop := make(chan struct{})
	doneCh := make(chan struct{})
	var once sync.Once
	cancel = func() { once.Do(func() { close(stop) }) }
	go func() {
		defer close(doneCh)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var backoff time.Duration
		var infraErr error
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			exists, err := m.HasSession(name)
			if err == nil {
				backoff = 0
				infraErr = nil
				if exists {
					continue
				}
				callback(WatchEvent{Type: WatchEventSessionExit})
				return
			}
			// server 消失：被监视会话随之消失，按会话退出归类。
			if errors.Is(err, ErrNoTmuxServer) {
				callback(WatchEvent{Type: WatchEventSessionExit})
				return
			}
			// 其他基础设施错误：退避重试，持续失败按 infra_error 处理。
			infraErr = err
			if backoff < 8*time.Second {
				backoff += time.Second
			}
			if backoff >= 8*time.Second {
				callback(WatchEvent{Type: WatchEventInfraError, Err: infraErr})
				return
			}
		}
	}()
	return cancel, doneCh
}

// --- attach 客户端 PTY ---

// AttachPty 构造 attach 客户端 PTY（design.md §18：复用 Manager 的
// socket/tmpdir/baseEnv 构造 `tmux -L <socket> attach -t <name>` 客户端）。
// 返回的 *pty.Pty 仅作渲染客户端——断开/杀 PTY 只 detach，不影响 tmux 会话
// 与任务进程。cols/rows 为初始 PTY 尺寸（≤0 默认 80×24），兑现 WS 首帧尺寸契约。
//
// attach 客户端 env 使用与 tmux 命令一致的清洗后 baseEnv + TMUX_TMPDIR，
// 保证 attach 连接到 ocdeck 专属 socket（而非宿主默认 socket）。
func (m *Manager) AttachPty(name string, cols, rows int) (*pty.Pty, error) {
	if err := ValidateSessionName(name); err != nil {
		return nil, err
	}
	args := append([]string{"-L", m.socketName, "-f", "/dev/null"}, "attach", "-t", name)
	cmd := exec.Command("tmux", args...)
	cmd.Env = m.tmuxExecEnv()
	return pty.Open(cmd, "", m.tmuxExecEnv(), cols, rows)
}

// --- 辅助 ---

// sortedKeys 返回 map key 的排序切片，确保 env -e 顺序可复现。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// sortStrings 简单插入排序（env key 数量小，避免引入 sort 包开销与可复现）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// envKeyRe 校验 env key 形态：字母数字下划线开头，允许 - ._，禁止空格/特殊字符
// （防止 -e 解析歧义与注入）。
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

func validateEnvKey(k string) error {
	if k == "" || len(k) > 256 {
		return fmt.Errorf("invalid env key length")
	}
	if !envKeyRe.MatchString(k) {
		return fmt.Errorf("invalid env key charset")
	}
	return nil
}

// isSessionNotFoundExit 判断 tmuxCmdError 是否为"会话不存在"（design.md §18：区分
// "会话不存在"与"命令失败"）。MUST NOT 仅凭 exit code 1 判定——权限/协议错误也返回
// exit 1，误判会清掉本应保留的会话。仅当 stderr 含明确的"找不到会话"特征串时认定。
func isSessionNotFoundExit(ce *tmuxCmdError) bool {
	low := strings.ToLower(ce.stderr)
	// tmux has-session 不存在会话：stderr "can't find session: <name>"。
	if strings.Contains(low, "can't find session") {
		return true
	}
	// 部分版本表述为 "session not found"。
	if strings.Contains(low, "session not found") {
		return true
	}
	return false
}

// isNoServerExit 判断 tmuxCmdError 是否为"无 tmux server"（list-sessions/kill-server 等）。
// tmux 无 server 时 stderr 含 "no server running"；exit code 通常为 1，但 MUST 以
// stderr 特征串为准——避免把会话不存在的 exit 1 误判为"无 server"。
// 另一种无 server 形态：TMUX_TMPDIR 已设但 server 从未启动时，连接 socket 报
// "no such file or directory"（socket 文件不存在），同样表示空运行时。
func isNoServerExit(ce *tmuxCmdError) bool {
	low := strings.ToLower(ce.stderr)
	if strings.Contains(low, "no server running") {
		return true
	}
	// socket 连接失败：error connecting to <path> (...) ... No such file or directory
	if strings.Contains(low, "no such file or directory") {
		return true
	}
	return false
}