// resolver.go 实现单一 resolver 决策表（opencode-v2-provider tasks 6.3/6.6 /
// design D4）：激活/恢复准入统一按 R1–R12 自上而下首个命中生效。纯函数 +
// Manager 装配读取，错误分类 R2/R6/R9→ErrKindCapabilityUnavailable、
// R3/R7→ErrKindProviderNotFound；已有 binding 优先于钉扎判定（R1–R3）；
// 消费点恒按 binding 路由（R4/R12，不经本表——registryDispatch 既有路径）。
package task

import (
	appagent "ocdeck/internal/application/agentprovider"
	ocdeckstore "ocdeck/internal/infrastructure/store"
)

// machineFacts 是 resolver 的机器事实（装配期注入：config.OpenCodeMajor/
// config.V2Adoption；测试直构）。
type machineFacts struct {
	// MachineMajor 机器实装 opencode 主版本；0 = unknown（探测/解析失败）。
	MachineMajor int
	// V2Enabled 承接开关事实（装配期取自 config.V2Adoption：未配置=开，
	// 显式 0/false/off=关；零值 false 对应显式关闭形态）。
	V2Enabled bool
}

// execDecision 是 resolver 决策结果。
type execDecision struct {
	// ExecProviderID 是执行 provider（v1/v2 命名空间常量）。
	ExecProviderID string
	// Adopt 标记承接路径（R8：D5 主流程）。
	Adopt bool
	// DenyKind 非 nil-Kind 时为 fail-closed 分类（零副作用 + 用户可感知提示）。
	DenyKind appagent.ErrorKind
	// DenyReason 是 fail-closed 的用户可感知原因（R9 附显式关闭说明与恢复方式）。
	DenyReason string
}

// resolveExecutionPath 按 D4 决策表返回激活/恢复准入决策（自上而下首个命中）。
// bindingProviderID 为空表示无 binding（存量任务/新任务）；pinnedProviderID 为
// 任务行钉扎值（tasks.provider_id，恒非空——回填门禁保证）。
func resolveExecutionPath(bindingProviderID, pinnedProviderID string, mf machineFacts) execDecision {
	dec := resolveExecutionPathTable(bindingProviderID, pinnedProviderID, mf)
	debugf("resolver: binding=%q pinned=%q machineMajor=%d v2Enabled=%t -> exec=%q adopt=%t denyKind=%v denyReason=%q",
		bindingProviderID, pinnedProviderID, mf.MachineMajor, mf.V2Enabled, dec.ExecProviderID, dec.Adopt, dec.DenyKind, dec.DenyReason)
	return dec
}

func resolveExecutionPathTable(bindingProviderID, pinnedProviderID string, mf machineFacts) execDecision {
	const v1 = ocdeckstore.ProviderIDV1
	const v2 = ocdeckstore.ProviderIDV2
	machineV2 := mf.MachineMajor >= 2
	hasV2Binding := bindingProviderID == v2

	// R1–R3：已有 v2 binding 优先于钉扎判定（消费点 R4 恒按 binding 路由，
	// 不经激活 resolver）。
	if hasV2Binding {
		switch {
		case machineV2 && mf.V2Enabled:
			return execDecision{ExecProviderID: v2} // R1：v2 恢复（延续原 session）
		case !machineV2:
			// R2：机器 1 或 unknown → fail-closed（该检查优先于开关判定）。
			return execDecision{DenyKind: appagent.ErrKindCapabilityUnavailable,
				DenyReason: "任务为 v2 执行身份，但本机 opencode 主版本非 2（或探测失败）"}
		default:
			// R3：开关关 → v2 未注册，MUST NOT 转 v1。
			return execDecision{DenyKind: appagent.ErrKindProviderNotFound,
				DenyReason: "任务为 v2 执行身份，但承接开关关闭（v2 provider 未注册）"}
		}
	}

	switch {
	case pinnedProviderID == v2:
		// R5–R7（自上而下：R6 版本拒绝优先于 R7 开关拒绝）。
		if machineV2 {
			if mf.V2Enabled {
				return execDecision{ExecProviderID: v2} // R5：v2 执行
			}
			// R7：开关关 → v2 未注册。
			return execDecision{DenyKind: appagent.ErrKindProviderNotFound,
				DenyReason: "任务钉扎 opencode-v2，但承接开关关闭（v2 provider 未注册）"}
		}
		// R6：机器 1 或 unknown → fail-closed（优先于 R7）。
		return execDecision{DenyKind: appagent.ErrKindCapabilityUnavailable,
			DenyReason: "任务钉扎 opencode-v2，但本机 opencode 主版本非 2（或探测失败）"}

	case pinnedProviderID == v1:
		// R8–R11：v1 钉扎。
		if machineV2 {
			if mf.V2Enabled {
				// R8：承接（执行 provider=v2，钉扎值不变；A/B 分支由历史判定矩阵）。
				return execDecision{ExecProviderID: v2, Adopt: true}
			}
			// R9：开关显式关 + 机器 v2 + v1 钉扎 → fail-closed 不静默 v1
			//（v1 provider 无法驱动 v2 二进制）+ 提示显式关闭状态与恢复方式。
			return execDecision{DenyKind: appagent.ErrKindCapabilityUnavailable,
				DenyReason: "本机 opencode 已升级到 v2 且任务钉扎 v1：承接开关已被显式关闭（环境变量 OCDECK_V2_ADOPTION 设为 0/false/off）；如需恢复承接，请移除该环境变量或设为 1 后激活/恢复；v1 provider 无法驱动 v2 二进制"}
		}
		// R10：机器 1 → 纯 v1；R11：unknown → 纯 v1（保守不承接）。
		return execDecision{ExecProviderID: v1}

	default:
		// 钉扎值既非 v1 也非 v2（数据异常）→ 保守 fail-closed（不猜测）。
		return execDecision{DenyKind: appagent.ErrKindProviderNotFound,
			DenyReason: "任务钉扎 provider " + pinnedProviderID + " 未注册"}
	}
}

// hasV2Binding 便捷判定（binding ProviderID 是否 v2 命名空间）。
func hasV2Binding(providerID string) bool { return providerID == ocdeckstore.ProviderIDV2 }
