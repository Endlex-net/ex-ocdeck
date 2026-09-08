# Design: host-env-sync-and-display

## Context

ocdeck-server 常由服务管理器启动（Ubuntu: systemd user service `contrib/ocdeck.service`；macOS: brew services/launchd），进程环境极简。当前宿主环境读取的唯一入口是 `os.LookupEnv`（`internal/task/notice.go:16`、`internal/api/env.go:138`），follow_host 解析（`internal/task/activate.go:139`）在进程环境查不到变量时跳过不注入，导致用户在 shell 配置中 export 的变量无法到达任务进程。全代码无任何主动捕获用户环境的机制。

现有补救通道：`internal/config/config.go:266-282` `ApplyEnvFile`（启动时把 `~/.config/ocdeck/env` 写入进程环境），需手动维护且改后须重启 server。

> 注：本节为原始问题背景。实现已全部落地（hostenv 包、D1 接线收窄、列举/刷新 API、展示区 UI），见 tasks.md 完成记录。

## Goals / Non-Goals

**Goals:**
- follow_host 增加 login shell 环境兜底解析，systemd/launchd 启动场景下可用
- 提供宿主环境变量列举 API（合并视图 + 来源标注）与手动刷新 API
- 设置页新增系统环境变量展示区（掩码、搜索、一键 follow_host、刷新）

**Non-Goals:**
- 不改变 env file 语义；不改变任务 env 快照生效时机（挂起后激活）
- 不改变注入层叠优先级与**基础集白名单及其取值来源**（基础集继续只取进程环境）
- 不支持 Windows（现仓库已限定 darwin/linux）

## Decisions

### D1: 宿主环境解析器收口到 `internal/infrastructure/hostenv` 包（范围收窄）

调整现有 `hostenv` 包（初版已存在），导出 `Lookup(key) (string, bool)` 与 `Refresh() error`。接线按**调用用途**划分（不能按共用函数整体替换）：

**接 hostenv.Lookup**（进程环境未命中时兜底捕获缓存）：

- `internal/task/activate.go` `layerEnvSnapshot` 内的 follow_host 分支（`case "follow_host"` 值解析处）
- `internal/api/env.go` `hostEnvLookup`（→ 全局列表 resolvedValue 展示）
- 新增列举 API 的合并视图数据源

**保持进程环境读取（os.LookupEnv）不变**：

- `internal/task/activate.go` `layerEnvSnapshot` 内的 `envBaselineKeys` 基础集循环、LANG/LC_ALL/LC_CTYPE locale 判断
- `internal/task/attach_shell.go` `userShell()` 的 SHELL 读取
- `internal/task/notice.go` `hostEnv` 保持 `os.LookupEnv`（初版曾被整体替换为 `hostenv.Lookup`，已收窄恢复），供上述调用点继续使用
- `cmd/ocdeck-server/main.go` 两处 `process.DefaultBaseEnv(os.LookupEnv)`

理由：`DefaultBaseEnv` 在 server 启动时（组合根）立即遍历基础集，接线后启动即触发捕获，违背懒加载契约；基础集若取捕获值会出现"启动时固定值"与"刷新后新值"两个时间版本。基础集、locale 判断、userShell 的来源与默认行为不变是本 change 的不变量。

解析顺序：进程环境（非空）→ login shell 捕获缓存（非空）→ 未命中。

**调用顺序不变量**：任务激活路径上，项目/任务形态校验（kind/mode/base_ref/branch）MUST 先于任何可能触发捕获的宿主解析。非法任务拒绝路径 MUST NOT：触发捕获或 Refresh、启动 login shell、发布捕获缓存、持久化新快照、创建任务进程（既有 init/pre_delete 落账不受影响）。

### D2: login shell 捕获协议

- 懒加载：首次兜底未命中 / 首次列举 / 首次刷新时触发，不在 server 启动时执行。
- 命令（`/usr/bin/env` 显式路径，避免同名 shell 函数劫持协议）：

  ```sh
  printf '\n__OCDECK_ENV_BEGIN__\n' && /usr/bin/env -0 && printf '\n__OCDECK_ENV_END__\n\0'
  ```

  shell 取 `$SHELL`；为空时按平台回退：darwin 固定 `/bin/zsh`；linux 先查 `/etc/passwd` 中当前用户的登录 shell（用户可能使用 zsh 等非 bash shell），查不到再回退 `/bin/bash`。以 `-ilc` 执行。Bash 交互式 login shell 读取 login profile（`.bashrc` 是否加载取决于该 profile 是否 source 它）；zsh 按自身启动文件规则读取——遵循所选 shell 的启动文件规则，不保证执行所有 rc 文件。
- **字节级帧语法**：定位独立成行的 BEGIN 标记（取首个出现）后，其后内容按 NUL 切分为完整记录。**END 为独立的 NUL 终止控制记录**：仅当某条完整 NUL 记录精确等于 `\n__OCDECK_ENV_END__\n` 时帧结束。合法环境记录必含 `=`，与该无 `=` 控制记录无歧义——键或值中即使含完整标记文本的记录也不构成帧边界，原样保留。END 控制记录之后允许存在尾部内容（交互 shell 退出钩子输出），忽略。
- **成功判据（同时满足，缺一即失败）**：未超时；退出码为零；存在 BEGIN 行且其后帧以 END 控制记录完整结束（缺 END 控制记录、缺终止 NUL、输出截断均判失败）；帧内条目可解析。
- 条目解析与合法性：NUL 分隔；按第一个 `=` 切分（值可含 `=` 与换行，含标记文本原样保留）；**无 `=` 或键为空的记录忽略**；**合法 `KEY=` 空值条目 MUST 保留在捕获缓存中**（键存在性用于列举 source 计算；仅 `Lookup` 的有效值判断将空串视为未命中）；重复键取首条。**集合级成功判据**：忽略后零个合法条目 → 本次捕获判失败（MUST NOT 发布"成功空缓存"）。判定示例：整帧仅 `garbage\0` → 失败；`=value\0`（空键）→ 忽略该记录；`KEY=\0` → 保留空值键。
- 超时 5s（context.WithTimeout）：超时后 MUST kill shell 进程及其进程组，防止后代持有输出管道导致等待悬挂。stderr 丢弃，stdin 不连接。
- 失败 MUST NOT 发布部分结果或"成功空结果"；失败只记一行 warning 日志，绝不输出捕获内容（键或值）。

### D3: 缓存状态机与刷新语义

缓存三态：`未加载 → 成功缓存 / 失败缓存`。

| 事件 | 未加载 | 成功缓存 | 失败缓存 |
|---|---|---|---|
| 普通读取（Lookup/列举） | 触发捕获，按结果迁移 | 直接用缓存 | 直接用缓存（=空集），**不自动重试** |
| 显式 Refresh 成功 | 迁移为成功缓存 | 原子替换为新缓存 | 原子替换为新缓存 |
| 显式 Refresh 失败 | 迁移为失败缓存，返回 error | **保留旧成功缓存**，返回 error | 保留失败缓存，返回 error |

「刷新失败保留旧缓存」仅适用于已有缓存（成功/失败态）；未加载态首次 Refresh 失败 MUST 迁移为失败缓存，后续普通读取按失败缓存服务、不再次捕获，仅下一次显式 Refresh 可重试。

- 刷新只影响后续 follow_host 解析与列举，**不触碰已激活任务快照**，不更新 process manager / watchdog / tmux 的基础环境。
- 并发安全：读写锁；替换为原子发布；进行中的首次捕获与 Refresh 须协调，后到者不得用旧结果覆盖新缓存。

### D4: 列举与刷新 API

在 `registerEnvRoutes`（`internal/api/env.go:176-189`）新增：

- `GET /api/v1/env/host` → `{ "vars": [{ "key", "value", "source" }] }`，`source ∈ {"process", "shell", "both"}`。首次调用在缓存「未加载」时触发捕获（前端需 loading 态）。
- `POST /api/v1/env/host/refresh` → 调 `hostenv.Refresh()`；成功返回最新合并视图（同 GET 结构）；失败返回既有内部错误形态（`HTTP 500, code=internal`，固定文案，不含捕获内容）；缓存状态迁移严格遵循 D3（已有缓存保留，未加载态迁移为失败缓存）。

**合并规则**（区分键存在性与有效值命中）：

- key 集 = 进程环境键 ∪ 捕获缓存键；`source` 按两侧键**存在性**计算。
- `value` 依次取：进程环境非空值 → 捕获缓存非空值 → 空串。即 `source=both` 时值不一定来自进程（进程为空串时取捕获值），与 follow_host"空视为未命中"的解析顺序严格一致。

**降级**：无可用成功缓存且捕获失败时，GET 正常返回仅含进程环境变量的视图（不报错）。

**值经 API 明文返回**：与既有 `resolvedValue` 明文展示一致（个人自用、本地服务）；UI 默认掩码用于减少屏幕可见暴露。

### D5: 系统环境变量展示区 UI

新组件 `web/src/components/SystemEnvPanel.tsx`，在 `SettingsPage.tsx:110-118` env tab 的「全局环境变量」`od-card` section **下方**新增一个 `od-card` section 承载。行为：

- 列表行：key、定长掩码值（`••••••••`，不泄露值长度）、来源 badge（进程/Shell/两者）、操作按钮；点击行值区域切换该行明文/掩码（行级状态）。
- 顶部搜索输入框，按 key 子串过滤。
- 刷新按钮：调 POST /env/host/refresh，进行中禁用并 loading；**失败时保留原列表、显示错误、解除 loading**，仅成功时替换列表。**刷新成功后同时触发全局环境变量列表重新加载**（follow_host 的 `resolvedValue` 可能因新捕获而变化，复用兄弟组件共享 reload 信号）；刷新失败不触发该联动。
- 每行「添加」按钮：调既有 `api.putGlobalEnv(key, 'follow_host', '')`（`GlobalEnvEditor.tsx:98` 同一接口），既有后端校验（`internal/api/env.go` `envKeyValid`/`envKeyReserved`）继续生效。**添加仅限：符合全局变量 key 校验、非保留键（`OCDECK_*` 前缀与 `OPENCODE_SERVER_PASSWORD`）、尚未配置的变量**。按钮状态优先级：已配置（标「已配置」）→ 保留键（标「系统保留」）→ 非法 key（标「非法 key」）→ 可添加；前三态禁用并分别说明原因，对应行仍正常展示（不违反键并集契约）。添加成功后刷新全局变量列表——`SystemEnvPanel` 与 `GlobalEnvEditor` 为兄弟组件，由 `SettingsPage` env tab 持有共享 reload 信号（回调或 key 递增）同步两个列表。
- 复用现有 `env-list`/`env-row`/`badge` 样式族。
- 展示区固定展示说明文案：「刷新更新宿主解析结果；任务需挂起后激活生效。」（不依赖全局列表的临时 mutation 提示状态）

### D6: 主流程决策表（follow_host 解析）

```text
Lookup(KEY)
  ├─ 进程环境非空？ ── 是 → 返回进程值
  ├─ 读捕获缓存（未加载则触发 D2 捕获）
  │    ├─ 缓存中 KEY 非空？ ── 是 → 返回捕获值
  │    └─ 否则 → 未命中（follow_host 跳过，不注入空值）
  └─ 缓存为失败态 = 空集，不自动重试（仅显式 Refresh 重试）
```

拒绝路径副作用表（激活校验失败时）：

| 动作 | 允许？ |
|---|---|
| 触发捕获 / Refresh / 启动 login shell | 禁止 |
| 发布捕获缓存 | 禁止 |
| 持久化新快照 / 创建任务进程 | 禁止（canonical spec 既有） |
| init/pre_delete 失败落账 | 允许（canonical spec 既有） |

## Risks / Trade-offs

- [交互式 shell 配置怪异导致捕获超时/输出垃圾] → 5s 进程组级超时 + 帧校验四判据 + 失败不发布。
- [`env -0` 不存在或失败时尾标记仍输出，误判成功空结果] → `&&` 链：env 失败则 END 不输出且退出码非零。
- [捕获把用户全部 shell 环境（含密钥）读进 server 内存] → 仅内存缓存不落盘；日志红线；API 明文返回与既有 resolvedValue 同级暴露，UI 掩码仅减少屏幕可见暴露。
- [刷新后用户误以为运行中任务立即生效] → spec 已明确快照时机不变；展示区固定展示「任务需挂起后激活生效」说明（D5）。
- [GET /env/host 首次调用延迟（shell 启动）] → 前端 loading 态；后续调用走缓存。
