package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"search-service/internal/search"
)

const (
	paperSourceManual    = "manual"
	importJSONMaxBytes   = 8 << 10
	importPDFMaxBytes    = 50 << 20
	importPerIP          = 8
	importWindow         = 10 * time.Minute
	importMetaTimeout    = 20 * time.Second
	importPDFTimeout     = 60 * time.Second
	importHandlerTimeout = 90 * time.Second
	importUserAgent      = "Mozilla/5.0 (compatible; AgentPapersBot/1.0; research)"
	importQueryHit       = "manual-import"
	importUploadQueryHit = "manual-upload"
	importManualScore    = 5.0
)

// Stable topic_tags keys accepted by POST /papers/import and the catalog UI.
var allowedTopicTags = map[string]struct{}{
	"self-evolution":     {},
	"security":           {},
	"both":               {},
	"llm-iot":            {},
	"survey":             {},
	"llm-training":       {},
	"agent-tools-memory": {},
	"other":              {},
}

var (
	// New-style YYMM.NNNNN plus optional version; also old-style archive/YYMMNNN.
	arxivBareRe   = regexp.MustCompile(`(?i)^(?:arxiv:)?(\d{4}\.\d{4,5}|[a-z\-]+(?:\.[a-z]{2})?/\d{7})(?:v\d+)?$`)
	arxivURLRe    = regexp.MustCompile(`(?i)(?:https?://)?(?:[\w.-]+\.)?arxiv\.org/(?:abs|pdf|html|src|ps)/(?:arxiv:)?(\d{4}\.\d{4,5}|[a-z\-]+(?:\.[a-z]{2})?/\d{7})(?:v\d+)?(?:\.pdf)?`)
	arxivIDFindRe = regexp.MustCompile(`(?i)(?:arxiv\.org/(?:abs|pdf|html|src)/|arxiv:)?(\d{4}\.\d{4,5})(?:v\d+)?`)
)

type importReq struct {
	URLOrID string `json:"url_or_id"`
	Tag     string `json:"tag"`
}

type importResp struct {
	OK        bool       `json:"ok"`
	Paper     paperEntry `json:"paper,omitempty"`
	Updated   bool       `json:"updated,omitempty"`
	Error     string     `json:"error,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
}

type importTarget struct {
	ArxivID string
	PDFURL  string
	Kind    string // arxiv | pdf
}

type importLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newImportLimiter() *importLimiter {
	return &importLimiter{hits: make(map[string][]time.Time)}
}

func (l *importLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = make(map[string][]time.Time)
	}
	cut := now.Add(-importWindow)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= importPerIP {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

func validateTopicTag(tag string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(tag))
	if t == "" {
		return "", false
	}
	_, ok := allowedTopicTags[t]
	if !ok {
		return "", false
	}
	return t, true
}

// parseArxivID extracts a canonical arXiv id (no version) from a bare id or URL.
func parseArxivID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "arXiv:")
	s = strings.TrimPrefix(s, "arxiv:")
	s = strings.TrimSpace(s)
	if m := arxivURLRe.FindStringSubmatch(s); len(m) > 1 {
		return canonicalizeArxivID(m[1])
	}
	// Bare id, optionally with version or surrounding whitespace/punctuation.
	if m := arxivBareRe.FindStringSubmatch(s); len(m) > 1 {
		return canonicalizeArxivID(m[1])
	}
	// Last-resort: new-style id embedded in free text / query strings.
	if m := arxivIDFindRe.FindStringSubmatch(s); len(m) > 1 {
		// Avoid treating a random https URL as arxiv unless the host is arxiv.
		if strings.Contains(strings.ToLower(s), "arxiv") || arxivBareRe.MatchString(s) {
			return canonicalizeArxivID(m[1])
		}
	}
	return ""
}

func canonicalizeArxivID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "arXiv:")
	id = strings.TrimPrefix(id, "arxiv:")
	if i := strings.IndexByte(strings.ToLower(id), 'v'); i > 0 {
		// Strip trailing version on new-style ids (2504.01990v2).
		if regexp.MustCompile(`(?i)^v\d+$`).MatchString(id[i:]) {
			id = id[:i]
		}
	}
	return sanitizeArxivID(id)
}

func parseImportTarget(urlOrID string) (importTarget, error) {
	raw := strings.TrimSpace(urlOrID)
	if raw == "" {
		return importTarget{}, errors.New("url_or_id is required")
	}
	if len(raw) > 2000 {
		return importTarget{}, errors.New("url_or_id is too long")
	}
	if id := parseArxivID(raw); id != "" {
		return importTarget{
			ArxivID: id,
			PDFURL:  "https://arxiv.org/pdf/" + id + ".pdf",
			Kind:    "arxiv",
		}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return importTarget{}, errors.New("expected an arXiv id, arXiv URL, or http(s) PDF URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return importTarget{}, errors.New("only http(s) PDF URLs are allowed")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "arxiv.org") {
		if id := parseArxivID(raw); id != "" {
			return importTarget{
				ArxivID: id,
				PDFURL:  "https://arxiv.org/pdf/" + id + ".pdf",
				Kind:    "arxiv",
			}, nil
		}
		// arXiv URL we could not parse as an id — still fetch as PDF if it looks like one.
	}
	return importTarget{PDFURL: u.String(), Kind: "pdf"}, nil
}

func (s *Server) handlePapersImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	ps := s.papers()
	if ps == nil {
		writeErr(w, http.StatusBadGateway, "papers store unavailable", "papers", nil, "")
		return
	}
	ip := requestIP(r)
	if !ps.importLimit.allow(ip, time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "too many imports from this address — try again later", "papers", nil, "")
		return
	}

	if isMultipartImport(r) {
		s.handlePapersImportMultipart(w, r, ps)
		return
	}
	s.handlePapersImportJSON(w, r, ps)
}

func isMultipartImport(r *http.Request) bool {
	if r == nil {
		return false
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "multipart/form-data")
}

func (s *Server) handlePapersImportJSON(w http.ResponseWriter, r *http.Request, ps *papersStore) {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, importJSONMaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body", search.CodeBadRequest, nil, "")
		return
	}
	if len(raw) > importJSONMaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "request too large", search.CodeBadRequest, nil, "")
		return
	}
	var req importReq
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
		return
	}
	// Tag is mandatory on every import path (URL/id and upload). Reject before fetch.
	tag, err := requireImportTag(req.Tag)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errImportTag.Error(), search.CodeBadRequest, nil, "")
		return
	}
	target, err := parseImportTarget(req.URLOrID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
		return
	}
	s.finishPapersImport(w, r, ps, func(ctx context.Context) (paperEntry, bool, error) {
		return ps.importPaper(ctx, target, tag)
	})
}

func (s *Server) handlePapersImportMultipart(w http.ResponseWriter, r *http.Request, ps *papersStore) {
	r.Body = http.MaxBytesReader(w, r.Body, importUploadMaxBytes+2<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isTooLargeErr(err) {
			writeImportErr(w, http.StatusRequestEntityTooLarge, importErrTooLarge, "pdf exceeds size limit")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid multipart body", search.CodeBadRequest, nil, "")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	// Same rule as JSON/URL import: no ingest without a known category key.
	tag, err := requireImportTag(firstNonBlank(r.FormValue("tag"), r.FormValue("category")))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errImportTag.Error(), search.CodeBadRequest, nil, "")
		return
	}

	file, hdr, ferr := r.FormFile("file")
	if ferr != nil {
		file, hdr, ferr = r.FormFile("pdf")
	}
	if ferr == nil && file != nil {
		defer file.Close()
		body, rerr := io.ReadAll(io.LimitReader(file, importUploadMaxBytes+1))
		if rerr != nil {
			writeErr(w, http.StatusBadRequest, "could not read uploaded file", search.CodeBadRequest, nil, "")
			return
		}
		if len(body) > importUploadMaxBytes {
			writeImportErr(w, http.StatusRequestEntityTooLarge, importErrTooLarge, "pdf exceeds size limit")
			return
		}
		name := ""
		if hdr != nil {
			name = hdr.Filename
		}
		s.finishPapersImport(w, r, ps, func(ctx context.Context) (paperEntry, bool, error) {
			return ps.importUploadedPaper(ctx, tag, name, body)
		})
		return
	}

	urlOrID := firstNonBlank(r.FormValue("url_or_id"), r.FormValue("url"))
	target, err := parseImportTarget(urlOrID)
	if err != nil {
		writeImportErr(w, http.StatusBadRequest, importErrNeedInput, "provide an arXiv id/URL, a PDF URL, or a PDF file")
		return
	}
	s.finishPapersImport(w, r, ps, func(ctx context.Context) (paperEntry, bool, error) {
		return ps.importPaper(ctx, target, tag)
	})
}

func (s *Server) finishPapersImport(w http.ResponseWriter, r *http.Request, ps *papersStore, run func(context.Context) (paperEntry, bool, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), importHandlerTimeout)
	defer cancel()

	paper, updated, err := run(ctx)
	if err != nil {
		log.Printf("papers import: %v", err)
		status, code, msg := importErrDetail(err)
		if code != "" && code != "papers" {
			writeImportErr(w, status, code, msg)
			return
		}
		writeErr(w, status, msg, "papers", nil, "")
		return
	}

	// Same downstream as auto-synced papers: catalog reload, ZH abstracts, optional BabelDOC.
	ps.invalidateCatalog()
	if cat, cerr := ps.catalog(); cerr == nil {
		if abs := ps.absZH; abs != nil {
			abs.ensureMissing(cat.Papers)
		}
		if s.translate() != nil && s.translate().AutoTranslate() {
			s.maybeAutoTranslate(cat.Papers)
		}
	}

	enriched := enrichPaper(paper, filepath.Join(ps.root, "pdfs"))
	writeJSON(w, http.StatusOK, importResp{OK: true, Paper: enriched, Updated: updated})
}

func writeImportErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, importResp{OK: false, Error: msg, ErrorCode: code})
}

func isTooLargeErr(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "too large") || strings.Contains(msg, "request body too large")
}

func importErrStatus(err error) (int, string) {
	status, _, msg := importErrDetail(err)
	return status, msg
}

func importErrDetail(err error) (int, string, string) {
	if err == nil {
		return http.StatusOK, "", ""
	}
	var check *importCheckError
	if errors.As(err, &check) && check != nil && check.Code != "" {
		status := http.StatusBadRequest
		if check.Code == importErrTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		return status, check.Code, check.Error()
	}
	msg := err.Error()
	switch {
	case errors.Is(err, errImportTag):
		return http.StatusBadRequest, importErrNeedTag, errImportTag.Error()
	case errors.Is(err, errImportNeedSrc):
		return http.StatusBadRequest, importErrNeedInput, "provide an arXiv id/URL, a PDF URL, or a PDF file"
	case errors.Is(err, errBlockedFetch):
		return http.StatusBadRequest, "", "that URL is not allowed"
	case errors.Is(err, errNotPDF):
		return http.StatusBadRequest, importErrNotPDF, "downloaded file is not a PDF"
	case errors.Is(err, errPDFTooLarge):
		return http.StatusRequestEntityTooLarge, importErrTooLarge, "pdf exceeds size limit"
	case errors.Is(err, errPDFTooSmall):
		return http.StatusBadRequest, importErrTooSmall, "pdf is too small"
	case errors.Is(err, errNotAPaper):
		return http.StatusBadRequest, importErrNotPaper, "pdf does not look like a traditional academic paper"
	case errors.Is(err, errScannedPDF):
		return http.StatusBadRequest, importErrScanned, "pdf looks like a scanned image with no extractable paper text"
	case errors.Is(err, errImportEmpty):
		return http.StatusBadRequest, "", "could not resolve a paper from that URL"
	case strings.Contains(msg, "timeout") || errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "", "timed out fetching the paper"
	default:
		return http.StatusBadGateway, "", "could not import paper: " + msg
	}
}

var (
	errBlockedFetch = errors.New("blocked fetch")
	errNotPDF       = errors.New("not a pdf")
	errImportEmpty  = errors.New("empty paper")
)

func (ps *papersStore) importPaper(ctx context.Context, target importTarget, tag string) (paperEntry, bool, error) {
	if ps == nil {
		return paperEntry{}, false, errors.New("papers store unavailable")
	}
	tag, err := requireImportTag(tag)
	if err != nil {
		return paperEntry{}, false, err
	}
	ps.importMu.Lock()
	defer ps.importMu.Unlock()

	p := paperEntry{
		TopicTags:      []string{tag},
		Score:          importManualScore,
		Source:         paperSourceManual,
		QueryHits:      []string{importQueryHit},
		DownloadStatus: "ok",
	}
	if target.ArxivID != "" {
		p.ArxivID = target.ArxivID
		p.SourceURL = "https://arxiv.org/abs/" + target.ArxivID
		p.PDFURL = target.PDFURL
		if meta, err := fetchArxivMeta(ctx, ps.httpClient(), ps.arxivQueryURL(target.ArxivID)); err != nil {
			log.Printf("papers import: arxiv meta %s: %v", target.ArxivID, err)
		} else {
			mergeArxivMeta(&p, meta)
		}
		if p.Title == "" {
			p.Title = "arXiv:" + target.ArxivID
		}
	} else {
		p.PDFURL = target.PDFURL
		p.SourceURL = target.PDFURL
		p.Title = titleFromPDFURL(target.PDFURL)
	}
	if strings.TrimSpace(p.Title) == "" {
		return paperEntry{}, false, errImportEmpty
	}

	pdfURL := strings.TrimSpace(p.PDFURL)
	if pdfURL == "" {
		pdfURL = target.PDFURL
	}
	body, err := fetchPDFBytes(ctx, ps.httpClient(), pdfURL, ps.allowPrivateFetch)
	if err != nil {
		return paperEntry{}, false, err
	}

	return ps.saveImportedPDF(p, body)
}

// importUploadedPaper stores a locally uploaded PDF after the academic-paper structure gate.
// tag must already be a known category key — missing/unknown tags never ingest.
func (ps *papersStore) importUploadedPaper(ctx context.Context, tag, uploadName string, body []byte) (paperEntry, bool, error) {
	if ps == nil {
		return paperEntry{}, false, errors.New("papers store unavailable")
	}
	_ = ctx
	tag, err := requireImportTag(tag)
	if err != nil {
		return paperEntry{}, false, err
	}
	if len(body) > importUploadMaxBytes {
		return paperEntry{}, false, importCheckErr(importErrTooLarge, "pdf exceeds size limit")
	}
	meta, err := inspectAcademicPaperPDF(body)
	if err != nil {
		return paperEntry{}, false, err
	}
	if title := titleFromUploadName(uploadName); title != "" && (meta.Title == "" || meta.Title == "Imported paper") {
		meta.Title = title
	}
	sum := sha1.Sum(body)
	hexid := hex.EncodeToString(sum[:])
	p := paperEntry{
		Title:          firstNonBlank(firstNonBlank(meta.Title, titleFromUploadName(uploadName)), "Imported paper"),
		Authors:        meta.Authors,
		Abstract:       meta.Abstract,
		Year:           meta.Year,
		PageCount:      meta.Pages,
		TopicTags:      []string{tag},
		Score:          importManualScore,
		Source:         paperSourceManual,
		QueryHits:      []string{importQueryHit, importUploadQueryHit},
		DownloadStatus: "ok",
		SourceURL:      "manual-upload:" + hexid[:16],
	}
	ps.importMu.Lock()
	defer ps.importMu.Unlock()
	return ps.saveImportedPDF(p, body)
}

func (ps *papersStore) saveImportedPDF(p paperEntry, body []byte) (paperEntry, bool, error) {
	root := ps.root
	if root == "" {
		root = papersRoot()
		ps.root = root
	}
	pdfDir := filepath.Join(root, "pdfs")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		return paperEntry{}, false, err
	}
	name := paperPDFFilename(p)
	dest := filepath.Join(pdfDir, name)
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return paperEntry{}, false, err
	}
	p.PDFPath = filepath.ToSlash(filepath.Join("pdfs", name))
	p.Filename = name
	if n := pdfPageCountBytes(body); n > 0 {
		p.PageCount = n
	} else if n := pdfPageCountFile(dest); n > 0 {
		p.PageCount = n
	}

	updated, err := ps.writeManifestPaper(p)
	if err != nil {
		return paperEntry{}, false, err
	}
	upsertImportedSQLite(root, p)
	return p, updated, nil
}

func mergeArxivMeta(p *paperEntry, meta paperEntry) {
	if meta.Title != "" {
		p.Title = meta.Title
	}
	if len(meta.Authors) > 0 {
		p.Authors = meta.Authors
	}
	if meta.Abstract != "" {
		p.Abstract = meta.Abstract
	}
	if meta.Year > 0 {
		p.Year = meta.Year
	}
	if meta.DOI != "" {
		p.DOI = meta.DOI
	}
	if meta.Published != "" {
		p.Published = meta.Published
	}
	if meta.Updated != "" {
		p.Updated = meta.Updated
	}
	if meta.SourceURL != "" {
		p.SourceURL = meta.SourceURL
	}
	if meta.PDFURL != "" {
		p.PDFURL = meta.PDFURL
	}
}

func (ps *papersStore) httpClient() *http.Client {
	if ps != nil && ps.importClient != nil {
		return ps.importClient
	}
	if ps != nil && ps.allowPrivateFetch {
		return &http.Client{Timeout: importPDFTimeout}
	}
	return defaultImportHTTPClient()
}

func (ps *papersStore) writeManifestPaper(p paperEntry) (updated bool, err error) {
	root := ps.root
	if root == "" {
		root = papersRoot()
	}
	manPath := filepath.Join(root, "manifest.json")
	ps.mu.Lock()
	defer ps.mu.Unlock()

	var man papersManifest
	if raw, rerr := os.ReadFile(manPath); rerr == nil && len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &man); uerr != nil {
			return false, uerr
		}
	}
	replaced := false
	for i, existing := range man.Papers {
		if sameImportedPaper(existing, p) {
			man.Papers[i] = mergeImportedPaper(existing, p)
			p = man.Papers[i]
			replaced = true
			break
		}
	}
	if !replaced {
		man.Papers = append(man.Papers, p)
	}
	man.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if man.Stats == nil {
		man.Stats = map[string]interface{}{}
	}
	man.Stats["kept"] = len(man.Papers)
	man.Stats["theme_counts"] = themeCounts(man.Papers)
	man.Stats["last_import_at"] = man.GeneratedAt

	tmp, err := os.CreateTemp(root, "manifest.*.json")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(man); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false, err
	}
	if err := os.Rename(tmpName, manPath); err != nil {
		os.Remove(tmpName)
		return false, err
	}
	ps.loaded = time.Time{}
	ps.modTime = time.Time{}
	return replaced, nil
}

func sameImportedPaper(a, b paperEntry) bool {
	if aid := sanitizeArxivID(a.ArxivID); aid != "" && aid == sanitizeArxivID(b.ArxivID) {
		return true
	}
	if a.DOI != "" && strings.EqualFold(a.DOI, b.DOI) {
		return true
	}
	if a.SourceURL != "" && b.SourceURL != "" && strings.EqualFold(strings.TrimRight(a.SourceURL, "/"), strings.TrimRight(b.SourceURL, "/")) {
		return true
	}
	if a.PDFURL != "" && b.PDFURL != "" && strings.EqualFold(strings.TrimRight(a.PDFURL, "/"), strings.TrimRight(b.PDFURL, "/")) {
		return true
	}
	na := normTitleKey(a.Title)
	nb := normTitleKey(b.Title)
	return na != "" && na == nb
}

func mergeImportedPaper(old, neu paperEntry) paperEntry {
	out := old
	out.Title = firstNonBlank(neu.Title, old.Title)
	if len(neu.Authors) > 0 {
		out.Authors = neu.Authors
	}
	if neu.Abstract != "" {
		out.Abstract = neu.Abstract
	}
	if neu.Year > 0 {
		out.Year = neu.Year
	}
	if neu.SourceURL != "" {
		out.SourceURL = neu.SourceURL
	}
	if neu.PDFURL != "" {
		out.PDFURL = neu.PDFURL
	}
	if neu.ArxivID != "" {
		out.ArxivID = neu.ArxivID
	}
	if neu.DOI != "" {
		out.DOI = neu.DOI
	}
	if len(neu.TopicTags) > 0 {
		out.TopicTags = neu.TopicTags
	}
	if neu.Score > out.Score {
		out.Score = neu.Score
	}
	if neu.PDFPath != "" {
		out.PDFPath = neu.PDFPath
	}
	if neu.DownloadStatus != "" {
		out.DownloadStatus = neu.DownloadStatus
	}
	if neu.Published != "" {
		out.Published = neu.Published
	}
	if neu.Updated != "" {
		out.Updated = neu.Updated
	}
	if neu.PageCount > 0 {
		out.PageCount = neu.PageCount
	}
	out.Source = paperSourceManual
	if !containsString(out.QueryHits, importQueryHit) {
		out.QueryHits = append(out.QueryHits, importQueryHit)
	}
	return out
}

func themeCounts(papers []paperEntry) map[string]int {
	out := map[string]int{}
	for _, p := range papers {
		for _, t := range p.TopicTags {
			if t != "" {
				out[t]++
			}
		}
	}
	return out
}

func firstNonBlank(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func normTitleKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func paperPDFFilename(p paperEntry) string {
	base := sanitizeArxivID(p.ArxivID)
	base = strings.ReplaceAll(base, "/", "_")
	if base == "" && p.DOI != "" {
		base = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' {
				return r
			}
			if r == '/' {
				return '_'
			}
			return -1
		}, p.DOI)
	}
	if base == "" {
		sum := sha1.Sum([]byte(normTitleKey(p.Title) + "|" + p.PDFURL))
		base = hex.EncodeToString(sum[:])[:16]
	}
	if len(base) > 80 {
		base = base[:80]
	}
	slug := slugTitle(p.Title, 40)
	if slug != "" {
		return base + "_" + slug + ".pdf"
	}
	return base + ".pdf"
}

func slugTitle(title string, max int) string {
	var b strings.Builder
	lastUnderscore := true
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	s := strings.Trim(b.String(), "_")
	if max > 0 && len(s) > max {
		s = strings.Trim(s[:max], "_")
	}
	return s
}

func titleFromPDFURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "Imported paper"
	}
	base := filepath.Base(u.Path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.Join(strings.Fields(base), " ")
	if base == "" || strings.EqualFold(base, "pdf") {
		return "Imported paper"
	}
	return base
}

type atomFeed struct {
	Entries []atomEntry `xml:"http://www.w3.org/2005/Atom entry"`
}

type atomEntry struct {
	ID        string `xml:"http://www.w3.org/2005/Atom id"`
	Title     string `xml:"http://www.w3.org/2005/Atom title"`
	Summary   string `xml:"http://www.w3.org/2005/Atom summary"`
	Published string `xml:"http://www.w3.org/2005/Atom published"`
	Updated   string `xml:"http://www.w3.org/2005/Atom updated"`
	Authors   []struct {
		Name string `xml:"http://www.w3.org/2005/Atom name"`
	} `xml:"http://www.w3.org/2005/Atom author"`
	Links []struct {
		Rel   string `xml:"rel,attr"`
		Href  string `xml:"href,attr"`
		Type  string `xml:"type,attr"`
		Title string `xml:"title,attr"`
	} `xml:"http://www.w3.org/2005/Atom link"`
	DOI string `xml:"http://arxiv.org/schemas/atom doi"`
}

func (ps *papersStore) arxivQueryURL(id string) string {
	base := "https://export.arxiv.org/api/query"
	if ps != nil && strings.TrimSpace(ps.arxivAPIBase) != "" {
		base = strings.TrimRight(strings.TrimSpace(ps.arxivAPIBase), "/")
	}
	return base + "?id_list=" + url.QueryEscape(id)
}

func fetchArxivMeta(ctx context.Context, client *http.Client, queryURL string) (paperEntry, error) {
	id := parseArxivID(queryURL)
	if id == "" {
		if u, err := url.Parse(queryURL); err == nil {
			id = sanitizeArxivID(u.Query().Get("id_list"))
		}
	}
	if queryURL == "" {
		return paperEntry{}, errImportEmpty
	}
	if client == nil {
		client = defaultImportHTTPClient()
	}
	u := queryURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return paperEntry{}, err
	}
	req.Header.Set("User-Agent", importUserAgent)
	req.Header.Set("Accept", "application/atom+xml, application/xml, text/xml")
	cctx, cancel := context.WithTimeout(ctx, importMetaTimeout)
	defer cancel()
	req = req.WithContext(cctx)
	resp, err := client.Do(req)
	if err != nil {
		return paperEntry{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return paperEntry{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return paperEntry{}, fmt.Errorf("arxiv api HTTP %d", resp.StatusCode)
	}
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return paperEntry{}, err
	}
	if len(feed.Entries) == 0 {
		return paperEntry{}, errImportEmpty
	}
	return paperFromAtom(feed.Entries[0], id), nil
}

func paperFromAtom(e atomEntry, fallbackID string) paperEntry {
	title := collapseWS(e.Title)
	abstract := collapseWS(e.AbstractOrSummary())
	if len(abstract) > 1200 {
		abstract = abstract[:1200]
	}
	authors := make([]string, 0, len(e.Authors))
	for _, a := range e.Authors {
		if n := collapseWS(a.Name); n != "" {
			authors = append(authors, n)
		}
	}
	id := parseArxivID(e.ID)
	if id == "" {
		id = fallbackID
	}
	year := 0
	if len(e.Published) >= 4 {
		fmt.Sscanf(e.Published[:4], "%d", &year)
	}
	sourceURL := "https://arxiv.org/abs/" + id
	pdfURL := "https://arxiv.org/pdf/" + id + ".pdf"
	for _, l := range e.Links {
		if strings.EqualFold(l.Type, "application/pdf") || strings.EqualFold(l.Title, "pdf") {
			if l.Href != "" {
				pdfURL = l.Href
			}
		}
		if strings.EqualFold(l.Rel, "alternate") && l.Href != "" {
			sourceURL = l.Href
		}
	}
	return paperEntry{
		Title:     title,
		Authors:   authors,
		Abstract:  abstract,
		Year:      year,
		SourceURL: sourceURL,
		PDFURL:    pdfURL,
		ArxivID:   id,
		DOI:       strings.TrimSpace(e.DOI),
		Published: strings.TrimSpace(e.Published),
		Updated:   strings.TrimSpace(e.Updated),
	}
}

func (e atomEntry) AbstractOrSummary() string {
	return e.Summary
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func fetchPDFBytes(ctx context.Context, client *http.Client, raw string, allowPrivate bool) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errImportEmpty
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errBlockedFetch
	}
	if !allowPrivate {
		if err := validatePublicURL(u); err != nil {
			return nil, err
		}
	}
	if client == nil {
		client = defaultImportHTTPClient()
	}
	cctx, cancel := context.WithTimeout(ctx, importPDFTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", importUserAgent)
	req.Header.Set("Accept", "application/pdf,*/*")
	resp, err := client.Do(req)
	if err != nil {
		if !allowPrivate && isBlockedNetErr(err) {
			return nil, errBlockedFetch
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pdf HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, importPDFMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > importPDFMaxBytes {
		return nil, errors.New("pdf exceeds size limit")
	}
	if !isPDFBytes(body) {
		// ArXiv sometimes serves an HTML interstitial; retry once with download=1.
		if strings.Contains(strings.ToLower(u.Host), "arxiv.org") && !strings.Contains(u.RawQuery, "download=") {
			q := u.Query()
			q.Set("download", "1")
			u.RawQuery = q.Encode()
			return fetchPDFBytes(ctx, client, u.String(), allowPrivate)
		}
		return nil, errNotPDF
	}
	return body, nil
}

func isPDFBytes(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	// Allow a short leading BOM / whitespace before %PDF.
	s := strings.TrimLeft(string(b[:min(len(b), 16)]), "\ufeff \t\r\n")
	return strings.HasPrefix(s, "%PDF")
}

func validatePublicURL(u *url.URL) error {
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return errBlockedFetch
	}
	if isForbiddenHost(host) {
		return errBlockedFetch
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return errBlockedFetch
	}
	return nil
}

func isForbiddenHost(host string) bool {
	switch host {
	case "localhost", "localhost.localdomain", "metadata.google.internal":
		return true
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	if host == "169.254.169.254" || host == "metadata" {
		return true
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

func isBlockedNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errBlockedFetch) {
		return true
	}
	var ne *net.OpError
	if errors.As(err, &ne) && ne.Err != nil && strings.Contains(ne.Err.Error(), "blocked") {
		return true
	}
	return strings.Contains(err.Error(), "blocked private")
}

func defaultImportHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 12 * time.Second}
	return &http.Client{
		Timeout: importPDFTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				if isForbiddenHost(strings.ToLower(host)) {
					return nil, errBlockedFetch
				}
				if ip := net.ParseIP(host); ip != nil {
					if isPrivateIP(ip) {
						return nil, fmt.Errorf("blocked private address")
					}
					return dialer.DialContext(ctx, network, addr)
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ipa := range ips {
					if isPrivateIP(ipa.IP) {
						return nil, fmt.Errorf("blocked private address")
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if err := validatePublicURL(req.URL); err != nil {
				return err
			}
			return nil
		},
	}
}

func requestIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func upsertImportedSQLite(root string, p paperEntry) {
	dbPath := filepath.Join(root, "papers.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
		if err != nil {
			return
		}
	}
	payload, err := json.Marshal(map[string]any{
		"arxiv_id":   p.ArxivID,
		"doi":        p.DOI,
		"title":      p.Title,
		"authors":    p.Authors,
		"abstract":   p.Abstract,
		"year":       p.Year,
		"tags":       p.TopicTags,
		"source_url": p.SourceURL,
		"pdf_url":    p.PDFURL,
		"pdf_path":   p.PDFPath,
		"published":  p.Published,
		"updated":    p.Updated,
	})
	if err != nil {
		return
	}
	script := `
import json, sqlite3, sys, hashlib
from datetime import datetime, timezone
db, raw = sys.argv[1], sys.stdin.read()
p = json.loads(raw)
pid = (p.get("arxiv_id") or "").strip()
if not pid and p.get("doi"):
    pid = "doi:" + str(p["doi"]).lower()
if not pid:
    norm = " ".join((p.get("title") or "").lower().split())
    pid = "title:" + hashlib.sha1(norm.encode()).hexdigest()[:16]
authors = p.get("authors") or []
if isinstance(authors, list):
    authors = "; ".join(str(x) for x in authors if x)
tags = p.get("tags") or []
if isinstance(tags, list):
    tags = "|".join(str(x) for x in tags if x)
now = datetime.now(timezone.utc).isoformat()
ch = hashlib.sha256(f"{p.get('title','')}\n{authors}\n{p.get('abstract','')}\n{tags}".encode()).hexdigest()[:32]
conn = sqlite3.connect(db)
conn.execute("""CREATE TABLE IF NOT EXISTS papers (
    id TEXT PRIMARY KEY, title TEXT NOT NULL, authors TEXT, abstract TEXT, year INTEGER,
    tags TEXT, source_url TEXT, pdf_url TEXT, pdf_path TEXT, published TEXT, updated TEXT,
    first_seen TEXT, last_seen TEXT, content_hash TEXT)""")
row = conn.execute("SELECT first_seen FROM papers WHERE id=?", (pid,)).fetchone()
first = (row[0] if row and row[0] else now)
conn.execute("""INSERT INTO papers(id,title,authors,abstract,year,tags,source_url,pdf_url,pdf_path,published,updated,first_seen,last_seen,content_hash)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
 title=excluded.title, authors=excluded.authors, abstract=excluded.abstract, year=excluded.year,
 tags=excluded.tags, source_url=excluded.source_url, pdf_url=excluded.pdf_url, pdf_path=excluded.pdf_path,
 published=COALESCE(NULLIF(excluded.published,''), papers.published),
 updated=COALESCE(NULLIF(excluded.updated,''), papers.updated),
 last_seen=excluded.last_seen, content_hash=excluded.content_hash""",
 (pid, p.get("title") or "", authors, p.get("abstract") or "", p.get("year"), tags,
  p.get("source_url") or "", p.get("pdf_url") or "", p.get("pdf_path") or "",
  p.get("published") or "", p.get("updated") or "", first, now, ch))
conn.commit()
`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, dbPath)
	cmd.Stdin = strings.NewReader(string(payload))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("papers import: sqlite upsert: %v %s", err, bytesPreview(out))
	}
}

func bytesPreview(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
