package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func interruptTestService(t *testing.T) *translateService {
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

func TestParseTranslateMaxInterruptRecoveries(t *testing.T) {
	t.Setenv(envTranslateMaxInterruptRecoveries, "")
	if got := parseTranslateMaxInterruptRecoveries(); got != translateMaxInterruptRecoveriesDefault {
		t.Fatalf("default=%d", got)
	}
	t.Setenv(envTranslateMaxInterruptRecoveries, "5")
	if got := parseTranslateMaxInterruptRecoveries(); got != 5 {
		t.Fatalf("5=%d", got)
	}
	t.Setenv(envTranslateMaxInterruptRecoveries, "0")
	if got := parseTranslateMaxInterruptRecoveries(); got != translateMaxInterruptRecoveriesDefault {
		t.Fatalf("zero=%d", got)
	}
	t.Setenv(envTranslateMaxInterruptRecoveries, "bogus")
	if got := parseTranslateMaxInterruptRecoveries(); got != translateMaxInterruptRecoveriesDefault {
		t.Fatalf("invalid=%d", got)
	}
}

func TestParseTranslateInterruptDebounce(t *testing.T) {
	t.Setenv(envTranslateInterruptDebounce, "")
	if got := parseTranslateInterruptDebounce(); got != translateInterruptDebounceDefault {
		t.Fatalf("default=%s", got)
	}
	t.Setenv(envTranslateInterruptDebounce, "45s")
	if got := parseTranslateInterruptDebounce(); got != 45*time.Second {
		t.Fatalf("45s=%s", got)
	}
	t.Setenv(envTranslateInterruptDebounce, "0")
	if got := parseTranslateInterruptDebounce(); got != 0 {
		t.Fatalf("zero should be allowed, got %s", got)
	}
	t.Setenv(envTranslateInterruptDebounce, "30")
	if got := parseTranslateInterruptDebounce(); got != 30*time.Second {
		t.Fatalf("integer seconds=%s", got)
	}
	t.Setenv(envTranslateInterruptDebounce, "bogus")
	if got := parseTranslateInterruptDebounce(); got != translateInterruptDebounceDefault {
		t.Fatalf("invalid=%s", got)
	}
}

func TestIsInterruptRestartError(t *testing.T) {
	yes := []string{
		"interrupted (process restarted)",
		"Interrupted (Process Restarted)",
		"interrupted (process restart)",
		"job interrupted after process restart",
		"interrupted: worker killed",
	}
	for _, msg := range yes {
		if !isInterruptRestartError(msg) {
			t.Fatalf("should match %q", msg)
		}
	}
	no := []string{
		"",
		"incomplete translation: missing Chinese on page 3",
		"Hub model returned empty content",
		"google translate: content error",
		"timeout (attempt 1/3); requeued",
		"timed out 3 times; permanently skipped",
		"interrupted 3 times; permanently skipped",
		"babeldoc produced no output PDFs",
		"existing translate process exited without outputs",
	}
	for _, msg := range no {
		if isInterruptRestartError(msg) {
			t.Fatalf("should not match %q", msg)
		}
	}
}

func TestIsRecoverablePaperID(t *testing.T) {
	yes := []string{"2609.15818", "2401.05459", "2504.01990v2", "1234.5678"}
	for _, id := range yes {
		if !isRecoverablePaperID(id) {
			t.Fatalf("should accept %q", id)
		}
	}
	no := []string{"", "2608.11274if", "IDls", "demo", "fast1", "hep-th_9901001", "2401.05459_title"}
	for _, id := range no {
		if isRecoverablePaperID(id) {
			t.Fatalf("should reject %q", id)
		}
	}
}

func TestRecoverInterruptedRequeuesAndIncrements(t *testing.T) {
	t.Setenv(envTranslateMaxInterruptRecoveries, "3")
	t.Setenv(envTranslateInterruptDebounce, "0")
	svc := interruptTestService(t)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	svc.mu.Lock()
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateFailed, Lane: translateLaneFast,
		PageCount: 12, Error: "interrupted (process restarted)",
		Finished: old, UpdatedAt: old, TimeoutCount: 1,
	}
	svc.fastQ = []string{"other"}
	svc.inQ = map[string]bool{"other": true}
	svc.mu.Unlock()

	svc.recoverInterruptedJobs()

	j, ok := svc.job("2609.15818")
	if !ok {
		t.Fatal("missing job")
	}
	if j.Status != translateQueued {
		t.Fatalf("status=%s", j.Status)
	}
	if j.InterruptCount != 1 {
		t.Fatalf("interrupt_count=%d", j.InterruptCount)
	}
	if j.TimeoutCount != 1 {
		t.Fatalf("timeout_count should stay independent, got %d", j.TimeoutCount)
	}
	if j.Error != translateInterruptRequeueError(1, 3) {
		t.Fatalf("error=%q", j.Error)
	}
	if j.StartedAt != "" {
		t.Fatalf("started_at should clear, got %q", j.StartedAt)
	}
	svc.mu.Lock()
	if svc.running["2609.15818"] {
		t.Fatal("running slot must stay free")
	}
	if len(svc.fastQ) != 2 || svc.fastQ[0] != "other" || svc.fastQ[1] != "2609.15818" {
		t.Fatalf("requeue should append to end of fast lane, got %v", svc.fastQ)
	}
	svc.mu.Unlock()

	p := overlayOne(paperEntry{ID: "2609.15818", HasLocal: true, PageCount: 12}, svc.root, svc.jobsCopy())
	if p.TranslateError != translateInterruptRequeueError(1, 3) {
		t.Fatalf("overlay error=%q", p.TranslateError)
	}
}

func TestRecoverInterruptedPermanentSkip(t *testing.T) {
	t.Setenv(envTranslateMaxInterruptRecoveries, "3")
	t.Setenv(envTranslateInterruptDebounce, "0")
	svc := interruptTestService(t)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	svc.mu.Lock()
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateFailed, Lane: translateLaneSlow,
		PageCount: 70, Error: "interrupted (process restarted)",
		Finished: old, UpdatedAt: old, InterruptCount: 2,
	}
	svc.mu.Unlock()

	svc.recoverInterruptedJobs()

	j, ok := svc.job("2609.15818")
	if !ok {
		t.Fatal("missing job")
	}
	if j.Status != translateSkipped {
		t.Fatalf("status=%s", j.Status)
	}
	if j.InterruptCount != 3 {
		t.Fatalf("interrupt_count=%d", j.InterruptCount)
	}
	if j.Error != translateInterruptSkipError(3) {
		t.Fatalf("error=%q", j.Error)
	}
	svc.mu.Lock()
	if svc.running["2609.15818"] {
		t.Fatal("running slot not freed")
	}
	if svc.inQ["2609.15818"] || len(svc.slowQ) != 0 {
		t.Fatalf("permanently skipped job must not be requeued inQ=%v slowQ=%v", svc.inQ, svc.slowQ)
	}
	svc.mu.Unlock()

	papers := []paperEntry{{
		ID: "2609.15818", HasLocal: true, Filename: "2609.15818.pdf", PageCount: 70,
		TranslateStatus: translateSkipped,
	}}
	if ids := pendingTranslateIDs(papers, true); len(ids) != 0 {
		t.Fatalf("auto-translate must not pick skipped: %v", ids)
	}
	if ids := pendingTranslateIDs(papers, false); len(ids) != 0 {
		t.Fatalf("translate-all must not pick skipped: %v", ids)
	}

	p := overlayOne(paperEntry{ID: "2609.15818", HasLocal: true, PageCount: 70}, svc.root, svc.jobsCopy())
	if p.TranslateStatus != translateSkipped || p.TranslateError != translateInterruptSkipError(3) {
		t.Fatalf("overlay %+v", p)
	}
}

func TestRecoverInterruptedLeavesIncompleteTranslation(t *testing.T) {
	t.Setenv(envTranslateInterruptDebounce, "0")
	svc := interruptTestService(t)
	svc.mu.Lock()
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateFailed, Lane: translateLaneFast,
		PageCount: 12, Error: "incomplete translation: missing Chinese on page 4",
	}
	svc.mu.Unlock()

	svc.recoverInterruptedJobs()

	j, ok := svc.job("2609.15818")
	if !ok || j.Status != translateFailed {
		t.Fatalf("quality failure should stay failed: %+v", j)
	}
	if j.InterruptCount != 0 {
		t.Fatalf("interrupt_count=%d", j.InterruptCount)
	}
	if !strings.Contains(j.Error, "incomplete translation") {
		t.Fatalf("error rewritten: %q", j.Error)
	}
	svc.mu.Lock()
	if len(svc.fastQ) != 0 {
		t.Fatalf("must not requeue quality failure: %v", svc.fastQ)
	}
	svc.mu.Unlock()
}

func TestRecoverInterruptedLeavesInvalidIDs(t *testing.T) {
	t.Setenv(envTranslateInterruptDebounce, "0")
	svc := interruptTestService(t)
	svc.mu.Lock()
	for _, id := range []string{"2608.11274if", "IDls"} {
		svc.status.Jobs[id] = translateJob{
			ID: id, Status: translateFailed,
			Error: "interrupted (process restarted)",
		}
	}
	svc.mu.Unlock()

	svc.recoverInterruptedJobs()

	svc.mu.Lock()
	for _, id := range []string{"2608.11274if", "IDls"} {
		j, ok := svc.status.Jobs[id]
		if !ok || j.Status != translateFailed {
			svc.mu.Unlock()
			t.Fatalf("%s should stay failed: ok=%v %+v", id, ok, j)
		}
		if j.InterruptCount != 0 {
			svc.mu.Unlock()
			t.Fatalf("%s interrupt_count=%d", id, j.InterruptCount)
		}
	}
	if len(svc.fastQ)+len(svc.slowQ) != 0 {
		svc.mu.Unlock()
		t.Fatalf("garbage ids must not be queued: fast=%v slow=%v", svc.fastQ, svc.slowQ)
	}
	svc.mu.Unlock()
}

func TestRecoverInterruptedDebouncesFreshMark(t *testing.T) {
	t.Setenv(envTranslateInterruptDebounce, "30s")
	svc := interruptTestService(t)
	now := time.Now().UTC().Format(time.RFC3339)
	svc.mu.Lock()
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateFailed, Lane: translateLaneFast,
		PageCount: 12, Error: "interrupted (process restarted)",
		Finished: now, UpdatedAt: now,
	}
	svc.mu.Unlock()

	svc.recoverInterruptedJobs()

	j, ok := svc.job("2609.15818")
	if !ok || j.Status != translateFailed {
		t.Fatalf("fresh interrupt must stay failed in the same tick: %+v", j)
	}
	if j.InterruptCount != 0 {
		t.Fatalf("interrupt_count=%d", j.InterruptCount)
	}
	svc.mu.Lock()
	if len(svc.fastQ) != 0 {
		t.Fatalf("must not requeue in the same tick: %v", svc.fastQ)
	}
	svc.mu.Unlock()
}

func TestRecoverJobsRequeuesStaleInterrupt(t *testing.T) {
	t.Setenv(envTranslateInterruptDebounce, "30s")
	t.Setenv(envTranslateMaxInterruptRecoveries, "3")
	svc := interruptTestService(t)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	svc.mu.Lock()
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateFailed, Lane: translateLaneFast,
		PageCount: 8, Error: "interrupted (process restarted)",
		Finished: old, UpdatedAt: old,
	}
	svc.mu.Unlock()

	svc.recoverJobs()

	j, ok := svc.job("2609.15818")
	if !ok || j.Status != translateQueued {
		t.Fatalf("startup recovery should requeue stale interrupt: %+v", j)
	}
	if j.InterruptCount != 1 {
		t.Fatalf("interrupt_count=%d", j.InterruptCount)
	}
	svc.mu.Lock()
	if len(svc.fastQ) != 1 || svc.fastQ[0] != "2609.15818" {
		t.Fatalf("fastQ=%v", svc.fastQ)
	}
	svc.mu.Unlock()
}

func TestEnqueueResetsInterruptCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "2609.15818.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.status.Jobs["2609.15818"] = translateJob{
		ID: "2609.15818", Status: translateSkipped, InterruptCount: 3, TimeoutCount: 1,
		Error: translateInterruptSkipError(3), PageCount: 10, Lane: translateLaneFast,
	}
	svc.mu.Unlock()

	papers := []paperEntry{{ID: "2609.15818", HasLocal: true, Filename: "2609.15818.pdf", PageCount: 10}}
	res := svc.enqueue([]string{"2609.15818"}, papers, false)
	if len(res.Queued) != 1 {
		t.Fatalf("manual enqueue of skipped should requeue: %+v", res)
	}
	j, ok := svc.job("2609.15818")
	if !ok || j.Status != translateQueued {
		t.Fatalf("job %+v ok=%v", j, ok)
	}
	if j.InterruptCount != 0 {
		t.Fatalf("manual re-translate should reset interrupt_count=%d", j.InterruptCount)
	}
	if j.TimeoutCount != 0 {
		t.Fatalf("manual re-translate should also reset timeout_count=%d", j.TimeoutCount)
	}
	if j.Error != "" {
		t.Fatalf("error should clear, got %q", j.Error)
	}
}

func TestPapersTranslatePostResetsInterruptCount(t *testing.T) {
	h, _ := papersHandler(t)
	srv, ok := h.(*Server)
	if !ok {
		t.Fatalf("handler type %T", h)
	}
	svc := srv.papers().translate()
	svc.putJob(translateJob{
		ID: "2401.05459", Status: translateSkipped, InterruptCount: 3,
		Error: translateInterruptSkipError(3), Lane: translateLaneFast, PageCount: 23,
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2401.05459"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var resp translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Queued) != 1 || resp.Queued[0] != "2401.05459" {
		t.Fatalf("queued=%v", resp.Queued)
	}
	j, ok := svc.job("2401.05459")
	if !ok {
		t.Fatal("missing job")
	}
	if j.InterruptCount != 0 {
		t.Fatalf("POST should reset interrupt_count=%d", j.InterruptCount)
	}
}

func TestPapersAPISurfacesInterruptSkipError(t *testing.T) {
	h, _ := papersHandler(t)
	srv, ok := h.(*Server)
	if !ok {
		t.Fatalf("handler type %T", h)
	}
	svc := srv.papers().translate()
	svc.putJob(translateJob{
		ID: "2401.05459", Status: translateSkipped, InterruptCount: 3,
		Error: translateInterruptSkipError(3), Lane: translateLaneFast, PageCount: 23,
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Papers) == 0 {
		t.Fatal("no papers")
	}
	p := cat.Papers[0]
	if p.TranslateStatus != translateSkipped {
		t.Fatalf("status=%s", p.TranslateStatus)
	}
	if p.TranslateError != translateInterruptSkipError(3) {
		t.Fatalf("error=%q", p.TranslateError)
	}
}

func TestInterruptErrorStrings(t *testing.T) {
	if !strings.Contains(translateInterruptRequeueError(1, 3), "attempt 1/3") {
		t.Fatal(translateInterruptRequeueError(1, 3))
	}
	if !strings.Contains(translateInterruptSkipError(3), "interrupted 3 times") {
		t.Fatal(translateInterruptSkipError(3))
	}
}
