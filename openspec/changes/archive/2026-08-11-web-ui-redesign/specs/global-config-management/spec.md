## MODIFIED Requirements

### Requirement: 配置文件列表

系统 SHALL 列出 opencode 全局配置目录（`~/.config/opencode/`）下的 JSON 与 JSONC 配置文件（如 opencode.json、opencode.jsonc、omo-slim.json、dcp.json）。该功能 MUST 作为设置页（`#/configs`）的 opencode 配置子标签呈现（深链 `#/configs#opencode`），MUST NOT 保留独立配置页面。

#### Scenario: 查看配置列表

- **WHEN** 用户打开设置页 opencode 配置子标签
- **THEN** 系统展示该目录下全部 *.json 与 *.jsonc 配置文件

#### Scenario: 深链直达子标签

- **WHEN** 用户打开 `#/configs#opencode`
- **THEN** 设置页打开并直接选中 opencode 配置子标签
