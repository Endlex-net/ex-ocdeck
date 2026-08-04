# Proposal: project-lifecycle-config

## Why

每个任务都是独立 worktree，但 gitignored/untracked 文件（`.env`、本地证书等）不会随 worktree 一起出现，新任务开箱即坏；同时项目常有"新环境必须先装依赖"（`pnpm install`）和"删除前必须释放外部资源"（`docker compose down`）的一次性准备工作，目前全靠用户手动在每个 worktree 里重复执行。项目级生命周期配置让这三件事声明一次、自动执行。

灵感来自 emdash 的 project config，但按 ocdeck 自身的任务模型裁剪：emdash 的 Run（dev server 预览）与 Shell setup（与现有 env 管理重叠）不适合 ocdeck，本期不做。

## What Changes

- 项目新增三项生命周期配置（每项目一份，存 SQLite）：
  - **Inherit patterns**：glob 模式列表（每行一个）。任务创建时，把主仓库中匹配的 gitignored/untracked 文件复制进新 worktree。复制失败仅记警告，不阻断创建。
  - **Init script**：shell 脚本。worktree 创建后执行一次（后台执行，状态落库）。执行失败 → `init_status=failed`，**阻塞任务激活**（fail-closed），用户可查看日志并重试。超时 10 分钟（写死，v1 不可配）。
  - **Pre-delete script**：shell 脚本。任务删除流程中、kill 残余会话之后、worktree 移除之前执行。失败 → `deletion_failed` 可重试（复用现有删除失败语义）。超时 2 分钟（写死，v1 不可配）。
- 新增项目级 Config API（GET/PUT）与 ProjectDetailPage 的 "Project Config" UI 区块（Inherit / Init / Pre-delete 三个编辑器）。
- 任务新增 `init_status`（`none|pending|running|succeeded|failed`）与 init/pre-delete 日志查看、手动 "Re-run Init" 入口（Re-run 限 suspended 任务）。
- 删除流程新增 init 进行中门禁（pending/running 拒绝 Delete/Archive）；**Force 删除语义正式扩展**：除跳过 oc session 删除外，亦跳过 pre-delete script（脚本持续失败的逃生舱）。
- 命名采用 ocdeck 自有概念：inherit / init / pre-delete，不沿用 emdash 的 preserve/setup/teardown；不使用 `cleanup` 一词（已被 cleanup debt 占用）。

## Capabilities

### New Capabilities

- `project-lifecycle-config`: 项目级生命周期配置（inherit patterns / init script / pre-delete script）的存储、API、执行语义、状态机、日志与 UI。

### Modified Capabilities

- `task-lifecycle`: 任务创建流程新增 inherit 复制与 init 执行步骤；激活新增 `init_status=succeeded` 门禁；删除流程新增 pre-delete 脚本步骤与 init 进行中门禁；Force 删除语义正式扩展（亦跳过 pre-delete）。
- `env-management`: "明文存储与日志红线"正式限定为 ocdeck 自身生成的日志与错误信息；用户脚本 stdout/stderr 属用户可控输出，按原样捕获落盘（附 UI 提示）。

## Impact

- **存储**：新 migration（独立表 `project_lifecycle_configs`——已决策，非 projects 增列；tasks 增 `init_status`/`init_error` 列）。
- **后端**：`internal/task/crud.go`（Create/retryCreate 挂 inherit+init）、`internal/task/activate.go`（激活门禁）、`internal/task/delete.go`（deleteResume 挂 pre-delete）、新增生命周期脚本执行器组件、`internal/api` 新路由与 DTO。
- **前端**：`web/src/pages/ProjectDetailPage.tsx` 新增 Config 区块、`api.ts`/`types.ts` 扩展、任务详情展示 init 状态与日志。
- **无破坏性变更**：未配置的项目行为与现状完全一致（三项配置均为可选，默认为空即不执行）。
- **明确不做**：emdash 的 Run script、Shell setup、per-activation setup、脚本超时的用户可配置化。
