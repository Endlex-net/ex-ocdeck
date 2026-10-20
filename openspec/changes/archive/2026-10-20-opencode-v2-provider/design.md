# Design: opencode-v2-provider

## Context

现状与约束（动机见 proposal.md）：

- **SPI 契约已存在且稳定**：`internal/application/agentprovider/agentprovider.go` 的 Provider 聚合接口（10 方法）+ 8 窄接口；v1 由 `internal/infrastructure/opencode/`（`spi_provider.go` / `spi_supervisor.go` / client 族）兑现，registry 单点装配在 `cmd/ocdeck-server/main.go:233-248`。
- **wire 差异已全部实证**：spike 结论（`openspec/changes/opencode-v2-contract-spike/design.md`「Spike 结论」节差异清单 #1–#13）是本设计的唯一契约事实起点；候选/待验证项按其标注处理，不得当作已验证事实。
- **数据割接不自研**：官方升级路径已实证保留 v1 数据（session ID 不变、消息原文一致，`cutover-check-20261014T043705Z`）。
- **钉扎语义不可改**：`tasks.provider_id` 创建时钉死（既有 spec）；存量任务 = v1。
- **必须 `serve` 子命令启动服务（serve 形态不接受 `--session` flag）；裸 `opencode` 进 TUI；TUI root 命令有 `--session`/`--server`（attach 形态用，D8-7 已实证）**；`/global/health` 是 SPA HTML 不能判就绪；存储为 SQLite（一律走 API，不直读文件布局）。

## Goals / Non-Goals

**Goals:**

- v2 provider 完整兑现 SPI（10 方法聚合 + 8 窄接口），对外行为与 v1 等价（消费者无感）
- 双 provider 并存：registry 同时注册 v1/v2；分派按钉扎 providerID + 机器实装版本规则
- v2 机器承接 v1 钉扎任务（执行 provider = v2，钉扎值不变）；v2 钉扎任务在 v1 机器 fail-closed
- v2 runtime 采用 A+ 模型：全局单 `opencode serve` 进程承载多 session（`location.directory` 绑定 worktree）
- v2 契约锚点集独立治理（钉 2.0.28 / 源码锚点 commit `026b106b`），v1 锚点不动
- spike 阻断项转化为本 change 的补证任务（见 Migration Plan）

**Non-Goals:**

- 不改 v1 provider 实现与 v1 契约锚点集
- 不做自研数据迁移/搬运（官方升级路径已实证）
- 不接共享 credential store、不做 OS 级沙箱（沿用 A+ 文档「明确不做」）
- v2 钉扎任务在 v1 机器的激活只 fail-closed，不提供降级执行
- status 等价能力（`Session.Info.outcome`+SSE）的运行时实证通过前，不依赖其承载任何用户可见状态语义
- terminal.01 的 CLI flag 形态不复刻；attach 行为 = REST 预验证 + 自动选中绑定 session（D3 TerminalIO 契约，用户已定自动定位），自动选中机制已实证冻结：`opencode --server <url> --session <id>`（D8-7）

## Decisions

### D1：v2 实现为独立包 `internal/infrastructure/opencode2`，与 v1 包并存

v2 包内部结构镜像 v1（client/sse/events/attention/prompt/session/supervisor/provider 适配器），wire 差异全部封闭在包内：base path 拼 `/api` 前缀、响应统一解 `{data}` envelope、事件载荷从 `data.*` 取值、事件名映射 `session.renamed`→SPI 的 session-updated 语义、permission 双端点聚合为 SPI 的 AttentionIO 单一语义。

- **为什么不改 v1 包加分支**：wire 差异是全面性的（13 项），分支会让 v1 包变成两个协议缠绕的难测体；双包并存与「v1 长期保留」的锁定方向一致，v1 包保持冻结可回归。
- **为什么不先抽象共享 toolkit**：`docs/design/multi-agent-spi.md` 已标 YAGNI——等 v1/v2 出现真实重复再抽；transport（loopbackTransport、Basic auth 注入、SSE 帧解析）允许小包级复用，但禁止为本期预抽象 SPI 之上的中间层。
- 备选：单包双 client——被拒绝：provider 身份常量、binding data 形状、supervisor 进程模型全部不同，单包内并列两套等于名义分包。

### D2：v2 身份常量与 binding data

- `ProviderID = "opencode-v2"`（`docs/design/multi-agent-spi.md` 稳定值表已预留）；`ProviderInstanceID = "default"`（单 server 模型下实例即本机 server）。
- v2 私有 binding data（`v2BindingData`）：`{port, directory, taskId, origin, auth{kind}}`——沿用 v1 的连接字段，新增 session 来源字段 `origin`；使用独立类型；密码经 D3 派生凭据原语（`credentials.go`）确定性派生、不落库，**派生标识 = 稳定的 v2 runtime 实例标识**：连接同一全局 server 的全部任务共用同一凭据（单 server 只有一套 Basic auth 密码），任务身份与目录归属仍按 binding 独立校验。upstream session id 来自 `POST /api/session` 响应的 `data.id`（`^ses` pattern）；承接路径沿用官方升级保留的原 session id（见 D5）。
- binding envelope（`TaskBinding` v1 格式）不变：ProviderID 字段承载 `opencode-v2`，Data 存 v2BindingData JSON。envelope 版本不升——格式兼容，仅 Data 载荷按 provider 私有解码。
- v2BindingData 完整契约（fail-closed 编解码；校验失败 MUST 先于凭据供给/网络/进程任何副作用）：
  ```json
  { "port": 4096, "directory": "/abs/worktree", "taskId": "task_xxx", "origin": "created", "auth": { "kind": "derived" } }
  ```
  全部字段 required；`port` int（1–65535）；`directory` 绝对路径且 MUST 等于任务 worktree 解析结果；`taskId` MUST 等于归属任务 ID；`origin` 枚举 `created`（ocdeck 首建）/ `adopted`（承接 A 延续的原 session）/ `ephemeral`（AcquireEphemeral 仅内存临时 binding），未知值拒绝——是恢复配置策略的可靠识别事实（见 D3 任务配置应用契约，MUST NOT 仅凭 binding.ProviderID 判断）；`auth.kind` 唯一合法值 `"derived"`，legacy/未知 kind 拒绝（`ErrKindBindingInvalid`）。凭据派生公式：`DeriveRuntimePassword(masterSecret, "opencode-v2", ProviderInstanceID)`（`internal/application/agentprovider/credentials.go:51`；第三参传 runtime 实例标识 `"default"`——同一 server 全部任务共用同一凭据）。envelope 层 `TerminalRef` 必填（生命周期 binding；`internal/domain/agentprovider/agentprovider.go:86` Validate）：v2 取值 = 交互 TUI 托管单元标识（见 D3 TerminalIO 契约；从任务身份确定性派生 `ocdeck-v2-tui-<taskID>`，TUI 延迟创建不影响引用稳定；serve 管理句柄由 supervisor 进程内持有，MUST NOT 入 binding）。校验顺序（生命周期 binding；仅内存临时 binding 例外见 D3 AcquireEphemeral 行）：envelope Validate → v2BindingData 解码校验 → 凭据派生 → 网络/进程副作用。
- **校验分层（结构 vs 归属，两阶段入口；生命周期 binding 与仅内存临时 binding 分别按 D2 各自结构校验，MUST NOT 用 domain `TaskBinding.Validate` 校验临时 binding）**：阶段一（不派生凭据）：结构校验（envelope Validate + v2BindingData 形状/值域/auth.kind，provider 私有解码）+ 归属校验（task 层将独立任务事实——任务行 ID 与 worktree 解析结果——传入新增可选窄接口 `BindingOwnershipChecker.CheckBindingOwnership(b, expectedTaskID, expectedDirectory)`，provider 解私有字段并逐字段比对，不符 → `ErrKindBindingInvalid` 拒绝）。阶段二（归属通过后才允许）：凭据派生与 `BindingRuntimeEndpoint` 解析、网络/进程副作用（`BindingRuntimeEndpoint` 返回值同时含凭据，`agentprovider.go:189`——因此它 MUST NOT 用于阶段一的归属比对）。task 层 MUST 先调 `CheckBindingOwnership`（provider 未实现该接口 → 拒绝使用 v2 binding，fail-closed），MUST NOT 以「binding 从该行读出」代替字段值比对。同步修订 D9：application 层新增此可选窄接口（其余 SPI 不变）。

### D3：runtime 托管 = A+ 全局单 server

v2 的 RuntimeSupervisor 语义映射：

| SPI 方法 | v1 语义 | v2 A+ 语义 |
|---|---|---|
| `Acquire` | 每任务 spawn 独立进程 | **仅限经判定确认无历史 session 的首次路径**（v2 钉扎新任务 / D5-B 承接首建）：确保全局 server 在跑（幂等；不在则 spawn `opencode serve --port <p> --hostname 127.0.0.1`）→ `POST /api/session`（body 含 `location.directory=<worktree>`）建任务 session → 返回 binding。**有历史 session 的任务 MUST 走 `Resume` 入口**（task 层按 D5 判定矩阵选择入口；历史信息经 Resume 的 `old` binding 参数到达 provider——v1 binding 的 UpstreamSessionID 即原 session id） |
| `Resume` | 重连/重建任务进程 | server 存活检查（`/api/session` 401/200 就绪判定）→ 按 binding 的 upstream session id `GET /api/session/{id}` 验证可读 → 存在则仅恢复该 session（重连事件/终端）；**session 缺失或不可读时 MUST 停止并提交用户决策——ResumePolicy 任何取值 MUST NOT 授权创建替代 session**（POST 创建仅允许于 D5-B 首次路径或显式 SessionControl.Create） |
| `Release` | 杀任务进程 | 释放任务的运行关联（退订事件、回收终端/连接资源）；**保留上游 session，MUST NOT `DELETE`**；server 进程不随任务释放（全局共享，Effect.never 语义）。挂起路径还须先停止该任务在 server 上的执行：停止 wire 已实证冻结 = `POST /api/session/{id}/interrupt`（200 `{interrupted: bool}`；`/abort` 不存在）；停止确认成功后才退订并提交 suspended，停止失败/超时如实报错且 MUST NOT 提交 suspended；停止操作 MUST 只作用于目标任务 session，不影响同 server 其他 session |
| `AcquireEphemeral` | 一次性临时 runtime（SPI 既有语义：不创建 session） | 确保 server 在跑 + 获得临时连接；MUST NOT 创建 session；server 不动。返回**仅内存临时 binding**（契约见 D2）：envelope `UpstreamSessionID` 允许为空（不伪造 session id）、`TerminalRef` 取固定保留值 `"none"`、Data=`v2BindingData{port, directory, taskId, origin:"ephemeral", auth{kind:"derived"}}`；校验经独立结构校验函数（MUST NOT 调用 domain `TaskBinding.Validate`）；taskId/directory 归属比对 MUST 先于凭据派生；该形态 MUST NOT Encode/持久化；对应 Release 仅回收临时连接，MUST NOT 调用 session abort/delete |

- 全局 server 句柄由 supervisor 持有（进程级单例），端口固定分配策略复用 v1 的 AllocatePort（但只分一次）；server 异常退出由 supervisor 负责重拉，重拉后全部活跃任务按 binding 重建 session 关联。
- **runtime 实例记录（跨进程恢复来源与记录生命周期）**：store 新增 `provider_runtime` 记录（provider_id/instance_id/port/pid，**pid 允许 NULL**）。确定性进程发现来源 = serve 托管单元标识（tmux 托管单元名 `ocdeck-v2-serve`，与 supervisor 进程级单例同一单元）——pid 未落盘时经该单元重新发现。

  启动顺序（唯一合法写序）：①分配端口并写入记录（pid=NULL，「启动中」）→ ②经托管单元启动 serve → ③获取实际 serve pid → ④验证（pid 存在 + cmdline 匹配预期启动参数 + 派生凭据 `GET /api/session` 200 且 envelope 可解析）→ ⑤更新记录写入 pid（「可用」）→ ⑥此后才允许任务业务副作用（创建 session/提交 binding）。任一失败 MUST NOT 创建 session、发送 prompt 或提交 binding；记录处置见下表。

  重启恢复矩阵：

  | 实例记录状态 | 托管单元/端口实际状态 | 处置 |
  |---|---|---|
  | 无记录 | — | 首次分配：写记录（pid=NULL）→ 走启动顺序 |
  | 记录存在、pid 有效 | cmdline 匹配 + 凭据验证 200 且 envelope 可解析 | 复用（不重启） |
  | 记录存在但 pid NULL/旧（崩溃窗口） | 经托管单元重新发现存活 serve 进程 → 取实际 pid → 重做步骤④验证 | 验证通过 → 更新记录复用；验证失败（身份/认证/协议不符）→ fail-closed，MUST NOT 仅凭端口响应接管 |
  | 记录存在；pid NULL 且托管单元未发现存活 serve，或已记录 pid 的进程不存在 | 确认无存活托管 serve 且端口未被占用 | 使用记录中的同一端口从步骤②继续，执行③④⑤，成功后才允许⑥；端口被占用 → fail-closed 报错 |
  | 启动失败（步骤②③） | — | 保留记录（pid=NULL 启动中态）+ 记 last_error；按 supervisor 独立有界重试，耗尽 → fail-closed |
  | 启动成功但 pid 更新失败（步骤⑤） | server 存活 | 本轮 MUST NOT 允许任务业务副作用；下次恢复走「pid NULL/旧」分支重新发现并验证 |

  `401/200` 仅为启动就绪信号，MUST NOT 单独作为实例归属证明。全部 v2 binding 的 port MUST 恒等于实例记录端口，不一致 → fail-closed 告警并提交用户决策。
- 会话删除仅允许于两类入口：①既有任务删除流程（经 SessionControl `DELETE /api/session/{id}`）；②首建失败补偿的受限例外——仅本次 attempt 新建且尚未成功提交的上游 session 允许经 SessionControl.Delete 回收（attempt 所有权：以本次 attempt 返回的 candidate session id 为准，其 claim/anchor 写入与 binding 提交同一原子事务，回滚范围仅限该 candidate）。DELETE 失败 MUST 记录清理债务（cleanup notice/last_error），且 MUST NOT 把残留事实误判为「从未建立会话」。此外一切路径（Release/挂起/恢复/承接）MUST NOT 删除 session。
- **TerminalIO 契约（v2；用户已定：自动定位任务会话）**：`EnsureTerminal` = 经 REST `GET /api/session/{id}` 验证 binding 的 upstream session 存在且归属本任务（前置条件，不代替选中）+ 幂等建立/获取交互 TUI 托管单元并返回其标识（`ocdeck-v2-tui-<taskID>`）；session 不存在 → `ErrKindNotFound`、归属不符 → `ErrKindBindingInvalid`，MUST NOT 创建替代 session。`AttachCommand` = 产出连接共享 server 的交互 TUI argv 并**自动选中 binding 的原 session**（认证经派生凭据经 `OPENCODE_SERVER_PASSWORD` env 传递；自动选中机制已实证冻结（D8-7）：`opencode --server <url> --session <id>`，session 存在则 TUI 启动即选中；该机制失效时返回明确失败并提交用户决策，MUST NOT 退化为手动定位）。TerminalRef = 交互 TUI 托管单元标识（独立于后台 serve 管理句柄；task 层 `attach_shell.go:66-83` 的 reopen/AttachPty 接线按 v2 分支改为 TUI 托管单元，经 D4 替换表分派）。终端回收随 Release 运行关联一并退订。
- **任务配置应用契约（环境/权限）**：任务环境（env）与权限模式的取值来源与 v1 相同——application/task 层经 seam 提供事实（v1 对应 `SupervisorSideEffects.ResolvePermissionMode`/`BuildServeEnv`，`spi_supervisor.go:58-68`；v2 侧设等价 seam 由 task 层注入）：首次路径取既有任务环境解析与权限模式规则的当前值；恢复路径 MUST 复用既有同代 env 快照（`recovery.go:739-740/766-769` 同代语义，MUST NOT 重新分层）。共享 serve 的进程环境只含 runtime 级事实（端口、派生凭据、实例标识），MUST NOT 将首个任务的环境/权限固化为全局 server 配置。session 级 environment/permissions 的应用位置（create 请求体 vs 后续 session API）随 session.06 与 attention 补证冻结（D8-5/D8-3）；冻结前 v2 路径不注入任务级 environment/permissions 并如实透出该能力缺口，冻结后应用完成确认 MUST 先于任务就绪提交与首个 prompt。承接 A 延续原 session 时 MUST NOT 覆盖该 session 的既有环境/权限配置。配置应用失败：保留旧 binding/归属/anchor，MUST NOT 提交就绪或发送 prompt；任务 env 快照持久化沿用既有机制，binding/状态事务不含配置内容。既有 session 的恢复配置规则（与承接 A 保护规则的优先级衔接）：以 binding `origin` 为识别事实——`origin=adopted`（承接 A 延续的原 session）的配置保护在后续 R1 恢复中持续有效，MUST NOT 通过恢复覆盖该 session 的环境/权限；`origin=created`（B 首建与 v2 钉扎新任务，session 由 ocdeck 创建）后续恢复按同代 env 快照与权限规则正常应用。
- **备选：每任务一进程（v1 模型）**——被拒绝：v2 serve 多 session 是官方形态，每任务一进程在 v2 下浪费内存且与 A+ 文档方向冲突。
- tmux 托管：serve 进程一个托管单元；TUI attach 形态沿用 v1 机制（第二实例同 directory 共享可见已实证，spike terminal.01）。

### D4：分派与承接规则（task 层改动面）

现状分派仅按 `tasks.provider_id`（`binding_dispatch.go:56-62` registryAdmitTask）。扩展为单一 resolver：

**resolver 决策表**（激活/恢复准入与消费点路由统一；自上而下首个命中生效；`unknown` = 版本探测失败）：

| # | binding 执行身份 | 钉扎 | 机器版本 | 开关 | 入口 | 结果 |
|---|---|---|---|---|---|---|
| R1 | v2 | * | ≥2 | 开 | 激活/恢复 | v2 恢复（延续原 session，D5-A） |
| R2 | v2 | * | 1 或 unknown | * | 激活/恢复 | fail-closed（`ErrKindCapabilityUnavailable`），零副作用 |
| R3 | v2 | * | * | 关 | 激活/恢复 | fail-closed（`ErrKindProviderNotFound`：v2 未注册），MUST NOT 转 v1 |
| R4 | v2 | * | * | * | 消费点 | 按 binding 路由 v2；未注册 → `ErrKindProviderNotFound` |
| R5 | v1 或无 | v2 | ≥2 | 开 | 激活/恢复 | v2 执行 |
| R6 | v1 或无 | v2 | 1 或 unknown | * | 激活/恢复 | fail-closed（`ErrKindCapabilityUnavailable`） |
| R7 | v1 或无 | v2 | * | 关 | 激活/恢复 | fail-closed（`ErrKindProviderNotFound`） |
| R8 | v1 或无 | v1 | ≥2 | 开 | 激活/恢复 | 承接（D5：A 延续 / B 首建） |
| R9 | v1 或无 | v1 | ≥2 | 关 | 激活/恢复 | fail-closed（`ErrKindCapabilityUnavailable` + 用户提示开开关；MUST NOT 静默按 v1 执行——v1 provider 无法驱动 v2 二进制） |
| R10 | v1 或无 | v1 | 1 | * | 激活/恢复 | 纯 v1（现状不变） |
| R11 | v1 或无 | v1 | unknown | * | 激活/恢复 | 纯 v1（保守不承接） |
| R12 | v1 | * | * | * | 消费点 | 按 binding 路由 v1 |

- 优先级：已有 v2 binding（R1–R4）先于钉扎判定；消费点恒按 binding 路由（R4/R12）。fail-closed 允许的诊断副作用仅为 last_error 记录与用户可感知提示。
- 开关关闭形态固定为「v2 provider 不注册」（不存在「注册但不承接」中间态）。
- 任务创建：机器实装主版本 ≥2 且开关开时允许创建 v2 钉扎任务；默认仍为 v1 钉扎（存量行为不变）。
- 机器实装版本探测：复用 `probeOpenCodeVersion`（`config.go:464`）扩展主版本提取；探测失败按 unknown（R2/R6/R11 保守口径）。
**编排接线与替换表**（task 层从固定 v1 supervisor 改为按任务分派；v1 分支行为不变）：

| 落点 | 现状 | 改动 |
|---|---|---|
| `main.go:252/256` `SetEphemeralSupervisor`/`SetRuntimeSupervisor` | 固定注入 v1 supervisor 单例 | Manager 增加「按任务解析 supervisor」接缝：activateRun/runRecoveryAttempt/删除临时绑定分支经 registry 按任务执行身份取 `provider.Supervisor()`（v1 任务解析到同一 v1 实例，行为等价） |
| `activate.go` activateRun/commitRuntimeReady | 每任务 spawn + tmux watcher + 进程探活提交 | v2 分支：Acquire=幂等确保全局 server + 建/续 session；不建 tmux watcher（终端经 D3 TerminalIO）；commitRuntimeReady 等价提交条件 = server 就绪（`/api/session` 401/200）+ session 创建/验证成功 + SSE 首帧握手 + binding/归属/anchor/状态 CAS 原子提交成功，四者缺一无提交 |
| 启动/恢复 reconcile（recovery.go） | 按每任务 runtime 探活重拉；`BeforeLaunch` 闭包含任务 permit 扣款（`recovery.go:747/790/817-823`） | v2 分支：全局 server 探活重拉的唯一 owner 是 supervisor；并发恢复请求经 supervisor 内单例闸门合并（in-flight 去重）：leader 执行重拉，等待者等待共享结果——MUST NOT 重复启动、MUST NOT 消耗各任务恢复预算；重拉结果向全部等待者分发。**v2 的 `BeforeLaunch` 仅执行取消/继续条件检查（不含扣款）**——任务 permit 在任务级动作（session 验证/关联重建 attempt）的确定边界独立消耗，MUST NOT 复用 v1 含扣款的 beforeCreate 闭包；SPI 注释（`agentprovider.go:71-73`）的 permit 语义对 v1=进程启动前闸门、对 v2=任务级关联 attempt 前闸门（实现落点注明）；supervisor 共享启动/重拉使用独立有界重试策略（次数与退避实现期固定并写入实现说明；耗尽 → 向 leader 与全部等待者分发同一失败结果），不扣任何任务 permit；任务 permit 恰在 server 可用后、每次 session 验证/关联重建 attempt 前消耗一次；实现时同步修订 `BeforeLaunch` 现有注释，避免其仍表达仅进程启动前闸门的语义 |
| 挂起（suspend.go 的 Release 调用） | minimal binding 调 Release | v2 分支：Release=停止执行（D8-6 冻结后）+ 退订 + 回收，MUST NOT 杀共享 server |
| 删除（delete.go 固定 v1 分派/解码 v1 Data） | 固定 v1 | 按 binding ProviderID 经 registry 分派；Data 解码下沉 provider（`BindingRuntimeEndpoint` 既有端口） |
| `oc_spi_bridge.go` SessionKey 构造（`bridgeSessionKey`，L37-44） | 固定 v1 命名空间（store.ProviderIDV1） | 按任务实际 binding 命名空间构造 SessionKey（v2 任务 → `(opencode-v2, default, upstreamID)`） |
| SSE 首次对齐与事件消费（activate.go startSSE/alignSessions/handleAgentEvent） | 事件消费即执行持久化 claim | 承接候选阶段（D5 步骤 4）仅握手与缓冲；原子提交成功后才允许对齐写入与事件消费，失败 MUST 关闭候选流 |
| 单任务取消/回滚 | 终止任务进程 | v2 分支 MUST NOT 终止共享 server |
| shutdown | 按 `ShutdownPolicy` 处理任务进程（`config.go:32-34`；`kill_on_start` 与 `kill_immediate` 均在关停时执行 kill 清理，`manager.go:1043/1054`） | v2 分支按策略分派：`persist` → 保留全局 server，仅停本进程事件订阅与监视（下次启动复用）；`kill_on_start` / `kill_immediate` → supervisor 单 owner 关闭共享 server 一次 + 逐任务退订（失败按既有 cleanup notice 语义报告）；`kill_on_start` 的下次启动清理规则保留 |

**生命周期状态矩阵**（v2 分支专用；v1 分支保持 suspend.go/activate.go 现状不变；D5 与 v2 runtime spec 按本矩阵引用）：

| 入口 | 前置状态 | 中间态 | 成功提交 | 失败收敛 |
|---|---|---|---|---|
| 承接 A（suspended 手动激活） | suspended | MUST NOT 写 activating（准备阶段长，activating 会触发不适用承接的激活补偿） | 单事务：binding + 归属转换 + anchor + suspended→active CAS（D5 步骤 5） | 零状态副作用；按 D5 失败出口矩阵 |
| 承接 A（active 存量任务，经运行期/启动恢复触发） | active | MUST NOT 写 activating | 单事务：binding + 归属转换 + anchor + active→active 守卫 CAS（状态不变，仅防并发漂移） | 零状态副作用；按 D5 失败出口矩阵 |
| 承接 B 首建 / v2 钉扎新任务激活 | suspended | suspended→activating（复用既有入口） | 既有 commitActivationBinding：activating→active CAS + binding | 既有激活补偿（清理本地 runtime/连接与 env 快照 + 回收本次 attempt 新建且未提交的上游 session（受限回收例外，D3）→ suspended + last_error）；无历史事实可伤 |
| 已有 v2 binding 激活（suspended 来源） | suspended | suspended→activating（复用既有入口） | activating→active CAS（无归属转换，承接已完成） | 既有激活语义 |
| 已有 v2 binding 运行期/启动恢复（active 来源，recovery.go:373-376 / reconcile.go resumeActive 路径） | active | active→activating（既有恢复入口） | activating→active CAS | 既有恢复语义（重试预算/permit 按任务级动作消耗） |
| v2 挂起 | active | active→suspending CAS 前 MUST 完成挂起门禁（停止 wire 已冻结）与 binding 归属校验；停止确认前 MUST 保留运行关联 | suspending→suspended | 停止失败/超时 → 收敛回 active、保留运行关联、记 last_error，MUST NOT 进入 v1 的 forceKillAll+finishSuspend 分支（suspend.go:92-95，对共享 server 禁用）；停止已成功但清理/最终 CAS 失败 → 收敛 suspended + last_error（执行已停止属实，不得报告完整 active），清理遗留按 cleanup notice 后台重试；MUST NOT 停留 suspending |

- server 端口固定不轮换：首次分配后端口即 runtime 实例的持久事实；重拉 MUST 使用同一端口，被占用 → fail-closed 报错（用户处理占用），MUST NOT 换端口重拉。因此 binding.port 恒有效，`BindingRuntimeEndpoint` 纯本地解码成立，消费点无需运行期端口发现。

### D5：承接的 binding 迁移语义

v1 钉扎任务被 v2 承接时区分两种情形：

**A. 已有历史上游 session 的任务**（既有 v1 binding，或官方升级保留的 session id；前提：机器已执行官方升级路径，原 session 数据已被官方升级保留——spike cutover 证据已实证 ID 与消息保留）：

- 旧 binding（若有，ProviderID=opencode-v1）按既有释放语义处理；新 binding 由 v2 provider 建立（ProviderID=opencode-v2），**upstream session id 沿用官方升级保留的原 session id**：v2 provider 经 `GET /api/session/{id}` 验证原 session 可读且可继续使用后延续它，仅重建连接、终端与 provider binding，MUST NOT 新建替代 session。
- 首次承接、重复激活、server 重启后恢复、承接中途失败重试，全部路径 MUST 保持同一原 session 的连续性（spike D6 #5 的四种承接路径）。
- 原 session 不可读或不可继续使用（官方升级未执行、数据损坏、id 不存在）时 MUST 停止承接、保留任务现状并提交用户决策，MUST NOT 自动新建替代会话。

**B. 从未建立上游会话的新任务**（v2 机器上新建、默认 v1 钉扎，无任何历史 binding/session）：

- 由 v2 provider 正常首次创建 session（`POST /api/session`），任务钉扎值保持 `opencode-v1` 不变；这是首次创建路径，MUST NOT 用于替代丢失或不可用的历史 session（A 情形的失败处理）。

两情形共同兑现 spike D6 #1（钉扎不变）/ #3（v1 私有 Data 由 v2 provider 重建——Data 重建但历史 session id 延续）/ #5（四种承接路径连续性）。

**历史会话判定矩阵**（承接准入只读阶段；区分「历史归属集合」与「锚定原 session」——前者可含多行含子 session，后者唯一、为承接延续目标）：

- 锚定原 session 识别规则：`anchor_*` 三元组的 session id 为承接目标；v1 binding 的 UpstreamSessionID MUST 等于 anchor（不等 → 来源不一致）；历史归属集合（task_sessions 行，含 parent_id 子 session）不要求等于 anchor，仅作「曾建立」证据。

| 来源组合 | 判定 |
|---|---|
| binding、anchor、归属行三者一致（anchor ∈ 归属集合、binding.UpstreamSessionID == anchor） | A 分支，目标 = anchor session id |
| 三者一致为空（无 binding、无 anchor、无归属行） | B 分支 |
| 已有 v2 binding（承接已完成过） | 非承接准入——走 resolver R1 恢复路径 |
| 其余（部分缺失、id 不一致、归属行命名空间混杂、解码损坏） | 停止承接、保留现状、提交用户决策，MUST NOT 归入 B |

**承接结构化主流程**（A 分支；标注每步副作用许可）：

1. 只读准入（零副作用）：resolver 决策表 + 判定矩阵 + 版本检查。
2. 幂等确保全局 server 在跑（同 Acquire 确保语义）。server 可用性失败的唯一出口：由 supervisor 独立有界重试策略承担（不消耗任务恢复预算/任务 permit），耗尽 → 停止本次承接、记 last_error、任务状态不变、提交用户决策，MUST NOT 作为普通任务 attempt 错误进入既有恢复循环；task 层经 supervisor 返回的阶段标识区分本步失败（server 可用性）与步骤 4 失败（关联握手，后者按既有恢复重试并消耗任务预算）。
3. 获取/验证原 session：目标 id 按判定矩阵取自 anchor；`GET /api/session/{id}` 返回 200 且可解析即「可继续使用」（验证谓词仅此，MUST NOT 发送测试 prompt）；失败 → 停止承接、提交用户决策（出口见下）。
4. 建立候选关联（内存态）：派生凭据连接 + 事件订阅首帧握手；握手失败 → 候选关联全内存态丢弃，零持久化副作用，任务状态不变，记 last_error，允许按既有恢复重试。
5. 原子提交（单事务）：新 v2 binding 写入 + task_sessions 命名空间转换 + anchor 三元组更新 + 任务状态 CAS 同事务——suspended 手动激活入口 CAS suspended→active；active 存量任务承接 CAS active→active（守卫谓词，状态不变）；均不经 activating 中间态（见 D4 生命周期状态矩阵）；CAS 失配/事务失败 → 整体回滚零写入，步骤 4 候选关联（订阅/连接）MUST 退订回收，任务保持原状态与全部旧事实，按冲突重试语义处理。
6. 退役旧关联：提交成功后释放旧 v1 运行关联（best-effort，失败仅 cleanup notice）。

承接失败出口（逐阶段矩阵）：

| 步骤 | 失败类型 | 处置 |
|---|---|---|
| 1 只读准入 | 历史事实不一致 / 版本不满足 / resolver 拒绝 | 停止承接、保留现状、记 last_error、提交用户决策 |
| 2 server 可用性 | 暂时性 runtime 故障（启动失败/未就绪） | supervisor 独立有界重试策略承担（不消耗任务恢复预算/任务 permit）；耗尽 → 停止、记 last_error、提交用户决策 |
| 3 原 session 验证 | 不可读/不存在/不可继续使用 | 停止承接、保留现状、记 last_error、提交用户决策 |
| 4 候选关联 | 握手失败 | 全内存态丢弃、零持久化副作用、记 last_error、按既有恢复重试 |
| 5 原子提交 | CAS 失配/事务失败 | 整体回滚零写入、候选关联退订回收、任务保持原状态、按冲突重试语义 |

任何失败路径 MUST NOT 进入普通激活失败补偿（不得清理 env 快照/历史事实），MUST NOT 自动转入 B 首建；last_error 为允许的诊断写入。

配置应用：承接 A MUST NOT 覆盖原 session 的既有环境/权限配置（D3 任务配置应用契约）；B 分支与 v2 钉扎新任务按首次路径应用，应用完成确认 MUST 先于就绪提交与首个 prompt。

**归属映射与原子提交**（ocdeck 元数据映射，非搬运上游会话）：

| 项 | 旧值 | 新值 |
|---|---|---|
| task_sessions 行命名空间 | `(opencode-v1, default, <ses_id>)` | `(opencode-v2, default, <ses_id>)` |
| anchor 三元组 | 同上 | 同上 |
| binding envelope | ProviderID=`opencode-v1` + v1 Data | ProviderID=`opencode-v2` + v2BindingData（UpstreamSessionID 不变） |
| `tasks.provider_id` | `opencode-v1` | **不变** |

规则：转换范围 = 该任务全部 task_sessions 行（含 parent_id 链的历史子 session）；写入前按新键三列谓词冲突检查（被他任务持有 → 拒绝承接）；store 新增命名空间转换方法——现有 upsert 的 ON CONFLICT 不更新命名空间列（`session_key_queries.go:71-78`），MUST NOT 复用其假定；转换与 binding/anchor/状态 CAS 同一事务。

- **备选：承接时新建 upstream session、不延续原会话**——被拒绝：切断原会话执行上下文；官方升级已实证保留原 session ID 与消息，延续原会话是用户已确认的方向。

### D6：v2 契约锚点集与检查脚本

- 新增 `scripts/check-opencode-v2-contract.sh` + `internal/infrastructure/opencode2/CONTRACT.md`：锚点钉 v2.0.28 源码锚点 commit `026b106b`（注意：与二进制构建 commit `044d8b7d` 分别记录，spike D1）。
- 锚点选自 spike 差异清单的实证项：`/api` 前缀、envelope、prompt 端点与 body 形状、`session.renamed`、`data.*` 归属、permission 双端点、`/openapi.json`、`/api/session` 就绪判定、serve argv 形态。
- v1 锚点脚本与 CONTRACT.md 不动。

### D7：wire→SPI 逐方法映射与事件归一化

**窄接口逐方法映射**（均以 `/api` 前缀 + `{data}` envelope 为前提；标注【待实证】的项列入 D8 前置冻结，冻结前对应能力保持保守语义）：

| SPI 方法 | v2 wire | 响应/载荷规则 | 错误映射 |
|---|---|---|---|
| `Sessions.List` | `GET /api/session?directory=<d>` | data 数组；分页完整性【待实证】 | envelope 畸形 → 协议违约透出 |
| `Sessions.Get` | `GET /api/session/{id}` | data=Session | 404 → `ErrKindNotFound` |
| `Sessions.Create` | `POST /api/session`，body 含 `location.directory` | data.id 须符合 `^ses` | 4xx/5xx → `ErrKindRuntimeUnavailable` |
| `Sessions.Delete` | `DELETE /api/session/{id}` | 成功即删除 | 404 → `ErrKindNotFound` |
| `Events.Subscribe` | `GET /api/event` SSE | 首帧 `server.connected` 握手；事件归一化见下表 | 断线 → 重连 + `GET /api/session/{id}` 快照自补 |
| `Attention.ListPermissions` | session 级 `GET /api/session/{id}/permission?directory=<d>`（pending 实证仅存于此）；全局 `GET /api/permission/request` MUST 带 `location[directory]` deepObject 过滤才返回 pending，不带过滤为空——不作 pending 来源；`/api/permission/saved` 仅存已保存规则 | pending=`Permission.Request`（`^per` id、sessionID、action、resources、save） | 端点 4xx/5xx → 透出，不得当空 |
| `Attention.ListQuestions` | question 端点不存在（OpenAPI 实证无）；等价=form 族：`GET /api/session/{id}/form`，pending=`Form.Detail.state.status="pending"` | Form 逐字段映射【待实证】入冻结清单 | 404 FormNotFound → 空视图 |
| `Attention.Reply` | permission：`POST /api/session/{id}/permission/{requestID}/reply?directory=<d>` body `{decision: once\|always\|reject}`（204，实证 PASS）；form：`POST /api/session/{id}/form/{formID}/reply` body `{answer}`（204） | 重复回复/幽灵 ID → 404 PermissionNotFoundError / FormNotFoundError | 404 → `ErrKindNotFound`；409 FormAlreadySettled → `ErrKindConflict`；端点缺失 → `ErrKindCapabilityUnavailable` |
| `Prompts.Send` | `POST /api/session/{id}/prompt`，body `{text, id}`（id 须符合 `^msg_`）；附件为同 body `files: FileAttachment[]`（实证：元素 `{uri(必), name?, description?, mention?}`，无 mime 字段）——`PromptRequest.Files[i]{URL,Mime,Filename}` → `uri=URL、name=Filename`，Mime 丢弃（上游无对应字段）；inline base64 形态未实证不开放 | `PromptResult` 逐字段唯一填充：`accepted`（上游 2xx 成功，含 204/空）→ Outcome=accepted、StatusCode=实际状态码（SPI 注释 accepted=204/空为 v1 语义，v2 归一化为 accepted 且保留实际码供诊断）、Body=空、Detail=空；`http_response`（非 2xx）→ StatusCode=实际码、Body=有界截断响应体、Detail=空；`transport_unknown`（httpClient.Do 失败，不推断是否已发出）→ StatusCode=0、Body=空、Detail=底层错误文本；`pre_send_failure`（进入适配层后的编码/构造失败）→ StatusCode=0、Body=空、Detail=失败原因 | 错误通道唯一：仅 SPI 准入失败（binding 无效/provider 未注册等进入适配层前）返回 error；进入适配层后一切失败经 PromptResult 四分类返回 |
| `Status.Current` | SSE `session.execution.started`/`session.execution.succeeded` 边界事件 + `GET /api/session/{id}` 消息流末条 `Session.Message.Idle.outcome ∈ {succeeded, failed, interrupted}`（实证 PASS，p1b_status_seq.json） | 冻结映射：execution.started→busy、execution.succeeded→idle；outcome 仅作完成诊断，不映射 idle/busy | 断线 → 重连快照自补 |
| `Terminal.EnsureTerminal`/`AttachCommand` | 见 D3 TerminalIO 契约（REST 预验证 + 托管单元 attach） | — | 不存在 → `ErrKindNotFound`；归属不符 → `ErrKindBindingInvalid` |
| `Titles.Sync` | `PATCH /api/session/{id}` body `{title}`（204，实证 PASS）；事件侧 `session.renamed` 归一化已实证 | — | — |
| `BindingRuntimeEndpoint` | 纯本地：解 v2BindingData + 派生凭据 | `(port, password, directory)` | 解码失败 → `ErrKindBindingInvalid` |

**事件归一化**（domain EventKind 精确映射；载荷一律 `data.*` 取值 + `data.sessionID` 归属校验，校验失败丢弃并记录）：

| v2 事件 | EventKind | 负载构造 |
|---|---|---|
| `session.created` | `SessionCreated` | `AgentSession{Key:{opencode-v2, default, data.sessionID}, Title:data.title, Parent:data.parentID→同命名空间 SessionKey（无父 nil）, CreatedAt/UpdatedAt:data.time.* epoch 毫秒}`；data 字段名以 `/openapi.json` 为准，未确认字段【待实证】入 D8 冻结清单 |
| `session.renamed` | `SessionUpdated` | 同上 AgentSession 映射，标题取 data 载荷 |
| `session.deleted` | `SessionDeleted` | Key 同上；触发方式【待实证】（spike event.02c 405） |
| message/part 增量 | `StatusChanged` | StatusDetail 逐字段映射【待实证】，冻结前不承载用户可见语义 |
| permission/question 族 | `AttentionRaised`/`AttentionResolved` | Attention 逐字段映射【待实证】（随 attention 端点补证冻结） |
| 错误族 | 保守形态（已冻结） | event.02g 未实证：MUST NOT 依赖具体错误事件名；仅识别 `effect/http-api/stream/failure` stream 级 failure；未知事件 MUST NOT 猜测为 `SessionError`，按保守丢弃+记录处理 |

Attention/StatusDetail/SessionErrorDetail 的逐字段映射随对应补证冻结进 v2 CONTRACT.md；冻结前按【待实证】保守处理，MUST NOT 以「data 映射」笼统代表完成。

### D8：补证任务内嵌为实现门禁

spike 阻断项全部转为本 change 的实现任务（不另开 change）：

0. **StatusReader 未验证期精确返回规则**：status 等价能力实证通过前，`Current` MUST 返回空 map + nil error，并经既有能力健康透出端口上报 status 能力为 `CapUnknown`（`internal/application/agentprovider/agentprovider.go:322`；未验证 ≠ 已证明不支持，MUST NOT 用 `CapUnsupported`）；消费者（后台对账）对空 map MUST 跳过该任务状态断言，MUST NOT 推导 idle/busy；MUST NOT 从 `Session.Info.outcome` 或 SSE 状态事件推导用户可见状态。实证通过后按冻结映射切换并转为 `CapAvailable`
1. ~~status 等价能力运行时实证~~ **已实证 PASS**（run 20261016T090000Z-0bb6ac6c `p1b_status_seq.json`）：冻结映射见 D7 Status.Current 行；第 0 条保守规则在实现切换到冻结映射前仍适用
2. ~~模型/fixture 依赖项补证~~ **已实证**（同 run）：event.02d/e/f、attention.02、prompt.03 全部 PASS；**migration.forward = 有效负面 FAIL**——v1 pending permission 为进程内存态、数据树零持久化，官方升级后 pending 丢失（承接已知限制，见 Risks）
3. ~~适配缺口修正~~ **已完成**（run 20261015T165420Z-4013391d）：session.05/attention.01/event.02c/title.01 转 PASS；migration.reverse 转出明确 VALID_NEGATIVE（检测口径已修）；saved/pending 语义、Files mime、分页完整性均冻结进 D7
4. D6 必答项 #2（envelope 逐项映射）、#3（v1 Data 迁移规则）、#5（四种承接路径连续性）——属 Migration Plan 阶段 4 验收（依赖实现存在），随实现回填 spike design.md，不参与阶段 1 冻结门禁
5. ~~A+ 共享 server 环境/配置隔离实证~~ **已实证 PASS**（`p1b_environment.json`）：环境注入 = `PUT /api/session/{id}/environment`（204 替换式，session 作用域），A/B 会话隔离实证（shell 输出互不串用）
6. ~~挂起停止 wire 补证~~ **已实证冻结**（`p1b_interrupt.json`）：`POST /api/session/{id}/interrupt`（`/abort` 不存在）→ 200 `{interrupted: bool}`；执行中 interrupt 后 outcome=interrupted、可继续 prompt。挂起路径 fail-closed 门禁解除
7. ~~交互 TUI 自动选中实证~~ **已实证（可行）**：机制 = `opencode --server <url> --session/-s <id>`（源码锚点 `packages/cli/src/commands/handlers/default.ts` findSession：存在即启动选中，commit `026b106b`；live pty 实证目标 session 标题渲染命中；证据 R2 `live/tui/tui-autoselect-facts.md`〔外部 scratch 证据归档副本，provenance 见该文件〕，锚点脚本已含断言）
8. **未执行语义证据缺口**：~~prompt.02~~ 已实证 PASS（messageID 经 `data.id` 原值回显）；conn.01（断线重连对账实验）与 event.02g（错误事件映射；无公开触发通道时冻结替代证据与保守行为）仍待补，冻结进 v2 CONTRACT.md 并在 D9 验收矩阵挂单

### D9：实现职责与验收矩阵

职责与依赖（当前无 Fx，`main.go` 手工 wiring 沿用）：

| 层 | 职责 | 本期改动 |
|---|---|---|
| domain | 值对象与不变量（SessionKey/TaskBinding/EventKind） | 无变更 |
| application | SPI 契约与 registry | 新增可选窄接口 `BindingOwnershipChecker`（归属校验端口，D2）；其余接口不变 |
| task | 编排：resolver 准入、承接主流程、按 binding 路由；环境/权限事实供给 seam（v2 侧等价 `SupervisorSideEffects` 注入实现） | D4 替换表 |
| infrastructure/opencode2 | v2 wire 封闭 + A+ supervisor | 全部新增 |
| infrastructure/store | 归属命名空间原子转换新方法 + runtime 实例记录（`provider_runtime`） | D3/D5 |
| cmd/ocdeck-server | 构造、双注册、开关装配 | D4 |

验收矩阵：每窄接口 fixture 单测；resolver 决策表 R1–R12 组合测试（至少覆盖 R9 与「机器 v1/unknown + 开关关」组合的错误分类）；承接原子转换（冲突拒绝/回滚零副作用）；并发 Acquire 单进程；任务 A 挂起/删除不影响 B；承接四路径连续性；AcquireEphemeral→端点解析→删除→Release 完整链；首建补偿三场景（POST 成功后配置失败 / SSE 失败 / 提交失败）；官方升级环境集成验证；v1 回归（v1 任务全路径不变）。mock seam = HTTP transport（v1 同款 loopbackTransport）+ 进程管理；MUST NOT 绕过生产接线 mock 分派层。

### D10：SPI Question 模型演进为结构化字段模型（方案 B，用户决策 2026-10-16）

背景：G2-F3 暴露 v2 form→SPI Question 的最小映射是未冻结的临时语义；用户决策「SPI 可以修改，需同时适配 v1/v2 与未来 provider（codex/pi/dsh）」。经方案咨询（A=冻结最小映射，长期风险高；C=opaque schema，过早泛化），采用方案 B 完整实施（含消费方迁移）。

**类型变更**（domain `agentprovider`；application 继续直接用 domain 类型）：

- `QuestionPayload` 新增 `Title string`、`Fields []QuestionField`；旧 `Questions []QuestionItem` 保留为 deprecated 兼容视图（迁移完成后可删）
- 新增 `QuestionField{Key, Label, Prompt, Type, Required, Options []QuestionOption{Value,Label}, Metadata json.RawMessage}`；`Type` 值域 `text/textarea/boolean/number/select/multiselect`，未知类型保留原值（消费方按可降级文本处理）
- `AttentionReply` 新增 `Values []QuestionAnswer`；旧 `Answers []string` 保留 deprecated
- 新增 `QuestionAnswer{Key string, Value any}`
- 纯函数（domain，不涉及 wire）：`QuestionPayload.EffectiveFields()`（Fields 优先，空则 Questions 转合成 key 字段）；`AttentionReply.EffectiveAnswers(payload)`（Values 优先，空则 Answers 按字段顺序转换；未知/重复 key、必填缺失、数量不匹配 → 参数错误，fail-closed）

**映射规则**：

- v1：每个 `QuestionItem` 一对一映射为 field（`Key="question-N"` 合成稳定 key——非上游 ID；`Label=Header`、`Prompt=Question`、`Type=text`）；同时填充旧 `Questions` 兼容视图；v1 reply 上游契约未冻结则维持 `ErrKindCapabilityUnavailable`，不得猜测
- v2：form 原生映射（`Form.ID/SessionID/Title`；field `key→Key、title→Label、description→Prompt、type→normalizeFormType` 有限映射：string/text→text、boolean、number、enum→select、array enum→multiselect、未知保留原值）；不再拼接文本、不再按位置 zip；回复 key-based（`answer[key]=value`）；`Metadata` 仅保留未入模型的字段，不得成为消费方主路径
- 消费方（task/API/通知/UI DTO）本 change 内迁移：优先读 `Fields`，回退 `Questions`；回复优先 `Values`，兼容 `Answers`

**备选**：方案 A（SPI 不动冻结最小映射）——被拒绝：字段 key 隐式化、类型信息丢失、未来 provider 各自发明降级规则；方案 C（opaque JSON schema）——被拒绝：语义下沉消费方、违背「raw 解析留在 adapter」原则、过早泛化。

## Risks / Trade-offs

- v2 协议在 pre-release/早期 stable 间漂移 → 锚点脚本钉源码 commit；上游新版发布时先跑锚点 diff 再决定是否扩展结论（spike D3 钩子）
- A+ 单 server 故障域变大（一进程挂影响全部 v2 任务）→ supervisor 重拉 + 按 binding 重建；内存累积按 A+ 文档风险 #3 观察，必要时按租户拆分（后续 change）
- SSE volatile（断线丢事件）→ 重连 + 状态自补；status 候选等价能力实证前不做依赖其语义的功能
- 双包并存的重复代码 → 接受（YAGNI 决策 D1）；真实重复出现时再抽 toolkit
- 承接路径的绑定重建与任务历史的用户可见语义 → spec（agent-provider-spi delta）中逐条定义 scenario，实现评审核对
- 官方升级已知限制：v1 pending permission 为进程内存态、零持久化，升级后丢失（migration.forward 有效负面实证）——承接发生在有 pending attention 的任务上时该 pending 项不可恢复；承接流程 MUST 如实记录（last_error/用户提示），用户需重新触发对应操作

## Migration Plan

无数据迁移（官方升级路径承接）。部署为显式依赖阶段，**未过阶段门禁不得进入下一阶段，任一阶段失败开关维持关闭**：

1. **契约补证与冻结**（仅独立上游探测，不含 provider 实现验收）：D8 清单实证项已全部转出结论（逐项标注见 D8；TUI 自动选中已实证——D8-7；仅剩 conn.01/event.02g 未实证，按保守语义冻结）；产出 = 冻结进 v2 CONTRACT.md + 锚点脚本（tasks 1.9）；未冻结项对应能力保持保守语义
2. **适配层实现**：opencode2 包 client/事件/attention/prompt/session/status/terminal（fixture 驱动单测）
3. **共享生命周期与分派**：A+ supervisor、registry 双注册、resolver、承接原子转换（开关默认关，形态=不注册）
4. **验收**：D9 验收矩阵全绿 + spike D6 必答项 #2/#3/#5 回填 spike design.md（承接四路径连续性在本阶段验证——依赖阶段 2/3 的实现，不参与阶段 1 冻结门禁）
5. **开放**：验收通过后允许打开开关；v1 钉扎任务在 v2 机器开始承接

回滚：关开关 → v2 provider 不注册——v2 钉扎任务与已承接任务的 v2 消费点按未注册 provider fail-closed 报错（保留 binding、归属与锚定不变）；MUST NOT 自动转回 v1 执行，不承诺恢复纯 v1 行为；v1 任务数据未被 v2 触碰（承接不改写 v1 存储，会话延续发生在官方升级后的 v2 存储上）

## Open Questions

- 承接开关的具体形态（config 字段 / feature flag）——不影响 spec 与任务拆分，实现时随 config 现有模式定
- v2 钉扎任务的创建入口 UI 形态——跟随既有 provider 选择的呈现方式，实现期定
