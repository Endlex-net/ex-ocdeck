# P1.7.3 预登记协议（预登记，实施前冻结）

本文档在运行动态可靠性实验**之前**写定。只允许写 schema、目标转换序列、每转换匹配窗口、最短稳定时长与判据；**不得包含运行时值**（sessionID/时间戳由运行时 realized ledger 记录）。看到结果后 MUST NOT 回调参数。

## 环境

- 专用实例：`opencode serve` 127.0.0.1:50300（Basic auth `opencode:p17test`），cwd=专用测试 worktree。
- mock provider 桩：127.0.0.1:50401，模式经 `stub_mode` 文件热切换：`fast`（立即短回答）/ `slow`（~5s 回答）/ `r429`（HTTP 429 + Retry-After:1）。
- 单测试 session：实验开始时创建一次，其 sessionID 作为全程关联标识；事件流按 `properties.sessionID` 过滤。

## Transition ledger schema

每次**预期**转换一行 JSON：

```json
{"seq": <int 单调>, "sessionID": "<realized>", "target_status": "busy|retry|idle", "ts_ms": <int>}
```

`ts_ms` 为驱动脚本发出该转换驱动动作的时刻。realized ledger 随原始记录归档。

## 事件原始记录

- 事件流 JSONL：`{"ts_ms", "type", "sessionID", "status"}`（`status` = `properties.status.type` 原值；非本 session 或非 session.status 事件也记录，`status` 可空）。
- oracle 采样 JSONL：每次迁移停留期内以 500ms 间隔轮询 REST `/session/status`，记 `{"ts_ms", "sessionID", "status"}`（idle 缺席记 `"absent"`，语义=idle）。

## 转换序列

### S1 稳定序列 ×3 轮

每轮依次（上轮结束即下轮开始）：

| seq 模 4 | 驱动动作 | target_status | 最短稳定时长 | 匹配窗口 |
|---|---|---|---|---|
| 0 | stub_mode=slow；发送短 prompt | busy | ≥3s（slow 回答约 5s 覆盖） | 驱动后 ≤3s 内出现 |
| 1 | （等待 slow 完成） | idle | ≥3s | busy 事件后 ≤8s 内出现 |
| 2 | stub_mode=r429；发送短 prompt | retry | ≥3s（attempt 1..5 约 1s 间隔覆盖） | 驱动后 ≤3s 内出现 |
| 3 | （等待重试耗尽失败 / 或 stub_mode=fast 后重发成功） | idle | ≥3s | 最后一个 retry 事件后 ≤10s 内出现 |

### S2 burst 序列 ×5 次（mock 桩驱动）

stub_mode 在 `fast`/`r429` 间交替，驱动脚本以 ≤1s 间隔连续完成 busy→retry→idle 转换 5 次：

- busy：fast 模式下发送 prompt（busy 可能极短，允许完全落在两次 oracle 采样之间——不构成漏帧证据，见判据）
- retry：r429 模式下发送 prompt（首个 retry 事件即满足）
- idle：fast 模式下发送 prompt 并完成

burst 每次转换 ledger 匹配窗口 ≤2s，不设最短稳定时长。

### S3 断流重连

制造一次 SSE 断流（kill/重启事件消费端连接或重启 serve 进程后重连事件流），随后重放 S1 稳定序列 1 轮。重连后 5s 内事件流与 oracle 须收敛一致。

## 判据（任一满足即门禁失败）

1. **完整性/顺序**：稳定序列与 burst 的每个 ledger 转换，在事件流中存在同 sessionID、status==target_status 的事件，且落在匹配窗口内、顺序与 ledger 一致；缺失或乱序即失败。
2. **字段漂移**：事件缺少 `properties.sessionID` 或 `properties.status.type`，或 type 出现非 `{idle,busy,retry}` 的值（未知值视为漂移）。
3. **burst 迟到事件**：burst 结束（最后一个 ledger 转换）后 2s 观察窗内，出现把状态改离 ledger 最终目标值的迟到旧事件即失败。
4. **重连收敛**：断流重连后 5s 内事件流未收敛至 oracle（REST）状态即失败。
5. **retry 可制造性**：若 `r429` 模式下 retry 状态未出现（稳定序列 seq≡2），门禁直接判失败并转模式 B（P1.7.4），MUST NOT 跳过该状态。

oracle（REST 轮询）仅作收敛交叉验证，不作为逐转换判据。

## 门禁判定（P1.7.4）

- 全部通过 → 模式 A（事件驱动）。
- 任一失败 → 模式 B（后台低频探测缓存，design D4）。
- MUST NOT 发明第三种方案或混搭。选择结果与判据记录于 design.md 与 PR 描述。
