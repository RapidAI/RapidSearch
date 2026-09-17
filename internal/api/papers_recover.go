package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"search-service/internal/search"
)

const (
	recoverOneTimeout   = 2 * time.Minute
	recoverBatchTimeout = 10 * time.Minute
	recoverMaxPDFBytes  = 80 << 20
	recoverConcurrency  = 2
	papersBotUA         = "Mozilla/5.0 (compatible; AgentPapersBot/1.0; research)"
)

var papersHTTPClient = &http.Client{
	Timeout: recoverOneTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

type recoverRequest struct {
	ID         string `json:"id"`
	ArxivID    string `json:"arxiv_id"`
	AllMissing bool   `json:"all_missing"`
}

type recoverOneResponse struct {
	OK             bool       `json:"ok"`
	Paper          paperEntry `json:"paper"`
	AlreadyPresent bool       `json:"already_present,omitempty"`
}

type recoverItemError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

type recoverBatchResponse struct {
	OK        bool               `json:"ok"`
	Recovered int                `json:"recovered"`
	Failed    int                `json:"failed"`
	Skipped   int                `json:"skipped"`
	Errors    []recoverItemError `json:"errors,omitempty"`
}

type recoverError struct {
	status int
	code   string
	msg    string
}

func (e *recoverError) Error() string { return e.msg }

type rawPapersManifest struct {
	GeneratedAt string                   `json:"generated_at"`
	Stats       map[string]interface{}   `json:"stats,omitempty"`
	Papers      []map[string]interface{} `json:"papers"`
}

func (s *Server) handlePapersPDFRecover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if !s.authorizeSettings(w, r) {
		return
	}
	defer r.Body.Close()
	var req recoverRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = strings.TrimSpace(req.ArxivID)
	}
	if id == "" && !req.AllMissing {
		writeErr(w, http.StatusBadRequest, "id is required (or set all_missing)", search.CodeBadRequest, nil, "")
		return
	}

	timeout := recoverOneTimeout
	if req.AllMissing && id == "" {
		timeout = recoverBatchTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if req.AllMissing && id == "" {
		writeJSON(w, http.StatusOK, s.papers().recoverAllMissing(ctx))
		return
	}

	paper, already, err := s.papers().recoverOne(ctx, id)
	if err != nil {
		writeRecoverErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recoverOneResponse{
		OK:             true,
		Paper:          paper,
		AlreadyPresent: already,
	})
}

func writeRecoverErr(w http.ResponseWriter, err error) {
	var re *recoverError
	if errors.As(err, &re) {
		writeErr(w, re.status, re.msg, re.code, nil, "")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeErr(w, http.StatusGatewayTimeout, "PDF download timed out", "fetch", nil, "")
		return
	}
	msg := err.Error()
	if msg == "" {
		msg = "PDF recover failed"
	}
	writeErr(w, http.StatusBadGateway, msg, "fetch", nil, "")
}

func (ps *papersStore) client() *http.Client {
	if ps != nil && ps.httpClient != nil {
		return ps.httpClient
	}
	return papersHTTPClient
}

func (ps *papersStore) recoverOne(ctx context.Context, id string) (paperEntry, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return paperEntry{}, false, &recoverError{http.StatusBadRequest, search.CodeBadRequest, "id is required"}
	}

	entry, destName, destPath, already, err := ps.planRecover(id)
	if err != nil {
		return paperEntry{}, false, err
	}

	if already {
		pages := cheapPDFPageCountFromFile(destPath)
		if err := ps.commitRecover(id, destName, pages); err != nil {
			return paperEntry{}, false, err
		}
		out, err := ps.enrichedByID(id)
		return out, true, err
	}

	urls := pdfCandidateURLs(entry)
	if len(urls) == 0 {
		return paperEntry{}, false, &recoverError{
			http.StatusBadRequest, search.CodeBadRequest,
			"no arXiv id or PDF URL to recover from",
		}
	}

	var (
		data []byte
		last error
	)
	for _, u := range urls {
		if err := ctx.Err(); err != nil {
			return paperEntry{}, false, err
		}
		data, last = fetchPDFBytes(ctx, ps.client(), u, false)
		if last == nil {
			break
		}
	}
	if last != nil {
		return paperEntry{}, false, last
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return paperEntry{}, false, fmt.Errorf("create pdfs dir: %w", err)
	}
	tmp := destPath + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return paperEntry{}, false, fmt.Errorf("write PDF: %w", err)
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return paperEntry{}, false, fmt.Errorf("write PDF: %w", err)
	}

	pages := cheapPDFPageCount(data)
	if err := ps.commitRecover(id, destName, pages); err != nil {
		return paperEntry{}, false, err
	}
	out, err := ps.enrichedByID(id)
	return out, false, err
}

func (ps *papersStore) recoverAllMissing(ctx context.Context) recoverBatchResponse {
	cat, err := ps.catalog()
	if err != nil {
		return recoverBatchResponse{
			OK:     false,
			Failed: 1,
			Errors: []recoverItemError{{ID: "", Error: "papers catalog unavailable"}},
		}
	}
	type job struct {
		id string
	}
	var jobs []job
	for _, p := range cat.Papers {
		if p.HasLocal {
			continue
		}
		id := sanitizeArxivID(p.ArxivID)
		if id == "" && strings.TrimSpace(p.PDFURL) == "" {
			continue
		}
		if id == "" {
			id = strings.TrimSpace(p.Filename)
		}
		if id == "" {
			continue
		}
		jobs = append(jobs, job{id: id})
	}
	out := recoverBatchResponse{OK: true}
	if len(jobs) == 0 {
		return out
	}

	type result struct {
		id      string
		already bool
		err     error
	}
	sem := make(chan struct{}, recoverConcurrency)
	ch := make(chan result, len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				ch <- result{id: j.id, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			_, already, err := ps.recoverOne(ctx, j.id)
			ch <- result{id: j.id, already: already, err: err}
		}()
	}
	wg.Wait()
	close(ch)
	for r := range ch {
		if r.err != nil {
			out.Failed++
			out.Errors = append(out.Errors, recoverItemError{ID: r.id, Error: r.err.Error()})
			continue
		}
		if r.already {
			out.Skipped++
			continue
		}
		out.Recovered++
	}
	return out
}

func (ps *papersStore) planRecover(id string) (paperEntry, string, string, bool, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	root := ps.ensureRootLocked()
	raw, _, err := readRawManifest(root)
	if err != nil {
		return paperEntry{}, "", "", false, &recoverError{http.StatusBadGateway, "papers", "papers catalog unavailable"}
	}
	idx := findRawPaperIndex(raw.Papers, id)
	if idx < 0 {
		return paperEntry{}, "", "", false, &recoverError{http.StatusNotFound, "papers", "paper not found in catalog"}
	}
	entry := paperFromRaw(raw.Papers[idx])
	pdfDir := filepath.Join(root, "pdfs")
	name := recoverFilename(entry)
	if hit := findPDFByArxiv(pdfDir, firstNonEmpty(entry.ArxivID, id)); hit != "" {
		name = hit
	}
	dest := filepath.Join(pdfDir, name)
	already := validLocalPDF(dest)
	return entry, name, dest, already, nil
}

func (ps *papersStore) commitRecover(id, filename string, pageCount int) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	root := ps.ensureRootLocked()
	raw, manPath, err := readRawManifest(root)
	if err != nil {
		return &recoverError{http.StatusBadGateway, "papers", "papers catalog unavailable"}
	}
	idx := findRawPaperIndex(raw.Papers, id)
	if idx < 0 {
		return &recoverError{http.StatusNotFound, "papers", "paper not found in catalog"}
	}
	rel := filepath.ToSlash(filepath.Join("pdfs", filename))
	raw.Papers[idx]["pdf_path"] = rel
	raw.Papers[idx]["download_status"] = "ok"
	if pageCount > 0 {
		raw.Papers[idx]["page_count"] = pageCount
	}
	if err := writeRawManifestAtomic(manPath, raw); err != nil {
		return fmt.Errorf("update manifest: %w", err)
	}
	ps.loaded = time.Time{}
	ps.modTime = time.Time{}
	updatePapersDB(root, catalogPaperID(raw.Papers[idx], id), rel)
	return nil
}

func (ps *papersStore) enrichedByID(id string) (paperEntry, error) {
	cat, err := ps.catalog()
	if err != nil {
		return paperEntry{}, &recoverError{http.StatusBadGateway, "papers", "papers catalog unavailable"}
	}
	for _, p := range cat.Papers {
		if paperMatchesID(p, id) {
			return p, nil
		}
	}
	return paperEntry{}, &recoverError{http.StatusNotFound, "papers", "paper not found in catalog"}
}

func (ps *papersStore) ensureRootLocked() string {
	if ps.root == "" {
		ps.root = papersRoot()
	}
	return ps.root
}

func readRawManifest(root string) (rawPapersManifest, string, error) {
	manPath := filepath.Join(root, "manifest.json")
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return rawPapersManifest{}, manPath, err
	}
	var man rawPapersManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return rawPapersManifest{}, manPath, err
	}
	return man, manPath, nil
}

func writeRawManifestAtomic(path string, man rawPapersManifest) error {
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func findRawPaperIndex(papers []map[string]interface{}, id string) int {
	want := strings.TrimSpace(id)
	wantID := sanitizeArxivID(want)
	wantBare := stripArxivVersion(wantID)
	for i, m := range papers {
		if paperRawMatches(m, want, wantID, wantBare) {
			return i
		}
	}
	return -1
}

func paperRawMatches(m map[string]interface{}, want, wantID, wantBare string) bool {
	if aid := sanitizeArxivID(rawString(m, "arxiv_id")); aid != "" {
		if wantID != "" && (aid == wantID || stripArxivVersion(aid) == wantBare) {
			return true
		}
	}
	pdfPath := rawString(m, "pdf_path")
	base := filepath.Base(pdfPath)
	if base != "" && base != "." && (base == want || strings.TrimSuffix(base, ".pdf") == want) {
		return true
	}
	return false
}

func paperMatchesID(p paperEntry, id string) bool {
	wantID := sanitizeArxivID(id)
	got := sanitizeArxivID(p.ArxivID)
	if wantID != "" && got != "" && (got == wantID || stripArxivVersion(got) == stripArxivVersion(wantID)) {
		return true
	}
	if p.Filename != "" && (p.Filename == id || strings.TrimSuffix(p.Filename, ".pdf") == id) {
		return true
	}
	return false
}

func paperFromRaw(m map[string]interface{}) paperEntry {
	p := paperEntry{
		Title:          rawString(m, "title"),
		Abstract:       rawString(m, "abstract"),
		SourceURL:      rawString(m, "source_url"),
		PDFURL:         rawString(m, "pdf_url"),
		ArxivID:        sanitizeArxivID(rawString(m, "arxiv_id")),
		DOI:            rawString(m, "doi"),
		PDFPath:        rawString(m, "pdf_path"),
		DownloadStatus: rawString(m, "download_status"),
		Published:      rawString(m, "published"),
		Updated:        rawString(m, "updated"),
	}
	if v, ok := m["year"].(float64); ok {
		p.Year = int(v)
	}
	if v, ok := m["page_count"].(float64); ok {
		p.PageCount = int(v)
	}
	p.Authors = rawStringSlice(m, "authors")
	p.TopicTags = rawStringSlice(m, "topic_tags")
	return p
}

func rawString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func rawStringSlice(m map[string]interface{}, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, _ := x.(string)
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func catalogPaperID(m map[string]interface{}, fallback string) string {
	if id := sanitizeArxivID(rawString(m, "arxiv_id")); id != "" {
		return stripArxivVersion(id)
	}
	if fallback != "" {
		return sanitizeArxivID(fallback)
	}
	return ""
}

func stripArxivVersion(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	i := len(id) - 1
	for i >= 0 && id[i] >= '0' && id[i] <= '9' {
		i--
	}
	if i > 0 && (id[i] == 'v' || id[i] == 'V') {
		return id[:i]
	}
	return id
}

func pdfCandidateURLs(p paperEntry) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(p.PDFURL)
	id := stripArxivVersion(sanitizeArxivID(p.ArxivID))
	if id != "" {
		add("https://arxiv.org/pdf/" + id + ".pdf")
		add("https://arxiv.org/pdf/" + id)
	}
	return out
}

func recoverFilename(p paperEntry) string {
	if name := filepath.Base(strings.TrimSpace(p.PDFPath)); looksLikePDFName(name) {
		return name
	}
	if looksLikePDFName(p.Filename) {
		return p.Filename
	}
	return papersSafeFilename(p)
}

func looksLikePDFName(name string) bool {
	if name == "" || name == "." || name == string(filepath.Separator) {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(name), ".pdf")
}

func papersSafeFilename(p paperEntry) string {
	base := stripArxivVersion(sanitizeArxivID(p.ArxivID))
	if base == "" && p.DOI != "" {
		base = strings.ReplaceAll(p.DOI, "/", "_")
	}
	if base == "" {
		base = "paper"
	}
	base = sanitizeFilenamePart(base, 80)
	slug := sanitizeFilenamePart(strings.ToLower(p.Title), 40)
	slug = strings.Trim(slug, "_")
	if slug != "" {
		return base + "_" + slug + ".pdf"
	}
	return base + ".pdf"
}

func sanitizeFilenamePart(s string, max int) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := b.String()
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

func validLocalPDF(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() < 5 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [5]byte
	n, _ := f.Read(magic[:])
	return n >= 4 && string(magic[:4]) == "%PDF"
}

func fetchPDFBytes(ctx context.Context, client *http.Client, rawURL string, retried bool) ([]byte, error) {
	if client == nil {
		client = papersHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &recoverError{http.StatusBadRequest, search.CodeBadRequest, "invalid PDF URL"}
	}
	req.Header.Set("User-Agent", papersBotUA)
	req.Header.Set("Accept", "application/pdf,*/*;q=0.8")
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, &recoverError{http.StatusBadGateway, "fetch", "network error downloading PDF: " + err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, &recoverError{
			http.StatusBadGateway, "fetch",
			"arXiv has no PDF for this paper (HTTP 404)",
		}
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, &recoverError{
			http.StatusBadGateway, "fetch",
			fmt.Sprintf("PDF download failed (HTTP %d)", res.StatusCode),
		}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, recoverMaxPDFBytes+1))
	if err != nil {
		return nil, &recoverError{http.StatusBadGateway, "fetch", "network error reading PDF: " + err.Error()}
	}
	if int64(len(data)) > recoverMaxPDFBytes {
		return nil, &recoverError{http.StatusBadGateway, "fetch", "PDF exceeds size cap"}
	}
	if bytes.HasPrefix(data, []byte("%PDF")) {
		return data, nil
	}
	if !retried && strings.Contains(strings.ToLower(rawURL), "arxiv.org") {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		return fetchPDFBytes(ctx, client, rawURL+sep+"download=1", true)
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	head := data
	if len(head) > 256 {
		head = head[:256]
	}
	if strings.Contains(ct, "html") || bytes.Contains(bytes.ToLower(head), []byte("<html")) || bytes.Contains(bytes.ToLower(head), []byte("<!doctype")) {
		return nil, &recoverError{
			http.StatusBadGateway, "fetch",
			"response is HTML, not a PDF (withdrawn or HTML-only)",
		}
	}
	return nil, &recoverError{http.StatusBadGateway, "fetch", "response is not a PDF"}
}

func cheapPDFPageCount(data []byte) int {
	n := 0
	for i := 0; i < len(data); {
		j := bytes.Index(data[i:], []byte("/Type"))
		if j < 0 {
			break
		}
		i += j + 5
		for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
			i++
		}
		if i+5 <= len(data) && string(data[i:i+5]) == "/Page" {
			if i+6 <= len(data) && data[i+5] == 's' {
				continue
			}
			n++
		}
	}
	return n
}

func cheapPDFPageCountFromFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return cheapPDFPageCount(data)
}

// updatePapersDB best-effort patches papers.db via python3 sqlite3 (same schema as agent-papers/db.py).
func updatePapersDB(root, id, pdfPath string) {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(pdfPath) == "" {
		return
	}
	db := filepath.Join(root, "papers.db")
	if st, err := os.Stat(db); err != nil || st.IsDir() {
		return
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	script := "import sqlite3,sys\n" +
		"c=sqlite3.connect(sys.argv[1])\n" +
		"cur=c.execute('UPDATE papers SET pdf_path=? WHERE id=?',(sys.argv[3],sys.argv[2]))\n" +
		"if cur.rowcount==0:\n" +
		" c.execute('UPDATE papers SET pdf_path=? WHERE id=?',(sys.argv[3],'arxiv:'+sys.argv[2]))\n" +
		"c.commit()\n"
	cmd := exec.CommandContext(ctx, py, "-c", script, db, id, pdfPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("papers recover: db update id=%s: %v %s", id, err, bytes.TrimSpace(out))
	}
}
