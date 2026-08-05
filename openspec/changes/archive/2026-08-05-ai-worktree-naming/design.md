# Design: ai-worktree-naming

## Context

ocdeck 以「项目 → 任务（git worktree + 分支）」建模 agent 工作单元。当前命名链路（已核实）：

```
用户输入任务名（常为中文）
  → POST /api/v1/projects/{id}/tasks            (internal/api/tasks.go:138)
  → task.Manager.Create                          (internal/task/crud.go:15)
      branch  = "ocdeck/" + slugify(taskName)    (crud.go:26, slugify: 415-435)
      wtPath  = <dataDir>/worktrees/<projectID>/<taskID>   (crud.go:438, worktree.go:227-229)
      → 前置校验(无副作用): ValidateBranchName + BranchExists  (crud.go:34-42)
      → 插入 creating 行(含 worktree_path)                    (crud.go:45)
      → worktree.Manager.Add → git worktree add  (worktree.go:49, git/worktree.go:35)
      → inherit → suspended → init/activate
```

- `projectID`/`taskID` 均为 32 位 hex（`api/projects.go:253`、`task/util.go:11`），目录人不可读。
- `slugify`（crud.go:415-435）只保留 `[a-z0-9-]`，**空结果一律兜底 `"task"`**——纯中文任务名退化为 `ocdeck/task`；这也意味着「项目名 slug 为空回退 `project-<id8>`」不能复用 slugify，必须拆出允许返回空的规范化函数。
- 代码库无任何 LLM 集成（无 SDK、无 provider 配置）。
- `tasks.worktree_path` 创建时落库，删除/挂起/激活等生命周期操作一律按 DB 记录执行（新格式不影响存量任务）。
- `worktree.checkContainment`（worktree.go:318-336）目前仅用于删除路径；新 `Add` 接收外部计算的绝对 dest 时，必须在任何文件/git 副作用前做同样的包含性校验。
- API 错误惯例（errors.go:36-47）：`invalid_input` → **422**（非 400）。
- 项目名是用户注册时输入的（`projects.name`），可能含中文/空格/特殊字符。

平台后续会有多个 LLM 驱动功能（命名、摘要、生成），本变更建立**可复用的 LLM 配置与调用底座**，并落地第一个场景（分支命名）。

## Goals / Non-Goals

**Goals:**

- 全局 AI provider 配置（openai / anthropic），`<dataDir>/ai.json`，后端 API + 前端配置页，热更新。
- 通用 LLM `Completer` 抽象（`Complete(ctx, Request) (Response, error)`），未来 LLM 功能直接复用；分支命名是其上的第一个 adapter。
- 创建任务时 LLM 将任务名提炼为语义化英文 slug，分支名 `ocdeck/<ai-slug>`；未配置/失败/非法输出一律回退 slugify，AI 错误本身不向用户返回、不阻断创建。
- 新任务 worktree 目录 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>`（branchPathSlug = 分支名去 `ocdeck/` 前缀后截断 ≤50 的目录段；rand4 = 4 位 `[a-z0-9]`）。
- 存量任务（旧路径格式）全生命周期行为不变。

**Non-Goals:**

- 不支持 openai/anthropic 以外的 provider（配置结构与 Completer 预留扩展，不实现）。
- 不迁移存量 worktree 目录；不引入 DB schema 变更。
- 不做分支名预览/编辑 UI；不改变分支冲突语义（仍 409 报错，不自动改名）。
- 不实现用量统计、缓存、LLM 重试框架（单次调用 + 超时 + 回退）。

## Decisions

### D1：AI 配置存 `<dataDir>/ai.json` 文件，而非 SQLite

与「全局配置（~/.ocdeck）」定位一致；AI 配置是 server 级基础设施配置，不依附任何项目；文件形态便于手工编辑与备份。备选：SQLite 全局表（lifecycle-config 模式）——适合项目级设置，全局基础设施配置用表增加迁移与 bootstrap 复杂度。

文件格式：

```json
{
  "provider": "openai",          // "openai" | "anthropic"
  "api_key": "sk-...",
  "base_url": "",                // 可选，空=provider 默认端点
  "model": "gpt-4o-mini",        // 必填，由用户按 provider 填
  "thinking": ""                 // 可选枚举：""(默认不下发) | "off" | "low" | "medium" | "high"
}
```

写入：临时文件 + 原子 rename，权限 0600。日志中 MUST NOT 出现 api_key 明文。`thinking` 非法值与其他校验失败同等处理（保存 422 / 加载 configured=false + load_error）。

### D2：通用 LLM `Completer` + 自实现 provider client（net/http，不引入 SDK）

go.mod 无 AI SDK。自实现薄 client（~250 行 + httptest），避免 SDK 依赖树；失败语义统一为「任何错误 → 调用方回退」，用不到 SDK 的重试/流式能力。

```text
internal/ai/
  config.go     — ProviderConfig + Load/Save（原子写、0600、校验）+ Store（见 D7）
  completer.go  — Completer 接口 + Request/Response + NewCompleter(cfg) 按 provider 分派
  openai.go     — OpenAI chat/completions 实现
  anthropic.go  — Anthropic messages 实现
  slugnamer.go  — SlugNamer（prompt 模板 + 清洗门禁 + 回退注入），实现 task.BranchNamer
```

**Completeness 契约（供未来所有 LLM 功能复用）：**

```go
type Request struct {
    System    string  // 可选 system prompt
    User      string  // user 消息
    MaxTokens int     // 必填上限（Anthropic 协议必填，OpenAI 同设）
}
type Response struct {
    Text string       // 首个文本块的全部内容
}
type Completer interface {
    Complete(ctx context.Context, req Request) (Response, error)
}
```

**Provider 字段级契约：**

| 项 | OpenAI | Anthropic |
|---|---|---|
| 默认端点 | `https://api.openai.com` | `https://api.anthropic.com` |
| URL | `{base}/v1/chat/completions` | `{base}/v1/messages` |
| 认证头 | `Authorization: Bearer <api_key>` | `x-api-key: <api_key>` + `anthropic-version: 2023-06-01` |
| 公共头 | 双方 MUST 发送 `Content-Type: application/json` | 同左 |
| 请求体 | `{model, messages:[{role:system\|user, content}], max_tokens}` | `{model, system, messages:[{role:user, content}], max_tokens}` |
| 响应取值 | `choices[0].message.content` | 拼接 `content[*]` 中 `type=="text"` 的 `text` |
| 失败语义 | 非 2xx / 超时 / JSON 解析失败 / 结构缺失 → 返回 error（调用方回退）；MUST NOT 重试（思考参数能力协商除外，见下） | 同左 |

**思考强度（thinking）映射**（全局配置，对所有 LLM 调用生效；`""` = 不下发任何思考参数，跟随模型/网关默认，即现状行为）：

| thinking | Anthropic 请求体 | OpenAI 请求体 |
|---|---|---|
| `off` | `thinking:{"type":"disabled"}` | `reasoning_effort:"minimal"` |
| `low` / `medium` / `high` | `thinking:{"type":"enabled","budget_tokens":1024/4096/16384}` | `reasoning_effort:"low"/"medium"/"high"` |

约束与降级：
- Anthropic 协议要求 `max_tokens > budget_tokens`；provider 端 MUST 自动提升 `max_tokens = max(请求值, budget_tokens+512)`。
- **能力协商（对非思考模型通用）**：若响应为 4xx 且错误体表明不支持 thinking/reasoning 参数（如 unknown/unsupported parameter），MUST 剥离思考参数原样重试一次（确定性能力协商，非瞬时重试）；再失败才返回 error。保证「无论对方是否是思考模型」配置都不破坏调用。
- 思考强度越高延迟越大（Slug 场景受 10s 超时约束，高超度可能超时回退——用户可感知取舍，UI 提示）。

- **base_url 归一化**：拼接 endpoint 前 MUST 去除 base_url 尾部 `/`。
- HTTP：`http.Client{Timeout: 10s}`；请求/响应 body 上限 1MB（读取 `limit+1` 字节，超限 MUST 显式拒绝返回 error，而非截断后解析）；api_key 不写入任何日志。

### D3：slug 输出清洗门禁（回退判定）

LLM 输出不可信，统一过三道闸，任一失败 → 回退：

1. **清洗**：取首行、去首尾空白、去包裹引号（`"` `'` `` ` ``）、去尾部标点（`.`, `,`, `;`, `:`）、lowercase；
2. **格式**：必须匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`（≤50 字符，可作 `ocdeck/` 后缀且过 `git check-ref-format`）；
3. **语义兜底**：清洗后为空或命中无意义词表（**固化集合**：`task`、`new`、`untitled`）→ 视为失败。

### D4：命名职责拆分——task 侧接口、ai 侧 adapter、fallback 显式注入

- `internal/task/types.go` 新增 `BranchNamer` 接口（依赖抽象区，与 `WorktreeBackend` 同模式）：`Slug(ctx, taskName) string`（**永不返回 error**，内部自回退）。
- `internal/ai/slugnamer.go` 的 `SlugNamer` 组合：配置 Store（可用性判定）+ Completer + D3 清洗门禁 + **注入的 fallback func**。
- fallback 即现有 slugify 逻辑：`internal/task` 导出 `Slugify(name string) string`（原函数改导出，行为不变），wiring 时注入 `ai.NewSlugNamer(store, task.Slugify)`。AI 未配置时 SlugNamer 零网络调用直接走 fallback。
- **拆分 normalize 与 slugify**：新增 `normalizeSlug(name string) string`（slugify 的规范化部分，**允许返回空**）；`Slugify` = normalize + 空兜底 `"task"`。项目名目录段用 normalize（见 D5），任务分支 fallback 用 Slugify。
- **nil 防御**：`task.Options.Namer == nil` 时 `Create` MUST 直接使用 `Slugify`（防御性默认，杜绝 nil panic；正常 wiring 下不会发生）。
- Prompt：「将任务名提炼为 2-5 个词的英文 kebab-case 短语，只输出该短语」，任务名截断 500 字符。

### D5：worktree 目录新格式与 `Add` 唯一契约

**目录格式**：`<dataDir>/worktrees/<projectNameSlug>/<branchPathSlug>-<rand4>/`

- `projectNameSlug` = `normalizeSlug(proj.Name)`，为空（纯中文项目名）→ `project-<projectID前8位>`；截断至 ≤50 字符（截断后去尾部 `-`）。
- **`branchPathSlug`（目录段）与分支名的关系**：分支名行为完全不变（`Slugify` 无长度限制，AI slug 经 D3 已 ≤50）。目录段 `branchPathSlug` = 分支名去 `ocdeck/` 前缀后**截断至 ≤50 字符**（截断后去尾部 `-`，截空时兜底 `task`）。即：AI 路径下两者一致；超长机械 fallback 分支仅目录段被截断，分支名不变——目录是分支的派生展示，DB `worktree_path` 为唯一事实源，无需从目录反推分支。
- `rand4` = 4 位 `[a-z0-9]`（crypto/rand）。**熵失败语义（Go 1.24）**：`crypto/rand.Read` 自 Go 1.24 起保证填充成功，底层熵失败直接 fatal（进程终止，天然零副作用）；代码经可注入 `rand4Fn` seam 保留 error 返回路径——注入熵源失败时 MUST 返回错误、保持零副作用（该路径同时使碰撞/耗尽测试可确定性构造）。

**Add 契约（消除两处 worktreePath 重复，路径计算唯一归属 task 包）：**

```go
// internal/worktree/worktree.go — 签名由 (projectID, taskID, ...) 改为：
Add(ctx context.Context, repoPath, dest, branch, baseRef string) error
```

- `task.Manager.Create` 计算完整绝对 dest 并传入；`worktree.Manager` 不再自行拼路径（删除其 `worktreePath`）。
- `Add` 在任何文件/git 副作用之前 MUST 先 `checkContainment(dest)`（根仍为 `<dataDir>/worktrees`，不变）。原 `validateIdent` 对新路径段的约束由「slug 白名单（D3/normalizeSlug 输出字符集）+ containment」共同覆盖，`validateIdent` 随旧签名移除。
- `WorktreeBackend` 接口（task/types.go:87-106）的 `Add` 方法签名同步更新；adapter（`task.NewWorktreeAdapter`）透传。

**碰撞协议（无需 DB CAS）：** 碰撞检测循环**前移到落库前**（`os.Stat` 存在性检查无副作用）：

```
Create(projectID, taskName)
  → 前置: 项目存在                                  (无副作用)
  → slug   = namer.Slug(ctx, taskName)              # LLM 或回退；≤10s；无副作用
  → branch = "ocdeck/" + slug
  → 前置: ValidateBranchName + BranchExists          (无副作用，现状不变)
  → dest 生成循环（≤3 次，均无副作用）:
      rand4 → dest = worktrees/<projSlug>/<branchPathSlug>-<rand4>
      os.Stat(dest) 已存在 → 重生 rand4；3 次均碰撞 → 返回 internal 错误
      （os.Stat 返回 IsNotExist 以外的错误 → MUST 直接返回错误、保持零副作用）
  → 插入 creating 行(含 dest) → worktree.Add(dest) → inherit → suspended → init/activate
      （后续链路与现状完全一致）
```

残余 TOCTOU（并发创建同路径）由 `Add` 失败 + 既有 `creation_failed` 补偿链兜底（本地单用户工具，概率可忽略，风险已记录）。`RetryCreate` 路径复用落库的 `row.Branch`/`row.WorktreePath`，**不经新格式重算**——首次创建与重试是同一主流程的局部变体（重试跳过已完成的副作用步骤）。

### D6：配置 API 与 UI 契约

- 路由：`GET/PUT /api/v1/ai/config`（全局，无项目维度），沿用 server token 鉴权中间件。
- **GET 响应**：`{configured: bool, provider, base_url, model, thinking, api_key_masked, load_error?}`
  - 未配置（文件不存在）：`configured=false`，其余字段为空字符串，`load_error` 缺省。
  - 文件损坏/不可读：`configured=false` + `load_error=<人类可读原因>`（**不返回 500**，降级语义与运行时一致）。
  - `api_key_masked`：len(key) ≥ 8 → 前 4 位 + `***`；len < 8 → 纯 `***`；无 key → `""`。
- **PUT 请求**：`{provider, api_key, base_url, model, thinking}`。校验失败 → **422 `invalid_input`**（项目惯例，非 400）；成功 → 200 + GET 同形响应。
  - `api_key` 为掩码值（含 `***`）或空字符串 → 保留已存储原 key；无旧 key 且提交空/掩码 key → 422（configured 永远要求 key 非空）。
  - `provider` 枚举外、`model` 空、`base_url` 非空但非合法 http(s) URL、`thinking` 枚举外 → 422。`thinking` 缺省/空 = 不下发思考参数。
- **UI**：设置区新增「AI 配置」页：provider 下拉、api_key 密码框（展示掩码、可覆盖输入）、base_url、model、**思考强度下拉（默认/关闭/低/中/高，附延迟提示）**、保存按钮 + 成功/失败反馈（含 `load_error` 展示）。`web/src/api.ts` 增加 `getAIConfig`/`saveAIConfig`。

### D7：配置运行时——快照 Store 与状态机

`internal/ai` 提供 `Store`（内存快照 + 文件持久化）：

```go
type Store struct { /* atomic.Value 存 snapshot */ }
type snapshot struct { cfg ProviderConfig; configured bool; loadErr error }
```

- **启动**：`LoadStore(dataDir)`——文件不存在 → `configured=false`（正常态）；JSON 损坏/读取失败 → `configured=false` + 记录 loadErr + 日志告警（**不拒绝启动**）。
- **热更新**：PUT 校验通过 → 写文件（原子）→ 替换 atomic 快照。读方（SlugNamer）每次调用取一次快照，单次命名全程使用同一快照——无 data race、无半新半旧混用。
- **写串行化**：Store 持有写 mutex，PUT 的「读取旧 key 合并 → 校验 → 文件原子 rename → 快照替换」MUST 在单个写锁内串行完成；文件写入失败时 MUST 保持旧快照不变并返回错误。保证任意并发 PUT 序列下内存快照与磁盘最终一致（last-writer-wins 按写锁获得顺序）。
- **可用性判定**：`configured = (provider 合法 && api_key 非空 && model 非空 && loadErr == nil)`。
- **同一实例 wiring**：main.go MUST 构造**单个** `ai.Store` 实例，同时注入 `ai.NewSlugNamer(store, task.Slugify)`（→ `task.Options.Namer`）与 API 层（新增 `api.Server.SetAIConfigStore(store)`，沿用 `SetTaskBackend` 模式，在路由注册前调用）。禁止两处各自 LoadStore。
- 并发保证：快照不可变、atomic 读、mutex 串行写；`-race` 测试覆盖并发 PUT+PUT（内存/磁盘一致性）与 PUT+Slug。

### D8：激活 capability probe 的冷启动重试（评审反馈新增）

**问题**：全新 worktree 是 opencode 的冷项目，`opencode serve` 启动后首次 `/session/status` 可达 7s+（实测 7.3s），机器负载下超过 Probe client 的 `OpTimeout: 10s` → 超时归类 `ErrServeNotReady` → 激活失败落 suspended（实测日志 `capability probe (serve not ready)`）。

**决策**：`startServeWithPortRetry` 中健康检查通过后的 `oc.Probe(ctx)` 增加**有限重试**：
- 仅 `errors.Is(err, opencode.ErrServeNotReady)` 触发重试；`ErrCapabilityMismatch`（结构漂移）与 `ErrUnauthorized`（内部 bug）MUST NOT 重试，语义不变。
- 共 3 次尝试（1 + 2 重试），退避 2s/4s；重试期间 **serve 会话保活**（健康检查已通过，慢的是冷端点而非进程）；全部失败才 kill 会话并落 suspended（现状补偿语义不变）。
- 首次调用即使客户端超时，服务端项目初始化通常已完成，第二次尝试命中热路径（毫秒级），3 次尝试足以覆盖冷启动窗口。

## Risks / Trade-offs

- [LLM 延迟拖慢创建] → 10s 超时 + 回退；slug 生成在无副作用前置阶段，超时代价仅为创建延迟。
- [LLM 输出不合规/语义差] → D3 三道闸 + 回退；最坏情况退化为现状行为。
- [api_key 泄露面] → 文件 0600；API 掩码返回；不写日志；UI 密码框。本地开发工具，接受文件存储（用户决策）。
- [项目名 slug 碰撞]（两项目 slugify 后相同）→ 第二段 rand4 消歧；containment 按全路径，正确性不受影响。
- [TOCTOU 路径碰撞]（并发创建）→ 落库前预检 + Add 失败走 creation_failed 补偿；单用户本地工具，接受。
- [新旧路径并存的理解成本] → DB 记录为唯一事实源；「创建时定格式，之后只读 DB」写入 spec。
- [provider API 契约漂移] → client 层隔离 + httptest 固化请求/响应形状；失败一律回退。
- [超长路径] → 目录段 projectNameSlug ≤50、branchPathSlug ≤50、rand4=4，单段 ≤55 字符，总路径远低于 macOS 上限（分支名本身无长度限制，仅目录派生段截断）。

## Migration Plan

1. Lane A：`internal/ai` 包（config/Store/Completer/两 provider/SlugNamer）+ 单测。
2. Lane B：worktree `Add` 契约改造 + task 侧路径生成与碰撞循环 + 受影响单测（可与 Lane A 并行）。
3. Lane C：命名集成与 wiring（types.go `BranchNamer`、`crud.go Create` 接入、main.go 注入）——依赖 A+B。
4. Lane D：API 路由 + 前端配置页——依赖 A（Store）。
5. 集成门禁：`go build ./...`、`go test ./...`（含 `-race`）、`web/` build、手工 E2E（未配置回退 / mock provider LLM 路径 / 存量任务回归）。

回滚：纯新增 + 创建链路改动，无 DB 迁移；回退代码即可，存量任务不受影响。

## Open Questions

- 无。Oracle 评审的 4 个问题已决策：碰撞协议前移（无 CAS）；损坏配置降级（不拒绝启动）；校验错误 422（项目惯例）；接受通用 Completer 抽象。
