package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/queue"
)

// CancelWatcher reports whether cancellation has been requested. The
// executor consults it BETWEEN EVERY TURN, not only between steps — a
// step with 12 turns runs for minutes, and a cancel that waits for the
// step boundary is not a cancel (PRD §13.3 step 2).
//
// The full cancellation contract is PRD §13.3, all five steps. Steps
// 1-4 (request, check, terminate, transition) are this interface plus
// Executor's turn loop and RunStep's sandbox teardown. Step 5 — the
// case most implementations omit — is queue.sweep's force-cancel path:
// if the worker never acknowledges within the lease TTL, it may be
// wedged, so the sweeper transitions the job to CANCELLED and
// force-destroys the sandbox by ID directly, without the worker's
// cooperation.
type CancelWatcher interface {
	Cancelled(ctx context.Context, jobID uuid.UUID) (bool, error)
}

// dbCancelWatcher is the production CancelWatcher: a direct read of
// jobs.cancel_requested_at. No caching — a stale cached "not cancelled"
// would delay the very check this type exists to make fast.
type dbCancelWatcher struct {
	pool *pgxpool.Pool
}

// NewCancelWatcher returns a CancelWatcher backed by pool.
func NewCancelWatcher(pool *pgxpool.Pool) CancelWatcher {
	return dbCancelWatcher{pool: pool}
}

func (w dbCancelWatcher) Cancelled(ctx context.Context, jobID uuid.UUID) (bool, error) {
	cancelled, err := queue.IsCancelRequested(ctx, w.pool, jobID)
	if err != nil {
		return false, fmt.Errorf("agent: check cancellation: %w", err)
	}
	return cancelled, nil
}

// noopCancelWatcher never reports a cancellation — used by unit tests
// that don't exercise the cancellation path and would otherwise need
// to wire a real one.
type noopCancelWatcher struct{}

func (noopCancelWatcher) Cancelled(context.Context, uuid.UUID) (bool, error) { return false, nil }
