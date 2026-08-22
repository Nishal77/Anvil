package sandbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PRD §17.2's sandbox metrics this package is positioned to observe
// from the control-plane side of the Runner protocol — Collector
// handles registered via promauto, per RULE F5; not mutable
// application state (CLAUDE.md §5.2) for the same reason every other
// package's metrics.go gives.
//
// anvil_sandbox_oom_kills_total and anvil_egress_denials_total are
// PRD §17.2 metrics deliberately NOT declared here yet: nothing in
// this codebase detects an OOM kill (would need a container inspect
// after every exec) or denies egress (the allowlist proxy is
// BUILD-PLAN Week 11, task 11.3) — declaring either collector now
// would mean a metric that's always zero, which is worse than absent:
// a dashboard panel that's always flat reads as "this never happens"
// rather than "this isn't wired up yet."
var (
	sandboxActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_sandbox_active",
		Help: "Sandboxes currently created and not yet destroyed, from this process's view.",
	})

	sandboxCreateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "anvil_sandbox_create_duration_seconds",
		Help:    "Time to create a sandbox, from the control plane's request to the Runner's response.",
		Buckets: prometheus.DefBuckets,
	})

	sandboxExecDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "anvil_sandbox_exec_duration_seconds",
		Help:    "Time for one exec call to run to completion, cancellation, or timeout.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 0.1s .. ~409s: exec durations span far wider than a typical HTTP request
	})

	sandboxTimeoutKillsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "anvil_sandbox_timeout_kills_total",
		Help: "Exec calls killed for exceeding their timeout.",
	})
)
