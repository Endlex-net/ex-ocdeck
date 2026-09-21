-- migrations/0017_task_permission_mode_at_start.sql
-- 任务启动事实列（task-permission-mode D5）。
-- permission_mode_at_start 为可空 TEXT：本次激活 argv 实际使用的权限模式值，
-- 仅由 startRuntimeWithPortRetry 建 serve 进程前写入；NULL = 无启动事实
-- （未启动过 / shell、temp serve 等不承载权限语义的会话）。恢复路径按本列
-- fail-closed 校验后重建 RuntimePermissionState。事实写入不推进 updated_at、不发事件。
ALTER TABLE tasks ADD COLUMN permission_mode_at_start TEXT;
