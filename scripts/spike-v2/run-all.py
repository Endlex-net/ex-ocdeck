#!/usr/bin/env python3
"""run-all.py — spike orchestrator：prepare → diff → live(probes) → live(migration) 顺序串联。

契约来源：openspec/changes/opencode-v2-contract-spike/design.md
  D2（run-id 前置、run 两类校验之初始化入口、边界生命周期：run-all 在同一已建立边界内
      启动全部阶段）
  D3（脚本签名、阶段状态机与退出码汇总、证据归档结构）

用法: run-all.py <candidate-manifest> [canary-target]
  canary-target 原样透传 prepare-env.py（「可选」仅表示可由候选 manifest 提供，无默认值）。
  前置环境变量：SPIKE_RUN_ID（格式见 D2）、SPIKE_BOUNDARY_MECHANISM
  （+other 时 SPIKE_BOUNDARY_DETAIL）。

go/no-go 判定不在本脚本：summary.md 写入执行摘要，最终判定由执行者按 D8 决策表完成。
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Optional

sys.path.insert(0, str(Path(__file__).resolve().parent))
import spikelib as S  # noqa: E402

HERE = Path(__file__).resolve().parent

# 阶段退出码记录（finalize 的 D8 第 1 条判定输入）
_PHASE_CODES: list[int] = []

# F9：阶段子进程纳入三层清理（run-all 作为编排层同样建立 registry）
_REGISTRY: Optional[S.ProcessRegistry] = None


def run_phase(name: str, argv: list[str]) -> int:
    """运行一个阶段脚本，返回其退出码（并记录进 _PHASE_CODES 供 D8 判定）。
    F9：阶段进程登记 item 层，阶段结束即收割；异常路径由 install_signal_handlers 兜底。"""
    global _REGISTRY
    print(f"==> phase: {name}")
    if _REGISTRY is not None:
        p = subprocess.Popen(argv)
        try:
            _REGISTRY.add("item", p.pid)
        except Exception:
            S.kill_tree(p.pid)
            raise
        rc = p.wait()
        _REGISTRY.cleanup_layer("item")   # 阶段结束有界收割（正常退出为 no-op）
    else:
        rc = subprocess.run(argv, check=False).returncode
    print(f"==> phase {name} exit={rc}")
    _PHASE_CODES.append(rc)
    return rc


def compute_go_no_go(run_dir: Path, phase_codes: list[int]) -> str:
    """D8 go/no-go 决策表（第 0 步证据效力预处理 + 5 条按序判定）。

    D6 承接必答八项在 spike 运行期间尚未填写（spike 执行后才在 design.md 结论节沉淀），
    因此机械判定中 D6 门禁按未解决处理——全部实证通过时输出「待补证据（D6 必答项未解决）」。
    """
    # F18：已知阶段码 5/2 优先于任何文件读取（读取故障不得遮蔽安全结论）
    if S.EXIT_SAFETY in phase_codes:
        return "NO-GO(安全越界)"
    if S.EXIT_PRE_REJECT in phase_codes:
        return "NO-GO(前置拒绝)"
    results_file = run_dir / "results.jsonl"
    records: list[dict] = []
    parse_fault = False
    if results_file.is_file():
        try:
            content = results_file.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            parse_fault = True
            content = ""
        for line in content.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                parse_fault = True
                continue
            if not isinstance(obj, dict):   # F13：非对象记录不得进入字段消费
                parse_fault = True
                continue
            # 完整结果 schema 校验（F13）：契约外记录按 harness 故障分流；
            # 有锁定 manifest 副本时核对双侧基线一致性
            try:
                locked_doc = None
                locked_copy = run_dir / "manifest.json"
                if locked_copy.is_file():
                    try:
                        locked_doc = json.loads(locked_copy.read_text(encoding="utf-8"))
                    except (json.JSONDecodeError, OSError):
                        locked_doc = None
                S.validate_result(obj, os.environ.get("SPIKE_RUN_ID", ""), locked_doc)
            except S.SpikeExit:
                parse_fault = True
                continue
            records.append(obj)
    # safety_invalid 记录（含 reverse——D8 第 1 条全局安全门禁对 reverse 同样适用）
    # F33：记录级安全事实优先于阶段 harness 故障分流，不得被阶段码 4 遮蔽
    if any(r.get("evidence_validity") == "safety_invalid" for r in records):
        return "NO-GO(安全越界)"
    # 证据效力分流（D8 第 0 步）：坏记录/重复 ID → 待补证据
    if parse_fault:
        return "待补证据（results.jsonl 读取/解析故障，证据效力无效）"
    seen: dict[str, int] = {}
    for r in records:
        pid = r.get("probe_id")
        if isinstance(pid, str):
            seen[pid] = seen.get(pid, 0) + 1
    if any(c > 1 for c in seen.values()):
        return "待补证据（results.jsonl 含重复 probe_id，违反一次执行一条契约）"

    def last(pid: str) -> dict | None:
        for r in reversed(records):
            if r.get("probe_id") == pid:
                return r
        return None

    required_live = [p for p in S.LIVE_PROBE_IDS
                     if not p.startswith("migration.") and p != "migration.reverse"]
    required = required_live + ["migration.forward"]
    # 第 0 步：效力预处理（safety_invalid → 第 1 条已覆盖；conflict/harness_invalid → 第 4 条）
    forward = last("migration.forward")
    # 第 2 条：有效实证承接前置失败（migration.forward 命中四类）
    handoff = ("READABILITY_FAILURE", "DATA_IDENTITY_FAILURE",
               "PRESERVATION_FAILURE", "CONTINUITY_FAILURE")
    if (forward and forward.get("evidence_validity") == "valid"
            and forward.get("exec_status") == "FAIL"
            and any(c in handoff for c in forward.get("failure_classes", []))):
        # 同时列出其他全部失败项（D8 第 2 条要求）
        others = [r["probe_id"] for r in records
                  if r.get("evidence_validity") == "valid"
                  and r.get("exec_status") == "FAIL"
                  and r.get("probe_id") != "migration.forward"]
        return (f"暂停 go/no-go：承接前置条件有效实证失败，提交用户决策（D6）；"
                f"其他失败项：{others or '无'}")
    # 第 3 条：其他必需能力有效实证不可满足
    failed_caps = []
    for pid in required_live:
        r = last(pid)
        if (r and r.get("evidence_validity") == "valid"
                and r.get("capability_verdict") == "不可满足"):
            failed_caps.append(pid)
    if failed_caps:
        return f"NO-GO（必需能力实证不可满足：{failed_caps}）"
    # F4/F33：阶段 harness 故障（退出码 4）分流在有效负面事实（第 2/3 条）之后——
    # 有效承接失败/能力不可满足不因其他阶段的 harness 故障被遮蔽。
    # F33 影响范围：若故障范围可确定为仅 reverse（reverse 记录 harness_invalid 且
    # 全部必需项记录均 valid 非 INCONCLUSIVE），按 D5「reverse 仅记录不参与」排除，
    # 不阻断必需项判定；范围无法确定时才保持存疑分流。
    if S.EXIT_HARNESS in phase_codes:
        reverse = last("migration.reverse")
        forward_r = last("migration.forward")

        def _clean(pid: str) -> bool:
            r = last(pid)
            return (r is not None
                    and r.get("evidence_validity") == "valid"
                    and r.get("exec_status") != "INCONCLUSIVE"
                    and r.get("capability_verdict") != "待验证")

        required_clean = all(_clean(pid) for pid in required)
        # F35：只有「故障来源可证明仅在最后的 migration 阶段」才排除 reverse。
        # 更早阶段（prepare/diff/probes）出现码 4 时范围存疑，不得因记录表面
        # clean + reverse harness_invalid 而放行。
        earlier_harness = any(c == S.EXIT_HARNESS for c in phase_codes[:-1])
        last_is_harness = bool(phase_codes) and phase_codes[-1] == S.EXIT_HARNESS
        reverse_only = (
            last_is_harness
            and not earlier_harness
            and reverse is not None
            and reverse.get("evidence_validity") == "harness_invalid"
            and forward_r is not None
            and forward_r.get("evidence_validity") == "valid"
            and required_clean)
        if not reverse_only:
            return ("待补证据（阶段 harness 故障：泵/证据/记录完整性存疑；"
                    "故障影响范围无法机械确定，全部结果需人工复核后重跑）")
        # reverse 专属故障（仅 migration 阶段码 4）：不阻断，继续第 4/5 条
    # 第 4 条：待补证据
    for pid in required:
        r = last(pid)
        if r is None:
            return f"待补证据（必需项 {pid} 无结果）"
        if (r.get("evidence_validity") in ("conflict", "harness_invalid")
                or r.get("exec_status") == "INCONCLUSIVE"
                or r.get("capability_verdict") == "待验证"):
            return f"待补证据（必需项 {pid} 证据不足或无效）"
    # 第 5 条：全部有效通过 + D6 门禁（运行期未解决）
    return "待补证据（全部必需项实证通过；D6 承接必答八项待 spike 结论节解决后方可 GO）"


def finalize(run_dir: Path, code: int, note: str) -> int:
    """summary.md 写入 run 级执行摘要 + D8 机械判定（F13）。
    canary 意外哨兵残留（D2 ⑤）必须在 summary 中可见。"""
    residual = ""
    canary_log = run_dir / "canary.log"
    if canary_log.is_file():
        for line in canary_log.read_text(encoding="utf-8", errors="replace").splitlines():
            if line.startswith("RESIDUAL:"):
                residual = line
                break
    verdict = compute_go_no_go(run_dir, _PHASE_CODES)
    lines = [
        "# spike run summary",
        "",
        f"- run_id: {os.environ.get('SPIKE_RUN_ID', '?')}",
        f"- run-all exit code: {code}",
        f"- note: {note}",
        f"- go/no-go（D8 机械判定，reverse 仅记录不参与）: {verdict}",
    ]
    if residual:
        lines.append(f"- canary residual: {residual}")
    # 双侧版本基线（D8 补充规则：结论列出证据状态与适用基线）
    locked_copy = run_dir / "manifest.json"
    if locked_copy.is_file():
        try:
            lm = json.loads(locked_copy.read_text(encoding="utf-8"))
            lines.append(f"- baselines: v1={lm['v1']['version']}({lm['v1']['commit_sha'][:8]}) "
                         f"v2={lm['v2']['version']}({lm['v2']['commit_sha'][:8]})")
        except (json.JSONDecodeError, KeyError):
            pass
    lines.append("- reverse（migration.reverse）结论固定标注：仅记录，不作承诺")
    # F13：D8 第 0 步冲突效力预处理——脚本无法自动判定实机观测与文档/源码冲突，
    # 在 summary 中显式列出可审计的人工核对步骤（人工确认前结论不得视为最终）
    lines.append("- D8 第 0 步人工核对（go/no-go 生效前必须完成）："
                 "逐条检查 results.jsonl 中 verdict=可满足/不可满足 的记录，"
                 "若其观测结论与 check-contract-diff 产出的静态契约事实或 design.md 锁定语义"
                 "存在冲突，须将该记录效力降级为 conflict 并重新判定；"
                 "无冲突时在结论节记录「人工核对无冲突」。")
    S.evidence_write(run_dir, "summary.md", "\n".join(lines) + "\n")
    return code


def main() -> int:
    argv = sys.argv[1:]
    if not 1 <= len(argv) <= 2:
        print("Usage: run-all.py <candidate-manifest> [canary-target]", file=sys.stderr)
        return S.EXIT_PRE_REJECT
    candidate = argv[0]
    canary_arg = argv[1] if len(argv) == 2 else ""
    if os.environ.get("SPIKE_SELFTEST"):
        S.pre_reject("SPIKE_SELFTEST must not be set in formal runs")

    # run 初始化入口（D2 两类校验之①）：目录不存在并原子创建，已存在 → 2
    run_dir = S.run_init_new()
    registry = S.ProcessRegistry(run_dir)
    S.install_signal_handlers(registry)
    global _REGISTRY
    _REGISTRY = registry   # F9：run_phase 阶段进程登记

    sbroot = S.sandbox_root()
    locked_manifest = sbroot / "manifest.json"

    # 阶段状态机（D3 唯一规则）：2/5 立即终止直接返回；否则继续，最终 4→1→3→0。
    # 未知阶段码先归一为 harness 故障 4（再应用 prepare 的 4 立即终止特例）。
    best = S.EXIT_OK

    # prepare
    prepare_argv = [sys.executable, str(HERE / "prepare-env.py"),
                    candidate, str(sbroot), str(run_dir)]
    if canary_arg:
        prepare_argv.append(canary_arg)
    code = run_phase("prepare", prepare_argv)
    if code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
        return finalize(run_dir, code, "prepare 前置拒绝/安全越界，立即终止")
    if code not in (0, 1, 3):
        return finalize(run_dir, S.EXIT_HARNESS,
                        f"prepare harness 故障（码 {code}）：无有效锁定 manifest，"
                        "后续阶段全部无法执行")
    best = S.phase_priority_max(best, code)

    # diff
    code = run_phase("diff", [sys.executable, str(HERE / "check-contract-diff.py"),
                              str(locked_manifest), str(run_dir)])
    if code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
        return finalize(run_dir, code, "diff 前置拒绝/安全越界，立即终止")
    best = S.phase_priority_max(best, code)

    # live(probes)
    code = run_phase("live-probes", [sys.executable, str(HERE / "live-probe.py"),
                                     str(locked_manifest), str(run_dir),
                                     "--mode", "probes"])
    if code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
        return finalize(run_dir, code, "live(probes) 前置拒绝/安全越界，立即终止")
    best = S.phase_priority_max(best, code)

    # live(migration)
    code = run_phase("live-migration", [sys.executable, str(HERE / "live-probe.py"),
                                        str(locked_manifest), str(run_dir),
                                        "--mode", "migration"])
    if code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
        return finalize(run_dir, code, "live(migration) 前置拒绝/安全越界，立即终止")
    best = S.phase_priority_max(best, code)

    return finalize(run_dir, best, "全部阶段执行完毕（汇总优先级 4→1→3→0）")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except S.SpikeExit as e:
        sys.exit(e.code)
    except Exception as e:  # F3：非契约异常收口为 harness 故障 4
        print(S.redact_text(f"harness error: unexpected: {type(e).__name__}: {e}"),
              file=sys.stderr)
        sys.exit(4)
