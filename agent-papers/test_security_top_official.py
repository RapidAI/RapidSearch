#!/usr/bin/env python3
"""Title matching, DBLP venue URLs, and TOC parsing for official security-top ingest."""
from __future__ import annotations

import argparse
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from search_download_papers import Paper, retain_official_security_top, should_skip_paywall_crawl

import ingest_security_top_official as official
from ingest_security_top_official import (
    CatalogIndex,
    OfficialPaper,
    apply_official,
    choose_open_links,
    coverage_row,
    dblp_toc_url,
    discover_pdf_hrefs,
    is_main_security_volume,
    normalize_title,
    papers_from_sparql_bindings,
    parse_toc_html,
    resolve_venue,
    volume_key,
)


TOC_HTML = """
<html><body>
<h2>Main track</h2>
<ul>
<li class="entry inproceedings" id="conf/sp/Good24" itemscope>
  <cite class="data">
    <span itemprop="author"><span itemprop="name" title="Jane Doe 0001">Jane Doe 0001</span></span>,
    <span itemprop="author"><span itemprop="name" title="Alex Roe">Alex Roe</span></span>:
    <span class="title" itemprop="name">Hello, World: A Full Title.</span>
    <meta itemprop="datePublished" content="2024">
  </cite>
  <nav class="publ"><ul>
    <li class="ee"><a href="https://doi.org/10.1109/SP54263.2024.00001">doi</a></li>
    <li class="ee"><a href="https://arxiv.org/abs/2401.00001">arxiv</a></li>
  </ul></nav>
  <span class="Z3988" title="rft.atitle=Truncated&amp;rft.btitle=IEEE+Symposium+on+Security+and+Privacy%2C+SP+2024&amp;rft.date=2024&amp;rft_id=info:doi/10.1109/SP54263.2024.00001&amp;rfr_id=info:sid/dblp.org:conf/sp/Good24"></span>
</li>
<li class="entry inproceedings" id="conf/sp/Work24">
  <cite class="data">
    <span itemprop="name" title="Workshop Author">Workshop Author</span>:
    <span class="title" itemprop="name">Should Stay Out.</span>
  </cite>
  <span class="Z3988" title="rft.atitle=Should+Stay+Out.&amp;rft.btitle=IEEE+Security+and+Privacy+Workshops&amp;rft.date=2024&amp;rfr_id=info:sid/dblp.org:conf/sp/Work24"></span>
</li>
</ul>
<h2>Workshops</h2>
<ul>
<li class="entry inproceedings" id="conf/sp/Side24">
  <cite class="data">
    <span itemprop="name" title="Side Author">Side Author</span>:
    <span class="title" itemprop="name">Co-located Writeup.</span>
  </cite>
  <span class="Z3988" title="rft.atitle=Co-located+Writeup.&amp;rft.btitle=IEEE+Symposium+on+Security+and+Privacy%2C+SP+2024&amp;rft.date=2024&amp;rfr_id=info:sid/dblp.org:conf/sp/Side24"></span>
</li>
</ul>
<li class="entry informaleditorial">not a paper</li>
</body></html>
"""


class TestVenueMap(unittest.TestCase):
    def test_toc_urls(self):
        self.assertEqual(dblp_toc_url("sp", 2024), "https://dblp.org/db/conf/sp/sp2024.html")
        self.assertEqual(dblp_toc_url("oakland", 2022), "https://dblp.org/db/conf/sp/sp2022.html")
        self.assertEqual(dblp_toc_url("ccs", 2024), "https://dblp.org/db/conf/ccs/ccs2024.html")
        self.assertEqual(dblp_toc_url("usenix", 2024), "https://dblp.org/db/conf/uss/uss2024.html")
        self.assertEqual(
            dblp_toc_url("usenix-security", 2025),
            "https://dblp.org/db/conf/uss/uss2025.html",
        )
        self.assertEqual(dblp_toc_url("ndss", 2026), "https://dblp.org/db/conf/ndss/ndss2026.html")
        self.assertEqual(resolve_venue("IEEE S&P / Oakland").label, "IEEE S&P / Oakland")
        self.assertEqual(resolve_venue("ACM CCS").key, "ccs")

    def test_main_volume_keys_exclude_workshops(self):
        self.assertTrue(is_main_security_volume(volume_key(resolve_venue("sp"), 2024)))
        self.assertTrue(is_main_security_volume("https://dblp.org/db/conf/uss/uss2024.html"))
        self.assertTrue(is_main_security_volume("conf/ccs/ccs2025"))
        self.assertTrue(is_main_security_volume("conf/ndss/ndss2022"))
        for satellite in (
            "conf/sp/sp2024w",
            "conf/uss/cset2024",
            "conf/soups/soups2024",
            "conf/woot/woot2026",
            "conf/sp/spw2024",
        ):
            self.assertFalse(is_main_security_volume(satellite), satellite)


class TestTocParse(unittest.TestCase):
    def test_keeps_main_track_and_drops_workshops(self):
        papers = parse_toc_html(TOC_HTML, "sp", 2024)
        self.assertEqual([p.title for p in papers], ["Hello, World: A Full Title"])
        paper = papers[0]
        self.assertEqual(paper.authors, ["Jane Doe", "Alex Roe"])
        self.assertEqual(paper.year, 2024)
        self.assertEqual(paper.doi, "10.1109/sp54263.2024.00001")
        self.assertEqual(paper.arxiv_id, "2401.00001")
        self.assertEqual(paper.pdf_url, "https://arxiv.org/pdf/2401.00001.pdf")
        self.assertEqual(paper.dblp_key, "conf/sp/Good24")
        self.assertEqual(paper.venue, "IEEE S&P / Oakland")
        self.assertNotIn("Truncated", paper.title)

    def test_sparql_groups_authors_and_skips_workshop_label(self):
        spec = resolve_venue("ndss")
        bindings = [
            {
                "pub": {"value": "https://dblp.org/rec/conf/ndss/Aaa22"},
                "title": {"value": "Open Copy."},
                "year": {"value": "2022"},
                "ee": {"value": "https://www.ndss-symposium.org/ndss-paper/open-copy/"},
                "page": {"value": "https://www.usenix.org/system/files/open-copy.pdf"},
                "name": {"value": "Ada Lovelace"},
                "ord": {"value": "2"},
                "publishedIn": {"value": "NDSS"},
            },
            {
                "pub": {"value": "https://dblp.org/rec/conf/ndss/Aaa22"},
                "title": {"value": "Open Copy."},
                "year": {"value": "2022"},
                "ee": {"value": "https://www.ndss-symposium.org/ndss-paper/open-copy/"},
                "name": {"value": "Alan Turing 0001"},
                "ord": {"value": "1"},
                "publishedIn": {"value": "NDSS"},
            },
            {
                "pub": {"value": "https://dblp.org/rec/conf/ndss/Bad22"},
                "title": {"value": "Workshop Piece."},
                "year": {"value": "2022"},
                "publishedIn": {"value": "NDSS Workshops"},
                "name": {"value": "Skip Me"},
                "ord": {"value": "1"},
            },
        ]
        papers = papers_from_sparql_bindings(bindings, spec, 2022)
        self.assertEqual(len(papers), 1)
        self.assertEqual(papers[0].title, "Open Copy")
        self.assertEqual(papers[0].authors, ["Alan Turing", "Ada Lovelace"])
        self.assertEqual(papers[0].pdf_url, "https://www.usenix.org/system/files/open-copy.pdf")


class TestMatching(unittest.TestCase):
    def test_normalize_title(self):
        self.assertEqual(normalize_title("Hello, World."), normalize_title("hello   world"))
        self.assertEqual(normalize_title("A &amp; B"), normalize_title("A and B"))

    def test_match_doi_then_title_and_reject_year_conflict(self):
        known = Paper(
            title="Hello World",
            year=2024,
            arxiv_id="2401.00001",
            topic_tags=["security"],
            abstract="keep me",
        )
        other_year = Paper(title="Hello, World.", year=2020, arxiv_id="2001.00001")
        index = CatalogIndex([known, other_year])
        official = parse_toc_html(TOC_HTML, "sp", 2024)[0]
        # DOI is not on the catalog row; arXiv id matches the 2024 paper.
        hit = index.find(official)
        self.assertIs(hit, known)
        apply_official(hit, official)
        self.assertIn("security-top", hit.topic_tags)
        self.assertIn("security", hit.topic_tags)
        self.assertEqual(hit.venue, "IEEE S&P / Oakland")
        self.assertEqual(hit.abstract, "keep me")
        self.assertEqual(hit.doi, "10.1109/sp54263.2024.00001")

        title_only = OfficialPaperStub()
        self.assertIsNone(index.find(title_only))

    def test_open_links_skip_paywall(self):
        arxiv_id, pdf, landings = choose_open_links(
            [
                "https://doi.org/10.1109/SP.2024.1",
                "https://ieeexplore.ieee.org/document/1",
                "https://www.usenix.org/conference/usenixsecurity24/presentation/doe",
                "https://arxiv.org/pdf/2406.00001v2.pdf",
            ]
        )
        self.assertEqual(arxiv_id, "2406.00001")
        self.assertEqual(pdf, "https://arxiv.org/pdf/2406.00001.pdf")
        self.assertEqual(
            landings,
            ["https://www.usenix.org/conference/usenixsecurity24/presentation/doe"],
        )
        arxiv_id, pdf, landings = choose_open_links(
            ["https://www.usenix.org/system/files/sec24-doe.pdf"]
        )
        self.assertEqual(pdf, "https://www.usenix.org/system/files/sec24-doe.pdf")
        self.assertEqual(landings, [])

    def test_landing_prefers_paper_pdf_over_slides(self):
        page = """
        <a href="/system/files/sec24-doe-slides.pdf">slides</a>
        <a href="https://www.usenix.org/system/files/sec24-doe.pdf">paper</a>
        """
        hrefs = discover_pdf_hrefs(page, "https://www.usenix.org/conference/usenixsecurity24/presentation/doe")
        self.assertEqual(hrefs, ["https://www.usenix.org/system/files/sec24-doe.pdf"])


class OfficialPaperStub:
    """Year-conflicting title with no DOI or arXiv id."""

    title = "Hello, World."
    year = 1999
    doi = ""
    arxiv_id = ""
    venue_key = "sp"


class TestCoverageAndRetention(unittest.TestCase):
    def test_coverage_row_names(self):
        spec = resolve_venue("ccs")
        row = coverage_row(
            spec=spec,
            year=2024,
            source="html",
            official_count=418,
            in_catalog=40,
            pdf_ok=12,
            inserted=378,
        )
        self.assertEqual(row["official_count"], 418)
        self.assertEqual(row["in_catalog"], 40)
        self.assertEqual(row["missing"], 378)
        self.assertEqual(row["pdf_ok"], 12)
        self.assertEqual(row["coverage_pct"], 9.6)
        self.assertEqual(row["dblp_key"], "conf/ccs/ccs2024")
        self.assertEqual(row["metadata_after_pct"], 100.0)

    def test_search_keeps_security_top_without_crawling_doi(self):
        official = Paper(
            title="Not an agent paper",
            topic_tags=["security-top"],
            venue="NDSS",
            source_url="https://doi.org/10.14722/ndss.2024.1",
            score=0.0,
        )
        selected = retain_official_security_top([], {"title:abc": official})
        self.assertEqual(selected, [official])
        self.assertTrue(should_skip_paywall_crawl(official))
        official.arxiv_id = "2401.00002"
        self.assertFalse(should_skip_paywall_crawl(official))


class TestIngestWritesMetadata(unittest.TestCase):
    def test_missing_pdf_is_still_upserted_and_second_run_is_complete(self):
        spec = resolve_venue("ndss")
        paper = OfficialPaper(
            title="No Preprint Available",
            authors=["Ada Lovelace"],
            year=2024,
            doi="10.14722/ndss.2024.23001",
            ee_urls=["https://doi.org/10.14722/ndss.2024.23001"],
            dblp_key="conf/ndss/Lovelace24",
            dblp_url="https://dblp.org/rec/conf/ndss/Lovelace24.html",
            venue=spec.label,
            venue_key=spec.key,
        )

        def fake_fetch(got_spec, year, cache_dir):
            self.assertEqual(got_spec.key, "ndss")
            self.assertEqual(year, 2024)
            return [paper], "html", ""

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            args = argparse.Namespace(
                out=out,
                db=out / "papers.db",
                venues=[spec],
                years=[2024],
                dry_run=False,
                new_only=False,
                fetch_oa=False,
            )
            with patch.object(official, "fetch_volume", fake_fetch):
                first = official.ingest(args)
                second = official.ingest(args)
            manifest = json.loads((out / "manifest.json").read_text(encoding="utf-8"))
            rows = manifest["papers"]
            self.assertEqual(len(rows), 1)
            self.assertEqual(rows[0]["title"], "No Preprint Available")
            self.assertEqual(rows[0]["topic_tags"], ["security-top"])
            self.assertEqual(rows[0]["venue"], "NDSS")
            self.assertEqual(rows[0]["download_status"], "no-pdf")
            self.assertEqual(first["rows"][0]["official_count"], 1)
            self.assertEqual(first["rows"][0]["in_catalog"], 0)
            self.assertEqual(first["rows"][0]["missing"], 1)
            self.assertEqual(first["rows"][0]["pdf_ok"], 0)
            self.assertEqual(second["rows"][0]["in_catalog"], 1)
            self.assertEqual(second["rows"][0]["missing"], 0)
            self.assertEqual(second["rows"][0]["coverage_pct"], 100.0)
            self.assertTrue((out / "security_top_official_report.json").is_file())
            self.assertTrue((out / "papers.db").is_file())


if __name__ == "__main__":
    unittest.main()
