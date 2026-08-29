package browser

import (
	"context"
	"fmt"
	"log"
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

// Manager owns one Chrome process and serializes page work so the
// browser is never raced by concurrent API requests.
type Manager struct {
	opts    Options
	mu      sync.Mutex
	sem     chan struct{}
	browser *rod.Browser
	launch  *launcher.Launcher
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
	return &Manager{
		opts: opts,
		sem:  make(chan struct{}, 1),
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
		Set("window-size", "1440,900").
		Set("user-agent", ua).
		Set("lang", "en-US,en").
		Env(append(os.Environ(),
			"DISPLAY="+m.opts.Display,
			"HOME="+os.Getenv("HOME"),
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

// Do serializes work on a fresh stealth page. Concurrent callers queue
// on a semaphore (one in-flight Chrome search at a time).
func (m *Manager) Do(ctx context.Context, fn func(*rod.Page) error) error {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	m.mu.Lock()
	if err := m.ensureLocked(ctx); err != nil {
		m.mu.Unlock()
		return err
	}
	b := m.browser
	m.mu.Unlock()

	// Keep at least one tab alive. Chrome may tear down the browser
	// process (and our CDP connection) if the last tab is closed.
	m.keepSpare(b)

	page, err := stealth.Page(b)
	if err != nil {
		m.mu.Lock()
		m.closeLocked()
		if err2 := m.ensureLocked(ctx); err2 != nil {
			m.mu.Unlock()
			return err2
		}
		b = m.browser
		m.mu.Unlock()
		m.keepSpare(b)
		page, err = stealth.Page(b)
		if err != nil {
			return fmt.Errorf("open page: %w", err)
		}
	}
	defer func() {
		_ = page.Close()
		m.keepSpare(b)
	}()

	page = page.Context(ctx)
	_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             1440,
		Height:            900,
		DeviceScaleFactor: 1,
		Mobile:            false,
	})
	return fn(page)
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
