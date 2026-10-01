package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNextReconnectBackoff(t *testing.T) {
	if got := NextReconnectBackoff(0); got != ReconnectMin {
		t.Fatalf("from 0: got %s want %s", got, ReconnectMin)
	}
	if got := NextReconnectBackoff(ReconnectMin); got != time.Second {
		t.Fatalf("from min: got %s", got)
	}
	got := ReconnectMin
	var steps []time.Duration
	for i := 0; i < 8; i++ {
		got = NextReconnectBackoff(got)
		steps = append(steps, got)
		if got > ReconnectMax {
			t.Fatalf("backoff exceeded cap: %s", got)
		}
	}
	if steps[len(steps)-1] != ReconnectMax {
		t.Fatalf("did not reach cap, last=%s steps=%v", steps[len(steps)-1], steps)
	}
}

func TestPingerMissingPong(t *testing.T) {
	p := NewPinger()
	if p.TimedOut() {
		t.Fatal("fresh pinger is not waiting on a ping")
	}
	p.MarkPing()
	if p.TimedOut() {
		t.Fatal("just-sent ping should not be overdue")
	}
	p.lastPong = time.Now().Add(-(PongTimeout + time.Second))
	if !p.TimedOut() {
		t.Fatal("expected missing-pong timeout")
	}
	p.OnPong()
	p.MarkPing()
	if p.TimedOut() {
		t.Fatal("pong should reset the watchdog")
	}
}

func TestReadDeadlineRefreshedOnPingPong(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	idle := 80 * time.Millisecond
	if err := SetReadIdle(a, idle); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = WriteFrame(b, Frame{Type: TypePing, ID: "1"})
		time.Sleep(50 * time.Millisecond) // would exceed the original 80ms deadline
		_ = WriteFrame(b, Frame{Type: TypePong, ID: "1"})
		time.Sleep(50 * time.Millisecond)
		_ = WriteFrame(b, Frame{Type: TypePing, ID: "2"})
	}()

	var f Frame
	if err := ReadFrameRefreshingIdle(a, a, &f, idle); err != nil {
		t.Fatalf("first ping: %v", err)
	}
	if f.Type != TypePing || f.ID != "1" {
		t.Fatalf("got %+v", f)
	}
	if err := ReadFrameRefreshingIdle(a, a, &f, idle); err != nil {
		t.Fatalf("pong after refresh: %v", err)
	}
	if f.Type != TypePong {
		t.Fatalf("got %+v", f)
	}
	if err := ReadFrameRefreshingIdle(a, a, &f, idle); err != nil {
		t.Fatalf("second ping: %v", err)
	}
	if f.ID != "2" {
		t.Fatalf("got %+v", f)
	}
}

func TestReadDeadlineExpiresWithoutFrames(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	errCh := make(chan error, 1)
	go func() {
		var f Frame
		errCh <- ReadFrameRefreshingIdle(a, a, &f, 40*time.Millisecond)
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected idle timeout")
		}
		if !IsTimeout(err) {
			t.Fatalf("want timeout, got %v", err)
		}
		if classified := ClassifyReadError(err); !IsTimeout(classified) {
			t.Fatalf("classify: %v", classified)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ReadFrame did not return")
	}
}

func TestKeepaliveEmitsPingAndAcceptsPong(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	died := make(chan error, 1)
	var wmu sync.Mutex
	write := func(f Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return WriteFrame(a, f)
	}
	onPong, stop := StartKeepalive(context.Background(), KeepaliveConfig{
		Interval: 30 * time.Millisecond,
		PongWait: 200 * time.Millisecond,
	}, write, func(err error) { died <- err })
	defer stop()

	go func() {
		for {
			var f Frame
			if err := ReadFrame(b, &f); err != nil {
				return
			}
			if f.Type == TypePing {
				// Local read-loop would record the matching pong here.
				onPong()
			}
		}
	}()

	select {
	case err := <-died:
		t.Fatalf("keepalive died: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestKeepaliveDiesWithoutPong(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	died := make(chan error, 1)
	write := func(f Frame) error {
		return WriteFrame(a, f)
	}
	_, stop := StartKeepalive(context.Background(), KeepaliveConfig{
		Interval: 20 * time.Millisecond,
		PongWait: 80 * time.Millisecond,
	}, write, func(err error) { died <- err })
	defer stop()

	go func() {
		for {
			var f Frame
			if err := ReadFrame(b, &f); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-died:
		if !errors.Is(err, ErrPongTimeout) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("expected pong timeout")
	}
}

func TestKeepaliveStopsWithoutDie(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	died := make(chan error, 1)
	_, stop := StartKeepalive(context.Background(), KeepaliveConfig{
		Interval: 20 * time.Millisecond,
		PongWait: time.Second,
	}, func(f Frame) error {
		return WriteFrame(a, f)
	}, func(err error) { died <- err })

	var f Frame
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	if err := ReadFrame(b, &f); err != nil {
		t.Fatalf("first ping: %v", err)
	}
	stop()
	select {
	case err := <-died:
		t.Fatalf("stop should not die: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestClassifyReadErrorPongTimeout(t *testing.T) {
	err := ClassifyReadError(ErrPongTimeout)
	if !errors.Is(err, ErrPongTimeout) {
		t.Fatalf("%v", err)
	}
	if ClassifyReadError(nil) != nil {
		t.Fatal("nil")
	}
	if ClassifyReadError(io.EOF) != io.EOF {
		t.Fatal("eof passthrough")
	}
}
