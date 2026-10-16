#!/usr/bin/env python3
"""live-probe.py — live 层：D4 probe 矩阵（probes 模式）与 D5 迁移实验（migration 模式）。

契约来源：openspec/changes/opencode-v2-contract-spike/design.md
  D3（脚本签名、两模式互斥执行集合、migration 前置依赖谓词、退出码契约）
  D4（逐项 probe 契约、wire 形态发现先行、event.02 总述、fixture DAG、豁免记录）
  D5（fixture 两态生命周期、快照隔离、两步比较谓词、失败分类表、结局六状态机、清理三层）

用法: live-probe.py <locked-manifest> <evidence-dir> [--mode probes|migration]

本脚本只验证并使用调用方已建立的执行边界（D2）；模型依赖项按 D3 映射表允许 INCONCLUSIVE。
每个 probe 恰好输出一条结果记录；阶段退出码按结果聚合（harness 故障 → 4）。
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.parse
from pathlib import Path
from typing import Any, Optional

sys.path.insert(0, str(Path(__file__).resolve().parent))
import spikelib as S  # noqa: E402


# ---- OpenAPI ID pattern 发现（模块级：probes 与 migration 进程共用） ----

def _resolve_ref(doc: Any, ref: Any, depth: int = 0) -> Any:
    """解析本地 $ref（#/components/schemas/...）；限深防环。"""
    if depth > 8 or not isinstance(ref, str) or not ref.startswith("#/"):
        return None
    cur: Any = doc
    for part in ref[2:].split("/"):
        if not isinstance(cur, dict):
            return None
        cur = cur.get(part)
    return cur


def _schema_pattern(doc: Any, schema: Any, depth: int = 0) -> Optional[str]:
    """从 schema 提取 pattern，解析 $ref/anyOf/oneOf/allOf
    （v2 用 $ref 组织 ID schema，如 Session.ID/Session.Message.ID）。"""
    if depth > 8 or not isinstance(schema, dict):
        return None
    pat = schema.get("pattern")
    if isinstance(pat, str):
        return pat
    ref = schema.get("$ref")
    if isinstance(ref, str):
        return _schema_pattern(doc, _resolve_ref(doc, ref, depth + 1), depth + 1)
    for key in ("anyOf", "oneOf", "allOf"):
        subs = schema.get(key)
        if isinstance(subs, list):
            for sub in subs:
                p = _schema_pattern(doc, sub, depth + 1)
                if p is not None:
                    return p
    return None


def _find_pattern(doc: Any, obj: Any, name: str, depth: int = 0) -> Optional[str]:
    """在 OpenAPI 文档树中递归查找名为 name 的参数/属性的 pattern（解析 $ref）。"""
    if depth > 40:
        return None
    if isinstance(obj, dict):
        if obj.get("name") == name:
            p = _schema_pattern(doc, obj.get("schema"))
            if p is not None:
                return p
        props = obj.get("properties")
        if isinstance(props, dict) and name in props:
            p = _schema_pattern(doc, props[name])
            if p is not None:
                return p
        for v in obj.values():
            r = _find_pattern(doc, v, name, depth + 1)
            if r is not None:
                return r
    elif isinstance(obj, list):
        for v in obj:
            r = _find_pattern(doc, v, name, depth + 1)
            if r is not None:
                return r
    return None


def _discover_id_patterns(ctx: "Ctx", base: str, label: str) -> None:
    """面向指定 serve 实例的轻量 ID pattern 发现（migration 进程用——
    probes 进程的 wire 发现结果不跨进程继承，migration 需对自身 serve 重新发现）。
    成功时填充 ctx.sid_pattern/mid_pattern；失败保持 None（调用方按 F5 落 INCONCLUSIVE）。
    证据写入 live/discovery-migration-<label>.txt，区别于 probes 的 discovery.txt。"""
    # F44：发现前先清空——不得把上一实例已发现的 pattern 沿用到未证明的新实例
    ctx.sid_pattern = None
    ctx.mid_pattern = None
    doc: Any = None
    for ep in ("/doc", "/openapi.json"):
        r = ctx.http(f"{base}{ep}", timeout=30.0)
        if r.status == 200 and r.body:
            try:
                cand = json.loads(r.body)
            except json.JSONDecodeError:
                continue
            if isinstance(cand, dict) and isinstance(cand.get("paths"), dict):
                doc = cand
                break
    if doc is None:
        ctx.evidence(f"live/discovery-migration-{label}.txt",
                     f"migration serve {base}: no OpenAPI doc discovered\n")
        return
    ctx.sid_pattern = _find_pattern(doc, doc, "sessionID")
    ctx.mid_pattern = _find_pattern(doc, doc, "messageID")
    ctx.evidence(f"live/discovery-migration-{label}.txt",
                 f"migration serve {base}: sessionID_pattern={ctx.sid_pattern} "
                 f"messageID_pattern={ctx.mid_pattern}\n")


class Ctx:
    """live 层共享上下文：结果计数 + serve 生命周期 + wire 发现态。"""

    def __init__(self, run_dir: Path, locked: dict):
        self.run_dir = run_dir
        self.locked = locked
        self.any_fail = False
        self.any_inconclusive = False
        self.any_harness = False
        self.evidence_broken: set[str] = set()  # F3：证据写入失败的 rel 集合
        self.password = f"spike-{__import__('secrets').token_hex(16)}"
        # F2：运行期生成的凭据显式登记进脱敏集合
        S.register_secret(self.password)
        S.register_secret(f"opencode:{self.password}")
        import base64 as _b64
        S.register_secret(_b64.b64encode(f"opencode:{self.password}".encode()).decode())
        self.sbroot = S.sandbox_root()
        self.work = self.sbroot / "work"
        S.sandbox_mkdir(self.work)
        self.base: Optional[str] = None         # 当前共享 serve 基址
        self.api_prefix = ""                    # v2 API 前缀（/api）；v1 为空——由发现层填充
        self.sid: Optional[str] = None          # 共享 session fixture
        self.sid_prefix = "ses"
        self.prompt_path: Optional[str] = None
        self.sid_pattern: Optional[str] = None
        self.mid_pattern: Optional[str] = None
        self.registry = S.ProcessRegistry(run_dir)
        self.serve_procs: dict[str, subprocess.Popen] = {}
        self.completed: set[str] = set()   # F3：已落盘结果 ID，供组异常后补齐缺失项

    # ---- 结果封装（每 probe 恰好一条） ----
    def emit(self, probe_id: str, exec_status: str, diff_class: str,
             verdict: str, reason: str, evidence_path: str,
             equivalent: str = "", failure_classes: Optional[list[str]] = None) -> None:
        # F32：每 probe 恰好一条的完整性保护——重复 emit 即 harness 故障
        if probe_id in self.completed:
            S.harness_fail(f"duplicate emit for probe_id: {probe_id}")
        if exec_status == "FAIL":
            self.any_fail = True
        elif exec_status == "INCONCLUSIVE":
            self.any_inconclusive = True
        # F3：结果引用的证据文件必须真实存在——缺失时生成真实的原因说明文件，
        # 绝不让结果引用不存在的路径；但补说明不等于证据有效——
        # 意外缺失的实验证据使本记录效力 harness_invalid（不得作为 valid 消费）
        stub_generated = False
        if evidence_path and not (self.run_dir / evidence_path).exists():
            stub_generated = True
            stub_rel = evidence_path
            try:
                S.evidence_write(self.run_dir, stub_rel,
                                 f"证据文件由 emit 补齐：{probe_id} 引用的 {evidence_path} "
                                 f"未在执行路径中生成；本记录效力已降级 harness_invalid。\n")
            except S.SpikeExit as e:
                if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                    raise
                self.any_harness = True
        try:
            # F3：本记录引用的证据写入曾失败或证据意外缺失 → 记录效力必须为
            # harness_invalid，不能作为 valid 事实进入 D8 消费
            validity = ("harness_invalid"
                        if stub_generated or evidence_path in self.evidence_broken
                        else "valid")
            if validity == "harness_invalid":
                self.any_harness = True
            S.emit_result(self.run_dir, self.locked, probe_id, exec_status,
                          diff_class, verdict, validity, reason, evidence_path,
                          equivalent, failure_classes)
            self.completed.add(probe_id)
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise   # F19：前置拒绝/安全越界必须传播，不得吞成 harness 继续
            # 单条结果写入失败不中断其余独立项；阶段以 4 收尾
            self.any_harness = True

    def emit_harness(self, probe_id: str, reason: str) -> None:
        self.any_harness = True
        note_rel = f"live/harness-{probe_id.replace('.', '_')}.txt"
        try:
            S.evidence_write(self.run_dir, note_rel, f"harness failure: {reason}\n")
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise
            # F3/F38：故障说明写失败不得引用不存在路径——不可恢复
            S.harness_fail(f"harness note write failed for {probe_id}: {e}")
        if not (self.run_dir / note_rel).is_file():
            S.harness_fail(f"harness note missing after write: {note_rel}")
        # F23：豁免项保持契约固定字段（PASS/不适用/不适用），仅 evidence_validity 标 harness_invalid
        if probe_id in S.EXEMPT_PROBE_IDS:
            es, dc, cv = "PASS", "不适用", "不适用"
        else:
            es, dc, cv = "INCONCLUSIVE", "待验证", "待验证"
        # F25：migration 记录必须带 failure_classes（schema 强制）
        fc = ["HARNESS_FAILURE"] if probe_id.startswith("migration.") else None
        try:
            S.emit_result(self.run_dir, self.locked, probe_id, es, dc, cv,
                          "harness_invalid", reason, note_rel, "", fc)
            self.completed.add(probe_id)   # 仅成功落盘才登记（F23）
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise   # F19
            S.harness_fail(f"harness result write failed for {probe_id}: {e}")

    def evidence(self, rel: str, content: str | bytes) -> str:
        """证据写入（脱敏+受保护）。失败（F3）：写真实失败原因文件并登记
        self.evidence_broken，调用方据此使受影响记录 invalid；返回占位路径永不落盘引用。
        2/5 传播（F19）。"""
        try:
            S.evidence_write(self.run_dir, rel, content)
            return rel
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise
            self.any_harness = True
            self.evidence_broken.add(rel)
            note_rel = f"live/evidence-write-failed-{rel.replace('/', '_').replace('.', '_')}.txt"
            self.evidence_broken.add(note_rel)  # F3：返回的说明路径同样标记，
            # emit/mig_emit 按返回路径检查时能命中 → 记录效力 harness_invalid
            try:
                S.evidence_write(self.run_dir, note_rel,
                                 f"evidence write failed: {rel}: {type(e).__name__}: {e}\n")
            except S.SpikeExit as e2:
                if e2.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                    raise
                # F3/F38：fallback 说明也写失败 → 不可恢复，不得引用不存在路径
                S.harness_fail(f"evidence fallback write failed: {rel}: {e2}")
            if not (self.run_dir / note_rel).is_file():
                S.harness_fail(f"evidence fallback missing after write: {note_rel}")
            return note_rel

    # ---- HTTP ----
    def api(self, path: str) -> str:
        """API 端点完整 URL：base + api_prefix（v2=/api，v1=空） + path。"""
        return f"{self.base}{self.api_prefix}{path}"

    def http(self, url: str, method: str = "GET", json_body: Any = None,
             timeout: float = 10.0, auth: bool = True) -> S.HttpResult:
        return S.http_request(url, method=method,
                              password=self.password if auth else None,
                              json_body=json_body, timeout=timeout)

    def dir_param(self, d: Path) -> str:
        return urllib.parse.quote(str(d), safe="")

    # ---- serve 生命周期 ----
    def serve_start(self, name: str, binary: str, port: int, envroot: Path,
                    cwd: Path, extra: Optional[list[str]] = None,
                    deadline_s: float = 30.0) -> int:
        """0=就绪；1=进程死亡；2=超时未就绪（deadline 截止）。"""
        log = self.run_dir / "live" / f"serve-{name}.log"
        # 实机修正：v1/v2 CLI 均以 serve 子命令启动服务（裸 --port/--hostname 被拒）
        argv = [binary, "serve", "--port", str(port), "--hostname", "127.0.0.1"] + (extra or [])
        proc = S.spawn_upstream(envroot, cwd, argv, log_file=log, password=self.password)
        try:
            self.registry.add("shared", proc.pid)
        except Exception:
            S.kill_tree(proc.pid)
            raise
        self.serve_procs[name] = proc
        deadline = time.time() + deadline_s
        while proc.poll() is None:
            if time.time() >= deadline:
                return 2
            remain = deadline - time.time()
            # 实机修正：v2 的 /global/health 返回 SPA HTML（非 JSON）；用真实 API 端点
            # /api/session（无凭证 401 / 带凭证 200）作为就绪信号，避免 SPA 兜底误判
            r = self.http(f"http://127.0.0.1:{port}/api/session",
                          timeout=max(1.0, min(2.0, remain)))
            if r.status in (200, 401):
                return 0
            time.sleep(0.2)
        return 1

    def serve_stop(self, name: str) -> bool:
        """整树停止并确认退出（冻结前置）。返回是否已确认退出。"""
        proc = self.serve_procs.pop(name, None)
        if proc is None:
            return True
        S.kill_tree(proc.pid)
        deadline = time.time() + 5
        while time.time() < deadline:
            if proc.poll() is not None:
                return True
            time.sleep(0.2)
        return False

    # ---- wire 形态发现（D4/F22/F5：三级发现，每级落证据；serve 就绪后先于 session 操作） ----
    def discover_wire_forms(self) -> bool:
        """三级发现：①文档端点（/doc 或 /openapi.json）→ ②路由清单（/app 等）→ ③源码锚点检索。
        填充 prompt_path/sid_pattern/mid_pattern/api_prefix；返回是否有可用提交端点事实。
        实机修正：v2 的 /doc 返回 HTML（SPA），真实 OpenAPI 在 /openapi.json；
        v2 全部 API 路径带 /api 前缀，发现后据此设置 api_prefix。"""
        if self.base is None:
            return False
        # 第一级：依次尝试 /doc（v1）与 /openapi.json（v2）
        doc = None
        for ep in ("/doc", "/openapi.json"):
            r = self.http(f"{self.base}{ep}", timeout=30.0)
            if r.status is None:
                self.evidence("live/discovery.txt",
                              f"level1 {ep} transport error: {r.error}\n")
                continue
            if r.status != 200 or not r.body:
                self.evidence("live/discovery.txt", f"level1 {ep} status={r.status}\n")
                continue
            self.evidence(f"live/discovery-doc{ep.replace('/', '_')}.json", r.body)
            try:
                doc = json.loads(r.body)
            except json.JSONDecodeError:
                # HTML（SPA 页面）不是 OpenAPI——继续尝试下一端点
                self.evidence("live/discovery.txt",
                              f"level1 {ep} returned non-JSON (likely SPA HTML)\n")
                continue
            if isinstance(doc, dict) and isinstance(doc.get("paths"), dict):
                break   # 拿到真实 OpenAPI
            doc = None
        if doc is None:
            self._discover_fallback()
            return False
        paths = doc.get("paths") or {}
        # v2 wire 修正：按关键 API 路径（session/event/permission/prompt）判定 /api 前缀——
        # 个别非 API 路径（如 /auth/connect/{code}）不影响判定
        api_paths = [k for k in paths if any(
            s in k for s in ("/session", "/event", "/permission", "/prompt", "/message"))]
        if api_paths and all(k.startswith("/api/") for k in api_paths):
            self.api_prefix = "/api"
        # F5：优先含 sessionID 模板 + prompt 语义的路径
        prompt_paths = [k for k in paths if "prompt" in k]
        preferred = [k for k in prompt_paths
                     if "{sessionID}" in k or "prompt_async" in k]
        self.prompt_path = (preferred or prompt_paths or [None])[0]

        self.sid_pattern = _find_pattern(doc, doc, "sessionID")
        self.mid_pattern = _find_pattern(doc, doc, "messageID")
        prev = ""
        dtxt = self.run_dir / "live" / "discovery.txt"
        if dtxt.is_file():
            prev = dtxt.read_text(encoding="utf-8")
        self.evidence("live/discovery.txt",
                      prev + f"api_prefix={self.api_prefix}\n"
                      f"prompt_path={self.prompt_path}\n"
                      f"sessionID_pattern={self.sid_pattern}\n"
                      f"messageID_pattern={self.mid_pattern}\n")
        return self.prompt_path is not None

    def _discover_fallback(self) -> None:
        """第二级（路由清单探测）+ 第三级（源码锚点检索，绑定锁定 commit）。"""
        assert self.base is not None
        r_app = self.http(f"{self.base}/app", timeout=10.0)
        self.evidence("live/discovery-l2.txt",
                      f"level2 /app status={r_app.status} err={r_app.error}\n")
        v2ref = self.locked["v2"]["commit_sha"]
        r_tree = S.http_request(
            f"https://api.github.com/repos/anomalyco/opencode/git/trees/{v2ref}?recursive=1",
            timeout=30.0)
        prompt_paths: list[str] = []
        if r_tree.status == 200:
            try:
                tree = json.loads(r_tree.body)
                prompt_paths = [e.get("path", "") for e in tree.get("tree", [])
                                if "prompt" in e.get("path", "")]
            except json.JSONDecodeError:
                pass
        self.evidence("live/discovery-l3.txt",
                      f"level3 source tree at {v2ref}: prompt-paths={prompt_paths[:20]}\n")
        # F5：三级发现必须实际产出 wire 契约事实——对候选源文件抓原文并提取路由注册行
        # （Hono/Express 风格 .post("/session/:sessionID/prompt_async" 等）；失败保持待验证
        extracted: list[str] = []
        for sp in prompt_paths[:5]:
            if not sp.endswith((".ts", ".js")):
                continue
            r_src = S.http_request(
                f"https://raw.githubusercontent.com/anomalyco/opencode/{v2ref}/{sp}",
                timeout=30.0)
            if r_src.status != 200 or not r_src.body:
                continue
            try:
                src = r_src.body.decode("utf-8", "replace")
            except Exception:
                continue
            for m in re.finditer(
                    r'''\.(?:post|get|put|delete)\(\s*["'`]([^"'`]*(?:prompt|message)[^"'`]*)["'`]''',
                    src, re.IGNORECASE):
                extracted.append(f"{sp}: {m.group(1)}")
            # messageID/sessionID 校验 pattern（zod/正则字面量）
            for m in re.finditer(
                    r'''(?:messageID|sessionID)[^\n]{0,80}?(?:regex|pattern)\((/[^/]+/|`[^`]+`|"[^"]+")''',
                    src):
                extracted.append(f"{sp} pattern: {m.group(1)}")
        if extracted:
            self.evidence("live/discovery-l3-extract.txt",
                          "level3 extracted wire facts:\n" + "\n".join(extracted[:50]) + "\n")
        else:
            self.evidence("live/discovery-l3-extract.txt",
                          "level3 extraction yielded no wire facts; discovery 待验证\n")

    def make_message_id(self, tag: str) -> Optional[str]:
        """按发现的 messageID pattern 构造合法 ID（F22/F5：构造结果必须满足完整 pattern；
        无已发现 pattern 或无法满足时返回 None——缺发现事实保持待验证，不猜默认值，
        调用方必须 INCONCLUSIVE 而非继续提交伪合法 ID）。"""
        if not self.mid_pattern:
            return None   # F5：无发现事实不猜 ^msg
        suffix = f"{tag}_{__import__('secrets').token_hex(8)}"
        pat = self.mid_pattern
        m = re.match(r"^\^([a-z][a-z0-9]*)", pat)
        if m is None:
            return None   # pattern 形态超出支持（非简单前缀锚定），不猜测构造
        prefix = m.group(1)
        candidate = f"{prefix}_{suffix}"
        try:
            if not re.match(pat, candidate):
                return None
        except re.error:
            return None
        return candidate


# ================================================================== probes 模式

def probe_supervisor(ctx: Ctx) -> None:
    ev = "live/supervisor"
    # supervisor.01：最小参数 + 显式参数 + 非法参数拒绝（子断言齐全才 PASS）
    min_ready = False
    # 实机修正：v2 裸命令（无子命令）进入 TUI 而非服务；最小服务启动=serve 子命令
    p_min = S.spawn_upstream(ctx.sbroot, ctx.work,
                             [ctx.locked["v2"]["exec_path"], "serve"],
                             log_file=ctx.run_dir / f"{ev}-minimal.log",
                             password=ctx.password)
    try:
        ctx.registry.add("item", p_min.pid)
    except Exception:
        S.kill_tree(p_min.pid)
        raise
    deadline = time.time() + 15
    while p_min.poll() is None and time.time() < deadline:
        # 实机修正：v2 /global/health 是 SPA HTML；用真实 API 端点判就绪
        # （带凭证 200 / 无凭证 401 均证明服务已监听）
        r = ctx.http("http://127.0.0.1:4096/api/session", timeout=2.0)
        if r.status in (200, 401):
            min_ready = True
            break
        time.sleep(0.2)
    S.kill_tree(p_min.pid)
    # 非法参数拒绝（带截止时间；F9：登记 registry 纳入三层清理）
    # 实机修正：挂到 serve 子命令下，验证的是端口参数校验而非 CLI usage 拒绝
    inv = S.run_upstream(ctx.sbroot, ctx.work,
                         [ctx.locked["v2"]["exec_path"], "serve", "--port",
                          "notaport", "--hostname", "127.0.0.1"],
                         timeout=15.0, password=ctx.password,
                         registry=ctx.registry)
    ctx.evidence(f"{ev}/invalid-args.txt", inv.stdout or "")
    invalid_ok = inv.returncode != 0
    # 显式参数启动（共享 serve）
    rc = ctx.serve_start("main", ctx.locked["v2"]["exec_path"], 29711, ctx.sbroot, ctx.work)
    if rc == 0:
        ctx.base = "http://127.0.0.1:29711"
        # wire 形态发现必须先于认证探针——api_prefix 由发现层填充（v2=/api）
        ctx.discover_wire_forms()
        if ctx.sid_pattern:
            m = re.match(r"^\^([a-z][a-z0-9]*)", ctx.sid_pattern)
            if m:
                ctx.sid_prefix = m.group(1)
    if rc == 0 and min_ready and invalid_ok:
        ctx.emit("supervisor.01", "PASS", "待验证", "可满足",
                 "三次启动断言齐全：最小参数（默认端口就绪）+ 显式 --port/--hostname + "
                 f"非法参数拒绝（exit={inv.returncode}）", f"{ev}/invalid-args.txt")
    elif rc != 0:
        ctx.emit("supervisor.01", "FAIL", "待验证", "不可满足",
                 f"显式参数启动失败（rc={rc}；1=进程死亡 2=超时）", "live/serve-main.log")
    else:
        ctx.emit("supervisor.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"显式启动成功但子断言未齐全：最小参数就绪={min_ready} "
                 f"非法参数拒绝={invalid_ok}", f"{ev}-minimal.log")
    if rc != 0:
        ctx.base = None
        ctx.emit("supervisor.02", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：serve 未能启动", "live/serve-main.log")
        ctx.emit("supervisor.03", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：serve 未能启动", "live/serve-main.log")
        return
    # supervisor.02：认证——无凭证 401 / 带凭证 200
    # 实机修正：v2 的 /global/health 返回 SPA HTML；认证语义用真实 API 端点 /api/session
    anon = ctx.http(f"{ctx.base}{ctx.api_prefix}/session", auth=False, timeout=5.0)
    auth = ctx.http(f"{ctx.base}{ctx.api_prefix}/session", timeout=5.0)
    ep = ctx.evidence(f"{ev}/auth.txt", f"anon={anon.status} auth={auth.status}\n")
    if anon.status is None or auth.status is None:
        ctx.emit("supervisor.02", "INCONCLUSIVE", "待验证", "待验证",
                 f"认证观测传输失败（anon={anon.error} auth={auth.error}）", ep)
    elif anon.status == 401 and auth.status == 200:
        ctx.emit("supervisor.02", "PASS", "兼容", "可满足",
                 "无凭证 401 / 带凭证 200（与 v1 认证语义一致）", ep)
    elif auth.status == 200:
        ctx.emit("supervisor.02", "FAIL", "不兼容", "不可满足",
                 f"带凭证 200 但无凭证返回 {anon.status}（非 401，无等价证据）", ep)
    else:
        ctx.emit("supervisor.02", "FAIL", "待验证", "不可满足",
                 f"带凭证请求非 200（{auth.status}）：认证不可用", ep)
    # supervisor.03：健康就绪（serve_start 返回 0 即期限内识别就绪；
    # v2 /global/health 返回 SPA HTML——就绪证据用真实 API 端点 /api/session）
    r = ctx.http(f"{ctx.base}{ctx.api_prefix}/session", timeout=5.0)
    ep = ctx.evidence(f"{ev}/health-body.json", r.body)
    health_ok = r.status == 200
    if health_ok:
        try:
            json.loads(r.body)
        except json.JSONDecodeError:
            health_ok = False
    if health_ok:
        ctx.emit("supervisor.03", "PASS", "待验证", "可满足",
                 "期限内识别就绪且就绪后 200；响应体可解析，形状记录于证据", ep)
    else:
        ctx.emit("supervisor.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"就绪后健康响应不可用（status={r.status} err={r.error}）", ep)
    # wire 形态发现已在 supervisor.01 就绪后立即执行（上方），此处不再重复


def _extract_session_id(doc_path: Path) -> tuple[Optional[str], str]:
    """从创建响应提取 session 标识（wire 形态发现先行；规则记录进 reason）。
    v2 响应包 {data: {...}} envelope——先解包再找 id。"""
    try:
        doc = json.loads(doc_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None, "unparseable"
    # v2 envelope：data 键包裹真实载荷
    if isinstance(doc, dict) and isinstance(doc.get("data"), dict):
        doc = doc["data"]

    def find(x: Any, path: str) -> Optional[tuple[str, str]]:
        if isinstance(x, dict):
            for k, v in x.items():
                p = f"{path}.{k}" if path else k
                if isinstance(v, str) and v and k.lower() in ("id", "sessionid", "session_id"):
                    return v, p
                r = find(v, p)
                if r:
                    return r
        return None

    r = find(doc, "")
    if not r:
        return None, "no-id-field"
    return r[0], f"jsonpath={r[1]}"


def _create_session(ctx: Ctx, base: str, cwd: Path, title: str,
                    evidence_rel: str,
                    extra_body: Optional[dict[str, Any]] = None
                    ) -> tuple[Optional[str], str, Optional[int]]:
    """POST 创建 session；返回 (sid 或 None, 提取规则, HTTP 状态)。
    F6：传输失败 status=None，调用方必须区分传输错误与协议失败。
    v2 wire：API 在 api_prefix（/api）前缀下；v2 无 directory query 参数——
    directory 定位走 body location:{directory}（OpenAPI：POST /api/session
    parameters=[]，Session.Create.location=Location.PublicRef）。
    location 仅在探测独立目录分页时传入（session.05）。"""
    body: dict[str, Any] = {"title": title}
    if extra_body:
        body.update(extra_body)
    r = ctx.http(f"{base}{ctx.api_prefix}/session?directory={ctx.dir_param(cwd)}",
                 method="POST", json_body=body, timeout=30.0)
    ep = ctx.evidence(evidence_rel, r.body or (r.error or ""))
    if r.status not in (200, 201):
        return None, f"http={r.status} err={r.error}", r.status
    sid, rule = _extract_session_id(ctx.run_dir / ep)
    return sid, rule, r.status


def _perm_equiv(entry: Any, sample: Any) -> bool:
    """F10/F31：权限条目逻辑等价——样本的每个字段值在 entry 中存在且相等
    （子集语义：entry 可有额外字段；非 dict 用值相等）。
    F31：空 dict 样本不得匹配任何条目（键不存在≠值为 None，空样本无信息量）。"""
    if isinstance(sample, dict):
        if not sample or not isinstance(entry, dict):
            return False
        # 字段不存在与值为 None 必须区分：sample 的键必须在 entry 中实际存在
        return all(k in entry and entry[k] == v for k, v in sample.items())
    if sample is None:
        return False
    return entry == sample


def _strict_id(value: Any) -> Optional[str]:
    """F12：可用 ID 必须是非空字符串；数值/容器/空串一律不可比较。"""
    if isinstance(value, str) and value.strip():
        return value
    return None


def _entry_id(obj: Any) -> Optional[str]:
    """F12：从 dict 或嵌套 info 提取严格 ID；info 非 dict 不得异常。"""
    if not isinstance(obj, dict):
        return None
    sid = _strict_id(obj.get("id")) or _strict_id(obj.get("permissionID"))
    if sid:
        return sid
    info = obj.get("info")
    if isinstance(info, dict):
        return _strict_id(info.get("id"))
    return None


def _history_comparable(items: Any) -> bool:
    """F12：历史必须是 dict 数组，且每条都有非空字符串 ID。"""
    return isinstance(items, list) and all(_entry_id(m) is not None for m in items)


def _processing_success(ev_obj: dict, fsid: str, cmid: str) -> bool:
    """F11/F37：有效后续处理必须是 agent/assistant 侧产出，不能是用户消息入史。
    精确匹配 type；多层 session 归属；无角色时只接受合法 StatusDetail
    或带明确完成标记的 part 产出。"""
    etype = ev_obj.get("type")
    if etype not in ("message.updated", "message.part.updated",
                     "part.updated", "part.delta",
                     "session.status", "session.status_changed", "status.changed"):
        return False
    if S.event_session_id(ev_obj) != fsid:
        return False
    if not _event_mentions_mid(ev_obj, cmid):
        return False
    role = None
    for d in S._payload_dicts(ev_obj):
        r = d.get("role") or d.get("agent")
        if isinstance(r, str) and r.strip():
            role = r.strip().lower()
            break
    if role in ("user", "human"):
        return False
    if role in ("assistant", "agent", "system"):
        return True
    if etype in ("session.status", "session.status_changed", "status.changed"):
        return S.status_detail_ok(ev_obj)
    # 无角色：仅带明确完成标记的 part 产出可计为处理成功；普通文本回显不行
    for d in S._payload_dicts(ev_obj):
        part = d.get("part") if isinstance(d.get("part"), dict) else d
        if not isinstance(part, dict):
            continue
        t = part.get("text") or part.get("output")
        done = bool(part.get("completed") or d.get("completed")
                    or part.get("time") or d.get("time"))
        if isinstance(t, str) and t.strip() and done and part.get("type") != "text":
            return True
    return False


def _event_mentions_mid(ev_obj: dict, mid: str) -> bool:
    """F11：事件结构化字段精确关联 messageID（递归查 messageID/message.id 字段，
    不用全文本子串——插入其他内容或错误事件不得误命中）。"""
    def find(x: Any) -> bool:
        if isinstance(x, dict):
            for k, v in x.items():
                if k == "messageID" and v == mid:
                    return True
                if k in ("message", "info") and isinstance(v, dict) and v.get("id") == mid:
                    return True
                if find(v):
                    return True
        elif isinstance(x, list):
            return any(find(i) for i in x)
        return False

    return find(ev_obj)


def _mid_echoed(body: bytes, mid: str) -> bool:
    """F6：结构化回显核对——解析响应 JSON 递归找 messageID 字段精确匹配，不用字节子串。
    v2 wire：prompt 响应 data.id 即 messageID（Session.Inbox.User 无 messageID 字段名）。"""
    try:
        doc = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return False

    def find(x: Any) -> bool:
        if isinstance(x, dict):
            for k, v in x.items():
                if k in ("messageID", "id") and v == mid:
                    return True
                if find(v):
                    return True
        elif isinstance(x, list):
            return any(find(i) for i in x)
        return False

    return find(doc)


def _parse_id_list(r: S.HttpResult) -> Optional[list[str]]:
    """解析 session 列表响应为 ID 集合（F6：避免字节子串误命中）。
    F6 严格化：整表逐项校验——任一项畸形（非 dict / 无字符串 ID）即返回 None
    （不可比较），调用方必须 INCONCLUSIVE，不得对残缺集合做子集判定。
    v2 envelope：列表包在 {"data": [...]} 里。"""
    if r.status != 200 or not r.body:
        return None
    try:
        v = json.loads(r.body)
    except json.JSONDecodeError:
        return None
    if isinstance(v, dict) and isinstance(v.get("data"), list):
        v = v["data"]   # v2 envelope 解包
    if not isinstance(v, list):
        return None
    out = []
    for item in v:
        if not isinstance(item, dict):
            return None   # 坏项 → 整表不可比较
        sid = item.get("id") or item.get("sessionID")
        if not isinstance(sid, str) or not sid:
            return None   # 坏项 → 整表不可比较
        out.append(sid)
    return out


def probe_sessions(ctx: Ctx) -> None:
    ev = "live/session"
    if ctx.base is None:
        for p in ("01", "02", "03", "05", "06", "07"):
            ctx.emit(f"session.{p}", "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：serve 未运行", f"{ev}/no-serve.txt")
        return
    # session.01：创建 + v2 ID 形态发现
    sid, rule, sid_status = _create_session(ctx, ctx.base, ctx.work, "spike-probe",
                                f"{ev}/create.json")
    if sid is None and sid_status is None:
        ctx.emit("session.01", "INCONCLUSIVE", "待验证", "待验证",
                 "创建请求传输失败，未获得协议事实", f"{ev}/create.json")
        for p in ("02", "03", "05", "06", "07"):
            ctx.emit(f"session.{p}", "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：session 创建契约未获得（传输失败）", f"{ev}/create.json")
        return
    if sid:
        ctx.sid = sid
        if not ctx.sid_pattern:
            # 发现未获得 ID pattern（F21：缺发现事实 → INCONCLUSIVE 而不是带疑 PASS）
            ctx.emit("session.01", "INCONCLUSIVE", "待验证", "待验证",
                     f"创建返回非空标识（规则 {rule}）但 /doc 未发现 sessionID pattern，"
                     "ID 格式符合性未验证", f"{ev}/create.json")
        else:
            pat_ok = bool(re.match(ctx.sid_pattern, sid))
            ctx.emit("session.01", "PASS" if pat_ok else "FAIL",
                     "待验证", "可满足" if pat_ok else "不可满足",
                     f"创建返回非空 session 标识；提取规则 {rule}；"
                     f"符合已发现 pattern({ctx.sid_pattern})={pat_ok}", f"{ev}/create.json")
    else:
        ctx.emit("session.01", "FAIL", "待验证", "不可满足",
                 f"创建响应无法提取非空 session 标识（{rule}）", f"{ev}/create.json")
        for p in ("02", "03", "05", "06", "07"):   # F32：session.04 归 probe_destructive
            ctx.emit(f"session.{p}", "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：session 创建契约未获得", f"{ev}/create.json")
        return
    bad_id = f"{ctx.sid_prefix}_doesnotexist00000000000000"
    # session.02：GET 存在（200 且响应体 ID 一致）/ 不存在（明确信号）
    r_ok = ctx.http(ctx.api(f"/session/{sid}?directory={ctx.dir_param(ctx.work)}"))
    ep_ok = ctx.evidence(f"{ev}/get.json", r_ok.body)
    body_id = None
    if r_ok.status == 200 and r_ok.body:
        try:
            parsed = json.loads(r_ok.body)
            if isinstance(parsed, dict):
                if isinstance(parsed.get("data"), dict):
                    parsed = parsed["data"]   # v2 envelope 解包
                # F5：按发现的字段形态取值（id/sessionID），不固定 v1 字段名
                body_id = parsed.get("id") or parsed.get("sessionID")
        except json.JSONDecodeError:
            body_id = None
    r_ms = ctx.http(ctx.api(f"/session/{bad_id}?directory={ctx.dir_param(ctx.work)}"))
    ctx.evidence(f"{ev}/missing.json", r_ms.body)
    missing_explicit = (r_ms.status == 404) or (
        r_ms.status is not None and r_ms.status < 500
        and bool(re.search(rb"(?i)not.?found|no.?such|missing", r_ms.body)))
    ep2 = ctx.evidence(f"{ev}/get-codes.txt",
                       f"exists={r_ok.status} body_id={body_id} "
                       f"missing={r_ms.status} explicit={missing_explicit}\n")
    if r_ok.status is None or r_ms.status is None:
        ctx.emit("session.02", "INCONCLUSIVE", "待验证", "待验证",
                 f"传输错误（exists={r_ok.error} missing={r_ms.error}）", ep2)
    elif r_ok.status == 200 and body_id == sid and missing_explicit:
        ctx.emit("session.02", "PASS", "待验证", "可满足",
                 f"GET 存在=200 且 ID 一致；不存在的明确信号={r_ms.status}", ep2)
    else:
        ctx.emit("session.02", "FAIL", "待验证", "不可满足",
                 f"GET 语义不满足：exists={r_ok.status} body_id={body_id} "
                 f"missing={r_ms.status} explicit={missing_explicit}", ep2)
    # session.03：列表（解析 JSON 数组核成员，不用字节子串——F6）
    r_ls = ctx.http(ctx.api(f"/session?directory={ctx.dir_param(ctx.work)}"))
    ep3 = ctx.evidence(f"{ev}/list.json", r_ls.body)
    listed_ids = _parse_id_list(r_ls)
    if r_ls.status is None:
        ctx.emit("session.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"列表请求传输失败（{r_ls.error}）", ep3)
    elif listed_ids is None:
        ctx.emit("session.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"列表响应非可解析数组（status={r_ls.status}）", ep3)
    elif ctx.sid in listed_ids:
        ctx.emit("session.03", "PASS", "待验证", "可满足",
                 "列表含已建 session（解析后 ID 集合核对）；directory 过滤语义记录于证据", ep3)
    else:
        ctx.emit("session.03", "FAIL", "待验证", "不可满足",
                 f"列表不含已建 session（status={r_ls.status}）", ep3)
    # session.07：三分流（Unknown 样本必须与 Missing 可区分）
    r_bad = ctx.http(ctx.api(f"/session/notavalidid?directory={ctx.dir_param(ctx.work)}"))
    ep7 = ctx.evidence(f"{ev}/tri.txt",
                       f"found={r_ok.status} missing={r_ms.status} unknown={r_bad.status}\n")
    if r_ok.status is None or r_ms.status is None or r_bad.status is None:
        ctx.emit("session.07", "INCONCLUSIVE", "待验证", "待验证", "三分流观测传输失败", ep7)
    elif (r_ok.status == 200 and missing_explicit
          and r_bad.status != 200 and r_bad.status != r_ms.status):
        ctx.emit("session.07", "PASS", "待验证", "可满足",
                 f"Found=200 / Missing={r_ms.status} / Unknown 样本={r_bad.status} 三者可区分", ep7)
    elif r_ok.status == 200 and missing_explicit:
        ctx.emit("session.07", "INCONCLUSIVE", "待验证", "待验证",
                 f"Unknown 样本({r_bad.status})与 Missing({r_ms.status}) 不可区分——"
                 "三分流映射需适配层归一，实证记录于证据", ep7)
    else:
        ctx.emit("session.07", "FAIL", "待验证", "不可满足",
                 f"三分流不满足：found={r_ok.status} missing={r_ms.status} "
                 f"unknown={r_bad.status}", ep7)
    # session.04（删除）为破坏性操作——已移至 probe_destructive，
    # 排在共享 fixture 全部消费者之后（D3 fixture DAG，F7）
    # session.05：分页（独立 directory，固定集合 E=5）
    dir5 = ctx.work / "session05"
    S.sandbox_mkdir(dir5)
    E: list[str] = []
    for i in range(1, 6):
        # v2：POST /api/session 无 directory query 参数，独立目录分页必须在 body
        # location 指定 directory（OpenAPI Session.Create.location）
        s, _, _ = _create_session(ctx, ctx.base, dir5, f"spike-page-{i}",
                               f"{ev}/page-create-{i}.json",
                               extra_body={"location": {"directory": str(dir5)}})
        if s:
            E.append(s)
    if len(E) < 5:
        ctx.emit("session.05", "INCONCLUSIVE", "待验证", "待验证",
                 f"fixture 不完整：仅创建 {len(E)}/5 个 session", f"{ev}/page-create-1.json")
    else:
        d5 = ctx.dir_param(dir5)
        r1 = ctx.http(ctx.api(f"/session?directory={d5}&limit=1"))
        r3 = ctx.http(ctx.api(f"/session?directory={d5}&limit=3"))
        ra = ctx.http(ctx.api(f"/session?directory={d5}"))
        ctx.evidence(f"{ev}/page-limit1.json", r1.body)
        ctx.evidence(f"{ev}/page-limit3.json", r3.body)
        ctx.evidence(f"{ev}/page-all.json", ra.body)
        def _len(r: S.HttpResult) -> int:
            try:
                v = json.loads(r.body)
                if isinstance(v, dict) and isinstance(v.get("data"), list):
                    v = v["data"]   # v2 envelope 解包（与 _parse_id_list 同口径）
                return len(v) if isinstance(v, list) else -1
            except json.JSONDecodeError:
                return -1
        c1, c3 = _len(r1), _len(r3)
        all_ids = _parse_id_list(ra)
        missing = (sum(1 for s in E if s not in all_ids)
                   if all_ids is not None else -1)
        # F6：limit 响应必须 HTTP 200 且成员属于预期集合 E（不接受任意计数）；
        # 解析失败 None 不得经 or [] 变空集合——显式要求非 None 才可比较
        ids1 = _parse_id_list(r1)
        ids3 = _parse_id_list(r3)
        limit1_ok = (r1.status == 200 and c1 >= 0
                     and ids1 is not None and set(ids1) <= set(E))
        limit3_ok = (r3.status == 200 and c3 >= 0
                     and ids3 is not None and set(ids3) <= set(E))
        ep5 = ctx.evidence(f"{ev}/page-counts.txt",
                           f"limit1={c1} limit3={c3} missing_in_all={missing} "
                           f"limit1_ok={limit1_ok} limit3_ok={limit3_ok}\n")
        # F6：不可比较的分页载荷只能 INCONCLUSIVE，不得落入 FAIL/PASS 分支
        if ids1 is None or ids3 is None:
            ctx.emit("session.05", "INCONCLUSIVE", "待验证", "待验证",
                     f"limit 分页响应不可比较（含坏项/解析失败）："
                     f"limit1_parse={'ok' if ids1 is not None else 'bad'} "
                     f"limit3_parse={'ok' if ids3 is not None else 'bad'}", ep5)
        elif ra.status is None or c1 == -1 or all_ids is None:
            ctx.emit("session.05", "INCONCLUSIVE", "待验证", "待验证",
                     f"分页响应不可解析或传输失败（all={ra.status} limit1={r1.status}）", ep5)
        elif missing > 0:
            ctx.emit("session.05", "FAIL", "待验证", "不可满足",
                     f"无 limit 查询缺 {missing} 个预期 session（缺一即 FAIL）", ep5)
        elif c1 == 1 and c3 == 3 and limit1_ok and limit3_ok:
            ctx.emit("session.05", "PASS", "待验证", "可满足",
                     "limit 精确生效（count==limit 且成员属于预期集合）且无限查询完整（F⊆V）；"
                     "适配层可按保守规则判 Overflow", ep5)
        else:
            ctx.emit("session.05", "INCONCLUSIVE", "待验证", "待验证",
                     f"无限查询完整但 limit 计数为 {c1}/{c3}（有效={limit1_ok}/{limit3_ok}）"
                     "——无上游承诺填满 limit 的证据，不得仅凭 count<limit 推完整性；"
                     "需额外分页/对账规则实证", ep5)
    # session.06：per-session environment 三级发现
    env_found = ""
    doc_path = ctx.run_dir / "live" / "discovery-doc.json"
    if doc_path.is_file():
        try:
            doc = json.loads(doc_path.read_text(encoding="utf-8"))
            hits = [k for k in (doc.get("paths") or {}) if re.search(r"(?i)env", k)]
            ctx.evidence(f"{ev}/env-l1-doc.txt", "\n".join(hits) + "\n")
            if hits:
                env_found = "doc-route"
        except json.JSONDecodeError:
            pass
    if not env_found:
        r_app = ctx.http(f"{ctx.base}/app", timeout=10.0)
        ctx.evidence(f"{ev}/env-l2-routes.txt",
                     f"level2 routes probe: /app status={r_app.status} err={r_app.error}\n")
        v2ref = ctx.locked["v2"]["commit_sha"]
        r_tree = S.http_request(
            f"https://api.github.com/repos/anomalyco/opencode/git/trees/{v2ref}?recursive=1",
            timeout=30.0)
        src_hits = -1
        if r_tree.status == 200:
            try:
                tree = json.loads(r_tree.body)
                src_hits = sum(1 for e in tree.get("tree", [])
                               if re.search(r"(?i)environment", e.get("path", "")))
            except json.JSONDecodeError:
                pass
        ctx.evidence(f"{ev}/env-l3-source.txt",
                     f"level3 source search at {v2ref}: environment-path hits={src_hits}\n")
        if src_hits > 0:
            env_found = "source-anchor"
    if env_found:
        ctx.emit("session.06", "INCONCLUSIVE", "待验证", "待验证",
                 f"发现疑似 environment 形态（来源级别：{env_found}，各级证据 env-l*.txt）；"
                 "注入对 session 内 shell 的实证需进一步实验", f"{ev}/env-l3-source.txt")
    else:
        ctx.emit("session.06", "INCONCLUSIVE", "待验证", "待验证",
                 "三级发现均无 per-session environment 形态（各级证据 env-l1/l2/l3）；"
                 "替代形态考察：v1 以每任务独立进程天然隔离", f"{ev}/env-l3-source.txt")


def probe_events(ctx: Ctx) -> None:
    ev = "live/event"
    stream_rel = f"{ev}/stream.ndjson"
    if ctx.base is None or ctx.sid is None:
        ctx.evidence(f"{ev}/no-serve.txt", "serve 或共享 session 不可用\n")
        ctx.emit("event.01", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：serve 或共享 session 不可用", f"{ev}/no-serve.txt")
        for e in ("event.02a.session_created", "event.02b.session_updated",
                  "event.02d.status_changed",
                  "event.02e.attention_raised", "event.02f.attention_resolved",
                  "event.02g.session_error"):
            ctx.emit(e, "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：事件流未建立", f"{ev}/no-serve.txt")
        return
    # event.01：订阅建立 + 首事件 SSE 解帧后可解析（F4：轮询握手而非固定 1s 后判定）
    stream_abs = ctx.run_dir / stream_rel
    S.evidence_write(ctx.run_dir, stream_rel, "")
    enc = ctx.dir_param(ctx.work)
    sub = S.spawn_upstream(ctx.sbroot, ctx.work,
                           ["curl", "-sS", "-N", "--max-time", "180",
                            "-u", f"opencode:{ctx.password}",
                            f"{ctx.base}{ctx.api_prefix}/event?directory={enc}"],
                           log_file=stream_abs)
    # F9：取得 sub 后立即进入清理保护——registry.add 失败也必须杀已启动进程
    try:
        ctx.registry.add("item", sub.pid)
        _probe_events_run(ctx, sub, stream_rel, stream_abs, enc, ev)
    finally:
        S.kill_tree(sub.pid)
        close_deadline = time.monotonic() + 5
        while time.monotonic() < close_deadline:
            if sub.poll() is not None:
                break
            time.sleep(0.1)
        if sub.poll() is None:
            ctx.any_harness = True   # 订阅未确认关闭 = harness 故障证据


def _probe_events_run(ctx: Ctx, sub: "subprocess.Popen[bytes]", stream_rel: str,
                      stream_abs: Path, enc: str, ev: str) -> None:
    """probe_events 事件观测主体（订阅生命周期由调用方 finally 保证）。
    调用方已确认 ctx.base/ctx.sid 非 None。"""
    base = ctx.base
    sid = ctx.sid
    assert base is not None and sid is not None
    # F8/F29：持久增量解帧器——增量 UTF-8 decoder 保留半字符，行缓冲保留半行；
    # 空行照常 feed 以提交帧；每帧记录其首行字节偏移，观测基线之前的帧被过滤
    import codecs as _codecs

    class _StreamReader:
        def __init__(self) -> None:
            self.off = 0                     # 已读取的字节偏移
            self.dec = _codecs.getincrementaldecoder("utf-8")("replace")
            self.line_buf = ""               # 未完成的半行
            self.parser = S.SSEParser()      # 持久帧状态（跨读取积累 data: 行）
            self.frame_start: Optional[int] = None  # 当前积累帧首行的字节偏移
            self.next_line_off = 0           # 下一完整行起始字节偏移

        def poll(self) -> list[tuple[int, dict]]:
            """返回 [(frame_start_offset, event), ...]；半行/半字符留待下次。
            F29：帧结束（空行）即复位 frame_start——解析失败的帧不污染下一帧起点。"""
            try:
                with open(stream_abs, "rb") as f:
                    f.seek(self.off)
                    chunk = f.read()
            except OSError:
                return []
            self.off += len(chunk)
            self.line_buf += self.dec.decode(chunk)
            out: list[tuple[int, dict]] = []
            while "\n" in self.line_buf:
                line, _, self.line_buf = self.line_buf.partition("\n")
                line_off = self.next_line_off
                self.next_line_off += len((line + "\n").encode("utf-8"))
                # F34：CRLF 帧——partition 按 \n 分行后行尾残留 \r；
                # 帧边界判定与 SSEParser.feed_line 同一标准（rstrip("\r\n") 后为空）
                stripped = line.rstrip("\r")
                if self.frame_start is None and stripped.strip():
                    self.frame_start = line_off
                ev_obj = self.parser.feed_line(line)
                if stripped == "":
                    # 空行（含 CRLF 的 "\r"）= 帧边界：无论解析成败都复位帧起点
                    if ev_obj is not None:
                        fs = self.frame_start if self.frame_start is not None else line_off
                        out.append((fs, ev_obj))
                    self.frame_start = None
            return out

        def baseline(self) -> int:
            """观测起点（F29）：先 drain 已落盘但未 poll 的旧帧（消费并丢弃），
            再返回流末尾偏移——触发前积累但未读取的旧帧天然排除。"""
            self.poll()   # drain：旧帧被消费丢弃，不进入后续观测
            return self.off

    reader = _StreamReader()

    def _wait(type_candidates: tuple[str, ...], sid: Optional[str],
              required_field: Optional[str], start: int, timeout: float,
              payload_ok: Any = None) -> bool:
        # F29：只接受帧起点 ≥ 观测基线的事件——触发前积累的旧帧不得计入
        # F8：required_field / payload_ok 走结构化字段，不用整段 JSON 正则
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for fs, ev_obj in reader.poll():
                if fs >= start and S.event_matches(
                        ev_obj, type_candidates, sid,
                        required_field=required_field, payload_ok=payload_ok):
                    return True
            time.sleep(0.2)
        return False

    # 订阅握手：10s 内轮询首帧可解析；进程死亡即失败（F4）
    first_events: list[dict] = []
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if sub.poll() is not None and not stream_abs.stat().st_size:
            break
        for _, ev_obj in reader.poll():
            first_events.append(ev_obj)
        if first_events:
            break
        time.sleep(0.2)

    if first_events:
        ctx.emit("event.01", "PASS", "待验证", "可满足",
                 f"事件流建立且首事件可解析（type={first_events[0].get('type', '?')}）；"
                 "server.connected 等价物存在与否见 stream 证据", stream_rel)
    else:
        ctx.emit("event.01", "FAIL", "待验证", "不可满足",
                 "事件流订阅建立后无首事件或不可解析", stream_rel)
        for e in ("event.02a.session_created", "event.02b.session_updated",
                  "event.02d.status_changed",
                  "event.02e.attention_raised", "event.02f.attention_resolved",
                  "event.02g.session_error"):
            ctx.emit(e, "INCONCLUSIVE", "待验证", "待验证",
                     "订阅失败：无法安全触发（D4 event.02 总述）", stream_rel)
        return
    # event.02a：创建触发（独立新 session），观测起点 + 精确 type 候选 + 归属
    start = reader.baseline()  # F29：字节偏移观测基线
    new_id, _, _ = _create_session(ctx, base, ctx.work, "spike-event-create",
                                f"{ev}/create-trigger.json")
    if not new_id:
        ctx.emit("event.02a.session_created", "INCONCLUSIVE", "待验证", "待验证",
                 "触发行为失败：无法创建事件 fixture session", f"{ev}/create-trigger.json")
    elif _wait(("session.created", "session_created", "session.created.v2"), new_id,
               "id", start, 30):
        ctx.emit("event.02a.session_created", "PASS", "待验证", "可满足",
                 f"观测到归属 {new_id} 的创建类事件（精确 type + sessionID 归属 + 负载 id 键）",
                 stream_rel)
    else:
        ctx.emit("event.02a.session_created", "FAIL", "待验证", "不可满足",
                 "30s 内未观测到归属目标 session 的创建事件", stream_rel)
    # event.02b：更新触发（title 写入共享 fixture；触发未接受 → INCONCLUSIVE）
    start = reader.baseline()  # F29：字节偏移观测基线
    r_upd = ctx.http(ctx.api(f"/session/{ctx.sid}?directory={enc}"), method="PATCH",
                     json_body={"title": "spike-event-updated"}, timeout=30.0)
    if not r_upd.ok:
        ctx.emit("event.02b.session_updated", "INCONCLUSIVE", "待验证", "待验证",
                 f"更新触发未被接受（status={r_upd.status} err={r_upd.error}）", stream_rel)
    elif _wait(("session.updated", "session_updated", "session.renamed"), ctx.sid, "id", start, 30):
        # v2 wire：title 更新事件名为 session.renamed（session.updated 的 v2 等价物）
        ctx.emit("event.02b.session_updated", "PASS", "待验证", "可满足",
                 "观测到归属共享 session 的更新事件（精确 type + 归属校验；"
                 "v2 事件名 session.renamed）", stream_rel)
    else:
        ctx.emit("event.02b.session_updated", "FAIL", "待验证", "不可满足",
                 "30s 内未观测到归属目标 session 的更新事件", stream_rel)
    # event.02c（删除事件）为破坏性操作——已移至 probe_destructive，
    # 排在共享 fixture 全部消费者之后（D3 fixture DAG，F7）
    # event.02d：状态变更（F7：实际尝试触发——提交 prompt 驱动状态机；
    # F27：无合法 ID/端点时不得观测——自然到达的状态事件不能当作本次触发证据）
    start = reader.baseline()  # F29：字节偏移观测基线
    triggered = False
    if ctx.prompt_path:
        mid_d = ctx.make_message_id("status")
        if mid_d is not None:
            r_trig = ctx.http(f"{base}{ctx.prompt_path.replace('{sessionID}', sid)}?directory={enc}",
                              method="POST",
                              # v2 wire：prompt body 为 {"text","id"}
                              json_body={"id": mid_d,
                                         "text": "status probe ping"},
                              timeout=30.0)
            triggered = r_trig.ok
    if not triggered:
        ctx.emit("event.02d.status_changed", "INCONCLUSIVE", "待验证", "待验证",
                 "状态变更触发未执行（无提交端点/合法 ID/提交未接受）——本项不可观测",
                 stream_rel)
    elif _wait(("session.status", "session.status_changed", "status.changed"), ctx.sid,
               "status", start, 15, payload_ok=S.status_detail_ok):
        # F8：真实 JSON 键 status + 合法 StatusDetail 状态（idle/busy/retry），
        # 字段值 "status" 或 note="status" 不得命中
        ctx.emit("event.02d.status_changed", "PASS", "待验证", "可满足",
                 "观测到归属共享 session 的 status 类事件（prompt 提交驱动），"
                 "载荷含真实 status 字段且 StatusDetail 状态 ∈ {idle,busy,retry}",
                 stream_rel)
    else:
        ctx.emit("event.02d.status_changed", "INCONCLUSIVE", "待验证", "待验证",
                 "未观测到归属目标 session 的状态变更事件；状态处理需真实模型则本项不可观测",
                 stream_rel)
    # event.02e/f/g：permission/question/error 触发需模型或故障注入
    for e, note in (("event.02e.attention_raised", "permission/question 升起依赖真实模型执行"),
                    ("event.02f.attention_resolved", "resolved 依赖先升起"),
                    ("event.02g.session_error", "会话错误注入无公开通道")):
        ctx.emit(e, "INCONCLUSIVE", "待验证", "待验证",
                 f"无法安全触发：{note}（D4 总述允许 INCONCLUSIVE）", stream_rel)


def probe_attention(ctx: Ctx) -> None:
    ev = "live/attention"
    if ctx.base is None:
        for p in ("01", "02", "03"):
            ctx.emit(f"attention.{p}", "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：serve 未运行", f"{ev}/no-serve.txt")
        return
    enc = ctx.dir_param(ctx.work)
    # attention.01：仅当端点可读且 pending 非空（载荷含 id/sessionID 非空值）才 PASS
    # v2 wire：/api/permission/request（pending）+ /api/permission/saved（已存）
    r1 = ctx.http(ctx.api(f"/permission/request?location={enc}"))
    r2 = ctx.http(ctx.api(f"/permission/saved?projectID={enc}"))
    ctx.evidence(f"{ev}/permission-list.json", r1.body)
    ctx.evidence(f"{ev}/permissions-list.json", r2.body)

    def _pending_len(r: S.HttpResult) -> int:
        try:
            v = json.loads(r.body)
            return len(v) if isinstance(v, list) else -1
        except json.JSONDecodeError:
            return -1

    p1, p2 = _pending_len(r1), _pending_len(r2)
    ep = f"{ev}/permission-list.json"
    best = r1 if p1 >= 0 else r2
    best_list = None
    try:
        best_list = json.loads(best.body)
    except json.JSONDecodeError:
        pass
    has_real_pending = isinstance(best_list, list) and any(
        isinstance(item, dict) and item.get("id") and item.get("sessionID")
        for item in best_list)
    if has_real_pending:
        ctx.emit("attention.01", "PASS", "待验证", "可满足",
                 f"非空 pending 快照获得（permission={p1}/permissions={p2}）；"
                 "载荷含非空 id/sessionID；v1 permission vs v2 permissions 命名差异实证", ep)
    elif p1 >= 0 or p2 >= 0:
        ctx.emit("attention.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"端点可读（permission={p1}/permissions={p2}；命名差异实证记录）但无有效 pending——"
                 "非空 fixture 需审批规则+模型触发，无法安全触发", ep)
    else:
        ctx.emit("attention.01", "INCONCLUSIVE", "待验证", "待验证",
                 "permission/permissions 端点均无有效响应；无法观测 pending 快照", ep)
    # attention.02：回复语义需独立 once/reject pending fixture（模型依赖）
    ctx.emit("attention.02", "INCONCLUSIVE", "待验证", "待验证",
             "回复实证（wire 取值发现/重复回复/不存在目标区分/REST+SSE 对账）需独立 once/reject "
             "pending fixture；无模型环境无法安全触发", ep)
    # attention.03：question 非空载荷 + 回复（F17：初始列表必须 HTTP 200 且为数组）
    # v2 wire：/api/question（若无此端点则 404 → INCONCLUSIVE）
    rq = ctx.http(ctx.api(f"/question?directory={enc}"))
    ctx.evidence(f"{ev}/question-list.json", rq.body)
    if rq.status != 200:
        ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"question 端点不可用（status={rq.status} err={rq.error}）",
                 f"{ev}/question-list.json")
        return
    q1 = _pending_len(rq)
    if q1 == -1:
        ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                 "question 端点无有效响应", f"{ev}/question-list.json")
    elif q1 == 0:
        ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                 "question 端点可读但为空；回复断言需非空 fixture（模型触发）",
                 f"{ev}/question-list.json")
    else:
        # 非空 fixture 已存在：校验目标项载荷（id/题目文本/session 归属非空），
        # 回复后确认**同一项**消失（F17）
        try:
            qlist = json.loads(rq.body)
            q0 = qlist[0]
            # F17：类型 + 非空校验（truthiness 不够——7、{} 等无效值必须拒绝）
            qid = q0.get("id") or q0.get("questionID")
            q_sid = q0.get("sessionID")
            q_text = q0.get("text") or (
                (q0.get("questions") or [{}])[0].get("question")
                if isinstance(q0.get("questions"), list) else None)
            qid = qid if isinstance(qid, str) and qid else None
            q_sid = q_sid if isinstance(q_sid, str) and q_sid else None
            q_text = q_text if isinstance(q_text, str) and q_text else None
        except (json.JSONDecodeError, IndexError, AttributeError):
            qid = None
            q_sid = None
            q_text = None
        if not qid or not q_sid or not q_text:
            ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                     f"question 载荷缺必需字段（id={bool(qid)} sessionID={bool(q_sid)} "
                     f"text={bool(q_text)}），无法构造有效回复", f"{ev}/question-list.json")
        else:
            r_rep = ctx.http(ctx.api(f"/question/{qid}/reply?directory={enc}"),
                             method="POST",
                             json_body={"answers": [["spike-answer"]]}, timeout=10.0)
            rq2 = ctx.http(ctx.api(f"/question?directory={enc}"))
            ctx.evidence(f"{ev}/question-after-reply.json", rq2.body)
            after_ids: Optional[list[str]] = None
            if rq2.status == 200:
                try:
                    after = json.loads(rq2.body)
                    if isinstance(after, list):
                        # F17：逐项严格校验——任一非对象/无效 ID 项使列表不可比较（None）
                        # 不得通过丢弃坏项来证明目标消失
                        items: list[str] = []
                        strict_ok = True
                        for q in after:
                            if not isinstance(q, dict):
                                strict_ok = False
                                break
                            qid_here = q.get("id") or q.get("questionID")
                            if not isinstance(qid_here, str) or not qid_here:
                                strict_ok = False
                                break
                            items.append(qid_here)
                        if strict_ok:
                            after_ids = items
                except json.JSONDecodeError:
                    after_ids = None
            ep_q = ctx.evidence(f"{ev}/question-reply.txt",
                                f"reply_status={r_rep.status} err={r_rep.error} "
                                f"target={qid} before={q1} after={after_ids}\n")
            if r_rep.status is None or rq2.status is None:
                ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                         f"回复或回读传输失败（reply={r_rep.error} readback={rq2.error}）", ep_q)
            elif r_rep.ok and after_ids is not None and qid not in after_ids:
                ctx.emit("attention.03", "PASS", "待验证", "可满足",
                         f"非空 question 载荷含 ID/题目/session 归属（{q_sid}）；"
                         f"答案文本数组回复被接受（HTTP {r_rep.status}）且**同一项**从列表消失",
                         ep_q)
            elif r_rep.ok and after_ids is None:
                # F17：不可比较回读（解析失败/坏项）→ INCONCLUSIVE，不得判 FAIL
                ctx.emit("attention.03", "INCONCLUSIVE", "待验证", "待验证",
                         "回复被接受但回读列表不可比较（解析失败或含无效项）", ep_q)
            elif r_rep.ok:
                ctx.emit("attention.03", "FAIL", "待验证", "不可满足",
                         f"回复被接受但目标项仍在列表（或回读不可解析）", ep_q)
            else:
                ctx.emit("attention.03", "FAIL", "待验证", "不可满足",
                         f"回复未被接受：HTTP {r_rep.status}", ep_q)


def probe_prompt(ctx: Ctx) -> None:
    ev = "live/prompt"
    if ctx.base is None or ctx.sid is None:
        for p in ("01", "02", "03"):
            ctx.emit(f"prompt.{p}", "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失：serve 或共享 session 不可用", f"{ev}/no-fixture.txt")
        return
    enc = ctx.dir_param(ctx.work)
    if not ctx.prompt_path:
        # F5：发现失败是 harness/发现层局限，不能据此认定上游能力不存在 → INCONCLUSIVE
        ctx.emit("prompt.01", "INCONCLUSIVE", "待验证", "待验证",
                 "/doc 不可获取或无 prompt 端点，无法定位提交契约（发现失败≠能力缺失）",
                 "live/discovery-doc.json")
        ctx.emit("prompt.02", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：提交端点未发现", "live/discovery-doc.json")
        ctx.emit("prompt.03", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：提交端点未发现", "live/discovery-doc.json")
        return
    submit_path = ctx.prompt_path.replace("{sessionID}", ctx.sid)
    mid = ctx.make_message_id("spike")
    if mid is None:
        # F5：无法构造满足发现 pattern 的合法 messageID——不提交伪请求
        ctx.emit("prompt.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"无法构造满足已发现 pattern（{ctx.mid_pattern}）的合法 messageID；"
                 "提交实验未执行", "live/discovery.txt")
        ctx.emit("prompt.02", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：合法 messageID 无法构造", "live/discovery.txt")
        ctx.emit("prompt.03", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：合法 messageID 无法构造", "live/discovery.txt")
        return
    r_sub = ctx.http(f"{ctx.base}{submit_path}?directory={enc}", method="POST",
                     # v2 wire：prompt body 为 {"text": ..., "id": mid}（required: text）
                     json_body={"id": mid,
                                "text": "reply with the single word: pong"},
                     timeout=30.0)
    ep_sub = ctx.evidence(f"{ev}/submit-resp.json", r_sub.body)
    ep_meta = ctx.evidence(f"{ev}/submit.txt",
                           f"submit={r_sub.status} messageID={mid} path={submit_path} "
                           f"err={r_sub.error}\n")
    # prompt.01：端点存在 + pattern 机器可读 + 成功提交（全部齐全才 PASS）
    if ctx.sid_pattern and ctx.mid_pattern and r_sub.ok:
        ctx.emit("prompt.01", "PASS", "待验证", "可满足",
                 f"端点 {ctx.prompt_path} 机器可读（sessionID={ctx.sid_pattern} "
                 f"messageID={ctx.mid_pattern}）且提交成功（HTTP {r_sub.status}）",
                 "live/discovery.txt")
    elif not (ctx.sid_pattern and ctx.mid_pattern):
        # F5：pattern 缺失是发现层局限（doc 未暴露 schema），不能据此认定能力不存在
        ctx.emit("prompt.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"ID pattern 约束未在发现契约中暴露（sid={ctx.sid_pattern} "
                 f"mid={ctx.mid_pattern}）——发现不足≠能力缺失",
                 "live/discovery.txt")
    else:
        ctx.emit("prompt.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"端点/约束可读但提交未成功（status={r_sub.status} err={r_sub.error}）", ep_meta)
    # prompt.02：messageID 透传（必须有回显证据才 PASS）
    if r_sub.status is None:
        ctx.emit("prompt.02", "INCONCLUSIVE", "待验证", "待验证",
                 f"提交传输失败（{r_sub.error}），无法观测 accepted 语义", ep_meta)
    elif not r_sub.ok:
        ctx.emit("prompt.02", "FAIL", "待验证", "不可满足",
                 f"提交未被接受：HTTP {r_sub.status}", ep_meta)
    elif _mid_echoed(r_sub.body, mid):
        ctx.emit("prompt.02", "PASS", "待验证", "可满足",
                 f"提交被接受且 messageID 在响应中结构化回显（不重写实证）；accepted 确认条件=HTTP "
                 f"{r_sub.status}；transport_unknown/pre_send_failure 为本地传输语义"
                 "（无需外部 probe）；未知交付禁止重投依据：D5 重试契约第 3 条", ep_sub)
    else:
        ctx.emit("prompt.02", "INCONCLUSIVE", "待验证", "待验证",
                 f"提交被接受（HTTP {r_sub.status}）但 messageID 回显未在响应体观测到——"
                 "透传不重写断言证据不足", ep_meta)
    # prompt.03：file part（接受≠agent 可读；无模型 → INCONCLUSIVE）
    # F26：file 的 messageID 独立构造——None 时不提交伪请求
    file_mid = ctx.make_message_id("file")
    if file_mid is None:
        ctx.emit("prompt.03", "INCONCLUSIVE", "待验证", "待验证",
                 "无法为 file part 构造满足已发现 pattern 的合法 messageID；提交实验未执行",
                 "live/discovery.txt")
        return
    fpath = ctx.work / "attach-probe.txt"
    S.sandbox_write(fpath, "spike attachment probe content\n")
    r_file = ctx.http(f"{ctx.base}{submit_path}?directory={enc}", method="POST",
                      # v2 wire：PromptInput.FileAttachment 要求 uri 字段（required）
                      json_body={"id": file_mid,
                                 "text": "read the attached file",
                                 "files": [{"uri": f"file://{fpath}",
                                            "name": "attach-probe.txt"}]},
                      timeout=30.0)
    ep_f = ctx.evidence(f"{ev}/file-submit.txt",
                        f"file-submit={r_file.status} err={r_file.error}\n")
    if r_file.ok:
        ctx.emit("prompt.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"file part 形态被接受（HTTP {r_file.status}）但「agent 可读文件」需真实模型实证，"
                 "本轮无模型环境", ep_f)
    elif r_file.status is None:
        ctx.emit("prompt.03", "INCONCLUSIVE", "待验证", "待验证",
                 f"file part 提交传输失败（{r_file.error}）", ep_f)
    else:
        ctx.emit("prompt.03", "FAIL", "不兼容", "不可满足",
                 f"file part 形态被拒绝：HTTP {r_file.status}（拒绝形态记录于证据）", ep_f)


def probe_status_capability_title(ctx: Ctx) -> None:
    ev = "live/misc"
    if ctx.base is None or ctx.sid is None:
        for p in ("status.01", "capability.01", "title.01"):
            ctx.emit(p, "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失", f"{ev}/no-fixture.txt")
        return
    enc = ctx.dir_param(ctx.work)
    # status.01（F6：必须 200 + 可解析为对象；HTTP 500 的 JSON 错误体不算 PASS）
    r_st = ctx.http(ctx.api(f"/session/status?directory={enc}"))
    ep = ctx.evidence(f"{ev}/status.json", r_st.body)
    if r_st.status is None:
        ctx.emit("status.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"status 端点传输失败（{r_st.error}）", ep)
    elif r_st.status != 200:
        ctx.emit("status.01", "FAIL", "待验证", "不可满足",
                 f"status 端点返回 {r_st.status}（非 200；响应体可能是错误对象，不作形状证据）", ep)
    else:
        try:
            parsed = json.loads(r_st.body)
            if not isinstance(parsed, (dict, list)):
                raise json.JSONDecodeError("not a container", r_st.body.decode(), 0)
            # F6：提取已知状态枚举清单作为证据
            enums: set[str] = set()
            def _collect_status(x: Any) -> None:
                if isinstance(x, dict):
                    v = x.get("status") or x.get("type")
                    if isinstance(v, str):
                        enums.add(v)
                    for vv in x.values():
                        _collect_status(vv)
                elif isinstance(x, list):
                    for vv in x:
                        _collect_status(vv)
            _collect_status(parsed)
            ctx.emit("status.01", "PASS", "待验证", "可满足",
                     f"status 响应 200 且形状可解析；已知状态枚举={sorted(enums)}；"
                     "未知枚举归一（StatusUnknown）属内部适配语义", ep)
        except json.JSONDecodeError:
            ctx.emit("status.01", "FAIL", "待验证", "不可满足",
                     "status 端点响应不可解析", ep)
    # capability.01：/doc 无凭证 401 + 带凭证可解析 + prompt 端点形状机器可读
    r_anon = ctx.http(f"{ctx.base}/doc", auth=False)
    doc_ok = bool(ctx.prompt_path)
    ep2 = ctx.evidence(f"{ev}/doc-codes.txt",
                       f"doc_anon={r_anon.status} doc_auth_parseable={doc_ok}\n")
    if r_anon.status == 401 and doc_ok:
        ctx.emit("capability.01", "PASS", "待验证", "可满足",
                 "/doc 无凭证 401、带凭证可解析（diffreview probePromptAsync 依赖成立）", ep2)
    elif r_anon.status is None:
        ctx.emit("capability.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"无凭证 /doc 观测传输失败（{r_anon.error}）", ep2)
    else:
        ctx.emit("capability.01", "FAIL", "待验证", "不可满足",
                 f"capability 探测不满足：anon={r_anon.status} parseable={doc_ok}", ep2)
    # title.01：写入并经 GET 读回
    r_tw = ctx.http(ctx.api(f"/session/{ctx.sid}?directory={enc}"), method="PATCH",
                     json_body={"title": "spike-title-probe"}, timeout=30.0)
    ctx.evidence(f"{ev}/title-write.json", r_tw.body)
    r_tg = ctx.http(ctx.api(f"/session/{ctx.sid}?directory={enc}"))
    ep3 = ctx.evidence(f"{ev}/title-get.json", r_tg.body)
    # F6：解析字段核对 title，不用字节子串；v2 envelope：GET 单资源包在 {"data": {...}}
    title_read = ""
    if r_tg.status == 200 and r_tg.body:
        try:
            parsed_t = json.loads(r_tg.body)
            if isinstance(parsed_t, dict) and isinstance(parsed_t.get("data"), dict):
                parsed_t = parsed_t["data"]   # v2 envelope 解包
            if isinstance(parsed_t, dict):
                title_read = parsed_t.get("title") or ""
        except json.JSONDecodeError:
            pass
    if r_tw.ok and title_read == "spike-title-probe":
        ctx.emit("title.01", "PASS", "待验证", "可满足",
                 "title 写入后经 GET 读回（title 字段精确匹配）；字段名记录于证据", ep3)
    elif r_tg.status is None or r_tw.status is None:
        ctx.emit("title.01", "INCONCLUSIVE", "待验证", "待验证",
                 "title 写入/回读观测传输失败", ep3)
    else:
        ctx.emit("title.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"title 写入路径未确认（PATCH={r_tw.status} 可能非 v2 形态；"
                 "事件观测路径见 event 证据）", ep3)


def probe_terminal_conn_recovery(ctx: Ctx) -> None:
    ev = "live/lifecycle"
    if ctx.base is None or ctx.sid is None:
        for p in ("terminal.01", "conn.01", "recovery.01"):
            ctx.emit(p, "INCONCLUSIVE", "待验证", "待验证",
                     "依赖事实缺失", f"{ev}/no-fixture.txt")
        return
    enc = ctx.dir_param(ctx.work)
    # terminal.01：第二实例共享可见（解析 ID 集合）+ 不存在 ID 非零退出拒绝（单条结果）
    # 实机修正：v2 的 serve 子命令不接受 --session flag——第二实例仅同 directory 启动
    # F45：列表可比性独立三态——ok=有效列表含目标 / fail=有效列表明确缺目标 /
    # None=传输失败、非 200、解析失败或形状不符（不可比较，不得折叠为 FAIL）
    second_list_state: Optional[bool] = None
    rc2 = ctx.serve_start("second", ctx.locked["v2"]["exec_path"], 29712,
                          ctx.sbroot, ctx.work)
    if rc2 == 0:
        r_2nd = ctx.http(f"http://127.0.0.1:29712{ctx.api_prefix}/session?directory={enc}")
        ctx.evidence(f"{ev}/second-list.json", r_2nd.body)
        ids2 = _parse_id_list(r_2nd)
        if r_2nd.status == 200 and ids2 is not None:
            second_list_state = ctx.sid in ids2
    # F45 修复：不存在 ID 拒绝子断言——v2 移除了 serve --session（CLI 侧无
    # session 选择操作），原「第二实例选择不存在 session 拒绝启动/连接」语义
    # 在 v2 无等价形态。REST GET 404 只证明查询语义，不能代证 CLI 拒绝语义。
    missing_id = f"{ctx.sid_prefix}_doesnotexist00000000000000"
    r_miss = ctx.http(f"{ctx.base}{ctx.api_prefix}/session/{missing_id}?directory={enc}",
                      timeout=10.0)
    ctx.evidence(f"{ev}/attach-missing.txt",
                 f"GET {ctx.api_prefix}/session/{missing_id} -> {r_miss.status} "
                 f"err={r_miss.error}\n")
    query_404 = r_miss.status == 404
    if second_list_state is True:
        # 共享可见已实证；拒绝语义无 CLI 等价形态 → 整项 INCONCLUSIVE（契约未修订为
        # REST 口径前，不以部分事实判 PASS）。REST 查询拒绝事实如实记录。
        ctx.emit("terminal.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"第二实例（同 directory serve）共享可见已实证（second-list.json）；"
                 f"「选择不存在 session 拒绝」在 v2 无 CLI 等价形态（serve 无 --session）"
                 f"——REST 查询不存在 ID 返回 {r_miss.status}（仅查询语义实证，"
                 f"不覆盖原启动/连接拒绝断言）",
                 f"{ev}/second-list.json")
    elif second_list_state is False:
        # 有效列表明确缺少目标 session——有效负面观测
        ctx.emit("terminal.01", "FAIL", "待验证", "不可满足",
                 f"第二实例有效列表（HTTP 200 可解析）明确缺少目标 session "
                 f"（共享可见不成立）；不存在 ID 查询 status={r_miss.status}",
                 f"{ev}/second-list.json")
    else:
        # second_list_state is None：传输失败/非 200/解析失败/形状不符/serve 未就绪——
        # 均不可比较，不得折叠为 FAIL
        ctx.emit("terminal.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"观测不完整：第二实例 rc={rc2} 列表不可比较（传输/非200/解析/形状）、"
                 f"不存在 ID 查询 status={r_miss.status}",
                 f"{ev}/attach-missing.txt")
    # conn.01：A 的流断开并重连（F30：精确断开 A 订阅；F6：重连必须产生可解析首事件才 PASS）
    if second_list_state is True:
        rel_a_rel = f"{ev}/stream-a.ndjson"
        rel_a = ctx.run_dir / rel_a_rel
        S.evidence_write(ctx.run_dir, rel_a_rel, "")
        enc2 = ctx.dir_param(ctx.work)
        sub_a = S.spawn_upstream(ctx.sbroot, ctx.work,
                                 ["curl", "-sS", "-N", "--max-time", "60",
                                  "-u", f"opencode:{ctx.password}",
                                  f"http://127.0.0.1:29712/event?directory={enc2}"],
                                 log_file=rel_a)
        try:
            ctx.registry.add("item", sub_a.pid)
        except Exception:
            S.kill_tree(sub_a.pid)
            raise
        time.sleep(1)
        S.kill_tree(sub_a.pid)   # 只断开 A 的订阅
        time.sleep(1)
        before = 0
        try:
            before = len(rel_a.read_text(errors="replace").splitlines())
        except OSError:
            pass
        sub_a2 = S.spawn_upstream(ctx.sbroot, ctx.work,
                                  ["curl", "-sS", "-N", "--max-time", "60",
                                   "-u", f"opencode:{ctx.password}",
                                   f"http://127.0.0.1:29712/event?directory={enc2}"],
                                  log_file=rel_a)
        try:
            ctx.registry.add("item", sub_a2.pid)
        except Exception:
            S.kill_tree(sub_a2.pid)
            raise
        time.sleep(2)
        after_events = S.sse_parse_line_events(
            rel_a.read_text(errors="replace").splitlines()[before:])
        ctx.evidence(f"{ev}/reconnect.txt",
                     f"lines_before={before} reconnect_new_events={len(after_events)}\n")
        if after_events:
            ctx.emit("conn.01", "PASS", "待验证", "可满足",
                     f"实例 B 可见实例 A 的 session（共享存储实证）；A 的流断开后重连成功，"
                     f"重连后新增可解析事件 {len(after_events)} 条（对账口径：首事件形状见证据）",
                     f"{ev}/reconnect.txt")
        else:
            ctx.emit("conn.01", "INCONCLUSIVE", "待验证", "待验证",
                     "重连后未观测到新事件——对账口径（全量快照 vs 增量）无法实证",
                     f"{ev}/reconnect.txt")
    else:
        ctx.emit("conn.01", "INCONCLUSIVE", "待验证", "待验证",
                 "第二实例不可用，共享存储语义无法观测", f"{ev}/second-list.json")
    # recovery.01：重启后 list + GET 双断言
    for name in list(ctx.serve_procs):
        ctx.serve_stop(name)
    time.sleep(1)
    rc3 = ctx.serve_start("restarted", ctx.locked["v2"]["exec_path"], 29711,
                          ctx.sbroot, ctx.work)
    if rc3 != 0:
        ctx.emit("recovery.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"serve 重启未就绪（rc={rc3}），恢复事实无法观测", f"{ev}/serve-restarted.log")
        return
    ctx.base = "http://127.0.0.1:29711"
    r_rl = ctx.http(ctx.api(f"/session?directory={enc}"))
    ctx.evidence(f"{ev}/restart-list.json", r_rl.body)
    ids_r = _parse_id_list(r_rl)
    listed = ids_r is not None and ctx.sid in ids_r
    get_code: Optional[int] = None
    get_transport_fail = False
    if listed:
        r_rg = ctx.http(ctx.api(f"/session/{ctx.sid}?directory={enc}"))
        get_code = r_rg.status
        get_transport_fail = r_rg.status is None
        # F6：GET 200 必须核对响应体 session ID 与目标一致
        if r_rg.status == 200:
            get_id_ok = False
            try:
                parsed_g = json.loads(r_rg.body)
                if isinstance(parsed_g, dict):
                    if isinstance(parsed_g.get("data"), dict):
                        parsed_g = parsed_g["data"]   # v2 envelope 解包
                    gid = parsed_g.get("id") or parsed_g.get("sessionID")
                    get_id_ok = gid == ctx.sid
            except json.JSONDecodeError:
                get_id_ok = False
            if not get_id_ok:
                get_code = -1   # 200 但内容不符：协议事实不满足
        ctx.evidence(f"{ev}/restart-get.json", r_rg.body)
    ep_r = ctx.evidence(f"{ev}/restart-get.txt", f"listed={listed} get={get_code}\n")
    if r_rl.status is None or get_transport_fail:
        ctx.emit("recovery.01", "INCONCLUSIVE", "待验证", "待验证",
                 f"重启后观测传输失败（list={r_rl.error}）", ep_r)
    elif listed and get_code == 200:
        ctx.emit("recovery.01", "PASS", "待验证", "可满足",
                 "重启后既有 session 可列出且可 GET 读取（200）；endpoint/凭据/目录解析所需"
                 "上游事实（BindingRuntimeEndpoint 对应面）重启后可获得", ep_r)
    elif not listed:
        ctx.emit("recovery.01", "FAIL", "待验证", "不可满足",
                 f"重启后既有 session 列表不可见（status={r_rl.status}）", ep_r)
    else:
        ctx.emit("recovery.01", "FAIL", "待验证", "不可满足",
                 f"重启后列表可见但 GET={get_code}", ep_r)


def emit_exemptions(ctx: Ctx) -> None:
    ctx.evidence("live/exempt.txt", "SPI 豁免记录（D4 覆盖表理由）\n")
    ctx.emit("spi.exempt.runtime_supervisor.acquire_ephemeral", "PASS", "不适用", "不适用",
             "无需外部 probe：ocdeck 删除清理专用的内部生命周期语义，上游无对应契约面",
             "live/exempt.txt")
    ctx.emit("spi.exempt.runtime_supervisor.release", "PASS", "不适用", "不适用",
             "无需外部 probe：进程终止/清理为 ocdeck 托管语义；上游侧行为由 terminal.01 间接覆盖",
             "live/exempt.txt")
    ctx.emit("spi.exempt.provider.meta", "PASS", "不适用", "不适用",
             "无需外部 probe：registry 装配为 ocdeck 内部语义", "live/exempt.txt")
    ctx.emit("spi.exempt.provider.binding_runtime_endpoint", "PASS", "不适用", "不适用",
             "无需外部 probe：provider 私有 data 解码为内部语义；endpoint/凭据/目录的上游事实"
             "由 recovery.01 覆盖", "live/exempt.txt")


# F3：probe 组 → 该组负责的结果 ID 集合（组异常时补齐未完成项的 harness_invalid 记录）
def probe_destructive(ctx: Ctx) -> None:
    """破坏性删除 probe（D3 fixture DAG，F7）：独立专用 fixture，
    排在共享 fixture 全部消费者之后执行。"""
    ev = "live/destructive"
    if ctx.base is None:
        ctx.evidence(f"{ev}/no-serve.txt", "serve 未运行\n")
        ctx.emit("session.04", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：serve 未运行", f"{ev}/no-serve.txt")
        ctx.emit("event.02c.session_deleted", "INCONCLUSIVE", "待验证", "待验证",
                 "依赖事实缺失：serve 未运行", f"{ev}/no-serve.txt")
        return
    # session.04：独立专用 fixture 删除
    del_id, _, _ = _create_session(ctx, ctx.base, ctx.work, "spike-delete",
                                f"{ev}/delete-create.json")
    if not del_id:
        ctx.emit("session.04", "INCONCLUSIVE", "待验证", "待验证",
                 "无法创建删除专用 fixture session", f"{ev}/delete-create.json")
    else:
        r_del = ctx.http(ctx.api(f"/session/{del_id}?directory={ctx.dir_param(ctx.work)}"),
                         method="DELETE")
        r_af = ctx.http(ctx.api(f"/session/{del_id}?directory={ctx.dir_param(ctx.work)}"))
        after_missing = r_af.status == 404 or (
            r_af.status is not None and r_af.status < 500
            and bool(re.search(rb"(?i)not.?found|no.?such|missing", r_af.body)))
        ep4 = ctx.evidence(f"{ev}/delete-codes.txt",
                           f"delete={r_del.status} after={r_af.status} "
                           f"after_missing={after_missing}\n")
        if r_del.status is None or r_af.status is None:
            ctx.emit("session.04", "INCONCLUSIVE", "待验证", "待验证",
                     f"删除观测传输失败：delete={r_del.error} after={r_af.error}", ep4)
        elif r_del.status is not None and 200 <= r_del.status < 300 and after_missing:
            ctx.emit("session.04", "PASS", "待验证", "可满足",
                     f"删除成功(delete={r_del.status})且删除后 GET={r_af.status}（明确不存在信号）", ep4)
        else:
            ctx.emit("session.04", "FAIL", "待验证", "不可满足",
                     f"删除语义不满足：delete={r_del.status} after={r_af.status}", ep4)
    # event.02c：删除事件（独立专用 session + 独立短订阅，后解析全量流）
    enc = ctx.dir_param(ctx.work)
    stream_rel = f"{ev}/stream.ndjson"
    stream_abs = ctx.run_dir / stream_rel
    S.evidence_write(ctx.run_dir, stream_rel, "")
    sub = S.spawn_upstream(ctx.sbroot, ctx.work,
                           ["curl", "-sS", "-N", "--max-time", "60",
                            "-u", f"opencode:{ctx.password}",
                            f"{ctx.base}{ctx.api_prefix}/event?directory={enc}"],
                           log_file=stream_abs)
    try:
        ctx.registry.add("item", sub.pid)
    except Exception:
        S.kill_tree(sub.pid)
        raise
    del_ev, _, _ = _create_session(ctx, ctx.base, ctx.work, "spike-event-delete",
                                f"{ev}/delete-trigger.json")
    if not del_ev:
        ctx.emit("event.02c.session_deleted", "INCONCLUSIVE", "待验证", "待验证",
                 "无法创建删除专用 fixture", f"{ev}/delete-trigger.json")
    else:
        # 订阅握手：等首帧可解析（确认订阅建立）再触发删除
        import time as _t
        deadline = _t.monotonic() + 10
        subscribed = False
        while _t.monotonic() < deadline:
            if S.sse_parse_line_events(
                    stream_abs.read_text(errors="replace").splitlines()):
                subscribed = True
                break
            if sub.poll() is not None and not stream_abs.stat().st_size:
                break
            _t.sleep(0.2)
        if not subscribed:
            ctx.emit("event.02c.session_deleted", "INCONCLUSIVE", "待验证", "待验证",
                     "删除事件订阅未建立（无首帧/进程死亡）", stream_rel)
        else:
            # v2 wire：删除走 /api 前缀（与 session.04 同口径）；漏前缀会被 SPA 路由
            # 解析成 405/404，删除根本未发生
            r_del = ctx.http(ctx.api(f"/session/{del_ev}?directory={enc}"),
                             method="DELETE", timeout=30.0)
            if not r_del.ok:
                ctx.emit("event.02c.session_deleted", "INCONCLUSIVE", "待验证", "待验证",
                         f"删除触发未被接受（status={r_del.status}）", stream_rel)
            else:
                deadline = _t.monotonic() + 30
                found = False
                while _t.monotonic() < deadline and not found:
                    for ev_obj in S.sse_parse_line_events(
                            stream_abs.read_text(errors="replace").splitlines()):
                        if S.event_matches(
                                ev_obj,
                                ("session.deleted", "session_deleted", "session.removed"),
                                del_ev, None):
                            found = True
                            break
                    if not found:
                        _t.sleep(0.2)
                if found:
                    ctx.emit("event.02c.session_deleted", "PASS", "待验证", "可满足",
                             f"观测到归属 {del_ev} 的删除事件（独立 fixture，精确 type+归属）",
                             stream_rel)
                else:
                    ctx.emit("event.02c.session_deleted", "FAIL", "待验证", "不可满足",
                             "30s 内未观测到归属目标 session 的删除事件", stream_rel)
    S.kill_tree(sub.pid)


GROUP_IDS: dict[str, list[str]] = {
    "probe_supervisor": ["supervisor.01", "supervisor.02", "supervisor.03"],
    "probe_sessions": ["session.01", "session.02", "session.03",
                       "session.05", "session.06", "session.07"],
    "probe_events": ["event.01",
                     "event.02a.session_created", "event.02b.session_updated",
                     "event.02d.status_changed",
                     "event.02e.attention_raised", "event.02f.attention_resolved",
                     "event.02g.session_error"],
    "probe_attention": ["attention.01", "attention.02", "attention.03"],
    "probe_prompt": ["prompt.01", "prompt.02", "prompt.03"],
    "probe_status_capability_title": ["status.01", "capability.01", "title.01"],
    "probe_terminal_conn_recovery": ["terminal.01", "conn.01", "recovery.01"],
    "probe_destructive": ["session.04", "event.02c.session_deleted"],
    "emit_exemptions": ["spi.exempt.runtime_supervisor.acquire_ephemeral",
                        "spi.exempt.runtime_supervisor.release",
                        "spi.exempt.provider.meta",
                        "spi.exempt.provider.binding_runtime_endpoint"],
}


def run_probes_mode(ctx: Ctx) -> None:
    # F3/F19：probe 级故障隔离——但退出码 2/5（前置拒绝/安全越界）必须原样传播，
    # 立即停止后续实验（不得吞成 harness 4 继续执行）。
    for fn in (probe_supervisor, probe_sessions, probe_events, probe_attention,
               probe_prompt, probe_status_capability_title, probe_terminal_conn_recovery,
               probe_destructive, emit_exemptions):
        try:
            fn(ctx)
            # F3：组正常返回后也核对缺失 ID（emit 故障被吞时组仍正常返回）
            for pid in GROUP_IDS.get(fn.__name__, []):
                if pid not in ctx.completed:
                    ctx.emit_harness(pid, f"组 {fn.__name__} 正常返回但该 ID 无结果记录")
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise   # 前置拒绝/安全越界：立即终止全部流程
            ctx.any_harness = True
            print(S.redact_text(f"harness error in {fn.__name__}: {e}"), file=sys.stderr)
            # F3：补齐本组未完成 ID（每 probe 恰好一条，含组内中断）
            for pid in GROUP_IDS.get(fn.__name__, []):
                if pid not in ctx.completed:
                    ctx.emit_harness(pid, f"组 {fn.__name__} harness 故障中断：{e}")
        except Exception as e:  # 非契约异常不得中断独立后续项
            ctx.any_harness = True
            print(S.redact_text(
                f"harness error in {fn.__name__}: {type(e).__name__}: {e}"), file=sys.stderr)
            for pid in GROUP_IDS.get(fn.__name__, []):
                if pid not in ctx.completed:
                    ctx.emit_harness(pid, f"组 {fn.__name__} 意外异常中断：{type(e).__name__}")
    # F4：先停止全部残留进程（泵随 stdout EOF 结束），再有界 join 泵，再消费泵错误
    ctx.registry.cleanup_all()
    S.pump_join_all()
    if S.pump_still_alive() or S.pump_errors():
        ctx.any_harness = True
        # F4：泵故障影响本阶段全部已落盘 valid 记录——D8 不得消费这些 valid 负面
        n = S.invalidate_matching_results(
            ctx.run_dir, ctx.locked,
            lambda obj: obj.get("probe_id") in S.LIVE_PROBE_IDS
            and not str(obj.get("probe_id", "")).startswith("migration."),
            " [harness: log pump failure; live evidence stream may be incomplete]")
        print(S.redact_text(
            f"harness error: log pump failure, invalidated {n} records"),
              file=sys.stderr)


# ================================================================== migration 模式

MIG_STATES = {
    # 状态 → (exec_status, capability_verdict, evidence_validity, failure_classes 覆盖)
    "NOT_STARTED_DEPENDENCY_MISSING": ("INCONCLUSIVE", "待验证", "valid", []),
    "NOT_STARTED_FIXTURE_INVALID": ("INCONCLUSIVE", "待验证", "valid", ["INCONCLUSIVE"]),
    "STARTED_INCONCLUSIVE": ("INCONCLUSIVE", "待验证", "valid", ["INCONCLUSIVE"]),
    "VALID_NEGATIVE": ("FAIL", "不可满足", "valid", None),
    "VALID_PASS": ("PASS", "可满足", "valid", []),
    "HARNESS_INVALID": ("INCONCLUSIVE", "待验证", "harness_invalid", ["HARNESS_FAILURE"]),
}


def mig_emit(ctx: Ctx, which: str, state: str, failure_classes: Optional[list[str]],
             diff_class: str, reason: str) -> None:
    """D5 结局六状态机 → 统一结果 schema（唯一映射表）。"""
    if state not in MIG_STATES:
        S.harness_fail(f"unknown migration state: {state}")
    es, verdict, validity, fc_override = MIG_STATES[state]
    fc = fc_override if fc_override is not None else (failure_classes or [])
    if state == "HARNESS_INVALID":
        ctx.any_harness = True
    # 证据路径必须指向真实存在的文件（F13）：先落一条状态说明
    ev_path = ctx.evidence(f"migration/{which}-{state.lower()}.txt",
                           f"state={state}\nfailure_classes={fc}\nreason={reason}\n")
    # F3/F10：任一关键证据写入失败 → 该方向整体 harness_invalid，不得定稿 valid
    if (ev_path in ctx.evidence_broken
            or any(p.startswith("migration/") for p in ctx.evidence_broken)):
        validity = "harness_invalid"
        ctx.any_harness = True
        if state != "HARNESS_INVALID":
            es, verdict, validity = "INCONCLUSIVE", "待验证", "harness_invalid"
            fc = ["HARNESS_FAILURE"]
            reason = f"{reason}；关键证据写入失败，方向不得定稿为 valid"
    if not (ctx.run_dir / ev_path).is_file():
        S.harness_fail(f"migration evidence missing after write: {ev_path}")
    try:
        S.emit_result(ctx.run_dir, ctx.locked, f"migration.{which}", es,
                      diff_class, verdict, validity, reason, ev_path, "", fc)
        ctx.completed.add(f"migration.{which}")
    except S.SpikeExit as e:
        if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
            raise   # F19：migration 封装同样传播 2/5
        # F39：结果落盘失败必须抛出，让外层补齐未完成方向（各恰好一条 HARNESS_INVALID）
        ctx.any_harness = True
        ctx.completed.discard(f"migration.{which}")
        raise S.SpikeExit(S.EXIT_HARNESS, f"migration.{which} result write failed: {e}") from e
    if es == "FAIL":
        ctx.any_fail = True
    elif es == "INCONCLUSIVE":
        ctx.any_inconclusive = True


def _freeze_hashes(root: Path) -> dict[str, str]:
    """契约格式校验值清单（relpath → 64-hex sha256）：仅普通文件，
    符号链接与目录不进本表（snapshot_checksums 字段契约保持纯哈希）。"""
    out: dict[str, str] = {}
    for p in sorted(root.rglob("*")):
        if p.is_file() and not p.is_symlink():
            out[str(p.relative_to(root))] = hashlib.sha256(p.read_bytes()).hexdigest()
    return out


def _freeze(root: Path) -> dict[str, str]:
    """完整校验值清单（F10：文件 sha256 + 符号链接目标 + 目录结构，relpath → 值）。
    前缀区分类型：file:/link:/dir:；目录与链接同为迁移完整性断言对象。"""
    out: dict[str, str] = {}
    for p in sorted(root.rglob("*")):
        rel = str(p.relative_to(root))
        if p.is_symlink():
            out[rel] = "link:" + os.readlink(p)
        elif p.is_file():
            out[rel] = "file:" + hashlib.sha256(p.read_bytes()).hexdigest()
        elif p.is_dir():
            out[rel] = "dir:"
    return out


def run_migration_mode(ctx: Ctx) -> None:
    ev = "migration"
    S.evidence_mkdir(ctx.run_dir, f"{ev}/fixture-content")  # F28：run 侧受保护入口
    mig_work = ctx.sbroot / "migration" / os.environ.get("SPIKE_RUN_ID", "unknown")
    S.assert_sandbox_path(mig_work)
    if mig_work.exists():
        S.harness_fail(f"migration work dir already exists (runs must not reuse): {mig_work}")
    S.sandbox_mkdir(mig_work)

    # 前置依赖（D3 机械谓词）
    missing = S.migration_deps_satisfied(ctx.run_dir / "results.jsonl")
    if missing:
        for which in ("forward", "reverse"):
            mig_emit(ctx, which, "NOT_STARTED_DEPENDENCY_MISSING", None, "待验证",
                     f"前置依赖事实缺失：{','.join(missing)}")
        return

    # ---- 步骤 1：v1 造数与冻结（实例独立环境根；配置样本先于 serve 启动写入，F10） ----
    gen = mig_work / "fixture-gen"
    for sub in ("home", "xdg/config", "xdg/data", "xdg/state", "xdg/cache", "tmp", "cwd"):
        S.sandbox_mkdir(gen / sub)
    S.sandbox_write(gen / "cwd" / "opencode.json",
                    '{"$schema":"https://opencode.ai/config.json","theme":"spike-fixture"}\n')
    rc = ctx.serve_start("fixture-gen", ctx.locked["v1"]["exec_path"], 29721, gen, gen / "cwd")
    forward_started = False
    if rc != 0:
        mig_emit(ctx, "forward", "NOT_STARTED_FIXTURE_INVALID", None, "待验证",
                 f"fixture 造数失败：v1 serve 未就绪（rc={rc}；1=进程死亡 2=超时——"
                 "超时/环境不归迁移违约）")
    else:
        gbase = "http://127.0.0.1:29721"
        gdir = ctx.dir_param(gen / "cwd")
        # migration 进程独立发现：v1 fixture serve 的 ID pattern（F5 发现驱动）
        _discover_id_patterns(ctx, gbase, "fixture-gen")
        fsid, _ = None, None
        r_c = ctx.http(f"{gbase}/session?directory={gdir}", method="POST",
                       json_body={"title": "spike-migration-fixture"}, timeout=30.0)
        ctx.evidence(f"{ev}/fixture-session.json", r_c.body)
        if r_c.ok:
            # F2：创建响应经脱敏文本入口（ID 提取用，无需原始字节）
            tmp_rel = f"{ev}/.fixture-session.json"
            S.evidence_write(ctx.run_dir, tmp_rel, r_c.body.decode("utf-8", "replace"))
            fsid, _ = _extract_session_id(ctx.run_dir / tmp_rel)
            (ctx.run_dir / tmp_rel).unlink(missing_ok=True)
        if not fsid:
            mig_emit(ctx, "forward", "NOT_STARTED_FIXTURE_INVALID", None, "待验证",
                     "fixture 造数不完整：session 创建失败")
        else:
            fmid = ctx.make_message_id("fixture")
            if fmid is None:
                mig_emit(ctx, "forward", "NOT_STARTED_FIXTURE_INVALID", None, "待验证",
                         "fixture 造数不完整：无法构造满足发现 pattern 的 messageID")
                migration_reverse(ctx, mig_work)
                return
            ctx.http(f"{gbase}/session/{fsid}/prompt_async?directory={gdir}", method="POST",
                     json_body={"messageID": fmid,
                                "parts": [{"type": "text", "text": "fixture marker ping"}]},
                     timeout=60.0)
            time.sleep(3)
            r_perm = ctx.http(f"{gbase}/permission?directory={gdir}", timeout=10.0)
            perm_actual = ""
            perm_ok = False
            perm_sample_id = ""
            perm_sample: Optional[dict] = None
            if r_perm.status == 200:
                perm_actual = r_perm.body.decode("utf-8", "replace")
                try:
                    pv = json.loads(perm_actual)
                    # 权限样本：必须是非空 JSON 数组（F25 最低完整性）
                    if isinstance(pv, list) and len(pv) > 0:
                        # F31：样本必须有真实非空 ID + 非空 dict 条目——
                        # [{}]/[null]/缺 ID 均为畸形样本，不得进入 forward
                        first = pv[0]
                        if isinstance(first, dict) and first:
                            pid = first.get("id") or first.get("permissionID")
                            if isinstance(pid, str) and pid:
                                perm_sample = first
                                perm_sample_id = pid
                                perm_ok = True
                except json.JSONDecodeError:
                    perm_ok = False
            # 消息原文：经 v1 消息历史读取契约取得（F10/F15：必须在停服前读取，
            # 停服后同一端点只剩传输失败）
            msg_bytes: Optional[bytes] = None
            r_hist = ctx.http(f"{gbase}/session/{fsid}/message?directory={gdir}",
                              timeout=15.0)
            if r_hist.status == 200 and r_hist.body:
                try:
                    msgs = json.loads(r_hist.body)
                    if isinstance(msgs, list):
                        for mobj in msgs:
                            if isinstance(mobj, dict) and (
                                    mobj.get("id") == fmid
                                    or (mobj.get("info") or {}).get("id") == fmid):
                                # F10：哈希对象为消息原文（text parts 拼接），
                                # 不是序列化 API envelope——envelope 含易变元数据，
                                # 跨版本比较无意义
                                texts: list[str] = []
                                parts = mobj.get("parts")
                                if isinstance(parts, list):
                                    for pt in parts:
                                        if isinstance(pt, dict) and \
                                                isinstance(pt.get("text"), str):
                                            texts.append(pt["text"])
                                if texts:
                                    msg_bytes = "\n".join(texts).encode("utf-8")
                                break
                except json.JSONDecodeError:
                    msg_bytes = None
            # 停写冻结：整树停止并确认退出（F24：失败不吞掉）——所有在线读取已完成
            if not ctx.serve_stop("fixture-gen"):
                mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                         "fixture 写入树停止未确认（serve_stop 超时）")
                migration_reverse(ctx, mig_work)
                return
            sums = _freeze_hashes(gen)
            # F30：完整类型清单（含链接/目录）作为独立证据；checksums 保持纯哈希契约
            S.evidence_write(ctx.run_dir, f"{ev}/snapshot-full-manifest.json",
                             json.dumps(_freeze(gen), indent=2))
            S.evidence_write(ctx.run_dir, f"{ev}/snapshot-checksums.json",
                             json.dumps(sums, indent=2))
            # msg_bytes/perm_ok 均已在停服前取得（F15：停写后只做离线核对，不再请求已停服务）
            if msg_bytes is None or not perm_ok:
                mig_emit(ctx, "forward", "NOT_STARTED_FIXTURE_INVALID", None, "待验证",
                         f"fixture 造数不完整：消息历史读取={'有' if msg_bytes else '无'} "
                         f"权限样本非空={perm_ok}（可能需真实模型）")
            else:
                # F16：原始字节写入（不脱敏），哈希输入与落盘字节完全一致
                S.artifact_write(ctx.run_dir, f"{ev}/fixture-content/msg-1.txt", msg_bytes)
                mhash = hashlib.sha256(msg_bytes).hexdigest()
                manifest = {
                    "sessions": [{"id": fsid, "messages": [
                        {"id": fmid, "content_path": "msg-1.txt",
                         "content_sha256": mhash}]}],
                    "cwd": str(gen / "cwd"),
                    "config": [{"key": "theme", "v1_source": "cwd/opencode.json#$.theme",
                                "expected": "spike-fixture"}],
                    "permissions": [{"id": perm_sample_id,
                                     "v1_source": "api:/permission?directory=<cwd>",
                                     # D5/F31：expected 为字符串化预期状态（规范化 JSON）
                                     "expected": json.dumps(perm_sample, ensure_ascii=False,
                                                            sort_keys=True)}],
                    "snapshot_checksums": sums,
                }
                S.evidence_write(ctx.run_dir, f"{ev}/fixture-manifest.json",
                                 json.dumps(manifest, ensure_ascii=False, indent=2))
                # 造数态两步谓词第①步：v1 侧实际值 == expected
                try:
                    actual_theme = json.loads(
                        (gen / "cwd" / "opencode.json").read_text()).get("theme", "")
                except (OSError, json.JSONDecodeError):
                    actual_theme = ""
                if actual_theme != "spike-fixture":
                    mig_emit(ctx, "forward", "NOT_STARTED_FIXTURE_INVALID", None, "待验证",
                             f"fixture 造数校验失败：v1 侧 config theme 实际值 "
                             f"'{actual_theme}' != expected")
                else:
                    forward_started = True
                    # F10/F31：forward 消费 manifest 中同一字符串化 expected
                    migration_forward(ctx, mig_work, gen, fsid, fmid, mhash,
                                      manifest["permissions"][0]["expected"], manifest)
    migration_reverse(ctx, mig_work)
    _ = forward_started


def migration_forward(ctx: Ctx, mig_work: Path, gen: Path, fsid: str, fmid: str,
                      mhash: str, perm_expected: Any, manifest: dict) -> None:
    ev = "migration"
    fwd = mig_work / "forward"
    if fwd.exists():
        S.harness_fail(f"forward dir exists (run reuse): {fwd}")
    S.assert_sandbox_path(fwd / "instance")
    # F30：symlinks=True 保留链接语义（默认解引用会使 link: 变 file:，快照比较失真）；
    # 复制后复核完整清单一致（含链接目标与目录结构）
    shutil.copytree(gen, fwd / "instance", symlinks=True)
    if _freeze(gen) != _freeze(fwd / "instance"):
        mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                 "迁移副本与源树完整清单不一致（含链接/目录结构）——复制不可信")
        return
    # 复制核对（F24）：副本与 canonical 快照一致性
    if _freeze(gen) != _freeze(fwd / "instance"):
        mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                 "forward 副本与 canonical 快照不一致")
        return
    # 重定位检索（D5 复制重定位规则）
    relocation_hits = [str(p) for p in (fwd / "instance").rglob("*")
                       if p.is_file() and not p.is_symlink()
                       and str(gen).encode() in p.read_bytes()[:0] == b"" and False]
    # 仅检索文本文件中的绝对路径引用
    hits = []
    for p in (fwd / "instance").rglob("*"):
        if p.is_file() and not p.is_symlink():
            try:
                content = p.read_bytes()
                if str(gen).encode() in content:
                    hits.append(str(p.relative_to(fwd / "instance")))
            except OSError:
                continue
    S.evidence_write(ctx.run_dir, f"{ev}/relocation.log",
                     "relocation policy: 仅调整环境变量与 cwd 指向新实例路径；"
                     "数据文件内绝对路径引用清单见 relocation-candidates.txt\n"
                     "改写记录：无（未经记录的手工改写一律禁止）\n")
    S.evidence_write(ctx.run_dir, f"{ev}/relocation-candidates.txt", "\n".join(hits) + "\n")
    _ = relocation_hits
    rc = ctx.serve_start("forward", ctx.locked["v2"]["exec_path"], 29722,
                         fwd / "instance", fwd / "instance" / "cwd")
    if rc != 0:
        log = ctx.run_dir / "live" / "serve-forward.log"
        # F12：只有日志含明确的数据/迁移类错误才算有效负面读取实证；
        # 配置错误、端口冲突、超时等环境原因 → INCONCLUSIVE。
        # 泛词（unsupported/database/sqlite 单独出现可能是环境/配置信息）不构成证据——
        # 要求数据损坏/迁移失败/缺表语义组合（"has no session table" 实测自 v1
        # 打开 v2 SQLite 数据：Database is not empty and has no session table）
        log_text = log.read_text(encoding="utf-8", errors="replace") if log.is_file() else ""
        has_data_error = bool(re.search(
            r"(?i)corrupt|invalid data|migration failed|schema version mismatch|"
            r"unsupported (?:data|storage|schema|version)|has no \w+ table|"
            r"(?:database|sqlite).*(?:corrupt|error|failed|mismatch)",
            log_text))
        if rc == 1 and has_data_error:
            mig_emit(ctx, "forward", "VALID_NEGATIVE", ["READABILITY_FAILURE"], "待验证",
                     "v2 打开迁移数据明确报数据类错误（serve 进程死亡且日志含数据/迁移错误，"
                     "见 serve-forward.log）——有效负面读取实证")
        else:
            mig_emit(ctx, "forward", "STARTED_INCONCLUSIVE", None, "待验证",
                     f"v2 serve 未就绪（rc={rc}）——超时/环境/配置原因不认定迁移违约")
        return
    fbase = "http://127.0.0.1:29722"
    fdir = ctx.dir_param(fwd / "instance" / "cwd")
    # migration 进程独立发现：v2 forward serve 的 ID pattern（F5 发现驱动）
    _discover_id_patterns(ctx, fbase, "forward")
    fapi = f"{fbase}/api"   # v2 wire：API 在 /api 前缀下
    classes: list[str] = []
    # ① F⊆V：fixture session 可列出（解析 ID 集合，不用字节子串，F11）
    r_ls = ctx.http(f"{fapi}/session?directory={fdir}", timeout=15.0)
    ctx.evidence(f"{ev}/forward-list.json", r_ls.body)
    listed_ids = _parse_id_list(r_ls)
    if r_ls.status is None:
        classes.append("INCONCLUSIVE")   # 传输失败不认定违约（F12）
    elif listed_ids is None:
        classes.append("INCONCLUSIVE")   # 响应不可解析，无有效读取事实
    elif fsid not in listed_ids:
        classes.append("DATA_IDENTITY_FAILURE")
    # ② 旧消息历史完整：经 v2 API 消息历史核对（ID 在 + 内容哈希一致），非磁盘文件猜测
    r_hist2 = ctx.http(f"{fapi}/session/{fsid}/message?directory={fdir}",
                       timeout=15.0)
    ctx.evidence(f"{ev}/forward-history.json", r_hist2.body)
    old_msg_bytes: Optional[bytes] = None
    if r_hist2.status is None:
        classes.append("INCONCLUSIVE")
    elif r_hist2.status >= 500:
        # F12：5xx 是服务端故障/未知，不是明确拒绝读取的负面实证
        classes.append("INCONCLUSIVE")
    elif r_hist2.status != 200:
        classes.append("READABILITY_FAILURE")   # 4xx：明确负面读取实证
    else:
        try:
            msgs2 = json.loads(r_hist2.body)
        except json.JSONDecodeError:
            # F12：不可比较 JSON → 无有效事实，不得判 IDENTITY
            classes.append("INCONCLUSIVE")
            msgs2 = None
        if msgs2 is not None:
            if not isinstance(msgs2, list):
                classes.append("INCONCLUSIVE")   # 形状不符 → 不可比较
            elif not _history_comparable(msgs2):
                # F12：畸形成员（非 dict / 无非空字符串 ID / info 非 dict）
                # 使整表不可比较，不得据此判缺失/改写
                classes.append("INCONCLUSIVE")
            else:
                for mobj in msgs2:
                    if _entry_id(mobj) == fmid:
                        # F10：与 fixture 侧同一语义——哈希对象为消息原文文本 parts，
                        # 不是序列化 API envelope
                        _texts: list[str] = []
                        _parts = mobj.get("parts")
                        if isinstance(_parts, list):
                            for _pt in _parts:
                                if isinstance(_pt, dict) and isinstance(_pt.get("text"), str):
                                    _texts.append(_pt["text"])
                        if _texts:
                            old_msg_bytes = "\n".join(_texts).encode("utf-8")
                        break
                if old_msg_bytes is None:
                    classes.append("DATA_IDENTITY_FAILURE")
                elif hashlib.sha256(old_msg_bytes).hexdigest() != mhash:
                    classes.append("DATA_IDENTITY_FAILURE")
    # ③ 配置与权限保留（执行态：v2_source 发现后原子写回 fixture manifest）
    r_pp = ctx.http(f"{fapi}/permission/request?location={fdir}", timeout=10.0)
    r_pps = ctx.http(f"{fapi}/permission/saved?projectID={fdir}", timeout=10.0)
    v2_perm, v2_ep = None, ""
    if r_pp.status == 200:
        v2_perm, v2_ep = r_pp.body.decode("utf-8", "replace"), "api:/permission?directory=<cwd>"
    elif r_pps.status == 200:
        v2_perm, v2_ep = r_pps.body.decode("utf-8", "replace"), "api:/permissions?directory=<cwd>"
    # F10：v2_source 必须是实际发现结果——文件存在才记录路径，否则标记未发现
    v2_cfg = fwd / "instance" / "cwd" / "opencode.json"
    if v2_cfg.is_file():
        manifest["config"][0]["v2_source"] = "cwd/opencode.json#$.theme"
    else:
        manifest["config"][0]["v2_source"] = "<not-found>"
    manifest["permissions"][0]["v2_source"] = v2_ep
    S.evidence_write(ctx.run_dir, f"{ev}/fixture-manifest.json",
                     json.dumps(manifest, ensure_ascii=False, indent=2))
    try:
        v2_theme = json.loads(v2_cfg.read_text()).get("theme", "")
    except (OSError, json.JSONDecodeError):
        v2_theme = ""
    if not v2_cfg.is_file():
        classes.append("INCONCLUSIVE")   # 配置来源未发现 → 不可比较
    elif v2_theme != "spike-fixture":
        classes.append("PRESERVATION_FAILURE")
    if v2_perm is None:
        classes.append("INCONCLUSIVE")   # v2 侧定位无法建立 → 证据不足
    else:
        # F10：逻辑样本比较——v2 权限列表中须存在与 fixture 样本等价的条目
        # （子集语义 F⊆V：额外项不失败；全文本相等对易变字段过严）
        v2_perm_list: Optional[list] = None
        try:
            _pv = json.loads(v2_perm)
            if isinstance(_pv, list):
                v2_perm_list = _pv
        except json.JSONDecodeError:
            v2_perm_list = None
        if v2_perm_list is None:
            classes.append("INCONCLUSIVE")   # 不可比较载荷 → 无有效事实
        else:
            # F31：畸形成员（非 dict / 无合法逻辑 ID / 空 dict）使整表不可比较，
            # 不得作为"样本消失"的有效事实
            def _entry_ok(entry: Any) -> bool:
                if not isinstance(entry, dict) or not entry:
                    return False
                pid = entry.get("id") or entry.get("permissionID")
                return isinstance(pid, str) and bool(pid)

            if any(not _entry_ok(entry) for entry in v2_perm_list):
                classes.append("INCONCLUSIVE")
            else:
                # F31：消费 manifest 中同一字符串化 expected（两步比较第②步）；
                # 按逻辑 ID 定位 v2 对应条目（id/permissionID 形态变化允许），
                # 非 ID 字段缺失/改写才判改写
                sample: Optional[dict] = None
                if isinstance(perm_expected, str):
                    try:
                        _pe = json.loads(perm_expected)
                        if isinstance(_pe, dict):
                            sample = _pe
                    except json.JSONDecodeError:
                        sample = None
                elif isinstance(perm_expected, dict):
                    sample = perm_expected
                if sample is None or not sample:
                    classes.append("INCONCLUSIVE")   # expected 不可解析 → 无有效事实
                else:
                    want_id = sample.get("id") or sample.get("permissionID")
                    target = None
                    for entry in v2_perm_list:
                        if entry.get("id") == want_id or entry.get("permissionID") == want_id:
                            target = entry
                            break
                    if target is None:
                        classes.append("PRESERVATION_FAILURE")   # 样本条目在 v2 侧消失
                    elif not all(
                            k in ("id", "permissionID")
                            or (k in target and target[k] == v)
                            for k, v in sample.items()):
                        classes.append("PRESERVATION_FAILURE")   # 非 ID 字段缺失或被改写
    # ④ 最小连续性断言：唯一 messageID 提交 → 新消息入史 + 有效后续处理 + 旧历史保留
    cmid = ctx.make_message_id("continuity")
    if cmid is None:
        # F5：无法构造合法 messageID——连续性断言不可观测（INCONCLUSIVE），不执行订阅/提交
        classes.append("INCONCLUSIVE")
    else:
        # F11/D3：前置 prompt.01 若为有效 FAIL，连续性断言保持 FAIL，不被新一次 accepted 覆盖
        prompt01_failed = False
        results_f = ctx.run_dir / "results.jsonl"
        if results_f.is_file():
            for line in results_f.read_text(encoding="utf-8").splitlines():
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if (rec.get("probe_id") == "prompt.01" and rec.get("exec_status") == "FAIL"
                        and rec.get("evidence_validity") == "valid"):
                    prompt01_failed = True
                    break
        # 事件流订阅（forward 实例），观测处理类事件
        stream_abs_rel = f"{ev}/forward-stream.ndjson"
        stream_abs = ctx.run_dir / stream_abs_rel
        S.evidence_write(ctx.run_dir, stream_abs_rel, "")
        sub = S.spawn_upstream(fwd / "instance", fwd / "instance" / "cwd",
                               ["curl", "-sS", "-N", "--max-time", "60",
                                "-u", f"opencode:{ctx.password}",
                                f"{fapi}/event?directory={fdir}"],
                               log_file=stream_abs)
        try:
            ctx.registry.add("item", sub.pid)
        except Exception:
            S.kill_tree(sub.pid)
            raise
        # F11：订阅握手必须观测到实际 SSE 首帧（进程存活≠握手成立），再提交连续性 prompt
        subscribed = False
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and not subscribed:
            if S.sse_parse_line_events(
                    stream_abs.read_text(errors="replace").splitlines()):
                subscribed = True
                break
            if sub.poll() is not None and not stream_abs.stat().st_size:
                break
            time.sleep(0.2)
        if not subscribed:
            classes.append("INCONCLUSIVE")   # 订阅未确认（无首帧/进程死亡），处理证据不可观测
        else:
            r_cont = ctx.http(f"{fapi}/session/{fsid}/prompt?directory={fdir}",
                              method="POST",
                              # v2 wire：prompt body {"text","id"}
                              json_body={"id": cmid,
                                         "text": "continuity probe pong"},
                              timeout=30.0)
            ctx.evidence(f"{ev}/continuity.txt",
                         f"continuity-submit={r_cont.status} messageID={cmid} err={r_cont.error}\n")
            # F11：已取得的 prompt.01 有效 FAIL 是独立负面事实，必须优先继承——
            # 新传输失败不得遮蔽它（D3 依赖规则）
            if prompt01_failed:
                classes.append("CONTINUITY_FAILURE")
            elif r_cont.status is None:
                classes.append("INCONCLUSIVE")   # 传输失败，不认定违约
            elif not r_cont.ok:
                classes.append("CONTINUITY_FAILURE")   # 提交协议失败（有效实证）
            else:
                # 有效后续处理证据（F11）：精确 type + 角色/完成态领域证据；
                # 用户消息入史（message.updated + role=user）不得代证处理成功
                processed = False
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline and not processed:
                    for ev_obj in S.sse_parse_line_events(
                            stream_abs.read_text(errors="replace").splitlines()):
                        if _processing_success(ev_obj, fsid, cmid):
                            processed = True
                            break
                    if not processed:
                        time.sleep(0.5)
                # 新消息入史 + 旧历史保留：经 v2 API 历史读取复核（F11，非磁盘文件猜测）
                r_hist3 = ctx.http(f"{fapi}/session/{fsid}/message?directory={fdir}",
                                   timeout=15.0)
                ctx.evidence(f"{ev}/forward-history-after.json", r_hist3.body)
                landed = False
                old_kept = False
                old_content_ok = False
                hist3_ok = False   # F12：是否取得可比较历史（200 + 合法 JSON 数组）
                if r_hist3.status == 200 and r_hist3.body:
                    try:
                        msgs3 = json.loads(r_hist3.body)
                        if _history_comparable(msgs3):
                            hist3_ok = True
                        else:
                            msgs3 = []
                    except json.JSONDecodeError:
                        msgs3 = []
                    for mobj in (msgs3 if hist3_ok else []):
                        mid_here = _entry_id(mobj)
                        if mid_here == cmid:
                            landed = True
                        if mid_here == fmid:
                            old_kept = True
                            # F11：旧历史保留必须复核内容哈希不变，
                            # 仅 ID 存在不算保留（内容可能被改写）
                            _texts3: list[str] = []
                            _parts3 = mobj.get("parts")
                            if isinstance(_parts3, list):
                                for _pt in _parts3:
                                    if isinstance(_pt, dict) and \
                                            isinstance(_pt.get("text"), str):
                                        _texts3.append(_pt["text"])
                            if _texts3 and hashlib.sha256(
                                    "\n".join(_texts3).encode("utf-8")
                                    ).hexdigest() == mhash:
                                old_content_ok = True
                if r_hist3.status is None or r_hist3.status != 200 or not hist3_ok:
                    # F12：传输失败/非 200/坏 JSON/非数组 → 未取得可比较历史，
                    # 不得经 not landed 落到 CONTINUITY_FAILURE
                    classes.append("INCONCLUSIVE")
                elif landed and old_kept and old_content_ok and processed:
                    pass   # 连续性满足（ID + 旧内容哈希 + 处理事件均成立）
                elif not landed:
                    classes.append("CONTINUITY_FAILURE")
                elif not old_kept:
                    classes.append("DATA_IDENTITY_FAILURE")
                elif not old_content_ok:
                    classes.append("DATA_IDENTITY_FAILURE")   # 旧消息内容被改写
                else:
                    classes.append("INCONCLUSIVE")   # 缺模型/处理不可观测
    # F10/F35：停写并完成必需证据后再定稿——post-open 失败不得把已 emit 的
    # valid PASS 误判成 reverse 专属故障
    if not ctx.serve_stop("forward"):
        mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                 "forward 写入树停止未确认，打开后结构证据不可信")
        return
    try:
        pre_open = _freeze(gen)
        post_open = _freeze(fwd / "instance")
        diff_lines = []
        for k in sorted(set(pre_open) | set(post_open)):
            a, b = pre_open.get(k), post_open.get(k)
            if a != b:
                diff_lines.append(
                    f"{'added' if a is None else 'removed' if b is None else 'changed'}: {k}")
        S.evidence_write(ctx.run_dir, f"{ev}/post-open-diff.txt",
                         "v2 首次打开迁移树后的结构变化（相对 fixture 源树）:\n"
                         + ("\n".join(diff_lines) if diff_lines else "(no changes)") + "\n")
    except S.SpikeExit as e:
        if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
            raise
        mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                 f"post-open 结构证据写入失败：{e}")
        return
    except OSError as e:
        mig_emit(ctx, "forward", "HARNESS_INVALID", None, "待验证",
                 f"post-open 结构证据采集失败：{type(e).__name__}: {e}")
        return
    # 汇总：多类并存按 D5 分类表顺序（结局状态机）；规范化/ID 投影规则记入 reason
    classes = sorted(set(classes), key=S.FAILURE_CLASSES.index)
    has_neg = any(c in ("READABILITY_FAILURE", "DATA_IDENTITY_FAILURE",
                        "PRESERVATION_FAILURE", "CONTINUITY_FAILURE") for c in classes)
    norm_note = ("规范化：UTF-8 + 去首尾空白；权限逻辑 ID 投影 id↔permissionID；"
                 "消息哈希对象为 text parts 原文")
    if not classes:
        mig_emit(ctx, "forward", "VALID_PASS", [], "待验证",
                 "正向迁移全部断言满足：F⊆V、消息哈希一致、配置/权限保留、"
                 f"最小连续性（新消息入史+有效后续处理+旧历史保留）；{norm_note}")
    elif has_neg:
        mig_emit(ctx, "forward", "VALID_NEGATIVE", classes, "待验证",
                 f"正向迁移有效负面实证：{classes}（逐项证据见 migration/forward-*）；{norm_note}")
    else:
        mig_emit(ctx, "forward", "STARTED_INCONCLUSIVE", classes, "待验证",
                 f"正向迁移部分断言证据不足：{classes}（逐项证据见 migration/forward-*）；{norm_note}")


def migration_reverse(ctx: Ctx, mig_work: Path) -> None:
    """反向（仅记录，不阻断 GO；安全门禁全局适用）。每 run 全新目录。"""
    ev = "migration"
    v2gen = mig_work / "v2-gen"
    for sub in ("home", "xdg/config", "xdg/data", "xdg/state", "xdg/cache", "tmp", "cwd"):
        S.sandbox_mkdir(v2gen / sub)
    rc = ctx.serve_start("v2-gen", ctx.locked["v2"]["exec_path"], 29723, v2gen, v2gen / "cwd")
    if rc != 0:
        mig_emit(ctx, "reverse", "STARTED_INCONCLUSIVE", None, "待验证",
                 f"reverse：v2 原生快照造数失败（rc={rc}）；仅记录，不作承诺")
        return
    vbase = "http://127.0.0.1:29723"
    vdir = ctx.dir_param(v2gen / "cwd")
    # migration 进程独立发现：v2 v2-gen serve 的 ID pattern（F5 发现驱动）
    _discover_id_patterns(ctx, vbase, "v2-gen")
    vapi = f"{vbase}/api"   # v2 wire：API 在 /api 前缀下
    rsid = None
    r_c = ctx.http(f"{vapi}/session?directory={vdir}", method="POST",
                   json_body={"title": "spike-reverse-fixture"}, timeout=30.0)
    ctx.evidence(f"{ev}/reverse-session.json", r_c.body)
    if r_c.ok:
        tmp_rel = f"{ev}/.reverse-session.json"
        S.evidence_write(ctx.run_dir, tmp_rel, r_c.body.decode("utf-8", "replace"))
        rsid, _ = _extract_session_id(ctx.run_dir / tmp_rel)
        (ctx.run_dir / tmp_rel).unlink(missing_ok=True)
    rmid = ctx.make_message_id("reverse")
    if rsid and rmid is not None:
        ctx.http(f"{vapi}/session/{rsid}/prompt?directory={vdir}", method="POST",
                 # v2 wire：prompt body {"text","id"}
                 json_body={"id": rmid,
                            "text": "reverse marker ping"},
                 timeout=60.0)
        time.sleep(3)
    if not ctx.serve_stop("v2-gen"):
        mig_emit(ctx, "reverse", "HARNESS_INVALID", None, "待验证",
                 "reverse 写入树停止未确认")
        return
    # 反向最低样本：≥1 v2 原生 session + ≥1 条可识别消息（落盘核验）
    rmsg = None
    if rsid and rmid is not None:
        # v1 布局：marker 在 message/msg/part 路径文件；v2 布局（spike 结论 #11）：
        # SQLite xdg/data/opencode/opencode.db（含 -wal/-shm 伴生文件，WAL 未 checkpoint
        # 时 marker 可能在 -wal）
        for p in (v2gen / "xdg" / "data").rglob("*"):
            if not p.is_file():
                continue
            if not (re.search(r"/(message|msg|part)", str(p))
                    or p.name.startswith("opencode.db")):
                continue
            try:
                if rmid.encode() in p.read_bytes():
                    rmsg = p
                    break
            except OSError:
                continue
    if not rsid or rmsg is None:
        mig_emit(ctx, "reverse", "STARTED_INCONCLUSIVE", None, "待验证",
                 f"reverse 最低样本不满足：session={'有' if rsid else '无'} "
                 f"消息落盘={'有' if rmsg else '无'}；仅记录，不作承诺")
        return
    rev_sums = _freeze(v2gen)
    S.evidence_write(ctx.run_dir, f"{ev}/reverse-snapshot-checksums.json",
                     json.dumps(rev_sums, indent=2))
    rev = mig_work / "reverse"
    S.assert_sandbox_path(rev / "instance")
    # F30：reverse 同样保留链接语义并复核完整清单
    shutil.copytree(v2gen, rev / "instance", symlinks=True)
    if _freeze(v2gen) != _freeze(rev / "instance"):
        mig_emit(ctx, "reverse", "HARNESS_INVALID", None, "待验证",
                 "reverse 副本与源树完整清单不一致——复制不可信")
        return
    if _freeze(v2gen) != _freeze(rev / "instance"):
        mig_emit(ctx, "reverse", "HARNESS_INVALID", None, "待验证",
                 "reverse 副本与快照不一致")
        return
    rc = ctx.serve_start("reverse", ctx.locked["v1"]["exec_path"], 29724,
                         rev / "instance", rev / "instance" / "cwd")
    if rc != 0:
        log = ctx.run_dir / "live" / "serve-reverse.log"
        # F12：与 forward 同一收窄规则——泛词不构成数据类错误证据
        rlog_text = log.read_text(encoding="utf-8", errors="replace") if log.is_file() else ""
        has_data_error = bool(re.search(
            r"(?i)corrupt|invalid data|migration failed|schema version mismatch|"
            r"unsupported (?:data|storage|schema|version)|has no \w+ table|"
            r"(?:database|sqlite).*(?:corrupt|error|failed|mismatch)",
            rlog_text))
        if rc == 1 and has_data_error:
            mig_emit(ctx, "reverse", "VALID_NEGATIVE", [], "待验证",
                     "reverse：v1 打开 v2 原生数据明确报数据类错误（见 serve-reverse.log）；"
                     "仅记录，不作承诺")
        else:
            mig_emit(ctx, "reverse", "STARTED_INCONCLUSIVE", None, "待验证",
                     f"reverse：v1 serve 超时/环境未就绪（rc={rc}）；仅记录，不作承诺")
        return
    # 反向读取（API 级证据）：列表含 rsid + 消息历史可读
    rbase = "http://127.0.0.1:29724"
    rdir = ctx.dir_param(rev / "instance" / "cwd")
    r_rl = ctx.http(f"{rbase}/session?directory={rdir}", timeout=15.0)
    ctx.evidence(f"{ev}/reverse-list.json", r_rl.body)
    if r_rl.status is None:
        mig_emit(ctx, "reverse", "STARTED_INCONCLUSIVE", None, "待验证",
                 "reverse：v1 列表请求传输失败；仅记录，不作承诺")
        ctx.serve_stop("reverse")
        return
    if r_rl.status != 200:
        mig_emit(ctx, "reverse", "VALID_NEGATIVE", [], "待验证",
                 f"reverse：v1 列表明确失败（HTTP {r_rl.status}）；仅记录，不作承诺")
        ctx.serve_stop("reverse")
        return
    rev_ids = _parse_id_list(r_rl)
    list_ok = bool(rev_ids is not None and rsid in rev_ids)
    msg_ok = False
    msg_transport_fail = False
    if list_ok:
        r_rm = ctx.http(f"{rbase}/session/{rsid}/message?directory={rdir}", timeout=15.0)
        ctx.evidence(f"{ev}/reverse-messages.json", r_rm.body)
        if r_rm.status is None:
            msg_transport_fail = True
        elif r_rm.status == 200:
            try:
                rmsgs = json.loads(r_rm.body)
                msg_ok = any(isinstance(o, dict) and (
                    o.get("id") == rmid or (o.get("info") or {}).get("id") == rmid)
                    for o in (rmsgs if isinstance(rmsgs, list) else []))
            except json.JSONDecodeError:
                msg_ok = False
    if list_ok and msg_ok:
        mig_emit(ctx, "reverse", "VALID_PASS", [], "待验证",
                 "reverse：v1 经 API 成功读取 v2 原生 session 与消息历史"
                 "（意外的前向兼容观测）；仅记录，不作承诺")
    elif msg_transport_fail:
        mig_emit(ctx, "reverse", "STARTED_INCONCLUSIVE", None, "待验证",
                 "reverse：消息历史读取传输失败；仅记录，不作承诺")
    else:
        mig_emit(ctx, "reverse", "VALID_NEGATIVE", [], "待验证",
                 f"reverse：v1 未能经 API 完整读取 v2 原生数据"
                 f"（list_ok={list_ok} msg_ok={msg_ok}）；仅记录，不作承诺")
    ctx.serve_stop("reverse")


# ================================================================== 主流程

def main() -> int:
    argv = sys.argv[1:]
    mode = "probes"
    positional: list[str] = []
    i = 0
    while i < len(argv):
        if argv[i] == "--mode":
            if i + 1 >= len(argv) or argv[i + 1] not in ("probes", "migration"):
                print("Usage: live-probe.py <locked-manifest> <evidence-dir> "
                      "[--mode probes|migration]", file=sys.stderr)
                return S.EXIT_PRE_REJECT
            mode = argv[i + 1]
            i += 2
        elif argv[i].startswith("-"):
            print("Usage: live-probe.py <locked-manifest> <evidence-dir> "
                  "[--mode probes|migration]", file=sys.stderr)
            return S.EXIT_PRE_REJECT
        else:
            positional.append(argv[i])
            i += 1
    if len(positional) != 2:
        print("Usage: live-probe.py <locked-manifest> <evidence-dir> "
              "[--mode probes|migration]", file=sys.stderr)
        return S.EXIT_PRE_REJECT
    locked_path, evidence_arg = positional
    if os.environ.get("SPIKE_SELFTEST"):
        S.pre_reject("SPIKE_SELFTEST must not be set in formal runs")

    run_dir = S.run_use_existing()
    if S.canonical(evidence_arg) != S.canonical(run_dir):
        S.pre_reject(f"<evidence-dir> is not the canonical run root: {evidence_arg}")
    sbroot = S.sandbox_root()
    if S.canonical(locked_path) != sbroot / "manifest.json":
        S.pre_reject(f"locked manifest must be the authoritative sandbox file: "
                     f"{sbroot}/manifest.json")
    evidence_copy = run_dir / "manifest.json"
    if not evidence_copy.is_file():
        S.pre_reject("evidence copy of locked manifest missing (prepare unpublished)")
    if Path(locked_path).read_bytes() != evidence_copy.read_bytes():
        S.harness_fail("authoritative manifest and evidence copy diverge")
    locked = S.validate_manifest(Path(locked_path), "locked")

    mech, detail = S.boundary_env_check()
    if mech != locked["boundary_mechanism"] or detail != locked.get("boundary_detail", ""):
        S.pre_reject("boundary description mismatch with locked manifest")
    S.canary_verify(locked["canary_target"], "canary.log", run_dir)

    ctx = Ctx(run_dir, locked)
    S.install_signal_handlers(ctx.registry)

    if mode == "probes":
        run_probes_mode(ctx)
    else:
        # F3：migration 补齐——2/5（前置拒绝/安全越界）原样传播；
        # 其他 SpikeExit（4 harness 故障等）与普通异常一样补齐未完成方向
        mig_exc: Optional[str] = None
        try:
            run_migration_mode(ctx)
        except S.SpikeExit as e:
            if e.code in (S.EXIT_PRE_REJECT, S.EXIT_SAFETY):
                raise
            mig_exc = f"SpikeExit({e.code}): {e}"
        except Exception as e:
            mig_exc = f"{type(e).__name__}: {e}"
        if mig_exc is not None:
            ctx.any_harness = True
            print(S.redact_text(f"harness error in migration: {mig_exc}"), file=sys.stderr)
            for which in ("forward", "reverse"):
                pid = f"migration.{which}"
                if pid not in ctx.completed:
                    ctx.emit_harness(pid, f"migration 中断补齐：{mig_exc}")
        # F4：migration 同样消费泵错误（先清残留进程再 join）
        ctx.registry.cleanup_all()
        S.pump_join_all()
        if S.pump_still_alive() or S.pump_errors():
            ctx.any_harness = True
            n = S.invalidate_matching_results(
                ctx.run_dir, ctx.locked,
                lambda obj: str(obj.get("probe_id", "")).startswith("migration."),
                " [harness: log pump failure; migration evidence stream may be incomplete]")
            print(S.redact_text(
                f"harness error: log pump failure, invalidated {n} migration records"),
                  file=sys.stderr)

    if ctx.any_harness:
        return S.EXIT_HARNESS
    if ctx.any_fail:
        return S.EXIT_FAIL
    if ctx.any_inconclusive:
        return S.EXIT_INCONCLUSIVE
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
