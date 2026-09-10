// sessions_snapshot.go active sessions 场景的共享快照组装 helper
// （sse-active-sessions P2.2；design.md D3：REST 与 SSE 帧同构）。
//
// 组装 = ListActiveTaskOverview 读模型 + per-task attention 快照 + AgentStatusSnapshot
// 内存快照（P1 已上线的 event-driven agentStatus），产出与 REST 响应同构的 DTO 裸数组。
// 不做实时探测（原 REST handler 的并发 8/3s 水合已移除）：agentStatus 由内存快照
// 提供，不可用时空串经 omitempty 省略，降级语义与原实时探测失败一致。
package api

import (
	"context"
	"fmt"
)

// buildActiveSessionsSnapshot 组装 active sessions 全量快照（REST handler 与
// SSE 全量重推共用；design.md D3）。store 失败返回 error（调用方 500 / SSE 保留
// 上次快照）；空结果返回非 nil 空切片（JSON `[]` 非 null）。
// mode 必有透传（add-local-path-task-mode D7）：非法 kind/mode 组合为持久化损坏，
// 返回错误 fail-closed——REST 500；SSE 初始组装 500、update 保持 dirty 重试，
// MUST NOT 推送缺 mode 的帧。
func (s *Server) buildActiveSessionsSnapshot(ctx context.Context) ([]activeSessionDTO, error) {
	rows, err := s.tasks.ListActiveTaskOverview(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]activeSessionDTO, 0, len(rows))
	for _, row := range rows {
		if !validTaskModeForKind(row.Kind, row.Mode) {
			return nil, fmt.Errorf("task %s: invalid mode %q for kind %q", row.ID, row.Mode, row.Kind)
		}
		// permission_mode 必有透传（task-permission-mode D7）：空值输出 ask、未知持久化值
		// fail-closed，错误语义与 mode 一致（REST 500；SSE 保持 dirty 重试）。
		permissionMode, ae := permissionModeForOutput(row.ID, row.PermissionMode)
		if ae != nil {
			return nil, ae
		}
		dto := activeSessionDTO{
			TaskID: row.ID, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
			Name: row.Name, Branch: row.Branch, WorktreePath: row.WorktreePath,
			Mode: row.Mode, PermissionMode: permissionMode,
			LastActiveAt: row.LastActiveAt,
			AgentStatus:  s.tasks.AgentStatusSnapshot(row.ID),
		}
		// D6 注意力信号快照透出（空数组非 null）。
		att, _ := s.tasks.Attention(row.ID)
		dto.Attention = toAttentionDTO(att)
		out = append(out, dto)
	}
	return out, nil
}
