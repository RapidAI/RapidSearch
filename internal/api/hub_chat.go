package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"search-service/internal/search"
)

const (
	hubChatMaxAttempts = 5
	hubChatRetryBase   = 400 * time.Millisecond
	hubChatRetryMax    = 8 * time.Second
	hubChatBodyLimit   = 2 << 20
)

// hubChatBackoff waits before the next retry. Tests replace this to avoid sleeping.
var hubChatBackoff = defaultHubChatBackoff

func defaultHubChatBackoff(ctx context.Context, attempt int) error {
	if attempt < 1 {
		attempt = 1
	}
	d := hubChatRetryBase << (attempt - 1)
	if d > hubChatRetryMax {
		d = hubChatRetryMax
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type chatHTTPError struct {
	Status int
	Code   string
	Msg    string
}

func (e *chatHTTPError) Error() string {
	if e == nil {
		return "chat HTTP error"
	}
	switch {
	case e.Code != "" && e.Msg != "":
		return fmt.Sprintf("chat HTTP %d (%s): %s", e.Status, e.Code, e.Msg)
	case e.Msg != "":
		return fmt.Sprintf("chat HTTP %d: %s", e.Status, e.Msg)
	case e.Code != "":
		return fmt.Sprintf("chat HTTP %d (%s)", e.Status, e.Code)
	default:
		return fmt.Sprintf("chat HTTP %d", e.Status)
	}
}

func retryableChatStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusTooManyRequests
}

func retryableChatTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected eof") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "eof")
}

func parseHubError(body []byte) (code, msg string) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return "", compactHubText(string(body))
	}
	code = jsonStringField(obj, "code")
	msg = jsonStringField(obj, "message")
	if raw, ok := obj["error"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if code == "" {
				code = jsonStringField(nested, "code")
			}
			if msg == "" {
				msg = jsonStringField(nested, "message")
			}
		} else if s := jsonRawString(raw); s != "" && msg == "" {
			msg = s
		}
	}
	return strings.TrimSpace(code), compactHubText(msg)
}

func jsonStringField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	return jsonRawString(raw)
}

func jsonRawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func compactHubText(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return ""
	}
	const max = 240
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func chatHTTPErrorFromBody(status int, body []byte) *chatHTTPError {
	code, msg := parseHubError(body)
	return &chatHTTPError{Status: status, Code: code, Msg: msg}
}

func isChatHTTPStatus(err error, status int) bool {
	var ce *chatHTTPError
	return errors.As(err, &ce) && ce != nil && ce.Status == status
}

// postHubChat POSTs an OpenAI-compatible chat/completions payload.
// It retries HTTP 502/503/429 and transient transport errors with exponential
// backoff and stops when ctx is done. A 404 falls through to the /v1 URL variant.
func postHubChat(ctx context.Context, client *http.Client, base, apiKey string, payload []byte) ([]byte, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	urls := []string{base + "/chat/completions"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/chat/completions")
	}
	var last error
	for _, u := range urls {
		body, err := postHubChatURL(ctx, client, u, apiKey, payload)
		if err == nil {
			return body, nil
		}
		last = err
		if isChatHTTPStatus(err, http.StatusNotFound) {
			continue
		}
		return nil, err
	}
	if last == nil {
		last = fmt.Errorf("chat request failed")
	}
	return nil, last
}

func postHubChatURL(ctx context.Context, client *http.Client, u, apiKey string, payload []byte) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= hubChatMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return nil, last
			}
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if last != nil {
					return nil, last
				}
				return nil, err
			}
			last = err
			if !retryableChatTransport(err) || attempt == hubChatMaxAttempts {
				return nil, err
			}
			log.Printf("papers hub chat: retry %d/%d after %v", attempt, hubChatMaxAttempts, err)
			if berr := hubChatBackoff(ctx, attempt); berr != nil {
				return nil, last
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, hubChatBodyLimit))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		last = chatHTTPErrorFromBody(resp.StatusCode, body)
		if resp.StatusCode == http.StatusNotFound || !retryableChatStatus(resp.StatusCode) || attempt == hubChatMaxAttempts {
			return nil, last
		}
		log.Printf("papers hub chat: retry %d/%d after %v", attempt, hubChatMaxAttempts, last)
		if berr := hubChatBackoff(ctx, attempt); berr != nil {
			return nil, last
		}
	}
	if last == nil {
		last = fmt.Errorf("chat request failed")
	}
	return nil, last
}

func writePapersLLMErr(w http.ResponseWriter, fallback string, err error) {
	if err == nil {
		writeErr(w, http.StatusBadGateway, fallback, search.CodeEngine, nil, "")
		return
	}
	if err == errReviewLLMNotReady {
		writeErr(w, http.StatusServiceUnavailable, err.Error(), search.CodeEngine, nil, "")
		return
	}
	msg := sanitizeUserError(err.Error())
	if msg == "" {
		msg = fallback
	}
	writeErr(w, http.StatusBadGateway, msg, search.CodeEngine, nil, "")
}
