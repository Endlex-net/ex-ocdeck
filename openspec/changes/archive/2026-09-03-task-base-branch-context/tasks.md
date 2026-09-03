## 1. Backend: lifecycle branch env

- [x] 1.1 在 `internal/task` 新增 `baseBranchShortName(fullRef string) (string, bool)`：`refs/heads/<non-empty>` / `refs/remotes/<non-empty>` 去前缀；其它返回 false（D5）
- [x] 1.2 修改 `layerEnvSnapshot`：`repo` 注入 `OCDECK_TASK_BASE_BRANCH`/`OCDECK_TASK_HEAD_BRANCH`（先校验 D5）；`dir` 强制不注入两键；未知 kind 返回 internal error。repo 异常 `base_ref`/`branch` 返回 error，MUST NOT persist、MUST NOT 创建进程（D4/D5）
- [x] 1.3 修改 `loadEnvSnapshot`：快照缺失、JSON 非法、`vars` 缺失或 null 返回普通 error；拒绝 `vars == nil`，不得返回 nil map（D8）
- [x] 1.4 修改 `runRecoveryIncident`：每轮在 `runRecoveryAttempt`、permit、backoff 之前 `loadEnvSnapshot`；失败包装 `&recoveryTerminalError{err: newOpErr(codeInternal, err)}` 走既有终态分派。坏快照 MUST NOT `persistEnvSnapshot`、MUST NOT 写入更新后的 env map、MUST NOT `NewSession`、MUST NOT `AcquireRecoveryPermit`、MUST NOT backoff；既有终态补偿事务仍执行。有效 map 传入 `runRecoveryAttempt`（D8）
- [x] 1.5 修改 `runRecoveryAttempt`：接收已校验 map；仍以 `AcquireRecoveryPermit` 为首动作；仅覆盖 `OCDECK_SERVE_PORT` 后 persist；MUST NOT 调用 `layerEnvSnapshot` / `mergeEnvSnapshot`（D8）

## 2. Backend tests

- [x] 2.1 所有会到达 `layerEnvSnapshot` 的正常 repo fixture 默认带 `Branch` 与 `BaseRef=refs/heads/main`：至少 `seedSuspendedTask`/`seedActiveTask`，并审计手写 `TaskRow`（含 `lifecycle_phase3_test.go:409`）。异常路径再显式覆盖空/非法前缀
- [x] 2.2 `layerEnvSnapshot` 表驱动：heads → `OCDECK_TASK_BASE_BRANCH=main`；remotes → `origin/main`；dir 脏值仍缺两键；repo 异常 ref / 空 branch / 未知 kind → error 且不 persist
- [x] 2.3 快照含两键；用现有 mock process 断言 serve 与 `CreateShell` 环境含相同值
- [x] 2.4 Activate 异常行回到 suspended、无 `NewSession`、无新快照
- [x] 2.5 init 落 `init_status=failed` 且不运行脚本、不激活
- [x] 2.6 pre-delete：仅 `DeleteNormal` + 目录存在 + 非空脚本路径落 `deletion_failed`、不运行脚本、不删 worktree/任务行；`TestPreDelete_ScriptFails_NoWorktreeRemove` 补齐 `Branch`/`BaseRef` 并断言 `RunScript` 调用次数仍为 1；Force / 目录不存在 / 空脚本跳过路径 MUST NOT 因本变更调用 `layerEnvSnapshot`
- [x] 2.7 Recovery 同代快照：旧快照无新键则重拉后仍无；已有新键则保持原值，仅 `OCDECK_SERVE_PORT` 可更新；MUST NOT 调用 `layerEnvSnapshot`
- [x] 2.8 Recovery 坏快照（缺失、非法 JSON、`{"vars":null}`）：`AcquireRecoveryPermit` 调用次数为 0、无 backoff、无 `persistEnvSnapshot`、无更新后的 env map 写入、无 `NewSession`、无 panic；既有终态补偿事务仍执行

## 3. Frontend: branch rank and submit

- [x] 3.1 抽出纯函数 `rankBranchOptions(options, query)` 实现 D2 元组排序（同名命中、`origin/` 远端、原顺序下标），不依赖 React
- [x] 3.2 `NewTaskPanel` 维护 `idle|loading|ready|error` 与 `lastSuccessfulBranches`（D9）。选中 repo → loading + 初次 GET；成功写入 `lastSuccessfulBranches` 并 ready；失败 error 且无历史则列表为空。refresh 在途 MUST NOT 清空 `lastSuccessfulBranches`；成功覆盖之；失败保留 stale 展示并标注「本地快照未刷新」。仅 `ready` 计算基础候选与过滤首项。`canSubmit` 对 repo 另须 `ready`。stale MUST NOT 用于 `filteredBranches[0]` 或提交
- [x] 3.3 `submit`：repo 用 `filteredBranches[0]`（`base_ref = filteredBranches[0]`）；`ready` 且空列表时省略 `base_ref`；dir 不传 `base_ref`。统一 `normalizedInput = 输入框当前值.trim()`（D3）

## 4. Frontend tests

- [x] 4.1 `rankBranchOptions`：大小写不敏感同名命中；稳定 tie-break；`upstream/*` 不加第 2 键；输入 `master` 时 `origin/master` 第一
- [x] 4.2 预填 `main` 且存在 `origin/main` 时点创建提交 `origin/main`；任务名框 Enter 同路径
- [x] 4.3 基础候选 `["main","develop"]` 且输入 `  feature-x  ` 时提交 `feature-x`；基础候选 `["origin/main"]` 且 `normalizedInput=main` 时提交 `origin/main`（synthetic 不保证第一）；dir 省略 `base_ref`
- [x] 4.4 初次加载在途不发 POST；加载完成后提交 `origin/main`；初次加载失败列表为空且不提交；刷新失败保留旧列表、标注「本地快照未刷新」、禁止提交；刷新成功恢复提交
