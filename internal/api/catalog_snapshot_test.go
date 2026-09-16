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

func TestNoneMatchAndAcceptsGzip(t *testing.T) {
	if !noneMatch(`"abc"`, "abc") || !noneMatch("abc", "abc") || !noneMatch(`W/"abc"`, "abc") {
		t.Fatal("etag match")
	}
	if noneMatch("", "abc") || noneMatch(`"xyz"`, "abc") {
		t.Fatal("etag miss")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br, gzip;q=0.8")
	if !acceptsGzip(req) {
		t.Fatal("gzip accepted")
	}
	req.Header.Set("Accept-Encoding", "identity")
	if acceptsGzip(req) {
		t.Fatal("identity only")
	}
}

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
	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "max-age") || strings.Contains(cc, "no-store") {
		t.Fatalf("cache headers: %s", cc)
	}
	if !strings.Contains(cc, "public") || !strings.Contains(cc, "stale-while-revalidate") {
		t.Fatalf("want public SWR cache, got %s", cc)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if rr.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("vary: %s", rr.Header().Get("Vary"))
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
	if cat.CanManage {
		t.Fatal("static catalog must not overlay can_manage; progress owns that flag")
	}

	etag := rr.Header().Get("ETag")
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/api/catalog", nil)
	req.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/static/catalog-snapshot.json", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("static snapshot status=%d", rr.Code)
	}
	if rr.Header().Get("ETag") != etag {
		t.Fatalf("static etag=%s catalog etag=%s", rr.Header().Get("ETag"), etag)
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
	if !strings.Contains(body, "rs_papers_catalog_v1") || !strings.Contains(body, "hydrateCatalogCache") {
		t.Fatal("page must persist last-good catalog for instant paint")
	}
	if !strings.Contains(body, "fetchCatalogJSON") || !strings.Contains(body, "If-None-Match") {
		t.Fatal("catalog fetch must send If-None-Match and allow HTTP cache")
	}
	if strings.Contains(body, `cache: "no-store"`) {
		t.Fatal("catalog fetch must not bypass the browser cache")
	}
	if !strings.Contains(body, `updatingCatalog: "Updating catalog…"`) || !strings.Contains(body, `updatingCatalog: "正在更新目录…"`) {
		t.Fatal("updating copy must be localized")
	}
}

func TestLeanCatalogTruncatesAbstracts(t *testing.T) {
	longEN := strings.Repeat("word ", 200) // well over 400 runes
	longZH := strings.Repeat("摘要内容", 80)
	cat := papersCatalog{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Count:       1,
		Papers: []paperEntry{
			{
				Title:      "Long Abstract Paper",
				Abstract:   longEN,
				AbstractZH: longZH,
				Brief:      longEN,
				QueryHits:  []string{"secret-hit"},
			},
		},
	}
	lean := leanCatalogForSnapshot(nil, cat)
	if lean.Papers[0].Abstract == longEN || lean.Papers[0].AbstractZH == longZH {
		t.Fatal("snapshot abstracts must be truncated")
	}
	if utf8RuneCount(lean.Papers[0].Abstract) > catalogSnapshotAbstractRunes+1 {
		t.Fatalf("en runes=%d", utf8RuneCount(lean.Papers[0].Abstract))
	}
	if utf8RuneCount(lean.Papers[0].AbstractZH) > catalogSnapshotAbstractZHRunes+1 {
		t.Fatalf("zh runes=%d", utf8RuneCount(lean.Papers[0].AbstractZH))
	}
	if lean.Papers[0].QueryHits != nil {
		t.Fatal("query_hits must be omitted from the lean snapshot")
	}
	if !strings.Contains(lean.Papers[0].Abstract, "word") {
		t.Fatal("truncated EN abstract must keep searchable prefix")
	}
	if !strings.Contains(lean.Papers[0].AbstractZH, "摘要") {
		t.Fatal("truncated ZH abstract must keep card brief")
	}
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func TestCatalogSnapshotGzipAnd304(t *testing.T) {
	h, dir := papersHandler(t)
	// Inflate the fixture so gzip has something to do.
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{
				Title:     "Personal LLM Agents",
				Abstract:  strings.Repeat("Agents cooperate across tools and memory. ", 40),
				Year:      2024,
				ArxivID:   "2401.05459",
				TopicTags: []string{"survey"},
				PDFPath:   "pdfs/2401.05459_personal_llm_agents.pdf",
			},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ps := h.(*Server).papers()
	ps.invalidateCatalog()
	ps.refreshCatalogSnapshot()

	gzPath := catalogSnapshotGZPath(dir)
	gz, err := os.ReadFile(gzPath)
	if err != nil {
		t.Fatalf("gzip snapshot: %v", err)
	}
	plain, err := os.ReadFile(catalogSnapshotPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(gz) == 0 || len(gz) >= len(plain) {
		t.Fatalf("gzip %d should be smaller than plain %d", len(gz), len(plain))
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api/catalog", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding=%s", rr.Header().Get("Content-Encoding"))
	}
	if rr.Body.Len() != len(gz) {
		t.Fatalf("gzip body %d want %d", rr.Body.Len(), len(gz))
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/api/catalog", nil)
	h.ServeHTTP(rr, req)
	if rr.Header().Get("Content-Encoding") != "" {
		t.Fatal("identity response must not set Content-Encoding")
	}
	if !strings.Contains(rr.Body.String(), "Agents cooperate") {
		t.Fatal("identity body should include lean abstract prefix")
	}
}
