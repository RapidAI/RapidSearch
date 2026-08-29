package search

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"html"
)

const (
	maxBodyBytes     = 1536 * 1024 // ~1.5MB
	maxContentRunes  = 1200
	extractTimeout   = 8 * time.Second
	maxExtractConcur = 3
	maxExtractHits   = 8
)

var extractClient = &http.Client{
	Timeout: extractTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

func extractInto(ctx context.Context, results []Result, qTokens []string, limit int) {
	n := len(results)
	capN := limit
	if capN > maxExtractHits {
		capN = maxExtractHits
	}
	if n > capN {
		n = capN
	}
	if n <= 0 {
		return
	}
	sem := make(chan struct{}, maxExtractConcur)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			results[i].Content = fetchRelevantContent(ctx, results[i].URL, qTokens)
		}(i)
	}
	wg.Wait()
}

func fetchRelevantContent(ctx context.Context, rawURL string, qTokens []string) string {
	if rawURL == "" {
		return ""
	}
	low := strings.ToLower(rawURL)
	for _, ext := range []string{".pdf", ".zip", ".gz", ".tgz", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".mp4", ".mp3", ".exe", ".dmg", ".woff", ".css", ".js"} {
		if strings.Contains(low, ext) && (strings.HasSuffix(strings.Split(low, "?")[0], ext) || strings.Contains(low, ext+"?")) {
			return ""
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.1")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := extractClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !isHTMLLike(ct) {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil && len(body) == 0 {
		return ""
	}
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	if len(body) >= 4 && bytes.Equal(body[:4], []byte("%PDF")) {
		return ""
	}
	if bytes.IndexByte(body[:min(len(body), 512)], 0) >= 0 {
		return ""
	}
	if !utf8.Valid(body) {
		body = bytes.ToValidUTF8(body, []byte(""))
	}
	text := htmlMainText(string(body))
	if text == "" {
		return ""
	}
	return relevantExcerpt(text, qTokens, maxContentRunes)
}

func isHTMLLike(ct string) bool {
	return strings.Contains(ct, "html") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "text/plain") ||
		strings.Contains(ct, "text/xhtml")
}

func htmlMainText(raw string) string {
	var article, main, paras, rest strings.Builder
	inArticle, inMain, inP := 0, 0, 0
	skipTag := ""
	i := 0
	n := len(raw)
	emit := func(s string) {
		if s == "" {
			return
		}
		if inArticle > 0 {
			article.WriteString(s)
		}
		if inMain > 0 {
			main.WriteString(s)
		}
		if inP > 0 {
			paras.WriteString(s)
		}
		rest.WriteString(s)
	}
	nl := func() { emit("\n") }

	for i < n {
		if skipTag != "" {
			close := "</" + skipTag
			idx := strings.Index(strings.ToLower(raw[i:]), close)
			if idx < 0 {
				break
			}
			k := i + idx + len(close)
			for k < n && raw[k] != '>' {
				k++
			}
			if k < n {
				k++
			}
			i = k
			skipTag = ""
			nl()
			continue
		}
		if raw[i] != '<' {
			j := i + 1
			for j < n && raw[j] != '<' {
				j++
			}
			emit(raw[i:j])
			i = j
			continue
		}
		if strings.HasPrefix(raw[i:], "<!--") {
			k := strings.Index(raw[i+4:], "-->")
			if k < 0 {
				break
			}
			i += 4 + k + 3
			continue
		}
		if strings.HasPrefix(raw[i:], "<!") || strings.HasPrefix(raw[i:], "<?") {
			k := strings.IndexByte(raw[i:], '>')
			if k < 0 {
				break
			}
			i += k + 1
			continue
		}
		j := i + 1
		isEnd := false
		if j < n && raw[j] == '/' {
			isEnd = true
			j++
		}
		startName := j
		for j < n && isTagNameChar(raw[j]) {
			j++
		}
		name := strings.ToLower(raw[startName:j])
		selfClose := false
		for j < n && raw[j] != '>' {
			c := raw[j]
			if c == '"' {
				j++
				for j < n && raw[j] != '"' {
					j++
				}
			} else if c == '\'' {
				j++
				for j < n && raw[j] != '\'' {
					j++
				}
			}
			j++
		}
		if j > i+1 && j-1 < n && raw[j-1] == '/' {
			selfClose = true
		}
		if j < n {
			j++
		}
		i = j
		if name == "" {
			continue
		}
		if selfClose || voidTags[name] {
			if name == "br" || name == "hr" {
				nl()
			}
			continue
		}
		if isEnd {
			switch name {
			case "article":
				if inArticle > 0 {
					inArticle--
				}
				nl()
			case "main":
				if inMain > 0 {
					inMain--
				}
				nl()
			case "p":
				if inP > 0 {
					inP--
				}
				nl()
			default:
				if blockTags[name] {
					nl()
				}
			}
			continue
		}
		if skipTags[name] {
			skipTag = name
			continue
		}
		switch name {
		case "article":
			inArticle++
		case "main":
			inMain++
		case "p":
			inP++
			nl()
		default:
			if blockTags[name] {
				nl()
			}
		}
	}

	pick := func(s string) string {
		return collapseWS(html.UnescapeString(s))
	}
	a, m, p, r := pick(article.String()), pick(main.String()), pick(paras.String()), pick(rest.String())
	best := ""
	bestN := 0
	consider := func(s string, min int) {
		n := runeLen(s)
		if n >= min && n > bestN {
			best, bestN = s, n
		}
	}
	consider(a, 80)
	if bestN == 0 {
		consider(m, 80)
	}
	if bestN == 0 {
		consider(p, 80)
	}
	if bestN == 0 {
		consider(a, 1)
		consider(m, 1)
		consider(p, 1)
	}
	if bestN < 40 {
		consider(r, 1)
	}
	return best
}

func relevantExcerpt(text string, qTokens []string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	sents := splitSentences(text)
	if len(sents) == 0 {
		if tokensOverlap(text, qTokens) {
			return clipRunes(text, maxRunes)
		}
		return ""
	}
	keep := make([]bool, len(sents))
	any := false
	for i, s := range sents {
		if tokensOverlap(s, qTokens) {
			keep[i] = true
			if i > 0 {
				keep[i-1] = true
			}
			if i+1 < len(sents) {
				keep[i+1] = true
			}
			any = true
		}
	}
	if !any {
		if tokensOverlap(text, qTokens) {
			return clipRunes(text, maxRunes)
		}
		return ""
	}
	var b strings.Builder
	last := -2
	for i, s := range sents {
		if !keep[i] {
			continue
		}
		if last == i-1 {
			b.WriteByte(' ')
		} else if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
		last = i
		if runeLen(b.String()) >= maxRunes {
			break
		}
	}
	return clipRunes(strings.TrimSpace(b.String()), maxRunes)
}

func splitSentences(text string) []string {
	var out []string
	var buf []rune
	flush := func() {
		s := strings.TrimSpace(string(buf))
		buf = buf[:0]
		if s != "" {
			out = append(out, s)
		}
	}
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		buf = append(buf, r)
		end := false
		switch r {
		case '。', '！', '？', '；':
			end = true
		case '!', '?':
			end = true
		case '.':
			if i+1 >= len(rs) || unicode.IsSpace(rs[i+1]) || rs[i+1] == '"' || rs[i+1] == '\'' || rs[i+1] == '”' {
				end = true
			}
		case '\n':
			end = true
		}
		if end {
			flush()
		}
	}
	flush()
	return out
}

func collapseWS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			kept = append(kept, ln)
		}
	}
	return strings.Join(kept, "\n")
}

func isTagNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "nav": true,
	"footer": true, "header": true, "aside": true, "form": true,
	"iframe": true, "svg": true, "template": true, "button": true,
	"select": true, "textarea": true, "head": true, "canvas": true,
}

var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "blockquote": true, "article": true, "section": true,
	"main": true, "pre": true, "td": true, "dt": true, "dd": true,
}
