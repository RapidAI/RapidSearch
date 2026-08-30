package search

import (
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// enginePaceRange is the minimum gap between navigations to one engine.
func enginePaceRange(engine string) (min, max time.Duration) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "google":
		return 8 * time.Second, 15 * time.Second
	default:
		return 1500 * time.Millisecond, 4 * time.Second
	}
}

func randDuration(min, max time.Duration) time.Duration {
	if max < min {
		max = min
	}
	if max == min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)+1))
}

// EnginePacer spaces navigations to the same engine across browser slots.
type EnginePacer struct {
	mu     sync.Mutex
	last   map[string]time.Time
	now    func() time.Time
	sleep  func(time.Duration)
	jitter func(min, max time.Duration) time.Duration
}

func NewEnginePacer() *EnginePacer {
	return &EnginePacer{
		last:   make(map[string]time.Time),
		now:    time.Now,
		sleep:  time.Sleep,
		jitter: randDuration,
	}
}

var defaultPacer = NewEnginePacer()

// PaceEngine waits so two slots cannot burst the same engine.
func PaceEngine(engine string) {
	defaultPacer.Wait(engine)
}

func (p *EnginePacer) Wait(engine string) {
	if p == nil {
		return
	}
	engine = strings.ToLower(strings.TrimSpace(engine))
	if engine == "" {
		return
	}
	min, max := enginePaceRange(engine)
	gap := min
	if p.jitter != nil {
		gap = p.jitter(min, max)
	}
	nowFn := p.now
	if nowFn == nil {
		nowFn = time.Now
	}
	sleepFn := p.sleep
	if sleepFn == nil {
		sleepFn = time.Sleep
	}

	p.mu.Lock()
	now := nowFn()
	last := p.last[engine]
	sleepUntil := now
	if !last.IsZero() {
		target := last.Add(gap)
		if target.After(now) {
			sleepUntil = target
		}
	}
	p.last[engine] = sleepUntil
	p.mu.Unlock()

	if d := sleepUntil.Sub(now); d > 0 {
		log.Printf("humanize step=pace engine=%s wait_ms=%d", engine, d.Milliseconds())
		sleepFn(d)
	}
}
