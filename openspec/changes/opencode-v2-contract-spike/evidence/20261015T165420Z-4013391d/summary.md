# 模型无关契约形态探测结论（run 3 = 20261015T165420Z-4013391d，干净单轮）

基线：v1=1.18.26 / v2=2.0.28（源码锚点 026b106b，二进制 044d8b7d，sha256 核验）。
上游 update 渠道漂移（latest 已到 2.0.29）经 harness 渠道漂移回落核验通过（`source-chain/v2.update-channel-drift.log`）。
边界：Docker 容器（--read-only rootfs + tmpfs，SPIKE_BOUNDARY_MECHANISM=container）。

## 1. OpenAPI 静态形态提取（A1–A9，置信=静态schema推断）

来源快照：`evidence/20261014T045810Z-f125cafd/live/discovery-doc_openapi.json.json`
（openapi 3.1.0，118 paths，sha256 b0cdccd2…55feeb）。
逐项 schema 摘录见 `live/static-extract.endpoint-dumps.json`；完整结论见
`live/static-extract.openapi-forms.md`（A1–A9）。要点：

- **A1 question 端点：不存在。** 全文档无 question 字样；等价能力 = form 族
  （GET/POST `/api/session/{id}/form`、GET/DELETE `/api/session/{id}/form/{formID}`、
  POST `.../form/{formID}/reply`（204）、GET `/api/form` location deepObject 查询）。
  错误语义：404 FormNotFound / 409 FormAlreadySettled / 400 FormInvalidAnswer。
- **A2 permission 回复端点**：POST `/api/session/{sessionID}/permission/{requestID}/reply`，
  body `{decision: "once"|"always"|"reject", message?}`，成功码 **204**；
  pending = GET `/api/permission/request`；saved = GET `/api/permission/saved`
  （projectID query，`{data[]: PermissionSaved.Info}`，required
  id/projectID/action/resource/time）+ DELETE `/api/permission/saved/{id}`（204）。
- **A3 停止端点：无 /abort。** 等价 = POST `/api/session/{sessionID}/interrupt`
  （query `resume?: boolean`；200 `{interrupted: boolean}`）。
- **A4 标题写入端点**：PATCH `/api/session/{sessionID}`（body `{title?, metadata?,
  permissions?}`，additionalProperties false；**204**）；读回 GET 200 `{data: Session.Info}`。
- **A5 session 列表分页**：cursor 模式。query：limit(string，默认 newest 50)、
  order(asc|desc)、cursor(opaque)、search、parentID、directory、project、subpath；
  200 = `SessionsResponse {data: Session.Info[], cursor: {previous, next}}`，
  required [data, cursor]；400 InvalidCursorError。
- **A6 prompt body schema**：`{id?: ^msg_, text(required), files: FileAttachment[],
  agents?, skills?, metadata?, delivery?, resume?}`，additionalProperties false。
  FileAttachment = `{uri(必), name?, description?, mention?}` —— **无 mime 字段**
  （mime 归属由 uri/上游推断，schema 层不存在）。
- **A7 session 删除**：DELETE `/api/session/{sessionID}`（204，级联子 session）。
- **A8 prompt 响应回显 messageID**：200 `{data: Session.Inbox.User}`，
  `data.id` 即 Session.Message.ID（^msg_），请求自带 id 原值透传（live 亦实证，见下）。
- **A9 event 类型枚举**：OpenAPI **无封闭枚举**（V2EventEncoded={type:string,
  contentMediaType}）；唯一字面量 failureEvent=`effect/http-api/stream/failure`；
  message.type 枚举=[agent-switched, model-switched, location-switched, user,
  synthetic, system, skill, shell, assistant, compaction]。

## 2. 模型无关 live 探测（置信=实证）

- **title 写入（title.01 PASS）**：PATCH /api/session/{id} body {title} → 204；
  GET 读回 title 精确匹配。evidence: `live/misc/title-get.json` 等 `live/title/`。
- **session 分页完整性（session.05 PASS）**：造 5 个 session，limit 精确生效
  （count==limit 且成员属于预期集合），无限查询完整（F⊆V）；
  适配层可按保守规则判 Overflow。evidence: `live/session/page-counts.txt`。
  **新契约差异（13 项之外）**：v2 `POST /api/session` 无 directory query 参数
  （v1 有），目录定位走 body `location:{directory}`——create-directory 语义变化。
- **删除触发（event.02c.session_deleted PASS）**：DELETE /api/session/{id} = 204，
  event 流观测到归属该 session 的删除事件（精确 type+归属）。
  evidence: `live/destructive/stream.ndjson`。session.04 PASS：删除后 GET=404。
- **GET /api/session 包裹形状（session.05 / session.03 PASS）**：200 响应为
  `{"data":[...],"cursor":{...}}` envelope（A5 静态形态的 live 确认），
  解析缺口已由 harness envelope 解包修正覆盖。
- **prompt messageID 回显（prompt.02 PASS）**：提交被接受（HTTP 200）且
  `data.id`（msg_spike_*）与请求自带 id 原值一致，响应内结构化回显。
  evidence: `live/prompt/submit-resp.json`。

## 3. migration.reverse 检测口径修正 + 重跑结论

- 修正：harness 落盘 marker 检测原只匹配 v1 文件布局（`/(message|msg|part)`），
  已补 `opencode.db`（含 -wal/-shm 伴生）覆盖 v2 SQLite 布局。
- 重跑结论（run 3）：**migration.reverse = FAIL / 不可满足（VALID_NEGATIVE）**——
  v1 打开 v2 原生 SQLite 数据明确报数据类错误
  `Database is not empty and has no session table`（rc=1）。
  evidence: `migration/reverse-valid_negative.txt` + `live/serve-reverse.log`。
  仅为事实记录，不作承诺。
- migration.forward = INCONCLUSIVE（权限样本造数需真实模型，属模型依赖泳道）。

## 过程备注

- run 1（20261015T162223Z-2997862e）= harness 修复迭代过程证据（results.jsonl 5 轮
  追加 + title.01/session.05/event.02c/prompt.02 逐项转 PASS 过程）；其静态提取
  产物已复制进本 run。
- run 2（20261015T164857Z-2df53b5f）= F12 缺表正则修正验证轮（reverse 错误文本
  首次捕获）。
- harness 同 run 重跑受保护（probe_id 唯一性 / migration work dir 不可复用），
  迭代修复通过新 run 完成；本 run 为唯一干净单轮完整结论。
