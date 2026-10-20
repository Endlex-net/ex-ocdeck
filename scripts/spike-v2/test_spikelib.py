#!/usr/bin/env python3
"""test_spikelib.py — spikelib 单元测试（unittest，stdlib only）。

用法: python3 -m unittest discover -s scripts/spike-v2 -p 'test_*.py' -v
"""
from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path
from typing import Any
from unittest import mock

import spikelib as S

B64 = "a" * 64
C40 = "b" * 40
RUN_ID = "20261012T000000Z-deadbeef"


def make_candidate() -> dict:
    side_v1 = {
        "side": "v1", "version": "1.18.26",
        "release_url": "https://github.com/anomalyco/opencode/releases/tag/v1.18.26",
        "tag": "v1.18.26", "commit_sha": C40, "asset_name": "opencode-darwin-arm64.zip",
        "os": "darwin", "arch": "arm64",
        "download_url": "https://github.com/anomalyco/opencode/releases/download/v1.18.26/x.zip",
        "official_sha256": B64, "source_chain_status": "verified",
    }
    side_v2 = dict(side_v1)
    side_v2.update({"side": "v2", "version": "2.0.23", "tag": "v2.0.23"})
    return {"schema_version": 1, "v1": side_v1, "v2": side_v2}


def make_locked() -> dict:
    m = make_candidate()
    m["canary_target"] = "/tmp/canary"
    m["boundary_mechanism"] = "vm"
    for side in ("v1", "v2"):
        m[side].update({
            "locked_at": "2026-10-12T00:00:00Z",
            "download_sha256": B64,
            "sha256_match": True,
            "exec_path": f"/sb/{side}/bin/opencode",
            "runtime_version_output": m[side]["version"],
        })
    return m


class SpikeTestCase(unittest.TestCase):
    """公共 fixture：临时伪仓库 + run 环境。"""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        (self.tmp / S.SPIKE_CHANGE_REL).mkdir(parents=True)
        self.env_patch = mock.patch.dict(os.environ, {
            "SPIKE_SELFTEST": "1",
            "SPIKE_REPO_ROOT": str(self.tmp),
            "SPIKE_RUN_ID": RUN_ID,
            "SPIKE_BOUNDARY_MECHANISM": "vm",
        })
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        import shutil
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def write_manifest(self, doc: dict, name: str = "m.json") -> Path:
        p = self.tmp / name
        p.write_text(json.dumps(doc))
        return p


class TestManifest(SpikeTestCase):
    def test_candidate_valid(self):
        m = S.validate_manifest(self.write_manifest(make_candidate()), "candidate")
        self.assertEqual(m["v1"]["version"], "1.18.26")

    def test_locked_valid(self):
        S.validate_manifest(self.write_manifest(make_locked()), "locked")

    def test_candidate_bool_schema_version_rejected(self):
        m = make_candidate()
        m["schema_version"] = True
        with self.assertRaises(S.SpikeExit) as cm:
            S.validate_manifest(self.write_manifest(m), "candidate")
        self.assertEqual(cm.exception.code, 2)

    def test_candidate_float_schema_version_accepted(self):
        # JSON Schema const:1 数值相等语义：1.0 合法
        m = make_candidate()
        m["schema_version"] = 1.0
        S.validate_manifest(self.write_manifest(m), "candidate")

    def test_bad_uri_rejected(self):
        for bad in ("https://bad host", "https://example.com/%zz",
                    "https://example.com/a b", "http://insecure.com/x"):
            m = make_candidate()
            m["v1"]["release_url"] = bad
            with self.assertRaises(S.SpikeExit, msg=bad):
                S.validate_manifest(self.write_manifest(m), "candidate")

    def test_v2_major_enforced(self):
        m = make_candidate()
        m["v2"]["version"] = "1.99.0"
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "candidate")

    def test_side_key_consistency(self):
        m = make_candidate()
        m["v1"]["side"] = "v2"
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "candidate")

    def test_candidate_forbids_boundary_fields(self):
        m = make_candidate()
        m["boundary_mechanism"] = "vm"
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "candidate")

    def test_locked_requires_locked_fields(self):
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(make_candidate()), "locked")

    def test_locked_other_requires_detail(self):
        m = make_locked()
        m["boundary_mechanism"] = "other"
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "locked")

    def test_locked_unverified_rejected(self):
        m = make_locked()
        m["v2"]["source_chain_status"] = "unverified"
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "locked")

    def test_locked_sha_mismatch_rejected(self):
        m = make_locked()
        m["v2"]["download_sha256"] = "c" * 64
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "locked")

    def test_locked_runtime_version_mismatch_exit4(self):
        m = make_locked()
        m["v2"]["runtime_version_output"] = "9.9.9"
        with self.assertRaises(S.SpikeExit) as cm:
            S.validate_manifest(self.write_manifest(m), "locked")
        self.assertEqual(cm.exception.code, 4)

    def test_unknown_field_rejected(self):
        m = make_candidate()
        m["v1"]["surprise"] = 1
        with self.assertRaises(S.SpikeExit):
            S.validate_manifest(self.write_manifest(m), "candidate")


class TestRunIdAndDirs(SpikeTestCase):
    def test_run_id_valid(self):
        self.assertEqual(S.run_id(), RUN_ID)

    def test_run_id_invalid(self):
        with mock.patch.dict(os.environ, {"SPIKE_RUN_ID": "bad"}):
            with self.assertRaises(S.SpikeExit) as cm:
                S.run_id()
            self.assertEqual(cm.exception.code, 2)

    def test_run_init_new_then_reject_dup(self):
        d = S.run_init_new()
        self.assertTrue((d / "results.jsonl").is_file())
        self.assertTrue((d / "source-chain").is_dir())
        with self.assertRaises(S.SpikeExit) as cm:
            S.run_init_new()
        self.assertEqual(cm.exception.code, 2)

    def test_run_use_existing_reuses(self):
        d1 = S.run_init_new()
        d2 = S.run_use_existing()
        self.assertEqual(d1, d2)

    def test_symlink_escape_rejected(self):
        d = S.run_init_new()
        outside = self.tmp / "outside"
        outside.mkdir()
        (d / "escape").symlink_to(outside)
        with self.assertRaises(S.SpikeExit):
            S.evidence_write(d, "escape/x.json", "{}")


class TestResultSchema(SpikeTestCase):
    def setUp(self):
        super().setUp()
        self.run_dir = S.run_init_new()
        self.locked = make_locked()

    def emit_ok(self, **kw: Any):
        base: dict[str, Any] = dict(run_dir=self.run_dir, locked=self.locked,
                    probe_id="session.01", exec_status="PASS", diff_class="兼容",
                    capability_verdict="可满足", evidence_validity="valid",
                    reason="t", evidence_path="live/x.json")
        base.update(kw)
        S.emit_result(**base)

    def test_valid_live(self):
        self.emit_ok()
        lines = (self.run_dir / "results.jsonl").read_text().splitlines()
        self.assertEqual(len(lines), 1)

    def test_valid_static(self):
        self.emit_ok(probe_id="static.01", capability_verdict="不适用")

    def test_valid_exempt(self):
        self.emit_ok(probe_id="spi.exempt.provider.meta", diff_class="不适用",
                     capability_verdict="不适用")

    def test_valid_migration(self):
        self.emit_ok(probe_id="migration.forward", exec_status="FAIL",
                     diff_class="不兼容", capability_verdict="不可满足",
                     failure_classes=["READABILITY_FAILURE"])

    def test_valid_invalidated_record(self):
        # D8 第 0 步无效化记录：保留原 exec_status 但 validity!=valid，豁免映射一致性
        self.emit_ok(exec_status="INCONCLUSIVE", capability_verdict="待验证",
                     diff_class="待验证", evidence_validity="harness_invalid")

    def test_unknown_probe_id(self):
        with self.assertRaises(S.SpikeExit) as cm:
            self.emit_ok(probe_id="bogus.99")
        self.assertEqual(cm.exception.code, 4)

    def test_run_id_mismatch(self):
        with mock.patch.dict(os.environ, {"SPIKE_RUN_ID": "19990101T000000Z-00000000"}):
            with self.assertRaises(S.SpikeExit):
                self.emit_ok()

    def test_fail_with_satisfiable_rejected(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(exec_status="FAIL", capability_verdict="可满足",
                         diff_class="不兼容")

    def test_static_with_verdict_rejected(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(probe_id="static.01", capability_verdict="可满足")

    def test_equivalent_on_compatible_rejected(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(equivalent="live/x.json")

    def test_equivalent_missing_file_rejected(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(diff_class="不兼容", equivalent="live/no-such.json")

    def test_equivalent_valid(self):
        (self.run_dir / "live").mkdir(exist_ok=True)
        (self.run_dir / "live" / "eq.json").write_text("{}")
        self.emit_ok(diff_class="不兼容", equivalent="live/eq.json")

    def test_non_migration_with_failure_classes_rejected(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(failure_classes=[])

    def test_failure_classes_order_enforced(self):
        with self.assertRaises(S.SpikeExit):
            self.emit_ok(probe_id="migration.forward", exec_status="FAIL",
                         diff_class="不兼容", capability_verdict="不可满足",
                         failure_classes=["CONTINUITY_FAILURE", "READABILITY_FAILURE"])

    def test_redaction_reason_only(self):
        with mock.patch.dict(os.environ, {"SPIKE_TEST_TOKEN": "s3cr3t"}):
            self.emit_ok(reason="token s3cr3t leaked")
        line = (self.run_dir / "results.jsonl").read_text().splitlines()[-1]
        doc = json.loads(line)
        self.assertNotIn("s3cr3t", doc["reason"])
        self.assertEqual(doc["run_id"], RUN_ID)

    def test_short_secret_identity_preserved(self):
        with mock.patch.dict(os.environ, {"SPIKE_TEST_TOKEN": "1"}):
            self.emit_ok(reason="short token 1 here")
        line = (self.run_dir / "results.jsonl").read_text().splitlines()[-1]
        doc = json.loads(line)
        self.assertEqual(doc["run_id"], RUN_ID)
        self.assertEqual(doc["probe_id"], "session.01")


class TestCanary(SpikeTestCase):
    def setUp(self):
        super().setUp()
        self.run_dir = S.run_init_new()
        self.host = self.tmp / "host"
        self.host.mkdir()

    def test_missing_proof_rejected(self):
        with self.assertRaises(S.SpikeExit) as cm:
            S.canary_verify(str(self.host / "sentinel"), "canary.log", self.run_dir)
        self.assertEqual(cm.exception.code, 2)
        self.assertFalse((self.host / "sentinel").exists())

    def test_missing_target_rejected(self):
        with self.assertRaises(S.SpikeExit):
            S.canary_verify("", "canary.log", self.run_dir)

    def test_missing_parent_invalid_challenge(self):
        with mock.patch.dict(os.environ, {"SPIKE_CANARY_PERMISSION_PROOF": "proof"}):
            with self.assertRaises(S.SpikeExit) as cm:
                S.canary_verify(str(self.tmp / "no-such-dir" / "s"), "canary.log", self.run_dir)
            self.assertEqual(cm.exception.code, 2)

    def test_target_in_allowed_set_rejected(self):
        with mock.patch.dict(os.environ, {"SPIKE_CANARY_PERMISSION_PROOF": "proof"}):
            with self.assertRaises(S.SpikeExit):
                S.canary_verify(str(self.run_dir / "inside"), "canary.log", self.run_dir)

    def test_target_exists_rejected(self):
        existing = self.host / "existing"
        existing.mkdir()
        with mock.patch.dict(os.environ, {"SPIKE_CANARY_PERMISSION_PROOF": "proof"}):
            with self.assertRaises(S.SpikeExit):
                S.canary_verify(str(existing), "canary.log", self.run_dir)

    def test_unprotected_creation_is_pre_reject_with_residual(self):
        # 无真实边界：mkdir 成功 → 意外创建分支 → 2 + 保留现场 + RESIDUAL 记录
        target = self.host / f"sentinel-{RUN_ID}"
        with mock.patch.dict(os.environ, {"SPIKE_CANARY_PERMISSION_PROOF": "proof"}):
            with self.assertRaises(S.SpikeExit) as cm:
                S.canary_verify(str(target), "canary.log", self.run_dir)
            self.assertEqual(cm.exception.code, 2)
        self.assertTrue(target.is_dir())  # 保留现场，不删除
        log = (self.run_dir / "canary.log").read_text()
        self.assertIn("BOUNDARY-INEFFECTIVE", log)
        self.assertIn("RESIDUAL:", log)
        self.assertIn("caller-proof: proof", log)

    def test_eacces_with_proof_is_valid_boundary(self):
        # 模拟边界拒绝（EPERM/EACCES）：chmod 0555 父目录
        self.host.chmod(0o555)
        self.addCleanup(self.host.chmod, 0o755)
        with mock.patch.dict(os.environ, {"SPIKE_CANARY_PERMISSION_PROOF": "proof"}):
            S.canary_verify(str(self.host / "sentinel-ok"), "canary.log", self.run_dir)
        log = (self.run_dir / "canary.log").read_text()
        self.assertIn("REJECTED as expected", log)


class TestAggregationAndDeps(SpikeTestCase):
    def test_phase_priority(self):
        self.assertEqual(S.phase_priority_max(0, 1), 1)
        self.assertEqual(S.phase_priority_max(1, 3), 1)
        self.assertEqual(S.phase_priority_max(3, 1), 1)
        self.assertEqual(S.phase_priority_max(1, 4), 4)
        self.assertEqual(S.phase_priority_max(4, 3), 4)
        self.assertEqual(S.phase_priority_max(0, 9), 4)  # 未知码 → harness

    def _full_record(self, pid: str, exec_status: str = "PASS",
                     validity: str = "valid") -> dict:
        verdict = {"PASS": "可满足", "FAIL": "不可满足",
                   "INCONCLUSIVE": "待验证"}[exec_status]
        return {
            "run_id": RUN_ID, "probe_id": pid,
            "v1_baseline": {"version": "1.18.26", "commit_sha": C40},
            "v2_baseline": {"version": "2.0.23", "commit_sha": C40},
            "exec_status": exec_status, "diff_class": "兼容",
            "capability_verdict": verdict,
            "evidence_validity": validity,
            "equivalent": "", "reason": "t", "evidence_path": "live/x.json",
        }

    def _write_results(self, records: list[dict]) -> Path:
        d = S.run_init_new()
        f = d / "results.jsonl"
        f.write_text("\n".join(json.dumps(r) for r in records) + "\n")
        return f

    def test_deps_all_satisfied(self):
        f = self._write_results([self._full_record(p) for p in S.MIGRATION_DEPS])
        self.assertIsNone(S.migration_deps_satisfied(f))

    def test_deps_fail_counts_as_fact(self):
        # FAIL 表示协议事实已获得（能力断言失败），仍满足依赖谓词
        f = self._write_results([
            self._full_record(p, exec_status="FAIL") for p in S.MIGRATION_DEPS])
        self.assertIsNone(S.migration_deps_satisfied(f))

    def test_deps_inconclusive_missing(self):
        recs = [self._full_record(p) for p in S.MIGRATION_DEPS]
        recs[-1] = self._full_record("prompt.01", exec_status="INCONCLUSIVE",
                                     validity="valid")
        f = self._write_results(recs)
        missing = S.migration_deps_satisfied(f)
        self.assertEqual(missing, ["prompt.01"])

    def test_deps_missing_file(self):
        missing = S.migration_deps_satisfied(self.tmp / "no-such.jsonl")
        self.assertIsNotNone(missing)

    def test_deps_malformed_jsonl(self):
        d = S.run_init_new()
        f = d / "results.jsonl"
        f.write_text('{"broken json\n')
        with self.assertRaises(S.SpikeExit) as cm:
            S.migration_deps_satisfied(f)
        self.assertEqual(cm.exception.code, 4)

    def test_deps_non_object_record(self):
        d = S.run_init_new()
        f = d / "results.jsonl"
        f.write_text('["not","an","object"]\n')
        with self.assertRaises(S.SpikeExit):
            S.migration_deps_satisfied(f)


class TestSSE(SpikeTestCase):
    def test_basic_frame(self):
        evs = S.sse_parse_line_events(['data: {"type":"session.created","properties":{"sessionID":"s1"}}', ""])
        self.assertEqual(len(evs), 1)
        self.assertEqual(evs[0]["type"], "session.created")

    def test_multiline_data(self):
        evs = S.sse_parse_line_events([
            'data: {"type":"a",', 'data: "x":1}', ""])
        self.assertEqual(len(evs), 1)

    def test_comment_and_crlf(self):
        evs = S.sse_parse_line_events([": keep-alive", 'data: {"type":"x"}\r', ""])
        self.assertEqual(len(evs), 1)

    def test_non_json_data_skipped(self):
        evs = S.sse_parse_line_events(['data: not-json', "", 'data: {"type":"y"}', ""])
        self.assertEqual(len(evs), 1)
        self.assertEqual(evs[0]["type"], "y")

    def test_event_matches(self):
        ev = {"type": "session.created", "properties": {"sessionID": "s1", "info": {"id": "s1"}}}
        self.assertTrue(S.event_matches(ev, ("session.created",), "s1", "id"))
        self.assertFalse(S.event_matches(ev, ("session.updated",), "s1", None))
        self.assertFalse(S.event_matches(ev, ("session.created",), "s2", None))
        self.assertFalse(S.event_matches(ev, ("session.created",), "s1", "missing_key"))
        # F8：文本出现 "status" 不等于真实 status 字段
        note = {"type": "session.status",
                "properties": {"sessionID": "s1", "note": "status"}}
        self.assertFalse(S.event_matches(note, ("session.status",), "s1",
                                         "status", payload_ok=S.status_detail_ok))
        ok = {"type": "session.status",
              "properties": {"sessionID": "s1", "status": "busy"}}
        self.assertTrue(S.event_matches(ok, ("session.status",), "s1",
                                        "status", payload_ok=S.status_detail_ok))


class TestRedaction(unittest.TestCase):
    def test_long_secret_redacted(self):
        with mock.patch.dict(os.environ, {"MY_API_TOKEN": "supersecretvalue"}):
            self.assertNotIn("supersecretvalue", S.redact_text("token is supersecretvalue ok"))

    def test_short_secret_token_boundary(self):
        with mock.patch.dict(os.environ, {"MY_TOKEN": "ab"}):
            self.assertEqual(S.redact_text("ab"), "***REDACTED***")
            # 不破坏包含它的更长标识符
            self.assertEqual(S.redact_text("abc"), "abc")


class TestGoNoGoOrdering(SpikeTestCase):
    """F18：安全门禁不得被坏证据遮蔽（run-all.compute_go_no_go）。"""

    def _load_run_all(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "run_all", str(Path(__file__).parent / "run-all.py"))
        assert spec is not None and spec.loader is not None
        ra = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(ra)
        return ra

    def _run_dir_with_results(self, lines: list[str]) -> Path:
        d = S.run_init_new()
        (d / "results.jsonl").write_text("\n".join(lines) + ("\n" if lines else ""))
        return d

    def test_safety_code_beats_bad_json(self):
        ra = self._load_run_all()
        d = self._run_dir_with_results(['{"broken'])
        self.assertEqual(ra.compute_go_no_go(d, [S.EXIT_SAFETY]), "NO-GO(安全越界)")

    def test_safety_invalid_record_beats_dup(self):
        ra = self._load_run_all()
        rec = json.dumps({
            "run_id": RUN_ID, "probe_id": "migration.reverse",
            "v1_baseline": {"version": "1.18.26", "commit_sha": C40},
            "v2_baseline": {"version": "2.0.23", "commit_sha": C40},
            "exec_status": "INCONCLUSIVE", "diff_class": "待验证",
            "capability_verdict": "待验证", "evidence_validity": "safety_invalid",
            "equivalent": "", "reason": "t", "evidence_path": "migration/r.txt",
            "failure_classes": []})
        d = self._run_dir_with_results([rec])
        self.assertEqual(ra.compute_go_no_go(d, []), "NO-GO(安全越界)")

    def test_bad_json_gives_inconclusive_pending(self):
        ra = self._load_run_all()
        d = self._run_dir_with_results(['{"broken'])
        self.assertIn("待补证据", ra.compute_go_no_go(d, []))


class TestProbeIsolation(SpikeTestCase):
    """F19：probe 隔离不得吞掉退出码 2/5。"""

    def _load_live_probe(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "live_probe", str(Path(__file__).parent / "live-probe.py"))
        assert spec is not None and spec.loader is not None
        lp = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(lp)
        return lp

    def test_spike_exit_2_propagates_and_stops_following(self):
        lp = self._load_live_probe()
        ctx = mock.Mock()
        ctx.any_harness = False
        calls: list[str] = []

        def failing(_c):
            raise S.SpikeExit(S.EXIT_PRE_REJECT, "pre")

        def following(_c):
            calls.append("ran")

        with mock.patch.object(lp, "probe_supervisor", failing), \
                mock.patch.object(lp, "probe_sessions", following), \
                mock.patch.object(lp, "probe_events", following), \
                mock.patch.object(lp, "probe_attention", following), \
                mock.patch.object(lp, "probe_prompt", following), \
                mock.patch.object(lp, "probe_status_capability_title", following), \
                mock.patch.object(lp, "probe_terminal_conn_recovery", following), \
                mock.patch.object(lp, "probe_destructive", following), \
                mock.patch.object(lp, "emit_exemptions", following):
            with self.assertRaises(S.SpikeExit) as cm:
                lp.run_probes_mode(ctx)
        self.assertEqual(cm.exception.code, S.EXIT_PRE_REJECT)
        self.assertEqual(calls, [])   # 2/5 之后不得执行后续组

    def test_generic_exception_isolated_and_continues(self):
        lp = self._load_live_probe()
        ctx = mock.Mock()
        ctx.any_harness = False
        calls: list[str] = []

        def failing(_c):
            raise RuntimeError("boom")

        def following(_c):
            calls.append("ran")

        with mock.patch.object(lp, "probe_supervisor", failing), \
                mock.patch.object(lp, "probe_sessions", following), \
                mock.patch.object(lp, "probe_events", following), \
                mock.patch.object(lp, "probe_attention", following), \
                mock.patch.object(lp, "probe_prompt", following), \
                mock.patch.object(lp, "probe_status_capability_title", following), \
                mock.patch.object(lp, "probe_terminal_conn_recovery", following), \
                mock.patch.object(lp, "probe_destructive", following), \
                mock.patch.object(lp, "emit_exemptions", following):
            lp.run_probes_mode(ctx)
        self.assertTrue(ctx.any_harness)
        self.assertEqual(len(calls), 8)   # 普通异常隔离后继续独立项


class TestRegistryRemove(SpikeTestCase):
    """F40：同步进程结束后必须从 registry 移除，避免 cleanup 命中复用 PID。"""

    def test_remove_drops_pid(self):
        d = S.run_init_new()
        reg = S.ProcessRegistry(d)
        reg.add("item", 4242)
        self.assertIn("4242", (d / ".item.pids").read_text())
        reg.remove("item", 4242)
        self.assertEqual((d / ".item.pids").read_text(), "")

    def test_run_upstream_removes_pid_on_success(self):
        d = S.run_init_new()
        reg = S.ProcessRegistry(d)
        sb = S.sandbox_root()
        S.sandbox_mkdir(sb / "work")
        cp = S.run_upstream(sb, sb / "work", ["/usr/bin/true"],
                            timeout=5.0, registry=reg)
        self.assertEqual(cp.returncode, 0)
        leftover = (d / ".item.pids").read_text() if (d / ".item.pids").exists() else ""
        self.assertEqual(leftover.strip(), "")

    def test_remove_failure_does_not_return_success(self):
        # F41：remove 失败不得吞掉并返回成功
        d = S.run_init_new()
        reg = S.ProcessRegistry(d)
        sb = S.sandbox_root()
        S.sandbox_mkdir(sb / "work")
        with mock.patch.object(S.ProcessRegistry, "remove",
                               side_effect=S.SpikeExit(S.EXIT_HARNESS, "remove boom")):
            with self.assertRaises(S.SpikeExit) as cm:
                S.run_upstream(sb, sb / "work", ["/usr/bin/true"],
                               timeout=5.0, registry=reg)
        self.assertEqual(cm.exception.code, S.EXIT_HARNESS)

    def test_add_failure_kills_and_attempts_remove(self):
        # F41：add 失败后杀进程并尝试移除可能已写入的 PID
        d = S.run_init_new()
        reg = S.ProcessRegistry(d)
        sb = S.sandbox_root()
        S.sandbox_mkdir(sb / "work")
        removed: list[int] = []

        def boom_add(_self, _layer, pid):
            raise S.SpikeExit(S.EXIT_HARNESS, "add boom")

        def rec_remove(_self, _layer, pid):
            removed.append(pid)

        with mock.patch.object(S.ProcessRegistry, "add", boom_add), \
                mock.patch.object(S.ProcessRegistry, "remove", rec_remove), \
                mock.patch.object(S, "kill_tree") as kt:
            with self.assertRaises(S.SpikeExit) as cm:
                S.run_upstream(sb, sb / "work", ["/usr/bin/true"],
                               timeout=5.0, registry=reg)
        self.assertEqual(cm.exception.code, S.EXIT_HARNESS)
        self.assertTrue(kt.called)
        self.assertEqual(len(removed), 1)


class TestMigEmitReraise(SpikeTestCase):
    """F39：migration 结果落盘失败必须抛出，让外层补齐未完成方向。"""

    def test_mig_emit_write_failure_raises_harness(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "live_probe", str(Path(__file__).parent / "live-probe.py"))
        assert spec is not None and spec.loader is not None
        lp = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(lp)
        d = S.run_init_new()
        S.evidence_layout_init(d)
        locked = make_locked()
        ctx = lp.Ctx(d, locked)
        with mock.patch.object(S, "emit_result",
                               side_effect=S.SpikeExit(S.EXIT_HARNESS, "write boom")):
            with self.assertRaises(S.SpikeExit) as cm:
                lp.mig_emit(ctx, "forward", "VALID_PASS", [], "待验证", "ok")
        self.assertEqual(cm.exception.code, S.EXIT_HARNESS)
        self.assertNotIn("migration.forward", ctx.completed)

    def test_migration_outer_backfill_exactly_once(self):
        """集成式模拟：forward 结果落盘失败 → 外层收尾后 forward/reverse
        各恰好一条 harness_invalid（HARNESS_FAILURE）。"""
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "live_probe", str(Path(__file__).parent / "live-probe.py"))
        assert spec is not None and spec.loader is not None
        lp = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(lp)
        d = S.run_init_new()
        S.evidence_layout_init(d)
        locked = make_locked()
        ctx = lp.Ctx(d, locked)

        calls: list[str] = []
        real_emit = S.emit_result
        failed = {"done": False}

        def flaky_emit(run_dir, lk, probe_id, *a, **kw):
            calls.append(probe_id)
            if probe_id == "migration.forward" and not failed["done"]:
                failed["done"] = True
                raise S.SpikeExit(S.EXIT_HARNESS, "write boom")
            return real_emit(run_dir, lk, probe_id, *a, **kw)

        with mock.patch.object(S, "emit_result", flaky_emit):
            with self.assertRaises(S.SpikeExit):
                lp.mig_emit(ctx, "forward", "VALID_PASS", [], "待验证", "ok")
            # 外层收尾逻辑：为未完成方向补齐
            for which in ("forward", "reverse"):
                pid = f"migration.{which}"
                if pid not in ctx.completed:
                    ctx.emit_harness(pid, "migration 中断补齐：write boom")

        recs = {}
        for line in (d / "results.jsonl").read_text().splitlines():
            obj = json.loads(line)
            recs.setdefault(obj["probe_id"], []).append(obj)
        self.assertEqual(len(recs["migration.forward"]), 1)
        self.assertEqual(len(recs["migration.reverse"]), 1)
        for pid in ("migration.forward", "migration.reverse"):
            r = recs[pid][0]
            self.assertEqual(r["evidence_validity"], "harness_invalid")
            self.assertEqual(r["failure_classes"], ["HARNESS_FAILURE"])


class TestKillTreeSelfGroup(SpikeTestCase):
    """F42：kill_tree 的进程组信号必须排除自身进程组——root 为自身 pid 或自身
    pgid 时不得 killpg（否则 TERM/KILL 波及本进程）。"""

    def _run_kill_tree(self, root_pid, descendants):
        sent: list[tuple[str, int, int]] = []
        with mock.patch.object(S, "_descendants", return_value=descendants), \
             mock.patch.object(S, "_alive", return_value=False), \
             mock.patch.object(S.os, "killpg",
                               side_effect=lambda pg, sig: sent.append(("killpg", pg, sig))), \
             mock.patch.object(S.os, "kill",
                               side_effect=lambda p, sig: sent.append(("kill", p, sig))), \
             mock.patch.object(S.os, "waitpid", side_effect=ChildProcessError):
            S.kill_tree(root_pid, grace=0.1)
        return sent

    def test_self_root_no_descendants_no_group_signal(self):
        me = os.getpid()
        sent = self._run_kill_tree(me, [])
        self.assertFalse(any(kind == "killpg" for kind, _, _ in sent),
                         f"自身根不得发送进程组信号: {sent}")
        self.assertFalse(any(kind == "kill" and p == me for kind, p, _ in sent))

    def test_self_root_with_descendants_kills_pids_only(self):
        me = os.getpid()
        sent = self._run_kill_tree(me, [900001, 900002])
        self.assertFalse(any(kind == "killpg" for kind, _, _ in sent),
                         f"自身根有后代时仍不得 killpg 自身组: {sent}")
        killed = sorted(p for kind, p, _ in sent if kind == "kill")
        self.assertEqual(killed, sorted([900001, 900002] * 2))  # TERM 一轮 + KILL 一轮

    def test_foreign_root_still_uses_group_signal(self):
        sent = self._run_kill_tree(950000, [950001])
        groups = [pg for kind, pg, _ in sent if kind == "killpg"]
        self.assertEqual(groups, [950000, 950000])  # TERM + KILL 兜底各一次


if __name__ == "__main__":
    unittest.main()
