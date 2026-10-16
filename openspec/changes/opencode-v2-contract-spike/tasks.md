# Tasks

执行顺序即依赖顺序；全部脚本落点 `scripts/spike-v2/`（design D7）。契约细节一律以 design.md D1–D8 为唯一来源，tasks 不复制机制描述。

## 1. Harness 骨架与共享契约

- [x] 1.1 创建 `scripts/spike-v2/` 目录与公共库 `spikelib.py`：日志、证据文件写入（`evidence/<run-id>/` 固定结构，含 `source-chain/`、`static/`、`live/`、`migration/`）、凭据脱敏、子进程清理三层职责（D2/D5）
- [x] 1.2 实现统一结果 schema 的写入与校验函数：`probe_id` 封闭集合（静态 `static.01-24` ∪ live ∪ 4 个豁免 ID）、三维判定字段、`evidence_validity` 四枚举、`failure_classes`（D3）
- [x] 1.3 实现 D1 manifest 两态 JSON Schema 校验（candidate/locked 独立定义、共享 `$defs.fields`、`boundary_detail` 条件规则）与语义校验表（side 一致性/主版本/来源链/下载校验/运行时版本→退出码 4）
- [x] 1.4 实现统一退出码与失败分类：0–5 语义、前置拒绝(2) vs 运行中越界(5) 的 canary 阶段判定表、run 初始化两类校验（新 run 原子创建 vs 同 run 阶段复用）、`SPIKE_RUN_ID` 格式校验（D2/D3）

## 2. prepare-env.py

- [x] 2.1 实现签名 `prepare-env.py <candidate-manifest> <sandbox-root> <evidence-dir> [canary-target]`：路径/canonical/逃逸符号链接/继承变量前置校验（先于创建/下载/解包）
- [x] 2.2 实现边界验证：`SPIKE_BOUNDARY_MECHANISM`/`SPIKE_RUN_ID` 前置读取与记录、canary 契约（显式目标、挑战有效性前置条件、意外哨兵保留现场）、允许写入集合=两根
- [x] 2.3 实现下载/解包/校验/受控 `--version` 与锁定 manifest 双文件原子发布协议（临时文件→evidence 副本 rename→sandbox 权威 rename 唯一提交点、`previous-manifest.json` 归档）（D1）

## 3. check-contract-diff.py（静态层）

- [x] 3.1 实现静态锚点检查：`static.01-24`（与 `scripts/check-opencode-contract.sh` ANCHORS 对齐）+ 追加 v2 新锚点规则（追加即扩表、旧 ID 不变）
- [x] 3.2 实现静态观测状态五态（SAME/DIFF/CONFIRMED_REMOVED_OR_MOVED/FETCH_ERROR/CONFIRMED_V2_ONLY）到结果 schema 的固定映射，网络失败不推断源码不存在；静态项不进入 D8 必需集合

## 4. live-probe.py（probes 模式）

- [x] 4.1 实现 `--mode probes`：执行 D4 矩阵中非 `migration.*` 的 30 项，含 wire 形态发现先行、event.02 总述（订阅先后/目标关联/超时判定）与七类事件独立报告、SPI 8 窄接口+聚合覆盖闭环、4 个豁免记录 ID
- [x] 4.2 实现 fixture DAG：共享 session.01 生命周期（全部消费者完成前不删除）、破坏性操作独立 fixture、单项/共享/整阶段三层清理、单项失败继续独立后续项
- [x] 4.3 实现统一重试契约：probe 禁止自动重试、prompt 未知交付禁止重投（新建实验实例并关联原 run）、迁移前置依赖事实谓词（`evidence_validity=valid` 且 PASS/FAIL）

## 5. live-probe.py（migration 模式）

- [x] 5.1 实现 fixture manifest 两态生命周期（造数态 `v2_source` 可省略/执行态原子写回）、最低完整性谓词、快照冻结与逐字节校验（D5）
- [x] 5.2 实现正/反向快照隔离实验：独立目录、F⊆V 集合语义、两步比较谓词（v1 侧先校验 `expected`）、最小连续性断言（唯一 messageID 入史+有效后续处理+旧历史保留）
- [x] 5.3 实现 migration 结局六状态机（含 `NOT_STARTED_FIXTURE_INVALID`、reverse 专属断言集合与最低样本、reverse 仅记录不进 D8 第 2–5 条但安全门禁全局适用）与失败分类互斥（READABILITY 先判）

## 6. run-all.py 编排与汇总

- [x] 6.1 实现 `run-all.py <candidate-manifest> [canary-target]`：canary 参数透传 prepare、阶段顺序 `prepare → diff → live(probes) → live(migration)`、阶段状态机（2/5 立即终止直接返回；否则 4→1→3→0 汇总）
- [x] 6.2 实现 `summary.md` 生成：D8 go/no-go 第 0 步证据效力预处理 + 5 条按序判定（第 1 条拆前置拒绝/安全越界两分支）、D6 八项 resolved 谓词门禁、结论列出证据状态与双侧版本基线

## 7. 版本锁定、实机执行与结论回写（依赖用户提供隔离验证环境）

- [x] 7.1 按 D1 解析锁定时具体 v2 stable release、选定 v1 基线（项目 ContractBaseline），生成 candidate manifest 并准备来源链证据、记录版本选择依据；无符合条件的 v2 stable 时停止并提交用户决策（依赖用户隔离环境就绪，7.2 依赖本任务完成）
- [x] 7.2 执行完整 spike run，归档 `evidence/<run-id>/` 全部证据
- [x] 7.3 在 design.md 的「Spike 结论」节填写：协议差异清单、D5 迁移契约验证结论、D6 承接与兼容边界八项结论、D8 最终结论；承接前置条件实证失败时暂停并提交用户决策（不自动选降级）
- [x] 7.4 回写 `docs/design/opencode-v2-a-plus-migration.md`：版本锁定记录与结论引用，旧草案中与双 provider 长期并存方向冲突的建议标注为历史草案
