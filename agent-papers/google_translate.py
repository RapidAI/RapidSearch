"""Google Translate backends for RapidSearch paper PDF jobs.

BabelDOC 0.6.x only exposes ``--openai``. When the admin selects the Google
engine we start a local OpenAI-compatible shim and point BabelDOC at it.
The shim calls:

1. Google Cloud Translation API v2 when ``GOOGLE_TRANSLATE_API_KEY`` is set
2. Otherwise the public Google Translate web endpoints (same family as
   pdf2zh-next's Google translator: ``translate.googleapis.com`` / ``translate.google.com/m``)

This module is imported by ``translate_worker.py`` and is safe to unit-test
without BabelDOC.
"""
from __future__ import annotations

import html
import json
import re
import socket
import threading
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable

GOOGLE_CLOUD_URL = "https://translation.googleapis.com/language/translate/v2"
GOOGLE_GTX_URL = "https://translate.googleapis.com/translate_a/single"
GOOGLE_MOBILE_URL = "https://translate.google.com/m"
MAX_RUNES = 4500

_INPUT_MARKERS = ("Input:\n\n", "Input:\r\n\r\n")
_PLACEHOLDER_RE = re.compile(r"\{\{[^}]+\}\}|\{v\d+\}|</?b\d+>|</?style[^>]*>")
_RESULT_RE = re.compile(r'(?s)class="(?:t0|result-container)">(.*?)<')
_TERM_HINTS = ("glossary", "term extraction", "extract terms", "json object")


def normalize_lang(raw: str, fallback: str) -> str:
    s = (raw or "").strip()
    if not s:
        return fallback
    low = s.lower().replace("_", "-")
    if low in {"zh", "zh-cn", "chinese", "simplified chinese"}:
        return "zh-CN"
    if low in {"zh-tw"}:
        return "zh-TW"
    return s


def extract_babeldoc_input(user: str) -> str:
    text = (user or "").strip()
    for marker in _INPUT_MARKERS:
        i = text.rfind(marker)
        if i >= 0:
            return text[i + len(marker) :].strip()
    return text


def last_user_content(messages: list) -> str:
    if not messages:
        return ""
    for item in reversed(messages):
        if not isinstance(item, dict):
            continue
        if str(item.get("role") or "").strip().lower() == "user":
            return str(item.get("content") or "")
    last = messages[-1]
    if isinstance(last, dict):
        return str(last.get("content") or "")
    return str(last)


def looks_like_term_extraction(text: str) -> bool:
    low = (text or "").lower()
    return any(h in low for h in _TERM_HINTS)


def protect_placeholders(text: str) -> tuple[str, list[str]]:
    tokens: list[str] = []

    def _sub(m: re.Match[str]) -> str:
        tokens.append(m.group(0))
        return f"⟨GPH{len(tokens) - 1}⟩"

    return _PLACEHOLDER_RE.sub(_sub, text or ""), tokens


def restore_placeholders(text: str, tokens: list[str]) -> str:
    out = text or ""
    for i, tok in enumerate(tokens):
        out = out.replace(f"⟨GPH{i}⟩", tok)
        out = out.replace(f"<GPH{i}>", tok)
    return out


def split_chunks(text: str, max_runes: int = MAX_RUNES) -> list[str]:
    if max_runes <= 0 or len(text) <= max_runes:
        return [text]
    return [text[i : i + max_runes] for i in range(0, len(text), max_runes)]


def parse_gtx(body: bytes | str) -> str:
    parsed = json.loads(body)
    if not parsed:
        raise RuntimeError("Google Translate: empty response")
    sentences = parsed[0]
    if not isinstance(sentences, list):
        raise RuntimeError("Google Translate: unexpected shape")
    parts: list[str] = []
    for row in sentences:
        if isinstance(row, list) and row and isinstance(row[0], str):
            parts.append(row[0])
    out = "".join(parts).strip()
    if not out:
        raise RuntimeError("Google Translate: empty translation")
    return out


def _http_json(
    url: str,
    *,
    method: str = "GET",
    data: bytes | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 20.0,
) -> tuple[int, bytes]:
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return int(getattr(resp, "status", 200) or 200), resp.read()
    except urllib.error.HTTPError as e:
        raw = b""
        try:
            raw = e.read()
        except Exception:  # noqa: BLE001
            raw = b""
        return int(getattr(e, "code", 0) or 0), raw


def translate_cloud(
    text: str,
    *,
    api_key: str,
    lang_in: str,
    lang_out: str,
    timeout: float = 20.0,
) -> str:
    payload = json.dumps(
        {
            "q": [text],
            "source": lang_in,
            "target": lang_out,
            "format": "text",
        }
    ).encode("utf-8")
    url = GOOGLE_CLOUD_URL + "?key=" + urllib.parse.quote(api_key)
    status, raw = _http_json(
        url,
        method="POST",
        data=payload,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
        timeout=timeout,
    )
    if status < 200 or status >= 300:
        raise RuntimeError(f"Google Cloud Translation HTTP {status}")
    obj = json.loads(raw.decode("utf-8", errors="replace") or "{}")
    rows = (((obj.get("data") or {}).get("translations")) or [])
    if not rows:
        raise RuntimeError("Google Cloud Translation: empty response")
    return html.unescape(str(rows[0].get("translatedText") or ""))


def translate_gtx(text: str, *, lang_in: str, lang_out: str, timeout: float = 20.0) -> str:
    q = urllib.parse.urlencode(
        {"client": "gtx", "sl": lang_in, "tl": lang_out, "dt": "t", "q": text}
    )
    status, raw = _http_json(
        GOOGLE_GTX_URL + "?" + q,
        headers={
            "User-Agent": "Mozilla/5.0 RapidSearch-papers",
            "Accept": "application/json",
        },
        timeout=timeout,
    )
    if status < 200 or status >= 300:
        raise RuntimeError(f"Google Translate HTTP {status}")
    return parse_gtx(raw)


def translate_mobile(text: str, *, lang_in: str, lang_out: str, timeout: float = 20.0) -> str:
    q = urllib.parse.urlencode({"sl": lang_in, "tl": lang_out, "q": text})
    status, raw = _http_json(
        GOOGLE_MOBILE_URL + "?" + q,
        headers={"User-Agent": "Mozilla/4.0 (compatible; MSIE 6.0; Windows NT 5.1)"},
        timeout=timeout,
    )
    if status < 200 or status >= 300:
        raise RuntimeError(f"Google Translate HTTP {status}")
    m = _RESULT_RE.search(raw.decode("utf-8", errors="replace"))
    if not m:
        raise RuntimeError("Google Translate: no result-container")
    return html.unescape(m.group(1)).strip()


def translate_text(
    text: str,
    *,
    lang_in: str = "en",
    lang_out: str = "zh-CN",
    api_key: str = "",
    timeout: float = 20.0,
) -> tuple[str, str]:
    """Return (translated_text, via). via is google_cloud or google_web."""
    src = (text or "").strip()
    if not src:
        return "", "google_web" if not api_key else "google_cloud"
    lang_in = normalize_lang(lang_in, "en")
    lang_out = normalize_lang(lang_out, "zh-CN")
    protected, tokens = protect_placeholders(src)
    parts: list[str] = []
    if api_key:
        via = "google_cloud"
        for chunk in split_chunks(protected):
            parts.append(
                translate_cloud(
                    chunk, api_key=api_key, lang_in=lang_in, lang_out=lang_out, timeout=timeout
                )
            )
    else:
        via = "google_web"
        for chunk in split_chunks(protected):
            try:
                parts.append(translate_gtx(chunk, lang_in=lang_in, lang_out=lang_out, timeout=timeout))
            except Exception:  # noqa: BLE001 — fall back to mobile HTML
                parts.append(
                    translate_mobile(chunk, lang_in=lang_in, lang_out=lang_out, timeout=timeout)
                )
    return restore_placeholders("".join(parts), tokens), via


def google_preflight(
    *,
    api_key: str = "",
    lang_in: str = "en",
    lang_out: str = "zh-CN",
    timeout: float = 20.0,
) -> tuple[bool, str]:
    try:
        out, via = translate_text(
            "hello", lang_in=lang_in, lang_out=lang_out, api_key=api_key, timeout=timeout
        )
    except Exception as e:  # noqa: BLE001
        return False, f"Google Translate unavailable ({e}); not starting BabelDOC"
    if not (out or "").strip():
        return False, "Google Translate unavailable (empty); not starting BabelDOC"
    return True, via


class _ShimHandler(BaseHTTPRequestHandler):
    translator: Callable[..., tuple[str, str]]
    lang_in: str = "en"
    lang_out: str = "zh-CN"
    api_key: str = ""

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003
        return

    def _write_json(self, status: int, obj: dict) -> None:
        raw = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path.endswith("/models"):
            self._write_json(
                200,
                {
                    "object": "list",
                    "data": [{"id": "google-translate", "object": "model"}],
                },
            )
            return
        self._write_json(404, {"error": "not found"})

    def do_HEAD(self) -> None:  # noqa: N802
        self.do_GET()

    def do_POST(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        if not path.endswith("/chat/completions"):
            self._write_json(404, {"error": "not found"})
            return
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n > 0 else b"{}"
        try:
            body = json.loads(raw.decode("utf-8", errors="replace") or "{}")
        except json.JSONDecodeError:
            self._write_json(400, {"error": "invalid json"})
            return
        messages = body.get("messages") if isinstance(body, dict) else []
        if not isinstance(messages, list):
            messages = []
        user = last_user_content(messages)
        fmt = body.get("response_format") if isinstance(body, dict) else None
        want_json = isinstance(fmt, dict) and fmt.get("type") == "json_object"
        if want_json or looks_like_term_extraction(user):
            content = '{"glossary":[],"terms":[]}'
        else:
            src = extract_babeldoc_input(user)
            try:
                content, _via = self.translator(
                    src,
                    lang_in=self.lang_in,
                    lang_out=self.lang_out,
                    api_key=self.api_key,
                )
            except Exception as e:  # noqa: BLE001
                self._write_json(502, {"error": str(e)})
                return
        self._write_json(
            200,
            {
                "id": "chatcmpl-google",
                "object": "chat.completion",
                "model": "google-translate",
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "stop",
                        "message": {"role": "assistant", "content": content},
                    }
                ],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            },
        )


class GoogleOpenAIShim:
    def __init__(
        self,
        *,
        api_key: str = "",
        lang_in: str = "en",
        lang_out: str = "zh-CN",
        translator: Callable[..., tuple[str, str]] | None = None,
    ) -> None:
        self.api_key = (api_key or "").strip()
        self.lang_in = normalize_lang(lang_in, "en")
        self.lang_out = normalize_lang(lang_out, "zh-CN")
        self.translator = translator or translate_text
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None

    def start(self) -> str:
        class Handler(_ShimHandler):
            pass

        Handler.translator = staticmethod(self.translator)
        Handler.lang_in = self.lang_in
        Handler.lang_out = self.lang_out
        Handler.api_key = self.api_key
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        host, port = sock.getsockname()
        sock.close()
        self._httpd = ThreadingHTTPServer((host, port), Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()
        return f"http://{host}:{port}/v1"

    def stop(self) -> None:
        if self._httpd is not None:
            self._httpd.shutdown()
            self._httpd.server_close()
            self._httpd = None
        if self._thread is not None:
            self._thread.join(timeout=2)
            self._thread = None
