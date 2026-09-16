package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withIDs(papers []paperEntry) []paperEntry {
	out := append([]paperEntry(nil), papers...)
	for i := range out {
		out[i].ID = paperTranslateID(out[i])
	}
	return out
}

func TestExtractArxivIDs(t *testing.T) {
	text := "相对 arXiv:2504.15585v2 与 https://arxiv.org/abs/2401.05459，以及 1706.03762 一文。"
	got := extractArxivIDs(text)
	want := map[string]bool{"2504.15585": true, "2401.05459": true, "1706.03762": true}
	if len(got) != len(want) {
		t.Fatalf("ids=%v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected %q in %v", id, got)
		}
	}
}

func TestExtractArxivIDsOldStyle(t *testing.T) {
	got := extractArxivIDs("see hep-th/9901001v2 and nothing else")
	if len(got) != 1 || got[0] != "hep-th/9901001" {
		t.Fatalf("got %v", got)
	}
}

func TestMatchReviewRefsArxivAndTitle(t *testing.T) {
	papers := withIDs([]paperEntry{
		{Title: "Personal LLM Agents", ArxivID: "2401.05459", HasReview: true},
		{Title: "Attention Is All You Need", ArxivID: "1706.03762"},
		{Title: "Tiny", ArxivID: "1234.56789"},
		{Title: "Self Paper", ArxivID: "2504.15585"},
	})
	text := "相对 2401.05459 与 Attention Is All You Need，本文（2504.15585）不引用 Tiny。"
	refs := matchReviewRefs(text, "2504.15585", papers)
	if len(refs) != 2 {
		t.Fatalf("refs=%+v", refs)
	}
	byID := map[string]paperReviewRef{}
	for _, r := range refs {
		byID[r.PaperID] = r
	}
	a := byID["2401.05459"]
	if a.Match != "arxiv_id" || !a.HasReview || a.Title != "Personal LLM Agents" {
		t.Fatalf("arxiv ref: %+v", a)
	}
	b := byID["1706.03762"]
	if b.Match != "title" || b.HasReview {
		t.Fatalf("title ref: %+v", b)
	}
	if _, ok := byID["2504.15585"]; ok {
		t.Fatal("must not link the review's own paper")
	}
	if _, ok := byID["1234.56789"]; ok {
		t.Fatal("short title Tiny must not match")
	}
}

func TestMatchReviewRefsIgnoresMissingCatalog(t *testing.T) {
	papers := withIDs([]paperEntry{
		{Title: "Personal LLM Agents", ArxivID: "2401.05459"},
	})
	text := "我们对比了 1111.22222 与 A Completely Unknown Paper Title About Widgets。"
	refs := matchReviewRefs(text, "2401.05459", papers)
	if len(refs) != 0 {
		t.Fatalf("invented refs: %+v", refs)
	}
}

func TestMatchReviewRefsVersionedAndURL(t *testing.T) {
	papers := withIDs([]paperEntry{
		{Title: "Personal LLM Agents", ArxivID: "2401.05459"},
	})
	text := "详见 https://arxiv.org/pdf/2401.05459v3.pdf"
	refs := matchReviewRefs(text, "other", papers)
	if len(refs) != 1 || refs[0].PaperID != "2401.05459" || refs[0].Match != "arxiv_id" {
		t.Fatalf("%+v", refs)
	}
}

func TestTitleSearchableRejectsShort(t *testing.T) {
	if titleSearchable("Tiny") || titleSearchable("BERT") || titleSearchable("GPT-4") {
		t.Fatal("short titles must not be searchable")
	}
	if !titleSearchable("Personal LLM Agents") {
		t.Fatal("expected catalog title to be searchable")
	}
}

func TestRefreshRefsHTTPAndPersist(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{
				Title:    "Personal LLM Agents",
				Abstract: "A survey of personal agents.",
				Year:     2024,
				ArxivID:  "2401.05459",
			},
			{
				Title:    "Attention Is All You Need",
				Abstract: "Transformers.",
				Year:     2017,
				ArxivID:  "1706.03762",
			},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	srv.papers().invalidateCatalog()

	writeReviewFile(t, dir, "2401.05459", paperReviewFile{
		Title: "Personal LLM Agents",
		Analysis: paperReviewAnalysis{
			MethodPrinciples: "方法借鉴了 Attention Is All You Need 的注意力机制。",
			MethodEssence:    "未见 9999.00000 这篇馆外论文。",
			Experiment:       "实验完整。",
			Quality:          "总评尚可。",
		},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/refs", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var view paperReviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Refs) != 1 || view.Refs[0].PaperID != "1706.03762" || view.Refs[0].Match != "title" {
		t.Fatalf("view refs: %+v", view.Refs)
	}

	b, err := os.ReadFile(reviewFilePath(dir, "2401.05459"))
	if err != nil {
		t.Fatal(err)
	}
	var rec paperReviewFile
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RefsUpdatedAt == "" || len(rec.Refs) != 1 || rec.Refs[0].PaperID != "1706.03762" {
		t.Fatalf("persisted: %+v", rec)
	}

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/papers/review/2401.05459", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get status=%d %s", get.Code, get.Body.String())
	}
	if !strings.Contains(get.Body.String(), `"paper_id":"1706.03762"`) {
		t.Fatalf("get missing persisted ref: %s", get.Body.String())
	}
}

func TestGenerateWritesRefs(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{Title: "Personal LLM Agents", Abstract: "A survey of personal agents.", Year: 2024, ArxivID: "2401.05459"},
			{Title: "Attention Is All You Need", Abstract: "Transformers.", Year: 2017, ArxivID: "1706.03762"},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	srv.papers().invalidateCatalog()
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "k"
	ps.xlate.cfg.Model = "m"
	ps.reviews.generateFn = func(ctx context.Context, snap translateSnapshot, paper paperEntry) (paperReviewAnalysis, string, error) {
		return paperReviewAnalysis{
			MethodPrinciples: "对照 arXiv:1706.03762 的注意力机制。",
			MethodEssence:    "本质清晰。",
			Experiment:       "实验有限。",
			Quality:          "值得跟进。",
		}, "m", nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/generate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var view paperReviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Refs) != 1 || view.Refs[0].PaperID != "1706.03762" || view.Refs[0].Match != "arxiv_id" {
		t.Fatalf("generate refs: %+v body=%s", view.Refs, rr.Body.String())
	}
}

func TestRefreshRefsRequiresReview(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/review/2401.05459/refs", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
}
