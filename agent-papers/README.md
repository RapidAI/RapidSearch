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
| `both` | agent安全自进化 | intersection: safe/secure self-evolution & alignment of self-modifying agents |
| `llm-iot` | LLM based 物联网 | LLM/AIoT / LLM agents for IoT, edge, smart home/city, CPS (**not** generic IoT hardware) |
| `survey` | 综述 | secondary chip when title/abstract matches survey\|review\|综述 |

`llm-iot` is a **primary** when the paper is IoT+LLM and not clearly evolve/security; it can also attach as an **additional** tag on overlapping agent papers.

Retag existing corpus (no PDF changes):

```bash
/workspace/crawl4ai-venv/bin/python search_download_papers.py --out /workspace/agent-papers --retag
```

The public `/papers` page does **not** load `manifest.json` directly. search-service writes a lean `catalog-snapshot.json` (+ `.gz`) under this directory every `PAPERS_CATALOG_SNAPSHOT_INTERVAL` (default 60s) and after imports. See the repo README **Catalog snapshot regenerate / invalidate**. Do not hand-edit those snapshot files.

## Notes / 说明

- ArXiv API: sleeps ≥3s between calls; retries on rate limits; sync sorts by `lastUpdatedDate`.
- Full search **merges** with existing `manifest.json` (does not drop known papers); PDF download skips files already on disk.
- Sync loads existing ids from **DB + manifest**, keeps only unknown papers.
- Crawl4AI is used only for HTML landing pages when a direct PDF URL is missing (full search path). Prefer ArXiv PDFs.
- Relevance prefers agents + (self-evolv*/self-improv*/self-modif*/meta-learn*/continual/agentic) or (secur*/safet*/adversar*/jailbreak/alignment) or (IoT/AIoT/edge/smart-home/MQTT/CPS **paired with** LLM/agent); surveys boosted. Generic IoT-only hardware papers are dropped.
