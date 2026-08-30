package browser

import (
	"os/exec"
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
		"Linux x86_64",
	}
	for _, s := range need {
		if !strings.Contains(js, s) {
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

func TestWebGLPatchNoIntelUHD(t *testing.T) {
	js := StealthJS
	for _, s := range []string{"Intel Inc.", "Intel(R)", "UHD Graphics", "UHD Graphics"} {
		if strings.Contains(js, s) {
			t.Fatalf("WebGL patch must not inject %q", s)
		}
	}
	if strings.Contains(js, "Intel") {
		t.Fatal("WebGL patch must not inject Intel GPU strings")
	}
	if !strings.Contains(js, "HeadlessChrome") || !strings.Contains(js, "replace") {
		t.Fatal("WebGL patch should only strip HeadlessChrome")
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
	h := chromeClientHints(chromeFullVersion(""))
	if h.Mobile || h.Platform != "Linux" || h.Bitness != "64" || h.Architecture != "x86" {
		t.Fatalf("%+v", h)
	}
	for _, b := range h.Brands {
		if strings.Contains(b.Brand, "Headless") {
			t.Fatalf("brand %q", b.Brand)
		}
	}
}

func TestChromeUAFourPartWhenParseable(t *testing.T) {
	full, ok := parseChromeVersionOutput("Google Chrome 151.0.7922.71\n")
	if !ok || full != "151.0.7922.71" {
		t.Fatalf("got %q ok=%v", full, ok)
	}
	ua := formatChromeUA(full)
	if !strings.Contains(ua, "Chrome/151.0.7922.71") {
		t.Fatalf("ua=%s", ua)
	}
	if strings.Contains(ua, "151.0.0.0") {
		t.Fatalf("four-part UA should not fall back to reduced patch: %s", ua)
	}
	if _, ok := parseChromeVersionOutput("not a chrome"); ok {
		t.Fatal("expected unparseable")
	}
	// Reduced-only input still yields a UA, but not a parsed four-part.
	if got := formatChromeUA("151"); !strings.Contains(got, "Chrome/151.0.0.0") {
		t.Fatalf("unparseable fallback ua=%s", got)
	}

	out, err := exec.Command("google-chrome-stable", "--version").Output()
	if err != nil {
		t.Skip("google-chrome-stable not installed")
	}
	host, ok := parseChromeVersionOutput(string(out))
	if !ok {
		t.Fatalf("host chrome --version not four-part: %q", out)
	}
	got := chromeUA()
	if !strings.Contains(got, "Chrome/"+host) {
		t.Fatalf("chromeUA=%s want Chrome/%s", got, host)
	}
	h := chromeClientHints(host)
	if h.FullVersion != host {
		t.Fatalf("fullVersion=%s want %s", h.FullVersion, host)
	}
	for _, b := range h.FullVersionList {
		if b.Brand == "Google Chrome" && b.Version != host {
			t.Fatalf("fullVersionList chrome=%s want %s", b.Version, host)
		}
		if b.Brand == "Chromium" && b.Version != host {
			t.Fatalf("fullVersionList chromium=%s want %s", b.Version, host)
		}
	}
}

func TestChromeClientHintsBrands(t *testing.T) {
	h := chromeClientHints("151.0.7922.71")
	if h.FullVersion != "151.0.7922.71" {
		t.Fatalf("full %s", h.FullVersion)
	}
	if len(h.Brands) < 2 || len(h.FullVersionList) < 2 {
		t.Fatal("missing brand lists")
	}
	if h.Mobile || h.Platform != "Linux" || h.Architecture != "x86" || h.Bitness != "64" {
		t.Fatalf("%+v", h)
	}
	gBrand, gMajor, gFull := greaseBrand(151)
	if gBrand != "Not=A?Brand" || gMajor != "99" || gFull != "99.0.0.0" {
		t.Fatalf("grease for 151: %s %s %s", gBrand, gMajor, gFull)
	}
	var sawGrease, sawChrome bool
	for _, b := range h.Brands {
		if b.Brand == gBrand && b.Version == gMajor {
			sawGrease = true
		}
		if b.Brand == "Google Chrome" && b.Version == "151" {
			sawChrome = true
		}
	}
	if !sawGrease || !sawChrome {
		t.Fatalf("brands=%+v", h.Brands)
	}
}
