package browser

import (
	"context"
	"os"
	"strconv"
	"strings"
)

const (
	defaultBrowserSlots = 2
	minBrowserSlots     = 1
	maxBrowserSlots     = 4
)

// SlotLimiter caps overlapping Chrome SERP work. Cache hits and HTTP
// preprocess/extract do not acquire a slot.
type SlotLimiter struct {
	sem chan struct{}
	n   int
}

func ClampSlots(n int) int {
	if n < minBrowserSlots {
		return minBrowserSlots
	}
	if n > maxBrowserSlots {
		return maxBrowserSlots
	}
	return n
}

// ParseBrowserSlots reads SEARCH_BROWSER_SLOTS. Empty/invalid → 2; then clamp 1–4.
func ParseBrowserSlots(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultBrowserSlots
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultBrowserSlots
	}
	return ClampSlots(n)
}

func slotsFromEnv() int {
	return ParseBrowserSlots(os.Getenv("SEARCH_BROWSER_SLOTS"))
}

func NewSlotLimiter(n int) *SlotLimiter {
	n = ClampSlots(n)
	return &SlotLimiter{sem: make(chan struct{}, n), n: n}
}

func (s *SlotLimiter) Cap() int {
	if s == nil {
		return 0
	}
	return s.n
}

func (s *SlotLimiter) Acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SlotLimiter) Release() {
	select {
	case <-s.sem:
	default:
	}
}
