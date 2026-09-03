## 1. Domain 配置与 URL 校验

- [x] 1.1 `internal/domain/notification/config.go`：新增 `WecomChannelConfig{Enabled bool json:"enabled"; URL string json:"url"}`；`ChannelsConfig` 增加 `Wecom`（json `wecom`）；`DefaultConfig` 默认 `Enabled:false, URL:""`；`Validate` 在 bark/`base_url` 之后调用 `validateWecomURL`（D1/D3）
- [x] 1.2 同文件实现 `validateWecomURL`：空串合法；禁止复用 `validateOptionalURL`；https hierarchical、允许 query 与非根 path、禁止 userinfo/fragment；错误文案 `invalid wecom url: <reason>`，reason ∈ `invalid url` / `must be a hierarchical https URL` / `scheme must be https` / `host must not be empty` / `userinfo not allowed` / `fragment not allowed`；错误信息与 `url.Parse` 失败均不得含 URL 原文、不得 `%w` 包装（spec「企业微信渠道」URL 校验；D3）
- [x] 1.3 `internal/domain/notification/notification.go`：仅更新 `ChannelConfig` 注释——wecom 把完整 webhook URL 放入 `Endpoint`，`Token` 留空；不改接口（D2）
- [x] 1.4 `config_test.go`：默认 wecom 关闭且 `url=""`；合法 `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...` 通过；http / userinfo / fragment 拒绝；错误信息不含 URL 原文（D8）

## 2. Store 兼容、掩码与 Put 校验点

- [x] 2.1 `internal/infrastructure/notify/store.go`：`wireChannels` 增加 `Wecom *wireWecom`（`Enabled *bool` json `enabled`，`URL *string` json `url`）；`toConfig` 的必填 `checks` **不**列入 wecom；缺/null 按 spec「通知配置存储」兼容规则填充（对象缺失或 null → `enabled=false, url=""`；嵌套键各自独立默认）（D4）
- [x] 2.2 同文件：`Put` 写锁内顺序为「bark token 与 wecom url 掩码合并 → `merged.Validate()` → 原子写 → 快照替换」。wecom 合并条件：`URL == "" || strings.Contains(URL, "***")` → `prevStoredWecomURL`（prev 为 nil 或 `loadErr != nil` 时空串）。新增 `notify.ConfigValidationError`，`Put` 在 Validate 失败时返回 `*ConfigValidationError`（`Error()` 委托内层校验文案，并实现 `Unwrap()`）；`saveConfigFile` 失败保持普通 error。`Put` 为 PUT 路径唯一业务校验点（spec「通知配置读写 API」；D4）
- [x] 2.3 同文件新增 `MaskWecomURL`：空串 → `""`；非空 → 固定 `***`；MUST NOT 复用 `MaskToken`（D5）
- [x] 2.4 `store_test.go`：三渠道旧 JSON 无 wecom → 无 loadErr 且 wecom 默认关闭；缺 bark 键仍 loadErr；wecom 对象缺失或为 `null` 均合法填充默认值；嵌套 `enabled`/`url` 各自 missing 与 null 均合法填充；wecom 在场但字段类型不匹配仍 loadErr；Put 空串/`***` 保留 url；旧快照为 `loadErr` 时提交 `***`，Put 成功且 URL 为空；`MaskWecomURL` 非空为 `***`（D8）

## 3. WeCom 渠道适配器与注册

- [x] 3.1 新增 `internal/infrastructure/notify/wecom.go`：`Name()="wecom"`，`Caps()=0`；`Send` 按 spec「企业微信渠道」逐字模板渲染 markdown（Title 行与 Body 一个换行；有 URL 时 `\n\n[打开任务](url)`；空 URL 省略链接行含前导空行）；`truncateUTF8Bytes` 从左按 rune 截到 ≤4096 有效 UTF-8；不插入、不扫描剥离 @ 提及（D6）
- [x] 3.2 同文件 HTTP：复制 bark 客户端（10s、`CheckRedirect`→`http.ErrUseLastResponse`、单次 `Do`、不重试）；`cfg.Endpoint` 原样 POST，MUST NOT TrimRight / 拼接 path / 剥离 query；`Content-Type: application/json`；请求体 `wecomRequest{MsgType:"markdown", Markdown.Content}`；响应 `wecomResponse{ErrCode *int64}`；成功 = HTTP 2xx 且 `errcode` 非 nil 且 `==0`；64KiB+1 LimitReader；`Result.Err` 仅用 D5 固定 `wecom:` 前缀，禁止 `%v` 包裹含 URL 的底层错误（spec「企业微信渠道」；D2/D5/D6）
- [x] 3.3 `internal/infrastructure/notify/channels.go`：`BuildChannels` 追加 `NewWecomChannel()`，顺序 `web,bark,macos,wecom`（D1）
- [x] 3.4 `channels_test.go`：长度 4，顺序 `web,bark,macos,wecom`（D8）
- [x] 3.5 `wecom_test.go`（httptest）：markdown 体与链接；URL 空省略链接行；content 恰好 4096 字节不截断且 POST，4097 字节截断至 ≤4096 有效 UTF-8 后仍 POST；errcode 0 成功；非 0 / 缺字段 / 非 2xx / 非法 JSON；响应恰好 64KiB 成功、64KiB+1 失败；不跟随重定向（第二次请求计数为 0）；默认 client 10s 超时 + `ErrUseLastResponse`；单次 Send 仅一次 `Do`；`Result.Err` 与日志不含 webhook URL（D8）

## 4. Dispatch 解析

- [x] 4.1 `internal/application/notification/dispatch.go`：`resolveOneChannel` 增加 `case "wecom"`：`Enabled && URL != ""` 才配置成功；`Config.Endpoint = cfg.Channels.Wecom.URL`，`Token` 空；未配置 skipped 且零 HTTP。不改 `deliverParallel`、不改触发器（D1/D2）
- [x] 4.2 dispatch 测试：wecom 开关开但 url 空 → skipped 且零 HTTP；wecom 与 web 同时启用且已配置时各自投递（spec「通知渠道投递与降级」；D8）

## 5. API DTO 与 PUT 校验链

- [x] 5.1 `internal/api/notification_config.go`：GET DTO 增加 wecom `{enabled, url_masked}`（`url_masked` 必填，空用 `""`）；PUT 字段名 `url`；`buildNotificationConfigDTOFromState` 用 `MaskWecomURL`（spec「通知配置读写 API」；D7）
- [x] 5.2 同文件：`DecodeConfig` 成功后直接 `notifyStore.Put(cfg)`，删除 handler `cfg.Validate()` 预校验；`var validationErr *notify.ConfigValidationError`，`errors.As(err, &validationErr)` 判定 → 422 `invalid_input` 且 body 为该错误 `Error()`；其余 `Put` 错误 → 500。沿用 `notificationConfigPutBodyMax = 4 << 10`：`>4096` → 400 `invalid_input`，MUST NOT 进入解码/校验/写（spec「通知配置读写 API」；D4/D7）
- [x] 5.3 API 测试：GET `url_masked=="***"` 且响应无 URL 原文；测试通知 `results[]` 含 `name=wecom`；已存 URL 后分别提交 `""`、`"***"`、`"prefix***suffix"` 均 200 且保留原值；从未保存 URL 时提交 `***` 返回 200 且 `url_masked=""`；其它业务字段非法仍 422；请求体 `>4096` 字节返回 400 且不写盘（D8）

## 6. 前端设置页

- [x] 6.1 `web/src/types.ts`：`NotificationChannels.wecom: { enabled, url_masked }`（`url_masked` 必填）；PUT `{ enabled, url }`（D7）
- [x] 6.2 `web/src/components/NotificationConfigPanel.tsx`：四渠道；wecom 复选框 + 密码输入（`type="password"` `autoComplete="new-password"`）；已配置时 placeholder `***（留空保持不变）`；保存提交 `url: wecomURL`（未改则为空串）；保存成功后清空输入并刷新 `url_masked`（spec「通知设置界面」；D7/D8）
- [x] 6.3 `web/src/__tests__/notification-settings.test.tsx`：全部通知配置夹具补 `wecom`；断言 wecom 输入为 `type=password`、`autoComplete=new-password`、已配置 placeholder 为 `***（留空保持不变）`；断言 PUT 使用 `channels.wecom.url`；保存成功后输入清空并采用响应中的 `url_masked` 刷新 placeholder。同步更新受必填类型影响的夹具：`web/src/__tests__/App.notification-stream.test.tsx`、`web/src/__tests__/App.palette-config.test.tsx`（D8）

## 7. 构建与回归

- [x] 7.1 `openspec validate add-wecom-hook-channel --strict` 通过；`go test` 覆盖 `./internal/domain/notification` `./internal/infrastructure/notify` `./internal/application/notification` `./internal/api`；前端 `pnpm -C web test` 与 `pnpm -C web build` 均通过。不改触发器/门禁/LLM/web/bark/macos 行为。
