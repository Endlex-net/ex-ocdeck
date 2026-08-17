// Package config 负责服务端配置加载与启动期校验（design.md §10/§11/§14）。
//
// v1 仅通过环境变量配置（OCDECK_TOKEN / OCDECK_SHUTDOWN_POLICY 等），
// 配置文件方案留待后续阶段。运行时不可变。
package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"ocdeck/internal/opencode"
)

// ShutdownPolicy 关停策略（design.md §10）。
type ShutdownPolicy string

const (
	ShutdownPersist       ShutdownPolicy = "persist"
	ShutdownKillOnStart   ShutdownPolicy = "kill_on_start"
	ShutdownKillImmediate ShutdownPolicy = "kill_immediate"
)

// ContractBaseline / ContractMinVersion 是已验证契约区间的上/下限（design.md §11/§20）。
// 版本号仅作告警非门禁；激活门禁是能力探测。区间检查见 internal/opencode/CONTRACT.md。
// 唯一真值定义在 internal/opencode（契约归属方），这里通过常量别名引用，避免跨包重复字面量。
const (
	ContractBaseline   = opencode.ContractBaseline
	ContractMinVersion = opencode.ContractMinVersion
)

// PortRange serve 端口分配范围（design.md §3）。
type PortRange struct {
	Min int
	Max int
}

// DefaultPortRange 默认 50000-50999（design.md §3）。
var DefaultPortRange = PortRange{Min: 50000, Max: 50999}

// Config 服务端配置，启动期一次性加载并校验，运行时只读。
type Config struct {
	// Token 访问令牌，MUST 非空（design.md §14）。
	Token string
	// DataDir 数据目录，worktree/tmux socket/DB 文件均在其下（design.md §2/§6/§10）。
	DataDir string
	// ListenAddr HTTP 监听地址，默认 127.0.0.1（design.md §14）。
	ListenAddr string
	// ListenPort HTTP 监听端口，默认 0 由系统分配（MVP 占位，后续可配置化）。
	ListenPort int
	// ServePortRange serve 进程端口范围（design.md §3）。
	ServePortRange PortRange
	// ShutdownPolicy 关停策略，默认 persist（design.md §10）。
	ShutdownPolicy ShutdownPolicy
	// AllowedOrigins WS Origin 白名单（design.md §7），空表示默认 localhost。
	AllowedOrigins []string
	// OpenCodeVersion 启动时 `opencode --version` 记录（design.md §11），仅告警非门禁。
	OpenCodeVersion string
	// VersionVerified OpenCodeVersion 落在 [ContractMinVersion, ContractBaseline] 的比较结果（design.md §11）。
	// 仅作告警/UI 提示，不作为激活门禁（门禁是能力探测）。
	VersionVerified bool
	// TmuxVersion 启动时 `tmux -V` 记录（design.md §2），MUST >= 3.2。
	TmuxVersion string
}

// EnvLookup 环境变量读取函数，便于测试注入。MUST NOT 在日志中输出值。
type EnvLookup func(key string) (string, bool)

// OSInfo GOOS 探测，便于测试注入（design.md §10 平台边界）。
type OSInfo struct {
	GOOS string
}

// Options 加载选项，便于测试注入而不污染 os.Getenv。
type Options struct {
	EnvLookup EnvLookup
	OS        OSInfo
	// BinaryProbe 版本探测函数，默认用 exec.Command；测试可注入 mock。
	OpenCodeProbe func() (string, error)
	TmuxProbe     func() (string, error)
}

// DefaultOptions 生产默认：读 os.Getenv、真实 exec 探测。
func DefaultOptions() Options {
	return Options{
		EnvLookup:     os.LookupEnv,
		OS:            OSInfo{GOOS: runtime.GOOS},
		OpenCodeProbe: probeOpenCodeVersion,
		TmuxProbe:     probeTmuxVersion,
	}
}

// Load 加载并校验配置。返回的 release 持有 dataDir 单实例 flock，
// 调用方 MUST 在进程退出前调用 release（design.md §10）。
func Load(opts Options) (*Config, func(), error) {
	if opts.EnvLookup == nil {
		opts.EnvLookup = os.LookupEnv
	}
	if opts.OS.GOOS == "" {
		opts.OS.GOOS = runtime.GOOS
	}
	if opts.OpenCodeProbe == nil {
		opts.OpenCodeProbe = probeOpenCodeVersion
	}
	if opts.TmuxProbe == nil {
		opts.TmuxProbe = probeTmuxVersion
	}

	// 平台边界：v1 仅 macOS/Darwin（design.md §10）。
	if opts.OS.GOOS != "darwin" {
		return nil, nil, fmt.Errorf("ocdeck v1 only supports macOS/Darwin, current GOOS=%s", opts.OS.GOOS)
	}

	token, ok := opts.EnvLookup("OCDECK_TOKEN")
	if !ok || token == "" {
		// 日志红线：不打印 token 值。
		return nil, nil, errors.New("OCDECK_TOKEN is required (env not set or empty)")
	}

	dataDir, ok := opts.EnvLookup("OCDECK_DATA_DIR")
	if !ok || dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve home dir for default data dir: %w", err)
		}
		dataDir = filepath.Join(home, ".ocdeck")
	}
	// 启动期转绝对路径（design.md §6）：相对路径在后续 worktree/socket/lock 拼接中
	// 会随进程 CWD 漂移，导致恢复/reconcile 路径不一致。
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve absolute data dir %s: %w", dataDir, err)
	}
	dataDir = filepath.Clean(absDir)

	listenAddr, ok := opts.EnvLookup("OCDECK_LISTEN_ADDR")
	if !ok || listenAddr == "" {
		listenAddr = "127.0.0.1"
	}
	listenPort := 0
	if portStr, ok := opts.EnvLookup("OCDECK_LISTEN_PORT"); ok && portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 0 || p > 65535 {
			return nil, nil, fmt.Errorf("invalid OCDECK_LISTEN_PORT %q", portStr)
		}
		listenPort = p
	}

	portRange := DefaultPortRange
	if pr, ok := opts.EnvLookup("OCDECK_SERVE_PORT_RANGE"); ok && pr != "" {
		parsed, err := parsePortRange(pr)
		if err != nil {
			return nil, nil, err
		}
		portRange = parsed
	}

	policy := ShutdownPersist
	if policyStr, ok := opts.EnvLookup("OCDECK_SHUTDOWN_POLICY"); ok && policyStr != "" {
		p, err := parseShutdownPolicy(policyStr)
		if err != nil {
			return nil, nil, err
		}
		policy = p
	}

	var allowedOrigins []string
	if origins, ok := opts.EnvLookup("OCDECK_ALLOWED_ORIGINS"); ok && origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if s := strings.TrimSpace(o); s != "" {
				allowedOrigins = append(allowedOrigins, s)
			}
		}
	}

	// 数据目录创建（0700）。
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}

	// 单实例 flock（design.md §10）：MUST 在任何版本探测与 store 打开之前获取，
	// 避免两实例并行启动时第二个实例已完成探测/打开 DB 才发现冲突。
	release, err := acquireInstanceLock(dataDir)
	if err != nil {
		return nil, nil, err
	}

	// 启动期版本探测（design.md §2/§11）。
	ocVersion, err := opts.OpenCodeProbe()
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("opencode binary check failed: %w", err)
	}
	tmuxVersion, err := opts.TmuxProbe()
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("tmux version check failed: %w", err)
	}
	if err := validateTmuxVersion(tmuxVersion); err != nil {
		release()
		return nil, nil, err
	}

	cfg := &Config{
		Token:           token,
		DataDir:         dataDir,
		ListenAddr:      listenAddr,
		ListenPort:      listenPort,
		ServePortRange:  portRange,
		ShutdownPolicy:  policy,
		AllowedOrigins:  allowedOrigins,
		OpenCodeVersion: ocVersion,
		VersionVerified: VersionSupported(ocVersion),
		TmuxVersion:     tmuxVersion,
	}
	if !cfg.VersionVerified {
		log.Printf("warning: opencode version %s outside verified contract range [%s, %s]; ocdeck may behave unexpectedly (activation is gated by capability probe, not version)", ocVersion, opencode.ContractMinVersion, ContractBaseline)
	}
	return cfg, release, nil
}

func parseShutdownPolicy(s string) (ShutdownPolicy, error) {
	switch ShutdownPolicy(s) {
	case ShutdownPersist, ShutdownKillOnStart, ShutdownKillImmediate:
		return ShutdownPolicy(s), nil
	default:
		return "", fmt.Errorf("invalid OCDECK_SHUTDOWN_POLICY %q (want persist|kill_on_start|kill_immediate)", s)
	}
}

func parsePortRange(s string) (PortRange, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return PortRange{}, fmt.Errorf("invalid OCDECK_SERVE_PORT_RANGE %q (want MIN-MAX)", s)
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return PortRange{}, fmt.Errorf("invalid OCDECK_SERVE_PORT_RANGE min %q: %w", parts[0], err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return PortRange{}, fmt.Errorf("invalid OCDECK_SERVE_PORT_RANGE max %q: %w", parts[1], err)
	}
	if min <= 0 || max <= 0 || min > max || min < 1024 || max > 65535 {
		return PortRange{}, fmt.Errorf("invalid OCDECK_SERVE_PORT_RANGE %d-%d", min, max)
	}
	return PortRange{Min: min, Max: max}, nil
}

// acquireInstanceLock 在 <dataDir>/ocdeck.lock 上获取独占 flock（design.md §10）。
// 失败拒绝启动；返回的 release 释放锁。
//
// 调用顺序约束（design.md §10）：flock MUST 在任何版本探测与 store 打开之前获取，
// 避免两个实例并行启动时第二个实例已完成探测/打开 DB 才发现冲突。
// 调用方（main）MUST 保证 store.Close() 在 release() 之前执行（defer 为 LIFO，
// 故 defer release() 先于 defer db.Close() 注册即可）。
//
// 平台语义（v1 仅 Darwin/macOS）：
//   - 使用 golang.org/x/sys/unix.Flock，即 BSD flock(2) on Darwin。
//   - LOCK_EX|LOCK_NB：非阻塞独占锁。已被另一进程持有时返回 EWOULDBLOCK
//     → 映射为"另一个 ocdeck 实例已持锁"的明确错误。
//   - flock 在 Darwin 上是进程级：进程退出（含崩溃）时内核自动释放，无需 atexit 处理。
//   - flock 不依赖 NFS，ocdeck v1 仅本地文件系统，不处理 NFS flock 语义差异。
//   - 未来非 POSIX 平台（如 Windows）需用平台原生独占原语（如 LockFileEx）替代，
//     封装在 internal/config 内，调用方契约不变。
func acquireInstanceLock(dataDir string) (func(), error) {
	lockPath := filepath.Join(dataDir, "ocdeck.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another ocdeck instance holds the lock on %s (data dir %s)", lockPath, dataDir)
		}
		return nil, fmt.Errorf("acquire instance lock %s: %w", lockPath, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		})
	}
	return release, nil
}

func probeOpenCodeVersion() (string, error) {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func probeTmuxVersion() (string, error) {
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// validateTmuxVersion 校验 tmux -V 输出 >= 3.2（design.md §2）。
func validateTmuxVersion(s string) error {
	major, minor, ok := parseTmuxSemver(s)
	if !ok {
		return fmt.Errorf("cannot parse tmux version %q", s)
	}
	if major < 3 || (major == 3 && minor < 2) {
		return fmt.Errorf("tmux version %s is too old, need >= 3.2", s)
	}
	return nil
}

func parseTmuxSemver(s string) (major, minor int, ok bool) {
	// 形如 "tmux 3.6a"。
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0, 0, false
	}
	ver := strings.TrimPrefix(fields[1], "v")
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(stripVersionSuffix(parts[0]))
	minor, err2 := strconv.Atoi(stripVersionSuffix(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// stripVersionSuffix 去掉数字段后的非数字后缀（如 "6a" → "6"）。
func stripVersionSuffix(s string) string {
	for i, c := range s {
		if c < '0' || c > '9' {
			return s[:i]
		}
	}
	// 全是数字或空串。
	return s
}

// VersionSupported 判断探测到的 opencode 版本是否落在已验证契约区间
// [ContractMinVersion, ContractBaseline]（design.md §11）。
// 形如 "opencode 1.18.18" 或 "1.18.18"；按 semver major.minor.patch 比较。
// 无法解析或不含三段数字时返回 false（触发告警）。
func VersionSupported(detected string) bool {
	maj, min, pat, ok := parseOCSemver(detected)
	if !ok {
		return false
	}
	loMaj, loMin, loPat, loOk := parseOCSemver(ContractMinVersion)
	hiMaj, hiMin, hiPat, hiOk := parseOCSemver(ContractBaseline)
	if !loOk || !hiOk {
		return false
	}
	return versionAtLeast(maj, min, pat, loMaj, loMin, loPat) &&
		versionAtLeast(hiMaj, hiMin, hiPat, maj, min, pat)
}

// parseOCSemver 严格解析 opencode 版本：取最后一个空白分隔 token，去掉可选前缀 `v`，
// 要求恰好 major.minor.patch 三段，每段为无前导零的非负整数（"0" 本身允许），
// 不含任何其它字符。不使用 stripVersionSuffix（该 helper 仅服务于 tmux）。
func parseOCSemver(s string) (major, minor, patch int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, 0, 0, false
	}
	ver := strings.TrimPrefix(fields[len(fields)-1], "v")
	parts := strings.Split(ver, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	for _, p := range parts {
		if !isStrictNumericPart(p) {
			return 0, 0, 0, false
		}
	}
	major, _ = strconv.Atoi(parts[0])
	minor, _ = strconv.Atoi(parts[1])
	patch, _ = strconv.Atoi(parts[2])
	return major, minor, patch, true
}

// isStrictNumericPart 判断是否为合法的数字段：非空、纯数字、无前导零（"0" 除外）。
func isStrictNumericPart(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func versionAtLeast(maj, min, pat, loMaj, loMin, loPat int) bool {
	if maj != loMaj {
		return maj > loMaj
	}
	if min != loMin {
		return min > loMin
	}
	return pat >= loPat
}
