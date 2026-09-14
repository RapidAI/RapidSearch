#!/usr/bin/env python3
"""Build AgentMail-ready digest text: title + brief + PDF URL. Never file paths."""
from __future__ import annotations
import argparse, json
from datetime import datetime, timezone, timedelta
from pathlib import Path

TAG_CN = {
    "self-evolution": "agent自进化",
    "security": "agent安全",
    "both": "agent安全自进化",
    "llm-iot": "LLM based 物联网",
    "survey": "综述",
}

def brief(title, abstract, tags):
    t = (title or "").lower()
    a = (abstract or "")[:500].lower()
    tags = tags or []
    if "survey" in t or "综述" in t or "comprehensive survey" in a or "survey" in tags:
        return "综述：系统梳理该方向的方法与挑战。"
    if "jailbreak" in t or "jailbreak" in a:
        return "自进化/代理式越狱与攻击面分析。"
    if "guardrail" in t:
        return "自改进 agent 护栏与约束相关讨论。"
    if "audit" in t:
        return "面向 agent 应用的安全审计/分析。"
    if "security" in tags or "safety" in t or "security" in t:
        if any(x in t for x in ("evolv", "improv", "self-")):
            return "交叉：自改进与安全/治理。"
        return "LLM agent 安全、威胁或防护。"
    if "llm-iot" in tags or "iot" in t or "aiot" in t or "internet of things" in t or "internet of things" in a:
        return "LLM / agent 驱动的物联网（AIoT）系统。"
    if any(x in t for x in ("evolv", "improv", "mutab")):
        return "agent 自进化/自改进或持续适应。"
    abs0 = (abstract or "").strip().split(". ")[0]
    if abs0:
        return (abs0[:140] + "…") if len(abs0) > 140 else abs0 + ("." if not abs0.endswith(".") else "")
    return "与 agent 自进化或安全相关。"

def build(papers, header_note=""):
    tz = timezone(timedelta(hours=8))
    today = datetime.now(tz).strftime("%Y-%m-%d")
    lines = [f"Agent 论文速览 · {len(papers)} 篇 · {today}", ""]
    if header_note:
        lines += [header_note, ""]
    lines.append("每篇：标题、简要描述、页面与 PDF 链接（可直接点开阅读）。")
    lines.append("")
    for i, p in enumerate(papers, 1):
        tags = p.get("topic_tags") or p.get("tags") or []
        if isinstance(tags, str):
            tags = [tags]
        tag_s = "/".join(TAG_CN.get(t, t) for t in tags) or "相关"
        authors = p.get("authors") or []
        if isinstance(authors, str):
            authors = [authors]
        auth = ", ".join(authors[:3]) + (" et al." if len(authors) > 3 else "")
        arxiv = p.get("arxiv_id") or ""
        abs_url = p.get("source_url") or (f"https://arxiv.org/abs/{arxiv}" if arxiv else "")
        pdf_url = p.get("pdf_url") or (f"https://arxiv.org/pdf/{arxiv}.pdf" if arxiv else "")
        lines.append(f"{i}. [{tag_s}] {p.get('title','')}")
        if auth:
            lines.append(f"   作者: {auth} · {p.get('year','')}")
        lines.append(f"   简介: {brief(p.get('title',''), p.get('abstract',''), tags)}")
        if abs_url:
            lines.append(f"   页面: {abs_url}")
        if pdf_url:
            lines.append(f"   PDF:  {pdf_url}")
        lines.append("")
    lines.append("— 码卡龙本龙")
    return "\n".join(lines)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--manifest", default="/workspace/agent-papers/manifest.json")
    ap.add_argument("--out", default="/workspace/agent-papers/email_digest_body.txt")
    ap.add_argument("--only-new", action="store_true", help="Use sync_report new papers if present")
    ap.add_argument("--note", default="")
    args = ap.parse_args()
    papers = []
    if args.only_new:
        sr = Path("/workspace/agent-papers/sync_report.json")
        if sr.exists():
            data = json.loads(sr.read_text())
            papers = data.get("new_papers") or data.get("papers") or []
    if not papers:
        data = json.loads(Path(args.manifest).read_text())
        papers = data.get("papers") or []
    text = build(papers, header_note=args.note)
    Path(args.out).write_text(text, encoding="utf-8")
    print(text)

if __name__ == "__main__":
    main()
