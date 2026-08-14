package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlannedStep is one step SavePlan writes as a row — the planner's
// output shape, kept here (not in internal/agent) so this package's
// only writer of the steps table owns the type it writes.
type PlannedStep struct {
	Title       string
	Description string
	Acceptance  string
	Optional    bool
}

const setJobPlanSQL = `UPDATE jobs SET plan_summary = $2, plan_risks = $3 WHERE id = $1`

const insertPlannedStepSQL = `
INSERT INTO steps (job_id, idx, title, description, acceptance, optional)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (job_id, idx) DO NOTHING`

// SavePlan persists a completed plan for jobID in ONE transaction: the
// plan summary and risks, every step row, and the job's transition out
// of PLANNING — either to AWAITING_APPROVAL or, if autoApprove is set,
// straight to QUEUED. A crash between the step inserts and the status
// change would otherwise leave a job that believes it has a plan with
// no steps to show for it, and the sweeper's reclaim path would re-plan
// on top of the orphaned rows (FR-020).
func SavePlan(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, summary string, risks []string, steps []PlannedStep, autoApprove bool) error {
	riskJSON, err := json.Marshal(risks)
	if err != nil {
		return fmt.Errorf("queue: save plan for job %s: encode risks: %w", jobID, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("queue: save plan for job %s: begin transaction: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, setJobPlanSQL, jobID, summary, riskJSON); err != nil {
		return fmt.Errorf("queue: save plan for job %s: set summary: %w", jobID, err)
	}

	for idx, step := range steps {
		if _, err := tx.Exec(ctx, insertPlannedStepSQL, jobID, idx, step.Title, step.Description, step.Acceptance, step.Optional); err != nil {
			return fmt.Errorf("queue: save plan for job %s: insert step %d: %w", jobID, idx, err)
		}
	}

	to := StatusAwaitingApproval
	if autoApprove {
		to = StatusQueued
	}
	// PLANNING requires a lease (requiresLease), AWAITING_APPROVAL and
	// QUEUED do not — this transition must release it, or the lease
	// lingers until its TTL expires with no worker holding it.
	if err := Transition(ctx, tx, jobID, StatusPlanning, to, JobStatusFields{ReleaseLease: true}); err != nil {
		return fmt.Errorf("queue: save plan for job %s: %w", jobID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("queue: save plan for job %s: commit: %w", jobID, err)
	}
	return nil
}
