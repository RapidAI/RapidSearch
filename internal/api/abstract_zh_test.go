package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBriefRunes(t *testing.T) {
	if briefRunes("短", 10) != "短" {
		t.Fatal("short")
	}
	s := briefRunes("一二三四五六七八九十十一十二十三十四十五", 10)
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("brief=%q", s)
	}
	if len([]rune(s)) > 11 { // 10 + ellipsis
		t.Fatalf("too long: %q (%d)", s, len([]rune(s)))
	}
}

func TestAbstractZHOverlayAndAPI(t *testing.T) {
	h, dir := papersHandler(t)
	cache := abstractsZHFile{
		Version: 1,
		Items: map[string]abstractZHItem{
			"2401.05459": {
				AbstractZH: "个人智能体调研摘要。",
				SourceSHA1: abstractSourceSHA1("A survey of personal agents."),
				UpdatedAt:  "2026-01-01T00:00:00Z",
			},
		},
	}
	raw, _ := json.MarshalIndent(cache, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, abstractsZHName), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force reload of papers store so absZH picks up the file.
	// New() already constructed store before we wrote the file; reload via service load.
	srv := h.(*Server)
	ps := srv.papers()
	if err := ps.absZH.load(); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers/api", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var cat papersCatalog
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Papers) != 1 {
		t.Fatalf("%+v", cat)
	}
	p := cat.Papers[0]
	if p.Abstract == "" {
		t.Fatal("expected english abstract")
	}
	if p.AbstractZH != "个人智能体调研摘要。" {
		t.Fatalf("abstract_zh=%q", p.AbstractZH)
	}
	rawBody := rr.Body.String()
	if !strings.Contains(rawBody, `"abstract_zh"`) {
		t.Fatal("api must include abstract_zh field")
	}
	if strings.Contains(rawBody, "api_key") {
		t.Fatal("must not leak api keys")
	}
}

func TestAbstractZHStaleInvalidates(t *testing.T) {
	s := newAbstractZHService(t.TempDir(), nil)
	s.file.Items["x"] = abstractZHItem{
		AbstractZH: "旧译文",
		SourceSHA1: abstractSourceSHA1("old english"),
	}
	papers := []paperEntry{{ID: "x", Abstract: "new english", AbstractZH: ""}}
	s.overlay(papers)
	if papers[0].AbstractZH != "" {
		t.Fatalf("stale cache should not overlay: %q", papers[0].AbstractZH)
	}
}

func TestAbstractZHEnsureMissingUsesLLM(t *testing.T) {
	dir := t.TempDir()
	xlate := newTranslateService(dir)
	xlate.cfg.APIKey = "test-key"
	xlate.cfg.Model = "test-model"
	xlate.cfg.BaseURL = "http://example.invalid/v1"
	s := newAbstractZHService(dir, xlate)
	s.translateFn = func(ctx context.Context, snap translateSnapshot, abstract string) (string, error) {
		if abstract != "Hello world abstract." {
			t.Fatalf("abstract=%q", abstract)
		}
		return "你好世界摘要。", nil
	}
	s.start()
	papers := []paperEntry{{
		ID:       "demo1",
		Abstract: "Hello world abstract.",
	}}
	s.ensureMissing(papers)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		item, ok := s.get("demo1")
		if ok && item.AbstractZH == "你好世界摘要。" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	item, ok := s.get("demo1")
	if !ok || item.AbstractZH != "你好世界摘要。" {
		t.Fatalf("cached=%v %+v", ok, item)
	}
	// Persist file exists.
	if _, err := os.Stat(abstractsZHPath(dir)); err != nil {
		t.Fatal(err)
	}
	// Overlay applies.
	out := []paperEntry{{ID: "demo1", Abstract: "Hello world abstract."}}
	s.overlay(out)
	if out[0].AbstractZH != "你好世界摘要。" {
		t.Fatalf("%q", out[0].AbstractZH)
	}
}

func TestPapersPageAbstractTooltipJS(t *testing.T) {
	h, _ := papersHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/papers", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{
		"abstractDisplay",
		"abstract_zh",
		"absTranslating",
		"摘要翻译中…",
		`title="`,
		"briefChars",
		"abstract_zh_pending",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("papers.html missing %q", want)
		}
	}
}

func TestExtractChatContentShapes(t *testing.T) {
	reviewJSON := `{"method_principles":"甲","method_essence":"乙","experiment":"丙","quality":"丁"}`

	t.Run("message content", func(t *testing.T) {
		got, err := extractChatContent([]byte(`{"choices":[{"message":{"content":"  hello  "}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if got != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("empty content no reasoning", func(t *testing.T) {
		_, err := extractChatContent([]byte(`{"choices":[{"finish_reason":"length","message":{"content":""}}]}`))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "empty chat content") {
			t.Fatalf("want empty chat content, got %v", err)
		}
		if !strings.Contains(err.Error(), "finish_reason=length") {
			t.Fatalf("want finish_reason in error, got %v", err)
		}
		if strings.Contains(err.Error(), "unexpected end of JSON") {
			t.Fatalf("must not leak JSON parse error: %v", err)
		}
	})

	t.Run("null content", func(t *testing.T) {
		_, err := extractChatContent([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":null}}]}`))
		if err == nil || !strings.Contains(err.Error(), "empty chat content") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty choices", func(t *testing.T) {
		_, err := extractChatContent([]byte(`{"choices":[]}`))
		if err == nil || !strings.Contains(err.Error(), "empty chat choices") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("reasoning_content JSON", func(t *testing.T) {
		body := chatBody(t, "length", "", "thinking...\n"+reviewJSON+"\n", "")
		got, err := extractChatContent(body)
		if err != nil {
			t.Fatal(err)
		}
		if got != reviewJSON {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("reasoning field JSON", func(t *testing.T) {
		body := chatBody(t, "", "", "", "prefix "+reviewJSON)
		got, err := extractChatContent(body)
		if err != nil {
			t.Fatal(err)
		}
		if got != reviewJSON {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("reasoning without JSON", func(t *testing.T) {
		_, err := extractChatContent([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"","reasoning_content":"only chain of thought"}}]}`))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "empty chat content") || !strings.Contains(err.Error(), "reasoning present") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("content preferred over reasoning", func(t *testing.T) {
		body := chatBody(t, "", "from content", reviewJSON, "")
		got, err := extractChatContent(body)
		if err != nil {
			t.Fatal(err)
		}
		if got != "from content" {
			t.Fatalf("got %q", got)
		}
	})
}

func chatBody(t *testing.T, finish, content, reasoningContent, reasoning string) []byte {
	t.Helper()
	msg := map[string]any{"content": content}
	if reasoningContent != "" {
		msg["reasoning_content"] = reasoningContent
	}
	if reasoning != "" {
		msg["reasoning"] = reasoning
	}
	choice := map[string]any{"message": msg}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	b, err := json.Marshal(map[string]any{"choices": []any{choice}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
