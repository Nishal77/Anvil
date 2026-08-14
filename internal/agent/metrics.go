package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collector handles registered via promauto, per RULE F5 — write-only
// metric sinks, the same package-level exception CLAUDE.md §5.2 grants
// log/slog's default handle (see internal/llm/metrics.go).
var (
	idemHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_idempotency_hits_total",
		Help: "Tool calls served from the idempotency cache instead of re-executing, by tool.",
	}, []string{"tool"})

	idemMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_idempotency_misses_total",
		Help: "Tool calls that executed because no idempotency cache entry existed, by tool.",
	}, []string{"tool"})

	policyDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_policy_decisions_total",
		Help: "Policy engine decisions, by tool and decision.",
	}, []string{"tool", "decision"})

	stepTurnsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_step_turns_total",
		Help: "Executor turns run across all steps.",
	})

	// contextPressureTotal is PRD §12.3's proof the context budget is
	// enforced, not aspirational: it increments every time ContextBuilder
	// drops a tier to stay under MaxContextTokens. A benchmark run where
	// this never fires means the budget was never actually tested.
	contextPressureTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "anvil_context_pressure_total",
		Help: "Times the context builder dropped a tier to stay within the token budget.",
	})

	repairAttemptsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_repair_attempts_total",
		Help: "Repair turns issued after a step's tool call failed to satisfy its acceptance criterion.",
	})

	repairCapExceededTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agent_repair_cap_exceeded_total",
		Help: "Steps that exhausted their repair budget (FR-022) without recovering.",
	})
)
