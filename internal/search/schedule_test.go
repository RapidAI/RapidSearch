package search

import (
	"reflect"
	"testing"
)

func TestIsChinaRoute(t *testing.T) {
	cases := []struct {
		h    RouteHints
		want bool
		name string
	}{
		{RouteHints{Query: "golang http 服务器"}, true, "han"},
		{RouteHints{Query: "北京天气"}, true, "city-han"},
		{RouteHints{Query: "golang http server"}, false, "english"},
		{RouteHints{Query: "wechat mini program"}, true, "wechat"},
		{RouteHints{Query: "ByteDance news"}, true, "bytedance"},
		{RouteHints{Query: "hello", Region: "cn"}, true, "region-cn"},
		{RouteHints{Query: "hello", Locale: "zh"}, true, "locale-zh"},
		{RouteHints{Query: "hello", HL: "zh-CN"}, true, "hl-zh-CN"},
		{RouteHints{Query: "hello", Region: "us"}, false, "region-us"},
		{RouteHints{Query: "shanghai weather"}, true, "shanghai"},
	}
	for _, tc := range cases {
		if got := IsChinaRoute(tc.h); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestScheduleAuto(t *testing.T) {
	cn := Schedule("auto", true, RouteHints{Query: "北京天气"})
	if !reflect.DeepEqual(cn, []string{"baidu", "sogou", "360", "bing", "duckduckgo_html", "duckduckgo"}) {
		t.Fatalf("china chain: %v", cn)
	}
	for _, e := range cn {
		if e == "google" {
			t.Fatalf("china chain must not include google: %v", cn)
		}
	}
	hasSogou, has360 := false, false
	for _, e := range cn {
		if e == "sogou" {
			hasSogou = true
		}
		if e == "360" {
			has360 = true
		}
	}
	if !hasSogou || !has360 {
		t.Fatalf("china chain missing sogou/360: %v", cn)
	}
	gl := Schedule("", true, RouteHints{Query: "golang http server"})
	if !reflect.DeepEqual(gl, []string{"duckduckgo_html", "bing", "sogou", "360", "baidu", "duckduckgo"}) {
		t.Fatalf("global chain: %v", gl)
	}
	for _, e := range gl {
		if e == "google" {
			t.Fatalf("auto global must omit google: %v", gl)
		}
	}
	idxHTML, idxChrome := -1, -1
	for i, e := range gl {
		if e == "duckduckgo_html" {
			idxHTML = i
		}
		if e == "duckduckgo" {
			idxChrome = i
		}
	}
	if idxHTML < 0 || idxChrome < 0 || idxHTML > idxChrome {
		t.Fatalf("global should prefer ddg html before chrome ddg: %v", gl)
	}
	if ContainsHan("golang") || !ContainsHan("语言") {
		t.Fatalf("ContainsHan")
	}
}

func TestScheduleExplicit(t *testing.T) {
	one := Schedule("baidu", false, RouteHints{Query: "Go语言"})
	if !reflect.DeepEqual(one, []string{"baidu"}) {
		t.Fatalf("explicit no fallback: %v", one)
	}
	fb := Schedule("google", true, RouteHints{Query: "golang"})
	if !reflect.DeepEqual(fb, []string{"google", "duckduckgo_html", "bing", "sogou", "360", "baidu", "duckduckgo"}) {
		t.Fatalf("explicit google fallback global: %v", fb)
	}
	fbCN := Schedule("bing", true, RouteHints{Query: "北京"})
	if fbCN[0] != "bing" {
		t.Fatalf("explicit first: %v", fbCN)
	}
	for _, e := range fbCN {
		if e == "google" {
			t.Fatalf("google should not join china fallback chain: %v", fbCN)
		}
	}
}

func TestShouldFallback(t *testing.T) {
	if !ShouldFallback("auto", false, false) {
		t.Fatal("auto defaults to fallback")
	}
	if ShouldFallback("google", false, false) {
		t.Fatal("explicit defaults to no fallback")
	}
	if ShouldFallback("auto", true, false) {
		t.Fatal("explicit fallback=0 disables auto failover")
	}
	if !ShouldFallback("baidu", true, true) {
		t.Fatal("fallback=1 enables explicit failover")
	}
}

func TestNormalizeEngineAutoAndBaidu(t *testing.T) {
	got, err := NormalizeEngine("")
	if err != nil || got != "auto" {
		t.Fatalf("empty -> auto, got %q err=%v", got, err)
	}
	got, err = NormalizeEngine("bd")
	if err != nil || got != "baidu" {
		t.Fatalf("bd -> baidu, got %q err=%v", got, err)
	}
	got, err = NormalizeEngine("BAIDU")
	if err != nil || got != "baidu" {
		t.Fatalf("BAIDU -> baidu, got %q err=%v", got, err)
	}
	if _, err := NormalizeEngine("yahoo"); err == nil {
		t.Fatal("yahoo should fail")
	}
}

func TestScheduleDuckDuckGoTransportSplit(t *testing.T) {
	one := Schedule("duckduckgo", false, RouteHints{Query: "golang"})
	if !reflect.DeepEqual(one, []string{"duckduckgo_html", "duckduckgo"}) {
		t.Fatalf("ddg split: %v", one)
	}
	htmlOnly := Schedule("duckduckgo_html", false, RouteHints{Query: "golang"})
	if !reflect.DeepEqual(htmlOnly, []string{"duckduckgo_html"}) {
		t.Fatalf("html only: %v", htmlOnly)
	}
	sogou := Schedule("sogou", false, RouteHints{Query: "北京"})
	if !reflect.DeepEqual(sogou, []string{"sogou"}) {
		t.Fatalf("sogou: %v", sogou)
	}
	so := Schedule("360", false, RouteHints{Query: "北京"})
	if !reflect.DeepEqual(so, []string{"360"}) {
		t.Fatalf("360: %v", so)
	}
}

func TestNormalizeEngineNewEngines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sogou", "sogou"},
		{"360", "360"},
		{"so360", "360"},
		{"duckduckgo_html", "duckduckgo_html"},
		{"ddg_html", "duckduckgo_html"},
		{"ddg", "duckduckgo"},
	}
	for _, tc := range cases {
		got, err := NormalizeEngine(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s -> %q err=%v want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestPartitionHTTPChromeAutoExhaustsHTTPFirst(t *testing.T) {
	cn := Schedule("auto", true, RouteHints{Query: "北京天气"})
	http, chrome := PartitionHTTPChrome(cn, "auto", true)
	if !reflect.DeepEqual(http, []string{"baidu", "sogou", "360", "bing", "duckduckgo_html"}) {
		t.Fatalf("china http: %v", http)
	}
	if !reflect.DeepEqual(chrome, []string{"duckduckgo"}) {
		t.Fatalf("china chrome: %v", chrome)
	}
	for _, e := range http {
		if IsChromeOnly(e) {
			t.Fatalf("http chain contains chrome-only %s", e)
		}
		if !SupportsHTTP(e) {
			t.Fatalf("http chain missing HTTP support: %s", e)
		}
	}
	if containsEngine(chrome, "baidu") || containsEngine(chrome, "bing") {
		t.Fatalf("auto must not chrome-fallback dual engines: %v", chrome)
	}
	if containsEngine(append(http, chrome...), "google") {
		t.Fatal("auto china must not include google")
	}

	gl := Schedule("auto", true, RouteHints{Query: "golang http server"})
	http, chrome = PartitionHTTPChrome(gl, "auto", true)
	if !reflect.DeepEqual(http, []string{"duckduckgo_html", "bing", "sogou", "360", "baidu"}) {
		t.Fatalf("global http: %v", http)
	}
	if !reflect.DeepEqual(chrome, []string{"duckduckgo"}) {
		t.Fatalf("global chrome: %v", chrome)
	}
	if containsEngine(http, "google") || containsEngine(chrome, "google") {
		t.Fatalf("auto global must omit google http=%v chrome=%v", http, chrome)
	}
	if idx := indexOf(http, "bing"); idx < 0 || indexOf(http, "duckduckgo_html") < 0 {
		t.Fatalf("bing http and ddg_html must both run before chrome: %v", http)
	}
	if indexOf(http, "bing") > indexOf(append(http, chrome...), "duckduckgo") && !containsEngine(http, "duckduckgo_html") {
		t.Fatal("ddg_html should be attempted before bing chrome")
	}
}

func TestPartitionHTTPChromeBingHTTPBeforeDuckDuckGoHTML(t *testing.T) {
	// After bing HTTP miss, later HTTP engines (ddg_html / 360 / sogou) must
	// run before any bing Chrome.
	chain := []string{"baidu", "sogou", "360", "bing", "duckduckgo_html", "duckduckgo"}
	http, chrome := PartitionHTTPChrome(chain, "auto", true)
	bing := indexOf(http, "bing")
	ddg := indexOf(http, "duckduckgo_html")
	sogou := indexOf(http, "sogou")
	so := indexOf(http, "360")
	if bing < 0 || ddg < 0 || sogou < 0 || so < 0 {
		t.Fatalf("http=%v", http)
	}
	if containsEngine(chrome, "bing") {
		t.Fatalf("bing chrome queued before HTTP exhausted: chrome=%v", chrome)
	}
}

func TestPartitionHTTPChromeExplicitDualKeepsChromeFallback(t *testing.T) {
	http, chrome := PartitionHTTPChrome([]string{"bing"}, "bing", false)
	if !reflect.DeepEqual(http, []string{"bing"}) || !reflect.DeepEqual(chrome, []string{"bing"}) {
		t.Fatalf("explicit bing: http=%v chrome=%v", http, chrome)
	}
	http, chrome = PartitionHTTPChrome([]string{"google"}, "google", false)
	if len(http) != 0 || !reflect.DeepEqual(chrome, []string{"google"}) {
		t.Fatalf("explicit google: http=%v chrome=%v", http, chrome)
	}
	http, chrome = PartitionHTTPChrome([]string{"duckduckgo_html", "duckduckgo"}, "duckduckgo", false)
	if !reflect.DeepEqual(http, []string{"duckduckgo_html"}) || !reflect.DeepEqual(chrome, []string{"duckduckgo"}) {
		t.Fatalf("ddg split: http=%v chrome=%v", http, chrome)
	}
}

func TestIsChromeOnly(t *testing.T) {
	if !IsChromeOnly("google") || !IsChromeOnly("duckduckgo") {
		t.Fatal("google/ddg are chrome-only")
	}
	if IsChromeOnly("bing") || IsChromeOnly("baidu") || IsChromeOnly("duckduckgo_html") || IsChromeOnly("auto") {
		t.Fatal("dual/http/auto are not chrome-only")
	}
}

func indexOf(chain []string, name string) int {
	for i, e := range chain {
		if e == name {
			return i
		}
	}
	return -1
}
