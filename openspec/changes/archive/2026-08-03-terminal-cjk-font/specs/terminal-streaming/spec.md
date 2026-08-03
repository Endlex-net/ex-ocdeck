## ADDED Requirements

### Requirement: 终端进程 UTF-8 locale
宿主无有效 locale 配置时，系统 SHALL 保证 tmux 命令（含 attach 客户端）与任务会话进程运行在 UTF-8 locale 下：LANG/LC_ALL/LC_CTYPE 均未设置或为空时，系统 MUST 注入默认 `LANG=en_US.UTF-8`，此时 tmux attach 客户端的 `client_utf8` flag MUST 为 1，CJK 输出 MUST NOT 被转写为 `_` 或其他替代符号。宿主显式设置的非空 locale 变量（LANG/LC_ALL/LC_CTYPE，含非 UTF-8 值）MUST 原样透传到子进程环境，不得覆盖、不得纠正；此场景下终端 UTF-8 行为以用户配置为准。空串值（如 `LANG=`）MUST 视为未设置。

#### Scenario: 宿主无 locale 时注入默认
- **WHEN** ocdeck-server 进程环境未设置 LANG/LC_ALL/LC_CTYPE（如 launchd 启动），创建会话或 attach 客户端
- **THEN** 进程环境含 `LANG=en_US.UTF-8`，attach 客户端 `client_utf8=1`，中文原样输出

#### Scenario: 宿主显式 locale 被尊重
- **WHEN** 宿主显式设置了非空 LANG（任意非空值，含非 UTF-8）
- **THEN** 系统透传该值，不注入默认值、不覆盖

#### Scenario: 高位 locale 变量存在时不注入且原样透传
- **WHEN** 宿主未设 LANG 但已设非空 LC_ALL 或 LC_CTYPE
- **THEN** 系统不注入 LANG 默认值，且该高位变量 MUST 原样出现在子进程环境中

#### Scenario: 空串 locale 视为未设置
- **WHEN** 宿主 LANG 为空串且 LC_ALL/LC_CTYPE 未设置或为空
- **THEN** 系统注入默认 `LANG=en_US.UTF-8`

### Requirement: 终端文本 CJK 渲染
系统 SHALL 为浏览器终端（xterm.js）配置包含 CJK 回退字体的默认字体栈：等宽拉丁字体（JetBrains Mono / SF Mono / ui-monospace / Menlo / Consolas）之后 MUST 追加常见 CJK 系统字体（至少含 PingFang SC、Noto Sans Mono CJK SC、Microsoft YaHei 中的回退声明），使中文及其他 CJK 字符在装有对应字体的浏览器环境中正常渲染，而非退化为替代符号。

#### Scenario: 默认字体栈渲染中文
- **WHEN** 浏览器环境装有任一常见 CJK 系统字体，终端输出包含中文
- **THEN** 中文以 CJK 字体正常渲染，占 2 列宽度，不显示为 `_` 或方块

#### Scenario: 拉丁字符渲染不受回退链影响
- **WHEN** 终端输出仅含 ASCII/拉丁字符
- **THEN** 仍由字体栈前部的等宽拉丁字体渲染，列宽度量与现状一致

### Requirement: 终端外观偏好
系统 SHALL 在「全局配置」页提供「终端外观」配置（该偏好为浏览器端全局偏好，不放在任务级设置入口），允许用户自定义终端 fontFamily 与 fontSize（整数，合法范围 8–32）。偏好 MUST 存于浏览器 localStorage（key：`ocdeck.terminal.fontFamily` / `ocdeck.terminal.fontSize`），对当前浏览器所有任务的 TUI 与 shell 终端生效；未设置时 MUST 使用含 CJK 回退的默认字体栈与默认字号 13。系统 MUST 提供「恢复默认」操作（清除 localStorage 对应项）。保存时 MUST 先完整校验两个字段，全部合法后才写入 localStorage；任一字段非法 MUST NOT 修改任何存储项并提示用户。fontFamily 去除首尾空白后为空视为未设置（删除对应存储项，回到默认栈），允许单独保存合法 fontSize。

#### Scenario: 保存自定义字体
- **WHEN** 用户输入自定义 fontFamily 与合法 fontSize 并保存
- **THEN** 偏好写入 localStorage，当前页所有已打开终端即时按新偏好渲染

#### Scenario: 未设置偏好
- **WHEN** localStorage 无终端外观偏好
- **THEN** 终端使用含 CJK 回退的默认字体栈与字号 13

#### Scenario: 恢复默认
- **WHEN** 用户点击「恢复默认」
- **THEN** localStorage 对应项被清除，终端回到默认字体栈与字号

#### Scenario: 非法字号
- **WHEN** 用户输入越界、非整数或非数字 fontSize 并保存
- **THEN** 系统拒绝保存并提示，localStorage 中已有偏好（含 fontFamily）不被修改

#### Scenario: 空白字体栈
- **WHEN** 用户将 fontFamily 清空或仅输入空白并保存（fontSize 合法）
- **THEN** fontFamily 存储项被删除回到默认栈，fontSize 偏好正常保存

#### Scenario: 损坏的持久化数据
- **WHEN** localStorage 中偏好数据损坏或非法
- **THEN** 终端按默认值渲染，且读取过程不得改写 localStorage

#### Scenario: 存储不可用
- **WHEN** localStorage 读写抛出异常（如 SecurityError / quota 超限）
- **THEN** 读取失败时终端按默认值正常可用；保存失败时不派发变更事件、已打开终端保持现状，并向用户显示错误

### Requirement: 偏好变更即时生效
偏好保存或清除成功后，系统 MUST 将变更即时应用到当前页所有已打开的终端实例（TUI 与 shell），并 MUST 应用到同源其他浏览器标签页中的终端实例。应用方式 MUST 为就地更新 xterm `fontFamily`/`fontSize` 选项并重新 fit/同步尺寸，MUST NOT 重建终端实例、MUST NOT 断开或重连 WebSocket，浏览器侧 scrollback 与选择状态 MUST 保留。

#### Scenario: 同页全部终端即时生效
- **WHEN** 用户保存新偏好且当前页有多个已打开终端
- **THEN** 所有终端实例 WS 连接保持不断，就地切换到新字体/字号，终端尺寸重新同步，scrollback 保留

#### Scenario: 跨标签页生效
- **WHEN** 用户在标签页 A 保存偏好，同源标签页 B 也开着终端
- **THEN** 标签页 B 的终端通过 storage 事件即时应用新偏好

#### Scenario: 隐藏终端后续激活
- **WHEN** 偏好变更时某终端处于隐藏（inactive）标签
- **THEN** 其字体选项就地更新，下次激活时由现有 fit 逻辑完成尺寸适配，无异常
