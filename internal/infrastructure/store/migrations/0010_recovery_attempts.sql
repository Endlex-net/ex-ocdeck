-- migrations/0010_recovery_attempts.sql
-- per-task recovery attempt 时间戳表（single-process-opencode D3 预算协议）。
-- 每次 AcquirePermit 原子写入一条记录；滚动 5 分钟窗口内计数，过期行惰性裁剪。
CREATE TABLE task_recovery_attempts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempted_at INTEGER NOT NULL
);

CREATE INDEX idx_task_recovery_attempts_task_at
    ON task_recovery_attempts (task_id, attempted_at);
