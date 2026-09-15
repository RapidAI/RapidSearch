package api

import (
	"encoding/json"
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
		{Title: "Gamma AIoT", TopicTags: []string{"llm-iot"}, Abstract: "llm iot", ArxivID: "5.6"},
	}
	got := filterPapers(in, "alpha", "")
	if len(got) != 1 || got[0].Title != "Alpha Security" {
		t.Fatalf("%+v", got)
	}
	got = filterPapers(in, "", "survey")
	if len(got) != 1 || got[0].Title != "Beta Survey" {
		t.Fatalf("%+v", got)
	}
	got = filterPapers(in, "", "llm-iot")
	if len(got) != 1 || got[0].Title != "Gamma AIoT" {
		t.Fatalf("llm-iot filter: %+v", got)
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
	if !strings.Contains(body, "page_count") || !strings.Contains(body, `class="pages"`) {
		t.Fatal("papers page must render page_count near year/tags")
	}
	if !strings.Contains(body, "Over 100 pages — skipped translation") || !strings.Contains(body, "超过 100 页，不翻译") {
		t.Fatal("papers page must label the over-100-pages skip reason in EN and ZH")
	}
	if !strings.Contains(body, "too_many_pages") || !strings.Contains(body, "TRANSLATE_MAX_PAGES") {
		t.Fatal("papers page must hide translate when too_many_pages")
	}
	if !strings.Contains(body, "paperTooManyPages") || !strings.Contains(body, "xlate-skip") {
		t.Fatal("over-limit papers must show skip reason instead of a silent skip")
	}
	if !strings.Contains(body, "慢速翻译队列") || !strings.Contains(body, "Slow translation queue") {
		t.Fatal("papers page must label the 51–100 page slow lane")
	}
	if !strings.Contains(body, `skipped: "Skipped"`) || !strings.Contains(body, `skipped: "已跳过"`) {
		t.Fatal("papers page must label permanently skipped timeout jobs")
	}
	if !strings.Contains(body, "TRANSLATE_FAST_MAX_PAGES") || !strings.Contains(body, "paperSlowLane") {
		t.Fatal("papers page must classify fast vs slow translate lanes")
	}
	if !strings.Contains(body, `tagChips: "Tags",`) || !strings.Contains(body, `tagChips: "标签",`) {
		t.Fatal("tagChips i18n keys must have trailing commas before reviewView")
	}
	if !strings.Contains(body, `reviewView: "View review"`) || !strings.Contains(body, `reviewView: "查看解读"`) {
		t.Fatal("review overlay i18n must follow tagChips")
	}
	if strings.Contains(body, "let banner =") || strings.Contains(body, "var banner =") {
		t.Fatal("renderTranslateBanner must not redeclare the banner DOM element")
	}
	if !strings.Contains(body, "bannerTitle") {
		t.Fatal("translate banner title string must use bannerTitle")
	}
	if !strings.Contains(body, `id="xlate-banner"`) || !strings.Contains(body, "翻译进行中") {
		t.Fatal("papers page must show page-level translation progress banner")
	}
	if !strings.Contains(body, "white-space: pre-line") {
		t.Fatal("banner-detail must wrap running titles with pre-line")
	}
	detailCSSStart := strings.Index(body, "#xlate-banner .banner-detail")
	if detailCSSStart < 0 {
		t.Fatal("missing banner-detail CSS")
	}
	detailCSS := body[detailCSSStart:]
	if i := strings.Index(detailCSS, "#xlate-banner .banner-meta"); i > 0 {
		detailCSS = detailCSS[:i]
	}
	if strings.Contains(detailCSS, "ellipsis") || strings.Contains(detailCSS, "nowrap") {
		t.Fatal("banner-detail must not ellipsis-truncate or nowrap running titles")
	}
	if !strings.Contains(body, "formatRunningBannerList") || !strings.Contains(body, `(i + 1) + ". "`) {
		t.Fatal("banner must render a numbered multi-line running list")
	}
	if strings.Contains(body, `titles.join(" · ")`) || strings.Contains(body, `ids.join(" · ")`) {
		t.Fatal("running titles must not be joined on one truncated line")
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
	if !strings.Contains(body, "agent自进化") || !strings.Contains(body, "agent安全") ||
		!strings.Contains(body, "agent安全自进化") || !strings.Contains(body, "LLM based 物联网") {
		t.Fatal("papers page must show Chinese category labels")
	}
	if !strings.Contains(body, "llm-iot") || !strings.Contains(body, "LLM-based IoT") {
		t.Fatal("papers page must include llm-iot key and English label")
	}
	if !strings.Contains(body, "llm-training") || !strings.Contains(body, "LLM 训练") ||
		!strings.Contains(body, "LLM training") {
		t.Fatal("papers page must include llm-training key and ZH/EN labels")
	}
	if !strings.Contains(body, "agent-tools-memory") || !strings.Contains(body, "agent工具与记忆") ||
		!strings.Contains(body, "Agent tools & memory") {
		t.Fatal("papers page must include agent-tools-memory key and ZH/EN labels")
	}
	if !strings.Contains(body, `"other"`) || !strings.Contains(body, "其它") ||
		!strings.Contains(body, "Other") {
		t.Fatal("papers page must include other key and ZH/EN labels")
	}
	if !strings.Contains(body, `id="import-open"`) || !strings.Contains(body, `id="import-dialog"`) ||
		!strings.Contains(body, "/papers/import") {
		t.Fatal("papers page must offer public import paper control")
	}
	if !strings.Contains(body, `id="import-pdf"`) || !strings.Contains(body, `accept="application/pdf,.pdf"`) {
		t.Fatal("import dialog must offer PDF upload")
	}
	if !strings.Contains(body, `id="import-tag" required`) && !strings.Contains(body, `id="import-tag" required>`) {
		t.Fatal("category select must stay required for every import path")
	}
	if !strings.Contains(body, "importNeedTag") || !strings.Contains(body, "请选择分类") {
		t.Fatal("import UI must keep the required-category message")
	}
	if !strings.Contains(body, "if (!tag)") {
		t.Fatal("submitImport must reject a missing category before URL or upload")
	}
	if !strings.Contains(body, "FormData") || !strings.Contains(body, `fd.append("tag", tag)`) {
		t.Fatal("upload path must send the required tag in multipart form")
	}
	if !strings.Contains(body, "importChecking") || !strings.Contains(body, "importUploading") {
		t.Fatal("upload UI must show checking and uploading status")
	}
	if !strings.Contains(body, "importErr_not_a_paper") || !strings.Contains(body, "不像传统学术论文") {
		t.Fatal("import UI must localize structure-check failures")
	}
	if !strings.Contains(body, "导入论文") || !strings.Contains(body, "Import paper") {
		t.Fatal("papers page must localize the import button")
	}
	if !strings.Contains(body, `source: "Source"`) || !strings.Contains(body, `source: "来源"`) {
		t.Fatal("paper card source link must use Source / 来源, not arXiv-only copy")
	}
	if strings.Contains(body, `t("arxiv")`) || strings.Contains(body, `arxiv: "arXiv"`) {
		t.Fatal("card action i18n key arxiv should have been renamed to source")
	}
	if !strings.Contains(body, `id="tag-chips"`) {
		t.Fatal("papers page must show tag filter chips")
	}
	if !strings.Contains(body, "已有中文译本 {zh} 篇") || !strings.Contains(body, "{zh} with Chinese PDF") {
		t.Fatal("papers meta line must show catalog-wide Chinese PDF count")
	}
	if !strings.Contains(body, "countCatalogZhPDFs") || !strings.Contains(body, "lastCatalog.papers") {
		t.Fatal("ZH PDF count must come from the full catalog, not the filtered list")
	}
	if !strings.Contains(body, `TAG_KEYS`) || !strings.Contains(body, "tagLabel") {
		t.Fatal("papers page must map stable tag keys to localized labels")
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
