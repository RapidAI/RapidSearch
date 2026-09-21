package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"search-service/internal/search"
)

const (
	catalogSnapshotFileName    = "catalog-snapshot.json"
	catalogSnapshotGZFileName  = "catalog-snapshot.json.gz"
	catalogSnapshotIntervalDef = 60 * time.Second
	envCatalogSnapshotInterval = "PAPERS_CATALOG_SNAPSHOT_INTERVAL"
	// Public + SWR so browsers/CDN can reuse the static snapshot. Live
	// can_manage / visits / translate progress stay on /papers/api/progress.
	catalogSnapshotCacheControl = "public, max-age=60, stale-while-revalidate=300"
	// Revalidate HTML so visit bumps still run; allow stale reuse on slow tunnels.
	papersHTMLCacheControl = "public, max-age=0, stale-while-revalidate=300"
	// Card UI shows 280 EN / 140 ZH runes — first paint only needs that teaser.
	catalogSnapshotAbstractRunes   = 280
	catalogSnapshotAbstractZHRunes = 140
	catalogFirstPageLimit          = 25
	catalogMaxAuthors              = 8
	catalogPageLimitMax            = 10000
	// Warn if the lean JSON would stress the 2 MiB tunnel buffered limit.
	catalogSnapshotWarnBytes = 2 << 20
)

// papersProgress is the public live payload for GET /papers/api/progress.
// It stays far smaller than the full catalog (no abstracts).
type papersProgress struct {
	GeneratedAt       string                     `json:"generated_at"`
	SnapshotAt        string                     `json:"snapshot_at,omitempty"`
	SnapshotETag      string                     `json:"snapshot_etag,omitempty"`
	TranslateProgress *translateProgress         `json:"translate_progress"`
	AbstractZHPending int                        `json:"abstract_zh_pending"`
	CanManage         bool                       `json:"can_manage"`
	Visits            int64                      `json:"visits,omitempty"`
	Jobs              map[string]progressJobView `json:"jobs,omitempty"`
}

// progressJobView is a compact per-paper overlay for volatile translate state.
type progressJobView struct {
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	ZhPDF   string `json:"zh_pdf,omitempty"`
	DualPDF string `json:"dual_pdf,omitempty"`
}

func catalogSnapshotPath(root string) string {
	if root == "" {
		root = papersRoot()
	}
	return filepath.Join(root, catalogSnapshotFileName)
}

func catalogSnapshotGZPath(root string) string {
	if root == "" {
		root = papersRoot()
	}
	return filepath.Join(root, catalogSnapshotGZFileName)
}

// catalogSnapshotInterval is how often the static catalog file is rewritten.
// PAPERS_CATALOG_SNAPSHOT_INTERVAL accepts a Go duration ("60s", "2m") or an
// integer number of seconds. Default: 60s.
func catalogSnapshotInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(envCatalogSnapshotInterval))
	if raw == "" {
		return catalogSnapshotIntervalDef
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return catalogSnapshotIntervalDef
}

func (ps *papersStore) startCatalogSnapshot() {
	if ps == nil {
		return
	}
	ps.snapMu.Lock()
	if ps.snapKick != nil {
		ps.snapMu.Unlock()
		return
	}
	ps.snapKick = make(chan struct{}, 1)
	ps.snapStop = make(chan struct{})
	ps.snapWG.Add(1)
	ps.snapMu.Unlock()
	go func() {
		defer ps.snapWG.Done()
		ps.catalogSnapshotLoop()
	}()
	ps.requestCatalogSnapshot()
}

func (ps *papersStore) stopCatalogSnapshot() {
	if ps == nil {
		return
	}
	ps.snapMu.Lock()
	if ps.snapStop == nil || ps.snapStopped {
		ps.snapMu.Unlock()
		return
	}
	ps.snapStopped = true
	close(ps.snapStop)
	ps.snapMu.Unlock()
	ps.snapWG.Wait()
}

func (ps *papersStore) requestCatalogSnapshot() {
	if ps == nil {
		return
	}
	ps.snapMu.RLock()
	ch := ps.snapKick
	ps.snapMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (ps *papersStore) catalogSnapshotLoop() {
	interval := catalogSnapshotInterval()
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ps.snapStop:
			return
		case <-t.C:
			ps.refreshCatalogSnapshot()
		case <-ps.snapKick:
			ps.refreshCatalogSnapshot()
		}
	}
}

func (ps *papersStore) snapshotStopped() bool {
	if ps == nil {
		return true
	}
	ps.snapMu.RLock()
	defer ps.snapMu.RUnlock()
	return ps.snapStopped
}

func (ps *papersStore) refreshCatalogSnapshot() {
	if ps == nil || ps.snapshotStopped() {
		return
	}
	cat, err := ps.catalog()
	if err != nil {
		log.Printf("papers catalog snapshot: %v", err)
		return
	}
	out := leanCatalogForSnapshot(ps, cat)
	raw, gz, etag, err := encodeCatalogSnapshot(out)
	if err != nil {
		log.Printf("papers catalog snapshot marshal: %v", err)
		return
	}
	out.SnapshotETag = etag
	if err := atomicWriteFile(catalogSnapshotPath(ps.root), raw, 0o644); err != nil {
		log.Printf("papers catalog snapshot write: %v", err)
		return
	}
	if len(gz) > 0 {
		if err := atomicWriteFile(catalogSnapshotGZPath(ps.root), gz, 0o644); err != nil {
			log.Printf("papers catalog snapshot gzip write: %v", err)
			gz = nil
		}
	}
	if len(raw) > catalogSnapshotWarnBytes {
		log.Printf("papers catalog snapshot is %d bytes (gzip %d); over 2MiB tunnel buffered limit — streaming still works", len(raw), len(gz))
	}
	ps.snapMu.Lock()
	ps.snapCat = out
	ps.snapRaw = raw
	ps.snapGZ = gz
	ps.snapETag = etag
	ps.snapReady = true
	ps.snapMu.Unlock()
}

func snapshotFromCatalog(ps *papersStore, cat papersCatalog) papersCatalog {
	out := papersCatalog{
		GeneratedAt: cat.GeneratedAt,
		Stats:       cat.Stats,
		Count:       cat.Count,
		Papers:      append([]paperEntry(nil), cat.Papers...),
		SnapshotAt:  time.Now().UTC().Format(time.RFC3339),
	}
	// Volatile progress is served live via /papers/api/progress.
	out.TranslateProgress = nil
	out.CanManage = false
	if ps != nil && ps.absZH != nil {
		out.AbstractZHPending = ps.absZH.pendingCount(out.Papers)
	}
	if ps != nil && ps.visits != nil {
		out.Visits = ps.visits.Total()
	}
	return out
}

// leanCatalogForSnapshot copies the catalog and keeps only card-sized fields
// so first paint (and client search) stays well under the tunnel frame budget.
func leanCatalogForSnapshot(ps *papersStore, cat papersCatalog) papersCatalog {
	out := snapshotFromCatalog(ps, cat)
	if len(out.Papers) == 0 {
		return out
	}
	papers := make([]paperEntry, len(out.Papers))
	for i, p := range out.Papers {
		papers[i] = leanPaperForSnapshot(p)
	}
	out.Papers = papers
	return out
}

// leanPaperForSnapshot keeps the fields cards + client search need and drops
// duplicates / unused metadata that dominated the public ~1.5MB snapshot.
func leanPaperForSnapshot(p paperEntry) paperEntry {
	p.Abstract = briefRunes(p.Abstract, catalogSnapshotAbstractRunes)
	p.AbstractZH = briefRunes(p.AbstractZH, catalogSnapshotAbstractZHRunes)
	p.Brief = ""
	p.QueryHits = nil
	p.PDFURL = ""
	p.DOI = ""
	p.Score = 0
	p.DownloadStatus = ""
	p.Updated = ""
	p.Filename = ""
	p.PDFPath = ""
	if len(p.Authors) > catalogMaxAuthors {
		p.Authors = append([]string(nil), p.Authors[:catalogMaxAuthors]...)
	}
	return p
}

func catalogPageArgs(r *http.Request) (offset, limit int, paged bool) {
	if r == nil {
		return 0, 0, false
	}
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("limit")) == "" && strings.TrimSpace(q.Get("offset")) == "" {
		return 0, 0, false
	}
	offset, _ = strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	limit, _ = strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > catalogPageLimitMax {
		limit = catalogPageLimitMax
	}
	return offset, limit, true
}

func sliceCatalog(cat papersCatalog, offset, limit int) papersCatalog {
	total := cat.Count
	if total == 0 {
		total = len(cat.Papers)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(cat.Papers) {
		offset = len(cat.Papers)
	}
	if limit <= 0 {
		limit = catalogPageLimitMax
	}
	end := offset + limit
	if end > len(cat.Papers) {
		end = len(cat.Papers)
	}
	out := cat
	out.Papers = append([]paperEntry(nil), cat.Papers[offset:end]...)
	out.Count = total
	out.Offset = offset
	out.Limit = limit
	out.Partial = end < len(cat.Papers) || offset > 0
	if end < len(cat.Papers) {
		out.NextOffset = end
	}
	return out
}

func encodeCatalogPage(full papersCatalog, offset, limit int, fullETag string) (raw, gz []byte, httpETag string, err error) {
	page := sliceCatalog(full, offset, limit)
	page.SnapshotETag = fullETag
	raw, err = json.Marshal(page)
	if err != nil {
		return nil, nil, "", err
	}
	raw = append(raw, '\n')
	gz = gzipBytes(raw)
	httpETag = fullETag + "-o" + strconv.Itoa(offset) + "l" + strconv.Itoa(limit)
	return raw, gz, httpETag, nil
}

func encodeCatalogSnapshot(out papersCatalog) (raw, gz []byte, etag string, err error) {
	raw, err = json.Marshal(out)
	if err != nil {
		return nil, nil, "", err
	}
	etag = snapshotETag(raw)
	out.SnapshotETag = etag
	raw, err = json.Marshal(out)
	if err != nil {
		return nil, nil, "", err
	}
	raw = append(raw, '\n')
	gz = gzipBytes(raw)
	return raw, gz, etag, nil
}

func gzipBytes(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func snapshotETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func (ps *papersStore) cachedCatalogSnapshot() (papersCatalog, string, bool) {
	if ps == nil {
		return papersCatalog{}, "", false
	}
	ps.snapMu.RLock()
	defer ps.snapMu.RUnlock()
	if !ps.snapReady {
		return papersCatalog{}, "", false
	}
	out := ps.snapCat
	out.Papers = append([]paperEntry(nil), ps.snapCat.Papers...)
	return out, ps.snapETag, true
}

func (ps *papersStore) catalogSnapshotBytes() (raw, gz []byte, etag string, err error) {
	if ps == nil {
		return nil, nil, "", os.ErrNotExist
	}
	ps.snapMu.RLock()
	if ps.snapReady && len(ps.snapRaw) > 0 {
		raw = ps.snapRaw
		gz = ps.snapGZ
		etag = ps.snapETag
		ps.snapMu.RUnlock()
		return raw, gz, etag, nil
	}
	ps.snapMu.RUnlock()
	_, etag, err = ps.loadCatalogSnapshot()
	if err != nil {
		return nil, nil, "", err
	}
	ps.snapMu.RLock()
	raw = ps.snapRaw
	gz = ps.snapGZ
	etag = ps.snapETag
	ps.snapMu.RUnlock()
	if len(raw) == 0 {
		return nil, nil, "", os.ErrNotExist
	}
	return raw, gz, etag, nil
}

func (ps *papersStore) loadCatalogSnapshot() (papersCatalog, string, error) {
	if out, etag, ok := ps.cachedCatalogSnapshot(); ok {
		return out, etag, nil
	}
	raw, err := os.ReadFile(catalogSnapshotPath(ps.root))
	if err != nil {
		return papersCatalog{}, "", err
	}
	var out papersCatalog
	if err := json.Unmarshal(raw, &out); err != nil {
		return papersCatalog{}, "", err
	}
	etag := strings.TrimSpace(out.SnapshotETag)
	if etag == "" {
		etag = snapshotETag(raw)
		out.SnapshotETag = etag
	}
	gz, _ := os.ReadFile(catalogSnapshotGZPath(ps.root))
	if len(gz) == 0 {
		gz = gzipBytes(raw)
	}
	ps.snapMu.Lock()
	ps.snapCat = out
	ps.snapRaw = raw
	ps.snapGZ = gz
	ps.snapETag = etag
	ps.snapReady = true
	ps.snapMu.Unlock()
	out.Papers = append([]paperEntry(nil), out.Papers...)
	return out, etag, nil
}

func (s *Server) handlePapersCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	ps := s.papers()
	raw, gz, etag, err := ps.catalogSnapshotBytes()
	var papers []paperEntry
	if cached, _, ok := ps.cachedCatalogSnapshot(); ok {
		papers = cached.Papers
	}
	if err != nil {
		cat, liveErr := ps.catalog()
		if liveErr != nil {
			log.Printf("papers catalog snapshot: %v (live: %v)", err, liveErr)
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		out := leanCatalogForSnapshot(ps, cat)
		var encErr error
		raw, gz, etag, encErr = encodeCatalogSnapshot(out)
		if encErr != nil {
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		out.SnapshotETag = etag
		papers = out.Papers
		ps.requestCatalogSnapshot()
	}
	canManage := s.settingsAuthed(r)
	s.kickPapersSideEffects(canManage, papers)
	if offset, limit, paged := catalogPageArgs(r); paged {
		var full papersCatalog
		if cached, _, ok := ps.cachedCatalogSnapshot(); ok {
			full = cached
		} else if err := json.Unmarshal(raw, &full); err != nil {
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		if full.SnapshotETag == "" {
			full.SnapshotETag = etag
		}
		pageRaw, pageGZ, pageETag, encErr := encodeCatalogPage(full, offset, limit, etag)
		if encErr != nil {
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		if noneMatch(r.Header.Get("If-None-Match"), pageETag) || noneMatch(r.Header.Get("If-None-Match"), etag) {
			w.Header().Set("Cache-Control", catalogSnapshotCacheControl)
			w.Header().Set("Vary", "Accept-Encoding")
			w.Header().Set("ETag", `"`+pageETag+`"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		serveCachedJSON(w, r, pageRaw, pageGZ, pageETag)
		return
	}
	serveCachedJSON(w, r, raw, gz, etag)
}

func serveCachedValue(w http.ResponseWriter, r *http.Request, v interface{}, etag string) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "encode failed", "papers", nil, "")
		return
	}
	raw = append(raw, '\n')
	serveCachedJSON(w, r, raw, gzipBytes(raw), etag)
}

func serveCachedJSON(w http.ResponseWriter, r *http.Request, raw, gz []byte, etag string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", catalogSnapshotCacheControl)
	w.Header().Set("Vary", "Accept-Encoding")
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
		if noneMatch(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body := raw
	if acceptsGzip(r) && len(gz) > 0 && (len(raw) == 0 || len(gz) < len(raw)) {
		w.Header().Set("Content-Encoding", "gzip")
		body = gz
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func acceptsGzip(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.EqualFold(enc, "gzip") {
			return true
		}
	}
	return false
}

func noneMatch(header, etag string) bool {
	etag = strings.TrimSpace(etag)
	if header == "" || etag == "" {
		return false
	}
	quoted := `"` + etag + `"`
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(part)
		if token == "*" {
			return true
		}
		token = strings.TrimPrefix(token, "W/")
		if token == quoted || token == etag {
			return true
		}
	}
	return false
}

func (s *Server) handlePapersProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	ps := s.papers()
	snapAt, snapETag := "", ""
	var papers []paperEntry
	if cached, etag, ok := ps.cachedCatalogSnapshot(); ok {
		snapAt = cached.SnapshotAt
		snapETag = etag
		papers = append([]paperEntry(nil), cached.Papers...)
	} else {
		cat, err := ps.catalog()
		if err != nil {
			log.Printf("papers progress: %v", err)
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		papers = cat.Papers
	}
	canManage := s.settingsAuthed(r)
	s.kickPapersSideEffects(canManage, papers)
	overlayJobStatus(papers, ps)

	pending := 0
	if ps != nil && ps.absZH != nil {
		pending = ps.absZH.pendingCount(papers)
	}
	out := papersProgress{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		SnapshotAt:        snapAt,
		SnapshotETag:      snapETag,
		TranslateProgress: summarizeTranslateProgress(papers),
		AbstractZHPending: pending,
		CanManage:         canManage,
		Jobs:              progressJobs(ps, snapAt),
	}
	if ps != nil && ps.visits != nil {
		out.Visits = ps.visits.Total()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func progressJobs(ps *papersStore, snapshotAt string) map[string]progressJobView {
	if ps == nil || ps.xlate == nil {
		return nil
	}
	jobs := ps.xlate.jobsCopy()
	if len(jobs) == 0 {
		return nil
	}
	out := make(map[string]progressJobView, 8)
	for id, j := range jobs {
		keep := false
		switch j.Status {
		case translateQueued, translateRunning, translateFailed, translateSkipped:
			keep = true
		case translateDone:
			// Surface completions that landed after the static snapshot.
			if jobNewerThanSnapshot(j.UpdatedAt, snapshotAt) {
				keep = true
			}
		}
		if !keep {
			continue
		}
		view := progressJobView{
			Status: j.Status,
			Error:  sanitizeUserError(j.Error),
		}
		if j.ZhRel != "" {
			view.ZhPDF = translatedPDFURL(translateKindZH, id)
		}
		if j.DualRel != "" {
			view.DualPDF = translatedPDFURL(translateKindDual, id)
		}
		out[id] = view
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func jobNewerThanSnapshot(jobUpdated, snapshotAt string) bool {
	if snapshotAt == "" || strings.TrimSpace(jobUpdated) == "" {
		return false
	}
	jt, jok := parseJobStartedAt(jobUpdated)
	st, sok := parseJobStartedAt(snapshotAt)
	if jok && sok {
		return jt.After(st)
	}
	return jobUpdated > snapshotAt
}

func overlayJobStatus(papers []paperEntry, ps *papersStore) {
	if ps == nil || ps.xlate == nil || len(papers) == 0 {
		return
	}
	jobs := ps.xlate.jobsCopy()
	if len(jobs) == 0 {
		return
	}
	for i := range papers {
		id := papers[i].ID
		if id == "" {
			id = paperTranslateID(papers[i])
		}
		j, ok := jobs[id]
		if !ok {
			continue
		}
		papers[i].TranslateStatus = j.Status
		switch j.Status {
		case translateDone, translateRunning:
			papers[i].TranslateError = ""
		default:
			papers[i].TranslateError = sanitizeUserError(j.Error)
		}
		if j.ZhRel != "" {
			papers[i].ZhPDF = translatedPDFURL(translateKindZH, id)
		}
		if j.DualRel != "" {
			papers[i].DualPDF = translatedPDFURL(translateKindDual, id)
		}
	}
}

func writePapersHTML(w http.ResponseWriter, r *http.Request, page []byte) {
	lang := acceptLang(r)
	body := applyHTMLLang(page, lang)
	etag := snapshotETag(body)
	gz := gzipBytes(body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", papersHTMLCacheControl)
	w.Header().Set("Vary", "Accept-Encoding, Accept-Language")
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
		if noneMatch(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	out := body
	if acceptsGzip(r) && len(gz) > 0 && len(gz) < len(body) {
		w.Header().Set("Content-Encoding", "gzip")
		out = gz
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) kickPapersSideEffects(canManage bool, papers []paperEntry) {
	if canManage {
		s.maybeAutoTranslate(papers)
	}
	if abs := s.papers().absZH; abs != nil {
		abs.ensureMissing(papers)
	}
	if sec := s.papers().secTrends; sec != nil {
		sec.ensureMissing()
	}
}
