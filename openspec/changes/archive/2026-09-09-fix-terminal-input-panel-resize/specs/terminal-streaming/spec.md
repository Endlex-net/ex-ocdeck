# Delta Spec: terminal-streaming

## ADDED Requirements

### Requirement: Shift+Enter 换行输入

终端 SHALL 将 Shift+Enter 按键翻译为 opencode TUI 可识别的换行输入，而不是普通回车提交。xterm.js 对 Enter 与 Shift+Enter 均发送 `\r`（shift 修饰键在终端数据层丢失），浏览器侧 MUST 在键盘事件层拦截 Shift+Enter 并向终端输入流发送 TUI 可区分的换行序列；普通 Enter（无修饰键）行为 MUST 保持不变。IME 组合进行中（composition 或 IME process key 事件）MUST NOT 触发该拦截翻译。生效范围与迁移边界：修复对新建与重建的终端会话生效；修复发布前已在运行且未重建的存量会话可维持旧行为，任务重新激活（会话重建）后生效。

#### Scenario: Shift+Enter 插入换行

- **WHEN** 终端聚焦且非 IME 组合状态，用户按下 Shift+Enter
- **THEN** opencode TUI 收到换行输入（在输入框中插入换行），MUST NOT 提交发送消息

#### Scenario: 普通 Enter 提交行为不变

- **WHEN** 终端聚焦，用户按下无修饰键的 Enter
- **THEN** 行为与修复前一致（opencode TUI 收到普通回车）

#### Scenario: IME 组合中不拦截

- **WHEN** IME composition 进行中或收到 IME process key 事件，用户按下 Shift+Enter
- **THEN** 系统 MUST NOT 发送换行翻译序列，事件交由 IME 流程处理

### Requirement: IME 组合输入去重

终端 IME 输入补偿机制 SHALL 保证同一段用户输入只发送一次。交付前提：仅当输入门禁通过（已认证、连接可写、终端未锁定）时 IME 内容才可交付；终端锁定状态 MUST 保持零发送（锁定契约优先于恰好一次）。恰好一次的适用范围为可复现且可证明的路径：正常上屏 MUST 交付 composition 提交文本恰好一次；中文输入法 composition 进行中切换输入法等异常结束路径，已敲内容 MUST 按输入法实际 commit 的文本恰好交付一次，MUST NOT 出现片段重复（如输入 "nihao" 欲得「你好」，切换后终端不得收到 "hi haohihao" 类重复）；不可证明的候选 MUST fail-closed 丢弃（漏发优先于双发）。任何失败或不确定路径 MUST NOT 补发，MUST NOT 通过退格或重发整串修正已发送内容。

#### Scenario: 组合中途切换输入法不重复

- **WHEN** 用户使用中文输入法输入拼音（如 "nihao"）尚未上屏，直接切换到英文输入法导致 composition 结束
- **THEN** 终端最终收到输入法实际 commit 的文本恰好一份（如 "nihao"），无任何片段重复

#### Scenario: 正常上屏不重复

- **WHEN** 用户完成中文输入并正常上屏（compositionend 正常触发）
- **THEN** 上屏文本恰好发送一次
