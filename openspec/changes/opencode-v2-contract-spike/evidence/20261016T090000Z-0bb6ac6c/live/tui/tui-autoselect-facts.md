# TUI attach 自动选中实证（opencode v2.0.28，tasks 1.4）

## 结论：可行。机制 = TUI 启动参数 `--session/-s`（root 命令），可配合 `--server <url>` attach 外部 server。

## 机制（源码锚点 commit 026b106b）
- CLI spec：`packages/cli/src/commands/commands.ts` root 命令 `--session/-s`（"Session ID to continue, or to create if it does not exist"）+ `--server`（"Connect to a server URL instead of the background service"）；`opencode mini --session`、`opencode run --session` 同语义。
- 装配：`packages/cli/src/commands/handlers/default.ts`
  - `findSession(client, session)` 判存在性（SessionNotFoundError → undefined）；
  - `args.sessionID = sessionExists ? session : undefined`（已存在 → TUI 启动即选中该 session）；
  - `args.newSessionID = sessionExists ? undefined : session`（不存在 → 该 ID 成为首个 prompt 所建 session 的 ID）。
- 二进制 `--help`（linux/arm64 v2.0.28，sha256 58fd4154…9670）输出与源码 spec 一致。

## live 实证（Docker 容器 spike-rerun-20261015T162223Z-2997862e，linux/arm64）
- 步骤：v2 serve（127.0.0.1:29732，OPENCODE_SERVER_PASSWORD 注入）→ POST /api/session 建标题 `tui-autoselect-probe` 的 session（ses_ebf084d6affebBY3dZWtfIFf5w，200）→ python pty（120x40）启动 `opencode --server http://127.0.0.1:29732 --session <sid>` → 采集 14s 渲染输出。
- 结果：serve_ready=True；anon=401；**捕获帧中出现目标 session 标题 2 次（title_rendered=True）** → TUI 启动即选中指定 session。
- 未渲染裸 sid（TUI 显示 title，不显示 id）——正常。

## 证据文件（外部 scratch 证据归档副本）

**Provenance**：本证据由独立 probe 容器 `spike-rerun-20261015T162223Z-2997862e`（linux/arm64 v2.0.28，二进制 sha256 58fd4154…9670）于 2026-10-16 产出，原生于 repo 外 scratch（`/var/folders/.../T/opencode/`），现归档至本 run 目录；非本 R2 run 原生证据，不受 R2 manifest 约束。

- 原始 TUI 渲染捕获：本目录 `tui-autoselect-capture.bin`（17174 bytes）
- 本记录：本目录 `tui-autoselect-facts.md`

## 约束记录
- v2 serve 无 OPENCODE_SERVER_PASSWORD 时会**自动生成随机密码**并打印 stdout（`server password …`），API 面始终 401 保护——客户端必须以 env 密码（Basic auth）连接。TUI 客户端从自身 env 读取同一密码。
- TUI 需要 TMPDIR 指向可执行目录（bun 单文件解包 .so 后 dlopen；noexec tmpfs 会失败）——本探测复用 spike sandbox/tmp。

## 冻结建议（供 orchestrator）
- ocdeck attach 形态：`opencode --server <url> --session <ses_id>`（密码经 OPENCODE_SERVER_PASSWORD env）；session 已存在 → 启动即选中；不存在 → 首个 prompt 采用该 ID 建 session（两种语义都已源码钉死并被 check-opencode-v2-contract.sh 断言覆盖）。
