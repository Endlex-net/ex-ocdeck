## MODIFIED Requirements

### Requirement: 任务创建

系统 SHALL 支持在项目下创建任务。每个任务 MUST 拥有独立的 git worktree 与独立分支（从项目默认分支切出）：分支名由任务名称生成（`ocdeck/<slug>`），worktree 路径 MUST 为 `<dataDir>/worktrees/<projectID>/<taskID>/`（路径以 taskID 标识，不随任务名变化）。创建流程在 worktree 创建成功后、提交 suspended 前，SHALL 执行项目配置的 inherit 文件继承（语义见 project-lifecycle-config spec），inherit 失败 MUST NOT 阻断创建。提交 suspended 后：若项目配置了 init script，SHALL 先异步执行 init 并仅在成功后触发自动激活；init 失败 MUST NOT 触发激活，任务保持 suspended 且 init_status=failed（init 状态机见 project-lifecycle-config spec）；激活失败任务落挂起并记录 last_error，用户可手动重试激活。项目未配置 inherit/init 时，创建流程与既有行为完全一致。

#### Scenario: 创建任务

- **WHEN** 用户在项目下创建任务（提供任务名称）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: 配置 init 的项目创建任务

- **WHEN** 项目配置了 init script，创建任务
- **THEN** worktree 创建 → inherit 复制 → 挂起（init_status=pending）→ init 执行成功 → 自动激活

#### Scenario: init 失败停留在挂起

- **WHEN** 创建链中 init script 执行失败
- **THEN** 任务保持挂起、init_status=failed、init_error 落库，无 serve/tui 会话，用户可查看日志并 Re-run

#### Scenario: 未配置项目行为不变

- **WHEN** 项目未配置 inherit patterns 与 init script，创建任务
- **THEN** 创建流程与既有行为一致：worktree 创建后直接自动激活，init_status=none

#### Scenario: 分支名冲突

- **WHEN** 生成的分支名已存在
- **THEN** 系统报错并提示用户更换任务名称

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

## ADDED Requirements

### Requirement: 任务 init 状态可见性

任务 DTO SHALL 携带 init_status 与 init_error 字段，UI SHALL 在任务列表/工作台展示全部 init 状态：`pending|running` 显示"init 进行中"标识；`failed` 显示失败标识与日志查看入口；`none|succeeded` 不显示 init 徽标；Re-run 入口 MUST 仅在 `status=suspended 且 init_status ∈ {failed,succeeded}` 时提供（与后端门禁一致，archived 任务不出现必然被拒的按钮）；init_status 非 `none|succeeded` 时全部激活入口（含工作台内联激活按钮）MUST 禁用并说明原因；任务详情 MUST 始终提供 init 日志查看入口（inherit 警告经此可见）。删除因 pre_delete script 失败（deletion_failed，以 last_error 的 `pre-delete:` 前缀识别）时，UI SHALL 提供 pre-delete 日志查看入口；删除确认弹窗的 Force 选项 SHALL 说明其同时跳过 pre-delete 脚本。

#### Scenario: init 失败的任务展示

- **WHEN** 任务 status=suspended 且 init_status=failed
- **THEN** UI 显示失败标识，提供日志查看与 Re-run 按钮，激活按钮禁用并提示需先 Re-run init；archived 任务不提供 Re-run 入口

#### Scenario: pre-delete 失败可诊断

- **WHEN** 任务因 pre_delete script 失败落 deletion_failed
- **THEN** UI 提供 pre-delete 日志查看入口，用户可据日志修复后重试或强制删除
