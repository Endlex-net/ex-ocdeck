## Why

三个线上 bug 影响任务操作效率与权限安全体感：任务详情页内切换「终端 / shell」tab 后键盘焦点不进入终端输入框（此前只修复了跨任务导航聚焦）；权限 AI 自动识别（ai-auto）模式下无论识别通过与否都发通知，通过场景属于打扰；ai-auto 存在自动拒绝路径，与「AI 只应辅助放行、决策权留给用户」的预期不符。另有一项能力缺口：权限模式只能在创建任务时选定，创建后无法更改，用户调整授权策略只能重建任务。

## What Changes

- **任务详情页 tab 聚焦**：用户在任务详情页点击「终端」或「shell N」tab 后，键盘焦点进入对应终端的输入框，可直接键入。Git / 设置 tab 不涉及。既有跨任务导航聚焦行为（侧栏、任务切换器、指挥中心任务行）保持不变。
- **ai-auto 识别通过不通知**：任务处于 ai-auto 权限模式时，权限请求若被 AI 自动放行，则不再发起权限通知；仅在需要用户决策时（AI 判定拒绝、无法识别、判定失败/超时）才通知。非 ai-auto 任务的通知行为不变。AI 判定迟迟不返回时需要有超时兜底，超时后按「等待用户决策」通知，不允许用户长时间不知情。
- **ai-auto 不再自动拒绝**：ai-auto 模式下 AI 判定为拒绝时，不再自动回复 reject，与「无法识别」一样转为等待用户决策；AI 自动动作仅限「放行」。判定为拒绝的审计留痕保留（仅不再自动执行拒绝回复）。
- **权限模式创建后可更改**：任务详情页设置 tab 支持修改权限模式，ask / all-approve / ai-auto 三档任意互转。修改后立即保存生效；ai-auto 判定行为即时切换（切入 ai-auto 时对现有待处理权限立即补判——以 all-approve 启动的运行中进程除外，其 AI 判定在下次激活才启用；切出 ai-auto 后在途判定不再自动回复）；all-approve 的自动批准行为在任务下次激活时切换（运行中的进程保持原行为）。**BREAKING（行为语义）**：反转既有「权限模式创建后不可修改」的规格约束。

**非目标**：不改变 AI 判定模型/prompt 的识别能力本身；不改变问题（question）类通知；不改变终端锁定、移动端适配等其他聚焦相关既有语义；不调整通知渠道/内容格式。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `web-ui-shell`: 任务导航聚焦要求扩展——新增「任务详情页内切换到终端类 tab（终端 / shell）后聚焦对应终端输入」的行为要求。
- `task-notifications`: 权限通知触发要求修改——ai-auto 任务的权限请求被 AI 自动放行时不再通知；仅在需要用户决策（判定拒绝、无法识别、判定失败或超时）时通知。
- `task-permission-mode`: 两项修改——① ai-auto 判定结果处理要求修改：移除「判定拒绝 MUST 自动回复 reject」的要求，改为「仅放行可自动执行，拒绝与无法识别均等待用户决策」；② 移除「权限模式创建后不可修改」的要求，新增「创建后可更改权限模式」的行为要求（含生效时机语义）。

## Impact

- 前端：`web/src/pages/TaskWorkbenchPage.tsx`（tab 切换）、`web/src/terminal/`（终端聚焦相关）、相关前端测试。
- 后端：`internal/task/permit_auto.go`（判定结果处理）、`internal/application/notification/`（权限通知触发）、任务更新 API 链路（api/task/store 各层，承载权限模式变更）、相关 Go 测试。
- 规格：`openspec/specs/web-ui-shell`、`openspec/specs/task-notifications`、`openspec/specs/task-permission-mode` 三个 capability 的 requirement 变更。
- 外部契约：opencode ReplyPermission API 不变（仅减少 reject 调用场景）；通知 Intent/渠道契约不变。
