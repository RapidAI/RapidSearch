package search

import (
	"strings"

	"github.com/go-rod/rod"
)

// Interstitial copy observed on this box (Google zh-CN recaptcha, Baidu
// wappass, DDG bot-wall). Keep in sync with the JS snippet list below.
var (
	googleURLMarkers   = []string{"/sorry/", "sorry.google", "ipv4.google.com", "ipv6.google.com"}
	googleTitleMarkers = []string{"unusual traffic", "进行人机身份验证", "关于此网页"}
	googleBodySnippets = []string{
		"our systems have detected unusual traffic",
		"detected unusual traffic from your computer",
		"unusual traffic from your computer network",
		"why did this happen",
		"please show you",
		"verify you are human",
		"please solve this puzzle",
		"checking your browser before accessing",
		"needs to review the security of your connection",
		"sorry, we have detected unusual traffic",
		"进行人机身份验证",
		"我们的系统检测到您的计算机网络中存在异常流量",
		"此网页用于确认这些请求是由您而不是自动程序发出的",
		"为什么会这样",
	}
	baiduURLMarkers   = []string{"wappass.baidu.com", "wappass"}
	baiduTitleMarkers = []string{"安全验证"}
	ddgBodySnippets   = []string{
		"unfortunately, bots use duckduckgo too",
		"unfortunately bots use duckduckgo too",
		"please complete the following challenge",
	}
	bingBodySnippets = []string{
		"please verify you are a human",
		"help us confirm you are not a robot",
	}
)

func containsAny(hay string, needles []string) string {
	h := strings.ToLower(hay)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(h, strings.ToLower(n)) {
			return n
		}
	}
	return ""
}

// classifyInterstitial is the URL/title half of detectBlock, unit-tested
// without Chrome. Organic Baidu SERPs (content_left) must not match.
func classifyInterstitial(pageURL, title string) (bool, string) {
	u := strings.ToLower(pageURL)
	t := strings.ToLower(title)

	if containsAny(u, baiduURLMarkers) != "" {
		if strings.Contains(u, "wappass") || strings.Contains(u, "captcha") {
			return true, "baidu verify / wappass interstitial"
		}
	}
	if strings.Contains(u, "baidu.com") && (strings.Contains(u, "captcha") || containsAny(t, baiduTitleMarkers) != "") {
		return true, "baidu captcha / 安全验证"
	}
	if containsAny(t, baiduTitleMarkers) != "" && (strings.Contains(u, "baidu") || strings.Contains(u, "wappass")) {
		return true, "baidu 安全验证"
	}
	if containsAny(u, googleURLMarkers) != "" {
		return true, "google unusual-traffic / captcha interstitial"
	}
	if strings.Contains(t, "unusual traffic") {
		return true, "unusual traffic page"
	}
	if (strings.Contains(u, "google.") || strings.Contains(u, "google.com")) && containsAny(t, googleTitleMarkers) != "" {
		if strings.Contains(t, "进行人机身份验证") || strings.Contains(t, "unusual traffic") {
			return true, "google unusual-traffic / captcha interstitial"
		}
	}
	return false, ""
}

func detectBlock(page *rod.Page) (bool, string) {
	info, err := page.Info()
	url := ""
	if err == nil && info != nil {
		url = info.URL
	}
	title := ""
	if obj, err := page.Eval(`() => document.title || ''`); err == nil && obj != nil {
		title = obj.Value.Str()
	}
	if blocked, why := classifyInterstitial(url, title); blocked {
		return true, why
	}

	obj, err := page.Eval(`() => {
  const body = ((document.body && document.body.innerText) || '').toLowerCase();
  const htmlHas = (sel) => { try { return !!document.querySelector(sel); } catch (e) { return false; } };
  const snippets = [
    'our systems have detected unusual traffic',
    'detected unusual traffic from your computer',
    'unusual traffic from your computer network',
    'why did this happen',
    'verify you are human',
    'please solve this puzzle',
    'checking your browser before accessing',
    'needs to review the security of your connection',
    'sorry, we have detected unusual traffic',
    '进行人机身份验证',
    '我们的系统检测到您的计算机网络中存在异常流量',
    '此网页用于确认这些请求是由您而不是自动程序发出的',
    'unfortunately, bots use duckduckgo too',
    'please complete the following challenge',
    'please verify you are a human',
    'help us confirm you are not a robot'
  ];
  let hit = '';
  for (const s of snippets) {
    if (body.includes(s)) { hit = s; break; }
  }
  const loc = ((location && location.href) || '').toLowerCase();
  const recaptcha = htmlHas('#captcha-form, form#captcha, #recaptcha, iframe[src*="recaptcha"], iframe[title*="recaptcha" i], iframe[src*="google.com/recaptcha"]');
  const cf = htmlHas('#challenge-form, #cf-challenge-running, iframe[src*="challenges.cloudflare"]');
  const bingCap = htmlHas('#b_captcha, #captchaContainer') && (body.includes('captcha') || body.includes('verify you are'));
  const baiduHost = loc.includes('baidu.com') || loc.includes('wappass');
  const organicBaidu = htmlHas('#content_left .result, #content_left .c-container h3, #content_left h3.t, #content_left h3');
  const baiduCap = baiduHost && !organicBaidu && (
    htmlHas('.passMod_dialog, #pass-verify, iframe[src*="wappass"], .vcode-spin-button') ||
    body.includes('安全验证') ||
    loc.includes('wappass.baidu.com') ||
    (loc.includes('/captcha') && !organicBaidu)
  );
  const ddgCap = loc.includes('duckduckgo') && (
    body.includes('bots use duckduckgo') || htmlHas('#challenge-form, iframe[src*="challenges.cloudflare"]')
  );
  return {
    hit: hit,
    recaptcha: recaptcha,
    cf: cf,
    bingCap: bingCap,
    baiduCap: baiduCap,
    ddgCap: ddgCap,
    organicBaidu: organicBaidu,
    shortBody: body.slice(0, 400)
  };
}`)
	if err != nil || obj == nil {
		return false, ""
	}

	type det struct {
		Hit          string `json:"hit"`
		Recaptcha    bool   `json:"recaptcha"`
		CF           bool   `json:"cf"`
		BingCap      bool   `json:"bingCap"`
		BaiduCap     bool   `json:"baiduCap"`
		DdgCap       bool   `json:"ddgCap"`
		OrganicBaidu bool   `json:"organicBaidu"`
	}
	var d det
	if err := obj.Value.Unmarshal(&d); err != nil {
		return false, ""
	}
	if d.Recaptcha {
		return true, "captcha widget detected (recaptcha)"
	}
	if d.CF {
		return true, "bot-challenge interstitial (cloudflare)"
	}
	if d.BingCap {
		return true, "bing captcha interstitial"
	}
	if d.BaiduCap {
		return true, "baidu captcha / 安全验证"
	}
	if d.DdgCap {
		return true, "duckduckgo bot-challenge interstitial"
	}
	if d.Hit != "" {
		return true, "blocked page: " + d.Hit
	}
	return false, ""
}
