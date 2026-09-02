package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"

	"search-service/internal/browser"
	"search-service/internal/cache"
	"search-service/internal/download"
	"search-service/internal/proxyauth"
	"search-service/internal/search"
)

type Server struct {
	mgr       *browser.Manager
	debugDir  string
	cache     *cache.Cache
	dl        *download.Downloader
	mux       *http.ServeMux
	flight    flightGroup
	breaker   *search.GoogleBreaker
	admit     *chromeAdmit
	httpAdmit *chromeAdmit
	// hedgeAfter overrides the 3s hedged-failover delay (tests).
	hedgeAfter time.Duration
	// runEngine, if set, replaces mgr.Do + search.Run (unit tests).
	runEngine func(ctx context.Context, engine, query string, limit int) ([]search.Result, error)
	cfg       *search.Store
	auth      *proxyauth.Checker
}

type engineTransport int

const (
	transportAuto engineTransport = iota
	transportHTTP
	transportChrome
)

type ctxKey int

const httpHeldKey ctxKey = 1

func contextWithHTTPHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, httpHeldKey, true)
}

func httpHeld(ctx context.Context) bool {
	v, _ := ctx.Value(httpHeldKey).(bool)
	return v
}

func New(mgr *browser.Manager, debugDir string, c *cache.Cache, dl *download.Downloader) http.Handler {
	n := 3
	if mgr != nil {
		n = mgr.InstanceCount()
	}
	s := &Server{
		mgr:       mgr,
		debugDir:  debugDir,
		cache:     c,
		dl:        dl,
		mux:       http.NewServeMux(),
		breaker:   search.DefaultGoogleBreaker(),
		admit:     newChromeAdmit(n),
		httpAdmit: newHTTPAdmit(),
	}
	s.cfg = openSearchConfig()
	s.auth = newSettingsAuth()
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/search", s.handleSearch)
	s.mux.HandleFunc("/settings", s.handleSettingsPage)
	s.mux.HandleFunc("/settings/{$}", s.handleSettingsPage)
	s.mux.HandleFunc("/settings/config", s.handleSettingsConfig)
	s.mux.HandleFunc("/settings/login", s.handleSettingsLogin)
	s.mux.HandleFunc("/settings/logout", s.handleSettingsLogout)
	s.mux.HandleFunc("/cache/stats", s.handleCacheStats)
	s.mux.HandleFunc("/download", s.handleDownload)
	return s
}

func openSearchConfig() *search.Store {
	st, err := search.OpenConfig(search.ConfigPath())
	if err != nil {
		log.Printf("search-config: %v (continuing with empty config)", err)
		st, err = search.OpenConfig("")
		if err != nil {
			return nil
		}
	}
	search.ActivateStore(st)
	return st
}

func newSettingsAuth() *proxyauth.Checker {
	token := strings.TrimSpace(os.Getenv("SEARCH_TOKEN"))
	if token == "" {
		if b, err := os.ReadFile("proxy.token"); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	return proxyauth.New(token, proxyauth.ParseBases(os.Getenv("HUB_AUTH_BASES")))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
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
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if s.dl == nil {
		writeErr(w, http.StatusBadGateway, "download not configured", "fetch", nil, "")
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
	OK              bool            `json:"ok"`
	Query           string          `json:"query"`
	Engine          string          `json:"engine"`
	RequestedEngine string          `json:"requested_engine"`
	Tried           []string        `json:"tried"`
	Skipped         []string        `json:"skipped,omitempty"`
	Results         []search.Result `json:"results"`
	Count           int             `json:"count"`
	TookMs          int64           `json:"took_ms"`
	Cached          bool            `json:"cached,omitempty"`
	CacheAgeMs      int64           `json:"cache_age_ms,omitempty"`
}

type errBody struct {
	OK     bool     `json:"ok"`
	Error  string   `json:"error"`
	Code   string   `json:"code"`
	Engine string   `json:"engine,omitempty"`
	Tried  []string `json:"tried,omitempty"`
}

const (
	handlerTimeout = 170 * time.Second
	perTryTimeout  = 40 * time.Second
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
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
			writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
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
		writeErr(w, http.StatusBadRequest, "query is required", search.CodeBadRequest, nil, "")
		return
	}
	requested, err := search.NormalizeEngine(engine)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), search.CodeEngine, nil, "")
		return
	}
	limit = search.ClampLimit(limit)

	hints := search.RouteHints{Query: q, Region: region, Locale: locale, HL: hl}
	useFallback := search.ShouldFallback(requested, fallbackSet, fallback)
	snap := s.configSnap()
	chain := search.ScheduleWith(requested, useFallback, hints, snap)
	if len(chain) == 0 {
		writeErr(w, http.StatusBadRequest, "no engines scheduled", search.CodeEngine, nil, "")
		return
	}

	keyIn := cache.KeyInput{
		Query:     q,
		Engine:    requested,
		Limit:     limit,
		Content:   wantContent,
		Region:    region,
		Locale:    locale,
		HL:        hl,
		Fallback:  useFallback,
		ConfigSig: snap.CacheSig(),
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
				OK:              true,
				Query:           hit.Query,
				Engine:          hit.Engine,
				RequestedEngine: hit.RequestedEngine,
				Tried:           hit.Tried,
				Skipped:         hit.Skipped,
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

	// Identical in-flight searches share one Chrome trip. Cache hits above
	// stay fully concurrent. Inner work uses a detached timeout so one
	// cancelled client does not abort waiters.
	v, _, shared := s.flight.Do(cache.Key(keyIn), func() (interface{}, error) {
		return s.executeLiveSearch(q, requested, limit, wantContent, useFallback, chain, keyIn), nil
	})
	if shared {
		log.Printf("search singleflight=shared query=%q engine=%s", shortQuery(q), requested)
	}
	out := v.(liveSearchOut)
	if out.errStatus != 0 {
		log.Printf("search query=%q requested=%s tried=%v count=0 took_ms=%d error=%s code=%s", shortQuery(q), requested, out.tried, time.Since(start).Milliseconds(), out.errMsg, out.errCode)
		writeErr(w, out.errStatus, out.errMsg, out.errCode, out.tried, out.errEngine)
		return
	}
	body := out.body
	body.TookMs = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, body)
}

type liveSearchOut struct {
	body      successBody
	errStatus int
	errMsg    string
	errCode   string
	errEngine string
	tried     []string
}

func (s *Server) configSnap() search.ConfigSnapshot {
	if s != nil && s.cfg != nil {
		return s.cfg.Snapshot()
	}
	return search.ConfigSnapshot{}
}

func (s *Server) googleBreaker() *search.GoogleBreaker {
	if s != nil && s.breaker != nil {
		return s.breaker
	}
	return search.DefaultGoogleBreaker()
}

func (s *Server) hedgeDelay() time.Duration {
	if s != nil && s.hedgeAfter > 0 {
		return s.hedgeAfter
	}
	return defaultHedgeDelay
}

func (s *Server) executeLiveSearch(q, requested string, limit int, wantContent, useFallback bool, chain []string, keyIn cache.KeyInput) liveSearchOut {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	start := time.Now()

	br := s.googleBreaker()
	plan := br.Apply(requested, chain)
	if len(plan.Skipped) > 0 {
		log.Printf("google breaker=skip requested=%s skipped=%v chain=%v", requested, plan.Skipped, plan.Engines)
	}

	if plan.FailFast {
		tried := plan.Engines
		if len(tried) == 0 {
			tried = []string{"google"}
		}
		return liveSearchOut{
			errStatus: http.StatusForbidden,
			errMsg:    clientMessage(search.CodeCaptcha, ""),
			errCode:   search.CodeCaptcha,
			errEngine: "google",
			tried:     tried,
		}
	}
	if len(plan.Engines) == 0 {
		return liveSearchOut{
			errStatus: http.StatusBadRequest,
			errMsg:    "no engines scheduled",
			errCode:   search.CodeEngine,
			tried:     nil,
		}
	}

	httpChain, chromeChain := search.PartitionHTTPChrome(plan.Engines, requested, useFallback)
	if len(httpChain) > 0 {
		log.Printf("search phase=http requested=%s engines=%v", requested, httpChain)
	}
	if len(chromeChain) > 0 {
		log.Printf("search phase=chrome requested=%s engines=%v", requested, chromeChain)
	}

	var (
		wonEngine string
		results   []search.Result
		tried     []string
		lastErr   error
	)

	if len(httpChain) > 0 {
		if err := s.acquireHTTP(ctx); err != nil {
			code, msg := classifyLiveErr(err)
			return liveSearchOut{errStatus: liveErrStatus(code), errMsg: msg, errCode: code, errEngine: "", tried: nil}
		}
		func() {
			defer s.releaseHTTP()
			httpCtx := contextWithHTTPHeld(ctx)
			wonEngine, results, tried, lastErr = runSequentialChain(httpCtx, httpChain, func(tryCtx context.Context, eng string) ([]search.Result, error) {
				return s.runOneEngineMode(tryCtx, eng, q, limit, transportHTTP)
			})
		}()
	}

	if wonEngine == "" && len(chromeChain) > 0 {
		if s.admit != nil && s.admit.tooLittleTime(ctx) {
			log.Printf("search phase=chrome skip remain_below_min tried=%v", tried)
			if lastErr == nil {
				lastErr = search.NewError(search.CodeTimeout, "search timed out")
			}
		} else {
			var cTried []string
			wonEngine, results, cTried, lastErr = runHedgedChain(ctx, chromeChain, s.hedgeDelay(), func(tryCtx context.Context, eng string) ([]search.Result, error) {
				return s.runOneEngineMode(tryCtx, eng, q, limit, transportChrome)
			})
			tried = append(tried, cTried...)
		}
	}

	if wonEngine == "" {
		code, msg := classifyLiveErr(lastErr)
		eng := ""
		if len(tried) > 0 {
			eng = tried[len(tried)-1]
		}
		return liveSearchOut{errStatus: liveErrStatus(code), errMsg: msg, errCode: code, errEngine: eng, tried: tried}
	}

	// Chrome slot already released. Filter/score (and optional HTTP fetch)
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

	if s.cache != nil && len(results) > 0 {
		s.cache.Put(keyIn, cache.Payload{
			Query:           q,
			Engine:          wonEngine,
			RequestedEngine: requested,
			Tried:           tried,
			Skipped:         plan.Skipped,
			Results:         results,
			Count:           len(results),
		})
	}

	log.Printf("search query=%q engine=%s requested=%s tried=%v count=%d took_ms=%d content=%v error=", shortQuery(q), wonEngine, requested, tried, len(results), took, wantContent)
	return liveSearchOut{body: successBody{
		OK:              true,
		Query:           q,
		Engine:          wonEngine,
		RequestedEngine: requested,
		Tried:           tried,
		Skipped:         plan.Skipped,
		Results:         results,
		Count:           len(results),
		TookMs:          took,
	}}
}

func (s *Server) acquireChrome(ctx context.Context, eng string) error {
	if s == nil || s.admit == nil {
		return nil
	}
	if !search.NeedsChrome(eng) {
		return nil
	}
	start := time.Now()
	waiting := s.admit.Waiting()
	inflight := s.admit.InFlight()
	if waiting > 0 || !s.admit.HasFree() {
		log.Printf("search admit=wait engine=%s waiting=%d inflight=%d cap=%d", eng, waiting, inflight, s.admit.Cap())
	}
	err := s.admit.Acquire(ctx)
	if err != nil {
		log.Printf("search admit=%s engine=%s wait_ms=%d waiting=%d inflight=%d", search.CodeOf(err), eng, time.Since(start).Milliseconds(), s.admit.Waiting(), s.admit.InFlight())
		return err
	}
	if time.Since(start) > 50*time.Millisecond {
		log.Printf("search admit=go engine=%s wait_ms=%d inflight=%d", eng, time.Since(start).Milliseconds(), s.admit.InFlight())
	}
	return nil
}

func (s *Server) releaseChrome(eng string) {
	if s == nil || s.admit == nil || !search.NeedsChrome(eng) {
		return
	}
	s.admit.Release()
}

func (s *Server) acquireHTTP(ctx context.Context) error {
	if s == nil || s.httpAdmit == nil {
		return nil
	}
	if httpHeld(ctx) {
		return nil
	}
	start := time.Now()
	waiting := s.httpAdmit.Waiting()
	inflight := s.httpAdmit.InFlight()
	if waiting > 0 || !s.httpAdmit.HasFree() {
		log.Printf("search http-admit=wait waiting=%d inflight=%d cap=%d", waiting, inflight, s.httpAdmit.Cap())
	}
	err := s.httpAdmit.Acquire(ctx)
	if err != nil {
		log.Printf("search http-admit=%s wait_ms=%d waiting=%d inflight=%d", search.CodeOf(err), time.Since(start).Milliseconds(), s.httpAdmit.Waiting(), s.httpAdmit.InFlight())
		return err
	}
	if time.Since(start) > 50*time.Millisecond {
		log.Printf("search http-admit=go wait_ms=%d inflight=%d", time.Since(start).Milliseconds(), s.httpAdmit.InFlight())
	}
	return nil
}

func (s *Server) releaseHTTP() {
	if s == nil || s.httpAdmit == nil {
		return
	}
	s.httpAdmit.Release()
}

func (s *Server) runOneEngine(ctx context.Context, eng, q string, limit int) ([]search.Result, error) {
	return s.runOneEngineMode(ctx, eng, q, limit, transportAuto)
}

func (s *Server) runOneEngineMode(ctx context.Context, eng, q string, limit int, tr engineTransport) ([]search.Result, error) {
	var results []search.Result
	var err error

	tryHTTP := tr == transportHTTP || (tr == transportAuto && search.SupportsHTTP(eng))
	tryChrome := tr == transportChrome || (tr == transportAuto && search.NeedsChrome(eng))
	if tr == transportHTTP {
		tryChrome = false
	}
	if tr == transportChrome {
		tryHTTP = false
	}

	if tryHTTP {
		results, err = s.runHTTPAttempt(ctx, eng, q, limit)
		if err == nil && len(results) > 0 {
			s.googleBreaker().Observe(eng, nil)
			log.Printf("search attempt engine=%s ok count=%d via=http", eng, len(results))
			return results, nil
		}
		if !tryChrome {
			if err == nil {
				err = search.NewError(search.CodeParse, "no organic results parsed from "+eng)
			}
			s.googleBreaker().Observe(eng, err)
			log.Printf("search attempt engine=%s failed code=%s err=%v via=http", eng, search.CodeOf(err), err)
			return nil, err
		}
		log.Printf("search attempt engine=%s http-miss code=%s; chrome fallback", eng, search.CodeOf(err))
	}

	if tryChrome {
		results, err = s.runChromeAttempt(ctx, eng, q, limit)
	} else if err == nil {
		err = search.NewError(search.CodeEngine, eng+" is HTTP-only; use RunHTTP")
	}

	if errors.Is(err, browser.ErrNoGoogleInstance) {
		err = search.NewError(search.CodeCaptcha, "all chrome instances quarantined from google")
	}
	if err == nil && len(results) == 0 {
		err = search.NewError(search.CodeParse, "no organic results parsed from "+eng)
	}
	s.googleBreaker().Observe(eng, err)
	if err != nil {
		log.Printf("search attempt engine=%s failed code=%s err=%v", eng, search.CodeOf(err), err)
		return nil, err
	}
	log.Printf("search attempt engine=%s ok count=%d", eng, len(results))
	return results, nil
}

func (s *Server) runHTTPAttempt(ctx context.Context, eng, q string, limit int) ([]search.Result, error) {
	if !httpHeld(ctx) {
		if err := s.acquireHTTP(ctx); err != nil {
			return nil, err
		}
		defer s.releaseHTTP()
	}
	tryCtx, cancel := context.WithTimeout(ctx, search.HTTPTryTimeout())
	defer cancel()
	if s.runEngine != nil {
		return s.runEngine(tryCtx, eng, q, limit)
	}
	return search.RunHTTP(tryCtx, eng, q, limit)
}

func (s *Server) runChromeAttempt(ctx context.Context, eng, q string, limit int) ([]search.Result, error) {
	if s.runEngine != nil {
		if err := s.acquireChrome(ctx, eng); err != nil {
			return nil, err
		}
		defer s.releaseChrome(eng)
		tryCtx, cancel := context.WithTimeout(ctx, perTryTimeout)
		defer cancel()
		return s.runEngine(tryCtx, eng, q, limit)
	}
	if s.mgr == nil {
		return nil, search.NewError(search.CodeOffline, "chrome not configured")
	}
	if err := s.acquireChrome(ctx, eng); err != nil {
		return nil, err
	}
	defer s.releaseChrome(eng)
	tryCtx, cancel := context.WithTimeout(ctx, perTryTimeout)
	defer cancel()
	var results []search.Result
	err := s.mgr.Do(tryCtx, eng, func(page *rod.Page) error {
		page = page.Context(tryCtx)
		var e error
		results, e = search.Run(page, eng, q, limit)
		if e != nil && search.Is(e, search.CodeCaptcha) {
			_ = s.mgr.Screenshot(page, "captcha-"+eng)
		}
		return e
	})
	return results, err
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

func writeErr(w http.ResponseWriter, status int, msg, code string, tried []string, engine string) {
	writeJSON(w, status, errBody{OK: false, Error: msg, Code: code, Engine: engine, Tried: tried})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
