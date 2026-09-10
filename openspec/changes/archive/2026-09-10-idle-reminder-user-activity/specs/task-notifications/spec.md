## MODIFIED Requirements

### Requirement: 通知触发——空闲超时（idle）

idle 计时 SHALL 且仅由满足以下全部条件的 `run_status_changed` 事件武装：`available=true`、`from=busy`、`to=idle`。武装时刻（idleSince）为进入 idle 的时刻；每次判定 MUST 按最新配置计算 `idleSince + 空闲阈值`（默认 60 秒，可配置）：缩短阈值 SHALL 在下一判定周期即可到期，延长阈值 SHALL 顺延。届满触发一次 idle 类别通知。武装后的计时在以下任一情况 MUST 取消且本周期不再触发：任务回到非 idle；聚合可用性变为不可用；出现任意 pending question/permission；进入异常周期（retry/error）；任务离开 active；系统处理该任务的主动操作信号时该 idle 计时仍未被消费（识别规则见「用户主动操作识别」；判定以系统串行处理顺序为准：信号先被处理则取消计时，计时先被判定消费则本周期结果不变，近边界一次误发或漏发为已接受语义，见「通知抑制、启动基线与对账」）。通知后 MUST 进入抑制态，仅当出现新的满足武装条件的 busy→idle 迁移时才重新武装。`from` 为不可用态（空串）或 `from=retry` 的迁移 MUST NOT 武装 idle 计时。

#### Scenario: 忙后空闲超时触发通知

- **WHEN** 任务聚合状态从 busy 迁移到 idle（available=true），持续超过空闲阈值未回到非 idle，期间无 pending 注意力、未进入异常周期且未观察到该任务的主动操作
- **THEN** 系统触发一次 idle 类别通知

#### Scenario: 有 pending 注意力时取消 idle 计时

- **WHEN** 任务 busy→idle 已武装计时，计时期间出现 pending question
- **THEN** idle 计时取消，本周期不触发 idle 通知

#### Scenario: 不可用恢复为 idle 不武装

- **WHEN** 任务聚合状态从不可用（from 为空串）恢复为 idle
- **THEN** 系统 MUST NOT 武装 idle 计时，不触发 idle 通知

#### Scenario: 启动时已是 idle 不通知

- **WHEN** ocdeck 启动后，任务聚合状态一直是 idle（未观察到满足条件的 busy→idle 迁移）
- **THEN** 系统 MUST NOT 触发 idle 通知

#### Scenario: 通知后重新武装

- **WHEN** 任务已触发 idle 通知，之后出现新的 busy→idle（available=true）迁移并再次超过阈值
- **THEN** 系统重新触发 idle 类别通知

#### Scenario: 缩短阈值立即到期

- **WHEN** 任务已武装 idle 计时（idleSince 为 3 分钟前），用户将空闲阈值从 300 秒改为 60 秒并保存
- **THEN** 下一判定周期按新阈值计算（3 分钟 > 60 秒），idle 通知触发

#### Scenario: 延长阈值顺延

- **WHEN** 任务已武装 idle 计时（idleSince 为 50 秒前），用户将空闲阈值从 60 秒改为 300 秒并保存
- **THEN** idle 通知顺延至 idleSince + 300 秒才触发

#### Scenario: 等待窗口内主动操作取消 idle 计时

- **WHEN** 任务 busy→idle 已武装 idle 计时，系统处理该任务的主动操作信号时计时仍未被消费（识别规则见「用户主动操作识别」）
- **THEN** idle 计时取消，本周期不触发 idle 通知

#### Scenario: 计时先被消费则主动操作不改变本周期结果

- **WHEN** 任务的 idle 计时已在判定周期被消费（该次触发按既有门禁语义进入处理），之后才处理到该任务的主动操作信号
- **THEN** 本周期结果不变，该主动操作信号不产生额外效果

#### Scenario: 主动操作先于武装被处理不缓存

- **WHEN** 系统处理某任务的主动操作信号时，该任务尚无已武装的 idle 计时（如信号先于满足武装条件的 busy→idle 迁移被处理）
- **THEN** 该信号被丢弃、不缓存；之后的武装迁移照常起算计时

#### Scenario: 武装后持续触摸移动取消计时

- **WHEN** 任务的主动触摸翻阅在 idle 计时武装前已开始（起始事件被处理并丢弃、不缓存），武装后用户继续主动移动，系统处理后续移动事件时该计时仍存在
- **THEN** idle 计时取消，本周期不触发 idle 通知

#### Scenario: 主动操作取消后持续空闲不再触发

- **WHEN** 任务的 idle 计时因主动操作被取消，之后任务持续保持 idle 且未出现新的满足武装条件的 busy→idle 迁移
- **THEN** 无论空闲持续多久，系统 MUST NOT 触发 idle 通知

#### Scenario: 其他任务的主动操作不影响本任务计时

- **WHEN** 任务 A 已武装 idle 计时，计时期间仅观察到任务 B 的主动操作
- **THEN** 任务 A 的 idle 计时不受影响，届满照常触发

#### Scenario: 通知已触发后主动操作不改变抑制态

- **WHEN** 任务已触发 idle 通知进入抑制态，之后观察到该任务的主动操作
- **THEN** 抑制态不变，仅新的满足武装条件的 busy→idle 迁移才重新武装

## ADDED Requirements

### Requirement: 用户主动操作识别

系统 SHALL 识别按任务归属的用户主动操作信号，来源限定为两类：

1. 终端/shell 页面的键盘输入：经已认证、已建立桥接的终端 WebSocket 通道收到的非空输入字节（含粘贴等文本输入帧）。识别 MUST NOT 以写入 PTY 成功为前提——写入失败不撤销已观察到的用户输入。窗口尺寸调整（resize）等控制帧与空帧 MUST NOT 计为键盘输入。
2. git 页面区域内的用户主动交互：点击、滚轮/触摸翻阅、键盘翻页及编辑输入。前端自动加载、定时或自动刷新、程序滚动及其他非用户触发的事件 MUST NOT 计为主动操作。

主动操作 MUST 携带任务归属（操作发生于哪个任务，取操作发生时的任务）；其他任务的主动操作 MUST NOT 影响本任务的 idle 计时。主动操作仅作用于 idle 类别计时，MUST NOT 影响 question / permission / retry / error 类别。未处于已武装 idle 计时状态的任务上的主动操作 MUST 无效果（不缓存、不延后生效）。

#### Scenario: 终端键盘输入计为主动操作

- **WHEN** 已认证、已建立桥接的终端/shell WebSocket 通道收到该任务的非空输入字节（含粘贴）
- **THEN** 识别为该任务的主动操作

#### Scenario: PTY 写入失败不撤销已观察到的输入

- **WHEN** 已建立桥接的终端通道收到非空输入字节，随后写入 PTY 失败
- **THEN** 仍计为该任务的主动操作

#### Scenario: resize 控制帧与空帧不计为主动操作

- **WHEN** 终端窗口尺寸变化产生 resize 控制帧，或通道收到空输入帧
- **THEN** 不计为主动操作

#### Scenario: git 页面用户主动交互计为主动操作

- **WHEN** 用户在任务的 git 页面区域内点击、滚轮/触摸翻阅、键盘翻页或编辑输入
- **THEN** 识别为该任务的主动操作

#### Scenario: 自动加载、刷新与程序滚动不计为主动操作

- **WHEN** 前端自动加载、定时/自动刷新或程序滚动产生 git 区域事件或请求
- **THEN** 不计为主动操作

#### Scenario: 无已武装 idle 计时任务的主动操作无效果

- **WHEN** 任务当前没有已武装的 idle 计时，观察到该任务的主动操作
- **THEN** 该主动操作不产生任何通知相关效果（不缓存、不延后生效）
