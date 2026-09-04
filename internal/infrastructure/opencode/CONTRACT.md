# opencode 契约区间

ocdeck 对 opencode 的兼容性按**已验证版本区间**声明，当前为 **[1.18.14, 1.18.26]**。

- `ContractMinVersion`（`1.18.14`）：区间下限
- `ContractBaseline`（`1.18.26`）：区间上限 / 最近一次核验版本

> 1.18.18 → 1.18.25 核验（2026-08-31）：23 锚点中 21 个字节一致；仅 `handlers/global.ts` / `groups/global.ts` 有 diff，全部位于 `/global/upgrade` 自升级端点（`target` 改为必填 + semver 校验、raw body 改声明式 schema），ocdeck 使用的端点契约未变。
>
> 1.18.25 → 1.18.26 核验（2026-09-02）：23 锚点全部字节一致，ocdeck 使用的端点契约未变。

版本检查**仅告警**（启动日志 + `/api/v1/server/status` 的 `versionVerified`），**不是激活门禁**。真正的激活门禁是能力探测：`GET /global/health` 可达、`GET /session/status` 结构、session 列表字段形状（DELETE 形状不做 live 探测，首次真实删除时校验）。区间外的版本仍可尝试激活；探测失败才阻止。

区间内各版本的契约锚点文件已逐相邻版本核验（锚点 diff SOP，见下"升级 SOP"与"1.18.18→1.18.26 相邻对核验记录"）。扩展区间前必须再跑一遍锚点 diff + live probe。external 分支行为变化（默认不启动真实 server、API 面分叉、auth 语义变化）MUST 阻断区间扩展。

## diff-review-workbench 新增契约锚点（D1 提交通道）

本 change 新增三个契约锚点，用于 `POST /session/{sessionID}/prompt_async` 提交通道与能力探测。核验方式：源码锚点（1.18.18）+ 相邻对锚点 diff（1.18.18→1.18.26）+ 本机 live probe（1.18.26，见下"live probe 记录"）。区间已正式上调至 [1.18.14, 1.18.26]。

### 新增锚点文件

- `packages/schema/src/v1/session.ts`（**已含于既有 23 锚点**；MessageID 前缀 `msg` 由 `isStartsWith("msg")` 约束，本 change 复用该约束）
- `packages/opencode/src/session/prompt.ts`（PromptInput 类型：`{messageID, parts:[{type:"text",text}]}` 为最小发送子集；完整 PromptInput 允许 model/agent/noReply/tools/format/system/variant 等可选字段，本 change 不发送）—— **新增为第 24 个锚点**
- opencode OpenAPI 文档端点 `GET /doc` 返回的 `paths["/session/{sessionID}/prompt_async"].post`：
  - 路径键逐字：`/session/{sessionID}/prompt_async`
  - operationId 逐字：`session.prompt_async`
  - requestBody.messageID schema pattern：`^msg`（对应 schema `isStartsWith("msg")`）

### 新增端点契约摘要

| 端点 | 关键形状 |
|---|---|
| `POST /session/{sessionID}/prompt_async?directory=` | body `{messageID, parts:[{type:"text",text}]}`；**204 = accepted**（异步执行，无去重——至多一次由本系统保证）；其余状态码按 design.md D1 错误矩阵 |
| `GET /doc` | OpenAPI JSON；受 Basic Auth 中间件保护（配置 `OPENCODE_SERVER_PASSWORD` 时无 Auth→401；未配置时无需认证，无 Auth 返回 200）；能力探测结构化解析 prompt_async 路径键 + operationId |

### 1.18.18→1.18.26 相邻对锚点核验记录

`scripts/check-opencode-contract.sh` 逐相邻版本 diff（24 锚点，2026-09-02 本机执行）：

| 相邻对 | DIFF 锚点 |
|---|---|
| v1.18.18 → v1.18.19 | 无 |
| v1.18.19 → v1.18.20 | 无 |
| v1.18.20 → v1.18.21 | `packages/opencode/src/session/prompt.ts`（内部助手：`lastAssistant.finish` 比较增加 `"unknown"` 值；**不影响 PromptInput 类型结构与 messageID pattern**） |
| v1.18.21 → v1.18.22 | `packages/opencode/src/server/routes/instance/httpapi/handlers/global.ts` + `groups/global.ts`（`/global/upgrade` 端点重构：响应改用 `HttpServerResponse.jsonUnsafe`、`GlobalUpgradeInput.target` 改为必填 semver（加 `Schema.makeFilter` semver 校验）、移除 `upgradeRaw`；**health 形状未变**——`GlobalHealth` schema 仍为 `{healthy: Schema.Literal(true), version: Schema.String}`、health handler 仍返回 `{healthy: true, version: InstallationVersion}`；**不影响 ocdeck 依赖的 health/event/SSE/session 契约**） |
| v1.18.22 → v1.18.23 | 无 |
| v1.18.23 → v1.18.24 | 无 |
| v1.18.24 → v1.18.25 | 无 |
| v1.18.25 → v1.18.26 | 无 |

**结论**：3 处 DIFF 均为 ocdeck 不依赖的端点（`/global/upgrade`）内部重构或内部助手逻辑变更，未触及 ocdeck 依赖的契约形状（health `{healthy,version}`、session CRUD、session/status、event SSE、permission、question、prompt_async PromptInput、/doc paths 键）。区间正式上调至 **[1.18.14, 1.18.26]**，`ContractBaseline="1.18.26"`，锚点计数 23 → **24**（新增 `packages/opencode/src/session/prompt.ts`）。

### live probe 记录（1.18.26，本机）

- 命令：`OPENCODE_SERVER_PASSWORD=probe-pw opencode serve --port 0 --hostname 127.0.0.1`（监听 127.0.0.1:4096）
- `GET /doc`（带 Basic Auth）→ 200；`paths["/session/{sessionID}/prompt_async"].post.operationId` == `"session.prompt_async"` ✅
- `GET /doc`（无 Auth，已配置密码）→ 401 ✅（认证中间件保护）
- `GET /session/status?directory=`（带 Auth）→ 200 ✅
- sessionID pattern 为 `^ses`；prompt_async 对不同 sessionID 的响应码矩阵（1.18.26，空目录与真实 repo 均验证一致）：
  - **格式合法但不存在** session id（如 `ses_does_not_exist`，满足 `^ses` 前缀）→ **404**（契约层 `http_response` 分类覆盖；404 分流穷尽规则由 lane 3 GetSession 处理）
  - **不满足 sessionID pattern** 的参数（如 `missing-session`，无 `ses` 前缀）→ **500**（参数校验异常，非"不存在 session"语义；契约层同样按 `http_response` 分类，不区分 404/500）
- requestBody.messageID schema pattern 核验为 `^msg` ✅

**结论**：1.18.26 的 prompt_async 端点契约与 1.18.18 源码锚点一致（路径键、operationId、messageID pattern）；GetSession 对格式合法的缺失 session 返回 404，lane 3 的 404 穷尽分流在该版本可正常触发。

## 契约锚点（24 个文件）

### Schema

- `packages/schema/src/v1/permission.ts`
- `packages/schema/src/v1/question.ts`
- `packages/schema/src/v1/session.ts`
- `packages/schema/src/v1/legacy-event.ts`
- `packages/schema/src/session-status-event.ts`
- `packages/schema/src/session-event.ts`
- `packages/core/src/event.ts`

### Server handlers

- `packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts`
- `packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts`
- `packages/opencode/src/server/routes/instance/httpapi/handlers/permission.ts`
- `packages/opencode/src/server/routes/instance/httpapi/handlers/question.ts`
- `packages/opencode/src/server/routes/instance/httpapi/handlers/global.ts`

### Server route groups

- `packages/opencode/src/server/routes/instance/httpapi/groups/session.ts`
- `packages/opencode/src/server/routes/instance/httpapi/groups/event.ts`
- `packages/opencode/src/server/routes/instance/httpapi/groups/permission.ts`
- `packages/opencode/src/server/routes/instance/httpapi/groups/question.ts`
- `packages/opencode/src/server/routes/instance/httpapi/groups/global.ts`

### Session status

- `packages/opencode/src/session/status.ts`
- `packages/opencode/src/event-v2-bridge.ts`
- `packages/opencode/src/session/prompt.ts`（diff-review-workbench 新增；PromptInput 类型与 messageID 编码）

### Attach CLI / TUI（单进程）

- `packages/opencode/src/cli/cmd/attach.ts`
- `packages/opencode/src/cli/tui/validate-session.ts`（单进程 `--session` 校验：形态合法但缺失的 `ses_` id 在 HTTP 就绪前退出，stderr `Error: Session not found`；不以 `ses` 开头的 id 只走本地 decode，报 `Invalid session ID`）
- `packages/opencode/src/cli/cmd/tui.ts`（external 分支判断：`--port`/`--hostname` 走真实 HTTP server）
- `packages/opencode/src/cli/tui/worker.ts`（external 模式下 `Server.listen` 启动路径）

## 端点契约摘要

完整字段与失败语义以本文档与 live probe 为准；归档 `design.md §20` 仅为历史记录，不再作为当前权威。此处只列 ocdeck 依赖的形状。

| 端点 | 关键形状 |
|---|---|
| `GET /global/health` | `{healthy, version}` |
| `GET /session?directory=&limit=` | `[]Session`，顶层 `id` / `time.updated` / `parentID` |
| `POST /session?directory=` | 单个 Session，同上 |
| `DELETE /session/:id?directory=` | **200 + JSON `true`**（非 204）；404 视为已删除 |
| `GET /session/status?directory=` | `{sessionID: {type}}`，`type ∈ {idle, busy, retry}` |
| `GET /event?directory=` (SSE) | envelope `{type, properties}`；首事件 `server.connected` |
| `GET /permission` | experimental pending 列表 |
| `GET /question` | experimental 列表 |
| `POST /session/{sessionID}/prompt_async?directory=` | body `{messageID, parts:[{type:"text",text}]}`；**204 = accepted**；其余状态码按 design.md D1 错误矩阵（详见上方 diff-review-workbench 锚点） |
| `GET /doc` | OpenAPI JSON；Basic Auth 保护（配置 `OPENCODE_SERVER_PASSWORD` 时无 Auth→401；未配置时无需认证）；能力探测解析 prompt_async 路径键 + operationId |

## 升级 SOP

1. 跑锚点 diff：

   ```bash
   scripts/check-opencode-contract.sh <oldRef> <newRef>
   # 例：scripts/check-opencode-contract.sh v1.18.18 v1.18.19
   ```

2. **相邻对核验**：扩展区间必须按相邻版本逐对 diff（例如 18→19、19→20、20→21），禁止从当前上限一次性跳到更远 tag（中间 tag 的改了又改回会被漏掉）。**零 DIFF**：把 `ContractBaseline` 上调到新版本；若向下扩区间则同时下调 `ContractMinVersion`。
3. **有 DIFF**：对照上表分析影响；必要时改 `internal/infrastructure/opencode`（漂移只改本包）与相关测试。
4. 手工启动 bare TUI external：`opencode --port <p> --hostname 127.0.0.1`（设好 `OPENCODE_SERVER_PASSWORD`），按脚本末尾 checklist live-probe：Basic Auth / health / session CRUD / status / SSE 首事件 / `--session` 恢复与校验失败语义。external 分支行为变化阻断区间扩展。
5. `go build ./... && go test ./...`。
