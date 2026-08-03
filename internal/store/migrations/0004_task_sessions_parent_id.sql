-- migrations/0004_task_sessions_parent_id.sql
-- task_sessions 增加 parent_id（design.md §4/§2 锚定隔离 background subagent 子会话）。
-- parent_id 为空表示顶层会话（用户主会话）；非空表示 background subagent 子会话，
-- 与主会话同 directory。锚定候选 MUST 仅取顶层会话（避免锚定到子会话）。
-- agentStatus 聚合保持全量 session（子会话也算 busy，语义不变）。
ALTER TABLE task_sessions ADD COLUMN parent_id TEXT;