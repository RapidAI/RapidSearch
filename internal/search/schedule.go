package search

import (
	"strings"
	"unicode"
)

// China-intent tokens for English/pinyin queries. Keep this list small.
var cnIntentTokens = []string{
	"china", "chinese", "beijing", "shanghai",
	"wechat", "weixin", "zhihu", "bilibili",
	"xiaohongshu", "bytedance",
}

// Failover chains. Google is omitted from the China chain: it is captcha-prone
// here and not the right corpus for CN queries.
var (
	chinaChain  = []string{"baidu", "bing", "duckduckgo"}
	globalChain = []string{"google", "bing", "duckduckgo"}
)

// RouteHints are optional API signals used with the query to pick a chain.
type RouteHints struct {
	Query  string
	Region string
	Locale string
	HL     string
}

// IsChinaRoute is true when the query or hints point at China / Chinese content.
func IsChinaRoute(h RouteHints) bool {
	if ContainsHan(h.Query) {
		return true
	}
	region := strings.ToLower(strings.TrimSpace(h.Region))
	locale := strings.ToLower(strings.TrimSpace(h.Locale))
	hl := strings.ToLower(strings.TrimSpace(h.HL))
	switch region {
	case "cn", "china", "zh", "zh-cn", "zh_cn":
		return true
	}
	if locale == "zh" || strings.HasPrefix(locale, "zh-") || strings.HasPrefix(locale, "zh_") {
		return true
	}
	if hl == "zh" || strings.HasPrefix(hl, "zh-") || strings.HasPrefix(hl, "zh_") {
		return true
	}
	return hasCNIntent(h.Query)
}

// ContainsHan reports whether s contains any Unicode Han character.
func ContainsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func hasCNIntent(q string) bool {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	for _, t := range cnIntentTokens {
		if set[t] {
			return true
		}
	}
	return false
}

// ShouldFallback: auto defaults to failover on; an explicit engine defaults off
// unless the caller set fallback. fallbackSet distinguishes omitted vs false.
func ShouldFallback(requested string, fallbackSet, fallback bool) bool {
	if fallbackSet {
		return fallback
	}
	return requested == "auto" || requested == ""
}

// Schedule returns the engine attempt list. requested is already normalized
// (auto|google|bing|baidu|duckduckgo). The same engine is never listed twice.
func Schedule(requested string, fallback bool, h RouteHints) []string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	china := IsChinaRoute(h)
	if requested == "" || requested == "auto" {
		chain := globalChain
		if china {
			chain = chinaChain
		}
		if !fallback {
			return []string{chain[0]}
		}
		out := make([]string, len(chain))
		copy(out, chain)
		return out
	}
	if !fallback {
		return []string{requested}
	}
	base := globalChain
	if china || requested == "baidu" {
		base = chinaChain
	}
	out := []string{requested}
	seen := map[string]bool{requested: true}
	for _, e := range base {
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
