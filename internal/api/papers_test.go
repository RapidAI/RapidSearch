package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLocalPDFName(t *testing.T) {
	root := t.TempDir()
	pdfDir := filepath.Join(root, "pdfs")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2401.05459_personal_llm_agents_insights_and_survey.pdf"
	if err := os.WriteFile(filepath.Join(pdfDir, name), []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLocalPDFName(root, name)
	if err != nil || got != name {
		t.Fatalf("by filename: got %q %v", got, err)
	}
	got, err = resolveLocalPDFName(root, "2401.05459")
	if err != nil || got != name {
		t.Fatalf("by arxiv: got %q %v", got, err)
	}
	if _, err := resolveLocalPDFName(root, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal reject")
	}
	if _, err := resolveLocalPDFName(root, "missing-id"); err == nil {
		t.Fatal("expected missing")
	}
}

func TestFilterPapers(t *testing.T) {
	in := []paperEntry{
		{Title: "Alpha Security", TopicTags: []string{"security"}, Abstract: "foo", ArxivID: "1.2"},
		{Title: "Beta Survey", TopicTags: []string{"survey"}, Abstract: "bar", ArxivID: "3.4"},
	}
	got := filterPapers(in, "alpha", "")
	if len(got) != 1 || got[0].Title != "Alpha Security" {
		t.Fatalf("%+v", got)
	}
	got = filterPapers(in, "", "survey")
	if len(got) != 1 || got[0].Title != "Beta Survey" {
		t.Fatalf("%+v", got)
	}
}

func TestBriefText(t *testing.T) {
	if briefText("short", 100) != "short" {
		t.Fatal("short")
	}
	s := briefText("one two three four five six seven eight nine ten eleven twelve thirteen", 40)
	if len([]rune(s)) < 10 || !strings.HasSuffix(s, "…") {
		t.Fatalf("brief=%q", s)
	}
}

func TestPapersPageLightTheme(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-theme="light"`) {
		t.Fatal("default theme is not light")
	}
	if !strings.Contains(body, "--bg: #f5f6f8") && !strings.Contains(body, "--bg:#f5f6f8") {
		t.Fatal("missing light background token")
	}
	if strings.Contains(body, "id=\"base-url\"") || strings.Contains(body, "id=\"test-cfg\"") || strings.Contains(body, "id=\"save-cfg\"") {
		t.Fatal("LLM settings panel should have moved to /settings")
	}
	if !strings.Contains(body, "id=\"llm-settings-link\"") || !strings.Contains(body, "翻译 LLM 请到「") {
		t.Fatal("papers page should link Translation LLM settings to /settings")
	}
	if !strings.Contains(body, "中文版") || !strings.Contains(body, "中英对照") {
		t.Fatal("missing translated download labels")
	}
	if !strings.Contains(body, `Authorization`) || !strings.Contains(body, "withToken") {
		t.Fatal("page must forward operator ?token= to API and PDF links")
	}
}
