# Task Lifecycle Specification

## Purpose
以独立 git worktree + 分支建模任务，定义活跃/挂起/归档/删除状态机与挂起恢复、删除清理、重启 reconciliation 等生命周期行为。

## Requirements

### Requirement: 任务创建
系统 SHALL 支持在项目下创建任务。每个任务 MUST 拥有独立的 git worktree 与独立分支（从项目默认分支切出）：分支名由任务名称生成（`ocdeck/<slug>`），worktree 路径 MUST 为 `<dataDir>/worktrees/<projectID>/<taskID>/`（路径以 taskID 标识，不随任务名变化）。

#### Scenario: 创建任务
- **WHEN** 用户在项目下创建任务（提供任务名称）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: 分支名冲突
- **WHEN** 生成的分支名已存在
- **THEN** 系统报错并提示用户更换任务名称

### Requirement: 任务状态机
任务 SHALL 具有三种用户可见状态：活跃、挂起、归档；删除为**硬删除**（成功后记录移除，无"已删除"展示态）。合法流转 MUST 限定为：挂起→活跃、活跃→挂起、挂起→归档、归档→挂起、活跃→挂起（自动，kill 模式服务端重启；persist 模式重启且会话存活时保持活跃）、挂起/归档/创建失败→删除。系统 SHALL 额外维护内部过渡状态（creating/creation_failed/activating/suspending/deleting/deletion_failed）与 `last_error` 字段，用于表达部分失败与恢复；对过渡态任务的 Retry 操作 MUST 幂等（外部资源不存在视为该步骤已成功）。

#### Scenario: 激活挂起任务
- **WHEN** 用户对挂起任务执行激活
- **THEN** 系统启动该任务的 opencode 会话组（serve + TUI）并置为活跃；中途失败则回退为挂起并记录 last_error

#### Scenario: 挂起活跃任务
- **WHEN** 用户对活跃任务执行挂起
- **THEN** 系统停止该任务全部 tmux 会话、保留 worktree 并置为挂起

挂起结果 MUST 按以下互斥决策树收敛（按序判定取首个命中分支）：

#### Scenario: 分支 a — serve 已死
- **WHEN** 挂起过程中发现 serve 进程已退出
- **THEN** 系统继续完成剩余清理后落为挂起；个别会话杀不掉时注册残留会话、记录 notice 并后台重试

#### Scenario: 分支 b — serve 存活且清理全部成功
- **WHEN** serve 存活且全部会话终止成功
- **THEN** 任务落为挂起

#### Scenario: 分支 c — serve 存活但清理部分失败
- **WHEN** serve 仍存活但存在 kill 失败
- **THEN** 系统尝试修复运行时（重订阅 SSE、重开 attach）；修复成功回活跃 + last_error；修复失败或期间 serve 死亡则转分支 a 落挂起

#### Scenario: 存在残留时拒绝激活
- **WHEN** 任务存在未清理的旧代残留会话或残留进程 cleanup debt，用户执行激活
- **THEN** 系统拒绝激活并提示先清理或强制删除（管理面与强制删除保持可用）

#### Scenario: 归档与恢复
- **WHEN** 用户对挂起任务执行归档
- **THEN** 任务收起展示但 worktree 保留，且可恢复为挂起

#### Scenario: 创建中途失败
- **WHEN** 创建任务时 worktree 已建但 DB 写入失败
- **THEN** 任务呈 creation_failed 并记录 last_error，用户可重试（仅补 DB 写）或删除（清理 worktree 与分支）

### Requirement: 挂起后恢复会话
系统 SHALL 持久化任务关联的 opencode session ID（经 serve SSE 事件捕获与全量对齐，取 `last_seen_at` 最大者）。**激活 MUST 立即锚定确定 session，MUST NOT 使用 `--continue`**（其"目录最近会话"语义不等于本任务会话）：有记录时以 `attach --session <最近 sessionID>` 恢复本任务会话（启动前经 `GET /session/:id` 预检）；**无记录或预检 404 时，ocdeck MUST 先经 `POST /session` 创建新会话并持久化到 task_sessions，再以 `attach --session <newID>` 启动**——任务首次激活即产生确定 session 归属，用户未输入任何内容也可确定性恢复。认证失败、网络失败等其他 attach 失败 MUST NOT 触发任何回退。

#### Scenario: 按 session ID 恢复
- **WHEN** 用户激活一个曾活跃过且有 session 记录的任务
- **THEN** attach 以 `--session <id>` 恢复，用户看到本任务之前的对话历史

#### Scenario: session 已不存在
- **WHEN** 记录的 sessionID 已被删除（serve 返回 404）
- **THEN** ocdeck 经 REST 创建新会话并持久化，attach 以 `--session <newID>` 启动（不使用 --continue）

#### Scenario: 无 session 记录
- **WHEN** 任务无任何 session 记录
- **THEN** ocdeck 经 REST 创建新会话并持久化到 task_sessions，attach 以 `--session <newID>` 启动，任务立即拥有确定 session 归属

### Requirement: 任务删除清理
系统 SHALL 在删除任务前完成全部前置检查（dirty/untracked 确认、分支被其他 worktree 占用检查、路径包含性校验），**全部通过后才允许任何副作用**。删除副作用 MUST 按序执行：① 持久化 delete_mode + 置 deleting ② **RetryReap 既有 cleanup debt**（remaining 非空则落 deletion_failed，不得继续）③ 删 oc session 数据（逐个，404 幂等视为成功）④ kill 残余 tmux 会话（若有）⑤ 删 worktree ⑥ 删本地分支 ⑦ 删 DB 记录。远端分支 MUST NOT 被删除。**Force 模式只能跳过 ③，MUST NOT 跳过 ② 进程收割**。

#### Scenario: 删除挂起任务
- **WHEN** 用户删除一个挂起任务并完成 dirty 确认（如有）
- **THEN** 系统按序完成全部清理，任务记录移除

#### Scenario: 进程已死时删除
- **WHEN** 删除任务时其 opencode 进程不存在（如服务端崩溃后）
- **THEN** 系统临时启动一次性 serve 完成 session 删除（不直接操作 opencode DB），其余清理照常

#### Scenario: 删除中途失败
- **WHEN** 删除任一步骤失败
- **THEN** 任务进入 deletion_failed 状态并记录 last_error，允许用户重试（幂等，从失败步骤继续）或选择"强制删除"

#### Scenario: 强制删除
- **WHEN** 用户对删除失败的任务选择强制删除
- **THEN** 系统跳过 oc session 删除（保留 session 数据并提示残留），完成其余清理

#### Scenario: dirty worktree 删除确认
- **WHEN** 删除的任务 worktree 存在未提交或未跟踪文件
- **THEN** 系统提示变更内容并要求显式确认后才继续

#### Scenario: 分支被占用
- **WHEN** 任务分支被其他 worktree 使用中
- **THEN** 系统拒绝删除并说明占用方

### Requirement: 服务端重启语义
服务端进程的生命周期语义 SHALL 由关停策略（shutdownPolicy）决定：persist 模式下任务进程托管于 tmux 会话、不随服务端退出而终止，服务端重启后 MUST 对 **active/activating** 中会话存活且无 cleanup debt 的任务恢复活跃（重订阅 SSE + 全量对齐），会话已消失的任务落为挂起；**suspending 任务 MUST 完成清理落为挂起**（以持久化意图为准，不得恢复活跃）；**archived/creation_failed/deletion_failed 等持久状态 MUST 保持原状**（仅清理其异常会话）；kill_on_start / kill_immediate 模式下服务端退出则全部任务进程被终止（立即或下次启动清理），重启后 active/activating/suspending 任务 MUST 收敛为挂起（其余持久状态保持原状），由用户手动激活恢复。

#### Scenario: 服务端重启后（persist）
- **WHEN** 服务端重启（persist 模式，此前有活跃任务）
- **THEN** active/activating 中 serve 健康且无 cleanup debt 的任务自动恢复活跃，用户打开终端即见 agent 当前状态；suspending 完成清理落挂起；其余持久状态保持原状

#### Scenario: 服务端重启后（kill 模式）
- **WHEN** 服务端重启（kill_on_start 或 kill_immediate 模式）
- **THEN** active/activating/suspending 任务显示为挂起（archived/creation_failed/deletion_failed 保持原状），用户可逐个激活恢复会话

### Requirement: 无人工并发配额
系统 SHALL NOT 设置并行活跃任务数量的人工配额；当端口、文件描述符或磁盘等资源耗尽时 MUST 返回明确错误而非静默失败。

#### Scenario: 并行多任务
- **WHEN** 用户同时激活多个任务
- **THEN** 系统为每个任务独立运行 tmux 会话组，互不干扰

#### Scenario: 端口范围耗尽
- **WHEN** 可配置端口范围内无可用端口
- **THEN** 激活失败并提示端口耗尽，任务回退挂起