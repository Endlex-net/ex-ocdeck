-- migrations/0016_task_rename_pending.sql
-- 任务改名恢复意图列（task-info-editable D2/D6）。
-- rename_pending 为可空 TEXT（JSON 内容，覆盖同次保存的名称/分支/env 快照目标值），
-- NULL = 无未收敛意图。仅由内部 store row / service 映射消费，MUST NOT 进入公共任务 DTO。
-- 意图写入/清除不推进 updated_at（应用层同值 no-op 语义，非 DDL 约束）。
ALTER TABLE tasks ADD COLUMN rename_pending TEXT;
