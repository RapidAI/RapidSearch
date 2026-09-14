package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"search-service/internal/search"
)

const (
	translateConfigName         = "translate-config.json"
	translateStatusName         = "translate_status.json"
	translateConfigMode         = 0o600
	translateDefaultQPS         = 4
	translateDefaultConcurrency = 3
	translateMinConcurrency     = 1
	translateMaxConcurrency     = 8
)

// Translation job statuses exposed on /papers/api.
const (
	translateNone    = ""
	translateQueued  = "queued"
	translateRunning = "running"
	translateDone    = "done"
	translateFailed  = "failed"
)

type translateFileConfig struct {
	Version       int    `json:"version"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key,omitempty"`
	Model         string `json:"model"`
	QPS           int    `json:"qps,omitempty"`
	AutoTranslate bool   `json:"auto_translate"`
}

type translateSnapshot struct {
	BaseURL       string
	APIKey        string
	Model         string
	QPS           int
	AutoTranslate bool
}

func (s translateSnapshot) ready() bool {
	return strings.TrimSpace(s.APIKey) != "" && strings.TrimSpace(s.Model) != ""
}

type TranslatePublicView struct {
	OK            bool             `json:"ok"`
	ConfigPath    string           `json:"config_path"`
	BaseURL       string           `json:"base_url"`
	Model         string           `json:"model"`
	QPS           int              `json:"qps"`
	AutoTranslate bool             `json:"auto_translate"`
	APIKey        search.MaskedKey `json:"api_key"`
	BabelDOC      bool             `json:"babeldoc"`
}

type translateConfigPatch struct {
	BaseURL       *string `json:"base_url"`
	APIKey        *string `json:"api_key"`
	Model         *string `json:"model"`
	QPS           *int    `json:"qps"`
	AutoTranslate *bool   `json:"auto_translate"`
	ClearAPIKey   bool    `json:"clear_api_key"`
}

type translateJob struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Finished  string `json:"finished_at,omitempty"`
	Filename  string `json:"filename,omitempty"`
	ZhRel     string `json:"zh_pdf,omitempty"`
	DualRel   string `json:"dual_pdf,omitempty"`
	PageCount int    `json:"page_count,omitempty"`
	Lane      string `json:"lane,omitempty"` // fast | slow
}

type translateStatusFile struct {
	Version   int                     `json:"version"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
	Jobs      map[string]translateJob `json:"jobs"`
}

type translateService struct {
	root   string
	mu     sync.Mutex
	cfg    translateFileConfig
	status translateStatusFile
	// Dual-lane queues: fast (≤50 pages) and slow (51–100). inQ dedupes both.
	fastQ []string
	slowQ []string
	inQ   map[string]bool
	// running is the set of paper ids with an in-process runOne goroutine.
	running map[string]bool
	// concurrency is the max number of distinct-paper BabelDOC jobs (1–8).
	concurrency int
	kick        chan struct{}
	runner      translateRunner
	client      *http.Client
	// started is true after the background loop is launched.
	started bool
}

type translateRunner func(ctx context.Context, job translateRun) (zhRel, dualRel string, err error)

type translateRun struct {
	ID       string
	InputPDF string
	Root     string
	Cfg      translateSnapshot
}

func newTranslateService(root string) *translateService {
	if root == "" {
		root = papersRoot()
	}
	s := &translateService{
		root: root,
		cfg:  translateFileConfig{Version: 1, Model: "gpt-4o-mini", QPS: translateDefaultQPS},
		status: translateStatusFile{
			Version: 1,
			Jobs:    map[string]translateJob{},
		},
		inQ:         make(map[string]bool),
		running:     make(map[string]bool),
		concurrency: parseTranslateConcurrency(),
		kick:        make(chan struct{}, 1),
	}
	_ = s.loadConfig()
	_ = s.loadStatus()
	return s
}

func (s *translateService) start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	s.recoverJobs()
	go s.loop()
}

// recoverJobs rehydrates queued ids into the in-memory queue after a reload.
// Orphan BabelDOC processes occupy concurrency slots via occupiedSlots()
// and are adopted/skipped by reconcileExternal; we do not spawn a second
// worker for the same paper id.
func (s *translateService) recoverJobs() {
	if s == nil {
		return
	}
	s.reconcileExternal()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	dirty := false
	for id, j := range s.status.Jobs {
		switch j.Status {
		case translateRunning:
			if jobProcessBusy(s.root, id) || s.running[id] {
				continue
			}
			// Dead "running" jobs are finalized by reconcileExternal.
		case translateQueued:
			if s.inQ[id] || s.running[id] {
				continue
			}
			if jobProcessBusy(s.root, id) {
				// OS worker still running — treat as running, do not re-enqueue.
				j.Status = translateRunning
				j.Error = ""
				j.UpdatedAt = now
				s.status.Jobs[id] = j
				dirty = true
				continue
			}
			s.pushQueuedLocked(id, j.PageCount)
		}
	}
	if dirty {
		s.status.UpdatedAt = now
		s.status.Version = 1
		if err := s.persistStatusLocked(); err != nil {
			log.Printf("papers translate-status write: %v", err)
		}
	}
	if len(s.fastQ)+len(s.slowQ) > 0 {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

func (s *Server) translate() *translateService {
	ps := s.papers()
	if ps == nil {
		return nil
	}
	return ps.translate()
}

func (ps *papersStore) translate() *translateService {
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.xlate == nil {
		ps.xlate = newTranslateService(ps.root)
		ps.xlate.start()
	}
	return ps.xlate
}

func translateConfigPath(root string) string {
	return filepath.Join(root, translateConfigName)
}

func translateStatusPath(root string) string {
	return filepath.Join(root, translateStatusName)
}

func (s *translateService) loadConfig() error {
	if s == nil {
		return nil
	}
	b, err := os.ReadFile(translateConfigPath(s.root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var fc translateFileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return err
	}
	if fc.Version == 0 {
		fc.Version = 1
	}
	if fc.QPS <= 0 {
		fc.QPS = translateDefaultQPS
	}
	s.mu.Lock()
	s.cfg = fc
	s.mu.Unlock()
	log.Printf("papers translate-config loaded path=%s key=%s model=%s auto=%v",
		translateConfigPath(s.root), boolWord(strings.TrimSpace(fc.APIKey) != ""), strings.TrimSpace(fc.Model), fc.AutoTranslate)
	return nil
}

func (s *translateService) loadStatus() error {
	if s == nil {
		return nil
	}
	b, err := os.ReadFile(translateStatusPath(s.root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st translateStatusFile
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Jobs == nil {
		st.Jobs = map[string]translateJob{}
	}
	if st.Version == 0 {
		st.Version = 1
	}
	// Do not rewrite running→failed here. start()/reconcileExternal decide
	// based on whether an OS worker is still alive, so a service reload does
	// not spawn a duplicate BabelDOC for the same paper.
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
	return nil
}

func (s *translateService) snapshot() translateSnapshot {
	if s == nil {
		return translateSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return translateSnapshot{
		BaseURL:       strings.TrimSpace(s.cfg.BaseURL),
		APIKey:        s.cfg.APIKey,
		Model:         strings.TrimSpace(s.cfg.Model),
		QPS:           s.cfg.QPS,
		AutoTranslate: s.cfg.AutoTranslate,
	}
}

func (s *translateService) public() TranslatePublicView {
	snap := s.snapshot()
	qps := snap.QPS
	if qps <= 0 {
		qps = translateDefaultQPS
	}
	path := ""
	if s != nil {
		path = translateConfigPath(s.root)
	}
	return TranslatePublicView{
		OK:            true,
		ConfigPath:    path,
		BaseURL:       snap.BaseURL,
		Model:         snap.Model,
		QPS:           qps,
		AutoTranslate: snap.AutoTranslate,
		APIKey:        maskAPIKey(snap.APIKey),
		BabelDOC:      babeldocOnPath(),
	}
}

func (s *translateService) applyPatch(p translateConfigPatch) error {
	if s == nil {
		return fmt.Errorf("translate config is not available")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.BaseURL != nil {
		s.cfg.BaseURL = strings.TrimSpace(*p.BaseURL)
	}
	if p.ClearAPIKey {
		s.cfg.APIKey = ""
	} else if p.APIKey != nil {
		if v := strings.TrimSpace(*p.APIKey); v != "" {
			s.cfg.APIKey = v
		}
	}
	if p.Model != nil {
		if v := strings.TrimSpace(*p.Model); v != "" {
			s.cfg.Model = v
		}
	}
	if p.QPS != nil {
		q := *p.QPS
		if q < 0 {
			q = 0
		}
		if q > 64 {
			q = 64
		}
		if q == 0 {
			q = translateDefaultQPS
		}
		s.cfg.QPS = q
	}
	if p.AutoTranslate != nil {
		s.cfg.AutoTranslate = *p.AutoTranslate
	}
	if s.cfg.Version == 0 {
		s.cfg.Version = 1
	}
	if strings.TrimSpace(s.cfg.Model) == "" {
		s.cfg.Model = "gpt-4o-mini"
	}
	if s.cfg.QPS <= 0 {
		s.cfg.QPS = translateDefaultQPS
	}
	if err := s.persistConfigLocked(); err != nil {
		return err
	}
	log.Printf("papers translate-config saved path=%s key=%s model=%s auto=%v",
		translateConfigPath(s.root), boolWord(s.cfg.APIKey != ""), s.cfg.Model, s.cfg.AutoTranslate)
	return nil
}

func (s *translateService) persistConfigLocked() error {
	s.cfg.Version = 1
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(translateConfigPath(s.root), append(b, '\n'), translateConfigMode)
}

func (s *translateService) jobsCopy() map[string]translateJob {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]translateJob, len(s.status.Jobs))
	for k, v := range s.status.Jobs {
		out[k] = v
	}
	return out
}

func (s *translateService) job(id string) (translateJob, bool) {
	id = sanitizePaperID(id)
	if s == nil || id == "" {
		return translateJob{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.status.Jobs[id]
	return j, ok
}

func (s *translateService) putJob(j translateJob) {
	if s == nil || j.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Jobs == nil {
		s.status.Jobs = map[string]translateJob{}
	}
	j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.status.Jobs[j.ID] = j
	s.status.UpdatedAt = j.UpdatedAt
	s.status.Version = 1
	if err := s.persistStatusLocked(); err != nil {
		log.Printf("papers translate-status write: %v", err)
	}
}

func (s *translateService) persistStatusLocked() error {
	b, err := json.MarshalIndent(s.status, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(translateStatusPath(s.root), append(b, '\n'), 0o644)
}

func summarizeTranslateProgress(papers []paperEntry) *translateProgress {
	p := &translateProgress{}
	for _, paper := range papers {
		switch paper.TranslateStatus {
		case translateQueued:
			p.Queued++
			switch paperTranslateLane(paper.PageCount) {
			case translateLaneSlow:
				p.SlowQueued++
			case translateLaneFast:
				p.FastQueued++
			}
		case translateRunning:
			p.Running++
			switch paperTranslateLane(paper.PageCount) {
			case translateLaneSlow:
				p.SlowRunning++
			case translateLaneFast:
				p.FastRunning++
			}
			id := paper.ID
			title := strings.TrimSpace(paper.Title)
			if id != "" {
				p.RunningIDs = append(p.RunningIDs, id)
			}
			if title != "" {
				p.RunningTitles = append(p.RunningTitles, title)
			} else if id != "" {
				p.RunningTitles = append(p.RunningTitles, id)
			}
			if p.RunningID == "" {
				p.RunningID = id
				p.RunningTitle = title
			}
		}
	}
	p.Active = p.Queued > 0 || p.Running > 0
	return p
}

func overlayTranslations(papers []paperEntry, root string, jobs map[string]translateJob) {
	for i := range papers {
		papers[i] = overlayOne(papers[i], root, jobs)
	}
}

func overlayOne(p paperEntry, root string, jobs map[string]translateJob) paperEntry {
	id := p.ID
	if id == "" {
		id = paperTranslateID(p)
		p.ID = id
	}
	if id == "" {
		return p
	}
	if _, err := translatedPDFAbs(root, translateKindZH, id); err == nil {
		p.ZhPDF = translatedPDFURL(translateKindZH, id)
	}
	if _, err := translatedPDFAbs(root, translateKindDual, id); err == nil {
		p.DualPDF = translatedPDFURL(translateKindDual, id)
	}
	if j, ok := jobs[id]; ok {
		p.TranslateStatus = j.Status
		if j.Status == translateFailed {
			p.TranslateError = sanitizeUserError(j.Error)
		} else {
			p.TranslateError = ""
		}
	}
	// On-disk outputs mean done only when not actively queued/running (force re-translate).
	if p.ZhPDF != "" && p.DualPDF != "" {
		switch p.TranslateStatus {
		case translateQueued, translateRunning:
			// keep in-flight status so the UI shows progress / 再次翻译 is disabled while running
		default:
			p.TranslateStatus = translateDone
			p.TranslateError = ""
		}
	}
	p.TranslateSkipReason = paperTranslateSkipReason(p)
	p.TranslateLane = paperTranslateLane(p.PageCount)
	return p
}

func maskAPIKey(key string) search.MaskedKey {
	key = strings.TrimSpace(key)
	if key == "" {
		return search.MaskedKey{Configured: false}
	}
	return search.MaskedKey{Configured: true, Last4: last4Runes(key)}
}

func last4Runes(s string) string {
	if s == "" {
		return ""
	}
	n := utf8.RuneCountInString(s)
	if n <= 4 {
		return s
	}
	i := 0
	skip := n - 4
	for idx := range s {
		if i == skip {
			return s[idx:]
		}
		i++
	}
	return s
}

func babeldocOnPath() bool {
	_, err := exec.LookPath("babeldoc")
	return err == nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	_ = os.Chmod(path, mode)
	ok = true
	return nil
}

func sanitizeUserError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Redact tokens that look like API keys.
	fields := strings.Fields(s)
	for i, f := range fields {
		if looksLikeSecret(f) {
			fields[i] = "[redacted]"
		}
	}
	out := strings.Join(fields, " ")
	if len(out) > 480 {
		out = out[:480] + "…"
	}
	return out
}

func looksLikeSecret(s string) bool {
	s = strings.Trim(s, `"'`)
	if len(s) < 12 {
		return false
	}
	if strings.HasPrefix(s, "sk-") || strings.HasPrefix(s, "sk_") {
		return true
	}
	if strings.Contains(strings.ToLower(s), "api_key") && len(s) > 20 {
		return true
	}
	// Long base64-ish blobs.
	n := 0
	for _, r := range s {
		if unicodeIsSecretRune(r) {
			n++
		} else {
			return false
		}
	}
	return n >= 24
}

func unicodeIsSecretRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '+' || r == '/' || r == '='
}

func (s *Server) handlePapersTranslateConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeSettings(w, r) {
		return
	}
	svc := s.translate()
	if svc == nil {
		writeErr(w, http.StatusBadGateway, "translate config is not available", search.CodeEngine, nil, "")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, svc.public())
	case http.MethodPut:
		defer r.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body", search.CodeBadRequest, nil, "")
			return
		}
		var patch translateConfigPatch
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &patch); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
				return
			}
		}
		if err := svc.applyPatch(patch); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), search.CodeEngine, nil, "")
			return
		}
		writeJSON(w, http.StatusOK, svc.public())
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *Server) handlePapersTranslateTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if !s.authorizeSettings(w, r) {
		return
	}
	svc := s.translate()
	if svc == nil {
		writeErr(w, http.StatusBadGateway, "translate config is not available", search.CodeEngine, nil, "")
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body", search.CodeBadRequest, nil, "")
		return
	}
	var body struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
			return
		}
	}
	snap := svc.snapshot()
	base := strings.TrimSpace(body.BaseURL)
	if base == "" {
		base = snap.BaseURL
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = snap.APIKey
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = snap.Model
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "api key is not configured", search.CodeUnauthorized, nil, "")
		return
	}
	if base == "" {
		writeErr(w, http.StatusBadRequest, "base_url is required", search.CodeBadRequest, nil, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	via, usedModel, err := svc.pingLLM(ctx, base, key, model)
	if err != nil {
		log.Printf("papers translate-test fail via=%s: %s", via, sanitizeUserError(err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": sanitizeUserError(err.Error()),
			"via":   via,
			"model": usedModel,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"via":   via,
		"model": usedModel,
	})
}

type translateEnqueueReq struct {
	ID    string `json:"id"`
	All   bool   `json:"all"`
	Force bool   `json:"force"` // manual re-translate; ignored for auto / translate-all
}

type translateEnqueueResp struct {
	OK         bool              `json:"ok"`
	Queued     []string          `json:"queued"`
	Skipped    []string          `json:"skipped,omitempty"`
	Rejected   map[string]string `json:"rejected,omitempty"`
	Running    string            `json:"running,omitempty"`
	RunningIDs []string          `json:"running_ids,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func (s *Server) handlePapersTranslate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeSettings(w, r) {
		return
	}
	svc := s.translate()
	if svc == nil {
		writeErr(w, http.StatusBadGateway, "translate queue is not available", search.CodeEngine, nil, "")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, svc.queuePublic())
	case http.MethodPost:
		defer r.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body", search.CodeBadRequest, nil, "")
			return
		}
		var req translateEnqueueReq
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
				return
			}
		}
		cat, err := s.papers().catalog()
		if err != nil {
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		var ids []string
		force := req.Force
		if id := sanitizePaperID(req.ID); id != "" {
			ids = []string{id}
		} else {
			// Translate-all is never a force re-translate of completed papers.
			force = false
			ids = pendingTranslateIDs(cat.Papers, false)
		}
		res := svc.enqueue(ids, cat.Papers, force)
		resp := translateEnqueueResp{
			OK:         true,
			Queued:     res.Queued,
			Skipped:    res.Skipped,
			Rejected:   res.Rejected,
			Running:    svc.runningID(),
			RunningIDs: svc.runningIDs(),
		}
		if len(res.Queued) == 0 && len(res.Rejected) > 0 && (force || hasTranslateSkipReject(res.Rejected)) {
			// Prefer a clear Chinese message when the only target is mid-flight.
			// Page-count rejects use the machine-readable too_many_pages reason.
			for _, reason := range res.Rejected {
				resp.OK = false
				resp.Error = reason
				break
			}
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *translateService) queuePublic() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	fast := append([]string(nil), s.fastQ...)
	slow := append([]string(nil), s.slowQ...)
	pending := append(append([]string{}, fast...), slow...)
	jobs := make(map[string]translateJob, len(s.status.Jobs))
	for k, v := range s.status.Jobs {
		v.Error = sanitizeUserError(v.Error)
		jobs[k] = v
	}
	ids := s.runningIDsLocked()
	first := ""
	if len(ids) > 0 {
		first = ids[0]
	}
	return map[string]any{
		"ok":          true,
		"queued":      pending,
		"fast_queued": fast,
		"slow_queued": slow,
		"running":     first,
		"running_ids": ids,
		"concurrency": s.concurrencyLimit(),
		"jobs":        jobs,
		"babeldoc":    babeldocOnPath(),
	}
}

func (s *translateService) runningID() string {
	ids := s.runningIDs()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (s *translateService) runningIDs() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningIDsLocked()
}

func (s *translateService) runningIDsLocked() []string {
	if s == nil || len(s.running) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *translateService) concurrencyLimit() int {
	if s == nil {
		return translateDefaultConcurrency
	}
	if s.concurrency >= translateMinConcurrency {
		return s.concurrency
	}
	return parseTranslateConcurrency()
}

func parseTranslateConcurrency() int {
	raw := strings.TrimSpace(os.Getenv("PAPERS_TRANSLATE_CONCURRENCY"))
	if raw == "" {
		return translateDefaultConcurrency
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return translateDefaultConcurrency
	}
	if n < translateMinConcurrency {
		return translateMinConcurrency
	}
	if n > translateMaxConcurrency {
		return translateMaxConcurrency
	}
	return n
}

func (s *translateService) AutoTranslate() bool {
	return s != nil && s.snapshot().AutoTranslate
}

func hasTranslateSkipReject(rejected map[string]string) bool {
	for _, reason := range rejected {
		if reason == translateSkipTooManyPages {
			return true
		}
	}
	return false
}

func pendingTranslateIDs(papers []paperEntry, auto bool) []string {
	var ids []string
	for _, p := range papers {
		if !p.HasLocal || p.ID == "" {
			continue
		}
		// Auto-translate never spends a slot on over-long PDFs.
		// Translate-all still includes them so enqueue can return too_many_pages.
		if auto && paperTooManyPages(p) {
			continue
		}
		// Already translated — never auto / bulk-enqueue again.
		if p.TranslateStatus == translateDone || (p.ZhPDF != "" && p.DualPDF != "") {
			continue
		}
		switch p.TranslateStatus {
		case translateQueued, translateRunning:
			continue
		case translateFailed:
			if auto {
				continue
			}
		}
		ids = append(ids, p.ID)
	}
	return ids
}

func (s *translateService) enqueuePending(papers []paperEntry, auto bool) {
	ids := pendingTranslateIDs(papers, auto)
	if len(ids) == 0 {
		return
	}
	// Auto-translate never forces re-translation of done/failed papers.
	s.enqueue(ids, papers, false)
}

func (s *Server) maybeAutoTranslate(papers []paperEntry) {
	svc := s.translate()
	if svc == nil || !svc.AutoTranslate() {
		return
	}
	// Non-blocking: enqueue + worker already running.
	go svc.enqueuePending(papers, true)
}

func (s *translateService) pingLLM(ctx context.Context, baseURL, apiKey, model string) (via, usedModel string, err error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = strings.TrimSpace(model)
	client := s.httpClient()
	if via, used, e := pingOpenAIModels(ctx, client, baseURL, apiKey, model); e == nil {
		return via, used, nil
	} else {
		err = e
		via = "models"
	}
	if via2, used, e := pingOpenAIChat(ctx, client, baseURL, apiKey, model); e == nil {
		return via2, used, nil
	} else if err == nil {
		err = e
		via = "chat"
	}
	return via, model, err
}

func (s *translateService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: 12 * time.Second}
}

func pingOpenAIModels(ctx context.Context, client *http.Client, base, key, model string) (string, string, error) {
	urls := []string{base + "/models"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/models")
	}
	var last error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			last = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			used := pickModelFromList(body, model)
			return "models", used, nil
		}
		last = fmt.Errorf("models HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			continue
		}
		return "models", model, fmt.Errorf("models HTTP %d", resp.StatusCode)
	}
	if last == nil {
		last = fmt.Errorf("models ping failed")
	}
	return "models", model, last
}

func pingOpenAIChat(ctx context.Context, client *http.Client, base, key, model string) (string, string, error) {
	if model == "" {
		model = "gpt-4o-mini"
	}
	urls := []string{base + "/chat/completions"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/chat/completions")
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	var last error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			last = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "chat", model, nil
		}
		last = fmt.Errorf("chat HTTP %d", resp.StatusCode)
	}
	if last == nil {
		last = fmt.Errorf("chat ping failed")
	}
	return "chat", model, last
}

func pickModelFromList(body []byte, want string) string {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		if want != "" {
			return want
		}
		return ""
	}
	if want != "" {
		for _, m := range parsed.Data {
			if m.ID == want {
				return want
			}
		}
	}
	if len(parsed.Data) > 0 && parsed.Data[0].ID != "" {
		if want != "" {
			return want
		}
		return parsed.Data[0].ID
	}
	return want
}
