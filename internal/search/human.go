package search

import (
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
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

// keyDelayRange is 60–180ms, slower on space/punct.
func keyDelayRange(r rune) (lo, hi int) {
	lo, hi = 60, 180
	if r == ' ' || r == '.' || r == '-' || r == ',' {
		return 90, 220
	}
	return lo, hi
}

func shouldLongPause() bool {
	return rand.Intn(100) < 12 // ~12% mid-word think
}

func shouldTypo() bool {
	return rand.Intn(100) < 4 // rare
}

func typoNear(r rune) rune {
	if r >= 'a' && r <= 'z' {
		adj := []rune{r - 1, r + 1, r}
		return adj[rand.Intn(len(adj))]
	}
	if r >= 'A' && r <= 'Z' {
		adj := []rune{r - 1, r + 1, r}
		return adj[rand.Intn(len(adj))]
	}
	return r
}

func typeQuery(page *rod.Page, el *rod.Element, query string) error {
	log.Printf("humanize step=type chars=%d", len([]rune(query)))
	_ = el.Focus()
	humanPause(80, 180)
	_ = el.SelectAllText()
	_ = page.Keyboard.Press(input.Backspace)
	humanPause(60, 140)

	runes := []rune(query)
	for i, r := range runes {
		if shouldTypo() && unicode.IsLetter(r) {
			wrong := typoNear(r)
			if wrong != r {
				log.Printf("humanize step=typo")
				if err := page.InsertText(string(wrong)); err != nil {
					if err2 := el.Input(query); err2 != nil {
						return err
					}
					return nil
				}
				humanPause(80, 160)
				_ = page.Keyboard.Press(input.Backspace)
				humanPause(50, 120)
			}
		}
		if err := page.InsertText(string(r)); err != nil {
			if err2 := el.Input(query); err2 != nil {
				return err
			}
			return nil
		}
		lo, hi := keyDelayRange(r)
		humanPause(lo, hi)
		if shouldLongPause() && i+1 < len(runes) && unicode.IsLetter(runes[i+1]) {
			log.Printf("humanize step=type-pause")
			humanPause(280, 700)
		}
	}

	got := inputValue(el)
	if got != query {
		// Last-resort correction only — never the primary path.
		log.Printf("humanize step=type-correct")
		_ = el.SelectAllText()
		_ = page.Keyboard.Press(input.Backspace)
		for _, r := range query {
			if err := page.InsertText(string(r)); err != nil {
				if err2 := el.Input(query); err2 != nil {
					return err
				}
				return nil
			}
			humanPause(40, 90)
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

type box struct{ X, Y, W, H float64 }

func elementBox(el *rod.Element) (box, error) {
	obj, err := el.Eval(`() => {
  const r = this.getBoundingClientRect();
  return {x:r.x, y:r.y, w:r.width, h:r.height};
}`)
	if err != nil || obj == nil {
		return box{}, err
	}
	var b box
	if err := obj.Value.Unmarshal(&b); err != nil {
		return box{}, err
	}
	return b, nil
}

func humanMoveTo(page *rod.Page, x, y float64) {
	from := page.Mouse.Position()
	steps := 8 + rand.Intn(8) // 8–15
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		// slight arc
		jx := (rand.Float64() - 0.5) * 3
		jy := (rand.Float64() - 0.5) * 3
		nx := from.X + (x-from.X)*t + jx
		ny := from.Y + (y-from.Y)*t + jy
		_ = page.Mouse.MoveTo(proto.Point{X: nx, Y: ny})
		humanPause(8, 22)
	}
	_ = page.Mouse.MoveTo(proto.Point{X: x, Y: y})
}

func humanClick(page *rod.Page, el *rod.Element) error {
	_ = el.ScrollIntoView()
	humanPause(80, 180)
	b, err := elementBox(el)
	if err != nil || b.W < 2 || b.H < 2 {
		log.Printf("humanize step=click-fallback")
		if err2 := el.Click(proto.InputMouseButtonLeft, 1); err2 != nil {
			_ = el.Focus()
			return err2
		}
		return nil
	}
	jx := (rand.Float64() - 0.5) * minFloat(b.W*0.35, 10)
	jy := (rand.Float64() - 0.5) * minFloat(b.H*0.35, 6)
	x := b.X + b.W/2 + jx
	y := b.Y + b.H/2 + jy
	log.Printf("humanize step=mouse-move")
	humanMoveTo(page, x, y)
	if rand.Intn(100) < 40 {
		log.Printf("humanize step=hover")
		humanPause(120, 380)
	} else {
		humanPause(40, 120)
	}
	if err := page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		_ = el.Focus()
		return err
	}
	return nil
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func humanScroll(page *rod.Page) {
	// 2–3 small reading-style steps (never one big jump).
	steps := 2 + rand.Intn(2)
	log.Printf("humanize step=scroll steps=%d", steps)
	for i := 0; i < steps; i++ {
		dy := 140 + rand.Intn(120)
		_ = page.Mouse.Scroll(0, float64(dy), 3+rand.Intn(3))
		humanPause(220, 520)
	}
}

func waitIdleIsh(page *rod.Page) {
	log.Printf("humanize step=wait-idle")
	_ = page.Timeout(6 * time.Second).WaitIdle(1200 * time.Millisecond)
	humanPause(200, 450)
}

var (
	googleMu    sync.Mutex
	googleFirst = true
)

func warmGoogleHomepage(page *rod.Page) {
	googleMu.Lock()
	first := googleFirst
	googleFirst = false
	googleMu.Unlock()
	if first {
		log.Printf("humanize step=warm-idle")
		humanPause(1000, 2000)
	}
}

// stealthEval spoofs webdriver / languages on the current document.
const stealthEval = `() => {
  try {
    Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
  } catch (e) {}
  try {
    Object.defineProperty(navigator, 'languages', {get: () => ['zh-CN', 'zh', 'en-US', 'en']});
  } catch (e) {}
  try {
    Object.defineProperty(navigator, 'language', {get: () => 'zh-CN'});
  } catch (e) {}
  try {
    const orig = navigator.permissions && navigator.permissions.query;
    if (orig) {
      navigator.permissions.query = (p) => (
        p && p.name === 'notifications'
          ? Promise.resolve({state: Notification.permission})
          : orig(p)
      );
    }
  } catch (e) {}
}`

func applyDocumentStealth(page *rod.Page) {
	_, _ = page.Eval(stealthEval)
}
