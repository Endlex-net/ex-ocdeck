# Release Note: single-process-opencode

> 变更：`openspec/changes/single-process-opencode/`（design.md D1-D8 / specs delta / tasks）

## 模型变更摘要

每个任务的 opencode 运行时从**双进程**（`serve` + `attach`，`ocdeck-<taskID>-serve` / `-tui` 两个 tmux 会话）切换为**单进程**：

- 启动命令 `opencode --port <p> --hostname 127.0.0.1`（有锚定 session 时追加 `--session <id>`），托管于 `ocdeck-<taskID>-runtime` 单一 tmux 会话；TUI 与 HTTP API/SSE 控制面同进程（external 模式，与 `opencode serve` 同一 `Server.listen` 实现）。
- 内存收益：每任务消除 attach 进程（约 115-300MB）。
- **自动重拉**：任务进程异常退出（用户 TUI exit / 崩溃）后自动恢复——固定预算（滚动 5 分钟最多 3 次进程创建，退避 5s/15s/45s）内重拉并恢复锚定 session；预算耗尽落挂起并记录 last_error。
- **恢复中语义**：恢复期间打开终端收到 HTTP 409（application 错误码 `recovering`）+ WebSocket close code **1013**（Try Again Later）；前端轮询任务状态，回到 active 后自动重连，恢复期统一显示「进程启动中」。
- 旧版 `-serve` / `-tui` 会话在新版启动 reconciliation 中一律按异常会话清理（不支持热迁移恢复双进程运行时）。

## 部署前提（必须）

1. **升级前挂起全部任务。** 新版不支持从旧版双进程布局热迁移：启动 reconciliation 会把旧 `-serve`/`-tui` 会话清理掉，任务以单进程模型重新激活。
2. 部署新版服务端并重启；启动 reconcile 自动完成对账（persist 模式下仅 active 且 runtime 健康、无 cleanup debt 的任务原地恢复，其余按矩阵收敛）。
3. 任务手动激活，进入单进程模型。

## 回滚步骤

回滚到旧版（双进程）前，**必须按顺序**执行，不得在新版服务仍运行时直接 `kill-server`（watcher 会把 `-runtime` 缺失当成故障并自动重拉）：

1. **挂起全部任务**（停止自动重拉的触发源）。
2. **停止新版 ocdeck-server**（进程退出后 watcher 不再重拉）。
3. **清理 ocdeck 专属 tmux socket**。生产 socket 不在默认 `/tmp`：`TMUX_TMPDIR=<dataDir>/tmux`（`cmd/ocdeck-server/main.go` 经 `process.EnsureTmpDir` 拼为 `<dataDir>/tmux`；tmux 实际文件为 `$TMUX_TMPDIR/tmux-<uid>/ocdeck`）。`dataDir` 默认 `$HOME/.ocdeck`，启动时若设置了 `OCDECK_DATA_DIR` 则用该目录：

```sh
# dataDir 默认 $HOME/.ocdeck；自定义来源：OCDECK_DATA_DIR
TMUX_TMPDIR="${OCDECK_DATA_DIR:-$HOME/.ocdeck}/tmux" tmux -L ocdeck -f /dev/null kill-server
```

4. **部署并启动旧版**。

或者：在完成步骤 1–2 后，以 `kill_on_start` / `kill_immediate` shutdown 模式启动一次旧版（启动时清理全部 ocdeck 会话）后再切回 persist 模式。

原因（tasks 6.2 验证结论）：旧版代码的会话名 parser 只识别 `serve` / `tui` / `shell` 后缀，对 `-runtime` 后缀的会话在 persist 模式启动对账时被分组阶段静默丢弃——既不恢复也不清理，`opencode --port` 进程会残留并继续占用端口与内存；kill 模式则无差别清理全部会话。裸命令 `tmux -L ocdeck kill-server` 连的是默认 socket（无 `TMUX_TMPDIR`、无 `-f /dev/null`），清不到生产会话。

回滚后任务重新激活即回到双进程模型（旧版数据无破坏性迁移；`anchor_session_id` 列等新增 schema 对旧版只读无害）。

## 契约与验证基线

- opencode 契约锚点 23 个（新增 `cli/cmd/tui.ts`、`cli/tui/worker.ts`）；`scripts/check-opencode-contract.sh` 数量断言已同步。
- 验证基线：`go build ./... && go test ./... && go test -race -count=1 ./internal/task/ ./internal/api/`；`cd web && pnpm build && pnpm test`。
