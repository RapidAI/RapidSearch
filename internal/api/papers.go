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
	Filename        string `json:"filename,omitempty"`
	HasLocal        bool   `json:"has_local"`
	LocalPDF        string `json:"local_pdf,omitempty"` // relative download path
	Brief           string `json:"brief,omitempty"`
	ID              string `json:"id,omitempty"`
	AbstractZH      string `json:"abstract_zh,omitempty"`
	ZhPDF           string `json:"zh_pdf,omitempty"`
	DualPDF         string `json:"dual_pdf,omitempty"`
	TranslateStatus string `json:"translate_status,omitempty"`
	TranslateError  string `json:"translate_error,omitempty"`

	// Review flags for catalog cards (no rater list).
	HasReview   bool    `json:"has_review"`
	AvgStars    float64 `json:"avg_stars"`
	RatingCount int     `json:"rating_count"`
}

type papersManifest struct {
	GeneratedAt string                 `json:"generated_at"`
	Stats       map[string]interface{} `json:"stats,omitempty"`
	Papers      []paperEntry           `json:"papers"`
}

type papersCatalog struct {
	GeneratedAt       string                 `json:"generated_at"`
	Stats             map[string]interface{} `json:"stats,omitempty"`
	Count             int                    `json:"count"`
	Papers            []paperEntry           `json:"papers"`
	TranslateProgress *translateProgress     `json:"translate_progress,omitempty"`
	// AbstractZHPending is the number of papers still missing a cached Chinese abstract.
	// Used by the ZH UI to keep polling until translations land.
	AbstractZHPending int `json:"abstract_zh_pending,omitempty"`
	// CanManage is true when the caller may enqueue translations / open admin UI.
	// Anonymous catalog readers get false; Hub admin / SEARCH_TOKEN get true.
	CanManage bool `json:"can_manage"`
	// Visits is cumulative GET /papers page views (not API polls).
	Visits int64 `json:"visits"`
}

// translateProgress is a page-level summary of background BabelDOC jobs.
type translateProgress struct {
	Queued        int      `json:"queued"`
	Running       int      `json:"running"`
	Active        bool     `json:"active"`
	RunningID     string   `json:"running_id,omitempty"`
	RunningTitle  string   `json:"running_title,omitempty"`
	RunningIDs    []string `json:"running_ids,omitempty"`
	RunningTitles []string `json:"running_titles,omitempty"`
}

type papersStore struct {
	mu      sync.Mutex
	root    string
	loaded  time.Time
	modTime time.Time
	cat     papersCatalog
	xlate   *translateService
	absZH   *abstractZHService
	reviews *reviewService
	visits  *visitCounter
	daily   *hfDailyService
}

func papersRoot() string {
	if v := strings.TrimSpace(os.Getenv("PAPERS_DIR")); v != "" {
		return v
	}
	return defaultPapersDir
}

func newPapersStore(root string) *papersStore {
	if root == "" {
		root = papersRoot()
	}
	xlate := newTranslateService(root)
	ps := &papersStore{
		root:    root,
		xlate:   xlate,
		absZH:   newAbstractZHService(root, xlate),
		reviews: newReviewService(root, xlate),
		visits:  newVisitCounter(root),
	}
	ps.daily = newHFDailyService(ps)
	ps.xlate.start()
	ps.absZH.start()
	return ps
}

func (s *Server) papers() *papersStore {
	if s == nil {
		return nil
	}
	if s.papersStore == nil {
		s.papersStore = newPapersStore(papersRoot())
	}
	return s.papersStore
}

func (ps *papersStore) catalog() (papersCatalog, error) {
	base, err := ps.catalogBase()
	if err != nil {
		return papersCatalog{}, err
	}
	out := papersCatalog{
		GeneratedAt: base.GeneratedAt,
		Stats:       base.Stats,
		Count:       base.Count,
		Papers:      append([]paperEntry(nil), base.Papers...),
	}
	ps.decoratePapers(out.Papers)
	return out, nil
}

func (ps *papersStore) decoratePapers(papers []paperEntry) {
	if ps == nil || len(papers) == 0 {
		return
	}
	var jobs map[string]translateJob
	if ps.xlate != nil {
		jobs = ps.xlate.jobsCopy()
	}
	overlayTranslations(papers, ps.root, jobs)
	if ps.absZH != nil {
		ps.absZH.overlay(papers)
	}
	if ps.reviews != nil {
		ps.reviews.overlay(papers)
	}
}

func (ps *papersStore) invalidateCatalog() {
	if ps == nil {
		return
	}
	ps.mu.Lock()
	ps.loaded = time.Time{}
	ps.modTime = time.Time{}
	ps.mu.Unlock()
}

func (ps *papersStore) catalogBase() (papersCatalog, error) {
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
	p.ID = paperTranslateID(p)
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
	case http.MethodGet:
		// Count HTML page views only (not HEAD, not /papers/api polls).
		if ps := s.papers(); ps != nil && ps.visits != nil {
			ps.visits.Bump()
		}
		// Catalog page is public; Settings link always shown. Translate actions
		// are gated in the UI via /papers/api can_manage, and mutations still
		// require authorizeSettings. Unauthenticated /settings shows login.
		writeSettingsHTML(w, r, papersPageHTML)
	case http.MethodHead:
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
	// Public catalog listing. can_manage reflects Hub admin / SEARCH_TOKEN.
	// Do not expose translate LLM config or API keys here.
	cat, err := s.papers().catalog()
	if err != nil {
		log.Printf("papers catalog: %v", err)
		writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
		return
	}
	canManage := s.settingsAuthed(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	// Progress is always from the full catalog so filters cannot hide in-flight work.
	progress := summarizeTranslateProgress(cat.Papers)
	out := cat
	out.CanManage = canManage
	out.TranslateProgress = progress
	if ps := s.papers(); ps != nil && ps.visits != nil {
		out.Visits = ps.visits.Total()
	}
	if q != "" || tag != "" {
		filtered := filterPapers(cat.Papers, q, tag)
		out.Papers = filtered
		out.Count = len(filtered)
	}
	// Auto-enqueue PDF BabelDOC is an admin side effect — never from anonymous GETs.
	if canManage {
		s.maybeAutoTranslate(cat.Papers)
	}
	// Chinese abstracts are public catalog data: fill missing cache in background.
	if abs := s.papers().absZH; abs != nil {
		abs.ensureMissing(cat.Papers)
		out.AbstractZHPending = abs.pendingCount(out.Papers)
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
				p.AbstractZH,
				p.ArxivID,
				p.ID,
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
	// Existing local / zh / dual PDFs are publicly downloadable.
	raw := strings.TrimPrefix(r.URL.Path, "/papers/pdf/")
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	root := s.papers().root
	if root == "" {
		root = papersRoot()
	}
	kind, rest, isXlate := splitTranslatePDFPath(raw)
	var full, serveName string
	var err error
	if isXlate {
		full, serveName, err = resolveTranslatedPDFName(root, kind, rest)
		if err != nil || full == "" {
			http.NotFound(w, r)
			return
		}
	} else {
		name, err := resolveLocalPDFName(root, raw)
		if err != nil || name == "" {
			http.NotFound(w, r)
			return
		}
		full, err = originalPDFAbs(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		serveName = name
	}
	disp := "inline"
	if strings.EqualFold(r.URL.Query().Get("download"), "1") ||
		strings.EqualFold(r.URL.Query().Get("download"), "true") {
		disp = "attachment"
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", disp+`; filename="`+sanitizeDisposition(serveName)+`"`)
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeFile(w, r, full)
}

func splitTranslatePDFPath(raw string) (kind, rest string, ok bool) {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if strings.HasPrefix(raw, "zh/") {
		return translateKindZH, strings.TrimPrefix(raw, "zh/"), true
	}
	if strings.HasPrefix(raw, "dual/") {
		return translateKindDual, strings.TrimPrefix(raw, "dual/"), true
	}
	return "", raw, false
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
