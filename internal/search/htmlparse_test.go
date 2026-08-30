package search

import (
	"os"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseDuckDuckGoHTMLFixture(t *testing.T) {
	hits := parseDuckDuckGoHTML(readTestdata(t, "duckduckgo_html.html"), 10)
	if len(hits) < 2 {
		t.Fatalf("want >=2 organic, got %+v", hits)
	}
	var urls []string
	for _, h := range hits {
		lu := strings.ToLower(h.URL)
		if strings.Contains(lu, "ads.example.com") || strings.Contains(strings.ToLower(h.Title), "buy go hosting") {
			t.Fatalf("ad kept: %+v", h)
		}
		if h.Title == "" || h.URL == "" {
			t.Fatalf("empty hit: %+v", h)
		}
		urls = append(urls, h.URL)
	}
	joined := strings.Join(urls, " ")
	if !strings.Contains(joined, "pkg.go.dev/net/http") {
		t.Fatalf("missing pkg.go.dev: %v", urls)
	}
	if !strings.Contains(joined, "go.dev/doc") {
		t.Fatalf("uddg unwrap failed: %v", urls)
	}
	if strings.Contains(joined, "duckduckgo.com/l/") {
		t.Fatalf("left DDG redirect: %v", urls)
	}
}

func TestParseSogouHTMLDropsAds(t *testing.T) {
	hits := parseSogouHTML(readTestdata(t, "sogou.html"), 10)
	if len(hits) < 2 {
		t.Fatalf("want organic sogou hits, got %+v", hits)
	}
	for _, h := range hits {
		if strings.Contains(h.URL, "shop.example.com") || strings.Contains(h.Title, "促销") {
			t.Fatalf("sogou ad kept: %+v", h)
		}
		if strings.Contains(h.URL, "sogou.com/web") {
			t.Fatalf("SERP self url kept: %+v", h)
		}
		if strings.Contains(h.Title, "广告") && strings.HasPrefix(strings.TrimSpace(h.Title), "广告") {
			t.Fatalf("ad title kept: %+v", h)
		}
	}
}

func TestParse360HTMLDropsAds(t *testing.T) {
	hits := parse360HTML(readTestdata(t, "so360.html"), 10)
	if len(hits) < 2 {
		t.Fatalf("want organic 360 hits, got %+v", hits)
	}
	var sawSide bool
	for _, h := range hits {
		if strings.Contains(h.URL, "ad.example.com") || strings.Contains(h.Title, "买云服务器") || strings.Contains(h.Title, "侧栏推广") {
			t.Fatalf("360 ad kept: %+v", h)
		}
		if strings.Contains(h.URL, "go.dev/learn") {
			sawSide = true
		}
	}
	if !sawSide {
		t.Fatalf("expected #side organic, got %+v", hits)
	}
}

func TestNeedsChromeHTTPOnly(t *testing.T) {
	if NeedsChrome("duckduckgo_html") {
		t.Fatal("duckduckgo_html must not need Chrome")
	}
	if !NeedsChrome("google") || !NeedsChrome("duckduckgo") || !NeedsChrome("sogou") || !NeedsChrome("360") {
		t.Fatal("chrome engines should need chrome")
	}
	if !SupportsHTTP("duckduckgo_html") || !SupportsHTTP("sogou") || !SupportsHTTP("360") {
		t.Fatal("expected HTTP-capable engines")
	}
	if SupportsHTTP("google") || SupportsHTTP("bing") {
		t.Fatal("google/bing are chrome SERPs")
	}
}
