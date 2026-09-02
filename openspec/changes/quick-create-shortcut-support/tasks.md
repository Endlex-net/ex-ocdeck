# Tasks: quick-create-shortcut-support

实施顺序遵循 design「实施顺序（分 lane）」：Lane A 冻结契约与纯函数 → Lane B（后端）与 Lane C（前端配置面板）并行 → Lane D（前端集成）→ Lane E（回归）。行为规范以 specs/ 各 delta 为唯一事实源，design 为机制与 why。

## 1. Lane A：共享契约与纯函数

- [x] 1.1 新增 `internal/domain/palette`：Go domain 为 `Config{ Hotkey, TriggerWord, MatchMode string }`（camelCase 仅属 wire DTO）、默认值（`mod+k` / `new` / `exact-then-substring`）与完整校验函数（hotkey 规范串 grammar + mask 冲突矩阵 + 有限拒绝表；triggerWord 空白集合 + 32 code point；matchMode 枚举），错误返回可读原因
- [x] 1.2 新增 `web/src/fuzzy-match.ts`：`foldForMatch`、`type MatchKind = 'exact' | 'prefix' | 'substring'`、`type MatchResult = { kind: MatchKind; index: number }`、`classifyMatch(name: string, query: string): MatchResult | null`（空查询返回 null，非空命中 index = fold 后 indexOf 下标）、`rankByQuery<T extends { name: string }>(items: readonly T[], query: string): T[]`（空查询返回全部按名称确定序；非空排除不命中；exact > prefix > substring > 位置 > 名称确定序 > 输入顺序兜底；不改输入数组）
- [x] 1.3 新增 `web/src/hotkey.ts`：`normalizeHotkey(raw): string | null`（仅语法规范化：分段/trim/lowercase/重排；空段或非修饰 token 数 ≠ 1 → null）、`validateCanonicalHotkey(canonical): string | null`（完整冲突矩阵）、`matchHotkey(e, canonical): boolean`（mask 精确匹配 + eventToken：字母 key.toLowerCase()、数字优先 event.code Digit0-9、Numpad 不匹配 digit）、`formatHotkey(canonical): string`（展示映射，含 mod 双平台形式）
- [x] 1.4 `web/src/palette-focus.ts` 类型扩展：`PaletteFocusId`、`PaletteFocusPayload = { projectName?: undefined; projectID?: undefined } | { projectName: string; projectID?: string }`、`PaletteFocusDetail = { id: PaletteFocusId } & PaletteFocusPayload`（扁平结构，MUST NOT 嵌套 `{id, payload}`）、`PaletteMatchMode = 'exact' | 'exact-then-substring'`、`type PaletteConfig = { hotkey: string; triggerWord: string; matchMode: PaletteMatchMode }`（事件 detail 直接为完整 `PaletteConfig`，MUST NOT 包一层 `{ config }`）、`od:palette-config-changed` 事件常量——本任务仅类型/常量，不改运行时行为
- [x] 1.5 Lane A 纯函数单测：fuzzy-match（档位优先级、fold 后名称确定序含前缀长度规则、`Afoo`/`aBar`、空查询例外、classifyMatch 空查询 null、index 语义、`İ` 长度变化、不修改输入数组、返回原 item 引用的新数组、非 ASCII UTF-16 顺序、正常/空查询/fallback 三处同一比较器、排序不受 matchMode 影响、顺序/反序输入一致性）、hotkey（normalize/validate/match/format 全矩阵含 `mod+t`/`mod+b`/`meta+shift+t`/`meta+shift+b`/`meta+ctrl+b`/`mod+shift+1`/Numpad/格式展示用例）——须验证在旧实现下失败、新实现下通过

## 2. Lane B：后端存储与 API（依赖 Lane A，可与 Lane C 并行）

- [x] 2.1 新增 `internal/infrastructure/palette/store.go`（平移 `notify/store.go` 模式）：`LoadStore(dataDir)`、`<dataDir>/palette.json` 0600 原子写、内存快照 + 写互斥、损坏/非法/配置文件不可读 → 降级默认 + 告警且不拒绝启动
- [x] 2.2 新增 `internal/api/palette_config.go`：`GET /api/v1/palette/config`、`PUT /api/v1/palette/config`；PUT 执行序（完整解码校验 → 临时文件写/关/rename → 快照替换）；错误矩阵：空体/纯空白/语法错误/尾随第二 JSON 值/>1024 字节 → 400 `invalid_input`，顶层非对象/三键缺失/null/类型错误/业务非法 → 422 `invalid_input`，写盘失败 → 500 `internal`；未知附加键忽略；1 KiB 上限不进入校验或写路径；失败不改旧文件/旧快照
- [x] 2.3 接线：`internal/api/server.go` 增加 `Server.paletteConfig` 字段、`SetPaletteConfigStore`、`registerPaletteConfigRoutes` 与注册调用；`cmd/ocdeck-server/main.go` LoadStore + 注入，保证在 RebuildRoutes 前完成
- [x] 2.4 Go 测试：store（不存在→默认、损坏→降级、配置文件不可读 → 降级默认 + 告警、合法 JSON 但字段非法的配置文件 → 启动降级默认配置 + 告警、原子写、落盘 JSON camelCase 三键精确键名、禁 PascalCase、0600 权限断言）、API（全错误矩阵含合法 JSON 补空白至恰好 1024 字节继续解码并成功（200）/ 1025 字节 400 且超限请求不进入校验/写盘、默认 GET 返回默认配置、PUT 后 GET 返回新配置、未知附加键成功忽略、PUT 成功返回当前生效配置、旧快照不变）、Go 表驱动 domain 校验测试（保留组合、侧栏 ⌘B 冲突、精确空白集合、code point 上限、matchMode 枚举，与 TS hotkey 矩阵同源）、`SetPaletteConfigStore + RebuildRoutes` 接线 smoke test——镜像 `notification_config_test.go` 模式

## 3. Lane C：前端配置面板（依赖 Lane A，可与 Lane B 并行）

- [x] 3.1 `web/src/api.ts` 增加 `getPaletteConfig` / `putPaletteConfig`（wire camelCase 三键）
- [x] 3.2 `web/src/router.ts`：`ConfigsTab` 与 `CONFIGS_TABS` 增加 `'palette'`
- [x] 3.3 新增 `web/src/components/PaletteConfigPanel.tsx`（镜像 NotificationConfigPanel 结构）：不独立 GET（接收 App 下发的 config/loadState/loadError，接口预留由 Lane D 接线）；热键文本录入（raw/canonical 展示）+ 输入框旁只读预览（formatHotkey）+ 前端预校验（normalizeHotkey + validateCanonicalHotkey，本地失败不调用 PUT）；触发词校验（原始字符串非空/空白集合/`Array.from` ≤32，不 trim 不规范化，本地失败不 PUT）；匹配模式二选 radio；加载中禁保存；保存失败展示后端错误原因；PUT 200 后以返回的完整 `PaletteConfig` 直接作为 `detail` 派发 `od:palette-config-changed`（MUST NOT 包 `{ config }`），保存失败 MUST NOT 派发
- [x] 3.4 `web/src/pages/SettingsPage.tsx`：TABS 增加「命令面板」条目与对应 tabpanel
- [x] 3.5 Lane C 组件测试：镜像 `notification-settings.test.tsx` 模式（mock `../api`）——`#panel-palette` 渲染、深链 `/configs#palette`、draft 初始化、加载中禁保存、triggerWord 边界（空串/NBSP/U+FEFF/32/33 code point）、热键规范化往返（加载后不修改直接保存；`K+Shift+Mod` → `mod+shift+k`）、PUT 200 成功派发 `od:palette-config-changed`（`detail` 直接为完整 `PaletteConfig`，MUST NOT 包 `{ config }`）、保存失败 MUST NOT 派发

## 4. Lane D：前端集成（依赖 Lane A + Lane C，单一 lane 串行）

- [x] 4.1 `web/src/palette-focus.ts` 运行时实现：`emitPaletteFocus(id, payload?)`、pending 存 payload、`consumePendingPaletteFocus` 返回 `PaletteFocusPayload | null`（匹配无 payload 返回 `{}`、无匹配 null）；pending 与实时事件共用同一 payload 归一函数（非法 `{projectID}` 归一为 `{}`）；迁移 `ProjectsManagePage` 的 `register-project-name` 消费方
- [x] 4.2 `web/src/App.tsx`：持有唯一配置快照 `{config, loadState, loadError}`；认证代际五条规则（authed=false 不 GET 用默认；首次挂载 authed=true 开新代际 GET；false→true 新代际 GET；true→false 失效+重置默认；旧代际不覆盖新代际）；`od:palette-config-changed` 监听（事件 detail 直接为完整 `PaletteConfig`，MUST NOT 包一层 `{ config }`；PUT 200 成功事件 MUST 原子执行：应用 `event.detail` 为当前配置、使在途 GET 代际失效、将 loadState 置为成功/ready 并清空 loadError；后台重拉仅最新代际写入、失败保留 PUT 值）；热键监听改用 `matchHotkey(e, config.hotkey)`；向 CommandPalette/CommandCenterPage/SettingsPage/AppShell 下发配置；导航并原样转发可选 `PaletteFocusPayload`
- [x] 4.3 `web/src/components/CommandPalette.tsx`：`parseQuickCreateQuery(query: string, triggerWord: string): { projectQuery: string } | null`（triggerWord 大小写不敏感字面前缀、空白集合、`+ 空白` 即进入模式、余文整段 trim 保留内部空白）；快速新建模式渲染（置顶「新建任务」始终可执行 + 候选列表读 `useProjects()` 共享快照、候选副文案展示项目路径、选中候选携带 `projectID + projectName`、零命中 fallback 展示全部项目按名称确定序、首次无快照仅置顶命令）；自由文本 Enter 只传 `{ projectName }`、空余文（`new ` 后空白）必须传 `{ projectName: '' }`；静态入口增加「设置 · 命令面板」（7 项）；触发词/匹配模式来自 App 下发配置
- [x] 4.4 `web/src/pages/CommandCenterPage.tsx` + `NewTaskPanel`：消费信号（有效 projectID 直接选中 → 失效回退文本匹配（按 matchMode）→ 失败填过滤词；非法 detail 归一为 `{}` 按普通 `new`；只按信号到达时快照判定不自动重试）；父层递增 nonce，新 nonce 应用项目状态并聚焦任务名；快速新建仅替换项目相关状态、保留 taskName；空字符串 payload `{ projectName: '' }` 清空项目选择/过滤词但保留 taskName，与无 payload（保持全部表单状态）区分；无 payload 只展开聚焦；不绕过提交门禁
- [x] 4.5 `web/src/components/AppShell.tsx`：仅消费 App 下发的配置 prop，替换 :135/:139 两处 ⌘K 文案（title 与可见徽标）为 `formatHotkey(config.hotkey)` 展示文本
- [x] 4.6 Lane D 组件测试：CommandPalette 触发词模式（置顶命令、候选排序、Enter 执行、候选点击携带 projectID、零命中 fallback、仅触发词不进入模式、含空格项目名、空余文展示全部项目候选且 MUST NOT 自动预选、同名项目候选 projectID 传递）、独立 `parseQuickCreateQuery` 纯函数单测（非触发词前缀、字面前缀非正则、大小写不敏感、`triggerWord + 空白` 空余文、空白集合边界、余文保留内部空格、`İ` fold 长度变化下原始 UTF-16 切片不越界）、NewTaskPanel 初始化（预选/不预选填过滤词/门禁不绕过/连续 nonce/保留 taskName/空字符串 payload `{ projectName: '' }` 清空项目选择/过滤词但保留 taskName/无 payload 保持全部表单状态/非法 detail）、palette-focus 双通路（实时事件 + pending 跨路由）、register-project-name 回归、App 配置代际（TokenGate 认证后加载、401 后重新认证重载、deferred 乱序、保存后重拉失败保留 PUT 值、GET 失败→PUT 成功清除提示、App 首次挂载且 `authed=true` 时 MUST 开启新代际并发起 palette config GET）、`od:palette-config-changed` 成功派发（`detail` 直接为完整 `PaletteConfig`，MUST NOT 包 `{ config }`）与保存失败 MUST NOT 派发

## 5. Lane E：回归与跨层验证（依赖 Lane B + Lane D）

- [x] 5.1 全量前端测试（vitest）通过；Go scoped 测试通过：`go test ./internal/domain/palette/... ./internal/infrastructure/palette/...` 与 `go test ./internal/api/ -run 'TestPalette|TestSetPalette'`（internal/api 全量中另有 4 个 git 相关测试因 fixture `git init` 未带 `-b` 生成 master、测试期待 main 而失败，与本 change 改动集无交集）；新增行为测试逐一验证「旧实现失败、新实现通过」（mutation 式自检：临时注释放行分支确认测试变红）
- [ ] 5.2 手动冒烟：⌘K → `new <项目名>` Enter → 面板预选项目 + 光标落任务名；`new zzzz`（零命中）→ 全部项目候选 + Enter 打开面板不预选；设置页改触发词/热键/匹配模式 → 立即生效；`#/configs#palette` 深链直达
- [x] 5.3 回归：现有 `new`（无参数）行为不变；⌘B 侧栏折叠不冲突；既有 palette-focus 链路（register-project-name）正常；路由深链既有行为不变
