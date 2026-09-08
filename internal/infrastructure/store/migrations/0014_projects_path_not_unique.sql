-- migrations/0014_projects_path_not_unique.sql
-- 移除 projects.path 唯一约束（allow-duplicate-project-path D3）：
-- 同一 canonical path 允许注册多个项目，项目唯一标识仅为 id。
-- SQLite 无法删除列级 UNIQUE，需重建表；重建 MUST 在连接级 FK 关闭下执行
-- （store.go migrationsRequiringForeignKeysOff 登记）：FK ON 下 DROP projects
-- 的隐式 DELETE 会级联清空 tasks 等子表数据。完整性由 commit 前
-- foreign_key_check 兜底（违反则整个迁移事务回滚）。

CREATE TABLE projects_new (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    path          TEXT NOT NULL,
    default_branch TEXT,
    created_at    INTEGER NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'repo'
);
INSERT INTO projects_new (id, name, path, default_branch, created_at, kind)
    SELECT id, name, path, default_branch, created_at, kind FROM projects;
DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;
