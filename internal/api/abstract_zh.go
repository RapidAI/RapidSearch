package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	abstractsZHName       = "abstracts_zh.json"
	abstractZHMaxWorkers  = 2
	abstractZHMaxQueue    = 64
	abstractZHBriefRunes  = 140
	abstractZHHTTPTimeout = 90 * time.Second
)

type abstractZHItem struct {
	AbstractZH string `json:"abstract_zh"`
	SourceSHA1 string `json:"source_sha1,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type abstractsZHFile struct {
	Version   int                       `json:"version"`
	UpdatedAt string                    `json:"updated_at,omitempty"`
	Items     map[string]abstractZHItem `json:"items"`
}

type abstractZHService struct {
	root  string
	xlate *translateService

	mu      sync.Mutex
	file    abstractsZHFile
	queue   []abstractZHJob
	inQ     map[string]bool
	running int
	kick    chan struct{}
	started bool

	// client overrides http client (tests).
	client *http.Client
	// translateFn overrides LLM call (tests).
	translateFn func(ctx context.Context, snap translateSnapshot, abstract string) (string, error)
}

type abstractZHJob struct {
	ID       string
	Abstract string
	SHA1     string
}

func abstractsZHPath(root string) string {
	return filepath.Join(root, abstractsZHName)
}

func newAbstractZHService(root string, xlate *translateService) *abstractZHService {
	if root == "" {
		root = papersRoot()
	}
	s := &abstractZHService{
		root:  root,
		xlate: xlate,
		file: abstractsZHFile{
			Version: 1,
			Items:   map[string]abstractZHItem{},
		},
		inQ:  make(map[string]bool),
		kick: make(chan struct{}, 1),
	}
	_ = s.load()
	return s
}

func (s *abstractZHService) start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

func (s *abstractZHService) load() error {
	if s == nil {
		return nil
	}
	b, err := os.ReadFile(abstractsZHPath(s.root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f abstractsZHFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f.Items == nil {
		f.Items = map[string]abstractZHItem{}
	}
	if f.Version == 0 {
		f.Version = 1
	}
	s.mu.Lock()
	s.file = f
	s.mu.Unlock()
	return nil
}

func (s *abstractZHService) persistLocked() error {
	s.file.Version = 1
	s.file.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(abstractsZHPath(s.root), append(b, '\n'), 0o644)
}

func abstractSourceSHA1(abstract string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(abstract)))
	return hex.EncodeToString(sum[:])
}

func (s *abstractZHService) get(id string) (abstractZHItem, bool) {
	if s == nil || id == "" {
		return abstractZHItem{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.file.Items[id]
	return item, ok
}

func (s *abstractZHService) overlay(papers []paperEntry) {
	if s == nil || len(papers) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range papers {
		id := papers[i].ID
		if id == "" {
			id = paperTranslateID(papers[i])
			papers[i].ID = id
		}
		if id == "" {
			continue
		}
		item, ok := s.file.Items[id]
		if !ok || strings.TrimSpace(item.AbstractZH) == "" {
			continue
		}
		src := strings.TrimSpace(papers[i].Abstract)
		if src != "" && item.SourceSHA1 != "" && item.SourceSHA1 != abstractSourceSHA1(src) {
			// Stale cache vs current English abstract — leave empty so ensureMissing requeues.
			continue
		}
		papers[i].AbstractZH = item.AbstractZH
	}
}

// ensureMissing enqueues papers that lack a valid Chinese abstract.
// Safe for anonymous /papers/api callers; does not block.
func (s *abstractZHService) ensureMissing(papers []paperEntry) {
	if s == nil {
		return
	}
	snap := translateSnapshot{}
	if s.xlate != nil {
		snap = s.xlate.snapshot()
	}
	if !snap.ready() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for _, p := range papers {
		if len(s.queue) >= abstractZHMaxQueue {
			break
		}
		id := p.ID
		if id == "" {
			id = paperTranslateID(p)
		}
		src := strings.TrimSpace(p.Abstract)
		if id == "" || src == "" {
			continue
		}
		sha := abstractSourceSHA1(src)
		if item, ok := s.file.Items[id]; ok {
			if strings.TrimSpace(item.AbstractZH) != "" && (item.SourceSHA1 == "" || item.SourceSHA1 == sha) {
				continue
			}
		}
		if s.inQ[id] {
			continue
		}
		s.queue = append(s.queue, abstractZHJob{ID: id, Abstract: src, SHA1: sha})
		s.inQ[id] = true
		added++
	}
	if added > 0 {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

func (s *abstractZHService) pendingCount(papers []paperEntry) int {
	if s == nil {
		return 0
	}
	if s.xlate == nil || !s.xlate.snapshot().ready() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range papers {
		id := p.ID
		if id == "" {
			id = paperTranslateID(p)
		}
		src := strings.TrimSpace(p.Abstract)
		if id == "" || src == "" {
			continue
		}
		if strings.TrimSpace(p.AbstractZH) != "" {
			continue
		}
		item, ok := s.file.Items[id]
		sha := abstractSourceSHA1(src)
		if ok && strings.TrimSpace(item.AbstractZH) != "" && (item.SourceSHA1 == "" || item.SourceSHA1 == sha) {
			continue
		}
		n++
	}
	return n
}

func (s *abstractZHService) loop() {
	for range s.kick {
		for s.tryStartOne() {
		}
	}
}

func (s *abstractZHService) tryStartOne() bool {
	s.mu.Lock()
	if s.running >= abstractZHMaxWorkers || len(s.queue) == 0 {
		s.mu.Unlock()
		return false
	}
	job := s.queue[0]
	s.queue = s.queue[1:]
	s.running++
	s.mu.Unlock()
	go s.runJob(job)
	return true
}

func (s *abstractZHService) runJob(job abstractZHJob) {
	defer func() {
		s.mu.Lock()
		s.running--
		delete(s.inQ, job.ID)
		more := len(s.queue) > 0 && s.running < abstractZHMaxWorkers
		s.mu.Unlock()
		if more {
			select {
			case s.kick <- struct{}{}:
			default:
			}
		}
	}()

	snap := translateSnapshot{}
	if s.xlate != nil {
		snap = s.xlate.snapshot()
	}
	if !snap.ready() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abstractZHHTTPTimeout)
	defer cancel()
	zh, err := s.callTranslate(ctx, snap, job.Abstract)
	if err != nil {
		log.Printf("papers abstract_zh translate id=%s: %v", job.ID, err)
		return
	}
	zh = strings.TrimSpace(zh)
	if zh == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.Items == nil {
		s.file.Items = map[string]abstractZHItem{}
	}
	s.file.Items[job.ID] = abstractZHItem{
		AbstractZH: zh,
		SourceSHA1: job.SHA1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.persistLocked(); err != nil {
		log.Printf("papers abstracts_zh write: %v", err)
	}
}

func (s *abstractZHService) callTranslate(ctx context.Context, snap translateSnapshot, abstract string) (string, error) {
	if s != nil && s.translateFn != nil {
		return s.translateFn(ctx, snap, abstract)
	}
	return translateAbstractOpenAI(ctx, s.httpClient(), snap.BaseURL, snap.APIKey, snap.Model, abstract)
}

func (s *abstractZHService) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: abstractZHHTTPTimeout}
}

func translateAbstractOpenAI(ctx context.Context, client *http.Client, base, key, model, abstract string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	if client == nil {
		client = &http.Client{Timeout: abstractZHHTTPTimeout}
	}
	urls := []string{base + "/chat/completions"}
	if !strings.HasSuffix(base, "/v1") {
		urls = append(urls, base+"/v1/chat/completions")
	}
	sys := "You are a professional academic translator. Translate the English paper abstract into Simplified Chinese. Keep technical terms accurate. Output only the Chinese translation with no preamble or quotes."
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": abstract},
		},
		"temperature": 0.2,
	})
	if err != nil {
		return "", err
	}
	var last error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			last = err
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			last = fmt.Errorf("chat HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusNotFound {
				continue
			}
			return "", last
		}
		text, err := extractChatContent(body)
		if err != nil {
			return "", err
		}
		return text, nil
	}
	if last == nil {
		last = fmt.Errorf("chat translate failed")
	}
	return "", last
}

func extractChatContent(body []byte) (string, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", errChatEmpty
	}
	if looksTruncatedJSON(body) {
		return "", errChatTruncated
	}
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		if isTruncatedJSONError(err) {
			return "", errChatTruncated
		}
		return "", errChatInvalid
	}
	if len(parsed.Choices) == 0 {
		return "", errChatEmpty
	}
	ch := parsed.Choices[0]
	content := strings.TrimSpace(chatMessageText(ch.Message.Content))
	if content != "" {
		return content, nil
	}
	reasoning := strings.TrimSpace(ch.Message.ReasoningContent)
	if reasoning == "" {
		reasoning = strings.TrimSpace(ch.Message.Reasoning)
	}
	if obj, ok := extractJSONObject(reasoning); ok {
		return obj, nil
	}
	reason := strings.TrimSpace(ch.FinishReason)
	if reason == "" {
		reason = "unknown"
	}
	if reasoning != "" {
		return "", fmt.Errorf("%w (finish_reason=%s; reasoning present but no JSON object)", errChatEmpty, reason)
	}
	return "", fmt.Errorf("%w (finish_reason=%s)", errChatEmpty, reason)
}

func chatMessageText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		var text string
		if json.Unmarshal(part, &text) == nil {
			b.WriteString(text)
			continue
		}
		var obj struct {
			Text string `json:"text"`
			Type string `json:"type"`
		}
		if json.Unmarshal(part, &obj) == nil && strings.TrimSpace(obj.Text) != "" {
			b.WriteString(obj.Text)
		}
	}
	return b.String()
}

// extractJSONObject returns the first JSON object embedded in s, if it parses.
func extractJSONObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return "", false
	}
	cand := s[i : j+1]
	var probe any
	if json.Unmarshal([]byte(cand), &probe) != nil {
		return "", false
	}
	return cand, true
}

// briefRunes truncates by Unicode code points (better for CJK abstracts).
func briefRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	cut := runes[:max]
	// Prefer breaking near a space/punctuation in the last third.
	for i := len(cut) - 1; i > max*2/3; i-- {
		switch cut[i] {
		case ' ', '\t', '\n', '，', '。', '、', ',', '.', ';', '；':
			cut = cut[:i]
			return strings.TrimSpace(string(cut)) + "…"
		}
	}
	return strings.TrimSpace(string(cut)) + "…"
}
