#!/usr/bin/env python3
"""Title matching and DBLP TOC parsing for the official security-top ingest."""
from __future__ import annotations

import argparse
import hashlib
import sqlite3
import tempfile
import unittest
from pathlib import Path

from db import connect, init_db, upsert_paper

from ingest_security_top_official import (
    DBLP_TOC,
    VENUE_CCS,
    VENUE_NDSS,
    VENUE_SP,
    VENUE_USENIX,
    DblpClient,
    active_papers,
    normalize_title,
    parse_dblp_toc_html,
    resolve_venues,
    resolve_years,
    security_top_counts_by_venue_year,
    title_match_key,
)


TOC_HTML = """
<ul class="publ-list">
<li class="entry inproceedings" id="conf/sp/Good24" itemscope>
  <span itemprop="author" itemscope itemtype="http://schema.org/Person">
    <span itemprop="name">Jane Doe</span>
  </span>
  <span class="title" itemprop="name">Hello, World: A Full Title.</span>
  <a href="https://doi.org/10.1109/SP54263.2024.00001" itemprop="url">doi</a>
  <a href="https://arxiv.org/abs/2401.00001" itemprop="url">arxiv</a>
</li>
<li class="entry editor" id="conf/sp/2024">
  <span class="title" itemprop="name">45th IEEE Symposium on Security and Privacy.</span>
</li>
<li class="entry inproceedings" id="conf/sp/Side24">
  <span class="title" itemprop="name">Poster: Not a main-track paper.</span>
</li>
<li class="entry inproceedings" id="conf/sp/2024w">
  <span class="title" itemprop="name">Affiliated session paper.</span>
</li>
<li class="entry inproceedings" id="conf/sp/Ws24">
  <span class="title" itemprop="name">1st Workshop on Satellite Crypto.</span>
</li>
</ul>
"""


class TestTitleMatch(unittest.TestCase):
    def test_normalize_ignores_case_and_trailing_punctuation(self):
        self.assertEqual(normalize_title("Hello, World."), normalize_title("hello   world"))
        self.assertEqual(title_match_key("Foo—Bar."), title_match_key("foo bar"))
        self.assertNotEqual(normalize_title("Hello World"), normalize_title("Hello Worlds"))


class TestVenueMap(unittest.TestCase):
    def test_dblp_toc_paths(self):
        self.assertEqual(DBLP_TOC[VENUE_SP].format(year=2024), "sp/sp2024")
        self.assertEqual(DBLP_TOC[VENUE_CCS].format(year=2024), "ccs/ccs2024")
        self.assertEqual(DBLP_TOC[VENUE_USENIX].format(year=2024), "uss/uss2024")
        self.assertEqual(DBLP_TOC[VENUE_NDSS].format(year=2026), "ndss/ndss2026")
        self.assertEqual(VENUE_SP, "IEEE S&P")
        self.assertEqual(VENUE_USENIX, "USENIX Security")

    def test_cli_venue_and_year(self):
        venues = resolve_venues(argparse.Namespace(venue=["usenix", "sp", "ccs", "ndss"]))
        self.assertEqual(venues, [VENUE_USENIX, VENUE_SP, VENUE_CCS, VENUE_NDSS])
        self.assertEqual(
            resolve_years(argparse.Namespace(year=[], years_from=2022, years_to=2026)),
            [2022, 2023, 2024, 2025, 2026],
        )
        self.assertEqual(
            resolve_years(argparse.Namespace(year=[2026, 2024], years_from=2022, years_to=2026)),
            [2024, 2026],
        )


class TestTocParse(unittest.TestCase):
    def test_keeps_main_track_and_marks_editor_and_workshop(self):
        papers = parse_dblp_toc_html(TOC_HTML, VENUE_SP, 2024)
        by_key = {p.dblp_key: p for p in papers}
        main = by_key["conf/sp/Good24"]
        self.assertEqual(main.title, "Hello, World: A Full Title")
        self.assertEqual(main.authors, ["Jane Doe"])
        self.assertEqual(main.year, 2024)
        self.assertEqual(main.venue, "IEEE S&P")
        self.assertEqual(main.doi, "10.1109/SP54263.2024.00001")
        self.assertEqual(main.arxiv_id, "2401.00001")
        self.assertEqual(main.skipped_reason, "")
        self.assertEqual(by_key["conf/sp/2024"].skipped_reason, "editor")
        self.assertEqual(by_key["conf/sp/Side24"].skipped_reason, "workshop-title")
        self.assertEqual(by_key["conf/sp/2024w"].skipped_reason, "workshop-key")
        self.assertEqual(by_key["conf/sp/Ws24"].skipped_reason, "workshop-title")
        active = active_papers(papers)
        self.assertEqual([p.dblp_key for p in active], ["conf/sp/Good24"])


class TestAnubis(unittest.TestCase):
    def test_pow_meets_difficulty(self):
        nonce, digest, _elapsed = DblpClient._solve_pow("security-top", 1)
        raw = hashlib.sha256(("security-top" + str(nonce)).encode()).digest()
        self.assertEqual(digest, raw.hex())
        self.assertEqual(raw[0] >> 4, 0)


class TestVenueColumn(unittest.TestCase):
    def test_legacy_db_gains_venue_and_counts_security_top(self):
        with tempfile.TemporaryDirectory() as tmp:
            db_path = Path(tmp) / "papers.db"
            conn = sqlite3.connect(db_path)
            conn.execute(
                """
                CREATE TABLE papers (
                    id TEXT PRIMARY KEY,
                    title TEXT NOT NULL,
                    authors TEXT,
                    abstract TEXT,
                    year INTEGER,
                    tags TEXT,
                    source_url TEXT,
                    pdf_url TEXT,
                    pdf_path TEXT,
                    published TEXT,
                    updated TEXT,
                    first_seen TEXT,
                    last_seen TEXT,
                    content_hash TEXT
                )
                """
            )
            conn.commit()
            conn.close()
            conn = connect(db_path)
            init_db(conn)
            cols = {row[1] for row in conn.execute("PRAGMA table_info(papers)")}
            self.assertIn("venue", cols)
            upsert_paper(
                conn,
                {
                    "title": "Official row",
                    "arxiv_id": "2401.00009",
                    "year": 2024,
                    "venue": "NDSS",
                    "topic_tags": ["security-top", "venue-ndss"],
                },
            )
            conn.close()
            counts = security_top_counts_by_venue_year(db_path)
            self.assertEqual(counts[("NDSS", 2024)], 1)


if __name__ == "__main__":
    unittest.main()
