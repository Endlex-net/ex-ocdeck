# Tasks: web-ui-redesign

依赖图（同一层级可并行，跨层按序）：

```
L0: 1.设计源固化            L1: 2.样式基底 ║ 3.后端注意力信号（可并行）
        │                       │
        └───────────▶ 4.全局壳层（依赖 1+2+3.5 的 projects 摘要）
                            │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
        5.指挥中心      6.项目管理      7.设置（可并行，均依赖 4）
              └─────────────┴─────────────┘
                            ▼
              8.工作台适配 + 旧代码删除 + 收尾验证（最后）
```

L3 并行 lane 所有权（避免共享文件冲突）：第 4 组完成时**冻结**共享契约——路由表与解析函数、`types.ts` 新增类型、api.ts 新增方法、共享 store hook、AppShell 插槽签名；lane 5/6/7 各拥有**独占文件集**（各自页面目录 + 各自页面级 CSS 文件 `web/src/pages/<page>.css`，共享设计系统样式表归第 2 组独占，页面样式只新增文件不交叉修改），契约层变更只能回第 4 组补丁；第 8 组兼任最终集成 lane（App.tsx 接线、旧代码删除、跨页一致性）。路由/theme/store 纯函数测试归第 4 组（谁建契约谁带测试）。

## 1. 设计源固化（L0）

- [x] 1.1 复制设计交付物入库：根目录 `brand-spec.md`；`docs/design/assets/`（`ocdeck-ui.css`、`ocdeck-palette.js`、`ocdeck-theme.js`、`ocdeck-sidebar.js`）（来源：Open Design 项目 2396c3bb-e795-41fb-9536-723f69c71734），文件头注明来源与固化日期
- [x] 1.2 vendor 时收编 `ocdeck-ui.css` 冲突项（`color:#fff` → `var(--on-ink)`；暗色 6 token + `--term-bg` 对齐 brand-spec；触屏目标 ≥44px 三处修正），文件头注明差异
- [x] 1.3 复制 4 个 canonical 页面稿入库 `docs/design/screens/`（command-center / task-workbench / projects / configs），文件头注明布局/状态/交互规格效力与"mock 数据非契约"

## 2. 样式基底与设计系统（L1，可与 3 并行）

- [x] 2.1 以 `docs/design/assets/ocdeck-ui.css` 为基底创建 `web/src/` 设计系统样式（token + 组件类），从 `main.tsx` 接入；新旧样式共存期内设计系统优先，旧 `styles.css` 仅在本组内保留，删除见 8.5
- [x] 2.2 硬编码色值审计：收编现有组件中 token 之外的 hex 色值为 token 派生（`color-mix(in oklch)`）
- [x] 2.3 文本字符图标（▶ ⏸ ⟳ ⋯ 等）替换为内联 SVG 图标组件（1.6px 描边 `currentColor`），不引图标库
- [x] 2.4 `index.html` 内联同步主题脚本（首帧防闪烁，`data-theme` + localStorage `od-theme`）

## 3. 后端注意力信号（L1，可与 2 并行）

- [x] 3.1 `internal/opencode/client.go`：SSE 事件解析扩展——`permission.asked/replied`、`question.asked/replied/rejected` + v2 家族同形处理；按 payload `type` 分派；未知/缺字段事件与未知 requestID 的 replied/rejected 静默忽略
- [x] 3.2 `internal/opencode`：`OCClient` 接口（internal/task/types.go:75-88）新增 `ListPermissions(ctx, dir)` / `ListQuestions(ctx, dir)` 并在 client.go 实现；404 映射为可导出类型化错误（`errors.Is` 可判）；全部 mocks/wrappers（含 internal/task/mock_test.go）同步扩展
- [x] 3.2b 三层类型：opencode 包内完成 `Event.Properties` map → typed attention 事件解析（task 层不消费非类型化 map）；opencode 输出不含 Since 的规范化类型，runtime 本地状态附加 Since（本地首次观察时间），api 独立 DTO
- [x] 3.3 task runtime：任务锁内 pending 集合（asked 登记/replied|rejected 移除，枚举值不校验一律了结）；只读快照方法返回拷贝；挂起/删除清空；**代际与并发模型**——独立 `attentionEpoch`（atomic，不动现有 activation generation），挂起/删除先推进再清空；`attentionMu` + per-type reconciling owner/增量缓冲（REST 锁外、写回锁内：epoch+代际校验→替换→重放；仅 owner 清标记）；teardown `context.Canceled` 中性（不迁 degraded、不写回）；注意力对账独立于 session align，任何失败不影响任务状态机
- [x] 3.4 能力状态机（per 任务 × per 类型）：unknown→available/unsupported/degraded（unknown+非404 也迁 degraded）；404→unsupported 停止对账、忽略该类型 SSE、透出空数组；非 404→degraded 透出旧快照+SSE 增量、继续重试；两类型独立迁移
- [x] 3.5 API 透出：`GET /api/v1/tasks/{id}` 附加 `attention`（since=本地首次观察时间 Unix 秒，对账同 ID 保留原值新 ID 取对账时刻；空数组非 null）；`GET /api/v1/sessions/active` 元素附加 `attention`；`GET /api/v1/projects` 附加 tasks 摘要（id/name/status/init_status/branch/worktree_path/last_error/notice/updated_at/agentStatus/attention_count）
- [x] 3.6 后端测试（`go test -race`）：10 种事件 fixture 解析 + REST pending 响应 fixture（正常/null/非数组/坏元素 → 整体失败）；pending 生命周期（登记/移除/未知 ID 忽略/原子替换/挂起清空）；能力状态机全迁移路径（含运行期 404、degraded 30s 周期重试恢复、teardown context.Canceled 中性不迁移）；**挂起-对账竞态**（在途对账遇挂起代际推进 → 写回被拒、集合保持清空）；API 透出字段测试（attention 结构/since 本地首次观察时间/空数组非 null、projects 摘要 10 存储字段 + 1 水合字段/agentStatus 降级）；SSE 写入 × API 快照读取并发；**确定性并发测试：周期对账 REST 在途期间到达的 asked/replied 事件不被旧快照覆盖（两条路径各一）；后台 REST 在途时发生 SSE 重连 → align 路径新对账抢占、旧结果被拒（reconcile epoch）；**被抢占旧对账返回后不触碰新 owner 的 reconciling 标记与增量缓冲**；**接管时旧缓冲归并至旧集合并清空，新快照只重放新 owner 观察到的增量（断连期间已回复的请求不得复活）**；非404失败时增量缓冲重放到保留集合**

## 4. 全局壳层（L2，依赖 1+2+3.5）

- [x] 4.1 `AppShell` 组件：侧栏（品牌区/指挥中心顶层项/任务组/管理组/底栏）+ 内容区；未认证只渲染 TokenGate；ServerStatusBanner 壳层内全页面可见
- [x] 4.2 App 层共享 projects 轮询 store（5s single-flight）：侧栏、指挥中心与项目管理页同一数据源；暴露 single-flight `refresh()` 供变更操作后调用；侧栏轮询失败保留旧数据静默展示
- [x] 4.3 侧栏任务组：项目分组、agent 状态点、注意力标记；点击导航 `#/task/:id`；归档任务不显示
- [x] 4.4 侧栏折叠：⌘B + 按钮，图标轨形态，localStorage `ocdeck:side-collapsed` 持久化
- [x] 4.5 主题系统：`useTheme` hook（`system|light|dark`，跟随系统默认）
- [x] 4.6 ⌘K 命令面板：按 `docs/design/assets/ocdeck-palette.js` 行为规格自实现（注册表同源的 6 个静态入口：指挥中心/项目管理/设置四子标签深链 + 任务列表 + 新建任务/注册项目操作；模糊匹配含中文；↑↓/Enter/Esc；⌘K/Ctrl+K）；匹配器抽纯函数
- [x] 4.7 路由收敛与重定向：`#/`→指挥中心、`#/projects`、`#/configs`、`#/task/:id` 保留；`#/active`→`#/`、`#/ai-config`→`#/configs#ai`、`#/project/:id`→`#/projects#<id>`；非法深链/tab 回退（不选中/回 appearance）；路由解析抽纯函数
- [x] 4.8 TokenGate 与 ServerStatusBanner 适配新壳层视觉
- [x] 4.9 契约层纯函数测试（第 4 组自建自测）：路由解析/重定向映射、`?from` 归一映射、主题 resolver、命令面板匹配器、共享 store single-flight 与 trailing refresh()

## 5. 指挥中心（L3，依赖 4）

- [x] 5.1 CommandCenterPage 骨架：页头（新建任务入口 + 最近刷新时间）+ 三分区；共享 store + `GET /sessions/active` 5s single-flight；双快照不一致按各自快照呈现；失败保留旧数据；加载/空态区分
- [x] 5.2 「需要关注」推导 selector（纯函数）：六类优先级 + 单任务去重（最高优先级主呈现 + 次要标记）+ 同类排序（活跃用 last_active_at、非活跃用 updated_at）+ 过渡态排除 + 信号降级仅缺 3/4 类
- [x] 5.3 行内操作：creation_failed（重试 + 普通删除 + last_error 行内）、deletion_failed（重试 + 强制删除 + pre-delete 日志）、init 失败（查看日志 + 重跑初始化）复用现有组件；权限/问题项点击跳转工作台
- [x] 5.4 内联新建任务面板：项目可过滤下拉 + 任务名 + 基准分支（repo）+ 刷新远端分支 + dir 警告；提交门禁（有效项目 ID + 非空任务名才可 POST、偏离已选项清除 ID、base_ref 仅 repo、在途防重复提交）；创建成功跳转工作台
- [x] 5.4b 「挂起与归档」区操作：挂起（激活/归档/删除）、归档（恢复/删除），复用现有 API 与确认/脏确认/强制删除组件行为；本 lane 全部变更入口成功后调用共享 store `refresh()`（内联创建、行内操作）
- [x] 5.5 前端纯函数测试：注意力推导/排序 selector（含双快照 join 字段级来源、单侧存在归类、分区内排序回退、同类 ID tie-break）（路由/theme/store/匹配器测试归 4.9）

## 6. 项目管理 master-detail（L3，依赖 4，可与 5/7 并行）

- [x] 6.1 单页结构：左项目轨道列（搜索 + 列表 + 注册入口）+ 右详情面板；`#/projects#<id>` 深链选中；非法 id 不选中展示占位
- [x] 6.2 详情面板：头部 + 子标签（概览/自动化/环境变量），迁移现有 ProjectDetailPage 功能（不含创建表单）
- [x] 6.3 概览子标签：任务行完整状态机 + "前往指挥中心新建任务"提示链接
- [x] 6.4 ≤1024px 钻取式导航（详情含返回列表入口）
- [x] 6.5 注册表单迁移：repo/dir 类型选择 + 上下文提示 + 空项目删除两步确认语义保留；注册/删除成功后调用共享 store `refresh()`
- [x] 6.6 本 lane 内 mutation 的 `refresh()` 接线：项目管理页内全部变更入口（注册/删除项目、任务行操作）成功后调用共享 store `refresh()`（其他 lane 各自接线本页入口，全量审计见 8.8）

## 7. 设置四合一（L3，依赖 4，可与 5/6 并行）

- [x] 7.1 设置页骨架：四子标签 + `#appearance|#env|#opencode|#ai` 深链恢复；未知 tab 回退 appearance
- [x] 7.2 终端外观子标签：迁移 TermAppearanceEditor + 主题分段控件（跟随系统/浅色/深色）
- [x] 7.3 环境变量子标签：迁移 GlobalEnvEditor（follow_host/manual 模式保留）
- [x] 7.4 opencode 配置子标签：迁移配置文件列表/编辑器/乐观锁冲突处理
- [x] 7.5 AI 配置子标签：迁移 AIConfigPage 全部行为（掩码 key/校验/load_error 展示）

## 8. 工作台适配、旧代码删除与收尾验证（L4，依赖 5+6+7）

- [x] 8.1 TaskWorkbenchPage 并入壳层：页头/告警条/标签条视觉对齐设计稿；`?from` 来源感知返回保留（归一映射）；**页头任务切换器**（≤767px 侧栏任务组隐藏时的任务直达入口，hash 导航 + key 重挂载）
- [x] 8.2 终端组件视觉适配（颜色/字号 token 化）；mobile.css z 轴层级、IME、触摸锁、手势、连接状态机逻辑零改动
- [x] 8.3 Git 面板视觉适配；diff 横向滚动所有权不变量保持（仅 diff 容器滚动）
- [x] 8.4 删除任务交互统一：脏确认 + 强制删除（deletion_failed）三处一致（指挥中心/项目管理/工作台），活跃态不出现删除按钮
- [x] 8.5 删除旧代码：旧 styles.css、ActiveSessionsPage、AIConfigPage、ProjectDetailPage、ProjectsPage 及死代码（本组开始前旧页面必须已被新页面完全替代）
- [x] 8.6 全站人工走查矩阵（四屏 × 功能 × 响应式，对照 `docs/design/screens/`）：① 响应式：>1024 双栏 / ≤1024 项目管理钻取、工作台图标化+溢出菜单 / ≤767 页头两行、主控件 ≥44px、密集辅助 ≥32px、终端锁与遮罩层级；② 主题：首帧防闪烁、跟随系统切换、亮暗 token 翻转、终端配色跟随应用主题（暗=深色终端、亮=浅色终端，切换即时生效）；③ 壳层：⌘B 折叠持久化、⌘K/Ctrl+K 面板（中文匹配/6 静态入口/任务跳转）、侧栏状态点与注意力标记；④ 路由：旧深链重定向（#/active、#/ai-config、#/project/:id）、非法深链/tab 回退、?from 归一；⑤ 指挥中心：需要关注六类与排序、内联创建门禁、挂起/归档操作、轮询失败保留；⑥ 项目管理：master-detail、深链选中、注册/删除、环境变量/生命周期编辑；⑦ 设置：四子标签深链恢复、409 冲突、AI 掩码 key；⑧ 工作台：终端连接状态三覆盖层、接管、Git 面板勾选与 diff 滚动、脏确认/强制删除、移动端任务切换器
- [x] 8.7 验证（按序）：`openspec validate web-ui-redesign --strict` → `pnpm --dir web install --frozen-lockfile` → `pnpm --dir web test` → `pnpm --dir web build` → 仓库根 `go test -race ./...` → `go vet ./...`；现有 vitest 终端契约套件零回归
- [x] 8.8 `refresh()` 全量接线审计（集成 lane）：逐一核对全部变更入口（指挥中心创建/行内操作、项目管理注册/删除/任务行、工作台任务操作、侧栏操作）成功后均调用共享 store `refresh()`，清单逐一打勾
- [x] 8.9 终端配色跟随应用主题（实现 review 新增范围，用户决策）：亮/暗双 palette 统一定义（CSS --term-* 与 xterm ITheme 同一 resolver，覆盖容器/前景/背景/光标/选区/ANSI 16 色）；主题切换时已挂载终端原地更新（不重建实例/不重连 WS，scrollback/焦点/锁状态保持）；session.ts 硬编码 hex 收编；设置页预览面板同步跟随；无重连契约测试；走查矩阵 ② 项重新走查
