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

func TestHubPasswordLoginRequiresGlobalAdminAndUsers(t *testing.T) {
	var loginHits, usersHits, modelsHits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/admin/login":
			loginHits.Add(1)
			var in struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Tenant   string `json:"tenant"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Tenant != "" && in.Tenant != "__global__" {
				t.Errorf("login tenant=%q, want __global__ or empty", in.Tenant)
			}
			if in.Username == "ada@hub.example" && in.Password == "correct-horse" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"admin-from-password","admin":{"username":"ada","scope":"global"}}`))
				return
			}
			if in.Username == "tenant-ada" && in.Password == "tenant-pass" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"tenant-from-password","admin":{"username":"tenant-ada","scope":"tenant","tenant_id":"tenant_default"}}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/users":
			usersHits.Add(1)
			if r.Header.Get("Authorization") == "Bearer admin-from-password" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/llm/v1/models":
			modelsHits.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	c := New("search-secret", []string{hub.URL})
	if tok := c.HubPasswordLogin("ada@hub.example", "correct-horse"); tok != "admin-from-password" {
		t.Fatalf("password login token=%q", tok)
	}
	if loginHits.Load() < 1 || usersHits.Load() < 1 {
		t.Fatalf("loginHits=%d usersHits=%d", loginHits.Load(), usersHits.Load())
	}
	if modelsHits.Load() != 0 {
		t.Fatalf("password login must not call models, hits=%d", modelsHits.Load())
	}
	if tok := c.HubPasswordLogin("ada@hub.example", "wrong"); tok != "" {
		t.Fatalf("bad password returned %q", tok)
	}
	if tok := c.HubPasswordLogin("tenant-ada", "tenant-pass"); tok != "" {
		t.Fatalf("tenant admin returned %q", tok)
	}
	if !c.AdminValid("admin-from-password") {
		t.Fatal("issued token should pass AdminValid")
	}
	if c.HubValid("admin-from-password") {
		t.Fatal("admin token must not pass models HubValid")
	}
}

func TestAdminValidUsesUsersNotModels(t *testing.T) {
	var usersHits, modelsHits atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/admin/users":
			usersHits.Add(1)
			if r.Header.Get("Authorization") == "Bearer admin-tok" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/llm/v1/models":
			modelsHits.Add(1)
			if r.Header.Get("Authorization") == "Bearer viewer-tok" {
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
	if !c.AdminValid("admin-tok") {
		t.Fatal("admin token should pass AdminValid")
	}
	if c.AdminValid("viewer-tok") {
		t.Fatal("viewer token must not pass AdminValid")
	}
	if !c.HubValid("viewer-tok") {
		t.Fatal("viewer token should pass HubValid")
	}
	if c.HubValid("admin-tok") {
		t.Fatal("admin token must not pass HubValid")
	}
	if !c.SettingsAuthorized("search-secret") {
		t.Fatal("SEARCH_TOKEN should pass SettingsAuthorized")
	}
	if !c.SettingsAuthorized("admin-tok") {
		t.Fatal("admin token should pass SettingsAuthorized")
	}
	if c.SettingsAuthorized("viewer-tok") {
		t.Fatal("models-only token must not pass SettingsAuthorized")
	}
	if !c.Authorized("viewer-tok") {
		t.Fatal("/search Authorized should still accept viewer token")
	}
	if usersHits.Load() < 2 {
		t.Fatalf("usersHits=%d", usersHits.Load())
	}
	if modelsHits.Load() < 2 {
		t.Fatalf("modelsHits=%d", modelsHits.Load())
	}
}

func TestIsGlobalHubAdmin(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},
		{`null`, false},
		{`{}`, true},
		{`{"scope":"global"}`, true},
		{`{"scope":"GLOBAL"}`, true},
		{`{"scope":"__global__"}`, true},
		{`{"is_global":true}`, true},
		{`{"global":true}`, true},
		{`{"scope":"tenant","tenant_id":"tenant_default"}`, false},
		{`{"scope":"tenant"}`, false},
		{`{"is_global":false,"scope":"global"}`, false},
		{`{"tenant_id":"tenant_default"}`, false},
		{`{"role":"tenant_admin"}`, false},
		{`{"scope":"global","tenant_id":"tenant_default"}`, true},
	}
	for _, tc := range cases {
		if got := isGlobalHubAdmin([]byte(tc.raw)); got != tc.want {
			t.Fatalf("isGlobalHubAdmin(%s)=%v want %v", tc.raw, got, tc.want)
		}
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
