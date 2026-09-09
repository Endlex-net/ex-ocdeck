# Delta Spec: git-operations

## ADDED Requirements

### Requirement: 评审文件面板尺寸调整与收起

git 评审面板（`.git-panel`）的左侧文件列表面板 SHALL 支持拖拽调整宽度与收起/展开：面板右缘提供拖拽把手，拖动实时调整宽度且受最小/最大值约束（具体取值见 design）；提供显式收起/展开入口，收起后文件列表区域隐藏、diff 区域占满，展开后恢复收起前宽度。调整后的宽度 MUST 持久化于 localStorage 并在刷新/重新进入后恢复。窄视口（≤1024px）纵向堆叠形态下的既有布局行为 MUST NOT 被破坏。

#### Scenario: 拖拽调整文件面板宽度并持久化

- **WHEN** 用户在 git 评审面板拖拽左侧文件面板右缘调整宽度，随后刷新页面重新进入评审
- **THEN** 文件面板以调整后的宽度渲染

#### Scenario: 收起后展开恢复宽度

- **WHEN** 用户收起左侧文件面板，随后展开
- **THEN** 文件面板恢复收起前的宽度，diff 区域相应回缩

### Requirement: diff 并排分栏比例调整

side-by-side diff 视图中 old/new 两侧的边界 SHALL 支持拖拽调整分栏比例：边界处提供拖拽把手，拖动实时调整两侧编辑器宽度占比；调整后的比例 MUST 持久化于 localStorage 并在刷新/重新进入后恢复。unified（单列）视图不受影响；切回 side-by-side 时 MUST 应用已持久化的比例。**适用范围仅两侧文件均存在的并排形态**：纯新增/纯删除 diff 的单侧坍缩布局 MUST NOT 被分栏比例覆盖（不应用比例、不显示把手、不访问比例存储），已持久化的比例 MUST 保留并在返回双侧形态时恢复。

#### Scenario: 拖拽调整分栏比例并持久化

- **WHEN** 用户在 side-by-side diff 视图拖拽 old/new 边界调整比例，随后刷新页面重新打开该 diff
- **THEN** 两侧编辑器以调整后的比例渲染

#### Scenario: 单列视图不受影响

- **WHEN** 用户切换到 unified 单列视图
- **THEN** diff 渲染为单列形态，分栏比例不生效、不被清除
