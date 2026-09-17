package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTranslatePapers(dir string, specs []paperEntry) []paperEntry {
	var papers []paperEntry
	for _, p := range specs {
		if p.Filename == "" {
			p.Filename = p.ID + ".pdf"
		}
		_ = os.WriteFile(filepath.Join(dir, "pdfs", p.Filename), []byte("%PDF "+p.ID), 0o644)
		if !p.HasLocal {
			p.HasLocal = true
		}
		papers = append(papers, p)
	}
	return papers
}

func fastQueuedIDs(svc *translateService) []string {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return append([]string(nil), svc.fastQ...)
}

func slowQueuedIDs(svc *translateService) []string {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return append([]string(nil), svc.slowQ...)
}

func TestPriorityEnqueueInsertsAheadOfNormalQueue(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()

	papers := testTranslatePapers(dir, []paperEntry{
		{ID: "a", PageCount: 10},
		{ID: "b", PageCount: 12},
		{ID: "c", PageCount: 8},
		{ID: "hot", PageCount: 9},
	})
	if res := svc.enqueue([]string{"a", "b", "c"}, papers, false); len(res.Queued) != 3 {
		t.Fatalf("tail enqueue %+v", res)
	}
	if res := svc.enqueueAt([]string{"hot"}, papers, false, true); len(res.Queued) != 1 {
		t.Fatalf("priority enqueue %+v", res)
	}
	got := fastQueuedIDs(svc)
	want := []string{"hot", "a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fastQ=%v want %v", got, want)
	}
}

func TestPriorityEnqueueBumpsAlreadyQueued(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()

	papers := testTranslatePapers(dir, []paperEntry{
		{ID: "a", PageCount: 10},
		{ID: "b", PageCount: 12},
		{ID: "c", PageCount: 8},
	})
	if res := svc.enqueue([]string{"a", "b", "c"}, papers, false); len(res.Queued) != 3 {
		t.Fatalf("tail enqueue %+v", res)
	}
	// Without priority, a second click is a no-op.
	res := svc.enqueue([]string{"b"}, papers, false)
	if len(res.Queued) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("dup without priority %+v", res)
	}
	if got := fastQueuedIDs(svc); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("queue changed on no-op: %v", got)
	}

	res = svc.enqueueAt([]string{"b"}, papers, false, true)
	if len(res.Queued) != 1 || res.Queued[0] != "b" {
		t.Fatalf("priority bump %+v", res)
	}
	got := fastQueuedIDs(svc)
	if strings.Join(got, ",") != "b,a,c" {
		t.Fatalf("bumped fastQ=%v want b,a,c", got)
	}
	n := 0
	for _, id := range got {
		if id == "b" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate queue entries for b: %v", got)
	}
}

func TestPriorityEnqueueDoesNotPreemptRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.running["run"] = true
	svc.status.Jobs["run"] = translateJob{ID: "run", Status: translateRunning, Lane: translateLaneFast, PageCount: 10}
	svc.mu.Unlock()

	papers := testTranslatePapers(dir, []paperEntry{
		{ID: "run", PageCount: 10},
		{ID: "wait", PageCount: 11},
		{ID: "hot", PageCount: 9},
	})
	if res := svc.enqueue([]string{"wait"}, papers, false); len(res.Queued) != 1 {
		t.Fatalf("wait %+v", res)
	}
	if res := svc.enqueueAt([]string{"run"}, papers, false, true); res.Rejected["run"] != "正在翻译中" {
		t.Fatalf("running must stay rejected: %+v", res)
	}
	if res := svc.enqueueAt([]string{"hot"}, papers, false, true); len(res.Queued) != 1 {
		t.Fatalf("hot %+v", res)
	}
	got := fastQueuedIDs(svc)
	if strings.Join(got, ",") != "hot,wait" {
		t.Fatalf("pending after running=%v", got)
	}
	if next := svc.tryPop(); next != "hot" {
		t.Fatalf("next start=%q want hot (after currently running)", next)
	}
}

func TestPriorityEnqueueStaysInLane(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()

	papers := testTranslatePapers(dir, []paperEntry{
		{ID: "f1", PageCount: 12},
		{ID: "f2", PageCount: 20},
		{ID: "s1", PageCount: 70},
		{ID: "s2", PageCount: 80},
		{ID: "shot", PageCount: 90},
	})
	if res := svc.enqueue([]string{"f1", "f2", "s1", "s2"}, papers, false); len(res.Queued) != 4 {
		t.Fatalf("seed %+v", res)
	}
	if res := svc.enqueueAt([]string{"shot"}, papers, false, true); len(res.Queued) != 1 {
		t.Fatalf("slow priority %+v", res)
	}
	if got := fastQueuedIDs(svc); strings.Join(got, ",") != "f1,f2" {
		t.Fatalf("fast lane must be unchanged: %v", got)
	}
	if got := slowQueuedIDs(svc); strings.Join(got, ",") != "shot,s1,s2" {
		t.Fatalf("slow priority head=%v", got)
	}

	if res := svc.enqueueAt([]string{"s2"}, papers, false, true); len(res.Queued) != 1 {
		t.Fatalf("slow bump %+v", res)
	}
	if got := slowQueuedIDs(svc); strings.Join(got, ",") != "s2,shot,s1" {
		t.Fatalf("slow bump order=%v", got)
	}
}

func TestPriorityEnqueueIgnoredForTranslateAll(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.WriteFile(filepath.Join(dir, "translate.pause"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p-a", "p-b"} {
		if err := os.WriteFile(filepath.Join(dir, "pdfs", id+".pdf"), []byte("%PDF "+id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{Title: "A", ArxivID: "p-a", PDFPath: "pdfs/p-a.pdf"},
			{Title: "B", ArxivID: "p-b", PDFPath: "pdfs/p-b.pdf"},
			{Title: "Personal LLM Agents", ArxivID: "2401.05459", PDFPath: "pdfs/2401.05459_personal_llm_agents.pdf"},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	h.(*Server).papers().invalidateCatalog()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"all":true,"priority":true}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var enq translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if !enq.OK || len(enq.Queued) < 2 {
		t.Fatalf("all enqueue %+v", enq)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/translate", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	var pub map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &pub); err != nil {
		t.Fatal(err)
	}
	fast, _ := pub["fast_queued"].([]any)
	if len(fast) < 2 {
		t.Fatalf("fast_queued %+v", pub["fast_queued"])
	}
	// Catalog sort is title-asc here: p-a, p-b, 2401.05459. Tail append
	// keeps that order; front-insert of the whole backlog would reverse it.
	first, _ := fast[0].(string)
	if first != "p-a" {
		t.Fatalf("translate-all must ignore priority (FIFO tail), first=%q queue=%v", first, fast)
	}
}

func TestPapersTranslatePostPriorityNearFront(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.WriteFile(filepath.Join(dir, "translate.pause"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tail-a", "tail-b", "hot"} {
		if err := os.WriteFile(filepath.Join(dir, "pdfs", id+".pdf"), []byte("%PDF "+id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{Title: "Tail A", ArxivID: "tail-a", PDFPath: "pdfs/tail-a.pdf"},
			{Title: "Tail B", ArxivID: "tail-b", PDFPath: "pdfs/tail-b.pdf"},
			{Title: "Hot", ArxivID: "hot", PDFPath: "pdfs/hot.pdf"},
			{Title: "Personal LLM Agents", ArxivID: "2401.05459", PDFPath: "pdfs/2401.05459_personal_llm_agents.pdf"},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	h.(*Server).papers().invalidateCatalog()

	// Seed a normal tail backlog first.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"tail-a"}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed a %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"tail-b"}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed b %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"hot","priority":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("priority without auth status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"hot","priority":true}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("priority %d %s", rr.Code, rr.Body.String())
	}
	var enq translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if !enq.OK || strings.Join(enq.Queued, ",") != "hot" {
		t.Fatalf("priority resp %+v", enq)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/translate", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	var pub map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &pub); err != nil {
		t.Fatal(err)
	}
	fast, _ := pub["fast_queued"].([]any)
	ids := make([]string, 0, len(fast))
	for _, v := range fast {
		ids = append(ids, v.(string))
	}
	joined := strings.Join(ids, ",")
	if !strings.HasPrefix(joined, "hot,") {
		t.Fatalf("priority must sit at lane head, got %v", ids)
	}
	if !strings.Contains(joined, "tail-a") || !strings.Contains(joined, "tail-b") {
		t.Fatalf("normal backlog missing: %v", ids)
	}
}
