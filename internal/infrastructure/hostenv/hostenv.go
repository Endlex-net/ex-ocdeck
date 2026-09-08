// Package hostenv 提供宿主环境变量解析：服务端进程环境优先，未命中时兜底从
// 用户 login shell 捕获的环境解析。systemd user service / launchd 启动的 server
// 进程环境极简，兜底捕获使 follow_host 解析与 API 列举/resolvedValue 仍能取到
// 用户 shell 启动文件中的变量；基础集（envBaselineKeys）只取进程环境，不经本包兜底。
package hostenv

import (
	"bytes"
	"context"
	"errors"
	"log"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// captureTimeout 限制 login shell 捕获的最长耗时（交互 shell 加载 profile 可能较慢），
// 超时按捕获失败处理，不长时间阻塞环境变量解析。包级变量供测试注入。
var captureTimeout = 5 * time.Second

// waitDelay 限制进程退出后 stdout 管道收尾的最长等待（Cmd.WaitDelay）：后代进程
// （如 setsid 脱离进程组）可能仍持有管道写端，超期即强制关闭管道（Wait 返回
// ErrWaitDelay，按捕获失败处理），保证 captureMu 不被无限占用。包级变量供测试注入。
var waitDelay = 500 * time.Millisecond

// Capture 抽象 login shell 环境捕获（返回 nil 表示捕获失败）。包级变量供
// 宿主隔离类测试注入 fake（禁用真实 shell 捕获）；生产路径为 captureLoginShellEnv。
var Capture = captureLoginShellEnv

// BEGIN/END 标记包裹 env -0 输出区间：login shell 可能打印 motd/prompt 等垃圾输出，
// 解析时按标记过滤。标记名必须与 captureLoginShellEnv 中的捕获命令保持一致。
const (
	envBeginMark = "__OCDECK_ENV_BEGIN__"
	envEndMark   = "__OCDECK_ENV_END__"
)

// captureMu 串行化「捕获 + 发布」临界区（D3 并发协调）：懒加载首次捕获与 Refresh
// 的捕获均在锁内执行，保证发布顺序 = 捕获顺序——并发 Refresh 时后完成者的捕获必然
// 更晚开始，不存在旧结果迟到覆盖新缓存。缓存的实际读写仍由 mu 保护（读路径可并发）。
var (
	captureMu sync.Mutex

	mu             sync.RWMutex
	shellEnvCache  map[string]string
	shellEnvLoaded bool
)

// Lookup 解析宿主环境变量：进程环境命中且非空直接返回；未命中时兜底查 login shell
// 捕获缓存（懒加载，进程生命周期内只捕获一次，含失败结果不重试）。两侧均未命中
// 返回未找到。并发安全。
func Lookup(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, true
	}
	if v, ok := shellEnv()[key]; ok && v != "" {
		return v, true
	}
	return "", false
}

// ResetForTest 将捕获缓存重置为未加载态（仅测试使用：跨包测试需要确定性的缓存
// 初始态；生产路径无重置语义，缓存迁移严格遵循 D3 状态机）。
func ResetForTest() {
	captureMu.Lock()
	defer captureMu.Unlock()
	mu.Lock()
	shellEnvCache, shellEnvLoaded = nil, false
	mu.Unlock()
}

// Snapshot 返回 login shell 捕获缓存副本（未加载时触发懒加载捕获；含 `KEY=` 空值键，
// 供列举 source 按键存在性计算）。失败缓存或空集返回 nil（调用方按空集降级处理）。
// 并发安全。
func Snapshot() map[string]string {
	env := shellEnv()
	if len(env) == 0 {
		return nil
	}
	return maps.Clone(env)
}

// Refresh 重新执行 login shell 捕获并原子替换缓存，供系统环境变量区手动刷新
// （进程 env 部分实时读、无需刷新）。失败按 D3 状态机处理：已有缓存（成功/失败态）
// 保留旧缓存；未加载态迁移为失败缓存（后续普通读取按失败缓存服务、不自动重试，
// 仅下一次显式 Refresh 可重试）。错误信息与日志均不含捕获内容（明文存储与日志红线）。
func Refresh() error {
	// 捕获+发布整体串行化（D3）：与懒加载共用 captureMu，发布顺序 = 捕获顺序，
	// 后到者不得用旧结果覆盖新缓存。
	captureMu.Lock()
	defer captureMu.Unlock()

	env := Capture()
	mu.Lock()
	defer mu.Unlock()
	if env == nil {
		if !shellEnvLoaded || shellEnvCache == nil {
			// 从未持有成功缓存：迁移失败缓存（与懒加载失败同语义），防止隐式自动重试。
			shellEnvCache, shellEnvLoaded = nil, true
		}
		return errors.New("hostenv: capture login shell env failed")
	}
	shellEnvCache = env
	shellEnvLoaded = true
	return nil
}

// shellEnv 返回 login shell 捕获缓存；首次调用时懒加载捕获（捕获+发布经 captureMu
// 串行化，并发下整包只捕获一次且发布顺序 = 捕获顺序）。捕获失败同样置位 loaded
// （进程生命周期内不重试，手动重试走 Refresh）。
func shellEnv() map[string]string {
	mu.RLock()
	if shellEnvLoaded {
		env := shellEnvCache
		mu.RUnlock()
		return env
	}
	mu.RUnlock()

	captureMu.Lock()
	defer captureMu.Unlock()
	// double-check：等锁期间可能已被并发捕获（懒加载或 Refresh）。
	mu.RLock()
	if shellEnvLoaded {
		env := shellEnvCache
		mu.RUnlock()
		return env
	}
	mu.RUnlock()

	env := Capture()
	mu.Lock()
	shellEnvCache = env
	shellEnvLoaded = true
	mu.Unlock()
	return env
}

// captureLoginShellEnv 通过用户 login shell 捕获完整环境：
// -i -l 触发 .bashrc/.zshrc/.profile 等 profile/rc 脚本中的 export；-c 执行 && 链式
// 捕获命令——env 用显式路径且失败（如不支持 -0）时 END 标记不输出且退出码非零，
// 防止尾标记仍输出被误判为成功空结果。stderr 丢弃、stdin 不连接，避免污染解析或
// 阻塞等待输入。成功需同时满足：未超时、退出码为零、stdout 含完整有效帧（见
// parseLoginShellEnv）；任一不满足返回 nil（视为捕获失败，由调用方缓存失败结果）。
func captureLoginShellEnv() map[string]string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = defaultShell()
	}
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd := exec.Command(shell, "-ilc",
		`printf '\n`+envBeginMark+`\n' && /usr/bin/env -0 && printf '\n`+envEndMark+`\n\0'`)
	// 独立进程组：超时后 kill 整组，防止 shell 后代持有 stdout 管道导致 Wait 悬挂。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// K3：管道收尾仍需有限上限——setsid 脱离进程组的后代可继续持有 stdout 写端，
	// kill 原进程组不会关闭它。WaitDelay 保证进程退出/被杀后最多再等 waitDelay 即
	// 强制关闭管道（Wait 返回 ErrWaitDelay，按捕获失败处理，不发布缓存），
	// captureMu 得以释放、不留仍在写 buffer 的 goroutine。
	cmd.WaitDelay = waitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		// 日志红线：只记一行 warning，不输出任何捕获内容/键名（整包捕获）。
		log.Printf("hostenv: start login shell capture failed, fallback disabled: %v", err)
		return nil
	}
	// Wait 放单独 goroutine：超时分支先杀进程组再收 Wait 结果（组死 → 管道关闭 →
	// Wait 返回），不阻塞超时响应。
	waitc := make(chan error, 1)
	go func() { waitc <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitc
		log.Printf("hostenv: capture login shell env timed out, fallback disabled")
		return nil
	case err := <-waitc:
		if err != nil {
			log.Printf("hostenv: capture login shell env failed, fallback disabled: %v", err)
			return nil
		}
	}
	env, ok := parseLoginShellEnv(stdout.Bytes())
	if !ok {
		log.Printf("hostenv: login shell env frame invalid, fallback disabled")
		return nil
	}
	return env
}

// parseLoginShellEnv 解析 stdout 的字节级帧（design.md D2）：
//   - 定位独立成行的 BEGIN 标记作为帧头（其前的垃圾输出忽略）；
//   - 其后按 NUL 切分为完整记录，记录内的换行与标记文本一律视为值内容；
//   - 帧结束仅由精确等于 END 控制 token（"\n__OCDECK_ENV_END__\n"）的完整 NUL 终止记录
//     标识：合法环境记录必含 =，与控制记录无歧义；键或值中含完整标记文本的记录不构成
//     帧边界、原样保留；END 控制记录之后的尾部内容忽略；
//   - 缺 END 控制记录 / 缺终止 NUL / 帧截断 / BEGIN 缺失 / 顺序颠倒均判失败；
//     忽略后零个合法条目同样判失败（不得发布成功空缓存）。
//   - 条目按第一个 = 切分（值可含 = 与换行）；无 = 忽略；合法 KEY= 空值条目保留进
//     缓存（键存在性供列举 source 计算，Lookup 对空值视为未命中）；重复键取首条。
func parseLoginShellEnv(out []byte) (map[string]string, bool) {
	// 定位独立成行的 BEGIN 标记（唯一帧头；其后记录内的标记文本按值处理，不参与定位）。
	// 仅接受以实际换行结束的 BEGIN 行：BEGIN 恰好结束于 EOF（无换行）属截断帧，
	// 判捕获失败（L1：若在 EOF 伪行上匹配，起始索引会越过切片末尾导致越界 panic）。
	beginEnd := -1
	lineStart := 0
	for i := 0; i < len(out); i++ {
		if out[i] != '\n' {
			continue
		}
		if bytes.Equal(out[lineStart:i], []byte(envBeginMark)) {
			beginEnd = i + 1
			break
		}
		lineStart = i + 1
	}
	if beginEnd < 0 {
		return nil, false
	}
	endToken := []byte("\n" + envEndMark + "\n")
	env := map[string]string{}
	records := 0
	for pos := beginEnd; ; {
		nul := bytes.IndexByte(out[pos:], 0)
		if nul < 0 {
			// 剩余字节构不成完整 NUL 终止记录：缺 END 控制记录 / 帧截断。
			return nil, false
		}
		record := out[pos : pos+nul]
		if bytes.Equal(record, endToken) {
			// END 控制记录：帧结束，其后尾部内容忽略。
			if records == 0 {
				return nil, false
			}
			return env, true
		}
		k, v, found := bytes.Cut(record, []byte("="))
		if found && len(k) > 0 {
			if _, dup := env[string(k)]; !dup {
				env[string(k)] = string(v)
			}
			records++
		}
		pos += nul + 1
	}
}

// defaultShell 返回 SHELL 未设置时的兜底 shell：
// darwin 固定 /bin/zsh；linux 先查 /etc/passwd 中当前用户的登录 shell（用户可能
// 使用 zsh 等非 bash shell），查不到再回退 /bin/bash。
func defaultShell() string {
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	if shell := passwdShell(); shell != "" {
		return shell
	}
	return "/bin/bash"
}

// passwdShell 从 /etc/passwd 查当前用户（os/user.Current 的 Username 匹配）的登录
// shell（第 7 字段），仅接受非空且为绝对路径的值；任何一步失败返回空串（由调用方
// 回退 /bin/bash）。
func passwdShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	content, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	return parsePasswdShell(string(content), u.Username)
}

// parsePasswdShell 解析 passwd 内容，返回 username 对应行的第 7 字段（登录 shell）。
// 行格式 name:passwd:uid:gid:gecos:home:shell：格式损坏行（字段数不足 7）跳过；
// 匹配到用户后即决策——shell 非空且以 / 开头返回之，否则返回空串（不找同名后行）。
func parsePasswdShell(content, username string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[0] != username {
			continue
		}
		if shell := fields[6]; strings.HasPrefix(shell, "/") {
			return shell
		}
		return ""
	}
	return ""
}
