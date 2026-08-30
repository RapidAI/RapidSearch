package browser

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseBrowserSlots(t *testing.T) {
	if ParseBrowserSlots("") != 2 {
		t.Fatalf("default: %d", ParseBrowserSlots(""))
	}
	if ParseBrowserSlots("nope") != 2 {
		t.Fatal("invalid")
	}
	if ParseBrowserSlots("0") != 1 {
		t.Fatal("clamp low")
	}
	if ParseBrowserSlots("9") != 4 {
		t.Fatal("clamp high")
	}
	if ParseBrowserSlots("3") != 3 {
		t.Fatal("pass through")
	}
}

func TestSlotLimiterCapsOverlap(t *testing.T) {
	lim := NewSlotLimiter(2)
	var (
		cur int32
		max int32
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := lim.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := atomic.AddInt32(&cur, 1)
			for {
				old := atomic.LoadInt32(&max)
				if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			lim.Release()
		}()
	}
	close(start)
	wg.Wait()
	if max > 2 {
		t.Fatalf("overlap %d exceeds slots 2", max)
	}
	if max < 1 {
		t.Fatal("no work ran")
	}
}

func TestSlotLimiterCancel(t *testing.T) {
	lim := NewSlotLimiter(1)
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lim.Acquire(ctx); err == nil {
		t.Fatal("expected cancel while slot held")
	}
	lim.Release()
}
