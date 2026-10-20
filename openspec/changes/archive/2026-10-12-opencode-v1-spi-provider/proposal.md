# Proposal: opencode v1 作为 provider 接入标准 SPI

## Why

ocdeck 计划将 opencode 升级到 v2 并适配更多 agent（codex、pi 等）。当前 opencode v1 的集成是硬编码的单体形态：CLI 启动、HTTP/SSE 契约、tmux 进程托管、session 归属判定全部绑死在 v1 假设上，直接升级 v2 需要在一次变更中同时处理"架构抽象 + 契约迁移"两类风险。本 change 先做升级前准备：在不改变任何对外行为的前提下，定义标准 Agent SPI 契约，并让 opencode v1 作为首个 provider 迁入该契约，为后续 provider 接入建立统一契约边界，并通过 v1 重构验证该抽象（面向后续 provider 的契约冻结不在本期承诺内）。

## What Changes

- 定义标准 Agent SPI 契约（provider-neutral）：domain DTO、typed events、能力接口集合（全能力契约，适配层自选 API/TUI 通道兑现）、运行时托管与任务绑定生命周期契约（RuntimeSupervisor + TaskBinding）、SessionKey 会话命名空间。契约形态以 `docs/design/multi-agent-spi.md` v3（已经过架构评审与全能力模型对齐）中的**本期 SPI 契约形态**为准；该文档中的跨 provider 验证及 v2 / codex / pi 后续接入安排不属于本 change。
- 将 opencode v1 的现有集成重构为该 SPI 的首个 provider 实现：现有 task 层各消费点（激活、attention、permit_auto、agent_status、diffreview 等）从直接依赖 `opencode.*` 具体类型，迁移为经 SPI 契约与 provider registry 分派；**对外行为完全不变**。
- 持久化迁移：任务记录新增 provider 身份与 TaskBinding；session 归属判定从裸上游 session ID 迁移到带 provider 命名空间的 SessionKey；存量数据确定性回填。
- 重构前置保护网：为 v1 的启动、SSE 重连、恢复（reconcile）、挂起、删除等关键路径补齐 characterization tests，先建网再重构。
- 非目标（本 change 不做）：接入 opencode v2 provider；接入 codex / pi；A+ 共享 server 拓扑；TUI 通道 toolkit；任何对外行为变更。

## Capabilities

### New Capabilities

- `agent-provider-spi`: 标准 Agent SPI 契约——provider 身份模型与 registry 分派、运行时托管与任务绑定生命周期契约（RuntimeSupervisor + TaskBinding）、SessionKey 会话归属命名空间、typed events 契约、SPI 能力接口集合及其运行时健康状态透出。本期由 opencode v1 兑现，既有托管行为保持不变。

### Modified Capabilities

- `opencode-orchestration`: opencode v1 的运行时托管从"硬编码直连"迁移为"经标准 SPI 的 provider 实现"——runtime 绑定的事实来源从 tmux 会话 env 反查迁移为持久化 TaskBinding，session 归属判定迁入 SessionKey 命名空间；端口分配、健康检查、cleanup notice、SSE 归属捕获等既有行为契约保持不变。

## Impact

- **代码**：`internal/task/`（types/manager/activate/attention/permit_auto/agent_status/reconcile/suspend/delete/diffreview 各消费点）、`internal/infrastructure/opencode/`（v1 契约客户端迁入 SPI 适配器形态）、`internal/application/`（DTO 去除 `opencode.*` 类型依赖）、新增 SPI 契约包与 provider registry。
- **持久化**：tasks 表新增 provider/binding 相关列；`task_sessions` 及归属查询（Claim/OwnerOf）迁移到 SessionKey 命名空间；存量数据迁移回填。
- **测试**：新增 v1 关键路径 characterization tests；全部既有测试在重构后保持通过。
- **API / 对外行为**：既有端点、字段与行为不变（行为保持是本 change 的核心约束）；唯一例外为服务状态端点新增 `capabilities` 只读增量字段。
- **文档**：`docs/design/multi-agent-spi.md` 为契约形态依据。
