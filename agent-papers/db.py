#!/usr/bin/env python3
"""SQLite FTS5 retrieval database for agent papers."""
from __future__ import annotations

import hashlib
import json
import sqlite3
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Optional

DEFAULT_DB = Path(__file__).resolve().parent / "papers.db"

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS papers (
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
    content_hash TEXT,
    venue TEXT
);

CREATE VIRTUAL TABLE IF NOT EXISTS papers_fts USING fts5(
    title,
    abstract,
    authors,
    tags,
    content='papers',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TRIGGER IF NOT EXISTS papers_ai AFTER INSERT ON papers BEGIN
  INSERT INTO papers_fts(rowid, title, abstract, authors, tags)
  VALUES (new.rowid, new.title, new.abstract, new.authors, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS papers_ad AFTER DELETE ON papers BEGIN
  INSERT INTO papers_fts(papers_fts, rowid, title, abstract, authors, tags)
  VALUES ('delete', old.rowid, old.title, old.abstract, old.authors, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS papers_au AFTER UPDATE ON papers BEGIN
  INSERT INTO papers_fts(papers_fts, rowid, title, abstract, authors, tags)
  VALUES ('delete', old.rowid, old.title, old.abstract, old.authors, old.tags);
  INSERT INTO papers_fts(rowid, title, abstract, authors, tags)
  VALUES (new.rowid, new.title, new.abstract, new.authors, new.tags);
END;
"""


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def content_hash(
    title: str = "",
    abstract: str = "",
    authors: str = "",
    tags: str = "",
) -> str:
    blob = f"{title}\n{authors}\n{abstract}\n{tags}".encode("utf-8")
    return hashlib.sha256(blob).hexdigest()[:32]


def paper_id_from_fields(
    arxiv_id: str = "",
    doi: str = "",
    title: str = "",
    dedupe_key: str = "",
) -> str:
    if arxiv_id:
        return arxiv_id.strip()
    if dedupe_key:
        # strip prefixes like arxiv:/doi:/title:
        if dedupe_key.startswith("arxiv:"):
            return dedupe_key[6:]
        if dedupe_key.startswith("doi:"):
            return "doi:" + dedupe_key[4:]
        if dedupe_key.startswith("title:"):
            return dedupe_key
        return dedupe_key
    if doi:
        return f"doi:{doi.lower()}"
    norm = " ".join((title or "").lower().split())
    return f"title:{hashlib.sha1(norm.encode()).hexdigest()[:16]}"


def connect(db_path: Path | str = DEFAULT_DB) -> sqlite3.Connection:
    path = Path(db_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def _ensure_venue_column(conn: sqlite3.Connection) -> None:
    """Add papers.venue on databases created before the official-list ingest."""
    cols = {row[1] for row in conn.execute("PRAGMA table_info(papers)")}
    if "venue" not in cols:
        conn.execute("ALTER TABLE papers ADD COLUMN venue TEXT")


def init_db(conn: sqlite3.Connection) -> None:
    conn.executescript(SCHEMA_SQL)
    _ensure_venue_column(conn)
    conn.commit()


def _as_str_list(val: Any) -> str:
    if val is None:
        return ""
    if isinstance(val, list):
        return "; ".join(str(x) for x in val if x)
    return str(val)


def _as_tags(val: Any) -> str:
    if val is None:
        return ""
    if isinstance(val, list):
        return "|".join(str(x) for x in val if x)
    return str(val)


def upsert_paper(conn: sqlite3.Connection, paper: dict[str, Any]) -> str:
    """Insert or update a paper. Accepts dict with Paper fields or DB fields."""
    init_db(conn)
    pid = paper.get("id") or paper_id_from_fields(
        arxiv_id=paper.get("arxiv_id") or "",
        doi=paper.get("doi") or "",
        title=paper.get("title") or "",
        dedupe_key=paper.get("dedupe_key") or "",
    )
    title = (paper.get("title") or "").strip()
    authors = _as_str_list(paper.get("authors"))
    abstract = paper.get("abstract") or ""
    tags = _as_tags(paper.get("tags") or paper.get("topic_tags"))
    year = paper.get("year")
    if year is not None:
        try:
            year = int(year)
        except (TypeError, ValueError):
            year = None
    source_url = paper.get("source_url") or ""
    pdf_url = paper.get("pdf_url") or ""
    pdf_path = paper.get("pdf_path") or ""
    published = paper.get("published") or ""
    updated = paper.get("updated") or ""
    venue = (paper.get("venue") or "").strip()
    ch = paper.get("content_hash") or content_hash(title, abstract, authors, tags)
    now = _now_iso()

    existing = conn.execute("SELECT first_seen FROM papers WHERE id = ?", (pid,)).fetchone()
    if existing:
        first_seen = existing["first_seen"] or now
        conn.execute(
            """
            UPDATE papers SET
                title=?, authors=?, abstract=?, year=?, tags=?,
                source_url=?, pdf_url=?, pdf_path=?,
                published=COALESCE(NULLIF(?, ''), published),
                updated=COALESCE(NULLIF(?, ''), updated),
                venue=COALESCE(NULLIF(?, ''), venue),
                last_seen=?, content_hash=?
            WHERE id=?
            """,
            (
                title,
                authors,
                abstract,
                year,
                tags,
                source_url,
                pdf_url,
                pdf_path,
                published,
                updated,
                venue,
                now,
                ch,
                pid,
            ),
        )
    else:
        first_seen = paper.get("first_seen") or now
        conn.execute(
            """
            INSERT INTO papers (
                id, title, authors, abstract, year, tags,
                source_url, pdf_url, pdf_path, published, updated,
                first_seen, last_seen, content_hash, venue
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                pid,
                title,
                authors,
                abstract,
                year,
                tags,
                source_url,
                pdf_url,
                pdf_path,
                published,
                updated,
                first_seen,
                now,
                ch,
                venue,
            ),
        )
    conn.commit()
    return pid


def rebuild_from_manifest(
    manifest_path: Path | str,
    db_path: Path | str = DEFAULT_DB,
) -> int:
    """Ingest all papers from manifest.json into papers.db. Returns row count."""
    manifest_path = Path(manifest_path)
    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    papers = data.get("papers") or []
    conn = connect(db_path)
    try:
        init_db(conn)
        for p in papers:
            upsert_paper(conn, p)
        n = conn.execute("SELECT COUNT(*) AS c FROM papers").fetchone()["c"]
        return int(n)
    finally:
        conn.close()


def _fts_query(raw: str) -> str:
    """Build a reasonably tolerant FTS5 query from free text."""
    raw = (raw or "").strip()
    if not raw:
        return ""
    # If user already uses FTS operators, pass through
    if any(op in raw for op in ('"', " AND ", " OR ", " NOT ", "*")):
        return raw
    tokens = [t for t in raw.replace(",", " ").split() if t]
    # Quote tokens that aren't pure alnum to avoid syntax errors; join with AND
    parts = []
    for t in tokens:
        if t.isalnum():
            parts.append(f'{t}*')
        else:
            # escape double quotes
            safe = t.replace('"', '""')
            parts.append(f'"{safe}"')
    return " AND ".join(parts)


def search(
    query: str,
    limit: int = 10,
    db_path: Path | str = DEFAULT_DB,
) -> list[dict[str, Any]]:
    """Ranked FTS search; returns list of dicts with snippet."""
    conn = connect(db_path)
    try:
        init_db(conn)
        fts_q = _fts_query(query)
        if not fts_q:
            return []
        rows = conn.execute(
            """
            SELECT
                p.id, p.title, p.authors, p.abstract, p.year, p.tags,
                p.source_url, p.pdf_url, p.pdf_path, p.published, p.updated,
                p.first_seen, p.last_seen,
                snippet(papers_fts, 1, '>>>', '<<<', '…', 24) AS snippet,
                bm25(papers_fts) AS rank
            FROM papers_fts
            JOIN papers p ON p.rowid = papers_fts.rowid
            WHERE papers_fts MATCH ?
            ORDER BY rank
            LIMIT ?
            """,
            (fts_q, limit),
        ).fetchall()
        return [dict(r) for r in rows]
    except sqlite3.OperationalError:
        # Fallback: LIKE search if FTS query is invalid
        like = f"%{query}%"
        rows = conn.execute(
            """
            SELECT id, title, authors, abstract, year, tags,
                   source_url, pdf_url, pdf_path, published, updated,
                   first_seen, last_seen,
                   substr(abstract, 1, 160) AS snippet,
                   0.0 AS rank
            FROM papers
            WHERE title LIKE ? OR abstract LIKE ? OR authors LIKE ? OR tags LIKE ?
            ORDER BY year DESC NULLS LAST, title
            LIMIT ?
            """,
            (like, like, like, like, limit),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def list_new_since(
    iso: str,
    db_path: Path | str = DEFAULT_DB,
) -> list[dict[str, Any]]:
    """Papers whose first_seen >= iso timestamp."""
    conn = connect(db_path)
    try:
        init_db(conn)
        rows = conn.execute(
            """
            SELECT * FROM papers
            WHERE first_seen >= ?
            ORDER BY first_seen DESC, title
            """,
            (iso,),
        ).fetchall()
        return [dict(r) for r in rows]
    finally:
        conn.close()


def list_all_ids(db_path: Path | str = DEFAULT_DB) -> set[str]:
    conn = connect(db_path)
    try:
        init_db(conn)
        rows = conn.execute("SELECT id FROM papers").fetchall()
        return {r["id"] for r in rows}
    finally:
        conn.close()


def count_papers(db_path: Path | str = DEFAULT_DB) -> int:
    conn = connect(db_path)
    try:
        init_db(conn)
        return int(conn.execute("SELECT COUNT(*) AS c FROM papers").fetchone()["c"])
    finally:
        conn.close()


if __name__ == "__main__":
    import argparse
    import sys

    ap = argparse.ArgumentParser(description="Papers DB utilities")
    sub = ap.add_subparsers(dest="cmd", required=True)

    def add_db(sp: argparse.ArgumentParser) -> None:
        sp.add_argument("--db", type=Path, default=DEFAULT_DB)

    b = sub.add_parser("rebuild", help="Rebuild DB from manifest.json")
    add_db(b)
    b.add_argument("--manifest", type=Path, required=True)

    s = sub.add_parser("search", help="FTS search")
    add_db(s)
    s.add_argument("query")
    s.add_argument("--limit", type=int, default=10)

    n = sub.add_parser("new", help="List papers first_seen since ISO time")
    add_db(n)
    n.add_argument("iso")

    c = sub.add_parser("count", help="Row count")
    add_db(c)

    args = ap.parse_args()
    if args.cmd == "rebuild":
        n_rows = rebuild_from_manifest(args.manifest, args.db)
        print(f"Rebuilt {args.db}: {n_rows} papers")
    elif args.cmd == "search":
        for hit in search(args.query, limit=args.limit, db_path=args.db):
            print(f"- {hit['title']} [{hit.get('tags')}] ({hit.get('year')}) id={hit['id']}")
            print(f"  {hit.get('snippet')}")
    elif args.cmd == "new":
        for hit in list_new_since(args.iso, db_path=args.db):
            print(f"- {hit['first_seen']} {hit['id']} {hit['title']}")
    elif args.cmd == "count":
        print(count_papers(args.db))
    sys.exit(0)
