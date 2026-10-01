package api

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Per-lane hang limits for a job while status is "running".
//
// PAPERS_TRANSLATE_FAST_TIMEOUT / PAPERS_TRANSLATE_SLOW_TIMEOUT accept a Go
// duration ("45m", "3h") or an integer number of minutes. Defaults: 45m fast,
// 180m (3h) slow. PAPERS_TRANSLATE_MAX_TIMEOUTS is the strike count before a
// job is permanently skipped (default 3).
const (
	translateFastTimeoutDefault = 45 * time.Minute
	translateSlowTimeoutDefault = 180 * time.Minute
	translateMaxTimeoutsDefault = 3
	// translateTimeoutGrace is added to the lane limit for CommandContext so
	// the loop watchdog (started_at wall-clock) reaps first and owns requeue
	// vs permanent-skip policy.
	translateTimeoutGrace = time.Minute
)

const (
	envTranslateFastTimeout = "PAPERS_TRANSLATE_FAST_TIMEOUT"
	envTranslateSlowTimeout = "PAPERS_TRANSLATE_SLOW_TIMEOUT"
	envTranslateMaxTimeouts = "PAPERS_TRANSLATE_MAX_TIMEOUTS"
)

// parseTranslateDurationEnv reads a Go duration or integer minutes.
// Empty or invalid values return def. The value must be > 0.
func parseTranslateDurationEnv(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return def
}

func parseTranslateMaxTimeouts() int {
	raw := strings.TrimSpace(os.Getenv(envTranslateMaxTimeouts))
	if raw == "" {
		return translateMaxTimeoutsDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return translateMaxTimeoutsDefault
	}
	return n
}

// translateLaneTimeout is the max wall-clock runtime for a running job.
func translateLaneTimeout(lane string) time.Duration {
	if lane == translateLaneSlow {
		return parseTranslateDurationEnv(envTranslateSlowTimeout, translateSlowTimeoutDefault)
	}
	return parseTranslateDurationEnv(envTranslateFastTimeout, translateFastTimeoutDefault)
}

func translateTimeoutRequeueError(attempt, max int) string {
	return fmt.Sprintf("timeout (attempt %d/%d); requeued", attempt, max)
}

func translateTimeoutSkipError(n int) string {
	return fmt.Sprintf("timed out %d times; permanently skipped", n)
}

func parseJobStartedAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

type timeoutHit struct {
	id        string
	lane      string
	elapsed   time.Duration
	attempt   int
	max       int
	permanent bool
}

// reapTimedOut kills OS worker trees that have exceeded their lane limit,
// frees the concurrency slot, and either requeues (end of the same lane so
// other work can proceed) or permanently skips after PAPERS_TRANSLATE_MAX_TIMEOUTS.
func (s *translateService) reapTimedOut() {
	if s == nil {
		return
	}
	now := time.Now()
	max := parseTranslateMaxTimeouts()
	var hits []timeoutHit
	s.mu.Lock()
	dirty := false
	for id, j := range s.status.Jobs {
		if j.Status != translateRunning || id == "" {
			continue
		}
		started, ok := parseJobStartedAt(j.StartedAt)
		if !ok {
			j.StartedAt = now.UTC().Format(time.RFC3339)
			s.status.Jobs[id] = j
			dirty = true
			continue
		}
		lane := s.jobLaneLocked(id)
		limit := translateLaneTimeout(lane)
		elapsed := now.Sub(started)
		if elapsed < limit {
			continue
		}
		delete(s.running, id)
		s.dropQueuedLocked(id)
		j.TimeoutCount++
		j.Lane = lane
		j.UpdatedAt = now.UTC().Format(time.RFC3339)
		attempt := j.TimeoutCount
		permanent := attempt >= max
		if permanent {
			j.Status = translateSkipped
			j.Error = translateTimeoutSkipError(attempt)
			j.Finished = j.UpdatedAt
		} else {
			// Append to the end of this lane (pushQueuedLocked) so a hung
			// retry does not jump ahead of other queued papers.
			j.Status = translateQueued
			j.Error = translateTimeoutRequeueError(attempt, max)
			j.StartedAt = ""
			j.Finished = ""
			s.status.Jobs[id] = j
			s.pushQueuedLocked(id, j.PageCount)
		}
		if s.status.Jobs == nil {
			s.status.Jobs = map[string]translateJob{}
		}
		s.status.Jobs[id] = j
		dirty = true
		hits = append(hits, timeoutHit{
			id: id, lane: lane, elapsed: elapsed,
			attempt: attempt, max: max, permanent: permanent,
		})
	}
	if dirty {
		s.status.UpdatedAt = now.UTC().Format(time.RFC3339)
		s.status.Version = 1
		if err := s.persistStatusLocked(); err != nil {
			log.Printf("papers translate-status write: %v", err)
		}
	}
	requeued := false
	for _, h := range hits {
		if !h.permanent {
			requeued = true
		}
	}
	s.mu.Unlock()

	for _, h := range hits {
		killTranslateJobTree(s.root, h.id)
		action := "requeue"
		if h.permanent {
			action = "skip"
		}
		log.Printf("papers translate id=%s lane=%s status=timeout elapsed=%s attempt=%d/%d action=%s",
			h.id, h.lane, h.elapsed.Round(time.Second), h.attempt, h.max, action)
	}
	if requeued {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

func (s *translateService) dropQueuedLocked(id string) {
	if id == "" {
		return
	}
	delete(s.inQ, id)
	s.fastQ = filterQueuedIDs(s.fastQ, id)
	s.slowQ = filterQueuedIDs(s.slowQ, id)
}

func filterQueuedIDs(q []string, id string) []string {
	if len(q) == 0 {
		return q
	}
	out := q[:0]
	for _, x := range q {
		if x != id && x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (s *translateService) jobLane(id string) string {
	if s == nil {
		return translateLaneFast
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobLaneLocked(id)
}

func (s *translateService) stillRunning(id string) bool {
	j, ok := s.job(id)
	return ok && j.Status == translateRunning
}
