package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVisitCounterBumpPersist(t *testing.T) {
	dir := t.TempDir()
	vc := newVisitCounter(dir)
	if vc.Total() != 0 {
		t.Fatalf("start=%d", vc.Total())
	}
	if n := vc.Bump(); n != 1 {
		t.Fatalf("bump1=%d", n)
	}
	if n := vc.Bump(); n != 2 {
		t.Fatalf("bump2=%d", n)
	}
	path := filepath.Join(dir, visitStatsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d visitStatsFileData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Total != 2 {
		t.Fatalf("file total=%d", d.Total)
	}
	vc2 := newVisitCounter(dir)
	if vc2.Total() != 2 {
		t.Fatalf("reload=%d", vc2.Total())
	}
}
