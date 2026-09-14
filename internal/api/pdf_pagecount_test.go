package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func nPagePDF(n int) []byte {
	if n < 1 {
		n = 1
	}
	pages := make([][]string, n)
	for i := 0; i < n; i++ {
		pages[i] = []string{fmt.Sprintf("Page %d of a research paper.", i+1)}
	}
	return buildTextPDF(pages, nil)
}

func TestPDFPageCountFromBytesUsesPagesCount(t *testing.T) {
	if got := pdfPageCountFromBytes(academicPaperPDF()); got != 2 {
		t.Fatalf("academic paper pages=%d", got)
	}
	if got := pdfPageCountFromBytes(nPagePDF(50)); got != 50 {
		t.Fatalf("50-page fixture=%d", got)
	}
	if got := pdfPageCountFromBytes(nPagePDF(51)); got != 51 {
		t.Fatalf("51-page fixture=%d", got)
	}
	if got := pdfPageCountFromBytes([]byte("%PDF-1.4 not a real tree")); got != 0 {
		t.Fatalf("unknown stub=%d", got)
	}
}

func TestCatalogBackfillPersistsPageCount(t *testing.T) {
	dir := t.TempDir()
	pdfDir := filepath.Join(dir, "pdfs")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2401.05459_pages.pdf"
	if err := os.WriteFile(filepath.Join(pdfDir, name), academicPaperPDF(), 0o644); err != nil {
		t.Fatal(err)
	}
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{{
			Title:   "Page Count Paper",
			Year:    2024,
			ArxivID: "2401.05459",
			PDFPath: "pdfs/" + name,
		}},
	}
	if err := persistManifestJSON(filepath.Join(dir, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}

	ps := &papersStore{root: dir}
	cat, err := ps.catalogBase()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Papers) != 1 || cat.Papers[0].PageCount != 2 {
		t.Fatalf("catalog page_count %+v", cat.Papers)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted papersManifest
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Papers) != 1 || persisted.Papers[0].PageCount != 2 {
		t.Fatalf("manifest page_count not persisted: %s", raw)
	}
	if persisted.Papers[0].PDFPath == "" {
		t.Fatal("persist must keep pdf_path")
	}
}
