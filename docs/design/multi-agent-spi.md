# 多 Agent 适配 SPI 设计

> 状态：草案 v3（架构评审修订 + 全能力契约模型对齐）｜ 目标：ocdeck 通过统一 SPI 适配多种 agent（API 通道：opencode v1/v2；TUI 通道：codex/pi）
> 依据：现状调研 + opencode v2 契约调研（`docs/design/opencode-v2-a-plus-migration.md`）+ 架构评审（2026-10-06）

## 0. 评审结论与本次修订

初版草案被判定"方向正确，但还不能作为可实施的 SPI 契约"：现有系统把**进程生命周期、任务绑定、会话归属、SSE、tmux 所有权**绑在一起，初版只做了传输层抽象（port→Endpoint），没有表达这些边界。本版按评审意见重构：

- `Endpoint` 降级为 provider 内部传输细节，新增持久化 **`TaskBinding`** 与 **runtime supervisor**（§3）
- 新增 **`SessionKey`** 命名空间，解决多 provider 下 session ID 冲突（§4）
- 能力模型从布尔位改为**原语 × 状态 × 作用域**（§5）
- SPI 输出**带类型事件**，raw 解析留在适配器内（§3.3）
- TUI 通道定位为 **TerminalIO task driver**，不伪装成完整 HTTP client（§6.2）
- 落地顺序前置 **v1 characterization tests** 与两个 spike（§8）

## 1. 目标与原则

```
ocdeck（task 生命周期 / attention / permission / diffreview / Web 透出）
  │
SPI（provider-neutral：domain DTO + TaskBinding + 能力模型 + typed events）
  ├── API 通道 ── opencode v1（每任务一进程）│ opencode v2（共享 server 池 + 多 session）
  └── TUI 通道 ── codex │ pi（TerminalIO driver）
```

原则：
1. **边界先于抽象**：先分离运行时所有权、任务绑定、会话归属，再谈接口形状。
2. **全能力契约**：SPI 上每项能力对每个 provider 都必须可用；差异只在适配层用哪条**通道**兑现——API 优先、TUI 兜底、允许混合。能力缺口是适配层的质量责任，不是产品层的可选项。
3. **两个真实 provider 落地前不冻结契约**：v1 先行验证，v2 压测拓扑差异，TUI 压测非 API 通道的可行性。
4. **provider 身份显式化**：任务持久化稳定 provider ID + 激活时 binding；active task 按 immutable binding 路由。

## 2. Provider 身份模型（四个概念分开，勿混用）

| 概念 | 语义 | 例子 | 持久化 |
|---|---|---|---|
| `ProviderID` | 稳定产品族+协议代 | `opencode-v1` / `opencode-v2` / `codex` | tasks.provider |
| 契约版本 | 该 provider 已验证的 API 区间 | v1: 1.18.14–1.18.26；v2: 2.0.x | 代码常量+锚点 |
| 二进制 | 本机实际可执行文件与版本 | `~/.opencode2/bin/opencode @2.0.23` | 探测缓存 |
| `TaskBinding` | 某任务激活时产生的运行时绑定 | 见 §3.2 | tasks/task_bindings |

规则：**active task 只按 binding 路由**；provider 只允许 suspended 状态下切换，并显式定义 anchor/session 是否清空。二进制探测按需执行——某个可选 provider 缺失不得阻止其他 provider 启动（现 `config.go:464` 全局单例探测需拆除）。

## 3. 运行时所有权与任务绑定（P0 修复）

### 3.1 Runtime Supervisor

现有模型把"进程生命周期 = 任务生命周期"写死（`reconcile.go:193-340` 要求每个 active task 存在 `ocdeck-<task>-runtime` tmux 会话；`process.go:292-323` 只允许任务级命名）。v2 A+ 共享 server 与此根本不兼容，必须独立建模：

```go
type RuntimeSupervisor interface {
    Acquire(ctx, task TaskSpec) (TaskBinding, error) // 建/复用运行时 + 建立任务绑定
    Resume(ctx, task TaskRecord) (TaskBinding, error) // 重启/恢复后重建绑定
    Release(ctx, binding TaskBinding) error
}
```

两种实现：
- **v1：per-task supervisor**——每任务 spawn 独立 server，语义同现状。
- **v2：shared-server supervisor**——server 池按 **trust-domain key**（credential/config/security domain）分组；server 级状态机独立存在；ensure-once、引用计数、崩溃时所有绑定任务的 fencing/状态扇出/恢复顺序/恢复期间准入策略都要显式定义。process 层需新增 provider-runtime 命名空间（不能伪装成某任务的 runtime）。

### 3.2 TaskBinding（持久化）

由 `Acquire/Resume` 返回并落库，**不是**从 TaskSpec 推导：

```go
type TaskBinding struct {
    ProviderID         string
    ProviderInstanceID string // 稳定的后端数据域/运行池标识（不是会变的 PID）
    UpstreamSessionID  string
    TerminalRef        string // tmux 会话等（如有）
    Data               json.RawMessage // 版本化 provider 私有数据（baseURL、auth 引用、location 等）
}
```

替代现状的"DB 字段 + tmux 会话名/env 反查"双存储（`attention.go:918-928` 靠 `ShowSessionEnvContext` 反查 port/password 的模式随之消亡）。

### 3.3 SPI 能力接口（全部必需实现，通道自选）

按消费者拆窄接口（范式参考 `internal/infrastructure/ai/completer.go:25-53`）。**每项能力都是契约的一部分，provider 适配器必须全部实现**；"TUI 通道"列表示该能力在非 API agent 上的兑现方式：

| 能力接口 | 方法面 | API 通道兑现 | TUI 通道兑现 |
|---|---|---|---|
| `SessionControl` | 枚举/创建/删除/层级（含 ParentID） | `/api/session` 族 | 终端会话管理（单会话=任务，或刮取会话列表） |
| `EventSource` | 订阅 typed events | SSE 订阅+重连 | 输出流模式匹配 → 合成事件 |
| `AttentionIO` | 读 permission/question + 回复 | permission/question 端点 | 识别终端确认提示 + send-keys 回复 |
| `AsyncPrompt` | 异步注入 prompt | `prompt_async` | send-keys + 回车边界判定 |
| `StatusReader` | busy/idle/retry | `session.status` | 输出活动启发式（Spinner/静默超时） |
| `TerminalIO` | 输入注入/输出采集/attach | PTY 端点 | tmux 原生 |
| `TitleUpdater` | 标题同步 | title 端点 | 刮取或合成 |

适配器内部按能力选择通道、**允许混合**（例如 opencode：事件走 API，用户交互走 TUI attach；codex：全部走 TUI）。选择哪条通道是适配层实现细节，对 SPI 消费者透明。

**事件必须带类型**：`SessionCreated / SessionUpdated / SessionDeleted / StatusChanged / AttentionRaised / AttentionResolved / SessionError`。raw envelope 解析（v1 `{type,properties}`、v2 `/api/event`、TUI 输出模式）留在适配器内部——初版的 `AgentEvent{Kind, Props}` raw map 会让 task 层继续解析 provider payload（`activate.go:1454-1621` 现状），已否决。事件语义标注 **volatile**：断线不重放，消费者容忍重连缺口、重连后主动拉状态对齐。

**共享事件路由**（v2 特有）：一个 server 级 SSE reader 按 SessionKey 路由到任务；禁止每任务各自订阅全局事件再过滤（重复接收+全量对齐）。

## 4. Session 命名空间（P0 修复）

领域层现假设上游 `session_id` 全局唯一（`internal/domain/session/session.go:6-29`），Claim/OwnerOf 按裸 ID 判归属（`queries.go:1609-1669`）。多 provider / 多 server 下 ID 必然碰撞：

```go
type SessionKey struct {
    ProviderID         string
    ProviderInstanceID string
    UpstreamID         string
}
```

- `task_sessions`、父子层级（`ParentID` 是现有不变量，`0004_task_sessions_parent_id.sql`，DTO 必须保留）、`anchor_session_id`、OwnerOf/Claim API 全部迁移到 SessionKey；父子必须同命名空间。
- 存量 v1 数据确定性回填（ProviderID=`opencode-v1`，InstanceID 取稳定数据域标识）。
- v2 的 `location.directory` **只能用于校验/辅助路由，不能作为绑定身份**（local-path 任务允许共享目录，`session.go:214-223`）。

## 5. 能力实现矩阵与运行时健康状态

**能力 = 契约（全量必需）；通道 = 实现（适配层自选）。** 每个 provider 落地时必须产出一张实现矩阵，由 spike 填实：

| 能力 \ Provider | opencode v1 | opencode v2 | codex | pi |
|---|---|---|---|---|
| SessionControl | API | API | TUI | TUI |
| EventSource | API (SSE) | API (SSE, volatile) | TUI 模式匹配 | TUI 模式匹配 |
| AttentionIO | API | API（v2 事件家族） | TUI 识别+按键回复 | TUI |
| AsyncPrompt | API | API | TUI send-keys | TUI |
| StatusReader | API | API | TUI 启发式 | TUI |
| TerminalIO | API/TUI 混合 | API/TUI 混合 | TUI 原生 | TUI 原生 |
| TitleUpdater | API | API | TUI/合成 | TUI/合成 |

（矩阵中 TUI 兑现行为待 codex/pi spike 确认后填实。）

**运行时健康状态**（保留评审要求的动态模型）：能力虽为契约必需，但运行时可故障/降级（prompt_async 三态 `prompt_async.go:125-207`；attention 四态 `attention.go:32-41`；title 按 runtime 实例缓存 `title_sync.go:28-136`）：

```go
type CapabilityHealth struct {
    Primitive Primitive       // 能力原语
    State     CapState        // unknown | available | unsupported | degraded
    Scope     CapScope        // provider | server-instance | runtime-epoch | session
    Reason    string          // 故障/降级原因（透出给用户与运维）
    Epoch     int64           // runtime 实例代际（重启即失效重探）
}
```

`unsupported`/`degraded` 在此表示**适配层故障或上游变更导致的违约**，需要告警与修复——而不是"该 provider 天生没有此能力"。

注意：
- `DiffReview` 是多项原语组合的**派生产品能力**，不是 SPI 原生能力，从能力表移除。
- 模型切换、快照/回滚、成本统计暂无明确消费流程，**推迟**（YAGNI）。

**TUI 兑现的固有脆弱性与对策**：终端输出格式是上游的非正式契约，会因版本升级漂移。对策与 HTTP 契约治理同构——为每个 TUI provider 建立**输出模式锚点表 + 契约测试**（复用 `check-opencode-contract.sh` 的 SOP 思路：版本钉住、模式命中探测、失败告警）。

## 6. 两条通道的适配要点

### 6.1 API 通道

共享骨架（健康检查、auth、SSE 重连）**暂不预抽象**——等 v1/v2 出现真实重复再抽取（YAGNI）。

| | opencode v1 | opencode v2 |
|---|---|---|
| 拓扑 | per-task supervisor | shared-server supervisor（trust-domain 池） |
| argv | `opencode --port <p> --hostname 127.0.0.1 [--session id] [--auto]` | `opencode serve --port <p> --hostname 127.0.0.1` |
| API | 裸路径 | `/api` 前缀、`/api/info`、`/openapi.json` |
| 认证 | `OPENCODE_SERVER_PASSWORD`（可空） | 强制 Basic auth |
| env | 进程 env 注入 | 进程 env + per-session `PUT environment`（**不隔离** MCP/LSP/credential——trust-domain 划池的原因） |
| 隔离 | 现状 | XDG 三件套（与 v1 共存必做，防共享 `opencode.db` 致 v1 启动 die） |

A+ 安全前提：**同池任务必须同信任域**；session 创建 + PUT environment 需事务补偿与失败清理。

### 6.2 TUI 通道（与 API 平级的一等兑现通道）

TUI 通道承担双重角色：既是部分能力的**实现通道**（对无 API 的 agent 兑现全部 SPI 能力），也是用户交互的**终端载体**（TerminalIO）。适配层内部以 toolkit 形式提供两类原语：

- **API toolkit**：HTTP client、auth、SSE 订阅+重连、健康检查（v1/v2 共享，等真实重复出现后再抽公共骨架——YAGNI）。
- **TUI toolkit**：tmux 托管 + send-keys + capture-pane/pipe-pane + 输出模式匹配 + prompt 边界判定。现有 process 层只有创建/env 读取/退出监视/PTY attach（`process.go:488-550`、`:908-1052`），**缺** send-keys、capture-pane、pipe-pane、prompt 边界判定——这些原语要先补齐。

每个 provider 的适配器从两个 toolkit 中逐能力选择兑现通道（见 §5 实现矩阵），允许混合：例如 opencode 的事件走 API、用户 attach 走 TUI；codex 全部走 TUI。

**codex/pi spike 是契约冻结前的硬门槛**：验证输入转义、输出边界、完成判定、进程重连、交互确认、背压——即"TUI 通道能否兑现全部 SPI 能力"的可行性证据。spike 若发现某能力无法经 TUI 可靠兑现（如结构化 diff 数据），回到 SPI 层重新讨论该能力的契约形态，而不是悄悄降级。

## 7. 迁移面（比初版估计的大）

初版称"taskOcClient 是唯一汇聚点，改动面集中"——**错误**。直接调用点包括：`activate.go:629,1155,1295,1710`、`reconcile.go:387,509`、`suspend.go:342`、`delete.go:541`、`attention.go:911-935`、`permit_auto.go:362`、`agent_status.go:44`。且 v1-specific 行为覆盖启动、端口重试、双启动 anchor bootstrap、SSE 对齐、reconcile、suspend/delete/shutdown（`activate.go:1067-1782`）。另需移除 `internal/application/dto.go:141-162` 对 `opencode.PermissionRequest/QuestionRequest` 的直接嵌入。

## 8. 落地顺序（修订版）

```
0. spike：opencode v2 实机契约验证 + codex/pi 终端行为摸底（契约冻结硬门槛）
   同时为 v1 建立 characterization tests（启动/重连/恢复/挂起/删除）
1. 提取 v1 task driver + typed events + TaskBinding（拓扑不变，测试保住）
2. provider/binding/SessionKey 命名空间 migration；迁完 §7 全部直接调用点
3. v2 shared-server supervisor + server 级 SSE 路由（XDG 隔离 + trust-domain 池）
4. TUI TerminalIO 原语 + codex driver（压测能力不对称）
5. pi 及后续 provider
```

检验标准：第 3 步完成时 SPI 未被迫改形状 = 抽象成立；第 4 步暴露的应是能力覆盖问题而非契约缺陷。

## 9. 风险清单

1. 官方文档滞后：v2 一切以 2.0.23 实机 + `/openapi.json` 为准。
2. SSE volatile：attention/status 链路必须容忍断线缺口。
3. A+ 共享状态：saved approvals / credential / bus / jobs 全局——trust-domain 划池是硬约束不是可选项。
4. v2 单 server 多 location 内存累积，需观察，必要时按池拆分。
5. 用户配置含 v2 `permissions` 键会让 v1 provider 启动抛错（已核实）——Web 配置编辑侧需按 provider 版本校验提示。
6. codex/pi 终端适配可行性未经 spike 确认。

## 10. YAGNI（本期砍掉/推迟）

- 通用 `Endpoint` union、raw `Props` 事件信封、provider"契约区间"抽象、预抽象的共享 HTTP 骨架。
- 模型切换、快照/回滚、成本统计能力位。
- 不能推迟的：session 层级（ParentID）、SessionKey 命名空间、动态能力状态、TaskBinding、共享运行时所有权、TUI I/O 原语。
