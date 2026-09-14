package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const visitStatsFile = "visit-stats.json"

type visitStatsFileData struct {
	Total     int64  `json:"total"`
	UpdatedAt string `json:"updated_at"`
}

type visitCounter struct {
	mu   sync.Mutex
	path string
	n    int64
}

func newVisitCounter(root string) *visitCounter {
	vc := &visitCounter{path: filepath.Join(root, visitStatsFile)}
	vc.load()
	return vc
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
}

func (vc *visitCounter) Total() int64 {
	if vc == nil {
		return 0
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.n
}

// Bump increments the page-view counter and persists it. Safe for concurrent use.
func (vc *visitCounter) Bump() int64 {
	if vc == nil {
		return 0
	}
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.n++
	_ = vc.persistLocked()
	return vc.n
}

func (vc *visitCounter) persistLocked() error {
	d := visitStatsFileData{
		Total:     vc.n,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
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
