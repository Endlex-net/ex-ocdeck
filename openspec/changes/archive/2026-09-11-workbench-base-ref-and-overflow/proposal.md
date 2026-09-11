# Proposal: workbench-base-ref-and-overflow

## Why

用户在 worktree 模式下无法看到任务的来源分支（从哪个分支切出），合并/回溯时需要离开界面去查；该信息（base_ref）已存在于后端领域模型与读模型，但从未透出到前端。同时，任务工作台页头的「更多操作（⋯）」溢出菜单在没有任何可见菜单项时仍显示按钮、点开是空菜单，属于断掉的 affordance。

## What Changes

- 任务详情 API（REST 详情与 SSE 详情流）透出已持久化的任务来源分支（base_ref），工作台据此展示来源分支；字段形态、空值语义与兼容性契约见 design D1 与 task-detail-stream delta。
- 任务工作台页头分支信息区展示「来源分支」与「当前分支」（不使用"目标分支"措辞）：worktree 模式且 base_ref 非空时两行展示（上行当前、下行来源）；local-path（repo 项目）任务展示文件系统当前 checkout 分支；dir 项目无 git 不展示。术语与非 worktree 展示范围按本轮人工批注更新，具体数据来源和边界态见 design D3。
- 页头展示的每个分支名支持点击复制（复制其显示短名，toast 反馈）；交互细节见 design D3。
- 新建任务面板的权限模式选择缺省改为「AI 自动识别」（ai-auto，原为 ask）；仅面板默认选中变化，创建接口契约与后端缺省（不传仍为 ask）不变。
- 任务工作台页头的「更多操作（⋯）」溢出菜单：当没有任何可见菜单项时，整个入口按钮不渲染（对齐仓库已有的「不可用即不出现」空态哲学，与 OpenInEditorMenu 无工具时隐藏的先例一致）。
- 任务列表（指挥中心、项目管理、侧栏）不展示来源分支，维持现状。

**Non-goals**：不在任务列表/侧栏展示来源分支；不新增独立任务详情页；不向页头堆叠其他任务元信息（worktree 路径、模式等，未来如有需要走 Settings tab 独立需求）；不改变溢出菜单各菜单项自身的显示条件（init 日志、删除任务的现有条件不变）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `task-detail-stream`: 任务详情对象字段清单新增 `base_ref`（来源分支，仅 worktree 模式有值），REST 详情与 SSE 详情流同步透出。
- `web-ui-shell`: 任务工作台页头新增来源分支展示要求（worktree 模式、base_ref 非空时）；溢出菜单新增「无可见菜单项时整个入口不渲染」的空态要求。
- `task-permission-mode`: 新建任务面板权限模式缺省选中从 `ask` 改为 `ai-auto`（仅 Web 表单行为；创建接口缺省语义不变）。

## Impact

- **后端**：`internal/api/tasks.go`（`taskRowDTO` / `toTaskDTO` 组装透出 base_ref；数据已存在于 `internal/application/dto.go` 的 `TaskRow.BaseRef`，无持久化变更）。
- **前端**：`web/src/types.ts`（`Task` 类型新增 `base_ref`）、`web/src/pages/TaskWorkbenchPage.tsx`（页头分支展示——local-path 任务额外一次性调用既有 `git/status` 端点取当前分支、WorkbenchOverflow 空态隐藏）、`web/src/pages/CommandCenterPage.tsx`（新建任务面板权限模式缺省 ai-auto）。
- **API 契约**：详情对象新增必有字段 `base_ref`，向后兼容（旧客户端忽略新字段）；该字段由共享 DTO 组装函数透出，所有复用 `toTaskDTO` 的响应（任务详情 REST/SSE、任务列表、创建响应、rerun-init 响应）都会携带它，但列表 UI 不展示来源分支。
- **测试**：后端 DTO 组装测试、前端组件/逻辑测试。
