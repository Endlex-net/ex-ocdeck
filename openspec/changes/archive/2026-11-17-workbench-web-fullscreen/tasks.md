## 1. 后端配置模型与原子事务

- [x] 1.1 在 `internal/domain/palette/config.go` 增加 `WorkbenchFullscreenHotkey`、默认值 `mod+shift+f` 及与 TS 等价的 key + modifier-mask 交集校验；在 `config_test.go` 覆盖默认配置、mod 三 mask、冲突/非冲突、浏览器保留组合和侧栏冲突，运行 `go test ./internal/domain/palette` 通过。
- [x] 1.2 在 `internal/infrastructure/palette/store.go` 增加五键 diskConfig 与带 `WorkbenchFullscreenHotkeyPresent` 的 DecodedConfig，区分缺失、null、非字符串和空串；磁盘缺字段补默认，保留 commandTriggers 旧迁移，并在 `store_test.go` 覆盖旧文件正常迁移、默认键冲突整体降级、未知键和错误矩阵，运行 `go test ./internal/infrastructure/palette` 通过。
- [x] 1.3 实现 `Store.PutDecoded` 在同一写锁内补当前全屏热键、完整校验、0600 临时文件 + rename、替换快照并返回本次提交配置；保留完整配置 Put 的共用 commit 路径，增加 typed validation error 和可控交错测试，证明旧请求不会覆盖较新值且校验/写盘失败保旧，运行 `go test ./internal/infrastructure/palette` 通过。
- [x] 1.4 扩展 `internal/api/palette_config.go` GET/PUT DTO，PUT 使用 PutDecoded 并以其返回值写响应，按 validation/write 错误映射 422/500，保持 400、4KiB 和未知键契约；在 `palette_config_test.go` 覆盖五键 GET/PUT、旧四键省略字段、null/类型错误及响应为本次提交值，运行 `go test ./internal/api` 通过。

## 2. 前端热键配置链路（依赖第 1 组契约）

- [x] 2.1 在 `web/src/palette-focus.ts`、`PaletteConfigPanel.tsx` 与 `web/src/hotkey.ts` 增加五键类型、默认 `mod+shift+f` 和导出 `hotkeysConflict(a,b)`；在 `hotkey.test.ts` 使用与 Go 等价矩阵验证 mask 交集、完整修饰键匹配及默认展示，运行 `pnpm --dir web test -- src/__tests__/hotkey.test.ts` 通过。
- [x] 2.2 在 `PaletteConfigPanel` 增加独立“工作台全屏热键” draft、规范化和只读预览，对两个热键做单键及跨动作校验，本地失败不 PUT，成功提交并派发完整五键，失败不派事件；在 `palette-settings.test.tsx` 验证加载快照、成功/失败与新旧热键，运行 `pnpm --dir web test -- src/__tests__/palette-settings.test.tsx` 通过。
- [x] 2.3 更新 App 的 `isPaletteConfigDetail`、五键快照应用及 `TaskWorkbenchPage` 热键 prop，保持认证代际和后台重拉语义；在 `App.palette-config.test.tsx` 覆盖五键事件、保存即时生效、迟到 GET 不覆盖和旧值失效，运行 `pnpm --dir web test -- src/__tests__/App.palette-config.test.tsx` 通过。

## 3. 共享 compact 任务导航与悬浮详情（可与第 1 组并行）

- [x] 3.1 提取共享任务状态点/文案 helper 并实现 `CompactTaskNav`，以 `orderSidebarGroups()` 和 `useProjects()` 为唯一排序与数据入口，保留过渡态、attention 优先、当前任务、原生 href 和 `requestTerminalFocus`；在侧栏相关测试中验证 active/suspended/过渡态、排序、可访问名称和无新增数据请求，运行对应 Vitest 文件通过。
- [x] 3.2 实现 portal `TaskHoverCard`，展示任务名、项目、状态、待处理数量和当前任务标识；打开零延迟、A→B 即时替换、关闭仅 100ms 宽限，覆盖 trigger/card 进出、假时钟、任务消失、滚动、resize/blur、owner 隐藏和卸载清理，运行新增 `compact-task-nav.test.tsx` 通过。
- [x] 3.3 将 AppShell 桌面 collapsed 接入 CompactTaskNav，展开侧栏保持原渲染，compact 项移除原生 title 并补 `aria-current`、`aria-describedby` 和焦点样式；在 `sidebar-attention.test.tsx`、`sidebar-resize.test.tsx` 与导航焦点回归中验证两处状态语义一致、⌘B 偏好和任务导航不变，运行对应 Vitest 文件通过。
- [x] 3.4 为 compact 任务轨和悬浮详情添加 token 化 CSS：全局 collapsed 与 Zen 展开轨均为 44px、任务点命中区保持 32px，隐藏后无全高残轨且工作区全宽、仅保留左下小型展开按钮、列表独立滚动、非裁切浮层和正确 z 轴；通过组件断言与浏览器检查确认不遮挡 Banner/modal/命令面板，长任务列表和视口边缘可用。

## 4. Zen 状态、布局与热键集成（依赖第 2、3 组）

- [x] 4.1 在 `TaskWorkbenchPage` 实现 `zenRequested`、`taskRailVisible`、`workbenchPresentable` 与统一退出状态机，使用 layout effect 同步/清理独立 `body.od-workbench-zen`；新增稳定 `.workbench-main`，CSS 隐藏全局侧栏、resize handle 和页头但保留 Banner、告警及 pane 父节点；设置标签保持 Zen 与任务轨显隐，仅不可呈现统一退出。在工作台测试中验证进入/退出、设置保持、普通 error 和全局侧栏偏好不变，运行对应 Vitest 文件通过。
- [x] 4.2 接入 Zen 桌面任务轨及底部隐藏/左下小型展开按钮，隐藏后无全高残轨且工作区全宽；显隐跨 TUI/Shell/Git/设置保持、退出重置，≤767px 不渲染任务轨或独立展开按钮；验证隐藏/恢复焦点、任务切换不继承、767/768px 边界和离开/unmount 清理，运行新增 Zen 工作台测试通过。
- [x] 4.3 将 tabstrip 分为滚动标签区与固定操作格，普通 TUI/Shell/Git 提供带 `formatHotkey` 提示的 `zen`，已进入 Zen 后包括设置在内均提供 `out zen`；验证多 Shell、Git、设置、≤1024px 和移动端退出入口始终可见，不调用原生 Fullscreen API，运行 Zen 工作台与既有 tab 测试通过。
- [x] 4.4 按 design D3 实现 window capture 热键：IME guard → presentable/tab 资格 → matchHotkey → preventDefault + stopPropagation → repeat 消费但不切换；从终端 textarea 及 Git 控件派发事件，验证默认/自定义热键、单次切换、repeat、IME、裸 Esc、loading/404/设置不消费及卸载移除，运行 Zen 热键测试通过。
- [x] 4.5 补齐 Zen 中 SSE `onGone` 转 404 的统一退出测试，断言全局导航恢复、body class 与 portal 无残留；同时覆盖 loading 热键、普通流错误不退出、StrictMode 和路由离开，运行 Zen 生命周期测试通过。

## 5. 工作内容与尺寸回归

- [x] 5.1 验证 Zen 和任务轨显隐只触发终端 ResizeObserver → fit → WebSocket resize，不改变 TerminalView 的 key、active 或 wsPath；在终端适配与 Zen 测试中覆盖进入、退出、rail 显隐，无主动 disconnect/connect、scrollback 保留，运行相关 Vitest 文件通过。
- [x] 5.2 验证 Git flex 与 DiffViewer ResizeObserver 在 Zen/rail 尺寸变化后更新宽度，比例应用不进入编辑器重建 deps；运行 `git-panel-resize.test.tsx`、`diff-split-ratio.test.tsx` 和 Zen Git 测试，确认无重取、无写回、无 destroy、既有工作内容保留。

## 6. 全量验证与交付证据

- [x] 6.1 运行 `pnpm --dir web test` 与 `pnpm --dir web build`，记录 exit 0；若失败，修复本变更导致的回归后重跑。
- [x] 6.2 运行 `go test ./...` 和 `bash /Users/mendlex/workplace/opencode_ext/go-lint/lint.sh`，记录 exit 0；若本轮改动已提交到 HEAD，lint 改用 `--all`。
- [x] 6.3 用真实浏览器验收 TUI/Shell/Git 的 Zen 进入退出、终端焦点热键、IME/Esc、WebSocket/scrollback 连续、Git diff resize、任务悬浮详情、窄栏显隐、设置保持 Zen、ServerStatusBanner、多个 Shell、404 恢复及 767/768px 布局，保存关键检查结果。
- [x] 6.4 运行 `openspec validate workbench-web-fullscreen --strict`，核对两个 spec delta、design 和 tasks 的所有实现落点均有完成证据，无 design 外功能或新增依赖。
