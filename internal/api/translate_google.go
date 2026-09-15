package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const googleTranslateMaxRunes = 4500

var (
	googleCloudTranslateURL  = "https://translation.googleapis.com/language/translate/v2"
	googleGTXTranslateURL    = "https://translate.googleapis.com/translate_a/single"
	googleMobileTranslateURL = "https://translate.google.com/m"
)

var (
	babelDOCInputMarker = []string{"Input:\n\n", "Input:\r\n\r\n"}
	placeholderRE       = regexp.MustCompile(`\{\{[^}]+\}\}|\{v\d+\}|</?b\d+>|</?style[^>]*>`)
	googleResultRE      = regexp.MustCompile(`(?s)class="(?:t0|result-container)">(.*?)<`)
)

type googleTranslateClient struct {
	client *http.Client
	apiKey string
}

func newGoogleTranslateClient(client *http.Client, apiKey string) *googleTranslateClient {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &googleTranslateClient{client: client, apiKey: strings.TrimSpace(apiKey)}
}

func (g *googleTranslateClient) via() string {
	if g != nil && strings.TrimSpace(g.apiKey) != "" {
		return "google_cloud"
	}
	return "google_web"
}

func (g *googleTranslateClient) translate(ctx context.Context, text, langIn, langOut string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	langIn = normalizeGoogleLang(langIn, "en")
	langOut = normalizeGoogleLang(langOut, "zh-CN")
	protected, tokens := protectPlaceholders(text)
	var out string
	var err error
	if g != nil && strings.TrimSpace(g.apiKey) != "" {
		out, err = g.translateCloud(ctx, protected, langIn, langOut)
	} else {
		out, err = g.translateWeb(ctx, protected, langIn, langOut)
	}
	if err != nil {
		return "", err
	}
	return restorePlaceholders(out, tokens), nil
}

func (g *googleTranslateClient) translateCloud(ctx context.Context, text, langIn, langOut string) (string, error) {
	chunks := splitTranslateChunks(text, googleTranslateMaxRunes)
	var b strings.Builder
	for _, chunk := range chunks {
		payload, err := json.Marshal(map[string]any{
			"q":      []string{chunk},
			"source": langIn,
			"target": langOut,
			"format": "text",
		})
		if err != nil {
			return "", err
		}
		u := googleCloudTranslateURL + "?key=" + url.QueryEscape(g.apiKey)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := g.client.Do(req)
		if err != nil {
			return "", err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("Google Cloud Translation HTTP %d", resp.StatusCode)
		}
		var parsed struct {
			Data struct {
				Translations []struct {
					TranslatedText string `json:"translatedText"`
				} `json:"translations"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return "", fmt.Errorf("Google Cloud Translation: invalid json")
		}
		if len(parsed.Data.Translations) == 0 {
			return "", fmt.Errorf("Google Cloud Translation: empty response")
		}
		b.WriteString(html.UnescapeString(parsed.Data.Translations[0].TranslatedText))
	}
	return b.String(), nil
}

func (g *googleTranslateClient) translateWeb(ctx context.Context, text, langIn, langOut string) (string, error) {
	chunks := splitTranslateChunks(text, googleTranslateMaxRunes)
	var b strings.Builder
	for _, chunk := range chunks {
		out, err := g.translateGTX(ctx, chunk, langIn, langOut)
		if err != nil {
			out, err = g.translateMobile(ctx, chunk, langIn, langOut)
		}
		if err != nil {
			return "", err
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

func (g *googleTranslateClient) translateGTX(ctx context.Context, text, langIn, langOut string) (string, error) {
	u, err := url.Parse(googleGTXTranslateURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client", "gtx")
	q.Set("sl", langIn)
	q.Set("tl", langOut)
	q.Set("dt", "t")
	q.Set("q", text)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 RapidSearch-papers")
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Google Translate HTTP %d", resp.StatusCode)
	}
	out, err := parseGoogleGTX(body)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (g *googleTranslateClient) translateMobile(ctx context.Context, text, langIn, langOut string) (string, error) {
	u, err := url.Parse(googleMobileTranslateURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("sl", langIn)
	q.Set("tl", langOut)
	q.Set("q", text)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/4.0 (compatible; MSIE 6.0; Windows NT 5.1)")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Google Translate HTTP %d", resp.StatusCode)
	}
	m := googleResultRE.FindSubmatch(body)
	if len(m) < 2 {
		return "", fmt.Errorf("Google Translate: no result-container")
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1]))), nil
}

func parseGoogleGTX(body []byte) (string, error) {
	var parsed []any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("Google Translate: invalid json")
	}
	if len(parsed) == 0 {
		return "", fmt.Errorf("Google Translate: empty response")
	}
	sentences, ok := parsed[0].([]any)
	if !ok {
		return "", fmt.Errorf("Google Translate: unexpected shape")
	}
	var b strings.Builder
	for _, s := range sentences {
		row, ok := s.([]any)
		if !ok || len(row) == 0 {
			continue
		}
		if piece, ok := row[0].(string); ok {
			b.WriteString(piece)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("Google Translate: empty translation")
	}
	return out, nil
}

func pingGoogleTranslate(ctx context.Context, client *http.Client, apiKey, langIn, langOut string) (via string, err error) {
	g := newGoogleTranslateClient(client, apiKey)
	via = g.via()
	out, err := g.translate(ctx, "hello", langIn, langOut)
	if err != nil {
		return via, err
	}
	if strings.TrimSpace(out) == "" {
		return via, fmt.Errorf("Google Translate returned empty text")
	}
	return via, nil
}

func extractBabelDOCInput(user string) string {
	user = strings.TrimSpace(user)
	for _, marker := range babelDOCInputMarker {
		if i := strings.LastIndex(user, marker); i >= 0 {
			return strings.TrimSpace(user[i+len(marker):])
		}
	}
	return user
}

func lastUserContent(messages []map[string]string) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i]["role"]), "user") {
			return messages[i]["content"]
		}
	}
	if len(messages) > 0 {
		return messages[len(messages)-1]["content"]
	}
	return ""
}

func looksLikeTermExtraction(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "glossary") ||
		strings.Contains(low, "term extraction") ||
		strings.Contains(low, "extract terms") ||
		strings.Contains(low, "json object")
}

func normalizeGoogleLang(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	switch strings.ToLower(s) {
	case "zh", "zh-cn", "zh_cn", "chinese", "simplified chinese":
		return "zh-CN"
	case "zh-tw", "zh_tw":
		return "zh-TW"
	}
	return s
}

func protectPlaceholders(text string) (string, []string) {
	var tokens []string
	out := placeholderRE.ReplaceAllStringFunc(text, func(m string) string {
		tokens = append(tokens, m)
		return fmt.Sprintf("⟨GPH%d⟩", len(tokens)-1)
	})
	return out, tokens
}

func restorePlaceholders(text string, tokens []string) string {
	for i, tok := range tokens {
		text = strings.ReplaceAll(text, fmt.Sprintf("⟨GPH%d⟩", i), tok)
		text = strings.ReplaceAll(text, fmt.Sprintf("<GPH%d>", i), tok)
	}
	return text
}

func splitTranslateChunks(text string, maxRunes int) []string {
	if maxRunes <= 0 || utf8Len(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	var b strings.Builder
	n := 0
	for _, r := range text {
		if n >= maxRunes {
			chunks = append(chunks, b.String())
			b.Reset()
			n = 0
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

func utf8Len(s string) int {
	return len([]rune(s))
}

type googleOpenAIShim struct {
	translator *googleTranslateClient
	langIn     string
	langOut    string
}

func startGoogleOpenAIShim(ctx context.Context, client *http.Client, apiKey, langIn, langOut string) (baseURL string, cleanup func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", func() {}, err
	}
	shim := &googleOpenAIShim{
		translator: newGoogleTranslateClient(client, apiKey),
		langIn:     normalizeGoogleLang(langIn, "en"),
		langOut:    normalizeGoogleLang(langOut, "zh-CN"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", shim.handleModels)
	mux.HandleFunc("/models", shim.handleModels)
	mux.HandleFunc("/v1/chat/completions", shim.handleChat)
	mux.HandleFunc("/chat/completions", shim.handleChat)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ln)
	}()
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		_ = ln.Close()
		wg.Wait()
	}
	if ctx != nil {
		go func() {
			<-ctx.Done()
			stop()
		}()
	}
	return "http://" + ln.Addr().String() + "/v1", stop, nil
}

func (s *googleOpenAIShim) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]string{{"id": "google-translate", "object": "model"}},
	})
}

func (s *googleOpenAIShim) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var body struct {
		Messages []map[string]string `json:"messages"`
		Format   *struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	user := lastUserContent(body.Messages)
	wantJSON := body.Format != nil && body.Format.Type == "json_object"
	if wantJSON || looksLikeTermExtraction(user) {
		s.writeChat(w, `{"glossary":[],"terms":[]}`)
		return
	}
	src := extractBabelDOCInput(user)
	out, err := s.translator.translate(r.Context(), src, s.langIn, s.langOut)
	if err != nil {
		http.Error(w, sanitizeUserError(err.Error()), http.StatusBadGateway)
		return
	}
	s.writeChat(w, out)
}

func (s *googleOpenAIShim) writeChat(w http.ResponseWriter, content string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-google",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "google-translate",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]string{
				"role":    "assistant",
				"content": content,
			},
		}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}
