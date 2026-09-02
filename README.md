# RapidSearch

A search service for Agent.

Local HTTP JSON API that searches the web by driving a real Chrome/Chromium window (go-rod + stealth). It does **not** call third-party search APIs.

本地 HTTP JSON 接口：用真实 Chrome 打开搜索引擎首页，模拟输入并解析结果页。不调用 SerpAPI / CSE 等第三方搜索 API。默认 `engine=auto`：中文/中国相关走百度→搜狗→360，其余走 DuckDuckGo HTML→Bing（Google 在熔断关闭时仍可试），失败则按链 failover。

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
- `SEARCH_QUEUE_MAX` (optional, default `0` = unlimited waiters): max Chrome jobs waiting for a slot. Overflow returns `{"ok":false,"code":"busy"}` (HTTP 503) instead of sitting until 504. HTTP-only work never queues.
- `SEARCH_CHROME_MIN_REMAIN` (optional, default `15s`): do not start a Chrome SERP if remaining handler time is below this (Go duration or integer seconds). Those requests fail with `code=timeout` so a doomed 40s try is not started.
- `CACHE_DIR` (optional, default `./cache`)
- `CACHE_TTL` (optional Go duration, default `1h`)

## API

`GET /health` → `{"ok":true}`

`GET /search?q=<query>&engine=auto|google|bing|baidu|duckduckgo|duckduckgo_html|sogou|360&n=10&content=1&fallback=1`

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

`ok` is always `true` on HTTP 200. `engine` is the engine that actually produced results (never `"auto"`). `requested_engine` is what the caller asked for. `tried` lists every engine attempted. `skipped` is present when the Google circuit breaker dropped Google from an auto/fallback chain (so Bing winning is expected, not a silent engine swap).

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

`code` is one of `captcha` | `timeout` | `busy` | `parse` | `offline` | `unauthorized` | `engine` | `bad_request`. `error` is short English. `engine` is the last engine tried when known. Error responses are not cached. `busy` (HTTP 503) means the Chrome admission queue was full (`SEARCH_QUEUE_MAX`). `timeout` (HTTP 504) includes “could not start Chrome before the handler deadline”.


## Result cache / 结果缓存

Successful search JSON (after preprocess) is stored under `./cache` on disk. Errors, captcha, timeouts, and empty results are **not** cached.

成功的搜索 JSON（预处理后）写入 `./cache`。错误、验证码、超时、空结果不缓存。

- Key = SHA-256 of `v3|` + normalized query + engine + limit + content flag + region/locale/hl + fallback. `content=0` and `content=1` are different keys. Version bump avoids serving pre-`ok` bodies as `ok: false`.
- 缓存键 = `v3|` + 规范化查询 + 引擎 + limit + content + region/locale/hl + fallback 的 SHA-256。`content=0` 与 `content=1` 是不同的键。
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

### Auto routing / 自动调度

**China path** if any of:

- the query contains Han (CJK) characters / 查询含汉字
- `region=cn` / `locale=zh` / `hl=zh-CN` (GET) or POST `region` / `locale`
- English/pinyin CN-intent tokens (small list): `china`, `chinese`, `beijing`, `shanghai`, `wechat`, `weixin`, `zhihu`, `bilibili`, `xiaohongshu`, `bytedance`

**Global path** otherwise.

Failover chains (next engine on captcha, timeout, parse error, or empty results; never retry the same engine):

| path | chain |
|---|---|
| China | `baidu` → `sogou` → `360` → `bing` → `duckduckgo_html` → `duckduckgo` |
| Global | `duckduckgo_html` → `bing` → `google` → `duckduckgo` |

Google is **not** on the China chain (captcha-prone here, wrong corpus). Global tries DuckDuckGo HTML before Google so auto can succeed without waiting on a captcha. `duckduckgo_html` is a datacenter-friendly GET of `html.duckduckgo.com` (fallback `duckduckgo.com/html/`) and does **not** take a Chrome SERP slot. If HTML returns 0 hits, Chrome DuckDuckGo remains later in the chain. Explicit `engine=duckduckgo` means HTML then Chrome once. Explicit `engine=sogou` / `engine=360` / `engine=baidu` / `engine=bing` / `engine=duckduckgo_html` try HTTP first (Chrome only if the GET/parse fails). `engine=google` is still Chrome-only.

**Google circuit breaker** (process-wide, in addition to per-instance quarantine): after a Google captcha or “no Google Chrome instance”, auto/fallback chains **skip Google for ~15 minutes** (global becomes `duckduckgo_html` → `bing` → `duckduckgo`). A half-open probe allows one Google attempt after 15 minutes; success closes the breaker. Explicit `engine=google` is still attempted: if the breaker is open it **fails fast** with `code=captcha` (no 40s wait), or skips to the next engine when `fallback=1`.

**Hedged failover** (auto/fallback): if the first engine has not returned in ~3s and another engine is in the chain, the next engine starts in parallel (cap 2 in-flight engines per request). HTML / HTTP-first engines (`duckduckgo_html`, and HTTP probes for sogou/360/baidu/bing) do not take a Chrome instance slot. Chrome fallback waits on the admission queue (bounded to `SEARCH_BROWSER_INSTANCES`) instead of starting a Chrome job per client. First success wins; the loser is cancelled. Never runs three Googles. `ErrNoGoogleInstance` failovers immediately (does not consume the 40s per-try timeout).

Explicit `engine=google|bing|baidu|sogou|360|duckduckgo_html` is predictable: no failover unless `fallback=1`. `engine=duckduckgo` still tries HTML then Chrome once. Auto defaults to failover on (`fallback=0` disables it).

Per-try Chrome timeout ~40s; whole request ~3 min. Preprocess (relevance + optional content extract) runs on the **winning** result set, including Baidu hits. `content=0` still skips fetch.

Chrome SERP 工作最多 `SEARCH_BROWSER_INSTANCES` 路并发（默认 3 个独立 Chrome 进程，每进程 1 路 SERP）。额外请求在准入队列里等槽，而不是每人开一路 Chrome 等到 504。`duckduckgo_html` 以及百度/Bing/搜狗/360 的 HTTP 探测与落地页抽取/下载一样不占 Chrome 槽。Google 全池串行并保持 8–15s 间隔；某实例 Google captcha 后约 10 分钟内不再用该实例打 Google（仍可用于 Bing/百度/搜狗/360/DDG）。另有进程级 Google 熔断 ~15 分钟，避免 auto 在验证码上烧 40s+。缓存命中、预处理和正文抽取不受此限制。

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

Hub login is enough; users never configure a RapidSearch API key. Tokens are never logged.

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
# equivalently ?token= on the query string
```

If no relay is connected, the proxy returns `503 {"error":"search backend offline","code":"offline"}`. Tunnel protocol: TCP, `AUTH <SEARCH_TOKEN>` then length-prefixed JSON request/response frames (bodies base64). Latest tunnel connection wins. Hub tokens are not used in that handshake.

Binaries: `go build -o search-proxy ./cmd/proxy` and `go build -o search-relay ./cmd/relay`. Copy them to the VPS; they are static-ish Go binaries (same module).

### One-password VPS deploy / 一键部署（只输入一次 root 密码）

Windows 11: run `deploy-proxy.cmd` from the repo root (or double-click it). Linux/mac: `./deploy-proxy.sh`. Type the **root** password when `ssh` asks (once). Builds linux/amd64, installs `/opt/search-proxy` on `root@hub.maclaw.top`, and restarts `search-proxy.service`. Override host with env `SEARCH_PROXY_HOST`.

Windows 11：在仓库根目录运行 `deploy-proxy.cmd`。Linux/mac：`./deploy-proxy.sh`。`ssh` 提示时输入一次 root 密码即可。

## Concurrency / 并发

Identical in-flight searches (same cache key) share one Chrome trip via singleflight. Cache hits never touch Chrome. A **pool of independent headed Chrome processes** (default 3, `SEARCH_BROWSER_INSTANCES`, clamp 1–4) each runs one SERP on a **fresh stealth page** (closed afterwards). Total in-flight Chrome SERPs = instance count. Extra Chrome work **waits in an admission queue** (handler timeout ~170s) instead of overlapping until 504. If a request cannot start Chrome before the deadline it returns `ok:false` with `code=timeout` (or `busy` when `SEARCH_QUEUE_MAX` is set and the waiter list is full) — never HTTP 200 with empty results. `SEARCH_BROWSER_SLOTS` if set caps that total (`min(instances, slots)`); per-process is always 1.

Google is globally serialized (at most one in-flight Google across the whole pool) and still paced 8–15s between navigations so the same datacenter IP is not hit in parallel. Bing / Baidu / DuckDuckGo may overlap a Google on other instances. If an instance hits `code=captcha` on Google, it is quarantined from Google for ~10 minutes (still used for other engines). A **process-wide Google breaker** then skips Google on auto/fallback for ~15 minutes so later requests do not queue behind captcha. Hedged failover may start the next engine after ~3s (max 2 in-flight engines per request); HTTP probes do not take a slot, and Chrome fallbacks share the same admission cap.

Instances launch lazily (instance 0 on first search / Ensure; others when all busy). If one Chrome dies it is relaunched alone; the pool is not killed. Downloads (`WithPage`) use any idle instance and do not take a SERP slot.

相同缓存键的进行中查询合并为一次 Chrome 抓取。缓存命中不走浏览器。默认 3 个独立 headed Chrome 进程，每进程同时只跑 1 路 SERP。Google 全池串行并保持 8–15 秒间隔；某实例 Google 验证码后约 10 分钟不再用它打 Google（仍可用于其他引擎）。下载不占用搜索槽位。

## Limitations / 限制

- **CAPTCHA / unusual traffic**: Google in particular often flags datacenter IPs and automation. Auto mode tries DuckDuckGo HTML and Bing before Google (China: Baidu → Sogou → 360 → Bing → DDG HTML). Explicit engine returns `code=captcha` (no loop unless `fallback=1`) and may save a PNG under `./debug/`. Baidu may show `wappass` / 安全验证 (same `code=captcha`). Bing is usually less aggressive.
- Google 更容易出人机验证。`engine=auto` 全球链先走 DuckDuckGo HTML / Bing；中文链为百度 → 搜狗 → 360 → Bing → DDG HTML。显式引擎默认不 failover。百度可能出现 `wappass` / 安全验证，同样返回 `code=captcha`。
- Organic results only; ads / “people also ask” are skipped as best-effort. Selectors change — parsers use fallbacks.
- 只解析自然结果；广告和 PAA 尽量跳过。页面 DOM 会变，解析做了多选择器兜底。
- Requires a working display (`DISPLAY`) and Chrome/Chromium. No GPU needed.
- 需要可用的图形显示与 Chrome/Chromium。不需要 GPU。
