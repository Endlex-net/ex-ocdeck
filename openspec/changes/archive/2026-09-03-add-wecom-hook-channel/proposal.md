## Why

ocdeck 已有任务通知能力（web / Bark / macOS），但缺少企业微信群机器人 webhook，团队无法把「需要人工介入」的信号投到企微群。各家 webhook 协议互不兼容，不能用通用 HTTP POST 覆盖企微，因此需要一条专用渠道。

## What Changes

- 在现有通知渠道体系中新增一条企业微信（wecom）渠道：用户粘贴完整 webhook URL，启用后可与现有渠道共同启用。
- 消息形态为 markdown，正文含标题、详情与任务跳转链接；不新增 @ 提及能力。
- 设置页、配置读写 API、测试通知纳入该渠道（开关 + webhook URL）。完整 webhook URL 整体按密钥保护，配置回显与日志均不得暴露明文。
- 旧配置文件缺少该渠道键时按默认关闭处理，不得因此整份配置失效。

**Non-goals**

- 不做通用 webhook，也不做钉钉 / 飞书 / Slack。
- 不做 @all / userid、news / template_card / markdown_v2、多群 URL 列表。
- 不新增厂商 host 白名单或限流排队能力。
- 不改五类触发器、门禁、LLM 总结与 web / Bark / macOS 既有行为。

## Capabilities

### New Capabilities

- （无）本变更扩展已有任务通知能力，不引入独立 capability。

### Modified Capabilities

- `task-notifications`: 渠道集合从三渠道扩展为四渠道，增加企业微信 webhook 的配置、投递与设置界面。

## Impact

- 通知配置模型、磁盘 schema、配置 API DTO 与前端设置页增加 wecom 渠道字段。
- 通知渠道范围增加企业微信 webhook；不改事件源与现有三渠道行为。
- 无新外部依赖。
- 测试覆盖渠道注册、配置 schema、投递与 API/前端契约。
