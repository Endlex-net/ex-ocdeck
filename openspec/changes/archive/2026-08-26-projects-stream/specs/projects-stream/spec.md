# projects-stream 变更 Delta

## ADDED Requirements

### Requirement: 项目任务树 SSE 推送端点

系统 SHALL 提供 `GET /api/v1/projects/stream` 端点，鉴权方式与其他 `/api/v1/*` 管理 API 一致（Bearer token）。端点 MUST 以 `text/event-stream` 推送，所有数据帧的 data MUST 为与 REST `GET /api/v1/projects` 响应体完全同构的**裸数组**（元素结构见 project-management spec「项目列表与详情」，含每个项目的 `tasks` 摘要数组；摘要元素含 `agentStatus`——来自内存快照、不可用时省略——与 `attention_count`）。建连时序 MUST 为：认证通过 → 对领域 topic `task`/`session`/`serve_runtime`/`control` 各订阅一次并 fan-in（任一路溢出视为溢出）→ 再组装初始快照；组装期间到达且通过本场景消费过滤的事件 MUST 置脏标记。初始快照组装失败时 MUST 四路全部退订并返回 500 标准错误信封，MUST NOT 写入 SSE 响应头。组装成功 MUST 写 200 与 SSE 响应头、发送完整 `event: snapshot` 帧并随即 flush（首帧 MUST 立即可达）；若脏标记已置位 MUST 紧接着进入合并窗口补发 `event: update`。此后事件到达 MUST 经合并窗口（固定 500ms，与活跃会话流一致；后续调整须另走规格变更）合并，窗口到期以最新全量快照发送 `update` 帧；组装失败 MUST 跳过本次发送、保持脏标记并在后续事件或心跳 tick 重试，MUST NOT 关闭连接。订阅溢出信号置位时 MUST 先置脏标记再立即触发一次窗口外全量快照重推；写/flush 失败按统一写路径立即退订退出。无事件期间 MUST 以心跳注释行维持连接（默认 25s；任一成功写出的业务帧 MUST 重新起算心跳）。所有帧 MUST 经统一写路径写出并检查写与 flush 错误。帧组装 MUST 复用与 REST 端点相同的读模型组装逻辑。推送路径 MUST 为纯读操作，MUST NOT 实时调用 opencode 接口，MUST NOT 产生任何写副作用。端点 MUST 在客户端断开或服务进程 context 取消时释放订阅并退出 handler；服务端关停 MUST 先取消活跃 stream 再执行 HTTP Shutdown。路由 MUST 以字面路径注册：请求 `GET /api/v1/projects/stream` MUST 由本端点处理，MUST NOT 被 `GET /api/v1/projects/{id}` 通配模式吞没。

#### Scenario: 连接即收快照

- **WHEN** 已认证客户端建立 SSE 连接
- **THEN** 客户端立即收到一帧 `snapshot`，data 为当前项目任务树裸数组（无项目时为 `[]` 非 `null`），无需等待心跳

#### Scenario: 帧与 REST 同构

- **WHEN** 客户端先后请求 `GET /api/v1/projects` 与订阅本端点并接收首帧
- **THEN** 两者 data/响应体的项目数组完全同构，任务摘要字段（含 `agentStatus` omitempty 与 `attention_count`）逐字段一致

#### Scenario: 路由不被项目详情通配吞没

- **WHEN** 客户端请求 `GET /api/v1/projects/stream`（已认证）与 `GET /api/v1/projects/{id}`（真实项目 ID）
- **THEN** 前者按 SSE 端点处理（返回 `text/event-stream`），后者按项目详情处理（返回 JSON 项目对象），互不串扰

#### Scenario: 订阅先于首次组装

- **WHEN** 建连过程中组装初始快照期间发生通过本场景消费过滤的领域事件
- **THEN** snapshot 帧发送后紧接补发一帧 `update`，变更不丢失

#### Scenario: 初始组装失败

- **WHEN** 建连时初始快照组装因底层查询失败
- **THEN** 响应为 500 标准错误信封，不写入 SSE 响应头，订阅被释放

#### Scenario: 事件驱动更新

- **WHEN** 连接存续期间某任务状态发生真实迁移（如 `suspended → activating`）
- **THEN** 客户端在合并窗口到期后收到一帧 `update`，data 为该变更后的最新全量裸数组

#### Scenario: 窗口内多次变更合并

- **WHEN** 500ms 合并窗口内到达多个被本场景消费过滤标脏的领域事件
- **THEN** 客户端仅收到一帧 `update`，data 为窗口到期时刻的全量快照

#### Scenario: 心跳维持连接且业务帧重置

- **WHEN** 连接存续且超过心跳间隔无任何业务帧
- **THEN** 客户端收到 `: ping` 注释行，连接保持打开；任一成功写出的业务帧后心跳间隔重新起算

#### Scenario: 组装失败不断连且可重试

- **WHEN** 某次 update 组装时底层读模型查询失败
- **THEN** 该帧被跳过并记录日志，脏标记保持，连接保持，后续事件或心跳 tick 触发重试

#### Scenario: 事件溢出自愈

- **WHEN** 订阅缓冲溢出导致事件被丢弃、溢出信号置位
- **THEN** 服务端立即推送一次最新全量快照，客户端状态自愈；若该次组装失败，脏标记保持并由后续事件或心跳 tick 重试至成功；若写/flush 失败，连接关闭，客户端重连后经 snapshot 自愈

#### Scenario: 未认证访问被拒

- **WHEN** 请求缺失或携带错误 token
- **THEN** 返回 401，不建立事件流，不泄露任何资源信息

#### Scenario: 推送无写副作用

- **WHEN** SSE 连接存续并发生多次推送
- **THEN** 数据库内容、任务状态机与进程集合除外部因素外不发生变化，且推送路径不发起任何 opencode 调用

#### Scenario: 服务关停及时释放

- **WHEN** SSE 连接存续期间服务进程 context 被取消
- **THEN** handler 退出、订阅释放，HTTP Shutdown 在其预算内完成，不拖到超时

### Requirement: 全状态任务树消费过滤

projects 场景适配器 MUST 按下列口径判定领域事件是否标脏，标脏后 MUST 重组装全量快照重推，MUST NOT 按 Payload 做增量合并：

- `task.created`、`task.status_changed`（**任意** from/to 迁移，含两端均非 `active` 的过渡/失败/归档迁移）、`task.deleted`（**任意** `from`，含非 active 任务的删除）、`task.activity_changed`：MUST 标脏——本场景呈现全部非删除态任务，任一任务的进入、迁移、离开或可见字段变化都改变任务树投影；
- 全部 `session.*`（`session.claimed`/`session.touched`/`session.deleted`/`sessions.aligned`）、`serve_runtime.attention_changed`、`serve_runtime.run_status_changed`、`resync.requested`、未知 `Type`：MUST 标脏（保守标脏，避免漏推）；
- 任一路订阅溢出信号置位：MUST 先置脏再触发窗口外全量重推。

本场景与活跃会话流（active-sessions-stream spec）的消费过滤 MUST 相互独立定义、互不影响：本场景为全状态任务树视图（侧栏/项目管理页），活跃会话流为 active-only 视图（指挥中心）。项目 CRUD（创建/重命名/删除项目）不产生领域事件（本能力不新增 project 事件），该类变更不经本流感知，由前端低频兜底轮询覆盖（见「前端 projects store 订阅与兜底轮询」Requirement）。

#### Scenario: task.created 触发更新

- **WHEN** 某项目下创建新任务（`task.created` 发布）
- **THEN** 本流在合并窗口后推送 `update`（任务树新增行），而已连接的活跃会话流不因此单独推送 `update`

#### Scenario: 非 active 迁移触发更新

- **WHEN** 发生两端均非 `active` 的状态迁移（如 `suspended → archived`、`creating → creation_failed`）或 `from != active` 的 `task.deleted`
- **THEN** 本流推送 `update`（树内该任务行变化/移除），活跃会话流不因此推送

#### Scenario: 跨越 active 边界的迁移触发更新

- **WHEN** 发生 `(from==active) != (to==active)` 的 `task.status_changed`
- **THEN** 本流与活跃会话流均推送 `update`

#### Scenario: resync 与未知 Type 保守标脏

- **WHEN** 到达 `resync.requested` 或未知 `Type` 事件
- **THEN** 本流标脏并在窗口/立即重推路径推送最新全量快照

### Requirement: SSE 循环核心复用

`GET /api/v1/projects/stream` 与 `GET /api/v1/tasks/active/stream` MUST 共享同一 SSE 循环核心——订阅时序（先订阅再组装、组装期 drain 判脏）、fan-in、初始组装失败 500 语义、合并窗口、溢出重推、心跳间隔与业务帧重置语义、统一写路径与写/flush 错误退出、客户端断开/进程 context 取消释放——场景差异 MUST 仅体现为快照组装函数与消费过滤函数两个注入点（日志前缀/500 文案等展示参数允许经构造配置传入，不构成行为注入点）；MUST NOT 平行复制事件循环实现。

#### Scenario: 双端点纪律一致

- **WHEN** 对两端点注入相同的测试事件序列、组装桩与时间参数（合并窗口/心跳）
- **THEN** 观察到的帧序列语义一致：snapshot 首帧立即可达、窗口合并、溢出窗口外重推、业务帧后心跳重置、写失败立即退出

#### Scenario: 场景差异仅过滤与组装

- **WHEN** 同一领域事件（如 `task.created`）到达两条存续连接
- **THEN** 两端点行为差异仅由各自消费过滤决定（本流推送 update、活跃会话流不推送），循环纪律本身无差异

### Requirement: 前端 projects store 订阅与兜底轮询

App 层共享 projects store MUST 以 `GET /api/v1/projects/stream` 的 SSE 订阅（fetch + ReadableStream，携带 `Authorization: Bearer`）获取 projects 快照：`snapshot` 与 `update` 帧走同一解析路径并以帧内裸数组整表替换 store 快照；订阅的重连退避与 401 语义 MUST 与活跃会话订阅一致（非 401 错误指数退避 1s 起步、上限 30s、首个有效帧到达后重置；401 清除 token 并广播未授权事件且不再重连）。store MUST NOT 保留固定 5 秒轮询。store MUST 保留常驻低频兜底轮询（固定 30s、single-flight：并发请求合并为一次在途）：项目 CRUD（创建/重命名/删除项目）不产生领域事件，兜底轮询 MUST 覆盖项目集合变更。store 的 `refresh()`（trailing 语义）MUST 保持：变更操作成功后调用即触发一次立即加载，若调用时已有加载在途 MUST 在其结束后补发一次，承诺反映调用之后的最新状态。订阅或兜底轮询失败 MUST 保留上次成功数据并进入 store 错误通道，侧栏与页面 MUST NOT 闪现空态。消费方（壳层侧栏、指挥中心、项目管理页）MUST 继续从同一 store 读取，MUST NOT 各自建立第二条 projects 订阅或轮询。

#### Scenario: 订阅替代高频轮询

- **WHEN** 应用加载并完成认证
- **THEN** store 建立 projects SSE 订阅，首帧后快照可用，不存在对 `/api/v1/projects` 的固定 5 秒轮询请求

#### Scenario: 任务事件经流收敛

- **WHEN** store 订阅存续期间收到 `snapshot` 或 `update` 帧
- **THEN** store 以帧内容整表替换 projects 快照，消费方随之收敛

#### Scenario: 项目 CRUD 经 refresh 立即收敛

- **WHEN** 用户在项目管理页成功执行创建/重命名/删除项目操作
- **THEN** 变更操作成功后调用 `refresh()`，store 以补发请求的最新结果收敛，不等待兜底轮询周期

#### Scenario: 外部项目变更经兜底轮询收敛

- **WHEN** 项目集合发生不经任何领域事件与用户操作的变更（如外部脚本直接注册项目）
- **THEN** 该变更在至多一个兜底轮询周期（30s）加一次在途请求的时间内反映到 store 快照

#### Scenario: 断线退避重连

- **WHEN** projects SSE 连接因网络或服务端原因断开
- **THEN** store 按指数退避自动重连，期间保留上次成功数据并进入错误通道；重连成功收到有效帧后以新快照收敛

#### Scenario: 401 停止重连

- **WHEN** projects SSE 连接收到 401
- **THEN** 应用清除 token、进入未授权流程，且不再发起重连
