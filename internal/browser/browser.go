package browser

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

// Options configure the long-lived Chromium instance.
type Options struct {
	UserDataDir string
	DebugDir    string
	Display     string
	Bin         string
}

// Manager owns one Chrome process. SERP work is capped by a small slot
// limiter (default 2); each slot uses a fresh stealth page that is closed
// after the search. Downloads use WithPage and do not take a search slot.
type Manager struct {
	opts    Options
	mu      sync.Mutex
	slots   *SlotLimiter
	browser *rod.Browser
	launch  *launcher.Launcher
	width   int
	height  int
	ua      string
}

func New(opts Options) *Manager {
	if opts.Display == "" {
		opts.Display = os.Getenv("DISPLAY")
	}
	if opts.Display == "" {
		opts.Display = ":1"
	}
	if opts.Bin == "" {
		opts.Bin = findChrome()
	}
	w := 1280 + rand.Intn(41) - 20 // 1260–1320
	h := 800 + rand.Intn(31) - 15  // 785–815
	n := slotsFromEnv()
	log.Printf("browser slots=%d", n)
	return &Manager{
		opts:   opts,
		slots:  NewSlotLimiter(n),
		width:  w,
		height: h,
	}
}

func findChrome() string {
	if v := os.Getenv("CHROME_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	for _, c := range []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
		if p, err := exec.LookPath(filepath.Base(c)); err == nil {
			return p
		}
	}
	if p, ok := launcher.LookPath(); ok && p != "" {
		return p
	}
	return ""
}

// ChromeBin returns the resolved browser binary (may be empty until Ensure).
func (m *Manager) ChromeBin() string { return m.opts.Bin }

// DebugDir is where captcha screenshots are written.
func (m *Manager) DebugDir() string { return m.opts.DebugDir }

// Ensure launches Chrome if needed (or relaunches after a crash).
func (m *Manager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLocked(ctx)
}

func (m *Manager) ensureLocked(ctx context.Context) error {
	if m.browser != nil {
		if _, err := m.browser.Version(); err == nil {
			return nil
		}
		log.Printf("browser unresponsive, relaunching: resetting session")
		m.closeLocked()
	}
	if m.opts.Bin == "" {
		m.opts.Bin = findChrome()
	}
	if m.opts.Bin == "" {
		return fmt.Errorf("chrome binary not found (install google-chrome or chromium)")
	}
	if err := os.MkdirAll(m.opts.UserDataDir, 0o700); err != nil {
		return err
	}

	ua := chromeUA()
	m.ua = ua
	if m.width <= 0 {
		m.width = 1280
	}
	if m.height <= 0 {
		m.height = 800
	}
	l := launcher.New().
		Bin(m.opts.Bin).
		UserDataDir(m.opts.UserDataDir).
		Headless(false).
		NoSandbox(true).
		Leakless(true).
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-dev-shm-usage").
		Set("enable-unsafe-swiftshader").
		Set("password-store", "basic").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-infobars").
		Set("mute-audio").
		Set("disable-features", "Translate,MediaRouter,OptimizationHints,IsolateOrigins,site-per-process").
		Set("disable-background-timer-throttling").
		Set("disable-backgrounding-occluded-windows").
		Set("disable-renderer-backgrounding").
		Set("window-size", fmt.Sprintf("%d,%d", m.width, m.height)).
		Set("user-agent", ua).
		Set("lang", "zh-CN").
		Set("accept-lang", "zh-CN,zh;q=0.9,en;q=0.8").
		Env(append(os.Environ(),
			"DISPLAY="+m.opts.Display,
			"HOME="+os.Getenv("HOME"),
			"LANG=zh_CN.UTF-8",
			"LANGUAGE=zh_CN:zh:en",
			"TZ=Asia/Shanghai",
		)...)

	// Hide the "Chrome is being controlled by automated test software" banner.
	l.Delete("enable-automation")
	l.Delete("no-startup-window")
	l.Set("excludeSwitches", "enable-automation")

	// Never bind the launcher to a per-request context: cancelling it
	// would kill the long-lived Chrome process.
	launchCtx, launchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer launchCancel()
	l = l.Context(launchCtx)

	controlURL, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch chrome (%s): %w", m.opts.Bin, err)
	}

	b := rod.New().ControlURL(controlURL).NoDefaultDevice().Context(launchCtx)
	if err := b.Connect(); err != nil {
		l.Kill()
		return fmt.Errorf("connect chrome: %w", err)
	}
	b = b.Context(context.Background())

	m.launch = l
	m.browser = b
	log.Printf("launched chrome bin=%s ua=%q display=%s", m.opts.Bin, ua, m.opts.Display)
	return nil
}

func chromeUA() string {
	// Match installed Chrome major when possible; keep a realistic desktop UA.
	major := "151"
	if out, err := exec.Command("google-chrome-stable", "--version").Output(); err == nil {
		var maj, min, build, patch int
		if _, err := fmt.Sscanf(string(out), "Google Chrome %d.%d.%d.%d", &maj, &min, &build, &patch); err == nil && maj > 0 {
			major = fmt.Sprintf("%d", maj)
		}
	}
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", major)
}

// Do runs fn on a fresh stealth page, capped by SEARCH_BROWSER_SLOTS.
// The page is closed afterwards and is never reused across users.
func (m *Manager) Do(ctx context.Context, fn func(*rod.Page) error) error {
	if err := m.slots.Acquire(ctx); err != nil {
		return err
	}
	defer m.slots.Release()
	if err := startJitter(ctx); err != nil {
		return err
	}
	return m.withPage(ctx, fn)
}

// WithPage opens a fresh stealth page without taking a search slot
// (used by download fallback so downloads are not serialized behind SERP work).
func (m *Manager) WithPage(ctx context.Context, fn func(*rod.Page) error) error {
	return m.withPage(ctx, fn)
}

func startJitter(ctx context.Context) error {
	d := time.Duration(200+rand.Intn(601)) * time.Millisecond
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (m *Manager) withPage(ctx context.Context, fn func(*rod.Page) error) error {
	m.mu.Lock()
	if err := m.ensureLocked(ctx); err != nil {
		m.mu.Unlock()
		return err
	}
	b := m.browser
	m.mu.Unlock()

	page, err := m.openFreshPage(ctx, b)
	if err != nil {
		return err
	}
	defer func() { _ = page.Close() }()
	page = page.Context(ctx)
	m.humanizePage(page)
	return fn(page)
}

func (m *Manager) openFreshPage(ctx context.Context, b *rod.Browser) (*rod.Page, error) {
	page, err := m.stealthPage(b)
	if err != nil {
		m.mu.Lock()
		m.closeLocked()
		if err2 := m.ensureLocked(ctx); err2 != nil {
			m.mu.Unlock()
			return nil, err2
		}
		b = m.browser
		m.mu.Unlock()
		page, err = m.stealthPage(b)
		if err != nil {
			return nil, fmt.Errorf("open page: %w", err)
		}
	}
	return page, nil
}

func (m *Manager) stealthPage(b *rod.Browser) (*rod.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keepSpare(b)
	page, err := stealth.Page(b)
	if err != nil {
		return nil, err
	}
	_, _ = page.EvalOnNewDocument(stealthJS)
	return page, nil
}

const stealthJS = `(() => {
  try { Object.defineProperty(navigator, 'webdriver', {get: () => undefined}); } catch (e) {}
  try { Object.defineProperty(navigator, 'languages', {get: () => ['zh-CN', 'zh', 'en-US', 'en']}); } catch (e) {}
  try { Object.defineProperty(navigator, 'language', {get: () => 'zh-CN'}); } catch (e) {}
})();`

func (m *Manager) humanizePage(page *rod.Page) {
	w, h := m.width, m.height
	if w <= 0 {
		w = 1280
	}
	if h <= 0 {
		h = 800
	}
	w += rand.Intn(17) - 8
	h += rand.Intn(13) - 6
	if w < 1100 {
		w = 1100
	}
	if h < 700 {
		h = 700
	}
	_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             w,
		Height:            h,
		DeviceScaleFactor: 1,
		Mobile:            false,
	})
	_ = proto.EmulationSetTimezoneOverride{TimezoneID: "Asia/Shanghai"}.Call(page)
	_ = proto.EmulationSetLocaleOverride{Locale: "zh-CN"}.Call(page)
	ua := m.ua
	if ua == "" {
		ua = chromeUA()
	}
	_ = proto.NetworkSetUserAgentOverride{
		UserAgent:      ua,
		AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8",
		Platform:       "Linux x86_64",
	}.Call(page)
	_, _ = page.Eval(`() => {
  try { Object.defineProperty(navigator, 'webdriver', {get: () => undefined}); } catch (e) {}
  try { Object.defineProperty(navigator, 'languages', {get: () => ['zh-CN', 'zh', 'en-US', 'en']}); } catch (e) {}
  try { Object.defineProperty(navigator, 'language', {get: () => 'zh-CN'}); } catch (e) {}
}`)
	log.Printf("humanize step=stealth viewport=%dx%d tz=Asia/Shanghai lang=zh-CN", w, h)
}

// Screenshot writes a PNG under DebugDir. Best-effort.
func (m *Manager) Screenshot(page *rod.Page, tag string) string {
	if m.opts.DebugDir == "" || page == nil {
		return ""
	}
	_ = os.MkdirAll(m.opts.DebugDir, 0o755)
	name := fmt.Sprintf("%s-%s.png", time.Now().Format("20060102-150405"), tag)
	path := filepath.Join(m.opts.DebugDir, name)
	bin, err := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
	if err != nil {
		log.Printf("screenshot failed: %v", err)
		return ""
	}
	if err := os.WriteFile(path, bin, 0o644); err != nil {
		log.Printf("screenshot write: %v", err)
		return ""
	}
	log.Printf("saved screenshot %s", path)
	return path
}

func (m *Manager) keepSpare(b *rod.Browser) {
	if b == nil {
		return
	}
	pages, err := b.Pages()
	if err == nil && len(pages) > 0 {
		return
	}
	_, _ = b.Page(proto.TargetCreateTarget{URL: "about:blank"})
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeLocked()
}

func (m *Manager) closeLocked() error {
	var err error
	if m.browser != nil {
		err = m.browser.Close()
		m.browser = nil
	}
	if m.launch != nil {
		m.launch.Kill()
		m.launch = nil
	}
	return err
}
