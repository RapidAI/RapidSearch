#!/usr/bin/env python3
"""Ingest Big-4 security papers from official DBLP conference TOCs.

Source of truth: DBLP TOC / search API (not arXiv comment heuristics).
Venues: IEEE S&P, ACM CCS, USENIX Security, NDSS — years 2022–latest.

Examples:
  python ingest_security_top_official.py --gap-report
  python ingest_security_top_official.py --dry-run --venue sp --year 2024
  python ingest_security_top_official.py --new-only --arxiv-lookup
  python ingest_security_top_official.py --venue usenix --year 2023
"""
from __future__ import annotations

import argparse
import hashlib
import http.cookiejar
import json
import re
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import asdict, dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Optional

from db import connect, count_papers, init_db, paper_id_from_fields, upsert_paper
from search_download_papers import (
    Paper,
    arxiv_query,
    download_pdf,
    ensure_dirs,
    extract_arxiv_id,
    log,
    merge_overlay_tags,
    tag_topics,
    write_index,
    write_manifest,
)

TOPIC_KEY = "security-top"
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
)

VENUE_USENIX = "USENIX Security"
VENUE_CCS = "ACM CCS"
VENUE_SP = "IEEE S&P"
VENUE_NDSS = "NDSS"

VENUE_ALIASES = {
    "usenix": VENUE_USENIX,
    "usenix-security": VENUE_USENIX,
    "uss": VENUE_USENIX,
    "ccs": VENUE_CCS,
    "acm-ccs": VENUE_CCS,
    "sp": VENUE_SP,
    "oakland": VENUE_SP,
    "ieee-sp": VENUE_SP,
    "ieee-s&p": VENUE_SP,
    "ndss": VENUE_NDSS,
}

# DBLP TOC path templates under /db/conf/
DBLP_TOC = {
    VENUE_SP: "sp/sp{year}",
    VENUE_CCS: "ccs/ccs{year}",
    VENUE_USENIX: "uss/uss{year}",
    VENUE_NDSS: "ndss/ndss{year}",
}

VENUE_TAG = {
    VENUE_USENIX: "venue-usenix-security",
    VENUE_CCS: "venue-acm-ccs",
    VENUE_SP: "venue-ieee-sp",
    VENUE_NDSS: "venue-ndss",
}

_WORKSHOP_RE = re.compile(
    r"\b("
    r"workshop|woot|cset|soups|hotsec|enigma|poster|demo\b|"
    r"affiliated|satellite|doctoral|phd\s+symposium|"
    r"industrial\s+track|tool\s+demo"
    r")\b",
    re.I,
)
_EDITOR_KEY_RE = re.compile(r"^conf/[^/]+/\d{4}$")


@dataclass
class OfficialPaper:
    title: str
    authors: list[str] = field(default_factory=list)
    year: int = 0
    venue: str = ""
    doi: str = ""
    ee: list[str] = field(default_factory=list)
    dblp_key: str = ""
    dblp_url: str = ""
    arxiv_id: str = ""
    skipped_reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


# -------------------- title normalization / matching --------------------

def normalize_title(title: str) -> str:
    t = (title or "").lower()
    t = t.replace("–", "-").replace("—", "-").replace("’", "'").replace("“", '"').replace("”", '"')
    # Strip trailing punctuation often present in DBLP titles
    t = re.sub(r"[.!?,;:]+$", "", t.strip())
    t = re.sub(r"[^a-z0-9]+", " ", t)
    t = re.sub(r"\s+", " ", t).strip()
    return t


def title_match_key(title: str) -> str:
    return normalize_title(title)


# -------------------- Anubis + DBLP HTTP --------------------

class DblpClient:
    """HTTP client that solves Anubis PoW when challenged and caches cookies."""

    def __init__(self, cache_dir: Path, min_interval: float = 1.2):
        self.cache_dir = cache_dir
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self.cookie_path = cache_dir / "dblp_cookies.txt"
        self.min_interval = min_interval
        self._last = 0.0
        self.cj = http.cookiejar.CookieJar()
        self._load_cookies()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.cj)
        )

    def _load_cookies(self) -> None:
        if not self.cookie_path.exists():
            return
        try:
            jar = http.cookiejar.MozillaCookieJar(str(self.cookie_path))
            jar.load(ignore_discard=True, ignore_expires=True)
            for c in jar:
                self.cj.set_cookie(c)
        except Exception as e:
            log(f"  cookie load warn: {e!r}")

    def _save_cookies(self) -> None:
        lines = ["# Netscape HTTP Cookie File"]
        for c in self.cj:
            if "dblp" not in (c.domain or ""):
                continue
            domain = c.domain
            flag = "TRUE" if domain.startswith(".") else "FALSE"
            secure = "TRUE" if c.secure else "FALSE"
            exp = str(int(c.expires)) if c.expires else "0"
            lines.append(
                f"{domain}\t{flag}\t{c.path or '/'}\t{secure}\t{exp}\t{c.name}\t{c.value}"
            )
        self.cookie_path.write_text("\n".join(lines) + "\n", encoding="utf-8")

    def _throttle(self) -> None:
        elapsed = time.time() - self._last
        if elapsed < self.min_interval:
            time.sleep(self.min_interval - elapsed)
        self._last = time.time()

    def _request(self, url: str, timeout: int = 60) -> tuple[str, bytes]:
        self._throttle()
        req = urllib.request.Request(
            url,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "application/json,text/html,*/*",
            },
        )
        with self.opener.open(req, timeout=timeout) as resp:
            return resp.geturl(), resp.read()

    @staticmethod
    def _solve_pow(random_data: str, difficulty: int) -> tuple[int, str, float]:
        c = difficulty // 2
        odd = (difficulty % 2) != 0
        nonce = 0
        t0 = time.time()
        while True:
            digest = hashlib.sha256((random_data + str(nonce)).encode()).digest()
            ok = True
            for a in range(c):
                if digest[a] != 0:
                    ok = False
                    break
            if ok and odd and (digest[c] >> 4) != 0:
                ok = False
            if ok:
                return nonce, digest.hex(), time.time() - t0
            nonce += 1

    def _pass_anubis(self, html: str, redir: str) -> None:
        m = re.search(
            r'<script id="anubis_challenge"[^>]*>(.*?)</script>', html, re.S
        )
        if not m:
            raise RuntimeError("Anubis challenge page without challenge JSON")
        wrap = json.loads(m.group(1))
        challenge = wrap["challenge"]
        rules = wrap["rules"]
        log(
            f"  Anubis PoW difficulty={rules.get('difficulty')} "
            f"id={challenge.get('id')[:12]}…"
        )
        nonce, digest, elapsed = self._solve_pow(
            challenge["randomData"], int(rules["difficulty"])
        )
        log(f"  Anubis solved nonce={nonce} in {elapsed:.2f}s")
        params = urllib.parse.urlencode(
            {
                "id": challenge["id"],
                "response": digest,
                "nonce": str(nonce),
                "redir": redir,
                "elapsedTime": str(int(elapsed * 1000)),
            }
        )
        pass_url = (
            "https://dblp.org/.within.website/x/cmd/anubis/api/pass-challenge?"
            + params
        )
        self._request(pass_url)
        self._save_cookies()

    def get_bytes(self, url: str, retries: int = 4) -> bytes:
        last_err: Exception | None = None
        for attempt in range(retries):
            try:
                final, body = self._request(url)
                text_head = body[:200].decode("utf-8", "replace")
                if "anubis_challenge" in body[:8000].decode("utf-8", "replace"):
                    html = body.decode("utf-8", "replace")
                    self._pass_anubis(html, redir=url)
                    continue
                if body[:1] == b"{" or b"<" in body[:20] or len(body) > 100:
                    return body
                last_err = RuntimeError(f"unexpected body from {final}: {text_head[:80]}")
            except urllib.error.HTTPError as e:
                last_err = e
                if e.code == 429:
                    wait = 60 * (attempt + 1)
                    log(f"  DBLP 429 — sleep {wait}s")
                    time.sleep(wait)
                    continue
                if e.code in (502, 503, 504):
                    time.sleep(5 * (attempt + 1))
                    continue
                raise
            except Exception as e:
                last_err = e
                time.sleep(3 * (attempt + 1))
        raise RuntimeError(f"DBLP fetch failed for {url}: {last_err!r}")

    def get_json(self, url: str) -> dict:
        body = self.get_bytes(url)
        return json.loads(body.decode("utf-8"))


def dblp_toc_exists(client: DblpClient, path: str) -> bool:
    url = f"https://dblp.org/db/conf/{path}.html"
    try:
        body = client.get_bytes(url)
        text = body[:500].decode("utf-8", "replace")
        if "Making sure" in text:
            return False
        return b"<title>" in body[:2000]
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return False
        raise



def _html_unescape(s: str) -> str:
    import html as _html
    return _html.unescape(s.replace("&apos;", "'").replace("&nbsp;", " "))


_DOI_RE = re.compile(r"10\.\d{4,9}/[-._;()/:A-Z0-9]+", re.I)


def parse_dblp_toc_html(html: str, venue: str, year: int) -> list[OfficialPaper]:
    """Parse DBLP conference TOC HTML into OfficialPaper rows."""
    papers: list[OfficialPaper] = []
    # Split on entry starts so nested </ul> inside nav.publ cannot truncate blocks.
    parts = re.split(r'(?=<li class="entry (?:inproceedings|editor|article)")', html, flags=re.I)
    for part in parts:
        hm = re.match(
            r'<li class="entry (inproceedings|editor|article)"[^>]*\bid="([^"]+)"',
            part,
            re.I,
        )
        if not hm:
            continue
        etype, key = hm.group(1).lower(), hm.group(2)
        block = part
        title_m = re.search(r'<span class="title"[^>]*>(.*?)</span>', block, re.S)
        if not title_m:
            continue
        title = _html_unescape(re.sub(r"<[^>]+>", "", title_m.group(1)))
        title = re.sub(r"\s+", " ", title).strip()
        if title.endswith("."):
            title = title[:-1].strip()

        authors: list[str] = []
        for am in re.finditer(
            r'itemprop="author"[^>]*>.*?<span[^>]*itemprop="name"[^>]*>([^<]+)</span>',
            block,
            re.S,
        ):
            authors.append(_html_unescape(am.group(1).strip()))

        ees_raw = re.findall(r'href="(https?://[^"]+)"[^>]*itemprop="url"', block)
        # Keep publisher / preprint links; drop author profile URLs (dblp.org/pid/…).
        _EE_OK = re.compile(
            r"(doi\.org|arxiv\.org|usenix\.org|ndss-symposium\.org|"
            r"ieee\.org|acm\.org|dl\.acm\.org|ieeexplore)",
            re.I,
        )
        ees = [u for u in ees_raw if _EE_OK.search(u)]
        doi = ""
        for link in ees:
            dm = _DOI_RE.search(link)
            if dm:
                doi = dm.group(0).rstrip(".")
                break
        if not doi:
            dm = re.search(r'data-doi="([^"]+)"', block)
            if dm:
                doi = urllib.parse.unquote(dm.group(1))

        arxiv_id = ""
        for link in ees:
            arxiv_id = extract_arxiv_id(link)
            if arxiv_id:
                break

        op = OfficialPaper(
            title=title,
            authors=authors,
            year=year,
            venue=venue,
            doi=doi,
            ee=ees,
            dblp_key=key,
            dblp_url=f"https://dblp.org/rec/{key}" if key else "",
            arxiv_id=arxiv_id,
        )
        if etype == "editor" or _EDITOR_KEY_RE.match(key):
            op.skipped_reason = "editor"
        elif key and re.search(r"/\d{4}w\d*$|workshop", key, re.I):
            op.skipped_reason = "workshop-key"
        elif re.match(r"(?i)(poster|demo)\s*:", title) or re.search(
            r"(?i)\b\d+(st|nd|rd|th)?\s+workshop\b|workshop on ", title
        ):
            op.skipped_reason = "workshop-title"
        papers.append(op)
    return papers



def fetch_official_list(
    client: DblpClient,
    venue: str,
    year: int,
    cache_dir: Path,
    force: bool = False,
) -> list[OfficialPaper]:
    path = DBLP_TOC[venue].format(year=year)
    cache_file = cache_dir / f"dblp_{venue.replace(' ', '_').replace('&', 'and')}_{year}.json"
    if cache_file.exists() and not force:
        age = time.time() - cache_file.stat().st_mtime
        if age < 7 * 86400:
            data = json.loads(cache_file.read_text(encoding="utf-8"))
            return [OfficialPaper(**x) for x in data]

    url = f"https://dblp.org/db/conf/{path}.html"
    log(f"  GET {url}")
    try:
        body = client.get_bytes(url)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            log(f"  TOC not found (404): {path}")
            cache_file.write_text("[]", encoding="utf-8")
            return []
        raise
    html = body.decode("utf-8", "replace")
    if "anubis_challenge" in html[:8000] or "Making sure you" in html[:2000]:
        raise RuntimeError("Anubis not cleared for TOC HTML")
    papers = parse_dblp_toc_html(html, venue, year)
    log(f"  HTML parsed {len(papers)} entries for {venue} {year}")

    cache_file.write_text(
        json.dumps([p.to_dict() for p in papers], ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    return papers



def active_papers(papers: list[OfficialPaper]) -> list[OfficialPaper]:
    return [p for p in papers if not p.skipped_reason]


# -------------------- catalog index --------------------

def load_catalog_indexes(out: Path, db_path: Path) -> tuple[dict[str, dict], dict[str, dict], dict[str, dict]]:
    """Return (by_norm_title, by_arxiv, by_doi) maps to paper dicts."""
    by_title: dict[str, dict] = {}
    by_arxiv: dict[str, dict] = {}
    by_doi: dict[str, dict] = {}

    conn = connect(db_path)
    try:
        init_db(conn)
        rows = conn.execute(
            "SELECT id, title, authors, abstract, year, tags, source_url, pdf_url, "
            "pdf_path, published, updated, venue FROM papers"
        ).fetchall()
        for r in rows:
            d = dict(r)
            d["topic_tags"] = [t for t in (d.get("tags") or "").split("|") if t]
            d["arxiv_id"] = d["id"] if re.match(r"^\d{4}\.\d{4,5}$", d["id"] or "") else ""
            if d["id"].startswith("doi:"):
                d["doi"] = d["id"][4:]
            else:
                d["doi"] = ""
            key = title_match_key(d.get("title") or "")
            if key:
                by_title[key] = d
            if d.get("arxiv_id"):
                by_arxiv[d["arxiv_id"]] = d
            if d.get("doi"):
                by_doi[d["doi"].lower()] = d
    finally:
        conn.close()

    man = out / "manifest.json"
    if man.exists():
        try:
            data = json.loads(man.read_text(encoding="utf-8"))
            for p in data.get("papers") or []:
                key = title_match_key(p.get("title") or "")
                aid = (p.get("arxiv_id") or "").strip()
                doi = (p.get("doi") or "").strip().lower()
                if key and key not in by_title:
                    by_title[key] = p
                if aid:
                    by_arxiv[aid] = p
                if doi:
                    by_doi[doi] = p
        except Exception as e:
            log(f"manifest index warn: {e!r}")
    return by_title, by_arxiv, by_doi


def security_top_counts_by_venue_year(db_path: Path) -> dict[tuple[str, int], int]:
    conn = connect(db_path)
    try:
        rows = conn.execute(
            "SELECT venue, year, COUNT(*) AS c FROM papers "
            "WHERE tags LIKE '%security-top%' AND venue IS NOT NULL AND venue != '' "
            "GROUP BY venue, year"
        ).fetchall()
        return {(r["venue"], int(r["year"] or 0)): int(r["c"]) for r in rows}
    finally:
        conn.close()


def apply_security_top_paper(p: Paper, venue: str, year: int) -> Paper:
    tags = tag_topics(p.title, p.abstract)
    tags = merge_overlay_tags(p.topic_tags, tags)
    if TOPIC_KEY not in tags:
        tags = list(tags) + [TOPIC_KEY]
    vtag = VENUE_TAG.get(venue)
    if vtag and vtag not in tags:
        tags.append(vtag)
    # Drop other venue-* tags if present
    tags = [t for t in tags if not t.startswith("venue-") or t == vtag]
    if vtag and vtag not in tags:
        tags.append(vtag)
    p.topic_tags = tags
    p.venue = venue
    p.year = year
    p.score = max(p.score, 5.0)
    return p


def official_to_paper(op: OfficialPaper) -> Paper:
    p = Paper(
        title=op.title,
        authors=list(op.authors),
        year=op.year,
        doi=op.doi,
        arxiv_id=op.arxiv_id,
        venue=op.venue,
        source_url=op.dblp_url or (f"https://doi.org/{op.doi}" if op.doi else ""),
        published=f"{op.year}-01-01",
        source="dblp-official",
    )
    if op.arxiv_id:
        p.pdf_url = f"https://arxiv.org/pdf/{op.arxiv_id}.pdf"
        p.source_url = f"https://arxiv.org/abs/{op.arxiv_id}"
    elif op.doi:
        p.source_url = f"https://doi.org/{op.doi}"
    return apply_security_top_paper(p, op.venue, op.year)


def find_arxiv_by_title(title: str, cache_dir: Path) -> Optional[Paper]:
    # Exact-ish title search
    q = f'ti:"{title}"'
    try:
        hits = arxiv_query(q, cache_dir, start=0, max_results=5, sort_by="relevance")
    except Exception as e:
        log(f"  arxiv lookup error: {e!r}")
        return None
    want = title_match_key(title)
    for h in hits:
        if title_match_key(h.title) == want:
            return h
    # Soft: first hit if high overlap
    if hits:
        h = hits[0]
        a = set(want.split())
        b = set(title_match_key(h.title).split())
        if a and b and len(a & b) / max(len(a), len(b)) >= 0.9:
            return h
    return None


def load_manifest_papers(manifest_path: Path) -> dict[str, Paper]:
    if not manifest_path.exists():
        return {}
    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    out: dict[str, Paper] = {}
    for d in data.get("papers") or []:
        try:
            p = Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})
            out[p.dedupe_key()] = p
        except Exception:
            continue
    return out


# -------------------- gap report --------------------

def write_gap_report(
    path: Path,
    matrix: list[dict],
    samples: dict[tuple[str, int], list[str]],
) -> None:
    lines = [
        "security-top official (DBLP) gap report",
        f"Generated: {datetime.now().astimezone(__import__('zoneinfo').ZoneInfo('Asia/Shanghai')).strftime('%Y-%m-%d %H:%M:%S %Z')} (Asia/Shanghai)",
        "",
        "== Per venue × year ==",
        f"{'venue':16} {'year':>4} {'official':>8} {'tagged':>7} {'missing':>7} {'cov%':>6}",
    ]
    for row in matrix:
        lines.append(
            f"{row['venue']:16} {row['year']:4d} {row['official']:8d} "
            f"{row['tagged']:7d} {row['missing']:7d} {row['coverage']:5.1f}%"
        )
    lines.append("")
    lines.append("== Missing title samples (up to 8 per cell) ==")
    for (venue, year), titles in samples.items():
        lines.append(f"\n[{venue} {year}] ({len(titles)} shown)")
        for t in titles:
            lines.append(f"  - {t}")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    log(f"Wrote gap report {path}")


# -------------------- main --------------------

def parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", type=Path, default=Path("/workspace/agent-papers"))
    ap.add_argument("--venue", action="append", default=[], help="usenix|ccs|sp|ndss (repeatable)")
    ap.add_argument("--year", type=int, action="append", default=[], help="Conference year (repeatable)")
    ap.add_argument("--years-from", type=int, default=2022)
    ap.add_argument("--years-to", type=int, default=2026)
    ap.add_argument("--gap-report", action="store_true", help="Only produce gap report vs DBLP")
    ap.add_argument("--gap-report-path", type=Path, default=Path("/workspace/sec-official-gap-report.txt"))
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--new-only", action="store_true", help="Only upsert papers not already in catalog")
    ap.add_argument("--skip-download", action="store_true")
    ap.add_argument("--arxiv-lookup", action="store_true", help="Search arXiv by title for missing PDFs")
    ap.add_argument("--arxiv-lookup-max", type=int, default=80)
    ap.add_argument("--force-fetch", action="store_true", help="Ignore DBLP list cache")
    ap.add_argument("--dblp-interval", type=float, default=4.0)
    return ap.parse_args()


def resolve_venues(args: argparse.Namespace) -> list[str]:
    if not args.venue:
        return [VENUE_SP, VENUE_CCS, VENUE_USENIX, VENUE_NDSS]
    out = []
    for v in args.venue:
        key = VENUE_ALIASES.get(v.lower().strip())
        if not key:
            raise SystemExit(f"Unknown venue alias: {v}")
        if key not in out:
            out.append(key)
    return out


def resolve_years(args: argparse.Namespace) -> list[int]:
    if args.year:
        return sorted(set(args.year))
    return list(range(args.years_from, args.years_to + 1))


def main() -> int:
    args = parse_args()
    out: Path = args.out
    paths = ensure_dirs(out)
    db_path = out / "papers.db"
    cache_dir = paths["cache"] / "dblp"
    cache_dir.mkdir(parents=True, exist_ok=True)

    venues = resolve_venues(args)
    years = resolve_years(args)
    client = DblpClient(cache_dir, min_interval=args.dblp_interval)

    log(f"Official DBLP ingest venues={venues} years={years}")

    # Fetch all requested official lists
    lists: dict[tuple[str, int], list[OfficialPaper]] = {}
    for venue in venues:
        for year in years:
            path = DBLP_TOC[venue].format(year=year)
            log(f"Fetching {venue} {year} ({path})…")
            time.sleep(max(3.0, args.dblp_interval))
            try:
                papers = fetch_official_list(
                    client, venue, year, cache_dir, force=args.force_fetch
                )
            except Exception as e:
                log(f"  FAIL {venue} {year}: {e!r}")
                lists[(venue, year)] = []
                continue
            active = active_papers(papers)
            log(
                f"  {venue} {year}: raw={len(papers)} active={len(active)} "
                f"skipped={len(papers)-len(active)}"
            )
            lists[(venue, year)] = papers

    by_title, by_arxiv, by_doi = load_catalog_indexes(out, db_path)
    before_counts = security_top_counts_by_venue_year(db_path)

    # Gap analysis
    matrix = []
    samples: dict[tuple[str, int], list[str]] = {}
    for venue in venues:
        for year in years:
            papers = active_papers(lists.get((venue, year), []))
            official_n = len(papers)
            tagged = before_counts.get((venue, year), 0)
            missing_titles = []
            for op in papers:
                key = title_match_key(op.title)
                hit = None
                if op.arxiv_id and op.arxiv_id in by_arxiv:
                    hit = by_arxiv[op.arxiv_id]
                elif op.doi and op.doi.lower() in by_doi:
                    hit = by_doi[op.doi.lower()]
                elif key in by_title:
                    hit = by_title[key]
                if hit is None:
                    missing_titles.append(op.title)
                else:
                    tags = hit.get("topic_tags") or [
                        t for t in (hit.get("tags") or "").split("|") if t
                    ]
                    if TOPIC_KEY not in tags or (hit.get("venue") or "") != venue:
                        missing_titles.append(op.title)
            missing = len(missing_titles)
            # Coverage = share of official titles present+tagged in catalog (any year field)
            present_tagged = official_n - missing
            cov = (100.0 * present_tagged / official_n) if official_n else 0.0
            matrix.append(
                {
                    "venue": venue,
                    "year": year,
                    "official": official_n,
                    "tagged": tagged,
                    "missing": missing,
                    "coverage": cov,
                    "present_tagged": present_tagged,
                }
            )
            samples[(venue, year)] = missing_titles[:8]

    if args.gap_report or args.dry_run:
        write_gap_report(args.gap_report_path, matrix, samples)

    if args.gap_report:
        # Also dump JSON summary next to report
        summary_path = out / "ingest_security_top_official_gap.json"
        summary_path.write_text(
            json.dumps({"matrix": matrix, "samples": {f"{v}|{y}": t for (v, y), t in samples.items()}}, indent=2),
            encoding="utf-8",
        )
        log("Gap-report-only mode; exiting")
        return 0

    # Ingest
    failures: list[dict] = []
    added = 0
    retagged = 0
    downloaded = 0
    meta_only = 0
    arxiv_lookups = 0
    processed_ops: list[tuple[OfficialPaper, Paper, str]] = []  # op, paper, action

    for venue in venues:
        for year in years:
            papers = active_papers(lists.get((venue, year), []))
            log(f"Ingesting {venue} {year}: {len(papers)} official papers")
            for op in papers:
                key = title_match_key(op.title)
                existing = None
                if op.arxiv_id and op.arxiv_id in by_arxiv:
                    existing = by_arxiv[op.arxiv_id]
                elif op.doi and op.doi.lower() in by_doi:
                    existing = by_doi[op.doi.lower()]
                elif key in by_title:
                    existing = by_title[key]

                if existing:
                    tags = existing.get("topic_tags") or [
                        t for t in (existing.get("tags") or "").split("|") if t
                    ]
                    already = TOPIC_KEY in tags and (existing.get("venue") or "") == venue
                    if args.new_only and already:
                        continue
                    # Build Paper from existing + overlay
                    p = Paper(
                        title=existing.get("title") or op.title,
                        authors=(
                            existing.get("authors")
                            if isinstance(existing.get("authors"), list)
                            else [a.strip() for a in (existing.get("authors") or "").split(";") if a.strip()]
                        )
                        or list(op.authors),
                        abstract=existing.get("abstract") or "",
                        year=year,
                        source_url=existing.get("source_url") or "",
                        pdf_url=existing.get("pdf_url") or "",
                        arxiv_id=existing.get("arxiv_id")
                        or (existing["id"] if re.match(r"^\d{4}\.\d{4,5}$", existing.get("id") or "") else "")
                        or op.arxiv_id,
                        doi=existing.get("doi") or op.doi,
                        topic_tags=list(tags),
                        pdf_path=existing.get("pdf_path") or "",
                        published=existing.get("published") or f"{year}-01-01",
                        updated=existing.get("updated") or "",
                        venue=venue,
                        source=existing.get("source") or "dblp-official",
                    )
                    if not p.pdf_url and p.arxiv_id:
                        p.pdf_url = f"https://arxiv.org/pdf/{p.arxiv_id}.pdf"
                    p = apply_security_top_paper(p, venue, year)
                    action = "retag" if already else "retag-new-tag"
                    if not args.skip_download and p.arxiv_id and not args.dry_run:
                        if not (p.pdf_path and Path(p.pdf_path).exists()):
                            download_pdf(p, paths["pdfs"], dry_run=False)
                            if p.download_status == "ok":
                                downloaded += 1
                    processed_ops.append((op, p, action))
                    retagged += 1
                    # refresh indexes
                    by_title[key] = {**existing, "title": p.title, "venue": venue, "year": year, "topic_tags": p.topic_tags}
                    continue

                # Missing entirely
                p = official_to_paper(op)
                if args.arxiv_lookup and not p.arxiv_id and arxiv_lookups < args.arxiv_lookup_max:
                    arxiv_lookups += 1
                    hit = find_arxiv_by_title(op.title, paths["cache"])
                    if hit and hit.arxiv_id:
                        p.arxiv_id = hit.arxiv_id
                        p.abstract = hit.abstract or p.abstract
                        p.pdf_url = f"https://arxiv.org/pdf/{p.arxiv_id}.pdf"
                        p.source_url = f"https://arxiv.org/abs/{p.arxiv_id}"
                        if hit.authors:
                            p.authors = hit.authors
                        log(f"  arXiv hit {p.arxiv_id}: {p.title[:60]}")

                if not args.skip_download and p.arxiv_id and not args.dry_run:
                    download_pdf(p, paths["pdfs"], dry_run=False)
                    if p.download_status == "ok":
                        downloaded += 1
                    elif p.download_status.startswith("failed"):
                        failures.append(
                            {"title": p.title, "arxiv_id": p.arxiv_id, "error": p.download_status}
                        )
                else:
                    if not p.arxiv_id:
                        p.download_status = "no-pdf"
                        meta_only += 1
                    elif args.skip_download or args.dry_run:
                        p.download_status = "skipped-meta" if args.skip_download else "dry-run"

                processed_ops.append((op, p, "new"))
                added += 1
                # index
                by_title[key] = {
                    "title": p.title,
                    "venue": venue,
                    "year": year,
                    "topic_tags": p.topic_tags,
                    "arxiv_id": p.arxiv_id,
                    "doi": p.doi,
                }
                if p.arxiv_id:
                    by_arxiv[p.arxiv_id] = by_title[key]
                if p.doi:
                    by_doi[p.doi.lower()] = by_title[key]

    log(
        f"Processed actions: added≈{added} retagged≈{retagged} "
        f"downloaded={downloaded} meta_only={meta_only} failures={len(failures)}"
    )

    if args.dry_run:
        write_gap_report(args.gap_report_path, matrix, samples)
        log("Dry-run: no DB/manifest writes")
        return 0

    # Persist DB + manifest
    if processed_ops:
        conn = connect(db_path)
        try:
            for op, p, action in processed_ops:
                upsert_paper(conn, {**asdict(p), "tags": p.topic_tags})
        finally:
            conn.close()

        existing = load_manifest_papers(out / "manifest.json")
        merged = dict(existing)
        for op, p, action in processed_ops:
            key = p.dedupe_key()
            # Prefer arxiv key when available; also remove stale title-only dupes
            if key in merged:
                old = merged[key]
                if old.pdf_path and not p.pdf_path:
                    p.pdf_path = old.pdf_path
                if old.download_status == "ok" and p.download_status in (
                    "skipped",
                    "skipped-meta",
                    "no-pdf",
                    "",
                ):
                    p.download_status = "ok"
                p.topic_tags = merge_overlay_tags(old.topic_tags, p.topic_tags)
                if TOPIC_KEY not in p.topic_tags:
                    p.topic_tags = list(p.topic_tags) + [TOPIC_KEY]
                if old.abstract and not p.abstract:
                    p.abstract = old.abstract
            merged[key] = p
            # Drop title-hash duplicate if we now have arxiv/doi id
            title_key = f"title:{hashlib.sha1(normalize_title(p.title).encode()).hexdigest()[:16]}"
            # Paper.dedupe_key uses different norm — also clear normalize_title variant
            alt = Paper(title=p.title).dedupe_key()
            if alt != key and alt in merged and not merged[alt].arxiv_id:
                del merged[alt]

        papers = list(merged.values())
        papers.sort(key=lambda x: (-(x.year or 0), -x.score, x.title))
        theme_counts: dict[str, int] = {}
        venue_counts: dict[str, int] = {}
        for p in papers:
            for t in p.topic_tags:
                theme_counts[t] = theme_counts.get(t, 0) + 1
            if p.venue:
                venue_counts[p.venue] = venue_counts.get(p.venue, 0) + 1
        man_stats = {
            "kept": len(papers),
            "downloaded": downloaded,
            "failures": len(failures),
            "theme_counts": theme_counts,
            "venue_counts": venue_counts,
            "ingest": "security-top-official",
            "new_this_sync": added,
            "retagged": retagged,
            "meta_only": meta_only,
        }
        write_manifest(out, papers, man_stats)
        write_index(out, papers, man_stats)

        # Sidecar venue map for arxiv ids
        sidecar = out / "security_top_venues.json"
        venue_map: dict[str, str] = {}
        if sidecar.exists():
            try:
                venue_map = json.loads(sidecar.read_text(encoding="utf-8"))
            except Exception:
                venue_map = {}
        for p in papers:
            if p.arxiv_id and p.venue:
                venue_map[p.arxiv_id] = p.venue
        sidecar.write_text(
            json.dumps(venue_map, ensure_ascii=False, indent=2, sort_keys=True),
            encoding="utf-8",
        )

    after_counts = security_top_counts_by_venue_year(db_path)

    # Final report
    report_lines = [
        "security-top official (DBLP) ingest report",
        f"Generated: {datetime.now().astimezone(__import__('zoneinfo').ZoneInfo('Asia/Shanghai')).strftime('%Y-%m-%d %H:%M:%S %Z')} (Asia/Shanghai)",
        "",
        "== Summary ==",
        f"Added (new catalog rows actions): {added}",
        f"Retagged / refreshed: {retagged}",
        f"PDFs downloaded this run: {downloaded}",
        f"Metadata-only (no arXiv PDF): {meta_only}",
        f"Failures: {len(failures)}",
        f"DB rows: {count_papers(db_path)}",
        f"security-top total: {sum(after_counts.values())}",
        "",
        "== Per venue × year (official vs before/after tagged by venue+year) ==",
        f"{'venue':16} {'year':>4} {'official':>8} {'before':>7} {'after':>7} {'cov%':>6}",
    ]
    for row in matrix:
        venue, year = row["venue"], row["year"]
        official = row["official"]
        before = before_counts.get((venue, year), 0)
        after = after_counts.get((venue, year), 0)
        cov = (100.0 * after / official) if official else 0.0
        report_lines.append(
            f"{venue:16} {year:4d} {official:8d} {before:7d} {after:7d} {cov:5.1f}%"
        )
        row["before"] = before
        row["after"] = after
        row["coverage_after"] = cov

    report_lines.append("")
    report_lines.append("== Notes ==")
    report_lines.append("- official = DBLP TOC inproceedings (editor front-matter skipped)")
    report_lines.append("- before/after = papers.db rows with tags containing security-top and venue+year")
    report_lines.append("- Year set to conference edition year from DBLP on upsert/retag")
    report_lines.append("- PDFs downloaded only when arXiv id known (ee link or --arxiv-lookup)")
    report_lines.append("- Paywalled publisher PDFs are metadata-only by design")
    if failures:
        report_lines.append("")
        report_lines.append("== Failures (sample) ==")
        for f in failures[:20]:
            report_lines.append(f"  - {f}")

    report_path = Path("/workspace/sec-official-ingest-report.txt")
    report_path.write_text("\n".join(report_lines) + "\n", encoding="utf-8")
    (out / "ingest_security_top_official_report.json").write_text(
        json.dumps(
            {
                "matrix": matrix,
                "added": added,
                "retagged": retagged,
                "downloaded": downloaded,
                "meta_only": meta_only,
                "failures": failures,
                "security_top_total": sum(after_counts.values()),
            },
            indent=2,
            ensure_ascii=False,
        ),
        encoding="utf-8",
    )
    log(f"Wrote {report_path}")
    log("=== OFFICIAL SECURITY-TOP INGEST DONE ===")
    return 0


if __name__ == "__main__":
    sys.exit(main())
