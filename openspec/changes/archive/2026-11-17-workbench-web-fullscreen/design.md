## Context

需求动机与行为边界见 `proposal.md` 及两个 spec delta；本设计只定义实现机制。当前工作台由 `TaskWorkbenchPage` 持有 tab 与 visited 状态，TUI、Shell、已访问 Git 通过隐藏 pane 而不是卸载来保活（`web/src/pages/TaskWorkbenchPage.tsx:187-212,719-814`）。`App` 以 `key={taskID}` 重挂工作台，并持有唯一 palette 配置快照（`web/src/App.tsx:87-97,111-163,214-216`）。全局侧栏与 Zen 窄栏都可读取同一个 `useProjects()` 单例，不能新增 projects 数据流（`web/src/hooks.ts:296-365`）。

现有终端 host 由 `ResizeObserver` 触发 fit 与 WebSocket resize（`web/src/terminal/session.ts:256-257,656-674`），TerminalView 的 connect/disconnect 只由 `active/wsPath` 驱动（`web/src/terminal/TerminalView.tsx:375-384`）。因此 Zen 必须只改布局，不得把 Zen 状态加入终端 key、`active` 或 `wsPath`。

## Goals / Non-Goals

**Goals:**

- 以稳定 DOM 和页面本地状态实现 Zen，不重挂终端或 Git 工作区。
- 复用同一任务排序、状态呈现和悬浮详情，支撑全局折叠侧栏与 Zen 任务窄栏。
- 让配置热键在终端焦点内也可靠生效，同时保留 Esc、IME 和现有标签焦点契约。
- 在现有 palette 配置事务内完成新字段兼容、跨动作校验、原子写盘和运行时发布。

**Non-Goals:**

- 不引入通用快捷键框架、全局 tooltip 服务、Zen context 或第二条 projects 订阅。
- 不使用 fixed/portal 重挂工作区，不调用浏览器 Fullscreen API。
- 不新增 API 路由、任务实体、DDL、ticker 或终端 WebSocket 消息。
- 不改变展开侧栏、移动任务切换器、Git 内部文件栏的既有显隐语义。

## Decisions

### D1. Zen 由 TaskWorkbenchPage 本地持有，使用稳定 rail/content 布局

新增两个会话态：`zenRequested: boolean` 与 `taskRailVisible: boolean`（初始 `false` / `true`）。定义 `workbenchPresentable = task !== null && !notFound`，覆盖现有 loading/404 提前返回（`web/src/pages/TaskWorkbenchPage.tsx:413-434`）；`zenActive = zenRequested && workbenchPresentable`，不再以设置标签退出。工作台不可呈现时走统一退出流程：清除 `zenRequested`、重置 `taskRailVisible = true` 并撤销 body class，避免 404 残留隐藏导航。进入设置保持 `zenRequested`、Zen 布局和任务轨显隐；普通布局的设置标签不允许通过热键首次进入，已经进入 Zen 的设置标签中热键也不消费。普通流错误只要仍有 task 且非 notFound，就保留现有内容和 Zen，不得把 `error` 当作额外退出信号。任务切换与离页依赖现有工作台 unmount 清理，不持久化这两个状态。

根结构调整为始终存在的 `.workbench-main`；页头、告警、标签栏、pane-area 和现有模态均留在其中。任务轨只是 Zen 时新增的兄弟节点，pane 的父节点不随 Zen 改变：

```text
AppShell
+-- ServerStatusBanner
+-- od-content-fullbleed
    +-- workbench (normal: column / Zen desktop: row)
        +-- [TaskStatusRail, Zen desktop only]
        +-- workbench-main (always mounted)
            +-- page-header (Zen: CSS hidden)
            +-- alerts
            +-- tabstrip-scroll + fixed tabstrip-actions
            +-- pane-area (existing panes, no reparenting)
            +-- existing modals
```

使用独立 `body.od-workbench-zen` 标记隐藏 AppShell 的全局侧栏及其 resize handle；在 layout effect 中同步并在 unmount 清理，不能复用或写入 `od-side-collapsed`、`ocdeck:side-collapsed`、`ocdeck:sidebar-width`。ServerStatusBanner 仍在 `.od-main` 内、内容区之前，不受该标记影响（`web/src/components/AppShell.tsx:189-205`）。

**备选**：提升到 App/context 会增加路由和任务身份清理；fixed overlay 或 portal 工作区会破坏高度链并覆盖 Banner，均不采用。

### D2. 任务轨复用 compact task navigation，悬浮详情只做有限共享

新增 `CompactTaskNav` 与 `TaskHoverCard`（建议落点 `web/src/components/CompactTaskNav.tsx`、`web/src/components/TaskHoverCard.tsx`）。全局侧栏在桌面 collapsed 时和 Zen 桌面任务轨共用 `CompactTaskNav`；展开侧栏保留现有分组渲染。将 `agentDotClass` 从 AppShell 私有函数提取为共享任务呈现 helper，继续以 `orderSidebarGroups()` 为唯一排序/收录入口（`web/src/components/sidebar-order.ts:3-11,51-78`）。两处 compact 均省略可见项目标题及组间装饰，保留排序与悬浮详情中的项目名，不能把页头切换器的较小任务集合复制过来。

任务项仍为原生 `href="#/task/:id"`，点击继续调用 `requestTerminalFocus(taskID)`；当前项保留 `aria-current="page"`，两处均使用与原折叠侧栏一致的中性 `surface` 选中背景，不设置蓝色 current 边框或 accent 竖线；键盘 `focus-visible` 焦点环保留。链接必须有任务名 + 项目名的可访问名称，不能依赖 CSS 隐藏文本或原生 `title`。

`CompactTaskNav` 每个列表实例只维护一张当前 hover card，不建设全局管理器。卡片 portal 到 `document.body` 避免侧栏滚动裁切；保存 taskID、projectID 与 anchor，内容从本次渲染的最新 groups 解析，任务消失时关闭。两处 compact 任务项及其状态点不再设置原生 `title`。

悬浮详情固定为只读摘要：任务名、所属项目、当前状态、待处理数量（缺省为 0），当前任务额外显示“当前任务”。状态文案规则：过渡态使用 `transitionalLabel()`；suspended 显示“任务已挂起”；active 按等待人工 > busy > retry > idle > agent 状态未知排序展示。卡片 `role="tooltip"`，触发器以 `aria-describedby` 关联，不含按钮、不抢焦点、不新增 Esc 监听。

打开零延迟，A → B 立即替换；关闭允许 100ms 宽限，进入 trigger/card 任一区域取消旧 timer。列表滚动、窗口 resize/blur、页面隐藏、owner 隐藏或卸载、任务消失时立即关闭并清 timer。该宽限只用于跨越 trigger 与卡片间隙，不得形成打开延迟。

**尺寸与层级**：全局 collapsed 侧栏与 Zen 展开任务轨均为 44px，compact 任务点命中区保持 32px；隐藏后移除整条任务轨及边框，工作区恢复全宽，仅在左下角保留 28px 小型展开按钮，不保留全高残轨。展开按钮以小型浮层定位，不形成持续布局占位；其 z 轴低于 modal 与命令面板。隐藏/显示按钮均位于左下区域，键盘隐藏后焦点转移到展开按钮。任务列表独立纵向滚动，底部按钮固定。悬浮层高于普通 pane/菜单、低于 modal 与命令面板。

**备选**：复制两套任务点和文案容易漂移；通用 tooltip 事件总线超出本变更需求，均不采用。

### D3. 工作台热键使用 window capture，配置经 App props 下发

`App` 将 `paletteConfig.workbenchFullscreenHotkey` 作为 prop 传给 `TaskWorkbenchPage`，沿用现有配置快照，不在工作台新增 GET/context（`web/src/App.tsx:87-97,214-216`）。按钮提示使用 `formatHotkey()`。

`TaskWorkbenchPage` 注册 `window.addEventListener('keydown', handler, true)`，通过 ref 读取最新 hotkey、tab、工作台可呈现条件与 Zen 状态。处理顺序固定为：

1. IME guard：`isComposing`、key `Process`、keyCode 229 直接放行，不切换也不消费。
2. 页面资格：`workbenchPresentable` 且当前 tab 为 TUI、Shell 或 Git；loading、404、设置标签均不消费 Zen 热键。
3. 使用 `matchHotkey()` 精确匹配当前配置；不匹配直接放行，裸 Esc 不进入 Zen 分支。
4. 命中后执行 `preventDefault()` + `stopPropagation()`，保证组合键不会继续进入终端或其他页面动作。
5. `event.repeat` 仍消费但不再次 toggle；非 repeat 才切换 Zen。

选择 capture 是为了让工作台在下游终端监听器之前完成精确处理，不依赖终端事件是否继续传播；不为该能力修改 xterm 输入逻辑。跨动作配置校验保证 palette 与 Zen 不会响应同一实际组合，因此不需要 `stopImmediatePropagation()` 或动作优先级。

**备选**：App 统一监听需要额外的工作台动作注册；bubble + xterm 自定义桥接会形成两条输入路径，均不采用。

### D4. 前后端共享热键冲突语义，但不共享代码包

前端在 `web/src/hotkey.ts` 新增导出 `hotkeysConflict(a, b)`，内部复用私有 `expandMasks`：key token 相同且 mask 集合有交集即冲突。Go 在 `internal/domain/palette/config.go` 增加等价纯函数与 `Config.Validate()` 调用；`mod` 仍展开为 meta、ctrl、meta+ctrl。两端以同一测试矩阵验证，而不是引入跨语言生成器。

模型新增：

```text
Go Config.WorkbenchFullscreenHotkey string
TS PaletteConfig.workbenchFullscreenHotkey: string
DefaultWorkbenchFullscreenHotkey = "mod+shift+f"
DEFAULT_PALETTE_CONFIG.workbenchFullscreenHotkey = "mod+shift+f"
```

`PaletteConfigPanel` 为两个热键分别执行 `normalizeHotkey → validateCanonicalHotkey`，再执行 `hotkeysConflict`。任一失败不发 PUT；成功时发送完整五键，保存事件 detail 直接携带 PUT 200 返回的五键配置。App 的 `isPaletteConfigDetail` 同步要求新字段为字符串，现有认证代际、保存后重拉失败保留 PUT 值的机制不变（`web/src/App.tsx:60-78,111-163`）。

### D5. 旧客户端缺字段在 Store 写锁内补全，PUT 返回本次提交值

在 `internal/infrastructure/palette/store.go` 引入解码结果：

```go
type DecodedConfig struct {
    Config                           domainpalette.Config
    WorkbenchFullscreenHotkeyPresent bool
}
```

`DecodeConfig` 保留原有四键必填与精确 camelCase 规则；新字段缺失只设置 presence=false，显式 null、非字符串或空字符串分别按类型/业务错误拒绝。不能用 `*string` 同时表达缺失和 null。

新增 `Store.PutDecoded(incoming DecodedConfig) (domainpalette.Config, error)`，在同一个 `s.mu` 临界区执行：

```text
decode result
  --> lock
  --> if field absent: copy current snapshot's workbenchFullscreenHotkey
  --> validate complete five-field config
  --> saveConfigFile (0600 temp + rename)
  --> replace atomic snapshot
  --> return the exact committed config
  --> unlock
```

保留 `Store.Put(Config) error` 给完整配置调用方，内部复用同一 locked commit helper。Store 将 Validate 错误包装为可 `errors.As` 识别的 validation error；API 将其映射为 422，其他写盘错误为 500。HTTP handler 不再在锁外先 Validate，也不再于成功后重新 `Store.Config()` 组装 PUT 响应，而是直接用 `PutDecoded` 的返回值，避免并发写导致响应与本次提交不一致。

**不采用 handler 预读当前值后补全**：解码后到写锁之间可能发生另一次保存，会把旧全屏热键写回。也不采用新字段 PUT 必填、CAS/version 或通用 patch 协议。

### D6. 磁盘迁移与 API 响应分开处理

`decodeDiskConfig` 缺少新字段时补 `DefaultWorkbenchFullscreenHotkey`，再交给现有 `LoadStore → Config.Validate`；若与既有 palette hotkey 冲突，沿用既有“默认配置 + loadErr + 告警”降级路径（`internal/infrastructure/palette/store.go:57-77`）。缺 `commandTriggers` 的旧迁移顺序保持：先补 commandTriggers，再补全屏热键，最后整体验证。

`diskConfig` 与 HTTP DTO 增加 `workbenchFullscreenHotkey`。GET/PUT 200 总是五键；旧客户端 PUT 可缺新字段。`saveConfigFile` 继续现有 0600 临时文件 + rename，不因启动迁移主动改写旧文件（`internal/infrastructure/palette/store.go:233-268`）。

### D7. 尺寸变化依赖现有 observer，不新增重连或编辑器重建

Zen、任务轨显隐只改变容器尺寸。终端继续由 host ResizeObserver 调 `fitNow()`；不得手工 disconnect/connect。Git 主布局由 flex 自适应，DiffViewer 继续由容器 ResizeObserver 更新宽度，且比例应用不得加入编辑器重建 deps（`web/src/components/diff/DiffViewer.tsx:322-354`）。

tabstrip 拆成可滚动 `.tabstrip-scroll` 与固定 `.tabstrip-actions`：普通布局仅 TUI、Shell、Git 显示 `zen`；已经进入 Zen 后包括设置在内均显示 `out zen`，退出按钮始终位于固定操作格，避免现有 ≤1024px 横向滚动把入口滚出视口（`web/src/legacy-components.css:2113-2125`）。按钮可见文案精确为小写 `zen` / `out zen`，继续用 `formatHotkey()` 提供热键提示。≤767px 不渲染任务轨或独立展开按钮；768px 起恢复桌面任务轨。

## Protocol Changes

| 类别 | 变更前 | 变更后 |
|---|---|---|
| HTTP `GET /api/v1/palette/config` | 四键配置 | 五键配置，新增 `workbenchFullscreenHotkey` |
| HTTP `PUT /api/v1/palette/config` | 四键必填 | 原四键必填；新字段可省略，省略时锁内沿用当前值；200 始终返回本次提交的五键 |
| 前端配置保存事件 | detail 为四键 `PaletteConfig` | `PALETTE_CONFIG_CHANGED_EVENT` detail 为五键完整配置，不增加包层 |
| 持久化 `<dataDir>/palette.json` | 四键；旧三键有专用迁移 | 五键；旧文件缺新字段补默认并整体验证，不在加载时主动回写 |
| RPC / DDL / ticker | 无变更 | 无变更 |
| 终端 WebSocket | 现有 resize 与输入协议 | 无变更 |

## Domain Model Changes

仅扩展 palette 配置 VO，不改变任务、项目或终端实体：

```text
PaletteConfig
  hotkey: string
  workbenchFullscreenHotkey: string   [new]
  triggerWord: string
  matchMode: exact | exact-then-substring
  commandTriggers: exactly 8 existing entries
```

`DecodedConfig` 是 infrastructure 的 HTTP 解码 DTO，不进入领域 VO；presence 只用于兼容旧客户端，落盘和运行时配置始终是完整五键。

### UI 状态机

```text
Normal (zenRequested=false, rail=true)
  | button/configured hotkey on presentable TUI/Shell/Git
  v
Zen + rail (zenRequested=true, rail=true)
  | hide rail
  v
Zen + floating expand button (zenRequested=true, rail=false; no residual rail)
  | expand rail
  v
Zen + rail

Zen + rail         -- TUI/Shell/Git/Settings switch --> Zen + rail (self)
Zen + floating button -- TUI/Shell/Git/Settings switch --> Zen + floating button (self)
Either Zen state   -- TUI/Shell/Git hotkey or any-tab out zen --> Normal (rail reset)
Either Zen state   -- Settings hotkey --> stay unchanged, event not consumed
Either Zen state   -- task gone --> Normal + 404 (request/body class cleared)
Any Zen state      -- other task / route exit --> unmount --> Normal on next mount
Loading / 404 / Normal Settings -- Zen hotkey --> no action, event not consumed
```

状态生产唯一入口为 `TaskWorkbenchPage`；body class 是 `zenActive` 的派生副作用，不是第二事实源。设置标签保持 Zen，`notFound` 走统一退出流程，App 的 task key 重挂与 effect cleanup 是跨任务/离页重置路径。

## Business Flow Changes

### 进入、工作与退出 Zen

1. 用户在合资格标签点击固定操作格按钮，或按配置热键。
2. 工作台派生 `zenActive`，隐藏全局侧栏和页头；桌面默认展示任务轨，Banner、告警、标签与工作区保持。
3. 用户在 TUI/Shell/Git 间切换，pane 常驻与 rail 显隐保持。
4. 用户退出、任务转为 notFound、切换任务或离页，按状态机恢复普通布局；进入设置保持 Zen 与任务轨显隐，全局侧栏偏好不变。loading/404 不响应 Zen 热键，普通流错误不清除仍可呈现工作区的 Zen。

### 任务观察与切换

1. 全局折叠侧栏或 Zen 任务轨从共享 projects 快照生成同一排序的 compact items。
2. hover/focus 立即展示只读详情，store 更新使用最新快照，不新增请求。
3. 点击任务仍走原生导航 + `requestTerminalFocus`；不同 taskID 触发工作台重挂，Zen 与 rail 状态不继承。

### 配置保存与生效

1. 设置面板从 App 快照初始化两个热键 draft，本地规范化、单键校验和跨动作校验。
2. PUT 解码保留新字段 presence；Store 在写锁内补当前值、验证、原子写并返回本次提交配置。
3. 仅 PUT 200 后更新 draft 并派发完整五键事件；App 采用该值、推进代际并使新热键生效。
4. 校验/写盘失败不更新旧文件、快照、App 配置或事件；迟到 GET 仍受既有代际保护。

## Implementation Flow

### Phase 1：配置模型与事务（后端 lane）

- 扩展 Go Config、DTO、diskConfig 与默认值，新增跨动作冲突验证。
- 实现 DecodedConfig presence、磁盘迁移、`PutDecoded` 锁内补值和 typed validation error。
- API 改为返回 PutDecoded 的本次提交值，保持原错误矩阵与 4KiB 限制。
- 完成 domain/store/API 测试，特别是可控并发交错。

### Phase 2：共享任务呈现（前端独立 lane，可与 Phase 1 并行）

- 提取共享状态点/文案 helper，实现 CompactTaskNav 与 TaskHoverCard。
- AppShell 仅桌面 collapsed 接入 compact navigation，展开态保持原渲染。
- 覆盖排序、状态、即时 hover、关闭 timer、portal 清理、导航与 focus request。

### Phase 3：前端配置链路（依赖 Phase 1 契约）

- 扩展 TS PaletteConfig、默认值、hotkeysConflict、设置面板与 App 事件 guard。
- App 向工作台传递热键，覆盖保存即时生效、旧键失效和认证代际回归。

### Phase 4：Zen 集成（依赖 Phase 2/3）

- 在 TaskWorkbenchPage 加状态机、稳定 workbench-main、任务轨、固定 tabstrip actions 和 capture listener。
- 增加 body class、AppShell 隐藏规则与响应式 CSS；不改 panes、TerminalView 或 DiffViewer 生命周期。
- 完成标签、rail、快捷键、unmount、移动端与尺寸回归。

### Phase 5：端到端验证

- 前端 focused Vitest → 全量前端测试与 build。
- Go focused packages → `go test ./...`。
- 真实浏览器验证终端焦点热键、WebSocket/scrollback 连续、Git diff resize、悬浮层裁切、767/768px 与多 Shell 标签。

## Migration Plan

1. 启动读取旧 palette.json：缺新字段补默认；冲突时整体降级默认并记录告警，服务不中断。
2. 旧客户端继续以四键 PUT：服务端锁内沿用当前全屏热键，响应增加新字段。
3. 新客户端保存后写入五键；不在读取迁移时主动回写。
4. 回滚代码时，旧版本按未知附加键规则忽略磁盘中的新字段；既有四键仍可读取。若回滚期间旧客户端保存，新字段会按该旧版本行为处理，因此回滚应以恢复代码为主，不要求数据回滚。

## Risks / Trade-offs

- [capture 与终端输入顺序错误] → 仅在精确命中且合资格时消费；从终端 textarea 做真实事件测试，repeat/IME/Esc 单独回归。
- [body class 或 portal 卡片泄漏] → layout effect cleanup + owner/路由/断点/卸载关闭测试，包含 StrictMode、loading 热键与 Zen 中 SSE 转 404。
- [新增 wrapper 破坏终端或 Git 高度链] → `.workbench-main` 始终保持 flex/min-height/height 链，浏览器验证进入、退出和 rail 显隐的 resize。
- [任务状态文案分叉] → 共享 helper、排序与详情组件，覆盖 suspended/unknown/attention/transitional。
- [旧请求覆盖新全屏热键] → Store 写锁内补值；用可控交错测试，不依赖 `go test -race` 发现逻辑竞态。
- [PUT 返回并发另一次写入] → 使用 PutDecoded 返回的本次提交值，不再读取后的快照组响应。
- [100ms 关闭被误解为打开延迟] → 打开路径无 timer；假时钟分别验证即时打开与关闭宽限。
- [固定操作格挤压标签] → 标签滚动与操作格分离，验证 ≤767px、768–1024px、多 Shell 与 Git/设置切换。

## Verification Strategy

- **Domain/Store/API**：默认值、两端冲突矩阵、旧文件迁移、缺省/null/空串、原子失败、可控并发交错、GET/PUT 五键。
- **React 配置**：本地失败不 PUT、保存失败不派事件、保存成功五键即时生效、迟到 GET 不覆盖。
- **React Zen**：进入/退出、全部设置路径、任务/离页重置、rail 显隐、全局偏好不变、移动终端热键、repeat/IME/Esc、unmount cleanup；loading 中热键不消费，Zen 中 SSE 转 404 后导航恢复且无 body class/portal 残留，普通流错误不误退出。
- **任务详情**：两处同数据同文案、即时打开、100ms 关闭、任务消失/滚动/失焦清理、无额外 projects 请求。
- **尺寸与连续性**：终端 fit/WS resize、无主动重连、Git resize 不重建编辑会话。
- **构建**：前端全量测试与 `tsc --noEmit && vite build`，后端 `go test ./...`，并运行项目要求的中央 Go lint。

无开放问题。
