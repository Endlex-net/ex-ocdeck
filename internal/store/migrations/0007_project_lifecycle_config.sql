-- migrations/0007_project_lifecycle_config.sql
-- 项目级生命周期配置（design.md §2：inherit / init / pre-delete）。
-- 独立表而非 projects 加列，与 project_env_vars 解耦风格一致；
-- 无配置项目无行（读时缺行 = 三字段全空）。
-- FK 依赖 PRAGMA foreign_keys=ON（store.Open 已在连接级开启），ON DELETE CASCADE 生效。
--
-- tasks 增 init_status（默认 none）与 init_error（nullable）：
--   init_status ∈ none | pending | running | succeeded | failed；存量任务迁移后为 none。
--   init_status=failed 时 init_error 有值，其余状态 init_error 为 NULL。
CREATE TABLE project_lifecycle_configs (
    project_id        TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    inherit_patterns  TEXT NOT NULL DEFAULT '',
    init_script       TEXT NOT NULL DEFAULT '',
    pre_delete_script TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL
);

ALTER TABLE tasks ADD COLUMN init_status TEXT NOT NULL DEFAULT 'none';
ALTER TABLE tasks ADD COLUMN init_error  TEXT;