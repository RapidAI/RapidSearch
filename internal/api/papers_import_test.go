package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArxivID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2504.01990", "2504.01990"},
		{"arXiv:2504.01990", "2504.01990"},
		{"arxiv:2504.01990v2", "2504.01990"},
		{"https://arxiv.org/abs/2504.01990", "2504.01990"},
		{"https://arxiv.org/abs/2504.01990v1", "2504.01990"},
		{"https://arxiv.org/pdf/2504.01990", "2504.01990"},
		{"https://arxiv.org/pdf/2504.01990.pdf", "2504.01990"},
		{"http://arxiv.org/html/2504.01990", "2504.01990"},
		{"https://export.arxiv.org/pdf/2504.01990.pdf", "2504.01990"},
		{"  2401.05459  ", "2401.05459"},
		{"not-a-paper", ""},
		{"https://example.com/paper.pdf", ""},
		{"hep-th/9901001", "hep-th/9901001"},
		{"https://arxiv.org/abs/hep-th/9901001", "hep-th/9901001"},
	}
	for _, tc := range cases {
		if got := parseArxivID(tc.in); got != tc.want {
			t.Fatalf("parseArxivID(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateTopicTag(t *testing.T) {
	ok := []string{
		"self-evolution", "security", "both", "llm-iot", "survey",
		"llm-training", "agent-tools-memory", "other",
		"LLM-Training", " Other ",
	}
	for _, tag := range ok {
		got, valid := validateTopicTag(tag)
		if !valid || got != strings.ToLower(strings.TrimSpace(tag)) {
			t.Fatalf("expected valid %q got %q valid=%v", tag, got, valid)
		}
	}
	for _, tag := range []string{"", "unknown", "llm", "training", "agent"} {
		if _, valid := validateTopicTag(tag); valid {
			t.Fatalf("expected invalid %q", tag)
		}
	}
}

func TestParseImportTarget(t *testing.T) {
	got, err := parseImportTarget("2504.01990")
	if err != nil || got.Kind != "arxiv" || got.ArxivID != "2504.01990" {
		t.Fatalf("%+v %v", got, err)
	}
	if !strings.Contains(got.PDFURL, "2504.01990") {
		t.Fatalf("pdf url %s", got.PDFURL)
	}
	got, err = parseImportTarget("https://cdn.example.com/a/paper.pdf")
	if err != nil || got.Kind != "pdf" || got.ArxivID != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := parseImportTarget("file:///etc/passwd"); err == nil {
		t.Fatal("file URL must be rejected")
	}
	if _, err := parseImportTarget(""); err == nil {
		t.Fatal("empty must be rejected")
	}
	if _, err := parseImportTarget("ftp://x/y.pdf"); err == nil {
		t.Fatal("ftp must be rejected")
	}
}

func TestImportRejectsBadTagAndURL(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/import", strings.NewReader(`{"url_or_id":"2504.01990","tag":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad tag status=%d body=%s", rr.Code, rr.Body.String())
	}

	for _, body := range []string{
		`{"url_or_id":"2504.01990"}`,
		`{"url_or_id":"2504.01990","tag":""}`,
		`{"url_or_id":"2504.01990","tag":"   "}`,
	} {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, "/papers/import", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "tag is required") {
			t.Fatalf("missing tag %s status=%d body=%s", body, rr.Code, rr.Body.String())
		}
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/import", strings.NewReader(`{"url_or_id":"not-valid","tag":"other"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad url status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/import", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET import status=%d", rr.Code)
	}
}

func TestImportPaperRejectsMissingTag(t *testing.T) {
	h, _ := papersHandler(t)
	ps := h.(*Server).papers()
	_, _, err := ps.importPaper(context.Background(), importTarget{
		ArxivID: "2504.01990",
		PDFURL:  "https://arxiv.org/pdf/2504.01990.pdf",
		Kind:    "arxiv",
	}, "")
	if err == nil {
		t.Fatal("empty tag must not ingest")
	}
	_, _, err = ps.importUploadedPaper(context.Background(), "", "paper.pdf", academicPaperPDF())
	if err == nil {
		t.Fatal("upload without tag must not ingest")
	}
}

func TestImportArxivPaper(t *testing.T) {
	h, dir := papersHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "id_list=") || strings.Contains(r.URL.Path, "/query") {
			w.Header().Set("Content-Type", "application/atom+xml")
			_, _ = w.Write([]byte(arxivAtomFixture("2504.01990")))
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 imported-bytes"))
	}))
	t.Cleanup(srv.Close)

	ps := h.(*Server).papers()
	ps.importClient = srv.Client()
	ps.allowPrivateFetch = true
	ps.arxivAPIBase = srv.URL + "/query"

	paper, updated, err := ps.importPaper(context.Background(), importTarget{
		ArxivID: "2504.01990",
		PDFURL:  srv.URL + "/pdf/2504.01990.pdf",
		Kind:    "arxiv",
	}, "llm-training")
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("first import should insert")
	}
	if paper.ArxivID != "2504.01990" || paper.Title != "A Training Paper" {
		t.Fatalf("meta not applied: %+v", paper)
	}
	if len(paper.TopicTags) != 1 || paper.TopicTags[0] != "llm-training" {
		t.Fatalf("tags %+v", paper.TopicTags)
	}
	if paper.Source != paperSourceManual || paper.Year != 2025 {
		t.Fatalf("source/year %+v", paper)
	}
	if !paperHasLocalPDF(dir, paper) {
		t.Fatalf("missing pdf for %+v", paper)
	}

	// Upsert same arxiv id with a new tag.
	paper2, updated, err := ps.importPaper(context.Background(), importTarget{
		ArxivID: "2504.01990",
		PDFURL:  srv.URL + "/pdf/2504.01990.pdf",
		Kind:    "arxiv",
	}, "survey")
	if err != nil || !updated {
		t.Fatalf("upsert err=%v updated=%v", err, updated)
	}
	if !containsString(paper2.TopicTags, "survey") {
		t.Fatalf("retagged %+v", paper2.TopicTags)
	}

	ps.invalidateCatalog()
	cat, err := ps.catalog()
	if err != nil {
		t.Fatal(err)
	}
	// After upsert, tag is survey (replaced, not merged).
	got := filterPapers(cat.Papers, "", "survey")
	if len(got) == 0 {
		t.Fatalf("survey tag not filterable: %+v", cat.Papers)
	}
	found := false
	for _, p := range cat.Papers {
		if p.ArxivID == "2504.01990" && p.HasLocal {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported paper missing from catalog: %+v", cat.Papers)
	}
}

func TestImportGenericPDFHTTP(t *testing.T) {
	h, dir := papersHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 generic"))
	}))
	t.Cleanup(srv.Close)
	ps := h.(*Server).papers()
	ps.importClient = srv.Client()
	ps.allowPrivateFetch = true

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/import", strings.NewReader(
		`{"url_or_id":"`+srv.URL+`/cool-agent-memory.pdf","tag":"other"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp importResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.Paper.TopicTags) == 0 || resp.Paper.TopicTags[0] != "other" || !resp.Paper.HasLocal {
		t.Fatalf("%+v", resp)
	}
	if resp.Paper.Source != paperSourceManual {
		t.Fatalf("source %+v", resp.Paper)
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "pdfs"))
	foundPDF := false
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".pdf") && e.Name() != "2401.05459_personal_llm_agents.pdf" {
			foundPDF = true
		}
	}
	if !foundPDF {
		t.Fatalf("pdfs=%v", importDirNames(ents))
	}

	got := filterPapers([]paperEntry{resp.Paper}, "", "other")
	if len(got) != 1 {
		t.Fatalf("other tag filter: %+v", got)
	}
}

func TestImportRejectsNonPDF(t *testing.T) {
	h, _ := papersHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>nope</html>"))
	}))
	t.Cleanup(srv.Close)
	ps := h.(*Server).papers()
	ps.importClient = srv.Client()
	ps.allowPrivateFetch = true

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/import", strings.NewReader(
		`{"url_or_id":"`+srv.URL+`/x.pdf","tag":"survey"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestImportRateLimit(t *testing.T) {
	lim := newImportLimiter()
	now := time.Now()
	for i := 0; i < importPerIP; i++ {
		if !lim.allow("1.2.3.4", now) {
			t.Fatalf("hit %d should be allowed", i)
		}
	}
	if lim.allow("1.2.3.4", now) {
		t.Fatal("expected rate limit")
	}
	if !lim.allow("9.9.9.9", now) {
		t.Fatal("other IP should be allowed")
	}
}

func TestImportBlockedPrivateURL(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1/secret.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePublicURL(u); err == nil {
		t.Fatal("loopback must be blocked")
	}
	u, err = url.Parse("http://169.254.169.254/latest/meta-data")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePublicURL(u); err == nil {
		t.Fatal("metadata must be blocked")
	}
	u, err = url.Parse("http://localhost/x.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePublicURL(u); err == nil {
		t.Fatal("localhost must be blocked")
	}
}

func TestFilterPapersNewTags(t *testing.T) {
	in := []paperEntry{
		{Title: "Train", TopicTags: []string{"llm-training"}},
		{Title: "Mem", TopicTags: []string{"agent-tools-memory"}},
		{Title: "Misc", TopicTags: []string{"other"}},
	}
	if got := filterPapers(in, "", "llm-training"); len(got) != 1 || got[0].Title != "Train" {
		t.Fatalf("%+v", got)
	}
	if got := filterPapers(in, "", "agent-tools-memory"); len(got) != 1 || got[0].Title != "Mem" {
		t.Fatalf("%+v", got)
	}
	if got := filterPapers(in, "", "other"); len(got) != 1 || got[0].Title != "Misc" {
		t.Fatalf("%+v", got)
	}
}

func paperHasLocalPDF(dir string, p paperEntry) bool {
	name := filepath.Base(p.PDFPath)
	if name == "" || name == "." {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "pdfs", name))
	return err == nil && st.Size() > 0
}

func importDirNames(ents []os.DirEntry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func arxivAtomFixture(id string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>
    <id>http://arxiv.org/abs/` + id + `v1</id>
    <title>A Training Paper</title>
    <summary>An abstract about LLM training.</summary>
    <published>2025-04-02T00:00:00Z</published>
    <updated>2025-04-03T00:00:00Z</updated>
    <author><name>Ada Lovelace</name></author>
    <link rel="alternate" href="https://arxiv.org/abs/` + id + `" type="text/html"/>
    <link title="pdf" href="https://arxiv.org/pdf/` + id + `" rel="related" type="application/pdf"/>
    <arxiv:doi>10.0000/demo</arxiv:doi>
  </entry>
</feed>`
}

func TestImportUploadAcademicPaper(t *testing.T) {
	h, dir := papersHandler(t)
	rr := postImportUpload(t, h, "agent-tools-memory", "agent-memory-study.pdf", academicPaperPDF())
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp importResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Paper.TopicTags[0] != "agent-tools-memory" || !resp.Paper.HasLocal {
		t.Fatalf("%+v", resp)
	}
	if resp.Paper.Source != paperSourceManual {
		t.Fatalf("source %+v", resp.Paper)
	}
	if !containsString(resp.Paper.QueryHits, importUploadQueryHit) {
		t.Fatalf("query hits %+v", resp.Paper.QueryHits)
	}
	if resp.Paper.Filename == "" || !strings.HasSuffix(resp.Paper.Filename, ".pdf") {
		t.Fatalf("filename %+v", resp.Paper)
	}
	if st, err := os.Stat(filepath.Join(dir, "pdfs", resp.Paper.Filename)); err != nil || st.Size() == 0 {
		t.Fatalf("missing pdf %s: %v", resp.Paper.Filename, err)
	}
	if !strings.Contains(strings.ToLower(resp.Paper.Title+resp.Paper.Abstract), "memory") {
		t.Fatalf("expected extracted meta: %+v", resp.Paper)
	}

	ps := h.(*Server).papers()
	ps.invalidateCatalog()
	cat, err := ps.catalog()
	if err != nil {
		t.Fatal(err)
	}
	got := filterPapers(cat.Papers, "", "agent-tools-memory")
	if len(got) == 0 {
		t.Fatalf("uploaded paper missing from catalog: %+v", cat.Papers)
	}
}

func TestImportUploadRejectsNonPaperAndBadTag(t *testing.T) {
	h, dir := papersHandler(t)
	before := pdfCount(t, dir)

	rr := postImportUpload(t, h, "other", "slides.pdf", slideDeckPDF())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("slides status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp importResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ErrorCode != importErrNotPaper {
		t.Fatalf("slides resp %+v", resp)
	}

	rr = postImportUpload(t, h, "survey", "note.pdf", helloWorldPDF())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("hello status=%d %s", rr.Code, rr.Body.String())
	}

	rr = postImportUpload(t, h, "survey", "x.txt", []byte("not a pdf at all, just text"))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), importErrNotPDF) && !strings.Contains(rr.Body.String(), "not a PDF") {
		t.Fatalf("not pdf status=%d %s", rr.Code, rr.Body.String())
	}

	rr = postImportUpload(t, h, "", "paper.pdf", academicPaperPDF())
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "tag is required") {
		t.Fatalf("missing tag status=%d %s", rr.Code, rr.Body.String())
	}

	rr = postImportUpload(t, h, "nope", "paper.pdf", academicPaperPDF())
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "tag is required") {
		t.Fatalf("unknown tag status=%d %s", rr.Code, rr.Body.String())
	}

	if pdfCount(t, dir) != before {
		t.Fatalf("rejected uploads must not write pdfs: before=%d after=%d names=%v", before, pdfCount(t, dir), listPDFNames(t, dir))
	}
}

func postImportUpload(t *testing.T, h http.Handler, tag, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if tag != "\x00omit" {
		if err := mw.WriteField("tag", tag); err != nil {
			t.Fatal(err)
		}
	}
	if body != nil {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/import", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(rr, req)
	return rr
}

func pdfCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, "pdfs"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".pdf") {
			n++
		}
	}
	return n
}

func listPDFNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, "pdfs"))
	if err != nil {
		t.Fatal(err)
	}
	return importDirNames(ents)
}
