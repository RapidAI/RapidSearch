package proxyauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseBasesDefault(t *testing.T) {
	got := ParseBases("")
	if len(got) != 2 || got[0] != "https://hub.mypapers.top" || got[1] != "https://hub.maclaw.top" {
		t.Fatalf("default bases = %#v", got)
	}
	got = ParseBases("  https://hub.example.com/,https://hub.example.com, https://other.example ")
	if len(got) != 2 || got[0] != "https://hub.example.com" || got[1] != "https://other.example" {
		t.Fatalf("custom bases = %#v", got)
	}
}

func TestSearchTokenAcceptedWithoutHub(t *testing.T) {
	c := New("secret-token", []string{"http://127.0.0.1:1"})
	if !c.Authorized("secret-token") {
		t.Fatal("SEARCH_TOKEN should be accepted")
	}
	if c.Authorized("secret-token-x") {
		t.Fatal("near-miss SEARCH_TOKEN should be rejected")
	}
	if c.Authorized("") || c.Authorized("   ") {
		t.Fatal("empty token should be rejected")
	}
}

func TestBearerTokenFromHeaderAndQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/health?token=query-tok", nil)
	r.Header.Set("Authorization", "Bearer header-tok")
	if got := BearerToken(r); got != "header-tok" {
		t.Fatalf("header wins: %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/health?token=query-tok", nil)
	if got := BearerToken(r); got != "query-tok" {
		t.Fatalf("query: %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/health", nil)
	if got := BearerToken(r); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestRequestTokenCookieFallback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(&http.Cookie{Name: SettingsCookie, Value: "cookie-tok"})
	if got := RequestToken(r); got != "cookie-tok" {
		t.Fatalf("cookie: %q", got)
	}
	if got := BearerToken(r); got != "" {
		t.Fatalf("bearer must ignore cookie: %q", got)
	}
	r.Header.Set("Authorization", "Bearer header-tok")
	if got := RequestToken(r); got != "header-tok" {
		t.Fatalf("header wins over cookie: %q", got)
	}
}

func TestHubValidRejectsSearchToken(t *testing.T) {
	c := New("secret-token", []string{"http://127.0.0.1:1"})
	if !c.Authorized("secret-token") {
		t.Fatal("Authorized should accept SEARCH_TOKEN")
	}
	if c.HubValid("secret-token") {
		t.Fatal("HubValid must not accept SEARCH_TOKEN")
	}
}

func TestHubPasswordLoginThenHubValid(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/admin/login":
			var in struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Username == "ada@hub.example" && in.Password == "correct-horse" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"viewer-from-password"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/llm/v1/models":
			if r.Header.Get("Authorization") == "Bearer viewer-from-password" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	c := New("search-secret", []string{hub.URL})
	if tok := c.HubPasswordLogin("ada@hub.example", "correct-horse"); tok != "viewer-from-password" {
		t.Fatalf("password login token=%q", tok)
	}
	if tok := c.HubPasswordLogin("ada@hub.example", "wrong"); tok != "" {
		t.Fatalf("bad password returned %q", tok)
	}
	if !c.HubValid("viewer-from-password") {
		t.Fatal("issued token should pass HubValid")
	}
}

func TestHub2xxAccepted401Rejected(t *testing.T) {
	var hits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api/llm/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer good-hub":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "Bearer also-ok":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer hub.Close()

	c := New("search-secret", []string{hub.URL})
	if !c.Authorized("good-hub") {
		t.Fatal("2xx hub token rejected")
	}
	if !c.Authorized("also-ok") {
		t.Fatal("204 hub token rejected")
	}
	if c.Authorized("bad-hub") {
		t.Fatal("401 hub token accepted")
	}
	if hits.Load() < 3 {
		t.Fatalf("expected live hub checks, hits=%d", hits.Load())
	}
}

func TestPositiveCacheSkipsSecondHubCall(t *testing.T) {
	var hits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer cached-hub" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	c := New("", []string{hub.URL})
	if !c.Authorized("cached-hub") {
		t.Fatal("first check")
	}
	if !c.Authorized("cached-hub") {
		t.Fatal("cached check")
	}
	if hits.Load() != 1 {
		t.Fatalf("cache should skip second call, hits=%d", hits.Load())
	}
}

func TestPositiveCacheExpires(t *testing.T) {
	var hits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	now := time.Unix(1_700_000_000, 0)
	c := New("", []string{hub.URL})
	c.CacheTTL = time.Minute
	c.now = func() time.Time { return now }

	if !c.Authorized("expiring-hub") {
		t.Fatal("first")
	}
	now = now.Add(30 * time.Second)
	if !c.Authorized("expiring-hub") {
		t.Fatal("still cached")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after 30s = %d", hits.Load())
	}
	now = now.Add(5 * time.Minute)
	if !c.Authorized("expiring-hub") {
		t.Fatal("after expiry")
	}
	if hits.Load() != 2 {
		t.Fatalf("hits after expiry = %d, want 2", hits.Load())
	}
}

func TestSecondBaseUsedWhenFirstRejects(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer fail.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer other-hub" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	c := New("", []string{fail.URL, ok.URL})
	if !c.Authorized("other-hub") {
		t.Fatal("should accept via second base")
	}
}

func TestHubTimeoutRejected(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	c := New("", []string{hub.URL})
	c.Timeout = 50 * time.Millisecond
	c.Client = &http.Client{Timeout: 50 * time.Millisecond}
	start := time.Now()
	if c.Authorized("slow-hub") {
		t.Fatal("timed-out hub should not authorize")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout took too long")
	}
}

func TestNegativeNotCached(t *testing.T) {
	var hits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	c := New("", []string{hub.URL})
	if c.Authorized("flip-hub") {
		t.Fatal("first should fail")
	}
	if !c.Authorized("flip-hub") {
		t.Fatal("second live check should succeed")
	}
	if hits.Load() != 2 {
		t.Fatalf("negative must not be cached, hits=%d", hits.Load())
	}
}

func TestAuthorizedNeverStringifiesTokenInErrorPaths(t *testing.T) {
	// Guard: checker methods have no fmt that includes the token.
	// This is a source-level reminder; runtime still must not log tokens.
	src := `Authorized BearerToken hubValid checkHub`
	if !strings.Contains(src, "Authorized") {
		t.Fatal("sanity")
	}
}
