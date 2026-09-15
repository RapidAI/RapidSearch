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
	"testing"
	"time"
)

func TestNormalizeTranslateEngine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", translateEngineHub, false},
		{"hub", translateEngineHub, false},
		{"OpenAI", translateEngineHub, false},
		{"llm", translateEngineHub, false},
		{"google", translateEngineGoogle, false},
		{"Google_Translate", translateEngineGoogle, false},
		{"bing", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeTranslateEngine(tc.in)
		if tc.err {
			if err == nil {
				t.Fatalf("%q: expected error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q %v want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestSnapshotPDFReadyByEngine(t *testing.T) {
	t.Parallel()
	hubMissing := translateSnapshot{Engine: translateEngineHub, Model: "x"}
	if hubMissing.pdfReady() || hubMissing.ready() || hubMissing.llmReady() {
		t.Fatalf("hub without key should not be ready: %+v", hubMissing)
	}
	hubOK := translateSnapshot{Engine: translateEngineHub, APIKey: "k", Model: "m"}
	if !hubOK.pdfReady() || !hubOK.ready() {
		t.Fatalf("hub with key should be ready")
	}
	googleNoKey := translateSnapshot{Engine: translateEngineGoogle}
	if !googleNoKey.pdfReady() {
		t.Fatal("google engine is pdf-ready without a Cloud key (web path)")
	}
	if googleNoKey.ready() {
		t.Fatal("google engine must not flip Hub LLM ready() for abstracts/reviews")
	}
}

func TestTranslateConfigPersistsEngine(t *testing.T) {
	h, dir := papersHandler(t)
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(body))
		papersAuth(req)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := put(`{"engine":"google","google_api_key":"gcloud-secret-key-9999","model":"keep-hub","api_key":"hub-secret-key-4242"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("set status=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "gcloud-secret-key-9999") || strings.Contains(rr.Body.String(), "hub-secret-key-4242") {
		t.Fatal("PUT leaked raw keys")
	}
	var view TranslatePublicView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Engine != translateEngineGoogle {
		t.Fatalf("engine=%q", view.Engine)
	}
	if !view.GoogleAPIKey.Configured || view.GoogleAPIKey.Last4 != "9999" {
		t.Fatalf("google mask %+v", view.GoogleAPIKey)
	}
	if !view.APIKey.Configured || view.APIKey.Last4 != "4242" {
		t.Fatalf("hub mask %+v", view.APIKey)
	}

	rr = put(`{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty %s", rr.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "translate-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"engine": "google"`) {
		t.Fatalf("engine not persisted: %s", onDisk)
	}
	if !strings.Contains(string(onDisk), "gcloud-secret-key-9999") || !strings.Contains(string(onDisk), "hub-secret-key-4242") {
		t.Fatalf("keys wiped: %s", onDisk)
	}

	rr = put(`{"engine":"hub"}`)
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Engine != translateEngineHub {
		t.Fatalf("switch back %q", view.Engine)
	}

	rr = put(`{"clear_google_api_key":true}`)
	_ = json.Unmarshal(rr.Body.Bytes(), &view)
	if view.GoogleAPIKey.Configured {
		t.Fatal("clear google key")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "translate-config.json"))
	if strings.Contains(string(raw), "gcloud-secret-key-9999") {
		t.Fatal("google key still on disk")
	}
	if !strings.Contains(string(raw), "hub-secret-key-4242") {
		t.Fatal("hub key should remain")
	}
}

func TestTranslateConfigRejectsUnknownEngine(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(`{"engine":"bing"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
}

func TestGoogleEngineRoutesPDFJobWithoutHubKey(t *testing.T) {
	h, dir := papersHandler(t)
	s, ok := h.(*Server)
	if !ok {
		t.Fatal("handler type")
	}
	rr := httptest.NewRecorder()
	preq := httptest.NewRequest(http.MethodPut, "/papers/translate/config", strings.NewReader(`{"engine":"google"}`))
	papersAuth(preq)
	preq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, preq)
	if rr.Code != http.StatusOK {
		t.Fatalf("config %d %s", rr.Code, rr.Body.String())
	}

	svc := s.translate()
	if snap := svc.snapshot(); snap.pdfEngine() != translateEngineGoogle || !snap.pdfReady() || snap.ready() {
		t.Fatalf("snapshot %+v", snap)
	}

	seen := make(chan translateRun, 1)
	svc.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		_ = ctx
		seen <- job
		return installTranslatedPDFs(job.Root, job.ID, job.InputPDF, job.InputPDF)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate", strings.NewReader(`{"id":"2401.05459"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enqueue %d %s", rr.Code, rr.Body.String())
	}

	select {
	case job := <-seen:
		if job.Cfg.pdfEngine() != translateEngineGoogle {
			t.Fatalf("runner engine=%q", job.Cfg.pdfEngine())
		}
		if job.Cfg.APIKey != "" {
			t.Fatal("google job should not require Hub key")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not run")
	}

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
}

func TestHubEngineStillRequiresKey(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pdfs", "demo.pdf"), []byte("%PDF"), 0o644)
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.cfg.Engine = translateEngineHub
	svc.cfg.Model = "demo"
	svc.mu.Unlock()
	called := false
	svc.runner = func(ctx context.Context, job translateRun) (string, string, error) {
		called = true
		return "", "", nil
	}
	svc.runOne("demo")
	if called {
		t.Fatal("hub engine must not start BabelDOC without api_key")
	}
	j, ok := svc.job("demo")
	if !ok || j.Status != translateFailed || !strings.Contains(j.Error, "api_key") {
		t.Fatalf("%+v ok=%v", j, ok)
	}
}

func TestExtractBabelDOCInput(t *testing.T) {
	t.Parallel()
	user := ";; Treat next line as plain text input and translate it into zh-CN, output translation ONLY. Input:\n\nHello {{1}} world"
	if got := extractBabelDOCInput(user); got != "Hello {{1}} world" {
		t.Fatalf("%q", got)
	}
	if got := extractBabelDOCInput("just text"); got != "just text" {
		t.Fatalf("%q", got)
	}
}

func TestProtectPlaceholders(t *testing.T) {
	t.Parallel()
	in := "See {{1}} and {v2} here"
	prot, tokens := protectPlaceholders(in)
	if strings.Contains(prot, "{{1}}") || strings.Contains(prot, "{v2}") {
		t.Fatalf("not protected: %s", prot)
	}
	if restorePlaceholders(prot, tokens) != in {
		t.Fatalf("roundtrip %q", restorePlaceholders(prot, tokens))
	}
}

func TestParseGoogleGTX(t *testing.T) {
	t.Parallel()
	raw := []byte(`[[["你好","Hello",null,null,10]],null,"en"]`)
	got, err := parseGoogleGTX(raw)
	if err != nil || got != "你好" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestGoogleTranslateClientCloudAndWeb(t *testing.T) {
	var sawCloud, sawGTX bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/language/translate/v2"):
			sawCloud = true
			if r.URL.Query().Get("key") != "cloud-key-zzzz" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"translations":[{"translatedText":"你好 {{1}}"}]}}`)
		case strings.HasPrefix(r.URL.Path, "/translate_a/single"):
			sawGTX = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[[["网页译文","hello",null,null,10]],null,"en"]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	origCloud, origGTX, origMobile := googleCloudTranslateURL, googleGTXTranslateURL, googleMobileTranslateURL
	googleCloudTranslateURL = hub.URL + "/language/translate/v2"
	googleGTXTranslateURL = hub.URL + "/translate_a/single"
	googleMobileTranslateURL = hub.URL + "/m"
	t.Cleanup(func() {
		googleCloudTranslateURL = origCloud
		googleGTXTranslateURL = origGTX
		googleMobileTranslateURL = origMobile
	})

	ctx := context.Background()
	cloud := newGoogleTranslateClient(hub.Client(), "cloud-key-zzzz")
	out, err := cloud.translate(ctx, "hello {{1}}", "en", "zh-CN")
	if err != nil || !strings.Contains(out, "你好") || !strings.Contains(out, "{{1}}") {
		t.Fatalf("cloud %q %v", out, err)
	}
	if !sawCloud {
		t.Fatal("expected cloud request")
	}

	web := newGoogleTranslateClient(hub.Client(), "")
	out, err = web.translate(ctx, "hello", "en", "zh-CN")
	if err != nil || out != "网页译文" {
		t.Fatalf("web %q %v", out, err)
	}
	if !sawGTX {
		t.Fatal("expected gtx request")
	}
}

func TestGoogleOpenAIShimChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[[["机器译文","x",null,null,1]],null,"en"]`)
	}))
	defer upstream.Close()
	origGTX, origMobile, origCloud := googleGTXTranslateURL, googleMobileTranslateURL, googleCloudTranslateURL
	googleGTXTranslateURL = upstream.URL
	googleMobileTranslateURL = upstream.URL + "/m"
	googleCloudTranslateURL = upstream.URL + "/cloud"
	t.Cleanup(func() {
		googleGTXTranslateURL = origGTX
		googleMobileTranslateURL = origMobile
		googleCloudTranslateURL = origCloud
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, stop, err := startGoogleOpenAIShim(ctx, upstream.Client(), "", "en", "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	payload, _ := json.Marshal(map[string]any{
		"model": "google-translate",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a translator"},
			{"role": "user", "content": ";; Treat next line as plain text input and translate it into zh-CN. Input:\n\nHello papers"},
		},
	})
	req, err := http.NewRequest(http.MethodPost, base+"/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "机器译文") {
		t.Fatalf("shim body=%s", body)
	}
	if strings.Contains(string(body), "Treat next line") {
		t.Fatal("shim translated the prompt wrapper")
	}
}

func TestTranslateTestGoogleEngine(t *testing.T) {
	h, _ := papersHandler(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[[["你好","hello",null,null,1]],null,"en"]`)
	}))
	defer upstream.Close()
	origGTX, origMobile := googleGTXTranslateURL, googleMobileTranslateURL
	googleGTXTranslateURL = upstream.URL
	googleMobileTranslateURL = upstream.URL + "/m"
	t.Cleanup(func() {
		googleGTXTranslateURL = origGTX
		googleMobileTranslateURL = origMobile
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/translate/test", strings.NewReader(`{"engine":"google"}`))
	papersAuth(req)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "api_key") && strings.Contains(rr.Body.String(), "sk-") {
		t.Fatal("leaked key")
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["engine"] != translateEngineGoogle || out["via"] != "google_web" {
		t.Fatalf("%+v", out)
	}
}

func TestPythonWorkerArgsIncludeEngine(t *testing.T) {
	t.Parallel()
	args := []string{"--engine", "google", "--model", "demo"}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--engine google") {
		t.Fatal(joined)
	}
	if cmdlineHasAPIKeyFlag(args) {
		t.Fatal("engine args must not include openai-api-key")
	}
}
