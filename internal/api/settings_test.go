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

func TestSettingsUnauthenticated401(t *testing.T) {
	h, _ := settingsHandler(t)
	for _, path := range []string{"/settings", "/settings/config"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "serper_api_key") {
			t.Fatal("401 leaked key field")
		}
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
