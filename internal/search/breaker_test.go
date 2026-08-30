package search

import (
	"reflect"
	"testing"
	"time"
)

func TestBreakerSkipsGoogleInAutoChain(t *testing.T) {
	b := NewGoogleBreaker()
	frozen := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return frozen }
	b.Trip()

	chain := Schedule("auto", true, RouteHints{Query: "golang http server"})
	if !reflect.DeepEqual(chain, []string{"google", "bing", "duckduckgo"}) {
		t.Fatalf("global chain: %v", chain)
	}
	plan := b.Apply("auto", chain)
	if containsEngine(plan.Engines, "google") {
		t.Fatalf("auto chain still has google: %v", plan.Engines)
	}
	if !reflect.DeepEqual(plan.Engines, []string{"bing", "duckduckgo"}) {
		t.Fatalf("rewritten chain: %v", plan.Engines)
	}
	if !reflect.DeepEqual(plan.Skipped, []string{"google"}) {
		t.Fatalf("skipped: %v", plan.Skipped)
	}
	if plan.FailFast {
		t.Fatal("auto skip is not fail-fast")
	}
}

func TestBreakerExplicitGoogleStillAttempted(t *testing.T) {
	b := NewGoogleBreaker()
	frozen := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return frozen }
	b.Trip()

	one := Schedule("google", false, RouteHints{Query: "golang"})
	plan := b.Apply("google", one)
	if !reflect.DeepEqual(plan.Engines, []string{"google"}) {
		t.Fatalf("explicit google dropped: %v", plan.Engines)
	}
	if !plan.FailFast {
		t.Fatal("explicit google while open should fail fast")
	}

	fb := Schedule("google", true, RouteHints{Query: "golang"})
	fbPlan := b.Apply("google", fb)
	if containsEngine(fbPlan.Engines, "google") {
		t.Fatalf("fallback should skip google: %v", fbPlan.Engines)
	}
	if fbPlan.Engines[0] != "bing" {
		t.Fatalf("fallback first: %v", fbPlan.Engines)
	}
	if fbPlan.FailFast {
		t.Fatal("fallback skip is not fail-fast")
	}
}

func TestBreakerHalfOpenOneGoogleProbeThenSuccessCloses(t *testing.T) {
	b := NewGoogleBreaker()
	frozen := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return frozen }
	b.Trip()

	b.now = func() time.Time { return frozen.Add(DefaultGoogleBreakerCooldown + time.Second) }
	chain := []string{"google", "bing", "duckduckgo"}
	p1 := b.Apply("auto", chain)
	if !reflect.DeepEqual(p1.Engines, chain) || p1.FailFast {
		t.Fatalf("half-open probe: %#v", p1)
	}
	p2 := b.Apply("auto", chain)
	if containsEngine(p2.Engines, "google") {
		t.Fatalf("second request should skip while probe held: %v", p2.Engines)
	}

	b.Observe("google", nil)
	p3 := b.Apply("auto", chain)
	if !reflect.DeepEqual(p3.Engines, chain) {
		t.Fatalf("closed after success: %#v", p3)
	}
	if b.Open() {
		t.Fatal("breaker still open after success")
	}
}

func TestBreakerCaptchaReopens(t *testing.T) {
	b := NewGoogleBreaker()
	frozen := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return frozen }
	b.Observe("google", NewError(CodeCaptcha, "unusual traffic"))
	if !b.Open() {
		t.Fatal("captcha should open breaker")
	}
	b.Observe("bing", NewError(CodeCaptcha, "bing captcha"))
	plan := b.Apply("auto", []string{"google", "bing", "duckduckgo"})
	if containsEngine(plan.Engines, "google") {
		t.Fatalf("still skip google: %v", plan.Engines)
	}
}

func TestBreakerChinaChainUnchanged(t *testing.T) {
	b := NewGoogleBreaker()
	b.Trip()
	cn := Schedule("auto", true, RouteHints{Query: "北京天气"})
	plan := b.Apply("auto", cn)
	if !reflect.DeepEqual(plan.Engines, cn) {
		t.Fatalf("china chain rewritten: %v vs %v", plan.Engines, cn)
	}
	if len(plan.Skipped) != 0 {
		t.Fatalf("skipped: %v", plan.Skipped)
	}
}
