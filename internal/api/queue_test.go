package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"search-service/internal/cache"
	"search-service/internal/search"
)

func TestParseQueueEnv(t *testing.T) {
	if parseQueueMax("") != 0 || parseQueueMax("nope") != 0 || parseQueueMax("-1") != 0 {
		t.Fatal("empty/invalid queue max should be unlimited")
	}
	if parseQueueMax("8") != 8 {
		t.Fatal("queue max 8")
	}
	if parseChromeMinRemain("") != defaultChromeMinRemain {
		t.Fatal("default min remain")
	}
	if parseChromeMinRemain("8s") != 8*time.Second {
		t.Fatal("duration min remain")
	}
	if parseChromeMinRemain("12") != 12*time.Second {
		t.Fatal("integer seconds min remain")
	}
	if parseChromeMinRemain("0") != 0 {
		t.Fatal("zero min remain allowed")
	}
	if parseHTTPMax("") != defaultHTTPMax || parseHTTPMax("nope") != defaultHTTPMax {
		t.Fatal("default http max")
	}
	if parseHTTPMax("8") != 8 {
		t.Fatal("http max 8")
	}
	if parseHTTPMax("0") != 0 {
		t.Fatal("http max 0 is unlimited")
	}
	if parseHTTPMax("99") != maxHTTPMax {
		t.Fatal("http max clamp")
	}
}

func TestAdmitCapsInFlight(t *testing.T) {
	a := newTestAdmit(2, 0, 0)
	var cur, max int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := a.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := atomic.AddInt32(&cur, 1)
			for {
				old := atomic.LoadInt32(&max)
				if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			a.Release()
		}()
	}
	close(start)
	wg.Wait()
	if max > 2 {
		t.Fatalf("overlap %d exceeds cap 2", max)
	}
	if max < 2 {
		t.Fatalf("expected full cap, max=%d", max)
	}
	if a.InFlight() != 0 || a.Waiting() != 0 {
		t.Fatalf("leaked inflight=%d waiting=%d", a.InFlight(), a.Waiting())
	}
}

func TestAdmitBusyWhenQueueFull(t *testing.T) {
	a := newTestAdmit(1, 1, 0)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	go func() {
		close(waiting)
		_ = a.Acquire(context.Background())
	}()
	<-waiting
	deadline := time.Now().Add(time.Second)
	for a.Waiting() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if a.Waiting() < 1 {
		t.Fatal("expected a waiter")
	}
	err := a.Acquire(context.Background())
	if err == nil || !search.Is(err, search.CodeBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	a.Release()
}

func TestAdmitTimeoutWhenHeld(t *testing.T) {
	a := newTestAdmit(1, 0, 0)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := a.Acquire(ctx)
	if err == nil || !search.Is(err, search.CodeTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	a.Release()
}

func organicHit() []search.Result {
	return []search.Result{{Rank: 1, Title: "Go docs", URL: "https://go.dev/doc", Snippet: "golang http server docs"}}
}

func TestHTTPPathDoesNotConsumeChromeSlot(t *testing.T) {
	admit := newTestAdmit(1, 0, 0)
	if err := admit.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var chrome atomic.Int32
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      admit,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if search.NeedsChrome(engine) {
				chrome.Add(1)
			}
			if engine != "duckduckgo_html" {
				t.Errorf("unexpected engine %s", engine)
			}
			return organicHit(), nil
		},
	}
	start := time.Now()
	out := s.executeLiveSearch("golang http server", "duckduckgo_html", 5, false, false, []string{"duckduckgo_html"}, cache.KeyInput{})
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("HTTP path waited on chrome slot: %s", time.Since(start))
	}
	if out.errStatus != 0 {
		t.Fatalf("err %d %s %s", out.errStatus, out.errCode, out.errMsg)
	}
	if !out.body.OK || out.body.Count < 1 {
		t.Fatalf("ok=%v count=%d", out.body.OK, out.body.Count)
	}
	if chrome.Load() != 0 {
		t.Fatal("HTTP-only engine must not take a Chrome slot")
	}
	if admit.InFlight() != 1 {
		t.Fatalf("HTTP path released or stole the held slot: inflight=%d", admit.InFlight())
	}
	admit.Release()
}

func TestChromeOverflowWaitsThenSucceeds(t *testing.T) {
	admit := newTestAdmit(1, 0, 0)
	if err := admit.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      admit,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			return organicHit(), nil
		},
	}
	done := make(chan liveSearchOut, 1)
	go func() {
		done <- s.executeLiveSearch("golang", "google", 5, false, false, []string{"google"}, cache.KeyInput{})
	}()
	deadline := time.Now().Add(time.Second)
	for admit.Waiting() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if admit.Waiting() < 1 {
		t.Fatal("overflow should wait")
	}
	select {
	case out := <-done:
		t.Fatalf("finished before release: %+v", out)
	case <-time.After(40 * time.Millisecond):
	}
	admit.Release()
	select {
	case out := <-done:
		if out.errStatus != 0 {
			t.Fatalf("err %d %s %s", out.errStatus, out.errCode, out.errMsg)
		}
		if !out.body.OK || out.body.Count < 1 {
			t.Fatalf("ok=%v count=%d", out.body.OK, out.body.Count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued chrome work did not finish")
	}
}

func TestChromeOverflowTimeoutExplicit(t *testing.T) {
	admit := newTestAdmit(1, 0, 0)
	if err := admit.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer admit.Release()
	s := &Server{
		breaker:    search.NewGoogleBreaker(),
		admit:      admit,
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			t.Fatal("chrome run should not start")
			return nil, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.runOneEngine(ctx, "google", "golang", 5)
	if err == nil || !search.Is(err, search.CodeTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}

	rr := httptest.NewRecorder()
	writeErr(rr, liveErrStatus(search.CodeTimeout), clientMessage(search.CodeTimeout, ""), search.CodeTimeout, nil, "google")
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

func TestChromeBusyExplicitJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	writeErr(rr, liveErrStatus(search.CodeBusy), clientMessage(search.CodeBusy, ""), search.CodeBusy, nil, "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rr.Code)
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeBusy || body.Error == "" {
		t.Fatalf("body=%+v", body)
	}
}

func TestHandleSearchHTTPSuccessOKTrue(t *testing.T) {
	s := &Server{
		mux:        http.NewServeMux(),
		breaker:    search.NewGoogleBreaker(),
		admit:      newTestAdmit(1, 0, 0),
		hedgeAfter: time.Hour,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if engine != "duckduckgo_html" {
				return nil, search.NewError(search.CodeParse, "unexpected "+engine)
			}
			return organicHit(), nil
		},
	}
	s.mux.HandleFunc("/search", s.handleSearch)
	if err := s.admit.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang+http+server&engine=duckduckgo_html&n=5&content=0", nil)
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
		t.Fatalf("HTTP search consumed chrome slot: inflight=%d", s.admit.InFlight())
	}
	s.admit.Release()
}
