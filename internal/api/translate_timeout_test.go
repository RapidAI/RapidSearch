package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTranslateDurationEnv(t *testing.T) {
	t.Setenv(envTranslateFastTimeout, "")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, translateFastTimeoutDefault); got != translateFastTimeoutDefault {
		t.Fatalf("empty default=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "45m")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, time.Hour); got != 45*time.Minute {
		t.Fatalf("45m=%s", got)
	}
	t.Setenv(envTranslateSlowTimeout, "3h")
	if got := parseTranslateDurationEnv(envTranslateSlowTimeout, time.Hour); got != 3*time.Hour {
		t.Fatalf("3h=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "12")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, time.Hour); got != 12*time.Minute {
		t.Fatalf("integer minutes=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "bogus")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, translateFastTimeoutDefault); got != translateFastTimeoutDefault {
		t.Fatalf("invalid=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "0")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, translateFastTimeoutDefault); got != translateFastTimeoutDefault {
		t.Fatalf("zero=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "-5m")
	if got := parseTranslateDurationEnv(envTranslateFastTimeout, translateFastTimeoutDefault); got != translateFastTimeoutDefault {
		t.Fatalf("negative=%s", got)
	}
}

func TestParseTranslateMaxTimeouts(t *testing.T) {
	t.Setenv(envTranslateMaxTimeouts, "")
	if got := parseTranslateMaxTimeouts(); got != translateMaxTimeoutsDefault {
		t.Fatalf("default=%d", got)
	}
	t.Setenv(envTranslateMaxTimeouts, "5")
	if got := parseTranslateMaxTimeouts(); got != 5 {
		t.Fatalf("5=%d", got)
	}
	t.Setenv(envTranslateMaxTimeouts, "0")
	if got := parseTranslateMaxTimeouts(); got != translateMaxTimeoutsDefault {
		t.Fatalf("zero=%d", got)
	}
	t.Setenv(envTranslateMaxTimeouts, "bogus")
	if got := parseTranslateMaxTimeouts(); got != translateMaxTimeoutsDefault {
		t.Fatalf("invalid=%d", got)
	}
}

func TestTranslateLaneTimeoutDefaults(t *testing.T) {
	t.Setenv(envTranslateFastTimeout, "")
	t.Setenv(envTranslateSlowTimeout, "")
	if got := translateLaneTimeout(translateLaneFast); got != 45*time.Minute {
		t.Fatalf("fast default=%s", got)
	}
	if got := translateLaneTimeout(translateLaneSlow); got != 180*time.Minute {
		t.Fatalf("slow default=%s", got)
	}
	if got := translateLaneTimeout(""); got != 45*time.Minute {
		t.Fatalf("unknown lane should use fast=%s", got)
	}
	t.Setenv(envTranslateFastTimeout, "10m")
	t.Setenv(envTranslateSlowTimeout, "90")
	if got := translateLaneTimeout(translateLaneFast); got != 10*time.Minute {
		t.Fatalf("fast override=%s", got)
	}
	if got := translateLaneTimeout(translateLaneSlow); got != 90*time.Minute {
		t.Fatalf("slow override minutes=%s", got)
	}
}

func timeoutTestService(t *testing.T) *translateService {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()
	return svc
}

func TestReapTimedOutRequeuesAndIncrements(t *testing.T) {
	t.Setenv(envTranslateFastTimeout, "45m")
	t.Setenv(envTranslateMaxTimeouts, "3")
	svc := timeoutTestService(t)
	started := time.Now().UTC().Add(-46 * time.Minute).Format(time.RFC3339)
	svc.mu.Lock()
	svc.running["fast1"] = true
	svc.status.Jobs["fast1"] = translateJob{
		ID: "fast1", Status: translateRunning, Lane: translateLaneFast,
		PageCount: 12, StartedAt: started,
	}
	// A later queued paper must stay ahead of the requeued timed-out job.
	svc.fastQ = []string{"other"}
	svc.inQ = map[string]bool{"other": true}
	svc.mu.Unlock()

	svc.reapTimedOut()

	j, ok := svc.job("fast1")
	if !ok {
		t.Fatal("missing job")
	}
	if j.Status != translateQueued {
		t.Fatalf("status=%s", j.Status)
	}
	if j.TimeoutCount != 1 {
		t.Fatalf("timeout_count=%d", j.TimeoutCount)
	}
	if j.Error != translateTimeoutRequeueError(1, 3) {
		t.Fatalf("error=%q", j.Error)
	}
	if j.StartedAt != "" {
		t.Fatalf("started_at should clear for the next attempt, got %q", j.StartedAt)
	}
	svc.mu.Lock()
	if svc.running["fast1"] {
		t.Fatal("running slot not freed")
	}
	if len(svc.fastQ) != 2 || svc.fastQ[0] != "other" || svc.fastQ[1] != "fast1" {
		t.Fatalf("requeue should append to end of fast lane, got %v", svc.fastQ)
	}
	svc.mu.Unlock()

	p := overlayOne(paperEntry{ID: "fast1", HasLocal: true, PageCount: 12}, svc.root, svc.jobsCopy())
	if p.TranslateError != translateTimeoutRequeueError(1, 3) {
		t.Fatalf("overlay error=%q", p.TranslateError)
	}
}

func TestReapTimedOutPermanentSkip(t *testing.T) {
	t.Setenv(envTranslateSlowTimeout, "3h")
	t.Setenv(envTranslateMaxTimeouts, "3")
	svc := timeoutTestService(t)
	started := time.Now().UTC().Add(-181 * time.Minute).Format(time.RFC3339)
	svc.mu.Lock()
	svc.running["slow1"] = true
	svc.status.Jobs["slow1"] = translateJob{
		ID: "slow1", Status: translateRunning, Lane: translateLaneSlow,
		PageCount: 70, StartedAt: started, TimeoutCount: 2,
	}
	svc.mu.Unlock()

	svc.reapTimedOut()

	j, ok := svc.job("slow1")
	if !ok {
		t.Fatal("missing job")
	}
	if j.Status != translateSkipped {
		t.Fatalf("status=%s", j.Status)
	}
	if j.TimeoutCount != 3 {
		t.Fatalf("timeout_count=%d", j.TimeoutCount)
	}
	if j.Error != translateTimeoutSkipError(3) {
		t.Fatalf("error=%q", j.Error)
	}
	svc.mu.Lock()
	if svc.running["slow1"] {
		t.Fatal("running slot not freed")
	}
	if svc.inQ["slow1"] || len(svc.slowQ) != 0 {
		t.Fatalf("permanently skipped job must not be requeued inQ=%v slowQ=%v", svc.inQ, svc.slowQ)
	}
	svc.mu.Unlock()

	papers := []paperEntry{{
		ID: "slow1", HasLocal: true, Filename: "slow1.pdf", PageCount: 70,
		TranslateStatus: translateSkipped,
	}}
	if ids := pendingTranslateIDs(papers, true); len(ids) != 0 {
		t.Fatalf("auto-translate must not pick skipped: %v", ids)
	}
	if ids := pendingTranslateIDs(papers, false); len(ids) != 0 {
		t.Fatalf("translate-all must not pick skipped: %v", ids)
	}

	p := overlayOne(paperEntry{ID: "slow1", HasLocal: true, PageCount: 70}, svc.root, svc.jobsCopy())
	if p.TranslateStatus != translateSkipped || p.TranslateError != translateTimeoutSkipError(3) {
		t.Fatalf("overlay %+v", p)
	}
}

func TestReapTimedOutLeavesFreshJob(t *testing.T) {
	t.Setenv(envTranslateFastTimeout, "45m")
	svc := timeoutTestService(t)
	started := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	svc.mu.Lock()
	svc.running["fresh"] = true
	svc.status.Jobs["fresh"] = translateJob{
		ID: "fresh", Status: translateRunning, Lane: translateLaneFast,
		PageCount: 8, StartedAt: started,
	}
	svc.mu.Unlock()

	svc.reapTimedOut()

	j, ok := svc.job("fresh")
	if !ok || j.Status != translateRunning {
		t.Fatalf("fresh job moved: ok=%v %+v", ok, j)
	}
	if j.TimeoutCount != 0 {
		t.Fatalf("timeout_count=%d", j.TimeoutCount)
	}
	if j.Error != "" {
		t.Fatalf("error=%q", j.Error)
	}
	svc.mu.Lock()
	if !svc.running["fresh"] {
		t.Fatal("fresh job lost its slot")
	}
	svc.mu.Unlock()
}

func TestReapTimedOutSetsMissingStartedAt(t *testing.T) {
	svc := timeoutTestService(t)
	svc.mu.Lock()
	svc.running["orphan"] = true
	svc.status.Jobs["orphan"] = translateJob{
		ID: "orphan", Status: translateRunning, Lane: translateLaneFast, PageCount: 5,
	}
	svc.mu.Unlock()

	svc.reapTimedOut()

	j, ok := svc.job("orphan")
	if !ok || j.Status != translateRunning {
		t.Fatalf("should remain running until a clock is set: %+v", j)
	}
	if j.StartedAt == "" {
		t.Fatal("expected started_at to be filled")
	}
	if j.TimeoutCount != 0 {
		t.Fatalf("timeout_count=%d", j.TimeoutCount)
	}
}

func TestEnqueueResetsTimeoutCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "demo.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.status.Jobs["demo"] = translateJob{
		ID: "demo", Status: translateSkipped, TimeoutCount: 3,
		Error: translateTimeoutSkipError(3), PageCount: 10, Lane: translateLaneFast,
	}
	svc.mu.Unlock()

	papers := []paperEntry{{ID: "demo", HasLocal: true, Filename: "demo.pdf", PageCount: 10}}
	res := svc.enqueue([]string{"demo"}, papers, false)
	if len(res.Queued) != 1 {
		t.Fatalf("manual enqueue of skipped should requeue: %+v", res)
	}
	j, ok := svc.job("demo")
	if !ok || j.Status != translateQueued {
		t.Fatalf("job %+v ok=%v", j, ok)
	}
	if j.TimeoutCount != 0 {
		t.Fatalf("manual re-translate should reset timeout_count=%d", j.TimeoutCount)
	}
	if j.Error != "" {
		t.Fatalf("error should clear, got %q", j.Error)
	}
}

func TestKillTranslateJobTreeUsesPidfile(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := writeTranslatePID(dir, "demo", cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	n := killTranslateJobTree(dir, "demo")
	if n < 1 {
		_ = cmd.Process.Kill()
		t.Fatal("expected at least one pid killed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sleep should have been killed")
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process still alive after killTranslateJobTree")
	}
	if _, err := os.Stat(filepath.Join(translateWorkDir(dir, "demo"), "worker.pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be cleared: %v", err)
	}
}

func TestTimeoutErrorStrings(t *testing.T) {
	if !strings.Contains(translateTimeoutRequeueError(1, 3), "attempt 1/3") {
		t.Fatal(translateTimeoutRequeueError(1, 3))
	}
	if !strings.Contains(translateTimeoutSkipError(3), "timed out 3 times") {
		t.Fatal(translateTimeoutSkipError(3))
	}
}
