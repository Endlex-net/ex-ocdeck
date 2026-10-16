#!/usr/bin/env python3
"""check-contract-diff.py — 静态层：24 个上游契约锚点在 v1/v2 两个 commit 间的 diff。

契约来源：openspec/changes/opencode-v2-contract-spike/design.md D3（本脚本签名、
静态五态到结果 schema 的固定映射、退出码契约）、D2（边界验证前置）、D5（重试契约）。

用法: check-contract-diff.py <locked-manifest> <evidence-dir>
  - <locked-manifest> 必须是规范 sandbox 根的权威文件（manifest.json），且与本次 run
    evidence 副本逐字节一致（不一致 → 退出码 4）；双侧 ref 只从 commit_sha 读取。
  - <evidence-dir> 必须是规范 run 根（evidence/<run-id>/），否则退出码 2。
  - 追加 v2 侧新锚点时：扩展 ANCHORS 表（ID 不复用不改号）并同步 design D3 静态锚点 ID 表。
"""
from __future__ import annotations

import difflib
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import spikelib as S  # noqa: E402

REPO = "anomalyco/opencode"
RAW = "https://raw.githubusercontent.com"
API = "https://api.github.com"

# D3 静态锚点 ID 表（ID 即字面值；追加只许扩表，ID 不复用不改号）
ANCHORS: list[tuple[str, str]] = [
    ("static.01", "packages/schema/src/v1/permission.ts"),
    ("static.02", "packages/schema/src/v1/question.ts"),
    ("static.03", "packages/schema/src/v1/session.ts"),
    ("static.04", "packages/schema/src/v1/legacy-event.ts"),
    ("static.05", "packages/schema/src/session-status-event.ts"),
    ("static.06", "packages/schema/src/session-event.ts"),
    ("static.07", "packages/core/src/event.ts"),
    ("static.08", "packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts"),
    ("static.09", "packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts"),
    ("static.10", "packages/opencode/src/server/routes/instance/httpapi/handlers/permission.ts"),
    ("static.11", "packages/opencode/src/server/routes/instance/httpapi/handlers/question.ts"),
    ("static.12", "packages/opencode/src/server/routes/instance/httpapi/handlers/global.ts"),
    ("static.13", "packages/opencode/src/server/routes/instance/httpapi/groups/session.ts"),
    ("static.14", "packages/opencode/src/server/routes/instance/httpapi/groups/event.ts"),
    ("static.15", "packages/opencode/src/server/routes/instance/httpapi/groups/permission.ts"),
    ("static.16", "packages/opencode/src/server/routes/instance/httpapi/groups/question.ts"),
    ("static.17", "packages/opencode/src/server/routes/instance/httpapi/groups/global.ts"),
    ("static.18", "packages/opencode/src/session/status.ts"),
    ("static.19", "packages/opencode/src/event-v2-bridge.ts"),
    ("static.20", "packages/opencode/src/cli/cmd/attach.ts"),
    ("static.21", "packages/opencode/src/cli/tui/validate-session.ts"),
    ("static.22", "packages/opencode/src/cli/cmd/tui.ts"),
    ("static.23", "packages/opencode/src/cli/tui/worker.ts"),
    ("static.24", "packages/opencode/src/session/prompt.ts"),
]


def fetch_anchor(ref: str, path: str, log_rel_base: str, run_dir: Path) -> tuple[bytes | None, S.HttpResult]:
    """D5 重试契约第 2 条：仅传输失败重试（fetch_with_retry），至多 2 次、间隔 ≥5s、
    **每次尝试独立证据文件**（F14：<base>.attempt<N>.log，含侧别）。
    HTTP 错误状态属协议事实不重试。返回 (内容或 None, 最终 HttpResult)。"""
    url = f"{RAW}/{REPO}/{ref}/{path}"
    last: S.HttpResult | None = None

    def _log(attempt: int, r: S.HttpResult) -> None:
        S.safe_append(run_dir, f"{log_rel_base}.attempt{attempt}.log",
                      f"GET {url} attempt={attempt} status={r.status} err={r.error}\n")

    last = S.fetch_with_retry(url, timeout=30.0, on_attempt=_log)
    if last.status == 200 and last.body:
        return last.body, last
    return None, last


def confirm_removed_at_ref(ref: str, path: str, log_rel: str, run_dir: Path) -> bool:
    """仓库检索证实删除/迁移：raw 404 后经 GitHub contents API 证实该 ref 下不存在。"""
    url = f"{API}/repos/{REPO}/contents/{path}?ref={ref}"
    r = S.http_request(url, timeout=30.0)
    # F21：确认证据独立文件，供结果证据链引用
    S.safe_append(run_dir, log_rel,
                  f"contents-api probe: ref={ref} path={path} status={r.status} err={r.error}\n")
    return r.status == 404


def main() -> int:
    argv = sys.argv[1:]
    if len(argv) != 2:
        print("Usage: check-contract-diff.py <locked-manifest> <evidence-dir>", file=sys.stderr)
        return S.EXIT_PRE_REJECT
    locked_path, evidence_arg = argv
    if os.environ.get("SPIKE_SELFTEST"):
        S.pre_reject("SPIKE_SELFTEST must not be set in formal runs")

    # 前置：evidence 根校验
    run_dir = S.run_use_existing()
    if S.canonical(evidence_arg) != S.canonical(run_dir):
        S.pre_reject(f"<evidence-dir> is not the canonical run root: {evidence_arg}")

    registry = S.ProcessRegistry(run_dir)
    S.install_signal_handlers(registry)

    # 前置：锁定 manifest 消费契约（D1 双文件提交协议下游侧）
    sbroot = S.sandbox_root()
    if S.canonical(locked_path) != sbroot / "manifest.json":
        S.pre_reject(f"locked manifest must be the authoritative sandbox file: {sbroot}/manifest.json")
    evidence_copy = run_dir / "manifest.json"
    if not evidence_copy.is_file():
        S.pre_reject("evidence copy of locked manifest missing (prepare unpublished)")
    if Path(locked_path).read_bytes() != evidence_copy.read_bytes():
        S.harness_fail("authoritative manifest and evidence copy diverge")
    locked = S.validate_manifest(Path(locked_path), "locked")

    # 前置：边界验证（①canary 复验 ②边界描述一致性）
    mech, detail = S.boundary_env_check()
    if mech != locked["boundary_mechanism"] or detail != locked.get("boundary_detail", ""):
        S.pre_reject("boundary description mismatch with locked manifest")
    S.canary_verify(locked["canary_target"], "canary.log", run_dir)

    v1_ref, v2_ref = locked["v1"]["commit_sha"], locked["v2"]["commit_sha"]

    any_inconclusive = False
    for pid, path in ANCHORS:
        flog_base = f"static/{pid}.fetch"
        flog = f"{flog_base}.summary.log"   # 结果引用汇总文件（含各次 attempt 与确认证据清单）
        ev_rel = f"static/{pid}.diff"
        old, r_old = fetch_anchor(v1_ref, path, f"{flog_base}.v1", run_dir)
        new, r_new = fetch_anchor(v2_ref, path, f"{flog_base}.v2", run_dir)

        def _write_summary(extra: str = "") -> None:
            import glob as _glob
            attempts = sorted(
                str(Path(p).relative_to(run_dir))
                for p in _glob.glob(str(run_dir / "static" / f"{pid}.fetch.*.log"))
            )
            S.evidence_write(run_dir, flog,
                             f"anchor={path}\nv1_ref={v1_ref}\nv2_ref={v2_ref}\n"
                             f"attempts={attempts}\n{extra}")

        if old is None:
            # F22：v1 raw 404 + v2 有内容 → 经仓库检索证实旧侧不存在 → CONFIRMED_V2_ONLY
            if r_old.status == 404 and new is not None:
                contents_log = f"{flog_base}.v1.contents.log"
                if confirm_removed_at_ref(v1_ref, path, contents_log, run_dir):
                    _write_summary(f"v1-removed-proof={contents_log}\n")
                    S.emit_result(run_dir, locked, pid, "PASS", "待验证", "不适用", "valid",
                                  f"CONFIRMED_V2_ONLY: 旧侧经仓库检索证实不存在（{contents_log}）、"
                                  "新侧存在——旧侧缺对照面", flog)
                    continue
            _write_summary()
            S.emit_result(run_dir, locked, pid, "INCONCLUSIVE", "待验证", "不适用", "valid",
                          f"FETCH_ERROR: v1 侧拉取失败（status={r_old.status}，重试耗尽），"
                          "不得推断源码不存在", flog)
            any_inconclusive = True
            continue
        if new is None:
            # v1 存在 + v2 失败：区分 CONFIRMED_REMOVED_OR_MOVED 与 FETCH_ERROR。
            if r_new.status == 404:
                contents_log = f"{flog_base}.v2.contents.log"
                if confirm_removed_at_ref(v2_ref, path, contents_log, run_dir):
                    _write_summary(f"v2-removed-proof={contents_log}\n")
                    S.emit_result(run_dir, locked, pid, "PASS", "不兼容", "不适用", "valid",
                                  f"CONFIRMED_REMOVED_OR_MOVED: v1 侧存在、v2 侧 raw 404 且 "
                                  f"contents API 证实该 ref 不存在（{contents_log}；无新位置证据）",
                                  flog)
                    continue
            _write_summary()
            S.emit_result(run_dir, locked, pid, "INCONCLUSIVE", "待验证", "不适用", "valid",
                          f"FETCH_ERROR: v2 侧拉取失败（status={r_new.status} "
                          f"err={r_new.error}），仓库检索未能证实删除；不得推断源码不存在",
                          flog)
            any_inconclusive = True
            continue
        _write_summary()
        if old == new:
            S.evidence_write(run_dir, ev_rel, "identical\n")
            S.emit_result(run_dir, locked, pid, "PASS", "兼容", "不适用", "valid",
                          "SAME: 两 ref 内容一致", ev_rel)
        else:
            diff = "\n".join(difflib.unified_diff(
                old.decode("utf-8", "replace").splitlines(),
                new.decode("utf-8", "replace").splitlines(),
                fromfile=f"{v1_ref}/{path}", tofile=f"{v2_ref}/{path}", lineterm=""))
            S.evidence_write(run_dir, ev_rel, diff + "\n")
            # 源码差异本身 exec_status=PASS（能力影响由 live 层判定），不贡献退出码 1
            S.emit_result(run_dir, locked, pid, "PASS", "不兼容", "不适用", "valid",
                          "DIFF: 两 ref 内容存在差异（能力影响由 live 层判定）", ev_rel)

    # 汇总（静态层无 FAIL 来源；有 INCONCLUSIVE → 3，否则 0）
    return S.EXIT_INCONCLUSIVE if any_inconclusive else S.EXIT_OK


if __name__ == "__main__":
    try:
        sys.exit(main())
    except S.SpikeExit as e:
        sys.exit(e.code)
    except Exception as e:  # F3/F2：非契约异常收口为 4；打印前脱敏
        print(S.redact_text(f"harness error: unexpected: {type(e).__name__}: {e}"),
              file=sys.stderr)
        sys.exit(4)
