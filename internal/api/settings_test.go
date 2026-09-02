package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"search-service/internal/search"
)

func settingsHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "search-config.json")
	t.Setenv("SEARCH_CONFIG_PATH", path)
	t.Setenv("SEARCH_TOKEN", "settings-secret")
	t.Setenv("HUB_AUTH_BASES", "http://127.0.0.1:1")
	h := New(nil, "", nil, nil)
	t.Cleanup(func() { search.ActivateStore(nil) })
	return h, path
}

func TestSettingsUnauthenticatedLoginHTML(t *testing.T) {
	h, _ := settingsHandler(t)
	for _, path := range []string{"/settings", "/settings/"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		ct := rr.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("%s ct=%s", path, ct)
		}
		body := rr.Body.String()
		if strings.Contains(body, "serper_api_key") || strings.Contains(body, "Serper API key") {
			t.Fatal("unauthenticated /settings served settings form")
		}
		if !strings.Contains(body, "Hub access / viewer token") {
			t.Fatal("login page missing token field")
		}
		if strings.Contains(body, "SEARCH_TOKEN") || strings.Contains(body, "proxy.token") {
			t.Fatal("login page mentioned operator token")
		}
	}
}

func TestSettingsConfigUnauthenticated401JSON(t *testing.T) {
	h, _ := settingsHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings/config", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("ct=%s", ct)
	}
	if strings.Contains(rr.Body.String(), "serper_api_key") {
		t.Fatal("401 leaked key field")
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeUnauthorized {
		t.Fatalf("%+v", body)
	}
}

func TestSettingsGETMasksKeys(t *testing.T) {
	h, _ := settingsHandler(t)
	put := httptest.NewRecorder()
	preq := httptest.NewRequest(http.MethodPut, "/settings/config", strings.NewReader(`{"serper_api_key":"abcdXYZQ"}`))
	preq.Header.Set("Authorization", "Bearer settings-secret")
	preq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(put, preq)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d %s", put.Code, put.Body.String())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings/config", nil)
	req.Header.Set("Authorization", "Bearer settings-secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "abcdXYZQ") {
		t.Fatal("GET leaked raw key")
	}
	var view search.PublicView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.OK || !view.Serper.Configured || view.Serper.Last4 != "XYZQ" {
		t.Fatalf("%+v", view.Serper)
	}
	if view.Brave.Configured {
		t.Fatal("brave should be empty")
	}
}

func TestSettingsEmptyPUTDoesNotWipe(t *testing.T) {
	h, path := settingsHandler(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/settings/config", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer settings-secret")
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := put(`{"serper_api_key":"keep-this-key-4242"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("set status=%d %s", rr.Code, rr.Body.String())
	}
	rr = put(`{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty status=%d %s", rr.Code, rr.Body.String())
	}
	rr = put(`{"serper_api_key":"","brave_api_key":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("blank status=%d %s", rr.Code, rr.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "keep-this-key-4242") {
		t.Fatalf("key wiped: %s", raw)
	}
	if strings.Contains(rr.Body.String(), "keep-this-key-4242") {
		t.Fatal("response leaked raw key")
	}
}

func TestSettingsPageHTMLAuthenticated(t *testing.T) {
	h, _ := settingsHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Authorization", "Bearer settings-secret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("ct=%s", ct)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "Serper") || !strings.Contains(string(body), "Brave") {
		t.Fatal("html missing key fields")
	}
}

func mockHub(t *testing.T) (*httptest.Server, http.Handler) {
	t.Helper()
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
				_, _ = w.Write([]byte(`{"access_token":"good-hub-token"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/llm/v1/models":
			if r.Header.Get("Authorization") == "Bearer good-hub-token" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(hub.Close)
	dir := t.TempDir()
	t.Setenv("SEARCH_CONFIG_PATH", filepath.Join(dir, "search-config.json"))
	t.Setenv("SEARCH_TOKEN", "settings-secret")
	t.Setenv("HUB_AUTH_BASES", hub.URL)
	h := New(nil, "", nil, nil)
	t.Cleanup(func() { search.ActivateStore(nil) })
	return hub, h
}

func TestSettingsLoginBad401(t *testing.T) {
	_, h := mockHub(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(`{"token":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ck := rr.Result().Cookies(); len(ck) != 0 {
		t.Fatalf("bad login set cookies: %+v", ck)
	}
}

func TestSettingsLoginSearchTokenRejected(t *testing.T) {
	_, h := mockHub(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(`{"token":"settings-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("SEARCH_TOKEN login status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func cookieFromLogin(t *testing.T, h http.Handler, body string) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ck *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "rs_settings" {
			ck = c
			break
		}
	}
	if ck == nil {
		t.Fatal("missing Set-Cookie rs_settings")
	}
	if !ck.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if ck.Path != "/" {
		t.Fatalf("cookie path=%q", ck.Path)
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("samesite=%v", ck.SameSite)
	}
	if ck.Secure {
		t.Fatal("Secure should be off without HTTPS")
	}
	if strings.Contains(rr.Header().Get("Set-Cookie"), "HttpOnly") == false {
		t.Fatal("Set-Cookie missing HttpOnly")
	}
	return ck
}

func TestSettingsLoginTokenSetCookieThenSettingsHTML(t *testing.T) {
	_, h := mockHub(t)
	ck := cookieFromLogin(t, h, `{"token":"good-hub-token"}`)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("ct=%s", rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), "Serper") || !strings.Contains(rr.Body.String(), "Brave") {
		t.Fatal("expected settings HTML after cookie login")
	}
}

func TestSettingsLoginPasswordSetCookie(t *testing.T) {
	_, h := mockHub(t)
	ck := cookieFromLogin(t, h, `{"username":"ada@hub.example","password":"correct-horse"}`)
	if ck.Value != "good-hub-token" {
		t.Fatalf("cookie value=%q", ck.Value)
	}
}

func TestSettingsLoginHTTPSSetsSecureCookie(t *testing.T) {
	_, h := mockHub(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(`{"token":"good-hub-token"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var ck *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "rs_settings" {
			ck = c
		}
	}
	if ck == nil || !ck.Secure {
		t.Fatalf("expected Secure cookie, got %+v", ck)
	}
}

func TestSettingsConfig401WithoutCookie(t *testing.T) {
	_, h := mockHub(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings/config", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("ct=%s", rr.Header().Get("Content-Type"))
	}
}

func TestSearchStill401WithoutBearerEvenWithSettingsCookie(t *testing.T) {
	_, h := mockHub(t)
	ck := cookieFromLogin(t, h, `{"token":"good-hub-token"}`)
	search.ActivateStore(nil)
	search.SetKeyedTestHooks("http://127.0.0.1:1", "", func(string) string { return "" })
	t.Cleanup(search.ResetKeyedTestHooks)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang&engine=serper&content=0", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeUnauthorized {
		t.Fatalf("%+v", body)
	}
}

func TestSettingsLogoutClearsCookie(t *testing.T) {
	_, h := mockHub(t)
	ck := cookieFromLogin(t, h, `{"token":"good-hub-token"}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/logout", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == "rs_settings" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear cookie")
	}
}

func TestHandleSearchSerperWithoutKeyUnauthorized(t *testing.T) {
	search.ActivateStore(nil)
	search.SetKeyedTestHooks("http://127.0.0.1:1", "", func(string) string { return "" })
	t.Cleanup(search.ResetKeyedTestHooks)
	s := &Server{mux: http.NewServeMux(), breaker: search.NewGoogleBreaker()}
	s.mux.HandleFunc("/search", s.handleSearch)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang&engine=serper&content=0", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeUnauthorized {
		t.Fatalf("%+v", body)
	}
}
