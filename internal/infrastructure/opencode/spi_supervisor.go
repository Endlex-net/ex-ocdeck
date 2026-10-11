// spi_supervisor.go 实现 RuntimeSupervisor 的 v1 真实承载（opencode-v1-spi-provider
// tasks 3.6 / design D9/D10；替换 C1 阶段 0 的 fail-closed 占位）。
//
// 职责边界（D9 binding 提交协议）：supervisor 只确定并返回最终 binding，MUST NOT
// 自行持久化 binding/任务状态——锚定写入经注入的 store seam 单事务原子完成
// （claim+锚定三元组），binding 提交事务由 task 层在激活/恢复成功后完成。
// 入口差异（D9a 表）：Acquire=首次（锚定三支全流程，无 permit/无 BeforeLaunch）；
// Resume=恢复类（policy.AllowSessionCreate 禁止锚定创建时修复即终止；
// BeforeLaunch 闸门先于端口分配与任何进程副作用）；AcquireEphemeral=删除清理
// （临时绑定：端口+进程+健康，无锚定/无 claim/无对齐）；Release=幂等 kill。
//
// 阶段 2 中间态：凭据仍为随机密码（tmux env 反查包装成 binding 形态随 task 层
// 3.6 现状保留），派生凭据接线属后续段（tasks 4.x）。runtime 生命周期旁路副作用
// （env_snapshot 端口持久化、permission_mode_at_start、env 组装）经注入 seam
// 委托 task 层现状实现，保证行为逐点等价。
//
// 现状行为来源：internal/task/activate.go 的 startRuntimeWithPortRetry /
// bootstrapRuntime / waitServeReadyOrDead（C2a 搬运，端口重试/kill 分类/启动
// argv 的单点原语在 internal/application/agentprovider/runtime_cmd.go——
// task 层与本适配器共用同一实现）。
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/process"
)

// SupervisorProcSeam 是 supervisor 的进程托管 seam（process.Manager 满足）。
type SupervisorProcSeam interface {
	NewSession(spec process.SessionSpec) error
	KillSession(name string) (process.KillResult, error)
	HasSession(name string) (bool, error)
}

// SupervisorStoreSeam 是 supervisor 的持久化 seam（store 层对应方法包装满足）。
type SupervisorStoreSeam interface {
	// GetTaskAnchor 读锚定 upstream session id（空串=无锚定）。
	GetTaskAnchor(ctx context.Context, taskID string) (anchorID string, err error)
	// ClaimSessionKeyAndSetAnchor 单事务 claim + 写入锚定三元组（v1 命名空间；
	// store.Claims 三列谓词 D4）。
	ClaimSessionKeyAndSetAnchor(ctx context.Context, taskID, sessionID string,
		createdAt, firstSeen, lastSeen int64, parentID string) (application.ClaimResult, error)
	// ClearTaskAnchorConditional 锚定 CAS 清空（失效锚定分派：Matched=已清空）。
	ClearTaskAnchorConditional(ctx context.Context, taskID, oldAnchor string) (application.MutationResult, error)
	// GetTaskLastPort 读端口复用提示（allocate 候选优先）。
	GetTaskLastPort(ctx context.Context, taskID string) (port int, valid bool, err error)
}

// SupervisorSideEffects 是 task 层拥有的旁路副作用 seam（行为保持现状）。
type SupervisorSideEffects interface {
	// ResolvePermissionMode 解析权限模式并写入启动事实（现状：
	// resolvePermissionMode + SetPermissionModeAtStart；错误 fail-closed 不建进程）。
	ResolvePermissionMode(ctx context.Context, taskID string) (mode string, err error)
	// PersistServePort 持久化轮换后的 serve 端口（现状 = env_snapshot 端口更新；
	// 失败 → 本次 attempt 失败，MUST NOT 建会话）。
	PersistServePort(ctx context.Context, taskID string, env map[string]string) error
	// BuildServeEnv 组装 serve env 快照（现状 = layerEnvSnapshot 合并结果，含
	// OCDECK_TASK_ID 与用户/生命周期 env）。
	BuildServeEnv(ctx context.Context, taskID string) (map[string]string, error)
}

// SupervisorDeps 注入 supervisor 的实现依赖；全部提供后为真实实现（nil Proc/
// Store/SideFx/AllocatePort 时保持 C1 fail-closed 占位——阶段 0 测试依赖该形态）。
type SupervisorDeps struct {
	Proc   SupervisorProcSeam
	Store  SupervisorStoreSeam
	SideFx SupervisorSideEffects
	// PasswordFor 返回任务运行时密码（D3 派生凭据；同任务恒定）。nil 时默认
	// 派生实现（ResolveMasterSecret + DeriveRuntimePassword）——主密钥缺失
	// fail-closed（C2c tasks 4.1）。
	PasswordFor  func(taskID string) (string, error)
	AllocatePort func(lastPort int, lastValid bool, exclude int) (int, error)
	// AttachClientArgv 返回 attach 客户端 argv（TerminalIO.AttachCommand 复用
	// process.Manager 的 socket 绑定构造；生产装配注入 process.Manager 适配）。
	AttachClientArgv func(sessionName string) ([]string, error)
	// OnRuntimeReady 是 runtime 换代回调（tasks 3.8 epoch 语义）：Acquire/Resume
	// 成功确定新 runtime 时调用一次（恰一次；失败路径不调用）。生产装配注入
	// HealthTracker.BumpRuntimeEpoch；nil 时跳过。
	OnRuntimeReady func(taskID string)
	// OnRuntimeRelease 是 runtime 回收回调（C6 门禁修复「换代重置旧观测」的
	// 释放侧）：Release 成功终止 runtime 会话后调用一次（幂等 kill 的 !exists
	// 分支不调——本代 runtime 早已不在）。生产装配注入 HealthTracker.ResetRuntime
	//（清空该任务 runtime/session 作用域观测与代际）；nil 时跳过。
	OnRuntimeRelease func(taskID string)
	// ReadyTimeout/PollInterval 健康轮询墙钟预算（生产 10s/500ms；测试注入短值）。
	ReadyTimeout time.Duration
	PollInterval time.Duration
	// ProbeColdStartBackoff 覆盖能力探测冷启动重试退避序列（生产 nil → 2s/4s 与
	// task 层 defaultProbeColdStartBackoff 一致；测试注入短序列）。
	ProbeColdStartBackoff func() []time.Duration
}

// passwordFor 解析任务密码（deps 覆盖或默认派生实现；主密钥缺失 fail-closed）。
func (d *SupervisorDeps) passwordFor(taskID string) (string, error) {
	if d.PasswordFor != nil {
		return d.PasswordFor(taskID)
	}
	master, err := appagent.EnsureMasterSecret()
	if err != nil {
		return "", appagent.NewError("supervisor.derive_password", appagent.ErrKindBindingInvalid, err)
	}
	return appagent.DeriveRuntimePassword(master, ProviderID, taskID), nil
}

// supPortRetries 端口轮换预算（与 task 层 servePortRetries 现状一致）。
const supPortRetries = 3

// bindingAuthKindRandom 是迁移导入存量随机密码的 auth.kind 值（D3 legacy
// 例外；由迁移导入 runner 写入，supervisor 新建 binding 一律 derived）。
const bindingAuthKindRandom = appagent.LegacyCredKind

// buildBinding 按 D3 envelope 形状构造最终 binding（只返回不持久化）。
// C2c（tasks 4.1）：新建 runtime 一律派生凭据——auth.kind=derived、密码不落库
// （消费方经 resolveBinding 现场派生）；TaskID 随 data 携带（派生入参）。
func buildBinding(task appagent.TaskRef, anchorID, sessionName string, port int) domain.TaskBinding {
	data, _ := json.Marshal(v1BindingData{
		Port:      port,
		Directory: task.Directory,
		TaskID:    task.TaskID,
		Auth:      v1BindingAuth{Kind: appagent.DerivedCredKind},
	})
	return domain.TaskBinding{
		Version:            domain.TaskBindingVersion,
		ProviderID:         ProviderID,
		ProviderInstanceID: ProviderInstanceID,
		UpstreamSessionID:  anchorID,
		TerminalRef:        sessionName,
		Data:               data,
	}
}

// decodeV1BindingEndpoint 解析 binding 的连接端点与凭据（复用解析规则：
// provider 命名空间校验 + data 解码 + derived 现场派生 / legacy 存量值——与
// SPIProvider.resolveBinding 同一形状，供 Resume 的 TerminalRef 存活复查使用）。
func decodeV1BindingEndpoint(b domain.TaskBinding) (port int, password string, dir string, err error) {
	if b.ProviderID != ProviderID {
		return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
			fmt.Errorf("binding provider %q 不属于 %s", b.ProviderID, ProviderID))
	}
	var data v1BindingData
	if len(b.Data) > 0 {
		if uerr := json.Unmarshal(b.Data, &data); uerr != nil {
			return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
				fmt.Errorf("decode v1 binding data: %w", uerr))
		}
	}
	if data.Port <= 0 || data.Auth.Kind == "" {
		return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
			errors.New("v1 binding 缺少 port/auth.kind"))
	}
	password = data.Auth.Password
	switch data.Auth.Kind {
	case appagent.DerivedCredKind:
		if data.TaskID == "" {
			return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
				errors.New("derived binding 缺少 taskId（派生入参）"))
		}
		master, merr := appagent.EnsureMasterSecret()
		if merr != nil {
			return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid, merr)
		}
		password = appagent.DeriveRuntimePassword(master, ProviderID, data.TaskID)
	case appagent.LegacyCredKind:
		if password == "" {
			return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
				errors.New("legacy binding 缺少导入密码"))
		}
	default:
		return 0, "", "", appagent.NewError("supervisor.resume_reuse", appagent.ErrKindBindingInvalid,
			fmt.Errorf("未知 auth.kind %q", data.Auth.Kind))
	}
	return data.Port, password, data.Directory, nil
}

// startServeWithRetry 之后的密码经 passwordFor 派生（见函数体；C2c tasks 4.1）。

// NewSPIProviderWithDeps 构造带 supervisor deps 的 v1 Provider（生产装配用）。
func NewSPIProviderWithDeps(deps SupervisorDeps) *SPIProvider {
	p := NewSPIProvider()
	p.supDeps = &deps
	return p
}

// --- RuntimeSupervisor 入口 ---

// Acquire（首次激活，D9：无 permit、无 BeforeLaunch；锚定创建允许——双启动
// 子协议不耗恢复 permit）→ 锚定三支子协议。
func (p *SPIProvider) Acquire(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	if p.supDeps == nil {
		return domain.TaskBinding{}, supervisorPendingError("supervisor.acquire")
	}
	return p.supDeps.runBinding(ctx, task, domain.TaskBinding{}, appagent.ResumePolicy{AllowSessionCreate: true}, false)
}

// Resume（恢复类入口，D9a 表）：old 为现状 binding；policy 承载入口差异——
// AllowSessionCreate=false 时锚定缺席/失效即失败（tryRepairRuntime 禁创建）；
// BeforeLaunch 是恢复预算闸门（先于端口分配与首次建程；无锚定双启动的正式进程
// 前由 noAnchorDualStart 再次调用——每次进程创建对应一个已写入 permit）。
// AllowRebuild 为 provider-neutral 契约字段（D9a）；v1 的 terminal_ref（tmux
// 会话名）失效即锚定重建路径，与 AllowSessionCreate 同一开关承载，无独立分支。
func (p *SPIProvider) Resume(ctx context.Context, task appagent.TaskRef,
	old domain.TaskBinding, policy appagent.ResumePolicy) (domain.TaskBinding, error) {
	if p.supDeps == nil {
		return domain.TaskBinding{}, supervisorPendingError("supervisor.resume")
	}
	return p.supDeps.runBinding(ctx, task, old, policy, true)
}

// AcquireEphemeral（删除清理）：临时 binding——端口+进程+健康检查，无锚定
// bootstrap/无 claim/无对齐/无任务状态提交；调用方 MUST NOT 持久化返回值
// （specs 删除清理例外；D9a 分支②）。
func (p *SPIProvider) AcquireEphemeral(ctx context.Context, task appagent.TaskRef) (domain.TaskBinding, error) {
	if p.supDeps == nil {
		return domain.TaskBinding{}, supervisorPendingError("supervisor.acquire_ephemeral")
	}
	if err := p.supDeps.checkDeps(task); err != nil {
		return domain.TaskBinding{}, err
	}
	b, rerr := p.supDeps.runServeEphemeral(ctx, task, "ocdeck-"+task.TaskID+"-runtime")
	if rerr != nil {
		return domain.TaskBinding{}, rerr
	}
	// 临时 runtime 亦是进程换代（epoch 递增；不持久化 binding 不变）。
	if p.supDeps.OnRuntimeReady != nil {
		p.supDeps.OnRuntimeReady(task.TaskID)
	}
	return b, nil
}

// Release（挂起/删除）：幂等 kill runtime 会话；clean → nil；非 clean disposition
// → PendingCleanupError（调用方按 cleanup notice 处置，D10）。
func (p *SPIProvider) Release(ctx context.Context, b domain.TaskBinding) error {
	if p.supDeps == nil {
		return supervisorPendingError("supervisor.release")
	}
	return p.supDeps.releaseBinding(ctx, b)
}

// releaseBinding 是 release 的实现体（幂等 kill；非 clean → PendingCleanupError）。
func (d *SupervisorDeps) releaseBinding(ctx context.Context, b domain.TaskBinding) error {
	if d.Proc == nil {
		return appagent.NewError("supervisor.release", appagent.ErrKindCapabilityUnavailable,
			errors.New("supervisor Proc seam 未注入"))
	}
	name := b.TerminalRef
	if name == "" {
		return nil // 无可释放目标（幂等）。
	}
	exists, herr := d.Proc.HasSession(name)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		return appagent.CleanupStageError("supervisor.release",
			appagent.CleanupPayload{SessionName: name, Reason: appagent.KillReasonKillFailed, Retryable: true},
			fmt.Errorf("release: has session: %w", herr))
	}
	if !exists {
		return nil
	}
	res, kerr := d.Proc.KillSession(name)
	if kerr != nil {
		return appagent.CleanupStageError("supervisor.release",
			appagent.CleanupPayload{SessionName: name, Reason: appagent.KillReasonKillFailed, Tickets: res.CleanupTickets, Retryable: true},
			kerr)
	}
	cls := appagent.ClassifyKill(string(res.Disposition), res.SessionKilled)
	switch cls.Action {
	case "none":
		// C6：runtime 已终止——回收该任务的代际与旧观测（释放侧换代重置）。
		// C6 残余修复（review attempt 2）：身份键用真实 task ID——ready/report/
		// release/reset 同键。binding data.taskId 是显式上下文（derived/迁移后
		// legacy 均携带）；拿 upstream session ID 顶替的旧形态移除。无 taskId 的
		// 遗留预迁移 binding：release 观测以 provider 作用域落账（epoch 不挂靠，
		// 迁移导入后新 binding 携带 taskId）。
		// mutation 证据：OnRuntimeRelease 回调删除 → 组合测试 ResetRuntime
		// 断言红（旧观测留存）；测试见 cmd/ocdeck-server TestComposeRoot
		// health wiring（生产装配直测 OnRuntimeRelease→tracker.ResetRuntime）。
		if d.OnRuntimeRelease != nil {
			d.OnRuntimeRelease(bindingHealthTaskID(b))
		}
		return nil
	default:
		return appagent.CleanupStageError("supervisor.release",
			appagent.CleanupPayload{SessionName: name, Reason: cls.Reason, Tickets: res.CleanupTickets, Retryable: cls.Retryable},
			fmt.Errorf("release disposition %s", res.Disposition))
	}
}

// --- 共享主流程 ---

// runServeEphemeral 临时绑定（AcquireEphemeral）：端口分配 + 单次启动（无
// --session、无锚定 bootstrap、无 claim）+ 健康检查。返回的 binding 一次性内存值。
func (d *SupervisorDeps) runServeEphemeral(ctx context.Context, task appagent.TaskRef, sessionName string) (domain.TaskBinding, error) {
	port, _, err := d.startServeWithRetry(ctx, task, sessionName, "", false, true, nil)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	return buildBinding(task, "", sessionName, port), nil
}

// runBinding 是 Acquire/Resume 的共享锚定主流程（D9）：恢复类入口先做旧
// TerminalRef 存活复用（C4：Resume 先验证并复用旧 binding 的存活 runtime——
// 存活且健康即返回旧 binding，零进程副作用；失效才按 AllowRebuild 进入锚定
// 重建）→ BeforeLaunch（先于端口分配与副作用）→ 锚定读取 → bootstrapAttempt
// （内含端口重试预算；每次 NewSession 前过 oncePerCreate 闸门）。
func (d *SupervisorDeps) runBinding(ctx context.Context, task appagent.TaskRef,
	old domain.TaskBinding, policy appagent.ResumePolicy, isResume bool) (domain.TaskBinding, error) {
	if err := d.checkDeps(task); err != nil {
		return domain.TaskBinding{}, err
	}
	sessionName := "ocdeck-" + task.TaskID + "-runtime"
	// C4（specs「终端引用失效重建绑定」）：Resume 先按旧 binding 的 TerminalRef
	// 存活复用——tmux 会话仍在且健康检查通过 → 直接返回旧 binding（零 NewSession；
	// 上游凭据未变化，派生凭据同任务恒定）。缺失/infra 错误 → 视为已死亡进入重建。
	// mutation 证据（已验证 → 红）：复用旁路（`&& false`）→
	// resume_reuses_alive_terminal 红——存活任务被重建（new=2）、绑定锚定被换新。
	if isResume && old.TerminalRef != "" {
		if binding, reused, rerr := d.tryReuseAliveTerminal(ctx, task, old); rerr != nil {
			return domain.TaskBinding{}, rerr
		} else if reused {
			// 复用同样是「runtime 确定就绪」的换代观测点（tasks 3.8）。
			if d.OnRuntimeReady != nil {
				d.OnRuntimeReady(task.TaskID)
			}
			return binding, nil
		}
	}
	// 首次 BeforeLaunch 先于端口分配（D9a：闸门先于端口分配与启动副作用）。
	if isResume && policy.BeforeLaunch != nil {
		if err := policy.BeforeLaunch(ctx); err != nil {
			return domain.TaskBinding{}, err
		}
	}
	// 每进程创建闸门收口（specs「任务进程自动重拉」预算协议：每次进程创建对应
	// 一个已写入 permit——端口轮换的新进程同样先过闸门再分配端口）。恢复类入口
	// 的闸门在 BeforeLaunch 与轮次闸门间共享同一 policy 闭包。
	var perCreate func(ctx context.Context) error
	if isResume && policy.BeforeLaunch != nil {
		perCreate = policy.BeforeLaunch
	}
	anchor := ""
	if isResume {
		anchor = old.UpstreamSessionID
	} else {
		rowAnchor, aerr := d.Store.GetTaskAnchor(ctx, task.TaskID)
		if aerr != nil {
			return domain.TaskBinding{}, fmt.Errorf("supervisor: read anchor: %w", aerr)
		}
		anchor = rowAnchor
	}
	// C4：AllowRebuild=false（修复入口）在 terminal_ref 失效时按既有「禁创建」
	// 分派（D9a：repair 不启动进程）；AllowRebuild=true 与 AllowSessionCreate
	// 在 v1 同开关承载（tmux 会话名重建即锚定重建）。
	// C4 残余修复（review attempt 2）：AllowRebuild 独立约束——复用失败（terminal
	// 已死）且禁止重建 → 修复失败返回，零端口分配零 NewSession；重建与锚定创建
	// 独立闸门（AllowRebuild=false + AllowSessionCreate=true 的组合：无重建但
	// 允许补锚——本期无此入口形态，语义保留扩展点）。
	// C4 残余修复（review attempt 2）：AllowRebuild 独立约束——仅 isResume（恢复类
	// 入口；Acquire 无复用语义恒重建不受此闸）。复用失败（terminal 已死）且禁止重建
	// → 修复失败返回，零端口分配零 NewSession（有锚定情形：直启属重建动作，被
	// AllowRebuild=false 拒绝）。无锚定情形不在此拦——锚定创建由 AllowSessionCreate
	// 独立闸门（禁创建 → errRepairCompleteNoAnchor 修复完成语义，D9a），两个开关
	// 独立处理。
	if isResume && anchor != "" && !policy.AllowRebuild {
		return domain.TaskBinding{}, appagent.NewError("supervisor.rebuild",
			appagent.ErrKindRuntimeUnavailable,
			fmt.Errorf("rebuild disallowed (terminal %s dead)", sessionName))
	}
	binding, berr := d.bootstrapAttempt(ctx, task, sessionName, anchor, policy, perCreate)
	if berr != nil {
		// 携带锚定/建程失败或修复完成信号（errRepairCompleteNoAnchor）原样返回；
		// 调用方 errors.Is 区分修复完成与失败。
		return domain.TaskBinding{}, berr
	}
	// runtime 换代点（tasks 3.8）：成功确定新 runtime 后恰一次回调。
	if d.OnRuntimeReady != nil {
		d.OnRuntimeReady(task.TaskID)
	}
	return binding, nil
}

// tryReuseAliveTerminal 按 C4/specs 复用旧 binding 的存活 runtime：TerminalRef
// 对应 tmux 会话仍在且健康检查（+能力探测）通过 → (binding,true,nil)；会话缺失
// → (zero,false,nil) 进入重建；infra 错误/健康失败 → 非零 error（fail-closed，
// 不静默重建——健康失败属修复失败语义，调用方 task 层按挂起补偿处置）。
func (d *SupervisorDeps) tryReuseAliveTerminal(ctx context.Context, task appagent.TaskRef,
	old domain.TaskBinding) (domain.TaskBinding, bool, error) {
	exists, herr := d.Proc.HasSession(old.TerminalRef)
	if herr != nil {
		if !errors.Is(herr, process.ErrNoTmuxServer) {
			return domain.TaskBinding{}, false, appagent.NewError("supervisor.resume_reuse",
				appagent.ErrKindRuntimeUnavailable, herr)
		}
		return domain.TaskBinding{}, false, nil // tmux 不可达 = 无法复用，进入重建。
	}
	if !exists {
		return domain.TaskBinding{}, false, nil
	}
	// 复用前健康 + 能力探测（凭据按派生规则现场解析——binding 不携带密码）。
	port, pw, _, derr := decodeV1BindingEndpoint(old)
	if derr != nil {
		return domain.TaskBinding{}, false, appagent.NewError("supervisor.resume_reuse",
			appagent.ErrKindBindingInvalid, derr)
	}
	oc := NewClient(port, pw, Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	h, herr := oc.Health(ctx)
	if herr != nil || !h.Healthy {
		return domain.TaskBinding{}, false, appagent.NewError("supervisor.resume_reuse",
			appagent.ErrKindRuntimeUnavailable, fmt.Errorf("reused runtime health: %v", herr))
	}
	if _, perr := oc.Probe(ctx); perr != nil {
		return domain.TaskBinding{}, false, appagent.NewError("supervisor.resume_reuse",
			appagent.ErrKindRuntimeUnavailable, fmt.Errorf("reused runtime probe: %w", perr))
	}
	return old, true, nil
}

// bootstrapAttempt 单次锚定尝试（D9 主流程压缩）：
//   - 有锚定：--session 直启 → 列表校验；缺席 → CAS 清空转无锚定（policy 禁创建
//     时直接失败）；
//   - 无锚定：policy.AllowSessionCreate=false → 失败（tryRepairRuntime 终止语义）；
//     否则双启动（bootstrap 进程 + POST + claim + 确认终止 + 正式进程 --session）。
//     perCreate 在每次 NewSession（含端口轮换轮次）前调用。
func (d *SupervisorDeps) bootstrapAttempt(ctx context.Context, task appagent.TaskRef,
	sessionName, anchor string, policy appagent.ResumePolicy, perCreate func(ctx context.Context) error) (domain.TaskBinding, error) {
	if anchor != "" {
		// ① 有锚定直启。
		binding, serr := d.startServeWithListCheck(ctx, task, sessionName, anchor, perCreate)
		if serr == errAnchorMissingInList {
			// ② 锚定失效 → CAS 清空（D5：条件清空 + 失配复读）。
			cleared, cerr := d.Store.ClearTaskAnchorConditional(ctx, task.TaskID, anchor)
			if cerr != nil {
				return domain.TaskBinding{}, appagent.NewError("supervisor.clear_stale_anchor",
					appagent.ErrKindRuntimeUnavailable, cerr)
			}
			if !cleared.Matched {
				// 失配复读：其他路径已改锚定——本次终止（MUST NOT 覆盖，D5）。
				return domain.TaskBinding{}, appagent.NewError("supervisor.clear_stale_anchor",
					appagent.ErrKindConflict, errors.New("anchor CAS mismatch"))
			}
			// 直启的进程在复用会话名/端口前确认终止（现状：bootstrapRuntime 的
			// confirmRuntimeTerminated，activate.go:1739——非 clean disposition
			// → Retryable PendingCleanupError）。
			if terr := d.confirmTerminated(ctx, task.TaskID, sessionName); terr != nil {
				return domain.TaskBinding{}, terr
			}
			if !policy.AllowSessionCreate {
				// 修复路径（tryRepairRuntime）：禁创建 → 修复完成即终止
				//（D9a：无锚定且无 owned session 在「对齐」步终止；以 typed
				// 修复完成信号返回，调用方区分于失败）。
				return domain.TaskBinding{}, errRepairCompleteNoAnchor
			}
			return d.noAnchorDualStart(ctx, task, sessionName, perCreate)
		}
		if serr != nil {
			return domain.TaskBinding{}, serr
		}
		return binding, nil
	}
	// ③ 无锚定：修复路径禁创建 → 修复完成信号（同上）。
	if !policy.AllowSessionCreate {
		return domain.TaskBinding{}, errRepairCompleteNoAnchor
	}
	return d.noAnchorDualStart(ctx, task, sessionName, perCreate)
}

// errRepairCompleteNoAnchor 修复路径无锚定且禁创建：修复完成（非失败），
// 调用方（task 层 tryRepairRuntime）据此进入对齐/挂起语义，不得判恢复失败。
var errRepairCompleteNoAnchor = errors.New("supervisor: repair complete (no anchor, create disallowed)")

// noAnchorDualStart 双启动子事务（D9 第 4 步）：bootstrap 进程 → 健康 → POST /
// session 创建 + 原子 claim + 锚定写入 → 确认 bootstrap 终止 → 正式进程
// （--session，新凭据）→ 健康 + 能力探测 + 锚存在校验。perCreate 非 nil
// 时在每次 NewSession（bootstrap 与正式进程；含正式进程端口轮换轮次）前调用
// （恢复预算闸门：每次进程创建对应一个已写入 permit——specs「每任务单进程
// 实例」双启动子协议⑥）。
func (d *SupervisorDeps) noAnchorDualStart(ctx context.Context, task appagent.TaskRef, sessionName string, perCreate func(ctx context.Context) error) (domain.TaskBinding, error) {
	// bootstrap 进程（无 --session）。
	port, password, err := d.startServeWithRetry(ctx, task, sessionName, "", true, false, perCreate)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	oc := NewClient(port, password, Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	created, cerr := oc.CreateSession(ctx, task.Directory, "")
	if cerr != nil {
		return domain.TaskBinding{}, appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("create anchor session: %w", cerr))
	}
	now := time.Now().Unix()
	cres, perr := d.Store.ClaimSessionKeyAndSetAnchor(ctx, task.TaskID, created.ID,
		int64(created.Time.Created), now, int64(created.Time.Updated), "")
	if perr != nil {
		return domain.TaskBinding{}, appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("persist anchor %s: %w", created.ID, perr))
	}
	if !cres.Claimed {
		// claim 冲突（D9 第 3 步）：激活/恢复失败 + last_error（task 层），MUST NOT 接入。
		// C12 残余修复：阶段错误统一 StageError 返回类型——task 层
		// supervisorStageErrorForTyped 仅经 StageErrorOf 检出 *StageError；
		// 普通 Error 会逃过「claim_anchor→anchorStageError→终态补偿」分派，被
		// 原样返回走 attempt 重试（烧预算，specs 逐阶段失败分派表违约）。
		return domain.TaskBinding{}, appagent.NewStageError("supervisor.claim_anchor", appagent.ErrKindConflict,
			fmt.Errorf("anchor session %s conflict (owned by task %s); MUST NOT attach", created.ID, cres.OwnerTaskID))
	}
	// 确认 bootstrap 终止（既有 KillResult 规则）。
	if terr := d.confirmTerminated(ctx, task.TaskID, sessionName); terr != nil {
		return domain.TaskBinding{}, terr
	}
	// 正式进程（--session，同端口新密码）：恢复路径在第二次建程前过预算闸门
	//（任务层 beforeCreate 语义：连续续检 + permit + 退避）。已 claim 的锚定保留
	//（specs「每任务单进程实例」⑥）——闸门拒绝时本次直接失败，终态补偿由调用方
	//（task 层恢复执行器）处置。mutation 证据（已验证 → 红）：闸门旁路
	//（`if false && perCreate != nil`）→ beforeLaunch_precedes_port_allocation
	// 红（第二次闸门时 new=0）+ beforeLaunch_gates_every_process_create 红
	//（gate=1 new=2——正式进程未过闸门）。
	if perCreate != nil {
		if gerr := perCreate(ctx); gerr != nil {
			return domain.TaskBinding{}, gerr
		}
	}
	port, password, err = d.startServeWithRetry(ctx, task, sessionName, created.ID, false, false, perCreate)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	oc = NewClient(port, password, Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	sessions, lerr := oc.ListSessions(ctx, task.Directory, 1000)
	if lerr != nil {
		return domain.TaskBinding{}, appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("list sessions for anchor check: %w", lerr))
	}
	present := false
	for _, s := range sessions {
		if s.ID == created.ID {
			present = true
			break
		}
	}
	if !present {
		return domain.TaskBinding{}, appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("anchor session %s missing after dual-start", created.ID))
	}
	return buildBinding(task, created.ID, sessionName, port), nil
}

// startServeWithListCheck 有锚定直启 + 列表校验（缺席返回 errAnchorMissingInList）。
// perCreate 在每次 NewSession（含端口轮换轮次）前调用（恢复闸门收口，C4）。
func (d *SupervisorDeps) startServeWithListCheck(ctx context.Context, task appagent.TaskRef,
	sessionName, anchor string, perCreate func(ctx context.Context) error) (domain.TaskBinding, error) {
	port, password, err := d.startServeWithRetry(ctx, task, sessionName, anchor, false, false, perCreate)
	if err != nil {
		return domain.TaskBinding{}, err
	}
	oc := NewClient(port, password, Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
	sessions, lerr := oc.ListSessions(ctx, task.Directory, 1000)
	if lerr != nil {
		// 锚定列表校验对 overflow 的既有失败语义保留（count==limit → 错误）。
		return domain.TaskBinding{}, appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("list sessions for anchor check: %w", lerr))
	}
	for _, s := range sessions {
		if s.ID == anchor {
			return buildBinding(task, anchor, sessionName, port), nil
		}
	}
	return domain.TaskBinding{}, errAnchorMissingInList
}

// errAnchorMissingInList 标记锚定列表校验缺席（转 CAS 清空/无锚定流程）。
var errAnchorMissingInList = errors.New("anchor session missing in list")

// beforeGate 把 ResumePolicy 的 BeforeLaunch 闸门适配为 noAnchorDualStart 的
// 无参形态（nil 保持 nil——Acquire 双启动不过闸门）。
func beforeGate(policy appagent.ResumePolicy) func() error {
	if policy.BeforeLaunch == nil {
		return nil
	}
	return func() error { return policy.BeforeLaunch(context.Background()) }
}

// startServeWithRetry 端口重试循环（现状 startRuntimeWithPortRetry 的搬运形态）：
// 每轮 BeforeGate（perCreate，先于该轮端口分配——恢复预算闸门：每次进程创建
// 对应一个已写入 permit，specs「任务进程自动重拉」）→ AllocatePort →
// PersistServePort（轮换时）→ NewSession → waitServeReadyOrDead → 失败分派
// （进程死亡→无 kill 直接轮换；健康超时→kill 门禁+轮换；最后一轮→终态）。
// 健康就绪后执行能力探测（现状 probeWithColdStartRetry：冷启动 ErrServeNotReady
// 短退避重试；探测失败按既定错误返回——「探测失败不轮换」）。
// ephemeral（删除清理临时 runtime）：跳过 ResolvePermissionMode——权限模式恒为
// ask（task-permission-mode D3 清理语义，与旧 task 层 startTempServe 一致）且
// MUST NOT 写启动事实、不过闸门、不做能力探测。
func (d *SupervisorDeps) startServeWithRetry(ctx context.Context, task appagent.TaskRef,
	sessionName, sessionID string, freshPasswordEach, ephemeral bool, perCreate func(ctx context.Context) error) (int, string, error) {
	permissionMode := ""
	if ephemeral {
		// tasks 4.4（疑点3 处置）：清理语义的 ask 模式经 application 常量引用。
		permissionMode = appagent.PermissionModeAsk
	} else {
		mode, perr := d.SideFx.ResolvePermissionMode(ctx, task.TaskID)
		if perr != nil {
			return 0, "", appagent.NewError("supervisor.permission_mode", appagent.ErrKindBindingInvalid, perr)
		}
		permissionMode = mode
	}
	// 临时路径亦不持久化轮换端口（env_snapshot 为激活期事实；删除清理无 env
	// 生效点，design D9 临时绑定无任务状态/快照副作用），也不读 last_port 候选。
	env := map[string]string{}
	if !ephemeral {
		var eerr error
		env, eerr = d.SideFx.BuildServeEnv(ctx, task.TaskID)
		if eerr != nil {
			return 0, "", appagent.NewError("supervisor.env", appagent.ErrKindRuntimeUnavailable, eerr)
		}
		// C13 门禁修复（D8 同代快照 ownership / spec「先持久化实际新端口再建程」）：
		// task 层在 attempt 顶部已把同代端口持久化进 env_snapshot（恢复入口
		// runRecoveryAttempt、激活入口 mergeEnvSnapshot）；BuildServeEnv seam 已
		// 叠加快照——env 中的 OCDECK_SERVE_PORT 即本代 ownership 端口，supervisor
		// 直接采用并跳过重新分配（GetTaskLastPort+AllocatePort 路径保留为 env 无
		// 端口时的防御形态）。分配 owner 统一为 task 层快照持久化动作。
		if snapshotPort, perr := strconv.Atoi(env["OCDECK_SERVE_PORT"]); perr == nil && snapshotPort > 0 {
			// 首 NewSession 前幂等重写持久化（spec：实际端口先于进程副作用落库）；
			// 失败禁建程。
			if perr := d.SideFx.PersistServePort(ctx, task.TaskID, env); perr != nil {
				return 0, "", appagent.NewError("supervisor.persist_port", appagent.ErrKindRuntimeUnavailable,
					fmt.Errorf("persist snapshot port: %w", perr))
			}
			return d.startServeLoop(ctx, task, sessionName, sessionID, ephemeral, permissionMode, env, snapshotPort, perCreate)
		}
		port, valid, lerr := d.Store.GetTaskLastPort(ctx, task.TaskID)
		if lerr != nil {
			return 0, "", appagent.NewError("supervisor.read_last_port", appagent.ErrKindRuntimeUnavailable, lerr)
		}
		allocated, aerr := d.AllocatePort(port, valid, 0)
		if aerr != nil {
			return 0, "", appagent.NewError("supervisor.allocate", appagent.ErrKindRuntimeUnavailable, aerr)
		}
		// C13 残余修复（spec :145「可重复进程 attempt：新端口持久化……失败 MUST NOT
		// 创建会话」）：快照缺失的后备首轮（last_port 回退分配）同样先持久化再建程
		//——分配即写入 env 快照，与主路径（attempt 0 直接快照端口）同序。
		env["OCDECK_SERVE_PORT"] = strconv.Itoa(allocated)
		if perr := d.SideFx.PersistServePort(ctx, task.TaskID, env); perr != nil {
			return 0, "", appagent.NewError("supervisor.persist_port", appagent.ErrKindRuntimeUnavailable,
				fmt.Errorf("persist allocated port: %w", perr))
		}
		return d.startServeLoop(ctx, task, sessionName, sessionID, ephemeral, permissionMode, env, allocated, perCreate)
	}
	allocated, aerr := d.AllocatePort(0, false, 0)
	if aerr != nil {
		return 0, "", appagent.NewError("supervisor.allocate", appagent.ErrKindRuntimeUnavailable, aerr)
	}
	return d.startServeLoop(ctx, task, sessionName, sessionID, ephemeral, permissionMode, env, allocated, perCreate)
}

// startServeLoop 是 startServeWithRetry 的端口重试主体（前置已确定初始端口与 env）。
// attempt 0 不过闸门——该次建程的 permit 由入口 BeforeLaunch（恢复路径 attempt
// 顶部）承载；attempt>0（端口轮换的新进程）每轮先过 perCreate 闸门再分配端口
// （C4/specs 预算协议：轮换新进程各耗 permit 一个，先于该轮端口分配）。
func (d *SupervisorDeps) startServeLoop(ctx context.Context, task appagent.TaskRef,
	sessionName, sessionID string, ephemeral bool, permissionMode string, env map[string]string, allocated int, perCreate func(ctx context.Context) error) (int, string, error) {
	password := ""
	var prevErr error
	for attempt := 0; attempt < supPortRetries; attempt++ {
		if attempt > 0 {
			// 轮换闸门（先于本次端口分配与进程副作用）。
			if perCreate != nil {
				if gerr := perCreate(ctx); gerr != nil {
					return 0, "", gerr
				}
			}
			var aerr error
			allocated, aerr = d.AllocatePort(allocated, true, allocated)
			if aerr != nil {
				// C12 残余修复：端口耗尽 = 端口分配终态（specs「可重复进程 attempt」
				// 分派表：allocate 阶段失败→终态补偿，MUST NOT 再烧恢复预算轮换）。
				// 普通 NewError 逃过 StageErrorOf→按重试分派；统一 StageError。
				return 0, "", appagent.NewStageError("supervisor.allocate", appagent.ErrKindRuntimeUnavailable,
					fmt.Errorf("%v: allocate: %w", prevErr, aerr))
			}
			if !ephemeral {
				env["OCDECK_SERVE_PORT"] = strconv.Itoa(allocated)
				if perr := d.SideFx.PersistServePort(ctx, task.TaskID, env); perr != nil {
					return 0, "", appagent.NewError("supervisor.persist_port", appagent.ErrKindRuntimeUnavailable,
						fmt.Errorf("%v: persist: %w", prevErr, perr))
				}
			}
		}
		if password == "" {
			// C2c（tasks 4.1）：派生凭据——同任务恒定（D3 批准的内部变更），
			// 不随重启/轮换重新生成；主密钥缺失 fail-closed（不建进程）。
			var derr error
			password, derr = d.passwordFor(task.TaskID)
			if derr != nil {
				return 0, "", derr
			}
		}
		serveEnv := copyEnv(env)
		serveEnv["OPENCODE_SERVER_PASSWORD"] = password
		serveEnv[appagent.CredKindEnvKey] = appagent.DerivedCredKind
		serveEnv["OCDECK_SERVE_PORT"] = strconv.Itoa(allocated)
		if err := d.Proc.NewSession(process.SessionSpec{
			Name:    sessionName,
			Dir:     task.Directory,
			Env:     serveEnv,
			CmdArgv: appagent.RuntimeCmdArgv(allocated, sessionID, permissionMode),
		}); err != nil {
			return 0, "", appagent.NewError("supervisor.spawn", appagent.ErrKindRuntimeUnavailable,
				fmt.Errorf("runtime session: %w", err))
		}
		oc := NewClient(allocated, password, Options{HealthTimeout: 2 * time.Second, OpTimeout: 10 * time.Second})
		werr := d.waitServeReadyOrDead(ctx, oc, sessionName)
		if werr == nil {
			if !ephemeral {
				// C4（specs「serve 启动成功判定」/「任务进程自动重拉」逐阶段表：
				// 能力探测任何失败 → 本次 attempt 失败，MUST NOT 本地 kill/换端口
				// ——kill+notice 委托调用方既有补偿）。冷启动 ErrServeNotReady
				// 短退避重试（现状 probeWithColdStartRetry 语义搬运）。
				if perr := probeWithColdStartRetry(ctx, oc, d.probeColdStartBackoff()); perr != nil {
					// C12 残余修复：普通 NewError 逃过 StageErrorOf 检出→task 层
					// 原样返回走 attempt 重试（烧预算）——specs「能力探测任何失败
					// →终态补偿」违约；统一 StageError 类型使 supervisor.probe
					// 进终态分派。
					return 0, "", appagent.NewStageError("supervisor.probe", probeErrKind(perr), perr)
				}
			}
			return allocated, password, nil
		}
		prevErr = werr
		if ctx.Err() != nil || errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
			return 0, "", appagent.NewError("supervisor.wait_ready", appagent.ErrKindRuntimeUnavailable, ctx.Err())
		}
		if errors.Is(werr, errServeSessionDied) {
			continue // 死亡轮换：无 kill 直接换端口。
		}
		// 健康超时：kill 门禁（clean → 轮换；非 clean → PendingCleanupError）。
		res, kerr := d.Proc.KillSession(sessionName)
		if kerr != nil {
			// C17 残余修复：KillSession 失败出口保留 res.CleanupTickets（部分
			// 会话收割产生的 tickets 随 res 返回；省略会使残余进程失去收割索引）。
			return 0, "", appagent.CleanupStageError("supervisor.serve_rotate", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Tickets: res.CleanupTickets, Retryable: true}, kerr)
		}
		cls := appagent.ClassifyKill(string(res.Disposition), res.SessionKilled)
		switch cls.Action {
		case "none":
			continue // clean → 轮换。
		default:
			return 0, "", appagent.CleanupStageError("supervisor.serve_rotate",
				appagent.CleanupPayload{SessionName: sessionName, Reason: cls.Reason, Tickets: res.CleanupTickets, Retryable: cls.Retryable},
				fmt.Errorf("rotate: disposition %s", res.Disposition))
		}
	}
	return 0, "", appagent.NewError("supervisor.wait_ready", appagent.ErrKindRuntimeUnavailable,
		fmt.Errorf("serve not ready after port retries: %v", prevErr))
}

// waitServeReadyOrDead 健康轮询 + 进程死亡判定（现状搬运：死亡优先于健康、
// 取消零副作用短路、墙钟耗尽 → health check timeout）。
func (d *SupervisorDeps) waitServeReadyOrDead(ctx context.Context, oc *Client, sessionName string) error {
	timeout := d.ReadyTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	poll := d.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		alive, derr := d.Proc.HasSession(sessionName)
		if derr == nil && !alive || errors.Is(derr, process.ErrNoTmuxServer) {
			return fmt.Errorf("%w: %s", errServeSessionDied, sessionName)
		}
		h, herr := oc.Health(ctx)
		if herr == nil && h.Healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("health check timeout")
}

// errServeSessionDied 进程死亡哨兵（与 task 层 errServeSessionDied 语义一致；
// C2b 收敛单点）。
var errServeSessionDied = errors.New("supervisor: serve session died")

// supProbeColdStartAttempts / supProbeColdStartBackoff 能力探测冷启动重试
// （现状 activate.go probeColdStartAttempts/defaultProbeColdStartBackoff 语义搬运：
// 首次 + 2 次重试，退避 2s/4s；测试注入 ProbeColdStartBackoff 覆盖退避序列）。
const supProbeColdStartAttempts = 3

func (d *SupervisorDeps) probeColdStartBackoff() []time.Duration {
	if d.ProbeColdStartBackoff != nil {
		return d.ProbeColdStartBackoff()
	}
	return []time.Duration{2 * time.Second, 4 * time.Second}
}

// probeWithColdStartRetry 能力探测（冷启动 ErrServeNotReady 短退避重试；其他错误
// 立即返回）。探测期间会话保活（不在重试内 kill——与 task 层现状一致）。
func probeWithColdStartRetry(ctx context.Context, oc *Client, backoff []time.Duration) error {
	var lastErr error
	for attempt := 0; attempt < supProbeColdStartAttempts; attempt++ {
		_, err := oc.Probe(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrServeNotReady) {
			return err
		}
		if attempt == supProbeColdStartAttempts-1 {
			break
		}
		timer := time.NewTimer(backoff[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("after %d attempts: %w", supProbeColdStartAttempts, lastErr)
}

// probeErrKind 将 Probe 失败映射为 typed error kind（现状 probeErrToOpCode 对应
// 规则：mismatch → binding 不可用（结构漂移 fail-closed）；unauthorized → internal
// bug 形态（凭据派生错误）；其余（网络/超时）→ runtime unavailable）。
func probeErrKind(err error) appagent.ErrorKind {
	switch {
	case errors.Is(err, ErrCapabilityMismatch), errors.Is(err, ErrUnauthorized):
		return appagent.ErrKindBindingInvalid
	default:
		return appagent.ErrKindRuntimeUnavailable
	}
}

// confirmTerminated 按既有 KillResult 规则确认 runtime 终止（双启动第②步）。
func (d *SupervisorDeps) confirmTerminated(ctx context.Context, taskID, sessionName string) error {
	exists, herr := d.Proc.HasSession(sessionName)
	if herr != nil && !errors.Is(herr, process.ErrNoTmuxServer) {
		return appagent.CleanupStageError("supervisor.anchor_stage", appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Retryable: true}, fmt.Errorf("confirm runtime terminated: has session: %w", herr))
	}
	if !exists {
		return nil
	}
	res, kerr := d.Proc.KillSession(sessionName)
	if kerr != nil {
		return appagent.CleanupStageError("supervisor.confirm_terminated",
			appagent.CleanupPayload{SessionName: sessionName, Reason: appagent.KillReasonKillFailed, Tickets: res.CleanupTickets, Retryable: true},
			kerr)
	}
	cls := appagent.ClassifyKill(string(res.Disposition), res.SessionKilled)
	switch cls.Action {
	case "none":
		return nil
	default:
		return appagent.CleanupStageError("supervisor.confirm_terminated",
			appagent.CleanupPayload{SessionName: sessionName, Reason: cls.Reason, Tickets: res.CleanupTickets, Retryable: cls.Retryable},
			fmt.Errorf("confirm runtime terminated: disposition %s", res.Disposition))
	}
}

// checkDeps 校验 deps 完整与 TaskRef 形状。
func (d *SupervisorDeps) checkDeps(task appagent.TaskRef) error {
	switch {
	case d.Proc == nil, d.Store == nil, d.SideFx == nil, d.AllocatePort == nil:
		return appagent.NewError("supervisor", appagent.ErrKindCapabilityUnavailable,
			errors.New("supervisor deps 未注入（占位态；生产装配经 NewSPIProviderWithDeps）"))
	case task.TaskID == "" || task.Directory == "":
		return appagent.NewError("supervisor", appagent.ErrKindBindingInvalid,
			errors.New("TaskRef 缺少 TaskID/Directory"))
	}
	return nil
}

// copyEnv 拷贝 env map（nil → 空 map）。
func copyEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}
