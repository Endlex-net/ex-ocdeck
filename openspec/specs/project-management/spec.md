# Project Management Specification

## Purpose
将本机 git 仓库注册为项目并提供列表、详情与删除管理，约定任务 worktree 统一存放于 ocdeck 数据目录下，避免污染源仓库。

## Requirements

### Requirement: 项目注册
系统 SHALL 允许用户将本机目录注册为项目。注册时 MUST 显式指定项目类型 `kind ∈ {repo, dir}`（请求字段 `kind`，缺省 `repo`，向后兼容；非法值 MUST 返回 invalid_input / HTTP 422）。

`kind=repo`：系统 MUST 校验该路径存在且为 git 仓库（含 `.git` 或 `git rev-parse` 校验），并记录项目的默认分支。

`kind=dir`（纯目录项目）：系统 MUST NOT 要求该路径为 git 仓库，MUST 仅校验路径存在且为目录；MUST NOT 探测默认分支（`default_branch` 记录为空）。系统 MUST NOT 根据 `IsGitRepo` 失败隐式推断 `kind=dir`——类型只来自用户显式指定。

项目 DTO SHALL 暴露项目 `kind`；任务 DTO SHALL 暴露 `project_kind`（`repo|dir`），供 UI 标识与降级。

#### Scenario: 注册合法仓库
- **WHEN** 用户以 `kind=repo`（或缺省）提交一个存在的 git 仓库绝对路径与项目名称
- **THEN** 系统创建项目记录并返回项目详情（含默认分支与 `kind=repo`）

#### Scenario: 拒绝非仓库路径
- **WHEN** 用户以 `kind=repo` 提交的路径不存在或不是 git 仓库
- **THEN** 系统拒绝注册并返回明确错误原因

#### Scenario: 注册纯目录项目
- **WHEN** 用户以 `kind=dir` 提交一个存在的目录绝对路径（无论其是否为 git 仓库）
- **THEN** 系统创建项目记录，`kind=dir`、`default_branch` 为空，不执行任何 git 校验

#### Scenario: dir 拒绝不存在或非目录路径
- **WHEN** 用户以 `kind=dir` 提交的路径不存在或不是目录
- **THEN** 系统拒绝注册并返回明确错误原因

#### Scenario: 拒绝隐式推断
- **WHEN** 用户以 `kind=repo` 提交一个非 git 仓库路径
- **THEN** 系统拒绝注册（而非自动降级为 dir 项目），错误信息提示可显式选择纯目录类型

### Requirement: 项目列表与详情
系统 SHALL 提供项目列表与单项目详情查询，详情包含该项目下的任务数量与任务状态分布。项目列表（`GET /api/v1/projects`）的每个元素 MUST 附加 `tasks` 摘要数组，摘要元素字段为 `id`、`name`、`status`、`init_status`、`branch`、`worktree_path`、`last_error`、`notice`（形状与现有任务 DTO 一致：`NoticeItem[]`，无 notice 时省略，见 `web/src/types.ts:210` `parseNotice`）、`updated_at`（Unix 秒）、`agentStatus`（活跃任务水合，不可用时省略；命名与 `GET /sessions/active` 一致）、`attention_count`（pending 权限 + 问题总数，见 agent-attention spec）；该字段为可加性扩展，MUST NOT 改变既有字段语义。任务摘要 MUST 覆盖该项目的全部非删除态任务（活跃、挂起、归档、过渡与失败态）；项目无任务时 `tasks` MUST 为空数组 `[]`（MUST NOT 为 `null`）。

失败与只读语义（与 `GET /sessions/active` 对齐）：摘要查询失败 MUST 返回 500 标准错误信封且 MUST NOT 开始 agentStatus 水合；agentStatus 水合 MUST 并发执行（每请求并发上限 8，预算 3 秒），单任务水合失败/超时 MUST 降级为该字段省略，MUST NOT 导致整个请求失败；完整请求路径 MUST 为纯读操作（MUST NOT 写数据库、触发 align、改变任务状态机或启动/停止进程）。

#### Scenario: 查看项目列表
- **WHEN** 用户请求项目列表
- **THEN** 系统返回全部已注册项目及其任务概况，每个项目元素携带 `tasks` 摘要数组

#### Scenario: 摘要覆盖多状态任务

- **WHEN** 项目 A 有 1 个活跃任务、1 个挂起任务、1 个归档任务
- **THEN** 项目 A 的 `tasks` 摘要包含全部 3 个任务，状态字段各自正确

#### Scenario: 注意力计数

- **WHEN** 项目 A 的某活跃任务有 2 个 pending 权限请求与 1 个 pending 问题请求
- **THEN** 该任务摘要的 `attention_count` 为 3

#### Scenario: 无任务项目返回空数组

- **WHEN** 项目 B 下没有任何任务
- **THEN** 项目 B 的 `tasks` 字段为 `[]`（非 `null`）

#### Scenario: 摘要查询失败不水合

- **WHEN** 底层摘要查询返回错误
- **THEN** 返回 500 标准错误信封，且不发起任何 agentStatus 水合调用

#### Scenario: 单任务水合失败降级

- **WHEN** 某活跃任务的 serve 实例不可达或水合超时
- **THEN** 返回 200，该任务摘要的 `agentStatus` 省略，其余任务不受影响

### Requirement: 项目管理单页（master-detail）

系统 SHALL 将项目列表与项目详情合并为单页（`#/projects`）：左侧为项目轨道列（搜索 + 项目列表 + 注册项目入口），右侧为选中项目的详情面板（头部含名称/类型徽章/路径/删除项目；子标签含概览（健康摘要 + 任务行）、自动化（init 脚本/文件继承/pre-delete 脚本）、环境变量）。`#/projects#<projectID>` 深链 MUST 直接选中对应项目。≤1024px 视口 MUST 转为列表⇄详情钻取式导航（详情视图提供返回列表入口）。任务行 MUST 完整呈现状态机（活跃/挂起/归档/过渡/失败/init 状态与对应操作集），操作行为与现有项目详情页一致。项目删除 MUST 保留两步确认与"仍有任务时禁止删除"语义；注册表单 MUST 保留 repo/dir 类型选择与上下文提示。

新建任务入口 MUST 收敛至指挥中心：本单页 MUST NOT 提供新建任务表单；概览子标签 MUST 提供"前往指挥中心新建任务"的提示链接。

#### Scenario: 选择项目查看详情

- **WHEN** 用户在项目管理页点击左侧某项目
- **THEN** 右侧展示该项目的详情面板，URL 更新为 `#/projects#<id>` 深链

#### Scenario: 深链直达项目

- **WHEN** 用户打开 `#/projects#abc`
- **THEN** 项目管理页打开且直接选中项目 abc 的详情

#### Scenario: 窄屏钻取导航

- **WHEN** 用户在 ≤1024px 视口打开项目管理页并点击某项目
- **THEN** 视图切换为该项目的详情，并提供返回项目列表的入口

#### Scenario: 详情页操作等价

- **WHEN** 用户在新单页中编辑环境变量或生命周期配置
- **THEN** 行为与 API 契约与原项目详情页完全一致

#### Scenario: 新建任务入口在指挥中心

- **WHEN** 用户在项目管理单页查看项目概览
- **THEN** 页面不提供新建任务表单，展示"前往指挥中心新建任务"提示链接，点击跳转 `#/`

### Requirement: 项目删除
系统 SHALL 允许删除项目。当项目下仍存在未删除的任务时，系统 MUST 拒绝删除并提示先处理任务。

#### Scenario: 删除含任务的项目
- **WHEN** 用户删除一个仍有任务的项目
- **THEN** 系统拒绝并提示存在活跃/挂起/归档任务

#### Scenario: 删除空项目
- **WHEN** 用户删除一个无任何任务的项目
- **THEN** 系统删除项目记录（不触碰磁盘上的仓库）

### Requirement: worktree 存放位置约定
系统 SHALL 将 `kind=repo` 项目的任务 worktree 统一创建在 ocdeck 自有数据目录下：`<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（路径格式详见 task-lifecycle spec 任务创建要求；存量旧格式目录行为不变），MUST NOT 创建在源仓库内部。路径 MUST 以 canonical path + `filepath.Rel` 语义校验包含性。

`kind=dir` 项目的任务 MUST NOT 创建 worktree：任务运行目录直接锚定为项目路径本身（`worktree_path` 记录为项目路径）。ocdeck 的任务创建内建步骤在 init/激活开始前 MUST NOT 在项目目录内创建任何文件或子目录；用户授权的 init script、pre-delete script 与 agent 会话行为除外。

#### Scenario: 创建任务时的 worktree 路径
- **WHEN** repo 项目下创建任务
- **THEN** worktree 落于 `<dataDir>/worktrees/` 下按任务创建要求生成的路径，源仓库目录不被污染

#### Scenario: dir 项目任务不创建 worktree
- **WHEN** dir 项目下创建任务
- **THEN** 任务记录的 `worktree_path` 等于项目路径；init/激活开始前磁盘上无新增目录或文件（用户授权的 init script 与 agent 会话行为除外）