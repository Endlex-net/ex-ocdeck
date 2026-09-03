# Tasks: diff-review-workbench

实现顺序与依赖遵循 design D9 五 lane；每 lane 前序未完成不进后续。所有新增行为测试必须满足实现者自检（旧实现失败、新实现通过）。契约唯一来源：design.md D1-D9 与 specs。

## 1. store 层（migration 0012 + 查询层）

- [x] 1.1 新增 `internal/infrastructure/store/migrations/0012_diff_annotations.sql`：三表（diff_annotations / diff_review_submissions / diff_review_submission_items），字段、索引、UNIQUE 约束与 design D3 逐字一致
- [x] 1.2 查询层原语（全部列出，无遗漏）：diff_annotations CRUD（创建/更新评论 revision+1/删除/按 task 列表）；diff_review_submissions 创建（事务内写 items + revision 复核）、队列读取（queued/sending 按 seq ASC）、CAS 转移（queued→sending 条件 UPDATE、能力门禁 queued→failed、sending→failed、sending→delivery_unknown）、**sent 清理事务（sending→sent + 按 id+revision 删批注，不可拆分的唯一事务原语——不存在独立的 MarkSent 方法）**、撤回 DELETE（仅 queued）、终态删除（仅 sent/failed/delivery_unknown）、分区列表（history 按 sent_at DESC seq DESC、failures 按 created_at DESC seq DESC）、**全局启动收敛**（单事务全部 sending→delivery_unknown + 固定 error）
- [x] 1.3 store repository 原子性单测（仅存储层语义，调度编排归 3.11）：条件 UPDATE CAS 语义（matched/!matched 分类）、sent 清理事务原子性与 revision 语义（同秒编辑不误删、事务不可拆分）、message_id UNIQUE 碰撞、分区排序稳定性（同秒 seq 决胜）、启动收敛事务原子回滚与错误传播（不开放 API/调度器的编排断言归 3.11）

## 2. opencode 契约层

- [x] 2.1 CONTRACT.md 扩充契约锚点（prompt_async handler、PromptInput/MessageID schema、GET /doc），按升级 SOP 跑锚点 diff + live probe，扩展已验证区间（本机 1.18.26）
- [x] 2.2 `client.PromptAsync` 实现（opencode.PromptResult transport DTO、四 Kind 分类规则唯一：marshal/NewRequest→pre_send_failure；Do 错误一律 transport_unknown；204→accepted；其余一切状态码→http_response）
- [x] 2.3 能力探测**底层**（本任务只管请求与解析）：GET /doc 结构化解析 `paths["/session/{sessionID}/prompt_async"].post` + operationId `session.prompt_async`，返回三值 supported/unsupported/unknown（结果语义矩阵按 D1：200 路径存在/缺失或 operationId 不符/401/404/5xx/网络/畸形 JSON）
- [x] 2.4 `OCClient` 接口新增 PromptAsync（签名与 *Client 逐字一致）+ 编译期接口断言 + 全部 mock/fake 同步
- [x] 2.5 契约层测试（仅低层请求编码与解析）：PromptAsync 分类（204/意外 2xx/400/401/404/transport/pre-send）、messageID 请求体编码（msg_ 前缀透传）、/doc 解析（路径缺失/operationId 不符/401/畸形 JSON）、OCClient 接口断言与 mock 同步

## 3. application/task 层（diffreview service + 队列 + 编辑写回）

- [x] 3.1 GitDiff 重构：`Manager.GitDiff` 拆为公共加锁入口 + 已持锁核心 helper（六阶段顺序、八字段不变量原样保留，守卫测试先行）；核心 helper 内加 UTF-8 规范化管线（raw → NUL 嗅探 → ToValidUTF8 → rune 边界 524288 bytes，truncated 真值表按 delta spec）
- [x] 3.2 新包 `internal/application/diffreview`：service + 五个 consumer-owned ports（DiffReviewRepository / PromptPort / DiffSourcePort / RuntimePort / TaskScopePort），domain 类型 PromptOutcome/PromptOutcomeKind；service MUST NOT 反向依赖 task/infrastructure
- [x] 3.3 adapters（仅适配，不含业务规则）：SQLite DiffReviewRepository + TaskScopePort（store 包，调 1.2 原语）；PromptPort（task 层，opencode.PromptResult→PromptOutcome 显式逐字段映射，经 taskOcClient(taskID) 路由）；DiffSourcePort/RuntimePort（task 层）
- [x] 3.4 能力协调（task/application 层，D1 事件模型逐项落实）：runtime ready 首探；GET/annotations 与提交准入遇 absent/unknown 同步复探（singleflight 合并并发）；缓存绑定 instVersion（Suspend/重启/实例替换失效）；PromptAsync 400 或意外 2xx → 置 unknown 复探；404 → GetSession 穷尽分流（含端点不支持分支 sending→failed + 固定 error + 缓存转 unsupported + 零重投）
- [x] 3.5 批注用例：创建（来源组合约束、1-based 闭区间、窗口自洽校验）、列表（D4 stale 惰性计算：同源重读全窗口比对、截断窗口规则、读取失败=stale 不阻断）、编辑评论（空白拒绝/同值 revision 不变）、删除
- [x] 3.6 提交用例：准入（任务运行中/锚定/能力 supported/批次 id 分类优先级：任一跨任务→invalid_input，否则缺失或 revision 不符→conflict，与数组顺序无关）、payload 组装（D7 逐字公式、有界单遍算法、动态 fence、组合映射、截断公式）、单事务落库（queued + items 快照 + revision 复核）
- [x] 3.7 队列调度器：每任务循环（2s 轮询 SessionStatus，idle 或缺席可投递）、**按最小 seq 串行取队首**、能力门禁（CAS 前三分支含缓存已 unsupported 直接 failed）、CAS 抢占（与生命周期操作任务锁互斥）、发送（PromptOutcome 状态映射）、准备重试 ≤3、adapter 获取失败=pre_send_failure（固定 Detail "runtime client unavailable"）
- [x] 3.8 重启恢复两段式：服务启动收敛（调 1.2 全局收敛原语，sending→delivery_unknown，固定 error "delivery unknown after restart"，fail-closed，独立于 runtime）；runtime 启动恢复本任务 queued；调度器随 runtime 生命周期启动、Manager Shutdown join
- [x] 3.9 文件编辑读取：判定链（词法→tryLockTask→task 存在→worktree 非空→repo kind→Lstat 禁锢→regular→NUL 嗅探→524288→UTF-8→换行统一→owner 写位），判别联合响应（含 mode/hasBom/lineEnding/baseHash），reasonCode 七值枚举
- [x] 3.10 文件写回（接收已解码 DTO，负责领域格式校验 + 冲突检查 + D5 写盘 9 步）：禁锢复检→初检（hash/换行风格/mode）→BOM 推导+重建+524288 检查→临时文件+baseMode Chmod（含特殊位映射）+flush→终检（禁锢/regular/hash/mode）→原子 rename→返回新 baseHash；分阶段错误映射唯一
- [x] 3.11 task/application 测试（service/调度编排所有权；存储层原子性归 1.3、HTTP wire 归 4.4 不重复）：CAS 抢占编排与撤回竞态（service 方法竞态，不经 HTTP）、重启恢复两段式（runtime 不可用仍收敛；启动收敛写库失败 → API/调度器不开放 fail-closed）、至多一次全分支（204 后事务失败/意外 2xx/404 分流/adapter 获取失败）、messageID 生成/持久化/准备重试复用同一值、sent 清理 revision 语义（service 层）、65536 边界、formatter golden 全清单、UTF-8 规范化顺序、有界单遍内存行为（首个来源超预算后续 formatter 零调用）、payload 拼接 golden、能力事件模型（首探/复探 singleflight/instVersion 失效/门禁三分支）、批次混合错误优先级（跨任务+缺失同批交换顺序恒 invalid_input）、请求到达前批注已改版/已删除 → conflict、service 方法级并发编辑/删除批注 → conflict(409) 零副作用、PATCH 空白/同值 revision 不变、stale 算法（CRLF 快照不漂移、truncated 窗口完整保持 active、读取失败=stale 不阻断）、多来源同时失败返回排序最前错误、双任务路由隔离、写回禁锢/初检/终检/特殊位保真/只读拒绝/CRLF-BOM-末尾换行保真/content 含 CR 拒绝/lineEnding 冻结（CRLF 删除全部换行后新增换行仍按 crlf 重建、风格与冻结值不一致 409）、TaskScopePort 准入、`go test -race`

## 4. api 层

- [x] 4.1 `decodeBoundedJSON` 统一 helper（MaxBytesReader + 首值解码 + 强制 io.EOF + MaxBytesError 分类 → invalid_input）
- [x] 4.2 `internal/api/annotations.go`：批注 CRUD + submissions（提交/分区列表/撤回/删除历史）路由，DTO camelCase 与 D8 表逐字一致，wire 上限逐端点
- [x] 4.3 `git.go` 追加 GET/POST `/git/file` 路由：独占 wire 上限、JSON 完整解码与 HTTP 错误映射；解码成功后调用 3.9/3.10（domain 校验与写盘在 lane 3）
- [x] 4.4 api 测试：wire 上限临界 ±1 字节与 2×/6× 转义膨胀、超量尾随空白与第二 JSON 值拒绝、逐端点错误映射（含 not_found 仅任务、跨任务 id invalid_input、复核 conflict、revision 非法值 0/-1/小数/溢出 → invalid_input）

## 5. frontend 层

- [x] 5.1 `web/src/api.ts` + `types.ts`：新增端点客户端与类型（Annotation/Submission/SubmissionItem/编辑读取判别联合含 mode/hasBom/lineEnding）
- [x] 5.2 DiffViewer 查看模式批注手势：框选/点击创建（快照从原始 GitDiffResult 侧内容构造保留行尾字符、side 映射规则、禁止跨侧选区、空评论丢弃）；binary/gitlink/双侧不存在文件不提供手势（truncated 允许可见范围）；选区 decoration + gutter 聚合标记（同一行多条聚合带数量，悬停按 (createdAt,id) 排序显示摘要，点击定位并高亮全部匹配；标记按三元组隔离）
- [x] 5.3 DiffViewer 编辑模式（D5 前端协议状态机逐项落实）：**编辑入口门禁完整判定——渲染了 merge 视图、新侧存在、非 binary、非 truncated、非 symlink/gitlink、GET 编辑读取 editable=true，全部满足才提供编辑入口；不满足时显示明确原因**；可编辑扩展替换 readOnlyExtensions、debounce 500ms；每文件单在途写请求 + 冻结 sentContent + 串行合并；切换文件/退出编辑模式/还原前 flush 并等待在途；409 阻塞（保留内容+暂停自动写回+冲突提示）；结果未知 → 重读四元确认（content/hasBom/lineEnding 条件化/mode）→ 采用新基线并立即补发最新编辑或保持阻塞；显式放弃出口；lineEnding/hasBom/mode 冻结携带；还原入口（确认后按快照走写回端点，会话内有效）
- [x] 5.4 GitPanel 面板：selFile 扩展为 (path,ref,untracked) 三元组（高亮与 decoration 按三元组匹配）；批注列表（查看/编辑评论/删除，stale 漂移标识在列表与行内标记双处展示）；提交预览弹层（条目临时移除+补充说明，携带 id+revision，409 时保留弹层刷新批注后重新确认）；queue/history/failures 分区视图（撤回仅 queued、删除历史仅终态）；submitCapability 非 supported 禁用提交并提示；busy 编辑警告横幅
- [x] 5.5 视觉与交互定稿由 designer 完成（批注列表布局、预览弹层、标记样式、警告横幅）
- [x] 5.6 前端交互测试（可重复执行）：批注创建/编辑/删除/stale 双处展示、CRLF 选择后提交的 snapshot 保留 `\r`（前端快照构造自原始 diff 数据而非编辑器态）、视图三元组隔离（同路径 staged+unstaged 批注标记互不串扰）、预览临时移除与版本冲突重确认、撤回与终态删除的状态限制、编辑入口禁用（truncated/binary/gitlink/symlink/新侧缺失/GET 不可编辑）、lineEnding 冻结（CRLF 文件删除全部换行后新增换行，写请求仍携带冻结 crlf）、编辑写回串行与 409/未知恢复、还原；构建通过

## 6. 全量验证检查点（lane 5 完成后）

- [x] 6.1 `go test -race` 全量通过 + 前端构建通过
- [x] 6.2 端到端走通：diff 页批注 → 多轮累积 → 预览提交 → task agent 会话收到并修复 → 历史可见；编辑模式修改实时生效 + 还原可用
- [x] 6.3 非门禁诊断：payload 最坏输入 benchmark 与内存 benchmark 结果记录
