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
	"strings"
	"time"

	"search-service/internal/search"
)

const (
	hfDailyTrendHTTPTimeout = 120 * time.Second
	hfDailyTrendMaxTokens   = 3500
)

type hfDailyTrendFn func(ctx context.Context, snap translateSnapshot, date string, papers []paperEntry) (hfDailyTrend, error)

type hfDailyTrend struct {
	Date        string   `json:"date"`
	Headline    string   `json:"headline"`
	Overview    string   `json:"overview"`
	Themes      []string `json:"themes,omitempty"`
	Highlights  []string `json:"highlights,omitempty"`
	Outlook     string   `json:"outlook,omitempty"`
	Model       string   `json:"model,omitempty"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	PaperCount  int      `json:"paper_count,omitempty"`
}

func (t hfDailyTrend) any() bool {
	return strings.TrimSpace(t.Headline) != "" ||
		strings.TrimSpace(t.Overview) != "" ||
		len(t.Themes) > 0 ||
		len(t.Highlights) > 0 ||
		strings.TrimSpace(t.Outlook) != ""
}

type hfDailyTrendView struct {
	OK          bool     `json:"ok"`
	Date        string   `json:"date"`
	Headline    string   `json:"headline"`
	Overview    string   `json:"overview"`
	Themes      []string `json:"themes,omitempty"`
	Highlights  []string `json:"highlights,omitempty"`
	Outlook     string   `json:"outlook,omitempty"`
	Model       string   `json:"model,omitempty"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	PaperCount  int      `json:"paper_count,omitempty"`
	HasTrend    bool     `json:"has_trend"`
	Skipped     bool     `json:"skipped,omitempty"`
}

func (t hfDailyTrend) public(skipped bool) hfDailyTrendView {
	return hfDailyTrendView{
		OK:          true,
		Date:        t.Date,
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

func (s *hfDailyService) loadTrend(date string) (hfDailyTrend, bool) {
	if s == nil || date == "" {
		return hfDailyTrend{}, false
	}
	b, err := os.ReadFile(hfDailyTrendPath(s.root, date))
	if err != nil {
		return hfDailyTrend{}, false
	}
	var rec hfDailyTrend
	if json.Unmarshal(b, &rec) != nil || !rec.any() {
		return hfDailyTrend{}, false
	}
	if rec.Date == "" {
		rec.Date = date
	}
	return rec, true
}

func (s *hfDailyService) saveTrend(rec hfDailyTrend) error {
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
	return atomicWriteFile(hfDailyTrendPath(s.root, rec.Date), append(b, '\n'), 0o644)
}

func (s *hfDailyService) beginTrend(date string) (wait <-chan struct{}, already bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generating == nil {
		s.generating = map[string]chan struct{}{}
	}
	if ch, ok := s.generating[date]; ok {
		return ch, true
	}
	ch := make(chan struct{})
	s.generating[date] = ch
	return ch, false
}

func (s *hfDailyService) endTrend(date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.generating[date]
	if !ok {
		return
	}
	delete(s.generating, date)
	close(ch)
}

func (s *hfDailyService) generateTrend(ctx context.Context, date string, papers []paperEntry, force bool) (hfDailyTrend, bool, error) {
	if date == "" {
		return hfDailyTrend{}, false, fmt.Errorf("date is required")
	}
	if rec, ok := s.loadTrend(date); ok && rec.any() && !force {
		return rec, true, nil
	}

	wait, already := s.beginTrend(date)
	if already {
		select {
		case <-wait:
		case <-ctx.Done():
			return hfDailyTrend{}, false, ctx.Err()
		}
		if rec, ok := s.loadTrend(date); ok && rec.any() && !force {
			return rec, true, nil
		}
		if !force {
			return hfDailyTrend{}, false, fmt.Errorf("trend is not ready")
		}
		wait, already = s.beginTrend(date)
		if already {
			select {
			case <-wait:
			case <-ctx.Done():
				return hfDailyTrend{}, false, ctx.Err()
			}
			if rec, ok := s.loadTrend(date); ok && rec.any() {
				return rec, true, nil
			}
			return hfDailyTrend{}, false, fmt.Errorf("trend is not ready")
		}
	}
	defer s.endTrend(date)

	if rec, ok := s.loadTrend(date); ok && rec.any() && !force {
		return rec, true, nil
	}

	var snap translateSnapshot
	if s.store != nil && s.store.xlate != nil {
		snap = s.store.xlate.snapshot()
	}
	if !snap.ready() {
		return hfDailyTrend{}, false, errReviewLLMNotReady
	}

	rec, err := s.callTrend(ctx, snap, date, papers)
	if err != nil {
		return hfDailyTrend{}, false, err
	}
	if !rec.any() {
		return hfDailyTrend{}, false, fmt.Errorf("empty trend from model")
	}
	rec.Date = date
	rec.PaperCount = len(papers)
	if rec.Model == "" {
		rec.Model = snap.Model
	}
	rec.GeneratedAt = s.clock().Format(time.RFC3339)
	if err := s.saveTrend(rec); err != nil {
		return hfDailyTrend{}, false, err
	}
	return rec, false, nil
}

func (s *hfDailyService) callTrend(ctx context.Context, snap translateSnapshot, date string, papers []paperEntry) (hfDailyTrend, error) {
	if s != nil && s.generateFn != nil {
		return s.generateFn(ctx, snap, date, papers)
	}
	return generateHFDailyTrendOpenAI(ctx, s.httpClient(), snap, date, papers)
}

const hfDailyTrendSystemPrompt = `你是资深 AI 研究分析师，擅长从一天的论文列表中提炼技术趋势。
请基于当日 Hugging Face Daily Papers 的标题、作者与摘要，撰写简体中文「每日技术趋势综述」，帮助读者快速判断当天方向。
必须只输出一个 JSON 对象，不要 markdown 围栏，不要前言。字段：
{
  "headline": "一句话总标题，点出当天最显著的技术主题。",
  "overview": "当日整体技术图景：主线、交叉点与值得注意的转向。2–4 段。",
  "themes": ["3–6 个主题短语，概括聚类方向"],
  "highlights": ["3–6 条要点，各用一句话点名具体工作或方法，不要编造摘要中没有的结果"],
  "outlook": "对后续研究与工程落地的简短展望。1–2 段。"
}
语气专业、克制、具体。不要把每篇论文逐条复述成目录；要做跨论文综合。信息不足时明确说明。`

func hfDailyTrendUserPrompt(date string, papers []paperEntry) string {
	var b strings.Builder
	b.WriteString("日期：")
	b.WriteString(date)
	b.WriteString("\n论文数量：")
	b.WriteString(fmt.Sprintf("%d", len(papers)))
	b.WriteString("\n\n")
	const maxPapers = 40
	n := len(papers)
	if n > maxPapers {
		n = maxPapers
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
		if abs == "" {
			abs = strings.TrimSpace(p.AbstractZH)
		}
		if len([]rune(abs)) > 600 {
			abs = string([]rune(abs)[:600]) + "…"
		}
		b.WriteString("摘要：")
		b.WriteString(abs)
		b.WriteString("\n\n")
	}
	if len(papers) > maxPapers {
		fmt.Fprintf(&b, "（其余 %d 篇已省略标题列表。）\n", len(papers)-maxPapers)
	}
	return b.String()
}

func generateHFDailyTrendOpenAI(ctx context.Context, client *http.Client, snap translateSnapshot, date string, papers []paperEntry) (hfDailyTrend, error) {
	var empty hfDailyTrend
	base := strings.TrimRight(strings.TrimSpace(snap.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(snap.Model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	if client == nil {
		client = &http.Client{Timeout: hfDailyTrendHTTPTimeout}
	}
	urls := []string{base + "/chat/completions"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/chat/completions")
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": hfDailyTrendSystemPrompt},
			{"role": "user", "content": hfDailyTrendUserPrompt(date, papers)},
		},
		"temperature": 0.3,
		"max_tokens":  hfDailyTrendMaxTokens,
	})
	if err != nil {
		return empty, err
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
			return empty, last
		}
		text, err := extractChatContent(body)
		if err != nil {
			return empty, err
		}
		trend, err := parseHFDailyTrend(text)
		if err != nil {
			return empty, err
		}
		trend.Model = model
		return trend, nil
	}
	if last == nil {
		last = fmt.Errorf("chat trend failed")
	}
	return empty, last
}

func parseHFDailyTrend(raw string) (hfDailyTrend, error) {
	var empty hfDailyTrend
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
	var t hfDailyTrend
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return empty, err
	}
	t.Headline = strings.TrimSpace(t.Headline)
	t.Overview = strings.TrimSpace(t.Overview)
	t.Outlook = strings.TrimSpace(t.Outlook)
	t.Themes = trimNonEmpty(t.Themes)
	t.Highlights = trimNonEmpty(t.Highlights)
	if !t.any() {
		return empty, fmt.Errorf("empty trend")
	}
	return t, nil
}

func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

type hfDailyTrendReq struct {
	Force bool `json:"force"`
}

func (s *Server) handleHFDailyTrend(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.PathValue("date"))
	if date == "" {
		date = strings.TrimSpace(r.URL.Query().Get("date"))
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
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		rec, ok := daily.loadTrend(parsed)
		if !ok {
			writeJSON(w, http.StatusOK, hfDailyTrendView{OK: true, Date: parsed, HasTrend: false})
			return
		}
		writeJSON(w, http.StatusOK, rec.public(false))
	case http.MethodPost:
		var req hfDailyTrendReq
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && err != io.EOF {
				writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), hfDailyTrendHTTPTimeout)
		defer cancel()
		day, err := daily.ensureDay(ctx, parsed)
		if err != nil {
			log.Printf("papers hf-daily trend date=%s ensure: %v", parsed, err)
			writeErr(w, http.StatusBadGateway, "huggingface daily papers unavailable", "papers", nil, "")
			return
		}
		papers := daily.resolveDayPapers(day)
		if len(papers) == 0 {
			writeErr(w, http.StatusNotFound, "no papers for this date", "papers", nil, "")
			return
		}
		rec, skipped, err := daily.generateTrend(ctx, parsed, papers, req.Force)
		if err != nil {
			if err == errReviewLLMNotReady {
				writeErr(w, http.StatusServiceUnavailable, err.Error(), search.CodeEngine, nil, "")
				return
			}
			log.Printf("papers hf-daily trend date=%s: %v", parsed, err)
			writeErr(w, http.StatusBadGateway, "could not generate trend summary", search.CodeEngine, nil, "")
			return
		}
		writeJSON(w, http.StatusOK, rec.public(skipped))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}
