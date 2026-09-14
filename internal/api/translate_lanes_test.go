package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryPopDualLaneReservation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.concurrency = 3
	svc.mu.Unlock()

	var papers []paperEntry
	ids := []string{"s1", "s2", "f1", "f2", "f3"}
	pages := map[string]int{"s1": 80, "s2": 90, "f1": 10, "f2": 20, "f3": 30}
	for _, id := range ids {
		name := id + ".pdf"
		if err := os.WriteFile(filepath.Join(dir, "pdfs", name), []byte("%PDF "+id), 0o644); err != nil {
			t.Fatal(err)
		}
		papers = append(papers, paperEntry{ID: id, HasLocal: true, Filename: name, PageCount: pages[id]})
	}
	res := svc.enqueue(ids, papers, false)
	if len(res.Queued) != 5 {
		t.Fatalf("queued %+v", res)
	}

	first := []string{svc.tryPop(), svc.tryPop(), svc.tryPop()}
	fast, slow := 0, 0
	for _, id := range first {
		if id == "" {
			t.Fatal("expected 3 starts")
		}
		if pages[id] > translateFastMaxPages {
			slow++
		} else {
			fast++
		}
	}
	if fast != 2 || slow != 1 {
		t.Fatalf("want 2 fast + 1 slow, got %v (fast=%d slow=%d)", first, fast, slow)
	}
	if extra := svc.tryPop(); extra != "" {
		t.Fatalf("4th start %s", extra)
	}

	// Finish one fast job: the reserved slow slot stays filled, next is fast.
	var finishFast string
	for _, id := range first {
		if pages[id] <= translateFastMaxPages {
			finishFast = id
			break
		}
	}
	svc.mu.Lock()
	delete(svc.running, finishFast)
	svc.mu.Unlock()
	next := svc.tryPop()
	if next == "" || pages[next] > translateFastMaxPages {
		t.Fatalf("expected remaining fast, got %q", next)
	}

	// Drain remaining fast; leftover slots should take the second slow job.
	svc.mu.Lock()
	for id := range svc.running {
		if pages[id] <= translateFastMaxPages {
			delete(svc.running, id)
		}
	}
	svc.mu.Unlock()
	// pop leftover fast if any, then slow
	var gotSlow string
	for i := 0; i < 3; i++ {
		id := svc.tryPop()
		if id == "" {
			break
		}
		if pages[id] > translateFastMaxPages {
			gotSlow = id
		}
	}
	if gotSlow != "s2" {
		t.Fatalf("expected work-steal slow s2, got %q running=%v", gotSlow, svc.runningIDs())
	}
}

func TestTryPopAllFastWhenSlowEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.concurrency = 3
	svc.mu.Unlock()
	var papers []paperEntry
	for _, id := range []string{"a", "b", "c"} {
		name := id + ".pdf"
		_ = os.WriteFile(filepath.Join(dir, "pdfs", name), []byte("%PDF"), 0o644)
		papers = append(papers, paperEntry{ID: id, HasLocal: true, Filename: name, PageCount: 8})
	}
	if res := svc.enqueue([]string{"a", "b", "c"}, papers, false); len(res.Queued) != 3 {
		t.Fatalf("%+v", res)
	}
	n := 0
	for i := 0; i < 3; i++ {
		if id := svc.tryPop(); id != "" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("all-fast should use 3 slots, got %d", n)
	}
}

func TestQueuePublicExposesLanes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pdfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTranslateService(dir)
	svc.mu.Lock()
	svc.started = true
	svc.mu.Unlock()
	papers := []paperEntry{
		{ID: "fast", HasLocal: true, Filename: "fast.pdf", PageCount: 12},
		{ID: "slow", HasLocal: true, Filename: "slow.pdf", PageCount: 70},
	}
	for _, p := range papers {
		_ = os.WriteFile(filepath.Join(dir, "pdfs", p.Filename), []byte("%PDF"), 0o644)
	}
	svc.enqueue([]string{"fast", "slow"}, papers, false)
	pub := svc.queuePublic()
	fast, _ := pub["fast_queued"].([]string)
	slow, _ := pub["slow_queued"].([]string)
	if len(fast) != 1 || fast[0] != "fast" {
		t.Fatalf("fast_queued %+v", pub["fast_queued"])
	}
	if len(slow) != 1 || slow[0] != "slow" {
		t.Fatalf("slow_queued %+v", pub["slow_queued"])
	}
}
