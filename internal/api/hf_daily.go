package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"search-service/internal/search"
)

const (
	hfDailyDirName       = "hf-daily"
	hfDailyTag           = "hf-daily"
	hfDailyAPITimeout    = 25 * time.Second
	hfDailyPDFTimeout    = 45 * time.Second
	hfDailyRefreshRecent = 30 * time.Minute
	hfDailyDateWindow    = 14
	hfDailyUserAgent     = "RapidSearch-papers/1.0"
	defaultHFDailyAPI    = "https://huggingface.co/api/daily_papers"
)

var (
	arxivIDPattern = regexp.MustCompile(`(?i)^(?:arxiv:)?((?:\d{4}\.\d{4,5})(?:v\d+)?|[a-z\-]+/\d{7})$`)
	dateOnlyPat    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type hfDailyFetchFn func(ctx context.Context, date string) ([]byte, error)
type hfDailyDownloadFn func(ctx context.Context, p paperEntry) (filename string, err error)

type hfDailyFile struct {
	Date      string       `json:"date"`
	FetchedAt string       `json:"fetched_at"`
	Source    string       `json:"source,omitempty"`
	Count     int          `json:"count"`
	Papers    []paperEntry `json:"papers"`
}

type hfDailyDateInfo struct {
	Date     string `json:"date"`
	Count    int    `json:"count,omitempty"`
	HasTrend bool   `json:"has_trend"`
	Cached   bool   `json:"cached"`
}

type hfDailyDatesResp struct {
	OK      bool              `json:"ok"`
	Default string            `json:"default"`
	Dates   []hfDailyDateInfo `json:"dates"`
}

type hfDailyDayResp struct {
	OK                bool               `json:"ok"`
	Date              string             `json:"date"`
	FetchedAt         string             `json:"fetched_at,omitempty"`
	Count             int                `json:"count"`
	Papers            []paperEntry       `json:"papers"`
	HasTrend          bool               `json:"has_trend"`
	CanManage         bool               `json:"can_manage,omitempty"`
	TranslateProgress *translateProgress `json:"translate_progress,omitempty"`
	AbstractZHPending int                `json:"abstract_zh_pending,omitempty"`
	Stale             bool               `json:"stale,omitempty"`
	FromCache         bool               `json:"from_cache,omitempty"`
}

type hfDailyService struct {
	store *papersStore
	root  string

	client     *http.Client
	fetchFn    hfDailyFetchFn
	downloadFn hfDailyDownloadFn
	generateFn hfDailyTrendFn
	now        func() time.Time

	mu         sync.Mutex
	dateLocks  map[string]*sync.Mutex
	dlSeen     map[string]bool
	generating map[string]chan struct{}
}

func newHFDailyService(ps *papersStore) *hfDailyService {
	root := ""
	if ps != nil {
		root = ps.root
	}
	if root == "" {
		root = papersRoot()
	}
	return &hfDailyService{
		store:      ps,
		root:       root,
		client:     &http.Client{Timeout: hfDailyAPITimeout},
		now:        func() time.Time { return time.Now().UTC() },
		dateLocks:  map[string]*sync.Mutex{},
		dlSeen:     map[string]bool{},
		generating: map[string]chan struct{}{},
	}
}

func hfDailyDir(root string) string {
	if root == "" {
		root = papersRoot()
	}
	return filepath.Join(root, hfDailyDirName)
}

func hfDailyFilePath(root, date string) string {
	return filepath.Join(hfDailyDir(root), date+".json")
}

func hfDailyTrendPath(root, date string) string {
	return filepath.Join(hfDailyDir(root), date+".trend.json")
}

func parseHFDailyDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("date is required")
	}
	if !dateOnlyPat.MatchString(s) {
		return "", fmt.Errorf("date must be YYYY-MM-DD")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("invalid date")
	}
	// Reject obviously impossible / far-future dates (clock skew of a day is OK).
	max := time.Now().UTC().Add(36 * time.Hour)
	if t.After(max) {
		return "", fmt.Errorf("date is in the future")
	}
	if t.Year() < 2020 {
		return "", fmt.Errorf("date is too old")
	}
	return t.Format("2006-01-02"), nil
}

func todayUTC(now func() time.Time) string {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return now().UTC().Format("2006-01-02")
}

func (s *hfDailyService) lockDate(date string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dateLocks == nil {
		s.dateLocks = map[string]*sync.Mutex{}
	}
	m, ok := s.dateLocks[date]
	if !ok {
		m = &sync.Mutex{}
		s.dateLocks[date] = m
	}
	return m
}

func (s *hfDailyService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: hfDailyAPITimeout}
}

func (s *hfDailyService) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func hfDailyAPIURL(date string) string {
	base := strings.TrimSpace(os.Getenv("HF_DAILY_API"))
	if base == "" {
		base = defaultHFDailyAPI
	}
	return strings.TrimRight(base, "?&") + "?date=" + date
}

func (s *hfDailyService) fetchRaw(ctx context.Context, date string) ([]byte, error) {
	if s != nil && s.fetchFn != nil {
		return s.fetchFn(ctx, date)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hfDailyAPIURL(date), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", hfDailyUserAgent)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		// HF returns 400/404 when a day has not been published yet.
		return []byte("[]"), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("huggingface daily_papers HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func parseHFDailyPapers(raw []byte, date string) ([]paperEntry, error) {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty huggingface daily_papers body")
	}
	var items []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
	} else {
		var wrap struct {
			Papers json.RawMessage `json:"papers"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			return nil, err
		}
		inner := wrap.Papers
		if len(inner) == 0 {
			inner = wrap.Data
		}
		if len(inner) == 0 || inner[0] != '[' {
			return nil, fmt.Errorf("unexpected huggingface daily_papers shape")
		}
		if err := json.Unmarshal(inner, &items); err != nil {
			return nil, err
		}
	}
	out := make([]paperEntry, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		p, ok := paperFromHFDailyItem(item, date)
		if !ok {
			continue
		}
		id := paperTranslateID(p)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		p.ID = id
		out = append(out, p)
	}
	return out, nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func paperFromHFDailyItem(raw json.RawMessage, date string) (paperEntry, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return paperEntry{}, false
	}
	paperRaw := top["paper"]
	var paper map[string]json.RawMessage
	if len(paperRaw) > 0 && paperRaw[0] == '{' {
		_ = json.Unmarshal(paperRaw, &paper)
	}
	if paper == nil {
		paper = map[string]json.RawMessage{}
	}

	title := firstJSONString(paper["title"], top["title"])
	summary := firstJSONString(paper["summary"], top["summary"], paper["ai_summary"])
	id := firstJSONString(paper["id"], top["id"])
	published := firstJSONString(paper["publishedAt"], top["publishedAt"], paper["submittedOnDailyAt"])
	authors := parseHFAuthors(paper["authors"])
	if len(authors) == 0 {
		authors = parseHFAuthors(top["authors"])
	}

	arxiv := normalizeHFArxivID(id)
	if arxiv == "" {
		arxiv = normalizeHFArxivID(firstJSONString(paper["arxiv_id"], paper["arxivId"]))
	}
	if title == "" && arxiv == "" {
		return paperEntry{}, false
	}

	p := paperEntry{
		Title:     title,
		Authors:   authors,
		Abstract:  summary,
		TopicTags: []string{hfDailyTag},
		Published: published,
		Updated:   date,
	}
	if arxiv != "" {
		p.ArxivID = arxiv
		p.SourceURL = "https://arxiv.org/abs/" + arxiv
		p.PDFURL = "https://arxiv.org/pdf/" + arxiv + ".pdf"
	} else if id != "" {
		// Non-arXiv HF paper: still addressable via Hugging Face.
		p.SourceURL = "https://huggingface.co/papers/" + strings.TrimSpace(id)
	}
	if y := yearFromPublished(published, date); y > 0 {
		p.Year = y
	}
	if n := firstJSONFloat(paper["upvotes"], top["upvotes"]); n > 0 {
		p.Score = n
	}
	p.ID = paperTranslateID(p)
	if p.ID == "" && id != "" {
		p.ID = sanitizePaperID(id)
	}
	if p.ID == "" {
		return paperEntry{}, false
	}
	return p, true
}

func firstJSONString(vals ...json.RawMessage) string {
	for _, v := range vals {
		if len(v) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
	}
	return ""
}

func firstJSONFloat(vals ...json.RawMessage) float64 {
	for _, v := range vals {
		if len(v) == 0 {
			continue
		}
		var n float64
		if json.Unmarshal(v, &n) == nil {
			return n
		}
	}
	return 0
}

func parseHFAuthors(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	out := make([]string, 0, len(arr))
	seen := map[string]bool{}
	for _, item := range arr {
		name := ""
		var s string
		if json.Unmarshal(item, &s) == nil {
			name = strings.TrimSpace(s)
		} else {
			var obj struct {
				Name string `json:"name"`
				User struct {
					Fullname string `json:"fullname"`
					Name     string `json:"name"`
				} `json:"user"`
			}
			if json.Unmarshal(item, &obj) == nil {
				name = strings.TrimSpace(obj.Name)
				if name == "" {
					name = strings.TrimSpace(obj.User.Fullname)
				}
				if name == "" {
					name = strings.TrimSpace(obj.User.Name)
				}
			}
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func normalizeHFArxivID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	id = strings.TrimPrefix(id, "https://arxiv.org/abs/")
	id = strings.TrimPrefix(id, "http://arxiv.org/abs/")
	id = strings.TrimPrefix(id, "https://arxiv.org/pdf/")
	id = strings.TrimPrefix(id, "http://arxiv.org/pdf/")
	id = strings.TrimSuffix(id, ".pdf")
	m := arxivIDPattern.FindStringSubmatch(id)
	if m == nil {
		// Bare new-style arXiv ids sometimes arrive without a prefix.
		if sanitizeArxivID(id) == "" {
			return ""
		}
		// Accept YYYY.NNNNN even if the year is in the 2020s–2090s.
		if regexp.MustCompile(`^\d{4}\.\d{4,5}(?:v\d+)?$`).MatchString(id) {
			return sanitizeArxivID(id)
		}
		return ""
	}
	return sanitizeArxivID(m[1])
}

func yearFromPublished(published, date string) int {
	for _, s := range []string{published, date} {
		s = strings.TrimSpace(s)
		if len(s) >= 4 {
			var y int
			if _, err := fmt.Sscanf(s[:4], "%d", &y); err == nil && y >= 1990 && y <= 2100 {
				return y
			}
		}
	}
	return 0
}

func (s *hfDailyService) loadFile(date string) (hfDailyFile, bool) {
	if s == nil || date == "" {
		return hfDailyFile{}, false
	}
	b, err := os.ReadFile(hfDailyFilePath(s.root, date))
	if err != nil {
		return hfDailyFile{}, false
	}
	var rec hfDailyFile
	if json.Unmarshal(b, &rec) != nil || rec.Date == "" {
		return hfDailyFile{}, false
	}
	return rec, true
}

func (s *hfDailyService) saveFile(rec hfDailyFile) error {
	if s == nil {
		return fmt.Errorf("hf daily store is not available")
	}
	if err := os.MkdirAll(hfDailyDir(s.root), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(hfDailyFilePath(s.root, rec.Date), append(b, '\n'), 0o644)
}

func (s *hfDailyService) cacheFresh(rec hfDailyFile, date string) bool {
	if rec.Date != date {
		return false
	}
	fetched, err := time.Parse(time.RFC3339, rec.FetchedAt)
	if err != nil {
		return false
	}
	today := todayUTC(s.now)
	if date < today {
		// Historical day: once fetched, keep it.
		return true
	}
	return s.clock().Sub(fetched) < hfDailyRefreshRecent
}

func (s *hfDailyService) ensureDay(ctx context.Context, date string) (hfDailyFile, error) {
	rec, _, err := s.dayForGET(ctx, date)
	return rec, err
}

// dayForGET is cache-first: a local day file is returned immediately and a
// stale day is refreshed in the background. A missing day may do one short
// upstream fetch (tests + first visit). GET never 503s when a cache exists.
func (s *hfDailyService) dayForGET(ctx context.Context, date string) (hfDailyFile, bool, error) {
	if rec, ok := s.loadFile(date); ok {
		stale := !s.cacheFresh(rec, date)
		if stale {
			s.refreshDayAsync(date)
		} else {
			s.kickDownloads(rec.Papers)
		}
		return rec, stale, nil
	}
	rec, err := s.fetchAndStore(ctx, date)
	if err != nil {
		log.Printf("papers hf-daily date=%s: %v (empty list)", date, err)
		return hfDailyFile{Date: date, FetchedAt: s.clock().Format(time.RFC3339)}, false, nil
	}
	return rec, false, nil
}

func (s *hfDailyService) refreshDayAsync(date string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), hfDailyAPITimeout)
		defer cancel()
		if _, err := s.fetchAndStore(ctx, date); err != nil {
			log.Printf("papers hf-daily background date=%s: %v", date, err)
		}
	}()
}

func (s *hfDailyService) fetchAndStore(ctx context.Context, date string) (hfDailyFile, error) {
	m := s.lockDate(date)
	m.Lock()
	defer m.Unlock()

	if rec, ok := s.loadFile(date); ok && s.cacheFresh(rec, date) {
		s.kickDownloads(rec.Papers)
		return rec, nil
	}

	raw, err := s.fetchRaw(ctx, date)
	if err != nil {
		if rec, ok := s.loadFile(date); ok {
			log.Printf("papers hf-daily fetch date=%s: %v (using cache)", date, err)
			return rec, nil
		}
		return hfDailyFile{}, err
	}
	papers, err := parseHFDailyPapers(raw, date)
	if err != nil {
		if rec, ok := s.loadFile(date); ok {
			log.Printf("papers hf-daily parse date=%s: %v (using cache)", date, err)
			return rec, nil
		}
		return hfDailyFile{}, err
	}
	rec := hfDailyFile{
		Date:      date,
		FetchedAt: s.clock().Format(time.RFC3339),
		Source:    hfDailyAPIURL(date),
		Count:     len(papers),
		Papers:    papers,
	}
	if err := s.saveFile(rec); err != nil {
		return hfDailyFile{}, err
	}
	s.mergeIntoCatalog(papers)
	s.kickDownloads(papers)
	return rec, nil
}

func paperCatalogKey(p paperEntry) string {
	if id := sanitizeArxivID(p.ArxivID); id != "" {
		return "arxiv:" + strings.ToLower(id)
	}
	if id := paperTranslateID(p); id != "" {
		return "id:" + strings.ToLower(id)
	}
	return "title:" + strings.ToLower(strings.TrimSpace(p.Title))
}

func ensureHFDailyTag(tags []string) []string {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), hfDailyTag) {
			if len(tags) == 0 {
				return []string{hfDailyTag}
			}
			return tags
		}
	}
	return append(append([]string{}, tags...), hfDailyTag)
}

func (s *hfDailyService) mergeIntoCatalog(incoming []paperEntry) {
	if s == nil || s.store == nil || len(incoming) == 0 {
		return
	}
	ps := s.store
	ps.mu.Lock()
	defer ps.mu.Unlock()

	man, err := readPapersManifest(ps.root)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("papers hf-daily manifest read: %v", err)
		return
	}
	byKey := make(map[string]int, len(man.Papers))
	for i, p := range man.Papers {
		byKey[paperCatalogKey(p)] = i
	}
	changed := false
	for _, in := range incoming {
		key := paperCatalogKey(in)
		if i, ok := byKey[key]; ok {
			if !hasTag(man.Papers[i].TopicTags, hfDailyTag) {
				man.Papers[i].TopicTags = ensureHFDailyTag(man.Papers[i].TopicTags)
				changed = true
			}
			// Fill missing bibliographic fields from HF without clobbering curated ones.
			if strings.TrimSpace(man.Papers[i].Abstract) == "" && in.Abstract != "" {
				man.Papers[i].Abstract = in.Abstract
				changed = true
			}
			if strings.TrimSpace(man.Papers[i].SourceURL) == "" && in.SourceURL != "" {
				man.Papers[i].SourceURL = in.SourceURL
				changed = true
			}
			if strings.TrimSpace(man.Papers[i].PDFURL) == "" && in.PDFURL != "" {
				man.Papers[i].PDFURL = in.PDFURL
				changed = true
			}
			if strings.TrimSpace(man.Papers[i].Published) == "" && in.Published != "" {
				man.Papers[i].Published = in.Published
				changed = true
			}
			continue
		}
		man.Papers = append(man.Papers, in)
		byKey[key] = len(man.Papers) - 1
		changed = true
	}
	if !changed && man.GeneratedAt != "" {
		return
	}
	man.GeneratedAt = s.clock().Format(time.RFC3339)
	if man.Stats == nil {
		man.Stats = map[string]interface{}{}
	}
	man.Stats["hf_daily"] = true
	if err := writePapersManifest(ps.root, man); err != nil {
		log.Printf("papers hf-daily manifest write: %v", err)
		return
	}
	ps.loaded = time.Time{}
	ps.modTime = time.Time{}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

func readPapersManifest(root string) (papersManifest, error) {
	var man papersManifest
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return man, err
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return man, err
	}
	return man, nil
}

func writePapersManifest(root string, man papersManifest) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(root, "manifest.json"), append(b, '\n'), 0o644)
}

func (s *hfDailyService) resolveDayPapers(rec hfDailyFile) []paperEntry {
	// Fast path for GET: decorate the cached day rows. Do not rebuild the
	// full catalog (that used to block the public tunnel on every day load).
	pdfDir := ""
	if s != nil && s.root != "" {
		pdfDir = filepath.Join(s.root, "pdfs")
	}
	bySnap := map[string]paperEntry{}
	if s != nil && s.store != nil {
		if snap, _, ok := s.store.cachedCatalogSnapshot(); ok {
			for _, p := range snap.Papers {
				id := p.ID
				if id == "" {
					id = paperTranslateID(p)
				}
				if id != "" {
					bySnap[id] = p
				}
			}
		}
	}
	out := make([]paperEntry, 0, len(rec.Papers))
	seen := map[string]bool{}
	for _, p := range rec.Papers {
		id := p.ID
		if id == "" {
			id = paperTranslateID(p)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if hit, ok := bySnap[id]; ok {
			p = hit
		} else if pdfDir != "" {
			p = enrichPaper(p, pdfDir)
		}
		p = leanPaperForSnapshot(p)
		out = append(out, p)
	}
	if s != nil && s.store != nil {
		s.store.decoratePapers(out)
	}
	return out
}

func (s *hfDailyService) findCached(id string) (paperEntry, bool) {
	id = sanitizePaperID(id)
	if s == nil || id == "" {
		return paperEntry{}, false
	}
	ents, err := os.ReadDir(hfDailyDir(s.root))
	if err != nil {
		return paperEntry{}, false
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".trend.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(hfDailyDir(s.root), name))
		if err != nil {
			continue
		}
		var rec hfDailyFile
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		for _, p := range rec.Papers {
			pid := p.ID
			if pid == "" {
				pid = paperTranslateID(p)
			}
			if sanitizePaperID(pid) == id {
				return p, true
			}
		}
	}
	return paperEntry{}, false
}

func (s *hfDailyService) listDates() []hfDailyDateInfo {
	cached := map[string]hfDailyDateInfo{}
	if ents, err := os.ReadDir(hfDailyDir(s.root)); err == nil {
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".trend.json") {
				continue
			}
			date := strings.TrimSuffix(name, ".json")
			if _, err := parseHFDailyDate(date); err != nil {
				continue
			}
			info := hfDailyDateInfo{Date: date, Cached: true}
			if rec, ok := s.loadFile(date); ok {
				info.Count = rec.Count
				if info.Count == 0 {
					info.Count = len(rec.Papers)
				}
			}
			if _, err := os.Stat(hfDailyTrendPath(s.root, date)); err == nil {
				info.HasTrend = true
			}
			cached[date] = info
		}
	}

	today := todayUTC(s.now)
	start, _ := time.Parse("2006-01-02", today)
	seen := map[string]bool{}
	out := make([]hfDailyDateInfo, 0, hfDailyDateWindow+len(cached))
	for i := 0; i < hfDailyDateWindow; i++ {
		d := start.AddDate(0, 0, -i).Format("2006-01-02")
		seen[d] = true
		if info, ok := cached[d]; ok {
			out = append(out, info)
			continue
		}
		out = append(out, hfDailyDateInfo{Date: d, Cached: false})
	}
	extra := make([]string, 0, len(cached))
	for d := range cached {
		if !seen[d] {
			extra = append(extra, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(extra)))
	for _, d := range extra {
		out = append(out, cached[d])
	}
	return out
}

func (s *hfDailyService) defaultDate(dates []hfDailyDateInfo) string {
	today := todayUTC(s.now)
	for _, d := range dates {
		if d.Date == today && d.Count > 0 {
			return today
		}
	}
	for _, d := range dates {
		if d.Count > 0 {
			return d.Date
		}
	}
	return today
}

func (s *hfDailyService) kickDownloads(papers []paperEntry) {
	if s == nil {
		return
	}
	for _, p := range papers {
		id := p.ID
		if id == "" {
			id = paperTranslateID(p)
		}
		if id == "" || strings.TrimSpace(p.PDFURL) == "" && p.ArxivID == "" {
			continue
		}
		s.mu.Lock()
		if s.dlSeen == nil {
			s.dlSeen = map[string]bool{}
		}
		if s.dlSeen[id] {
			s.mu.Unlock()
			continue
		}
		s.dlSeen[id] = true
		s.mu.Unlock()
		go s.downloadOne(p)
	}
}

func (s *hfDailyService) downloadOne(p paperEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), hfDailyPDFTimeout)
	defer cancel()
	name, err := s.downloadPDF(ctx, p)
	if err != nil {
		log.Printf("papers hf-daily pdf id=%s: %v", paperTranslateID(p), err)
		return
	}
	if name == "" {
		return
	}
	if s.store != nil {
		s.store.invalidateCatalog()
	}
}

func hfDailyPDFName(p paperEntry) string {
	id := sanitizeArxivID(p.ArxivID)
	if id == "" {
		id = paperTranslateID(p)
	}
	id = strings.ReplaceAll(id, "/", "_")
	if id == "" {
		return ""
	}
	slug := slugTitle(p.Title, 40)
	if slug != "" {
		return id + "_" + slug + ".pdf"
	}
	return id + ".pdf"
}

func (s *hfDailyService) downloadPDF(ctx context.Context, p paperEntry) (string, error) {
	if s != nil && s.downloadFn != nil {
		return s.downloadFn(ctx, p)
	}
	root := s.root
	if root == "" {
		root = papersRoot()
	}
	pdfDir := filepath.Join(root, "pdfs")
	if hit := findPDFByArxiv(pdfDir, p.ArxivID); hit != "" {
		return hit, nil
	}
	name := hfDailyPDFName(p)
	if name == "" {
		return "", nil
	}
	dest := filepath.Join(pdfDir, name)
	if st, err := os.Stat(dest); err == nil && !st.IsDir() && st.Size() > 1000 {
		return name, nil
	}
	url := strings.TrimSpace(p.PDFURL)
	if url == "" && p.ArxivID != "" {
		url = "https://arxiv.org/pdf/" + sanitizeArxivID(p.ArxivID) + ".pdf"
	}
	if url == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", hfDailyUserAgent)
	req.Header.Set("Accept", "application/pdf,*/*")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("pdf HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 80<<20))
	if err != nil {
		return "", err
	}
	if len(body) < 5 || string(body[:4]) != "%PDF" {
		return "", fmt.Errorf("response is not a PDF")
	}
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		return "", err
	}
	if err := atomicWriteFile(dest, body, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Server) handleHFDailyDates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	daily := s.papers().daily
	if daily == nil {
		writeErr(w, http.StatusBadGateway, "hf daily store is not available", "papers", nil, "")
		return
	}
	dates := daily.listDates()
	out := hfDailyDatesResp{
		OK:      true,
		Default: daily.defaultDate(dates),
		Dates:   dates,
	}
	serveCachedValue(w, r, out, dailyDatesETag(out))
}

func dailyDatesETag(out hfDailyDatesResp) string {
	var b strings.Builder
	b.WriteString(out.Default)
	for _, d := range out.Dates {
		b.WriteByte('|')
		b.WriteString(d.Date)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(d.Count))
		if d.HasTrend {
			b.WriteByte('t')
		}
		if d.Cached {
			b.WriteByte('c')
		}
	}
	return snapshotETag([]byte(b.String()))
}

func (s *Server) handleHFDailyDay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	date := strings.TrimSpace(r.PathValue("date"))
	if date == "" {
		date = strings.TrimSpace(r.URL.Query().Get("date"))
	}
	if date == "" {
		date = todayUTC(nil)
	}
	parsed, err := parseHFDailyDate(date)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
		return
	}
	daily := s.papers().daily
	if daily == nil {
		writeErr(w, http.StatusBadGateway, "hf daily store is not available", "papers", nil, "")
		return
	}
	_, fromCache := daily.loadFile(parsed)
	ctx, cancel := context.WithTimeout(r.Context(), hfDailyAPITimeout)
	defer cancel()
	rec, stale, err := daily.dayForGET(ctx, parsed)
	if err != nil {
		log.Printf("papers hf-daily date=%s: %v", parsed, err)
		rec = hfDailyFile{Date: parsed}
	}
	papers := daily.resolveDayPapers(rec)
	_, hasTrend := daily.loadTrend(parsed)
	out := hfDailyDayResp{
		OK:        true,
		Date:      parsed,
		FetchedAt: rec.FetchedAt,
		Count:     len(papers),
		Papers:    papers,
		HasTrend:  hasTrend,
		Stale:     stale,
		FromCache: fromCache,
	}
	etag := snapshotETag([]byte(parsed + "|" + rec.FetchedAt + "|" + strconv.Itoa(len(papers))))
	serveCachedValue(w, r, out, etag)
	go s.kickPapersSideEffects(s.settingsAuthed(r), papers)
}
