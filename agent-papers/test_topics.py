#!/usr/bin/env python3
"""Unit tests for arXiv id parse + stable topic tag validation."""
from __future__ import annotations

import unittest

from search_download_papers import (
    STABLE_TAGS,
    extract_arxiv_id,
    preserve_manual_tags,
    validate_topic_tag,
    Paper,
)


class TestExtractArxivID(unittest.TestCase):
    def test_bare_and_urls(self):
        cases = {
            "2504.01990": "2504.01990",
            "arXiv:2504.01990": "2504.01990",
            "arxiv:2504.01990v2": "2504.01990",
            "https://arxiv.org/abs/2504.01990": "2504.01990",
            "https://arxiv.org/pdf/2504.01990.pdf": "2504.01990",
            "https://arxiv.org/html/2504.01990": "2504.01990",
            "hep-th/9901001": "hep-th/9901001",
            "https://arxiv.org/abs/hep-th/9901001": "hep-th/9901001",
            "https://example.com/paper.pdf": "",
            "not-a-paper": "",
        }
        for raw, want in cases.items():
            self.assertEqual(extract_arxiv_id(raw), want, raw)


class TestValidateTopicTag(unittest.TestCase):
    def test_known_keys(self):
        for key in STABLE_TAGS:
            self.assertEqual(validate_topic_tag(key), key)
            self.assertEqual(validate_topic_tag(key.upper()), key)
        self.assertEqual(validate_topic_tag(" LLM-Training "), "llm-training")
        self.assertIsNone(validate_topic_tag(""))
        self.assertIsNone(validate_topic_tag("unknown"))
        self.assertIsNone(validate_topic_tag("llm"))

    def test_preserve_manual(self):
        p = Paper(title="x", topic_tags=["llm-training"], source="manual")
        self.assertTrue(preserve_manual_tags(p))
        p2 = Paper(title="x", topic_tags=["other"])
        self.assertTrue(preserve_manual_tags(p2))
        p3 = Paper(title="x", topic_tags=["self-evolution"], source="")
        self.assertFalse(preserve_manual_tags(p3))


if __name__ == "__main__":
    unittest.main()
