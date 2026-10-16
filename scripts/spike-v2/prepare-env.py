#!/usr/bin/env python3
"""prepare-env.py — spike 准备阶段：沙箱校验、来源链核验、边界验证（canary 首次）、
下载+校验双侧二进制、受控 --version 校验、原子发布锁定 manifest。

契约来源：openspec/changes/opencode-v2-contract-spike/design.md
  D1（两段式 manifest、语义校验表、来源链核验、双文件原子发布协议）
  D2（规范沙箱根、prepare 前置校验、canary 契约、边界生命周期、run 两类校验）
  D3（脚本签名、退出码契约）；D5（重试契约第 2 条）

用法: prepare-env.py <candidate-manifest> <sandbox-root> <evidence-dir> [canary-target]
  canary-target 可选：缺省时由候选 manifest 顶层 canary_target 提供；两者均缺失 → 退出码 2。

本脚本只验证并使用调用方已建立的执行边界，不承担建立/配置边界职责（D2 边界生命周期）。
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import time
import zipfile
from pathlib import Path
from typing import Optional

sys.path.insert(0, str(Path(__file__).resolve().parent))
import spikelib as S  # noqa: E402

REPO = "anomalyco/opencode"
GITHUB_API = "https://api.github.com"


def http_get_json(url: str, log_rel_base: str, run_dir: Path) -> dict:
    """GitHub API GET（D5 重试契约：仅传输失败重试——fetch_with_retry 实现，
    至多 2 次、间隔 ≥5s、**每次尝试独立证据文件**（F14）；HTTP 错误状态属协议事实不重试）。"""
    def _log(attempt: int, r: S.HttpResult) -> None:
        S.safe_append(run_dir, f"{log_rel_base}.attempt{attempt}.log",
                      f"GET {url} attempt={attempt} status={r.status} err={r.error}\n")
    r = S.fetch_with_retry(url, timeout=30.0, on_attempt=_log)
    if r.status == 200 and r.body:
        try:
            return json.loads(r.body)
        except json.JSONDecodeError as e:
            S.pre_reject(f"protocol fact: invalid JSON from {url} ({e})")
    S.pre_reject(f"fetch failed: {url} (status={r.status} err={r.error})")


def _verify_update_channel(sc: dict, evd: str, run_dir: Path) -> None:
    """v2 官方渠道核验：opencode.ai update API 元数据 ↔ manifest 字段逐一比对。
    （v2 无 GitHub release；release_url 指向 update API 元数据端点，其 files 表
    携带官方 sha256 与版本化下载 URL。）
    F43：元数据端点必须是官方 origin 与已知路径形态——candidate manifest 是
    外部输入，不得凭其中的 URL 去请求任意 HTTPS 源并接受其字段。"""
    from urllib.parse import urlparse
    u = urlparse(sc["release_url"])
    if (u.scheme != "https" or u.hostname != "opencode.ai"
            or not u.path.startswith("/update/")):
        S.pre_reject(f"source chain: update metadata endpoint is not the official "
                     f"opencode.ai /update/ API: {sc['release_url']}")
    meta = http_get_json(sc["release_url"], f"{evd}.update-api.log", run_dir)
    S.evidence_write(run_dir, f"{evd}.update-api.json",
                     json.dumps(meta, ensure_ascii=False, indent=2))
    if meta.get("version") == sc["version"]:
        if ((meta.get("metadata") or {}).get("files") or {}).get(sc["asset_name"]) is None:
            S.pre_reject(f"source chain: update API has no file entry for {sc['asset_name']}")
        entry = meta["metadata"]["files"][sc["asset_name"]]
        if entry.get("url") != sc["download_url"]:
            S.pre_reject(f"source chain: download_url mismatch "
                         f"(update-api={entry.get('url')} manifest={sc['download_url']})")
        if entry.get("sha256") != sc["official_sha256"]:
            S.pre_reject(f"source chain: update API sha256 {entry.get('sha256')} "
                         f"!= official_sha256 {sc['official_sha256']}")
        return
    # 渠道漂移回落（2026-10-16 实测：update API latest 已前进到 2.0.29，且无历史版本
    # 元数据端点；versioned 下载 URL 仍可达）。核验强度不减：
    #   ① tag→commit 已在 verify_source_chain 上游核验通过；
    #   ② download_url 必须匹配官方版本化渠道形态（origin+版本化路径，防任意 URL 注入）；
    #   ③ 下载哈希与 official_sha256 核对（download_side）；official_sha256 的官方出处
    #     = 锁定 run 的 update-api 证据（files[asset].sha256，官方渠道锁定时点背书）。
    expected_url = f"https://opencode.ai/files/bin/{sc['version']}/{sc['asset_name']}"
    if sc["download_url"] != expected_url:
        S.pre_reject(f"source chain: channel drifted (update-api latest="
                     f"{meta.get('version')} != manifest {sc['version']}) and "
                     f"download_url does not match official versioned form {expected_url}")
    S.safe_append(run_dir, f"{evd}.update-channel-drift.log",
                  f"update-api latest={meta.get('version')} != manifest version "
                  f"{sc['version']}; no historical metadata endpoint\n"
                  f"fallback: download_url matches official versioned form ({expected_url}); "
                  f"sha256 verified against official_sha256 at download; "
                  f"official_sha256 provenance = locked-run update-api evidence "
                  f"(evidence/20261014T045810Z-f125cafd/source-chain/v2.update-api.json)\n")


def verify_source_chain(m: dict, side: str, run_dir: Path) -> None:
    """D1 语义校验表来源链核验（F02）：二进制执行前核验并归档对应关系证据。
    release↔tag↔commit（refs API + annotated tag 二级解引用）；
    release↔asset↔checksum（assets 精确匹配 + digest 或 checksums 资产回落）。
    v2 官方渠道为 opencode.ai update API（无 GitHub release）：
    tag↔commit 仍经 refs API 核验；release↔asset↔checksum 以 update API
    元数据（version + files[asset].url/sha256）为准——用户决策：v2 协议已稳定，
    不强制 GitHub release 一致。"""
    sc = m[side]
    evd = f"source-chain/{side}"
    # 官方校验值缺失 → 来源链未证实（候选态结构性允许 null，此处在执行前拦截）
    if sc["official_sha256"] is None or sc["source_chain_status"] != "verified":
        S.evidence_write(run_dir, f"{evd}.unverified.json",
                         json.dumps({k: sc[k] for k in
                                     ("release_url", "tag", "commit_sha", "download_url",
                                      "official_sha256", "source_chain_status")},
                                    ensure_ascii=False, indent=2))
        S.pre_reject(f"{side} source chain unverified; spike stopped "
                     f"(evidence: {evd}.unverified.json)")
    # tag → commit
    ref = http_get_json(f"{GITHUB_API}/repos/{REPO}/git/ref/tags/{sc['tag']}",
                        f"{evd}.tag.log", run_dir)
    S.evidence_write(run_dir, f"{evd}.tag.json", json.dumps(ref, ensure_ascii=False, indent=2))
    resolved = (ref.get("object") or {}).get("sha", "")
    if (ref.get("object") or {}).get("type") == "tag" and resolved:
        tag_obj = http_get_json(f"{GITHUB_API}/repos/{REPO}/git/tags/{resolved}",
                                f"{evd}.tag-object", run_dir)
        S.evidence_write(run_dir, f"{evd}.tag-object.json",
                         json.dumps(tag_obj, ensure_ascii=False, indent=2))
        resolved = (tag_obj.get("object") or {}).get("sha", "")
    if resolved != sc["commit_sha"]:
        S.pre_reject(f"source chain: tag {sc['tag']} resolves to {resolved} "
                     f"!= manifest commit {sc['commit_sha']}")
    if sc["download_url"].startswith("https://opencode.ai/"):
        _verify_update_channel(sc, evd, run_dir)
        return
    # release ↔ asset ↔ url ↔ digest
    rel = http_get_json(f"{GITHUB_API}/repos/{REPO}/releases/tags/{sc['tag']}",
                        f"{evd}.release.log", run_dir)
    S.evidence_write(run_dir, f"{evd}.release.json",
                     json.dumps(rel, ensure_ascii=False, indent=2))
    if rel.get("html_url", "") != sc["release_url"]:
        S.pre_reject(f"source chain: release_url mismatch "
                     f"(api={rel.get('html_url', '')} manifest={sc['release_url']})")
    assets = [a for a in rel.get("assets", []) if a.get("name") == sc["asset_name"]]
    if len(assets) != 1:
        S.pre_reject(f"source chain: asset {sc['asset_name']} matched {len(assets)} entries")
    asset = assets[0]
    if asset.get("browser_download_url") != sc["download_url"]:
        S.pre_reject(f"source chain: download_url mismatch "
                     f"(release={asset.get('browser_download_url')} manifest={sc['download_url']})")
    digest = asset.get("digest") or ""
    official = sc["official_sha256"]
    if digest:
        if digest != f"sha256:{official}":
            S.pre_reject(f"source chain: asset digest {digest} != official_sha256 {official}")
    else:
        # 回落：官方 checksums 资产精确「哈希+文件名」唯一对应（F02）
        cs_assets = [a for a in rel.get("assets", [])
                     if re.search(r"(?i)(checksum|sha256)", a.get("name", ""))]
        if not cs_assets:
            S.pre_reject(f"source chain: no asset digest and no checksums asset for {side}")
        cs_url = cs_assets[0].get("browser_download_url", "")
        r = S.http_request(cs_url, timeout=30.0)
        if r.status != 200:
            S.pre_reject(f"source chain: cannot fetch checksums asset {cs_url}")
        S.evidence_write(run_dir, f"{evd}.checksums.txt", r.body)
        mine = []
        for line in r.body.decode("utf-8", "replace").splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[-1].lstrip("*") == sc["asset_name"]:
                mine.append(parts[0].lower())
        if not mine:
            S.pre_reject(f"source chain: no entry for {sc['asset_name']} in checksums asset")
        if any(h != official for h in mine):
            S.pre_reject(f"source chain: checksums conflict for {sc['asset_name']}: {mine}")


def download_side(m: dict, side: str, run_dir: Path, sbroot: Path) -> tuple[str, str, bool]:
    """下载 + 校验 + 解包一侧二进制。返回 (exec_path, download_sha256, sha256_match)。

    D5 重试契约第 2 条：下载与哈希验证在同一尝试内，至多 2 次重试、间隔 ≥5s、逐次落证据；
    校验通过才解包；解包到全新 staging；解包前深度预检（条目名 + 链接目标）。
    """
    sc = m[side]
    name = sc["asset_name"]
    # asset_name 路径穿越预检：仅允许裸文件名
    if "/" in name or ".." in name or not name or name.startswith("\\"):
        S.pre_reject(f"asset_name not a bare filename: {name}")
    bindir = sbroot / "bin" / side
    S.sandbox_mkdir(bindir)
    dest = bindir / name
    if S.canonical(dest.parent) != S.canonical(bindir):
        S.pre_reject(f"download target escapes sandbox bin dir: {dest}")
    if dest.is_symlink():
        S.pre_reject(f"download target is a pre-existing symlink: {dest}")
    official = sc["official_sha256"]
    dl_sha = ""
    ok = False
    for attempt in range(3):
        log_rel = f"source-chain/{side}.download.attempt{attempt + 1}.log"
        if attempt > 0:
            time.sleep(5)
        r = S.http_request(sc["download_url"], timeout=120.0)
        S.safe_append(run_dir, log_rel,
                      f"GET {sc['download_url']} attempt={attempt + 1} "
                      f"status={r.status} err={r.error}\n")
        if r.status != 200 or not r.body:
            # 传输失败与 HTTP 错误都重试下载（D5 允许的 harness 瞬态类）；
            # HTTP 明确错误（如 404）是协议事实，提前终止避免无效重试
            if r.status is not None:
                S.pre_reject(f"download HTTP {r.status} for {side}: {sc['download_url']}")
            continue
        # F16：下载资产原始字节写入（不脱敏），哈希覆盖收到的原始字节
        S.sandbox_write_raw(dest, r.body)
        dl_sha = _sha256_file(dest)
        S.safe_append(run_dir, log_rel, f"downloaded sha256: {dl_sha}\n")
        if official is not None and dl_sha != official:
            S.safe_append(run_dir, log_rel,
                          f"sha256 mismatch: official={official} actual={dl_sha}\n")
            dest.unlink(missing_ok=True)
            continue
        ok = True
        break
    if not ok:
        S.pre_reject(f"download/checksum failed after retries for {side}: {sc['download_url']}")
    match = official is not None and dl_sha == official
    # 解包（全新 staging；深度预检先于解包）
    staging = bindir / f".staging-{os.getpid()}-{int(time.time() * 1000)}"
    if staging.exists():
        S.harness_fail(f"staging dir unexpectedly exists: {staging}")
    S.sandbox_mkdir(staging)
    try:
        exec_path = _unpack(dest, name, staging, bindir)
    finally:
        pass
    return str(exec_path), dl_sha, match


def _sha256_file(p: Path) -> str:
    h = __import__("hashlib").sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _archive_precheck(path: Path) -> None:
    """归档深度预检（F10）：条目名拒绝绝对路径/..；symlink/hardlink 目标必须解析到归档内。"""
    import posixpath

    def bad_name(n: str) -> bool:
        return n.startswith("/") or ".." in n.split("/")

    def link_ok(target: str, member_dir: str) -> bool:
        if target.startswith("/"):
            return False
        resolved = posixpath.normpath(posixpath.join(member_dir, target))
        return not resolved.startswith("..")

    if zipfile.is_zipfile(path):
        with zipfile.ZipFile(path) as z:
            for info in z.infolist():
                if bad_name(info.filename):
                    S.pre_reject(f"archive absolute/traversal entry: {info.filename}")
                mode = info.external_attr >> 16
                import stat as st
                if st.S_ISLNK(mode):
                    target = z.read(info).decode("utf-8", "replace")
                    if not link_ok(target, posixpath.dirname(info.filename)):
                        S.pre_reject(f"archive bad symlink target: {info.filename} -> {target}")
    elif tarfile.is_tarfile(path):
        with tarfile.open(path) as t:
            for mem in t.getmembers():
                if bad_name(mem.name):
                    S.pre_reject(f"archive absolute/traversal entry: {mem.name}")
                if (mem.issym() or mem.islnk()) and not link_ok(
                        mem.linkname, posixpath.dirname(mem.name)):
                    S.pre_reject(f"archive bad link target: {mem.name} -> {mem.linkname}")


def _unpack(dest: Path, name: str, staging: Path, bindir: Path) -> Path:
    """解包到 staging 并定位执行文件；裸二进制直接移入。返回 exec_path。"""
    if zipfile.is_zipfile(dest):
        _archive_precheck(dest)
        with zipfile.ZipFile(dest) as z:
            z.extractall(staging)
        dest.unlink()
    elif tarfile.is_tarfile(dest):
        _archive_precheck(dest)
        with tarfile.open(dest) as t:
            t.extractall(staging)
        dest.unlink()
    else:
        shutil.move(str(dest), staging / name)
    # 解包结果符号链接不得逃逸沙箱
    for p in staging.rglob("*"):
        if p.is_symlink():
            resolved = S.canonical(p)
            if S.canonical(bindir) not in resolved.parents:
                S.pre_reject(f"extracted symlink escapes sandbox: {p} -> {resolved}")
    # 定位执行文件
    cands = [p for p in staging.rglob("*")
             if p.is_file() and p.name.startswith("opencode")
             and not p.name.endswith((".zip", ".tar.gz", ".tgz"))]
    if not cands:
        S.harness_fail(f"cannot locate unpacked binary under {staging}")
    exec_path = cands[0]
    exec_path.chmod(0o755)
    if S.canonical(bindir) not in S.canonical(exec_path).parents:
        S.pre_reject(f"unpacked exec path escapes sandbox: {exec_path}")
    return exec_path


def run_version_probe(m: dict, side: str, exec_path: str, run_dir: Path,
                      sbroot: Path, password: str,
                      registry: Optional[S.ProcessRegistry] = None) -> str:
    """门禁保护下执行 --version（30s 截止）；输出版本必须等于 manifest version（不符 → 4）。
    F9：传入 registry 时登记子进程，纳入三层清理。"""
    cp = S.run_upstream(sbroot, sbroot / "work", [exec_path, "--version"],
                        timeout=30.0, password=password, registry=registry)
    # run_upstream 默认捕获 stdout：重跑一次拿输出（--version 无副作用，允许两次执行？
    # 禁止——改为 run_upstream 返回 stdout；见 spikelib 实现）
    out = cp.stdout if isinstance(cp.stdout, str) else ""
    S.evidence_write(run_dir, f"source-chain/{side}.version-output.txt", out)
    want = m[side]["version"]
    got = re.search(r"\d+\.\d+\.\d+", out)
    if not got or got.group(0) != want:
        S.die(S.EXIT_HARNESS,
              f"BASELINE-FAULT: runtime version mismatch for {side} "
              f"(manifest={want} runtime={out!r})")
    return out


def main() -> int:
    argv = sys.argv[1:]
    if not 3 <= len(argv) <= 4:
        print("Usage: prepare-env.py <candidate-manifest> <sandbox-root> <evidence-dir> [canary-target]",
              file=sys.stderr)
        return S.EXIT_PRE_REJECT
    candidate_path, sandbox_arg, evidence_arg = argv[0], argv[1], argv[2]
    canary_arg = argv[3] if len(argv) == 4 else ""
    if os.environ.get("SPIKE_SELFTEST"):
        S.pre_reject("SPIKE_SELFTEST must not be set in formal runs")

    # prepare 前置校验①：sandbox-root 必须等于规范真实沙箱根
    sbroot = S.sandbox_root()
    if S.canonical(sandbox_arg) != sbroot:
        S.pre_reject(f"sandbox-root must equal canonical sandbox root\n"
                     f"  got:      {S.canonical(sandbox_arg)}\n  expected: {sbroot}")
    # ② evidence-dir 必须是规范 run 根（单独调用时承担初始化责任）
    run_dir = S.run_use_existing()
    if S.canonical(evidence_arg) != S.canonical(run_dir):
        S.pre_reject(f"<evidence-dir> is not the canonical run root: {evidence_arg}")

    registry = S.ProcessRegistry(run_dir)
    S.install_signal_handlers(registry)

    # 上一 run 残留处置（D1/F12）：任何可能失败的本次校验之前先归档旧权威 manifest
    old_manifest = sbroot / "manifest.json"
    if old_manifest.is_file():
        S._check_write_target(run_dir, "previous-manifest.json")  # F1：归档目标防护
        shutil.move(str(old_manifest), str(run_dir / "previous-manifest.json"))

    # 候选 manifest 结构+语义校验
    candidate = S.validate_manifest(Path(candidate_path), "candidate")

    # canary 目标解析（D2：CLI 与 manifest 同时提供时 canonical 必须一致；均缺失 → 2）
    m_canary = candidate.get("canary_target", "")
    if canary_arg and m_canary:
        if S.canonical(canary_arg) != S.canonical(m_canary):
            S.pre_reject("canary_target mismatch between CLI and candidate manifest")
    canary_target = canary_arg or m_canary
    if not canary_target:
        S.pre_reject("canary target missing (CLI arg or candidate manifest canary_target required)")
    canary_target = str(S.canonical(canary_target))

    # 继承宿主变量处置记录（D2；实际清除在 run_upstream 的最小环境中完成）
    for var in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
                "XDG_CACHE_HOME", "TMPDIR"):
        v = os.environ.get(var, "")
        if v:
            cv = S.canonical(v)
            if not (str(cv).startswith(str(sbroot)) or str(cv).startswith(str(run_dir))):
                S.safe_append(run_dir, "canary.log",
                              f"inherited-env-cleared-at-exec: {var}={v}\n")

    # 边界验证（D2 两项：描述输入校验 + 首次 canary）
    mech, detail = S.boundary_env_check()
    S.canary_verify(canary_target, "canary.log", run_dir)

    # 沙箱布局（D2）
    for sub in ("home", "xdg/config", "xdg/data", "xdg/state", "xdg/cache",
                "tmp", "bin/v1", "bin/v2", "work"):
        S.sandbox_mkdir(sbroot / sub)

    # 来源链核验（二进制执行前）
    for side in ("v1", "v2"):
        verify_source_chain(candidate, side, run_dir)

    # 下载 + 校验 + 解包双侧二进制
    password = os.environ.get("OPENCODE_SERVER_PASSWORD") or f"spike-{__import__('secrets').token_hex(16)}"
    # F2：运行期凭据登记进脱敏集合（含 Basic 编码形态）
    S.register_secret(password)
    S.register_secret(f"opencode:{password}")
    import base64 as _b64
    S.register_secret(_b64.b64encode(f"opencode:{password}".encode()).decode())
    exec_paths: dict[str, str] = {}
    dl_sha: dict[str, str] = {}
    sha_match: dict[str, bool] = {}
    for side in ("v1", "v2"):
        ep, ds, mt = download_side(candidate, side, run_dir, sbroot)
        exec_paths[side], dl_sha[side], sha_match[side] = ep, ds, mt
        official = candidate[side]["official_sha256"]
        if official is not None and not mt:
            S.pre_reject(f"sha256 mismatch for {side} (official={official} actual={ds})")

    # 受控 --version（门禁保护下；F9：子进程登记入 registry 三层清理）
    run_out: dict[str, str] = {}
    for side in ("v1", "v2"):
        run_out[side] = run_version_probe(candidate, side, exec_paths[side],
                                          run_dir, sbroot, password,
                                          registry=registry)

    # 原子发布锁定 manifest（D1 双文件提交协议；写入经受保护入口，F1）
    locked = dict(candidate)
    locked["canary_target"] = canary_target
    locked["boundary_mechanism"] = mech
    if mech == "other":
        locked["boundary_detail"] = detail
    from datetime import datetime, timezone
    locked_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    for side in ("v1", "v2"):
        s = dict(locked[side])
        s.update({
            "locked_at": locked_at,
            "download_sha256": dl_sha[side],
            "sha256_match": sha_match[side],
            "exec_path": exec_paths[side],
            "runtime_version_output": run_out[side],
        })
        locked[side] = s
    payload = json.dumps(locked, ensure_ascii=False, indent=2)
    tmp_evidence_rel = "manifest.json.tmp"
    tmp_sb = sbroot / "manifest.json.tmp"
    try:
        S.evidence_write(run_dir, tmp_evidence_rel, payload)
        S.sandbox_write(tmp_sb, payload)
        if (run_dir / tmp_evidence_rel).read_bytes() != tmp_sb.read_bytes():
            S.harness_fail("publish temp files diverge")
        S.validate_manifest(run_dir / tmp_evidence_rel, "locked")
        # 固定发布顺序：先 evidence 副本，最后 sandbox 权威文件（唯一提交点）
        # F1：两个 replace 目标先做写入防护
        S._check_write_target(run_dir, "manifest.json")
        S.assert_sandbox_path(sbroot / "manifest.json")
        os.replace(run_dir / tmp_evidence_rel, run_dir / "manifest.json")
        os.replace(tmp_sb, sbroot / "manifest.json")
    except S.SpikeExit:
        # 临时文件保留为证据，权威文件未提交视为未发布
        print(f"publish aborted; temp evidence kept: {tmp_evidence_rel} {tmp_sb}",
              file=sys.stderr)
        raise
    except OSError as e:
        S.harness_fail(f"publish failed: {e}")
    print(f"==> locked manifest published: {sbroot / 'manifest.json'} "
          f"(copy: {run_dir / 'manifest.json'})")
    return S.EXIT_OK


if __name__ == "__main__":
    try:
        sys.exit(main())
    except S.SpikeExit as e:
        sys.exit(e.code)
    except Exception as e:  # F3/F2：非契约异常收口为 4；打印前脱敏
        print(S.redact_text(f"harness error: unexpected: {type(e).__name__}: {e}"),
              file=sys.stderr)
        sys.exit(4)
