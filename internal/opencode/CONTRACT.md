# opencode 契约区间

ocdeck 对 opencode 的兼容性按**已验证版本区间**声明，当前为 **[1.18.14, 1.18.18]**。

- `ContractMinVersion`（`1.18.14`）：区间下限
- `ContractBaseline`（`1.18.18`）：区间上限 / 最近一次核验版本

版本检查**仅告警**（启动日志 + `/api/v1/server/status` 的 `versionVerified`），**不是激活门禁**。真正的激活门禁是能力探测：`GET /global/health` 可达、`GET /session/status` 结构、session 列表字段形状（DELETE 形状不做 live 探测，首次真实删除时校验）。区间外的版本仍可尝试激活；探测失败才阻止。

区间内各版本的 21 个契约锚点文件已逐对核验为字节一致。扩展区间前必须再跑一遍锚点 diff + live probe。

## 契约锚点（21 个文件）

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

### Attach CLI

- `packages/opencode/src/cli/cmd/attach.ts`
- `packages/opencode/src/cli/tui/validate-session.ts`

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

## 升级 SOP

1. 跑锚点 diff：

   ```bash
   scripts/check-opencode-contract.sh <oldRef> <newRef>
   # 例：scripts/check-opencode-contract.sh v1.18.18 v1.18.19
   ```

2. **相邻对核验**：扩展区间必须按相邻版本逐对 diff（例如 18→19、19→20、20→21），禁止从当前上限一次性跳到更远 tag（中间 tag 的改了又改回会被漏掉）。**零 DIFF**：把 `ContractBaseline` 上调到新版本；若向下扩区间则同时下调 `ContractMinVersion`。
3. **有 DIFF**：对照上表分析影响；必要时改 `internal/opencode`（漂移只改本包）与相关测试。
4. 手工启动 `opencode serve --port <p> --hostname 127.0.0.1`（设好 `OPENCODE_SERVER_PASSWORD`），按脚本末尾 checklist live-probe：health / session CRUD / status / permission / question。
5. `go build ./... && go test ./...`。
