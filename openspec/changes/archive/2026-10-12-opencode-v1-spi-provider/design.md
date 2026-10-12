# Design: opencode v1 作为 provider 接入标准 SPI

## Context

当前 opencode v1 的集成硬编码在 task 层：`OCClient` 接口（`internal/task/types.go:81`）的方法签名直接暴露 `opencode.*` 具体类型并有编译期断言锁定（`types.go:119`）；`defaultOCFactory`（`manager.go:573`）是唯一绑定；CLI 启动形态硬编码于 `runtimeCmdArgv`（`activate.go:1072`）；runtime 凭据经 tmux 会话 env 反查（`ShowSessionEnvContext`，`process.go:913`）；session 归属按裸 `session_id` 判定（`claimTaskSessionInTx`，`queries.go:1636`；upsert 使用 `ON CONFLICT(task_id, session_id)`，`queries.go:1662`）。持久化现状：`task_sessions` 主键 `(task_id, session_id)`（`0001_init.sql:62`），无跨任务唯一约束；`tasks.anchor_session_id` 为裸 session ID（`0009`）；migration 只向前（`store.go:101-116`），DB 文件 0600（`store.go:43-44`）。现有测试已禁止 `OPENCODE_SERVER_PASSWORD` 写入 `env_snapshot`（`p1_gate_test.go:131-132`）。项目无 Fx 依赖注入框架，装配在 `cmd/ocdeck-server` 手工完成。

行为规范（SessionKey 归属唯一性、typed events 集合、全能力契约、行为保持、provider 切换重建语义、迁移导入桥接、terminal_ref 失效重建）以 specs delta 为准，本文件不重复定义，只承载机制与决策理由。

## Goals / Non-Goals

**Goals:**
- 定义 SPI 契约包（provider-neutral 类型与接口）并让 opencode v1 作为首个 provider 迁入，对外行为不变。
- 持久化迁移：tasks 表新增 provider 身份与 TaskBinding；`task_sessions` 归属迁入 SessionKey 命名空间；存量确定性回填。
- 迁移顺序保证随时可代码回滚（向前兼容 schema，不提供 down migration）。

**Non-Goals:**
- 不接入 opencode v2 / codex / pi provider；不做 A+ 共享 server 拓扑；不做 TUI toolkit。
- 不改变任何对外行为与 Web DTO 既有字段语义；HTTP API 既有端点与字段保持不变（唯一例外：服务状态端点新增 `capabilities` 只读增量字段，见 D7）。
- 不引入跨任务数据库唯一索引（历史重复归属行的处理策略超出本期，见 Open Questions）。
- 阶段 2 的中间态不作为可发布形态（D6）。
- provider 切换的跨 provider 上游 session 迁移协议（无第二个 provider 可验证，本期采用重建语义，见 specs「Provider 身份模型」）。

## 实现方案概述

新增 SPI 契约包定义 provider-neutral 类型（SessionKey、TaskBinding、typed events、能力接口集合、健康状态），opencode v1 现有客户端迁入为该契约的首个 provider 适配器；task 层全部消费点经 provider registry 按任务持久化的 provider 标识分派；runtime 凭据事实来源从 tmux env 反查切换为持久化 TaskBinding；归属键从裸 session_id 迁移为 SessionKey。全程分 5 个阶段灰度，每阶段可独立验证、随时代码回滚。

## Decisions

### D1: SPI 契约包形态——窄接口集合 + typed events，参照 Completer 范式

SPI 不复制整个 `OCClient` 再让弱 provider 返回 unsupported，而是按消费者拆窄接口（接口形状见 D7）。事件为带类型事件（类型集合归 specs「typed events 契约」），provider 原始事件解析留在适配层内部。实现范式参照项目已有的 `Completer` 抽象（`internal/infrastructure/ai/completer.go:38`）。

**备选**：单一宽接口（现 `OCClient` 泛化）——否决：TUI provider 将大面积 unsupported，且宽接口难以按能力独立演进（oracle 评审 P0-1）。

### D2: Provider 身份模型——四概念分离，v1 实例标识统一单命名空间

区分 provider 标识 / 契约版本区间 / 二进制版本 / TaskBinding（语义归 specs「Provider 身份模型」）。本期只注册一个 provider：`opencode-v1`。

`provider_instance_id` 语义为"稳定的后端数据域/运行池标识"。**本期 v1 全部归属使用统一稳定实例**（整个既有 v1 数据域一个命名空间，取值固定常量 `default`）。理由：归属唯一性约束的冲突集合必须与现状逐点一致——若以归属任务为数据域身份，任务 A/B 对同一上游 session 的 claim 将落在不同 SessionKey，跨任务他主冲突消失、历史重复归属行被"合法化"为不同领域身份，与现有 fail-closed 语义（`internal/domain/session/session.go:193`）冲突，违反行为保持。每任务一进程仅证明事件流隔离，不构成上游 session 数据域隔离。实例拆分推迟到真实隔离数据域出现时（如未来 v2 trust-domain server 池）。

**备选**：task_id 派生值——否决（上述归属语义理由，方向评审确认）；PID/端口/目录/随机 UUID——分别因易变、非身份、可共享、无法确定性回填而否决。

### D3: TaskBinding 持久化——tasks.task_binding 版本化 JSON envelope

tasks 表新增 nullable TEXT 列 `task_binding`，存版本化 JSON envelope：

```json
{
  "version": 1,
  "provider_id": "opencode-v1",
  "provider_instance_id": "default",
  "upstream_session_id": "<anchor session id>",
  "terminal_ref": "ocdeck-<taskID>-runtime",
  "data": { "port": 50001, "directory": "...", "auth": { "kind": "derived" } }
}
```

**秘密凭据一律派生，不落库**（用户决策，行为规范归 specs「运行时托管与任务绑定生命周期」）。**派生公式（envelope version=1 固定，含测试向量列入 tasks）**：

```
password = base64url_nopad(HMAC-SHA256(
    key = master_secret,                        // UTF-8 原始字节
    msg = "ocdeck-agent-v1" || 0x00 || provider_id || 0x00 || task_id  // UTF-8，0x00 分隔
))[:32]   // 取前 32 个 base64url 字符
```

master secret 解析（顺序固定）：环境变量 `OCDECK_MASTER_SECRET` 优先，其次配置文件 `~/.config/ocdeck/master_secret`（0600，TrimSpace 读取）；文件缺失时首启自动生成（随机 32 字节 hex，0600 写入并向 stderr 提示路径——D3 修订：存量安装升级不崩启动）；显式空 env / 空文件 / 读盘其他错误 → 涉及凭据的操作 fail-closed。本期主密钥在既有 derived runtime 生命周期内 MUST 保持稳定，不支持在线轮换（轮换后旧 runtime 不可恢复，须经重建切换）。**legacy 例外**：迁移导入的存量 runtime 密码为历史随机值，存入 `data.auth = {"kind":"legacy","password":"..."}`；该任务下次激活/重建（新进程）起切换为派生凭据（`auth.kind` 变为 `"derived"`）。

当前绑定是任务级单值状态，查询模式为按 task_id 整取，无按字段查询需求。解析规则：读 binding → 校验 provider/instance/terminal_ref 完整性 → 按 `version` 解析 `data` → 任一失败即按恢复失败处理（语义归 specs「运行时托管与任务绑定生命周期」），未知版本 fail-closed。

**凭据角色收敛**（消除双事实来源）：
- `task_binding`：非秘密连接数据（端点/目录/锚定/终端引用）的事实来源（唯一）。
- 秘密凭据：派生函数（新任务）或 legacy 标记的导入值（存量迁移期内）。
- `env_snapshot`：普通环境快照，不承载连接凭据。
- `last_port`：端口分配历史/复用提示，保留该角色。
- tmux 会话 env：仅向 agent 进程注入凭据与来源标记（`OCDECK_CRED_KIND=derived`，非秘密，供 runner 来源判定）的通道；正常生命周期路径的读取全部移除（迁移导入桥接与删除清理为仅有的凭据读取特许例外；persist 恢复的 `OCDECK_TASK_ID` 会话 env 复检为安全校验特许保留——仅核对会话 env 任务标识与 DB 任务一致，不读取凭据或连接端点。语义归 specs）。

**敏感数据保护**：常态 binding 不含秘密；legacy 例外的 `password` 字段与 master secret MUST NOT 进入 API DTO、日志、事件、错误文本；DB 文件 0600 保护（`store.go:43-44`）。

**备选**：展开多列——否决：provider 私有字段随 v2/codex 演化；独立 task_bindings 表——否决：无绑定历史/多版本并存需求。

### D4: SessionKey 持久化——task_sessions 原表加命名空间列，保留旧主键，归属约束维持应用层原子 claim

`task_sessions` 新增 `provider_id`、`provider_instance_id` 两列（回填统一 v1 命名空间），`session_id` 列继续存上游标识。**保留旧主键 `(task_id, session_id)`**：现有 upsert 使用 `ON CONFLICT(task_id, session_id)`（`queries.go:1662`），替换主键且不保留同名唯一约束会使旧二进制（回滚场景）的归属写入失败；本期 v1 为单命名空间，旧主键语义与 SessionKey 行身份等价。主键扩展推迟到真正需要同任务多命名空间的后续变更。

**claim 原子唯一性算法**（在 SQLite 并发下可验证，实现归属 specs「session 归属捕获」的原子 claim 规范）：
1. claim 在 `BEGIN IMMEDIATE` 事务中执行（单写者语义，busy 冲突按 store 层既有重试策略处理，失败如实返回）；
2. 按完整 `(provider_id, provider_instance_id, session_id)` 查询是否被他任务持有；
3. 已被他任务持有 → 返回冲突（不写入）；
4. 未被持有 → 插入/幂等更新本任务行（沿用现有 changed 判定语义）；
5. 归属唯一性维持应用层原子 claim 执行，不新增跨任务唯一索引——存量历史重复归属行不做启动修复，强加唯一索引会使迁移失败或改变历史语义。
6. 新增真实 SQLite 并发测试：两个任务并发 claim 同一 SessionKey，仅一个成功（行为断言与现状一致）。
7. 历史重复行只可能来自迁移前数据；新写入 MUST NOT 制造重复；OwnerOf 遇历史重复行维持现状 fail-closed（typed ambiguity，`session.go:193`）。

`parent_id` 与父 session 同命名空间（同一任务内父子 SessionKey 的 provider/instance 必相同，存裸 upstream parent id 即可由行内命名空间列解释，不额外加列）。

`tasks.anchor_session_id` 迁移到命名空间：新增 `anchor_provider_id`、`anchor_provider_instance_id` 两列，与既有 `anchor_session_id` 共同构成锚定 SessionKey，保持现有 CAS 事务语义。

旧二进制在回滚窗口产生的归属/任务写，重新升级时 MUST 经补齐流程纳入命名空间（见 D6 迁移导入）。

**备选**：独立归属表——否决：迁移成本高且与现有 claim/对齐代码路径偏差大；单一合成键列——否决：不利于按命名空间查询与演进。

### D5: registry 分派形态——taskOcClient 升级为分派单点，全部直调点迁移

provider registry 为 `map[providerID]Provider`（Provider 接口形状见 D7）。`taskOcClient`（`attention.go:912`）升级为按任务 provider 分派的统一入口；`defaultOCFactory` 迁移为 registry 查表。

必须迁移的 client 构造点清单（经 Evidence Drift Check 逐行核实）：`ocFactory` 直调点为 `activate.go:629,1155,1295,1710,1750,1772`、`reconcile.go:387,509`、`suspend.go:342`、`delete.go:541`、`agent_status.go:44`；经 `taskOcClient` 间接构造的点为 `attention.go:912`、`permit_auto.go:362`（`taskOcClient` 内部分派后这些点随之迁移）。另需移除 `internal/application/dto.go:146,152` 对 `opencode.PermissionRequest/QuestionRequest` 类型的直接嵌入，替换为 provider-neutral DTO（字段形状见 D8）。

任务 provider 未注册时 fail-closed（语义归 specs「Provider registry 分派」）。

### D6: 迁移与灰度排序——5 阶段，回滚 = 代码回滚 + 向前兼容 schema

**阶段 0：characterization tests + 行为保持矩阵**。覆盖激活/端口重试、binding 恢复、SSE 捕获与 claim、parent session、anchor bootstrap/CAS/清理、attention/permission/status/diffreview、suspend/delete/reconcile、重启后路由。现有 `store_test.go`、`anchor_recovery_test.go` 的断言扩展至完整 SessionKey。行为保持矩阵见「行为保持验证」节。

**阶段 1：向前兼容 schema**（migration 0018）：新增 `tasks.provider_id`、`tasks.task_binding`、`tasks.anchor_provider_id`、`tasks.anchor_provider_instance_id`、`task_sessions.provider_id`、`task_sessions.provider_instance_id`。**命名空间列一律带常量默认值**（`provider_id DEFAULT 'opencode-v1'`、`provider_instance_id DEFAULT 'default'`、anchor 两列同理）——SQLite `ALTER TABLE ADD COLUMN ... DEFAULT` 使存量行与旧二进制（回滚窗口）的新 INSERT 自动获得正确命名空间，免除回填 SQL 与再升级时的重复回填。`task_binding` 为 nullable TEXT 无默认值。写入行为保持旧语义。

**命名空间防御性补齐**（每次启动、在任何命名空间消费者运行前执行，幂等、单事务）：将命名空间列为 NULL 或空串的行（含旧二进制显式写空值的边界情形）补齐为统一 v1 命名空间，范围覆盖 tasks、task_sessions、非空锚定的 anchor 命名空间列；已有合法值保留。该步骤与 active 任务的 binding 导入 runner（D6 阶段 3）相互独立、先后无依赖，但两者都必须早于对应消费者。

**验证 fixture**：行为保持矩阵新增「升级 → 旧二进制写入（新任务/归属/锚定）→ 再升级」场景，断言再升级后全部行命名空间正确且归属判定结果与现状一致。

**阶段 2：类型与接口重构，旧事实来源不变**。task 层 / store 层 / v1 适配器类型迁移到 SessionKey / TaskBinding / typed events，但 runtime 凭据读取仍走 tmux env 反查（由适配器包装成 binding 形态）。**阶段 2 是不可发布的中间态**：对外行为与事务边界不变，与阶段 3 同 release 交付，不得单独发布（与最终契约"凭据不得从环境变量反查"的冲突仅存在于该中间态，release 形态不存在该冲突）。

**阶段 3：迁移导入 + 切换 TaskBinding 为 runtime 事实来源**。先执行迁移导入桥接（行为规范归 specs「迁移导入桥接」），再切换：激活成功时在同一事务内持久化 binding；健康检查、attention、permission、status、diffreview、恢复、删除全部按 binding 路由。binding 存在后正常路径上旧来源内容不同亦不得覆盖。

**迁移导入桥接的执行契约**（机制级，行为规范归 specs「迁移导入桥接」）：
- **触发**：server 每次启动时由启动序列执行的后台 runner（非一次性标记、非显式命令）。runner 无状态幂等：完成判定不依赖标记位，而由候选集收敛为空自然达成。
- **统一候选规则**：候选 = 持久化 active 的任务。**主密钥准入门（先于一切网络验证与桥接）**：runner 启动时先解析并校验 master secret 可用（能完成派生计算）；不可用则本轮 runner 整体放弃且 **server MUST 以启动错误拒绝进入正常生命周期与服务就绪**（候选屏障不释放、不耗恢复预算、不改任务状态、不读 env——规范归 specs「迁移导入桥接」）。对每个候选按「验证或导入」处理：
  1. binding 存在且可解析 → 直接以其端点/凭据做健康检查（derived 凭据现场计算）；通过则保留（视为有效），不读取旧来源；
  2. binding 缺失、解析失败、或健康检查失败 → 经**来源标记判定**（规范归 specs「迁移导入桥接」，唯一判据为 tmux env 的非秘密标记 `OCDECK_CRED_KIND`，MUST NOT 以密码值比对代替）：标记为 `derived` → 派生凭据 runtime，MUST NOT 读取/导入/落库其密码，记 last_error 标注疑似主密钥或凭据问题，不修改 binding；标记缺失 → 旧二进制随机凭据 runtime（回滚窗口重拉，activate.go:1139 现状），读取端口与密码并健康检查，通过则以 `auth.kind="legacy"` **写入或替换** binding，失败记 last_error。该任务下次激活/重建（新进程）起切换为派生凭据。防御性不变量：任何路径 MUST NOT 把等于当前派生值的密码写入存储（比对仅作防御断言，不作来源判据）。
- **单任务失败继续**：单任务失败不中断其余任务；不触发额外自动恢复（既有 watcher/重拉路径自然接管）。
- **启动屏障**：runner 对某候选任务出结论（保留/替换/失败）之前，正常生命周期消费者（resumeActive、watcher 重拉、attention 对账等）MUST NOT 处理该任务——候选任务在结论前视为"导入待定"，消费者跳过且不判恢复失败、不耗预算。runner 崩中断后重启即续跑（幂等，无断点状态）。
- **副作用边界**：归 specs「迁移导入桥接」（不建进程、不删 session、不改锚定与归属）。

**阶段 4：清理旧读取路径**。正常生命周期路径移除 tmux env 反查、`last_port` 恢复依据、`env_snapshot` 的 runtime 解释（迁移导入桥接不在清理范围——它是 runner 的特许运维通道，每次启动幂等执行，见 D6 阶段 3；persist 恢复的 `OCDECK_TASK_ID` 会话 env 复检为安全校验特许保留——仅核对会话 env 任务标识与 DB 任务一致，不读取凭据或连接端点）。旧字段保留于 schema（支持代码回滚，符合只向前 migration 机制）。

**回滚定义**：migration 提交前失败 → 事务回滚；提交后代码启动失败 → 恢复旧二进制（忽略新列、旧主键不变）继续运行；不提供自动 down migration；新旧二进制不得并发操作同一 DB 的 session 归属（升级期间单写者/停机边界为部署前提）；旧二进制回滚窗口内产生的写，重新升级时 MUST 经命名空间补齐与迁移导入重新纳入，不得直接信任切换前遗留的 binding；真正 schema 降级需离线备份 + 专用降级步骤。

**备选**：一次性切换——否决：违反"随时可回滚"；长期双事实双写——否决：归属唯一性判定双写易出现单边成功。

### D7: SPI 接口形状（provider-neutral 契约，Go 形状）

分层落点见 D11。枚举一律用带稳定字符串值的类型；VO 与接口如下。**说明：以下为接口形状示意（非独立可编译片段）**——跨包引用约定为 application 包导入 domain 包并直接引用其类型；方法体、import 与常规装配代码从略。

```go
package agentprovider // internal/domain/agentprovider

// --- 身份与绑定 ---
type SessionKey struct {
    ProviderID         string // 如 "opencode-v1"
    ProviderInstanceID string // 本期固定 "default"
    UpstreamID         string // 上游 session id
}
type TaskBinding struct {
    Version            int             // envelope 版本，本期恒为 1
    ProviderID         string
    ProviderInstanceID string
    UpstreamSessionID  string
    TerminalRef        string          // tmux 会话名，如 "ocdeck-<taskID>-runtime"
    Data               json.RawMessage // provider 私有，按 Version 解码
}

// --- 事件 ---
type EventKind string
const (
    EventSessionCreated    EventKind = "SessionCreated"
    EventSessionUpdated    EventKind = "SessionUpdated"
    EventSessionDeleted    EventKind = "SessionDeleted"
    EventStatusChanged     EventKind = "StatusChanged"
    EventAttentionRaised   EventKind = "AttentionRaised"
    EventAttentionResolved EventKind = "AttentionResolved"
    EventSessionError      EventKind = "SessionError"
)
type AgentStatus string
const (
    StatusUnknown AgentStatus = "unknown" // 上游未知/缺失枚举的唯一映射，不得猜测
    StatusIdle    AgentStatus = "idle"
    StatusBusy    AgentStatus = "busy"
    StatusRetry   AgentStatus = "retry"
)
type AttentionKind string
const (
    AttentionPermission AttentionKind = "permission"
    AttentionQuestion    AttentionKind = "question"
)
// AgentEvent 按 Kind 取用恰好一个非 nil 负载字段；不变量：Session（Key）恒非零。
type AgentEvent struct {
    Kind      EventKind
    Key       SessionKey
    Session   *AgentSession       // SessionCreated/Updated/Deleted 时非 nil
    Status    *StatusDetail       // StatusChanged 时非 nil
    Attention *Attention          // AttentionRaised/Resolved 时非 nil
    Error     *SessionErrorDetail // SessionError 时非 nil
}
type StatusDetail struct { // 承载 v1 session.status 全量信息（session_status.go:8，agent_status_state.go:681 消费）
    State   AgentStatus
    Attempt int    // State=retry 时有效：当前重试序号
    Message string // State=retry 时有效：失败摘要
    NextAt  int64  // State=retry 时有效：下次尝试时刻，epoch **毫秒**（与上游一致，不得换算）
}
type SessionErrorDetail struct { // 承载结构化错误字段（session_error.go:9 现状）；可空字段保留可空语义
    Name        string // 必填：TrimSpace 非空
    Message     string // 必填：TrimSpace 非空
    StatusCode  *int   // 可空：缺失/null 为 nil；类型非法仅降级该字段为 nil
    IsRetryable *bool  // 可空：同上
}
type Attention struct {
    ID         string
    Kind       AttentionKind
    Session    SessionKey
    Permission *PermissionPayload // Kind=permission 时非 nil
    Question   *QuestionPayload   // Kind=question 时非 nil
}
type PermissionPayload struct { // 字段与 v1 opencode.PermissionRequest 一致（attention.go:21）
    ID, SessionID, Permission string
    Patterns []string
    Metadata json.RawMessage
}
type QuestionPayload struct { // 字段与 v1 opencode.QuestionRequest 一致（attention.go:53）
    ID, SessionID string
    Questions     []QuestionItem
}
type QuestionItem struct{ Header, Question string }
// AgentSession 位于 domain（provider-neutral VO），供 AgentEvent 负载与 SessionControl 共用——
// 禁止放 application（会造成 domain→application 反向依赖）。
type AgentSession struct {
    Key       SessionKey
    Title     string
    Parent    *SessionKey // 同命名空间；无父为 nil
    CreatedAt int64       // epoch 毫秒（与上游 time.created 一致）
    UpdatedAt int64       // epoch 毫秒
}
```

```go
package agentprovider // internal/application/agentprovider

type TaskRef struct { // 入口上下文：任务标识 + 运行目录 + 有效模式（已解析）
    TaskID    string
    Directory string
    Mode      string // worktree | local-path
}
type ProviderMeta struct {
    ProviderID       string
    DisplayName      string
    ContractMin      string // 已验证契约区间下限
    ContractBaseline string // 已验证契约区间上限
}
// EventStream 为 volatile 事件流：断流/溢出即终止，Err() 返回终止原因；
// 重连与断流后对齐由调用方负责（specs「typed events 契约」）。
type EventStream interface {
    Events() <-chan AgentEvent
    Err() error
    Close() error
}
type AttentionReply struct { // permission/question 回复
    Action string // permission 取值集合以 CONTRACT.md 锚点为准；question 为答案文本数组的封装按 v1 client 现状搬运
    Answers []string
}

type RuntimeSupervisor interface {
    Acquire(ctx context.Context, task TaskRef) (TaskBinding, error)          // 激活：启动 runtime 并建立绑定
    // Resume 恢复/重建；policy 承载入口差异（D9）：恢复预算的准入判定由 task 层经
    // BeforeLaunch 回调提供（supervisor 内部每次启动进程前调用；预算所有权留在 task 层，
    // 对应现有 beforeCreate 钩子，recovery.go:756）。
    Resume(ctx context.Context, task TaskRef, old TaskBinding, policy ResumePolicy) (TaskBinding, error)
    // AcquireEphemeral 删除清理专用：一次性内存临时绑定——MUST NOT 持久化、不执行锚定
    // bootstrap、不创建 session、不建立 SSE、不提交任务状态（规范归 specs 删除清理例外）。
    AcquireEphemeral(ctx context.Context, task TaskRef) (TaskBinding, error)
    Release(ctx context.Context, binding TaskBinding) error                  // 挂起/删除：幂等；失败记 cleanup notice
}
type ResumePolicy struct {
    AllowSessionCreate bool                          // tryRepairRuntime=false；resumeActive/ensureRecovery=true（锚定失效/预检 404 时）
    AllowRebuild       bool                          // terminal_ref 失效时是否允许重建 runtime
    BeforeLaunch       func(ctx context.Context) error // supervisor 内部每次启动前的准入闸门（permit/退避/继续校验）
}
// ListResult 承载列表边界语义：v1 保留原始 limit=1000，数量等于 limit 时 Overflow=true
//（client.go:408 / ErrSessionOverflow 现状）。Overflow 为不完整快照信号，不是错误：
// 对齐路径 MUST 按 complete=false 处理（orchestration spec「session 归属捕获」ownedOnly 语义），
// MUST NOT 按普通错误中止；锚定列表校验对 overflow 的既有失败语义保留。
type ListResult struct {
    Sessions []AgentSession
    Overflow bool
}
type SessionControl interface {
    List(ctx context.Context, b TaskBinding) (ListResult, error)
    Get(ctx context.Context, b TaskBinding, s SessionKey) (AgentSession, error) // 单 session 预检（diffreview 用，diffreview_capability.go:216）
    Create(ctx context.Context, b TaskBinding) (AgentSession, error) // 返回上游 ID 供原子 claim
    Delete(ctx context.Context, b TaskBinding, s SessionKey) error   // 仅允许删除本任务拥有的（specs 归属规则）
}
type EventSource interface {
    Subscribe(ctx context.Context, b TaskBinding) (EventStream, error)
}
type AttentionIO interface {
    ListPermissions(ctx context.Context, b TaskBinding, s SessionKey) ([]PermissionPayload, error) // REST 对账快照（types.go:90 现状）
    ListQuestions(ctx context.Context, b TaskBinding, s SessionKey) ([]QuestionPayload, error)
    Reply(ctx context.Context, b TaskBinding, a Attention, reply AttentionReply) error // 目标不存在/已了结 → ErrorKind=NotFound（按响应体分类，见 D8）
}
// PromptOutcome 保留 v1 prompt_async 的四类发送结果（prompt_async.go:25-28），
// 至多一次发送决策依赖该分类，不得压平为 error。
type PromptOutcome string
const (
    PromptAccepted         PromptOutcome = "accepted"
    PromptHTTPResponse     PromptOutcome = "http_response"
    PromptTransportUnknown PromptOutcome = "transport_unknown"
    PromptPreSendFailure   PromptOutcome = "pre_send_failure"
)
type PromptRequest struct {
    Text      string
    MessageID string        // 调用方生成的幂等标识，透传不重写
    Files     []PromptFile  // 附件（text 由 Text 承载，不进入 Files）
}
type PromptFile struct { // 与 v1 PromptFilePart 一致（prompt_async.go:63）
    URL      string // file:// 绝对路径 URI
    Mime     string
    Filename string
}
// PromptResult 字段填充规则与 Outcome 绑定（唯一，沿用 prompt_async.go:30-44 现状）：
//   accepted:           StatusCode=204，Body/Detail 为空
//   http_response:      StatusCode=实际状态码，Body=有界截断响应体，Detail 为空
//   transport_unknown:  StatusCode=0，Body 为空，Detail=底层错误文本
//   pre_send_failure:   StatusCode=0，Body 为空，Detail=底层错误文本
type PromptResult struct {
    Outcome    PromptOutcome
    StatusCode int
    Body       string
    Detail     string
}
type AsyncPrompt interface {
    // 通道规则（唯一）：仅 SPI 准入失败（binding 无效、provider 未注册等进入 provider 前）返回 error；
    // 进入 provider 适配层后 MUST 严格沿用四分类经 PromptResult 返回——httpClient.Do 失败
    // （dial/超时/断连/ctx 取消）一律 transport_unknown，MUST NOT 推断请求是否已发出。
    Send(ctx context.Context, b TaskBinding, s SessionKey, req PromptRequest) (PromptResult, error)
}
type StatusReader interface {
    Current(ctx context.Context, b TaskBinding) (map[SessionKey]AgentStatus, error)
}
type TerminalIO interface { // v1 经 tmux 兑现：输入/输出由 attach 后直连承担，不经 ocdeck 中继
    EnsureTerminal(ctx context.Context, b TaskBinding) (terminalRef string, err error)
    AttachCommand(ctx context.Context, b TaskBinding) (argv []string, err error) // 浏览器/CLI attach 用
    // 程序化输入注入/输出采集（send-keys/capture-pane）属 TUI toolkit，本期 Non-Goal
}
type TitleUpdater interface {
    Sync(ctx context.Context, b TaskBinding, s SessionKey, title string) error // title 为写入目标（title_sync.go:162 现状）
}
type Provider interface {
    Meta() ProviderMeta
    Supervisor() RuntimeSupervisor
    Sessions() SessionControl
    Events() EventSource
    Attention() AttentionIO
    Prompts() AsyncPrompt
    Status() StatusReader
    Terminal() TerminalIO
    Titles() TitleUpdater
}

// --- 错误与能力健康 ---
type ErrorKind string
const (
    ErrKindProviderNotFound    ErrorKind = "provider_not_found"
    ErrKindBindingInvalid      ErrorKind = "binding_invalid"
    ErrKindRuntimeUnavailable  ErrorKind = "runtime_unavailable"
    ErrKindNotFound            ErrorKind = "not_found"
    ErrKindConflict            ErrorKind = "conflict"
    ErrKindCapabilityUnavailable ErrorKind = "capability_unavailable"
)
// Error 包装规则：每个 ErrorKind 对应一个包级 sentinel（ErrProviderNotFound、ErrBindingInvalid、
// ErrRuntimeUnavailable、ErrNotFound、ErrConflict、ErrCapabilityUnavailable）；
// Error.Is(target) 按 Kind 匹配对应 sentinel（errors.Is 可用）；Err 保留 provider 原始错误链（errors.As 可取）。
type Error struct {
    Kind ErrorKind
    Op   string
    Err  error
}
func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool // 按 Kind 匹配对应 sentinel

type CapabilityState string
const (
    CapUnknown      CapabilityState = "unknown"
    CapAvailable    CapabilityState = "available"
    CapUnsupported  CapabilityState = "unsupported"
    CapDegraded     CapabilityState = "degraded"
)
type CapabilityScope string
const (
    ScopeProvider CapabilityScope = "provider"
    ScopeRuntime  CapabilityScope = "runtime"
    ScopeSession  CapabilityScope = "session"
)
type CapabilityHealth struct {
    ProviderID string
    TaskID     string          // runtime scope 时定位所属任务
    Session    *SessionKey     // session scope 时非 nil
    Primitive  string // session_control | event_source | attention_io | async_prompt | status_reader | terminal_io | title_updater
    Scope      CapabilityScope
    Epoch      int64  // runtime 实例代际：Acquire 成功或 Resume 重建成功时递增；server 重启后归零为 unknown
    State      CapabilityState
    Reason     string
}
```

**能力健康透出**：按 runtime 代际维护于内存；透出通道为日志（状态迁移记 warn）+ 现有服务状态端点（`GET /api/v1/server/status` 模式）新增 `capabilities` 数组字段（元素字段名即 CapabilityHealth 的 JSON 小驼峰键）；不持久化；metrics 出口推迟。状态迁移规则：初始 `unknown`；探测成功 → `available`；协议违约/契约外响应 → `degraded`（带 reason）；上游明确不支持 → `unsupported`。

### D8: v1 外部契约映射（来源链闭环，字段级）

v1 适配器 MUST 逐字继承现有解析语义（现状行为为 oracle；以下字段名均已对照代码核实）：

**SSE 事件 → typed events**（envelope `{type, properties}`，`client.go:73`）：

| 上游事件 | properties 字段 | 映射 | 缺失/异常处置 |
|---|---|---|---|
| `session.created` | `info.id`（必填）、`info.title`、`info.parentID`、`info.time.created/updated`、`info.directory`（同源校验，缺失或与本任务目录不同 → 丢弃，`activate.go:1473`） | `SessionCreated`（负载 AgentSession） | 缺 id 或目录不匹 → 丢弃 + 诊断日志（现状） |
| `session.updated` | 同上 | `SessionUpdated`（负载 AgentSession） | 同上；未归属 session 忽略（spec 语义） |
| `session.deleted` | `info.id` + `info.directory`（同源校验，不同或缺失均丢弃） | `SessionDeleted` | fail-closed 丢弃（`activate.go:1546-1552`） |
| `session.status` | `sessionID`（回退 `info.id`）+ `status.type` ∈ {idle,busy,retry}（retry 含 `attempt/message/next`，映射 StatusDetail） | `StatusChanged` | SSE 路径：未知枚举**忽略不应用**（保持投影，`agent_status_state.go:667` 现状）；REST 路径（StatusReader.Current）：未知枚举 → `StatusUnknown`。归属反查 fail-closed（`activate.go:1593-1616`） |
| `session.error` | 字段路径（唯一，`session_error.go:9`）：`properties.sessionID` → `AgentEvent.Key.UpstreamID`（必填，MUST NOT 回退 `info.id`）；`error.name` → `Name`；`error.data.message` → `Message`；`error.data.statusCode` → `StatusCode`；`error.data.isRetryable` → `IsRetryable`。sessionID/name/message 为 TrimSpace 后非空字符串（详情保留原文），任一必填非法 → 丢弃整个事件且不做归属 I/O；statusCode 仅接受可无损表示为 Go int 的 JSON 整数，isRetryable 仅接受 JSON 布尔；可空字段缺失/null/类型非法时仅该字段置 nil，不拒绝整个事件 | `SessionError` | 畸形事件 MUST NOT 落入通用反查（`activate.go:1582-1590`） |
| `permission.asked` / `permission.v2.asked` | `id`+`sessionID` 必填，`permission` 必填，`patterns` string[]，`metadata` | `AttentionRaised`（permission） | 缺字段 → (zero,false) 静默忽略（`attention.go:95-118`） |
| `permission.replied` / `permission.v2.replied` | `sessionID`+`requestID` 必填 | `AttentionResolved`（permission） | 同上 |
| `question.asked` / `question.v2.asked` | `id`+`sessionID` 必填，`questions` 非空且每条 header+question 非空 | `AttentionRaised`（question） | 同上 |
| `question.replied` / `question.v2.replied`、`question.rejected` / `question.v2.rejected` | `sessionID`+`requestID` 必填 | `AttentionResolved`（question） | 同上 |
| 未知 type | — | 不映射 | 静默忽略（不中断流） |

**HTTP 端点 → 窄接口**：

| 端点（v1） | 接口方法 | 说明 |
|---|---|---|
| `GET /global/health` | RuntimeSupervisor 内部健康探测 | `{healthy, version}`（`client.go:66`）；版本用于契约区间比对 |
| `GET /session?directory=` | SessionControl.List | directory 参数 MUST 显式携带（orchestration spec「连接管理」）；`limit=1000`，`count==limit` → `ListResult.Overflow=true`（不完整快照信号，非错误） |
| `GET /session/{id}?directory=` | SessionControl.Get | 单 session 预检（diffreview_capability.go:216 现状） |
| `POST /session?directory=` | SessionControl.Create | 响应 ID 供原子 claim |
| `DELETE /session/{id}` | SessionControl.Delete | 仅本任务拥有的（spec 归属规则） |
| `GET /session/status` | StatusReader.Current | map 键为上游 session id，适配层包装为 SessionKey |
| `GET /permission?directory=` | AttentionIO.ListPermissions | REST 对账快照（attention.go:442/620 消费现状） |
| `GET /question?directory=` | AttentionIO.ListQuestions | 同上 |
| `POST /permission/{id}/reply` | AttentionIO.Reply（permission） | reply 取值集合以 CONTRACT.md 锚点为准；按响应体 `_tag` 分类（例外见下） |
| `POST /question/{id}/reply` | AttentionIO.Reply（question） | 同上 |
| `PATCH /session/{id}`（title） | TitleUpdater.Sync | 标题写入（title_sync.go:162 现状） |
| `POST /session/{id}/prompt_async` | AsyncPrompt.Send | 四类结果映射 PromptOutcome 及字段填充规则（见 D7）；能力探测三态沿用 `prompt_async.go:125-207` 现状 |
| SSE `GET /event` | EventSource.Subscribe | volatile：断流即终止 |

**HTTP 错误 → typed error（默认规则 + 操作级例外）**。默认：网络/连接/超时/5xx → `ErrKindRuntimeUnavailable`；404 → `ErrKindNotFound`；401/403 → `ErrKindBindingInvalid`（凭据失效）；decode 失败/契约外形状 → `ErrKindRuntimeUnavailable` 且对应能力健康置 `degraded`（带 reason）。**操作级例外**（优先于默认）：
- AttentionIO.Reply：按完整响应体 `_tag` 分类（`permission_reply.go:43` 现状），区分"请求已了结/不存在"（→ `ErrKindNotFound`）与能力端点缺失（→ `ErrKindCapabilityUnavailable`），不得仅按 404 压平；
- TitleUpdater.Sync：所有 HTTP 404（不区分路由级/会话级）一律映射为 `ErrKindCapabilityUnavailable`——沿用既有标题能力降级语义（`session_title.go:15`）与 runtime 代际降级缓存（`title_sync.go:120`），MUST NOT 套用"对象不存在"默认分类；
- AsyncPrompt.Send：发送结果不经 error 通道，四分类经 `PromptResult.Outcome` 返回（上表）。通道规则以 D7 为唯一表述：仅进入 provider 前的 SPI 准入失败（registry/binding）返回 error；适配层本地构造失败 → `pre_send_failure`；一切 `httpClient.Do` 失败（dial/超时/断连/ctx 取消）→ `transport_unknown`，MUST NOT 推断请求是否已发出，不得以增加健康预检改变分类；
- 能力探测类端点不存在 → `ErrKindCapabilityUnavailable`（保留"能力故障"与"对象不存在"的区别）。
EventSource 断流、解析失败与上游关闭统一经 `EventStream.Err()` 传递（断流=终止原因，非 typed error 返回）。

### D9: 主流程与入口变体（结构化）

四个运行时入口共享同一主流程。**关键顺序约定**：`Acquire`/`Resume` 的返回点 = **最终 runtime 就绪且凭据确定**（双启动等内部子协议在 supervisor 内部完成，见下）；SSE 订阅在 supervisor 返回后建立，订阅对象恒为**最终 runtime**；锚定创建/确认与原子 claim 属于 supervisor 子协议的一部分（按入口差异表），主流程图中列出仅为表达其在整体时序中的位置：

```
解析有效模式（项目 kind + 任务 mode，非法组合报错零副作用）
  → registry 查找 provider（未注册 fail-closed）
  → binding 前置检查（按入口区分，见下表）
  → Acquire（Activate）或 Resume（恢复类入口）
      ├─ Activate 内部子协议（沿用 activate.go:1688 现状顺序；按锚定状态分三支）：
      │   ① 有有效锚定：带 --session 直接启动 → 健康检查+能力探测+列表校验通过即返回（activate.go:1704）
      │   ② 锚定失效：条件清空旧锚定（CAS）后转分支 ③
      │   ③ 无锚定（双启动）：bootstrap 进程启动 → 健康检查+能力探测 → POST /session 创建锚定 + 原子 claim
      │      → 确认 bootstrap 进程终止 → 正式进程（--session，新凭据）启动
      │      → 健康检查+能力探测+锚定列表校验 → 返回
      └─ 恢复类入口：Resume 既有 runtime（或 terminal_ref 失效时重建）→ 锚定确认/创建 + claim → 返回
  → 建立 EventSource（SSE 订阅，对象为最终 runtime）
  → 按有效模式对齐（worktree 全量 / dir、local-path ownedOnly）
  → 提交任务状态（binding 与任务状态 CAS 同事务，见下）
```

**binding 提交协议**（D6/D9/D10 的唯一表述）：supervisor（Acquire/Resume）只确定并**返回**最终 binding，MUST NOT 自行持久化；binding 由 task 层在激活/恢复成功的提交事务内写入（与任务状态 CAS 同事务；恢复类入口的新绑定原子替换旧绑定也在该事务内，契合 specs「终端引用失效重建绑定」）。supervisor 返回后、提交前的 SSE 建立与对齐使用内存中的 binding 值（端点/凭据已知，不经 DB）。SSE 建立、对齐或状态提交失败时：binding 零持久化副作用（未写入），runtime 按既有 KillResult/cleanup notice 规则终止，恢复类入口的旧 binding 保持原值。

| 入口 | 允许的 binding 前置状态 | Acquire/Resume | 允许创建 session | claim 冲突语义 | 对齐模式 | 失败清理与预算 |
|---|---|---|---|---|---|---|
| Activate（首次） | **缺失为正常前置**（校验通过即可） | Acquire | 是（双启动子协议，不耗恢复 permit） | 激活失败 + last_error | 按有效模式 | 既有统一补偿 + cleanup notice 回放 |
| resumeActive | 必须存在且可解析；缺失/失效 → 恢复失败（迁移导入已完成的正常运行期） | Resume | 是（锚定失效时） | 恢复失败 + last_error（计预算） | 按有效模式 | 既有恢复路径 |
| tryRepairRuntime | 必须存在且可解析；**无锚定且无 owned session 时在「对齐」步正常终止**（修复完成，无需锚定创建） | Resume（仅修复 SSE/对齐/锚定） | 否 | 忽略 + 诊断 | ownedOnly/按有效模式 | 挂起失败修复语义 |
| ensureRecovery（含 ReopenAttach 转发） | 必须存在且可解析（terminal_ref 失效按 specs 重建语义） | Resume | 是（预检 404 时） | 恢复失败 + last_error（计重拉预算） | 按有效模式 | 重拉预算耗尽落挂起 |
| 迁移导入（独立流程） | 全部 active 任务（验证或导入，规则归 D6 执行契约） | 不 Acquire/Resume | 否 | 不适用（不写归属） | 不对齐 | specs「迁移导入桥接」副作用边界 |
| 删除清理（删除任务时的上游归属清理） | 分支规则：① 有有效 binding 且 runtime 可复用（存活）→ 直接复用既有 binding；② 无可复用 runtime（无 binding 或 binding 陈旧/对应 runtime 已终止）→ AcquireEphemeral 内存临时绑定——陈旧 binding MUST NOT 走普通 Resume（不得触发恢复、锚定创建或 active 提交）；旧 binding 的保留规则：删除成功则随任务级联删除，删除失败则保留原值 | AcquireEphemeral（分支②） | 否 | 不适用 | 不对齐 | 清理失败阻断任务删除（D10 例外行） |

各恢复入口与 ResumePolicy 的映射：`resumeActive` → {AllowSessionCreate=true, AllowRebuild=true, BeforeLaunch=恢复预算闸门}；`tryRepairRuntime` → {AllowSessionCreate=false, AllowRebuild=false, BeforeLaunch=nil（不会被调用——修复不启动进程；存活性/健康校验失败即修复失败，走原挂起补偿，suspend.go:320 现状语义）}；`ensureRecovery`（含 ReopenAttach 转发）→ {AllowSessionCreate=true, AllowRebuild=true, BeforeLaunch=重拉预算闸门}。`BeforeLaunch` 的调用点 MUST 先于端口分配与任何进程启动副作用（沿用现有 beforeCreate 顺序，recovery.go:756）。

### D9a: provider 切换流程（本期不可达，启用前置明确）

**本期可达性**：registry 仅注册 `opencode-v1`，切换路径本期不可达（无目标 provider）。specs「Provider 身份模型」的切换语义为未来契约；启用前置条件：(1) 第二个 provider 注册；(2) 归属行身份支持同任务多命名空间——旧主键 `(task_id, session_id)` 下，目标 provider 产生相同 upstream ID 时会与保留的历史行主键冲突，需先行演进（行身份含 namespace，或历史行归档存储），且对齐/attach/删除/owned 集合查询 MUST 按当前 binding 命名空间过滤（F-21 风险见 Risks）；(3) 本节失败矩阵落地为实现。

**切换步骤与失败矩阵**（行为规范归 specs「Provider 身份模型」）：

| 步骤 | 失败处置 |
|---|---|
| ① 校验输入（任务 suspended、目标 provider 已注册） | 失败 → 零副作用报错（诊断性 last_error 允许） |
| ② Release 旧 runtime + 清 task_binding | Release 失败 → 记 cleanup notice，**切换失败**，provider_id 不变（本路径为例外，排除 D10「Release 失败不阻断」通用规则）；允许重试（Release 幂等） |
| ③ DB 原子提交（provider_id 更新、anchor 三元组清空、归属行保留为历史） | 提交失败 → provider_id 保持原值、binding 已空状态保留；重试时识别「旧 runtime 已释放」状态并跳过 ② 直接提交 |
| ④ 成功 | 任务保持 suspended，下次激活由目标 provider Acquire 建立新 runtime/binding/锚定 |

输入已为「suspended 且 binding 已释放」的任务：①后直接执行 ③（幂等）。runtime 清理是幂等补偿（不可随 DB 事务回滚），DB 字段原子提交——两者分离规定，不得假设整体原子。

### D10: 副作用边界与失败矩阵

任何入口在执行 spawn/connect/create/save/send 副作用前 MUST 完成：有效模式解析、provider 注册检查、binding 前置检查（按 D9 表区分入口）。失败矩阵：

| 失败点 | 已发生副作用的处置 | 记录 |
|---|---|---|
| 有效模式解析失败（非法组合） | 完全零副作用——不修改任务状态/归属/锚定/绑定，**不写 last_error**（与 orchestration spec「dir+worktree 非法组合 fail-closed」逐字一致） | 仅日志 |
| provider 未注册 | 零进程/网络副作用；诊断性 last_error 为允许的持久化副作用（specs「Provider registry 分派」） | last_error |
| binding 前置检查失败 | 零进程/网络副作用；按入口语义（D9）：Activate 缺失为正常前置，恢复类入口判恢复失败 | last_error（恢复类入口） |
| runtime 已启动但 claim/anchor 失败 | 按既有 KillResult/cleanup notice 规则终止 runtime（Activate 统一补偿，orchestration spec「端口分配策略」语义不变） | cleanup notice + last_error |
| SSE 建立/对齐/状态提交失败（supervisor 已返回最终 binding） | binding 零持久化副作用（未写入）；runtime 按既有 KillResult/cleanup notice 规则终止；恢复类入口的旧 binding 保持原值（D9 binding 提交协议） | cleanup notice + last_error |
| 任务状态提交事务失败（含 binding 写入） | 终止 runtime，不进入 active；binding 未落库 | last_error |
| Release 失败 | 记 cleanup notice（沿用既有 notice 体系），不阻断后续 | cleanup notice |
| 删除清理的临时 runtime 终止非净或清理落账失败 | **阻断任务 DB 删除**（本路径为例外，排除上方 Release 通用规则），cleanup tickets 保留可重试；MUST NOT 越过删除提交点导致票据随级联删除丢失（delete.go:483 现状语义） | cleanup notice + last_error |
| 删除/挂起失败 | binding 保留，随既有重试路径处置 | last_error |

允许的持久化副作用：cleanup notice、session_overflow notice、last_error、任务状态字段；其余诊断仅记日志。

### D11: 实现落点与分层

| 包 | 职责 | 内容 |
|---|---|---|
| `internal/domain/agentprovider`（新增） | provider-neutral VO 与枚举 | D7 上半全部：SessionKey、TaskBinding、AgentSession、AgentEvent、AgentStatus、StatusDetail、SessionErrorDetail、Attention、payload、EventKind |
| `internal/application/agentprovider`（新增） | SPI port 与 registry | D7 下半全部：Provider/RuntimeSupervisor/各窄接口、EventStream、registry、分派、Error/CapabilityHealth |
| `internal/infrastructure/opencode`（改造） | v1 适配器 | 现 client/SSE/事件解析迁入 Provider 实现；runtime supervisor（per-task 形态，沿用现有 tmux 托管） |
| `internal/infrastructure/store`（改造） | 持久化 | SessionKey 列与查询、TaskBinding 列读写、migration 0018 |
| `internal/task`（改造） | 仅消费 application port | D5 清单全部构造点迁移；删除对 `opencode.*` 类型的依赖 |
| `internal/application/dto.go`（改造） | Web DTO | `PendingPermission/PendingQuestion` 改为嵌入 provider-neutral 的 PermissionPayload/QuestionPayload + Since（字段形状不变，DTO 语义不变） |
| `cmd/ocdeck-server`（改造） | 装配 | registry 构造与注入（项目无 Fx，手工装配） |
| mocks | 测试边界 | mock 各窄接口（SPI port）与 store 事务边界，不 mock v1 client 内部 |

依赖方向：`internal/task` → `internal/application/agentprovider` → `internal/domain/agentprovider`；`internal/infrastructure/opencode` 实现 port 并被 registry 注册，task 层不反向依赖。

## 行为保持验证（阶段 0 矩阵）

**方法**：以脚本化 fake opencode server（httptest，按 CONTRACT.md 锚点形状应答 + 可编程 SSE 事件脚本）为固定上游；同一 fixture 分别在旧路径（重构前基线）与新路径（迁入 SPI 后）运行，各自使用独立 DB 实例与独立 tmux server socket，比较六个观察面：

1. HTTP/Web 响应：DTO 序列化后逐字段 JSON 对比（字段序无关、值全等）。**比较例外**：服务状态端点新增的 `capabilities` 字段为纯增量（旧基线无对应），不参与等价比较，单独按新契约验证；
2. store 终态：相关表 dump 为排序行集后对比（SessionKey 新列按回填期望值注入旧基线对比时归一化）；
3. process 调用序列：tmux/进程操作的调用名与参数序列（经现有 process 层记录钩子）；
4. notice/last_error：精确字符串与计数；
5. session ownership：归属集合（SessionKey 归一化后）；
6. SSE 消费结果：事件应用后的内存态（pending 集合、agentStatus）。

能力健康与旧 CapabilityState 的比较映射：`supported` ↔ `available`、`unsupported` ↔ `unsupported`、`unknown` ↔ `unknown`；`degraded` 为新增态，旧路径无对应，仅在契约违约场景断言。

| 领域 | 输入 fixture（固定数据） | 关键断言面 |
|---|---|---|
| 激活/端口重试 | 端口占用脚本、进程死亡脚本、健康超时脚本 | 端口序列、cleanup notice、错误聚合文本 |
| SSE 捕获与 claim | 并发 claim 双任务脚本、断流重连脚本 | ownership 唯一性、对齐补记集合 |
| anchor bootstrap | 有锚定/无锚定/锚定失效三 fixture | 双启动子协议步骤序、CAS 结果 |
| attention/permission/status | 六家族事件脚本（含缺字段畸形事件） | pending 集合、回复路由、状态映射（REST 未知枚举→unknown；SSE 未知枚举忽略不应用） |
| diffreview | 能力探测开/关两 fixture | CapabilityState 映射等价 |
| 标题同步 | 成功 PATCH / 404 降级 / 网络失败三 fixture | 成功写入；404 映射能力降级并按 runtime 代际缓存、发对应 notice；网络失败不写缓存 |
| suspend/delete/reconcile | 正常与各失败注入 | 会话终止顺序、binding/归属清理终态 |
| 重启恢复 | 有 binding / 无 binding（导入前）/ terminal_ref 失效 | 恢复成功/失败分类、预算计数 |
| 迁移导入 | 存活 runtime / 已消失 runtime / 解析失败 | binding 写入与幂等、副作用边界（specs） |
| provider 切换 | 未注册目标 / 正常切换 | fail-closed、业务关联保留与锚定清空（specs） |
| 历史重复行 | 预置重复归属 fixture | OwnerOf fail-closed、新写入不产生重复 |
| 未注册 provider | 篡改 provider_id fixture | 生命周期操作 fail-closed 零副作用 |
| 回滚再升级 | 升级 → 旧二进制写入（新任务/归属/锚定）→ 再升级 | 全部行命名空间正确（DDL 默认值 + 防御性补齐），归属判定与现状一致 |
| 删除清理 | 存量 suspended + owned sessions + NULL binding；陈旧 binding（runtime 已终止）；临时 runtime 清理失败重试 | 分支①复用/分支②临时绑定不持久化、逐项删除 owned、不建锚定/SSE；陈旧 binding 不走普通 Resume；清理失败阻断删除提交且票据可重试，重试成功 |
| 派生凭据 | 新任务激活（derived，含来源标记注入）；存量任务导入（legacy）→ 下次激活切换 derived；缺主密钥；**密钥改变+原 derived runtime 仍存活**；密钥未变+旧二进制随机密码 runtime（回滚序列）；主密钥不可用时启动 | 派生公式固定测试向量通过；DB 全文检索：derived 任务不含密码、legacy 任务仅 `auth.kind="legacy"` 标记字段含导入值；密钥改变时 derived runtime 凭标记识别、禁止读取/落库其密码（记 last_error）；密钥未变的回滚序列经标记缺失识别并 legacy 导入收敛；主密钥不可用时 server 拒绝进入服务就绪、任务状态不变 |

## Risks / Trade-offs

- **历史重复归属行长期存在双重语义** → 统一命名空间保持原有冲突集合与 OwnerOf fail-closed（typed ambiguity）；新写入不得制造重复（D4）；删除任务按 SessionKey 精确删除不影响其他历史拥有者；跨任务唯一索引与清理迁移推迟（Open Questions）。
- **切换后同任务多命名空间历史行与旧主键冲突**（未来启用切换时） → 本期切换不可达（D9a）；启用前置含行身份演进与按命名空间过滤 owned 集合，未满足前不得注册第二个 provider。
- **binding 与 env_snapshot/last_port 双事实来源漂移** → D3 角色收敛 + 阶段 3 的"binding 存在即唯一来源"规则 + characterization tests 覆盖。
- **凭据泄露面** → 秘密凭据派生不落库（D3）；legacy 例外字段与 master secret 的 DTO/日志审查列入 tasks。
- **派生密码不再随重启轮换**（同任务恒定） → 用户已批准的内部变更（派生决策）；威胁模型仍为本机 loopback + DB 0600；若未来需要轮换，按 D3 的轮换不支持条款经任务重建切换。
- **存量活跃任务无完整 binding** → 迁移导入桥接（specs + D6 执行契约）；导入失败走既有恢复失败路径。
- **阶段 2 中间态的类型双形态成本** → 阶段 2 不可发布（D6），与阶段 3 同 release。
- **重构面大于直觉（13+ 构造点 + 启动/恢复/清理全链路）** → 阶段 0 保护网先行；阶段 2/3 分离降低单步风险。
- **provider 未注册 fail-closed 可能误伤数据异常任务** → 回填保证存量任务 provider_id 非空；注册表启动期构建，缺失即启动失败（开发期暴露）。
- **能力健康透出仅日志+状态端点，无 metrics** → 本期可接受（沿用现有 VersionVerified 透出模式），推迟项记录于此。

## Migration Plan

见 D6 阶段 0-4。DDL 变更明细（migration 0018）：

| 对象 | 变更前 | 变更后 |
|---|---|---|
| `tasks` | 无 provider 列 | +`provider_id` TEXT NOT NULL DEFAULT 'opencode-v1'、+`task_binding` TEXT NULL、+`anchor_provider_id` TEXT DEFAULT 'opencode-v1'、+`anchor_provider_instance_id` TEXT DEFAULT 'default' |
| `task_sessions` | PK `(task_id, session_id)` | +`provider_id` TEXT NOT NULL DEFAULT 'opencode-v1'、+`provider_instance_id` TEXT NOT NULL DEFAULT 'default'（存量行与旧二进制新 INSERT 均自动获得命名空间）；**主键不变**（D4，扩展推迟到多命名空间变更） |

协议变更汇总：rpc 无变更；http 既有契约保持（`GET /api/v1/server/status` 新增 `capabilities` 只读字段，为纯增量透出，不改变既有字段语义）；event 由原始 SSE 事件在适配层收敛为 typed events（行为规范归 specs「typed events 契约」，对外 Web 透出形态不变）；ddl 见上表；ticker 等定时任务无变更。

领域模型变更：新增 VO `SessionKey`、`TaskBinding`（结构归 D3/D7）；任务聚合新增 provider 身份属性与绑定关联（生命周期附属现有激活/挂起/删除状态，**无新增状态机**——任务状态机不变；binding 的 Acquire/Resume/Release 为现有激活/恢复/挂起/删除流程内的子动作，入口变体见 D9）。

业务流程变更：激活、重启恢复、后台对账（attention/permit_auto/agent_status/diffreview）的流程步骤不变，runtime 凭据读取的数据源从 tmux env 反查变为持久化 TaskBinding（阶段 3 切换，迁移导入 runner 为前置桥接）；session 归属判定键从裸 ID 变为 SessionKey（行为等价，当前单命名空间）；provider 切换采用保留业务关联的重建语义（规范归 specs「Provider 身份模型」）。

实现流程：按 D6 阶段 0→4 顺序执行，每阶段以测试全绿为进入下一阶段的条件；阶段 1（schema）与阶段 2（类型重构）之间允许同 PR 或分 PR，阶段 3（迁移导入 + 事实来源切换）必须独立 PR 并可独立回滚。

## Open Questions

- 跨任务 `UNIQUE(provider_id, provider_instance_id, session_id)` 索引是否在未来引入：取决于历史重复归属行的清理策略，与本期行为保持无关，推迟到后续 provider 接入前评估。
