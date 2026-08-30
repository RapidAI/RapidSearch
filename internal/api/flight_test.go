package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlightGroupSharesOneRun(t *testing.T) {
	var g flightGroup
	var runs atomic.Int32
	var sharedN atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	const n = 8
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err, shared := g.Do("same", func() (interface{}, error) {
				runs.Add(1)
				time.Sleep(60 * time.Millisecond)
				return "ok", nil
			})
			if err != nil {
				t.Errorf("err=%v", err)
			}
			if shared {
				sharedN.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("inner runs=%d want 1", runs.Load())
	}
	if sharedN.Load() < 1 {
		t.Fatalf("expected waiters to share, shared=%d", sharedN.Load())
	}
}

func TestFlightGroupSeparateKeys(t *testing.T) {
	var g flightGroup
	var runs atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _ = g.Do("a", func() (interface{}, error) {
			runs.Add(1)
			time.Sleep(30 * time.Millisecond)
			return 1, nil
		})
	}()
	go func() {
		defer wg.Done()
		_, _, _ = g.Do("b", func() (interface{}, error) {
			runs.Add(1)
			time.Sleep(30 * time.Millisecond)
			return 2, nil
		})
	}()
	wg.Wait()
	if runs.Load() != 2 {
		t.Fatalf("runs=%d want 2", runs.Load())
	}
}
