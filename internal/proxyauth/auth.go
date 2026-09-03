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
	// adminLoginPath is Hub's password login for the admin console.
	// Request tenant "__global__" (or omit tenant) for a global admin.
	// Admin tokens are validated by Hub admin middleware, not models.
	adminLoginPath = "/api/admin/login"
	adminUsersPath = "/api/admin/users"
	globalTenant   = "__global__"
)

// SettingsCookie is the HttpOnly settings-session cookie. Path=/ covers
// both /settings and the public nginx prefix /searchproxy/settings.
const SettingsCookie = "rs_settings"

// Checker accepts SEARCH_TOKEN, Hub viewer tokens (models), and Hub admin
// tokens (admin users). Positive Hub results are cached by sha256(token).
// The raw token is never logged.
type Checker struct {
	SearchToken string
	Bases       []string
	Client      *http.Client
	CacheTTL    time.Duration
	Timeout     time.Duration
	now         func() time.Time

	mu         sync.Mutex
	cache      map[string]time.Time // models: hex(sha256(token)) -> expiry
	adminCache map[string]time.Time // admin: hex(sha256(token)) -> expiry
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
		adminCache:  map[string]time.Time{},
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

// Authorized reports whether token is SEARCH_TOKEN or a cached/live Hub
// viewer token (GET /api/llm/v1/models). Used by /search on the public proxy.
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

// SettingsAuthorized reports whether token may open /settings APIs: the
// operator SEARCH_TOKEN (Bearer / ?token=, not the browser cookie) or a
// Hub admin token accepted by GET /api/admin/users. Viewer/models tokens
// are not enough.
func (c *Checker) SettingsAuthorized(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil {
		return false
	}
	if c.SearchToken != "" && tokenEq(token, c.SearchToken) {
		return true
	}
	return c.adminValid(token)
}

// HubValid reports whether token is accepted by Hub GET /api/llm/v1/models.
// It does not accept SEARCH_TOKEN. Used by /search Hub-viewer checks.
func (c *Checker) HubValid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil {
		return false
	}
	return c.hubValid(token)
}

// AdminValid reports whether token is accepted by Hub GET /api/admin/users
// (requireAdmin). It does not accept SEARCH_TOKEN. Admin tokens fail the
// models endpoint; do not use HubValid for settings sessions.
func (c *Checker) AdminValid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil {
		return false
	}
	return c.adminValid(token)
}

// HubPasswordLogin POSTs username/password to Hub /api/admin/login with
// global scope (tenant "__global__") on each configured base. It returns
// access_token only when the response admin is a global admin (not
// tenant-scoped) and GET /api/admin/users accepts the token.
// Empty username or password returns "".
func (c *Checker) HubPasswordLogin(username, password string) string {
	username = strings.TrimSpace(username)
	if username == "" || password == "" || c == nil {
		return ""
	}
	for _, base := range c.Bases {
		tok, global, issued := c.hubPasswordLogin(base, username, password)
		if issued && !global {
			return ""
		}
		if tok != "" && global && c.adminValid(tok) {
			return tok
		}
	}
	return ""
}

func (c *Checker) hubValid(token string) bool {
	return c.cachedValid(token, &c.cache, c.checkHubs)
}

func (c *Checker) adminValid(token string) bool {
	return c.cachedValid(token, &c.adminCache, c.checkHubAdmins)
}

func (c *Checker) cachedValid(token string, cache *map[string]time.Time, live func(string) bool) bool {
	key := tokenKey(token)
	now := c.nowTime()
	c.mu.Lock()
	if *cache == nil {
		*cache = map[string]time.Time{}
	}
	if exp, ok := (*cache)[key]; ok && now.Before(exp) {
		c.mu.Unlock()
		return true
	}
	if exp, ok := (*cache)[key]; ok && !now.Before(exp) {
		delete(*cache, key)
	}
	c.mu.Unlock()

	if live(token) {
		c.mu.Lock()
		if *cache == nil {
			*cache = map[string]time.Time{}
		}
		(*cache)[key] = now.Add(c.cacheTTL())
		c.pruneLocked(*cache, now)
		c.mu.Unlock()
		return true
	}
	return false
}

func (c *Checker) pruneLocked(cache map[string]time.Time, now time.Time) {
	if len(cache) < 1024 {
		return
	}
	for k, exp := range cache {
		if !now.Before(exp) {
			delete(cache, k)
		}
	}
}

func (c *Checker) checkHubs(token string) bool {
	for _, base := range c.Bases {
		if c.checkHubPath(base, modelsPath, token) {
			return true
		}
	}
	return false
}

func (c *Checker) checkHubAdmins(token string) bool {
	for _, base := range c.Bases {
		if c.checkHubPath(base, adminUsersPath, token) {
			return true
		}
	}
	return false
}

func (c *Checker) checkHubPath(base, path, token string) bool {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || path == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
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

// hubPasswordLogin returns (token, isGlobal, issued). issued is true when Hub
// returned a 2xx login body with an access_token (tenant admins included).
func (c *Checker) hubPasswordLogin(base, username, password string) (token string, global, issued bool) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "", false, false
	}
	payload, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
		"tenant":   globalTenant,
	})
	if err != nil {
		return "", false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+adminLoginPath, strings.NewReader(string(payload)))
	if err != nil {
		return "", false, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return "", false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return "", false, false
	}
	var out struct {
		AccessToken string          `json:"access_token"`
		Token       string          `json:"token"`
		Admin       json.RawMessage `json:"admin"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", false, false
	}
	tok := strings.TrimSpace(out.AccessToken)
	if tok == "" {
		tok = strings.TrimSpace(out.Token)
	}
	if tok == "" {
		return "", false, false
	}
	return tok, isGlobalHubAdmin(out.Admin), true
}

type hubAdminProfile struct {
	Scope    string `json:"scope"`
	Tenant   string `json:"tenant"`
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
	IsGlobal *bool  `json:"is_global"`
	Global   *bool  `json:"global"`
}

func isGlobalHubAdmin(raw json.RawMessage) bool {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var a hubAdminProfile
	if err := json.Unmarshal(raw, &a); err != nil {
		return false
	}
	if a.IsGlobal != nil {
		return *a.IsGlobal
	}
	if a.Global != nil {
		return *a.Global
	}
	scope := strings.ToLower(strings.TrimSpace(a.Scope))
	if scope == "tenant" {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(a.Role))
	if strings.Contains(role, "tenant") {
		return false
	}
	tenant := strings.TrimSpace(a.TenantID)
	if tenant == "" {
		tenant = strings.TrimSpace(a.Tenant)
	}
	if isGlobalTenantID(tenant) {
		return scope == "" || scope == "global" || scope == globalTenant
	}
	// Non-empty tenant id: only accept when Hub explicitly marks global.
	return scope == "global" || scope == globalTenant
}

func isGlobalTenantID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return id == "" || id == globalTenant || id == "global"
}
