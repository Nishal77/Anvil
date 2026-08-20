package queue

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const jobHasFailedStepSQL = `SELECT EXISTS(SELECT 1 FROM steps WHERE job_id = $1 AND status = 'FAILED')`

// jobHasFailedStep reports whether jobID has at least one step in
// FAILED status — the signal that it has partial progress to resume
// from, as opposed to having failed before any step ran.
func jobHasFailedStep(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx, jobHasFailedStepSQL, jobID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check failed steps for job %s: %w", jobID, err)
	}
	return exists, nil
}

// RetryJob transitions jobID from FAILED back to QUEUED, so the
// dispatcher claims it again (US-08, PRD §11.2: "Retry from first
// failed step"). It does not touch any step row: RunStep's own resume
// logic (skip SUCCEEDED and SKIPPED, re-attempt everything else,
// internal/agent/executor.go's runAllSteps) is what makes this a
// resume rather than a restart — a step that never finished
// successfully the first time is exactly the one that runs again.
//
// Returns IllegalTransitionError if jobID is not currently FAILED, and
// ErrNoStepsToRetry if it is FAILED but has no FAILED step — it failed
// before any step ran (a planner error, PRD §13.1's PLANNING -> FAILED
// edge), so there is no partial progress to resume; queuing it would
// hand the dispatcher a job with an empty plan, which runAllSteps
// would silently treat as "all steps done." Status is checked first
// so a job in the wrong status is never misreported as merely
// step-less.
func RetryJob(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) error {
	job, err := GetJob(ctx, pool, jobID)
	if err != nil {
		return fmt.Errorf("queue: retry job %s: %w", jobID, err)
	}
	if job.Status != StatusFailed {
		return fmt.Errorf("queue: retry job %s: %w", jobID, &IllegalTransitionError{JobID: jobID, From: job.Status, To: StatusQueued})
	}

	hasFailedStep, err := jobHasFailedStep(ctx, pool, jobID)
	if err != nil {
		return fmt.Errorf("queue: retry job %s: %w", jobID, err)
	}
	if !hasFailedStep {
		return fmt.Errorf("queue: retry job %s: %w", jobID, ErrNoStepsToRetry)
	}

	if err := Transition(ctx, pool, jobID, StatusFailed, StatusQueued, JobStatusFields{}); err != nil {
		return fmt.Errorf("queue: retry job %s: %w", jobID, err)
	}
	return nil
}
