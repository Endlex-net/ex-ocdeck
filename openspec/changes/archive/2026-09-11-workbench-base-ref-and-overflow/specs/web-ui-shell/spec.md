# Delta: web-ui-shell（workbench-base-ref-and-overflow）

## ADDED Requirements

### Requirement: 工作台页头分支展示（来源分支与当前分支）

任务工作台页头分支信息区 SHALL 按以下有序规则渲染。术语约定：「来源分支」= 任务 `base_ref` 的展示短名；「当前分支」= worktree 任务的 `branch` 字段，local-path 任务的文件系统当前 checkout 分支。MUST NOT 使用"目标分支"措辞。

1. dir 任务（gitless）：分支信息 span 整体不渲染，MUST NOT 新增路径或占位文本。
2. local-path 任务（repo 项目）：SHALL 展示文件系统当前 checkout 分支（单行仅当前分支，无来源行）；获取失败或分支为空（detached HEAD、git 异常）时不展示，静默降级，MUST NOT 展示占位文本。
3. worktree 任务：保留现有外层渲染条件（当前分支非空时才渲染分支信息区）。
4. worktree 且 `base_ref` 非空：两行展示——上行当前分支、下行来源分支（行序本身表达方向性，无箭头符号），来源分支视觉层级弱于当前分支（更小字号 + 更弱颜色 + 静态"派生自"图标）。
5. worktree 且 `base_ref` 为空串（历史任务或兼容性缺失）：仅展示当前分支单行，与现状完全一致，MUST NOT 展示"未知""-"等占位文本。

来源分支展示文本 SHALL 将全限定 ref 转换为短名：`refs/heads/<name>` → `<name>`；`refs/remotes/<name>` → `<name>`（保留 remote 段，如 `refs/remotes/origin/main` → `origin/main`）。「同名不去重」比较的是转换后的来源短名与当前分支名：两者相同仍正常两行各自展示，MUST NOT 做去重特判。展示分支名的完整未截断文本 SHALL 经 tooltip 可达。窄屏（页头分支信息区既有隐藏断点 ≤1024px）下分支信息随既有行为整段不展示，本期 MUST NOT 为窄屏新增展示。任务列表（指挥中心、项目管理、侧栏）MUST NOT 展示来源分支。

#### Scenario: worktree 任务两行展示当前与来源分支

- **WHEN** 打开 worktree 模式且 `base_ref` 为 `refs/heads/main`、当前分支为 `feature-x` 的任务工作台
- **THEN** 页头分支信息区两行展示：上行当前分支 `feature-x`（提亮）、下行来源分支 `main`（弱化 + 静态派生图标），tooltip 含两者完整文本

#### Scenario: 远端基线保留 remote 段

- **WHEN** 任务的 `base_ref` 为 `refs/remotes/origin/main`
- **THEN** 页头来源分支展示为 `origin/main`

#### Scenario: local-path 任务展示文件系统当前分支

- **WHEN** 打开 local-path 模式（repo 项目）任务的工作台，其路径当前 checkout 分支为 `feature-y`
- **THEN** 页头分支信息区单行展示当前分支 `feature-y`（无来源行）

#### Scenario: local-path 分支获取失败静默降级

- **WHEN** 打开 local-path 任务的工作台，当前分支获取失败（非 401 错误）或返回空串（如 detached HEAD）
- **THEN** 页头不渲染分支信息，不出现占位文本，不设置页面业务错误、不自动重试；401 响应沿用共享 API 客户端的既有认证失效流程，不受本降级语义约束

#### Scenario: dir 任务不渲染分支信息区

- **WHEN** 打开 dir 项目任务的工作台
- **THEN** 页头不渲染分支信息 span，不出现来源分支、路径替代文本或任何占位

#### Scenario: 历史任务 base_ref 空串降级

- **WHEN** 打开 worktree 模式但 `base_ref` 为空串的任务（如本特性上线前创建的历史任务）
- **THEN** 页头仅展示当前分支，与本特性上线前表现一致

### Requirement: 工作台页头分支名点击复制

任务工作台页头展示的每个分支名 SHALL 为独立的可复制控件：点击复制该控件自己显示的短名文本（所见即所得；显示被截断时仍复制完整名）。分支图标（⎇）与来源行的派生图标（↳）MUST 保持静态、不可点击。复制反馈 SHALL 经页面底部 toast 呈现：成功时 toast 含被复制的分支名（如 `已复制 feature-x`）；失败时 toast 指引 tooltip 兜底（如 `复制失败，完整分支名见悬浮提示`），MUST NOT 提供第三种失败 UI。每个分支控件 SHALL 有可发现性信号（复制指针、hover 提亮）与屏幕阅读器可辨的角色标注（aria-label 区分「复制来源分支」与「复制当前分支」）；tooltip SHALL 下沉到各分支控件并包含「点击复制」提示与完整分支文本。剪贴板 API 不可用时 SHALL 走既有降级链（execCommand 兜底）。`base_ref` 为空串时仅当前分支可复制，MUST NOT 渲染禁用态来源控件；来源与当前分支同名时两者 SHALL 照常各自独立可复制。窄屏（页头分支信息区既有隐藏断点 ≤1024px）下复制交互随展示一起不存在，本期无需处理。

#### Scenario: 复制来源分支成功

- **WHEN** 打开 `base_ref` 为 `refs/heads/main`、当前分支为 `feature-x` 的 worktree 任务工作台，点击来源分支 `main`
- **THEN** 剪贴板写入 `main`，页面底部 toast 显示「已复制 main」

#### Scenario: 复制当前分支成功

- **WHEN** 点击页头当前分支 `feature-x`
- **THEN** 剪贴板写入 `feature-x`，页面底部 toast 显示「已复制 feature-x」

#### Scenario: 复制失败反馈

- **WHEN** 点击分支名时剪贴板写入失败（Clipboard API 可用但写入被拒绝，或 API 不可用且 execCommand 降级失败）
- **THEN** 页面底部 toast 提示复制失败并指引 tooltip 兜底，tooltip 内含完整分支文本

#### Scenario: local-path 单分支复制

- **WHEN** 打开 local-path 任务的工作台（页头仅展示当前分支），点击该分支名
- **THEN** 剪贴板写入当前分支名，toast 反馈与双分支态一致

#### Scenario: 同名来源与当前分支各自复制

- **WHEN** 任务的来源短名与当前分支同名（如均为 `main`），分别点击两个分支控件
- **THEN** 两次均正常复制并各自 toast 反馈，不做去重特判

### Requirement: 工作台溢出菜单空态隐藏

任务工作台页头的「更多操作」溢出菜单入口 SHALL 仅在存在至少一个**可见**菜单项时渲染；当所有菜单项的既有显示条件均不满足时，整个菜单入口（按钮及其容器）MUST NOT 渲染，MUST NOT 以置灰占位项替代。菜单项的**可见性**仅由其显示条件决定（init 日志项仅 `init_status = failed` 时可见；删除项仅 `status ≠ active` 时可见，顺序保持"日志→删除"）；菜单项的禁用状态（如过渡状态下删除项 disabled）MUST NOT 参与入口显隐判定。可见菜单项集合随任务状态变化时，入口的显隐 MUST 随之动态更新（同一订阅数据驱动，无额外请求）。菜单项由非空变为空导致入口隐藏时 MUST 同时关闭展开状态；后续菜单项恢复时仅显示关闭的触发器，MUST NOT 自动展开菜单或调用任何操作回调。

#### Scenario: 无可见菜单项时入口隐藏

- **WHEN** 打开 `status = active` 且 `init_status ≠ failed` 的任务工作台
- **THEN** 页头不渲染「更多操作」溢出菜单入口

#### Scenario: 存在可见菜单项时入口出现

- **WHEN** 打开 `init_status = failed` 或 `status ≠ active` 的任务工作台
- **THEN** 页头渲染「更多操作」溢出菜单入口，点开可见满足条件的菜单项

#### Scenario: 过渡状态禁用项保留入口

- **WHEN** 任务处于过渡状态（`isTransitional` 集合：`creating`/`activating`/`suspending`/`deleting`）且 `init_status ≠ failed`
- **THEN** 「更多操作」入口保留渲染，菜单内删除项显示且处于禁用状态

#### Scenario: 状态迁移驱动入口动态显隐

- **WHEN** 当前任务由 `active` 迁移为 `suspended`（删除项变为可见）
- **THEN** 「更多操作」入口随订阅推送的数据更新而出现，无需手动刷新页面

#### Scenario: 展开中入口隐藏后恢复不自动展开

- **WHEN** 溢出菜单处于展开状态，期间可见菜单项变为空（入口隐藏），随后菜单项恢复（入口重新出现）
- **THEN** 恢复后的入口为关闭状态，菜单不自动展开，不调用任何操作回调
