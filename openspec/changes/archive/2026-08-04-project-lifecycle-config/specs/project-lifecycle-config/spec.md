## ADDED Requirements

### Requirement: 生命周期配置存储

系统 SHALL 为每个项目持久化三项生命周期配置：inherit patterns（每行一个 glob）、init script、pre_delete script。配置以项目为作用域整体读写（GET 返回全量三字段，PUT 整体替换 upsert），三字段均可为空（空 = 不执行）。未配置过的项目读取时 SHALL 返回三字段空串。项目删除时其生命周期配置 MUST 级联删除。

#### Scenario: 未配置项目读取返回空配置

- **WHEN** 对从未保存过生命周期配置的项目执行 GET
- **THEN** 返回 inherit_patterns、init_script、pre_delete_script 均为空串，HTTP 200

#### Scenario: 整体替换保存

- **WHEN** PUT 提交三字段全量内容
- **THEN** 后续 GET 返回完全相同的内容；再次 PUT 仅含部分字段变化时，未变字段保持本次提交值

#### Scenario: 项目删除级联

- **WHEN** 删除一个已保存生命周期配置的项目
- **THEN** 其生命周期配置行随项目级联删除

### Requirement: 生命周期配置校验与生效时机

系统 SHALL 在保存时校验 inherit patterns 逐行 glob 语法：非法行 MUST 拒绝整个 PUT 并返回 invalid_input 错误（HTTP 422）且指明行号；空行与 `#` 开头注释行 MUST 忽略。脚本字段长度 MUST 不超过 64KB，inherit_patterns 整体长度 MUST 不超过 16KB。每次脚本执行尝试（创建链 init、Re-run、删除 Retry 的 pre-delete）SHALL 在开始时读取一次当时的配置作为本次执行内容；执行中不受后续配置修改影响，修改后的脚本供后续执行尝试使用。

#### Scenario: 非法 glob 拒绝保存

- **WHEN** PUT 的 inherit_patterns 第 3 行是非法 glob
- **THEN** 返回 invalid_input（HTTP 422），错误信息指明第 3 行，配置未被修改

#### Scenario: 空行与注释被忽略

- **WHEN** inherit_patterns 含空行与 `#` 注释行
- **THEN** 保存成功，执行 inherit 时这些行不参与匹配

#### Scenario: 执行中修改配置不影响本次执行

- **WHEN** init 脚本执行期间用户 PUT 修改了 init_script
- **THEN** 本次执行继续使用启动时读到的脚本；下一次 Re-run 使用新脚本

### Requirement: Inherit 文件继承

任务创建时（worktree 创建成功后、提交 suspended 前），系统 SHALL 以文件级粒度枚举主仓库中 gitignored 与 untracked 的文件（枚举 MUST 展开未跟踪目录与 ignored 目录至文件级记录，不得把目录内已跟踪文件卷入），按 inherit patterns 过滤后复制进新 worktree，保持相对路径与文件权限；符号链接 MUST 按链接复制；普通文件 MUST no-clobber 原子发布（同目录临时文件完整写入后以不占位方式发布——目标已存在或并发出现时 MUST 跳过且不覆盖，任何时刻不得留下部分内容的目标文件；目标路径任一祖先为符号链接时 MUST 拒绝）。`.git` 内容 MUST NOT 被复制。目标路径 MUST 通过 containment 校验（拒绝绝对路径与 `..` 逃逸）。retryCreate（creation_failed 重入）SHALL 无论 worktree 是复用还是重建都重新枚举并幂等执行 inherit，消除"worktree.Add 后、inherit 前崩溃"导致的永久漏拷窗口。inherit 的全部机制失败（枚举、glob 匹配、单文件复制）MUST 一律降级为警告（写入任务 inherit 日志），MUST NOT 阻断任务创建与激活；inherit 日志写入失败本身时警告可丢弃或写服务端日志，同样 MUST NOT 阻断；创建链唯一可因 inherit 阻断的前置是配置读取失败。

#### Scenario: gitignored 配置文件被继承

- **WHEN** 项目 inherit_patterns 含 `.env`，主仓库根目录存在 gitignored 的 `.env`，创建任务
- **THEN** 新 worktree 根目录存在内容一致的 `.env`，任务正常进入后续流程

#### Scenario: 已跟踪文件不被复制

- **WHEN** inherit_patterns 的 glob 同时匹配到一个已 git 跟踪的文件
- **THEN** 该文件不通过 inherit 复制（worktree 检出已含该文件），不计警告

#### Scenario: 单文件复制失败仅警告

- **WHEN** 匹配到的某文件因权限不可读
- **THEN** 警告写入任务 inherit 日志，任务创建继续，init 与激活不受影响

#### Scenario: retryCreate 幂等重跑继承

- **WHEN** 创建在 worktree.Add 后、inherit 执行前因崩溃或配置读取失败落 creation_failed，用户 Retry
- **THEN** retryCreate 重新枚举并执行 inherit，缺失文件被补齐，已存在文件自动跳过

### Requirement: Init 脚本一次性执行

项目配置了 init script 时，系统 SHALL 在任务创建提交 suspended 后异步执行一次该脚本：cwd 为任务 worktree，经 `/bin/sh -c` 非交互执行，超时 10 分钟；脚本以 Manager 持有的独立 runner ctx 执行（不随 HTTP 请求取消，也不复用服务端 signal ctx——见优雅关停要求）。init script MUST 幂等（SIGKILL/崩溃可能遗留仍在运行的旧脚本进程，Re-run 可能与之并行）。任务 SHALL 持久化 init_status（`none|pending|running|succeeded|failed`），且提交 suspended 与 init_status 置位 MUST 为单条原子更新（不存在 suspended 已提交而 init_status 未置位的窗口）。状态迁移 MUST 使用条件更新（CAS）：`pending→running` 要求任务仍为 suspended 且 init_status=pending；`failed|succeeded→running`（Re-run）要求任务仍为 suspended；`running→succeeded|failed` 要求 init_status=running。执行成功 MUST 置 `succeeded` 并触发自动激活；执行失败（非零退出、超时）MUST 置 `failed` 并记录 init_error，MUST NOT 触发激活。未配置 init script 的任务 `init_status` MUST 为 `none`，创建后直接自动激活（现状行为不变）。

#### Scenario: init 成功后自动激活

- **WHEN** 项目配置了 init script，创建任务且脚本 exit 0
- **THEN** init_status 变为 succeeded，任务随后自动进入激活流程

#### Scenario: init 失败不激活

- **WHEN** init script 非零退出
- **THEN** init_status=failed、init_error 含退出信息，任务保持 suspended，无 serve/tui 会话被创建

#### Scenario: 未配置时行为不变

- **WHEN** 项目未配置 init script，创建任务
- **THEN** init_status=none，任务创建后直接自动激活，与既有行为一致

#### Scenario: init 超时

- **WHEN** init script 执行超过 10 分钟
- **THEN** 脚本进程组被杀，init_status=failed，init_error 指明超时

#### Scenario: 并发 claim 只有一个执行者

- **WHEN** 两个执行者同时对同一 pending 任务 claim init
- **THEN** 条件更新仅一个成功，脚本只被执行一次

### Requirement: 激活与生命周期操作的 init 门禁

任务激活（含创建链自动激活与手动激活）SHALL 检查 init_status：`none|succeeded` 放行；`pending|running` MUST 拒绝（invalid_state，提示 init 进行中）；`failed` MUST 拒绝（invalid_state，含 init_error 与 Re-run 指引）；未知或空 init_status MUST fail-closed 拒绝。init 失败的任务在 Re-run 成功前 MUST NOT 建立 serve/tui 会话。Delete 与 Archive SHALL 在 `init_status ∈ {pending,running}` 时拒绝（invalid_state，提示 init 进行中），保证 init 执行期间任务状态保持 suspended 且 worktree 不被移除。

#### Scenario: init 进行中拒绝激活

- **WHEN** 任务 init_status=running，用户手动激活
- **THEN** 返回 invalid_state 错误，任务状态不变

#### Scenario: init 失败拒绝激活

- **WHEN** 任务 init_status=failed，用户手动激活
- **THEN** 返回 invalid_state 错误，错误信息含 init_error 摘要

#### Scenario: init 进行中拒绝删除

- **WHEN** 任务 init_status=running，用户删除或归档该任务
- **THEN** 返回 invalid_state 错误，worktree 与任务保持原状，init 执行不受影响

#### Scenario: Re-run 成功后可以激活

- **WHEN** init_status=failed 的任务 Re-run 成功
- **THEN** init_status=succeeded，手动激活放行

### Requirement: Init 手动重跑

系统 SHALL 提供 Re-run init 入口：仅任务处于 **suspended** 且 `init_status ∈ {failed, succeeded}` 时可调用（其余状态 MUST 拒绝：active 任务须先挂起；`pending|running` 拒绝重复执行）。Re-run 的门禁检查与 CAS claim MUST 在与 Activate/Delete/Archive 相同的任务级互斥锁下完成（防止 `succeeded→running` 与并发激活/删除形成非法状态组合）；脚本执行异步进行、不持锁。CAS 竞争失败 MUST 返回 conflict。Re-run 成功 MUST NOT 自动触发激活（由用户手动激活）。每次执行 MUST 覆盖重写 init 日志。

#### Scenario: failed 状态重跑成功

- **WHEN** 对 suspended 且 init_status=failed 的任务调用 Re-run 且脚本 exit 0
- **THEN** init_status=succeeded，init_error 清空，任务保持 suspended 等待手动激活

#### Scenario: 活跃任务拒绝重跑

- **WHEN** 对 active 任务调用 Re-run
- **THEN** 返回 invalid_state 错误，提示先挂起任务

#### Scenario: running 中拒绝重复重跑

- **WHEN** 对 init_status=running 的任务调用 Re-run
- **THEN** 返回错误，执行中的脚本不受影响

### Requirement: Init 状态重启收敛

服务端启动 Reconcile 时，SHALL 将 `init_status ∈ {pending, running}` 的任务批量收敛为 `failed`，init_error 记录 "interrupted by server restart"。该收敛 MUST 在既有启动恢复步骤（cleanup debt 恢复、任务运行时恢复）之前完成；收敛更新失败 MUST fail-closed 阻止服务端开放 HTTP。系统 MUST NOT 在重启后自动重跑 init；init 已 `succeeded` 而未及激活的任务（崩溃窗口）MUST 保持 suspended 等待手动激活，MUST NOT 由 Reconcile 自动激活。SIGKILL/崩溃可能遗留仍在运行的旧脚本进程，Reconcile 只收敛 DB 状态，不负责定位回收旧进程（脚本幂等要求见 init 执行要求）。

#### Scenario: 重启收敛 running 任务

- **WHEN** 服务端在任务 init_status=running 时退出并重启
- **THEN** 重启后该任务 init_status=failed，init_error 指明被重启打断，用户可 Re-run

#### Scenario: succeeded 未激活的任务重启后不自动激活

- **WHEN** 服务端在 init 成功落库后、自动激活触发前崩溃并重启
- **THEN** 任务保持 suspended + succeeded，不自动激活，用户手动激活

#### Scenario: 收敛失败拒绝开放服务

- **WHEN** 启动收敛的数据库更新失败
- **THEN** Reconcile 失败，服务端不开放 HTTP（与 cleanup debt 恢复失败同级处理）

### Requirement: 脚本执行环境

init 与 pre_delete 脚本 SHALL 使用与任务会话一致的 env 分层（baseline < global < project < task < 生命周期变量），但 MUST NOT 注入 `OCDECK_SERVE_PORT`，且不持久化 env 快照。生命周期变量 MUST 含 `OCDECK_TASK_ID`、`OCDECK_TASK_NAME`、`OCDECK_TASK_PATH`、`OCDECK_PROJECT_PATH`。脚本超时到期 MUST 杀整个进程组，不得遗留孙子进程。执行链中配置读取、env 合并、日志文件创建或脚本执行的任何失败 MUST 按该脚本所属流程的失败语义落账（init → init_status=failed；pre-delete → deletion_failed），且 MUST NOT 触发后续副作用（init 失败不得激活；pre-delete 失败不得移除 worktree）。

#### Scenario: 脚本读到项目级环境变量

- **WHEN** 项目级 env 配置 `FOO=bar`，init script 执行 `echo $FOO`
- **THEN** init.log 含 `bar`；且脚本环境中不存在 OCDECK_SERVE_PORT

#### Scenario: 超时杀进程组

- **WHEN** init script 派生子进程后整体超过超时
- **THEN** 整个进程组被终止，无残留子孙进程

#### Scenario: 执行链前置步骤失败按所属流程落账

- **WHEN** init 执行前配置读取或日志文件创建失败
- **THEN** init_status=failed 且不触发激活；同样失败发生在 pre-delete 执行前则落 deletion_failed 且不移除 worktree

### Requirement: 生命周期日志

日志 SHALL 按单一写入者管理，存放于 `<dataDir>/logs/<taskID>/`：`inherit.log`（仅 inherit 步骤写入，每次执行 inherit 时重写，本次无警告则删除既有文件；写入失败仅丢警告、不阻断创建）、`init.log`（仅 init 执行写入，每次执行覆盖重写）、`pre-delete.log`（仅 pre-delete 执行写入，每次执行覆盖重写）。**1MB 上限统一适用于三个日志文件**：脚本输出超限截断并追加截断标记；inherit 警告超限同样截断并追加标记。系统 SHALL 提供 init 日志读取 API（响应 = inherit 警告节 + init.log，tail ≤64KB）与 pre-delete 日志读取 API（tail ≤64KB）；无日志文件时返回 200 空内容。ocdeck 自身 MUST NOT 向日志写入 env 值；UI MUST 在脚本编辑器旁提示"脚本输出会落盘，勿打印敏感凭据"。任务删除成功（DB 行删除）后 SHALL best-effort 删除该任务日志目录（忽略错误）。执行链在脚本启动前失败时日志文件可能为旧内容，UI MUST 以 init_error/last_error 为本次失败的权威信息。日志文件权限 MUST 为 0600、日志目录权限 MUST 为 0700（日志可能含用户脚本打印的敏感信息）。

#### Scenario: 日志覆盖重写

- **WHEN** 同一任务两次执行 init
- **THEN** init.log 只含第二次执行的输出

#### Scenario: 无日志时读取返回空

- **WHEN** 任务从未执行过 init，读取 init 日志
- **THEN** 返回 200 空内容

#### Scenario: 陈旧 inherit 警告被清除

- **WHEN** 上次 inherit 产生警告，本次 retryCreate 重跑 inherit 无任何警告
- **THEN** 既有 inherit.log 被删除，init 日志 API 不再返回陈旧警告

#### Scenario: 任务删除后日志清理

- **WHEN** 任务删除流程成功完成
- **THEN** 该任务的日志目录被 best-effort 删除；清理失败不影响删除结果

### Requirement: Pre-delete 脚本

任务删除流程 SHALL 在 kill 残余会话与二次 dirty 门禁通过之后、`wt.Remove` 之前执行 pre_delete script（cwd 为 worktree，超时 2 分钟；脚本以 Manager 持有的独立 runner ctx 执行，不随 HTTP 请求取消，也不复用服务端 signal ctx——见优雅关停要求）。worktree 目录 os.Stat 仅 IsNotExist 时 MUST 跳过该步骤（幂等，与"资源不存在视为成功"一致）；其他 Stat 错误 MUST 落 deletion_failed（fail-closed）。脚本失败 MUST 使删除落 `deletion_failed` 并记录 last_error，且 last_error MUST 以固定前缀 `pre-delete:` 开头（UI 据此稳定识别并展示日志入口）（既有删除失败语义，可 Retry 重入）。`DeleteForce` MUST 跳过 pre_delete script。删除重试 MUST 重新执行 pre_delete script（脚本需幂等，UI 须有提示）。未配置 pre_delete script 时删除流程与现状一致。

#### Scenario: pre-delete 在 worktree 移除前执行

- **WHEN** 配置了 pre_delete script，删除任务
- **THEN** 脚本在 worktree 目录仍存在、任务进程已终止后执行，脚本成功后才移除 worktree

#### Scenario: pre-delete 失败可重试

- **WHEN** pre_delete script 非零退出
- **THEN** 任务落 deletion_failed，worktree 未移除；Retry 后脚本被重新执行

#### Scenario: worktree 已不存在时幂等跳过

- **WHEN** 删除流程在 worktree 移除后、DB 删除前失败，Retry 重入
- **THEN** pre-delete 步骤检测到 worktree 不存在而跳过，删除流程幂等收敛

#### Scenario: Force 删除跳过脚本

- **WHEN** pre_delete script 持续失败，用户以 Force 模式删除
- **THEN** 脚本不执行，删除流程继续

#### Scenario: 未配置时行为不变

- **WHEN** 项目未配置 pre_delete script，删除任务
- **THEN** 删除序列与既有行为完全一致

### Requirement: 脚本执行器优雅关停

init 与 pre-delete 脚本 SHALL 以 Manager 持有的独立 runner ctx 执行（不随 HTTP 请求取消，也不复用服务端 signal ctx——signal ctx 先于 Shutdown 取消会造成反向时序窗口；仅 Shutdown 在拒绝新执行后取消 runner ctx），并共用 runner 准入与等待组。新执行 MUST 先经 Shutdown 准入登记等待组（admission），再做状态 claim；claim 失败或 admission 后任何同步退出（任务不存在、门禁失败、存储错误）MUST 恰好一次释放登记；异步执行启动后登记所有权移交执行 goroutine。Shutdown 开始后 MUST 拒绝新执行：init 不得修改 init_status（保持 pending 由 Reconcile 收敛），Re-run MUST 返回错误且 init_status 不变，pre-delete MUST 停止删除序列且绝不执行 worktree 移除（本次删除操作返回错误，供下次 Retry）。等待组 MUST 覆盖完整执行尝试（配置读取、env 合并、日志准备、脚本执行、最终状态落账）；pre-delete 的登记 MUST 持有到删除序列成功提交或失败落账；runner ctx 被取消后，最终状态落账 SHALL 使用独立的短超时非取消 context；Shutdown 等待 MUST 在全部执行尝试收敛（取消并尽力落账）后才关闭存储。

#### Scenario: Shutdown 进行中拒绝 Re-run

- **WHEN** Shutdown 已开始，用户调用 Re-run init
- **THEN** 返回错误，init_status 不被修改，任务由重启后 Reconcile 收敛

#### Scenario: Shutdown 进行中 pre-delete 停止删除

- **WHEN** 删除流程执行到 pre-delete 步骤时 Shutdown 已开始（无法登记 runner）
- **THEN** 删除序列停止，worktree 未被移除，本次操作返回错误，下次 Retry 重新执行脚本

#### Scenario: 优雅关停等待在跑脚本收敛

- **WHEN** Shutdown 时有脚本正在执行
- **THEN** 脚本进程组被取消，执行尝试尽力完成状态落账后，存储才被关闭
