# opencode v2 迁移实施建议：A+ 模式（单 server 多 session）

> 状态：建议草案 ｜ 依据：opencode v2.0.23（`@opencode/cli`，anomalyco/opencode v2 分支源码核实）
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
| 安装包 | `opencode-ai`（1.18.x） | `@opencode/cli`（≥2.0.23）；与 v1 同名 `opencode`，不能并存 |
| 启动命令 | `opencode --port <p> --hostname 127.0.0.1 [--session <id>]` | `opencode serve --port <p> --hostname 127.0.0.1`（根命令已无 `--port`） |
| HTTP 路径 | 裸路径（`/global/health`、`/session`、`/event`、`/doc`） | 统一 `/api` 前缀；健康检查 = `GET /api/info`（`{version,pid,urls}`）；OpenAPI 在 `/openapi.json` |
| SSE | `GET /event` | `GET /api/event`，首事件 `server.connected`；**volatile**：断线不重放、慢消费断流，客户端必须重连+自补状态 |
| 认证 | `OPENCODE_SERVER_PASSWORD`，可空 | **强制** Basic auth（user=`opencode`）；自管 serve 继续用 env 注入密码，语义不变 |
| 任务目录 | 进程 cwd（每任务一进程天然隔离） | `POST /api/session` body 带 `location: {directory: "<worktree>"}`；请求级可用 `x-opencode-directory` 头 |
| 任务 env | 进程 env 注入 | `PUT /api/session/:id/environment`（per-session，只覆盖该 session 的 shell 工具） |
| 权限配置 | `permission: {tool: "ask"}` | `permissions: [{action, resource, effect}]` 规则数组；`bash`→`shell`、`task`→`subagent`；建 session 可传 `permissions: Ruleset` |

**A+ 下变为 server 全局共享、需评估的**：credential 库（`/connect` 存的 provider key）、saved permission approvals（一处 "allow always" 全 server 生效）、Bus/后台 Job。若后续有任务间凭证隔离需求，按"凭证域分组，每组一个 server"演进。

## 3. 实施步骤

### 阶段 0：实机 spike（1–2 天，先行）
- 本机装 `@opencode/cli@2.0.23`，验证文档与源码的冲突点：`opencode --help`（确认根命令无 `--port`）、`serve` 默认端口/密码打印行为、`/api/info` 响应形状、`POST /api/session` 的 `location` 字段、`x-opencode-directory` 头、SSE 重连语义。
- 产出：精确的 v1↔v2 契约 diff 清单，回填本文档第 2 节。

### 阶段 1：契约层改造（核心工作量）
- `internal/infrastructure/opencode/`：base path 加 `/api`；健康检查改 `/api/info`；SSE 改 `/api/event` 并加重连与状态自补逻辑；事件 envelope 按 v2 形状更新（含 `permission.v2.asked` 双家族兼容可移除）。
- `POST /api/session` 增加 `location` 字段；新增 `PUT /api/session/:id/environment` 客户端方法。
- 更新 testdata fixture 为 v2 形状。

### 阶段 2：进程模型改造
- `internal/task/activate.go`：`runtimeCmdArgv` 改为 `serve` 形态；进程管理从"每任务一进程"改为"全局单 server 句柄 + 每任务建 session"。
- 任务目录绑定：建 session 时传 `location.directory=<任务 worktree>`；任务 env 从进程注入迁到 session environment API。
- tmux 托管模型简化：serve 进程一个托管单元即可（TUI attach 另议）。

### 阶段 3：契约治理切换
- `internal/infrastructure/opencode/CONTRACT.md`：24 个锚点重钉到 anomalyco/opencode **v2 分支**（注意仓库已从 sst 迁移）；新增锚点：`location` 字段、`x-opencode-directory`、`/api/info`、SSE volatile 语义。
- `scripts/check-opencode-contract.sh`：上游 raw URL 切 v2 分支；`ContractMinVersion`/`ContractBaseline` 上调（`client.go:28/31`）。
- live probe checklist 更新为 v2 端点。

### 阶段 4：灰度与回退
- v1/v2 不能同名并存：部署上以 ocdeck 版本为界整体切换，或安装脚本把 v2 二进制放到独立路径并参数化。
- 回退预案：保留 v1 契约层分支，切换期问题可整体回滚（v1 仍在维护，1.18.34）。

## 4. 明确不做（本期）

- **不接共享 service（方案 C）**：任务间 env/凭证隔离无法保证，且 daemon 生命周期不归我们管。
- **不做 OS 级沙箱**：v2 无内建沙箱；`shell` 包装器/hook 有文件工具盲区；Workspace driver 待官方成熟。后续单独立项评估。
- **不迁移到共享 credential store**：继续进程 env 注入 provider key（A+ 下即 server 进程 env，语义与 v1 一致）。

## 5. 主要风险

1. **官方文档滞后/与源码冲突**（cli.mdx flag 表、server.mdx 路径均为 v1 残留）：一切以 2.0.23 实机 + `/openapi.json` + v2 分支源码为准；阶段 0 的 spike 是硬性前置。
2. **SSE volatile**：断线丢事件，attention/permission 检测链路必须容忍重连缺口（重连后主动拉 session/status 对齐）。
3. **多 location 内存累积**：每个目录 boot 一份完整实例（Config/Provider/MCP/LSP/Watcher），长期运行的单 server 需观察内存；必要时按租户拆分 server。
4. **`--stdio` 握手模式**（方案 B）flag 未公开，本期不用；若日后想要随机端口+stdin EOF 关停，再钉版本评估。

## 6. 工作量估算

阶段 0 spike 1–2 天；阶段 1–3 核心改造集中在 `internal/infrastructure/opencode/` + `internal/task/` 少量调用点，量级 1–2 人周（含测试 fixture 与契约锚点重钉）；阶段 4 灰度视部署形态而定。复用现有 `check-opencode-contract.sh` SOP，风险可控。
