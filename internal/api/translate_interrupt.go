package api

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Interrupt recovery for PDF jobs that died because search-service restarted
// or a BabelDOC / translate_worker tree was killed.
//
// Design (leave-failed + debounce):
//
//	reconcileExternal still marks dead orphans failed with
//	"interrupted (process restarted)" and does not requeue in the same tick.
//	recoverInterruptedJobs then picks those failures up on startup
//	(recoverJobs) and on every translate-loop watchdog pass.
//	A job whose Finished/UpdatedAt is newer than
//	PAPERS_TRANSLATE_INTERRUPT_DEBOUNCE (default 30s) is left failed so a
//	just-marked interrupt cannot fight the fail path in the same tick.
//	Already-failed jobs from a previous process (old timestamps) recover
//	immediately on start. Mid-process kills recover within ~30–60s.
//
// Strikes are independent of hang-timeout timeout_count: a paper that timed
// out once can still recover from restarts. Default max 3 each.
const (
	translateMaxInterruptRecoveriesDefault = 3
	translateInterruptDebounceDefault      = 30 * time.Second
)

const (
	envTranslateMaxInterruptRecoveries = "PAPERS_TRANSLATE_MAX_INTERRUPT_RECOVERIES"
	envTranslateInterruptDebounce      = "PAPERS_TRANSLATE_INTERRUPT_DEBOUNCE"
)

// recoverablePaperIDRe is a modern arXiv id: YYMM.NNNNN plus optional version.
// Garbage keys in translate_status.json (e.g. "2608.11274if", "IDls") must
// not be auto-requeued.
var recoverablePaperIDRe = regexp.MustCompile(`(?i)^\d{4}\.\d{4,5}(?:v\d+)?$`)

func parseTranslateMaxInterruptRecoveries() int {
	raw := strings.TrimSpace(os.Getenv(envTranslateMaxInterruptRecoveries))
	if raw == "" {
		return translateMaxInterruptRecoveriesDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return translateMaxInterruptRecoveriesDefault
	}
	return n
}

// parseTranslateInterruptDebounce accepts a Go duration or integer seconds.
// Zero is allowed (tests / immediate recovery). Invalid values use 30s.
func parseTranslateInterruptDebounce() time.Duration {
	raw := strings.TrimSpace(os.Getenv(envTranslateInterruptDebounce))
	if raw == "" {
		return translateInterruptDebounceDefault
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return translateInterruptDebounceDefault
}

func translateInterruptRequeueError(attempt, max int) string {
	return fmt.Sprintf("interrupted; requeued (attempt %d/%d)", attempt, max)
}

func translateInterruptSkipError(n int) string {
	return fmt.Sprintf("interrupted %d times; permanently skipped", n)
}

// isInterruptRestartError matches the production reconcile string and close
// restart/kill variants. Ordinary quality / Hub / Google failures stay failed.
func isInterruptRestartError(msg string) bool {
	low := strings.ToLower(strings.TrimSpace(msg))
	if low == "" {
		return false
	}
	if strings.Contains(low, "permanently skipped") {
		return false
	}
	if strings.Contains(low, "incomplete translation") {
		return false
	}
	if strings.Contains(low, "interrupted (process restarted)") {
		return true
	}
	if !strings.Contains(low, "interrupt") {
		return false
	}
	return strings.Contains(low, "restart") || strings.Contains(low, "killed")
}

// isRecoverablePaperID is true only for sane arXiv-like job keys.
func isRecoverablePaperID(id string) bool {
	if id == "" {
		return false
	}
	clean := sanitizePaperID(id)
	if clean == "" || clean != id {
		return false
	}
	return recoverablePaperIDRe.MatchString(clean)
}

func interruptDebounceElapsed(j translateJob, now time.Time, debounce time.Duration) bool {
	if debounce <= 0 {
		return true
	}
	for _, raw := range []string{j.Finished, j.UpdatedAt} {
		if t, ok := parseJobStartedAt(raw); ok {
			return !now.Add(-debounce).Before(t)
		}
	}
	// No timestamp: treat as eligible so legacy failed rows still recover.
	return true
}

type interruptHit struct {
	id        string
	lane      string
	attempt   int
	max       int
	permanent bool
}

// recoverInterruptedJobs requeues (or permanently skips) failed interrupt
// jobs. It never occupies a concurrency slot: running is cleared, and the
// id is appended to the end of its existing lane.
func (s *translateService) recoverInterruptedJobs() {
	if s == nil {
		return
	}
	now := time.Now()
	max := parseTranslateMaxInterruptRecoveries()
	debounce := parseTranslateInterruptDebounce()
	var hits []interruptHit
	s.mu.Lock()
	dirty := false
	for id, j := range s.status.Jobs {
		if j.Status != translateFailed {
			continue
		}
		if !isRecoverablePaperID(id) {
			continue
		}
		if !isInterruptRestartError(j.Error) {
			continue
		}
		if s.running[id] || jobProcessBusy(s.root, id) {
			continue
		}
		if !interruptDebounceElapsed(j, now, debounce) {
			continue
		}

		delete(s.running, id)
		s.dropQueuedLocked(id)
		j.InterruptCount++
		lane := s.jobLaneLocked(id)
		j.Lane = lane
		j.UpdatedAt = now.UTC().Format(time.RFC3339)
		attempt := j.InterruptCount
		permanent := attempt >= max
		if permanent {
			j.Status = translateSkipped
			j.Error = translateInterruptSkipError(attempt)
			j.Finished = j.UpdatedAt
		} else {
			j.Status = translateQueued
			j.Error = translateInterruptRequeueError(attempt, max)
			j.StartedAt = ""
			j.Finished = ""
			if s.status.Jobs == nil {
				s.status.Jobs = map[string]translateJob{}
			}
			s.status.Jobs[id] = j
			s.pushQueuedLocked(id, j.PageCount)
		}
		if s.status.Jobs == nil {
			s.status.Jobs = map[string]translateJob{}
		}
		s.status.Jobs[id] = j
		dirty = true
		hits = append(hits, interruptHit{
			id: id, lane: lane, attempt: attempt, max: max, permanent: permanent,
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
		action := "requeue"
		if h.permanent {
			action = "skip"
		}
		log.Printf("papers translate id=%s lane=%s status=interrupt attempt=%d/%d action=%s",
			h.id, h.lane, h.attempt, h.max, action)
	}
	if requeued {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}
