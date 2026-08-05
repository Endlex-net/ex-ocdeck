package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ocdeck/internal/config"
	"ocdeck/internal/lifecycle"
	"ocdeck/internal/opencode"
	"ocdeck/internal/process"
)

// TaskStore 抽象 TaskManager 所需的 store 能力（design.md §8/§18）。
// 方法签名对齐 internal/store.Queries，便于 *store.DB 直接实现（adapter 在 api 层）。
type TaskStore interface {
	// 项目
	GetProject(ctx context.Context, id string) (ProjectRow, error)
	// 任务
	CreateTask(ctx context.Context, t TaskRow) error
	GetTask(ctx context.Context, id string) (TaskRow, error)
	ListTasksByProject(ctx context.Context, projectID string) ([]TaskRow, error)
	ListAllTasks(ctx context.Context) ([]TaskRow, error)
	UpdateTaskStatus(ctx context.Context, id, status string, lastError sql.NullString) error
	UpdateTaskStatusConditional(ctx context.Context, id, fromStatus, toStatus string, lastError sql.NullString) (bool, error)
	UpdateTaskEnvSnapshot(ctx context.Context, id string, envSnapshot sql.NullString) error
	UpdateTaskLastPort(ctx context.Context, id string, port int) error
	UpdateTaskNotice(ctx context.Context, id string, notice sql.NullString) error
	UpdateTaskNoticeCAS(ctx context.Context, id string, expected, newNotice sql.NullString) (bool, error)
	SetTaskDeleteMode(ctx context.Context, id, mode string) error
	BeginDeleteIntent(ctx context.Context, id, mode string, fromStatuses []string) (bool, error)
	ArchiveTask(ctx context.Context, id string) error
	RestoreTask(ctx context.Context, id string) error
	DeleteTask(ctx context.Context, id string) error
	// 生命周期配置（design.md §2.1）
	GetLifecycleConfig(ctx context.Context, projectID string) (LifecycleConfigRow, error)
	UpsertLifecycleConfig(ctx context.Context, projectID, inheritPatterns, initScript, preDeleteScript string) error
	// init_status CAS（design.md §2.1/§3）
	CommitCreated(ctx context.Context, taskID, expectedStatus, initStatus string) (bool, error)
	ClaimInitRun(ctx context.Context, taskID string) (bool, error)
	ClaimInitRerun(ctx context.Context, taskID string) (bool, error)
	FinishInitRun(ctx context.Context, taskID, status string, initError sql.NullString) (bool, error)
	ConvergeInterruptedInitRuns(ctx context.Context) (int64, error)
	// env
	ListGlobalEnvVars(ctx context.Context) ([]GlobalEnvVarRow, error)
	ListProjectEnvVars(ctx context.Context, projectID string) ([]EnvVarRow, error)
	ListTaskEnvVars(ctx context.Context, taskID string) ([]EnvVarRow, error)
	// sessions
	UpsertTaskSession(ctx context.Context, s SessionRow) error
	ListTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error)
	// ListTopLevelTaskSessions 列出顶层会话（parent_id 为空），供锚定候选取最近顶层 session
	//（design.md §4 锚定隔离 background subagent 子会话）。
	ListTopLevelTaskSessions(ctx context.Context, taskID string) ([]SessionRow, error)
	DeleteTaskSession(ctx context.Context, taskID, sessionID string) error
	AlignSessions(ctx context.Context, taskID string, sessions []SessionRow, complete bool, noticeFn func(sql.NullString) sql.NullString) error
}

// CleanupDebtStore 持久化未收敛的 orphan cleanup tickets（design.md §10）。
// 可选依赖：未注入时 orphan 持久化降级为仅内存（进程退出即丢失），注入后跨重启恢复重试。
// 拆为独立接口而非并入 TaskStore，避免强制所有 mock 实现该方法（fb-05：不为单一场景
// 强加宽接口）。
type CleanupDebtStore interface {
	UpsertCleanupDebt(ctx context.Context, sessionName, ticketsJSON string, createdAt int64) error
	DeleteCleanupDebt(ctx context.Context, sessionName string) error
	ListCleanupDebts(ctx context.Context) ([]CleanupDebtRow, error)
}

// CleanupDebtRow cleanup_debts 表行映射（解耦 store 包结构）。
type CleanupDebtRow struct {
	SessionName string
	Tickets     string // JSON 编码 []string
	CreatedAt   int64
}

// ProjectRow / TaskRow / EnvVarRow / SessionRow 解耦 store 包结构（design.md §18）。
type ProjectRow struct {
	ID            string
	Name          string
	Path          string
	DefaultBranch string
	CreatedAt     int64
}

type TaskRow struct {
	ID           string
	ProjectID    string
	Name         string
	Branch       string
	Status       string
	WorktreePath string
	LastPort     sql.NullInt64
	LastError    sql.NullString
	Notice       sql.NullString
	DeleteMode   sql.NullString
	EnvSnapshot  sql.NullString
	CreatedAt    int64
	UpdatedAt    int64
	ArchivedAt   sql.NullInt64
	InitStatus   string
	InitError    sql.NullString
}

type EnvVarRow struct {
	Key   string
	Value string
}

// LifecycleConfigRow 项目生命周期配置行（design.md §2.1，解耦 store 包结构）。
// 缺行读取时三脚本字段为空串（无配置 = 空配置语义）。
type LifecycleConfigRow struct {
	ProjectID       string
	InheritPatterns string
	InitScript      string
	PreDeleteScript string
	UpdatedAt       int64
}

// GlobalEnvVarRow 全局级 env 变量行（解耦 store 包，design.md §8/§2）。
// Mode ∈ follow_host | manual；follow_host 时 Value 忽略，激活时从服务端进程 env 解析。
type GlobalEnvVarRow struct {
	Key   string
	Mode  string
	Value string
}

type SessionRow struct {
	TaskID           string
	SessionID        string
	SessionCreatedAt int64
	FirstSeenAt      int64
	LastSeenAt       int64
	// ParentID 非空表示 background subagent 子会话；空为顶层会话（design.md §4 锚定隔离）。
	ParentID string
}

// --- Manager ---

// Manager 是任务状态转换/进程/worktree 操作的唯一入口（design.md §1/§18）。
type Manager struct {
	cfg       *config.Config
	store     TaskStore
	proc      ProcessBackend
	wt        WorktreeBackend
	ocFactory OCClientFactory
	// namer 将任务名提炼为分支 slug（ai-worktree-naming）。nil 时 Create 回退到 Slugify
	//（构造期或测试未注入时的防御）。生产 wiring 在 main.go 注入 ai.SlugNamer（tasks 3.3）。
	namer BranchNamer
	// rand4Fn 生成 4 位 [a-z0-9] 随机串，供 newWorktreePath 碰撞重试。可测试注入以确定性
	// 构造碰撞/rand 失败场景。默认用 crypto/rand（Go 1.24 起 Read 永不返回 error，失败 fatal；
	// 故 rand4 的 error 分支为防御性保留，生产路径不可达）。
	rand4Fn func() (string, error)
	// probeColdStartBackoffFn 返回 capability probe 冷启动重试退避序列（design.md D8）。
	// 默认 defaultProbeColdStartBackoff（2s、4s）；测试可注入更短序列避免拖慢。
	// nil 防御回退默认值（直接 &Manager{} 构造的测试）。
	probeColdStartBackoffFn func() []time.Duration

	// keyedMu 提供每任务互斥锁（design.md §1：每任务 keyed mutex，冲突返回 409）。
	keyedMu sync.Map // taskID -> *keyedLock

	// runtime 维护活跃任务的运行时索引（RuntimeGroup），供回调隔离与退出监视取消。
	rtMu     sync.Mutex
	runtimes map[string]*taskRuntime // taskID -> runtime

	// lastGen 记录每任务最后使用的 generation（B4：runtime 清除后 generation 不得回 0，
	// Manager 侧单调递增持久持有）。即便 runtime 被 clearRuntime 移除，下次 newRuntime 也
	// 从 lastGen+1 续递增，保证回调三元组校验的 generation 在进程生命周期内不回卷。
	genMu   sync.Mutex
	lastGen map[string]int // taskID -> last used generation

	// lifeCtx 是 Manager 生命周期 context（design.md §4：SSE/退出监视挂 Manager 生命周期，
	// 非 HTTP request context）。由 SetLifecycleCtx 注入；nil 时回退 context.Background()。
	lifeCtx context.Context

	// portCursor 端口轮转游标（design.md §3：避免每次从头扫，降低并行 Activate 选同端口）。
	portCursorMu sync.Mutex
	portCursor   int

	// bgCancel 控制后台周期重试 goroutine（design.md §5）。
	bgCancel context.CancelFunc
	// bgDone 在后台周期 goroutine 退出时关闭，供 Shutdown join（H：关停等待后台收尾）。
	bgDone chan struct{}

	// orphanFailures 记录 reconcile 中 kill 失败的孤儿会话，供后台周期重试（B9，F3）。
	// 不仅记 session name，还聚合 kill 失败产生的 cleanupTickets，供 RetryReap 重入
	// （design.md §5：orphan 清理失败/收割时聚合 cleanup tickets 写入供后台 RetryReap）。
	orphanMu       sync.Mutex
	orphanFailures []orphanFailure

	// debtStore 持久化未收敛 orphan tickets 跨重启恢复（design.md §10）。nil 时降级为仅内存。
	debtStore CleanupDebtStore

	// shutdownGate 控制自动激活触发准入（B2）：Shutdown 开始后拒绝新自动激活触发。
	// shutdownGateMu 保护 shutdownStarted 与 autoActivateWG；autoActivateWG 登记所有
	// triggerActivate goroutine，供 Shutdown 等待自动激活收尾后再清理。
	shutdownGateMu  sync.Mutex
	shutdownStarted bool
	autoActivateWG  sync.WaitGroup

	// lifecycleRunner 提供 init/pre-delete 脚本执行与 inherit 文件复制（design.md §7.1）。
	// 为 nil 时跳过脚本执行与 inherit（无生命周期配置的旧路径行为）。
	lifecycleRunner lifecycle.LifecycleRunner
	// logDir 为生命周期脚本日志根目录（design.md §7.4：<dataDir>/logs）。
	logDir string

	// runnerCtx 是 InitRunner/pre-delete 脚本执行所用的独立 context（design.md §6.1）：
	// 不复用 SetLifecycleCtx 的 signal ctx，仅 Shutdown 关 gate 后取消。
	// runnerWG 登记全部 InitRunner 与 pre-delete 执行 goroutine，Shutdown 在关 gate 后
	// cancel runnerCtx 并 wait runnerWG，确保脚本执行在 store.Close 前收敛（§6.1）。
	runnerCtx    context.Context
	runnerCancel context.CancelFunc
	runnerWG     sync.WaitGroup
}

// orphanFailure 记录孤儿会话清理失败项（F3）：会话名 + kill 失败产生的 cleanup tickets。
type orphanFailure struct {
	sessionName string
	tickets     []string
}

// keyedLock 是每任务的互斥锁，支持 TryLock（普通操作冲突即 409）与 lockWait
// （仅 ReopenAttach 可等待并复查资源后幂等复用，design.md §1/§21）。
type keyedLock struct {
	mu sync.Mutex
}

// taskRuntime 维护单个活跃任务的运行时状态（design.md §2 RuntimeGroup）。
type taskRuntime struct {
	taskID       string
	generation   int                        // 激活代，回调校验用
	instanceID   string                     // 本代实例标识，回调三元组校验用（B4）
	groups       map[string]*runtimeGroup   // sessionName -> group
	sseCancel    context.CancelFunc         // SSE 订阅取消（阻塞式：cancel 并 join SSE goroutine）
	sseDone      chan struct{}              // SSE goroutine 退出信号（stopAll 时 join）
	watchCancels map[string]func()          // sessionName -> watch cancel
	watchDones   map[string]<-chan struct{} // sessionName -> watch goroutine 退出信号（join 用）
	mu           sync.Mutex
}

// runtimeGroup 对应 design.md §2 RuntimeGroup。
type runtimeGroup struct {
	Role        string // serve / tui / shell
	SessionName string
	Generation  int
	InstanceID  string
}

// registerGroup 写入注册表（B4：groups 真实写入，回调三元组校验依据）。
func (rt *taskRuntime) registerGroup(role, sessionName string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.groups[sessionName] = &runtimeGroup{
		Role: role, SessionName: sessionName,
		Generation: rt.generation, InstanceID: rt.instanceID,
	}
}

// removeGroup 从注册表移除（会话退出时）。
func (rt *taskRuntime) removeGroup(sessionName string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.groups, sessionName)
}

// Options 构造 Manager 的依赖注入。
type Options struct {
	Cfg       *config.Config
	Store     TaskStore
	Proc      ProcessBackend
	Worktree  WorktreeBackend
	OCFactory OCClientFactory
	// Namer 可选：注入后 Create 用其提炼分支 slug（ai-worktree-naming）。
	// 为 nil 时回退到本包 Slugify（构造期或测试未注入时防御）。
	Namer BranchNamer
	// DebtStore 可选：注入后未收敛 orphan tickets 持久化跨重启恢复（design.md §10）。
	DebtStore CleanupDebtStore
	// LifecycleRunner 可选：注入后启用 init/pre-delete 脚本与 inherit 文件继承
	//（design.md §7.1）。为 nil 时跳过脚本与 inherit（旧路径行为）。
	LifecycleRunner lifecycle.LifecycleRunner
	// LogDir 生命周期脚本日志根目录（design.md §7.4）。为空时回退 <cfg.DataDir>/logs。
	LogDir string
}

// New 构造 Manager。OCFactory 为 nil 时用默认 opencode.Client 工厂。
func New(opts Options) *Manager {
	m := &Manager{
		cfg:             opts.Cfg,
		store:           opts.Store,
		proc:            opts.Proc,
		wt:              opts.Worktree,
		ocFactory:       opts.OCFactory,
		namer:           opts.Namer,
		debtStore:       opts.DebtStore,
		lifecycleRunner: opts.LifecycleRunner,
		logDir:          opts.LogDir,
		runtimes:                make(map[string]*taskRuntime),
		lastGen:                 make(map[string]int),
		rand4Fn:                 rand4,
		probeColdStartBackoffFn: defaultProbeColdStartBackoff,
	}
	if m.ocFactory == nil {
		m.ocFactory = defaultOCFactory
	}
	if m.logDir == "" && m.cfg != nil {
		m.logDir = m.cfg.DataDir + "/logs"
	}
	// runnerCtx 独立于 SetLifecycleCtx 的 signal ctx（design.md §6.1）：
	// 仅 Shutdown 关 gate 后取消，脚本执行不受 HTTP 请求 ctx 或信号 ctx 提前取消影响。
	m.runnerCtx, m.runnerCancel = context.WithCancel(context.Background())
	return m
}

// SetLifecycleCtx 注入 Manager 生命周期 context（design.md §4）。
// SSE 订阅与退出监视挂此 context，而非 HTTP request context，保证请求结束后 SSE 仍存活。
// 应在构造后、首次 Activate/Reconcile 前调用；幂等（覆盖旧值并取消旧 ctx 不影响已建 SSE）。
func (m *Manager) SetLifecycleCtx(ctx context.Context) {
	m.lifeCtx = ctx
}

// lifecycleCtx 返回 Manager 生命周期 context（未注入回退 context.Background()）。
func (m *Manager) lifecycleCtx() context.Context {
	if m.lifeCtx != nil {
		return m.lifeCtx
	}
	return context.Background()
}

// defaultOCFactory 用 opencode.NewClient 构造 OCClient。
func defaultOCFactory(port int, password string, opts opencode.Options) OCClient {
	return opencode.NewClient(port, password, opts)
}

// tryLockTask 尝试获取任务级互斥锁，不阻塞。成功返回 unlock，失败返回 nil + 409 错误。
// 用于普通状态操作（Create/Activate/Suspend/Delete/Retry/Archive/Restore/CreateShell/CloseShell）。
func (m *Manager) tryLockTask(taskID string) (func(), error) {
	v, _ := m.keyedMu.LoadOrStore(taskID, &keyedLock{})
	kl := v.(*keyedLock)
	if !kl.mu.TryLock() {
		return nil, newOpErr(codeConflict, fmt.Errorf("task %s is busy (another operation in progress)", taskID))
	}
	return func() { kl.mu.Unlock() }, nil
}

// lockTaskWait 等待获取任务级互斥锁，感知 ctx 取消与内部 deadline。仅 ReopenAttach 使用：
// 可等待并复查资源后幂等复用（design.md §21：并发 REST/WS 重开幂等复用同一新 TUI 会话）。
// taskBusy 机制：
//   - 内部 deadline（30s）：超时返回 409 conflict "任务忙，请稍后重试"，不得无限等待；
//   - 拿锁后 MUST 复查任务状态：等待期间任务可能被挂起/删除，非 active → invalid_state
//     （释放锁后返回，不执行副作用）；
//   - 现有 ctx 取消感知保留；ReopenAttach 幂等复用语义不变。
//
// waiter 泄漏修复（R7 TOCTOU）：goroutine 拿锁后通过 select 把锁所有权交给调用方或
// 在 waitCtx 取消时释放锁——发送与接收同步（unbuffered channel），不存在"检查 abandoned
// 后写入无人接收的缓冲 → 锁永不释放"窗口。调用方放弃（waitCtx.Done）时 goroutine 的
// select 必走 waitCtx.Done 分支释放锁，锁最终必然可被后续操作获取。
//
// 返回 unlock；ctx 取消返回 ctx.Err()（包装为 conflict）；deadline 超时返回 conflict；
// 拿锁后状态非 active 返回 invalid_state（已释放锁）。
func (m *Manager) lockTaskWait(ctx context.Context, taskID string) (func(), error) {
	v, _ := m.keyedMu.LoadOrStore(taskID, &keyedLock{})
	kl := v.(*keyedLock)
	// TryLock 快路径：无冲突直接返回。
	if kl.mu.TryLock() {
		return m.recheckActiveOrUnlock(ctx, taskID, kl)
	}
	// 等待路径：unbuffered channel + waitCtx 驱动的锁所有权移交。
	waitCtx, waitCancel := context.WithTimeout(ctx, lockWaitDeadline)
	defer waitCancel()
	lockedCh := make(chan struct{}) // unbuffered：发送与接收同步，无 TOCTOU 窗口。
	go func() {
		kl.mu.Lock()
		select {
		case lockedCh <- struct{}{}:
			// 锁所有权已移交调用方（调用方已从 lockedCh 收到信号）。
		case <-waitCtx.Done():
			// 调用方已放弃（ctx 取消或 deadline 超时）：释放锁，避免锁永久占用。
			kl.mu.Unlock()
		}
	}()
	select {
	case <-lockedCh:
		return m.recheckActiveOrUnlock(ctx, taskID, kl)
	case <-waitCtx.Done():
		// ctx 取消或 deadline 超时：goroutine 会经 waitCtx.Done 释放锁。
		// 区分：ctx 自身取消 → 包装 ctx.Err()；deadline 超时 → errTaskBusy。
		if ctx.Err() != nil {
			return nil, newOpErr(codeConflict, ctx.Err())
		}
		return nil, errTaskBusy
	}
}

// recheckActiveOrUnlock 拿锁后复查任务状态：非 active 释放锁并返回 invalid_state。
// 等待期间任务可能被挂起/删除，拿锁后不得直接执行副作用。
func (m *Manager) recheckActiveOrUnlock(ctx context.Context, taskID string, kl *keyedLock) (func(), error) {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		kl.mu.Unlock()
		return nil, newOpErr(codeNotFound, fmt.Errorf("task not found: %w", err))
	}
	if row.Status != StatusActive {
		kl.mu.Unlock()
		return nil, newOpErr(codeInvalidState, fmt.Errorf("task %s no longer active (got %s) after lock wait", taskID, row.Status))
	}
	return func() { kl.mu.Unlock() }, nil
}

// errTaskBusy 表示 lockTaskWait 内部 deadline 超时（任务忙，请稍后重试）。
var errTaskBusy = newOpErr(codeConflict, fmt.Errorf("task busy, please retry later"))

// lockWaitDeadline 是 lockTaskWait 的内部等待上限（design.md taskBusy 机制）。
const lockWaitDeadline = 30 * time.Second

// convergeLockDeadline 是 convergeToSuspended 阻塞等锁上限（B4a：fatal runtime event MUST 必达，
// 串行收敛不得因锁忙丢事件，但不得无限阻塞 watcher goroutine）。
const convergeLockDeadline = 30 * time.Second

// lockTaskForConverge 为收敛路径阻塞等待任务锁（B4a：fatal runtime event MUST 必达，
// 不得因锁忙丢事件→留 active 无 SSE）。与 lockTaskWait 区别：不复查 active 状态
// （收敛语义是“无论当前状态都清理残留 + 落 suspended”，状态已由调用方/事件判定）。
// 内部 deadline（30s）防止锁持有者卡死时无限等待；超时回退为 tryLock（仍尽力收敛）。
// R7 TOCTOU 修复同 lockTaskWait：unbuffered channel + waitCtx 驱动的锁所有权移交，
// 调用方放弃时 goroutine 经 waitCtx.Done 释放锁，不存在锁永久占用窗口。
// 返回 unlock；超时且 tryLock 也失败返回 errTaskBusy（调用方尽力记录但不静默）。
func (m *Manager) lockTaskForConverge(taskID string) (func(), error) {
	v, _ := m.keyedMu.LoadOrStore(taskID, &keyedLock{})
	kl := v.(*keyedLock)
	if kl.mu.TryLock() {
		return func() { kl.mu.Unlock() }, nil
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), lockWaitDeadline)
	defer waitCancel()
	lockedCh := make(chan struct{}) // unbuffered：发送与接收同步，无 TOCTOU 窗口。
	go func() {
		kl.mu.Lock()
		select {
		case lockedCh <- struct{}{}:
			// 锁所有权已移交调用方。
		case <-waitCtx.Done():
			// 调用方已放弃：释放锁。
			kl.mu.Unlock()
		}
	}()
	select {
	case <-lockedCh:
		return func() { kl.mu.Unlock() }, nil
	case <-waitCtx.Done():
		// 超时回退：再试一次 tryLock（锁可能刚好释放），仍失败则放弃但不静默。
		if kl.mu.TryLock() {
			return func() { kl.mu.Unlock() }, nil
		}
		return nil, errTaskBusy
	}
}

// getRuntime 返回任务的运行时（不存在返回 nil）。
func (m *Manager) getRuntime(taskID string) *taskRuntime {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	return m.runtimes[taskID]
}

// setRuntime 设置任务的运行时。
func (m *Manager) setRuntime(taskID string, rt *taskRuntime) {
	m.rtMu.Lock()
	defer m.rtMu.Unlock()
	m.runtimes[taskID] = rt
}

// clearRuntime 移除任务的运行时并停止其 SSE/退出监视。
func (m *Manager) clearRuntime(taskID string) {
	m.rtMu.Lock()
	rt := m.runtimes[taskID]
	delete(m.runtimes, taskID)
	m.rtMu.Unlock()
	if rt != nil {
		rt.stopAll()
	}
}

// stopAll 停止该运行时的 SSE 订阅与退出监视（design.md §4 lifecycle 收敛）。
// 两阶段：先在 rt.mu 下捕获 cancel/done，释放锁后 cancel。
// SSE：cancel 并 join goroutine（sseCancel 阻塞式包装；SSE goroutine 不调用 stopAll，无自死锁）。
// watch：仅 cancel（非阻塞），不 join——因 stopAll 可能在某 watch 回调内被调用
// （handleServeExit → cleanupActivationRuntime → stopAll），join 自身 goroutine 会死锁。
// watch goroutine 的 join 由 stopAllJoin（Shutdown 路径）负责。
func (rt *taskRuntime) stopAll() {
	rt.mu.Lock()
	sseCancel := rt.sseCancel
	rt.sseCancel = nil
	cancels := make([]func(), 0, len(rt.watchCancels))
	for name, c := range rt.watchCancels {
		cancels = append(cancels, c)
		delete(rt.watchCancels, name)
		delete(rt.watchDones, name)
	}
	rt.mu.Unlock()

	if sseCancel != nil {
		sseCancel() // 阻塞式：cancel + join SSE goroutine
	}
	for _, c := range cancels {
		c() // 非阻塞
	}
}

// stopAllJoin 停止并 join 该运行时的全部 SSE/watch goroutine（design.md §4 lifecycle 收敛，G）。
// 仅在非 watch 回调上下文调用（Shutdown 路径）：join watch goroutine 不会自死锁。
func (rt *taskRuntime) stopAllJoin() {
	rt.mu.Lock()
	sseCancel := rt.sseCancel
	rt.sseCancel = nil
	sseDone := rt.sseDone
	rt.sseDone = nil
	type wd struct {
		cancel func()
		done   <-chan struct{}
	}
	watches := make([]wd, 0, len(rt.watchCancels))
	for name, c := range rt.watchCancels {
		watches = append(watches, wd{cancel: c, done: rt.watchDones[name]})
		delete(rt.watchCancels, name)
		delete(rt.watchDones, name)
	}
	rt.mu.Unlock()

	// SSE：cancel + join。
	if sseCancel != nil {
		sseCancel()
	} else if sseDone != nil {
		<-sseDone
	}
	// watch：cancel（非阻塞）+ join done 通道。
	for _, w := range watches {
		if w.cancel != nil {
			w.cancel()
		}
		if w.done != nil {
			<-w.done
		}
	}
}

// StartBackground 启动后台周期重试 goroutine（design.md §5：30s 周期消化 retryable notice）。
// 返回 stop 函数。幂等：重复调用返回新 stop，旧 goroutine 由其自身 ctx 控制。
// stop 仅 cancel context；goroutine 退出由 bgDone 通道表示，Shutdown 负责真正 join（H）。
func (m *Manager) StartBackground(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	m.bgCancel = cancel
	m.bgDone = make(chan struct{})
	done := m.bgDone
	go func() {
		defer close(done)
		m.backgroundLoop(ctx)
	}()
	return cancel
}

// backgroundLoop 周期处理 retryable notice 与孤儿会话重试（design.md §5）。
// 处理错误聚合记录（不静默吞错），后台周期循环自身不退出（除非 ctx 取消）。
func (m *Manager) backgroundLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.processRetryableNotices(ctx); err != nil {
				log.Printf("background: retryable notices: %v", err)
			}
			if err := m.retryOrphanSessions(ctx); err != nil {
				// B2：后台周期不阻塞，但 cleanup_debt 持久化错误 MUST 记录（不静默吞没）。
				log.Printf("background: retry orphan sessions: %v", err)
			}
		}
	}
}

// Shutdown 正常关停（design.md §10）：quiesce → 停后台周期并 join → 同步收尾残留 retryable debt
// → 清理任务会话（kill 模式）→ 停全部 runtime goroutine（join，防可写已关资源）。
// 关停顺序保持 §10：tm.Shutdown → bgStop → wd.Stop（调用方控制）。
//
// H：Shutdown MUST 等待后台 notice resolver 收尾。30s ticker 下残留 retryable debt
// 不得静默丢失：停后台周期后同步执行一次 processRetryableNotices（用传入 ctx），
// 错误聚合记录；仍残留的 debt 留 DB，下次启动 reconcile 处理。
//
// G：清停全部活跃 runtime 的 SSE/watch goroutine（stopAndJoinAllRuntimes），
// 避免 cancel 后 goroutine 仍写已关资源（store）。
func (m *Manager) Shutdown(ctx context.Context) error {
	// B2：准入 gate——禁止新自动激活触发，等待已登记自动激活 goroutine 收尾（有界超时），
	// 再执行既有清理。消灭窗口：kill 模式 shutdown 枚举后再建 tmux 会话；
	// persist 模式 Shutdown 返回后继续注册 runtime/访问 store。
	m.shutdownGateMu.Lock()
	m.shutdownStarted = true
	m.shutdownGateMu.Unlock()

	// §6.1 固定顺序：关 gate → cancel runnerCtx → wait runnerWG → 关 store。
	// runnerCtx cancel 紧随 gate 关闭，立即终止在跑脚本进程组，避免被持锁 pre-delete 拉长 Shutdown。
	// runnerWG wait 在 store.Close 前完成：脚本执行被终止后 InitRunner 用独立非取消 ctx 落账
	//（仍在 WG 内，Done 在最终状态写库之后）。
	if m.runnerCancel != nil {
		m.runnerCancel()
	}
	m.runnerWG.Wait()
	// 置空 runnerCtx/runnerCancel：Shutdown 后 Manager 若被复用（New 重建），
	// 旧 cancel 不与新 ctx 共存误杀复用后 runner 工作。复用时由 New 重建 runnerCtx。
	m.shutdownGateMu.Lock()
	m.runnerCtx = nil
	m.runnerCancel = nil
	m.shutdownGateMu.Unlock()

	// B2：等待在途自动激活结束（autoActivateWG 语义保持：triggerActivate goroutine 收尾）。
	// 有界超时：避免 Shutdown 无限阻塞；超时后仍继续清理（残留自动激活 goroutine 走 lifecycle ctx，
	// 由调用方在 Shutdown 后取消 lifecycle ctx 收尾）。
	waitDone := make(chan struct{})
	go func() {
		m.autoActivateWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
		log.Printf("shutdown: timed out waiting for in-flight auto-activate goroutines: %v", ctx.Err())
	}

	// quiesce：停后台周期重试，避免关停期间与清理竞态。
	if m.bgCancel != nil {
		m.bgCancel()
	}
	// join 后台周期 goroutine（H：等待后台收尾，不发起新周期）。
	if m.bgDone != nil {
		<-m.bgDone
		m.bgDone = nil
	}
	// 同步收尾残留 retryable debt（H：30s ticker 下可能未到周期点，关停时 MUST 主动跑一次，
	// 不静默丢失）。用传入 ctx（含关停超时）。
	if err := m.processRetryableNotices(ctx); err != nil {
		log.Printf("shutdown: final retryable notice sweep: %v", err)
	}
	// R7：收割内存 orphanFailures（kill 失败的孤儿会话 tickets，非仅 DB notice）。
	// 后台周期已停（bgCancel），关停时 MUST 主动调用一次 retryOrphanSessions，避免逃逸进程
	// tickets 仅存内存随进程退出丢失（design.md §10：runtime 已空 = 无会话且无可重试 cleanup debt，
	// 含 orphanFailures）。仍残留的 orphanFailure 计入 clean 判定。
	if err := m.retryOrphanSessions(ctx); err != nil {
		// B2：cleanup_debt 持久化错误 MUST 记录（不静默）；关停不因持久化错误中断停 goroutine。
		log.Printf("shutdown: retry orphan sessions: %v", err)
	}
	// kill 模式：按 shutdownPolicy 清理全部任务会话，确认 runtime 已空。
	policy := m.cfg.ShutdownPolicy
	var killErr error
	if policy == config.ShutdownKillOnStart || policy == config.ShutdownKillImmediate {
		killErr = m.shutdownKillAllSessions(ctx)
		if killErr != nil {
			// 记录但继续停 runtime goroutine，避免 goroutine 泄漏；错误向上传播。
			log.Printf("shutdown: kill all sessions: %v", killErr)
		}
	} else {
		// persist 模式：会话保留（tmux 持有），但 orphanFailures 非空意味着仍有未收割的逃逸进程
		// tickets，不得视为干净退出（design.md §10）。返回错误让调用方感知。
		m.orphanMu.Lock()
		orphanRemaining := len(m.orphanFailures)
		m.orphanMu.Unlock()
		if orphanRemaining > 0 {
			killErr = fmt.Errorf("shutdown: %d orphan cleanup tickets remain (persist mode)", orphanRemaining)
		}
	}
	// P4 复评阻塞 3c：shutdownKillAllSessions 新产生的 orphan tickets MUST 再次持久化
	//（顺序：先收割既有 → kill 全部会话 → 新产生的 orphan 再持久化，design.md §10）。
	// 不得先持久化再 kill——kill 产生的新 orphan 会落在仅内存随进程退出丢失。
	if err := m.persistOrphanDebts(ctx); err != nil {
		// B2：cleanup_debt 持久化错误 MUST 传播给 Shutdown 返回值（main 据此感知）。
		log.Printf("shutdown: persist orphan debts: %v", err)
		if killErr == nil {
			killErr = fmt.Errorf("shutdown: persist orphan debts: %w", err)
		} else {
			killErr = fmt.Errorf("%w; persist orphan debts: %v", killErr, err)
		}
	}
	// 停全部活跃 runtime 的 SSE/watch goroutine 并 join（G：防可写已关资源）。
	// persist 模式会话保留（tmux 持有），但 in-process 监视 goroutine 必须停。
	m.stopAndJoinAllRuntimes()
	return killErr
}

// stopAndJoinAllRuntimes 清停全部活跃 runtime 的 SSE/watch goroutine 并 join（G）。
// 不杀会话（kill 模式已由 shutdownKillAllSessions 处理；persist 模式保留会话）。
func (m *Manager) stopAndJoinAllRuntimes() {
	m.rtMu.Lock()
	rts := make([]*taskRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		rts = append(rts, rt)
	}
	m.runtimes = map[string]*taskRuntime{}
	m.rtMu.Unlock()
	// 逐 runtime stopAllJoin（cancel + join SSE/watch），避免持有 rtMu 时 join 与回调取 rtMu 死锁。
	for _, rt := range rts {
		rt.stopAllJoin()
	}
}

// shutdownKillAllSessions kill 全部 ocdeck 会话并确认 runtime 已空（design.md §10 kill 模式）。
// 确认空 = 无 tmux 会话且无可重试 cleanup debt。
// KillResult 的 disposition/tickets/error MUST 检查：残留（非 clean）MUST 持久化 tickets
// 为 notice（逃逸进程下次启动可定位，design.md §8/§10）并返回错误（runtime 未净）。
// 已知 task（sessionName 可解析 taskID 且有 DB 行）tickets 落该 task 的 DB notice；
// 无 DB 行的孤儿会话 tickets 退回内存 orphanFailures，供后台周期重试（F3）。
// kill 模式下有残留或 DB retryable debt 未清时返回非 nil，调用方 Shutdown 据此决定是否停 watchdog。
func (m *Manager) shutdownKillAllSessions(ctx context.Context) error {
	sessions, err := m.proc.ListSessions()
	if err != nil && !errors.Is(err, process.ErrNoTmuxServer) {
		return fmt.Errorf("shutdown: list sessions: %w", err)
	}
	killFailed := false
	for _, name := range sessions {
		res, kerr := m.proc.KillSession(name)
		if kerr != nil {
			// kill 基础设施错误（tmux 不可达）：视为未净，返回错误让调用方保留 watchdog 兜底（design.md §10）。
			killFailed = true
			continue
		}
		// KillResult disposition 判定（design.md §10/§8）：clean 视为已收割；
		// 非 clean（snapshot_failed/kill_failed/reap_failed/snapshot_missing_degraded）MUST 持久化 tickets
		// 为 notice（逃逸进程下次启动可定位），不得随会话删除丢弃。
		if res.Disposition != "" && res.Disposition != process.DispositionClean {
			// 已知 task：tickets 落该 task 的 DB notice（下次启动 reconcile 经 retryTaskNotices 处理）。
			// 无 DB 行的孤儿会话：退回内存 orphanFailures，供后台周期 retryOrphanSessions 重试（F3）。
			if tid := taskIDFromSessionName(name); tid != "" {
				if _, gerr := m.store.GetTask(ctx, tid); gerr == nil {
					if nerr := m.recordResidualNoticeFromDisposition(ctx, tid, name, res); nerr != nil {
						log.Printf("shutdown: record residual notice for %s: %v", name, nerr)
					}
				} else {
					m.orphanMu.Lock()
					m.orphanFailures = append(m.orphanFailures, orphanFailure{
						sessionName: name,
						tickets:     res.CleanupTickets,
					})
					m.orphanMu.Unlock()
				}
			}
			killFailed = true
		}
	}
	// 确认空：再次枚举，残留记录日志。
	remaining, err := m.proc.ListSessions()
	if err != nil && !errors.Is(err, process.ErrNoTmuxServer) {
		return fmt.Errorf("shutdown: verify empty: %w", err)
	}
	residualSessions := len(remaining) > 0
	if residualSessions {
		log.Printf("shutdown: %d sessions remain after kill (will retry on next start)", len(remaining))
	}
	// 检查 DB retryable debt 未清：任一任务仍有 retryable residual notice → runtime 未净（design.md §10）。
	// kill 模式下确认 runtime 已空 = 无 tmux 会话 且 无可重试 cleanup debt（debt 仍存意味着逃逸进程未收割）。
	// 任一不满足 → 返回非 nil，调用方（cmd）据此保留 watchdog 兜底不停。
	debtRemaining := m.hasRetryableDebtAny(ctx)
	// R7：orphanFailures 非空（retryOrphanSessions 后仍有未收割的逃逸进程 tickets）也视为 runtime 未净，
	// 不得返回成功退出（design.md §10）。
	m.orphanMu.Lock()
	orphanRemaining := len(m.orphanFailures)
	m.orphanMu.Unlock()
	if killFailed || residualSessions || debtRemaining || orphanRemaining > 0 {
		return fmt.Errorf("shutdown: runtime not clean (killFailed=%v residualSessions=%v debtRemaining=%v orphanFailures=%d)",
			killFailed, residualSessions, debtRemaining, orphanRemaining)
	}
	return nil
}

// hasRetryableDebtAny 检查是否存在任何任务仍有 retryable residual notice（供 Shutdown kill 模式确认 runtime 已空）。
func (m *Manager) hasRetryableDebtAny(ctx context.Context) bool {
	tasks, err := m.store.ListAllTasks(ctx)
	if err != nil {
		// 读不回视为有 debt（fail-closed：不确定就不放行）。
		return true
	}
	for _, t := range tasks {
		has, _ := m.hasRetryableNotice(ctx, t)
		if has {
			return true
		}
	}
	return false
}

// retryOrphanSessions 重试 kill 失败的孤儿会话（B9：非仅日志，进后台重试）。
// RetryReap 返回的 error 与 remaining tickets MUST 保留：remaining>0 视为未收敛，
// 保留 orphanFailure 进下轮重试；会话已消失时不得直接丢弃未收割 tickets，
// 仍需对既有 tickets 执行 RetryReap，未收割者继续保留。
// 后续 kill 产生的新 tickets 与既有聚合（append，不覆盖丢失）。
// R7 fail-closed：HasSession infra 错误保守视为存活（未收敛），不得当 absent 跳过 kill
// 致逃逸进程误判干净（design.md §5/§10）。
// R7 持久化：未收敛的 orphan tickets 经 debtStore 持久化（跨重启恢复），收敛的从表删除。
// B2：返回 persistOrphanDebts 的错误（cleanup_debt List/Upsert/Delete 失败），
// 供 Reconcile 传播给 main 据此拒开 HTTP。后台周期与 Shutdown 调用方仅记日志。
func (m *Manager) retryOrphanSessions(ctx context.Context) error {
	m.orphanMu.Lock()
	failures := m.orphanFailures
	m.orphanFailures = nil
	m.orphanMu.Unlock()
	var stillFailing []orphanFailure
	for _, f := range failures {
		// 既有 tickets 先 RetryReap（不论会话是否仍存在）：会话已消失但 tickets 未收割时，
		// 不得丢弃，仍需 reap；remaining>0 视为未收敛保留。
		var reapRemaining []string
		if len(f.tickets) > 0 {
			left, rerr := m.proc.RetryReap(f.tickets)
			if rerr != nil {
				// reap 基础设施错误：保留全部既有 tickets 进下轮重试。
				reapRemaining = f.tickets
			} else {
				reapRemaining = left
			}
		}
		// 再尝试 kill 仍存活的会话；kill 成功可能产生新 tickets。
		// R7 fail-closed：HasSession infra 错误保守视为存活（alive=true），
		// 不得当 absent 跳过 kill——Has 错误时 tickets 可能仍需 kill 收割，
		// 误判干净会让逃逸进程脱离重试（design.md §5/§10）。
		var newTickets []string
		alive, herr := m.proc.HasSession(f.sessionName)
		if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
			// infra 错误：保守视为存活（alive=true）。
			alive = true
		}
		if alive {
			res, kerr := m.proc.KillSession(f.sessionName)
			if kerr != nil || (res.Disposition != "" && res.Disposition != process.DispositionClean) {
				// kill 失败：聚合新 tickets 与既有 remaining（不覆盖丢失）。
				// R7 fail-closed：kill infra 错误时保留既有 f.tickets（reap 可能已清但 kill 失败
				// 意味会话仍可能存活，保守保留原始 tickets 进下轮重试，design.md §5/§10）。
				newTickets = append(newTickets, reapRemaining...)
				newTickets = append(newTickets, f.tickets...)
				newTickets = append(newTickets, res.CleanupTickets...)
				stillFailing = append(stillFailing, orphanFailure{sessionName: f.sessionName, tickets: newTickets})
				continue
			}
			// kill 成功但可能产生 reap_failed tickets，聚合进 remaining。
			reapRemaining = append(reapRemaining, res.CleanupTickets...)
		}
		// 会话已消失或 kill 成功后，仍按 remaining 判定是否收敛。
		if len(reapRemaining) > 0 {
			// 未收割 tickets 保留进下轮（会话已消失也不丢弃）。
			stillFailing = append(stillFailing, orphanFailure{sessionName: f.sessionName, tickets: reapRemaining})
		}
	}
	if len(stillFailing) > 0 {
		m.orphanMu.Lock()
		m.orphanFailures = append(m.orphanFailures, stillFailing...)
		m.orphanMu.Unlock()
	}
	// R7 持久化：未收敛 orphan tickets 写入 cleanup_debts（跨重启恢复），收敛的从表删除（design.md §10）。
	return m.persistOrphanDebts(ctx)
}

// persistOrphanDebts 将当前内存 orphanFailures 同步到 cleanup_debts 表（design.md §10）。
// 未收敛的 upsert（P4 复评阻塞 3e：既有 tickets union，不 latest-wins 覆盖），已无内存条目的
// 旧 debt 从表删除（已收敛）。debtStore 为 nil 时降级为仅内存（不跨重启恢复）。
// B2：cleanup_debt 的 List/Upsert/Delete 失败 MUST 聚合返回（不再仅记日志），供 Reconcile 传播。
func (m *Manager) persistOrphanDebts(ctx context.Context) error {
	if m.debtStore == nil {
		return nil
	}
	m.orphanMu.Lock()
	current := make(map[string][]string, len(m.orphanFailures))
	for _, f := range m.orphanFailures {
		current[f.sessionName] = f.tickets
	}
	m.orphanMu.Unlock()
	var errs []error
	// upsert 未收敛的（逐项走 persistOrphanDebt 做 union，与 killOrphanSession 路径一致）。
	for sessionName, tickets := range current {
		if err := m.persistOrphanDebt(ctx, sessionName, tickets); err != nil {
			errs = append(errs, err)
		}
	}
	// 删除已收敛的：表中存在但内存已无的 session。
	existing, err := m.debtStore.ListCleanupDebts(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("persist orphan debts: list existing: %w", err))
		return errors.Join(errs...)
	}
	for _, row := range existing {
		if _, ok := current[row.SessionName]; !ok {
			if err := m.debtStore.DeleteCleanupDebt(ctx, row.SessionName); err != nil {
				errs = append(errs, fmt.Errorf("persist orphan debts: delete converged %s: %w", row.SessionName, err))
			}
		}
	}
	return errors.Join(errs...)
}

// restoreCleanupDebts 从 cleanup_debts 表恢复未收敛 orphan tickets 到内存 orphanFailures
// （design.md §10：进程退出再启动后 Reconcile 从该表恢复重试）。
// debtStore 为 nil 时无操作（降级为仅内存）。
// P4 复评阻塞 3：错误 MUST 传播（Reconcile restore 失败 → fail-closed 拒绝开放 HTTP，
// 与既有 Reconcile fail-closed 一致）。
func (m *Manager) restoreCleanupDebts(ctx context.Context) error {
	if m.debtStore == nil {
		return nil
	}
	rows, err := m.debtStore.ListCleanupDebts(ctx)
	if err != nil {
		return fmt.Errorf("restore orphan debts: list: %w", err)
	}
	var restored []orphanFailure
	for _, r := range rows {
		var tickets []string
		if err := json.Unmarshal([]byte(r.Tickets), &tickets); err != nil {
			// 损坏 JSON：保守保留 sessionName + 原始 JSON 作为单 ticket（不丢弃会话身份）。
			tickets = []string{r.Tickets}
		}
		restored = append(restored, orphanFailure{sessionName: r.SessionName, tickets: tickets})
	}
	if len(restored) > 0 {
		m.orphanMu.Lock()
		m.orphanFailures = append(m.orphanFailures, restored...)
		m.orphanMu.Unlock()
	}
	return nil
}
