# Tasks: workbench-base-ref-and-overflow

## 1. 后端 DTO 透出

- [x] 1.1 `internal/api/tasks.go`：`taskRowDTO` 增加 `BaseRef string \`json:"base_ref"\``（无 omitempty，必有字段），`toTaskDTO` 直接映射 `t.BaseRef`（design D1）。验证：后端单测断言详情 DTO JSON 含 `base_ref` 键——worktree 任务为落库全限定 ref（`refs/heads/...`、`refs/remotes/...` 两形态原样透出）、非 worktree 与历史空值为空串，`go test ./internal/api/...` 通过
- [x] 1.2 验证 REST 详情与 SSE 详情流同任务 `base_ref` 一致（SSE 复用同一组装逻辑，无需改流代码）、历史空值任务详情不报错不回填。验证：相关测试通过或手动核对两条路径输出一致

## 2. 前端共享逻辑抽取

- [x] 2.1 `web/src/types.ts`：`Task` 接口新增 `base_ref: string`（必有；对旧服务端运行时字段缺失按空串降级处理，design D1）。验证：`tsc` 类型检查通过
- [x] 2.2 新增 `baseRefShortName(ref: string): string` 纯逻辑 helper（design D2/D5）：空串返回空串；仅当输入以 `refs/heads/` 或 `refs/remotes/` 开头**且前缀后名称非空**时移除前缀；其余输入（未匹配前缀、前缀后为空等异常形态）原样返回，不抛错。验证：vitest 单测覆盖两前缀正常形态、空串、未匹配前缀，并明确断言 `refs/heads/`、`refs/remotes/`（前缀后为空）各自原样返回
- [x] 2.3 提取共享剪贴板 util（design D5）：`writeTextToClipboard` 与 `copyViaExecCommand` 从 `web/src/terminal/TerminalView.tsx:61-79` 迁入 `web/src/clipboard.ts`，保持 `Promise<void>` 语义与降级链（writeText 可用→单次调用，reject 即失败不再 execCommand；不可用→execCommand，true 成功/false 或抛错失败）；TerminalView 改为引用共享 util。验证：vitest 覆盖两分支降级语义；TerminalView 既有复制行为（含 takeToastSlot 节流）不回归
- [x] 2.4 溢出菜单可见项计算 helper（design D4/D5）：输入 `init_status`/`status`，输出可见菜单项列表（`init_status==='failed'` → init 日志在前；`status!=='active'` → 删除在后；禁用状态不参与可见性）。验证：vitest 覆盖四种组合

## 3. 页头分支展示与点击复制

- [x] 3.1 `TaskWorkbenchPage.tsx:420` 页头分支区扩展（design D3）：worktree 任务首先保留现有外层渲染条件——`task.branch` 非空才渲染分支信息区（`branch` 为空时整段不渲染，即使 `base_ref` 非空）；满足后再按 `base_ref` 分流——非空 → 两行展示（上行当前、下行来源短名 + ↳ 静态图标），空串 → 仅当前分支单行；dir 整段不渲染。验证：vitest 渲染矩阵（worktree 有/无 base_ref、worktree 但 `branch=''` 且 `base_ref` 非空时整段不渲染、dir 不渲染、同名不去重）
- [x] 3.2 local-path 当前分支获取（design D3）：请求条件严格为 `task.project_kind === 'repo' && task.mode === 'local-path'`（MUST NOT 用 `!isGitlessTask` 门控）；任务详情就绪后经既有 `api.gitStatus` 获取一次，按任务身份隔离、随 taskID 切换重新获取、不轮询；非 401 失败/空串静默降级不展示，401 沿用共享客户端认证流程。验证：vitest 覆盖请求谓词（dir 不发请求、worktree 不发请求）、成功展示、失败/空串降级
- [x] 3.3 分支名 button 化与点击复制（design D3 点击复制子块）：每个分支名独立 `<button type="button">` 视觉归零（继承 `.header-meta` 样式层级，来源弱化/当前标准，`:focus-visible` 焦点环保留）、`cursor: copy` + hover 提亮 + underline dotted；点击经共享 util 复制各自显示短名；反馈复用 `.od-toast`（2000ms、clearTimeout 重显、不套用 takeToastSlot）——成功 `已复制 <name>`、失败 `复制失败，完整分支名见悬浮提示`；aria-label 双分支态 `复制来源分支 <name>`/`复制当前分支 <name>`、单分支态 `复制当前分支 <name>`；tooltip 下沉到各 button（唯一模板集见 design D3），容器 span 不挂 title。验证：vitest 覆盖复制内容=显示短名、成功/失败 toast 文案、aria-label 两态、tooltip 模板三形态；local-path 单分支点击复制及 toast；同名来源/当前两个按钮分别点击均复制成功并各自反馈（同名连续点击同时断言反馈计时重置）
- [x] 3.4 页头样式：两行布局（上行当前 10px/--fg、下行来源 9px/muted·0.7 + ↳ 静态图标）与 button 视觉归零样式。验证：页面渲染肉眼核对（人工已确认） + 既有页头测试不回归

## 4. 溢出菜单空态隐藏

- [x] 4.1 `WorkbenchOverflow`（`TaskWorkbenchPage.tsx:38-135`）改用 2.4 的可见项列表渲染，列表为空时整个 `.header-overflow` `return null`（所有 hook 之后，同 `OpenInEditorMenu:94` 写法）；菜单项由非空变空时关闭展开状态（`menuOpen` 重置），恢复后不自动展开、不调回调；失焦/Escape 关闭、焦点恢复、backdrop、点击回调与禁用条件均不变（design D4）。验证：vitest 覆盖显隐矩阵（active+init 正常 → 隐藏；init failed 或非 active → 显示；过渡状态禁用项保留入口）与"展开→隐藏→恢复"交互

## 5. 新建任务面板权限模式缺省

- [x] 5.1 `CommandCenterPage.tsx` 新建任务面板权限模式选择器缺省选中从 `ask` 改为 `ai-auto`；面板内切换/清除/改选项目时的重置目标同步改为 `ai-auto`（design D6；现有 permArg 逻辑自动显式传值，后端契约不变）。验证：vitest 断言初始选中为 ai-auto、切换项目重置为 ai-auto、改选其他模式后提交携带所选值

## 6. 整体验证

- [x] 6.1 全量验证：`go test ./...` 与前端 `vitest run` + 前端构建通过；`openspec validate workbench-base-ref-and-overflow` 通过。验证：命令输出全绿
