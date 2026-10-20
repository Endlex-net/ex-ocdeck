# spike run summary

- run_id: 20261014T040614Z-79bd8efd
- run-all exit code: 2
- note: prepare 前置拒绝/安全越界，立即终止
- go/no-go（D8 机械判定，reverse 仅记录不参与）: NO-GO(前置拒绝)
- reverse（migration.reverse）结论固定标注：仅记录，不作承诺
- D8 第 0 步人工核对（go/no-go 生效前必须完成）：逐条检查 results.jsonl 中 verdict=可满足/不可满足 的记录，若其观测结论与 check-contract-diff 产出的静态契约事实或 design.md 锁定语义存在冲突，须将该记录效力降级为 conflict 并重新判定；无冲突时在结论节记录「人工核对无冲突」。
