package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Options configure the Chrome instance pool (shared display, debug dir,
// and base user-data directory). Each process gets chrome-profile/iN.
type Options struct {
	UserDataDir string
	DebugDir    string
	Display     string
	Bin         string
}

// chromeHost is one Chrome (or a fake in tests). The scheduler never talks
// to rod; tests inject hosts so pool logic runs without a real browser.
type chromeHost interface {
	Ensure(ctx context.Context) error
	withPage(ctx context.Context, fn func(*rod.Page) error) error
	Close() error
}

// Manager is a pool of independent headed Chrome processes. Each instance
// runs one SERP at a time. Google is serialized across the pool.
type Manager struct {
	opts       Options
	n          int
	sched      *Scheduler
	hosts      []chromeHost
	skipJitter bool
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
	n := instancesFromEnv()
	log.Printf("browser instances=%d", n)
	hosts := make([]chromeHost, n)
	for i := 0; i < n; i++ {
		io := opts
		io.UserDataDir = instanceProfile(opts.UserDataDir, i)
		hosts[i] = newInstance(i, io)
	}
	return &Manager{
		opts:  opts,
		n:     n,
		sched: NewScheduler(n),
		hosts: hosts,
	}
}

func newTestManager(n int, hosts []chromeHost) *Manager {
	n = ClampInstances(n)
	if len(hosts) != n {
		panic("newTestManager: host count must equal n")
	}
	return &Manager{
		n:          n,
		sched:      NewScheduler(n),
		hosts:      hosts,
		skipJitter: true,
	}
}

// ChromeBin returns the resolved browser binary (may be empty until Ensure).
func (m *Manager) ChromeBin() string { return m.opts.Bin }

// DebugDir is where captcha screenshots are written.
func (m *Manager) DebugDir() string { return m.opts.DebugDir }

// InstanceCount is the configured pool size (clamp 1–4).
func (m *Manager) InstanceCount() int { return m.n }

// Ensure launches instance 0 if needed. Other instances start on demand.
func (m *Manager) Ensure(ctx context.Context) error {
	if m == nil || len(m.hosts) == 0 {
		return errors.New("no chrome instances")
	}
	return m.hosts[0].Ensure(ctx)
}

// Do runs fn on a fresh stealth page on one pool instance.
// engine selects a host: Google is globally serialized and skips
// captcha-quarantined instances; other engines take any free instance.
func (m *Manager) Do(ctx context.Context, engine string, fn func(*rod.Page) error) error {
	id, err := m.sched.Acquire(ctx, engine)
	if err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			m.sched.Release(id, engine, false)
		}
	}()
	if !m.skipJitter {
		if err := startJitter(ctx); err != nil {
			return err
		}
	}
	host := m.hosts[id]
	if err := host.Ensure(ctx); err != nil {
		return err
	}
	log.Printf("browser serp instance=%d engine=%s", id, engine)
	runErr := host.withPage(ctx, fn)
	captcha := isGoogle(engine) && isCaptchaErr(runErr)
	m.sched.Release(id, engine, captcha)
	released = true
	return runErr
}

// WithPage opens a fresh stealth page without taking a search slot
// (used by download fallback so downloads are not serialized behind SERP work).
func (m *Manager) WithPage(ctx context.Context, fn func(*rod.Page) error) error {
	id := 0
	if m.sched != nil {
		id = m.sched.PickDownload()
	}
	if id < 0 || id >= len(m.hosts) {
		id = 0
	}
	host := m.hosts[id]
	if err := host.Ensure(ctx); err != nil {
		return err
	}
	return host.withPage(ctx, fn)
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

type captchaFlag interface {
	IsCaptcha() bool
}

func isCaptchaErr(err error) bool {
	if err == nil {
		return false
	}
	var c captchaFlag
	if errors.As(err, &c) {
		return c.IsCaptcha()
	}
	return false
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

func (m *Manager) Close() error {
	var first error
	for _, h := range m.hosts {
		if h == nil {
			continue
		}
		if err := h.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
