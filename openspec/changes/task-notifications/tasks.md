# Tasks: task-notifications

## 1. Lane A：配置与事件契约

- [x] 1.1 新增 `internal/domain/notification` 值对象与渠道契约：Intent（taskID/taskName/category/level/title/body/url）、Category（question/permission/idle/retry/error/test）、Level（passive/active/timeSensitive/critical）、Capability 位掩码（CapGroup/CapReplace/CapWithdraw）、Result{OK, Err}、Channel 接口（`Name() string`、`Caps() Capability`、`Send(ctx, Intent, ChannelConfig) Result`）与 ChannelConfig/DispatchPlan/ResolvedChannel 类型（design D4）、Config 模型（spec「通知配置存储」schema）
- [x] 1.2 新增 `internal/infrastructure/notify/store.go`：`LoadStore(dataDir)`——`<dataDir>/notification.json` 原子写/0600/热更新快照/load_error 降级/未知字段忽略/校验（阈值 [10,3600]、URL 规则），行为以 spec「通知配置存储/读写 API/配置运行时生效」为准
- [x] 1.3 新增 `internal/infrastructure/opencode/session_error.go`：`ParseSessionErrorEvent`——必填 `sessionID`/`error.name`/`error.data.message`（TrimSpace 非空），可空 `statusCode`（仅无损 Go int 的 JSON 整数）/`isRetryable`（仅布尔），fail-closed 语义以 spec「通知触发——错误未恢复」为准
- [x] 1.4 `internal/domain/event/event.go` 事件目录增量：新增 `serve_runtime.session_error` 类型 + payload 构造器（`{task_id, session_id, name, message, status_code *int, is_retryable *bool}`），更新目录注释
- [x] 1.5 Lane A 测试：store 原子写/掩码/损坏降级/写锁串行化并发更新；session_error 解析 fixture（沿用 session_status_events.jsonl 模式，含 null/空白/小数/溢出样本）
- [x] 1.6 task 层 SSE 派发链接入：合法 session.error → 发布 `serve_runtime.session_error`（attention/status 同一消费点）

## 2. Lane B：触发器（application/notification）

- [ ] 2.1 新增 `internal/application/notification`：Notifier run loop（单 goroutine 串行：事件/计时/扫描）+ 窄端口（EventSubscriber/EventSubscription、TaskNotificationSnapshot 组合快照、ListAllActiveTaskIDs）
- [ ] 2.2 task 层小扩展：agentStatus 内存态保留每 session 最近 retry 详情（apply 点 agent_status_state.go:523-525），实现组合快照端口（RetryDetail 有效性 `Attempt>0 && TrimSpace(Message)!=""`，多 session 选择规则 Next>0 最小→Next==0 排后→sessionID 字典序）；不变量：聚合语义与既有事件发布行为不变
- [ ] 2.3 question/permission 触发：attention_changed → 组合快照读 pending 集合，按（类型, requestID）去重，了结即从去重集合移除
- [ ] 2.4 idle 触发：仅 `available=true && from=busy && to=idle` 武装（记 idleSince），取消条件全集，扫描以 `idleSince + 当前阈值` 判定（阈值热更新）
- [ ] 2.5 retry/error 触发 + episode 状态机：retry/error deadline（60s 固定）、episode 开闭、episodeConsumed 名额仲裁（spec「发送前门禁与投递原子性」仲裁表）、同 tick error>retry
- [ ] 2.6 启动基线与 overflow 对账：先订阅→基线快照→drain；attention 只播种不补发；idle 不武装；retry 从 now+60s；枚举失败禁用触发；overflow 保留两张 notified map 与 `episodeConsumed`、取消全部计时并按基线规则重建、对账失败进 reconciling（10s 重试）
- [ ] 2.7 发送前门禁与 DispatchPlan（`internal/application/notification/dispatch.go`，dispatch 唯一归属）：复验序列、ConfigSnapshot 单读、DispatchPlan 构造（URL/LLMSummary/ResolvedChannel 固化）、原子标记已消费、门禁失败零副作用、并行投递与失败隔离、无 CapGroup 时标题加 `[<TaskName>]` 前缀、全渠道失败不重试仍计已消费；内容组装模板（design D4：各类别标题/正文/拼接/截断/降级文案）与级别映射
- [ ] 2.8 Lane B 测试（fake clock，不睡真实时间）：五类触发、抑制/重新武装、episode 仲裁四分支、episode 已消费后 overflow 对账不得由另一异常类别再次投递、启动基线（含枚举失败）、overflow 对账（含 reconciling）、门禁复验、suspend→reactivate、busy→retry→busy→retry、idle 阈值升降热更新、DispatchPlan 并行投递/失败隔离/前缀降级

## 3. Lane C：渠道适配器（infrastructure/notify）

- [ ] 3.1 bark.go：POST `<endpoint>/push` JSON（device_key/title/body/level/group/url）、10s 超时、禁重定向/重试、64KiB 响应上限、成功=2xx 且 code==200、token/请求体/响应原文禁日志；Caps=Group；wire 契约以 spec「Bark 渠道」为准
- [ ] 3.2 macos.go：LookPath 探测 terminal-notifier 与 osascript（均不存在→skipped）；terminal-notifier（Caps=Group|Replace，`-group -title -message -open -sound default`）→ 仅未安装时 osascript（Caps=0，on run argv 固定脚本）；无 shell、10s 硬超时、4KB 输出上限；执行失败不再降级
- [ ] 3.3 web.go：窄端口 WebPublisher.Publish(Intent) (accepted bool)，Caps=Replace
- [ ] 3.4 Lane C 测试：bark httptest fake server（wire 契约全项：请求体字段/超时/禁重定向/64KiB 上限/成功判定/token 禁日志）、macos 注入 command runner fake（探测选择、执行失败不再降级、超时与输出上限）、web fake publisher、DispatchPlan 固化（LLM 阻塞期间并发 PUT 不影响在途投递）

## 4. Lane D：API 与前端

- [ ] 4.1 `internal/api`：WebHub（连接注册表、每连接缓冲 channel 16、非阻塞 enqueue、慢客户端断开、`event: notification` 帧 + spec 唯一 JSON 形状、无重放）+ `GET /api/v1/notifications/stream`
- [ ] 4.2 `internal/api`：配置 API `GET/PUT /api/v1/notification/config`（GET/PUT JSON 形状、token_masked 掩码、load_error 只读、400/422/500 矩阵、并发 PUT last-writer-wins）+ `POST /api/v1/notification/test`（`{"results":[{name,status,error}]}`，跳过 active/类别复验，总开关关闭返回 422）
- [ ] 4.3 `Server.Listen() error` / `BoundAddr() net.Addr` / `Serve(ctx)` 拆分（Listen 只 bind 记录地址；重复 Listen 报错；Serve 未 Listen 报错；既有 Start 语义迁移）
- [ ] 4.4 `internal/api/import_graph_test.go` import 断言扩展：domain/notification stdlib-only、application/notification 不 import infrastructure 具体类型、infrastructure 不依赖 api
- [ ] 4.5 前端：ConfigsTab 增加 `notifications`、设置页通知子标签（总开关/五类别/阈值/三渠道参数/base_url 提示/llm_summary/保存/测试按钮/web 权限状态与申请入口/load_error 展示，深链 `#/configs#notifications`）
- [ ] 4.6 前端：App 根部通知 SSE 订阅（仅 web 启用且权限 granted 时连接），`new Notification(title, {body, tag: task_id})`，onclick 聚焦并导航到帧内 url；api.ts/types.ts 增加对应 DTO
- [ ] 4.7 Lane D 测试：配置 API 状态码矩阵、并发 PUT、测试通知 DTO、WebHub 帧格式/慢客户端断开/多连接、Listen/Serve 拆分、前端子标签渲染与权限分支

## 5. Lane E：LLM 停止原因总结

- [ ] 5.1 Notifier 接入 ai completer：llm_summary 开关（DispatchPlan 固化）、固定 prompt（design D9 逐字模板）、max_tokens 200、5s 总预算、空白/超 300 字符/失败/未配置 → 确定性摘要降级
- [ ] 5.2 Lane E 测试：completer fake（成功/超时/空白/超长/未配置分支）、预算上界

## 6. 装配与端到端

- [ ] 6.1 main.go 装配（design D11 伪代码顺序）：notifyStore → webHub → notifier（eventSubscriberAdapter + 窄端口 + ResolveBaseURL(configuredBaseURL)）→ SetNotificationStore/SetNotificationTester → RebuildRoutes → Listen → notifier.Start → Serve；关停 notifier.Stop() 先于 tm.Shutdown
- [ ] 6.2 `openspec validate task-notifications --strict` 通过；`go build ./...` 与 `go test ./...` 通过；前端 `pnpm -C web build`（或项目既有构建命令）通过
- [ ] 6.3 端到端冒烟：真实 serve 起任务制造 question/idle，验证 web 通知与测试通知逐渠道报告
