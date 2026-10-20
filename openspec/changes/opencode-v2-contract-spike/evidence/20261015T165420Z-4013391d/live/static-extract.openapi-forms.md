# Phase A：v2.0.28 OpenAPI 静态形态提取（模型无关部分）

- run_id: 20261015T162223Z-2997862e
- 探测对象快照: `evidence/20261014T045810Z-f125cafd/live/discovery-doc_openapi.json.json`
  （v2.0.28 serve 实机 `/openapi.json` 快照；sha256 `b0cdccd2c0c651f2adbded5efc6a77506521fbf509188c91712d5a854255feeb`，276235 bytes）
- 快照 meta: openapi 3.1.0，118 paths；components.schemas 为准，本文所有 schema 引用均来自该快照原文
- 置信标注约定：`[静态schema推断]` = 本 run 仅依据 OpenAPI 快照原文；`[实证]` = 本 run 有 live 响应佐证（Phase B/C 产出）
- 提取过程佐证: `live/static-extract.endpoint-dumps.json`（逐端点原始 schema 摘录，与本文件同 run 落盘）

## A1. question 相关端点

**结论：不存在**。全文档 118 paths 中没有任何 `question` 字样（路径、operationId、description 均无）。
v2 的等价能力形态为 **form** 端点族：
- `GET /api/form`（form.list，query `location` deepObject：`location[directory]=...`）
- `GET|POST /api/session/{sessionID}/form`（session.form.list / session.form.create）
- `GET|DELETE /api/session/{sessionID}/form/{formID}`
- `POST /api/session/{sessionID}/form/{formID}/reply`（session.form.reply，**成功码 204**）

**置信：[静态schema推断]**（live 佐证：原 spike attention.03 曾对 question 候选路径得 404，与「不存在」一致）

Form schema 要点（原文摘录）：
- `Form.Info`: required `id, sessionID, title, fields`；`Form.ID` pattern `^frm_`
- `Form.Fields`: array，prefixItems + items = `Form.Field`，minItems 1
- `Form.Field`: anyOf `Form.StringField | Form.NumberField | Form.IntegerField | Form.BooleanField | Form.MultiselectField | Form.ExternalField`
- `Form.Reply`: body `{"answer": Form.Answer}`；`Form.Answer`: object（additionalProperties=Form.Value）；`Form.Value`: string | number("Infinity"/"-Infinity"/"NaN" 字面量) | boolean | string[]
- `Form.CreatePayload`: required `title, fields`；成功 200 `{data: Form.Info}`；reply 错误：404 FormNotFoundError / 409 FormAlreadySettledError / 400 FormInvalidAnswerError

## A2. permission 回复端点形态与 /api/permission/saved 语义

**结论**：
- 回复 pending permission：`POST /api/session/{sessionID}/permission/{requestID}/reply`（session.permission.reply），body `{"decision": Permission.Reply, "message": string|null}`，required `decision`；`Permission.Reply` enum **`["once","always","reject"]`**；成功码 **204**。
- pending 列表：`GET /api/permission/request`（permission.request.list，query `location` deepObject，同 A1 form），200 `{location, data: Permission.Request[]}`；`Permission.Request` required `id, sessionID, action, resources`（`resources: string[]`，`save: string[]`）。
- 另有 session 维度端点：`GET|POST /api/session/{sessionID}/permission`、`GET /api/session/{sessionID}/permission/{requestID}`。
- `GET /api/permission/saved`（permission.saved.list，query `projectID` 可选）：语义=「Retrieve saved permissions, optionally filtered by project」，200 `{data: PermissionSaved.Info[]}`（**顶层直接 data 包裹，无 location 键**）；`PermissionSaved.Info` required `id, projectID, action, resource, time`；`DELETE /api/permission/saved/{id}` 存在（204）。
- saved 合并语义线索：reply decision=`always` 时落 saved（此为静态推断，无文档文字直说）；`Permission.Request.save: string[]` 表示请求方可建议的 save 选项。

**置信：[静态schema推断]**（reply 触发需真实模型驱动工具调用，原 spike 已证无法无模型触发 pending，故 saved 语义只能到此粒度）

## A3. session 停止端点

**结论：`POST /api/session/{sessionID}/interrupt`（session.interrupt），无 /abort 端点**。
- 全 118 paths 与全部 operationId 中无 abort/stop 字样。
- query 参数 `resume`（"true"|"false"，可选）：resume=true 时「执行恢复等待 steering input 与 next-in-line control items，queued prompts 保持 parked」。
- 成功 200 `{data: SessionInterruptResponse}`；`SessionInterruptResponse` = `{interrupted: boolean}`——true=实际中断了活跃执行，false=idle no-op。
- 错误：400 InvalidRequestError / 401 / 404 (SessionNotFound|LocationNotFound)。

**置信：[静态schema推断]**

## A4. 标题写入端点

**结论：`PATCH /api/session/{sessionID}`（session.update），成功码 204**。
- body `{"title": string|null, "metadata": Session.Metadata|null, "permissions": Permission.Ruleset|null}`，additionalProperties false，requestBody required。
- 读回：`GET /api/session/{sessionID}` 200 `{data: Session.Info}`，`Session.Info` 含 `title: string`。
- 错误：400 / 401 / 404 (SessionNotFound|LocationNotFound)。

**置信：[静态schema推断]**（live 回读佐证见 Phase B title.01 修正后结果）

## A5. session 列表分页参数

**结论**：`GET /api/session`（session.list）分页 = **cursor 模式**，参数：
- `limit: string|null`（「Maximum number of sessions to return. Defaults to the newest 50 sessions.」）
- `order: "asc"|"desc"|"null"`（「Session order for the first page」）
- `cursor: SessionsCursor|null`（opaque string，取自上次响应 `cursor.next`/`cursor.previous`）
- 其余过滤：`search`、`parentID`（Session.ID 或字面量 "null"=只取根）、`directory`、`project`、`subpath`
- 200 响应 = `SessionsResponse`：`{data: Session.Info[], cursor: {previous, next}}`，**required [data, cursor]**（分页包裹形状的静态依据，供 session.05）
- 400 含 InvalidCursorError。

**置信：[静态schema推断]**（live 完整性遍历佐证见 Phase B）

## A6. prompt body schema

**结论**：`POST /api/session/{sessionID}/prompt`（session.prompt）：
- body：`{id: Session.Message.ID|null, text: string, files: PromptInput.FileAttachment[], agents, skills, metadata, delivery, resume}`，required 仅 `text`，additionalProperties false。
- `Session.Message.ID`: string pattern `^msg_` —— **调用方可自带 id（msg_ 前缀）**，即 prompt.02 的 messageID 原值透传在 schema 层面被支持。
- `PromptInput.FileAttachment`: `{uri: string(必填), name, description, mention}` —— **无 mime 字段**；mime 归属推断为服务端按 uri/内容判定（文件扩展名），调用方不可指定。
- 成功 200 `{data: Session.Inbox.User}`；`Session.Inbox.User` required `id, sessionID, time, type, payload, delivery`，`type: enum ["user"]`，`delivery: enum ["steer","queue"]`。
- 409 ConflictError 存在（执行冲突）。

**置信：[静态schema推断]**（原 spike prompt.01 实证过 `{text,id}` 提交成功；prompt.02 透传 live 佐证见 Phase B）

## A7. session 删除正确方法

**结论：`DELETE /api/session/{sessionID}`（session.remove），成功码 204**（「Delete a session and its child sessions.」——级联删除子 session）。
- 解原 spike event.02c/session.04 405 的依据：v1 是 `DELETE /session/{id}`，v2 是 `DELETE /api/session/{id}`（405 源于 /api 前缀缺失被 SPA 路由接管）。

**置信：[静态schema推断]**（live 佐证见 Phase B/C event.02c 修正后结果）

## A8. prompt 响应是否回显 messageID

**结论：是（形态上）**。200 响应 `{data: Session.Inbox.User}`，`Session.Inbox.User.id: Session.Message.ID`（`^msg_`）。请求 body 的 `id` 若提供则应原样回显（schema 未写「若缺省自动生成」，但 required 列表含 id，说明返回必然携带 id）。live 透传实证见 Phase B prompt.02。

**置信：[静态schema推断]**

## A9. event 类型枚举（含错误事件名）

**结论：OpenAPI 不内联事件枚举**。`GET /api/event`（event.subscribe，SSE）200 响应 schema：`{id: string|null, event: string, data: V2EventEncoded}`；`V2EventEncoded` = `{type: string, contentMediaType: application/json}`——即事件载荷为不透明 JSON 字符串，**event 类型名单在静态契约中不存在**。event 名相关字面量仅 `failureEvent: "effect/http-api/stream/failure"`（流失败事件，effect 框架元数据）。
- live 已实证的事件名（原 spike + 本 run Phase C）：`session.created`、`session.renamed`、`session.deleted`、`session.error` 等；静态层无法给出封闭枚举，冻结建议以 live 枚举为准。
- message type 枚举（`GET /api/session/{id}/message` 的 `type` query）：`["agent-switched","model-switched","location-switched","user","synthetic","system","skill","shell","assistant","compaction"]`。

**置信：[静态schema推断]**（「无封闭枚举」为静态事实；事件名清单见 live 证据）

## 覆盖核对（对任务 Exact contracts 第 1 组）

| 项 | 结论 | 置信 |
|---|---|---|
| question 端点 | 不存在；等价=form 族（A1） | 静态schema推断 |
| permission 回复 | POST reply 端点 204；decision enum once/always/reject；saved=list+delete（A2） | 静态schema推断 |
| session 停止 | POST /interrupt（无 /abort）（A3） | 静态schema推断 |
| 标题写入 | PATCH /api/session/{id} 204（A4） | 静态schema推断 |
| session 分页 | cursor 模式，SessionsResponse 包裹（A5） | 静态schema推断 |
| prompt body | required text；files={uri,...} 无 mime；id 可透传（A6） | 静态schema推断 |
| session 删除 | DELETE /api/session/{id} 204（A7） | 静态schema推断 |
| prompt 回显 messageID | 200 data.id（A8） | 静态schema推断 |
| event 类型枚举 | 静态无枚举；仅 failureEvent 字面量（A9） | 静态schema推断 |
