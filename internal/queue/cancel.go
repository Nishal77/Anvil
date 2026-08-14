package queue

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// requestCancelSQL sets cancel_requested_at only if it isn't already
// set — the first cancel request wins; a second POST /cancel on an
// already-cancelling job is a no-op, not a reset of the deadline
// clock the sweeper's wedged-worker sweep measures from.
const requestCancelSQL = `UPDATE jobs SET cancel_requested_at = COALESCE(cancel_requested_at, now()) WHERE id = $1`

// RequestCancel implements PRD §13.3 step 1: mark jobID for
// cancellation. It does not itself change jobs.status — the executor
// (steps 2-4) or the sweeper's wedged-worker path (step 5) does that,
// once it actually stops.
func RequestCancel(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) error {
	if _, err := pool.Exec(ctx, requestCancelSQL, jobID); err != nil {
		return fmt.Errorf("queue: request cancel for job %s: %w", jobID, err)
	}
	return nil
}

const isCancelRequestedSQL = `SELECT cancel_requested_at IS NOT NULL FROM jobs WHERE id = $1`

// IsCancelRequested reports whether jobID has a pending cancellation —
// the query the executor's CancelWatcher runs between every turn
// (PRD §13.3 step 2).
func IsCancelRequested(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) (bool, error) {
	var requested bool
	if err := pool.QueryRow(ctx, isCancelRequestedSQL, jobID).Scan(&requested); err != nil {
		return false, fmt.Errorf("queue: check cancel requested for job %s: %w", jobID, err)
	}
	return requested, nil
}
