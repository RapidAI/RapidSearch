#!/usr/bin/env python3
"""Run BabelDOC and install Chinese-only + bilingual PDFs under PAPERS_DIR.

Layout (under --out-root / PAPERS_DIR):

    pdfs/{original}.pdf
    pdfs/zh/{id}.zh.pdf      # monolingual Chinese
    pdfs/dual/{id}.dual.pdf  # bilingual Chinese–English

BabelDOC CLI (must be on PATH):

    uv tool install --python 3.12 BabelDOC

Quality defaults (baked in here so Go can stay thin; opt out via env/CLI/config):

    --disable-same-text-fallback   ON
    --translate-table-text         ON
    --custom-system-prompt         academic CS → zh-CN (all prose)
    --min-text-length              3 (optional; omit with 0)
    --glossary-files               optional CSV list
    --enhance-compatibility        OFF (do not default)

Also: LLM preflight, optional pause file / delay, mono-ZH completeness
check, and one --ignore-cache retry. The API key is read from
OPENAI_API_KEY or a BabelDOC TOML config — never from argv. Stdout is
a single JSON object.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Callable, Iterable


DEFAULT_SYSTEM_PROMPT = """You are a professional Simplified Chinese (zh-CN) native translator for academic computer-science papers (AI, systems, security, agents).

Translate ALL human-readable prose into fluent Simplified Chinese: title, abstract, section headings, body paragraphs, figure/table captions, footnotes, and table cell text. Do not leave English sentences or paragraphs untranslated. Title-only Chinese with an English abstract or body is a failure.

Keep unchanged:
- mathematical formulas, equation numbers, and LaTeX-like notation
- source code, commands, file paths, and API / function / class identifiers
- paper IDs, arXiv IDs, DOIs, URLs, and citation keys such as [12] or (Smith et al., 2024)
- author names (you may translate university or department names)

Preserve meaning, numbering, placeholders, and XML/HTML-like tags exactly. Prefer established Chinese CS terminology. Output only the translation for each unit.
Follow all rules strictly."""

QUALITY_CONFIG_NAME = "translate-quality.json"
DEFAULT_PAUSE_FILE_NAME = "translate.pause"
SECRET_ARGV_FLAGS = ("--openai-api-key", "--openai_api_key")

# Optional BabelDOC flags we may strip if an older CLI rejects them.
OPTIONAL_FLAG_SPECS: dict[str, bool] = {
    "--disable-same-text-fallback": True,
    "--translate-table-text": True,
    "--enhance-compatibility": True,
    "--ignore-cache": True,
    "--custom-system-prompt": False,
    "--glossary-files": False,
    "--min-text-length": False,
    "--watermark-output-mode": False,
}

# Completeness: catch majority-English mono ZH (title Chinese, body English)
# without failing a good paper that keeps IDs / citations / English terms.
CJK_RE = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")
LATIN_RE = re.compile(r"[A-Za-z]")
BIBLIO_HEAD_RE = re.compile(
    r"^\s*(\d+\s*[\.\)]\s*)?(references|bibliography|参考文献|參考文獻)\b",
    re.IGNORECASE,
)
CITE_TOKEN_RE = re.compile(
    r"\((?:[A-Z][A-Za-z\-]+(?:\s+(?:and|&)\s+[A-Z][A-Za-z\-]+)*(?:,\s*\d{4}[a-z]?)?)\)"
    r"|\[\d+(?:\s*[,–-]\s*\d+)*\]"
    r"|arXiv:\d{4}\.\d{4,5}(?:v\d+)?"
    r"|10\.\d{4,9}/[-._;()/:A-Za-z0-9]+"
    r"|https?://\S+",
    re.IGNORECASE,
)


InvokeFn = Callable[[list[str]], subprocess.CompletedProcess[str]]
SleepFn = Callable[[float], None]


def log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def which_babeldoc() -> str | None:
    return shutil.which("babeldoc")


def parse_bool(raw: str | None, default: bool) -> bool:
    if raw is None:
        return default
    s = str(raw).strip().lower()
    if s in {"1", "true", "yes", "on"}:
        return True
    if s in {"0", "false", "no", "off", ""}:
        return False
    return default


@dataclass
class QualitySettings:
    disable_same_text_fallback: bool = True
    translate_table_text: bool = True
    min_text_length: int | None = 3
    custom_system_prompt: str = DEFAULT_SYSTEM_PROMPT
    glossary_files: str = ""
    enhance_compatibility: bool = False
    preflight: bool = True
    completeness: bool = True
    ignore_cache_retry: bool = True
    pause_seconds: float = 0.0
    pause_file: str = ""

    def public_dict(self) -> dict[str, Any]:
        d = asdict(self)
        prompt = d.get("custom_system_prompt") or ""
        d["custom_system_prompt"] = bool(prompt.strip())
        d["custom_system_prompt_chars"] = len(prompt)
        d["glossary_files"] = bool(str(d.get("glossary_files") or "").strip())
        return d


@dataclass
class CompletenessReport:
    ok: bool
    reason: str = ""
    extracted: bool = False
    cjk_chars: int = 0
    latin_letters: int = 0
    cjk_ratio: float = 0.0
    residual_en_paragraphs: int = 0
    scored_paragraphs: int = 0

    def public_dict(self) -> dict[str, Any]:
        return asdict(self)


def _load_quality_file(path: Path) -> dict[str, Any]:
    if not path.is_file():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        log(f"quality config ignored ({path}): {e}")
        return {}
    return data if isinstance(data, dict) else {}


def _apply_mapping(q: QualitySettings, data: dict[str, Any]) -> None:
    if "disable_same_text_fallback" in data:
        q.disable_same_text_fallback = bool(data["disable_same_text_fallback"])
    if "translate_table_text" in data:
        q.translate_table_text = bool(data["translate_table_text"])
    if "enhance_compatibility" in data:
        q.enhance_compatibility = bool(data["enhance_compatibility"])
    if "preflight" in data:
        q.preflight = bool(data["preflight"])
    if "completeness" in data:
        q.completeness = bool(data["completeness"])
    if "ignore_cache_retry" in data:
        q.ignore_cache_retry = bool(data["ignore_cache_retry"])
    if "min_text_length" in data:
        try:
            n = int(data["min_text_length"])
        except (TypeError, ValueError):
            n = 0
        q.min_text_length = n if n > 0 else None
    if "custom_system_prompt" in data and data["custom_system_prompt"] is not None:
        q.custom_system_prompt = str(data["custom_system_prompt"])
    if "glossary_files" in data and data["glossary_files"] is not None:
        q.glossary_files = str(data["glossary_files"]).strip()
    if "pause_seconds" in data:
        try:
            q.pause_seconds = max(0.0, float(data["pause_seconds"]))
        except (TypeError, ValueError):
            pass
    if "pause_file" in data and data["pause_file"] is not None:
        q.pause_file = str(data["pause_file"]).strip()


def _existing_glossary(paths: Iterable[str]) -> str:
    kept: list[str] = []
    for raw in paths:
        for part in str(raw).split(","):
            p = part.strip()
            if not p:
                continue
            if Path(p).expanduser().is_file():
                kept.append(str(Path(p).expanduser().resolve()))
            else:
                log(f"glossary skipped (missing): {p}")
    # Preserve order, drop dupes.
    out: list[str] = []
    seen: set[str] = set()
    for p in kept:
        if p not in seen:
            seen.add(p)
            out.append(p)
    return ",".join(out)


def resolve_quality_settings(
    root: Path | None,
    args: argparse.Namespace | None = None,
    environ: dict[str, str] | None = None,
) -> QualitySettings:
    """Merge defaults ← quality JSON ← env ← CLI (CLI wins)."""
    q = QualitySettings()
    env = environ if environ is not None else os.environ

    cfg_path = ""
    if env.get("PAPERS_TRANSLATE_QUALITY_CONFIG"):
        cfg_path = env["PAPERS_TRANSLATE_QUALITY_CONFIG"].strip()
    elif root is not None:
        cfg_path = str(root / QUALITY_CONFIG_NAME)
    if cfg_path:
        _apply_mapping(q, _load_quality_file(Path(cfg_path).expanduser()))

    if "PAPERS_TRANSLATE_DISABLE_SAME_TEXT_FALLBACK" in env:
        q.disable_same_text_fallback = parse_bool(
            env.get("PAPERS_TRANSLATE_DISABLE_SAME_TEXT_FALLBACK"), True
        )
    if "PAPERS_TRANSLATE_TABLE_TEXT" in env:
        q.translate_table_text = parse_bool(env.get("PAPERS_TRANSLATE_TABLE_TEXT"), True)
    if "PAPERS_TRANSLATE_ENHANCE_COMPATIBILITY" in env:
        q.enhance_compatibility = parse_bool(
            env.get("PAPERS_TRANSLATE_ENHANCE_COMPATIBILITY"), False
        )
    if "PAPERS_TRANSLATE_PREFLIGHT" in env:
        q.preflight = parse_bool(env.get("PAPERS_TRANSLATE_PREFLIGHT"), True)
    if "PAPERS_TRANSLATE_COMPLETENESS" in env:
        q.completeness = parse_bool(env.get("PAPERS_TRANSLATE_COMPLETENESS"), True)
    if "PAPERS_TRANSLATE_IGNORE_CACHE_RETRY" in env:
        q.ignore_cache_retry = parse_bool(
            env.get("PAPERS_TRANSLATE_IGNORE_CACHE_RETRY"), True
        )
    if "PAPERS_TRANSLATE_MIN_TEXT_LENGTH" in env:
        raw = (env.get("PAPERS_TRANSLATE_MIN_TEXT_LENGTH") or "").strip()
        if raw in {"", "0", "none", "default"}:
            q.min_text_length = None
        else:
            try:
                n = int(raw)
            except ValueError:
                n = 3
            q.min_text_length = n if n > 0 else None
    prompt_file = (env.get("PAPERS_TRANSLATE_SYSTEM_PROMPT_FILE") or "").strip()
    if prompt_file:
        try:
            q.custom_system_prompt = Path(prompt_file).expanduser().read_text(encoding="utf-8")
        except OSError as e:
            log(f"system prompt file ignored ({prompt_file}): {e}")
    if "PAPERS_TRANSLATE_SYSTEM_PROMPT" in env:
        q.custom_system_prompt = env.get("PAPERS_TRANSLATE_SYSTEM_PROMPT") or ""
    if "PAPERS_TRANSLATE_GLOSSARY_FILES" in env:
        q.glossary_files = (env.get("PAPERS_TRANSLATE_GLOSSARY_FILES") or "").strip()
    if "PAPERS_TRANSLATE_PAUSE_SECONDS" in env:
        try:
            q.pause_seconds = max(0.0, float(env.get("PAPERS_TRANSLATE_PAUSE_SECONDS") or 0))
        except ValueError:
            pass
    if "PAPERS_TRANSLATE_PAUSE_FILE" in env:
        q.pause_file = (env.get("PAPERS_TRANSLATE_PAUSE_FILE") or "").strip()

    if args is not None:
        if getattr(args, "disable_same_text_fallback", None) is not None:
            q.disable_same_text_fallback = bool(args.disable_same_text_fallback)
        if getattr(args, "translate_table_text", None) is not None:
            q.translate_table_text = bool(args.translate_table_text)
        if getattr(args, "enhance_compatibility", None) is not None:
            q.enhance_compatibility = bool(args.enhance_compatibility)
        if getattr(args, "preflight", None) is not None:
            q.preflight = bool(args.preflight)
        if getattr(args, "completeness", None) is not None:
            q.completeness = bool(args.completeness)
        if getattr(args, "ignore_cache_retry", None) is not None:
            q.ignore_cache_retry = bool(args.ignore_cache_retry)
        if getattr(args, "min_text_length", None) is not None:
            n = int(args.min_text_length)
            q.min_text_length = n if n > 0 else None
        if getattr(args, "custom_system_prompt", None) is not None:
            q.custom_system_prompt = str(args.custom_system_prompt)
        if getattr(args, "glossary_files", None) is not None:
            q.glossary_files = str(args.glossary_files).strip()
        if getattr(args, "pause_seconds", None) is not None:
            q.pause_seconds = max(0.0, float(args.pause_seconds))

    # Auto-pick a local glossary when none was configured.
    if not q.glossary_files and root is not None:
        for cand in (root / "glossary.csv", root / "translate-glossary.csv"):
            if cand.is_file():
                q.glossary_files = str(cand.resolve())
                break
    if q.glossary_files:
        q.glossary_files = _existing_glossary(q.glossary_files.split(","))

    if not q.pause_file and root is not None:
        default_pause = root / DEFAULT_PAUSE_FILE_NAME
        q.pause_file = str(default_pause)
    return q


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


def argv_has_secret(argv: list[str]) -> bool:
    for i, a in enumerate(argv):
        low = a.lower()
        for flag in SECRET_ARGV_FLAGS:
            if low == flag or low.startswith(flag + "="):
                return True
            if i > 0 and argv[i - 1].lower() == flag:
                return True
    return False


def build_babeldoc_cmd(
    bin_path: str,
    inp: Path,
    work: Path,
    *,
    model: str,
    base_url: str,
    qps: int,
    lang_in: str,
    lang_out: str,
    quality: QualitySettings,
    ignore_cache: bool = False,
) -> list[str]:
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
    if quality.disable_same_text_fallback:
        cmd.append("--disable-same-text-fallback")
    if quality.translate_table_text:
        cmd.append("--translate-table-text")
    if quality.enhance_compatibility:
        cmd.append("--enhance-compatibility")
    if quality.min_text_length is not None and quality.min_text_length > 0:
        cmd.extend(["--min-text-length", str(quality.min_text_length)])
    prompt = (quality.custom_system_prompt or "").strip()
    if prompt:
        cmd.extend(["--custom-system-prompt", prompt])
    if quality.glossary_files:
        cmd.extend(["--glossary-files", quality.glossary_files])
    if ignore_cache:
        cmd.append("--ignore-cache")
    if argv_has_secret(cmd):
        raise RuntimeError("internal error: API key must not appear on babeldoc argv")
    return cmd


def strip_optional_flags(argv: list[str], names: Iterable[str]) -> list[str]:
    drop = set(names)
    out: list[str] = []
    skip_value = False
    for a in argv:
        if skip_value:
            skip_value = False
            continue
        key = a.split("=", 1)[0]
        if key in drop:
            if "=" not in a and not OPTIONAL_FLAG_SPECS.get(key, True):
                skip_value = True
            continue
        out.append(a)
    return out


def flags_mentioned_in_output(output: str) -> list[str]:
    found: list[str] = []
    low = output.lower()
    for name in OPTIONAL_FLAG_SPECS:
        if name.lower() in low or name.lstrip("-").lower() in low.replace("_", "-"):
            found.append(name)
    return found


def _paragraphs(text: str) -> list[str]:
    chunks = re.split(r"\n\s*\n+", text.replace("\r\n", "\n"))
    if len(chunks) == 1:
        chunks = [ln.strip() for ln in text.splitlines() if ln.strip()]
    return [c.strip() for c in chunks if c.strip()]


def _strip_keep_tokens(text: str) -> str:
    return CITE_TOKEN_RE.sub(" ", text)


def analyze_zh_text(text: str) -> CompletenessReport:
    """Score extracted mono-ZH text for residual English prose."""
    raw = (text or "").strip()
    if len(raw) < 80:
        return CompletenessReport(ok=True, reason="too little text to score", extracted=False)

    in_biblio = False
    scored = 0
    residual = 0
    cjk_total = 0
    latin_total = 0
    for para in _paragraphs(raw):
        if BIBLIO_HEAD_RE.match(para):
            in_biblio = True
        if in_biblio:
            continue
        cleaned = _strip_keep_tokens(para)
        cjk = len(CJK_RE.findall(cleaned))
        latin = len(LATIN_RE.findall(cleaned))
        letters = cjk + latin
        if letters < 40:
            continue
        cjk_total += cjk
        latin_total += latin
        scored += 1
        latin_ratio = latin / letters if letters else 0.0
        # Long English prose with almost no Chinese — residual body block.
        if letters >= 80 and latin_ratio >= 0.72 and cjk < 16:
            residual += 1

    letters = cjk_total + latin_total
    ratio = (cjk_total / letters) if letters else 0.0
    if letters < 120 or scored == 0:
        return CompletenessReport(
            ok=True,
            reason="too little prose to score",
            extracted=False,
            cjk_chars=cjk_total,
            latin_letters=latin_total,
            cjk_ratio=round(ratio, 4),
            residual_en_paragraphs=residual,
            scored_paragraphs=scored,
        )

    if letters >= 400 and ratio < 0.20:
        return CompletenessReport(
            ok=False,
            reason=f"majority English (cjk_ratio={ratio:.2f})",
            extracted=True,
            cjk_chars=cjk_total,
            latin_letters=latin_total,
            cjk_ratio=round(ratio, 4),
            residual_en_paragraphs=residual,
            scored_paragraphs=scored,
        )
    if residual >= 3 and residual / max(scored, 1) >= 0.25:
        return CompletenessReport(
            ok=False,
            reason=f"mixed residual English ({residual}/{scored} body paragraphs)",
            extracted=True,
            cjk_chars=cjk_total,
            latin_letters=latin_total,
            cjk_ratio=round(ratio, 4),
            residual_en_paragraphs=residual,
            scored_paragraphs=scored,
        )
    return CompletenessReport(
        ok=True,
        reason="ok",
        extracted=True,
        cjk_chars=cjk_total,
        latin_letters=latin_total,
        cjk_ratio=round(ratio, 4),
        residual_en_paragraphs=residual,
        scored_paragraphs=scored,
    )


def extract_pdf_text(path: Path) -> str:
    """Best-effort text extract for completeness; empty means skip the check."""
    try:
        from pypdf import PdfReader  # type: ignore

        reader = PdfReader(str(path))
        parts = [(page.extract_text() or "") for page in reader.pages]
        text = "\n\n".join(parts).strip()
        if text:
            return text
    except Exception:
        pass
    pdftotext = shutil.which("pdftotext")
    if pdftotext:
        try:
            proc = subprocess.run(
                [pdftotext, "-layout", str(path), "-"],
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                check=False,
            )
            if proc.returncode == 0 and (proc.stdout or "").strip():
                return proc.stdout
        except OSError:
            pass
    return ""


def check_mono_completeness(mono: Path | None) -> CompletenessReport:
    if mono is None or not Path(mono).is_file():
        return CompletenessReport(ok=True, reason="no mono PDF to score", extracted=False)
    text = extract_pdf_text(Path(mono))
    if not text.strip():
        return CompletenessReport(ok=True, reason="could not extract PDF text", extracted=False)
    return analyze_zh_text(text)


def _join_openai_urls(base: str, suffix: str) -> list[str]:
    base = (base or "").strip().rstrip("/")
    if not base:
        base = "https://api.openai.com/v1"
    urls = [base + suffix]
    if not base.endswith("/v1"):
        urls.append(base + "/v1" + suffix)
    return urls


def preflight_llm(
    base_url: str,
    api_key: str,
    model: str,
    *,
    timeout: float = 20.0,
    opener: Callable[..., Any] | None = None,
) -> str:
    """Ping models, then a tiny chat completion. Never logs the key."""
    if not api_key.strip():
        raise RuntimeError("LLM preflight failed: OPENAI_API_KEY is empty")
    model = (model or "gpt-4o-mini").strip()
    open_fn = opener or urllib.request.urlopen

    def do(req: urllib.request.Request) -> tuple[int, bytes]:
        try:
            with open_fn(req, timeout=timeout) as resp:
                body = resp.read(1 << 20)
                return int(getattr(resp, "status", 200) or 200), body
        except urllib.error.HTTPError as e:
            _ = e.read(1 << 16)
            return int(e.code), b""

    last = "preflight failed"
    for u in _join_openai_urls(base_url, "/models"):
        req = urllib.request.Request(
            u,
            headers={
                "Authorization": "Bearer " + api_key,
                "Accept": "application/json",
            },
            method="GET",
        )
        try:
            status, _ = do(req)
        except Exception as e:  # noqa: BLE001
            last = f"models: {e}"
            continue
        if 200 <= status < 300:
            return "models"
        last = f"models HTTP {status}"
        if status in {404, 405}:
            continue
        raise RuntimeError(f"LLM preflight failed: {last}")

    payload = json.dumps(
        {
            "model": model,
            "max_tokens": 1,
            "messages": [{"role": "user", "content": "ping"}],
        }
    ).encode("utf-8")
    for u in _join_openai_urls(base_url, "/chat/completions"):
        req = urllib.request.Request(
            u,
            data=payload,
            headers={
                "Authorization": "Bearer " + api_key,
                "Content-Type": "application/json",
            },
            method="POST",
        )
        try:
            status, _ = do(req)
        except Exception as e:  # noqa: BLE001
            last = f"chat: {e}"
            continue
        if 200 <= status < 300:
            return "chat"
        last = f"chat HTTP {status}"
    raise RuntimeError(f"LLM preflight failed: {last}")


def wait_if_paused(
    pause_file: str,
    *,
    poll: float = 1.0,
    max_seconds: float = 3600.0,
    sleeper: SleepFn | None = None,
    clock: Callable[[], float] | None = None,
) -> bool:
    """Block while translate.pause exists. Returns True if we waited."""
    path = Path(pause_file).expanduser() if pause_file else None
    if path is None or not path.is_file():
        return False
    sleep = sleeper or time.sleep
    now = clock or time.monotonic
    start = now()
    log(f"translate paused; waiting for removal of {path}")
    while path.is_file():
        if now() - start >= max_seconds:
            raise RuntimeError(f"translate pause file still present after {int(max_seconds)}s: {path}")
        sleep(poll)
    log("translate pause file removed; continuing")
    return True


def pause_seconds(seconds: float, sleeper: SleepFn | None = None) -> None:
    if seconds > 0:
        (sleeper or time.sleep)(seconds)


def _snippet(text: str, n: int = 800) -> str:
    s = (text or "").strip()
    return s[-n:] if len(s) > n else s


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
    quality: QualitySettings,
    ignore_cache: bool = False,
    invoke: InvokeFn | None = None,
    babeldoc_bin: str | None = None,
) -> list[str]:
    bin_path = babeldoc_bin or which_babeldoc()
    if not bin_path:
        raise RuntimeError(
            "babeldoc not on PATH; install with: uv tool install --python 3.12 BabelDOC"
        )
    work.mkdir(parents=True, exist_ok=True)
    cmd = build_babeldoc_cmd(
        bin_path,
        inp,
        work,
        model=model,
        base_url=base_url,
        qps=qps,
        lang_in=lang_in,
        lang_out=lang_out,
        quality=quality,
        ignore_cache=ignore_cache,
    )
    env = os.environ.copy()
    if api_key:
        # Prefer env so `ps` / process lists never leak the key.
        env["OPENAI_API_KEY"] = api_key

    def default_invoke(argv: list[str]) -> subprocess.CompletedProcess[str]:
        if argv_has_secret(argv):
            raise RuntimeError("refusing to exec babeldoc with API key on argv")
        return subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            env=env,
            check=False,
        )

    run = invoke or default_invoke
    proc = run(cmd)
    if proc.returncode != 0:
        out = proc.stdout or ""
        if "unrecognized" in out.lower() or "unrecognised" in out.lower() or "watermark-output-mode" in out:
            mentioned = flags_mentioned_in_output(out)
            if not mentioned and "watermark-output-mode" in out:
                mentioned = ["--watermark-output-mode"]
            if mentioned:
                log(f"babeldoc rejected flags {mentioned}; retrying without them")
                cmd = strip_optional_flags(cmd, mentioned)
                proc = run(cmd)
    if proc.returncode != 0:
        raise RuntimeError(f"babeldoc exited {proc.returncode}: {_snippet(proc.stdout or '')}")
    return cmd


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def add_quality_args(p: argparse.ArgumentParser) -> None:
    p.add_argument(
        "--disable-same-text-fallback",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default ON. Opt out with --no-disable-same-text-fallback.",
    )
    p.add_argument(
        "--translate-table-text",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default ON. Opt out with --no-translate-table-text.",
    )
    p.add_argument(
        "--enhance-compatibility",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default OFF. Do not enable unless a PDF needs it.",
    )
    p.add_argument(
        "--preflight",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default ON. Ping the LLM before BabelDOC.",
    )
    p.add_argument(
        "--completeness",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default ON. Fail majority-English / mixed residual mono ZH.",
    )
    p.add_argument(
        "--ignore-cache-retry",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Default ON. One --ignore-cache rerun after a completeness miss.",
    )
    p.add_argument("--min-text-length", type=int, default=None, help="Default 3; 0 omits the flag.")
    p.add_argument("--custom-system-prompt", default=None, help="Override the academic CS prompt.")
    p.add_argument("--glossary-files", default=None, help="Comma-separated glossary CSV paths.")
    p.add_argument("--pause-seconds", type=float, default=None, help="Delay after preflight / before retry.")


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="BabelDOC wrapper for RapidSearch papers")
    p.add_argument("--check", action="store_true", help="Print babeldoc availability and quality defaults")
    p.add_argument("--input", type=Path, help="Source English PDF")
    p.add_argument("--id", default="", help="Paper id (arxiv or filename stem)")
    p.add_argument("--out-root", type=Path, default=None, help="PAPERS_DIR root")
    p.add_argument("--base-url", default="", help="OpenAI-compatible base URL")
    p.add_argument("--model", default="gpt-4o-mini")
    p.add_argument("--qps", type=int, default=4)
    p.add_argument("--lang-in", default="en")
    p.add_argument("--lang-out", default="zh-CN")
    add_quality_args(p)
    args = p.parse_args(argv)

    root_for_cfg = args.out_root.expanduser() if args.out_root is not None else None
    quality = resolve_quality_settings(root_for_cfg, args)

    if args.check:
        emit(
            {
                "ok": True,
                "babeldoc": bool(which_babeldoc()),
                "quality": quality.public_dict(),
            }
        )
        return 0

    if args.input is None or args.out_root is None or not args.id:
        emit({"ok": False, "error": "--input, --id, and --out-root are required"})
        return 2

    inp = args.input.expanduser().resolve()
    root = args.out_root.expanduser().resolve()
    quality = resolve_quality_settings(root, args)
    pid = args.id.strip().replace("/", "_")
    if not inp.is_file():
        emit({"ok": False, "error": "input PDF not found"})
        return 2

    api_key = os.environ.get("OPENAI_API_KEY", "").strip()
    work = root / "translate-work" / pid
    used_cmd: list[str] = []
    completeness: CompletenessReport | None = None
    retried = False
    via = ""
    try:
        if quality.preflight:
            via = preflight_llm(args.base_url.strip(), api_key, args.model)
            log(f"LLM preflight ok via={via}")
        wait_if_paused(quality.pause_file)
        pause_seconds(quality.pause_seconds)

        run_kwargs = dict(
            model=args.model,
            base_url=args.base_url.strip(),
            qps=args.qps,
            lang_in=args.lang_in,
            lang_out=args.lang_out,
            api_key=api_key,
            quality=quality,
        )
        used_cmd = run_babeldoc(inp, work, ignore_cache=False, **run_kwargs)
        mono, dual = collect_outputs(work)
        if quality.completeness:
            completeness = check_mono_completeness(mono)
            if completeness.extracted and not completeness.ok and quality.ignore_cache_retry:
                log(f"completeness miss ({completeness.reason}); one --ignore-cache retry")
                wait_if_paused(quality.pause_file)
                pause_seconds(quality.pause_seconds if quality.pause_seconds > 0 else 2.0)
                used_cmd = run_babeldoc(inp, work, ignore_cache=True, **run_kwargs)
                retried = True
                mono, dual = collect_outputs(work)
                completeness = check_mono_completeness(mono)
            if completeness.extracted and not completeness.ok:
                emit(
                    {
                        "ok": False,
                        "error": f"translation completeness failed: {completeness.reason}",
                        "completeness": completeness.public_dict(),
                        "retried_ignore_cache": retried,
                        "quality": quality.public_dict(),
                    }
                )
                return 1

        zh_rel, dual_rel = install_outputs(root, pid, mono, dual)
        if not zh_rel and not dual_rel:
            emit({"ok": False, "error": "babeldoc produced no output PDFs"})
            return 1
        out: dict[str, Any] = {
            "ok": True,
            "zh": zh_rel,
            "dual": dual_rel,
            "quality": quality.public_dict(),
            "retried_ignore_cache": retried,
            "babeldoc_flags": [a for a in used_cmd if a.startswith("--")],
        }
        if completeness is not None:
            out["completeness"] = completeness.public_dict()
        if via:
            out["preflight"] = via
        emit(out)
        return 0
    except Exception as e:  # noqa: BLE001 — surface to Go as JSON
        emit({"ok": False, "error": str(e), "retried_ignore_cache": retried})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
