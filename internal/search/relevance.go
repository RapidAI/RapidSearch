package search

import (
	"context"
	"log"
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

// PreprocessOpts controls post-SERP cleaning, relevance filtering, and
// optional landing-page content extraction.
type PreprocessOpts struct {
	Query        string
	Engine       string
	Limit        int
	FetchContent bool
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"if": true, "then": true, "else": true, "when": true, "at": true, "by": true,
	"for": true, "with": true, "about": true, "against": true, "between": true,
	"into": true, "through": true, "during": true, "before": true, "after": true,
	"above": true, "below": true, "to": true, "from": true, "up": true, "down": true,
	"in": true, "out": true, "on": true, "off": true, "over": true, "under": true,
	"again": true, "further": true, "once": true, "here": true, "there": true,
	"all": true, "any": true, "both": true, "each": true, "few": true, "more": true,
	"most": true, "other": true, "some": true, "such": true, "no": true, "nor": true,
	"not": true, "only": true, "own": true, "same": true, "so": true, "than": true,
	"too": true, "very": true, "can": true, "will": true, "just": true,
	"should": true, "now": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "of": true, "as": true, "it": true,
	"its": true, "this": true, "that": true, "these": true, "those": true,
	"i": true, "me": true, "my": true, "we": true, "our": true, "you": true,
	"your": true, "he": true, "she": true, "they": true, "them": true, "his": true,
	"her": true, "their": true, "what": true, "which": true, "who": true,
	"whom": true, "how": true, "where": true, "why": true, "www": true,
	"com": true, "org": true, "net": true, "edu": true, "html": true, "htm": true,
	"php": true, "asp": true, "aspx": true, "jsp": true, "index": true,
	"en": true, "us": true, "utf": true, "amp": true,
}

const (
	weightTitle   = 3.0
	weightSnippet = 1.5
	weightURL     = 1.0
)

// Preprocess cleans organic hits, strips ads/SERP junk, drops off-topic
// results, re-ranks by relevance, and optionally fetches topic-relevant
// landing-page text. Chrome is not used. Ads filtering is mandatory; if
// relevance filtering would drop every remaining hit, the ads-stripped
// cleaned list is returned instead of an error.
func Preprocess(ctx context.Context, in []Result, opt PreprocessOpts) []Result {
	opt.Limit = ClampLimit(opt.Limit)
	unwrapBaiduHits(in)
	cleaned := cleanResults(in, opt.Engine)
	cleaned = filterAds(cleaned)
	if len(cleaned) == 0 {
		return cleaned
	}
	qTokens := Tokenize(opt.Query)
	scored := scoreResults(cleaned, qTokens)
	kept := filterRelevant(scored, qTokens)
	if len(kept) == 0 {
		log.Printf("preprocess fallback=cleaned query=%q in=%d cleaned=%d", opt.Query, len(in), len(cleaned))
		kept = scored
	} else {
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].Relevance == kept[j].Relevance {
				return kept[i].Rank < kept[j].Rank
			}
			return kept[i].Relevance > kept[j].Relevance
		})
	}
	if len(kept) > opt.Limit {
		kept = kept[:opt.Limit]
	}
	for i := range kept {
		kept[i].Rank = i + 1
		kept[i].Content = ""
	}
	if opt.FetchContent {
		extractInto(ctx, kept, qTokens, opt.Limit)
	}
	log.Printf("preprocess query=%q cleaned=%d kept=%d fetch=%v", opt.Query, len(cleaned), len(kept), opt.FetchContent)
	return kept
}

func cleanResults(in []Result, engine string) []Result {
	out := make([]Result, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, r := range in {
		title := strings.TrimSpace(r.Title)
		if i := strings.IndexAny(title, "\n\r"); i >= 0 {
			title = strings.TrimSpace(title[:i])
		}
		snippet := strings.Join(strings.Fields(strings.TrimSpace(r.Snippet)), " ")
		u := cleanURL(strings.TrimSpace(r.URL), engine)
		if title == "" || u == "" {
			continue
		}
		lu := strings.ToLower(u)
		if strings.HasPrefix(lu, "javascript:") {
			continue
		}
		if seen[lu] {
			continue
		}
		seen[lu] = true
		out = append(out, Result{
			Rank:    r.Rank,
			Title:   title,
			URL:     u,
			Snippet: snippet,
		})
	}
	return out
}

type scoredResult struct {
	Result
	titleHits   int
	snippetHits int
	urlHits     int
	covered     int
}

func scoreResults(in []Result, qTokens []string) []Result {
	out := make([]Result, len(in))
	for i, r := range in {
		sr := scoreOne(r, qTokens)
		r.Relevance = sr.Relevance
		out[i] = r
	}
	return out
}

func scoreOne(r Result, qTokens []string) scoredResult {
	titleSet := tokenSet(Tokenize(r.Title))
	snipSet := tokenSet(Tokenize(r.Snippet))
	urlSet := tokenSet(Tokenize(urlPathText(r.URL)))
	var titleHits, snippetHits, urlHits, covered int
	for _, t := range qTokens {
		th := tokenIn(t, titleSet)
		sh := tokenIn(t, snipSet)
		uh := tokenIn(t, urlSet)
		if th {
			titleHits++
		}
		if sh {
			snippetHits++
		}
		if uh {
			urlHits++
		}
		if th || sh || uh {
			covered++
		}
	}
	rel := 0.0
	if n := len(qTokens); n > 0 {
		raw := weightTitle*float64(titleHits) + weightSnippet*float64(snippetHits) + weightURL*float64(urlHits)
		norm := (weightTitle + weightSnippet + weightURL) * float64(n)
		if norm > 0 {
			rel = raw / norm
		}
		if titleHits == n {
			rel = math.Min(1, rel+0.08)
		}
	}
	r.Relevance = roundRel(rel)
	return scoredResult{
		Result:      r,
		titleHits:   titleHits,
		snippetHits: snippetHits,
		urlHits:     urlHits,
		covered:     covered,
	}
}

func filterRelevant(in []Result, qTokens []string) []Result {
	if len(qTokens) == 0 {
		return in
	}
	out := make([]Result, 0, len(in))
	needTitle := (len(qTokens) + 1) / 2
	if needTitle < 1 {
		needTitle = 1
	}
	strongN := 0
	for _, t := range qTokens {
		if !weakQueryToken(t) {
			strongN++
		}
	}
	for _, r := range in {
		sr := scoreOne(r, qTokens)
		if sr.covered == 0 {
			continue
		}
		if strings.TrimSpace(r.Snippet) == "" && sr.titleHits < needTitle {
			continue
		}
		if strongN > 0 && onlyWeakCoverage(r, qTokens) && sr.titleHits+sr.snippetHits <= 1 {
			continue
		}
		r.Relevance = sr.Relevance
		out = append(out, r)
	}
	return out
}

func onlyWeakCoverage(r Result, qTokens []string) bool {
	titleSet := tokenSet(Tokenize(r.Title))
	snipSet := tokenSet(Tokenize(r.Snippet))
	urlSet := tokenSet(Tokenize(urlPathText(r.URL)))
	for _, t := range qTokens {
		if weakQueryToken(t) {
			continue
		}
		if tokenIn(t, titleSet) || tokenIn(t, snipSet) || tokenIn(t, urlSet) {
			return false
		}
	}
	return true
}

// weakQueryToken is a leftover that should not, by itself, keep a hit
// when title+snippet coverage is tiny (e.g. only "http", or a CJK
// 2-gram of a stop-ish word).
func weakQueryToken(t string) bool {
	if t == "" || stopwords[t] || weakTokens[t] {
		return true
	}
	if runeLen(t) == 2 && isCJK([]rune(t)[0]) && isCJK([]rune(t)[1]) && cjkWeakBigram[t] {
		return true
	}
	return false
}

var weakTokens = map[string]bool{
	"http": true, "https": true, "www": true, "html": true, "htm": true,
	"page": true, "home": true, "web": true, "site": true, "online": true,
	"click": true, "info": true, "news": true, "blog": true, "index": true,
}

var cjkWeakBigram = map[string]bool{
	"什么": true, "怎么": true, "如何": true, "一个": true, "这个": true,
	"那个": true, "可以": true, "没有": true, "不是": true, "自己": true,
	"我们": true, "他们": true, "以及": true, "或者": true, "因为": true,
	"所以": true, "如果": true, "但是": true, "还是": true, "一下": true,
}

func urlPathText(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.Split(host, ".")
	if len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	path := u.Path
	path = strings.ReplaceAll(path, "/", " ")
	path = strings.ReplaceAll(path, "-", " ")
	path = strings.ReplaceAll(path, "_", " ")
	return strings.Join(parts, " ") + " " + path
}

// Tokenize splits s into English tokens (non-alnum separators) and CJK
// runs plus optional character 2-grams. Stopwords and 1-char latin tokens
// are dropped. Order is preserved; duplicates are removed.
func Tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var (
		tokens []string
		seen   = map[string]bool{}
		latin  []rune
		cjk    []rune
	)
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] || stopwords[t] {
			return
		}
		seen[t] = true
		tokens = append(tokens, t)
	}
	flushLatin := func() {
		if len(latin) == 0 {
			return
		}
		t := string(latin)
		latin = latin[:0]
		if len([]rune(t)) < 2 {
			return
		}
		add(t)
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		run := make([]rune, len(cjk))
		copy(run, cjk)
		cjk = cjk[:0]
		add(string(run))
		if len(run) >= 2 {
			for i := 0; i+1 < len(run); i++ {
				add(string(run[i : i+2]))
			}
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isCJK(r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			latin = append(latin, r)
		default:
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

func tokenSet(toks []string) map[string]bool {
	m := make(map[string]bool, len(toks))
	for _, t := range toks {
		m[t] = true
	}
	return m
}

var tokenAliases = map[string][]string{
	"golang": {"go"},
	"go":     {"golang"},
}

func tokenIn(tok string, set map[string]bool) bool {
	candidates := []string{tok}
	candidates = append(candidates, tokenAliases[tok]...)
	for _, c := range candidates {
		if set[c] {
			return true
		}
		if len(c) >= 4 && strings.HasSuffix(c, "s") && set[strings.TrimSuffix(c, "s")] {
			return true
		}
		if len(c) >= 3 && set[c+"s"] {
			return true
		}
	}
	return false
}

func tokensOverlap(text string, qTokens []string) bool {
	if len(qTokens) == 0 {
		return false
	}
	set := tokenSet(Tokenize(text))
	for _, t := range qTokens {
		if tokenIn(t, set) {
			return true
		}
	}
	return false
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

func roundRel(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return math.Round(f*100) / 100
}

func runeLen(s string) int { return len([]rune(s)) }

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	r = r[:n]
	// avoid cutting mid-word when possible
	for i := len(r) - 1; i > n*3/4; i-- {
		if unicode.IsSpace(r[i]) || r[i] == '.' || r[i] == '。' {
			r = r[:i]
			break
		}
	}
	return strings.TrimSpace(string(r)) + "…"
}
