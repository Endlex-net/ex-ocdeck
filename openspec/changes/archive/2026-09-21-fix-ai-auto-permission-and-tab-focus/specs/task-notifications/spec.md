## MODIFIED Requirements

### Requirement: 通知触发——等待权限批准（permission）

当任务（处于 active 状态）新增 pending permission 注意力时，系统 SHALL 触发 permission 类别通知。通知详情 MUST 包含权限名称与模式（patterns）。去重键、重新武装与读失败语义与 question 一致（按（注意力类型, request ID），与 question 的去重键相互独立）。

当前有效权限行为为 `ai-auto`（即 `effective_permission_mode` 为 `ai-auto`，见 task-permission-mode spec「权限模式创建后变更」——以 `all-approve` 启动的运行进程保存为 `ai-auto` 时有效模式仍为 `all-approve`，MUST NOT 误入本延迟语义）的任务适用延迟触发语义：新增 pending permission 时 MUST NOT 立即触发通知，系统转为等待该请求的 AI 判定结论——判定放行且回复已被受理、或回复时请求已被了结（正常竞态）时 MUST NOT 触发通知（即使 attention 瞬时尚未收敛）；判定结论为拒绝、无法识别、判定失败，或判定放行但回复结果未知/回复能力不可用/回复未实际发送（即转为需要用户决策）时系统 SHALL 立即触发通知；判定在系统界定的等待超时窗口（自首次观察到该 pending 起算，重复观察 MUST NOT 延长，参数见 design）内仍未终结时，系统 SHALL 兜底触发通知。触发前系统 MUST 复核该请求仍为 pending 且未被自动放行；已了结或已自动放行的请求 MUST NOT 触发。去重作用域为（runtime 实例, request ID）：同一 runtime 实例内同一 request ID 最多触发一次，对两种模式一致；runtime 换代（含 server 重启后重建 runtime）后仍 pending 的同一 request ID 允许再次按本语义评估通知，server 重启本身的启动基线语义（不补发）见「通知抑制、启动基线与对账」不变。有效模式非 `ai-auto` 的任务的触发语义 MUST 保持现状（新增 pending 即触发）。任务有效模式由 `ai-auto` 变更为其他模式后，仍在等待判定结论的请求按非 `ai-auto` 语义处理。

#### Scenario: 新增 pending permission 触发通知

- **WHEN** 非 `ai-auto` 的 active 任务的 attention 快照中出现新的 pending permission（新 request ID）
- **THEN** 系统触发一次 permission 类别通知，通知详情含权限名称与 patterns

#### Scenario: ai-auto 自动放行不通知

- **WHEN** `ai-auto` 的 active 任务出现新的 pending permission，随后该请求被 AI 自动放行（了结）
- **THEN** 系统 MUST NOT 触发该 request ID 的 permission 通知

#### Scenario: ai-auto 判定转人工立即通知

- **WHEN** `ai-auto` 的 active 任务出现新的 pending permission，AI 判定结论为拒绝、无法识别或判定失败（转人工）
- **THEN** 系统立即触发一次 permission 类别通知，提示用户决策

#### Scenario: ai-auto 判定超时兜底通知

- **WHEN** `ai-auto` 的 active 任务出现新的 pending permission，超过等待超时窗口仍未得到判定结论且该请求仍 pending
- **THEN** 系统兜底触发一次 permission 类别通知，提示用户决策

#### Scenario: 同一 permission 不重复通知

- **WHEN** 同一 runtime 实例内某 request ID 的 pending permission 已触发过通知且仍 pending
- **THEN** 系统 MUST NOT 再次触发该 request ID 的通知（runtime 换代后仍 pending 的同一 request ID 允许重新评估通知）

### Requirement: 通知抑制、启动基线与对账

各触发器的计时与抑制状态 MUST 为进程内存态，MUST NOT 持久化。事件处理、计时届满判定与投递复验 MUST 在单一串行化上下文中执行（单 run loop），对任务运行信息的读取 MUST 为单次组合快照（含任务行、attention、run_status、retry 详情），MUST NOT 分次独立读取。残余竞态窗口（投递副作用执行期间到达新事件）的后果上限为：每个受影响的触发条件可能误发或漏发一次，为已接受语义，MUST NOT 为此引入 revision/代际机制。启动基线语义：ocdeck 启动时已是 pending 的 attention MUST 只用于播种去重集合、不补发通知；启动时已是 idle 的任务 MUST NOT 武装 idle 计时；启动时已处于 retry 的任务 MUST 从启动时刻起重新计时 1 分钟；error 计时 MUST NOT 从启动前的历史恢复。启动基线的 active 任务枚举失败时 MUST 记错误日志并整体禁用通知触发（配置 API 与测试通知仍可用）。事件订阅溢出（丢事件）后的对账 MUST 保留已通知集合与去重集合，仅以当前快照重建计时基准：对账发现的未消费 pending attention MUST 仅播种去重集合、不补发通知（与启动基线一致）；MUST NOT 重发已消费的条件。**例外（ai-auto 延迟触发）：对账发现的有效模式为 `ai-auto` 的任务 pending permission 既未通知也无等待判定条目时（attention/判定结论事件在等待建立前丢失），MUST NOT 直接播种去重——系统 MUST 为其建立等待判定条目并以本次对账观察时刻为超时起算，后续按「通知触发——等待权限批准（permission）」延迟触发语义处理。对账时已存在的等待判定条目仍 pending 时 MUST 按对账快照定论：当前有效模式非 `ai-auto`（如模式变更事件因溢出丢失）时，系统 MUST 在完整对账成功并退出对账中状态后立即按正常门禁投递（请求仍 pending 且未通知）；当前有效模式仍为 `ai-auto` 时保留原超时起算时刻，并按快照中的判定状态立即重评（需人工即通知、已自动放行清除、未定论继续等待）。仅「此前无等待判定条目」的普通非 `ai-auto` pending 与 question 沿用仅播种去重不补发。**对账期间枚举或快照读取失败 MUST 进入对账中状态：抑制全部发送并周期重试，成功重建后恢复。任务离开 active 状态时 MUST 取消该任务全部待决计时且不再触发通知。

#### Scenario: 重启后 retry 重新计时

- **WHEN** 任务处于 retry 已达 50 秒时 ocdeck 重启
- **THEN** 重启后从启动时刻重新计时 1 分钟，不基于重启前时长补发

#### Scenario: 启动时不补发已 pending 的 attention

- **WHEN** ocdeck 启动时任务已存在 pending question
- **THEN** 该 request ID 进入去重集合但不触发通知

#### Scenario: 任务挂起取消待决通知

- **WHEN** 任务在 idle 计时未届满时被挂起
- **THEN** 计时器取消，不再触发该任务的 idle 通知

#### Scenario: 事件在等待建立前溢出丢失

- **WHEN** `ai-auto` 任务的 pending permission 相关事件在通知层建立等待判定条目前因订阅溢出丢失，随后对账发现该 pending 既未通知也无等待条目
- **THEN** 系统建立等待判定条目并以对账观察时刻为超时起算，后续按延迟触发语义处理（不直接播种去重）

#### Scenario: 模式变更事件溢出丢失后既有等待条目仍通知

- **WHEN** `ai-auto` 任务存在等待判定条目，随后任务切出 `ai-auto`（模式变更事件因订阅溢出丢失），对账发现该等待条目仍 pending 且当前有效模式非 `ai-auto`
- **THEN** 系统在完整对账成功后立即按正常门禁投递该请求的 permission 通知（不等原超时、不播种去重压掉）
