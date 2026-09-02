package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"search-service/internal/proxyauth"
)

func testProxy(t *testing.T) *hub {
	t.Helper()
	return newHub("proxy-secret", proxyauth.New("proxy-secret", []string{"http://127.0.0.1:1"}))
}

func TestProxySearch401WithoutBearer(t *testing.T) {
	h := testProxy(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang", nil)
	h.serveHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body proxyErr
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != "unauthorized" {
		t.Fatalf("%+v", body)
	}
}

func TestProxySearch401IgnoresSettingsCookie(t *testing.T) {
	h := testProxy(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=golang", nil)
	req.AddCookie(&http.Cookie{Name: proxyauth.SettingsCookie, Value: "proxy-secret"})
	h.serveHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie must not authorize /search, status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestProxySettingsSkipsBearerSoLoginCanRender(t *testing.T) {
	h := testProxy(t)
	for _, path := range []string{"/settings", "/settings/", "/settings/login"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.serveHTTP(rr, req)
		if rr.Code == http.StatusUnauthorized {
			t.Fatalf("%s should not 401 at the proxy (want forward/offline), body=%s", path, rr.Body.String())
		}
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d body=%s (no tunnel → 503)", path, rr.Code, rr.Body.String())
		}
	}
}
