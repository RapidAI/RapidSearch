#!/usr/bin/env python3
"""Incremental sync: fetch only NEW ArXiv papers into local DB + manifest."""
from __future__ import annotations

import argparse
import json
import sys
import time
from dataclasses import asdict
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo
from pathlib import Path
from typing import Optional

# Local imports
from db import (
    DEFAULT_DB,
    connect,
    count_papers,
    init_db,
    list_all_ids,
    paper_id_from_fields,
    upsert_paper,
)
from search_download_papers import (
    DEFAULT_QUERIES,
    Paper,
    arxiv_query,
    download_pdf,
    ensure_dirs,
    is_relevant,
    log,
    merge_papers,
    tag_topics,
    write_index,
    write_manifest,
    AGENT_RE,
    LLM_RE,
)


def load_manifest_papers(manifest_path: Path) -> dict[str, Paper]:
    if not manifest_path.exists():
        return {}
    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    out: dict[str, Paper] = {}
    for d in data.get("papers") or []:
        p = Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})
        out[p.dedupe_key()] = p
    return out


def existing_ids(out: Path, db_path: Path) -> set[str]:
    ids = set(list_all_ids(db_path))
    manifest = out / "manifest.json"
    if manifest.exists():
        data = json.loads(manifest.read_text(encoding="utf-8"))
        for d in data.get("papers") or []:
            aid = (d.get("arxiv_id") or "").strip()
            if aid:
                ids.add(aid)
            else:
                p = Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})
                ids.add(
                    paper_id_from_fields(
                        arxiv_id=p.arxiv_id,
                        doi=p.doi,
                        title=p.title,
                        dedupe_key=p.dedupe_key(),
                    )
                )
    return ids


def paper_already_known(p: Paper, known: set[str]) -> bool:
    pid = paper_id_from_fields(
        arxiv_id=p.arxiv_id,
        doi=p.doi,
        title=p.title,
        dedupe_key=p.dedupe_key(),
    )
    if pid in known:
        return True
    if p.arxiv_id and p.arxiv_id in known:
        return True
    return False


def submitted_window(days: int) -> tuple[str, str]:
    now = datetime.now(timezone.utc)
    start = now - timedelta(days=days)
    # ArXiv expects YYYYMMDDHHMM
    return start.strftime("%Y%m%d%H%M"), now.strftime("%Y%m%d%H%M")


def filter_relevant(pool: dict[str, Paper]) -> list[Paper]:
    candidates = [p for p in pool.values() if is_relevant(p)]
    if len(candidates) < 3:
        candidates = [
            p
            for p in pool.values()
            if p.score >= 2.0
            and (
                AGENT_RE.search(p.title + " " + p.abstract)
                or LLM_RE.search(p.title + " " + p.abstract)
            )
            and (p.topic_tags or tag_topics(p.title, p.abstract))
        ]
        for p in candidates:
            if not p.topic_tags:
                p.topic_tags = tag_topics(p.title, p.abstract) or ["self-evolution"]
    candidates.sort(key=lambda x: (-(x.year or 0), -x.score, x.title))
    return candidates


def merge_manifest_and_write(
    out: Path,
    existing: dict[str, Paper],
    newcomers: list[Paper],
    sync_stats: dict,
) -> list[Paper]:
    merged = dict(existing)
    for p in newcomers:
        key = p.dedupe_key()
        merged[key] = p
    papers = list(merged.values())
    papers.sort(key=lambda x: (-(x.year or 0), -x.score, x.title))
    stats = {
        "searched": sync_stats.get("searched", 0),
        "pool_unique": len(merged),
        "kept": len(papers),
        "downloaded": sync_stats.get("downloaded", 0),
        "skipped": sync_stats.get("skipped", 0),
        "failures": sync_stats.get("failures", 0),
        "theme_counts": {},
        "dry_run": sync_stats.get("dry_run", False),
        "sync": True,
        "new_this_sync": sync_stats.get("new_count", 0),
    }
    for p in papers:
        for t in p.topic_tags:
            stats["theme_counts"][t] = stats["theme_counts"].get(t, 0) + 1
    write_manifest(out, papers, stats)
    write_index(out, papers, stats)
    return papers


def write_sync_reports(
    out: Path,
    new_papers: list[Paper],
    failures: list[dict],
    stats: dict,
) -> None:
    report = {
        "synced_at": datetime.now(timezone.utc).isoformat(),
        "new_count": len(new_papers),
        "titles": [p.title for p in new_papers],
        "ids": [
            paper_id_from_fields(arxiv_id=p.arxiv_id, doi=p.doi, title=p.title, dedupe_key=p.dedupe_key())
            for p in new_papers
        ],
        "failures": failures,
        "stats": stats,
    }
    (out / "sync_report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    lines = [
        "# Sync latest",
        "",
        f"- Time: {datetime.now(timezone.utc).astimezone(ZoneInfo('Asia/Shanghai')).strftime('%Y-%m-%d %H:%M %Z')}",
        f"- New papers: **{len(new_papers)}**",
        f"- Failures: **{len(failures)}**",
        f"- Dry-run: {stats.get('dry_run', False)}",
        "",
    ]
    if new_papers:
        lines.append("## New titles")
        lines.append("")
        for i, p in enumerate(new_papers, 1):
            tags = ", ".join(p.topic_tags) or "?"
            lines.append(f"{i}. **{p.title}** ({p.year or '?'}) — {tags}")
            if p.arxiv_id:
                lines.append(f"   - ArXiv: `{p.arxiv_id}`")
            if p.pdf_path:
                lines.append(f"   - PDF: `{p.pdf_path}`")
        lines.append("")
    if failures:
        lines.append("## Failures")
        lines.append("")
        for f in failures:
            lines.append(f"- {f.get('title', '?')}: {f.get('error', '?')}")
        lines.append("")
    if not new_papers and not failures:
        lines.append("_No new papers in this window._")
        lines.append("")
    (out / "sync_latest.md").write_text("\n".join(lines), encoding="utf-8")


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    default_out = Path(__file__).resolve().parent
    p = argparse.ArgumentParser(description="Incrementally sync NEW agent papers")
    p.add_argument("--out", type=Path, default=default_out)
    p.add_argument("--db", type=Path, default=None, help="SQLite DB path (default: OUT/papers.db)")
    p.add_argument("--max-new", type=int, default=30, help="Max new papers to keep/download")
    p.add_argument("--days", type=int, default=7, help="Lookback days for ArXiv submittedDate")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--queries", type=str, default="", help="; -separated ArXiv query overrides")
    p.add_argument("--per-query", type=int, default=15, help="max_results per ArXiv query")
    return p.parse_args(argv)


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    paths = ensure_dirs(args.out)
    db_path = args.db or (args.out / "papers.db")
    queries = (
        [q.strip() for q in args.queries.split(";") if q.strip()]
        if args.queries
        else list(DEFAULT_QUERIES)
    )

    # Ensure DB exists / schema ready
    conn = connect(db_path)
    init_db(conn)
    conn.close()

    known = existing_ids(args.out, db_path)
    log(f"Known paper ids (DB+manifest): {len(known)}")
    log(f"DB rows before: {count_papers(db_path)}")

    lo, hi = submitted_window(args.days)
    log(f"ArXiv submittedDate window: {lo} → {hi} (days={args.days})")

    pool: dict[str, Paper] = {}
    searched = 0
    for i, q in enumerate(queries, 1):
        log(f"ArXiv sync [{i}/{len(queries)}]: {q[:90]}…")
        papers = arxiv_query(
            q,
            paths["cache"],
            max_results=args.per_query,
            sort_by="lastUpdatedDate",
            sort_order="descending",
            submitted_from=lo,
            submitted_to=hi,
        )
        searched += len(papers)
        # Keep only unknown
        fresh = [p for p in papers if not paper_already_known(p, known)]
        n = merge_papers(pool, fresh, f"sync-arxiv:{i}")
        log(f"  got {len(papers)}, unknown {len(fresh)}, merged new {n}, pool={len(pool)}")

    candidates = filter_relevant(pool)
    selected = candidates[: args.max_new]
    log(f"Selected {len(selected)} new relevant (candidates={len(candidates)}, searched={searched})")

    failures: list[dict] = []
    downloaded = skipped = fail_n = 0
    for i, p in enumerate(selected, 1):
        if p.arxiv_id and not p.pdf_url:
            p.pdf_url = f"https://arxiv.org/pdf/{p.arxiv_id}.pdf"
        log(f"PDF [{i}/{len(selected)}]: {p.title[:70]}…")
        if not args.dry_run:
            download_pdf(p, paths["pdfs"], dry_run=False)
        else:
            download_pdf(p, paths["pdfs"], dry_run=True)
        if p.download_status == "ok":
            downloaded += 1
        elif p.download_status in ("skipped", "dry-run"):
            skipped += 1
        else:
            fail_n += 1
            failures.append(
                {
                    "title": p.title,
                    "arxiv_id": p.arxiv_id,
                    "error": p.download_status,
                }
            )
        time.sleep(1.0)

    sync_stats = {
        "searched": searched,
        "downloaded": downloaded,
        "skipped": skipped,
        "failures": fail_n,
        "dry_run": args.dry_run,
        "new_count": len(selected),
        "days": args.days,
        "max_new": args.max_new,
        "db_rows": count_papers(db_path),
    }

    if args.dry_run:
        log("Dry-run: skipping DB upsert / manifest merge (reports only)")
        write_sync_reports(args.out, selected, failures, sync_stats)
    else:
        conn = connect(db_path)
        try:
            for p in selected:
                upsert_paper(conn, {**asdict(p), "tags": p.topic_tags})
        finally:
            conn.close()
        existing_manifest = load_manifest_papers(args.out / "manifest.json")
        merge_manifest_and_write(args.out, existing_manifest, selected, sync_stats)
        sync_stats["db_rows"] = count_papers(db_path)
        write_sync_reports(args.out, selected, failures, sync_stats)

    log("=== SYNC DONE ===")
    log(json.dumps(sync_stats, indent=2))
    log(f"sync_report: {args.out / 'sync_report.json'}")
    log(f"sync_latest: {args.out / 'sync_latest.md'}")
    log(f"db: {db_path} ({sync_stats['db_rows']} rows)")
    return 0  # always 0 for cron


if __name__ == "__main__":
    sys.exit(main())
