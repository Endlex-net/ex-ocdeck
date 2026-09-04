// restart.go 实现 3.8 重启恢复两段式（design.md D2 重启恢复）。
//
// 两段式（D2 唯一）：
//
//	① 服务启动收敛（独立于 runtime）：API 与调度器开放前，全局单事务 sending→delivery_unknown +
//	   固定 error "delivery unknown after restart"。写库失败 fail-closed（不开放 API/调度器）。
//	② runtime 启动恢复本任务 queued：runtime 每次 ready 仅扫描本任务 queued 重新入队
//	   （queued 行已存在，无需重写；恢复语义=调度器随 runtime 启动接管本任务队列）。
package diffreview

import "context"

// ConvergeOnStartup 服务启动全局收敛（design.md D2 重启恢复①）。
//
// 调 store ConvergeDiffReviewOnStartup 原语：单事务全部 sending→delivery_unknown +
// 固定 error "delivery unknown after restart"。返回 affected 行数。
// 写库失败 fail-closed：调用方（server 启动序列）MUST 不开放 API/调度器（返回 error 传播）。
//
// 独立于 runtime：无论 runtime 是否 ready 均执行（sending 不会无限停留）。
func (s *Service) ConvergeOnStartup(ctx context.Context) (int64, error) {
	return s.repo.ConvergeDiffReviewOnStartup(ctx)
}

// RecoverQueuedForTask runtime 启动恢复本任务 queued（design.md D2 重启恢复②）。
//
// runtime 每次 ready 仅扫描本任务 queued 重新入队。queued 行已存在（状态未变），
// 恢复语义=确认本任务 queued 队列可被调度器接管。返回 queued 行数（供 task 层确认调度器需启动）。
//
// runtime 无法 ready 不影响①（ConvergeOnStartup 独立执行）。
func (s *Service) RecoverQueuedForTask(ctx context.Context, taskID string) (int64, error) {
	subs, err := s.repo.ListDiffReviewQueue(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, sub := range subs {
		if sub.Status == "queued" {
			n++
		}
	}
	return n, nil
}
