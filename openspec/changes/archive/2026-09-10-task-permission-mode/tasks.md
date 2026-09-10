# Tasks: task-permission-mode

实施组织：单 lane，顺序执行（design D10）。每个任务附验证方式；行为测试须提供有效性证据（旧实现下失败、新实现下通过，或等价 mutation 验证）。

## 1. 字段与全部读写映射（D1/D2/D7）

- [x] 1.1 新增迁移 `internal/infrastructure/store/migrations/0015_task_permission_mode.sql`：`ALTER TABLE tasks ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'ask'`；验证：迁移后存量行读为 `ask` 的测试通过
- [x] 1.2 task 包定义取值常量 `PermissionModeAsk/AllApprove/AIAuto` 与 `resolvePermissionMode(row)` 纯函数（空串防御→ask；未知值 fail-closed internal error，types.go TaskMode 同处）；验证：表驱动测试覆盖三值/空串/未知值
- [x] 1.3 store 层贯通：`TaskRow` 加 `PermissionMode`，`CreateTask` INSERT 显式写入，查询 SELECT 补齐；验证：显式写三值 roundtrip 测试通过
- [x] 1.4 application 层贯通：`TaskRow`（dto.go）与 `CreateTaskOptions` 加 `PermissionMode`（空串=缺省）；`Manager.Create` 缺省归一化为 `ask` 后落库（task 层 fail-closed 复核非法值，invalid_input 零副作用）；验证：创建非法值→invalid_input 且零落库/零 worktree 副作用测试通过
- [x] 1.5 api 层贯通：`createTaskReq.PermissionMode *string`（presence 语义），本地常量镜像三值，校验顺序 name → mode → permission_mode（trim 后空串/未知值→invalid_input）；handleCreateTask 透传；验证：非法/空串/缺省三态 + 校验顺序的 API 测试通过
- [x] 1.6 输出矩阵透出（镜像 TaskMode）：创建响应/任务详情 DTO、项目任务摘要（ListProjectTaskSummaries）、ActiveTaskOverviewRow、内部 TaskSnapshot 与 lifecycle/legacy 双路径适配均含 `permission_mode`；空值输出 ask、未知值 fail-closed internal；验证：四个读模型的透出/空值/坏值测试通过

## 2. argv 注入（D3）

- [x] 2.1 `runtimeCmdArgv` 增加 permissionMode 参数：`all-approve` → 追加 `--auto`，`ask`/`ai-auto` 与现状逐字一致；`startRuntimeWithPortRetry` 调用处经 `resolvePermissionMode` 解析（失败 fail-closed，MUST NOT 启动进程）；验证：argv 表驱动测试（三值 + 解析失败），既有 start_tui_anchor 测试按新签名更新后通过

## 3. Judge/Reply 端口与装配（D5/D6）

- [x] 3.1 opencode 客户端新增 `ReplyPermission(ctx, dir, requestID, reply string)`：POST `/permission/{requestID}/reply`，结果映射按 D6（200 true 成功；200 false/空体/非法 JSON、401、其他 4xx/5xx/超时→普通错误；404 按 `_tag` 分 `ErrPermissionRequestGone` 与 `ErrCapabilityUnsupported`）；`OCClient` 接口与全部 fake/wrapper 同步；验证：表驱动测试覆盖结果映射全部分支
- [x] 3.2 task 包定义 `PermissionJudge` 端口接口（BranchNamer 同型，避免 task→infrastructure/ai 依赖）与 `PermissionJudgeInput`（任务名、Permission、Patterns）、`PermissionVerdict` 三值类型，输入不扩展 metadata/always；infrastructure/ai 实现 `PermJudge`：`ai.Store.State()` 单次快照，未 configured→uncertain；prompt 遵循 D5 保守规则（仅明确安全才 APPROVE、明确危险才 REJECT、其余 UNCERTAIN），严格解析，其他输出=uncertain；Judge 入口派生 10s 总 deadline（初次+能力协商共用）；验证：fake Completer 脚本化各分支 + 协商共享预算测试通过
- [x] 3.3 main.go 装配 `ai.NewPermJudge(aiStore)` 注入 Manager（nil 防御约定：nil judge 时 ai-auto 全部转人工）；验证：构建通过 + 装配测试验证 Judge 注入（nil judge 零 Judge/零 Reply 的行为验收归 4.3）

## 4. 统一消费者（D4）

- [x] 4.1 新增 `internal/task/permit_auto.go`：`judgeScan(rt)` 统一入口 + rt 挂载 `judgedPerms` 与实例就绪状态（newRuntime 未就绪，三条提交路径成功且确认当前实例后置就绪）；准入两阶段：DB active/mode 锁外预检（竞态由 taskOcClient 门槛与最终发送准入兜底）；rt 锁内原子准入（实例就绪 && 未停止 && 未 unsupported）→ 同域登记 judged-set → 锁外 goroutine 逐条判定（条间复核终止条件），**最终发送准入**：taskOcClient 构造后、Reply 前验证 Manager 当前实例同一性 + instVersion + ready/停止/unsupported/ctx；goroutine cancel+join 接入既有 runtime 停止路径（先 cancel judge ctx 再 join SSE，join 无条件）。**本条职责不含判定结果映射与客户端回复执行（归 4.3）**；验证：准入矩阵 + judged-set 去重 + 生命周期（旧实例/停止不发起/不发送）单测通过
- [x] 4.2 接入触发点（只负责触发接线，不含回复行为）：(a) SSE asked 应用后（activate.go:1521-1527）；(b1) 三条就绪提交路径（activate.go CAS :624-645 / reconcile.go:419-424 / suspend.go:128-152，先置就绪再 judgeScan）；(b2) 重连 align 对账完成后（activate.go:1359）；(c) degraded 后台对账完成后（成功与失败重放均触发，不以 changed 为前提）；验证：触发时机单测——提交前暂缓不登记、提交成功/重连/恢复/修复后扫描发起、失败路径不放行（涉及完整回复行为的 D9 全量验收归 4.3）
- [x] 4.3 回复执行与全量行为验收（依赖 3.1–3.3、4.1–4.2）：判定 APPROVE→`once`、REJECT→`reject`、UNCERTAIN/错误→不回复；`ErrPermissionRequestGone`→忽略；`ErrCapabilityUnsupported`→该 runtime 标记 reply-unsupported 停止后续判定；结果未知（分类②）→不重试不补偿不本地断言；回复预算 5s（taskOcClient OpTimeout）；验证：D9 judge/回复及消费者全量行为测试通过——含 nil judge 零 Judge/零 Reply、旧实例/停止零回复、恢复/提交失败零回复、结果未知不本地改状态、unsupported 停止后续判定

## 5. Web 表单与详情展示（D7/D8）

- [x] 5.1 `web/src/types.ts`：`TaskPermissionMode = 'ask'|'all-approve'|'ai-auto'`；`Task`、`TaskSummary` 等响应类型加必填 `permission_mode: TaskPermissionMode`；`api.createTask` 加可选 `permissionMode` 参数映射 JSON `permission_mode`；验证：前端类型检查/构建通过
- [x] 5.2 CommandCenterPage 新建面板加「权限模式」segmented control（工作空间控件同型），缺省 ask；重置规则与 runMode 逐字一致（项目 ID 变更→重置 ask，同项目保持）；三档文案「人工批准/全部批准/AI 自动识别」；任务详情按 `task.permission_mode` 只读展示；验证：前端测试/构建通过，手工核对表单三档提交与详情展示

## 6. 集成验证

- [x] 6.1 全量回归：`go build ./... && go test ./...`（Go 侧）与前端构建/测试通过
- [x] 6.2 行为有效性证据核对：本 change 新增行为测试逐一确认在旧实现下失败、新实现下通过（或对关键放行分支做 mutation 验证），清单随实现提交
- [x] 6.3 `openspec validate task-permission-mode --strict` 通过；人工冒烟（可选）：ai-auto 任务触发权限请求观察自动回复、all-approve 任务观察零 pending

## 7. ai-auto 审计日志（D11）

- [x] 7.1 `internal/infrastructure/auditlog` 新包：`NewPermAuditLogger(path string) (*Logger, error)`——mutex 串行化单行 JSONL 追加写（O_APPEND|O_CREATE|O_WRONLY 0600 + MkdirAll；**打开后 Chmod(0600) 收紧既有文件权限**，OpenFile 创建权限不修正既有文件，runner.go:60-74 先例，Chmod 失败关文件返回错误），一行一次 Write 防交错；task 包定义 `PermAuditLogger` 端口（**`Record(PermAuditRecord) error`**——写入错误返回调用方）+ `PermAuditRecord`（字段与 spec「ai-auto 权限判定审计日志」逐字对应：Time/TaskID/TaskName/RequestID/Permission/Patterns/Verdict/ReplyResult/Reply）；Manager Options 新增 `PermAuditLogger`（nil 防御：零写入零行为差异；**MUST NOT 注入持有 nil 指针的非 nil interface**）；main.go 装配 `<LogDir>/ai-permission-audit.jsonl`，装配失败仅记日志注入 nil 不阻断启动。验证：auditlog 单测（字段完整 JSONL 行/0600——**含预置 0644 文件构造后收紧且保留内容**/追加/并发不交错/写失败返回错误）+ 装配测试
- [x] 7.2 judgeAndReply 接入（锁外、判定/回复分支之后，审计写入不再受 permGate 拦截）：**记录计数从调用判定器起算——调用 Judge 前门禁拒绝零记录；调用后所有终结路径恰好一条**：UNCERTAIN/FAILED 判定终结即写（reply_result=not_applicable）；APPROVE/REJECT 回复已发送按结果写（ok 含 reply 字段 / gone / unknown / unsupported），回复未实际发送（实例失效/停止/ctx 取消/发送前门禁拒绝/客户端构造失败）写 not_applicable 且不含 reply；**判定器返回语义接缝（D5 已同步）**：ai.PermJudge 未配置/非法输出返回 uncertain + 非 nil error，task 侧非 nil error → verdict=FAILED；写入失败仅 log.Printf 不影响判定与回复。验证：四 verdict × 回复结果全分支（含判定后未发送分支）恰好一条记录的行为测试；**真实 ai.PermJudge（httptest）经组合根 adapter 的未配置/非法输出/合法 UNCERTAIN/调用失败/超时 → FAILED/UNCERTAIN 映射验收（不得仅用 fake Judge 手工错误代替）**；写入失败注入行为不受影响；mutation 证据（移除写入调用 → 审计断言变红）
- [x] 7.3 集成验证：`go build ./... && go test ./...` 通过；`openspec validate task-permission-mode --strict` 通过

## 8. 判定上下文增强与 XML 格式（D5/D12）

- [x] 8.1 观察层 metadata 捕获（D12）：`PermissionRequest` 增 `Metadata json.RawMessage`；**SSE 的 `AttentionEvent`/`parsePermissionAsked` 与 REST 的 `parsePermissionRequest` 双入口同步捕获/归一化**（缺失或非法 JSON → nil，MUST NOT 丢弃请求）；upsert/REST 替换/缓冲重放/快照深拷贝携带；等值判断修正（SessionID/Permission/Patterns 原条件不变，仅 metadata 单方变化更新内部存储但不触发 attention_changed 发布）；`toAttentionDTO` 投影不变（metadata MUST NOT 泄入对外 DTO）。验证：双入口携带与等值判断测试、DTO 不泄露测试
- [x] 8.2 判定输入与类别提取（D5）：`PermissionJudgeInput` 扩展平台语境（任务/项目/分支/目录，store 端口无项目查询则新增最小只读方法）+ `RequestDetail`（含 `Degraded` 三值降级原因）；task 层确定性提取纯函数（**严格按 D5 提取表执行**——字段 JSON 路径/类型/必填/完整性条件，界值 command ≤2048B/单文件 diff·patch ≤4096B/files ≤20/总量 ≤8192B，UTF-8 安全截断，截断顺序与遍历序：字段级→files 数量→总量（按类别字段列序、files 原数组序、成员 type→relativePath→patch→movePath，总量从末尾逆向缩减）；`Degraded` 归一化（metadata 仅 JSON object 有效、观察层归 nil 后关键类别记 missing_critical、malformed 仅用于 object 内形状错误、多原因优先级 malformed > missing > truncated）；judgeScanAsync 语境组装（锁外 judgeCtx，不进同步 SSE 入口）；**`permJudgeAdapter` 完整映射链**（task PermissionJudgeInput 全字段 → ai.PermJudgeInput，含平台语境/Detail/Degraded——漏映射会静默丢增强数据）。验证：提取表驱动（含 20/21 文件、恰好界值/超一字节、多字节文本、**多字段总量超限断言具体保留结果**、合法 JSON 非 object、缺字段且超限、多个 files 成员分别异常、多原因优先级用例）+ 降级规则测试 + adapter 映射测试
- [x] 8.3 prompt 与 XML verdict（D5）：system 信任边界（不可信证据声明）+ user JSON 三对象（platform_context/request/evidence）；`<verdict>` 有限标签协议解析（`<verdict`/`</verdict>` 各恰好一次、无属性、固定小写、内容 trim 全匹配三值；缺失/多标签/嵌套/自闭合/带属性/大小写变体一律 FAILED）；**确定性降级短路先于配置检查**（`Degraded` 非空 → uncertain+nil 零 LLM；未配置且降级并存 → UNCERTAIN）；审计 `detail` 字段（string **最终串 ≤1024B 含原因前缀与截断标记**、同源 RequestDetail 摘要、空串字段必有）三处（PermAuditRecord/permAuditAdapter/auditlog.Entry）同步透传。验证：解析验收样例矩阵（正常/标签外理由/缺失/多标签/嵌套/自闭合/带属性/大小写/非法值/注入样本）、降级×配置组合测试（恰好一条 UNCERTAIN 审计记录含原因）、真实 adapter 五态映射更新、SSE/REST→真实 adapter 链路（prompt 收到详情 + 降级标记到达 AI Judge）、所有终结分支审计记录含 detail
- [x] 8.4 集成验证：`go build ./... && go test ./...` 通过；`openspec validate --strict` 通过；mutation 证据（移除 XML 标签要求/移除提取 → 对应用例变红）
