## Why

用户反馈：当前 idle 提醒过于机械——任务运行停止后只要 1 分钟（`idle_timeout_seconds`，默认 60s）没有恢复运行就发送提醒，即使用户这期间正在该任务的 git 页面点击/翻页、在终端或 shell 页面输入，说明用户其实正在处理，提醒造成打扰。

## What Changes

- idle 提醒的等待窗口内，若用户在该任务上有主动操作（终端/shell 键盘输入、git 页面区域内的用户主动交互，如点击/翻页/滚动翻阅/编辑输入），则取消本次 idle 提醒；直到任务下次重新进入 idle（新的运行停止）才重新计算提醒。
- 主动操作按任务归属：用户在某任务页面上的操作只影响该任务的 idle 提醒，不影响其他任务。
- 仅影响 idle 类别提醒；question / permission / retry / error 类别行为不变。
- 前端自动轮询、定时刷新、程序滚动等非用户触发的交互不算主动操作。

### Non-goals

- 除新增本任务主动操作取消本次 idle 提醒的行为外，不改变既有 idle 提醒规则及等待阈值配置。
- 不改变提醒发送渠道、内容、配置项取值域。
- 不处理 retry/error 提醒窗口。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `task-notifications`: idle 提醒增加用户主动操作抑制语义——等待窗口内检测到该任务的主动操作时取消本次 idle 提醒；新增「用户主动操作识别」requirement。
- `task-detail-stream`: 消费过滤新增口径——合法的 `task.user_activity` 事件不标脏（用户活动不影响任务详情投影）。
- `active-sessions-stream`: 领域事件目录新增 `task.user_activity`（瞬时用户输入事实，不受"状态真实变更后发布"前提约束）；指挥中心消费过滤新增口径——合法 `task.user_activity` 不标脏。
- `projects-stream`: 消费过滤新增口径——合法的 `task.user_activity` 事件不标脏（用户活动不影响任务树投影）。

## Impact

- 应用层通知模块（`internal/application/notification/`）：支持本任务主动操作取消本次 idle 提醒的行为。
- API 层（`internal/api/`）：涉及终端/shell 输入与 git 用户操作的识别范围。
- 前端（`web/src/`）：git 页面用户手势需与自动轮询/刷新区分。
- 通知配置与渠道不受影响；新增用户活动上报 API（`POST /api/v1/tasks/{id}/activity`，设计阶段已选定）。
- SSE 读模型流（task-detail / active-sessions / projects）的消费过滤需适配新事件类型，避免用户活动触发无关快照重推。
