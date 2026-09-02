package proxyauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultBases is the HUB_AUTH_BASES default: MaClaw Hub origins used to
// validate viewer/session/machine tokens.
const DefaultBases = "https://hub.mypapers.top,https://hub.maclaw.top"

const (
	defaultTimeout = 5 * time.Second
	defaultCache   = 5 * time.Minute
	modelsPath     = "/api/llm/v1/models"
	// adminLoginPath is Hub's only password login (admin web). Viewer
	// accounts on hub.maclaw.top / hub.mypapers.top use email magic-link
	// (/api/auth/email-request), not a password. A returned access_token
	// is accepted only if HubValid still passes (same models check).
	adminLoginPath = "/api/admin/login"
)

// SettingsCookie is the HttpOnly settings-session cookie. Path=/ covers
// both /settings and the public nginx prefix /searchproxy/settings.
const SettingsCookie = "rs_settings"

// Checker accepts SEARCH_TOKEN or a Hub token that Hub's models endpoint
// accepts. Positive Hub results are cached by sha256(token). The raw token
// is never logged.
type Checker struct {
	SearchToken string
	Bases       []string
	Client      *http.Client
	CacheTTL    time.Duration
	Timeout     time.Duration
	now         func() time.Time

	mu    sync.Mutex
	cache map[string]time.Time // hex(sha256(token)) -> expiry
}

// ParseBases splits HUB_AUTH_BASES (comma-separated). Empty input uses DefaultBases.
func ParseBases(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		raw = DefaultBases
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return ParseBases(DefaultBases)
	}
	return out
}

// New builds a Checker. bases should already be ParseBases'd.
func New(searchToken string, bases []string) *Checker {
	if len(bases) == 0 {
		bases = ParseBases("")
	}
	return &Checker{
		SearchToken: searchToken,
		Bases:       bases,
		Client:      &http.Client{Timeout: defaultTimeout},
		CacheTTL:    defaultCache,
		Timeout:     defaultTimeout,
		cache:       map[string]time.Time{},
	}
}

func (c *Checker) nowTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Checker) timeout() time.Duration {
	if c != nil && c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c *Checker) cacheTTL() time.Duration {
	if c != nil && c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return defaultCache
}

func (c *Checker) client() *http.Client {
	if c != nil && c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: c.timeout()}
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokenEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// BearerToken extracts a bearer token from Authorization or ?token=.
// It does not read cookies; /search on the public proxy stays header/query only.
func BearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(a), "bearer ") {
		if t := strings.TrimSpace(a[7:]); t != "" {
			return t
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// CookieToken returns the settings-session cookie value, if any.
func CookieToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	c, err := r.Cookie(SettingsCookie)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// RequestToken is BearerToken, or the settings cookie when no bearer/query token
// is present. Used only by the /settings UI — never by /search.
func RequestToken(r *http.Request) string {
	if t := BearerToken(r); t != "" {
		return t
	}
	return CookieToken(r)
}

// Authorized reports whether token is SEARCH_TOKEN or a cached/live Hub token.
func (c *Checker) Authorized(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil {
		return false
	}
	if c.SearchToken != "" && tokenEq(token, c.SearchToken) {
		return true
	}
	return c.hubValid(token)
}

// HubValid reports whether token is accepted by Hub GET /api/llm/v1/models.
// It does not accept SEARCH_TOKEN. Used by the settings login form so the
// operator-only proxy token is never stored in the browser cookie.
func (c *Checker) HubValid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil {
		return false
	}
	return c.hubValid(token)
}

// HubPasswordLogin POSTs username/password to Hub /api/admin/login on each
// configured base and returns the access_token only when HubValid accepts it.
// Empty username or password returns "".
func (c *Checker) HubPasswordLogin(username, password string) string {
	username = strings.TrimSpace(username)
	if username == "" || password == "" || c == nil {
		return ""
	}
	for _, base := range c.Bases {
		if tok := c.hubPasswordLogin(base, username, password); tok != "" && c.hubValid(tok) {
			return tok
		}
	}
	return ""
}

func (c *Checker) hubValid(token string) bool {
	key := tokenKey(token)
	now := c.nowTime()
	c.mu.Lock()
	if exp, ok := c.cache[key]; ok && now.Before(exp) {
		c.mu.Unlock()
		return true
	}
	if exp, ok := c.cache[key]; ok && !now.Before(exp) {
		delete(c.cache, key)
	}
	c.mu.Unlock()

	if c.checkHubs(token) {
		c.mu.Lock()
		c.cache[key] = now.Add(c.cacheTTL())
		c.pruneLocked(now)
		c.mu.Unlock()
		return true
	}
	return false
}

func (c *Checker) pruneLocked(now time.Time) {
	if len(c.cache) < 1024 {
		return
	}
	for k, exp := range c.cache {
		if !now.Before(exp) {
			delete(c.cache, k)
		}
	}
}

func (c *Checker) checkHubs(token string) bool {
	for _, base := range c.Bases {
		if c.checkHub(base, token) {
			return true
		}
	}
	return false
}

func (c *Checker) checkHub(base, token string) bool {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+modelsPath, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client().Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c *Checker) hubPasswordLogin(base, username, password string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	payload, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+adminLoginPath, strings.NewReader(string(payload)))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return ""
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
		ViewerToken string `json:"viewer_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return ""
	}
	for _, tok := range []string{out.AccessToken, out.Token, out.ViewerToken} {
		if t := strings.TrimSpace(tok); t != "" {
			return t
		}
	}
	return ""
}
