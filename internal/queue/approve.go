package queue

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApproveJob transitions jobID from AWAITING_APPROVAL to QUEUED (PRD
// §13.1, US-02). Transition itself is the enforcement point: a job
// stuck in RUNNING or any other status rejects this call with
// IllegalTransitionError, so approval is a backend invariant, not
// something the frontend can be relied on to gate (INV-4).
func ApproveJob(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) error {
	if err := Transition(ctx, pool, jobID, StatusAwaitingApproval, StatusQueued, JobStatusFields{}); err != nil {
		return fmt.Errorf("queue: approve job %s: %w", jobID, err)
	}
	return nil
}
