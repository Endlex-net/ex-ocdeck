# Proposal: task-base-branch-context

## Why

创建任务选择基准分支时，本地与远端同名分支（如 `main` 与 `origin/main`）在分支下拉列表中排序不透明，用户难以快速选中期望的远端分支；同时任务运行上下文（agent bash、页面 shell）拿不到本次任务的基线分支与任务分支名，agent 无法感知"我从哪个分支切出、我在哪个分支上工作"。

## What Changes

- 任务创建面板的基准分支下拉列表：过滤结果中远端分支（`origin/*`）相对同名本地分支获得更高优先级排序；repo 项目任一提交路径（创建按钮或表单 Enter）以下拉过滤排序后的第一项作为 `base_ref` 提交。
- 任务激活时向任务环境新增注入两个生命周期环境变量：`OCDECK_TASK_BASE_BRANCH`（基线分支短名，用户所见形态）与 `OCDECK_TASK_HEAD_BRANCH`（任务自身分支名），agent 的 bash 执行与页面打开的 shell 均可读取。已 active 任务的自动重拉不重新分层环境变量（须挂起再激活才获得新键）。
- dir（纯目录）项目无分支概念，不注入上述变量。

非目标：

- 不修改项目创建流程（项目默认分支仍为自动探测，不新增项目级分支选择）。
- 不修改后端 `base_ref` 解析规则（heads 优先、仅接受 heads/remotes 两命名空间的行为不变）。
- 不接入 opencode session metadata 通道（无现成写入路径，环境变量已覆盖两条消费链路）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `env-management`: 生命周期环境变量集合新增 `OCDECK_TASK_BASE_BRANCH` 与 `OCDECK_TASK_HEAD_BRANCH` 两个注入项（含 dir 项目不注入的边界）。
- `command-center`: 任务创建面板基准分支下拉的过滤排序规则，以及任一提交路径（创建按钮或表单 Enter）使用过滤首项作为 `base_ref`。

## Impact

- 后端：`internal/task/activate.go` 生命周期环境变量注入点；`internal/task/recovery.go` 自动重拉复用同代 env 快照（不重新分层）。env 快照持久化后，opencode serve 与 web shell 两条消费链路自动覆盖。
- 前端：`web/src/pages/CommandCenterPage.tsx` 任务创建面板分支下拉组件。
- 无 REST/JSON API 契约及存储 schema 变化；新增两个任务进程环境变量契约（`OCDECK_TASK_BASE_BRANCH`、`OCDECK_TASK_HEAD_BRANCH`）。`base_ref` 解析与落库行为不变，向后兼容。
