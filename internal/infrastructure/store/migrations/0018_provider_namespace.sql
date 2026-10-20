-- migrations/0018_provider_namespace.sql
-- provider 命名空间与任务绑定列（opencode-v1-spi-provider 阶段 1，design D3/D4/D6）。
--   - tasks.provider_id / task_sessions.provider_id + provider_instance_id：
--     SessionKey 命名空间列。本期 v1 全部归属统一命名空间
--     ('opencode-v1'/'default')，归属判定冲突集合与现状逐点一致（D2）。
--   - tasks.task_binding：版本化 JSON envelope（D3），nullable——存量任务无绑定，
--     由阶段 3 迁移导入 runner 建立；本阶段仅落列，无读写方。
--   - tasks.anchor_provider_id / anchor_provider_instance_id：与既有 anchor_session_id
--     共同构成锚定 SessionKey（D4）。nullable TEXT（无锚定行保持 NULL 语义）。
-- 主键不变：task_sessions 保留 (task_id, session_id)（D4——兼容旧二进制
-- 回滚窗口的 ON CONFLICT(task_id, session_id) upsert，queries.go 现状）。
-- DEFAULT 常量使存量行与旧二进制（回滚窗口内省略新列的 INSERT）自动获得正确
-- 命名空间（SQLite ALTER TABLE ADD COLUMN ... DEFAULT 语义）；显式 NULL/空串的
-- 边界行由启动期防御性补齐收敛（provider_namespace.go，tasks 2.2）。
-- 向前兼容：不提供 down migration；旧行为写入语义零变化（本迁移仅加列）。
ALTER TABLE tasks ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'opencode-v1';
ALTER TABLE tasks ADD COLUMN task_binding TEXT;
ALTER TABLE tasks ADD COLUMN anchor_provider_id TEXT DEFAULT 'opencode-v1';
ALTER TABLE tasks ADD COLUMN anchor_provider_instance_id TEXT DEFAULT 'default';

ALTER TABLE task_sessions ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'opencode-v1';
ALTER TABLE task_sessions ADD COLUMN provider_instance_id TEXT NOT NULL DEFAULT 'default';
