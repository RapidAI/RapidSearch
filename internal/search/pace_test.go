package search

import (
	"sync"
	"testing"
	"time"
)

func TestEnginePaceRange(t *testing.T) {
	min, max := enginePaceRange("google")
	if min != 8*time.Second || max != 15*time.Second {
		t.Fatalf("google %v-%v", min, max)
	}
	for _, e := range []string{"bing", "baidu", "duckduckgo"} {
		min, max = enginePaceRange(e)
		if min != 1500*time.Millisecond || max != 4*time.Second {
			t.Fatalf("%s %v-%v", e, min, max)
		}
	}
}

func TestEnginePacerSerializesSameEngine(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	var sleeps []time.Duration
	p := &EnginePacer{
		last: make(map[string]time.Time),
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		sleep: func(d time.Duration) {
			mu.Lock()
			sleeps = append(sleeps, d)
			now = now.Add(d)
			mu.Unlock()
		},
		jitter: func(min, max time.Duration) time.Duration { return min },
	}
	p.Wait("bing")
	if len(sleeps) != 0 {
		t.Fatalf("first bing should not wait: %v", sleeps)
	}
	p.Wait("bing")
	if len(sleeps) != 1 || sleeps[0] != 1500*time.Millisecond {
		t.Fatalf("second bing wait=%v", sleeps)
	}
	p.Wait("google")
	if len(sleeps) != 1 {
		t.Fatalf("first google should not wait: %v", sleeps)
	}
	p.Wait("google")
	if len(sleeps) != 2 || sleeps[1] != 8*time.Second {
		t.Fatalf("second google wait=%v", sleeps)
	}
}
