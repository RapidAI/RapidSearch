package search

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultHTTPTryTimeout = 5 * time.Second
	minHTTPTryTimeout     = 1 * time.Second
	maxHTTPTryTimeout     = 15 * time.Second
	envHTTPTryTimeout     = "SEARCH_HTTP_TRY_TIMEOUT"
)

var httpSearchClient = &http.Client{
	Timeout: maxHTTPTryTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

type htmlGetter func(ctx context.Context, rawURL string) (body string, status int, err error)

var getSearchHTML htmlGetter = defaultGetSearchHTML

func defaultGetSearchHTML(ctx context.Context, rawURL string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := httpSearchClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil && len(b) == 0 {
		return "", resp.StatusCode, err
	}
	if !utf8.Valid(b) {
		b = []byte(strings.ToValidUTF8(string(b), ""))
	}
	return string(b), resp.StatusCode, nil
}

// NeedsChrome is false for HTML-only engines (they must not take a SERP slot).
func NeedsChrome(engine string) bool {
	name, err := NormalizeEngine(engine)
	if err != nil || name == "auto" {
		return true
	}
	e, ok := engines[name]
	return !ok || !e.HTTPOnly
}

// SupportsHTTP reports engines that can parse a SERP from a plain GET.
func SupportsHTTP(engine string) bool {
	name, err := NormalizeEngine(engine)
	if err != nil {
		return false
	}
	switch name {
	case "duckduckgo_html", "sogou", "360", "baidu", "bing", "serper", "brave":
		return true
	default:
		return false
	}
}

func httpSERPURLs(engine, query string) []string {
	q := url.QueryEscape(query)
	switch engine {
	case "duckduckgo_html":
		return []string{
			"https://html.duckduckgo.com/html/?q=" + q,
			"https://duckduckgo.com/html/?q=" + q,
		}
	case "sogou":
		return []string{"https://www.sogou.com/web?query=" + q}
	case "360":
		return []string{"https://www.so.com/s?q=" + q}
	case "baidu":
		return []string{
			"https://www.baidu.com/s?ie=utf-8&wd=" + q,
			"https://www.baidu.com/s?wd=" + q,
		}
	case "bing":
		return []string{"https://www.bing.com/search?q=" + q}
	default:
		return nil
	}
}

func engineSERPURL(engine, query string) string {
	q := url.QueryEscape(query)
	switch engine {
	case "sogou":
		return "https://www.sogou.com/web?query=" + q
	case "360":
		return "https://www.so.com/s?q=" + q
	case "baidu":
		return "https://www.baidu.com/s?ie=utf-8&wd=" + q
	case "bing":
		return "https://www.bing.com/search?q=" + q
	default:
		return ""
	}
}

func httpLooksBlocked(engine, body string) bool {
	low := strings.ToLower(body)
	switch engine {
	case "duckduckgo_html", "duckduckgo":
		return strings.Contains(low, "bots use duckduckgo") ||
			strings.Contains(low, "please complete the following challenge")
	case "sogou":
		if strings.Contains(low, "class=\"vrwrap\"") || strings.Contains(low, "class='vrwrap'") || strings.Contains(low, `class="rb"`) {
			return false
		}
		return strings.Contains(low, "seccode") || strings.Contains(low, "anti-bot")
	case "360":
		if strings.Contains(low, "res-list") || strings.Contains(low, `id="main"`) {
			return false
		}
		return strings.Contains(low, "captcha") && strings.Contains(low, "verify")
	case "baidu":
		if strings.Contains(low, "content_left") && (strings.Contains(low, "c-container") || strings.Contains(low, "class=\"result\"") || strings.Contains(low, "class='result'")) {
			return false
		}
		return strings.Contains(low, "wappass") || strings.Contains(low, "安全验证") ||
			(strings.Contains(low, "captcha") && strings.Contains(low, "verify"))
	case "bing":
		if strings.Contains(low, "b_algo") {
			return false
		}
		return strings.Contains(low, "please verify you are a human") ||
			strings.Contains(low, "help us confirm you are not a robot") ||
			strings.Contains(low, `id="b_captcha"`) || strings.Contains(low, `id='b_captcha'`)
	default:
		return false
	}
}

// RunHTTP fetches a SERP over net/http (no Chrome). Used for duckduckgo_html
// and as a first try for sogou/360/baidu/bing when the first response has organic hits.
func RunHTTP(ctx context.Context, engineName, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, NewError(CodeBadRequest, "query is required")
	}
	engName, err := concreteEngine(engineName)
	if err != nil {
		return nil, err
	}
	limit = ClampLimit(limit)
	if isKeyedEngine(engName) {
		return runKeyedHTTP(ctx, engName, query, limit)
	}
	urls := httpSERPURLs(engName, query)
	if len(urls) == 0 {
		return nil, NewError(CodeEngine, engName+" does not support HTTP search")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Do not call PaceEngine here: the HTTP admission cap (SEARCH_HTTP_MAX)
	// is the stampede brake. A process-wide 1.5–4s gap would serialize 100
	// waiters past the handler deadline and starve later HTTP engines.

	try := HTTPTryTimeout()
	var lastErr error
	for _, u := range urls {
		if err := ctx.Err(); err != nil {
			return nil, NewError(CodeTimeout, "timeout fetching "+engName)
		}
		reqCtx, cancel := context.WithTimeout(ctx, try)
		body, status, err := getSearchHTML(reqCtx, u)
		cancel()
		if err != nil {
			if isTimeout(err) || reqCtx.Err() != nil {
				lastErr = NewError(CodeTimeout, "timeout fetching "+engName)
			} else {
				lastErr = NewError(CodeParse, err.Error())
			}
			log.Printf("search step=http engine=%s url=%s err=%v", engName, u, err)
			continue
		}
		if status >= 400 {
			lastErr = NewError(CodeParse, fmt.Sprintf("%s HTTP %d", engName, status))
			log.Printf("search step=http engine=%s url=%s status=%d", engName, u, status)
			continue
		}
		if httpLooksBlocked(engName, body) {
			lastErr = NewError(CodeCaptcha, engName+" captcha / bot wall")
			log.Printf("search step=http engine=%s url=%s blocked", engName, u)
			continue
		}
		hits := parseEngineHTML(engName, body, limit)
		if len(hits) == 0 {
			lastErr = NewError(CodeParse, "no organic results parsed from "+engName)
			log.Printf("search step=http engine=%s url=%s count=0", engName, u)
			continue
		}
		for i := range hits {
			hits[i].Rank = i + 1
		}
		log.Printf("search step=http engine=%s url=%s count=%d", engName, u, len(hits))
		return hits, nil
	}
	if lastErr == nil {
		lastErr = NewError(CodeParse, "no organic results parsed from "+engName)
	}
	return nil, lastErr
}

// HTTPTryTimeout is the per-engine HTTP SERP budget (default 5s) so auto
// can fail over to the next HTTP engine instead of waiting on one GET.
func HTTPTryTimeout() time.Duration {
	return parseHTTPTryTimeout(os.Getenv(envHTTPTryTimeout))
}

func parseHTTPTryTimeout(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultHTTPTryTimeout
	}
	var d time.Duration
	if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
		d = parsed
	} else if n, err := strconv.Atoi(v); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	} else {
		return defaultHTTPTryTimeout
	}
	if d < minHTTPTryTimeout {
		return minHTTPTryTimeout
	}
	if d > maxHTTPTryTimeout {
		return maxHTTPTryTimeout
	}
	return d
}
