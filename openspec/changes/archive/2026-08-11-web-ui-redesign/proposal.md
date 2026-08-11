# Proposal: web-ui-redesign

## Why

当前 Web UI 是 ad-hoc 的"项目优先"六页结构：无全局导航、无设计系统、管理页面无移动端适配，且用户最关心的问题——"哪个 agent 需要我"——没有答案入口。UX 设计已交付（亮壳驾驶舱 × 深色终端，任务优先 · 指挥中心制；实现 review 阶段用户决策终端配色改为跟随应用主题），本 change 将其完整落地。

## What Changes

- **全局壳层**：新增侧栏导航脊柱（指挥中心 / 任务组 / 管理组）、⌘B 折叠、亮/暗双主题体系（`data-theme` + localStorage，跟随系统默认）、⌘K 全局命令面板；TokenGate 与 ServerStatusBanner 适配新壳层。
- **设计系统**：以设计交付物 `ocdeck-ui.css` 为基底的 token + 组件类体系（6 品牌 token、语义色 color-mix 派生、徽章/告警/按钮/表单/模态全套），替换现有单一 ad-hoc 样式表；现有 React 组件逻辑全部保留，仅替换样式与结构。
- **指挥中心（新首页）**：取代项目列表与活跃会话页。"需要关注"分区置顶（等待权限确认 / 等待回答问题 / 失败态 / init 失败 / notice 残留 / agent idle 活跃任务），活跃任务、挂起与归档分区，页头内联新建任务。注意力项点击跳转对应任务工作台（信号+跳转，审批动作仍在 TUI 内完成）。
- **任务工作台并入壳层**：侧栏任务组作为就地任务切换器；终端子系统（IME 补偿、触摸锁、手势、连接状态机）逻辑不动，仅视觉适配。
- **项目管理 master-detail**：项目列表 + 项目详情合并为单页（左轨道列 + 右详情面板，含概览/自动化/环境变量子标签）；≤1024px 钻取式导航。
- **设置四合一**：终端外观（含亮/暗主题）、全局环境变量、opencode 配置、AI 配置合并为单页四子标签；原 `#/configs`、`#/ai-config` 深链重定向。
- **后端注意力信号（小改）**：SSE 订阅新增捕获 `permission.asked/replied`、`question.asked/replied/rejected`（含 `*.v2.*` 家族）事件，按任务维护 pending 集合，在任务详情与活跃会话聚合 API 中透出。不新增 reply 代理端点。
- **BREAKING（UI 层）**：路由收敛——`#/` 改为指挥中心；`#/active`、`#/ai-config`、`#/project/:id` 重定向到新页面（旧深链不 404）。后端 API 无 breaking change。

## Capabilities

### New Capabilities

- `web-ui-shell`: 全局应用壳层——侧栏导航、亮/暗主题系统、⌘K 命令面板、响应式分级、TokenGate/状态横幅适配。
- `command-center`: 指挥中心首页——"需要关注"注意力聚合、活跃/挂起/归档任务分区、内联新建任务。
- `agent-attention`: 后端注意力信号——opencode 权限/问题 pending 请求的 SSE 捕获、按任务维护、API 透出。

### Modified Capabilities

- `active-sessions-overview`: 活跃会话独立页并入指挥中心，原页面路由重定向；聚合数据扩展注意力字段。
- `project-management`: 项目列表页与项目详情页合并为 master-detail 单页。
- `global-config-management`: 设置页改为四子标签统一入口（opencode 配置为其中之一）。
- `ai-provider-config`: AI 配置并入设置页子标签，原独立路由重定向。

## Impact

- **前端（主要）**：`web/src/` 全量换肤 + 结构重组——新增壳层/指挥中心/命令面板组件，App.tsx 路由改造，6 页收敛为 4 屏，`styles.css` 被设计系统样式替换。终端子系统（`web/src/terminal/`）逻辑不动。
- **后端（小改）**：`internal/opencode/client.go` SSE 事件处理扩展；`internal/task/` 维护权限/问题 pending 状态；`internal/api/tasks.go` 活跃会话聚合与任务详情响应扩展注意力字段。无 schema/迁移变更，无新端点（除注意力字段并入现有响应）。
- **外部契约**：opencode serve 1.18.14 的 experimental permission/question 事件与端点（源码级确认，1.18.0–1.18.15 逐字节一致）；遵守能力探测策略，信号不可用时降级为无注意力项而非报错。
- **依赖**：不新增前端运行时依赖（命令面板按设计稿 palette.js 行为规格自实现）。
