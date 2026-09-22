#!/usr/bin/env python3
"""Official-list ingest for the Big-4 security conferences.

The arXiv comment/title heuristic misses proceedings papers that never
mention acceptance, and it never sees papers that have no arXiv preprint.
This script treats the DBLP main-volume table of contents as the list of
record for:

- IEEE S&P (Oakland)
- ACM CCS
- USENIX Security
- NDSS

Years default to 2022 through the current calendar year. A volume that
DBLP has not published yet (for example CCS/USENIX in a year whose
proceedings are not online) is reported as unavailable rather than as a
hole in the catalog.

Source of truth
---------------
Preferred: the DBLP conference TOC HTML page (schema.org title/authors +
COinS ``span.Z3988``), same approach as security-paper-mcp-server.
Example pages:

- https://dblp.org/db/conf/sp/sp2024.html
- https://dblp.org/db/conf/ccs/ccs2024.html
- https://dblp.org/db/conf/uss/uss2024.html
- https://dblp.org/db/conf/ndss/ndss2024.html

The DBLP publication search API is not used. It ranks by relevance and
drops recent years. If the HTML page cannot be fetched, the script asks
the DBLP SPARQL endpoint for ``dblp:Inproceedings`` listed on that same
TOC URI (``https://dblp.org/db/conf/sp/sp2024``). That is the same volume,
not a keyword search.

Workshop volumes are separate DBLP keys and are not requested. Co-located
events that share a stream (SOUPS, CSET, WOOT, ``spYYYYw``) are excluded.
A "Workshops" heading inside a main TOC, or a COinS book title containing
"workshop", is skipped. Main-track inproceedings stay, including papers
with no arXiv PDF.

Verified 2026-09-22 against DBLP SPARQL (records on the TOC, including the
proceedings editorship, which this ingest drops):

| volume | TOC records | notes |
| --- | ---: | --- |
| sp2022..sp2026 | 149, 201, 262, 256, 255 | plus spYYYYw workshops |
| ccs2022..ccs2025 | 288, 293, 419, 396 | 2026 not in DBLP yet |
| uss2022..uss2025 | 257, 423, 419, 440 | 2026 not in DBLP yet |
| ndss2022..ndss2026 | 84, 95, 141, 212, 266 | |

S&P 2024 specifically is 261 inproceedings + 1 editorship. security-paper-mcp-server
reported 261 / 418 / 418 for S&P, USENIX, CCS 2024; the extra TOC record
is the editorship.

Venue x year -> DBLP key
-------------------------
Catalog ``venue`` strings match the papers UI (``SECURITY_TOP_VENUES``).

| CLI --venue | catalog venue | DBLP key | TOC URL |
| --- | --- | --- | --- |
| sp, oakland, ieee-sp | IEEE S&P / Oakland | conf/sp/spYYYY | https://dblp.org/db/conf/sp/spYYYY.html |
| ccs, acm-ccs | ACM CCS | conf/ccs/ccsYYYY | https://dblp.org/db/conf/ccs/ccsYYYY.html |
| uss, usenix, usenix-security | USENIX Security | conf/uss/ussYYYY | https://dblp.org/db/conf/uss/ussYYYY.html |
| ndss | NDSS | conf/ndss/ndssYYYY | https://dblp.org/db/conf/ndss/ndssYYYY.html |

Not ingested: conf/sp/spYYYYw, conf/uss/csetYYYY, conf/soups/soupsYYYY,
conf/woot/wootYYYY.

Matching and PDFs
------------------
Each official title is matched to the local catalog by DOI, then arXiv id,
then normalized title (case, punctuation, trailing period) when the year
agrees. A hit gains the ``security-top`` tag and the canonical venue.
Missing fields (doi, authors, year, arXiv, open PDF URL) are filled.
Papers with no arXiv and no open PDF are still upserted.

Open PDFs are arXiv URLs and direct ``.pdf`` links on open hosts
(USENIX, NDSS, IEEE S&P web). Publisher DOI hosts (IEEE Xplore, ACM DL)
are stored as metadata only. On a real run, USENIX/NDSS landing pages are
fetched once to discover a ``.pdf`` href (``--no-fetch-oa`` skips that).

Coverage report
---------------
Stdout and ``{out}/security_top_official_report.json`` include, per
venue x year:

- official_count — main-volume inproceedings
- in_catalog — how many of those already matched a catalog row
- missing — official_count - in_catalog (the gap before this run)
- pdf_ok — local PDF already present, or downloaded on this run
- coverage_pct — 100 * in_catalog / official_count

A second non-dry run should show missing=0 and coverage_pct=100 for every
available volume (metadata complete vs DBLP). pdf_ok / official_count is
the open-PDF coverage. This script does not publish to papers.maclaw.top;
ops regenerate the catalog snapshot separately.

Be polite to DBLP: one request at a time, default 3s gap
(``DBLP_MIN_INTERVAL``), User-Agent ``AgentPapersBot/1.0``.

Examples
--------
    python ingest_security_top_official.py --out /workspace/agent-papers --dry-run
    python ingest_security_top_official.py --venue ndss --year 2024 --dry-run
    python ingest_security_top_official.py --venue sp --year 2022-2026
    python ingest_security_top_official.py --new-only
"""
from __future__ import annotations

import argparse
import html as html_lib
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Optional
from urllib.parse import urlparse

import db as papers_db
from search_download_papers import (
    SECURITY_TOP_VENUES,
    Paper,
    cache_get,
    cache_set,
    download_pdf,
    ensure_dirs,
    extract_arxiv_id,
    extract_doi,
    log,
    paper_to_manifest_dict,
    write_index,
    write_manifest,
)

USER_AGENT = (
    "AgentPapersBot/1.0 (security-top official ingest; research; "
    "+https://github.com/RapidAI/RapidSearch)"
)
DBLP_HTML_BASE = "https://dblp.org/db"
DBLP_SPARQL = "https://sparql.dblp.org/sparql"
DEFAULT_YEAR_START = 2022
REPORT_NAME = "security_top_official_report.json"

# Direct PDF hosts we will download. DOI / publisher portals are not open.
OPEN_PDF_HOSTS = (
    "arxiv.org",
    "export.arxiv.org",
    "arxiv-vanity.com",
    "usenix.org",
    "ndss-symposium.org",
    "ieee-security.org",
)
# Landing pages that often link an open PDF but are not the PDF themselves.
OA_LANDING_HOSTS = (
    "usenix.org",
    "ndss-symposium.org",
    "ieee-security.org",
)
PAYWALL_HOSTS = (
    "doi.org",
    "dx.doi.org",
    "ieeexplore.ieee.org",
    "dl.acm.org",
    "acm.org",
    "link.springer.com",
    "sciencedirect.com",
    "onlinelibrary.wiley.com",
)

WORKSHOP_RE = re.compile(r"\b(workshops?|co-located|affiliated)\b", re.I)
MAIN_VOLUME_RE = re.compile(
    r"conf/(sp/sp|ccs/ccs|uss/uss|ndss/ndss)(?:19|20)\d{2}$"
)
ENTRY_OR_HEADING_RE = re.compile(
    r'(?is)<h[12][^>]*>(.*?)</h[12]>|<li\b(?=[^>]*class="[^"]*\bentry\b)(?=[^>]*class="[^"]*\binproceedings\b)[^>]*>'
)
PDF_HREF_RE = re.compile(r'(?i)href=["\']([^"\']+\.pdf(?:\?[^"\']*)?)["\']')

_last_dblp_call = 0.0
_last_oa_call = 0.0
_html_down = False


@dataclass(frozen=True)
class VenueSpec:
    key: str
    label: str
    dblp_conf: str
    prefix: str
    aliases: tuple[str, ...]


VENUES: dict[str, VenueSpec] = {
    "sp": VenueSpec(
        key="sp",
        label="IEEE S&P / Oakland",
        dblp_conf="sp",
        prefix="sp",
        aliases=(
            "sp",
            "s&p",
            "oakland",
            "ieee-sp",
            "ieee sp",
            "ieee s&p",
            "ieee s&p / oakland",
            "ieee symposium on security and privacy",
        ),
    ),
    "ccs": VenueSpec(
        key="ccs",
        label="ACM CCS",
        dblp_conf="ccs",
        prefix="ccs",
        aliases=(
            "ccs",
            "acm-ccs",
            "acm ccs",
            "acm conference on computer and communications security",
        ),
    ),
    "uss": VenueSpec(
        key="uss",
        label="USENIX Security",
        dblp_conf="uss",
        prefix="uss",
        aliases=(
            "uss",
            "usenix",
            "usenix-security",
            "usenix security",
            "usenix security symposium",
        ),
    ),
    "ndss": VenueSpec(
        key="ndss",
        label="NDSS",
        dblp_conf="ndss",
        prefix="ndss",
        aliases=(
            "ndss",
            "network and distributed system security",
            "network and distributed system security symposium",
        ),
    ),
}

# Guard: catalog chips must stay the strings the papers UI already matches.
assert tuple(v.label for v in VENUES.values()) == tuple(SECURITY_TOP_VENUES)


@dataclass
class OfficialPaper:
    title: str
    authors: list[str] = field(default_factory=list)
    year: Optional[int] = None
    doi: str = ""
    arxiv_id: str = ""
    ee_urls: list[str] = field(default_factory=list)
    dblp_key: str = ""
    dblp_url: str = ""
    venue: str = ""
    venue_key: str = ""
    booktitle: str = ""
    section: str = ""
    pdf_url: str = ""

    def identity(self) -> str:
        if self.dblp_key:
            return self.dblp_key
        if self.doi:
            return "doi:" + self.doi
        return normalize_title(self.title) + "|" + str(self.year or "")


def dblp_sleep(kind: str = "dblp") -> None:
    """One in-flight DBLP (or OA landing) request at a time."""
    global _last_dblp_call, _last_oa_call
    if kind == "oa":
        interval = float(os.environ.get("OA_MIN_INTERVAL", "1.0"))
        last = _last_oa_call
    else:
        interval = float(os.environ.get("DBLP_MIN_INTERVAL", "3.0"))
        last = _last_dblp_call
    elapsed = time.time() - last
    if elapsed < interval:
        time.sleep(interval - elapsed)
    now = time.time()
    if kind == "oa":
        _last_oa_call = now
    else:
        _last_dblp_call = now


def resolve_venue(name: str) -> VenueSpec:
    raw = (name or "").strip().lower()
    raw = raw.replace("_", " ").replace("-", " ")
    raw = re.sub(r"\s+", " ", raw).strip()
    if not raw or raw == "all":
        raise KeyError(name)
    for spec in VENUES.values():
        aliases = {re.sub(r"\s+", " ", a.lower().replace("-", " ").replace("_", " ")) for a in spec.aliases}
        aliases.add(spec.key)
        if raw in aliases:
            return spec
    raise KeyError(name)


def volume_key(spec: VenueSpec, year: int) -> str:
    return f"conf/{spec.dblp_conf}/{spec.prefix}{year}"


def dblp_toc_url(venue: str | VenueSpec, year: int) -> str:
    spec = venue if isinstance(venue, VenueSpec) else resolve_venue(venue)
    return f"{DBLP_HTML_BASE}/{volume_key(spec, year)}.html"


def dblp_toc_uri(spec: VenueSpec, year: int) -> str:
    return f"https://dblp.org/db/{volume_key(spec, year)}"


def is_main_security_volume(key: str) -> bool:
    """True for a Big-4 main proceedings key, false for workshops and satellites.

    ``conf/sp/sp2024`` is main. ``conf/sp/sp2024w``, ``conf/uss/cset2024``,
    ``conf/soups/soups2024``, and ``conf/woot/woot2024`` are not.
    """
    text = (key or "").strip()
    text = re.sub(r"^https?://(?:dblp\.org|dblp\.uni-trier\.de)/db/", "", text)
    text = re.sub(r"\.html?$", "", text).strip("/")
    return bool(MAIN_VOLUME_RE.fullmatch(text))


def normalize_title(title: str) -> str:
    """Loose title key: case, punctuation, and a trailing period do not matter."""
    s = html_lib.unescape(title or "")
    s = s.replace("\u00a0", " ").replace("&", " and ")
    s = s.lower()
    s = re.sub(r"[{}]", "", s)
    s = re.sub(r"[^a-z0-9]+", " ", s)
    return re.sub(r"\s+", " ", s).strip()


def normalize_doi(raw: str) -> str:
    s = html_lib.unescape(raw or "").strip()
    s = re.sub(r"^https?://(?:dx\.)?doi\.org/", "", s, flags=re.I)
    s = re.sub(r"^doi:\s*", "", s, flags=re.I)
    found = extract_doi(s)
    return found.lower().rstrip(".") if found else ""


def clean_author(name: str) -> str:
    s = html_lib.unescape(name or "").strip()
    s = re.sub(r"\s+\d{4}$", "", s)
    return re.sub(r"\s+", " ", s).strip()


def display_title(title: str) -> str:
    s = re.sub(r"\s+", " ", html_lib.unescape(title or "")).strip()
    if s.endswith("."):
        s = s[:-1].rstrip()
    return s


def _host(url: str) -> str:
    try:
        host = (urlparse(url).hostname or "").lower()
    except ValueError:
        return ""
    if host.startswith("www."):
        host = host[4:]
    return host


def _host_in(url: str, hosts: Iterable[str]) -> bool:
    host = _host(url)
    return any(host == h or host.endswith("." + h) for h in hosts)


def is_direct_open_pdf(url: str) -> bool:
    if not url or not url.lower().startswith("http"):
        return False
    if _host_in(url, PAYWALL_HOSTS):
        return False
    path = urlparse(url).path.lower()
    if not path.endswith(".pdf"):
        return False
    if _host_in(url, OPEN_PDF_HOSTS) or _host_in(url, ("arxiv.org",)):
        return True
    # ar5iv / other mirrors: accept a .pdf only on the known open hosts.
    return False


def choose_open_links(urls: Iterable[str]) -> tuple[str, str, list[str]]:
    """Return ``(arxiv_id, direct_pdf_url, oa_landings)`` from DBLP ee links.

    arXiv wins over a publisher PDF. Paywalled DOI links are ignored as PDFs
    and are not returned as landings to crawl.
    """
    arxiv_id = ""
    direct = ""
    landings: list[str] = []
    seen: set[str] = set()
    for raw in urls:
        url = html_lib.unescape(raw or "").strip()
        if not url or url in seen:
            continue
        seen.add(url)
        found = extract_arxiv_id(url)
        if found and not arxiv_id:
            arxiv_id = found
        if is_direct_open_pdf(url) and not direct:
            direct = url
        elif (
            url.lower().startswith("http")
            and _host_in(url, OA_LANDING_HOSTS)
            and not urlparse(url).path.lower().endswith(".pdf")
        ):
            landings.append(url)
    pdf_url = ""
    if arxiv_id:
        pdf_url = f"https://arxiv.org/pdf/{arxiv_id}.pdf"
    elif direct:
        pdf_url = direct
    return arxiv_id, pdf_url, landings


def _first(params: dict[str, list[str]], key: str) -> str:
    vals = params.get(key) or []
    return html_lib.unescape(vals[0]).strip() if vals else ""


def is_workshop_context(section: str = "", booktitle: str = "", dblp_key: str = "") -> bool:
    if dblp_key and not is_main_security_volume(dblp_key) and re.search(
        r"(?:^|/)(?:sp\d{4}w|cset\d{4}|soups\d{4}|woot\d{4})(?:/|$)",
        dblp_key,
        re.I,
    ):
        return True
    if WORKSHOP_RE.search(section or ""):
        return True
    if WORKSHOP_RE.search(booktitle or ""):
        return True
    return False


def _parse_entry_block(block: str, section: str, spec: VenueSpec, year: int) -> Optional[OfficialPaper]:
    coins_m = re.search(r'(?is)<span class="Z3988" title="([^"]+)"', block)
    params: dict[str, list[str]] = {}
    if coins_m:
        params = urllib.parse.parse_qs(html_lib.unescape(coins_m.group(1)), keep_blank_values=False)
    title_m = re.search(r'(?is)<span class="title" itemprop="name">([^<]*)</span>', block)
    if title_m:
        title = display_title(title_m.group(1))
    else:
        title = display_title(_first(params, "rft.atitle").replace("+", " "))
    if not title:
        return None
    authors = [
        clean_author(html_lib.unescape(m))
        for m in re.findall(r'(?is)<span itemprop="name" title="([^"]*)">', block)
    ]
    authors = [a for a in authors if a]
    if not authors:
        authors = [clean_author(a.replace("+", " ")) for a in params.get("rft.au") or []]
        authors = [a for a in authors if a]
    year_m = re.search(r'(?is)<meta itemprop="datePublished" content="(\d{4})"', block)
    paper_year = int(year_m.group(1)) if year_m else year
    if not year_m:
        raw_date = _first(params, "rft.date")
        if raw_date[:4].isdigit():
            paper_year = int(raw_date[:4])
    rfr = _first(params, "rfr_id")
    dblp_key = ""
    if "dblp.org:" in rfr:
        dblp_key = rfr.split("dblp.org:", 1)[1].strip()
    elif rfr:
        dblp_key = rfr
    id_m = re.search(r'(?is)\bid="(conf/[^"]+)"', block)
    if id_m and not dblp_key:
        dblp_key = id_m.group(1)
    booktitle = _first(params, "rft.btitle")
    ee_urls = re.findall(r'(?is)<li class="ee">\s*<a href="([^"]+)"', block)
    if not ee_urls:
        ee_urls = re.findall(r'(?is)<a[^>]+itemprop="url"[^>]+href="([^"]+)"', block)
    for rft_id in params.get("rft_id") or []:
        if rft_id.startswith("info:doi/"):
            ee_urls.append("https://doi.org/" + rft_id[len("info:doi/") :])
        elif rft_id.startswith("http"):
            ee_urls.append(rft_id)
    doi = ""
    for candidate in ee_urls + list(params.get("rft_id") or []):
        doi = normalize_doi(candidate)
        if doi:
            break
    arxiv_id, pdf_url, _landings = choose_open_links(ee_urls)
    if is_workshop_context(section, booktitle, dblp_key):
        return None
    dblp_url = f"https://dblp.org/rec/{dblp_key}.html" if dblp_key else ""
    return OfficialPaper(
        title=title,
        authors=authors,
        year=paper_year,
        doi=doi,
        arxiv_id=arxiv_id,
        ee_urls=list(dict.fromkeys(ee_urls)),
        dblp_key=dblp_key,
        dblp_url=dblp_url,
        venue=spec.label,
        venue_key=spec.key,
        booktitle=booktitle,
        section=section,
        pdf_url=pdf_url,
    )


def parse_toc_html(html: str, venue: str | VenueSpec, year: int) -> list[OfficialPaper]:
    """Parse a DBLP volume TOC. Workshop sections and non-inproceedings are omitted."""
    spec = venue if isinstance(venue, VenueSpec) else resolve_venue(venue)
    matches = list(ENTRY_OR_HEADING_RE.finditer(html or ""))
    section = ""
    papers: list[OfficialPaper] = []
    seen: set[str] = set()
    for i, match in enumerate(matches):
        heading = match.group(1)
        if heading is not None:
            text = re.sub(r"(?is)<[^>]+>", " ", heading)
            section = re.sub(r"\s+", " ", html_lib.unescape(text)).strip()
            continue
        end = matches[i + 1].start() if i + 1 < len(matches) else len(html)
        # Stop the block at the next entry even if a heading regex did not fire.
        nxt = re.search(r'(?is)<li class="entry ', html[match.end() : end])
        block_end = match.end() + nxt.start() if nxt else end
        block = html[match.start() : block_end]
        paper = _parse_entry_block(block, section, spec, year)
        if paper is None:
            continue
        ident = paper.identity()
        if ident in seen:
            continue
        seen.add(ident)
        papers.append(paper)
    return papers


def papers_from_sparql_bindings(
    bindings: list[dict[str, Any]],
    spec: VenueSpec,
    year: int,
) -> list[OfficialPaper]:
    """Group SPARQL rows (one per author / link) into official papers."""
    grouped: dict[str, dict[str, Any]] = {}
    for row in bindings:
        pub = (row.get("pub") or {}).get("value") or ""
        if not pub:
            continue
        slot = grouped.setdefault(
            pub,
            {
                "title": "",
                "year": year,
                "doi": "",
                "urls": [],
                "authors": [],
                "published_in": "",
            },
        )
        title = (row.get("title") or {}).get("value") or ""
        if title and not slot["title"]:
            slot["title"] = display_title(title)
        yraw = (row.get("year") or {}).get("value") or ""
        if str(yraw)[:4].isdigit():
            slot["year"] = int(str(yraw)[:4])
        doi_val = (row.get("doi") or {}).get("value") or ""
        if doi_val and not slot["doi"]:
            slot["doi"] = normalize_doi(doi_val)
        published = (row.get("publishedIn") or {}).get("value") or ""
        if published and not slot["published_in"]:
            slot["published_in"] = published
        for field_name in ("ee", "page"):
            url = (row.get(field_name) or {}).get("value") or ""
            if url and url not in slot["urls"]:
                slot["urls"].append(url)
        name = clean_author((row.get("name") or {}).get("value") or "")
        ord_raw = (row.get("ord") or {}).get("value") or ""
        if name:
            try:
                ordinal = int(ord_raw)
            except ValueError:
                ordinal = len(slot["authors"]) + 1
            slot["authors"].append((ordinal, name))
    papers: list[OfficialPaper] = []
    for pub, slot in grouped.items():
        if not slot["title"]:
            continue
        if WORKSHOP_RE.search(slot["published_in"] or ""):
            continue
        authors = [n for _, n in sorted(slot["authors"], key=lambda x: (x[0], x[1]))]
        # Signatures repeat across link rows; keep first occurrence of each name.
        dedup_authors: list[str] = []
        for name in authors:
            if name not in dedup_authors:
                dedup_authors.append(name)
        key = pub
        key = re.sub(r"^https?://dblp\.org/rec/", "", key).strip("/")
        key = re.sub(r"\.html?$", "", key)
        arxiv_id, pdf_url, _landings = choose_open_links(slot["urls"])
        doi = slot["doi"] or ""
        if not doi:
            for url in slot["urls"]:
                doi = normalize_doi(url)
                if doi:
                    break
        papers.append(
            OfficialPaper(
                title=slot["title"],
                authors=dedup_authors,
                year=slot["year"],
                doi=doi,
                arxiv_id=arxiv_id,
                ee_urls=list(slot["urls"]),
                dblp_key=key,
                dblp_url=pub if pub.endswith(".html") else pub + ".html",
                venue=spec.label,
                venue_key=spec.key,
                booktitle=slot["published_in"],
                pdf_url=pdf_url,
            )
        )
    papers.sort(key=lambda p: (p.title.lower(), p.dblp_key))
    return papers


def http_get(url: str, *, accept: str, timeout: float, kind: str = "dblp") -> tuple[int, bytes]:
    """GET with retries on 429/503. Returns ``(status, body)``. Status 0 is a transport failure."""
    delay = 4.0
    last_status = 0
    last_body = b""
    conn_failures = 0
    for attempt in range(4):
        dblp_sleep(kind)
        req = urllib.request.Request(
            url,
            headers={"User-Agent": USER_AGENT, "Accept": accept},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return int(resp.status), resp.read()
        except urllib.error.HTTPError as exc:
            last_status = int(exc.code)
            last_body = exc.read() if exc.fp else b""
            if last_status in (429, 503) and attempt < 3:
                log(f"  HTTP {last_status} — sleep {delay:.0f}s ({url})")
                time.sleep(delay)
                delay *= 2
                continue
            return last_status, last_body
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            conn_failures += 1
            last_status = 0
            last_body = str(exc).encode("utf-8", "replace")
            if conn_failures < 2:
                log(f"  fetch error {exc!r} — sleep {delay:.0f}s")
                time.sleep(delay)
                delay *= 2
                continue
            return 0, last_body
    return last_status, last_body


def fetch_toc_html(spec: VenueSpec, year: int, cache_dir: Path) -> tuple[str, str]:
    """Return ``(html, error)``. ``error`` is empty on HTTP 200."""
    global _html_down
    url = dblp_toc_url(spec, year)
    if _html_down:
        return "", "html skipped after connection failure"
    cache_key = f"dblp-toc-html|{url}"
    cached = cache_get(cache_dir, cache_key, max_age_s=7 * 86400)
    if isinstance(cached, str) and cached:
        log(f"  cache hit: {url}")
        return cached, ""
    status, body = http_get(url, accept="text/html", timeout=25, kind="dblp")
    if status == 0:
        _html_down = True
        log("  DBLP HTML host failed; later volumes will use the SPARQL TOC fallback")
    if status == 404:
        return "", "404"
    if status != 200:
        snippet = body[:180].decode("utf-8", "replace")
        return "", f"http {status or 'error'}: {snippet}"
    text = body.decode("utf-8", "replace")
    if "entry inproceedings" not in text and "Z3988" not in text:
        return text, "html had no inproceedings markup"
    cache_set(cache_dir, cache_key, text)
    return text, ""


SPARQL_VOLUME = """
PREFIX dblp: <https://dblp.org/rdf/schema#>
SELECT ?pub ?title ?year ?doi ?ee ?page ?name ?ord ?publishedIn WHERE {
  ?pub a dblp:Inproceedings .
  ?pub dblp:listedOnTocPage <%s> .
  ?pub dblp:title ?title .
  OPTIONAL { ?pub dblp:yearOfPublication ?year }
  OPTIONAL { ?pub dblp:doi ?doi }
  OPTIONAL { ?pub dblp:primaryDocumentPage ?ee }
  OPTIONAL { ?pub dblp:documentPage ?page }
  OPTIONAL { ?pub dblp:publishedIn ?publishedIn }
  OPTIONAL {
    ?pub dblp:hasSignature ?sig .
    ?sig dblp:signatureDblpName ?name .
    ?sig dblp:signatureOrdinal ?ord .
  }
}
"""


def fetch_toc_sparql(spec: VenueSpec, year: int, cache_dir: Path) -> tuple[list[OfficialPaper], str]:
    toc = dblp_toc_uri(spec, year)
    cache_key = f"dblp-toc-sparql|{toc}"
    cached = cache_get(cache_dir, cache_key, max_age_s=7 * 86400)
    if isinstance(cached, list):
        log(f"  cache hit: SPARQL {toc}")
        return papers_from_sparql_bindings(cached, spec, year), ""
    query = SPARQL_VOLUME % toc
    url = DBLP_SPARQL + "?" + urllib.parse.urlencode({"query": query})
    status, body = http_get(
        url,
        accept="application/sparql-results+json",
        timeout=120,
        kind="dblp",
    )
    if status != 200:
        snippet = body[:180].decode("utf-8", "replace")
        return [], f"sparql http {status or 'error'}: {snippet}"
    try:
        payload = json.loads(body.decode("utf-8"))
    except json.JSONDecodeError as exc:
        return [], f"sparql json: {exc}"
    bindings = ((payload.get("results") or {}).get("bindings")) or []
    total = ((payload.get("meta") or {}).get("result-size-total"))
    try:
        total_n = int(total)
    except (TypeError, ValueError):
        total_n = None
    if total_n is not None and total_n > len(bindings):
        return [], f"sparql truncated {len(bindings)}/{total_n}"
    cache_set(cache_dir, cache_key, bindings)
    return papers_from_sparql_bindings(bindings, spec, year), ""


def load_catalog(manifest_path: Path) -> list[Paper]:
    if not manifest_path.exists():
        return []
    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    papers: list[Paper] = []
    for raw in data.get("papers") or []:
        if not isinstance(raw, dict):
            continue
        papers.append(Paper(**{k: v for k, v in raw.items() if k in Paper.__dataclass_fields__}))
    return papers


class CatalogIndex:
    def __init__(self, papers: list[Paper]):
        self.papers = papers
        self.by_doi: dict[str, Paper] = {}
        self.by_arxiv: dict[str, Paper] = {}
        self.by_title: dict[str, list[Paper]] = {}
        for paper in papers:
            self.index(paper)

    def index(self, paper: Paper) -> None:
        doi = normalize_doi(paper.doi)
        if doi:
            self.by_doi[doi] = paper
        if paper.arxiv_id:
            self.by_arxiv[paper.arxiv_id] = paper
        key = normalize_title(paper.title)
        if key:
            self.by_title.setdefault(key, []).append(paper)

    def find(self, official: OfficialPaper) -> Optional[Paper]:
        if official.doi and official.doi in self.by_doi:
            return self.by_doi[official.doi]
        if official.arxiv_id and official.arxiv_id in self.by_arxiv:
            return self.by_arxiv[official.arxiv_id]
        key = normalize_title(official.title)
        candidates = list(self.by_title.get(key) or [])
        if not candidates:
            return None
        if official.year:
            same_year = [p for p in candidates if not p.year or int(p.year) == int(official.year)]
            conflict = [p for p in candidates if p.year and int(p.year) != int(official.year)]
            if same_year:
                candidates = same_year
            elif conflict and not same_year:
                return None
        compatible: list[Paper] = []
        for paper in candidates:
            existing = venue_key_of_label(paper.venue)
            if existing and existing != official.venue_key:
                continue
            compatible.append(paper)
        if len(compatible) == 1:
            return compatible[0]
        if len(compatible) > 1:
            # Prefer a row that already carries this venue, else the first.
            for paper in compatible:
                if venue_key_of_label(paper.venue) == official.venue_key:
                    return paper
            return compatible[0]
        return None


def venue_key_of_label(label: str) -> str:
    text = (label or "").strip()
    if not text:
        return ""
    try:
        return resolve_venue(text).key
    except KeyError:
        return ""


def _append_unique(items: list[str], value: str) -> list[str]:
    if value and value not in items:
        items.append(value)
    return items


def apply_official(paper: Paper, official: OfficialPaper) -> None:
    """Overlay official metadata onto a catalog row. Keep abstract and local PDF."""
    if "security-top" not in paper.topic_tags:
        paper.topic_tags = list(paper.topic_tags or []) + ["security-top"]
    if not venue_key_of_label(paper.venue) or venue_key_of_label(paper.venue) == official.venue_key:
        paper.venue = official.venue
    if official.doi and not normalize_doi(paper.doi):
        paper.doi = official.doi
    if official.arxiv_id and not paper.arxiv_id:
        paper.arxiv_id = official.arxiv_id
    if official.authors and not paper.authors:
        paper.authors = list(official.authors)
    if official.year and not paper.year:
        paper.year = official.year
    if official.pdf_url and not paper.pdf_url:
        paper.pdf_url = official.pdf_url
    if not paper.source_url:
        if official.arxiv_id:
            paper.source_url = f"https://arxiv.org/abs/{official.arxiv_id}"
        elif official.dblp_url:
            paper.source_url = official.dblp_url
        elif official.ee_urls:
            paper.source_url = official.ee_urls[0]
    hit = f"dblp:{official.venue_key}{official.year or ''}"
    paper.query_hits = _append_unique(list(paper.query_hits or []), hit)


def new_paper_from_official(official: OfficialPaper) -> Paper:
    if official.arxiv_id:
        source = f"https://arxiv.org/abs/{official.arxiv_id}"
    elif official.dblp_url:
        source = official.dblp_url
    elif official.ee_urls:
        source = official.ee_urls[0]
    else:
        source = ""
    return Paper(
        title=official.title,
        authors=list(official.authors),
        year=official.year,
        source_url=source,
        pdf_url=official.pdf_url,
        arxiv_id=official.arxiv_id,
        doi=official.doi,
        topic_tags=["security-top"],
        venue=official.venue,
        query_hits=[f"dblp:{official.venue_key}{official.year or ''}"],
        score=0.0,
    )


def has_local_pdf(paper: Paper) -> bool:
    path = (paper.pdf_path or "").strip()
    if not path:
        return False
    file = Path(path)
    try:
        return file.is_file() and file.stat().st_size > 1000
    except OSError:
        return False


def discover_pdf_hrefs(page_html: str, page_url: str) -> list[str]:
    found: list[str] = []
    for href in PDF_HREF_RE.findall(page_html or ""):
        absolute = urllib.parse.urljoin(page_url, html_lib.unescape(href))
        if is_direct_open_pdf(absolute) or (
            urlparse(absolute).path.lower().endswith(".pdf") and not _host_in(absolute, PAYWALL_HOSTS)
        ):
            # Keep same-site PDFs from the landing host even if the host
            # check above already passed; reject slide decks when a paper PDF exists.
            found.append(absolute)
    paperish = [u for u in found if not re.search(r"slide|poster", urlparse(u).path, re.I)]
    return paperish or found


def resolve_open_pdf(official: OfficialPaper, cache_dir: Path, fetch_oa: bool) -> None:
    """Fill ``pdf_url`` from ee links, then one polite landing-page fetch."""
    arxiv_id, pdf_url, landings = choose_open_links(official.ee_urls)
    if arxiv_id:
        official.arxiv_id = official.arxiv_id or arxiv_id
    if pdf_url:
        official.pdf_url = pdf_url
        return
    if not fetch_oa or not landings:
        return
    landing = landings[0]
    cache_key = f"oa-landing|{landing}"
    cached = cache_get(cache_dir, cache_key, max_age_s=14 * 86400)
    if isinstance(cached, str):
        hrefs = discover_pdf_hrefs(cached, landing)
    else:
        status, body = http_get(landing, accept="text/html", timeout=45, kind="oa")
        if status != 200:
            return
        text = body.decode("utf-8", "replace")
        cache_set(cache_dir, cache_key, text)
        hrefs = discover_pdf_hrefs(text, landing)
    if hrefs:
        official.pdf_url = hrefs[0]


def coverage_row(
    *,
    spec: VenueSpec,
    year: int,
    source: str,
    official_count: int,
    in_catalog: int,
    pdf_ok: int,
    inserted: int = 0,
    updated: int = 0,
    error: str = "",
) -> dict[str, Any]:
    missing = max(official_count - in_catalog, 0)
    coverage = round(100.0 * in_catalog / official_count, 1) if official_count else None
    after = in_catalog + inserted
    metadata_after = round(100.0 * after / official_count, 1) if official_count else None
    return {
        "venue": spec.label,
        "venue_key": spec.key,
        "year": year,
        "dblp_key": volume_key(spec, year),
        "dblp_url": dblp_toc_url(spec, year),
        "source": source,
        "official_count": official_count,
        "in_catalog": in_catalog,
        "missing": missing,
        "pdf_ok": pdf_ok,
        "inserted": inserted,
        "updated": updated,
        "coverage_pct": coverage,
        "metadata_after_pct": metadata_after,
        "available": source in ("html", "sparql") and not error,
        "error": error,
    }


def parse_year_args(values: list[str], today_year: int) -> list[int]:
    if not values or any(v.strip().lower() == "all" for v in values):
        return list(range(DEFAULT_YEAR_START, today_year + 1))
    years: list[int] = []
    for raw in values:
        for piece in raw.split(","):
            piece = piece.strip()
            if not piece:
                continue
            if re.fullmatch(r"\d{4}-\d{4}", piece):
                start_s, end_s = piece.split("-")
                start, end = int(start_s), int(end_s)
                if end < start:
                    raise argparse.ArgumentTypeError(f"bad year range {piece}")
                years.extend(range(start, end + 1))
            elif re.fullmatch(r"\d{4}", piece):
                years.append(int(piece))
            else:
                raise argparse.ArgumentTypeError(f"bad year {piece}")
    out: list[int] = []
    for year in years:
        if year < 1980 or year > today_year + 1:
            raise argparse.ArgumentTypeError(f"year out of range: {year}")
        if year not in out:
            out.append(year)
    return out


def parse_venue_args(values: list[str]) -> list[VenueSpec]:
    if not values or any(v.strip().lower() == "all" for v in values):
        return list(VENUES.values())
    specs: list[VenueSpec] = []
    seen: set[str] = set()
    for raw in values:
        for piece in raw.split(","):
            piece = piece.strip()
            if not piece:
                continue
            spec = resolve_venue(piece)
            if spec.key not in seen:
                specs.append(spec)
                seen.add(spec.key)
    return specs


def fetch_volume(spec: VenueSpec, year: int, cache_dir: Path) -> tuple[list[OfficialPaper], str, str]:
    """Return ``(papers, source, error)``. HTML TOC first, SPARQL on the same key."""
    if not is_main_security_volume(volume_key(spec, year)):
        return [], "error", f"refusing non-main volume {volume_key(spec, year)}"
    html, html_err = fetch_toc_html(spec, year, cache_dir)
    if html and not html_err:
        papers = parse_toc_html(html, spec, year)
        if papers:
            return papers, "html", ""
        log(f"  HTML TOC parsed 0 papers ({html_err or 'empty'}); trying SPARQL")
    elif html_err == "404":
        # Confirm with SPARQL before calling the year unavailable. A 404 on
        # the HTML host can also be a transient block.
        papers, sparql_err = fetch_toc_sparql(spec, year, cache_dir)
        if papers:
            return papers, "sparql", ""
        if sparql_err:
            return [], "error", f"html 404; {sparql_err}"
        return [], "unavailable", ""
    else:
        log(f"  HTML TOC unavailable ({html_err}); trying SPARQL {dblp_toc_uri(spec, year)}")
    papers, sparql_err = fetch_toc_sparql(spec, year, cache_dir)
    if sparql_err:
        return [], "error", sparql_err
    if not papers:
        return [], "unavailable", ""
    return papers, "sparql", ""


def _download_open_pdf(paper: Paper, pdf_dir: Path, dry_run: bool) -> None:
    if has_local_pdf(paper):
        paper.download_status = paper.download_status or "skipped"
        return
    if not (paper.pdf_url or paper.arxiv_id):
        paper.download_status = "no-pdf"
        return
    if paper.arxiv_id and not paper.pdf_url:
        paper.pdf_url = f"https://arxiv.org/pdf/{paper.arxiv_id}.pdf"
    download_pdf(paper, pdf_dir, dry_run=dry_run)
    if paper.download_status not in ("skipped", "no-pdf", "dry-run"):
        time.sleep(1.0)


def ingest(args: argparse.Namespace) -> dict[str, Any]:
    paths = ensure_dirs(args.out)
    manifest_path = paths["root"] / "manifest.json"
    catalog = CatalogIndex(load_catalog(manifest_path))
    rows: list[dict[str, Any]] = []
    touched: list[Paper] = []
    hard_errors = 0

    for spec in args.venues:
        for year in args.years:
            log(f"=== {spec.label} {year} ({volume_key(spec, year)}) ===")
            papers, source, error = fetch_volume(spec, year, paths["cache"])
            log(f"  source={source} official={len(papers)} {error}")
            if source == "error":
                hard_errors += 1
            matched = inserted = updated = pdf_ok = 0
            for official in papers:
                if args.fetch_oa and not args.dry_run:
                    resolve_open_pdf(official, paths["cache"], fetch_oa=True)
                elif not official.pdf_url:
                    # Still record a direct open PDF / arXiv URL without fetching landings.
                    arxiv_id, pdf_url, _landings = choose_open_links(official.ee_urls)
                    if arxiv_id and not official.arxiv_id:
                        official.arxiv_id = arxiv_id
                    if pdf_url:
                        official.pdf_url = pdf_url
                existing = catalog.find(official)
                if existing is not None:
                    matched += 1
                    if not args.new_only:
                        apply_official(existing, official)
                        updated += 1
                        if not args.dry_run:
                            _download_open_pdf(existing, paths["pdfs"], dry_run=False)
                        touched.append(existing)
                    if has_local_pdf(existing):
                        pdf_ok += 1
                    continue
                created = new_paper_from_official(official)
                inserted += 1
                if not args.dry_run:
                    _download_open_pdf(created, paths["pdfs"], dry_run=False)
                    catalog.papers.append(created)
                    catalog.index(created)
                    touched.append(created)
                    if has_local_pdf(created):
                        pdf_ok += 1
            row = coverage_row(
                spec=spec,
                year=year,
                source=source,
                official_count=len(papers),
                in_catalog=matched,
                pdf_ok=pdf_ok,
                inserted=inserted,
                updated=updated,
                error=error,
            )
            rows.append(row)
            log(
                "  official_count={official_count} in_catalog={in_catalog} "
                "missing={missing} pdf_ok={pdf_ok} coverage_pct={coverage_pct}".format(**row)
            )

    if not args.dry_run and touched:
        papers_out = catalog.papers
        theme_counts: dict[str, int] = {}
        for paper in papers_out:
            for tag in paper.topic_tags or []:
                theme_counts[tag] = theme_counts.get(tag, 0) + 1
        stats = {
            "kept": len(papers_out),
            "pool_unique": len(papers_out),
            "theme_counts": theme_counts,
            "security_top_official": {
                "generated_at": datetime.now(timezone.utc).isoformat(),
                "rows": rows,
            },
            "dry_run": False,
        }
        write_manifest(paths["root"], papers_out, stats)
        write_index(paths["root"], papers_out, stats)
        conn = papers_db.connect(args.db)
        try:
            papers_db.init_db(conn)
            seen_ids: set[str] = set()
            for paper in touched:
                payload = paper_to_manifest_dict(paper)
                pid = papers_db.upsert_paper(conn, payload)
                seen_ids.add(pid)
            log(f"Upserted {len(seen_ids)} papers into {args.db}")
        finally:
            conn.close()

    totals = {
        "official_count": sum(r["official_count"] for r in rows),
        "in_catalog": sum(r["in_catalog"] for r in rows),
        "missing": sum(r["missing"] for r in rows),
        "pdf_ok": sum(r["pdf_ok"] for r in rows),
        "inserted": sum(r["inserted"] for r in rows),
        "updated": sum(r["updated"] for r in rows),
        "errors": hard_errors,
        "unavailable": sum(1 for r in rows if r["source"] == "unavailable"),
    }
    if totals["official_count"]:
        totals["coverage_pct"] = round(100.0 * totals["in_catalog"] / totals["official_count"], 1)
    else:
        totals["coverage_pct"] = None
    report = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "dry_run": bool(args.dry_run),
        "new_only": bool(args.new_only),
        "rows": rows,
        "totals": totals,
    }
    report_path = paths["root"] / REPORT_NAME
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    log(
        "TOTAL official_count={official_count} in_catalog={in_catalog} "
        "missing={missing} pdf_ok={pdf_ok} coverage_pct={coverage_pct}".format(**totals)
    )
    log(f"report: {report_path}")
    if args.dry_run:
        log("Dry-run: manifest, database, and PDFs were not written")
    report["exit_code"] = 1 if hard_errors and not any(r["official_count"] for r in rows) else 0
    return report


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Ingest Big-4 security proceedings from DBLP main-volume TOCs."
    )
    parser.add_argument(
        "--out",
        type=Path,
        default=Path(__file__).resolve().parent,
        help="Catalog root (manifest.json, pdfs/, cache/). Default: this directory.",
    )
    parser.add_argument(
        "--db",
        type=Path,
        default=None,
        help="SQLite path (default: OUT/papers.db).",
    )
    parser.add_argument(
        "--venue",
        action="append",
        default=[],
        help="sp|ccs|uss|ndss or a catalog label. Repeat or comma-separate. Default: all four.",
    )
    parser.add_argument(
        "--year",
        action="append",
        default=[],
        help="YYYY, YYYY-YYYY, or comma list. Default: 2022 through the current year.",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Fetch lists and report coverage only. Do not write manifest, DB, or PDFs.",
    )
    parser.add_argument(
        "--new-only",
        action="store_true",
        help="Insert papers that are not already in the catalog. Do not refresh matches.",
    )
    parser.add_argument(
        "--no-fetch-oa",
        action="store_true",
        help="Do not fetch USENIX/NDSS landing pages to discover a PDF href.",
    )
    args = parser.parse_args(argv)
    today = datetime.now(timezone.utc).year
    try:
        args.venues = parse_venue_args(args.venue)
        args.years = parse_year_args(args.year, today)
    except (KeyError, argparse.ArgumentTypeError) as exc:
        parser.error(str(exc))
    if args.db is None:
        args.db = args.out / "papers.db"
    args.fetch_oa = not args.no_fetch_oa
    return args


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    report = ingest(args)
    return int(report.get("exit_code") or 0)


if __name__ == "__main__":
    raise SystemExit(main())
