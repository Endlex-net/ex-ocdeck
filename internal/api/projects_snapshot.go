// projects_snapshot.go projects 场景（侧栏/项目管理页全任务树投影）的共享快照组装
// helper（projects-stream design D3；REST /projects 与 SSE /projects/stream 帧同构）。
//
// 组装 = projects 列表 + ListProjectTaskSummaries 全量任务摘要 + 逐项目 counts + 分组 +
// 摘要 DTO（active 摘要 agentStatus 读内存快照），产出与 REST 响应同构的 projectDTO
// 裸数组。全链路纯读：无 goroutine、无实时探测；快照不可用时空串经 omitempty 省略。
package api

import (
	"context"

	"ocdeck/internal/application"
)

// buildProjectsSnapshot 组装 projects 全量快照（REST handler 与 SSE 全量重推共用；
// projects-stream design D3）。store 失败返回 error（REST 500 / SSE 初始组装 500、
// 重推保留上次快照，决策留在调用方）；无项目返回非 nil 空切片（JSON `[]` 非 null）。
func (s *Server) buildProjectsSnapshot(ctx context.Context) ([]projectDTO, error) {
	rows, err := s.projs.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	// 先取全部任务摘要（store 失败 → 500，spec）。
	var summaries []application.ProjectTaskSummary
	if s.tasks != nil {
		summaries, err = s.tasks.ListProjectTaskSummaries(ctx)
		if err != nil {
			return nil, err
		}
	}
	byProject := groupSummariesByProject(summaries)
	out := make([]projectDTO, 0, len(rows))
	for _, p := range rows {
		counts, cerr := s.projs.CountProjectTasks(ctx, p.ID)
		if cerr != nil {
			return nil, cerr
		}
		dto := toProjectDTO(p, counts)
		dto.TaskSummaries = s.toProjectTaskSummaryDTOs(byProject[p.ID])
		out = append(out, dto)
	}
	return out, nil
}
