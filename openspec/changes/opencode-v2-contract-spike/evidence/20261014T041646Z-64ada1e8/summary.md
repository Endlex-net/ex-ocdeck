# spike run summary

- run_id: 20261014T041646Z-64ada1e8
- run-all exit code: 1
- note: 全部阶段执行完毕（汇总优先级 4→1→3→0）
- go/no-go（D8 机械判定，reverse 仅记录不参与）: NO-GO（必需能力实证不可满足：['status.01', 'capability.01']）
- baselines: v1=1.18.26(774cc7c1) v2=2.0.28(026b106b)
- reverse（migration.reverse）结论固定标注：仅记录，不作承诺
- D8 第 0 步人工核对（go/no-go 生效前必须完成）：逐条检查 results.jsonl 中 verdict=可满足/不可满足 的记录，若其观测结论与 check-contract-diff 产出的静态契约事实或 design.md 锁定语义存在冲突，须将该记录效力降级为 conflict 并重新判定；无冲突时在结论节记录「人工核对无冲突」。
