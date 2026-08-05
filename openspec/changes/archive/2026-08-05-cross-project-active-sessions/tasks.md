## 1. 前置收敛：ctx-aware 水合调用链（design.md D0）

- [x] 1.1 `internal/process` 新增 `ShowSessionEnvContext(ctx, name, key)`：语义与 `ShowSessionEnv` 完全一致（校验、错误包装、`KEY=value` 解析、`ErrNoTmuxServer` 映射），遵守传入 ctx 且**内部以 `context.WithTimeout(ctx, 5s)` 封顶**（调用方更短 deadline 优先，无 deadline 调用方保留存量 5s 保护）；`ShowSessionEnv` 改为薄包装（`Background()` + 5s），对外行为不变
- [x] 1.2 `internal/task`：`ProcessBackend` 接口（types.go:67）新增 `ShowSessionEnvContext`；`ProcessAdapter`（adapters.go:319）委托实现；同步扩展 `mockProc` 及编译受影响的 task 包测试替身。**仅** `AgentStatus`（agent_status.go:21,25）的密码/端口两次 tmux 读取改走 ctx 变体；共享辅助 `recoverPassword` 与 suspend/attach/reconcile/delete 路径的全部 `ShowSessionEnv` 调用保持原样
- [x] 1.3 测试：ctx 取消/超时使水合提前返回；既有 `ShowSessionEnv` 行为回归（5s 上限、缺失返回 `("", nil)`、错误包装）；suspend/attach 存量路径行为回归

## 2. Store 查询（design.md D2）

- [x] 2.1 `internal/store/queries.go` 新增 `ActiveTaskOverviewRow` 与 `ListActiveTaskOverview(ctx)`：JOIN `projects × tasks` + LEFT JOIN `task_sessions`，`WHERE t.status='active'`，`last_active_at = COALESCE(MAX(CASE WHEN s.last_seen_at >= 100000000000 THEN CAST(s.last_seen_at/1000 AS INTEGER) ELSE s.last_seen_at END), t.updated_at)`（毫秒→秒归一化，design.md D2），GROUP BY task，`ORDER BY last_active_at DESC, t.id ASC`
- [x] 2.2 store 层测试：多项目聚合、**混合单位排序（真实 13 位毫秒 last_seen_at vs 秒 updated_at 混排，含毫秒实际更早/更晚两个方向，毫秒数据必须 ≥1e11 真正触发归一化分支）**、**同一 task 秒/毫秒 session 混合（验证逐行归一化先于 MAX）**、无会话行回退 `updated_at`、非 active 任务排除、排序与 tie-breaker（`internal/store/store_test.go`）

## 3. Task 层 read model（design.md D1）

- [x] 3.1 `internal/task` 新增 `ActiveTaskOverviewRow` 镜像类型；`TaskStore` 接口（manager.go:28）与 `StoreAdapter`（adapters.go）新增 `ListActiveTaskOverview`；`Manager` 新增同名薄委托方法
- [x] 3.2 同步扩展 `internal/task/mock_test.go` mockStore 及编译受影响的 task 包测试替身；**mockStore.ListActiveTaskOverview 语义必须精确镜像生产 SQL（含毫秒归一化、存在会话行时仅取会话 MAX 而非 max(updated_at, 会话值)）**
- [x] 3.3 task 层测试：Manager 委托、adapter 类型转换

## 4. API 端点（design.md D3/D4）

- [x] 4.1 `TaskBackend` 接口（api/tasks.go:33）新增 `ListActiveTaskOverview`；同步扩展全部 fake backend（`agent_status_api_test.go`、`git_api_test.go`、`p3_review4_fixes_test.go` 等，编译即发现）。task 层 adapter 落点已在 3.1（`internal/task/adapters.go`）；API 层无新增 store adapter
- [x] 4.2 新增 `GET /api/v1/sessions/active` handler + DTO（`task_id/project_id/project_name/name/branch/worktree_path/last_active_at/agentStatus,omitempty`），注册路由（`server.go` registerRoutes），鉴权复用 `/api/v1/*` Bearer 中间件；store 查询失败返回 500 且不水合；空结果初始化空切片保证编码为 `[]`
- [x] 4.3 实现水合 worker（design.md D4 伪代码）：每请求 semaphore 上限 8 + DB 完成后 3s 水合预算；read model 先整体转 DTO 切片，每 goroutine 只写自己的 `out[i]` DTO 槽位；`wg.Wait()` 后构造响应
- [x] 4.4 API 层测试（沿用 `agent_status_api_test.go` fake 模式）：正常返回、单任务水合失败降级、**并发上限 ≤8（channel barrier 确定性等待 8 个调用入场，确认第 9 个在释放前无法入场，不用 Sleep）**、**水合预算（fake 校验收到的 deadline ≈3s 并立即返回 + 短 parent ctx 单独验证取消传播，不做 2.9–3.5s 墙钟窗口）**、**客户端请求取消场景**、store 错误 500 不水合、401、空数组 `[]` 非 `null`（body 只读一次后复用字符串断言）、字段与排序断言、task read model 在水合前后不变（防止 agentStatus 回填领域投影）
- [x] 4.5 修正 `internal/task/agent_status.go` 顶部过期注释（"最近 session" → 全部 session 聚合）

## 5. 前端页面（design.md D5）

- [x] 5.1 `web/src/api.ts` 新增 `listActiveSessions()`，`web/src/types.ts` 新增 `ActiveSessionItem`
- [x] 5.2 新增 `ActiveSessionsPage`：固定 5s 轮询 + single-flight（在途请求未完成则跳过 tick）；三态区分（初次 loading / 失败保留旧数据 + 错误提示 / 空态引导文案）；行展示项目名/任务名/分支/最近活跃相对时间/复用 `AgentStatusBadge`；点击行跳转 `#/task/:id`
- [x] 5.3 `web/src/App.tsx` 注册 `#/active` 路由；`ProjectsPage` 顶部加"活跃会话"入口

## 6. 验证

- [x] 6.1 `go test ./internal/process/... ./internal/store/... ./internal/task/... ./internal/api/...` 通过；并发相关包执行 `go test -race ./internal/process/... ./internal/task/... ./internal/api/...`（覆盖水合 worker、ctx 收敛）
- [x] 6.2 前端 `pnpm build` 通过后 `go build ./...` 整体验证（`web/embed.go` 依赖 dist）
- [ ] 6.3 手动冒烟：两个项目各激活一个任务，`#/active` 列表排序/水合/跳转/空态/轮询不重叠符合 spec
