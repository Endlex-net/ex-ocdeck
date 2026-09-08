# Design: allow-duplicate-project-path

## Context

用户需求：同一目录允许注册多个项目（repo/dir 均放开），创建时仅告警不拒绝（当前 `POST /api/v1/projects` 对重复 path 返回 409 `project already registered for path`）。

现状（已核实）：

- 后端拒绝链：`internal/api/projects.go:249-252` 创建前 `GetProjectByPath` 预检返回 409；`:256-258` `isUniqueViolation` 兜底 409。`isUniqueViolation` 全仓仅 `projects.go:256` 一处调用（定义在 `:464`）。
- 存储层唯一约束：`internal/infrastructure/store/migrations/0001_init.sql:17` `path TEXT NOT NULL UNIQUE`（列级内联约束）。SQLite 无法通过 ALTER 删除列级 UNIQUE，需重建表。
- projects 表最终列序：`id, name, path, default_branch, created_at`（0001）+ `kind`（0008 ALTER 追加，`:6`）。无其他 ALTER。
- 引用 projects(id) 的子表：`tasks`、`project_env_vars`（0001:24,43）、`project_lifecycle_configs`（0007:10-11），均 `ON DELETE CASCADE`。
- migration runner：`internal/infrastructure/store/store.go:109` 连接级 `PRAGMA foreign_keys=ON`（tx 外）；`:113-160` 全部待执行 migration 共用一个事务（`:113` begin，`:140-158` 循环逐文件 `tx.Exec` 并记录版本，`:160` 统一 commit）。SQLite 的 `PRAGMA foreign_keys` 在事务内为 no-op，migration 文件内无法临时关闭 FK 强制。
- FK ON 下重建被引用表的危险（已用系统 sqlite3 实测复现）：`ALTER TABLE projects RENAME TO projects_old` 会将子表 FK 引用改写指向 `projects_old`（`PRAGMA legacy_alter_table=ON` 不能阻止该改写），随后 `DROP TABLE projects_old` 的隐式 DELETE 触发 `ON DELETE CASCADE`，存量 tasks 等子表数据被级联清空。因此重建 MUST 在 FK 关闭下进行。
- `GetProjectByPath` 生产代码唯一调用点即上述预检；其余为接口/适配器/测试：`internal/api/server.go:80`、`internal/api/store_adapter.go:39`、`internal/api/projects_test.go:53`（fake）、`internal/infrastructure/store/queries.go:151`、`internal/infrastructure/sqlite/adapter_session_test.go:28`（仅用于 seed 幂等判断，可替换为 `GetProject(ctx, "p1")`）。
- SQLite 驱动：modernc.org/sqlite v1.39.1。
- 前端注册表单：`web/src/pages/ProjectsManagePage.tsx` `RegisterForm`（`:490-646`），路径输入为受控 state（`:612-622` `value={path} onChange`）；已有 `od-alert od-alert-info` 提醒样式先例（`:591` dir 类型提示）；共享 projects store `useProjects()`（`web/src/hooks.ts:222`）提供全量 `Project[]`，`Project.path` 为后端 canonical 归一后的路径（`web/src/types.ts:15`）；`RegisterForm` 当前仅用 `useProjectsRefresh`（`:502`），父组件 `ProjectsManagePage` 已消费 `useProjects`（`:683`）。

## Goals / Non-Goals

**Goals:**

- 同一 canonical path 可注册多个项目（repo/dir 均允许），创建 API 对重复 path 返回 201 成功。
- 拆除 `projects.path` 唯一约束，存量数据完整保留，子表外键关系不受损。
- 注册表单输入路径时，与已注册项目 path 重复即实时提醒，不阻塞提交。

**Non-Goals:**

- 不新增后端查重接口；创建 API 响应不新增 warning 字段。
- 前端查重为字符串相等比对，不处理 symlink 别名归一（接受漏检，用户已确认）。
- 不改变 path 的 canonical 归一（绝对化 + EvalSymlinks）与存储语义。
- 不改动项目删除、详情、任务创建等其他流程。

## Decisions

### D1: 移除后端重复 path 拒绝（预检 + 兜底一并删除）

删除 `projects.go:249-252` 的 `GetProjectByPath` 预检与 `:256-258` 的 `isUniqueViolation` 分支；`isUniqueViolation` 函数（`:464`）全仓无其他调用方，一并删除。schema 迁移后 path 无唯一约束，兜底分支成为不可达死代码。

创建主流程（既有结构不变，仅删除重复 path 拒绝分支）：

```
decode+字段校验(422) → canonical 归一(422) → kind 局部校验(repo: git 校验+默认分支探测 / dir: 目录校验; 422|git_error)
  → CreateProject(失败 → CodeInternal 500) → GetProject 回读(失败 → CodeInternal 500) → 201
```

副作用与失败边界（既有语义保留， MUST 不因本变更改变）：

- 任一前置校验失败 MUST NOT 调用 `CreateProject`，MUST NOT 产生项目记录。
- `CreateProject` 失败返回 `CodeInternal` 500（`:260` 既有路径）。
- 创建成功后 `GetProject` 回读失败仍返回 `CodeInternal` 500，已写入记录不额外回滚（`:263-266` 既有语义）。
- 成功响应不新增 warning 字段，DTO 形状不变。

### D2: 删除 GetProjectByPath 全链

path 不再唯一后，「按 path 查单行」语义未定义（返回哪一行不确定）。其唯一生产用途（唯一性预检）已随 D1 移除，故删除整条链而非保留歧义 API：

- `internal/infrastructure/store/queries.go:151` 查询方法
- `internal/api/server.go:80` 接口声明、`internal/api/store_adapter.go:39` 适配器实现
- `internal/api/projects_test.go:53` fake 实现
- `internal/infrastructure/sqlite/adapter_session_test.go:28` seed 幂等判断改用 `GetProject(ctx, "p1")`

### D3: migration 0014 重建 projects 表移除 UNIQUE（runner 支持 no-FK 迁移）

新增 `0014_projects_path_not_unique.sql`（下一可用编号，现最新为 0013）。因 SQLite 无法删除列级 UNIQUE 约束，且 FK ON 下 rename 重建会导致子表级联清空（见 Context 实测结论），重建 MUST 在 FK 关闭下按「建新表 → 复制 → 删旧表 → 新表改名」执行。

**runner 改动**（`internal/infrastructure/store/store.go`）：新增显式 no-FK 迁移登记表（如 `migrationsRequiringForeignKeysOff = map[int]struct{}{14: {}}`，带注释说明原因）。`runMigrations` 流程调整为：

1. 事务外引导读取已应用版本：只读查询 `sqlite_schema` 判断 `schema_version` 表是否存在——不存在（全新库）视为已应用版本集合为空；查询出错 MUST 返回错误，MUST NOT 当作空集合；存在时读取版本，读取失败 MUST 返回错误。据此列出待执行迁移。
2. 若待执行集合含 no-FK 登记版本 → 在 begin 之前 `PRAGMA foreign_keys=OFF`（连接级、事务外，此时有效）；否则维持既有 `PRAGMA foreign_keys=ON`。
3. begin 后在 tx 内保留既有 `CREATE TABLE IF NOT EXISTS schema_version`（失败原子性不变：全新库迁移失败回滚后版本表不残留），随后单事务内按序执行全部待执行 migration 并记录版本。
4. 若第 2 步关闭了 FK → commit 前在 tx 内执行 `PRAGMA foreign_key_check`；返回任何行或查询出错 MUST 回滚并报错。
5. commit 后（含失败回滚后）MUST 恢复 `PRAGMA foreign_keys=ON` 并读回验证；恢复失败 MUST 返回错误、拒绝启动。

**0014 SQL**（在 FK OFF 下执行）：

```sql
CREATE TABLE projects_new (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    path          TEXT NOT NULL,
    default_branch TEXT,
    created_at    INTEGER NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'repo'
);
INSERT INTO projects_new (id, name, path, default_branch, created_at, kind)
    SELECT id, name, path, default_branch, created_at, kind FROM projects;
DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;
```

机制依据（均已核实）：

- 子表（tasks / project_env_vars / project_lifecycle_configs）始终引用表名 `projects`；本流程不将旧 `projects` 重命名，而是在 FK OFF 下删除旧表、再将 `projects_new` 改名为 `projects`，因此子表引用保持正确：FK OFF 使 `DROP TABLE projects` 的隐式 DELETE 不触发级联，被改名的 `projects_new` 不是任何子表的引用目标。
- FK OFF 期间的写入完整性由 commit 前 `foreign_key_check` 兜底（违反则回滚）。
- 列序与现网迁移后结构一致（`id, name, path, default_branch, created_at, kind`，kind 为 0008 追加列）；`kind` 保留 `NOT NULL DEFAULT 'repo'`（与 0008 一致）。
- 新表不含 path 唯一约束，也不新建 path 索引（现有查询均按 id；`GetProjectByPath` 已删除）。

失败语义：migration 任一语句失败或 `foreign_key_check` 非空 → 单事务整体回滚、FK 恢复为 ON、启动失败（与既有迁移语义一致），不产生半迁移状态。

### D4: 前端注册表单实时重复提醒

`RegisterForm` 新增 `useProjects()` 消费共享 store（单例，父组件已在用，无额外请求）。派生：

```ts
const trimmedPath = path.trim();
const dupProjects = trimmedPath
  ? projects.filter((p) => p.path === trimmedPath)
  : [];
```

- `dupProjects.length > 0` 时在路径输入字段下方渲染 `od-alert od-alert-info`（复用 `:591` 既有样式），文案列出已注册该路径的项目名称，如：`该路径已注册项目「A」「B」，将继续创建独立的新项目`。
- 受控 `onChange` 天然实现「输入即提醒、修正即消失」，无防抖/额外副作用。
- 不阻塞提交：提交按钮 disabled 逻辑（`:639`）不变。

## Risks / Trade-offs

- [symlink 别名漏检：前端字符串比对无法识别 `/tmp` vs `/private/tmp` 这类别名] → 用户已确认接受；后端 canonical 归一保持不变，数据层不出现别名重复行之外的歧义。
- [重建表迁移误触级联删除子表数据] → FK OFF 下重建 + commit 前 `foreign_key_check` 兜底；迁移测试验证数据保留与 FK 恢复（见 Test Strategy）。
- [`GetProjectByPath` 删除波及面] → 全仓调用点已枚举（D2），无生产代码残留依赖。
- [runner 为 no-FK 迁移引入分支逻辑] → 显式登记表硬编码版本 14，影响面收敛在 `runMigrations`；正常路径（无 no-FK 待执行迁移）行为完全不变。

## Test Strategy

后端：

- `handleCreateProject`：同 path 二次注册返回 201（repo+repo、dir+dir、repo/dir 交叉——交叉场景路径本身须为合法 git 仓库）；重复 path 但不满足既有校验仍拒绝（如非 git 仓库路径以 kind=repo 重复注册 → 422）；fake store 移除 `GetProjectByPath`。
- migration 0014 升级测试（走真实 runner `Migrate`，参照 `task_mode_migration_test.go` 的 `applyMigrationsUpto` 构造 0013 库）：0013 库插入项目+任务+`project_env_vars`+`project_lifecycle_configs` 数据 → 应用 0014 → 断言数据逐行保留（含子表）、子表 FK 目标仍为 `projects`、迁移后子表可正常写入、`path` 可插入重复值、迁移后 `PRAGMA foreign_key_check` 无违规、连接 FK 恢复为 ON。
- 失败路径：构造 0014 执行失败/完整性违规场景 → 断言回滚（schema_version 未记录 14、旧表数据不变）且 FK 恢复为 ON。
- 全新空库：从零建库成功应用全部 migration（含 0014）；空库迁移中途失败 → 回滚后 `schema_version` 与业务 DDL 均不残留。
- runner 正常路径回归：无 no-FK 待执行迁移时（如已是 14 的库重新启动）行为与既有完全一致（幂等跳过）。

前端（匹配规则以 spec「注册表单重复路径提醒」为准）：

- 注册表单测试：输入与 store 项目重复 path → 提醒出现且含项目名；修改为不重复 → 提醒消失；提醒展示中提交 → createProject 正常调用。
- 匹配规则边界：输入带前后空白（如 `" /repo "`）→ trim 后命中提醒；输入仅空白 → 不匹配不提醒。

## Migration Plan

代码、migration 0014 与 runner no-FK 支持同版本发布；启动时自动应用。提交前迁移或完整性检查失败 → 整体回滚并拒绝启动；提交成功后 FK 恢复或读回验证失败 → 拒绝启动，但已提交的 schema 与版本记录保留（下次启动幂等跳过 0014）。无单独回滚策略（schema 前向迁移，发布包内含全部历史 migration）。
