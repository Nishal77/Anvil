package bench

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// WriteResultsMarkdown appends one run's results to w in the PRD
// §20.5 shape — pass rate, mean cost, mean duration, p95 tokens —
// prefixed with runAt's date. Appends rather than overwrites:
// benchmarks/results.md is a historical record (PRD §20.5: "Reporting
// a 62% pass rate honestly is more credible than an unqualified claim
// of success" only holds if past runs stay visible).
func WriteResultsMarkdown(w io.Writer, results []Result, runAt time.Time) error {
	if len(results) == 0 {
		return fmt.Errorf("bench: write results: no results to report")
	}

	if err := writeSummary(w, summarize(results), runAt); err != nil {
		return err
	}
	return writeTable(w, results)
}

// runSummary is the aggregate line PRD §20.5 requires: pass rate,
// mean cost, mean duration, p95 input tokens.
type runSummary struct {
	n             int
	passed        int
	meanCostUSD   float64
	meanDuration  time.Duration
	tokenP95Input int64
}

func summarize(results []Result) runSummary {
	s := runSummary{n: len(results), tokenP95Input: percentile95(tokenTotals(results))}
	var totalCost int64
	var totalDuration time.Duration
	for _, r := range results {
		if r.HarnessError == "" && r.Passed {
			s.passed++
		}
		totalCost += r.CostUSDMicros
		totalDuration += r.Duration
	}
	s.meanCostUSD = float64(totalCost) / float64(s.n) / 1_000_000
	s.meanDuration = (totalDuration / time.Duration(s.n)).Round(time.Millisecond)
	return s
}

func writeSummary(w io.Writer, s runSummary, runAt time.Time) error {
	if _, err := fmt.Fprintf(w, "\n## %s\n\n", runAt.Format("2006-01-02")); err != nil {
		return fmt.Errorf("bench: write results: %w", err)
	}
	passRate := float64(s.passed) / float64(s.n) * 100
	if _, err := fmt.Fprintf(w,
		"Pass rate: %.0f%% (%d/%d) · mean cost: $%.4f · mean duration: %s · p95 input tokens: %d\n\n",
		passRate, s.passed, s.n, s.meanCostUSD, s.meanDuration, s.tokenP95Input,
	); err != nil {
		return fmt.Errorf("bench: write results: %w", err)
	}
	return nil
}

func writeTable(w io.Writer, results []Result) error {
	if _, err := fmt.Fprintln(w, "| Task | Tier | Result | Cost (USD) | Duration | Input tokens | Output tokens |"); err != nil {
		return fmt.Errorf("bench: write results: %w", err)
	}
	if _, err := fmt.Fprintln(w, "|---|---|---|---|---|---|---|"); err != nil {
		return fmt.Errorf("bench: write results: %w", err)
	}
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | $%.4f | %s | %d | %d |\n",
			r.Task, r.Tier, resultStatus(r), float64(r.CostUSDMicros)/1_000_000,
			r.Duration.Round(time.Millisecond), r.Usage.InputTokens, r.Usage.OutputTokens,
		); err != nil {
			return fmt.Errorf("bench: write results: %w", err)
		}
	}
	return nil
}

func resultStatus(r Result) string {
	switch {
	case r.HarnessError != "":
		return "ERROR: " + r.HarnessError
	case r.Passed:
		return "PASS"
	default:
		return "FAIL"
	}
}

func tokenTotals(results []Result) []int64 {
	tokens := make([]int64, len(results))
	for i, r := range results {
		tokens[i] = r.Usage.InputTokens
	}
	return tokens
}

// percentile95 is a small, nearest-rank p95 — proportional to the
// task counts in this suite (single digits to low tens), where a
// full interpolating percentile implementation would be overkill.
func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted))*0.95 + 0.5)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
