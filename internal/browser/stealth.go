package browser

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

// StealthJS is injected on every new document. It reinforces go-rod/stealth
// and keeps the fingerprint consistent with a headed Linux Chrome (zh-CN).
// It must never advertise HeadlessChrome. SwiftShader/llvmpipe cannot be
// swapped for a real GPU; we only rewrite those strings.
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
      const VENDOR = 0x9245, RENDERER = 0x9246;
      if (p === VENDOR) return 'Intel Inc.';
      if (p === RENDERER) return 'Mesa Intel(R) UHD Graphics (CML GT2)';
      const v = orig.apply(this, arguments);
      if (typeof v === 'string' && /HeadlessChrome|SwiftShader|llvmpipe/i.test(v)) {
        return v.replace(/HeadlessChrome/ig, 'Chrome').replace(/SwiftShader/ig, 'UHD Graphics').replace(/llvmpipe/ig, 'UHD Graphics');
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

func chromeMajor() string {
	major := "151"
	if out, err := exec.Command("google-chrome-stable", "--version").Output(); err == nil {
		var maj, min, build, patch int
		if _, err := fmt.Sscanf(string(out), "Google Chrome %d.%d.%d.%d", &maj, &min, &build, &patch); err == nil && maj > 0 {
			major = fmt.Sprintf("%d", maj)
		}
	} else if out, err := exec.Command("chromium", "--version").Output(); err == nil {
		s := string(out)
		for _, prefix := range []string{"Chromium ", "Chrome "} {
			if i := strings.Index(s, prefix); i >= 0 {
				var maj int
				if _, err := fmt.Sscanf(s[i+len(prefix):], "%d", &maj); err == nil && maj > 0 {
					major = fmt.Sprintf("%d", maj)
				}
			}
		}
	}
	return major
}

func chromeUA() string {
	major := chromeMajor()
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", major)
}

func chromeClientHints(major string) *proto.EmulationUserAgentMetadata {
	if major == "" {
		major = chromeMajor()
	}
	full := major + ".0.0.0"
	brand := func(b, v string) *proto.EmulationUserAgentBrandVersion {
		return &proto.EmulationUserAgentBrandVersion{Brand: b, Version: v}
	}
	return &proto.EmulationUserAgentMetadata{
		Brands: []*proto.EmulationUserAgentBrandVersion{
			brand("Not:A-Brand", "99"),
			brand("Google Chrome", major),
			brand("Chromium", major),
		},
		FullVersionList: []*proto.EmulationUserAgentBrandVersion{
			brand("Not:A-Brand", "10.0.0.4"),
			brand("Google Chrome", full),
			brand("Chromium", full),
		},
		FullVersion:     full,
		Platform:        "Linux",
		PlatformVersion: "6.12.0",
		Architecture:    "x86",
		Model:           "",
		Mobile:          false,
		Bitness:         "64",
	}
}
