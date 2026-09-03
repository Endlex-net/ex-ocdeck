## Context

ocdeck 已有任务通知能力：领域抽象 `notification.Channel`（`internal/domain/notification/notification.go:82`），配置登记在 `ChannelsConfig{Web, Bark, Macos}`（`config.go:47`），实例由 `notify.BuildChannels` 组装为稳定顺序 web → bark → macos（`internal/infrastructure/notify/channels.go:9`），启用判定在 `resolveOneChannel` 的 `switch ch.Name()`（`internal/application/notification/dispatch.go:108`）。磁盘 schema 经 `wire*` 指针 DTO 强制必填键（`store.go:216-239`），缺键/null 整份降级为 `DefaultConfig()` + `load_error`。Bark 是唯一 HTTP 渠道：10s 超时、禁重定向、64KiB 响应上界、成功 = HTTP 2xx 且 JSON `code==200`（`bark.go:87-134`）。Bark / `base_url` 校验 `validateOptionalURL` 禁止 query 与非根 path（`config.go:120-128`），不能直接套用到企微 webhook。

企业微信群机器人官方契约（https://developer.work.weixin.qq.com/document/path/91770）：POST `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<KEY>`，`Content-Type: application/json`，markdown 体为 `{"msgtype":"markdown","markdown":{"content":"..."}}`，`content` 最长 4096 字节 UTF-8，支持 `[text](url)`，成功 `errcode==0`（以 errcode 判定、不匹配 errmsg，https://developer.work.weixin.qq.com/document/path/90313）。各家 webhook 协议不兼容，本变更只加专用 `wecom` 渠道。

行为规范（已配置判定、wire、掩码、缺键兼容、失败语义）的唯一归属是 spec delta「企业微信渠道」及被修改的「通知渠道投递与降级 / 通知配置存储 / 通知配置读写 API / 通知设置界面」。本文只承载机制与决策理由；提及行为规范时按 requirement 名引用。

## Goals / Non-Goals

**Goals:**

- 按 spec「企业微信渠道」落地专用 wecom 适配器，并接入既有渠道注册、解析、配置存储、API、设置页与测试通知
- 旧 `notification.json` 缺 `channels.wecom` 时按 spec「通知配置存储」兼容规则默认关闭填充，不整份失效
- 完整 webhook URL 按 spec「通知配置读写 API」整体掩码；日志与 Result.Err 不泄漏 URL / 请求体 / 响应原文

**Non-Goals:**

- 通用 webhook、钉钉 / 飞书 / Slack
- @all / userid、news / template_card / markdown_v2、多群 URL
- 厂商 host 白名单、20 条/分钟限流排队或自动重试
- 改五类触发器、门禁、LLM 总结、web / Bark / macOS 行为
- 抽取跨渠道 HTTP 客户端框架；不新增 Fx / 事件 / 领域 Intent 字段

## Decisions

### D1: 沿用既有 Channel 插件点，不引入新抽象

```
domain/notification          ChannelsConfig.Wecom + Validate(wecom URL)
        ▲
application/notification     resolveOneChannel case "wecom"
        ▲
infrastructure/notify        wecom.go + BuildChannels 追加
api / web                    DTO + 设置页
```

落地顺序与职责：

| 落点 | 职责 |
| --- | --- |
| `internal/domain/notification/config.go` | 新增 `WecomChannelConfig`，字段 `Enabled bool`（json `enabled`）、`URL string`（json `url`）；`ChannelsConfig` 增加 `Wecom`（json `wecom`）；`DefaultConfig` 默认关闭且 `url=""`；`Validate` 调用 wecom URL 校验（不改 bark/`base_url` 规则） |
| `internal/domain/notification/notification.go` | 仅更新 `ChannelConfig` 注释：wecom 把完整 webhook URL 放入 `Endpoint`，`Token` 留空 |
| `internal/infrastructure/notify/wecom.go` | 新适配器：`Name()="wecom"`，`Caps()=0`，`Send` 渲染 markdown 并 POST |
| `internal/infrastructure/notify/channels.go` | `BuildChannels` 追加 `NewWecomChannel()`，顺序变为 web、bark、macos、wecom |
| `internal/application/notification/dispatch.go` | `resolveOneChannel` 增加 `case "wecom"` |
| `internal/infrastructure/notify/store.go` | `wireWecom`；缺键填充；Put 合并 url；Put 为 PUT 唯一业务校验点（校验错误可识别）；GET 掩码 |
| `internal/api/notification_config.go` | GET `url_masked` / PUT `url`；删除 handler 预校验；Put 校验错误→422、写盘→500；沿用 4 KiB 请求体上限 |
| `web/src/types.ts`、`NotificationConfigPanel.tsx` | 四渠道类型与密码型 URL 输入 |

不改 `Channel` 接口、不改 `deliverParallel`、不改触发器。application 仍不得 import infrastructure（既有 `import_graph_test.go`）。

**不选**独立 webhook 框架或策略表：本期只有一家厂商，bark 已证明「专用适配器 + Name switch」足够。

### D2: ChannelConfig.Endpoint 承载完整 webhook URL

`ChannelConfig` 现为 `{Endpoint, Token}`，仅 bark 使用（`notification.go:74-77`）。wecom 只有一个密钥型地址，映射为：

- `resolveOneChannel`：`Enabled && URL != ""` 才配置成功；`ResolvedChannel.Config.Endpoint = cfg.Channels.Wecom.URL`，`Token` 空
- `WecomChannel.Send`：`cfg.Endpoint` 原样作为 POST URL，MUST NOT `TrimRight`、MUST NOT 拼接 path、MUST NOT 剥离 query

**不选**给 `ChannelConfig` 新增 `URL` 字段：会迫使 web/bark/macos 的零值/测试同步改动，无行为收益。

```
evaluate / SendTestNotification
        │
        ▼
resolveOneChannel(name)
  web     → enabled
  bark    → enabled && endpoint && token
  macos   → enabled && Available()
  wecom   → enabled && url != ""   ──► Config.Endpoint = url
  default → skipped
        │
        ▼
deliverParallel → Channel.Send(ctx, Intent, Config)
        │
        ▼
WecomChannel.Send
  渲染 markdown.content（spec 逐字模板）
  截断至 ≤4096 字节有效 UTF-8
  POST cfg.Endpoint
  2xx && errcode==0 → OK
```

### D3: wecom URL 专用校验，禁止复用 validateOptionalURL

`validateOptionalURL`（`config.go:100-129`）禁止 query / 非根 path，且错误信息内嵌 raw URL。wecom 另写 `validateWecomURL`（同文件，仅被 `Config.Validate` 在 bark/`base_url` 之后调用）：

- 空串合法
- `url.Parse` 失败、`Opaque != ""`、`Hostname()==""`、`User != nil`、原始串含 `#` → 非法
- scheme MUST 为 `https`（http 及其它非法）
- query（含空 `?`）与任意 path 合法
- MUST NOT 校验 host 是否为 `qyapi.weixin.qq.com`，MUST NOT 校验 query 是否含 `key=`

错误信息 MUST NOT 包含 URL 原文（PUT 422 回传校验错误的 `Error()`，见 D4/D7）。`url.Parse` 失败也 MUST NOT `%w` 包装（parse error 常含原文）。固定文案形如 `invalid wecom url: <reason>`，reason ∈ `invalid url` / `must be a hierarchical https URL` / `scheme must be https` / `host must not be empty` / `userinfo not allowed` / `fragment not allowed`。

bark endpoint 与 `base_url` 规则保持不变。

### D4: 缺 wecom 键默认关闭填充（DecodeConfig 唯一入口）

磁盘加载与 PUT 共用 `DecodeConfig`（`store.go:205`）。wecom 与既有必填键不同：

- `channels.wecom` 对象或其嵌套 `enabled` / `url` 缺失或 JSON null → 填 `enabled=false`、`url=""`，视为合法
- wecom 在场但字段类型不匹配 → 仍随 `json.Unmarshal` 失败，走损坏语义
- 其它既有必填键（含 `channels.bark.*`）缺失/null 语义不变，仍 `missing required key`

实现：`wireChannels` 增加 `Wecom *wireWecom`（字段 `Enabled *bool` json `enabled`，`URL *string` json `url`）；`toConfig` 的 `checks` **不**把 wecom 列入必填。缺键填充规则的唯一行为表述在 spec「通知配置存储」兼容规则；组装对应为：

```
wecomEnabled, wecomURL = false, ""
if w.Channels.Wecom != nil {
    if w.Channels.Wecom.Enabled != nil { wecomEnabled = *Enabled }
    if w.Channels.Wecom.URL != nil { wecomURL = *URL }
}
```

`DefaultConfig` 显式含 `Wecom{Enabled:false, URL:""}`。`saveConfigFile` 仍 `json.MarshalIndent` 整个 `Config`，成功写入后磁盘必含 wecom 键。

Put 合并（对齐 bark token，`store.go:117-119`）：`incoming.Channels.Wecom.URL == "" || strings.Contains(URL, "***")` 时替换为 `prevStoredWecomURL(prev)`（prev 为 nil 或 `loadErr != nil` 时空串）。`enabled` 以本次解码值为准（spec「通知配置存储」兼容规则：对象缺失时两字段均取默认）。缺 wecom 键的 PUT 因此会关掉该渠道，但已存储 URL 经空串合并保留。新设置页总会提交 wecom。

`Store.Put` 是 PUT 路径唯一业务校验点（spec「通知配置读写 API」：业务校验 MUST 发生在 token/url 掩码合并之后）。顺序固定为写锁内「bark token 与 wecom url 掩码合并 → `merged.Validate()` → 原子写 → 快照替换」。Validate 失败 MUST 返回可识别的校验错误（包装类型或 sentinel，`Error()` 仍为内层校验文案，便于 422 原样回传）；`saveConfigFile` 失败保持普通 error。handler MUST NOT 在调用 `Put` 前再执行 `cfg.Validate()`：现有 `notification_config.go:94-96` 预校验会把 `url:"***"` 当非法 URL 打成 422，到不了合并。

`requiredConfigKeys` 测试列表增加 wecom 三键的「缺失不报 loadErr」反向用例，而不是把它们加进必填循环。

### D5: 掩码与日志红线

GET `channels.wecom.url_masked`：空串 → `""`；非空 → 固定 `***`。MUST NOT 复用 `MaskToken`（其 ≥8 位回显前 4 字符，会泄漏 `http`/`https` 前缀）。新增 `MaskWecomURL(url string) string`（与 `MaskToken` 同文件）。

`WecomChannel.Send` 的 `Result.Err` 只含判定要素，固定前缀 `wecom:`：

- `wecom: marshal request failed`
- `wecom: build request failed`
- `wecom: http request failed`
- `wecom: unexpected status <code>`
- `wecom: read response failed`
- `wecom: response body exceeds 64KiB limit`
- `wecom: invalid response JSON or missing errcode`
- `wecom: response errcode <n>`

禁止把 `cfg.Endpoint`、请求体、响应原文拼进 `Err`。`http.Client.Do` 的底层 error 常含 URL：一律丢弃 `%v`，用固定文案。dispatch 现有失败日志只打印 `res.Err`（`dispatch.go:158-159`），只要 Send 守这条红线，应用层不必特判 wecom。

### D6: markdown 渲染、截断与 HTTP 判定

渲染在 `WecomChannel.Send` 内完成，不改 `content.go`（Intent 保持渠道无关）。逐字模板见 spec「企业微信渠道」。实现：

```
content := "**" + in.Title + "**\n" + in.Body
if in.URL != "" {
    content += "\n\n[打开任务](" + in.URL + ")"
}
content = truncateUTF8Bytes(content, 4096)
```

`truncateUTF8Bytes`：从左扫描 rune，再追加将超过 4096 字节则停止；MUST NOT 截断到半个 UTF-8 序列。超限截断后仍投递。渠道 MUST NOT 插入 `<@all>` / `<@userid>`；MUST NOT 扫描剥离 Title/Body 中可能出现的同形片段（避免误伤提问原文）。

HTTP 客户端对齐 bark（复制，不抽取）：

- `Timeout: 10s`，`CheckRedirect` 返回 `http.ErrUseLastResponse`
- 单次 `Do`，不重试
- `Content-Type: application/json`
- 响应 `io.LimitReader(..., 64KiB+1)`，超限失败
- 成功：`status ∈ [200,300)` 且 JSON `errcode` 指针非 nil 且 `== 0`
- 非 2xx / 非法 JSON / 缺 `errcode` / `errcode != 0` → 失败
- 不解析、不匹配 `errmsg`

请求体：

```go
type wecomRequest struct {
    MsgType  string `json:"msgtype"`
    Markdown struct {
        Content string `json:"content"`
    } `json:"markdown"`
}
```

`msgtype` 字面量 `"markdown"`。响应体：

```go
type wecomResponse struct {
    ErrCode *int64 `json:"errcode"`
}
```

### D7: 设置页与 API DTO

API GET 在 `buildNotificationConfigDTOFromState` 增加 wecom：`Enabled` 原值，`URLMasked: notify.MaskWecomURL(...)`。PUT 仍走 `DecodeConfig`，wecom 字段名 `url`。handler 在 `DecodeConfig` 成功后直接 `notifyStore.Put(cfg)`，MUST NOT 再调用 `cfg.Validate()`。`Put` 返回可识别校验错误 → 422 `invalid_input` 且 body 为该错误 `Error()`；其余 `Put` 错误 → 500。沿用既有 `notificationConfigPutBodyMax = 4 << 10`：`<=4096` 继续解码，`>4096`（`MaxBytesReader` 读失败）→ 400 `invalid_input`，MUST NOT 进入解码/校验/写路径。

前端：

- `NotificationChannels.wecom: { enabled, url_masked }`（`url_masked` 必填；空 URL 用值 `""` 表达，不靠字段缺失）
- PUT `channels.wecom: { enabled, url }`
- 面板：复选框 + 密码输入（`type="password"` `autoComplete="new-password"`），placeholder 在已配置时为 `***（留空保持不变）`，保存提交 `url: wecomURL`（用户未改则为空串，走服务端合并）
- 保存成功后清空输入并刷新 `url_masked`

不改 SSE Intent、不改测试通知专用变体；测试路径经同一 `resolveOneChannel`，wecom 会出现在 `results[]`（`name=wecom`）。

### D8: 实现不分期，验证按落点

变更是单渠道插件，无 Fx、无迁移脚本、无触发器改动，**不分期、不设并行 lane**。实现按 D1 表自上而下一次做完。

验证（实现阶段必须覆盖）：

- `channels_test.go`：长度 4，顺序 `web,bark,macos,wecom`
- `config_test.go`：默认 wecom 关闭；合法 `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...`；http / userinfo / fragment 拒绝；错误信息不含 URL 原文
- `store_test.go`：三渠道旧 JSON 无 wecom → 无 loadErr 且 wecom 默认关闭；缺 bark 键仍 loadErr；Put 空串/`***` 保留 url；`MaskWecomURL` 非空为 `***`；wecom 对象缺失或为 `null` 均合法填充默认值；嵌套 `enabled`/`url` 各自 missing 与 null 均合法填充；wecom 在场但字段类型不匹配仍 loadErr
- `wecom_test.go`（httptest）：markdown 体与链接；URL 空省略链接行；content 恰好 4096 字节不截断且 POST，4097 字节截断至 ≤4096 有效 UTF-8 后仍 POST；errcode 0 成功；非 0 / 缺字段 / 非 2xx / 非法 JSON；响应恰好 64KiB 成功、64KiB+1 失败；不跟随重定向（第二次请求计数为 0）；默认 client 10s 超时 + `ErrUseLastResponse`；单次 Send 仅一次 `Do`；`Result.Err` 与日志不含 webhook URL
- `dispatch`：wecom 开关开但 url 空 → skipped 且零 HTTP
- API：GET `url_masked=="***"` 且响应无 URL 原文；测试通知含 `name=wecom`；已存 URL 后分别提交 `""`、`"***"`、`"prefix***suffix"` 均 200 且保留原值；其它业务字段非法仍 422；请求体 `>4096` 字节返回 400 且不写盘
- 前端：wecom URL 为密码输入；已配置时 placeholder 为 `***（留空保持不变）`；PUT 字段名为 `url`；保存成功后清空输入并刷新 `url_masked`

不要求真实打到 `qyapi.weixin.qq.com`。

## Risks / Trade-offs

- [旧设置页标签并发 PUT 不带 wecom 键会把 enabled 写成 false] → URL 经空串合并仍保留；新 UI 始终提交 wecom。接受。
- [net/http 错误默认含 URL] → D5 禁止 `%v` 包裹 Do/NewRequest 错误。
- [截断从左切可能切掉文末链接] → 与 spec「截断 content」一致；不另做链接保底。
- [厂商 20 条/分钟超限会失败] → 非目标，失败记日志不重试，与「全渠道失败不重试」一致。
- [用户粘贴非企微 URL] → 无 host 白名单；https 校验通过即保存，投递失败由 errcode/HTTP 暴露。
- [markdown 子集不含表格等] → 只用加粗与链接，落在官方子集内。

## Migration Plan

1. 部署后旧 `notification.json` 缺 wecom 键仍可加载，渠道默认关闭，行为与升级前一致。
2. 用户在通知子标签粘贴 webhook URL、启用 wecom、保存（磁盘补全 wecom 键）并用测试通知验证。
3. 回滚：关闭 wecom 开关或回退二进制；磁盘多出的 wecom 键被旧版 `DecodeConfig` 当未知字段忽略（旧版 `wireChannels` 无 wecom 字段且不 `DisallowUnknownFields`）。

## Open Questions

无。explore 已锁定渠道名 `wecom`、完整 URL、markdown+链接、不新增 @ 提及能力（行为见 spec「企业微信渠道」）、缺键默认关闭、非目标范围。
