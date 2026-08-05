# AI Provider Config Specification

## Purpose

定义全局 AI provider 配置（openai/anthropic）的存储、读写 API、运行时快照与热更新语义、思考强度（thinking）下发与能力协商、以及前端 AI 配置页行为。该配置是平台 LLM 驱动功能（首个场景：任务分支命名）的共享底座。

## Requirements

### Requirement: AI provider 配置存储

系统 SHALL 将全局 AI provider 配置存储于 `<dataDir>/ai.json`（默认 `~/.ocdeck/ai.json`），字段为 `provider`（枚举：`openai` | `anthropic`）、`api_key`、`base_url`（可选，空表示 provider 默认端点）、`model`（必填非空）、`thinking`（可选枚举：`""` | `off` | `low` | `medium` | `high`，空表示不下发思考参数）。写入 MUST 采用临时文件 + 原子 rename，文件权限 MUST 为 0600。`provider` 为枚举以外的值、`model` 为空、`thinking` 为枚举以外的值、或 `base_url` 非空但不是合法 http(s) URL 时 MUST 拒绝保存并返回 422（`invalid_input`，遵循项目错误惯例）。系统 MUST NOT 在日志中输出 `api_key` 明文。

#### Scenario: 保存合法配置

- **WHEN** 用户提交 `provider=openai`、非空 `api_key` 与 `model`、可选 `base_url`
- **THEN** 配置以 0600 权限原子写入 `<dataDir>/ai.json` 并立即生效（后续 LLM 调用使用该配置，无需重启服务）

#### Scenario: 拒绝非法配置

- **WHEN** 提交的 `provider` 不在枚举内，或 `model` 为空，或 `base_url` 非空但不是合法 http(s) URL
- **THEN** 保存被拒绝并返回 422 `invalid_input`，原配置文件保持不变

### Requirement: AI 配置读写 API

系统 SHALL 提供全局 AI 配置的读取与保存接口（`GET/PUT /api/v1/ai/config`），鉴权与 server 其他管理 API 一致。

GET MUST 返回 `{configured, provider, base_url, model, thinking, api_key_masked, load_error?}`：未配置（文件不存在）时 `configured=false` 且其余字段为空；**配置文件损坏或不可读时 MUST NOT 返回 500**，而是 `configured=false` 且携带人类可读的 `load_error`。`api_key_masked` 规则：key 长度 ≥ 8 时回显前 4 位 + `***`；长度 < 8 时为纯 `***`；无 key 时为空字符串。响应中 MUST NOT 出现完整 key。

PUT 时若 `api_key` 字段为掩码值（含 `***`）或空字符串，MUST 保留已存储的原 key；若无已存储 key 且提交空/掩码 key，MUST 返回 422。PUT 成功返回 200 与 GET 同形响应。

#### Scenario: 读取已配置的配置

- **WHEN** AI 配置已存在，客户端请求 GET
- **THEN** 返回 `configured=true`、`provider`、`base_url`、`model` 与掩码后的 `api_key`，响应中不存在完整 key

#### Scenario: 配置文件损坏时降级读取

- **WHEN** `<dataDir>/ai.json` 存在但 JSON 损坏或无读取权限，客户端请求 GET
- **THEN** 返回 `configured=false` 与人类可读的 `load_error`，不返回 500；运行时 LLM 功能同步走降级路径

#### Scenario: 保存时保留原 key

- **WHEN** 用户仅修改 `model` 并以掩码值（或空）提交 `api_key`
- **THEN** 保存成功，已存储的 `api_key` 不被清空

#### Scenario: 无旧 key 时拒绝空 key

- **WHEN** 从未保存过 key，用户以空或掩码值提交 `api_key`
- **THEN** 返回 422 `invalid_input`，配置不落盘

### Requirement: LLM 可用性判定与运行时快照

系统 SHALL 在 server 启动时加载 AI 配置到内存快照：文件不存在为正常未配置态；文件损坏/不可读时 MUST 记录 load_error 并日志告警，**MUST NOT 拒绝启动**。AI 可用（configured）的判定 MUST 为：`provider` 合法、`api_key` 与 `model` 均非空、且无 load_error。PUT 保存成功后 MUST 原子替换内存快照；读方单次 LLM 操作全程使用同一快照（无 data race、无半新半旧混用）。并发 PUT MUST 由写锁串行化「读取旧 key 合并 → 校验 → 文件原子写入 → 快照替换」全过程，文件写入失败时 MUST 保持旧快照不变并返回错误；任意并发 PUT 序列下内存快照与磁盘最终一致（按写锁获得顺序 last-writer-wins）。未配置时依赖 LLM 的功能 MUST 走各自降级路径（对分支命名为 slugify 回退），MUST NOT 报错阻断主流程。

#### Scenario: 未配置时功能降级

- **WHEN** `<dataDir>/ai.json` 不存在或字段不完整
- **THEN** LLM 功能静默走降级路径，主流程（如任务创建）行为与无 AI 时一致

#### Scenario: 损坏配置不阻断启动

- **WHEN** server 启动时 `<dataDir>/ai.json` JSON 损坏
- **THEN** server 正常启动，AI 判定为不可用（configured=false + load_error），日志告警，后续 PUT 合法配置可覆盖修复

#### Scenario: 配置热更新

- **WHEN** 用户通过 API 保存新的 AI 配置
- **THEN** 内存快照原子替换，下一次依赖 LLM 的操作立即使用新配置，无需重启 server

### Requirement: 思考强度（thinking）下发与能力协商

当 `thinking` 非空时，LLM 请求 MUST 按 provider 映射下发思考参数：Anthropic 协议 `off` → `thinking:{"type":"disabled"}`，`low`/`medium`/`high` → `thinking:{"type":"enabled","budget_tokens":1024/4096/16384}`（此时 MUST 保证 `max_tokens > budget_tokens`，不足自动提升）；OpenAI 协议 `off` → `reasoning_effort:"minimal"`，其余 → `reasoning_effort:"low"/"medium"/"high"`。`thinking` 为空时 MUST NOT 下发任何思考参数（跟随模型/网关默认）。无论目标是否为思考模型配置都 MUST NOT 破坏调用：若 4xx 错误体表明不支持思考参数，MUST 剥离该参数原样重试一次，再失败才按失败语义回退。

#### Scenario: 关闭思考

- **WHEN** `thinking=off` 且 provider=anthropic，发起 LLM 调用
- **THEN** 请求体含 `thinking:{"type":"disabled"}`，响应无 thinking 块

#### Scenario: 思考预算档位

- **WHEN** `thinking=low` 且 provider=anthropic，发起 LLM 调用
- **THEN** 请求体含 `thinking:{"type":"enabled","budget_tokens":1024}`，且 `max_tokens` 自动提升至大于 budget_tokens

#### Scenario: OpenAI 协议映射

- **WHEN** `thinking=high` 且 provider=openai，发起 LLM 调用
- **THEN** 请求体含 `reasoning_effort:"high"`

#### Scenario: 非思考模型自动降级

- **WHEN** 目标模型不支持思考参数（4xx 且错误体表明 unsupported/unknown parameter）
- **THEN** 系统剥离思考参数原样重试一次，调用行为与未配置思考强度一致；重试仍失败才走失败回退

#### Scenario: 默认不下发

- **WHEN** `thinking` 为空（未设置）
- **THEN** 请求体不含 thinking/reasoning_effort 参数，行为与既有版本一致

### Requirement: AI 配置页

前端 SHALL 提供 AI 配置页，包含：`provider` 下拉（openai/anthropic）、`api_key` 密码输入框（展示掩码值，可覆盖输入）、`base_url`、`model` 输入框、`thinking` 下拉（默认/关闭/低/中/高，附高超度延迟提示）与保存按钮。保存成功/失败 MUST 有明确反馈；后端校验失败（422）时 MUST 展示错误原因；GET 返回 `load_error` 时 MUST 在页面展示。

#### Scenario: 配置 AI provider

- **WHEN** 用户在配置页选择 provider、填写 api_key 与 model 并保存
- **THEN** 页面提示保存成功，配置立即生效

#### Scenario: 展示已保存配置

- **WHEN** 用户打开 AI 配置页且配置已存在
- **THEN** 页面展示 provider/base_url/model 与掩码后的 api_key

#### Scenario: 展示配置加载错误

- **WHEN** 配置文件损坏，用户打开 AI 配置页
- **THEN** 页面展示 load_error 提示，用户可直接重新保存合法配置修复
