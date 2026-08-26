-- migrations/0008_project_kind.sql
-- 项目类型 kind 与任务基线分支 base_ref（add-plain-dir-project D1/D10）。
--   projects.kind ∈ repo | dir，存量项目与缺省注册均为 repo。
--   tasks.base_ref 为 repo 任务的基线分支全引用（如 refs/heads/main），
--     dir 项目任务落空串；存量任务回填为对应项目 default_branch 的全引用。
ALTER TABLE projects ADD COLUMN kind TEXT NOT NULL DEFAULT 'repo';
ALTER TABLE tasks    ADD COLUMN base_ref TEXT NOT NULL DEFAULT '';

UPDATE tasks
   SET base_ref = 'refs/heads/' || (SELECT default_branch FROM projects WHERE projects.id = tasks.project_id);