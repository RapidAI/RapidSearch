package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const hfDailyFixture = `[
  {
    "publishedAt": "2026-09-14T00:00:00.000Z",
    "title": "DataFlex-RL: An Evaluation Platform for RLVR Data Policies",
    "summary": "Data policies for reinforcement learning with verifiable rewards.",
    "paper": {
      "id": "2609.06107",
      "title": "DataFlex-RL: An Evaluation Platform for RLVR Data Policies",
      "summary": "Data policies for reinforcement learning with verifiable rewards.",
      "publishedAt": "2026-09-05T00:00:00.000Z",
      "upvotes": 96,
      "authors": [
        {"name": "Hao Liang"},
        {"name": "Mingrui Chen"}
      ]
    }
  },
  {
    "title": "Bare title fallback",
    "summary": "No nested paper object.",
    "paper": {
      "id": "2609.00001",
      "title": "Bare title fallback",
      "summary": "No nested paper object.",
      "authors": ["Ada Lovelace", {"name": "Alan Turing", "user": {"fullname": "Alan M. Turing"}}]
    }
  }
]`

func TestParseHFDailyDate(t *testing.T) {
	got, err := parseHFDailyDate("2026-09-14")
	if err != nil || got != "2026-09-14" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := parseHFDailyDate(""); err == nil {
		t.Fatal("empty date")
	}
	if _, err := parseHFDailyDate("09/14/2026"); err == nil {
		t.Fatal("slash date")
	}
	if _, err := parseHFDailyDate("2026-13-40"); err == nil {
		t.Fatal("invalid calendar date")
	}
	if _, err := parseHFDailyDate("1999-01-01"); err == nil {
		t.Fatal("too old")
	}
	if _, err := parseHFDailyDate("2099-01-01"); err == nil {
		t.Fatal("far future")
	}
}

func TestHFDailyHTTP400IsEmptyList(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()
	t.Setenv("HF_DAILY_API", ts.URL)
	s := newHFDailyService(nil)
	raw, err := s.fetchRaw(context.Background(), "2026-09-15")
	if err != nil || string(raw) != "[]" {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
}

func TestHFDailyUnpublishedDayIsEmpty(t *testing.T) {
	h, _ := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.daily.fetchFn = func(ctx context.Context, date string) ([]byte, error) {
		return []byte("[]"), nil
	}
	ps.daily.downloadFn = func(ctx context.Context, p paperEntry) (string, error) {
		return "", nil
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-15", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var day hfDailyDayResp
	if err := json.Unmarshal(rr.Body.Bytes(), &day); err != nil {
		t.Fatal(err)
	}
	if !day.OK || day.Count != 0 || len(day.Papers) != 0 {
		t.Fatalf("%+v", day)
	}
}

func TestParseHFDailyPapers(t *testing.T) {
	papers, err := parseHFDailyPapers([]byte(hfDailyFixture), "2026-09-14")
	if err != nil {
		t.Fatal(err)
	}
	if len(papers) != 2 {
		t.Fatalf("len=%d", len(papers))
	}
	p := papers[0]
	if p.ArxivID != "2609.06107" || p.ID != "2609.06107" {
		t.Fatalf("arxiv %+v", p)
	}
	if p.Title == "" || !strings.Contains(p.Abstract, "reinforcement") {
		t.Fatalf("title/abstract %+v", p)
	}
	if len(p.Authors) != 2 || p.Authors[0] != "Hao Liang" {
		t.Fatalf("authors %+v", p.Authors)
	}
	if p.Year != 2026 {
		t.Fatalf("year=%d", p.Year)
	}
	if p.SourceURL != "https://arxiv.org/abs/2609.06107" || !strings.Contains(p.PDFURL, "2609.06107") {
		t.Fatalf("urls %+v", p)
	}
	if !hasTag(p.TopicTags, hfDailyTag) {
		t.Fatalf("tags %+v", p.TopicTags)
	}
	if papers[1].ID != "2609.00001" || len(papers[1].Authors) != 2 {
		t.Fatalf("second %+v", papers[1])
	}
}

func TestParseHFDailyPapersWrappedAndDedup(t *testing.T) {
	raw := `{"papers":[` + strings.TrimSuffix(strings.TrimPrefix(hfDailyFixture, "["), "]") + `,
	  {"paper":{"id":"2609.06107","title":"dup","summary":"dup"}}]}`
	papers, err := parseHFDailyPapers([]byte(raw), "2026-09-14")
	if err != nil {
		t.Fatal(err)
	}
	if len(papers) != 2 {
		t.Fatalf("dedup len=%d", len(papers))
	}
}

func TestNormalizeHFArxivID(t *testing.T) {
	cases := map[string]string{
		"2609.06107":                           "2609.06107",
		"arXiv:2401.05459":                     "2401.05459",
		"https://arxiv.org/abs/2401.05459":     "2401.05459",
		"https://arxiv.org/pdf/2401.05459.pdf": "2401.05459",
		"hep-th/9901001":                       "hep-th/9901001",
		"not-an-id":                            "",
		"":                                     "",
	}
	for in, want := range cases {
		if got := normalizeHFArxivID(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestHFDailyListIngestsIntoCatalog(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.daily.fetchFn = func(ctx context.Context, date string) ([]byte, error) {
		if date != "2026-09-14" {
			t.Fatalf("date=%s", date)
		}
		return []byte(hfDailyFixture), nil
	}
	ps.daily.downloadFn = func(ctx context.Context, p paperEntry) (string, error) {
		return "", nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-14", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("daily status=%d body=%s", rr.Code, rr.Body.String())
	}
	var day hfDailyDayResp
	if err := json.Unmarshal(rr.Body.Bytes(), &day); err != nil {
		t.Fatal(err)
	}
	if !day.OK || day.Date != "2026-09-14" || day.Count != 2 {
		t.Fatalf("%+v", day)
	}
	if day.Papers[0].ID != "2609.06107" || !hasTag(day.Papers[0].TopicTags, hfDailyTag) {
		t.Fatalf("day paper %+v", day.Papers[0])
	}
	if _, err := os.Stat(hfDailyFilePath(dir, "2026-09-14")); err != nil {
		t.Fatal(err)
	}

	// Cached second fetch must not hit the network again.
	var fetches atomic.Int32
	ps.daily.fetchFn = func(ctx context.Context, date string) ([]byte, error) {
		fetches.Add(1)
		return []byte(hfDailyFixture), nil
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-14", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || fetches.Load() != 0 {
		t.Fatalf("cache refetch status=%d fetches=%d", rr.Code, fetches.Load())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range cat.Papers {
		if p.ID == "2609.06107" {
			found = true
			if !hasTag(p.TopicTags, hfDailyTag) {
				t.Fatalf("catalog tags %+v", p.TopicTags)
			}
		}
	}
	if !found {
		t.Fatal("ingested daily paper missing from main catalog")
	}

	// Existing curated paper must still be present and not lose its survey tag.
	kept := false
	for _, p := range cat.Papers {
		if p.ID == "2401.05459" {
			kept = true
			if !hasTag(p.TopicTags, "survey") {
				t.Fatalf("curated tags clobbered %+v", p.TopicTags)
			}
		}
	}
	if !kept {
		t.Fatal("curated catalog paper missing after HF ingest")
	}
}

func TestHFDailyDatesAndBadDate(t *testing.T) {
	h, _ := papersHandler(t)
	srv := h.(*Server)
	srv.papers().daily.now = func() time.Time {
		tm, _ := time.Parse(time.RFC3339, "2026-09-15T12:00:00Z")
		return tm
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/daily/dates", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dates status=%d %s", rr.Code, rr.Body.String())
	}
	var out hfDailyDatesResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Default == "" || len(out.Dates) < 14 {
		t.Fatalf("%+v", out)
	}
	if out.Dates[0].Date != "2026-09-15" {
		t.Fatalf("first date %+v", out.Dates[0])
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/daily/not-a-date", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad date status=%d %s", rr.Code, rr.Body.String())
	}
}

func TestHFDailyReuseTranslateAndReviewRoutes(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.daily.fetchFn = func(ctx context.Context, date string) ([]byte, error) {
		return []byte(hfDailyFixture), nil
	}
	ps.daily.downloadFn = func(ctx context.Context, p paperEntry) (string, error) {
		return "", nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-14", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("daily status=%d %s", rr.Code, rr.Body.String())
	}

	if err := os.WriteFile(filepath.Join(dir, "pdfs", "2609.06107_dataflex.pdf"), []byte("%PDF-1.4 daily"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps.invalidateCatalog()
	ps.xlate.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		return "", "", context.Canceled
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2609.06107"}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("translate status=%d %s", rr.Code, rr.Body.String())
	}
	var resp translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("translate not ok: %s", rr.Body.String())
	}
	queued := strings.Join(resp.Queued, ",")
	if !strings.Contains(queued, "2609.06107") {
		t.Fatalf("daily paper not queued on existing translate route: %s", rr.Body.String())
	}

	paper, ok := ps.findPaper("2609.06107")
	if !ok || paper.Title == "" {
		t.Fatalf("findPaper daily: ok=%v %+v", ok, paper)
	}

	ps.xlate.cfg.APIKey = "test-key"
	ps.xlate.cfg.Model = "test-model"
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		if paper.ID != "2609.06107" {
			t.Fatalf("review paper id=%q", paper.ID)
		}
		return sampleAnalysis(), "test-model", nil
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/review/2609.06107/generate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("review generate status=%d %s", rr.Code, rr.Body.String())
	}
	var view paperReviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasReview || view.PaperID != "2609.06107" {
		t.Fatalf("review view %+v", view)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/review/2609.06107", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("review get status=%d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/review/2609.06107/rate", strings.NewReader(`{"stars":5}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("review rate status=%d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/pdf/2609.06107", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pdf status=%d %s", rr.Code, rr.Body.String())
	}
}

func TestHFDailyTrendGenerateAndPersist(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.daily.fetchFn = func(ctx context.Context, date string) ([]byte, error) {
		return []byte(hfDailyFixture), nil
	}
	ps.daily.downloadFn = func(ctx context.Context, p paperEntry) (string, error) {
		return "", nil
	}
	ps.xlate.cfg.APIKey = "test-key"
	ps.xlate.cfg.Model = "test-model"
	var calls atomic.Int32
	ps.daily.generateFn = func(ctx context.Context, snap translateSnapshot, date string, papers []paperEntry) (hfDailyTrend, error) {
		calls.Add(1)
		if date != "2026-09-14" || len(papers) == 0 {
			t.Fatalf("trend args date=%s n=%d", date, len(papers))
		}
		if snap.APIKey == "" || snap.Model == "" {
			t.Fatal("trend must reuse translate LLM snapshot")
		}
		return hfDailyTrend{
			Headline:   "RL 数据策略成为主线",
			Overview:   "当日工作围绕 RLVR 数据选择与评测。",
			Themes:     []string{"RLVR", "评测平台"},
			Highlights: []string{"DataFlex-RL 对比均匀采样"},
			Outlook:    "后续应扩大种子与基准。",
		}, nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-14/trend", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty trend status=%d %s", rr.Code, rr.Body.String())
	}
	var empty hfDailyTrendView
	if err := json.Unmarshal(rr.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.HasTrend {
		t.Fatal("expected no trend yet")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/daily/2026-09-14/trend", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate status=%d %s", rr.Code, rr.Body.String())
	}
	var view hfDailyTrendView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasTrend || view.Skipped || view.Headline == "" {
		t.Fatalf("%+v", view)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if _, err := os.Stat(hfDailyTrendPath(dir, "2026-09-14")); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/daily/2026-09-14/trend", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second generate status=%d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Skipped || calls.Load() != 1 {
		t.Fatalf("expected persist skip calls=%d skipped=%v", calls.Load(), view.Skipped)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/daily/2026-09-14", nil)
	h.ServeHTTP(rr, req)
	var day hfDailyDayResp
	if err := json.Unmarshal(rr.Body.Bytes(), &day); err != nil {
		t.Fatal(err)
	}
	if !day.HasTrend {
		t.Fatal("day should report has_trend")
	}
}

func TestParseHFDailyTrendFences(t *testing.T) {
	raw := "```json\n{\"headline\":\"甲\",\"overview\":\"乙\",\"themes\":[\"丙\"],\"highlights\":[\"丁\"],\"outlook\":\"戊\"}\n```"
	tr, err := parseHFDailyTrend(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Headline != "甲" || tr.Outlook != "戊" || len(tr.Themes) != 1 {
		t.Fatalf("%+v", tr)
	}
}

func TestHFDailyDirGitignored(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "agent-papers/hf-daily/") {
		t.Fatalf(".gitignore missing hf-daily:\n%s", b)
	}
}

func TestPapersPageHFDailyUI(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{
		"Hugging Face Daily Papers",
		"Hugging Face 每日论文",
		"查看趋势综述",
		"View trend summary",
		`id="tab-daily"`,
		`id="daily-date"`,
		`id="trend-modal"`,
		`id="daily-dates"`,
		"/papers/daily/",
		"hf-daily",
		"openTrendPanel",
		"selectDailyDate",
		`if (q) q.value = ""`,
		"renderPaperCard",
		"/papers/review/",
		"/papers/translate",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("papers.html missing %q", want)
		}
	}
}
