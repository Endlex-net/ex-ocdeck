# task-notifications Delta Specification

## ADDED Requirements

### Requirement: 通知触发——等待回答问题（question）

当任务（处于 active 状态）新增 pending question 注意力时，系统 SHALL 触发 question 类别通知。通知详情 MUST 包含提问内容。判定以 attention 快照中的（注意力类型, request ID）为去重键：同一 request ID 的 pending question 已通知后 MUST NOT 重复通知；该 request ID 从 pending 集合消失（了结）时 MUST 同时从去重集合移除（去重集合大小以当前 pending 集合为上界，不得无限增长），之后新增不同 request ID 的 pending question MUST 重新通知。question 与 permission 的去重键相互独立（同值 request ID 不跨类型抑制）。读取 attention 快照失败时 MUST 按"无变化"处理，MUST NOT 因读失败误发。

#### Scenario: 新增 pending question 触发通知

- **WHEN** active 任务的 attention 快照中出现新的 pending question（新 request ID）
- **THEN** 系统触发一次 question 类别通知，通知详情含该 question 的提问内容

#### Scenario: 同一 question 不重复通知

- **WHEN** 某 request ID 的 pending question 已触发过通知，attention 快照重复刷新且其仍 pending
- **THEN** 系统 MUST NOT 再次触发该 request ID 的通知

#### Scenario: 了结后重新提问再次通知

- **WHEN** 已通知过的 question 被了结（request ID 消失），之后出现新 request ID 的 pending question
- **THEN** 系统重新触发 question 类别通知

### Requirement: 通知触发——等待权限批准（permission）

当任务（处于 active 状态）新增 pending permission 注意力时，系统 SHALL 触发 permission 类别通知。通知详情 MUST 包含权限名称与模式（patterns）。去重键、重新武装与读失败语义与 question 一致（按（注意力类型, request ID），与 question 的去重键相互独立）。

#### Scenario: 新增 pending permission 触发通知

- **WHEN** active 任务的 attention 快照中出现新的 pending permission（新 request ID）
- **THEN** 系统触发一次 permission 类别通知，通知详情含权限名称与 patterns

#### Scenario: 同一 permission 不重复通知

- **WHEN** 某 request ID 的 pending permission 已触发过通知且仍 pending
- **THEN** 系统 MUST NOT 再次触发该 request ID 的通知

### Requirement: 通知触发——空闲超时（idle）

idle 计时 SHALL 且仅由满足以下全部条件的 `run_status_changed` 事件武装：`available=true`、`from=busy`、`to=idle`。武装时刻（idleSince）为进入 idle 的时刻；每次判定 MUST 按最新配置计算 `idleSince + 空闲阈值`（默认 60 秒，可配置）：缩短阈值 SHALL 在下一判定周期即可到期，延长阈值 SHALL 顺延。届满触发一次 idle 类别通知。武装后的计时在以下任一情况 MUST 取消且本周期不再触发：任务回到非 idle；聚合可用性变为不可用；出现任意 pending question/permission；进入异常周期（retry/error）；任务离开 active。通知后 MUST 进入抑制态，仅当出现新的满足武装条件的 busy→idle 迁移时才重新武装。`from` 为不可用态（空串）或 `from=retry` 的迁移 MUST NOT 武装 idle 计时。

#### Scenario: 忙后空闲超时触发通知

- **WHEN** 任务聚合状态从 busy 迁移到 idle（available=true），持续超过空闲阈值未回到非 idle，期间无 pending 注意力且未进入异常周期
- **THEN** 系统触发一次 idle 类别通知

#### Scenario: 有 pending 注意力时取消 idle 计时

- **WHEN** 任务 busy→idle 已武装计时，计时期间出现 pending question
- **THEN** idle 计时取消，本周期不触发 idle 通知

#### Scenario: 不可用恢复为 idle 不武装

- **WHEN** 任务聚合状态从不可用（from 为空串）恢复为 idle
- **THEN** 系统 MUST NOT 武装 idle 计时，不触发 idle 通知

#### Scenario: 启动时已是 idle 不通知

- **WHEN** ocdeck 启动后，任务聚合状态一直是 idle（未观察到满足条件的 busy→idle 迁移）
- **THEN** 系统 MUST NOT 触发 idle 通知

#### Scenario: 通知后重新武装

- **WHEN** 任务已触发 idle 通知，之后出现新的 busy→idle（available=true）迁移并再次超过阈值
- **THEN** 系统重新触发 idle 类别通知

#### Scenario: 缩短阈值立即到期

- **WHEN** 任务已武装 idle 计时（idleSince 为 3 分钟前），用户将空闲阈值从 300 秒改为 60 秒并保存
- **THEN** 下一判定周期按新阈值计算（3 分钟 > 60 秒），idle 通知触发

#### Scenario: 延长阈值顺延

- **WHEN** 任务已武装 idle 计时（idleSince 为 50 秒前），用户将空闲阈值从 60 秒改为 300 秒并保存
- **THEN** idle 通知顺延至 idleSince + 300 秒才触发

### Requirement: 通知触发——重试持续未恢复（retry）

当任务的聚合运行状态迁移为 retry 且持续 1 分钟（固定，不可配置）未离开 retry 时，系统 SHALL 触发一次 retry 类别通知。通知详情在可得时 MUST 包含重试信息（attempt/message）；不可得时 MUST 使用固定降级文案而非留空。异常周期（episode）从任务进入 retry 或观察到 session.error 起，到任务聚合回到 busy 止；同一 episode 内 retry 与 error 通知合计至多一次，两个计时同一判定周期同时届满时 error 优先于 retry。episode 结束后重新武装。

#### Scenario: 重试持续 1 分钟触发通知

- **WHEN** 任务聚合状态迁移到 retry 且持续 1 分钟仍未离开 retry，本 episode 内尚未发过 retry/error 通知
- **THEN** 系统触发一次 retry 类别通知

#### Scenario: 1 分钟内恢复不通知

- **WHEN** 任务进入 retry 后在 1 分钟内聚合回到 busy
- **THEN** 系统 MUST NOT 触发 retry 通知

#### Scenario: 同 episode error 已通知则抑制 retry

- **WHEN** 本 episode 内已触发过 error 通知，retry 计时届满
- **THEN** 系统 MUST NOT 触发 retry 通知

#### Scenario: retry 详情不可得时降级

- **WHEN** retry 通知触发但任务的重试详情（attempt/message）不可得
- **THEN** 通知使用固定降级文案，不因此放弃投递

### Requirement: 通知触发——错误未恢复（error）

系统 SHALL 消费 opencode SSE 的 session.error 事件。事件解析的必填字段为 `sessionID`、`error.name`、`error.data.message`，三者 MUST 为 TrimSpace 后非空的字符串（详情保留原文）；`error.data.statusCode` 与 `error.data.isRetryable` 为可空字段，缺失合法，其中 `statusCode` 仅接受可无损表示为 Go int 的 JSON 整数（小数、超出 int 范围、非数字一律按缺失处理），`isRetryable` 仅接受 JSON 布尔。必填字段缺失、空白或类型非法时 MUST 忽略整个事件；可空字段存在但类型非法时 MUST 仅将该字段降级为缺失，不拒绝整个事件。当任务观察到合法 session.error 时若聚合状态已为 busy，MUST 视为瞬时错误，不打开 error 计时；否则开启 error 计时：1 分钟（固定，不可配置）内聚合回到 busy 视为已恢复 MUST NOT 通知；届满未恢复且本 episode 未通知过则触发 error 类别通知，详情 MUST 包含 message 与 statusCode（缺失时省略该字段）。episode 存续期间的重复 session.error（无论是否同一 session）MUST NOT 延长首个 error 的计时起点，仅更新最新详情。事件解析失败 MUST 静默忽略，MUST NOT 中断事件流或影响其他触发器。

#### Scenario: 错误后 1 分钟未恢复触发通知

- **WHEN** 任务观察到合法 session.error 且聚合非 busy，之后 1 分钟内聚合未回到 busy，本 episode 尚未通知
- **THEN** 系统触发一次 error 类别通知，详情含 message（与 statusCode，若有）

#### Scenario: 错误后快速恢复不通知

- **WHEN** 任务观察到 session.error，1 分钟内聚合回到 busy
- **THEN** 系统 MUST NOT 触发 error 通知

#### Scenario: 聚合已 busy 的瞬时错误不计时

- **WHEN** 任务聚合状态为 busy 时观察到 session.error
- **THEN** 系统 MUST NOT 打开 error 计时

#### Scenario: 不可重试错误终止后仍触发

- **WHEN** 任务观察到 isRetryable=false 的 session.error，随后聚合转为 idle 且 1 分钟内未回到 busy
- **THEN** 系统触发一次 error 类别通知（idle 计时因 episode 存在而被抑制）

#### Scenario: 重复错误不延长计时

- **WHEN** 任务在 error 计时期间再次观察到 session.error
- **THEN** 计时起点不变，仅详情更新为最新一条

#### Scenario: 必填字段缺失忽略整个事件

- **WHEN** 收到缺 `error.data.message` 的 session.error 事件
- **THEN** 系统忽略该事件，事件流与其他触发器不受影响

#### Scenario: 可空字段类型非法仅降级该字段

- **WHEN** 收到 `statusCode` 为非数字的 session.error 事件（其余必填字段合法）
- **THEN** 事件被接受，statusCode 按缺失处理

### Requirement: 通知抑制、启动基线与对账

各触发器的计时与抑制状态 MUST 为进程内存态，MUST NOT 持久化。事件处理、计时届满判定与投递复验 MUST 在单一串行化上下文中执行（单 run loop），对任务运行信息的读取 MUST 为单次组合快照（含任务行、attention、run_status、retry 详情），MUST NOT 分次独立读取。残余竞态窗口（投递副作用执行期间到达新事件）的后果上限为：每个受影响的触发条件可能误发或漏发一次，为已接受语义，MUST NOT 为此引入 revision/代际机制。启动基线语义：ocdeck 启动时已是 pending 的 attention MUST 只用于播种去重集合、不补发通知；启动时已是 idle 的任务 MUST NOT 武装 idle 计时；启动时已处于 retry 的任务 MUST 从启动时刻起重新计时 1 分钟；error 计时 MUST NOT 从启动前的历史恢复。启动基线的 active 任务枚举失败时 MUST 记错误日志并整体禁用通知触发（配置 API 与测试通知仍可用）。事件订阅溢出（丢事件）后的对账 MUST 保留已通知集合与去重集合，仅以当前快照重建计时基准：对账发现的未消费 pending attention MUST 仅播种去重集合、不补发通知（与启动基线一致）；MUST NOT 重发已消费的条件。对账期间枚举或快照读取失败 MUST 进入对账中状态：抑制全部发送并周期重试，成功重建后恢复。任务离开 active 状态时 MUST 取消该任务全部待决计时且不再触发通知。

#### Scenario: 重启后 retry 重新计时

- **WHEN** 任务处于 retry 已达 50 秒时 ocdeck 重启
- **THEN** 重启后从启动时刻重新计时 1 分钟，不基于重启前时长补发

#### Scenario: 启动时不补发已 pending 的 attention

- **WHEN** ocdeck 启动时任务已存在 pending question
- **THEN** 该 request ID 进入去重集合但不触发通知

#### Scenario: 任务挂起取消待决通知

- **WHEN** 任务在 idle 计时未届满时被挂起
- **THEN** 计时器取消，不再触发该任务的 idle 通知

### Requirement: 发送前门禁与投递原子性

每次投递前系统 SHALL 依次复验：触发条件仍成立（任务仍 active、当前状态/注意力/episode 与触发时一致）、总开关开启、该类别开关开启、跳转链接 URL 可用、至少一个渠道已启用且已配置。任一复验失败时 MUST NOT 发生任何副作用（不调用 LLM、不投递任何渠道），且该触发条件按已消费处理。复验全部通过后 MUST 先原子标记本条件已消费，再执行 LLM 总结（若启用）与多渠道并行投递；全部渠道失败 MUST NOT 自动重试，仍计为已消费。单次投递全过程 MUST 使用同一份配置快照，投递期间的配置变更只影响下一次投递。

episode 名额占用仲裁（同一 episode 内 retry/error 合计至多一次投递）MUST 遵循：总开关关闭时不产生任何投递，MUST NOT 占用 episode 名额；类别开关关闭时该触发器不产生投递且 MUST NOT 占用 episode 名额（另一类别仍可投递）；触发条件失效（如 retry 计时期间离开 retry）时该类别计时取消——若此时 episode 仍存续（如 retry→idle），MUST NOT 占用 episode 名额；URL 不可用或无启用渠道的复验失败，以及已发起投递（无论成败），MUST 占用 episode 名额（另一类别本 episode 不再投递）。

#### Scenario: 投递前状态已恢复

- **WHEN** retry 计时届满但复验时发现聚合已回到 busy
- **THEN** 不投递任何渠道，不调用 LLM，该条件计为已消费

#### Scenario: 投递期间配置变更不影响本次

- **WHEN** 一次投递的 LLM 总结进行中，用户保存了新的通知配置
- **THEN** 本次投递继续使用原配置快照完成，新配置从下一次判定起生效

#### Scenario: 全渠道失败不重试

- **WHEN** 一次投递的全部已启用渠道均失败
- **THEN** 记录日志，不重试，该条件计为已消费

### Requirement: 通知渠道投递与降级

系统 SHALL 支持三类通知渠道：网页（web）、Bark 推送（bark）、macOS 本地通知（macos）。每次触发 MUST 向全部"已启用且已配置"的渠道并行投递；单一渠道失败 MUST NOT 影响其他渠道投递，MUST NOT 阻塞或影响任务主流程，失败 MUST 记录日志。渠道抽象 MUST 为渠道无关的通知意图（任务 ID/名称、类别、级别、标题、正文、跳转链接；类别详情由内容组装进入正文，意图不单独携带详情字段），各渠道按声明的能力位（分组 Group / 同键替换 Replace / 撤回 Withdraw）落地；标题统一携带任务名（见「通知内容与跳转链接」的标题格式），分组能力缺失的唯一降级表现为通知中心内不折叠分组，MUST NOT 报错阻断。通知级别 MUST 按类别映射：question/permission → timeSensitive；error → critical；retry → timeSensitive；idle → active；test → active。

渠道"已配置"判定与能力位矩阵 MUST 遵循：web —— 启用即已配置，Caps=Replace（无 Group，标题加任务名前缀），零连接投递计为 failed；bark —— endpoint 与 token 均非空才算已配置（否则 skipped），Caps=Group；macos —— 仅 darwin 且 terminal-notifier 或 osascript 可用才算可用（否则 skipped），terminal-notifier Caps=Group|Replace，osascript Caps=0（标题加任务名前缀）；所有渠道本期 MUST NOT 声明 Withdraw。

#### Scenario: 多渠道并行投递

- **WHEN** 触发一次通知且 bark 与 web 渠道均已启用并配置
- **THEN** 两个渠道各自收到投递

#### Scenario: 单渠道失败隔离

- **WHEN** bark 渠道投递失败（网络错误/凭证失效）
- **THEN** web 渠道投递不受影响，任务主流程不受影响，失败写入日志

#### Scenario: 分组能力缺失自动降级

- **WHEN** 某渠道不支持分组能力
- **THEN** 该渠道正常投递（标题本就统一携带任务名），仅通知中心内不折叠分组，不报错

### Requirement: Bark 渠道

Bark 渠道 SHALL 通过 HTTP POST 向 `<endpoint>/push` 发送 JSON 请求体（endpoint 尾部 `/` 拼接前剔除），`Content-Type: application/json`。请求体 MUST 包含：`device_key`（值为配置的 token）、`title`、`body`、`level`、`group`（值为 `ocdeck/<任务名>`，任务名截断至 40 字符——Bark 为通用推送工具，分组名 MUST 人类可读且自带来源标识）、`url`（值为 `Intent.URL`，目标页推导规则见「通知内容与跳转链接」）。endpoint 与 token MUST 可配置，endpoint 默认 `https://api.day.app`，支持自建 server。单次请求超时 MUST 为 10 秒，MUST NOT 跟随重定向，MUST NOT 自动重试，响应体读取 MUST 有 64 KiB 大小上界，超限 MUST 判定为失败。投递成功判定 MUST 为：HTTP 2xx 且响应体 JSON 的 `code` 字段等于 200；响应体非法 JSON 或缺 `code` 字段 MUST 判定为失败。token、请求体与 Bark 原始响应体 MUST NOT 写入日志。

#### Scenario: 发送 Bark 推送

- **WHEN** bark 渠道已启用且配置了 endpoint 与 token，触发一次通知
- **THEN** 系统向 `<endpoint>/push` POST JSON，body 含 device_key/title/body/level/group/url，level 按类别映射

#### Scenario: 未配置 token 视为未配置

- **WHEN** bark 渠道开关打开但 token 为空
- **THEN** 该渠道按未配置处理，不投递、不报错

#### Scenario: 推送失败判定

- **WHEN** Bark server 返回非 2xx，或响应体 JSON 的 code 非 200，或响应体非法
- **THEN** 该次投递判定为失败，记录日志（不含 token 与响应原文），不影响其他渠道

### Requirement: 网页通知渠道

网页渠道 SHALL 由服务端 WebHub 经 SSE 向已连接前端投递通知意图，前端使用浏览器 Notification API 展示系统通知。SSE 帧格式 MUST 为 `event: notification` + 单行 `data: <JSON>`，JSON 字段名唯一表述如下（snake_case）：

```json
{"task_id": "...", "task_name": "...", "category": "question|permission|idle|retry|error|test", "level": "active|timeSensitive|critical", "title": "...", "body": "...", "url": "..."}
```

前端 MUST 仅在 web 渠道已启用且通知权限已授予时建立该 SSE 连接；断线重连 MUST NOT 重放历史通知。通知 MUST 携带 `Intent.URL`（目标页推导规则见「通知内容与跳转链接」），点击通知 MUST 聚焦窗口并导航到该 URL 对应的目标页。支持 `tag` 的浏览器 MUST 以任务标识为 tag；多标签页同时在线时各连接均收到投递，由浏览器 tag 替换语义收敛（Safari 等 tag 无效的浏览器可能出现多条，为已接受降级）。渠道投递结果判定 MUST 为：WebHub 至少接纳一个已连接前端为成功；零连接为失败（不影响其他渠道）。

#### Scenario: 前端展示通知

- **WHEN** 前端已连接通知 SSE 且已授权，收到一帧通知意图
- **THEN** 浏览器弹出系统通知，内容含任务名与类别

#### Scenario: 点击跳转目标页

- **WHEN** 用户点击网页通知
- **THEN** 窗口聚焦并导航到该通知 `Intent.URL` 对应的目标页（真实类别为 `#/task/<任务ID>`，test 为 `#/configs#notifications`）

#### Scenario: 无连接前端计为该渠道失败

- **WHEN** 触发通知时 WebHub 无任何已连接前端
- **THEN** 网页渠道本次投递计为失败，其他渠道不受影响

#### Scenario: 断线重连不重放

- **WHEN** 前端 SSE 断线后重连
- **THEN** 不补发断线期间的通知

### Requirement: macOS 本地通知渠道

macOS 渠道向运行 ocdeck-server 的 Darwin 主机投递本地通知，仅 darwin 运行环境可启用。系统 SHALL 以 `exec.LookPath("terminal-notifier")` 探测：存在时使用 terminal-notifier（以 `ocdeck/<任务名>`（任务名截断至 40 字符）为 `-group` 实现同任务替换，`-open` 携带跳转链接）；仅当未找到 terminal-notifier 时才选用 osascript `display notification`。terminal-notifier 已存在但执行失败时 MUST NOT 降级到 osascript，仅记录失败。两种实现 MUST 均不使用 shell（argv 直传），osascript 必须使用固定脚本模板经 argv 传入标题与正文，进程 MUST 有统一硬超时与输出大小上界。非 darwin 或两者均不可用时该渠道视为不可用，MUST NOT 影响其他渠道。

#### Scenario: 优先 terminal-notifier

- **WHEN** 本机存在 terminal-notifier 且 macOS 渠道已启用，触发一次通知
- **THEN** 经 terminal-notifier 发送，同任务的通知替换上一条，点击打开 `Intent.URL` 指定的目标页

#### Scenario: 未安装时降级 osascript

- **WHEN** 本机不存在 terminal-notifier，触发一次通知
- **THEN** 经 osascript display notification 发送

#### Scenario: terminal-notifier 执行失败不再降级

- **WHEN** terminal-notifier 存在但执行返回非零
- **THEN** 本次 macOS 投递计为失败并记日志，不再调用 osascript

### Requirement: 通知内容与跳转链接

每条通知 MUST 包含：任务名称、通知类别（人类可读）、类别详情。通知标题格式 MUST 为 `[ocdeck] [<任务名>] <类别标题>`（通知可能经 Bark 等通用渠道与其他应用的通知混合呈现；任务名截断至 12 字符，过短时原样使用），test 类别任务名为 `ocdeck`。类别详情规则：question 为提问内容（多条提问拼接，超出长度截断）；permission 为权限名称与 patterns（多个 patterns 拼接，超出长度截断）；error 为 message（与 statusCode，若有）；retry 为 attempt 与 message（不可得时用固定降级文案）；idle 为已空闲时长；test 为固定文案「通知链路测试」。任何详情字段缺失时 MUST 使用固定降级文案或省略该字段，MUST NOT 出现空白详情。五个真实类别的通知 MUST 携带任务页跳转链接 `#/task/<任务ID>`；test 类别 MUST 携带设置页链接 `#/configs#notifications`（完整 URL 推导规则见设计）。支持跳转的渠道 MUST 实现点击直达对应页面。

#### Scenario: 通知内容完整

- **WHEN** 触发任意类别通知
- **THEN** 通知内容含任务名称、类别与该类别详情

#### Scenario: 携带跳转链接

- **WHEN** 触发通知
- **THEN** 通知意图含其类别对应的目标页面深链（真实类别为任务页 `#/task/<任务ID>`，test 为设置页 `#/configs#notifications`）

### Requirement: LLM 停止原因总结（可选增强）

系统 SHALL 提供 LLM 总结开关（默认关闭）。开关打开且 AI 配置可用时，通知正文 MUST 附带由 LLM 生成的停止原因总结，总结输入 MUST 仅为任务名、类别与类别详情（不拉取会话消息）。LLM 调用总预算 MUST 有 5 秒上界；调用失败、超时、未配置、返回空白或超长输出时 MUST 降级为确定性摘要（类别详情），MUST NOT 因此失败或丢失通知，MUST NOT 延迟投递超过该上界。

#### Scenario: LLM 总结成功

- **WHEN** LLM 总结开关打开、AI 配置可用且调用在预算内成功
- **THEN** 通知正文含 LLM 生成的停止原因总结

#### Scenario: LLM 失败降级

- **WHEN** LLM 总结开关打开但调用失败、超时或输出不可用
- **THEN** 通知以确定性摘要在预算内正常投递

#### Scenario: 默认不启用

- **WHEN** LLM 总结开关关闭
- **THEN** 通知仅含确定性摘要，不发起任何 LLM 调用

### Requirement: 通知配置存储

系统 SHALL 将通知配置存储于 `<dataDir>/notification.json`，写入 MUST 采用临时文件 + 原子 rename，文件权限 MUST 为 0600。磁盘 schema 的唯一表述如下（字段均为必填键，未知字段 MUST 忽略）：

```json
{
  "enabled": false,
  "categories": {"question": true, "permission": true, "idle": true, "retry": true, "error": true},
  "idle_timeout_seconds": 60,
  "channels": {
    "web":   {"enabled": false},
    "bark":  {"enabled": false, "endpoint": "https://api.day.app", "token": ""},
    "macos": {"enabled": false}
  },
  "llm_summary": false,
  "base_url": ""
}
```

校验规则：`idle_timeout_seconds` MUST ∈ [10, 3600]；`endpoint` 与 `base_url` 非空时 MUST 为有非空 host 的 http(s) hierarchical URL，且 MUST NOT 含 userinfo、query、fragment，path 仅允许空或 `/`；校验失败 MUST 拒绝保存并返回 422（invalid_input）。bark token MUST NOT 在日志或 API 响应中以明文完整出现。

#### Scenario: 保存合法配置

- **WHEN** 用户提交合法的通知配置
- **THEN** 配置以 0600 权限原子写入 `<dataDir>/notification.json` 并立即生效

#### Scenario: 拒绝非法配置

- **WHEN** 提交的空闲阈值越界或 bark endpoint/base_url 非法
- **THEN** 保存被拒绝并返回 422，原配置不变

#### Scenario: 未知字段忽略

- **WHEN** 提交的配置含 schema 外未知字段
- **THEN** 未知字段被忽略，已知字段正常校验与保存

### Requirement: 通知配置读写 API

系统 SHALL 提供通知配置的读取与保存接口（`GET/PUT /api/v1/notification/config`），鉴权与 server 其他管理 API 一致。

GET MUST 返回 200 与配置对象，JSON 形状唯一表述如下（snake_case；`token_masked` 替代 `token`）：

```json
{
  "enabled": false,
  "categories": {"question": true, "permission": true, "idle": true, "retry": true, "error": true},
  "idle_timeout_seconds": 60,
  "channels": {
    "web":   {"enabled": false},
    "bark":  {"enabled": false, "endpoint": "https://api.day.app", "token_masked": ""},
    "macos": {"enabled": false}
  },
  "llm_summary": false,
  "base_url": ""
}
```

`load_error` 为只读字段，仅在配置损坏或不可读时出现（非空字符串），正常读取时 MUST 省略该字段。`token_masked` 掩码规则与 ai-provider-config 的 api_key 一致（≥8 位回显前 4 位 + `***`，<8 位纯 `***`，无 token 为空串），响应中 MUST NOT 出现完整 token。配置文件不存在时返回默认配置（即存储 schema 所示默认值：总开关关闭、类别全开、阈值 60、渠道全关）；配置文件损坏或不可读时 MUST NOT 返回 500，而是返回默认配置并携带人类可读的 `load_error` 字段。

PUT 请求体 JSON 形状与 GET 相同，仅两处差异：bark 渠道的令牌字段名为 `token`（非 `token_masked`）；MUST NOT 含 `load_error`（只读字段，出现按未知字段忽略）。token 语义：值为空字符串或任意含 `***` 的字符串 MUST 视为掩码并保留已存储的原 token；无已存储 token 时按空处理，该渠道视为未配置，不因此拒绝保存。成功返回 200 与 GET 同形响应（含 `token_masked`，不含 `load_error`）；请求体非法 JSON 返回 400；业务校验失败返回 422；文件写入失败返回 500 且 MUST 保持旧内存快照不变。并发 PUT MUST 由写锁串行化「合并 → 校验 → 原子写入 → 快照替换」全过程，按写锁获得顺序 last-writer-wins。

#### Scenario: 读取默认配置

- **WHEN** 从未保存过通知配置，客户端请求 GET
- **THEN** 返回 200 与默认配置

#### Scenario: 损坏配置降级读取

- **WHEN** notification.json 损坏，客户端请求 GET
- **THEN** 返回 200、默认配置与 load_error，不返回 500

#### Scenario: 保存时保留原 token

- **WHEN** 用户仅修改类别开关并以掩码值提交 bark token
- **THEN** 保存成功，已存储的 token 不被清空

#### Scenario: 写失败保持旧快照

- **WHEN** PUT 校验通过但文件写入失败
- **THEN** 返回 500，内存中的配置快照保持旧值不变

### Requirement: 配置运行时生效

系统 SHALL 在 server 启动时加载通知配置到内存快照：文件不存在为默认配置态；文件损坏/不可读时 MUST 记录 load_error 并日志告警，MUST NOT 拒绝启动。PUT 保存成功后 MUST 原子替换内存快照，后续通知判定立即使用新配置，无需重启。总开关关闭时 MUST 不触发任何通知；类别开关关闭时该类别 MUST 不触发；渠道开关关闭或未配置时该渠道 MUST 不投递。

#### Scenario: 配置热更新

- **WHEN** 用户保存新的通知配置
- **THEN** 内存快照原子替换，下一次通知判定立即使用新配置

#### Scenario: 总开关关闭

- **WHEN** 总开关关闭，任务出现 pending question
- **THEN** 不触发任何通知

#### Scenario: 类别开关关闭

- **WHEN** error 类别开关关闭，任务观察到 session.error 且超时未恢复
- **THEN** 不触发 error 通知，其他类别触发器不受影响

#### Scenario: 损坏配置不阻断启动

- **WHEN** server 启动时 notification.json 损坏
- **THEN** server 正常启动，通知按默认配置（总开关关闭）运行，日志告警

### Requirement: 通知设置界面

前端 SHALL 在设置页（`#/configs`）新增通知子标签（深链 `#/configs#notifications`），MUST NOT 保留独立页面。界面 MUST 包含：总开关、五个类别的独立开关、空闲阈值输入、三个渠道的独立开关与参数（bark：endpoint/token；web：浏览器权限状态展示与申请入口）、LLM 总结开关、`base_url` 输入（附"Bark 在手机上打开链接需要可达地址"提示）、保存按钮与测试通知入口。保存成功/失败 MUST 有明确反馈；GET 返回 load_error 时 MUST 在界面展示。

#### Scenario: 配置通知

- **WHEN** 用户在通知子标签开启总开关与 bark 渠道、填写 token 并保存
- **THEN** 界面提示保存成功，配置立即生效

#### Scenario: 深链直达

- **WHEN** 用户打开 `#/configs#notifications`
- **THEN** 设置页打开并直接选中通知子标签

#### Scenario: 展示配置加载错误

- **WHEN** 配置文件损坏，用户打开通知子标签
- **THEN** 界面展示 load_error 提示，可直接重新保存合法配置修复

### Requirement: 测试通知

系统 SHALL 提供测试通知入口（`POST /api/v1/notification/test` 与设置页按钮），向全部已启用且已配置的渠道投递一条标识为测试的通知。测试通知 MUST 使用专用局部变体：TaskID 为 `notification-test`、任务名为 `ocdeck`、类别为 test（级别 active）、详情为固定文案「通知链路测试」、跳转链接为设置页 `#/configs#notifications`。测试通知 MUST 跳过任务 active 复验与类别开关检查，仅检查总开关、URL 可用性与渠道启用/配置；MUST 与真实通知走同一投递链路（含级别映射与降级），但 MUST NOT 调用 LLM 总结（test 类别正文为固定文案，不受 llm_summary 开关影响）。响应 MUST 为 200 与逐渠道结果对象，顶层形状唯一表述为 `{"results": [{"name": "...", "status": "success|failed|skipped", "error": "..."}]}`（`status` 枚举：`success` | `failed` | `skipped`，skipped 表示未启用或未配置；`error` 为失败原因，成功或跳过时为空字符串）；各渠道的成功判定 MUST 与该渠道真实投递的判定一致（web 渠道为 WebHub 至少接纳一个已连接前端）。总开关关闭时 MUST 返回 422（invalid_state 或项目惯例等价码）并提示先开启总开关。

#### Scenario: 发送测试通知

- **WHEN** 用户在设置页点击测试通知且 bark 渠道已启用并配置
- **THEN** bark 渠道收到一条标识为测试的通知，响应报告该渠道成功

#### Scenario: 测试通知逐渠道报告

- **WHEN** bark 配置错误且 web 有已连接前端，用户发送测试通知
- **THEN** 响应中 bark 为失败（含原因）、web 为成功

#### Scenario: 总开关关闭时拒绝测试

- **WHEN** 总开关关闭，用户发送测试通知
- **THEN** 返回 422 并提示先开启总开关
