package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"search-service/internal/search"
)

func papersHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAPERS_DIR", dir)
	t.Setenv("SEARCH_TOKEN", "papers-secret")
	t.Setenv("HUB_AUTH_BASES", "http://127.0.0.1:1")
	t.Setenv("SEARCH_CONFIG_PATH", filepath.Join(dir, "search-config.json"))
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	man := papersManifest{
		GeneratedAt: "2026-01-01T00:00:00Z",
		Papers: []paperEntry{
			{
				Title:     "Personal LLM Agents",
				Abstract:  "A survey of personal agents.",
				Year:      2024,
				ArxivID:   "2401.05459",
				TopicTags: []string{"survey"},
				PDFPath:   "pdfs/2401.05459_personal_llm_agents.pdf",
			},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "2401.05459_personal_llm_agents.pdf"), []byte("%PDF-1.4 orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(nil, "", nil, nil)
	t.Cleanup(func() { search.ActivateStore(nil) })
	return h, dir
}

func papersAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer papers-secret")
}

func TestMaskTranslateKey(t *testing.T) {
	m := maskAPIKey("abcdXYZQ")
	if !m.Configured || m.Last4 != "XYZQ" {
		t.Fatalf("%+v", m)
	}
	if maskAPIKey("").Configured {
		t.Fatal("empty")
	}
	raw, _ := json.Marshal(TranslatePublicView{OK: true, APIKey: maskAPIKey("super-secret-key-9999")})
	if strings.Contains(string(raw), "super-secret-key-9999") {
		t.Fatal("public view leaked key")
	}
}

func TestTranslateConfigEmptyPUTDoesNotWipe(t *testing.T) {
	h, dir := papersHandler(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(body))
		papersAuth(req)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := put(`{"base_url":"https://example.test/v1","model":"demo-model","api_key":"keep-this-key-4242"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("set status=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "keep-this-key-4242") {
		t.Fatal("PUT response leaked raw key")
	}
	rr = put(`{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty status=%d %s", rr.Code, rr.Body.String())
	}
	rr = put(`{"api_key":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("blank status=%d %s", rr.Code, rr.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "translate-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "keep-this-key-4242") {
		t.Fatalf("key wiped: %s", onDisk)
	}
	if strings.Contains(rr.Body.String(), "keep-this-key-4242") {
		t.Fatal("GET-after-PUT leaked raw key")
	}
	var view TranslatePublicView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.APIKey.Configured || view.APIKey.Last4 != "4242" {
		t.Fatalf("%+v", view.APIKey)
	}
	if view.BaseURL != "https://example.test/v1" || view.Model != "demo-model" {
		t.Fatalf("%+v", view)
	}
}

func TestTranslateConfigClearKey(t *testing.T) {
	h, dir := papersHandler(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(body))
		papersAuth(req)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := put(`{"api_key":"wipe-me-soon"}`); rr.Code != http.StatusOK {
		t.Fatalf("%s", rr.Body.String())
	}
	rr := put(`{"clear_api_key":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s", rr.Body.String())
	}
	var view TranslatePublicView
	_ = json.Unmarshal(rr.Body.Bytes(), &view)
	if view.APIKey.Configured {
		t.Fatal("clear should wipe")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "translate-config.json"))
	if strings.Contains(string(raw), "wipe-me-soon") {
		t.Fatal("file still has key")
	}
}

func TestTranslateConfigUnauthenticated401(t *testing.T) {
	h, _ := papersHandler(t)
	for _, path := range []string{"/papers/translate/config", "/settings/translate"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestSettingsTranslateConfigAlias(t *testing.T) {
	h, dir := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/settings/translate", strings.NewReader(
		`{"base_url":"https://example.test/v1","model":"demo-model","api_key":"settings-path-key-8888"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "settings-path-key-8888") {
		t.Fatal("settings translate leaked raw key")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "translate-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "settings-path-key-8888") {
		t.Fatalf("not persisted: %s", raw)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/settings/translate", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d %s", rr.Code, rr.Body.String())
	}
	var view TranslatePublicView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.APIKey.Configured || view.APIKey.Last4 != "8888" || view.Model != "demo-model" {
		t.Fatalf("%+v", view)
	}
}

func TestTranslateStatusJSON(t *testing.T) {
	dir := t.TempDir()
	svc := newTranslateService(dir)
	svc.putJob(translateJob{ID: "2401.05459", Status: translateQueued})
	raw, err := os.ReadFile(filepath.Join(dir, "translate_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status": "queued"`) {
		t.Fatalf("%s", raw)
	}
	svc2 := newTranslateService(dir)
	j, ok := svc2.job("2401.05459")
	if !ok || j.Status != translateQueued {
		t.Fatalf("reload %+v %v", j, ok)
	}
}

func TestResolveTranslatedPDFName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pdfs", "zh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pdfs", "dual"), 0o755); err != nil {
		t.Fatal(err)
	}
	zh := filepath.Join(root, "pdfs", "zh", "2401.05459.zh.pdf")
	dual := filepath.Join(root, "pdfs", "dual", "2401.05459.dual.pdf")
	if err := os.WriteFile(zh, []byte("%PDF-1.4 zh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dual, []byte("%PDF-1.4 dual"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, name, err := resolveTranslatedPDFName(root, "zh", "2401.05459")
	if err != nil || name != "2401.05459.zh.pdf" || !strings.HasSuffix(got, name) {
		t.Fatalf("zh: %q %q %v", got, name, err)
	}
	got, name, err = resolveTranslatedPDFName(root, "dual", "2401.05459.dual.pdf")
	if err != nil || name != "2401.05459.dual.pdf" {
		t.Fatalf("dual: %q %q %v", got, name, err)
	}
	if _, _, err := resolveTranslatedPDFName(root, "zh", "../etc/passwd"); err == nil {
		t.Fatal("expected traversal reject")
	}
	if _, _, err := resolveTranslatedPDFName(root, "zh", "missing-id"); err == nil {
		t.Fatal("expected missing")
	}
}

func TestCollectBabelOutputsPrefersNoWatermark(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "paper.zh-CN.mono.pdf"), []byte("m"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "paper.no_watermark.zh-CN.mono.pdf"), []byte("mnw"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "paper.zh-CN.dual.pdf"), []byte("d"), 0o644)
	mono, dual := collectBabelOutputs(dir)
	if !strings.Contains(mono, "no_watermark") {
		t.Fatalf("mono=%s", mono)
	}
	if !strings.Contains(dual, "dual") {
		t.Fatalf("dual=%s", dual)
	}
}

func TestPaperTranslateID(t *testing.T) {
	if got := paperTranslateID(paperEntry{ArxivID: "arXiv:2401.05459"}); got != "2401.05459" {
		t.Fatalf("%s", got)
	}
	if got := paperTranslateID(paperEntry{ArxivID: "hep-th/9901001"}); got != "hep-th_9901001" {
		t.Fatalf("%s", got)
	}
	if got := paperTranslateID(paperEntry{Filename: "foo_bar.pdf"}); got != "foo_bar" {
		t.Fatalf("%s", got)
	}
}

func TestSanitizeUserErrorRedactsKey(t *testing.T) {
	got := sanitizeUserError("boom sk-abcDEF1234567890 extra")
	if strings.Contains(got, "sk-abcDEF1234567890") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("%s", got)
	}
}

func TestPapersAPIIncludesTranslateFields(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.MkdirAll(filepath.Join(dir, "pdfs", "zh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "zh", "2401.05459.zh.pdf"), []byte("%PDF-1.4 zh"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	p := cat.Papers[0]
	if p.ID != "2401.05459" || p.ZhPDF != "/papers/pdf/zh/2401.05459" {
		t.Fatalf("%+v", p)
	}
	if p.DualPDF != "" {
		t.Fatalf("unexpected dual %s", p.DualPDF)
	}
}

func TestPapersPDFServesTranslated(t *testing.T) {
	h, dir := papersHandler(t)
	if err := os.MkdirAll(filepath.Join(dir, "pdfs", "dual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdfs", "dual", "2401.05459.dual.pdf"), []byte("%PDF-1.4 dual-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/pdf/2401.05459", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "orig") {
		t.Fatalf("original status=%d body=%q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/pdf/dual/2401.05459", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "pdf") {
		t.Fatalf("ct=%s", rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), "dual-bytes") {
		t.Fatalf("body=%q", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/pdf/zh/missing-id", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTranslateTestDoesNotLeakKey(t *testing.T) {
	h, _ := papersHandler(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer super-secret-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key super-secret-test-key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"demo-model"}]}`)
	}))
	defer hub.Close()

	body := `{"base_url":"` + hub.URL + `","api_key":"super-secret-test-key","model":"demo-model"}`
	for _, path := range []string{"/papers/translate/test", "/settings/translate/test"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		papersAuth(req)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d %s", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "super-secret-test-key") {
			t.Fatalf("%s leaked key", path)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["ok"] != true || out["via"] != "models" {
			t.Fatalf("%s %+v", path, out)
		}
	}
}

func TestTranslateEnqueueFakeRunner(t *testing.T) {
	h, dir := papersHandler(t)
	s, ok := h.(*Server)
	if !ok {
		t.Fatal("handler type")
	}
	// Need a configured key so the worker will invoke the runner.
	rr := httptest.NewRecorder()
	preq := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(
		`{"model":"demo","api_key":"not-a-real-key-zzzz"}`))
	papersAuth(preq)
	preq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, preq)
	if rr.Code != http.StatusOK {
		t.Fatalf("config %d %s", rr.Code, rr.Body.String())
	}

	svc := s.translate()
	done := make(chan struct{})
	svc.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		_ = ctx
		if job.Cfg.APIKey != "" && strings.Contains(job.Cfg.APIKey, "not-a-real") {
			// key is available to the runner but must not appear in HTTP responses.
		}
		zh, dual, err := installTranslatedPDFs(job.Root, job.ID, job.InputPDF, job.InputPDF)
		close(done)
		return zh, dual, err
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2401.05459"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enqueue %d %s", rr.Code, rr.Body.String())
	}
	var enq translateEnqueueResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if !enq.OK || len(enq.Queued) != 1 {
		t.Fatalf("%+v", enq)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not run")
	}
	// Status file + outputs should settle shortly after the runner returns.
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, ok := svc.job("2401.05459")
		if ok && j.Status == translateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v ok=%v", j, ok)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "pdfs", "zh", "2401.05459.zh.pdf")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pdfs", "dual", "2401.05459.dual.pdf")); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	papersAuth(req)
	h.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), "not-a-real-key-zzzz") {
		t.Fatal("catalog leaked key")
	}
	if !strings.Contains(rr.Body.String(), `"zh_pdf":"/papers/pdf/zh/2401.05459"`) {
		t.Fatalf("missing zh link: %s", rr.Body.String())
	}
}

func TestSummarizeTranslateProgress(t *testing.T) {
	papers := []paperEntry{
		{ID: "a", Title: "Alpha", TranslateStatus: translateQueued},
		{ID: "b", Title: "Beta Running", TranslateStatus: translateRunning},
		{ID: "c", Title: "Gamma", TranslateStatus: translateDone},
		{ID: "d", Title: "Delta", TranslateStatus: translateQueued},
		{ID: "e", Title: "Epsilon", TranslateStatus: translateFailed},
		{ID: "f", Title: "Zeta Also", TranslateStatus: translateRunning},
	}
	p := summarizeTranslateProgress(papers)
	if p == nil || !p.Active || p.Queued != 2 || p.Running != 2 {
		t.Fatalf("%+v", p)
	}
	if p.RunningID != "b" || p.RunningTitle != "Beta Running" {
		t.Fatalf("running title %+v", p)
	}
	if len(p.RunningIDs) != 2 || p.RunningIDs[0] != "b" || p.RunningIDs[1] != "f" {
		t.Fatalf("running_ids %+v", p.RunningIDs)
	}
	if len(p.RunningTitles) != 2 || p.RunningTitles[0] != "Beta Running" || p.RunningTitles[1] != "Zeta Also" {
		t.Fatalf("running_titles %+v", p.RunningTitles)
	}
	idle := summarizeTranslateProgress([]paperEntry{{ID: "x", TranslateStatus: translateDone}})
	if idle == nil || idle.Active || idle.Queued != 0 || idle.Running != 0 {
		t.Fatalf("idle %+v", idle)
	}
}

func TestPapersAPIIncludesTranslateProgress(t *testing.T) {
	h, _ := papersHandler(t)
	srv, ok := h.(*Server)
	if !ok {
		t.Fatalf("handler type %T", h)
	}
	svc := srv.papers().translate()
	svc.putJob(translateJob{ID: "2401.05459", Status: translateRunning})
	svc.putJob(translateJob{ID: "queued-other", Status: translateQueued})
	svc.mu.Lock()
	svc.running["2401.05459"] = true // prevent reconcileExternal from clearing synthetic running
	svc.mu.Unlock()

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
	if cat.TranslateProgress == nil || !cat.TranslateProgress.Active {
		t.Fatalf("missing progress %+v", cat.TranslateProgress)
	}
	if cat.TranslateProgress.Running < 1 {
		t.Fatalf("expected running in progress %+v", cat.TranslateProgress)
	}
	if cat.TranslateProgress.RunningTitle == "" && cat.TranslateProgress.RunningID == "" {
		t.Fatalf("expected running identity %+v", cat.TranslateProgress)
	}
}

func TestEnqueueSkipsDoneUnlessForce(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pdfs", "zh"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "pdfs", "dual"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "demo.pdf"), []byte("%PDF"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "zh", "demo.zh.pdf"), []byte("%PDF zh"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "dual", "demo.dual.pdf"), []byte("%PDF dual"), 0o644)

	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()
	svc.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		return installTranslatedPDFs(job.Root, job.ID, job.InputPDF, job.InputPDF)
	}
	papers := []paperEntry{{
		ID: "demo", HasLocal: true, Filename: "demo.pdf",
		ZhPDF: "/papers/pdf/zh/demo", DualPDF: "/papers/pdf/dual/demo",
		TranslateStatus: translateDone,
	}}

	res := svc.enqueue([]string{"demo"}, papers, false)
	if len(res.Queued) != 0 {
		t.Fatalf("auto/normal must skip done: %+v", res)
	}

	res = svc.enqueue([]string{"demo"}, papers, true)
	if len(res.Queued) != 1 {
		t.Fatalf("force must re-queue done: %+v", res)
	}
	if outputsExist(dir, "demo") {
		t.Fatal("force should clear prior outputs before run")
	}
}

func TestEnqueueRejectsWhileRunning(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "demo.pdf"), []byte("%PDF"), 0o644)
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true // do not start background loop
	svc.running["demo"] = true
	svc.status.Jobs["demo"] = translateJob{ID: "demo", Status: translateRunning}
	svc.mu.Unlock()

	res := svc.enqueue([]string{"demo"}, []paperEntry{{ID: "demo", HasLocal: true, Filename: "demo.pdf"}}, true)
	if len(res.Queued) != 0 {
		t.Fatalf("must not queue while running: %+v", res)
	}
	if res.Rejected["demo"] != "正在翻译中" {
		t.Fatalf("rejected=%v", res.Rejected)
	}
}

func TestEnqueueSkipsDuplicateQueued(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "demo.pdf"), []byte("%PDF"), 0o644)
	svc := newTranslateService(dir)
	// Prevent background loop from consuming the queue during the test.
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()

	papers := []paperEntry{{ID: "demo", HasLocal: true, Filename: "demo.pdf"}}
	res1 := svc.enqueue([]string{"demo"}, papers, false)
	if len(res1.Queued) != 1 {
		t.Fatalf("first %+v", res1)
	}
	res2 := svc.enqueue([]string{"demo"}, papers, false)
	if len(res2.Queued) != 0 || len(res2.Skipped) != 1 {
		t.Fatalf("dup %+v", res2)
	}
	svc.mu.Lock()
	n := 0
	for _, id := range svc.queue {
		if id == "demo" {
			n++
		}
	}
	svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("queue copies=%d", n)
	}
}

func TestParseTranslateConcurrency(t *testing.T) {
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "")
	if got := parseTranslateConcurrency(); got != 3 {
		t.Fatalf("default=%d", got)
	}
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "2")
	if got := parseTranslateConcurrency(); got != 2 {
		t.Fatalf("2=%d", got)
	}
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "0")
	if got := parseTranslateConcurrency(); got != 1 {
		t.Fatalf("min=%d", got)
	}
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "99")
	if got := parseTranslateConcurrency(); got != 8 {
		t.Fatalf("max=%d", got)
	}
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "bogus")
	if got := parseTranslateConcurrency(); got != 3 {
		t.Fatalf("invalid=%d", got)
	}
}

func TestTranslateConcurrencyStartsMultiple(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAPERS_TRANSLATE_CONCURRENCY", "3")
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	ids := []string{"p1", "p2", "p3", "p4"}
	var papers []paperEntry
	for _, id := range ids {
		if err := os.WriteFile(filepath.Join(dir, "pdfs", id+".pdf"), []byte("%PDF "+id), 0o644); err != nil {
			t.Fatal(err)
		}
		papers = append(papers, paperEntry{ID: id, HasLocal: true, Filename: id + ".pdf"})
	}

	svc := newTranslateService(dir)
	if svc.concurrency != 3 {
		t.Fatalf("concurrency=%d", svc.concurrency)
	}
	svc.mu.Lock()
	svc.cfg.APIKey = "not-a-real-key-zzzz"
	svc.cfg.Model = "demo"
	svc.mu.Unlock()
	started := make(chan string, 8)
	release := make(chan struct{})
	var live atomic.Int32
	var maxLive atomic.Int32
	svc.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		n := live.Add(1)
		for {
			old := maxLive.Load()
			if n <= old || maxLive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- job.ID
		<-release
		live.Add(-1)
		return installTranslatedPDFs(job.Root, job.ID, job.InputPDF, job.InputPDF)
	}
	svc.start()

	res := svc.enqueue(ids, papers, false)
	if len(res.Queued) != 4 {
		t.Fatalf("queued %+v", res)
	}

	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case id := <-started:
			got[id] = true
		case <-deadline:
			t.Fatalf("only started %d: %v", len(got), got)
		}
	}
	select {
	case id := <-started:
		t.Fatalf("4th started too early: %s", id)
	case <-time.After(250 * time.Millisecond):
	}
	if maxLive.Load() != 3 {
		t.Fatalf("maxLive=%d", maxLive.Load())
	}
	pub := svc.queuePublic()
	if n, _ := pub["concurrency"].(int); n != 3 {
		t.Fatalf("queue concurrency %+v", pub["concurrency"])
	}
	runningIDs, _ := pub["running_ids"].([]string)
	if len(runningIDs) != 3 {
		t.Fatalf("running_ids=%v", runningIDs)
	}
	if first, _ := pub["running"].(string); first == "" {
		t.Fatal("backward-compat running empty")
	}

	same := svc.enqueue([]string{"p1"}, papers, true)
	if same.Rejected["p1"] != "正在翻译中" {
		t.Fatalf("same-id %+v", same)
	}

	close(release)
	select {
	case id := <-started:
		got[id] = true
	case <-time.After(3 * time.Second):
		t.Fatal("4th never started")
	}
	if len(got) != 4 {
		t.Fatalf("started set %v", got)
	}
	deadline2 := time.Now().Add(3 * time.Second)
	for _, id := range ids {
		for {
			j, ok := svc.job(id)
			if ok && j.Status == translateDone {
				break
			}
			if time.Now().After(deadline2) {
				t.Fatalf("%s status %+v ok=%v", id, j, ok)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestBabelDOCArgsOmitAPIKey(t *testing.T) {
	args := []string{"--openai", "--openai-model", "auto", "--files", "x.pdf"}
	if cmdlineHasAPIKeyFlag(append(args, "--openai-api-key", "secret")) != true {
		t.Fatal("detector")
	}
	if cmdlineHasAPIKeyFlag(args) {
		t.Fatal("clean args flagged")
	}
}
