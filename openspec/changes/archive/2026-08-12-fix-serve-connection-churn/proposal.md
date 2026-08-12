# Proposal: fix-serve-connection-churn

## Why

2026-08-11 下午 ocdeck 发生一次生产故障：server 进程未崩溃，但 17:06–18:21 之间所有新 task 激活失败（`capability probe (serve not ready)` / `serve not ready: health check timeout`），重启后才恢复。重启时 reconcile 对**仍然存活**的 serve（50004）做健康检查，dial 报 `connect: can't assign requested address`（EADDRNOTAVAIL）——即 loopback 临时端口/套接字资源耗尽，所有发往 127.0.0.1:500xx 的新连接全部失败。

根因是 occlient 的资源管理缺陷叠加一个重试逻辑 bug：

1. **每个 `NewClient` 实例新建独立 `http.Transport`，无跨实例连接池复用**（`internal/opencode/client.go` `NewClient` 内 `loopbackTransport := &http.Transport{Proxy: nil}`）：AgentStatus 每次查询、reconcile/attention 每个调用点都新建 client 实例（=新建 transport），loopback 短连接持续 churn，TIME_WAIT 不断堆积；且零值 Transport 无 `IdleConnTimeout`，空闲连接无回收上限。
2. **端口轮换重试空转**（`internal/task/activate.go` `startServeWithPortRetry`）：健康检查失败 kill serve 后，`allocatePort(lastPort=旧端口)` 立刻又分回刚释放的同一端口，`newPort == port` 时直接终态报错 `serve not ready`，`servePortRetries=3` 的重试循环实际第一次就退出——故障时没有任何真正的换端口重试。该循环还存在次生缺陷：最后一次迭代失败后仍分配并持久化一个从未启动 serve 的端口；`KillSession` 结果未确认即继续；端口耗尽错误被健康检查错误覆盖。

## What Changes

- occlient 改为**共享 loopback Transport**：clone `http.DefaultTransport` 后置 `Proxy: nil`（显式不变量，防代理语义漂移），同一 host:port 的 HTTP 连接跨 client 实例池化复用，空闲连接有界回收，消除短连接 churn。
- 修复 `startServeWithPortRetry` 重试闭环：serve 未通过健康检查时重试 MUST 切换到**不同**端口；旧 serve 会话未确认终止时 MUST NOT 分配/持久化新端口或创建新会话；最后一次尝试失败后 MUST NOT 再分配/持久化端口；端口耗尽错误 MUST 如实返回（不被健康检查错误覆盖）。
- 不改创建接口的异步激活语义、不改 UI、不改错误分类结构（probe 错误细节保留、UI 失败可见性另开 change）。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `opencode-orchestration`: 「端口分配策略」扩展轮换触发条件与重试副作用边界——serve 未通过健康检查（serve not ready）时重试 MUST 使用不同端口，且换端口以旧会话确认终止为前提；「serve 就绪等待与能力探测」区分单端口尝试失败（换端口重试）与全部尝试耗尽（激活失败）；新增「serve HTTP 客户端连接管理」要求——ocdeck-server 内经 occlient 发起的 REST/SSE 请求 MUST 经共享 loopback transport 连接复用。

## Impact

- **代码**：`internal/opencode/client.go`（共享 transport）、`internal/task/activate.go`（allocatePort 排除参数 + 重试闭环）、对应单测。
- **行为**：激活重试路径真实生效（总计 3 次尝试、相邻尝试不同端口）；serve HTTP 请求连接复用，TIME_WAIT 量级显著下降；端口耗尽与 kill 未确认故障如实上报。
- **兼容性**：无特殊约束（用户已确认）；共享 transport 仅作用于 loopback，无外部契约变化。
- **非目标**：probe/健康检查错误细节保留（`ErrServeNotReady` 吞掉 dial 细节）、激活失败的 UI 可见性、SSE 重连策略调整、TUI `opencode attach` 进程的连接行为——均不在本 change 范围。
