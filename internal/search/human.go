package search

import (
	"math/rand"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

func humanPause(minMs, maxMs int) {
	if maxMs < minMs {
		maxMs = minMs
	}
	d := minMs
	if maxMs > minMs {
		d = minMs + rand.Intn(maxMs-minMs+1)
	}
	time.Sleep(time.Duration(d) * time.Millisecond)
}

func findSearchInput(page *rod.Page) (*rod.Element, error) {
	sel := strings.Join([]string{
		`textarea[name="q"]`,
		`input[name="q"]`,
		`input[name="p"]`,
		`input[name="wd"]`,
		`textarea[name="wd"]`,
		`#kw`,
		`input#kw`,
		`#chat-textarea`,
		`textarea#chat-textarea`,
		`#index-kw`,
		`input[type="search"]`,
		`#sb_form_q`,
		`#searchbox_input`,
		`input[id="search_form_input"]`,
		`textarea[aria-label*="Search" i]`,
		`input[aria-label*="Search" i]`,
		`textarea[title="Search"]`,
		`input[title="Search"]`,
		`input[maxlength="255"]`,
	}, ", ")
	el, err := page.Timeout(8 * time.Second).Element(sel)
	if err != nil {
		return nil, err
	}
	return el, nil
}

func typeQuery(page *rod.Page, el *rod.Element, query string) error {
	_ = el.Focus()
	humanPause(80, 180)
	_ = el.SelectAllText()
	_ = page.Keyboard.Press(input.Backspace)
	humanPause(60, 140)

	for _, r := range query {
		if err := page.InsertText(string(r)); err != nil {
			// fallback: dump the rest via Input
			if err2 := el.Input(query); err2 != nil {
				return err
			}
			return nil
		}
		// Slightly slower on spaces / punctuation.
		lo, hi := 35, 110
		if r == ' ' || r == '.' || r == '-' {
			lo, hi = 70, 160
		}
		humanPause(lo, hi)
	}

	got := inputValue(el)
	if got != query {
		_ = el.SelectAllText()
		_ = page.Keyboard.Press(input.Backspace)
		if err := el.Input(query); err != nil {
			return err
		}
	}
	return nil
}

func inputValue(el *rod.Element) string {
	v, err := el.Property("value")
	if err == nil && !v.Nil() {
		s := v.Str()
		if s != "" {
			return s
		}
	}
	obj, err := el.Eval(`() => (this.value != null ? this.value : (this.textContent || ''))`)
	if err == nil && obj != nil {
		return obj.Value.Str()
	}
	return ""
}

func humanScroll(page *rod.Page) {
	_ = page.Mouse.Scroll(0, 280, 4)
	humanPause(180, 360)
	_ = page.Mouse.Scroll(0, 220, 3)
	humanPause(120, 260)
	_, _ = page.Eval(`() => { try { window.scrollBy(0, 180); } catch (e) {} }`)
}
