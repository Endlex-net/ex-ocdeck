# Delta Spec: terminal-streaming

## ADDED Requirements

### Requirement: 终端链接修饰键点击打开

终端 SHALL 识别输出中的链接并支持修饰键点击打开：纯文本 `http(s)://` URL 与程序输出的 OSC 8 超链接均 MUST 可识别；Cmd+点击（macOS）或 Ctrl+点击（其他平台）MUST 在用户手势中同步请求当前浏览器以新标签页/窗口打开该链接（具体呈现由浏览器决定；被阻止时 MUST NOT 异步重试、MUST NOT 转为当前页导航、MUST NOT 影响终端会话）；不带修饰键的普通点击 MUST NOT 触发打开。终端锁定状态（见「触屏设备终端输入锁定」）下 overlay 拦截全部 pointer 事件，链接**不可点击**；解锁后可点击。修饰键点击链接 MUST NOT 产生键盘输入数据（鼠标报告行为按 xterm 既有语义，见 design D1）。链接打开 MUST NOT 影响终端会话、输入门禁与既有键盘/IME 行为。

#### Scenario: Cmd+点击打开纯文本 URL

- **WHEN** 终端解锁且输出包含纯文本 `https://example.com/path`，用户 Cmd+点击该 URL 文本
- **THEN** 请求当前浏览器以新标签页/窗口打开该 URL，终端无键盘输入数据产生

#### Scenario: 普通点击不触发

- **WHEN** 用户不带修饰键点击终端中的链接文本
- **THEN** 不打开浏览器，保持既有点击/选择行为

#### Scenario: OSC 8 超链接可点击

- **WHEN** 终端解锁且程序输出 OSC 8 超链接（如 opencode markdown 链接），用户 Cmd+点击链接文本
- **THEN** 请求当前浏览器以新标签页/窗口打开链接目标 URL

### Requirement: Emoji grapheme cluster 渲染

终端 SHALL 正确渲染 emoji：**字形覆盖（彩色优先）**——终端字体栈 MUST 包含系统 emoji 字体回退（`Apple Color Emoji` / `Segoe UI Emoji` / `Noto Color Emoji`）；在字体可用、字体支持目标序列且浏览器支持彩色渲染时：默认 emoji 呈现字符（`Emoji_Presentation=Yes`）、含 VS16 的 emoji 呈现序列、有效 ZWJ/旗帜组合序列 MUST 渲染为可见彩色图形而非豆腐块；默认文本呈现且未请求 emoji 呈现的字符（如裸 ❤、©）以前序正文字体为准（对默认栈与自定义栈同样适用）；emoji 码点被用户自定义栈中字体覆盖时以用户字体为准。**宽度与分段**——组合字符按 grapheme cluster 计算：ZWJ 组合 emoji（如 👨‍👩‍👧）、肤色修饰符、旗帜 emoji、组合附加符号 MUST 作为单个簇渲染与占位，MUST NOT 拆散为多个字符或产生宽度错位。普通 ASCII/CJK 文本的既有宽度行为 MUST NOT 改变。emoji 系统族名为声明式引用（不下载不打包），其缺失平台（无对应系统字体）降级为豆腐块、不影响终端其余功能。

#### Scenario: 组合 emoji 单簇渲染

- **WHEN** 终端程序输出 ZWJ 组合 emoji 序列（如 👨‍👩‍👧），且系统 emoji 字体可用、支持该序列、浏览器支持彩色渲染
- **THEN** 终端将其作为单个 grapheme cluster 渲染为可见彩色图形，光标与后续字符按单簇宽度推进

#### Scenario: 连续 emoji 不粘连

- **WHEN** 终端程序输出连续多个 emoji（如 😀😀😀，或 ZWJ 序列与普通 emoji 混合）
- **THEN** 每个 emoji 独立占据正确的单元格宽度，互不重叠/粘连（用户实证：修复前 TUI 中连续 emoji 粘在一起）

#### Scenario: 普通文本宽度不变

- **WHEN** 终端输出纯 ASCII 或 CJK 文本
- **THEN** 宽度计算与换行行为与修复前一致

### Requirement: Nerd Font 图标字形内置

终端 SHALL 内置图标字体回退，使 Nerd Font 私有区（PUA）字形（文件夹、分支、提示符符号等）无需用户本地安装字体即可渲染。图标字体 MUST 追加在有效字体栈的**绝对末尾**（含 generic 字体之后），且其 `@font-face` 声明 MUST 以 `unicode-range` 限定为 Nerd Font 图标码点范围：正文与 CJK 字符的字体选择（含用户自定义字体偏好，见终端外观偏好）MUST NOT 被改变，仅当栈中所有字体均不含某字形且码点落在约定图标范围时才使用图标字体。该变换只作用于运行时有效栈，MUST NOT 写回用户偏好存储。

#### Scenario: shell 主题图标正常显示

- **WHEN** 终端程序输出 Nerd Font PUA 图标（如 starship 主题的文件夹/分支图标）
- **THEN** 图标以图标字体渲染为可见图形，不显示为豆腐块（□）

#### Scenario: 用户自定义字体优先级保持

- **WHEN** 用户已设置自定义终端字体偏好
- **THEN** 正文/CJK 字符仍按用户字体渲染，仅用户字体栈缺失的图标字形走内置回退
