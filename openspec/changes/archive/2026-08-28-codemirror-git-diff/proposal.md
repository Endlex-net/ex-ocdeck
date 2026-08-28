# Proposal: codemirror-git-diff

## Why

任务工作台的 git diff 目前由 diff2html 渲染为静态 HTML（`dangerouslySetInnerHTML` 注入），只有 diff 级别（+/− 行）着色，无语法高亮、无并排对照、样式定制困难。同时，后续规划中的「工作区文件读/编辑」特性已选定 CodeMirror 6 作为编辑器栈——继续在 diff 场景维护 diff2html 会造成两套渲染体系并存。本 change 先用 CodeMirror 6 替换 git diff 渲染，统一编辑器依赖并顺带提升 diff 阅读体验，为后续文件读/编辑特性趟平集成路径。

## What Changes

- 前端 git diff 视图从 diff2html 替换为 CodeMirror 6 的 merge/diff 能力，支持语法级高亮，并支持并排（side-by-side）与单列（unified）两种形态切换。
- 后端 git diff 相关 API 的响应数据承载方式从「unified diff 文本」调整为「变更文件两侧版本的内容」，以支撑前端 merge 视图；端点与普通单文件查询参数形态不变（历史空 path 全仓 diff 形态收紧为 invalid_input）；超大文件处理策略随新承载方式调整。
- 移除 diff2html 前端依赖及其样式。
- 编辑器封装层在本 change 中以只读 diff 场景落地，但其形态需为后续「文件读/编辑」特性复用预留演进空间（不在本 change 内实现编辑）。
- **BREAKING**（内部 API）：git diff 端点的响应结构变更；该 API 仅服务于内嵌前端，无外部消费者。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `git-operations`: 「文件 diff 查看」与「diff 视图渲染对齐」两个 requirement 的行为将变化——diff 内容由两侧版本内容承载、前端渲染形态从 diff2html 静态 HTML 变为 CodeMirror merge 视图（含并排/单列切换与语法高亮）。除数据承载与渲染方式外，超大文件、未跟踪新文件、二进制文件、空新文件、非法参数等既有场景的用户可见语义保持不变；新承载方式下继续提供有界、不致前端冻结的展示体验，具体限制值与截断表示由 spec/design 定案。

## Impact

- **前端**：`web/src/components/GitPanel.tsx` diff 渲染区重写；新增 CodeMirror 6 相关依赖（核心 + merge + 按需语言包），移除 diff2html；`web/package.json` 依赖变更。
- **后端**：`internal/api/git.go` diff 端点的响应数据承载变更；相关 git 基础设施的数据获取与返回契约受影响，具体内容获取方式由 design 定案。
- **API 契约**：git diff 端点的响应结构变更，仅影响内嵌前端。
- **范围边界与非目标**：
  - 不做工作区文件树浏览、任意文件读/编辑（后续独立 change）。
  - 不做 diff 视图内编辑、不做 LSP/代码跳转。
  - 不改变 git status 分组、提交勾选、commit/push 等既有交互逻辑。
  - 移动端/窄屏下的 diff 形态适配策略由 design 定案，但不在本 change 引入新的移动端专属交互。
