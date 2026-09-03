# RapidSearch

A search service for Agent.

Local HTTP JSON API that searches the web with HTTP scrapes, optional keyed APIs (Serper / Brave), and Chrome fallbacks (go-rod + stealth). Third-party search APIs are **off** until you paste a key in the settings page.

本地 HTTP JSON 接口：默认仍是 HTTP 抓取 + Chrome 兜底，不调用第三方搜索 API。若在设置页粘贴 Serper / Brave key，`engine=auto`（以及显式 `engine=serper` / `engine=brave`）才会走这些 API。默认 `engine=auto`：先穷尽 HTTP 引擎（中文：百度→搜狗→360→Bing→DuckDuckGo HTML；英文：DuckDuckGo HTML→Bing→搜狗→360→百度），有 key 时 Serper→Brave 会排在最前；全部失败且剩余时间足够才走 Chrome。Auto 默认不打 Google Chrome。

## Run / 启动

```bash
cd /workspace/search-service
./run.sh
```

Listens on `127.0.0.1:18765`. Each Chrome instance keeps cookies in `./chrome-profile/i0`, `i1`, …

监听 `127.0.0.1:18765`。每个 Chrome 实例的 Cookie 在 `./chrome-profile/i0`、`i1` 等目录。

Stop: `Ctrl+C`, or `pkill -f '/workspace/search-service/search-service'`.

停止：前台 `Ctrl+C`，或 `pkill -f '/workspace/search-service/search-service'`。

Environment / 环境变量:

- `DISPLAY` (default `:1`)
- `CHROME_BIN` (optional chrome path)
- `SEARCH_LISTEN` (optional, default `127.0.0.1:18765`)
- `SEARCH_BROWSER_INSTANCES` (optional, default `3`, clamp 1–4): independent headed Chrome processes. Each runs **one SERP at a time**. This is also the Chrome **admission** cap (in-flight Chrome jobs). Profiles: `chrome-profile/i0`, `i1`, `i2`. Lazy-launched (instance 0 on first search / Ensure; others when all busy). If one Chrome dies it is relaunched alone.
- `SEARCH_BROWSER_SLOTS` (optional): legacy alias/cap. When set, total in-flight SERPs = `min(instances, slots)`. Per-process is always 1.
- `SEARCH_QUEUE_MAX` (optional, default `0` = unlimited waiters): max Chrome **or** HTTP jobs waiting for a slot. Overflow returns `{"ok":false,"code":"busy"}` (HTTP 503) instead of sitting until 504.
- `SEARCH_CHROME_MIN_REMAIN` (optional, default `15s`): do not start a Chrome SERP if remaining handler time is below this (Go duration or integer seconds). Those requests fail with `code=timeout` so a doomed 40s try is not started. Chrome is only considered after the HTTP chain is exhausted.
- `SEARCH_HTTP_MAX` (optional, default `10`, clamp 1–32; `0` = unlimited): max in-flight HTTP SERP fetches. Extra clients wait on the ~170s handler deadline instead of stampeding baidu/bing. Overflow that cannot start in time returns `code=timeout` / `busy` — never HTTP 200 with empty results. HTTP waiters do **not** take a Chrome slot.
- `SEARCH_HTTP_TRY_TIMEOUT` (optional, default `5s`, clamp 1–15s): per-engine HTTP GET budget so failover to the next HTTP engine is fast.
- `CACHE_DIR` (optional, default `./cache`)
- `CACHE_TTL` (optional Go duration, default `1h`)
- `SEARCH_CONFIG_PATH` (optional, default `./search-config.json`): API keys + engine priority. Created with mode `0600`, gitignored. Never commit this file.
- `SEARCH_TOKEN` (optional on the search process): same secret as the public proxy. Operator Bearer / `?token=` can still open `/settings/config` for non-browser use. Browser settings login is Hub **global admin** username + password only. If unset, `./proxy.token` is read when present.
- `HUB_AUTH_BASES` (optional): comma-separated Hub origins used to validate Hub tokens. Default `https://hub.mypapers.top,https://hub.maclaw.top`. `/search` checks viewer tokens with `GET {HUB}/api/llm/v1/models`. Settings cookie/admin Bearer is checked with `GET {HUB}/api/admin/users` (not models).

## API

`GET /health` → `{"ok":true}`

### Settings / 设置页

HTML + JSON on the search process. Open `https://hub.maclaw.top/searchproxy/settings` in a browser and sign in — no `Authorization` header required after login.

- `GET /settings` — if signed in (Hub **global admin** cookie, or operator/admin Bearer / `?token=`): HTML form (Serper / Brave keys, enable + drag or ↑↓ priority). If not signed in: **HTML login page** (HTTP 200), not JSON 401. Login and settings UI are **Chinese / English**: default from `navigator.language` or `Accept-Language` (`zh*` → Chinese, else English); on-page ZH/EN toggle stored in `localStorage` key `rs_settings_lang`.
- `POST /settings/login` (also `POST /settings`) — Hub **global admin** username + password only (`POST {HUB}/api/admin/login` with `tenant` omitted or `"__global__"`). Parses `access_token` and rejects the login unless the returned `admin` is a **global** admin (not tenant-scoped). Then checks `GET {HUB}/api/admin/users` (2xx). Sets HttpOnly cookie `rs_settings` (`Path=/`, `SameSite=Lax`, `Secure` when HTTPS). No token paste field. Login never accepts or displays the operator `SEARCH_TOKEN` / `proxy.token`.
- `POST /settings/logout` — clears the cookie.
- `GET /settings/config` — masked JSON (`configured` yes/no, optional `last4`). Never returns raw keys. Unauthenticated → **JSON 401** (API stays machine-readable).
- `PUT /settings/config` — update keys and/or priority. An empty key string **does not wipe** a stored key; send `"clear_serper": true` / `"clear_brave": true` to delete.

Hub global admin tokens are validated by Hub admin middleware (`Authenticate`), **not** by `GET /api/llm/v1/models`. A models-only viewer token is not enough for `/settings`. Tenant-scoped admins are rejected at login. There is no captcha on Hub admin login.

Listen locally at `http://127.0.0.1:18765/settings`. search-proxy forwards `/settings`, `/settings/login`, Cookie, and Set-Cookie the same way as `/settings/config`. Hub can open `https://hub.maclaw.top/searchproxy/settings` if nginx already prefixes `/searchproxy/` to the proxy. Do not bind the search process to `0.0.0.0`.

本地打开 `http://127.0.0.1:18765/settings`。公网路径在反代已转发 `/searchproxy/` 时为 `https://hub.maclaw.top/searchproxy/settings`。未登录的浏览器看到 **Hub 全局管理员账号密码** 登录页（无 token 粘贴）；`/settings/config` 仍是 JSON 401，不会泄露 key。

Persisted to `SEARCH_CONFIG_PATH` (default `./search-config.json`, mode `0600`, gitignored). Raw keys are never logged.

**Default auto priority** (no saved file):

| keys | China | Global |
|---|---|---|
| none | `baidu` → `sogou` → `360` → `bing` → `duckduckgo_html` → `duckduckgo` | `duckduckgo_html` → `bing` → `sogou` → `360` → `baidu` → `duckduckgo` |
| Serper and/or Brave set | `serper` then `brave` (only if that key exists) **prepended**, then the same HTTP-first chain | same prepend |

Google is **omitted** on auto unless you enable it in a saved priority list. If enabled, the captcha breaker still skips / fail-fasts when open. After you save a custom order, auto uses that enabled list (disabled engines and keyed engines without a key are skipped).

**Auth:** `/settings` and `/settings/config` accept `Authorization: Bearer …`, `?token=`, or the `rs_settings` HttpOnly cookie. The cookie must be a Hub **global admin** token (`GET {HUB}/api/admin/users` 2xx). Operator `SEARCH_TOKEN` / `./proxy.token` Bearer still works for non-browser `/settings/config`. Viewer/models tokens are not enough for settings. Local `/search` on `127.0.0.1` stays open; the public proxy still authenticates `/search` with Bearer/`?token=` only (Hub viewer or `SEARCH_TOKEN`; settings cookie is ignored).

`GET /search?q=<query>&engine=auto|google|bing|baidu|duckduckgo|duckduckgo_html|sogou|360|serper|brave&n=10&content=1&fallback=1`

Optional routing hints: `region=cn`, `locale=zh`, `hl=zh-CN`.

`POST /search` JSON `{"query":"...","engine":"auto","limit":10,"content":true,"region":"cn","locale":"zh","fallback":true}`

Defaults: `engine=auto` (omitted engine means auto), `limit=10` (clamped 1–20), `content=true`. Auto always failovers. An explicit engine does **not** failover unless `fallback=1` / `"fallback": true`.

`content=0` / `false` skips landing-page fetches (still cleans, filters, and scores). Omitted POST `content` defaults to true.

`content=0` / `false` 会跳过抓取落地页正文（仍会清洗、相关性过滤和打分）。POST 省略 `content` 时默认为 true。

### curl

```bash
curl -sS http://127.0.0.1:18765/health

curl -sS 'http://127.0.0.1:18765/search?q=golang+http+server&n=5'

curl -sS 'http://127.0.0.1:18765/search?q=北京天气&n=3'

curl -sS 'http://127.0.0.1:18765/search?q=Go语言&engine=baidu&n=3'

# skip landing-page fetch / 不抓取正文
curl -sS 'http://127.0.0.1:18765/search?q=golang+http+server&engine=bing&n=5&content=0'

curl -sS -X POST http://127.0.0.1:18765/search \
  -H 'Content-Type: application/json' \
  -d '{"query":"golang http server","engine":"google","limit":5}'
```

Success (HTTP 200):

```json
{
  "ok": true,
  "query": "golang http server",
  "engine": "bing",
  "requested_engine": "auto",
  "tried": ["bing"],
  "skipped": ["google"],
  "results": [
    {
      "rank": 1,
      "title": "...",
      "url": "...",
      "snippet": "...",
      "content": "topic-relevant text from the landing page",
      "relevance": 0.83
    }
  ],
  "count": 3,
  "took_ms": 1234
}
```

`ok` is always `true` on HTTP 200. `engine` is the engine that actually produced results (never `"auto"`). `requested_engine` is what the caller asked for. `tried` lists every engine attempted. `skipped` is present when Google was dropped from an auto chain (auto never spends the handler budget on Google Chrome).

**Non-200 is a failure.** Do not treat HTTP 4xx/5xx as “no hits”. The discriminator is `code`, not an empty `results` array. Engine failure (captcha, timeout, parse, offline) is **never** HTTP 200 with `results: []`. A genuine zero-hit success would be HTTP 200 + `ok: true` + empty `results`; this service currently returns an error instead of 200-empty when the engine fails to parse organic hits.

Errors: HTTP 4xx/5xx with:

```json
{
  "ok": false,
  "error": "search blocked by captcha",
  "code": "captcha",
  "engine": "google",
  "tried": ["google"]
}
```

`code` is one of `captcha` | `timeout` | `busy` | `parse` | `offline` | `unauthorized` | `engine` | `bad_request`. `error` is short English. `engine` is the last engine tried when known. Error responses are not cached. `busy` (HTTP 503) means the Chrome or HTTP admission queue was full (`SEARCH_QUEUE_MAX`). `timeout` (HTTP 504) includes “could not start HTTP or Chrome before the handler deadline”.


## Result cache / 结果缓存

Successful search JSON (after preprocess) is stored under `./cache` on disk. Errors, captcha, timeouts, and empty results are **not** cached.

成功的搜索 JSON（预处理后）写入 `./cache`。错误、验证码、超时、空结果不缓存。

- Key = SHA-256 of `v4|` + normalized query + engine + limit + content flag + region/locale/hl + fallback + config signature (which keyed engines have keys / enabled order). `engine=serper` and `engine=bing` never share a cache entry. `content=0` and `content=1` are different keys.
- 缓存键 = `v4|` + 规范化查询 + 引擎 + limit + content + region/locale/hl + fallback + 配置指纹（哪些付费引擎有 key / 启用顺序）的 SHA-256。`engine=serper` 与 `engine=bing` 不会撞键。
- Eviction is LFU then LRU. TTL default **1 hour** (`CACHE_TTL`, e.g. `1h`).
- 淘汰：先 LFU 再 LRU。TTL 默认 1 小时（`CACHE_TTL`）。
- **Disk budget**: `syscall.Statfs` on the cache dir. Budget = clamp(5% of filesystem size, 64MB min, 2GB max) **and** never more than 25% of currently free space. Recomputed on start, every 5 minutes, and before write.
- **磁盘预算**：对缓存目录 `Statfs`。预算 = clamp(文件系统 5%，64MB～2GB)，且不超过当前空闲空间的 25%。启动时、每 5 分钟、写入前重算。
- Landing-page `content` is omitted from the **first** disk write when that payload is >4KiB, so one-off queries do not eat disk. A later live store (TTL refresh / `nocache`) for a key with hits ≥ 2 writes the full body.
- 首次写入若落地页 `content` 合计超过 4KiB 会省略正文，避免一次性查询占盘。同一键命中 ≥ 2 后再活抓会写入完整正文。
- Cache hit adds `"cached": true` and `cache_age_ms`. `took_ms` is the cache-serve time.
- `GET /search?...&nocache=1` or `Cache-Control: no-cache` skips the read (still may write).
- `GET /cache/stats` → `{bytes, entries, budget_bytes, hits, misses, fs_size, fs_free}` (no query text).

```bash
curl -sS 'http://127.0.0.1:18765/cache/stats'
```

## Download / 下载

`GET /download?url=<https-url>` and `POST /download` JSON `{"url":"https://..."}`.

Only `http`/`https`. `file://`, `javascript:`, `data:` are rejected.

仅允许 http/https。拒绝 `file://`、`javascript:`、`data:`。

The handler streams the upstream bytes with Content-Type and a safe Content-Disposition filename. It retries (3× backoff) on 5xx / timeout / connection reset, follows up to 10 redirects, sends a desktop Chrome UA, and passes through `Range`. If the URL’s host was recently seen in a search, Chrome cookies are reused when practical. net/http runs first; 403 / challenge / empty falls back to Chrome (`WithPage` on any idle instance, does not take a SERP slot, ~3 min). Size cap is min(512MB, 25% of free disk); larger → 413. Successful bodies under 2MB may be stored as blobs under the same cache budget (not the search JSON store).

先用 net/http 拉取（重试、重定向、Range、桌面 UA）；若该站刚在搜索结果里出现过，会尽量复用 Chrome cookie。403/挑战/空响应再走空闲 Chrome 实例的页面 fetch（不占 SERP 槽）。单文件不超过 512MB 且不超过空闲盘 25%，否则 413。小于 2MB 的成功下载可进独立 blob 目录，计入同一磁盘预算。

```bash
curl -sS -o /tmp/t.bin 'http://127.0.0.1:18765/download?url=https://example.com/'
curl -sS -X POST http://127.0.0.1:18765/download -H 'Content-Type: application/json' -d '{"url":"https://example.com/"}'
```

Public proxy (Bearer token) also exposes `/download`; the relay streams the file in tunnel chunks so it is not loaded as one JSON frame.

公网反代同样提供 `/download`；隧道按块传输，避免整文件塞进一帧。

## Engines / 引擎

| engine | homepage / SERP | transport | aliases |
|---|---|---|---|
| `auto` (default when omitted) | (router) | | |
| `google` | https://www.google.com/ | Chrome | |
| `bing` | https://www.bing.com/search?q= | HTTP then Chrome | |
| `baidu` | https://www.baidu.com/s?wd= | HTTP then Chrome | `bd` |
| `sogou` | https://www.sogou.com/web?query= | HTTP then Chrome | |
| `360` | https://www.so.com/s?q= | HTTP then Chrome | `so360` |
| `duckduckgo_html` | https://html.duckduckgo.com/html/ | HTTP only (no Chrome slot) | `ddg_html` |
| `duckduckgo` | https://duckduckgo.com/ | Chrome (HTML is tried first) | `ddg`, `duck` |
| `serper` | https://google.serper.dev/search | Keyed HTTP API (no Chrome slot); skipped on auto without a key | |
| `brave` | https://api.search.brave.com/res/v1/web/search | Keyed HTTP API (no Chrome slot); skipped on auto without a key | |

### Auto routing / 自动调度

**China path** if any of:

- the query contains Han (CJK) characters / 查询含汉字
- `region=cn` / `locale=zh` / `hl=zh-CN` (GET) or POST `region` / `locale`
- English/pinyin CN-intent tokens (small list): `china`, `chinese`, `beijing`, `shanghai`, `wechat`, `weixin`, `zhihu`, `bilibili`, `xiaohongshu`, `bytedance`

**Global path** otherwise.

Failover chains (next engine on captcha, timeout, parse error, or empty results; never retry the same engine):

| path | HTTP first (no Chrome slot) | Chrome only if HTTP exhausted and remaining time exceeds `SEARCH_CHROME_MIN_REMAIN` |
|---|---|---|
| China | (`serper` → `brave` if keys exist) `baidu` → `sogou` → `360` → `bing` → `duckduckgo_html` | `duckduckgo` |
| Global | (`serper` → `brave` if keys exist) `duckduckgo_html` → `bing` → `sogou` → `360` → `baidu` | `duckduckgo` |
| Saved settings priority | enabled engines in that order (keyed APIs without a key skipped) | Chrome-only leftovers (`duckduckgo`, and `google` only if enabled) |

Google is **not** on either built-in auto chain. Enable it in Settings if you want it on auto (breaker still applies). Keyed APIs share the HTTP admission cap (`SEARCH_HTTP_MAX`); they never take a Chrome slot. Explicit `engine=serper` / `engine=brave` without a key returns `ok:false` with `code=unauthorized` (HTTP 401), never HTTP 200 empty. A datacenter IP will not spend the ~170s handler budget on Google Chrome during auto unless you enabled google. `duckduckgo_html` is a datacenter-friendly GET of `html.duckduckgo.com` (fallback `duckduckgo.com/html/`). Dual engines (`baidu` / `sogou` / `360` / `bing`) are attempted as HTTP on auto; their Chrome fallback is **not** started until every HTTP engine has failed, and only when that engine was explicitly requested. If bing HTTP fails, auto tries `duckduckgo_html` (and sogou/360/baidu) **before** any Chrome. Explicit `engine=duckduckgo` means HTML then Chrome once. Explicit `engine=sogou` / `engine=360` / `engine=baidu` / `engine=bing` / `engine=duckduckgo_html` / `engine=serper` / `engine=brave` try HTTP first. `engine=google` is still Chrome-only.

**Google circuit breaker** (process-wide, in addition to per-instance quarantine): built-in auto omits Google. If you enable google in Settings, an open breaker still **skips** it on auto (or **fails fast** for explicit `engine=google` with no fallback). After a Google captcha or “no Google Chrome instance”, explicit `engine=google` with `fallback=1` **skips Google for ~15 minutes**. A half-open probe allows one Google attempt after 15 minutes; success closes the breaker.

**HTTP then Chrome** (auto/fallback): HTTP engines run sequentially with a short per-try timeout (`SEARCH_HTTP_TRY_TIMEOUT`, default 5s) and a separate admission cap (`SEARCH_HTTP_MAX`, default 10). One request holds one HTTP slot across its HTTP chain so failover does not re-queue. Hedge (3s, max 2 in-flight) applies only to the Chrome phase after HTTP is exhausted — it cannot steal a Chrome slot while HTTP engines remain. Chrome waits on `SEARCH_BROWSER_INSTANCES` (clamp 1–4). First HTTP success wins. `ErrNoGoogleInstance` failovers immediately (does not consume the 40s per-try timeout).

Explicit `engine=google|bing|baidu|sogou|360|duckduckgo_html|serper|brave` is predictable: no failover unless `fallback=1`. `engine=duckduckgo` still tries HTML then Chrome once. Auto defaults to failover on (`fallback=0` disables it).

Per-try Chrome timeout ~40s; whole request ~3 min. Preprocess (relevance + optional content extract) runs on the **winning** result set, including Baidu hits. `content=0` still skips fetch.

Chrome SERP 工作最多 `SEARCH_BROWSER_INSTANCES` 路并发（默认 3 个独立 Chrome 进程，每进程 1 路 SERP）。额外 Chrome 请求在准入队列里等槽。HTTP SERP 另有 `SEARCH_HTTP_MAX`（默认 10）路并发，不占 Chrome 槽；100 路 stampede 在 HTTP 队列上等，而不是同时打爆百度/Bing 后再卡在 3 个 Chrome 上。Auto 先穷尽 HTTP 再考虑 Chrome。Google 全池串行并保持 8–15s 间隔；auto 不打 Google。缓存命中、预处理和正文抽取不受此限制。

## Preprocessing / 结果预处理

After organic SERP parse, Chrome is released, then results are:

有机结果解析完成后释放 Chrome，然后：

1. **Clean / 清洗**: unwrap tracker URLs, trim, drop empty title/url, `javascript:`, and engine-internal links.
   解开跟踪链接，去掉空标题/URL、`javascript:` 和搜索引擎站内链接。
2. **Ads / junk filter / 广告过滤**: drop sponsored/affiliate/PLA hits (ad hosts and `/aclk`/`pagead` paths, badge labels like `广告`/`Sponsored`/`Ad ·` at the start of title or snippet). Organic pages that merely talk about advertising (or domains like adobe.com) are kept. This step is not optional.
   丢掉赞助/联盟/购物广告（广告域名、`/aclk` 等，以及标题/摘要开头的 `广告`/`Sponsored`/`Ad ·` 角标）。讨论广告的正常文章和 adobe.com 等域名保留。此步不可跳过。
3. **Relevance filter / 相关性过滤**: tokenize English (non-alnum split) and CJK (runs + 2-grams). Score title + snippet + URL path vs the query. Drop off-topic / empty-snippet junk unless the title is a strong match; a hit that only overlaps a generic leftover token (e.g. only `http`) is dropped when title+snippet coverage is tiny. Re-rank by score (`relevance` is 0..1) and reassign `rank` 1..n. If every remaining hit would be dropped, the **ads-stripped** cleaned list is returned instead of an error.
   英文按非字母数字切词，中日韩按连续字串（可选二元组）切词。用标题+摘要+URL 路径对查询打分。丢掉跑题或空摘要垃圾（标题强匹配除外）。仅命中泛化残留词（如只有 `http`）且覆盖极低的结果也会丢掉。按分数重排，`relevance` 为 0..1。若相关性过滤后一条不剩，则回退到**已去广告**的清洗列表，不报错。
4. **Extract / 抽取正文** (default): `net/http` (not Chrome), desktop User-Agent, 8s timeout, max 3 concurrent, first `min(limit, 8)` hits. Strip script/style/nav/footer; keep sentences overlapping query tokens (+ one neighbor), cap ~1200 runes. Failures, PDFs, binary, 4xx/5xx → omit `content`. `took_ms` includes this step.
   默认用 `net/http` 抓取落地页（不用 Chrome），桌面 UA，8 秒超时，最多 3 并发，只处理前 `min(limit, 8)` 条。去掉 script/style/nav/footer，保留与查询词重叠的句子（含前后各一句），约 1200 字。失败/PDF/二进制/4xx/5xx 则省略 `content`。`took_ms` 包含预处理时间。


## Public proxy / 公网反代

This service binds `127.0.0.1:18765` on a machine without a public IP. To expose search to the internet, run `search-proxy` on a VPS that **has** a public IP, and `search-relay` next to this service. Do **not** bind search-service itself to `0.0.0.0`.

本机无公网 IP，search-service 只听 `127.0.0.1:18765`。在有公网 IP 的机器上跑 `search-proxy`，在本机跑 `search-relay` 反向隧道。不要把 search-service 绑到 `0.0.0.0`。

```
public clients
    → search-proxy :18780  (Bearer token)
        → tunnel :18781
            → search-relay (this box)
                → http://127.0.0.1:18765
```

Public HTTP `/health`, `/search`, and `/download` accept **either**:

1. `SEARCH_TOKEN` as `Authorization: Bearer …` or `?token=` (ops / internal)
2. a valid MaClaw Hub viewer, session, or machine token (the signed-in Hub credential). The proxy checks it with `GET {HUB_AUTH_BASE}/api/llm/v1/models` and `Authorization: Bearer <token>`. HTTP 2xx means valid. Timeout is about 5s. Positive results are cached about 5 minutes, keyed by SHA-256 of the token.

`/settings` and `/settings/*` are forwarded without a proxy-side Bearer check so the browser login page can render. The search process requires a Hub **global admin** cookie, an admin Bearer, or operator `SEARCH_TOKEN` for the settings HTML and for `/settings/config`. `/search` does **not** accept the settings cookie and does **not** require an admin cookie (agents keep using Hub viewer / `SEARCH_TOKEN`).

Hub global admin login is enough; users never paste a RapidSearch or Hub viewer token. Tokens are never logged.

**nginx:** the existing `location /searchproxy/` snippet already `proxy_pass`es to the proxy root and sets `Authorization`. Cookie and `Set-Cookie` pass through with default `proxy_pass` (no extra `proxy_set_header Cookie` is required for a new deploy). Cookie `Path=/` covers both `/settings` and `/searchproxy/settings`.

`HUB_AUTH_BASES` is a comma-separated list of Hub origins. Default: `https://hub.mypapers.top,https://hub.maclaw.top`.

The tunnel `AUTH` line remains `SEARCH_TOKEN` only. Do not put Hub tokens in the tunnel handshake.

Shared secret `SEARCH_TOKEN` (file `./proxy.token`, chmod 600) is still required for the relay tunnel. `run-proxy.sh` generates one if missing. Copy the same file to the other host.

1. On the public VPS:

```bash
export SEARCH_TOKEN="$(cat proxy.token)"   # or let run-proxy.sh read ./proxy.token
./run-proxy.sh
# PROXY_LISTEN=0.0.0.0:18780  TUNNEL_LISTEN=0.0.0.0:18781
# HUB_AUTH_BASES defaults to https://hub.mypapers.top,https://hub.maclaw.top
```

2. On this box (search-service already running):

```bash
export PROXY_TUNNEL="PUBLIC_IP:18781"   # or http://PUBLIC_IP:18781
./run-relay.sh
# SEARCH_BACKEND defaults to http://127.0.0.1:18765
```

3. Clients hit the proxy:

```bash
TOKEN="$(cat proxy.token)"
curl -sS -H "Authorization: Bearer $TOKEN" http://PUBLIC_IP:18780/health
curl -sS -H "Authorization: Bearer $TOKEN" \
  'http://PUBLIC_IP:18780/search?q=北京天气&n=3&content=0'
# settings UI (also https://hub.maclaw.top/searchproxy/settings when nginx prefixes /searchproxy/)
curl -sS -H "Authorization: Bearer $TOKEN" http://PUBLIC_IP:18780/settings/config
# equivalently ?token= on the query string
```

If no relay is connected, the proxy returns `503 {"error":"search backend offline","code":"offline"}`. Tunnel protocol: TCP, `AUTH <SEARCH_TOKEN>` then length-prefixed JSON request/response frames (bodies base64). Latest tunnel connection wins. Hub tokens are not used in that handshake.

Binaries: `go build -o search-proxy ./cmd/proxy` and `go build -o search-relay ./cmd/relay`. Copy them to the VPS; they are static-ish Go binaries (same module).

### One-password VPS deploy / 一键部署（只输入一次 root 密码）

Windows 11: run `deploy-proxy.cmd` from the repo root (or double-click it). Linux/mac: `./deploy-proxy.sh`. Type the **root** password when `ssh` asks (once). Builds linux/amd64, installs `/opt/search-proxy` on `root@hub.maclaw.top`, and restarts `search-proxy.service`. Override host with env `SEARCH_PROXY_HOST`.

Windows 11：在仓库根目录运行 `deploy-proxy.cmd`。Linux/mac：`./deploy-proxy.sh`。`ssh` 提示时输入一次 root 密码即可。

## Concurrency / 并发

Identical in-flight searches (same cache key) share one live trip via singleflight. Cache hits never touch Chrome or HTTP SERP. A **pool of independent headed Chrome processes** (default 3, `SEARCH_BROWSER_INSTANCES`, clamp 1–4) each runs one SERP on a **fresh stealth page** (closed afterwards). Total in-flight Chrome SERPs = instance count. Extra Chrome work **waits in an admission queue** (handler timeout ~170s). HTTP SERP fetches are capped separately by `SEARCH_HTTP_MAX` (default 10) so a 100-way stampede queues instead of opening 100 baidu/bing GETs. If a request cannot start HTTP or Chrome before the deadline it returns `ok:false` with `code=timeout` (or `busy` when `SEARCH_QUEUE_MAX` is set and the waiter list is full) — never HTTP 200 with empty results. `SEARCH_BROWSER_SLOTS` if set caps Chrome (`min(instances, slots)`); per-process is always 1.

Google is globally serialized (at most one in-flight Google across the whole pool) and still paced 8–15s between navigations. Auto never launches Google Chrome. If an instance hits `code=captcha` on explicit Google, it is quarantined from Google for ~10 minutes (still used for other engines). Hedged failover may start the next **Chrome** engine after ~3s (max 2 in-flight) only after the HTTP chain is exhausted.

Instances launch lazily (instance 0 on first search / Ensure; others when all busy). If one Chrome dies it is relaunched alone; the pool is not killed. Downloads (`WithPage`) use any idle instance and do not take a SERP slot.

相同缓存键的进行中查询合并为一次 Chrome 抓取。缓存命中不走浏览器。默认 3 个独立 headed Chrome 进程，每进程同时只跑 1 路 SERP。Google 全池串行并保持 8–15 秒间隔；某实例 Google 验证码后约 10 分钟不再用它打 Google（仍可用于其他引擎）。下载不占用搜索槽位。

## Limitations / 限制

- **CAPTCHA / unusual traffic**: Google in particular often flags datacenter IPs and automation. Auto mode exhausts HTTP engines (DDG HTML / Bing / Sogou / 360 / Baidu) and never launches Google Chrome. Explicit engine returns `code=captcha` (no loop unless `fallback=1`) and may save a PNG under `./debug/`. Baidu may show `wappass` / 安全验证 (same `code=captcha`). Bing is usually less aggressive.
- Google 更容易出人机验证。`engine=auto` 先走全部 HTTP 引擎，不打 Google Chrome。显式引擎默认不 failover。百度可能出现 `wappass` / 安全验证，同样返回 `code=captcha`。
- Organic results only; ads / “people also ask” are skipped as best-effort. Selectors change — parsers use fallbacks.
- 只解析自然结果；广告和 PAA 尽量跳过。页面 DOM 会变，解析做了多选择器兜底。
- Requires a working display (`DISPLAY`) and Chrome/Chromium. No GPU needed.
- 需要可用的图形显示与 Chrome/Chromium。不需要 GPU。
