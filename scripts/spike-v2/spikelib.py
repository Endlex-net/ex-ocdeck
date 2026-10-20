"""spikelib.py — spike-v2 四脚本公共库（python3 标准库）。

契约唯一来源：openspec/changes/opencode-v2-contract-spike/design.md
  D1 manifest 两态 schema 与语义校验表 / 原子发布协议
  D2 允许写入集合 / 边界生命周期 / canary 契约 / SPIKE_RUN_ID / run 两类校验
  D3 统一结果 schema / probe_id 封闭集合 / 退出码 0-5 / canary 阶段判定表 / 阶段汇总优先级

所有字段名/枚举/正则/退出码与 design 逐字一致；本库不实现 design 之外的语义。
"""
from __future__ import annotations

import errno
import hashlib
import json
import os
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, NoReturn, Optional

# ---------------------------------------------------------------- 退出码（D3）
EXIT_OK = 0            # 全部必执行项通过
EXIT_FAIL = 1          # 至少一项断言失败
EXIT_PRE_REJECT = 2    # 前置拒绝：本阶段未启动任何上游操作、未产生实验副作用
EXIT_INCONCLUSIVE = 3  # 证据不足（存在 INCONCLUSIVE 且无 FAIL）
EXIT_HARNESS = 4       # harness 自身故障（受影响证据无效）
EXIT_SAFETY = 5        # 运行中发现安全越界：立即停止、保留现场、证据越界


class SpikeExit(Exception):
    """以契约退出码终止当前脚本。message 脱敏后写 stderr（F2）。"""

    def __init__(self, code: int, message: str = ""):
        super().__init__(message)
        self.code = code
        if message:
            print(redact_text(message), file=sys.stderr)


def die(code: int, message: str) -> NoReturn:
    raise SpikeExit(code, message)


def pre_reject(msg: str) -> NoReturn:
    die(EXIT_PRE_REJECT, f"PRE-REJECT: {msg}")


def harness_fail(msg: str) -> NoReturn:
    die(EXIT_HARNESS, f"harness error: {msg}")


# ---------------------------------------------------------------- 路径规范（D2）
SPIKE_CHANGE_REL = Path("openspec/changes/opencode-v2-contract-spike")
LIB_DIR = Path(__file__).resolve().parent


def repo_root() -> Path:
    """仓库根 = 本库文件所在仓库（git 以库文件目录为基准，与 cwd 无关）。

    SPIKE_REPO_ROOT 覆盖仅在 SPIKE_SELFTEST=1 时生效（单测专用）；
    正式四脚本入口处拒绝 SPIKE_SELFTEST 被设置。
    """
    if os.environ.get("SPIKE_SELFTEST") == "1" and os.environ.get("SPIKE_REPO_ROOT"):
        return Path(os.environ["SPIKE_REPO_ROOT"])
    try:
        out = subprocess.run(
            ["git", "-C", str(LIB_DIR), "rev-parse", "--show-toplevel"],
            capture_output=True, text=True, check=True,
        )
        return Path(out.stdout.strip())
    except (OSError, subprocess.CalledProcessError) as e:
        harness_fail(f"cannot locate repo root from {LIB_DIR}: {e}")


def canonical(p: os.PathLike | str) -> Path:
    """realpath 语义（含不存在路径）。"""
    return Path(os.path.realpath(str(p)))


def assert_ancestors_contained(path: Path, root: Path) -> None:
    """F04 路径逃逸防护：① 最终 canonical 路径必须在 canonical 根内；
    ② 根以下已存在的符号链接组件解析后必须仍在根内。
    根以上的系统级链接（macOS /var→/private/var）不参与检查。"""
    croot = canonical(root)
    cpath = canonical(path)
    if cpath != croot and croot not in cpath.parents:
        pre_reject(f"path escapes allowed root: {path} -> {cpath} (root {croot})")
    # 根以下组件逐级检查符号链接
    try:
        rel = Path(path).resolve().relative_to(croot)
    except ValueError:
        # 与 root 无前缀关系（异常情形）：canonical 终判已覆盖
        rel = None
    if rel is not None:
        comp = croot
        for part in rel.parts:
            comp = comp / part
            if comp.is_symlink():
                resolved = canonical(comp)
                if resolved != croot and croot not in resolved.parents and resolved != comp:
                    pre_reject(f"ancestor symlink escapes allowed root: {comp} -> {resolved}")


def sandbox_root() -> Path:
    """规范真实沙箱根（唯一定义，D2）。"""
    logical = repo_root() / SPIKE_CHANGE_REL / "sandbox"
    assert_ancestors_contained(logical, repo_root())
    return canonical(logical)


def evidence_parent() -> Path:
    logical = repo_root() / SPIKE_CHANGE_REL / "evidence"
    assert_ancestors_contained(logical, repo_root())
    return canonical(logical)


# ---------------------------------------------------------------- run-id（D2）
RUN_ID_PATTERN = re.compile(r"^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$")


def run_id() -> str:
    rid = os.environ.get("SPIKE_RUN_ID", "")
    if not RUN_ID_PATTERN.match(rid):
        pre_reject(f"SPIKE_RUN_ID missing or invalid (must match {RUN_ID_PATTERN.pattern})")
    return rid


def evidence_dir() -> Path:
    return evidence_parent() / run_id()


def assert_canonical_run_dir(d: Path) -> None:
    want = evidence_dir()
    assert_ancestors_contained(d, evidence_parent())
    if canonical(d) != want:
        pre_reject(f"not the canonical run dir: {d} (resolved {canonical(d)} != {want})")


EVIDENCE_LAYOUT = ("source-chain", "static", "live", "migration")


def evidence_layout_init(d: Path) -> None:
    assert_canonical_run_dir(d)
    for sub in EVIDENCE_LAYOUT:
        (d / sub).mkdir(parents=True, exist_ok=True)
    results = d / "results.jsonl"
    if results.is_symlink():
        harness_fail(f"results.jsonl is a symlink: {results}")
    results.touch(exist_ok=True)


def run_init_new() -> Path:
    """新 run 初始化入口（run-all.py 专用）：目录不存在并原子创建；已存在 → 2。"""
    d = evidence_dir()
    parent = d.parent
    assert_ancestors_contained(d, parent)
    parent.mkdir(parents=True, exist_ok=True)
    try:
        d.mkdir()
    except FileExistsError:
        pre_reject(f"evidence run dir already exists: {d}")
    except OSError as e:
        harness_fail(f"cannot create evidence run dir {d}: {e}")
    evidence_layout_init(d)
    return d


def run_use_existing() -> Path:
    """同一 run 的阶段脚本：目录允许已存在；单独调用 prepare 且不存在时承担原子创建。"""
    d = evidence_dir()
    if not d.is_dir():
        parent = d.parent
        assert_ancestors_contained(d, parent)
        parent.mkdir(parents=True, exist_ok=True)
        try:
            d.mkdir()
        except FileExistsError:
            pass
        except OSError as e:
            harness_fail(f"cannot create evidence run dir {d}: {e}")
    assert_canonical_run_dir(d)
    evidence_layout_init(d)
    return d


# ---------------------------------------------------------------- 边界描述输入（D2）
BOUNDARY_MECHANISMS = ("vm", "container", "sandbox-exec", "other")


def boundary_env_check() -> tuple[str, str]:
    """返回 (mechanism, detail)；缺失/非法 → 退出码 2。"""
    mech = os.environ.get("SPIKE_BOUNDARY_MECHANISM", "")
    detail = os.environ.get("SPIKE_BOUNDARY_DETAIL", "")
    if mech not in BOUNDARY_MECHANISMS:
        pre_reject(f"SPIKE_BOUNDARY_MECHANISM missing or invalid (got {mech!r})")
    if mech == "other" and not detail:
        pre_reject("SPIKE_BOUNDARY_DETAIL required when SPIKE_BOUNDARY_MECHANISM=other")
    if mech != "other" and detail:
        pre_reject("SPIKE_BOUNDARY_DETAIL forbidden unless SPIKE_BOUNDARY_MECHANISM=other")
    return mech, detail


# ---------------------------------------------------------------- 凭据脱敏（D3）
_SECRET_RE = re.compile(r"PASSWORD|TOKEN|SECRET|KEY")

# 运行期凭据登记（F2）：生成密码等不落 env 的凭据显式登记，纳入脱敏集合。
_RUNTIME_SECRETS: list[str] = []


def register_secret(value: str) -> None:
    if value:
        _RUNTIME_SECRETS.append(value)
        _RUNTIME_SECRETS.sort(key=len, reverse=True)


def _collect_secrets() -> list[str]:
    """收集全部凭据形态：原始值 + URL 编码 + Basic base64 + JSON 转义
    （F2：响应/日志/URL 可能以编码形态回显凭据，仅替换原始值不够）。"""
    import base64
    import json as _json
    import urllib.parse
    out = list(_RUNTIME_SECRETS)
    for k, v in os.environ.items():
        if v and _SECRET_RE.search(k.upper()):
            out.append(v)
    expanded: list[str] = []
    for v in out:
        expanded.append(v)
        expanded.append(urllib.parse.quote(v, safe=""))
        expanded.append(base64.b64encode(v.encode()).decode())
        # JSON 字符串内转义形态（两种 Unicode 输出：ensure_ascii=True 的 \uXXXX
        # 与 ensure_ascii=False 的原字符；F2：非 ASCII 凭据在 JSON 响应中的两种回显）
        for ea in (True, False):
            jv = _json.dumps(v, ensure_ascii=ea)
            if jv != f'"{v}"':
                expanded.append(jv[1:-1])
    # 长凭据先替换，避免短凭据是长凭据子串时的遮蔽
    expanded = [e for e in dict.fromkeys(expanded) if e]
    expanded.sort(key=len, reverse=True)
    return expanded


def redact_text(text: str) -> str:
    """文本证据脱敏：>=4 字符凭据全子串替换；<4 字符按独立 token 边界替换
    （避免破坏版本号/ID 等协议字面值）。"""
    for v in _collect_secrets():
        if len(v) >= 4:
            text = text.replace(v, "***REDACTED***")
        else:
            text = re.sub(
                r"(?<![A-Za-z0-9._-])" + re.escape(v) + r"(?![A-Za-z0-9._-])",
                "***REDACTED***", text,
            )
    return text


def redact_result(doc: dict) -> dict:
    """JSON 结果脱敏：仅自由文本 reason 字段全量脱敏；身份字段保持契约字面。"""
    if isinstance(doc.get("reason"), str):
        d = dict(doc)
        d["reason"] = redact_text(d["reason"])
        return d
    return doc


# ---------------------------------------------------------------- 证据写入（D3/F04）
def _check_write_target(d: Path, rel: str) -> Path:
    assert_canonical_run_dir(d)
    rp = Path(rel)
    if rp.is_absolute() or ".." in rp.parts:
        harness_fail(f"evidence path must be relative, no '..': {rel}")
    target = d / rp
    assert_ancestors_contained(target, d)
    if target.is_symlink():
        harness_fail(f"evidence target is a symlink: {target}")
    return target


def evidence_mkdir(d: Path, rel: str) -> Path:
    """F28：run 侧目录创建的受保护入口（canonical run 根校验 + 链接检查先于创建）。"""
    target = _check_write_target(d, rel)
    target.mkdir(parents=True, exist_ok=True)
    return target


def evidence_write(d: Path, rel: str, content: str | bytes) -> Path:
    """受保护原子写入：校验 → 临时文件 → rename。
    文本内容先脱敏；bytes 证据按 UTF-8 可解则脱敏后落盘（F2：HTTP 文本响应不脱敏会漏凭据），
    不可解的二进制 artifact 原样落盘（消息原文哈希输入等）。"""
    target = _check_write_target(d, rel)
    target.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(content, str):
        content = redact_text(content).encode()
    else:
        try:
            content = redact_text(content.decode("utf-8")).encode("utf-8")
        except UnicodeDecodeError:
            # F2：二进制 artifact 仍须字节级脱敏——凭据的 UTF-8 原字节与
            # ASCII 编码形态（URL 编码/base64）均可在二进制中明文出现；
            # 不能用 ASCII ignore 丢字符构造匹配值
            for v in _collect_secrets():
                for enc in ("utf-8", "ascii"):
                    try:
                        b = v.encode(enc)
                    except UnicodeEncodeError:
                        continue
                    if len(b) >= 4 and b in content:
                        content = content.replace(b, b"***REDACTED***")
    fd, tmp = tempfile.mkstemp(dir=target.parent, prefix=target.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
        os.replace(tmp, target)
    except OSError as e:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        harness_fail(f"evidence write failed: {target}: {e}")
    return target


def safe_append(d: Path, rel: str, content: str) -> Path:
    """受保护追加（日志类）：目标与祖先链校验先于写入；内容脱敏。"""
    target = _check_write_target(d, rel)
    target.parent.mkdir(parents=True, exist_ok=True)
    try:
        with open(target, "a", encoding="utf-8") as f:
            f.write(redact_text(content))
    except OSError as e:
        harness_fail(f"append failed: {target}: {e}")
    return target


def protected_append_path(target: Path, content: str, root: Optional[Path] = None) -> Path:
    """F1：绝对路径追加入口——打开前再做 canonical/祖先链检查。"""
    if root is None:
        try:
            root = canonical(evidence_dir())
        except Exception:
            root = sandbox_root()
    assert_ancestors_contained(target, root)
    cp = canonical(target)
    if cp != root and root not in cp.parents:
        pre_reject(f"append target escapes root: {target} -> {cp}")
    if target.is_symlink() or (target.parent.is_symlink() if target.parent.exists() else False):
        harness_fail(f"append target is a symlink: {target}")
    target.parent.mkdir(parents=True, exist_ok=True)
    try:
        with open(target, "a", encoding="utf-8") as f:
            f.write(redact_text(content))
    except OSError as e:
        harness_fail(f"append failed: {target}: {e}")
    return target


# ---------------------------------------------------------------- 沙箱侧写入防护（D2/F1）
def assert_sandbox_path(p: Path) -> None:
    """目标必须在规范沙箱根内、祖先链无逃逸链接（F1：sandbox 侧统一入口）。"""
    sbroot = sandbox_root()
    assert_ancestors_contained(p, sbroot)
    cp = canonical(p)
    if cp != sbroot and sbroot not in cp.parents:
        pre_reject(f"sandbox write target escapes: {p} -> {cp}")


def sandbox_mkdir(p: Path, parents: bool = True) -> None:
    """沙箱内目录创建的统一受保护入口（先校验后创建）。"""
    assert_sandbox_path(p)
    p.mkdir(parents=parents, exist_ok=True)


def sandbox_write(p: Path, content: str | bytes) -> Path:
    """沙箱内文件写入的统一受保护入口（文本脱敏；原子 rename）。"""
    assert_sandbox_path(p)
    if isinstance(content, str):
        content = redact_text(content).encode()
    else:
        try:
            content = redact_text(content.decode("utf-8")).encode("utf-8")
        except UnicodeDecodeError:
            pass
    p.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=p.parent, prefix=p.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
        os.replace(tmp, p)
    except OSError as e:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        harness_fail(f"sandbox write failed: {p}: {e}")
    return p


def sandbox_write_raw(p: Path, content: bytes) -> Path:
    """沙箱内原始字节写入（F16）：受保护但绝不脱敏——下载资产等哈希校验对象专用。"""
    assert_sandbox_path(p)
    p.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=p.parent, prefix=p.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
        os.replace(tmp, p)
    except OSError as e:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        harness_fail(f"sandbox raw write failed: {p}: {e}")
    return p


def artifact_write(d: Path, rel: str, content: bytes) -> Path:
    """原始字节 artifact 写入（F16）：受保护但绝不脱敏——用于下载资产、
    消息原文等哈希校验对象。脱敏会改变字节内容，破坏 checksum/哈希一致性。"""
    target = _check_write_target(d, rel)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=target.parent, prefix=target.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
        os.replace(tmp, target)
    except OSError as e:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        harness_fail(f"artifact write failed: {target}: {e}")
    return target


# ---------------------------------------------------------------- canary（D2/D3）
def canary_verify(target: str, log_rel: str, run_dir: Path) -> None:
    """canary 契约 + 挑战有效性前置条件。所有拒绝 → 退出码 2；返回 None 表示边界有效。

    - SPIKE_CANARY_PERMISSION_PROOF 必填（调用方建边界前的创建权限依据，记入 canary.log）
    - 目标父目录必须存在；目标执行前必须不存在
    - 目标必须位于允许写入集合之外（sandbox 根 ∪ 本次 run evidence 根之外）
    - 边界内创建哨兵必须被拒绝；errno 必须是 EPERM/EACCES（其余无法归因 → 挑战无效 → 2）
    - 意外创建成功：保留现场、canary.log 记 RESIDUAL（summary 流程读取）→ 2
    """
    if not target:
        pre_reject("canary target missing (no default; provide via CLI or candidate manifest)")
    canon = canonical(target)
    proof = os.environ.get("SPIKE_CANARY_PERMISSION_PROOF", "")
    if not proof:
        pre_reject("canary challenge invalid: SPIKE_CANARY_PERMISSION_PROOF missing")
    sbroot = sandbox_root()
    evroot = evidence_dir()
    for root in (sbroot, evroot):
        if canon == root or root in canon.parents:
            pre_reject(f"canary target inside allowed-write set: {canon}")
    if not canon.parent.is_dir():
        safe_append(run_dir, log_rel,
                    f"[{_utc_now()}] INVALID-CHALLENGE: parent dir missing: {canon.parent}\n"
                    f"caller-proof: {proof}\n")
        pre_reject(f"canary challenge invalid (parent dir missing): {canon.parent}")
    if canon.exists() or canon.is_symlink():
        pre_reject(f"canary target already exists: {canon}")
    try:
        canon.mkdir()
    except OSError as e:
        # EPERM/EACCES = 权限拒绝；EROFS = 只读文件系统（ro rootfs 容器边界）——
        # 三者均可归因于执行边界
        if e.errno in (errno.EPERM, errno.EACCES, errno.EROFS):
            safe_append(run_dir, log_rel,
                        f"[{_utc_now()}] REJECTED as expected: {canon}\n"
                        f"errno: {e.errno} ({e.strerror})\ncaller-proof: {proof}\n")
            return
        safe_append(run_dir, log_rel,
                    f"[{_utc_now()}] INVALID-CHALLENGE: unattributable error at {canon}\n"
                    f"errno: {e.errno} ({e.strerror})\ncaller-proof: {proof}\n")
        pre_reject(f"canary challenge invalid: unattributable errno {e.errno} ({e.strerror})")
    # 意外创建成功：边界无效。保留现场，禁止删除（D2 ⑤）
    safe_append(run_dir, log_rel,
                f"[{_utc_now()}] BOUNDARY-INEFFECTIVE: sentinel unexpectedly created at {canon}\n"
                f"RESIDUAL: {canon} (left in place; user disposal required)\n"
                f"caller-proof: {proof}\n")
    pre_reject(f"canary sentinel created (boundary ineffective); residual kept at {canon}")


def _utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------- probe_id 封闭集合（D3/D4）
STATIC_PROBE_IDS = [f"static.{i:02d}" for i in range(1, 25)]

LIVE_PROBE_IDS = [
    "supervisor.01", "supervisor.02", "supervisor.03",
    "session.01", "session.02", "session.03", "session.04", "session.05",
    "session.06", "session.07",
    "event.01",
    "event.02a.session_created", "event.02b.session_updated", "event.02c.session_deleted",
    "event.02d.status_changed", "event.02e.attention_raised", "event.02f.attention_resolved",
    "event.02g.session_error",
    "attention.01", "attention.02", "attention.03",
    "prompt.01", "prompt.02", "prompt.03",
    "status.01", "capability.01", "terminal.01", "title.01", "conn.01", "recovery.01",
    "migration.forward", "migration.reverse",
]

EXEMPT_PROBE_IDS = [
    "spi.exempt.runtime_supervisor.acquire_ephemeral",
    "spi.exempt.runtime_supervisor.release",
    "spi.exempt.provider.meta",
    "spi.exempt.provider.binding_runtime_endpoint",
]

CLOSED_PROBE_IDS = set(STATIC_PROBE_IDS) | set(LIVE_PROBE_IDS) | set(EXEMPT_PROBE_IDS)

# D5 迁移结果分类表六类（优先级顺序固定）
FAILURE_CLASSES = [
    "READABILITY_FAILURE", "DATA_IDENTITY_FAILURE", "PRESERVATION_FAILURE",
    "CONTINUITY_FAILURE", "INCONCLUSIVE", "HARNESS_FAILURE",
]

EXEC_STATUSES = ("PASS", "FAIL", "INCONCLUSIVE")
DIFF_CLASSES = ("兼容", "不兼容", "待验证")
CAPABILITY_VERDICTS = ("可满足", "不可满足", "待验证", "不适用")
EVIDENCE_VALIDITIES = ("valid", "conflict", "harness_invalid", "safety_invalid")

_SEMVER_RE = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
_SHA40_RE = re.compile(r"^[0-9a-f]{40}$")
_SHA64_RE = re.compile(r"^[0-9a-f]{64}$")


def _baseline_ok(b: Any) -> bool:
    return (isinstance(b, dict) and set(b) == {"version", "commit_sha"}
            and isinstance(b["version"], str) and bool(_SEMVER_RE.match(b["version"]))
            and isinstance(b["commit_sha"], str) and bool(_SHA40_RE.match(b["commit_sha"])))


def validate_result(doc: Any, run_id_expected: str,
                    locked: Optional[dict] = None) -> None:
    """统一结果 schema 校验（D3）。违规 → harness_fail（退出码 4）。"""
    if not isinstance(doc, dict):
        harness_fail("result is not an object")
    pid = doc.get("probe_id")
    if not isinstance(pid, str) or pid not in CLOSED_PROBE_IDS:
        harness_fail(f"probe_id not in closed set: {pid!r}")
    is_static = pid.startswith("static.")
    is_exempt = pid in EXEMPT_PROBE_IDS
    is_migration = pid.startswith("migration.")
    if doc.get("run_id") != run_id_expected:
        harness_fail(f"run_id mismatch: {doc.get('run_id')!r} != {run_id_expected}")
    if not _baseline_ok(doc.get("v1_baseline")) or not _baseline_ok(doc.get("v2_baseline")):
        harness_fail(f"baseline shape invalid for {pid}")
    if doc.get("exec_status") not in EXEC_STATUSES:
        harness_fail(f"exec_status invalid for {pid}: {doc.get('exec_status')!r}")
    if doc.get("evidence_validity") not in EVIDENCE_VALIDITIES:
        harness_fail(f"evidence_validity invalid for {pid}: {doc.get('evidence_validity')!r}")
    if not isinstance(doc.get("reason"), str) or not isinstance(doc.get("evidence_path"), str):
        harness_fail(f"reason/evidence_path must be strings for {pid}")
    ep = doc["evidence_path"]
    if ep.startswith("/") or ".." in Path(ep).parts:
        harness_fail(f"evidence_path must be run-relative for {pid}: {ep}")
    eq = doc.get("equivalent")
    if not isinstance(eq, str):
        harness_fail(f"equivalent must be a string for {pid}")
    dc = doc.get("diff_class")
    cv = doc.get("capability_verdict")
    if is_exempt:
        if dc != "不适用" or cv != "不适用" or eq != "" or doc["exec_status"] != "PASS":
            harness_fail(f"exempt record must be PASS/不适用/不适用/equivalent='' for {pid}")
    else:
        if dc not in DIFF_CLASSES:
            harness_fail(f"diff_class invalid for {pid}: {dc!r}")
        if is_static:
            if cv != "不适用" or eq != "":
                harness_fail(f"static record must have capability_verdict=不适用, equivalent='' for {pid}")
        else:
            if cv not in CAPABILITY_VERDICTS:
                harness_fail(f"capability_verdict invalid for {pid}: {cv!r}")
            if eq and not (dc == "不兼容" and cv == "可满足"):
                harness_fail(f"equivalent non-empty requires diff_class=不兼容 and 可满足 for {pid}")
    if is_migration:
        fc = doc.get("failure_classes")
        if not isinstance(fc, list) or any(f not in FAILURE_CLASSES for f in fc):
            harness_fail(f"failure_classes invalid for {pid}: {fc!r}")
        if fc != sorted(fc, key=FAILURE_CLASSES.index):
            harness_fail(f"failure_classes must be in D5 priority order for {pid}")
    elif "failure_classes" in doc:
        harness_fail(f"failure_classes forbidden for non-migration record {pid}")
    # 有效 live 记录的映射一致性；无效化记录（D8 第 0 步）豁免
    if not is_static and not is_exempt and doc["evidence_validity"] == "valid":
        es, cvd = doc["exec_status"], cv
        if not ((es == "PASS" and cvd == "可满足")
                or (es == "FAIL" and cvd == "不可满足")
                or (es == "INCONCLUSIVE" and cvd == "待验证")):
            harness_fail(f"exec_status/capability_verdict mapping violation for {pid}: {es}/{cvd}")
    # baseline 与锁定 manifest 一致性（给了 locked 时）
    if locked is not None:
        for side in ("v1", "v2"):
            b = doc[f"{side}_baseline"]
            m = locked[side]
            if b["version"] != m["version"] or b["commit_sha"] != m["commit_sha"]:
                harness_fail(f"baseline diverges from locked manifest for {pid} ({side})")


def result_append(run_dir: Path, doc: dict, locked: Optional[dict] = None) -> None:
    """脱敏（reason）→ 校验 → 追加 results.jsonl。失败 → harness_fail。"""
    doc = redact_result(doc)
    validate_result(doc, run_id(), locked)
    # equivalent 非空时：run 内相对路径、canonical 后仍在 run 内且文件存在
    eq = doc.get("equivalent", "")
    if eq:
        eqp = Path(eq)
        if eqp.is_absolute() or ".." in eqp.parts:
            harness_fail(f"equivalent must be run-relative: {eq}")
        target = canonical(run_dir / eqp)
        if canonical(run_dir) not in target.parents:
            harness_fail(f"equivalent escapes run dir: {eq}")
        if not target.is_file():
            harness_fail(f"equivalent evidence path missing: {eq}")
    safe_append(run_dir, "results.jsonl", json.dumps(doc, ensure_ascii=False) + "\n")


def emit_result(run_dir: Path, locked: dict, probe_id: str, exec_status: str,
                diff_class: str, capability_verdict: str, evidence_validity: str,
                reason: str, evidence_path: str, equivalent: str = "",
                failure_classes: Optional[list[str]] = None) -> None:
    """统一构造 + 校验 + 落盘一条结果。baselines 取自锁定 manifest。"""
    doc: dict[str, Any] = {
        "run_id": run_id(),
        "probe_id": probe_id,
        "v1_baseline": {"version": locked["v1"]["version"], "commit_sha": locked["v1"]["commit_sha"]},
        "v2_baseline": {"version": locked["v2"]["version"], "commit_sha": locked["v2"]["commit_sha"]},
        "exec_status": exec_status,
        "diff_class": diff_class,
        "capability_verdict": capability_verdict,
        "evidence_validity": evidence_validity,
        "equivalent": equivalent,
        "reason": reason,
        "evidence_path": evidence_path,
    }
    if failure_classes is not None:
        doc["failure_classes"] = failure_classes
    result_append(run_dir, doc, locked)


# ---------------------------------------------------------------- manifest（D1）
_RE_HTTPS = re.compile(r"^https://")
_RE_LOCKED_AT = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")

_SIDE_CANDIDATE_FIELDS: dict[str, Any] = {
    "side": lambda v: v in ("v1", "v2"),
    "version": lambda v: isinstance(v, str) and bool(_SEMVER_RE.match(v)),
    "release_url": lambda v: _is_https_uri(v),
    "tag": lambda v: isinstance(v, str) and len(v) >= 1,
    "commit_sha": lambda v: isinstance(v, str) and bool(_SHA40_RE.match(v)),
    "asset_name": lambda v: isinstance(v, str) and len(v) >= 1,
    "os": lambda v: isinstance(v, str) and len(v) >= 1,
    "arch": lambda v: isinstance(v, str) and len(v) >= 1,
    "download_url": lambda v: _is_https_uri(v),
    "official_sha256": lambda v: v is None or (isinstance(v, str) and bool(_SHA64_RE.match(v))),
    "source_chain_status": lambda v: v in ("verified", "unverified"),
}
_SIDE_LOCKED_EXTRA: dict[str, Any] = {
    "locked_at": lambda v: isinstance(v, str) and bool(_RE_LOCKED_AT.match(v)),
    "download_sha256": lambda v: isinstance(v, str) and bool(_SHA64_RE.match(v)),
    "sha256_match": lambda v: isinstance(v, bool),
    "exec_path": lambda v: isinstance(v, str) and v.startswith("/"),
    "runtime_version_output": lambda v: isinstance(v, str),
}


def _is_https_uri(v: Any) -> bool:
    """format:uri + ^https:// 的可执行等价：ASCII 可打印、无空白/控制字符/未保留字符、
    合法百分号编码、scheme=https 且 netloc 非空。"""
    if not isinstance(v, str) or not _RE_HTTPS.match(v):
        return False
    if re.search(r"%(?![0-9A-Fa-f]{2})", v):
        return False
    for c in v:
        o = ord(c)
        if o < 0x21 or o > 0x7E or c in '<>"{}|\\^`':
            return False
    from urllib.parse import urlparse
    try:
        p = urlparse(v)
    except ValueError:
        return False
    return p.scheme == "https" and bool(p.netloc)


def validate_manifest(path: Path, mode: str) -> dict:
    """manifest 两态校验（candidate/locked）。结构/语义违规 → 退出码 2；
    锁定态运行时版本不符 → 退出码 4。返回解析后的 manifest。"""
    if mode not in ("candidate", "locked"):
        harness_fail(f"unknown manifest mode: {mode}")
    try:
        m = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        pre_reject(f"cannot read/parse manifest {path}: {e}")
    if not isinstance(m, dict):
        pre_reject("manifest top-level must be an object")
    locked = mode == "locked"
    errors: list[str] = []
    top_allowed = {"schema_version", "canary_target", "v1", "v2"}
    if locked:
        top_allowed |= {"boundary_mechanism", "boundary_detail"}
    for k in m:
        if k not in top_allowed:
            errors.append(f"top: unknown field {k!r}")
    sv = m.get("schema_version")
    if isinstance(sv, bool) or not isinstance(sv, (int, float)) or sv != 1:
        errors.append("schema_version must be numeric 1")
    for side_name in ("v1", "v2"):
        if side_name not in m:
            errors.append(f"top: missing required field {side_name!r}")
        elif not isinstance(m[side_name], dict):
            errors.append(f"{side_name}: must be an object")
    if "canary_target" in m:
        ct = m["canary_target"]
        if not isinstance(ct, str) or not ct.startswith("/"):
            errors.append("canary_target: must be an absolute-path string")
    if locked:
        for k in ("canary_target", "boundary_mechanism"):
            if k not in m:
                errors.append(f"top: missing required locked field {k!r}")
        if "boundary_mechanism" in m and m["boundary_mechanism"] not in BOUNDARY_MECHANISMS:
            errors.append(f"boundary_mechanism: invalid value {m['boundary_mechanism']!r}")
        if m.get("boundary_mechanism") == "other":
            if not (isinstance(m.get("boundary_detail"), str) and len(m["boundary_detail"]) >= 1):
                errors.append("boundary_detail: required (minLength 1) when boundary_mechanism=other")
        elif "boundary_detail" in m:
            errors.append("boundary_detail: forbidden unless boundary_mechanism=other")
    else:
        for k in ("boundary_mechanism", "boundary_detail"):
            if k in m:
                errors.append(f"top: {k!r} forbidden in candidate state")
    for side_name in ("v1", "v2"):
        side = m.get(side_name)
        if not isinstance(side, dict):
            continue
        allowed = dict(_SIDE_CANDIDATE_FIELDS)
        if locked:
            allowed.update(_SIDE_LOCKED_EXTRA)
        for k in side:
            if k not in allowed:
                errors.append(f"{side_name}: unknown field {k!r}")
        for k in sorted(set(allowed) - set(side)):
            errors.append(f"{side_name}: missing required field {k!r}")
        for k, ok in allowed.items():
            if k in side and not ok(side[k]):
                errors.append(f"{side_name}.{k}: invalid value {side[k]!r}")
    if errors:
        pre_reject("manifest invalid: " + "; ".join(errors))
    # 语义校验表（D1：结构合法之外的不变量）
    v1, v2 = m["v1"], m["v2"]
    if v1["side"] != "v1" or v2["side"] != "v2":
        pre_reject("semantic: side must equal its object key")
    if int(v1["version"].split(".")[0]) != 1:
        pre_reject(f"semantic: v1 major must be 1, got {v1['version']}")
    if int(v2["version"].split(".")[0]) != 2:
        pre_reject(f"semantic: v2 major must be 2, got {v2['version']}")
    for name, s in (("v1", v1), ("v2", v2)):
        if s["source_chain_status"] == "verified" and s["official_sha256"] is None:
            pre_reject(f"semantic: {name}.source_chain_status=verified requires non-null official_sha256")
        if locked:
            if s["source_chain_status"] != "verified":
                pre_reject(f"semantic: {name}.source_chain_status must be verified in locked state")
            if s["sha256_match"] is not True:
                pre_reject(f"semantic: {name}.sha256_match must be true in locked state")
            if s["download_sha256"] != s["official_sha256"]:
                pre_reject(f"semantic: {name}.download_sha256 != official_sha256")
            mv = re.search(r"\d+\.\d+\.\d+", s["runtime_version_output"])
            if not mv or mv.group(0) != s["version"]:
                # 退出码 4：准备阶段基线验证故障（二进制已执行，非前置拒绝）
                die(EXIT_HARNESS,
                    f"manifest baseline fault: {name} runtime version mismatch "
                    f"(manifest={s['version']} runtime_output={s['runtime_version_output']!r})")
    return m


# ---------------------------------------------------------------- 迁移依赖谓词（D3）
MIGRATION_DEPS = ["supervisor.01", "supervisor.02", "supervisor.03",
                  "session.01", "session.02", "session.03", "prompt.01"]


def migration_deps_satisfied(results_file: Path) -> Optional[list[str]]:
    """返回 None=满足；否则返回缺失项列表。
    文件缺失 → ["<missing: results.jsonl>"]；解析/结构故障 → harness_fail（4）。
    机械谓词：该 probe 最后一条记录 evidence_validity=valid 且 exec_status∈{PASS,FAIL}；
    记录必须过完整结果校验、probe_id 无重复冲突（同 ID 多条取最后，重复本身不拒绝——
    重试契约禁止同 run 重跑，同 run 重复属 harness 异常 → 4）。
    """
    if not results_file.is_file():
        return ["<missing: results.jsonl>"]
    records: list[dict] = []
    try:
        for line in results_file.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            obj = json.loads(line)
            validate_result(obj, run_id())
            records.append(obj)
    except (json.JSONDecodeError, UnicodeDecodeError) as e:
        harness_fail(f"results.jsonl parse failure: {e}")
    seen: dict[str, int] = {}
    for r in records:
        seen[r["probe_id"]] = seen.get(r["probe_id"], 0) + 1
    dup = [k for k, c in seen.items() if c > 1]
    if dup:
        harness_fail(f"duplicate probe_id in results.jsonl: {dup}")
    last = {r["probe_id"]: r for r in records}
    missing = [n for n in MIGRATION_DEPS
               if n not in last
               or last[n]["evidence_validity"] != "valid"
               or last[n]["exec_status"] not in ("PASS", "FAIL")]
    return missing or None


# ---------------------------------------------------------------- 阶段汇总（D3）
def phase_priority_max(best: int, code: int) -> int:
    """4→1→3→0 优先级（非数值序）；未知阶段码视为 harness 故障。"""
    if code == 4:
        return 4
    if code == 1:
        return 4 if best == 4 else 1
    if code == 3:
        return best if best in (4, 1) else 3
    if code == 0:
        return best
    return 4


# ---------------------------------------------------------------- 上游执行（D2/D5）
def _upstream_env(envroot: Path, password: Optional[str]) -> dict[str, str]:
    env = {
        "HOME": str(envroot / "home"),
        "XDG_CONFIG_HOME": str(envroot / "xdg" / "config"),
        "XDG_DATA_HOME": str(envroot / "xdg" / "data"),
        "XDG_STATE_HOME": str(envroot / "xdg" / "state"),
        "XDG_CACHE_HOME": str(envroot / "xdg" / "cache"),
        "TMPDIR": str(envroot / "tmp"),
        "PATH": "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
    }
    if password is not None:
        env["OPENCODE_SERVER_PASSWORD"] = password
    for sub in ("home", "xdg/config", "xdg/data", "xdg/state", "xdg/cache", "tmp"):
        sandbox_mkdir(envroot / sub)   # F1：受保护入口
    return env


def assert_log_path(p: Path) -> None:
    """F28：日志目标写入前防护——允许 canonical run 根或 sandbox 根之一，
    祖先链无逃逸链接；两者之外一律前置拒绝。"""
    try:
        run_root = canonical(evidence_dir())
    except Exception:
        run_root = None
    for root in (run_root, sandbox_root()):
        if root is None:
            continue
        cp = canonical(p)
        if cp == root or root in cp.parents:
            assert_ancestors_contained(p, root)
            return
    pre_reject(f"log target escapes allowed roots: {p} -> {canonical(p)}")


def spawn_upstream(envroot: Path, cwd: Path, argv: list[str],
                   log_file: Optional[Path] = None,
                   password: Optional[str] = None) -> subprocess.Popen:
    """后台启动上游进程（独立进程组）；stdout/stderr 经脱敏线程写入 log_file。
    返回 Popen；调用方负责登记 ProcessRegistry 并按层清理。"""
    env = _upstream_env(envroot, password)
    sandbox_mkdir(cwd)
    if log_file is not None:
        assert_log_path(log_file)  # F1/F28：run 根与 sandbox 根均合法
        log_file.parent.mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen(
        argv, cwd=str(cwd), env=env,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        start_new_session=True, text=True, errors="replace",  # F4：解码容错
    )
    if log_file is not None and proc.stdout is not None:
        import threading

        def _pump():
            # F1/F4：逐行经受保护追加入口写入；解码/写入故障登记到泵错误表。
            assert proc.stdout is not None
            try:
                for line in proc.stdout:
                    protected_append_path(log_file, line)
            except SpikeExit as e:
                _PUMP_ERRORS.append(f"{log_file}: SpikeExit({e.code}): {e}")
            except (OSError, UnicodeError) as e:
                _PUMP_ERRORS.append(f"{log_file}: {type(e).__name__}: {e}")
            finally:
                try:
                    proc.stdout.close()
                except OSError:
                    pass

        t = threading.Thread(target=_pump, daemon=True)
        t.start()
        _PUMP_THREADS.append(t)
    return proc


# 日志泵故障登记表（F4）：非空即 harness 故障证据。
_PUMP_ERRORS: list[str] = []
_PUMP_THREADS: list[Any] = []


def pump_errors() -> list[str]:
    return list(_PUMP_ERRORS)


def pump_join_all(timeout: float = 5.0) -> None:
    """F4：进程清理后 join 泵线程，再消费泵错误。"""
    deadline = time.monotonic() + timeout
    for t in _PUMP_THREADS:
        remain = deadline - time.monotonic()
        if remain <= 0:
            break
        t.join(timeout=max(0.0, remain))


def pump_still_alive() -> bool:
    """F4：join 超时后仍有泵线程存活（stdout 未 EOF/写入阻塞）。"""
    return any(t.is_alive() for t in _PUMP_THREADS)


def run_upstream(envroot: Path, cwd: Path, argv: list[str],
                 timeout: float = 30.0, log_file: Optional[Path] = None,
                 password: Optional[str] = None,
                 registry: Optional["ProcessRegistry"] = None,
                 layer: str = "item") -> subprocess.CompletedProcess:
    """以最小隔离环境执行上游二进制：清除全部继承变量（env -i 语义），
    仅显式传入沙箱 HOME/XDG_*/TMPDIR、最小 PATH 与凭据；超时杀整棵进程树。
    给了 registry 时登记子进程（F9：同步上游也纳入三层清理）。"""
    env = _upstream_env(envroot, password)
    sandbox_mkdir(cwd)
    proc = subprocess.Popen(
        argv, cwd=str(cwd), env=env,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        start_new_session=True, text=True, errors="replace",
    )
    registered = False
    try:
        if registry is not None:
            registry.add(layer, proc.pid)
            registered = True
    except Exception:
        # F41：add 可能已部分写入 PID——杀进程后尽力移除，再向上抛出
        kill_tree(proc.pid)
        if registry is not None:
            try:
                registry.remove(layer, proc.pid)
            except SpikeExit:
                pass
        raise
    remove_exc: Optional[BaseException] = None
    try:
        try:
            out, _ = proc.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            kill_tree(proc.pid)
            harness_fail(f"upstream exec timeout after {timeout}s: {argv[0]}")
        if log_file is not None:
            # F1：同步可选日志也走受保护入口（与 spawn_upstream 同一守卫）
            assert_log_path(Path(log_file))
            safe_append(Path(log_file).parent, Path(log_file).name, out or "")
        return subprocess.CompletedProcess(argv, proc.returncode, stdout=out)
    finally:
        # F40/F41：无论正常结束、超时还是日志落盘失败，都必须移除已结束 PID；
        # remove 失败不得吞掉成功返回——PID 状态不确定即 harness 故障
        if registered and registry is not None:
            try:
                registry.remove(layer, proc.pid)
            except SpikeExit as e:
                remove_exc = e
        if remove_exc is not None:
            if isinstance(remove_exc, SpikeExit) and remove_exc.code in (
                    EXIT_PRE_REJECT, EXIT_SAFETY):
                raise remove_exc
            harness_fail(f"registry.remove failed after upstream exit: {remove_exc}")


def kill_tree(root_pid: int, grace: float = 5.0) -> None:
    """TERM 进程组 → 递归后代 TERM → 等待 grace 秒（单调时钟）→ KILL 残余 → 收割（F9）。

    start_new_session 启动的进程 pgid=pid，先整组 TERM 覆盖未登记后代；
    后代清单在 TERM 前抓取，期间新生的进程由进程组信号覆盖。"""
    pids = [p for p in ([root_pid] + _descendants(root_pid)) if p != os.getpid()]
    # F42：registry 文件在 run-all 与阶段子进程间共享——阶段进程清理时可能读到
    # 自身 pid（run-all 登记的正是该阶段进程）；对自身发 SIGTERM 会触发本进程
    # handler → cleanup → kill_tree 无限递归。自身与自身进程组永远不在清理对象内。
    # F42 修复：进程组信号同样必须排除自身组——root_pid 等于自身 pid 或自身
    # 进程组 pgid 时，killpg 会波及本进程；此时只做逐 pid 定向清理后代。
    _own_pid = os.getpid()
    _own_pgid = os.getpgrp()
    group_safe = root_pid != _own_pid and root_pid != _own_pgid
    if pids and group_safe:
        try:
            os.killpg(root_pid, signal.SIGTERM)  # 独立进程组整体终止
        except (ProcessLookupError, PermissionError, OSError):
            pass
    for p in pids:
        try:
            os.kill(p, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            pass
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        if not any(_alive(p) for p in pids):
            break
        time.sleep(0.2)
    for p in pids:
        try:
            os.kill(p, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
    if group_safe:
        try:
            os.killpg(root_pid, signal.SIGKILL)  # 进程组 KILL 兜底（F9）
        except (ProcessLookupError, PermissionError, OSError):
            pass
    # 收割直接子进程，避免僵尸（F9：KILL 后有界等待收割，单次 WNOHANG 不保证完成；
    # 非子进程会抛 ChildProcessError，忽略）
    reap_deadline = time.monotonic() + 2.0
    for p in pids:
        while time.monotonic() < reap_deadline:
            try:
                done, _ = os.waitpid(p, os.WNOHANG)
            except (ChildProcessError, OSError):
                break   # 非子进程或已收割
            if done == p:
                break
            time.sleep(0.05)


def _alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def _descendants(pid: int) -> list[int]:
    out: list[int] = []
    try:
        r = subprocess.run(["pgrep", "-P", str(pid)], capture_output=True, text=True)
        for tok in r.stdout.split():
            child = int(tok)
            out.append(child)
            out.extend(_descendants(child))
    except (OSError, ValueError):
        pass
    return out


class ProcessRegistry:
    """子进程清理三层（D5）：item（单项专属）/ shared（fixture 所有者）/ 全阶段。"""

    def __init__(self, run_dir: Path):
        self.run_dir = run_dir
        self._files = {
            "item": run_dir / ".item.pids",
            "shared": run_dir / ".shared.pids",
        }

    def add(self, layer: str, pid: int) -> None:
        if layer not in self._files:
            harness_fail(f"unknown pid layer: {layer}")
        f = self._files[layer]
        _check_write_target(self.run_dir, f.name)   # F1：PID 文件写入前校验
        safe_append(self.run_dir, f.name, f"{pid}\n")

    def remove(self, layer: str, pid: int) -> None:
        """F40：同步进程正常结束后从 registry 移除，避免后续 cleanup 命中复用 PID。"""
        if layer not in self._files:
            harness_fail(f"unknown pid layer: {layer}")
        f = self._files[layer]
        _check_write_target(self.run_dir, f.name)
        if not f.exists():
            return
        kept = [tok for tok in f.read_text().split() if tok.strip() and tok.strip() != str(pid)]
        evidence_write(self.run_dir, f.name, ("\n".join(kept) + ("\n" if kept else "")))

    def cleanup_layer(self, layer: str) -> None:
        f = self._files.get(layer)
        if f is None:
            harness_fail(f"unknown pid layer: {layer}")
        if not f.exists():
            return
        for tok in f.read_text().split():
            if tok.strip():
                kill_tree(int(tok))
        _check_write_target(self.run_dir, f.name)   # F1：清空前同样校验
        evidence_write(self.run_dir, f.name, "")

    def cleanup_all(self) -> None:
        self.cleanup_layer("item")
        self.cleanup_layer("shared")


def install_signal_handlers(registry: ProcessRegistry) -> None:
    """信号/退出时整阶段清理（第③层）。父进程死亡由 watchdog 线程覆盖。"""
    import atexit
    import threading

    def _cleanup():
        try:
            registry.cleanup_all()
        except Exception:
            pass

    atexit.register(_cleanup)
    for sig in (signal.SIGINT, signal.SIGTERM):
        def handler(_s, _f):
            # F42：防重入——cleanup 内部的 kill_tree 可能再次触发本 handler；
            # 恢复默认处理器后重入即按默认语义终止，不再递归
            signal.signal(_s, signal.SIG_DFL)
            _cleanup()
            sys.exit(EXIT_HARNESS)
        signal.signal(sig, handler)

    parent = os.getppid()

    def _watchdog():
        while _alive(parent):
            time.sleep(1)
        # 父亡：清理登记进程并终止主流程（禁止父亡后继续启动新进程，F9）
        _cleanup()
        try:
            os.kill(os.getpid(), signal.SIGTERM)
        except OSError:
            os._exit(EXIT_HARNESS)

    threading.Thread(target=_watchdog, daemon=True).start()


# ---------------------------------------------------------------- HTTP 助手
class HttpResult:
    """传输错误与 HTTP 状态分离：error 非空时表示未获得 HTTP 响应。"""

    def __init__(self, status: Optional[int], body: bytes, error: Optional[str] = None):
        self.status = status      # None = 传输失败
        self.body = body
        self.error = error        # 传输错误描述

    @property
    def ok(self) -> bool:
        return self.status is not None and 200 <= self.status < 300


def http_request(url: str, method: str = "GET", password: Optional[str] = None,
                 json_body: Optional[Any] = None, timeout: float = 30.0) -> HttpResult:
    """urllib 实现的 HTTP 调用。basic auth（opencode:<password>）。
    传输失败/正文截断 status=None（F24：成功与错误响应统一经完整性读取器）。"""
    req = urllib.request.Request(url, method=method)
    # F43：Cloudflare 拦 urllib 默认 UA（容器内 403）——显式声明客户端 UA
    req.add_header("User-Agent", "opencode-spike-harness/1.0 (python-urllib)")
    if password is not None:
        import base64
        req.add_header("Authorization",
                       "Basic " + base64.b64encode(f"opencode:{password}".encode()).decode())
    data = None
    if json_body is not None:
        req.add_header("Content-Type", "application/json")
        data = json.dumps(json_body).encode()

    def _read_full(resp: Any, deadline: float) -> bytes:
        chunks: list[bytes] = []
        while True:
            remain = deadline - time.monotonic()
            if remain <= 0:
                raise TimeoutError(f"http read total deadline exceeded: {url}")
            # F9：单次 read 的 socket 超时随剩余总时限收缩——阻塞读不能越过总 deadline
            sock = getattr(getattr(getattr(resp, "fp", None), "raw", None), "_sock", None)
            if sock is not None:
                try:
                    sock.settimeout(max(0.1, remain))
                except OSError:
                    pass
            # F9：read1 及时返回已到达数据（trickle 下不会为凑满 64K 阻塞到底）；
            # 无 read1 的回退小读，逐次重检 deadline
            read1 = getattr(resp, "read1", None)
            try:
                raw_chunk: Any = read1(65536) if callable(read1) else resp.read(4096)
                chunk: bytes = raw_chunk if isinstance(raw_chunk, bytes) else b""
            except (TimeoutError, OSError) as te:
                # socket 超时到期 → 视为总 deadline 耗尽
                raise TimeoutError(f"http read deadline exceeded: {url}: {te}") from te
            if not chunk:
                break
            chunks.append(chunk)
        body = b"".join(chunks)
        declared = resp.headers.get("Content-Length") if hasattr(resp, "headers") else None
        if declared is not None:
            try:
                if len(body) != int(declared):
                    raise IncompleteBodyError(
                        f"incomplete body: got {len(body)} != Content-Length {declared}")
            except ValueError:
                pass
        return body

    deadline = time.monotonic() + timeout
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as resp:
            return HttpResult(resp.status, _read_full(resp, deadline))
    except urllib.error.HTTPError as e:
        # HTTP 错误状态属协议事实；但正文读取仍受完整性/总时限约束（F24）
        try:
            remaining = deadline - time.monotonic()
            err_body = _read_full(e, time.monotonic() + max(0.0, remaining)) \
                if e.fp else b""
        except Exception as be:
            return HttpResult(None, b"", f"{type(be).__name__}: {be}")
        return HttpResult(e.code, err_body)
    except Exception as e:  # 含 IncompleteBodyError/IncompleteRead/BadStatusLine/URLError/TimeoutError
        return HttpResult(None, b"", f"{type(e).__name__}: {e}")


class IncompleteBodyError(Exception):
    """HTTP 正文完整性校验失败（Content-Length 不符等）。"""


def fetch_with_retry(url: str, timeout: float = 30.0,
                     on_attempt: Optional[Any] = None) -> HttpResult:
    """D5 统一重试契约第 2 条：仅传输类失败（无 HTTP 响应）允许重试，至多 2 次、
    间隔 ≥5s；HTTP 错误状态属于协议事实，不重试（F14）。
    on_attempt(attempt_no, result) 用于逐次落证据。"""
    last: Optional[HttpResult] = None
    for attempt in range(3):
        if attempt > 0:
            time.sleep(5)
        r = http_request(url, timeout=timeout)
        last = r
        if on_attempt is not None:
            on_attempt(attempt + 1, r)
        if r.status is not None:
            return r
    assert last is not None
    return last


# ---------------------------------------------------------------- SSE 解帧（D4 event.02）
class SSEParser:
    """增量 SSE 解帧器（F8）：feed() 逐行输入，仅在空行提交完整帧；
    不强制 flush 尾部未完成帧（轮询文件场景：未完整帧留给后续读取）。
    处理 data: 多行积累、CRLF、注释（: 开头）。"""

    def __init__(self) -> None:
        self._buf: list[str] = []

    def feed_line(self, raw: str) -> Optional[dict]:
        line = raw.rstrip("\r\n")
        if line == "":
            return self._flush()
        if line.startswith(":"):
            return None
        if line.startswith("data:"):
            self._buf.append(line[5:].lstrip())
        # event:/id:/retry: 等字段行忽略（事件类型从 payload.type 读取）
        return None

    def _flush(self) -> Optional[dict]:
        if not self._buf:
            return None
        payload = "\n".join(self._buf)
        self._buf.clear()
        try:
            obj = json.loads(payload)
        except json.JSONDecodeError:
            return None
        return obj if isinstance(obj, dict) else None


def sse_parse_line_events(lines: list[str]) -> list[dict]:
    """便捷批解析：仅返回完整帧（不以 EOF 强提未完成帧）。"""
    parser = SSEParser()
    out: list[dict] = []
    for line in lines:
        ev = parser.feed_line(line)
        if ev is not None:
            out.append(ev)
    return out


def _payload_dicts(ev: dict) -> list[dict]:
    """事件载荷候选对象：properties / properties.info / properties.session / data / 顶层。
    v2 事件载荷在 data 键下（session.created → data.sessionID 等）。"""
    out: list[dict] = []
    if isinstance(ev, dict):
        out.append(ev)
        props = ev.get("properties")
        if isinstance(props, dict):
            out.append(props)
            info = props.get("info")
            if isinstance(info, dict):
                out.append(info)
            sess = props.get("session")
            if isinstance(sess, dict):
                out.append(sess)
        data = ev.get("data")
        if isinstance(data, dict):
            out.append(data)
    return out


def payload_has_field(ev: dict, name: str) -> bool:
    """F8：字段必须作为真实 JSON 键存在，文本出现（如 note='status'）不算。"""
    return any(name in d for d in _payload_dicts(ev))


_STATUS_STATES = frozenset({"idle", "busy", "retry"})


def status_detail_ok(ev: dict) -> bool:
    """F8：StatusDetail 必须有真实 status 字段，且状态 ∈ {idle,busy,retry}。
    接受字符串状态或 {type|state|status: <enum>} 对象；retry 明细可选。"""
    for d in _payload_dicts(ev):
        if "status" not in d:
            continue
        status = d["status"]
        if isinstance(status, str) and status.strip().lower() in _STATUS_STATES:
            return True
        if isinstance(status, dict):
            state = status.get("type") or status.get("state") or status.get("status")
            if isinstance(state, str) and state.strip().lower() in _STATUS_STATES:
                return True
    return False


def event_session_id(ev: dict) -> Optional[str]:
    """F8/F11：多层 session 归属解析（properties.sessionID / info.id / session.id /
    data.sessionID / 顶层）。v2 事件载荷在 data 键下。"""
    raw_props = ev.get("properties")
    props: dict = raw_props if isinstance(raw_props, dict) else {}
    info_raw = props.get("info")
    sess_raw = props.get("session")
    info: dict = info_raw if isinstance(info_raw, dict) else {}
    sess: dict = sess_raw if isinstance(sess_raw, dict) else {}
    raw_data = ev.get("data")
    data: dict = raw_data if isinstance(raw_data, dict) else {}
    sid_val = (props.get("sessionID") or info.get("id") or sess.get("id")
               or data.get("sessionID") or ev.get("sessionID") or "")
    return sid_val if isinstance(sid_val, str) and sid_val else None


def event_matches(ev: dict, type_candidates: tuple[str, ...],
                  sid: Optional[str], required_field: Optional[str] = None,
                  payload_ok: Optional[Any] = None) -> bool:
    """精确 type ∈ 候选表 + session 归属 + 可选真实字段 / 载荷谓词（F8）。
    归属字段按发现顺序尝试：properties.sessionID / properties.info.id /
    properties.session.id / 顶层 sessionID。"""
    etype = ev.get("type")
    if not isinstance(etype, str) or etype not in type_candidates:
        return False
    if sid is not None:
        if event_session_id(ev) != sid:
            return False
    if required_field is not None and not payload_has_field(ev, required_field):
        return False
    if payload_ok is not None and not payload_ok(ev):
        return False
    return True


def invalidate_matching_results(run_dir: Path, locked: dict,
                                match: Any, reason_suffix: str) -> int:
    """F4：将匹配且仍为 valid 的结果改写为 harness_invalid（原子替换）。
    match(obj) -> bool。返回改写条数。"""
    results = run_dir / "results.jsonl"
    if not results.is_file():
        return 0
    try:
        content = results.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        harness_fail("results.jsonl unreadable during pump invalidation")
    changed = 0
    out: list[str] = []
    for line in content.splitlines():
        if not line.strip():
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            out.append(line)
            continue
        if (isinstance(obj, dict) and obj.get("evidence_validity") == "valid"
                and match(obj)):
            obj["evidence_validity"] = "harness_invalid"
            obj["reason"] = (obj.get("reason") or "") + reason_suffix
            changed += 1
            try:
                validate_result(obj, run_id(), locked)
            except SpikeExit:
                pass
            out.append(json.dumps(obj, ensure_ascii=False))
        else:
            out.append(line if not isinstance(obj, dict)
                       else json.dumps(obj, ensure_ascii=False))
    evidence_write(run_dir, "results.jsonl",
                   "\n".join(out) + ("\n" if out else ""))
    return changed
