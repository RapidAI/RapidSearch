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
	if !reflect.DeepEqual(cn, []string{"baidu", "bing", "duckduckgo"}) {
		t.Fatalf("china chain: %v", cn)
	}
	gl := Schedule("", true, RouteHints{Query: "golang http server"})
	if !reflect.DeepEqual(gl, []string{"google", "bing", "duckduckgo"}) {
		t.Fatalf("global chain: %v", gl)
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
	if !reflect.DeepEqual(fb, []string{"google", "bing", "duckduckgo"}) {
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
