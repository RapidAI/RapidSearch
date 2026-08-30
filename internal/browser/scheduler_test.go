package browser

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerGoogleSerializedBingOverlaps(t *testing.T) {
	s := NewScheduler(3)
	ctx := context.Background()
	g1, err := s.Acquire(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}

	var google2 atomic.Bool
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		id, err := s.Acquire(ctx, "google")
		if err != nil {
			t.Errorf("second google: %v", err)
			close(done)
			return
		}
		google2.Store(true)
		s.Release(id, "google", false)
		close(done)
	}()
	<-started
	time.Sleep(40 * time.Millisecond)
	if google2.Load() {
		t.Fatal("two Google Acquire overlapped")
	}

	b, err := s.Acquire(ctx, "bing")
	if err != nil {
		t.Fatal(err)
	}
	if b == g1 {
		t.Fatal("bing stole the in-flight google instance")
	}
	s.Release(b, "bing", false)

	s.Release(g1, "google", false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second google did not proceed after release")
	}
	if !google2.Load() {
		t.Fatal("second google never ran")
	}
}

func TestSchedulerCaptchaQuarantineSkipsGoogle(t *testing.T) {
	s := NewScheduler(3)
	ctx := context.Background()
	id, err := s.Acquire(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Fatalf("want instance 0, got %d", id)
	}
	s.Release(id, "google", true)

	id2, err := s.Acquire(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id {
		t.Fatalf("quarantined instance %d reused for google", id2)
	}
	s.Release(id2, "google", false)

	bing, err := s.Acquire(ctx, "bing")
	if err != nil {
		t.Fatal(err)
	}
	if bing != id {
		t.Fatalf("bing should use quarantined instance %d, got %d", id, bing)
	}
	s.Release(bing, "bing", false)
}

func TestSchedulerAllGoogleQuarantined(t *testing.T) {
	s := NewScheduler(2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		id, err := s.Acquire(ctx, "google")
		if err != nil {
			t.Fatal(err)
		}
		s.Release(id, "google", true)
	}
	_, err := s.Acquire(ctx, "google")
	if !errors.Is(err, ErrNoGoogleInstance) {
		t.Fatalf("want ErrNoGoogleInstance, got %v", err)
	}
	id, err := s.Acquire(ctx, "baidu")
	if err != nil {
		t.Fatal(err)
	}
	s.Release(id, "baidu", false)
}

func TestSchedulerQuarantineExpires(t *testing.T) {
	s := NewScheduler(1)
	frozen := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return frozen }
	ctx := context.Background()
	id, err := s.Acquire(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	s.Release(id, "google", true)
	_, err = s.Acquire(ctx, "google")
	if !errors.Is(err, ErrNoGoogleInstance) {
		t.Fatalf("still quarantined: %v", err)
	}
	s.now = func() time.Time { return frozen.Add(DefaultGoogleQuarantine + time.Second) }
	id2, err := s.Acquire(ctx, "google")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("expired quarantine should reuse %d, got %d", id, id2)
	}
	s.Release(id2, "google", false)
}

func TestSchedulerAcquireCancel(t *testing.T) {
	s := NewScheduler(1)
	ctx := context.Background()
	id, err := s.Acquire(ctx, "bing")
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(cctx, "baidu"); err == nil {
		t.Fatal("expected cancel while instance held")
	}
	s.Release(id, "bing", false)
}

func TestSchedulerPickDownloadIdle(t *testing.T) {
	s := NewScheduler(3)
	ctx := context.Background()
	id, err := s.Acquire(ctx, "bing")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.PickDownload(); got != 1 {
		t.Fatalf("prefer idle instance, got %d", got)
	}
	s.Release(id, "bing", false)
	if got := s.PickDownload(); got != 0 {
		t.Fatalf("all idle should pick 0, got %d", got)
	}
}

func TestSchedulerNonGoogleParallel(t *testing.T) {
	s := NewScheduler(3)
	ctx := context.Background()
	var cur, max int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := s.Acquire(ctx, "bing")
			if err != nil {
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
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			s.Release(id, "bing", false)
		}()
	}
	close(start)
	wg.Wait()
	if max > 3 {
		t.Fatalf("overlap %d exceeds instances 3", max)
	}
	if max < 3 {
		t.Fatalf("expected full pool use, max=%d", max)
	}
}
