# Delta Spec: task-lifecycle

## MODIFIED Requirements

### Requirement: 任务创建

系统 SHALL 支持在项目下创建任务。每个任务 MUST 拥有独立的 git worktree 与独立分支（从项目默认分支切出）。

分支名 MUST 为 `ocdeck/<slug>`，slug 生成策略：当 AI 配置可用（见 ai-provider-config spec 的可用性判定）时，SHALL 调用 LLM 将任务名提炼为语义化英文 kebab-case slug（≤50 字符，匹配 `^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$` 且不命中无意义词表，否则视为失败）；LLM 调用失败、超时、输出非法或 AI 未配置时，MUST 回退到机械 slugify（与既有行为一致，空结果兜底 `task`）。**AI 错误本身 MUST NOT 向用户返回、MUST NOT 阻断任务创建**——命名回退后创建流程继续，但随后仍可能因既有的分支名校验/冲突等前置检查失败（该语义不变）。LLM 调用 SHALL 设置超时（≤10s）且发生在任何副作用（落库、worktree add）之前。

新建任务的 worktree 路径 MUST 为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`：`projectName-slug` 为项目名经规范化（小写、非 `[a-z0-9-]` 折叠为 `-`，允许为空）的结果，为空时回退 `project-<projectID前8位>`，且 MUST 截断至 ≤50 字符；`branchPathSlug` 为分支名去掉 `ocdeck/` 前缀后截断至 ≤50 字符的目录段（截断后去尾部 `-`，截空时兜底 `task`）——目录段是分支名的派生展示，**分支名本身行为不变**（机械 slugify 无长度限制），DB 落库的 `worktree_path` 为唯一事实源，MUST NOT 从目录反推分支；`rand4` 为 4 位小写字母数字随机后缀（crypto/rand）。熵失败语义：Go 1.24 起 `crypto/rand` 底层熵失败为不可恢复 fatal（进程终止，天然满足零副作用）；实现保留 error 返回路径作为可注入熵源的防御 seam，**当使用可注入熵源且其返回错误时 MUST 返回错误且零副作用**。目录碰撞检测 MUST 在落库前以无副作用的存在性检查完成：碰撞时重新生成后缀（≤3 次），3 次均碰撞 MUST 返回错误且不产生任何副作用。路径在创建时确定并落库，此后删除/挂起/激活/重试等全部生命周期操作 MUST 按 DB 记录的 `worktree_path` 执行，**MUST NOT 按新格式重算**——既有任务（含旧 `<projectID>/<taskID>` 格式路径）行为不变，不做迁移。worktree 创建在任何文件/git 副作用前 MUST 通过 `<dataDir>/worktrees` 根的包含性校验。

创建流程在 worktree 创建成功后、提交 suspended 前，SHALL 执行项目配置的 inherit 文件继承（语义见 project-lifecycle-config spec），inherit 失败 MUST NOT 阻断创建。提交 suspended 后：若项目配置了 init script，SHALL 先异步执行 init 并仅在成功后触发自动激活；init 失败 MUST NOT 触发激活，任务保持 suspended 且 init_status=failed（init 状态机见 project-lifecycle-config spec）；激活失败任务落挂起并记录 last_error，用户可手动重试激活。项目未配置 inherit/init 时，创建流程与既有行为完全一致。

#### Scenario: 创建任务

- **WHEN** 用户在项目下创建任务（提供任务名称）
- **THEN** 系统创建 worktree 与分支，任务进入挂起状态，随后**自动触发激活**（异步启动进程组并锚定 session）；激活失败任务落挂起并记录 last_error，用户可手动重试激活

#### Scenario: LLM 生成语义化分支名

- **WHEN** AI 配置可用，用户以中文任务名（如「接入AI与worktree命名优化」）创建任务
- **THEN** 系统调用 LLM 生成英文 slug，分支名为 `ocdeck/<ai-slug>`（如 `ocdeck/ai-worktree`），worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<ai-slug>-<rand4>/`（AI 路径下目录段与分支 slug 一致）

#### Scenario: AI 未配置或失败时回退

- **WHEN** AI 未配置、调用失败/超时、或输出未通过清洗门禁
- **THEN** 系统回退到机械 slugify 生成分支名，AI 错误不向用户暴露；创建流程继续，随后仍遵循既有前置检查语义（如分支冲突时报错）

#### Scenario: 新路径格式的人类可读目录

- **WHEN** 创建新任务
- **THEN** worktree 目录为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>/`（branchPathSlug 为分支名去 `ocdeck/` 前缀后截断 ≤50 字符的目录段，分支名本身不变），项目名与分支语义可从路径直接辨认；存量旧格式任务的目录与全部生命周期操作（含创建重试）不受影响

#### Scenario: 纯中文项目名的目录回退

- **WHEN** 项目名规范化后为空（如纯中文项目名）
- **THEN** 目录第一段为 `project-<projectID前8位>`，保证非空、合法、可区分

#### Scenario: 目录碰撞重试

- **WHEN** 落库前的存在性检查发现目标目录已存在
- **THEN** 系统重新生成 4 位随机后缀重试（≤3 次）；3 次均碰撞则返回错误，不产生落库或 worktree 副作用

#### Scenario: 配置 init 的项目创建任务

- **WHEN** 项目配置了 init script，创建任务
- **THEN** worktree 创建 → inherit 复制 → 挂起（init_status=pending）→ init 执行成功 → 自动激活

#### Scenario: init 失败停留在挂起

- **WHEN** 创建链中 init script 执行失败
- **THEN** 任务保持挂起、init_status=failed、init_error 落库，无 serve/tui 会话，用户可查看日志并 Re-run

#### Scenario: 未配置项目行为不变

- **WHEN** 项目未配置 inherit patterns 与 init script，创建任务
- **THEN** 创建流程与既有行为一致：worktree 创建后直接自动激活，init_status=none

#### Scenario: Probe 冷启动重试

- **WHEN** 创建后自动激活时 capability probe 因冷启动超时/网络类错误（ErrServeNotReady）失败
- **THEN** 系统保持 serve 会话并短退避重试（共 3 次尝试，退避 2s/4s）；任一次成功则激活继续；全部失败才落 suspended 并记录 last_error（语义与现状一致）。结构不兼容（ErrCapabilityMismatch）与凭据错误（ErrUnauthorized）不重试

#### Scenario: 分支名冲突

- **WHEN** 生成的分支名已存在（无论由 LLM 生成还是 slugify 回退）
- **THEN** 系统报错并提示用户更换任务名称
