## Why

用户会在多个项目的多个 worktree 中同时推进任务，当前只能按「项目列表 → 项目详情 → 任务」逐层钻取查看，无法一眼看到全平台哪些任务正在活跃、agent 是否在干活、最近活跃时间，跟进成本高。

## What Changes

- 新增跨项目活跃任务列表 API：`GET /api/v1/sessions/active`，按 task 粒度返回所有 `status='active'` 的任务（含所属项目、分支、最近活跃时间），并实时水合 `agentStatus`（idle/busy/retry），按最近活跃时间倒序。
- 新增前端独立页面 `#/active`：展示跨项目活跃任务列表，轮询刷新，点击行跳转对应任务工作台；项目列表页（`#/`）加入口。
- 新增 store 层跨项目查询：join `projects × tasks × task_sessions`，取每个 task 的 `MAX(last_seen_at)`。

无 BREAKING 变更；不新增快捷操作（suspend/archive 仍在任务工作台内进行）。

## Capabilities

### New Capabilities

- `active-sessions-overview`: 跨项目活跃任务列表的后端聚合查询、API 语义（含水合降级规则、排序、空态）与前端独立页面行为。

### Modified Capabilities

（无）

## Impact

- **代码**：
  - `internal/process/`：前置收敛——新增 context-aware `ShowSessionEnvContext`（现有 `ShowSessionEnv` 忽略调用方 ctx，无法兑现水合硬超时）。
  - `internal/store/queries.go`：新增跨项目活跃任务概览查询。
  - `internal/task/`：扩展 `TaskStore` 接口、`StoreAdapter`、`Manager`，暴露概览查询 read model；`ProcessBackend` 接口新增 ctx 变体，**仅** `AgentStatus` 内两次 tmux 读取改走 ctx 变体（共享辅助 `recoverPassword` 与其他存量路径保持原样）。
  - `internal/api/`：新增路由与 handler（DTO、并发 agentStatus 水合），扩展 `TaskBackend` 接口及测试 fakes。
  - `web/src/`：新增 `ActiveSessionsPage`、路由、`api.ts` 方法与类型；`ProjectsPage` 加入口；轮询 single-flight。
- **API**：新增 `GET /api/v1/sessions/active`（Bearer 鉴权，与其他管理 API 一致）。
- **DB**：无 schema 变更，纯查询。
- **依赖**：无新增外部依赖。
