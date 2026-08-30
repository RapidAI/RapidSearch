package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"

	"search-service/internal/browser"
	"search-service/internal/cache"
	"search-service/internal/download"
	"search-service/internal/search"
)

type Server struct {
	mgr      *browser.Manager
	debugDir string
	cache    *cache.Cache
	dl       *download.Downloader
	mux      *http.ServeMux
}

func New(mgr *browser.Manager, debugDir string, c *cache.Cache, dl *download.Downloader) http.Handler {
	s := &Server{mgr: mgr, debugDir: debugDir, cache: c, dl: dl, mux: http.NewServeMux()}
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/search", s.handleSearch)
	s.mux.HandleFunc("/cache/stats", s.handleCacheStats)
	s.mux.HandleFunc("/download", s.handleDownload)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil)
		return
	}
	if s.cache == nil {
		writeJSON(w, http.StatusOK, cache.Stats{})
		return
	}
	writeJSON(w, http.StatusOK, s.cache.Stats())
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil)
		return
	}
	if s.dl == nil {
		writeErr(w, http.StatusBadGateway, "download not configured", "fetch", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.dl.Timeout())
	defer cancel()
	s.dl.Serve(ctx, w, r)
}

type postBody struct {
	Query    string `json:"query"`
	Engine   string `json:"engine"`
	Limit    int    `json:"limit"`
	Content  *bool  `json:"content"`
	Region   string `json:"region"`
	Locale   string `json:"locale"`
	Fallback *bool  `json:"fallback"`
}

type successBody struct {
	Query           string          `json:"query"`
	Engine          string          `json:"engine"`
	RequestedEngine string          `json:"requested_engine"`
	Tried           []string        `json:"tried"`
	Results         []search.Result `json:"results"`
	Count           int             `json:"count"`
	TookMs          int64           `json:"took_ms"`
	Cached          bool            `json:"cached,omitempty"`
	CacheAgeMs      int64           `json:"cache_age_ms,omitempty"`
}

type errBody struct {
	Error string   `json:"error"`
	Code  string   `json:"code"`
	Tried []string `json:"tried,omitempty"`
}

const (
	handlerTimeout = 170 * time.Second
	perTryTimeout  = 40 * time.Second
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil)
		return
	}

	var q, engine, region, locale, hl string
	var limit int
	wantContent := true
	var fallbackSet, fallback bool

	switch r.Method {
	case http.MethodGet:
		q = r.URL.Query().Get("q")
		if q == "" {
			q = r.URL.Query().Get("query")
		}
		engine = r.URL.Query().Get("engine")
		if n := r.URL.Query().Get("n"); n != "" {
			limit, _ = strconv.Atoi(n)
		} else if n := r.URL.Query().Get("limit"); n != "" {
			limit, _ = strconv.Atoi(n)
		}
		if v := r.URL.Query().Get("content"); v != "" {
			wantContent = parseContentFlag(v)
		}
		region = r.URL.Query().Get("region")
		locale = r.URL.Query().Get("locale")
		hl = r.URL.Query().Get("hl")
		if v, ok := r.URL.Query()["fallback"]; ok && len(v) > 0 {
			fallbackSet = true
			fallback = parseTruthy(v[0])
		}
	case http.MethodPost:
		defer r.Body.Close()
		var body postBody
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil)
			return
		}
		q = body.Query
		engine = body.Engine
		limit = body.Limit
		if body.Content != nil {
			wantContent = *body.Content
		}
		region = body.Region
		locale = body.Locale
		if body.Fallback != nil {
			fallbackSet = true
			fallback = *body.Fallback
		}
	}

	q = strings.TrimSpace(q)
	if q == "" {
		writeErr(w, http.StatusBadRequest, "query is required", search.CodeBadRequest, nil)
		return
	}
	requested, err := search.NormalizeEngine(engine)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), search.CodeEngine, nil)
		return
	}
	limit = search.ClampLimit(limit)

	hints := search.RouteHints{Query: q, Region: region, Locale: locale, HL: hl}
	useFallback := search.ShouldFallback(requested, fallbackSet, fallback)
	chain := search.Schedule(requested, useFallback, hints)
	if len(chain) == 0 {
		writeErr(w, http.StatusBadRequest, "no engines scheduled", search.CodeEngine, nil)
		return
	}

	keyIn := cache.KeyInput{
		Query:    q,
		Engine:   requested,
		Limit:    limit,
		Content:  wantContent,
		Region:   region,
		Locale:   locale,
		HL:       hl,
		Fallback: useFallback,
	}
	bypass := cacheBypass(r)

	start := time.Now()
	if s.cache != nil && !bypass {
		if hit, stored, ok := s.cache.Get(keyIn); ok {
			age := time.Since(stored).Milliseconds()
			if age < 0 {
				age = 0
			}
			log.Printf("search cache=hit query=%q engine=%s content=%v age_ms=%d", shortQuery(q), requested, wantContent, age)
			writeJSON(w, http.StatusOK, successBody{
				Query:           hit.Query,
				Engine:          hit.Engine,
				RequestedEngine: hit.RequestedEngine,
				Tried:           hit.Tried,
				Results:         hit.Results,
				Count:           hit.Count,
				TookMs:          time.Since(start).Milliseconds(),
				Cached:          true,
				CacheAgeMs:      age,
			})
			return
		}
		log.Printf("search cache=miss query=%q engine=%s content=%v", shortQuery(q), requested, wantContent)
	} else if bypass {
		log.Printf("search cache=bypass query=%q engine=%s", shortQuery(q), requested)
	}

	// Whole request ~3 min (WriteTimeout is 3 min). Each Chrome attempt is
	// ~40s so a 3-engine chain plus preprocess still fits.
	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()

	var (
		results   []search.Result
		tried     []string
		wonEngine string
		lastErr   error
	)

	for _, eng := range chain {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		tried = append(tried, eng)
		log.Printf("search attempt engine=%s requested=%s query=%q fallback=%v chain=%v", eng, requested, shortQuery(q), useFallback, chain)

		err := s.mgr.Do(ctx, func(page *rod.Page) error {
			searchCtx, cancelSearch := context.WithTimeout(ctx, perTryTimeout)
			defer cancelSearch()
			page = page.Context(searchCtx)
			var e error
			results, e = search.Run(page, eng, q, limit)
			if e != nil && search.Is(e, search.CodeCaptcha) {
				_ = s.mgr.Screenshot(page, "captcha-"+eng)
			}
			return e
		})
		if err == nil && len(results) > 0 {
			wonEngine = eng
			log.Printf("search attempt engine=%s ok count=%d", eng, len(results))
			break
		}
		lastErr = err
		if err == nil && len(results) == 0 {
			lastErr = search.NewError(search.CodeParse, "no organic results parsed from "+eng)
		}
		code := search.CodeOf(lastErr)
		log.Printf("search attempt engine=%s failed code=%s err=%v; next in chain", eng, code, lastErr)
		results = nil
	}

	if wonEngine == "" {
		took := time.Since(start).Milliseconds()
		if lastErr == nil {
			lastErr = search.NewError(search.CodeParse, "all engines failed")
		}
		code := search.CodeOf(lastErr)
		msg := lastErr.Error()
		if errors.Is(lastErr, context.DeadlineExceeded) || errors.Is(lastErr, context.Canceled) {
			code = search.CodeTimeout
			msg = "search timed out"
		}
		status := http.StatusBadGateway
		switch code {
		case search.CodeBadRequest, search.CodeEngine:
			status = http.StatusBadRequest
		case search.CodeTimeout:
			status = http.StatusGatewayTimeout
		case search.CodeCaptcha:
			status = http.StatusForbidden
		}
		log.Printf("search query=%q requested=%s tried=%v count=0 took_ms=%d error=%s code=%s", shortQuery(q), requested, tried, took, msg, code)
		writeErr(w, status, msg, code, tried)
		return
	}

	// Chrome mutex already released. Filter/score (and optional HTTP fetch)
	// run here so landing-page extraction cannot block other searches.
	results = search.Preprocess(ctx, results, search.PreprocessOpts{
		Query:        q,
		Engine:       wonEngine,
		Limit:        limit,
		FetchContent: wantContent,
	})
	took := time.Since(start).Milliseconds()

	if len(results) > 0 && s.dl != nil {
		urls := make([]string, 0, len(results))
		for _, r := range results {
			if r.URL != "" {
				urls = append(urls, r.URL)
			}
		}
		s.dl.RememberSearchURLs(urls)
	}

	// Persist after Chrome is released. Do not cache empty / error bodies.
	if s.cache != nil && len(results) > 0 {
		s.cache.Put(keyIn, cache.Payload{
			Query:           q,
			Engine:          wonEngine,
			RequestedEngine: requested,
			Tried:           tried,
			Results:         results,
			Count:           len(results),
		})
	}

	log.Printf("search query=%q engine=%s requested=%s tried=%v count=%d took_ms=%d content=%v error=", shortQuery(q), wonEngine, requested, tried, len(results), took, wantContent)
	writeJSON(w, http.StatusOK, successBody{
		Query:           q,
		Engine:          wonEngine,
		RequestedEngine: requested,
		Tried:           tried,
		Results:         results,
		Count:           len(results),
		TookMs:          took,
	})
}

func cacheBypass(r *http.Request) bool {
	if v := r.URL.Query().Get("nocache"); v != "" && (v == "1" || parseTruthy(v)) {
		return true
	}
	cc := strings.ToLower(r.Header.Get("Cache-Control"))
	return strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store")
}

func shortQuery(q string) string {
	const max = 80
	if len(q) <= max {
		return q
	}
	return q[:max] + "…"
}

func parseContentFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "n":
		return false
	default:
		return true
	}
}

func parseTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

func writeErr(w http.ResponseWriter, status int, msg, code string, tried []string) {
	writeJSON(w, status, errBody{Error: msg, Code: code, Tried: tried})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
