# Delta: task-permission-mode（workbench-base-ref-and-overflow）

## REMOVED Requirements

### Requirement: Web 新建任务权限模式选择

**Reason**: 表单缺省选中从 `ask` 改为 `ai-auto`，scenario 名称需同步更新（delta 的 MODIFIED 不允许删除/重命名 scenario，故以 REMOVED + ADDED 新名替换）。
**Migration**: 由下方 ADDED 的「Web 新建任务权限模式选择（缺省 AI 自动识别）」整体接替。

## ADDED Requirements

### Requirement: Web 新建任务权限模式选择（缺省 AI 自动识别）

Web 新建任务表单 SHALL 提供权限模式选择项，三档取值与创建接口一致（`ask` / `all-approve` / `ai-auto`），缺省选中 `ai-auto`；提交时按选择传递权限模式参数。权限模式选择器 SHALL 在面板打开后即渲染，与是否已选项目无关；未选项目时表单仍不可提交（既有提交门禁不变），项目选择变化只按重置规则处理其值。创建接口的缺省语义不变：未提供 `permission_mode` 时仍按 `ask` 处理（见「任务权限模式选择与持久化」，本变更不改 API 契约）。

#### Scenario: 表单缺省 ai-auto

- **WHEN** 用户打开新建任务表单且未改动权限模式选择
- **THEN** 表单缺省选中 `ai-auto`，创建请求显式携带 `permission_mode: 'ai-auto'`，任务按 `ai-auto` 模式落库

#### Scenario: 未选项目时选择器可见

- **WHEN** 用户打开新建任务面板且尚未选择项目
- **THEN** 权限模式三档选择器可见且缺省选中 `ai-auto`；表单仍不可提交

#### Scenario: 表单选择模式创建

- **WHEN** 用户在新建任务表单选择 `ask` 或 `all-approve` 并提交
- **THEN** 创建请求携带所选权限模式（`ask` 按既有"非缺省才传"惯例可不携带字段，后端缺省补 `ask`），任务按该模式落库

#### Scenario: 切换项目重置为 ai-auto

- **WHEN** 用户在新建任务面板改变已选项目（含切换项目、清除选择、切到 dir 项目）
- **THEN** 权限模式选择重置为 `ai-auto`；不改变已选项目的面板信号保持当前选择
