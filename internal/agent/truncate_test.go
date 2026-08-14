package agent

import (
	"strings"
	"testing"
)

func TestTruncateObservation_UnderLimitUnchanged(t *testing.T) {
	s := "short observation"
	if got := truncateObservation(s, 100); got != s {
		t.Errorf("truncateObservation() = %q, want unchanged", got)
	}
}

func TestTruncateObservation_KeepsHeadAndTailWithMarker(t *testing.T) {
	head := strings.Repeat("A", 100)
	middle := strings.Repeat("B", 1000)
	tail := "THE ACTUAL ERROR IS HERE"
	s := head + middle + tail

	got := truncateObservation(s, 200)

	if !strings.HasPrefix(got, "AAAA") {
		t.Error("truncateObservation() dropped the head")
	}
	if !strings.Contains(got, tail) {
		t.Error("truncateObservation() dropped the tail — compiler/test errors are conventionally the LAST thing printed, this must survive")
	}
	if !strings.Contains(got, "elided") {
		t.Error("truncateObservation() missing the elision marker")
	}
	if len(got) >= len(s) {
		t.Errorf("truncateObservation() length = %d, want less than input length %d", len(got), len(s))
	}
}

func TestTruncateObservation_MarkerReportsElidedCount(t *testing.T) {
	s := strings.Repeat("x", 10000)
	got := truncateObservation(s, 100)
	if !strings.Contains(got, "9") { // elided count is in the thousands; just confirm a digit-bearing marker exists
		t.Errorf("truncateObservation() = %q, want a marker reporting a byte count", got)
	}
}
