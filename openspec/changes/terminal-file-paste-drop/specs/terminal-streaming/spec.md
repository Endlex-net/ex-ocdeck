# terminal-streaming Delta Spec

## MODIFIED Requirements

### Requirement: WebSocket 协议与认证

终端 WebSocket SHALL 使用二进制帧双向传输终端 IO，JSON 控制帧承载 auth/resize 与附件投递控制消息。首条消息 MUST 为 `{"type":"auth","token":"...","cols":N,"rows":M}`（认证与初始尺寸握手合一，5 秒超时，认证成功前不订阅 PTY），token MUST NOT 通过 query 参数传递。认证成功帧 MUST 为 `{"type":"auth_ok","connId":"<uuid>"}`，connId 为 server 为该连接签发的连接 ID（上传与投递授权绑定的依据）。系统 MUST 校验 Origin、限制帧大小上限、使用原生 ping/pong。关闭码语义：4001 未认证、4009 被新连接替换、4010 任务已挂起、1011 服务端内部错误。

附件投递控制帧：client→server 为文本帧 `{"type":"deliver","uploadId":"<32 hex>"}`；server→client 为文本帧 `{"type":"deliver_result","uploadId":"<32 hex>","ok":true}`，失败时 `"ok":false` 且携带 `"error"`，error 取值枚举：`not_found`（uploadId 不存在或物理文件缺失）|`expired`（已过保留期）|`forbidden`（task/connId/终端类型不匹配，含 shell 连接）|`task_inactive`（任务挂起/删除）|`write_failed`（PTY 写失败，含部分写入）|`invalid_input`（合法 deliver 请求的投递内容校验失败）。

服务端 MUST 按类型显式解析控制帧，不再回落写 PTY。控制帧分发分支规则：① 非法 JSON 或未知 type：丢弃 + 日志，不回帧、不调用 onInput；② `type=resize`：沿用既有尺寸处理，不校验 uploadId；③ 仅当 `type=deliver` 时才要求 uploadId 为合法 32 hex，不合法则丢弃（+日志、无回帧、无 onInput），合法才进入投递业务校验及活动上报（投递业务校验的唯一优先级见 `terminal-file-delivery` spec「连接归属与投递准入」）。deliver_result MUST 经既有有界 WS 写队列发出（见「输出削峰与背压」），deliver 处理路径 MUST NOT 直接阻塞写 WS。合法 deliver 帧（uploadId 为 32 hex）MUST 视为用户活动触发 onInput 上报。客户端 MUST 仅在确认服务端支持附件投递（上传接口可用）后发送 deliver 帧，MUST NOT 向未确认能力的服务端发送。

#### Scenario: 未认证连接

- **WHEN** 连接在超时内未完成首消息认证
- **THEN** 连接以 4001 关闭，未收到任何 PTY 数据

#### Scenario: 初始尺寸握手

- **WHEN** 客户端认证消息携带 cols/rows
- **THEN** 服务端以该尺寸创建 attach 客户端 PTY，tmux 将会话窗口调整到客户端尺寸，画面无尺寸跳变

#### Scenario: 认证成功帧携带 connId

- **WHEN** 客户端完成 auth 握手
- **THEN** 收到携带本连接 connId 的 auth_ok 帧，后续上传与投递以该 connId 绑定授权

#### Scenario: 未识别或畸形控制帧零 PTY 写入

- **WHEN** 服务端收到类型未识别的 JSON 控制帧、非法 JSON 文本帧，或 uploadId 缺失/格式不合法（非 32 hex）的 deliver 帧
- **THEN** 帧被丢弃并记录日志，PTY 零写入、不回帧、不触发 onInput，终端画面无异常文本

#### Scenario: deliver 投递成功回执

- **WHEN** 服务端收到合法 deliver 帧且归属校验与注入均成功
- **THEN** 经有界写队列回 `{"type":"deliver_result","uploadId":"...","ok":true}`，deliver 处理路径未直接阻塞写 WS

#### Scenario: shell 连接 deliver 被拒绝

- **WHEN** shell 终端的 WS 连接收到合法 deliver 帧（uploadId 为 32 hex）
- **THEN** 回 `deliver_result` `ok:false` 且 error 为 `forbidden`，PTY 无任何写入

#### Scenario: 新客户端对旧服务端不发送 deliver

- **WHEN** 新版前端连接旧版 server：auth_ok 缺失或 connId 非法，或 connId 有效但上传接口返回 404
- **THEN** 前者不发起任何 HTTP 上传，提示"当前 server 版本不支持文件投递"；后者上传终止于 404，提示"上传目标不可用，任务可能已删除或服务端不支持该接口"——两条路径均未发送任何 deliver 帧，普通终端功能不受影响
