# Proposal: init-ocdeck-mvp

## Why

emdash 证明了"多 agent 并行 + worktree 隔离 + 统一控制台"的产品价值，但它是 Electron 桌面应用、为兼容 20+ 种 CLI 付出 PTY 抓屏的复杂度代价。用户只需要服务 OpenCode 一个 agent，且希望浏览器任意位置访问（而非桌面应用）。OpenCode 原生提供 `serve`（HTTP API + SSE）与 `attach`（TUI 作为 HTTP 客户端）架构，使得"服务端 + Web + 原生 TUI 体验"的编排台可以用远小于 emdash 的复杂度实现。

## What Changes

新建 ex-ocdeck：个人自用的 OpenCode 并行任务编排台（绿field，Go + SQLite + Web）。

- **项目管理**：注册本地 git 仓库为项目
- **任务管理**：任务 = 独立 git worktree + 分支，同项目可并行多任务；状态机 活跃⇄挂起→归档→删除
- **OpenCode 编排**：每个活跃任务运行独立 `opencode serve` 与 tmux 会话内 `opencode attach`（TUI），服务端经专属 tmux socket（`tmux -L ocdeck`）托管会话生命周期；关停策略可配置（persist / kill_on_start / kill_immediate）
- **浏览器终端**：xterm.js 经 WebSocket 桥接 tmux attach 客户端直连任务 tmux 会话。每任务两类终端：opencode TUI 终端（原生体验，断开不杀任务进程，重连经 tmux reattach 恢复当前屏幕）与普通 shell 终端（可多个，cwd=worktree，注入任务 env，用于 dev server / 测试 / 日志等命令行操作）
- **Git 操作**：Web 端 status / diff 查看 / commit / push（不含 PR 创建）
- **环境变量**：项目级 + 任务级两层 key-value，任务级覆盖项目级，进程启动时注入
- **全局配置管理**：`~/.config/opencode/` 下 JSON/JSONC 配置文件（opencode.json、opencode.jsonc、omo-slim.json、dcp.json 等）的列表与编辑（按扩展名分流语法校验）
- **访问控制**：单 token 认证

明确非目标：skill 管理、定时任务、SSH 远程、issue tracker 连接器、PR 创建、多用户/复杂认证、移动端适配、多 CLI 兼容。

## Impact

- **Affected specs**：全部新增（绿field）：`project-management`、`task-lifecycle`、`opencode-orchestration`、`terminal-streaming`（含 shell 终端）、`git-operations`、`env-management`、`global-config-management`、`access-control`
- **Affected code**：全新仓库 `/Users/mendlex/workplace/private/ex-ocwork`（Go 服务端 + web 前端）
- **外部依赖**：本机 `opencode` CLI（serve/attach 模式）、git CLI、xterm.js
- **运维形态**：单 Go 进程 + 嵌入式 SQLite，localhost 起步，用户自管反代
