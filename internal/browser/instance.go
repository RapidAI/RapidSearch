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

// instance is one headed Chrome process. The scheduler guarantees at most
// one SERP at a time; WithPage may share the process without a SERP slot.
type instance struct {
	id      int
	opts    Options
	mu      sync.Mutex
	browser *rod.Browser
	launch  *launcher.Launcher
	width   int
	height  int
	ua      string
}

func instanceProfile(base string, id int) string {
	if base == "" {
		base = "chrome-profile"
	}
	return filepath.Join(base, fmt.Sprintf("i%d", id))
}

func newInstance(id int, opts Options) *instance {
	if opts.Display == "" {
		opts.Display = os.Getenv("DISPLAY")
	}
	if opts.Display == "" {
		opts.Display = ":1"
	}
	if opts.Bin == "" {
		opts.Bin = findChrome()
	}
	w := 1280 + rand.Intn(41) - 20 + id*16
	h := 800 + rand.Intn(31) - 15 + id*10
	if w < 1100 {
		w = 1100
	}
	if h < 700 {
		h = 700
	}
	return &instance{
		id:     id,
		opts:   opts,
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

func (in *instance) Ensure(ctx context.Context) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.ensureLocked(ctx)
}

func (in *instance) ensureLocked(ctx context.Context) error {
	if in.browser != nil {
		if _, err := in.browser.Version(); err == nil {
			return nil
		}
		log.Printf("browser instance=%d unresponsive, relaunching: resetting session", in.id)
		in.closeLocked()
	}
	if in.opts.Bin == "" {
		in.opts.Bin = findChrome()
	}
	if in.opts.Bin == "" {
		return fmt.Errorf("chrome binary not found (install google-chrome or chromium)")
	}
	if err := os.MkdirAll(in.opts.UserDataDir, 0o700); err != nil {
		return err
	}

	ua := chromeUA()
	in.ua = ua
	if in.width <= 0 {
		in.width = 1280 + in.id*16
	}
	if in.height <= 0 {
		in.height = 800 + in.id*10
	}
	l := launcher.New().
		Bin(in.opts.Bin).
		UserDataDir(in.opts.UserDataDir).
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
		Set("window-size", fmt.Sprintf("%d,%d", in.width, in.height)).
		Set("window-position", fmt.Sprintf("%d,%d", 20+in.id*48, 20+in.id*36)).
		Set("user-agent", ua).
		Set("lang", "zh-CN").
		Set("accept-lang", "zh-CN,zh;q=0.9,en;q=0.8").
		Env(append(os.Environ(),
			"DISPLAY="+in.opts.Display,
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
		return fmt.Errorf("launch chrome instance=%d (%s): %w", in.id, in.opts.Bin, err)
	}

	b := rod.New().ControlURL(controlURL).NoDefaultDevice().Context(launchCtx)
	if err := b.Connect(); err != nil {
		l.Kill()
		return fmt.Errorf("connect chrome instance=%d: %w", in.id, err)
	}
	b = b.Context(context.Background())

	in.launch = l
	in.browser = b
	log.Printf("launched chrome instance=%d bin=%s ua=%q display=%s profile=%s", in.id, in.opts.Bin, ua, in.opts.Display, in.opts.UserDataDir)
	return nil
}

func (in *instance) withPage(ctx context.Context, fn func(*rod.Page) error) error {
	in.mu.Lock()
	if err := in.ensureLocked(ctx); err != nil {
		in.mu.Unlock()
		return err
	}
	b := in.browser
	in.mu.Unlock()

	page, err := in.openFreshPage(ctx, b)
	if err != nil {
		return err
	}
	defer func() { _ = page.Close() }()
	page = page.Context(ctx)
	in.humanizePage(page)
	return fn(page)
}

func (in *instance) openFreshPage(ctx context.Context, b *rod.Browser) (*rod.Page, error) {
	page, err := in.stealthPage(b)
	if err != nil {
		in.mu.Lock()
		in.closeLocked()
		if err2 := in.ensureLocked(ctx); err2 != nil {
			in.mu.Unlock()
			return nil, err2
		}
		b = in.browser
		in.mu.Unlock()
		page, err = in.stealthPage(b)
		if err != nil {
			return nil, fmt.Errorf("open page instance=%d: %w", in.id, err)
		}
	}
	return page, nil
}

func (in *instance) stealthPage(b *rod.Browser) (*rod.Page, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.keepSpare(b)
	page, err := stealth.Page(b)
	if err != nil {
		return nil, err
	}
	_, _ = page.EvalOnNewDocument("(" + StealthJS + ")()")
	return page, nil
}

func (in *instance) humanizePage(page *rod.Page) {
	w, h := in.width, in.height
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
	ua := in.ua
	if ua == "" {
		ua = chromeUA()
	}
	_ = proto.NetworkSetUserAgentOverride{
		UserAgent:         ua,
		AcceptLanguage:    "zh-CN,zh;q=0.9,en;q=0.8",
		Platform:          "Linux x86_64",
		UserAgentMetadata: chromeClientHints(chromeMajor()),
	}.Call(page)
	_, _ = page.Eval(StealthJS)
	log.Printf("humanize step=stealth instance=%d viewport=%dx%d tz=Asia/Shanghai lang=zh-CN", in.id, w, h)
}

func (in *instance) keepSpare(b *rod.Browser) {
	if b == nil {
		return
	}
	pages, err := b.Pages()
	if err == nil && len(pages) > 0 {
		return
	}
	_, _ = b.Page(proto.TargetCreateTarget{URL: "about:blank"})
}

func (in *instance) Close() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.closeLocked()
}

func (in *instance) closeLocked() error {
	var err error
	if in.browser != nil {
		err = in.browser.Close()
		in.browser = nil
	}
	if in.launch != nil {
		in.launch.Kill()
		in.launch = nil
	}
	return err
}
