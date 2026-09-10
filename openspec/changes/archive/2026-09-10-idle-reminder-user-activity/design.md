## Context

现状（均已按代码核实）：

- idle 计时由 `Notifier.onRunStatusChanged` 在 `busy→idle + available=true` 时武装（`internal/application/notification/triggers.go:135-141`），`scan()` 以 10s tick（`notifier.go:25` `defaultTickInterval`）判定 `idleSince + idle_timeout_seconds`（默认 60s，`internal/domain/notification/config.go:79`）届满后触发；届满时立即消费计时（`triggers.go:203-207`：`idleSince=nil`、`idleSuppressed=true` 后 `fireIdle`）。
- 代码中不存在任何用户活动信号源：领域事件目录（`internal/domain/event/event.go:29-66`）为闭合枚举，无用户交互类事件；终端 WS 输入转发（`internal/api/ws_terminal.go:237-260` `pumpWSToPTY`）不发布事件；git handler（`internal/api/git.go`）不发布事件（grep 零命中）。
- 事件发布先例：应用层经窄 `application.Publisher` 端口（`internal/application/ports.go:28-30`）发布 typed 事件（如 `apptask` 的 `lifecycle.go:90`）；bus 单例经窄接口注入（`cmd/ocdeck-server/main.go:134-139`）。
- API 层 `Server.tasks` 为 `TaskBackend` 接口（`internal/api/tasks.go:16`），具体实现为 `task.Manager`（`main.go:203` `srv.SetTaskBackend(tm)`）。
- shell 终端 ID 可解析出任务 ID：`taskIDFromSessionName` 定义于 `internal/task/util.go:109`（内部调用 canonical parser `process.ParseSessionName`，`internal/infrastructure/process/process.go:208`）；既有 shell 校验的调用点见 `internal/task/attach_shell.go:306`。
- `Notifier` 已订阅 `TopicServeRuntime` 与 `TopicTask`（`notifier.go:140-142`），按 Type 字面量分发。
- **既有 SSE 消费过滤对未知事件并非忽略**：`eventDirtiesTaskDetail`（`internal/api/task_filter.go:71-72`）与 `eventDirtiesActiveSessions`（`internal/api/sessions_filter.go:71-74`）的 default 分支对未知 Type 保守标脏；`eventDirtiesProjectsTaskTree`（`internal/api/projects_filter.go:26-27`）对所有事件标脏；标脏后按 500ms 窗口重组装全量快照重推（`internal/api/read_model_stream.go`）。新事件类型 MUST 显式进入各过滤表，否则用户输入会引发无关的 SSE 全量重推。

## Goals / Non-Goals

**Goals:**

- 为通知模块提供按任务归属的用户主动操作信号，覆盖终端/shell 键盘输入与 git 页面区域内的用户主动交互。
- idle 计时取消的行为契约归 spec delta「通知触发——空闲超时（idle）」/「用户主动操作识别」（含串行判定顺序语义），本文不重复表述，仅定义机制。
- 新事件类型不引发既有 SSE 场景的无关重推。

**Non-Goals:**

- 不改变事件总线、投递链路、配置与渠道的任何既有行为。
- 不引入持久化：主动操作信号为进程内存态事件，与既有触发器状态一致（spec「通知抑制、启动基线与对账」）。
- 不做全局活动追踪/统计，信号仅服务于 idle 计时取消。
- 不修复既有的 spec↔代码漂移（`serve_runtime.session_error` 已存在于代码过滤表但未进入 stream spec 决策表——另行处理，见 Risks）。

## Decisions

### D1：活动信号载体 = 新领域事件 `task.user_activity`（显式扩展闭合事件目录）

新增事件类型 `TypeTaskUserActivity = "task.user_activity"`，`Topic=TopicTask`，`RID=taskID`（`Event` 信封无 TaskID 字段，`event.go:75-80`；任务归属由 RID 承载），`Payload=struct{}{}`。目录扩展遵循既有先例（task-notifications 对 `serve_runtime.session_error` 的显式增量，`event.go:59-63`），并在常量注释中标注本 change 的增量来源。

- 为什么选事件而非共享"最近活动时间"注册表：既有触发器架构为事件驱动 + 单 run loop 串行化（spec「通知抑制、启动基线与对账」），事件载体天然满足"判定在单一串行化上下文执行"的不变量；共享注册表会引入跨 goroutine 可变状态，偏离该不变量。
- 为什么挂 `TopicTask` 而非新 topic：主体为 task 聚合，`Notifier` 已订阅该 topic，无需新增订阅。
- **消费者影响（区别于"未知 Type 忽略"的错误假设）**：三个 SSE 场景过滤函数 MUST 显式新增 `task.user_activity` 分支——合法事件（Topic `task` 且 Payload `struct{}{}`）不标脏，Topic/Payload 畸形沿用各场景保守标脏惯例（契约见 task-detail-stream / active-sessions-stream / projects-stream 的 spec delta）。事件仍进入各订阅缓冲：极端频率下溢出会触发既有对账/自愈重推，为已接受代价（见 D6）。
- 发布前提例外：`task.user_activity` 是一次性用户输入事实，不对应任务行变更——不受 active-sessions-stream「内部事件总线」"状态真实变更落账后发布"通则约束（该通则的 delta 已显式列出本例外），且 MUST NOT 为满足通则额外推进 `updated_at` 或产生 `task.activity_changed`。

### D2：消费语义 = 仅取消已武装的 idle 计时（lookup-only，不建状态）

`Notifier.handleEvent` 新增 `task.user_activity` 分支：`taskID := ev.RID`，仅查现有任务状态（不创建状态、不读快照），`idleSince != nil` 时置 `nil`。活动事件仅按任务归属，不携带实例或周期标识；迟到事件可能取消处理时该任务当前已武装的计时——处理结果遵循本 change 已确认的串行消费顺序（spec delta「通知触发——空闲超时（idle）」，用户决策 DR1），不额外引入 fencing 或代际机制。其余字段（retryDeadline/errorDeadline/episode/去重集合/抑制态）一律不动。

- 时序语义以 spec delta「通知触发——空闲超时（idle）」的串行判定规则为唯一表述：以 Notifier 串行循环处理顺序判定，处理活动时存在尚未消费的 idle 计时则取消；计时先被消费则本周期结果不变；无计时不缓存活动。近边界一次误发或漏发为既有已接受语义（spec「通知抑制、启动基线与对账」的残余竞态窗口条款），本 change 不引入 revision/代际机制，也不承诺严格时间误差上界。
- 为什么不需要 `idleSuppressed = true`：armed 状态的唯一来源是 `onRunStatusChanged` 的 `busy→idle` 迁移（triggers.go:136），取消后任务保持 idle 不会自动重新武装，与 spec「主动操作取消后持续空闲不再触发」一致；抑制态只服务于"已触发后"场景。
- 任务非 active 或无既有状态时事件为 no-op（spec「无已武装 idle 计时任务的主动操作无效果」）。

### D3：终端/shell 输入识别点 = WS→PTY pump 的输入帧（服务端判定，无需前端改动）

在 `pumpWSToPTY`（`ws_terminal.go:237-260`）的读取循环内，处理顺序唯一确定为：

```
读帧失败/ctx 取消 → 退出（既有行为）
  ↓
text 帧先按既有逻辑尝试 resize 控制帧解析：命中 resize → 仅调 Resize，不计活动
  ↓
非空 binary 帧 / 非空非 resize text 帧 → 先上报活动，再沿用既有 PTY 写入与错误退出逻辑
  ↓
空帧（零长度 payload）→ 不上报，仍按既有路径处理
```

- 活动代表"收到用户输入"，不以 PTY 写入成功为业务条件（用户决策 DR2）——写入失败不撤销已观察到的输入；识别点在写入之前。
- 上报回调由 handler 层注入：`handleWSTUI` 持 path 中的 `taskID`，直接回调 `RecordUserActivity`；`handleWSShell` 持 `tid`，回调 `RecordShellUserActivity`（由 Manager 内部经 `taskIDFromSessionName` 解析任务 ID，解析失败静默忽略，不导出解析函数）。bridge/pump 签名扩展为传入回调，既有 WS bridge 测试入口（`internal/api/ws_bridge_test.go`）需适配。
- 备选（拒绝）：前端在 keydown 时上报——多一层前端契约且可能被移动端 IME/粘贴路径漏报；服务端输入帧是键盘输入的权威边界。

### D4：git 用户手势 = 前端显式上报专用端点（不从既有 git API 推断）

新增 `POST /api/v1/tasks/{id}/activity`：空请求体；鉴权与错误信封沿用既有 tasks 路由。处理链唯一确定为：

```
handler: TaskBackend.Get(ctx, taskID) 存在性校验
  ├─ 错误 → 经既有 mapTaskErr 映射返回（MUST NOT 发布任何活动事件）
  └─ 成功 → TaskBackend.RecordUserActivity(ctx, taskID)（无返回值）→ 204
```

- 存在性校验复用既有 `TaskBackend.Get`（`tasks.go:27`）及其错误语义：`Manager.Get` 当前将底层读取错误统一映射为 `not_found`（`internal/task/crud.go:793`），本设计沿用该语义，不声称区分记录不存在与存储故障。
- 任务存在但非 active：不新增错误分支，照常 204；消费端无该任务状态/无计时自然 no-op（D2）。
- 发布不等待任何消费者完成；总线溢出沿用既有对账/自愈机制，本端点不感知。
- 备选（拒绝）：新增带返回错误的校验发布入口——需与 WS 快速上报路径分清职责，当前规模下不必要。

**前端事件表**（GitPanel 区域，`web/src/components/GitPanel.tsx`；用户决策 DR3：范围为整个 git 区域的主动交互）：

| 计为主动操作 | 不计 |
|---|---|
| 点击（按钮、文件项、提交等） | 激活时自动加载（GitPanel.tsx:144 区域既有行为） |
| 滚轮/触摸翻阅 | 定时/自动刷新产生的请求 |
| 键盘翻页 | 程序滚动（如 `scrollIntoView`） |
| 编辑输入（如提交信息输入框） | 其他非用户触发的事件 |

- 接入方式：在 GitPanel 区域容器上以**捕获阶段**集中监听用户输入类 DOM 事件（`click`/`wheel`/`touchstart`/`touchmove`/`keydown`/`input`；`wheel`/`touchmove` 使用 passive 监听，MUST NOT 阻止默认滚动），嵌套组件（ReviewPanel、DiffViewer 等）复用同一上报入口；MUST NOT 在 `loadStatus`/`openDiff`/`refreshDiff` 等请求函数内推断手势来源。
- 持续触摸翻阅由 `touchmove` 覆盖：武装前的 `touchstart` 按"无计时不缓存"丢弃，武装后用户继续主动移动时，后续 `touchmove` 事件被处理时计时仍存在即取消（spec scenario「武装后持续触摸移动取消计时」）；程序 `scroll` 事件 MUST NOT 计为活动。
- 归属取手势发生时的 `taskID`；上报 fire-and-forget（fetch 忽略响应与错误），不等待任何 git 操作成功。
- 备选（拒绝）：给既有 git API 加 `X-User-Gesture` header——污染既有契约，且轮询与手势命中相同 endpoint，后端无法可靠区分。

### D5：发布入口 = TaskBackend 新增两方法 + Manager 窄 Publisher 端口注入

`TaskBackend` 接口（`internal/api/tasks.go:16`）新增两个方法，`task.Manager` 实现：

```go
RecordUserActivity(ctx context.Context, taskID string)
RecordShellUserActivity(ctx context.Context, tid string)
```

- `RecordUserActivity`：发布 D1 事件（`ocdeckevent.NewTaskUserActivity(taskID)` 构造器，`event.go` 增量）。
- `RecordShellUserActivity`：内部 `taskIDFromSessionName(tid)` 解析任务 ID，解析失败静默忽略（非法 tid 不产生任何效果），成功则发布同一事件。
- Publisher 注入：`task.Options` 新增可选字段 `Publish application.Publisher`（沿用 P1.6.5 窄接口注入模式），在 `main.go:145` 的 `task.New` 中传入现有同一个 bus；未注入（测试）时为 no-op，不改变任何既有路径行为。本项目为 main.go 手动 composition root，无 Fx module 变更。
- 为什么不从 API 层直接发布：API 层当前不持有 Publisher，事件发布是应用/任务层职责（既有全部发布点均在 `apptask`/`task`）；保持该边界。
- 幂等与无副作用：重复上报只是重复发布同一事件，消费端为 lookup-only 置空，天然幂等。

### D6：git 前端上报采用 leading+trailing 源端节流（2s 窗口，用户决策 DR4）

消费端处理为 O(1) map 查找 + 置空，进程内 bus 直投。git 区域的滚轮/触摸事件频率远高于纯键盘输入，实测产生大量 activity 请求（人工 review 反馈），故前端上报入口采用 leading+trailing 节流（2s 窗口，用户决策 DR4）。终端/shell WS 输入不节流（键盘输入频率天然低，且为进程内上报不经 HTTP）。节流后持续交互下每标签页约每 2s 至多一次请求（相对逐事件发送降频约 97%+）；事件仍进入各订阅缓冲，极端频率下溢出触发既有对账/自愈重推，为已接受代价（非"其他订阅方完全不受影响"）。

节流规则（唯一确定，避免不同实现解释）：

1. 六类可信手势共用一个上报入口与同一份节流状态；`isTrusted=false` 不发送，也不改变节流状态。
2. 距上次发送已满 2s 的手势立即发送（leading）。
3. 冷却期内的新手势仅置一个 `pending` 标记，并安排在允许发送时刻补报一次（trailing）；后续手势不继续推迟该时刻。
4. 补报后清空 `pending`；无新手势则无发送——孤立一次手势只发一次，不凭空生成活动。
5. `pending` 绑定手势发生时的 taskID；任务切换、组件卸载或页面进入 hidden 时，对已存在的 `pending` 尽力补报一次后清理定时器；无 `pending` 不发送。
6. 断网/发送失败不排队、不重放旧手势；失败的发送仍计入节流窗口（避免逐事件重试）；恢复后由新的真实手势继续上报。
7. 间隔以单调时钟衡量（如 `performance.now()`）。

语义影响（如实陈述，不声称与逐手势即时发送等价）：

- 纯 leading 节流存在跨武装边界的确定性漏取消（busy 期间手势上报无效 → 随后进入 idle 武装 → 冷却期内最后手势被丢弃 → 仍提醒）；trailing 补报闭合该缺口：冷却期内出现过的手势到点必补报一次。
- 合并等待为冷却期内手势引入至多约一个窗口（2s，为正常调度下的目标等待时间而非硬上界——后台/冻结状态下浏览器会节流定时器）的延迟；尾部信号仍可能晚于计时消费、或取消到达时该任务新武装的周期——沿用 DR1 串行消费顺序与近边界已接受误差，不承诺严格送达时限。
- 节流仅作用于前端 HTTP 上报入口（GitPanel 监听回调），不改变"什么算主动操作"的识别契约（spec「用户主动操作识别」不变）。

- 溢出后的既有恢复语义影响需如实陈述：`onOverflow`（`notifier.go:334-344`）会清空**全部任务**的 idle/retry/error 计时并进入 reconciling；`attemptReconcile`（`notifier.go:372-376`）重建时不恢复 idle/error 计时、retry 重新起算、仅播种 pending attention。即高频活动事件引发溢出时，代价不止 SSE 全量重推，还包括既有全局计时重置语义——该语义为既有行为，非本 change 新增。
- 「仅取消本任务 idle 计时」描述的是活动事件**正常消费**路径的直接效果；异常溢出路径仍执行上述既有全局恢复语义，两者不矛盾。

- 历史决策：v1 曾定"不做源端节流"并将源端窗口合并记录为可后补备选；人工 review 实测事件量后先暂写 5s 纯 leading，经方案咨询发现纯 leading 的跨武装边界漏取消缺陷，修正为 2s leading+trailing（DR4），识别契约不变。

### 主流程（局部变体共享一条主链）

```
用户操作
  |-- 终端/shell 键盘输入 --> WS 输入帧 --> pumpWSToPTY 识别（D3，resize/空帧除外）--+
  |-- git 区域手势 --------> POST /tasks/{id}/activity                                |
  |                           ├─ Get 校验失败 → 错误返回，MUST NOT 发布（D4）          |
  |                           └─ 成功 → RecordUserActivity --------------------------+
                                                                                     v
                                                              发布 task.user_activity（D1/D5）
                                                                                     v
                                                              Notifier.handleEvent（D2）
                                                                                     v
                                                          idleSince != nil ? 置 nil : no-op
```

### 实现顺序

无需分阶段上线；建议实现顺序（每批保持可编译）：**契约及过滤**（event.go 新类型 + 三个 SSE 过滤分支）→ **发布/消费与接线**（Manager 方法 + Publisher 注入 + Notifier 分支）→ **HTTP/WS**（activity 端点 + pump 回调）→ **Git 前端**（GitPanel 捕获监听）→ **跨层验证**。

### 验证矩阵

| 层 | 覆盖点 | 手段 |
|---|---|---|
| Notifier | 活动取消计时、取消后持续 idle 不触发、重新武装、跨任务隔离、retry/error/episode/去重集合不受影响、无状态 no-op、迟到活动事件命中同任务当前已武装计时的串行顺序 | 既有 fake clock + 端口注入（`notifier.go:27` Options） |
| Manager | 事件信封（Topic/Type/RID/Payload）、Publish 未注入 no-op、shell tid 归属解析与非法 tid 静默 | 记录型 Publisher double |
| API activity 端点 | Get 失败零发布 + 错误映射；成功 204 且发布一次；非 active 任务 204 | TaskBackend stub |
| WS bridge | binary/非 resize text 上报、resize/空帧不上报、PTY 写失败仍上报 | 既有 bridge 直测入口适配新签名 |
| SSE 过滤 | 三场景：合法 user_activity 不标脏、畸形标脏 | 过滤纯函数表驱动用例 |
| 前端 GitPanel | 手势（含嵌套组件与持续触摸翻阅 `touchmove`）调用上报、自动加载/刷新/程序滚动不调用、leading+trailing 节流（首次立即、冷却期 pending 到点补报一次且不推迟、孤立手势不重复、切任务/卸载/hidden 尽力补报 pending、程序事件排除且不影响节流状态） | 组件测试 |
| 总线溢出 | 活动事件导致订阅溢出时执行既有恢复语义（`onOverflow` 清空全部计时 + `attemptReconcile` 重建，D6） | 既有溢出对账测试扩展事件类型 |

### 既有逻辑无需前置重构

`Notifier` 状态机、事件目录、WS bridge、TaskBackend 均为增量扩展点，无需要先收敛/替换的既有逻辑；不变量保持不变：单 run loop 串行化、组合快照、副作用门禁（本 change 不新增任何投递路径）。

## Risks / Trade-offs

- [前端漏报 git 手势（新加的 git 交互忘记接上报点）] → 上报点集中在 GitPanel 区域容器的捕获监听（D4），新增交互自动落入；测试以"嵌套组件手势也上报"为断言。
- [事件量级被低估（高频滚轮/触摸/输入场景）] → 已被实测触发并修复：D6 采纳前端 leading+trailing 节流（2s 窗口，DR4），持续交互下每标签页约每 2s 至多一次请求；溢出时既有全局恢复语义（清空全部任务计时并重建）的陈述保留；消费端 O(1) 不变。
- [shell tid 解析失败导致活动丢失] → 静默忽略为已接受语义（非法 tid 本就不应产生效果）；解析复用既有 `taskIDFromSessionName`，与 shell WS 身份校验同源。
- [多标签页同时操作] → 每个标签页各自上报同一 taskID，事件重复无害（D2 幂等）。
- [既有漂移：`serve_runtime.session_error` 已在代码过滤表（task_filter.go:52-62、sessions_filter.go:65-70）但未进入 task-detail-stream / active-sessions-stream 的 spec 决策表] → 本 change 不顺带修改（避免范围蔓延），建议在人工 review 后另行立项或在本 change 人工 review 中决定是否顺带补齐。
