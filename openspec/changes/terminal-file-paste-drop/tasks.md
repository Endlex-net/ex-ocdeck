# Tasks: terminal-file-paste-drop

依赖顺序：① spike 前置 → ② 后端（存储 → 上传 → WS/投递，bridge 单 lane）→ ③ 前端 → ④ Linux 端到端验收。契约以 `design.md`（D1-D8）与 `specs/terminal-file-delivery/spec.md`、`specs/terminal-streaming/spec.md` 为准；本文件不重复契约细节，只组织执行与验证。

## 1. Spike（D8，实施前置门槛）

- [x] 1.1 PTY 注入链路 spike（核心不确定性）：复刻生产链路（PTY attach `tmux attach-session` → 写入单个完整 `ESC[200~`+绝对路径+`ESC[201~`）注入单张 PNG → opencode TUI 出现 `[Image 1]`；验证 tmux bracketed paste 转发（起止标记完整到达 pane、无路径残留、无重复、无自动提交）。验证方式：实测并记录（环境、tmux 3.7c、opencode 1.18.30、观察到的占位符，记录见 design.md D8「Spike 记录」节；用户确认远程连接 macOS 为同等场景，目标 Linux 浏览器全链路留 6.1 验收）
- [x] 1.2 多文件 spike：连续注入两个图片 + 一个 PDF，**完整性强制**——每个文件都在 TUI 出现对应结果（实测 `[Image 1] [Image 2] [PDF 1]` 3/3）；最终排列顺序仅记录实测现象（不作为验收）。同时覆盖常见异常状态观察（copy-mode 下注入不被 prompt 消费，与普通键盘输入同语义）。验证方式：实测记录每文件结果（design.md D8「Spike 记录」节）
- [x] 1.3 spike 结论归档：通过 → 继续 2.x；不通过（转发失败或完整性不达标）→ **暂停实现回到设计**，范围收缩提交用户决定，不自行裁剪。验证方式：spike 记录写入 design.md D8 节或独立记录文件
- [x] 1.4 按 `internal/infrastructure/opencode/CONTRACT.md` SOP 记录 opencode v1.18.30 附件链路验证结果（粘贴路径分类、附件占位行为）。验证方式：CONTRACT.md 要求的验证记录产物存在

## 2. 后端：配置与受管存储

- [x] 2.1 `internal/config/config.go`：新增 `OCDECK_UPLOAD_DIR` / `OCDECK_UPLOAD_MAX_BYTES` / `OCDECK_UPLOAD_RETENTION` 三项（默认值与 D4 一致；TTL 缺省/0 = 关闭），含绝对化、M 合法范围（1MiB ≤ M ≤ 100MiB，非法启动报错）、uploadsDir 反斜杠与控制字符（rune < 0x20 或 == 0x7F）启动拒绝。验证方式：config 单元测试（默认值、非法值、控制字符）通过
- [x] 2.2 `internal/infrastructure/uploads`（新包）：受管存储执行层——受管命名（32hex + 合法扩展名规则）、流式落盘 `.partial` → 原子 rename、sidecar（六键 schema、`.tmp`→`.meta.json`、提交顺序与失败撤销）、归属查询、扫描（启动恢复分类表：有效配对/孤儿/损坏/读取失败重试）、周期清理（孤儿 >1h、TTL、停滞 `.partial`）。验证方式：包级单元测试（含时钟注入、存储错误注入、启动恢复用例：字段不一致→孤儿、读取失败→不删重试）通过
- [x] 2.3 `internal/application`（新编排）：上传**初始准入**（任务存在/活跃、connId 归属，D1 分阶段顺序中的准入阶段）与 **finalize 复查**（D4）；**投递七步准入**（D5：连接当前性→任务活跃→uploadId 查找→归属→TTL→文件/路径→写入）；活跃上传登记与 `lastProgressAt` 超时表（独立 1h 计时器、三步取消收尾、408 语义）；保留规则（TTL 刷新、失败保留已持久化时间）；持有窄存储端口。**上传与投递是两套准入顺序，不得混用**；准入决策唯一归属本层，API/WS 层只做协议解析与调用编排。验证方式：application 层单元测试（fakes：窄端口/时钟/事件）覆盖上传准入与 finalize 复查顺序、投递七步优先级全表、停滞取消与 finalize 竞争、TTL 刷新失败语义，通过
- [x] 2.4 组合根（`cmd/ocdeck-server/main.go`）：构造注入、启动扫描、周期清理器启停、优雅关闭（无 Fx，显式构造）。验证方式：组合根启停验证测试通过
- [x] 2.5 生命周期协调接入（design.md:197）：任务实际状态提交进入同一把 per-task 协调锁——接入 `Manager.Suspend`、`BeginDeleteIntent` / `BeginRetryDeleteIntent`、最终 `DeleteTask` 及其他离开 active 的提交；固定锁顺序为**既有 Manager 任务锁 → per-task 协调锁**，禁止反向取锁；`task.deleted` 事件订阅仅触发回收、不承担串行化（delivery spec:190）。本任务是 3.2 上传 finalize 与 4.2 投递闭环的前置依赖；连接替换提交接入归 4.x 单 lane。验证方式：并发屏障测试——提交先行时投递零注入、上传 finalize 不发布 uploadId，通过

## 3. 后端：HTTP 上传接口

- [x] 3.1 `internal/api/errors.go`：新增 `forbidden`、`payload_too_large` ErrorCode（snake_case 风格，`httpStatusFor` 映射 403/413）。验证方式：错误映射单元测试通过
- [x] 3.2 `internal/api/uploads.go`（新）：上传 handler——Bearer → Origin（复用 WS 同一白名单语义，403 先于任何写盘）→ multipart 分阶段解析（首个 part 必须 connId 文本字段、唯一 file part、完整尾部校验）→ 三段计量（payload M+1 探测→413、总量 M+1MiB→413、非文件开销 64KiB→400，首次违规优先）→ **在对应阶段调用 application 上传编排**（初始准入/finalize 复查归 2.3，handler 仅协议解析与错误映射，不落盘不做准入决策；落盘在 infrastructure）→ 201 返回 `{"uploadId": "32hex"}`；上传停滞 408 固定 message `"upload stalled for 1 hour"`。**依赖 2.3/2.5 的编排接口**。验证方式：handler 测试覆盖 spec 上传接口错误表全部行（含尾随垃圾、额外 part、同时违规优先级）；HTTP/application 联动停滞测试——读取阻塞期间、首 part 未读完、file header 阻塞、尾部等待 EOF 四类阻塞下停滞计时器触发取消，验证读取确实中断、无锁等待环、不返回 201、可响应时返回固定 408——全部通过
- [x] 3.3 `internal/api/server.go`：注册上传路由到 `/api/v1` 子 mux（自动获得 Bearer 中间件）。验证方式：路由存在且未认证请求 401 的测试通过

## 4. 后端：WS 协议与投递（ws_terminal.go / bridge 单一 lane）

- [x] 4.1 `internal/api/ws_terminal.go`：`auth_ok` 新增 server 签发的 `connId`（每连接一个）；文本帧显式类型分发三分支（非法 JSON/未知 type → 丢弃+日志+无回帧+无 onInput；`resize` 沿用既有处理不校验 uploadId；仅 `deliver` 校验 uploadId 为 32 hex）。验证方式：WS 协议单元测试（畸形帧、未知类型、resize 回归）通过
- [x] 4.2 deliver 处理（**依赖 2.3/2.5 的编排接口与协调锁**）：WS 适配层——接收 deliver 帧、调用 application 投递编排（七步准入归 2.3，本任务不重复实现准入/TTL 决策）、接入串行 PTY writer（bracketed paste 注入收敛为单一函数边界，与普通输入共用串行写泵，完整写入 `n == len && err == nil` 才算成功）、`deliver_result` 经共享有界写队列回帧；连接替换提交接入 per-task 协调锁（状态提交接入归 2.5）；shell 连接 deliver 一律拒绝。验证方式：投递闭环单元测试（并发屏障：替换/挂起/删除与投递线性化；shell 拒绝）+ 准入基础设施错误注入测试（delivery spec:306——任务查询/元数据读取/文件 stat 故障时停止后续调用、零 PTY 写入、关闭 1011、不伪造业务回执）全部通过
- [x] 4.3 PTY 可中断写入与 bridge 收尾：infrastructure 提供真正可中断单次写入（5s 写 deadline + 连接取消终止，超时后确认底层写结束才释放锁，禁止调用方超时返回留后台 goroutine）；bridge 收尾两阶段预算（5s 写期限管写退出/后备终止/锁释放；锁释放后独立 1s writer/连接收尾预算，互不借用）；关闭原因仲裁（先提交者为准：故障 1011 / 替换 4009，唯一关闭所有者，可独立于 `Conn.Close` 强制关闭底层 transport，禁止 Close+延迟 CloseNow）；所有未完整成功写入（超时/取消/立即错误/无错误短写）统一 `write_failed` + 有条件先入队回执。验证方式：故障注入测试全过——「PTY 不消费写阻塞」「PTY EOF 先到」「WS 先取消」「writer 队列满」「故障先提交」「替换先提交」「立即写错误」「无错误短写」「非超时失败与连接替换竞争」「回执已写出但对端不回关闭帧」（delivery spec:311——实际 transport 关闭与收尾退出均满足独立 1 秒预算）；其中非超时失败用例 MUST 明确验证**关闭码、回执尝试、后续零 PTY 写入**三项断言

## 5. 前端：捕获、上传与状态机

- [x] 5.1 `web/src/terminal/file-delivery.ts`（新）：paste（capture listener 先于 xterm textarea、同步提取 File 后 preventDefault+stopPropagation、items/files 主+fallback、纯文本不拦截）、drop/dragover（防导航、防闪烁）、文件选择器入口；共享队列状态机（排队中（待上传）/上传中/待投递/等待回执 10s/已发送/明确失败/结果未知；write_failed 或回执超时/断线 → 结果未知 + 队列级暂停；仅未知项恢复尝试解除暂停；手动重试 = 重新上传新 uploadId、File 不可用打开绑定该项重试意图的选择器；迟到回执只丢弃+日志）。验证方式：前端单元测试（状态机迁移、暂停/恢复、迟到回执、门禁失败分类）通过
- [x] 5.2 `web/src/terminal/session.ts` + `web/src/api.ts`：`sendDeliver(uploadId)`（门禁复查 `shouldSendInput` 同款判定、`syntheticInFlight` 固定 false、connId 代次绑定、deliver 文本帧）；`uploadAttachment(taskID, file, connId)`（FormData 先 append connId 后 append file）；auth_ok connId 接收与能力降级（无 connId → 提示不支持、不发起上传/投递；有效 connId 上传 404 → 单项失败文案、不永久降级）。验证方式：session/api 单元测试（帧格式、代次过期丢弃、降级路径）通过
- [x] 5.3 `web/src/terminal/TerminalView.tsx`：仅 TUI 实例挂载/清理 listener 与状态浮层（逐项文件名/状态/重试，成功文案"已发送到终端"）、"选择文件"入口；shell 终端不挂任何入口。验证方式：组件测试（TUI 挂载/shell 不挂载、浮层状态呈现、拖拽视觉反馈）通过

## 6. 端到端验收（目标 Linux）

- [x] 6.1 端到端验收：三入口（picker 实测、paste/drop 合成事件）× 图片/PDF/SVG/未知类型 按 spec 场景验收；多文件完整性强制（顺序仅记录）；文本输入回归。实测环境：macOS（用户已确认远程连接 macOS 为同等场景），opencode 1.18.30、tmux 3.7c；锁定态/挂起/断线重连/连接替换/任务删除/上传停滞/清理器行为由单测与集成测试覆盖；Linux 目标环境的 tmux 版本差异验证在部署时按交付说明补做。验证方式：验收记录（见 .slim/deepwork/terminal-file-paste-drop.md P6 节，每场景 PASS/FAIL + 环境信息）
- [x] 6.2 回归：纯文本粘贴、OSC52 复制（终端→浏览器）、resize、旧 client+新 server / 新 client+旧 server 兼容矩阵。验证方式：回归清单全部通过
