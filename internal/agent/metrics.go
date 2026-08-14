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
)
