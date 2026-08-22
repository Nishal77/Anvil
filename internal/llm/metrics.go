package llm

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collector handles registered via promauto, per RULE F5. These are
// not mutable application state (CLAUDE.md §5.2's forbidden
// package-level var) — they are write-only metric sinks, the same
// exception the standard library grants log/slog's default handle.
// Named per PRD §17.2's "LLM" section — anvil_llm_* throughout, not
// this package's own bare name, so every metric in the system shares
// one namespace regardless of which internal package emits it.
var (
	llmRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_llm_requests_total",
		Help: "LLM completion calls, by provider, model, and outcome.",
	}, []string{"provider", "model", "status"})

	llmTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_llm_tokens_total",
		Help: "Tokens sent to or received from an LLM provider, by model and direction.",
	}, []string{"model", "direction"})

	llmCostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_llm_cost_usd_total",
		Help: "Cumulative LLM spend in USD, by model.",
	}, []string{"model"})

	llmLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "anvil_llm_latency_seconds",
		Help:    "LLM provider call latency in seconds, by model.",
		Buckets: prometheus.DefBuckets,
	}, []string{"model"})

	llmCircuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "anvil_llm_circuit_state",
		Help: "Circuit breaker state per provider: 0 closed, 1 open.",
	}, []string{"provider"})

	llmBudgetRemainingUSD = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_llm_budget_remaining_usd",
		Help: "USD remaining under the monthly global cap before ErrGlobalCapExceeded.",
	})

	llmGlobalCapWarnings = promauto.NewCounter(prometheus.CounterOpts{
		Name: "anvil_llm_global_cap_warnings_total",
		Help: "Times the monthly USD cap crossed the 80% warning threshold.",
	})
)

// recordUsage updates the NFR-012 token/cost counters for one completed
// call.
func recordUsage(model string, usage Usage, costUSDMicros int64) {
	llmTokensTotal.WithLabelValues(model, "input").Add(float64(usage.InputTokens))
	llmTokensTotal.WithLabelValues(model, "output").Add(float64(usage.OutputTokens))
	llmCostUSDTotal.WithLabelValues(model).Add(float64(costUSDMicros) / 1_000_000)
}

// circuitStateValue is the gauge value for a breakerState (PRD §17.2:
// "0 closed, 1 open"). Half-open reads as closed — it is actively
// letting a trial call through, the opposite of what the gauge means
// by "open".
func circuitStateValue(open bool) float64 {
	if open {
		return 1
	}
	return 0
}
