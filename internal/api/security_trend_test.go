package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeSecurityVenue(t *testing.T) {
	cases := []struct {
		in, slug, label string
	}{
		{"IEEE S&P / Oakland", "ieee-sp", "IEEE S&P / Oakland"},
		{"oakland", "ieee-sp", "IEEE S&P / Oakland"},
		{"IEEE Symposium on Security and Privacy 2025", "ieee-sp", "IEEE S&P / Oakland"},
		{"ACM CCS", "acm-ccs", "ACM CCS"},
		{"ccs", "acm-ccs", "ACM CCS"},
		{"USENIX Security", "usenix-security", "USENIX Security"},
		{"Usenix Sec", "usenix-security", "USENIX Security"},
		{"NDSS", "ndss", "NDSS"},
		{"Network and Distributed System Security", "ndss", "NDSS"},
		{"", "other", "Other venue"},
		{"Some Workshop", "other", "Other venue"},
	}
	for _, c := range cases {
		label, slug := normalizeSecurityVenue(c.in)
		if slug != c.slug || label != c.label {
			t.Fatalf("normalize %q: slug=%q label=%q want %q / %q", c.in, slug, label, c.slug, c.label)
		}
	}
}

func TestSecurityTrendPath(t *testing.T) {
	p := securityTrendPath("/tmp/papers", "usenix-security", 2025)
	if filepath.Base(p) != "usenix-security-2025.json" {
		t.Fatalf("base=%s", filepath.Base(p))
	}
	if !strings.Contains(p, "security-trends") {
		t.Fatalf("missing dir: %s", p)
	}
	if got := sanitizeSecuritySlug("USENIX Security"); got != "usenixsecurity" {
		t.Fatalf("sanitizer is not a venue normalizer, got %q", got)
	}
}

func TestGroupSecurityPapers(t *testing.T) {
	in := []paperEntry{
		{Title: "CCS old", Venue: "ACM CCS", Year: 2023, TopicTags: []string{"security-top"}},
		{Title: "USENIX new", Venue: "USENIX Security", Year: 2025, TopicTags: []string{"security-top"}},
		{Title: "Oakland", Venue: "oakland", Year: 2025, TopicTags: []string{"security-top"}},
		{Title: "Workshop", Venue: "Random WS", Year: 2024, TopicTags: []string{"security-top"}},
		{Title: "NDSS", Venue: "NDSS", Year: 2024, TopicTags: []string{"security-top"}},
		{Title: "CCS new", Venue: "CCS", Year: 2025, TopicTags: []string{"security-top"}},
	}
	got := groupSecurityPapers(in)
	if len(got) < 5 {
		t.Fatalf("groups=%d %+v", len(got), got)
	}
	// Venue order: S&P, CCS, USENIX, NDSS, Other; years desc within venue.
	if got[0].Slug != "ieee-sp" || got[0].Year != 2025 || len(got[0].Papers) != 1 {
		t.Fatalf("first group %+v", got[0])
	}
	if got[1].Slug != "acm-ccs" || got[1].Year != 2025 {
		t.Fatalf("ccs 2025 %+v", got[1])
	}
	if got[2].Slug != "acm-ccs" || got[2].Year != 2023 {
		t.Fatalf("ccs 2023 %+v", got[2])
	}
	if got[3].Slug != "usenix-security" || got[3].Year != 2025 {
		t.Fatalf("usenix %+v", got[3])
	}
	if got[len(got)-1].Slug != "other" {
		t.Fatalf("other should be last: %+v", got[len(got)-1])
	}
}

func TestFilterSecurityVenueYear(t *testing.T) {
	in := []paperEntry{
		{Title: "A", Venue: "USENIX Security", Year: 2025, TopicTags: []string{"security-top"}},
		{Title: "B", Venue: "USENIX Security", Year: 2024, TopicTags: []string{"security-top"}},
		{Title: "C", Venue: "ACM CCS", Year: 2025, TopicTags: []string{"security-top"}},
		{Title: "D", Venue: "USENIX Security", Year: 2025, TopicTags: []string{"security"}},
	}
	got := filterSecurityVenueYear(in, "usenix-security", 2025)
	if len(got) != 1 || got[0].Title != "A" {
		t.Fatalf("%+v", got)
	}
	if n := filterSecurityVenueYear(in, "ndss", 2025); len(n) != 0 {
		t.Fatalf("empty want 0 got %+v", n)
	}
}

func writeSecurityManifest(t *testing.T, dir string, papers []paperEntry) {
	t.Helper()
	man := papersManifest{GeneratedAt: "2026-01-01T00:00:00Z", Papers: papers}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityTrendGenerateAuthAndEmpty(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	writeSecurityManifest(t, dir, []paperEntry{
		{Title: "USENIX paper", Venue: "USENIX Security", Year: 2025, TopicTags: []string{"security-top"}, Abstract: "auth", ArxivID: "2501.00001"},
	})
	srv.papers().invalidateCatalog()
	var calls atomic.Int32
	srv.papers().secTrends.generateFn = func(ctx context.Context, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error) {
		calls.Add(1)
		return securityTrend{Headline: "x", Overview: "y"}, nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/security-trend?venue=usenix-security&year=2025", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anon POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("anon POST must not call LLM")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/security-trend?venue=usenix-security&year=2025", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("public GET status=%d %s", rr.Code, rr.Body.String())
	}
	var view securityTrendView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.HasTrend {
		t.Fatal("expected no trend yet")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/security-trend?venue=ndss&year=2025", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty group status=%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no papers for this venue and year") {
		t.Fatalf("empty message: %s", rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("empty group must not call LLM")
	}
}

func TestSecurityTrendGenerateAndPersist(t *testing.T) {
	h, dir := papersHandler(t)
	srv := h.(*Server)
	writeSecurityManifest(t, dir, []paperEntry{
		{
			Title: "USENIX A", Venue: "USENIX Security", Year: 2025,
			TopicTags: []string{"security-top"}, Abstract: "side channels",
			AbstractZH: "侧信道", ArxivID: "2501.00001",
		},
		{
			Title: "USENIX B", Venue: "usenix sec", Year: 2025,
			TopicTags: []string{"security-top", "security"}, Abstract: "fuzzing",
			ArxivID: "2501.00002",
		},
		{
			Title: "CCS A", Venue: "ACM CCS", Year: 2025,
			TopicTags: []string{"security-top"}, Abstract: "other venue",
			ArxivID: "2501.00003",
		},
	})
	srv.papers().invalidateCatalog()
	ps := srv.papers()
	ps.xlate.cfg.APIKey = "test-key"
	ps.xlate.cfg.Model = "test-model"
	var calls atomic.Int32
	ps.secTrends.generateFn = func(ctx context.Context, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error) {
		calls.Add(1)
		if year != 2025 || !strings.Contains(venue, "USENIX") {
			t.Fatalf("args venue=%s year=%d", venue, year)
		}
		if len(papers) != 2 {
			t.Fatalf("want 2 usenix 2025 papers, got %d", len(papers))
		}
		if snap.APIKey == "" || snap.Model == "" {
			t.Fatal("must reuse translate LLM snapshot")
		}
		return securityTrend{
			Headline:   "侧信道与模糊测试并行",
			Overview:   "该年 USENIX 工作围绕侧信道与 fuzzing。",
			Themes:     []string{"侧信道", "模糊测试"},
			Highlights: []string{"USENIX A 讨论侧信道"},
			Outlook:    "后续应交叉验证。",
		}, nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/security-trend?venue=USENIX+Security", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("years status=%d %s", rr.Code, rr.Body.String())
	}
	var years securityVenueYearsResp
	if err := json.Unmarshal(rr.Body.Bytes(), &years); err != nil {
		t.Fatal(err)
	}
	if years.Slug != "usenix-security" || years.DefaultYear != 2025 {
		t.Fatalf("%+v", years)
	}
	found := false
	for _, y := range years.Years {
		if y.Year == 2025 && y.Count == 2 && !y.HasTrend {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing 2025 count: %+v", years.Years)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/security-trend?venue=usenix-security&year=2025", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("generate status=%d %s", rr.Code, rr.Body.String())
	}
	var view securityTrendView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasTrend || view.Skipped || view.Headline == "" || view.PaperCount != 2 {
		t.Fatalf("%+v", view)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if _, err := os.Stat(securityTrendPath(dir, "usenix-security", 2025)); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/security-trend?venue=usenix-security&year=2025", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second generate %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Skipped || calls.Load() != 1 {
		t.Fatalf("expected persist skip calls=%d skipped=%v", calls.Load(), view.Skipped)
	}

	// Failed force regenerate must keep the good cache.
	ps.secTrends.generateFn = func(ctx context.Context, snap translateSnapshot, venue string, year int, papers []paperEntry) (securityTrend, error) {
		calls.Add(1)
		return securityTrend{}, fmt.Errorf("boom")
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/papers/security-trend?venue=usenix-security&year=2025", strings.NewReader(`{"force":true}`))
	req.Header.Set("Content-Type", "application/json")
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("force fail should not be 200: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/security-trend/usenix-security/2025", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get after fail %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.HasTrend || view.Headline != "侧信道与模糊测试并行" {
		t.Fatalf("good trend overwritten: %+v", view)
	}
}

func TestSecurityTrendDirGitignored(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "agent-papers/security-trends/") {
		t.Fatalf(".gitignore missing security-trends:\n%s", b)
	}
}

func TestPapersPageSecurityVenueUI(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{
		`data-section="ieee-sp"`,
		`data-section="acm-ccs"`,
		`data-section="usenix-security"`,
		`data-section="ndss"`,
		"IEEE S&P",
		"ACM CCS",
		"USENIX Security",
		"NDSS",
		`id="venue-tools"`,
		`id="venue-years"`,
		`id="venue-trend-open"`,
		"Research trends",
		"研究趋势",
		"查看趋势综述",
		"/papers/security-trend",
		"setSection",
		"selectVenueYear",
		"openVenueTrend",
		"security-top",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("papers.html missing %q", want)
		}
	}
	if !strings.Contains(body, `tabSP: "IEEE S&P"`) && !strings.Contains(body, "tabSP") {
		t.Fatal("missing IEEE S&P i18n key")
	}
}
