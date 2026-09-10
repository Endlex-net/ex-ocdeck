-- migrations/0015_task_permission_mode.sql
-- 任务级权限模式 permission_mode（task-permission-mode D1）。
--   tasks.permission_mode ∈ ask | all-approve | ai-auto：
--     - ask（缺省）：人工逐条批准，既有任务行为不变；
--     - all-approve：激活时以 --auto 启动 opencode（用户配置中的显式 deny 仍生效）；
--     - ai-auto：平台全局 LLM 判定权限请求，不可用时转人工。
--   存量任务由 DEFAULT 覆盖为 ask（行为等价现状），无需回填（比 0013 更简单，无 kind 维度）。
ALTER TABLE tasks ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'ask';
