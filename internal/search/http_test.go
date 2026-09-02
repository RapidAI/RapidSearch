package search

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunHTTPDuckDuckGoNoChrome(t *testing.T) {
	body, err := os.ReadFile("testdata/duckduckgo_html.html")
	if err != nil {
		t.Fatal(err)
	}
	orig := getSearchHTML
	t.Cleanup(func() { getSearchHTML = orig })
	var sawURL string
	getSearchHTML = func(ctx context.Context, rawURL string) (string, int, error) {
		sawURL = rawURL
		if ctx.Err() != nil {
			t.Fatal("ctx already done")
		}
		if !strings.Contains(rawURL, "duckduckgo.com/html") && !strings.Contains(rawURL, "html.duckduckgo.com") {
			t.Fatalf("unexpected url %s", rawURL)
		}
		return string(body), 200, nil
	}
	hits, err := RunHTTP(context.Background(), "duckduckgo_html", "golang http server", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits=%+v", hits)
	}
	if sawURL == "" {
		t.Fatal("HTTP getter was not called")
	}
	for _, h := range hits {
		if strings.Contains(h.URL, "ads.example.com") {
			t.Fatalf("ad in RunHTTP: %+v", h)
		}
	}
}

func TestParseHTTPTryTimeout(t *testing.T) {
	if parseHTTPTryTimeout("") != defaultHTTPTryTimeout {
		t.Fatal("default")
	}
	if parseHTTPTryTimeout("4s") != 4*time.Second {
		t.Fatal("duration")
	}
	if parseHTTPTryTimeout("6") != 6*time.Second {
		t.Fatal("seconds")
	}
	if parseHTTPTryTimeout("500ms") != minHTTPTryTimeout {
		t.Fatal("clamp min")
	}
	if parseHTTPTryTimeout("60s") != maxHTTPTryTimeout {
		t.Fatal("clamp max")
	}
}

func TestRunHTTPUnsupportedEngine(t *testing.T) {
	_, err := RunHTTP(context.Background(), "google", "golang", 5)
	if err == nil {
		t.Fatal("google should not have an HTTP SERP path")
	}
}

func TestRunHTTPBaiduNoChrome(t *testing.T) {
	body, err := os.ReadFile("testdata/baidu.html")
	if err != nil {
		t.Fatal(err)
	}
	orig := getSearchHTML
	t.Cleanup(func() { getSearchHTML = orig })
	var sawURL string
	getSearchHTML = func(ctx context.Context, rawURL string) (string, int, error) {
		sawURL = rawURL
		if !strings.Contains(rawURL, "baidu.com/s") {
			t.Fatalf("unexpected url %s", rawURL)
		}
		return string(body), 200, nil
	}
	hits, err := RunHTTP(context.Background(), "baidu", "golang http server", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits=%+v", hits)
	}
	if sawURL == "" {
		t.Fatal("HTTP getter was not called")
	}
	for _, h := range hits {
		if strings.Contains(h.URL, "ad.example.com") {
			t.Fatalf("ad in RunHTTP: %+v", h)
		}
	}
}

func TestRunHTTPBingNoChrome(t *testing.T) {
	body, err := os.ReadFile("testdata/bing.html")
	if err != nil {
		t.Fatal(err)
	}
	orig := getSearchHTML
	t.Cleanup(func() { getSearchHTML = orig })
	getSearchHTML = func(ctx context.Context, rawURL string) (string, int, error) {
		if !strings.Contains(rawURL, "bing.com/search") {
			t.Fatalf("unexpected url %s", rawURL)
		}
		return string(body), 200, nil
	}
	hits, err := RunHTTP(context.Background(), "bing", "golang http server", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits=%+v", hits)
	}
	for _, h := range hits {
		if strings.Contains(h.URL, "ads.example.com") {
			t.Fatalf("ad in RunHTTP: %+v", h)
		}
	}
}

func TestHTTPLooksBlockedBaiduOrganic(t *testing.T) {
	body, err := os.ReadFile("testdata/baidu.html")
	if err != nil {
		t.Fatal(err)
	}
	if httpLooksBlocked("baidu", string(body)) {
		t.Fatal("organic baidu fixture must not look blocked")
	}
	if !httpLooksBlocked("baidu", `<html><title>安全验证</title><body>wappass captcha verify</body></html>`) {
		t.Fatal("baidu wappass should look blocked")
	}
	if httpLooksBlocked("bing", `<ol id="b_results"><li class="b_algo">ok</li></ol>`) {
		t.Fatal("organic bing must not look blocked")
	}
	if !httpLooksBlocked("bing", `<html>please verify you are a human</html>`) {
		t.Fatal("bing captcha should look blocked")
	}
}
