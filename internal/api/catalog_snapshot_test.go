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

func TestCatalogSnapshotInterval(t *testing.T) {
	t.Setenv(envCatalogSnapshotInterval, "")
	if catalogSnapshotInterval() != catalogSnapshotIntervalDef {
		t.Fatal("default")
	}
	t.Setenv(envCatalogSnapshotInterval, "90s")
	if catalogSnapshotInterval() != 90*time.Second {
		t.Fatal("duration")
	}
	t.Setenv(envCatalogSnapshotInterval, "45")
	if catalogSnapshotInterval() != 45*time.Second {
		t.Fatal("integer seconds")
	}
	t.Setenv(envCatalogSnapshotInterval, "nope")
	if catalogSnapshotInterval() != catalogSnapshotIntervalDef {
		t.Fatal("invalid falls back")
	}
}

func TestPapersCatalogSnapshotWrittenAndServed(t *testing.T) {
	h, dir := papersHandler(t)
	ps := h.(*Server).papers()
	ps.refreshCatalogSnapshot()

	path := catalogSnapshotPath(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("snapshot file: %v", err)
	}
	var snap papersCatalog
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.SnapshotAt == "" || snap.SnapshotETag == "" {
		t.Fatalf("snapshot identity missing: %+v", snap)
	}
	if snap.TranslateProgress != nil {
		t.Fatal("static snapshot must omit live translate_progress")
	}
	if snap.Count != 1 || len(snap.Papers) != 1 {
		t.Fatalf("snapshot papers: %+v", snap)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api/catalog", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("cache headers: %s", rr.Header().Get("Cache-Control"))
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if cat.CanManage {
		t.Fatal("anonymous catalog can_manage must be false")
	}
	if cat.TranslateProgress != nil {
		t.Fatal("catalog route must not embed translate_progress")
	}
	if cat.SnapshotETag == "" || len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	if cat.Papers[0].Title != "Personal LLM Agents" {
		t.Fatalf("paper=%+v", cat.Papers[0])
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/api/catalog", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if !cat.CanManage {
		t.Fatal("admin catalog can_manage must be true")
	}
}

func TestPapersProgressSmallJSON(t *testing.T) {
	h, _ := papersHandler(t)
	srv, ok := h.(*Server)
	if !ok {
		t.Fatalf("handler type %T", h)
	}
	ps := srv.papers()
	ps.refreshCatalogSnapshot()
	svc := ps.translate()
	svc.putJob(translateJob{
		ID: "2401.05459", Status: translateRunning,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	svc.mu.Lock()
	svc.running["2401.05459"] = true
	svc.mu.Unlock()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api/progress", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	if n := rr.Body.Len(); n > 50*1024 {
		t.Fatalf("progress JSON too large: %d", n)
	}
	var prog papersProgress
	if err := json.Unmarshal(rr.Body.Bytes(), &prog); err != nil {
		t.Fatal(err)
	}
	if prog.TranslateProgress == nil || !prog.TranslateProgress.Active {
		t.Fatalf("progress: %+v", prog.TranslateProgress)
	}
	if prog.TranslateProgress.Running < 1 {
		t.Fatalf("expected running: %+v", prog.TranslateProgress)
	}
	if prog.SnapshotETag == "" {
		t.Fatal("progress should echo snapshot etag")
	}
	if _, ok := prog.Jobs["2401.05459"]; !ok {
		t.Fatalf("jobs overlay: %+v", prog.Jobs)
	}
	if strings.Contains(rr.Body.String(), `"abstract":`) {
		t.Fatal("progress must not include paper abstracts")
	}
	if strings.Contains(rr.Body.String(), "A survey of personal agents") {
		t.Fatal("progress leaked catalog abstract text")
	}
}

func TestInvalidateCatalogRefreshesSnapshot(t *testing.T) {
	h, dir := papersHandler(t)
	ps := h.(*Server).papers()
	ps.refreshCatalogSnapshot()
	first := ps.snapETag
	if first == "" {
		t.Fatal("empty etag")
	}

	man := papersManifest{
		GeneratedAt: "2026-02-02T00:00:00Z",
		Papers: []paperEntry{
			{Title: "New Paper", Abstract: "hello", Year: 2026, ArxivID: "2602.00001", TopicTags: []string{"survey"}},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ps.invalidateCatalog()
	// invalidateCatalog kicks the loop; wait briefly then force a sync refresh
	// so the test does not depend on scheduler timing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ps.refreshCatalogSnapshot()
		if ps.snapETag != first && ps.snapCat.Count == 1 && ps.snapCat.Papers[0].Title == "New Paper" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("snapshot not refreshed: etag=%s title=%v", ps.snapETag, ps.snapCat.Papers)
}

func TestPapersPageWiresCatalogAndProgress(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "/papers/api/catalog") || !strings.Contains(body, "/papers/api/progress") {
		t.Fatal("page must load snapshot then progress")
	}
	if !strings.Contains(body, "loadCatalog") || !strings.Contains(body, "loadProgress") {
		t.Fatal("page must split catalog and progress fetches")
	}
	if !strings.Contains(body, `loadingCatalog: "Loading catalog…"`) || !strings.Contains(body, `loadingCatalog: "正在加载目录…"`) {
		t.Fatal("loading copy must be localized")
	}
	if !strings.Contains(body, `catalogUnavailable: "Catalog snapshot is unavailable."`) ||
		!strings.Contains(body, `catalogUnavailable: "目录快照不可用。"`) {
		t.Fatal("catalog error copy must be localized")
	}
	if strings.Contains(body, `setInterval(function () { load({ silent: true }); }`) {
		t.Fatal("banner poll must not refetch the full catalog")
	}
}
