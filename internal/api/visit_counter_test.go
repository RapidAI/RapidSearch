package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func browserPapersReq(ip, ua, visitor string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/128.0.0.0"
	}
	req.Header.Set("User-Agent", ua)
	if ip != "" {
		req.Header.Set("X-Real-IP", ip)
		req.RemoteAddr = "127.0.0.1:9"
	}
	if visitor != "" {
		req.AddCookie(&http.Cookie{Name: visitorCookieName, Value: visitor})
	}
	return req
}

func visitorFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == visitorCookieName {
			if !validVisitorID(c.Value) {
				t.Fatalf("invalid visitor cookie %q", c.Value)
			}
			return c.Value
		}
	}
	t.Fatal("missing visitor cookie")
	return ""
}

func TestVisitCounterHitPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	vc := newVisitCounter(dir)
	if vc.Total() != 0 {
		t.Fatalf("start=%d", vc.Total())
	}
	rr := httptest.NewRecorder()
	if n := vc.Hit(rr, browserPapersReq("203.0.113.10", "", "")); n != 1 {
		t.Fatalf("first hit=%d", n)
	}
	id := visitorFrom(t, rr)
	rr2 := httptest.NewRecorder()
	if n := vc.Hit(rr2, browserPapersReq("203.0.113.10", "", id)); n != 1 {
		t.Fatalf("refresh same cookie=%d", n)
	}

	path := filepath.Join(dir, visitStatsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d visitStatsFileData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Total != 1 {
		t.Fatalf("file total=%d", d.Total)
	}
	if d.LastIP != "203.0.113.10" {
		t.Fatalf("last_ip=%q", d.LastIP)
	}
	if _, ok := d.Seen[id]; !ok {
		t.Fatalf("seen missing %s in %s", id, string(b))
	}

	vc2 := newVisitCounter(dir)
	if vc2.Total() != 1 {
		t.Fatalf("reload total=%d", vc2.Total())
	}
	rr3 := httptest.NewRecorder()
	if n := vc2.Hit(rr3, browserPapersReq("203.0.113.10", "", id)); n != 1 {
		t.Fatalf("reload same cookie=%d", n)
	}
}

func TestVisitCounterCookieNotIP(t *testing.T) {
	vc := newVisitCounter(t.TempDir())
	const sharedIP = "198.51.100.20"

	rrA := httptest.NewRecorder()
	if n := vc.Hit(rrA, browserPapersReq(sharedIP, "Mozilla/5.0 Chrome/120", "")); n != 1 {
		t.Fatalf("browser A=%d", n)
	}
	idA := visitorFrom(t, rrA)

	rrB := httptest.NewRecorder()
	reqB := browserPapersReq(sharedIP, "Mozilla/5.0 Firefox/121", "")
	reqB.Header.Set("X-Forwarded-For", "203.0.113.1, "+sharedIP)
	if n := vc.Hit(rrB, reqB); n != 2 {
		t.Fatalf("browser B same IP must count separately, got %d", n)
	}
	idB := visitorFrom(t, rrB)
	if idA == idB {
		t.Fatal("distinct browsers must receive distinct visitor cookies")
	}

	rrA2 := httptest.NewRecorder()
	if n := vc.Hit(rrA2, browserPapersReq(sharedIP, "Mozilla/5.0 Chrome/120", idA)); n != 2 {
		t.Fatalf("A refresh should not increment, got %d", n)
	}
}

func TestVisitCounterDedupeWindowExpires(t *testing.T) {
	vc := newVisitCounter(t.TempDir())
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	vc.now = func() time.Time { return now }

	rr := httptest.NewRecorder()
	if n := vc.Hit(rr, browserPapersReq("203.0.113.8", "", "")); n != 1 {
		t.Fatalf("t0=%d", n)
	}
	id := visitorFrom(t, rr)

	now = now.Add(11 * time.Hour)
	if n := vc.Hit(httptest.NewRecorder(), browserPapersReq("203.0.113.8", "", id)); n != 1 {
		t.Fatalf("inside window=%d", n)
	}

	now = now.Add(2 * time.Hour) // 13h after first count
	if n := vc.Hit(httptest.NewRecorder(), browserPapersReq("203.0.113.8", "", id)); n != 2 {
		t.Fatalf("after window=%d", n)
	}
}

func TestVisitCounterSkipsBotsAndPrefetch(t *testing.T) {
	vc := newVisitCounter(t.TempDir())
	req := browserPapersReq("203.0.113.5", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "")
	if n := vc.Hit(httptest.NewRecorder(), req); n != 0 {
		t.Fatalf("googlebot=%d", n)
	}
	curl := httptest.NewRequest(http.MethodGet, "/papers", nil)
	curl.Header.Set("User-Agent", "curl/8.7.1")
	curl.Header.Set("X-Real-IP", "203.0.113.5")
	if n := vc.Hit(httptest.NewRecorder(), curl); n != 0 {
		t.Fatalf("curl=%d", n)
	}
	empty := httptest.NewRequest(http.MethodGet, "/papers", nil)
	empty.Header.Set("X-Real-IP", "203.0.113.5")
	if n := vc.Hit(httptest.NewRecorder(), empty); n != 0 {
		t.Fatalf("empty ua=%d", n)
	}
	prefetch := browserPapersReq("203.0.113.5", "", "")
	prefetch.Header.Set("Purpose", "prefetch")
	if n := vc.Hit(httptest.NewRecorder(), prefetch); n != 0 {
		t.Fatalf("prefetch=%d", n)
	}
	if vc.Total() != 0 {
		t.Fatalf("total=%d", vc.Total())
	}
}

func TestVisitCounterLoadsLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"total": 42, "updated_at": "2026-09-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, visitStatsFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	vc := newVisitCounter(dir)
	if vc.Total() != 42 {
		t.Fatalf("legacy total=%d", vc.Total())
	}
}

func TestRequestClientIPForwardedHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		real   string
		xff    string
		remote string
		want   string
	}{
		{
			name:   "x-real-ip wins over spoofed leftmost xff",
			real:   "203.0.113.9",
			xff:    "1.2.3.4, 203.0.113.9",
			remote: "127.0.0.1:18780",
			want:   "203.0.113.9",
		},
		{
			name:   "rightmost xff when no x-real-ip",
			xff:    "8.8.8.8, 198.51.100.7",
			remote: "10.0.0.1:9",
			want:   "198.51.100.7",
		},
		{
			name:   "remote addr host when no forwarded headers",
			remote: "192.0.2.10:54321",
			want:   "192.0.2.10",
		},
		{
			name:   "ipv6 x-real-ip",
			real:   "2001:db8::1",
			remote: "[::1]:9",
			want:   "2001:db8::1",
		},
		{
			name:   "invalid x-real-ip falls through to xff",
			real:   "not-an-ip",
			xff:    "203.0.113.11",
			remote: "127.0.0.1:1",
			want:   "203.0.113.11",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/papers", nil)
			req.RemoteAddr = tc.remote
			if tc.real != "" {
				req.Header.Set("X-Real-IP", tc.real)
			}
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := requestClientIP(req); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestVisitCounterRecordsForwardedIPNotAsKey(t *testing.T) {
	vc := newVisitCounter(t.TempDir())
	hit := func(visitor, xff string) (int64, string) {
		req := browserPapersReq("", "Mozilla/5.0 Safari/17", visitor)
		req.Header.Del("X-Real-IP")
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = "127.0.0.1:18780"
		rr := httptest.NewRecorder()
		n := vc.Hit(rr, req)
		id := visitor
		if id == "" {
			id = visitorFrom(t, rr)
		}
		return n, id
	}
	n1, a := hit("", "1.1.1.1, 203.0.113.40")
	n2, b := hit("", "1.1.1.1, 203.0.113.40")
	if n1 != 1 || n2 != 2 {
		t.Fatalf("same XFF two new cookies: %d then %d", n1, n2)
	}
	if a == b {
		t.Fatal("expected distinct visitor ids")
	}
	if vc.lastIP != "203.0.113.40" {
		t.Fatalf("recorded ip=%q (should be rightmost XFF, not spoofed 1.1.1.1)", vc.lastIP)
	}
	n3, _ := hit(a, "1.1.1.1, 203.0.113.40")
	if n3 != 2 {
		t.Fatalf("same cookie + same XFF refresh=%d", n3)
	}
}

func TestVisitorCookieSecureBehindProxy(t *testing.T) {
	vc := newVisitCounter(t.TempDir())
	req := browserPapersReq("203.0.113.1", "", "")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	vc.Hit(rr, req)
	var ck *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == visitorCookieName {
			ck = c
		}
	}
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie=%+v", ck)
	}
}

func TestIsBotVisit(t *testing.T) {
	t.Parallel()
	bot := httptest.NewRequest(http.MethodGet, "/", nil)
	bot.Header.Set("User-Agent", "python-requests/2.32.0")
	if !isBotVisit(bot) {
		t.Fatal("python-requests")
	}
	human := httptest.NewRequest(http.MethodGet, "/", nil)
	human.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	if isBotVisit(human) {
		t.Fatal("browser flagged")
	}
}

func TestPapersPageVisitUsesVisitorCookie(t *testing.T) {
	h, _ := papersHandler(t)
	get := func(cookie, ip, ua string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/papers", nil)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-Real-IP", ip)
		req.RemoteAddr = "10.0.0.2:443"
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: visitorCookieName, Value: cookie})
		}
		h.ServeHTTP(rr, req)
		return rr
	}
	apiVisits := func() int64 {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("api status=%d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("catalog api cache=%q", rr.Header().Get("Cache-Control"))
		}
		var cat papersCatalog
		if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
			t.Fatal(err)
		}
		return cat.Visits
	}

	rr1 := get("", "203.0.113.77", "Mozilla/5.0 Chrome/128")
	if rr1.Code != http.StatusOK {
		t.Fatalf("html status=%d", rr1.Code)
	}
	id1 := visitorFrom(t, rr1)
	if apiVisits() != 1 {
		t.Fatalf("after first browser visits=%d", apiVisits())
	}

	rr2 := get(id1, "203.0.113.77", "Mozilla/5.0 Chrome/128")
	if rr2.Code != http.StatusOK {
		t.Fatalf("refresh status=%d", rr2.Code)
	}
	if apiVisits() != 1 {
		t.Fatalf("refresh visits=%d", apiVisits())
	}

	rr3 := get("", "203.0.113.77", "Mozilla/5.0 Firefox/130")
	id3 := visitorFrom(t, rr3)
	if id3 == id1 {
		t.Fatal("second browser reused first visitor cookie")
	}
	if apiVisits() != 2 {
		t.Fatalf("second browser visits=%d", apiVisits())
	}

	head := httptest.NewRecorder()
	hreq := httptest.NewRequest(http.MethodHead, "/papers", nil)
	hreq.Header.Set("User-Agent", "Mozilla/5.0 Chrome/128")
	hreq.Header.Set("X-Real-IP", "203.0.113.77")
	h.ServeHTTP(head, hreq)
	if apiVisits() != 2 {
		t.Fatalf("HEAD must not count, visits=%d", apiVisits())
	}
	if apiVisits() != 2 {
		t.Fatal("api poll counted")
	}
}
