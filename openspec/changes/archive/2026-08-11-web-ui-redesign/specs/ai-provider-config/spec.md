## MODIFIED Requirements

### Requirement: AI 配置页

前端 SHALL 提供 AI 配置界面，作为设置页（`#/configs`）的 AI 子标签呈现（深链 `#/configs#ai`），MUST NOT 保留独立页面；旧路由 `#/ai-config` MUST 重定向至 `#/configs#ai`。界面包含：`provider` 下拉（openai/anthropic）、`api_key` 密码输入框（展示掩码值，可覆盖输入）、`base_url`、`model` 输入框、`thinking` 下拉（默认/关闭/低/中/高，附高超度延迟提示）与保存按钮。保存成功/失败 MUST 有明确反馈；后端校验失败（422）时 MUST 展示错误原因；GET 返回 `load_error` 时 MUST 在界面展示。

#### Scenario: 配置 AI provider

- **WHEN** 用户在设置页 AI 子标签选择 provider、填写 api_key 与 model 并保存
- **THEN** 界面提示保存成功，配置立即生效

#### Scenario: 展示已保存配置

- **WHEN** 用户打开设置页 AI 子标签且配置已存在
- **THEN** 界面展示 provider/base_url/model 与掩码后的 api_key

#### Scenario: 展示配置加载错误

- **WHEN** 配置文件损坏，用户打开设置页 AI 子标签
- **THEN** 界面展示 load_error 提示，用户可直接重新保存合法配置修复

#### Scenario: 旧路由重定向

- **WHEN** 用户打开 `#/ai-config`
- **THEN** 应用重定向至 `#/configs#ai` 并选中 AI 子标签
