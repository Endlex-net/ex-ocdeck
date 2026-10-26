// status.go 实现 StatusReader 端口（opencode-v2-provider tasks 4.3）：
//   - 未验证期（默认）：Current 返回空 map + nil error，并经能力健康透出端口
//     上报 status 能力 CapUnknown（不得推导 idle/busy、不得用 CapUnsupported
//     ——D8-0 精确规则）；
//   - 实证通过（1.2 PASS）后按冻结映射切换并转为 CapAvailable（D8-1）：
//     busy = session.execution.started；succeeded/failed/interrupted 均为
//     执行终态 → idle，Session.Info.outcome 仅诊断不映射；未知枚举跳过。
//
// 切换经 ProviderOptions.StatusVerified 显式装配决策（D8-0 保守规则在切换前
// 适用）；事件侧状态由事件路径经 recordStatus 维护。
package opencode2

import (
	"context"

	appagent "ocdeck/internal/application/agentprovider"
	domain "ocdeck/internal/domain/agentprovider"
)

type spiStatus struct{ p *SPIProvider }

// Current 返回状态快照：保守态恒空 map + nil error + CapUnknown 上报（消费方
// 对空 map MUST 跳过任务状态断言）；实证态返回事件侧追踪的 execution 边界
// 状态（冻结映射），上报 CapAvailable。
func (a *spiStatus) Current(ctx context.Context, b domain.TaskBinding) (map[domain.SessionKey]domain.AgentStatus, error) {
	h := a.p.healthBase(b, appagent.PrimitiveStatusReader, appagent.ScopeRuntime, nil)
	if _, _, err := a.p.resolveBinding(b); err != nil {
		a.p.reportConversion(h, "status.current", err)
		return nil, err
	}
	if !a.p.statusVerified {
		// 未验证期精确规则（D8-0）：空快照 + nil error + CapUnknown。
		if a.p.health != nil {
			a.p.health.report(appagent.CapabilityHealth{
				ProviderID: ProviderID, TaskID: bindingHealthTaskID(b),
				Primitive: appagent.PrimitiveStatusReader, Scope: appagent.ScopeRuntime,
				State:  appagent.CapUnknown,
				Reason: "status.current: 等价能力未验证（保守空快照，不得推导 idle/busy）",
			})
		}
		return map[domain.SessionKey]domain.AgentStatus{}, nil
	}
	a.p.statusMu.Lock()
	out := make(map[domain.SessionKey]domain.AgentStatus, len(a.p.status))
	for id, state := range a.p.status {
		if state != domain.StatusIdle && state != domain.StatusBusy {
			continue // 未知枚举跳过（冻结映射仅 busy/idle）
		}
		out[v2Namespace(id)] = state
	}
	a.p.statusMu.Unlock()
	a.p.reportConversion(h, "status.current", nil)
	return out, nil
}
