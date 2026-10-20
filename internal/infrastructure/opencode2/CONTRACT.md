# opencode v2 契约冻结（opencode2）

ocdeck 对 opencode v2 的兼容性按**冻结基线**声明，当前锚点：

- `ContractVersion`（`2.0.28`）
- 源码锚点 commit：`026b106bc5d41b679ffc413264229c340871e4d3`（tag `v2.0.28`；evidence run manifest `v2.commit_sha` 一致）
- 二进制构建 commit：`044d8b7d`（`--version` 构建元数据，与源码锚点分别记录；见 spike design.md「Spike 结论」）

证据基线（路径均相对 `openspec/changes/opencode-v2-contract-spike/evidence/`）：

- **R1** = `20261015T165420Z-4013391d`（模型无关实证 run，含 OpenAPI 静态提取）
- **R2** = `20261016T090000Z-0bb6ac6c`（模型依赖实证 run）

静态锚点核验脚本：`scripts/check-opencode-v2-contract.sh`（对源码锚点 commit 拉取锚点文件做存在性/形态断言，exit 0 = 通过）。升级 SOP 见文末。

每条事实 = 契约断言 + 证据引用 + 置信标注：**〔实证〕** = live 响应；**〔静态schema〕** = OpenAPI/源码提取。

## 1. 传输形态

- API 前缀 `/api`；成功响应包 `{data: ...}` envelope。〔实证〕R1 `live/discovery.txt`（`api_prefix=/api`）+ `live/session/create.json` + `live/session/page-limit1.json`
- 错误响应**非** envelope：`{"_tag": <ErrorTag>, "message": ..., "kind": ...}`。〔实证〕R1 `live/misc/status.json`（`_tag: InvalidRequestError`）
- OpenAPI 契约发现 = `GET /openapi.json`；`/doc` 返回 SPA HTML（非 JSON 文档）。〔实证〕R1 `live/discovery.txt` + `live/discovery-doc_openapi.json.json`（openapi 3.1.0，118 paths，sha256 `b0cdccd2…55feeb`）
- `/global/health` 不存在于 v2 API 面（OpenAPI 快照 0 命中 health 路径；非 `/api` 路径由 SPA fallback 服务，`/doc` 实证返回 HTML）→ **不能**用作就绪判定。就绪判定 = `GET /api/session`：配置密码时匿名 401 / 带 Basic Auth 200。〔实证〕R1 `live/supervisor/auth.txt`（`anon=401 auth=200`）+ OpenAPI 快照
- serve 必须子命令形态：`opencode serve [flags]`（无裸 `--port` 全局启动）。〔实证〕R1 `live/supervisor/invalid-args.txt`（USAGE 块）

## 2. Session

- 创建 `POST /api/session`，**directory 定位在 body `location: {directory}`**（无 `directory` query 参数；v1 为 query 参数、v2 忽略之——create-directory 语义变化，spike 13 项差异之外新增）。响应 `{data: Session.Info}`，session ID `^ses`。〔实证〕R1 `live/session/create.json`；`live/session/page-create-*.json`（v1 式 query 传法全部落到默认目录）；〔静态schema〕R1 `live/static-extract.endpoint-dumps.json`（`session.create` payload `location: Location.PublicRef`）
- 标题写入 `PATCH /api/session/{id}` body `{title}` → **204**（空体）；读回 `GET /api/session/{id}` → `{data.title}` 精确匹配。〔实证〕R1 `live/misc/title-write.json`（204 空体）+ `live/misc/title-get.json` + `results.jsonl`（title.01 PASS）
- 删除 `DELETE /api/session/{id}` → **204**（级联删子 session）；随后 GET 404。〔实证〕R1 `live/destructive/delete-codes.txt`（`delete=204 after=404`）
- 列表分页 = **锚点式 cursor** 模式：`{data: Session.Info[], cursor: {previous, next}}`，cursor 为 opaque string——base64 anchor，解码形如 `{"directory":"...","anchor":{"id":"<last-id>","time":...,"direction":"next"}}`。**`next`/`previous` 永远给出（导航锚点），非空 ≠ 还有数据**；完整性信号 = **短页**（`len(data) < limit`）或跟随 next 得空页。客户端 HasMore 推导 = `len(data)==limit && next!=null`（恰好满页且 next 非空的保守 overflow 是罕见边界，可接受）。〔实证·语义修订〕2026-10-20 v2.0.28 容器实测：`GET /api/session?directory=/home/ocdeck&limit=1000` 返回 data 仅 3 条但 cursor.previous/next 均非空，跟随 next → `{"cursor":{"previous":null,"next":null},"data":[]}`——推翻早期「next 非空 → 多页」假设；envelope required 集合（根 object/data 非 null 数组/cursor 双键 string|null）不变。〔实证·形态〕R1 `live/session/page-limit1.json` + `page-limit3.json` + `page-counts.txt`（session.05 PASS）；〔静态schema〕`live/static-extract.endpoint-dumps.json`（`SessionsResponse` required `[data, cursor]`，400 `InvalidCursorError`）
- 环境注入 `PUT /api/session/{id}/environment` body `{variables: {...}}` → **204 替换式**；session 间隔离已实证（A/B 互不串用）。〔实证〕R2 `live/extra/p1b_environment.json` + `live/env/env-shell.json`

## 3. Prompt

- `POST /api/session/{id}/prompt` body `{text, id}`（id `^msg_`，可自带）→ **200** `{data: ...}`，`data.id` 与请求 id **原值回显**。〔实证〕R1 `live/prompt/submit-resp.json`（`data.id=msg_spike_*` 与提交一致）+ `results.jsonl`（prompt.02 PASS）
- 附件 = `files: [{uri(必), name?, description?, mention?}]`，**无 mime 字段**（mime 归属由 uri/服务端推断，客户端不传）。〔静态schema〕`packages/schema/src/prompt-input.ts` `FileAttachment` + R1 `live/static-extract.openapi-forms.md` A6；〔实证〕R2 `live/prompt/assistant-texts.json`（`file://` uri 附件被接受且 agent 读出内容）
- inline base64 附件：**未实证**（v2 仅有 uri 形态实证；不冻结 base64 行为）

## 4. 停止

- 停止端点 = `POST /api/session/{id}/interrupt` → **200** `{interrupted: bool}`；执行中 `interrupted=true`、turn `outcome=interrupted`，之后可继续 prompt；`/abort` **不存在**（OpenAPI 0 命中 + live 404）。〔实证〕R2 `live/extra/p1b_interrupt.json`（含 `abort_candidate http=404`）+ `live/lifecycle/interrupt-facts-1.json`；R1 OpenAPI 快照（abort 0 命中）

## 5. Permission

- 回复 `POST /api/session/{id}/permission/{requestID}/reply?directory=<d>` body `{decision: "once"|"always"|"reject"}` → **204**（query 形态与 design D7 一致，R2 once/reject 实证 204）；重复回复 / 幽灵 `per_` id → **404 PermissionNotFoundError**。〔实证〕R2 `live/attention/once-facts.json` + `reject-facts.json`
- pending 仅存于 session 级 `GET /api/session/{id}/permission`；全局 `GET /api/permission/request` 必须带 `location[directory]` deepObject 过滤（否则返回空 `{location, data:[]}`）；`/api/permission/saved` 仅存已保存规则（非 pending，`{data: []}` 初始为空）。〔实证〕R2 `live/attention/pending-observation.json`
- permission ID `^per`；升起事件 `permission.asked`（data 含 per id / sessionID / action / resources）。〔实证〕R2 `live/event/perm-frames.jsonl`；〔静态schema〕`packages/schema/src/permission.ts`（`isStartingWith("per")`、`permission.asked` 定义）

## 6. Question → Form 等价

- question 端点**不存在**：OpenAPI 快照 0 命中 `question`，live `GET /api/question` 404。〔实证〕R1 OpenAPI 快照 + `results.jsonl`（attention.03）；〔静态schema〕R1 `live/static-extract.openapi-forms.md` A1
- 等价 = form 族：`GET /api/session/{id}/form`（pending 判定 = `state.status: "pending"`）、`POST /api/session/{id}/form/{formID}/reply` body `{answer}` → **204**；重复回复 → **409 FormAlreadySettled**（另 404 FormNotFound / 400 FormInvalidAnswer）；全局 `GET /api/form`（location deepObject）。form ID `^frm_`。〔静态schema〕R1 `live/static-extract.openapi-forms.md` A1 + `static-extract.endpoint-dumps.json`（`session.form.*` error 列表）；form 端到端交互依赖模型产出，未做 live 实证
- **form 字段结构化映射（已冻结，design D10，2026-10-16 用户决策；原「逐字段映射待实证」标注移除）**：field `key→Key、title→Label、description→Prompt、type→normalizeFormType`（有限映射：string/text→text、boolean→boolean、number→number、enum→select、multiselect（array enum）→multiselect、未知类型保留原值不降级）；回复 key-based（`answer[key]=value`），SPI `AttentionReply.Values` 优先、`Answers` 按字段顺序兼容转换；不再拼接文本、不再按位置 zip；已建模属性之外的属性（hidden/when/format/default/custom 等）原样归入 `QuestionField.Metadata`（仅保留未入模型的字段，不得成为消费方主路径）

## 7. 事件

- SSE = `GET /api/event`；事件结构 `{id: evt_*, created, type, location: {directory}?, data: {..., sessionID}, durable: {aggregateID, seq, version}?}`；**归属键 = `data.sessionID`**。〔实证〕R2 `live/event/p1b-frames.jsonl`（`session.inbox.enqueued` / `session.execution.started` 逐字段可见）
- 执行边界事件：`session.execution.started` / `session.execution.succeeded`（**无** v1 式 `session.status` idle/busy 事件）。〔实证〕R2 `live/extra/p1b_status_seq.json` + `live/event/p1b-frames.jsonl`
- 删除事件 `session.deleted`，归属目标 session（独立 fixture 精确 type + 归属）。〔实证〕R1 `live/destructive/stream.ndjson` + `results.jsonl`（event.02c.session_deleted PASS）
- 事件类型**无封闭枚举**（OpenAPI `V2EventEncoded.type: string`）〔静态schema〕`packages/schema/src/event-manifest.ts` + `packages/protocol/src/groups/event.ts`（union + `server.connected` + `rpc.*` template）；唯一 failureEvent 字面量 `effect/http-api/stream/failure`（stream 级）〔OpenAPI 静态提取观察〕R1 `live/static-extract.openapi-forms.md` A9——该字面量**不存在于 opencode 源码锚点文件**（effect 框架运行时产物），不得作为静态锚点

## 8. Status 冻结映射

- busy = SSE `session.execution.started`；idle = `session.execution.succeeded`；v2 已移除 `GET /session/status`（400）。〔实证〕R2 `live/extra/p1b_status_seq.json` + R1 `live/misc/status.json`
- `Session.Info.outcome ∈ {succeeded, failed, interrupted}`：仅诊断字段，**不映射**用户可见状态。〔静态schema〕R1 `live/static-extract.endpoint-dumps.json`（`Session.Info.outcome` Literals）；〔实证〕R1 `live/supervisor/health-body.json`（`"outcome":"succeeded"`）

## 9. 未实证项（保守语义冻结）

- conn.01（断线重连对账）未实证 → 重连对账冻结为**按快照自补**语义，不依赖事件回放补齐。〔实证〕R1 `results.jsonl`（conn.01 INCONCLUSIVE）
- event.02g（错误事件名）未实证 → 错误事件映射冻结为**保守形态**：不依赖具体错误事件名，仅 `effect/http-api/stream/failure` 为 stream 级失败信号。〔OpenAPI 静态提取观察〕R1 `live/static-extract.openapi-forms.md` A9 + `summary.md`（非 opencode 源码锚点，见 §7）

## 10. TUI attach 自动选中

- 机制 = `opencode --server <url> --session/-s <id>`（root 命令参数；`mini`/`run` 子命令同语义）：session 存在 → TUI 启动即选中；不存在 → 该 ID 成为首个 prompt 所建 session 的 ID（装配点 `packages/cli/src/commands/handlers/default.ts` findSession）。〔实证+静态schema〕R2 `live/tui/tui-autoselect-facts.md` + `live/tui/tui-autoselect-capture.bin`
- serve 无 `OPENCODE_SERVER_PASSWORD` 时自动生成随机密码并打印 stdout（API 面始终 401 保护）；TUI 客户端从自身 env 读同一密码。〔实证〕R2 `live/tui/tui-autoselect-facts.md`

## 升级 SOP

1. 静态锚点核验：`bash scripts/check-opencode-v2-contract.sh`（锚点钉在脚本头部 `COMMIT`；对锁定 commit exit 0 = 通过）。
2. 上游发新版：改 `COMMIT` 重跑脚本；有断言失败时对照本文件逐项分析；live 项（204/200 状态码、回显、事件序列）需重跑 spike probes（`scripts/spike-v2/`，参照 R1/R2 同构 run）后再更新本文件。
3. 本文件只记录冻结事实；设计判断回 openspec artifacts。
