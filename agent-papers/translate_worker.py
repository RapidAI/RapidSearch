#!/usr/bin/env python3
"""Run BabelDOC and install Chinese-only + bilingual PDFs under PAPERS_DIR.

Layout (under --out-root / PAPERS_DIR):

    pdfs/{original}.pdf
    pdfs/zh/{id}.zh.pdf      # monolingual Chinese
    pdfs/dual/{id}.dual.pdf  # bilingual Chinese–English

BabelDOC CLI (must be on PATH):

    uv tool install --python 3.12 BabelDOC

This helper is spawned by RapidSearch. The API key is read from
OPENAI_API_KEY (never printed). Stdout is a single JSON object.

After install, a completeness check compares EN vs ZH page counts and
per-page CJK / Latin density so near-blank body pages and still-English
(untranslated) pages are not marked done. An all-English first result is
retried once with --ignore-cache, then rejected if still English.
Quality defaults (disable with PAPERS_BABELDOC_NO_QUALITY=1):
  --disable-same-text-fallback, --translate-table-text,
  --custom-system-prompt (academic), glossary CSV, --min-text-length 3.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path


# Heuristics for near-empty / still-English translated pages
# (see RapidSearch papers bugs 2608.11274, 2606.03895).
_EN_SUBSTANTIAL_CHARS = 500
_ZH_MIN_CJK = 30
_EMPTY_RATIO_FAIL = 0.20
# Lots of Latin words on a "ZH" page with almost no CJK => BabelDOC no-op.
_ZH_STILL_EN_LATIN_WORDS = 40
# Prefer "still English" wording (and ignore-cache retry) when this share of
# pages failed the CJK density check.
_STILL_EN_PAGE_SHARE = 0.50

_CJK_RE = re.compile(r"[\u4e00-\u9fff]")
_LATIN_WORD_RE = re.compile(r"[A-Za-z]{2,}")
_REFS_HEAD_RE = re.compile(
    r"^\s*(references|bibliography|works\s+cited|参考文献|引用文献)\b",
    re.I | re.M,
)
_APPENDIX_HEAD_RE = re.compile(
    r"^\s*(appendix|appendices|附录|补充材料|"
    r"[A-Z]\s{1,4}[A-Z][a-z]|"  # e.g. "A    Limitations"
    r"局限|研究议程|research\s+agenda)\b",
    re.I | re.M,
)
_CITE_LINE_RE = re.compile(r"^\[\d+\]")


def log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def which_babeldoc() -> str | None:
    return shutil.which("babeldoc")



DEFAULT_ACADEMIC_SYSTEM_PROMPT = (
    "You are a professional Simplified Chinese (zh-CN) native translator for "
    "academic computer-science papers (AI agents, LLMs, security, IoT). "
    "Fully translate ALL prose into Simplified Chinese: titles, abstracts, "
    "introductions, body paragraphs, captions, footnotes, and table text. "
    "Do not leave English sentences or paragraphs untranslated. "
    "Keep unchanged: math formulas, code, file paths, URLs, DOI/arXiv IDs, "
    "citation markers like [12], author names, and placeholder tokens such as {1}. "
    "Prefer consistent terminology (AI agent → 智能体 unless it is a proper noun)."
)


def _env_truthy(name: str, default: bool = False) -> bool:
    raw = os.environ.get(name, "").strip().lower()
    if not raw:
        return default
    if raw in ("1", "true", "yes", "on"):
        return True
    if raw in ("0", "false", "no", "off"):
        return False
    return default


def build_quality_extra_args(
    *,
    script_dir: Path,
    enhance_compatibility: bool = False,
    translate_table_text: bool | None = None,
    ignore_cache: bool = False,
    disable_same_text_fallback: bool | None = None,
    custom_system_prompt: str | None = None,
    glossary_path: Path | None = None,
    min_text_length: int | None = None,
) -> list[str]:
    """Default BabelDOC quality flags for academic paper ZH translation.

    Opt out with env: PAPERS_BABELDOC_NO_QUALITY=1, or per-flag env vars.
    """
    extra: list[str] = []
    quality_on = not _env_truthy("PAPERS_BABELDOC_NO_QUALITY", False)

    if enhance_compatibility or _env_truthy("PAPERS_BABELDOC_ENHANCE_COMPAT"):
        extra.append("--enhance-compatibility")

    table_on = (
        translate_table_text
        if translate_table_text is not None
        else _env_truthy("PAPERS_BABELDOC_TRANSLATE_TABLES", quality_on)
    )
    if table_on:
        extra.append("--translate-table-text")

    if ignore_cache or _env_truthy("PAPERS_TRANSLATE_IGNORE_CACHE"):
        extra.append("--ignore-cache")

    same_fb = (
        disable_same_text_fallback
        if disable_same_text_fallback is not None
        else _env_truthy("PAPERS_BABELDOC_DISABLE_SAME_TEXT_FALLBACK", quality_on)
    )
    if same_fb:
        extra.append("--disable-same-text-fallback")

    prompt = custom_system_prompt
    if prompt is None:
        env_prompt = os.environ.get("PAPERS_BABELDOC_SYSTEM_PROMPT", "").strip()
        if env_prompt:
            prompt = env_prompt
        elif quality_on and not _env_truthy("PAPERS_BABELDOC_NO_SYSTEM_PROMPT"):
            prompt = DEFAULT_ACADEMIC_SYSTEM_PROMPT
    if prompt:
        extra.extend(["--custom-system-prompt", prompt])

    gloss = glossary_path
    if gloss is None:
        env_g = os.environ.get("PAPERS_BABELDOC_GLOSSARY", "").strip()
        if env_g:
            gloss = Path(env_g)
        elif quality_on and not _env_truthy("PAPERS_BABELDOC_NO_GLOSSARY"):
            cand = script_dir / "babeldoc-glossary-zh-CN.csv"
            if cand.is_file():
                gloss = cand
    if gloss is not None and Path(gloss).is_file():
        extra.extend(["--glossary-files", str(Path(gloss).resolve())])

    mtl = min_text_length
    if mtl is None:
        raw = os.environ.get("PAPERS_BABELDOC_MIN_TEXT_LENGTH", "").strip()
        if raw.isdigit():
            mtl = int(raw)
        elif quality_on:
            mtl = 3
    if mtl is not None and mtl > 0:
        extra.extend(["--min-text-length", str(mtl)])

    if raw := os.environ.get("PAPERS_BABELDOC_EXTRA_ARGS", "").strip():
        extra.extend(raw.split())
    return extra


def prefer_no_watermark(paths: list[Path]) -> Path | None:
    if not paths:
        return None
    for p in paths:
        if "no_watermark" in p.name.lower():
            return p
    return paths[0]


def collect_outputs(work: Path) -> tuple[Path | None, Path | None]:
    monos: list[Path] = []
    duals: list[Path] = []
    if not work.is_dir():
        return None, None
    for p in work.rglob("*.pdf"):
        low = p.name.lower()
        if "dual" in low:
            duals.append(p)
        elif "mono" in low:
            monos.append(p)
    return prefer_no_watermark(monos), prefer_no_watermark(duals)


def install_outputs(root: Path, pid: str, mono: Path | None, dual: Path | None) -> tuple[str, str]:
    zh_rel = ""
    dual_rel = ""
    if mono is not None:
        dest = root / "pdfs" / "zh" / f"{pid}.zh.pdf"
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(mono, dest)
        zh_rel = f"pdfs/zh/{pid}.zh.pdf"
    if dual is not None:
        dest = root / "pdfs" / "dual" / f"{pid}.dual.pdf"
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(dual, dest)
        dual_rel = f"pdfs/dual/{pid}.dual.pdf"
    return zh_rel, dual_rel


def remove_installed(root: Path, pid: str) -> None:
    for rel in (f"pdfs/zh/{pid}.zh.pdf", f"pdfs/dual/{pid}.dual.pdf"):
        try:
            (root / rel).unlink(missing_ok=True)
        except OSError:
            pass


def _toml_basic_string(value: str) -> str:
    """Escape a value for a TOML basic string (double-quoted)."""
    return value.replace("\\", "\\\\").replace('"', '\\"')


def _watermark_arg_rejected(output: str) -> bool:
    """True only when BabelDOC rejected the watermark flag as an argument."""
    low = (output or "").lower()
    if "watermark" not in low:
        return False
    return "unrecognized arguments" in low or "invalid choice" in low


def run_babeldoc(
    inp: Path,
    work: Path,
    *,
    model: str,
    base_url: str,
    qps: int,
    lang_in: str,
    lang_out: str,
    api_key: str,
    extra_args: list[str] | None = None,
) -> None:
    bin_path = which_babeldoc()
    if not bin_path:
        raise RuntimeError(
            "babeldoc not on PATH; install with: uv tool install --python 3.12 BabelDOC"
        )
    work.mkdir(parents=True, exist_ok=True)
    cmd = [
        bin_path,
        "--openai",
        "--openai-model",
        model,
        "--files",
        str(inp),
        "--lang-in",
        lang_in,
        "--lang-out",
        lang_out,
        "--output",
        str(work),
        "--watermark-output-mode=no_watermark",
    ]
    if base_url:
        cmd.extend(["--openai-base-url", base_url])
    if qps > 0:
        cmd.extend(["--qps", str(qps)])
    if extra_args:
        cmd.extend(extra_args)
    env = os.environ.copy()
    if api_key:
        # Keep env set; BabelDOC 0.6.4 still requires --config / --openai-api-key.
        env["OPENAI_API_KEY"] = api_key

    config_path: str | None = None
    try:
        if api_key:
            # Prefer temp TOML so `ps` does not show the key on argv.
            try:
                fd, config_path = tempfile.mkstemp(prefix="babeldoc-", suffix=".toml")
                os.close(fd)
                os.chmod(config_path, 0o600)
                Path(config_path).write_text(
                    f'[babeldoc]\nopenai-api-key = "{_toml_basic_string(api_key)}"\n',
                    encoding="utf-8",
                )
                cmd.extend(["--config", config_path])
            except OSError:
                if config_path:
                    try:
                        os.unlink(config_path)
                    except OSError:
                        pass
                    config_path = None
                # Fall back to CLI flag if temp config cannot be written.
                cmd.extend(["--openai-api-key", api_key])

        def invoke(argv: list[str]) -> subprocess.CompletedProcess[str]:
            return subprocess.run(
                argv,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                env=env,
                check=False,
            )

        proc = invoke(cmd)
        if proc.returncode != 0 and _watermark_arg_rejected(proc.stdout or ""):
            log("babeldoc rejected --watermark-output-mode; retrying without it")
            cmd = [a for a in cmd if not a.startswith("--watermark-output-mode")]
            proc = invoke(cmd)
        if proc.returncode != 0:
            snippet = (proc.stdout or "").strip()
            if len(snippet) > 800:
                snippet = snippet[-800:]
            raise RuntimeError(f"babeldoc exited {proc.returncode}: {snippet}")
    finally:
        if config_path:
            try:
                os.unlink(config_path)
            except OSError:
                pass


def pause_flag_path(root: Path) -> Path:
    return root / "translate.pause"


def translation_paused(root: Path) -> bool:
    """True when PAPERS_DIR/translate.pause exists (ops drain pause)."""
    try:
        return pause_flag_path(root).is_file()
    except OSError:
        return False


def llm_preflight(
    *,
    base_url: str,
    model: str,
    api_key: str,
    timeout: float = 45.0,
) -> tuple[bool, str]:
    """POST a tiny chat/completions before starting BabelDOC.

    Returns (ok, error_message). On HTTP 429/5xx/timeout/network errors,
    callers must NOT start BabelDOC (avoids English-only ZH PDFs).
    """
    base = (base_url or "").rstrip("/")
    if not base:
        return False, "LLM unavailable (no base_url); not starting BabelDOC"
    if not api_key:
        return False, "LLM unavailable (no API key); not starting BabelDOC"
    url = base + "/chat/completions"
    payload = {
        "model": model or "auto",
        "temperature": 0,
        "max_tokens": 16,
        "messages": [
            {
                "role": "user",
                "content": "Reply with exactly one Chinese character: 好",
            }
        ],
    }
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = getattr(resp, "status", 200) or 200
            raw = resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as e:
        code = int(getattr(e, "code", 0) or 0)
        detail = ""
        try:
            raw_err = e.read().decode("utf-8", errors="replace")
            obj = json.loads(raw_err) if raw_err.strip() else {}
            if isinstance(obj, dict):
                hub_code = str(obj.get("code") or "")
                msg = str(obj.get("message") or "")
                err = obj.get("error") if isinstance(obj.get("error"), dict) else {}
                if not hub_code and err:
                    hub_code = str(err.get("code") or "")
                if not msg and err:
                    msg = str(err.get("message") or "")
                if hub_code or msg:
                    detail = f"{hub_code}: {msg}".strip(": ")
        except Exception:  # noqa: BLE001
            detail = ""
        # Always surface Hub code/message so ops can triage (not bare "403").
        if detail:
            hint = ""
            low = detail.lower()
            if code == 403 and ("credit" in low or "exhausted" in low):
                hint = (
                    " — top up on Hub or wait for the grant/period window to reset"
                )
            elif code == 429:
                hint = " — backoff 2–5 min then retry"
            elif code >= 500:
                hint = " — backoff and retry when Hub recovers"
            return (
                False,
                f"LLM unavailable (HTTP {code} {detail}); not starting BabelDOC"
                f"{hint}",
            )
        if code == 429:
            return (
                False,
                "LLM unavailable (HTTP 429); not starting BabelDOC — "
                "Hub rate / period limit; backoff 2–5 min then retry",
            )
        if code >= 500:
            return (
                False,
                f"LLM unavailable (HTTP {code}); not starting BabelDOC — "
                "backoff and retry when Hub recovers",
            )
        return False, f"LLM unavailable (HTTP {code}); not starting BabelDOC"
    except TimeoutError:
        return (
            False,
            "LLM unavailable (timeout); not starting BabelDOC — "
            "backoff and retry",
        )
    except Exception as e:  # noqa: BLE001
        err = str(e).strip() or e.__class__.__name__
        # urllib raises URLError wrapping timeout on some platforms
        low = err.lower()
        if "timed out" in low or "timeout" in low:
            return (
                False,
                "LLM unavailable (timeout); not starting BabelDOC — "
                "backoff and retry",
            )
        return False, f"LLM unavailable ({err}); not starting BabelDOC"

    if status == 429 or status >= 500:
        return (
            False,
            f"LLM unavailable (HTTP {status}); not starting BabelDOC — "
            "backoff and retry",
        )
    if status < 200 or status >= 300:
        return False, f"LLM unavailable (HTTP {status}); not starting BabelDOC"
    # Soft check: response parseable JSON (do not require Chinese here —
    # some models ignore the prompt; HTTP success is enough to start).
    try:
        json.loads(raw)
    except json.JSONDecodeError:
        return False, "LLM unavailable (invalid JSON); not starting BabelDOC"
    return True, "ok"


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def pdf_page_count(path: Path) -> int:
    """Return page count via pdfinfo; 0 if unavailable."""
    try:
        out = subprocess.check_output(
            ["pdfinfo", str(path)],
            text=True,
            errors="replace",
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError):
        return 0
    for line in out.splitlines():
        if line.lower().startswith("pages:"):
            try:
                return int(line.split(":", 1)[1].strip())
            except ValueError:
                return 0
    return 0


def pdf_page_text(path: Path, page: int) -> str:
    """Extract text for a 1-based page via pdftotext."""
    try:
        return subprocess.check_output(
            ["pdftotext", "-f", str(page), "-l", str(page), "-layout", str(path), "-"],
            text=True,
            errors="replace",
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError):
        return ""


def count_cjk(text: str) -> int:
    return len(_CJK_RE.findall(text or ""))


def count_latin_words(text: str) -> int:
    return len(_LATIN_WORD_RE.findall(text or ""))


def format_page_list(pages: list[int]) -> str:
    """Compact consecutive pages: [1, 2, 3, 4] -> [1..4]."""
    if not pages:
        return "[]"
    pages = sorted({int(p) for p in pages})
    parts: list[str] = []
    start = prev = pages[0]
    for n in pages[1:]:
        if n == prev + 1:
            prev = n
            continue
        parts.append(f"{start}..{prev}" if prev > start else str(start))
        start = prev = n
    parts.append(f"{start}..{prev}" if prev > start else str(start))
    return "[" + ", ".join(parts) + "]"


def should_retry_english_only(details: dict) -> bool:
    """True when BabelDOC likely returned untranslated English on body pages.

    Only the all-English / zero-CJK case is retried (majority of pages).
    A few leftover English pages do not trigger another full BabelDOC run.
    """
    still = list(details.get("still_english_non_refs") or [])
    empty_nr = list(details.get("empty_non_refs") or [])
    en_n = int(details.get("en_pages") or 0)
    if not still or en_n <= 0:
        return False
    # All-English / zero CJK on body pages.
    if (len(still) / en_n) > _STILL_EN_PAGE_SHARE:
        return True
    if empty_nr and (len(empty_nr) / en_n) > _STILL_EN_PAGE_SHARE:
        return set(still) >= set(empty_nr) or len(still) >= len(empty_nr)
    return False


def is_references_page(en_text: str, prev_was_refs: bool) -> bool:
    """Heuristic: bibliography section / citation-heavy continuation pages."""
    t = (en_text or "").strip()
    if not t:
        return prev_was_refs
    if _APPENDIX_HEAD_RE.search(t[:400]):
        return False
    if _REFS_HEAD_RE.search(t[:400]):
        return True
    if not prev_was_refs:
        return False
    lines = [ln.strip() for ln in t.splitlines() if ln.strip()]
    if not lines:
        return True
    cite_like = sum(1 for ln in lines if _CITE_LINE_RE.match(ln))
    # Continuation refs: many [N] lines and no clear appendix heading.
    return cite_like >= max(2, len(lines) // 4)


def check_translation_completeness(
    en_pdf: Path, zh_pdf: Path
) -> tuple[bool, str, dict]:
    """Compare EN vs ZH; fail on blank body/appendix or high empty ratio.

    Returns (ok, message, details). References-only sparsity under the empty
    ratio threshold is accepted with a warning message (ok=True).
    """
    details: dict = {
        "en_pages": 0,
        "zh_pages": 0,
        "empty_pages": [],
        "empty_non_refs": [],
        "still_english_pages": [],
        "still_english_non_refs": [],
        "blank_non_refs": [],
        "refs_pages": [],
        "empty_ratio": 0.0,
        "still_english": False,
    }
    if not en_pdf.is_file() or not zh_pdf.is_file():
        return False, "completeness check: missing EN or ZH PDF", details

    en_n = pdf_page_count(en_pdf)
    zh_n = pdf_page_count(zh_pdf)
    details["en_pages"] = en_n
    details["zh_pages"] = zh_n
    if en_n <= 0 or zh_n <= 0:
        # Tools missing or unreadable — do not block install.
        log("completeness check: pdfinfo unavailable or zero pages; skipping density check")
        return True, "skipped (pdfinfo unavailable)", details
    if en_n != zh_n:
        return (
            False,
            f"page count mismatch: EN={en_n} ZH={zh_n}",
            details,
        )

    empty_pages: list[int] = []
    refs_pages: list[int] = []
    empty_non_refs: list[int] = []
    still_english_pages: list[int] = []
    still_english_non_refs: list[int] = []
    blank_non_refs: list[int] = []
    prev_refs = False
    page_stats: list[dict] = []

    for i in range(1, en_n + 1):
        en_t = pdf_page_text(en_pdf, i)
        zh_t = pdf_page_text(zh_pdf, i)
        en_chars = len(en_t.strip())
        cjk = count_cjk(zh_t)
        latin = count_latin_words(zh_t)
        is_refs = is_references_page(en_t, prev_refs)
        prev_refs = is_refs
        if is_refs:
            refs_pages.append(i)
        # Low CJK while EN has substantial text: either still-English or blank.
        is_empty = en_chars > _EN_SUBSTANTIAL_CHARS and cjk < _ZH_MIN_CJK
        still_en = is_empty and latin >= _ZH_STILL_EN_LATIN_WORDS
        page_stats.append(
            {
                "page": i,
                "en_chars": en_chars,
                "zh_cjk": cjk,
                "zh_latin_words": latin,
                "refs": is_refs,
                "empty": is_empty,
                "still_english": still_en,
            }
        )
        if still_en:
            still_english_pages.append(i)
        if is_empty:
            empty_pages.append(i)
            if not is_refs:
                empty_non_refs.append(i)
                if still_en:
                    still_english_non_refs.append(i)
                else:
                    blank_non_refs.append(i)

    details["empty_pages"] = empty_pages
    details["empty_non_refs"] = empty_non_refs
    details["still_english_pages"] = still_english_pages
    details["still_english_non_refs"] = still_english_non_refs
    details["blank_non_refs"] = blank_non_refs
    details["refs_pages"] = refs_pages
    details["empty_ratio"] = (len(empty_pages) / en_n) if en_n else 0.0
    details["still_english"] = bool(still_english_non_refs)
    details["pages"] = page_stats

    if empty_non_refs:
        empty_share = (len(empty_non_refs) / en_n) if en_n else 0.0
        # Eng-heavy / majority failure: say "still English", not "near-empty".
        if still_english_non_refs and (
            empty_share > _STILL_EN_PAGE_SHARE
            or len(still_english_non_refs) >= len(blank_non_refs)
        ):
            return (
                False,
                (
                    "incomplete translation: ZH still English on pages "
                    f"{format_page_list(still_english_non_refs)} "
                    "(no Chinese text extracted)"
                ),
                details,
            )
        if blank_non_refs and not still_english_non_refs:
            return (
                False,
                (
                    "incomplete translation: near-empty body/appendix pages "
                    f"{format_page_list(blank_non_refs)} "
                    f"(EN text present, ZH CJK<{_ZH_MIN_CJK})"
                ),
                details,
            )
        # Mixed: mention both, still-English first.
        bits = []
        if still_english_non_refs:
            bits.append(
                "ZH still English on pages "
                f"{format_page_list(still_english_non_refs)} "
                "(no Chinese text extracted)"
            )
        if blank_non_refs:
            bits.append(
                "near-empty body/appendix pages "
                f"{format_page_list(blank_non_refs)} "
                f"(EN text present, ZH CJK<{_ZH_MIN_CJK})"
            )
        return False, "incomplete translation: " + "; ".join(bits), details
    if details["empty_ratio"] > _EMPTY_RATIO_FAIL:
        return (
            False,
            (
                f"incomplete translation: empty_ratio={details['empty_ratio']:.0%} "
                f"> {_EMPTY_RATIO_FAIL:.0%} (empty pages {format_page_list(empty_pages)})"
            ),
            details,
        )
    if empty_pages:
        msg = (
            f"warning: sparse references-only pages {empty_pages} "
            "(accepted; body/appendix OK)"
        )
        log(msg)
        return True, msg, details
    return True, "ok", details


def main() -> int:
    p = argparse.ArgumentParser(description="BabelDOC wrapper for RapidSearch papers")
    p.add_argument("--check", action="store_true", help="Print babeldoc availability and exit")
    p.add_argument(
        "--check-completeness",
        action="store_true",
        help="Compare --input (EN) vs --zh PDF density and exit",
    )
    p.add_argument("--input", type=Path, help="Source English PDF")
    p.add_argument("--zh", type=Path, default=None, help="Chinese PDF (for --check-completeness)")
    p.add_argument("--id", default="", help="Paper id (arxiv or filename stem)")
    p.add_argument("--out-root", type=Path, default=None, help="PAPERS_DIR root")
    p.add_argument("--base-url", default="", help="OpenAI-compatible base URL")
    p.add_argument("--model", default="gpt-4o-mini")
    p.add_argument("--qps", type=int, default=4)
    p.add_argument("--lang-in", default="en")
    p.add_argument("--lang-out", default="zh-CN")
    p.add_argument(
        "--enhance-compatibility",
        action="store_true",
        help="Pass --enhance-compatibility to BabelDOC",
    )
    p.add_argument(
        "--translate-table-text",
        action="store_true",
        help="Pass --translate-table-text to BabelDOC",
    )
    p.add_argument(
        "--ignore-cache",
        action="store_true",
        help="Pass --ignore-cache to BabelDOC",
    )
    args = p.parse_args()

    if args.check:
        emit({"ok": True, "babeldoc": bool(which_babeldoc())})
        return 0

    if args.check_completeness:
        if args.input is None or args.zh is None:
            emit({"ok": False, "error": "--input and --zh required for --check-completeness"})
            return 2
        ok, msg, details = check_translation_completeness(
            args.input.expanduser().resolve(),
            args.zh.expanduser().resolve(),
        )
        emit({"ok": ok, "message": msg, "details": details})
        return 0 if ok else 1

    if args.input is None or args.out_root is None or not args.id:
        emit({"ok": False, "error": "--input, --id, and --out-root are required"})
        return 2

    inp = args.input.expanduser().resolve()
    root = args.out_root.expanduser().resolve()
    pid = args.id.strip().replace("/", "_")
    if not inp.is_file():
        emit({"ok": False, "error": "input PDF not found"})
        return 2

    api_key = os.environ.get("OPENAI_API_KEY", "").strip()
    work = root / "translate-work" / pid
    extra = build_quality_extra_args(
        script_dir=Path(__file__).resolve().parent,
        enhance_compatibility=args.enhance_compatibility,
        translate_table_text=True if args.translate_table_text else None,
        ignore_cache=args.ignore_cache,
        disable_same_text_fallback=True if getattr(args, "disable_same_text_fallback", False) else None,
        custom_system_prompt=(getattr(args, "custom_system_prompt", "") or "").strip() or None,
        glossary_path=getattr(args, "glossary", None),
        min_text_length=getattr(args, "min_text_length", None),
    )

    if translation_paused(root):
        emit(
            {
                "ok": False,
                "error": (
                    "translation paused (translate.pause present); "
                    "not starting BabelDOC"
                ),
            }
        )
        return 1

    ok_pf, pf_msg = llm_preflight(
        base_url=args.base_url.strip(),
        model=args.model,
        api_key=api_key,
    )
    if not ok_pf:
        log(pf_msg)
        emit({"ok": False, "error": pf_msg})
        return 1

    try:
        def run_once(run_extra: list[str]) -> tuple[str, str, bool, str, dict]:
            run_babeldoc(
                inp,
                work,
                model=args.model,
                base_url=args.base_url.strip(),
                qps=args.qps,
                lang_in=args.lang_in,
                lang_out=args.lang_out,
                api_key=api_key,
                extra_args=run_extra or None,
            )
            mono, dual = collect_outputs(work)
            zh_rel, dual_rel = install_outputs(root, pid, mono, dual)
            if not zh_rel and not dual_rel:
                return "", "", False, "babeldoc produced no output PDFs", {}
            if not zh_rel or not dual_rel:
                remove_installed(root, pid)
                return (
                    zh_rel,
                    dual_rel,
                    False,
                    "babeldoc produced incomplete output (need both zh and dual)",
                    {},
                )
            ok, msg, details = check_translation_completeness(inp, root / zh_rel)
            return zh_rel, dual_rel, ok, msg, details

        zh_rel, dual_rel, ok, msg, details = run_once(extra)
        ignore_cache_retry = False
        if (not ok) and details and should_retry_english_only(details):
            log(
                f"completeness failed for {pid}: {msg}; "
                "retrying babeldoc once with --ignore-cache"
            )
            remove_installed(root, pid)
            if work.is_dir():
                shutil.rmtree(work, ignore_errors=True)
            retry_extra = list(extra)
            if "--ignore-cache" not in retry_extra:
                retry_extra.append("--ignore-cache")
            zh_rel, dual_rel, ok, msg, details = run_once(retry_extra)
            ignore_cache_retry = True
            if details:
                details = dict(details)
                details["ignore_cache_retry"] = True

        if not zh_rel and not dual_rel:
            emit({"ok": False, "error": msg or "babeldoc produced no output PDFs"})
            return 1
        if not zh_rel or not dual_rel:
            remove_installed(root, pid)
            emit(
                {
                    "ok": False,
                    "error": msg or "babeldoc produced incomplete output (need both zh and dual)",
                    "zh": zh_rel,
                    "dual": dual_rel,
                }
            )
            return 1

        if not ok:
            log(f"completeness failed for {pid}: {msg}")
            remove_installed(root, pid)
            fail: dict = {"ok": False, "error": msg, "completeness": details}
            if ignore_cache_retry:
                fail["ignore_cache_retry"] = True
            emit(fail)
            return 1

        out: dict = {"ok": True, "zh": zh_rel, "dual": dual_rel}
        if ignore_cache_retry:
            out["ignore_cache_retry"] = True
        if msg and msg != "ok":
            out["warning"] = msg
            out["completeness"] = details
        emit(out)
        return 0
    except Exception as e:  # noqa: BLE001 — surface to Go as JSON
        emit({"ok": False, "error": str(e)})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
