// ownership.go 定义 v2 provider 引入的可选窄接口 BindingOwnershipChecker
// （opencode-v2-provider design D2 校验分层）：provider 对持久化 binding 做
// 「结构 + 归属」两阶段校验的阶段一端口——task 层先经本端口比对独立任务事实
// （任务行 ID 与 worktree 解析结果），归属通过后才允许凭据派生与网络副作用。
//
// task 层契约：provider 未实现本接口 → 拒绝使用其 binding（fail-closed），
// MUST NOT 以「binding 从该任务行读出」代替字段值比对。
package agentprovider

import (
	domain "ocdeck/internal/domain/agentprovider"
)

// BindingOwnershipChecker 是 binding 归属校验端口（阶段一；不派生凭据）。
// 实现方解 provider 私有 data 并逐字段比对 taskId/directory，任何不符
// （含解码失败）→ ErrKindBindingInvalid；比对通过 → nil。
// 阶段二（凭据派生、BindingRuntimeEndpoint、网络/进程副作用）MUST 在本端口
// 通过后才允许——BindingRuntimeEndpoint 返回值含凭据，MUST NOT 用于阶段一。
type BindingOwnershipChecker interface {
	CheckBindingOwnership(b domain.TaskBinding, expectedTaskID, expectedDirectory string) error
}
