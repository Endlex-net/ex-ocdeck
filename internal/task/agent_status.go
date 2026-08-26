package task

import (
	"context"
	"fmt"
	"time"

	"ocdeck/internal/infrastructure/opencode"
)

// AgentStatus 返回任务的 agent 运行态（design.md 2.8：任务详情/列表 DTO 增加 agentStatus，
// idle/busy/retry/空串）。任务 active 时经该任务 serve 的 GET /session/status 实时查询：
// 聚合该任务全部 session 状态（busy > retry > idle，未记录在 status map 的 session 默认 idle）。
// 查询失败或非 active 时返回空串（降级不阻塞详情返回）。
func (m *Manager) AgentStatus(ctx context.Context, taskID string) string {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil || row.Status != StatusActive {
		return ""
	}
	serveName := serveSessionName(taskID)
	// D0：AgentStatus 直接经 ShowSessionEnvContext 读取 serve 会话 env（ctx-aware，
	// 受调用方 deadline 约束），不再经 recoverPassword 间接包一层 Background+5s。
	password, perr := m.proc.ShowSessionEnvContext(ctx, serveName, "OPENCODE_SERVER_PASSWORD")
	if perr != nil || password == "" {
		return ""
	}
	portStr, perr := m.proc.ShowSessionEnvContext(ctx, serveName, "OCDECK_SERVE_PORT")
	if perr != nil || portStr == "" {
		return ""
	}
	port, ok := parsePort(portStr)
	if !ok {
		return ""
	}
	// 聚合任务全部 session 状态（busy > retry > idle）：background subagent 是独立
	// 子 session（同 worktree 创建，经 SSE 捕获进 task_sessions），主 session idle
	// 而子 session busy 时也必须显示运行中；只看"最近一个 session"会漏报（m0625 实测）。
	// opencode 契约：未记录在 status map 的 session 默认 idle。
	sessions, serr := m.store.ListTaskSessions(ctx, taskID)
	if serr != nil || len(sessions) == 0 {
		return ""
	}

	oc := m.ocFactory(port, password, opencode.Options{
		HealthTimeout: 2 * time.Second,
		OpTimeout:     3 * time.Second,
	})
	statuses, qerr := oc.SessionStatus(ctx, row.WorktreePath)
	if qerr != nil {
		return ""
	}
	result := "idle"
	for _, s := range sessions {
		st, exists := statuses[s.SessionID]
		if !exists {
			continue // 未记录 = idle，不影响聚合
		}
		switch st.Type {
		case "busy":
			return "busy"
		case "retry":
			result = "retry"
		}
	}
	return result
}

// ListAllActiveTaskIDs 返回当前全部 active 任务 ID（供全局配置保存后受影响任务提示，
// design.md §13/global-config-management spec）。查询失败返回错误。
func (m *Manager) ListAllActiveTaskIDs(ctx context.Context) ([]string, error) {
	rows, err := m.store.ListAllTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all tasks for active ids: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, t := range rows {
		if t.Status == StatusActive {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// ListActiveTaskOverview 聚合全部 active 任务的跨项目概览
//（cross-project-active-sessions D1：纯读聚合查询，委托 store）。
func (m *Manager) ListActiveTaskOverview(ctx context.Context) ([]ActiveTaskOverviewRow, error) {
	return m.store.ListActiveTaskOverview(ctx)
}