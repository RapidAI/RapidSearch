package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "embed"

	"search-service/internal/search"
)

//go:embed papers.html
var papersPageHTML []byte

const defaultPapersDir = "/workspace/agent-papers"

type paperEntry struct {
	Title          string   `json:"title"`
	Authors        []string `json:"authors,omitempty"`
	Abstract       string   `json:"abstract,omitempty"`
	Year           int      `json:"year,omitempty"`
	SourceURL      string   `json:"source_url,omitempty"`
	PDFURL         string   `json:"pdf_url,omitempty"`
	ArxivID        string   `json:"arxiv_id,omitempty"`
	DOI            string   `json:"doi,omitempty"`
	TopicTags      []string `json:"topic_tags,omitempty"`
	Score          float64  `json:"score,omitempty"`
	PDFPath        string   `json:"pdf_path,omitempty"`
	DownloadStatus string   `json:"download_status,omitempty"`
	Published      string   `json:"published,omitempty"`
	Updated        string   `json:"updated,omitempty"`

	// Filled for API/HTML consumers.
	Filename  string `json:"filename,omitempty"`
	HasLocal  bool   `json:"has_local"`
	LocalPDF  string `json:"local_pdf,omitempty"` // relative download path
	Brief     string `json:"brief,omitempty"`
	PageCount int    `json:"page_count,omitempty"`
}

type papersManifest struct {
	GeneratedAt string                 `json:"generated_at"`
	Stats       map[string]interface{} `json:"stats,omitempty"`
	Papers      []paperEntry           `json:"papers"`
}

type papersCatalog struct {
	GeneratedAt string                 `json:"generated_at"`
	Stats       map[string]interface{} `json:"stats,omitempty"`
	Count       int                    `json:"count"`
	Papers      []paperEntry           `json:"papers"`
	CanManage   bool                   `json:"can_manage"`
}

type papersStore struct {
	mu         sync.Mutex
	root       string
	loaded     time.Time
	modTime    time.Time
	cat        papersCatalog
	httpClient *http.Client
}

func papersRoot() string {
	if v := strings.TrimSpace(os.Getenv("PAPERS_DIR")); v != "" {
		return v
	}
	return defaultPapersDir
}

func (s *Server) papers() *papersStore {
	if s == nil {
		return nil
	}
	if s.papersStore == nil {
		s.papersStore = &papersStore{root: papersRoot()}
	}
	return s.papersStore
}

func (ps *papersStore) catalog() (papersCatalog, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	root := ps.root
	if root == "" {
		root = papersRoot()
		ps.root = root
	}
	manPath := filepath.Join(root, "manifest.json")
	st, err := os.Stat(manPath)
	if err != nil {
		return papersCatalog{}, err
	}
	if !ps.loaded.IsZero() && st.ModTime().Equal(ps.modTime) {
		return ps.cat, nil
	}
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return papersCatalog{}, err
	}
	var man papersManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return papersCatalog{}, err
	}
	pdfDir := filepath.Join(root, "pdfs")
	entries := make([]paperEntry, 0, len(man.Papers))
	for _, p := range man.Papers {
		p = enrichPaper(p, pdfDir)
		entries = append(entries, p)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Year != entries[j].Year {
			return entries[i].Year > entries[j].Year
		}
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		return strings.ToLower(entries[i].Title) < strings.ToLower(entries[j].Title)
	})
	ps.cat = papersCatalog{
		GeneratedAt: man.GeneratedAt,
		Stats:       man.Stats,
		Count:       len(entries),
		Papers:      entries,
	}
	ps.modTime = st.ModTime()
	ps.loaded = time.Now()
	return ps.cat, nil
}

func enrichPaper(p paperEntry, pdfDir string) paperEntry {
	name := filepath.Base(strings.TrimSpace(p.PDFPath))
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	if name == "" && p.ArxivID != "" {
		if hit := findPDFByArxiv(pdfDir, p.ArxivID); hit != "" {
			name = hit
		}
	}
	p.Filename = name
	if name != "" {
		full := filepath.Join(pdfDir, name)
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			p.HasLocal = true
			p.LocalPDF = "/papers/pdf/" + name
		}
	}
	p.Brief = briefText(p.Abstract, 280)
	// Do not leak absolute host paths in API responses.
	p.PDFPath = ""
	return p
}

func findPDFByArxiv(pdfDir, arxivID string) string {
	id := sanitizeArxivID(arxivID)
	if id == "" {
		return ""
	}
	ents, err := os.ReadDir(pdfDir)
	if err != nil {
		return ""
	}
	prefix := id + "_"
	prefixAlt := strings.ReplaceAll(id, "/", "_") + "_"
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(strings.ToLower(n), ".pdf") {
			continue
		}
		if strings.HasPrefix(n, prefix) || strings.HasPrefix(n, prefixAlt) ||
			strings.TrimSuffix(n, ".pdf") == id || strings.TrimSuffix(n, ".pdf") == strings.ReplaceAll(id, "/", "_") {
			return n
		}
	}
	return ""
}

func sanitizeArxivID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "arXiv:")
	id = strings.TrimPrefix(id, "arxiv:")
	// Keep only safe id characters (digits, letters, dots, slash, hyphen).
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '/' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func briefText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	// Prefer breaking at a word boundary.
	cut := s[:max]
	if i := strings.LastIndexAny(cut, " \t\n"); i > max*2/3 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

func (s *Server) handlePapersPage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/papers" && path != "/papers/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !s.settingsAuthed(r) {
			writeSettingsHTML(w, r, loginPageHTML)
			return
		}
		writeSettingsHTML(w, r, papersPageHTML)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *Server) handlePapersAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if !s.authorizeSettings(w, r) {
		return
	}
	cat, err := s.papers().catalog()
	if err != nil {
		log.Printf("papers catalog: %v", err)
		writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	out := cat
	out.CanManage = true
	if q != "" || tag != "" {
		filtered := filterPapers(cat.Papers, q, tag)
		out.Papers = filtered
		out.Count = len(filtered)
	}
	writeJSON(w, http.StatusOK, out)
}

func filterPapers(in []paperEntry, q, tag string) []paperEntry {
	q = strings.ToLower(strings.TrimSpace(q))
	tag = strings.ToLower(strings.TrimSpace(tag))
	out := make([]paperEntry, 0, len(in))
	for _, p := range in {
		if tag != "" {
			ok := false
			for _, t := range p.TopicTags {
				if strings.ToLower(t) == tag {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if q != "" {
			blob := strings.ToLower(strings.Join([]string{
				p.Title,
				p.Abstract,
				p.ArxivID,
				p.Filename,
				strings.Join(p.Authors, " "),
				strings.Join(p.TopicTags, " "),
			}, " "))
			if !strings.Contains(blob, q) {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

func (s *Server) handlePapersPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if !s.authorizeSettings(w, r) {
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/papers/pdf/")
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	// URL path may encode spaces etc.; Go already decodes Path.
	name, err := resolveLocalPDFName(s.papers().root, raw)
	if err != nil || name == "" {
		http.NotFound(w, r)
		return
	}
	root := s.papers().root
	if root == "" {
		root = papersRoot()
	}
	full := filepath.Join(root, "pdfs", name)
	full, err = filepath.Abs(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pdfRoot, err := filepath.Abs(filepath.Join(root, "pdfs"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := filepath.Rel(pdfRoot, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	disp := "inline"
	if strings.EqualFold(r.URL.Query().Get("download"), "1") ||
		strings.EqualFold(r.URL.Query().Get("download"), "true") {
		disp = "attachment"
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", disp+`; filename="`+sanitizeDisposition(name)+`"`)
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeFile(w, r, full)
}

// resolveLocalPDFName maps an arxiv id or filename to a basename under pdfs/.
func resolveLocalPDFName(root, idOrName string) (string, error) {
	if root == "" {
		root = papersRoot()
	}
	idOrName = strings.TrimSpace(idOrName)
	idOrName = strings.ReplaceAll(idOrName, `\`, `/`)
	// Reject absolute / traversal before Base.
	if strings.Contains(idOrName, "..") {
		return "", os.ErrNotExist
	}
	base := filepath.Base(idOrName)
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		return "", os.ErrNotExist
	}
	pdfDir := filepath.Join(root, "pdfs")
	candidate := filepath.Join(pdfDir, base)
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return base, nil
	}
	// Try with .pdf suffix.
	if !strings.HasSuffix(strings.ToLower(base), ".pdf") {
		with := base + ".pdf"
		if st, err := os.Stat(filepath.Join(pdfDir, with)); err == nil && !st.IsDir() {
			return with, nil
		}
	}
	// Arxiv id lookup (may be path like "pdf/2401.05459" after Base → still id).
	id := sanitizeArxivID(strings.TrimSuffix(base, ".pdf"))
	if id == "" {
		return "", os.ErrNotExist
	}
	if hit := findPDFByArxiv(pdfDir, id); hit != "" {
		return hit, nil
	}
	return "", os.ErrNotExist
}

func sanitizeDisposition(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127 || r == '"' || r == '\\' || r == ';':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "paper.pdf"
	}
	return out
}
