# web-ui-shell 变更 Delta

## MODIFIED Requirements

### Requirement: 全局应用壳层与侧栏导航

系统 SHALL 提供全局应用壳层包裹全部管理页面：左侧导航脊柱包含品牌区、指挥中心顶层项、按项目分组的任务组（任务项带 agent 状态点与注意力标记）、管理组（项目管理、设置）与底栏（⌘K 入口、本地地址、主题切换）。壳层 MUST 支持 ⌘B 快捷键与底栏按钮折叠为图标轨，折叠状态 MUST 持久化于 localStorage 并在下次打开时恢复。未认证时 MUST NOT 渲染壳层（仅呈现令牌门）。ServerStatusBanner MUST 在壳层内所有页面可见。

侧栏任务组数据 MUST 来自 App 层共享 projects store（`/api/v1/projects/stream` SSE 订阅 + 常驻低频兜底轮询，见 projects-stream spec）：壳层侧栏、指挥中心与项目管理页消费同一数据源，应用内 MUST NOT 存在第二个 `/projects` 轮询或第二条 projects 流订阅（兜底轮询属于 store 内部、single-flight 语义保持）。store MUST 暴露 `refresh()`（trailing 语义）：任何变更操作成功后调用；若调用时已有加载在途，MUST 在该请求结束后再补发一次——`refresh()` 承诺其结果反映调用之后的最新状态（MUST NOT 以 mutation 前的在途快照交差）。流订阅或兜底轮询失败时侧栏 MUST 保留上次成功数据静默展示（错误由 ServerStatusBanner 与页面级错误提示承担，侧栏本身不闪空态）。

**移动端任务入口**：≤767px 视口侧栏任务组隐藏时，任务切换入口 MUST 由工作台页头的任务切换器承担（执行与侧栏相同的 hash 导航 + 按 taskID 重挂载），移动端 MUST NOT 失去任务间直达能力。

#### Scenario: 跨页面导航

- **WHEN** 用户在任意页面点击侧栏的指挥中心/项目管理/设置
- **THEN** 应用切换到对应页面且侧栏保持渲染（不整页重载）

#### Scenario: 折叠持久化

- **WHEN** 用户按 ⌘B 折叠侧栏后刷新页面
- **THEN** 侧栏保持折叠的图标轨形态

#### Scenario: 侧栏任务组状态呈现

- **WHEN** 存在活跃或挂起任务
- **THEN** 侧栏按项目分组展示这些任务，活跃任务显示 agent 状态点（idle/busy/retry），有待处理注意力项的任务显示注意力标记；归档任务 MUST NOT 出现在侧栏

#### Scenario: projects 数据流驱动更新

- **WHEN** store 订阅存续期间收到 projects 流的 `snapshot` 或 `update` 帧
- **THEN** store 以帧内裸数组整表替换快照，侧栏任务组随之收敛，期间不存在对 `/api/v1/projects` 的固定 5 秒轮询请求

#### Scenario: 侧栏切换任务

- **WHEN** 用户点击侧栏任务组中的某任务
- **THEN** 应用导航至该任务工作台 `#/task/:id`
