package search

import (
	"strings"

	"github.com/go-rod/rod"
)

func detectBlock(page *rod.Page) (bool, string) {
	info, err := page.Info()
	url := ""
	if err == nil && info != nil {
		url = strings.ToLower(info.URL)
	}
	title := ""
	if obj, err := page.Eval(`() => document.title || ''`); err == nil && obj != nil {
		title = strings.ToLower(obj.Value.Str())
	}

	if strings.Contains(url, "wappass.baidu.com") {
		return true, "baidu verify / wappass interstitial"
	}
	if strings.Contains(url, "baidu.com") && (strings.Contains(url, "captcha") || strings.Contains(title, "安全验证")) {
		return true, "baidu captcha / 安全验证"
	}
	if strings.Contains(title, "安全验证") && (strings.Contains(url, "baidu") || strings.Contains(url, "wappass")) {
		return true, "baidu 安全验证"
	}
	if strings.Contains(url, "/sorry/") || strings.Contains(url, "sorry.google") {
		return true, "google unusual-traffic / captcha interstitial"
	}
	if strings.Contains(url, "ipv4.google.com") || strings.Contains(url, "ipv6.google.com") {
		return true, "google sorry/ipv interstitial"
	}
	if strings.Contains(title, "unusual traffic") || strings.Contains(title, "before you continue") && strings.Contains(url, "consent") {
		// "before you continue" is often cookie consent, not captcha — don't treat as captcha.
	}
	if strings.Contains(title, "unusual traffic") {
		return true, "unusual traffic page"
	}

	obj, err := page.Eval(`() => {
  const body = ((document.body && document.body.innerText) || '').toLowerCase();
  const htmlHas = (sel) => !!document.querySelector(sel);
  const snippets = [
    'our systems have detected unusual traffic',
    'detected unusual traffic from your computer',
    'unusual traffic from your computer network',
    'why did this happen',
    'please show you',
    'verify you are human',
    'please solve this puzzle',
    'checking your browser before accessing',
    'needs to review the security of your connection',
    'sorry, we have detected unusual traffic'
  ];
  let hit = '';
  for (const s of snippets) {
    if (body.includes(s)) { hit = s; break; }
  }
  const loc = ((location && location.href) || '').toLowerCase();
  const recaptcha = htmlHas('#captcha-form, form#captcha, #recaptcha, iframe[src*="recaptcha"], iframe[title*="recaptcha" i]');
  const cf = htmlHas('#challenge-form, #cf-challenge-running, iframe[src*="challenges.cloudflare"]');
  const bingCap = htmlHas('#b_captcha, .captcha, #captchaContainer');
  const baiduHost = loc.includes('baidu.com') || loc.includes('wappass');
  const baiduCap = baiduHost && (
    htmlHas('#captcha, .passMod_dialog, #pass-verify, iframe[src*="wappass"], .vcode-spin-button, #pass-input') ||
    body.includes('安全验证') ||
    loc.includes('wappass.baidu.com') ||
    loc.includes('/captcha')
  );
  // Bing cookie wall is not a captcha.
  return {
    hit: hit,
    recaptcha: recaptcha,
    cf: cf,
    bingCap: bingCap && body.includes('captcha'),
    baiduCap: baiduCap,
    shortBody: body.slice(0, 400)
  };
}`)
	if err != nil || obj == nil {
		return false, ""
	}

	type det struct {
		Hit       string `json:"hit"`
		Recaptcha bool   `json:"recaptcha"`
		CF        bool   `json:"cf"`
		BingCap   bool   `json:"bingCap"`
		BaiduCap  bool   `json:"baiduCap"`
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
	if d.Hit != "" {
		return true, "blocked page: " + d.Hit
	}
	return false, ""
}
