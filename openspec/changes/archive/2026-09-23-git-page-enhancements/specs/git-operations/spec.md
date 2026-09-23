## ADDED Requirements

### Requirement: 并排 diff 横向滚动协同

side-by-side（并排）形态下，两侧编辑区的横向滚动 SHALL 双向协同：用户横向滚动任一侧时，另一侧 MUST 同步至其最大可行横向偏移（目标偏移 = 源偏移经目标侧最大可滚动距离 clamp；两侧内容最大可滚动距离相同时即为相同偏移）。协同 MUST NOT 形成滚动回环——由同步驱动的一侧滚动 MUST NOT 再次触发反向同步；目标侧 clamp 到小于源侧的偏移时，源侧 MUST 保持用户设定偏移，MUST NOT 被目标侧 echo 回拉。同值竞态例外（显式声明）：程序性同步写入的 echo 事件与同值的用户滚动事件在技术上无法区分；若用户恰在 echo 在途期间将一侧滚至与在途 echo 相同的偏移，该次滚动允许不触发对侧同步，任一下一次滚动事件到达后 MUST 恢复同步（自愈，MUST NOT 形成持久分叉）。开启「换行」时两侧不存在横向滚动，本需求不产生行为；unified（单列）形态不适用；纯新增/纯删除 diff 的单侧坍缩布局不适用——仅一侧存在内容时该侧 MUST 可独立横向滚动，MUST NOT 被同步逻辑回拉。协同 MUST NOT 改变既有纵向滚动与变更块对齐行为；查看模式与编辑模式下行为一致。

#### Scenario: 拖动任一侧横向滚动

- **WHEN** 用户在并排形态（未开启换行）下横向滚动旧侧或新侧
- **THEN** 另一侧同步滚动至其最大可行横向偏移（源偏移经目标侧 clamp；两侧最大可滚动距离相同则为相同偏移），两侧代码列保持对齐

#### Scenario: 同步无回环振荡

- **WHEN** 用户连续快速交替拖动两侧横向滚动
- **THEN** 两侧最终收敛停住，无持续振荡或失控滚动；两侧最大可滚动距离不同时，目标侧停在其最大可行偏移，源侧保持用户偏移不被回拉

#### Scenario: 换行模式无横向协同

- **WHEN** 用户开启「换行」
- **THEN** 两侧内容自动折行、无横向滚动条，横向协同不适用

#### Scenario: 单侧 diff 不启用协同

- **WHEN** 纯新增（或纯删除）diff 处于并排形态的单侧坍缩布局，用户横向滚动存在内容的一侧
- **THEN** 该侧独立横向滚动查看长行，不被同步逻辑回拉

#### Scenario: 单列形态不适用

- **WHEN** 用户切换到 unified 单列形态
- **THEN** 横向协同不生效，视图行为与既有单列形态一致

### Requirement: diff 文件列表混合展示与状态标识

未提交变更视图的 diff 文件列表 SHALL 以单一混合列表展示全部变更文件（已暂存/未暂存/未跟踪），替代按状态分组的展示形态；每条目 MUST 通过文件名颜色区分其状态，状态到颜色的映射固定：已暂存 → 成功绿 `var(--success)`、未暂存 → teal `rgb(128,203,196)`、未跟踪 → 琥珀 `var(--warn)`；颜色 MUST NOT 作为唯一状态信息——每条目 MUST 同时附状态文本（`title`/`aria-label`）。条目内容布局：flat 形态 MUST 先展示文件名、随后展示目录路径（目录路径为次要视觉层级），替代仅展示单一路径字符串的形态；tree 形态文件节点 MUST 仅展示文件名（目录层级由树结构表达，文件名后 MUST NOT 跟目录路径）；两种形态条目的 `title`/`aria-label` 均 MUST 保留完整路径。同一路径同时存在已暂存与未暂存变更时 MUST 显示为两条独立条目，各自携带对应状态颜色，点击后打开的 diff 来源与既有语义一致（已暂存条目对应 `ref=HEAD`，未暂存条目对应 `ref=''`）。列表排序 MUST 确定：路径按 UTF-8 byte-wise 字典序（与「分支对比变更文件列表」服务端排序同一比较规则，MUST NOT 使用 locale 相关比较），同路径多条目按状态固定顺序（已暂存先于未暂存）。条目的增删行数、二进制标记与点击打开 diff 的行为 MUST 与既有分组形态一致。本需求的颜色与状态语义同样适用于分支对比视图中的未提交条目（见「分支对比变更文件列表」）。

#### Scenario: 三种状态混合展示

- **WHEN** 工作区同时存在已暂存、未暂存与未跟踪文件
- **THEN** 全部文件出现在同一列表中，三种状态的文件名颜色互相可区分（已暂存成功绿 / 未暂存 teal / 未跟踪琥珀）

#### Scenario: 文件名优先展示

- **WHEN** 列表（扁平形态）展示位于深层目录的文件
- **THEN** 条目先展示文件名、随后展示目录路径，用户无需横向滚动即可识别文件名

#### Scenario: 同路径暂存加未暂存显示两条

- **WHEN** 某文件先暂存了一部分改动、工作区又有新的未暂存改动
- **THEN** 列表中该路径显示为已暂存与未暂存两条独立条目，分别打开对应的 diff 来源

#### Scenario: 排序确定

- **WHEN** 列表包含多个路径（含非 ASCII 路径）与同路径多条目
- **THEN** 条目按路径 UTF-8 byte-wise 字典序排列，同路径条目已暂存先于未暂存，多次刷新顺序一致

### Requirement: diff 文件列表横向滚动

文件列表行内容（完整路径、增删统计）超出列表可用宽度时，列表 SHALL 支持横向滚动以查看完整内容，MUST NOT 仅以路径尾部截断作为唯一查看手段。横向滚动 MUST NOT 破坏列表纵向滚动与「评审文件面板尺寸调整与收起」的拖拽调宽行为。本需求对未提交变更视图与分支对比视图的文件列表均生效。

#### Scenario: 长路径横向滚动

- **WHEN** 文件路径超出列表宽度
- **THEN** 用户可横向滚动查看完整路径与增删统计

#### Scenario: 调窄面板后仍可查看

- **WHEN** 用户将文件面板拖拽调窄后行内容溢出
- **THEN** 横向滚动仍可用，纵向滚动与条目点击行为正常

### Requirement: diff 文件列表 tree 模式

文件列表 SHALL 支持扁平列表与目录树两种展示形态，用户 MUST 可显式切换，默认形态为扁平列表。目录树形态 MUST 按路径的目录层级组织条目：目录节点 MUST 可折叠/展开，文件节点 MUST 保留状态颜色（未提交条目）、增删统计与点击打开 diff 的行为，与扁平形态一致；文件节点 MUST 仅展示文件名（目录层级由树结构表达，文件名后 MUST NOT 跟目录路径），其 `title`/`aria-label` MUST 保留完整路径（与扁平形态一致）；不含任何变更文件的目录层级 MUST NOT 出现。节点标识与排序 MUST 确定：目录节点按目录路径唯一；同一路径的多状态条目在树中为多个独立文件节点（不得去重）；同级节点按路径段 UTF-8 byte-wise 字典序排列，同路径多条目顺序与扁平形态一致（已提交 → 已暂存 → 未暂存 → 未跟踪）；分支对比视图的已提交条目无暂存状态概念，其节点身份不得复用暂存状态枚举值（实现层以固定后缀区分，见 design D6）。两种形态 SHALL 对未提交变更视图与分支对比视图的文件列表均生效。形态选择 MUST 在 Git 面板会话内保留，会话结束丢弃、MUST NOT 持久化。

#### Scenario: 切换目录树形态

- **WHEN** 用户切换到目录树形态
- **THEN** 文件按目录层级嵌套展示，目录可折叠/展开，文件条目行为与扁平形态一致

#### Scenario: 形态选择会话内保留

- **WHEN** 用户切换到目录树形态后切换查看不同文件或切换视图
- **THEN** 列表保持目录树形态；Git 面板会话结束（或页面刷新）后恢复默认扁平列表

### Requirement: 分支对比变更文件列表

系统 SHALL 提供任务 worktree 当前 HEAD 相对指定基础 ref 的已提交变更文件列表，并与当前未提交变更混合展示为单一列表。已提交条目的对比语义唯一为 three-dot：先求 HEAD 与基础 ref 的 merge-base，文件差异为 merge-base 至 HEAD 的提交差异。未提交条目来源为「工作区状态查询」的暂存/未暂存/未跟踪三组，展开规则、状态颜色（已暂存成功绿 / 未暂存 teal / 未跟踪琥珀）、勾选与点击行为 MUST 与未提交变更视图一致（未提交条目在分支对比视图中等同于对已提交文件的新修改，保持完整编辑/批注/提交能力）。已提交条目与未提交条目 MUST 混合为单一列表并按路径 UTF-8 byte-wise 字典序统一排序；同一路径可同时存在已提交条目与未提交条目（相互独立、不得去重），**同路径多条目顺序固定为：已提交条目 → 已暂存 → 未暂存 → 未跟踪**；已提交条目 MUST 与未提交条目视觉可区分（具体颜色由设计确定），且无暂存状态概念、为只读。基础 ref MUST 先经 `git rev-parse --verify --end-of-options` 解析校验（与既有 ref 安全约束一致），解析失败 MUST 返回 invalid_input 类明确错误且不执行后续 git 读操作；merge-base 不存在（无共同祖先）或 HEAD 无法解析（仓库无任何提交）时 MUST 返回明确错误，MUST NOT 降级为空列表或全仓差异。已提交条目 MUST 含路径与增删行数；二进制文件按既有口径标记 IsBinary 且不计行；已提交条目 MUST 由服务端按路径字典序（byte-wise 字符串比较）排序后返回，多次请求顺序一致；变更文件数上限与超限错误语义 MUST 与「工作区状态查询」一致（默认 10000，超限返回明确错误而非截断）。该查询为只读操作，MUST NOT 修改 git index 或工作区，MUST NOT 进入 repo 写锁。

#### Scenario: 查看分支对比文件列表

- **WHEN** 用户在分支对比视图指定合法基础 ref
- **THEN** 系统返回 merge-base 至 HEAD 的变更文件列表（含增删行数）；API 响应仅含已提交子集，GitPanel 将其与未提交条目合成为用户可见的混合列表

#### Scenario: 未提交变更混合展示

- **WHEN** 工作区存在未提交修改或未跟踪文件，且分支对比视图已加载已提交差异
- **THEN** 未提交条目与已提交条目混合在同一列表按路径排序展示，未提交条目携带暂存状态颜色，点击后打开既有未提交 diff 来源且编辑/批注/提交能力可用

#### Scenario: 基础 ref 非法

- **WHEN** 指定的基础 ref 无法经 rev-parse 解析
- **THEN** 返回 invalid_input 类明确错误，未执行后续 git 读操作

#### Scenario: 无共同祖先

- **WHEN** HEAD 与基础 ref 不存在 merge-base
- **THEN** 返回明确错误，不展示空列表或全仓差异

### Requirement: 分支对比文件 diff 查看

系统 SHALL 提供分支对比视图的单文件两侧版本内容查询：旧侧为 merge-base 版本、新侧为 HEAD 版本，两侧均为已提交内容。存在性语义：文件在 merge-base 不存在 → 旧侧存在性=false（纯新增）；文件在 HEAD 不存在 → 新侧存在性=false（纯删除）。内容处理管线（原始字节读取 → NUL 二进制嗅探 → UTF-8 规范化 → rune 边界 524288 字节上限与 truncated 语义）、响应字段契约（oldContent/newContent/oldExists/newExists/oldMode/newMode/isBinary/truncated 八字段恒全量返回）与前端渲染派生状态及优先级 MUST 与「文件 diff 查看」「diff 视图渲染」逐字一致。blob 内容读取 MUST 以存在性探测取得的 blob OID 为读取对象，MUST NOT 以 `<ref>:<path>` 形式二次拼路径读取；存在性探测 MUST 使用 `:(literal)` pathspec 包裹 path 并逐条核对返回记录的路径与对象类型（与「文件 diff 查看」ref 分支同一判定口径）。`path` MUST 属于同一 `(mergeBaseOID, headOID)` 计算出的变更路径集合；不属于时 MUST 返回 invalid_input 类明确错误，且 MUST 在任何 blob 内容读取前拒绝（防止借该端点读取未变更的任意已提交文件）；rename 条目按列表展示的新路径判定归属，且 rename 条目的两侧内容 MUST 均按该新路径读取（旧侧该路径不存在时按纯新增展示），MUST NOT 以旧路径读取或传递旧路径。该查询为只读操作，MUST NOT 修改 git index 或工作区，MUST NOT 进入 repo 写锁。已提交条目（three-dot 差异）为只读：MUST NOT 提供编辑入口与编辑资格预取，MUST NOT 提供源码行级/块级批注手势与批注标记，MUST NOT 提供批注的创建、编辑、删除与提交入口。分支对比视图中的未提交条目不在此限：其 diff 来源、编辑、批注与提交能力 MUST 与未提交变更视图逐字一致（编辑写回、批注身份三元组 `(path, ref, untracked)`、离开守卫语义均不变）。

#### Scenario: 查看分支对比文件 diff

- **WHEN** 用户在分支对比视图选择某个变更文件
- **THEN** 系统返回旧侧（merge-base 版本）与新侧（HEAD 版本）内容，前端渲染 merge 对比视图

#### Scenario: 分支新增文件

- **WHEN** 文件在 merge-base 不存在、在 HEAD 存在
- **THEN** 旧侧存在性=false、内容为空，前端渲染为全部新增行视图

#### Scenario: 分支删除文件

- **WHEN** 文件在 HEAD 不存在、在 merge-base 存在
- **THEN** 新侧存在性=false、内容为空，前端渲染为全部删除行视图

#### Scenario: 只读视图无编辑与批注

- **WHEN** 用户在分支对比视图查看某个**已提交条目**的文件 diff
- **THEN** 视图不提供编辑入口与批注手势，不展示批注标记

#### Scenario: 分支视图未提交条目保持完整能力

- **WHEN** 用户在分支对比视图选择某个**未提交条目**
- **THEN** 打开的 diff 来源与未提交变更视图一致（工作区 vs HEAD/index/未跟踪），编辑、批注与提交能力均可用，行为与未提交变更视图一致

#### Scenario: 路径不属于变更集合

- **WHEN** 请求的文件路径不在该次 `(mergeBaseOID, headOID)` 的变更路径集合中（含未变更的已提交文件）
- **THEN** 返回 invalid_input 类明确错误，未执行任何 blob 内容读取

### Requirement: 分支对比视图入口与基础 ref 选择

Git 面板 SHALL 提供「未提交变更」与「分支对比」两个并存视图，用户 MUST 可切换；未提交变更视图的行为 MUST 与既有三组 diff 来源语义一致，不受分支对比能力影响。分支对比视图的文件列表为已提交差异与未提交变更的混合单列表（语义见「分支对比变更文件列表」）；commit 勾选与提交输入区在分支对比视图中对未提交条目可用，行为与未提交变更视图一致；批注管理面板在分支对比视图中可用，展示任务全部活动批注（与既有 diff-annotations 规范一致——文件从 status 消失后的 stale 批注仍可见、可编辑、可提交，两视图行为一致）；已提交条目的 diff 视图不接入批注能力（见「分支对比文件 diff 查看」）。分支对比视图 MUST 提供基础 ref 输入：默认值取任务的 `base_ref`，用户 MUST 可修改为任意合法 ref（本地分支、远端分支、tag 或 commit OID）；任务 `base_ref` 为空时默认值为空，用户 MUST 手动填写合法 ref 后方可查看分支对比的已提交部分（未提交条目的展示不依赖基础 ref）。基础 ref 变更后 MUST 清空当前选中的已提交条目 diff，并重新获取已提交变更文件列表（未提交条目不受影响）。输入生效时机 MUST 为失焦或 Enter（MUST NOT 每次按键即触发请求）；生效前 MUST 对输入做 trim；trim 后为空时 MUST 回落任务 `base_ref` 默认值重新加载（任务无 `base_ref` 时不对已提交部分发请求，清空已提交条目与对应 diff/错误状态并提示需手动填写，在途旧响应 MUST 丢弃）；任务 `base_ref` 数据后续刷新 MUST NOT 覆盖用户会话内手工输入的值（仅用户从未手改时跟随 `base_ref` 默认值）。视图选择与已输入的基础 ref MUST 在 Git 面板会话内保留，会话结束丢弃、MUST NOT 持久化。切换视图 MUST 视为离开当前 diff 视图：未保存编辑的离开确认语义与「编辑模式」既有离开守卫一致（在途写入失败或用户取消时 MUST NOT 完成切换）。

#### Scenario: 默认取任务 base_ref

- **WHEN** 任务配置了 `base_ref`，用户首次进入分支对比视图
- **THEN** 基础 ref 输入默认填入该 `base_ref` 并加载对比结果

#### Scenario: base_ref 为空需手填

- **WHEN** 任务无 `base_ref`，用户进入分支对比视图
- **THEN** 基础 ref 输入为空，用户手动填写合法 ref 后才展示变更文件列表

#### Scenario: 修改基础 ref 刷新

- **WHEN** 用户将基础 ref 修改为另一合法 ref
- **THEN** 变更文件列表与文件 diff 按新 merge-base 重新获取

#### Scenario: 输入生效时机与空值回落

- **WHEN** 用户编辑基础 ref 输入后按 Enter 或使输入框失焦
- **THEN** 新值生效并重新获取列表；输入过程中（未生效）不触发任何请求；输入清空后生效时回落任务 `base_ref` 默认值（任务无 `base_ref` 时不发请求并提示需手动填写）

#### Scenario: 切换两个视图

- **WHEN** 用户在未提交变更视图与分支对比视图之间切换
- **THEN** 两视图各自保持其文件列表与选中状态，未提交视图行为与既有语义一致

## MODIFIED Requirements

### Requirement: repo 项目 local-path 模式任务的 git 能力

repo 项目 local-path 模式任务 SHALL 开放任务级 git 管理：status / diff / diff review / commit / push / diff 视图内文件编辑读写 / 分支对比，操作对象为项目目录（`worktree_path` 即项目路径）当前 checkout 的**当前分支**。`assertGitRepoTask` 门禁按 D2 有效模式放行（repo + local-path 通过），非法 kind/mode 组合仍 internal fail-closed。未提交 diff 来源为 GitPanel 三组（对比 HEAD/index）：`ref=HEAD`（已暂存，工作区 vs HEAD）、`ref=''`（未暂存，工作区 vs index）、`untracked`（未跟踪文件），语义不变。分支对比视图 SHALL 对 local-path 模式任务开放，行为契约见「分支对比变更文件列表」「分支对比文件 diff 查看」「分支对比视图入口与基础 ref 选择」，对比 ref 的解析对象为项目目录当前 checkout 所在仓库；分支对比视图按条目类型门控：已提交条目只读，未提交条目提供文件编辑写回。commit SHALL 提交到当前分支 HEAD；push SHALL 为 `git push -u origin <当前分支>` 且 MUST NOT force-push。diff 视图内文件编辑读写 SHALL 经 /git/file 接线与 diffreview_fileedit.go 同一门禁放行：读（ReadRaw）落点为项目目录，编辑写回落点为项目目录当前 checkout；编辑能力仅适用于未提交条目（两视图一致），MUST NOT 对已提交条目提供。风险归属（显式契约）：操作对象是用户主仓库当前分支，改动就地生效、提交/推送/文件编辑写回直接作用于用户分支与工作区；dirty 混杂与直接修改/提交/推送主仓库的风险由用户自担（与 local 模式共享目录语义一致）。

#### Scenario: local-path 模式任务 git 操作放行且作用于项目目录

- **WHEN** 对 repo 项目 local-path 模式任务调用 git status/diff/commit/push 任一 API
- **THEN** 系统放行并在项目目录当前 checkout 上执行（`worktree_path` 即项目路径），无 base_ref 依赖；commit 落到当前分支 HEAD，push 执行 `git push -u origin <当前分支>` 且 MUST NOT force-push

#### Scenario: local-path 模式任务 diff 来源为 GitPanel 三组（仅未提交部分）

- **WHEN** repo 项目 local-path 模式任务在 GitPanel 查看未提交变更视图的 diff
- **THEN** diff 来源为三组真值：`ref=HEAD`（已暂存，工作区 vs HEAD）、`ref=''`（未暂存，工作区 vs index）、`untracked`（未跟踪文件），对比 HEAD/index，语义不变

#### Scenario: local-path 模式任务分支对比视图可用

- **WHEN** repo 项目 local-path 模式任务在 GitPanel 切换到分支对比视图
- **THEN** 分支对比可用：已提交部分为项目目录当前 checkout HEAD 相对基础 ref 的 three-dot 差异，并与未提交条目混合展示（未提交条目保持完整编辑/批注/提交能力）

#### Scenario: local-path 模式任务 diff review 放行

- **WHEN** repo 项目 local-path 模式任务发起 diff review（DiffSourcePortAdapter.ReadLocked 同一门禁，来源 `(ref, path, untracked)` 由 UI 传入）
- **THEN** 门禁放行，review 基于项目目录当前 checkout 的指定来源执行

#### Scenario: local-path 模式任务 diff 视图内文件编辑读写放行

- **WHEN** repo 项目 local-path 模式任务在 GitPanel diff 视图内读取（ReadRaw）或编辑写回文件（/git/file 接线，经 diffreview_fileedit.go 同一门禁）
- **THEN** 读取与写回落点均为项目目录当前 checkout（`worktree_path` 即项目路径），改动就地生效；风险由用户自担

#### Scenario: 分支对比视图已提交条目只读

- **WHEN** repo 项目 local-path 模式任务在分支对比视图查看**已提交条目**的文件 diff
- **THEN** 该条目不提供编辑入口与批注手势；未提交条目的文件编辑读写能力在分支对比视图与未提交变更视图中均可用，行为一致

#### Scenario: detached HEAD 边界

- **WHEN** 项目目录当前分支被外部切为 detached HEAD 后，用户查看 status 或执行 push
- **THEN** status 的分支显示留空、不阻断（gitops.go:74 现状语义）；push 失败并透传 git 错误（worktree.go:236 现状语义），MUST NOT 伪装成功

#### Scenario: local-path 模式任务的 UI 展示

- **WHEN** 用户在 Web UI 打开 repo 项目 local-path 模式任务
- **THEN** Git tab 与 git 面板入口可见（isGitless 仅对 dir 项目成立）；工作台页头不展示任务分支名（`task.branch` 恒为空），GitPanel 内展示实时分支（status.branch）

#### Scenario: 任务行分支显示维持隐藏

- **WHEN** 用户在指挥中心或项目页查看 repo 项目 local-path 模式任务行
- **THEN** 任务行不展示分支名（维持 gitless 隐藏不变）
