package cache

import (
	"strings"
	"testing"
	"time"

	"search-service/internal/search"
)

func TestKeyContentFlagAndNormalize(t *testing.T) {
	a := Key(KeyInput{Query: "  Golang   HTTP Server ", Engine: "Bing", Limit: 2, Content: false})
	b := Key(KeyInput{Query: "golang http server", Engine: "bing", Limit: 2, Content: false})
	if a != b {
		t.Fatalf("normalized keys differ:\n%s\n%s\n%s\n%s", a, b, Canonical(KeyInput{Query: "  Golang   HTTP Server ", Engine: "Bing", Limit: 2}), Canonical(KeyInput{Query: "golang http server", Engine: "bing", Limit: 2}))
	}
	c0 := Key(KeyInput{Query: "golang http server", Engine: "bing", Limit: 2, Content: false})
	c1 := Key(KeyInput{Query: "golang http server", Engine: "bing", Limit: 2, Content: true})
	if c0 == c1 {
		t.Fatal("content=0 and content=1 must be different keys")
	}
	r1 := Key(KeyInput{Query: "q", Engine: "auto", Limit: 10, Region: "CN"})
	r2 := Key(KeyInput{Query: "q", Engine: "auto", Limit: 10, Region: "us"})
	if r1 == r2 {
		t.Fatal("region must be part of the key")
	}
	f1 := Key(KeyInput{Query: "q", Engine: "bing", Limit: 5, Fallback: true})
	f2 := Key(KeyInput{Query: "q", Engine: "bing", Limit: 5, Fallback: false})
	if f1 == f2 {
		t.Fatal("fallback must be part of the key")
	}
	if !strings.HasPrefix(Canonical(KeyInput{Query: "Q", Engine: "BING", Limit: 3, Content: true}), "v2|q|bing|3|1|") {
		t.Fatalf("canonical: %s", Canonical(KeyInput{Query: "Q", Engine: "BING", Limit: 3, Content: true}))
	}
}

func TestComputeBudgetClamp(t *testing.T) {
	const GB = 1 << 30
	const MB = 1 << 20
	// 126GB-ish disk: 5% >> 2GB, free is plentiful → 2GB max.
	if got := ComputeBudget(126*GB, 113*GB); got != MaxBudget {
		t.Fatalf("large disk: got %d want %d", got, MaxBudget)
	}
	// 1GB disk, 500MB free: 5%=51MB → min 64MB; 25% of 500MB=125MB → 64MB.
	if got := ComputeBudget(1*GB, 500*MB); got != MinBudget {
		t.Fatalf("small disk floor: got %d want %d", got, MinBudget)
	}
	// Headroom wins over the 64MB floor: 100MB free → 25MB.
	if got := ComputeBudget(1*GB, 100*MB); got != 25*MB {
		t.Fatalf("headroom: got %d want %d", got, 25*MB)
	}
	// Tight free space on a large disk.
	if got := ComputeBudget(100*GB, 100*MB); got != 25*MB {
		t.Fatalf("large disk tight free: got %d want %d", got, 25*MB)
	}
	if got := ComputeBudget(0, 0); got != 0 {
		t.Fatalf("zero fs: %d", got)
	}
	if got := MaxObjectBytes(113 * GB); got != MaxDownload {
		t.Fatalf("max object plentiful: %d", got)
	}
	if got := MaxObjectBytes(100 * MB); got != 25*MB {
		t.Fatalf("max object tight: %d", got)
	}
}

func payload(q string, n int) Payload {
	rs := make([]search.Result, n)
	pad := strings.Repeat("x", 180)
	for i := 0; i < n; i++ {
		rs[i] = search.Result{Rank: i + 1, Title: q + " t", URL: "https://example.com/" + q, Snippet: pad}
	}
	return Payload{Query: q, Engine: "bing", RequestedEngine: "bing", Tried: []string{"bing"}, Results: rs, Count: n}
}

func TestTTLandGet(t *testing.T) {
	now := time.Date(2026, 8, 30, 4, 0, 0, 0, time.UTC)
	c, err := Open(Options{
		Dir:      t.TempDir(),
		TTL:      time.Hour,
		MaxBytes: 8 << 20,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	in := KeyInput{Query: "golang http server", Engine: "bing", Limit: 2}
	c.Put(in, payload("golang http server", 2))
	if _, _, ok := c.Get(in); !ok {
		t.Fatal("expected hit")
	}
	now = now.Add(2 * time.Hour)
	if _, _, ok := c.Get(in); ok {
		t.Fatal("expired entry must miss")
	}
	if c.HitsForTest(in) != 0 {
		t.Fatal("expired entry should be deleted")
	}
}

func TestLFUthenLRU(t *testing.T) {
	now := time.Date(2026, 8, 30, 5, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	c, err := Open(Options{
		Dir:               dir,
		TTL:               time.Hour,
		MaxBytes:          900, // ~2 small payloads
		Now:               func() time.Time { return now },
		DeferContentBytes: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	a := KeyInput{Query: "alpha-query", Engine: "bing", Limit: 1}
	b := KeyInput{Query: "bravo-query", Engine: "bing", Limit: 1}
	d := KeyInput{Query: "delta-query", Engine: "bing", Limit: 1}

	c.Put(a, payload("alpha-query", 1))
	now = now.Add(time.Second)
	c.Put(b, payload("bravo-query", 1))
	// A is used more → higher LFU.
	if _, _, ok := c.Get(a); !ok {
		t.Fatal("a")
	}
	if _, _, ok := c.Get(a); !ok {
		t.Fatal("a2")
	}
	now = now.Add(time.Second)
	c.Put(d, payload("delta-query", 1))
	// B (hits=1) should be gone; A (hits>=3) stays.
	if _, _, ok := c.Get(b); ok {
		t.Fatalf("LFU victim B still present keys=%v", c.KeysForTest())
	}
	if _, _, ok := c.Get(a); !ok {
		t.Fatalf("frequent A evicted keys=%v", c.KeysForTest())
	}
}

func TestLRUTieBreak(t *testing.T) {
	now := time.Date(2026, 8, 30, 6, 0, 0, 0, time.UTC)
	c, err := Open(Options{
		Dir:               t.TempDir(),
		TTL:               time.Hour,
		MaxBytes:          900,
		Now:               func() time.Time { return now },
		DeferContentBytes: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a := KeyInput{Query: "alpha-query", Engine: "bing", Limit: 1}
	b := KeyInput{Query: "bravo-query", Engine: "bing", Limit: 1}
	d := KeyInput{Query: "delta-query", Engine: "bing", Limit: 1}
	c.Put(a, payload("alpha-query", 1))
	now = now.Add(2 * time.Second)
	c.Put(b, payload("bravo-query", 1))
	now = now.Add(2 * time.Second)
	c.Put(d, payload("delta-query", 1))
	// All hits=1; A is oldest LastHit → evicted.
	if _, _, ok := c.Get(a); ok {
		t.Fatalf("LRU victim A still present keys=%v", c.KeysForTest())
	}
	if _, _, ok := c.Get(b); !ok {
		t.Fatal("B should remain")
	}
}

func TestDoNotStoreEmpty(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), TTL: time.Hour, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	in := KeyInput{Query: "empty", Engine: "bing", Limit: 2}
	c.Put(in, Payload{Query: "empty", Count: 0})
	if _, _, ok := c.Get(in); ok {
		t.Fatal("empty payload must not be cached")
	}
}

func TestStatfsNonZero(t *testing.T) {
	info, err := Statfs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size == 0 || info.Free == 0 {
		t.Fatalf("statfs zero: %+v", info)
	}
	b := ComputeBudget(info.Size, info.Free)
	if b < 0 {
		t.Fatal(b)
	}
	t.Logf("statfs size=%d free=%d budget=%d", info.Size, info.Free, b)
}
