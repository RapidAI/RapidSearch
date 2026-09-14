package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"search-service/internal/proxyauth"
	"search-service/internal/tunnel"
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

func TestProxyPapersSkipsBearerSoLoginCanRender(t *testing.T) {
	h := testProxy(t)
	for _, path := range []string{"/papers", "/papers/", "/papers/api", "/papers/pdf/x.pdf"} {
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

func TestSessionEmitsAndAnswersPing(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	s := newSession(server, bufio.NewReader(server))
	s.startKeepaliveCfg(tunnel.KeepaliveConfig{
		Interval: 30 * time.Millisecond,
		PongWait: 400 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		s.readLoop()
		close(done)
	}()

	var mu sync.Mutex
	sawPong := false
	sawPing := false
	got := make(chan struct{}, 4)
	go func() {
		for {
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			var f tunnel.Frame
			if err := tunnel.ReadFrame(client, &f); err != nil {
				return
			}
			switch f.Type {
			case tunnel.TypePong:
				if f.ID == "peer" {
					mu.Lock()
					sawPong = true
					mu.Unlock()
					got <- struct{}{}
				}
			case tunnel.TypePing:
				_ = tunnel.WriteFrame(client, tunnel.Frame{Type: tunnel.TypePong, ID: f.ID})
				mu.Lock()
				first := !sawPing
				sawPing = true
				mu.Unlock()
				if first {
					got <- struct{}{}
				}
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy should emit its own ping")
	}
	if err := tunnel.WriteFrame(client, tunnel.Frame{Type: tunnel.TypePing, ID: "peer"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy should answer peer ping")
	}
	mu.Lock()
	okPing, okPong := sawPing, sawPong
	mu.Unlock()
	if !okPing {
		t.Fatal("proxy should emit its own ping")
	}
	if !okPong {
		t.Fatal("proxy should answer peer ping")
	}
	s.close(io.EOF)
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit")
	}
}
