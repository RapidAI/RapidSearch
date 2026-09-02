package search

import (
	"html"
	"strings"
	"unicode"
)

type htmlElem struct {
	Tag   string
	Attrs map[string]string
	Inner string
}

func elemAttr(e htmlElem, name string) string {
	if e.Attrs == nil {
		return ""
	}
	return e.Attrs[strings.ToLower(name)]
}

func classTokens(e htmlElem) []string {
	return strings.Fields(elemAttr(e, "class"))
}

func hasClass(e htmlElem, name string) bool {
	for _, c := range classTokens(e) {
		if c == name {
			return true
		}
	}
	return false
}

func innerText(inner string) string {
	var b strings.Builder
	i := 0
	n := len(inner)
	skip := ""
	for i < n {
		if skip != "" {
			close := "</" + skip
			idx := strings.Index(strings.ToLower(inner[i:]), close)
			if idx < 0 {
				break
			}
			k := i + idx + len(close)
			for k < n && inner[k] != '>' {
				k++
			}
			if k < n {
				k++
			}
			i = k
			skip = ""
			b.WriteByte(' ')
			continue
		}
		if inner[i] != '<' {
			j := i + 1
			for j < n && inner[j] != '<' {
				j++
			}
			b.WriteString(inner[i:j])
			i = j
			continue
		}
		if strings.HasPrefix(inner[i:], "<!--") {
			k := strings.Index(inner[i+4:], "-->")
			if k < 0 {
				break
			}
			i += 4 + k + 3
			continue
		}
		j := i + 1
		isEnd := false
		if j < n && inner[j] == '/' {
			isEnd = true
			j++
		}
		ns := j
		for j < n && isTagNameChar(inner[j]) {
			j++
		}
		name := strings.ToLower(inner[ns:j])
		for j < n && inner[j] != '>' {
			if inner[j] == '"' {
				j++
				for j < n && inner[j] != '"' {
					j++
				}
			} else if inner[j] == '\'' {
				j++
				for j < n && inner[j] != '\'' {
					j++
				}
			}
			j++
		}
		if j < n {
			j++
		}
		i = j
		if !isEnd && (name == "script" || name == "style" || name == "noscript") {
			skip = name
			continue
		}
		if name == "br" || name == "p" || name == "div" || name == "li" || name == "h3" || name == "h2" || name == "tr" {
			b.WriteByte(' ')
		}
	}
	s := html.UnescapeString(b.String())
	return strings.Join(strings.Fields(s), " ")
}

func findElements(doc, tag string, pred func(htmlElem) bool) []htmlElem {
	tag = strings.ToLower(tag)
	var out []htmlElem
	i := 0
	guard := 0
	for i < len(doc) && guard < 8000 {
		guard++
		idx := findOpenTag(doc, tag, i)
		if idx < 0 {
			break
		}
		attrs, openEnd, selfClose := parseOpenTag(doc, idx)
		if openEnd <= idx {
			i = idx + 1
			continue
		}
		el := htmlElem{Tag: tag, Attrs: attrs}
		if !selfClose {
			closeStart, _ := findMatchingClose(doc, tag, openEnd)
			if closeStart >= openEnd {
				el.Inner = doc[openEnd:closeStart]
			}
		}
		i = openEnd
		if pred == nil || pred(el) {
			out = append(out, el)
		}
	}
	return out
}

func findOpenTag(s, tag string, from int) int {
	if from < 0 {
		from = 0
	}
	low := strings.ToLower(s)
	needle := "<" + tag
	i := from
	for i < len(low) {
		j := strings.Index(low[i:], needle)
		if j < 0 {
			return -1
		}
		j += i
		after := j + len(needle)
		if after >= len(s) {
			return j
		}
		c := s[after]
		if c == '>' || c == '/' || unicode.IsSpace(rune(c)) {
			return j
		}
		i = after
	}
	return -1
}

func parseOpenTag(s string, start int) (attrs map[string]string, end int, selfClose bool) {
	attrs = map[string]string{}
	if start < 0 || start >= len(s) || s[start] != '<' {
		return attrs, start + 1, false
	}
	i := start + 1
	for i < len(s) && !unicode.IsSpace(rune(s[i])) && s[i] != '>' && s[i] != '/' {
		i++
	}
	for i < len(s) {
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		if i >= len(s) {
			return attrs, len(s), selfClose
		}
		if s[i] == '>' {
			return attrs, i + 1, selfClose
		}
		if s[i] == '/' {
			selfClose = true
			i++
			continue
		}
		ns := i
		for i < len(s) && s[i] != '=' && !unicode.IsSpace(rune(s[i])) && s[i] != '>' && s[i] != '/' {
			i++
		}
		name := strings.ToLower(s[ns:i])
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		val := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && unicode.IsSpace(rune(s[i])) {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				vs := i
				for i < len(s) && s[i] != q {
					i++
				}
				val = s[vs:i]
				if i < len(s) {
					i++
				}
			} else {
				vs := i
				for i < len(s) && !unicode.IsSpace(rune(s[i])) && s[i] != '>' {
					i++
				}
				val = s[vs:i]
			}
		}
		if name != "" {
			attrs[name] = html.UnescapeString(val)
		}
	}
	return attrs, len(s), selfClose
}

func findMatchingClose(s, tag string, from int) (closeStart, closeEnd int) {
	low := strings.ToLower(s)
	open := "<" + tag
	close := "</" + tag
	depth := 1
	i := from
	guard := 0
	for i < len(low) && depth > 0 && guard < 20000 {
		guard++
		o := indexTagAt(low, open, i)
		c := strings.Index(low[i:], close)
		if c >= 0 {
			c += i
		} else {
			return -1, -1
		}
		if o >= 0 && o < c {
			_, oe, self := parseOpenTag(s, o)
			if oe <= o {
				i = o + 1
				continue
			}
			if !self {
				depth++
			}
			i = oe
			continue
		}
		ce := c + len(close)
		for ce < len(s) && s[ce] != '>' {
			ce++
		}
		if ce < len(s) {
			ce++
		}
		depth--
		if depth == 0 {
			return c, ce
		}
		i = ce
	}
	return -1, -1
}

func indexTagAt(low, needle string, from int) int {
	i := from
	for i < len(low) {
		j := strings.Index(low[i:], needle)
		if j < 0 {
			return -1
		}
		j += i
		after := j + len(needle)
		if after >= len(low) {
			return j
		}
		c := low[after]
		if c == '>' || c == '/' || unicode.IsSpace(rune(c)) {
			return j
		}
		i = after
	}
	return -1
}

func elemLooksAd(e htmlElem) bool {
	for _, c := range classTokens(e) {
		cl := strings.ToLower(c)
		if cl == "e_idea" || cl == "result--ad" || cl == "result--sponsored" || cl == "sponsored" {
			return true
		}
		if strings.Contains(cl, "tuiguang") || strings.Contains(cl, "e_idea") {
			return true
		}
	}
	id := strings.ToLower(elemAttr(e, "id"))
	if strings.Contains(id, "advert") || strings.Contains(id, "_ad_") || strings.HasPrefix(id, "ad_") {
		return true
	}
	return innerHasAdBadge(e.Inner)
}

func innerHasAdBadge(inner string) bool {
	for _, tag := range []string{"span", "em", "label", "i", "strong", "b"} {
		for _, el := range findElements(inner, tag, nil) {
			t := strings.TrimSpace(innerText(el.Inner))
			switch t {
			case "广告", "推广", "赞助", "Ad", "Ads", "Sponsored":
				return true
			}
		}
	}
	return false
}

func absHref(raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return raw
}

func hitsToResults(raw []rawHit, limit int, engine string) []Result {
	out := make([]Result, 0, len(raw))
	seen := map[string]bool{}
	for _, h := range raw {
		u := cleanURL(absHref(h.URL), engine)
		title := strings.TrimSpace(h.Title)
		if i := strings.IndexAny(title, "\n\r"); i >= 0 {
			title = strings.TrimSpace(title[:i])
		}
		if title == "" || u == "" {
			continue
		}
		key := strings.ToLower(u)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Result{
			Title:   title,
			URL:     u,
			Snippet: strings.TrimSpace(h.Snippet),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func firstAnchor(inner string, className string) (title, href string) {
	as := findElements(inner, "a", func(e htmlElem) bool {
		if className != "" && !hasClass(e, className) {
			return false
		}
		return strings.TrimSpace(elemAttr(e, "href")) != ""
	})
	if len(as) == 0 {
		return "", ""
	}
	a := as[0]
	href = elemAttr(a, "href")
	if v := elemAttr(a, "data-mdurl"); looksLikeAbsURL(absHref(v)) {
		href = v
	} else if v := elemAttr(a, "data-url"); looksLikeAbsURL(absHref(v)) {
		href = v
	}
	return innerText(a.Inner), href
}

func firstSnippet(inner string, classes ...string) string {
	for _, tag := range []string{"a", "p", "div", "span", "td"} {
		for _, el := range findElements(inner, tag, func(e htmlElem) bool {
			for _, c := range classes {
				if hasClass(e, c) {
					return true
				}
			}
			return false
		}) {
			t := innerText(el.Inner)
			if t != "" {
				return t
			}
		}
	}
	return ""
}

func parseDuckDuckGoHTML(doc string, limit int) []Result {
	blocks := findElements(doc, "div", func(e htmlElem) bool {
		return hasClass(e, "result")
	})
	var hits []rawHit
	push := func(title, href, snippet string, ad bool) {
		if ad || title == "" || href == "" {
			return
		}
		hits = append(hits, rawHit{Title: title, URL: href, Snippet: snippet})
	}
	for _, b := range blocks {
		if elemLooksAd(b) {
			continue
		}
		title, href := firstAnchor(b.Inner, "result__a")
		snippet := firstSnippet(b.Inner, "result__snippet")
		push(title, href, snippet, false)
		if len(hits) >= limit*2 {
			break
		}
	}
	if len(hits) == 0 {
		for _, a := range findElements(doc, "a", func(e htmlElem) bool {
			return hasClass(e, "result__a")
		}) {
			push(innerText(a.Inner), elemAttr(a, "href"), "", elemLooksAd(a))
			if len(hits) >= limit*2 {
				break
			}
		}
	}
	return hitsToResults(hits, limit, "duckduckgo")
}

func parseSogouHTML(doc string, limit int) []Result {
	blocks := findElements(doc, "div", func(e htmlElem) bool {
		return hasClass(e, "vrwrap") || hasClass(e, "rb")
	})
	var hits []rawHit
	for _, b := range blocks {
		if elemLooksAd(b) {
			continue
		}
		title, href := "", ""
		h3s := findElements(b.Inner, "h3", nil)
		if len(h3s) > 0 {
			title, href = firstAnchor(h3s[0].Inner, "")
			if title == "" {
				title = innerText(h3s[0].Inner)
			}
		}
		if href == "" {
			title, href = firstAnchor(b.Inner, "")
		}
		if title == "" || href == "" {
			continue
		}
		snippet := firstSnippet(b.Inner, "star-wiki", "str-info", "ft", "space-txt")
		if snippet == "" {
			snippet = innerText(b.Inner)
			if strings.HasPrefix(snippet, title) {
				snippet = strings.TrimSpace(strings.TrimPrefix(snippet, title))
			}
		}
		hits = append(hits, rawHit{Title: title, URL: href, Snippet: snippet})
		if len(hits) >= limit*2 {
			break
		}
	}
	if len(hits) == 0 {
		for _, h3 := range findElements(doc, "h3", nil) {
			title, href := firstAnchor(h3.Inner, "")
			if title == "" || href == "" {
				continue
			}
			hits = append(hits, rawHit{Title: title, URL: href})
			if len(hits) >= limit*2 {
				break
			}
		}
	}
	return hitsToResults(hits, limit, "sogou")
}

func parse360HTML(doc string, limit int) []Result {
	var hits []rawHit
	seen := map[string]bool{}
	collect := func(root string) {
		lis := findElements(root, "li", func(e htmlElem) bool {
			return hasClass(e, "res-list") || hasClass(e, "res") || strings.TrimSpace(e.Inner) != ""
		})
		blocks := lis
		if len(blocks) == 0 {
			blocks = findElements(root, "div", func(e htmlElem) bool {
				return hasClass(e, "res-list")
			})
		}
		for _, b := range blocks {
			if elemLooksAd(b) {
				continue
			}
			title, href := "", ""
			h3s := findElements(b.Inner, "h3", nil)
			if len(h3s) > 0 {
				title, href = firstAnchor(h3s[0].Inner, "")
			}
			if href == "" {
				title, href = firstAnchor(b.Inner, "")
			}
			if title == "" || href == "" {
				continue
			}
			key := strings.ToLower(absHref(href))
			if seen[key] {
				continue
			}
			seen[key] = true
			snippet := firstSnippet(b.Inner, "res-desc", "res-rich", "res-list-desc")
			hits = append(hits, rawHit{Title: title, URL: href, Snippet: snippet})
		}
	}

	mains := findElements(doc, "ul", func(e htmlElem) bool {
		return strings.EqualFold(elemAttr(e, "id"), "main")
	})
	if len(mains) == 0 {
		mains = findElements(doc, "div", func(e htmlElem) bool {
			return strings.EqualFold(elemAttr(e, "id"), "main")
		})
	}
	for _, m := range mains {
		collect(m.Inner)
	}
	sides := findElements(doc, "div", func(e htmlElem) bool {
		return strings.EqualFold(elemAttr(e, "id"), "side")
	})
	for _, s := range sides {
		collect(s.Inner)
	}
	if len(hits) == 0 {
		collect(doc)
	}
	return hitsToResults(hits, limit, "360")
}

func baiduLooksAd(e htmlElem) bool {
	if strings.TrimSpace(elemAttr(e, "cmatchid")) != "" {
		return true
	}
	tpl := strings.ToLower(elemAttr(e, "tpl") + " " + elemAttr(e, "data-tpl"))
	if strings.Contains(tpl, "adv") || strings.Contains(tpl, "ad_") {
		return true
	}
	for _, c := range classTokens(e) {
		cl := strings.ToLower(c)
		if strings.Contains(cl, "tuiguang") || strings.HasPrefix(cl, "ec-") || strings.HasPrefix(cl, "ec_") || cl == "c-container-ad" {
			return true
		}
	}
	return elemLooksAd(e)
}

func baiduCardHref(card htmlElem, href string) string {
	if mu := elemAttr(card, "mu"); looksLikeAbsURL(absHref(mu)) && !strings.Contains(strings.ToLower(mu), "baidu.com/link") {
		return mu
	}
	return href
}

func parseBaiduHTML(doc string, limit int) []Result {
	roots := findElements(doc, "div", func(e htmlElem) bool {
		return strings.EqualFold(elemAttr(e, "id"), "content_left")
	})
	body := doc
	if len(roots) > 0 {
		body = roots[0].Inner
	}
	cards := findElements(body, "div", func(e htmlElem) bool {
		if hasClass(e, "result") || hasClass(e, "c-container") || hasClass(e, "result-op") {
			return true
		}
		return strings.TrimSpace(elemAttr(e, "mu")) != "" || strings.TrimSpace(elemAttr(e, "srcid")) != ""
	})
	var hits []rawHit
	for _, card := range cards {
		if baiduLooksAd(card) {
			continue
		}
		title, href := "", ""
		h3s := findElements(card.Inner, "h3", nil)
		if len(h3s) > 0 {
			title, href = firstAnchor(h3s[0].Inner, "")
			if title == "" {
				title = innerText(h3s[0].Inner)
			}
		}
		if href == "" {
			title, href = firstAnchor(card.Inner, "")
		}
		href = baiduCardHref(card, href)
		if title == "" || href == "" {
			continue
		}
		snippet := firstSnippet(card.Inner, "c-abstract", "content-right_8ZsFk", "c-span9", "c-line-clamp3", "c-line-clamp2", "c-font-normal")
		hits = append(hits, rawHit{Title: title, URL: href, Snippet: snippet})
		if len(hits) >= limit*2 {
			break
		}
	}
	if len(hits) == 0 {
		for _, h3 := range findElements(body, "h3", nil) {
			title, href := firstAnchor(h3.Inner, "")
			if title == "" || href == "" {
				continue
			}
			hits = append(hits, rawHit{Title: title, URL: href})
			if len(hits) >= limit*2 {
				break
			}
		}
	}
	return hitsToResults(hits, limit, "baidu")
}

func bingLooksAd(e htmlElem) bool {
	if hasClass(e, "b_ad") || hasClass(e, "b_adlastchild") || hasClass(e, "b_adslug") {
		return true
	}
	switch strings.ToLower(elemAttr(e, "id")) {
	case "b_ads", "b_pole", "b_ad", "b_topw":
		return true
	}
	return elemLooksAd(e)
}

func parseBingHTML(doc string, limit int) []Result {
	var hits []rawHit
	items := findElements(doc, "li", func(e htmlElem) bool {
		return hasClass(e, "b_algo")
	})
	for _, li := range items {
		if bingLooksAd(li) {
			continue
		}
		title, href := "", ""
		h2s := findElements(li.Inner, "h2", nil)
		if len(h2s) > 0 {
			title, href = firstAnchor(h2s[0].Inner, "")
			if title == "" {
				title = innerText(h2s[0].Inner)
			}
		}
		if href == "" {
			title, href = firstAnchor(li.Inner, "")
		}
		if title == "" || href == "" {
			continue
		}
		snippet := firstSnippet(li.Inner, "b_lineclamp2", "b_lineclamp3", "b_lineclamp4", "b_algoSlug", "b_snippet", "b_caption")
		if snippet == "" {
			ps := findElements(li.Inner, "p", nil)
			if len(ps) > 0 {
				snippet = innerText(ps[0].Inner)
			}
		}
		hits = append(hits, rawHit{Title: title, URL: href, Snippet: snippet})
		if len(hits) >= limit*2 {
			break
		}
	}
	if len(hits) == 0 {
		roots := findElements(doc, "ol", func(e htmlElem) bool {
			return strings.EqualFold(elemAttr(e, "id"), "b_results")
		})
		scope := doc
		if len(roots) > 0 {
			scope = roots[0].Inner
		}
		for _, h2 := range findElements(scope, "h2", nil) {
			title, href := firstAnchor(h2.Inner, "")
			if title == "" || href == "" {
				continue
			}
			hits = append(hits, rawHit{Title: title, URL: href})
			if len(hits) >= limit*2 {
				break
			}
		}
	}
	return hitsToResults(hits, limit, "bing")
}

func parseEngineHTML(engine, body string, limit int) []Result {
	switch engine {
	case "duckduckgo_html", "duckduckgo":
		return parseDuckDuckGoHTML(body, limit)
	case "sogou":
		return parseSogouHTML(body, limit)
	case "360":
		return parse360HTML(body, limit)
	case "baidu":
		return parseBaiduHTML(body, limit)
	case "bing":
		return parseBingHTML(body, limit)
	default:
		return nil
	}
}
