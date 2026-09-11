# Terminal File Delivery Specification

## Purpose

让浏览器侧的文件/图片（粘贴或拖入）经上传通道进入 server 受管存储，并投递到对应任务的 opencode TUI 终端，由其以原生附件行为消费，使远程部署下"给 AI 看截图/文件"可用。

## Requirements

### Requirement: 终端入口范围与文件队列

系统 SHALL 仅在 opencode TUI 终端（TerminalView 的 TUI 实例）提供文件上传与投递入口（粘贴、拖入两种常态入口；文件选择器无常驻入口按钮，仅用于失败/未知项的重试重选），shell 终端 MUST NOT 挂载任何入口。两个常态入口 MUST 进入同一个文件队列，共用输入门禁、连接归属（connId）、投递状态机与重试逻辑。server 侧 MUST 拒绝来自 shell 终端连接的投递请求。

前端 MUST 以 auth_ok 携带的 connId 作为文件能力判据：auth_ok 缺失或 connId 非法（空串/非 uuid 形态）时，WS 连接照常建立、普通终端功能不受影响，但文件功能 MUST 置为不可用——常态入口捕获到文件时提示"当前 server 版本不支持文件投递"，MUST NOT 发起 HTTP 上传、MUST NOT 发送 deliver 帧，MUST NOT 以空串/undefined/自造值充当 connId。connId 有效但上传接口返回 404（业务 404——如任务已删除——与旧 server 无该路由不可区分）时 MUST 提示"上传目标不可用，任务可能已删除或服务端不支持该接口"、终止当前上传项，MUST NOT 把整个连接的文件能力永久降级（后续新上传仍可尝试）。

#### Scenario: shell 终端无入口且投递被拒

- **WHEN** 用户在 shell 终端尝试粘贴文件或拖入文件，或 shell 终端的 WS 连接收到 deliver 帧
- **THEN** 浏览器侧不触发任何上传/投递流程（无入口）；server 侧对 shell 连接的 deliver 帧拒绝且 PTY 无写入

#### Scenario: 常态入口共用队列

- **WHEN** 用户分别通过粘贴、拖入发起文件投递
- **THEN** 两种来源的文件进入同一队列，门禁、状态展示与重试行为一致

#### Scenario: 旧 server 无 connId 时文件功能降级

- **WHEN** auth_ok 帧缺失 connId 或其值非法（空串/非 uuid 形态），用户在终端区域粘贴/拖入文件
- **THEN** WS 连接与普通终端功能不受影响；前端提示"当前 server 版本不支持文件投递"，不发起 HTTP 上传、不发送 deliver 帧，不以空串/undefined/自造值充当 connId

#### Scenario: 上传 404 终止当前项且不永久降级

- **WHEN** connId 有效但上传接口返回 404（业务 404——如任务已删除——与旧 server 无该路由不可区分）
- **THEN** 提示"上传目标不可用，任务可能已删除或服务端不支持该接口"，当前上传项终止、不发送 deliver 帧；文件能力不因此永久降级，后续新上传仍可尝试

### Requirement: 粘贴文件捕获与上传

系统 SHALL 在 TUI 终端区域捕获剪贴板中含文件的粘贴事件并上传进入投递流程。剪贴板提取 MUST 按 `clipboardData.items` 顺序提取有效 File；存在有效 items 时 MUST NOT 合并 `files` 兜底，仅当 items 中无有效 File 时才读取 `files`。纯文本粘贴 MUST NOT 被拦截，行为与现状完全一致；混合剪贴板（文件与文本并存）MUST 以文件为准，文本表示 MUST NOT 被插入终端。同一剪贴板条目 MUST NOT 被重复上传；系统 MUST NOT 按文件名或大小推断去重。

#### Scenario: 粘贴剪贴板图片

- **WHEN** 用户在 TUI 终端区域粘贴含图片的剪贴板内容（如截图）
- **THEN** 图片被上传并进入投递流程，终端不插入该图片的文本表示

#### Scenario: 纯文本粘贴行为不变

- **WHEN** 用户粘贴仅含文本的剪贴板内容
- **THEN** 走既有文本粘贴通道，无上传发生，行为与引入本能力前一致

#### Scenario: items 有效时不读 files 兜底

- **WHEN** 剪贴板 items 中存在有效 File，且 `files` 列表与 items 存在口径差异
- **THEN** 仅按 items 顺序提取，不合并 `files`，不因口径差异产生重复上传

#### Scenario: 混合剪贴板以文件为准

- **WHEN** 剪贴板同时含文件与其文本表示（如文件管理器中复制文件）
- **THEN** 仅文件被上传投递，文本表示不被插入终端

### Requirement: 拖入文件捕获与上传

系统 SHALL 支持将文件拖入 TUI 终端区域并上传：拖拽悬停终端区域时 MUST 给出可投放的视觉反馈，drop 时读取实际文件并上传；文件拖拽 MUST NOT 触发浏览器导航。混合拖入（文件与目录并存）MUST 逐项处理：目录项分项拒绝并明确提示，普通文件继续正常处理；系统 MUST NOT 遍历目录。

#### Scenario: 拖入图片文件

- **WHEN** 用户将图片文件拖入终端区域并释放
- **THEN** 文件被上传并进入投递流程

#### Scenario: 混合拖入目录分项拒绝

- **WHEN** 用户同时拖入一个目录与一个普通文件并释放
- **THEN** 目录项被拒绝并提示不支持目录，普通文件继续上传投递；目录内容不被遍历

#### Scenario: 拖拽不触发浏览器导航

- **WHEN** 用户将文件拖入终端区域（含释放点在子元素上）
- **THEN** 浏览器不发生页面导航，终端区域给出正确的悬停反馈

### Requirement: 文件上传接口与安全约束

server SHALL 提供 `POST /api/v1/tasks/{taskID}/attachments`（multipart/form-data）上传接口。请求的**第一个 part MUST 为 `connId` 文本字段**，随后恰好一个字段名为 `file` 的文件 part；首 part 非 `connId`、出现其它字段或多文件 part MUST 拒绝。成功返回 201 与 `{"uploadId":"<32 hex>"}`，客户端原始文件名仅作展示元数据（取自 part header），MUST NOT 进入落盘路径。接口 MUST 复用现有 Bearer 鉴权（401 `unauthorized`）并复用既有 Origin 白名单判定（与 WS `checkWSOrigin` 同一白名单语义；非法来源 403 `forbidden`，且 MUST 先于任何文件写入）。错误统一现有信封 `{"error":{"code","message"}}`：404 `not_found`（任务不存在）、409 `invalid_state`（任务非活跃）、403 `forbidden`（Origin 非法，或 connId 不是该任务 TUI 终端当前连接 ID）、400 `invalid_input`（multipart 畸形 / 首 part 非 `connId` / 无 `file` part / 多文件 part / 非文件开销超限 / 零字节以外的格式问题）、413 `payload_too_large`（文件 payload 或请求总量超上限）、500 `internal`（磁盘写失败，或任务查询/存储基础设施故障——只有已确认不存在/非活跃才映射 404/409，基础设施故障 MUST NOT 误映射为 404/409）、408 `invalid_input`（上传停滞达 1 小时被取消收尾：HTTP 尚可响应时固定返回 408，message 固定为 `"upload stalled for 1 hour"`——本错误表为该文案唯一来源；客户端已断开则不保证响应。取消收尾流程见「保留生命周期与回收」）。

校验与解析 MUST 固定分阶段执行：① 认证 → Origin → 任务存在/活跃（均先于请求体解析；共享任务状态准入表见「连接归属与投递准入」：仅 `status == active` 通过，任务不存在 → 404 `not_found`，非 active → 409 `invalid_state`）；② 有界解析请求体，第一个 part MUST 为 `connId` 文本字段（否则 400 `invalid_input`，文件内容 MUST NOT 落盘）；③ connId 归属校验（非该任务 TUI 终端当前连接 → 403 `forbidden`）；④ 解析唯一 `file` part 并流式写 `.partial`（计量规则见下方唯一计量表）；⑤ 校验 multipart 完整结束 boundary、拒绝额外 part/字段（400 `invalid_input`，清理 `.partial`）；⑥ 原子 rename 提交 → 返回 201 uploadId（提交顺序与发布点：sidecar 就绪后才加入可投递索引，见「保留生命周期与回收」）。任一步失败 MUST NOT 留下可投递的半成品文件。前端 FormData 构造顺序 MUST 固定：先 `append("connId", ...)` 再 `append("file", ...)`。

计量规则（唯一计量表，design 按本表引用；冲突优先级：按协议解析顺序首次确定的违规返回对应错误，底层 read-ahead MUST NOT 改变结果）：

| 计量域 | 字节域定义 | 上限 | 超限错误 |
|---|---|---|---|
| 文件 payload N | 落盘的原始 file part 字节数，按**不解码的 raw part 语义**读取；file part 携带非 identity 的 Content-Transfer-Encoding（如 quoted-printable）→ 400 `invalid_input`，不得隐式解码改变计量 | `0 ≤ N ≤ M`（M 为配置上限，默认 20971520，合法配置范围 `1MiB ≤ M ≤ 100MiB`） | N > M → 413 `payload_too_large`（流式 `M+1` 探测） |
| 非文件开销 | multipart 线上字节中除 file part 内容外的全部：preamble、boundary 行、part headers、CRLF、connId 字段值、epilogue | 64KiB | 400 `invalid_input` |
| 请求总量（防御兜底） | 整个请求体（`http.MaxBytesReader`）；合规请求理论最大 M+64KiB，M+1MiB 与其差 960KiB，仅作兜底 | M + 1MiB | 413 `payload_too_large` |

读到最终 boundary 后 MUST 继续有界消费请求体至 EOF（计入总量兜底），再原子 rename 提交。server MUST NOT 仅信任 Content-Length。

#### Scenario: 未认证上传被拒绝

- **WHEN** 未携带有效 token 的请求调用上传接口
- **THEN** 返回 401 `unauthorized`，无文件落盘

#### Scenario: 非法 Origin 上传被拒绝

- **WHEN** Origin 不在白名单的请求调用上传接口
- **THEN** 返回 403 `forbidden`，先于任何文件写入，无文件落盘

#### Scenario: 首个 part 非 connId 被拒绝

- **WHEN** 请求体第一个 part 是文件而非 `connId` 文本字段
- **THEN** 返回 400 `invalid_input`，文件内容不落盘

#### Scenario: connId 非当前连接被拒绝

- **WHEN** 上传请求携带的 connId 不是该任务 TUI 终端当前连接 ID（如旧标签页已被替换）
- **THEN** 返回 403 `forbidden`，无文件落盘

#### Scenario: 大小边界与超限

- **WHEN** 上传文件实际字节数分别为 M-1、M、M+1（M 为配置上限）
- **THEN** M-1 与 M 被接受并原子落盘返回 uploadId；M+1 返回 413 `payload_too_large`，不留可投递文件

#### Scenario: 伪造或缺失 Content-Length

- **WHEN** 请求伪造 Content-Length 或缺失该头部
- **THEN** server 以实际读取字节数执行上限判定，不因 Content-Length 放行超限内容

#### Scenario: 额外文件 part 与尾部格式损坏

- **WHEN** 请求包含第二个文件 part，或 multipart 尾部格式损坏
- **THEN** 返回 400 `invalid_input`，已写入的临时文件被清理，不留可投递文件

#### Scenario: 成功上传

- **WHEN** 已认证用户向存在且活跃的任务、以当前连接 connId 上传大小合规的文件
- **THEN** 文件原子落盘，接口返回 201 与可用于投递的 uploadId

#### Scenario: 尾随垃圾数据计入总量且不阻断提交

- **WHEN** 请求在最终 boundary 之后携带尾随垃圾数据（epilogue），且文件 payload、累计非文件开销与请求总量均合规
- **THEN** 垃圾数据被有界消费至 EOF（计入请求总量兜底）后正常提交；epilogue 使累计非文件开销超 64KiB 时返回 400 `invalid_input` 且不留可投递文件；其余超限按唯一计量表的冲突规则（按协议解析顺序首次违规）返回对应错误

#### Scenario: 非 identity 编码的 file part 被拒绝

- **WHEN** file part 携带 quoted-printable 等非 identity 的 Content-Transfer-Encoding
- **THEN** 返回 400 `invalid_input`，按 raw part 语义处理、不隐式解码，无文件落盘

#### Scenario: 前置开销先超限返回 400

- **WHEN** 请求同时存在多个计量违规，且位于文件内容之前的前置开销（如超长的 part headers/connId 字段、preamble）先超 64KiB 上限（文件 payload 也超 M）
- **THEN** 解析到前置开销时返回 400 `invalid_input`（按协议解析顺序首次违规），底层 read-ahead 不改变结果

#### Scenario: 前置开销合规时 payload 先超限返回 413

- **WHEN** 请求同时存在多个计量违规，但前置开销合规、文件 payload 在流式读取中先超 M（尾部开销随后才超限）
- **THEN** 返回 413 `payload_too_large`（按协议解析顺序首次违规），底层 read-ahead 不改变结果

#### Scenario: 非文件开销 64KiB 边界

- **WHEN** 非文件开销（preamble/boundary 行/part headers/connId 字段值等）恰好等于 64KiB 或超出
- **THEN** 等于 64KiB 时开销不超限、可正常解析；超出返回 400 `invalid_input`

### Requirement: 受管存储、命名与路径安全

上传文件 SHALL 写入 server 受管目录 `<uploadsDir>/<taskID>/`，按任务隔离存放；目录权限 MUST 为 0700，文件权限 MUST 为 0600。落盘名 MUST 为 server 生成的 32 位 hex（crypto/rand 16 字节）加合法扩展名；扩展名提取自原始文件名并小写化，须匹配 `^\.[a-z0-9]{1,10}$`，不匹配或缺失则落盘文件无扩展名（仍可投递）。客户端文件名 MUST 仅取 basename 作展示元数据，MUST NOT 用于落盘路径。`uploadsDir` MUST 在配置加载时绝对化（同 DataDir 模式），允许空格与非 ASCII 字符，MUST 拒绝含反斜杠 `\` 或控制字符（判定标准固定：rune < 0x20 或 rune == 0x7F）的配置值；投递前 MUST 对最终绝对路径做反斜杠与控制字符复查（同一判定标准），不通过则拒绝投递。文件 MUST 先写 `<name>.partial`，完整写入并校验后原子 rename；文件与 sidecar 均成功后才加入可投递索引、此后才可被投递（提交顺序与发布点见「保留生命周期与回收」）；finalize（rename）前 MUST 在同一 per-task 协调锁内复查（连接替换后同样复查），顺序：任务存在（否则 404 `not_found`）→ active（否则 409 `invalid_state`）→ 当前 connId（否则 403 `forbidden`，任务状态准入表仅 `status == active` 通过，见「连接归属与投递准入」）；任一失败 MUST NOT rename、MUST NOT 发布 uploadId，且清理 `.partial`。任务已删除的在途上传 MUST NOT 发布有效 uploadId。投递（注入终端）成功后 MUST NOT 立即删除文件。

#### Scenario: 非法或缺失扩展名

- **WHEN** 上传文件的原始名无扩展名，或扩展名小写化后不匹配 `^\.[a-z0-9]{1,10}$`
- **THEN** 落盘名为 32 位 hex 且不带扩展名，文件仍可正常投递

#### Scenario: uploadsDir 含反斜杠或控制字符拒绝启动

- **WHEN** `OCDECK_UPLOAD_DIR` 配置值包含反斜杠，或包含 ESC/换行等控制字符（rune < 0x20 或 rune == 0x7F）
- **THEN** server 拒绝启动并明确报错

#### Scenario: 任务删除瞬间在途上传不发布

- **WHEN** 上传 finalize（rename）执行时该任务已被删除
- **THEN** 该上传不发布有效 uploadId，临时文件随任务目录回收

### Requirement: 保留生命周期与回收

保留期 TTL SHALL 经 `OCDECK_UPLOAD_RETENTION` 配置：缺省或 `0` 表示关闭；正 Go duration 表示启用；负值或非法值 MUST 使启动报错。TTL 启用时，过期基准 MUST 为上传完成时间，仅完整成功投递（deliver_result `ok:true`）刷新过期时间；`now >= expiresAt` 的文件 MUST 不可投递（按 `expired` 拒绝）。TTL 元数据唯一真值为磁盘 sidecar 文件 `<最终文件名>.meta.json`（与最终文件同目录）。固定 JSON schema（design 与本 spec 同文逐字）：`{"uploadId":"<32hex>","taskId":"<id>","connId":"<uuid>","filename":"<最终文件名>","uploadedAt":"<RFC3339Nano UTC>","lastDeliveredAt":"<RFC3339Nano UTC>|null"}`；序列化 MUST 始终输出全部键；`lastDeliveredAt` 为 `null` 表示从未成功投递；键缺失或类型/格式非法视为损坏 sidecar；不引入通用存储框架、不使用 SQLite 表。过期时间计算公式固定：`expiresAt = (lastDeliveredAt ?? uploadedAt) + TTL`（TTL 启用时）。提交顺序与发布点：先写文件 `.partial` → 原子 rename 为最终文件名；再写 sidecar `.tmp` → 原子 rename 为 `.meta.json`；两者均成功后该上传才加入可投递索引并返回 201。sidecar 提交失败 MUST 返回 500 `internal`，并尽力撤销已 rename 的文件（撤销失败记录日志，残留按孤儿规则处理），MUST NOT 返回 uploadId。成功 PTY 写入后的 TTL 刷新（更新 lastDeliveredAt）失败：MUST NOT 重写 PTY、MUST NOT 影响 `ok:true` 回执；MUST 保留磁盘上已持久化的 `lastDeliveredAt`，MUST NOT 覆盖或回退到 `uploadedAt`；记录日志、不重试（下次成功投递再刷新）。启动恢复：扫描 uploadsDir，先按有效配对不变量判定，再按下方启动恢复分类表处理（design 按本表引用）。有效配对不变量：`taskId` MUST 等于扫描目录名；`filename` MUST 等于该 sidecar 去掉 `.meta.json` 后的配对文件 basename，且符合「受管存储、命名与路径安全」的受管命名规则；`uploadId` MUST 等于该文件名的 32hex 前缀。任一不一致 MUST 归为损坏 sidecar（按孤儿规则处理），MUST NOT 进入投递索引。"对应文件"固定指扫描位置的同名配对文件，MUST NOT 根据损坏字段寻找或删除其他路径。读取失败与内容损坏 MUST 区分：sidecar 文件权限/I/O 读取失败 MUST NOT 归为孤儿、MUST NOT 触发删除，MUST 记录日志并在下一轮清理/启动扫描重试；仅内容损坏（键缺失或类型/格式非法）按损坏 sidecar 归孤儿：

| 启动扫描发现 | 分类 | 处理 |
|---|---|---|
| 文件与合法 sidecar 有效配对 | 有效上传 | 进入可投递索引 |
| 缺 sidecar 的文件 | 孤儿 | 不进入投递索引、不恢复为可投递上传，按孤儿统一规则处理 |
| 缺文件的单边 sidecar | 孤儿 | 同上 |
| 损坏 sidecar 及其对应文件 | 孤儿 | 同上 |
| 遗留 `.tmp` | 孤儿 | 同上 |

孤儿统一规则：modtime 超过 1 小时由周期清理删除，独立于 TTL 开关（TTL 关闭时同样删除）；文件 mtime 仅用于孤儿清理的时间计算，MUST NOT 用于重建可投递上传的过期基准；MUST NOT 把重启时刻当作上传完成时刻。TTL 配置变化时，清理计算以清理执行时的当前配置值为准。task 删除 MUST 触发该任务上传目录的失效与回收（接入既有 `task.deleted` 发布点）；任务挂起、WS 重连、删除 opencode 对话 session MUST NOT 触发回收。上传提交（rename 前）、投递、清理、task 删除对同一任务目录的操作 MUST 经 per-task 协调锁（或等效单线程协调）串行化；任务删除/挂起的**实际提交** MUST 参与该锁（使在途上传 finalize 复查失败、投递准入失败），`task.deleted` 事件仅用于触发回收、不承担串行化职责。server SHALL 在内存登记活跃上传（uploadId 预留 + task 绑定，结束/失败时注销）。活跃上传进度/超时判定按下方活跃上传进度/超时表执行（design 按本表引用）：

| 项 | 契约 |
|---|---|
| 登记起点 | handler 通过认证/Origin/任务准入后开始读取请求体时登记活跃上传 |
| 进度字段 | 内存 `lastProgressAt`（不落盘） |
| 刷新事件 | 读取到任何非零请求体字节即刷新 `lastProgressAt`（覆盖首 part header、connId、file payload、尾部 epilogue 至 EOF 全阶段） |
| 触发时机 | 独立 1 小时停滞计时器在阈值处直接触发现有取消收尾流程（不等待周期扫描轮次） |
| 磁盘 mtime 边界 | 磁盘 mtime 仅承担非活跃 `.partial`/孤儿清理，MUST NOT 参与活跃上传停滞判定 |
| 取消与 finalize 先后 | 取消标记与 finalize 的先后仍按 per-task 协调锁内顺序裁决（见取消收尾流程） |

上传停滞达 1 小时（由独立停滞计时器触发）的取消收尾流程固定：① 持 per-task 协调锁标记该上传不可提交并发出取消信号，随后**释放协调锁**；② 中断请求体读取、等待上传执行结束；③ 重新持锁确认身份及非活跃后删除文件（注销登记）。MUST NOT 持协调锁等待 handler 退出（防等待环：handler finalize/失败收尾也需同一锁）。取消标记后的 finalize MUST NOT 发布 uploadId、MUST NOT 返回 201；HTTP 尚可响应时固定返回 408（错误信封 code 用现有 `invalid_input`，message 用固定上传停滞提示文案——唯一文案见「文件上传接口与安全约束」错误表），客户端已断开则不保证响应。系统 SHALL 每小时执行周期清理并在启动时重扫：删除 `.partial` 前 MUST 先确认其不属于任何活跃上传，`.partial` 仅当 modtime 超过 1 小时（非活跃 `.partial`；磁盘 mtime 不参与活跃上传停滞判定）才删除；已完成文件按 expiresAt 删除（仅 TTL 启用时）；启动恢复分类表中的孤儿按孤儿统一规则删除（modtime 超 1 小时，独立于 TTL 开关）；TTL 关闭时仍 MUST 清理孤儿 `.partial` 与已删除任务的目录。启动时进程新建、无活跃上传，上次遗留 `.partial` 按孤儿规则清理。清理过程中的任务查询失败 MUST NOT 被当作"任务不存在"而误删；删除失败 MUST 记录日志并在下一轮重试。TTL 启用时的验收口径为"过期后的下一轮每小时扫描完成清理"，不要求恰好到期物理删除。

#### Scenario: TTL 默认关闭

- **WHEN** server 以默认配置（未设置 `OCDECK_UPLOAD_RETENTION`）运行
- **THEN** 上传文件不因时长过期被拒或被清理，仅受 task 删除回收与孤儿清理约束

#### Scenario: 非法保留期拒绝启动

- **WHEN** `OCDECK_UPLOAD_RETENTION` 设置为负值或不可解析值
- **THEN** server 拒绝启动并明确报错

#### Scenario: TTL 启用时投递刷新与过期拒绝

- **WHEN** TTL 启用，文件上传后被完整成功投递，过期时间到达后再次请求投递
- **THEN** 过期时间曾自该次成功投递刷新；再次到期后 deliver 按 `expired` 拒绝，下一轮每小时扫描将其清理

#### Scenario: sidecar 缺失或损坏按孤儿处理且不误判上传时间

- **WHEN** server 重启后扫描发现已完成文件缺少 sidecar，或 sidecar 损坏/文件单边存在
- **THEN** 该文件按启动恢复分类表归为孤儿（不进入投递索引、不恢复为可投递上传、不立即删除，modtime 超 1 小时由周期清理删除）；文件 mtime 仅用于孤儿清理的时间计算，MUST NOT 用于重建可投递上传的过期基准；上传完成时间不取重启时刻

#### Scenario: sidecar 字段与配对不一致按损坏处理

- **WHEN** 启动扫描发现 sidecar 存在，但其 `taskId` 不等于扫描目录名、`filename` 不等于去 `.meta.json` 后的配对文件 basename 或不符合受管命名规则、或 `uploadId` 不等于文件名的 32hex 前缀
- **THEN** 该 sidecar 归为损坏（孤儿规则）：扫描位置的同名配对文件不进入投递索引、不恢复为可投递上传；MUST NOT 根据损坏字段寻找或删除其他路径

#### Scenario: sidecar 读取失败不删除并重试

- **WHEN** 启动扫描或周期清理读取 sidecar 时发生权限或 I/O 错误
- **THEN** 该 sidecar 及其配对文件 MUST NOT 归为孤儿、MUST NOT 被删除；记录日志并在下一轮清理/启动扫描重试

#### Scenario: TTL 刷新失败不影响成功回执

- **WHEN** PTY 完整写入成功（`ok:true`）但 lastDeliveredAt 更新失败
- **THEN** 不重写 PTY、回执不受影响；记录日志且不重试；磁盘上已持久化的 `lastDeliveredAt` MUST 保留（MUST NOT 覆盖或回退到 `uploadedAt`），过期时间仍按 `expiresAt = (lastDeliveredAt ?? uploadedAt) + TTL` 计算

#### Scenario: task 删除触发回收

- **WHEN** 任务被删除
- **THEN** 该任务上传目录失效并被回收；任务挂起或 WS 重连不产生同等回收

#### Scenario: 清理不误删

- **WHEN** 周期清理中某任务查询失败，或某目录删除失败
- **THEN** 查询失败的任务目录不被删除；删除失败记录日志并在下一轮重试

#### Scenario: 启动清理孤儿

- **WHEN** server 启动时受管目录存在上次运行遗留的停滞 `.partial`（modtime 超 1 小时）、启动恢复分类表中的孤儿（modtime 超 1 小时）或已删除任务的目录
- **THEN** 启动清理将其删除；活跃上传（含 1 小时内的 `.partial`）不受影响

#### Scenario: 活跃上传不受清理误伤

- **WHEN** 周期清理遇到仍属于活跃上传的 `.partial`，或某 `.partial` 停滞达 1 小时
- **THEN** 前者不删除；后者为非活跃停滞 `.partial`，直接删除（磁盘 mtime 仅承担非活跃 `.partial` 清理；活跃上传停滞由独立 1 小时计时器经活跃上传进度/超时表触发取消收尾流程处理，见本 requirement 上文）

### Requirement: 连接归属与投递准入

上传与投递 SHALL 按 `(taskID, 终端类型=TUI, server 签发的连接 ID connId)` 绑定授权。server MUST 在 TUI 终端 WS 新连接注册时签发 connId（uuid）并经 auth_ok 帧下发（`{"type":"auth_ok","connId":"<uuid>"}`）；上传请求携带的 connId MUST 是该任务 TUI 终端的当前连接 ID，否则拒绝上传。deliver 帧 MUST 仅在它到达的那条 WS 连接的处理闭包中执行。投递校验 MUST 按唯一优先级执行，多项同时失败时返回首个失败：① 终端类型/连接当前性（非 TUI 或 connId 非当前 → `forbidden`）；② 任务活跃性（挂起/删除 → `task_inactive`）；③ uploadId 查找（不存在 → `not_found`）；④ task/connId 归属（不匹配 → `forbidden`）；⑤ TTL（过期 → `expired`）；⑥ 文件存在性与路径校验（物理文件缺失 → `not_found`；路径含反斜杠/控制字符 → `invalid_input`）；⑦ 写入（失败 → `write_failed`）。任务状态准入表（上传与投递共享）：仅 `status == active` 通过，其余全部状态拒绝；HTTP 侧任务不存在 → 404 `not_found`、非 active → 409 `invalid_state`，deliver 侧按上述优先级返回 `task_inactive`。投递准入检查（该 connId 仍为注册表当前值、任务活跃）与 PTY 写入 MUST 在同一 per-task 协调锁临界区内完成；连接替换提交、任务挂起/删除提交 MUST 进入同一把锁，线性化点为临界区内校验+写入完成的时刻；等待 4009 关闭握手时 MUST NOT 持有该锁。投递单次 PTY 写入使用 5 秒服务端写 deadline，连接取消时写入 MUST 终止；写入未在期限内完整成功统一将结果分类为 `write_failed`（结果未知语义），回执可达性与收尾按 bridge 收尾表执行。infrastructure 层（pty 包）MUST 提供真正可中断的单次写入：超时或取消后 MUST 确认底层写操作已结束，才释放 per-task 协调锁；MUST NOT 仅让调用方超时返回而留下后台写入继续写 PTY。后备实现：若目标平台无法对 PTY 可靠设置写 deadline，通过终止该 attach 客户端并关闭其 PTY 解除阻塞；任务本体 tmux 会话不随 attach 客户端终止。投递准入遇到非业务错误（任务查询失败、上传元数据读取失败、文件 stat 权限/I/O 错误等基础设施故障）且尚未写入 PTY 时，MUST 停止后续校验与 PTY 调用，记录内部错误日志，并以 WS close code 1011 结束连接；MUST NOT 伪造 `not_found`/`task_inactive`/`write_failed` 等业务码（前端按断线进入"结果未知"）；只有已确认不存在/非活跃才映射对应业务码。客户端 MUST 捕获上传时的 connId；WS 重连获得新 connId 后，旧上传回调 MUST NOT 转投新连接。投递 MUST 与普通终端输入共用串行写入路径。

bridge 收尾表（所有未完整成功的投递写入——超时、取消、立即错误、短写——的回执与连接收尾；design 按本表引用）：

| 收尾情形 | 契约 |
|---|---|
| 写期限边界 | 5 秒写期限验收只覆盖底层写退出与协调锁释放，回执传输不计入该期限 |
| 写超时且 WS 仍可用 | MUST 先生成并入队 `write_failed` 回执；PTY 后备终止产生的 EOF MUST NOT 抢先取消该回执；释放协调锁后执行独立、有界的 writer 收尾 |
| 非超时写错误/短写（立即错误如 EIO、无错误短写 `0 < n < len && err == nil`） | 底层写结束后 MUST 停止该连接继续处理输入；WS 可用且写队列有容量时 MUST 先生成并入队 `write_failed` 回执；释放协调锁后进入既有独立 1 秒 writer/连接收尾预算；关闭码按「故障与替换并发」行仲裁——故障先提交 1011、替换先提交 4009；不补写 |
| WS 已取消、写队列满或回帧失败 | 不保证回执到达，客户端按断线归"结果未知"；MUST NOT 补写 |
| 原生 PTY deadline 超时（无需后备终止） | 故障收尾统一使用 close code 1011；对端不可达时允许没有关闭帧（预算耗尽直接关底层连接） |
| 后备终止引起的故障关闭 | MUST 使用 close code 1011（避免 1000 导致前端停止自动重连；证据：session.ts:378 收到 1000 停止重连；ws_terminal.go:195-200 正常收尾可能发 1000、:178/:226-232 双向取消） |
| 故障与替换并发 | 关闭原因提交点冻结、唯一关闭所有者：终止原因 MUST 在同一协调边界（per-task 协调锁或等价的 per-conn 提交点）内提交，以先提交者为准。故障收尾先提交 1011 → 由 bridge 完成 1011 收尾，替换 handler MUST NOT 对该连接另发 4009；替换先提交 → 替换流程拥有关闭（4009），后续取消/超时产生的写失败 MUST NOT 覆盖该原因（该连接仍可能有回执入队尝试，但不改变关闭码）。依据：coder/websocket 仅第一次 Close 执行握手（close.go:99），第二次 Close 无法改写关闭码；前端对 4009 停止重连、对 1011 自动重连（session.ts:370/:378），不得交由 goroutine 调度随机决定 |

bridge 收尾预算冻结：底层写退出、后备终止及协调锁释放受 5 秒写期限约束；释放协调锁后启动独立的 **1 秒** writer/连接收尾预算，覆盖回执写出等待、关闭握手、必要的底层 transport 强制关闭及该阶段退出；两个阶段不得互相借用预算，MUST NOT 无界等待；1 秒预算耗尽后 MUST 取消 writer 并强制关闭连接，不再等待回执到达。**1 秒预算的实现边界**：现有 `coder/websocket`（v1.8.15）`Conn.Close` 写关闭帧最多等 5s、再等对端关闭帧 5s，且 `Close` 已开始后 `CloseNow` 走 waitGoroutines 上限 15s——「启动 Close、1s 后 CloseNow」无法兑现 1s 预算；现有 wsClose（ws.go:128）忽略 ctx 直接 `c.Close`，同样不满足。1 秒预算 MUST 通过**经验证、可独立于 `Conn.Close` 强制关闭底层 transport 的适配边界**兑现（例如 accept/握手前包装底层 net.Conn 并保留句柄，预算耗尽时直接关闭底层连接），或固定另一条已证明可中断握手的路径；MUST NOT 将 `CloseNow` 视为可抢占正在执行的 `Close`；取消空闲 writer 不保证中止进行中的关闭握手。「故障关闭码恒为 1011」仅当**故障收尾先提交关闭原因**时适用；替换先提交的连接关闭码为 4009（仲裁规则见 bridge 收尾表「故障与替换并发」行）。

#### Scenario: 正常投递图片

- **WHEN** 图片上传成功、任务终端连接有效且 connId 一致，用户触发投递
- **THEN** opencode TUI 中出现图片附件占位（如 `[Image 1]`），终端无路径文本残留、无自动提交

#### Scenario: 连接被替换后旧投递被拒绝

- **WHEN** 上传完成后该任务终端已被新连接替换（如另一标签页接管），旧 connId 的投递请求到达
- **THEN** 投递被拒绝，新连接的终端不收到任何注入内容

#### Scenario: 重连后旧上传回调不转投

- **WHEN** 上传响应返回前 WS 断开重连，客户端已持有新 connId
- **THEN** 旧 connId 的上传回调不转投到新连接，该项按规则分类（从未发送 deliver 为明确失败；已发送无确定回执为结果未知），不自动重发

#### Scenario: 物理文件缺失按 not_found 拒绝

- **WHEN** uploadId 查找成功但落盘物理文件已不存在，且前序校验均通过
- **THEN** 投递按优先级第 6 步拒绝，deliver_result 回 `not_found`，PTY 无写入

#### Scenario: 任务挂起后投递被拒绝

- **WHEN** 上传完成后任务被挂起，投递请求到达
- **THEN** 投递被拒绝（`task_inactive`）并向用户反馈失败

#### Scenario: PTY 不消费时写入有界退出

- **WHEN** 通过可控写入替身，或停止读取并预先耗尽 PTY 输入缓冲，确认本次投递写入无法完成，投递写入发生
- **THEN** 5 秒内底层写退出且锁释放，连接替换、任务挂起、任务删除均可正常继续；被终止的旧写操作不再向 PTY 注入任何后续内容；`write_failed` 的入队、写出尝试及失败关闭按 bridge 收尾表执行（回执可达性受「WS 已取消、写队列满或回帧失败」行约束）

#### Scenario: 基础设施故障以 1011 结束且下游零调用

- **WHEN** 投递准入中任务查询失败，或上传元数据读取/文件 stat 发生权限或 I/O 错误，且尚未写入 PTY
- **THEN** 停止后续校验与 PTY 调用（PTY 与存储下游零调用），记录内部错误日志，连接以 1011 关闭；不回伪造的 `not_found`/`task_inactive`/`write_failed`；前端按断线进入"结果未知"

#### Scenario: 回执已写出但对端不回关闭帧

- **WHEN** `write_failed` 回执已成功写入 WS，server 发起故障收尾但对端不回关闭帧
- **THEN** 实际连接关闭与收尾退出均在 1 秒收尾预算内完成（预算耗尽时经适配边界直接关闭底层连接），不依赖对端关闭握手完成

#### Scenario: 故障关闭码恒为 1011

- **WHEN** 投递链路发生故障收尾（含原生 PTY deadline 超时路径与后备终止路径），且故障收尾先提交关闭原因（未被替换先提交覆盖）
- **THEN** 对外关闭码恒为 1011；对端不可达时允许没有关闭帧（直接关底层连接），MUST NOT 使用 1000 等其他关闭码（替换先提交的连接关闭码为 4009，见 bridge 收尾表「故障与替换并发」行）

### Requirement: 投递状态机与结果反馈

每个文件项 SHALL 维护状态机：上传中 → 待投递 → 等待回执（deliver 已发）→ 已发送到终端 | 明确失败 | 结果未知。"已发送到终端" MUST 仅在当前请求与当前 connId 匹配的 deliver_result `ok:true` 时成立。单项失败分类——含 deliver 尚未发送的本地门禁拒绝（锁定态/未连接、不调用上传/发送接口）归"明确失败"——MUST 以「多文件完整性与投递节奏」的共享队列状态表为唯一来源，本文不重复枚举。普通断线 MUST NOT 把已成功项改为未知。结果未知 MUST NOT 自动补发；手动重试时 UI MUST 提示可能重复。`write_failed` MUST 触发共享队列暂停（队列级暂停规则唯一来源见「多文件完整性与投递节奏」的共享队列状态表）、不补写；其手动重试 UI MUST 提示用户先检查终端输入框、可能重复。手动重试 = 重新上传取得新 uploadId：旧尝试永久结束，旧 uploadId 的回执到达 MUST 只丢弃并记录日志；重新上传须满足当前 connId；原 File 对象已不可用时 MUST 提示用户重新选择文件——重选经**绑定该项重试意图的文件选择器**完成，选中即为该项创建新尝试（新 uploadId，进入暂停期间允许执行的重试调度，暂停语义见「多文件完整性与投递节奏」共享队列状态表），用户取消选择则该项保持原状态。同一 uploadId 同时 MUST 最多存在一个待确认投递；状态已迁移的迟到回执 MUST 被丢弃并记录日志。server 侧单次 PTY 写入 MUST 只有完整写入（`n == len && err == nil`）才回 `ok:true`；单次写入使用 5 秒服务端写 deadline（连接取消时写入终止），未在期限内完整成功（含部分写入、超时、取消、立即写错误、无错误短写）MUST 统一将结果分类为 `write_failed` 且不补写（回执可达性与连接收尾按「连接归属与投递准入」bridge 收尾表执行，非超时情形按其「非超时写错误/短写」行处理），沿用"结果未知"语义；可中断写入与锁释放契约见「连接归属与投递准入」。浏览器侧 10 秒等待回执时限保持不变，与服务端写 deadline 为独立的两层时限。项进入"已发送到终端"后 MUST 经约 1200ms 呈现层驻留供用户确认，随后从浮层隐去（仅呈现层：状态机不删除该项、不改变其状态）；浮层 MUST 仅在存在可见项或提示时呈现——仅剩已隐去的成功项时整个浮层消失，MUST NOT 持续遮挡终端输入区；失败与"结果未知"项 MUST 始终保留直至重试或状态解除。

#### Scenario: 投递成功反馈

- **WHEN** deliver_result 返回 `ok:true` 且与当前请求、当前 connId 匹配
- **THEN** 该项状态为"已发送到终端"

#### Scenario: 成功项驻留后从浮层隐去

- **WHEN** 项进入"已发送到终端"且约 1200ms 呈现层驻留期满
- **THEN** 该项从浮层隐去（状态机中该项状态不变）；若浮层内仅剩已隐去的成功项且无提示，整个浮层消失；失败与"结果未知"项始终保留

#### Scenario: 回执超时进入结果未知

- **WHEN** deliver 帧发出后 10 秒内未收到 deliver_result，或期间 WS 断线
- **THEN** 该项进入"结果未知"，不自动补发；用户手动重试时提示可能重复

#### Scenario: write_failed 归类结果未知并暂停队列

- **WHEN** deliver_result 返回 `write_failed`（含部分写入）
- **THEN** 该项进入"结果未知（可能部分写入）"，触发共享队列暂停（队列级，见「多文件完整性与投递节奏」共享队列状态表）、不补写；手动重试时提示先检查终端输入框、可能重复

#### Scenario: 重试即重新上传

- **WHEN** 用户对"明确失败"或"结果未知"项手动重试
- **THEN** 重新上传取得新 uploadId（满足当前 connId），旧 uploadId 的迟到回执只丢弃并记录日志；原 File 对象不可用时经绑定该项重试意图的文件选择器重选（选中即创建新尝试，取消选择则该项保持原状态）

#### Scenario: 断线不改写已成功项

- **WHEN** 某项已因 deliver_result `ok:true` 进入"已发送到终端"，随后 WS 断线
- **THEN** 该项保持"已发送到终端"，不因断线改为"结果未知"

#### Scenario: 迟到回执被丢弃

- **WHEN** 该项状态已迁移（如已被标记失败并重试）后原 deliver_result 才到达
- **THEN** 迟到回执被丢弃并记录日志，不改变该项状态与队列状态（包括队列已恢复的情况）

#### Scenario: 部分 PTY 写入不冒充成功

- **WHEN** 注入时 PTY 写入仅完成部分字节或返回错误
- **THEN** deliver_result 回 `write_failed`，不补写、不回 `ok:true`；回执与连接收尾按「连接归属与投递准入」bridge 收尾表「非超时写错误/短写」行执行

### Requirement: 投递输入门禁

文件上传与投递 MUST 遵循与终端输入一致的门禁判定（已认证、连接可写、终端未锁定）：上传前 MUST 执行 `shouldSendInput` 同款判定，发送 deliver 前 MUST 复查。文件投递是非合成输入，门禁调用的 `syntheticInFlight` MUST 固定为 `false`（合成输入的锁定例外 MUST NOT 被无差别复用）。门禁不通过时上传/投递 MUST 失败并向用户明确反馈，MUST NOT 静默丢弃，MUST NOT 在终端锁定状态下向终端写入任何内容。

#### Scenario: 锁定状态下上传与投递被拦截

- **WHEN** 终端处于锁定状态（触屏设备终端输入锁定生效中），用户粘贴/拖入文件或触发已上传文件的投递
- **THEN** 上传与 deliver 均被拦截并提示先解锁终端，终端无任何输入产生

### Requirement: 附件能力表（CAPABILITY-TABLE）

投递后文件的处理 MUST 遵循 opencode TUI 原生行为，按扩展名分类（取自落盘扩展名，大小写不敏感）。本 requirement 为附件能力的唯一来源（CAPABILITY-TABLE，依据 opencode v1.18.30 `local-attachment.ts` extname 小写匹配），其他文档引用本表而不得另行列举：

| 扩展名 | 行为 |
|---|---|
| `.png` `.jpg` `.jpeg` `.gif` `.webp` `.avif` `.pdf` | 真附件：TUI 出现 `[Image N]` / `[PDF N]` 占位符，提交后作为附件进入 AI 消息 |
| `.svg` | 文本内容内联 |
| 其余（含无扩展名） | 作为路径文本消费：路径文本进入输入框，用户可正常编辑或删除；可按 opencode 原生 paste-summary 配置显示为摘要占位（如 `[Pasted ~N lines]`），底层文本仍为路径（证据：opencode `packages/tui/src/component/prompt/index.tsx:1206`，文本长度超 150 且 paste summary 启用时走摘要） |

产品文案与文档 MUST NOT 宣称任意文件都会成为附件。验收前提：文件有效、可读、且被目标 opencode 版本接受；server 读盘/解码失败 MUST NOT 由投递回执冒充为附件成功。

#### Scenario: 图片与 PDF 成为附件

- **WHEN** 能力表中附件类扩展名（如 png、pdf）的文件被投递到终端
- **THEN** TUI 出现对应附件占位符，提交后作为附件进入 AI 消息

#### Scenario: SVG 内联与其它类型仅路径

- **WHEN** `.svg` 文件与 zip、无扩展名文件分别被投递到终端
- **THEN** svg 以文本内容内联；其余仅在输入框出现路径文本（长路径在 paste-summary 启用时按原生摘要占位显示，底层仍为路径），用户可正常编辑或删除

#### Scenario: 长路径未知类型显示原生摘要不为投递失败

- **WHEN** 未知类型（或无扩展名）文件被投递，其最终绝对路径超 150 字符且 opencode paste summary 启用
- **THEN** 输入框显示原生摘要占位（如 `[Pasted ~N lines]`），底层文本仍为完整路径；该原生摘要 MUST NOT 被误判为附件成立，也 MUST NOT 被误判为投递失败

### Requirement: 多文件完整性与投递节奏

一次粘贴、拖入或选择多个文件时，系统 SHALL 逐个上传并按串行节奏投递：MUST 等待上一项 deliver_result（或其 10 秒超时进入结果未知）后再发送下一项 deliver。共享队列状态表（队列级暂停；本表为队列暂停/恢复/重试规则的唯一来源，其他文档按表名引用）：

| 当前队列状态 | 事件 | 项状态变化 | 允许的上传与发送 | 下一队列状态 |
|---|---|---|---|---|
| 自动推进 | 任一文件项进入"结果未知"（回执超时、等待回执期间断线或 `write_failed`） | 该项归"结果未知" | 无新 deliver 发送 | **暂停**（非批次级） |
| 暂停 | 常态入口新增项入队 | 新项归「排队中（待上传）」（尚未取得 uploadId，与已取得 uploadId 的"待投递"区分） | 仅入队、不自动发送；MUST NOT 隐式替代任何"结果未知"项 | 暂停 |
| 暂停 | 显式手动重试任一失败项（"明确失败"或"结果未知"），原文件仍可用 | 该项创建新尝试（新 uploadId） | 进入同一串行重试调度（仍最多一个待确认请求）；重复点击同一项 MUST NOT 创建并发尝试，其他重试请求串行排队 | 暂停 |
| 暂停 | 显式手动重试任一失败项且原文件（File 对象）已不可用 | 打开**绑定该项重试意图的文件选择器**，选定后为该项创建新尝试（新 uploadId），进入暂停期间允许执行的重试调度 | 同上（串行重试调度、同项不并发） | 暂停（用户取消选择 → 该项保持原状态、队列保持暂停） |
| 暂停 | 针对"结果未知"项发起的恢复尝试取得确定结果 | 该项以新尝试结果为准 | 恢复自动推进 | **自动推进**；该次尝试再次进入"结果未知"（如再次 `write_failed`）→ 继续暂停 |
| 暂停 | 重试"明确失败"项取得确定结果 | 该项以新尝试结果为准 | — | 暂停（MUST NOT 解除既有暂停：只有针对"结果未知"项发起的恢复尝试取得确定结果才能解除暂停） |
| 自动推进（恢复后） | 队列恢复时仍存在其他"结果未知"项 | 原未知项保持 | 未经其各自单独手动重试，MUST NOT 被补发 | 按各自后续重试操作处理 |

恢复自动推进后，未上传项 MUST 先完成上传、取得 uploadId 后才能 deliver（「排队中（待上传）」→ 上传 →「待投递」→ deliver）。迟到回执始终只丢弃并记录日志，不改变项状态或队列状态，包括队列已恢复的情况。单项**明确失败**（注入前准入拒绝、上传失败，或 deliver 尚未发送的本地门禁拒绝——锁定态/未连接，此时不调用上传/发送接口）MUST 继续后续项，后续项按各自门禁继续判定。多文件**完整性**为强制验收：正常链路、文件有效、目标 TUI 可接收时，每个文件 MUST 在 TUI 实际出现对应结果（附件占位 / 内联文本 / 路径文本；两个图片 = 两个占位符）——此为文件消费完整性验收；Web 浮层的"明确失败/结果未知"仅为分项反馈状态，MUST NOT 作为文件已完整进入 TUI 的替代验收。最终附件**排列顺序**不承诺。各项状态 MUST 分项展示，失败/结果未知项 MUST 可单独重试且不影响已成功项；投递成功文案 MUST 表述为"已发送到终端"，附件是否成立以 TUI 占位符为准。

#### Scenario: 多文件分别投递

- **WHEN** 用户一次粘贴两个图片文件
- **THEN** 两个文件串行上传投递，TUI 中分别出现附件占位符，每个文件都有对应结果

#### Scenario: 单项失败继续后续项

- **WHEN** 多文件处理中某一文件上传失败、收到注入前准入拒绝（如 `forbidden`/`task_inactive`），或被本地门禁拒绝（锁定态/未连接，deliver 尚未发送）
- **THEN** 该项归类"明确失败"并显示重试入口（门禁拒绝项不调用上传/发送接口），后续文件继续处理、按各自门禁判定，已成功项不受影响

#### Scenario: 任一未知项暂停整个共享队列

- **WHEN** 某一文件项进入"结果未知"（回执超时或 `write_failed`）
- **THEN** 整个共享队列暂停（非批次级），暂停期间新入队项归「排队中（待上传）」不自动发送；用户手动重试该项取新 uploadId 进入同一串行调度，得到确定结果后队列恢复自动推进，再次未知则继续暂停

#### Scenario: 队列恢复不补发原未知项

- **WHEN** 队列因用户手动重试某一未知项并得到确定结果而恢复
- **THEN** 其他仍处"结果未知"的项未经其各自单独手动重试，MUST NOT 被补发，保持等待用户操作

#### Scenario: 未知项重选文件成功后队列恢复

- **WHEN** 队列暂停期间用户手动重试某"结果未知"项且原 File 已不可用，用户从绑定该项重试意图的文件选择器中选定新文件
- **THEN** 该项以新 uploadId 创建新尝试并进入暂停期间允许执行的重试调度；该次尝试得到确定成功结果后队列恢复自动推进，原"结果未知"状态被新尝试结果取代

#### Scenario: 未知项取消选择保持暂停

- **WHEN** 队列暂停期间用户手动重试某"结果未知"项且原 File 已不可用，但用户取消了文件选择
- **THEN** 该项保持"结果未知"不变，队列保持暂停；暂停期间新增入队项仍只入队不发送、MUST NOT 隐式替代该未知项

#### Scenario: 暂停期间重试明确失败项不解除暂停

- **WHEN** 项 A 上传失败归"明确失败"，随后项 B 回执超时进入"结果未知"并触发共享队列暂停；暂停期间用户显式重试 A，A 进入同一串行重试调度并取得确定结果，之后用户重试 B 并取得确定结果
- **THEN** A 的重试可进入同一串行重试调度，但其确定结果 MUST NOT 解除既有暂停；B 的恢复尝试取得确定结果后队列才恢复自动推进；同一项重复点击 MUST NOT 创建并发尝试，其他重试请求串行排队
