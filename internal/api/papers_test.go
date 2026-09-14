package api

import (
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
