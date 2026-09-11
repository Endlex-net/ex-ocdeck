# Design: terminal-file-paste-drop

## Context

动机与范围见 proposal.md。本设计只记录实现所需的现状与约束。

**终端链路现状（已核实）**：

```
浏览器 xterm.js ──WS──> Go server ──PTY──> tmux -L ocdeck attach ──> tmux 会话 ──> opencode TUI
             <──WS──            <──PTY──                  <──
```

- 输入路径：xterm `onData`/`onBinary` → `session.ts sendInput`（统一输入门禁 input-gate：authed/wsOpen/locked/syntheticInFlight，session.ts:613-629）→ **始终以二进制帧发送**（`encoder.encode`，文本帧仅用于 auth/resize JSON，session.ts:303-310、582）→ server `pumpWSToPTY`（ws_terminal.go:252-282）→ `p.Write`。
- server 现行为：文本帧解析为 `{"type":"resize"}` 则调 `p.Resize`，**其余文本帧原样写入 PTY**（ws_terminal.go:266-279）——本变更将该兜底改为显式类型分发（D2，唯一行为变更点）。
- WS 帧读上限 1MiB（ws.go:26 `wsMaxFrame`，`acceptWS` SetReadLimit）；WS Origin 白名单校验（ws.go:76-103）；`/api/v1/*` 走 api 子 mux + Bearer 认证中间件（server.go:186，middleware.go:46 `AuthMiddleware`，401 返回统一错误信封并带 `WWW-Authenticate: Bearer`）；单交互客户端注册表 `wsClients.register`（internal/api/ws.go:172-191，仅替换注册表并返回旧连接与旧 bridge cancel 函数，不自行发送 4009、不执行 cancel——4009 关闭握手与 cancel 由 handler 层执行，ws_terminal.go:74-78）。
- auth_ok 现为 `{"type":"auth_ok"}`（ws_terminal.go:81，`wsAuthResp` 定义于 ws.go:50-51）——本变更新增 `connId` 字段。
- PTY 写入口现状：`pty.Pty.Write`（internal/infrastructure/pty/pty.go:256）直接 `p.ptmx.Write(b)`，无 context/deadline——投递写入的可中断退出为新增能力（D3）。
- 错误信封与错误码（internal/api/errors.go）：`ErrorCode` 为 snake_case 字符串，统一响应体 `{"error":{"code","message"}}`；`writeError` 按 code 映射默认 HTTP 状态，`writeJSONError` 支持显式状态（注释即说明用于保留非默认状态码）。既有枚举含 `unauthorized`(401)/`not_found`(404)/`conflict`(409)/`invalid_state`(默认422)/`invalid_input`(默认422)/`internal`(500) 等；**无** `forbidden`、`payload_too_large`（本变更新增，命名沿用同风格）。
- 配置经环境变量加载（config.go `Load`，`OCDECK_*`），`DataDir` 启动期绝对化（config.go:135-149）。
- 任务删除事件发布点：`LifecycleService.DeleteTask` 在级联 `session.deleted` 之后发布 `task.deleted`（internal/application/task/delete_reconcile.go:44）——上传目录回收接入该发布点（D4）。
- 输入门禁：`shouldSendInput`（web/src/terminal/input-gate.ts:16-23）按 authed/wsOpen/locked/syntheticInFlight 判定，合成输入有锁定例外（`locked && !syntheticInFlight` 才拦截）；文件投递是非合成输入，不享受该例外（D6）。
- web 端未发现浏览器文件输入捕获、上传与投递链路（已 rg 核实；`clipboard.ts` 中的 drop 字样为剪贴板输出策略语义，与此无关）。

**opencode TUI 行为（v1.18.30 TS TUI 源码核实；仓库 `sst/opencode`，commit `859106eb17d5b840475f5e4b78e64c9622f8750e`）**：bracketed paste 进来的文本经 `pastedFilepath`（`packages/tui/src/component/prompt/index.tsx:1183` `pasteInputText`；去引号/file:// 还原，不剥 `@`）→ `readLocalAttachment`：按扩展名（`packages/tui/src/component/prompt/local-attachment.ts:25` 附件类型表，extname 小写匹配，类型清单见附件能力表 CAPABILITY-TABLE）→ 读盘→base64→`[Image N]`/`[PDF N]` 附件；svg → 文本内联；其它扩展名或无扩展名 → 纯路径文本；opencode 默认启用图片缩放，默认尺寸阈值宽/高各 2000、大小阈值 base64 5MiB（`packages/opencode/src/image/image.ts:10` MAX_BASE64_BYTES；:76-87 读取可配置阈值并按 base64 字节计量；:101-109 自动缩放关闭时返回 SizeError——配置可覆盖，解码或缩放可能失败）。读盘发生在异步处理阶段，**注入完成后文件不能立即删除**。

**opencode 兼容区间**：项目既有已验证区间为 [1.18.14, 1.18.26]（internal/infrastructure/opencode/CONTRACT.md:3-14）。本变更附件链路针对 v1.18.30 验证并单独记录（D8）；扩展项目兼容区间遵循 CONTRACT.md 既有 SOP（锚点 diff + live probe）。

## Goals / Non-Goals

**Goals:**

- 粘贴、拖拽常态入口（文件选择器仅用于失败/未知项重试重选）共用的"浏览器文件 → server 受管存储 → 终端注入"链路，仅覆盖 opencode TUI 终端。
- 复用现有鉴权、输入门禁、串行写路径；WS 协议向后兼容（新旧 client/server 任意组合无异常行为）。
- 上传按 task 归属管理：task 删除触发该任务上传目录的失效与回收；单文件大小上限强制；保留 TTL 可配置且**默认关闭**。

**Non-Goals:**

- 拖入目录遍历、断点续传、上传进度百分比、TUI 层接收确认（回执只到"已写入终端"）。
- 图片压缩（无损/有损）：opencode 侧图片缩放默认启用（默认阈值与限制见 Context「opencode TUI 行为」节），存储已由大小上限 + 回收机制约束，首版不引入图片管线。
- 通用上传框架与 paste-buffer 策略系统（仅预留扩展位，见"职责分层与实施阶段"）。
- 适配 Go 时代 opencode TUI 的行为差异。
- 全局磁盘占用配额（首版仅单文件上限 + task 删除回收 + 孤儿清理；TTL 为可选启用项）。

## 职责分层与实施阶段

**分层（洋葱边界，外层依赖内层，不反向）：**

| 层 | 职责 |
|---|---|
| `internal/api` | HTTP/WS 协议解析、认证、错误映射（HTTP 状态/错误码、WS 帧编解码）；handler **仅解析 multipart 与映射错误**，不做落盘/准入/TTL 决策 |
| `internal/application` | 准入（任务状态/connId/校验优先级）、TTL 规则、投递状态机决策、任务生命周期协调（`task.deleted` 订阅仅触发回收）；持有窄存储端口（接口），不接触 multipart/WS 细节 |
| `internal/infrastructure/uploads` | 执行层：文件落盘、原子 rename、sidecar 读写、扫描清理——**流式落盘与 rename 唯一归属本层**，API 不重复分配 |
| 组合根（`cmd/ocdeck-server/main.go`，无 Fx 显式构造注入，go.mod 无 fx 依赖——已核实） | 构造与注入、启动扫描、定时清理器启停、优雅关闭 |

**测试要求（窄端口可测性）**：时钟注入（TTL/超时判定）、存储错误注入（清理失败语义）、可中断 PTY writer fake（写退出契约）、并发屏障（per-task 协调锁线性化测试）、启动恢复扫描用例（sidecar 字段不一致 → 按损坏 sidecar 归孤儿；sidecar 读取失败 → 不删、记录日志、下一轮重试）、bridge 收尾并发用例（「PTY EOF 先到」「WS 先取消」「writer 队列满」，及关闭原因仲裁屏障用例「故障先提交」「替换先提交」）、非超时写失败用例（「立即写错误」「无错误短写」「非超时失败与连接替换竞争」——验证关闭码、回执尝试、后续零 PTY 写入）、上传停滞超时用例（「取消与 finalize 竞争」「读取阻塞期间停滞取消」「首 part 未读完停滞」「file header 阻塞停滞」「尾部等待 EOF 停滞」）、前端迟到回执/连接换代测试、组合根启停验证。

**阶段表：**

| 阶段 | 内容 | 并行性 |
|---|---|---|
| ① 契约冻结 + spike | 本文档契约冻结；D8 端到端 spike | 契约冻结后前后端可分 lane |
| ② 后端存储/投递闭环 + WS 协议重构 | uploads 存储、上传 handler、deliver/deliver_result、auth_ok connId、清理器、per-task 协调锁、PTY 可中断写入能力 | `ws_terminal.go` 与 bridge 由**单一 lane** 负责 |
| ③ 前端入口/状态机 | paste/drop 捕获（picker 仅重试重选）、上传、投递状态 UI | 依赖 ① 的冻结契约 |
| ④ Linux 端到端验收 | 目标环境全链路验收（含多文件完整性） | — |

**步骤图（主路径 + 局部分支）：**

```
浏览器捕获 (paste / drop)
  → 门禁 (shouldSendInput 同款判定; syntheticInFlight 固定 false)
  → HTTP 上传 (multipart 分阶段解析: 先 connId part 后 file part; 三段计量, 见 D4)
      ├─ 上传失败 → 明确失败(手动重试 = 重新上传取得新 uploadId)
  → deliver → 投递准入 (per-task 协调锁临界区内, 按 D5 优先级: 连接当前性 → 任务活跃
      → uploadId → 归属 → TTL → 文件/路径 → 写入)
      ├─ 注入前准入拒绝 (forbidden/task_inactive/not_found/expired/invalid_input,
      │   确定零 PTY 写入) → 明确失败, 继续后续项
  → 注入 (bracketed paste, 共享串行写路径, 完整写入才算成功)
  → 回执 (deliver_result, 经有界写队列发出)
      ├─ write_failed(含部分写入) → 结果未知/可能部分写入, 触发共享队列暂停
      │   (队列级暂停, 唯一规则见 delivery spec「多文件完整性与投递节奏」共享队列状态表);
      │   手动重试提示先检查终端输入框、可能重复
      ├─ 10s 超时 / 断线 → 结果未知, 同样触发共享队列暂停; 手动重试提示可能重复
重连/替换: auth_ok 下发新 connId; 连接替换/挂起/删除提交与投递准入经同一把
per-task 协调锁线性化(D5)
```

**扩展位（首版不实现，仅保留边界）：**

- 配置化大小上限与 TTL（配置项已含，规则集中一处）。
- 附件能力表以数据表表达（CAPABILITY-TABLE），类型扩展不改流程代码。
- 注入函数边界：bracketed paste 注入收敛为单一函数入口，未来 `paste-buffer` 后备方案只替换该边界、不动协议层。
- 首版不实现通用上传框架与 paste-buffer 策略系统。

## Decisions

### D1: 文件传输走 HTTP 上传（契约表 UPLOAD）

新增 `POST /api/v1/tasks/{taskID}/attachments`（multipart/form-data）。浏览器侧 `File` 直接放入 `FormData`，不做 FileReader 预读/base64；server 流式读盘。

- **为什么**：WS 读上限 1MiB（ws.go:26），传文件需应用层分片/重组/背压/取消协议，且与终端交互共用一条连接会互相影响；HTTP 上传天然支持流式、限大小、错误语义清晰。`/api/v1` 挂载自动获得 Bearer 中间件（server.go:186）。
- **契约**（唯一定义在 delivery spec「文件上传接口与安全约束」requirement，此处为实现视角摘要）：
  - 请求字段：**第一个 part MUST 为 `connId` 文本字段**，随后恰好一个文件 part（字段名 `file`）；首 part 非 `connId`、出现其它字段或多文件 part → 400 invalid_input（首 part 非法时文件内容 MUST NOT 落盘）。
  - 成功：201 `{"uploadId":"<32 hex>"}`；原始文件名仅作展示元数据（取自 part header，不参与落盘路径）。
  - 错误统一现有信封 `{"error":{"code","message"}}`：

| HTTP | code | 触发 |
|---|---|---|
| 401 | `unauthorized` | 中间件：无/错 token（middleware.go:46 现有行为） |
| 403 | `forbidden`（新增） | Origin 不在白名单，或 connId 不是该任务 TUI 终端当前连接 ID |
| 404 | `not_found` | 任务不存在 |
| 409 | `invalid_state` | 任务非活跃 |
| 400 | `invalid_input` | multipart 畸形 / 无 `file` part / 多文件 part / 零字节以外的格式问题 |
| 413 | `payload_too_large`（新增） | 文件 payload 超上限 |
| 408 | `invalid_input`（沿用） | 上传停滞达 1 小时被取消收尾；message 固定 `"upload stalled for 1 hour"`（完整错误表与该文案唯一来源见 delivery spec「文件上传接口与安全约束」） |
| 500 | `internal` | 磁盘写失败（沿用既有 `CodeInternal`；现有错误码体系无 507 风格，不引入） |

  - 实现注记：`forbidden`、`payload_too_large` 为新增 ErrorCode，命名沿用 errors.go 的 snake_case 风格。本表状态值与 `httpStatusFor` 的默认映射不同（`invalid_state`/`invalid_input` 现映射 422），handler MUST 使用 `writeJSONError` 显式状态写入——该 helper 即为此场景存在。
  - Origin 策略：上传接口**复用既有 Origin 白名单判定**（与 `checkWSOrigin` 同一白名单语义），非法来源返回 403 `forbidden` 且 MUST 先于任何文件写入；WS 侧 Origin 规则保持现状不变。校验顺序为 Bearer → Origin → 任务/connId 准入。
  - 校验与解析固定分阶段：① 认证 → Origin → 任务存在/活跃（均先于请求体解析）；② 有界解析请求体，第一个 part MUST 为 `connId` 文本字段（否则 400 invalid_input，文件内容不落盘）；③ connId 归属校验（非该任务 TUI 当前连接 → 403 forbidden）；④ 解析唯一 `file` part 流式写 `.partial`（三段计量，见 D4）；⑤ 校验 multipart 完整结束 boundary、拒绝额外 part/字段（400 invalid_input，清理 `.partial`）；⑥ 原子 rename 提交 → 201 uploadId。任一步失败不留可投递文件。前端 FormData 构造顺序固定：先 `append("connId", ...)` 再 `append("file", ...)`。
- **备选**：base64 分片走 WS（+33% 体积，协议复杂，否决）；二进制分片走 WS（可行但需完整上传协议，仅在未来部署确实无法提供 HTTP 时考虑）；server 写 OS 剪贴板再注入 ctrl+v（无桌面环境不可靠、跨任务共享竞争，否决）。

### D2: 投递协议 = WS 新控制帧 + 显式类型分发 + 有界写协调

- 帧契约（与 terminal-streaming delta MODIFIED requirement 逐字一致）：
  - client→server：`{"type":"deliver","uploadId":"<32 hex>"}`（文本帧）。
  - server→client：`{"type":"deliver_result","uploadId":"<32 hex>","ok":true}`；失败 `{"type":"deliver_result","uploadId":"<32 hex>","ok":false,"error":"<code>"}`，error 枚举：`not_found`（uploadId 不存在或物理文件缺失）|`expired`（已过保留期）|`forbidden`（task/connId/终端类型不匹配，含 shell 连接）|`task_inactive`（任务挂起/删除）|`write_failed`（PTY 写失败，含部分写入）|`invalid_input`（合法 deliver 请求的投递内容校验失败）。
  - auth_ok 帧新增 `connId` 字段：`{"type":"auth_ok","connId":"<uuid>"}`（注册时签发，见 D5）。
- **server 侧行为变更**：`pumpWSToPTY` 文本帧改为显式按类型分发，**不再回落写 PTY**。控制帧分发分支规则：① 非法 JSON 或未知 type：丢弃 + 日志，不回帧、不调用 onInput；② `type=resize`：沿用既有尺寸处理，不校验 uploadId；③ 仅当 `type=deliver` 时才要求 uploadId 为合法 32 hex，不合法则丢弃（+日志、无回帧、无 onInput），合法才进入投递业务校验及活动上报。deliver 帧在当前 WS 连接的 pump 读循环闭包内处理（该连接 connId 已知）；投递业务校验的唯一优先级见 D5。
- **有界写协调**：`bridgeTerminal` 持有一个共享有界写队列（现 `pumpPTYToWS` 内部队上提为 bridge 级），`pumpPTYToWS` 的 PTY 输出与 `deliver_result` 控制帧都经它入队，由既有独立写 goroutine 串行写出（terminal-streaming 主 spec「输出削峰与背压」要求独立写 goroutine + 有界队列）。deliver 处理路径 **MUST NOT 直接阻塞写 WS**；队列满（慢客户端）沿用既有断开语义。
- **onInput 活动统计**：合法 deliver 帧视为用户活动，在 PTY 写入前调用 onInput（沿用既有语义：活动代表"收到用户输入"，不以写成功为前提）；未识别/畸形帧不调用；二进制帧行为不变。
- **重构不变量（WS 协议重构 MUST 保持的既有契约）**：

| 不变量 | 现状 |
|---|---|
| 二进制输入路径 | 非空 binary 帧 → onInput → `p.Write`（ws_terminal.go:259-265），不改 |
| resize 控制帧 | `{"type":"resize","cols","rows"}` → `p.Resize`，不改 |
| 首帧认证 | 5s 超时、认证成功前不订阅 PTY、失败 4001，不改 |
| 关闭码 | 4001/4009/4010/1011（及 shell 4004、recovering 1013）语义不变 |
| 单交互客户端替换 | 替换先提交关闭原因时，由替换流程先发送 4009，再 cancel 旧 bridge；故障先提交时，由 bridge 完成故障收尾，替换 handler 不另发 4009。唯一关闭所有者与并发规则见 delivery spec bridge 收尾表「故障与替换并发」行。等待关闭握手不持 per-task 协调锁（见 D5），不变 |
| PTY→WS 输出 | 有界队列 + 慢客户端断开（B10），仅队列所有权上提到 bridge 级 |
| 非法 JSON / 未知文本类型 | 旧行为回落写 PTY（ws_terminal.go:271-278）→ 改为丢弃+日志、不回落写 PTY（本变更唯一行为变更点） |

- **兼容性证据**：现客户端文本帧只有 auth（连接建立期）与 resize（session.ts:303-310、582）；用户输入全部走二进制帧（sendInput 两个分支都 `ws.send(Uint8Array)`，session.ts:613-629）。因此"未识别文本帧不写 PTY"对现有客户端零影响。
- **能力协商**：能力判定以 auth_ok 的 connId 为第一道（缺失/非法 → 文件功能整体不可用，见 D7「connId 能力降级」）；上传接口可用即表明 server 支持 deliver 协议，前端只在上传成功后发送 deliver 帧，MUST NOT 向未确认能力的 server 发送（connId 有效但旧 server 上传返回 404 为第二道兜底，终止当前项，不发送任何 deliver 帧）。
- **备选**：浏览器直接拼绝对路径文本注入（无法校验归属/过期/存在性，否决）；复用 resize 帧语义扩展（类型混淆，否决）。

### D3: 注入 = bracketed paste 序列，共用串行写路径

deliver 校验通过后向 PTY 写入 `ESC[200~` + 文件绝对路径 + `ESC[201~`，单个文件一次性完整写入，不带 `@`、不附加回车。server 侧单次 PTY 写入只有**完整写入**（`n == len(data) && err == nil`）才视为成功（deliver_result `ok:true`）；部分写入、立即写错误（如 EIO）与无错误短写（`0 < n < len && err == nil`）均回 `write_failed`、不补写（部分粘贴序列补写会产生终止符错乱的输入流），其回执与连接收尾按 delivery spec「连接归属与投递准入」bridge 收尾表「非超时写错误/短写」行执行。

**写入退出契约（唯一契约，delivery spec「连接归属与投递准入」同文）**：投递单次 PTY 写入使用 **5 秒服务端写 deadline**，连接取消时写入 MUST 终止；写入未在期限内完整成功（超时、取消、立即错误、短写）统一将结果分类为 `write_failed`（结果未知语义）；回执可达性与连接收尾按 delivery spec「连接归属与投递准入」的 bridge 收尾表执行（该表覆盖所有未完整成功的投递写入，非超时情形见「非超时写错误/短写」行）。infrastructure 层（pty 包）MUST 提供真正可中断的单次写入：超时或取消后 MUST 确认底层写操作已结束，才释放 per-task 协调锁；**MUST NOT 仅让调用方超时返回而留下后台写入继续写 PTY**（破坏临界区线性化）。后备实现：若目标平台无法对 PTY 可靠设置写 deadline，通过终止该 attach 客户端并关闭其 PTY 解除阻塞；任务本体 tmux 会话不随 attach 客户端终止。事实约束：现有 `Pty.Close()`（pty.go:281）在等待 readerDone 后还可能等待子进程最多 5 秒——后备路径的 5 秒验收针对**写操作退出与锁释放**，MUST 区分三个阶段：底层写终止、锁释放、进程回收；MUST NOT 把"5s 到期后直接调用现有 Close"当作满足退出验收的实现（Close 的进程回收等待不得阻塞锁释放验收）。现状：`pty.Pty.Write`（internal/infrastructure/pty/pty.go:256）直接 `p.ptmx.Write(b)`、无 context/deadline，本契约为其新增能力。浏览器侧 10 秒等待回执时限保持不变，与服务端写 deadline 为独立的两层时限。

- **bridge 收尾**：写超时/后备终止后的回执与连接收尾按 delivery spec「连接归属与投递准入」的 bridge 收尾表执行（design 按表引用）：5 秒写期限只覆盖底层写退出与协调锁释放，回执传输不计入；写超时且 WS 仍可用时 MUST 先生成并入队 `write_failed` 回执，PTY 后备终止产生的 EOF MUST NOT 抢先取消该回执，释放协调锁后执行独立、有界的 writer 收尾；WS 已取消、写队列满或回帧失败不保证回执到达（客户端按断线归"结果未知"，MUST NOT 补写）；后备终止引起的服务端故障关闭 MUST 使用 close code 1011（避免 1000 导致前端停止自动重连，证据：session.ts:378、ws_terminal.go:195-200/:178/:226-232）；原生 PTY deadline 超时（无需后备终止）的故障收尾同样统一 1011，对端不可达时允许没有关闭帧（预算耗尽直接关底层连接）；「恒为 1011」仅当故障收尾先提交关闭原因时适用。故障与替换并发时的关闭原因仲裁按 delivery spec bridge 收尾表「故障与替换并发」行执行（design 按表引用）：终止原因 MUST 在同一协调边界（per-task 协调锁或等价的 per-conn 提交点）内提交、以先提交者为准——故障先提交由 bridge 完成 1011 收尾（替换 handler MUST NOT 另发 4009）；替换先提交由替换流程拥有关闭（4009），后续取消/超时产生的写失败 MUST NOT 覆盖该原因（coder/websocket 仅第一次 Close 执行握手，close.go:99；前端 4009 停止重连、1011 自动重连，session.ts:370/:378）。收尾预算冻结（两阶段定义唯一契约见 delivery spec bridge 收尾表）：底层写退出、后备终止及锁释放受 5 秒写期限约束；释放协调锁后启动独立的 1 秒 writer/连接收尾预算，覆盖回执写出等待、关闭握手、必要的底层 transport 强制关闭及该阶段退出，两阶段不得互相借用；1 秒预算 MUST 经可独立于 `Conn.Close` 强制关闭底层 transport 的适配边界兑现（如握手前包装底层 net.Conn 并保留句柄，预算耗尽直接关底层连接）——现有 `Conn.Close` 写关闭帧最多等 5s、`CloseNow` 不可抢占进行中的 `Close`（Close 后 waitGoroutines 上限 15s）、取消空闲 writer 不保证中止关闭握手，禁止以「Close + 延迟 CloseNow」冒充 1s 预算。
- **为什么**：与普通输入同一 goroutine 同一 `p.Write` 出口（ws_terminal.go:252-282），天然获得与键盘输入的串行排序；bracketed paste 触发 opencode 原生 `pasteInputText` → `readLocalAttachment` 附件管线。
- **tmux 转发**：tmux 3.5a 源码确认 attach 客户端输入中的 `ESC[200~`/`ESC[201~` 被识别为 KEYC_PASTE_START/END（[tty-keys.c](https://github.com/tmux/tmux/blob/3.5a/tty-keys.c)）；CLIENT_BRACKETPASTING 状态在 attach 客户端上维护（[server-client.c](https://github.com/tmux/tmux/blob/3.5a/server-client.c)）；pane 开启 MODE_BRACKETPASTE 时输出起止标记、未开启时过滤该序列（[input-keys.c](https://github.com/tmux/tmux/blob/3.5a/input-keys.c)）。opencode TUI 启用 bracketed paste。**该链路必须在目标 Linux tmux 版本上实测（D8 spike）**；若实测不通，后备方案为命名 buffer + `tmux paste-buffer -p -d`（需新的实测与生命周期管理，不在首版范围，仅保留注入函数边界扩展位）。
- **路径安全不依赖"随机命名保证"**：落盘名固定为 `[32hex][合法扩展名]`（D4），`uploadsDir` 配置值在加载时拒绝反斜杠 `\` 与控制字符；注入前对**最终绝对路径**复查反斜杠与控制字符（判定标准固定：rune < 0x20 或 rune == 0x7F），不通过则拒绝投递、不写 PTY（校验优先级第 6 步，error 取 `invalid_input`，确定零 PTY 写入；同时记录日志）。最终路径允许空格与中文（无需 shell 转义：无 `@`、无回车、无反斜杠、无控制字符）。

### D4: 受管存储、命名与生命周期

- 目录布局：`<uploadsDir>/<taskID>/<32hex><ext>`；`uploadsDir` 默认 `$OCDECK_DATA_DIR/uploads`。
- 权限：目录 0700、文件 0600（opencode 与 server 同用户同机，可读本机路径）。
- **命名与路径安全**：落盘名 = crypto/rand 16 字节的 32 位 hex + 合法扩展名。扩展名提取自原始文件名：小写化后须匹配 `^\.[a-z0-9]{1,10}$`，不匹配或缺失则落盘文件**无扩展名**（仍可投递，opencode 按未知类型处理为路径文本）。客户端原始文件名仅取 basename 作展示元数据，MUST NOT 用于落盘路径。`uploadsDir` 在 config 加载时绝对化（同 DataDir 模式），允许空格/中文，MUST 拒绝含反斜杠 `\` 或控制字符（rune < 0x20 或 rune == 0x7F）的配置值（启动报错）。
- **写入与提交**：先写 `<name>.partial`，完整写入后原子 rename 为最终文件名，再提交 sidecar；文件与 sidecar 均成功后才加入可投递索引并发布 uploadId（提交顺序、发布点与 sidecar 失败语义见 delivery spec「保留生命周期与回收」）；finalize（rename）前在同一 per-task 协调锁内复查（连接替换后同样复查），顺序：任务存在（否则 404）→ active（否则 409）→ 当前 connId（否则 403）——任务状态准入表仅 `status == active` 通过（共享表见 delivery spec「连接归属与投递准入」）；任一失败 MUST NOT rename、MUST NOT 发布 uploadId，且清理 `.partial`。task 删除后在途上传 MUST NOT 发布有效 uploadId。
- **三段计量**：唯一计量表（字节域定义、冲突优先级、验收用例）见 delivery spec「文件上传接口与安全约束」，design 按表引用，不重复数值。要点：file part 按不解码的 raw part 语义读取；读到最终 boundary 后 MUST 继续有界消费请求体至 EOF，再原子 rename 提交；`M + 1MiB` 仅为防御兜底（合规请求理论最大 M+64KiB）。
- **生命周期（TTL 默认关闭）**：
  - `OCDECK_UPLOAD_RETENTION`：缺省或 `0` = 关闭 TTL；正 Go duration = 启用；负值或非法值 → 启动报错。
  - TTL 启用时：过期基准 = 上传完成时间；仅完整成功投递（deliver_result `ok:true`）刷新过期时间（刷新即更新 sidecar `lastDeliveredAt`）；`now >= expiresAt` 即不可投递（deliver 回 `expired`）。物理删除由周期清理执行——验收口径为"过期后的下一轮每小时扫描完成清理"，不要求恰好到期删除。
  - **TTL 元数据唯一真值（sidecar 方案）**：磁盘 sidecar `<最终文件名>.meta.json`（与最终文件同目录）。固定 JSON schema（与 delivery spec「保留生命周期与回收」同文逐字）：`{"uploadId":"<32hex>","taskId":"<id>","connId":"<uuid>","filename":"<最终文件名>","uploadedAt":"<RFC3339Nano UTC>","lastDeliveredAt":"<RFC3339Nano UTC>|null"}`；序列化 MUST 始终输出全部键；`lastDeliveredAt` 为 `null` 表示从未成功投递；键缺失或类型/格式非法视为损坏 sidecar；不引入通用存储框架、不用 SQLite 表。过期时间计算公式固定：`expiresAt = (lastDeliveredAt ?? uploadedAt) + TTL`。提交顺序与发布点：先写文件 `.partial` → 原子 rename 为最终文件名；再写 sidecar `.tmp` → 原子 rename 为 `.meta.json`；两者均成功后才加入可投递索引并返回 201；sidecar 提交失败 → 500 `internal`，尽力撤销已 rename 的文件（撤销失败记录日志，残留按孤儿规则处理），MUST NOT 返回 uploadId。投递刷新（更新 lastDeliveredAt）失败：MUST NOT 重写 PTY、MUST NOT 影响 `ok:true` 回执；MUST 保留磁盘上已持久化的 `lastDeliveredAt`（不得覆盖或回退到 `uploadedAt`）；记录日志、不重试（下次成功投递再刷新）。启动恢复按 delivery spec「保留生命周期与回收」的启动恢复分类表执行（design 按表引用）：有效配对判定含配对不变量（`taskId`＝扫描目录名、`filename`＝去 `.meta.json` 后的配对文件 basename 且符合受管命名规则、`uploadId`＝文件名 32hex 前缀，任一不一致归损坏 sidecar 按孤儿规则；"对应文件"固定指扫描位置的同名配对文件，MUST NOT 按损坏字段寻找或删除其他路径）与读取失败语义（sidecar 权限/I/O 读取失败 MUST NOT 归孤儿、MUST NOT 删除，记录日志并在下一轮清理/启动扫描重试）；有效配对进入可投递索引；缺 sidecar 的文件、缺文件的单边 sidecar、损坏 sidecar 及其对应文件、遗留 `.tmp` 均为孤儿，MUST NOT 进入投递索引、MUST NOT 恢复为可投递上传，孤儿统一规则：modtime 超过 1 小时由周期清理删除，独立于 TTL 开关（TTL 关闭时同样删除）。mtime fallback（文件 mtime）仅用于孤儿清理的时间计算，MUST NOT 用于重建可投递上传的过期基准，MUST NOT 把重启时刻当作上传完成时刻。TTL 配置变化：清理计算以清理执行时的当前配置值为准。
  - task 删除 → 该 task 上传目录失效并回收：订阅既有 `task.deleted` 发布点（delete_reconcile.go:44）；**挂起、重连、删除 opencode 对话 session 不触发回收**。
  - 周期清理（每小时）：活跃上传不删——删除 `.partial` 前 MUST 先确认其不属于任何活跃上传；`.partial` 仅当 modtime 超过 1 小时（停滞上传）才删；已完成文件按 expiresAt 删除（仅 TTL 启用时）；启动恢复分类表中的孤儿文件按孤儿统一规则删除（modtime 超 1 小时，独立于 TTL 开关）；TTL 关闭时仍清理孤儿 `.partial` 与已删除 task 的目录。启动重扫同一套规则。
  - 失败语义：清理/查询失败时任务查询失败 MUST NOT 当作"任务不存在"而误删；删除失败记录日志，下一轮重试。
  - **活跃上传登记**：server 内存登记活跃上传（uploadId 预留 + task 绑定，结束/失败时注销）。活跃上传进度/超时判定按 delivery spec「保留生命周期与回收」的活跃上传进度/超时表执行（design 按表引用）：登记起点 = handler 通过认证/Origin/任务准入后开始读取请求体时；进度字段为内存 `lastProgressAt`（不落盘），读取到任何非零请求体字节即刷新；独立 1 小时停滞计时器在阈值处直接触发现有取消收尾流程（不等待周期扫描轮次）；磁盘 mtime 仅承担非活跃 `.partial`/孤儿清理，不参与活跃上传停滞判定。取消收尾流程固定（唯一契约见 delivery spec「保留生命周期与回收」）：① 持 per-task 协调锁标记该上传不可提交并发出取消信号 → **释放协调锁**；② 中断请求体读取、等待上传执行结束；③ 重新持锁确认身份及非活跃后删除文件（注销登记）。MUST NOT 持协调锁等待 handler 退出（防等待环：handler finalize/失败收尾也需同一锁）；取消标记后的 finalize MUST NOT 发布 uploadId（不返回 201）；HTTP 尚可响应时固定返回 408（信封 code 用现有 `invalid_input`，message 用固定上传停滞提示文案，唯一来源见 delivery spec 上传接口错误表），客户端已断开则不保证响应。启动时进程新建、无活跃上传，上次遗留 `.partial` 按孤儿规则清理。
  - 串行化：上传提交（rename 前）、投递、清理、task 删除对同一 task 目录的操作经 **per-task 协调锁**（或等效单线程协调）串行化；**任务删除/挂起的实际提交**（DB 状态提交，不只是事件订阅）也进入该锁，使在途上传 finalize 复查失败、投递准入失败；`task.deleted` 事件（delete_reconcile.go:44，发布在 DB 删除之后）仅用于**触发回收**，不承担串行化职责。
- **配置项**（config.go `Load` 同款 env 模式）：
  - `OCDECK_UPLOAD_DIR`（默认 `$OCDECK_DATA_DIR/uploads`；加载时绝对化；拒绝控制字符）
  - `OCDECK_UPLOAD_MAX_BYTES`（默认 `20971520`；合法范围 `1MiB ≤ M ≤ 100MiB`，越界或非法值拒绝启动）
  - `OCDECK_UPLOAD_RETENTION`（缺省/`0`=关闭；正 duration=启用；负值/非法值拒绝启动）
- **备选**：上传后立即删（opencode 异步读盘会读到不存在文件，否决）；默认保留期（TTL 默认关闭、以 task 删除为主回收，保留期仅作可选运维手段，否决）。

### D5: 连接归属模型（connId）与投递准入

- 存储所有者 = task；投递授权绑定三元组 `(taskID, 终端类型=TUI, server 签发的连接 ID connId)`。
- **connId 签发与下发**：handler 层为每条新连接签发 `connId`（uuid），注册（`wsClients.register`，ws.go:172-191）后随 auth_ok 帧下发：`{"type":"auth_ok","connId":"<uuid>"}`。
- **上传准入**：上传请求携带 `connId`（multipart **首个 part**，分阶段解析顺序见 D1），server 在解析出该字段后校验它是该 taskID TUI 终端的**当前**连接 ID，否则拒绝上传（403 forbidden）。
- **投递校验唯一优先级**（多项同时失败时按此顺序返回首个失败；与 D2 error 枚举一一对应）：
  1. 终端类型/连接当前性（非 TUI 或 connId 非当前 → `forbidden`）
  2. 任务活跃性（挂起/删除 → `task_inactive`）
  3. uploadId 查找（不存在 → `not_found`）
  4. task/connId 归属（不匹配 → `forbidden`）
  5. TTL（过期 → `expired`）
  6. 文件存在性与路径校验（物理文件缺失 → `not_found`；路径含反斜杠/控制字符 → `invalid_input`）
  7. 写入（失败 → `write_failed`）

  shell 终端连接收到**合法 deliver 帧**（uploadId 为 32 hex）在第 1 步即拒绝（`forbidden`）。
- **原子边界（线性化）**：投递准入检查（连接当前性 = 该 connId 仍为注册表当前值、任务活跃）与 PTY 写入必须在**同一 per-task 协调锁临界区**内完成；连接替换提交、任务挂起/删除提交也进入同一把锁。线性化点 = 临界区内校验+写入完成的时刻：替换/挂起/删除提交先获锁则投递准入在校验时失败（零注入）；投递先获锁并完成写入则不承诺撤销。投递写入使用 5 秒服务端写 deadline（退出契约见 D3），锁在确认底层写操作已结束后才释放。等待 4009 关闭握手时 MUST NOT 持有该锁（慢关闭客户端不得阻塞投递/替换路径）。
- **协调锁映射与加锁顺序**：per-task 投递协调锁的写侧协调点落在"离开 active 的事实提交入口"——`Manager.Suspend` 的 active→suspending 提交（internal/task/suspend.go:19）、删除意图提交（`BeginDeleteIntent`/`BeginRetryDeleteIntent`，delete_reconcile.go:13-31）、最终删行提交（`DeleteTask`，delete_reconcile.go:35-47），以及任何其他离开 active 的状态提交。加锁顺序固定：既有 Manager 任务锁 → per-task 投递协调锁；投递链路持协调锁时 MUST NOT 反向获取任务锁（防死锁）。
- **基础设施错误语义**：投递准入遇非业务错误（任务查询失败、上传元数据读取失败、文件 stat 权限/I/O 错误）且尚未写入 PTY：MUST 停止后续校验与 PTY 调用，记录内部错误日志，以 WS close code 1011 结束连接；MUST NOT 伪造 `not_found`/`task_inactive`/`write_failed` 等业务码（前端按断线进入"结果未知"）。HTTP 侧任务查询/存储基础设施故障 → 500 `internal`。只有已确认不存在/非活跃才映射对应业务码。
- **客户端代次**：客户端闭包捕获上传时的 connId；WS 重连且 auth_ok 下发新 connId 后，旧上传回调 MUST NOT 转投新连接——上传响应返回但 connId 已过期 → 该项按 D7 投递状态机规则分类（从未发送 deliver = 明确失败；已发送无确定回执 = 结果未知），不自动重发。
- 上传中任务被挂起/删除：删除/挂起提交持 per-task 协调锁，投递准入在第 2 步拒绝（`task_inactive`）。

### D6: 输入门禁在浏览器侧执行

终端锁定状态是浏览器侧状态（input-gate.ts `locked`），server 无感知。因此：

- 两个常态入口共用门禁：上传前检查 `shouldSendInput` 同款判定（authed/wsOpen/locked），发送 deliver 前复查。
- 文件投递是**非合成输入**：门禁调用中 `syntheticInFlight` 固定传 `false`。input-gate.ts 对合成输入有锁定例外（`locked && !syntheticInFlight` 才拦截），该例外 MUST NOT 被无差别复用——锁定状态下文件上传与 deliver 都 MUST 被拦截并提示解锁。
- server 侧不依赖客户端门禁，仍以 D5 校验为权威；门禁是 UX 层防误操作，不是安全边界。

### D7: 前端捕获与投递状态机

- 新模块 `web/src/terminal/file-delivery.ts`：两个常态入口（paste/drop）进入**同一个文件队列**，共用门禁、归属（connId）、状态机与重试逻辑；文件选择器无常驻入口按钮，仅用于失败/未知项的重试重选（隐藏 input 保留）。仅挂在 `TerminalView` 的 **TUI 实例**上，shell 终端不挂载任何入口；随终端实例生命周期挂载/清理。
- **paste**：终端容器 capture 阶段原生 listener（先于 xterm textarea）；首个 `await` 前同步提取；确认含文件后同步 `preventDefault()` + `stopPropagation()`；按 `clipboardData.items` 顺序提取有效 File，存在有效 items 时**不合并** `files` 兜底，仅当 items 无有效 File 时才读 `files`；**禁止按文件名/大小推断重复**。纯文本不拦截；文件+文本混合以文件为准。
- **drop**：`dragover` 阻止默认 + `dropEffect='copy'` + 悬停视觉反馈（dragenter/leave 计数防闪烁）；`drop` 阻止默认防浏览器导航；混合拖入逐项判定：目录项（`DataTransferItem.webkitGetAsEntry()?.isDirectory`）分项拒绝并提示，普通文件继续处理，不遍历目录；拖拽阶段 `files` 可能为空，只在 drop 时读取。
- **投递状态机（每项）**：
  - `上传中 → 待投递 → 等待回执（deliver 已发）→ 已发送到终端`——"已发送到终端"仅当**当前请求 + 当前 connId 匹配**的 deliver_result `ok:true`。
  - `明确失败`：**注入前准入拒绝**（`forbidden`/`task_inactive`/`not_found`/`expired`/`invalid_input`——确定零 PTY 写入）或上传失败，或**从未发送 deliver 的旧代次操作**；可手动重试（= 重新上传）。
  - `结果未知`：**已发送 deliver 但无确定回执**——回执等待超时 10s / 等待回执期间 WS 断线 / `write_failed`（可能部分写入）；**不自动补发**；可手动重试，UI 提示可能重复。
  - 普通 WS 断线 MUST NOT 把已成功项改为未知。
  - 同一 uploadId 同时最多一个待确认投递；状态已迁移的迟到回执丢弃并记录日志。
- **connId 能力降级**：auth_ok 缺失或 connId 非法（空串/非 uuid 形态）时，WS 连接照常建立、普通终端功能不受影响；文件功能置为不可用——常态入口捕获到文件时提示"当前 server 版本不支持文件投递"，MUST NOT 发起 HTTP 上传、MUST NOT 发送 deliver 帧，MUST NOT 以空串/undefined/自造值充当 connId。另一条失败路径（非能力降级）：connId 有效但上传接口返回 404（业务 404——如任务已删除——与旧 server 无该路由不可区分）→ 提示"上传目标不可用，任务可能已删除或服务端不支持该接口"，仅终止当前上传项；MUST NOT 把整个连接的文件能力永久降级（后续新上传仍可尝试）。
- **手动重试 = 重新上传**：取得新 uploadId，旧尝试永久结束，旧 uploadId 的回执到达只丢弃 + 日志；重新上传须满足当前 connId（D6 门禁 + D5 代次）；原 File 对象已不可用时 UI 提示用户重新选择文件：打开**绑定该项重试意图的文件选择器**，选中后为该项创建新尝试（新 uploadId，进入暂停期间允许执行的重试调度），用户取消选择则该项保持原状态（未知项所在共享队列保持暂停；完整状态路径见 delivery spec「多文件完整性与投递节奏」共享队列状态表，暂停期间新增项不隐式替代未知项）。
- **多文件投递节奏（队列级）**：常态入口共享单一队列（重试重选路径同样进入该队列）。队列级暂停/恢复、未知项不因队列恢复补发、单项失败（含本地门禁拒绝——锁定态/未连接，未调用上传/发送接口，归"明确失败"）的分类与后续项继续判定，均以 delivery spec「多文件完整性与投递节奏」的共享队列状态表为唯一来源，design 不重复维护。
- **状态 UI**：终端区域内的轻量浮层列表，逐项展示文件名/状态/重试；成功文案为"已发送到终端"。项进入"已发送到终端"后经约 1200ms 呈现层驻留供用户确认，随后从浮层隐去（仅呈现层，状态机不删项）；浮层仅在存在可见项或提示时呈现——仅剩已隐去的成功项时整个浮层消失，不持续遮挡终端输入区；失败与"结果未知"项始终保留。浮层"明确失败/结果未知"仅为分项反馈状态，MUST NOT 替代文件消费完整性验收——正常链路、有效文件、目标 TUI 可接收时，每个文件 MUST 在 TUI 实际出现对应结果（附件占位/内联/路径文本）；最终附件**排列顺序**不承诺（spike 实测记录，见 D8）。
- **重连代次**：上传回调闭包捕获上传时 connId（异步完成后仍须满足 D5 代次要求）；auth_ok 更新 connId 后旧回调不得转投新连接。

### D8: Spike 先行与验证记录

- 实施第一步为端到端 spike（目标 Linux 环境）：上传单张 PNG → deliver → TUI 出现 `[Image 1]`，无路径残留、无重复、无自动提交；同时验证 tmux bracketed paste 转发。
- **多文件 spike**：两个图片、不同大小、混合类型；**完整性为强制项**——每个文件都要在 TUI 出现对应结果；排列顺序仅记录实测现象、不作为验收。
- **记录要求**：目标 opencode / tmux 版本号；源码证据路径（opencode v1.18.30 源码 clone，commit 859106eb17d5b840475f5e4b78e64c9622f8750e，含 `prompt/index.tsx`、`component/prompt/local-attachment.ts`；tmux 3.5a 源码文件取 GitHub tmux/tmux 3.5a tag，含 `tty-keys.c`/`server-client.c`/`input-keys.c`）；测试步骤；结果记录。
- **兼容区间**：项目既有已验证 opencode 区间为 [1.18.14, 1.18.26]（internal/infrastructure/opencode/CONTRACT.md:3-14）；本变更附件链路针对 opencode v1.18.30 的验证单独记录于本 change；扩展项目兼容区间遵循 CONTRACT.md 既有 SOP（锚点 diff + live probe）。
- spike 不通过 → 暂停全面实现、回到设计（评估 `paste-buffer` 后备方案）；**范围收缩须提交用户决定**，不自行裁剪。

### Spike 记录（2026-09-11，本机 macOS；用户确认"远程连接 macOS 为同等场景"，目标 Linux 部署仍需按 Risks 提示实测其 tmux 版本）

- 环境：tmux 3.7c、opencode 1.18.30（/opt/homebrew/bin）；探针复刻生产链路：PTY attach `tmux -L ocdeck-spike attach-session` → 向 attach ptmx 写入单个完整 `ESC[200~`+绝对路径+`ESC[201~`（不带 @、不加回车）→ `capture-pane` 观察。
- 单文件：64×64 PNG 注入后 TUI 出现 `[Image 1]`，无路径残留、无重复、无自动提交。**通过**。
- 多文件：连续注入第二张 PNG + PDF → prompt 显示 `[Image 1] [Image 2] [PDF 1]`，**完整性 3/3 通过**；本次实测顺序与注入顺序一致（仅记录现象，不作为验收口径）。
- 异常状态：先经 attach 客户端进入 copy-mode 再注入路径 → 未产生 `[Image 3]`，退出 copy-mode 后 prompt 无变化。注入在 copy-mode 下不被 prompt 消费，与普通键盘输入同语义（用户自行负责焦点），符合 Risks 既定表述。
- 附件链路行为核验（1.18.30）：bracketed paste 绝对路径 → 扩展名分类（png→图片附件、pdf→PDF 附件）→ `[Image N]`/`[PDF N]` 占位；与 local-attachment.ts/prompt/index.tsx 锚点行为一致。

## 主要实现落点

| 位置 | 职责 |
|---|---|
| `web/src/terminal/file-delivery.ts`（新） | 事件捕获、上传、deliver、投递状态机（D7） |
| `web/src/terminal/TerminalView.tsx` | 仅 TUI 实例挂载/清理 listener、状态浮层（成功项驻留后隐去）、隐藏文件选择器（仅重试重选路径使用） |
| `web/src/terminal/session.ts` | 新增 `sendDeliver(uploadId)`（门禁复查 + connId 代次 + deliver 文本帧） |
| `web/src/terminal/input-gate.ts` | 复用 `shouldSendInput`；文件投递固定 `syntheticInFlight=false`（D6） |
| `web/src/api.ts` | 新增 `uploadAttachment(taskID, file, connId)` |
| `internal/api/uploads.go`（新） | 上传 handler：multipart 解析与校验顺序（D1）、MaxBytesReader 总量兜底、调用 application 编排、错误映射、返回 uploadId；文件落盘与 rename 由 infrastructure 执行 |
| `internal/api/errors.go` | 新增 `forbidden`、`payload_too_large` ErrorCode（沿用 snake_case 命名风格） |
| `internal/api/ws_terminal.go` | auth_ok 增 connId；文本帧显式类型分发 + deliver 处理（D5 校验 + D3 注入 + deliver_result 经共享有界写队列回帧）；bridgeTerminal 持有共享写队列 |
| `internal/infrastructure/uploads`（新包） | 受管存储：命名、写入/原子 rename、元数据、归属查询、保留期、定期/启动清理、删除 |
| `internal/application`（uploads 编排，新） | 上传与投递准入、活跃上传登记（D4）、任务生命周期协调（`task.deleted` 订阅仅触发回收，串行化走 per-task 协调锁）、保留规则；持有窄存储端口 |
| `internal/config/config.go` | `OCDECK_UPLOAD_DIR/MAX_BYTES/RETENTION` 三项配置 + 绝对化与控制字符校验 |
| `internal/api/server.go` | 注册上传路由（api 子 mux，自动获得 Bearer 中间件） |

## Risks / Trade-offs

- [tmux 版本差异导致 bracketed paste 转发失败] → D8 spike 前置；后备 `paste-buffer` 方案留档不实施（仅保留注入函数边界扩展位）。
- [多附件在 TUI 异步 onPaste 下最终顺序不确定] → 完整性为强制验收、顺序不承诺（已写入 spec）；串行投递节奏降低乱序概率；spike 实测记录；不可靠则暂停实现、范围收缩提交用户决定。
- [TTL 默认关闭下上传目录磁盘占用随 task 存活增长] → 单文件上限强制（默认 20971520 字节）+ task 删除触发目录回收 + 停滞 `.partial` 每小时清理；TTL 为可选运维手段（`OCDECK_UPLOAD_RETENTION`）。
- [deliver 无 TUI 层 ACK，"已发送"≠"已成附件"] → 回执三态（已发送/失败/结果未知）；文案"已发送到终端"；附件成立以 TUI 占位符为准；结果未知不自动补发、手动重试提示可能重复。
- [图片读盘/base64 峰值内存由 opencode 侧承担；不做图片压缩] → 单文件大小上限兜底；opencode 侧图片缩放默认启用（默认阈值与限制见 Context「opencode TUI 行为」节）。
- [非活跃 pane/copy-mode/TUI 对话框等状态下注入去向不可控] → 与普通键盘输入同等语义（用户自行负责焦点），文档提示；spike 覆盖常见异常状态。
- [清理器把任务查询失败误判为任务不存在而误删] → 查询失败不删、记日志、下轮重试（D4 失败语义）。

## Migration Plan

- 纯增量：新路由、新配置（默认值即可用）、新增错误码、WS 协议新增类型与 auth_ok 新字段。无数据迁移。
- 新旧组合：新 client + 旧 server → auth_ok 无 connId：WS 连接照常建立、普通终端功能不受影响，文件功能降级不可用（提示"当前 server 版本不支持文件投递"，不发起 HTTP 上传、不发送 deliver 帧，不以自造值充当 connId）；connId 有效但上传返回 404（业务 404 与旧 server 无路由不可区分）→ 提示"上传目标不可用，任务可能已删除或服务端不支持该接口"、仅终止当前上传项，MUST NOT 把整个连接的文件能力永久降级（后续新上传仍可尝试）。旧 client + 新 server → 无 deliver 帧，行为不变（auth_ok 新增字段对不解析未知字段的旧客户端无感知）；新 server 对未识别文本帧从"回落写 PTY"改为"丢弃+日志"（现有客户端文本帧仅 auth/resize，证据见 D2，零影响）。
- 回滚：删除路由、配置与新增错误码即可。**回滚到无上传组件的版本时，遗留上传文件由运维按受管目录（uploadsDir）手工清理；或回滚版本保留清理器继续回收。** 不依赖"启动清理兜底"——回滚后清理组件已不存在。

## Open Questions

- 多附件在 opencode TUI 异步 `onPaste` 下的最终顺序：spike 实测记录，**不作为验收口径**（完整性才是）；不影响 specs 与任务拆分（串行投递已含）。
- 若 spike 实测不通过（bracketed paste 转发或多文件完整性）：暂停全面实现回到设计，范围收缩提交用户决定。
