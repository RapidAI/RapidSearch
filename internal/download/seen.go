package download

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// Recent remembers hosts/URLs from recent successful searches so
// downloads can reuse Chrome cookies when practical.
type Recent struct {
	mu    sync.Mutex
	hosts map[string]time.Time
	urls  map[string]time.Time
	ttl   time.Duration
}

func NewRecent(ttl time.Duration) *Recent {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Recent{hosts: map[string]time.Time{}, urls: map[string]time.Time{}, ttl: ttl}
}

func (r *Recent) Remember(rawURLs []string) {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked(now)
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		r.urls[strings.TrimSpace(raw)] = now
		r.hosts[strings.ToLower(u.Hostname())] = now
	}
}

func (r *Recent) Has(raw string) bool {
	if r == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked(now)
	if t, ok := r.urls[strings.TrimSpace(raw)]; ok && now.Sub(t) < r.ttl {
		return true
	}
	host := strings.ToLower(u.Hostname())
	t, ok := r.hosts[host]
	return ok && now.Sub(t) < r.ttl
}

func (r *Recent) gcLocked(now time.Time) {
	for k, t := range r.urls {
		if now.Sub(t) >= r.ttl {
			delete(r.urls, k)
		}
	}
	for k, t := range r.hosts {
		if now.Sub(t) >= r.ttl {
			delete(r.hosts, k)
		}
	}
}
