package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pagesOf(n int) [][]string {
	out := make([][]string, n)
	for i := 0; i < n; i++ {
		out[i] = []string{fmt.Sprintf("Page %d Abstract Introduction References", i+1)}
	}
	return out
}

func TestPaperTranslateSkipReason(t *testing.T) {
	if paperTooManyPages(paperEntry{PageCount: 0}) {
		t.Fatal("unknown page count must not skip")
	}
	if paperTooManyPages(paperEntry{PageCount: 100}) {
		t.Fatal("100 pages must be allowed")
	}
	if !paperTooManyPages(paperEntry{PageCount: 101}) {
		t.Fatal("101 pages must skip")
	}
	if got := paperTranslateSkipReason(paperEntry{PageCount: 101}); got != translateSkipTooManyPages {
		t.Fatalf("reason=%q", got)
	}
	if got := paperTranslateSkipReason(paperEntry{PageCount: 100}); got != "" {
		t.Fatalf("100 must have no skip reason, got %q", got)
	}
}

func TestPDFPageCountFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "three.pdf")
	if err := os.WriteFile(path, buildTextPDF(pagesOf(3), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := pdfPageCountFile(path); n != 3 {
		t.Fatalf("page count=%d", n)
	}
	if n := pdfPageCountBytes(buildTextPDF(pagesOf(101), nil)); n != 101 {
		t.Fatalf("bytes page count=%d", n)
	}
}

func TestPendingTranslateIDsPageGate(t *testing.T) {
	papers := []paperEntry{
		{ID: "short", HasLocal: true, PageCount: 12},
		{ID: "long", HasLocal: true, PageCount: 101},
		{ID: "edge", HasLocal: true, PageCount: 100},
	}
	auto := pendingTranslateIDs(papers, true)
	if len(auto) != 2 || auto[0] != "short" || auto[1] != "edge" {
		t.Fatalf("auto pending %+v", auto)
	}
	all := pendingTranslateIDs(papers, false)
	if len(all) != 3 {
		t.Fatalf("translate-all should include over-limit ids for reject: %+v", all)
	}
}

func TestEnqueueRejectsTooManyPages(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "long.pdf"), buildTextPDF(pagesOf(101), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "edge.pdf"), buildTextPDF(pagesOf(100), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()

	papers := []paperEntry{
		{ID: "long", HasLocal: true, Filename: "long.pdf", PageCount: 101},
		{ID: "edge", HasLocal: true, Filename: "edge.pdf", PageCount: 100},
	}
	res := svc.enqueue([]string{"long", "edge"}, papers, true)
	if res.Rejected["long"] != translateSkipTooManyPages {
		t.Fatalf("expected too_many_pages, got %+v", res)
	}
	if len(res.Queued) != 1 || res.Queued[0] != "edge" {
		t.Fatalf("100-page paper must queue: %+v", res)
	}
}

func TestEnrichPaperComputesPageCountAndSkipReason(t *testing.T) {
	dir := t.TempDir()
	pdfDir := filepath.Join(dir, "pdfs")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "long.pdf"
	if err := os.WriteFile(filepath.Join(pdfDir, name), buildTextPDF(pagesOf(101), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	p := enrichPaper(paperEntry{Title: "Long", PDFPath: "pdfs/" + name}, pdfDir)
	if p.PageCount != 101 {
		t.Fatalf("page_count=%d", p.PageCount)
	}
	if p.TranslateSkipReason != translateSkipTooManyPages {
		t.Fatalf("skip=%q", p.TranslateSkipReason)
	}
	if !p.HasLocal {
		t.Fatal("expected local pdf")
	}
}

func TestPapersAPIPageCountAndSkipReason(t *testing.T) {
	h, dir := papersHandler(t)
	name := "2401.05459_personal_llm_agents.pdf"
	if err := os.WriteFile(filepath.Join(dir, "pdfs", name), buildTextPDF(pagesOf(101), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	manPath := filepath.Join(dir, "manifest.json")
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(manPath, now, now); err != nil {
		t.Fatal(err)
	}
	h.(*Server).papers().invalidateCatalog()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	p := cat.Papers[0]
	if p.PageCount != 101 {
		t.Fatalf("page_count=%d", p.PageCount)
	}
	if p.TranslateSkipReason != translateSkipTooManyPages {
		t.Fatalf("skip=%q paper=%+v", p.TranslateSkipReason, p)
	}
}

func TestPapersTranslatePostRejectsTooManyPages(t *testing.T) {
	h, dir := papersHandler(t)
	name := "2401.05459_personal_llm_agents.pdf"
	if err := os.WriteFile(filepath.Join(dir, "pdfs", name), buildTextPDF(pagesOf(101), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "manifest.json"), now, now); err != nil {
		t.Fatal(err)
	}
	h.(*Server).papers().invalidateCatalog()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2401.05459"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var enq translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if enq.OK {
		t.Fatalf("expected reject ok=false: %+v", enq)
	}
	if enq.Error != translateSkipTooManyPages {
		t.Fatalf("error=%q", enq.Error)
	}
	if enq.Rejected["2401.05459"] != translateSkipTooManyPages {
		t.Fatalf("rejected %+v", enq.Rejected)
	}
	if len(enq.Queued) != 0 {
		t.Fatalf("queued %+v", enq.Queued)
	}
}

func TestPapersTranslateAllReportsTooManyPages(t *testing.T) {
	h, dir := papersHandler(t)
	name := "2401.05459_personal_llm_agents.pdf"
	if err := os.WriteFile(filepath.Join(dir, "pdfs", name), buildTextPDF(pagesOf(101), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "manifest.json"), now, now); err != nil {
		t.Fatal(err)
	}
	h.(*Server).papers().invalidateCatalog()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"all":true}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var enq translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if enq.Rejected["2401.05459"] != translateSkipTooManyPages {
		t.Fatalf("rejected %+v", enq)
	}
	if len(enq.Queued) != 0 {
		t.Fatalf("queued %+v", enq.Queued)
	}
}
