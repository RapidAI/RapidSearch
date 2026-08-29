package search

import (
	"context"
	"strings"
	"testing"
)

func TestTokenizeEnglishAndCJK(t *testing.T) {
	toks := Tokenize("golang http server")
	got := strings.Join(toks, ",")
	if !containsAll(toks, "golang", "http", "server") {
		t.Fatalf("english tokens=%s", got)
	}
	if containsAll(toks, "the") {
		t.Fatalf("stopword leaked: %s", got)
	}

	cjk := Tokenize("Go语言 教程")
	if !containsAll(cjk, "语言") && !containsAll(cjk, "教程") {
		t.Fatalf("cjk tokens=%v", cjk)
	}
	// 2-grams of 教程 (2 chars) include 教程 itself via the run plus bigram
	if !containsAll(Tokenize("你好世界"), "你好", "好世", "世界") {
		t.Fatalf("cjk 2-grams missing: %v", Tokenize("你好世界"))
	}
}

func TestPreprocessFiltersOffTopic(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "net/http - Go", URL: "https://pkg.go.dev/net/http", Snippet: "Package http provides HTTP client and server implementations."},
		{Rank: 2, Title: "Buy cheap shoes", URL: "https://example.com/shoes", Snippet: "Running sneakers on sale this week."},
		{Rank: 3, Title: "Writing HTTP servers in Go", URL: "https://go.dev/doc/articles/wiki/", Snippet: "Building a wiki with the net/http server."},
		{Rank: 4, Title: "javascript:void", URL: "javascript:alert(1)", Snippet: "golang http server"},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query:        "golang http server",
		Engine:       "bing",
		Limit:        10,
		FetchContent: false,
	})
	if len(out) < 2 {
		t.Fatalf("kept too few: %+v", out)
	}
	for _, r := range out {
		if strings.Contains(strings.ToLower(r.Title), "shoes") {
			t.Fatalf("off-topic kept: %+v", r)
		}
		if strings.HasPrefix(strings.ToLower(r.URL), "javascript:") {
			t.Fatalf("javascript url kept: %+v", r)
		}
		if r.Relevance <= 0 {
			t.Fatalf("expected positive relevance: %+v", r)
		}
		if r.Content != "" {
			t.Fatalf("content should be omitted when fetch=false: %+v", r)
		}
	}
	if out[0].Rank != 1 {
		t.Fatalf("ranks not reassigned: %+v", out)
	}
	// stronger title/snippet should rank first-ish
	if out[0].Relevance < out[len(out)-1].Relevance {
		t.Fatalf("not sorted by relevance: %+v", out)
	}
	// "Go" in title/snippet should count toward query token "golang"
	var wiki *Result
	for i := range out {
		if strings.Contains(out[i].URL, "go.dev") || strings.Contains(out[i].Title, "Writing HTTP") {
			wiki = &out[i]
			break
		}
	}
	if wiki == nil || wiki.Relevance < 0.4 {
		t.Fatalf("golang/go alias should boost Go http-server hits, got %+v", out)
	}
}

func TestPreprocessFallbackKeepsCleaned(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "Unrelated weather", URL: "https://weather.example/today", Snippet: "Rain in shanghai tonight."},
		{Rank: 2, Title: "Sports scores", URL: "https://sports.example/nba", Snippet: "Lakers win in overtime."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query:        "golang http server",
		Engine:       "bing",
		Limit:        10,
		FetchContent: false,
	})
	if len(out) != 2 {
		t.Fatalf("fallback should keep cleaned originals, got %d: %+v", len(out), out)
	}
}

func TestCleanDropsEmptyAndInternal(t *testing.T) {
	in := []Result{
		{Title: "", URL: "https://example.com", Snippet: "x"},
		{Title: "Bing search", URL: "https://www.bing.com/search?q=foo", Snippet: "internal"},
		{Title: "OK", URL: "https://example.com/a", Snippet: "golang http"},
	}
	out := cleanResults(in, "bing")
	if len(out) != 1 || out[0].Title != "OK" {
		t.Fatalf("clean=%+v", out)
	}
}

func containsAll(toks []string, want ...string) bool {
	set := map[string]bool{}
	for _, t := range toks {
		set[t] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
