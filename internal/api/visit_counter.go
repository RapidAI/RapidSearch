package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	visitStatsFile = "visit-stats.json"

	// visitorCookieName is a first-party durable id for one browser/device.
	// It is the unique visitor key. Client IP is recorded for ops only.
	visitorCookieName   = "rs_papers_visitor"
	visitorCookieMaxAge = 365 * 24 * 60 * 60

	// visitDedupeWindow is how long the same visitor cookie is treated as
	// one catalog open. Refresh / back / reopen inside this window does
	// not increment. A later return after the window counts again.
	visitDedupeWindow = 12 * time.Hour

	visitSeenCap = 200_000
)

type visitStatsFileData struct {
	Total     int64             `json:"total"`
	UpdatedAt string            `json:"updated_at"`
	LastIP    string            `json:"last_ip,omitempty"`
	Seen      map[string]string `json:"seen,omitempty"`
}

type visitCounter struct {
	mu     sync.Mutex
	path   string
	n      int64
	lastIP string
	seen   map[string]time.Time
	now    func() time.Time
}

func newVisitCounter(root string) *visitCounter {
	vc := &visitCounter{
		path: filepath.Join(root, visitStatsFile),
		seen: map[string]time.Time{},
	}
	vc.load()
	return vc
}

func (vc *visitCounter) clock() time.Time {
	if vc != nil && vc.now != nil {
		return vc.now()
	}
	return time.Now()
}

func (vc *visitCounter) load() {
	if vc == nil {
		return
	}
	b, err := os.ReadFile(vc.path)
	if err != nil {
		return
	}
	var d visitStatsFileData
	if json.Unmarshal(b, &d) != nil {
		return
	}
	if d.Total > 0 {
		vc.n = d.Total
	}
	vc.lastIP = strings.TrimSpace(d.LastIP)
	now := vc.clock()
	if vc.seen == nil {
		vc.seen = map[string]time.Time{}
	}
	for id, raw := range d.Seen {
		if !validVisitorID(id) {
			continue
		}
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			continue
		}
		if now.Sub(ts) >= visitDedupeWindow {
			continue
		}
		vc.seen[id] = ts
	}
}

func (vc *visitCounter) Total() int64 {
	if vc == nil {
		return 0
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.n
}

// Hit records a GET /papers HTML open. Increments at most once per visitor
// cookie inside visitDedupeWindow. Always (re)sets the visitor cookie so a
// new browser/device gets its own identity even when it shares a public IP.
func (vc *visitCounter) Hit(w http.ResponseWriter, r *http.Request) int64 {
	if vc == nil {
		return 0
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()

	id, minted := visitorIDFromRequest(r)
	if w != nil && r != nil {
		setVisitorCookie(w, r, id)
	}
	if r == nil || isBotVisit(r) {
		return vc.n
	}

	now := vc.clock()
	if !minted {
		if last, ok := vc.seen[id]; ok {
			delta := now.Sub(last)
			if delta >= 0 && delta < visitDedupeWindow {
				return vc.n
			}
		}
	}

	vc.n++
	if vc.seen == nil {
		vc.seen = map[string]time.Time{}
	}
	vc.seen[id] = now
	ip := requestClientIP(r)
	vc.lastIP = ip
	vc.pruneSeenLocked(now)
	_ = vc.persistLocked()
	if len(id) > 8 {
		id = id[:8]
	}
	log.Printf("papers visit counted ip=%s visitor=%s total=%d", ip, id, vc.n)
	return vc.n
}

func (vc *visitCounter) persistLocked() error {
	seen := make(map[string]string, len(vc.seen))
	now := vc.clock()
	for id, ts := range vc.seen {
		if now.Sub(ts) >= visitDedupeWindow {
			continue
		}
		seen[id] = ts.UTC().Format(time.RFC3339)
	}
	d := visitStatsFileData{
		Total:     vc.n,
		UpdatedAt: now.UTC().Format(time.RFC3339),
		LastIP:    vc.lastIP,
		Seen:      seen,
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(vc.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := vc.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, vc.path)
}

func (vc *visitCounter) pruneSeenLocked(now time.Time) {
	for id, ts := range vc.seen {
		if now.Sub(ts) >= visitDedupeWindow {
			delete(vc.seen, id)
		}
	}
	if len(vc.seen) <= visitSeenCap {
		return
	}
	// Drop oldest extra entries if a flood filled the window.
	type pair struct {
		id string
		ts time.Time
	}
	extra := len(vc.seen) - visitSeenCap
	oldest := make([]pair, 0, extra)
	for id, ts := range vc.seen {
		oldest = append(oldest, pair{id, ts})
	}
	// Partial selection: repeatedly drop the current oldest.
	for i := 0; i < extra; i++ {
		min := 0
		for j := 1; j < len(oldest); j++ {
			if oldest[j].ts.Before(oldest[min].ts) {
				min = j
			}
		}
		delete(vc.seen, oldest[min].id)
		oldest[min] = oldest[len(oldest)-1]
		oldest = oldest[:len(oldest)-1]
	}
}

func visitorIDFromRequest(r *http.Request) (id string, minted bool) {
	if r != nil {
		if c, err := r.Cookie(visitorCookieName); err == nil && c != nil && validVisitorID(c.Value) {
			return c.Value, false
		}
	}
	return newVisitorID(), true
}

func setVisitorCookie(w http.ResponseWriter, r *http.Request, id string) {
	if w == nil || id == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     visitorCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   visitorCookieMaxAge,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func newVisitorID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b[:])
}

func validVisitorID(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 16 || len(v) > 64 {
		return false
	}
	for _, r := range v {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

// requestClientIP returns the end-user address for ops/logging.
// Behind nginx/search-proxy we trust X-Real-IP (nginx overwrites it) and
// the rightmost X-Forwarded-For hop (nginx appends $remote_addr). Leftmost
// XFF is ignored so a client cannot spoof the recorded IP. This value is
// never the unique visitor key.
func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := parseIPToken(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := parseIPToken(parts[i]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := parseIPToken(host); ip != "" {
			return ip
		}
		return host
	}
	return parseIPToken(r.RemoteAddr)
}

func parseIPToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.Trim(s, "[]")
	if net.ParseIP(s) == nil {
		return ""
	}
	return s
}

func isBotVisit(r *http.Request) bool {
	if r == nil {
		return true
	}
	purpose := strings.ToLower(strings.TrimSpace(r.Header.Get("Purpose")))
	if purpose == "" {
		purpose = strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Purpose")))
	}
	if strings.Contains(purpose, "prefetch") || strings.Contains(purpose, "preview") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Moz")), "prefetch") {
		return true
	}
	ua := strings.ToLower(strings.TrimSpace(r.Header.Get("User-Agent")))
	if ua == "" {
		return true
	}
	for _, hint := range visitBotUAHints {
		if strings.Contains(ua, hint) {
			return true
		}
	}
	return false
}

// Known crawlers / CLI clients. Substring match on a lowercased UA.
var visitBotUAHints = []string{
	"googlebot", "bingbot", "bingpreview", "slurp", "duckduckbot",
	"baiduspider", "yandexbot", "yandex.com/bots", "sogou",
	"facebookexternalhit", "twitterbot", "linkedinbot", "slackbot",
	"ahrefsbot", "semrushbot", "mj12bot", "dotbot", "petalbot",
	"bytespider", "gptbot", "chatgpt-user", "claudebot", "anthropic",
	"ccbot", "applebot", "amazonbot", "ia_archiver",
	"curl/", "wget/", "python-requests", "python-urllib", "go-http-client",
	"httpie", "scrapy", "libwww-perl", "okhttp",
	"headlesschrome", "phantomjs",
}
