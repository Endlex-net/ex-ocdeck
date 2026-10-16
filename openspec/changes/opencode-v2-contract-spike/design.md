# Design: opencode v2 实机契约 spike

## Context

动机与范围见 proposal.md（Why / What Changes）。本设计只回答 spike 怎么做。

当前契约治理现状（写作前已核实原文）：

- SPI 契约已冻结：`internal/application/agentprovider/agentprovider.go:81-190` 定义 8 个窄接口（RuntimeSupervisor / SessionControl / EventSource / AttentionIO / AsyncPrompt / StatusReader / TerminalIO / TitleUpdater）+ Provider 聚合接口与 7 个能力原语（`agentprovider.go:341-348`，session_control / event_source / attention_io / async_prompt / status_reader / terminal_io / title_updater）。spike 的探测覆盖矩阵以此为准。
- v1 契约检查：`scripts/check-opencode-contract.sh` 做两件事——24 个上游源码锚点文件在两个 ref 间的静态 diff（`check-opencode-contract.sh:17-43`），以及一段**手工执行**的 live-probe checklist（`check-opencode-contract.sh:96-177`，覆盖 health 认证、session CRUD、status、permission/question、event `server.connected`、attach 拒绝/共享 session、`/doc` 中 prompt_async 形状断言）。opencode 上游仓库为 `anomalyco/opencode`（`check-opencode-contract.sh:15`）。
- 版本门禁：`internal/config/config.go:41-42` 的 `ContractBaseline`/`ContractMinVersion` 仅产生告警（`config.go:299`），激活由能力探测门禁——v1/v2 不兼容场景下，v2 机器上 v1 探测会 fail-closed。
- binding 模型：`internal/domain/agentprovider/agentprovider.go:39-46` 的 TaskBinding envelope（Version / ProviderID / ProviderInstanceID / UpstreamSessionID / TerminalRef / Data）；命名空间常量 `ProviderIDV1 = "opencode-v1"`、`ProviderInstanceV1Default = "default"`（`internal/infrastructure/store/provider_namespace.go:20-21`）。承接语义设计以此模型为基础。
- v2 迁移设计 `docs/design/opencode-v2-a-plus-migration.md` 基于 2.0.23 草案，未经实机验收。

## Goals / Non-Goals

**Goals:**

- 锁定一个具体的 opencode v2 目标版本（精确版本号 + 获取方式 + 锁定日期），替代 2.0.23 草案作为后续实现 change 的验收基线。
- 产出可重跑的 v2 契约探测脚本集（静态锚点 diff + 自动化 live probe），覆盖 SPI 全部 8 窄接口（+ Provider 聚合）对应的外部行为面与 diffreview 依赖操作。
- 产出 v1↔v2 协议差异清单：逐项标注 兼容 / 不兼容 / 待验证；每项结论必须有实机探测或上游源码锚点证据，仅有文档依据的标「待验证」。
- 实测验证单向数据迁移契约（proposal 第 12 行）：v1 数据可自动迁移至 v2、v2 环境可打开迁移前数据；反向行为仅记录，不作承诺。
- 产出承接契约与跨版本兼容边界的设计结论（命名空间、归属、判定规则、失败语义），作为后续 v2 provider 实现 change 的输入。
- 产出 go/no-go 建议与后续实现 change 的范围输入。

**Non-Goals:**

- v2 provider 实现；C19 / F3 等代码修复；事件路由与共享 server 生命周期改造；codex / pi；开发机升级；任何运行时行为变更（proposal 非目标的子集，不重复展开）。
- 探测脚本的长期工程化（CI 集成、跨平台）——spike 阶段以可重跑为准，长期治理形态由后续实现 change 决定（D3 保留稳定 probe ID 与结果 schema 作为最小钩子）。

## Decisions

### D1：目标版本 = 锁定时解析出的具体 v2 stable release，双侧版本基线完整记录

- 方法：从 `anomalyco/opencode` release 列表解析最新 v2 stable（排除 pre-release/草案），在锁定时刻固化为具体版本号——实验输入是「解析出的具体版本」，不是「latest」。
- **两段式 manifest（消除启动循环）**，JSON schema 固定如下（字段名/类型/必填性唯一，缺失必填字段即未锁定，schema 校验失败 → 退出码 2；**未知字段一律拒绝**）：

```json
{
  "schema_version": 1,
  "v1":  { "side": "v1", "version": "1.18.26", "release_url": "...", "tag": "...", "commit_sha": "...", "asset_name": "...", "os": "...", "arch": "...", "download_url": "...", "official_sha256": "...", "source_chain_status": "verified" },
  "v2":  { "side": "v2", "version": "...", "release_url": "...", "tag": "...", "commit_sha": "...", "asset_name": "...", "os": "...", "arch": "...", "download_url": "...", "official_sha256": "...", "source_chain_status": "..." }
}
```

上例为**候选 manifest 的结构模板**（`"..."` 占位值须替换为实际值后才能通过校验）：候选态必填 = `schema_version` + 每侧 `side`/`version`/`release_url`/`tag`/`commit_sha`/`asset_name`/`os`/`arch`/`download_url`/`official_sha256`/`source_chain_status`（prepare 下载校验所需的全部来源字段）；锁定态在此基础上补全下表「候选态=省略」的五个侧对象字段（`locked_at`/`download_sha256`/`sha256_match`/`exec_path`/`runtime_version_output`），并补全顶层 `canary_target` 与 `boundary_mechanism`（及条件必填的 `boundary_detail`）。

每侧对象字段（候选 manifest 与锁定 manifest 共用结构）：

| 字段 | 类型 | 候选态 | 锁定态 | 含义 |
| --- | --- | --- | --- | --- |
| `side` | string `"v1"`/`"v2"` | 必填 | 必填 | 版本侧 |
| `version` | string semver | 必填 | 必填 | 精确版本号 |
| `release_url` | string URL | 必填 | 必填 | release 页面 |
| `tag` | string | 必填 | 必填 | git tag |
| `commit_sha` | string 40-hex | 必填 | 必填 | tag 解析的不可变 commit |
| `asset_name` | string | 必填 | 必填 | 下载资产/包名 |
| `os` / `arch` | string | 必填 | 必填 | 平台 |
| `download_url` | string URL | 必填 | 必填 | 精确下载来源 |
| `official_sha256` | string 64-hex 或 `null` | 必填（可 null） | 必填（可 null） | 官方公示校验值；缺失统一用 JSON `null`（禁止空串/省略字段） |
| `source_chain_status` | string `"verified"`/`"unverified"` | 必填 | 必填 | `official_sha256=null` 或包渠道↔release 对应关系无法证实时为 `unverified` → 停止 spike |
| `locked_at` | string RFC3339 | 省略 | 必填 | 锁定时间 |
| `download_sha256` | string 64-hex | 省略 | 必填 | 实测下载校验值 |
| `sha256_match` | bool | 省略 | 必填 | 实测 vs 官方比对；`official_sha256=null` 时为 `false` |
| `exec_path` | string 绝对路径 | 省略 | 必填 | 沙箱内解包后执行路径 |
| `runtime_version_output` | string | 省略 | 必填 | 门禁下 `--version` 输出原文 |

顶层字段：
- `canary_target`：类型 string（canonical 绝对路径，经 realpath 校验且不得为 sandbox-root 子路径）；候选态可省略（显式提供时须通过 D2 边界校验），锁定态必填——记录 prepare 实际使用的最终路径。
- `boundary_mechanism`：类型 string，枚举 `vm` / `container` / `sandbox-exec` / `other`；候选态**禁止出现**，锁定态必填——记录 D2 执行边界的实际形态，来源为调用方提供的 `SPIKE_BOUNDARY_MECHANISM`（D2 边界生命周期）。
- `boundary_detail`：类型 string；仅当 `boundary_mechanism=other` 时锁定态必填，其余情形（含候选态）一律**禁止出现**。

D3 四个脚本均消费/产出本 schema；静态 diff 的 source ref 取 `commit_sha`。

**正式 JSON Schema（draft 2020-12，四脚本统一引用本定义校验）**——候选态与锁定态为两个独立 schema，validator 按当前校验目标二选一（prepare 输入校验用 candidate，prepare 产出与下游脚本输入校验用 locked）：

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$defs": {
    "fields": {
      "side": { "enum": ["v1", "v2"] },
      "version": { "type": "string", "pattern": "^(0|[1-9]\\d*)\\.(0|[1-9]\\d*)\\.(0|[1-9]\\d*)$" },
      "release_url": { "type": "string", "format": "uri", "pattern": "^https://" },
      "tag": { "type": "string", "minLength": 1 },
      "commit_sha": { "type": "string", "pattern": "^[0-9a-f]{40}$" },
      "asset_name": { "type": "string", "minLength": 1 },
      "os": { "type": "string", "minLength": 1 },
      "arch": { "type": "string", "minLength": 1 },
      "download_url": { "type": "string", "format": "uri", "pattern": "^https://" },
      "official_sha256": { "type": ["string", "null"], "pattern": "^[0-9a-f]{64}$" },
      "source_chain_status": { "enum": ["verified", "unverified"] },
      "locked_at": { "type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z$" },
      "download_sha256": { "type": "string", "pattern": "^[0-9a-f]{64}$" },
      "sha256_match": { "type": "boolean" },
      "exec_path": { "type": "string", "pattern": "^/" },
      "runtime_version_output": { "type": "string" }
    },
    "sideCandidate": {
      "type": "object",
      "additionalProperties": false,
      "required": ["side", "version", "release_url", "tag", "commit_sha", "asset_name", "os", "arch", "download_url", "official_sha256", "source_chain_status"],
      "properties": {
        "side": { "$ref": "#/$defs/fields/side" },
        "version": { "$ref": "#/$defs/fields/version" },
        "release_url": { "$ref": "#/$defs/fields/release_url" },
        "tag": { "$ref": "#/$defs/fields/tag" },
        "commit_sha": { "$ref": "#/$defs/fields/commit_sha" },
        "asset_name": { "$ref": "#/$defs/fields/asset_name" },
        "os": { "$ref": "#/$defs/fields/os" },
        "arch": { "$ref": "#/$defs/fields/arch" },
        "download_url": { "$ref": "#/$defs/fields/download_url" },
        "official_sha256": { "$ref": "#/$defs/fields/official_sha256" },
        "source_chain_status": { "$ref": "#/$defs/fields/source_chain_status" }
      }
    },
    "sideLocked": {
      "type": "object",
      "additionalProperties": false,
      "required": ["side", "version", "release_url", "tag", "commit_sha", "asset_name", "os", "arch", "download_url", "official_sha256", "source_chain_status", "locked_at", "download_sha256", "sha256_match", "exec_path", "runtime_version_output"],
      "properties": {
        "side": { "$ref": "#/$defs/fields/side" },
        "version": { "$ref": "#/$defs/fields/version" },
        "release_url": { "$ref": "#/$defs/fields/release_url" },
        "tag": { "$ref": "#/$defs/fields/tag" },
        "commit_sha": { "$ref": "#/$defs/fields/commit_sha" },
        "asset_name": { "$ref": "#/$defs/fields/asset_name" },
        "os": { "$ref": "#/$defs/fields/os" },
        "arch": { "$ref": "#/$defs/fields/arch" },
        "download_url": { "$ref": "#/$defs/fields/download_url" },
        "official_sha256": { "$ref": "#/$defs/fields/official_sha256" },
        "source_chain_status": { "$ref": "#/$defs/fields/source_chain_status" },
        "locked_at": { "$ref": "#/$defs/fields/locked_at" },
        "download_sha256": { "$ref": "#/$defs/fields/download_sha256" },
        "sha256_match": { "$ref": "#/$defs/fields/sha256_match" },
        "exec_path": { "$ref": "#/$defs/fields/exec_path" },
        "runtime_version_output": { "$ref": "#/$defs/fields/runtime_version_output" }
      }
    },
    "candidate": {
      "type": "object",
      "additionalProperties": false,
      "required": ["schema_version", "v1", "v2"],
      "properties": {
        "schema_version": { "const": 1 },
        "canary_target": { "type": "string", "pattern": "^/" },
        "v1": { "$ref": "#/$defs/sideCandidate" },
        "v2": { "$ref": "#/$defs/sideCandidate" }
      }
    },
    "locked": {
      "type": "object",
      "additionalProperties": false,
      "required": ["schema_version", "v1", "v2", "canary_target", "boundary_mechanism"],
      "properties": {
        "schema_version": { "const": 1 },
        "canary_target": { "type": "string", "pattern": "^/" },
        "boundary_mechanism": { "enum": ["vm", "container", "sandbox-exec", "other"] },
        "boundary_detail": { "type": "string", "minLength": 1 },
        "v1": { "$ref": "#/$defs/sideLocked" },
        "v2": { "$ref": "#/$defs/sideLocked" }
      },
      "allOf": [
        {
          "if": { "properties": { "boundary_mechanism": { "const": "other" } } },
          "then": { "required": ["boundary_detail"] },
          "else": { "not": { "required": ["boundary_detail"] } }
        }
      ]
    }
  }
}
```

校验口径：候选态 = `$defs.candidate`（`additionalProperties:false` 使 `boundary_mechanism`/`boundary_detail` 候选态禁止出现）；锁定态 = `$defs.locked`（含锁定字段必填与 `boundary_detail` 条件规则）。`version` pattern 不允许 pre-release/build metadata；URL 限 https scheme；`locked_at` 为秒级 UTC RFC3339。

**manifest 语义校验表（结构合法之外的不变量，四脚本共同引用；除表中单独标注外任一违反 → 退出码 2，且来源链门禁通过前禁止执行任何二进制）**：

| 校验项 | 规则 | 失败语义 |
| --- | --- | --- |
| side 一致性 | `v1.side=="v1"` 且 `v2.side=="v2"`（与对象键相等） | 输入自相矛盾 → 退出码 2（下载前） |
| 主版本约束 | v1 侧 major==1，v2 侧 major==2 且为 stable（排除 pre-release） | 同上 → 退出码 2 |
| 来源链 | `source_chain_status=verified` 要求 `official_sha256` 非 null 且 release↔tag↔commit、release↔asset↔checksum 对应关系证据落 `evidence/<run-id>/source-chain/`；无法证实 → `unverified` → 停止 spike（退出码 2），禁止下载后执行二进制 | 退出码 2，spike 停止 |
| 下载校验（锁定态） | `sha256_match=true` 且 `download_sha256==official_sha256`；校验失败按重试契约（D5 统一重试契约第 2 条）重试，耗尽 → 退出码 2 | 退出码 2 |
| 运行时版本（锁定态） | `--version` 输出解析的版本号必须等于 manifest `version` | 不符 → **退出码 4**（准备阶段基线验证故障：二进制已执行，不属「未启动上游操作」；保留执行证据，不产出有效锁定 manifest） |
- **来源链校验**：静态 diff 的 source ref 必须使用锁定 manifest 中的不可变 commit SHA；官方公示校验值缺失或包发布渠道与 GitHub release 的对应关系无法证实时，标记「来源链未证实」并停止——不得把静态与 live 证据当作同一基线下结论。
- v1 侧基线 = 项目 `ContractBaseline`（当前 `1.18.26`，`internal/application/agentprovider/occlient.go:25`），按同一两段式模板记录并保存选择依据；**不使用**「开发机当时碰巧安装的版本」。
- 无符合条件的 v2 stable release 时：停止 spike，记录待用户决策的替代路径（采用 pre-release 属用户权责，不预选）。
- 备选：直接以 2.0.23 草案验收——被拒绝：草案未发布、契约可能再变，验收结论不可复用。
- 备选：等 v2 长期稳定后再 spike——被拒绝：用户已明确 v1/v2 需长期并存维护，等待不消除契约盘点工作，只推迟它。

### D2：spike 环境 = 隔离沙箱 + 实际阻止越界写入的执行边界

- 沙箱布局：独立实验根目录（`openspec/changes/opencode-v2-contract-spike/sandbox/`，gitignore），内含独立 `HOME`、`XDG_CONFIG_HOME`/`XDG_DATA_HOME`/`XDG_STATE_HOME`/`XDG_CACHE_HOME`、`TMPDIR` 与实验 cwd；二进制下载/解包只落在沙箱内，禁止安装到全局 PATH、禁止修改宿主 shell 配置。
- **规范真实沙箱根（唯一定义）**：`sandbox_root` = 脚本自身所在仓库根经 `git rev-parse --show-toplevel` 定位后拼接 `openspec/changes/opencode-v2-contract-spike/sandbox`，再经 `realpath` 解析；允许根仅此一个，传入的 sandbox-root 参数解析后必须等于该值，不等 → 退出码 2。
- **canary 契约**：canary 目标路径必须由调用方显式提供（`prepare-env.py` 的 `canary_target` 参数或候选 manifest 顶层字段），**无默认值——两者都缺失 → 退出码 2**。**取值优先级**：CLI 参数与候选 manifest 的 `canary_target` 同时提供时，canonical 路径必须一致，不一致 → 退出码 2；仅一方提供时用该值。最终路径在首次 canary 前确定并完成校验（此时仅记录于 prepare 准备证据），prepare 全部校验成功时随完整锁定 manifest 原子发布写入；后续阶段只从锁定 manifest 读取。校验规则：①必须位于**验证实例之内**、且在允许写入集合（D2）**之外**（含不得为 sandbox-root 子路径），不满足 → 退出码 2——调用方负责提供满足该谓词的实例内路径（如实例内 `$HOME/.ocdeck-spike-canary-<run-id>`），脚本只做 canonical/存在性/集合校验，不验证「是否开发机」（该保证由 D2 执行环境约束承担）；②执行前校验目标**不存在**，已存在 → 退出码 2（禁止覆盖/清理宿主既有文件）；③在边界内尝试创建该哨兵，**必须被拒绝**；④拒绝证据（错误输出原文）写入 `evidence/<run-id>/canary.log`；⑤**不得**删除宿主任何文件——若哨兵意外创建成功（边界无效），保留现场、在 canary.log 与 summary.md 报告残留路径，由用户处置。**挑战有效性前置条件**：canary 目标父目录必须存在，且调用方在建立边界前须确认同一执行身份原本具备该目标的创建权限（依据记入 `canary.log`）；`ENOENT`、无法归因于边界策略的权限/文件系统错误只能证明**挑战无效** → 退出码 2，不得作为边界有效证据。
- **canary 阶段判定表**（与退出码契约一致）：

| 情形 | 处置 |
| --- | --- |
| canary_target 校验失败 / 目标已存在 | 输入拒绝 → 退出码 2，未启动实验 |
| 首次验证：哨兵创建被边界拒绝 | 边界有效，继续 |
| 首次验证：哨兵创建成功（未被拒绝） | 边界无效 → 退出码 2 前置停止；意外哨兵保留现场并报告残留 |
| 复验无法执行（锁定 manifest 缺 canary_target 等） | 退出码 2 |
| 阶段前置复验：哨兵创建成功（边界未生效） | 退出码 2 前置拒绝（本阶段上游操作未启动）；意外哨兵保留现场并报告残留 |
| 阶段前置门禁通过后、上游操作启动后发现允许写入集合之外的实际写入 | 退出码 5：立即停止全部实验、保留现场、受影响证据无效（**5 仅限此情形**） |

「零实验副作用」统一解释为**未启动任何上游实验进程**；允许的准备性写入 = 沙箱目录、evidence 日志、canary 哨兵创建尝试（被拒绝时无残留）。
- **prepare 前置校验（先于任何创建/下载/解包）**，两条独立谓词：①传入的 sandbox-root 参数解析后（含符号链接解析）必须**等于**规范真实沙箱根，其祖先链不得含使该路径逃逸到其他位置的符号链接，不等 → 退出码 2（目录不存在时按「先校验父链、再创建该目录」的顺序处理）；②下载/解包/实验写入目标解析后必须**位于**规范根**之内**（证据写入例外：只允许写入本次 run 的 canonical evidence 根，见 D2 允许写入集合），拒绝归档内路径穿越（`../` 类条目）。拒绝或清除继承的宿主 `HOME`/`XDG_*`/`TMPDIR` 及上游配置路径覆盖类环境变量。发现上游额外写入路径时，只能通过配置/环境变量/实例内路径**重定位**使其实际写入落入既定允许写入集合——**不得新增集合外的可写路径**；无法重定位时停止相应实验（该实验结果按环境不足类 INCONCLUSIVE 记录），重跑遵循新 run-id 规则。
- **执行环境（用户已锁定）**：spike 全程在**用户提供、与开发机隔离的验证实例**（VM / 容器 / 独立系统用户）中执行，**不在开发机本机执行**。在此之上的**执行边界（目录与变量隔离之外的硬边界，必需）**：任何上游二进制执行、安装器运行、模型调用，必须在具备文件系统写入限制的边界内进行。边界的实现形态：验证实例自身的机器/用户边界，或其内部再叠加 OS 级写限制机制（如 macOS `sandbox-exec` profile：allow 允许写入集合、deny 其余 `file-write*`）——`sandbox-exec` 类机制只是验证环境**内部**的写限制手段，**不能成为改在开发机执行的依据**。
- **允许写入集合（唯一定义）**：规范 sandbox 根 ∪ 本次 run 的 canonical evidence 根（`evidence/<run-id>/`，realpath 校验、不得经符号链接逃逸、run-id 唯一且不得覆盖旧 run）。边界只允许写这两个根；evidence 写入是授权写入，不触发退出码 5；其他 run 的 evidence 目录与真实宿主配置/数据路径仍不可写。canary 目标必须在该允许写入集合之外（D2 canary 校验规则①的「执行边界之外」即指此集合之外）。
- **边界生命周期**：执行边界是**环境属性**而非单脚本内部状态——由外部调用方（人工或环境启动器）在任何 spike 脚本执行前建立，生命周期覆盖 prepare/diff/live/migration 全部阶段；`run-all.py` 必须在同一已建立边界内顺序启动全部阶段。**四脚本均不承担建立/配置边界职责，只验证并使用**。**run-id 前置**：调用方在建立边界前先分配唯一 run-id 并经环境变量 `SPIKE_RUN_ID` 传入，使允许写入集合中的 evidence 根在边界建立时即已确定——禁止通过放行整个 evidence 父目录绕过「其他 run 不可写」。run-id 格式固定为完整匹配 `^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`（UTC 日期时间 + 8 位 hex 随机，禁路径分隔符及额外字符），非法 → 退出码 2。**run 目录两类校验**：①新 run 初始化入口（`run-all.py`）——检查 `evidence/<run-id>/` 不存在并原子创建，已存在 → 退出码 2；②同一 run 的阶段脚本（prepare/diff/live，无论经 run-all 还是单独调用）——目录**允许已存在**，校验其为规范 run 根（canonical 路径与 `SPIKE_RUN_ID` 对应、位于允许写入集合内）后继续使用，不执行「存在即拒绝」；单独调用 prepare 且目录尚不存在时由 prepare 承担原子创建（初始化责任）。边界描述输入固定为环境变量：调用方在任何脚本执行前设置 `SPIKE_BOUNDARY_MECHANISM`（枚举 `vm`/`container`/`sandbox-exec`/`other`；为 `other` 时必须同时设置 `SPIKE_BOUNDARY_DETAIL` 说明机制；缺失或取值非法 → 退出码 2）。每个脚本（含单独调用）在执行任何上游操作前必须先完成两项**边界验证**，任一失败 → 退出码 2（此时本阶段尚未启动上游操作，属前置拒绝）：①canary 复验（D2 canary 契约）——哨兵写入被边界拒绝 → 边界有效；写入**成功**（边界未生效）→ 拒绝执行；②边界描述一致性——`SPIKE_BOUNDARY_MECHANISM`（及 `boundary_detail`）与锁定 manifest 记录**相等**（prepare 阶段无锁定 manifest，改为校验后写入；后续阶段从锁定 manifest 读取比对）。canary 只能验证写入受限这一事实，不能识别边界机制种类——机制一致性完全依赖描述比对，脚本不得声称自动识别。
- **边界生效验证（canary）**：首次执行前，在边界内尝试向宿主真实路径（如真实 `$HOME` 下哨兵文件）写入，**必须被拒绝**；拒绝证据（错误输出）记入 `evidence/<run-id>/canary.log`。canary 未被拒绝 → 边界无效，停止 spike。仅目录/环境变量隔离且无法证明全部写入受控时，同样停止。
- **写入路径查明**：v2 是否写入沙箱之外的位置，只能在边界保护下探测；发现额外写入路径时记录证据，并按上条规则**重定位**（不是扩大允许写入集合）后重跑 canary。**禁止**「无边界先启动、发现越界后再补」的执行顺序。该门禁对锁定的 v1 基线二进制同等适用。
- 备选：仅目录 + 环境变量隔离——被拒绝：不能阻止上游程序忽略变量后直接写宿主绝对路径（评审 F12）。
- 备选：直接升级开发机——被用户明确拒绝（开发机暂不升级，数据迁移不可逆）。
- 执行安排：实机验证环境由用户提供；因此 `prepare-env.py` 与 manifest（D1）以可交接形态产出，用户可按清单在等价环境执行。

### D3：探测脚本三层 + orchestrator，行为契约固定可交接

**脚本清单与参数**：

- `scripts/spike-v2/prepare-env.py <candidate-manifest> <sandbox-root> <evidence-dir> [canary-target]`：创建沙箱与隔离环境、**验证**调用方已建立的执行边界（canary + `SPIKE_BOUNDARY_MECHANISM`/`SPIKE_BOUNDARY_DETAIL` 校验并写入锁定 manifest；canary-target 为可选参数：缺省时由候选 manifest 顶层 `canary_target` 提供，两者均缺失按 D2 退出码 2——**无默认值**）、下载双侧二进制并校验、门禁保护下执行 `--version`；产出锁定 manifest `<sandbox-root>/manifest.json` 与 `<evidence-dir>/canary.log`。`<evidence-dir>` 必须是规范 run 根（`evidence/<run-id>/`），缺失或不合规 → 退出码 2。门禁失败退出码 2，本阶段未产生任何实验副作用。**锁定 manifest 发布顺序（固定）**：解析并校验最终 canary 路径（记录于准备证据）→ 首次 canary → 来源链校验 → 下载与校验双侧二进制 → 门禁保护下执行 `--version` 校验 → 全部通过后**原子发布**完整锁定 manifest；任一失败不得留下可被下游消费的半成品锁定 manifest。**双文件提交协议**：①两份目标（`<sandbox-root>/manifest.json` 权威文件与 `<evidence-dir>/manifest.json` 副本）先各写临时文件，内容必须一致且通过锁定态 schema 校验；②固定发布顺序：先 rename 发布 evidence 副本，最后 rename 发布 sandbox 权威文件——**唯一提交点 = 权威文件 rename 完成**；③下游脚本只消费权威文件已发布的锁定 manifest，并以 evidence 副本逐字节比对验证一致性（不一致 → 退出码 4）；④任一发布步骤失败或收到信号 → 临时文件保留为证据、权威文件未提交视为未发布（下游不得消费），退出码 4。**上一 run 残留**：prepare 开始时若 `<sandbox-root>/manifest.json` 已存在，先将其移入本次 run evidence 归档为 `previous-manifest.json`（只读保留），再执行本次发布——避免上一 run 产物被误当本次成功结果。
- `scripts/spike-v2/check-contract-diff.py <locked-manifest> <evidence-dir>`：静态层。双侧 ref 从锁定 manifest 的 commit SHA 读取（不接受命令行裸 ref）；`<evidence-dir>` 必须是规范 run 根，否则退出码 2。锚点清单继承 `scripts/check-opencode-contract.sh` 的 24 项并允许追加 v2 侧新锚点（追加时更新数量断言）。
- `scripts/spike-v2/live-probe.py <locked-manifest> <evidence-dir> [--mode probes|migration]`：live 层，两模式执行集合互斥且不重叠——`probes`（默认）只执行 D4 矩阵中**非 `migration.*`** 的条目；`migration` 只执行 `migration.forward`/`migration.reverse`（D5 实验，D4 保留这两行用于覆盖核对与 D8 汇总）。evidence-dir 同上校验；将现有手工 checklist（`check-opencode-contract.sh:96-177`）固化为自动化断言。**migration 模式前置依赖**：probes 模式中 `supervisor.01-03`、`session.01-03`、`prompt.01` 已获得有效协议事实。**依赖满足的机械谓词**：该 probe 的 `evidence_validity=valid` **且** `exec_status ∈ {PASS, FAIL}`（FAIL 表示协议事实已获得但能力断言失败，仍提供事实）；`INCONCLUSIVE` 或证据无效一律不满足（无论 `reason`）。所需事实清单：supervisor.01 启动方式与参数、supervisor.02 认证方式、supervisor.03 健康就绪契约、session.01 创建契约、session.02 读取契约、session.03 列表契约、prompt.01 提交契约——依赖的是「协议事实已获得」，不是「能力断言通过」；若 prompt 能力实证 FAIL，`migration.forward` 的「session 可继续使用」断言判 FAIL（按 D6/D8 处置），其余数据读取类断言继续执行。前置事实缺失时两个迁移条目标 INCONCLUSIVE 并记录具体缺失项，禁止省略结果或回头调用完整 probes 模式。
- `scripts/spike-v2/run-all.py <candidate-manifest> [canary-target]`：orchestrator，校验并使用调用方经 `SPIKE_RUN_ID` 分配的 run-id 创建规范 evidence 根，按 prepare → diff → live(probes) → live(migration) 顺序串联；`canary-target` 原样透传给 prepare-env.py（「可选」仅表示可由候选 manifest 提供，不代表存在默认值），live/migration 阶段的 canary 复验从锁定 manifest 的 `canary_target` 字段读取（复验处置按 D2 canary 阶段判定表逐行执行）。**run-all 阶段状态机与退出码汇总**（唯一规则，summary.md 状态与 results.jsonl 必须与此一致）：

| 阶段退出码 | 处置 |
| --- | --- |
| 2（前置拒绝） | 立即终止全部流程，run-all 直接返回 2（覆盖此前累计的 1/3/4）；被拒绝阶段未启动任何上游操作，此前阶段已产出的证据保留 |
| 5（运行中安全越界） | 立即终止全部流程，run-all 直接返回 5（覆盖此前累计结果）；保留现场、受影响证据无效 |
| 4（harness 故障） | 该阶段受影响证据标记无效；**继续**后续独立阶段；最终汇总计入 4。**例外**：prepare 返回 4 时无有效锁定 manifest，依赖它的 diff/live/migration 全部无法执行 → 直接终止并返回 4 |
| 1（存在 FAIL） | 继续后续阶段；最终汇总计入 1 |
| 3（存在 INCONCLUSIVE 无 FAIL） | 继续后续阶段；最终汇总计入 3 |
| 0 | 继续后续阶段 |

run-all 最终返回码：任一阶段返回 2 或 5 时立即终止并直接返回该码；未遇 2/5 时按 `4 → 1 → 3 → 0` 优先级取最高者。脚本允许单独调用（参数契约同上，调用方自行保证 run 根一致且处于已验证边界内——单独调用同样先跑 canary 复验再执行任何上游操作），但推荐经 run-all.py 执行。

**证据归档**：固定根 `openspec/changes/opencode-v2-contract-spike/evidence/<run-id>/`（run-id 由调用方经 `SPIKE_RUN_ID` 环境变量提供，格式规范见 D2 run-id 前置；run-all.py 作为初始化入口校验唯一性并原子创建，阶段脚本按 D2 两类校验使用）；下设 `manifest.json`（锁定 manifest 副本）、`source-chain/`（来源链校验证据——D1 语义校验表要求的 release↔tag↔commit、release↔asset↔checksum 对应关系证据落此；来源链未证实时记录失败依据）、`static/`、`live/`、`migration/`、`canary.log`、`results.jsonl`、`summary.md`。重跑生成新 run-id，**不覆盖**旧 run。超时统一 30s（单项可覆盖）；收到信号或父进程退出时清理全部子进程；写入证据前对凭据（密码/token）脱敏。

**统一结果 schema**（每 probe/锚点一条 JSONL，落 `results.jsonl`）：

| 字段 | 取值 | 适用性 |
| --- | --- | --- |
| `run_id` / `probe_id` | 字符串；probe_id 跨 run 稳定，取值以统一封闭集合（静态锚点 ID ∪ D4 live probe ID ∪ 豁免记录 ID，三表字面值）为唯一规范，不再另行要求段式格式 | 全部必填 |
| `v1_baseline` / `v2_baseline` | 固定对象 `{"version": "<semver>", "commit_sha": "<40-hex>"}`，取自锁定 manifest | 全部必填 |
| `exec_status` | `PASS` / `FAIL` / `INCONCLUSIVE` | 全部必填 |
| `diff_class` | `兼容` / `不兼容` / `待验证`（固定三枚举）；**条件扩展**：仅四个豁免记录 ID（见 D4 豁免记录 ID 表）填 `不适用` | 全部必填；无 v1 对照面的纯新能力项填「待验证」并在 `reason` 说明 |
| `capability_verdict` | `可满足` / `不可满足` / `待验证` / `不适用` | live 项必填；静态项固定「不适用」（源码相同/不同不能自动推出运行时能力成立与否） |
| `evidence_validity` | `valid` / `conflict` / `harness_invalid` / `safety_invalid`（固定四枚举，默认 `valid`；冲突项由 D8 第 0 步置 `conflict`，harness 故障置 `harness_invalid`，安全越界置 `safety_invalid`） | 全部必填；D8 判定只消费本字段，不解析自由文本 |
| `failure_classes` | 字符串数组，取值限定 D5 迁移结果分类表六类，按该表优先级顺序排列 | 仅 `migration.*` 记录必填（无失败为空数组）；其余记录固定省略 |
| `equivalent` | 等价替代接口的证据路径或空串 | **仅限 live 项**：当 `diff_class=不兼容` 且等价接口经 live 实证时填证据路径（此时 `capability_verdict` 须为「可满足」）；静态项与其余情形固定空串（源码新位置不能证明运行时能力等价） |
| `reason` / `evidence_path` | 文本 / 相对 evidence 根路径 | 全部必填 |

**静态层观测状态到 schema 的映射**：`SAME` → exec_status=PASS + diff_class=兼容；`DIFF` → exec_status=PASS + diff_class=不兼容（是否影响能力由 live 层判定）；`CONFIRMED_REMOVED_OR_MOVED`（旧 ref 存在且新 ref 经仓库检索证实删除/迁移）→ exec_status=PASS + diff_class=不兼容，`equivalent` 留空，删除证据或新位置引用记入 `evidence_path`/`reason`（纯删除无新位置时只记删除证据）；`FETCH_ERROR` → exec_status=INCONCLUSIVE + diff_class=待验证——**FETCH_ERROR 不得推断源码不存在**（修正现有脚本语义，`check-opencode-contract.sh:75-91`）；`CONFIRMED_V2_ONLY`（追加的 v2 侧新锚点：旧侧经仓库检索证实不存在、新侧存在）→ exec_status=PASS + diff_class=待验证 + capability_verdict=不适用，`reason` 说明旧侧缺对照面并关联检索证据——与 FETCH_ERROR 明确区分。

**live 层观测到 schema 的唯一映射表**（D4 各 probe 行中的「断言失败」「无法观测」「无法安全触发」「记录待验证」等表述一律按下表归一，禁止实现者自行推断）：

| 观测结果 | `exec_status` | `capability_verdict` | `diff_class` |
| --- | --- | --- | --- |
| 断言满足 | `PASS` | `可满足` | 按静态/实证差异填三枚举 |
| 协议事实已获得、但能力断言实证失败 | `FAIL` | `不可满足` | 同上 |
| 无法观测 / 模型不可用 / 依赖事实缺失 / 无法安全触发 | `INCONCLUSIVE` | `待验证` | 同上（`reason` 记录具体缺失原因） |
| harness 自身故障 | `INCONCLUSIVE`（脚本退出码 4） | `待验证` | `待验证`；证据标记无效，`reason` 记故障原因 |
| D4 豁免记录 ID（纯内部语义） | `PASS` | `不适用` | `不适用`（豁免专属取值） |

**静态锚点 ID 表**（`check-opencode-contract.sh:18-42` 的 24 项按数组序分配稳定 ID；追加 v2 侧新锚点时同步扩展本表，ID 不复用不改号；静态项不属于 D8 必需能力集合，其结论只作 live 判定的输入）：

| probe_id | 锚点路径 |
| --- | --- |
| `static.01` | packages/schema/src/v1/permission.ts |
| `static.02` | packages/schema/src/v1/question.ts |
| `static.03` | packages/schema/src/v1/session.ts |
| `static.04` | packages/schema/src/v1/legacy-event.ts |
| `static.05` | packages/schema/src/session-status-event.ts |
| `static.06` | packages/schema/src/session-event.ts |
| `static.07` | packages/core/src/event.ts |
| `static.08` | packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts |
| `static.09` | packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts |
| `static.10` | packages/opencode/src/server/routes/instance/httpapi/handlers/permission.ts |
| `static.11` | packages/opencode/src/server/routes/instance/httpapi/handlers/question.ts |
| `static.12` | packages/opencode/src/server/routes/instance/httpapi/handlers/global.ts |
| `static.13` | packages/opencode/src/server/routes/instance/httpapi/groups/session.ts |
| `static.14` | packages/opencode/src/server/routes/instance/httpapi/groups/event.ts |
| `static.15` | packages/opencode/src/server/routes/instance/httpapi/groups/permission.ts |
| `static.16` | packages/opencode/src/server/routes/instance/httpapi/groups/question.ts |
| `static.17` | packages/opencode/src/server/routes/instance/httpapi/groups/global.ts |
| `static.18` | packages/opencode/src/session/status.ts |
| `static.19` | packages/opencode/src/event-v2-bridge.ts |
| `static.20` | packages/opencode/src/cli/cmd/attach.ts |
| `static.21` | packages/opencode/src/cli/tui/validate-session.ts |
| `static.22` | packages/opencode/src/cli/cmd/tui.ts |
| `static.23` | packages/opencode/src/cli/tui/worker.ts |
| `static.24` | packages/opencode/src/session/prompt.ts |

结果 ID 封闭集合 = 静态锚点 ID ∪ D4 live probe ID ∪ 豁免记录 ID，三者互不重叠。

**退出码契约**（四脚本统一）：

| 码 | 含义 |
| --- | --- |
| 0 | 全部必执行项通过 |
| 1 | 至少一项断言失败（FAIL） |
| 2 | 前置拒绝：环境断言/输入 manifest/evidence 根校验失败——**本阶段**未启动任何上游操作、未产生实验副作用（此前阶段已产出的证据不受影响） |
| 3 | 证据不足（存在 INCONCLUSIVE 且无 FAIL） |
| 4 | harness 自身故障（受影响证据无效） |
| 5 | 运行中发现安全越界（canary 后仍有允许写入集合之外的写入）：立即停止，保留现场并标记证据越界 |

混合结果优先级：前置拒绝单独返回 2；实验已开始后按 5 → 4 → 1 → 3 → 0 汇总。单项失败不中断独立后续项；依赖项按 D5 失败处置表跳过并记录原因。

- 备选：shell + curl 脚本 harness——曾被采纳，**实施后翻案被拒绝**：bash 实现经历 6 轮实现评审未收敛（子壳状态丢失、pipeline 退出码、errno 文本归因、SSE 流解析、进程树管理等 bash 固有风险反复产生阻断级缺陷），harness 承载的安全门禁 + 证据链 + 状态机 + 迁移实验复杂度超出 shell 的可靠表达能力。**改定为：python3 标准库 harness**（无第三方依赖、无构建步骤；HTTP 用 urllib、归档用 zipfile/tarfile、JSON 原生、子进程/信号/SSE 解析语义明确），四个入口脚本为 `prepare-env.py` / `check-contract-diff.py` / `live-probe.py` / `run-all.py`，共享公共库 `spikelib.py`；D1/D2/D3 的全部契约（manifest schema、退出码 0-5、结果 schema、probe_id 封闭集合、证据结构、canary 语义、阶段状态机）逐字保持不变，仅实现语言变化。
- 备选：Go 测试 harness——被拒绝：spike 产出是一次性契约事实采集，引入 Go 模块与构建步骤超出 spike 范围；python3 标准库已满足全部能力需求且可随仓库直接运行；稳定 probe ID 与结果 schema 已为后续长期治理保留最小接口。
- 备选：只跑静态 diff 不实机——被拒绝：静态 diff 覆盖不了运行时行为（认证流程、事件流时序、数据迁移），恰是本次 spike 的核心缺口。

### D4：探测覆盖矩阵——逐项实例化的 probe 契约

**判定三维**：见 D3 结果 schema（`exec_status` / `diff_class` / `capability_verdict`），映射规则与 `equivalent` 字段语义同 D3。未知 v2 端点允许在 spike 中发现，发现流程固定为：`/doc`（或等价文档端点）→ 路由清单 → 源码锚点检索；每级失败记录证据后继续下一级，三级均无结果标「待验证」并记录检索范围（产生最终结果，不再继续发现）。不可观测（如需真实模型而不可用）标 INCONCLUSIVE 并记录缺失原因。

**wire 形态发现先行规则**：凡 v2 可能改变的 wire 形态（ID 格式、枚举取值、字段名），一律先经上述三级发现确认 v2 形态，再按**发现的 v2 形态**执行能力断言；v1 字面值/取值是否被接受单独记为差异观察（进 `diff_class`/`reason`），不参与能力判定——能力判定只回答「按 v2 自身契约能否兑现 SPI 语义」。

**event.02 子项总述**（a–g 共用）：先建立事件流订阅（event.01 已验证流可建立），再触发对应行为；事件按目标 session ID 归属关联；30s 内未观测到目标事件 → 协议事实已获得但事件缺失，按 D3 live 映射表判 FAIL；订阅本身失败或行为无法安全触发 → INCONCLUSIVE 及原因。event.02c 使用**独立专用 session**（订阅建立后创建、再删除，按该 ID 关联事件），不消费共享 fixture。

**probe 契约（每项：fixture / 操作 / 断言 / v1 基准来源；超时默认 30s，证据落 `live/`）**：

| probe_id | 必需 | 契约 |
| --- | --- | --- |
| `supervisor.01` | 必需 | fixture：无。操作：以最小参数启动 v2 serve，再分别以显式 `--port/--hostname`（或 v2 等价物）启动。断言：两次启动均在沙箱端口监听；缺省参数值与文档/源码一致；非法参数启动失败且非零退出。v1 基准：`check-opencode-contract.sh:100-117`。 |
| `supervisor.02` | 必需 | fixture：设置服务器密码环境变量。操作：无凭证请求 health；带凭证请求 health。断言：无凭证 → 401；带凭证 → 200。v1 基准：`check-opencode-contract.sh:99,118-120`。 |
| `supervisor.03` | 必需 | fixture：运行中 serve。操作：轮询 health 直到就绪。断言：在期限内可识别就绪且就绪后 200、响应体形状记录；未采样到就绪前的非 200 响应不构成失败（启动过快的正常情形）。v1 基准：`check-opencode-contract.sh:109-117`。 |
| `session.01` | 必需 | fixture：运行中 serve + 测试目录。操作：POST 创建 session。断言：返回可按已发现 v2 schema 提取的**非空 session 标识**，符合 v2 ID 格式；实际字段路径与提取规则记入 `reason`/`evidence_path`；`id` 字面值是否保留仅作差异观察（不进能力判定）。下游 fixture 统一使用归一后的 session 标识。v1 基准：`check-opencode-contract.sh:121-123`。 |
| `session.02` | 必需 | fixture：session.01 产出。操作：GET 单 session。断言：200 且 ID 一致；不存在 ID（按发现的 v2 合法 ID 格式构造）→ 404 或 v2 等价明确「不存在」信号。v1 基准：`check-opencode-contract.sh:124,133`。 |
| `session.03` | 必需 | fixture：session.01 产出。操作：GET 列表（带 directory 参数）。断言：列表包含已建 session；directory 过滤语义记录。v1 基准：`check-opencode-contract.sh:125,149`。 |
| `session.04` | 必需 | fixture：**独立专用 session**（不用 session.01 产出）。操作：DELETE session。断言：删除成功；再 GET → 不存在信号。v1 基准：`check-opencode-contract.sh:150`。 |
| `session.05` | 必需 | fixture：隔离 directory 中创建固定预期集合 E（5 个 session）。操作：分别以 limit=1、limit=3、无 limit 请求列表。断言：无 limit 查询必须包含全部 E（缺一即 FAIL）；limit 生效——若上游契约承诺填满 limit 则断言 `count=min(limit,|E|)`，若上游允许不足量返回则记录该事实且适配层不得仅凭 `count<limit` 推完整性（须额外分页/对账规则，记入结论）；边界行为记录（limit=0/超限）；Overflow 判定接受**已验证的保守规则**——适配侧可按 `count==limit` 保守置 Overflow（现状实现 `spi_provider.go:314,324-330`），不要求上游提供额外元数据。v1 基准：`check-opencode-contract.sh:125`（limit=1）。 |
| `session.06` | 必需 | fixture：运行中 serve。操作：按三级发现流程查询 v2 是否提供 per-session environment 注入（配置/启动参数/API）。断言：任一形态发现则**实证**注入对 session 内 shell 生效 → 可满足；三级发现均无 → 按 D3 live 映射表标「待验证」（`diff_class=待验证`、`reason` 记录检索范围与替代形态考察结论）。v1 基准：v1 serve 每任务独立进程天然隔离（无 per-session API），记录为对比基线。 |
| `session.07` | 必需 | fixture：运行中 serve。操作：GET 存在 session（200）/ GET 合法格式不存在 ID / GET 非法格式 ID。断言：三响应对应 diffreview GetSession 的 Found / Missing / Unknown 三分流（`diffreview_capability.go:209-229`）——存在=200、明确不存在=404 或 v2 等价信号、其他无法确认情形归入 Unknown（非法 ID 只是第三类测试样本之一）。v1 基准：`check-opencode-contract.sh:133-136,171-174`。 |
| `event.01` | 必需 | fixture：运行中 serve。操作：订阅事件流端点。断言：流建立；首事件类型记录（`server.connected` 等价物存在与否）。v1 基准：`check-opencode-contract.sh:129-130`。 |
| `event.02a.session_created` | 必需 | fixture/操作同 event.02 总述（订阅流 + 触发对应行为）。断言：观测到上游来源事件；记录事件类型与 session 归属字段；按固定 domain 映射检查必需字段——`AgentSession` 负载且 `SessionKey` 非零（依据 `domain/agentprovider/agentprovider.go` `NewSessionEvent` 工厂不变量）。未观测到 → FAIL。 |
| `event.02b.session_updated` | 必需 | 同上；触发 update 行为；映射同 a。 |
| `event.02c.session_deleted` | 必需 | fixture：**独立专用 session**（不用 session.01 产出）。操作：订阅建立后创建该 session、再删除，按该 session ID 关联事件。断言：观测到 delete 类事件；映射同 a。 |
| `event.02d.status_changed` | 必需 | 同上；触发状态变更；断言 `StatusDetail` 负载字段齐全（依据 `NewStatusEvent` 不变量）。 |
| `event.02e.attention_raised` | 必需 | 同上；触发 permission/question 升起；断言 `Attention` 负载（ID + SessionKey 非零；permission/question 负载与种类一一对应，`NewAttentionEvent` 不变量）。无法安全触发 → INCONCLUSIVE 及原因。 |
| `event.02f.attention_resolved` | 必需 | 同上；触发 resolved；映射同 e。无法安全触发 → INCONCLUSIVE 及原因。 |
| `event.02g.session_error` | 必需 | 同上；触发会话错误；断言 `SessionErrorDetail`（name/message 必填，`NewSessionErrorEvent` 不变量）。无法安全触发 → INCONCLUSIVE 及原因。 |
| `attention.01` | 必需 | fixture：配置产生 pending permission 的规则（如 bash 命令审批）。操作：触发需审批操作后 GET permission 列表。断言：返回非空 pending 快照；载荷字段逐项记录（id/sessionID/命令内容等）；v1 `permission` vs v2 `permissions` 命名差异实证。 |
| `attention.02` | 必需 | fixture：once 与 reject 各用一个**独立** pending 项（避免相互污染）。操作：先按「wire 形态发现先行规则」发现 v2 reply 取值集合、验证与 once/reject 的语义对应项，以**v2 wire 值**回复（v1 字面值是否被接受记为差异观察，不影响能力判定；v1 禁 always 的约束依据 `permission_reply.go:40-41`）；再对同目标重复回复；对不存在目标回复。断言：成功回复后该 pending 项从列表消失（REST 对账）；SSE 出现对应 resolved 类事件且目标 ID 一致；重复回复/不存在目标均返回可识别为「目标已不可回复」的信号（须与「能力端点缺失/401」区分；两种情形之间是否可区分作为观察记录，不作必需断言——SPI 统一映射 NotFound）；取值集合实证记录。 |
| `attention.03` | 必需 | fixture：触发 question 的场景（无法安全触发时 INCONCLUSIVE）。操作：GET question 列表 + 以答案文本数组回复。断言：非空 question 载荷含 ID、题目文本、session 归属字段；回复被接受；回复后该项从列表消失。 |
| `prompt.01` | 必需 | fixture：session.01 产出。操作：GET `/doc`（或等价）定位 prompt 提交端点；POST 提交（含 messageID、text part）。断言：端点存在性；sessionID/messageID pattern 约束；parts 结构；成功响应状态码。v1 基准：`check-opencode-contract.sh:151-176` 的 /doc 断言。 |
| `prompt.02` | 必需 | fixture：prompt.01。操作：提交后观测事件流与 session 状态；记录 messageID 是否在结果/事件中回显。断言：messageID 透传不重写；accepted 的确认条件（204 等价物）；四分类中 `accepted`/`http_response` 由上游实证，`transport_unknown`/`pre_send_failure` 为本地传输语义（标注「无需外部 probe」，理由：由适配层 HTTP 行为决定）；未知交付禁止重投的判定依据记录。v1 基准：`agentprovider.go:120-125`。 |
| `prompt.03` | 必需 | fixture：session + 沙箱内文本文件。操作：以 `file://` 绝对路径 URI + mime + basename 构造 file part 提交。断言：v2 接受该 part 形态且 agent 可读文件；拒绝形态记录。v1 基准：`diffreview_adapters.go:49-51,67-71`。 |
| `status.01` | 必需 | fixture：运行中 session。操作：GET session/status（或等价）。断言：响应形状与已知状态枚举清单记录；未知枚举归一规则属 ocdeck 内部适配语义（`StatusReader` 未知枚举 → StatusUnknown），**无需向真实上游注入不存在的枚举**，标「无需外部 probe」子项并给出本理由。v1 基准：`check-opencode-contract.sh:126`。 |
| `capability.01` | 必需 | fixture：运行中 serve。操作：GET `/doc` 无凭证/带凭证；检索文档端点等价物。断言：无凭证 401；带凭证返回可解析 API 文档；prompt 端点在文档中的形状可机器读取（diffreview probePromptAsync 依赖，`diffreview_capability.go:231-239`）。v1 基准：`check-opencode-contract.sh:153-155`。 |
| `terminal.01` | 必需 | fixture：session.01 产出 + 第二端口。操作：以 `--session <SID>` 启动第二实例（共享）；以不存在 ID（按发现的 v2 合法 ID 格式构造）启动。断言：共享实例可列出同 session；不存在 ID 拒绝并退出非零（错误文本记录）。v1 基准：`check-opencode-contract.sh:133-149`。 |
| `title.01` | 必需 | fixture：session.01 产出。操作：经 API 或事件观测 title 写入路径。断言：title 可写入且经 GET 可读回；字段名记录。 |
| `conn.01` | 必需 | fixture：同 directory 两个 serve 实例。操作：实例 A 建 session，实例 B 列表查询；中断 A 的事件流后重连。断言：B 可见 A 的 session（共享存储语义）；重连后状态对账口径记录（全量快照 vs 增量）。 |
| `recovery.01` | 必需 | fixture：session.01 产出。操作：重启 serve 进程后以相同 directory 重连。断言：既有 session 可列出/读取；endpoint/凭据/目录解析所需上游事实（`BindingRuntimeEndpoint` 对应面，`agentprovider.go:186-189`）在重启后的可获得性。 |
| `migration.forward` | 必需 | 见 D5 步骤 2（正向迁移逐项断言）。 |
| `migration.reverse` | 否（仅记录） | 见 D5 步骤 3；反向失败不阻断 GO。 |

**fixture 执行顺序（DAG）**：session.01 产出的共享 fixture 供 session.02/03/05/07、event.02a/b/d/e/f/g、attention、prompt、status、title、recovery 消费；**一切破坏性操作使用独立专用 fixture**（session.04 删除、event.02c 删除各自独立的 session），且必须排在共享 fixture 的全部消费者之后执行；共享 session.01 在全部消费者完成前不可删除；attention.02 的 once/reject 各用独立 pending fixture；无法确定依赖时默认串行执行并在 results 中记录顺序。

`probe_id` 取值规则：必须为封闭集合（D4 矩阵第一列 + 豁免记录 ID 表 + 静态锚点 ID 表）的字面值之一，全局唯一、跨 run 稳定；D8 必需项集合将七个 event.02 子项逐项展开计入。

**SPI 方法覆盖闭环表**（8 窄接口 + Provider 聚合逐方法 → probe ID 或豁免理由；豁免项在 results.jsonl 中生成 `capability_verdict=不适用`、`exec_status=PASS` 的记录并注明理由，不计入 D8 必需项）：

| SPI 方法 | 覆盖 |
| --- | --- |
| RuntimeSupervisor.Acquire | supervisor.01-03 + session.01（启动/认证/健康/建 session 的外部事实） |
| RuntimeSupervisor.Resume | recovery.01 |
| RuntimeSupervisor.AcquireEphemeral | 无需外部 probe：ocdeck 删除清理专用的内部生命周期语义，上游无对应契约面 |
| RuntimeSupervisor.Release | 无需外部 probe：进程终止/清理为 ocdeck 托管语义；上游侧行为由 terminal.01 间接覆盖 |
| SessionControl.List / Get / Create / Delete | session.03+session.05 / session.02+session.07 / session.01 / session.04 |
| EventSource.Subscribe | event.01 + event.02a.session_created + event.02b.session_updated + event.02c.session_deleted + event.02d.status_changed + event.02e.attention_raised + event.02f.attention_resolved + event.02g.session_error |
| AttentionIO.ListPermissions / ListQuestions / Reply | attention.01 / attention.03 / attention.02+attention.03 |
| AsyncPrompt.Send | prompt.01 + prompt.02 + prompt.03 |
| StatusReader.Current | status.01 |
| TerminalIO.EnsureTerminal / AttachCommand | terminal.01（**覆盖边界**：两方法的终端存活与 attach argv 属 ocdeck 内部进程/tmux 管理义务，无上游契约面；terminal.01 实证的是外部部分——第二实例的 session 选择、共享存储可见性、不存在 ID 拒绝。若后续 v2 设计需要上游提供的 attach 命令，须增加该命令连接目标 session 的最小实证） |
| TitleUpdater.Sync | title.01 |
| Provider.Meta / BindingRuntimeEndpoint | 无需外部 probe：registry 装配与 provider 私有 data 解码为 ocdeck 内部语义；endpoint/凭据/目录的上游事实由 recovery.01 覆盖 |

**豁免记录 ID（D4 封闭集合的一部分，非 D8 必需项）**：豁免项以固定 ID 写入 `results.jsonl`，结果值固定为 `exec_status=PASS` / `diff_class=不适用`（该取值仅限豁免记录）/ `capability_verdict=不适用` / `equivalent=""` / `reason`=豁免理由：

| 豁免记录 ID | 对应 SPI 方法 |
| --- | --- |
| `spi.exempt.runtime_supervisor.acquire_ephemeral` | RuntimeSupervisor.AcquireEphemeral |
| `spi.exempt.runtime_supervisor.release` | RuntimeSupervisor.Release |
| `spi.exempt.provider.meta` | Provider.Meta |
| `spi.exempt.provider.binding_runtime_endpoint` | Provider.BindingRuntimeEndpoint |

纯 ocdeck 内部行为（预算所有权、cleanup notice 分派、prompt 四分类中的本地传输两态）标「无需外部 probe」并给出理由；不得以 provider 实现测试替代本 spike。D8 决策表的「必需项」= 上表标记必需的 probe + D6 必答模板。

### D5：单向迁移契约 = 基于快照的独立实验，fixture 冻结 + 逐项成功断言

**fixture manifest（造数阶段逐条记录，落 `migration/fixture-manifest.json`）**，固定 JSON schema：

```json
{
  "sessions": [{ "id": "<session-id>", "messages": [{ "id": "<message-id>", "content_path": "<相对 fixture-content/ 的唯一路径>", "content_sha256": "<64-hex>" }] }],
  "cwd": "<造数工作目录>",
  "config": [{ "key": "<配置项>", "v1_source": "<v1 侧定位：相对文件路径#JSONPath 或 API 端点>", "v2_source": "<v2 侧定位，形态可不同>", "expected": "<字符串化预期值>" }],
  "permissions": [{ "id": "<权限记录标识>", "v1_source": "<v1 侧定位>", "v2_source": "<v2 侧定位>", "expected": "<字符串化预期状态>" }],
  "snapshot_checksums": { "<相对路径>": "<64-hex>" }
}
```

必填性按**两态生命周期**（非全字段恒定必填）：**造数态**（初始写入 `fixture-manifest.json`）——`sessions[].id`、`messages[].id`/`content_path`/`content_sha256`、`cwd`、`config/permissions` 的 `key`/`id`+`v1_source`+`expected`、`snapshot_checksums` 必填；`v2_source` **允许省略**（含义=待发现）。（无内容的数组允许为空数组并注明理由，但受下方最低完整性谓词约束。）**执行态**（正向实验断言前）——必须先完成 `v2_source` 发现并以**原子更新**（写临时文件+rename）写回 manifest，更新动作与发现证据记入 evidence；发现失败 → 对应断言 INCONCLUSIVE（D8 第 4 条），发现成功 → 填值后按比较谓词断言。哈希规则唯一：对 `migration/fixture-content/` 中保存的**消息原文文件逐字节**计算 SHA-256、小写 hex 编码——两侧比对均用同一规则，禁止一侧哈希文本另一侧哈希结构化 JSON。**关联与比较规则**：`content_path` 唯一确定该消息的原文文件（造数侧写入、验证侧读取，同一相对路径）；集合比对一律集合语义（顺序无关）；v2 侧**额外**项记为观察不判失败，缺失或内容/值不一致 → 对应失败分类；消息 ID 重复 → 造数不完整（INCONCLUSIVE）；`config`/`permissions` 按**逻辑样本**（`key`/`id`）对应两侧：两态生命周期见上方（造数态 `v2_source` 省略待发现，执行态原子写回）；`v2_source` 允许与 v1 侧路径/字段/端点形态不同。比较谓词固定为**两步且顺序固定**：①造数态校验——v1 侧按 `v1_source` 定位提取实际值，字符串化后必须等于 `expected`（规范化仅限 UTF-8 + 去首尾空白，其余规范化须逐条记入 `reason`）；不一致、`v1_source` 不可读取、实际值无法提取或 `expected` 格式非法 → 均判 fixture 不完整（INCONCLUSIVE），`migration.forward` 不得 PASS；②执行态断言——v2 侧建立对应后，按 `v2_source` 提取的实际值必须等于**同一 `expected`**；不一致或丢失 → `PRESERVATION_FAILURE`。v2 侧定位无法建立（端点不存在/字段重命名且无对应）→ 该断言 INCONCLUSIVE（进 D8 第 4 条，不得判保留失败）；缺 `v1_source`/`expected` 字段 → 造数不完整（INCONCLUSIVE）。

**fixture 最低完整性谓词（机械可检查）**：`sessions` 至少 1 个；迁移前消息至少 1 条（含 ID 与内容哈希）；`config` 与 `permissions` 各至少 1 个非空样本。任一样本无法构造 → 对应断言标 INCONCLUSIVE 且 `migration.forward` 整体**不得判 PASS**（空数组+说明不满足验收）。**连续性覆盖口径**：固定选择 fixture 中第一个 session 作为连续性测试对象（选择规则记入 `reason`），结论适用范围限于该 session 类形态。

**实验结构（每个方向独立目录、独立证据）**：

1. **造数与冻结**：用锁定的 v1 基线二进制（D1）在沙箱内制造 fixture 数据；固化 canonical v1 快照前**停止全部写入进程并等待退出**（或采用已证明一致的备份方式），快照记录文件级校验值；canonical 快照固化后不再作为运行目录。
2. **v1→v2（正向，`migration.forward`）**：从 canonical 快照复制出独立实验目录，以 v2 目标版本二进制打开。**复制重定位规则**：只允许调整环境变量与 cwd 指向新实例路径；对快照内数据文件中的绝对路径引用——先用 grep 类检索发现并记录清单；发现的引用若必须改写，改写操作逐条记录到 `migration/relocation.log`，且**经改写的实验结果单独标注，不得代证未经改写的自动迁移**；未经记录的任何手工改写上游数据一律禁止（违规则该实验证据无效）。**主结果规则**：`migration.forward` 的主结果只代表**未经上游数据改写**的 canonical 快照实验；改写实验仅作为辅助证据记入同一记录的 `evidence_path`/`reason`（不产生封闭集合之外的结果 ID，不替换主结果）。主实验未完成有效验证 → 主结果保持 INCONCLUSIVE；已取得有效负面事实 → 保留对应 FAIL——两者均不得被改写实验的成功覆盖。**成功断言逐项核对 fixture manifest**：session 可列出且集合谓词为 **F ⊆ V**（F=fixture 声明的 session 集合，V=v2 实际集合；`V\F` 额外项记为观察不判失败；fixture session 缺失或 ID 改变 → FAIL 并触发 D6 用户决策，不得靠映射说明直接恢复 GO）；消息历史完整（逐条比对 ID 与内容 SHA-256，同属 F ⊆ V 集合语义）；配置与权限数据保留；记录 v2 首次打开时的显式迁移动作（日志/目录结构变化）；**session 可继续使用（最小连续性断言）**：对该 session 提交带唯一 messageID 的 prompt，观测新消息实际进入该 session 的消息历史且发生有效后续处理（状态/事件变化），并确认迁移前历史仍完整保留——prompt 被 accepted/形状正确只是提交前提，不构成连续性证据；缺模型或处理过程不可观测 → 本断言 INCONCLUSIVE；有效实证处理失败 → `CONTINUITY_FAILURE` → D8 第 2 条。目录不变不能单独证明迁移成功，也不能单独证明失败。
3. **v2→v1（反向，`migration.reverse`，仅记录）**：以 v2 二进制生成 v2 原生数据快照（同样冻结），复制到第三个独立目录用 v1 基线打开，仅记录实际行为（可读/报错/静默丢弃），结论标注「不作承诺」；反向失败不影响 GO。
4. 每个实验实例在 D2 边界内独立隔离全部已查明写入路径，避免实验间相互污染。
5. 每个方向保留：迁移前后目录树快照、关键文件 diff、双侧版本信息、进程日志。

**失败处置表（D3 live 层同样适用）**：

| 失败类别 | 处置 |
| --- | --- |
| 安全门禁失败 | 按 D2 canary 阶段判定表：输入拒绝/首次验证失败/复验无法执行 → 退出码 2；门禁通过后的运行中安全越界 → 退出码 5，立即停止全部实验，受影响证据无效 |
| fixture 造数不完整 | 相关实验标 INCONCLUSIVE；**禁止**把后续「查不到数据」判成迁移不兼容 |
| 上游行为违约（断言失败） | 记录 FAIL 并继续独立后续项；依赖项跳过并记录原因 |
| 网络/模型/工具缺失 | 记录 INCONCLUSIVE 及缺失原因 |
| harness 自身故障 | 受影响证据无效（退出码 4），修复后以新 run-id 重跑 |
| 已提交 prompt 的未知交付 | 禁止自动重投；重试必须使用新实验实例并关联原 run-id |

任何失败均保留现场、记录跳过项及依赖原因。**子进程清理三层生命周期**：①单项结束只清理该项专属子进程；②共享 serve/订阅由 fixture 所有者（harness）在最后一个消费者完成后清理——单项失败不得终止共享 serve，依赖项跳过只能由明确的 fixture/事实缺失触发；③安全越界、阶段退出、信号及父进程退出时才清理整个阶段的进程树。

**统一重试契约**（封闭规则，禁止实现者自行扩大）：

1. probe 断言类结果（PASS/FAIL/INCONCLUSIVE）**一律禁止自动重试**——一次执行一条最终结果；普通超时、HTTP 5xx、启动失败、模型调用失败均不例外（按其观测类别入结果）。
2. 仅 harness 级瞬态错误允许自动重试：下载校验失败、release 元数据/源码拉取的网络错误。至多 2 次重试、间隔 ≥5s，每次尝试单独落证据文件；最终只保留一条该项结果，重试历史在 `reason` 中关联。
3. 已提交 prompt 的未知交付：禁止自动重投；确需重试必须使用新实验实例并在证据中关联原 run-id（与失败处置表一致）。
4. harness 故障修复后或人工重跑：必须使用新 run-id，旧 run 证据只读保留、不得覆盖。

**迁移结果分类表（`migration.forward` 的失败分类与 D8 唯一映射）**：

| 类别 | 判定条件 | D8 映射 |
| --- | --- | --- |
| `READABILITY_FAILURE` | 在 fixture 完整、环境可用前提下取得**有效负面读取实证**（列表/读取操作明确报错或返回明确的数据缺失信号）——按观测阶段先判此项；纯超时/网络/环境故障**不**归此类，归 INCONCLUSIVE（不得据此认定迁移违约） | 承接前置条件失败 → 第 2 条，暂停 go/no-go 提交用户决策 |
| `DATA_IDENTITY_FAILURE` | **成功取得可比较数据后**，fixture 声明的 session/消息在 v2 侧缺失或 ID 改变，或消息内容 SHA-256 不一致（F ⊆ V 不满足；v2 侧额外项不算失败） | 同上 → 第 2 条用户决策 |
| `PRESERVATION_FAILURE` | 配置/权限数据丢失或被改写 | 同上 → 第 2 条用户决策 |
| `CONTINUITY_FAILURE` | 数据可读，但有效实证证明步骤 2 的最小连续性断言不可满足——包括提交协议失败或后续处理失败；缺模型/不可观测按 INCONCLUSIVE | 同上 → 第 2 条用户决策 |
| `INCONCLUSIVE` | fixture 不完整 / 工具 / 环境不足 / 前置事实缺失 | 第 4 条「待补证据」；禁止误判为迁移不兼容 |
| `HARNESS_FAILURE` | 脚本自身故障 | 第 0 步证据无效，修复后新 run-id 重跑 |

每类失败的判定条件以 fixture manifest 逐项断言为准。**互斥判定顺序**：先按观测阶段判 READABILITY（有效负面读取实证）再判 DATA_IDENTITY（读取成功但内容不一致）——同一断言不得双归类；仅当不同断言分别失败时才记录多个类别，多类并存按上表顺序汇总优先级。

**migration 结局状态机与统一结果 schema 映射**（六状态与字段映射对 `migration.forward`/`migration.reverse` 通用；D8 只消费本表映射后的字段，不解析自由文本）。**reverse 专属规则**：reverse 记录同样按本表落 schema。**排除范围收窄**：reverse 的反向可读性、协议能力结果与仅影响 reverse 的证据不足/harness 故障**不进入 D8 第 2–5 条**；**D8 第 1 条全局环境与安全门禁对 reverse 同样适用**（reverse 阶段安全越界照常全局 NO-GO 并终止全部流程）；reverse 普通负面结果可计入 run-all 退出码汇总但不影响 GO；`summary.md` 中 reverse 结论固定标注「仅记录，不作承诺」。**reverse 方向专属断言集合**（正向的最低完整性谓词与连续性断言**不适用**于 reverse）：前置=v2 原生快照有效冻结（冻结失败 → `NOT_STARTED_FIXTURE_INVALID`）且**反向最低样本**——至少 1 个 v2 原生 session、至少 1 条可识别消息（无法构造 → `STARTED_INCONCLUSIVE` 并记录原因）；v1 成功读取预定 session 与消息历史 → `VALID_PASS`；明确报错或实证数据被丢弃 → `VALID_NEGATIVE`（`failure_classes=[]`——分类表是承接前置专用，reverse 观测记入 `reason`）；无法观测 → `STARTED_INCONCLUSIVE`；harness 故障 → `HARNESS_INVALID`；退出码贡献同表。

| 结局状态 | 触发条件 | `exec_status` | `capability_verdict` | `diff_class` | `evidence_validity` | `failure_classes` | 退出码贡献 | D8 分支（forward） |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `NOT_STARTED_DEPENDENCY_MISSING` | 前置依赖事实缺失（D3 依赖谓词不满足） | `INCONCLUSIVE` | `待验证` | `待验证` | `valid` | `[]`（`reason` 记缺失项） | 3 | 第 4 条 |
| `NOT_STARTED_FIXTURE_INVALID` | 造数态校验失败（v1_source 不可读/v1 实际值≠`expected`/`expected` 非法/最低完整性不满足/消息 ID 重复）——发生在实验启动前，**不得进入 v2 打开实验** | `INCONCLUSIVE` | `待验证` | `待验证` | `valid` | `["INCONCLUSIVE"]`（`reason` 记失败项） | 3 | 第 4 条 |
| `STARTED_INCONCLUSIVE` | 实验已启动但 fixture 不完整/模型缺失/纯超时等 | `INCONCLUSIVE` | `待验证` | `待验证` | `valid` | `["INCONCLUSIVE"]` | 3 | 第 4 条 |
| `VALID_NEGATIVE` | 有效实证命中四类承接前置失败（可多个） | `FAIL` | `不可满足` | 按实证三枚举 | `valid` | 命中类按 D5 分类表顺序 | 1 | 第 2 条 |
| `VALID_PASS` | 全部断言满足（含最低完整性与连续性断言） | `PASS` | `可满足` | 按实证三枚举 | `valid` | `[]` | 0 | 第 5 条输入 |
| `HARNESS_INVALID` | harness 自身故障且影响主实验可信度 | `INCONCLUSIVE` | `待验证` | `待验证` | `harness_invalid` | `["HARNESS_FAILURE"]` | 4 | 第 0 步无效，修复后新 run-id 重跑 |

单项断言唯一分类；多类并存时 `failure_classes` 记录全部命中类（按 D5 分类表顺序），三维主字段按最高优先级类对应的行映射。**证据效力优先于失败分类**：任何影响主实验可信度的 harness 故障使整条迁移记录进入 `HARNESS_INVALID`（退出码 4），已观测负面事实仅保留在原始证据中、不参与分类聚合；仅当记录整体有效（`evidence_validity=valid`）时才按四类承接失败与 `INCONCLUSIVE` 聚合。状态迁移唯一：`NOT_STARTED_DEPENDENCY_MISSING` 不得在同 run 补齐依赖后补跑；任何状态的人工/修复后重跑一律新 run-id。

- 备选：同目录顺序实验——被拒绝：迁移可能改写目录结构，同目录反向实验无法区分「v1 数据被 v2 改写」与「v2 原生数据」，证据不可靠。
- 备选：纯文档调研——被拒绝：单向迁移是基础假设，必须实机验证（proposal Why）。

### D6：承接契约与兼容边界——必答模板与记录规则

基于已确认的架构决策（任务创建时钉死 providerID；分派按机器实装版本；v2 机器上 v1 任务由 v2 provider 承接；反向 fail-closed），本 spike 在差异清单（D4）与迁移实测（D5）之上产出承接设计结论。**以下必答模板现在固定记录义务，具体方案允许 spike 执行后填写，但任何方案不得改写用户已确认的「任务创建时钉死版本」语义；未解决的必答项不得支持 GO（D8）。**

**承接语义必答模板**（对应持久化三面：任务钉扎列 / anchor 命名空间 / session 行，`internal/infrastructure/store/provider_namespace.go:28-55`）：

1. `tasks.provider_id` 是否保持创建时钉扎值；实际执行 provider 如何记录（新字段 / envelope Data / 运行态派生）。
2. binding envelope 全字段（`domain/agentprovider/agentprovider.go:39-46`：Version / ProviderID / ProviderInstanceID / UpstreamSessionID / TerminalRef / Data）在承接前后的逐项映射规则。
3. v1 私有 Data 由谁解析（v2 provider 承接时）；terminal / endpoint / 凭据如何失效或重建。
4. 上游迁移是否保留 session ID（由 `migration.forward` 实证）；按 D5 规则，ID 改变即 FAIL 并触发用户决策——本项记录的是该情形的归属冲突备选规则，供用户决策时选择，spike 不预选。
5. 首次承接、重复承接、进程重启、承接中途失败四种路径的连续性语义。
6. 区分「仅代码回滚且数据未被 v2 改写」与「数据已被 v2 改写后的 v1 打开」两种回滚形态——不得由前者推出后者可恢复。
7. 每个承接前置条件关联 D4 probe ID 与证据路径（承接前置条件的封闭集合见下方定义）。
8. 跨版本兼容边界：v2 钉扎任务在 v1 机器上激活的 fail-closed 判定依据（钉扎 providerID 与实装主版本的比较口径）、错误语义（映射到既有 ErrorKind 或新增）、用户可感知提示口径。

**必答项记录格式（机械判定「已解决」）**：八个必答项逐项以固定字段记录于 spike 结论节——`status`（枚举 `resolved` / `pending` / `user_decision_required`）、`decision`（结论文本，`resolved` 时必填非空）、`evidence`（证据相对路径，`resolved` 时必填且指向存在的证据文件）、`notes`（可选）。D8 的「D6 必答项全部解决」谓词 = 八项全部 `status=resolved`；`user_decision_required` 须经用户决策后方可改为 `resolved`（`notes` 引用用户决策原话）。

**跨版本兼容边界**：见必答模板第 8 项（内容即上述 fail-closed 判定依据、错误语义、用户提示口径），其解决状态计入 D8 必答项谓词。

**承接前置条件封闭集合（`handoff_prerequisite`）**：承接前置面唯一等于 **`migration.forward` 的四个失败分类**——`DATA_IDENTITY_FAILURE` / `READABILITY_FAILURE` / `PRESERVATION_FAILURE` / `CONTINUITY_FAILURE`（D5 迁移结果分类表）。以下均**不属于**承接前置面：静态层差异（diff_class 任何取值）、缺 v1 对照基线的纯新能力项、D4 其他必需 live probe 的失败（它们走 D8 第 3 条 NO-GO）。该集合现在冻结，禁止 spike 执行后临时扩展；D6 必答模板第 7 项的「承接前置条件」只能从本集合取值。

**前置条件未满足的处置（用户决策，spike 不预选）**：当**有效实证证明承接前置条件不可满足**（`migration.forward` 命中上述四个失败分类之一，而非仅协议存在差异）时，probe 仍完整记录观测事实（哪些能力失败、任务数据是否可读、是否可恢复），但**不自动转化为产品语义**——暂停 go/no-go，将观测事实提交用户决策；完整承接 / 只读降级 / 整体禁止承接均为后续方案选项，由用户在后续实现 change 中选择。（用户已明确：该情况原则上不应发生；若发生，先咨询用户再定方案。）

### D7：产出物落点

- 探测脚本：`scripts/spike-v2/prepare-env.py`、`check-contract-diff.py`、`live-probe.py`、`run-all.py`（行为契约见 D3）。
- 证据归档：固定根 `openspec/changes/opencode-v2-contract-spike/evidence/<run-id>/`（结构见 D3，重跑不覆盖旧 run）。
- spike 结论：差异清单、迁移契约验证结论、承接契约与兼容边界设计、go/no-go 建议，沉淀为本 design.md 的「Spike 结论」节（spike 执行后填写，实现评审时核对）。
- 文档回写：`docs/design/opencode-v2-a-plus-migration.md` 回写版本锁定记录与结论引用；其中与「v1/v2 双 provider 长期并存」方向冲突的旧建议（如整体切换、单 provider 过渡）须标注为历史草案，避免仅更新版本号后仍被当作当前实施方案。

### D8：go/no-go 结论汇总规则（证据效力预处理 + 按序决策表）

**第 0 步（先于决策表）**：证据效力预处理——逐项检查实机观察与源码/文档依据是否冲突；冲突项不得挑选有利证据：保留原始观察记录，该项 `evidence_validity` 置 `conflict`、用于决策的 `capability_verdict` 置「待验证」并在 `reason` 记录冲突双方。**冲突项不作为第 2/3 条的有效负面实证**，由第 4 条接住。harness 故障（退出码 4）影响的证据置 `harness_invalid`，安全越界（退出码 5）影响的证据置 `safety_invalid`。无效化记录保留原 `exec_status` 字段但**不再被任何准入/否定判定消费**，仅按无效类别分流。

必需项集合 = D4 表标记必需的 live probe（不含静态项与豁免项）+ D6 必答模板。**字段级谓词**：一切准入/否定判定的前提是 `evidence_validity=valid`（非 valid 记录按第 0 步分流：`safety_invalid` → 第 1 条，`conflict`/`harness_invalid` → 第 4 条）；在此之上能力准入只看 `capability_verdict` 与 `exec_status`。`diff_class=待验证` 本身不阻断——它只表示缺 v1 对照基线，若该能力已实证可满足则不进入第 4 条。按序判定，先命中先出：

1. 隔离边界/来源链校验失败（D1/D2），按两个阶段分支：**前置拒绝（退出码 2）**——实验未启动，该阶段 `results.jsonl` 无实验记录，`summary.md` 标 `NO-GO(前置拒绝)`；**运行中安全越界（退出码 5）**——实验已启动，立即终止，受影响证据 `evidence_validity=safety_invalid`，`summary.md` 标 `NO-GO(安全越界)`。两分支结论均为 `NO-GO`，run-all 返回码与 summary 状态一致。
2. 有效实证（`evidence_validity=valid`）证明承接前置条件不可满足（`migration.forward` 命中 D6 `handoff_prerequisite` 封闭集合的四个失败分类之一）→ 暂停 go/no-go，提交用户决策（D6 处置）；同时列出其他全部失败项，不自动选择降级。
3. 其他必需能力经有效实证（`evidence_validity=valid`）不可满足（`capability_verdict=不可满足`）→ `NO-GO`。
4. 任一必需项 `capability_verdict=待验证`、`exec_status=INCONCLUSIVE`、`evidence_validity ∈ {conflict, harness_invalid}`、`migration.forward` 未完成、或 D6 必答项未解决 → 「待补证据」，不得输出无条件 `GO`。
5. 全部必需 live 项 `evidence_validity=valid` 且 `exec_status=PASS` 且 `capability_verdict=可满足` + `migration.forward` **未经上游数据改写的正向实验有效通过**（D5 主结果规则）+ 承接必答项全部解决 → `GO`。

补充规则：协议差异存在经验证的等价映射（`diff_class=不兼容` 且 `equivalent` 有实证、`capability_verdict=可满足`）本身不导致 `NO-GO` 或用户决策暂停；所有结论列出证据状态（实机探测 / 源码锚点 / 文档依据）与适用的双侧版本基线（D1）。

## Spike 结论（2026-10-14 实机执行后填写）

### 版本基线与执行环境（D1/D2）

- v1 基线 = **1.18.26**（GitHub release，commit `774cc7c1`，asset digest sha256:90b99cc2…689c3e）。
- v2 目标 = **2.0.28**（无 GitHub release；经 opencode.ai update API 渠道核验：versioned URL + files[asset].sha256=58fd4154…9670）。两个 commit 分别记录、不视为同一基线：**源码锚点 commit** = tag v2.0.28 指向的 `026b106b`；**二进制构建 commit** = update API 元数据 github sha `044d8b7d`（比 tag 领先 1 commit——用户已批准不强制 tag 一致，「v2 协议已基本稳定」）。
- 执行环境：Docker 容器（linux/arm64，python:3.12），`--read-only` rootfs + `--tmpfs /tmp` + 三/四个 bind mount，canary 目标 `/root/spike-canary-<rid>`（mkdir→EROFS 拒绝，每轮通过）。
- 最终完整 run：**`20261014T045810Z-f125cafd`**（31 PASS / 27 INCONCLUSIVE / 2 FAIL；terminal.01 修正后以有效 v2 操作实证 PASS）。此前 `20261014T042602Z-90e9d6ed`（42 PASS）为 post-gate 复审前归档，保留不覆盖。两 run 的 static.14–24 差异：旧 run 经 contents API 证实 CONFIRMED_REMOVED_OR_MOVED，新 run 因 GitHub 匿名 API 限流落 FETCH_ERROR（环境性，非回归）——静态结论以旧 run 证据为准，新 run 该 11 项不覆盖旧结论。证据归档 `evidence/<run-id>/`（含全部历史 run）。

### 协议差异清单（live 实证，v1.18.26 ↔ v2.0.28）

| # | 差异项 | v1 | v2 | 判定 |
|---|---|---|---|---|
| 1 | API 前缀 | 根路径 | 全部 API 在 `/api` 前缀下 | 不兼容（实证），适配层统一加前缀即可满足 |
| 2 | 响应 envelope | 直接对象/数组 | 统一 `{data: ...}` 包裹 | 不兼容（实证），适配层解包即可满足 |
| 3 | prompt 提交 | `POST /session/{id}/prompt_async` body `{messageID, parts}` | `POST /api/session/{sessionID}/prompt` body `{text, id}`（file part 用 `{uri, name}`） | 不兼容（实证 prompt.01 PASS），适配层映射即可满足 |
| 4 | 事件名 | `session.updated` | `session.renamed` | 不兼容（实证 event.02b PASS），改名映射即可满足 |
| 5 | 事件载荷归属键 | `properties.*` | `data.*`；sessionID 在 `data.sessionID` | 不兼容（实证），取值路径映射即可满足 |
| 6 | permission 端点 | `/permission` | `/api/permission/request` + `/api/permission/saved` | 不兼容（实证），双端点映射；pending 触发需真实模型（attention.01/02 INCONCLUSIVE，D4 允许） |
| 7 | 文档端点 | `/doc` 返回真实 OpenAPI 且匿名 401 | `/doc` 返回公开 SPA HTML（匿名 200）；真实 OpenAPI 在 `/openapi.json` | **FAIL（capability.01）**：v1 认证门禁语义不成立；等价能力核验：`/openapi.json` 可匿名取回真实 OpenAPI（本结论的 v2 OpenAPI 快照即如此获得）——契约发现能力**存在等价形态**，仅端点与认证语义变化 |
| 8 | 全局 session status | `GET /session/status` 返回各 session 状态 | 端点移除（请求被 `/api/session/{sessionID}` 路由把 "status" 当 ID 解析，400） | **FAIL（status.01）**：旧 wire 端点不兼容已实证。等价能力为**候选映射（schema/源码依据，运行时等价性待证）**：`Session.Info.outcome` 枚举 + SSE 状态事件在 OpenAPI 中存在，但 event.02d 运行时观测仍 INCONCLUSIVE，未实证其承载等价状态语义 |
| 9 | serve 启动 | `serve` 子命令 | 必须 `serve` 子命令（裸 `opencode` 进 TUI）；serve 不接受 `--session` flag | 不兼容（实证 supervisor.01 PASS），启动参数改造即可满足 |
| 10 | 就绪判定 | `/global/health` 200 JSON | `/global/health` 返回 SPA HTML（不可作就绪信号）；用 `/api/session` 401/200 判定 | 不兼容（实证），就绪探针换端点即可满足 |
| 11 | 存储布局 | 文件存储（project/session/message 目录树） | SQLite `xdg/data/opencode/opencode.db` + snapshot git 树 | 不兼容（实证），不经文件布局直读，一律走 API |
| 12 | 认证 | Basic `opencode:<password>`，`OPENCODE_SERVER_PASSWORD` env | 同语义（实证 supervisor.02 PASS 兼容）；未设置密码时 serve 日志打印随机密码 | 兼容 |
| 13 | 静态锚点 | 24 项锚点 | 19 项 CONFIRMED_REMOVED_OR_MOVED + 5 项 DIFF | 不兼容（源码锚点实证），仅作 live 判定输入 |

待验证项按归因分三类（不归并、不统一归因缺模型）：

- **模型/fixture 依赖**（需带模型环境补证）：event.02d（状态变更事件）、event.02e/f（permission/question 升起与解决）、attention.02（回复实证需独立 pending fixture，无模型环境无法安全触发）、prompt.03（agent 可读文件实证）、migration.forward（pending permission 样本）。
- **请求/解析适配缺口**（harness 侧可修，非能力缺失）：session.05（分页响应解析缺口，all/limit1 均 200）、attention.01（permission 端点返回 location 查询格式错误）、attention.03（question 端点 404，端点形态待发现）、event.02c（删除触发 HTTP 405，方法/路径待对齐）、title.01（PATCH=204 写入路径未确认）、migration.reverse（落盘检测路径过滤未覆盖 v2 SQLite 布局，marker 实际已写入 opencode.db）。
- **未执行语义**（需补实验设计）：event.02g（会话错误注入无公开通道）、session.06（发现疑似 environment 形态，注入实证未做）、conn.01（重连对账口径未实证）、prompt.02（messageID 回显未在响应体观测——透传不重写断言证据不足）。

**补证回填（2026-10-16，opencode-v2-provider change 阶段 1；证据 run 20261015T165420Z-4013391d / 20261016T090000Z-0bb6ac6c）**：

- 模型/fixture 依赖项：event.02d/e/f、attention.02、prompt.03 全部 PASS（实际事件族 `permission.asked`、`session.execution.started/succeeded`）；migration.forward 转出有效负面 FAIL——v1 pending permission 为进程内存态零持久化，官方升级后丢失（承接已知限制）。
- 适配缺口：session.05/title.01/event.02c/session.04 修正后全部 PASS（envelope 解包、`/api` 前缀、DELETE 204 级联、PATCH title 204 回读一致）；attention.01/03 定因——pending permission 仅存于 session 级 `GET /api/session/{id}/permission?directory=<d>`，question 端点不存在（等价 form 族：`GET/POST /api/session/{id}/form*`）；migration.reverse 修正检测后转 VALID_NEGATIVE（v1 拒绝打开 v2 SQLite，符合预期）。
- 未执行语义：session.06 已实证（`PUT /api/session/{id}/environment` 204 替换式注入、会话间隔离）；prompt.02 已实证（messageID 经 `data.id` 原值回显）；conn.01、event.02g 仍未实证（保守语义冻结）。另发现差异清单外新事实：v2 `POST /api/session` 的目录定位走 body `location.directory`（无 directory query 参数）。

### D5 迁移契约验证结论

- `migration.forward`：INCONCLUSIVE——fixture 造数卡在权限样本（pending permission 只有真实模型驱动工具调用才产生，容器无模型凭据）。未命中四个失败分类（非负面实证）。
- `migration.reverse`：INCONCLUSIVE——消息落盘核验的路径过滤按 v1 文件布局编写，未覆盖 v2 SQLite（实际 marker 已确认写入 `opencode.db`，属 harness 检测缺口而非数据丢失）。仅记录，不作承诺。
- **官方升级路径一次性验证（`evidence/cutover-check-20261014T043705Z/`）：PASS**——v1 serve 造 session（`ses_ec74cdc2affe4yu6EOZ8oC7KHl`）+ marker 消息 → 停 v1 → 同 XDG 数据目录换 v2 二进制启动（官方升级形态）→ v2 列出原 session（ID 不变）且消息原文逐字节一致。官方割接保留 v1 数据已实证。
- **用户决策（2026-10-14）**：「官方有 v1→v2 割接过程，我们需要的是完成 v2 的 provider 适配」「测试环境创建 v1 版本然后用官方方式升级到 v2 再验证，不需要我们自己割接」——自研快照搬运实证（migration harness）**降级停用**，不再作为 GO 门槛；迁移数据保留性采信官方升级路径的一次性实证。migration harness 代码保留在 `scripts/spike-v2/`（可重跑钩子），其 INCONCLUSIVE 结果如实归档。

### D6 承接必答项结论

| # | 必答项 | status | decision / evidence |
|---|---|---|---|
| 1 | provider_id 钉扎 | resolved | `tasks.provider_id` 保持创建时钉扎值不变；承接不改变钉扎语义（用户已锁定「任务创建时钉死 providerID」）。实际执行 provider 由分派时的机器实装版本决定，不新增持久化字段。证据：本结论节 + 用户决策记录 |
| 2 | binding envelope 映射 | pending | 逐项映射规则依赖 v2 Data 编码细节，留待 v2 provider 实现 change 完成；上游侧前置事实（session ID 保留、endpoint/凭据重启后可获得）已由 cutover-check 与 recovery.01 覆盖 |
| 3 | v1 私有 Data 解析 | pending | 承接时由 v2 provider 按自身编码重建 Data；terminal/endpoint/凭据在承接时重建（recovery.01 实证重启后上游事实可获得）。v1 Data 的逐字段迁移规则待实现 change |
| 4 | 迁移保留 session ID | resolved | **保留**。cutover-check 实证 v1 session `ses_ec74cdc2affe4yu6EOZ8oC7KHl` 经官方升级路径后在 v2 原 ID 可见（`evidence/cutover-check-20261014T043705Z/cutover-result.json`）；ID 冲突备选规则不触发 |
| 5 | 四种路径连续性 | pending | 进程重启路径已实证（recovery.01 PASS）；首次/重复承接与中途失败的连续性语义需随承接设计在实现 change 中定义并用集成测试覆盖 |
| 6 | 回滚形态区分 | resolved | 「仅代码回滚（数据未被 v2 改写）」= 回到 v1 原始状态，安全（v1 布局未被触碰）。「数据被 v2 改写后 v1 打开」= **用户锁定的产品边界：单向迁移、不提供反向可读承诺**；反向可读性本身**未验证**（migration.reverse INCONCLUSIVE，最低样本未达成，不作任何方向性推断）。证据：差异清单 #11（存储形态实证）+ migration.reverse 归档记录 |
| 7 | 承接前置条件关联 | resolved | 承接前置面 = migration.forward 四分类封闭集合（D5）。forward 未命中任何失败分类（INCONCLUSIVE 非负面实证）；官方升级路径 PASS 提供承接数据可用性的替代实证（cutover-check）。关联证据：`evidence/20261014T042602Z-90e9d6ed/results.jsonl`、`evidence/cutover-check-20261014T043705Z/` |
| 8 | 跨版本兼容边界 | resolved | v2 钉扎任务在 v1 机器激活：fail-closed——比较钉扎 providerID 主版本与机器实装主版本，钉扎 > 实装即拒绝激活；错误语义与提示口径在实现 change 中映射到既有 ErrorKind |

### D8 go/no-go 结论

**一、归档 run 的机械结论（按原始字段逐字推导，不可绕过）**

归档 run `20261014T042602Z-90e9d6ed` 的 `results.jsonl` 中，status.01 与 capability.01 均为 `exec_status=FAIL`、`evidence_validity=valid`、`capability_verdict=不可满足`——按 D8 第 3 条字面命中，该 run 的机械结论是 **NO-GO**（`summary.md` 如实判定）。修正后重跑 `20261014T045810Z-f125cafd` 同样含这两个 FAIL，其 summary 同为 NO-GO。第 0 步冲突核对：无 conflict/harness_invalid/safety_invalid 记录。

**二、后续能力评估与用户决策（修订判定的依据，逐项可审计）**

1. **capability.01 等价能力已实证**：`/openapi.json` 可匿名取回真实 OpenAPI（v2 OpenAPI 快照证据即如此获得）——契约发现能力存在等价形态，FAIL 实证的是「`/doc` 的 v1 认证门禁语义不成立」，不是能力缺失。若按等价映射重新判定：不兼容 + equivalent=`/openapi.json` 实证 + 可满足。
2. **status.01 等价能力为候选（未实证）**：`Session.Info.outcome` 枚举与 SSE 状态事件在 OpenAPI schema 中存在（源码/文档依据），但 event.02d 运行时观测 INCONCLUSIVE，等价性**待验证**。
3. **用户决策（2026-10-14）**：①「官方有 v1→v2 割接过程，我们需要的是完成 v2 的 provider 适配」「不需要我们自己割接」——**D5 migration.forward 的 GO 门槛语义被用户显式替代**：迁移数据保留性采信官方升级路径一次性实证（cutover-check PASS），自研快照搬运 harness 降级停用、不再作为 GO 门槛；②「不用完全追求 tag 一致，v2 版本的协议基本已经稳定了」——接受 v2.0.28 锁定基线（源码锚点 commit `026b106b` 与二进制构建 commit `044d8b7d` 分别记录，不视为同一基线）。

**三、修订后结论（仅在上列用户决策明确替代的验收语义下成立）**

按用户决策修订验收语义后重新推导：第 2 条不命中（官方升级路径 PASS 替代 migration.forward 门槛）。第 3 条——capability.01 经等价实证不再命中；status.01 的 `capability_verdict=不可满足` 是 probe 依据旧 wire 端点失败给出的判定，**只证明端点不兼容、未证明能力缺失**（候选等价映射存在但未实证）——修订判定依据：①该 probe 的契约断言对象是 wire 端点形状（D4 status.01 行），不是能力存在性；②候选等价形态的 schema 依据存在（v2 OpenAPI 快照）；③运行时等价性未验证（event.02d INCONCLUSIVE）。据此 status.01 的能力判定修订为「待验证」，第 3 条不据此判能力缺失，由第 4 条接住。第 4 条——16 项 live/migration INCONCLUSIVE + 11 项静态 FETCH_ERROR（共 27 项，为归档 run 原始计数；terminal.01 的 PASS 更正为 INCONCLUSIVE 另计，见下方「terminal.01 结论更正」）+ D6 第 2/3/5 项 pending + status.01 能力待验证。

**修订后最终结论：待补证据（D8 第 4 条），不输出无条件 GO。** 阻断项为：status.01 等价能力的运行时实证、模型/fixture 依赖项补证、适配缺口类修正后重跑、D6 第 2/3/5 项回填、terminal.01 拒绝语义的覆盖口径决策。这些项全部转为 v2 provider 实现 change 的前置任务，由实现 change 收口后终判。

**terminal.01 结论更正**：run `20261014T045810Z-f125cafd` 中 terminal.01 记为 PASS，其「不存在 ID 拒绝」子断言实证的只是 REST 查询 404 语义，未覆盖原契约的「第二实例选择不存在 session 拒绝启动/连接」语义（v2 serve 无 `--session`，CLI 侧无等价形态）。harness 已修正为整项 INCONCLUSIVE（共享可见事实保留实证）；归档 run 的该条 PASS 按本更正作废。是否将契约覆盖口径修订为 REST 检查，留待实现 change 决策。

**面向 v2 provider 实现 change 的建议路径**：核心 wire 形态已发现并实证（差异清单 #1–#6、#9–#12）；剩余未知项已明确列出（#7/#8 等价形态定性、待验证项三类清单）。实现 change 启动时：先按差异清单完成适配层映射（前缀/envelope/prompt/事件/permission/启动与就绪/存储访问一律走 API），再以带模型的集成环境补证模型/fixture 依赖项、修正适配缺口类后重跑、实证 status.01 候选等价形态，最后回填 D6 第 2/3/5 项与 D8 终判。

## Risks / Trade-offs

- v2 在 spike 期间发布新版 → 锁定记录含精确版本号与日期；后续实现 change 启动时先用 D3 静态层对新版做增量 diff，**再依据受影响 probe 重跑对应 live 项**；无覆盖依据时不得将旧结论扩展到新版（D3 的稳定 probe ID 与结果 schema 即为此保留的最小钩子）。
- 隔离边界不可用或 canary 未通过导致 spike 暂停 → 按 D2 停止并报告用户（环境由用户提供，边界形态可在用户环境上调整）；宁停勿错，数据迁移不可逆。
- v2 无 stable release 或 stable 与文档差异大 → 差异清单如实标注，go/no-go 建议给出「锁定 pre-release 版本 + 风险清单」的替代路径，由用户决策。
- 承接前置条件实证不成立（v2 无法兼容 v1 迁移数据的关键路径）→ 按 D6 处置：完整记录观测事实、暂停 go/no-go、提交用户决策；spike 不自动降级为「v1 任务只读」或任何其他产品语义。
- live probe 依赖真实模型调用（prompt 产生消息历史）→ 探测项区分「无模型依赖」（协议形状类）与「需模型调用」（消息历史类），后者允许配置可用的轻量模型或标记 INCONCLUSIVE 并记录原因。
- 验证实例内部的写限制机制（如 sandbox-exec）不可用 → 验证实例自身边界**经验证满足 D2 允许写入集合及全部门禁**时可作为执行边界继续；不满足则 spike 停止并报告用户，不得降级为在开发机或纯目录隔离下执行。

## Migration Plan

无运行时部署。spike 执行顺序（实现任务按此顺序拆分）：

1. 目标版本锁定（D1 两段式 manifest）与隔离环境准备（D2 沙箱 + 执行边界 + canary）。
2. 静态锚点 diff（D3 静态层）。
3. live probe 矩阵执行（D3 live 层 + D4）。
4. 单向迁移契证实测（D5）。
5. 差异清单汇总 + 承接契约与兼容边界设计结论（D6）+ go/no-go（D8）。

## Open Questions

- v2 目标版本具体版本号（spike 任务 1 即解决，不影响任务拆分）。
- v2 数据/配置目录完整布局（D2 边界保护下的写入路径查明解决）。
- v2 是否提供无模型依赖的 prompt 形状验证通道（影响 prompt.02 是否需真实模型；不阻断拆分）。
- 用户提供的验证环境具体形态（VM / 容器 / 独立系统用户）及其内部写限制机制——影响 `boundary_mechanism` 取值与 canary 目标选择，不影响任务拆分；脚本契约对任一形态统一。
