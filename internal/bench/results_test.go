package bench

import (
	"strings"
	"testing"
	"time"

	"github.com/anvil-dev/anvil/internal/llm"
)

func TestWriteResultsMarkdown_ReportsPassRateAndTable(t *testing.T) {
	results := []Result{
		{Task: "hello-world", Tier: TierTrivial, Passed: true, CostUSDMicros: 1000, Duration: 2 * time.Second, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}},
		{Task: "rest-api", Tier: TierSimple, Passed: false, CostUSDMicros: 2000, Duration: 3 * time.Second, Usage: llm.Usage{InputTokens: 200, OutputTokens: 30}},
		{Task: "broken-sandbox", Tier: TierTrivial, HarnessError: "create sandbox: boom", Duration: time.Second},
	}

	var buf strings.Builder
	runAt := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	if err := WriteResultsMarkdown(&buf, results, runAt); err != nil {
		t.Fatalf("WriteResultsMarkdown() error = %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "2026-08-12") {
		t.Error("output missing the run date")
	}
	// 1 of 3 passed: hello-world passes, rest-api fails its check, the
	// harness-error task counts as not-passed either way.
	if !strings.Contains(out, "Pass rate: 33%") {
		t.Errorf("output missing expected pass rate, got:\n%s", out)
	}
	if !strings.Contains(out, "hello-world") || !strings.Contains(out, "PASS") {
		t.Error("output missing the passing task row")
	}
	if !strings.Contains(out, "ERROR: create sandbox: boom") {
		t.Error("output missing the harness-error row")
	}
}

func TestWriteResultsMarkdown_EmptyResultsIsAnError(t *testing.T) {
	var buf strings.Builder
	if err := WriteResultsMarkdown(&buf, nil, time.Now()); err == nil {
		t.Fatal("WriteResultsMarkdown() with no results error = nil, want an error")
	}
}

func TestPercentile95(t *testing.T) {
	got := percentile95([]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100})
	if got != 100 {
		t.Errorf("percentile95() = %d, want 100 (nearest-rank on 10 values)", got)
	}
}
