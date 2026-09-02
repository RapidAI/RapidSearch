package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	defaultSerperURL = "https://google.serper.dev/search"
	defaultBraveURL  = "https://api.search.brave.com/res/v1/web/search"
	maxKeyedBody     = 2 * 1024 * 1024
)

var keyedHTTPClient = &http.Client{
	Timeout: maxHTTPTryTimeout,
}

type keyedHooks struct {
	mu        sync.RWMutex
	serperURL string
	braveURL  string
	lookup    func(engine string) string
	hasLookup bool
	do        func(req *http.Request) (*http.Response, error)
}

var keyed = keyedHooks{
	serperURL: defaultSerperURL,
	braveURL:  defaultBraveURL,
}

func testHasLookup() bool {
	keyed.mu.RLock()
	defer keyed.mu.RUnlock()
	return keyed.hasLookup
}

func testLookupAPIKey(engine string) string {
	keyed.mu.RLock()
	fn := keyed.lookup
	has := keyed.hasLookup
	keyed.mu.RUnlock()
	if !has || fn == nil {
		return ""
	}
	return fn(engine)
}

func keyedSerperURL() string {
	keyed.mu.RLock()
	defer keyed.mu.RUnlock()
	if keyed.serperURL != "" {
		return keyed.serperURL
	}
	return defaultSerperURL
}

func keyedBraveURL() string {
	keyed.mu.RLock()
	defer keyed.mu.RUnlock()
	if keyed.braveURL != "" {
		return keyed.braveURL
	}
	return defaultBraveURL
}

func keyedDo(req *http.Request) (*http.Response, error) {
	keyed.mu.RLock()
	fn := keyed.do
	keyed.mu.RUnlock()
	if fn != nil {
		return fn(req)
	}
	return keyedHTTPClient.Do(req)
}

// SetKeyedTestHooks overrides API URLs and key lookup for tests. Call reset
// from t.Cleanup. lookup == nil with hasLookup true means "no keys".
func SetKeyedTestHooks(serperURL, braveURL string, lookup func(engine string) string) {
	keyed.mu.Lock()
	defer keyed.mu.Unlock()
	if serperURL != "" {
		keyed.serperURL = serperURL
	}
	if braveURL != "" {
		keyed.braveURL = braveURL
	}
	keyed.lookup = lookup
	keyed.hasLookup = true
}

// ResetKeyedTestHooks restores production Serper/Brave endpoints.
func ResetKeyedTestHooks() {
	keyed.mu.Lock()
	keyed.serperURL = defaultSerperURL
	keyed.braveURL = defaultBraveURL
	keyed.lookup = nil
	keyed.hasLookup = false
	keyed.do = nil
	keyed.mu.Unlock()
}

func runKeyedHTTP(ctx context.Context, engine, query string, limit int) ([]Result, error) {
	key := strings.TrimSpace(lookupAPIKey(engine))
	if key == "" {
		return nil, NewError(CodeUnauthorized, engine+" API key is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	try := HTTPTryTimeout()
	reqCtx, cancel := context.WithTimeout(ctx, try)
	defer cancel()

	var (
		hits []Result
		err  error
	)
	switch engine {
	case "serper":
		hits, err = fetchSerper(reqCtx, key, query, limit)
	case "brave":
		hits, err = fetchBrave(reqCtx, key, query, limit)
	default:
		return nil, NewError(CodeEngine, engine+" is not a keyed engine")
	}
	if err != nil {
		if isTimeout(err) || reqCtx.Err() != nil {
			if e, ok := err.(*Error); ok {
				return nil, e
			}
			return nil, NewError(CodeTimeout, "timeout fetching "+engine)
		}
		return nil, err
	}
	if len(hits) == 0 {
		return nil, NewError(CodeParse, "no organic results parsed from "+engine)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Rank = i + 1
	}
	log.Printf("search step=http engine=%s via=api count=%d", engine, len(hits))
	return hits, nil
}

type serperReq struct {
	Q   string `json:"q"`
	Num int    `json:"num,omitempty"`
}

type serperResp struct {
	Organic []serperHit `json:"organic"`
}

type serperHit struct {
	Title    string `json:"title"`
	Link     string `json:"link"`
	Snippet  string `json:"snippet"`
	Position int    `json:"position"`
}

func fetchSerper(ctx context.Context, key, query string, limit int) ([]Result, error) {
	body, err := json.Marshal(serperReq{Q: query, Num: limit})
	if err != nil {
		return nil, NewError(CodeParse, "serper encode: "+err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keyedSerperURL(), bytes.NewReader(body))
	if err != nil {
		return nil, NewError(CodeEngine, "serper request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", key)
	req.Header.Set("User-Agent", desktopUA)

	raw, status, err := doKeyed(req)
	if err != nil {
		return nil, err
	}
	if err := keyedStatus("serper", status); err != nil {
		return nil, err
	}
	var parsed serperResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, NewError(CodeParse, "serper json: "+err.Error())
	}
	out := make([]Result, 0, len(parsed.Organic))
	for _, h := range parsed.Organic {
		u := cleanURL(h.Link, "serper")
		title := strings.TrimSpace(h.Title)
		if title == "" || u == "" {
			continue
		}
		out = append(out, Result{
			Title:   title,
			URL:     u,
			Snippet: strings.TrimSpace(h.Snippet),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type braveResp struct {
	Web *braveWeb `json:"web"`
}

type braveWeb struct {
	Results []braveHit `json:"results"`
}

type braveHit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

func fetchBrave(ctx context.Context, key, query string, limit int) ([]Result, error) {
	u, err := url.Parse(keyedBraveURL())
	if err != nil {
		return nil, NewError(CodeEngine, "brave url: "+err.Error())
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, NewError(CodeEngine, "brave request: "+err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("User-Agent", desktopUA)

	raw, status, err := doKeyed(req)
	if err != nil {
		return nil, err
	}
	if err := keyedStatus("brave", status); err != nil {
		return nil, err
	}
	var parsed braveResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, NewError(CodeParse, "brave json: "+err.Error())
	}
	var hits []braveHit
	if parsed.Web != nil {
		hits = parsed.Web.Results
	}
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		dest := cleanURL(h.URL, "brave")
		title := strings.TrimSpace(h.Title)
		if title == "" || dest == "" {
			continue
		}
		out = append(out, Result{
			Title:   title,
			URL:     dest,
			Snippet: strings.TrimSpace(h.Description),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func doKeyed(req *http.Request) ([]byte, int, error) {
	resp, err := keyedDo(req)
	if err != nil {
		if isTimeout(err) {
			return nil, 0, NewError(CodeTimeout, "timeout fetching keyed API")
		}
		return nil, 0, NewError(CodeEngine, err.Error())
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyedBody))
	if err != nil && len(b) == 0 {
		return nil, resp.StatusCode, NewError(CodeParse, err.Error())
	}
	if !utf8.Valid(b) {
		b = []byte(strings.ToValidUTF8(string(b), ""))
	}
	return b, resp.StatusCode, nil
}

func keyedStatus(engine string, status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return NewError(CodeUnauthorized, engine+" API key rejected")
	case status == http.StatusTooManyRequests:
		return NewError(CodeBusy, engine+" API rate limited")
	case status >= 500:
		return NewError(CodeEngine, fmt.Sprintf("%s HTTP %d", engine, status))
	case status >= 400:
		return NewError(CodeEngine, fmt.Sprintf("%s HTTP %d", engine, status))
	default:
		return nil
	}
}
