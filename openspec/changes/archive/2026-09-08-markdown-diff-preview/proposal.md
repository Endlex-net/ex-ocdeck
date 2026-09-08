# Proposal: markdown-diff-preview

## Why

在 git review（DiffViewer）中阅读 `.md` 文档改动时，只能看带语法高亮的 markdown 源码；标题层级、列表、表格、链接等排版效果无法直观确认，review 文档类变更的效率和准确性都较差。用户希望直接以渲染后的结果阅读 markdown 文件。

## What Changes

- DiffViewer 对 `.md` / `.markdown` 文件新增「源码 / 渲染预览」开关：渲染模式下左右两栏（old/new）各自按 **GFM（GitHub Flavored Markdown）语义**渲染 markdown（含表格、任务列表、删除线、自动链接等），便于阅读 GFM 渲染效果（不承诺与 GitHub 页面像素级一致）。
- 渲染模式为只读预览；编辑等现有交互保持只在源码模式下可用（具体交互细节在 design 中定义）。
- 预览模式支持块级批注：点击渲染出的块（段落/标题/列表项/表格/代码块/引用）创建锚定该块源码行范围的批注，与既有行范围批注模型复用同一数据契约。
- 提交给 AI 的内容新增统一省略规则（对所有文件类型生效）：批注引用范围超过 5 行或 300 字符时，仅注入批注头（文件、行范围、侧别与来源）与评论，不注入引用内容本身；否则维持既有行为。
- 引入安全的 markdown 渲染能力：markdown 源中的原始 HTML 不被注入或执行（具体机制约束在 design/spec 中定义）。
- 同步修复既有换行默认漂移：diff 视图默认换行由开启改为关闭（对齐 `git-operations` spec「代码行默认 MUST NOT 折行」的既定契约），用户仍可手动开启换行。

非目标（Non-goals）：
- 不提供渲染级的行/段落差异高亮——渲染模式只看各自渲染效果，变更对比仍由源码模式承担。
- 不扩展到其他格式的渲染预览（如 HTML、SVG、图片 diff）。
- 不改动后端 diff 接口与数据契约。
- 不处理无后缀文件（README、CHANGELOG 等）的 markdown 识别。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `git-operations`: diff 视图渲染相关需求变更——markdown 文件在 diff 视图中新增可选的渲染预览模式及其与源码模式的切换、只读边界等行为。
- `diff-annotations`: 预览模式新增块级批注能力（块级手势、徽章显示）；提交内容新增统一省略规则（大引用范围仅注入批注头（文件、行范围、侧别与来源）与评论）。

## Impact

- **前端**：DiffViewer、markdown 文件识别及新增渲染组件受影响。
- **依赖**：新增前端 markdown 渲染依赖（GFM 能力；最终选型在 design 中确定）。
- **后端**：diff 接口与 `GitDiffDTO` 无改动（文件类型由前端依据 path 判定）；提交 payload 组装新增统一省略规则分支，无 API/DB 契约变更。
- **规范**：`openspec/specs/git-operations/spec.md` 与 `openspec/specs/diff-annotations/spec.md` 的需求增量更新。
- **兼容性**：markdown 预览与块级批注为增量能力，无 BREAKING；同时包含一项既有行为修复（默认换行关闭，对齐既有 spec 契约）；其余源码模式的 diff 展示与批注创建行为不变，提交 payload 统一省略规则对所有文件类型生效。
