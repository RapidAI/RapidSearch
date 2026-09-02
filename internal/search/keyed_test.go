package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunHTTPSerperParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "stub-serper-key" {
			t.Errorf("missing key header")
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"organic": []map[string]interface{}{
				{"title": "Go docs", "link": "https://go.dev/doc", "snippet": "The Go programming language", "position": 1},
				{"title": "Effective Go", "link": "https://go.dev/doc/effective_go", "snippet": "tips", "position": 2},
			},
		})
	}))
	defer srv.Close()
	SetKeyedTestHooks(srv.URL, "", func(engine string) string {
		if engine == "serper" {
			return "stub-serper-key"
		}
		return ""
	})
	t.Cleanup(ResetKeyedTestHooks)

	hits, err := RunHTTP(context.Background(), "serper", "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].URL != "https://go.dev/doc" || hits[0].Title != "Go docs" {
		t.Fatalf("%+v", hits)
	}
	if hits[0].Rank != 1 || hits[1].Rank != 2 {
		t.Fatalf("ranks %+v", hits)
	}
}

func TestRunHTTPBraveParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "stub-brave-key" {
			t.Errorf("missing token header")
		}
		if !strings.Contains(r.URL.RawQuery, "q=golang") {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"web": map[string]interface{}{
				"results": []map[string]interface{}{
					{"title": "Brave Go", "url": "https://example.com/go", "description": "golang hits"},
				},
			},
		})
	}))
	defer srv.Close()
	SetKeyedTestHooks("", srv.URL, func(engine string) string {
		if engine == "brave" {
			return "stub-brave-key"
		}
		return ""
	})
	t.Cleanup(ResetKeyedTestHooks)

	hits, err := RunHTTP(context.Background(), "brave", "golang", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/go" || hits[0].Title != "Brave Go" {
		t.Fatalf("%+v", hits)
	}
}

func TestRunHTTPSerperWithoutKeyUnauthorized(t *testing.T) {
	SetKeyedTestHooks("http://127.0.0.1:1", "", func(string) string { return "" })
	t.Cleanup(ResetKeyedTestHooks)
	_, err := RunHTTP(context.Background(), "serper", "golang", 5)
	if err == nil || !Is(err, CodeUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestKeyedHTTPErrorNotEmpty200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid API Key"}`))
	}))
	defer srv.Close()
	SetKeyedTestHooks(srv.URL, "", func(string) string { return "bad" })
	t.Cleanup(ResetKeyedTestHooks)
	_, err := RunHTTP(context.Background(), "serper", "golang", 5)
	if err == nil || !Is(err, CodeUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestNormalizeEngineSerperBrave(t *testing.T) {
	for _, name := range []string{"serper", "brave"} {
		got, err := NormalizeEngine(name)
		if err != nil || got != name {
			t.Fatalf("%s: %q %v", name, got, err)
		}
		if !SupportsHTTP(name) || NeedsChrome(name) || IsChromeOnly(name) {
			t.Fatalf("%s transport flags", name)
		}
	}
}
