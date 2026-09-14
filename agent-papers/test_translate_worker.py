#!/usr/bin/env python3
"""Unit tests for BabelDOC quality defaults in translate_worker.py."""
from __future__ import annotations

import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import translate_worker as tw


def _ok_proc(stdout: str = "") -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(args=[], returncode=0, stdout=stdout, stderr="")


class QualitySettingsTests(unittest.TestCase):
    def test_defaults_enable_quality_flags(self) -> None:
        q = tw.resolve_quality_settings(None, environ={})
        self.assertTrue(q.disable_same_text_fallback)
        self.assertTrue(q.translate_table_text)
        self.assertEqual(q.min_text_length, 3)
        self.assertFalse(q.enhance_compatibility)
        self.assertTrue(q.preflight)
        self.assertTrue(q.completeness)
        self.assertTrue(q.ignore_cache_retry)
        self.assertIn("Translate ALL human-readable prose", q.custom_system_prompt)
        self.assertIn("zh-CN", q.custom_system_prompt)

    def test_env_opt_out(self) -> None:
        q = tw.resolve_quality_settings(
            None,
            environ={
                "PAPERS_TRANSLATE_DISABLE_SAME_TEXT_FALLBACK": "0",
                "PAPERS_TRANSLATE_TABLE_TEXT": "false",
                "PAPERS_TRANSLATE_MIN_TEXT_LENGTH": "0",
                "PAPERS_TRANSLATE_ENHANCE_COMPATIBILITY": "1",
                "PAPERS_TRANSLATE_PREFLIGHT": "off",
                "PAPERS_TRANSLATE_COMPLETENESS": "no",
                "PAPERS_TRANSLATE_IGNORE_CACHE_RETRY": "0",
                "PAPERS_TRANSLATE_SYSTEM_PROMPT": "short prompt",
            },
        )
        self.assertFalse(q.disable_same_text_fallback)
        self.assertFalse(q.translate_table_text)
        self.assertIsNone(q.min_text_length)
        self.assertTrue(q.enhance_compatibility)
        self.assertFalse(q.preflight)
        self.assertFalse(q.completeness)
        self.assertFalse(q.ignore_cache_retry)
        self.assertEqual(q.custom_system_prompt, "short prompt")

    def test_quality_json_then_env_then_cli(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / tw.QUALITY_CONFIG_NAME).write_text(
                json.dumps({"min_text_length": 8, "translate_table_text": False}),
                encoding="utf-8",
            )
            q = tw.resolve_quality_settings(
                root,
                environ={"PAPERS_TRANSLATE_MIN_TEXT_LENGTH": "4"},
            )
            self.assertEqual(q.min_text_length, 4)
            self.assertFalse(q.translate_table_text)

            ns = argparse_ns(min_text_length=6, translate_table_text=True)
            q2 = tw.resolve_quality_settings(
                root,
                ns,
                environ={"PAPERS_TRANSLATE_MIN_TEXT_LENGTH": "4"},
            )
            self.assertEqual(q2.min_text_length, 6)
            self.assertTrue(q2.translate_table_text)

    def test_auto_glossary_if_present(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            g = root / "glossary.csv"
            g.write_text("source,target\nagent,智能体\n", encoding="utf-8")
            q = tw.resolve_quality_settings(root, environ={})
            self.assertEqual(q.glossary_files, str(g.resolve()))


def argparse_ns(**kwargs):
    class NS:
        disable_same_text_fallback = None
        translate_table_text = None
        enhance_compatibility = None
        preflight = None
        completeness = None
        ignore_cache_retry = None
        min_text_length = None
        custom_system_prompt = None
        glossary_files = None
        pause_seconds = None

    for k, v in kwargs.items():
        setattr(NS, k, v)
    return NS()


class BuildCmdTests(unittest.TestCase):
    def test_default_cmd_has_quality_flags_and_no_key(self) -> None:
        q = tw.QualitySettings()
        cmd = tw.build_babeldoc_cmd(
            "babeldoc",
            Path("/tmp/in.pdf"),
            Path("/tmp/work"),
            model="demo",
            base_url="https://example.test/v1",
            qps=4,
            lang_in="en",
            lang_out="zh-CN",
            quality=q,
        )
        self.assertIn("--disable-same-text-fallback", cmd)
        self.assertIn("--translate-table-text", cmd)
        self.assertIn("--min-text-length", cmd)
        self.assertEqual(cmd[cmd.index("--min-text-length") + 1], "3")
        self.assertIn("--custom-system-prompt", cmd)
        self.assertNotIn("--enhance-compatibility", cmd)
        self.assertNotIn("--ignore-cache", cmd)
        self.assertFalse(tw.argv_has_secret(cmd))
        joined = " ".join(cmd)
        self.assertNotIn("sk-", joined)
        self.assertNotIn("api_key", joined.lower().replace("-", "_"))

    def test_opt_out_and_ignore_cache(self) -> None:
        q = tw.QualitySettings(
            disable_same_text_fallback=False,
            translate_table_text=False,
            min_text_length=None,
            custom_system_prompt="",
            enhance_compatibility=True,
        )
        cmd = tw.build_babeldoc_cmd(
            "babeldoc",
            Path("in.pdf"),
            Path("work"),
            model="m",
            base_url="",
            qps=0,
            lang_in="en",
            lang_out="zh-CN",
            quality=q,
            ignore_cache=True,
        )
        self.assertNotIn("--disable-same-text-fallback", cmd)
        self.assertNotIn("--translate-table-text", cmd)
        self.assertNotIn("--min-text-length", cmd)
        self.assertNotIn("--custom-system-prompt", cmd)
        self.assertIn("--enhance-compatibility", cmd)
        self.assertIn("--ignore-cache", cmd)

    def test_strip_unrecognized_flags(self) -> None:
        argv = [
            "babeldoc",
            "--openai",
            "--disable-same-text-fallback",
            "--custom-system-prompt",
            "hello",
            "--min-text-length",
            "3",
            "--watermark-output-mode=no_watermark",
        ]
        stripped = tw.strip_optional_flags(
            argv, ["--custom-system-prompt", "--watermark-output-mode"]
        )
        self.assertIn("--disable-same-text-fallback", stripped)
        self.assertNotIn("hello", stripped)
        self.assertTrue(all(not a.startswith("--watermark") for a in stripped))
        self.assertIn("--min-text-length", stripped)


class CompletenessTests(unittest.TestCase):
    def test_majority_english_fails(self) -> None:
        paras = [
            "This paper presents a novel framework for autonomous agents. " * 4,
            "We evaluate the method on several security benchmarks. " * 4,
            "Results show consistent improvement over prior work. " * 4,
            "The abstract remains entirely in English throughout. " * 4,
        ]
        report = tw.analyze_zh_text("\n\n".join(paras))
        self.assertFalse(report.ok)
        self.assertIn("majority English", report.reason)

    def test_mixed_residual_english_fails(self) -> None:
        zh = (
            "本文提出一种面向智能体安全的自进化方法，并在多个基准上验证有效性。"
            "章节还讨论了提示注入、工具调用与记忆模块的设计取舍。"
        ) * 4
        en = "However the evaluation protocol and the main experimental analysis stay English. " * 5
        text = "\n\n".join([zh, zh, zh, en, en, en, zh])
        report = tw.analyze_zh_text(text)
        self.assertFalse(report.ok)
        self.assertIn("residual English", report.reason)
        self.assertGreaterEqual(report.cjk_ratio, 0.20)

    def test_translated_body_passes(self) -> None:
        zh = (
            "本文研究大型语言模型智能体的自我改进与安全对齐问题。"
            "我们提出一种可验证的训练流程，并在公开基准上报告结果。"
            "方法保留公式、代码标识符与引用编号，例如 GPT-4 与 [12]。"
        ) * 4
        report = tw.analyze_zh_text("\n\n".join([zh, zh, zh]))
        self.assertTrue(report.ok)
        self.assertGreater(report.cjk_ratio, 0.5)

    def test_references_section_ignored(self) -> None:
        zh = "本文给出智能体安全综述，覆盖提示注入与越狱攻击的现有防御。" * 5
        refs = "References\n" + ("Smith et al. A long English bibliographic entry about agents. " * 6)
        report = tw.analyze_zh_text(zh + "\n\n" + refs)
        self.assertTrue(report.ok)


class PreflightAndPauseTests(unittest.TestCase):
    def test_preflight_models_ok(self) -> None:
        class Resp:
            status = 200

            def read(self, _n: int) -> bytes:
                return b'{"data":[{"id":"demo"}]}'

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

        def opener(req, timeout=20.0):
            self.assertIn("/models", req.full_url)
            self.assertTrue(req.get_header("Authorization").startswith("Bearer "))
            return Resp()

        via = tw.preflight_llm("https://example.test/v1", "secret-key", "demo", opener=opener)
        self.assertEqual(via, "models")

    def test_preflight_empty_key_fails(self) -> None:
        with self.assertRaises(RuntimeError) as ctx:
            tw.preflight_llm("https://example.test/v1", "  ", "demo")
        self.assertIn("OPENAI_API_KEY", str(ctx.exception))

    def test_pause_file_waits_then_continues(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            pause = Path(tmp) / "translate.pause"
            pause.write_text("1", encoding="utf-8")
            ticks = {"n": 0}

            def sleeper(_s: float) -> None:
                ticks["n"] += 1
                if ticks["n"] >= 2:
                    pause.unlink()

            waited = tw.wait_if_paused(str(pause), poll=0.0, max_seconds=10, sleeper=sleeper)
            self.assertTrue(waited)
            self.assertGreaterEqual(ticks["n"], 2)


class RunFlowTests(unittest.TestCase):
    def test_run_babeldoc_retries_without_unknown_flag(self) -> None:
        calls: list[list[str]] = []

        def invoke(argv: list[str]) -> subprocess.CompletedProcess[str]:
            calls.append(argv)
            if any(a.startswith("--watermark-output-mode") for a in argv):
                return subprocess.CompletedProcess(
                    args=argv,
                    returncode=2,
                    stdout="unrecognized arguments: --watermark-output-mode",
                    stderr="",
                )
            return _ok_proc()

        q = tw.QualitySettings(custom_system_prompt="", min_text_length=None)
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp) / "work"
            tw.run_babeldoc(
                Path(tmp) / "in.pdf",
                work,
                model="m",
                base_url="",
                qps=0,
                lang_in="en",
                lang_out="zh-CN",
                api_key="SECRETKEY",
                quality=q,
                invoke=invoke,
                babeldoc_bin="babeldoc",
            )
        self.assertEqual(len(calls), 2)
        self.assertTrue(any(a.startswith("--watermark") for a in calls[0]))
        self.assertFalse(any(a.startswith("--watermark") for a in calls[1]))
        for argv in calls:
            self.assertFalse(tw.argv_has_secret(argv))
            self.assertNotIn("SECRETKEY", " ".join(argv))

    def test_main_ignore_cache_retry_on_completeness_miss(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            src = root / "paper.pdf"
            src.write_bytes(b"%PDF-1.4\n")
            work = root / "translate-work" / "p1"
            work.mkdir(parents=True)
            mono = work / "out.mono.pdf"
            dual = work / "out.dual.pdf"
            mono.write_bytes(b"%PDF-1.4 mono\n")
            dual.write_bytes(b"%PDF-1.4 dual\n")

            calls: list[list[str]] = []

            def fake_run(inp, work_dir, **kwargs):
                calls.append(["--ignore-cache"] if kwargs.get("ignore_cache") else ["first"])
                return ["babeldoc", "--openai"]

            bad = tw.CompletenessReport(
                ok=False, reason="majority English (cjk_ratio=0.04)", extracted=True
            )
            good = tw.CompletenessReport(ok=True, reason="ok", extracted=True, cjk_ratio=0.7)
            scores = iter([bad, good])

            buf = io.StringIO()
            with (
                mock.patch.object(tw, "preflight_llm", return_value="models"),
                mock.patch.object(tw, "run_babeldoc", side_effect=fake_run),
                mock.patch.object(tw, "check_mono_completeness", side_effect=lambda *_a, **_k: next(scores)),
                mock.patch.object(tw, "pause_seconds"),
                mock.patch.object(tw, "wait_if_paused", return_value=False),
                mock.patch.object(sys, "stdout", buf),
            ):
                rc = tw.main(
                    [
                        "--input",
                        str(src),
                        "--id",
                        "p1",
                        "--out-root",
                        str(root),
                        "--model",
                        "demo",
                        "--no-preflight",
                    ]
                )
            self.assertEqual(rc, 0)
            self.assertEqual(calls, [["first"], ["--ignore-cache"]])
            payload = json.loads(buf.getvalue().strip().splitlines()[-1])
            self.assertTrue(payload["ok"])
            self.assertTrue(payload["retried_ignore_cache"])
            self.assertTrue((root / "pdfs" / "zh" / "p1.zh.pdf").is_file())

    def test_check_prints_quality_defaults(self) -> None:
        buf = io.StringIO()
        with mock.patch.object(sys, "stdout", buf):
            rc = tw.main(["--check"])
        self.assertEqual(rc, 0)
        payload = json.loads(buf.getvalue().strip().splitlines()[-1])
        self.assertTrue(payload["ok"])
        self.assertTrue(payload["quality"]["disable_same_text_fallback"])
        self.assertTrue(payload["quality"]["translate_table_text"])
        self.assertFalse(payload["quality"]["enhance_compatibility"])
        self.assertEqual(payload["quality"]["min_text_length"], 3)


if __name__ == "__main__":
    unittest.main()
