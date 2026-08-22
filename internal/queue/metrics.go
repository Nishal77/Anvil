package queue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PRD §17.2's queue-health metrics — the ones the extraction triggers
// in PRD §9.8 read to decide whether the Runner needs to become a
// standalone service. Collector handles registered via promauto, per
// RULE F5; not mutable application state (CLAUDE.md §5.2) for the same
// reason internal/llm's metrics.go gives.
var (
	jobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_jobs_total",
		Help: "Jobs that have entered a status, by status.",
	}, []string{"status"})

	jobsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_jobs_active",
		Help: "Jobs not yet in a terminal status (SUCCEEDED, FAILED, or CANCELLED).",
	})

	jobStepsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_job_steps_total",
		Help: "Steps that finished, by status (SUCCEEDED, FAILED, SKIPPED).",
	}, []string{"status"})

	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_queue_depth",
		Help: "Jobs in QUEUED status and eligible to run (run_after has elapsed).",
	})

	queueOldestPendingSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_queue_oldest_pending_seconds",
		Help: "Age in seconds of the oldest QUEUED, eligible-to-run job. 0 when the queue is empty.",
	})

	leaseReclaimsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_lease_reclaims_total",
		Help: "Expired leases reclaimed by the sweeper, by outcome (reclaimed, dead_lettered, cancelled).",
	}, []string{"reason"})

	workerUtilization = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "anvil_worker_utilization_ratio",
		Help: "Fraction of this process's dispatcher workers currently running a job (0.0-1.0).",
	})
)

// recordTransitionMetrics updates jobsTotal and jobsActive for one
// successful transition. Called from the single guarded Transition
// function (CLAUDE.md I-1), so every status change anywhere in the
// codebase is counted exactly once, regardless of which caller
// (Claim, RetryJob, handleApproveJob, the sweeper, ...) triggered it.
func recordTransitionMetrics(from, to Status) {
	jobsTotal.WithLabelValues(string(to)).Inc()
	if !isTerminal(from) && isTerminal(to) {
		jobsActive.Dec()
	}
}

const queueDepthSQL = `
SELECT count(*), COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)
FROM jobs
WHERE status = 'QUEUED' AND run_after <= now()`

// queueDepthSnapshot returns the number of QUEUED jobs eligible to run
// right now (run_after has elapsed — a job backing off after a reclaim
// isn't "pending" in the sense this gauge means) and the age in seconds
// of the oldest one, 0 when there are none.
func queueDepthSnapshot(ctx context.Context, pool *pgxpool.Pool) (depth int64, oldestSeconds float64, err error) {
	if err := pool.QueryRow(ctx, queueDepthSQL).Scan(&depth, &oldestSeconds); err != nil {
		return 0, 0, fmt.Errorf("queue: queue depth snapshot: %w", err)
	}
	return depth, oldestSeconds, nil
}
