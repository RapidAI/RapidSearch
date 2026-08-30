package browser

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const DefaultGoogleQuarantine = 10 * time.Minute

// ErrNoGoogleInstance means every Chrome in the pool is quarantined from Google.
// Callers should failover to another engine.
var ErrNoGoogleInstance = errors.New("all chrome instances quarantined from google")

// Scheduler assigns SERP work to Chrome instances without talking to Chrome.
// Tests drive it with fake clocks; the pool uses it to pick processes.
type Scheduler struct {
	n           int
	mu          sync.Mutex
	cond        *sync.Cond
	busy        []bool
	googleUntil []time.Time
	googleHeld  int
	quarantine  time.Duration
	now         func() time.Time
}

func NewScheduler(n int) *Scheduler {
	n = ClampInstances(n)
	s := &Scheduler{
		n:           n,
		busy:        make([]bool, n),
		googleUntil: make([]time.Time, n),
		googleHeld:  -1,
		quarantine:  DefaultGoogleQuarantine,
		now:         time.Now,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Scheduler) Cap() int {
	if s == nil {
		return 0
	}
	return s.n
}

func normalizeEngine(engine string) string {
	return strings.ToLower(strings.TrimSpace(engine))
}

func isGoogle(engine string) bool {
	return normalizeEngine(engine) == "google"
}

// Acquire reserves one instance for a SERP. Google is globally serialized and
// skips quarantined instances. Non-Google takes any free instance.
func (s *Scheduler) Acquire(ctx context.Context, engine string) (int, error) {
	if s == nil || s.n == 0 {
		return -1, errors.New("no chrome instances")
	}
	google := isGoogle(engine)

	s.mu.Lock()
	defer s.mu.Unlock()

	stop := context.AfterFunc(ctx, func() {
		s.cond.Broadcast()
	})
	defer stop()

	for {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		now := s.clock()
		if google {
			if s.allGoogleQuarantined(now) {
				return -1, ErrNoGoogleInstance
			}
			if s.googleHeld < 0 {
				if id, ok := s.pickFree(now, true); ok {
					s.busy[id] = true
					s.googleHeld = id
					return id, nil
				}
			}
		} else if id, ok := s.pickFree(now, false); ok {
			s.busy[id] = true
			return id, nil
		}
		s.cond.Wait()
	}
}

func (s *Scheduler) Release(id int, engine string, captcha bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 0 || id >= s.n {
		return
	}
	s.busy[id] = false
	if isGoogle(engine) && s.googleHeld == id {
		s.googleHeld = -1
	}
	if captcha && isGoogle(engine) {
		until := s.clock().Add(s.quarantine)
		s.googleUntil[id] = until
		log.Printf("browser instance=%d google-quarantine=%s", id, s.quarantine)
	}
	s.cond.Broadcast()
}

// PickDownload returns an instance for WithPage without taking a SERP slot.
// Prefers an idle instance; if all are in a SERP, returns 0 (shared path).
func (s *Scheduler) PickDownload() int {
	if s == nil || s.n == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.n; i++ {
		if !s.busy[i] {
			return i
		}
	}
	return 0
}

func (s *Scheduler) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Scheduler) allGoogleQuarantined(now time.Time) bool {
	if s.n == 0 {
		return true
	}
	for i := 0; i < s.n; i++ {
		if !now.Before(s.googleUntil[i]) {
			return false
		}
	}
	return true
}

func (s *Scheduler) pickFree(now time.Time, google bool) (int, bool) {
	for i := 0; i < s.n; i++ {
		if s.busy[i] {
			continue
		}
		if google && now.Before(s.googleUntil[i]) {
			continue
		}
		return i, true
	}
	return -1, false
}
