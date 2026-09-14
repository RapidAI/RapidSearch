#!/usr/bin/env python3
"""
Search & download academic papers on agent self-evolution, agent security,
LLM-based IoT (AIoT), LLM training, and agent tools/memory.
Primary source: ArXiv API. Optional: Semantic Scholar, local RapidSearch + Crawl4AI.

Stable topic_tags keys: self-evolution | security | both | llm-iot | survey
    | llm-training | agent-tools-memory | other
Auto search/sync tags llm-training and agent-tools-memory. `other` is
manual-import only. Manual imports set source=manual and keep the user-chosen tag.
There is no checked-in search_keywords.json — daily exhaust uses DEFAULT_QUERIES
and/or a runtime file produced by maintain_keywords.py (seed = DEFAULT_QUERIES).
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import time
import xml.etree.ElementTree as ET
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable, Optional
from urllib.parse import quote_plus, urlparse

# Prefer httpx; fall back to requests
try:
    import httpx

    def _get(url: str, **kw):
        timeout = kw.pop("timeout", 60.0)
        headers = kw.pop("headers", None) or {}
        with httpx.Client(follow_redirects=True, timeout=timeout, headers=headers) as c:
            return c.get(url, **kw)

    def _get_bytes(url: str, **kw) -> tuple[int, bytes, str]:
        timeout = kw.pop("timeout", 120.0)
        headers = kw.pop("headers", None) or {
            "User-Agent": "Mozilla/5.0 (compatible; AgentPapersBot/1.0; research)"
        }
        with httpx.Client(follow_redirects=True, timeout=timeout, headers=headers) as c:
            r = c.get(url, **kw)
            return r.status_code, r.content, r.headers.get("content-type", "")

except ImportError:
    import requests

    def _get(url: str, **kw):
        timeout = kw.pop("timeout", 60.0)
        headers = kw.pop("headers", None) or {}
        return requests.get(url, timeout=timeout, headers=headers, allow_redirects=True, **kw)

    def _get_bytes(url: str, **kw) -> tuple[int, bytes, str]:
        timeout = kw.pop("timeout", 120.0)
        headers = kw.pop("headers", None) or {
            "User-Agent": "Mozilla/5.0 (compatible; AgentPapersBot/1.0; research)"
        }
        r = requests.get(url, timeout=timeout, headers=headers, allow_redirects=True, **kw)
        return r.status_code, r.content, r.headers.get("content-type", "")


ATOM_NS = {"atom": "http://www.w3.org/2005/Atom", "arxiv": "http://arxiv.org/schemas/atom"}

DEFAULT_QUERIES = [
    # self-evolution
    'all:"self-evolving agent" OR all:"self-improving LLM agent" OR all:"agent self-evolution"',
    'all:"self-improving agent" OR all:"autonomous agent evolution" OR all:"self-modifying agent"',
    'all:"continual learning" AND all:"LLM agent" AND (all:rewrite OR all:self-improve OR all:evolve)',
    'ti:"self-evolving" AND (ti:agent OR ti:LLM OR all:"language agent")',
    'all:"self-evolving" AND (all:agent OR all:"language agent") AND (all:survey OR all:review OR all:overview)',
    'all:"self-improving agent" AND (all:survey OR all:review) OR all:"autonomous agents" AND all:survey AND all:evolution',
    'all:"continual learning" AND (all:agent OR all:agents) AND (all:survey OR all:review) AND (all:LLM OR all:"language model")',
    'all:"agentic reinforcement learning" AND (all:survey OR all:review OR all:LLM OR all:agent)',
    'all:"open-ended evolution" AND (all:agent OR all:LLM) OR all:"meta-learning" AND all:"language agent"',
    # security / agentic safety
    'all:"LLM agent" AND (all:security OR all:safety OR all:adversarial OR all:jailbreak)',
    'all:"agent safety" OR all:"adversarial agents" OR all:"secure agent"',
    'all:"LLM agent" AND (all:survey OR all:review) AND (all:security OR all:safety OR all:jailbreak)',
    'all:"agentic safety" OR all:"agent security" AND (all:survey OR all:review OR all:LLM)',
    'all:"red teaming" AND (all:"LLM agent" OR all:agentic) OR all:"prompt injection" AND all:agent',
    # safe / secure self-evolution (intersection)
    'all:"safe self-evolution" OR all:"secure agent self-improvement" OR all:"self-modifying" alignment',
    'all:"alignment" AND all:"self-modifying agents" OR all:"agentic" AND all:jailbreak',
    'all:"secure self-improvement" OR all:"safe self-improving" OR all:"aligned self-evolution"',
    'all:"self-evolving" AND (all:safety OR all:security OR all:alignment) AND (all:agent OR all:LLM)',
    # LLM-based IoT / AIoT (always AND LLM/agent framing — not generic IoT hardware)
    'all:"LLM" AND (all:IoT OR all:AIoT OR all:"Internet of Things")',
    'all:"language model" AND (all:IoT OR all:"smart home" OR all:"edge device" OR all:MQTT)',
    'all:"LLM agent" AND (all:IoT OR all:AIoT OR all:"sensor network" OR all:"cyber-physical")',
    'all:AIoT AND (all:LLM OR all:agent OR all:"foundation model" OR all:"language model")',
    'all:"Internet of Things" AND (all:"LLM agent" OR all:"language agent" OR all:agentic)',
    # LLM training (SFT / RLHF / pretraining / post-training — not generic ML)
    'all:LLM AND (all:SFT OR all:RLHF OR all:DPO OR all:"instruction tuning")',
    'all:"large language model" AND (all:pretraining OR all:"pre-training" OR all:"post-training")',
    'all:"supervised fine-tuning" AND (all:LLM OR all:"language model")',
    'all:RLHF AND (all:LLM OR all:"language model")',
    'all:"continued pre-training" OR all:"continual pretraining" AND (all:LLM OR all:"language model")',
    'all:"continual learning" AND (all:LLM OR all:"language model") AND (all:SFT OR all:pretraining OR all:"fine-tuning")',
    'all:"LLM training" OR all:"language model" AND all:"fine-tuning" AND (all:SFT OR all:alignment OR all:preference)',
    'all:LLM AND (all:LoRA OR all:QLoRA OR all:PEFT OR all:"parameter-efficient")',
    # Agent tools & memory (tool use / function calling / agent memory / RAG-for-agents)
    'all:"LLM agent" AND (all:"tool use" OR all:"tool calling" OR all:"function calling")',
    'all:"language agent" AND (all:memory OR all:RAG OR all:"tool use")',
    'all:"tool-using agent" OR all:"function calling" AND (all:"LLM agent" OR all:agentic)',
    'all:"agent memory" OR all:"long-term memory" AND (all:"LLM agent" OR all:"language agent")',
    'all:RAG AND (all:"LLM agent" OR all:"language agent" OR all:agentic)',
    'all:"tool calling" AND (all:LLM OR all:"language model") AND (all:agent OR all:agentic OR all:memory)',
]

# Broader web/RapidSearch queries (human phrasing)
WEB_QUERIES = [
    "self-evolving LLM agent arxiv",
    "self-improving language agent paper",
    "agent self-evolution meta-learning LLM",
    "survey self-evolving agents LLM review",
    "continual learning LLM agents survey",
    "agentic reinforcement learning survey LLM",
    "LLM agent security safety adversarial",
    "LLM agent security survey review",
    "agentic safety survey jailbreak red-teaming",
    "safe self-evolution agent alignment",
    "secure agent self-improvement",
    "secure self-improvement aligned self-modifying agents",
    "LLM Internet of Things AIoT arxiv",
    "LLM agent IoT edge device paper",
    "AIoT large language model agent",
    "LLM smart home MQTT agent",
    "LLM SFT RLHF instruction tuning paper",
    "large language model pretraining post-training arxiv",
    "supervised fine-tuning language model",
    "LLM LoRA PEFT continual learning fine-tuning",
    "LLM agent tool use function calling",
    "language agent memory RAG tool calling",
    "agent long-term memory LLM paper",
]


KEYWORDS_FILENAME = "search_keywords.json"


def load_search_queries(out_dir: Path | str | None = None) -> list[str]:
    """Return ArXiv queries: merged_queries from search_keywords.json if present, else DEFAULT_QUERIES.

    Merged list is seed ∪ auto, already gated/deduped by maintain_keywords.py.
    Does not broaden into general ML — file is produced under topic gates.
    """
    base = Path(out_dir) if out_dir else Path(__file__).resolve().parent
    kw_path = base / KEYWORDS_FILENAME
    if not kw_path.exists():
        return list(DEFAULT_QUERIES)
    try:
        data = json.loads(kw_path.read_text(encoding="utf-8"))
    except Exception:
        return list(DEFAULT_QUERIES)
    merged = data.get("merged_queries") or []
    cleaned = [q.strip() for q in merged if isinstance(q, str) and q.strip()]
    return cleaned if cleaned else list(DEFAULT_QUERIES)

# Relevance patterns
AGENT_RE = re.compile(
    r"\b(agent|agents|agentic|llm[\s-]?agent|language[\s-]?agent|tool[\s-]?using|"
    r"multi[\s-]?agent|autonomous[\s-]?agent)\b",
    re.I,
)
EVOLVE_RE = re.compile(
    r"\b(self[\s-]?evolv\w*|self[\s-]?improv\w*|self[\s-]?modif\w*|meta[\s-]?learn\w*|"
    r"self[\s-]?rewrit\w*|self[\s-]?updat\w*|autonomous[\s-]?evolution|"
    r"continual[\s-]?improv\w*|continual[\s-]?learn\w*|open[\s-]?ended[\s-]?evolution|"
    r"agentic[\s-]?reinforcement|self[\s-]?refine\w*|self[\s-]?adapt\w*)\b",
    re.I,
)
SECURITY_RE = re.compile(
    r"\b(secur\w*|safet\w*|adversar\w*|jailbreak\w*|alignment|red[\s-]?team|"
    r"prompt[\s-]?injection|trojan|backdoor|exfiltrat\w*|attack|defense)\b",
    re.I,
)
SURVEY_RE = re.compile(
    r"\b(survey|surveys|review|reviews|overview|综述|systematic\s+review|"
    r"literature\s+review|taxonomy|state[\s-]of[\s-]the[\s-]art)\b",
    re.I,
)
# Classic RL-only noise (no LLM/agent framing) — soft downrank / drop if no agent+LLM
RL_ONLY_RE = re.compile(
    r"\b(reinforcement[\s-]?learning|markov|q[\s-]?learning|policy[\s-]?gradient)\b",
    re.I,
)
LLM_RE = re.compile(
    r"\b(llm|large[\s-]?language|language[\s-]?model|gpt|transformer|foundation[\s-]?model|"
    r"chatbot|instruct(?:ion)?[\s-]?tun)\b",
    re.I,
)
# IoT / AIoT / CPS / edge / smart-home — only used when paired with LLM/agent framing
IOT_RE = re.compile(
    r"\b("
    r"iot|aiot|iiot|"
    r"internet[\s-]?of[\s-]?things|"
    r"edge[\s-]?devices?|"
    r"smart[\s-]?homes?|"
    r"smart[\s-]?cit(?:y|ies)|"
    r"mqtt|"
    r"sensor[\s-]?networks?|"
    r"cyber[\s-]?physical(?:\s+systems?)?"
    r")\b",
    re.I,
)
# Tighter than LLM_RE: drop transformer/chatbot-only so vision-IoT does not flood.
LLM_IOT_FRAME_RE = re.compile(
    r"\b(llm|llms|large[\s-]?language|language[\s-]?model|language[\s-]?agent|"
    r"foundation[\s-]?model|gpt-\d|chatgpt)\b",
    re.I,
)
# "without any language model" / "no LLM" should not count as framing.
LLM_IOT_NEG_RE = re.compile(
    r"\b(?:without|no|not|unlike|non)[\s\w-]{0,48}"
    r"(?:llm|llms|language[\s-]?model|language[\s-]?agent|foundation[\s-]?model)\b",
    re.I,
)
# LLM training / post-training — paired with LLM_RE (not generic ML training).
TRAINING_RE = re.compile(
    r"\b("
    r"sft|rlhf|dpo|kto|grpo|orpo|"
    r"supervised[\s-]?fine[\s-]?tun\w*|"
    r"reinforcement[\s-]?learning[\s-]?from[\s-]?human[\s-]?feedback|"
    r"direct[\s-]?preference[\s-]?optim\w*|"
    r"instruction[\s-]?tun\w*|instruct[\s-]?tun\w*|"
    r"pre[\s-]?train\w*|continued[\s-]?pre[\s-]?train\w*|"
    r"post[\s-]?train\w*|mid[\s-]?train\w*|"
    r"fine[\s-]?tun\w*|finetun\w*|"
    r"preference[\s-]?tun\w*|preference[\s-]?optim\w*|"
    r"llm[\s-]?train\w*|language[\s-]?model[\s-]?train\w*|"
    r"lora|qlora|peft|parameter[\s-]?efficient|"
    r"knowledge[\s-]?distill\w*"
    r")\b",
    re.I,
)
# Agent tool-use / function calling / memory / RAG-for-agents.
TOOLS_MEMORY_RE = re.compile(
    r"\b("
    r"tool[\s-]?use|tool[\s-]?using|tool[\s-]?call\w*|function[\s-]?call\w*|"
    r"tool[\s-]?learn\w*|tool[\s-]?augment\w*|external[\s-]?tools?|"
    r"model[\s-]?context[\s-]?protocol|"
    r"agent[\s-]?memory|long[\s-]?term[\s-]?memory|episodic[\s-]?memory|"
    r"memory[\s-]?module|memory[\s-]?bank|memory[\s-]?augment\w*|"
    r"retrieval[\s-]?augment\w*|rag\b|"
    r"scratchpad|working[\s-]?memory|context[\s-]?memory"
    r")\b",
    re.I,
)
TOOL_CALL_RE = re.compile(
    r"\b(tool[\s-]?call\w*|function[\s-]?call\w*|tool[\s-]?use|tool[\s-]?using)\b",
    re.I,
)

PRIMARY_TAGS = (
    "both",
    "self-evolution",
    "security",
    "llm-iot",
    "llm-training",
    "agent-tools-memory",
)
STABLE_TAGS = (
    "self-evolution",
    "security",
    "both",
    "llm-iot",
    "survey",
    "llm-training",
    "agent-tools-memory",
    "other",
)
# `other` is never assigned by tag_topics / search — operator import only.
MANUAL_ONLY_TAGS = frozenset({"other"})

ARXIV_ID_RE = re.compile(
    r"(?:https?://)?(?:[\w.-]+\.)?arxiv\.org/(?:abs|pdf|html|src)/"
    r"(?:arxiv:)?(\d{4}\.\d{4,5}|[a-z\-]+(?:\.[a-zA-Z]{2})?/\d{7})(?:v\d+)?(?:\.pdf)?",
    re.I,
)
ARXIV_BARE_RE = re.compile(
    r"^(?:arxiv:)?(\d{4}\.\d{4,5}|[a-z\-]+(?:\.[a-zA-Z]{2})?/\d{7})(?:v\d+)?$",
    re.I,
)
ARXIV_LOOSE_RE = re.compile(
    r"(?:arxiv\.org/(?:abs|pdf|html|src)/|arxiv:)?(\d{4}\.\d{4,5})(?:v\d+)?",
    re.I,
)
DOI_RE = re.compile(r"10\.\d{4,9}/[-._;()/:A-Z0-9]+", re.I)


@dataclass
class Paper:
    title: str
    authors: list[str] = field(default_factory=list)
    abstract: str = ""
    year: Optional[int] = None
    source_url: str = ""
    pdf_url: str = ""
    arxiv_id: str = ""
    doi: str = ""
    topic_tags: list[str] = field(default_factory=list)
    score: float = 0.0
    query_hits: list[str] = field(default_factory=list)
    pdf_path: str = ""
    download_status: str = ""  # ok | skipped | failed | dry-run | no-pdf
    published: str = ""
    updated: str = ""
    source: str = ""  # "manual" for user imports; empty for search/sync
    page_count: int = 0  # local PDF pages; 0 = unknown

    def dedupe_key(self) -> str:
        if self.arxiv_id:
            return f"arxiv:{self.arxiv_id}"
        if self.doi:
            return f"doi:{self.doi.lower()}"
        norm = re.sub(r"\W+", " ", self.title.lower()).strip()
        return f"title:{hashlib.sha1(norm.encode()).hexdigest()[:16]}"


def log(msg: str) -> None:
    ts = datetime.now().strftime("%H:%M:%S")
    print(f"[{ts}] {msg}", flush=True)


def ensure_dirs(out: Path) -> dict[str, Path]:
    paths = {
        "root": out,
        "pdfs": out / "pdfs",
        "cache": out / "cache",
    }
    for p in paths.values():
        p.mkdir(parents=True, exist_ok=True)
    return paths


def cache_path(cache_dir: Path, key: str) -> Path:
    h = hashlib.sha1(key.encode()).hexdigest()
    return cache_dir / f"{h}.json"


def cache_get(cache_dir: Path, key: str, max_age_s: int = 86400) -> Optional[Any]:
    p = cache_path(cache_dir, key)
    if not p.exists():
        return None
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
        if time.time() - data.get("_ts", 0) > max_age_s:
            return None
        return data.get("payload")
    except Exception:
        return None


def cache_set(cache_dir: Path, key: str, payload: Any) -> None:
    p = cache_path(cache_dir, key)
    p.write_text(
        json.dumps({"_ts": time.time(), "payload": payload}, ensure_ascii=False),
        encoding="utf-8",
    )


def extract_arxiv_id(text: str) -> str:
    """Parse a canonical arXiv id (no version) from a bare id or abs/pdf URL."""
    s = (text or "").strip()
    if not s:
        return ""
    m = ARXIV_ID_RE.search(s)
    if m:
        return m.group(1)
    m = ARXIV_BARE_RE.match(s)
    if m:
        return m.group(1)
    # Loose new-style match only when the text mentions arxiv (avoid years in titles).
    if "arxiv" in s.lower():
        m = ARXIV_LOOSE_RE.search(s)
        if m:
            return m.group(1)
    return ""


def validate_topic_tag(tag: str) -> Optional[str]:
    """Return the stable tag key, or None if unknown."""
    t = (tag or "").strip().lower()
    if t in STABLE_TAGS:
        return t
    return None


def preserve_manual_tags(paper: Paper, old_tags: Optional[list[str]] = None) -> bool:
    tags = list(old_tags if old_tags is not None else (paper.topic_tags or []))
    if (paper.source or "").strip().lower() == "manual":
        return True
    return bool(set(tags) & MANUAL_ONLY_TAGS)


def extract_doi(text: str) -> str:
    m = DOI_RE.search(text or "")
    return m.group(0).rstrip(".") if m else ""


IOT_STRONG_RE = re.compile(
    r"\b(aiot|iiot|internet[\s-]?of[\s-]?things|llm[\s-]?iot|"
    r"iot[\s-]?agents?|agents?[\s-]?iot)\b",
    re.I,
)


def is_llm_iot(title: str, abstract: str) -> bool:
    """True when IoT/AIoT/CPS/edge/smart-home is framed as LLM/agent/foundation-model.

    Passing mentions (e.g. "smartphones and IoT") do not count unless IoT is in
    the title, a strong phrase (AIoT / Internet of Things), or within ~80 chars
    of an LLM/agent frame.
    """
    text = f"{title}. {abstract}"
    if not IOT_RE.search(text):
        return False
    framed = bool(AGENT_RE.search(text) or LLM_IOT_FRAME_RE.search(text))
    if not framed:
        return False
    if LLM_IOT_NEG_RE.search(text) and not (
        AGENT_RE.search(title) or LLM_IOT_FRAME_RE.search(title)
    ):
        return False
    if IOT_RE.search(title) or IOT_STRONG_RE.search(text):
        return True
    # Same-sentence pairing only — avoids passing "smartphones and IoT, …" then later "LLM".
    for sent in re.split(r"[.!?;\n]+", text):
        if IOT_RE.search(sent) and (AGENT_RE.search(sent) or LLM_IOT_FRAME_RE.search(sent)):
            return True
    return False


def is_llm_training(title: str, abstract: str) -> bool:
    """True when the paper is about training / post-training an LLM (not generic ML)."""
    text = f"{title}. {abstract}"
    if not TRAINING_RE.search(text):
        return False
    return bool(LLM_RE.search(text))


def is_agent_tools_memory(title: str, abstract: str) -> bool:
    """True for agent tool-use / function calling / memory / RAG-for-agents."""
    text = f"{title}. {abstract}"
    if not TOOLS_MEMORY_RE.search(text):
        return False
    if AGENT_RE.search(text):
        return True
    # Function/tool calling is agent-shaped even when the word "agent" is absent.
    return bool(LLM_RE.search(text) and TOOL_CALL_RE.search(text))


def tag_topics(title: str, abstract: str) -> list[str]:
    """Return stable English tag keys (primary + optional overlays).

    Primary (mutually exclusive): both | self-evolution | security | llm-iot
        | agent-tools-memory | llm-training
    Overlay: llm-iot / agent-tools-memory / llm-training may also attach
    when they overlap an agent primary. `other` is never auto-assigned.
    Secondary: survey
    """
    text = f"{title} {abstract}"
    evo = bool(EVOLVE_RE.search(text))
    sec = bool(SECURITY_RE.search(text))
    survey = bool(SURVEY_RE.search(text))
    llm_iot = is_llm_iot(title, abstract)
    tools = is_agent_tools_memory(title, abstract)
    train = is_llm_training(title, abstract)
    tags: list[str] = []
    if evo and sec:
        tags.append("both")
    elif evo:
        tags.append("self-evolution")
    elif sec:
        tags.append("security")
    elif llm_iot:
        tags.append("llm-iot")
    elif tools:
        tags.append("agent-tools-memory")
    elif train:
        tags.append("llm-training")
    if llm_iot and "llm-iot" not in tags:
        tags.append("llm-iot")
    if tools and "agent-tools-memory" not in tags:
        tags.append("agent-tools-memory")
    if train and "llm-training" not in tags:
        tags.append("llm-training")
    if survey:
        if tags:
            tags.append("survey")
        elif AGENT_RE.search(text) or LLM_RE.search(text):
            tags = ["survey"]
    return tags


def relevance_score(title: str, abstract: str) -> float:
    text = f"{title}\n{abstract}"
    has_agent = bool(AGENT_RE.search(text))
    has_llm = bool(LLM_RE.search(text))
    evo = bool(EVOLVE_RE.search(text))
    sec = bool(SECURITY_RE.search(text))
    llm_iot = is_llm_iot(title, abstract)
    tools = is_agent_tools_memory(title, abstract)
    train = is_llm_training(title, abstract)

    if not (has_agent or has_llm):
        # pure classic RL / generic IoT hardware / unrelated
        return -1.0
    if not (evo or sec or llm_iot or tools or train):
        return -0.5

    score = 0.0
    if has_agent:
        score += 2.0
    if has_llm:
        score += 1.5
    if evo:
        score += 3.0
    if sec:
        score += 3.0
    if evo and sec:
        score += 1.5  # bonus for both themes
    if llm_iot:
        score += 3.0  # enough to pass the gate without evo/sec
        if evo or sec:
            score += 0.5
    if tools:
        score += 3.0
        if evo or sec:
            score += 0.5
    if train:
        score += 3.0
        if evo or sec or tools:
            score += 0.5
    # title hits weigh more
    if EVOLVE_RE.search(title) or SECURITY_RE.search(title):
        score += 2.0
    if IOT_RE.search(title) and (has_agent or has_llm):
        score += 1.5
    if TRAINING_RE.search(title) and has_llm:
        score += 1.5
    if TOOLS_MEMORY_RE.search(title) and (has_agent or has_llm):
        score += 1.5
    if AGENT_RE.search(title):
        score += 1.0
    # soft penalty: RL-heavy without LLM
    if RL_ONLY_RE.search(text) and not has_llm and not EVOLVE_RE.search(text) and not llm_iot:
        score -= 2.0
    # surveys/reviews are high-value for corpus coverage
    if SURVEY_RE.search(text) and (evo or sec or llm_iot or tools or train):
        score += 1.5
    if SURVEY_RE.search(title) and (evo or sec or has_agent or llm_iot or tools or train):
        score += 1.0
    return score


def is_relevant(paper: Paper) -> bool:
    return paper.score >= 3.0 and bool(paper.topic_tags)


# -------------------- ArXiv --------------------

_last_arxiv_call = 0.0


def arxiv_sleep(min_interval: float = 8.0) -> None:
    global _last_arxiv_call
    try:
        min_interval = float(os.environ.get("ARXIV_MIN_INTERVAL", min_interval))
    except ValueError:
        pass
    elapsed = time.time() - _last_arxiv_call
    if elapsed < min_interval:
        time.sleep(min_interval - elapsed)
    _last_arxiv_call = time.time()


def arxiv_query(
    query: str,
    cache_dir: Path,
    start: int = 0,
    max_results: int = 15,
    retries: int = 8,
    sort_by: str = "relevance",
    sort_order: str = "descending",
    submitted_from: Optional[str] = None,
    submitted_to: Optional[str] = None,
) -> list[Paper]:
    """Query ArXiv Atom API with backoff + cache.

    sort_by: relevance | lastUpdatedDate | submittedDate
    submitted_from/to: YYYYMMDDHHMM (inclusive) for submittedDate filter.
    """
    cache_key = (
        f"arxiv|{query}|{start}|{max_results}|{sort_by}|{sort_order}"
        f"|{submitted_from}|{submitted_to}"
    )
    cached = cache_get(cache_dir, cache_key)
    if cached is not None:
        log(f"  cache hit: arxiv ({len(cached)} entries)")
        return [_paper_from_dict(x) for x in cached]

    search_q = query
    if submitted_from or submitted_to:
        lo = submitted_from or "198001010000"
        hi = submitted_to or "209912312359"
        date_clause = f"submittedDate:[{lo} TO {hi}]"
        search_q = f"({query}) AND {date_clause}"

    url = (
        "https://export.arxiv.org/api/query?"
        f"search_query={quote_plus(search_q)}&start={start}"
        f"&max_results={max_results}&sortBy={sort_by}&sortOrder={sort_order}"
    )
    body = ""
    for attempt in range(retries):
        arxiv_sleep()
        try:
            r = _get(
                url,
                headers={"User-Agent": "AgentPapersBot/1.0 (research; mailto:local)"},
                timeout=60.0,
            )
            status = getattr(r, "status_code", 200)
            body = r.text if hasattr(r, "text") else r.content.decode("utf-8", "replace")
            if status == 429 or "Rate exceeded" in body or "Retry after" in body:
                wait = 45 * (attempt + 1)
                log(f"  ArXiv rate limit — sleep {wait}s (attempt {attempt+1}/{retries})")
                time.sleep(wait)
                continue
            if status >= 500:
                wait = 20 * (attempt + 1)
                log(f"  ArXiv HTTP {status} — sleep {wait}s")
                time.sleep(wait)
                continue
            if status != 200:
                log(f"  ArXiv HTTP {status}: {body[:200]}")
                return []
            break
        except Exception as e:
            wait = 20 * (attempt + 1)
            log(f"  ArXiv error {e!r} — sleep {wait}s")
            time.sleep(wait)
            body = ""
    else:
        log("  ArXiv: giving up after retries")
        return []

    papers = parse_arxiv_atom(body)
    cache_set(cache_dir, cache_key, [asdict(p) for p in papers])
    return papers


def parse_arxiv_atom(xml_text: str) -> list[Paper]:
    papers: list[Paper] = []
    try:
        root = ET.fromstring(xml_text)
    except ET.ParseError as e:
        log(f"  ArXiv XML parse error: {e}")
        return []

    for entry in root.findall("atom:entry", ATOM_NS):
        title = (entry.findtext("atom:title", default="", namespaces=ATOM_NS) or "").strip()
        title = re.sub(r"\s+", " ", title)
        summary = (entry.findtext("atom:summary", default="", namespaces=ATOM_NS) or "").strip()
        summary = re.sub(r"\s+", " ", summary)
        authors = [
            (a.findtext("atom:name", default="", namespaces=ATOM_NS) or "").strip()
            for a in entry.findall("atom:author", ATOM_NS)
        ]
        published = entry.findtext("atom:published", default="", namespaces=ATOM_NS) or ""
        updated = entry.findtext("atom:updated", default="", namespaces=ATOM_NS) or ""
        year = None
        if published[:4].isdigit():
            year = int(published[:4])

        arxiv_id = ""
        id_url = entry.findtext("atom:id", default="", namespaces=ATOM_NS) or ""
        arxiv_id = extract_arxiv_id(id_url)
        doi = ""
        doi_el = entry.find("arxiv:doi", ATOM_NS)
        if doi_el is not None and doi_el.text:
            doi = doi_el.text.strip()

        pdf_url = ""
        source_url = id_url
        for link in entry.findall("atom:link", ATOM_NS):
            href = link.attrib.get("href", "")
            rel = link.attrib.get("rel", "")
            title_attr = link.attrib.get("title", "")
            if link.attrib.get("type") == "application/pdf" or title_attr == "pdf":
                pdf_url = href
            if rel == "alternate":
                source_url = href
        if arxiv_id and not pdf_url:
            pdf_url = f"https://arxiv.org/pdf/{arxiv_id}.pdf"
        if arxiv_id and not source_url:
            source_url = f"https://arxiv.org/abs/{arxiv_id}"

        tags = tag_topics(title, summary)
        score = relevance_score(title, summary)
        papers.append(
            Paper(
                title=title,
                authors=authors,
                abstract=summary[:1200],
                year=year,
                source_url=source_url,
                pdf_url=pdf_url,
                arxiv_id=arxiv_id,
                doi=doi,
                topic_tags=tags,
                score=score,
                published=published,
                updated=updated,
            )
        )
    return papers


def _paper_from_dict(d: dict) -> Paper:
    return Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})


# -------------------- Semantic Scholar (optional) --------------------

def semantic_scholar_search(query: str, cache_dir: Path, limit: int = 10) -> list[Paper]:
    cache_key = f"s2|{query}|{limit}"
    cached = cache_get(cache_dir, cache_key)
    if cached is not None:
        return [_paper_from_dict(x) for x in cached]

    url = (
        "https://api.semanticscholar.org/graph/v1/paper/search"
        f"?query={quote_plus(query)}&limit={limit}"
        "&fields=title,abstract,year,authors,externalIds,url,openAccessPdf"
    )
    try:
        time.sleep(1.0)
        r = _get(url, headers={"User-Agent": "AgentPapersBot/1.0"}, timeout=45.0)
        if getattr(r, "status_code", 200) == 429:
            log("  Semantic Scholar rate-limited; skipping")
            return []
        if getattr(r, "status_code", 200) != 200:
            log(f"  Semantic Scholar HTTP {getattr(r, 'status_code', '?')}")
            return []
        data = r.json()
    except Exception as e:
        log(f"  Semantic Scholar error: {e!r}")
        return []

    papers: list[Paper] = []
    for item in data.get("data") or []:
        title = (item.get("title") or "").strip()
        abstract = (item.get("abstract") or "")[:1200]
        authors = [a.get("name", "") for a in (item.get("authors") or []) if a.get("name")]
        year = item.get("year")
        ext = item.get("externalIds") or {}
        arxiv_id = (ext.get("ArXiv") or "").replace("arXiv:", "")
        doi = ext.get("DOI") or ""
        pdf_url = ""
        oa = item.get("openAccessPdf") or {}
        if oa.get("url"):
            pdf_url = oa["url"]
        if arxiv_id and not pdf_url:
            pdf_url = f"https://arxiv.org/pdf/{arxiv_id}.pdf"
        source_url = item.get("url") or (
            f"https://arxiv.org/abs/{arxiv_id}" if arxiv_id else ""
        )
        tags = tag_topics(title, abstract)
        score = relevance_score(title, abstract)
        papers.append(
            Paper(
                title=title,
                authors=authors,
                abstract=abstract,
                year=year,
                source_url=source_url,
                pdf_url=pdf_url,
                arxiv_id=arxiv_id,
                doi=doi,
                topic_tags=tags,
                score=score,
            )
        )
    cache_set(cache_dir, cache_key, [asdict(p) for p in papers])
    return papers


# -------------------- RapidSearch (optional) --------------------

def rapidsearch_available(base: str = "http://127.0.0.1:18765") -> bool:
    try:
        r = _get(f"{base}/health", timeout=3.0)
        return getattr(r, "status_code", 0) == 200
    except Exception:
        return False


def rapidsearch_query(
    query: str,
    cache_dir: Path,
    base: str = "http://127.0.0.1:18765",
    n: int = 8,
) -> list[dict]:
    """Return raw search hits (title, url, snippet). Local /search needs no token."""
    cache_key = f"rs|{base}|{query}|{n}"
    cached = cache_get(cache_dir, cache_key, max_age_s=3600)
    if cached is not None:
        return cached

    url = f"{base}/search?q={quote_plus(query)}&n={n}&content=0"
    try:
        r = _get(url, timeout=90.0)
        if getattr(r, "status_code", 0) != 200:
            log(f"  RapidSearch HTTP {getattr(r, 'status_code', '?')}")
            return []
        data = r.json()
    except Exception as e:
        log(f"  RapidSearch error: {e!r}")
        return []

    results = data.get("results") or data.get("items") or data.get("data") or []
    if isinstance(data, list):
        results = data
    hits = []
    for item in results:
        if not isinstance(item, dict):
            continue
        hits.append(
            {
                "title": item.get("title") or item.get("name") or "",
                "url": item.get("url") or item.get("link") or item.get("href") or "",
                "snippet": item.get("snippet") or item.get("description") or item.get("content") or "",
            }
        )
    cache_set(cache_dir, cache_key, hits)
    return hits


def papers_from_web_hits(hits: list[dict]) -> list[Paper]:
    papers: list[Paper] = []
    for h in hits:
        title = (h.get("title") or "").strip()
        url = h.get("url") or ""
        snippet = h.get("snippet") or ""
        arxiv_id = extract_arxiv_id(url) or extract_arxiv_id(title) or extract_arxiv_id(snippet)
        if not arxiv_id and "arxiv" not in url.lower() and "openreview" not in url.lower():
            # only keep arxiv / openreview-ish, or clearly on-topic title+snippet
            blob = f"{title} {snippet}"
            framed = bool(AGENT_RE.search(title) or LLM_RE.search(blob))
            themed = bool(
                EVOLVE_RE.search(blob) or SECURITY_RE.search(blob) or IOT_RE.search(blob)
            )
            if not (framed and themed):
                continue
        doi = extract_doi(url) or extract_doi(snippet)
        pdf_url = f"https://arxiv.org/pdf/{arxiv_id}.pdf" if arxiv_id else ""
        source_url = f"https://arxiv.org/abs/{arxiv_id}" if arxiv_id else url
        tags = tag_topics(title, snippet)
        score = relevance_score(title, snippet)
        # bump if we only have title from web
        if score < 0 and arxiv_id:
            score = 2.0  # will enrich via arxiv later
        papers.append(
            Paper(
                title=title or f"arxiv:{arxiv_id}",
                abstract=snippet[:800],
                source_url=source_url,
                pdf_url=pdf_url,
                arxiv_id=arxiv_id,
                doi=doi,
                topic_tags=tags or (["self-evolution"] if score >= 0 else []),
                score=max(score, 0),
            )
        )
    return papers


def enrich_from_arxiv_id(arxiv_id: str, cache_dir: Path) -> Optional[Paper]:
    papers = arxiv_query(f"id:{arxiv_id}", cache_dir, max_results=1)
    return papers[0] if papers else None


# -------------------- Crawl4AI PDF extraction (sparingly) --------------------

async def _crawl4ai_find_pdf(page_url: str) -> str:
    from crawl4ai import AsyncWebCrawler, BrowserConfig, CrawlerRunConfig

    browser_cfg = BrowserConfig(headless=True, verbose=False)
    run_cfg = CrawlerRunConfig(
        wait_until="domcontentloaded",
        page_timeout=30000,
    )
    async with AsyncWebCrawler(config=browser_cfg) as crawler:
        result = await crawler.arun(url=page_url, config=run_cfg)
        html = getattr(result, "html", None) or ""
        markdown = getattr(result, "markdown", None) or ""
        text = f"{html}\n{markdown}"
        # prefer direct pdf links
        pdfs = re.findall(
            r'href=["\']([^"\']+\.pdf[^"\']*)["\']',
            html,
            flags=re.I,
        )
        pdfs += re.findall(r"(https?://[^\s\)\"']+\.pdf)", text, flags=re.I)
        for p in pdfs:
            if p.startswith("//"):
                p = "https:" + p
            if p.startswith("http"):
                return p
        # openreview pdf pattern
        m = re.search(r"openreview\.net/(?:pdf\?id=|forum\?id=)([A-Za-z0-9_-]+)", page_url)
        if m:
            return f"https://openreview.net/pdf?id={m.group(1)}"
        aid = extract_arxiv_id(page_url) or extract_arxiv_id(text)
        if aid:
            return f"https://arxiv.org/pdf/{aid}.pdf"
    return ""


def find_pdf_via_crawl4ai(page_url: str) -> str:
    try:
        import asyncio

        return asyncio.run(_crawl4ai_find_pdf(page_url))
    except Exception as e:
        log(f"  Crawl4AI failed for {page_url}: {e!r}")
        return ""


# -------------------- Download --------------------

def safe_filename(paper: Paper) -> str:
    base = paper.arxiv_id or extract_doi(paper.doi).replace("/", "_") or paper.dedupe_key()
    base = re.sub(r"[^\w.\-]+", "_", base)[:80]
    # short title slug
    slug = re.sub(r"[^\w]+", "_", paper.title.lower())[:40].strip("_")
    return f"{base}_{slug}.pdf" if slug else f"{base}.pdf"


def download_pdf(paper: Paper, pdf_dir: Path, dry_run: bool = False) -> Paper:
    dest = pdf_dir / safe_filename(paper)
    if dest.exists() and dest.stat().st_size > 1000:
        paper.pdf_path = str(dest)
        paper.download_status = "skipped"
        return paper

    if dry_run:
        paper.download_status = "dry-run"
        paper.pdf_path = str(dest)
        return paper

    pdf_url = paper.pdf_url
    if not pdf_url and paper.source_url:
        # try crawl4ai only for non-arxiv HTML pages
        if "arxiv.org" in paper.source_url and paper.arxiv_id:
            pdf_url = f"https://arxiv.org/pdf/{paper.arxiv_id}.pdf"
        else:
            log(f"  Crawl4AI extract PDF: {paper.source_url[:80]}")
            pdf_url = find_pdf_via_crawl4ai(paper.source_url)
            paper.pdf_url = pdf_url

    if not pdf_url:
        paper.download_status = "no-pdf"
        return paper

    try:
        status, content, ctype = _get_bytes(pdf_url)
        if status != 200:
            paper.download_status = f"failed:{status}"
            return paper
        # ArXiv sometimes returns HTML interstitial
        if content[:4] != b"%PDF" and "pdf" not in (ctype or "").lower():
            if "arxiv.org" in pdf_url:
                time.sleep(2)
                status, content, ctype = _get_bytes(pdf_url + ("&" if "?" in pdf_url else "?") + "download=1")
            if content[:4] != b"%PDF":
                paper.download_status = "failed:not-pdf"
                return paper
        dest.write_bytes(content)
        paper.pdf_path = str(dest)
        paper.download_status = "ok"
        log(f"  downloaded {dest.name} ({len(content)//1024} KB)")
    except Exception as e:
        paper.download_status = f"failed:{e!r}"
        log(f"  download error: {e!r}")
    return paper


# -------------------- Merge / report --------------------

def load_existing_manifest(out: Path) -> dict[str, Paper]:
    """Load papers already in manifest.json for merge-on-expand."""
    mp = out / "manifest.json"
    if not mp.exists():
        return {}
    try:
        data = json.loads(mp.read_text(encoding="utf-8"))
    except Exception as e:
        log(f"Could not load existing manifest: {e!r}")
        return {}
    out_map: dict[str, Paper] = {}
    for d in data.get("papers") or []:
        try:
            p = Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})
            # Refresh tags/score with current heuristics (e.g. survey flag).
            # Manual imports keep the operator-chosen category.
            if not preserve_manual_tags(p):
                fresh_tags = tag_topics(p.title, p.abstract)
                if fresh_tags:
                    p.topic_tags = fresh_tags
            p.score = max(p.score, relevance_score(p.title, p.abstract))
            out_map[p.dedupe_key()] = p
        except Exception:
            continue
    return out_map



def merge_papers(existing: dict[str, Paper], newcomers: Iterable[Paper], query_label: str) -> int:
    added = 0
    for p in newcomers:
        if not p.title and not p.arxiv_id:
            continue
        key = p.dedupe_key()
        if key in existing:
            old = existing[key]
            # enrich
            if len(p.abstract) > len(old.abstract):
                old.abstract = p.abstract
            if p.pdf_url and not old.pdf_url:
                old.pdf_url = p.pdf_url
            if p.authors and not old.authors:
                old.authors = p.authors
            if p.year and not old.year:
                old.year = p.year
            if p.score > old.score:
                old.score = p.score
                old.topic_tags = p.topic_tags or old.topic_tags
            if query_label not in old.query_hits:
                old.query_hits.append(query_label)
        else:
            p.query_hits = [query_label]
            existing[key] = p
            added += 1
    return added


def write_manifest(out: Path, papers: list[Paper], stats: dict) -> None:
    payload = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "stats": stats,
        "papers": [asdict(p) for p in papers],
    }
    (out / "manifest.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )


def write_index(out: Path, papers: list[Paper], stats: dict) -> None:
    lines = [
        "# Agent Self-Evolution & Security Papers",
        "",
        f"Generated: {datetime.now(timezone.utc).astimezone().strftime('%Y-%m-%d %H:%M %Z')}",
        "",
        f"- Searched candidates (raw merged): **{stats.get('searched', 0)}**",
        f"- Kept after relevance filter: **{stats.get('kept', 0)}**",
        f"- PDFs downloaded (ok): **{stats.get('downloaded', 0)}**",
        f"- Skipped (already present): **{stats.get('skipped', 0)}**",
        f"- Failures / no-pdf: **{stats.get('failures', 0)}**",
        "",
        "## Themes / 主题",
        "",
        "- **self-evolution** — agent自进化: self-evolving / self-improving / self-modifying / continual agents",
        "- **security** — agent安全: LLM agent security, agentic safety, jailbreak, red-teaming, prompt injection",
        "- **both** — agent安全自进化: safe/secure self-evolution & alignment of self-modifying agents",
        "- **llm-iot** — LLM based 物联网: LLM/AIoT / LLM agents for IoT, edge, smart home/city, CPS (not generic IoT hardware)",
        "- **survey** — 综述: survey / review (secondary flag when detected in title/abstract)",
        "- **llm-training** — LLM 训练 / LLM training: SFT, RLHF, DPO, pretraining, post-training, instruction tuning",
        "- **agent-tools-memory** — agent工具与记忆 / Agent tools & memory: tool use, function calling, agent memory, RAG-for-agents",
        "- **other** — 其它 / Other (manual import only)",
        "",
    ]
    theme_counts = stats.get("theme_counts") or {}
    if theme_counts:
        lines.append("Theme counts: " + ", ".join(f"**{k}**={v}" for k, v in sorted(theme_counts.items())))
        lines.append("")
    lines.extend([
        "## Papers",
        "",
    ])
    for i, p in enumerate(papers, 1):
        tags = ", ".join(p.topic_tags) or "?"
        authors = ", ".join(p.authors[:5])
        if len(p.authors) > 5:
            authors += " et al."
        lines.append(f"### {i}. {p.title}")
        lines.append("")
        lines.append(f"- **Tags:** {tags} | **Year:** {p.year or '?'} | **Score:** {p.score:.1f}")
        if authors:
            lines.append(f"- **Authors:** {authors}")
        if p.arxiv_id:
            lines.append(f"- **ArXiv:** [{p.arxiv_id}](https://arxiv.org/abs/{p.arxiv_id})")
        if p.source_url:
            lines.append(f"- **Source:** {p.source_url}")
        if p.pdf_url:
            lines.append(f"- **PDF URL:** {p.pdf_url}")
        if p.pdf_path:
            lines.append(f"- **Local:** `{p.pdf_path}` ({p.download_status})")
        else:
            lines.append(f"- **Download:** {p.download_status or 'n/a'}")
        if p.abstract:
            snip = p.abstract[:400] + ("…" if len(p.abstract) > 400 else "")
            lines.append(f"- **Abstract:** {snip}")
        lines.append("")
    (out / "index.md").write_text("\n".join(lines), encoding="utf-8")


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    default_out = Path(__file__).resolve().parent
    p = argparse.ArgumentParser(
        description="Search & download agent self-evolution / security / LLM-IoT / training / tools-memory papers"
    )
    p.add_argument("--out", type=Path, default=default_out, help="Output directory")
    p.add_argument("--max", type=int, default=20, help="Max unique papers to keep/download")
    p.add_argument("--dry-run", action="store_true", help="Do not download PDFs")
    p.add_argument(
        "--retag",
        action="store_true",
        help="Recompute topic_tags/score for existing corpus (manifest+DB); no search/download",
    )
    p.add_argument(
        "--queries",
        type=str,
        default="",
        help="Override ArXiv queries (; -separated)",
    )
    p.add_argument("--skip-web", action="store_true", help="Skip RapidSearch / web")
    p.add_argument("--skip-s2", action="store_true", help="Skip Semantic Scholar")
    return p.parse_args(argv)


def retag_corpus(out: Path) -> dict:
    """Recompute topic_tags + score for all papers in manifest + papers.db.

    Does not delete or rewrite PDFs. Only metadata (tags/score) is refreshed.
    """
    mp = out / "manifest.json"
    if not mp.exists():
        log("No manifest.json — nothing to retag")
        return {"n": 0, "changed": 0, "theme_counts": {}, "primary": {}}

    data = json.loads(mp.read_text(encoding="utf-8"))
    papers: list[Paper] = []
    theme_counts: dict[str, int] = {}
    primary_counts: dict[str, int] = {}
    changed = 0
    for d in data.get("papers") or []:
        p = Paper(**{k: v for k, v in d.items() if k in Paper.__dataclass_fields__})
        old_tags = list(p.topic_tags or [])
        if preserve_manual_tags(p, old_tags):
            p.topic_tags = old_tags
        else:
            p.topic_tags = tag_topics(p.title, p.abstract)
        p.score = relevance_score(p.title, p.abstract)
        if p.topic_tags != old_tags:
            changed += 1
        papers.append(p)
        for t in p.topic_tags:
            theme_counts[t] = theme_counts.get(t, 0) + 1
        prim = "none"
        for k in PRIMARY_TAGS:
            if k in p.topic_tags:
                prim = k
                break
        primary_counts[prim] = primary_counts.get(prim, 0) + 1

    stats = dict(data.get("stats") or {})
    stats["theme_counts"] = theme_counts
    stats["retagged_at"] = datetime.now(timezone.utc).isoformat()
    stats["retag_changed"] = changed
    write_manifest(out, papers, stats)
    write_index(out, papers, stats)

    db_path = out / "papers.db"
    if db_path.exists():
        from db import _as_tags, connect, content_hash, init_db, paper_id_from_fields

        conn = connect(db_path)
        try:
            init_db(conn)
            updated = 0
            for p in papers:
                pid = paper_id_from_fields(
                    arxiv_id=p.arxiv_id,
                    doi=p.doi,
                    title=p.title,
                    dedupe_key=p.dedupe_key(),
                )
                tags = _as_tags(p.topic_tags)
                row = conn.execute(
                    "SELECT title, abstract, authors FROM papers WHERE id=?",
                    (pid,),
                ).fetchone()
                if not row:
                    continue
                ch = content_hash(row["title"] or "", row["abstract"] or "", row["authors"] or "", tags)
                conn.execute(
                    "UPDATE papers SET tags=?, content_hash=? WHERE id=?",
                    (tags, ch, pid),
                )
                updated += 1
            conn.commit()
            log(f"DB tags updated for {updated} rows (PDFs untouched)")
        finally:
            conn.close()

    log(f"Retagged {len(papers)} papers, {changed} tag-sets changed")
    log(f"theme_counts={theme_counts}")
    log(f"primary_counts={primary_counts}")
    return {
        "n": len(papers),
        "changed": changed,
        "theme_counts": theme_counts,
        "primary": primary_counts,
    }


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    if getattr(args, "retag", False):
        retag_corpus(args.out)
        return 0
    paths = ensure_dirs(args.out)
    queries = [q.strip() for q in args.queries.split(";") if q.strip()] if args.queries else load_search_queries(args.out)

    pool: dict[str, Paper] = {}
    searched = 0

    log(f"Output: {paths['root']}")
    log(f"Running {len(queries)} ArXiv queries (max keep={args.max})")

    # Merge in existing corpus so expand never drops known papers
    existing = load_existing_manifest(paths["root"])
    if existing:
        merge_papers(pool, existing.values(), "existing")
        log(f"Loaded {len(existing)} existing papers from manifest into pool")

    per_query = max(15, min(40, max(20, args.max // max(len(queries), 1) + 8)))
    log(f"ArXiv max_results per query: {per_query}")

    # 1) ArXiv
    for i, q in enumerate(queries, 1):
        log(f"ArXiv [{i}/{len(queries)}]: {q[:90]}…")
        papers = arxiv_query(q, paths["cache"], max_results=per_query)
        searched += len(papers)
        n = merge_papers(pool, papers, f"arxiv:{i}")
        log(f"  got {len(papers)}, new unique {n}, pool={len(pool)}")

    # 2) Semantic Scholar (light)
    if not args.skip_s2:
        s2_queries = [
            "self-evolving LLM agent",
            "self-improving language agent",
            "survey self-evolving agents LLM",
            "continual learning LLM agents survey",
            "LLM agent security safety",
            "LLM agent security survey",
            "agentic safety survey jailbreak",
            "safe self-evolution agent alignment",
            "secure self-improvement agent alignment",
            "adversarial agents jailbreak LLM",
            "LLM IoT AIoT agent",
            "large language model Internet of Things",
            "LLM agent smart home edge device",
            "LLM SFT RLHF instruction tuning",
            "large language model pretraining post-training",
            "LLM agent tool use function calling",
            "language agent memory RAG",
        ]
        for i, q in enumerate(s2_queries, 1):
            log(f"Semantic Scholar [{i}/{len(s2_queries)}]: {q}")
            papers = semantic_scholar_search(q, paths["cache"], limit=8)
            searched += len(papers)
            n = merge_papers(pool, papers, f"s2:{i}")
            log(f"  got {len(papers)}, new unique {n}, pool={len(pool)}")

    # 3) RapidSearch → arxiv ids / openreview
    if not args.skip_web and rapidsearch_available():
        log("RapidSearch local is up")
        for i, q in enumerate(WEB_QUERIES, 1):
            log(f"RapidSearch [{i}/{len(WEB_QUERIES)}]: {q}")
            hits = rapidsearch_query(q, paths["cache"], n=6)
            web_papers = papers_from_web_hits(hits)
            # enrich bare arxiv ids
            enriched: list[Paper] = []
            for wp in web_papers:
                if wp.arxiv_id and (not wp.abstract or len(wp.abstract) < 80):
                    full = enrich_from_arxiv_id(wp.arxiv_id, paths["cache"])
                    if full:
                        full.query_hits = wp.query_hits
                        enriched.append(full)
                        continue
                enriched.append(wp)
            searched += len(enriched)
            n = merge_papers(pool, enriched, f"web:{i}")
            log(f"  hits={len(hits)}, papers={len(enriched)}, new={n}, pool={len(pool)}")
    elif not args.skip_web:
        log("RapidSearch not available — skipping web search")

    # Filter + rank
    candidates = [p for p in pool.values() if is_relevant(p)]
    # If filter too strict, relax slightly
    if len(candidates) < max(5, args.max // 2):
        log("Relaxing relevance threshold (score>=2 + agent/llm)")
        candidates = [
            p
            for p in pool.values()
            if p.score >= 2.0
            and (AGENT_RE.search(p.title + " " + p.abstract) or LLM_RE.search(p.title + " " + p.abstract))
            and (p.topic_tags or tag_topics(p.title, p.abstract))
        ]
        for p in candidates:
            if not p.topic_tags:
                p.topic_tags = tag_topics(p.title, p.abstract) or ["self-evolution"]

    # Balance themes: prefer mix of core + training + tools/memory (+ surveys)
    candidates.sort(key=lambda x: (-x.score, -(x.year or 0), x.title))
    selected: list[Paper] = []
    counts = {
        "self-evolution": 0,
        "security": 0,
        "both": 0,
        "llm-iot": 0,
        "llm-training": 0,
        "agent-tools-memory": 0,
        "survey": 0,
    }
    target_each = max(2, args.max // 6)
    survey_target = max(4, args.max // 8)

    def primary_tag(p: Paper) -> str:
        tags = p.topic_tags or []
        if "both" in tags:
            return "both"
        if "security" in tags:
            return "security"
        if "self-evolution" in tags:
            return "self-evolution"
        if "llm-iot" in tags:
            return "llm-iot"
        if "agent-tools-memory" in tags:
            return "agent-tools-memory"
        if "llm-training" in tags:
            return "llm-training"
        return "self-evolution"

    existing_keys = set(existing.keys()) if existing else set()

    # Always keep already-downloaded / existing corpus papers that remain relevant
    for p in candidates:
        if p.dedupe_key() in existing_keys:
            if p not in selected:
                selected.append(p)
                counts[primary_tag(p)] = counts.get(primary_tag(p), 0) + 1
                if "survey" in (p.topic_tags or []):
                    counts["survey"] = counts.get("survey", 0) + 1

    # Prefer surveys next (coverage goal)
    for p in candidates:
        if len(selected) >= args.max:
            break
        if p in selected:
            continue
        if "survey" in (p.topic_tags or []) and counts.get("survey", 0) < survey_target:
            selected.append(p)
            counts[primary_tag(p)] = counts.get(primary_tag(p), 0) + 1
            counts["survey"] = counts.get("survey", 0) + 1

    # Balanced theme fill
    for p in candidates:
        if len(selected) >= args.max:
            break
        if p in selected:
            continue
        tag = primary_tag(p)
        if counts[tag] < target_each or tag == "both":
            selected.append(p)
            counts[tag] = counts.get(tag, 0) + 1
            if "survey" in (p.topic_tags or []):
                counts["survey"] = counts.get("survey", 0) + 1

    # fill remainder by score
    for p in candidates:
        if len(selected) >= args.max:
            break
        if p not in selected:
            selected.append(p)
            counts[primary_tag(p)] = counts.get(primary_tag(p), 0) + 1
            if "survey" in (p.topic_tags or []):
                counts["survey"] = counts.get("survey", 0) + 1

    # If still under max but existing had papers filtered out, re-attach them
    if existing and len(selected) < args.max:
        for key, p in existing.items():
            if len(selected) >= args.max:
                break
            if any(s.dedupe_key() == key for s in selected):
                continue
            if not p.topic_tags:
                p.topic_tags = tag_topics(p.title, p.abstract) or ["self-evolution"]
            selected.append(p)
            counts[primary_tag(p)] = counts.get(primary_tag(p), 0) + 1

    log(f"Selected {len(selected)} / {len(candidates)} relevant (pool={len(pool)}, searched_raw={searched})")
    log(f"Theme mix: {counts}")

    # Ensure pdf urls for arxiv
    for p in selected:
        if p.arxiv_id and not p.pdf_url:
            p.pdf_url = f"https://arxiv.org/pdf/{p.arxiv_id}.pdf"

    # Download
    downloaded = skipped = failures = 0
    for i, p in enumerate(selected, 1):
        log(f"PDF [{i}/{len(selected)}]: {p.title[:70]}…")
        download_pdf(p, paths["pdfs"], dry_run=args.dry_run)
        if p.download_status == "ok":
            downloaded += 1
        elif p.download_status == "skipped":
            skipped += 1
        elif p.download_status == "dry-run":
            skipped += 1
        else:
            failures += 1
        time.sleep(1.0)  # be nice to arxiv CDN

    stats = {
        "searched": searched,
        "pool_unique": len(pool),
        "kept": len(selected),
        "downloaded": downloaded,
        "skipped": skipped,
        "failures": failures,
        "theme_counts": counts,
        "dry_run": args.dry_run,
    }
    write_manifest(paths["root"], selected, stats)
    write_index(paths["root"], selected, stats)

    log("=== DONE ===")
    log(json.dumps(stats, indent=2))
    log(f"manifest: {paths['root'] / 'manifest.json'}")
    log(f"index:    {paths['root'] / 'index.md'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
