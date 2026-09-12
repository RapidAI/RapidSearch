#!/usr/bin/env python3
"""CLI: search the local papers FTS database."""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

from db import DEFAULT_DB, search


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Search local agent-papers FTS database")
    p.add_argument("query", help="Search query (FTS5; plain words OK)")
    p.add_argument("--limit", type=int, default=10)
    p.add_argument("--db", type=Path, default=None)
    p.add_argument("--out", type=Path, default=Path(__file__).resolve().parent)
    args = p.parse_args(argv)

    db_path = args.db or (args.out / "papers.db")
    if not db_path.exists():
        print(f"DB not found: {db_path}", file=sys.stderr)
        print("Run: python db.py rebuild --manifest manifest.json", file=sys.stderr)
        return 1

    hits = search(args.query, limit=args.limit, db_path=db_path)
    if not hits:
        print(f"No hits for: {args.query!r}")
        return 0

    print(f"Found {len(hits)} hit(s) for: {args.query!r}\n")
    for i, h in enumerate(hits, 1):
        tags = h.get("tags") or "?"
        year = h.get("year") or "?"
        pid = h.get("id") or "?"
        pdf = h.get("pdf_path") or "(no local pdf)"
        snip = (h.get("snippet") or "").replace("\n", " ")
        print(f"{i}. {h.get('title')}")
        print(f"   tags={tags}  year={year}  id={pid}")
        print(f"   pdf={pdf}")
        if snip:
            print(f"   snippet: {snip}")
        print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
