package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"search-service/internal/cache"
	"search-service/internal/search"
)

func TestAutoHTTPBeforeAnyChrome(t *testing.T) {
	var mu sync.Mutex
	var saw []string
	var chrome atomic.Int32
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(2, 0, 0),
		httpAdmit:  newTestAdmit(8, 0, 0),
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			mu.Lock()
			saw = append(saw, engine)
			mu.Unlock()
			if search.NeedsChrome(engine) && !search.SupportsHTTP(engine) {
				chrome.Add(1)
			}
			if engine == "duckduckgo_html" {
				return organicHit(), nil
			}
			return nil, search.NewError(search.CodeParse, "http miss "+engine)
		},
	}
	chain := search.Schedule("auto", true, search.RouteHints{Query: "北京天气"})
	out := s.executeLiveSearch("北京天气", "auto", 5, false, true, chain, cache.KeyInput{})
	if out.errStatus != 0 {
		t.Fatalf("err %d %s %s", out.errStatus, out.errCode, out.errMsg)
	}
	if !out.body.OK || out.body.Count < 1 || out.body.Engine != "duckduckgo_html" {
		t.Fatalf("body=%+v", out.body)
	}
	mu.Lock()
	got := append([]string(nil), saw...)
	mu.Unlock()
	want := []string{"baidu", "sogou", "360", "bing", "duckduckgo_html"}
	if len(got) != len(want) {
		t.Fatalf("tried=%v want %v", got, want)
	}
	for i, e := range want {
		if got[i] != e {
			t.Fatalf("order %v want %v", got, want)
		}
	}
	if chrome.Load() != 0 {
		t.Fatalf("chrome started before HTTP chain finished: %v", got)
	}
	if s.admit.InFlight() != 0 {
		t.Fatalf("HTTP path took a chrome slot: inflight=%d", s.admit.InFlight())
	}
}

func TestBingHTTPFailTriesDuckDuckGoHTMLBeforeBingChrome(t *testing.T) {
	var mu sync.Mutex
	var saw []string
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(2, 0, 0),
		httpAdmit:  newTestAdmit(8, 0, 0),
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			mu.Lock()
			saw = append(saw, engine)
			mu.Unlock()
			if engine == "google" || engine == "duckduckgo" {
				t.Errorf("chrome %s must not run before HTTP is exhausted", engine)
			}
			if engine == "duckduckgo_html" {
				return organicHit(), nil
			}
			return nil, search.NewError(search.CodeParse, "http miss "+engine)
		},
	}
	// Bing first so an HTTP miss must continue through later HTTP engines
	// (sogou/360/ddg_html) instead of bing Chrome.
	chain := []string{"bing", "sogou", "360", "duckduckgo_html", "duckduckgo"}
	out := s.executeLiveSearch("golang http server", "auto", 5, false, true, chain, cache.KeyInput{})
	if out.errStatus != 0 || !out.body.OK {
		t.Fatalf("err %d %s body=%+v", out.errStatus, out.errCode, out.body)
	}
	mu.Lock()
	got := append([]string(nil), saw...)
	mu.Unlock()
	want := []string{"bing", "sogou", "360", "duckduckgo_html"}
	if len(got) != len(want) {
		t.Fatalf("tried=%v want %v", got, want)
	}
	for i, e := range want {
		if got[i] != e {
			t.Fatalf("order %v want %v", got, want)
		}
	}
	if out.body.Engine != "duckduckgo_html" {
		t.Fatalf("engine=%s tried=%v", out.body.Engine, got)
	}
}

func TestHTTPCapDoesNotTakeChromeSlots(t *testing.T) {
	chrome := newTestAdmit(1, 0, 0)
	httpAd := newTestAdmit(2, 0, 0)
	if err := chrome.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var httpPeak, chromeRuns atomic.Int32
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      chrome,
		httpAdmit:  httpAd,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if search.NeedsChrome(engine) && !search.SupportsHTTP(engine) {
				chromeRuns.Add(1)
			}
			n := int32(httpAd.InFlight())
			for {
				old := httpPeak.Load()
				if n <= old || httpPeak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return organicHit(), nil
		},
	}
	var wg sync.WaitGroup
	errs := make(chan liveSearchOut, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf("golang http server %d", i)
			errs <- s.executeLiveSearch(q, "duckduckgo_html", 5, false, false, []string{"duckduckgo_html"}, cache.KeyInput{})
		}(i)
	}
	wg.Wait()
	close(errs)
	for out := range errs {
		if out.errStatus != 0 || !out.body.OK || out.body.Count < 1 {
			t.Fatalf("out=%+v", out)
		}
	}
	if chrome.InFlight() != 1 {
		t.Fatalf("HTTP stole chrome slot: inflight=%d", chrome.InFlight())
	}
	if chromeRuns.Load() != 0 {
		t.Fatal("HTTP searches started chrome")
	}
	if httpPeak.Load() > 2 {
		t.Fatalf("http overlap %d exceeds cap 2", httpPeak.Load())
	}
	if httpAd.InFlight() != 0 {
		t.Fatalf("http leaked %d", httpAd.InFlight())
	}
	chrome.Release()
}

func TestHTTPOverflowTimeoutExplicit(t *testing.T) {
	httpAd := newTestAdmit(1, 0, 0)
	if err := httpAd.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer httpAd.Release()
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(1, 0, 0),
		httpAdmit:  httpAd,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			t.Fatal("HTTP run should not start")
			return nil, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.runOneEngineMode(ctx, "duckduckgo_html", "golang", 5, transportHTTP)
	if err == nil || !search.Is(err, search.CodeTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}

	rr := httptest.NewRecorder()
	writeErr(rr, liveErrStatus(search.CodeTimeout), clientMessage(search.CodeTimeout, ""), search.CodeTimeout, nil, "")
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d", rr.Code)
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeTimeout {
		t.Fatalf("body=%+v", body)
	}
}

func TestHTTPOverflowBusyExplicit(t *testing.T) {
	httpAd := newTestAdmit(1, 1, 0)
	if err := httpAd.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	go func() {
		close(waiting)
		_ = httpAd.Acquire(context.Background())
	}()
	<-waiting
	deadline := time.Now().Add(time.Second)
	for httpAd.Waiting() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	s := &Server{httpAdmit: httpAd, breaker: search.NewGoogleBreaker()}
	err := s.acquireHTTP(context.Background())
	if err == nil || !search.Is(err, search.CodeBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	rr := httptest.NewRecorder()
	writeErr(rr, liveErrStatus(search.CodeBusy), clientMessage(search.CodeBusy, ""), search.CodeBusy, nil, "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rr.Code)
	}
	httpAd.Release()
}

func TestHundredWayHTTPSucceedsWithoutChrome(t *testing.T) {
	chrome := newTestAdmit(1, 0, 0)
	if err := chrome.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer chrome.Release()
	httpAd := newTestAdmit(10, 0, 0)
	var chromeRuns atomic.Int32
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      chrome,
		httpAdmit:  httpAd,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if search.IsChromeOnly(engine) {
				chromeRuns.Add(1)
				return nil, search.NewError(search.CodeTimeout, "chrome should be rare")
			}
			time.Sleep(15 * time.Millisecond)
			return organicHit(), nil
		},
	}
	const n = 100
	outs := make(chan liveSearchOut, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf("unique query %d golang http server", i)
			req := "auto"
			chain := search.Schedule("auto", true, search.RouteHints{Query: q})
			if i%2 == 0 {
				q = fmt.Sprintf("北京天气 %d", i)
				chain = search.Schedule("auto", true, search.RouteHints{Query: q})
			}
			outs <- s.executeLiveSearch(q, req, 5, false, true, chain, cache.KeyInput{})
		}(i)
	}
	wg.Wait()
	close(outs)
	ok := 0
	for out := range outs {
		if out.errStatus != 0 || !out.body.OK || out.body.Count < 1 || len(out.body.Results) == 0 {
			t.Fatalf("fail status=%d code=%s msg=%s ok=%v count=%d", out.errStatus, out.errCode, out.errMsg, out.body.OK, out.body.Count)
		}
		ok++
	}
	if ok != n {
		t.Fatalf("ok=%d want %d", ok, n)
	}
	if chromeRuns.Load() != 0 {
		t.Fatalf("chrome ran %d times", chromeRuns.Load())
	}
	if chrome.InFlight() != 1 {
		t.Fatalf("chrome slot disturbed: %d", chrome.InFlight())
	}
	if httpAd.InFlight() != 0 {
		t.Fatalf("http leaked %d", httpAd.InFlight())
	}
}

func TestHandleSearchAutoHTTPSuccessOKTrue(t *testing.T) {
	s := &Server{
		mux:        http.NewServeMux(),
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(1, 0, 0),
		httpAdmit:  newTestAdmit(8, 0, 0),
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if search.IsChromeOnly(engine) {
				t.Errorf("chrome %s", engine)
			}
			return organicHit(), nil
		},
	}
	s.mux.HandleFunc("/search", s.handleSearch)
	if err := s.admit.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang+http+server&n=5&content=0", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body successBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Count < 1 || len(body.Results) == 0 {
		t.Fatalf("body=%+v", body)
	}
	if s.admit.InFlight() != 1 {
		t.Fatalf("auto HTTP consumed chrome slot: inflight=%d", s.admit.InFlight())
	}
	s.admit.Release()
}

func TestChromeOnlyAfterHTTPExhaustedAndTimeRemains(t *testing.T) {
	var mu sync.Mutex
	var saw []string
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(1, 0, 0),
		httpAdmit:  newTestAdmit(8, 0, 0),
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			mu.Lock()
			saw = append(saw, engine)
			mu.Unlock()
			if engine == "google" {
				t.Errorf("google on auto")
			}
			if engine == "duckduckgo" {
				return organicHit(), nil
			}
			return nil, search.NewError(search.CodeParse, "http miss "+engine)
		},
	}
	chain := search.Schedule("auto", true, search.RouteHints{Query: "golang http server"})
	out := s.executeLiveSearch("golang http server", "auto", 5, false, true, chain, cache.KeyInput{})
	if out.errStatus != 0 || !out.body.OK || out.body.Engine != "duckduckgo" {
		t.Fatalf("status=%d code=%s engine=%s", out.errStatus, out.errCode, out.body.Engine)
	}
	mu.Lock()
	got := append([]string(nil), saw...)
	mu.Unlock()
	if len(got) == 0 || got[len(got)-1] != "duckduckgo" {
		t.Fatalf("chrome ddg should be last: %v", got)
	}
	for i, e := range got[:len(got)-1] {
		if search.IsChromeOnly(e) {
			t.Fatalf("chrome %s before HTTP exhausted at %d: %v", e, i, got)
		}
		if !search.SupportsHTTP(e) {
			t.Fatalf("non-http before chrome: %v", got)
		}
	}
}
