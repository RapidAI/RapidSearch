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
	if !reflect.DeepEqual(gl, []string{"duckduckgo_html", "bing", "google", "duckduckgo"}) {
		t.Fatalf("global chain: %v", gl)
	}
	idxHTML, idxG := -1, -1
	for i, e := range gl {
		if e == "duckduckgo_html" {
			idxHTML = i
		}
		if e == "google" {
			idxG = i
		}
	}
	if idxHTML < 0 || idxG < 0 || idxHTML > idxG {
		t.Fatalf("global should prefer ddg html before google: %v", gl)
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
	if !reflect.DeepEqual(fb, []string{"google", "duckduckgo_html", "bing", "duckduckgo"}) {
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
