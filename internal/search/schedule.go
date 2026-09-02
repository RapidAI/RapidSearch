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

// Failover chains. HTTP-capable engines come first so auto can finish without
// a Chrome slot. Google is omitted from both built-in auto chains: a
// datacenter IP burns the handler budget on Google Chrome before any captcha
// can trip the breaker. A saved settings priority may enable google; the
// breaker still fail-fasts / skips when open.
//
// Default (no saved priority, no API keys): these chains unchanged.
// With a Serper and/or Brave key (and that engine not disabled): serper then
// brave are prepended. After the user saves a priority list, auto uses that
// enabled order (keyed engines without a key are skipped).
var (
	chinaChain  = []string{"baidu", "sogou", "360", "bing", "duckduckgo_html", "duckduckgo"}
	globalChain = []string{"duckduckgo_html", "bing", "sogou", "360", "baidu", "duckduckgo"}
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

// Schedule returns the engine attempt list using the built-in China/Global
// chains (no saved settings). Same as ScheduleWith with an empty snapshot.
func Schedule(requested string, fallback bool, h RouteHints) []string {
	return ScheduleWith(requested, fallback, h, ConfigSnapshot{})
}

// ScheduleWith returns the engine attempt list. requested is already
// normalized (auto|google|bing|baidu|duckduckgo|duckduckgo_html|sogou|360|
// serper|brave). The same engine is never listed twice.
//
// engine=duckduckgo is a transport split: HTML first (no Chrome slot), then
// Chrome DDG even when fallback is off.
//
// When cfg has API keys and no custom priority, serper/brave are prepended
// to the built-in auto chain. A saved priority list replaces the built-in
// order for auto (disabled / missing-key keyed engines skipped). Google
// stays off auto unless the user enabled it in that list.
func ScheduleWith(requested string, fallback bool, h RouteHints, cfg ConfigSnapshot) []string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	china := IsChinaRoute(h)
	if requested == "" || requested == "auto" {
		chain := autoChain(china, cfg)
		if len(chain) == 0 {
			return nil
		}
		if !fallback {
			return []string{chain[0]}
		}
		out := make([]string, len(chain))
		copy(out, chain)
		return out
	}
	if requested == "duckduckgo" {
		out := []string{"duckduckgo_html", "duckduckgo"}
		if !fallback {
			return out
		}
		base := autoChain(china, cfg)
		seen := map[string]bool{"duckduckgo_html": true, "duckduckgo": true}
		for _, e := range base {
			if seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
		return out
	}
	if !fallback {
		return []string{requested}
	}
	baseChina := china || requested == "baidu" || requested == "sogou" || requested == "360"
	base := autoChain(baseChina, cfg)
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

func autoChain(china bool, cfg ConfigSnapshot) []string {
	if cfg.HasCustomPriority() {
		if out := cfg.filterCustomAuto(); len(out) > 0 {
			return out
		}
	}
	base := globalChain
	if china {
		base = chinaChain
	}
	return prependKeyedDefaults(base, cfg)
}

func prependKeyedDefaults(base []string, cfg ConfigSnapshot) []string {
	var front []string
	for _, id := range []string{"serper", "brave"} {
		if cfg.keyedWantedOnDefaultAuto(id) {
			front = append(front, id)
		}
	}
	if len(front) == 0 {
		out := make([]string, len(base))
		copy(out, base)
		return out
	}
	out := make([]string, 0, len(front)+len(base))
	out = append(out, front...)
	seen := map[string]bool{}
	for _, e := range front {
		seen[e] = true
	}
	for _, e := range base {
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// IsChromeOnly is true for engines that have no HTTP SERP path (google, duckduckgo).
func IsChromeOnly(engine string) bool {
	name, err := NormalizeEngine(engine)
	if err != nil || name == "auto" {
		return false
	}
	return NeedsChrome(name) && !SupportsHTTP(name)
}

// PartitionHTTPChrome splits a scheduled chain so every HTTP-capable engine
// is attempted as HTTP before any Chrome slot is acquired.
//
// Dual engines (baidu/sogou/360/bing) are HTTP-only on auto. Their Chrome
// fallback is only queued when that engine was explicitly requested.
// Built-in auto chains omit Google Chrome. If the user enabled google in
// settings priority, it is queued as Chrome after HTTP (breaker still
// applies). Keyed APIs (serper/brave) stay on the HTTP side and never take
// a Chrome slot.
func PartitionHTTPChrome(chain []string, requested string, fallback bool) (httpChain, chromeChain []string) {
	_ = fallback
	requested, _ = NormalizeEngine(requested)
	auto := requested == "auto"

	seenH := make(map[string]bool, len(chain))
	seenC := make(map[string]bool, len(chain))
	for _, e := range chain {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || e == "auto" {
			continue
		}
		if SupportsHTTP(e) && !seenH[e] {
			httpChain = append(httpChain, e)
			seenH[e] = true
		}
	}
	for _, e := range chain {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || e == "auto" || seenC[e] || !NeedsChrome(e) {
			continue
		}
		if IsChromeOnly(e) {
			// Auto includes Google Chrome only when the user enabled it in
			// settings priority. Built-in chains never list google.
			chromeChain = append(chromeChain, e)
			seenC[e] = true
			continue
		}
		// Dual-engine Chrome only for an explicit request (single-engine or
		// after the HTTP chain has already been tried).
		if !auto && e == requested {
			chromeChain = append(chromeChain, e)
			seenC[e] = true
		}
	}
	return httpChain, chromeChain
}
