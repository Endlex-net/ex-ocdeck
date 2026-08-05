# Proposal: ai-worktree-naming

## Why

当前 worktree 目录为双层 32 位 hex（`<dataDir>/worktrees/<projectID>/<taskID>`），人不可读，在 Finder/终端中无法辨认属于哪个项目、哪个任务；分支名由机械 slugify 生成，中文任务名会被过滤成无意义短横线（如「接入AI与worktree命名优化」→ `ocdeck/ai-worktree` 纯属巧合，多数情况下退化为 `ocdeck/task`）。平台未来会有大量 LLM 驱动/生成功能，需要先有统一的 AI provider 配置底座。本次以「中文任务名 → 语义化英文分支名」为第一个 LLM 落地场景，同时把 worktree 目录改为人类可读的 `项目名/分支slug+随机短编号`。

## What Changes

- 新增全局 AI provider 配置：`<dataDir>/ai.json`（默认 `~/.ocdeck/ai.json`），支持 `openai` 与 `anthropic` 两个 provider，字段含 `provider`、`api_key`、`base_url`（可选）、`model`；提供后端读写 API 与前端配置页。
- 新增 LLM 分支命名：创建任务时，若 AI 配置可用，调用 LLM 将任务名（通常为中文）翻译/提炼为语义化英文 slug，分支名为 `ocdeck/<ai-slug>`；AI 未配置、调用失败或返回非法结果时 MUST 回退到现有 slugify 逻辑，创建流程不被 AI 阻断。
- **BREAKING（路径格式）**：新建任务的 worktree 路径从 `<dataDir>/worktrees/<projectID>/<taskID>` 改为 `<dataDir>/worktrees/<projectName-slug>/<branchPathSlug>-<rand4>`（branchPathSlug 为分支名去 `ocdeck/` 前缀后截断 ≤50 字符的目录段，分支名本身不变；`rand4` 为 4 位小写字母数字随机后缀，防目录冲突）。已存在的旧格式 worktree 不迁移，继续按 DB 中记录的 `worktree_path` 正常工作。
- 分支名冲突处理保持不变（报错提示），AI 生成与回退路径共用同一冲突语义。

## Capabilities

### New Capabilities

- `ai-provider-config`: 全局 AI provider 配置（openai/anthropic）的存储、读写 API、前端配置页，以及面向未来 LLM 功能的统一配置加载与可用性判定语义。

### Modified Capabilities

- `task-lifecycle`: 「任务创建」requirement 变化——分支名由 LLM 生成（带回退）替代纯机械 slugify；worktree 路径格式从 `<projectID>/<taskID>` 改为人类可读的 `<projectName-slug>/<branchPathSlug>-<rand4>`。

## Impact

- **代码**：
  - 新增 `internal/ai/`（provider 配置加载 + LLM client 抽象，openai/anthropic 实现）
  - `internal/task/crud.go`（分支名生成接入 LLM + 回退；worktreePath 新格式）
  - `internal/worktree/worktree.go`（`worktreePath`/`validateIdent`/`Add` 适配新路径段）
  - `internal/api/`（AI 配置读写 API 路由）
  - `cmd/ocdeck-server/main.go`（AI 配置加载与 wiring）
  - `web/`（AI 配置页 + API client）
- **配置**：新增 `<dataDir>/ai.json`（含 api_key，需 0600 权限与 .gitignore 外的本地存储）。
- **数据库**：无 schema 变更（`tasks.worktree_path` 已存全路径，新旧格式共存由 DB 记录隔离）。
- **外部契约**：OpenAI Chat Completions API 与 Anthropic Messages API（仅出站调用，失败一律回退）。
- **兼容性**：存量任务的 worktree 路径、分支名不变；删除/挂起/激活等生命周期操作均按 DB 记录路径执行，不受新格式影响。
