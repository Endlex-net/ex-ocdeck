# Design: task-permission-mode

## Context

现状（均已原文/实证核实）：

- 创建链路纵向切片：`createTaskReq{Name, BaseRef, Mode *string}`（internal/api/tasks.go:118-122，Mode 指针保留 presence 语义，校验 145-161）→ `CreateTaskOptions{Name, BaseRef, Mode string}`（internal/application/dto.go:78-82，空串=缺省）→ `Manager.Create` 落库。`TaskMode` 常量与 `resolveTaskMode` fail-closed 解析在 internal/task/types.go:187-241；`mode` 列由 migrations/0013_task_mode.sql 引入（`TEXT NOT NULL DEFAULT` + 定向回填）。domain task.Task 实体不含 mode——该字段只走 store/application/api 路径。
- 激活拉起：`runtimeCmdArgv(port, sessionID)`（internal/task/activate.go:1041-1047）产出 `opencode --port <p> --hostname 127.0.0.1 [--session <id>]`，由 `startRuntimeWithPortRetry`（activate.go:1064）经 tmux `NewSession` 启动，调用处 row（TaskRow）在手。激活提交序：`startSSE` 先于 CAS `activating→active`（activate.go:604-621），SSE 在任务仍为 activating 时已开始分发事件。
- 注意力管道：SSE `permission.asked` 在 activate.go:1517-1528 经 `ParseAttentionEvent` → `rt.applyAttentionEvent` 登记 pending；REST 对账（align 路径 `reconcileTaskAttention`、30s degraded 重试 `retryAttentionDegraded`，均 internal/task/attention.go）用 `ListPermissions` 全量校准，后台成功分支会写入此前未观察到的 pending（attention.go:507-515）。一次性 OCClient 由 `m.taskOcClient(ctx, taskID)`（attention.go:867-890）构造，**仅接受 active 状态任务**。
- 运行时实例：`taskRuntime.instVersion` 单字符串实例令牌（internal/task/manager.go:329-334），既有 fencing/回调校验均按等值判定；runtime 停止路径对 SSE 等 goroutine 做 cancel+join（`sseCancel`，阻塞式）。
- opencode 契约（本机 1.18.30 实证）：`POST /permission/{requestID}/reply`，body `{"reply":"once"|"always"|"reject"}`，成功返回 200 `true`；请求已了结时返回 404 + `{"_tag":"PermissionNotFoundError",...}`。`--auto` 是裸 `opencode` 默认命令的 flag（ocdeck 的 spawn 形态），语义=自动批准未显式 deny 的请求、显式 deny 仍生效（已实证：强制 `bash:"ask"` 下零 pending 直接执行）。
- AI 底座：`ai.Completer` 接口（internal/infrastructure/ai/completer.go:38-40）+ `ai.Store.State()` 一致性快照（infrastructure/ai/config.go，`configured = provider 合法 && api_key 非空 && model 非空 && loadErr==nil`）。Completer 单次 HTTP 超时 10s（completer.go:56），且按 ai-provider-config spec 在 4xx 表明不支持 thinking 参数时剥离参数重试一次（completer.go:95-110）——**单次判定可能包含两次 HTTP 调用**。task 包刻意不 import infrastructure/ai：`BranchNamer` 端口接口定义于 task/types.go:140-148，ai.SlugNamer 在 cmd/ocdeck-server/main.go:127 装配注入，Manager 持有 nil 时防御回退（manager.go:206-208）。
- Web：`TaskMode` 类型（web/src/types.ts:10）、`api.createTask(projectID, name, baseRef?, mode?)`（web/src/api.ts:172）、新建任务面板 `runMode` state 与工作空间 segmented control（web/src/pages/CommandCenterPage.tsx:797-798, 1049-1074）。选择器重置规则（CommandCenterPage.tsx:862-870）：已选项目 ID 变更（含切换项目、清除选择、切到 dir）→ 重置缺省值；同项目信号保持现状。

## Goals / Non-Goals

**Goals:**

- 新增 `permission_mode` 字段沿 TaskMode 纵向切片贯通：store 迁移 → application DTO → api 校验 → task 常量/解析 → 激活 argv → 只读输出 → Web 表单。
- all-approve 经 `--auto` argv 注入实现；ask/ai-auto 进程参数与现状一致。
- ai-auto 复用注意力管道观察权限请求，经全局 LLM 判定后调用回复端点；全部失败/不确定路径转人工。

**Non-Goals:**

- 不提供创建后修改权限模式的入口（无更新 API、无热切换）。
- 不做 per-tool 细粒度权限矩阵、不改动用户 opencode 配置。
- 不改动既有注意力对账/通知语义（ai-auto 是管道旁路消费者）。
- 不消费 `question.*` 事件（问题请求仍全部转人工）。
- ai-auto 判定输入的 metadata 仅在内部观察模型保留，按确定性规则提取后用于判定（第 8 组增强）；不使用上游顶层 `always`/`tool` 字段，不扩展对外 DTO（metadata MUST NOT 泄入 attention API）。

## Decisions

### D1: 字段模型与缺省——`permission_mode TEXT NOT NULL DEFAULT 'ask'`，镜像 TaskMode 切片但不进 domain 实体

- 取值常量定义于 internal/task（与 TaskMode 同处）：`PermissionModeAsk="ask"`、`PermissionModeAllApprove="all-approve"`、`PermissionModeAIAuto="ai-auto"`。
- 迁移 0015：`ALTER TABLE tasks ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'ask'`。DEFAULT 覆盖存量任务=ask（行为等价现状），**无需回填**（比 0013 更简单，无 kind 维度）。
- 与 mode 一样只走 store/application/api 路径，domain task.Task 实体不动。
- 运行时解析单点收口 `resolvePermissionMode(row)` 纯函数：空串（防御）与 `"ask"` → ask；三合法值 → 自身；未知持久化值 → internal error（fail-closed，持久化损坏，同 resolveTaskMode 哲学）。
- 备选：放入 env_snapshot / 不入库——拒绝，权限模式是任务的一等配置属性，需独立列查询与约束。

### D2: 创建链路——presence 语义 + 值域校验前置、零副作用

- `createTaskReq.PermissionMode *string`（json `permission_mode`）：null=缺省 ask；非 null trim 后须为三合法值，否则 invalid_input。校验在既有 name/mode 校验之后、任何副作用之前（参照 tasks.go:145-161 模式）。
- `CreateTaskOptions.PermissionMode string`：空串=缺省（→ ask），非空须合法（task 层 fail-closed 复核，与 Mode 的「api 校验 + task 层复核」双闸一致）。
- api 层本地常量镜像三取值（不经由 task 包，参照 tasks.go:126-129 projectKind/taskMode 本地常量先例）。
- 创建后不可修改：不新增任何更新入口。

### D3: all-approve 施加——argv 追加 `--auto`，ask/ai-auto argv 不变

- `runtimeCmdArgv` 增加 permissionMode 参数：`all-approve` → 追加 `--auto`；`ask`/`ai-auto` → 与现状逐字一致（不破坏既有 argv 锚定测试的语义基线，该测试需按新签名更新）。
- 解析在 `startRuntimeWithPortRetry` 调用 `runtimeCmdArgv` 处完成（row 在手；解析失败=持久化损坏，fail-closed 返回 internal error，MUST NOT 启动进程）。
- 备选 `OPENCODE_PERMISSION='{"*":"allow"}'` env 注入——拒绝：`--auto` 尊重用户配置中的显式 deny（更安全，语义即「全部批准但 deny 兜底」）；env 快照持久化会引入额外可变性；且 env 覆盖语义与用户项目配置的优先级不如 flag 直观。
- 兼容性风险：`--auto` 不存在于过老 opencode 版本时进程启动失败 → 激活失败落 last_error（既有失败路径），用户可感知，不做版本探测。

### D4: ai-auto 消费者——active 准入 + 统一扫描入口 + 实例绑定，永不阻塞事件流

新增 `internal/task/permit_auto.go`（ai-auto 判定器）。核心模型：**观察与判定分离**——注意力管道（SSE/REST 对账）只负责登记 pending，判定由统一入口 `judgeScan` 在准入条件满足时发起：

```
                         观察路径（只登记 pending，语义不变）
  permission.asked SSE ──► applyAttentionEvent ──┐
  align 对账写回（首次 align / 重连 align，均先于或独立于 CAS）──┤ 同一主流程的
  degraded 后台对账写回（成功 :507-515 / 非 404 失败重放 :499-506）───────┘ 观察变体
                                                         │
       触发点（同一入口 judgeScan 的调用位）：              ▼
         (a) SSE asked 应用后（activate.go:1521-1527）
         (b1) runtime 就绪提交后（激活/Recovery CAS、启动恢复
              resumeActive、挂起修复 suspending→active 三路径）
         (b2) 已 active 实例重连 align 对账完成后
         (c) degraded 后台对账完成后（成功与失败重放两种写回）
                                    Manager.judgeScan(rt)
                                                         │
              入口门禁（rt 锁内，零 DB/零阻塞）：           │
                本实例已就绪提交？  未停止？  未 unsupported？ │
              扫描 goroutine（judgeCtx，随停止 cancel）：    │
                DB 预检：任务 active？ mode=ai-auto？         │
                第二临界区复核门禁 → 登记 judged-set           │
                → 取 pending 快照中未登记 ID，登记 judged-set
                                                         │ 锁外
                                                         ▼
                          goroutine（绑定 rt + instVersion + 生命周期 ctx）:
                            Judge(ctx, perm)  ── 10s 总预算（D5）
                                                         │
                            发送前复核：仍是同实例 && 未停止 && 未 unsupported
                                                         │
                            APPROVE → ReplyPermission(once)
                            REJECT  → ReplyPermission(reject)
                            其他/错误 → 不回复（D6 失败语义）
```

- **准入条件（非阻塞入口 + 扫描期两段校验）**：judgeScan 入口 MUST 零 DB、零阻塞——触发点 (a) 位于 SSE 事件处理路径，同步 DB 读（SQLite 单连接）会阻塞事件流，且 lifecycle ctx 不随 runtime 停止取消。入口 rt.mu 临界区仅完成廉价门禁（实例已就绪提交 && 未停止 && 未标记 reply-unsupported）+ 惰性初始化 judgeCtx + 工作计数 + 捕获 instVersion，随后立即返回。**扫描工作全部在 goroutine 内（judgeCtx 随 runtime 停止先 cancel 后 join）**：先 DB 预检（任务 DB active——与 `taskOcClient` 门槛一致，attention.go:869——且 mode=ai-auto；mode 创建后不可变读取无竞态，DB 预检失效窗口由 taskOcClient active 门槛与最终发送准入兜底：最终发送准入检查发现失效时不发送，该点之后发生的停止按 D6「结果未知」收敛；DB 读在 judgeCtx 下，停止即可取消），再取 pending 快照，然后第二 rt.mu 临界区复核门禁（就绪/停止/unsupported/捕获令牌一致；实例同一性分两层——锁外先检查 Manager 注册表当前对象仍为 rt，锁内复核 instVersion==捕获值，检查与登记之间的替换窗口由后续每条请求的 permGate 拦截、fail-safe 为不回复）并同域登记 judged-set——登记 MUST 以完整准入通过为前提。**实例就绪状态**挂在 taskRuntime 上：`newRuntime` 创建时为未就绪，仅在三条就绪提交路径成功且确认当前实例后置为就绪（与 `StartDiffReviewSchedulerForTask` 同接缝）；全部触发点统一检查该状态。未就绪期（Activate/Recovery 的 activating、启动恢复重建、挂起失败修复重建）准入自然拒绝属**暂缓**（不登记 judged-set、不视为终止），重建期经首次 align/SSE 登记的 pending 由提交后 (b1) 扫描兜底。启动恢复 `resumeActive` 期间 DB 原本就是 active，但准入依据是本实例就绪状态而非 DB 状态——恢复中（env 快照恢复、watcher 恢复、写 active 仍有失败出口，reconcile.go:402/412/419）到达的 asked MUST 暂缓，否则「恢复/提交失败则不回复」无法满足。注意执行顺序事实：首次 align 的 `reconcileTaskAttention` 在各路径的 `startSSE` 内、就绪提交**之前**完成（activate.go:1393 先于 :620；reconcile.go/suspend.go 同构），因此首次扫描 MUST NOT 挂在该对账调用处，而 MUST 挂在触发点 (b1)。
- **触发点（同一主流程的局部变体，非平行流程）**：
  - (a) SSE asked 应用后（activate.go:1521-1527 接缝；准入未过自然拒绝，按准入规则暂缓或忽略）。注意力事件处理永不返回错误的既有约定不变——judgeScan 异步、任何失败仅记日志。
  - (b1) **每次 runtime 就绪提交后（三条提交路径，均与 `StartDiffReviewSchedulerForTask` 同接缝，幂等；先置实例就绪状态再调用 judgeScan）**：
    1. 首次激活/Recovery——`commitRuntimeReady`：activate.go CAS 分支的 `cas.Matched` 与同实例幂等成功分支（activate.go:624-645；该处既有注释「active 提交后才启动……taskOcClient 拒绝非 active task」即同款先例）；
    2. 启动恢复（server 重启 reconciliation）——`resumeActive`：reconcile.go:393-425，`writeStatus(active)` 提交点（:419）成功后（:424 调度器同接缝）。该路径 DB 状态原本就是 active，准入依据是「本次 runtime 已完成就绪提交」，MUST NOT 把「DB 为 active」混同于「本次 runtime 恢复已提交」；
    3. 挂起失败修复——`tryRepairRuntime` + `suspending→active` CAS（suspend.go:128）：matched 分支（:152）与幂等成功分支（:144）。
    三条路径的调用均覆盖各自 runtime 重建期间经首次 align/SSE 登记的全部 pending。MUST NOT 通过重排既有 align/CAS/提交顺序实现。
  - (b2) **已 active 实例 SSE 重连**：reconnect align 的 `reconcileTaskAttention` 完成后（activate.go:1359 调用处）调用同一入口；该路径不重新 CAS，准入以任务仍 active 为准。
  - (c) degraded 后台对账完成后（`retryAttentionDegraded` permission 分支），**覆盖两种写回变体且不以 REST 成功或 `changed=true` 为前提**：成功替换（attention.go:507-515）与非 404 失败后的缓冲重放（attention.go:499-506）。失败变体必须覆盖：后台 GET 在途时到达的 SSE asked 只入缓冲（attention.go:109-112），GET 失败重放时才写入 pending——仅在成功时触发会漏掉该请求。是否发起仍由 judgeScan 的 active/实例/停止/unsupported/judged-set 门禁决定；judged-set 保证 30s 周期只有未判过的 ID 会发起判定，不放大 LLM 调用。注意力集合与发布语义不变。
- **judged-set（per-runtime 计数契约）**：挂在 taskRuntime 上（`judgedPerms map[string]struct{}`），requestID 在扫描 goroutine 内、准入通过且调用 Judge 前，于第二 rt.mu 临界区同域登记。**同一 runtime 实例内每个 requestID 最多发起一次判定尝试**；判定失败不清除（该请求转人工，不会反复烧 LLM）。runtime 销毁即释放——新 runtime（含 server 重启后的 Recovery）可对仍为 pending 的同 ID 重新判定，**不保证跨 runtime/server 重启的全局恰好一次**。SSE 重放/对账重发现同 ID 由 judged-set 天然去重。
- **实例绑定与生命周期**：入口门禁时捕获 rt、`instVersion` 与 judgeCtx；扫描 goroutine 内 DB 预检后，judged 登记与 ready/stopped/unsupported/捕获令牌复核在第二 rt.mu 临界区同域完成（注册表同一性检查在锁外先行，替换窗口由 permGate 兜底）；LLM 调用、客户端构造、HTTP 发送全部在锁外；批内每条请求进入 Judge 前复核终止条件（停止/unsupported/实例替换/ctx 取消时，剩余条目不再进入 Judge）。**最终发送准入（发送同步点）**：`taskOcClient` 构造完成后、`ReplyPermission` 前，验证 Manager 当前 runtime 与捕获 rt 为同一对象 && `instVersion` 一致 && ready && 未停止 && 未标记 unsupported && ctx 未取消——旧实例延迟返回、客户端构造期间发生的停止/unsupported/实例替换的结果 MUST NOT 发送；该同步点之后才停止的请求按 D6「结果未知」收敛。判定/回复 goroutine 的 cancel+join 接入既有 runtime 停止路径（与 `sseCancel` 同族；停止时先 cancel judge ctx 再 join SSE；join 无条件执行、不依赖 cancel 所有权）。

### D5: 判定器——复用 ai.Completer，三值输出契约，保守语义，10s 判定总预算

- task 包定义端口接口（BranchNamer 同型，避免 task→infrastructure/ai 依赖）：

```go
// PermissionJudge 判定单条权限请求是否可通过。实现内部完成 LLM 调用与输出解析。
type PermissionJudge interface {
    // Judge 返回 approve/reject/uncertain；err 非 nil 一律不回复（转人工）。
    // 未配置与输出非法返回 uncertain + 非 nil error（供审计区分 FAILED 与真实
    // UNCERTAIN，见 D11）；合法 UNCERTAIN 返回 uncertain + nil。
    Judge(ctx context.Context, in PermissionJudgeInput) (PermissionVerdict, error)
}
```

- `PermissionJudgeInput`：**平台语境**（任务名、项目名、项目类型、任务模式、任务目录、项目目录、分支——任务行在 judgeScanAsync 已读，项目信息经 store 端口查询按次扫描复用，锁外 judgeCtx 下，MUST NOT 进同步 SSE 入口；store 端口无项目查询则新增最小只读方法）+ **请求**（`Permission`、`Patterns`、`Detail`——按类别从 metadata 确定性提取的请求详情，提取规则与界值见下；上游顶层 `always`/`tool` 字段仍不使用，见 Non-Goals）。
- **按类别提取（task 层确定性纯函数，非 LLM）**——从 `PermissionRequest.Metadata` 提取到 `RequestDetail`（本表为跨 artifact 唯一矩阵；spec/tasks 按本节引用，不复制）：

  | 权限类别 | 提取（JSON 路径 / 类型 / 必填） | 关键字段与完整性条件 |
  |---|---|---|
  | bash | `metadata.command`（string，必填） | command 非空 string；缺失/非 string/空串 → malformed_critical（缺失为 missing_critical） |
  | edit / write | `metadata.filepath`（string，必填）+ `metadata.diff`（string，必填） | filepath 非空且 diff 完整未截断 |
  | apply_patch | `metadata.files`（array，必填）：成员 `type`（string，`new`/`update`/`delete`/`move` 之一）、`relativePath`（string，必填）、`patch`（string，`move` 类型可为空，其余必填）、`movePath`（string，仅 `move` 类型必填） | files 非空、成员字段完整、未被裁剪 |
  | webfetch | `metadata.url`（string，可选；缺失时以 patterns 判定） | —（patterns 已含 URL） |
  | task | `metadata.description`（string，可选）+ `metadata.subagent_type`（string，可选；缺失以 patterns 判定） | — |
  | grep / glob | `metadata.pattern`/`path`/`include`（均 string，可选；缺失以 patterns 判定） | — |
  | read | 无（patterns 即路径） | — |
  | external_directory | shell 来源：`metadata.command`（string）；文件工具来源：`metadata.filepath`（string）+ `parentDir`/`directories`（string / []string）；两类来源按存在性选择，并存时两者都提取 | 越界目标（command 或 filepath/parentDir/directories 集合，至少其一存在且非空） |
  | 未识别类别 | 仅 permission+patterns | — |

  **界值与截断**（KB 按 1024 字节计，UTF-8 字节长度；截断按字节且不切断 UTF-8 序列，截断处追加 `…[truncated]` 标记，标记自身不计入限额）：command ≤ 2048 字节、单文件 diff/patch ≤ 4096 字节、files ≤ 20 个、Detail 全部提取字段字节总和 ≤ 8192 字节。**截断顺序与遍历序：先字段级截断，再 files 数量裁剪（裁尾部），最后总量裁剪。字段遍历顺序固定：按提取表的类别字段列序（如 edit 为 filepath → diff；apply_patch 成员按 type → relativePath → patch → movePath），files 按原数组序；总量裁剪按该顺序从末尾字符串字段逆向缩减，预算不足以保留整个字段时对该字段执行字段级截断（同样追加标记）。****任何关键证据因截断/裁剪丢失（command 截断、任一 diff/patch 截断、files 数量裁剪、总量裁剪波及关键字段）→ 置 truncated_critical**；非关键字段截断 → 截断 + 标记后照常判定。
- **确定性降级（Judge 内短路，先于配置检查）**：`RequestDetail.Degraded` 取 `missing_critical` / `malformed_critical` / `truncated_critical` 之一时，`PermJudge.Judge` 在 `Store.State()` 配置检查**之前**直接返回 `uncertain + nil`、零 LLM 调用（未配置与降级并存时降级优先——结果为 UNCERTAIN 而非 FAILED；判定流程已调用 Judge，审计计数契约不变）。**原因归一化**：metadata 有效形状仅为 JSON object——absent/null/非法 JSON/合法但非 object（由 D12 观察层统一归 nil）时，关键类别一律记 `missing_critical`；`malformed_critical` 仅用于有效 object 内的字段形状错误（类型错误、成员必填缺失、枚举外 type）；`truncated_critical` 仅用于截断/裁剪。多原因并存时按固定优先级取单值：`malformed_critical` > `missing_critical` > `truncated_critical`（全量扫描完成后统一选取，不依赖校验先后）。非强制降级类别继续以 permission+patterns 判定。
- 实现在 infrastructure/ai（`PermJudge`，SlugNamer 同文件族）：`ai.Store.State()` 单次快照判定 configured；未 configured → 返回 uncertain + 非 nil error（不调用 LLM；审计据此记 FAILED，见 D11）。prompt 要求模型在恰好一个 `<verdict>` 标签对内输出 `APPROVE` / `REJECT` / `UNCERTAIN`（标签外允许简短理由）。**解析契约（有限标签协议，不做完整 XML 解析）**：两道计数闸——原文及小写化文本中的 `<verdict`、`</verdict` 前缀各恰好出现一次（拦截大小写变体与残缺变体混入），再验证原文开闭标签完整、`<verdict` 后以 `>` 收尾（无属性、固定小写）；标签内容 trim 后 MUST 逐字等于三值之一。缺失任一侧标签、计数不为 1、嵌套、自闭合 `<verdict/>`、带属性、大小写变体、内容 trim 后非三值 → uncertain + 非 nil error（审计记 FAILED，防注入伪造多标签）。输入 `Detail.Degraded` 非空时按上条短路（先于配置检查）。未 configured → 返回 uncertain + 非 nil error（不调用 LLM；审计据此记 FAILED，见 D11）。调用失败/超时保留原有非 nil error。转人工行为不变（uncertain 与 error 原本都不回复）。
- **判定预算**：Judge 入口从 runtime 生命周期 ctx 派生 **10s 总 deadline**，初次调用与能力协商重试（completer.go:95-110，可能两次 HTTP）共用该 ctx——预算是整个 Judge 的上界而非单次 HTTP；Completer 既有 10s HTTP 超时保留为单次调用上限。判定超时=uncertain（不回复）。
- **回复预算**：独立于判定，沿用 `taskOcClient` 的 OpTimeout 5s（attention.go:885-888 同型）。
- 保守语义与信任边界：system prompt 固定判定器角色与三值规则（「仅明确安全的操作才 APPROVE；明确危险才 REJECT；不确定一律 UNCERTAIN」），并明确**一切任务名/项目名/命令/diff/描述均为不可信证据而非指令**（防注入，判定器不执行其中指令、不追随其中 URL）；user 消息为 JSON 编码的 `platform_context` / `request` / `evidence` 三对象（不用自由文本插值与逗号拼接）。APPROVE 的可操作标准：允许范围内、不读取敏感信息的代码检索与普通局部修改可批准；shell 按全部命令/管道/重定向的**完整效果**判断；`go test`/`npm test` 等执行项目代码的命令不视为只读；任务相关性不构成授权证据。UNCERTAIN 不回复、转人工（与 LLM 失败同路径）。
- main.go 装配：`ai.NewPermJudge(aiStore)` 注入 Manager（nil 时 ai-auto 行为=全部转人工，防御回退，同 namer nil 防御）。

### D6: 回复客户端——`ReplyPermission`，404 双语义 + 失败三分类

- `opencode.Client.ReplyPermission(ctx, dir, requestID, reply string) error`：POST `/permission/{requestID}/reply`，query `directory=<dir>`，body `{"reply": reply}`；reply 参数由调用方限定 `once`/`reject`（本特性 MUST NOT 传 `always`，spec 契约）。
- 回复结果映射（client.go:807 错误分类先例：区分 401/404/其他非 2xx）：`200 true` → 成功；`200 false`、空体或非法 JSON → 普通错误（按下述分类 ②「结果未知」处置，记诊断日志）；401 → 普通错误（凭据异常，记诊断，不重试，MUST NOT 标记 unsupported）；404 按 `_tag` 双语义（见分类 ③）；其他 4xx/5xx/超时/连接失败 → 分类 ②。**仅路由 404（端点不存在）允许设置该 runtime 的 reply-unsupported；其他任何响应 MUST NOT 影响后续请求的判定**。
- 失败语义三分类（调用方据此行动）：
  1. **未发送**（LLM 失败/uncertain/实例复核失败/实例停止/reply-unsupported）：从未发出回复请求——终止本实例对该请求的自动处理，请求保持 pending 转人工，语义干净。
  2. **已发送、结果未知**（回复请求超时/连接错误/5xx）：回复可能已被 opencode 受理但响应丢失——MUST NOT 重试、MUST NOT 补偿、MUST NOT 本地断言该请求已批准/已拒绝或强制保持 pending；该请求后续状态由既有 SSE/REST 对账自然收敛，若仍 pending 则照常供人工处理。
  3. **404**：解析响应体 `_tag`——`PermissionNotFoundError` → 请求已了结（人工竞态，实证），返回 `ErrPermissionRequestGone`，调用方视为正常忽略；其他 404（路由不存在=老版本 opencode 无此端点）→ `ErrCapabilityUnsupported`（沿用 attention.go:14 能力降级语义），该 runtime 标记 reply-unsupported 并停止后续判定尝试（全部转人工）。
- activating 期的准入拒绝属**暂缓**而非上述任何分类：不登记 judged-set、不终止、不视为失败，待 active 提交后由触发点 (b1) 扫描纳入。
- 回复用 `m.taskOcClient(ctx, taskID)` 一次性客户端。

### D7: 只读输出矩阵——镜像 TaskMode 透出，fail-closed

`permission_mode` 透出范围镜像 `mode` 字段（TaskMode 的全部读模型），不新增独立投影：

| 输出面 | 现状锚点 | 透出要求 |
|---|---|---|
| 创建响应 / 任务详情 | handleCreateTask 响应与任务详情 DTO | 必有 `permission_mode` |
| 项目任务摘要 | `ListProjectTaskSummaries` 组装（attention.go:899-922） | 必有 `permission_mode` |
| active 任务概览 | `ActiveTaskOverviewRow`（dto.go:100 起） | 必有 `permission_mode` |
| 内部 `TaskSnapshot` 与 lifecycle/legacy 双路径适配 | writeCreateTask 双路径 | 必须透传，不得丢字段 |

- 值域契约：输出三值枚举；存量空值输出 `ask`；未知持久化值 fail-closed 返回 internal error（同 validTaskModeForKind 哲学，DTO 输出路径不得产出坏值元素）。
- Web：`types.ts` 的 `Task`、`TaskSummary` 及其余对应响应类型新增**必填** `permission_mode: TaskPermissionMode`（Web 响应类型与 API 字段同名 snake_case，types.ts:32-40 `init_status`/`worktree_path` 先例；api.ts 直接 `JSON.parse` 透传，**不新增全局大小写转换层**）；任务详情按 `task.permission_mode` 只读展示所选模式（文案同 D8）。

### D8: Web——镜像工作空间 segmented control，重置规则与 runMode 逐字一致

- `types.ts` 加 `TaskPermissionMode = 'ask' | 'all-approve' | 'ai-auto'`；`api.createTask` 增加可选 `permissionMode` 参数（JSON `permission_mode`，缺省不传）。
- CommandCenterPage 新建面板加「权限模式」segmented control（工作空间控件同型，cc-segment-item 样式复用），缺省 `ask`。
- **重置规则与 runMode 逐字一致**（CommandCenterPage.tsx:862-870）：已选项目 ID 变更（含切换项目、清除选择、切到 dir）→ 重置为 `ask`；不改变项目 ID 的信号（同项目 apply/keep/无 payload new）保持当前选择。
- 三档文案：ask「人工批准」/ all-approve「全部批准」/ ai-auto「AI 自动识别」（proposal 定稿措辞）。

### D9: 测试策略

- `resolvePermissionMode` 表驱动（含未知值 fail-closed、空串防御）。
- argv：all-approve 含 `--auto`、ask/ai-auto 与旧 argv 逐字一致；既有 start_tui_anchor 测试按新签名更新。
- api 校验：非法值/空串/缺省三态 + 与 name/mode 校验顺序。
- store：0015 迁移后存量行读为 ask；CreateTask 显式写三值 roundtrip。
- judge：fake Completer 脚本化 APPROVE/REJECT/UNCERTAIN/乱输出/超时 → 分别断言 once/reject/不回复/不回复；能力协商重试与初次调用共享 10s 总预算（mock 两次 HTTP 合计超时 → uncertain）；fake OC client 404 两形态（PermissionNotFoundError vs 路由 404）→ 忽略 vs 标记 unsupported 停止判定；回复结果表驱动：`200 true` 成功 / `200 false` 与空体、非法 JSON → 结果未知不重试 / 401 → 普通错误且不影响后续判定 / 仅路由 404 标记 reply-unsupported；「服务端已受理但客户端读响应超时」（结果未知）→ 不重试、不本地改状态。
- 消费者准入/生命周期：activating 期到达的 asked 不发起（暂缓，不登记 judged-set）；首次提交扫描（CAS active 后 judgeScan 覆盖 activating 期登记的 pending，含 AI 立即返回的快路径恰好一次）与 active 重连扫描（reconnect align 后同一入口）分别验证；启动恢复与挂起失败修复两条路径：仅首次 REST 返回 pending、无后续 asked/重连/degraded 时，就绪提交后恰好判定一次，恢复/提交失败则不回复；resumeActive 提交前注入 asked（阻塞后续恢复步骤）+ 立即返回 APPROVE 的 fake Judge → 提交前零 Judge/零 Reply，提交成功后恰好一次判定，恢复/提交失败始终零回复；提交前观察、提交前人工了结（active 后扫描时 ID 已不在 pending → 不发起）；提交后 Judge 失败不回复且不再发起；runtime 重建后同 ID 仍 pending → 新实例重新判定一次；align 失败→后台恢复发现新 ID → 判定发起且重复快照不重发；后台 GET 在途 → asked 入缓冲 → GET 失败重放 → 该请求被判定一次（重复失败不重复判定）；旧实例延迟返回不发送；停止中的 runtime 不发送。
- 审计日志（D11）：auditlog 包单测（JSONL 行字段完整、0600——含预置 0644 文件构造后收紧且保留内容、追加写、并发写行不交错、写入失败返回错误）；消费者接入：调用 Judge 前门禁拒绝零记录；调用 Judge 后发送前门禁拒绝，保留判定 verdict、记录 `reply_result=not_applicable`，且恰好一条；四 verdict × 回复结果全分支恰好一条记录；写入失败（注入写盘错误）不影响判定与回复行为；nil logger 零写入零行为差异；真实 `ai.PermJudge`（httptest）经组合根 adapter 的未配置/非法输出/合法 UNCERTAIN/调用失败/超时 → FAILED/UNCERTAIN 映射验收（不得仅用 fake Judge 手工错误代替）。
- 判定增强（D5/D12）：类别提取表驱动（各权限类别 metadata → RequestDetail，含 20/21 文件、恰好界值/超一字节、多字节文本、总量截断用例）；确定性降级（missing_critical/malformed_critical/truncated_critical → Judge 内短路先于配置检查、零 LLM、uncertain+nil；降级×配置组合）；XML verdict 有限标签协议解析（正常/标签外理由/缺失/多标签/嵌套/自闭合/带属性/大小写变体/非法值/注入伪造文本）；观察层 metadata 双入口（SSE/REST）捕获与管道携带（upsert/REST 替换/缓冲重放/快照深拷贝）；仅 metadata 单方变化更新内部但不触发 attention_changed；toAttentionDTO 投影不含 metadata；permJudgeAdapter 全字段映射；判定语境组装含平台语境字段且不经同步 SSE 入口；审计 detail 字段三处（PermAuditRecord/adapter/auditlog.Entry）透传且所有终结分支含 detail、原因前缀不可截断。
- judged-set：同 ID SSE 重放只发起一次；ask 任务零发起。
- 输出矩阵：四个读模型透出三值、存量空值→ask、未知值 fail-closed；Web 详情按 `permission_mode` 字段读取展示（无大小写转换层）。
- 行为测试有效性证据：新增测试在旧实现下失败、新实现下通过（mutation 式验证：临时移除 `--auto` 注入/回复调用确认测试变红）。

### D10: 实施组织——单 lane 顺序实施，无既有逻辑重构

- 单 lane，顺序：**① 字段与全部读写映射（迁移/ store/ application/ api/ 输出矩阵）→ ② argv 注入 → ③ Judge/Reply 端口与 main.go 装配 → ④ 统一消费者（judgeScan + judged-set + 生命周期接入，接入激活/Recovery、启动恢复 resumeActive、挂起修复三条就绪提交路径与重连/后台对账触发点）→ ⑤ Web 表单与详情展示 → ⑥ 集成验证 → ⑦ 审计日志（PermAuditLogger 端口与 auditlog 实现、judgeAndReply 接入、装配与验证）→ ⑧ 判定增强（观察层 metadata 捕获 → 判定输入/提取/prompt/XML verdict → 审计 detail → 集成验证）**。
- 无需对既有逻辑做重构/收敛：继续复用 `writeCreateTask` 的 lifecycle/legacy 双写路径、Activate/Recovery 共用的 `startRuntimeWithPortRetry`、注意力对账两条路径；ai-auto 只新增旁路消费者，不改对账语义。
- `OCClient` 接口新增 `ReplyPermission` 方法时，既有 fake/wrapper（测试替身）必须同步实现，避免编译断裂散落。

### D11: ai-auto 审计日志——追加式 JSONL 旁路，nil 防御

- **行为契约 owner 是 spec「ai-auto 权限判定审计日志」requirement**（文件位置、JSONL、0600、单文件不滚动、字段集合、写入失败不影响行为）；本节只定义机制。
- **端口**：task 包定义窄端口 `PermAuditLogger`（`Record(PermAuditRecord) error`——写入错误必须返回给调用方；`PermAuditRecord` 字段与 spec 逐字对应：`Time/TaskID/TaskName/RequestID/Permission/Patterns/Verdict/ReplyResult/Reply/Detail`——Detail 为 string：本次判定使用的**同一份** `RequestDetail` 的摘要文本（judgeAndReply 统一生成，permAuditAdapter/auditlog 仅透传，**MUST NOT 重新读取 metadata 另生成依据**）；上限 ≤ 1024 字节（**以最终字符串 UTF-8 长度计，原因前缀与截断标记均计入**；生成时先预留前缀与标记空间，再安全截断主体）；降级原因（missing_critical/malformed_critical/truncated_critical）作为摘要前缀生成，总量截断只作用于摘要主体、MUST NOT 截掉原因前缀；无详情时为空字符串（字段必有）。所有 Judge 终结分支（含 FAILED 与发送前门禁拒绝）的记录均含 Detail。**nil 防御**：未装配 logger 时零写入、行为与无审计完全一致（与 PermissionJudge nil 防御同哲学；注入侧 MUST 保持 nil 接口，MUST NOT 注入持有 nil 指针的非 nil interface）。
- **实现**：新增 `internal/infrastructure/auditlog` 小包——`NewPermAuditLogger(path string) (*Logger, error)` 返回 mutex 串行化的追加写器：打开 `O_APPEND|O_CREATE|O_WRONLY` 0600（`MkdirAll` 父目录），**打开后、接受记录前执行 `Chmod(0600)` 收紧既有文件权限**（OpenFile 创建权限参数不修正既有文件，runner.go:60-74 先例），Chmod 失败关闭文件并返回装配错误；每条记录 marshal 为单行 JSON 后**一次 Write 调用**落盘（并发 goroutine 下行不交错）；写入失败返回错误，由 task 消费者降级为普通日志。
- **装配（main.go 组合根）**：路径 = `<LogDir>/ai-permission-audit.jsonl`（复用 `cfg.DataDir + "/logs"` 同源，main.go:156 既有 LogDir 装配先例；Manager Options 新增 `PermAuditLogger` 字段）。装配失败（如目录不可写）仅记日志、注入 nil——MUST NOT 阻断 server 启动。
- **写入时机与计数（judgeAndReply 内、锁外）**：记录计数从调用判定器起算——调用 Judge 前被准入或条间门禁拒绝的请求零记录；调用 Judge 后被发送前复核或最终发送准入拒绝的请求仍记录一条。一旦调用 Judge，其所有正常终结路径恰好一条记录：verdict 为 UNCERTAIN/FAILED 时判定终结即写（reply_result=not_applicable）；verdict 为 APPROVE/REJECT 时，回复已发送的分支按结果写（ok 含 reply 字段 / gone / unknown / unsupported），回复未实际发送的分支（实例失效、停止、ctx 取消、发送前门禁拒绝、客户端构造失败）写 reply_result=not_applicable。审计写入自身不再受 permGate 拦截（判定已完成，记录是对已发生事实的留痕）；写入失败仅 `log.Printf`，不影响任何既有分支语义。审计写入不参与 permGate/judged-set 门禁逻辑，纯旁路。
- **判定器返回语义接缝（D5 已同步）**：审计需区分 FAILED 与真实 UNCERTAIN——`ai.PermJudge` 对未配置、输出非法返回 `uncertain + 非 nil error`；task 侧审计映射：`Judge` 返回非 nil error → `verdict=FAILED`，三值 verdict 无 error → 映射为大写枚举。
- **不滚动**：单文件持续追加（量极小：每判定一行）；清理由用户手动删除文件。不做尺寸封顶/轮转（YAGNI；如未来需要另开 change）。
- **生命周期界限（已确认取舍）**：审计写入是错误处理意义上的旁路（写入失败不改变判定/回复），但**不是调度与生命周期上的完全隔离**——Record 同步持有 logger mutex 执行文件 Write，judge goroutine 的 Judge(10s)/Reply(5s) 有界，审计文件 IO 无独立取消或 deadline；极端情况下（磁盘卡住）缓慢 Write 会拖住该判定 goroutine 与 runtime 停止的 join。当前量级（每判定一行、本地磁盘）下可接受；若未来要求停止时间不受审计影响，另开 change 设计有界队列 + 独立写入 worker，MUST NOT 简单地每条记录启动一个 goroutine。

### D12: 观察层捕获请求详情——metadata 透传，DTO 投影不变

- `opencode.PermissionRequest` 增 `Metadata json.RawMessage`（原始 metadata 整体保留；`always`/`tool` 仍不持久化）。上游契约（v1 schema：id/sessionID/permission/patterns/metadata/always + 可选 tool）由 scripts/check-opencode-contract.sh 钉住；缺失/畸形 metadata 按 nil 处理，MUST NOT 丢弃请求。
- attention 管道携带：upsert、REST 替换、缓冲重放、快照深拷贝均携带 Metadata（RawMessage 拷贝）。**等值判断修正**（attention.go upsertPermLocked 现状为 sessionID+permission+patterns 同值 no-op）：SessionID/Permission/Patterns 的原有等值条件不变；**仅 metadata 单方变化**时更新内部存储但**不视为外部可见变化**（不触发 attention_changed 发布——前端投影不含 metadata，避免无意义通知）。
- SSE/REST 双入口捕获：SSE 的 `AttentionEvent` 与 `parsePermissionAsked`、REST 的 `parsePermissionRequest` 均需捕获/归一化 Metadata（**缺失、null、非法 JSON、合法但非 object 的 JSON 一律归 nil**，MUST NOT 丢弃请求；归 nil 后提取阶段对关键类别记 missing_critical——malformed_critical 仅用于有效 object 内的字段形状错误）；只扩展 `PermissionRequest` 会让 SSE 路径静默丢 metadata。
- API `toAttentionDTO` 投影（id/permission/patterns/since）与前端类型**零改动**——metadata 仅供内部判定消费，MUST NOT 泄入对外 DTO（隐私面收敛）。
- 判定消费侧语境组装在 judgeScanAsync 的既有异步 DB 路径上（任务行已读；项目信息一次查询按次扫描复用），锁外、judgeCtx 下；MUST NOT 把新增查询放进同步 SSE 入口（D4 非阻塞入口不变）。

## Risks / Trade-offs

- [ai-auto 误判批准危险操作] → 用户显式选择该模式；D5 保守 prompt + UNCERTAIN 转人工；`once` 不扩散授权面；请求当前状态由 SSE/REST 对账收敛（pending 为内存模型）；ai-auto 的判定与回复结果由 D11 审计日志留痕（追加式 JSONL 文件，非结构化查询存储，不滚动由用户管理）。
- [SSE 断连期间产生的请求漏判] → D4 触发点 (b)(c) 覆盖重连 align 与 degraded 恢复；残余遗漏退化为 ask 体验（安全方向）。
- [`--auto` 在过老 opencode 上不存在] → 激活失败落 last_error，可感知可重试；不做版本探测（D3）。
- [判定延迟期间权限请求挂起] → 判定总预算 10s（D5，含能力协商），回复独立 5s；opencode 原生即等待回复，人工可随时在终端抢先回复（404 竞态已实证干净）。
- [judge goroutine 泄漏] → ctx 随 runtime 生命周期取消并 join（D4）；单次判定 10s 有界；judged-set 防重入。
- [回复结果未知（已受理但响应丢失）] → D6 分类 2：不重试不补偿，由 SSE/REST 对账收敛，无双倍批准风险（同请求重复 reply 返回 404 已实证）。

## Migration Plan

1. 迁移 0015 加列（DEFAULT 'ask' 覆盖存量，零回填、零停服）。
2. 代码部署后：既有任务全部表现为 ask（行为不变）；新任务按选择生效。
3. 回滚：代码回退后新列残留无害（无人读取）；如需清理可后续迁移 DROP COLUMN。

## Open Questions

（无。）
