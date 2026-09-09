# Tasks: fix-terminal-input-panel-resize

实施顺序遵循 design「实施阶段与验收矩阵」：共享原语先行，接线依赖原语；D1/D2/D3 落点重叠（`session.ts`/`TerminalView.tsx` 区域）由统一实现所有者顺序集成，禁止并行改同一文件。

## 1. 共享 resize 原语（design D4）

- [x] 1.1 新建 `web/src/components/resize.tsx`：`ResizeHandle` 组件（6px 热区、`role="separator"`、`aria-orientation="vertical"`、`tabIndex={0}`、aria-valuenow/min/max）+ `usePersistedSize(key, default, min, max, enabled)` hook（JSON number 编解码；读取容错回退/clamp 规则；enabled=false 不读写存储、首次启用读一次、之后内存保留；读不写存储、写失败捕获保留内存）
- [x] 1.2 实现拖拽状态机 `idle → dragging → committed | cancelled → idle`：pointerdown（仅 isPrimary && button===0）冻结起始值 + setPointerCapture；pointermove 累计位移实时更新内存；pointerup 先标记 committed 清事务再保存一次后 release capture；仅 dragging 中 pointercancel/意外 lostpointercapture → cancelled 恢复起始值不写存储；**拖拽中失效（handle 卸载/enabled 变 false/实例销毁/布局失效）一律按 cancelled 结束**（恢复存活持有者起始值、不写存储、迟到 pointerup/lostpointercapture 不提交）
- [x] 1.3 单位适配与键盘：px 模式取整后 clamp；比例模式 `ratio = clamp(startRatio + deltaPx/containerWidth)`（容器零宽不启动）；方向键步进 10px（比例模式换算），每次有效调整立即保存
- [x] 1.4 组件测试：「pointerup → lostpointercapture」序列（尺寸不回退、只写一次）；拖动取消恢复起始值；「拖动中 → 切 unified/跨断点 → 返回」（恢复起始值、未写存储）；存储读取异常回退（解析失败/非有限/空串/null/字符串数字/非整数 px → 默认；合法越界 → clamp）
- [x] 1.5 验证：`cd web && pnpm test` resize 相关测试全绿

## 2. 后端 tmux 键盘配置（design D1）

- [x] 2.1 `internal/infrastructure/process/`：NewSession 前置链——`start-server \; set -s exit-empty off \; set -s extended-keys on`（纯配置链，与 new-session 分离）；**版本门禁仅作用于 extended-keys**：tmux < 3.2 或版本不可解析时，两个落点（前置链与 EnsureServerOptions）均仅跳过 extended-keys 配置并记日志，`start-server`/`exit-empty` 步骤照常执行；前置链失败降级（new-session 照常执行）
- [x] 2.2 new-session 独立调用；new-session 失败后立即 best-effort 恢复 `exit-empty on`（保留原创建错误、恢复失败汇总、不 kill-server 不影响已有会话）
- [x] 2.3 `EnsureServerOptions` 重组为三独立步骤：既有剪贴板步骤一字不动保持内部安全顺序、extended-keys 幂等步骤、`exit-empty on` 恢复步骤——除确定无 server（`ErrNoTmuxServer` 直通返回）外任一步失败不跳过其余步骤，错误汇总；恢复步骤用独立有效超时；NewSession 与 reconcile 双调用路径均覆盖
- [x] 2.4 Go 测试（复用 `execTmuxFn` 注入）：前置链先于 new-session 完成且命令序列/顺序正确；前置链失败降级；创建失败后恢复 exit-empty；**创建成功但配置失败仍返回创建成功、会话保留、不清理/重建会话、配置失败记录日志**；剪贴板失败仍恢复；extended-keys 失败仍恢复；恢复本身失败错误记录；`ErrNoTmuxServer` 直通；低版本/版本不可解析仅跳过 extended-keys（`start-server`/`exit-empty` 命令仍出现，extended-keys 不出现）；reconcile 幂等；既有剪贴板安全矩阵测试保持绿
- [x] 2.5 验证：`go test ./internal/infrastructure/process/... ./internal/task/...` 全绿

## 3. xterm 升级 + IME 补丁（design D2）

- [x] 3.1 升级依赖：`@xterm/xterm` 5.5.0→6.0.0、`@xterm/addon-fit` ^0.10.0→^0.11.0、`@xterm/addon-webgl` ^0.18.0→^0.19.0；`pnpm install` 更新 `web/pnpm-lock.yaml`
- [x] 3.2 `pnpm patch @xterm/xterm@6.0.0`：在 `CompositionHelper._finalizeComposition` 立即分支（非排除键 keydown 触发的 `_finalizeComposition(false)` 路径）插入 `this._dataAlreadySent = input;`（逐字移植上游 PR #6140）；补丁覆盖运行入口产物（`lib/xterm.js` 与 ESM 入口 `lib/xterm.mjs`）；生成 `web/patches/@xterm__xterm@6.0.0.patch` 并在 `web/package.json` 声明 `pnpm.patchedDependencies` 精确映射；`ime-compensator.ts` 不动
- [x] 3.3 放行门槛测试：jsdom 挂载真实 `Terminal`，合成 compositionstart/update + F7（keyCode 118）keydown + compositionend 序列驱动真实 CompositionHelper，断言 onData 恰好收到 commit 文本一次；**红绿举证：同一 6.0.0 未打补丁变红、打补丁变绿**（实现者提供证据，如 git stash 补丁后运行）
- [x] 3.4 CapsLock（keyCode 20）路径独立原生回归用例（不承担补丁红绿举证）；既有测试全绿并逐条承接 spec scenario：`session-adapter.test.ts` 的「Shift+Enter 拦截翻译」用例（承接 IME 组合/process key 期间不拦截 scenario）、`ime-compensator.test.ts` 决策表用例（承接正常上屏恰好一次 scenario）——若核对后发现 scenario 无对应用例则补写
- [x] 3.5 验证：全新安装 `pnpm install --frozen-lockfile` 通过 + 复现测试绿 + `pnpm build` 通过

## 4. 导航焦点请求（design D3，依赖任务 3 的 session.ts 顺序集成）

- [x] 4.1 新建 `web/src/terminal/focus-request.ts`：模块级内存单例；`requestTerminalFocus(taskID)`；请求字段 `{ taskID, seq 递增, ts }`；过期 5s；保留最新 pending、新覆盖旧；订阅时同步交付当前快照；消费/取消按 seq 比较、仅清除匹配 seq；卸载清理不删更晚请求
- [x] 4.2 三个生产点接线：`AppShell` 侧栏任务项 onClick、工作台任务切换器选择、指挥中心任务行点击（导航仍走原生 href/navigate，信号为附加调用）
- [x] 4.3 消费方：唯一消费方为目标任务 TUI TerminalView（wsPath 形态区分，`/ws/terminal/shell/...` 不消费；路由不匹配暂不消费保留）；`TermSession` 暴露 `focus()` 封装 `term.focus()`
- [x] 4.4 标签激活：目标 TaskWorkbenchPage 收到匹配未过期请求且当前 tab ≠ TUI 时 `switchTab(TUI_TAB)`——只激活不消费；仅显式导航请求触发，普通重连/无请求挂载不切标签
- [x] 4.5 就绪门禁与状态表（逐字按 design D3 状态表）：已 connected 立即门禁检查后消费；idle/connecting/recovering 挂起等待；显式请求在重连中挂起等待本次 connected；普通重连不产生请求、已消费不重新聚焦。门禁五条：未过期且 seq 最新、taskID 匹配路由、connected 且未锁定、焦点保护白名单（body/`.od-sidebar`/任务切换器/指挥中心任务行）、输入元素排除优先
- [x] 4.6 组件测试：「发布 B 请求 → A 卸载 → B 订阅 → B connected → 聚焦」；锁定/输入区/过期/路由离开取消路径；「当前 Git 标签 → 点击同任务 → TUI 激活 → connected 后聚焦」；重连不产生请求
- [x] 4.7 验证：`cd web && pnpm test` 焦点相关测试全绿

## 5. 三处布局接线（design D5/D6/D7，依赖任务 1）

- [x] 5.1 D5 应用侧栏：`AppShell` 持有 `sidebarW`（键 `ocdeck:sidebar-width`，int px ∈ [180,480]，默认 232），内联 `--sidebar-w` 设于 `.od-shell`；`ResizeHandle` 在 `.od-sidebar` 右缘；折叠（图标轨）与 ≤767px 顶栏形态不渲染 handle；⌘B 展开恢复持久化宽度
- [x] 5.2 D6 git 文件面板：`GitPanel` 持有 `gitSideW`（键 `ocdeck:git-side-width`，int px ∈ [240,600]，默认 340）+ `gitSideCollapsed`（会话内不持久化）；宽度经内联 `--git-side-w` CSS 变量传递（桌面规则 `width: var(--git-side-w, 340px)`，窄屏 `width:100%` 不动）；收起为 36px 窄条仅纵向展开按钮、收起态隐藏 handle；断点状态表按 design（>1024px 显示 handle+收起按钮，≤1024px 强制完整显示、隐藏两者、桌面偏好保留）
- [x] 5.3 D7 diff 分栏：`applySplitRatio(view, ratio)` 局部适配——MergeView 实际创建完成后（loadLanguage await 后、destroyed 检查）及比例变化时调用；经 `view.a.dom`/`view.b.dom` 定位并向上校验父节点为 `.cm-mergeViewEditor` flex item（a=old、b=new），失败安全退出（禁用 handle、console.warn、不写布局/比例）；比例持久化（键 `ocdeck:diff-split-ratio`，float ∈ [0.2,0.8]，默认 0.5）；enabled 谓词 = 源码模式 + merge 状态 + side-by-side + `oldExists && newExists`（单侧坍缩不应用、不显示、不访问存储、内存偏好保留）；比例变化不进入重建 deps、不触发 docChanged/保存/重取/编辑会话清除；handle 绝对定位中缝（容器 relative，left = ratio × 有效容器宽度；有效容器宽度 = `.diff-editor` 容器 contentBox 宽度）
- [x] 5.4 组件测试：三处持久化读写与刷新恢复；resize 不触发保存/rebuild；异步 MergeView 创建后比例应用；双侧↔纯新增/纯删除切换（单侧坍缩不被破坏、返回双侧恢复比例）；D6 断点状态；**侧栏 ⌘B 折叠→展开恢复自定义宽度；git 文件面板收起→展开恢复收起前宽度**
- [x] 5.5 验证：`cd web && pnpm test` 布局相关测试全绿 + `pnpm build` 通过

## 6. 集成回归与手动验收

- [x] 6.1 全量回归：`go build ./... && go test ./...` 与 `cd web && pnpm test && pnpm build` 全绿
- [x] 6.2 手动验收矩阵逐条执行（design「手动验收矩阵」九行）：新建任务 tmux + Shift+Enter 换行不提交；ocdeck 重启后存量任务 pane 重建 + Shift+Enter 同上；普通 Enter / shell 终端行为不变；真实 IME "nihao" 中途切英文（Chrome + Safari）恰好一份；**正常中文上屏不丢字回归**；CapsLock 切换路径不重复不丢字；6.0 滚动条渲染/滚动/选区回归；三处拖拽后刷新恢复；宽↔窄断点往返；编辑态 resize 不触发保存不清除编辑会话
