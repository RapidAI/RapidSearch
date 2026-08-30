package browser

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

const (
	viewportWidth  = 1280
	viewportHeight = 800
	fallbackMajor  = "151"
)

// StealthJS is injected on every new document. It reinforces go-rod/stealth
// and keeps the fingerprint consistent with this headed Linux Chrome (zh-CN).
// It must never advertise HeadlessChrome. WebGL must not claim a discrete GPU
// this process does not have (SwiftShader/ANGLE on the host); we only strip
// HeadlessChrome from renderer strings.
const StealthJS = `() => {
  const spoof = (obj, key, value) => {
    try {
      Object.defineProperty(obj, key, { get: () => value, configurable: true });
    } catch (e) {}
  };
  try { spoof(navigator, 'webdriver', undefined); } catch (e) {}
  try { delete Navigator.prototype.webdriver; } catch (e) {}
  spoof(navigator, 'languages', ['zh-CN', 'zh', 'en-US', 'en']);
  spoof(navigator, 'language', 'zh-CN');
  spoof(navigator, 'hardwareConcurrency', 8);
  spoof(navigator, 'deviceMemory', 8);
  spoof(navigator, 'platform', 'Linux x86_64');
  spoof(navigator, 'maxTouchPoints', 0);
  spoof(navigator, 'vendor', 'Google Inc.');
  try {
    const ua = String(navigator.userAgent || '').replace(/HeadlessChrome/g, 'Chrome');
    spoof(navigator, 'userAgent', ua);
    spoof(navigator, 'appVersion', ua.replace(/^Mozilla\//, ''));
  } catch (e) {}

  try {
    const uad = navigator.userAgentData;
    if (uad) {
      spoof(uad, 'mobile', false);
      spoof(uad, 'platform', 'Linux');
      const brands = (uad.brands || []).map((b) => ({
        brand: String(b.brand || '').replace(/HeadlessChrome/g, 'Chrome'),
        version: b.version
      }));
      spoof(uad, 'brands', brands);
    }
  } catch (e) {}

  try {
    spoof(screen, 'width', 1280);
    spoof(screen, 'height', 800);
    spoof(screen, 'availWidth', 1280);
    spoof(screen, 'availHeight', 800);
    spoof(screen, 'colorDepth', 24);
    spoof(screen, 'pixelDepth', 24);
  } catch (e) {}

  try {
    const pluginData = [
      { name: 'PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Chrome PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Chromium PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Microsoft Edge PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'WebKit built-in PDF', filename: 'internal-pdf-viewer', description: 'Portable Document Format' }
    ];
    const mime = { type: 'application/pdf', suffixes: 'pdf', description: 'Portable Document Format' };
    const plugins = pluginData.map((p, i) => {
      const plug = { name: p.name, filename: p.filename, description: p.description, length: 1, 0: mime, item: () => mime, namedItem: () => mime };
      return plug;
    });
    plugins.item = (i) => plugins[i] || null;
    plugins.namedItem = (n) => plugins.find((x) => x.name === n) || null;
    plugins.refresh = () => {};
    spoof(navigator, 'plugins', plugins);
    const mimes = [{ type: 'application/pdf', suffixes: 'pdf', description: 'Portable Document Format', enabledPlugin: plugins[0] }];
    mimes.item = (i) => mimes[i] || null;
    mimes.namedItem = (n) => mimes.find((x) => x.type === n) || null;
    spoof(navigator, 'mimeTypes', mimes);
  } catch (e) {}

  try {
    window.chrome = window.chrome || {};
    if (!window.chrome.runtime) {
      window.chrome.runtime = {
        id: undefined,
        connect: function () { return { onMessage: { addListener: function () {} }, postMessage: function () {} }; },
        sendMessage: function () {}
      };
    }
    if (!window.chrome.csi) window.chrome.csi = function () { return {}; };
    if (!window.chrome.loadTimes) window.chrome.loadTimes = function () { return {}; };
  } catch (e) {}

  const patchGL = (proto) => {
    if (!proto || !proto.getParameter) return;
    const orig = proto.getParameter;
    proto.getParameter = function (p) {
      const v = orig.apply(this, arguments);
      if (typeof v === 'string' && /HeadlessChrome/i.test(v)) {
        return v.replace(/HeadlessChrome/ig, 'Chrome');
      }
      return v;
    };
  };
  try { patchGL(window.WebGLRenderingContext && WebGLRenderingContext.prototype); } catch (e) {}
  try { patchGL(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype); } catch (e) {}

  try {
    const orig = navigator.permissions && navigator.permissions.query;
    if (orig) {
      navigator.permissions.query = (p) => (
        p && p.name === 'notifications'
          ? Promise.resolve({ state: Notification.permission })
          : orig.call(navigator.permissions, p)
      );
    }
  } catch (e) {}
}`

var greaseChars = []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
var greaseVersions = []string{"8", "99", "24"}

// parseChromeVersionOutput extracts a four-part version from `chrome --version` output.
func parseChromeVersionOutput(out string) (full string, ok bool) {
	for _, f := range strings.Fields(out) {
		f = strings.Trim(f, ",;")
		var a, b, c, d int
		n, err := fmt.Sscanf(f, "%d.%d.%d.%d", &a, &b, &c, &d)
		if err == nil && n == 4 && a > 0 {
			return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d), true
		}
	}
	return "", false
}

func normalizeChromeVersion(v string) (major, full string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallbackMajor, fallbackMajor + ".0.0.0"
	}
	parts := strings.Split(v, ".")
	if parts[0] == "" {
		return fallbackMajor, fallbackMajor + ".0.0.0"
	}
	major = parts[0]
	if len(parts) >= 4 {
		return major, strings.Join(parts[:4], ".")
	}
	return major, major + ".0.0.0"
}

func chromeFullVersion(bin string) string {
	try := []string{bin, "google-chrome-stable", "google-chrome", "chromium", "chromium-browser"}
	seen := map[string]bool{}
	for _, c := range try {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		if full, ok := parseChromeVersionOutput(string(out)); ok {
			return full
		}
	}
	return fallbackMajor + ".0.0.0"
}

func chromeMajor() string {
	major, _ := normalizeChromeVersion(chromeFullVersion(""))
	return major
}

func formatChromeUA(full string) string {
	_, full = normalizeChromeVersion(full)
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", full)
}

func chromeUA() string {
	return formatChromeUA(chromeFullVersion(findChrome()))
}

func chromeUAForBin(bin string) string {
	if bin == "" {
		bin = findChrome()
	}
	return formatChromeUA(chromeFullVersion(bin))
}

func greaseBrand(seed int) (brand, majorVer, fullVer string) {
	if seed < 0 {
		seed = 0
	}
	n := len(greaseChars)
	brand = "Not" + greaseChars[seed%n] + "A" + greaseChars[(seed+1)%n] + "Brand"
	majorVer = greaseVersions[seed%len(greaseVersions)]
	fullVer = majorVer + ".0.0.0"
	return
}

func shuffleBrandList(items [3]*proto.EmulationUserAgentBrandVersion, seed int) []*proto.EmulationUserAgentBrandVersion {
	orders := [6][3]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	if seed < 0 {
		seed = 0
	}
	order := orders[seed%6]
	out := make([]*proto.EmulationUserAgentBrandVersion, 3)
	for i, item := range items {
		out[order[i]] = item
	}
	return out
}

func chromeClientHints(full string) *proto.EmulationUserAgentMetadata {
	if strings.TrimSpace(full) == "" {
		full = chromeFullVersion("")
	}
	major, full := normalizeChromeVersion(full)
	seed := 151
	if n, err := strconv.Atoi(major); err == nil && n > 0 {
		seed = n
	}
	gBrand, gMajor, gFull := greaseBrand(seed)
	brand := func(b, v string) *proto.EmulationUserAgentBrandVersion {
		return &proto.EmulationUserAgentBrandVersion{Brand: b, Version: v}
	}
	// Chromium GenerateBrandVersionList order before shuffle: GREASE, Chromium, Google Chrome.
	brands := shuffleBrandList([3]*proto.EmulationUserAgentBrandVersion{
		brand(gBrand, gMajor),
		brand("Chromium", major),
		brand("Google Chrome", major),
	}, seed)
	fullList := shuffleBrandList([3]*proto.EmulationUserAgentBrandVersion{
		brand(gBrand, gFull),
		brand("Chromium", full),
		brand("Google Chrome", full),
	}, seed)
	return &proto.EmulationUserAgentMetadata{
		Brands:          brands,
		FullVersionList: fullList,
		FullVersion:     full,
		Platform:        "Linux",
		PlatformVersion: "",
		Architecture:    "x86",
		Model:           "",
		Mobile:          false,
		Bitness:         "64",
	}
}
