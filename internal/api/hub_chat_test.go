package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func instantHubBackoff(t *testing.T) {
	t.Helper()
	orig := hubChatBackoff
	hubChatBackoff = func(ctx context.Context, attempt int) error {
		return ctx.Err()
	}
	t.Cleanup(func() { hubChatBackoff = orig })
}

func officialUnavailableBody() []byte {
	return []byte(`{"error":{"code":"LLM_OFFICIAL_UNAVAILABLE","message":"MaClaw official service is temporarily unavailable (opencode-3 circuit probe)"}}`)
}

func chatCompletionBody(content string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"content": content}},
		},
	})
	return raw
}

func sampleTrendJSON() string {
	return `{"headline":"主线","overview":"综述","themes":["主题"],"highlights":["要点"],"outlook":"展望"}`
}

func TestParseHubErrorOfficialUnavailable(t *testing.T) {
	code, msg := parseHubError(officialUnavailableBody())
	if code != "LLM_OFFICIAL_UNAVAILABLE" {
		t.Fatalf("code=%q", code)
	}
	if !strings.Contains(msg, "MaClaw official service is temporarily unavailable") {
		t.Fatalf("msg=%q", msg)
	}
	err := chatHTTPErrorFromBody(http.StatusServiceUnavailable, officialUnavailableBody())
	got := err.Error()
	if !strings.Contains(got, "chat HTTP 503 (LLM_OFFICIAL_UNAVAILABLE):") {
		t.Fatalf("error=%q", got)
	}
	if !strings.Contains(got, "opencode-3 circuit probe") {
		t.Fatalf("missing hub message: %q", got)
	}
}

func TestGenerateHFDailyTrendOpenAIRetries503ThenSucceeds(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(officialUnavailableBody())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatCompletionBody(sampleTrendJSON()))
	}))
	defer ts.Close()

	trend, err := generateHFDailyTrendOpenAI(context.Background(), ts.Client(), translateSnapshot{
		BaseURL: ts.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, "2026-09-14", []paperEntry{{Title: "DataFlex-RL", Abstract: "RLVR data policies."}})
	if err != nil {
		t.Fatal(err)
	}
	if trend.Headline != "主线" || !trend.any() {
		t.Fatalf("%+v", trend)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d want 2", hits.Load())
	}
}

func TestGenerateHFDailyTrendOpenAIPersistent503FailsAfterN(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(officialUnavailableBody())
	}))
	defer ts.Close()

	_, err := generateHFDailyTrendOpenAI(context.Background(), ts.Client(), translateSnapshot{
		BaseURL: ts.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, "2026-09-14", []paperEntry{{Title: "DataFlex-RL"}})
	if err == nil {
		t.Fatal("expected error after persistent 503")
	}
	if !strings.Contains(err.Error(), "chat HTTP 503 (LLM_OFFICIAL_UNAVAILABLE):") {
		t.Fatalf("want hub message, got %v", err)
	}
	if !strings.Contains(err.Error(), "MaClaw official service is temporarily unavailable") {
		t.Fatalf("want hub body, got %v", err)
	}
	if hits.Load() != int32(hubChatMaxAttempts) {
		t.Fatalf("hits=%d want %d", hits.Load(), hubChatMaxAttempts)
	}
}

func TestGenerateReviewOpenAIRetries503ThenSucceeds(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	content, _ := json.Marshal(sampleAnalysis())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(officialUnavailableBody())
			return
		}
		_, _ = w.Write(chatCompletionBody(string(content)))
	}))
	defer ts.Close()

	got, model, err := generateReviewOpenAI(context.Background(), ts.Client(), translateSnapshot{
		BaseURL: ts.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, paperEntry{Title: "Personal LLM Agents", Abstract: "A survey."})
	if err != nil {
		t.Fatal(err)
	}
	if model != "test-model" || !got.any() {
		t.Fatalf("model=%s analysis=%+v", model, got)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d want 2", hits.Load())
	}
}

func TestGenerateReviewOpenAIPersistent503FailsAfterN(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(officialUnavailableBody())
	}))
	defer ts.Close()

	_, _, err := generateReviewOpenAI(context.Background(), ts.Client(), translateSnapshot{
		BaseURL: ts.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, paperEntry{Title: "Personal LLM Agents"})
	if err == nil {
		t.Fatal("expected error after persistent 503")
	}
	if !strings.Contains(err.Error(), "chat HTTP 503 (LLM_OFFICIAL_UNAVAILABLE):") {
		t.Fatalf("want hub message, got %v", err)
	}
	if hits.Load() != int32(hubChatMaxAttempts) {
		t.Fatalf("hits=%d want %d", hits.Load(), hubChatMaxAttempts)
	}
}

func TestPostHubChatRetries429AndTransport(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch n {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit","message":"slow down"}}`))
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream reset"}}`))
		default:
			_, _ = w.Write(chatCompletionBody("ok"))
		}
	}))
	defer ts.Close()

	body, err := postHubChat(context.Background(), ts.Client(), ts.URL+"/v1", "k", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	text, err := extractChatContent(body)
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestPostHubChatDoesNotRetry401(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"bad key"}}`))
	}))
	defer ts.Close()

	_, err := postHubChat(context.Background(), ts.Client(), ts.URL+"/v1", "k", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "chat HTTP 401") {
		t.Fatalf("err=%v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestPostHubChatRespectsDeadline(t *testing.T) {
	orig := hubChatBackoff
	hubChatBackoff = func(ctx context.Context, attempt int) error {
		return ctx.Err()
	}
	defer func() { hubChatBackoff = orig }()

	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(officialUnavailableBody())
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := postHubChat(ctx, ts.Client(), ts.URL+"/v1", "k", []byte(`{}`))
	if err == nil {
		t.Fatal("expected deadline/cancel error")
	}
	if hits.Load() != 0 {
		t.Fatalf("canceled context must not hit server, hits=%d", hits.Load())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestPostHubChatKeepsHubMessageWhenDeadlineHitsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var hits atomic.Int32
	orig := hubChatBackoff
	hubChatBackoff = func(ctx context.Context, attempt int) error {
		cancel()
		return ctx.Err()
	}
	defer func() { hubChatBackoff = orig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(officialUnavailableBody())
	}))
	defer ts.Close()

	_, err := postHubChat(ctx, ts.Client(), ts.URL+"/v1", "k", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "LLM_OFFICIAL_UNAVAILABLE") {
		t.Fatalf("want last hub error, got %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestHFDailyTrendHTTPSurfacesHubMessage(t *testing.T) {
	h, _ := papersHandler(t)
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
	ps.daily.generateFn = func(ctx context.Context, snap translateSnapshot, date string, papers []paperEntry) (hfDailyTrend, error) {
		return hfDailyTrend{}, chatHTTPErrorFromBody(http.StatusServiceUnavailable, officialUnavailableBody())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/papers/daily/2026-09-14/trend", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "LLM_OFFICIAL_UNAVAILABLE") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "MaClaw official service") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestRetryableChatTransport(t *testing.T) {
	if retryableChatTransport(io.EOF) != true {
		t.Fatal("EOF should retry")
	}
	if retryableChatTransport(context.Canceled) {
		t.Fatal("cancel must not retry")
	}
	if retryableChatTransport(context.DeadlineExceeded) {
		t.Fatal("deadline must not retry")
	}
}

func TestCompactHubTextAndBackoffBound(t *testing.T) {
	if compactHubText("  a \n b  ") != "a b" {
		t.Fatal(compactHubText("  a \n b  "))
	}
	long := strings.Repeat("x", 300)
	if got := compactHubText(long); !strings.HasPrefix(got, strings.Repeat("x", 240)) || !strings.HasSuffix(got, "…") {
		t.Fatalf("compact=%q len=%d", got, len(got))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// Real backoff must honor ctx instead of sleeping the full interval.
	start := time.Now()
	err := defaultHubChatBackoff(ctx, 8)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("backoff ignored deadline")
	}
}
