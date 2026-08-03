-- migrations/0003_global_env_vars.sql
-- 全局级 env（v1 增补，design.md §8/§2）：跨项目生效的用户变量层，无 FK。
-- mode ∈ follow_host | manual：
--   - manual  → 使用 value 存储的显式值；
--   - follow_host → value 忽略，激活合并时从 ocdeck 服务端进程 env 解析当前值
--                   （宿主未设置/空则该变量跳过不注入，design.md §2）。
CREATE TABLE global_env_vars (
    key   TEXT PRIMARY KEY,
    mode  TEXT NOT NULL,
    value TEXT
);