## ADDED Requirements

### Requirement: 新建任务分支 slug 高级选项

指挥中心内联新建任务面板 SHALL 提供折叠的高级选项区，内含可选的分支 slug 输入框。该输入框 MUST 仅在选中 repo 项目且运行模式为 worktree 时渲染；dir 项目与 local-path 模式下 MUST NOT 渲染。slug 留空（trim 后为空）时创建行为与现状完全一致（LLM 命名 / 机械 slugify）；填写时创建请求携带该 slug，最终分支名为 `<branch-prefix>/<slug>`。填写非空 slug 时输入框附近 SHALL 实时展示最终分支名预览（`<branch-prefix>/<slug>`，前缀取当前全局配置值）；**前缀配置 GET 尚未成功时预览 MUST 显示「前缀未加载」占位，MUST NOT 伪造准确预览，且前缀加载状态 MUST NOT 成为提交门禁**。slug 为可选项，MUST NOT 成为提交门禁的一部分（提交使能规则与现状一致）；slug 校验失败（invalid_input）沿既有创建失败路径展示错误原因。用户已输入的 slug 在切换运行模式、切换项目、消费命令面板初始化信号时 MUST 保留（与 taskName 的保留语义一致）。

#### Scenario: 高级选项默认折叠且留空行为不变

- **WHEN** 用户打开新建任务面板，不展开高级选项直接创建任务
- **THEN** 请求不携带分支 slug，分支命名走既有 AI/slugify 行为

#### Scenario: 填写 slug 并预览最终分支名

- **WHEN** 用户展开高级选项并输入 slug `my-feature`（当前全局前缀为 `ocdeck`）
- **THEN** 输入框附近实时展示预览 `ocdeck/my-feature`，提交时请求携带 slug `my-feature`

#### Scenario: 非法 slug 错误展示

- **WHEN** 用户填写的 slug 导致最终分支名未通过服务端校验，提交创建
- **THEN** 创建失败，页面沿既有失败路径展示错误原因，表单状态（含 slug 输入）保留

#### Scenario: dir / local-path 不渲染 slug 输入

- **WHEN** 用户在新建任务面板选中 dir 项目，或将运行模式切换为 local
- **THEN** 高级选项区不渲染分支 slug 输入框；已输入的 slug 保留但不随提交发送（local-path 提交契约不变：MUST NOT 携带分支 slug）
