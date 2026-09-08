# Proposal: allow-duplicate-project-path

## Why

当前系统对同一路径重复注册项目直接返回 409 `[conflict] project already registered for path: <path>`，用户无法为同一目录创建多个项目（例如同一仓库按不同工作流/配置拆成多个项目）。用户需要放开该限制，仅在创建时给予告警即可。

## What Changes

- 允许同一 canonical path 注册多个项目（`kind=repo` 与 `kind=dir` 均放开）：重复 path 不再导致注册被拒绝，创建照常成功。
- 创建项目表单中，用户输入目录路径时若与已注册项目 path 重复，前端实时展示提醒（不阻塞提交）。该提醒仅由前端基于已加载的项目列表提供，不新增后端查重接口；创建接口对重复 path 直接成功，不返回重复提示，也不新增 warning 字段。
- **BREAKING**（存储层）：拆除 `projects.path` 的唯一性约束，path 不再是项目的唯一标识维度（项目 id 仍是唯一主键）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `project-management`: 「项目注册」行为变更——同一 path 允许注册多个项目，重复 path 由拒绝改为允许；「项目管理单页」注册表单行为变更——输入路径与已有项目重复时实时提醒。

## Impact

- 存储层：移除 `projects.path` 唯一性约束，支持同一路径关联多个项目。
- 前端：项目管理页注册表单新增输入时重复 path 提醒。
- 兼容性：对外 API 契约中 409 重复 path 场景消失（重复 path 变为成功）；依赖「path 唯一」假设的调用方需核查。
