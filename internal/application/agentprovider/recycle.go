// recycle.go 是首建失败补偿的 candidate 回收端口（opencode-v2-provider design
// D3 受限删除例外）：仅「本次 attempt 新建且尚未成功提交」的上游 session 允许
// 经 SessionControl 删除回收。由 provider supervisor 实现（candidate 所有权以
// 本次 attempt 返回的 binding 为准）；DELETE 失败在 provider 层记录清理债务并
// 返回错误，调用方聚合进补偿错误链——MUST NOT 把残留事实误判为「从未建立会话」。
package agentprovider

import (
	"context"
	"fmt"

	domain "ocdeck/internal/domain/agentprovider"
)

// CandidateSessionRecycler 是 v2 首建失败补偿的 candidate 回收窄接口（可选能力，
// 形态同 BindingOwnershipChecker）：task 层激活失败补偿在 v2 分支经此回收本次
// attempt 新建且未提交的 candidate session。
type CandidateSessionRecycler interface {
	RecycleCandidateSession(ctx context.Context, b domain.TaskBinding) error
}

// ServerAvailabilityWarmer 是 v2 共享 server 可用性步骤的窄接口（可选能力，
// 形态同 BindingOwnershipChecker；D4/D5：共享 server 启动/重拉用 supervisor
// 独立有界重试承担，不消耗任务恢复预算）：确认 server 存活，死亡即重拉。
// 耗尽失败由 task 层按确定性阶段结果处置（保留现状 + last_error 提交用户
// 决策），MUST NOT 进入普通任务 attempt 预算循环。
type ServerAvailabilityWarmer interface {
	CheckServerAlive(ctx context.Context) error
}

// CandidateCleanupDebt 标记未提交 candidate session 的回收失败（收尾 R1 J3）：
// SessionID 为残留上游 session（仍待回收——POST 已成功、任务侧无任何历史事实，
// 若丢失将误判零历史再次首建）；Err 为 DELETE 错误。编排层据此把清理义务按
// task 持久化（retryable cleanup debt——纳入首建准入门禁：清理成功或用户处置
// 前拒绝再次首建）。
type CandidateCleanupDebt struct {
	SessionID string
	Err       error
}

func (d *CandidateCleanupDebt) Error() string {
	if d.Err != nil {
		return fmt.Sprintf("candidate session %s 回收失败（清理债务，残留事实）: %v", d.SessionID, d.Err)
	}
	return fmt.Sprintf("candidate session %s 回收失败（清理债务，残留事实）", d.SessionID)
}

func (d *CandidateCleanupDebt) Unwrap() error { return d.Err }

// CandidateDebtReaper 是 candidate 清理债务的重放收割窄接口（可选能力；J3）：
// 按 task/session 身份重新执行受限回收（DELETE）。实现者：v2 supervisor。
// 未实现者由调用方 fail-closed（债务保留，不得静默清除）。
type CandidateDebtReaper interface {
	ReapCandidateSession(ctx context.Context, taskID, sessionID string) error
}

// ServerMonitorController 是 v2 server 监控循环的关停窄接口（可选能力；评审
// I9 生命周期：persist/kill 两种关停策略都必须停止监控——取消在途重试并等待
// 退出，调用方在其返回后才允许关停底层资源）。
type ServerMonitorController interface {
	StopServerMonitoring()
}
