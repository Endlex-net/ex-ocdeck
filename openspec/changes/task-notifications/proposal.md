# Proposal: task-notifications

## Why

当前通知依赖 opencode 用户级插件 ex-notify：自动触发只能覆盖 permission/question 两个点，其余场景（blocked、critical failure 等）由 ex-workflow prompt 约束 LLM 主动调用工具，触发不稳定、会漏。ocdeck 作为任务编排台已经拥有确定性的任务状态信号（attention、run_status），把通知能力内建到 ocdeck 可以把"LLM 主动调工具"换成"Go 进程内事件驱动"，通知不再依赖 LLM 的自觉。

## What Changes

- 新增 ocdeck 内建通知能力：当任务进入需要用户关注的状态时，自动向用户发送通知，覆盖：
  - agent 等待用户回答问题（question）
  - agent 等待权限批准（permission）
  - 任务空闲超过可配置时长（默认 1 分钟）且未继续工作（agent 停了但没在等人）
  - 任务处于重试状态持续 1 分钟未恢复
  - agent 运行出错（含不可重试错误）持续 1 分钟未恢复
- 通知渠道可配置多选：网页通知（ocdeck 前端）、Bark 手机推送、macOS 本地通知。macOS 渠道向运行 ocdeck-server 的 macOS 主机投递本地通知（仅 darwin 运行环境）。
- 通知基于统一的渠道无关意图抽象：通知方只表达意图（任务、类别、级别、内容、跳转链接），各渠道声明自身能力位（分组 Group / 同键替换 Replace / 撤回 Withdraw），缺失能力自动降级（如标题加任务前缀），不阻塞其他渠道投递。
- 通知内容包含任务名称与通知类别；错误类通知携带错误详情；通知支持点击跳转到 ocdeck 对应任务页。
- 可选增强：由 LLM 总结任务停止原因附加到通知内容，失败时降级为确定性摘要（默认路径不依赖 LLM）。
- 通知配置遵循 ai-provider-config 先例（应用级配置文件 + 读写 API + 设置页子标签 + 保存即生效）：通知总开关；每个通知类别（question / permission / idle / retry / error）独立的用户开关；每个渠道（网页 / Bark / macOS）独立的用户开关与渠道参数；空闲阈值（重试与错误的 1 分钟持续时长为固定语义，不开放配置）。
- **BREAKING（对用户环境）**：ocdeck 内建通知替代 ex-notify 插件；插件文件（~/.config/opencode/plugin/ex-notify.ts）的删除由用户手动完成——本变更 MUST NOT 修改用户插件目录中的任何文件。插件移除后，裸 opencode TUI 场景不再提供通知。

## Capabilities

### New Capabilities

- `task-notifications`: ocdeck 的任务状态通知能力——基于任务运行信号（attention / run_status / session.error）确定性地触发通知，经可配置渠道（网页 / Bark / macOS）投递，携带任务名、类别、详情与任务页跳转链接。包含通知配置的存储、读写 API 与设置页界面：通知总开关；每个通知类别（question / permission / idle / retry / error）独立的用户开关；每个渠道（网页 / Bark / macOS）独立的用户开关与渠道参数；空闲阈值（重试与错误的 1 分钟持续时长为固定语义，不开放配置）；配置保存后立即生效（遵循 ai-provider-config 的应用配置先例）。

### Modified Capabilities

无。（通知配置由 task-notifications 自行承载，不修改 global-config-management——该 capability 的语义是编辑 `~/.config/opencode/` 下 opencode 自身配置文件，与 ocdeck 应用配置无关。）

## Impact

- **代码**：新增通知领域与应用模块（触发判定、内容组装、渠道抽象与降级、去抖/抑制）；新增 session.error 事件消费；全局配置模型扩展；前端新增通知权限申请与网页通知展示。
- **外部契约**：Bark server HTTP API（POST /push，官方 api.day.app 或自建 endpoint）；浏览器 Notification API；macOS 命令行通知工具。
- **配置**：新增通知相关全局配置项；Bark token 等敏感配置不再硬编码（现状在 ex-notify 源码中）。
- **用户环境**：ex-notify 插件由用户手动删除（本变更不触碰该目录）；裸 opencode（不经 ocdeck）会话失去通知。

## 范围边界与非目标

- 非目标：通知撤回/更新已发通知（如 retry 恢复后撤销告警）——渠道抽象预留能力位，本期不实现。
- 非目标：workflow 语义级通知（blocked、critical failure 等只有工作流内部知道的状态）——ocdeck 事件模型无对应信号，由 idle/error 等运行态信号间接覆盖。
- 非目标：通知历史记录与持久化、多用户/多设备路由。
- 非目标：ex-notify 插件的迁移或改造——直接下掉，不在本变更内处理插件仓库。
