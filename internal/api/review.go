package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"search-service/internal/proxyauth"
	"search-service/internal/search"
)

const (
	reviewsDirName    = "reviews"
	raterCookieName   = "rs_papers_rater"
	raterCookieMaxAge = 365 * 24 * 60 * 60
	reviewHTTPTimeout = 120 * time.Second
	reviewMaxTokens   = 2500
	reviewMinStars    = 1
	reviewMaxStars    = 5
)

// paperReviewAnalysis is the structured Chinese 精读+评审 write-up.
type paperReviewAnalysis struct {
	MethodPrinciples string `json:"method_principles"`
	MethodEssence    string `json:"method_essence"`
	Experiment       string `json:"experiment"`
	Quality          string `json:"quality"`
}

func (a paperReviewAnalysis) any() bool {
	return strings.TrimSpace(a.MethodPrinciples) != "" ||
		strings.TrimSpace(a.MethodEssence) != "" ||
		strings.TrimSpace(a.Experiment) != "" ||
		strings.TrimSpace(a.Quality) != ""
}

type paperReviewRating struct {
	UserKey string `json:"user_key"`
	Stars   int    `json:"stars"`
	At      string `json:"at"`
}

// paperReviewFile is the on-disk record under $PAPERS_DIR/reviews/<id>.json.
type paperReviewFile struct {
	PaperID     string              `json:"paper_id"`
	Title       string              `json:"title"`
	Analysis    paperReviewAnalysis `json:"analysis"`
	Model       string              `json:"model,omitempty"`
	GeneratedAt string              `json:"generated_at,omitempty"`
	Ratings       []paperReviewRating `json:"ratings,omitempty"`
	AvgStars      float64             `json:"avg_stars"`
	RatingCount   int                 `json:"rating_count"`
	Refs          []paperReviewRef    `json:"refs,omitempty"`
	RefsUpdatedAt string              `json:"refs_updated_at,omitempty"`
}

func (r *paperReviewFile) hasAnalysis() bool {
	return r != nil && r.Analysis.any()
}

type paperReviewView struct {
	OK          bool                `json:"ok"`
	PaperID     string              `json:"paper_id"`
	Title       string              `json:"title"`
	Analysis    paperReviewAnalysis `json:"analysis"`
	Model       string              `json:"model,omitempty"`
	GeneratedAt string              `json:"generated_at,omitempty"`
	AvgStars    float64             `json:"avg_stars"`
	RatingCount int                 `json:"rating_count"`
	HasReview   bool                `json:"has_review"`
	MyStars     int                 `json:"my_stars,omitempty"`
	Skipped     bool                `json:"skipped,omitempty"`
	Refs        []paperReviewRef    `json:"refs,omitempty"`
}

type reviewSummary struct {
	HasReview   bool
	AvgStars    float64
	RatingCount int
}

type reviewGenerateFn func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error)

type reviewService struct {
	root  string
	xlate *translateService

	mu         sync.Mutex
	index      map[string]reviewSummary
	indexOK    bool
	locks      map[string]*sync.Mutex
	generating map[string]chan struct{}

	client     *http.Client
	generateFn reviewGenerateFn
}

var (
	errReviewLLMNotReady = fmt.Errorf("translation LLM is not configured")
	errReviewNotReady    = fmt.Errorf("review is not ready")
	errReviewGenerating  = fmt.Errorf("review is still generating")
)

func reviewsDir(root string) string {
	return filepath.Join(root, reviewsDirName)
}

func reviewFilePath(root, id string) string {
	return filepath.Join(reviewsDir(root), sanitizePaperID(id)+".json")
}

func newReviewService(root string, xlate *translateService) *reviewService {
	if root == "" {
		root = papersRoot()
	}
	return &reviewService{
		root:       root,
		xlate:      xlate,
		index:      map[string]reviewSummary{},
		locks:      map[string]*sync.Mutex{},
		generating: map[string]chan struct{}{},
		client:     nil,
	}
}

func (s *reviewService) isGenerating(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.generating[id]
	return ok
}

func (s *reviewService) beginGenerate(id string) (wait <-chan struct{}, already bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generating == nil {
		s.generating = map[string]chan struct{}{}
	}
	if ch, ok := s.generating[id]; ok {
		return ch, true
	}
	ch := make(chan struct{})
	s.generating[id] = ch
	return ch, false
}

func (s *reviewService) endGenerate(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.generating[id]
	if !ok {
		return
	}
	delete(s.generating, id)
	close(ch)
}

func (s *reviewService) lockID(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

func (s *reviewService) load(id string) (paperReviewFile, bool) {
	id = sanitizePaperID(id)
	if s == nil || id == "" {
		return paperReviewFile{}, false
	}
	b, err := os.ReadFile(reviewFilePath(s.root, id))
	if err != nil {
		return paperReviewFile{}, false
	}
	var rec paperReviewFile
	if err := json.Unmarshal(b, &rec); err != nil {
		return paperReviewFile{}, false
	}
	recalcReviewStats(&rec)
	return rec, true
}

func (s *reviewService) persist(rec *paperReviewFile) error {
	if s == nil || rec == nil {
		return fmt.Errorf("review store is not available")
	}
	recalcReviewStats(rec)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(reviewFilePath(s.root, rec.PaperID), append(b, '\n'), 0o644); err != nil {
		return err
	}
	s.remember(*rec)
	return nil
}

func (s *reviewService) remember(rec paperReviewFile) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == nil {
		s.index = map[string]reviewSummary{}
	}
	s.index[rec.PaperID] = reviewSummary{
		HasReview:   rec.hasAnalysis(),
		AvgStars:    rec.AvgStars,
		RatingCount: rec.RatingCount,
	}
	s.indexOK = true
}

func (s *reviewService) ensureIndex() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.indexOK {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	dir := reviewsDir(s.root)
	ents, err := os.ReadDir(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == nil {
		s.index = map[string]reviewSummary{}
	}
	if err != nil {
		s.indexOK = true
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		id = sanitizePaperID(id)
		if id == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec paperReviewFile
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		if rec.PaperID == "" {
			rec.PaperID = id
		}
		recalcReviewStats(&rec)
		s.index[rec.PaperID] = reviewSummary{
			HasReview:   rec.hasAnalysis(),
			AvgStars:    rec.AvgStars,
			RatingCount: rec.RatingCount,
		}
	}
	s.indexOK = true
}

func (s *reviewService) overlay(papers []paperEntry) {
	if s == nil || len(papers) == 0 {
		return
	}
	s.ensureIndex()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range papers {
		id := papers[i].ID
		if id == "" {
			id = paperTranslateID(papers[i])
			papers[i].ID = id
		}
		if id == "" {
			continue
		}
		sum, ok := s.index[id]
		if !ok {
			continue
		}
		papers[i].HasReview = sum.HasReview
		papers[i].AvgStars = sum.AvgStars
		papers[i].RatingCount = sum.RatingCount
	}
}

func (s *reviewService) getPublic(id, raterKey string) (paperReviewView, bool) {
	rec, ok := s.load(id)
	if !ok || !rec.hasAnalysis() {
		return paperReviewView{}, false
	}
	return rec.public(raterKey, false), true
}

func (rec paperReviewFile) public(raterKey string, skipped bool) paperReviewView {
	return paperReviewView{
		OK:          true,
		PaperID:     rec.PaperID,
		Title:       rec.Title,
		Analysis:    rec.Analysis,
		Model:       rec.Model,
		GeneratedAt: rec.GeneratedAt,
		AvgStars:    rec.AvgStars,
		RatingCount: rec.RatingCount,
		HasReview:   rec.hasAnalysis(),
		MyStars:     myStars(rec.Ratings, raterKey),
		Skipped:     skipped,
		Refs:        rec.Refs,
	}
}

func myStars(ratings []paperReviewRating, key string) int {
	if key == "" {
		return 0
	}
	for _, r := range ratings {
		if r.UserKey == key {
			return r.Stars
		}
	}
	return 0
}

func upsertRating(ratings []paperReviewRating, userKey string, stars int, at string) []paperReviewRating {
	if userKey == "" {
		return ratings
	}
	for i := range ratings {
		if ratings[i].UserKey == userKey {
			ratings[i].Stars = stars
			ratings[i].At = at
			return ratings
		}
	}
	return append(ratings, paperReviewRating{UserKey: userKey, Stars: stars, At: at})
}

func recalcReviewStats(r *paperReviewFile) {
	if r == nil {
		return
	}
	sum := 0
	n := 0
	for _, x := range r.Ratings {
		if x.Stars < reviewMinStars || x.Stars > reviewMaxStars {
			continue
		}
		sum += x.Stars
		n++
	}
	r.RatingCount = n
	if n == 0 {
		r.AvgStars = 0
		return
	}
	r.AvgStars = math.Round(float64(sum)/float64(n)*10) / 10
}

func (s *reviewService) rate(id, userKey string, stars int) (paperReviewFile, error) {
	id = sanitizePaperID(id)
	if id == "" {
		return paperReviewFile{}, fmt.Errorf("invalid paper id")
	}
	if userKey == "" {
		return paperReviewFile{}, fmt.Errorf("missing rater")
	}
	if stars < reviewMinStars || stars > reviewMaxStars {
		return paperReviewFile{}, fmt.Errorf("stars must be %d–%d", reviewMinStars, reviewMaxStars)
	}
	// Fail closed immediately while generate owns this id — do not wait on
	// the persist lock (that lock is only for short file writes).
	if s.isGenerating(id) {
		return paperReviewFile{}, errReviewGenerating
	}
	m := s.lockID(id)
	m.Lock()
	defer m.Unlock()
	if s.isGenerating(id) {
		return paperReviewFile{}, errReviewGenerating
	}
	rec, ok := s.load(id)
	if !ok || !rec.hasAnalysis() {
		return paperReviewFile{}, errReviewNotReady
	}
	rec.Ratings = upsertRating(rec.Ratings, userKey, stars, time.Now().UTC().Format(time.RFC3339))
	if err := s.persist(&rec); err != nil {
		return paperReviewFile{}, err
	}
	return rec, nil
}

func (s *reviewService) generate(ctx context.Context, paper paperEntry, force bool) (paperReviewFile, bool, error) {
	id := paper.ID
	if id == "" {
		id = paperTranslateID(paper)
		paper.ID = id
	}
	id = sanitizePaperID(id)
	if id == "" {
		return paperReviewFile{}, false, fmt.Errorf("invalid paper id")
	}
	paper.ID = id

	m := s.lockID(id)
	m.Lock()
	if rec, ok := s.load(id); ok && rec.hasAnalysis() && !force {
		m.Unlock()
		return rec, true, nil
	}
	m.Unlock()

	wait, already := s.beginGenerate(id)
	if already {
		select {
		case <-wait:
		case <-ctx.Done():
			return paperReviewFile{}, false, ctx.Err()
		}
		if rec, ok := s.load(id); ok && rec.hasAnalysis() && !force {
			return rec, true, nil
		}
		if !force {
			return paperReviewFile{}, false, errReviewNotReady
		}
		wait, already = s.beginGenerate(id)
		if already {
			select {
			case <-wait:
			case <-ctx.Done():
				return paperReviewFile{}, false, ctx.Err()
			}
			if rec, ok := s.load(id); ok && rec.hasAnalysis() {
				return rec, true, nil
			}
			return paperReviewFile{}, false, errReviewNotReady
		}
	}
	defer s.endGenerate(id)

	if rec, ok := s.load(id); ok && rec.hasAnalysis() && !force {
		return rec, true, nil
	}

	snap := translateSnapshot{}
	if s.xlate != nil {
		snap = s.xlate.snapshot()
	}
	if !snap.ready() {
		return paperReviewFile{}, false, errReviewLLMNotReady
	}

	// LLM call without the persist lock so rate can fail closed immediately.
	analysis, model, err := s.callGenerate(ctx, snap, paper)
	if err != nil {
		return paperReviewFile{}, false, err
	}
	if !analysis.any() {
		return paperReviewFile{}, false, fmt.Errorf("empty review from model")
	}
	if model == "" {
		model = snap.Model
	}

	m.Lock()
	defer m.Unlock()
	rec, _ := s.load(id)
	rec.PaperID = id
	if title := strings.TrimSpace(paper.Title); title != "" {
		rec.Title = title
	}
	rec.Analysis = analysis
	rec.Model = model
	rec.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if rec.Ratings == nil {
		rec.Ratings = []paperReviewRating{}
	}
	if err := s.persist(&rec); err != nil {
		return paperReviewFile{}, false, err
	}
	return rec, false, nil
}

func (s *reviewService) callGenerate(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
	if s != nil && s.generateFn != nil {
		return s.generateFn(ctx, snap, paper)
	}
	return generateReviewOpenAI(ctx, s.httpClient(), snap, paper)
}

func (s *reviewService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: reviewHTTPTimeout}
}

const reviewSystemPrompt = `你是资深学术审稿人，擅长精读与评审 AI / 智能体 / 安全方向论文。
请基于给定的论文元数据（标题、作者、摘要、年份、标签）撰写简体中文解读，帮助读者判断是否值得精读。
必须只输出一个 JSON 对象，不要 markdown 围栏，不要前言。字段：
{
  "method_principles": "方法原理与创新：核心方法如何工作、相对已有工作的创新点。2–5 段。",
  "method_essence": "方法本质：用更本质的语言概括其在做什么（问题形式、关键假设、机制）。2–4 段。",
  "experiment": "实验完整性：从摘要能推断的实验设置、基准、对比、消融与不足。2–4 段。信息不足时明确指出局限。",
  "quality": "论文质量总评：贡献、严谨性、可复现性、写作与潜在问题的综合评价，给出是否值得跟进的建议。2–4 段。"
}
语气专业、克制、具体，避免空洞套话。不要编造摘要中不存在的实验结果。`

func reviewUserPrompt(p paperEntry) string {
	var b strings.Builder
	b.WriteString("标题：")
	b.WriteString(strings.TrimSpace(p.Title))
	b.WriteByte('\n')
	if len(p.Authors) > 0 {
		b.WriteString("作者：")
		b.WriteString(strings.Join(p.Authors, ", "))
		b.WriteByte('\n')
	}
	if p.Year > 0 {
		b.WriteString("年份：")
		b.WriteString(fmt.Sprintf("%d", p.Year))
		b.WriteByte('\n')
	}
	if id := strings.TrimSpace(p.ArxivID); id != "" {
		b.WriteString("arXiv：")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	if len(p.TopicTags) > 0 {
		b.WriteString("标签：")
		b.WriteString(strings.Join(p.TopicTags, ", "))
		b.WriteByte('\n')
	}
	abs := strings.TrimSpace(p.Abstract)
	if abs == "" {
		abs = strings.TrimSpace(p.Brief)
	}
	b.WriteString("摘要：\n")
	b.WriteString(abs)
	return b.String()
}

func generateReviewOpenAI(ctx context.Context, client *http.Client, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
	var empty paperReviewAnalysis
	base := strings.TrimRight(strings.TrimSpace(snap.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(snap.Model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	if client == nil {
		client = &http.Client{Timeout: reviewHTTPTimeout}
	}
	urls := []string{base + "/chat/completions"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/chat/completions")
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewSystemPrompt},
			{"role": "user", "content": reviewUserPrompt(paper)},
		},
		"temperature": 0.3,
		"max_tokens":  reviewMaxTokens,
	})
	if err != nil {
		return empty, "", err
	}
	var last error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			last = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+snap.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			last = fmt.Errorf("chat HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusNotFound {
				continue
			}
			return empty, "", last
		}
		text, err := extractChatContent(body)
		if err != nil {
			return empty, "", err
		}
		analysis, err := parseReviewAnalysis(text)
		if err != nil {
			return empty, "", err
		}
		return analysis, model, nil
	}
	if last == nil {
		last = fmt.Errorf("chat review failed")
	}
	return empty, "", last
}

func parseReviewAnalysis(raw string) (paperReviewAnalysis, error) {
	var empty paperReviewAnalysis
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var a paperReviewAnalysis
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return empty, err
	}
	a.MethodPrinciples = strings.TrimSpace(a.MethodPrinciples)
	a.MethodEssence = strings.TrimSpace(a.MethodEssence)
	a.Experiment = strings.TrimSpace(a.Experiment)
	a.Quality = strings.TrimSpace(a.Quality)
	if !a.any() {
		return empty, fmt.Errorf("empty analysis")
	}
	return a, nil
}

func (ps *papersStore) findPaper(id string) (paperEntry, bool) {
	id = sanitizePaperID(id)
	if ps == nil || id == "" {
		return paperEntry{}, false
	}
	cat, err := ps.catalogBase()
	if err != nil {
		return paperEntry{}, false
	}
	for _, p := range cat.Papers {
		pid := p.ID
		if pid == "" {
			pid = paperTranslateID(p)
		}
		if pid == id {
			return p, true
		}
	}
	return paperEntry{}, false
}

func (s *Server) handlePapersReview(w http.ResponseWriter, r *http.Request) {
	id := sanitizePaperID(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid paper id", search.CodeBadRequest, nil, "")
		return
	}
	rater := s.ensureRater(w, r)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		rev := s.papers().reviews
		if rev == nil {
			writeErr(w, http.StatusNotFound, "review not found", "papers", nil, "")
			return
		}
		view, ok := rev.getPublic(id, rater)
		if !ok {
			writeErr(w, http.StatusNotFound, "review not found", "papers", nil, "")
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

type reviewGenerateReq struct {
	Force bool `json:"force"`
}

func (s *Server) handlePapersReviewGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	id := sanitizePaperID(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid paper id", search.CodeBadRequest, nil, "")
		return
	}
	rater := s.ensureRater(w, r)
	var req reviewGenerateReq
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
			return
		}
	}
	ps := s.papers()
	paper, ok := ps.findPaper(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "paper not found", "papers", nil, "")
		return
	}
	if ps.reviews == nil {
		writeErr(w, http.StatusBadGateway, "review store is not available", search.CodeEngine, nil, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), reviewHTTPTimeout)
	defer cancel()
	rec, skipped, err := ps.reviews.generate(ctx, paper, req.Force)
	if err == nil && rec.hasAnalysis() && !skipped {
		if updated, rerr := ps.reviews.refreshRefs(id, catalogPapersForRefs(ps)); rerr == nil {
			rec = updated
		}
	}
	if err != nil {
		if err == errReviewLLMNotReady {
			writeErr(w, http.StatusServiceUnavailable, err.Error(), search.CodeEngine, nil, "")
			return
		}
		log.Printf("papers review generate id=%s: %v", id, err)
		writeErr(w, http.StatusBadGateway, "could not generate review", search.CodeEngine, nil, "")
		return
	}
	writeJSON(w, http.StatusOK, rec.public(rater, skipped))
}

type reviewRateReq struct {
	Stars int `json:"stars"`
}

func (s *Server) handlePapersReviewRate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	id := sanitizePaperID(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid paper id", search.CodeBadRequest, nil, "")
		return
	}
	rater := s.ensureRater(w, r)
	var req reviewRateReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
		return
	}
	if req.Stars < reviewMinStars || req.Stars > reviewMaxStars {
		writeErr(w, http.StatusBadRequest, "stars must be 1-5", search.CodeBadRequest, nil, "")
		return
	}
	rev := s.papers().reviews
	if rev == nil {
		writeErr(w, http.StatusNotFound, "review not found", "papers", nil, "")
		return
	}
	rec, err := rev.rate(id, rater, req.Stars)
	if err != nil {
		if errors.Is(err, errReviewGenerating) {
			writeErr(w, http.StatusConflict, errReviewGenerating.Error(), search.CodeBusy, nil, "")
			return
		}
		if errors.Is(err, errReviewNotReady) || os.IsNotExist(err) {
			writeErr(w, http.StatusConflict, errReviewNotReady.Error(), search.CodeBusy, nil, "")
			return
		}
		log.Printf("papers review rate id=%s: %v", id, err)
		writeErr(w, http.StatusBadGateway, "could not save rating", search.CodeEngine, nil, "")
		return
	}
	writeJSON(w, http.StatusOK, rec.public(rater, false))
}

func (s *Server) ensureRater(w http.ResponseWriter, r *http.Request) string {
	if tok := strings.TrimSpace(proxyauth.CookieToken(r)); tok != "" {
		return "hub:" + hashTokenPrefix(tok)
	}
	if tok := strings.TrimSpace(proxyauth.BearerToken(r)); tok != "" && !s.isSharedSearchToken(tok) {
		return "hub:" + hashTokenPrefix(tok)
	}
	if c, err := r.Cookie(raterCookieName); err == nil && c != nil && validRaterCookie(c.Value) {
		return "anon:" + c.Value
	}
	id := newRaterID()
	http.SetCookie(w, &http.Cookie{
		Name:     raterCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   raterCookieMaxAge,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	return "anon:" + id
}

func (s *Server) isSharedSearchToken(tok string) bool {
	if s == nil || s.auth == nil {
		return false
	}
	want := strings.TrimSpace(s.auth.SearchToken)
	if want == "" || len(want) != len(tok) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(tok)) == 1
}

func hashTokenPrefix(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func newRaterID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b[:])
}

func validRaterCookie(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 16 || len(v) > 64 {
		return false
	}
	for _, r := range v {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
