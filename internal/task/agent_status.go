package task

import (
	"context"
	"fmt"
	"time"

	"ocdeck/internal/opencode"
)

// AgentStatus 返回任务的 agent 运行态（design.md 2.8：任务详情/列表 DTO 增加 agentStatus，
// idle/busy/retry/空串）。任务 active 时经该任务 serve 的 GET /session/status 实时查询：
// 取该任务最近 session（ListTaskSessions 首项，已按 last_seen_at DESC 排序）的状态。
// 查询失败或非 active 时返回空串（降级不阻塞详情返回）。
func (m *Manager) AgentStatus(ctx context.Context, taskID string) string {
	row, err := m.store.GetTask(ctx, taskID)
	if err != nil || row.Status != StatusActive {
		return ""
	}
	serveName := serveSessionName(taskID)
	password := m.recoverPassword(ctx, taskID)
	if password == "" {
		return ""
	}
	portStr, perr := m.proc.ShowSessionEnv(serveName, "OCDECK_SERVE_PORT")
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