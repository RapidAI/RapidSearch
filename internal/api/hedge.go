package api

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"search-service/internal/search"
)

const (
	defaultHedgeDelay = 3 * time.Second
	maxHedgeInFlight  = 2
)

type engineRun func(ctx context.Context, engine string) ([]search.Result, error)

func runHedgedChain(ctx context.Context, chain []string, delay time.Duration, run engineRun) (won string, results []search.Result, tried []string, lastErr error) {
	if delay <= 0 {
		delay = defaultHedgeDelay
	}
	if len(chain) == 0 {
		return "", nil, nil, search.NewError(search.CodeEngine, "no engines scheduled")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		engine  string
		results []search.Result
		err     error
	}
	n := len(chain)
	ch := make(chan outcome, n)

	var mu sync.Mutex
	started := make([]bool, n)
	inFlight := 0
	googleLive := 0

	launch := func(i int) bool {
		if i < 0 || i >= n {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if started[i] || inFlight >= maxHedgeInFlight {
			return false
		}
		eng := chain[i]
		if strings.EqualFold(eng, "google") && googleLive > 0 {
			return false
		}
		started[i] = true
		inFlight++
		if strings.EqualFold(eng, "google") {
			googleLive++
		}
		go func(eng string) {
			res, err := run(runCtx, eng)
			ch <- outcome{engine: eng, results: res, err: err}
		}(eng)
		return true
	}

	markDone := func(eng string) {
		mu.Lock()
		inFlight--
		if strings.EqualFold(eng, "google") && googleLive > 0 {
			googleLive--
		}
		mu.Unlock()
	}

	collectTried := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, 0, n)
		for i, s := range started {
			if s {
				out = append(out, chain[i])
			}
		}
		return out
	}

	if !launch(0) {
		return "", nil, nil, search.NewError(search.CodeEngine, "no engines scheduled")
	}
	next := 1
	pending := 1
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var timerCh <-chan time.Time = timer.C

	for pending > 0 {
		select {
		case o := <-ch:
			markDone(o.engine)
			pending--
			if o.err == nil && len(o.results) > 0 {
				cancel()
				log.Printf("search hedge=win engine=%s tried=%v", o.engine, collectTried())
				return o.engine, o.results, collectTried(), nil
			}
			lastErr = o.err
			if lastErr == nil {
				lastErr = search.NewError(search.CodeParse, "no organic results parsed from "+o.engine)
			}
			if next < n && launch(next) {
				log.Printf("search hedge=next engine=%s after fail", chain[next])
				next++
				pending++
			}
		case <-timerCh:
			timerCh = nil
			if next < n && launch(next) {
				log.Printf("search hedge=parallel engine=%s delay=%s", chain[next], delay)
				next++
				pending++
			}
		case <-ctx.Done():
			cancel()
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return "", nil, collectTried(), lastErr
		}
	}
	if lastErr == nil {
		lastErr = search.NewError(search.CodeParse, "all engines failed")
	}
	return "", nil, collectTried(), lastErr
}

func classifyLiveErr(err error) (code, msg string) {
	if err == nil {
		return search.CodeParse, "all engines failed"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return search.CodeTimeout, "search timed out"
	}
	code = search.CodeOf(err)
	msg = clientMessage(code, err.Error())
	return code, msg
}

func clientMessage(code, fallback string) string {
	switch code {
	case search.CodeCaptcha:
		return "search blocked by captcha"
	case search.CodeTimeout:
		return "search timed out"
	case search.CodeParse:
		return "search failed to parse"
	case search.CodeOffline:
		return "search backend offline"
	case search.CodeUnauthorized:
		return "unauthorized"
	case search.CodeBadRequest:
		if fallback != "" {
			return fallback
		}
		return "bad request"
	case search.CodeEngine:
		if fallback != "" {
			return fallback
		}
		return "unsupported engine"
	default:
		if fallback != "" {
			return fallback
		}
		return "search failed"
	}
}

func liveErrStatus(code string) int {
	switch code {
	case search.CodeBadRequest, search.CodeEngine:
		return 400
	case search.CodeTimeout:
		return 504
	case search.CodeCaptcha:
		return 403
	case search.CodeUnauthorized:
		return 401
	case search.CodeOffline:
		return 503
	default:
		return 502
	}
}
