package search

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// Result is one organic hit.
type Result struct {
	Rank      int     `json:"rank"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Snippet   string  `json:"snippet"`
	Content   string  `json:"content,omitempty"`
	Relevance float64 `json:"relevance"`
}

// Error is a classified search failure.
type Error struct {
	Msg  string
	Code string
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) IsCaptcha() bool { return e != nil && e.Code == CodeCaptcha }

func NewError(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

func Is(err error, code string) bool {
	if e, ok := err.(*Error); ok {
		return e.Code == code
	}
	return false
}

func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return "parse"
}

const (
	CodeCaptcha      = "captcha"
	CodeTimeout      = "timeout"
	CodeParse        = "parse"
	CodeBadRequest   = "bad_request"
	CodeEngine       = "engine"
	CodeOffline      = "offline"
	CodeUnauthorized = "unauthorized"
	CodeBusy         = "busy"
)

// Engine metadata.
type Engine struct {
	Name     string
	HomeURL  string
	WaitSel  string
	Parse    func(*rod.Page, int) ([]Result, error)
	SERPURL  func(query string) string // if set, navigate directly to the SERP
	HTTPOnly bool                      // HTML GET; must not take a Chrome SERP slot
}

var engines = map[string]Engine{
	"google": {
		Name:    "google",
		HomeURL: "https://www.google.com/",
		WaitSel: `#search h3, #rso h3, div#center_col h3, #captcha-form, form#captcha-form, #recaptcha, iframe[src*="recaptcha"]`,
		Parse:   parseGoogle,
	},
	"bing": {
		Name:    "bing",
		HomeURL: "https://www.bing.com/",
		WaitSel: `li.b_algo, #b_results h2, #captcha-form, #b_captcha`,
		Parse:   parseBing,
		SERPURL: func(q string) string { return engineSERPURL("bing", q) },
	},
	"duckduckgo": {
		Name:    "duckduckgo",
		HomeURL: "https://duckduckgo.com/",
		WaitSel: `article[data-testid="result"], [data-testid="mainline"], [data-testid="web-vertical"], li[data-layout="organic"], #web_content_wrapper, .result, #links .result, ol.react-results--main`,
		Parse:   parseDuckDuckGo,
	},
	"duckduckgo_html": {
		Name:     "duckduckgo_html",
		HomeURL:  "https://html.duckduckgo.com/html/",
		HTTPOnly: true,
	},
	"baidu": {
		Name:    "baidu",
		HomeURL: "https://www.baidu.com/",
		WaitSel: `#content_left .result, #content_left .c-container, h3.t a, #content_left, .result-op, #captcha, #wappass, iframe[src*="wappass"]`,
		Parse:   parseBaidu,
		SERPURL: func(q string) string { return engineSERPURL("baidu", q) },
	},
	"sogou": {
		Name:    "sogou",
		HomeURL: "https://www.sogou.com/",
		WaitSel: `h3 a, .vrwrap, .rb a, #main, #captcha, .auth-box`,
		Parse:   parseSogou,
		SERPURL: func(q string) string { return engineSERPURL("sogou", q) },
	},
	"360": {
		Name:    "360",
		HomeURL: "https://www.so.com/",
		WaitSel: `#main .res-list, #main h3 a, ul.result li, .res-list, #side, .e_idea`,
		Parse:   parse360,
		SERPURL: func(q string) string { return engineSERPURL("360", q) },
	},
}

// NormalizeEngine maps user input to auto or a concrete engine name.
// Empty engine means auto (China vs global routing happens in Schedule).
func NormalizeEngine(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "auto" {
		return "auto", nil
	}
	switch s {
	case "ddg", "duck":
		s = "duckduckgo"
	case "ddg_html", "duck_html", "duckduckgo-html":
		s = "duckduckgo_html"
	case "bd":
		s = "baidu"
	case "so360", "so.com", "so", "qihoo", "qihoo360":
		s = "360"
	}
	if _, ok := engines[s]; !ok {
		return "", NewError(CodeEngine, fmt.Sprintf("unsupported engine %q (use auto, google, bing, baidu, duckduckgo, duckduckgo_html, sogou, 360)", s))
	}
	return s, nil
}

// concreteEngine rejects auto; Run() needs a real SERP engine.
func concreteEngine(s string) (string, error) {
	name, err := NormalizeEngine(s)
	if err != nil {
		return "", err
	}
	if name == "auto" {
		return "", NewError(CodeEngine, "engine auto must be resolved by the scheduler")
	}
	return name, nil
}

func ClampLimit(n int) int {
	if n <= 0 {
		return 10
	}
	if n > 20 {
		return 20
	}
	return n
}

// Run drives a human-like search on an already-open stealth page.
func Run(page *rod.Page, engineName, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, NewError(CodeBadRequest, "query is required")
	}
	engName, err := concreteEngine(engineName)
	if err != nil {
		return nil, err
	}
	limit = ClampLimit(limit)
	eng := engines[engName]
	if eng.HTTPOnly || eng.Parse == nil {
		return nil, NewError(CodeEngine, engName+" is HTTP-only; use RunHTTP")
	}

	PaceEngine(engName)
	applyDocumentStealth(page)

	if eng.SERPURL != nil {
		return runDirectSERP(page, eng, query, limit)
	}

	if err := navigateHome(page, eng.HomeURL, eng.Name); err != nil {
		if isTimeout(err) {
			return nil, NewError(CodeTimeout, "timeout loading "+eng.Name+" homepage")
		}
		return nil, wrap(err)
	}
	log.Printf("search step=home engine=%s url=%s", eng.Name, pageURL(page))
	if engName == "google" {
		warmGoogleHomepage(page)
	}

	log.Printf("humanize step=pre-search-pause")
	humanPause(800, 2500)
	dismissConsent(page)
	log.Printf("search step=consent engine=%s url=%s", eng.Name, pageURL(page))

	if !onEngineHost(page, eng.Name) {
		log.Printf("search step=off-host engine=%s url=%s; retrying homepage", eng.Name, pageURL(page))
		if err := navigateHome(page, eng.HomeURL, eng.Name); err != nil {
			return nil, wrap(err)
		}
		humanPause(300, 600)
		dismissConsent(page)
	}

	if blocked, why := detectBlock(page); blocked {
		return nil, NewError(CodeCaptcha, why)
	}

	inputEl, err := findSearchInput(page)
	if err != nil {
		if blocked, why := detectBlock(page); blocked {
			return nil, NewError(CodeCaptcha, why)
		}
		log.Printf("search step=no-input engine=%s url=%s err=%v", eng.Name, pageURL(page), err)
		return nil, NewError(CodeParse, "could not find search input on "+eng.Name)
	}

	if err := humanClick(page, inputEl); err != nil {
		_ = inputEl.Focus()
	}
	humanPause(120, 280)

	if err := typeQuery(page, inputEl, query); err != nil {
		if isTimeout(err) {
			return nil, NewError(CodeTimeout, "timeout typing query")
		}
		return nil, wrap(err)
	}
	humanPause(180, 350)

	if err := page.Keyboard.Press(input.Enter); err != nil {
		return nil, wrap(err)
	}
	log.Printf("search step=submitted engine=%s url=%s", eng.Name, pageURL(page))

	waitIdleIsh(page)
	if err := waitResults(page, eng.WaitSel); err != nil {
		if blocked, why := detectBlock(page); blocked {
			return nil, NewError(CodeCaptcha, why)
		}
		if isTimeout(err) {
			return nil, NewError(CodeTimeout, "timeout waiting for "+eng.Name+" results")
		}
		return nil, wrap(err)
	}
	log.Printf("search step=results engine=%s url=%s", eng.Name, pageURL(page))

	humanPause(200, 400)
	humanScroll(page)
	humanPause(250, 450)

	if blocked, why := detectBlock(page); blocked {
		return nil, NewError(CodeCaptcha, why)
	}

	results, err := eng.Parse(page, limit)
	if err != nil {
		return nil, wrap(err)
	}
	if len(results) == 0 {
		if blocked, why := detectBlock(page); blocked {
			return nil, NewError(CodeCaptcha, why)
		}
		return nil, NewError(CodeParse, "no organic results parsed from "+eng.Name)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results, nil
}

func pageURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}

func onEngineHost(page *rod.Page, engine string) bool {
	u, err := url.Parse(pageURL(page))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	switch engine {
	case "google":
		return strings.Contains(h, "google.")
	case "bing":
		return strings.Contains(h, "bing.com")
	case "duckduckgo", "duckduckgo_html":
		return strings.Contains(h, "duckduckgo.com")
	case "baidu":
		return strings.Contains(h, "baidu.com")
	case "sogou":
		return strings.Contains(h, "sogou.com")
	case "360":
		return h == "so.com" || strings.HasSuffix(h, ".so.com")
	default:
		return true
	}
}

func runDirectSERP(page *rod.Page, eng Engine, query string, limit int) ([]Result, error) {
	dest := eng.SERPURL(query)
	if err := page.Timeout(20 * time.Second).Navigate(dest); err != nil {
		if isTimeout(err) {
			return nil, NewError(CodeTimeout, "timeout loading "+eng.Name+" results")
		}
		return nil, wrap(err)
	}
	_ = page.Timeout(8 * time.Second).WaitLoad()
	applyDocumentStealth(page)
	log.Printf("search step=serp engine=%s url=%s", eng.Name, pageURL(page))
	waitIdleIsh(page)
	dismissConsent(page)
	if blocked, why := detectBlock(page); blocked {
		return nil, NewError(CodeCaptcha, why)
	}
	if err := waitResults(page, eng.WaitSel); err != nil {
		if blocked, why := detectBlock(page); blocked {
			return nil, NewError(CodeCaptcha, why)
		}
		if isTimeout(err) {
			return nil, NewError(CodeTimeout, "timeout waiting for "+eng.Name+" results")
		}
		return nil, wrap(err)
	}
	log.Printf("search step=results engine=%s url=%s", eng.Name, pageURL(page))
	humanPause(200, 400)
	humanScroll(page)
	humanPause(250, 450)
	if blocked, why := detectBlock(page); blocked {
		return nil, NewError(CodeCaptcha, why)
	}
	results, err := eng.Parse(page, limit)
	if err != nil {
		return nil, wrap(err)
	}
	if len(results) == 0 {
		if blocked, why := detectBlock(page); blocked {
			return nil, NewError(CodeCaptcha, why)
		}
		return nil, NewError(CodeParse, "no organic results parsed from "+eng.Name)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results, nil
}

func navigateHome(page *rod.Page, home, engine string) error {
	if onEngineHost(page, engine) {
		if blocked, why := detectBlock(page); blocked {
			log.Printf("humanize step=skip-reuse engine=%s reason=%s", engine, why)
		} else if _, err := findSearchInput(page); err == nil {
			log.Printf("humanize step=reuse-host engine=%s url=%s", engine, pageURL(page))
			return nil
		}
	}
	if err := page.Timeout(20 * time.Second).Navigate(home); err != nil {
		return err
	}
	_ = page.Timeout(8 * time.Second).WaitLoad()
	applyDocumentStealth(page)
	humanPause(300, 600)
	return nil
}

func waitResults(page *rod.Page, sel string) error {
	_, err := page.Timeout(20 * time.Second).Element(sel)
	if err != nil {
		return err
	}
	humanPause(350, 650)
	return nil
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	if isTimeout(err) {
		return NewError(CodeTimeout, err.Error())
	}
	return NewError(CodeParse, err.Error())
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "context canceled") || strings.Contains(s, "context cancelled")
}
