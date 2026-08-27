-- migrations/0011_recovery_debts.sql
-- Recovery tagged debt 结构化持久化（single-process-opencode D3 pending/replay，G3-10）。
-- cleanup_notice：task_id + session_name + tickets + reason + retryable + cause
--（每个未落库 cleanup 会话一行——同一任务可能多条，载荷不丢弃）
-- complete：仅 task_id + cause（session_name 固定空串位）
-- 复合主键 (task_id, session_name)：同一任务多 session debt 共存；complete 行与
-- cleanup_notice 行经 session_name 空串区分，不会冲突。收敛/服从最新状态时按 task_id
-- 整组删除。
CREATE TABLE IF NOT EXISTS recovery_debts (
    task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_name TEXT NOT NULL DEFAULT '',
    phase        TEXT NOT NULL,
    tickets      TEXT NOT NULL DEFAULT '[]',
    reason       TEXT NOT NULL DEFAULT '',
    retryable    INTEGER NOT NULL DEFAULT 0,
    cause        TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (task_id, session_name)
);

CREATE INDEX IF NOT EXISTS idx_recovery_debts_task ON recovery_debts(task_id);
