package api

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	// pdfCountHeadBytes is enough for typical linearized / first-object Pages trees.
	pdfCountHeadBytes = 8 << 20
	// pdfCountTailBytes covers xref-adjacent Pages dictionaries on large files.
	pdfCountTailBytes = 1 << 20
	pdfCountWindow    = 32 << 10
)

var (
	reTypePages   = regexp.MustCompile(`/Type\s*/Pages(?:\s|/|>)`)
	reCountInt    = regexp.MustCompile(`/Count\s+(\d+)`)
	reLinearized  = regexp.MustCompile(`/Linearized(?:\s|/|>)`)
	reLinearizedN = regexp.MustCompile(`/N\s+(\d+)`)
)

// pdfPageCountFromFile returns the PDF page count, or 0 if unknown.
func pdfPageCountFromFile(path string) int {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0
	}
	raw, err := readPDFCountBytes(path)
	if err == nil && len(raw) > 0 {
		if n := pdfPageCountFromBytes(raw); n > 0 {
			return n
		}
	}
	return pdfinfoPageCount(path)
}

func readPDFCountBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 {
		return nil, io.EOF
	}
	if size <= pdfCountHeadBytes+pdfCountTailBytes {
		return io.ReadAll(io.LimitReader(f, size))
	}
	head := make([]byte, pdfCountHeadBytes)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}
	if _, err := f.Seek(-pdfCountTailBytes, io.SeekEnd); err != nil {
		return head, nil
	}
	tail := make([]byte, pdfCountTailBytes)
	n, err := io.ReadFull(f, tail)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return head, nil
	}
	out := make([]byte, 0, len(head)+n)
	out = append(out, head...)
	out = append(out, tail[:n]...)
	return out, nil
}

// pdfPageCountFromBytes prefers /Type /Pages /Count (and linearized /N),
// then inflated streams, then a /Type /Page object count.
func pdfPageCountFromBytes(raw []byte) int {
	if n := maxPagesTreeCount(raw); n > 0 {
		return n
	}
	if n := linearizedPageCount(raw); n > 0 {
		return n
	}
	var inflatedMax int
	eachPDFStream(raw, func(dict, data []byte) {
		payload := data
		if reFlateFilter.Match(dict) {
			if inf, err := zlibInflate(data); err == nil && len(inf) > 0 {
				payload = inf
			}
		}
		if n := maxPagesTreeCount(payload); n > inflatedMax {
			inflatedMax = n
		}
	})
	if inflatedMax > 0 {
		return inflatedMax
	}
	if n := countPDFPages(raw); n > 0 {
		return n
	}
	return 0
}

func maxPagesTreeCount(raw []byte) int {
	max := 0
	for _, loc := range reTypePages.FindAllIndex(raw, -1) {
		start := loc[0] - pdfCountWindow
		if start < 0 {
			start = 0
		}
		end := loc[1] + pdfCountWindow
		if end > len(raw) {
			end = len(raw)
		}
		for _, m := range reCountInt.FindAllSubmatch(raw[start:end], -1) {
			n, err := strconv.Atoi(string(m[1]))
			if err != nil || n <= 0 {
				continue
			}
			if n > max {
				max = n
			}
		}
	}
	return max
}

func linearizedPageCount(raw []byte) int {
	if !reLinearized.Match(raw) {
		return 0
	}
	// Linearization dict is in the first objects.
	head := raw
	if len(head) > 16<<10 {
		head = head[:16<<10]
	}
	if i := bytes.Index(head, []byte("/Linearized")); i >= 0 {
		start := i - 256
		if start < 0 {
			start = 0
		}
		end := i + 512
		if end > len(head) {
			end = len(head)
		}
		if m := reLinearizedN.FindSubmatch(head[start:end]); len(m) == 2 {
			n, err := strconv.Atoi(string(m[1]))
			if err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func pdfinfoPageCount(path string) int {
	bin, err := exec.LookPath("pdfinfo")
	if err != nil {
		return 0
	}
	cmd := exec.Command(bin, path)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "pages:") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			return 0
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(rest))
		if convErr != nil || n <= 0 {
			return 0
		}
		return n
	}
	return 0
}
