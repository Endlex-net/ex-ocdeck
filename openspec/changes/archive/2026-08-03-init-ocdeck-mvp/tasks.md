# Tasks: init-ocdeck-mvp

> 实施策略见 design.md §16/§22：阶段一纵向链路验证三个架构假设，**任一假设未通过不得进入阶段二**。
> 依赖顺序：存储/配置 → git/worktree → pty/process → occlient → TaskManager 编排 → WS → 前端。
> [P1] 标记的测试是阶段一门禁的组成部分。

## 阶段一：纵向链路（架构假设验证）

目标：建 worktree → 起 serve+attach → 捕获 session ID → WS 断线恢复 → 挂起/恢复 → 完整删除，全程可用。

- [x] 1.1 Go module 初始化与布局：cmd/ocdeck-server + internal/{api,task,process,pty,worktree,git,opencode,store,config}（依赖方向见 design.md §18）
- [x] 1.2 服务端配置与启动：监听地址（默认 127.0.0.1）、OCDECK_TOKEN（未配置拒绝启动）、数据目录、端口范围、**shutdownPolicy（persist|kill_on_start|kill_immediate，默认 persist）**、dataDir 单实例 flock；启动时 `opencode --version` 记录（二进制缺失拒绝启动）、`tmux -V` ≥ 3.2 校验（不满足拒绝启动）；**激活门禁为能力探测**（版本不匹配仅告警，探测不兼容才拒绝激活，design §11）；**v1 仅支持 macOS/Darwin**（tmux + reaper ps 解析）
- [x] 1.3 SQLite 存储层：modernc.org/sqlite + sqlc + 嵌入式 migration；foreign_keys/busy_timeout；文件 0600；schema 按 design.md §8（task_sessions/env_vars ON DELETE CASCADE）
- [x] 1.4 token 认证中间件：REST Bearer + WS 首消息认证（design.md §21 统一错误结构）
- [x] 1.5 项目注册 API：git repo 校验、默认分支探测、CRUD
- [x] 1.6 worktree 管理：`<dataDir>/worktrees/<projectID>/<taskID>/` 创建（每 repo 写锁、check-ref-format、add 前 prune）；删除（EvalSymlinks 包含性校验、dirty 检查、分支占用检查、prune）
- [x] 1.7 git 封装：白名单 argv、context 取消、有界输出、porcelain v2 -z 流式解析（10000 上限）
- [x] 1.8 process 层：tmux 专属 socket（-L ocdeck -f /dev/null，TMUX_TMPDIR=<dataDir>/tmux，exec env 清洗）、命名会话创建（new-session -d -c -e，argv 白名单单引号转义，精确 target -t =name）、KillSession（reaper 快照→kill-session→逃逸子孙身份校验 TERM→KILL，结构化部分失败返回）、HasSession (bool,error) 区分 tmux 故障、has-session 轮询退出监视（返回 cancel 句柄）、watchdog FSM（spawn 失败拒绝启动、全局 reaper kill 路径、自身死亡重启降级、StopWatchdog ack 握手，仅 kill_immediate）；pty 层：attach 客户端 PTY（creack/pty）、16ms 批量刷新、resize
- [x] 1.9 occlient：health/version、/session/status、/session 列表、DELETE /session/:id（404 幂等）、SSE /event 订阅（properties.info.id 解析、断流退避重连+全量对齐）；OpenCode 1.18.9 contract fixture
- [x] 1.10 TaskManager：状态机（design.md §5 含 creating/creation_failed）、keyed mutex + 409、§19 副作用边界表逐操作实现（意图落库→副作用→提交点→补偿）、启动 reconciliation（§5 全状态矩阵；persist 恢复序列：show-environment 密码恢复→探活→SSE 订阅缓冲→全量对齐→重放→重建 RuntimeGroup/env 快照）
- [x] 1.11 激活流程：serve tmux 会话（端口策略 §3、随机密码经 -e env）→ 健康检查+能力探测 → SSE 订阅+全量对齐 → tui tmux 会话（launcher 不变量、密码 -e 注入、--session 锚定契约；2.10 起 --continue 已移除，见 2.10）
- [x] 1.12 WS 终端桥：auth+初始尺寸握手（以握手尺寸创建 attach 客户端 PTY）、二进制 IO、resize（tmux 自动传播）、单交互客户端替换（4009）、慢客户端断开、断开仅杀 attach 客户端、重连新建 attach 客户端取回当前屏幕
- [x] 1.13 挂起/恢复/归档/删除：按 §19 表实现（删除前置检查先于任何副作用、deletion_failed 幂等重试、Force 模式）
- [x] 1.14 最小前端：React+Vite+TS；项目/任务列表 + xterm.js 终端页（off-screen host、预 resize、重连）；`/server/status` 集成（watchdog degraded banner、opencode 版本未验证告警）；embed.FS 内嵌
- [x] 1.15 **架构假设验证记录**：(a) 多 serve 并发访问全局 OpenCode DB 无锁错误/串话 (b) session 捕获与 --session 恢复正确 (c) tmux 托管正确性（-e env 注入且 `show-environment` 可读回、断开/重连取回当前屏幕、resize 传播、reaper 收割逃逸子孙、无 tmux server 时 kill-server 退出码、-f /dev/null 跳过用户配置、最后 pane 退出后会话消失语义）——结论写入 changes 目录验证记录

### 阶段一测试（门禁）

- [x] 1.16 [P1] 单元测试：状态机流转、keyed mutex 冲突、env 合并优先级与基础集/tmux exec env 清洗、**env 快照不含 role-specific 密码（OPENCODE_SERVER_PASSWORD 不进 tasks.env_snapshot）**、tmux 命令转义构造、token 中间件、porcelain 解析（mock 边界：CommandRunner/TmuxBackend/OCClient/clock/port allocator）
- [x] 1.17 [P1] failpoint 测试：§19 每个副作用步骤的失败注入（Create 各步、serve 会话起一半、删除中途失败、SSE 断流、Suspend 三分支决策树、watchdog 启动前崩溃窗口、Force 删除中途失败按持久化 delete_mode 恢复、reaper 快照失败不 kill 且可重试、**SnapshotFailed 后会话在重试前自行消失→转 snapshot_missing_degraded 而非清除 notice**、kill 部分失败产生 cleanupTickets、**Delete 意图顺序：置 deleting→RetryReap→remaining 非空落 deletion_failed 不越提交点、Force 不跳过进程收割**）、reconcile 矩阵补例（健康 serve+remaining debt→kill runtime→suspended、creating×任意→creation_failed、creation_failed 清理异常会话保持状态、**suspending+健康 serve persist reconcile→完成清理落 suspended 不恢复活跃**、**deletion_failed+一次性 serve persist reconcile→kill 会话保持状态**、**archived/deletion_failed 无 runtime 时 persist reconcile 保持原状**、**persist 恢复中途失败→停 SSE+kill runtime→suspended 不留无人托管 serve**、**KillSession disposition 五值：clean/retryable_snapshot_failed/retryable_kill_failed/retryable_reap_failed/snapshot_missing_degraded**、**snapshot_failed 无 tickets+会话消失+立即 Activate 必须被拒绝（notice 存在性门禁）**、**kill_failed 后会话自行消失→RetryReap 完整快照 tickets 后方可清 notice**、**absent-at-entry 幂等短路**），验证补偿与幂等 Retry
- [x] 1.18 [P1] OpenCode 契约测试（基准 1.18.9）：fixture 固化 health/session 列表（顶层 id/time.updated、limit 行为、>1000 sessions 溢出路径）/DELETE 200+true/status 三值枚举/SSE envelope 形状，结构漂移即失败；含"能力探测不兼容被拒绝"与"版本不匹配仅告警且放行"两条路径
- [x] 1.19 [P1] 集成测试：真实 tmux 会话创建/终止、WS 断开重连取回当前屏幕、reaper 收割 setsid 逃逸子孙、RetryReap ticket 重试、watchdog 杀全组（kill -9 主进程模拟，含全局 reaper 收割）、watchdog 自身死亡重启降级、正常关停顺序（watchdog 存活期清理会话→StopWatchdog→退出，模拟窗口期 kill -9）、监视期间 tmux server 崩溃收敛 suspended、三档 shutdownPolicy 的 reconcile 恢复/清理（persist 含 show-environment 密码与 OCDECK_SERVE_PORT 恢复、activating 崩溃端口未写回场景、env 快照恢复原值不重新合并、cleanup debt pre-pass 阻止恢复 active）、孤儿会话清理及失败的后台周期重试、**后台周期重试完整链路（sessionName→KillSession→合并 tickets→RetryReap→keyed mutex+CAS 原子清 notice）**、TMUX_TMPDIR 隔离与测试并行安全、单实例锁、serve 会话异常消失触发完整运行时清理、并发 ReopenAttach 幂等、残留会话/cleanup debt 激活门禁

## 阶段二：横向扩展（阶段一门禁通过后才开始）

- [x] 2.1 shell 终端：每任务多 shell tmux 会话（cwd=worktree、-e 注入任务 env、attach 客户端接入），随挂起终止
- [x] 2.2 git 状态/diff API：numstat 统计、unified diff、字节数/文件数限制
- [x] 2.3 commit/push API：错误透传、push -u
- [x] 2.4 env 管理：项目级/任务级 CRUD + 合并优先级 + UI 提示（重启生效/明文风险）
- [x] 2.5 全局配置管理：~/.config/opencode/ 下 *.json 与 *.jsonc 列表/读取/保存（按扩展名分流语法校验、mtime+hash 乐观并发、原子 rename、.bak、symlink 拒绝、受影响任务提示）
- [x] 2.6 git 面板前端：status/diff（diff2html 转义渲染）/commit/push
- [x] 2.7 env 与全局配置编辑前端
- [x] 2.8 任务列表状态展示：serve /session/status 集成（idle/busy）
- [x] 2.9 全局环境变量：global_env_vars 表（key/mode/value，mode ∈ follow_host|manual）+ 合并层级（基础集<全局级<项目级<任务级；follow_host 激活时从服务端进程 env 解析、宿主未设置跳过；快照存解析后最终值）+ /env API（reserved key 校验同其他层）+ 前端编辑（模式选择、follow_host 展示服务端当前解析值）+ 单元测试
- [x] 2.10 激活即锚定 session：激活时无记录或预检 404 → occlient.CreateSession（POST /session，title=任务名）→ 持久化 task_sessions → attach --session <newID>；移除 --continue 路径（design §4/§19/§20、task-lifecycle spec 已更新）+ 单元测试
- [x] 2.11 创建即自动激活：Create 成功落 suspended 后异步触发 Activate 完整流程（经全部激活门禁与 session 锚定；失败落 suspended+last_error 可手动重试；服务端重启 reconcile 不重复触发）+ 单元测试（design §19 Create 行、task-lifecycle spec 创建 Scenario 已更新）

## 最终验证

- [x] 3.1 阶段二功能单元测试（git API、env CRUD、配置管理）
- [x] 3.2 端到端冒烟：注册项目→建任务→激活→终端交互→commit/push→挂起→恢复（会话历史在）→删除（worktree/分支/session 清理）
- [x] 3.3 `openspec validate init-ocdeck-mvp --strict` 通过
