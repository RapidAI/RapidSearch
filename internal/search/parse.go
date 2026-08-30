package search

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

type rawHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func parseGoogle(page *rod.Page, limit int) ([]Result, error) {
	obj, err := page.Eval(`(limit) => {
  const out = [];
  const seen = new Set();
  const isSkippable = (el) => {
    while (el) {
      const id = (el.id || '').toLowerCase();
      if (id === 'tads' || id === 'tadsb' || id === 'tadsbl' || id === 'cu-container' || id === 'bottomads' || id === 'tvcap' || id === 'taw') return 'ad';
      if (el.getAttribute && (el.hasAttribute('data-text-ad') || el.getAttribute('data-ad-slot'))) return 'ad';
      const cls = (el.className && el.className.toString && el.className.toString()) || '';
      if (/uEierd|commercial-unit|pla-unit|cu-container|commercial-unit-desktop-top/.test(cls)) return 'ad';
      if (/related-question-pair|wQiwMc/.test(cls)) return 'paa';
      if (el.getAttribute && el.getAttribute('jsname') === 'Cpkphb') return 'paa';
      el = el.parentElement;
    }
    return '';
  };
  const push = (title, href, snippet, el) => {
    title = (title || '').trim();
    href = (href || '').trim();
    snippet = (snippet || '').replace(/\s+/g, ' ').trim();
    if (!title || !href) return;
    if (href.startsWith('javascript:')) return;
    if (isSkippable(el)) return;
    const key = href + '|' + title;
    if (seen.has(key)) return;
    seen.add(key);
    out.push({title, url: href, snippet});
  };

  const cards = document.querySelectorAll('#rso .g, #search .g, #rso .MjjYud, #search .MjjYud, div[data-sokoban-container] .g, #center_col .g');
  cards.forEach((card) => {
    if (out.length >= limit * 2) return;
    const a = card.querySelector('a:has(h3), a h3') ? (card.querySelector('a:has(h3)') || card.querySelector('h3')?.closest('a')) : card.querySelector('h3')?.closest('a');
    const h3 = card.querySelector('h3');
    if (!a || !h3) return;
    let snippet = '';
    const sn = card.querySelector('div.VwiC3b, div[data-sncf], span.aCOpRe, div.IsZvec, div[style*="-webkit-line-clamp"], .lEBKkf');
    if (sn) snippet = sn.innerText;
    push(h3.innerText, a.href, snippet, card);
  });

  if (out.length < 3) {
    const hs = document.querySelectorAll('#search h3, #rso h3, #center_col h3');
    hs.forEach((h3) => {
      if (out.length >= limit * 2) return;
      const a = h3.closest('a');
      if (!a) return;
      let snippet = '';
      const block = h3.closest('.g, .MjjYud, [data-hveid]') || a.parentElement;
      if (block) {
        const sn = block.querySelector('div.VwiC3b, div[data-sncf], span.aCOpRe, .lEBKkf');
        if (sn) snippet = sn.innerText;
      }
      push(h3.innerText, a.href, snippet, h3);
    });
  }
  return out;
}`, limit)
	if err != nil {
		return nil, NewError(CodeParse, "google eval: "+err.Error())
	}
	return decodeHits(obj, limit, "google")
}

func parseBing(page *rod.Page, limit int) ([]Result, error) {
	obj, err := page.Eval(`(limit) => {
  const out = [];
  const seen = new Set();
  const isAdNode = (el) => {
    while (el) {
      const id = (el.id || '').toLowerCase();
      if (id === 'b_ads' || id === 'b_pole' || id === 'b_ad' || id === 'b_topw') return true;
      const cls = ((el.className && el.className.toString && el.className.toString()) || '').toLowerCase();
      if (/\bb_ad\b|\bb_adlastchild\b|\bb_adslug\b/.test(cls)) return true;
      if (el.matches && (el.matches('li.b_ad') || el.matches('.b_ad') || el.matches('#b_ads') || el.matches('#b_pole'))) return true;
      el = el.parentElement;
    }
    return false;
  };
  const hasAdLabel = (root) => {
    if (!root) return false;
    const nodes = root.querySelectorAll('span, a, em, div, p, cite, strong');
    for (const s of nodes) {
      const t = (s.innerText || '').replace(/\s+/g, ' ').trim();
      if (t === 'Ad' || t === 'Ads' || t === 'Sponsored' || t === '赞助' || t === '广告') return true;
    }
    return false;
  };
  const items = document.querySelectorAll('li.b_algo');
  items.forEach((li) => {
    if (out.length >= limit * 2) return;
    if (isAdNode(li) || hasAdLabel(li)) return;
    const a = li.querySelector('h2 a, h2 > a, a[h]');
    if (!a) return;
    const title = (a.innerText || a.getAttribute('title') || '').trim();
    let href = (a.href || '').trim();
    let snippet = '';
    const cap = li.querySelector('.b_caption p, p.b_lineclamp2, p.b_lineclamp3, p.b_lineclamp4, .b_algoSlug, .b_dList, .b_snippet, .b_caption');
    if (cap) snippet = cap.innerText;
    if (!snippet) {
      const p = li.querySelector('p');
      if (p) snippet = p.innerText;
    }
    const cite = li.querySelector('cite');
    const citeText = cite ? cite.innerText.trim() : '';
    const key = title + '|' + href;
    if (!title || !href || seen.has(key)) return;
    seen.add(key);
    out.push({title, url: href, snippet: (snippet || '').replace(/\s+/g, ' ').trim(), cite: citeText});
  });
  if (out.length === 0) {
    document.querySelectorAll('#b_results h2 a').forEach((a) => {
      const li = a.closest('li, .b_algo, .b_ad') || a.parentElement;
      if (isAdNode(li) || hasAdLabel(li) || isAdNode(a)) return;
      const title = (a.innerText || '').trim();
      const href = (a.href || '').trim();
      if (title && href) out.push({title, url: href, snippet: ''});
    });
  }
  return out;
}`, limit)
	if err != nil {
		return nil, NewError(CodeParse, "bing eval: "+err.Error())
	}
	return decodeHits(obj, limit, "bing")
}

func parseDuckDuckGo(page *rod.Page, limit int) ([]Result, error) {
	obj, err := page.Eval(`(limit) => {
  const out = [];
  const seen = new Set();
  const isAd = (el) => {
    while (el) {
      const id = (el.id || '').toLowerCase();
      if (id === 'ads') return true;
      if (el.getAttribute && el.getAttribute('data-testid') === 'ad') return true;
      const cls = ((el.className && el.className.toString && el.className.toString()) || '');
      if (/\bresult--ad\b|\bad-result\b/.test(cls)) return true;
      if (el.matches && (el.matches('[data-testid="ad"]') || el.matches('.result--ad') || el.matches('#ads'))) return true;
      el = el.parentElement;
    }
    return false;
  };
  const push = (title, href, snippet, el) => {
    title = (title || '').trim();
    href = (href || '').trim();
    snippet = (snippet || '').replace(/\s+/g, ' ').trim();
    if (!title || !href) return;
    if (isAd(el)) return;
    if (seen.has(href)) return;
    seen.add(href);
    out.push({title, url: href, snippet});
  };
  document.querySelectorAll('article[data-testid="result"], [data-testid="mainline"] article, [data-testid="web-vertical"] article').forEach((art) => {
    const a = art.querySelector('a[data-testid="result-title-a"], h2 a, a[data-testid="result-extras-url-link"], a[href]');
    const titleEl = art.querySelector('h2, a[data-testid="result-title-a"]');
    const sn = art.querySelector('[data-result="snippet"], [data-testid="result-snippet"], article span');
    if (a) push(titleEl ? titleEl.innerText : a.innerText, a.href, sn ? sn.innerText : '', art);
  });
  document.querySelectorAll('li[data-layout="organic"], .result.results_links, .nrn-react-div article, #links .result, ol.react-results--main li, [data-testid="mainline"] li').forEach((el) => {
    const a = el.querySelector('a.result__a, h2 a, a[data-testid="result-title-a"]') || el.querySelector('a[href]');
    if (!a) return;
    const sn = el.querySelector('.result__snippet, .result__body, [data-result="snippet"]');
    push(a.innerText, a.href, sn ? sn.innerText : '', el);
  });
  return out;
}`, limit)
	if err != nil {
		return nil, NewError(CodeParse, "duckduckgo eval: "+err.Error())
	}
	return decodeHits(obj, limit, "duckduckgo")
}

func parseBaidu(page *rod.Page, limit int) ([]Result, error) {
	obj, err := page.Eval(`(limit) => {
  const out = [];
  const seen = new Set();
  const looksAd = (card) => {
    if (!card) return false;
    if (card.hasAttribute && card.hasAttribute('cmatchid')) return true;
    const cls = ((card.className && card.className.toString && card.className.toString()) || '').toLowerCase();
    if (/\bec[-_]|ec_tuiguang|c-container-ad|ad-block/.test(cls)) return true;
    const tpl = (card.getAttribute('tpl') || card.getAttribute('data-tpl') || '').toLowerCase();
    if (/adv|ad_/.test(tpl)) return true;
    if (card.querySelector('.ec-tuiguang, .ec_tuiguang, [class*="tuiguang"], .c-text-publicity, span.ec_tuiguang_ppim_becl')) return true;
    const labels = card.querySelectorAll('span, a, em');
    for (const s of labels) {
      const t = (s.innerText || '').replace(/\s+/g, '').trim();
      if (t === '广告' || t === '推广') return true;
    }
    if (card.hasAttribute && card.hasAttribute('data-click')) {
      const raw = (card.innerText || '').replace(/\s+/g, ' ').trim();
      if (/^广告(\s|·|｜|\||$)/.test(raw)) return true;
    }
    return false;
  };
  const realHref = (a, card) => {
    if (card) {
      const mu = card.getAttribute('mu') || (card.dataset && card.dataset.mu) || '';
      if (/^https?:\/\//i.test(mu) && mu.indexOf('baidu.com/link') < 0) return mu;
    }
    const land = a.getAttribute('data-landurl') || a.getAttribute('data-url') || a.getAttribute('data-href') || '';
    if (/^https?:\/\//i.test(land) && land.indexOf('baidu.com/link') < 0) return land;
    return (a.href || '').trim();
  };
  const push = (title, href, snippet) => {
    title = (title || '').trim();
    href = (href || '').trim();
    snippet = (snippet || '').replace(/\s+/g, ' ').trim();
    if (!title || !href) return;
    if (href.startsWith('javascript:')) return;
    const key = href + '|' + title;
    if (seen.has(key)) return;
    seen.add(key);
    out.push({title, url: href, snippet});
  };
  const cards = document.querySelectorAll('#content_left .result, #content_left .c-container, #content_left div[srcid], .result.c-container, div.result-op.c-container, #content_left [mu], .c-result');
  cards.forEach((card) => {
    if (out.length >= limit * 2) return;
    if (looksAd(card)) return;
    if (card.id === 'content_left') return;
    const a = card.querySelector('h3.t a, h3 a, a[data-click] h3, .t a')
      ? (card.querySelector('h3.t a') || card.querySelector('h3 a') || (card.querySelector('h3') && card.querySelector('h3').closest('a')))
      : null;
    const h3 = card.querySelector('h3.t, h3');
    if (!a || !h3) return;
    let snippet = '';
    const sn = card.querySelector('.c-abstract, .content-right_8ZsFk, span.content-right_8ZsFk, .c-span9, .c-line-clamp3, .c-line-clamp2, .c-font-normal');
    if (sn) snippet = sn.innerText;
    push(h3.innerText, realHref(a, card), snippet);
  });
  if (out.length < 3) {
    document.querySelectorAll('#content_left h3.t a, #content_left h3 a, h3.t a').forEach((a) => {
      if (out.length >= limit * 2) return;
      const card = a.closest('.result, .c-container, [srcid]') || a.parentElement;
      if (looksAd(card)) return;
      const h3 = a.querySelector('h3') || a;
      let snippet = '';
      if (card) {
        const sn = card.querySelector('.c-abstract, .content-right_8ZsFk, .c-span9');
        if (sn) snippet = sn.innerText;
      }
      push(h3.innerText || a.innerText, realHref(a, card), snippet);
    });
  }
  return out;
}`, limit)
	if err != nil {
		return nil, NewError(CodeParse, "baidu eval: "+err.Error())
	}
	hits, err := decodeHits(obj, limit, "baidu")
	if err != nil {
		return nil, err
	}
	return hits, nil
}

func decodeHits(obj *proto.RuntimeRemoteObject, limit int, engine string) ([]Result, error) {
	if obj == nil {
		return nil, NewError(CodeParse, engine+": empty eval")
	}
	var raw []rawHit
	if err := obj.Value.Unmarshal(&raw); err != nil {
		// some rod versions nest JSON
		b := []byte(obj.Value.String())
		if err2 := json.Unmarshal(b, &raw); err2 != nil {
			return nil, NewError(CodeParse, engine+": unmarshal results: "+err.Error())
		}
	}
	out := make([]Result, 0, len(raw))
	seen := map[string]bool{}
	for _, h := range raw {
		u := cleanURL(h.URL, engine)
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
	return out, nil
}

func cleanURL(raw, engine string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "javascript:") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.Path)

	if isAdOrJunkURL(u.String()) {
		return ""
	}
	if strings.Contains(path, "/pagead/") || strings.Contains(path, "/aclk") {
		return ""
	}

	if strings.Contains(host, "google.") {
		if u.Path == "/url" || u.Path == "/imgres" {
			q := u.Query()
			for _, k := range []string{"q", "url", "q"} {
				if v := q.Get(k); looksLikeAbsURL(v) {
					return cleanURL(v, engine)
				}
			}
		}
		if skipGoogleHost(host, u.Path) {
			return ""
		}
	}

	if strings.Contains(host, "bing.com") {
		if strings.HasPrefix(u.Path, "/aclick") {
			return ""
		}
		if strings.HasPrefix(u.Path, "/ck/") || strings.HasPrefix(u.Path, "/ck") {
			if v := unwrapBingCK(u); v != "" {
				return v
			}
			// keep tracking URL rather than dropping a real organic hit
			return u.String()
		}
		if u.Path == "/search" || u.Path == "/" || strings.HasPrefix(u.Path, "/images") || strings.HasPrefix(u.Path, "/videos") {
			return ""
		}
	}

	if strings.Contains(host, "duckduckgo.com") {
		if u.Path == "/l/" || u.Path == "/l" {
			if v := u.Query().Get("uddg"); v != "" {
				if dec, err := url.QueryUnescape(v); err == nil && looksLikeAbsURL(dec) {
					return dec
				}
				if looksLikeAbsURL(v) {
					return v
				}
			}
		}
		if u.Path == "/" || u.Path == "/html/" || strings.HasPrefix(u.Path, "/?q") {
			return ""
		}
	}

	if strings.Contains(host, "baidu.com") {
		if strings.Contains(host, "wappass.baidu.com") {
			return ""
		}
		if u.Path == "/link" || strings.HasPrefix(u.Path, "/link") {
			// keep the baidu redirect rather than dropping the hit;
			// HTTP unwrap happens in unwrapBaiduHits after parse.
			return u.String()
		}
		if u.Path == "/s" || u.Path == "/" || u.Path == "/baidu" || strings.HasPrefix(u.Path, "/s?") {
			return ""
		}
	}

	return u.String()
}

var baiduUnwrapClient = &http.Client{
	Timeout: 4 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func unwrapBaiduHits(hits []Result) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range hits {
		if !isBaiduLinkURL(hits[i].URL) {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if v := unwrapBaiduLink(hits[i].URL); v != "" {
				hits[i].URL = v
			}
		}(i)
	}
	wg.Wait()
}

func isBaiduLinkURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if !strings.Contains(h, "baidu.com") {
		return false
	}
	return u.Path == "/link" || strings.HasPrefix(u.Path, "/link")
}

// unwrapBaiduLink follows www.baidu.com/link?url= with a cheap HTTP HEAD/GET
// (no Chrome). Returns the destination or "" if unwrap failed.
func unwrapBaiduLink(raw string) string {
	if raw == "" {
		return ""
	}
	try := func(method string) string {
		req, err := http.NewRequest(method, raw, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
		resp, err := baiduUnwrapClient.Do(req)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		loc := resp.Header.Get("Location")
		if dest := baiduDestFromLoc(loc); dest != "" {
			return dest
		}
		if method == http.MethodGet && resp.StatusCode == 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
			return baiduDestFromHTML(string(body))
		}
		return ""
	}
	if v := try(http.MethodHead); v != "" {
		return v
	}
	return try(http.MethodGet)
}

func baiduDestFromLoc(loc string) string {
	loc = strings.TrimSpace(loc)
	if !looksLikeAbsURL(loc) {
		return ""
	}
	if isBaiduLinkURL(loc) {
		return ""
	}
	lu := strings.ToLower(loc)
	if strings.Contains(lu, "wappass.baidu.com") {
		return ""
	}
	return loc
}

func baiduDestFromHTML(html string) string {
	low := strings.ToLower(html)
	keys := []string{
		"http-equiv=\"refresh\"",
		"http-equiv='refresh'",
		"window.location.replace",
		"window.location.href",
		"window.location =",
	}
	cutset := "\"' >\n\r\t"
	for _, key := range keys {
		i := strings.Index(low, key)
		if i < 0 {
			continue
		}
		chunk := html[i:]
		if len(chunk) > 800 {
			chunk = chunk[:800]
		}
		clow := strings.ToLower(chunk)
		for _, prefix := range []string{"https://", "http://"} {
			j := strings.Index(clow, prefix)
			if j < 0 {
				continue
			}
			rest := chunk[j:]
			end := strings.IndexAny(rest, cutset)
			if end < 0 {
				end = len(rest)
			}
			cand := strings.TrimRight(rest[:end], "\";')")
			if dest := baiduDestFromLoc(cand); dest != "" {
				return dest
			}
		}
	}
	return ""
}

func skipGoogleHost(host, path string) bool {
	if strings.Contains(host, "webcache.googleusercontent.com") || strings.Contains(host, "policies.google.") {
		return true
	}
	if strings.Contains(host, "translate.google") {
		return true
	}
	switch path {
	case "/search", "/sorry/index", "/", "/webhp":
		return true
	}
	if strings.HasPrefix(path, "/aclk") || strings.Contains(path, "/aclk") || strings.Contains(path, "/pagead/") {
		return true
	}
	if strings.HasPrefix(path, "/intl/") || strings.HasPrefix(path, "/sorry") {
		return true
	}
	return false
}

func unwrapBingCK(u *url.URL) string {
	val := u.Query().Get("u")
	if val == "" {
		return ""
	}
	val = strings.TrimPrefix(val, "a1")
	val = strings.TrimPrefix(val, "a1a")
	candidates := []func(string) ([]byte, error){
		base64.RawURLEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
	}
	// pad
	padded := val
	if m := len(padded) % 4; m != 0 {
		padded += strings.Repeat("=", 4-m)
	}
	for _, dec := range candidates {
		for _, s := range []string{val, padded} {
			b, err := dec(s)
			if err != nil {
				continue
			}
			got := strings.TrimSpace(string(b))
			if looksLikeAbsURL(got) {
				return got
			}
		}
	}
	return ""
}

func looksLikeAbsURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
