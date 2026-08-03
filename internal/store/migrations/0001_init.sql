-- migrations/0001_init.sql
-- 初始 schema（design.md §8）。
-- 注意：modernc.org/sqlite 默认禁用外键约束，需在连接级 PRAGMA foreign_keys=ON 启用。
-- ON DELETE CASCADE 依赖 foreign_keys=ON 才生效。
--
-- 业务 DDL 不使用 IF NOT EXISTS：schema 已 drifted 时启动即失败，避免静默遗留旧表
-- 与代码契约不一致（oracle B1/S1 建议）。schema_version 为 migration 元数据表，
-- 保留 IF NOT EXISTS 以兼容 store.go 迁移引导逻辑。

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);

CREATE TABLE projects (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    path          TEXT NOT NULL UNIQUE,
    default_branch TEXT,
    created_at    INTEGER NOT NULL
);

CREATE TABLE tasks (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    branch        TEXT NOT NULL,
    status        TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    last_port     INTEGER,
    last_error    TEXT,
    notice        TEXT,
    delete_mode   TEXT,
    env_snapshot  TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    archived_at   INTEGER
);

CREATE INDEX idx_tasks_project_id ON tasks(project_id);
CREATE INDEX idx_tasks_status ON tasks(status);

CREATE TABLE project_env_vars (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value       TEXT NOT NULL,
    PRIMARY KEY (project_id, key)
);

CREATE TABLE task_env_vars (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    PRIMARY KEY (task_id, key)
);

CREATE TABLE task_sessions (
    task_id          TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id       TEXT NOT NULL,
    session_created_at INTEGER NOT NULL,
    first_seen_at    INTEGER NOT NULL,
    last_seen_at     INTEGER NOT NULL,
    PRIMARY KEY (task_id, session_id)
);