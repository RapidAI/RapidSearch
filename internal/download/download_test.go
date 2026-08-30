package download

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	ok := []string{
		"https://example.com/",
		"http://example.com/a.bin",
		"https://example.com/path?x=1",
	}
	for _, u := range ok {
		if _, err := ValidateURL(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	bad := []string{
		"",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/plain,hi",
		"ftp://example.com/a",
		"/etc/passwd",
		"example.com/no-scheme",
	}
	for _, u := range bad {
		if _, err := ValidateURL(u); err == nil {
			t.Fatalf("accepted bad url %q", u)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	if g := safeFilename(`attachment; filename="report.pdf"`, "https://x/a"); g != "report.pdf" {
		t.Fatalf("cd: %q", g)
	}
	if g := safeFilename("", "https://cdn.example.com/files/foo.zip"); g != "foo.zip" {
		t.Fatalf("url path: %q", g)
	}
	if g := safeFilename("", "https://example.com/../../etc/passwd"); g != "passwd" {
		t.Fatalf("path skip: %q", g)
	}
	if g := safeFilename(`attachment; filename="../x"`, "https://x/"); g != "x" {
		t.Fatalf("cd path: %q", g)
	}
	if g := safeFilename("", "https://example.com/"); g != "download" {
		t.Fatalf("fallback: %q", g)
	}
}

func TestRetryable(t *testing.T) {
	if !retryableStatus(503) || !retryableStatus(429) || retryableStatus(404) || retryableStatus(200) {
		t.Fatal("status")
	}
	if retryableNet(errBadURL) {
		t.Fatal("bad url is not retryable")
	}
}

func TestHTTPDownloadExample(t *testing.T) {
	body := []byte("hello-download-body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Fatal("missing UA")
		}
		if r.Header.Get("Range") == "bytes=0-3" {
			w.Header().Set("Content-Range", "bytes 0-3/19")
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[:4])
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="hi.txt"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	d := New(nil, nil)
	d.hc.Timeout = 10 * time.Second

	req := httptest.NewRequest(http.MethodGet, "/download?url="+srv.URL+"/hi.txt", nil)
	rec := httptest.NewRecorder()
	d.Serve(req.Context(), rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("body %q", rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "hi.txt") {
		t.Fatalf("cd %q", rec.Header().Get("Content-Disposition"))
	}

	req2 := httptest.NewRequest(http.MethodGet, "/download?url="+srv.URL+"/hi.txt", nil)
	req2.Header.Set("Range", "bytes=0-3")
	rec2 := httptest.NewRecorder()
	d.Serve(req2.Context(), rec2, req2)
	if rec2.Code != http.StatusPartialContent {
		t.Fatalf("range status %d", rec2.Code)
	}
	if rec2.Body.String() != "hell" {
		t.Fatalf("range body %q", rec2.Body.String())
	}
}

func TestRejectFileURL(t *testing.T) {
	d := New(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/download?url=file:///etc/passwd", nil)
	rec := httptest.NewRecorder()
	d.Serve(req.Context(), rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bad_request") {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestRecentSeen(t *testing.T) {
	r := NewRecent(time.Hour)
	r.Remember([]string{"https://example.com/a", "https://other.test/b"})
	if !r.Has("https://example.com/a") || !r.Has("https://example.com/z") {
		t.Fatal("host should match")
	}
	if r.Has("https://nope.test/") {
		t.Fatal("unknown host")
	}
}
