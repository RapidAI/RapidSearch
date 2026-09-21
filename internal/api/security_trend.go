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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"search-service/internal/search"
)

const (
	securityTopTag             = "security-top"
	securityTrendDirName       = "security-trends"
	securityTopYearFloor       = 2022
	securityTrendHTTPTimeout   = 120 * time.Second
	securityTrendMaxTokens     = 4096
	securityTrendMaxPapers     = 80
	securityTrendAbstractRunes = 500
	securityTrendOtherSlug     = "other"
	securityTrendOtherLabel    = "Other venue"
	// Background auto-generate: one venue×year at a time so we do not stampede Hub.
	securityTrendAutoInterval = 20 * time.Minute
	securityTrendAutoGap      = 3 * time.Second
)

type securityVenueInfo struct {
	Slug  string
	Label string
}

var securityVenues = []securityVenueInfo{
	{Slug: "ieee-sp", Label: "IEEE S&P / Oakland"},
	{Slug: "acm-ccs", Label: "ACM CCS"},
	{Slug: "usenix-security", Label: "USENIX Security"},
	{Slug: "ndss", Label: "NDSS"},
}

// venueExact maps a canonical (lowercased, punctuation-stripped) venue
// string to a Big-4 slug. Short keys are exact-match only.
var securityVenueExact = map[string]string{
	"ieee s and p oakland":                   "ieee-sp",
	"ieee s and p":                           "ieee-sp",
	"ieee sp oakland":                        "ieee-sp",
	"ieee sp":                                "ieee-sp",
	"s and p oakland":                        "ieee-sp",
	"s and p":                                "ieee-sp",
	"oakland":                                "ieee-sp",
	"sp":                                     "ieee-sp",
	"ieee symposium on security and privacy": "ieee-sp",
	"ieee security and privacy":              "ieee-sp",
	"acm ccs":                                "acm-ccs",
	"ccs":                                    "acm-ccs",
	"acm conference on computer and communications security": "acm-ccs",
	"computer and communications security":                   "acm-ccs",
	"usenix security":                                        "usenix-security",
	"usenix sec":                                             "usenix-security",
	"usenix":                                                 "usenix-security",
	"usenix security symposium":                              "usenix-security",
	"ndss":                                                   "ndss",
	"network and distributed system security":                "ndss",
	"network and distributed systems security":               "ndss",
}

type securityTrendFn func(ctx context.Context, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error)

type securityTrend struct {
	Venue       string   `json:"venue"`
	Slug        string   `json:"slug"`
	Year        int      `json:"year"`
	Headline    string   `json:"headline"`
	Overview    string   `json:"overview"`
	Themes      []string `json:"themes,omitempty"`
	Highlights  []string `json:"highlights,omitempty"`
	Outlook     string   `json:"outlook,omitempty"`
	Model       string   `json:"model,omitempty"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	PaperCount  int      `json:"paper_count,omitempty"`
}

func (t securityTrend) any() bool {
	return strings.TrimSpace(t.Headline) != "" ||
		strings.TrimSpace(t.Overview) != "" ||
		len(t.Themes) > 0 ||
		len(t.Highlights) > 0 ||
		strings.TrimSpace(t.Outlook) != ""
}

type securityTrendView struct {
	OK          bool     `json:"ok"`
	Venue       string   `json:"venue,omitempty"`
	Slug        string   `json:"slug,omitempty"`
	Year        int      `json:"year,omitempty"`
	Headline    string   `json:"headline,omitempty"`
	Overview    string   `json:"overview,omitempty"`
	Themes      []string `json:"themes,omitempty"`
	Highlights  []string `json:"highlights,omitempty"`
	Outlook     string   `json:"outlook,omitempty"`
	Model       string   `json:"model,omitempty"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	PaperCount  int      `json:"paper_count,omitempty"`
	HasTrend    bool     `json:"has_trend"`
	Skipped     bool     `json:"skipped,omitempty"`
}

func (t securityTrend) public(skipped bool) securityTrendView {
	return securityTrendView{
		OK:          true,
		Venue:       t.Venue,
		Slug:        t.Slug,
		Year:        t.Year,
		Headline:    t.Headline,
		Overview:    t.Overview,
		Themes:      t.Themes,
		Highlights:  t.Highlights,
		Outlook:     t.Outlook,
		Model:       t.Model,
		GeneratedAt: t.GeneratedAt,
		PaperCount:  t.PaperCount,
		HasTrend:    t.any(),
		Skipped:     skipped,
	}
}

type securityYearInfo struct {
	Year     int  `json:"year"`
	Count    int  `json:"count"`
	HasTrend bool `json:"has_trend"`
}

type securityVenueYearsResp struct {
	OK          bool               `json:"ok"`
	Venue       string             `json:"venue"`
	Slug        string             `json:"slug"`
	DefaultYear int                `json:"default_year"`
	Years       []securityYearInfo `json:"years"`
}

type securityTrendIndexResp struct {
	OK     bool                     `json:"ok"`
	Venues []securityVenueYearsResp `json:"venues"`
}

type securityTrendService struct {
	store      *papersStore
	root       string
	generateFn securityTrendFn
	now        func() time.Time
	autoGap    time.Duration // default securityTrendAutoGap; tests may set 0

	mu         sync.Mutex
	generating map[string]chan struct{}
	kick       chan struct{}
	stopCh     chan struct{}
	started    bool
	stopped    bool
}

func newSecurityTrendService(ps *papersStore) *securityTrendService {
	root := ""
	if ps != nil {
		root = ps.root
	}
	if root == "" {
		root = papersRoot()
	}
	return &securityTrendService{
		store:      ps,
		root:       root,
		now:        func() time.Time { return time.Now().UTC() },
		autoGap:    securityTrendAutoGap,
		generating: map[string]chan struct{}{},
		kick:       make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}
}

// start launches the background worker that fills missing venue×year trend
// caches. Viewing cached trends is public; this path is the only unauthenticated
// generator (POST /papers/security-trend stays admin force-regenerate).
func (s *securityTrendService) start() {
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
	go s.loop()
	s.ensureMissing()
}

func (s *securityTrendService) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)
	s.mu.Unlock()
}

// ensureMissing kicks a scan for Big-4 venue×year groups that have papers but
// no valid trend JSON. Safe for anonymous catalog traffic; does not block.
func (s *securityTrendService) ensureMissing() {
	if s == nil || s.kick == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *securityTrendService) loop() {
	ticker := time.NewTicker(securityTrendAutoInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.kick:
			s.generateMissing()
		case <-ticker.C:
			s.generateMissing()
		}
	}
}

func (s *securityTrendService) gap() time.Duration {
	if s == nil {
		return securityTrendAutoGap
	}
	return s.autoGap
}

// missingTrendGroups returns Big-4 venue×year buckets that have papers and no
// valid cached trend. Empty groups and "other" venues are skipped.
func (s *securityTrendService) missingTrendGroups(papers []paperEntry) []securityPaperGroup {
	if s == nil {
		return nil
	}
	out := make([]securityPaperGroup, 0)
	for _, g := range groupSecurityPapers(papers) {
		if !securityVenueSlugOK(g.Slug) || g.Year <= 0 || len(g.Papers) == 0 {
			continue
		}
		if rec, ok := s.loadTrend(g.Slug, g.Year); ok && rec.any() {
			continue
		}
		out = append(out, g)
	}
	return out
}

// generateMissing fills missing security-top venue×year trend caches one at a
// time using the configured Hub/OpenAI-compatible review LLM. No HTTP auth.
func (s *securityTrendService) generateMissing() {
	if s == nil {
		return
	}
	var snap translateSnapshot
	if s.store != nil && s.store.xlate != nil {
		snap = s.store.xlate.snapshot()
	}
	if !snap.ready() {
		return
	}
	papers, err := s.securityTopPapers()
	if err != nil {
		log.Printf("papers security-trend auto catalog: %v", err)
		return
	}
	groups := s.missingTrendGroups(papers)
	gap := s.gap()
	for i, g := range groups {
		if rec, ok := s.loadTrend(g.Slug, g.Year); ok && rec.any() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), securityTrendHTTPTimeout)
		_, _, err := s.generateTrend(ctx, g.Slug, g.Year, g.Papers, false)
		cancel()
		if err != nil {
			log.Printf("papers security-trend auto slug=%s year=%d: %v", g.Slug, g.Year, err)
		}
		if gap > 0 && i < len(groups)-1 {
			select {
			case <-s.stopCh:
				return
			case <-time.After(gap):
			}
		}
	}
}

func securityTrendDir(root string) string {
	if root == "" {
		root = papersRoot()
	}
	return filepath.Join(root, securityTrendDirName)
}

func securityVenueSlugOK(slug string) bool {
	slug = strings.TrimSpace(slug)
	if slug == "" || slug == securityTrendOtherSlug {
		return false
	}
	for _, v := range securityVenues {
		if v.Slug == slug {
			return true
		}
	}
	return false
}

func securityVenueLabel(slug string) string {
	for _, v := range securityVenues {
		if v.Slug == slug {
			return v.Label
		}
	}
	if slug == securityTrendOtherSlug {
		return securityTrendOtherLabel
	}
	return slug
}

func canonVenueKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevSpace := true
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString(" and ")
			prevSpace = true
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		default:
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func stripYearTokens(s string) string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if y, err := strconv.Atoi(f); err == nil && y >= 1990 && y <= 2100 {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// normalizeSecurityVenue maps a free-form venue string to a Big-4 label+slug.
// Unknown names become "Other venue" / "other".
func normalizeSecurityVenue(raw string) (label, slug string) {
	canon := stripYearTokens(canonVenueKey(raw))
	if canon == "" {
		return securityTrendOtherLabel, securityTrendOtherSlug
	}
	if hit, ok := securityVenueExact[canon]; ok {
		return securityVenueLabel(hit), hit
	}
	bestSlug := ""
	bestLen := 0
	for alias, hit := range securityVenueExact {
		if len(alias) < 8 {
			continue
		}
		if strings.Contains(canon, alias) && len(alias) > bestLen {
			bestLen = len(alias)
			bestSlug = hit
		}
	}
	if bestSlug != "" {
		return securityVenueLabel(bestSlug), bestSlug
	}
	return securityTrendOtherLabel, securityTrendOtherSlug
}

func paperVenueRaw(p paperEntry) string {
	return strings.TrimSpace(p.Venue)
}

func paperSecuritySlug(p paperEntry) string {
	_, slug := normalizeSecurityVenue(paperVenueRaw(p))
	return slug
}

func paperYear(p paperEntry) int {
	if p.Year > 0 {
		return p.Year
	}
	s := strings.TrimSpace(p.Published)
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil && y >= 1990 && y <= 2100 {
			return y
		}
	}
	return 0
}

func securityTrendKey(slug string, year int) string {
	return slug + "-" + strconv.Itoa(year)
}

func securityTrendPath(root, slug string, year int) string {
	slug = sanitizeSecuritySlug(slug)
	if slug == "" {
		slug = securityTrendOtherSlug
	}
	return filepath.Join(securityTrendDir(root), securityTrendKey(slug, year)+".json")
}

func sanitizeSecuritySlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	var b strings.Builder
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseSecurityYear(s string) (int, error) {
	s = strings.TrimSpace(s)
	y, err := strconv.Atoi(s)
	if err != nil || y < 0 || y > 2100 {
		return 0, fmt.Errorf("invalid year")
	}
	return y, nil
}

func resolveSecurityVenue(raw string) (label, slug string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("venue is required")
	}
	clean := sanitizeSecuritySlug(raw)
	if securityVenueSlugOK(clean) {
		return securityVenueLabel(clean), clean, nil
	}
	label, slug = normalizeSecurityVenue(raw)
	if !securityVenueSlugOK(slug) {
		return "", "", fmt.Errorf("unknown security venue")
	}
	return label, slug, nil
}

type securityPaperGroup struct {
	Venue  string
	Slug   string
	Year   int
	Papers []paperEntry
}

func groupSecurityPapers(papers []paperEntry) []securityPaperGroup {
	type key struct {
		slug string
		year int
	}
	buckets := map[key][]paperEntry{}
	labels := map[string]string{}
	for _, p := range papers {
		label, slug := normalizeSecurityVenue(paperVenueRaw(p))
		labels[slug] = label
		k := key{slug: slug, year: paperYear(p)}
		buckets[k] = append(buckets[k], p)
	}
	order := make([]string, 0, len(securityVenues)+1)
	for _, v := range securityVenues {
		order = append(order, v.Slug)
	}
	order = append(order, securityTrendOtherSlug)
	keys := make([]key, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		oi := indexOfSlug(order, keys[i].slug)
		oj := indexOfSlug(order, keys[j].slug)
		if oi != oj {
			return oi < oj
		}
		return keys[i].year > keys[j].year
	})
	out := make([]securityPaperGroup, 0, len(keys))
	for _, k := range keys {
		label := labels[k.slug]
		if label == "" {
			label = securityVenueLabel(k.slug)
		}
		out = append(out, securityPaperGroup{
			Venue:  label,
			Slug:   k.slug,
			Year:   k.year,
			Papers: buckets[k],
		})
	}
	return out
}

func indexOfSlug(order []string, slug string) int {
	for i, s := range order {
		if s == slug {
			return i
		}
	}
	return len(order)
}

func filterSecurityVenueYear(papers []paperEntry, slug string, year int) []paperEntry {
	out := make([]paperEntry, 0, len(papers))
	for _, p := range papers {
		if !hasTag(p.TopicTags, securityTopTag) {
			continue
		}
		if paperSecuritySlug(p) != slug {
			continue
		}
		if paperYear(p) != year {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (s *securityTrendService) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

func (s *securityTrendService) securityTopPapers() ([]paperEntry, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("papers store is not available")
	}
	cat, err := s.store.catalog()
	if err != nil {
		return nil, err
	}
	return filterPapers(cat.Papers, "", securityTopTag), nil
}

func (s *securityTrendService) yearsForSlug(slug string, papers []paperEntry) []securityYearInfo {
	counts := map[int]int{}
	minYear := 0
	for _, p := range papers {
		if paperSecuritySlug(p) != slug {
			continue
		}
		y := paperYear(p)
		if y <= 0 {
			continue
		}
		counts[y]++
		if minYear == 0 || y < minYear {
			minYear = y
		}
	}
	nowY := s.clock().Year()
	floor := securityTopYearFloor
	if minYear > 0 && minYear < floor {
		floor = minYear
	}
	if floor > nowY {
		floor = nowY
	}
	out := make([]securityYearInfo, 0, nowY-floor+1)
	for y := nowY; y >= floor; y-- {
		_, has := s.loadTrend(slug, y)
		out = append(out, securityYearInfo{
			Year:     y,
			Count:    counts[y],
			HasTrend: has,
		})
		delete(counts, y)
	}
	extra := make([]int, 0, len(counts))
	for y := range counts {
		if y > 0 {
			extra = append(extra, y)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(extra)))
	for _, y := range extra {
		_, has := s.loadTrend(slug, y)
		out = append(out, securityYearInfo{Year: y, Count: counts[y], HasTrend: has})
	}
	return out
}

func defaultSecurityYear(years []securityYearInfo, nowYear int) int {
	for _, y := range years {
		if y.Count > 0 {
			return y.Year
		}
	}
	if nowYear > 0 {
		return nowYear
	}
	return securityTopYearFloor
}

func (s *securityTrendService) venueYears(slug string) (securityVenueYearsResp, error) {
	papers, err := s.securityTopPapers()
	if err != nil {
		return securityVenueYearsResp{}, err
	}
	years := s.yearsForSlug(slug, papers)
	return securityVenueYearsResp{
		OK:          true,
		Venue:       securityVenueLabel(slug),
		Slug:        slug,
		DefaultYear: defaultSecurityYear(years, s.clock().Year()),
		Years:       years,
	}, nil
}

func (s *securityTrendService) loadTrend(slug string, year int) (securityTrend, bool) {
	if s == nil || slug == "" {
		return securityTrend{}, false
	}
	b, err := os.ReadFile(securityTrendPath(s.root, slug, year))
	if err != nil {
		return securityTrend{}, false
	}
	var rec securityTrend
	if json.Unmarshal(b, &rec) != nil || !rec.any() {
		return securityTrend{}, false
	}
	if rec.Slug == "" {
		rec.Slug = slug
	}
	if rec.Venue == "" {
		rec.Venue = securityVenueLabel(slug)
	}
	if rec.Year == 0 {
		rec.Year = year
	}
	return rec, true
}

func (s *securityTrendService) saveTrend(rec securityTrend) error {
	if s == nil {
		return fmt.Errorf("security trend store is not available")
	}
	if err := os.MkdirAll(securityTrendDir(s.root), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(securityTrendPath(s.root, rec.Slug, rec.Year), append(b, '\n'), 0o644)
}

func (s *securityTrendService) begin(key string) (wait <-chan struct{}, already bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generating == nil {
		s.generating = map[string]chan struct{}{}
	}
	if ch, ok := s.generating[key]; ok {
		return ch, true
	}
	ch := make(chan struct{})
	s.generating[key] = ch
	return ch, false
}

func (s *securityTrendService) end(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.generating[key]
	if !ok {
		return
	}
	delete(s.generating, key)
	close(ch)
}

func (s *securityTrendService) generateTrend(ctx context.Context, slug string, year int, papers []paperEntry, force bool) (securityTrend, bool, error) {
	if slug == "" {
		return securityTrend{}, false, fmt.Errorf("venue is required")
	}
	key := securityTrendKey(slug, year)
	if rec, ok := s.loadTrend(slug, year); ok && rec.any() && !force {
		return rec, true, nil
	}

	wait, already := s.begin(key)
	if already {
		select {
		case <-wait:
		case <-ctx.Done():
			return securityTrend{}, false, ctx.Err()
		}
		if rec, ok := s.loadTrend(slug, year); ok && rec.any() && !force {
			return rec, true, nil
		}
		if !force {
			return securityTrend{}, false, fmt.Errorf("trend is not ready")
		}
		wait, already = s.begin(key)
		if already {
			select {
			case <-wait:
			case <-ctx.Done():
				return securityTrend{}, false, ctx.Err()
			}
			if rec, ok := s.loadTrend(slug, year); ok && rec.any() {
				return rec, true, nil
			}
			return securityTrend{}, false, fmt.Errorf("trend is not ready")
		}
	}
	defer s.end(key)

	if rec, ok := s.loadTrend(slug, year); ok && rec.any() && !force {
		return rec, true, nil
	}

	var snap translateSnapshot
	if s.store != nil && s.store.xlate != nil {
		snap = s.store.xlate.snapshot()
	}
	if !snap.ready() {
		return securityTrend{}, false, errReviewLLMNotReady
	}

	rec, err := s.callTrend(ctx, snap, securityVenueLabel(slug), year, papers)
	if err != nil {
		return securityTrend{}, false, err
	}
	if !rec.any() {
		return securityTrend{}, false, fmt.Errorf("empty trend from model")
	}
	rec.Venue = securityVenueLabel(slug)
	rec.Slug = slug
	rec.Year = year
	rec.PaperCount = len(papers)
	if rec.Model == "" {
		rec.Model = snap.Model
	}
	rec.GeneratedAt = s.clock().Format(time.RFC3339)
	if err := s.saveTrend(rec); err != nil {
		return securityTrend{}, false, err
	}
	return rec, false, nil
}

func (s *securityTrendService) callTrend(ctx context.Context, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error) {
	if s != nil && s.generateFn != nil {
		return s.generateFn(ctx, snap, venue, year, papers)
	}
	return generateSecurityTrendOpenAI(ctx, &http.Client{Timeout: securityTrendHTTPTimeout}, snap, venue, year, papers)
}

const securityTrendSystemPrompt = `你是资深系统安全研究分析师，擅长从某一安全顶会某一年的论文列表中提炼研究趋势。
请基于该会议该年论文的标题与摘要（含中文摘要，如有），撰写简体中文「研究趋势综述」，帮助读者快速把握当年该会的技术图景。
必须只输出一个 JSON 对象，不要 markdown 围栏，不要前言。字段：
{
  "headline": "一句话总标题，点出该会该年最显著的研究主题。",
  "overview": "当年整体研究图景：主线、交叉点与值得注意的转向。2–4 段。",
  "themes": ["3–6 个主题短语，概括聚类方向"],
  "highlights": ["3–6 条要点，各用一句话点名具体工作或方法，不要编造摘要中没有的结果"],
  "outlook": "对后续研究与工程落地的简短展望。1–2 段。"
}
语气专业、克制、具体。不要把每篇论文逐条复述成目录；要做跨论文综合。信息不足时明确说明。`

func securityTrendUserPrompt(venue string, year int, papers []paperEntry) string {
	var b strings.Builder
	b.WriteString("会议：")
	b.WriteString(strings.TrimSpace(venue))
	b.WriteString("\n年份：")
	b.WriteString(strconv.Itoa(year))
	b.WriteString("\n论文数量：")
	b.WriteString(strconv.Itoa(len(papers)))
	b.WriteString("\n\n")
	n := len(papers)
	if n > securityTrendMaxPapers {
		n = securityTrendMaxPapers
	}
	for i := 0; i < n; i++ {
		p := papers[i]
		fmt.Fprintf(&b, "【%d】%s\n", i+1, strings.TrimSpace(p.Title))
		if id := strings.TrimSpace(p.ArxivID); id != "" {
			b.WriteString("arXiv：")
			b.WriteString(id)
			b.WriteByte('\n')
		}
		if len(p.Authors) > 0 {
			b.WriteString("作者：")
			if len(p.Authors) > 6 {
				b.WriteString(strings.Join(p.Authors[:6], ", "))
				b.WriteString(" 等")
			} else {
				b.WriteString(strings.Join(p.Authors, ", "))
			}
			b.WriteByte('\n')
		}
		abs := strings.TrimSpace(p.Abstract)
		if abs == "" {
			abs = strings.TrimSpace(p.Brief)
		}
		if len([]rune(abs)) > securityTrendAbstractRunes {
			abs = string([]rune(abs)[:securityTrendAbstractRunes]) + "…"
		}
		if abs != "" {
			b.WriteString("摘要：")
			b.WriteString(abs)
			b.WriteByte('\n')
		}
		zh := strings.TrimSpace(p.AbstractZH)
		if len([]rune(zh)) > securityTrendAbstractRunes {
			zh = string([]rune(zh)[:securityTrendAbstractRunes]) + "…"
		}
		if zh != "" {
			b.WriteString("中文摘要：")
			b.WriteString(zh)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(papers) > securityTrendMaxPapers {
		fmt.Fprintf(&b, "（其余 %d 篇已省略标题列表。）\n", len(papers)-securityTrendMaxPapers)
	}
	return b.String()
}

func generateSecurityTrendOpenAI(ctx context.Context, client *http.Client, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error) {
	var empty securityTrend
	base := strings.TrimRight(strings.TrimSpace(snap.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(snap.Model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	if client == nil {
		client = &http.Client{Timeout: securityTrendHTTPTimeout}
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": securityTrendSystemPrompt},
			{"role": "user", "content": securityTrendUserPrompt(venue, year, papers)},
		},
		"temperature": 0.3,
		"max_tokens":  securityTrendMaxTokens,
	})
	if err != nil {
		return empty, err
	}
	body, err := postHubChat(ctx, client, base, snap.APIKey, payload)
	if err != nil {
		return empty, err
	}
	text, err := extractChatContent(body)
	if err != nil {
		return empty, err
	}
	parsed, err := parseHFDailyTrend(text)
	if err != nil {
		return empty, err
	}
	return securityTrend{
		Headline:   parsed.Headline,
		Overview:   parsed.Overview,
		Themes:     parsed.Themes,
		Highlights: parsed.Highlights,
		Outlook:    parsed.Outlook,
		Model:      model,
	}, nil
}

type securityTrendReq struct {
	Force bool `json:"force"`
}

func (s *Server) handleSecurityTrend(w http.ResponseWriter, r *http.Request) {
	svc := s.papers().secTrends
	if svc == nil {
		writeErr(w, http.StatusBadGateway, "security trend store is not available", "papers", nil, "")
		return
	}
	venueRaw := strings.TrimSpace(r.URL.Query().Get("venue"))
	yearRaw := strings.TrimSpace(r.URL.Query().Get("year"))
	if venueRaw == "" {
		venueRaw = strings.TrimSpace(r.PathValue("slug"))
	}
	if yearRaw == "" {
		yearRaw = strings.TrimSpace(r.PathValue("year"))
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if venueRaw == "" && yearRaw == "" {
			s.writeSecurityTrendIndex(w, svc)
			return
		}
		label, slug, err := resolveSecurityVenue(venueRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
			return
		}
		if yearRaw == "" {
			out, err := svc.venueYears(slug)
			if err != nil {
				log.Printf("papers security-trend years slug=%s: %v", slug, err)
				writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
				return
			}
			out.Venue = label
			writeJSON(w, http.StatusOK, out)
			return
		}
		year, err := parseSecurityYear(yearRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
			return
		}
		if rec, ok := svc.loadTrend(slug, year); ok {
			writeJSON(w, http.StatusOK, rec.public(false))
			return
		}
		papers, _ := svc.securityTopPapers()
		n := len(filterSecurityVenueYear(papers, slug, year))
		writeJSON(w, http.StatusOK, securityTrendView{
			OK:         true,
			Venue:      label,
			Slug:       slug,
			Year:       year,
			PaperCount: n,
			HasTrend:   false,
		})
	case http.MethodPost:
		// Admin force-regenerate only. Visitors view GET caches; missing
		// trends are filled by the background worker, not by anonymous POST.
		if !s.authorizeSettings(w, r) {
			return
		}
		label, slug, err := resolveSecurityVenue(venueRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
			return
		}
		year, err := parseSecurityYear(yearRaw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), search.CodeBadRequest, nil, "")
			return
		}
		var req securityTrendReq
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && err != io.EOF {
				writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
				return
			}
		}
		papers, err := svc.securityTopPapers()
		if err != nil {
			log.Printf("papers security-trend catalog: %v", err)
			writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
			return
		}
		group := filterSecurityVenueYear(papers, slug, year)
		if len(group) == 0 {
			writeErr(w, http.StatusNotFound, "no papers for this venue and year", "papers", nil, "")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), securityTrendHTTPTimeout)
		defer cancel()
		rec, skipped, err := svc.generateTrend(ctx, slug, year, group, req.Force)
		if err != nil {
			if err == errReviewLLMNotReady {
				writeErr(w, http.StatusServiceUnavailable, err.Error(), search.CodeEngine, nil, "")
				return
			}
			log.Printf("papers security-trend slug=%s year=%d: %v", slug, year, err)
			writeReviewGenerateErr(w, err)
			return
		}
		if rec.Venue == "" {
			rec.Venue = label
		}
		writeJSON(w, http.StatusOK, rec.public(skipped))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *Server) writeSecurityTrendIndex(w http.ResponseWriter, svc *securityTrendService) {
	papers, err := svc.securityTopPapers()
	if err != nil {
		log.Printf("papers security-trend index: %v", err)
		writeErr(w, http.StatusBadGateway, "papers catalog unavailable", "papers", nil, "")
		return
	}
	out := securityTrendIndexResp{OK: true, Venues: make([]securityVenueYearsResp, 0, len(securityVenues))}
	for _, v := range securityVenues {
		years := svc.yearsForSlug(v.Slug, papers)
		out.Venues = append(out.Venues, securityVenueYearsResp{
			OK:          true,
			Venue:       v.Label,
			Slug:        v.Slug,
			DefaultYear: defaultSecurityYear(years, svc.clock().Year()),
			Years:       years,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
