#!/usr/bin/env python3
"""Unit tests for the Google Translate helper (no BabelDOC required)."""
from __future__ import annotations

import json
import unittest
from unittest.mock import patch

import google_translate as gt


class ExtractTests(unittest.TestCase):
    def test_extract_babeldoc_input(self) -> None:
        user = (
            ";; Treat next line as plain text input and translate it into zh-CN, "
            "output translation ONLY. Input:\n\nHello {{1}} world"
        )
        self.assertEqual(gt.extract_babeldoc_input(user), "Hello {{1}} world")
        self.assertEqual(gt.extract_babeldoc_input("just text"), "just text")

    def test_last_user_content(self) -> None:
        msgs = [
            {"role": "system", "content": "sys"},
            {"role": "user", "content": "one"},
            {"role": "user", "content": "two"},
        ]
        self.assertEqual(gt.last_user_content(msgs), "two")

    def test_placeholders_roundtrip(self) -> None:
        src = "See {{1}} and {v2} here"
        prot, tokens = gt.protect_placeholders(src)
        self.assertNotIn("{{1}}", prot)
        self.assertEqual(gt.restore_placeholders(prot, tokens), src)

    def test_term_extraction(self) -> None:
        self.assertTrue(gt.looks_like_term_extraction("Extract glossary terms as JSON"))
        self.assertFalse(gt.looks_like_term_extraction("Hello world"))


class ParseTests(unittest.TestCase):
    def test_parse_gtx(self) -> None:
        raw = json.dumps([[["你好", "Hello", None, None, 10]], None, "en"])
        self.assertEqual(gt.parse_gtx(raw), "你好")


class TranslateRoutingTests(unittest.TestCase):
    def test_cloud_when_key(self) -> None:
        def fake_cloud(text, **kwargs):
            return "云端:" + text

        with patch.object(gt, "translate_cloud", side_effect=fake_cloud) as cloud, patch.object(
            gt, "translate_gtx"
        ) as gtx:
            out, via = gt.translate_text("hello {{1}}", api_key="secret-key")
            self.assertEqual(via, "google_cloud")
            self.assertIn("{{1}}", out)
            self.assertTrue(out.startswith("云端:"))
            cloud.assert_called()
            gtx.assert_not_called()

    def test_web_when_no_key(self) -> None:
        with patch.object(gt, "translate_gtx", return_value="网页") as gtx, patch.object(
            gt, "translate_cloud"
        ) as cloud:
            out, via = gt.translate_text("hello", api_key="")
            self.assertEqual((out, via), ("网页", "google_web"))
            gtx.assert_called()
            cloud.assert_not_called()

    def test_web_falls_back_to_mobile(self) -> None:
        with patch.object(gt, "translate_gtx", side_effect=RuntimeError("blocked")), patch.object(
            gt, "translate_mobile", return_value="手机页"
        ):
            out, via = gt.translate_text("hello")
            self.assertEqual((out, via), ("手机页", "google_web"))


class ShimTests(unittest.TestCase):
    def test_shim_translates_payload_only(self) -> None:
        def fake(text, **kwargs):
            return f"ZH:{text}", "google_web"

        shim = gt.GoogleOpenAIShim(translator=fake)
        try:
            base = shim.start()
            import urllib.request

            payload = json.dumps(
                {
                    "messages": [
                        {"role": "user", "content": "prompt wrapper Input:\n\nOnly this"}
                    ]
                }
            ).encode("utf-8")
            req = urllib.request.Request(
                base + "/chat/completions",
                data=payload,
                method="POST",
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                body = json.loads(resp.read().decode("utf-8"))
            content = body["choices"][0]["message"]["content"]
            self.assertEqual(content, "ZH:Only this")
        finally:
            shim.stop()

    def test_shim_term_extraction_json(self) -> None:
        called = []

        def fake(text, **kwargs):
            called.append(text)
            return "nope", "google_web"

        shim = gt.GoogleOpenAIShim(translator=fake)
        try:
            base = shim.start()
            import urllib.request

            payload = json.dumps(
                {
                    "response_format": {"type": "json_object"},
                    "messages": [{"role": "user", "content": "extract glossary"}],
                }
            ).encode("utf-8")
            req = urllib.request.Request(
                base + "/chat/completions",
                data=payload,
                method="POST",
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                body = json.loads(resp.read().decode("utf-8"))
            self.assertEqual(body["choices"][0]["message"]["content"], '{"glossary":[],"terms":[]}')
            self.assertEqual(called, [])
        finally:
            shim.stop()


class RateLimitTests(unittest.TestCase):
    def test_cloud_retries_then_succeeds(self) -> None:
        calls = {"n": 0}

        def fake_http(url, **kwargs):
            calls["n"] += 1
            if calls["n"] < 3:
                return 403, b'{"error":{"message":"User Rate Limit Exceeded"}}'
            payload = {"data": {"translations": [{"translatedText": "你好"}]}}
            return 200, json.dumps(payload, ensure_ascii=False).encode("utf-8")

        with patch.object(gt, "_http_json", side_effect=fake_http), patch.object(
            gt.time, "sleep", lambda _s: None
        ):
            out = gt.translate_cloud("hello", api_key="k", lang_in="en", lang_out="zh-CN")
            self.assertEqual(out, "你好")
            self.assertEqual(calls["n"], 3)

    def test_translate_text_falls_back_to_web_after_rate_limit(self) -> None:
        with patch.object(
            gt,
            "translate_cloud",
            side_effect=RuntimeError("Google Cloud Translation HTTP 403 (rate limit)"),
        ), patch.object(gt, "translate_gtx", return_value="网页你好") as gtx:
            out, via = gt.translate_text("hello", api_key="secret-key")
            self.assertEqual((out, via), ("网页你好", "google_web"))
            gtx.assert_called()

    def test_is_rate_limit_response(self) -> None:
        self.assertTrue(gt.is_rate_limit_response(429, b""))
        self.assertTrue(gt.is_rate_limit_response(403, b"User Rate Limit Exceeded"))
        self.assertTrue(gt.is_rate_limit_response(403, b""))
        self.assertFalse(gt.is_rate_limit_response(403, b"API key not valid"))
        self.assertFalse(gt.is_rate_limit_response(401, b"no"))


if __name__ == "__main__":
    raise SystemExit(unittest.main())
