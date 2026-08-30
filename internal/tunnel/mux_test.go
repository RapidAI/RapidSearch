package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// clientMux mirrors cmd/proxy session: write mutex + pend map, so several
// roundTrips can be in flight on one connection.
type clientMux struct {
	c    net.Conn
	wmu  sync.Mutex
	mu   sync.Mutex
	pend map[string]chan Frame
}

func (s *clientMux) write(f Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return WriteFrame(s.c, f)
}

func (s *clientMux) roundTrip(ctx context.Context, f Frame) (Frame, error) {
	ch := make(chan Frame, 1)
	s.mu.Lock()
	s.pend[f.ID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pend, f.ID)
		s.mu.Unlock()
	}()
	if err := s.write(f); err != nil {
		return Frame{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

func (s *clientMux) readLoop() {
	for {
		var f Frame
		if err := ReadFrame(s.c, &f); err != nil {
			return
		}
		s.mu.Lock()
		ch := s.pend[f.ID]
		s.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- f:
		default:
		}
	}
}

func TestConcurrentRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	// server: echo each req as resp with matching ID; do not serialize wait.
	go func() {
		var wmu sync.Mutex
		for {
			var f Frame
			if err := ReadFrame(b, &f); err != nil {
				return
			}
			go func(f Frame) {
				time.Sleep(20 * time.Millisecond)
				wmu.Lock()
				_ = WriteFrame(b, Frame{Type: TypeResp, ID: f.ID, Status: 200, Body: f.ID})
				wmu.Unlock()
			}(f)
		}
	}()

	cli := &clientMux{c: a, pend: map[string]chan Frame{}}
	go cli.readLoop()

	const n = 16
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		id := fmt.Sprintf("id-%d", i)
		go func(id string) {
			defer wg.Done()
			<-start
			resp, err := cli.roundTrip(ctx, Frame{Type: TypeReq, ID: id, Path: "/health"})
			if err != nil {
				errCh <- err
				return
			}
			if resp.ID != id || resp.Status != 200 {
				errCh <- fmt.Errorf("bad resp %+v", resp)
			}
		}(id)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)
	// If write+wait were under one mutex, 16 * 20ms would be ~320ms+.
	if elapsed > 800*time.Millisecond {
		t.Fatalf("roundTrips serialized: elapsed=%s", elapsed)
	}
}

func TestWriteFrameMutexRequired(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	var wmu sync.Mutex
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wmu.Lock()
			err := WriteFrame(w, Frame{Type: TypeReq, ID: fmt.Sprintf("%d", i), Path: "/x"})
			wmu.Unlock()
			if err != nil {
				t.Errorf("write: %v", err)
			}
		}(i)
	}
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for got < n {
			var f Frame
			if err := ReadFrame(r, &f); err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if f.Type != TypeReq || f.Path != "/x" {
				t.Errorf("bad frame %+v", f)
			}
			got++
		}
	}()
	wg.Wait()
	_ = w.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("read %d/%d frames", got, n)
	}
	if got != n {
		t.Fatalf("got %d", got)
	}
}
