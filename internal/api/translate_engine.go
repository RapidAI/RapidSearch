package api

import (
	"fmt"
	"strings"
)

// PDF translation backends persisted in translate-config.json.
// Abstract ZH / paper review / HF daily trend always use the Hub LLM
// snapshot (base_url + api_key + model) and ignore this switch.
const (
	translateEngineHub    = "hub"
	translateEngineGoogle = "google"
)

func normalizeTranslateEngine(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", translateEngineHub, "openai", "llm", "openai-compatible":
		return translateEngineHub, nil
	case translateEngineGoogle, "google_translate", "google-translate", "gtranslate":
		return translateEngineGoogle, nil
	default:
		return "", fmt.Errorf("unknown translation engine %q (use hub or google)", raw)
	}
}

func (s translateSnapshot) pdfEngine() string {
	eng, err := normalizeTranslateEngine(s.Engine)
	if err != nil {
		return translateEngineHub
	}
	return eng
}

func (s translateSnapshot) llmReady() bool {
	return strings.TrimSpace(s.APIKey) != "" && strings.TrimSpace(s.Model) != ""
}

// ready reports whether the Hub / OpenAI-compatible LLM is configured.
// Used by abstract_zh, paper review, and HF daily trend.
func (s translateSnapshot) ready() bool {
	return s.llmReady()
}

// pdfReady reports whether a new BabelDOC / paper PDF job can start.
func (s translateSnapshot) pdfReady() bool {
	if s.pdfEngine() == translateEngineGoogle {
		return true
	}
	return s.llmReady()
}

func (s translateSnapshot) pdfNotReadyError() string {
	if s.pdfEngine() == translateEngineGoogle {
		return "Google Translate engine is selected but not available"
	}
	return "LLM settings incomplete (need api_key and model)"
}
