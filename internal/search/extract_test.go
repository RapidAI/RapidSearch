package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLMainTextStripsChrome(t *testing.T) {
	raw := `<html><head><script>var x="golang http server junk"</script><style>p{color:red}</style></head>
<body>
<nav>Home About</nav>
<footer>copyright</footer>
<article>
<p>Package http provides HTTP client and server implementations in Go.</p>
<p>ListenAndServe starts an HTTP server on a given address.</p>
</article>
</body></html>`
	text := htmlMainText(raw)
	if strings.Contains(strings.ToLower(text), "var x") {
		t.Fatalf("script leaked: %q", text)
	}
	if !strings.Contains(text, "ListenAndServe") {
		t.Fatalf("missing article text: %q", text)
	}
	ex := relevantExcerpt(text, Tokenize("golang http server"), 1200)
	if !strings.Contains(ex, "HTTP") && !strings.Contains(strings.ToLower(ex), "server") {
		t.Fatalf("excerpt not relevant: %q", ex)
	}
}

func TestFetchRelevantContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pdf" {
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.4 junk golang http server"))
			return
		}
		if r.URL.Path == "/gone" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><main><p>The net/http package implements an HTTP server in Go.</p>
<p>Use http.ListenAndServe to start serving.</p>
<p>Unrelated cooking recipe for pasta.</p></main></body></html>`))
	}))
	defer srv.Close()

	toks := Tokenize("golang http server")
	got := fetchRelevantContent(context.Background(), srv.URL+"/", toks)
	if !strings.Contains(got, "ListenAndServe") && !strings.Contains(strings.ToLower(got), "http server") {
		t.Fatalf("content=%q", got)
	}
	if strings.Contains(strings.ToLower(got), "cooking") && !strings.Contains(strings.ToLower(got), "http") {
		t.Fatalf("kept off-topic only: %q", got)
	}
	if fetchRelevantContent(context.Background(), srv.URL+"/pdf", toks) != "" {
		t.Fatal("pdf should be skipped")
	}
	if fetchRelevantContent(context.Background(), srv.URL+"/gone", toks) != "" {
		t.Fatal("4xx should be skipped")
	}
}
