package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLocalPDFName(t *testing.T) {
	root := t.TempDir()
	pdfDir := filepath.Join(root, "pdfs")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2401.05459_personal_llm_agents_insights_and_survey.pdf"
	if err := os.WriteFile(filepath.Join(pdfDir, name), []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLocalPDFName(root, name)
	if err != nil || got != name {
		t.Fatalf("by filename: got %q %v", got, err)
	}
	got, err = resolveLocalPDFName(root, "2401.05459")
	if err != nil || got != name {
		t.Fatalf("by arxiv: got %q %v", got, err)
	}
	if _, err := resolveLocalPDFName(root, "../etc/passwd"); err == nil {
		t.Fatal("expected traversal reject")
	}
	if _, err := resolveLocalPDFName(root, "missing-id"); err == nil {
		t.Fatal("expected missing")
	}
}

func TestFilterPapers(t *testing.T) {
	in := []paperEntry{
		{Title: "Alpha Security", TopicTags: []string{"security"}, Abstract: "foo", ArxivID: "1.2"},
		{Title: "Beta Survey", TopicTags: []string{"survey"}, Abstract: "bar", ArxivID: "3.4"},
	}
	got := filterPapers(in, "alpha", "")
	if len(got) != 1 || got[0].Title != "Alpha Security" {
		t.Fatalf("%+v", got)
	}
	got = filterPapers(in, "", "survey")
	if len(got) != 1 || got[0].Title != "Beta Survey" {
		t.Fatalf("%+v", got)
	}
}

func TestBriefText(t *testing.T) {
	if briefText("short", 100) != "short" {
		t.Fatal("short")
	}
	s := briefText("one two three four five six seven eight nine ten eleven twelve thirteen", 40)
	if len([]rune(s)) < 10 || !strings.HasSuffix(s, "…") {
		t.Fatalf("brief=%q", s)
	}
}

func TestPapersPageLightTheme(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-theme="light"`) {
		t.Fatal("default theme is not light")
	}
	if !strings.Contains(body, "--bg: #f5f6f8") && !strings.Contains(body, "--bg:#f5f6f8") {
		t.Fatal("missing light background token")
	}
	if strings.Contains(body, `id="base-url"`) || strings.Contains(body, `id="api-key"`) ||
		strings.Contains(body, `id="test-cfg"`) || strings.Contains(body, `id="save-cfg"`) ||
		strings.Contains(body, `id="save-xlate"`) || strings.Contains(body, `id="test-xlate"`) ||
		strings.Contains(body, `id="auto-xlate"`) || strings.Contains(body, `id="qps"`) {
		t.Fatal("LLM settings panel should have moved to /settings")
	}
	if !strings.Contains(body, `id="llm-settings-link"`) || !strings.Contains(body, `href="/settings"`) {
		t.Fatal("papers page should link Translation LLM settings to /settings")
	}
	if !strings.Contains(body, "Configure the translation LLM in") || !strings.Contains(body, "中配置翻译 LLM") {
		t.Fatal("papers page missing EN/ZH note pointing at Settings")
	}
	if !strings.Contains(body, "hasOwnProperty.call(table, k)") {
		t.Fatal("i18n lookup must treat empty strings as valid (EN hintAfter must not leak the key name)")
	}
	if !strings.Contains(body, "中文版") || !strings.Contains(body, "中英对照") {
		t.Fatal("missing translated download labels")
	}
	if !strings.Contains(body, `Authorization`) || !strings.Contains(body, "withToken") {
		t.Fatal("page must forward operator ?token= to API and PDF links")
	}
	if !strings.Contains(body, `id="settings-link"`) {
		t.Fatal("settings link must always be present on papers page")
	}
	if !strings.Contains(body, "canManage") || !strings.Contains(body, "can_manage") {
		t.Fatal("papers page must gate translate UI on can_manage")
	}
	if !strings.Contains(body, "canManage && p.has_local") {
		t.Fatal("translate / re-translate buttons must require canManage")
	}
	if !strings.Contains(body, `id="xlate-banner"`) || !strings.Contains(body, "翻译进行中") {
		t.Fatal("papers page must show page-level translation progress banner")
	}
	if !strings.Contains(body, "POLL_ACTIVE_MS") && !strings.Contains(body, "4000") {
		t.Fatal("expected active polling while translations run")
	}
	if !strings.Contains(body, "PAGE_SIZE") || !strings.Contains(body, "pager-top") || !strings.Contains(body, "pager-bottom") {
		t.Fatal("papers page must paginate with top/bottom controls")
	}
	if !strings.Contains(body, `id="sort"`) || !strings.Contains(body, "sortNewest") {
		t.Fatal("papers page must offer publication-date sort")
	}
	if !strings.Contains(body, "reXlate") || !strings.Contains(body, "再次翻译") {
		t.Fatal("papers page must offer force re-translate control")
	}
	if !strings.Contains(body, "xlate-err") {
		t.Fatal("papers page must surface translate errors")
	}
	if !strings.Contains(body, "authors, abstract, tags") && !strings.Contains(body, "作者、摘要、标签") {
		t.Fatal("search placeholder should cover title/authors/abstract/tags")
	}
	if !strings.Contains(body, "{visits} visits") || !strings.Contains(body, "访问 {visits}") {
		t.Fatal("papers page must i18n public visit count")
	}
	if !strings.Contains(body, "MaClaw Selected papers") || !strings.Contains(body, "码卡龙论文精选") {
		t.Fatal("papers page must use MaClaw / 码卡龙 site title")
	}
	if !strings.Contains(body, "lastCatalog.visits") {
		t.Fatal("papers page must display visits from /papers/api catalog JSON")
	}
}

func TestPapersPageAnonymousOK(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="settings-link"`) || !strings.Contains(body, `href="/settings"`) {
		t.Fatal("anonymous papers page must still show Settings link")
	}
	if strings.Contains(body, `id="username"`) && strings.Contains(body, `id="password"`) && !strings.Contains(body, `id="list"`) {
		t.Fatal("anonymous /papers must not redirect to login HTML")
	}
	if !strings.Contains(body, `id="xlate-pending"`) || !strings.Contains(body, "hidden") {
		t.Fatal("translate-pending should start hidden until can_manage")
	}
}

func TestPapersAPIAnonymousPublic(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.MkdirAll(filepath.Join(dir, "pdfs", "zh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "zh", "2401.05459.zh.pdf"), []byte("%PDF-1.4 zh"), 0o644); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("anonymous /papers/api status=%d body=%s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if cat.CanManage {
		t.Fatal("anonymous can_manage must be false")
	}
	if len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	p := cat.Papers[0]
	if p.ZhPDF != "/papers/pdf/zh/2401.05459" {
		t.Fatalf("anonymous must still see zh_pdf: %+v", p)
	}
	raw := rr.Body.String()
	if strings.Contains(raw, "api_key") || strings.Contains(raw, "APIKey") || strings.Contains(raw, "api-key") {
		t.Fatal("anonymous papers API must not expose translate API keys")
	}
}

func TestPapersAPIAdminCanManage(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if !cat.CanManage {
		t.Fatal("authed admin can_manage must be true")
	}
}

func TestPapersPDFAnonymousOK(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.MkdirAll(filepath.Join(dir, "pdfs", "zh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pdfs", "dual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "zh", "2401.05459.zh.pdf"), []byte("%PDF-1.4 zh-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "dual", "2401.05459.dual.pdf"), []byte("%PDF-1.4 dual-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/papers/pdf/2401.05459",
		"/papers/pdf/zh/2401.05459",
		"/papers/pdf/dual/2401.05459",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Header().Get("Content-Type"), "pdf") {
			t.Fatalf("%s ct=%s", path, rr.Header().Get("Content-Type"))
		}
	}
}

func TestPapersTranslatePostAnonymous401(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2401.05459"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("POST /papers/translate status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSettingsStillGatedFromPapersFlow(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="username"`) || !strings.Contains(body, `id="password"`) {
		t.Fatal("unauthenticated /settings must show login UI")
	}
	if strings.Contains(body, `id="serper"`) || strings.Contains(body, "serper_api_key") {
		t.Fatal("unauthenticated /settings must not show settings form")
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/settings/config", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/settings/config status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func papersAPICatalog(t *testing.T, h http.Handler) papersCatalog {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/papers/api status=%d body=%s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestPapersVisitCounterBumpAndPersist(t *testing.T) {
	h, dir := papersHandler(t)
	if cat := papersAPICatalog(t, h); cat.Visits != 0 {
		t.Fatalf("initial visits=%d", cat.Visits)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/papers", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD /papers status=%d", rr.Code)
	}
	if cat := papersAPICatalog(t, h); cat.Visits != 0 {
		t.Fatalf("HEAD must not increment visits=%d", cat.Visits)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /papers status=%d", rr.Code)
	}
	if cat := papersAPICatalog(t, h); cat.Visits != 1 {
		t.Fatalf("after GET /papers visits=%d", cat.Visits)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "visit-stats.json"))
	if err != nil {
		t.Fatalf("visit-stats.json: %v", err)
	}
	var st visitStatsFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Visits != 1 {
		t.Fatalf("persisted visits=%d body=%s", st.Visits, raw)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/pdf/2401.05459", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PDF status=%d", rr.Code)
	}
	if cat := papersAPICatalog(t, h); cat.Visits != 1 {
		t.Fatalf("PDF download must not increment visits=%d", cat.Visits)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /papers/ status=%d", rr.Code)
	}
	if cat := papersAPICatalog(t, h); cat.Visits != 2 {
		t.Fatalf("after second HTML GET visits=%d", cat.Visits)
	}

	h2 := New(nil, "", nil, nil)
	if cat := papersAPICatalog(t, h2); cat.Visits != 2 {
		t.Fatalf("restart must reload persisted visits=%d", cat.Visits)
	}
}

func TestPapersVisitCounterConcurrent(t *testing.T) {
	h, dir := papersHandler(t)
	const n = 40
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/papers", nil)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				errCh <- fmt.Errorf("GET /papers status=%d", rr.Code)
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if cat := papersAPICatalog(t, h); cat.Visits != n {
		t.Fatalf("concurrent visits=%d want=%d", cat.Visits, n)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "visit-stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st visitStatsFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Visits != n {
		t.Fatalf("persisted concurrent visits=%d want=%d", st.Visits, n)
	}
}
