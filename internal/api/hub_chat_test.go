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

func TestPostHubChatRetriesTruncatedJSON(t *testing.T) {
	instantHubBackoff(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":`))
			return
		}
		_, _ = w.Write(chatCompletionBody("ok"))
	}))
	defer ts.Close()

	body, err := postHubChat(context.Background(), ts.Client(), ts.URL+"/v1", "k", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	text, err := extractChatContent(body)
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if hits.Load() != 2 {
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

func TestPublicReviewGenerateError(t *testing.T) {
	if publicReviewGenerateError(errors.New("unexpected end of JSON input")) != "truncated model response" {
		t.Fatal(publicReviewGenerateError(errors.New("unexpected end of JSON input")))
	}
	if publicReviewGenerateError(errChatEmpty) != "empty model response" {
		t.Fatal(publicReviewGenerateError(errChatEmpty))
	}
	if publicReviewGenerateError(errors.New("boom")) != "could not generate review" {
		t.Fatal(publicReviewGenerateError(errors.New("boom")))
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
	start := time.Now()
	err := defaultHubChatBackoff(ctx, 8)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("backoff ignored deadline")
	}
}
