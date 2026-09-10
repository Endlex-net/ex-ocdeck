## 1. 领域事件契约与 SSE 消费过滤

- [x] 1.1 在 `internal/domain/event/event.go` 闭合目录新增 `TypeTaskUserActivity = "task.user_activity"` 常量（注释标注本 change 增量来源）与 `NewTaskUserActivity(taskID string)` 构造器（`Topic=TopicTask`、`RID=taskID`、`Payload=struct{}{}`）；验证：新增单测断言信封四字段，`go build ./...` 通过
- [x] 1.2 `internal/api/task_filter.go` `eventDirtiesTaskDetail` 新增分支：合法 `task.user_activity`（Topic `task` 且 Payload `struct{}{}`）不标脏，Topic/Payload 畸形按保守惯例标脏；验证：表驱动单测覆盖合法不脏/畸形脏/未知 Type 仍脏（spec task-detail-stream 决策表）
- [x] 1.3 `internal/api/sessions_filter.go` `eventDirtiesActiveSessions` 同上规则新增分支；验证：表驱动单测覆盖合法不脏/畸形脏（spec active-sessions-stream「用户活动事件不驱动指挥中心推送」）
- [x] 1.4 `internal/api/projects_filter.go` `eventDirtiesProjectsTaskTree` 由全标脏改为：合法 `task.user_activity` 不标脏、畸形标脏，其余事件维持标脏；验证：表驱动单测覆盖合法不脏/畸形脏/其余事件仍脏（spec projects-stream「用户活动事件不触发更新」）

## 2. 发布入口与接线

2.1–2.3 为同一可编译批次：2.1 引入接口签名并适配 stub，2.2/2.3 完成 Manager 具体实现；批次的共同完成判据为 `go build ./...` 与既有测试通过。2.4 在该批次后执行。

- [x] 2.1 `internal/api/tasks.go` `TaskBackend` 接口新增 `RecordUserActivity(ctx context.Context, taskID string)` 与 `RecordShellUserActivity(ctx context.Context, tid string)` 两个方法签名，同步适配既有实现与测试 stub；验证：见本节共同完成判据
- [x] 2.2 `task.Options` 新增可选字段 `Publish application.Publisher`；`task.Manager` 实现 `RecordUserActivity`（Publish 未注入时 no-op，注入时发布 `NewTaskUserActivity(taskID)`）；验证：记录型 Publisher double 单测覆盖信封正确性与未注入 no-op，且断言对已有任务调用后任务行（至少 `updated_at`）保持不变、捕获的事件仅为 `task.user_activity`、不含 `task.activity_changed`
- [x] 2.3 `task.Manager` 实现 `RecordShellUserActivity`：内部经既有 `taskIDFromSessionName`（`internal/task/util.go:109`）解析任务 ID，解析失败静默忽略零发布，成功发布同一事件（发布副作用约束同 2.2）；验证：单测覆盖合法 tid 归属正确、非法 tid 零发布
- [x] 2.4 `cmd/ocdeck-server/main.go` 的 `task.New`（main.go:145）注入现有同一个 bus 为 `Publish`（沿用窄接口注入先例 main.go:134-139）；验证：`go build ./...` 通过，接线点与既有 bus 单例一致

## 3. Notifier 消费

- [x] 3.1 `internal/application/notification/notifier.go` `handleEvent` 新增 `task.user_activity` 分支：`taskID := ev.RID`，仅查 `n.states[taskID]`（不创建状态、不读快照），`idleSince != nil` 时置 `nil`，其余字段（retryDeadline/errorDeadline/episode/去重集合/抑制态）一律不动；验证：fake clock + 端口注入测试覆盖——武装后活动取消计时、取消后持续 idle 不触发、新 busy→idle 重新武装、跨任务隔离、无状态任务 no-op、迟到活动命中同任务当前已武装计时、retry/error/episode/去重集合不受影响（spec「通知触发——空闲超时（idle）」全部新增 scenario）

## 4. WS 输入识别

- [x] 4.1 `internal/api/ws_terminal.go` `pumpWSToPTY` 及 bridge 签名扩展注入活动回调，处理顺序固定为：读帧失败/ctx 取消退出 → text 帧先按既有 resize 解析，命中仅 Resize 不上报 → 非空 binary/非空非 resize text 先回调上报再沿用既有 PTY 写入与错误退出 → 空帧不上报；`handleWSTUI` 回调 `RecordUserActivity(path taskID)`，`handleWSShell` 回调 `RecordShellUserActivity(tid)`；适配既有 `internal/api/ws_bridge_test.go` 测试入口；验证：bridge 直测覆盖 binary/非 resize text 上报、resize/空帧不上报、PTY 写失败仍上报（spec「用户主动操作识别」终端相关 scenario）

## 5. HTTP activity 端点

- [x] 5.1 新增 `POST /api/v1/tasks/{id}/activity`：空请求体，鉴权与错误信封沿用既有 tasks 路由；handler 先 `TaskBackend.Get` 校验——错误经既有 `mapTaskErr` 映射返回且 MUST NOT 发布，成功调 `RecordUserActivity` 返回 204（任务存在但非 active 同样 204）；发布不等待消费者；验证：TaskBackend stub 测试覆盖 Get 失败零发布+错误映射、成功 204 且发布一次、非 active 204（design D4 处理链）

## 6. Git 前端上报

- [x] 6.1 `web/src/components/GitPanel.tsx` 区域容器以捕获阶段集中监听 `click`/`wheel`/`touchstart`/`touchmove`/`keydown`/`input`（`wheel`/`touchmove` 用 passive 监听，MUST NOT 阻止默认滚动），嵌套组件（ReviewPanel、DiffViewer 等）复用同一上报入口；命中即（经 6.2 leading+trailing 节流后）fire-and-forget 调 `POST /api/v1/tasks/{id}/activity`（取手势发生时的 taskID，fetch 忽略响应与错误）；MUST NOT 在 `loadStatus`/`openDiff`/`refreshDiff` 等请求函数内推断手势来源，程序 `scroll` 不计；验证：组件测试覆盖手势（含嵌套组件与持续 `touchmove`）调用上报、自动加载/定时刷新/程序滚动不调用（spec「用户主动操作识别」git 相关 scenario）
- [x] 6.2 GitPanel 上报入口实现 leading+trailing 节流（design D6，2s 窗口，用户决策 DR4）：距上次发送满 2s 的手势立即发送；冷却期内新手势仅置 `pending` 并到点补报一次（不被后续手势推迟，补报后清空）；孤立手势只发一次；`pending` 绑定手势发生时的 taskID，任务切换/卸载/进入 hidden 时尽力补报已有 `pending` 后清理定时器；程序事件（isTrusted=false）不发送也不改变节流状态；发送失败仍计入窗口；WS 终端输入不节流。适配既有组件测试（手势断言需考虑节流窗口，可用 fake timers 推进）；验证：组件测试覆盖首次立即上报、冷却期手势到点补报一次、孤立手势不重复补报、持续输入不饿死（每窗口至多一条且持续有报）、跨武装边界的最后手势经 trailing 补报不丢失、切任务不串报（pending 归属旧 taskID）

## 7. 跨层验证

- [x] 7.1 溢出恢复回归：活动事件进入订阅缓冲、溢出时执行既有恢复语义（`onOverflow` 清空全部计时 + `attemptReconcile` 重建）；验证：既有溢出对账测试扩展 `task.user_activity` 事件类型后通过
- [x] 7.2 全量验证：`go build ./...` 与 `go test ./...` 通过、web 前端测试与构建通过、`openspec validate idle-reminder-user-activity --strict` 通过（验证记录例外：`go test ./...` 唯一失败为 `internal/task` 的 `TestAllocatePort_ExcludeSkipsLastPortAndScan`——本机 50000-50002 端口被占用的既有环境性失败，git stash 基线复跑确认与本 change 无关）
