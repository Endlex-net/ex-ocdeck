## Context

ocdeck server 为单实例多项目架构：所有项目、任务、会话归属数据均在单个 SQLite（`<dataDir>/ocdeck.db`）中（`projects` / `tasks` / `task_sessions`，见 `internal/store/migrations/0001_init.sql`）。当前前端只能按 `#/`（项目列表）→ `#/project/:id`（任务列表）→ `#/task/:id`（工作台）逐层钻取，没有全局视角。

已有的三类"活跃"信号：

1. `tasks.status='active'`（`internal/task/types.go`）：ocdeck 管理的 serve 进程在跑，不代表 agent 正在干活。
2. `agentStatus`（idle/busy/retry，`internal/task/agent_status.go:15` `Manager.AgentStatus`）：实时查询该任务 opencode serve 的 `/session/status`，仅对 active 任务有意义；失败降级为 `""`。现有调用点（`internal/api/tasks.go:131,170`）为逐任务串行。
3. `task_sessions.last_seen_at`：由 SSE 事件与全量 align 刷新（`internal/store/queries.go:456` UpsertTaskSession，MAX 语义），按 DESC 排序（`queries.go:472`）。

约束：

- 单 SQLite 连接（modernc.org/sqlite），查询须轻量、不阻塞写路径。
- 每个 agentStatus 水合 = 对该任务 serve 的一次 HTTP 调用 + 两次 tmux 环境读取；跨项目场景 active 任务数可能较多，必须并发 + 超时 + 降级。
- **现有 `AgentStatus` 调用链不遵守 ctx**：`recoverPassword`（`internal/task/suspend.go:316`）丢弃传入 ctx；`process.ShowSessionEnv`（`internal/process/process.go:450,457`）内部用 `context.Background()` + 固定 5s 超时。没有前置收敛，水合硬超时无法兑现。
- 前端无 SSE 状态推送，统一 `usePoll` 轮询（`web/src/hooks.ts:4`）；`usePoll` 当前不阻止请求重叠，新页面须自行 single-flight。
- 路由与鉴权沿用现有模式：`/api/v1/*` Bearer 中间件（`internal/api/server.go`），hash router（`web/src/App.tsx`）。
- 项目无 Fx，无新增构造接线；生产链已在 `cmd/ocdeck-server/main.go` 装配，扩展的是接口方法而非新组件。

## Goals / Non-Goals

**Goals:**

- `GET /api/v1/sessions/active`：按 task 粒度返回全部 `status='active'` 任务，含项目名/分支/最近活跃时间/`agentStatus`，按最近活跃时间倒序。
- 最近活跃时间 = 该任务 `task_sessions.last_seen_at` 的 MAX；无会话行时回退 `tasks.updated_at`。
- agentStatus 水合并发执行（每请求 semaphore 上限 8）、DB 查询完成后起 3s 水合预算，单个任务失败/超时降级为字段缺省，不影响整体响应。
- 前端独立页面 `#/active`：固定 5s 轮询 + single-flight，点击行跳转 `#/task/:id`；`#/` 项目列表页加入口。
- 行为以新 capability spec（`active-sessions-overview`）固化。

**Non-Goals:**

- 不做列表行快捷操作（suspend/archive/删除仍在任务工作台内）。
- 不展示 suspended 任务（包括"最近活跃的非 active 任务"）。
- 不做 SSE/WS 实时推送；不改动现有任务详情/项目详情页行为。
- 不展开 opencode session 子粒度（列表行为 task）。
- 不做跨 ocdeck 实例聚合（单数据目录单实例已有 flock 保证）。
- 不引入通用查询框架或全局水合限流器；并发上限为每请求局部语义。

## Decisions

### D0（前置收敛）: ctx-aware 水合调用链

**现状问题**：`Manager.AgentStatus(ctx, ...)` 的 ctx 只约束 DB 与 opencode HTTP 调用；中间两次 tmux 环境读取（密码 + 端口）走 `ShowSessionEnv`，其内部 `context.Background()` + 5s 固定超时（`process.go:457`），`recoverPassword` 直接丢弃 ctx（`suspend.go:316-322`）。N 个任务 × 最坏 5s+ 的不可取消 tmux 调用会击穿任何水合预算。

**决策**：`process.Manager` 新增 `ShowSessionEnvContext(ctx, name, key)`，语义与 `ShowSessionEnv` 完全一致（同名校验、同错误包装、同 `KEY=value` 解析、同 `ErrNoTmuxServer` 映射），遵守传入 ctx；**内部再以 `context.WithTimeout(ctx, 5s)` 封顶**——调用方更短的 deadline（如活跃列表 3s 水合预算）照常优先生效，而无 deadline 的调用方（既有项目任务列表/任务详情端点的请求 ctx）获得与改造前相同的 5s 保护，不丢失存量上限。`ShowSessionEnv` 保留为薄包装（`Background()` + 5s），对外行为不变。同步扩展 task 层接口闭环节点：`ProcessBackend`（`internal/task/types.go:67`）新增 `ShowSessionEnvContext`、`ProcessAdapter`（`adapters.go:319`）委托实现、`mockProc` 及编译受影响的 task 包测试替身。

**改造范围严格限定为 `AgentStatus` 一个调用方**：`AgentStatus` 内的两次 tmux 读取（密码 `agent_status.go:21`、端口 `agent_status.go:25`）改走 ctx 变体。**共享辅助 `recoverPassword`（被 suspend.go:217、attach_shell.go:51 共用）与 reconcile/delete/suspend/attach 的全部 `ShowSessionEnv` 调用保持原样**，这些存量路径的取消语义、超时上限与错误行为一字不动。

**收敛期间必须保持的不变量**：

- `ShowSessionEnv` 对外签名与行为（含 5s 上限、`("", nil)` 缺失语义）不变，全部既有调用方（suspend/attach/reconcile/delete）不受影响。
- `AgentStatus` 聚合不变量不变：`busy > retry > idle`；status map 缺项视为 idle；凭据/端口/session 列表/HTTP 任一不可用返回 `""`。
- 纯机制改造，不改变任何状态机、不写 DB。

### D1: 调用链与 read model

概览查询必须经既有分层暴露，handler 不直接依赖 store：

```
handler (api)
  └─ TaskBackend.ListActiveTaskOverview(ctx)          // internal/api/tasks.go:33 接口扩展
       └─ task.Manager.ListActiveTaskOverview(ctx)    // 薄委托，无业务逻辑
            └─ TaskStore.ListActiveTaskOverview(ctx)  // internal/task/manager.go:28 接口扩展
                 └─ StoreAdapter.ListActiveTaskOverview // internal/task/adapters.go 类型转换
                      └─ store.Queries.ListActiveTaskOverview // internal/store/queries.go
```

类型：store 层 `ActiveTaskOverviewRow`（专用投影，不复用 `TaskRow`，避免空字段歧义）；task 层同名镜像 row + adapter 转换（沿用 `adapters.go` 既有模式）。需同步扩展的测试替身：`internal/task/mock_test.go` mockStore、API 层各 fake backend（`agent_status_api_test.go`、`git_api_test.go`、`p3_review4_fixes_test.go` 等实现 `TaskBackend` 的类型）。

### D2: Store 查询

`internal/store/queries.go` 新增 `ListActiveTaskOverview(ctx)`：

```sql
SELECT t.id, t.project_id, p.name AS project_name, t.name, t.branch,
       t.worktree_path,
       COALESCE(
         MAX(CASE
           WHEN s.last_seen_at >= 100000000000
           THEN CAST(s.last_seen_at / 1000 AS INTEGER)
           ELSE s.last_seen_at
         END),
         t.updated_at
       ) AS last_active_at
FROM tasks t
JOIN projects p ON p.id = t.project_id
LEFT JOIN task_sessions s ON s.task_id = t.id
WHERE t.status = 'active'
GROUP BY t.id
ORDER BY last_active_at DESC, t.id ASC
```

**单位归一化（实测确认）**：`task_sessions.last_seen_at` 直接持久化 opencode `time.updated`（`activate.go:897`，OpenCode 1.18.9 实测为**毫秒**，如 `1785797826297`）；`tasks.updated_at` 为 `nowUnix()`（**秒**）。存量库两种单位混存，不归一化则：①毫秒值（≈1.78e12）恒排在秒值（≈1.78e9）前，排序失真；②违反 API 的 Unix 秒契约；③前端相对时间被钳为"刚刚"。逐行 CASE 归一化（阈值 1e11：当前秒值 ≈1.78e9、毫秒值 ≈1.78e12，留足裕量）在 MAX 之前完成，读侧处理同时兼容存量与新写入数据，不触碰写路径。`session_created_at`/`first_seen_at` 同样混存，但本查询不使用。

无 schema 变更，无新索引需求（`tasks.status` 低基数、行数小；`task_sessions` 已有 `task_id` 前缀主键）。

### D3: 端点、DTO 与错误语义

新增 `GET /api/v1/sessions/active`（Bearer 鉴权，注册于 `internal/api/server.go` registerRoutes）。响应 JSON 数组：

```json
{
  "task_id": "...", "project_id": "...", "project_name": "...",
  "name": "...", "branch": "...", "worktree_path": "...",
  "last_active_at": 1690000000,
  "agentStatus": "busy"
}
```

`agentStatus` 降级时省略（`omitempty`，与 `taskRowDTO.AgentStatus` 惯例一致，`api/tasks.go:325`）。

错误语义表：

| 情况 | 行为 |
|---|---|
| store 查询失败 | 标准错误信封 500，**不开始水合** |
| 无 active 任务 | 200 + `[]`（初始化空切片，MUST NOT 编码为 `null`） |
| 单任务水合失败/超时 | 该元素 `agentStatus` 缺省，其余不受影响 |
| 客户端请求取消 | ctx 取消传播，水合终止，不保证响应 |

响应为**查询时刻快照**：查询完成后任务被 suspend 的，由下一轮轮询收敛，不在本请求内重做。

### D4: 水合 worker 算法

```text
rows = backend.ListActiveTaskOverview(ctx)        // task read model，DB 受 request ctx 约束
out  = toActiveSessionDTOs(rows)                  // 先整体转换为 API DTO 切片
hctx, cancel = WithTimeout(ctx, 3s)               // 水合预算自 DB 完成后起算；defer cancel()
sem = make(chan struct{}, 8)                      // 每请求并发上限
wg  = sync.WaitGroup{}
for i := range out:
    wg.Add(1)
    go func(i):
        defer wg.Done()
        select { case sem <- struct{}{}: defer release; case <-hctx.Done(): return }
        out[i].AgentStatus = backend.AgentStatus(hctx, out[i].TaskID)  // 只写自己的 DTO 槽位
wg.Wait()                                          // D0 保证 deadline 后快速排空
return out                                         // 超时未完成的槽位 agentStatus 为 ""（omitempty 省略）
```

task read model（`ActiveTaskOverviewRow`）不声明 `agentStatus` 字段；该字段只存在于 API DTO（`activeSessionDTO`），水合 worker 只写 DTO 切片，read model 全程不可变。

关键性质：

- 每个 goroutine 只写 `out[i]` 自己的 DTO 槽位，无共享 map、无数据竞争；迟到结果在 `wg.Wait()` 前要么已落槽要么 goroutine 已返回，响应在 `Wait` 之后构造，不存在"handler 返回后 worker 写响应"的窗口。
- 8 是**每请求**上限；前端 single-flight + 5s 轮询保证同客户端不叠加请求，无需进程级限流。
- 硬超时依赖 D0：ctx 全链路遵守后，`wg.Wait()` 在 deadline 后只剩排空开销（opencode HTTP 受 `OpTimeout` 与 ctx 双重约束，tmux 调用受 ctx 约束）。

备选：在 `task.Manager` 新增批量水合方法。拒绝理由：`AgentStatus` 已是单任务原子能力，批量并发属 API 编排层职责，保持 task 包不膨胀。

### D5: 前端页面

- 路由：`web/src/App.tsx` 增加 `#/active` → 新 `ActiveSessionsPage`。
- 数据：`api.ts` 新增 `listActiveSessions()`；`types.ts` 新增 `ActiveSessionItem`。
- 轮询：固定 5s；**single-flight**（上一请求未返回则跳过本次 tick；`usePoll` 不防重叠，页面本地 guard 或扩展 hook——实现时选最小改动方案；guard 必须在请求结束的 `finally` 中释放，避免一次失败后永久停止轮询）。
- 三态区分：初次 loading（无数据且请求中）/ 请求失败（有错误提示，保留上次成功数据）/ 真正空态（请求成功且数组为空 → 引导文案 + 回项目列表链接）。
- 行渲染：项目名 / 任务名 / 分支 / 最近活跃相对时间 / 复用现有 `AgentStatusBadge`；点击整行跳转 `#/task/:id`。
- 入口：`ProjectsPage` 顶部加"活跃会话"链接/按钮。

### 纯读不变量（MUST）

本特性全链路为只读：SQL SELECT、tmux `show-environment`、opencode `GET /session/status`。实现 MUST NOT：写 DB、触发 session align、改变任务状态机、启动/停止任何进程。

## Risks / Trade-offs

- [agentStatus 水合放大延迟：N 个 active 任务 = N 次 (tmux×2 + HTTP)] → 每请求有界并发 8 + 3s 水合预算 + D0 ctx 收敛 + 单点降级；前端 5s single-flight 轮询避免请求叠加。
- [last_seen_at 只反映 SSE 观察到的会话事件，agent 空闲但 serve 存活的任务时间戳可能偏旧] → 可接受：排序语义即"最近有过活动"，与"跟进任务"意图一致；spec 中明确该语义。
- [轮询期间任务被 suspend，行内状态过期] → 快照语义（D3），由下一轮轮询收敛，点击跳转后工作台页权威展示。
- [扩展 `TaskBackend`/`TaskStore` 宽接口导致多个 fake 编译失败] → tasks.md 已列全影响点，编译即发现，机械补齐。

## Migration Plan

纯增量：无 schema 变更、无配置变更、无 API 破坏。部署即替换二进制与前端静态资源；回滚 = 回退版本，遗留数据无影响。

## Open Questions

（无——文档评审问题已全部闭环：read model 走 Manager；3s 为 DB 后的水合预算；接受 D0 ctx-aware 前置收敛；前端固定 5s + single-flight。）
