package api

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// BabelDOC is not started for papers longer than this. 100 pages is allowed; 101+ is skipped.
const translateMaxPages = 100

// translateFastMaxPages is exclusive: page_count <= 50 is the fast lane;
// 51–100 is the slow lane so long jobs do not block short papers.
const translateFastMaxPages = 50

const (
	translateLaneFast = "fast"
	translateLaneSlow = "slow"
)

// Machine-readable enqueue rejection when page_count > translateMaxPages.
const translateSkipTooManyPages = "too_many_pages"

type pageCountMemoKey struct {
	path  string
	size  int64
	mtime int64
}

var pageCountMemo sync.Map // pageCountMemoKey -> int

func paperTooManyPages(p paperEntry) bool {
	return p.PageCount > translateMaxPages
}

func paperTranslateSkipReason(p paperEntry) string {
	if paperTooManyPages(p) {
		return translateSkipTooManyPages
	}
	return ""
}

// paperTranslateLane classifies a page count for the dual-lane worker pool.
// Unknown (0) counts as fast so short papers are not delayed.
func paperTranslateLane(pageCount int) string {
	if pageCount > translateMaxPages {
		return ""
	}
	if pageCount > translateFastMaxPages {
		return translateLaneSlow
	}
	return translateLaneFast
}

func paperSlowLane(p paperEntry) bool {
	return paperTranslateLane(p.PageCount) == translateLaneSlow
}

// pickTranslateLane chooses the next worker lane.
// When the slow queue has work, 1 of `limit` slots is reserved for it; the
// rest prefer fast. An empty lane donates its slots (work-stealing).
func pickTranslateLane(fastRun, slowRun, limit int, hasFast, hasSlow bool) string {
	if limit < 1 {
		return ""
	}
	if fastRun < 0 {
		fastRun = 0
	}
	if slowRun < 0 {
		slowRun = 0
	}
	if fastRun+slowRun >= limit {
		return ""
	}
	reserveSlow := 0
	if hasSlow {
		reserveSlow = 1
		if reserveSlow > limit {
			reserveSlow = limit
		}
	}
	fastCap := limit - reserveSlow
	if hasFast && fastRun < fastCap {
		return translateLaneFast
	}
	if hasSlow && (slowRun < reserveSlow || !hasFast) {
		return translateLaneSlow
	}
	return ""
}

func pdfPageCountFile(path string) int {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return 0
	}
	key := pageCountMemoKey{path: path, size: st.Size(), mtime: st.ModTime().UnixNano()}
	if v, ok := pageCountMemo.Load(key); ok {
		if n, _ := v.(int); n > 0 {
			return n
		}
	}
	n := pdfinfoPageCount(path)
	if n <= 0 {
		raw, rerr := os.ReadFile(path)
		if rerr == nil {
			n = countPDFPages(raw)
		}
	}
	if n > 0 {
		pageCountMemo.Store(key, n)
	}
	return n
}

func pdfPageCountBytes(raw []byte) int {
	if n := countPDFPages(raw); n > 0 {
		return n
	}
	return 0
}

func pdfinfoPageCount(path string) int {
	bin, err := exec.LookPath("pdfinfo")
	if err != nil {
		return 0
	}
	out, err := exec.Command(bin, path).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(strings.ToLower(line), "pages:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func attachPageCount(p paperEntry, pdfAbs string) paperEntry {
	if n := pdfPageCountFile(pdfAbs); n > 0 {
		p.PageCount = n
	}
	return p
}
