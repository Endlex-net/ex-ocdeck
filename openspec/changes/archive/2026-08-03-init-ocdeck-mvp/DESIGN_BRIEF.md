# ex-ocdeck 设计简案（供架构讨论）

> ⚠️ **历史文档，非规范（superseded）**：本简案是 design.md 的草稿来源，其中 `--continue` 恢复、WS query token 等早期方案已被 design.md 取代。规范以 design.md 与 specs/ 为准，本文档仅供追溯。

> 目的：在写正式 OpenSpec design.md 之前，就关键架构决策征求评审意见。

## 0. 产品概述

ex-ocdeck：个人自用的 OpenCode 并行任务编排台。Go 服务端 + SQLite + Web 界面。
跑在开发机上，浏览器任意位置访问（localhost 起步，用户自己处理反代）。

- 参考对象：emdash（Electron 桌面端、多 CLI 兼容、PTY 抓屏路线）
- 差异：服务端+Web 架构、只服务 opencode、对话界面是浏览器终端里的**原生 opencode TUI**

## 1. 已锁定的需求

- F1 项目管理：注册本地 git repo；F2 任务管理：任务=独立 worktree+分支，可并行
- F3 oc 编排：每任务独立 `opencode serve` + PTY `opencode attach`
- F4 浏览器终端：xterm.js ↔ WebSocket ↔ PTY，原生 TUI，断线重连
- F5 git 操作：status/diff/commit/push（不做 PR）
- F6 env：项目级+任务级两层注入，任务级覆盖项目级
- F7 全局配置管理：~/.config/opencode/ 下 JSON 文件列表+编辑（语法校验）
- F8 访问控制：简单 token
- 任务状态机：活跃(进程常驻)⇄挂起(进程停,worktree留,恢复时--continue接会话)→归档(同挂起但收起)→删除(删worktree+本地分支+oc session数据，远端分支保留)
- 服务端挂=所有 oc 进程挂；重启后全部呈挂起，手动恢复；不设并发上限
- 任务级 shell 终端：每任务可开多个普通 shell 终端（$SHELL，cwd=worktree，注入任务 env），与 TUI 终端并列，复用同一 PTY+WS 通道；随任务挂起终止（补充于文档阶段）

## 2. 已调研的关键事实（可采信）

1. `opencode serve --port N` 起 HTTP API + SSE；serve 进程不绑定目录，请求用 `?directory=` / `x-opencode-directory` 头限定
2. `opencode attach http://host:port` 让 TUI 作为 HTTP 客户端连到已运行的 serve；**TUI 以其 cwd 作为 directory**
3. TUI 与 serve 共享 session 存储（`~/.local/share/opencode/`，全局 DB）
4. serve 支持 Basic Auth（`OPENCODE_SERVER_PASSWORD`，启动时读取，不热更）
5. opencode 集成参数：`OPENCODE_PERMISSION='{"*":"allow"}'` 自动放行；`--session <id>`/`--continue` 恢复；`--prompt`/`--model`
6. serve API：`GET /session/status`、`GET /session/:id/diff`、`DELETE /session/:id`、`GET /event`(SSE)
7. emdash 参考：PTY 64KB 环形缓冲 + 16ms 批量刷新；worktree 池 `<repo>/.emdash/worktrees/`；git 操作串行化队列；确定性端口 hash 50000-59990；进程组 SIGTERM→wait→SIGKILL；删除前路径包含性校验

## 3. 提议架构

```
浏览器 (React+xterm.js)
   │  HTTP REST (管理API, token 认证)   │  WebSocket (终端IO, token)
   ▼                                    ▼
┌────────────────── Go 服务端 (单进程) ──────────────────┐
│ API 层: 项目/任务/env/配置/git 管理                      │
│ 编排层: TaskLifecycle (状态机) + ProcessManager          │
│ PTY 层: 每任务 PTY + 64KB 环形缓冲 + WS 桥接             │
│ Worktree 层: git worktree 增删 (串行队列+包含性校验)      │
│ 存储层: SQLite (projects/tasks/env_vars)                │
└───────┬──────────────────────┬─────────────────────────┘
        │ spawn (env 注入)      │ spawn PTY
        ▼                      ▼
  opencode serve --port N   opencode attach http://127.0.0.1:N
  (cwd=worktree)            (cwd=worktree → TUI directory=worktree)
```

每个任务 = worktree + serve 进程 + PTY(attach) 进程。Go 服务端通过 serve REST API 取结构化状态（/session/status 等），终端 IO 走 PTY。

## 4. 设计问题（请逐条评审）

**Q1 进程模型**：每任务独立 serve+attach，而非单个全局 serve 多 directory 复用。理由：env 注入在 serve 进程启动时固定，全局单 serve 无法按任务注入不同 env；且端口级隔离让崩溃影响面小。代价：N 任务=N×2 进程。个人规模可接受。是否同意？

**Q2 端口分配**：DB 记录每任务端口，激活时从 50000+ 顺序找空闲端口（listen 探测），而非 emdash 的 hash 确定性方案。理由：hash 方案冲突后仍需回退探测，直接探测更简单可靠。是否同意？

**Q3 PTY 层**：Go 用 creack/pty；每 PTY 配 64KB 环形缓冲（浏览器重连时先回放缓冲再接 live 流）；进程退出用进程组 kill（syscall.Kill(-pgid)）；resize 通过 WS 控制帧。是否有坑？

**Q4 WS 协议**：单 WS 连接/终端；二进制帧传 PTY 输出；JSON 控制帧做 resize/input/auth。token 在 WS 升级请求 query 参数带。重连=新 WS + 缓冲回放。是否合理？

**Q5 SQLite 访问**：modernc.org/sqlite（纯 Go 无 cgo，交叉编译/部署简单）+ sqlc（类型安全代码生成）或手写 sqlx。倾向 modernc+sqlc。是否有反对意见？

**Q6 前端**：React+Vite+TS+xterm.js；diff 展示用 Monaco diff editor（emdash 同款）还是轻量 unified diff 渲染（如 diff2html）？v1 倾向 diff2html 减体量。是否同意？

**Q7 仓库布局**：标准 Go 布局 cmd/ocdeck-server + internal/{api,task,process,pty,worktree,store,config}，前端 web/。不做 DDD 分层（个人项目规模，洋葱架构是过度设计）。是否同意？

**Q8 git 操作实现**：直接 exec git CLI（status --porcelain=v2 / diff / commit / push），不引入 go-git。理由：行为与用户本机 git 完全一致（含 hooks、签名、ssh agent）。是否同意？

## 5. 我已识别的风险

- R1 serve API 版本漂移：opencode 快速迭代，/session/status 等端口语义可能变。缓解：封装 occlient 包，启动时 GET /global/health 记录版本
- R2 删除任务的 oc session 数据：需经 serve `DELETE /session/:id`，但若进程已死（如服务端崩溃后删除任务），需临时起 serve 或直接操作 opencode DB（不推荐）。倾向：删除时若有活进程走 API，否则跳过并提示残留
- R3 attach TUI 的 directory 取自 cwd——PTY spawn 的 cwd 必须是 worktree，否则 TUI 连错项目
- R4 全局配置文件编辑与正在运行的 oc 进程的配置冲突（omo-slim.json 改动需重启任务进程生效）——UI 提示即可
