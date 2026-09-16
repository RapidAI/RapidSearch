package api

import (
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
	catalogSnapshotFileName     = "catalog-snapshot.json"
	catalogSnapshotIntervalDef  = 60 * time.Second
	envCatalogSnapshotInterval  = "PAPERS_CATALOG_SNAPSHOT_INTERVAL"
	catalogSnapshotCacheControl = "private, max-age=15"
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
	out := snapshotFromCatalog(ps, cat)
	raw, err := json.Marshal(out)
	if err != nil {
		log.Printf("papers catalog snapshot marshal: %v", err)
		return
	}
	etag := snapshotETag(raw)
	out.SnapshotETag = etag
	raw, err = json.Marshal(out)
	if err != nil {
		log.Printf("papers catalog snapshot marshal: %v", err)
		return
	}
	if err := atomicWriteFile(catalogSnapshotPath(ps.root), append(raw, '\n'), 0o644); err != nil {
		log.Printf("papers catalog snapshot write: %v", err)
		return
	}
	ps.snapMu.Lock()
	ps.snapCat = out
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
	ps.snapMu.Lock()
	ps.snapCat = out
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
	out, etag, err := ps.loadCatalogSnapshot()
	if err != nil {
		cat, liveErr := ps.catalog()
		if liveErr != nil {
			log.Printf("papers catalog snapshot: %v (live: %v)", err, liveErr)
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		out = snapshotFromCatalog(ps, cat)
		if raw, mErr := json.Marshal(out); mErr == nil {
			out.SnapshotETag = snapshotETag(raw)
		}
		ps.requestCatalogSnapshot()
		etag = out.SnapshotETag
	}
	canManage := s.settingsAuthed(r)
	out.CanManage = canManage
	out.TranslateProgress = nil
	if ps != nil && ps.visits != nil {
		out.Visits = ps.visits.Total()
	}
	s.kickPapersSideEffects(canManage, out.Papers)
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
	w.Header().Set("Cache-Control", catalogSnapshotCacheControl)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePapersProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	ps := s.papers()
	cat, err := ps.catalog()
	if err != nil {
		log.Printf("papers progress: %v", err)
		writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
		return
	}
	canManage := s.settingsAuthed(r)
	s.kickPapersSideEffects(canManage, cat.Papers)

	snapAt, snapETag := "", ""
	if cached, etag, ok := ps.cachedCatalogSnapshot(); ok {
		snapAt = cached.SnapshotAt
		snapETag = etag
	}

	pending := 0
	if ps != nil && ps.absZH != nil {
		pending = ps.absZH.pendingCount(cat.Papers)
	}
	out := papersProgress{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		SnapshotAt:        snapAt,
		SnapshotETag:      snapETag,
		TranslateProgress: summarizeTranslateProgress(cat.Papers),
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

func (s *Server) kickPapersSideEffects(canManage bool, papers []paperEntry) {
	if canManage {
		s.maybeAutoTranslate(papers)
	}
	if abs := s.papers().absZH; abs != nil {
		abs.ensureMissing(papers)
	}
}
