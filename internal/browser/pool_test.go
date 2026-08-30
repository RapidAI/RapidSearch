package browser

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

type fakeHost struct {
	id  int
	run func(ctx context.Context, fn func(*rod.Page) error) error
}

func (f *fakeHost) Ensure(context.Context) error { return nil }
func (f *fakeHost) Close() error                 { return nil }
func (f *fakeHost) withPage(ctx context.Context, fn func(*rod.Page) error) error {
	if f.run != nil {
		return f.run(ctx, fn)
	}
	return fn(nil)
}

type captchaErr struct{}

func (captchaErr) Error() string   { return "captcha" }
func (captchaErr) IsCaptcha() bool { return true }

func TestManagerGoogleSerializedBingOverlaps(t *testing.T) {
	n := 3
	hosts := make([]chromeHost, n)
	for i := 0; i < n; i++ {
		hosts[i] = &fakeHost{id: i}
	}
	m := newTestManager(n, hosts)

	var (
		gCur, gMax  int32
		bingOverlap int32
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = m.Do(context.Background(), "google", func(*rod.Page) error {
				c := atomic.AddInt32(&gCur, 1)
				for {
					old := atomic.LoadInt32(&gMax)
					if c <= old || atomic.CompareAndSwapInt32(&gMax, old, c) {
						break
					}
				}
				time.Sleep(80 * time.Millisecond)
				atomic.AddInt32(&gCur, -1)
				return nil
			})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(15 * time.Millisecond)
		_ = m.Do(context.Background(), "bing", func(*rod.Page) error {
			if atomic.LoadInt32(&gCur) > 0 {
				atomic.AddInt32(&bingOverlap, 1)
			}
			time.Sleep(40 * time.Millisecond)
			return nil
		})
	}()
	close(start)
	wg.Wait()
	if gMax > 1 {
		t.Fatalf("google overlap %d", gMax)
	}
	if gMax < 1 {
		t.Fatal("no google ran")
	}
	if bingOverlap < 1 {
		t.Fatal("bing should overlap an in-flight google")
	}
}

func TestManagerCaptchaQuarantineSkipsInstance(t *testing.T) {
	var mu sync.Mutex
	var ids []int
	var captchaOnce atomic.Bool
	hosts := make([]chromeHost, 3)
	for i := 0; i < 3; i++ {
		id := i
		hosts[i] = &fakeHost{
			id: id,
			run: func(ctx context.Context, fn func(*rod.Page) error) error {
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
				if id == 0 && captchaOnce.CompareAndSwap(false, true) {
					return captchaErr{}
				}
				return fn(nil)
			},
		}
	}
	m := newTestManager(3, hosts)
	ctx := context.Background()
	err := m.Do(ctx, "google", func(*rod.Page) error { return nil })
	if !isCaptchaErr(err) {
		t.Fatalf("first google: %v", err)
	}
	if err := m.Do(ctx, "google", func(*rod.Page) error { return nil }); err != nil {
		t.Fatalf("second google: %v", err)
	}
	if err := m.Do(ctx, "bing", func(*rod.Page) error { return nil }); err != nil {
		t.Fatalf("bing: %v", err)
	}
	mu.Lock()
	got := append([]int(nil), ids...)
	mu.Unlock()
	if len(got) < 3 || got[0] != 0 || got[1] == 0 || got[2] != 0 {
		t.Fatalf("instance order %v (want 0-captcha, other google, bing on 0)", got)
	}
}

func TestManagerWithPageDoesNotTakeSerpSlot(t *testing.T) {
	var serp atomic.Int32
	hosts := make([]chromeHost, 1)
	hosts[0] = &fakeHost{
		id: 0,
		run: func(ctx context.Context, fn func(*rod.Page) error) error {
			return fn(nil)
		},
	}
	m := newTestManager(1, hosts)
	ctx := context.Background()
	id, err := m.sched.Acquire(ctx, "bing")
	if err != nil {
		t.Fatal(err)
	}
	serp.Store(1)
	done := make(chan error, 1)
	go func() {
		done <- m.WithPage(ctx, func(*rod.Page) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WithPage blocked on SERP slot")
	}
	m.sched.Release(id, "bing", false)
}
