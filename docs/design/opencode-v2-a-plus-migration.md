# opencode v2 迁移实施建议：A+ 模式（单 server 多 session）

> 状态：spike 已收口（2026-10-14）｜版本锁定：v1 基线 **1.18.26**（commit `774cc7c1`）、v2 目标 **2.0.28**（update-API 渠道核验，github sha `044d8b7d`）
> Spike 结论与证据：`openspec/changes/opencode-v2-contract-spike/design.md`「Spike 结论」节；证据归档 `openspec/changes/opencode-v2-contract-spike/evidence/`（最终 run `20261014T042602Z-90e9d6ed` + 官方升级路径验证 `cutover-check-20261014T043705Z` PASS）
> 注意：spike 后锁定的方向是 **v1/v2 双 provider 长期并存**（任务创建时钉死 providerID，存量任务视为 v1）；本文第 3 节阶段 4 的「整体切换」建议与该方向冲突，已标注为历史草案（见该节）。
> 前置阅读：`internal/infrastructure/opencode/CONTRACT.md`、`docs/release-notes/single-process-opencode.md`

## 1. 目标与拓扑

**现状（v1）**：每个任务 spawn 一个独立 `opencode --port <p>` 进程（`internal/task/activate.go:1072`），tmux 托管，进程 env 注入 `OPENCODE_SERVER_PASSWORD`。

**目标（A+）**：一个自管 `opencode serve` 进程承载全部任务，每任务一个 session，经 `location.directory` 绑定各自 worktree。

```
ocdeck-server
  └─ spawn: opencode serve --port <p> --hostname 127.0.0.1   （全局唯一，Effect.never，无 idle 退出）
       ├─ session A  → location.directory=/path/worktree-A   （任务 A）
       ├─ session B  → location.directory=/path/worktree-B   （任务 B）
       └─ ...不同 session 并发执行；同 session 串行（run-coordinator 按 sessionID 分片）
```

## 2. v1 → v2 关键契约变化（改造清单）

| 维度 | v1（现状） | v2（目标） |
|---|---|---|
| 安装包 | `opencode-ai`（1.18.x） | `@opencode/cli`（锁定 **2.0.28**）；与 v1 同名 `opencode`，不能并存 |
| 启动命令 | `opencode --port <p> --hostname 127.0.0.1 [--session <id>]` | `opencode serve --port <p> --hostname 127.0.0.1`（根命令已无 `--port`） |
| HTTP 路径 | 裸路径（`/global/health`、`/session`、`/event`、`/doc`） | 统一 `/api` 前缀；健康检查 = `GET /api/info`（`{version,pid,urls}`）；OpenAPI 在 `/openapi.json` |
| SSE | `GET /event` | `GET /api/event`，首事件 `server.connected`；**volatile**：断线不重放、慢消费断流，客户端必须重连+自补状态 |
| 认证 | `OPENCODE_SERVER_PASSWORD`，可空 | Basic auth（user=`opencode`）；spike 实证：env 注入密码时与 v1 语义一致（supervisor.02 PASS）；**未设置密码时 serve 日志打印随机密码**（非拒绝启动），自管 serve 必须显式注入 |
| 任务目录 | 进程 cwd（每任务一进程天然隔离） | `POST /api/session` body 带 `location: {directory: "<worktree>"}`；请求级可用 `x-opencode-directory` 头 |
| 任务 env | 进程 env 注入 | `PUT /api/session/:id/environment`（per-session，只覆盖该 session 的 shell 工具） |
| 权限配置 | `permission: {tool: "ask"}` | `permissions: [{action, resource, effect}]` 规则数组；`bash`→`shell`、`task`→`subagent`；建 session 可传 `permissions: Ruleset` |

**A+ 下变为 server 全局共享、需评估的**：credential 库（`/connect` 存的 provider key）、saved permission approvals（一处 "allow always" 全 server 生效）、Bus/后台 Job。若后续有任务间凭证隔离需求，按"凭证域分组，每组一个 server"演进。

## 3. 实施步骤

### 阶段 0：实机 spike（1–2 天，先行）

**已完成（2026-10-14）**：隔离 Docker 环境对锁定基线（v1=1.18.26 / v2=2.0.28）执行完整 spike，协议差异清单与承接结论见 `openspec/changes/opencode-v2-contract-spike/design.md`「Spike 结论」节。对本节原有验证点的实证修正：

- `opencode --help` / `serve` 形态：实证确认根命令不收 `--port`，裸 `opencode` 进 TUI；`serve` 不接受 `--session` flag。
- 健康检查：本文原写 `GET /api/info` 未经 spike 验证；实证可用就绪信号是 `/api/session`（无凭证 401 / 带凭证 200）——`/global/health` 在 v2 返回 SPA HTML，不可作就绪判定。
- OpenAPI：`/doc` 在 v2 是公开 SPA HTML（匿名 200），真实 OpenAPI 在 `/openapi.json`。
- SSE：`/api/event` 首事件 `server.connected` 实证确认；volatile 重连语义未实证（conn.01 INCONCLUSIVE，需带模型环境补证）。
- spike 新增实证差异（本文第 2 节表格未覆盖）：响应统一 `{data}` envelope；prompt 提交为 `POST /api/session/{sessionID}/prompt` body `{text,id}`（无 prompt_async/parts）；事件名 `session.updated`→`session.renamed`，事件载荷归属键 `properties.*`→`data.*`；permission 端点为 `/api/permission/request` + `/api/permission/saved`；v1 的 `GET /session/status` 在 v2 移除（候选替代：`Session.Info.outcome` + SSE 状态事件，schema 依据存在、运行时等价性待证）；存储从文件树改为 SQLite `opencode.db`。完整清单以 design.md「Spike 结论」差异表为准。
- 官方升级路径：v1 数据目录同机换 v2 二进制启动后旧 session 可见、消息原文一致（`evidence/cutover-check-20261014T043705Z/` PASS）——数据割接采信官方升级路径，ocdeck 不做自研数据搬运。

### 阶段 1：契约层改造（核心工作量）
- `internal/infrastructure/opencode/`：base path 加 `/api`；就绪判定用 `/api/session`（401/200；`/api/info` 未经实证，勿用——`/global/health` 在 v2 是 SPA HTML）；SSE 改 `/api/event` 并加重连与状态自补逻辑；事件 envelope 按 v2 形状更新（`data` 归属键、`session.renamed` 事件名；`permission.v2.asked` 双家族兼容可移除）。
- `POST /api/session` 增加 `location` 字段；新增 `PUT /api/session/:id/environment` 客户端方法。
- 更新 testdata fixture 为 v2 形状。

### 阶段 2：进程模型改造
- `internal/task/activate.go`：`runtimeCmdArgv` 改为 `serve` 形态；进程管理从"每任务一进程"改为"全局单 server 句柄 + 每任务建 session"。
- 任务目录绑定：建 session 时传 `location.directory=<任务 worktree>`；任务 env 从进程注入迁到 session environment API。
- tmux 托管模型简化：serve 进程一个托管单元即可（TUI attach 另议）。

### 阶段 3：契约治理（双 provider 并存口径）

- **v1 契约保留不动**：`internal/infrastructure/opencode/CONTRACT.md` 与 `scripts/check-opencode-contract.sh` 继续钉 v1 基线（1.18.26），服务存量 v1 provider。
- **v2 单独适配**：为 v2 provider 新建契约锚点集（钉 v2.0.28 / commit `026b106b`），新增锚点：`location` 字段、`x-opencode-directory`、`/api` 前缀、`/openapi.json`、SSE volatile 语义、`session.renamed`/`data` 归属键、`/api/permission/request|saved`、`serve` 子命令启动形态。不得把 v1 锚点整体上调替换。

### 阶段 4：灰度与回退

> **历史草案**（与 spike 后锁定的「v1/v2 双 provider 长期并存、任务创建时钉死 providerID」方向冲突，保留仅供参考，不作为当前实施方案）：v1/v2 不能同名并存：部署上以 ocdeck 版本为界整体切换，或安装脚本把 v2 二进制放到独立路径并参数化。回退预案：保留 v1 契约层分支，切换期问题可整体回滚（v1 仍在维护，1.18.34）。

当前方向的灰度与回退以双 provider 并存为前提，具体方案由 v2 provider 实现 change 定义。

## 4. 明确不做（本期）

- **不接共享 service（方案 C）**：任务间 env/凭证隔离无法保证，且 daemon 生命周期不归我们管。
- **不做 OS 级沙箱**：v2 无内建沙箱；`shell` 包装器/hook 有文件工具盲区；Workspace driver 待官方成熟。后续单独立项评估。
- **不迁移到共享 credential store**：继续进程 env 注入 provider key（A+ 下即 server 进程 env，语义与 v1 一致）。

## 5. 主要风险

1. **官方文档滞后/与源码冲突**（cli.mdx flag 表、server.mdx 路径均为 v1 残留）：一切以锁定基线 2.0.28 实机 + `/openapi.json` + v2 分支源码为准；阶段 0 的 spike 已完成（2026-10-14），后续实现以 spike 差异清单为**已验证事实的起点**——清单中的候选映射与待验证项按其标注补证后方可作为契约依据。
2. **SSE volatile**：断线丢事件，attention/permission 检测链路必须容忍重连缺口；重连后的状态对账**不能拉 `session/status`**（v2 已移除该端点）——候选项为 `GET /api/session/{id}` 的 `outcome` 字段 + SSE 状态事件，运行时等价性待实现阶段实证。
3. **多 location 内存累积**：每个目录 boot 一份完整实例（Config/Provider/MCP/LSP/Watcher），长期运行的单 server 需观察内存；必要时按租户拆分 server。
4. **`--stdio` 握手模式**（方案 B）flag 未公开，本期不用；若日后想要随机端口+stdin EOF 关停，再钉版本评估。

## 6. 工作量估算

阶段 0 spike 1–2 天；阶段 1–3 核心改造集中在 `internal/infrastructure/opencode/` + `internal/task/` 少量调用点，量级 1–2 人周（含测试 fixture 与契约锚点重钉）；阶段 4 灰度视部署形态而定。复用现有 `check-opencode-contract.sh` SOP，风险可控。
