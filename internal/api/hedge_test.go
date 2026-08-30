package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"search-service/internal/browser"
	"search-service/internal/search"
)

func hit(title, u string) []search.Result {
	return []search.Result{{Rank: 1, Title: title, URL: u, Snippet: title}}
}

func TestHedgeFirstSuccessWins(t *testing.T) {
	var started atomic.Int32
	var googleStarted, bingStarted, ddgStarted atomic.Bool
	cancelled := make(chan struct{})

	run := func(ctx context.Context, eng string) ([]search.Result, error) {
		started.Add(1)
		switch eng {
		case "google":
			googleStarted.Store(true)
			select {
			case <-time.After(2 * time.Second):
				return hit("g", "https://g.example"), nil
			case <-ctx.Done():
				close(cancelled)
				return nil, ctx.Err()
			}
		case "bing":
			bingStarted.Store(true)
			time.Sleep(8 * time.Millisecond)
			return hit("b", "https://b.example"), nil
		default:
			ddgStarted.Store(true)
			return hit("d", "https://d.example"), nil
		}
	}

	start := time.Now()
	won, res, tried, err := runHedgedChain(context.Background(), []string{"google", "bing", "duckduckgo"}, 15*time.Millisecond, run)
	if err != nil {
		t.Fatal(err)
	}
	if won != "bing" || len(res) != 1 || res[0].Title != "b" {
		t.Fatalf("won=%s res=%v", won, res)
	}
	if !googleStarted.Load() || !bingStarted.Load() {
		t.Fatal("expected google and bing to start")
	}
	if ddgStarted.Load() {
		t.Fatal("third engine should not start after first success")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("loser google was not cancelled")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("hedge took too long: %s", time.Since(start))
	}
	if len(tried) < 2 || tried[0] != "google" || tried[1] != "bing" {
		t.Fatalf("tried=%v", tried)
	}
}

func TestHedgeCapTwoInFlight(t *testing.T) {
	var n atomic.Int32
	run := func(ctx context.Context, eng string) ([]search.Result, error) {
		n.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, _, tried, err := runHedgedChain(ctx, []string{"google", "bing", "duckduckgo"}, 10*time.Millisecond, run)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if n.Load() != 2 {
		t.Fatalf("in-flight=%d want 2, tried=%v", n.Load(), tried)
	}
}

func TestHedgeDoesNotRunThreeGoogles(t *testing.T) {
	var n atomic.Int32
	run := func(ctx context.Context, eng string) ([]search.Result, error) {
		n.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, _, _ = runHedgedChain(ctx, []string{"google", "google", "google"}, 5*time.Millisecond, run)
	if n.Load() != 1 {
		t.Fatalf("google in-flight=%d want 1", n.Load())
	}
}

func TestErrNoGoogleInstanceDoesNotEatTryTimeout(t *testing.T) {
	var mu sync.Mutex
	var order []string
	run := func(ctx context.Context, eng string) ([]search.Result, error) {
		mu.Lock()
		order = append(order, eng)
		mu.Unlock()
		if eng == "google" {
			return nil, browser.ErrNoGoogleInstance
		}
		return hit("b", "https://b.example"), nil
	}
	start := time.Now()
	won, _, tried, err := runHedgedChain(context.Background(), []string{"google", "bing"}, 3*time.Second, run)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if won != "bing" {
		t.Fatalf("won=%s", won)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ErrNoGoogleInstance ate timeout: %s tried=%v", elapsed, tried)
	}
	if len(order) < 2 || order[0] != "google" || order[1] != "bing" {
		t.Fatalf("order=%v", order)
	}
}

func TestHedgeFailFastEmptyChain(t *testing.T) {
	_, _, _, err := runHedgedChain(context.Background(), nil, time.Millisecond, func(context.Context, string) ([]search.Result, error) {
		t.Fatal("run should not be called")
		return nil, errors.New("no")
	})
	if !search.Is(err, search.CodeEngine) {
		t.Fatalf("err=%v", err)
	}
}
