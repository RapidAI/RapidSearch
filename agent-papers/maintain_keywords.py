#!/usr/bin/env python3
"""Mine on-topic ArXiv search keywords from the local agent-papers corpus.

Stay on topic: agent self-evolution / self-improving LLM agents / agent security /
agentic safety / red-teaming / jailbreak-for-agents / LLM-based IoT (AIoT).
Every auto query is gated with agent/LLM-agent framing so ambiguous terms never
search general ML or generic IoT hardware alone.
"""
from __future__ import annotations

import argparse
import json
import re
import sqlite3
import sys
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable, Optional
from zoneinfo import ZoneInfo

from search_download_papers import (
    AGENT_RE,
    DEFAULT_QUERIES,
    EVOLVE_RE,
    IOT_RE,
    LLM_RE,
    SECURITY_RE,
    SURVEY_RE,
    tag_topics,
)

KEYWORDS_FILENAME = "search_keywords.json"
DEFAULT_MERGED_CAP = 42
DEFAULT_AUTO_CAP = 16
MIN_TERM_COUNT = 2
MIN_TITLE_COUNT_FOR_UNIGRAM = 3

# Generic CS / ML noise — never emit alone; demote heavily if they slip through n-grams.
STOPWORDS = frozenset(
    """
    a an the and or of to in on for with by from as at is are was were be been being
    this that these those it its their our your we you they he she his her them
    into over under about between through during before after above below
    not no nor so if then than when while where which who whom what how why
    can could may might must shall should will would do does did done
    using based via using using using using
    paper papers study studies work works method methods approach approaches
    model models modeling modelling system systems framework frameworks
    data dataset datasets result results analysis research experiment experiments
    propose proposed proposes proposing present presents presented show shows shown
    new novel state art sota large scale high low performance based based
    general task tasks problem problems application applications domain domains
    human humans user users world real time multi single end to end
    """.split()
)

GENERIC_CS = frozenset(
    """
    neural network networks learning deep machine artificial intelligence ai
    transformer transformers attention embedding embeddings vector vectors
    training train trained inference optimization optimize optimized gradient
    loss function functions algorithm algorithms architecture architectures
    computer vision cv nlp natural language processing classification regression
    clustering representation representations feature features layer layers
    reinforcement rl markov policy policies reward rewards q-learning
    dataset benchmark benchmarks evaluation evaluate evaluating metric metrics
    pytorch tensorflow cuda gpu cpu parallel distributed cloud software code
    python java github open source opensource
    """.split()
)

# Ambiguous unigrams/bigrams that MUST be paired with agent/LLM in the query.

# Reject n-grams that start/end with these (keeps "self-evolving agents", drops "survey of")
EDGE_STOP = frozenset(
    """
    a an the and or of to in on for with by from as at is are was were be
    this that these those into over under about between through via using
    based based based new novel our their its such both more most other
    into onto upon over under within without across
    """.split()
)

PREPOSITIONAL_WEAK = frozenset(
    {
        "survey of",
        "review of",
        "overview of",
        "for agent",
        "for agents",
        "for llm",
        "for llms",
        "in llm",
        "in llms",
        "of agent",
        "of agents",
        "of llm",
        "and agent",
        "and agents",
        "the agent",
        "an agent",
        "a survey",
        "a framework",
        "a comprehensive",
        "comprehensive survey",  # prefer fuller topical phrases from titles
    }
)

# Unigrams too broad even with agent gate — do not emit as auto queries.
BROAD_UNIGRAMS = frozenset(
    {
        "agent",
        "agents",
        "llm",
        "llms",
        "model",
        "models",
        "system",
        "systems",
        "framework",
        "survey",
        "review",
        "learning",
        "language",
        "artificial",
        "intelligence",
        "iot",
        "aiot",
        "mqtt",
    }
)


AMBIGUOUS_ALONE = frozenset(
    {
        "evolution",
        "evolve",
        "evolving",
        "security",
        "safety",
        "secure",
        "safe",
        "alignment",
        "aligned",
        "adversarial",
        "attack",
        "defense",
        "defence",
        "jailbreak",
        "red team",
        "red-teaming",
        "red teaming",
        "prompt injection",
        "continual learning",
        "meta-learning",
        "meta learning",
        "self-improve",
        "self-improving",
        "self-evolution",
        "self-evolving",
        "self-modifying",
        "open-ended",
        "survey",
        "review",
        "overview",
        "robustness",
        "privacy",
        "governance",
        "isolation",
        "guardrails",
        "guardrail",
        "harness",
        "benchmark",
        "framework",
        "iot",
        "aiot",
        "iiot",
        "mqtt",
        "internet of things",
        "smart home",
        "smart city",
        "edge device",
        "sensor network",
        "cyber-physical",
    }
)

# Prefer these topic anchors when scoring affinity.
TOPIC_BONUS_RE = re.compile(
    r"\b("
    r"self[\s-]?evolv\w*|self[\s-]?improv\w*|self[\s-]?modif\w*|"
    r"self[\s-]?refin\w*|self[\s-]?adapt\w*|agentic|"
    r"jailbreak\w*|red[\s-]?team\w*|prompt[\s-]?injection|"
    r"agent[\s-]?safet\w*|agent[\s-]?secur\w*|llm[\s-]?agent|"
    r"language[\s-]?agent|multi[\s-]?agent|tool[\s-]?using|"
    r"continual[\s-]?improv\w*|open[\s-]?ended[\s-]?evolution|"
    r"agentic[\s-]?reinforcement|secure[\s-]?self|safe[\s-]?self|"
    r"aiot|internet[\s-]?of[\s-]?things|llm[\s-]?iot|cyber[\s-]?physical"
    r")\b",
    re.I,
)

TOKEN_RE = re.compile(r"[a-z0-9]+(?:[-'][a-z0-9]+)*", re.I)
HYPHEN_KEEP = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)+$", re.I)


def log(msg: str) -> None:
    ts = datetime.now().strftime("%H:%M:%S")
    print(f"[{ts}] {msg}", flush=True)


def _norm_space(s: str) -> str:
    return re.sub(r"\s+", " ", (s or "").strip())


def tokenize(text: str) -> list[str]:
    return [t.lower() for t in TOKEN_RE.findall(text or "")]


def ngrams(tokens: list[str], n: int) -> list[str]:
    if n <= 0 or len(tokens) < n:
        return []
    return [" ".join(tokens[i : i + n]) for i in range(len(tokens) - n + 1)]


def is_stop_or_generic(term: str) -> bool:
    parts = term.lower().split()
    if not parts:
        return True
    if all(p in STOPWORDS or p in GENERIC_CS for p in parts):
        return True
    if len(parts) == 1 and (parts[0] in STOPWORDS or parts[0] in GENERIC_CS):
        return True
    # pure numeric / too short
    if all(re.fullmatch(r"\d+", p) for p in parts):
        return True
    if any(len(p) < 2 for p in parts):
        return True
    if len(parts) >= 2 and (parts[0] in EDGE_STOP or parts[-1] in EDGE_STOP):
        return True
    if term.lower() in PREPOSITIONAL_WEAK:
        return True
    # mostly stopwords (e.g. "and guardrail for")
    content = [p for p in parts if p not in STOPWORDS and p not in EDGE_STOP]
    if len(parts) >= 2 and len(content) < max(1, len(parts) - 1) and len(content) <= 1:
        # allow "self-evolving agents" etc. when content words carry topic signal
        joined = " ".join(content)
        if not (TOPIC_BONUS_RE.search(joined) or AGENT_RE.search(joined)
                or EVOLVE_RE.search(joined) or SECURITY_RE.search(joined)):
            return True
    return False


def paper_on_topic(title: str, abstract: str, tags: list[str]) -> bool:
    text = f"{title}\n{abstract}"
    has_agent = bool(AGENT_RE.search(text))
    has_llm = bool(LLM_RE.search(text))
    evo = bool(EVOLVE_RE.search(text))
    sec = bool(SECURITY_RE.search(text))
    has_iot = bool(IOT_RE.search(text))
    if tags:
        tagset = {t.lower() for t in tags}
        if tagset & {"self-evolution", "security", "both", "survey", "llm-iot"}:
            # still require agent or LLM framing to avoid pure ML / generic IoT
            return has_agent or has_llm
    return (has_agent or has_llm) and (evo or sec or has_iot)


def load_papers(out: Path, db_path: Path) -> list[dict]:
    papers: dict[str, dict] = {}

    if db_path.exists():
        conn = sqlite3.connect(db_path)
        conn.row_factory = sqlite3.Row
        try:
            rows = conn.execute(
                "SELECT id, title, abstract, tags FROM papers ORDER BY id"
            ).fetchall()
        finally:
            conn.close()
        for r in rows:
            tags_raw = r["tags"] or ""
            if isinstance(tags_raw, str) and tags_raw.startswith("["):
                try:
                    tags = json.loads(tags_raw)
                except Exception:
                    tags = [t for t in tags_raw.replace("|", ",").split(",") if t.strip()]
            else:
                tags = [t for t in str(tags_raw).replace("|", ",").split(",") if t.strip()]
            papers[str(r["id"])] = {
                "id": str(r["id"]),
                "title": r["title"] or "",
                "abstract": r["abstract"] or "",
                "tags": tags,
            }

    manifest = out / "manifest.json"
    if manifest.exists():
        data = json.loads(manifest.read_text(encoding="utf-8"))
        for d in data.get("papers") or []:
            aid = (d.get("arxiv_id") or "").strip() or d.get("dedupe_key") or d.get("title", "")[:40]
            if aid in papers:
                # fill missing abstract/tags from manifest if needed
                if not papers[aid].get("abstract") and d.get("abstract"):
                    papers[aid]["abstract"] = d.get("abstract") or ""
                if not papers[aid].get("tags") and d.get("topic_tags"):
                    papers[aid]["tags"] = list(d.get("topic_tags") or [])
                continue
            papers[str(aid)] = {
                "id": str(aid),
                "title": d.get("title") or "",
                "abstract": d.get("abstract") or "",
                "tags": list(d.get("topic_tags") or tag_topics(d.get("title") or "", d.get("abstract") or "")),
            }

    return [papers[k] for k in sorted(papers.keys())]


def topic_affinity(term: str, title_hits: int, abs_hits: int, tag_counter: Counter) -> float:
    """Higher = more on-topic. Prefer multi-word + theme co-occurrence."""
    score = 0.0
    parts = term.split()
    n = len(parts)
    if n >= 2:
        score += 2.0 + 0.5 * min(n, 4)
    else:
        score += 0.5  # unigrams start weak

    if TOPIC_BONUS_RE.search(term):
        score += 3.0
    if AGENT_RE.search(term):
        score += 2.0
    if EVOLVE_RE.search(term):
        score += 2.5
    if SECURITY_RE.search(term):
        score += 2.5
    if LLM_RE.search(term):
        score += 1.5
    if IOT_RE.search(term) and (LLM_RE.search(term) or AGENT_RE.search(term)):
        score += 2.5
    elif IOT_RE.search(term):
        score += 1.0  # IoT alone is weak; must pair via agent gate
    if SURVEY_RE.search(term) and n >= 2:
        score += 0.5

    # frequency: titles weigh more than abstracts
    score += title_hits * 2.0 + abs_hits * 0.35

    # tag affinity from papers that contained the term
    for tag, c in tag_counter.items():
        if tag in ("self-evolution", "both"):
            score += 0.4 * c
        elif tag == "security":
            score += 0.4 * c
        elif tag == "llm-iot":
            score += 0.4 * c
        elif tag == "survey":
            score += 0.1 * c

    # demote generics / ambiguous alone
    if is_stop_or_generic(term):
        score -= 10.0
    if term in AMBIGUOUS_ALONE or (n == 1 and term in AMBIGUOUS_ALONE):
        score -= 1.0  # still usable if paired later
    if any(p in GENERIC_CS for p in parts) and not (
        AGENT_RE.search(term) or EVOLVE_RE.search(term) or SECURITY_RE.search(term)
    ):
        score -= 4.0

    return score


def mine_terms(papers: list[dict]) -> list[dict]:
    title_counts: Counter = Counter()
    abs_counts: Counter = Counter()
    tag_by_term: dict[str, Counter] = defaultdict(Counter)
    paper_ids_by_term: dict[str, set] = defaultdict(set)

    on_topic = [p for p in papers if paper_on_topic(p["title"], p["abstract"], p.get("tags") or [])]
    for p in on_topic:
        pid = p["id"]
        tags = [t.lower() for t in (p.get("tags") or [])]
        title_toks = tokenize(p["title"])
        # abstracts lightly: only first ~80 tokens to avoid noise
        abs_toks = tokenize(p["abstract"])[:80]

        title_phrases: set[str] = set()
        for n in (4, 3, 2, 1):
            for g in ngrams(title_toks, n):
                title_phrases.add(g)
        abs_phrases: set[str] = set()
        for n in (3, 2):  # no unigrams from abstract alone
            for g in ngrams(abs_toks, n):
                abs_phrases.add(g)

        for phrase in title_phrases:
            if is_stop_or_generic(phrase):
                continue
            title_counts[phrase] += 1
            paper_ids_by_term[phrase].add(pid)
            for t in tags:
                tag_by_term[phrase][t] += 1

        for phrase in abs_phrases:
            if is_stop_or_generic(phrase):
                continue
            # abstract-only: require some topic signal in the phrase itself
            if not (
                TOPIC_BONUS_RE.search(phrase)
                or AGENT_RE.search(phrase)
                or EVOLVE_RE.search(phrase)
                or SECURITY_RE.search(phrase)
                or LLM_RE.search(phrase)
                or IOT_RE.search(phrase)
            ):
                continue
            abs_counts[phrase] += 1
            paper_ids_by_term[phrase].add(pid)
            for t in tags:
                tag_by_term[phrase][t] += 1

    candidates: list[dict] = []
    all_terms = set(title_counts) | set(abs_counts)
    for term in sorted(all_terms):
        tc = title_counts.get(term, 0)
        ac = abs_counts.get(term, 0)
        total = len(paper_ids_by_term.get(term, set()))
        parts = term.split()
        if len(parts) == 1:
            # high-signal unigrams only, from titles, with enough support
            if tc < MIN_TITLE_COUNT_FOR_UNIGRAM:
                continue
            if not (
                TOPIC_BONUS_RE.search(term)
                or EVOLVE_RE.search(term)
                or SECURITY_RE.search(term)
                or IOT_RE.search(term)
                or term in {"jailbreak", "agentic", "agent", "agents"}
            ):
                continue
            if term in GENERIC_CS or term in STOPWORDS:
                continue
        else:
            if total < MIN_TERM_COUNT and tc < 1:
                continue
            # multi-word: need at least one title hit OR strong topic regex
            if tc < 1 and not (
                TOPIC_BONUS_RE.search(term)
                or EVOLVE_RE.search(term)
                or SECURITY_RE.search(term)
                or IOT_RE.search(term)
            ):
                continue

        aff = topic_affinity(term, tc, ac, tag_by_term[term])
        if aff < 3.5:
            continue

        tags_sorted = [t for t, _ in tag_by_term[term].most_common(4)]
        candidates.append(
            {
                "term": term,
                "count": total,
                "title_count": tc,
                "abstract_count": ac,
                "score": round(aff, 3),
                "tags": tags_sorted,
                "ambiguous": term in AMBIGUOUS_ALONE or (
                    len(parts) == 1 and parts[0] in AMBIGUOUS_ALONE
                ),
            }
        )

    # Prefer multi-word; deterministic sort
    candidates.sort(key=lambda x: (-x["score"], -x["count"], -len(x["term"].split()), x["term"]))
    return candidates


def agent_gate_clause() -> str:
    """Required framing so ArXiv hits stay agent/LLM-agent scoped."""
    return '(all:"LLM agent" OR all:"language agent" OR all:agentic OR ti:agent OR all:"multi-agent")'


def theme_clause_for_term(term: str, tags: list[str]) -> Optional[str]:
    """Extra theme OR-group when the term itself is not clearly evolve/security/IoT.

    Ambiguous terms still always get agent/LLM framing via agent_gate_clause(); this
    adds evolve/security only when the term alone would otherwise match generic agents.
    IoT phrases are never emitted bare — the agent/LLM gate is required, and the term
    itself already carries the IoT theme (do not add a second generic IoT OR).
    """
    has_evo = bool(EVOLVE_RE.search(term))
    has_sec = bool(SECURITY_RE.search(term))
    has_iot = bool(IOT_RE.search(term))
    # Strong theme words (self-evolving, jailbreak, AIoT, …): agent gate is enough.
    if has_evo or has_sec or has_iot:
        return None
    tagset = set(tags)
    if "llm-iot" in tagset and not (tagset & {"self-evolution", "security", "both"}):
        return '(all:IoT OR all:AIoT OR all:"Internet of Things")'
    if "security" in tagset and "self-evolution" not in tagset and "both" not in tagset:
        return '(all:security OR all:safety OR all:jailbreak OR all:"red teaming" OR all:adversarial)'
    if "self-evolution" in tagset and "security" not in tagset:
        return '(all:"self-evolving" OR all:"self-improving" OR all:"self-modifying" OR all:evolve)'
    if "both" in tagset:
        return (
            '(all:"self-evolving" OR all:"self-improving" OR all:security OR all:safety '
            'OR all:jailbreak OR all:agentic)'
        )
    # Default: require a theme so we do not drift to generic agent papers
    return (
        '(all:"self-evolving" OR all:"self-improving" OR all:"self-modifying" '
        'OR all:security OR all:safety OR all:jailbreak OR all:"red teaming")'
    )

def quote_arxiv_phrase(term: str) -> str:
    term = _norm_space(term)
    if " " in term or "-" in term:
        return f'all:"{term}"'
    return f"all:{term}"


def build_query_for_term(item: dict) -> str:
    term = item["term"]
    tags = item.get("tags") or []
    phrase = quote_arxiv_phrase(term)
    gate = agent_gate_clause()
    theme = theme_clause_for_term(term, tags)

    # Title-preferring form for strong multi-word phrases
    parts = term.split()
    if len(parts) >= 2 and item.get("title_count", 0) >= 2 and not item.get("ambiguous"):
        ti = f'ti:"{term}"'
        core = f"({ti} OR {phrase})"
    else:
        core = phrase

    clauses = [core, gate]
    if theme:
        clauses.append(theme)
    return " AND ".join(clauses)


def dedupe_queries(queries: Iterable[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for q in queries:
        key = re.sub(r"\s+", " ", q.strip().lower())
        if not key or key in seen:
            continue
        seen.add(key)
        out.append(q.strip())
    return out


def select_auto_queries(terms: list[dict], auto_cap: int) -> tuple[list[str], list[dict]]:
    """Build auto queries from top terms; prefer precision (multi-word, high affinity)."""
    chosen_terms: list[dict] = []
    queries: list[str] = []
    used_roots: set[str] = set()

    for item in terms:
        if len(queries) >= auto_cap:
            break
        term = item["term"]
        parts = term.split()
        if is_stop_or_generic(term) or term in PREPOSITIONAL_WEAK:
            continue
        if len(parts) == 1:
            if term in BROAD_UNIGRAMS:
                continue
            # Only high-signal theme unigrams (self-evolving, jailbreak, agentic, …)
            if item["score"] < 12.0 or item.get("title_count", 0) < MIN_TITLE_COUNT_FOR_UNIGRAM:
                continue
            if not (
                EVOLVE_RE.search(term)
                or SECURITY_RE.search(term)
                or TOPIC_BONUS_RE.search(term)
            ):
                continue
        else:
            # Precision first: need support in >=2 on-topic papers.
            # Rare exception: exact high-signal 2-grams like "open-ended evolution".
            if item["count"] < 2:
                if len(parts) != 2:
                    continue
                if not (
                    (EVOLVE_RE.search(term) or SECURITY_RE.search(term) or IOT_RE.search(term))
                    and (AGENT_RE.search(term) or LLM_RE.search(term) or TOPIC_BONUS_RE.search(term))
                ):
                    continue
            # Drop phrases with internal stopwords (title debris): "principle for llm-agent"
            if len(parts) >= 3 and any(p in EDGE_STOP or p in STOPWORDS for p in parts[1:-1]):
                continue
        # Avoid near-duplicates: skip if a longer chosen phrase contains this or vice versa
        skip = False
        for prev in list(used_roots):
            if term in prev or prev in term:
                skip = True
                break
        if skip:
            continue

        q = build_query_for_term(item)
        # Hard gate: every auto query must AND with agent/LLM-agent framing
        if "LLM agent" not in q and "language agent" not in q and "agentic" not in q:
            continue
        if " AND " not in q:
            continue
        queries.append(q)
        chosen_terms.append(item)
        used_roots.add(term)

    return queries, chosen_terms


def maintain(
    out: Path,
    db_path: Path,
    merged_cap: int = DEFAULT_MERGED_CAP,
    auto_cap: int = DEFAULT_AUTO_CAP,
) -> dict:
    papers = load_papers(out, db_path)
    log(f"Loaded {len(papers)} papers from DB/manifest")
    on_topic_n = sum(1 for p in papers if paper_on_topic(p["title"], p["abstract"], p.get("tags") or []))
    log(f"On-topic for mining: {on_topic_n}")

    terms = mine_terms(papers)
    log(f"Candidate terms after gates: {len(terms)}")

    auto_queries, chosen = select_auto_queries(terms, auto_cap=auto_cap)
    seed = list(DEFAULT_QUERIES)
    merged = dedupe_queries(list(seed) + auto_queries)[:merged_cap]

    # Persist top terms (not only ones that became queries) for inspection
    terms_out = [
        {
            "term": t["term"],
            "count": t["count"],
            "title_count": t["title_count"],
            "score": t["score"],
            "tags": t["tags"],
            "ambiguous": t["ambiguous"],
            "selected": t["term"] in {c["term"] for c in chosen},
        }
        for t in terms[:80]
    ]

    payload = {
        "updated_at": datetime.now(timezone.utc).isoformat(),
        "updated_at_local": datetime.now(timezone.utc)
        .astimezone(ZoneInfo("Asia/Shanghai"))
        .strftime("%Y-%m-%d %H:%M %Z"),
        "corpus_size": len(papers),
        "on_topic_size": on_topic_n,
        "seed_queries": seed,
        "auto_queries": auto_queries,
        "terms": terms_out,
        "merged_queries": merged,
        "caps": {"merged": merged_cap, "auto": auto_cap},
        "gates": {
            "require_agent_or_llm_framing": True,
            "ambiguous_must_pair_agent": True,
            "ban_generic_ml_only": True,
            "ban_generic_iot_without_llm": True,
            "min_term_count": MIN_TERM_COUNT,
            "min_title_count_unigram": MIN_TITLE_COUNT_FOR_UNIGRAM,
        },
    }

    out_path = out / KEYWORDS_FILENAME
    out_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    log(f"Wrote {out_path}")
    log(f"terms={len(terms_out)} auto_queries={len(auto_queries)} merged={len(merged)}")
    return payload


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    default_out = Path(__file__).resolve().parent
    p = argparse.ArgumentParser(description="Maintain ArXiv search keywords from paper corpus")
    p.add_argument("--out", type=Path, default=default_out, help="Corpus / output directory")
    p.add_argument("--db", type=Path, default=None, help="SQLite DB (default OUT/papers.db)")
    p.add_argument("--merged-cap", type=int, default=DEFAULT_MERGED_CAP)
    p.add_argument("--auto-cap", type=int, default=DEFAULT_AUTO_CAP)
    return p.parse_args(argv)


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    db_path = args.db or (args.out / "papers.db")
    payload = maintain(args.out, db_path, merged_cap=args.merged_cap, auto_cap=args.auto_cap)
    # Summary for operators
    print("--- summary ---")
    print(f"corpus={payload['corpus_size']} on_topic={payload['on_topic_size']}")
    print(f"terms={len(payload['terms'])} auto_queries={len(payload['auto_queries'])} "
          f"merged={len(payload['merged_queries'])}")
    print("sample auto_queries:")
    for q in payload["auto_queries"][:10]:
        print(f"  - {q}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
