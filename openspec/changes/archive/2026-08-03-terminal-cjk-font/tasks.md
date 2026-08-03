## 1. 偏好模块与默认字体栈

- [x] 1.1 新增 `web/src/terminal/preferences.ts`：导出 `TermPreferences` 类型、`DEFAULT_FONT_FAMILY`（含 CJK 回退链）、`DEFAULT_FONT_SIZE=13`、`FONT_FAMILY_KEY`/`FONT_SIZE_KEY`/`TERM_PREFS_CHANGED` 常量、`loadTermPrefs()`（损坏数据回退默认且不改写存储）、`resolveFontFamily/resolveFontSize`、`validateFontSize()`（整数 8–32，禁 parseInt）、`saveTermPrefs()`（先完整校验后写存储，空 fontFamily 删除 key，存储异常向上抛出）、`clearTermPrefs()`
- [x] 1.2 `web/src/terminal/session.ts`：`fontFamily`/`fontSize` 改为从 `preferences.ts` 导入并合并 `loadTermPrefs()` 解析值；新增 `applyPreferences(p: TermPreferences)`（就地设置 `term.options.fontFamily/fontSize`，下一帧 `requestAnimationFrame` 执行 fit 并同步 winsize）

## 2. 设置 UI

- [x] 2.1 新增 `web/src/components/TermAppearanceEditor.tsx`：fontFamily 文本输入（placeholder 显示默认栈）、fontSize 数字输入（留空=未设置）、保存 / 恢复默认按钮、CJK 提示文案；初始值取 `loadTermPrefs()`；保存前整体校验，成功后 `dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED))`，存储异常显示错误且不派发事件；恢复默认调 `clearTermPrefs()` 并派发事件
- [x] 2.2 「全局配置」页（`ConfigsPage.tsx`）在「全局环境变量」节之上挂载「终端外观」节（修订：原为工作台设置 tab，人工 review 反馈改为全局入口）；`styles.css` 补充样式（沿用现有 settings-section 风格）

## 3. 偏好变更生效

- [x] 3.1 `TerminalView` 监听 `TERM_PREFS_CHANGED`（同页）与 window `storage` 事件（跨标签页），两者均触发 `loadTermPrefs()` + `session.applyPreferences()`；组件卸载时移除监听

## 4. 后端 UTF-8 locale 默认值

- [x] 4.1 `internal/process/process.go` `DefaultBaseEnv`：宿主未设 LANG 且未设 LC_ALL/LC_CTYPE 时注入 `LANG=en_US.UTF-8`；宿主显式值原样透传；补单测（无 locale 注入默认 / 显式 LANG 透传 / 仅 LC_ALL 或 LC_CTYPE 时不注入）
- [x] 4.2 `internal/task/activate.go` env 快照基础集：同样规则补默认 LANG（保证 serve/TUI/shell 会话进程 UTF-8 locale）；补单测
- [x] 4.3（oracle 复核修订）`LC_ALL`/`LC_CTYPE` 加入 `DefaultBaseEnv` 与 `envBaselineKeys` 白名单透传非空值；注入/抑制判断改用非空语义（空串视为未设置）；测试断言高位变量原值透传 + 空串场景注入默认

## 5. 验证

- [x] 5.1 `cd web && npm run build`（含 `tsc --noEmit`）通过
- [x] 5.2 `go test ./internal/process/ ./internal/task/` 通过
- [x] 5.3 手动验证：重启服务后 attach 客户端 `client_utf8=1`；默认栈下中文正常渲染（不再显示 `_`）；保存自定义字体后当前页全部终端即时生效且 WS 不断、scrollback 保留；跨标签页即时生效；隐藏终端再次激活尺寸正确；非法字号被拒绝且不覆盖已有偏好；空白 fontFamily 回默认栈；恢复默认生效
