package search

import (
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

func dismissConsent(page *rod.Page) {
	// Budget a few seconds; never fail the search because of a banner.
	// Do NOT substring-match short tokens like "ok" (matches "Outlook").
	deadline := time.Now().Add(4 * time.Second)

	clickKnownButtons(page)
	clickByLabel(page)
	enterConsentIframe(page)

	for time.Now().Before(deadline) {
		clicked, err := page.Timeout(1500 * time.Millisecond).Eval(`() => {
  const exact = new Set([
    'accept all', 'accept all cookies', 'accept cookies', 'i agree', 'i accept',
    'agree', 'accept', 'allow all', 'allow all cookies', 'got it', 'ok',
    '同意', '接受全部', '全部接受', '接受', '同意全部'
  ]);
  const phrases = [
    'accept all', 'accept all cookies', 'accept cookies', 'allow all cookies',
    'i agree', 'i accept', '同意全部', '接受全部', '全部接受'
  ];
  const norm = (s) => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const clickIf = (el) => {
    const t = norm(el.innerText || el.value || el.getAttribute('aria-label') || '');
    if (!t || t.length > 48) return false;
    if (exact.has(t)) { el.click(); return true; }
    for (const p of phrases) {
      if (t.includes(p)) { el.click(); return true; }
    }
    return false;
  };
  const nodes = document.querySelectorAll('button, [role="button"], input[type="submit"], input[type="button"]');
  for (const n of nodes) {
    if (clickIf(n)) return true;
  }
  const ids = ['L2AGLb', 'bnp_btn_accept', 'onetrust-accept-btn-handler'];
  for (const id of ids) {
    const el = document.getElementById(id);
    if (el) { el.click(); return true; }
  }
  return false;
}`)
		if err == nil && clicked != nil && clicked.Value.Bool() {
			humanPause(200, 400)
			continue
		}
		break
	}
}

func clickKnownButtons(page *rod.Page) {
	sels := []string{
		`#L2AGLb`,
		`button#L2AGLb`,
		`button[aria-label="Accept all"]`,
		`button[aria-label="Accept all cookies"]`,
		`#bnp_btn_accept`,
		`button#onetrust-accept-btn-handler`,
	}
	for _, sel := range sels {
		el, err := page.Timeout(300 * time.Millisecond).Element(sel)
		if err != nil {
			continue
		}
		_ = el.Click(proto.InputMouseButtonLeft, 1)
		humanPause(120, 240)
		return
	}
}

func clickByLabel(page *rod.Page) {
	els, err := page.Timeout(400 * time.Millisecond).Elements("button, [role=button]")
	if err != nil {
		return
	}
	for _, el := range els {
		t, _ := el.Text()
		t = strings.ToLower(strings.Join(strings.Fields(t), " "))
		if t == "accept all" || t == "i agree" || t == "accept" || t == "同意" || t == "accept all cookies" || strings.Contains(t, "accept all") {
			_ = el.Click(proto.InputMouseButtonLeft, 1)
			return
		}
	}
}

func enterConsentIframe(page *rod.Page) {
	iframe, err := page.Timeout(500 * time.Millisecond).Element(`iframe[src*="consent"], iframe[src*="store.google.com"], iframe[title*="consent" i]`)
	if err != nil {
		return
	}
	frame, err := iframe.Frame()
	if err != nil {
		return
	}
	el, err := frame.Timeout(800 * time.Millisecond).Element(`#L2AGLb, button[aria-label="Accept all"]`)
	if err != nil {
		return
	}
	_ = el.Click(proto.InputMouseButtonLeft, 1)
}
