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
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path


def log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def which_babeldoc() -> str | None:
    return shutil.which("babeldoc")


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
    env = os.environ.copy()
    if api_key:
        env["OPENAI_API_KEY"] = api_key
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
    if proc.returncode != 0 and "watermark-output-mode" in (proc.stdout or ""):
        log("babeldoc rejected --watermark-output-mode; retrying without it")
        cmd = [a for a in cmd if not a.startswith("--watermark-output-mode")]
        proc = invoke(cmd)
    if proc.returncode != 0:
        snippet = (proc.stdout or "").strip()
        if len(snippet) > 800:
            snippet = snippet[-800:]
        raise RuntimeError(f"babeldoc exited {proc.returncode}: {snippet}")


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main() -> int:
    p = argparse.ArgumentParser(description="BabelDOC wrapper for RapidSearch papers")
    p.add_argument("--check", action="store_true", help="Print babeldoc availability and exit")
    p.add_argument("--input", type=Path, help="Source English PDF")
    p.add_argument("--id", default="", help="Paper id (arxiv or filename stem)")
    p.add_argument("--out-root", type=Path, default=None, help="PAPERS_DIR root")
    p.add_argument("--base-url", default="", help="OpenAI-compatible base URL")
    p.add_argument("--model", default="gpt-4o-mini")
    p.add_argument("--qps", type=int, default=4)
    p.add_argument("--lang-in", default="en")
    p.add_argument("--lang-out", default="zh-CN")
    args = p.parse_args()

    if args.check:
        emit({"ok": True, "babeldoc": bool(which_babeldoc())})
        return 0

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
    try:
        run_babeldoc(
            inp,
            work,
            model=args.model,
            base_url=args.base_url.strip(),
            qps=args.qps,
            lang_in=args.lang_in,
            lang_out=args.lang_out,
            api_key=api_key,
        )
        mono, dual = collect_outputs(work)
        zh_rel, dual_rel = install_outputs(root, pid, mono, dual)
        if not zh_rel and not dual_rel:
            emit({"ok": False, "error": "babeldoc produced no output PDFs"})
            return 1
        emit({"ok": True, "zh": zh_rel, "dual": dual_rel})
        return 0
    except Exception as e:  # noqa: BLE001 — surface to Go as JSON
        emit({"ok": False, "error": str(e)})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
