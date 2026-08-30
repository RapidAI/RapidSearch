package cache

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"search-service/internal/search"
)

const (
	indexName         = "index.json"
	defaultTTL        = time.Hour
	refreshEvery      = 5 * time.Minute
	deferContentBytes = 4 << 10 // strip landing-page content on first write if larger
	maxBlobBytes      = 2 << 20
	blobsDirName      = "blobs"
)

// Payload is the cacheable success body (no took_ms / cached flags).
type Payload struct {
	Query           string          `json:"query"`
	Engine          string          `json:"engine"`
	RequestedEngine string          `json:"requested_engine"`
	Tried           []string        `json:"tried"`
	Skipped         []string        `json:"skipped,omitempty"`
	Results         []search.Result `json:"results"`
	Count           int             `json:"count"`
}

// Stats is GET /cache/stats. No query text.
type Stats struct {
	Bytes       int64  `json:"bytes"`
	Entries     int    `json:"entries"`
	BudgetBytes int64  `json:"budget_bytes"`
	Hits        int64  `json:"hits"`
	Misses      int64  `json:"misses"`
	FSSize      uint64 `json:"fs_size"`
	FSFree      uint64 `json:"fs_free"`
}

type Options struct {
	Dir string
	TTL time.Duration
	// MaxBytes, if > 0, overrides the Statfs-derived budget (tests).
	MaxBytes int64
	Now      func() time.Time
	// DeferContentBytes overrides the first-write content strip threshold.
	// Negative disables stripping.
	DeferContentBytes int64
}

type indexEntry struct {
	Key        string `json:"key"`
	File       string `json:"file"`
	Bytes      int64  `json:"bytes"`
	Hits       int    `json:"hits"`
	StoredAt   int64  `json:"stored_at"`
	LastHit    int64  `json:"last_hit"`
	ExpiresAt  int64  `json:"expires_at"`
	HasContent bool   `json:"has_content"`
	Blob       bool   `json:"blob,omitempty"`
}

type indexFile struct {
	Entries map[string]indexEntry `json:"entries"`
	Hits    int64                 `json:"hits"`
	Misses  int64                 `json:"misses"`
}

// Cache is a thread-safe on-disk LFU/LRU search-result cache.
//
// Landing-page `content` is omitted from the first disk write when the
// combined content payload exceeds DeferContentBytes (default 4KiB). That
// keeps one-off content=1 queries from eating disk. Frequency is stored
// in the index. A later live Put (TTL refresh or nocache) for a key with
// Hits >= 2 writes the full body including content. Cache hits of a
// deferred entry still count as hits and return the slim body (no Chrome).
type Cache struct {
	dir        string
	ttl        time.Duration
	fixed      int64
	deferBytes int64
	now        func() time.Time

	mu      sync.Mutex
	idx     indexFile
	budget  int64
	fs      FSInfo
	fsAt    time.Time
	dirty   bool
	stopped chan struct{}
	done    chan struct{}
}

func Open(opt Options) (*Cache, error) {
	if opt.Dir == "" {
		opt.Dir = "cache"
	}
	if opt.TTL <= 0 {
		opt.TTL = defaultTTL
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	deferB := int64(deferContentBytes)
	if opt.DeferContentBytes != 0 {
		deferB = opt.DeferContentBytes
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opt.Dir, blobsDirName), 0o755); err != nil {
		return nil, err
	}
	c := &Cache{
		dir:        opt.Dir,
		ttl:        opt.TTL,
		fixed:      opt.MaxBytes,
		deferBytes: deferB,
		now:        opt.Now,
		idx:        indexFile{Entries: map[string]indexEntry{}},
		stopped:    make(chan struct{}),
		done:       make(chan struct{}),
	}
	c.loadIndex()
	c.refreshBudgetLocked()
	c.evictLocked(0)
	c.persistIndexLocked()
	go c.loop()
	log.Printf("search cache dir=%s ttl=%s entries=%d bytes=%d budget_bytes=%d fs_size=%d fs_free=%d",
		c.dir, c.ttl, len(c.idx.Entries), c.bytesLocked(), c.budget, c.fs.Size, c.fs.Free)
	return c, nil
}

func (c *Cache) Close() {
	select {
	case <-c.stopped:
		return
	default:
		close(c.stopped)
	}
	<-c.done
	c.mu.Lock()
	c.persistIndexLocked()
	c.mu.Unlock()
}

func (c *Cache) loop() {
	defer close(c.done)
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-c.stopped:
			return
		case <-t.C:
			c.mu.Lock()
			c.refreshBudgetLocked()
			c.evictLocked(0)
			c.persistIndexLocked()
			c.mu.Unlock()
		}
	}
}

func (c *Cache) loadIndex() {
	p := filepath.Join(c.dir, indexName)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var idx indexFile
	if err := json.Unmarshal(b, &idx); err != nil || idx.Entries == nil {
		log.Printf("search cache index unreadable, starting empty: %v", err)
		return
	}
	for k, e := range idx.Entries {
		fp := filepath.Join(c.dir, e.File)
		st, err := os.Stat(fp)
		if err != nil {
			delete(idx.Entries, k)
			continue
		}
		e.Bytes = st.Size()
		idx.Entries[k] = e
	}
	c.idx = idx
}

func (c *Cache) persistIndexLocked() {
	if c.idx.Entries == nil {
		c.idx.Entries = map[string]indexEntry{}
	}
	b, err := json.Marshal(c.idx)
	if err != nil {
		return
	}
	tmp := filepath.Join(c.dir, indexName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("search cache index write: %v", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, indexName)); err != nil {
		log.Printf("search cache index rename: %v", err)
	}
	c.dirty = false
}

func (c *Cache) refreshBudgetLocked() {
	info, err := Statfs(c.dir)
	if err != nil {
		log.Printf("search cache statfs: %v", err)
		return
	}
	c.fs = info
	c.fsAt = c.now()
	if c.fixed > 0 {
		c.budget = c.fixed
		return
	}
	c.budget = ComputeBudget(info.Size, info.Free)
}

func (c *Cache) maybeRefreshLocked() {
	if c.fsAt.IsZero() || c.now().Sub(c.fsAt) >= refreshEvery {
		c.refreshBudgetLocked()
	}
}

func (c *Cache) bytesLocked() int64 {
	var n int64
	for _, e := range c.idx.Entries {
		n += e.Bytes
	}
	return n
}

// Get returns a live entry and increments its LFU counter.
func (c *Cache) Get(in KeyInput) (Payload, time.Time, bool) {
	key := Key(in)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.idx.Entries[key]
	if !ok || e.Blob {
		c.idx.Misses++
		c.dirty = true
		return Payload{}, time.Time{}, false
	}
	now := c.now()
	if e.ExpiresAt > 0 && now.UnixMilli() >= e.ExpiresAt {
		c.deleteLocked(key)
		c.idx.Misses++
		c.persistIndexLocked()
		return Payload{}, time.Time{}, false
	}
	b, err := os.ReadFile(filepath.Join(c.dir, e.File))
	if err != nil {
		c.deleteLocked(key)
		c.idx.Misses++
		c.persistIndexLocked()
		return Payload{}, time.Time{}, false
	}
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil || p.Count <= 0 || len(p.Results) == 0 {
		c.deleteLocked(key)
		c.idx.Misses++
		c.persistIndexLocked()
		return Payload{}, time.Time{}, false
	}
	e.Hits++
	e.LastHit = now.UnixMilli()
	c.idx.Entries[key] = e
	c.idx.Hits++
	c.dirty = true
	stored := time.UnixMilli(e.StoredAt)
	return p, stored, true
}

// Put stores a successful (non-empty) search body. Errors/empty/captcha
// must not be passed in. Writes happen after the Chrome mutex is released.
func (c *Cache) Put(in KeyInput, p Payload) {
	if p.Count <= 0 || len(p.Results) == 0 {
		return
	}
	key := Key(in)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeRefreshLocked()

	now := c.now()
	prev, existed := c.idx.Entries[key]
	hits := 1
	if existed {
		hits = prev.Hits
		if hits < 1 {
			hits = 1
		}
	}

	toStore := p
	hasContent := contentBytes(p) > 0
	if c.deferBytes >= 0 && hasContent && contentBytes(p) > c.deferBytes && hits < 2 {
		toStore = stripContent(p)
		hasContent = false
	}

	raw, err := json.Marshal(toStore)
	if err != nil {
		return
	}
	name := key + ".json"
	if int64(len(raw)) > c.budget && c.budget > 0 {
		log.Printf("search cache skip key=%s size=%d over budget=%d", key[:12], len(raw), c.budget)
		return
	}
	c.evictLocked(int64(len(raw)) - prev.Bytes)
	if c.budget > 0 && c.bytesLocked()-prev.Bytes+int64(len(raw)) > c.budget {
		log.Printf("search cache skip key=%s cannot fit size=%d budget=%d", key[:12], len(raw), c.budget)
		return
	}

	tmp := filepath.Join(c.dir, name+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("search cache write: %v", err)
		return
	}
	final := filepath.Join(c.dir, name)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return
	}
	st, err := os.Stat(final)
	sz := int64(len(raw))
	if err == nil {
		sz = st.Size()
	}
	c.idx.Entries[key] = indexEntry{
		Key:        key,
		File:       name,
		Bytes:      sz,
		Hits:       hits,
		StoredAt:   now.UnixMilli(),
		LastHit:    now.UnixMilli(),
		ExpiresAt:  now.Add(c.ttl).UnixMilli(),
		HasContent: hasContent,
	}
	c.persistIndexLocked()
}

// GetBlob returns a previously stored small download (<2MB).
func (c *Cache) GetBlob(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.idx.Entries["blob:"+id]
	if !ok || !e.Blob {
		return nil, false
	}
	now := c.now()
	if e.ExpiresAt > 0 && now.UnixMilli() >= e.ExpiresAt {
		c.deleteLocked("blob:" + id)
		c.persistIndexLocked()
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(c.dir, e.File))
	if err != nil {
		c.deleteLocked("blob:" + id)
		return nil, false
	}
	e.Hits++
	e.LastHit = now.UnixMilli()
	c.idx.Entries["blob:"+id] = e
	c.dirty = true
	return b, true
}

// PutBlob stores a successful download body if it is under 2MB and fits.
func (c *Cache) PutBlob(id string, data []byte) {
	if id == "" || len(data) == 0 || int64(len(data)) > maxBlobBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeRefreshLocked()
	key := "blob:" + id
	prev := c.idx.Entries[key]
	need := int64(len(data)) - prev.Bytes
	c.evictLocked(need)
	if c.budget > 0 && c.bytesLocked()-prev.Bytes+int64(len(data)) > c.budget {
		return
	}
	rel := filepath.Join(blobsDirName, id)
	tmp := filepath.Join(c.dir, rel+".tmp")
	if err := os.MkdirAll(filepath.Join(c.dir, blobsDirName), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, rel)); err != nil {
		_ = os.Remove(tmp)
		return
	}
	now := c.now()
	hits := 1
	if prev.Hits > 0 {
		hits = prev.Hits
	}
	c.idx.Entries[key] = indexEntry{
		Key:       key,
		File:      rel,
		Bytes:     int64(len(data)),
		Hits:      hits,
		StoredAt:  now.UnixMilli(),
		LastHit:   now.UnixMilli(),
		ExpiresAt: now.Add(c.ttl).UnixMilli(),
		Blob:      true,
	}
	c.persistIndexLocked()
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeRefreshLocked()
	n := 0
	for _, e := range c.idx.Entries {
		if !e.Blob {
			n++
		}
	}
	return Stats{
		Bytes:       c.bytesLocked(),
		Entries:     n,
		BudgetBytes: c.budget,
		Hits:        c.idx.Hits,
		Misses:      c.idx.Misses,
		FSSize:      c.fs.Size,
		FSFree:      c.fs.Free,
	}
}

// Disk returns the last Statfs snapshot and current budget (for downloads).
func (c *Cache) Disk() (fs FSInfo, budget, used int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeRefreshLocked()
	return c.fs, c.budget, c.bytesLocked()
}

func (c *Cache) deleteLocked(key string) {
	e, ok := c.idx.Entries[key]
	if !ok {
		return
	}
	_ = os.Remove(filepath.Join(c.dir, e.File))
	delete(c.idx.Entries, key)
}

func (c *Cache) evictLocked(extra int64) {
	now := c.now().UnixMilli()
	for k, e := range c.idx.Entries {
		if e.ExpiresAt > 0 && now >= e.ExpiresAt {
			c.deleteLocked(k)
		}
	}
	if c.budget <= 0 {
		return
	}
	for c.bytesLocked()+extra > c.budget && len(c.idx.Entries) > 0 {
		victim := c.pickVictimLocked()
		if victim == "" {
			break
		}
		c.deleteLocked(victim)
	}
}

// pickVictimLocked chooses LFU (lowest Hits), then LRU (oldest LastHit).
func (c *Cache) pickVictimLocked() string {
	type cand struct {
		k    string
		hits int
		last int64
	}
	var best *cand
	for k, e := range c.idx.Entries {
		if best == nil || e.Hits < best.hits || (e.Hits == best.hits && e.LastHit < best.last) {
			cp := cand{k: k, hits: e.Hits, last: e.LastHit}
			best = &cp
		}
	}
	if best == nil {
		return ""
	}
	return best.k
}

func contentBytes(p Payload) int64 {
	var n int64
	for _, r := range p.Results {
		n += int64(len(r.Content))
	}
	return n
}

func stripContent(p Payload) Payload {
	out := p
	out.Results = make([]search.Result, len(p.Results))
	copy(out.Results, p.Results)
	for i := range out.Results {
		out.Results[i].Content = ""
	}
	return out
}

// KeysForTest returns stored keys (no query text) for unit tests.
func (c *Cache) KeysForTest() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.idx.Entries))
	for k := range c.idx.Entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c *Cache) HitsForTest(in KeyInput) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idx.Entries[Key(in)].Hits
}
