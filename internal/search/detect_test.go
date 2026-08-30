package search

import "testing"

func TestClassifyInterstitialGoogle(t *testing.T) {
	cases := []struct {
		url, title string
		want       bool
	}{
		{"https://www.google.com/sorry/index?continue=https://www.google.com/search", "sorry", true},
		{"https://ipv4.google.com/sorry/index", "", true},
		{"https://www.google.com/search?q=sqlite+wal", "unusual traffic from your computer network", true},
		{"https://www.google.com/search?q=sqlite", "进行人机身份验证", true},
		{"https://www.google.com/search?q=golang", "golang - Google Search", false},
		{"https://www.bing.com/search?q=golang", "golang - Bing", false},
	}
	for _, tc := range cases {
		got, why := classifyInterstitial(tc.url, tc.title)
		if got != tc.want {
			t.Errorf("url=%q title=%q got %v (%s) want %v", tc.url, tc.title, got, why, tc.want)
		}
	}
}

func TestClassifyInterstitialBaidu(t *testing.T) {
	blocked, _ := classifyInterstitial("https://wappass.baidu.com/static/captcha", "安全验证")
	if !blocked {
		t.Fatal("wappass should block")
	}
	blocked, _ = classifyInterstitial("https://www.baidu.com/s?wd=%E5%B0%8F%E7%B1%B3su7", "小米su7_百度搜索")
	if blocked {
		t.Fatal("organic baidu SERP must not be classified by URL/title alone")
	}
}

func TestGoogleBodySnippetsIncludeChinese(t *testing.T) {
	need := []string{"进行人机身份验证", "我们的系统检测到您的计算机网络中存在异常流量"}
	for _, n := range need {
		if containsAny(n, googleBodySnippets) == "" {
			t.Errorf("missing snippet %q", n)
		}
	}
}

func TestContainsAny(t *testing.T) {
	if containsAny("HELLO Unusual Traffic HERE", googleTitleMarkers) == "" {
		t.Fatal("expected unusual traffic")
	}
	if containsAny("nothing", googleURLMarkers) != "" {
		t.Fatal("false positive")
	}
}
