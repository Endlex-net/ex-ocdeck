# Global Config Management Specification

## Purpose
对 `~/.config/opencode/` 下的 JSON/JSONC 配置文件提供列表、读取、编辑与保存，按扩展名分流语法校验并以原子写入与乐观并发保护配置安全。

## Requirements

### Requirement: 配置文件列表
系统 SHALL 列出 opencode 全局配置目录（`~/.config/opencode/`）下的 JSON 与 JSONC 配置文件（如 opencode.json、opencode.jsonc、omo-slim.json、dcp.json）。该功能 MUST 作为设置页（`#/configs`）的 opencode 配置子标签呈现（深链 `#/configs#opencode`），MUST NOT 保留独立配置页面。

#### Scenario: 查看配置列表
- **WHEN** 用户打开设置页 opencode 配置子标签
- **THEN** 系统展示该目录下全部 *.json 与 *.jsonc 配置文件

#### Scenario: 深链直达子标签

- **WHEN** 用户打开 `#/configs#opencode`
- **THEN** 设置页打开并直接选中 opencode 配置子标签

### Requirement: 配置文件读取与编辑
系统 SHALL 提供配置文件内容的读取与保存，编辑器为 JSON 文本编辑。

#### Scenario: 读取配置
- **WHEN** 用户选择某配置文件
- **THEN** 系统返回文件全文

### Requirement: 保存前语法校验
系统 SHALL 在保存前按扩展名分流校验：`.json` 做严格 JSON 语法校验；`.jsonc` 用 JSONC 解析器校验（允许注释与尾逗号）。语法非法时 MUST 拒绝保存并指出错误位置。系统 MUST NOT 做业务 schema 校验（配置语义由 opencode 自己负责）。编辑器 MUST 保留原文（含注释原样保存）。

#### Scenario: 保存合法 JSON
- **WHEN** 用户保存语法合法的 JSON
- **THEN** 文件被写入

#### Scenario: 拒绝非法 JSON
- **WHEN** 用户保存语法非法的内容
- **THEN** 保存被拒绝，原文件不变，错误信息展示给用户

### Requirement: 保存前备份与原子写入
系统 SHALL 在覆盖写入前将原文件备份为 `<name>.bak`（保留最近一次）；写入 MUST 采用临时文件 + 原子 rename 并保留原文件权限；MUST 拒绝路径逃逸与不受控 symlink。

#### Scenario: 自动备份
- **WHEN** 一次保存成功
- **THEN** 同目录下存在保存前内容的 .bak 文件

### Requirement: 乐观并发校验
系统 SHALL 在读取配置时记录文件 mtime 与内容 hash；保存时 MUST 比对，若文件已被外部修改则返回冲突，由用户选择覆盖或重新加载。

#### Scenario: 外部并发修改
- **WHEN** 用户编辑期间文件被其他程序修改，随后用户保存
- **THEN** 系统返回冲突提示，不静默覆盖

### Requirement: 生效时机提示
系统 SHALL 在保存成功后列出受影响的活跃任务，提示需重启任务（挂起后激活）生效；MUST NOT 自动重启任何任务进程。

#### Scenario: 保存后提示
- **WHEN** 用户保存全局配置
- **THEN** 界面列出受影响活跃任务并提示需手动重启生效