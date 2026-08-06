# Delta: project-lifecycle-config

## MODIFIED Requirements

### Requirement: Inherit 文件继承

`kind=repo` 项目的任务创建时（worktree 创建成功后、提交 suspended 前），系统 SHALL 以文件级粒度枚举主仓库中 gitignored 与 untracked 的文件（枚举 MUST 展开未跟踪目录与 ignored 目录至文件级记录，不得把目录内已跟踪文件卷入），按 inherit patterns 过滤后复制进新 worktree，保持相对路径与文件权限；符号链接 MUST 按链接复制；普通文件 MUST no-clobber 原子发布（同目录临时文件完整写入后以不占位方式发布——目标已存在或并发出现时 MUST 跳过且不覆盖，任何时刻不得留下部分内容的目标文件；目标路径任一祖先为符号链接时 MUST 拒绝）。`.git` 内容 MUST NOT 被复制。目标路径 MUST 通过 containment 校验（拒绝绝对路径与 `..` 逃逸）。retryCreate（creation_failed 重入）SHALL 无论 worktree 是复用还是重建都重新枚举并幂等执行 inherit，消除"worktree.Add 后、inherit 前崩溃"导致的永久漏拷窗口。inherit 的全部机制失败（枚举、glob 匹配、单文件复制）MUST 一律降级为警告（写入任务 inherit 日志），MUST NOT 阻断任务创建与激活；inherit 日志写入失败本身时警告可丢弃或写服务端日志，同样 MUST NOT 阻断；创建链唯一可因 inherit 阻断的前置是配置读取失败。

`kind=dir` 项目的任务 MUST NOT 执行 inherit（无 worktree、无"主仓库"语义来源）；dir 创建链仍 MUST 读取 lifecycle 配置（供 init_status 决策），配置读取失败仍为创建链唯一阻断点，语义与 repo 一致。

#### Scenario: gitignored 配置文件被继承

- **WHEN** repo 项目 inherit_patterns 含 `.env`，主仓库根目录存在 gitignored 的 `.env`，创建任务
- **THEN** 新 worktree 根目录存在内容一致的 `.env`，任务正常进入后续流程

#### Scenario: 已跟踪文件不被复制

- **WHEN** inherit_patterns 的 glob 同时匹配到一个已 git 跟踪的文件
- **THEN** 该文件不通过 inherit 复制（worktree 检出已含该文件），不计警告

#### Scenario: 单文件复制失败仅警告

- **WHEN** 匹配到的某文件因权限不可读
- **THEN** 警告写入任务 inherit 日志，任务创建继续，init 与激活不受影响

#### Scenario: retryCreate 幂等重跑继承

- **WHEN** repo 任务创建在 worktree.Add 后、inherit 执行前因崩溃或配置读取失败落 creation_failed，用户 Retry
- **THEN** retryCreate 重新枚举并执行 inherit，缺失文件被补齐，已存在文件自动跳过

#### Scenario: dir 项目任务跳过 inherit

- **WHEN** dir 项目配置了 inherit_patterns，创建任务
- **THEN** 系统不枚举、不复制任何文件（inherit_patterns 对 dir 无效），创建链继续；lifecycle 配置读取失败仍落 creation_failed

### Requirement: Init 脚本一次性执行

项目配置了 init script 时，系统 SHALL 在任务创建提交 suspended 后异步执行一次该脚本：cwd 为任务运行目录（`kind=repo` 任务为其 worktree，`kind=dir` 任务为项目路径本身），经 `/bin/sh -c` 非交互执行，超时 10 分钟；脚本以 Manager 持有的独立 runner ctx 执行（不随 HTTP 请求取消，也不复用服务端 signal ctx——见优雅关停要求）。init script MUST 幂等（SIGKILL/崩溃可能遗留仍在运行的旧脚本进程，Re-run 可能与之并行）。任务 SHALL 持久化 init_status（`none|pending|running|succeeded|failed`），且提交 suspended 与 init_status 置位 MUST 为单条原子更新（不存在 suspended 已提交而 init_status 未置位的窗口）。状态迁移 MUST 使用条件更新（CAS）：`pending→running` 要求任务仍为 suspended 且 init_status=pending；`failed|succeeded→running`（Re-run）要求任务仍为 suspended；`running→succeeded|failed` 要求 init_status=running。执行成功 MUST 置 `succeeded` 并触发自动激活；执行失败（非零退出、超时）MUST 置 `failed` 并记录 init_error，MUST NOT 触发激活。未配置 init script 的任务 `init_status` MUST 为 `none`，创建后直接自动激活（现状行为不变）。

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

#### Scenario: dir 任务 init 在项目目录执行

- **WHEN** dir 项目配置了 init script，创建任务
- **THEN** init script 以项目路径为 cwd 执行，init 状态机与自动激活语义与 repo 完全一致

### Requirement: Pre-delete 脚本

任务删除流程 SHALL 在 kill 残余会话与二次 dirty 门禁通过之后、`wt.Remove` 之前执行 pre_delete script（cwd 为 worktree，超时 2 分钟；脚本以 Manager 持有的独立 runner ctx 执行，不随 HTTP 请求取消，也不复用服务端 signal ctx——见优雅关停要求）。worktree 目录 os.Stat 仅 IsNotExist 时 MUST 跳过该步骤（幂等，与"资源不存在视为成功"一致）；其他 Stat 错误 MUST 落 deletion_failed（fail-closed）。脚本失败 MUST 使删除落 `deletion_failed` 并记录 last_error，且 last_error MUST 以固定前缀 `pre-delete:` 开头（UI 据此稳定识别并展示日志入口）（既有删除失败语义，可 Retry 重入）。`DeleteForce` MUST 跳过 pre_delete script。删除重试 MUST 重新执行 pre_delete script（脚本需幂等，UI 须有提示）。未配置 pre_delete script 时删除流程与现状一致。

`kind=dir` 项目的任务删除（normal 模式）SHALL 同样执行 pre_delete script，cwd 为项目目录本身；该脚本是用户显式授权的操作，是 dir 删除"ocdeck 内建逻辑对用户目录零写删"不变量的唯一例外。dir force 模式与 repo 一致 MUST 跳过 pre_delete script。

#### Scenario: pre-delete 在 worktree 移除前执行（repo 项目）

- **WHEN** repo 项目配置了 pre_delete script，删除任务
- **THEN** 脚本在 worktree 目录仍存在、任务进程已终止后执行，脚本成功后才移除 worktree

#### Scenario: pre-delete 失败可重试（repo 项目）

- **WHEN** pre_delete script 非零退出
- **THEN** 任务落 deletion_failed，worktree 未移除；Retry 后脚本被重新执行

#### Scenario: worktree 已不存在时幂等跳过（repo 项目）

- **WHEN** 删除流程在 worktree 移除后、DB 删除前失败，Retry 重入
- **THEN** pre-delete 步骤检测到 worktree 不存在而跳过，删除流程幂等收敛

#### Scenario: Force 删除跳过脚本

- **WHEN** pre_delete script 持续失败，用户以 Force 模式删除
- **THEN** 脚本不执行，删除流程继续

#### Scenario: 未配置时行为不变

- **WHEN** 项目未配置 pre_delete script，删除任务
- **THEN** 删除序列与既有行为完全一致

#### Scenario: dir 任务的 pre-delete 脚本

- **WHEN** dir 项目配置了 pre_delete script，以 normal 模式删除任务
- **THEN** 脚本以项目目录为 cwd 执行（用户授权副作用）；脚本失败落 deletion_failed 且 last_error 以 `pre-delete:` 前缀开头，可重试或强制删除
