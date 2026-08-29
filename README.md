# RapidSearch

A search service for Agent.

Local HTTP JSON API that searches the web by driving a real Chrome/Chromium window (go-rod + stealth). It does **not** call third-party search APIs.

本地 HTTP JSON 接口：用真实 Chrome 打开搜索引擎首页，模拟输入并解析结果页。不调用 SerpAPI / CSE 等第三方搜索 API。默认 `engine=auto`：中文/中国相关走百度，其余走 Google，失败则按链 failover。

## Run / 启动

```bash
cd /workspace/search-service
./run.sh
```

Listens on `127.0.0.1:18765`. Chrome profile (cookies) persists in `./chrome-profile`.

监听 `127.0.0.1:18765`。Cookie 保存在 `./chrome-profile`。

Stop: `Ctrl+C`, or `pkill -f '/workspace/search-service/search-service'`.

停止：前台 `Ctrl+C`，或 `pkill -f '/workspace/search-service/search-service'`。

Environment / 环境变量:

- `DISPLAY` (default `:1`)
- `CHROME_BIN` (optional chrome path)
- `SEARCH_LISTEN` (optional, default `127.0.0.1:18765`)

## API

`GET /health` → `{"ok":true}`

`GET /search?q=<query>&engine=auto|google|bing|baidu|duckduckgo&n=10&content=1&fallback=1`

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

Success:

```json
{
  "query": "golang http server",
  "engine": "bing",
  "requested_engine": "auto",
  "tried": ["google", "bing"],
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

`engine` is the engine that actually produced results (never `"auto"`). `requested_engine` is what the caller asked for. `tried` lists every engine attempted.

Errors: HTTP 4xx/5xx with `{"error":"...","code":"captcha"|"timeout"|"parse"|"bad_request"|"engine"}`.

## Engines / 引擎

| engine | homepage | aliases |
|---|---|---|
| `auto` (default when omitted) | (router) | |
| `google` | https://www.google.com/ | |
| `bing` | https://www.bing.com/ | |
| `baidu` | https://www.baidu.com/ | `bd` |
| `duckduckgo` | https://duckduckgo.com/ | `ddg`, `duck` |

### Auto routing / 自动调度

**China path** if any of:

- the query contains Han (CJK) characters / 查询含汉字
- `region=cn` / `locale=zh` / `hl=zh-CN` (GET) or POST `region` / `locale`
- English/pinyin CN-intent tokens (small list): `china`, `chinese`, `beijing`, `shanghai`, `wechat`, `weixin`, `zhihu`, `bilibili`, `xiaohongshu`, `bytedance`

**Global path** otherwise.

Failover chains (next engine on captcha, timeout, parse error, or empty results; never retry the same engine):

| path | chain |
|---|---|
| China | `baidu` → `bing` → `duckduckgo` |
| Global | `google` → `bing` → `duckduckgo` |

Google is **not** on the China chain (captcha-prone here, wrong corpus).

Explicit `engine=google|bing|baidu|duckduckgo` is predictable: no failover unless `fallback=1`. Auto defaults to failover on (`fallback=0` disables it).

Per-try Chrome timeout ~40s; whole request ~3 min. One engine per `mgr.Do` (browser mutex released between attempts). Preprocess (relevance + optional content extract) runs on the **winning** result set, including Baidu hits. `content=0` still skips fetch.

搜索串行执行（同一时间只有一个 Chrome 任务）。并发请求会排队。单引擎尝试约 40 秒，整请求约 3 分钟。

## Preprocessing / 结果预处理

After organic SERP parse, Chrome is released, then results are:

有机结果解析完成后释放 Chrome，然后：

1. **Clean / 清洗**: unwrap tracker URLs, trim, drop empty title/url, `javascript:`, and engine-internal links.
   解开跟踪链接，去掉空标题/URL、`javascript:` 和搜索引擎站内链接。
2. **Relevance filter / 相关性过滤**: tokenize English (non-alnum split) and CJK (runs + 2-grams). Score title + snippet + URL path vs the query. Drop off-topic / empty-snippet junk unless the title is a strong match. Re-rank by score (`relevance` is 0..1) and reassign `rank` 1..n. If every hit would be dropped, the cleaned original list is returned instead of an error.
   英文按非字母数字切词，中日韩按连续字串（可选二元组）切词。用标题+摘要+URL 路径对查询打分。丢掉跑题或空摘要垃圾（标题强匹配除外）。按分数重排，`relevance` 为 0..1。若过滤后一条不剩，则回退到清洗后的原始列表，不报错。
3. **Extract / 抽取正文** (default): `net/http` (not Chrome), desktop User-Agent, 8s timeout, max 3 concurrent, first `min(limit, 8)` hits. Strip script/style/nav/footer; keep sentences overlapping query tokens (+ one neighbor), cap ~1200 runes. Failures, PDFs, binary, 4xx/5xx → omit `content`. `took_ms` includes this step.
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

Shared secret is `SEARCH_TOKEN` (file `./proxy.token`, chmod 600). `run-proxy.sh` generates one if missing. Copy the same file to the other host. The token is never logged.

1. On the public VPS:

```bash
export SEARCH_TOKEN="$(cat proxy.token)"   # or let run-proxy.sh read ./proxy.token
./run-proxy.sh
# PROXY_LISTEN=0.0.0.0:18780  TUNNEL_LISTEN=0.0.0.0:18781
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

If no relay is connected, the proxy returns `503 {"error":"search backend offline","code":"offline"}`. Tunnel protocol: TCP, `AUTH <token>` then length-prefixed JSON request/response frames (bodies base64). Latest tunnel connection wins.

Binaries: `go build -o search-proxy ./cmd/proxy` and `go build -o search-relay ./cmd/relay`. Copy them to the VPS; they are static-ish Go binaries (same module).

## Limitations / 限制

- **CAPTCHA / unusual traffic**: Google in particular often flags datacenter IPs and automation. Auto mode failovers to Bing then DuckDuckGo. Explicit engine returns `code=captcha` (no loop unless `fallback=1`) and may save a PNG under `./debug/`. Baidu may show `wappass` / 安全验证 (same `code=captcha`). Bing is usually less aggressive.
- Google 更容易出人机验证。`engine=auto` 会按链切到 Bing / DuckDuckGo。显式引擎默认不 failover。百度可能出现 `wappass` / 安全验证，同样返回 `code=captcha`。
- Organic results only; ads / “people also ask” are skipped as best-effort. Selectors change — parsers use fallbacks.
- 只解析自然结果；广告和 PAA 尽量跳过。页面 DOM 会变，解析做了多选择器兜底。
- Requires a working display (`DISPLAY`) and Chrome/Chromium. No GPU needed.
- 需要可用的图形显示与 Chrome/Chromium。不需要 GPU。
