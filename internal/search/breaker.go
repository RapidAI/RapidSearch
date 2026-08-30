package search

import (
	"log"
	"strings"
	"sync"
	"time"
)

// DefaultGoogleBreakerCooldown is how long auto/fallback chains skip Google
// after a captcha (or no healthy Google Chrome instance).
const DefaultGoogleBreakerCooldown = 15 * time.Minute

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// GoogleBreaker is a process-wide circuit around Google. Per-instance
// quarantine still exists; this stops auto mode from burning ~40s on a
// datacenter-wide captcha before Bing.
type GoogleBreaker struct {
	mu        sync.Mutex
	cooldown  time.Duration
	now       func() time.Time
	openUntil time.Time
	probeHeld bool
}

func NewGoogleBreaker() *GoogleBreaker {
	return &GoogleBreaker{
		cooldown: DefaultGoogleBreakerCooldown,
		now:      time.Now,
	}
}

var defaultGoogleBreaker = NewGoogleBreaker()

// DefaultGoogleBreaker is the process-wide instance used by search-service.
func DefaultGoogleBreaker() *GoogleBreaker { return defaultGoogleBreaker }

func (b *GoogleBreaker) clock() time.Time {
	if b == nil || b.now == nil {
		return time.Now()
	}
	return b.now()
}

func (b *GoogleBreaker) cooldownOrDefault() time.Duration {
	if b == nil || b.cooldown <= 0 {
		return DefaultGoogleBreakerCooldown
	}
	return b.cooldown
}

func (b *GoogleBreaker) stateLocked(now time.Time) breakerState {
	if b.openUntil.IsZero() {
		return breakerClosed
	}
	if now.Before(b.openUntil) {
		return breakerOpen
	}
	return breakerHalfOpen
}

// Trip opens the breaker for the cooldown. Auto chains skip Google.
func (b *GoogleBreaker) Trip() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openUntil = b.clock().Add(b.cooldownOrDefault())
	b.probeHeld = false
	log.Printf("google breaker=open cooldown=%s", b.cooldownOrDefault())
}

// Observe records a Google attempt. Success closes the breaker. Captcha
// (including mapped ErrNoGoogleInstance) reopens it. Other errors just
// release a half-open probe slot.
func (b *GoogleBreaker) Observe(engine string, err error) {
	if b == nil || !isGoogleName(engine) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeHeld = false
	if err == nil {
		if !b.openUntil.IsZero() {
			log.Printf("google breaker=closed")
		}
		b.openUntil = time.Time{}
		return
	}
	if Is(err, CodeCaptcha) {
		b.openUntil = b.clock().Add(b.cooldownOrDefault())
		log.Printf("google breaker=open cooldown=%s", b.cooldownOrDefault())
	}
}

// ChainPlan is the breaker-adjusted engine list for one request.
type ChainPlan struct {
	Engines  []string
	Skipped  []string
	FailFast bool // explicit google while open: do not wait on Chrome
}

// Apply rewrites a scheduled chain.
//
// Open: auto / fallback drop Google. Explicit engine=google with no
// fallback keeps Google but FailFast (code=captcha, no 40s wait).
// Half-open: exactly one in-flight Google probe; other requests skip it.
func (b *GoogleBreaker) Apply(requested string, chain []string) ChainPlan {
	if len(chain) == 0 {
		return ChainPlan{}
	}
	if b == nil {
		return ChainPlan{Engines: append([]string(nil), chain...)}
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	b.mu.Lock()
	defer b.mu.Unlock()

	if !containsEngine(chain, "google") {
		return ChainPlan{Engines: append([]string(nil), chain...)}
	}

	now := b.clock()
	switch b.stateLocked(now) {
	case breakerClosed:
		return ChainPlan{Engines: append([]string(nil), chain...)}
	case breakerOpen:
		explicitOnly := requested == "google" && len(chain) == 1 && chain[0] == "google"
		if explicitOnly {
			return ChainPlan{Engines: []string{"google"}, FailFast: true}
		}
		return ChainPlan{Engines: dropEngine(chain, "google"), Skipped: []string{"google"}}
	default: // half-open
		if b.probeHeld {
			return ChainPlan{Engines: dropEngine(chain, "google"), Skipped: []string{"google"}}
		}
		b.probeHeld = true
		return ChainPlan{Engines: append([]string(nil), chain...)}
	}
}

// Open reports whether Google is currently skipped for auto (not half-open).
func (b *GoogleBreaker) Open() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(b.clock()) == breakerOpen
}

func isGoogleName(engine string) bool {
	return strings.ToLower(strings.TrimSpace(engine)) == "google"
}

func containsEngine(chain []string, name string) bool {
	for _, e := range chain {
		if e == name {
			return true
		}
	}
	return false
}

func dropEngine(chain []string, name string) []string {
	out := make([]string, 0, len(chain))
	for _, e := range chain {
		if e != name {
			out = append(out, e)
		}
	}
	return out
}
