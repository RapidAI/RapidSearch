package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"search-service/internal/search"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *Server) useTestPDFHost(t *testing.T, srv *httptest.Server) {
	t.Helper()
	host := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
	s.papers().httpClient = &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			switch clone.URL.Hostname() {
			case "arxiv.org", "export.arxiv.org", "www.arxiv.org":
				clone.URL.Scheme = "http"
				clone.URL.Host = host
				clone.Host = host
			case strings.Split(host, ":")[0]:
				// httptest host (127.0.0.1 or localhost)
			default:
				if clone.URL.Host != host {
					return nil, fmt.Errorf("blocked unexpected host %s", clone.URL.Host)
				}
			}
			return http.DefaultTransport.RoundTrip(clone)
		}),
	}
}

const tinyPDF = "%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Count 1 /Kids [3 0 R] >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\ntrailer\n%%EOF\n"

func papersRecoverServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PAPERS_DIR", root)
	t.Setenv("SEARCH_TOKEN", "papers-secret")
	t.Setenv("HUB_AUTH_BASES", "http://127.0.0.1:1")
	h := New(nil, "", nil, nil)
	t.Cleanup(func() { search.ActivateStore(nil) })
	s := h.(*Server)
	_ = s.papers()
	if err := os.MkdirAll(filepath.Join(root, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func writeTestManifest(t *testing.T, root string, papers []map[string]interface{}) {
	t.Helper()
	man := rawPapersManifest{
		GeneratedAt: "2026-09-17T00:00:00Z",
		Papers:      papers,
	}
	if err := writeRawManifestAtomic(filepath.Join(root, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
}

func authRecover(s *Server, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/pdf/recover", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer papers-secret")
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)
	return rr
}

func TestRecoverPDFSuccess(t *testing.T) {
	s, root := papersRecoverServer(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing user-agent")
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, tinyPDF)
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":      "Recover Me",
			"arxiv_id":   "2609.03747",
			"source_url": "https://arxiv.org/abs/2609.03747",
			"pdf_url":    srv.URL + "/pdf/2609.03747.pdf",
			"pdf_path":   "",
			"year":       2026,
		},
	})

	rr := authRecover(s, `{"id":"2609.03747"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out recoverOneResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.AlreadyPresent {
		t.Fatalf("%+v", out)
	}
	if !out.Paper.HasLocal || out.Paper.LocalPDF == "" {
		t.Fatalf("paper not marked local: %+v", out.Paper)
	}
	if out.Paper.ArxivID != "2609.03747" {
		t.Fatalf("arxiv_id=%q", out.Paper.ArxivID)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}

	name := filepath.Base(out.Paper.LocalPDF)
	st, err := os.Stat(filepath.Join(root, "pdfs", name))
	if err != nil || st.Size() < 5 {
		t.Fatalf("pdf missing: %v", err)
	}
	raw, _, err := readRawManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Papers[0]["download_status"] != "ok" {
		t.Fatalf("manifest status=%v", raw.Papers[0]["download_status"])
	}
	if raw.Papers[0]["pdf_path"] == "" {
		t.Fatal("manifest pdf_path empty")
	}

	// Catalog snapshot should now report has_local.
	catRR := httptest.NewRecorder()
	creq := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	creq.Header.Set("Authorization", "Bearer papers-secret")
	s.ServeHTTP(catRR, creq)
	if catRR.Code != http.StatusOK {
		t.Fatalf("catalog %d %s", catRR.Code, catRR.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(catRR.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if !cat.CanManage || cat.Count != 1 || !cat.Papers[0].HasLocal {
		t.Fatalf("catalog=%+v", cat)
	}
}

func TestRecoverPDFArxiv404(t *testing.T) {
	s, root := papersRecoverServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<html>not found</html>")
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":    "Withdrawn",
			"arxiv_id": "2609.00000",
			"pdf_url":  srv.URL + "/pdf/2609.00000.pdf",
		},
	})

	rr := authRecover(s, `{"id":"2609.00000"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != "fetch" {
		t.Fatalf("%+v", body)
	}
	if !strings.Contains(body.Error, "404") {
		t.Fatalf("error=%q", body.Error)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "pdfs"))
	if len(ents) != 0 {
		t.Fatalf("wrote files on 404: %v", ents)
	}
}

func TestRecoverPDFHTMLOnly(t *testing.T) {
	s, root := papersRecoverServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><html><body>abstract only</body></html>")
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":    "HTML only",
			"arxiv_id": "2609.11111",
			"pdf_url":  srv.URL + "/abs/2609.11111",
		},
	})

	rr := authRecover(s, `{"id":"2609.11111"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "HTML") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRecoverPDFAlreadyPresent(t *testing.T) {
	s, root := papersRecoverServer(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Error("should not re-download an existing PDF")
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	name := "2609.03747_already.pdf"
	if err := os.WriteFile(filepath.Join(root, "pdfs", name), []byte(tinyPDF), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":    "Already here",
			"arxiv_id": "2609.03747",
			"pdf_url":  srv.URL + "/should-not-hit",
			"pdf_path": "pdfs/" + name,
		},
	})

	rr := authRecover(s, `{"id":"2609.03747"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out recoverOneResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !out.AlreadyPresent || !out.Paper.HasLocal {
		t.Fatalf("%+v", out)
	}
	if hits.Load() != 0 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestRecoverPDFUnauthorized(t *testing.T) {
	s, root := papersRecoverServer(t)
	writeTestManifest(t, root, []map[string]interface{}{
		{"title": "X", "arxiv_id": "1.2"},
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/pdf/recover", strings.NewReader(`{"id":"1.2"}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeUnauthorized {
		t.Fatalf("%+v", body)
	}
}

func TestRecoverPDFNotInCatalog(t *testing.T) {
	s, root := papersRecoverServer(t)
	writeTestManifest(t, root, []map[string]interface{}{
		{"title": "Other", "arxiv_id": "1111.22222"},
	})
	rr := authRecover(s, `{"id":"2609.03747"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRecoverAllMissing(t *testing.T) {
	s, root := papersRecoverServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fail") || strings.Contains(r.URL.Path, "2609.10002") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, tinyPDF)
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":    "Ok one",
			"arxiv_id": "2609.10001",
			"pdf_url":  srv.URL + "/ok.pdf",
		},
		{
			"title":    "Fail one",
			"arxiv_id": "2609.10002",
			"pdf_url":  srv.URL + "/fail.pdf",
		},
		{
			"title":    "Already",
			"arxiv_id": "2609.10003",
			"pdf_url":  srv.URL + "/already.pdf",
			"pdf_path": "pdfs/2609.10003_already.pdf",
		},
	})
	if err := os.WriteFile(filepath.Join(root, "pdfs", "2609.10003_already.pdf"), []byte(tinyPDF), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := authRecover(s, `{"all_missing":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out recoverBatchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Recovered != 1 || out.Failed != 1 {
		t.Fatalf("%+v", out)
	}
	if len(out.Errors) != 1 || out.Errors[0].ID != "2609.10002" {
		t.Fatalf("errors=%+v", out.Errors)
	}
}

func TestRecoverPDFCorruptRedownload(t *testing.T) {
	s, root := papersRecoverServer(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, tinyPDF)
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	name := "2609.03747_corrupt.pdf"
	if err := os.WriteFile(filepath.Join(root, "pdfs", name), []byte("<html>nope</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, root, []map[string]interface{}{
		{
			"title":    "Corrupt file",
			"arxiv_id": "2609.03747",
			"pdf_url":  srv.URL + "/pdf/2609.03747.pdf",
			"pdf_path": "pdfs/" + name,
		},
	})

	rr := authRecover(s, `{"id":"2609.03747"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if hits.Load() == 0 {
		t.Fatal("expected re-download of corrupt file")
	}
	data, err := os.ReadFile(filepath.Join(root, "pdfs", name))
	if err != nil || !strings.HasPrefix(string(data), "%PDF") {
		t.Fatalf("file not replaced: %v %q", err, data)
	}
}

func TestRecoverPDFUpdatesSQLite(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	s, root := papersRecoverServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, tinyPDF)
	}))
	defer srv.Close()
	s.useTestPDFHost(t, srv)

	db := filepath.Join(root, "papers.db")
	script := "import sqlite3,sys\n" +
		"c=sqlite3.connect(sys.argv[1])\n" +
		"c.execute('CREATE TABLE papers (id TEXT PRIMARY KEY, pdf_path TEXT)')\n" +
		"c.execute(\"INSERT INTO papers VALUES ('2609.03747','')\")\n" +
		"c.commit()\n"
	cmd := exec.Command("python3", "-c", script, db)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create db: %v %s", err, out)
	}

	writeTestManifest(t, root, []map[string]interface{}{
		{"title": "DB paper", "arxiv_id": "2609.03747", "pdf_url": srv.URL + "/ok.pdf"},
	})
	rr := authRecover(s, `{"id":"2609.03747"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	check := "import sqlite3,sys\nprint(sqlite3.connect(sys.argv[1]).execute('SELECT pdf_path FROM papers WHERE id=?',(sys.argv[2],)).fetchone()[0])\n"
	out, err := exec.Command("python3", "-c", check, db, "2609.03747").CombinedOutput()
	if err != nil {
		t.Fatalf("read db: %v %s", err, out)
	}
	if !strings.Contains(string(out), "pdfs/") {
		t.Fatalf("db pdf_path=%q", out)
	}
}

func TestRecoverPDFPageHasButtonCopy(t *testing.T) {
	s, _ := papersRecoverServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	req.Header.Set("Authorization", "Bearer papers-secret")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Recover PDF", "恢复 PDF", "recover-pdf", "/papers/pdf/recover", "all_missing"} {
		if !strings.Contains(body, want) {
			t.Fatalf("papers.html missing %q", want)
		}
	}
}

func TestPDFCandidateURLs(t *testing.T) {
	got := pdfCandidateURLs(paperEntry{
		ArxivID: "2609.03747v2",
		PDFURL:  "https://arxiv.org/pdf/2609.03747v2.pdf",
	})
	if len(got) < 2 {
		t.Fatalf("%v", got)
	}
	if got[0] != "https://arxiv.org/pdf/2609.03747v2.pdf" {
		t.Fatalf("first=%s", got[0])
	}
	foundBare := false
	for _, u := range got {
		if u == "https://arxiv.org/pdf/2609.03747.pdf" {
			foundBare = true
		}
	}
	if !foundBare {
		t.Fatalf("missing bare url: %v", got)
	}
}

func TestCheapPDFPageCount(t *testing.T) {
	if n := cheapPDFPageCount([]byte(tinyPDF)); n != 1 {
		t.Fatalf("pages=%d", n)
	}
}
