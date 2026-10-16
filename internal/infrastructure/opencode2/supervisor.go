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
	return &V2Supervisor{deps: deps, newClient: NewClient}
}

// --- ensureServer：六步写序 + 重启恢复矩阵 ---

// ensureServer 幂等确保全局 server 在跑，返回 (port, 派生密码)。单飞互斥；
// 实现启动顺序六步与 D3 重启恢复矩阵；启动按 deps.StartAttempts 独立有界重试
// （不扣任何任务 permit），耗尽 fail-closed。
func (s *V2Supervisor) ensureServer(ctx context.Context) (int, string, error) {
	password, err := derivePassword()
	if err != nil {
		return 0, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return 0, "", appagent.NewError("supervisor.ensure_server",
			appagent.ErrKindRuntimeUnavailable, errShuttingDown)
	}
	if s.ready {
		// G3-F1：快速路径轻量验活（进程存活 + 身份匹配）——失效即清除进入恢复矩阵，
		// 杜绝「ready 恒真」拿到失效端口/凭据。
		if s.verifyAliveLocked() {
			return s.port, password, nil
		}
		s.ready = false
		s.pid = 0
	}
	var lastErr error
	for attempt := 1; attempt <= s.deps.StartAttempts; attempt++ {
		port, err := s.ensureServerAttempt(ctx, password)
		if err == nil {
			s.ready = true
			s.port = port
			s.password = password
			return port, password, nil
		}
		s.pid = 0
		if errors.Is(err, errRuntimeIdentityMismatch) {
			// 身份/认证/协议不符：fail-closed，MUST NOT 仅凭端口响应接管
			//（D3 恢复矩阵；重试不改变事实——立即返回可判定错误）。
			return 0, "", err
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-time.After(time.Duration(attempt) * s.deps.PollInterval):
		}
	}
	return 0, "", fmt.Errorf("opencode2: server 启动重试耗尽（%d 次）: %w", s.deps.StartAttempts, lastErr)
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
		return rec.Port, nil
	}
	// 分支「pid NULL 且托管单元未发现存活 serve」：同端口从步骤②继续；
	// 端口被占用 → fail-closed（端口固定不轮换）。
	return s.startServer(ctx, rec.Port, password)
}

// startServer 启动顺序②③④⑤：spawn serve（OPENCODE_SERVER_PASSWORD 注入，
// 仅 runtime 级事实）→ 取 pid → 验证 → 落 pid。
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
		Env: map[string]string{
			"OPENCODE_SERVER_PASSWORD": password,
		},
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
	c, port, _, err := s.serverClient(ctx)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	created, err := c.CreateSession(ctx, task.Directory)
	if err != nil {
		if isTransportErr(err) {
			s.invalidateServer() // G3-F1：连接类失败 = server 可能已死，清就绪态
		}
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindRuntimeUnavailable, err)
	}
	// 契约发现校验（2.5；失败 → 本次 attempt 失败，candidate session 由 task 层
	// 首建补偿受限回收）。
	contract, err := s.contractOf(ctx, c)
	if err != nil {
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindCapabilityUnavailable, err)
	}
	if !contract.ValidSessionID(created.ID) {
		return domain.TaskBinding{}, appagent.NewError("supervisor.acquire",
			appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("session id %q 不匹配运行时发现的 pattern %q", created.ID, contract.SessionIDPatternRaw))
	}
	// 配置应用链（5.4）：首次路径按当前值应用，完成确认先于返回 binding。
	if err := s.applySessionEnvAt(ctx, c, created.ID, task.TaskID, false); err != nil {
		return domain.TaskBinding{}, err
	}
	return lifecycleBinding(task.TaskID, task.Directory, created.ID, port, originCreated)
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

// CheckServerAlive 暴露就绪/重拉（5.3：崩溃自动重拉——任何业务入口前调用；
// 监控循环亦经此触发）。重拉成功（原 ready=false → 本调用拉起）时触发
// OnServerRestarted（活跃任务按 binding 重建 session 关联）。
func (s *V2Supervisor) CheckServerAlive(ctx context.Context) error {
	// G3-F1：ready 时仍执行轻量进程/就绪验证——失败即清除就绪态进恢复矩阵
	//（ensureServer 快速路径已含同一验活；此处显式触发重拉语义）。
	s.mu.Lock()
	wasReady := s.ready
	if wasReady && !s.verifyAliveLocked() {
		s.ready = false
		s.pid = 0
		wasReady = false
	}
	s.mu.Unlock()
	if _, _, err := s.ensureServer(ctx); err != nil {
		return err
	}
	if !wasReady && s.deps.OnServerRestarted != nil {
		s.deps.OnServerRestarted()
	}
	return nil
}

// ShutdownServer 关闭全局共享 server（supervisor 单 owner——design D4 shutdown
// 行：kill_on_start/kill_immediate 策略下由 task 层关停编排调用一次；persist
// 策略保留 server）。G3-F1/F7：与 ensureServer 同一状态锁——等在进行中的
// ensure 完成后执行；成功后状态复位（ready=false/port=0/pid=0/password 空）并
// 置 shuttingDown（拒绝新业务 ensure）。幂等：单元不存在即成功。
func (s *V2Supervisor) ShutdownServer(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	s.ready = false
	s.port = 0
	s.pid = 0
	s.password = ""
	_, err := s.deps.Proc.KillSession(ServeSessionName)
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
