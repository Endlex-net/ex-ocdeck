## ADDED Requirements

### Requirement: 企业微信渠道

企业微信（wecom）渠道 SHALL 通过 HTTP POST 向用户配置的完整 webhook URL 发送 JSON 请求体，`Content-Type: application/json`。请求体 MUST 为：

```json
{"msgtype":"markdown","markdown":{"content":"..."}}
```

`msgtype` MUST 固定为 `markdown`（MUST NOT 使用 `text` / `news` / `template_card` / `markdown_v2`）。`markdown.content` MUST 由通知意图渲染，逐字模板为：

```
**{Intent.Title}**
{Intent.Body}

[打开任务]({Intent.URL})
```

其中 `{Intent.Title}`、`{Intent.Body}`、`{Intent.URL}` 为意图字段原值。Title 行与 Body 之间为一个换行（无空行）；Body 与链接行之间一个空行。`Intent.URL` 为空时 MUST 省略链接行（含其前导空行），MUST NOT 渲染空 `[]()`。渠道 MUST NOT 主动插入 `<@all>`、`<@userid>` 或任何 @ 提及；MUST NOT 扫描或剥离 Title/Body 中可能出现的同形片段。content MUST 为 UTF-8，长度 MUST NOT 超过 4096 字节；超限 MUST 截断至不超过 4096 字节的有效 UTF-8，MUST NOT 因此将投递判定为失败。

渠道名 MUST 为 `wecom`。渠道「已配置」判定见「通知渠道投递与降级」（webhook URL 非空）。未启用或未配置时 skipped，MUST NOT 发起 HTTP 请求、MUST NOT 报错。Caps=0（不声明 Group / Replace / Withdraw）。webhook URL 为用户粘贴的完整地址（含 query），系统 MUST 原样作为 POST 目标，MUST NOT 拼接 path、MUST NOT 剥离 query。

webhook URL 非空时校验 MUST 为：有非空 host 的 https hierarchical URL，MUST NOT 含 userinfo、fragment；query 与非根 path 允许。空串合法。MUST NOT 以厂商 host 白名单拒绝保存或投递。http 或其它 scheme MUST 拒绝保存。

单次请求超时 MUST 为 10 秒，MUST NOT 跟随重定向，MUST NOT 自动重试，响应体读取 MUST 有 64 KiB 大小上界，超限 MUST 判定为失败。投递成功判定 MUST 为：HTTP 2xx 且响应体 JSON 的 `errcode` 字段等于 0；MUST 以 `errcode` 判定，MUST NOT 匹配 `errmsg` 文本。响应体非法 JSON、缺 `errcode` 字段、非 2xx、或 `errcode` 非 0，均 MUST 判定为失败。失败 Result.Err 可含 HTTP status 与 `errcode` 数值，MUST NOT 含 webhook URL、请求体或响应原文。完整 webhook URL、请求体与企微原始响应体 MUST NOT 写入日志。

本期 MUST NOT 实现 @all / userid、多群 URL 列表、厂商 host 白名单或限流排队。

#### Scenario: 发送企业微信 markdown 推送

- **WHEN** wecom 渠道已启用且配置了非空 webhook URL，触发一次通知
- **THEN** 系统对该 URL 发出 HTTP POST，JSON 为 `msgtype=markdown` 且 content 含加粗标题、正文与 `[打开任务](<Intent.URL>)` 链接；渠道 MUST NOT 在模板字段之外主动插入任何 @ 提及；Title/Body 原文中的同形片段按本 requirement 保留

#### Scenario: 未配置 webhook URL 视为未配置

- **WHEN** wecom 渠道开关打开但 webhook URL 为空
- **THEN** 该渠道按未配置处理，不投递、不报错、不发起 HTTP 请求

#### Scenario: 推送失败判定

- **WHEN** 企微接口返回非 2xx，或响应体 JSON 的 errcode 非 0，或响应体非法 / 缺 errcode
- **THEN** 该次投递判定为失败，记录日志（不含 webhook URL、请求体与响应原文），不影响其他渠道

#### Scenario: 超长正文截断仍投递

- **WHEN** 渲染后的 markdown content 超过 4096 字节 UTF-8
- **THEN** content 截断至不超过 4096 字节的有效 UTF-8 后仍发起投递，不因此判定失败

## MODIFIED Requirements

### Requirement: 通知渠道投递与降级

系统 SHALL 支持四类通知渠道：网页（web）、Bark 推送（bark）、macOS 本地通知（macos）、企业微信群机器人（wecom）。每次触发 MUST 向全部"已启用且已配置"的渠道并行投递；单一渠道失败 MUST NOT 影响其他渠道投递，MUST NOT 阻塞或影响任务主流程，失败 MUST 记录日志。渠道抽象 MUST 为渠道无关的通知意图（任务 ID/名称、类别、级别、标题、正文、跳转链接；类别详情由内容组装进入正文，意图不单独携带详情字段），各渠道按声明的能力位（分组 Group / 同键替换 Replace / 撤回 Withdraw）落地；标题统一携带任务名（见「通知内容与跳转链接」的标题格式），分组能力缺失的唯一降级表现为通知中心内不折叠分组，MUST NOT 报错阻断。通知级别 MUST 按类别映射：question/permission → timeSensitive；error → timeSensitive（Bark critical 穿透勿扰与静音，error 场景不授予该最高优先级）；retry → timeSensitive；idle → active；test → active。

渠道"已配置"判定与能力位矩阵 MUST 遵循：web —— 启用即已配置，Caps=Replace（无 Group，标题加任务名前缀），零连接投递计为 failed；bark —— endpoint 与 token 均非空才算已配置（否则 skipped），Caps=Group；macos —— 仅 darwin 且 terminal-notifier 或 osascript 可用才算可用（否则 skipped），terminal-notifier Caps=Group|Replace，osascript Caps=0（标题加任务名前缀）；wecom —— webhook URL 非空才算已配置（否则 skipped），Caps=0；所有渠道本期 MUST NOT 声明 Withdraw。

#### Scenario: 多渠道并行投递

- **WHEN** 触发一次通知且 bark 与 web 渠道均已启用并配置
- **THEN** 两个渠道各自收到投递

#### Scenario: 单渠道失败隔离

- **WHEN** bark 渠道投递失败（网络错误/凭证失效）
- **THEN** web 渠道投递不受影响，任务主流程不受影响，失败写入日志

#### Scenario: 分组能力缺失自动降级

- **WHEN** 某渠道不支持分组能力
- **THEN** 该渠道正常投递（标题本就统一携带任务名），仅通知中心内不折叠分组，不报错

#### Scenario: 企业微信与现有渠道共同启用

- **WHEN** 触发一次通知且 wecom 与 web 渠道均已启用并配置
- **THEN** 两个渠道各自收到投递

### Requirement: 通知配置存储

系统 SHALL 将通知配置存储于 `<dataDir>/notification.json`，写入 MUST 采用临时文件 + 原子 rename，文件权限 MUST 为 0600。磁盘 schema 的唯一表述如下（除下文兼容规则外，字段均为必填键，未知字段 MUST 忽略）：

```json
{
  "enabled": false,
  "categories": {"question": true, "permission": true, "idle": true, "retry": true, "error": true},
  "idle_timeout_seconds": 60,
  "channels": {
    "web":   {"enabled": false},
    "bark":  {"enabled": false, "endpoint": "https://api.day.app", "token": ""},
    "macos": {"enabled": false},
    "wecom": {"enabled": false, "url": ""}
  },
  "llm_summary": false,
  "base_url": ""
}
```

校验规则：`idle_timeout_seconds` MUST ∈ [10, 3600]；`endpoint` 与 `base_url` 非空时 MUST 为有非空 host 的 http(s) hierarchical URL，且 MUST NOT 含 userinfo、query、fragment，path 仅允许空或 `/`；`channels.wecom.url` 非空时 MUST 为有非空 host 的 https hierarchical URL，MUST NOT 含 userinfo、fragment，query 与非根 path 允许；校验失败 MUST 拒绝保存并返回 422（invalid_input）。bark token 与 wecom webhook URL MUST NOT 在日志或 API 响应中以明文完整出现。

兼容规则：磁盘加载与 PUT 共用同一解码入口。`channels.wecom` 对象缺失或为 null 时，两个字段均取默认（`enabled=false`、`url=""`）。对象在场时，缺失或为 null 的嵌套键各自独立取默认（`enabled` 缺/null → `false`；`url` 缺/null → `""`），在场的键保持解码值。上述缺键/null MUST 视为合法，MUST NOT 因此产生 load_error 或返回 422。wecom 在场但字段类型不匹配仍视为损坏。除 wecom 外的既有必填键缺失或 null 的损坏语义不变。成功写入后的磁盘文件 MUST 含完整 wecom 键。

#### Scenario: 保存合法配置

- **WHEN** 用户提交合法的通知配置
- **THEN** 配置以 0600 权限原子写入 `<dataDir>/notification.json` 并立即生效

#### Scenario: 拒绝非法配置

- **WHEN** 提交的空闲阈值越界或 bark endpoint/base_url/wecom url 非法
- **THEN** 保存被拒绝并返回 422，原配置不变

#### Scenario: 未知字段忽略

- **WHEN** 提交的配置含 schema 外未知字段
- **THEN** 未知字段被忽略，已知字段正常校验与保存

#### Scenario: 旧配置缺 wecom 键按默认关闭加载

- **WHEN** 磁盘上的 notification.json 为升级前的三渠道合法文件（无 `channels.wecom`）
- **THEN** 加载成功且无 load_error，wecom 按默认关闭（enabled=false、url=""）运行，不得整份配置失效

### Requirement: 通知配置读写 API

系统 SHALL 提供通知配置的读取与保存接口（`GET/PUT /api/v1/notification/config`），鉴权与 server 其他管理 API 一致。

GET MUST 返回 200 与配置对象，JSON 形状唯一表述如下（snake_case；bark 的 `token_masked` 替代 `token`，wecom 的 `url_masked` 替代 `url`）：

```json
{
  "enabled": false,
  "categories": {"question": true, "permission": true, "idle": true, "retry": true, "error": true},
  "idle_timeout_seconds": 60,
  "channels": {
    "web":   {"enabled": false},
    "bark":  {"enabled": false, "endpoint": "https://api.day.app", "token_masked": ""},
    "macos": {"enabled": false},
    "wecom": {"enabled": false, "url_masked": ""}
  },
  "llm_summary": false,
  "base_url": ""
}
```

`load_error` 为只读字段，仅在配置损坏或不可读时出现（非空字符串），正常读取时 MUST 省略该字段。`token_masked` 掩码规则与 ai-provider-config 的 api_key 一致（≥8 位回显前 4 位 + `***`，<8 位纯 `***`，无 token 为空串）。`url_masked` 掩码规则：无 URL 为空串；非空 URL MUST 固定为 `***`（完整 webhook URL 整体按密钥保护，MUST NOT 回显任何原文片段）。响应中 MUST NOT 出现完整 token 或完整 webhook URL。配置文件不存在时返回默认配置（即存储 schema 所示默认值：总开关关闭、类别全开、阈值 60、渠道全关）；配置文件损坏或不可读时 MUST NOT 返回 500，而是返回默认配置并携带人类可读的 `load_error` 字段。

PUT 请求体 JSON 形状与 GET 相同，仅下列差异：bark 渠道的令牌字段名为 `token`（非 `token_masked`）；wecom 渠道的 webhook 字段名为 `url`（非 `url_masked`）；MUST NOT 含 `load_error`（只读字段，出现按未知字段忽略）。token / url 语义：值为空字符串或任意含 `***` 的字符串 MUST 视为掩码并保留已存储的原值；无已存储值时按空处理，该渠道视为未配置，不因此拒绝保存。PUT 请求体缺少 `channels.wecom` 或其嵌套键时 MUST 按「通知配置存储」兼容规则填充，MUST NOT 因此返回 422。成功返回 200 与 GET 同形响应（含 `token_masked` 与 `url_masked`，不含 `load_error`）；请求体非法 JSON 返回 400；业务校验失败返回 422；文件写入失败返回 500 且 MUST 保持旧内存快照不变。请求体上限为 4 KiB：`<=4096` 字节继续解码，`>4096` 字节返回 400 `invalid_input`，超限请求 MUST NOT 进入解码、校验或文件写路径。并发 PUT MUST 由写锁串行化「合并 → 校验 → 原子写入 → 快照替换」全过程，按写锁获得顺序 last-writer-wins。业务校验 MUST 发生在 token/url 掩码合并之后，MUST NOT 对合并前的掩码占位值（空串或含 `***` 的字符串）做 URL/业务校验。

#### Scenario: 读取默认配置

- **WHEN** 从未保存过通知配置，客户端请求 GET
- **THEN** 返回 200 与默认配置（含 wecom 默认关闭与空 `url_masked`）

#### Scenario: 损坏配置降级读取

- **WHEN** notification.json 损坏，客户端请求 GET
- **THEN** 返回 200、默认配置与 load_error，不返回 500

#### Scenario: 保存时保留原 token

- **WHEN** 用户仅修改类别开关并以掩码值提交 bark token
- **THEN** 保存成功，已存储的 token 不被清空

#### Scenario: 保存时保留原 wecom webhook URL

- **WHEN** 用户仅修改类别开关并以空串或含 `***` 的值提交 wecom url
- **THEN** 保存成功，已存储的 webhook URL 不被清空

#### Scenario: GET 不回显 wecom webhook URL 原文

- **WHEN** 已存储非空 wecom webhook URL，客户端请求 GET
- **THEN** 响应 `channels.wecom.url_masked` 为 `***`，MUST NOT 出现 URL 原文

#### Scenario: 写失败保持旧快照

- **WHEN** PUT 校验通过但文件写入失败
- **THEN** 返回 500，内存中的配置快照保持旧值不变

#### Scenario: PUT 请求体超过 4 KiB

- **WHEN** PUT 请求体大于 4096 字节
- **THEN** 返回 400 `invalid_input`，MUST NOT 解码、校验或写文件

### Requirement: 通知设置界面

前端 SHALL 在设置页（`#/configs`）新增通知子标签（深链 `#/configs#notifications`），MUST NOT 保留独立页面。界面 MUST 包含：总开关、五个类别的独立开关、空闲阈值输入、四个渠道的独立开关与参数（bark：endpoint/token；wecom：webhook URL；web：浏览器权限状态展示与申请入口）、LLM 总结开关、`base_url` 输入（附"Bark 在手机上打开链接需要可达地址"提示）、保存按钮与测试通知入口。wecom webhook URL 输入 MUST 为密码型，MUST NOT 回显明文；GET 的 `url_masked` 仅作已配置提示，留空保持已存储 URL（与 bark token 服务端语义一致）。保存成功/失败 MUST 有明确反馈；GET 返回 load_error 时 MUST 在界面展示。

#### Scenario: 配置通知

- **WHEN** 用户在通知子标签开启总开关与 bark 渠道、填写 token 并保存
- **THEN** 界面提示保存成功，配置立即生效

#### Scenario: 配置企业微信渠道

- **WHEN** 用户在通知子标签开启 wecom 渠道、粘贴完整 webhook URL 并保存
- **THEN** 界面提示保存成功，再次打开时 URL 输入框不回显原文

#### Scenario: 深链直达

- **WHEN** 用户打开 `#/configs#notifications`
- **THEN** 设置页打开并直接选中通知子标签

#### Scenario: 展示配置加载错误

- **WHEN** 配置文件损坏，用户打开通知子标签
- **THEN** 界面展示 load_error 提示，可直接重新保存合法配置修复
