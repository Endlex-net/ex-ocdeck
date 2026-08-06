# Design: fix-git-diff-new-file-and-linenum

## Context

任务界面 git 面板数据流（已核实）：

```
git CLI (exec.go:82 run, 子命令白名单 exec.go:14-30, argv 无 shell)
  → internal/git/ops.go:13 Status / :155 Diff（原始 unified diff 文本 + numstat）
  → internal/task/gitops.go:60 GitStatus / :101 GitDiff（task 锁, DTO :13-35）
  → internal/api/git.go:43 handleGitStatus / :57 handleGitDiff
  → GET /api/v1/tasks/{id}/git/status|diff?ref=&path=
  → web/src/api.ts:184-193 → GitPanel.tsx
     groupFiles :15-27（untracked 组 ref=''）/ openDiff :84-96 / diff2html :136-143
  → styles.css:1043-1193 diff 样式 + :1489-1622 ≤1024px 断点
```

两个 bug 根因：

- **Bug 1**：`ops.go:168-175` 只跑 `git diff [oid] -- [:(literal)path]`，git 对 untracked 文件永远输出空。前端 untracked 组可点击但只能得到空 diff → 显示"该文件暂无可展示的 diff"（GitPanel.tsx:261-263）。
- **Bug 2**：无虚拟滚动，diff2html 大 table 一次性渲染。gutter 行号即每行 table cell（diff2html 3.4.56 gutter 内部含绝对定位元素）。`styles.css:1121-1125` 强制 `font-size:12px` 但未设 `line-height`（body 全局 `line-height:1.45` 生效）；`.d2h-wrapper{overflow:hidden}`（`:1081-1086`）裁切横向溢出；`≤1024px` 时 `.git-diff` 切为 `overflow:visible`（`:1619-1622`）、滚动容器切到 `.git-panel`（`:1602-1605`）。横向滚动 owner 不唯一 + 行高不一致 → gutter 与代码行错位。

### 已实测的 git 外部契约（Apple Git 2.39.5，`git diff --no-index -- /dev/null <path>`）

| 输入 | exit code | stdout | stderr |
|---|---|---|---|
| 非空新文件 | 1 | 完整 unified diff（含 `new file mode` + hunks） | 空 |
| 空新文件 | 1 | 仅文件元数据 diff（`new file mode`，无 hunk） | 空 |
| 路径不存在 | 1 | 空 | `error: Could not access '<path>'` |
| 其他错误（如权限） | >1 | 视情况 | 错误信息 |

结论：**exit=1 本身不代表成功也不代表失败**，判定必须联合 stdout/stderr。唯一判定真值表见 D1（关键条件：exit=1 时除 stdout 非空外 MUST 同时满足 stderr 为空）。

## Goals / Non-Goals

**Goals:**

- G1：untracked 新文件在 git 面板可查看完整 new-file diff（全部新增行视图）。
- G2：untracked 文件在状态列表中显示真实行数统计（additions=行数，deletions=0；二进制不计行数）。
- G3：diff 视图在任意宽度/横向滚动下行号与代码行始终一一对齐。
- G4：不破坏既有安全约束（argv 白名单、有界输出、context 取消）与只读语义（status/diff 不进 repo 写锁、不改 index）。

**Non-Goals:**

- 不引入虚拟滚动/第三方 diff 渲染库替换 diff2html。
- 不改 commit/push 流程、不改已跟踪文件的 diff 行为与 `git.Diff` 既有签名。
- 不改 DDD 分层与 API DTO 结构（仅扩展查询参数与数值语义）。
- 不支持 ignored 文件 diff（仅 untracked）；不支持 Windows（`/dev/null` 为 POSIX 假设，经 `os.DevNull` 隔离）。

## Decisions

### D1：untracked diff 用 `git diff --no-index -- /dev/null <path>` 合成

**选择**：git 层新增独立函数，不改 `Diff` 签名：

```go
// internal/git/ops.go
func DiffUntracked(ctx context.Context, dir, path string) (diff string, truncated bool, err error)
```

**备选**：
- `git add -N`（intent-to-add）后普通 diff —— 否决：写 index、有副作用，违反只读语义（G4）。
- 后端读文件手工拼 unified diff —— 否决：需自实现 hunk 格式，易错；`--no-index` 输出天然带 `new file mode` + `--- /dev/null` 头，diff2html 直接渲染。
- 在 `Diff` 加 bool 参数 —— 否决：布尔参数弱化签名表达力；独立函数隔离 no-index 行为（扩展点：未来 ignored/Windows 变体只动此函数）。

**端到端链路（逐跳输入/输出/失败语义）**：

```
GitPanel groupFiles (untracked 组, FileGroup 增 untracked 标记)
  │ openDiff(path, ref='', untracked=true)
  ▼
api.ts gitDiff(taskID, ref, path, untracked)  →  query 追加 untracked=1
  ▼
handleGitDiff (internal/api/git.go:57)
  │ 解析 untracked 查询值域：absent / "0" / "1"；其他值 → invalid_input
  ▼
Manager.GitDiff(ctx, taskID, ref, path, untracked)   (internal/task/gitops.go:101)
  │ 用例不变量（在任何 git 命令前校验）：
  │   untracked=true && path==""   → invalid_input
  │   untracked=true && ref!=""    → invalid_input
  │ dir 项目降级断言 (assertGitRepoTask) 位置不变，先于一切 git 命令
  ▼
git.DiffUntracked(ctx, worktreePath, path)
  │ 防御性路径校验：拒绝绝对路径、".." 逃逸、NUL
  │ run(ctx, dir, "diff", "--no-index", "--", os.DevNull, path)
  │   （diff 子命令已在白名单内，exec.go 无需改动；
  │     --no-index 按文件系统路径比较，MUST NOT 套 ":(literal)" pathspec）
  │ 输出判定真值表（依据实测契约，三处表述唯一以此为准）：
  │   err == nil（含 stdout 为空）                       → 正常返回
  │   ExitCode==1 && stdout 非空 && stderr 为空           → 正常 diff 输出
  │   errors.Is(ErrOutputTruncated) && stdout 非空 && stderr 为空
  │                                                     → 512KB 前缀 + truncated=true
  │   其他任何非 nil error（ExitCode>1 / stdout 空 / stderr 非空）→ 错误透传
  │ 复用 isBinaryDiffOutput → 二进制返回 {diff:"", truncated:true}
  │ 复用 DiffMaxBytes=512KB 截断 → truncated=true
  ▼
GitDiffDTO{Diff, Truncated} → diff2html 渲染为全部新增视图
```

**关键实现点**：

1. **exec.go:108 的 error 链陷阱**：输出溢出时 `run()` 返回 `fmt.Errorf("%w (%v)", ErrOutputTruncated, ce)`，`commandError` 以 `%v` 拼接，`errors.As(*exec.ExitError)` 在溢出路径上失败。DiffUntracked 的判定 MUST 先查 `errors.Is(err, ErrOutputTruncated)` 再查 ExitError（真值表顺序即此顺序）；且 MUST 校验 stderr 为空——stderr 溢出时 run() 会丢失部分 stderr，"stdout 有部分内容但 stderr 溢出"的失败不得误判为正常 diff。stderr 从 run() 第二返回值获取。
2. **错误码映射**：git 层防御性路径校验失败（绝对路径/`..`/NUL，属用户输入问题）MUST 返回可识别的 sentinel 错误 **`git.ErrInvalidDiffPath`**（`errors.Is` 可判），Manager 据此映射 **invalid_input**——禁止靠错误字符串匹配；git 执行失败（路径不存在、读取失败等，stderr 透传）映射为 **git_error**（沿用 gitops.go:121 既有 newOpErr(codeGitError, ...) 模式）。
3. **空新文件**：stdout 为仅含 `new file mode` 元数据的 diff（无 hunk），diff2html 渲染文件头；属正常输出，不做特殊处理。
4. **调用方声明语义**：`untracked=1` 是调用方声明的展示模式，服务端不二次探测文件是否真 untracked（避免额外命令与 TOCTOU）。注意：对已跟踪路径传 `untracked=1` 会返回该文件全文的新增 diff——API 读取面确有扩大（干净 tracked 文件原本无 diff 输出）。按**可信调用方风险接受**处理：diff API 与调用方同属用户本机信任域，不构成跨信任边界的能力扩张；文档化即可。
5. **平台隔离**：空设备路径用 `os.DevNull`，不写死 `/dev/null` 字面量。
6. **接口签名同步**：`internal/api/tasks.go:52` `TaskBackend.GitDiff` 接口签名改为 `(ctx, taskID, ref, path string, untracked bool)`，所有实现/fake/mock（含 `git_api_test.go:20,38` 的 `diffFn`/`mockGitBackend`）MUST 同步更新。

### D2：untracked 行数统计在 `Status()` 内按行计数

**选择**：`ops.go:39-57` numstat 合并循环之后，对 `e.Untracked` 条目读文件计行：`additions = count('\n') + (末字节非 '\n' 且 len>0 ? 1 : 0)`，`deletions = 0`。

**执行顺序与有界读取**（预算语义：64MB 仅约束文本行数读取；所有 regular file 始终保证前 8000 字节二进制嗅探）：

```
for each untracked entry（numstat 合并完成后）:
  1. os.Lstat：非 regular file（symlink/fifo/设备）→ 跳过（additions=0，不报错）
  2. 打开文件，读前 8000 字节嗅探：含 NUL → IsBinary=true，不计行，下一文件
     （嗅探 MUST 始终执行——即使文本预算已耗尽，仍允许为判定 IsBinary 读 prefix，
     但不得据此继续行计数）
  3. 文本行数读取：prefix（8000B）计入单文件 16MB 与累计 64MB 双预算；
     续读用 io.LimitReader(min(16MB-prefixLen, 剩余累计预算)+1) 有界读取，
     预算按实际读取字节计（非 Lstat.Size()——避免 Lstat 后文件增长突破上限）
  4. 单文件 >16MB 或 64MB 累计文本预算耗尽 → 该文件跳过行计数（additions=0，
     IsBinary 仍已正确标记），后续文件仅做嗅探不再计行
  5. count('\n') + 末行无换行 +1 → additions
  IO 错误（权限/读失败）→ 返回明确错误（含文件路径与操作上下文），不静默置零；
  例外：Lstat/打开时 ENOENT（status 快照后文件被并发删除）属正常竞态，
  跳过计数且不视为错误（代码注释区分 ENOENT 竞态 vs 其他 IO 错误）
```

**关键实现点**：

1. **IO 预算**：单文件 16MB + 全部 untracked 累计 64MB 双上限，避免病态 worktree（如误放大型日志/数据目录）拖垮 status。超限跳过计数是容量保护，与「numstat 调用失败 MUST NOT 静默降级」（ops.go:12）的失败语义不冲突。
2. **二进制嗅探**：前 8000 字节含 NUL（对齐 git 启发式），与 `isBinaryDiffOutput` 同层新增 helper。
3. 修正 `types.go:25` 过时注释（untracked 行数估算本次落地）。

### D3：行号对齐修复——同表同行 + 禁折行 + 唯一横向滚动 owner

**选择**：纯 CSS 修复，三条构造性不变量：

```
不变量 1: 行号 cell 与代码 cell 永远在同一 <tr>（diff2html 结构天然满足，
          CSS 不得破坏 table 布局，不得给行容器加 transform/新建定位上下文）
不变量 2: 代码行禁止折行（white-space: nowrap），长行横向滚动而非 wrap
          → 每行高度恒定，gutter 与代码行高一致
不变量 3: 横向滚动 owner 唯一且确定为 .d2h-wrapper（包裹整张 diff table），
          gutter 与代码同容器同向滚动，结构上不可能错位
```

**关键实现点**（实现 lane 为 @designer；以下为硬约束，实现后已按真实 DOM 实测修正——见文末注）：

1. `.d2h-code-line/.d2h-code-side-line` 与 `.d2h-code-linenumber/.d2h-code-side-linenumber` 设**相同固定** `line-height: 18px`（配 12px mono）；代码侧禁止折行 MUST 用 `white-space: nowrap`（**非** `pre`——diff2html 生成的 HTML 模板含换行/缩进，`pre` 会把模板空白渲染成真实换行致行高爆炸；代码内容空白保留由内置 `.d2h-code-line-ctn{white-space:pre}` 承担）；`.d2h-diff-table td{padding:0}`（UA 默认 1px padding 使代码 cell 比 gutter 低 1px）。
2. **滚动 owner 收敛为 `.d2h-wrapper` 唯一**，其余容器显式关闭横向滚动：
   - `.d2h-wrapper`：`overflow-x: auto; overflow-y: hidden`（保留圆角裁切）。
   - `.git-diff`：`overflow-y: auto; overflow-x: hidden`。
   - `.git-panel` ≤1024px：显式 `overflow-x: hidden`。
   - `.d2h-file-diff`：`overflow: visible`（非 scroll container）。
   - `.d2h-diff-table` 及中间容器（file-wrapper/file-diff/code-wrapper）：`width: max-content; min-width: 100%`，保证 `scrollWidth > clientWidth` 的元素唯一为 `.d2h-wrapper`。
3. **根因修复（实测发现）**：diff2html gutter（`.d2h-code-linenumber`）是 `position:absolute` 的 td，无定位祖先时锚定 ICB——任何元素级滚动都会让 gutter 原地不动，这才是错位根因；仅收敛 scroll owner 不足以修复。给 `.d2h-code-wrapper`（内置无样式、不在禁止新建定位上下文的 `.d2h-file-diff`/`.d2h-diff-table` 之列）加 `position: relative`，gutter 锚定进滚动内容内部，双向滚动全程粘合；静止位置不受 containing block 切换影响，无漂移。
4. 不覆盖 diff2html 内置 gutter padding（内置 `padding: 0 8em` 为 gutter 预留空间；此前 8px 覆盖导致代码文本与 gutter 重叠，已移除）。
5. ≤1024px 断点：`.git-diff{overflow:visible}` 切换保留，`.d2h-wrapper` owner 身份不变。
6. 验收（已由 designer 用 Chrome+CDP 真实 DOM 完成）：4-hunk、286 字符超长行 diff，1440/1024/800px 三视口 × 6 种滚动状态逐行矩形采样，maxDy=0、maxDx=0；唯一 `scrollWidth > clientWidth` 元素为 `.d2h-wrapper`。

### D4：Spec 更新（git-operations delta）

- MODIFIED「文件 diff 查看」：untracked 场景、查询值域、ref 冲突、错误映射、空文件/二进制语义。
- MODIFIED「工作区状态查询」：untracked 行数统计语义（行计数、二进制不计、双上限容量保护、IO 错误不静默）。
- ADDED「diff 视图渲染对齐」：不变量 1-3 + 双视口验收场景。

## Risks / Trade-offs

- [`--no-index` exit=1 歧义（成功/空文件/路径不存在同为 1）] → 判定唯一以 D1 真值表为准（exit1+stdout 非空+stderr 空才为正常）；新增测试覆盖三种 exit=1 变体。
- [exec.go 溢出路径 error 链断裂（%v 拼接 commandError）] → DiffUntracked 先判 `errors.Is(ErrOutputTruncated)` 且校验 stderr 为空；不改 exec.go（避免影响其他调用方）。
- [`/dev/null` 平台限制] → `os.DevNull` 隔离；应用目标平台 darwin，Windows 列非目标。
- [`untracked=1` 用于已跟踪路径扩大 API 读取面（干净 tracked 文件全文）] → 可信调用方风险接受（同属本机信任域），调用方声明语义不二次探测，文档化。
- [预算耗尽后 additions=0 与真空文件不可区分] → 接受（文档化容量保护语义）；IsBinary 不受预算影响始终正确标记。
- [大 untracked 文件/病态目录的 status IO 开销] → 16MB 单文件 + 64MB 累计双上限（按实际读取字节）+ regular file 守卫 + 嗅探恒定 8000B。
- [CSS 固定 line-height/宽度撑破 diff2html 内置度量] → 不变量构造性保证 + designer 真实 DOM 验证 + 双视口人工验收；其余容器横向滚动显式关闭避免嵌套 owner。

## Migration Plan

无持久化/配置迁移。纯代码变更，随版本发布；回滚 = revert 提交。

## Open Questions

无。oracle 评审问题均已定案：`untracked=1` 为调用方声明模式（不二次探测）；与 `ref` 同现时拒绝（invalid_input）；横向滚动 owner 固定 `.d2h-wrapper` 且其余容器横向关闭；64MB 预算仅约束文本行数读取，二进制嗅探对所有 regular file 始终执行；路径防御错误统一映射 invalid_input。
