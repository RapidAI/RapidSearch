package search

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	envConfigPath     = "SEARCH_CONFIG_PATH"
	defaultConfigFile = "search-config.json"
	configFileMode    = 0o600
)

// Engine IDs that can appear in the settings priority list.
var AllEngineIDs = []string{
	"serper", "brave",
	"baidu", "sogou", "360", "bing",
	"duckduckgo_html", "duckduckgo", "google",
}

// DefaultPriorityIDs is the settings-page default order. Auto without a saved
// file still uses the China/Global HTTP-first chains; keyed engines are
// prepended only when a key exists. Google is present but disabled so auto
// does not launch Google Chrome unless the user enables it.
var DefaultPriorityIDs = []string{
	"serper", "brave",
	"baidu", "sogou", "360", "bing",
	"duckduckgo_html", "duckduckgo", "google",
}

// EnginePref is one row of the user-saved priority list.
type EnginePref struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// EngineKind classifies how an engine is executed.
type EngineKind string

const (
	KindKeyed  EngineKind = "keyed"
	KindHTTP   EngineKind = "http"
	KindChrome EngineKind = "chrome"
	KindDual   EngineKind = "http_chrome"
)

// EngineInfo is static UI/schedule metadata.
type EngineInfo struct {
	ID    string
	Label string
	Kind  EngineKind
	Hint  string
}

// EngineCatalog is the settings-page catalog (stable order = DefaultPriorityIDs).
func EngineCatalog() []EngineInfo {
	return []EngineInfo{
		{ID: "serper", Label: "Serper", Kind: KindKeyed, Hint: "google.serper.dev · requires API key"},
		{ID: "brave", Label: "Brave Search API", Kind: KindKeyed, Hint: "api.search.brave.com · requires API key"},
		{ID: "baidu", Label: "Baidu", Kind: KindDual, Hint: "HTTP scrape; Chrome only if requested explicitly"},
		{ID: "sogou", Label: "Sogou", Kind: KindDual, Hint: "HTTP scrape; Chrome only if requested explicitly"},
		{ID: "360", Label: "360 (so.com)", Kind: KindDual, Hint: "HTTP scrape; Chrome only if requested explicitly"},
		{ID: "bing", Label: "Bing", Kind: KindDual, Hint: "HTTP scrape; Chrome only if requested explicitly"},
		{ID: "duckduckgo_html", Label: "DuckDuckGo HTML", Kind: KindHTTP, Hint: "HTTP only · no Chrome slot"},
		{ID: "duckduckgo", Label: "DuckDuckGo", Kind: KindChrome, Hint: "Chrome fallback after HTML"},
		{ID: "google", Label: "Google", Kind: KindChrome, Hint: "Chrome only · disabled on auto by default; captcha breaker if enabled"},
	}
}

func engineInfo(id string) (EngineInfo, bool) {
	for _, e := range EngineCatalog() {
		if e.ID == id {
			return e, true
		}
	}
	return EngineInfo{}, false
}

func isKeyedEngine(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "serper", "brave":
		return true
	default:
		return false
	}
}

func knownEngineID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, e := range AllEngineIDs {
		if e == id {
			return true
		}
	}
	return false
}

// fileConfig is the on-disk JSON document. Keys are never written to logs.
type fileConfig struct {
	Version      int          `json:"version"`
	SerperAPIKey string       `json:"serper_api_key,omitempty"`
	BraveAPIKey  string       `json:"brave_api_key,omitempty"`
	Priority     []EnginePref `json:"priority,omitempty"`
}

// ConfigSnapshot is an immutable view used by the scheduler and cache key.
// API keys stay in this snapshot so RunHTTP can sign requests; CacheSig()
// never includes raw key material.
type ConfigSnapshot struct {
	SerperKey string
	BraveKey  string
	Priority  []EnginePref
	Custom    bool
}

// HasKey reports a non-empty API key for a keyed engine.
func (s ConfigSnapshot) HasKey(engine string) bool {
	return s.Key(engine) != ""
}

// Key returns the API key for serper/brave, or empty.
func (s ConfigSnapshot) Key(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "serper":
		return strings.TrimSpace(s.SerperKey)
	case "brave":
		return strings.TrimSpace(s.BraveKey)
	default:
		return ""
	}
}

// HasCustomPriority is true when the user saved an ordered list.
func (s ConfigSnapshot) HasCustomPriority() bool {
	return s.Custom && len(s.Priority) > 0
}

// CacheSig is a stable, non-secret fingerprint of keys-present + enabled order.
// Included in the search cache key so serper vs bing (or a later key change)
// cannot collide.
func (s ConfigSnapshot) CacheSig() string {
	var b strings.Builder
	b.WriteString("serper=")
	if s.HasKey("serper") {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	b.WriteString(",brave=")
	if s.HasKey("brave") {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	if s.HasCustomPriority() {
		b.WriteByte('|')
		first := true
		for _, p := range s.Priority {
			if !p.Enabled {
				continue
			}
			if isKeyedEngine(p.ID) && !s.HasKey(p.ID) {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(p.ID)
		}
	}
	return b.String()
}

func (s ConfigSnapshot) enabledInPriority(id string) (enabled bool, found bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range s.Priority {
		if strings.ToLower(strings.TrimSpace(p.ID)) == id {
			return p.Enabled, true
		}
	}
	return false, false
}

func (s ConfigSnapshot) keyedWantedOnDefaultAuto(id string) bool {
	if !s.HasKey(id) {
		return false
	}
	if en, found := s.enabledInPriority(id); found {
		return en
	}
	return true
}

func (s ConfigSnapshot) filterCustomAuto() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Priority {
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if !p.Enabled || !knownEngineID(id) || seen[id] {
			continue
		}
		if isKeyedEngine(id) && !s.HasKey(id) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Store is the process-wide search-config file (keys + priority).
type Store struct {
	path string
	mu   sync.RWMutex
	cfg  fileConfig
}

// ConfigPath resolves SEARCH_CONFIG_PATH or ./search-config.json.
func ConfigPath() string {
	if p := strings.TrimSpace(os.Getenv(envConfigPath)); p != "" {
		return p
	}
	return defaultConfigFile
}

// OpenConfig loads path if it exists. Missing file is not an error.
func OpenConfig(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = ConfigPath()
	}
	s := &Store{path: path, cfg: fileConfig{Version: 1}}
	if err := s.reload(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string {
	if s == nil {
		return ConfigPath()
	}
	return s.path
}

func (s *Store) reload() error {
	if s == nil || s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return fmt.Errorf("search-config: %w", err)
	}
	if fc.Version == 0 {
		fc.Version = 1
	}
	fc.Priority = normalizePriority(fc.Priority, false)
	s.mu.Lock()
	s.cfg = fc
	s.mu.Unlock()
	log.Printf("search-config loaded path=%s serper=%s brave=%s custom_priority=%v",
		s.path, boolWord(strings.TrimSpace(fc.SerperAPIKey) != ""), boolWord(strings.TrimSpace(fc.BraveAPIKey) != ""), len(fc.Priority) > 0)
	return nil
}

func boolWord(ok bool) string {
	if ok {
		return "set"
	}
	return "missing"
}

// Snapshot copies the current config for a request.
func (s *Store) Snapshot() ConfigSnapshot {
	if s == nil {
		return ConfigSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := ConfigSnapshot{
		SerperKey: s.cfg.SerperAPIKey,
		BraveKey:  s.cfg.BraveAPIKey,
		Custom:    len(s.cfg.Priority) > 0,
	}
	if len(s.cfg.Priority) > 0 {
		out.Priority = append([]EnginePref(nil), s.cfg.Priority...)
	}
	return out
}

// PublicView is the JSON GET /settings/config body. Keys are masked.
type PublicView struct {
	OK         bool              `json:"ok"`
	ConfigPath string            `json:"config_path"`
	Serper     MaskedKey         `json:"serper"`
	Brave      MaskedKey         `json:"brave"`
	Priority   []PublicEngineRow `json:"priority"`
	Default    string            `json:"default_priority"`
}

// MaskedKey is the safe GET representation of a stored API key.
type MaskedKey struct {
	Configured bool   `json:"configured"`
	Last4      string `json:"last4,omitempty"`
}

// PublicEngineRow is one settings-page engine.
type PublicEngineRow struct {
	ID        string     `json:"id"`
	Enabled   bool       `json:"enabled"`
	Label     string     `json:"label"`
	Kind      EngineKind `json:"kind"`
	Hint      string     `json:"hint"`
	Available bool       `json:"available"`
}

// Public returns a mask-only view. Never includes raw keys.
func (s *Store) Public() PublicView {
	snap := ConfigSnapshot{}
	path := ConfigPath()
	if s != nil {
		snap = s.Snapshot()
		if s.path != "" {
			path = s.path
		}
	}
	pri := snap.Priority
	if len(pri) == 0 {
		pri = defaultPriorityPrefs()
	}
	rows := make([]PublicEngineRow, 0, len(pri))
	for _, p := range pri {
		info, ok := engineInfo(p.ID)
		if !ok {
			continue
		}
		avail := true
		if info.Kind == KindKeyed {
			avail = snap.HasKey(p.ID)
		}
		rows = append(rows, PublicEngineRow{
			ID:        info.ID,
			Enabled:   p.Enabled,
			Label:     info.Label,
			Kind:      info.Kind,
			Hint:      info.Hint,
			Available: avail,
		})
	}
	return PublicView{
		OK:         true,
		ConfigPath: path,
		Serper:     maskKey(snap.SerperKey),
		Brave:      maskKey(snap.BraveKey),
		Priority:   rows,
		Default:    defaultPriorityDoc(),
	}
}

func defaultPriorityDoc() string {
	return "No saved file: China HTTP-first baidu→sogou→360→bing→duckduckgo_html→duckduckgo; " +
		"Global duckduckgo_html→bing→sogou→360→baidu→duckduckgo. " +
		"Serper then Brave are prepended on auto only when that key exists (and the engine is not disabled). " +
		"Google is omitted on auto unless enabled in a saved priority list."
}

func defaultPriorityPrefs() []EnginePref {
	out := make([]EnginePref, 0, len(DefaultPriorityIDs))
	for _, id := range DefaultPriorityIDs {
		out = append(out, EnginePref{ID: id, Enabled: id != "google"})
	}
	return out
}

func maskKey(key string) MaskedKey {
	key = strings.TrimSpace(key)
	if key == "" {
		return MaskedKey{Configured: false}
	}
	return MaskedKey{Configured: true, Last4: last4(key)}
}

func last4(s string) string {
	if s == "" {
		return ""
	}
	n := utf8.RuneCountInString(s)
	if n <= 4 {
		return s
	}
	i := 0
	skip := n - 4
	for idx := range s {
		if i == skip {
			return s[idx:]
		}
		i++
	}
	return s
}

// SettingsPatch is the PUT /settings/config body.
// Empty key strings leave the stored key unchanged. Clear* wipes it.
type SettingsPatch struct {
	SerperAPIKey *string      `json:"serper_api_key"`
	BraveAPIKey  *string      `json:"brave_api_key"`
	ClearSerper  bool         `json:"clear_serper"`
	ClearBrave   bool         `json:"clear_brave"`
	Priority     []EnginePref `json:"priority"`
}

// ApplyPatch updates keys and priority, then writes the file (0600).
func (s *Store) ApplyPatch(p SettingsPatch) error {
	if s == nil {
		return NewError(CodeEngine, "search config is not available")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.ClearSerper {
		s.cfg.SerperAPIKey = ""
	} else if p.SerperAPIKey != nil {
		if v := strings.TrimSpace(*p.SerperAPIKey); v != "" {
			s.cfg.SerperAPIKey = v
		}
	}
	if p.ClearBrave {
		s.cfg.BraveAPIKey = ""
	} else if p.BraveAPIKey != nil {
		if v := strings.TrimSpace(*p.BraveAPIKey); v != "" {
			s.cfg.BraveAPIKey = v
		}
	}
	if p.Priority != nil {
		s.cfg.Priority = normalizePriority(p.Priority, true)
	}
	s.cfg.Version = 1
	if err := s.persistLocked(); err != nil {
		return err
	}
	log.Printf("search-config saved path=%s serper=%s brave=%s priority=%d",
		s.path, boolWord(s.cfg.SerperAPIKey != ""), boolWord(s.cfg.BraveAPIKey != ""), len(s.cfg.Priority))
	return nil
}

func normalizePriority(in []EnginePref, fillMissing bool) []EnginePref {
	seen := map[string]bool{}
	var out []EnginePref
	for _, p := range in {
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if !knownEngineID(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, EnginePref{ID: id, Enabled: p.Enabled})
	}
	if fillMissing || len(out) > 0 {
		for _, id := range DefaultPriorityIDs {
			if seen[id] {
				continue
			}
			out = append(out, EnginePref{ID: id, Enabled: id != "google"})
		}
	}
	return out
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return fmt.Errorf("search-config: empty path")
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	s.cfg.Version = 1
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(s.path)
	if dir == "" || dir == "." {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".search-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(configFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	_ = os.Chmod(s.path, configFileMode)
	ok = true
	return nil
}

var (
	activeMu    sync.RWMutex
	activeStore *Store
)

// ActivateStore makes s the process-wide key source for RunHTTP.
// Passing nil clears the process-wide store (tests).
func ActivateStore(s *Store) {
	activeMu.Lock()
	activeStore = s
	activeMu.Unlock()
}

func activeSnapshot() ConfigSnapshot {
	activeMu.RLock()
	s := activeStore
	activeMu.RUnlock()
	if s == nil {
		return ConfigSnapshot{}
	}
	return s.Snapshot()
}

func lookupAPIKey(engine string) string {
	if k := testLookupAPIKey(engine); k != "" || testHasLookup() {
		return k
	}
	return activeSnapshot().Key(engine)
}
