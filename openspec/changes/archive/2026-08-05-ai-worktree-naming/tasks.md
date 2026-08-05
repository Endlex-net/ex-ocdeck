# Tasks: ai-worktree-naming

Lane 依赖：A（AI 底座）与 B（路径/Add 重构）可并行 → C（命名集成）依赖 A+B → D（API/UI）依赖 A → E（集成门禁）依赖全部。

## 1. Lane A：internal/ai 包（配置 + Completer + SlugNamer）

- [x] 1.1 `internal/ai/config.go`：`ProviderConfig`（provider/api_key/base_url/model）、文件 Load/Save（原子写、0600）、schema 校验（provider 枚举 openai/anthropic、model 非空、base_url 可空或合法 http(s) URL）
- [x] 1.2 `internal/ai` `Store`：启动加载（文件不存在=正常未配置；损坏/不可读=configured=false + loadErr + 日志告警，不拒绝启动）、atomic 快照读、**写 mutex 串行化**「读旧 key 合并 → 校验 → 原子 rename → 快照替换」（写文件失败保持旧快照）、并发安全
- [x] 1.3 `internal/ai/completer.go`：`Completer` 接口 `Complete(ctx, Request) (Response, error)`（Request: System/User/MaxTokens；Response: Text）、`NewCompleter(cfg)` 按 provider 分派、http.Client Timeout ≤10s、body 上限 1MB
- [x] 1.4 `internal/ai/openai.go`：`{base}/v1/chat/completions`，`Authorization: Bearer`，响应取 `choices[0].message.content`；非 2xx/超时/解析失败/结构缺失 → error 不重试
- [x] 1.5 `internal/ai/anthropic.go`：`{base}/v1/messages`，`x-api-key` + `anthropic-version: 2023-06-01`，请求含必填 `max_tokens`，响应拼接 `content[*].type=="text"`；失败语义同上
- [x] 1.6 `internal/ai/slugnamer.go`：`SlugNamer`（prompt 模板 + 任务名截断 500 字符 + 三道清洗门禁：清洗/格式 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`/无意义词表 + 注入 fallback func），实现 `Slug(ctx, taskName) string` 永不返回 error；未配置时零网络调用
- [x] 1.7 单测：config 读写/校验/0600、Store 状态机与 `-race` 并发 **PUT+PUT**（内存快照与磁盘最终一致）及 PUT+快照读、httptest 固化两 provider 请求/响应契约（含 `Content-Type: application/json`、base_url 去尾 `/`、1MB 超限拒绝）与失败分支、清洗门禁各分支（含固化词表 task/new/untitled）、回退路径

## 2. Lane B：worktree Add 契约 + task 侧路径生成

- [x] 2.1 `internal/worktree/worktree.go`：`Add` 签名改为 `Add(ctx, repoPath, dest, branch, baseRef string)`；dest 为绝对路径；任何文件/git 副作用前先 `checkContainment(dest)`；删除包内 `worktreePath` 与 `validateIdent`（约束由 slug 白名单 + containment 覆盖）
- [x] 2.2 `internal/task` 导出 `Slugify`（原 slugify 改导出，行为不变）并新增 `normalizeSlug`（允许返回空）
- [x] 2.3 `internal/task/crud.go` 路径生成：`<dataDir>/worktrees/<projectNameSlug>/<branchPathSlug>-<rand4>`（projectNameSlug=normalizeSlug(proj.Name) 空时 `project-<projectID前8位>`，截断 ≤50 去尾 `-`；branchPathSlug=分支名去 `ocdeck/` 前缀截断 ≤50 去尾 `-`、截空兜底 `task`，**分支名本身不变**；rand4=crypto/rand 4 位 [a-z0-9]，经可注入 rand4Fn seam 生成（Go 1.24 下底层熵失败为进程 fatal；注入熵源失败或 os.Stat 非 IsNotExist 错误直接返回且零副作用））；落库前碰撞预检循环 ≤3 次，3 次均碰撞返回错误
- [x] 2.4 `internal/task/types.go`：`WorktreeBackend.Add` 签名同步；`task.NewWorktreeAdapter` 透传 dest
- [x] 2.5 单测：路径格式生成（**含超长 fallback 分支名：分支名不变、目录段截断 ≤50**）、projectNameSlug 回退与截断、碰撞重试与耗尽、Add containment 前置、存量 DB 路径删除/清理回归（不经新格式重算）

## 3. Lane C：命名集成与 wiring（依赖 A+B）

- [x] 3.1 `internal/task/types.go`：新增 `BranchNamer` 接口（`Slug(ctx, taskName) string`）；`task.Options` 增加 Namer 注入（**nil 时 Create 直接使用 Slugify**，防御性默认）
- [x] 3.2 `internal/task/crud.go` `Create`：`slug := namer.Slug(ctx, taskName)` → `branch := "ocdeck/" + slug`（LLM/校验/冲突检查全部先于落库副作用）；RetryCreate 复用落库 Branch/WorktreePath 不重算
- [x] 3.3 `cmd/ocdeck-server/main.go`：**单个** `ai.LoadStore` 实例同时注入：`ai.NewSlugNamer(store, task.Slugify)` → `task.Options.Namer`，以及 `api.Server.SetAIConfigStore(store)`（新增 setter，沿用 SetTaskBackend 模式，路由注册前调用）；禁止两处各自 LoadStore
- [x] 3.4 单测：Create 全链路（mock Namer 命中/回退/分支冲突/碰撞）、RetryCreate 路径不变

## 4. Lane D：AI 配置 API + 前端（依赖 A；后端 handler 的 `SetAIConfigStore` setter 由 Lane C 3.3 落地，D 仅消费）

- [x] 4.1 `internal/api/ai_config.go`：`GET/PUT /api/v1/ai/config`（GET: configured/provider/base_url/model/api_key_masked/load_error；掩码规则 len≥8 前4+`***` 否则纯 `***`；PUT: 校验失败 422 invalid_input、掩码/空 key 保留原 key、无旧 key 且空 key→422、成功后 Store 热替换）；handler 从 `SetAIConfigStore` 注入的 Store 读取/写入（见 3.3 wiring）；路由注册 + token 鉴权
- [x] 4.2 API 单测：GET 掩码/未配置/损坏 load_error、PUT 422 各分支、保留原 key、热更新生效
- [x] 4.3 `web/src/api.ts`：`getAIConfig`/`saveAIConfig`
- [x] 4.4 AI 配置页：provider 下拉、api_key 密码框（掩码展示、可覆盖）、base_url、model、保存反馈、422 错误与 load_error 展示；接入设置区导航

## 6. Lane F：思考强度（thinking）全局配置（评审反馈新增，依赖 A/D 已完成）

- [x] 6.1 `internal/ai`：`ProviderConfig` 增加 `Thinking` 字段（枚举 `""`/`off`/`low`/`medium`/`high`，Validate 校验）；两 provider 按映射表下发（anthropic: off→disabled、档位→budget_tokens 1024/4096/16384 且 max_tokens 自动提升 > budget；openai: off→minimal、档位→reasoning_effort）；`""` 不下发任何参数
- [x] 6.2 `internal/ai`：能力协商——4xx 且错误体表明不支持 thinking/reasoning 参数时剥离该参数原样重试一次，再失败才返回 error
- [x] 6.3 `internal/ai` 单测：映射表全档位（httptest 断言请求体）、空值不下发、max_tokens 自动提升、协商重试（首次 4xx unsupported → 二次无参成功；二次仍败 → error）
- [x] 6.4 `internal/api/ai_config.go`：GET/PUT DTO 增加 `thinking` 字段；非法值 422；测试覆盖
- [x] 6.5 `web`：AI 配置页增加思考强度下拉（默认/关闭/低/中/高 + 延迟提示），types/api 同步
- [x] 6.6 门禁：`go build/test/vet`（ai/api -race）、web build、openspec validate --strict 全绿

## 7. Lane G：激活 probe 冷启动重试（评审反馈新增，design D8）

- [x] 7.1 `internal/task/activate.go` `startServeWithPortRetry`：`oc.Probe` 失败且 `errors.Is(err, opencode.ErrServeNotReady)` 时保活 serve 会话、退避 2s/4s 重试（共 3 次尝试）；ErrCapabilityMismatch/ErrUnauthorized 不重试；全部失败才 kill 会话并维持现有 OpError 语义
- [x] 7.2 单测：probe 首次 ErrServeNotReady、二次成功 → 激活继续（serve 会话未被 kill）；3 次均失败 → 落 suspended + last_error；ErrCapabilityMismatch 立即失败不重试
- [x] 7.3 门禁：`go build ./... && go test ./internal/task/... -race -count=1` 全绿

## 5. Lane E：集成门禁

- [x] 5.1 `go build ./...` + `go test ./...`（含关键包 `-race`）全绿
- [x] 5.2 `web/` 前端构建通过
- [x] 5.3 手工 E2E：未配置 AI 创建中文名任务 → 回退 slugify + 新路径格式；配置 mock provider → LLM slug + 新目录；存量任务删除/挂起/激活回归
