// v2_dispatch.go 实现单一 resolver 的执行分派接缝（opencode-v2-provider
// tasks 6.3/6.5/6.6 / design D4 替换表）：Manager 增加按任务解析 supervisor 的
// 接缝——activateRun/runRecoveryAttempt/删除临时绑定分支经 resolver + registry
// 按任务执行身份取 provider.Supervisor()；v1 任务解析到同一 v1 实例（行为等价）。
// 准入拒绝（fail-closed）零副作用：诊断性 last_error 允许，不改状态/归属/锚定。
package task

import (
	"context"
	"errors"
	"fmt"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// MachineFacts 是 resolver 机器事实（resolver.machineFacts 的导出别名，
// main.go 装配用）。
type MachineFacts = machineFacts

// SetMachineFacts 注入机器事实（main.go 装配：config.OpenCodeMajor/V2Adoption；
// 测试直构）。
func (m *Manager) SetMachineFacts(mf MachineFacts) { m.machine = mf }

// taskExecutionDecision 读任务事实（binding + 钉扎）并产出 resolver 决策。
// store 读取失败原样透传。G3-F9：一次读取，binding 事实随决策一并返回
// （调用方复用同一事实做归属校验——消除二次读取与两次事实不一致）。
func (m *Manager) taskExecutionDecision(ctx context.Context, taskID string) (execDecision, domain.TaskBinding, bool, error) {
	pinned, err := m.store.GetTaskProviderID(ctx, taskID)
	if err != nil {
		return execDecision{}, domain.TaskBinding{}, false, fmt.Errorf("resolver: get task provider: %w", err)
	}
	binding, hasBinding, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return execDecision{}, domain.TaskBinding{}, false, fmt.Errorf("resolver: get task binding: %w", err)
	}
	bindingProviderID := ""
	if hasBinding {
		bindingProviderID = binding.ProviderID
	}
	return resolveExecutionPath(bindingProviderID, pinned, m.machine), binding, hasBinding, nil
}

// admitExecution 准入并解析执行 supervisor（激活/恢复统一入口；design D4）。
// 拒绝（R2/R3/R6/R7/R9 及数据异常）→ typed 错误（零副作用；调用方按既有
// admission 处置写 last_error/用户提示）。已有 v2 binding 的 R1 路径先执行
// BindingOwnershipChecker（G3-F5：归属校验先于 supervisor 的任何网络/进程
// 副作用与凭据派生）；provider 未实现该窄接口 → fail-closed。registry 未注入
// → 回退固定 v1 supervisor（既有装配/测试现状，行为不变）。
// 评审 I7：binding 事实（同次决策读取）随返回值透出——调用方分派复用，MUST NOT
// 二次读取（漂移窗口）。
func (m *Manager) admitExecution(ctx context.Context, taskID string) (appagent.RuntimeSupervisor, execDecision, domain.TaskBinding, bool, error) {
	dec, binding, hasBinding, err := m.taskExecutionDecision(ctx, taskID)
	if err != nil {
		return nil, execDecision{}, domain.TaskBinding{}, false, err
	}
	if dec.DenyKind != "" {
		return nil, dec, binding, hasBinding, appagent.NewError("resolver.admit", dec.DenyKind, errors.New(dec.DenyReason))
	}
	if dec.ExecProviderID == ocdeckstore.ProviderIDV1 {
		// v1 决策 → 注入的 v1 supervisor 实例（D4 替换表「同一 v1 实例，行为
		// 等价」——生产装配 SetRuntimeSupervisor(v1Provider.Supervisor())、
		// 测试注入 fake；registry 不参与 v1 解析）。
		return m.runtimeSupervisor, dec, binding, hasBinding, nil
	}
	// v2 决策 → 经 registry（v2 provider 未注册 = fail-closed）。
	if m.providerRegistry == nil {
		return nil, dec, binding, hasBinding, appagent.NewError("resolver.admit", appagent.ErrKindProviderNotFound,
			fmt.Errorf("执行 provider %s 未注册（registry 未装配）", dec.ExecProviderID))
	}
	p, err := m.providerRegistry.Get(dec.ExecProviderID)
	if err != nil {
		return nil, dec, binding, hasBinding, err
	}
	// G3-F5/G3-F9：已有 v2 binding → 归属校验先于 supervisor 副作用（网络/进程/
	// 凭据）——binding 事实复用决策读取（hasBinding==false 才跳过；读取错误已在
	// decision 路径 fail-closed 透传，不得静默降级为「无 v2 binding」）。
	if hasBinding && binding.ProviderID == ocdeckstore.ProviderIDV2 {
		checker, ok := p.(appagent.BindingOwnershipChecker)
		if !ok {
			return nil, dec, binding, hasBinding, appagent.NewError("resolver.admit", appagent.ErrKindBindingInvalid,
				fmt.Errorf("provider %s 未实现 BindingOwnershipChecker（fail-closed 拒绝使用其 binding）", p.Meta().ProviderID))
		}
		trow, derr := m.store.GetTask(ctx, taskID)
		if derr != nil {
			return nil, dec, binding, hasBinding, derr
		}
		if err := checker.CheckBindingOwnership(binding, taskID, trow.WorktreePath); err != nil {
			return nil, dec, binding, hasBinding, err
		}
	}
	return p.Supervisor(), dec, binding, hasBinding, nil
}

// supervisorForTaskConsumption 消费点路由（R4/R12：恒按 binding ProviderID 经
// registry；已承接任务不因钉扎/开关/探测失败转回 v1）。v2 binding 消费前统一
// 执行 BindingOwnershipChecker 门禁（G2-F5：provider 未实现 → fail-closed；
// 归属校验先于任何凭据派生——BindingRuntimeEndpoint 返回值含凭据，MUST NOT
// 用于本校验）。
func (m *Manager) supervisorForTaskConsumption(ctx context.Context, taskID string, b domain.TaskBinding, expectedDirectory string) (appagent.RuntimeSupervisor, error) {
	if m.providerRegistry == nil {
		return nil, appagent.NewError("registry.get", appagent.ErrKindProviderNotFound,
			fmt.Errorf("provider registry 未注入（消费点分派不可用）"))
	}
	p, err := m.providerRegistry.Get(b.ProviderID)
	if err != nil {
		return nil, err
	}
	checker, ok := p.(appagent.BindingOwnershipChecker)
	if !ok {
		return nil, appagent.NewError("registry.ownership", appagent.ErrKindBindingInvalid,
			fmt.Errorf("provider %s 未实现 BindingOwnershipChecker（fail-closed 拒绝使用其 binding）", b.ProviderID))
	}
	// 归属校验先于凭据派生（阶段一端口——实现方不派生凭据）。
	if err := checker.CheckBindingOwnership(b, taskID, expectedDirectory); err != nil {
		return nil, err
	}
	return p.Supervisor(), nil
}

// checkBindingOwnershipGate 是任务级 binding 消费的统一归属门禁（评审 I7）：
// 一次读取 binding 后、endpoint/client 提供与凭据派生前执行——v2 binding 强制
// CheckBindingOwnership（provider 未实现 fail-closed 拒绝，形态同
// supervisorForTaskConsumption）；其他命名空间维持既有行为（v1 无该窄接口，
// 行为不变）。
func (m *Manager) checkBindingOwnershipGate(ctx context.Context, taskID string, b domain.TaskBinding) error {
	if b.ProviderID != ocdeckstore.ProviderIDV2 {
		return nil
	}
	trow, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("ownership gate: get task: %w", err)
	}
	p, err := m.providerFor(b.ProviderID)
	if err != nil {
		return err
	}
	checker, ok := p.(appagent.BindingOwnershipChecker)
	if !ok {
		return appagent.NewError("registry.ownership", appagent.ErrKindBindingInvalid,
			fmt.Errorf("provider %s 未实现 BindingOwnershipChecker（fail-closed 拒绝使用其 binding）", b.ProviderID))
	}
	return checker.CheckBindingOwnership(b, taskID, trow.WorktreePath)
}

// stopV2ServerMonitoring 停止 v2 supervisor 的 server 监控循环（评审 I9 生命
// 周期：persist/kill 两种关停策略都必须停止——persist 不调 ShutdownServer，但
// 关停后监控不得继续访问 store/重拉）。join（取消在途重试并等待退出）后调用方
// 才允许关 store。v2 未注册（开关关）→ no-op。
func (m *Manager) stopV2ServerMonitoring() {
	if m.providerRegistry == nil {
		return
	}
	p, err := m.providerRegistry.Get(ocdeckstore.ProviderIDV2)
	if err != nil {
		return // v2 未注册：无共享 server（开关关形态）
	}
	if ctl, ok := p.Supervisor().(appagent.ServerMonitorController); ok {
		ctl.StopServerMonitoring()
	}
}

// serverShutdownHook 是 v2 supervisor 单 owner 关停能力（V2Supervisor 满足；
// persist 策略不调用——server 保留）。
type serverShutdownHook interface {
	ShutdownServer(ctx context.Context) error
}

// shutdownV2Server kill 策略关停下由 supervisor 单 owner 关闭全局共享 server
// （一次；D4 shutdown 行）。v2 未注册（开关关）→ 无 server 可关（no-op）。
func (m *Manager) shutdownV2Server(ctx context.Context) error {
	if m.providerRegistry == nil {
		return nil
	}
	p, err := m.providerRegistry.Get(ocdeckstore.ProviderIDV2)
	if err != nil {
		return nil // v2 未注册：无共享 server（开关关形态）
	}
	hook, ok := p.Supervisor().(serverShutdownHook)
	if !ok {
		return nil // 非 v2 承载：无单 owner 关停面
	}
	return hook.ShutdownServer(ctx)
}

// RebuildV2Associations 是 v2 server 重拉后的任务关联重建编排（tasks 5.3 /
// design D3「重拉后活跃任务按 binding 重建 session 关联」；main.go 装配为
// supervisor OnServerRestarted 回调）：遍历运行时注册表快照，对 active 的 v2
// 任务复用既有恢复入口 ensureRecovery——R1 经 Resume 延续原 session
// （MUST NOT 重复建 session）；非 active/非 v2 任务不触碰。并发触发/恢复在途
// 去重由 ensureRecovery 的任务锁 + incident 闸门承担，不在此重复。
func (m *Manager) RebuildV2Associations() {
	m.rtMu.Lock()
	snap := make(map[string]*taskRuntime, len(m.runtimes))
	for id, rt := range m.runtimes {
		snap[id] = rt
	}
	m.rtMu.Unlock()
	ctx := m.lifecycleCtx()
	for taskID, rt := range snap {
		row, err := m.store.GetTask(ctx, taskID)
		if err != nil || row.Status != StatusActive {
			continue
		}
		b, has, err := m.store.GetTaskBinding(ctx, taskID)
		if err != nil || !has || b.ProviderID != ocdeckstore.ProviderIDV2 {
			continue
		}
		tok := rt.instVersion
		go m.ensureRecovery(taskID, tok)
	}
}
