package llm

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collector handles registered via promauto, per RULE F5. These are
// not mutable application state (CLAUDE.md §5.2's forbidden
// package-level var) — they are write-only metric sinks, the same
// exception the standard library grants log/slog's default handle.
var (
	llmTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total",
		Help: "Tokens sent to or received from an LLM provider, by model and direction.",
	}, []string{"model", "direction"})

	llmCostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_cost_usd_total",
		Help: "Cumulative LLM spend in USD, by model.",
	}, []string{"model"})

	llmGlobalCapWarnings = promauto.NewCounter(prometheus.CounterOpts{
		Name: "llm_global_cap_warnings_total",
		Help: "Times the monthly USD cap crossed the 80% warning threshold.",
	})
)

// recordUsage updates the NFR-012 counters for one completed call.
func recordUsage(model string, usage Usage, costUSDMicros int64) {
	llmTokensTotal.WithLabelValues(model, "input").Add(float64(usage.InputTokens))
	llmTokensTotal.WithLabelValues(model, "output").Add(float64(usage.OutputTokens))
	llmCostUSDTotal.WithLabelValues(model).Add(float64(costUSDMicros) / 1_000_000)
}
