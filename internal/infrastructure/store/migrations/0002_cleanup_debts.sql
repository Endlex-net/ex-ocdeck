-- migrations/0002_cleanup_debts.sql
-- orphan/逃逸进程 cleanup debt 持久化（design.md §10/§5）。
-- Shutdown 收割失败的 orphan tickets（脱离 tmux 的逃逸进程身份）MUST 持久化，
-- 主进程退出后下次启动 Reconcile 从该表恢复重试，不得仅存内存随进程退出丢失。
-- session_name 为主键：同一会话的未收敛 tickets 原地替换（最新聚合 wins，不重复累积）。
-- tickets 为 JSON 编码的字符串数组（[]string）。
CREATE TABLE cleanup_debts (
    session_name TEXT PRIMARY KEY,
    tickets      TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);