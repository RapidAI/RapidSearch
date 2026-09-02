package api

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"search-service/internal/browser"
	"search-service/internal/search"
)

const (
	defaultChromeMinRemain = 15 * time.Second
	envQueueMax            = "SEARCH_QUEUE_MAX"
	envChromeMinRemain     = "SEARCH_CHROME_MIN_REMAIN"
)

// chromeAdmit bounds in-flight Chrome / SERP-slot work to the instance pool.
// HTTP-only engines never acquire it. Extra callers wait until a slot frees or
// the request deadline; they never start overlapping Chrome jobs beyond Cap.
type chromeAdmit struct {
	slots     chan struct{}
	n         int
	minRemain time.Duration
	maxWait   int // 0 = unlimited waiters
	mu        sync.Mutex
	waiting   int
	inflight  int
}

func newChromeAdmit(n int) *chromeAdmit {
	n = browser.ClampInstances(n)
	return &chromeAdmit{
		slots:     make(chan struct{}, n),
		n:         n,
		minRemain: parseChromeMinRemain(os.Getenv(envChromeMinRemain)),
		maxWait:   parseQueueMax(os.Getenv(envQueueMax)),
	}
}

func newTestAdmit(n, maxWait int, minRemain time.Duration) *chromeAdmit {
	n = browser.ClampInstances(n)
	return &chromeAdmit{
		slots:     make(chan struct{}, n),
		n:         n,
		minRemain: minRemain,
		maxWait:   maxWait,
	}
}

func parseQueueMax(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func parseChromeMinRemain(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultChromeMinRemain
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return defaultChromeMinRemain
}

func deadlineRemain(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(d), true
}

func (a *chromeAdmit) Cap() int {
	if a == nil {
		return 0
	}
	return a.n
}

func (a *chromeAdmit) InFlight() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inflight
}

func (a *chromeAdmit) Waiting() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waiting
}

func (a *chromeAdmit) HasFree() bool {
	if a == nil {
		return true
	}
	return len(a.slots) < a.n
}

func (a *chromeAdmit) tooLittleTime(ctx context.Context) bool {
	if a == nil || a.minRemain <= 0 {
		return false
	}
	remain, ok := deadlineRemain(ctx)
	if !ok {
		return false
	}
	return remain < a.minRemain
}

func (a *chromeAdmit) Acquire(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return search.NewError(search.CodeTimeout, "search timed out")
	}
	if a.tooLittleTime(ctx) {
		return search.NewError(search.CodeTimeout, "search timed out")
	}

	a.mu.Lock()
	if a.maxWait > 0 && len(a.slots) >= a.n && a.waiting >= a.maxWait {
		a.mu.Unlock()
		return search.NewError(search.CodeBusy, "search backend busy")
	}
	a.waiting++
	a.mu.Unlock()

	var err error
	defer func() {
		a.mu.Lock()
		a.waiting--
		if err == nil {
			a.inflight++
		}
		a.mu.Unlock()
	}()

	select {
	case a.slots <- struct{}{}:
		if a.tooLittleTime(ctx) {
			select {
			case <-a.slots:
			default:
			}
			err = search.NewError(search.CodeTimeout, "search timed out")
			return err
		}
		return nil
	case <-ctx.Done():
		err = search.NewError(search.CodeTimeout, "search timed out")
		return err
	}
}

func (a *chromeAdmit) Release() {
	if a == nil {
		return
	}
	select {
	case <-a.slots:
		a.mu.Lock()
		if a.inflight > 0 {
			a.inflight--
		}
		a.mu.Unlock()
	default:
	}
}
