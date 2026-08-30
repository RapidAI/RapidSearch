package browser

import (
	"context"
	"os"
	"strconv"
	"strings"
)

const (
	defaultBrowserSlots     = 2
	minBrowserSlots         = 1
	maxBrowserSlots         = 4
	defaultBrowserInstances = 3
	minBrowserInstances     = 1
	maxBrowserInstances     = 4
)

// SlotLimiter caps overlapping work. Kept for tests and as a legacy helper;
// SERP concurrency is enforced by the instance scheduler (one SERP per Chrome).
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

func ClampInstances(n int) int {
	if n < minBrowserInstances {
		return minBrowserInstances
	}
	if n > maxBrowserInstances {
		return maxBrowserInstances
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

// ParseBrowserInstances resolves the Chrome process pool size.
// SEARCH_BROWSER_INSTANCES empty/invalid → 3, then clamp 1–4.
// If SEARCH_BROWSER_SLOTS is set, it is a legacy alias/cap:
// total in-flight SERPs = min(instances, slots). Each process still runs one SERP.
func ParseBrowserInstances(instancesEnv, slotsEnv string) int {
	n := defaultBrowserInstances
	if v := strings.TrimSpace(instancesEnv); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			n = parsed
		}
	}
	n = ClampInstances(n)
	if v := strings.TrimSpace(slotsEnv); v != "" {
		slots := ParseBrowserSlots(v)
		if slots < n {
			n = slots
		}
	}
	return n
}

func instancesFromEnv() int {
	return ParseBrowserInstances(os.Getenv("SEARCH_BROWSER_INSTANCES"), os.Getenv("SEARCH_BROWSER_SLOTS"))
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
