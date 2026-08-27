-- migrations/0009_anchor_session_id.sql
-- 任务锚定 session 显式列（single-process-opencode D5）。
-- 替代「最近顶层 owned session」推导；NULL 表示尚无权威锚定。
-- 存量 NULL 行按 ListTopLevelTaskSessions 同款排序回填最近顶层 owned session。
ALTER TABLE tasks ADD COLUMN anchor_session_id TEXT;

UPDATE tasks
   SET anchor_session_id = (
       SELECT session_id
         FROM task_sessions
        WHERE task_sessions.task_id = tasks.id
          AND (parent_id IS NULL OR parent_id = '')
        ORDER BY last_seen_at DESC, session_created_at DESC, session_id DESC
        LIMIT 1
   )
 WHERE anchor_session_id IS NULL;
