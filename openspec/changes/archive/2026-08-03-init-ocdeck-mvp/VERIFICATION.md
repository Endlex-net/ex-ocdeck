# 架构假设验证记录 — init-ocdeck-mvp task 1.15

> 对应 design.md §16 / §22 阶段一门禁。全部为**实证**（真实 opencode 1.18.9 + tmux 3.6a 本机运行），非代码审查。

## 环境

| 项 | 版本 | 命令 |
|---|---|---|
| opencode | 1.18.9 | `opencode --version` → `1.18.9` |
| tmux | 3.6a | `tmux -V` → `3.6a`（≥ 3.2 要求满足） |
| git | 系统自带 | `/usr/bin/git` |
| 平台 | macOS / Darwin | reaper `ps` 解析依赖 Darwin ps 语义 |

- 全部实验在临时目录 `/var/folders/.../T/opencode/ocdeck-verify/` 与独立 tmux socket（`ocdeck-test` / `ocdeck-verify`，**非生产 `ocdeck`**）下进行。
- 全局 opencode 数据目录 `~/.local/share/opencode/` 为多个 serve 共享（假设 (a) 的前提）。
- `~/.ocdeck` 未创建、未触碰；无运行中的 ocdeck 实例被干扰。
- 实验结束后：全部 opencode serve 进程已终止、tmux test server 已 `kill-server`、逃逸子孙已收割、临时目录已删除。

---

## 假设 (a)：多 serve 并发访问全局 OpenCode DB 无锁错误 / 串话（列表无串话，单条 GET 全局可读）

### 验证步骤

1. 在 3 个独立 git 目录 `dirA/dirB/dirC` 各启动一个 `opencode serve --port <P> --hostname 127.0.0.1`（端口 50200/50201/50202），每个实例独立随机密码（`OPENCODE_SERVER_PASSWORD`）。
2. 健康检查：`GET /global/health` 三实例均返回 `{"healthy":true,"version":"1.18.9"}`。
3. 并发创建 session：3 实例各并发 POST `/session?directory=<own dir>` 3 次（标题标记 A-sN/B-sN/C-sN）。
4. 并发发消息：3 实例同时 POST `/session/:id/message` 写入各自 session。
5. 并发删除：A 与 B 同时 DELETE 各自一个 session。
6. 串话探测：用 A 的密码在 A 的 serve 上 `GET /session/<B的sessionID>?directory=<dirA|dirB>`；用错误密码访问。
7. 检查全部 serve 日志是否出现 `lock|busy|sqlite|error|fail`。

### 观察结果

| 检查项 | 结果 |
|---|---|
| 3 serve 并发启动 | 全部健康，version 1.18.9 |
| 并发创建 session（共 ~10 个） | 全部 200，无错误 |
| 并发发消息（3 实例同时写） | 全部 200，无锁错误 |
| 并发删除（A+B 同时） | A:200 + `true`，B:200 + `true`，无锁错误 |
| 各 directory 列表隔离 | A 查 dirA=4（含初始）、B 查 dirB=3、C 查 dirC=3，互不出现对方 session |
| serve 日志锁错误 | 三实例日志均无 `lock/busy/sqlite/error/fail`（仅 `listening on ...`） |
| 删除后状态正确 | A count 4→3，B count 3→2，无状态损坏 |

### 串话探测结果（**契约偏差**）

- `GET /session?directory=<dir>`（列表）：**directory 过滤生效**，A 查 dirB 只返回 B 在 dirB 的 session，不返回 A 在 dirA 的 session。
- `GET /session/:id?directory=<dir>`（单条）：**directory 过滤不生效**。用 A 的密码 + A 的 serve，`directory=dirA` 查询 B 创建的 session ID，**仍返回 200 + B 的完整 session 对象**（含 directory=dirB 字段）。
- 即：**session ID 在全局 DB 内跨 directory 可见**，`?directory=` 在单条 GET 上被忽略。密码仅做实例级鉴权（错误密码 → 401），不做 directory 级隔离。
- design §20 将 `GET /session/:id?directory=<wt>` 列为预检端点并依赖 directory 语义；实测 directory 在该端点不构成访问边界。

### 结论

**PASS（核心假设成立，列表无串话 + SSE 流无串话，单条 GET 全局可读）**：多 serve 并发访问全局 OpenCode DB 无锁错误、无状态损坏。3 实例并发 create/message/delete 全部成功，日志无 SQLite 锁错误。directory 隔离在**列表端点**（`GET /session?directory=`）与**SSE 流**（`GET /event?directory=`，三向实证无串话）上成立；单条 `GET /session/:id` 的 directory 参数不生效（session ID 全局可读，见下方偏差）。阶段一假设 (a) 通过。

**契约偏差（需 design 知悉，不阻塞阶段一）**：
- `GET /session/:id` 的 `?directory=` 参数不生效，session ID 跨 directory 全局可读。occlient.GetSession 预检（design §4 "ocdeck 以 `GET /session/:id` 自行预检最近 sessionID 存在性"）仍可用（404 区分存在性正确），但**不能依赖 directory 做隔离**；隔离由 ocdeck 自身经 `GET /session?directory=` 列表 + session ID 归属表实现，而非依赖 serve 的单条 GET directory 过滤。
- design §20 该行应补注：单条 GET 的 directory 参数在 1.18.9 实测不参与过滤，仅列表端点参与。

### 跨 directory SSE 隔离验证（P4 门禁补做）

补充验证：`GET /event?directory=<dir>` SSE 流是否跨 directory 串话（假设 (a) 的 REST 端点已证列表隔离、单条 GET 全局可读，但 SSE 流的 directory 隔离未单独实证）。这是 ocdeck SSE 回调落库（activate.go 把收到的 session 事件写入当前任务 task_sessions）可信性的前置条件。

#### 验证步骤

1. 两个目录 `dirA/dirB` 各起一个 `opencode serve`（端口 50400/50401，各自随机 `OPENCODE_SERVER_PASSWORD`），健康检查均 `1.18.9`。
2. 同时订阅两个实例的 `GET /event?directory=<own dir>` SSE 流（A→dirA、B→dirB），分别写入独立日志。
3. **正向**：在 A 上对 dirA session 执行 create/message/delete，观察 A 流（预期收到）与 B 流（关键：是否也收到 A 的事件）。
4. **反向**：在 B 上对 dirB session 执行 create/message/delete，观察 B 流（预期收到）与 A 流（关键：是否收到 B 的事件）。
5. **同 serve 跨 directory 写**：在 A 的 serve 上用 `directory=dirB` 创建 session（session 归属 dirB），观察 A 流（订阅 dirA）是否收到——验证 SSE 过滤是按 session 归属 directory 而非按 serve 实例。
6. 提取各类 session 事件的 `properties.info.directory` 字段值，核对是否等于事件归属目录。
7. 检查 SSE 首事件序列（`server.connected` 后的事件类型）。

#### 观察结果

| 检查项 | 结果 |
|---|---|
| A 流初始事件 | `server.connected`（建连即发） |
| B 流初始事件 | `server.connected`（建连即发） |
| **正向：A 制造 dirA 事件，B 流是否收到** | **否**——B 流仅 `server.connected` + `server.heartbeat`，session.* 事件 count=0；A 流 0 次被 B SID 引用 |
| **反向：B 制造 dirB 事件，A 流是否收到** | **否**——A 流 session.created count=1（仅 A 自己），0 次引用 B SID；B 流收到自己的 session.created/updated/diff/status |
| **同 serve 跨 dir 写**：A serve 创建 dirB session，A 流（订阅 dirA）是否收到 | **否**——A 流 0 个 session.* 事件，0 次引用跨 dirB SID；session 归属 dirB 的事件只推给订阅 dirB 的流 |
| `session.created` 的 `properties.info.directory` | = dirA（A 事件）/ = dirB（B 事件），**携带可信归属** ✓ |
| `session.updated` 的 `properties.info.directory` | = 归属目录，**携带** ✓ |
| `session.deleted` 的 `properties.info.directory` | = 归属目录，**携带** ✓ |
| `session.status` / `session.diff` 的 `properties.info` | **无 info 对象**，仅有 `properties.sessionID`，**不带 directory 字段** |
| 首事件序列 | `server.connected` → 后续 session 事件（`session.created` 等）+ 周期性 `server.heartbeat` |

#### 结论

**PASS（无跨 directory SSE 串话）**：`GET /event?directory=<dir>` SSE 流按 **session 归属 directory** 严格隔离——A 流只收 dirA session 事件、B 流只收 dirB session 事件、同 serve 上跨 directory 写入的 session 事件也只推给订阅对应 directory 的流。三向（正向、反向、同 serve 跨 dir）均无串话。ocdeck 每任务 serve 订阅 `?directory=<worktree>` 只会收到该 worktree session 事件，SSE 回调落库无跨 directory 污染风险。

**事件 directory 字段存在性（关键，影响落库过滤实现）**：
- `session.created` / `session.updated` / `session.deleted`：**携带 `properties.info.directory`**（= session 归属目录，可信）。ocdeck 可在 SSE 回调落库前用此字段做**可信 directory 过滤**：仅当 `properties.info.directory` 等于当前任务 worktree 时才 upsert task_sessions，否则丢弃。这是防御性过滤——即便 SSE 流本身已隔离，事件自带 directory 字段提供第二层校验，防止任何未预期的跨流泄漏写入错误任务的归属表。
- `session.status` / `session.diff`：**无 `properties.info`，仅有 `properties.sessionID`，不带 directory 字段**。这类事件无法靠自身字段判断归属，ocdeck 必须用 `properties.sessionID` 反查 task_sessions 已有的 sessionID→worktree 归属记录：命中且归属匹配本任务才处理，未命中（孤儿事件）忽略（不写入归属表，避免凭空创建跨 directory 行）。

**对 ocdeck SSE 落库路径的影响评估**：
1. **SSE 流隔离本身可信**（实测三向无串话），ocdeck 经 `?directory=<worktree>` 订阅即可保证只收本任务事件，activate.go 无条件写入当前任务 task_sessions 在"流隔离"层面是安全的。
2. **但防御性 directory 过滤仍 MUST 做**（纵深防御，且处理 status/diff 类无 directory 字段事件）：SSE 回调按事件类型分流——
   - `created/updated/deleted`：校验 `properties.info.directory == 本任务 worktree`，不匹配则丢弃并告警（标志 SSE 隔离被破坏，属异常路径）。
   - `status/diff`：用 `properties.sessionID` 查 task_sessions 归属表，命中本任务才处理，未命中或归属他任务则忽略。
3. **design §4 "sessionID 均取 `properties.info.id`"** 对 status/diff 事件不适用（无 info）——这类事件用 `properties.sessionID` 取 sessionID（实测与 info.id 同值）。design §4/§20 应补注：sessionID 解析对 created/updated/deleted 取 `properties.info.id`，对 status/diff 取 `properties.sessionID`。
4. **不阻塞阶段一**：SSE 流隔离已实证，落库路径在"流隔离 + 事件 directory 字段过滤 + sessionID 归属表反查"三层下可信。防御性过滤是加固措施，非假设 (a) 的失败。

---

## 假设 (b)：session ID 捕获与 `--session` 恢复映射

### 验证步骤

1. 单 serve（端口 50300，独立密码），后台 SSE 订阅 `GET /event?directory=<dir>`。
2. POST 创建 session，观察 SSE 是否发出 `session.created`。
3. 解析 SSE envelope 形状，核对 `properties.info.id` 与 `properties.info.time.updated`。
4. `GET /session?limit=1000` 核对顶层字段 `id` / `time.updated`。
5. `GET /session/:id` 存在性预检（200 vs 404 形状）。
6. tmux 托管 `opencode attach --session <id>` 恢复（`-e OPENCODE_SERVER_PASSWORD` 注入），验证 TUI 接上并保持运行。
7. 删除该 session 后，`opencode attach --session <不存在的ID>` → 验证 exit 1 无回退。
8. `opencode attach --continue` → 验证恢复最近会话。（**历史验证记录**：2.10 起 ocdeck 已移除 --continue 路径，改为激活时经 REST 创建并锚定 session；本步骤结论仅作为 opencode CLI 行为实证保留）

### 观察结果

| 检查项 | 结果 | 契约对照 |
|---|---|---|
| SSE `session.created` envelope | `data: {"id":"evt_...","type":"session.created","properties":{"sessionID":"ses_...","info":{"id":"ses_...","slug":"...","version":"1.18.9","title":"...","time":{"created":...,"updated":...}}}}` | §20：`{type, properties:{info: Session}}`，sessionID=`properties.info.id` ✓ 一致 |
| `properties.info.id` = 创建返回 id | 一致（`ses_04ceced8bffe...`） | §4 ✓ |
| `properties.info.time.updated` 字段 | 存在（数值时间戳） | §4 "以事件 info.time.updated 刷新 last_seen_at" ✓ |
| 额外冗余字段 | `properties.sessionID`（与 info.id 同值）、envelope 顶层 `id`（事件 id） | §20 未提及但不冲突 |
| 初始事件 | `server.connected`（SSE 建连即发） | §20 未列，需 occlient 容忍 |
| `GET /session?limit=1000` 顶层字段 | `id`、`time.updated` 顶层存在（非嵌套） | §20 "顶层 id/time.updated" ✓ |
| `GET /session/:id` 存在 | 200 | ✓ |
| `GET /session/:id` 不存在 | 404 + `{"name":"NotFoundError","data":{"message":"Session not found: ..."}}` | §20 "404 = 不存在" ✓（body 形状为 NotFoundError 对象，非空 404） |
| `attach --session <存在ID>`（tmux 托管） | TUI 成功恢复并保持运行，渲染 OpenCode 界面（Orchestrator · K3 Auto、cwd=dirA） | §4 ✓ |
| `attach --session <不存在ID>` | **exit 1**（RC=1，无回退） | §20 "attach 遇错直接 exit 1 无回退，attach.ts:82-92" ✓ 一致 |
| `attach --continue` | 成功恢复最近会话（TUI 渲染、保持运行） | §4 回退路径 ✓（2.10 起已移除，改用锚定创建） |
| 运行中删除其 session 后 attach 行为 | attach **不退出**，TUI 仍显示界面 | §20 的 exit 1 仅针对**启动预检失败**，运行中 session 被远程删除不触发 attach 退出（ocdeck 需靠 serve 健康轮询 + 会话消失监视，而非 attach 自检） |

### 结论

**PASS**：session ID 捕获（SSE `properties.info.id`）、`GET /session?limit=1000` 顶层 `id/time.updated`、`attach --session` 恢复、删除后 attach exit 1 无回退、`--continue` 回退——全部与 design §4/§20 契约一致。（**2.10 变更注记**：ocdeck 后续移除 --continue 路径，无记录或预检 404 时经 REST CreateSession 创建并 `--session` 锚定；上述 --continue 结论为 opencode CLI 行为实证，仍然有效但不再被 ocdeck 使用）

**补充观察（非偏差，design 需纳入）**：
- SSE envelope 含冗余 `properties.sessionID` 与顶层事件 `id`，occlient 解析 sessionID 应统一取 `properties.info.id`（与 §20 一致），忽略冗余字段。
- `server.connected` 为 SSE 建连初始事件，occlient 须容忍（非 session 事件）。
- 404 body 为 `{"name":"NotFoundError","data":{...}}` 结构，occlient 区分 404 须以 HTTP 状态码为准（design §18 `GetSession` "404 区分于其他错误" 正确），body 形状不应作为类型判断依据。

---

## 假设 (c)：tmux 托管正确性

独立 socket `tmux -L ocdeck-test -f /dev/null`（及 `ocdeck-verify`），不使用生产 `ocdeck` socket。

### (c.1) `new-session -e` env 注入 + `show-environment` 读回

**步骤**：`new-session -d -s envtest -e MYTESTKEY=hello123 -e SECRET=leaked /bin/zsh`；会话内 `echo $MYTESTKEY`；`show-environment -t envtest MYTESTKEY`；宿主 shell 检查变量；长驻进程 `ps -o args` 查 argv。

| 检查项 | 结果 | 契约对照 |
|---|---|---|
| 会话内进程读 `$MYTESTKEY` | `hello123` ✓ | §2 env 注入 ✓ |
| 会话内进程读 `$SECRET` | `leaked` ✓ | §2 ✓ |
| 宿主 shell `$MYTESTKEY` / `$SECRET` | unset（不泄漏） ✓ | §2 "不继承宿主 env" 反向：注入不反向污染宿主 ✓ |
| `show-environment -t <session> KEY` 读回 | 输出 `MYTESTKEY=hello123`（含 key 前缀，需解析） ✓ | §2 "persist 重启恢复经 show-environment 读回 OPENCODE_SERVER_PASSWORD" **可行** ✓ |
| 长驻 `/bin/sleep 300` 进程 argv | 仅 `/bin/sleep 300`，**不含 `-e KEY=VALUE`** ✓ | §2 "不进入长期运行进程 argv" ✓ |
| `ps aux \| grep LONGSECRET` | 无匹配（秘密不在 argv） ✓ | §2 进程列表泄漏边界 ✓ |

**结论 PASS**：`-e` 注入会话内可读、宿主不泄漏、`show-environment` 可读回（design §2 密码恢复路径实证可行）、长期进程 argv 不含 env。`show-environment` 输出格式为 `KEY=VALUE`，ocdeck 读回时需 strip key 前缀。

### (c.2) attach 客户端断开 / 重连取回当前屏幕

**步骤**：tmux 会话内运行全屏程序 `vi`，输入标记文本 `SCREEN-MARKER-XYZ`；模拟客户端 detach（会话 pane 持久）；再次 capture-pane（等价 reattach）。

| 检查项 | 结果 |
|---|---|
| vi 运行时屏幕含 `SCREEN-MARKER-XYZ` | ✓ |
| detach 后（不杀会话）再次 capture | `SCREEN-MARKER-XYZ` 仍在 ✓ |
| 全屏程序状态保持 | ✓ |

**结论 PASS**：tmux 会话 pane 在客户端 detach 后持久保留，全屏程序屏幕状态完整，reattach 取回当前屏幕。design §12 "断线重连（tmux reattach）" 实证可行。

### (c.3) resize 传播

**步骤**：`new-session -d -s rtest -x 100 -y 30`；会话内 `echo $COLUMNS $LINES`；`resize-window -t rtest -x 150 -y 40`；再次 `echo`。

| 检查项 | 结果 |
|---|---|
| 初始 `list-windows` | `100x30` ✓ |
| 初始会话内 `$COLUMNS/$LINES` | `100/30` ✓ |
| `resize-window -x 150 -y 40` 后窗口 | `150x40` ✓ |
| 会话内 `$COLUMNS/$LINES` 跟随 | `150/40` ✓ |

**结论 PASS**：`resize-window -x -y` 改变窗口尺寸，会话内进程 `$COLUMNS/$LINES` 跟随更新。ocdeck WS 客户端 resize 帧 → `resize-window` 传播链路实证可行（design §18 pty resize / §21 resize 帧）。

### (c.4) reaper 收割逃逸子孙

**步骤**（按 design §2 reaper 协议 + §17 emdash 同款）：
1. `new-session -d -s reap /bin/zsh`；`list-panes -s -t reap -F '#{pane_pid}'` 取 pane_pid=34926。
2. 会话内 `nohup /bin/sleep 600 &`（macOS 无 `setsid` 命令，用 `nohup` 产生等价逃逸子孙：忽略 SIGHUP、reparent 到 init）。
3. 快照：`pgrep -P <pane_pid>` 递归 + `pgrep -f "sleep 600"` 取 pid=35197，`ps -o lstart=` 记录 startTime（身份校验字段）。
4. `kill-session -t reap`。
5. 对快照中幸存者做身份校验（pid + startTime 不变）→ TERM → （若仍存活）KILL。

| 检查项 | 结果 | 契约对照 |
|---|---|---|
| 快照阶段取到 pane_pid | 34926 ✓ | §2 "list-panes 取 pane_pid" ✓ |
| 快照取到逃逸子孙 pid+startTime | pid=35197, ppid=34926, lstart=Thu Jul 30 20:55:15 2026 ✓ | §2 "ps 收集子孙快照（pid+startTime）" ✓ |
| kill-session 后逃逸子孙存活 | pid=35197 存活，ppid 重父到 1（init） ✓ | §2 "kill 后幸存者 reparent 到 init 即不可达" ✓ |
| 身份校验（pid+startTime） | lstart 不变（同一进程） ✓ | §2 "身份校验 pid+startTime" ✓ |
| TERM 收割 | 首次因 survivors 文本解析问题未命中真实 pid；二次 `kill -9 35197` 成功 reap | §2 "先 TERM 宽限后 KILL" ✓（KILL 升级路径实证） |
| 最终全清理 | pgrep "sleep 600" clean ✓ | - |

**结论 PASS**：reaper 协议完整链路（list-panes 取 pane_pid → ps 收子孙快照含 startTime → kill-session → 幸存者 reparent init → 身份校验 → TERM→KILL）实证可行。`nohup` 进程可能需 KILL 升级（TERM 未必足够，取决于目标进程信号处理），design §2 "先 TERM 宽限后 KILL" 升级设计正确。

**macOS 约束（design §1.2 已限定 v1 仅 macOS）**：macOS 无 `setsid` 命令，reaper 测试用 `nohup` 等价模拟逃逸子孙；真实场景逃逸进程可能来自 dev server 的 daemon 化（同样 reparent init），收割机制不受影响。

### (c.5) `-f /dev/null` 跳过用户配置

**步骤**：`tmux -L ocdeck-test -f /dev/null new-session ...`；`show-options -g status-left`。

| 检查项 | 结果 |
|---|---|
| `-f /dev/null` 启动 | 成功 ✓ |
| `status-left` 为 tmux 默认值（`[#{session_name}] `） | ✓（未读用户配置） |

**结论 PASS**：`-f /dev/null` 使 tmux 跳过用户 `~/.tmux.conf`，使用内置默认。design §2 "`-f /dev/null` 跳过用户 `~/.tmux.conf`，防止 `remain-on-exit` 等配置破坏退出监视" 实证成立。

### (c.6) 无 tmux server 时 `kill-server` 退出码

**步骤**：对不存在的 server 执行 `tmux -L ocdeck-test -f /dev/null kill-server`。

| 检查项 | 结果 |
|---|---|
| stderr | `no server running on /private/tmp/tmux-501/ocdeck-test` |
| **退出码** | **1** |

**结论（契约要点，非偏差）**：无 server 时 `kill-server` 退出码 = 1。design §2 "启动时无 tmux server：空运行时，正常情况（无任何会话）" 与 §18 `KillSession` absent-at-entry 幂等短路：ocdeck 调用 `kill-server` / `kill-session` 时 MUST 区分"server/会话不存在"（exit 1 + stderr `no server`）与"真实 tmux 命令失败"，前者视为空运行时幂等成功，不得当致命错误。`HasSession` 同理须解析 stderr 区分二者（design §18 `HasSession` "区分会话不存在与 tmux 命令失败" 实证必要）。

### (c.7) 最后 pane 退出后会话 / server 消失语义

**步骤**：`new-session -d -s panetest /bin/zsh`；`has-session` = ALIVE；会话内 `exit`；再次 `has-session`。

| 检查项 | 结果 |
|---|---|
| pane exit 前 has-session | ALIVE ✓ |
| pane exit 后 has-session | `no server running`，会话消失 ✓ |
| pane exit 后 list-sessions | `no server running` |
| **tmux server 状态** | **最后一个会话消失后 tmux server 自动退出** ✓ |

**结论（契约要点）**：单会话 tmux server 在最后 pane 退出后会话消失，**且整个 tmux server 随之自动关闭**。这是 design §2 "监视期间 tmux server 消失 → 全局 runtime-loss 事件——server 消失意味着全部注册会话已丢失" 的实证依据：ocdeck 专属 socket 上的 server 生命周期绑定到最后一个会话；serve/shell 进程退出导致会话消失，连锁使 server 消失，TaskManager 经 has-session 轮询探测到 server 不在 → 全局 runtime-loss 收敛。退出监视（§2 "1-2s 周期 has-session 轮询"）须能区分"单个会话消失"与"server 整体消失"（二者 stderr 相同 `no server running`，ocdeck 需结合上下文判断）。

### (c.8) tmux target 语法细节（实现注意）

实测 `-t =<name>`（`=` 前缀禁用模糊匹配，design §2 要求）行为：

| 命令 | `-t '=name'` | `-t 'name'` |
|---|---|---|
| `has-session` | ✓ ALIVE 正确 | 亦可（但无禁用模糊匹配保护） |
| `list-panes -s` | **失败** `can't find pane` | ✓ 成功 |
| `capture-pane` | **失败** `can't find pane` | ✓ 成功 |
| `kill-session` | ✓ 成功 | ✓ 成功 |

**实现注意**：`=` 前缀在 `has-session` 上正确生效，但在 `list-panes`/`capture-pane` 上会找不到 pane。ocdeck 须对 `list-panes`/`capture-pane` 使用 `name`（无 `=`）或 `name:0` 形式，`has-session`/`kill-session` 使用 `=name`。design §2 "全部 tmux 操作 MUST 使用精确 target `-t =<name>`" 在 `list-panes` 上需调整：取 pane 用 `list-panes -s -t <name>`（会话名），pane 级操作用 pane_id。**这是 design §2 的实证细化点，非偏差**（安全意图不变，仅 target 语法按命令适配）。

---

## 总结

| 假设 | 结论 | 阻塞阶段二？ |
|---|---|---|
| (a) 多 serve 并发访问全局 DB 无锁错误/串话 | **PASS**（无锁错误/无状态损坏；列表 + SSE 流无串话，单条 GET 全局可读；SSE 三向隔离实证） | 否 |
| (b) session ID 捕获与 `--session` 恢复映射 | **PASS**（SSE envelope / 顶层字段 / attach 恢复 / exit 1 / --continue 全部一致；2.10 起 ocdeck 改用锚定创建，--continue 不再使用） | 否 |
| (c) tmux 托管正确性 | **PASS**（env 注入+读回 / 断开重连取回屏幕 / resize 传播 / reaper 收割 / -f /dev/null / server 消失语义 全部实证） | 否 |

**三个架构假设全部 PASS，阶段一门禁（架构假设部分）通过，可进入阶段二。**

## 发现的契约偏差 / 需 design 补注项

1. **`GET /session/:id?directory=<wt>` 的 directory 参数不生效**（假设 a）：session ID 跨 directory 全局可读，directory 仅在列表端点 `GET /session` 过滤。occlient.GetSession 预检仍可用（404 判断存在性正确），但 ocdeck 不得依赖单条 GET 的 directory 做隔离边界；directory 隔离由 ocdeck 经列表 + session ID 归属表自行实现。design §20 该行应补注此实测。

2. **SSE envelope 含冗余 `properties.sessionID` 与顶层事件 `id`**（假设 b）：与 §20 不冲突，occlient 解析 sessionID 统一取 `properties.info.id` 即可，但 design §20 可补记完整 envelope 形状（含 `server.connected` 初始事件、冗余 sessionID 字段）。

3. **SSE session 事件 directory 字段存在性分化**（假设 a SSE 补做）：`session.created/updated/deleted` 携带 `properties.info.directory`（可信归属），`session.status/diff` 无 `properties.info`、仅 `properties.sessionID`（不带 directory）。design §4/§20 "sessionID 取 `properties.info.id`" 对 status/diff 事件不适用——这类事件须取 `properties.sessionID`（与 info.id 同值）。ocdeck SSE 落库前 MUST 按事件类型分流：created/updated/deleted 校验 `properties.info.directory == 本任务 worktree`，status/diff 用 sessionID 反查 task_sessions 归属表。

4. **无 server 时 `kill-server` / `has-session` 退出码 = 1 + stderr `no server running`**（假设 c.6）：design §2 / §18 `HasSession`/`KillSession` 须显式区分"server 不存在"（幂等成功 / 空运行时）与"tmux 命令失败"（基础设施错误），不得把 exit 1 当致命错误。

5. **最后会话消失后 tmux server 自动关闭**（假设 c.7）：design §2 "server 消失 → 全局 runtime-loss" 实证成立，但 has-session 轮询须能从同一 stderr `no server running` 区分"单会话消失"vs"server 整体消失"（需结合轮询上下文/多次探测）。

6. **`-t =<name>` 在 `list-panes`/`capture-pane` 上找不到 pane**（假设 c.8）：design §2 "全部 tmux 操作 MUST 使用 `-t =<name>`" 需细化：`has-session`/`kill-session` 用 `=name`，`list-panes`/`capture-pane` 用 `name` 或 `name:0` / pane_id。安全意图不变。

7. **macOS 无 `setsid` 命令**（假设 c.4）：reaper 测试用 `nohup` 模拟逃逸子孙；真实场景 dev server daemon 化同样 reparent init，收割机制不受影响。design §1.2 已限定 v1 仅 macOS，此处为 macOS 实现约束记录。

以上 1 项为契约偏差（GET /session/:id directory 不生效），3 项为 SSE 事件字段/落库过滤补注（不阻塞），其余为实证细化点 / 实现注意，均不阻塞阶段一假设通过。