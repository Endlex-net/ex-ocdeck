# Design: init-ocdeck-mvp

> 本设计已经过架构评审（见 DESIGN_BRIEF.md 及评审结论），下文为定稿。

## 1. 总体架构：持久控制面 + 临时任务运行时

```
浏览器 (React + xterm.js)
   │  HTTPS/HTTP REST (Bearer token)      │  WebSocket (首消息认证)
   ▼                                       ▼
┌──────────────── ocdeck-server (Go 单二进制, 内嵌 web/dist) ───────────────┐
│ api        REST + WS 端点, token 中间件                                   │
│ task       TaskManager：任务状态转换/进程/worktree 操作的唯一入口           │
│ process    tmux 会话后端：serve / tui / shell 会话生命周期 + reaper + watchdog │
│ pty        attach 客户端 PTY 池 + WS 桥 + resize                           │
│ worktree   worktree 增删（每 repo 写锁）                                  │
│ git        git CLI 封装（argv 白名单）                                    │
│ opencode   occlient：serve REST/SSE 封装 + 版本/能力探测                   │
│ store      SQLite (modernc.org/sqlite + sqlc)，持久控制面状态             │
│ config     服务端配置 + 全局 oc 配置文件管理                              │
└───────┬──────────────────┬──────────────────┬──────────────────────────┘
        │ tmux new-session -d  │ tmux new-session -d        │ tmux new-session -d
        ▼                      ▼                            ▼
   ocdeck-<taskID>-serve   ocdeck-<taskID>-tui        ocdeck-<taskID>-shell-<n>
   opencode serve          opencode attach            $SHELL
   127.0.0.1:<port>        --session <id>             (普通终端)
   (cwd=worktree)          (cwd=worktree)             (cwd=worktree)
        ▲                       ▲                            ▲
        └──── 专属 tmux server（tmux -L ocdeck，与用户自有 tmux 隔离）────┘
                          ▲
   浏览器 WS ↔ PTY（PTY 内跑 tmux -L ocdeck attach -t <session>，仅渲染客户端）

   可选 watchdog（仅 shutdownPolicy=kill_immediate）：单个 ocdeck-server watchdog
   子进程，父亡即 tmux -L ocdeck kill-server
```

核心原则：
- **SQLite 只存控制面事实**（项目/任务/env/session 归属/last_error），PID 与端口不是可信运行事实；**tmux server 是运行时事实注册表**（`tmux -L ocdeck ls` 可枚举全部任务进程）。
- **TaskManager 是唯一入口**：API 层不得直接改 DB + 起进程 + 删目录的组合操作；每任务 keyed mutex（冲突操作返回 409，个人单用户场景不需要 in-flight 结果共享），每 repo 一把写锁。
- **不做 DDD/洋葱分层**：个人项目规模，扁平 internal 包。

## 2. 进程模型：tmux 会话托管（每活跃任务）

**专属 tmux server**：ocdeck 使用独立 socket `tmux -L ocdeck`，与用户自有 tmux 完全隔离；`kill-server` 只影响 ocdeck 会话。启动时校验 `tmux -V` ≥ 3.2（依赖 `new-session -e` 注入 env），不满足拒绝启动。

| 进程 | tmux 会话名 | 启动方式 | 说明 |
|---|---|---|---|
| serve | `ocdeck-<taskID>-serve` | `new-session -d -c <worktree> -e KEY=V… -- opencode serve --port <P> --hostname 127.0.0.1` | 结构化 API；env 注入含 `OPENCODE_SERVER_PASSWORD=<随机>` |
| TUI | `ocdeck-<taskID>-tui` | `new-session -d -c <worktree> -e … -- opencode attach http://127.0.0.1:<P> --session <id>` | TUI 跑在 tmux 会话内；**浏览器 PTY 只是 attach 客户端**（`tmux -L ocdeck attach -t <session>`）；密码经 `-e OPENCODE_SERVER_PASSWORD` 注入（**不走 --password argv**）；session id 为锚定解析结果：有记录→预检存在复用，无记录或 404 → ocdeck 经 REST CreateSession 创建并持久化（§4，不使用 --continue） |
| shell ×N | `ocdeck-<taskID>-shell-<n>` | `new-session -d -c <worktree> -e … -- $SHELL` | 任务级普通终端，浏览器同样经 attach 客户端接入 |

**会话命名即注册表**：会话名含 taskID，`tmux -L ocdeck ls` 按前缀过滤即可枚举某任务/全部任务的运行时；无需维护可信 PID/PGID 注册表（emdash 同款 `emdash-*` 命名模式）。全部 tmux 操作 MUST 使用精确 target `-t =<name>`（`=` 前缀禁用模糊匹配）——**实测例外（VERIFICATION.md）**：`list-panes`/`capture-pane` 对 `=name` 找不到 pane，这两类操作改用 `name` 或 pane_id；`has-session`/`kill-session` 仍用 `=name`。无 server 时 `has-session`/`kill-server` 退出码 1 + stderr `no server running`，MUST 与 tmux 基础设施错误区分（幂等语义）；最后一个会话消失后 tmux server 自动关闭。taskID 字符集 MUST 约束为 `[a-z0-9-]`，会话名 MUST 经格式校验后才用于命令。TaskManager 内存中仅为每任务维护 `groups: []RuntimeGroup` 作为回调隔离索引：`RuntimeGroup{Role string, SessionName string, Generation int, InstanceID string}`（Role ∈ serve/tui/shell；InstanceID 每次创建/reopen 唯一）。

**tmux 调用环境隔离**：ocdeck 全部 tmux 命令统一 `tmux -L ocdeck -f /dev/null`（`-f /dev/null` 跳过用户 `~/.tmux.conf`，防止 `remain-on-exit` 等配置破坏退出监视）；`TMUX_TMPDIR` MUST 设为 `<dataDir>/tmux`（socket 路径隔离，同时保证测试与开发实例并行安全）。

**退出监视**：以 1-2s 周期 `tmux -L ocdeck has-session -t =<name>` 轮询 + serve 另有 `/global/health` 健康轮询。tmux 调用结果 MUST 三分：
- **启动时无 tmux server**：空运行时，正常情况（无任何会话）。
- **监视期间 tmux server 消失**：全局 runtime-loss 事件——server 消失意味着全部注册会话已丢失，受影响任务 MUST 收敛 suspended + last_error（不得无限保持 active 假象）。
- **临时 exec/权限/协议错误**：退避重试，不改变任务状态；持续失败按基础设施错误告警。

SSE 回调、会话消失回调 MUST 校验 `(generation, role/sessionName, InstanceID)` 三元组仍与注册表当前项匹配，否则忽略——覆盖激活代内 TUI 重开、SSE 重订阅、Suspend 修复等旧实例延迟回调场景。

**终止与逃逸子孙收割（reaper）**：`kill-session` 前 MUST 先 `tmux list-panes -s -t =<name> -F '#{pane_pid}'` 取 pane pid，经 `ps` 收集其子孙进程快照（kill 后幸存者 reparent 到 init 即不可达）；kill-session 后对快照中仍存活者按身份校验（pid+startTime，`ps -o lstart=`）先 TERM 宽限后 KILL——专杀 setsid 逃逸的 dev server 之类（借 emdash tmux-reaper 模式）。**快照失败的规则**：会话仍存在而快照失败 → MUST NOT kill-session，返回结构化 `SnapshotFailed` 错误，由上层按 kill 失败路径重试（保留唯一可定位逃逸子孙的机会）；会话已消失且快照失败 → 视为 degraded cleanup：记 notice（标注"快照缺失，无法收割，不可重试"）后继续——这是唯一允许放弃收割的窗口。进程身份细节（pid+startTime）MUST NOT 出 process 包：对外（notice/DTO/接口）一律使用 **opaque cleanup ticket** 字符串（包内编码 pid+startTime），后台重试经 `RetryReap(tickets)` 重入。

约束与不变量：
- **启动顺序**：serve 会话 → 健康检查就绪 + 能力探测 → tui 会话。**launcher 不变量**：全部 tmux 会话只能由同一 launcher 创建并强制 canonical worktree cwd（`-c` 参数，评审 R3）；occlient 所有请求显式携带 `?directory=<worktree>`。
- **命令构造**：tmux `new-session` 的命令为单个 shell 字符串——MUST 从白名单 argv 逐元素单引号转义构造（`'` → `'\''`），禁止直接拼接未转义用户输入；env MUST 经 `-e KEY=VALUE` argv 传递（不进命令字符串）。
- **env 基线与合并优先级**：**不继承 ocdeck 服务端宿主 env**（与 emdash 同向）。基线 = 最小基础集：`TERM/COLORTERM/HOME/USER/PATH/SHELL/LANG/TMPDIR/SSH_AUTH_SOCK` + 代理三件套（`HTTP_PROXY/HTTPS_PROXY/NO_PROXY`，若宿主存在）。合并优先级：基础集 < 全局级 < 项目级 < 任务级 < 生命周期变量(OCDECK_*) < 内部变量(OPENCODE_SERVER_PASSWORD 等)。**全局级 env**（v1 增补）：跨项目生效的用户变量层，每项两种模式——`follow_host`（激活合并时从 ocdeck 服务端进程 env 解析当前值，宿主未设置则该变量跳过不注入）/ `manual`（使用存储的显式值）；激活快照持久化的是解析后的最终值（快照语义不变：同代复用、下次激活重新解析）。内部与生命周期变量不可被用户 env 覆盖。**激活时合并生成 env 快照并持久化到 `tasks.env_snapshot`**（绑定该 generation）：快照内容 = 基础集 + 全局级 + 项目级 + 任务级 + 通用生命周期变量(OCDECK_*)；**MUST NOT 含进程类型内部变量**（`OPENCODE_SERVER_PASSWORD` 等 role-specific secret）——密码在创建 serve/TUI 会话时叠加注入，不进快照、不落 DB；端口重试时 MUST 先更新快照中的 `OCDECK_SERVE_PORT` 再创建对应 serve。同代内 ReopenAttach 与新建 shell MUST 复用快照（不重新读 DB，shell 因此天然不含密码）；**persist 重启恢复 MUST 从 DB 读回原快照**（重启不是 env 生效点）；快照清除时机：Activate 失败、Suspend 成功、serve 异常退出落 suspended、kill 模式 reconcile 落 suspended；**Suspend 修复回 active MUST NOT 清除**——下次激活才重新合并，兑现"env 修改下次激活生效"。provider 凭据主通道为 opencode 自身 auth store（`~/.local/share/opencode/`），不依赖 env；需要 env 凭据的场景由用户在项目/任务 env 显式配置。
- **tmux exec env 清洗（不变量）**：tmux server 在首个命令时捕获调用方完整 env 作为全局环境并被后续会话继承——为防止宿主 env 经此后门隐式流入，ocdeck 执行全部 tmux 命令时 MUST 以清洗后的 env（仅最小基础集）调用 `exec.Command`，会话 env 只来自 `-e` 显式注入。ocdeck 专属 socket（`TMUX_TMPDIR=<dataDir>/tmux`）保证 tmux server 只由清洗后的 ocdeck 命令启动，全局环境恒为干净。例外：tmux 自注入的 `TMUX/TMUX_PANE` 属会话固有机制，不在清洗范围。
- **每 serve 独立强随机密码**：服务端生成，**不落 DB**，经 `-e OPENCODE_SERVER_PASSWORD` 注入 serve 会话环境。persist 重启恢复时经 `tmux show-environment -t =<session> OPENCODE_SERVER_PASSWORD` 读回（该行为列入 tasks 1.15 假设验证）；读回失败 → 任务落 suspended + last_error。
- **进程列表泄露边界**：`-e KEY=VALUE` 会短暂出现在 tmux 客户端进程的 argv 中（毫秒级，本机同 UID 可观测）——"避免进程列表泄露"的承诺范围为**不进入长期运行的 opencode/serve/shell 进程 argv**；长期进程内密码只存在于其环境变量。密码与 env 值 MUST NOT 写入日志。
- **shell 会话序号**：任务代内从 1 递增分配 `shell-<n>`；persist 恢复时从存活会话名解析已用序号继续分配。
- **serve 只绑 127.0.0.1**：ocdeck 作为唯一入口代理一切访问。

## 3. 端口分配

- DB `tasks.last_port` 仅记录上次成功端口，不是事实来源。
- 激活时：先试 `last_port` → 被占则在可配置范围（默认 50000-50999，轮转起点避免每次从头扫）选下一个 → serve 若 `EADDRINUSE` 退出则自动换端口重试 → 健康检查通过后写回 DB。

## 4. 会话归属与恢复（OpenCode 1.18.9 契约）

- `--continue` 选的是"该目录最近会话"，不等于本任务会话，**必须持久化明确 session ID**。
- **捕获**：任务激活后 occlient 订阅该 serve 的 `GET /event` SSE；`session.created` 与 `session.updated` 的 sessionID 均取 `properties.info.id`（1.18.9 源码核验：envelope `{type, properties:{info: Session}}`），以事件的 `info.time.updated` 刷新 `last_seen_at`。**实测补注（VERIFICATION.md）**：`session.status`/`session.diff` 事件无 `properties.info`，仅携带 `properties.sessionID`（与 info.id 同值，不带 directory）；created/updated/deleted 携带可信 `properties.info.directory`。SSE 流按 session 归属 directory 严格隔离（三向实证无串话），落库防御性过滤：created/updated/deleted 校验 `info.directory == 本任务 worktree`，status/diff 经 `properties.sessionID` 反查 task_sessions 归属表，未命中忽略。激活后 `GET /session?directory=<worktree>&limit=1000` 全量对齐一次（顶层 `id`/`time.updated`）。
- **对齐竞态与截断语义**：全量对齐期间到达的 SSE 事件 MUST 缓冲，对齐完成后按序重放（缓冲起止与对齐替换 MUST 原子）。`count < limit`（结果完整）→ upsert 返回项 + 删除缺席行；`count == limit`（可能截断）→ **仅 upsert，MUST NOT 删除缺席行**（缺席不能证明已删除），同时写入 `session_overflow` notice；后续对齐恢复完整结果时清除该 notice。upsert MUST 使用 `MAX(旧 last_seen_at, 新 time.updated)`（重放较旧事件不得回退时间戳）。
- **SSE 断流**：指数退避重连；重连成功后 MUST 全量对齐一次（断流期间可能错过事件）。
- **"最近 session"定义**（可观测语义）：本任务 directory 下 `last_seen_at` 最大的 session（取 `time.updated`，非对齐时刻；并列时取 `time.created` 大者，再并列取 id 字典序大者）。TUI 切换已有会话不产生 `session.created`，依赖 `session.updated` 与全量对齐兜底。
- **恢复与锚定（v1 增补）**：激活 MUST 立即锚定确定 session，不使用 `--continue`（其"目录最近会话"语义不等于本任务会话）。有记录 → ocdeck 以 `GET /session/:id` **自行预检**：存在 → `attach --session <id>`；**无记录或 404 → ocdeck 经 `POST /session` 创建新会话（title=任务名）→ 持久化 task_sessions → `attach --session <newID>`**——任务首次激活即产生确定 session 归属，用户即使未输入任何内容，后续恢复也是确定性的。预检/创建的其他错误 → 激活失败（attach 自身遇错直接 exit 1 无回退，attach.ts:82-92）。
- **进程退出监视**：serve/TUI 会话消失（has-session 轮询 + serve 健康轮询）时 TaskManager MUST 收到通知。**serve 异常退出（非挂起路径）→ 完整清理该任务运行时**（停 SSE 订阅、kill tui 与全部 shell 会话）→ 任务落挂起 + last_error；TUI 会话消失但 serve 存活则标记终端可重开（ReopenAttach），任务保持活跃。

## 5. 任务状态机

用户态（3 + 硬删除）：`suspended(挂起)` `active(活跃)` `archived(归档)`；删除为**硬删除**（成功后 DB 行移除，无"已删除"展示态）。
内部过渡态（落库用于恢复）：`creating` `creation_failed` `activating` `suspending` `deleting` `deletion_failed`，配 `tasks.last_error`。

```
(无) --创建--> creating --成功--> suspended
creating --失败--> creation_failed（保留记录与 last_error，用户可重试或删除）
suspended --激活--> activating --成功--> active
activating --失败--> suspended (last_error)
active --挂起--> suspending --成功--> suspended
suspending --失败--> active (last_error)
suspended --归档--> archived；active 归档须先经 suspending
archived --恢复--> suspended
suspended/archived/creation_failed --删除--> deleting --> (硬删除)
deleting --失败--> deletion_failed（可重试 / 可强制删除）
```

- **服务端重启 reconciliation（按 §10 shutdownPolicy 分模式）**：启动时**先做 cleanup-debt pre-pass**：读取全部任务 `residual_processes` notice → `RetryReap(cleanupTickets)` → 原子更新 remaining——**仍有 cleanup debt 的任务 MUST NOT 恢复 active**（防止旧代逃逸进程与新代并存）。随后枚举 `tmux -L ocdeck ls` 按 taskID 分组，与 DB 对账，按以下矩阵收敛：

  | DB 状态 × runtime | persist | kill_on_start / kill_immediate |
  |---|---|---|
  | active/activating × serve 会话存活且健康通过 × 无 cleanup debt | **恢复活跃**（恢复序列见下） | kill 全部会话 → suspended |
  | active/activating × serve 会话已消失/健康失败 | kill 残余会话 → suspended + last_error | kill 残余会话 → suspended |
  | suspending × 任意 | 完成清理 → suspended | kill 全部会话 → suspended |
  | suspended/archived × 存在会话 | kill 会话（状态不变，记 notice） | kill 会话（状态不变） |
  | deleting/deletion_failed × 存在会话 | kill 会话、**保持状态**，提示用户 Retry（Retry 按持久化 delete_mode 幂等重建一次性 serve 继续） | kill 全部会话（进程本不应存活）；状态保留提示 Retry |
  | creating × 任意 | kill 异常会话（如有）→ **creation_failed**（不得停留 creating） | 同左 |
  | creation_failed × 任意 | kill 异常会话（如有），保持 creation_failed | 同左 |
  | 孤儿会话（taskID 无 DB 行） | kill；失败记日志并进入**本次运行后台周期重试**（不止等下次启动） | 同左 |

  **带 debt 的健康 serve**（矩阵补充）：`active/activating × serve 存活健康 × pre-pass 后仍有 cleanup debt` → MUST kill 当前 runtime → suspended + last_error（不允许既不恢复也不终止的无人托管运行时）。

  **后台周期重试**：运行期间周期（默认 30s）逐任务处理 `residual_processes` notice，**必须取得该任务 keyed mutex** 且 notice 更新 MUST 为 CAS/事务（避免与 Delete/Suspend/SSE 的 notice 写互相覆盖）：① 若 data.sessionName 存在 → 先 `KillSession`（新产生 tickets 合并进 notice）② `RetryReap(cleanupTickets)` ③ 清除规则：**仅当取得过有效快照并完成 kill/reap（会话消失且 tickets 清空）时**原子清除对应 notice 项；**历史 SnapshotFailed 的会话在重试前自行消失 → MUST 转为 `snapshot_missing_degraded`（retryable=false）保留告警，MUST NOT 当作清理成功清除**。orphan 清理失败项同款重试。

  **persist 恢复中途失败补偿**：恢复序列任一步失败（密码/端口读回、健康检查、SSE 建连、全量对齐、env 快照解析等）→ MUST 停止已建立的 SSE 订阅、kill 该任务当前 runtime（部分清理失败登记 `residual_processes` notice 进后台重试）→ 落 suspended + last_error。**MUST NOT 出现"DB 为 suspended 但健康 serve 仍在运行"的无人托管状态**。

  **persist 恢复序列（严格按序）**：枚举会话 → `show-environment` 读回 serve 密码与 `OCDECK_SERVE_PORT`（读回失败 → 该任务落 suspended + last_error；端口 MUST 以会话内 OCDECK_SERVE_PORT 为准，`tasks.last_port` 仅作交叉校验，activating 崩溃窗口可能未写回）→ 认证健康检查探活（校验会话内 OCDECK_TASK_ID 与任务匹配）→ 建立 SSE 订阅并开始缓冲事件 → 全量对齐 → 重放缓冲 → 重建 RuntimeGroup、**从 DB 恢复原激活 env 快照**（`tasks.env_snapshot`，见 §2/§8；persist 重启不是 env 生效点）→ 标记 active。
- **归档 vs 挂起**：归档 = 挂起 + 列表收起；恢复归档任务先回挂起，再手动激活。

## 6. Worktree 管理

- **位置**：`<dataDir>/worktrees/<projectID>/<taskID>/`（评审修正：移出源仓库，路径确定、易校验、不污染 repo；`git worktree add` 支持任意路径）。
- **创建**：`git worktree add <path> -b <branch> <defaultBranch>`，每 repo 写锁串行。分支名 `ocdeck/<task-name-slug>`，`git check-ref-format` 校验。
- **删除**：canonical path + `filepath.Rel` 包含性校验（禁止字符串前缀判断）；存在 dirty/untracked 文件时需用户确认；分支被其他 worktree 占用时拒绝并说明；随后 `git worktree remove` + `git branch -D` + `git worktree prune`。

## 7. PTY 层与 WebSocket 协议

**PTY = tmux attach 客户端**：浏览器 WS 桥接的 PTY 内运行 `tmux -L ocdeck attach -t <session>`（creack/pty 启动）。PTY 不持有任务进程本体——断开 WS/杀 PTY 只 detach 客户端，tmux 会话与任务进程不受影响。

**断线重连（tmux 固有语义）**：重连 = 新建 PTY 重新 `tmux attach`，tmux 向新客户端推送**当前正确屏幕**——不需要环形缓冲回放，不存在 ANSI/UTF-8 截断问题（原 guard 方案的 64KB 回放 + SIGWINCH 重绘方案整体移除）。resize 由浏览器 → PTY winsize → tmux 客户端自动传播到会话窗口。

**WS 推送削峰与背压**：
- PTY 输出 ≤16ms 窗口批量合并后推送。
- 每 WS 写侧独立 goroutine + 有界队列，慢客户端直接断开。

**WS 协议**（每终端一条连接）：
- 二进制帧：双向终端 IO（输入也走二进制，JSON 不承载高频数据）。
- JSON 控制帧：`auth`（升级后第一条消息，短超时，认证成功前不订阅 PTY）、`resize`。
- token 不走 query（避免反代/访问日志泄露，评审修正）；**Origin 校验**：默认仅允许 `http://localhost:*` / `http://127.0.0.1:*` 源，反代场景经 `OCDECK_ALLOWED_ORIGINS` 配置允许列表，MUST NOT 信任 `X-Forwarded-*`/`Host` 头做同源判断；帧大小上限；原生 ping/pong。
- **单交互客户端**：同一终端新连接替换旧连接（v1 不做多客户端输入抢锁）。

## 8. 存储层

- modernc.org/sqlite（纯 Go 无 cgo）+ sqlc 生成类型安全查询；嵌入式按版本 migration。
- 连接级：`foreign_keys=ON`、`busy_timeout`；单 DB 连接；事务短小，**事务内禁止等待 git/进程/网络**。
- DB 文件 `0600`；日志不得输出 env 值、token、终端内容。

**Schema**：

```sql
projects(id TEXT PK, name, path UNIQUE, default_branch, created_at)
tasks(id TEXT PK, project_id FK REFERENCES projects ON DELETE CASCADE,
      name, branch, status, worktree_path,
      last_port INTEGER, last_error TEXT, notice TEXT, delete_mode TEXT,
      env_snapshot TEXT,  -- 激活时持久化的 env 合并快照（JSON），挂起时清除
      created_at, updated_at, archived_at)
global_env_vars(key TEXT PRIMARY KEY,  -- 全局级 env（v1 增补），无 FK
      mode TEXT NOT NULL,   -- follow_host | manual
      value TEXT)           -- manual 模式的显式值；follow_host 忽略
project_env_vars(project_id FK REFERENCES projects ON DELETE CASCADE,
      key TEXT, value TEXT, PRIMARY KEY(project_id, key))
task_env_vars(task_id FK REFERENCES tasks ON DELETE CASCADE,
      key TEXT, value TEXT, PRIMARY KEY(task_id, key))
task_sessions(task_id FK REFERENCES tasks ON DELETE CASCADE,
      session_id TEXT, session_created_at INTEGER, first_seen_at, last_seen_at,
      PRIMARY KEY(task_id, session_id))
cleanup_debts(session_name TEXT PRIMARY KEY,
      tickets TEXT NOT NULL, created_at INTEGER NOT NULL)
```

`cleanup_debts`（migration 0002）：无 DB 行可归的孤儿会话收割失败 tickets 的跨重启持久化存储——orphan tickets MUST 在产生时立即持久化（不等后台周期），Reconcile 首步骤恢复重试，收敛后删除对应行；恢复/持久化失败 fail-closed（Reconcile 拒绝开放 HTTP）。

session 关联生命周期：SSE `session.deleted` → 删除关联行；全量对齐的删除规则见 §4（仅完整结果可删缺席行）；溢出告警持久化到 `tasks.notice`。

**notice 结构**：`tasks.notice` 存 JSON 数组 `[{code, message, ts, data?}]`，code 枚举：`session_overflow`（对齐截断）、`residual_processes`（挂起/删除残留）。`residual_processes` 的 data：`{sessionName?, cleanupTickets: []string, reason: "snapshot_failed" | "kill_failed" | "reap_failed" | "snapshot_missing_degraded", retryable: bool}`——`snapshot_missing_degraded` 时 `retryable=false`（已接受丢失，不参与后台重试与激活门禁），其余 `retryable=true`。清除语义：`residual_processes` **仅当取得过有效快照并完成 kill/reap（会话消失且 tickets 清空）时**移除对应项；历史 SnapshotFailed 会话在重试前自行消失 MUST 转 `snapshot_missing_degraded` 保留而非清除（与 §5 一致）；`session_overflow` 在下次完整对齐后移除。任务详情 DTO MUST 返回 notice（ticket 为 opaque 字符串，不含可解析进程身份）。

**delete_mode 持久化**：进入 deleting 前持久化 `delete_mode ∈ {normal, force}` 到 tasks 行；`Retry(taskID, confirmDirty)` 无 mode 参数，MUST 按持久化的 delete_mode 重入 deleting——Force 删除中途失败或服务端重启后重试仍跳过 session 删除。删除重试的 dirty 门禁与首次一致：当前 dirty 非空且未显式 confirmDirty=true 时 MUST 拒绝（不得静默重基线化未确认文件）。

## 9. Git 操作

- exec git CLI，固定命令白名单 + argv 数组，**禁止 shell 拼接**；`context.Context` 可取消；stdout/stderr 有界读取。
- status：`git status --porcelain=v2 -z -uall`（`-z` 解析任意文件名）；统计：`git diff --numstat [--cached]`。
- diff：API 返回 **unified diff 文本**（`git diff [ref] -- <path>` 输出），前端用 diff2html 渲染；字节数/文件数超限返回截断标记。
- commit/push 用本机 git，保 hooks/签名/SSH agent 行为；HTTP 进程无 TTY，交互式签名口令可能失败——原样透传 git 错误。
- **串行化粒度**：仅 worktree 增删等仓库级写操作进每 repo 锁；status/diff 只读不进队列。
- diff API 服务端限制字节数与文件数，二进制/超大返回截断标记。

## 10. 服务端生命周期与关停策略（shutdownPolicy）

**配置项** `shutdownPolicy`（配置文件或 env `OCDECK_SHUTDOWN_POLICY`——**v1 实现仅 env 来源**，配置文件来源为已备案的后续项；全部配置项同此约定），三档：

| 模式 | 正常退出 | 异常死亡（kill -9） | 重启后语义 |
|---|---|---|---|
| `persist`（默认） | 保留全部 tmux 会话 | 会话存活（tmux server 持有） | reconcile **恢复活跃**（恢复序列见 §5） |
| `kill_on_start` | kill 全部会话 | 会话暂时存活 | reconcile 启动时 kill 全部 `ocdeck-*` 会话，任务落挂起 |
| `kill_immediate` | kill 全部会话 | **watchdog 兜底**（FSM 见下） | 同 kill_on_start（双保险清理） |

**watchdog FSM**（仅 kill_immediate）：
- **spawn 时机**：服务端启动时、任何 tmux 会话创建之前 spawn 单个 `ocdeck-server watchdog` 子进程；**spawn 失败 MUST 拒绝启动服务端**（该模式的核心保证无法兑现）。
- **运行**：轮询自身 ppid（1s）；父进程消失 → 进入 kill 路径。
- **kill 路径（内置全局 reaper）**：先对全部 `ocdeck-*` 会话 `list-panes` 收集 pane 子孙快照 → `tmux -L ocdeck kill-server` → 对快照存活者按身份校验 TERM→宽限→KILL → 自退。**setsid 逃逸子孙也在收割范围**——"服务端挂则全挂"包含 shell 里启动的 daemonized 子孙。无 tmux server 时 `kill-server` 退出非零 MUST 视为幂等成功。
- **watchdog 自身死亡**：ocdeck 检测到 watchdog 退出 MUST 立即重启（指数退避，上限 3 次）；连续失败 → 服务级告警（`GET /server/status` 暴露 `watchdogState: degraded`，UI banner 提示）并降级按 kill_on_start 语义运行（提示用户重启服务端恢复 kill_immediate）。
- **正常关停顺序（防 kill -9 窗口）**：quiesce（停止接受新操作）→ **watchdog 存活期间**按策略清理全部任务会话 → 确认 runtime 已空（**无 tmux 会话且无可重试 cleanup debt**，不止 `ListSessions` 为空）→ `StopWatchdog`（约定信号 + ack 等待，超时强杀并 Wait 回收）→ 主进程退出。顺序 MUST NOT 颠倒：先停 watchdog 再清理会话会留下"watchdog 已死、会话未清"的 kill -9 窗口，破坏 kill_immediate 保证。

**单实例锁**：同一 dataDir MUST 以 flock 持有 `<dataDir>/ocdeck.lock`，获取失败拒绝启动——禁止同 UID 多实例共享 `-L ocdeck` socket 互相枚举/清理/kill-server。

说明与边界：
- **`persist` 是 tmux 架构的核心收益**：服务端升级/调试重启不打断 agent 长任务。
- `kill_immediate` 用**单个** watchdog 兑现原 guard 方案的语义——tmux server 是天然单点，不需要 per-PGID guard 注册表与 ProcessHandle 模型。watchdog 先于任何会话创建 spawn，kill -9 窗口仅剩"服务端启动到 watchdog spawn 之间"的毫秒级，由重启 reconcile 兜底（此时还没有会话可漏）。
- `kill_on_start`/`kill_immediate` 模式下 kill -9 到下次启动之间 agent 仍在运行（无人可杀）——"立即"只对 kill_immediate 的 watchdog 路径成立。
- **平台边界**：tmux 仅 POSIX。未来 Windows 支持需降级为 ConPTY + Job Object 直管模式（`KILL_ON_JOB_CLOSE` 内核级兑现 kill_immediate 语义），平台差异封装在 `internal/process` 后端接口内，v1 仅 Darwin。

## 11. API 漂移防护（R1）：能力探测优先

opencode 升级频繁，版本等值校验会让 ocdeck 频繁罢工——**激活门禁是能力探测，不是版本号**：

- 服务端启动时 `opencode --version` 记录；`opencode` 不存在则拒绝启动并提示。
- **版本号仅作告警**：版本 != 契约基准（当前 1.18.9）→ warning 日志 + UI 提示"当前 opencode 版本未经 ocdeck 验证"，**不阻止任何操作**。
- **能力探测是激活门禁**：每次激活 serve 后探测 `/global/health` 可达 + `/session/status` 响应结构 + session 列表字段形状符合 occlient 契约（**DELETE 形状 MUST NOT 做 live 探测**——不能为探测制造删除副作用；首次真实删除时校验响应，不符报 deletion_failed）。不兼容 → 阻止激活并报错"当前 opencode 版本与 ocdeck 契约不兼容（契约基准 1.18.9），请升级 ocdeck 或回退 opencode"。
- contract fixture 锁定基准版本：opencode 升级导致契约变化时，fixture diff 精确定位漂移点，而非"版本不对"式无信息报错。
- occlient 集中封装全部端点与错误格式，漂移只改一处。

## 12. 删除任务流程（R2 修正）

顺序（与 §19 一致）：静态安全检查 → 持久化 delete_mode + 置 deleting → **RetryReap 既有 cleanup debt**（remaining 非空落 deletion_failed）→ 经 serve `DELETE /session/:id` 逐个删除 `task_sessions` 记录的会话 → kill tui/shell 会话 → kill serve 会话 → 删 worktree/分支 → 删 DB 行。
- 进程已死（服务端崩溃后删除）：临时以该 worktree 为 directory 起一个一次性 serve 会话执行 session 删除（不直接操作 opencode DB），随后清理。
- 任一步失败：任务落 `deletion_failed` + `last_error`，允许重试；另提供"强制删除（保留 oc session 数据）"选项。

## 13. 全局配置文件管理

- 列出/读取 `~/.config/opencode/` 下 `*.json` 与 `*.jsonc`；保存前按扩展名分流语法校验：`.json` 严格 JSON；`.jsonc` 用成熟 JSONC parser（允许注释与尾逗号，候选 `github.com/tidwall/jsonc` 或同等库）。不做业务 schema 校验；编辑保留原文（注释不改写）。
- **乐观并发**：读取时记录 mtime+hash，保存时比对，不一致返回冲突（评审 R4 修正）。
- 写入：临时文件 + 原子 rename + 保留原权限 + `.bak` 备份；拒绝路径逃逸与不受控 symlink。
- 保存成功后列出受影响的活跃任务，提示用户手动重启生效（不自动打断会话）。

## 14. 访问控制与安全边界

- 单 token：env `OCDECK_TOKEN` 或配置文件；未配置拒绝启动。REST 用 `Authorization: Bearer`；WS 用首消息认证。
- 默认绑 `127.0.0.1`；远程访问要求用户自管 HTTPS 反代（README 说明）。
- 日志红线：token/env 值/终端内容/git 输出中的敏感信息不得入普通日志。

## 15. 前端

- React + Vite + TS；xterm.js（WebGL renderer）；diff2html 渲染 diff（diff 内容必须转义，不得作为可信 HTML）。
- 生产构建 `web/dist` 用 `embed.FS` 内嵌进 Go 二进制，同源服务，部署与认证最简单。
- 页面：项目列表/详情、任务列表（状态机操作）、任务工作台（TUI 终端 + shell 终端标签 + git 面板）、env 编辑、全局配置编辑。

## 16. 实施策略：纵向链路优先

第一阶段只做一条纵向链路并验证三个架构假设（评审最高优先级风险）：
1. **多 serve 并发访问全局 OpenCode DB** 是否产生锁错误/串话（若不行，研究官方数据目录隔离方案再定架构）
2. session ID 捕获与 `--session` 恢复映射
3. **tmux 托管正确性**：`new-session -e` env 注入、attach 客户端断开/重连取回当前屏幕、resize 传播、reaper 收割逃逸子孙

纵向链路：建 worktree → 起 serve+TUI 会话 → 捕获 session ID → WS 断线重连（tmux reattach） → 挂起/恢复 → 完整删除。
通过后再横向扩展 git UI、env、全局配置等。

## 17. 实现模式借鉴（emdash 源码，clone 于 /tmp/emdash-research）

| 模式 | emdash 出处 | Go 实现策略 |
|---|---|---|
| tmux 会话命名 | tmux-session-name.ts（`emdash-*` 前缀命名 + list-sessions 过滤枚举） | `ocdeck-<taskID>-<role>` 命名 + `tmux -L ocdeck ls` 前缀过滤 |
| tmux reaper | tmux-reaper.ts（kill-session 前 list-panes 取 pane_pid → ps 收集子孙快照 → 杀逃逸子孙） | 同款：快照 → kill-session → 身份校验后 TERM→KILL 逃逸子孙 |
| tmux 兜底清理 | tmux-reconcile.ts（启动对账遗留会话） | 启动 reconcile 按 shutdownPolicy 恢复或清理 |
| 16ms 批量刷新 | pty-session-registry.ts:43-64 | time.Ticker + bytes.Buffer，WS 推送削峰 |
| git 串行化 | worktree-service.ts:54-58（promise 链） | 每 repo buffered chan + 单 goroutine 消费 |
| 路径包含性校验 | realpath-containment.ts（双端 EvalSymlinks，不存在路径向上找最近存在的祖先解析） | filepath.EvalSymlinks 双端解析 + filepath.Rel |
| worktree 校验 | worktree-service.ts:60-77（.git 文件存在 + rev-parse --is-inside-work-tree） | 相同 |
| LifecycleMap 去重 | lifecycle-map.ts（provision/teardown in-flight 去重 + 错误记录） | **简化为 keyed mutex + 409 冲突**（个人单用户，不做 in-flight 结果共享） |
| 三模式 teardown | task-session-manager.ts:66-83（detach/terminate/archive） | 对应挂起/删除/归档路径（tmux 下 detach 由 shutdownPolicy 全局决定） |
| porcelain v2 解析 | status-parser.ts（流式 NUL 分隔解析，10000 文件上限） | bufio 流式解析，同款上限 |
| git show ref:path | git-service.ts:402-422（含 :0: 暂存区引用） | 相同 |
| xterm off-screen host | renderer/lib/pty/pty.ts:76-157,200-225（创建一次、mount/unmount 仅 reparent、mount 前预 resize、rAF 强制重绘） | 前端照搬该模式 |
| worktree prune 时机 | worktree-service.ts（add 前、remove 后、校验失败后） | 相同 |

**env 策略与 emdash 同向**：不继承宿主 env，最小基础集 + 项目/任务显式配置（§2）。provider 凭据走 opencode 自身 auth store；需要 env 凭据时用户在 ocdeck env 管理中显式声明。全部 tmux 命令以清洗 env 执行，杜绝 tmux server 全局环境后门。

## 18. 包边界与核心接口契约

依赖方向（禁止反向）：`api → task → {process, pty, worktree, git, opencode, store}`；`store` 不依赖任何上层。

| 包 | 唯一职责 | 核心接口（方法签名级） |
|---|---|---|
| api | HTTP/WS 端点、token 中间件、DTO 校验；**不做任何业务编排** | 见 §21 路由表 |
| task | **TaskManager**：任务状态转换、进程、worktree 操作的唯一入口；每任务 keyed mutex | `Create(projectID, name) (Task, error)` / `Activate(taskID) error` / `Suspend(taskID) error` / `Archive(taskID) error` / `Restore(taskID) error` / `Delete(taskID, mode DeleteMode) error`（DeleteMode: Normal/Force）/ `Retry(taskID, confirmDirty bool) error`（对 creation_failed/deleting/deletion_failed；confirmDirty 仅作用于删除重试的 dirty 门禁）/ `CreateShell(taskID) (TerminalID, error)` / `CloseShell(terminalID) error` / `ReopenAttach(taskID) (terminalID, error)` / `Get/List` |
| process | tmux 会话后端（平台抽象边界：posix=tmux 实现；未来 windows=ConPTY+Job Object 降级实现）、会话生命周期、reaper、watchdog FSM、退出轮询、单实例锁 | `NewSession(spec SessionSpec{Name, Dir, Env, CmdArgv}) error`（argv 白名单转义构造，§2；exec env 清洗）/ `KillSession(name) (KillResult{SessionKilled bool, Disposition CleanupDisposition, CleanupTickets []string}, error)`（先 reaper 快照再 kill-session 再收割；`CleanupDisposition ∈ {clean, retryable_snapshot_failed, retryable_kill_failed, retryable_reap_failed, snapshot_missing_degraded}`：**clean MUST 满足 SessionKilled=true 且 tickets 为空且取得过有效快照**；会话存活而快照失败未执行 kill → retryable_snapshot_failed；快照成功但 kill-session 失败会话仍在 → retryable_kill_failed（**CleanupTickets MUST 携带该次有效快照的全部进程身份**——防止会话稍后自行消失时快照身份丢失、无法 RetryReap）；会话已杀但逃逸收割失败产生 tickets → retryable_reap_failed；快照缺失且会话已消失 → snapshot_missing_degraded，记入 notice 且 retryable=false；**absent-at-entry**：TaskManager MUST 只对已确认存在的会话调用 KillSession——调用前已不存在由上层直接视为该步骤幂等成功，KillSession 无需 already_absent 语义）/ `RetryReap(tickets []string) (remaining []string, error)`（按 opaque ticket 重试收割，供残留 notice 后台重试与启动 reconcile）/ `HasSession(name) (bool, error)`（区分"会话不存在"与"tmux 命令失败"）/ `ListSessions() ([]string, error)` / `ShowSessionEnv(name, key) (string, error)`（密码/端口恢复）/ `AttachPty(name) (*pty.Pty, error)` / `WatchExit(name, callback) (cancel func())`（轮询实现，返回取消句柄）/ `SpawnWatchdog() error` / `StopWatchdog() error`（ack 握手，仅 kill_immediate）/ `AcquireInstanceLock(dataDir) (release func(), error)` |
| pty | attach 客户端 PTY 池、16ms 批量刷新、resize | `Open(cmd, cwd, env) (*Pty, error)` / `Read/Write/Resize/Close` |
| worktree | worktree 增删、每 repo 写锁、包含性校验 | `Add(projectID, taskID, branch, baseRef) (path string, err error)` / `Remove(path string, opts) error`（dirty 检查、分支占用检查、prune） |
| git | git CLI 白名单封装 | `Status(dir) ([]FileStatus, error)` / `Diff(dir, ref, path) (unified string, truncated bool, err error)` / `Commit(dir, msg, paths) error` / `Push(dir, branch) error` |
| opencode | occlient：serve REST/SSE 封装、版本/能力探测 | `Probe(port, password) (version string, err error)`（health + /session/status 结构 + session 列表字段形状；DELETE 形状不做 live 探测，首次真实删除时校验，不符报 deletion_failed）/ `SessionStatus(...)` / `ListSessions(dir, limit)` / `GetSession(dir, id) (Session, error)`（404 区分于其他错误）/ `DeleteSession(id)`（404 幂等）/ `SubscribeEvents(ctx, dir, onEvent func(Event), onReconnect func()) error`（立即建立连接；断流重连成功后 onReconnect MUST 先于任何新事件回调触发，供 TaskManager 执行全量对齐屏障；ctx 取消即退订） |
| store | SQLite 访问（sqlc 生成）、migration | sqlc 生成的 Queries；`Migrate(db) error` |
| config | 服务端配置、全局 oc 配置文件管理 | `Load() Config` / `ListOCConfigs/ReadOCConfig/SaveOCConfig(name, content, mtime, hash)` |

## 19. 操作的副作用边界（前置检查 → 意图落库 → 外部副作用 → 提交点 → 补偿/重试）

统一模型：**先持久化意图（状态置过渡态），再做外部副作用，成功后置终态提交点；任一步失败置 *_failed + last_error；Retry 幂等（资源不存在视为已成功）**。

| 操作 | 前置检查（任一失败则无副作用） | 外部副作用顺序 | 提交点 | 失败补偿 |
|---|---|---|---|---|
| Create | 项目存在；分支名 check-ref-format；分支名不冲突 | ① **插入任务行（status=creating）** ② git worktree add ③ 更新 status=suspended ④ **自动触发激活**（异步，等价于用户手动 Activate：经 Activate 全部门禁与步骤，含 session 锚定；失败落 suspended+last_error，可手动重试） | ③ 成功 → suspended（④ 异步推进至 active） | ②③ 任一步失败 → creation_failed（任务行已存在，承载 last_error）；**Retry**：校验路径是否已是预期 worktree（.git 文件存在 + `rev-parse --is-inside-work-tree` + 检出分支匹配 + 属预期 repo）——是则跳过 add 重试 ③，否则重新 add；Delete：清理 worktree+分支+行 |
| Activate | 状态 ∈ {suspended}；opencode 二进制存在（版本不匹配仅告警，能力探测在步骤④把关，§11）；**无未清理的旧代残留会话**（`tmux ls` 中仍存在该任务会话则拒绝激活并提示先清理/Force Delete，管理面保持可用）；**无未清理的 cleanup debt**（存在任意 `residual_processes` notice 且 `data.retryable == true` 时拒绝激活——MUST 按 notice 存在性判定，MUST NOT 只看 tickets 或会话存活：快照失败类 notice 无 tickets 但会话可能仍活着并操作 worktree；后台重试成功清除后方可激活；degraded 不可重试 notice 不阻止激活但 UI 持续提示） | ① 置 activating ② 分配端口、合并 env 快照并持久化 ③ `NewSession(serve)`（-e 注入 env，含 OCDECK_SERVE_PORT）④ 健康检查+能力探测 ⑤ 订阅 SSE+全量对齐 ⑥ `NewSession(tui)`（opencode attach --session；无记录或 404 时先经 REST 创建会话锚定，§4） | tui 会话创建成功 → active | 任一步失败：kill 已建会话 → suspended + last_error；kill 部分失败（含 SnapshotFailed、逃逸子孙未净）→ 仍落 suspended，残留记 notice（cleanupTickets/reason）+ 后台 RetryReap；tmux 基础设施故障（server 不可达等非"会话不存在"错误）→ 操作失败 + last_error，MUST NOT 误判为资源已清理 |
| Suspend | 状态 ∈ {active} | ① 置 suspending ② 停 SSE 订阅 ③ KillSession 全部会话（tui→shells→serve，各自先 reaper 快照） | 全部会话终止 → suspended | **互斥决策树**（按序判定，取首个命中分支）：a) serve 会话已消失 → 继续完成剩余清理 → suspended（个别会话杀不掉：注册残留会话 + notice + 后台重试）；b) serve 存活且全部 kill 成功 → suspended；c) serve 存活但有 kill 失败 → 尝试修复运行时（重订阅 SSE、重开 tui 会话）→ 修复成功回 active + last_error；修复失败或修复期间 serve 死亡 → 转分支 a |
| Archive | 状态 ∈ {suspended}（active 须先 Suspend） | 无外部副作用，仅 DB | archived | - |
| Restore | 状态 ∈ {archived} | 无外部副作用，仅 DB | suspended | - |
| Delete(Normal) | 状态 ∈ {suspended, archived, creation_failed}；**静态安全检查全部通过才允许任何副作用**：dirty/untracked 确认（交互前置）、分支被其他 worktree 占用检查、路径包含性校验；degraded 不可重试 notice（快照缺失类）不阻止 Delete（属已接受丢失，UI 提示） | ① 持久化 delete_mode + 置 deleting ② **RetryReap 既有 cleanup debt**（remaining 非空 → 落 deletion_failed，MUST NOT 继续——防止删除 DB 后 ticket 级联丢失、逃逸进程失去身份索引）③ 删 oc sessions（逐个，404 视为成功，每个落账；**无活跃 serve 时起一次性 serve**：`NewSession(一次性 serve)` → 删完即 KillSession）④ KillSession 残余会话（若有）⑤ git worktree remove + branch -D + prune ⑥ 删 DB 行（CASCADE task_sessions/task_env_vars） | DB 行删除 | 任一步失败 → deletion_failed + last_error；Retry 按持久化 delete_mode 重入 deleting，从失败步骤继续（一次性 serve 会话失败同样清理） |
| Delete(Force) | 状态 ∈ {suspended, archived, creation_failed, deletion_failed}（**强制删除 MUST 接受 deletion_failed**，兑现"删除失败后可强制删除"）；静态安全检查同 Normal | 跳过 ③ oc session 删除，其余同 Normal（**含 ② RetryReap debt——Force 只能跳过 oc session 删除，MUST NOT 跳过进程收割**） | DB 行删除 | 同上 |

未知结果处理：DB 写与外部副作用之间的任何崩溃，由启动 reconciliation + *_failed 状态 + 幂等 Retry 收敛，不追求分布式事务。

## 20. OpenCode 1.18.9 外部契约附录

版本策略：**契约基准版本 `1.18.9`**（fixture 锁定），但**版本号不作为门禁**（见 §11）：版本不匹配仅告警；激活门禁为能力探测（health + `/session/status` 结构 + session 列表字段形状；DELETE 形状不做 live 探测，首次真实删除时校验）。contract test MUST 覆盖"能力探测不兼容被拒绝"与"版本不匹配仅告警且放行"两条路径。下表为 1.18.9 源码核验的契约基线，探测与 fixture 以此为参照：

| 端点/行为 | 请求 | 关键响应字段（源码核验） | 失败语义 |
|---|---|---|---|
| `GET /global/health` | - | `{healthy: bool, version: string}` | 超时/非 2xx → serve 未就绪 |
| `GET /session?directory=<wt>&limit=<N>` | Basic Auth + directory query | `[]Session`，**顶层字段** `id`、`time.updated`（session.ts:191-209，非嵌套 info.id）；默认 limit 100（session.ts:315），ocdeck 显式传 limit=1000 | 401 → 内部 bug；5xx → 激活失败；返回数=limit 视为溢出，告警并依赖 SSE 增量 |
| `GET /session/:id?directory=<wt>` | 同上 | Session 对象；404 = 不存在 | 用于 attach 前预检与孤儿 session 清理。**实测偏差（VERIFICATION.md）**：directory 参数在单条 GET 不生效，session ID 全局可读——directory 隔离由 ocdeck 经列表 + task_sessions 归属表自行保证，不得依赖单条 GET 的 directory 做隔离边界 |
| `GET /session/status?directory=<wt>` | 同上 | `{sessionID: {type: "idle"\|"busy"\|"retry", ...}}`（session-status-event.ts，三值枚举，未记录默认 idle） | 结构不符 → 能力探测失败，阻止激活 |
| `DELETE /session/:id?directory=<wt>` | 同上 | **200 + JSON true**（handlers/session.ts:201，非 204） | **404 视为已删除（幂等成功）**；其他失败 → deletion_failed |
| `GET /event?directory=<wt>` (SSE) | 同上 | envelope `{type, properties}`；`session.created`/`session.updated` 均为 `properties.info: Session`，sessionID = `properties.info.id`（types.gen.ts）；另有 `session.deleted`；**实测补注**：envelope 另含冗余 `properties.sessionID` 与顶层事件 `id`，首事件为 `server.connected`——occlient 统一只取 `properties.info.id` | 断流 → 指数退避重连 + 重连后全量对齐 |
| `attach --session <id>` | PTY 进程 | attach 内部预检 `GET /session/:id`，**任何失败直接 exit 1，无自动回退**（attach.ts:82-92） | **回退由 ocdeck 实现**：spawn 前 occlient.GetSession 预检，404 或无记录时经 `POST /session` 创建新会话锚定后 `--session`（§4）；预检/创建其他错误 → 激活失败 |

## 21. 管理面 REST / WS 契约

**统一错误结构**：`{"error": {"code": string, "message": string}}`；code 枚举：`unauthorized/not_found/conflict/invalid_state/invalid_input/oc_incompatible/git_error/process_error/internal`。HTTP 状态码语义化（401/404/409/422/500）。

**REST 路由**（全部 `/api/v1` 前缀，Bearer 认证）：

| Method | Path | 说明 |
|---|---|---|
| GET/POST | /projects | 项目列表 / 注册（body: name, path） |
| GET/DELETE | /projects/:id | 详情（含任务概况）/ 删除（有任务则 409） |
| GET/PUT/DELETE | /env | 全局级 env（PUT body: key,mode,value；mode ∈ follow_host\|manual，follow_host 激活时从服务端进程 env 解析） |
| GET/PUT/DELETE | /projects/:id/env | 项目级 env 列表 / 设置 / 删除（PUT body: key,value） |
| GET/POST | /projects/:id/tasks | 任务列表 / 创建（body: name） |
| GET | /tasks/:id | 任务详情（状态、last_error、notice（§8 结构化告警，MUST 返回）、worktree、sessions） |
| POST | /tasks/:id/activate \| suspend \| archive \| restore \| retry | 状态机操作；冲突返回 409；retry 支持 `?confirmDirty=true`（删除重试 dirty 确认） |
| POST | /tasks/:id/attach/reopen | attach 退出后显式重开（WS 建连亦自动触发） |
| DELETE | /tasks/:id?mode=normal\|force&confirmDirty=true | 删除 |
| GET/PUT/DELETE | /tasks/:id/env | 任务级 env |
| GET | /tasks/:id/git/status | worktree 状态 |
| GET | /tasks/:id/git/diff?ref=&path= | unified diff（截断标记） |
| POST | /tasks/:id/git/commit \| push | body: message,paths / 无 body |
| GET/POST | /tasks/:id/terminals | shell 终端列表 / 新建（返回 terminalID） |
| DELETE | /terminals/:tid | 关闭 shell 终端 |
| GET/PUT | /oc-configs 与 /oc-configs/:name | 全局 oc 配置列表/读取/保存（PUT 带 mtime+hash 乐观并发，409 冲突） |
| GET | /server/status | 服务端状态：opencode 版本与契约基准、watchdogState（off/running/degraded）、shutdownPolicy、tmux 版本 |

**WS 端点**：`/ws/terminal/:taskID`（TUI 终端）与 `/ws/terminal/shell/:terminalID`（shell 终端）。TUI WS 在**首帧认证与 Origin 校验成功后**，若任务活跃但 TUI 会话已消失，服务端 MUST 自动 ReopenAttach 再接入；并发 REST/WS 重开 MUST 幂等复用同一个新 TUI 会话（不产生 409 失败）。

- 首帧（client→server, JSON）：`{"type":"auth","token":"...","cols":N,"rows":M}`——认证+初始尺寸握手合一，5s 超时。
- 控制帧（JSON）：`{"type":"resize","cols":N,"rows":M}`；服务端响应 `{"type":"auth_ok"}` / `{"type":"error","code":...}`。
- 二进制帧：双向终端 IO。
- 关闭码：4001 未认证、4009 被新连接替换、4010 任务已挂起、1011 服务端内部错误。

## 22. 阶段一门禁

阶段一完成定义：tasks.md 1.15 三个架构假设验证记录结论 + 1.16-1.19 [P1] 测试全部通过。**架构假设任一未通过不得进入阶段二**，先回到设计调整（如 OpenCode 数据目录隔离方案）。
