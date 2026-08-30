package search

import (
	"testing"
	"unicode"
)

func TestKeyDelayRange(t *testing.T) {
	lo, hi := keyDelayRange('a')
	if lo < 60 || hi > 180 || lo > hi {
		t.Fatalf("letter delay %d-%d", lo, hi)
	}
	lo, hi = keyDelayRange(' ')
	if lo < 90 || hi < lo {
		t.Fatalf("space delay %d-%d", lo, hi)
	}
}

func TestTypoNearLetter(t *testing.T) {
	r := typoNear('m')
	if !unicode.IsLetter(r) {
		t.Fatalf("typoNear produced %q", r)
	}
}

func TestShouldHelpersBounded(t *testing.T) {
	// Smoke: functions return bool and do not panic.
	_ = shouldTypo()
	_ = shouldLongPause()
}
