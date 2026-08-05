# Task Lifecycle Specification

## Purpose
以独立 git worktree + 分支建模任务，定义活跃/挂起/归档/删除状态机与挂起恢复、删除清理、重启 reconciliation 等生命周期行为。
## Requirements
### Requirement: 任务创建

系统 SHALL 支持在项目下创建任务。每个任务 MUST 拥有独立的 git worktree 与独立分支（从项目默认分支切出）。

分支名 MUST 为 `ocdeck/<slug>`，slug 生成策略：当 AI 配置可用（见 ai-provider-config spec 的可用性判定）时，SHALL 调用 LLM 将任务名提炼为语义化英文 kebab-case slug（≤50 字符，匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$` 且不命中无意义词表，否则视为失败）；LLM 调用失败、超时、输出非法或 AI 未配置时，MUST 回退到机械 slugify（与既有行为一致，空结果兜底 `task`）。**AI 错误本身 MUST NOT 向用户返回、MUST NOT 阻断任务创建**——命名回退后创建流程继续，但随后仍可能因既有的分支名校验/冲突等前置检查失败（该语义不变）。LLM 调用 SHALL 设置超时（≤10s）且发生在任何副作用（落库、worktree add）之前。

新建任务的 worktree 路径 MUST 为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`：`projectName-slug` 为项目名经规范化（小写、非 `[a-z0-9-]` 折叠为 `-`，允许为空）的结果，为空时回退 `project-<projectID前8位>`，且 MUST 截断至 ≤50 字符；`branchPathSlug` 为分支名去掉 `ocdeck/` 前缀后截断至 ≤50 字符的目录段（截断后去尾部 `-`，截空时兜底 `task`）——目录段是分支名的派生展示，**分支名本身行为不变**（机械 slugify 无长度限制），DB 落库的 `worktree_path` 为唯一事实源，MUST NOT 从目录反推分支；`rand4` 为 4 位小写字母数字随机后缀（crypto/rand）。熵失败语义：Go 1.24 起 `crypto/rand` 底层熵失败为不可恢复 fatal（进程终止，天然满足零副作用）；实现保留 error 返回路径作为可注入熵源的防御 seam，**当使用可注入熵源且其返回错误时 MUST 返回错误且零副作用**。目录碰撞检测 MUST 在落库前以无副作用的存在性检查完成：碰撞时重新生成后缀（≤3 次），3 次均碰撞 MUST 返回错误且不产生任何副作用。路径在创建时确定并落库，此后删除/挂起/激活/重试等全部生命周期操作 MUST 按 DB 记录的 `worktree_path` 执行，**MUST NOT 按新格式重算**——既有任务（含旧 `<projectID>/<taskID>` 格式路径）行为不变，不做迁移。worktree 创建在任何文件/git 副作用前 MUST 通过 `<dataDir>/worktrees` 根的包含性校验。

创建流程在 worktree 创建成功后、提交 suspended 前，SHALL 执行项目配置的 inherit 文件继承（语义见 project-lifecycle-config spec），inherit 失败 MUST NOT 阻断创建。提交 suspended 后：若项目配置了 init script，SHALL 先异步执行 init 并仅在成功后触发自动激活；init 失败 MUST NOT 触发激活，任务保持 suspended 且 init_status=failed（init 状态机见 project-lifecycle-config spec）；激活失败任务落挂起并记录 last_error，用户可手动重试激活。项目未配置 inherit/init 时，创建流程与既有行为完全一致。

#### Scenario: 创建任务

- **WHEN** 用户在项目下创建任务（提供任务名称）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: LLM 生成语义化分支名

- **WHEN** AI 配置可用，用户以中文任务名（如「接入AI与worktree命名优化」）创建任务
- **THEN** 系统调用 LLM 生成英文 slug，分支名为 `ocdeck/<ai-slug>`（如 `ocdeck/ai-worktree`），worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<ai-slug>-<rand4>/`（AI 路径下目录段与分支 slug 一致）

#### Scenario: AI 未配置或失败时回退

- **WHEN** AI 未配置、调用失败/超时、或输出未通过清洗门禁
- **THEN** 系统回退到机械 slugify 生成分支名，AI 错误不向用户暴露；创建流程继续，随后仍遵循既有前置检查语义（如分支冲突时报错）

#### Scenario: 新路径格式的人类可读目录

- **WHEN** 创建新任务
- **THEN** worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（branchPathSlug 为分支名去 `ocdeck/` 前缀后截断 ≤50 字符的目录段，分支名本身不变），项目名与分支语义可从路径直接辨认；存量旧格式任务的目录与全部生命周期操作（含创建重试）不受影响

#### Scenario: 纯中文项目名的目录回退

- **WHEN** 项目名规范化后为空（如纯中文项目名）
- **THEN** 目录第一段为 `project-<projectID前8位>`，保证非空、合法、可区分

#### Scenario: 目录碰撞重试

- **WHEN** 落库前的存在性检查发现目标目录已存在
- **THEN** 系统重新生成 4 位随机后缀重试（≤3 次）；3 次均碰撞则返回错误，不产生落库或 worktree 副作用

#### Scenario: 配置 init 的项目创建任务

- **WHEN** 项目配置了 init script，创建任务
- **THEN** worktree 创建 → inherit 复制 → 挂起（init_status=pending）→ init 执行成功 → 自动激活

#### Scenario: init 失败停留在挂起

- **WHEN** 创建链中 init script 执行失败
- **THEN** 任务保持挂起、init_status=failed、init_error 落库，无 serve/tui 会话，用户可查看日志并 Re-run

#### Scenario: 未配置项目行为不变

- **WHEN** 项目未配置 inherit patterns 与 init script，创建任务
- **THEN** 创建流程与既有行为一致：worktree 创建后直接自动激活，init_status=none

#### Scenario: Probe 冷启动重试

- **WHEN** 创建后自动激活时 capability probe 因冷启动超时/网络类错误（ErrServeNotReady）失败
- **THEN** 系统保持 serve 会话并短退避重试（共 3 次尝试，退避 2s/4s）；任一次成功则激活继续；全部失败才落 suspended 并记录 last_error（语义与现状一致）。结构不兼容（ErrCapabilityMismatch）与凭据错误（ErrUnauthorized）不重试

#### Scenario: 分支名冲突

- **WHEN** 生成的分支名已存在（无论由 LLM 生成还是 slugify 回退）
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

系统 SHALL 在删除任务前完成全部前置检查（dirty/untracked 确认、分支被其他 worktree 占用检查、路径包含性校验），**全部通过后才允许任何副作用**。此外，任务 init 进行中（`init_status ∈ {pending,running}`）时 MUST 拒绝删除与归档（invalid_state，提示 init 进行中）。删除副作用 MUST 按序执行：① 持久化 delete_mode + 置 deleting ② **RetryReap 既有 cleanup debt**（remaining 非空则落 deletion_failed，不得继续）③ 删 oc session 数据（逐个，404 幂等视为成功）④ kill 残余 tmux 会话（若有）⑤ 二次 dirty 门禁 ⑥ pre_delete script（项目配置时；worktree 不存在则幂等跳过；语义见 project-lifecycle-config spec）⑦ 删 worktree ⑧ 删本地分支 ⑨ 删 DB 记录 ⑩ best-effort 清理任务日志目录（忽略错误）。远端分支 MUST NOT 被删除。**Force 模式只能跳过 ③ 与 ⑥，MUST NOT 跳过 ② 进程收割**。

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
- **THEN** 系统跳过 oc session 删除（保留 session 数据并提示残留）与 pre_delete script，完成其余清理

#### Scenario: init 进行中拒绝删除

- **WHEN** 任务 init_status 为 pending 或 running，用户执行删除或归档
- **THEN** 系统拒绝并提示 init 进行中，任务与 worktree 保持原状

#### Scenario: 配置 pre-delete 的删除顺序

- **WHEN** 项目配置了 pre_delete script，删除任务
- **THEN** pre_delete script 在 kill 残余会话与二次 dirty 门禁之后、worktree 移除之前执行；脚本失败落 deletion_failed，可重试或强制删除

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

### Requirement: 任务 init 状态可见性

任务 DTO SHALL 携带 init_status 与 init_error 字段，UI SHALL 在任务列表/工作台展示全部 init 状态：`pending|running` 显示"init 进行中"标识；`failed` 显示失败标识与日志查看入口；`none|succeeded` 不显示 init 徽标；Re-run 入口 MUST 仅在 `status=suspended 且 init_status ∈ {failed,succeeded}` 时提供（与后端门禁一致，archived 任务不出现必然被拒的按钮）；init_status 非 `none|succeeded` 时全部激活入口（含工作台内联激活按钮）MUST 禁用并说明原因；任务详情 MUST 始终提供 init 日志查看入口（inherit 警告经此可见）。删除因 pre_delete script 失败（deletion_failed，以 last_error 的 `pre-delete:` 前缀识别）时，UI SHALL 提供 pre-delete 日志查看入口；删除确认弹窗的 Force 选项 SHALL 说明其同时跳过 pre-delete 脚本。

#### Scenario: init 失败的任务展示

- **WHEN** 任务 status=suspended 且 init_status=failed
- **THEN** UI 显示失败标识，提供日志查看与 Re-run 按钮，激活按钮禁用并提示需先 Re-run init；archived 任务不提供 Re-run 入口

#### Scenario: pre-delete 失败可诊断

- **WHEN** 任务因 pre_delete script 失败落 deletion_failed
- **THEN** UI 提供 pre-delete 日志查看入口，用户可据日志修复后重试或强制删除

