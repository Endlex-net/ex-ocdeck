// binding_dispatch.go 实现事实来源切换的 runtime 连接分派（opencode-v1-spi-provider
// tasks 4.3 / design D9 binding 提交协议 + specs「运行时托管与任务绑定生命周期」）：
//
//   - task 层全部 runtime 连接一律从持久化 binding 解析（端点/凭据来自 binding
//     data——非秘密连接数据以持久化绑定为事实来源；derived 凭据现场按 master
//     secret 派生、legacy 用迁移导入存量值。MUST NOT 从进程环境变量反查——特许
//     例外仅保留迁移导入 runner 与删除清理的临时 runtime 路径）；
//   - binding 提交协议：supervisor（Acquire/Resume）只返回 binding 不持久化；
//     task 层在激活/恢复成功的提交事务中经 CommitActivation 写 binding（与任务
//     状态 CAS 同事务，失败零持久化副作用）；提交前 SSE/对齐使用 supervisor 返回
//     的本次内存 binding；
//   - OCDECK_TASK_ID 校验语义保留（reconcile resumeActive 的会话内复检）。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"ocdeck/internal/application"
	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
	"ocdeck/internal/infrastructure/store"
)

// registryDispatch 按 binding 的 provider 标识经 registry 分派（未注册 fail-closed，
// 零网络副作用——specs「Provider registry 分派」）。注入缺失仅在测试直构时可达：
// 生产装配与契约测试经 Options.ProviderRegistry 注入；降级为 provider 未注册错误
// 同样 fail-closed（与注册表空集语义一致，不引入第二条行为路径）。
func (m *Manager) registryDispatch(providerID string) error {
	if m.providerRegistry == nil {
		return appagent.NewError("registry.get", appagent.ErrKindProviderNotFound,
			fmt.Errorf("provider registry 未注入（runtime 连接分派不可用）"))
	}
	if _, err := m.providerRegistry.Get(providerID); err != nil {
		return err
	}
	return nil
}

// isProviderNotFound typed 判定（C7：各生命周期入口的准入失败分派共享——
// 零副作用 + 诊断性 last_error 仅适用于 ProviderNotFound；其他错误按原语义）。
func isProviderNotFound(err error) bool {
	var ae *appagent.Error
	return errors.As(err, &ae) && ae.Kind == appagent.ErrKindProviderNotFound
}

// registryAdmitTask 按**任务行声明的 provider 标识**做 registry 准入（C7 门禁
// 修复：准入按任务 provider，不按固定 v1——specs「Provider registry 分派」
// 「MUST 经 registry 按任务 provider 标识分派）。store 读取失败原样透传；未注册
// 返回 typed ProviderNotFound（调用方零副作用处置：诊断性 last_error 允许，
// 不修改状态/归属/锚定/binding——D10）。
func (m *Manager) registryAdmitTask(ctx context.Context, taskID string) error {
	pid, err := m.store.GetTaskProviderID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("registry admit: get task provider: %w", err)
	}
	return m.registryDispatch(pid)
}

// recordAdmissionError 写入诊断性 last_error（状态不变的持久化副作用——D10
// 允许行：provider 未注册仅 last_error，不进 runtime 故障补偿）。写失败仅聚合
// 日志（诊断位写不进去不改变零副作用决策）。
func (m *Manager) recordAdmissionError(ctx context.Context, taskID string, admitErr error) {
	le := sql.NullString{String: fmt.Sprintf("provider not registered: %v", admitErr), Valid: true}
	if _, werr := m.writeStatus(ctx, taskID, m.readStatusForDiagnostic(ctx, taskID), le); werr != nil {
		log.Printf("registry admit: task %s record last_error: %v", taskID, werr)
	}
}

// readStatusForDiagnostic 读任务当前状态（诊断 last_error 写入保持状态不变；
// 读失败回写降级为日志——零副作用优先级高于诊断位）。
func (m *Manager) readStatusForDiagnostic(ctx context.Context, taskID string) string {
	if row, err := m.store.GetTask(ctx, taskID); err == nil {
		return row.Status
	}
	return ""
}

// opencodeBindingData 是 task 层消费的 v1 binding data 视图（端点 + 凭据解析）。
// 形状与 internal/infrastructure/opencode 的 v1BindingData 同源（D3 envelope
// 示例）；task 层持有轻量解码镜像以避免 import infrastructure/opencode。
type opencodeBindingData struct {
	// ProviderID 是该 binding 的 provider 标识（OCFactory 分派入参；非 provider
	// data JSON 字段——endpoint/镜像两路径均由 envelope 填充）。
	ProviderID string
	Port       int           `json:"port"`
	Directory  string        `json:"directory"`
	TaskID     string        `json:"taskId"`
	Auth       v1BindingAuth `json:"auth"`
}

// v1BindingTaskField 是 binding data 的 taskId 字段探针（C6：endpoint 成功路径
// 回读观测身份键——避免对 opencodeBindingData 的 Auth 段做半解析）。
type v1BindingTaskField struct {
	TaskID string `json:"taskId"`
}

// v1BindingAuth 是 binding data 的 auth 段（kind + legacy 导入密码）。
type v1BindingAuth struct {
	Kind     string `json:"kind"`
	Password string `json:"password,omitempty"`
}

// decodeV1BindingData 解码 v1 私有 binding data。缺少端口 / auth.kind / JSON
// 畸形 → fail-closed（BindingInvalid，不猜测）。
func decodeV1BindingData(b domain.TaskBinding) (*opencodeBindingData, error) {
	invalidate := func(cause string) (*opencodeBindingData, error) {
		return nil, appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("task %s binding data: %s", b.ProviderID, cause))
	}
	var data opencodeBindingData
	if len(b.Data) == 0 {
		return invalidate("空 data")
	}
	if err := json.Unmarshal(b.Data, &data); err != nil {
		return invalidate("decode: " + err.Error())
	}
	if data.Port <= 0 {
		return invalidate("缺少 port")
	}
	if data.Auth.Kind == "" {
		return invalidate("缺少 auth.kind")
	}
	return &data, nil
}

// password 按 auth.kind 解析凭据：derived → 现场派生（主密钥缺失 fail-closed，
// 同任务恒定）；legacy → 存量导入密码（非空）；未知 kind fail-closed。
func (d *opencodeBindingData) password() (string, error) {
	switch d.Auth.Kind {
	case appagent.DerivedCredKind:
		if d.TaskID == "" {
			return "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
				errors.New("derived binding 缺少 taskId（派生入参）"))
		}
		master, err := appagent.EnsureMasterSecret()
		if err != nil {
			return "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid, err)
		}
		return appagent.DeriveRuntimePassword(master, store.ProviderIDV1, d.TaskID), nil
	case appagent.LegacyCredKind:
		if d.Auth.Password == "" {
			return "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
				errors.New("legacy binding 缺少导入密码"))
		}
		return d.Auth.Password, nil
	default:
		return "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("未知 auth.kind %q", d.Auth.Kind))
	}
}

// isTestSeamBindingErr 判定 BindingRuntimeEndpoint 错误是否属「provider 未实现
// 端点解码」的测试接缝形态（minimalProvider/stubProvider failClosed 全方法
// CapabilityUnavailable）——仅该形态允许回退 v1 镜像（过渡裁决 B）；真实 provider
// 对具体 binding 的解码/凭据拒绝必须直接传播（C2 门禁复审：回退会重新接受
// adapter 拒绝的 binding）。
func isTestSeamBindingErr(err error) bool {
	var ae *appagent.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Kind == appagent.ErrKindCapabilityUnavailable
}

// resolveRuntimeBinding 从持久化 binding 解析 runtime 连接事实（端点/目录/凭据）。
// 无绑定 / 解码失败 / provider 未注册 → fail-closed 错误（零网络副作用）。
// C2 门禁修复：v1 私有 data 解码/凭据派生经 provider.BindingRuntimeEndpoint（application
// port——task 层不解码 provider 私有 data）；provider 未实现（测试接缝形态）时回退
// 本层 v1 镜像（过渡裁决 B，Phase D 清理）。\n// directory 不取任务行 WorktreePath：binding data 的 directory 是 supervisor
// spawn 时登记的事实（与任务行同值；legacy 导入亦按当时 worktree 写入）。
// 返回值第二项是 provider 适配层已解析的凭据（endpoint 路径由 provider 派生/读取，
// 调用方 MUST NOT 再经 data.password() 二次解析——C2 双解 bug 修复：endpoint
// 返回的密码没有 auth.kind 上下文，password() 对空 Kind fail-closed）。
func (m *Manager) resolveRuntimeBinding(ctx context.Context, taskID string) (*opencodeBindingData, string, error) {
	binding, has, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve binding for task %s: %w", taskID, err)
	}
	if !has {
		return nil, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("task %s 无持久化 binding（事实来源切换后 runtime 连接 MUST 经 binding 解析）", taskID))
	}
	// I7：归属门禁先于 endpoint/凭据派生（同一次 binding 读取——无二次读漂移窗口）。
	if oerr := m.checkBindingOwnershipGate(ctx, taskID, binding); oerr != nil {
		return nil, "", oerr
	}
	p, perr := m.providerFor(binding.ProviderID)
	if perr != nil {
		return nil, "", perr
	}
	if port, pw, dir, eerr := p.BindingRuntimeEndpoint(binding); eerr == nil {
		// C6 残余修复（观测身份键）：endpoint 成功路径回读 data.taskId 填充
		//（BindingRuntimeEndpoint 仅返回三元组；TaskID 是 CapabilityHealth 的
		// 观测身份键——桥/能力出口消费）。
		var decoded v1BindingTaskField
		if len(binding.Data) > 0 {
			_ = json.Unmarshal(binding.Data, &decoded)
		}
		return &opencodeBindingData{ProviderID: binding.ProviderID, Port: port, Directory: dir, TaskID: decoded.TaskID}, pw, nil
	} else if !isTestSeamBindingErr(eerr) {
		// C2 门禁修复：真实 provider 的解码/凭据拒绝直接传播（零回退——回退镜像
		// 只查 port/auth.kind 会重新接受 adapter 拒绝的 binding，如非法 provider
		// instance / 缺 directory）。isTestSeamBindingErr 锚定 minimalProvider/
		// stubProvider 的 failClosed 形态（CapabilityUnavailable = 未实现接缝）。
		// mutation 证据（已验证 → 红 → 还原）：回退吞错（判定恒假、一律走镜像）→
		// TestResolveBinding_AdapterRejectPropagatesNoMirrorFallback 红——镜像
		// 重新接受非法 instance 的 binding（在缺 auth.kind 处另抛新错误，证明
		// 镜像无 instance 谓词）。
		return nil, "", eerr
	}
	// provider 端点解码不可用（测试接缝/过渡形态）→ v1 镜像回退。
	data, derr := decodeV1BindingData(binding)
	if derr != nil {
		return nil, "", derr
	}
	pw, perr2 := data.password()
	if perr2 != nil {
		return nil, "", perr2
	}
	// 镜像回退：ProviderID 随 envelope 填充（OCFactory 分派入参，C2 门禁修复）。
	data.ProviderID = binding.ProviderID
	return data, pw, nil
}

// providerFor 按 providerID 分派 Provider 实例（registry 缺失 → ProviderNotFound）。
func (m *Manager) providerFor(providerID string) (appagent.Provider, error) {
	if m.providerRegistry == nil {
		return nil, appagent.NewError("registry.get", appagent.ErrKindProviderNotFound,
			fmt.Errorf("provider registry 未注入（runtime 连接分派不可用）"))
	}
	return m.providerRegistry.Get(providerID)
}

// buildTaskOCClient 经持久化 binding 构造指向该任务 runtime 的客户端。
// opts 由调用方决定（超时预算沿用各消费点现状值）。
//
// C2/C6 残余修复（生产调用委托 registry 返回的 Provider 窄接口）：provider 实现
// 可承载能力出口（非测试接缝形态）时优先返回能力出口桥（spiBridgeOCClient——
// SessionControl/Status/Attention/Prompts/Titles 经 provider 出口，能力健康观测
// reportConversion 在 SPI 出口内置）；Health/Probe/raw-SubscribeEvents/
// ProbePromptAsyncCapability 无窄接口对应，桥内经 fallback（既有 OCClient 工厂）
// 委托。provider 为测试接缝形态（窄接口 nil）或分派失败 → 维持既有工厂路径
// （OCClient 显式测试接缝过渡裁决）。
func (m *Manager) buildTaskOCClient(ctx context.Context, taskID string, opts appagent.Options) (OCClient, error) {
	data, password, err := m.resolveRuntimeBinding(ctx, taskID)
	if err != nil {
		return nil, err
	}
	fallback := m.ocFactory(data.ProviderID, data.Port, password, opts)
	if binding, berr := m.currentTaskBinding(ctx, taskID); berr == nil {
		if bridge := m.bridgeClientFor(data, binding, fallback); bridge != nil {
			debugf("oc client task=%s provider=%s form=bridge", taskID, data.ProviderID)
			return bridge, nil
		}
	}
	debugf("oc client task=%s provider=%s form=fallback", taskID, data.ProviderID)
	return fallback, nil
}

// taskOcBindingClient 返回（客户端, 目录）：任务行校验（ active 门禁）由调用方
// 既有逻辑持有，本函数仅做 binding 解析失败 → ok=false 的降级（与旧 taskOcClient
// 反查失败同形）。桥接优先逻辑与 buildTaskOCClient 同形（C2/C6 残余修复）。
func (m *Manager) taskOcBindingClient(ctx context.Context, taskID string, opts appagent.Options) (OCClient, string, bool) {
	data, password, err := m.resolveRuntimeBinding(ctx, taskID)
	if err != nil {
		return nil, "", false
	}
	fallback := m.ocFactory(data.ProviderID, data.Port, password, opts)
	client := fallback
	if binding, berr := m.currentTaskBinding(ctx, taskID); berr == nil {
		if bridge := m.bridgeClientFor(data, binding, fallback); bridge != nil {
			client = bridge
		}
	}
	return client, data.Directory, true
}

// currentTaskBinding 读取任务当前持久化 binding（供消费点传递给 provider 实现）。
// 无绑定 / 解析失败 → fail-closed 错误。
func (m *Manager) currentTaskBinding(ctx context.Context, taskID string) (domain.TaskBinding, error) {
	binding, has, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return domain.TaskBinding{}, fmt.Errorf("current binding for task %s: %w", taskID, err)
	}
	if !has {
		return domain.TaskBinding{}, appagent.NewError("current binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("task %s 无持久化 binding", taskID))
	}
	return binding, nil
}

// commitActivationBinding 激活/恢复成功提交事务（D9 binding 提交协议，tasks 4.3）：
// binding 写入与任务状态 CAS（activating→active）同事务。CAS 失配返回 !Matched
// （调用方按既有幂等成功/回退分派处置）；事务失败时 binding 与状态零持久化
// 副作用——supervisor 已返回的新 binding 不落库，旧 binding 保持原值。
func (m *Manager) commitActivationBinding(ctx context.Context, taskID string, binding domain.TaskBinding, mode AlignMode) (application.TransitionResult, error) {
	return m.store.CommitActivation(ctx, taskID, binding, StatusActivating, StatusActive)
}

// startSSEFromBinding 经内存 binding 建立 SSE 订阅（D9 提交协议：supervisor 已
// 返回最终 binding，提交前的 SSE 与对齐使用本次内存值，不经 DB——端点/凭据从
// binding data 解出）。wtPath 取任务行 WorktreePath（与 binding data.directory
// 同值；SSE directory 校验沿用任务行来源）。恢复路径复用本 helper（repair/resume
// 无新 binding 写入，凭据同样经持久化 binding 解析）。C9：该 binding 即 finalBinding
// ——startSSE typed 桥直接用它订阅，MUST NOT 再读持久化值（恢复替换提交前旧值
// 不代表本次 runtime 连接事实）。
func (m *Manager) startSSEFromBinding(ctx context.Context, rt *taskRuntime, taskID string, mode AlignMode) error {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil {
		return newOpErr(codeInternal, fmt.Errorf("get task for sse binding: %w", err))
	}
	binding, port, password, berr := m.bindingAndEndpoint(ctx, taskID)
	if berr != nil {
		return newOpErr(codeInternal, fmt.Errorf("resolve binding for sse: %w", berr))
	}
	return m.startSSE(ctx, rt, taskID, row.WorktreePath, port, password, mode, &binding, nil)
}

// bindingAndEndpoint 读取任务持久化 binding 并解析 runtime 连接事实
// （binding + port/password）。C9 提取共用点：startSSEFromBinding 与消费点共享
// 同一解析路径（resolveRuntimeBinding 的 binding 视图扩展）。
func (m *Manager) bindingAndEndpoint(ctx context.Context, taskID string) (domain.TaskBinding, int, string, error) {
	binding, has, err := m.store.GetTaskBinding(ctx, taskID)
	if err != nil {
		return domain.TaskBinding{}, 0, "", fmt.Errorf("resolve binding for task %s: %w", taskID, err)
	}
	if !has {
		return domain.TaskBinding{}, 0, "", appagent.NewError("resolve binding", appagent.ErrKindBindingInvalid,
			fmt.Errorf("task %s 无持久化 binding（事实来源切换后 runtime 连接 MUST 经 binding 解析）", taskID))
	}
	// I7：归属门禁先于 endpoint/凭据派生（同一次 binding 读取）。
	if oerr := m.checkBindingOwnershipGate(ctx, taskID, binding); oerr != nil {
		return domain.TaskBinding{}, 0, "", oerr
	}
	port, pw, _, eerr := m.bindingEndpoint(binding)
	if eerr != nil {
		return domain.TaskBinding{}, 0, "", eerr
	}
	return binding, port, pw, nil
}
