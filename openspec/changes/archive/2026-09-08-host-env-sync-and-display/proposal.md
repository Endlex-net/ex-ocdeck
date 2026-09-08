# Proposal: host-env-sync-and-display

## Why

当 ocdeck-server 由服务管理器启动（Ubuntu 的 systemd user service、macOS 的 brew services/launchd）时，进程环境可能缺少用户在 shell 中配置的环境变量；当前 follow_host 仅从服务端进程环境取值，缺失的变量会被跳过，导致 opencode 会话无法获得这些变量。同时用户无法在产品内看到"服务端实际读到了哪些宿主环境变量"，排障只能靠猜。

## What Changes

- follow_host 解析增加 login shell 环境兜底：服务端进程环境查不到时，从用户 login shell 捕获的环境中解析，使 systemd 启动场景下跟随宿主可用。
- 设置页「环境变量」tab 在全局环境变量列表下方新增「系统环境变量」展示区：合并展示服务端进程环境与用户 login shell 捕获的环境变量并标注来源，值默认掩码、点击显示，支持搜索过滤与一键添加为 follow_host 全局变量。
- 系统环境变量支持手动刷新：用户修改 shell 配置后无需重启 server 即可重新捕获。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `env-management`: follow_host 的宿主解析语义扩展（进程环境未命中时兜底 login shell 捕获环境）；新增宿主环境变量列举与手动刷新能力；新增系统环境变量展示区的 UI 行为要求。

## Impact

- 后端：扩展宿主环境解析能力，提供宿主环境变量列举与手动刷新的 API。
- 前端：设置页环境变量 tab 新增系统环境变量展示区（掩码展示、搜索、一键 follow_host、刷新）。
- 文档：README env 配置章节。
- 兼容性：既有 follow_host 在进程环境命中时行为不变；手动模式、项目级/任务级 env、快照生效时机语义不变。

## 非目标（Non-goals）

- 不改变 env file（~/.config/ocdeck/env）的既有语义。
- 不改变任务 env 快照的生效时机（仍为挂起后激活）。
- 不向任务进程注入除既有层叠规则外的任何额外变量。
- 不支持 Windows。
