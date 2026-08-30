package search

import (
	"net/url"
	"strings"
	"unicode"
)

// filterAds drops sponsored, affiliate, PLA, and SERP-junk hits. It is not
// optional: Preprocess never falls back to a list that still contains ads.
func filterAds(in []Result) []Result {
	out := make([]Result, 0, len(in))
	for _, r := range in {
		if isAdOrJunk(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func isAdOrJunk(r Result) bool {
	title := strings.TrimSpace(r.Title)
	u := strings.TrimSpace(r.URL)
	if title == "" || u == "" {
		return true
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "javascript:") {
		return true
	}
	if isAdOrJunkURL(u) {
		return true
	}
	if hasAdLabel(title, r.Snippet) {
		return true
	}
	if isPAALeftover(title, u, r.Snippet) {
		return true
	}
	return false
}

func isPAALeftover(title, rawURL, snippet string) bool {
	if strings.TrimSpace(rawURL) != "" {
		return false
	}
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	if strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") {
		return true
	}
	return strings.TrimSpace(snippet) == ""
}

func isAdOrJunkURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.Path)
	if path == "" {
		path = "/"
	}
	q := u.RawQuery

	if adOrTrackerHost(host) {
		return true
	}
	if strings.Contains(path, "/pagead/") || strings.Contains(path, "/aclk") {
		return true
	}
	if strings.Contains(host, "amazon.") && strings.Contains(path, "/gp/slredirect") {
		return true
	}
	if jdUnion(host, path) {
		return true
	}
	if strings.Contains(host, "baidu.com") && (path == "/baidu.php" || strings.HasPrefix(path, "/baidu.php")) {
		return true
	}
	if strings.Contains(host, "bing.com") && strings.HasPrefix(path, "/aclick") {
		return true
	}
	// Engine SERP pages themselves (organic Bing ck/a redirects are NOT dropped here).
	if serpSelfURL(host, path, q) {
		return true
	}
	return false
}

func adOrTrackerHost(host string) bool {
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return false
	}
	needles := []string{
		"googleadservices.com",
		"doubleclick.net",
		"googlesyndication.com",
		"adservice.google.",
		"ads.yahoo.com",
		"cpro.baidu.com",
		"union.baidu.com",
		"pos.baidu.com",
		"s.click.taobao.com",
		"click.union.vip",
		"webcache.googleusercontent.com",
	}
	for _, n := range needles {
		if strings.Contains(host, n) {
			return true
		}
	}
	if strings.Contains(host, "translate.google") {
		return true
	}
	return false
}

func jdUnion(host, path string) bool {
	if !strings.Contains(host, "jd.com") {
		return false
	}
	if strings.Contains(host, "union") || strings.Contains(host, "click.jd") {
		return true
	}
	if strings.Contains(path, "/union") || strings.Contains(path, "unionclick") || strings.Contains(path, "union_click") {
		return true
	}
	return false
}

func serpSelfURL(host, path, rawQuery string) bool {
	if strings.Contains(host, "bing.com") {
		if path == "/search" || path == "/" || strings.HasPrefix(path, "/images") || strings.HasPrefix(path, "/videos") {
			return true
		}
	}
	if strings.Contains(host, "baidu.com") {
		if path == "/s" || path == "/" || path == "/baidu" {
			return true
		}
	}
	if strings.Contains(host, "duckduckgo.com") {
		if path == "/" || path == "/html/" || path == "/html" {
			return true
		}
		if strings.Contains(rawQuery, "q=") && (path == "/" || path == "") {
			return true
		}
	}
	if strings.Contains(host, "google.") {
		if path == "/search" || path == "/" || path == "/webhp" {
			return true
		}
	}
	if strings.Contains(host, "sogou.com") {
		if path == "/web" || path == "/" {
			return true
		}
	}
	if host == "so.com" || strings.HasSuffix(host, ".so.com") {
		if path == "/s" || path == "/" {
			return true
		}
	}
	return false
}

// hasAdLabel reports sponsored/ad badges. Whole-token / start-of-field only so
// organic pages about advertising, adobe.com, and admin consoles are kept.
func hasAdLabel(title, snippet string) bool {
	return looksLikeAdText(title) || looksLikeAdText(snippet)
}

func looksLikeAdText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "【广告】") || strings.HasPrefix(s, "[广告]") {
		return true
	}
	for _, p := range []string{"热门推荐", "人人都在搜", "精选好物"} {
		if strings.HasPrefix(s, p) {
			rest := s[len(p):]
			if rest == "" {
				return true
			}
			if isLabelSep([]rune(rest)[0]) {
				return true
			}
		}
	}
	if hasCJKAdPrefix(s) {
		return true
	}
	return hasEnglishAdPrefix(strings.ToLower(s))
}

func hasCJKAdPrefix(s string) bool {
	for _, badge := range []string{"广告", "赞助", "推广"} {
		if !strings.HasPrefix(s, badge) {
			continue
		}
		rest := s[len(badge):]
		if rest == "" {
			return true
		}
		r := []rune(rest)
		if isLabelSep(r[0]) {
			return true
		}
	}
	return false
}

func hasEnglishAdPrefix(lower string) bool {
	for _, p := range []string{
		"ads ", "ads\t", "ads ·", "ads·", "ads •", "ads•",
		"ads |", "ads|", "ads:", "ad ·", "ad·", "ad •", "ad•",
		"ad |", "ad|", "ad:",
	} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	if strings.HasPrefix(lower, "sponsored") {
		rest := strings.TrimPrefix(lower, "sponsored")
		if rest == "" {
			return true
		}
		trimmed := strings.TrimLeftFunc(rest, unicode.IsSpace)
		if trimmed == "" {
			return true
		}
		r := []rune(trimmed)
		if strings.ContainsRune("·•・|:;—–-", r[0]) {
			return true
		}
	}
	return false
}

func isLabelSep(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune("·•・|.|:;—–-【】[]（）()「」", r)
}
