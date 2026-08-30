package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"search-service/internal/browser"
	"search-service/internal/cache"
	"search-service/internal/search"
)

func TestExecuteLiveSearchBreakerSkipsGoogle(t *testing.T) {
	br := search.NewGoogleBreaker()
	br.Trip()
	var mu sync.Mutex
	var saw []string
	s := &Server{
		breaker:    br,
		hedgeAfter: time.Millisecond,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			mu.Lock()
			saw = append(saw, engine)
			mu.Unlock()
			if engine == "google" {
				t.Errorf("google should have been skipped")
			}
			return []search.Result{{Title: "Go docs", URL: "https://go.dev/doc", Snippet: "golang http server docs"}}, nil
		},
	}
	chain := search.Schedule("auto", true, search.RouteHints{Query: "golang http server"})
	out := s.executeLiveSearch("golang http server", "auto", 5, false, true, chain, cache.KeyInput{})
	if out.errStatus != 0 {
		t.Fatalf("err %d %s %s", out.errStatus, out.errCode, out.errMsg)
	}
	if out.body.Engine == "google" {
		t.Fatal("google won despite breaker")
	}
	if len(out.body.Skipped) != 1 || out.body.Skipped[0] != "google" {
		t.Fatalf("skipped=%v", out.body.Skipped)
	}
	if !out.body.OK {
		t.Fatal("ok")
	}
	mu.Lock()
	got := append([]string(nil), saw...)
	mu.Unlock()
	for _, e := range got {
		if e == "google" {
			t.Fatalf("google ran: %v", got)
		}
	}
}

func TestExecuteLiveSearchExplicitGoogleFailFast(t *testing.T) {
	br := search.NewGoogleBreaker()
	br.Trip()
	s := &Server{
		breaker: br,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			t.Fatal("explicit google while breaker open must not wait on chrome")
			return nil, nil
		},
	}
	out := s.executeLiveSearch("golang", "google", 5, false, false, []string{"google"}, cache.KeyInput{})
	if out.errStatus != 403 || out.errCode != search.CodeCaptcha {
		t.Fatalf("status=%d code=%s msg=%s", out.errStatus, out.errCode, out.errMsg)
	}
	if out.errEngine != "google" {
		t.Fatalf("engine=%s", out.errEngine)
	}
}

func TestExecuteLiveSearchErrNoGoogleInstanceImmediateFailover(t *testing.T) {
	br := search.NewGoogleBreaker()
	s := &Server{
		breaker:    br,
		hedgeAfter: 3 * time.Second,
		runEngine: func(ctx context.Context, engine, query string, limit int) ([]search.Result, error) {
			if engine == "google" {
				return nil, browser.ErrNoGoogleInstance
			}
			return []search.Result{{Title: "Go docs", URL: "https://go.dev/doc", Snippet: "golang http server"}}, nil
		},
	}
	start := time.Now()
	chain := []string{"google", "bing"}
	out := s.executeLiveSearch("golang http server", "auto", 5, false, true, chain, cache.KeyInput{})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("ate try timeout: %s", time.Since(start))
	}
	if out.errStatus != 0 {
		t.Fatalf("err %d %s %s", out.errStatus, out.errCode, out.errMsg)
	}
	if out.body.Engine != "bing" {
		t.Fatalf("engine=%s", out.body.Engine)
	}
	if !br.Open() {
		t.Fatal("ErrNoGoogleInstance should trip google breaker")
	}
}
