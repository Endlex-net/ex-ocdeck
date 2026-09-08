# web-ui-shell Delta Spec（add-frontend-tool-quick-open）

## MODIFIED Requirements

### Requirement: 路由收敛与旧链重定向

系统 SHALL 将路由收敛为：`#/`（指挥中心）、`#/task/:id`（任务工作台，保留 `?from` 来源感知返回）、`#/projects`（项目管理，`#/projects#<projectID>` 深链选中项目）、`#/configs`（设置，`#appearance|#env|#opencode|#ai|#notifications|#palette|#tools` 深链子标签）。旧路由 MUST 重定向而非 404：`#/active` → `#/`；`#/ai-config` → `#/configs#ai`；`#/project/:id` → `#/projects#<id>`。非法深链 MUST 有恢复路径：`#/projects#<不存在的id>` 回退为不选中任何项目（展示项目列表与空详情占位）；`#/configs#<未知tab>` 回退为 `#appearance`；`#/task/<不存在的id>` 保留现有 notFound 页与返回列表入口。工作台 `?from` 来源感知 MUST 归一为单一映射：`?from ∈ {home, projects, active}`，其中 `active` 为 legacy 别名映射到 `home`（旧 `#/task/:id?from=active` 链接不断）；未知值/缺省 → `home`；返回链接由统一函数解析（`home → #/`、`projects → #/projects#<projectID>`）。

#### Scenario: 旧来源参数兼容

- **WHEN** 用户打开 `#/task/abc?from=active`
- **THEN** 工作台正常打开，返回链接指向 `#/`（指挥中心）

#### Scenario: 未知来源参数回退

- **WHEN** 用户打开 `#/task/abc?from=foobar`（任务 abc 存在）
- **THEN** 工作台正常打开，返回链接指向 `#/`

#### Scenario: 旧活跃会话链接重定向

- **WHEN** 用户打开历史书签 `#/active`
- **THEN** 应用重定向至 `#/`（指挥中心）

#### Scenario: 旧 AI 配置链接重定向

- **WHEN** 用户打开 `#/ai-config`
- **THEN** 应用重定向至 `#/configs#ai` 并选中 AI 子标签

#### Scenario: 旧项目详情链接重定向

- **WHEN** 用户打开 `#/project/abc`
- **THEN** 应用重定向至 `#/projects#abc` 并在项目管理页选中项目 abc

#### Scenario: 非法项目深链回退

- **WHEN** 用户打开 `#/projects#不存在的id`
- **THEN** 项目管理页正常打开，不选中任何项目，展示项目列表与空详情占位，不报错

#### Scenario: 非法设置子标签回退

- **WHEN** 用户打开 `#/configs#foobar`
- **THEN** 设置页打开并回退选中 `#appearance` 子标签，不报错

#### Scenario: 命令面板子标签深链

- **WHEN** 用户打开 `#/configs#palette`
- **THEN** 设置页打开并选中「命令面板」子标签

#### Scenario: 常用工具子标签深链

- **WHEN** 用户打开 `#/configs#tools`
- **THEN** 设置页打开并选中「常用工具」子标签
