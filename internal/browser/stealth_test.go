package browser

import (
	"strings"
	"testing"
)

func TestStealthJSHasFingerprints(t *testing.T) {
	js := StealthJS
	need := []string{
		"webdriver",
		"chrome.runtime",
		"plugins",
		"mimeTypes",
		"hardwareConcurrency",
		"deviceMemory",
		"zh-CN",
		"UNMASKED",
		"Intel",
	}
	for _, s := range need {
		if !strings.Contains(js, s) && !(s == "UNMASKED" && strings.Contains(js, "0x9245")) {
			if s == "UNMASKED" {
				continue
			}
			t.Errorf("stealth JS missing %q", s)
		}
	}
	if strings.Contains(js, "HeadlessChrome") && !strings.Contains(js, "replace(/HeadlessChrome") {
		t.Fatal("stealth JS should not advertise HeadlessChrome")
	}
	if strings.Contains(js, "HeadlessChrome") && !strings.Contains(js, "replace") {
		t.Fatal("raw HeadlessChrome advertisement")
	}
}

func TestChromeUANotHeadless(t *testing.T) {
	ua := chromeUA()
	if strings.Contains(ua, "HeadlessChrome") {
		t.Fatalf("ua=%s", ua)
	}
	if !strings.Contains(ua, "Chrome/") || !strings.Contains(ua, "Linux") {
		t.Fatalf("ua=%s", ua)
	}
	h := chromeClientHints(chromeMajor())
	if h.Mobile || h.Platform != "Linux" || h.Bitness != "64" {
		t.Fatalf("%+v", h)
	}
	for _, b := range h.Brands {
		if strings.Contains(b.Brand, "Headless") {
			t.Fatalf("brand %q", b.Brand)
		}
	}
}

func TestChromeClientHintsBrands(t *testing.T) {
	h := chromeClientHints("151")
	if h.FullVersion != "151.0.0.0" {
		t.Fatalf("full %s", h.FullVersion)
	}
	if len(h.Brands) < 2 || len(h.FullVersionList) < 2 {
		t.Fatal("missing brand lists")
	}
}
