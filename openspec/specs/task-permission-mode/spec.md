## Purpose

任务级权限模式：创建任务时选定 ask / all-approve / ai-auto 三档授权策略并持久化，激活拉起 opencode 进程时按模式施加权限行为；ai-auto 由平台全局 LLM 自动判定每条权限请求，AI 不可用时转人工。

## Requirements

### Requirement: 任务权限模式选择与持久化

系统 SHALL 支持在创建任务时提供可选的权限模式参数：请求 JSON 字段为 `permission_mode`，合法取值为 `ask`、`all-approve`、`ai-auto`。未提供或为 null 视为缺省 → `ask`，与既有创建行为完全一致；非 null 字符串 trim 后为三值之一 MUST 接受；显式空串、纯空白或未知值 MUST 以 invalid_input 拒绝且 MUST NOT 产生落库或 worktree 等任何副作用。值域校验顺序为 name → mode → permission_mode（name 空白先于本参数报错）。权限模式 MUST 随任务持久化，任务后续激活 MUST 使用落库值，MUST NOT 从其他来源重算。权限模式创建后不可修改：系统 MUST NOT 提供修改入口。既有存量任务（无落库值）MUST 按 `ask` 处理，不做数据迁移改写。

#### Scenario: 缺省创建为 ask

- **WHEN** 用户创建任务且未提供权限模式
- **THEN** 任务以 `ask` 模式落库，权限行为与既有任务一致（人工逐条批准）

#### Scenario: 显式选择模式

- **WHEN** 用户创建任务并提供合法权限模式（`ask` / `all-approve` / `ai-auto`）
- **THEN** 该模式随任务落库，并在后续每次激活时生效

#### Scenario: 非法模式拒绝

- **WHEN** 用户创建任务并提供非法权限模式取值（含显式空串、纯空白或三值之外的未知值）
- **THEN** 系统返回 invalid_input，任务未创建，无任何落库与文件/git 副作用

#### Scenario: 创建后不可修改

- **WHEN** 任务已创建
- **THEN** 系统不提供修改其权限模式的入口；任务生命周期内权限模式保持落库值不变

### Requirement: 激活时按权限模式施加权限行为

系统 SHALL 在任务激活拉起 opencode 进程时，按任务落库的权限模式施加对应权限行为：`ask` 模式 MUST 保持 opencode 原生权限行为（不附加任何自动批准能力，与现状一致）；`all-approve` 模式 MUST 以自动批准方式启动 opencode——未被显式 deny 的权限请求自动批准，用户 opencode 配置中的显式 deny 规则 MUST 仍然生效；`ai-auto` 模式 MUST NOT 附加自动批准能力，权限请求按「ai-auto 权限请求自动判定」处理。

#### Scenario: ask 模式行为不变

- **WHEN** `ask` 模式任务激活
- **THEN** opencode 进程按现状参数启动（不附加自动批准或 AI 判定）；opencode 原生配置产生的 pending 权限请求由人工逐条处理，原生 allow/deny 行为保持不变

#### Scenario: all-approve 模式自动批准

- **WHEN** `all-approve` 模式任务激活，opencode 产生未被显式 deny 的权限请求
- **THEN** 该请求被自动批准，无需人工干预；opencode 配置中显式 deny 的规则仍被拒绝

### Requirement: ai-auto 权限请求自动判定

`ai-auto` 模式任务产生权限请求时，系统 SHALL 调用平台全局 LLM 配置（可用性判定见 ai-provider-config spec）对该请求做通过/拒绝判定：判定通过 MUST 回复 `once`（仅当次请求生效），判定拒绝 MUST 回复 `reject`；系统 MUST NOT 使用 `always` 回复。自动判定 MUST 覆盖全部观察路径——无论权限请求经 SSE 事件还是 REST 对账发现，均纳入判定。**计数契约：同一 runtime 实例内，每个请求最多发起一次自动判定尝试；满足准入且仍为待处理的请求 MUST 纳入判定扫描。runtime 重建（含 server 重启）后仍待处理的同一请求可被重新判定——系统不保证跨 runtime 的全局恰好一次。**任务运行时未就绪期间（activating、启动恢复重建、挂起失败修复重建）产生的权限请求 MUST 在该次 runtime 就绪提交后纳入判定（提交前不发起判定），MUST NOT 遗漏。仅判定通过/拒绝才回复；判定不确定、失败、实例停止或回复能力不可用时 MUST NOT 回复。LLM 未配置、调用失败、超时或输出非法时，系统 MUST NOT 回复该请求——请求保持待处理并转人工（退化为 `ask` 体验），判定失败本身 MUST NOT 影响任务进程与其他权限请求的处理。回复时请求已被了结（如人工已在终端批准/拒绝）时，系统 MUST 视为正常竞态忽略，MUST NOT 报错或重试。回复请求已发出但结果未知（超时/连接失败/服务端 5xx，回复可能已被受理）时，系统 MUST NOT 重试、MUST NOT 补偿、MUST NOT 本地断言该请求已批准或已拒绝——其后续状态由既有 SSE/REST 对账收敛，若仍为待处理则照常供人工处理。

判定输入 SHALL 包含平台语境与请求详情：平台语境为任务名、项目名、项目类型、任务模式、任务目录、项目目录、分支；请求详情为 permission、patterns 与按权限类别从请求 metadata 提取的详情（bash→完整命令 command；edit/write/apply_patch→文件目标与有界 diff；webfetch→完整 URL；task→子任务描述与类型；grep/glob→搜索范围；read→路径（patterns 即路径）；external_directory→越界目录信息；未识别类别→仅 permission 与 patterns；**字段级提取矩阵、界值与截断语义的唯一来源是 design D5 提取表，本节不复述**）。任务名、项目名、分支、命令、diff、描述等一切请求相关内容 MUST 以不可信数据形式传入判定器（JSON 编码的用户消息），MUST NOT 作为指令影响判定器行为。

判定输出格式：判定器 SHALL 要求模型在恰好一个 `<verdict>` 标签对内输出 `APPROVE` / `REJECT` / `UNCERTAIN` 之一（标签外允许简短理由）。**解析采用有限标签协议（不做完整 XML 解析）**：`<verdict` 与 `</verdict>` 字面子串各恰好出现一次、无属性、固定小写，标签内容 trim 后逐字等于三值之一时按内容处理；缺失任一侧标签、计数不为 1、嵌套、自闭合、带属性、大小写变体、内容非三值，MUST 一律视为判定失败（不回复、转人工、审计记 FAILED）。

证据降级规则：确定性降级原因三值——`missing_critical` / `malformed_critical` / `truncated_critical`（类别关键字段缺失 / 有效 metadata object 内字段形状错误 / 因截断或裁剪丢失，关键字段集合与界值见 design D5 提取表）。metadata 有效形状仅为 JSON object；absent/null/非法 JSON/合法但非 object 一律在观察层归 nil，关键类别统一记 `missing_critical`。多原因并存时按固定优先级取单值：`malformed_critical` > `missing_critical` > `truncated_critical`（全量扫描后统一选取，不依赖校验先后）。存在降级原因时系统 MUST 判定 UNCERTAIN 转人工（判定器内短路、不调用 LLM），MUST NOT 依据残缺证据批准；**降级短路先于 LLM 配置检查**（未配置与降级并存时结果为 UNCERTAIN 而非 FAILED）。webfetch/read/grep/glob/task 的详情缺失时以 permission+patterns 判定（这些类别 patterns 已含主要信息）。metadata 整体缺失或畸形时 MUST NOT 丢弃该 pending 请求——注意力集合与人工处理行为不变，仅判定信息质量按上述规则降级。

#### Scenario: AI 判定通过

- **WHEN** `ai-auto` 任务的 opencode 产生权限请求，LLM 判定可通过
- **THEN** 系统以 `once` 回复该请求，opencode 继续执行该次操作

#### Scenario: AI 判定拒绝

- **WHEN** `ai-auto` 任务的 opencode 产生权限请求，LLM 判定不可通过
- **THEN** 系统以 `reject` 回复该请求，该次操作被拒绝

#### Scenario: LLM 不可用转人工

- **WHEN** `ai-auto` 任务产生权限请求，但 LLM 未配置、调用失败、超时或输出非法
- **THEN** 系统不回复该请求，请求保持待处理，由人工按 `ask` 体验处理；任务进程与其他请求不受影响

#### Scenario: 与人工回复竞态

- **WHEN** 系统回复某权限请求时，该请求已被人工或其他途径了结
- **THEN** 系统忽略该竞态，不产生错误、不重试

#### Scenario: 就绪提交前产生的请求在提交后纳入判定

- **WHEN** `ai-auto` 任务在 runtime 就绪提交完成前（activating / 启动恢复重建 / 挂起失败修复重建）已产生权限请求（经 SSE 或对账观察登记），随后该次 runtime 就绪提交成功
- **THEN** 提交前系统不发起判定；提交后若该请求仍为待处理，系统将其纳入一次判定尝试；是否回复遵循判定结果规则（判定通过/拒绝才回复，不确定或失败不回复）

#### Scenario: 回复结果未知不重试

- **WHEN** 系统已发出回复请求但结果未知（超时/连接失败/服务端 5xx）
- **THEN** 系统不重试、不补偿、不本地断言该请求状态；其后续状态由既有对账收敛，仍为待处理时照常供人工处理

#### Scenario: XML verdict 正常提取

- **WHEN** 模型输出包含恰好一个 `<verdict>` 标签且内容为 APPROVE/REJECT/UNCERTAIN 之一（标签外可有简短理由）
- **THEN** 系统按标签内容作为判定结论处理（APPROVE→once，REJECT→reject，UNCERTAIN→不回复转人工）

#### Scenario: XML verdict 缺失或伪造按失败处理

- **WHEN** 模型输出缺失 `<verdict>` 标签、含多个 `<verdict>` 标签、或标签内容非三值之一
- **THEN** 系统视为判定失败：不回复该请求、转人工、审计记 FAILED

#### Scenario: 类别关键证据缺失或超限转人工

- **WHEN** bash/edit/write/apply_patch/external_directory 请求的类别关键字段（完整命令、diff、越界目录信息）缺失、畸形或超系统界值
- **THEN** 系统判定 UNCERTAIN 转人工（不依据残缺证据批准），审计记录标注原因

#### Scenario: metadata 缺失不丢请求

- **WHEN** 权限请求缺失或携带畸形 metadata
- **THEN** 该请求保持在 pending 集合中供人工处理，注意力对账与前端投影行为不变；判定信息质量按证据降级规则处理

### Requirement: ai-auto 权限判定审计日志

`ai-auto` 模式下，每次自动判定尝试终结时（含判定失败、不确定与未回复分支），系统 SHALL 向 `<数据目录>/logs/ai-permission-audit.jsonl` 追加恰好一条 JSONL 记录（一行一条 JSON 对象，文件权限 0600，单文件追加、不做滚动）。同一请求跨 runtime 重新判定时，各次判定尝试各自产生独立记录（与 per-runtime 判定计数契约一致）。记录计数从调用判定器起算：调用判定器之前被准入门禁拒绝的请求不产生审计记录；一旦调用判定器，无论判定结论如何、后续是否实际发送回复，该次尝试恰好产生一条记录。

每条记录 SHALL 包含字段：`time`（RFC3339 时间戳）、`task_id`、`task_name`、`request_id`、`permission`（工具名）、`patterns`（请求的模式列表，与观察到的权限请求一致）、`verdict`（`APPROVE` / `REJECT` / `UNCERTAIN` / `FAILED`，FAILED 表示 LLM 未配置、调用失败、超时或输出非法）、`reply_result`（`ok`：回复已被服务端接受；`gone`：请求已被了结的正常竞态；`unknown`：回复结果未知；`unsupported`：回复能力不可用；`not_applicable`：未实际发送回复——verdict 为 UNCERTAIN/FAILED 时，或 verdict 为 APPROVE/REJECT 但回复未实际发送（实例失效、实例停止、ctx 取消、发送前门禁拒绝或回复客户端构造失败）时）。verdict 为 APPROVE/REJECT 且 reply_result 为 `ok` 时，记录 SHALL 额外包含 `reply`（`once` / `reject`）。记录 SHALL 额外包含 `detail` 字段（string：本次判定使用的同一份提取详情的摘要文本，含完整命令或文件目标等，**最终字符串 UTF-8 长度 ≤ 1024 字节（降级原因前缀与截断标记均计入；生成时先预留前缀与标记空间再截断主体）**，供人工阅读判定依据；无详情时为空字符串、字段必有）；证据降级（关键字段缺失/畸形/超限）时 `detail` SHALL 标注降级原因。

审计写入是旁路：写入失败 MUST NOT 影响判定、回复与任务进程——仅记录普通日志，该请求的判定与回复行为与无审计时完全一致。

#### Scenario: 放行与拒绝留痕

- **WHEN** `ai-auto` 任务的权限请求被判定通过并以 `once` 回复（或被判定拒绝并以 `reject` 回复）
- **THEN** 审计文件追加一条记录，含该请求的工具名、具体模式（patterns）、判定结论与回复结果 `ok`

#### Scenario: 转人工与失败留痕

- **WHEN** 判定结果为 UNCERTAIN，或 LLM 未配置/失败/超时/输出非法（FAILED）
- **THEN** 审计文件追加一条记录，`reply_result` 为 `not_applicable`，请求保持待处理转人工的行为不变

#### Scenario: 判定后未发送回复留痕

- **WHEN** 判定结论为 APPROVE/REJECT，但回复未实际发送（实例失效、停止、ctx 取消、发送前门禁拒绝或回复客户端构造失败）
- **THEN** 审计文件追加一条记录，`reply_result` 为 `not_applicable`，不含 `reply` 字段

#### Scenario: 审计写入失败不影响行为

- **WHEN** 审计文件写入失败（如磁盘错误）
- **THEN** 该请求的判定与回复行为与无审计时完全一致，仅普通日志记录写入失败

### Requirement: 权限模式只读输出

系统 SHALL 在任务相关读模型中透出权限模式：创建响应、任务详情、项目任务摘要、active 任务概览均 MUST 包含 `permission_mode` 字段，取值为 `ask` / `all-approve` / `ai-auto` 三值枚举。存量空值 MUST 输出 `ask`；未知持久化值 MUST fail-closed 返回 internal error，MUST NOT 产出缺/坏值元素。该字段为只读输出，系统 MUST NOT 提供修改入口。

#### Scenario: 读模型透出三值

- **WHEN** 查询任务的创建响应、详情、项目任务摘要或 active 任务概览
- **THEN** 输出包含 `permission_mode`，取值为三值枚举之一

#### Scenario: 存量任务输出 ask

- **WHEN** 查询在本特性上线前创建的存量任务（无落库值）
- **THEN** 各读模型输出 `permission_mode` 为 `ask`

#### Scenario: 未知持久化值 fail-closed

- **WHEN** 任务持久化的权限模式为三值之外的未知值（持久化损坏）
- **THEN** 相关读模型返回 internal error，不产出含缺/坏值的结果

### Requirement: Web 新建任务权限模式选择

Web 新建任务表单 SHALL 提供权限模式选择项，三档取值与创建接口一致（`ask` / `all-approve` / `ai-auto`），缺省选中 `ask`；提交时按选择传递权限模式参数。

#### Scenario: 表单缺省 ask

- **WHEN** 用户打开新建任务表单且未改动权限模式选择
- **THEN** 表单缺省选中 `ask`，创建的任务为 `ask` 模式

#### Scenario: 表单选择模式创建

- **WHEN** 用户在新建任务表单选择 `all-approve` 或 `ai-auto` 并提交
- **THEN** 创建请求携带所选权限模式，任务按该模式落库

#### Scenario: 切换项目重置为 ask

- **WHEN** 用户在新建任务面板改变已选项目（含切换项目、清除选择、切到 dir 项目）
- **THEN** 权限模式选择重置为 `ask`；不改变已选项目的面板信号保持当前选择
