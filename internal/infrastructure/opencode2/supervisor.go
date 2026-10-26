// supervisor.go 实现 v2 RuntimeSupervisor（opencode-v2-provider tasks 5.1–5.4 /
// design D3）：A+ 全局单 `opencode serve` 承载多任务 session。
//
// 职责边界（D9 binding 提交协议与 v1 同构）：supervisor 只确定并返回最终
// binding，MUST NOT 自行持久化 binding/任务状态。
//
// 启动顺序（唯一合法写序，D3 六步）：①分配端口并写入实例记录（pid=NULL 启动中）
// → ②经托管单元 ocdeck-v2-serve 启动 serve → ③取实际 serve pid → ④验证
// （pid 存活 + cmdline 匹配 + 派生凭据 GET /api/session 200 且 envelope 可解析）
// → ⑤更新记录落 pid → ⑥此后才允许业务副作用（创建 session/返回 binding）。
// 任一失败 MUST NOT 创建 session/发送 prompt/提交 binding。
//
// 重启恢复矩阵六分支按 D3 表实现（见 ensureServer）；端口固定不轮换；启动独立
// 有界重试（不扣任何任务 permit）；AcquireEphemeral 仅内存临时 binding。
package opencode2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ocdeck/internal/infrastructure/hostenv"
	"ocdeck/internal/infrastructure/process"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// ServeSessionName 是 v2 共享 serve 的 tmux 托管单元名（design D3：确定性进程
// 发现来源，与 supervisor 进程级单例同一单元）。
const ServeSessionName = "ocdeck-v2-serve"

// EphemeralTerminalRef 是仅内存临时 binding 的 TerminalRef 保留值（design D3）。
const EphemeralTerminalRef = "none"

// SupervisorProcSeam 是 serve 托管 seam（process.Manager 满足）。
type SupervisorProcSeam interface {
	NewSession(spec process.SessionSpec) error
	HasSession(name string) (bool, error)
	KillSession(name string) (process.KillResult, error)
	// SessionPID 返回托管单元的 pane pid（步骤③/崩溃窗口重新发现）。
	SessionPID(name string) (int, bool, error)
}

// RuntimeRecordSeam 是 provider_runtime 实例记录 seam（store 满足）。
type RuntimeRecordSeam interface {
	GetProviderRuntime(ctx context.Context, providerID, instanceID string) (store.ProviderRuntimeRecord, bool, error)
	PutProviderRuntimeStart(ctx context.Context, providerID, instanceID string, port int) error
	SetProviderRuntimePID(ctx context.Context, providerID, instanceID string, pid int) error
	SetProviderRuntimeError(ctx context.Context, providerID, instanceID string, cause string) error
}

// TaskEnvSeam 是任务环境/权限事实 seam（tasks 5.4；等价 v1
// SupervisorSideEffects 形态，由 task 层注入）：ResolveSessionEnv 返回会话环境
// 事实——首次路径=当前值（recovery=false）；created 恢复=同代快照
// （recovery=true，task 层同代语义）。origin=adopted 时 provider 不调用
// （承接保护规则）。
type TaskEnvSeam interface {
	ResolveSessionEnv(ctx context.Context, taskID string, recovery bool) (map[string]string, error)
}

// SupervisorDeps 注入 v2 supervisor 的实现依赖（无 Fx；显式构造注入）。
type SupervisorDeps struct {
	Proc         SupervisorProcSeam
	Store        RuntimeRecordSeam
	Env          TaskEnvSeam
	AllocatePort func(lastPort int, lastValid bool, exclude int) (int, error)
	// ProcessIdentity 返回 pid 的存活与 cmdline（步骤④身份核验）。生产注入
	// `ps -p <pid> -o command=` 适配；测试注入 fake。
	ProcessIdentity func(pid int) (cmdline string, alive bool, err error)
	// OnServerRestarted server 重拉成功回调（tasks 5.3：活跃任务按 binding 重建
	// session 关联）。nil 跳过。
	OnServerRestarted func()
	// OpencodeBin 二进制路径/名称（默认 "opencode"）。
	OpencodeBin string
	// ReadyTimeout/PollInterval 就绪轮询预算（生产 10s/500ms；测试注入短值）。
	ReadyTimeout time.Duration
	PollInterval time.Duration
	// StartAttempts 启动独立有界重试次数（D3：耗尽 fail-closed；默认 3）。
	StartAttempts int
	// MonitorInterval 监控循环轮询间隔（I9：就绪态轻量进程验活周期；默认 5s——
	// 独立于业务 PollInterval，监控不做网络探测）。
	MonitorInterval time.Duration
}

// V2Supervisor 是 RuntimeSupervisor 的 v2 真实承载（进程级单例语义由 supervisor
// 内部互斥 + 实例记录承载）。
type V2Supervisor struct {
	deps SupervisorDeps

	// contractOf 复用 provider 的运行时契约发现缓存（2.5 单一来源；由
	// NewSPIProviderWithDeps 装配注入）。
	contractOf func(ctx context.Context, c *Client) (WireContract, error)
	// newClient 构造指向 server 的 Client（测试注入 baseURL 覆写）。
	newClient func(port int, password string, opts Options) *Client

	mu           sync.Mutex // 统一状态锁：ensureServer/CheckServerAlive/ShutdownServer/业务失效（G3-F7）
	ready        bool
	port         int
	pid          int
	password     string
	shuttingDown bool // shutdown 开始后拒绝新业务 ensure（G3-F7）
	// everReady 本实例曾确认就绪（I9：首个成功 ensure 置位；其后成功拉起即重拉——
	// 通知收口的「真实恢复状态转换」判据，首次启动不通知）。
	everReady bool
	// monitorStop/monitorCancel/monitorDone 监控循环生命周期（I9：supervisor 是
	// server 监控与重拉的唯一 owner；启动后置——首个成功 ensure 后启动；关停
	// 停止——persist/kill 两种策略都必须停止）。monitorCancel 持有监控 ctx 的
	// cancel（Round3 I9：停止入口可取消阻塞中的在途重试后再 join）。Round4 I9：
	// 生命周期字段归 monitorMu（独立小锁，不与 s.mu 互绕——ensureServerOnce 全程
	// 持 s.mu，停止入口 MUST NOT 等待它才能取消在途重试）；monitorStopped 为
	// 终态——停止后监控不再被后续 ensure 拉起（停止即终态，非可重启生命周期）。
	monitorStop     chan struct{}
	monitorStopOnce sync.Once
	monitorDone     chan struct{}
	monitorCancel   context.CancelFunc
	monitorMu       sync.Mutex
	monitorStarted  bool // runMonitor 已派发（join 判据）
	monitorStopped  bool // 停止终态（停止后 startMonitor 不再拉起）
}

// NewV2Supervisor 构造 supervisor（deps 完整性由调用方保证——生产装配在
// main.go；测试注入 fake）。
func NewV2Supervisor(deps SupervisorDeps) *V2Supervisor {
	if deps.OpencodeBin == "" {
		deps.OpencodeBin = "opencode"
	}
	if deps.ReadyTimeout <= 0 {
		deps.ReadyTimeout = 10 * time.Second
	}
	if deps.PollInterval <= 0 {
		deps.PollInterval = 500 * time.Millisecond
	}
	if deps.StartAttempts <= 0 {
		deps.StartAttempts = 3
	}
	if deps.MonitorInterval <= 0 {
		deps.MonitorInterval = 5 * time.Second
	}
	return &V2Supervisor{deps: deps, newClient: NewClient,
		monitorStop: make(chan struct{}), monitorDone: make(chan struct{})}
}

// --- ensureServer：六步写序 + 重启恢复矩阵 ---

// ensureServer 幂等确保全局 server 在跑，返回 (port, 派生密码)。单飞互斥；
// 实现启动顺序六步与 D3 重启恢复矩阵；启动按 deps.StartAttempts 独立有界重试
// （不扣任何任务 permit），耗尽 fail-closed。「确认重拉成功」的通知收口在
// ensureServerOnce 的真实恢复状态转换（I9 唯一 owner）：仅 everReady（本实例曾
// 确认就绪）之后的成功拉起视为重拉并异步触发 OnServerRestarted（异步：通知体
// 会取任务锁，同步与 Resume 持有的任务锁成环）；首次启动不通知；并发调用经
// 同一状态锁串行，单次重拉通知至多一次。首个成功 ensure 后置启动监控循环
// （I9：server 死亡自动重拉的生产触发 owner；关停停止）。
func (s *V2Supervisor) ensureServer(ctx context.Context) (int, string, error) {
	port, password, err, restarted := s.ensureServerOnce(ctx)
	if err == nil && restarted && s.deps.OnServerRestarted != nil {
		go s.deps.OnServerRestarted()
	}
	return port, password, err
}

func (s *V2Supervisor) ensureServerOnce(ctx context.Context) (int, string, error, bool) {
	password, err := derivePassword()
	if err != nil {
		return 0, "", err, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return 0, "", appagent.NewError("supervisor.ensure_server",
			appagent.ErrKindRuntimeUnavailable, errShuttingDown), false
	}
	if s.ready {
		// G3-F1：快速路径轻量验活（进程存活 + 身份匹配）——失效即清除进入恢复矩阵，
		// 杜绝「ready 恒真」拿到失效端口/凭据。
		if s.verifyAliveLocked() {
			s.startMonitorLocked()
			debugf("ensure_server: reuse ready serve port=%d", s.port)
			return s.port, password, nil, false
		}
		debugf("ensure_server: ready state stale（进程/身份失效），进入恢复矩阵")
		s.ready = false
		s.pid = 0
	}
	wasEverReady := s.everReady
	var lastErr error
	for attempt := 1; attempt <= s.deps.StartAttempts; attempt++ {
		port, err := s.ensureServerAttempt(ctx, password)
		if err == nil {
			s.ready = true
			s.port = port
			s.password = password
			s.everReady = true
			s.startMonitorLocked()
			debugf("ensure_server: serve ready port=%d attempt=%d restarted=%t", port, attempt, wasEverReady)
			return port, password, nil, wasEverReady
		}
		s.pid = 0
		if errors.Is(err, errRuntimeIdentityMismatch) {
			// 身份/认证/协议不符：fail-closed，MUST NOT 仅凭端口响应接管
			//（D3 恢复矩阵；重试不改变事实——立即返回可判定错误）。
			return 0, "", err, false
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, "", ctx.Err(), false
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err(), false
		case <-time.After(time.Duration(attempt) * s.deps.PollInterval):
		}
	}
	return 0, "", fmt.Errorf("opencode2: server 启动重试耗尽（%d 次）: %w", s.deps.StartAttempts, lastErr), false
}

// startMonitorLocked 首个成功 ensure 后置启动监控循环（I9：supervisor 是 server
// 监控与重拉的唯一 owner；关停停止——Round4 I9：停止为终态，停止后不再拉起）。
// 调用方持 s.mu；生命周期字段归 monitorMu（独立小锁——StopServerMonitoring 不等
// s.mu 即可取消在途重试）。monitorStopped 终态或已在运行时幂等返回。
func (s *V2Supervisor) startMonitorLocked() {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	if s.monitorStopped || s.monitorCancel != nil {
		return // 终态 / 已在运行
	}
	monCtx, cancel := context.WithCancel(context.Background())
	s.monitorCancel = cancel
	done := make(chan struct{})
	s.monitorDone = done
	s.monitorStarted = true
	go func() {
		defer close(done)
		defer cancel()
		s.runMonitor(monCtx)
	}()
}

// runMonitor 后台监视循环（I9）：就绪态轻量进程验活（不触网），死亡即清就绪态
// 并交 ensureServer 自愈——独立重试与重拉通知经同一收口（不扣任务预算；生产
// 触发闭环不再依赖业务入口顺带调 CheckServerAlive）。关停（shuttingDown /
// monitorStop 关闭 / monCtx 取消）即退出。
func (s *V2Supervisor) runMonitor(monCtx context.Context) {
	ticker := time.NewTicker(s.deps.MonitorInterval)
	defer ticker.Stop()
	for {
		if monCtx.Err() != nil {
			return
		}
		select {
		case <-s.monitorStop:
			return
		case <-monCtx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		if s.shuttingDown {
			s.mu.Unlock()
			return
		}
		alive := s.ready && s.verifyAliveLocked()
		if !alive {
			s.ready = false
			s.pid = 0
		}
		s.mu.Unlock()
		if !alive {
			_, _, _ = s.ensureServer(monCtx)
		}
	}
}

// StopServerMonitoring 停止监控循环并等待退出（评审 I9 生命周期修复：persist 与
// kill 两种关停策略都必须停止监控——persist 策略不关 server 但同样不得在关停后
// 继续访问 store/重拉）。Round4 I9：生命周期读/取消走 monitorMu（不等待 s.mu——
// ensureServerOnce 全程持 s.mu，在途重试中的停止必须即时取消而非等待）；取消后
// 再 join；未启动（v2 注册但从未成功 ensure）直接标记终态返回不 join。停止为
// 终态：monitorStopped 置位后 startMonitor 不再拉起（幂等）。
func (s *V2Supervisor) StopServerMonitoring() {
	s.monitorStopOnce.Do(func() { close(s.monitorStop) })
	s.monitorMu.Lock()
	cancel := s.monitorCancel
	started := s.monitorStarted
	s.monitorMu.Unlock()
	if cancel != nil {
		cancel() // 打断阻塞中的 ensureServer（在途重试随 monCtx 取消快速返回）
	}
	if !started {
		s.monitorMu.Lock()
		s.monitorStopped = true // 终态：从未派发也不得被后续 ensure 拉起
		s.monitorMu.Unlock()
		return
	}
	<-s.monitorDone
	s.monitorMu.Lock()
	s.monitorCancel = nil
	s.monitorStopped = true
	s.monitorMu.Unlock()
}

// verifyAliveLocked 轻量验活（调用方持 s.mu）：实例 pid 存活且身份匹配。
// 不触网（快速路径仅进程级验证；业务层失败经 invalidateServer 原子清除）。
func (s *V2Supervisor) verifyAliveLocked() bool {
	if s.pid <= 0 {
		return false
	}
	cmdline, alive, err := s.deps.ProcessIdentity(s.pid)
	return err == nil && alive && identityMatches(cmdline, s.port)
}

// invalidateServer 业务探活失败时原子清除就绪态（G3-F1：下一 ensure 走恢复矩阵）。
func (s *V2Supervisor) invalidateServer() {
	s.mu.Lock()
	s.ready = false
	s.pid = 0
	s.mu.Unlock()
}

// errShuttingDown 标记 server 关停中（G3-F7：拒绝新业务 ensure）。
var errShuttingDown = errors.New("opencode2: server shutting down")

// errRuntimeIdentityMismatch 标记实例身份核验失败（fail-closed 不接管）。
var errRuntimeIdentityMismatch = errors.New("opencode2: runtime identity mismatch")

// ensureServerAttempt 单次启动/恢复尝试（矩阵六分支）。
func (s *V2Supervisor) ensureServerAttempt(ctx context.Context, password string) (int, error) {
	rec, ok, err := s.deps.Store.GetProviderRuntime(ctx, ProviderID, ProviderInstanceID)
	if err != nil {
		return 0, fmt.Errorf("get provider runtime record: %w", err)
	}
	if !ok {
		// 分支「无记录」：首次分配 → 走启动顺序。
		port, err := s.deps.AllocatePort(0, false, 0)
		if err != nil {
			return 0, fmt.Errorf("allocate port: %w", err)
		}
		return s.startServer(ctx, port, password)
	}
	if rec.PID.Valid {
		// 分支「记录存在、pid 有效」（G3-F4 三分）：①pid 存活且身份正确→验证复用；
		// ②pid 存活但身份错误→fail-closed；③pid 不存在→确认无存活后同端口重启。
		cmdline, alive, ierr := s.deps.ProcessIdentity(int(rec.PID.Int64))
		if ierr == nil && alive {
			if !identityMatches(cmdline, rec.Port) {
				return 0, fmt.Errorf("%w: pid %d 身份不符（cmdline=%q）", errRuntimeIdentityMismatch, rec.PID.Int64, cmdline)
			}
			if !s.verifyReady(ctx, rec.Port, password) {
				return 0, fmt.Errorf("%w: pid %d 凭据/协议验证失败", errRuntimeIdentityMismatch, rec.PID.Int64)
			}
			// Round-final J2：复用成功同步安装已验证 PID——否则轻量验活对
			// pid<=0 判死，重启后复用存活 server 仍被监控误判死亡（周期性
			// 虚假 OnServerRestarted + 反复任务重建）。
			s.pid = int(rec.PID.Int64)
			return rec.Port, nil
		}
		// pid 不存在（进程已死/身份查询不可得）→ 落入「确认无存活」分支
		//（托管单元重新发现或同端口重启）。
	}
	// pid NULL/旧/已死（崩溃窗口）：经托管单元重新发现存活 serve → 取实际 pid → 重做④。
	if pid, found, perr := s.deps.Proc.SessionPID(ServeSessionName); perr == nil && found {
		if !s.verifyReady(ctx, rec.Port, password) {
			return 0, fmt.Errorf("%w: 重新发现 %s（pid %d）但凭据/协议验证失败", errRuntimeIdentityMismatch, ServeSessionName, pid)
		}
		// 验证通过 → 更新记录复用（pid 核验含 cmdline）。
		cmdline, alive, ierr := s.deps.ProcessIdentity(pid)
		if ierr != nil || !alive || !identityMatches(cmdline, rec.Port) {
			return 0, fmt.Errorf("%w: 重新发现 pid %d 身份核验失败", errRuntimeIdentityMismatch, pid)
		}
		if err := s.deps.Store.SetProviderRuntimePID(ctx, ProviderID, ProviderInstanceID, pid); err != nil {
			// 步骤⑤失败：本轮禁止业务副作用（D3 矩阵「pid 更新失败」分支）。
			return 0, fmt.Errorf("set provider runtime pid: %w", err)
		}
		// Round-final J2：重新发现复用同样安装已验证 PID（同上——监控验活依据）。
		s.pid = pid
		return rec.Port, nil
	}
	// 分支「pid NULL 且托管单元未发现存活 serve」：同端口从步骤②继续；
	// 端口被占用 → fail-closed（端口固定不轮换）。
	return s.startServer(ctx, rec.Port, password)
}

// startServer 启动顺序②③④⑤：spawn serve（宿主 env 并集 + OPENCODE_SERVER_PASSWORD）
// → 取 pid → 验证 → 落 pid。任务/项目 env 仍不得写入共享 serve。
func (s *V2Supervisor) startServer(ctx context.Context, port int, password string) (int, error) {
	// 端口固定不轮换：确认端口未被占用（D3「确认无存活托管 serve 且端口未被占用」
	// 分支前置；被占用 → fail-closed 报错，不换端口）。
	if portOccupied(port) {
		_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID,
			fmt.Sprintf("port %d 被占用（固定不轮换）", port))
		return 0, fmt.Errorf("port %d 被占用（fail-closed，端口固定不轮换）", port)
	}
	// 写序①：实例记录先于启动（pid=NULL 启动中）。
	if err := s.deps.Store.PutProviderRuntimeStart(ctx, ProviderID, ProviderInstanceID, port); err != nil {
		return 0, fmt.Errorf("put provider runtime start: %w", err)
	}
	// ②经托管单元启动 serve（MUST 子命令形态；裸 opencode/根命令 --port 禁止）。
	spec := process.SessionSpec{
		Name: ServeSessionName,
		Dir:  "/",
		Env: hostenv.MergeSpawnEnv(map[string]string{
			"OPENCODE_SERVER_PASSWORD": password,
		}),
		CmdArgv: []string{
			s.deps.OpencodeBin, "serve",
			"--port", strconv.Itoa(port),
			"--hostname", "127.0.0.1",
		},
	}
	if err := s.deps.Proc.NewSession(spec); err != nil {
		_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID, err.Error())
		return 0, fmt.Errorf("spawn %s: %w", ServeSessionName, err)
	}
	// ③取实际 serve pid。
	pid, found, err := s.deps.Proc.SessionPID(ServeSessionName)
	if err != nil || !found {
		_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID, "serve pid 不可得")
		return 0, fmt.Errorf("serve pid 不可得（found=%v err=%v）", found, err)
	}
	// ④验证：pid 存活 + cmdline 匹配 + 派生凭据 GET /api/session 200 且 envelope
	// 可解析（401/200 仅就绪信号，不作归属证明）。
	if !s.awaitReady(ctx, port, password) {
		_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID, "server 未在预算内就绪")
		return 0, fmt.Errorf("server 未在预算内就绪（port %d）", port)
	}
	cmdline, alive, ierr := s.deps.ProcessIdentity(pid)
	if ierr != nil || !alive || !identityMatches(cmdline, port) {
		_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID,
			fmt.Sprintf("serve pid %d 身份核验失败", pid))
		return 0, fmt.Errorf("%w: serve pid %d 身份核验失败", errRuntimeIdentityMismatch, pid)
	}
	// ⑤更新记录落 pid（失败：本轮禁止业务副作用——返回错误，调用方重试走
	// 崩溃窗口重新发现分支）。
	if err := s.deps.Store.SetProviderRuntimePID(ctx, ProviderID, ProviderInstanceID, pid); err != nil {
		return 0, fmt.Errorf("set provider runtime pid: %w", err)
	}
	s.pid = pid
	return port, nil
}

// identityMatches 校验 cmdline 为本实例 serve 的启动形态（G3-F6 token 化精确
// 匹配）：executable 基名 opencode + serve + --port <恰好等于端口值> +
// --hostname 127.0.0.1；无法解析出该形态 → 保守拒绝（不猜）。
func identityMatches(cmdline string, port int) bool {
	tokens := strings.Fields(cmdline)
	if len(tokens) < 5 {
		return false
	}
	hasBin, hasServe := false, false
	for i, t := range tokens {
		base := t
		if idx := strings.LastIndexByte(t, '/'); idx >= 0 {
			base = t[idx+1:]
		}
		if base == "opencode" && i == 0 {
			hasBin = true
		}
		switch t {
		case "serve":
			hasServe = true
		case "--port":
			if i+1 < len(tokens) && tokens[i+1] == strconv.Itoa(port) {
				// --port 与端口值必须成对精确相等（拒绝 4096 匹配 40961 前缀）。
				if hasBin && hasServe && hasHostname(tokens) {
					return true
				}
			}
		}
	}
	return false
}

// hasHostname 检查 --hostname 127.0.0.1 成对出现。
func hasHostname(tokens []string) bool {
	for i, t := range tokens {
		if t == "--hostname" && i+1 < len(tokens) && tokens[i+1] == "127.0.0.1" {
			return true
		}
	}
	return false
}

// isTransportErr 判定客户端错误是否连接类（server 未响应——非 HTTP 状态错误）。
func isTransportErr(err error) bool {
	var hse *httpStatusError
	return err != nil && !errors.As(err, &hse) && !errors.Is(err, ErrUnauthorized)
}

// portOccupied 探测端口占用（D3「确认端口未被占用」前置）。
func portOccupied(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return true // 占用/不可绑定 → 视为占用（fail-closed）
	}
	_ = ln.Close()
	return false
}

// awaitReady 就绪轮询（GET /api/session 200 且 envelope 可解析；MUST NOT 用
// /global/health——SPA HTML，D3/CONTRACT §1）。
func (s *V2Supervisor) awaitReady(ctx context.Context, port int, password string) bool {
	deadline := time.Now().Add(s.deps.ReadyTimeout)
	for {
		if s.verifyReady(ctx, port, password) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(s.deps.PollInterval):
		}
	}
}

// verifyReady 单次就绪探测（envelope 可解析 = ListSessions 成功；client 经
// newClient 工厂——测试覆写 baseURL 注入）。
func (s *V2Supervisor) verifyReady(ctx context.Context, port int, password string) bool {
	c := s.newClient(port, password, Options{OpTimeout: s.deps.PollInterval * 2})
	_, err := c.ListSessions(ctx, "/", 1)
	return err == nil
}

// serverClient 构造指向已确保 server 的 Client。
func (s *V2Supervisor) serverClient(ctx context.Context) (*Client, int, string, error) {
	port, password, err := s.ensureServer(ctx)
	if err != nil {
		return nil, 0, "", err
	}
	return s.newClient(port, password, Options{}), port, password, nil
}

// --- RuntimeSupervisor 四方法（design D3 语义映射） ---

// lifecycleBinding 组装生命周期 binding（origin created/adopted；TerminalRef 从
// 任务身份确定性派生；envelope 结构校验由调用方 domain Validate 在 Encode 承担）。
func lifecycleBinding(taskID, directory, upstreamID string, port int, origin bindingOrigin) (domain.TaskBinding, error) {
	data, err := encodeV2BindingData(v2BindingData{
		Port: port, Directory: directory, TaskID: taskID, Origin: origin,
		Auth: v2BindingAuth{Kind: "derived"},
	})
	if err != nil {
		return domain.TaskBinding{}, appagent.NewError("supervisor.binding", appagent.ErrKindBindingInvalid, err)
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  upstreamID,
		TerminalRef:        v2TerminalRef(taskID),
		Data:               data,
	}, nil
}

// applySessionEnvAt 对指定 upstream session 应用环境（5.4 配置应用链：PUT
// /api/session/{id}/environment 204 替换式——1.3 冻结结论；应用完成确认先于
// 就绪提交与首个 prompt——本调用在返回 binding 之前）。
func (s *V2Supervisor) applySessionEnvAt(ctx context.Context, c *Client, upstreamID, taskID string, recovery bool) error {
	debugf("apply_env: task=%s session=%s recovery=%t", taskID, upstreamID, recovery)
	if s.deps.Env == nil {
		return appagent.NewError("supervisor.apply_env", appagent.ErrKindCapabilityUnavailable,
			errors.New("任务环境事实 seam 未注入（配置应用链不可用）"))
	}
	vars, err := s.deps.Env.ResolveSessionEnv(ctx, taskID, recovery)
	if err != nil {
		return appagent.NewError("supervisor.apply_env", appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("resolve session env: %w", err))
	}
	if err := c.PutSessionEnvironment(ctx, upstreamID, vars); err != nil {
		return appagent.NewError("supervisor.apply_env", appagent.ErrKindRuntimeUnavailable, err)
	}
	return nil
}

// Acquire 仅限首次路径（v2 钉扎新任务 / D5-B 承接首建）：幂等确保全局 server →
// POST /api/session 建 session（location.directory）→ 校验 ^ses → 应用环境 →
// 返回 binding（origin=created）。配置应用失败 → 不返回 binding（保留旧事实，
// 零持久化——binding 提交协议）。
func (s *V2Supervisor) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	debugf("acquire: task=%s dir=%s", task.TaskID, task.Directory)
	c, port, _, err := s.serverClient(ctx)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	created, aerr := s.acquireCreateSession(ctx, c, task.Directory)
	if aerr != nil {
		return domain.TaskBinding{}, aerr
	}
	debugf("acquire: created candidate session=%s port=%d", created.ID, port)
	// 首建补偿（D3 受限回收例外）：POST 已成功后的 attempt 失败——本次新建且
	// 未提交的 candidate session 经 SessionControl.Delete 回收（candidate 所有权
	// 不出本 attempt：失败路径不返回 session id，回收只能在此收口）。
	binding, aerr := s.finishAcquire(ctx, c, created.ID, task, port)
	if aerr != nil {
		// J3：回收失败（typed 清理债务）Join 原失败向编排层传播——task 层按
		// task 持久化清理义务（门禁挡再次首建）。
		if derr := s.recycleCandidate(ctx, c, created.ID); derr != nil {
			aerr = errors.Join(aerr, derr)
		}
		return domain.TaskBinding{}, aerr
	}
	return binding, nil
}

// acquireCreateSession 是首建 POST 的三分收口（收尾 Round1 K1）：
//   - 确定未创建（请求未发出/HTTP 非 2xx）→ 普通 typed 错误（编排层可清占位）；
//   - 响应已可靠取得 session id（即使后续解码/形状失败）→ 转入该 ID 的受限回收；
//     回收成功 = 已确认清理；回收失败 → CandidateCleanupDebt 债务链；
//   - 交付结果未知且无 id → CandidateCreateUnknown（编排层 MUST 保留占位阻断）。
func (s *V2Supervisor) acquireCreateSession(ctx context.Context, c *Client, dir string) (wireSession, error) {
	created, idHint, err := c.createSession(ctx, dir)
	if err == nil {
		return created, nil
	}
	var unknown *createResultUnknownError
	if !errors.As(err, &unknown) {
		// 服务端明确拒绝/请求未发出：确定未创建（G3-F1 连接类失败清就绪态）。
		if isTransportErr(err) {
			s.invalidateServer()
		}
		return wireSession{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindRuntimeUnavailable, err)
	}
	s.invalidateServer() // 响应不可达/截断：server 可能已死，清就绪态
	if idHint != "" {
		// 响应中已可靠取得 ID：按该 ID 走受限回收/债务链，MUST NOT 判未创建。
		if derr := s.recycleCandidate(ctx, c, idHint); derr != nil {
			return wireSession{}, errors.Join(unknown, derr)
		}
		return wireSession{}, errors.Join(unknown,
			&appagent.CandidateCleanupDebt{SessionID: idHint, Err: errors.New("create 响应解码失败但已确认回收")})
	}
	return wireSession{}, errors.Join(unknown, &appagent.CandidateCreateUnknown{Err: unknown})
}

// finishAcquire POST 成功后的 attempt 收尾：契约发现校验（2.5）+ 配置应用链
// （5.4，完成确认先于返回 binding）+ 组装 binding（origin=created）。
func (s *V2Supervisor) finishAcquire(ctx context.Context, c *Client, createdID string, task appagent.TaskRef, port int) (domain.TaskBinding, error) {
	contract, err := s.contractOf(ctx, c)
	if err != nil {
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindCapabilityUnavailable, err)
	}
	if !contract.ValidSessionID(createdID) {
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("session id %q 不匹配运行时发现的 pattern %q", createdID, contract.SessionIDPatternRaw))
	}
	// 配置应用链（5.4）：首次路径按当前值应用，完成确认先于返回 binding。
	if err := s.applySessionEnvAt(ctx, c, createdID, task.TaskID, false); err != nil {
		return domain.TaskBinding{}, err
	}
	debugf("acquire: finish task=%s session=%s origin=created", task.TaskID, createdID)
	return lifecycleBinding(task.TaskID, task.Directory, createdID, port, originCreated)
}

// noteCandidateDebt 记录 candidate 回收清理债务（last_error 位，含残留 session
// id——不误判「从未建立会话」）。
func (s *V2Supervisor) noteCandidateDebt(ctx context.Context, candidateID string, derr error) {
	_ = s.deps.Store.SetProviderRuntimeError(ctx, ProviderID, ProviderInstanceID,
		fmt.Sprintf("首建补偿回收 candidate session %s 失败（残留事实，非「从未建立」）: %v", candidateID, derr))
}

// recycleCandidate 首建失败补偿的受限回收（D3）：独立有界 ctx（调用方 ctx 已
// 取消不得跳过回收）；DELETE 失败返回 CandidateCleanupDebt（含 candidate id）
// 由 Acquire Join 传播——task 层按 task 持久化清理义务（门禁挡再次首建）。
// 实例 last_error 仍记录（诊断位），但不再是无声的终点。
const candidateRecycleTimeout = 30 * time.Second

func (s *V2Supervisor) recycleCandidate(ctx context.Context, c *Client, candidateID string) error {
	rcCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), candidateRecycleTimeout)
	defer cancel()
	if derr := c.DeleteSession(rcCtx, candidateID); derr != nil {
		// 白名单字段（status/kind）：原始错误文本含响应 body（服务器可能回显
		// 请求敏感信息），不进 debug 日志；debug 关闭 = 零求值零输出
		//（debugHTTPStatus/KindOf 是函数调用，MUST NOT 在 debugf 参数位无条件求值）。
		if debugEnabled() {
			debugf("recycle_candidate: DELETE failed session=%s status=%d kind=%s", candidateID, debugHTTPStatus(derr), appagent.KindOf(derr))
		}
		s.noteCandidateDebt(rcCtx, candidateID, derr)
		return &appagent.CandidateCleanupDebt{SessionID: candidateID, Err: derr}
	}
	debugf("recycle_candidate: session=%s recycled", candidateID)
	return nil
}

// ReapCandidateSession 实现 CandidateDebtReaper（收尾 R1 J3）：按 task/session
// 身份重新执行受限回收（DELETE）——清理债务重放收割（candidate 建于共享 serve，
// 经实例记录端点访问）。
func (s *V2Supervisor) ReapCandidateSession(ctx context.Context, taskID, sessionID string) error {
	_ = taskID // session 身份即收割凭据（candidate 建于本实例共享 serve）
	c, _, _, err := s.serverClient(ctx)
	if err != nil {
		return appagent.NewError("supervisor.reap_candidate", appagent.ErrKindRuntimeUnavailable, err)
	}
	if derr := c.DeleteSession(ctx, sessionID); derr != nil {
		// Round2 J7：受限 candidate 清理语义内，404/NotFound 确认目标已不存在
		//（首轮 DELETE 已生效但响应丢失/notice 清除未落库）→ 完成清债，不再
		// 保留债务阻断 Activate。通用 API 的 404 分类不变。
		if errors.Is(derr, ErrSessionNotFound) || isNotFound(derr) {
			return nil
		}
		return appagent.NewError("supervisor.reap_candidate", appagent.ErrKindRuntimeUnavailable, derr)
	}
	return nil
}

// RecycleCandidateSession 是任务层首建失败补偿的 candidate 回收入口（D3 受限
// 回收例外；application CandidateSessionRecycler 窄接口）：删除本次 attempt 新建
// 且未提交的上游 session（以 binding upstream 为准）；DELETE 失败记实例清理债务
// 并返回错误供调用方聚合（不掩盖原失败）。无 upstream（candidate 未返回/已回收）
// 幂等成功。
func (s *V2Supervisor) RecycleCandidateSession(ctx context.Context, b domain.TaskBinding) error {
	if b.UpstreamSessionID == "" {
		return nil
	}
	data, derr := decodeV2Binding(b)
	if derr != nil {
		return appagent.NewError("supervisor.recycle_candidate", appagent.ErrKindBindingInvalid, derr)
	}
	password, perr := derivePassword()
	if perr != nil {
		return appagent.NewError("supervisor.recycle_candidate", appagent.ErrKindBindingInvalid, perr)
	}
	c := s.newClient(data.Port, password, Options{})
	if derr := c.DeleteSession(ctx, b.UpstreamSessionID); derr != nil {
		s.noteCandidateDebt(ctx, b.UpstreamSessionID, derr)
		return appagent.NewError("supervisor.recycle_candidate", appagent.ErrKindRuntimeUnavailable, derr)
	}
	return nil
}

// Resume 恢复/承接延续（R1 恢复 + D5-A 承接 A 共用入口）：server 存活检查 →
// 按 binding 的 upstream session id GET 验证可读 → 仅恢复该 session。
// session 缺失/不可读 → ErrKindNotFound（停止并提交用户决策——ResumePolicy 任何
// 取值 MUST NOT 授权创建替代 session）。origin 识别：binding data origin=adopted
// → 禁止覆盖环境（承接保护）；created → 同代快照应用（recovery=true）。
func (s *V2Supervisor) Resume(ctx context.Context, task appagent.TaskRef, old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	// G3-F5：结构/归属校验先于任何网络/进程副作用（ensureServer/凭据派生之前）。
	//   ①upstream 非空（恢复不新建替代）；
	//   ②v2 命名空间 old：envelope 私有 data 结构校验（decodeV2Binding fail-closed）
	//     + binding directory 必须等于任务 worktree（归属结构校验）。
	// ③端口与实例记录一致性校验在 ensureServer 之后（需实例记录；仍先于业务 GET）。
	if old.UpstreamSessionID == "" {
		return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
			appagent.ErrKindBindingInvalid, errors.New("old binding 缺少 upstream session id（恢复不新建替代 session）"))
	}
	if old.ProviderID == ProviderID && old.ProviderInstanceID == ProviderInstanceID {
		data, derr := decodeV2Binding(old)
		if derr != nil {
			return domain.TaskBinding{}, derr // 结构校验（envelope/私有 data）fail-closed
		}
		if data.Directory != task.Directory {
			return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
				appagent.ErrKindBindingInvalid,
				fmt.Errorf("binding directory %q 与任务 worktree %q 不一致（归属结构校验）", data.Directory, task.Directory))
		}
	}
	c, port, _, err := s.serverClient(ctx)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	if policy.BeforeLaunch != nil {
		// v2 的 BeforeLaunch 仅取消/继续条件检查（不含扣款——D4 替换表；
		// 闭包语义由 task 层注入实现）。
		if err := policy.BeforeLaunch(ctx); err != nil {
			return domain.TaskBinding{}, err
		}
	}
	sess, err := c.GetSession(ctx, old.UpstreamSessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || isNotFound(err) {
			return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
				appagent.ErrKindNotFound,
				fmt.Errorf("upstream session %s 缺失（停止并提交用户决策；不创建替代 session）", old.UpstreamSessionID))
		}
		if isTransportErr(err) {
			s.invalidateServer() // G3-F1：连接类失败清就绪态
		}
		return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
			appagent.ErrKindRuntimeUnavailable, err)
	}
	// 归属一致性：session 的 location.directory 必须等于任务 worktree。
	if sess.Location.Directory != task.Directory {
		return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
			appagent.ErrKindBindingInvalid,
			fmt.Errorf("session %s 归属不符：location.directory=%q != %q",
				old.UpstreamSessionID, sess.Location.Directory, task.Directory))
	}
	// origin 解析：old 为 v2 binding 时按其 data.origin；v1 binding（D5-A 承接
	// A 延续）按 adopted（原 session 非 ocdeck 创建——保护规则持续有效）。
	origin := originAdopted
	if data, derr := decodeV2Binding(old); derr == nil {
		origin = data.Origin
		// 全部 v2 binding 的 port MUST 恒等于实例记录端口（D3；不一致 fail-closed）。
		if data.Port != port {
			return domain.TaskBinding{}, appagent.NewError("supervisor.resume",
				appagent.ErrKindBindingInvalid,
				fmt.Errorf("binding port %d 与实例记录端口 %d 不一致（fail-closed）", data.Port, port))
		}
	}
	// 配置应用链：adopted 持续禁止覆盖；created 恢复按同代快照。
	if origin != originAdopted {
		if err := s.applySessionEnvAt(ctx, c, old.UpstreamSessionID, task.TaskID, true); err != nil {
			return domain.TaskBinding{}, err
		}
	}
	debugf("resume: task=%s session=%s origin=%s port=%d", task.TaskID, old.UpstreamSessionID, origin, port)
	return lifecycleBinding(task.TaskID, task.Directory, old.UpstreamSessionID, port, origin)
}

// Release 挂起/删除的运行关联回收：挂起路径先经冻结的 interrupt 停止执行
// （200 {interrupted:bool}；停止失败/超时如实报错——调用方收敛回 active，
// MUST NOT 提交 suspended）；ephemeral binding 仅回收连接（不调用 abort/delete，
// server 不动）。一切路径不删除 session。
func (s *V2Supervisor) Release(ctx context.Context, b domain.TaskBinding) error {
	data, derr := decodeV2Binding(b)
	if derr != nil {
		// ephemeral 形态：独立结构校验（MUST NOT 走 envelope Validate / 持久化解码）。
		var ed v2BindingData
		if uerr := json.Unmarshal(b.Data, &ed); uerr != nil {
			return derr
		}
		if verr := validateEphemeralBindingData(ed); verr != nil {
			return derr
		}
		return nil // ephemeral：仅回收连接（无中断、无删除、server 不动）
	}
	if b.UpstreamSessionID == "" {
		return nil // 无上游 session（B 首建失败补偿已回收）：幂等成功
	}
	password, err := derivePassword()
	if err != nil {
		return appagent.NewError("supervisor.release", appagent.ErrKindBindingInvalid, err)
	}
	c := s.newClient(data.Port, password, Options{})
	return c.InterruptSession(ctx, b.UpstreamSessionID)
}

// AcquireEphemeral 删除清理专用：确保 server 在跑 + 获得临时连接；MUST NOT 创建
// session；返回仅内存临时 binding（origin=ephemeral、TerminalRef="none"、
// UpstreamSessionID 空——调用方 MUST NOT Encode/持久化）。
func (s *V2Supervisor) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	port, _, err := s.ensureServer(ctx)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	ed := v2BindingData{
		Port: port, Directory: task.Directory, TaskID: task.TaskID,
		Origin: originEphemeral, Auth: v2BindingAuth{Kind: "derived"},
	}
	if err := validateEphemeralBindingData(ed); err != nil {
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire_ephemeral",
			appagent.ErrKindBindingInvalid, err)
	}
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  "",
		TerminalRef:        EphemeralTerminalRef,
		Data:               mustMarshal(ed),
	}, nil
}

// CheckServerAlive 暴露就绪/重拉（5.3：崩溃自动重拉——任何业务入口前调用）。
// 重拉成功的通知收口在 ensureServer 的真实恢复状态转换（I9 唯一 owner——本方法
// 不再另行通知；监控循环自愈亦经同一 ensureServer）。
func (s *V2Supervisor) CheckServerAlive(ctx context.Context) error {
	// G3-F1：ready 时仍执行轻量进程/就绪验证——失败即清除就绪态进恢复矩阵
	//（ensureServer 快速路径已含同一验活；此处显式触发重拉语义）。
	s.mu.Lock()
	if s.ready && !s.verifyAliveLocked() {
		s.ready = false
		s.pid = 0
	}
	s.mu.Unlock()
	_, _, err := s.ensureServer(ctx)
	return err
}

// ShutdownServer 关闭全局共享 server（supervisor 单 owner——design D4 shutdown
// 行：kill_on_start/kill_immediate 策略下由 task 层关停编排调用一次；persist
// 策略保留 server）。G3-F1/F7：与 ensureServer 同一状态锁——等在进行中的
// ensure 完成后执行；成功后状态复位（ready=false/port=0/pid=0/password 空）并
// 置 shuttingDown（拒绝新业务 ensure）。幂等：单元不存在即成功。
func (s *V2Supervisor) ShutdownServer(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	// I9：关停停止监控——close(monitorStop) 幂等收口；cancel+join 三态语义
	// 统一经 StopServerMonitoring（锁外执行——join 与循环体互斥需要锁让位）。
	s.monitorStopOnce.Do(func() { close(s.monitorStop) })
	s.ready = false
	s.port = 0
	s.pid = 0
	s.password = ""
	_, err := s.deps.Proc.KillSession(ServeSessionName)
	s.mu.Unlock()
	s.StopServerMonitoring()
	if err != nil && errors.Is(err, process.ErrNoTmuxServer) {
		return nil // 无 tmux server：本就为空
	}
	return err
}

// EphemeralRuntimeEndpoint 解析仅内存临时 binding 的端点（SPI 窄接口
// EphemeralEndpointResolver 实现；删除清理消费——临时连接凭据现场派生）。
func (s *V2Supervisor) EphemeralRuntimeEndpoint(b domain.TaskBinding) (int, string, string, error) {
	var ed v2BindingData
	if err := json.Unmarshal(b.Data, &ed); err != nil {
		return 0, "", "", appagent.NewError("resolve ephemeral", appagent.ErrKindBindingInvalid, err)
	}
	if err := validateEphemeralBindingData(ed); err != nil {
		return 0, "", "", appagent.NewError("resolve ephemeral", appagent.ErrKindBindingInvalid, err)
	}
	password, err := derivePassword()
	if err != nil {
		return 0, "", "", appagent.NewError("resolve ephemeral", appagent.ErrKindBindingInvalid, err)
	}
	return ed.Port, password, ed.Directory, nil
}
