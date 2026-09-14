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

func sampleAnalysis() paperReviewAnalysis {
	return paperReviewAnalysis{
		MethodPrinciples: "原理：用经验驱动共同进化安全策略。",
		MethodEssence:    "本质：把框架与策略放在同一反馈环。",
		Experiment:       "实验：摘要未给出完整消融，完整性有限。",
		Quality:          "总评：方向有价值，证据尚需加强。",
	}
}

func writeReviewFile(t *testing.T, root, id string, rec paperReviewFile) {
	t.Helper()
	if rec.PaperID == "" {
		rec.PaperID = id
	}
	recalcReviewStats(&rec)
	if err := os.MkdirAll(reviewsDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewFilePath(root, id), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertRatingAverage(t *testing.T) {
	var ratings []paperReviewRating
	ratings = upsertRating(ratings, "anon:aaa", 5, "2026-01-01T00:00:00Z")
	ratings = upsertRating(ratings, "anon:bbb", 3, "2026-01-01T00:00:01Z")
	rec := paperReviewFile{Ratings: ratings}
	recalcReviewStats(&rec)
	if rec.RatingCount != 2 || rec.AvgStars != 4 {
		t.Fatalf("first avg: count=%d avg=%v", rec.RatingCount, rec.AvgStars)
	}
	ratings = upsertRating(ratings, "anon:aaa", 1, "2026-01-01T00:00:02Z")
	rec.Ratings = ratings
	recalcReviewStats(&rec)
	if rec.RatingCount != 2 {
		t.Fatalf("upsert must not grow count: %d", rec.RatingCount)
	}
	if rec.AvgStars != 2 {
		t.Fatalf("after upsert avg=%v want 2", rec.AvgStars)
	}
}

func TestReviewRateHTTPUpsert(t *testing.T) {
	h, dir := papersHandler(t)
	writeReviewFile(t, dir, "2401.05459", paperReviewFile{
		Title:    "Personal LLM Agents",
		Analysis: sampleAnalysis(),
	})

	rate := func(cookie, stars string) paperReviewView {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":`+stars+`}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: raterCookieName, Value: cookie})
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("rate %s status=%d body=%s", cookie, rr.Code, rr.Body.String())
		}
		var view paperReviewView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(rr.Body.String(), `"ratings"`) || strings.Contains(rr.Body.String(), "user_key") {
			t.Fatal("public rate response must not include raw rater list")
		}
		return view
	}

	a := rate("aaaaaaaaaaaaaaaa", "5")
	if a.RatingCount != 1 || a.AvgStars != 5 || a.MyStars != 5 {
		t.Fatalf("A first: %+v", a)
	}
	a2 := rate("aaaaaaaaaaaaaaaa", "3")
	if a2.RatingCount != 1 || a2.AvgStars != 3 || a2.MyStars != 3 {
		t.Fatalf("A upsert: %+v", a2)
	}
	b := rate("bbbbbbbbbbbbbbbb", "5")
	if b.RatingCount != 2 || b.AvgStars != 4 {
		t.Fatalf("A+B avg: %+v", b)
	}
	if b.MyStars != 5 {
		t.Fatalf("B my_stars=%d", b.MyStars)
	}
}

func TestGenerateSkipIfExists(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "test-key"
	ps.xlate.cfg.Model = "test-model"
	var calls atomic.Int32
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		calls.Add(1)
		if paper.ID != "2401.05459" {
			t.Fatalf("paper id=%q", paper.ID)
		}
		return sampleAnalysis(), "test-model", nil
	}

	post := func(force bool) (*httptest.ResponseRecorder, paperReviewView) {
		t.Helper()
		body := `{}`
		if force {
			body = `{"force":true}`
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		var view paperReviewView
		_ = json.Unmarshal(rr.Body.Bytes(), &view)
		return rr, view
	}

	rr, view := post(false)
	if rr.Code != http.StatusOK || !view.HasReview || view.Skipped {
		t.Fatalf("first generate status=%d view=%+v body=%s", rr.Code, view, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("first generate calls=%d", calls.Load())
	}

	rateRR := httptest.NewRecorder()
	rateReq := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":4}`))
	rateReq.Header.Set("Content-Type", "application/json")
	rateReq.AddCookie(&http.Cookie{Name: raterCookieName, Value: "cccccccccccccccc"})
	h.ServeHTTP(rateRR, rateReq)
	if rateRR.Code != http.StatusOK {
		t.Fatalf("seed rate status=%d %s", rateRR.Code, rateRR.Body.String())
	}

	rr, view = post(false)
	if rr.Code != http.StatusOK || !view.Skipped {
		t.Fatalf("second generate should skip status=%d skipped=%v body=%s", rr.Code, view.Skipped, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("idempotent generate must not call LLM again, calls=%d", calls.Load())
	}

	rr, view = post(true)
	if rr.Code != http.StatusOK || view.Skipped {
		t.Fatalf("force generate status=%d skipped=%v body=%s", rr.Code, view.Skipped, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("force should regenerate, calls=%d", calls.Load())
	}
	if view.RatingCount != 1 || view.AvgStars != 4 {
		t.Fatalf("force generate must keep ratings: %+v", view)
	}

	if _, err := os.Stat(reviewFilePath(dir, "2401.05459")); err != nil {
		t.Fatal(err)
	}
}

func TestRateBeforeReviewError(t *testing.T) {
	h, dir := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":4}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: raterCookieName, Value: "dddddddddddddddd"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("rate-before-review status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "review is not ready") {
		t.Fatalf("want clear not-ready error, got %s", rr.Body.String())
	}
	if _, err := os.Stat(reviewFilePath(dir, "2401.05459")); !os.IsNotExist(err) {
		t.Fatal("rate must not create a ratings-only stub")
	}
}

func TestGenerateThenRateOK(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "k"
	ps.xlate.cfg.Model = "m"
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		return sampleAnalysis(), "m", nil
	}

	gen := httptest.NewRecorder()
	greq := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(`{}`))
	greq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(gen, greq)
	if gen.Code != http.StatusOK {
		t.Fatalf("generate status=%d %s", gen.Code, gen.Body.String())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":4}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: raterCookieName, Value: "eeeeeeeeeeeeeeee"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rate after generate status=%d %s", rr.Code, rr.Body.String())
	}
	var view paperReviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasReview || view.AvgStars != 4 || view.RatingCount != 1 || view.MyStars != 4 {
		t.Fatalf("after generate+rate: %+v", view)
	}
	if _, err := os.Stat(reviewFilePath(dir, "2401.05459")); err != nil {
		t.Fatal(err)
	}
}

func TestRateWhileGenerateInFlightFailsClosed(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "k"
	ps.xlate.cfg.Model = "m"
	started := make(chan struct{})
	release := make(chan struct{})
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return paperReviewAnalysis{}, "", ctx.Err()
		}
		return sampleAnalysis(), "m", nil
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		done <- rr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("generate did not start")
	}

	rateDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":5}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: raterCookieName, Value: "ffffffffffffffff"})
		h.ServeHTTP(rr, req)
		rateDone <- rr
	}()
	var rateRR *httptest.ResponseRecorder
	select {
	case rateRR = <-rateDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("rate blocked on in-flight generate; must fail closed")
	}
	if rateRR.Code != http.StatusConflict {
		t.Fatalf("in-flight rate status=%d body=%s", rateRR.Code, rateRR.Body.String())
	}
	if !strings.Contains(rateRR.Body.String(), "review is still generating") {
		t.Fatalf("want generating error, got %s", rateRR.Body.String())
	}
	if _, err := os.Stat(reviewFilePath(dir, "2401.05459")); !os.IsNotExist(err) {
		t.Fatal("in-flight rate must not write a ratings stub")
	}

	close(release)
	var genRR *httptest.ResponseRecorder
	select {
	case genRR = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("generate did not finish")
	}
	if genRR.Code != http.StatusOK {
		t.Fatalf("generate status=%d %s", genRR.Code, genRR.Body.String())
	}

	after := httptest.NewRecorder()
	areq := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/rate", strings.NewReader(`{"stars":5}`))
	areq.Header.Set("Content-Type", "application/json")
	areq.AddCookie(&http.Cookie{Name: raterCookieName, Value: "ffffffffffffffff"})
	h.ServeHTTP(after, areq)
	if after.Code != http.StatusOK {
		t.Fatalf("rate after generate completed status=%d %s", after.Code, after.Body.String())
	}
}

func TestGenerateAnonymousWorksWhenLLMConfigured(t *testing.T) {
	h, _ := papersHandler(t)
	srv := h.(*Server)
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "k"
	ps.xlate.cfg.Model = "m"
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		return sampleAnalysis(), snap.Model, nil
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("anonymous generate status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Set-Cookie"), raterCookieName) {
		t.Fatal("generate should set anonymous rater cookie")
	}
}

func TestGenerateRequiresLLM(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestGetReviewPublicHidesRaters(t *testing.T) {
	h, dir := papersHandler(t)
	writeReviewFile(t, dir, "2401.05459", paperReviewFile{
		Title:    "Personal LLM Agents",
		Analysis: sampleAnalysis(),
		Ratings: []paperReviewRating{
			{UserKey: "anon:secret-rater", Stars: 4, At: "2026-01-01T00:00:00Z"},
		},
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/review/2401.05459", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	raw := rr.Body.String()
	if strings.Contains(raw, "secret-rater") || strings.Contains(raw, `"ratings"`) {
		t.Fatalf("leaked raters: %s", raw)
	}
	var view paperReviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasReview || view.AvgStars != 4 || view.RatingCount != 1 {
		t.Fatalf("%+v", view)
	}
	if view.Analysis.MethodPrinciples == "" || view.Analysis.Quality == "" {
		t.Fatalf("missing analysis: %+v", view.Analysis)
	}
}

func TestCatalogReviewFlags(t *testing.T) {
	h, dir := papersHandler(t)
	writeReviewFile(t, dir, "2401.05459", paperReviewFile{
		Title:    "Personal LLM Agents",
		Analysis: sampleAnalysis(),
		Ratings: []paperReviewRating{
			{UserKey: "anon:a", Stars: 5, At: "2026-01-01T00:00:00Z"},
			{UserKey: "anon:b", Stars: 3, At: "2026-01-01T00:00:01Z"},
		},
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	p := cat.Papers[0]
	if !p.HasReview || p.AvgStars != 4 || p.RatingCount != 2 {
		t.Fatalf("catalog flags: %+v", p)
	}
	if strings.Contains(rr.Body.String(), "anon:a") {
		t.Fatal("catalog must not include rater keys")
	}
}

func TestParseReviewAnalysisFences(t *testing.T) {
	raw := "```json\n{\"method_principles\":\"甲\",\"method_essence\":\"乙\",\"experiment\":\"丙\",\"quality\":\"丁\"}\n```"
	a, err := parseReviewAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.MethodPrinciples != "甲" || a.Quality != "丁" {
		t.Fatalf("%+v", a)
	}
}

func TestReviewDirGitignored(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "agent-papers/reviews/") {
		t.Fatalf(".gitignore missing reviews dir:\n%s", text)
	}
}

func TestPapersPageReviewUI(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{
		"生成解读",
		"查看解读",
		"Generate review",
		"View review",
		`id="review-modal"`,
		"/papers/review/",
		"review-btn",
		"方法原理与创新",
		"方法本质",
		"实验完整性",
		"论文质量总评",
		"reviewGenerating",
		"reviewRateLocked",
		"setReviewRatingEnabled",
		"openReview.ready",
		"disabled",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("papers.html missing %q", want)
		}
	}
	if !strings.Contains(body, `esc(t("arxiv")) + "</a>";
    }
    const pid = p.id || p.arxiv_id || "";`) {
		t.Fatal("review button must be rendered after the arXiv action")
	}
}
