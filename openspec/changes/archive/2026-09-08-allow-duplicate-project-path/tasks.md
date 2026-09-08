# Tasks: allow-duplicate-project-path

依赖与交付顺序：1.1–1.3 共同完成后执行 1.4–1.7；2.1–2.2 配套完成后执行 2.3；第 3 节可与前两节并行实施；第 4 节在全部实现完成后执行。0014 SQL、runner no-FK 支持与 API 改动 MUST 同版本交付（design Migration Plan）。

## 1. 存储层：migration 0014 + runner no-FK 支持（design D3）

- [x] 1.1 新增 `internal/infrastructure/store/migrations/0014_projects_path_not_unique.sql`：按 design D3 的 SQL 逐字实现（CREATE projects_new 无 UNIQUE → INSERT SELECT → DROP projects → RENAME projects_new TO projects），列序 `id, name, path, default_branch, created_at, kind`，`kind TEXT NOT NULL DEFAULT 'repo'`
- [x] 1.2 `internal/infrastructure/store/store.go` runner 新增 no-FK 迁移登记表（`migrationsRequiringForeignKeysOff = map[int]struct{}{14: {}}`，带注释）；事务外引导：只读查询 `sqlite_schema` 判断 `schema_version` 是否存在（不存在视为空集合、查询出错返回错误、存在时读取失败返回错误）
- [x] 1.3 runner 事务主体：begin 后在 tx 内保留 `CREATE TABLE IF NOT EXISTS schema_version`，全部待执行 migration 按版本顺序执行并记录版本，共用一个事务；待执行集合含 no-FK 版本时 begin 前 `PRAGMA foreign_keys=OFF`（否则维持既有 ON）；仅当本次关闭了 FK 时，commit 前在 tx 内执行 `PRAGMA foreign_key_check`（有行或出错回滚报错）；commit/回滚后恢复 FK ON 并读回验证，恢复失败返回错误
- [x] 1.4 迁移测试（走真实 runner `Migrate`，参照 `applyMigrationsUpto` 构造 0013 库）：存量项目+任务+`project_env_vars`+`project_lifecycle_configs` 数据逐行保留、子表 FK 目标仍为 `projects`、迁移后子表可正常写入、`path` 可插入重复值、迁移后 `foreign_key_check` 无违规、FK 恢复 ON
- [x] 1.5 失败路径测试：0014 执行失败/完整性违规 → 回滚（schema_version 未记录 14、旧表数据不变）且 FK 恢复 ON
- [x] 1.6 空库测试：从零建库应用全部 migration 成功；空库迁移中途失败回滚后 `schema_version` 与业务 DDL 均不残留
- [x] 1.7 runner 正常路径回归：已是版本 14 的库重启幂等跳过，行为不变

## 2. 后端 API：移除重复 path 拒绝（design D1 + D2）

- [x] 2.1 `internal/api/projects.go` 删除 `:249-252` 预检与 `:256-258` `isUniqueViolation` 分支及 `:464` `isUniqueViolation` 函数；保持 D1 主流程与失败边界不变（前置校验失败不调 CreateProject；创建失败 500；回读失败 500 不回滚）
- [x] 2.2 删除 `GetProjectByPath` 全链：`internal/infrastructure/store/queries.go:151`、`internal/api/server.go:80` 接口、`internal/api/store_adapter.go:39` 实现、`internal/api/projects_test.go:53` fake；`internal/infrastructure/sqlite/adapter_session_test.go:28` 改用 `GetProject(ctx, "p1")`
- [x] 2.3 创建接口测试：同 path 二次注册 201（repo+repo、dir+dir、repo/dir 交叉——交叉场景路径为合法 git 仓库），且两次创建的 id 不同、两条记录均可按 id 回读、path 均为预期 canonical path、kind 各自正确；不同输入归一到同一 canonical path 的场景同样注册成功；重复 path 但不满足既有校验仍拒绝（非 git 仓库以 kind=repo 重复注册 → 422）；响应 DTO 无新增字段

## 3. 前端：注册表单重复路径提醒（design D4 + spec「注册表单重复路径提醒」）

- [x] 3.1 `web/src/pages/ProjectsManagePage.tsx` `RegisterForm` 消费 `useProjects()`，派生 `dupProjects`（`path.trim()` 为空则不匹配；否则 `p.path === candidate` 字符串全等、不按 kind 过滤、无其他归一化）
- [x] 3.2 `dupProjects.length > 0` 时在路径输入字段下方渲染 `od-alert od-alert-info`（复用既有样式），文案列出已注册该路径的项目名称；提交按钮 disabled 逻辑不变（提醒不阻塞提交）
- [x] 3.3 前端测试：输入重复 path 提醒出现且含项目名；修改为不重复提醒消失；提醒展示中可正常提交；输入带前后空白 trim 后命中；输入仅空白不提醒

## 4. 验证

- [x] 4.1 后端测试：`go build ./...` 通过；受影响包 `go test -count=1 ./internal/api/ ./internal/infrastructure/store/ ./internal/infrastructure/sqlite/` 全绿，新增行为测试提供有效性证据（mutation M1-M4 验证：恢复 UNIQUE / 移除 no-FK 登记 / 禁用 foreign_key_check / 恢复 409 预检均使对应测试变红）。全量 `go test ./...` 存在 2 个已确认的环境性例外（`internal/task` 端口绑定测试 `TestAllocatePort_ExcludeSkipsLastPortAndScan`、`TestPortRetry_SnapshotMissingDegradedContinuesRotate`，50000 段端口被本机无关进程占用，已在 HEAD 干净 worktree 复现，与本次改动无关）
- [x] 4.2 前端测试通过（web 目录测试命令），新增测试同样提供有效性证据
- [x] 4.3 `openspec validate` 通过
