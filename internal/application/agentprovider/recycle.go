// recycle.go 是首建失败补偿的 candidate 回收端口（opencode-v2-provider design
// D3 受限删除例外）：仅「本次 attempt 新建且尚未成功提交」的上游 session 允许
// 经 SessionControl 删除回收。由 provider supervisor 实现（candidate 所有权以
// 本次 attempt 返回的 binding 为准）；DELETE 失败在 provider 层记录清理债务并
// 返回错误，调用方聚合进补偿错误链——MUST NOT 把残留事实误判为「从未建立会话」。
package agentprovider

import (
	"context"

	domain "ocdeck/internal/domain/agentprovider"
)

// CandidateSessionRecycler 是 v2 首建失败补偿的 candidate 回收窄接口（可选能力，
// 形态同 BindingOwnershipChecker）：task 层激活失败补偿在 v2 分支经此回收本次
// attempt 新建且未提交的 candidate session。
type CandidateSessionRecycler interface {
	RecycleCandidateSession(ctx context.Context, b domain.TaskBinding) error
}
