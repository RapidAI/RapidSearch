# Agent Papers — Self-Evolution, Security & LLM-IoT

搜索并下载「智能体自进化 / 自改进」、「智能体安全 / 安全自进化」与「LLM based 物联网」相关学术论文（以 ArXiv 为主），并维护本地 **SQLite FTS5** 可检索库。

Search and download academic papers on **agent self-evolution / self-improvement**, **agent security / safe self-evolution**, and **LLM-based IoT / AIoT** (ArXiv primary), with a local **SQLite FTS5** retrieval database.

## Setup / 环境

```bash
# Prefer existing Crawl4AI venv
/workspace/crawl4ai-venv/bin/python -m pip install -r requirements.txt
```

Optional: local RapidSearch at `http://127.0.0.1:18765` (used by full search if `/health` is OK).

Python: `/workspace/crawl4ai-venv/bin/python`

## Paths / 路径

| Path | Role |
|------|------|
| `/workspace/agent-papers/papers.db` | SQLite + FTS5 retrieval DB |
| `/workspace/agent-papers/pdfs/` | Downloaded PDFs |
| `/workspace/agent-papers/manifest.json` | Structured metadata (merged on sync) |
| `/workspace/agent-papers/index.md` | Human-readable index |
| `/workspace/agent-papers/sync_report.json` | Last sync machine report |
| `/workspace/agent-papers/sync_latest.md` | Last sync short summary |
| `/workspace/agent-papers/cache/` | ArXiv / API response cache |

## Full search (one-shot) / 全量搜索下载

```bash
cd /workspace/agent-papers
/workspace/crawl4ai-venv/bin/python search_download_papers.py --out /workspace/agent-papers --max 80
```

| Flag | Meaning |
|------|---------|
| `--out DIR` | Output root (default: script dir) |
| `--max N` | Max unique papers to keep/download (default 20; use 80+ for expansion) |
| `--dry-run` | Search + filter only; no PDF downloads |
| `--queries "...;..."` | Override default query list (`;`-separated) |

## Incremental sync / 增量同步（只拉新论文）

Periodically fetch **only NEW** papers (by ArXiv `submittedDate` lookback + local id set), download new PDFs, upsert FTS DB, merge manifest/index.

```bash
cd /workspace/agent-papers
# Cron-friendly: exits 0 even if 0 new
/workspace/crawl4ai-venv/bin/python sync_papers.py --out /workspace/agent-papers --max-new 30 --days 7

# Preview without writing DB/manifest/PDFs
/workspace/crawl4ai-venv/bin/python sync_papers.py --out /workspace/agent-papers --max-new 5 --days 7 --dry-run
```

| Flag | Meaning |
|------|---------|
| `--out DIR` | Output root |
| `--max-new N` | Max new papers this run (default 30) |
| `--days N` | ArXiv `submittedDate` lookback days (default 7) |
| `--dry-run` | Search/filter only; write `sync_*.` reports, **no** DB/manifest/PDF writes |
| `--queries "...;..."` | Override ArXiv queries (`;`-separated) |

Cron example (daily 08:00 Asia/Shanghai):

```cron
0 0 * * * cd /workspace/agent-papers && /workspace/crawl4ai-venv/bin/python sync_papers.py --out /workspace/agent-papers --max-new 30 --days 3 >> /workspace/agent-papers/sync.log 2>&1
```

## Local search / 本地检索

```bash
cd /workspace/agent-papers
/workspace/crawl4ai-venv/bin/python search_papers.py "self-evolving jailbreak" --limit 10
/workspace/crawl4ai-venv/bin/python search_papers.py "agent security" --limit 5
```

Prints ranked FTS hits: title, tags, year, arxiv id, pdf path, snippet.

## Database / 数据库

- Engine: stdlib `sqlite3` + **FTS5** (`tokenize='unicode61'`, Chinese-friendly)
- Table `papers`: id, title, authors, abstract, year, tags, source_url, pdf_url, pdf_path, published, updated, first_seen, last_seen, content_hash
- Virtual table `papers_fts` over title + abstract + authors + tags

API / CLI (`db.py`):

```bash
# Bootstrap from existing manifest
/workspace/crawl4ai-venv/bin/python db.py rebuild --manifest /workspace/agent-papers/manifest.json --db /workspace/agent-papers/papers.db

/workspace/crawl4ai-venv/bin/python db.py count --db /workspace/agent-papers/papers.db
/workspace/crawl4ai-venv/bin/python db.py search "jailbreak" --limit 5
/workspace/crawl4ai-venv/bin/python db.py new 2026-09-01T00:00:00+00:00
```

Python helpers: `upsert_paper`, `rebuild_from_manifest`, `search(query, limit)`, `list_new_since(iso)`.

## Themes / 主题覆盖

Stable English keys (storage / API); Chinese labels shown in the papers UI.

| key | UI label | Meaning |
|-----|----------|---------|
| `self-evolution` | agent自进化 | self-evolving / self-improving / self-modifying / continual agents |
| `security` | agent安全 | LLM agent security, agentic safety, jailbreak, red-teaming, prompt injection |
| `security-top` | 安全顶会 | Security top-4 venues (IEEE S&P / Oakland, ACM CCS, USENIX Security, NDSS). Ingest/import only — not assigned by `tag_topics`. Optional catalog field `venue` holds the conference name shown on the card. |
| `both` | agent安全自进化 | intersection: safe/secure self-evolution & alignment of self-modifying agents |
| `llm-iot` | LLM based 物联网 | LLM/AIoT / LLM agents for IoT, edge, smart home/city, CPS (**not** generic IoT hardware) |
| `survey` | 综述 | secondary chip when title/abstract matches survey\|review\|综述 |

`llm-iot` is a **primary** when the paper is IoT+LLM and not clearly evolve/security; it can also attach as an **additional** tag on overlapping agent papers.

`security-top` is **not** assigned by daily search/`tag_topics` (keep `security` = agent安全). Official ingest sets `topic_tags` to include `security-top` and sets `venue` to `IEEE S&P`, `ACM CCS`, `USENIX Security`, or `NDSS`. The papers UI also accepts `IEEE S&P / Oakland` as the same IEEE S&P chip. The catalog card shows `venue` next to the year and topic chips.

## Official Big-4 proceedings / 安全顶会官方列表

`ingest_security_top_official.py` fills `security-top` from each year's **DBLP conference TOC HTML** (`https://dblp.org/db/conf/...`). That is the list ops already ran against the live catalog (2026-09-22: 4595 DBLP inproceedings, security-top 973 → 4854). It is not an arXiv comment heuristic. DBLP sits behind Anubis proof-of-work; the script solves that challenge and caches cookies under `{out}/cache/dblp/`.

A 404 (CCS 2026 and USENIX Security 2026 as of that run) leaves prior arXiv-tagged rows in place. Editor front-matter is skipped. Poster/demo titles, workshop titles, and workshop keys (WOOT and similar) are skipped. Papers with no arXiv id are still upserted as metadata. PDFs download only when an arXiv id is known from a DBLP `ee` link or from `--arxiv-lookup`. Paywalled IEEE/ACM PDFs stay metadata-only.

This repo copy does not itself update papers.maclaw.top. Ops run the script on the papers host; the search-service snapshot refreshes after the manifest write.

```bash
cd /workspace/agent-papers
python3 ingest_security_top_official.py --gap-report
python3 ingest_security_top_official.py --dry-run --venue sp --year 2024
python3 ingest_security_top_official.py --new-only --arxiv-lookup
python3 ingest_security_top_official.py --venue usenix --year 2023
python3 ingest_security_top_official.py --out /workspace/agent-papers
```

| Flag | Meaning |
|------|---------|
| `--venue` | `usenix` / `ccs` / `sp` / `ndss` (repeatable). Default: all four. |
| `--year` | Conference year (repeatable). Default: `--years-from` through `--years-to` (2022–2026). |
| `--gap-report` | Compare DBLP vs the catalog and exit. Writes `/workspace/sec-official-gap-report.txt` and `{out}/ingest_security_top_official_gap.json`. |
| `--dry-run` | Fetch lists and write the gap report. No DB, manifest, or PDF writes. |
| `--new-only` | Skip rows that already have `security-top` and the same venue. |
| `--arxiv-lookup` | Search arXiv by title for papers that have no arXiv id (cap `--arxiv-lookup-max`, default 80). |
| `--skip-download` | Do not download PDFs even when an arXiv id is known. |
| `--force-fetch` | Ignore the 7-day DBLP list cache. |
| `--dblp-interval` | Seconds between DBLP requests (default 4). |
| `--out DIR` | Catalog root (default `/workspace/agent-papers`). DB is `OUT/papers.db`. |

### Venue × year → DBLP key

| `--venue` | `venue` stored | TOC page |
|-----------|----------------|----------|
| `sp`, `oakland`, `ieee-sp` | `IEEE S&P` | `https://dblp.org/db/conf/sp/spYYYY.html` |
| `ccs`, `acm-ccs` | `ACM CCS` | `https://dblp.org/db/conf/ccs/ccsYYYY.html` |
| `usenix`, `uss`, `usenix-security` | `USENIX Security` | `https://dblp.org/db/conf/uss/ussYYYY.html` |
| `ndss` | `NDSS` | `https://dblp.org/db/conf/ndss/ndssYYYY.html` |

### Coverage vs DBLP

A real run writes `/workspace/sec-official-ingest-report.txt` and `{out}/ingest_security_top_official_report.json`. Per venue × year:

| Column | Meaning |
|--------|---------|
| `official` | DBLP TOC inproceedings (editor front-matter skipped) |
| `before` / `after` | `papers.db` rows tagged `security-top` with that venue and year |
| `cov%` | `100 * after / official` |

Coverage can sit a little above 100% when older arXiv-heuristic rows share the venue and year but are not on the DBLP TOC. `official == 0` means DBLP returned 404; those prior rows are kept. Open-PDF coverage stays near the existing arXiv set unless `--arxiv-lookup` finds more.

```bash
python3 -m unittest test_security_top_official.py
```

Retag existing corpus (no PDF changes):

```bash
/workspace/crawl4ai-venv/bin/python search_download_papers.py --out /workspace/agent-papers --retag
```

The public `/papers` page does **not** load `manifest.json` directly. search-service writes a lean `catalog-snapshot.json` (+ `.gz`) under this directory every `PAPERS_CATALOG_SNAPSHOT_INTERVAL` (default 60s) and after imports. First paint uses `GET /papers/api/catalog?offset=0&limit=25`, then merges the tail. Hugging Face Daily caches live under `hf-daily/`. See the repo README **Catalog snapshot regenerate / invalidate**. Do not hand-edit those snapshot files.

## Notes / 说明

- ArXiv API: sleeps ≥3s between calls; retries on rate limits; sync sorts by `lastUpdatedDate`.
- Full search **merges** with existing `manifest.json` (does not drop known papers). Rows tagged `security-top` stay in the catalog even when they are not agent-relevant and have no PDF; the search pass does not crawl their DOI pages. PDF download skips files already on disk.
- Sync loads existing ids from **DB + manifest**, keeps only unknown papers.
- Crawl4AI is used only for HTML landing pages when a direct PDF URL is missing (full search path). Prefer ArXiv PDFs.
- Relevance prefers agents + (self-evolv*/self-improv*/self-modif*/meta-learn*/continual/agentic) or (secur*/safet*/adversar*/jailbreak/alignment) or (IoT/AIoT/edge/smart-home/MQTT/CPS **paired with** LLM/agent); surveys boosted. Generic IoT-only hardware papers are dropped.
