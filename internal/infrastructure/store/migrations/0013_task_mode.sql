-- migrations/0013_task_mode.sql
-- 任务级运行模式 mode（add-local-path-task-mode D1）。
--   tasks.mode ∈ worktree | local-path：
--     - worktree（缺省）：隔离 worktree + 独立分支，既有 repo 任务语义不变；
--     - local-path：就地运行，语义与 dir 项目任务完全一致（无 worktree/分支）。
--   存量任务由 DEFAULT 覆盖为 worktree（repo 任务行为等价）；dir 项目的既有任务
--   显式回填 local-path，避免 dir+worktree 非法组合持久化。
ALTER TABLE tasks ADD COLUMN mode TEXT NOT NULL DEFAULT 'worktree';

UPDATE tasks
   SET mode = 'local-path'
 WHERE project_id IN (SELECT id FROM projects WHERE kind = 'dir');
