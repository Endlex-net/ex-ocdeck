# Design: task-notifications

## Context

ocdeck 是独立 Go 单进程 opencode 编排台：进程内 eventbus（`internal/infrastructure/eventbus`）+ 封闭领域事件目录（`internal/domain/event/event.go:17-60`），任务运行态信号已有 `serve_runtime.attention_changed`（question/permission pending）与 `serve_runtime.run_status_changed`（busy/retry/idle 聚合，busy>retry>idle，`internal/task/agent_status.go:35-65`；`from`/`to` 可为空串表不可用，`event.go:113-119`）。`session.error` 存在于 opencode SSE 流但当前刻意不消费（`internal/infrastructure/opencode/session_status_test.go:69`，真实样本 `testdata/session_status_events.jsonl:6`）。retry 详情（attempt/message/next）在 opencode 层解析存在（`session_status.go:8-14`）但 task 层 apply 只保留 status（`agent_status_state.go:523-525`），需扩展保留（D3）。应用配置先例为 ai-provider-config（`<dataDir>/ai.json` + 内存快照 + GET/PUT + 热更新）。行为规范（触发语义、抑制、门禁、渠道判定、配置 schema）的唯一归属是 spec delta，本文档只承载机制与决策理由，提及行为规范时按 requirement 名引用。

## Goals / Non-Goals

**Goals:**
- 事件驱动的五类触发（语义见 spec「通知触发」系列 requirement）
- 渠道无关 Intent 抽象 + 能力位降级；web / bark / macos 三渠道
- 通知配置（ai-provider-config 模式）+ 设置页子标签 + 测试通知
- 可选 LLM 停止原因总结（输入仅任务名+类别+详情），默认关闭，失败降级

**Non-Goals:**
- 通知撤回/更新（能力位预留，不实现）
- 通知历史持久化、多设备路由、通知自动重试
- workflow 语义级通知（blocked 等）
- ex-notify 插件文件处理（用户手动删除）
- LLM 总结拉取会话消息（YAGNI，见 D9）

## Decisions

### D1: 模块分层与依赖方向

```
internal/
  domain/event/event.go               # 仅新增 serve_runtime.session_error（D2）
  domain/notification/                # 值对象（stdlib only）：Intent/Category/Level/Capability/Result/Config
  application/notification/           # Notifier 编排：触发状态机、计时、抑制、门禁、内容组装
    notifier.go  triggers.go  episode.go  dispatch.go  content.go  ports.go
  infrastructure/
    opencode/session_error.go         # ParseSessionErrorEvent（D2）
    notify/                           # 渠道适配器 bark.go / macos.go / web.go + store.go（配置存储）
  api/                                # 配置 API、测试通知、WebHub + 通知 SSE 端点（D7）
web/src/                              # 设置页通知子标签 + 通知 SSE 订阅 + Notification 展示
```

- 依赖方向遵循既有架构门禁（`internal/api/import_graph_test.go`：api → application → domain，domain stdlib-only，infrastructure 不依赖 api），新增包的 import 断言一并加入该测试。
- Notifier 对任务侧与 bus 的依赖全部走窄端口（`ports.go`，沿用 `main.go:114-116` 的窄接口注入先例与 `cmd/ocdeck-server/events.go:20-27` 的组合根适配先例）：
  - `EventSubscriber` / `EventSubscription`（bus 订阅窄接口：`Subscribe(topic) EventSubscription`，`EventSubscription` 暴露 `C() <-chan event.Event` / `Overflow() <-chan struct{}` / `Close()`；main.go 以 adapter 包装 `*eventbus.Bus`——Bus.Subscribe 返回 `*Sub`，与端口返回类型不同，必须经组合根适配，application 不 import infrastructure 具体类型）
  - `TaskNotificationSnapshot(ctx, taskID) (TaskSnapshot, error)`——**单次组合读取**，避免分次快照的撕裂读：`TaskSnapshot{Task TaskRef{ID, Name, Status}, Attention application.Attention, RunStatus string, RetryDetail RetryDetail, HasRetryDetail bool}`
  - `ListAllActiveTaskIDs(ctx) ([]string, error)`（启动基线与 overflow 对账枚举）
- 事件处理、计时届满判定与投递复验 MUST 在 Notifier 单一 run loop 内串行执行；残余竞态窗口的接受语义以 spec「通知抑制、启动基线与对账」为唯一表述（不引入 revision/代际机制）。
- **为什么不挂进 `internal/task` Manager**：触发器是横切关注点，输入（bus 事件）与输出（渠道）与任务生命周期正交；Manager 只新增"retry 详情保留"这一小扩展（D3），不承担通知逻辑。

### D2: session.error 消费——`ParseSessionErrorEvent` + 新领域事件

- `internal/infrastructure/opencode/session_error.go` 新增 `ParseSessionErrorEvent`（与 `ParseAttentionEvent` 同形，`attention.go:80`）。字段规则（spec「通知触发——错误未恢复」为唯一表述）：必填 `sessionID`/`error.name`/`error.data.message`，缺失或类型非法 → `(zero, false)` 忽略整个事件；`error.data.statusCode`/`error.data.isRetryable` 可空，存在但类型非法仅降级该字段。typed 输出 `SessionErrorEvent{SessionID, Name, Message string; StatusCode *int; IsRetryable *bool}`。
- task 层 SSE 派发链（attention/status 同一消费点）将合法事件发布为新领域事件 `serve_runtime.session_error`：RID=instVersion，payload `{task_id, session_id, name, message, status_code *int, is_retryable *bool}`。这是对 D0 封闭事件目录的显式增量，`event.go` 目录注释同步更新。
- **为什么不并入 run_status**：run_status 是状态投影，error 是一次性事件；污染投影语义会影响既有 SSE 消费方。

### D3: 触发器状态机与 retry 详情保留

**task 层小扩展（既有逻辑收敛）**：`agentStatus` 内存态为每个 session 附加保留最近一次 retry 详情（apply 点 `agent_status_state.go:523-525` 扩展写入），新增 `RunStatusDetail` 读端口。类型定义（缺失显式表达，不用零值歧义）：

```go
type RetryDetail struct {
    Attempt int    // 重试序号，>=1 为有效；0 表示缺失
    Message string // 失败摘要；空串表示缺失
    Next    int64  // epoch ms；0 表示事件未携带 next
}
```

有效性规则（唯一成立条件）：`Attempt > 0 && strings.TrimSpace(Message) != ""` 时端口返回 `ok=true`，其余一律 `ok=false`（仅 Next、仅 Attempt、空白 Message 均为不可得，走降级文案）。多 session 同时 retry 的确定选择规则：先按上述规则过滤出有效详情的 session，取 `Next > 0` 中 Next 最小者，Next==0 的排最后，并列取 sessionID 字典序最小；无有效详情 session → `ok=false`。不变量：busy>retry>idle 聚合语义与既有事件发布行为不变，仅附加详情保留。

**Notifier 状态机**（单 goroutine run loop + 10s 扫描 tick；触发语义见 spec，此处为机制）：

```
                 run_status_changed          session_error
                 from=busy,to=idle,av=true   (聚合非 busy)
                      │                          │
                      ▼                          ▼
                ┌──────────┐               ┌───────────┐
                │ IDLE     │               │ ERROR     │◀── 重复 error 仅更新详情
                │ pending  │               │ pending   │    不延长计时起点
                └────┬─────┘               └─────┬─────┘
     取消：非idle/不可用 │ 届满触发 idle           │ 届满且 episode     │
     /pending出现/episode │                      │ 未通知 → error     │
     /离开active          ▼                      ▼ （error > retry 同 tick 优先）
                 ┌──────────┐  to=retry   ┌───────────┐
                 │ 抑制/ armed│            │ RETRY     │
                 │ 周期结束   │◀──────────│ pending   │
                 └──────────┘  离开retry   └───────────┘
                               取消          届满且 episode
                                            未通知 → retry

  episode = [进入 retry 或 session.error] → [聚合回到 busy]
  episode 存续期间：idle 不武装/已武装取消；retry/error 合计至多通知一次
```

- 每任务内存字段表：`idleSince *time.Time`（进入 idle 的武装时刻；扫描时以 `idleSince + 当前配置阈值` 判定到期，支持阈值热更新，spec idle requirement 为唯一表述）、`retryDeadline *time.Time`、`errorDeadline *time.Time`、`errorSeen bool`（本 episode 首个 error 只武装一次；重复 error 仅更新 lastError；episode 结束复位）、`episodeActive bool`、`episodeConsumed bool`（名额占用语义见 spec「发送前门禁与投递原子性」仲裁表）、`idleSuppressed bool`、`notifiedQuestions map[requestID]struct{}`、`notifiedPermissions map[requestID]struct{}`（两张独立 map，去重键不跨类型碰撞）、`lastError SessionErrorEvent`。
- **runtime fencing（生命周期保护，非 revision 机制）**：serve_runtime 事件 payload 的 RID 即 instVersion；组合快照暴露任务当前 runtime 的 instVersion，Notifier 处理 serve 事件时若与快照实例不一致 MUST 丢弃（旧 runtime 的迟到事件不得污染新 active 实例的状态机）。对账时若快照为 busy 则执行 episode 关闭语义并清除 episodeConsumed；仅对仍存续的非 busy episode 保留名额。
- **扫描窗口**：10s tick，触发延迟上界 = 阈值 + 10s（60s 阈值 → 最坏 70s），已接受。
- **启动基线**（spec「通知抑制、启动基线与对账」）：run loop 先 Subscribe 再取基线快照再 drain 队列；基线经 `ListAllActiveTaskIDs` 枚举 active 任务，**枚举失败 → 记错误日志并整体禁用通知触发**（配置 API 与测试通知仍可用，触发器待进程重启恢复）；attention 基线只播种 `notifiedQuestions`/`notifiedPermissions`；idle 不武装；已是 retry → `retryDeadline = now+60s`；不恢复 error 计时。
- **overflow 对账**（`Sub.Overflow`，`bus.go:136-154`）：检测到溢出后 MUST 先丢弃受污染订阅的既有事件队列（gap 前事件不得在对账后继续解释），再以当前快照重建。保留同实例的两张 notified map、`episodeConsumed` 与 `errorSeen`（runtime 换代或快照 busy 时全部清除），取消全部计时，以当前快照按启动基线同规则重建。每订阅溢出信号为合并信号（容量 1），消费至多一个 token，不得循环清空（防活锁）。对账期间 `ListAllActiveTaskIDs` 或快照读取失败 → 进入 reconciling 状态：抑制全部发送，每 10s 重试对账，成功重建后恢复正常判定（MUST NOT 带不可信状态继续投递）。

### D4: 渠道抽象与内容组装

```go
// domain/notification
type Intent struct {
    TaskID, TaskName string
    Category         Category  // question|permission|idle|retry|error|test
    Level            Level     // passive|active|timeSensitive|critical
    Title, Body      string
    URL              string
}
type Capability int  // 位掩码：CapGroup | CapReplace | CapWithdraw（Withdraw 本期无实现）
// 渠道「已配置」判定与各实现能力位矩阵的唯一表述在 spec「通知渠道投递与降级」。
type Channel interface {
    Name() string
    Caps() Capability
    Send(ctx context.Context, in Intent, cfg ChannelConfig) Result  // Result{OK bool, Err string}
}
```

**DispatchPlan（投递配置固化）**：候选判定开始时 MUST 只读取一次 `ConfigSnapshot`，门禁复验、URL 推导、LLM 开关与 ResolvedChannel 全部从该快照派生；门禁通过后生成不可变 DispatchPlan——`{URL, LLMSummary bool, Channels []ResolvedChannel}`，其中 `ResolvedChannel{Channel, ChannelConfig}`（bark 固化 endpoint/token；web/macos 无配置字段）。渠道实现 MUST 只持静态依赖（http.Client、exec runner、WebPublisher），MUST NOT 持有或读取 notifyStore；单次投递全过程使用 DispatchPlan 内的固化配置。BaseURL resolver MUST 只读取 `srv.BoundAddr()`；`base_url` 覆盖值 MUST 作为参数从候选判定的 ConfigSnapshot 传入，resolver MUST NOT 自行读取 notifyStore。

- 级别映射固定（spec「通知渠道投递与降级」为唯一表述）。
- 内容组装模板（Title 统一格式 `[ocdeck] [<任务名>] <类别标题>`——Bark 等通用渠道中通知与其他应用混合呈现，任务名截断至 12 rune，spec「通知内容与跳转链接」为唯一表述；Body 含任务名全称与详情）：
  - question：Title=「[ocdeck] [<任务名>] 等待你的回答」，Body=任务名 + 提问内容（多条 `\n` 拼接，单字段截 200 字符，整体截 500 字符）
  - permission：Title=「[ocdeck] [<任务名>] 等待权限确认」，Body=任务名 + 权限名 + patterns（`, ` 拼接，同截断）
  - error：Title=「[ocdeck] [<任务名>] 运行出错」，Body=任务名 + message（+ ` (HTTP <statusCode>)`，若有）
  - retry：Title=「[ocdeck] [<任务名>] 重试未恢复」，Body=任务名 + `第 <attempt> 次重试：<message>`；详情不可得 → 固定文案「任务持续处于重试状态」
  - idle：Title=「[ocdeck] [<任务名>] 任务已空闲」，Body=任务名 + `已空闲超过 <阈值> 秒`
  - test：Title=「[ocdeck] [ocdeck] 测试通知」，Body=「ocdeck 通知链路测试」
- dispatch 层不再做标题降级（标题统一含任务名后，无 CapGroup 渠道的降级仅表现为通知中心不折叠）。
- 分组键：Bark `group` 与 terminal-notifier `-group` 使用 `ocdeck/<任务名>`（任务名截 40 字符；分组名用户可见，MUST 可读且自带来源）；web `tag` 保持任务 ID（用户不可见，仅替换去重用途）。
- 降级文案规则的唯一行为表述在 spec「通知内容与跳转链接」。

### D5: 通知配置——ai-provider-config 模式平移

- `infrastructure/notify/store.go`：`LoadStore(dataDir)` 返回内存快照 Store（`main.go:104-107` 先例）；原子写、0600、热更新、load_error 降级——行为唯一表述在 spec「通知配置存储/读写 API/配置运行时生效」，本文不复制字段清单。
- Store 仅注入 Notifier 与 API 层；渠道适配器 MUST NOT 持有 Store（投递配置经 DispatchPlan 固化下发，见 D4）。
- JSON 解码未知字段忽略（对齐既有 `decodeJSON`，`internal/api/projects.go:433`，不使用 `DisallowUnknownFields`）。
- 配置的唯一 schema 表述在 spec「通知配置存储」，D5 不另列字段。

### D6: 事件目录扩展

仅新增 `serve_runtime.session_error`（D2）。~~notification.created bus 事件~~已取消——web 渠道改走 WebHub（D7），通知意图不是领域状态变更，不进事件目录。

### D7: web 渠道——WebHub（不用 bus 事件）

- `internal/api` 新增 WebHub：维护已连接 SSE 前端的注册表；`GET /api/v1/notifications/stream` 建立连接（帧格式 `event: notification\ndata: <Intent JSON>`，一帧一条，无 snapshot/重放）。每连接一个带缓冲 channel（容量 16），投递为非阻塞 enqueue：缓冲满判定该连接为慢客户端，断开并移除（前端自动重连，通知不重放——语义与 spec「断线重连不重放」一致）。`accepted=true` 当且仅当至少一个连接本次 enqueue 成功；零连接或全部连接缓冲满（零 enqueue 成功）均为 `accepted=false`。
- web 渠道适配器（`infrastructure/notify/web.go`）持窄端口 `WebPublisher.Publish(Intent) (accepted bool)`，由 WebHub 实现；`accepted=false`（零连接）即该渠道投递失败（spec「网页通知渠道」为判定唯一表述）。
- **为什么不用 bus**：bus `Publish` 无返回值（`bus.go:44-63`），无法表达"至少一个已授权连接接纳"的投递结果；且前端通用 SSE 解析器只接受 snapshot/update 帧（`web/src/sse.ts:104`），通知流是独立轻量订阅。
- 前端：App 根部在 web 渠道启用且 `Notification.permission === 'granted'` 时连接；收到帧 `new Notification(title, {body, tag: taskID})`，onclick 聚焦窗口并导航到该帧 `Intent.URL` 对应的目标页（hash 路由深链，`web/src/router.ts:37` 的 navigate 机制）。多标签页语义见 spec。

### D8: 跳转链接推导

真实五类别：`URL = <base>/#/task/<taskID>`；test 类别：`URL = <base>/#/configs#notifications`（spec「通知内容与跳转链接」为行为唯一表述）。base 推导顺序：配置 `base_url` 非空 → 剔除尾部 `/` 后用之；否则 `http://<host>:<port>`，host 为 `ListenAddr`（wildcard `0.0.0.0`/`::` 映射为 `127.0.0.1`），port 为**实际监听端口**。

- **装配顺序修复**（现 `Server.Start` 内部 bind 且无 accessor，`server.go:367-394`）：拆分为以下签名——

```go
func (s *Server) Listen() error                     // bind + 记录 listener 与实际地址；重复调用返回错误
func (s *Server) BoundAddr() net.Addr               // Listen 成功前返回 nil
func (s *Server) Serve(ctx context.Context) error   // 未 Listen 返回错误；以最终 mux 构造 http.Server
```

完整装配顺序以 D11 为准（构造惰性 notifier → 注入依赖 → RebuildRoutes → Listen → notifier.Start → Serve），BaseURL 闭包惰性读取 `BoundAddr()`，Listen 后即可解析。URL 校验规则（hierarchical、非空 host、禁 userinfo/query/fragment、path 空或 `/`）的唯一表述在 spec「通知配置存储」。
- URL 不可用时按 spec「发送前门禁」复验失败处理（无副作用）。

### D9: LLM 停止原因总结（选项 B：不拉取会话消息）

- 输入仅任务名 + 类别 + 类别详情（现 `TaskBackend` 无消息读取能力，`internal/api/tasks.go:15-76`，不为此扩展 serve 调用——YAGNI）。
- 固定 prompt 模板（逐字）：

```
你是通知摘要助手。根据以下任务运行信息，用一两句中文概括该任务停止或等待人工处理的原因，只基于给定信息，不要臆测。
任务：{{TaskName}}
类别：{{Category}}
详情：{{Detail}}
```

  max_tokens 200；整体 5s 预算（含 AI 配置读取）；输出空白或超 300 字符 → 降级。复用 `infrastructure/ai` completer 与 aiStore（`main.go:106`）。`llm_summary` 开关在 DispatchPlan 中固化（D4），LLM 执行期间的配置变更不影响本次投递。
- 失败/超时/未配置 → 确定性摘要；行为唯一表述在 spec「LLM 停止原因总结」。

### D10: macOS 渠道双实现

- 启动时探测并缓存两个可执行文件的存在性：`exec.LookPath("terminal-notifier")` 与 `exec.LookPath("osascript")`。terminal-notifier 存在 → `terminal-notifier -group <taskID> -title <t> -message <b> -open <url> -sound default`（Caps=Group|Replace，与 spec 能力矩阵一致）；仅未找到且 osascript 存在 → osascript（Caps=0）；两者均不存在 → 渠道 skipped（spec 能力矩阵为唯一表述）。
- osascript 固定脚本 `on run argv` 形式从 argv 读 title/body（`osascript -e <script> <title> <body>`），文案不进脚本字符串，杜绝转义注入。
- 两实现均 `exec.CommandContext`（无 shell），统一 10s 硬超时，stdout/stderr 读取上限 4KB。terminal-notifier 执行失败不再降级（spec「macOS 本地通知渠道」为唯一表述）。

### D11: 装配与关停（main.go）

```
notifyStore := notify.LoadStore(cfg.DataDir)              // 损坏不致命
webHub    := srv.NotificationHub()                        // api.New 时构造，路由与渠道共享同一实例
notifier  := notification.New(notification.Options{
    Bus: eventSubscriberAdapter{bus}, // 组合根适配，见 D1 窄端口
    Tasks: tm /*窄端口*/, AI: aiStore, Cfg: notifyStore,
    ResolveBaseURL: func(configuredBaseURL string) (string, error) { /* 只读 srv.BoundAddr()；configuredBaseURL 由候选判定的 ConfigSnapshot 传入（D4），闭包不读 notifyStore */ },
    Channels: notify.BuildChannels(webHub, runtime.GOOS), // 渠道只持静态依赖；bark endpoint/token 经 DispatchPlan 下发（D4）
})
srv.SetNotificationStore(notifyStore); srv.SetNotificationTester(notifier)
srv.RebuildRoutes()        // 延迟依赖先注入再 Rebuild（main.go:149-163 惯例）
err = srv.Listen()         // D8：bind 并记录实际地址（含系统分配端口）；不启动 Serve
notifier.Start(ctx)        // run loop + 10s tick + 启动基线（BaseURL 闭包此时可解析）
srv.Serve(ctx)             // 用最终 mux 构造 http.Server 并开始服务
```

注：`config.Config` 无 `base_url`/`GOOS` 字段（`internal/config/config.go:51-74`），`base_url` 属通知配置（notifyStore 快照），平台值直接由 `runtime.GOOS` 注入。`Listen` 仅 bind/store listener，`Serve` 才用最终 mux 构造 `http.Server`。

关停：notifier.Stop() 先于 tm.Shutdown（不再发通知）；in-flight 投递随 ctx 取消。

### D12: 实现分 lane 与验证策略

- **Lane A 配置与事件契约**：domain/notification 值对象、store、ParseSessionErrorEvent、事件目录扩展。测试：fixture 解析（沿用 session_status_events.jsonl 模式固化真实样本）、store 原子写/掩码/降级。
- **Lane B 触发器**：application/notification 状态机，fake clock 注入（不睡真实时间）。测试：五类触发、抑制/重新武装、episode 竞争（同 tick error>retry、episode 名额仲裁四分支）、启动基线（含枚举失败禁用触发）、overflow 对账（含对账失败进入 reconciling）、门禁复验、suspend→reactivate 重新武装、busy→retry→busy→retry episode 重开、idle 阈值升高/降低热更新、DispatchPlan 固化（LLM 阻塞期间并发 PUT 不影响在途投递：在途 URL/LLM 开关/渠道集合/bark endpoint+token 全部保持旧快照，下一次投递用新配置）。
- **Lane C 渠道**：bark（httptest fake server 验证 wire 契约）、macos（注入 command runner fake）、web（fake WebPublisher）。测试：成功/失败判定、降级前缀、超时、禁日志 token。
- **Lane D API/UI**：配置 GET/PUT、测试通知、WebHub SSE、设置页子标签、前端通知订阅。测试：状态码矩阵、并发 PUT、SSE 帧格式、多标签、权限分支。
- **Lane E LLM 总结**：completer 复用 + 预算/降级。最后做，可被前四 lane 独立交付。
- 全部 lane 完成后 D11 wiring + 端到端冒烟（真实 serve 起任务制造 question/idle）。

## Risks / Trade-offs

- [bus 溢出丢事件导致漏触发] → D3 overflow 对账；缓冲 64 远超通知事件量级。
- [attention 快照读失败] → 按"无变化"处理（spec question requirement），不误发。
- [LLM 重新引入不稳定依赖] → 默认关闭 + 5s 上界 + 确定性降级（D9）。
- [terminal-notifier 首次需 macOS 授权/无 GUI session 失败] → 仅记日志 + 测试通知结果暴露；osascript 兜底仅在未安装时（D10）。
- [base_url 推导为 loopback 时 Bark 手机不可达] → 显式 base_url 配置 + 界面提示（D8）；不猜测公网地址。
- [进程崩溃/渠道瞬时失败丢通知] → 已声明非目标（无历史、无重试），保留为显式验收风险。
- [session.error 契约漂移] → fail-closed 解析 + 真实样本 fixture（D2）。
- [多标签页/Safari 下网页通知重复] → 已接受降级（spec「网页通知渠道」）。

## Migration Plan

1. 部署新版 ocdeck（默认总开关关闭，行为与现状一致，零打扰）。
2. 用户在设置页通知子标签开启渠道与类别，测试通知验证。
3. 用户手动删除 `~/.config/opencode/plugin/ex-notify.ts`；Bark token 迁入通知配置。
4. 回滚：关闭总开关即恢复无通知行为；代码回滚安全（全部新增，既有路径修改仅：session.error 派发分支、agentStatus 详情保留、Server.Listen/Serve 拆分）。
