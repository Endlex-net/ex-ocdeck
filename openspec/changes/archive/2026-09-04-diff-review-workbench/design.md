# Design: diff-review-workbench

## Context

git diff 页面（`web/src/components/GitPanel.tsx:36`）现为只读：`internal/api/git.go:17` 注册 status/diff/commit/push 路由，`internal/task/gitops.go` 提供 Manager facade（`tryLockTask` 持任务锁），diff 内容经 `gitops.go:93` 六阶段产出八字段 DTO，前端 `DiffViewer.tsx:90-116` 用 CodeMirror MergeView（并排）/ unifiedMergeView（单列）只读渲染（`readOnlyExtensions`，`DiffViewer.tsx:5,85`）。注意：`DiffViewer.tsx:109-110` 已注明字符串形态的 CM 文档会被规范化为 `\n`（吞掉 CRLF）——**批注快照与编辑写回均不得取自 CM 文档态，必须基于原始侧内容**。

当前无向 agent 会话发消息的通道：`OCClient` 接口（`internal/task/types.go:79-93`）仅含 session CRUD/status/events；运行中任务经 `Manager.taskOcClient`（`internal/task/attention.go:854-873`）按存量 port/password 构造一次性客户端；任务锚定会话为 `TaskRow.AnchorSessionID`（`internal/task/activate.go:1575-1576`）。opencode 消息发送契约（已对 v1.18.18 源码核实）：`POST /session/{sessionID}/prompt_async`，body 必填 `parts:[{type:"text",text}]`，接受返回 204；`messageID` 必须满足 schema `isStartsWith("msg")`；**messageID 不是幂等键**，`prompt_async` 接受后立即异步执行、无去重检查——至多一次投递必须由本系统自行保证。OpenAPI 文档端点 `GET /doc` 存在（含认证中间件），路径键为 `/session/{sessionID}/prompt_async`、operationId `session.prompt_async`。契约治理见 `internal/infrastructure/opencode/CONTRACT.md`（已验证区间 [1.18.14, 1.18.18]，锚点逐版本核验 + live probe）。

持久化为 greenfield：SQLite migrations 现至 0011（`CREATE TABLE IF NOT EXISTS` + `task_id REFERENCES tasks(id) ON DELETE CASCADE`，见 `0011_recovery_debts.sql`）。注意 store 层 `nowUnix` 为秒精度（`queries.go:117`）且 `updated_at` 仅跨秒推进（同秒实变不推进，`queries.go:470` 注释）——**版本比对不得使用 updated_at**。HTTP 层公共 `decodeJSON`（`internal/api/projects.go:432-440`）无 body 上限，`http.MaxBytesReader` 仅在个别 handler 单用（`lifecycle_config.go:124`、`ai_config.go:70`）——新端点必须显式加装。

## Goals / Non-Goals

**Goals:**
- diff 页查看模式下框选/点击创建批注，持久化、跨轮累积、active/stale 漂移标记。
- 批注整体提交给当前 task 的 agent 会话（`prompt_async` 通道，至多一次投递），支持预览、补充说明、排队、排队中撤回、重启恢复。
- 只读可删除的提交历史（仅 sent）与独立失败分区。
- diff 页编辑模式直接修改工作区文件：实时写回、UTF-8/换行保真、冲突拒绝、编辑会话内还原。

**Non-Goals:**
- 不产生真实 git commit 语义变化；不经平台 LLM 直连；不做 IDE 级编辑。
- 不做 AI 处理状态徽标/完成通知/静默刷新保护/验收跳转/误删撤回/新手引导；不提供失败提交的自动重发入口（失败保留活动批注，用户可重新提交）。

## Decisions

### D1: 提交通道 —— OCClient 新增 PromptAsync

**签名与类型归属（三处钉死，唯一）**：

```go
// internal/infrastructure/opencode（transport DTO，opencode 包保持仅依赖标准库）
type PromptResultKind string
const (
    ResultAccepted         PromptResultKind = "accepted"           // 204
    ResultHTTPResponse     PromptResultKind = "http_response"      // 任何已收到且 status != 204 的响应
    ResultTransportUnknown PromptResultKind = "transport_unknown"  // 请求已发出、结果未知
    ResultPreSendFailure   PromptResultKind = "pre_send_failure"   // httpClient.Do 之前的本地失败
)
type PromptResult struct {
    Kind       PromptResultKind
    StatusCode int    // accepted=204；http_response=实际状态码；其余=0
    Body       string // 仅 http_response 非空（有界截断）；其余为空
    Detail     string // 仅 transport_unknown/pre_send_failure 非空（底层错误文本）
}
func (c *Client) PromptAsync(ctx context.Context, dir, sessionID, messageID, text string, files []PromptFilePart) PromptResult

// internal/task OCClient 接口同形透出 transport DTO（与 *Client 方法集完全一致——
// factory 直接返回 *opencode.Client（manager.go:448），签名 MUST 逐字一致；编译期接口断言 + 全部 mock/fake 同步）
OCClient.PromptAsync(ctx, dir, sessionID, messageID, text string, files []opencode.PromptFilePart) opencode.PromptResult

// internal/application/diffreview（domain 类型，consumer-owned）
type PromptOutcomeKind string // accepted|http_response|transport_unknown|pre_send_failure 同形常量集
type PromptOutcome struct { Kind PromptOutcomeKind; StatusCode int; Body string; Detail string }
PromptPort.PromptAsync(ctx, taskID, sessionID, messageID, text string, files []string) PromptOutcome
// taskID 为路由上下文：adapter 经 Manager.taskOcClient(taskID) 获取当前 client+directory
// （attention.go:854），保证多任务投递路由隔离；files 为批注涉及的 worktree 相对路径（见下方 file parts 条）
```

task 层 adapter 显式逐字段映射 `opencode.PromptResult` → `diffreview.PromptOutcome`（MUST NOT 类型别名跨层共享）。实现于 `prompt_async.go`，传输形态镜像 `CreateSession`：`POST /session/{id}/prompt_async?directory=<dir>`，basic auth，body `{"messageID": <messageID>, "parts": [{"type": "text", "text": <text>}, ...file parts]}`。**分类规则（唯一）**：marshal/NewRequest 失败 → pre_send_failure（Detail=错误文本）；`httpClient.Do` 返回错误（dial/超时/断连/ctx 取消）→ transport_unknown（Detail=错误文本）——MUST NOT 尝试区分"是否已发出"，一律按已发出处理；收到响应：204 → accepted，其余一切状态码（含意外 2xx 如 200/201/202）→ http_response。**adapter 获取失败（唯一规则）**：`taskOcClient(taskID)` 在任何 HTTP 调用前返回 ok=false（任务非 active、凭据/端口缺失等）→ PromptPort MUST 返回 `pre_send_failure`（Detail 固定非空文案 "runtime client unavailable"）→ 进入 D2 准备重试（≤3 次，耗尽 failed），MUST NOT 标 delivery_unknown。**意外 2xx 的状态映射：delivery_unknown**（请求可能已被新版本服务接受，MUST NOT 自动重投）**且能力缓存置 unknown 触发复探**；400/401/404/其他非 2xx 按下方错误矩阵。**DB 读行/组装失败由调度器在调用 PromptPort 前处理，不属于 PromptOutcome**；adapter 侧的 client 获取失败按上条 adapter 规则处理。mock/fake MUST 按该结构化分类仿真，MUST NOT 依赖错误字符串匹配。状态映射汇总：accepted → sent 事务；http_response 非 2xx → 错误矩阵；http_response 意外 2xx → delivery_unknown + 能力 unknown；transport_unknown → delivery_unknown；pre_send_failure → 准备重试（≤3 次，耗尽 failed）。
- **messageID 契约**：`messageID = "msg_" + <submission UUID 去连字符小写>`（满足 `isStartsWith("msg")` 且与 submission 一一对应，杜绝外部消息主键碰撞）；每次 submission 生成一次并持久化，准备重试 MUST 复用同一值，MUST NOT 重新生成。与本地 submission id 为独立字段（D3），仅用于外部追踪/对账，MUST NOT 作为幂等依据。
- **file parts（批注 7：引用 worktree 文件）**：`files` 以 file part 附在 text part 之后。**file part 契约（唯一）**：`{"type": "file", "mime": "text/plain", "filename": <basename>, "url": "file://<逐段 PathEscape 的绝对路径>"}`（三斜杠；服务端经 fileURLToPath 解码为本地绝对路径读取，read 工具注入 agent 上下文）。opencode 层 `PromptFilePart{URL, Mime, Filename}` 单 struct + omitempty，parts = text part + 逐 file part，files 为空时仅 text part（与原契约一致）。**构造与校验（唯一）**：task 层 adapter 的 `files` 为 worktree 相对路径，发送前逐一 `os.Stat` 校验存在且为 regular 文件，缺失/非 regular 逐个跳过（不阻断发送——批注快照已在 payload 中，跳过仅避免 agent 收到读取失败文本）；application 调度器发送前经 `ListDiffReviewSubmissionItems` 收集 items 的 path（去重、保持 items 顺序），items 读取失败 → `sending→failed`（error 固定 "read submission items failed"）且 MUST NOT 调用 PromptAsync。**至多一次投递语义不变**：file parts 的构造/跳过不产生独立失败通道、不触发重发。
- 目标会话 = `AnchorSessionID`，提交确认时冻结为 `target_session_id`；客户端经 `taskOcClient` 获取。
- **错误矩阵（唯一）**：
  - 204 → accepted（进入 sent 事务）。
  - 400 → 契约/客户端不兼容 → failed（error 记录响应体）并触发能力复核。
  - 401 → 内部凭据 bug → failed，MUST NOT 重试。
  - 404 → MUST 分流：`GetSession` 复查目标会话，穷尽规则——**有效解码的 200（存在）→ 端点不支持**：能力状态转 unsupported、提交能力禁用，**当前 submission 执行 `sending→failed`（error 固定为 "capability unsupported: prompt_async"），MUST NOT 重投**；**明确 404（不存在）→ failed（invalid_state）**；**其余一切结果（其他状态码、传输错误、body 解码失败）→ 本 submission failed（error 记录）且能力状态转 unknown**。POST 已明确返回 404 即请求未执行，MUST NOT 标 delivery_unknown、MUST NOT 自动重投。
  - 网络错误/超时/连接中断（HTTP 已发出、结果未知）→ **delivery_unknown**，MUST NOT 自动重投。
  - 其他非 2xx → failed，error 记录状态码与响应体。
  - 任何分支 MUST NOT 改走平台 LLM 或终端 PTY 注入。
- **能力探测（唯一实现）**：对该任务 serve 请求 `GET /doc`，结构化解析 OpenAPI JSON，判定 `paths["/session/{sessionID}/prompt_async"].post` 存在且 operationId 为 `session.prompt_async`（operationId 不匹配 → fail-closed 为 unsupported）。结果语义：200 且路径存在 → `supported`；200 且路径缺失或 operationId 不符 → `unsupported`；401 → `unknown`（凭据/中间件问题，可重试）；404/5xx/网络错误/畸形 JSON → `unknown`。能力状态三值 `supported | unsupported | unknown`，API 同形输出；`unknown` 时 MUST 拒绝创建 submission（invalid_state），探测可重试。
- **探测事件模型（唯一时序）**：① 首探：任务 runtime ready（激活完成）时 eager 执行一次；② 复探：GET /annotations 与提交准入遇到 absent/unknown 时同步复探，并发请求经 singleflight 合并为一次探测；③ 恢复门禁：重启恢复 queued 队列后，调度器发送前 MUST 先获得 supported；④ 运行时复核：PromptAsync 收到 400 或意外 2xx（http_response 且状态码为 2xx）时先将缓存置 `unknown` 再触发复探；⑤ 缓存绑定任务运行时实例（instVersion），Suspend/重启/实例替换即失效。
- **CONTRACT.md 同步**：实现时必须将 `prompt_async` handler、PromptInput/MessageID schema、`GET /doc` 锚点加入 `CONTRACT.md` 契约锚点清单，并按其升级 SOP 做锚点 diff + live probe（本机 1.18.25 已超出已验证区间 [1.18.14, 1.18.18]，区间扩展核验是本 change 的组成部分）。
- **类型归属（唯一）**：低层 `client.PromptAsync` 返回 transport DTO `opencode.PromptResult`（字段形状与上述 PromptOutcome 相同，opencode 包保持仅依赖标准库，`client.go:9`）；domain 类型 `PromptOutcome`/`PromptOutcomeKind` 归 `internal/application/diffreview` 所有（PromptPort 的返回类型）；task 层 adapter 做一一映射（MUST 显式逐字段转换，MUST NOT 类型别名跨层共享）。

### D2: 提交队列 —— 先落库、CAS 状态机、至多一次投递

- 状态机（唯一）：状态边为 `queued → sending → sent | failed | delivery_unknown`，外加唯一一条 `queued → failed`（仅能力门禁产生，见下调度器门禁条）；不存在回边；任何终态 MUST NOT 自动重投；仅 `queued` 可撤回（撤回=同事务 DELETE 行，不保留 cancelled 记录）。**sending 一旦发出 HTTP 请求，超时/断连/进程重启一律转 delivery_unknown**（用户决策 DR1：不接受重复投递）。

- **提交准入（全部满足才落库，否则 invalid_state/invalid_input/conflict，零副作用）**：任务运行中（runtime 存在）；`AnchorSessionID` 非空；能力探测 = `supported`；`annotations` 非空、id 不重复、revision 均为合法整数。**批次级 id 校验优先级（唯一，与数组顺序无关）**：先完成全部 id 分类——**任一 id 存在但属于其他任务 → invalid_input**；否则**任一 id 在本任务范围内不存在或 revision 不符 → conflict(409)**；全部通过后才允许 diff 读取/组装。能力探测与核心内容未超阈值（D7）。
- **落库**：单事务写 submissions 行（status=queued）+ submission_items 快照。FIFO 依据为表级 `INTEGER PRIMARY KEY AUTOINCREMENT` 的入队序号 `seq`（不复用、单调）；`id` 为 TEXT UNIQUE 的 UUID。提交组装全程持任务锁，并在入库事务内按 `id+revision` 复核**请求携带的版本**（与准入同一判定）；**复核失败 → 统一返回 conflict(409)，零落库、零清理、零调度**；前端保留预览弹层、刷新批注列表后要求用户重新确认。
- **调度器**：Manager 内每任务一个调度循环，随任务运行时启动、`Shutdown` join。队列非空时以 2s 间隔轮询 `SessionStatus`：目标会话 type=idle **或不在返回 map 中** → 视为可投递；busy/retry → 等待；状态查询失败 → 保持 queued 下轮重试（查询无发送副作用）。可投递时 `UPDATE ... SET status='sending' WHERE id=? AND status='queued'` 条件抢占（CAS），失败即让出。
- **能力门禁（每次投递前，唯一规则）**：每次 CAS 前 MUST 检查当前 instVersion 的能力缓存——`supported` → 继续；缓存已为 `unsupported` → 直接 `queued→failed`（error="capability unsupported"），MUST NOT 复探、MUST NOT 进入 sending、MUST NOT 调用 PromptAsync；`unknown`/absent → 先 singleflight 复探：复探后 `supported` → 继续，`unsupported` → 同上转 failed，仍 `unknown` → 保持 queued 下轮再试。
- **发送**：发起 HTTP 前的准备失败（读行、构造请求）可重试至多 3 次，耗尽 → failed；HTTP 发出后结果按 D1 错误矩阵转移；**204 后 sent 本地事务失败 → 转 delivery_unknown**（agent 已收，绝不重发）。
- **重启恢复（两段式，唯一）**：① **服务启动收敛**（独立于任何 runtime）：server 启动、API 与调度器开放前，全局单事务将所有 `sending` 行转 `delivery_unknown` 并写入固定非空 error（"delivery unknown after restart"）；收敛写库失败 MUST fail-closed（服务不开放）。② 任务 runtime 每次启动仅扫描本任务 `queued` 重新入队。runtime 无法 ready 不影响①——sending 不会无限停留。
- 调度与 Suspend/Delete 等生命周期操作经任务锁互斥（`tryLockTask` 约定）。

### D3: 持久化 schema（migration 0012_diff_annotations.sql）

```sql
CREATE TABLE IF NOT EXISTS diff_annotations (
    id                  TEXT PRIMARY KEY,          -- UUID
    task_id             TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    path                TEXT NOT NULL,
    side                TEXT NOT NULL,             -- 'old' | 'new'
    ref                 TEXT NOT NULL DEFAULT '',  -- 创建时 diff 来源 ref（空=index/untracked）
    untracked           INTEGER NOT NULL DEFAULT 0,
    start_line          INTEGER NOT NULL,          -- 1-based 闭区间
    end_line            INTEGER NOT NULL,
    snapshot_start_line INTEGER NOT NULL,          -- 快照窗口首行行号（含上下文）
    snapshot            TEXT NOT NULL,             -- 完整窗口文本（含行尾字符，见 D4）
    snapshot_line_count INTEGER NOT NULL,          -- 窗口行数
    comment             TEXT NOT NULL,
    revision            INTEGER NOT NULL DEFAULT 1, -- 每次实变严格 +1（版本比对唯一依据）
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_diff_annotations_task ON diff_annotations(task_id);

CREATE TABLE IF NOT EXISTS diff_review_submissions (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,  -- 全局入队序（FIFO 唯一依据，不复用）
    id                TEXT NOT NULL UNIQUE,        -- UUID
    task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    status            TEXT NOT NULL,               -- queued|sending|sent|failed|delivery_unknown
    target_session_id TEXT NOT NULL,
    message_id        TEXT NOT NULL UNIQUE,        -- msg_<submission UUID 去连字符>，见 D1
    note              TEXT NOT NULL DEFAULT '',
    payload           TEXT NOT NULL,
    truncated         INTEGER NOT NULL DEFAULT 0,
    error             TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,            -- 排队时间
    sent_at           INTEGER                      -- sent 事务本地提交时间
);
CREATE INDEX IF NOT EXISTS idx_diff_review_submissions_task ON diff_review_submissions(task_id);

CREATE TABLE IF NOT EXISTS diff_review_submission_items (
    submission_id      TEXT NOT NULL REFERENCES diff_review_submissions(id) ON DELETE CASCADE,
    annotation_id      TEXT NOT NULL,
    annotation_revision INTEGER NOT NULL,          -- 快照时批注 revision（sent 清理比对用）
    path               TEXT NOT NULL,
    side               TEXT NOT NULL,
    ref                TEXT NOT NULL DEFAULT '',
    untracked          INTEGER NOT NULL DEFAULT 0,
    start_line         INTEGER NOT NULL,
    end_line           INTEGER NOT NULL,
    snapshot_start_line INTEGER NOT NULL,
    snapshot           TEXT NOT NULL,
    comment            TEXT NOT NULL,
    PRIMARY KEY (submission_id, annotation_id)
);
```

- **不可变提交快照**：payload 由 submission_items 快照组装，排队期间用户继续编辑/删除活动批注不影响已提交内容。
- **sent 清理事务（唯一路径）**：`sending→sent` 与 `DELETE FROM diff_annotations WHERE id=? AND revision=?` 在同一 SQLite 事务完成——排队期间被编辑的批注（revision 已 +1）保留，未变的本次提交批注删除。取消/失败/delivery_unknown MUST NOT 删除任何活动批注。**版本比对只用 revision**（秒级 updated_at 在同秒实变下不推进，见 Context）。
- 预览中临时移除的条目不进入 submission_items/payload，自然保留为活动批注。
- 历史 = status=sent 的行；失败记录（failed/delivery_unknown）独立分区展示、可删除；撤回不留下任何记录（行已 DELETE）。

### D4: 快照与 stale 判定 —— 原始侧内容构造、同源全窗口比对

- **来源组合约束（继承 canonical `git-operations/spec.md:35`，任何读取/落库前校验，非法 → invalid_input）**：`side ∈ {"old","new"}`；`untracked=true` 时必须 `ref="" 且 side="new"`；`untracked=false` 时 side 任意、ref 可空（空=index）。
- **快照构造（前端，唯一来源）**：snapshot MUST 从原始 `GitDiffResult` 的对应侧内容（`oldContent`/`newContent` 字符串）按行号切取构造，**保留行尾 `\r`**（CM `Text.of(split('\n'))` 的同一形态）；MUST NOT 取自 CM `state.doc`（会吞掉 CRLF）。窗口 = 选中段前后各 3 行（文件边界裁短）；`snapshot_line_count` = 窗口行数，入库时校验 `start_line/end_line` 落在窗口内且窗口 1-based 关系自洽。
- **unified（单列）形态 side 映射（唯一）**：新增行与未变上下文行 → `new`；删除行 → `old`；MUST NOT 允许跨侧选区。并排形态按指针所在编辑器侧判定。
- **stale 判定（后端，列表读取时惰性计算，唯一规则）**：按批注来源元组（`ref`/`untracked`/`side`）经 `content.go` 既有读取路径重读该侧有界内容；取 `[snapshot_start_line, snapshot_start_line + snapshot_line_count)` 行窗口与 `snapshot` 全文比对，不相等 → stale=true。**截断文件的窗口完整落在返回前缀内时正常比对（保持 active）；仅当窗口无法完整取得（截断点在窗口内、文件删除、不可读）时 stale=true**。任何读取失败 MUST NOT 报错阻断列表。stale 仅计算返回，不落库。

### D5: 编辑写回 —— 判别联合读取契约 + 固定顺序原子写

**编辑读取**：`GET /api/v1/tasks/{id}/git/file?path=` → 判别联合：

- 可编辑：`{editable: true, content, baseHash, lineEnding, hasBom, mode}`。`content` 为**去除 UTF-8 BOM、CRLF 归一为 `\n`** 后的文本；`lineEnding ∈ "lf"|"crlf"`（无换行文件固定 `"lf"`）；`hasBom` = 原始字节以 UTF-8 BOM 开头；`mode` = 当前文件**完整 chmod 值**的四位八进制字符串（`0000..7777`，含 setuid/setgid/sticky 特殊位；与 Go 的转换为 `Perm() | ModeSetuid/ModeSetgid/ModeSticky` 双向映射）；`baseHash` = 当前文件**精确字节**（含 BOM、原始换行）的 SHA-256 小写 hex。
- 不可编辑：`{editable: false, reasonCode, reason}`。`reasonCode` 枚举唯一：`binary | non_utf8 | mixed_line_endings | too_large | not_regular | missing | read_only`（`read_only` = 文件无 owner 写位（mode & 0200 == 0）——用户决策：只读文件禁止编辑）。

判定顺序（唯一）：词法/路径防御（`git.ValidateDiffPath`）→ **tryLockTask（与生命周期操作互斥，冲突 → conflict 409，与 GitDiff 同一约定 `gitops.go:108`）** → task/worktree/repo 校验 → Lstat + worktree 禁锢（`content.go` 同一规则）→ regular file（否则 not_regular/missing）→ NUL 二进制嗅探（前 8000 字节含 NUL → binary，与 status/diff 同一口径）→ ≤512KiB（524288 bytes，否则 too_large）→ 有效 UTF-8（否则 non_utf8）→ 换行风格统一 LF 或 CRLF（CR-only 或混合 → mixed_line_endings）→ **无 owner 写位 → read_only**。

**写回**：`POST /api/v1/tasks/{id}/git/file`，body `{path, content, baseHash, lineEnding, baseMode}`。固定顺序，任何前置失败 MUST NOT 创建临时文件、修改目标文件或 git index：

1. wire 限制（D8 表，`decodeBoundedJSON`）+ JSON 解析 + 词法校验（path；`baseHash` MUST 为 64 位小写 hex，否则 invalid_input；**`content` MUST 仅以 `\n` 为换行字符，含任何 `\r` → invalid_input、零写盘**；**`lineEnding` MUST ∈ `"lf"|"crlf"`，否则 invalid_input**；**`baseMode` MUST 为四位八进制字符串（`0000..7777` 完整 chmod 值），否则 invalid_input**；**`baseMode` 无 owner 写位（& 0200 == 0）→ invalid_input（只读文件禁止编辑）**）
2. **tryLockTask（与生命周期操作互斥，冲突 → conflict 409）→ task 存在 → worktree 非空（空 → invalid_state）→ repo kind 校验**（与 GET 编辑读取、GitDiff 同一锁序，`gitops.go:108-122`）
3. **禁锢复检**：Lstat + resolved parent/target 禁锢 + regular file 校验（与 diff 新侧读取同一规则，`content.go`；逃逸 → invalid_input，且 MUST 早于临时文件创建）
4. 重读当前文件精确字节：文件消失或类型变为非 regular → **invalid_state**（提示刷新）；重算 SHA-256 与 `baseHash` 比对，不一致 → **409 conflict，零写盘**；**当前文件含换行且其风格与请求 `lineEnding` 不一致 → 409 conflict**（外部改动将换行风格改掉，视为冲突）；**当前 mode（完整 chmod 值，含特殊位）与请求 `baseMode` 不一致 → 409 conflict**（chmod 类外部改动不改变字节 hash，必须独立校验）；全部通过则以请求 `baseMode` 为本次终检基线
5. 从当前精确字节推导 BOM 有无；将 `content`（`\n` 分隔）按**请求携带的 `lineEnding`** 重建字节（crlf → `\n`→`\r\n`）、恢复原 BOM；**末尾换行状态以 `content` 表达为准**（用户决策：末尾换行属可编辑内容，IDE 惯例；未触碰时自然保持原值）；重建结果 >512KiB → invalid_input，MUST NOT 截断写入
6. 同目录临时文件写入 → **mode 设为请求 `baseMode`（含特殊位，经 `Perm() | ModeSetuid/ModeSetgid/ModeSticky` 映射后 Chmod）** → flush
7. **rename 前终检（乐观并发）**：临时文件 flush 后、rename 前再次执行禁锢 + regular file + 目标精确字节 SHA-256 + **当前 mode 与基线（`baseMode`）比对**（与步骤 3/4 同一规则）；任一不匹配 → 删除临时文件并返回 409 conflict。**终检与 rename 之间仍存在极小残余 TOCTOU 窗口，为探索阶段已接受的残留风险**（绝对保证需 agent 配合锁协议，超出本 change 范围）；rename 前任何失败清理临时文件
8. 原子 rename
9. 响应 `{baseHash: <新精确字节 hash>}`

错误映射（唯一）：词法/格式/大小 → invalid_input；**初检**时文件消失/类型变化 → invalid_state；**终检**时文件缺失、类型、禁锢、hash 或 mode 相对基线变化 → conflict(409)；初检 hash 不一致、**换行风格与冻结值不一致**、**mode 与 baseMode 不一致**或任务锁冲突 → conflict(409)；检查自身或写入 IO 失败 → internal。

**前端写协议**：`lineEnding`、`hasBom`、`mode` 在编辑会话内冻结为首次编辑读取 GET 的返回值；期间所有写回（含恢复确认后的补发与还原请求）MUST 携带冻结的 `lineEnding` 与 `baseMode=<冻结 mode>`。每文件同时只允许一个写请求在途；**每个写请求冻结 `sentContent`（实际发送的文档文本）**；在途期间的后续编辑合并为最新文档、携带最近一次响应的 baseHash 再发；切换文件、退出编辑模式、还原前 MUST flush 并等待在途写完成。**409 → 保留编辑器内容、暂停自动写回、显示冲突提示**；**网络/internal 等结果未知的失败 → 恢复确认流程：重新 GET 编辑读取，逐项比对——`content` 与该请求的 `sentContent` 相等、`hasBom` 与冻结值相等、（`sentContent` 含 `\n` 时）`lineEnding` 与冻结值相等、**`mode` 与首次编辑读取值相等** → 全部相等才采用其 baseHash 视为已确认（rename 可能已成功而响应丢失），此时若编辑器已有更新内容，立即以新基线发送最新文档；任一项不一致 → 保留最新编辑器内容、保持阻塞冲突态**。任何失败场景 MUST NOT 静默刷新或丢弃用户内容；未解决前阻止切换文件，直至用户重试成功或显式放弃本地改动。

**还原**：进入编辑会话时前端内存持有该文件快照；「还原」= 用快照走同一写回端点（含 baseHash 校验，需用户确认），天然继承冲突保护。快照仅存前端内存，编辑会话结束即弃——还原仅在当前编辑会话内有效，入口文案注明作用范围。

### D6: busy 时编辑宽严 —— 允许进入 + 醒目警告

agent 会话 busy 时允许进入编辑模式，编辑区顶部显示醒目警告横幅（agent 正在修改代码，保存可能冲突被拒）。最坏后果被 D5 base 校验 + rename 前终检限制为「拒绝+提示」；终检与 rename 之间的极小残余 TOCTOU 窗口为探索阶段已接受的残留风险（DR-R1 定案）。已写入 spec 为规范性契约（proposal 留设计阶段定案项，此为定案）。

### D7: prompt 组装与体积保护

提交确认时一次性组装并整体存入 `payload`（不可变；组装不读取 diff，批次 id/revision 复核在落库事务内完成，见 D2）。

**逐字模板（唯一）**：payload = 段序列以字面 `\n\n` 连接；段内构造固定，字段逐字插入 MUST NOT trim（字段自带的首尾换行原样保留为段内字节）。`strconv.Quote` 记为 `Q()`。

**逐字公式（唯一拼接规则）**：

```text
fixedHeader        = "以下是代码 review 批注，请在当前 worktree 中逐条修复。修复前先阅读相关代码，保持最小改动。"
noteSection        = note == "" ? ∅（不参与段序列） : "## 补充说明" + "\n" + note
annotationBlock(i) = "### 批注 " + i + " — " + Q(path) + ":" + range + " (" + side + "，来源 " + source + ")"
                   + "\n" + "评论：" + comment
                   + "\n" + fence + "\n" + snapshot + "\n" + fence
annotationSection  = "## 批注" + "\n\n" + Join(annotationBlocks, "\n\n")
core               = Join([fixedHeader, noteSection?, annotationSection], "\n\n")
```

- `source`：`untracked=true` → `"untracked"`；`ref` 非空 → `Q(ref)`；否则 `"index"`。`range`：单行 `<start>`，多行 `<start>-<end>`。`fence` = 动态反引号 fence（规则见下）。
- 批注排序键唯一：`created_at` 升序，平局按 `id` 字典序；`<i>` 从 1 连续编号。
- golden tests 必须包含完整期望字节样例：至少两条批注、comment/snapshot 分别以 `\n` 与 `\n\n` 结尾的组合。
- **不附加相关 diff/Context 段（用户决策）**：批注快照窗口自带上下文即足够，payload 组装不读取、不拼接任何 diff 上下文段；批注涉及的文件改以 file part 随 prompt_async 附在 text part 后（契约见 D1 file parts 条），文件缺失/非 regular 由 adapter 跳过，不影响提交与投递语义。

**fence 规则（唯一）**：动态反引号 fence，长度 = 被包围内容中最长反引号串 + 1（最小 3）；MUST NOT 用固定三反引号包围任意源码。

**体积（唯一准入）**：按 UTF-8 字节计量，阈值 65536 字节；`len(core) > 65536 → invalid_input`（零副作用，见 D2 准入——不创建 submission、不清批注、不调度）。无相关 diff 段即无预算截断、无截断标记：`Submission.truncated` 恒 false（DTO 字段保留 wire 兼容）。`Submission.error`：queued/sending/sent 必为空串；failed/delivery_unknown 必非空。原 payload 组装 benchmark（payload_bench）已随相关 diff 段一并删除，不设任何 benchmark 门禁。

**GitDiff 重构前置**：将 `Manager.GitDiff`（`gitops.go:93`）拆为「公共加锁入口 + 已持锁核心 helper」，六阶段顺序与八字段不变量原样保留；已持锁消费方（如 DiffSourcePort.ReadLocked）MUST 只调核心 helper（禁止递归加锁）。核心 helper 内新增**唯一 UTF-8 规范化步骤，顺序钉死**：raw bytes → NUL 嗅探 → `strings.ToValidUTF8(raw, "\uFFFD")` → 规范化结果按 UTF-8 rune 边界限制至 524288 bytes；`truncated=true` iff 原始读取超限**或**规范化结果因上限被裁短（替换扩张导致的裁短同样置位）。API、快照构造、stale 比对 MUST 全部消费同一规范化结果（canonical「UTF-8 文本契约」的显式化；Go `encoding/json` 本就会替换非法字节，此步骤使其在 diff API 边界前完成，保证前后端比对一致）。

### D8: API 契约与前端落点

路由（新文件 `internal/api/annotations.go` + `git.go` 追加 file 路由；handler 仅做 HTTP/DTO，模式镜像 `git.go` + `mapTaskErr`；JSON 字段一律 camelCase。**所有带 body 的端点 MUST 经新的统一 helper `decodeBoundedJSON`**（公共 `decodeJSON` 无界且只解首值，见 Context）：内部安装 `http.MaxBytesReader`、解码首个 JSON 值、再次解码并强制 `io.EOF`（拒绝尾随数据/第二 JSON 值）、区分 `MaxBytesError` 后统一映射 invalid_input，超限零业务副作用）：

| 方法 | 路径 | wire body 上限 | 请求 | 响应 | 错误 |
|---|---|---|---|---|---|
| GET | `/api/v1/tasks/{id}/annotations` | — | — | `{annotations: [Annotation], submitCapability: {state: "supported"\|"unsupported"\|"unknown", reason: string}}` | not_found(任务) / invalid_input(dir) |
| POST | 同上 | 1MiB（≈6× 两字段 decoded 上限 128KB + 结构余量） | `{path, side, ref, untracked, startLine, endLine, snapshotStartLine, snapshotLineCount, snapshot, comment}` | `Annotation`（201） | not_found(任务) / invalid_input（1-based 闭区间/窗口关系不自洽、comment 空白、字段越界、来源组合非法见 D4） |
| PATCH | `.../annotations/{aid}` | 512KiB | `{comment}` | `Annotation` | not_found(任务/批注) / invalid_input |
| DELETE | `.../annotations/{aid}` | — | — | 204 | not_found(任务/批注) |
| POST | `/api/v1/tasks/{id}/annotation-submissions` | 512KiB | `{annotations: [{id, revision}], note}`（note decoded ≤65536 bytes；annotations ≤500 且 id 不重复——**重复 id MUST 在任何落库/组装前以 invalid_input 拒绝**；**revision MUST 为 1..MaxInt64 的 JSON 整数，解析或范围失败在任何 task/store/diff 读取前返回 invalid_input**） | `Submission`（201，含 truncated） | not_found(**仅任务**) / invalid_input（空列表/重复 id/revision 非法/**id 存在但属于其他任务（跨任务）**/核心内容超阈值）/ invalid_state（任务未运行/无锚定会话/能力非 supported）/ **conflict（本任务范围内批注不存在、revision 不一致或复核失败——不区分"从未存在"与"预览后删除"，D2）** / git_error / internal（diff 读取失败，D7 组合映射） |
| GET | `.../annotation-submissions` | — | — | `{queue: [...], history: [...], failures: [...]}`（queue=queued/sending 按 seq 升序；history=sent 按 `sent_at DESC, seq DESC`；failures=failed/delivery_unknown 按 `created_at DESC, seq DESC`——秒级时间戳同秒时以 seq 决胜，排序稳定） | not_found(任务) |
| POST | `.../annotation-submissions/{sid}/cancel` | — | — | 204 | not_found(任务/提交) / invalid_state（非 queued） |
| DELETE | `.../annotation-submissions/{sid}` | — | — | 204 | not_found(任务/提交) / invalid_state（非终态 sent/failed/delivery_unknown） |
| GET | `/api/v1/tasks/{id}/git/file?path=` | — | — | 判别联合（D5）：`{editable:true, content, baseHash, lineEnding, hasBom, mode}` 或 `{editable:false, reasonCode, reason}` | not_found(任务) / invalid_input / invalid_state（任务无 worktree）/ conflict（任务锁冲突）/ internal（IO） |
| POST | `/api/v1/tasks/{id}/git/file` | 4MiB（≈6× content decoded 上限 512KiB + 结构余量） | `{path, content, baseHash, lineEnding, baseMode}` | `{baseHash}` | not_found(任务) / invalid_input / invalid_state（含任务无 worktree、初检文件消失/类型变化）/ conflict（含 hash/换行风格/mode 不一致、任务锁）/ internal |

DTO（带类型契约）：

```text
Annotation     {id: string, path: string, side: "old"|"new", ref: string, untracked: boolean,
                startLine: number, endLine: number, snapshotStartLine: number,
                snapshotLineCount: number, snapshot: string, comment: string,
                revision: number, stale: boolean, createdAt: number, updatedAt: number}
Submission     {id: string, status: "queued"|"sending"|"sent"|"failed"|"delivery_unknown",
                note: string, payload: string, truncated: boolean, error: string,
                createdAt: number, sentAt: number|null, items: SubmissionItem[]}
               // sentAt 仅 status=sent 时非 null；items 始终存在（可为空数组）
SubmissionItem {annotationId: string, path: string, side: "old"|"new", ref: string,
                untracked: boolean, startLine: number, endLine: number,
                snapshotStartLine: number, snapshot: string, comment: string}
               // 不存 snapshotLineCount：由 snapshot 行数推导，不属于提交快照契约
```

服务端校验（共用）：任务存在且非 dir 项目；路径词法校验；批注/提交 id 归属本任务。**文本字段 decoded 上限（唯一）**：`snapshot` / `comment` / `note` 各按 UTF-8 decoded bytes 单字段 ≤65536。PATCH comment：trim 后空白 → invalid_input、零修改且 revision 不变；与原值相同 → 返回原 DTO、revision 不变。stale 由 D4 计算、不落库；Annotation.revision 随每次实变严格 +1。

前端落点：
- **视图身份唯一**：diff 视图身份 MUST 为 `(path, ref, untracked)` 三元组（同一路径的 staged/unstaged/untracked 变体是不同视图）；`GitPanel` 的 `selFile`（现为 `{path, ref}`，`GitPanel.tsx:47`）扩展为完整三元组并传给 DiffViewer；文件选中高亮（现为仅按 path 比较，`GitPanel.tsx:189`）与行内批注 decoration MUST 均按三元组匹配，side 再决定编辑器侧。同路径 staged+unstaged 批注不得互相串标记。
- `DiffViewer.tsx`：模式 prop（默认查看）；查看模式挂批注扩展（选区 decoration + gutter 弱标记 + 气泡输入；快照构造与 side 映射按 D4）；编辑模式以可编辑扩展替换 `readOnlyExtensions`，debounce 500ms 触发 D5 写协议。
- `GitPanel.tsx`：批注列表面板（低存在感、可折叠）、预览弹层（条目临时移除 + 补充说明框）、queue/history/failures 分区视图、busy 警告横幅（D6）、`submitCapability.state != "supported"` 时禁用提交按钮并提示。视觉与交互细节由 designer 在实现阶段定稿。
- `web/src/api.ts` / `types.ts`：新增端点客户端与类型。

### D9: 实施顺序与测试策略

实施分 5 个有序 lane（前序未完成不进后续）：

1. **store**：migration 0012 + 查询层（CRUD、revision 递增、CAS 转移、sent 清理事务、AUTOINCREMENT seq）+ 单测。
2. **opencode 契约**：CONTRACT.md 锚点扩充与版本核验 → `client.PromptAsync` + `GET /doc` 能力探测 → `OCClient` 接口及全部 mock/fake 同步。
3. **task/application**：GitDiff 核心 helper 重构（不变量守卫测试先行）→ 批注/提交用例 → 队列/调度器/启动恢复 → 文件编辑读取与写回。**分层约束（唯一归属）**：批注/提交用例 MUST 放入新 application service 包 `internal/application/diffreview`；consumer-owned ports（均在 diffreview 包内定义、由外部实现注入）：`DiffReviewRepository`（批注/提交持久化）、`PromptPort`（投递，返回 diffreview 拥有的 `PromptOutcome`——D1 类型归属条）、`DiffSourcePort`（diff 来源内容读取，即 GitDiff 核心 helper 的能力面）、`RuntimePort`（任务 runtime 快照：instVersion、锚定会话、能力缓存、SessionStatus）、**`TaskScopePort`（任务作用域准入：查询任务存在性与项目 kind，返回结构化结果——任务不存在 / repo / dir / 未知 kind；diffreview service 自行执行准入校验，错误映射：任务不存在 → not_found、dir → invalid_input、未知 kind → internal；由 SQLite adapter 实现）**。adapters：SQLite repository 与 TaskScopePort 在 `internal/infrastructure/store`，PromptPort 在 `internal/task`（映射 `opencode.PromptResult` → `PromptOutcome`），DiffSourcePort/RuntimePort 在 `internal/task`。service MUST NOT 反向依赖 task/infrastructure 包（与 `internal/application` 现有约束一致，`application/task/lifecycle.go:1`）；Manager 仅提供任务锁、runtime 快照与调度生命周期协调，MUST NOT 继续扩大（`manager.go:182` 已标注 legacy facade）。
4. **api**：DTO + 路由 + MaxBytesReader + 错误映射。
5. **frontend**：DiffViewer 模式与批注扩展 → GitPanel 面板/预览/历史（视觉由 designer）。

测试底线（新增行为测试须在旧实现下失败、新实现下通过）：CAS 状态转移、撤回与调度竞态、重启恢复（queued 重入队、sending→delivery_unknown）、至多一次（HTTP 发出后任何失败零重投；204 后事务失败→delivery_unknown；意外 2xx（200/201/202）→ delivery_unknown + 能力置 unknown 复探；404 分流 GetSession 穷尽分支——含端点不支持分支的 sending→failed + error 文案 + 缓存转 unsupported + 零重投断言）、sent 清理 revision 语义（同秒编辑不误删）、65536 边界（core 超 65536 拒绝：65535/65536/65537）、动态 fence 含反引号内容、CRLF 快照不漂移（F15）、truncated 窗口完整保持 active（F14）、CRLF/BOM/末尾换行写回保真、写回禁锢复检（中间级 symlink 逃逸零写盘）、非 UTF-8/混合换行/CR-only 拒绝编辑、base 冲突零写盘、每文件写请求串行、wire 上限临界 ±1 字节与 2×/6× JSON 转义膨胀边界、PATCH 空白/同值 revision 不变、提交组装期间并发 PATCH/DELETE 批注 → conflict(409) 零副作用、请求到达前批注已改版/已删除 → conflict（不区分从未存在与预览后删除）、批次混合错误优先级（跨任务 id + 缺失批注同批且交换数组顺序，恒 invalid_input）、adapter 获取失败（taskOcClient ok=false → pre_send_failure → 重试耗尽 failed，绝不 delivery_unknown）、revision 非法值（0/-1/小数/溢出）→ invalid_input、写回 content 含 CRLF 或裸 `\r` 一律 invalid_input 零写盘、编辑会话 lineEnding 冻结（CRLF 文件删除全部换行后新增换行仍按 crlf 重建；当前文件换行风格与冻结值不一致 → 409）、能力探测事件模型（首探/复探 singleflight 合并/缓存随 instVersion 失效/恢复队列发送前 supported 门禁/400 置 unknown 复探/投递前门禁三分支含缓存已 unsupported 直接 failed 不复探）、decodeBoundedJSON（合法对象后附超量空白、第二 JSON 值、N/N+1 字节）、payload 字节拼接 golden（note/comment/snapshot 分别以无换行、`\n`、`\n\n` 结尾）、UTF-8 规范化顺序 golden（连续非法字节、边界半个 rune、替换扩张后超限置 truncated）、保存结果未知的重读确认恢复（四元逐项比对：content==sentContent 且 hasBom==冻结 且（含换行时）lineEnding==冻结 且 mode==首次读取值 → 采用新基线并立即补发最新编辑；任一不一致保持阻塞；规范化文本相同但 BOM、换行风格或 mode 不同仍阻塞）、写请求在途期间继续编辑后响应丢失、视图身份三元组（同路径 staged+unstaged 批注标记互不串扰）、`go test -race` 全量；messageID 与 submission 一一对应（准备重试复用同一值、`message_id` UNIQUE 碰撞测试）；写回 rename 前终检（flush 后目标被外部修改 → 删临时文件 + conflict）；OCClient 编译期接口断言与全部 mock/fake 同步；PromptPort 双任务路由隔离（adapter 经 taskID 取各自 client+directory，互不串投）；跨任务批注 id 提交 → invalid_input 零落库零 diff 读取；flush 后终检前 chmod → 删临时文件 + conflict；特殊权限位完整保真（setuid/setgid/sticky 经写回后不变）与终检前切换特殊位的冲突测试；只读文件（无 owner 写位）编辑读取 read_only + 写回 invalid_input；runtime 不可用仍完成 sending→delivery_unknown 启动收敛（收敛写库失败 fail-closed）；TaskScopePort 准入（任务 missing/dir/未知 kind 各自错误映射且零调用其他副作用端口）；文件 parts 投递（调度器收集 items 去重保序 path、items 读取失败 → failed 不发送、adapter 跳过缺失/非 regular 文件）。

### 主流程

```
查看模式 diff
   │ 框选/点击 + 评论（空评论丢弃；快照取自原始侧内容，含行尾字符）
   ▼
批注落库(diff_annotations, revision=1) ──列表读取同源全窗口比对──▶ stale 标记
   │ 积累 N 条
   ▼
提交预览（临时移除/补充说明）
   │ 确认：准入校验(运行中/锚定/能力/核心体积) → 任务锁内组装 payload
   │      → 单事务 submissions(queued)+items 快照           │ 准入失败 → invalid_input/invalid_state/conflict(复核失败)，零副作用
   ▼                                                       │ 撤回(仅 queued) → DELETE 行
调度器：SessionStatus idle/缺席 → CAS queued→sending
   ▼
PromptAsync(messageID=msg_<submission UUID 去连字符小写>, text=payload)
   │ 204                        │ 网络模糊/事务失败/意外2xx   │ 400/401/404/其他非2xx
   ▼                            ▼                  ▼
同事务 sent + 按 id+revision 删批注   delivery_unknown   failed（error 落库）
   │                            │                  │
   ▼                            ▼                  ▼
history 分区（可删）         failures 分区（可删，活动批注保留）
```

## Risks / Trade-offs

- TOCTOU：批注快照到 agent 实际读取期间代码可能再变 → 已接受（proposal 范围边界）；快照+上下文行使 agent 可定位。
- 低版本 opencode serve 无 `prompt_async` → D1 能力探测禁用 + 404 分流兜底，不降级到其他通道。
- delivery_unknown 需用户人工判断 agent 是否已收到 → 失败分区展示原文 payload，用户可复制内容手动重发（无自动入口）。
- 编辑写回与 agent 并发写 → 终检可观测范围内由 D5 hash 校验拒绝；终检至 rename 的残余窗口按 DR-R1 接受；D6 警告横幅降低意外。
- 还原快照仅内存态，页面刷新后不可还原 → 入口文案注明作用范围。
- 编辑限定 UTF-8 + 统一换行：其余文件 editable=false 并给明确 reasonCode（与 diff 读取的 UTF-8 文本契约一致，`git-operations/spec.md:40`）。

## Migration Plan

新增 `0012_diff_annotations.sql`（三表，模式对齐 `0011`）。无存量数据迁移；回滚 = 删表（新表无外部依赖）。

## Open Questions

- 批注列表面板的默认折叠状态与布局细节 → 实现阶段由 designer 定稿。
